// Package mir compiles the JIT's SSA (internal/jit/ssa) to machine code:
// it orders blocks, computes liveness, allocates registers by linear scan and
// selects instructions, then encodes them (internal/jit/asm). What every
// architecture shares is here (core); each has its code generator
// (amd64.go, arm64.go). Plain code over clever code, verified against the
// SSA evaluator (ssa.EvaluateHeap).
package mir

import (
	"errors"
	"fmt"
	"math"
	"math/bits"
	"runtime"
	"slices"

	"github.com/go-quickjs/go-quickjs/internal/jit/abi"
	"github.com/go-quickjs/go-quickjs/internal/jit/ir"
	"github.com/go-quickjs/go-quickjs/internal/jit/ssa"
)

// core is a function's analysis and allocation, which code generators for
// every architecture share.
type core struct {
	f     *ssa.Func
	enc   abi.Encoding
	order []*ssa.Block
	// lazy values have no location: constants used only by frame states,
	// and boxes used only by frame states and returns, which are
	// rematerialized where needed. lazy, locs and hasLoc are by value ID.
	lazy   []bool
	locs   []loc
	hasLoc []bool
	// origin is each tagged value's (ssa.Origins): the slot an exit looks
	// in for the reference the value may be.
	origin ssa.OriginMap
	// gprs and fprs are the architecture's allocatable registers, by number.
	gprs, fprs []int
	// used are the registers the allocation gave a value, by number: the
	// general ones, then the floating-point ones. Only these hold values an
	// exit or a call of Go must keep (usedRegs).
	used     [2]uint64
	usedList [2][]int
	// ws is the workspace the compile takes its tables from, or nil.
	ws *Workspace
	// sched is phiSchedule's lists, reused from edge to edge: the
	// workspace's, kept from compile to compile, or the core's own.
	sched *schedule
	own   schedule
	// saves are, for each native call (ssa's OpCall), the values live
	// across it that are in registers, which it saves to slots of their own
	// and restores after: the callee uses every register, as V8's code's
	// callees use the caller-saved ones. A call's are
	// saves[saveAt[2*ID]:saveAt[2*ID+1]] (savesOf), nil saveAt for none.
	saves  []saved
	saveAt []int
	// exitFrame is what exitDescriptor's descriptions share (abi.ExitFrame),
	// its slots and words each once, found again by exitSlots and
	// exitWords; exitDescs are the descriptions, carved from blocks, as
	// the code keeps them all as long: each block twice the last, up to a
	// limit, so a small function takes little.
	exitFrame *abi.ExitFrame
	exitSlots map[abi.ExitSlot]uint16
	exitWords map[uint64]int32
	exitDescs []abi.ExitDescriptor
}

// saved is a register a call saves, and the spill slot it saves it in.
type saved struct {
	reg, slot int
	float     bool
}

// schedule is phiSchedule's lists.
type schedule struct {
	moves  []phiMove
	steps  []phiStep
	parked []*ssa.Value
	// readers counts, by location (locKey), the pending moves that read
	// it: all 0 between edges.
	readers []int32
}

// prepare checks that f is one the backends compile, and analyses and
// allocates it, with the architecture's allocatable registers.
func prepare(w *Workspace, f *ssa.Func, enc abi.Encoding, gprs, fprs []int) (*core, error) {
	f.Finish()
	c := &core{f: f, enc: enc, gprs: gprs, fprs: fprs, ws: w}
	c.sched = &c.own
	if w != nil {
		c.sched = &w.sched
	}
	if enc.ValueSize != 16 {
		// Slots and elements are found by shifting an index by four.
		return nil, fmt.Errorf("%w: %d-byte values", ErrUnsupported, enc.ValueSize)
	}
	if n := f.Locals + f.StackSize; n > abi.MaxRecords {
		return nil, fmt.Errorf("%w: %d slots", ErrUnsupported, n)
	}
	if f.FrameLocals < 0 || f.FrameLocals > f.Locals {
		return nil, fmt.Errorf("%w: %d frame locals of %d", ErrUnsupported, f.FrameLocals, f.Locals)
	}
	if f.ThisSlot != -1 && (f.ThisSlot < f.FrameLocals || f.ThisSlot >= f.Locals) {
		return nil, fmt.Errorf("%w: the receiver at slot %d", ErrUnsupported, f.ThisSlot)
	}
	if err := capturedUnchanged(f); err != nil {
		return nil, err
	}
	c.origin = ssa.Origins(f)
	for _, b := range f.Blocks {
		for _, v := range b.Values {
			if o, ok := c.origin.Of(v); ok && (o == ssa.OriginAmbiguous || o == ssa.OriginHeap) && v.Shadow == nil {
				return nil, fmt.Errorf("%w: %v merges two slots' values with no shadow", ErrUnsupported, v)
			}
		}
	}
	c.layout()
	c.findLazy()
	if err := c.allocate(); err != nil {
		return nil, err
	}
	return c, nil
}

// ErrUnsupported reports a function the backend does not compile yet.
var ErrUnsupported = errors.New("mir: unsupported")

// Code is a compiled function: its bytes, and the offset of the entry for
// each slot IR PC that has one.
type Code struct {
	Bytes   []byte
	Entries map[int]int
	// CallEntry is the offset of the code's call entry, where native
	// callers jump (abi.Context's ReturnTo says how), or -1 for none: the
	// frame and the context the function runs in made once, here, not at
	// every call. Resume is the offset of the routine Go goes on after a
	// native call through (jit's SSACode.Resume), or -1 for code that makes
	// none.
	CallEntry, Resume int
	// Text is how many of Bytes are instructions: past them are the code's
	// own constants (amd64's movImm), data.
	Text int
	// Exits are the descriptions the code's exits name by address
	// (abi.ExitDescriptor), which whoever holds the code keeps alive.
	Exits []*abi.ExitDescriptor
	// core is the compiler's state, for Locations.
	core *core
}

// Locations lists where each value lives, for tests and debugging: made on
// demand, since compiling at run time never asks.
func (c *Code) Locations() string { return c.core.describe() }

// loc is where a value lives: a register of its class, or a spill slot.
type loc struct {
	reg   int // register number, or -1
	spill int // spill slot, or -1
}

// isLazy reports whether v has no location of its own (core.lazy).
func (c *core) isLazy(v *ssa.Value) bool { return v.ID < len(c.lazy) && c.lazy[v.ID] }

// loc is v's location, and whether it has one.
func (c *core) loc(v *ssa.Value) (loc, bool) {
	if v.ID >= len(c.locs) || !c.hasLoc[v.ID] {
		return loc{}, false
	}
	return c.locs[v.ID], true
}

// locAt is v's location, or the zero location if it has none.
func (c *core) locAt(v *ssa.Value) loc {
	l, _ := c.loc(v)
	return l
}

func (c *core) setLoc(v *ssa.Value, l loc) {
	c.locs[v.ID], c.hasLoc[v.ID] = l, true
	if l.reg >= 0 {
		k := 0
		if isFloat(v) {
			k = 1
		}
		c.used[k] |= 1 << l.reg
	}
}

// usedRegs are those of regs, the general registers or the floating-point
// ones, the allocation gave a value, in regs' order: what the routines that
// save the registers for an exit or a call of Go save, as a register no
// value is in holds nothing either needs. Each list is made once, from the
// compile's tables.
func (c *core) usedRegs(regs []int, float bool) []int {
	k := 0
	if float {
		k = 1
	}
	if c.usedList[k] == nil {
		out := c.ints(bits.OnesCount64(c.used[k]))[:0]
		for _, r := range regs {
			if c.used[k]&(1<<r) != 0 {
				out = append(out, r)
			}
		}
		c.usedList[k] = out
	}
	return c.usedList[k]
}

// valueSet is a set of value IDs.
type valueSet []uint64

func (s valueSet) add(i int)      { s[i>>6] |= 1 << (i & 63) }
func (s valueSet) has(i int) bool { return s[i>>6]&(1<<(i&63)) != 0 }
func (s valueSet) each(f func(int)) {
	for w, word := range s {
		for word != 0 {
			f(w*64 + bits.TrailingZeros64(word))
			word &= word - 1
		}
	}
}

type stubKey struct {
	state *ssa.FrameState
	kind  uint64
}

// capturedUnchanged requires that no instruction write a captured binding,
// or the receiver, past the frame's locals:
// every frame state then holds its value from entry there (in whatever
// representation the optimizer chose), and exits leave those slots alone.
func capturedUnchanged(f *ssa.Func) error {
	for i := f.FrameLocals; i < f.Locals; i++ {
		if f.Written(i) {
			return fmt.Errorf("%w: captured slot %d is written", ErrUnsupported, i)
		}
	}
	return nil
}

// layout orders blocks as the function has them, in reverse post-order from
// the entries (ssa.Func.Blocks), so that a loop's body follows its header.
func (c *core) layout() {
	c.order = append(c.blockList(len(c.f.Blocks))[:0], c.f.Blocks...)
}

// findLazy marks values that need no location of their own.
func (c *core) findLazy() {
	n := c.f.NumValues()
	used := c.bools(n) // by an argument, a control or a phi
	for _, b := range c.f.Blocks {
		for _, v := range b.Values {
			for _, a := range v.Args {
				used[a.ID] = true
			}
		}
		if b.Control != nil && b.Kind != ssa.BlockReturn {
			used[b.Control.ID] = true
		}
	}
	// A slot loaded at an entry and named only by frame states for that same
	// slot needs no register: an exit leaves that slot as it is.
	elsewhere := c.bools(n)
	state := func(s *ssa.FrameState) {
		if s == nil {
			return
		}
		for i, v := range s.Slots {
			if v != nil && !(v.Op == ssa.OpLoadSlot && v.Aux == i) {
				elsewhere[v.ID] = true
			}
		}
	}
	for _, b := range c.f.Blocks {
		for _, v := range b.Values {
			for _, a := range v.Args {
				if a.Op == ssa.OpLoadSlot {
					elsewhere[a.ID] = true
				}
			}
			state(v.State)
		}
		state(b.State)
		state(b.Header)
		if b.Control != nil && b.Control.Op == ssa.OpLoadSlot {
			elsewhere[b.Control.ID] = true
		}
	}
	c.lazy = c.bools(n)
	for _, b := range c.f.Blocks {
		for _, v := range b.Values {
			switch v.Op {
			case ssa.OpConstSource:
				c.lazy[v.ID] = true
			case ssa.OpConst, ssa.OpBoxF64, ssa.OpBoxBool:
				if !used[v.ID] {
					c.lazy[v.ID] = true
				}
			case ssa.OpLoadSlot:
				if !elsewhere[v.ID] {
					c.lazy[v.ID] = true
				}
			}
		}
	}
}

// numBlocks bounds f's block IDs.
func numBlocks(f *ssa.Func) int {
	n := 0
	for _, b := range f.Blocks {
		n = max(n, b.ID+1)
	}
	return n
}

// allocate computes liveness and assigns locations by linear scan. Its
// tables are indexed by value and block ID: a compile at run time pays for
// every pass (BenchmarkJITCompile).
func (c *core) allocate() error {
	nv, nb := c.f.NumValues(), numBlocks(c.f)
	// Number positions: each block has a start, one position per value, and
	// an end. at is the block of each position.
	pos := c.ints(nv)
	for i := range pos {
		pos[i] = -1
	}
	start, end := c.ints(nb), c.ints(nb)
	count := 0
	for _, b := range c.order {
		count += len(b.Values) + 2
	}
	at := c.blockList(count)[:0]
	for _, b := range c.order {
		start[b.ID] = len(at)
		at = append(at, b)
		for _, v := range b.Values {
			pos[v.ID] = len(at)
			at = append(at, b)
		}
		end[b.ID] = len(at)
		at = append(at, b)
	}
	// Uses: each value's uses, by the position of the use.
	uses := c.useList(4 * count)[:0]
	need := func(v *ssa.Value) bool { return hasResult(v) && !c.isLazy(v) }
	useState := func(s *ssa.FrameState, at int) {
		if s == nil {
			return
		}
		for _, v := range s.Slots {
			if v == nil {
				continue
			}
			if c.isLazy(v) || remat(v) {
				for _, a := range v.Args {
					uses = append(uses, use{a, at})
				}
				continue
			}
			uses = append(uses, use{v, at})
			if v.Shadow != nil {
				uses = append(uses, use{v.Shadow, at})
			}
		}
	}
	for _, b := range c.order {
		for i, v := range b.Values {
			if v.Op == ssa.OpPhi {
				continue // phi arguments are used at their predecessors' ends
			}
			for _, a := range v.Args {
				uses = append(uses, use{a, pos[v.ID]})
				if a.Shadow != nil {
					// It may read where its operand came from: a keep
					// (mir's keep), or an operation that finds an object's
					// pointer word there (sourceRef). A value the frame
					// state names keeps its shadow alive there too, but a
					// captured local's is in no state (its slot is its
					// cell), and the slot IR reads a local as an operand.
					uses = append(uses, use{a.Shadow, pos[v.ID]})
				}
			}
			at := pos[v.ID]
			if v.Op == ssa.OpCall && len(v.Calls()) != 0 && (v.Calls()[0].Go != 0 || ssa.RefillsPools(v)) {
				// A call of Go that Go refuses exits after it, Go having
				// taken the registers: what its state reads lives across
				// it, saved and restored (saves). So does a call whose
				// pool Go fills, made again from its start: its operands
				// are in its state.
				at++
			}
			if i+1 < len(b.Values) && b.Values[i+1].State == v.State {
				// The guards of one instruction share its state: what it
				// reads is used at the last of them, which subsumes the
				// others, as they are in one block.
				continue
			}
			useState(v.State, at)
		}
		if b.Control != nil {
			if c.isLazy(b.Control) {
				for _, a := range b.Control.Args {
					uses = append(uses, use{a, end[b.ID]})
				}
			} else {
				uses = append(uses, use{b.Control, end[b.ID]})
				if b.Control.Shadow != nil {
					uses = append(uses, use{b.Control.Shadow, end[b.ID]})
				}
			}
		}
		useState(b.State, end[b.ID])
		if b.LoopHeader {
			useState(b.Header, start[b.ID])
		}
		for _, s := range b.Succs {
			for i, p := range s.Preds {
				if p != b {
					continue
				}
				for _, phi := range s.Values {
					if phi.Op != ssa.OpPhi {
						break
					}
					uses = append(uses, use{phi.Args[i], end[b.ID]})
				}
				if s.LoopHeader && s.Backedge[i] {
					// A poll on this edge exits to the header's state.
					useState(s.Header, end[b.ID])
				}
			}
		}
	}
	// Block-level liveness, so that values live around a loop stay live for
	// all of it. Only a value used outside the block that defines it can be
	// live into a block: those are numbered, and the sets are over them, a
	// fraction of the values. byID finds a value from its number.
	defBlock := c.blockList(nv)
	for _, b := range c.order {
		for _, v := range b.Values {
			defBlock[v.ID] = b
		}
	}
	gid := c.ints(nv) // 1 plus a value's number, or 0
	byID := c.valueList(nv)[:0]
	for _, u := range uses {
		if need(u.v) && gid[u.v.ID] == 0 && defBlock[u.v.ID] != at[u.pos] {
			byID = append(byID, u.v)
			gid[u.v.ID] = len(byID)
		}
	}
	// The three sets of every block share one array.
	words := (len(byID) + 63) / 64
	sets := c.words(3 * nb * words)
	defsIn, usesIn, liveIn := c.setList(nb), c.setList(nb), c.setList(nb)
	for _, b := range c.order {
		at := 3 * b.ID * words
		defsIn[b.ID], usesIn[b.ID], liveIn[b.ID] = sets[at:at+words:at+words], sets[at+words:at+2*words:at+2*words], sets[at+2*words:at+3*words:at+3*words]
		for _, v := range b.Values {
			if g := gid[v.ID]; g != 0 {
				defsIn[b.ID].add(g - 1)
			}
		}
	}
	for _, u := range uses {
		if g := gid[u.v.ID]; g != 0 && defBlock[u.v.ID] != at[u.pos] {
			usesIn[at[u.pos].ID].add(g - 1)
		}
	}
	in := c.words(words)
	for changed := true; changed; {
		changed = false
		for i := len(c.order) - 1; i >= 0; i-- {
			b := c.order[i]
			copy(in, usesIn[b.ID])
			defs := defsIn[b.ID]
			for _, s := range b.Succs {
				if live := liveIn[s.ID]; live != nil {
					for w := range in {
						in[w] |= live[w] &^ defs[w]
					}
				}
			}
			if !slices.Equal(in, liveIn[b.ID]) {
				copy(liveIn[b.ID], in)
				changed = true
			}
		}
	}
	// Intervals: from definition to last use, stretched over every block
	// where the value is live in or out.
	iv := c.intervalList(nv) // by value ID; v is nil until made
	get := func(v *ssa.Value) *interval {
		it := &iv[v.ID]
		if it.v == nil {
			p := pos[v.ID]
			if p < 0 {
				panic(fmt.Sprintf("use of %v defined outside the function", v))
			}
			if v.Op == ssa.OpPhi {
				p = start[v.Block.ID]
			}
			*it = interval{v: v, start: p, end: p}
		}
		return it
	}
	for _, b := range c.order {
		for _, v := range b.Values {
			if need(v) {
				get(v)
			}
		}
	}
	for _, u := range uses {
		if need(u.v) {
			if it := get(u.v); u.pos > it.end {
				it.end = u.pos
			}
		}
	}
	for _, b := range c.order {
		bs := start[b.ID]
		liveIn[b.ID].each(func(id int) {
			it := get(byID[id])
			if bs < it.start {
				it.start = bs
			}
			if bs > it.end {
				it.end = bs
			}
		})
		for _, s := range b.Succs {
			if liveIn[s.ID] == nil {
				continue
			}
			liveIn[s.ID].each(func(id int) {
				v := byID[id]
				if !need(v) || v.Block == s && v.Op == ssa.OpPhi {
					return
				}
				it := get(v)
				if end[b.ID] > it.end {
					it.end = end[b.ID]
				}
				if bs < it.start && !defsIn[b.ID].has(id) {
					it.start = bs
				}
			})
		}
	}
	// By start, then value ID: a counting sort over the positions, the
	// intervals taken in ID order.
	first := c.ints(count + 1)
	n := 0
	for i := range iv {
		if iv[i].v != nil {
			first[iv[i].start+1]++
			n++
		}
	}
	for p := 1; p <= count; p++ {
		first[p] += first[p-1]
	}
	all := c.intervalRefs(nv)[:n]
	for i := range iv {
		if it := &iv[i]; it.v != nil {
			all[first[it.start]] = it
			first[it.start]++
		}
	}
	c.locs, c.hasLoc = c.locList(nv), c.bools(nv)
	// A phi and its arguments are given one register where they can be, as
	// a linear scan's hints have it: the phi the register of an argument
	// that is free when the phi begins, an argument the phi's if free when
	// it begins. An edge's phi moves (phiMoves) are then no moves at all:
	// given whichever register came free last, a fifth of a function's
	// code had been the moves at its edges. phiOf is, by value ID, a phi
	// the value is an argument of, or nil.
	phiOf := c.valueList(nv)
	for _, b := range c.order {
		for _, v := range b.Values {
			if v.Op != ssa.OpPhi {
				break
			}
			for _, a := range v.Args {
				if a != v && phiOf[a.ID] == nil {
					phiOf[a.ID] = v
				}
			}
		}
	}
	// hinted is the register free holds that v is to share, or -1.
	hinted := func(v *ssa.Value, free []int) int {
		want := func(x *ssa.Value) int {
			if x == nil || !c.hasLoc[x.ID] {
				return -1
			}
			if l := c.locAt(x); l.reg >= 0 {
				if i := slices.Index(free, l.reg); i >= 0 {
					return i
				}
			}
			return -1
		}
		if v.Op == ssa.OpPhi {
			for _, a := range v.Args {
				if i := want(a); i >= 0 {
					return i
				}
			}
		}
		return want(phiOf[v.ID])
	}
	// Spill slots are reused as registers are: a spilled interval's slot is
	// free once it ends. Spilling an active interval puts all of it in the
	// slot, from its start, so a slot is taken only where its last
	// occupant ended before the interval began. The two classes' passes use
	// separate slots, since the second does not know when the first's
	// spilled values die.
	type freeSlot struct{ slot, end int }
	spills := 0
	// The intervals of each class, in order, for its pass.
	var classes [2][]*interval
	for k := range classes {
		classes[k] = c.intervalRefs(len(all))[:0]
	}
	for _, it := range all {
		if isFloat(it.v) {
			classes[1] = append(classes[1], it)
		} else {
			classes[0] = append(classes[0], it)
		}
	}
	for class, float := range [2]bool{false, true} {
		var freeSlots []freeSlot
		regs := c.gprs
		if float {
			regs = c.fprs
		}
		free := append(c.ints(len(regs))[:0], regs...)
		// Neither list outgrows the intervals. The spilled intervals, in
		// the order spilled, are found by when they end through a heap of
		// their indexes (soonest, then first spilled, first); those that
		// end before an interval starts free their slots in the order
		// spilled.
		active, spilled := c.intervalRefs(len(all))[:0], c.intervalRefs(len(all))[:0]
		ends, done := c.ints(len(all))[:0], c.ints(len(all))[:0]
		before := func(i, j int) bool {
			a, b := spilled[ends[i]], spilled[ends[j]]
			return a.end < b.end || a.end == b.end && ends[i] < ends[j]
		}
		push := func(k int) {
			ends = append(ends, k)
			for i := len(ends) - 1; i > 0; {
				up := (i - 1) / 2
				if !before(i, up) {
					break
				}
				ends[i], ends[up] = ends[up], ends[i]
				i = up
			}
		}
		pop := func() int {
			k := ends[0]
			last := len(ends) - 1
			ends[0] = ends[last]
			ends = ends[:last]
			for i := 0; ; {
				m, l, r := i, 2*i+1, 2*i+2
				if l < len(ends) && before(l, m) {
					m = l
				}
				if r < len(ends) && before(r, m) {
					m = r
				}
				if m == i {
					break
				}
				ends[i], ends[m] = ends[m], ends[i]
				i = m
			}
			return k
		}
		slot := func(from int, v *ssa.Value) (int, error) {
			// The slot of a phi or an argument of it v is to share, if
			// free (hinted).
			related := func(x *ssa.Value) int {
				if x == nil || !c.hasLoc[x.ID] {
					return -1
				}
				if l := c.locAt(x); l.reg < 0 && l.spill >= 0 {
					for i, f := range freeSlots {
						if f.slot == l.spill && f.end < from {
							return i
						}
					}
				}
				return -1
			}
			i := related(phiOf[v.ID])
			if v.Op == ssa.OpPhi {
				for _, a := range v.Args {
					if i < 0 {
						i = related(a)
					}
				}
			}
			if i >= 0 {
				s := freeSlots[i].slot
				freeSlots = append(freeSlots[:i], freeSlots[i+1:]...)
				return s, nil
			}
			for i := len(freeSlots) - 1; i >= 0; i-- {
				if f := freeSlots[i]; f.end < from {
					freeSlots = append(freeSlots[:i], freeSlots[i+1:]...)
					return f.slot, nil
				}
			}
			if spills == abi.SpillSlots {
				return 0, fmt.Errorf("%w: more than %d spill slots", ErrUnsupported, abi.SpillSlots)
			}
			spills++
			return spills - 1, nil
		}
		for _, it := range classes[class] {
			kept := active[:0]
			for _, a := range active {
				if a.end < it.start {
					free = append(free, c.locAt(a.v).reg)
				} else {
					kept = append(kept, a)
				}
			}
			active = kept
			done = done[:0]
			for len(ends) > 0 && spilled[ends[0]].end < it.start {
				k := pop()
				// In the order spilled: insertion, as few end at once.
				done = append(done, k)
				for i := len(done) - 1; i > 0 && done[i-1] > done[i]; i-- {
					done[i-1], done[i] = done[i], done[i-1]
				}
			}
			for _, k := range done {
				a := spilled[k]
				freeSlots = append(freeSlots, freeSlot{c.locAt(a.v).spill, a.end})
			}
			if len(free) > 0 {
				i := hinted(it.v, free)
				if i < 0 {
					i = len(free) - 1
				}
				c.setLoc(it.v, loc{reg: free[i], spill: -1})
				free = append(free[:i], free[i+1:]...)
				active = append(active, it)
				continue
			}
			// Spill whichever interval ends last.
			victim := it
			vi := -1
			for i, a := range active {
				if a.end > victim.end {
					victim, vi = a, i
				}
			}
			s, err := slot(victim.start, victim.v)
			if err != nil {
				return err
			}
			if victim == it {
				c.setLoc(it.v, loc{reg: -1, spill: s})
			} else {
				c.setLoc(it.v, loc{reg: c.locAt(victim.v).reg, spill: -1})
				c.setLoc(victim.v, loc{reg: -1, spill: s})
				active[vi] = it
			}
			spilled = append(spilled, victim)
			push(len(spilled) - 1)
		}
	}
	// What each native call saves: the values live across it in registers,
	// each in a slot of its own past the spill slots while it lives, which
	// a value saved later takes once it has ended, as the spill slots are
	// reused. The calls come in order, so the intervals begun before one
	// are kept as they are met, in their order, and dropped once they end.
	// home is, by value ID, 1 plus the slot a value saved across a call
	// is saved in, or 0.
	var home []int
	var free []int
	open, next := c.intervalRefs(len(all))[:0], 0
	for _, b := range c.order {
		for _, v := range b.Values {
			if v.Op != ssa.OpCall || v.Calls() == nil {
				continue
			}
			if home == nil {
				home, c.saveAt = c.ints(nv), c.ints(2*nv)
			}
			c.saveAt[2*v.ID] = len(c.saves)
			p := pos[v.ID]
			for ; next < len(all) && all[next].start < p; next++ {
				open = append(open, all[next])
			}
			kept := open[:0]
			for _, it := range open {
				if it.end > p {
					kept = append(kept, it)
				} else if s := home[it.v.ID]; s != 0 {
					free = append(free, s-1)
					home[it.v.ID] = 0
				}
			}
			open = kept
			for _, it := range open {
				l := c.locAt(it.v)
				if l.reg < 0 {
					continue
				}
				s := home[it.v.ID] - 1
				if s < 0 {
					switch {
					case len(free) != 0:
						s, free = free[len(free)-1], free[:len(free)-1]
					case spills == abi.SpillSlots:
						return fmt.Errorf("%w: more than %d spill slots", ErrUnsupported, abi.SpillSlots)
					default:
						s, spills = spills, spills+1
					}
					home[it.v.ID] = s + 1
				}
				c.saves = c.appendSaved(c.saves, saved{reg: l.reg, slot: s, float: isFloat(it.v)})
			}
			c.saveAt[2*v.ID+1] = len(c.saves)
		}
	}
	return c.colorKeeps(iv, pos)
}

// ErrKeeps reports a function whose keep cells, given back once dead, are
// still more than the context has (abi.MaxKeeps) at once: the VM builds it
// again with no more than that (ssa.KeepFeedback).
var ErrKeeps = errors.New("mir: more keep cells live at once than the context has")

// colorKeeps gives the keep cells the build numbered, one for each call's
// result and each kept value (ssa.Func.Keeps), cells of the context's by
// when each is live, as registers are given: a cell is written where its
// call or keep is, and read wherever a value whose pointer word is there
// -- the cell's address (OpCallCell, OpKeep), a value it is the shadow of,
// a phi of such addresses, or a value whose shadow that phi is -- is live,
// in a register, a slot or a frame state an exit reads. A cell no value
// reads after its write, a call's result no one uses, is live at the write
// alone, and calls one after another share one. Cells whose intervals do
// not meet share a cell, the first free, in the order they begin.
func (c *core) colorKeeps(iv []interval, pos []int) error {
	f := c.f
	nk := f.Keeps
	if nk == 0 {
		return nil
	}
	lo, hi := c.ints(nk), c.ints(nk)
	for k := range lo {
		lo[k], hi[k] = -1, -1
	}
	extend := func(k, a, b int) {
		if lo[k] < 0 || a < lo[k] {
			lo[k] = a
		}
		if b > hi[k] {
			hi[k] = b
		}
	}
	writes := func(v *ssa.Value) bool {
		return v.Op == ssa.OpCall && len(v.Calls()) != 0 || v.Op == ssa.OpKeep
	}
	// cell is, by value ID, 1 plus the cell an address is of.
	cell := c.ints(f.NumValues())
	var phis []*ssa.Value
	for _, b := range c.order {
		for _, v := range b.Values {
			switch {
			case writes(v):
				if v.Index < 0 || v.Index >= nk {
					return fmt.Errorf("%w: keep cell %d of %d", ErrUnsupported, v.Index, nk)
				}
				extend(v.Index, pos[v.ID], pos[v.ID])
				if v.Op == ssa.OpKeep {
					cell[v.ID] = v.Index + 1
				}
			case v.Op == ssa.OpCallCell:
				if w := v.Args[0]; writes(w) {
					cell[v.ID] = w.Index + 1
				}
			case v.Op == ssa.OpPhi && v.Type == ssa.Source:
				phis = append(phis, v)
			}
		}
	}
	// The cells each phi of addresses may be the address of: its
	// arguments', through phis of phis, until none grows.
	var merged map[int][]int
	if len(phis) != 0 {
		merged = map[int][]int{}
		for grew := true; grew; {
			grew = false
			for _, p := range phis {
				for _, a := range p.Args {
					add := func(k int) {
						if !slices.Contains(merged[p.ID], k) {
							merged[p.ID] = append(merged[p.ID], k)
							grew = true
						}
					}
					if k := cell[a.ID]; k != 0 {
						add(k - 1)
					}
					if a.Op == ssa.OpPhi && a.Type == ssa.Source {
						for _, k := range merged[a.ID] {
							add(k)
						}
					}
				}
			}
		}
	}
	cellsOf := func(x *ssa.Value, each func(int)) {
		if x == nil {
			return
		}
		if k := cell[x.ID]; k != 0 {
			each(k - 1)
		}
		for _, k := range merged[x.ID] {
			each(k)
		}
	}
	for i := range iv {
		it := &iv[i]
		if it.v == nil {
			continue
		}
		span := func(k int) { extend(k, it.start, it.end) }
		cellsOf(it.v, span)
		cellsOf(it.v.Shadow, span)
	}
	// In the order they begin, then by number.
	order := c.ints(nk)[:0]
	for k := range nk {
		if lo[k] >= 0 {
			order = append(order, k)
		}
	}
	slices.SortFunc(order, func(a, b int) int {
		if lo[a] != lo[b] {
			return lo[a] - lo[b]
		}
		return a - b
	})
	phys := c.ints(nk)
	type active struct{ end, cell int }
	var live []active
	var free []int
	n := 0
	for _, k := range order {
		kept := live[:0]
		for _, a := range live {
			if a.end < lo[k] {
				free = append(free, a.cell)
			} else {
				kept = append(kept, a)
			}
		}
		live = kept
		var at int
		if len(free) != 0 {
			slices.Sort(free)
			at, free = free[0], free[1:]
		} else {
			if n == abi.MaxKeeps {
				return ErrKeeps
			}
			at, n = n, n+1
		}
		phys[k] = at
		live = append(live, active{hi[k], at})
	}
	for _, b := range c.order {
		for _, v := range b.Values {
			if writes(v) {
				v.Index = phys[v.Index]
			}
		}
	}
	f.Keeps = n
	return nil
}

// savesOf is what native call v saves (saves).
func (c *core) savesOf(v *ssa.Value) []saved {
	if c.saveAt == nil || 2*v.ID+1 >= len(c.saveAt) {
		return nil
	}
	return c.saves[c.saveAt[2*v.ID]:c.saveAt[2*v.ID+1]]
}

// operandsLive reports whether a call's operands, a state's slots from ops
// on, all have a location or can be made: none is a load left lazy, which
// only its own slot holds, and the call writes them where its callee has
// them (directOperand). A call is never in an entry block, so none is.
func (c *core) operandsLive(s *ssa.FrameState, ops int) bool {
	for _, x := range s.Slots[ops:] {
		if x == nil || x.Op == ssa.OpLoadSlot && c.isLazy(x) {
			return false
		}
	}
	return true
}

// inlineLevels are the inlined frames an exit in state s makes, the
// outermost first (ssa.InlineState).
func inlineLevels(s *ssa.FrameState) []*ssa.InlineState {
	var levels []*ssa.InlineState
	for in := s.Inline; in != nil; in = in.Parent {
		levels = append(levels, in)
	}
	slices.Reverse(levels)
	return levels
}

// b2u is 1 for true, 0 for false.
func b2u(b bool) uint64 {
	if b {
		return 1
	}
	return 0
}

// entryAt is the entry block for pc, or nil if the code has none there.
func (c *core) entryAt(pc int) *ssa.Block {
	for _, e := range c.f.Entries {
		if e.PC == pc {
			return e.Block
		}
	}
	return nil
}

// cellSource reports whether a shadow is a cell's address whatever happens
// at run time -- a property's, an element's, a global binding's, the
// context's -- and never a slot or -1, so that the value's pointer word is
// read there with no decoding (sourceAddr).
func cellSource(s *ssa.Value) bool {
	switch s.Op {
	case ssa.OpPropCell, ssa.OpElemCell, ssa.OpGlobalCell, ssa.OpLexCell, ssa.OpBindingCell, ssa.OpStringMethod, ssa.OpStringChar, ssa.OpCodeChar, ssa.OpStringProp, ssa.OpCallCell, ssa.OpConstCell, ssa.OpUpvalueCell, ssa.OpLocalCell:
		return true
	}
	return false
}

// numberWord is a number's bits as a word: NaN canonical.
func (c *core) numberWord(bits uint64) uint64 {
	if math.IsNaN(math.Float64frombits(bits)) {
		return c.enc.CanonicalNaN
	}
	return bits
}

// constWord is a tagged constant's word.
func (c *core) constWord(k ir.Value) uint64 {
	switch k.Kind {
	case ir.Number:
		return c.numberWord(k.Bits)
	case ir.Undefined:
		return c.enc.Undefined
	case ir.Null:
		return c.enc.Null
	case ir.Boolean:
		if k.Bits != 0 {
			return c.enc.True
		}
		return c.enc.False
	case ir.Uninitialized:
		return c.enc.Uninitialized
	}
	panic(fmt.Sprintf("tagged constant of kind %d", k.Kind))
}
func exitKind(aux int) uint64 {
	if ir.ExitKind(aux) == ir.HostExit {
		return abi.ExitHost
	}
	return abi.ExitDeopt
}

// remat reports a value an exit recomputes from its operands rather than
// reads from its location. A poll exits on a back-edge, after the phi moves
// and before the header runs again, so a box the header computed of a phi
// would still hold the last iteration's value; its operand, the phi, holds
// this one's.
func remat(v *ssa.Value) bool {
	return v.Op == ssa.OpBoxF64 || v.Op == ssa.OpBoxBool || v.Op == ssa.OpConst
}

// describe lists each value's location, in block order.
func (c *core) describe() string {
	out := ""
	for _, b := range c.order {
		for _, v := range b.Values {
			l, ok := c.loc(v)
			switch {
			case c.isLazy(v):
				out += fmt.Sprintf("%v:lazy ", v)
			case !ok:
			case l.reg >= 0 && isFloat(v):
				out += fmt.Sprintf("%v:x%d ", v, l.reg)
			case l.reg >= 0:
				out += fmt.Sprintf("%v:r%d ", v, l.reg)
			default:
				out += fmt.Sprintf("%v:spill%d ", v, l.spill)
			}
		}
	}
	return out
}

// isFloat reports a value's register class: true for floating point.
func isFloat(v *ssa.Value) bool { return v.Type == ssa.Float64 }

func hasResult(v *ssa.Value) bool { return v.Type != ssa.None }

// use is a value's use, at the position of the instruction that uses it.
type use struct {
	v   *ssa.Value
	pos int
}

// interval is a value's live range over the linear instruction numbering.
type interval struct {
	v          *ssa.Value
	start, end int
}

func (c *core) spillDisp(slot int) int32 { return abi.OffSpill + int32(slot)*8 }

// captured reports whether a slot is past the frame's locals: a captured
// binding, or the receiver.
func (c *core) captured(slot int) bool { return slot >= c.f.FrameLocals && slot < c.f.Locals }

// phiStep is one step of an edge's parallel move: park a value in the
// scratch register of its class (park), or move src into dst's location,
// from scratch when parked.
type phiStep struct {
	park, dst, src *ssa.Value
	parked         bool
}

// phiSchedule orders the parallel move of an edge's phi arguments into the
// phis' locations: a move once no pending move still reads its destination,
// and a cycle broken by parking one source in scratch.
// phiMove is a move phiSchedule orders.
type phiMove struct{ dst, src *ssa.Value }

// locKey is v's location as an index, its class's apart: phiSchedule's
// readers'. It is -1 for a value with no location of its own, which no
// move waits for.
func (c *core) locKey(v *ssa.Value) int {
	l, ok := c.loc(v)
	if !ok || c.isLazy(v) {
		return -1
	}
	k := l.reg
	if k < 0 {
		k = 64 + l.spill
	}
	k *= 2
	if isFloat(v) {
		k++
	}
	return k
}

func (c *core) phiSchedule(to *ssa.Block, idx int) []phiStep {
	moves := c.sched.moves[:0]
	for _, phi := range to.Values {
		if phi.Op != ssa.OpPhi {
			break
		}
		if _, ok := c.loc(phi); !ok {
			continue // dead
		}
		src := phi.Args[idx]
		if !c.isLazy(src) && c.locAt(src) == c.locAt(phi) {
			continue
		}
		moves = append(moves, phiMove{phi, src})
	}
	// Locations of different classes never coincide: R10 is not X10.
	same := func(a, b *ssa.Value) bool {
		if c.isLazy(a) || c.isLazy(b) || isFloat(a) != isFloat(b) {
			return false
		}
		return c.locAt(a) == c.locAt(b)
	}
	steps := c.sched.steps[:0]
	c.sched.parked = c.sched.parked[:0]
	isParked := func(v *ssa.Value) bool { return slices.Contains(c.sched.parked, v) }
	// A move waits while a pending move, its source not parked, reads its
	// destination: counted by location, so that telling takes no search.
	if n := 2 * (64 + abi.SpillSlots); len(c.sched.readers) < n {
		c.sched.readers = make([]int32, n)
	}
	readers := c.sched.readers
	for _, m := range moves {
		if k := c.locKey(m.src); k >= 0 {
			readers[k]++
		}
	}
	for len(moves) > 0 {
		progress := false
		for i := 0; i < len(moves); i++ {
			m := moves[i]
			if k := c.locKey(m.dst); k >= 0 && readers[k] > 0 {
				continue
			}
			parked := isParked(m.src)
			if k := c.locKey(m.src); k >= 0 && !parked {
				readers[k]--
			}
			steps = append(steps, phiStep{dst: m.dst, src: m.src, parked: parked})
			moves = append(moves[:i], moves[i+1:]...)
			i--
			progress = true
		}
		if !progress {
			// Every move is blocked, so they form cycles. Park the value
			// that blocks the first move -- the one in its destination -- in
			// scratch, which frees that destination. Its own move then reads
			// scratch. The cycle unwinds before another needs scratch.
			var blocker *ssa.Value
			for _, o := range moves {
				if same(o.src, moves[0].dst) && !isParked(o.src) {
					blocker = o.src
					break
				}
			}
			if blocker == nil {
				panic("phi moves: blocked without a blocker")
			}
			// One scratch register per class: a second value parked while
			// another of its class still waits would overwrite it. A cycle
			// unwinds before the next needs scratch, so this cannot happen;
			// if it did, the function is refused, never miscompiled.
			for _, o := range moves {
				if isParked(o.src) && isFloat(o.src) == isFloat(blocker) {
					panic("phi moves: scratch already holds a parked value")
				}
			}
			steps = append(steps, phiStep{park: blocker})
			c.sched.parked = append(c.sched.parked, blocker)
			// A parked source is read from scratch, not its location.
			for _, o := range moves {
				if o.src == blocker {
					readers[c.locKey(blocker)]--
				}
			}
		}
	}
	c.sched.moves, c.sched.steps = moves[:0], steps
	return steps
}

// Compile compiles f for the architecture the program runs on, given the
// VM's value encoding: amd64 or arm64.
func Compile(f *ssa.Func, enc abi.Encoding) (*Code, error) {
	if runtime.GOARCH == "arm64" {
		return CompileARM64(f, enc)
	}
	return CompileAMD64(f, enc)
}

// TableExits has an exit that returns to Go leave its frame to Go to write
// from a description (abi.ExitDescriptor), as V8's deoptimizer reads a
// frame state's translation, rather than write it itself: the exit's code
// is a jump. It is a switch for tests to compare the two by.
var TableExits = true

// exitDescriptor describes the exit to state s of the given kind, its
// frame's locals and operands at the registers localsReg and stackReg, as
// exitThen's code writes it; or nil where a value is where a description
// cannot say, and the exit's code writes the frame itself.
func (c *core) exitDescriptor(s *ssa.FrameState, kind uint64, localsReg, stackReg uint8) *abi.ExitDescriptor {
	if c.exitFrame == nil {
		enc := c.enc
		c.exitFrame = &abi.ExitFrame{FrameLocals: int32(c.f.FrameLocals), ThisSlot: int32(c.f.ThisSlot), Locals: int32(c.f.Locals), Enc: &enc}
		if w := c.ws; w != nil {
			// The workspace's, kept from compile to compile.
			if w.exitSlots == nil {
				w.exitSlots, w.exitWords = map[abi.ExitSlot]uint16{}, map[uint64]int32{}
			}
			clear(w.exitSlots)
			clear(w.exitWords)
			c.exitSlots, c.exitWords = w.exitSlots, w.exitWords
			c.exitFrame.Slots, c.exitFrame.Index, c.exitFrame.Words = w.exitSlotsBuf[:0], w.exitIndexBuf[:0], w.exitWordsBuf[:0]
		} else {
			c.exitSlots, c.exitWords = map[abi.ExitSlot]uint16{}, map[uint64]int32{}
		}
	}
	f := c.exitFrame
	d := abi.ExitDescriptor{Kind: uint32(kind), PC: uint32(s.PC), Depth: uint32(s.Depth), Site: int32(s.Site),
		Frame: f, Start: uint32(len(f.Index)), LocalsReg: localsReg, StackReg: stackReg}
	written := func(i int, v *ssa.Value) bool {
		return v != nil && (v.Op != ssa.OpLoadSlot || v.Aux != i) && !c.captured(i)
	}
	// What it adds to the frame's, taken back where a slot cannot be
	// described.
	index, slots, words := len(f.Index), len(f.Slots), len(f.Words)
	undo := func() *abi.ExitDescriptor {
		for _, e := range f.Slots[slots:] {
			delete(c.exitSlots, e)
		}
		for _, w := range f.Words[words:] {
			delete(c.exitWords, w)
		}
		f.Index, f.Slots, f.Words = f.Index[:index], f.Slots[:slots], f.Words[:words]
		return nil
	}
	direct := true
	writes := func(i int32) bool { return int(i) < len(s.Slots) && written(int(i), s.Slots[i]) }
	for i, v := range s.Slots {
		if !written(i, v) {
			continue
		}
		e := abi.ExitSlot{Slot: int32(i), Origin: -1}
		var ok bool
		if e.Value, ok = c.exitValue(v); !ok {
			return undo()
		}
		if e.Value.Kind == abi.ExitConst {
			w := c.constWord(v.Const)
			k, ok := c.exitWords[w]
			if !ok {
				k = int32(len(f.Words))
				f.Words = append(f.Words, w)
				c.exitWords[w] = k
			}
			e.Value.N = k
		}
		if v.Shadow != nil {
			if e.Shadow, ok = c.exitSource(v.Shadow); !ok {
				return undo()
			}
		} else if o, has := c.origin.Of(v); has && o >= 0 {
			e.Origin, e.Load = int32(o), v.Op == ssa.OpLoadSlot
		}
		// Written one by one if no slot's reference may come from another
		// slot written here: by its origin, or a source that is one's
		// address -- a slot's, or one known only at run time.
		switch {
		case e.Shadow.Kind == abi.ExitReg || e.Shadow.Kind == abi.ExitSpill,
			e.Shadow.Kind == abi.ExitSlotAddr && e.Shadow.N != e.Slot && writes(e.Shadow.N),
			e.Origin >= 0 && e.Origin != e.Slot && writes(e.Origin):
			direct = false
		}
		k, ok := c.exitSlots[e]
		if !ok {
			if len(f.Slots) > math.MaxUint16 {
				// More than an index names: the exit's code writes it.
				return undo()
			}
			k = uint16(len(f.Slots))
			f.Slots = append(f.Slots, e)
			c.exitSlots[e] = k
		}
		f.Index = append(f.Index, k)
	}
	if len(f.Index)-index > math.MaxUint16 {
		return undo()
	}
	d.Count, d.Direct = uint16(len(f.Index)-index), direct
	if s.Inline != nil {
		for _, in := range inlineLevels(s) {
			k := uint64(abi.ExitHost)
			if in == s.Inline {
				k = kind
			}
			d.Inline = append(d.Inline, abi.ExitInline{Closure: uint64(in.Closure), Callee: uint64(in.Callee), Locals: uint64(in.Locals),
				ThisSlot: uint64(in.ThisSlot + 1), Kind: k, PC: uint64(in.PC), Depth: uint64(in.Depth),
				Site: int64(in.Site), Base: int64(in.Base)})
		}
		d.Kind = uint32(abi.ExitHost)
	}
	if len(c.exitDescs) == cap(c.exitDescs) {
		c.exitDescs = make([]abi.ExitDescriptor, 0, max(min(2*cap(c.exitDescs), 32), 4))
	}
	c.exitDescs = append(c.exitDescs, d)
	return &c.exitDescs[len(c.exitDescs)-1]
}

// finishExits gives the code's descriptions' frame (abi.ExitFrame) arrays
// of their own, as long as what they hold: grown by doubling, they could
// hold twice that, for as long as the code lives.
func (c *core) finishExits() {
	if f := c.exitFrame; f != nil {
		if w := c.ws; w != nil {
			// Its buffers, for the next compile.
			w.exitSlotsBuf, w.exitIndexBuf, w.exitWordsBuf = f.Slots[:0], f.Index[:0], f.Words[:0]
		}
		f.Slots, f.Index, f.Words = exact(f.Slots), exact(f.Index), exact(f.Words)
	}
}

// exact is s in an array of its length, or nil for none.
func exact[T any](s []T) []T {
	if len(s) == 0 {
		return nil
	}
	return slices.Clone(s)
}

// exitValue is where an exit finds a tagged value's word: its register or
// spill slot, or what it is made from (materialize).
func (c *core) exitValue(v *ssa.Value) (abi.ExitLoc, bool) {
	at := func(x *ssa.Value, reg, spill uint8) (abi.ExitLoc, bool) {
		l, ok := c.loc(x)
		switch {
		case !ok || c.isLazy(x):
			return abi.ExitLoc{}, false
		case l.reg >= 0:
			return abi.ExitLoc{Kind: reg, N: int32(l.reg)}, true
		}
		return abi.ExitLoc{Kind: spill, N: int32(l.spill)}, true
	}
	switch v.Op {
	case ssa.OpConst:
		// Its word is the description's (exitDescriptor).
		return abi.ExitLoc{Kind: abi.ExitConst}, true
	case ssa.OpBoxF64:
		return at(v.Args[0], abi.ExitF64Reg, abi.ExitF64Spill)
	case ssa.OpBoxBool:
		return at(v.Args[0], abi.ExitBoolReg, abi.ExitBoolSpill)
	}
	if isFloat(v) {
		return abi.ExitLoc{}, false
	}
	return at(v, abi.ExitReg, abi.ExitSpill)
}

// exitSource is where an exit finds a source's address: its register or
// spill slot; a constant source's, a slot's or none.
func (c *core) exitSource(s *ssa.Value) (abi.ExitLoc, bool) {
	if s.Op == ssa.OpConstSource {
		if s.Aux < 0 {
			return abi.ExitLoc{Kind: abi.ExitConst, N: -1}, true
		}
		return abi.ExitLoc{Kind: abi.ExitSlotAddr, N: int32(s.Aux)}, true
	}
	l, ok := c.loc(s)
	switch {
	case !ok || c.isLazy(s) || isFloat(s):
		return abi.ExitLoc{}, false
	case l.reg >= 0:
		return abi.ExitLoc{Kind: abi.ExitReg, N: int32(l.reg)}, true
	}
	return abi.ExitLoc{Kind: abi.ExitSpill, N: int32(l.spill)}, true
}

// goOpWord is what a call of Go stores as its operation (abi.Context's
// GoOp): a call's with its arguments' count and kind, which Go would
// otherwise read from the bytecode.
func goOpWord(site *ssa.CallSite) uint64 {
	op := uint64(site.Go)
	if site.Go == ssa.GoClosure || site.Go == ssa.GoClose || site.Go == ssa.GoCompare {
		// The template's constant, the first local closed, or the operator
		// compared with (abi.GoOpIndexShift).
		op |= uint64(uint32(site.Argc)) << abi.GoOpIndexShift
	}
	if site.Go == ssa.GoConcat {
		op |= uint64(site.Argc) << abi.GoOpArgcShift
	}
	if site.Go == ssa.GoCall {
		op |= uint64(site.Argc) << abi.GoOpArgcShift
		switch {
		case site.Method:
			op |= abi.GoCallMethod << abi.GoOpKindShift
		case site.New:
			op |= abi.GoCallNew << abi.GoOpKindShift
		case site.ForOf:
			op |= abi.GoCallForOf << abi.GoOpKindShift
		case site.Fill:
			op |= abi.GoCallFill << abi.GoOpKindShift
		case site.Step:
			op |= abi.GoCallStep << abi.GoOpKindShift
		}
	}
	return op
}
