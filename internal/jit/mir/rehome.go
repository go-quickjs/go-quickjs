package mir

import (
	"math/bits"
	"slices"

	"github.com/go-quickjs/go-quickjs/internal/jit/abi"
	"github.com/go-quickjs/go-quickjs/internal/jit/ir"
	"github.com/go-quickjs/go-quickjs/internal/jit/ssa"
)

// rehomePlan is what a loop header's polls let Go write into the frame
// (abi.Rehome): the registers whose values' sources it may make their own
// (rewire), and those it leaves as they are (pinned), each in order; the
// keep cells a value live at the poll that the state does not name reads
// its objects from (reached, the build's numbers), and those that no value
// live there reads (dead, the context's, once colorKeeps has numbered
// them; abi.Rehome's DeadKeeps).
type rehomePlan struct {
	rewire, pinned []int32
	reached        []int32
	dead           []uint16
}

// findRehomes finds, for each loop header, whether the polls of its back
// edges may have Go write the header's state into the frame and go on
// (abi.Rehome, the VM's GoPoll), and what it may write: c.rehomes, by
// block ID, nil where none may. liveIn is each block's set at its start
// (blockLiveness), iv the intervals, whose values name the sets' IDs.
//
// Native code reads a reference through its source -- the slot that held
// it when the code was entered, a heap cell, a keep cell -- each time it
// uses it, and a value derived from one -- an object's address, a cell in
// it -- holds no reference of its own the collector sees. So a register
// is written only where nothing live at the poll still reads what it
// holds: a header's state names each register's value and its source,
// which Go checks against one another at run time (jit.Rehome); a value
// live there that the state does not name -- one GVN or LICM moved out of
// the loop -- is followed back to what holds its object, and a register
// it is read from is pinned, never written. One that cannot be followed
// -- a phi the state does not name, whose value at run time may be any
// turn's -- leaves the header with no plan. Typed code's raw slots, an
// inlined callee's frame and a generator's are left to the exits.
func (c *core) findRehomes(liveIn []ir.LiveSet, lb *ir.LiveBuilder, iv []interval) {
	c.rehomes = nil
	f := c.f
	if c.enc.CallGo == 0 || f.Kinds != nil || f.GenEntry {
		return
	}
	nv := f.NumValues()
	// stamp is, by value ID, 2*gen for the header's state's values and
	// their shadow phis, 2*gen+1 for those followed, below for neither.
	var stamp []int32
	var done, has []bool
	gen := int32(0)
	for _, b := range c.order {
		s := b.Header
		if !b.LoopHeader || s == nil || s.Inline != nil || !hasBackedge(b) {
			continue
		}
		// Only a state with a register whose value may not be what its
		// slot holds, and may be a reference or the slot may hold one:
		// found of the states' shared nodes, each once.
		root, h := s.Tree()
		if root == nil {
			continue
		}
		if done == nil {
			done, has = c.bools(f.NumSlotNodes()), c.bools(f.NumSlotNodes())
		}
		if !c.rehomeTrigger(root, h, 0, done, has) {
			continue
		}
		if stamp == nil {
			stamp = c.int32s(nv)
		}
		gen++
		// The state's values, and the sources only known at run time that
		// Go may move (a shadow phi's): those Go checks, at run time.
		var rewire []int32
		for i, v := range s.All() {
			c.steps++
			if v.Type == ssa.Tagged {
				stamp[v.ID] = 2 * gen
			}
			if sh := v.Shadow; sh != nil && sh.Op == ssa.OpPhi {
				stamp[sh.ID] = 2 * gen
				if i < f.FrameLocals && c.exitWritten(i, v) {
					rewire = append(rewire, int32(i))
				}
			}
		}
		// Every other value live at the poll: the header's phis, and what
		// is live into it -- a value made in the loop is live into its
		// header only as a phi's argument, so these are made before it.
		var pinned, reached []int32
		ok := true
		follow := func(u *ssa.Value) bool {
			p, ok := c.rehomeFollow(u, stamp, gen, pinned, &reached)
			pinned = p
			return ok
		}
		for _, v := range b.Values {
			if v.Op == ssa.OpPhi && !follow(v) {
				ok = false
				break
			}
		}
		if ok && liveIn != nil && lb != nil {
			lb.Added(lb.Empty(), liveIn[b.ID], func(id int) bool {
				c.steps++
				if id < nv && iv[id].v != nil && !follow(iv[id].v) {
					ok = false
				}
				return ok
			})
		}
		if !ok {
			continue
		}
		if c.rehomes == nil {
			c.rehomes = make([]*rehomePlan, numBlocks(f))
		}
		c.rehomes[b.ID] = &rehomePlan{rewire: rewire, pinned: sortedSlots(pinned), reached: sortedSlots(reached)}
	}
}

// rehomeTrigger reports whether node nd of the states' slots, of height
// h, whose first slot is base, has a frame register an exit writes whose
// value may be a reference, or that may hold one: not a number or a
// boolean made of one an entry checked the slot held, nor a constant
// (findRehomes). done and has are, by node ID, the nodes found and what.
func (c *core) rehomeTrigger(nd *ssa.SlotNode, h, base int, done, has []bool) bool {
	id := nd.ID()
	if done[id] {
		return has[id]
	}
	done[id] = true
	c.steps++
	if base >= c.f.FrameLocals {
		return false
	}
	m := nd.Mask()
	if h > 0 {
		for _, kid := range nd.Kids() {
			k := bits.TrailingZeros64(m)
			m &= m - 1
			if c.rehomeTrigger(kid, h-1, base+k<<(ssa.SlotShift*h), done, has) {
				has[id] = true
			}
		}
		return has[id]
	}
	for _, v := range nd.Vals() {
		i := base + bits.TrailingZeros64(m)
		m &= m - 1
		if i >= c.f.FrameLocals || !c.exitWritten(i, v) {
			continue
		}
		switch v.Op {
		case ssa.OpBoxF64, ssa.OpBoxBool, ssa.OpConst:
		default:
			has[id] = true
		}
	}
	return has[id]
}

// hasBackedge reports whether loop header b has a back edge, which polls.
func hasBackedge(b *ssa.Block) bool {
	for _, e := range b.Backedge {
		if e {
			return true
		}
	}
	return false
}

// rehomeFollow follows u, a value live at a loop header's poll, back to
// what holds the objects it reads (findRehomes), adding to pinned each
// frame register that does, and to reached each keep cell, and reports
// whether it could: stamp has, at 2*gen, the header's state's values and
// their shadow phis, which need no following, and at 2*gen+1 the values
// followed.
func (c *core) rehomeFollow(u *ssa.Value, stamp []int32, gen int32, pinned []int32, reached *[]int32) ([]int32, bool) {
	stack := append(c.rehomeStack[:0], u)
	defer func() { c.rehomeStack = stack[:0] }()
	for len(stack) > 0 {
		x := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if x.ID >= len(stamp) || stamp[x.ID] >= 2*gen {
			continue
		}
		stamp[x.ID] = 2*gen + 1
		c.steps++
		switch x.Type {
		case ssa.Tagged, ssa.Ptr, ssa.Source:
		default:
			// No address.
			continue
		}
		if c.isLazy(x) && x.Op != ssa.OpConstSource {
			// A constant or a box, made where it is needed.
			continue
		}
		switch x.Op {
		case ssa.OpPhi:
			if x.Type == ssa.Tagged && x.Shadow == nil && c.origin.At(x) == ssa.OriginScalar {
				// A primitive.
				continue
			}
			// Any turn's value, which no state names here.
			return pinned, false
		case ssa.OpLoadSlot:
			if x.Aux >= 0 && x.Aux < c.f.FrameLocals {
				pinned = append(pinned, int32(x.Aux))
			}
			continue
		case ssa.OpConstSource:
			if x.Aux >= 0 && x.Aux < c.f.FrameLocals {
				pinned = append(pinned, int32(x.Aux))
			}
			continue
		case ssa.OpKeep:
			// A keep cell, which its keep writes: kept.
			*reached = append(*reached, int32(x.Index))
			continue
		case ssa.OpCallCell, ssa.OpBigCell:
			// A call's result's cell, or a BigInt's: kept.
			if w := x.Args[0]; w.WritesKeepCell() {
				*reached = append(*reached, int32(w.Index))
			}
			continue
		case ssa.OpGlobalCell, ssa.OpLexCell, ssa.OpBindingCell, ssa.OpConstCell, ssa.OpUpvalueCell, ssa.OpLocalCell,
			ssa.OpHolderCell, ssa.OpStringChar, ssa.OpCodeChar, ssa.OpCodeCharOr,
			ssa.OpSubstringChar, ssa.OpStringAtOr, ssa.OpTypeofCell, ssa.OpStringMethod, ssa.OpStringProp, ssa.OpHeapBase, ssa.OpHeapLen:
			// A cell the runtime, a closure or the context keeps, or one of
			// an object the code was compiled for.
			continue
		}
		if x.Type == ssa.Tagged {
			if x.Shadow != nil {
				// Read from the cell its shadow is.
				stack = append(stack, x.Shadow)
				continue
			}
			if c.origin.At(x) < 0 {
				// A primitive native code made: no pointer word.
				continue
			}
		}
		for _, a := range x.Args {
			switch a.Type {
			case ssa.Tagged, ssa.Ptr, ssa.Source:
				stack = append(stack, a)
			}
		}
	}
	return pinned, true
}

// sortedSlots is slots in order, each once, in memory of their own.
func sortedSlots(slots []int32) []int32 {
	if len(slots) == 0 {
		return nil
	}
	out := slices.Clone(slots)
	slices.Sort(out)
	return slices.Compact(out)
}

// pollState describes a back edge's poll into loop header h, a call of
// Go (ssa.GoPoll): with what Go may write into the frame there
// (abi.Rehome), where findRehomes found it may.
func (c *core) pollState(h *ssa.Block, localsReg, stackReg uint8) *abi.GoCallState {
	st, _ := c.goState(uint64(ssa.GoPoll), 0, nil, nil)
	if h.ID >= len(c.rehomes) || c.rehomes[h.ID] == nil {
		return st
	}
	plan := c.rehomes[h.ID]
	d := c.exitDescriptor(h.Header, abi.ExitPoll, localsReg, stackReg)
	if d == nil {
		return st
	}
	// The slots whose values may be references (abi.Rehome's Slots): of
	// the frame's registers, which Go writes, those whose values are tagged
	// words or constants; of the others, which it never writes, those
	// whose references may be read from a register.
	var slots []abi.ExitSlot
	frame := int32(c.f.FrameLocals)
	d.Slots(func(e *abi.ExitSlot) {
		switch {
		case e.Slot >= frame:
			if e.Shadow.Kind == abi.ExitNone && e.Origin < 0 {
				return
			}
		case e.Value.Kind != abi.ExitReg && e.Value.Kind != abi.ExitSpill && e.Value.Kind != abi.ExitConst:
			return
		}
		slots = append(slots, *e)
	})
	if len(slots) == 0 {
		if len(plan.dead) != 0 {
			// Keep cells to empty alone, checked against the state's
			// sources (jit's emptyKeeps).
			st.Rehome = &abi.Rehome{Desc: d, DeadKeeps: plan.dead}
		}
		return st
	}
	marks := make([]uint8, len(slots))
	for i := range slots {
		if _, ok := slices.BinarySearch(plan.rewire, slots[i].Slot); ok {
			marks[i] |= abi.RehomeRewire
		}
		if _, ok := slices.BinarySearch(plan.pinned, slots[i].Slot); ok {
			marks[i] |= abi.RehomePinned
		}
	}
	st.Rehome = &abi.Rehome{Desc: d, Slots: slots, Marks: marks, DeadKeeps: plan.dead}
	return st
}

// rehomeDeadKeeps finds, for each loop header whose polls have a plan
// (findRehomes), the keep cells that no value live at its polls may read
// its reference from (rehomePlan's dead), of the cells colorKeeps gave
// (phys, by keep): those no keep of which a value live there that the
// header's state does not name was followed back to (reached). The
// follow is the one the rehome's registers rely on: it reaches every
// such value's sources or there is no plan. A state's value, or its
// shadow phi, may name a cell another keep was given before its own
// write -- a loop-carried result not yet made, its cell still holding
// what an earlier keep put there -- so which cells the state's values
// read Go finds at run time, and a cell one does is not emptied
// (jit.Rehome). None where the code makes loops of built-ins' calls
// (ssa.Func's Loops), whose loops keep cells of their own.
func (c *core) rehomeDeadKeeps(phys []int) {
	if c.rehomes == nil || c.f.Loops || len(phys) == 0 {
		return
	}
	var live, given [(abi.MaxKeeps + 63) / 64]uint64
	for _, p := range phys {
		if p < abi.MaxKeeps {
			given[p/64] |= 1 << (p % 64)
		}
	}
	for _, plan := range c.rehomes {
		if plan == nil {
			continue
		}
		live = [len(live)]uint64{}
		ok := true
		for _, k := range plan.reached {
			if k < 0 || int(k) >= len(phys) {
				ok = false
				break
			}
			if p := phys[k]; p < abi.MaxKeeps {
				live[p/64] |= 1 << (p % 64)
			}
		}
		if !ok {
			continue
		}
		for w := range given {
			for m := given[w] &^ live[w]; m != 0; m &= m - 1 {
				plan.dead = append(plan.dead, uint16(w*64+bits.TrailingZeros64(m)))
			}
		}
	}
}
