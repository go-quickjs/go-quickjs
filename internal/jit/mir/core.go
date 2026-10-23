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
	lazy []bool
	// fused values are computed by the branch that ends their block, as a
	// compare and a conditional jump, rather than where they are, as a
	// boolean (findFused). Each is lazy too.
	fused  []bool
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
	// its slots and words each once, found again by exitHead and
	// exitWords; exitDescs are the descriptions, carved from blocks, as
	// the code keeps them all as long: each block twice the last, up to a
	// limit, so a small function takes little. exitHead is, by slot, 1
	// plus the index in the frame's Slots of the entry of it last found or
	// added, or 0, and exitNext, by entry, 1 plus that of the next in its
	// slot's chain, or 0: a slot's entries are compared one by one, the
	// last found first, where a map had hashed every one -- a twentieth of
	// a compile's time in the backend.
	exitFrame *abi.ExitFrame
	exitHead  []int32
	exitNext  []int32
	exitWords map[uint64]int32
	exitDescs []abi.ExitDescriptor
	// calls are the descriptions of the native calls' states (callState),
	// which the code keeps (Code.Calls); goCalls its calls of Go's
	// (goState).
	calls   []*abi.CallState
	goCalls []*abi.GoCallState
	// keepRuns marks, by value ID, the first (keepFirst) and the last
	// (keepLast) of each run of keeps (ssa's OpKeep) one after another in
	// a block: the keeps a store or a call makes (ssa's keepAcrossStores),
	// between which nothing runs that could change the collector's phase,
	// so that the run tests the write-barrier flag once.
	keepRuns []int
	// dropped marks, by value ID, the native calls whose results nothing
	// uses (resultDropped).
	dropped []bool
	// budgets are the loops whose back edges count the back-edge budget
	// in a register (findBudgets), and budgetOf, by block ID, 1 plus the
	// index of the one a block is in, or 0.
	budgets  []budget
	budgetOf []int
	// iv and pos are the intervals and positions the last allocation
	// found (allocate), for colorKeeps; masks loopMasks's.
	iv    []interval
	pos   []int
	masks valueSet
}

// budget is an innermost loop that calls nothing (findBudgets): its back
// edges count the interpreter's back-edge budget (abi.Context's BackEdges)
// in a register of its own, loaded where the loop is entered and stored
// back wherever Go may read it -- before a call of Go, at an exit, where
// the loop is left -- rather than decrementing the counter in memory each
// time round: a store and a load on every turn's critical path, which
// store-to-load forwarding made the bound of tight loops (loop_sum's
// several cycles a turn), and whose cost hung on where the loop landed.
// v is the register's value, made for the allocator (no SSA value): if it
// is spilled, the loop counts in memory as any other.
type budget struct {
	header *ssa.Block
	in     []bool // by block ID
	v      *ssa.Value
}

// budgetReg is the register b counts in, or -1 if it counts in memory.
func (c *core) budgetReg(b int) int {
	if b < 0 {
		return -1
	}
	if l, ok := c.loc(c.budgets[b].v); ok && l.reg >= 0 {
		return l.reg
	}
	return -1
}

// loopBudget is the budget loop block b is in, or -1.
func (c *core) loopBudget(b *ssa.Block) int {
	if b.ID < len(c.budgetOf) {
		return c.budgetOf[b.ID] - 1
	}
	return -1
}

// findBudgets finds the loops that count their budget in a register
// (budget): each loop's body -- the blocks its header reaches that reach
// one of its back edges without passing it -- that holds no other loop's
// header, no call, native or Go's (OpCall), and no call in tail position,
// whose own poll counts in memory: blocks whose code reads or writes the
// counter but for the calls of Go any operation may make, which
// goCalling stores it back around.
func (c *core) findBudgets() {
	f := c.f
	nb := numBlocks(f)
	var stack []*ssa.Block
	var chains chainTables
	for _, h := range f.Blocks {
		if !h.LoopHeader {
			continue
		}
		in := c.bools(nb)
		in[h.ID] = true
		stack = stack[:0]
		for i, p := range h.Preds {
			if h.Backedge[i] && !in[p.ID] {
				in[p.ID] = true
				stack = append(stack, p)
			}
		}
		for len(stack) > 0 {
			b := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			for _, p := range b.Preds {
				if !in[p.ID] {
					in[p.ID] = true
					stack = append(stack, p)
				}
			}
		}
		// Of those, the blocks the header reaches: an entry at a PC in the
		// loop's body (an entry block, then the body's blocks after it)
		// reaches a back edge without passing the header, but is no part
		// of the loop, which the edge from it enters (edge's load).
		fwd := c.bools(nb)
		fwd[h.ID] = true
		stack = append(stack[:0], h)
		for len(stack) > 0 {
			b := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			for _, s := range b.Succs {
				if in[s.ID] && !fwd[s.ID] {
					fwd[s.ID] = true
					stack = append(stack, s)
				}
			}
		}
		in = fwd
		ok := true
		for _, b := range f.Blocks {
			if !in[b.ID] {
				continue
			}
			if b != h && b.LoopHeader || b.ID < len(c.budgetOf) && c.budgetOf[b.ID] != 0 {
				ok = false
				break
			}
			for _, v := range b.Values {
				if v.Op == ssa.OpCall || v.Op == ssa.OpTailSelf {
					ok = false
					break
				}
			}
			if !ok {
				break
			}
		}
		if !ok || c.turnChain(h, in, &chains) >= counterChain {
			continue
		}
		if c.budgetOf == nil {
			c.budgetOf = c.ints(nb)
		}
		c.budgets = append(c.budgets, budget{header: h, in: in, v: &ssa.Value{ID: f.NumValues() + len(c.budgets), Type: ssa.Int64}})
		for id, x := range in {
			if x {
				c.budgetOf[id] = len(c.budgets)
			}
		}
	}
}

// counterChain is how long, in cycles, a loop's turn waits on a budget
// counted in memory: the counter's load, its decrement and its store,
// forwarded to the next turn's load -- microbench's loop_sum, its turn an
// addition, ran 1.3 to 1.7 ns a turn counting so.
const counterChain = 6

// turnChain is how long a turn of the loop of header h, its blocks in,
// waits on itself: of each header phi, the latency (latency) of the
// longest path through the loop's values from the phi to its value on a
// back edge. A loop whose chain is as long as the counter's own
// (counterChain) or longer hides the counter in memory, and a register
// would save it nothing a turn, while each entry paid the register's load
// and each exit its store: AWFY's List, 1.5 to 2.9% slower with the
// budget in a register, whose isShorterThan walks two lists of a few
// nodes a call (x = x.next: an object's check, its shape's, its field's
// cell, the field, each a load the next waits on).
func (c *core) turnChain(h *ssa.Block, in []bool, t *chainTables) int {
	nv := c.f.NumValues()
	if t.at == nil {
		t.at, t.memo, t.seen = c.ints(nv), c.ints(nv), c.ints(nv)
	}
	// at is, by value ID, the loop being looked at (t.loop) where the value
	// is in it, negated for a header phi; seen, the phi being looked at
	// (t.phi) where memo holds the longest path to the value from it, -1
	// where none reaches it, -2 while it is being found (a cycle through a
	// phi of a block in the loop).
	t.loop++
	for _, b := range c.f.Blocks {
		if in[b.ID] {
			for _, v := range b.Values {
				t.at[v.ID] = t.loop
				if b == h && v.Op == ssa.OpPhi {
					t.at[v.ID] = -t.loop
				}
			}
		}
	}
	var path func(v, p *ssa.Value) int
	path = func(v, p *ssa.Value) int {
		if v == p {
			return 0
		}
		if v.ID >= nv || t.at[v.ID] != t.loop {
			return -1 // outside the loop, or another header phi
		}
		if t.seen[v.ID] == t.phi {
			return max(t.memo[v.ID], -1)
		}
		t.seen[v.ID], t.memo[v.ID] = t.phi, -2
		d := -1
		for _, a := range v.Args {
			d = max(d, path(a, p))
		}
		if d >= 0 && v.Op != ssa.OpPhi {
			d += latency(v.Op)
		}
		t.memo[v.ID] = d
		return d
	}
	chain := 0
	for _, p := range h.Values {
		if p.Op != ssa.OpPhi {
			break
		}
		t.phi++
		for i, a := range p.Args {
			if h.Backedge[i] && in[h.Preds[i].ID] {
				chain = max(chain, path(a, p))
			}
		}
	}
	return chain
}

// chainTables are turnChain's tables, made once for a compile's loops.
type chainTables struct {
	at, memo, seen []int
	loop, phi      int
}

// latency is how many cycles an operation's result takes from its
// operands', as amd64's code computes it: a load's four, a floating-point
// addition's three, an integer operation's one; a call of Go's a hundred.
func latency(op ssa.Op) int {
	switch op {
	case ssa.OpObjectOf, ssa.OpArrayOf, ssa.OpIndexedOf, ssa.OpElemRead, ssa.OpElemCell,
		ssa.OpLength, ssa.OpPropRead, ssa.OpPropCell, ssa.OpLoadCell, ssa.OpStringProp,
		ssa.OpStringCode, ssa.OpStringChar, ssa.OpCodeChar, ssa.OpSubstringChar,
		ssa.OpUpvalueCell, ssa.OpLocalCell, ssa.OpGlobalCell, ssa.OpLexCell, ssa.OpBindingCell:
		return 4
	case ssa.OpAddF64, ssa.OpSubF64, ssa.OpMulF64, ssa.OpMaxF64, ssa.OpMinF64, ssa.OpSignF64,
		ssa.OpMulI32, ssa.OpMulI64, ssa.OpMulW, ssa.OpMulU64:
		return 3
	case ssa.OpToInt32, ssa.OpI32ToF64, ssa.OpU32ToF64, ssa.OpFroundF64, ssa.OpBigOfF64:
		return 4
	case ssa.OpFloorF64, ssa.OpCeilF64, ssa.OpTruncF64, ssa.OpRoundF64:
		return 6
	case ssa.OpDivF64:
		return 13
	case ssa.OpSqrtF64:
		return 20
	case ssa.OpDivI64, ssa.OpModI64, ssa.OpDivU64, ssa.OpModU64:
		return 40
	case ssa.OpModF64, ssa.OpPowF64, ssa.OpPowI64, ssa.OpMathCall, ssa.OpStringMethod,
		ssa.OpInstanceOf, ssa.OpBigBox:
		return 100
	}
	return 1
}

// resultDropped reports a call whose result nothing uses: its return
// stores it nowhere, neither in its keep cell nor in a register, and so
// tests no write-barrier flag.
func (c *core) resultDropped(v *ssa.Value) bool { return v.ID < len(c.dropped) && c.dropped[v.ID] }

const (
	keepFirst = 1 << iota
	keepLast
)

// saved is a register a call saves, and the spill slot it saves it in; or,
// if frame is not 0, the frame's slot frame-1 it reads it back from, which
// holds its word throughout the call (frameResident), with nothing saved.
//
// id is the value saved, by ID: a register the call saves holds that
// value, and no other the code may also have kept there before or after
// it (callWord).
type saved struct {
	reg, slot int
	float     bool
	frame     int
	id        int
}

// frameResident reports whether x, live across native call v, is the word
// of a frame's slot that holds it throughout the call, and which: a local's
// or the receiver's value loaded at an entry (or a number it is unboxed
// to), in a slot no closure assigns -- native code writes the frame only at
// its exits, the callee its own frame -- and that v's state names it in
// (or leaves out), so that Go, making this frame should the callee leave
// native code (jit.ExpandCall), writes nothing else there. The call then
// reads it back from there, as V8's code reloads a parameter from the slot
// its caller pushed, and saves nothing: AWFY Towers' moveDisks had saved
// its receiver and its unboxed count at each call.
func (c *core) frameResident(x, v *ssa.Value) (int, bool) {
	sites := v.Calls()
	if v.Op != ssa.OpCall || len(sites) == 0 || sites[0].Go != 0 || len(sites[0].Gens) != 0 {
		return 0, false
	}
	for _, t := range sites {
		if t.Super {
			// It binds the receiver, in the context's This, before what
			// it saved is read back.
			return 0, false
		}
	}
	y := x
	if x.Op == ssa.OpUnboxF64 {
		y = x.Args[0]
	}
	if y.Op != ssa.OpLoadSlot {
		return 0, false
	}
	o := y.Aux
	if !(o >= 0 && o < c.f.FrameLocals && !c.f.Assigned(o) || o >= 0 && o == c.f.ThisSlot) {
		return 0, false
	}
	if s := v.State; s == nil || o >= len(s.Slots) || s.Slots[o] != nil && s.Slots[o] != y {
		return 0, false
	}
	return o, true
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
	if enc.StoreRef == 0 {
		// While the collector marks, every pointer the code stores goes
		// through Go's write barrier there.
		return nil, fmt.Errorf("%w: no StoreRef", ErrUnsupported)
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
	if f.Unsafe != "" {
		return nil, fmt.Errorf("%w: %s", ErrUnsupported, f.Unsafe)
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
	c.findBudgets()
	if err := c.allocateBest(); err != nil {
		return nil, err
	}
	c.findKeepRuns()
	return c, nil
}

// findKeepRuns marks the runs of keeps (keepRuns): each keep made, of a
// block's values one after another.
func (c *core) findKeepRuns() {
	isKeep := func(v *ssa.Value) bool { return v.Op == ssa.OpKeep && !c.isLazy(v) }
	for _, b := range c.f.Blocks {
		for i, v := range b.Values {
			if !isKeep(v) {
				continue
			}
			if c.keepRuns == nil {
				c.keepRuns = c.ints(c.f.NumValues())
			}
			if i == 0 || !isKeep(b.Values[i-1]) {
				c.keepRuns[v.ID] |= keepFirst
			}
			if i+1 == len(b.Values) || !isKeep(b.Values[i+1]) {
				c.keepRuns[v.ID] |= keepLast
			}
		}
	}
}

// keepRun is what of a run of keeps v is (keepFirst, keepLast).
func (c *core) keepRun(v *ssa.Value) int {
	if v.ID < len(c.keepRuns) {
		return c.keepRuns[v.ID]
	}
	return 0
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
	// GenEntry is the offset of the entry a loop's step that resumes a
	// generator of the function natively jumps to (ssa.Func's GenEntry),
	// or -1 for none.
	GenEntry int
	// Text is how many of Bytes are instructions: past them are the code's
	// own constants (amd64's movImm), data.
	Text int
	// Calls are the descriptions of its native calls' states, which the
	// calls name by address (abi.Context's CallState), as Exits; CallRefs
	// bounds the pointer words a call writes itself (abi.CallRecord's Ref),
	// the context's RecordRef[:CallRefs].
	Calls    []*abi.CallState
	CallRefs int
	// GoCalls are the descriptions of its calls of Go, which the calls name
	// by address (abi.Context's GoState).
	GoCalls []*abi.GoCallState
	// Exits are the descriptions the code's exits name by address
	// (abi.ExitDescriptor), which whoever holds the code keeps alive.
	Exits []*abi.ExitDescriptor
	// Marks say what each stretch of Bytes was emitted for, in the order
	// of their offsets, and Listing is the SSA compiled: both only while
	// Annotate is set, for a profiler of native code.
	Marks   []Mark
	Listing string
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

// isFused reports whether v is computed by its block's branch (core.fused).
func (c *core) isFused(v *ssa.Value) bool { return v.ID < len(c.fused) && c.fused[v.ID] }

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

// budgetStub is a stub of a budget loop's exit (stubLabel).
type budgetStub struct {
	state *ssa.FrameState
	kind  uint64
	loop  int
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
		// The guards of one instruction share its state, looked at once.
		var last *ssa.FrameState
		for _, v := range b.Values {
			for _, a := range v.Args {
				if a.Op == ssa.OpLoadSlot {
					elsewhere[a.ID] = true
				}
			}
			if v.State != last {
				state(v.State)
				last = v.State
			}
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
			case ssa.OpKept:
				// A BigInt's box only frame states name, which exits make
				// of the BigInt (exitBig), as V8's deoptimizer
				// materializes it: neither it nor its cell is made.
				if box, cell := v.Args[1], v.Args[0]; box.Op == ssa.OpBigBox && !used[v.ID] && box.Uses == 2 && cell.Uses == 1 {
					c.lazy[v.ID], c.lazy[box.ID], c.lazy[cell.ID] = true, true, true
				}
			}
		}
	}
	c.findFused()
}

// fusable reports whether a boolean operation can be computed by a branch
// as its test, as a compare and a conditional jump (each backend's test),
// and whether it is pure: it reads nothing but its operands, in their
// locations, nor exits, so that what is emitted between it and the branch
// changes nothing it reads, its operands kept live to the branch.
func fusable(op ssa.Op) (ok, pure bool) {
	switch op {
	case ssa.OpNot, ssa.OpCmpF64, ssa.OpCmpI64, ssa.OpStrictNullish, ssa.OpCellNone:
		return true, true
	case ssa.OpLooseNullish, ssa.OpEqTagged, ssa.OpTypeIs, ssa.OpInstanceOf, ssa.OpTruth:
		// They read the objects and strings their operands are, through
		// where they came from (origin.go), and exit to their states: they
		// are fused only where nothing is emitted after them in the block.
		return true, false
	}
	return false, false
}

// findFused marks the values an If block's branch computes itself (fused):
// its control, if it is a comparison or a test (fusable) that nothing but
// the branch uses -- no other value, no frame state, no other block -- and,
// through an OpNot so fused, which only swaps the branch's targets, its
// operand, as far as the chain goes. Such a value needs no location: the
// branch jumps on the flags it sets, where a boolean had been set from
// them, and tested again.
func (c *core) findFused() {
	n := c.f.NumValues()
	c.fused = c.bools(n)
	// Uses, counted here rather than taken from ssa.Value's Uses: a fused
	// value has no location, which any other use would need.
	uses := c.ints(n)
	state := func(s *ssa.FrameState) {
		if s != nil {
			for _, v := range s.Slots {
				if v != nil {
					uses[v.ID]++
				}
			}
		}
	}
	for _, b := range c.f.Blocks {
		for _, v := range b.Values {
			for _, a := range v.Args {
				uses[a.ID]++
			}
			if v.Shadow != nil {
				uses[v.Shadow.ID]++
			}
			state(v.State)
		}
		if b.Control != nil {
			uses[b.Control.ID]++
		}
		state(b.State)
		state(b.Header)
	}
	for _, b := range c.f.Blocks {
		if b.Kind != ssa.BlockIf {
			continue
		}
		for v := b.Control; v != nil && v.Block == b && uses[v.ID] == 1 && !c.lazy[v.ID] && v.Shadow == nil; {
			ok, pure := fusable(v.Op)
			if !ok || !pure && !c.lastEmitted(b, v) {
				break
			}
			c.fused[v.ID], c.lazy[v.ID] = true, true
			if v.Op != ssa.OpNot {
				break
			}
			v = v.Args[0]
		}
	}
}

// lastEmitted reports whether v is the last value of b emitted: what
// follows it is lazy, fused values included.
func (c *core) lastEmitted(b *ssa.Block, v *ssa.Value) bool {
	for i := len(b.Values) - 1; i >= 0; i-- {
		w := b.Values[i]
		if w == v {
			return true
		}
		if !c.lazy[w.ID] || w.Op == ssa.OpPhi {
			return false
		}
	}
	return false
}

// emptyEdge reports whether the edge from from to to does nothing -- no
// phi moves, as phiSchedule finds them, and no poll -- so that a branch
// can jump to to itself.
// flushIn is the loop counting its budget in a register whose register
// the start of b stores back (budgetFlush), or -1: of a block outside
// every such loop whose one predecessor is in one -- a loop's way out
// to a block of its own. Stored there, not on the edge, the edge moves
// nothing and the loop's test jumps straight to the block: the edge's
// code, out of line (leaves), had cost a loop entered for few turns a
// jump there and one back each time it left.
func (c *core) flushIn(b *ssa.Block) int {
	if len(b.Preds) != 1 || c.loopBudget(b) >= 0 {
		return -1
	}
	if f := c.loopBudget(b.Preds[0]); f >= 0 && c.budgetReg(f) >= 0 {
		return f
	}
	return -1
}

func (c *core) emptyEdge(from, to *ssa.Block) bool {
	idx := -1
	for i, p := range to.Preds {
		if p == from {
			idx = i
		}
	}
	if idx < 0 || to.LoopHeader && to.Backedge[idx] {
		return false
	}
	if f, t := c.budgetReg(c.loopBudget(from)), c.budgetReg(c.loopBudget(to)); (f >= 0 || t >= 0) && c.loopBudget(from) != c.loopBudget(to) && (t >= 0 || c.flushIn(to) != c.loopBudget(from)) {
		// Into or out of a loop that counts its budget in a register: the
		// register loaded, or stored back (edge).
		return false
	}
	for _, phi := range to.Values {
		if phi.Op != ssa.OpPhi {
			break
		}
		if _, ok := c.loc(phi); !ok {
			continue // dead
		}
		if src := phi.Args[idx]; c.isLazy(src) || c.locAt(src) != c.locAt(phi) {
			return false
		}
	}
	return true
}

// leaves reports whether the edge from b to to leaves a loop that the
// edge from b to other stays in: an If's way out of a loop, which branch
// lays out with the cold code, so that the loop's way falls through from
// the test rather than jumping past the edge's code on every turn -- an
// edge that had moved nothing, until it stored a loop's budget register
// back (edge), had put a taken branch in microbench's prop_read's turn.
// The way out runs once a loop's entry, through a jump there and one
// back.
func (c *core) leaves(b, to, other *ssa.Block) bool {
	m := c.loopMasks()
	return m[b.ID]&^m[to.ID]&m[other.ID] != 0
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
func (c *core) allocate(groups int) error {
	c.used, c.usedList, c.saves, c.saveAt, c.dropped = [2]uint64{}, [2][]int{}, c.saves[:0], nil, nil
	nv, nb := c.f.NumValues()+len(c.budgets), numBlocks(c.f)
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
	// useState uses what state s reads at position at. moved, if not nil,
	// is the loop header whose state a back edge's poll reads after the
	// edge's phi moves (edge): the header's phis it names are then in their
	// locations, written by the moves, not read at the edge's end, so they
	// are not used there. Used there, each phi had lived to the end of the
	// loop's last block, beside the argument the edge moves into it: in
	// AWFY List's isShorterThan, four phis and the three values the body
	// made took the seven registers, and the eighth, a property's cell
	// address, went to a spill slot, stored and loaded back each time round.
	useState := func(s *ssa.FrameState, at int, moved *ssa.Block) {
		if s == nil {
			return
		}
		add := func(x *ssa.Value) {
			if moved == nil || x.Op != ssa.OpPhi || x.Block != moved {
				uses = append(uses, use{x, at})
			}
		}
		for _, v := range s.Slots {
			if v == nil {
				continue
			}
			if x := bigKept(v); x != nil {
				// Its exits make it of the BigInt it boxes (exitBig).
				add(x)
				if c.isLazy(v) {
					continue
				}
			}
			if c.isLazy(v) || remat(v) {
				for _, a := range v.Args {
					add(a)
				}
				continue
			}
			add(v)
			if v.Shadow != nil {
				add(v.Shadow)
			}
		}
	}
	for _, b := range c.order {
		for i, v := range b.Values {
			if v.Op == ssa.OpPhi {
				continue // phi arguments are used at their predecessors' ends
			}
			// A fused value is computed at its block's branch (findFused):
			// what it reads is used there.
			p, fused := pos[v.ID], c.isFused(v)
			if fused {
				p = end[b.ID]
			}
			for _, a := range v.Args {
				uses = append(uses, use{a, p})
				if a.Shadow != nil {
					// It may read where its operand came from: a keep
					// (mir's keep), or an operation that finds an object's
					// pointer word there (sourceRef). A value the frame
					// state names keeps its shadow alive there too, but a
					// captured local's is in no state (its slot is its
					// cell), and the slot IR reads a local as an operand.
					uses = append(uses, use{a.Shadow, p})
				}
			}
			at := p
			if v.Op == ssa.OpCall && len(v.Calls()) != 0 && (v.Calls()[0].Go != 0 || ssa.RefillsPools(v)) {
				// A call of Go that Go refuses exits after it, Go having
				// taken the registers: what its state reads lives across
				// it, saved and restored (saves). So does a call whose
				// pool Go fills, made again from its start: its operands
				// are in its state.
				at++
			}
			if !fused && i+1 < len(b.Values) && b.Values[i+1].State == v.State {
				// The guards of one instruction share its state: what it
				// reads is used at the last of them, which subsumes the
				// others, as they are in one block.
				continue
			}
			useState(v.State, at, nil)
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
		useState(b.State, end[b.ID], nil)
		if b.LoopHeader {
			useState(b.Header, start[b.ID], nil)
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
					// A poll on this edge exits to the header's state,
					// after the moves.
					useState(s.Header, end[b.ID], s)
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
	// Each budget loop's register, from its header's start to the end of
	// the last of its blocks laid out.
	for _, bu := range c.budgets {
		it := &iv[bu.v.ID]
		*it = interval{v: bu.v, start: start[bu.header.ID], end: start[bu.header.ID]}
		for _, b := range c.order {
			if bu.in[b.ID] {
				it.start, it.end = min(it.start, start[b.ID]), max(it.end, end[b.ID])
			}
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
	// A native call's result nothing uses -- no value, state or control,
	// nor its keep cell's address (OpCallCell) -- is stored nowhere
	// (dropped).
	for _, b := range c.order {
		for _, v := range b.Values {
			if v.Op == ssa.OpCall && v.Calls() != nil && iv[v.ID].v != nil && iv[v.ID].end == pos[v.ID] {
				if c.dropped == nil {
					c.dropped = c.bools(nv)
				}
				c.dropped[v.ID] = true
			}
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
	// Beyond those hints, a phi and its arguments are coalesced into one
	// group where none of their intervals meet, so that a value carried
	// round a loop through several phis -- a loop whose body has an edge
	// of its own into the header, as a try statement's has -- keeps one
	// register through all of them: given each its neighbour's, a phi in
	// a block laid out before the header took the register of a header
	// phi of another variable, and the loop-carried value went through a
	// chain of moves, one an edge (try_catch's sum: three). group is a
	// union-find over value IDs, 1 plus the parent or 0 for a root; link
	// links each group's members, from its root, 1 plus the next or 0;
	// left counts, by root, its members not yet given a location; reg is,
	// by root, 1 plus the register its first member was given.
	group, link, left, greg := c.ints(nv), c.ints(nv), c.ints(nv), c.ints(nv)
	find := func(id int) int {
		for group[id] != 0 {
			id = group[id] - 1
		}
		return id
	}
	meets := func(a, b *interval) bool { return a.start <= b.end && b.start <= a.end }
	// unite joins the groups of x and y if no member of one meets a member
	// of the other: a bounded check, as a large function's phis can make
	// large groups.
	unite := func(x, y *ssa.Value) {
		if x == y || isFloat(x) != isFloat(y) || iv[x.ID].v == nil || iv[y.ID].v == nil {
			return
		}
		rx, ry := find(x.ID), find(y.ID)
		if rx == ry {
			return
		}
		work := 0
		for a := rx; ; {
			for b := ry; ; {
				if work++; work > 4096 || meets(&iv[a], &iv[b]) {
					return
				}
				if link[b] == 0 {
					break
				}
				b = link[b] - 1
			}
			if link[a] == 0 {
				break
			}
			a = link[a] - 1
		}
		// ry's members after rx's.
		last := rx
		for link[last] != 0 {
			last = link[last] - 1
		}
		link[last] = ry + 1
		group[ry] = rx + 1
	}
	for _, b := range c.order {
		for _, v := range b.Values {
			if v.Op != ssa.OpPhi {
				break
			}
			for _, a := range v.Args {
				if groups != groupsNone {
					unite(v, a)
				}
			}
		}
	}
	// A group's register is waited for while members of it are still to
	// come; wait is, by class and register, a list of the groups that
	// wait for it, linked through wnext (1 plus the next root, 0 at the
	// end; heads 1 plus a root), those done unlinked as they are met. A
	// register is spoken for, for an interval of start s and end e, where
	// a group waits for it a member of which, still to come, begins in
	// s..e: one that would find it taken. Then the first member of a group
	// does not take it for the group (two groups waiting for one register,
	// the second to come had found it taken and gone through moves at
	// every edge: array_for_of's iterator, round its loop, two), and the
	// hints of a value of no group, or of another, pass over it. Spoken
	// for by any group waiting for it at all, or by any whose members span
	// the interval, Mandreel's and zlib's largest functions, of many
	// groups and many entries, had lost the hints along their loops'
	// chains of joins by the dozen a turn. starts are, by group, its
	// members' starts in order, from mfrom[root].
	hs, he, wnext := c.ints(nv), c.ints(nv), c.ints(nv)
	mfrom, starts := c.ints(nv+1), c.ints(nv)
	for i := range iv {
		if iv[i].v != nil {
			r := find(i)
			if left[r] == 0 || iv[i].start < hs[r] {
				hs[r] = iv[i].start
			}
			he[r] = max(he[r], iv[i].end)
			left[r]++
		}
	}
	for r := range left {
		mfrom[r+1] = mfrom[r] + left[r]
	}
	fill := c.ints(nv)
	for _, it := range all {
		// all is in order of start.
		r := find(it.v.ID)
		starts[mfrom[r]+fill[r]] = it.start
		fill[r]++
	}
	var wait [2][64]int
	spoken := func(k, reg, s, e, self int) bool {
		prev := -1
		for w := wait[k][reg]; w != 0; {
			r := w - 1
			nx := wnext[r]
			if left[r] == 0 {
				if prev < 0 {
					wait[k][reg] = nx
				} else {
					wnext[prev] = nx
				}
				w = nx
				continue
			}
			if r != self && groups == groupsStrict {
				return true
			}
			if r != self && he[r] >= s && hs[r] <= e {
				ms := starts[mfrom[r]:mfrom[r+1]]
				if i, _ := slices.BinarySearch(ms, s); i < len(ms) && ms[i] <= e {
					return true
				}
			}
			prev, w = r, nx
		}
		return false
	}
	placed := func(v *ssa.Value) {
		r, k := find(v.ID), b2i(isFloat(v))
		if l := c.locAt(v); l.reg >= 0 && greg[r] == 0 && left[r] > 1 && !spoken(k, l.reg, hs[r], he[r], r) {
			greg[r] = l.reg + 1
			wnext[r], wait[k][l.reg] = wait[k][l.reg], r+1
		}
		left[r]--
	}
	// hinted is the register free holds that v is to share, or -1: its
	// group's, else a phi's or an argument's (above), not spoken for.
	hinted := func(v *ssa.Value, free []int) int {
		r, k := find(v.ID), b2i(isFloat(v))
		if g := greg[r]; g != 0 {
			if i := slices.Index(free, g-1); i >= 0 {
				return i
			}
		}
		it := &iv[v.ID]
		want := func(x *ssa.Value) int {
			if x == nil || !c.hasLoc[x.ID] {
				return -1
			}
			if l := c.locAt(x); l.reg >= 0 && !spoken(k, l.reg, it.start, it.end, r) {
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
		// minEnd is at most the soonest an active interval ends: none
		// ends before an interval that starts no later, and the active
		// are looked at only once one may.
		minEnd := math.MaxInt
		for _, it := range classes[class] {
			if minEnd < it.start {
				kept := active[:0]
				minEnd = math.MaxInt
				for _, a := range active {
					if a.end < it.start {
						free = append(free, c.locAt(a.v).reg)
					} else {
						kept = append(kept, a)
						minEnd = min(minEnd, a.end)
					}
				}
				active = kept
			}
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
					// The last freed, but one spoken for.
					i = len(free) - 1
					for j := len(free) - 1; j >= 0; j-- {
						if !spoken(b2i(float), free[j], it.start, it.end, find(it.v.ID)) {
							i = j
							break
						}
					}
				}
				c.setLoc(it.v, loc{reg: free[i], spill: -1})
				placed(it.v)
				free = append(free[:i], free[i+1:]...)
				active = append(active, it)
				minEnd = min(minEnd, it.end)
				continue
			}
			// Spill whichever interval ends last -- but a budget loop's
			// register first, whose loop then counts in memory: the
			// register costs no value of the loop a spill.
			victim := it
			vi := -1
			for i, a := range active {
				if a.end > victim.end {
					victim, vi = a, i
				}
			}
			if it.v.ID >= c.f.NumValues() {
				victim, vi = it, -1
			}
			for i, a := range active {
				if a.v.ID >= c.f.NumValues() {
					victim, vi = a, i
				}
			}
			budget := victim.v.ID >= c.f.NumValues()
			s := -1
			if !budget {
				// A budget's register given up takes no spill slot, which
				// another value's phi or argument may be hinted to.
				var err error
				if s, err = slot(victim.start, victim.v); err != nil {
					return err
				}
			}
			if victim == it {
				c.setLoc(it.v, loc{reg: -1, spill: s})
			} else {
				c.setLoc(it.v, loc{reg: c.locAt(victim.v).reg, spill: -1})
				c.setLoc(victim.v, loc{reg: -1, spill: s})
				active[vi] = it
				minEnd = min(minEnd, it.end)
			}
			placed(it.v)
			if !budget {
				spilled = append(spilled, victim)
				push(len(spilled) - 1)
			}
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
				if l.reg < 0 || it.v.ID >= c.f.NumValues() {
					// Not in a register; or a budget loop's, whose loop
					// calls nothing: the call is in a block laid out
					// among its blocks.
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
				sv := saved{reg: l.reg, slot: s, float: isFloat(it.v), id: it.v.ID}
				if o, ok := c.frameResident(it.v, v); ok {
					sv.frame = o + 1
				}
				c.saves = c.appendSaved(c.saves, sv)
			}
			c.saveAt[2*v.ID+1] = len(c.saves)
		}
	}
	c.iv, c.pos = iv, pos
	return nil
}

// allocateBest allocates the function twice, with the hints alone and
// with the phis' groups too (allocate), and keeps whichever leaves the
// fewer moves on its edges, weighed by how deep in loops each edge is
// (edgeCost), the hints' on a tie: the groups carry a value round a loop
// in one register through its chains of phis, but where many groups vie
// for few registers they could break the chains the hints made, as in
// microbench's array_write (6 moves on its edges with the hints, 27 with
// the groups) where the groups saved Mandreel's sortMinDown a third of its.
// Either is a linear scan's allocation, spilling the same values: only
// which free register each value takes differs. Then the keep cells are
// given out (colorKeeps), by the intervals the one kept found.
func (c *core) allocateBest() error {
	if err := c.allocate(groupsNone); err != nil {
		return err
	}
	if c.hasPhiArgs() {
		best, cost := groupsNone, c.edgeCost()
		for _, g := range [...]int{groupsStrict, groupsLoose} {
			if err := c.allocate(g); err != nil {
				return err
			}
			if k := c.edgeCost(); k < cost {
				best, cost = g, k
			}
		}
		if best != groupsLoose {
			if err := c.allocate(best); err != nil {
				return err
			}
		}
	}
	return c.colorKeeps(c.iv, c.pos)
}

// The allocations allocate makes: with the hints alone; with the phis'
// groups too, a register spoken for while any other group waits for it
// (strict) or only where a member of another, still to come, begins in an
// interval that would take it (loose). Strict keeps small functions'
// chains, which loose lets other groups break (array_write); loose,
// large functions', where strict leaves few registers unspoken for, and
// the hints with them (Mandreel's sortMinDown).
const (
	groupsNone = iota
	groupsStrict
	groupsLoose
)

// hasPhiArgs reports whether the function has a phi, which groups need.
func (c *core) hasPhiArgs() bool {
	for _, b := range c.f.Blocks {
		if len(b.Values) > 0 && b.Values[0].Op == ssa.OpPhi {
			return true
		}
	}
	return false
}

// edgeCost is the moves the edges' phis make (a phi whose argument is not
// where it is), each weighed 16 to the power of how many loops (at most
// three) hold both the edge's ends: a move on an inner loop's edge runs
// many times one before it.
func (c *core) edgeCost() int {
	masks := c.loopMasks()
	cost := 0
	for _, b := range c.f.Blocks {
		for i, p := range b.Preds {
			w := 1
			for d := min(bits.OnesCount64(masks[b.ID]&masks[p.ID]), 3); d > 0; d-- {
				w *= 16
			}
			for _, phi := range b.Values {
				if phi.Op != ssa.OpPhi {
					break
				}
				l, ok := c.loc(phi)
				if !ok {
					continue
				}
				if a := phi.Args[i]; c.isLazy(a) || c.locAt(a) != l {
					cost += w
				}
			}
		}
	}
	return cost
}

// loopMasks are, by block ID, the loops each block is in, a bit for each
// of the function's first 64 loop headers: a loop's blocks those its
// header reaches that reach one of its back edges without passing it.
// Made once for a compile.
func (c *core) loopMasks() []uint64 {
	if c.masks != nil {
		return c.masks
	}
	f := c.f
	nb := numBlocks(f)
	masks := c.words(nb)
	in, fwd := c.bools(nb), c.bools(nb)
	var stack []*ssa.Block
	k := 0
	for _, h := range f.Blocks {
		if !h.LoopHeader || k == 64 {
			continue
		}
		clear(in)
		clear(fwd)
		in[h.ID] = true
		stack = stack[:0]
		for i, p := range h.Preds {
			if h.Backedge[i] && !in[p.ID] {
				in[p.ID] = true
				stack = append(stack, p)
			}
		}
		for len(stack) > 0 {
			b := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			for _, p := range b.Preds {
				if !in[p.ID] {
					in[p.ID] = true
					stack = append(stack, p)
				}
			}
		}
		fwd[h.ID] = true
		stack = append(stack[:0], h)
		for len(stack) > 0 {
			b := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			for _, s := range b.Succs {
				if in[s.ID] && !fwd[s.ID] {
					fwd[s.ID] = true
					stack = append(stack, s)
				}
			}
		}
		for id, x := range fwd {
			if x {
				masks[id] |= 1 << k
			}
		}
		k++
	}
	c.masks = masks
	return masks
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
		return v.Op == ssa.OpCall && len(v.Calls()) != 0 || v.Op == ssa.OpKeep || v.Op == ssa.OpBigBox
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
			case v.Op == ssa.OpCallCell, v.Op == ssa.OpBigCell:
				if w := v.Args[0]; writes(w) {
					cell[v.ID] = w.Index + 1
				}
			case v.Op == ssa.OpPhi && v.Type == ssa.Source:
				phis = append(phis, v)
			}
		}
	}
	// The cells each phi of addresses may be the address of: its
	// arguments', through phis of phis, until none grows. phiAt is, by
	// value ID, 1 plus a phi's index in phis, or 0; merged, by that index,
	// its cells.
	var phiAt []int
	var merged [][]int
	if len(phis) != 0 {
		phiAt, merged = c.ints(f.NumValues()), make([][]int, len(phis))
		for i, p := range phis {
			phiAt[p.ID] = i + 1
		}
		for grew := true; grew; {
			grew = false
			for i, p := range phis {
				for _, a := range p.Args {
					add := func(k int) {
						if !slices.Contains(merged[i], k) {
							merged[i] = append(merged[i], k)
							grew = true
						}
					}
					if k := cell[a.ID]; k != 0 {
						add(k - 1)
					}
					if j := phiAt[a.ID]; j != 0 {
						for _, k := range merged[j-1] {
							add(k)
						}
					}
				}
			}
		}
	}
	cellsOf := func(x *ssa.Value, each func(int)) {
		if x == nil || x.ID >= len(cell) {
			// None, or a budget loop's register (findBudgets).
			return
		}
		if k := cell[x.ID]; k != 0 {
			each(k - 1)
		}
		if phiAt != nil {
			if j := phiAt[x.ID]; j != 0 {
				for _, k := range merged[j-1] {
					each(k)
				}
			}
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
	// A BigInt's box made only at its exits (findLazy) is written to its
	// cell there, by an exit's code (bigRebox), wherever a state names its
	// value: where the BigInt it boxes is live.
	var ivOf []int
	for _, b := range c.order {
		for _, v := range b.Values {
			if v.Op != ssa.OpBigBox || !c.isLazy(v) {
				continue
			}
			if ivOf == nil {
				ivOf = c.ints(f.NumValues())
				for i := range iv {
					if iv[i].v != nil && iv[i].v.ID < len(ivOf) {
						ivOf[iv[i].v.ID] = i + 1
					}
				}
			}
			if j := ivOf[v.Args[0].ID]; j != 0 {
				extend(v.Index, iv[j-1].start, iv[j-1].end)
			}
		}
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
// context's: a call's result's or a kept value's keep cell -- and never a
// slot or -1, so that the value's pointer word is read there with no
// decoding (sourceAddr) and no test.
func cellSource(s *ssa.Value) bool {
	switch s.Op {
	case ssa.OpPropCell, ssa.OpElemCell, ssa.OpGlobalCell, ssa.OpLexCell, ssa.OpBindingCell, ssa.OpStringMethod, ssa.OpStringChar, ssa.OpCodeChar, ssa.OpSubstringChar, ssa.OpStringProp, ssa.OpCallCell, ssa.OpBigCell, ssa.OpConstCell, ssa.OpUpvalueCell, ssa.OpLocalCell, ssa.OpKeep:
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

// frameSize is how many slots the code's frame takes in the VM's stack, the
// call entry's: its locals and operands, or more, to the end of its loops'
// slots (ssa.Func's LoopsEnd), past which the frames of the callees its
// loops call start.
func (c *core) frameSize() int {
	n := c.f.FrameLocals + c.f.StackSize
	if c.f.Loops {
		n = max(n, c.f.FrameLocals+c.f.LoopsEnd-c.f.Locals)
	}
	return n
}

// remat reports a value an exit recomputes from its operands rather than
// reads from its location. A poll exits on a back-edge, after the phi moves
// and before the header runs again, so a box the header computed of a phi
// would still hold the last iteration's value; its operand, the phi, holds
// this one's.
func remat(v *ssa.Value) bool {
	return v.Op == ssa.OpBoxF64 || v.Op == ssa.OpBoxBool || v.Op == ssa.OpConst
}

// held reports whether x, a box an exit makes again of its operand (remat),
// is in its own location at v, a call it is an operand of: live there, so
// that the call reads its word where it is, rather than boxing it again --
// four instructions for a number, each time a call writes it as an
// argument and again as a record.
//
// Not at a call a pool's refill may make again from its start
// (ssa.RefillsPools): Go fills the pool first, keeping the registers only
// of what lives past the call, which its state's values do, a box's
// operand among them, but not a box only the call reads.
func (c *core) held(x, v *ssa.Value) bool {
	if x.Op != ssa.OpBoxF64 && x.Op != ssa.OpBoxBool || c.isLazy(x) || ssa.RefillsPools(v) {
		return false
	}
	if _, ok := c.loc(x); !ok {
		return false
	}
	return slices.Contains(v.Args, x)
}

// scalar reports a value with no pointer word to read (pointerWord's 0):
// no shadow, and no slot its origin is.
func (c *core) scalar(x *ssa.Value) bool {
	o, static := c.origin.Of(x)
	return x.Shadow == nil && !(static && o >= 0)
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
	f := c.exitLayout()
	d := abi.ExitDescriptor{Kind: uint32(kind), PC: uint32(s.PC), Depth: uint32(s.Depth), Site: int32(s.Site),
		Frame: f, Start: uint32(len(f.Index)), LocalsReg: localsReg, StackReg: stackReg}
	written := func(i int, v *ssa.Value) bool {
		return v != nil && (v.Op != ssa.OpLoadSlot || v.Aux != i) && !c.captured(i)
	}
	// What it adds to the frame's, taken back where a slot cannot be
	// described.
	index, slots, words := len(f.Index), len(f.Slots), len(f.Words)
	undo := func() *abi.ExitDescriptor {
		// Each entry it added unchained from its slot's chain, wherever
		// it has come to be in it.
		for i := len(f.Slots) - 1; i >= slots; i-- {
			at, k := &c.exitHead[f.Slots[i].Slot], int32(i+1)
			for *at != k {
				at = &c.exitNext[*at-1]
			}
			*at = c.exitNext[i]
		}
		c.exitNext = c.exitNext[:slots]
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
		if x := bigKept(v); x != nil {
			// A BigInt's box, made of the BigInt unboxed, which a state
			// at a loop's header names before the box is made there.
			if e.Value, ok = c.exitBig(x); !ok {
				return undo()
			}
		} else if e.Value, ok = c.exitValue(v); !ok {
			return undo()
		}
		if e.Value.Kind == abi.ExitConst {
			e.Value.N = c.exitWord(c.constWord(v.Const))
		}
		if big, _ := e.Value.IsBig(); big {
			// Its pointer word is made of it too.
		} else if v.Shadow != nil {
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
		for int(e.Slot) >= len(c.exitHead) {
			c.exitHead = append(c.exitHead, 0)
		}
		// Most recently found first: the next exit's slot is most often
		// where the last one's was.
		k, prev := c.exitHead[e.Slot], int32(0)
		for k != 0 && f.Slots[k-1] != e {
			k, prev = c.exitNext[k-1], k
		}
		if k != 0 && prev != 0 {
			c.exitNext[prev-1] = c.exitNext[k-1]
			c.exitNext[k-1] = c.exitHead[e.Slot]
			c.exitHead[e.Slot] = k
		}
		if k == 0 {
			if len(f.Slots) > math.MaxUint16 {
				// More than an index names: the exit's code writes it.
				return undo()
			}
			f.Slots = append(f.Slots, e)
			c.exitNext = append(c.exitNext, c.exitHead[e.Slot])
			k = int32(len(f.Slots))
			c.exitHead[e.Slot] = k
		}
		f.Index = append(f.Index, uint16(k-1))
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
			d.Inline = append(d.Inline, abi.ExitInline{Closure: uint64(in.Closure), Callee: uint64(in.Callee), Locals: uint64(in.Locals), Args: in.Args,
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

// exitLayout is what the code's descriptions share (abi.ExitFrame), made
// at the first.
func (c *core) exitLayout() *abi.ExitFrame {
	if c.exitFrame == nil {
		enc := c.enc
		c.exitFrame = &abi.ExitFrame{FrameLocals: int32(c.f.FrameLocals), ThisSlot: int32(c.f.ThisSlot), Locals: int32(c.f.Locals), Enc: &enc}
		if w := c.ws; w != nil {
			// The workspace's, kept from compile to compile.
			if w.exitWords == nil {
				w.exitWords = map[uint64]int32{}
			}
			clear(w.exitWords)
			c.exitWords = w.exitWords
			c.exitHead, c.exitNext = w.exitHeadBuf[:0], w.exitNextBuf[:0]
			c.exitFrame.Slots, c.exitFrame.Index, c.exitFrame.Words = w.exitSlotsBuf[:0], w.exitIndexBuf[:0], w.exitWordsBuf[:0]
		} else {
			c.exitWords = map[uint64]int32{}
		}
	}
	return c.exitFrame
}

// exitWord is the index of word w among the descriptions' constants
// (abi.ExitFrame's Words), added if it is not one.
func (c *core) exitWord(w uint64) int32 {
	f := c.exitLayout()
	k, ok := c.exitWords[w]
	if !ok {
		k = int32(len(f.Words))
		f.Words = append(f.Words, w)
		c.exitWords[w] = k
	}
	return k
}

// callRecord is how call v records a slot of its state (callState): the
// slot, its value x, and whether the call writes its number word (num) and
// its pointer word (ref) -- what a description cannot find where the
// caller's code keeps it.
type callRecord struct {
	slot     int
	x        *ssa.Value
	num, ref bool
	// big marks a BigInt's box, whose words the call makes of the BigInt
	// (bigKept), the box made or not.
	big bool
}

// callState describes native call v's state s, every slot the frame does
// not hold as it is but those skip says (abi.CallState), as V8's
// deoptimization data does a call's: Go expands it into the records it
// makes the frame from if the callee leaves native code (jit.ExpandCall).
// A value is where the caller keeps it: a constant; a spill slot -- one
// the allocator spilled, or one a register live past the call is saved to
// (savesOf); for its pointer word, a slot of the frame that held it on
// entry, which nothing writes while the call runs. The call writes the
// rest, records' returns says which: a register's word dead past the
// call; a pointer word from a cell, or a slot a closure may assign; and
// every word of the slots from operands on, if not -1, which the call
// reads its callee's arguments from. Those whose pointer words it writes
// come first.
func (c *core) callState(v *ssa.Value, s *ssa.FrameState, skip func(int) bool, operands int) (*abi.CallState, []callRecord) {
	saves := c.savesOf(v)
	st := &abi.CallState{PC: uint32(s.PC), Depth: uint32(s.Depth), Site: int32(s.Site), Frame: c.exitLayout()}
	var refs, rest []callRecord
	var refRecs, restRecs []abi.CallRecord
	for i, x := range s.Slots {
		if x == nil || x.Op == ssa.OpLoadSlot && x.Aux == i || c.captured(i) || skip(i) {
			continue
		}
		rec, r := abi.CallRecord{Slot: int32(i), Origin: -1}, callRecord{slot: i, x: x}
		full := operands >= 0 && i >= operands
		if bigKept(x) != nil {
			r.num, r.ref, r.big, rec.Ref = true, true, true, true
			refs, refRecs = append(refs, r), append(refRecs, rec)
			continue
		}
		if l, ok := c.callWord(x, saves); ok && !full {
			rec.Num = l
		} else {
			r.num = true
		}
		o, static := c.origin.Of(x)
		switch {
		case full || x.Shadow != nil:
			r.ref = true
		case static && o >= 0 && c.stableSlot(o):
			rec.Origin, rec.Load = int32(o), x.Op == ssa.OpLoadSlot
		case static && o >= 0:
			r.ref = true
		}
		rec.Ref = r.ref
		if r.ref {
			refs, refRecs = append(refs, r), append(refRecs, rec)
		} else {
			rest, restRecs = append(rest, r), append(restRecs, rec)
		}
	}
	st.Records = append(refRecs, restRecs...)
	c.calls = append(c.calls, st)
	return st, append(refs, rest...)
}

// goState describes a call of Go, op, with Go's result to keep cell keep
// (abi.GoCallState) -- for a call (ssa.GoCall), the PC and depth of v's
// state -- and its operands args, each where the code keeps it as the call
// starts, as an exit's description does (exitDescriptor), the registers
// the call's routine saves (goSaving) among them. It reports which
// operands the call writes itself: those a description cannot find.
func (c *core) goState(op uint64, keep int, v *ssa.Value, args []*ssa.Value) (*abi.GoCallState, []bool) {
	enc := c.enc
	st := &abi.GoCallState{Op: op, Keep: uint64(keep), NumOffset: enc.NumOffset, RefOffset: enc.RefOffset,
		UpvalueSlot: enc.UpvalueSlot, True: enc.True, False: enc.False, NaN: enc.CanonicalNaN}
	if v != nil && ssa.GoOp(op&0xff) == ssa.GoCall {
		st.SetsPC, st.PC, st.Depth = true, uint32(v.State.PC), uint32(v.State.Depth)
		if in := v.State.Inline; in != nil {
			// Inside an inlined callee: its instruction, which Go makes
			// without a frame or not at all.
			st.Inline, st.InlinePC = in.Closure, in.PC
		}
	}
	c.goCalls = append(c.goCalls, st)
	if len(args) == 0 {
		return st, nil
	}
	written := make([]bool, len(args))
	st.Args = make([]abi.GoCallArg, len(args))
	for i, x := range args {
		a, ok := c.goArgOf(x)
		if !ok {
			written[i], a = true, abi.GoCallArg{}
		}
		st.Args[i] = a
	}
	return st, written
}

// goArgOf is where Go finds operand x of a call of Go (abi.GoCallArg), as
// an exit's description finds a slot's value (exitValue, exitSource), as
// offsets: or false, where it cannot say.
func (c *core) goArgOf(x *ssa.Value) (abi.GoCallArg, bool) {
	var a abi.GoCallArg
	l, ok := c.exitValue(x)
	if !ok {
		return a, false
	}
	switch l.Kind {
	case abi.ExitConst:
		a.Kind, a.Word = abi.GoArgConst, c.constWord(x.Const)
	case abi.ExitReg:
		a.Kind, a.At = abi.GoArgWord, abi.OffRegs+l.N*8
	case abi.ExitSpill:
		a.Kind, a.At = abi.GoArgWord, abi.OffSpill+l.N*8
	case abi.ExitF64Reg:
		a.Kind, a.At = abi.GoArgF64, abi.OffXRegs+(l.N&31)*8
	case abi.ExitF64Spill:
		a.Kind, a.At = abi.GoArgF64, abi.OffSpill+l.N*8
	case abi.ExitBoolReg:
		a.Kind, a.At = abi.GoArgBool, abi.OffRegs+(l.N&31)*8
	case abi.ExitBoolSpill:
		a.Kind, a.At = abi.GoArgBool, abi.OffSpill+l.N*8
	default:
		return a, false
	}
	a.Number = x.Shadow != nil || c.origin.At(x) >= 0
	o, static := c.origin.Of(x)
	switch {
	case x.Shadow != nil:
		src, ok := c.exitSource(x.Shadow)
		switch {
		case !ok:
			return a, false
		case src.Kind == abi.ExitReg:
			a.Ref, a.From = abi.GoRefSource, abi.OffRegs+src.N*8
		case src.Kind == abi.ExitSpill:
			a.Ref, a.From = abi.GoRefSource, abi.OffSpill+src.N*8
		case src.Kind == abi.ExitSlotAddr:
			a.Ref = abi.GoRefSlotSource
			a.Base, a.From = c.slotPlace(int(src.N))
		case src.Kind == abi.ExitConst:
			// No source: no pointer word.
		default:
			return a, false
		}
	case static && o >= 0:
		a.Ref, a.Load = abi.GoRefOrigin, x.Op == ssa.OpLoadSlot
		a.Base, a.From = c.slotPlace(o)
	}
	return a, true
}

// slotPlace is where slot i is, as abi.GoCallArg's Base and From say it.
func (c *core) slotPlace(i int) (uint8, int32) {
	vs := c.enc.ValueSize
	switch {
	case i < c.f.FrameLocals:
		return abi.ExitInLocals, int32(i) * vs
	case i == c.f.ThisSlot:
		return abi.ExitInThis, 0
	case i < c.f.Locals:
		return abi.ExitInCell, int32(i - c.f.FrameLocals)
	}
	return abi.ExitInStack, int32(i-c.f.Locals) * vs
}

// callRefs bounds the pointer words the code's calls write themselves
// (Code's CallRefs).
func (c *core) callRefs() int {
	n := 0
	for _, st := range c.calls {
		k := 0
		for k < len(st.Records) && st.Records[k].Ref {
			k++
		}
		n = max(n, k)
	}
	return n
}

// callWord is where a call's description finds the number word of x, a
// value of its state, after the call (callState): a constant, or a spill
// slot, if it is in one.
func (c *core) callWord(x *ssa.Value, saves []saved) (abi.ExitLoc, bool) {
	if x.Op == ssa.OpBoxF64 || x.Op == ssa.OpBoxBool {
		// A box live past the call, which the call saves: its word is in
		// its save slot, which the call need not write as a record. The
		// save is found by the value, not by its register: a state uses
		// a box's operand, not the box (allocate's useState), so a box the
		// call does not read may have ended before it, and its register
		// be another's that the call saves: a loop's counter, boxed in its
		// header, whose register a keep cell's address took before a
		// construction, had Go make the frame with that address for the
		// counter when the constructor threw, and the loop ran on.
		for _, sv := range saves {
			if sv.id == x.ID && !sv.float && sv.frame == 0 {
				return abi.ExitLoc{Kind: abi.ExitSpill, N: int32(sv.slot)}, true
			}
		}
	}
	l, ok := c.exitValue(x)
	if !ok {
		return l, false
	}
	switch l.Kind {
	case abi.ExitConst:
		l.N = c.exitWord(c.constWord(x.Const))
		return l, true
	case abi.ExitSpill, abi.ExitF64Spill, abi.ExitBoolSpill:
		return l, true
	}
	float, kind := l.Kind == abi.ExitF64Reg, abi.ExitSpill
	switch l.Kind {
	case abi.ExitF64Reg:
		kind = abi.ExitF64Spill
	case abi.ExitBoolReg:
		kind = abi.ExitBoolSpill
	}
	// The value whose register exitValue found: x, or the operand a box is
	// made of.
	in := x
	if remat(x) {
		in = x.Args[0]
	}
	for _, sv := range saves {
		if sv.id == in.ID && sv.reg == int(l.N) && sv.float == float && sv.frame == 0 {
			return abi.ExitLoc{Kind: kind, N: int32(sv.slot)}, true
		}
	}
	return l, false
}

// stableSlot reports whether nothing writes slot o while a native call
// runs: an operand's, the receiver, or a local no closure assigns -- the
// frame's slots are written only at its exits.
func (c *core) stableSlot(o int) bool {
	return o >= c.f.Locals || o == c.f.ThisSlot || o >= 0 && o < c.f.FrameLocals && !c.f.Assigned(o)
}

// finishExits gives the code's descriptions' frame (abi.ExitFrame) arrays
// of their own, as long as what they hold: grown by doubling, they could
// hold twice that, for as long as the code lives.
func (c *core) finishExits() {
	if f := c.exitFrame; f != nil {
		if w := c.ws; w != nil {
			// Its buffers, for the next compile.
			w.exitSlotsBuf, w.exitIndexBuf, w.exitWordsBuf = f.Slots[:0], f.Index[:0], f.Words[:0]
			w.exitHeadBuf, w.exitNextBuf = c.exitHead[:0], c.exitNext[:0]
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

// bigKept is the unboxed BigInt v, a tagged value, is the box of (ssa's
// OpBigBox, kept in its cell), or nil.
func bigKept(v *ssa.Value) *ssa.Value {
	if v.Op == ssa.OpKept && v.Args[1].Op == ssa.OpBigBox {
		return v.Args[1].Args[0]
	}
	return nil
}

// exitBig is where an exit finds x, an unboxed BigInt, its value's words
// made of it (abi.ExitLoc's IsBig).
func (c *core) exitBig(x *ssa.Value) (abi.ExitLoc, bool) {
	l, ok := c.loc(x)
	if !ok || c.isLazy(x) {
		return abi.ExitLoc{}, false
	}
	reg, spill := abi.ExitI64Reg, abi.ExitI64Spill
	if x.Type == ssa.Uint64 {
		reg, spill = abi.ExitU64Reg, abi.ExitU64Spill
	}
	if l.reg >= 0 {
		return abi.ExitLoc{Kind: reg, N: int32(l.reg)}, true
	}
	return abi.ExitLoc{Kind: spill, N: int32(l.spill)}, true
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
func goCallWord(site *ssa.CallSite) uint64 {
	if site.Super {
		// A super()'s, of the this slot and the arguments (GoCallSuper).
		return goOpWord(&ssa.CallSite{Go: ssa.GoCall, Argc: site.Argc, Super: true})
	}
	if site.Accessor != ssa.AccessorNone {
		return goOpWord(&ssa.CallSite{Go: ssa.GoCall, Argc: site.Argc, Accessor: site.Accessor, Key: site.Key})
	}
	return goOpWord(&ssa.CallSite{Go: ssa.GoCall, Argc: site.Argc, Method: site.Method, New: site.Pool != 0})
}

// derivedFunction is the function object whose code a derived
// construction runs (abi.Context's Function): the class constructed, or,
// for one with no constructor of its own, the class whose code it runs
// (ssa.CallSite's Parent).
func derivedFunction(t *ssa.CallSite) uintptr {
	if t.Parent != 0 {
		return t.Parent
	}
	return t.Callee
}

// lengthWrites reports whether a call of Go, site, of argc operands, is a
// property's write of length (abi.Encoding's LengthKey), which native code
// makes itself where it shortens an array (lengthWrite), the call Go's
// otherwise: the receiver and the value, the values 16 bytes.
func lengthWrites(site *ssa.CallSite, argc int, enc abi.Encoding) bool {
	return site.Go == ssa.GoCall && site.Accessor == ssa.AccessorSet && enc.LengthKey != 0 && site.Key == enc.LengthKey &&
		argc == 2 && enc.ValueSize == 16 && enc.WriteBarrier != 0
}

// goOpWord is the word a call of Go, site, passes Go (abi.Context's GoOp).
func goOpWord(site *ssa.CallSite) uint64 {
	op := uint64(site.Go)
	if site.Go == ssa.GoArith || site.Go == ssa.GoAdd || site.Go == ssa.GoAppend {
		// What of its operands' BigInts it may give back.
		op |= uint64(site.Release) << abi.GoOpKindShift
	}
	if site.Go == ssa.GoClosure || site.Go == ssa.GoClose || site.Go == ssa.GoCompare || site.Go == ssa.GoNewArray || site.Go == ssa.GoIndexOf || site.Go == ssa.GoArith ||
		site.Go == ssa.GoBigString {
		// The template's constant, the first local closed, the operator
		// compared with or applied, whether an array has room, an
		// indexOf's form, or a toString's arguments (abi.GoOpIndexShift).
		op |= uint64(uint32(site.Argc)) << abi.GoOpIndexShift
	}
	if (site.Go == ssa.GoClosure || site.Go == ssa.GoCells || site.Go == ssa.GoClose) && site.Pool != 0 {
		op |= abi.GoClosurePool << abi.GoOpKindShift
	}
	if site.Go == ssa.GoConcat {
		op |= uint64(site.Argc) << abi.GoOpArgcShift
	}
	if site.Go == ssa.GoLeaf {
		// Its arguments, and whether a receiver comes before them.
		op |= uint64(site.Argc) << abi.GoOpArgcShift
		if site.Method {
			op |= abi.GoCallMethod << abi.GoOpKindShift
		}
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
		case site.Rest:
			op |= abi.GoCallRest << abi.GoOpKindShift
		case site.Fill:
			op |= abi.GoCallFill << abi.GoOpKindShift
		case site.Step:
			op |= abi.GoCallStep << abi.GoOpKindShift
		case site.TypeOf:
			op |= abi.GoCallTypeOf << abi.GoOpKindShift
		case site.InstanceOf:
			op |= abi.GoCallInstanceOf << abi.GoOpKindShift
		case site.Super:
			op |= abi.GoCallSuper << abi.GoOpKindShift
		case site.Accessor == ssa.AccessorGet:
			op |= abi.GoCallGet<<abi.GoOpKindShift | uint64(site.Key)<<abi.GoOpIndexShift
		case site.Accessor == ssa.AccessorSet:
			op |= abi.GoCallSet<<abi.GoOpKindShift | uint64(site.Key)<<abi.GoOpIndexShift
		}
	}
	return op
}

// b2i is 1 for true, 0 for false.
func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

// stepGens reports whether v, a loop's step (ssa.CallSite's Step), resumes
// generators natively (CallSite's Gens): of 16-byte values, the encoding
// knowing a generator's layout and the collector's flag.
func stepGens(v *ssa.Value, enc abi.Encoding) bool {
	if enc.GenData == 0 || enc.ValueSize != 16 || enc.WriteBarrier == 0 || len(v.Calls()) != 1 {
		return false
	}
	site := v.Calls()[0]
	return len(site.Gens) != 0 && site.GenNext != 0
}
