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
	f   *ssa.Func
	enc abi.Encoding
	// opts are the switches the compile runs with (Options).
	opts  Options
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
	// exitView has loc give a split value's slot (loc's split), as what
	// reads a frame state does -- an exit, its description, a call's
	// records and the callee's arguments it copies from its state --
	// where the rest of the code is given its register.
	exitView bool
	// origin is each tagged value's (ssa.Origins): the slot an exit looks
	// in for the reference the value may be.
	origin ssa.OriginMap
	// gprs and fprs are the architecture's allocatable registers, by number.
	gprs, fprs []int
	// spills counts the spill slots the allocation took (Code's Spills).
	spills int
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
	// limit, so a small function takes little. exitHead is, by a hash of
	// an entry (exitHash), 1 plus the index in the frame's Slots of the
	// last added of its chain, or 0, and exitNext, by entry, 1 plus that of
	// the next in its chain, or 0: as many chains as entries at least,
	// made again twice as many as they fill (exitRehash). A map had hashed
	// every entry -- a twentieth of a compile's time in the backend -- and
	// chains by slot had grown with every entry of a slot, each new one
	// compared with all its slot's: four million comparisons in a run of
	// Typescript's compiles.
	exitFrame *abi.ExitFrame
	// encp is the encoding the code keeps (encoding).
	encp *abi.Encoding
	// exitInlineLast is where the last description's run of the frame's
	// inlined callees starts (ExitFrame's Inlines), or -1.
	exitInlineLast int
	exitHead       []int32
	exitNext       []int32
	// exitNodes are what exitNode found of each node of the states'
	// slots, by its ID; exitKids its list of a node's entries as it makes
	// them, and exitCross the slots the leaves' entries name as their
	// references' sources (exitNodeInfo's).
	exitNodes []exitNodeInfo
	exitKids  []uint32
	exitCross []int32
	// walk walks the states' slots' nodes (findLazy, allocate): the
	// workspace's, kept from compile to compile, or the core's own.
	walk      *ssa.SlotWalk
	ownWalk   ssa.SlotWalk
	exitWords map[uint64]int32
	exitDescs []abi.ExitDescriptor
	// calls are the descriptions of the native calls' states (callState),
	// which the code keeps (Code.Calls); goCalls its calls of Go's
	// (goState).
	calls []*abi.CallState
	// callSlab and goSlab are where the calls' states are carved from, as
	// exitDescs, and cs what their records are built with.
	callSlab []abi.CallState
	goSlab   []abi.GoCallState
	cs       *callScratch
	goCalls  []*abi.GoCallState
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
	// steps counts the compile's steps in the loops whose length is not
	// the item's own -- a block's predecessors, a loop's blocks -- for the
	// test that the compile's work is linear (Times' Steps).
	steps int
	// masks are loopMasks's.
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
// noBudget is a cold path's budget where none was set for it
// (setBudget): it takes the budget in force as it is emitted.
const noBudget = -2

type budget struct {
	header *ssa.Block
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
	if c.ws != nil {
		stack, c.budgets = c.ws.budgetStack[:0], c.ws.budgets[:0]
		defer func() { c.ws.budgetStack, c.ws.budgets = stack[:0], c.budgets[:0] }()
	}
	// back and fwd mark, by block ID, the blocks of the loop of header h
	// as its walks find them, with 1 plus h's ID: each walk's own marks,
	// and its blocks listed (body), so that a loop costs its own blocks,
	// not the function's -- a table of every block cleared for each loop,
	// and every block looked at, had taken a function of n loops n²
	// steps, 45 ms of a compile of 2,000 loops.
	var back, fwd []int
	var body []*ssa.Block
	if c.ws != nil {
		body = c.ws.budgetBody[:0]
	}
	var chains chainTables
	for _, h := range f.Blocks {
		if !h.LoopHeader {
			continue
		}
		if back == nil {
			back, fwd = c.ints(nb), c.ints(nb)
		}
		mark := h.ID + 1
		back[h.ID] = mark
		stack = stack[:0]
		for i, p := range h.Preds {
			if h.Backedge[i] && back[p.ID] != mark {
				back[p.ID] = mark
				stack = append(stack, p)
			}
		}
		for len(stack) > 0 {
			b := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			c.steps += len(b.Preds)
			for _, p := range b.Preds {
				if back[p.ID] != mark {
					back[p.ID] = mark
					stack = append(stack, p)
				}
			}
		}
		// Of those, the blocks the header reaches: an entry at a PC in the
		// loop's body (an entry block, then the body's blocks after it)
		// reaches a back edge without passing the header, but is no part
		// of the loop, which the edge from it enters (edge's load).
		fwd[h.ID] = mark
		body = append(body[:0], h)
		stack = append(stack[:0], h)
		for len(stack) > 0 {
			b := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			c.steps += len(b.Succs)
			for _, s := range b.Succs {
				if back[s.ID] == mark && fwd[s.ID] != mark {
					fwd[s.ID] = mark
					body = append(body, s)
					stack = append(stack, s)
				}
			}
		}
		ok := true
		for _, b := range body {
			if b != h && b.LoopHeader || b.ID < len(c.budgetOf) && c.budgetOf[b.ID] != 0 {
				ok = false
				break
			}
			c.steps += len(b.Values)
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
		if !ok || c.turnChain(h, body, fwd, &chains) >= counterChain {
			continue
		}
		if c.budgetOf == nil {
			c.budgetOf = c.ints(nb)
		}
		c.budgets = append(c.budgets, budget{header: h})
		for _, b := range body {
			c.budgetOf[b.ID] = len(c.budgets)
		}
	}
	if c.ws != nil {
		c.ws.budgetBody = body[:0]
	}
	// Each loop's register a value of its own, made once the loops are
	// known, from the workspace's: no value made for each.
	var vals []ssa.Value
	if c.ws != nil {
		vals = c.ws.budgetVals
	}
	if cap(vals) < len(c.budgets) {
		vals = make([]ssa.Value, len(c.budgets))
	}
	vals = vals[:len(c.budgets)]
	for i := range c.budgets {
		vals[i] = ssa.Value{ID: f.NumValues() + i, Type: ssa.Int64}
		c.budgets[i].v = &vals[i]
	}
	if c.ws != nil {
		c.ws.budgetVals = vals
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
func (c *core) turnChain(h *ssa.Block, body []*ssa.Block, in []int, t *chainTables) int {
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
	for _, b := range body {
		c.steps += len(b.Values)
		for _, v := range b.Values {
			t.at[v.ID] = t.loop
			if b == h && v.Op == ssa.OpPhi {
				t.at[v.ID] = -t.loop
			}
		}
	}
	chain := 0
	for _, p := range h.Values {
		if p.Op != ssa.OpPhi {
			break
		}
		t.phi++
		for i, a := range p.Args {
			if h.Backedge[i] && in[h.Preds[i].ID] == h.ID+1 {
				chain = max(chain, t.path(a, p, nv))
			}
		}
	}
	return chain
}

// path is the longest path, in cycles, from the header phi p to v through
// the loop being looked at (turnChain), or -1 where none reaches it; nv is
// the function's values' count.
func (t *chainTables) path(v, p *ssa.Value, nv int) int {
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
		d = max(d, t.path(a, p, nv))
	}
	if d >= 0 && v.Op != ssa.OpPhi {
		d += latency(v.Op)
	}
	t.memo[v.ID] = d
	return d
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
	case ssa.OpObjectOf, ssa.OpArrayOf, ssa.OpIndexedOf, ssa.OpElemRead, ssa.OpBigElemRead, ssa.OpElemCell,
		ssa.OpLength, ssa.OpPropRead, ssa.OpPropCell, ssa.OpLoadCell, ssa.OpStringProp,
		ssa.OpStringCode, ssa.OpStringChar, ssa.OpCodeChar, ssa.OpCodeCharOr, ssa.OpTypeofCell, ssa.OpSubstringChar, ssa.OpStringCodePointOr, ssa.OpStringAtOr, ssa.OpCursorCell,
		ssa.OpUpvalueCell, ssa.OpLocalCell, ssa.OpGlobalCell, ssa.OpLexCell, ssa.OpBindingCell, ssa.OpHolderCell:
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
//
// cold marks, at a call of Go, a value only the call's refusal reads after
// it: saved for that, restored only on its way (mir's goCallingLive), not
// on the way the code goes on after the call.
type saved struct {
	reg, slot int
	float     bool
	frame     int
	id        int
	cold      bool
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
	if s := v.State; s == nil || o >= s.Len() || s.Slot(o) != nil && s.Slot(o) != y {
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
func prepare(w *Workspace, f *ssa.Func, enc abi.Encoding, opts Options, gprs, fprs []int) (*core, error) {
	f.Finish()
	var c *core
	if w != nil {
		c = &w.core
		*c = core{f: f, enc: enc, opts: opts, gprs: gprs, fprs: fprs, ws: w, sched: &w.sched, cs: &w.calls}
		c.cs.records = c.cs.records[:0]
	} else {
		c = &core{f: f, enc: enc, opts: opts, gprs: gprs, fprs: fprs, cs: new(callScratch)}
		c.sched = &c.own
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
	if n := f.Locals + f.StackSize; n > abi.MaxSlot {
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
	c.layout()
	if err := c.findLazy(); err != nil {
		return nil, err
	}
	c.findBudgets()
	c.lap(PhasePrepare)
	if err := c.allocate(); err != nil {
		return nil, err
	}
	c.lap(PhaseSaves)
	return c, nil
}

// keepRunAt marks keep v, b's i'th value, if it is the first or the last
// of a run of keeps (keepRuns): each keep made, of a block's values one
// after another.
func (c *core) keepRunAt(b *ssa.Block, i int, v *ssa.Value) {
	isKeep := func(v *ssa.Value) bool { return v.Op == ssa.OpKeep && !c.isLazy(v) }
	if !isKeep(v) {
		return
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
	// TailEntry is the offset of the entry another function's call in
	// tail position jumps to (ssa.Func's TailEntry), or -1 for none: a
	// routine that makes what the call entry makes past the frame's window,
	// of the frame's size -- its operands' address, the VM's stack's top
	// and high water -- and its bindings past its parameters undefined, as
	// the call entry does, then goes on at the start.
	TailEntry int
	// Text is how many of Bytes are instructions: past them are the code's
	// own constants (amd64's movImm), data.
	Text int
	// Calls are the descriptions of its native calls' states, which the
	// calls name by address (abi.Context's CallState), as Exits; CallRefs
	// bounds the pointer words a call writes itself (abi.CallRecord's Ref),
	// the context's RecordRef[:CallRefs].
	Calls    []*abi.CallState
	CallRefs int
	// Spills counts the spill slots the code uses: past abi.SpillSlots,
	// the context it runs in must have the rest in its SpillMore
	// (abi.Context's GrowSpill) before it runs there.
	Spills int
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

// Spilled counts the values the allocation put in spill slots, for
// measuring the code: a value lazy or in a register is none.
func (c *Code) Spilled() int {
	n := 0
	k := c.core
	for _, b := range k.order {
		for _, v := range b.Values {
			if l, ok := k.loc(v); ok && !k.isLazy(v) && l.reg < 0 {
				n++
			} else if _, _, split := k.split(v); split {
				n++
			}
		}
	}
	return n
}

// loc is where a value lives: a register of its class, or a spill slot;
// or, split (allocate's split), both: the register from its definition to
// its last use but a frame state's, and the slot, which the code stores it
// to as it is made (the emitters' block) and every frame state reads
// (exitView).
type loc struct {
	reg   int // register number, or -1
	spill int // spill slot, or -1
}

// split is the register and the slot of v if it is split (loc), and
// whether it is.
func (c *core) split(v *ssa.Value) (reg, slot int, ok bool) {
	if v.ID >= len(c.locs) || !c.hasLoc[v.ID] {
		return 0, 0, false
	}
	l := c.locs[v.ID]
	return l.reg, l.spill, l.reg >= 0 && l.spill >= 0
}

// viewExits sets exitView, for what reads a frame state, and returns what
// sets it back: c.viewExits()() in a defer.
func (c *core) viewExits() func() {
	was := c.exitView
	c.exitView = true
	return func() { c.exitView = was }
}

// isLazy reports whether v has no location of its own (core.lazy).
func (c *core) isLazy(v *ssa.Value) bool { return v.ID < len(c.lazy) && c.lazy[v.ID] }

// isFused reports whether v is computed by its block's branch (core.fused).
func (c *core) isFused(v *ssa.Value) bool { return v.ID < len(c.fused) && c.fused[v.ID] }

// loc is v's location, and whether it has one: of a split value, its slot
// if exitView is set, else its register.
func (c *core) loc(v *ssa.Value) (loc, bool) {
	if v.ID >= len(c.locs) || !c.hasLoc[v.ID] {
		return loc{}, false
	}
	l := c.locs[v.ID]
	if l.reg >= 0 && l.spill >= 0 {
		if c.exitView {
			l.reg = -1
		} else {
			l.spill = -1
		}
	}
	return l, true
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
	// A register program's scratch slots (ssa.Func's Scratch), written
	// inside one register instruction, are values, in no frame state.
	for i := f.FrameLocals; i < f.Locals-f.Scratch; i++ {
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
//
// It reads every frame state once: each value, block state and loop
// header names a state, and many name the same one -- the guards of an
// instruction share its state -- so the states' references are counted
// (refs, by state ID) as the values are met, the states listed the first
// time each is, and each listed state's slots are read once, counted
// that many times as uses (findFused's).
//
// It refuses a function whose values merge two slots' references with no
// shadow to tell which (ssa.Origins), as it meets each.
func (c *core) findLazy() error {
	f := c.f
	n := f.NumValues()
	used := c.bools(n)  // by an argument, a control or a phi
	byPhi := c.bools(n) // by a phi
	// A slot loaded at an entry and named only by frame states for that same
	// slot needs no register: an exit leaves that slot as it is.
	elsewhere := c.bools(n)
	// Uses, counted here rather than taken from ssa.Value's Uses: a fused
	// value has no location, which any other use would need (findFused).
	uses := c.int32s(n)
	refs := c.int32s(f.NumStates())
	states := c.stateList()
	ref := func(s *ssa.FrameState) {
		if s != nil {
			if refs[s.ID()]++; refs[s.ID()] == 1 {
				states = append(states, s)
			}
		}
	}
	for _, b := range f.Blocks {
		for _, v := range b.Values {
			if o, ok := c.origin.Of(v); ok && (o == ssa.OriginAmbiguous || o == ssa.OriginHeap) && v.Shadow == nil {
				return fmt.Errorf("%w: %v merges two slots' values with no shadow", ErrUnsupported, v)
			}
			for _, a := range v.Args {
				used[a.ID] = true
				uses[a.ID]++
				if v.Op == ssa.OpPhi {
					byPhi[a.ID] = true
				}
				if a.Op == ssa.OpLoadSlot && !(v.Op == ssa.OpUnboxF64 && v.Block == a.Block) {
					// But its unboxing in its entry block, which reads the
					// slot itself (materialize), nothing having written it
					// since: an entry of a loop of many locals had held each
					// in a register to its last guard, as many as it loads.
					elsewhere[a.ID] = true
				}
			}
			if v.Shadow != nil {
				uses[v.Shadow.ID]++
			}
			ref(v.State)
		}
		if b.Control != nil {
			uses[b.Control.ID]++
			if b.Kind != ssa.BlockReturn {
				used[b.Control.ID] = true
			}
			if b.Control.Op == ssa.OpLoadSlot {
				elsewhere[b.Control.ID] = true
			}
		}
		ref(b.State)
		ref(b.Header)
	}
	// Each node of the states' slots once, its weight the references of
	// the states and the nodes that have it, summed from the roots down
	// (ssa's SlotWalk): read state by state, a function of n values live
	// at once and an exit at each of its instructions had been read n²
	// times.
	weight := c.int32s(f.NumSlotNodes())
	for _, s := range states {
		if root, _ := s.Tree(); root != nil {
			weight[root.ID()] += refs[s.ID()]
		}
	}
	c.slotWalker().Walk(f, states, func(nd *ssa.SlotNode, h, base int) {
		k := weight[nd.ID()]
		if h > 0 {
			for _, kid := range nd.Kids() {
				weight[kid.ID()] += k
			}
			return
		}
		m := nd.Mask()
		for _, v := range nd.Vals() {
			i := base + bits.TrailingZeros64(m)
			m &= m - 1
			uses[v.ID] += k
			if !(v.Op == ssa.OpLoadSlot && v.Aux == i) {
				elsewhere[v.ID] = true
			}
		}
	})
	c.keepStates(states)
	c.lazy = c.bools(n)
	// An int32's conversion only lazy boxes use is made by none (i32Boxed):
	// boxUses counts such uses.
	boxUses := c.int32s(n)
	for _, b := range f.Blocks {
		for _, v := range b.Values {
			switch v.Op {
			case ssa.OpConstSource:
				c.lazy[v.ID] = true
			case ssa.OpConst, ssa.OpBoxF64, ssa.OpBoxBool:
				if !used[v.ID] {
					c.lazy[v.ID] = true
				}
			case ssa.OpConstI32:
				// Made where it is used, as an immediate where it can be
				// (truncInts' constants, shifts' counts); a phi's needs
				// its location.
				if !byPhi[v.ID] {
					c.lazy[v.ID] = true
				}
			case ssa.OpLoadSlot:
				// A typed code's raw slot's, loaded where it is, never
				// lazily: materialize makes a word.
				if !elsewhere[v.ID] && v.Type == ssa.Tagged {
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
	for _, b := range f.Blocks {
		for _, v := range b.Values {
			if x := v.Args; v.Op == ssa.OpBoxF64 && c.lazy[v.ID] && x[0].Op == ssa.OpI32ToF64 {
				boxUses[x[0].ID]++
			}
		}
	}
	for _, b := range f.Blocks {
		for _, v := range b.Values {
			if v.Op == ssa.OpI32ToF64 && boxUses[v.ID] != 0 && boxUses[v.ID] == uses[v.ID] {
				c.lazy[v.ID] = true
			}
		}
	}
	c.findFused(uses)
	return nil
}

// fusable reports whether a boolean operation can be computed by a branch
// as its test, as a compare and a conditional jump (each backend's test),
// and whether it is pure: it reads nothing but its operands, in their
// locations, nor exits, so that what is emitted between it and the branch
// changes nothing it reads, its operands kept live to the branch.
func fusable(op ssa.Op) (ok, pure bool) {
	switch op {
	case ssa.OpNot, ssa.OpCmpF64, ssa.OpCmpI64, ssa.OpCmpI32, ssa.OpStrictNullish, ssa.OpCellNone:
		return true, true
	case ssa.OpLooseNullish, ssa.OpEqTagged, ssa.OpTypeIs, ssa.OpInstanceOf, ssa.OpTruth, ssa.OpKeyIs:
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
func (c *core) findFused(uses []int32) {
	n := c.f.NumValues()
	c.fused = c.bools(n)
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

// edgeWeights are allocate's weights of an edge by the loops holding both
// its ends: 16 to the power of them, up to three.
var edgeWeights = [4]int{1, 16, 256, 4096}

// startsLoop reports whether the i'th block laid out begins the code of a
// loop (of the first 64, loopMasks's): it is in a loop the block before
// it is not in. That block, not the loop's header, is the one aligned
// (amd64's Align): a loop's blocks laid out before its header -- the
// update of a for loop, a join on the way back -- end in the edge back
// to it, which had fallen into the header's padding, a jump over it, on
// every turn (microbench's array_for and prop_in: 6 and 7% slower than
// with no padding). Before the loop's first block only the ways into the
// loop pass the padding.
func (c *core) startsLoop(i int) bool {
	m := c.loopMasks()
	b := c.order[i]
	if i == 0 {
		return m[b.ID] != 0
	}
	return m[b.ID]&^m[c.order[i-1].ID] != 0
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
	c.dropped = nil
	nv, nb := c.f.NumValues()+len(c.budgets), numBlocks(c.f)
	// Number positions: each block has a start, one position per value, and
	// an end; count bounds them. most bounds the phis' groups' members
	// (below).
	pos := c.ints(nv)
	for i := range pos {
		pos[i] = -1
	}
	start, end := c.ints(nb), c.ints(nb)
	// callIn is, by block ID, the position of the last call in it that
	// saves the registers it keeps (savingCall), or -1: a value read after
	// one is not split.
	callIn := c.ints(nb)
	// Intervals, by value ID (v nil until made): each from its definition
	// -- a phi's from its block's start -- to its last use, stretched over
	// every block where the value is live in or out. The uses are met once,
	// block by block: each stretches its value's interval to it, and one in
	// another block than its value's is noted (xuses) for the liveness,
	// a dataflow over the blocks of those values' persistent sets
	// (blockLiveness).
	// What the uses read and write of each value is in one record (rec):
	// its last use's position, its block, the head of its uses in other
	// blocks, and how a frame state's slot naming it is used. The records
	// and the intervals are begun as the positions are numbered.
	need := func(v *ssa.Value) bool { return hasResult(v) && !c.isLazy(v) }
	rec := c.useRecs(nv)
	iv := c.intervalList(nv)
	count, most := 0, 0
	inOrder := c.bools(nb)
	for _, b := range c.order {
		inOrder[b.ID] = true
		start[b.ID] = count
		callIn[b.ID] = -1
		count++
		for _, v := range b.Values {
			p := count
			pos[v.ID] = p
			count++
			if savingCall(v) {
				callIn[b.ID] = p
			}
			if v.Op == ssa.OpPhi {
				most += 1 + len(v.Args)
				p = start[v.Block.ID]
			}
			r := &rec[v.ID]
			r.def = -1
			if need(v) {
				iv[v.ID] = interval{v: v, start: p, end: p}
				r.end, r.def, r.real, r.start = int32(p), int32(b.ID)+1, int32(p), int32(p)
			}
			if bigKept(v) != nil || c.isLazy(v) || remat(v) {
				r.slot = slotMade
			}
			if v.Shadow != nil {
				r.slot |= slotShadow
			}
			if isFloat(v) {
				r.slot |= slotFloat
			}
		}
		end[b.ID] = count
		count++
	}
	// xuses are the uses in another block than their value's, a block once
	// for each value in a row: each's next is 1 plus the index of the one
	// before it of its value, or 0, from its record's xhead.
	xuses := c.xuseList()
	var cur *ssa.Block
	// refused says the uses being met are a call of Go's state's, which
	// only its refusal reads (below): they leave a record's hot as it is.
	// inState says they are a frame state's, which leave its real as it is
	// (useRec's); lastCall is the position of the last call in cur met
	// that saves the registers it keeps, or -1; again says they are a
	// call's that may read them again after Go has had the registers
	// (callsAgain), as its state's are; own is the position of the first
	// of the values whose state they are, the guards of one instruction,
	// or -1.
	refused, inState, again, lastCall, own := false, false, false, -1, -1
	use := func(x *ssa.Value, p int) {
		r := &rec[x.ID]
		if r.def <= 0 {
			if r.def == 0 && need(x) {
				// Only a value defined outside the function has none.
				panic(fmt.Sprintf("use of %v defined outside the function", x))
			}
			return
		}
		r.end = max(r.end, int32(p))
		if !refused {
			r.hot = max(r.hot, int32(p))
		}
		if inState {
			if r.def == int32(cur.ID)+1 && own >= 0 && int(r.start) >= own {
				// Its own instruction's state, read as it is made.
				r.slot |= slotFar
			}
		} else {
			r.real = max(r.real, int32(p))
			// The call before p: before the value at p, or any in cur
			// for its end's -- a branch's, an edge's.
			lc := lastCall
			if p == end[cur.ID] {
				lc = callIn[cur.ID]
			}
			if r.def != int32(cur.ID)+1 || again || lc >= 0 && lc > int(r.start) {
				r.slot |= slotFar
			}
		}
		if r.def != int32(cur.ID)+1 {
			if h := r.xhead; h == 0 || xuses[h-1].b != cur {
				xuses = append(xuses, xuse{cur, h, int32(p)})
				r.xhead = int32(len(xuses))
			} else {
				xuses[h-1].at = max(xuses[h-1].at, int32(p))
			}
		}
	}
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
		inState = true
		add := func(x *ssa.Value) {
			if moved == nil || x.Op != ssa.OpPhi || x.Block != moved {
				use(x, at)
			}
		}
		for _, v := range s.All() {
			c.steps++
			if moved == nil && rec[v.ID].slot == 0 {
				// Neither a BigInt's box, nor made where needed, nor with
				// a shadow: the value itself.
				use(v, at)
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
				if x := i32Boxed(v); x != nil {
					// A box of an int32's conversion, made of the int32.
					add(x)
					continue
				}
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
		inState = false
	}
	// laterState is useState for a state with no moved header, put off to
	// the end of its block (useStates): the states of a block share their
	// slots' nodes, which useStates meets once each, every value as the
	// states that name it ask -- at the last of their positions, the last
	// of those Go does not refuse, and the first of their instructions'
	// own -- where a function of n values live at once and an exit at
	// each of its instructions had been read n² times. A use does the same
	// in any order, but for the uses in other blocks it lists, which are
	// listed by block, as the block's are.
	var later []laterUse
	if c.ws != nil {
		later = c.ws.laterUses[:0]
	}
	laterState := func(s *ssa.FrameState, at int) {
		if s != nil {
			if root, _ := s.Tree(); root != nil {
				later = append(later, laterUse{s, int32(at), int32(own), refused})
			}
		}
	}
	var asks []stateAsk
	var roots []*ssa.FrameState
	mark := int32(0)
	ask := func(id int, end, hot, own int32) {
		a := &asks[id]
		if a.mark != mark {
			*a = stateAsk{mark: mark, end: -1, hot: -1, own: math.MaxInt32}
		}
		a.end, a.hot, a.own = max(a.end, end), max(a.hot, hot), min(a.own, own)
	}
	// useAsk is use, in a state, of x as a asks.
	useAsk := func(x *ssa.Value, a *stateAsk) {
		r := &rec[x.ID]
		if r.def <= 0 {
			if r.def == 0 && need(x) {
				panic(fmt.Sprintf("use of %v defined outside the function", x))
			}
			return
		}
		r.end = max(r.end, a.end)
		if a.hot >= 0 {
			r.hot = max(r.hot, a.hot)
		}
		if r.def == int32(cur.ID)+1 && a.own != math.MaxInt32 && r.start >= a.own {
			// Its own instruction's state, read as it is made.
			r.slot |= slotFar
		}
		if r.def != int32(cur.ID)+1 {
			if h := r.xhead; h == 0 || xuses[h-1].b != cur {
				xuses = append(xuses, xuse{cur, h, a.end})
				r.xhead = int32(len(xuses))
			} else {
				xuses[h-1].at = max(xuses[h-1].at, a.end)
			}
		}
	}
	useStates := func() {
		if len(later) == 0 {
			return
		}
		if asks == nil {
			asks = c.stateAsks(c.f.NumSlotNodes())
		}
		mark++
		roots = roots[:0]
		for _, u := range later {
			root, _ := u.s.Tree()
			hot, own := u.at, u.own
			if u.refused {
				hot = -1
			}
			if own < 0 {
				own = math.MaxInt32
			}
			ask(root.ID(), u.at, hot, own)
			roots = append(roots, u.s)
		}
		later = later[:0]
		c.slotWalker().Walk(c.f, roots, func(nd *ssa.SlotNode, h, base int) {
			a := &asks[nd.ID()]
			c.steps++
			if h > 0 {
				for _, kid := range nd.Kids() {
					ask(kid.ID(), a.end, a.hot, a.own)
				}
				return
			}
			for _, v := range nd.Vals() {
				// As useState's.
				if rec[v.ID].slot == 0 {
					useAsk(v, a)
					continue
				}
				if x := bigKept(v); x != nil {
					useAsk(x, a)
					if c.isLazy(v) {
						continue
					}
				}
				if c.isLazy(v) || remat(v) {
					if x := i32Boxed(v); x != nil {
						useAsk(x, a)
						continue
					}
					for _, x := range v.Args {
						useAsk(x, a)
					}
					continue
				}
				useAsk(v, a)
				if v.Shadow != nil {
					useAsk(v.Shadow, a)
				}
			}
		})
	}
	// A phi's arguments are used at its block's predecessors' ends, as its
	// block is met: the phis are walked in order, each argument read once,
	// and what the groups (below) take of them noted as they are -- the
	// first phi of each argument (phiOf), the phis and their arguments
	// that have intervals numbered as members (mem, and each's record's
	// mid), each member's heaviest edge (mw), and the pairs of a phi and
	// an argument of one class, which may be joined.
	phiOf := c.valueList(nv)
	var masks valueSet
	weight := func(b *ssa.Block, i int) int {
		if masks == nil {
			masks = c.loopMasks()
		}
		return edgeWeights[min(bits.OnesCount64(masks[b.ID]&masks[b.Preds[i].ID]), 3)]
	}
	// run is, by position plus 1, how often the positions up to it run
	// (each its block's weight, bw below), from 0: the scan weighs what
	// spilling an interval gives up by it.
	run := c.words(count + 1)
	// spend adds w to what x costs in a spill slot (useRec's cost).
	spend := func(x *ssa.Value, w int32) {
		r := &rec[x.ID]
		r.cost = int32(min(int64(r.cost)+int64(w), math.MaxInt32))
	}
	mem, pairs := c.int32s(most)[:0], c.int32s(2 * most)[:0]
	gm := c.groupMembers(most)
	member := func(r *useRec, id int) int32 {
		if r.mid == 0 && r.def > 0 {
			mem = append(mem, int32(id))
			r.mid = int32(len(mem))
		}
		return r.mid - 1
	}
	// Of a block with phis, each predecessor's end -- -1 for one not laid
	// out -- and its edge's weight, found once for all its phis.
	var predEnd, predW []int32
	// kv are the values the calls' saves and the keep cells' coloring
	// look at -- the calls, the keeps, the BigInts' boxes, the cells'
	// addresses, the phis of sources -- in order, met with the uses.
	kv := c.keepList()
	for _, b := range c.order {
		cur = b
		if masks == nil {
			masks = c.loopMasks()
		}
		// How often b runs, against the function's entry, as weight's edges;
		// and so each of its positions, summed into run.
		bw := int32(edgeWeights[min(bits.OnesCount64(masks[b.ID]), 3)])
		for p := start[b.ID]; p <= end[b.ID]; p++ {
			run[p+1] = run[p] + uint64(bw)
		}
		if len(b.Values) != 0 && b.Values[0].Op == ssa.OpPhi {
			predEnd, predW = predEnd[:0], predW[:0]
			for i, p := range b.Preds {
				e := int32(-1)
				if inOrder[p.ID] {
					e = int32(end[p.ID])
				}
				predEnd, predW = append(predEnd, e), append(predW, int32(weight(b, i)))
			}
		}
		for _, v := range b.Values {
			if v.Op != ssa.OpPhi {
				break
			}
			if v.Type == ssa.Source {
				kv = append(kv, v)
			}
			mv := member(&rec[v.ID], v.ID)
			for i, a := range v.Args {
				if e := predEnd[i]; e >= 0 {
					cur = b.Preds[i]
					use(a, int(e))
					if a != v {
						// The edge's move reads the argument and writes
						// the phi; a phi that is its own argument there,
						// a value the loop does not change, moves nothing.
						spend(a, predW[i])
						spend(v, predW[i])
					}
				}
				if a != v && phiOf[a.ID] == nil {
					phiOf[a.ID] = v
				}
				ma := int32(-1)
				if a.ID < nv {
					ma = member(&rec[a.ID], a.ID)
				}
				w := predW[i]
				if mv >= 0 {
					gm[mv].weight = max(gm[mv].weight, w)
				}
				if ma < 0 {
					continue
				}
				gm[ma].weight = max(gm[ma].weight, w)
				if a != v && mv >= 0 && isFloat(a) == isFloat(v) {
					pairs = append(pairs, mv, ma)
				}
			}
		}
		cur, lastCall = b, -1
		// first is the position of the first value of the instruction
		// whose guards are being met, which share its state.
		first := -1
		for i, v := range b.Values {
			switch v.Op {
			case ssa.OpPhi:
				continue // phi arguments are used at their predecessors' ends
			case ssa.OpCall, ssa.OpKeep, ssa.OpBigBox, ssa.OpCallCell, ssa.OpBigCell, ssa.OpMathCall:
				kv = append(kv, v)
				if v.Op == ssa.OpKeep {
					c.keepRunAt(b, i, v)
				}
			}
			// A fused value is computed at its block's branch (findFused):
			// what it reads is used there.
			p, fused := pos[v.ID], c.isFused(v)
			if first < 0 {
				first = p
			}
			if fused {
				p = end[b.ID]
			}
			spend(v, bw)
			again = callsAgain(v)
			// A call whose pool Go fills is made again from its start, its
			// operands read again after Go had the registers: they live
			// across it, saved and restored (saves). Its state names them
			// where they are its frame's slots; a register program's
			// literal's elements are in scratch slots no state names, and a string element's words,
			// in registers, had been what Go's call left there.
			pa := p
			if !fused && ssa.RefillsPools(v) {
				pa = p + 1
			}
			for _, a := range v.Args {
				use(a, pa)
				spend(a, bw)
				if a.Op == ssa.OpBoxF64 || a.Op == ssa.OpBoxBool {
					// An operation may make a box again of its operand
					// (remat) rather than read its word. A frame state naming
					// the box keeps the operand too, but a captured local's
					// value is in no state, and its register had gone to
					// another's: a Go call's argument was what that held.
					use(a.Args[0], pa)
					spend(a.Args[0], bw)
					if x := i32Boxed(a); x != nil {
						// A box of an int32's conversion is made of the
						// int32 (materialize).
						use(x, pa)
						spend(x, bw)
					}
				}
				if r := &rec[a.ID]; r.slot&slotShadow != 0 || r.def == 0 && a.Shadow != nil {
					// It may read where its operand came from: a keep
					// (mir's keep), or an operation that finds an object's
					// pointer word there (sourceRef). A value the frame
					// state names keeps its shadow alive there too, but a
					// captured local's is in no state (its slot is its
					// cell), and the slot IR reads a local as an operand.
					use(a.Shadow, pa)
					spend(a.Shadow, bw)
				}
			}
			again = false
			if savingCall(v) {
				lastCall = p
			}
			at := p
			if callsAgain(v) {
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
			refused, own = at != p && v.Calls()[0].Go != 0, first
			laterState(v.State, at)
			refused, own, first = false, -1, -1
		}
		if b.Control != nil {
			if c.isLazy(b.Control) {
				for _, a := range b.Control.Args {
					use(a, end[b.ID])
					spend(a, bw)
				}
				if x := i32Boxed(b.Control); x != nil {
					// A return of an int32's number, made of the int32.
					use(x, end[b.ID])
					spend(x, bw)
				}
			} else {
				use(b.Control, end[b.ID])
				spend(b.Control, bw)
				if b.Control.Shadow != nil {
					use(b.Control.Shadow, end[b.ID])
				}
			}
		}
		laterState(b.State, end[b.ID])
		if b.LoopHeader {
			laterState(b.Header, start[b.ID])
		}
		for _, s := range b.Succs {
			if !s.LoopHeader {
				continue
			}
			for i, p := range s.Preds {
				if p != b {
					continue
				}
				if s.Backedge[i] {
					// A poll on this edge exits to the header's state,
					// after the moves.
					useState(s.Header, end[b.ID], s)
				}
			}
		}
		useStates()
	}
	clear(later[:cap(later)])
	if c.ws != nil {
		c.ws.laterUses = later[:0]
	}
	c.keepXuses(xuses)
	c.lap(PhaseUses)
	// Block-level liveness, so that values live around a loop stay live for
	// all of it: a value is live into each block from which a use of it in
	// another block than its own is reached without passing its own, and
	// out of each predecessor of such a block (blockLiveness), and its
	// interval is stretched over each.
	//
	// Each value's interval is then made, and counted by its start for the
	// sort (below): first, n and floats.
	lo, hi, liveIn, liveOut, lb := c.blockLiveness(rec, xuses, inOrder, start, end, nb)
	first := c.ints(count + 1)
	n, floats := 0, 0
	for id := range rec {
		r := &rec[id]
		if r.def <= 0 {
			continue
		}
		it := &iv[id]
		it.end = int(r.end)
		if lo[id] >= 0 {
			it.start, it.end = min(it.start, lo[id]), max(it.end, hi[id])
		}
		first[it.start+1]++
		n++
		if r.slot&slotFloat != 0 {
			floats++
		}
	}
	c.lap(PhaseLiveness)
	// Each budget loop's register, from its header's start to the end of
	// the last of its blocks laid out.
	for _, bu := range c.budgets {
		iv[bu.v.ID] = interval{v: bu.v, start: start[bu.header.ID], end: start[bu.header.ID]}
	}
	if len(c.budgets) > 0 {
		// One pass over the blocks for every loop, not one for each: n
		// loops had taken n² steps.
		c.steps += len(c.order)
		for _, b := range c.order {
			if k := c.loopBudget(b); k >= 0 {
				it := &iv[c.budgets[k].v.ID]
				it.start, it.end = min(it.start, start[b.ID]), max(it.end, end[b.ID])
			}
		}
	}
	for _, bu := range c.budgets {
		first[iv[bu.v.ID].start+1]++
		n++
	}
	// By start, then value ID: a counting sort over the positions, the
	// intervals counted as they were made, taken in ID order.
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
	c.lap(PhaseIntervals)
	// A phi and its arguments are given one register where they can be, as
	// a linear scan's hints have it: the phi the register of an argument
	// that is free when the phi begins, an argument the phi's if free when
	// it begins. An edge's phi moves (phiMoves) are then no moves at all:
	// given whichever register came free last, a fifth of a function's
	// code had been the moves at its edges. phiOf is, by value ID, a phi
	// the value is an argument of, or nil (made with the uses, above).
	// weight is how often an edge into b, from its i'th predecessor, runs
	// against the function's entry: 16 to the power of the loops (at most
	// three) holding both its ends, a move on an inner loop's edge running
	// many times one before it does.
	// Beyond those hints, a phi and its arguments are coalesced into one
	// group where none of their intervals meet, so that a value carried
	// round a loop through several phis -- a loop whose body has an edge
	// of its own into the header, as a try statement's has -- keeps one
	// register through all of them: given each its neighbour's, a phi in
	// a block laid out before the header took the register of a header
	// phi of another variable, and the loop-carried value went through a
	// chain of moves, one an edge (try_catch's sum: three). The groups are
	// made before the scan, from the intervals, and the scan alone uses
	// them: a second scan with them, kept where its edges moved less, had
	// been a compile's allocation again. The groups' members are the phis
	// and their arguments, numbered from 0 in mem (value IDs) as the phis
	// are met with the uses (most bounds them); a record's mid is 1 plus
	// its value's number, or 0 for a value of no group, which the groups'
	// tables leave out. Of members: me is the end of its interval; group is 1 plus its
	// group's root, or 0 for a root; mw is the weight of the heaviest edge
	// the member is a phi or an argument at. Of roots: hs and he are the
	// first start and the last end of the group's members' intervals; its
	// members are gn words of the pool from goff -- room for gcap -- each a
	// member's start, shifted 32 bits, and its number, in order of start
	// (a member's own interval meets none of its group's others, and none
	// shares a start): a member's interval is checked against a group by a
	// binary search, not against each member, and joins it by a copy.
	// The members' tables are filled from their intervals, and the pairs
	// listed with the uses kept where the two's own intervals do not meet,
	// in the order met, which is the order they are joined in.
	pool := c.groupPool(2 * most)
	for i, id := range mem {
		it := &iv[id]
		gm[i].end, gm[i].start, gm[i].last = int32(it.end), int32(it.start), int32(it.end)
		gm[i].off, gm[i].n, gm[i].room = int32(2*i), 1, 2
		pool[2*i] = uint64(it.start)<<32 | uint64(i)
	}
	// The spill classes (spillClasses) are made of every pair, the groups
	// of those whose intervals do not meet.
	sclass := c.spillClasses(rec, iv, pos, start, xuses, liveIn, liveOut, lb, mem, pairs)
	kept := pairs[:0]
	for p := 0; p < len(pairs); p += 2 {
		if mv, ma := pairs[p], pairs[p+1]; gm[mv].start > gm[ma].end || gm[ma].start > gm[mv].end {
			kept = append(kept, mv, ma)
		}
	}
	pairs = kept
	nm := len(mem)
	gm = gm[:nm]
	// members is root r's members, in order of start.
	members := func(r int) []uint64 { return pool[gm[r].off:][:gm[r].n] }
	find := func(i int) int {
		if g := gm[i].group; g != 0 {
			return int(g) - 1
		}
		return i
	}
	// root is the root of v's group, or -1 for a value of none.
	root := func(v *ssa.Value) int {
		if m := rec[v.ID].mid; m != 0 {
			return find(int(m) - 1)
		}
		return -1
	}
	// phiAt is the block of a phi that is a member, or nil.
	phiAt := func(v *ssa.Value) *ssa.Block {
		if rec[v.ID].mid != 0 {
			return v.Block
		}
		return nil
	}
	// overlap reports whether a member of group rb meets one of group ra:
	// for each of rb's, the last of ra's to begin no later than it ends,
	// which ends latest of those, ending no sooner than it begins.
	overlap := func(ra, rb int) bool {
		in := members(ra)
		for _, w := range members(rb) {
			s, e := w>>32, uint64(gm[uint32(w)].end)
			i, _ := slices.BinarySearch(in, e<<32|math.MaxUint32)
			if i > 0 && uint64(gm[uint32(in[i-1])].end) >= s {
				return true
			}
		}
		return false
	}
	// Each pair's groups are joined if no member of one meets a member of
	// the other: none where their spans are apart; else if the groups'
	// members meet nowhere (overlap) -- and not if the two groups' pairs of
	// members are more than 4096, as a check of the pairs had been bounded.
	// The smaller group's members join the larger's, each pointed at its
	// new root, the two arrays merged in order of start, from their ends.
	for p := 0; p < len(pairs); p += 2 {
		rx, ry := find(int(pairs[p])), find(int(pairs[p+1]))
		if rx == ry {
			continue
		}
		if gm[rx].n < gm[ry].n {
			rx, ry = ry, rx
		}
		if gm[rx].start <= gm[ry].last && gm[ry].start <= gm[rx].last && (int(gm[rx].n)*int(gm[ry].n) > 4096 || overlap(rx, ry)) {
			continue
		}
		from := members(ry)
		for _, w := range from {
			gm[uint32(w)].group = int32(rx) + 1
		}
		na, nb := int(gm[rx].n), len(from)
		if na+nb > int(gm[rx].room) {
			// Room twice what it now holds, at the pool's end.
			at := len(pool)
			pool = append(pool, make([]uint64, 2*(na+nb))...)
			copy(pool[at:], members(rx))
			gm[rx].off, gm[rx].room = int32(at), int32(2*(na+nb))
		}
		to := pool[gm[rx].off:][:na+nb]
		for i, j, k := na-1, nb-1, na+nb-1; j >= 0; k-- {
			if i >= 0 && to[i] > from[j] {
				to[k], i = to[i], i-1
			} else {
				to[k], j = from[j], j-1
			}
		}
		gm[rx].n = int32(na + nb)
		gm[rx].start, gm[rx].last = min(gm[rx].start, gm[ry].start), max(gm[rx].last, gm[ry].last)
	}
	c.keepGroupPool(pool)
	// A group's register is waited for while members of it are still to
	// come; wait is, by class and register, a list of the groups that
	// wait for it, linked through wnext (1 plus the next root, 0 at the
	// end; heads 1 plus a root), those done unlinked as they are met. A
	// register is spoken for (spoken), to a value of start s and end e,
	// where another group waits for it: the first member of a group does
	// not take it for the group (two groups waiting for one register, the
	// second to come had found it taken and gone through moves at every
	// edge: array_for_of's iterator, round its loop, two), nor does a
	// value with no hint where it is free. A value's hint passes over it
	// only where the group's member that would find it taken, still to
	// come, begins in s..e, and the group's edges weigh more than the
	// hint's (gw, weight): a hint given up for any group waiting -- or any
	// whose member would find the register taken -- had broken the hints'
	// chains through a large function's loops (Mandreel's sortMinDown, a
	// dozen moves a turn), while a value whose hint was an entry's edge
	// had taken an inner loop's group's register (array_for_of's iterator
	// again). left counts, by root, its members not yet given a location;
	// greg is, by root, 1 plus the register its first member was given;
	// gw is, by root, its heaviest member's weight. The unions done, these
	// take the tables only they used: gcap's, me's and mem's room, and mw,
	// which becomes gw in place.
	wnext := mem[:nm]
	for i := range gm {
		r := find(i)
		gm[r].weight = max(gm[r].weight, gm[i].weight)
		gm[i].room, gm[i].end = 0, 0
		if r == i {
			gm[i].room = gm[i].n
		}
	}
	clear(wnext)
	// waiting counts, by class and register, the groups on its wait list
	// with members still to come.
	var wait, waiting [2][64]int32
	// spoken reports whether reg is spoken for to a value of group self
	// (-1 for none), start s and end e, whose hint to it is the edge into
	// hb from its hi'th predecessor -- for hi -1, the one ha is the phi
	// hp's argument at, found only if a group asks -- or which has none
	// (hb nil): where a group waiting for it, but self, has a member to
	// come that begins in s..e, the first of its members to begin at s or
	// later.
	spoken := func(k, reg, s, e, self int, hb *ssa.Block, hi int, hp, ha *ssa.Value) bool {
		if hb == nil {
			n := waiting[k][reg]
			if self >= 0 && gm[self].end == int32(reg)+1 && gm[self].room > 0 {
				n--
			}
			return n > 0
		}
		hw := -1 // the hint's weight, once a group asks for it
		prev := -1
		for w := wait[k][reg]; w != 0; {
			c.steps++
			r := int(w) - 1
			nx := wnext[r]
			if gm[r].room == 0 {
				if prev < 0 {
					wait[k][reg] = nx
				} else {
					wnext[prev] = nx
				}
				w = nx
				continue
			}
			if r != self && int(gm[r].last) >= s && int(gm[r].start) <= e {
				if hw < 0 {
					if hi < 0 {
						hi = slices.Index(hp.Args, ha)
					}
					hw = weight(hb, hi)
				}
				ms := members(r)
				if i, _ := slices.BinarySearch(ms, uint64(s)<<32); int(gm[r].weight) > hw && i < len(ms) && int(ms[i]>>32) <= e {
					return true
				}
			}
			prev, w = r, nx
		}
		return false
	}
	// placed notes that a member of group r, of class k, was given
	// register reg, or -1 for none.
	placed := func(k, reg, r int) {
		if r < 0 {
			return
		}
		if reg >= 0 && gm[r].end == 0 && gm[r].room > 1 && !spoken(k, reg, int(gm[r].start), int(gm[r].last), r, nil, 0, nil, nil) {
			gm[r].end = int32(reg) + 1
			wnext[r], wait[k][reg] = wait[k][reg], int32(r)+1
			waiting[k][reg]++
		}
		if gm[r].room--; gm[r].room == 0 && gm[r].end != 0 {
			waiting[k][gm[r].end-1]--
		}
	}
	// hinted is the register free holds that the value of interval it, of
	// class k and group r, is to share, or -1: its group's, else a phi's or
	// an argument's (above), not spoken for.
	hinted := func(it *interval, k, r int, free []int) int {
		if r >= 0 {
			if g := gm[r].end; g != 0 {
				if i := slices.Index(free, int(g)-1); i >= 0 {
					return i
				}
			}
		}
		v := it.v
		want := func(x *ssa.Value, hb *ssa.Block, hi int) int {
			if x == nil || !c.hasLoc[x.ID] {
				return -1
			}
			if l := c.locAt(x); l.reg >= 0 && !spoken(k, l.reg, it.start, it.end, r, hb, hi, x, v) {
				if i := slices.Index(free, l.reg); i >= 0 {
					return i
				}
			}
			return -1
		}
		if v.Op == ssa.OpPhi {
			for j, a := range v.Args {
				if i := want(a, phiAt(v), j); i >= 0 {
					return i
				}
			}
		}
		if p := phiOf[v.ID]; p != nil {
			return want(p, phiAt(p), -1)
		}
		return -1
	}
	// The intervals of each class, in order, for its pass -- the general
	// registers' from ord's start, the floating-point registers' from ints
	// on -- and, by place in ord, the root of each's group (rootIn).
	ord, rootIn := c.intervalRefs(n), c.ints(n)
	at := [2]int{0, n - floats}
	for _, it := range all {
		// all is in order of start.
		k := b2i(isFloat(it.v))
		ord[at[k]], rootIn[at[k]] = it, root(it.v)
		at[k]++
	}
	c.lap(PhaseHints)
	c.used, c.usedList = [2]uint64{}, [2][]int{}
	c.locs, c.hasLoc = c.locList(nv), c.bools(nv)
	// Spill slots are reused as registers are: a spilled interval's slot is
	// free once it ends. Spilling an active interval puts all of it in the
	// slot, from its start, so a slot is taken only where its last
	// occupant ended before the interval began. The two classes' passes use
	// separate slots, since the second does not know when the first's
	// spilled values die.
	spills := 0
	// cslot is, by a spill class's root's value ID, its slot, or -1.
	cslot := c.ints(nv)
	for i := range cslot {
		cslot[i] = -1
	}
	for k, float := range [2]bool{false, true} {
		regs := c.gprs
		if float {
			regs = c.fprs
		}
		free := append(c.ints(len(regs))[:0], regs...)
		// The free slots: a list by slot, in the order freed, fnext and
		// fprev 1 plus the slot after and before it or 0, ftail 1 plus the
		// last; and the slots kept for phi groups whose members are still
		// to come (gslot, below), held, in a heap by the start of the
		// group's next member (kept, below). fend is, by slot, the end of
		// the interval that held it last; isFree whether it is in the
		// list or held. A slot is free once at most: freed when its
		// interval ends, taken to be used again. They have room for the
		// context's own slots at first, or the slots the first class
		// took, and grow, doubling, as the slots taken pass it: nothing
		// bounds a function's slots (abi.Context's SpillMore).
		room := abi.SpillSlots
		for room <= spills {
			room *= 2
		}
		fl := c.ints(5 * room)
		fnext, fprev, fend, gown, gap := fl[:room], fl[room:2*room], fl[2*room:3*room], fl[3*room:4*room], fl[4*room:]
		// dedicated marks the spill classes' slots (cslot), which no other
		// value takes.
		fb := c.bools(3 * room)
		isFree, held, dedicated := fb[:room], fb[room:2*room], fb[2*room:]
		grow := func() {
			n := 2 * len(fnext)
			fl := c.ints(5 * n)
			copy(fl, fnext)
			copy(fl[n:], fprev)
			copy(fl[2*n:], fend)
			copy(fl[3*n:], gown)
			copy(fl[4*n:], gap)
			fnext, fprev, fend, gown, gap = fl[:n], fl[n:2*n], fl[2*n:3*n], fl[3*n:4*n], fl[4*n:]
			b := c.bools(3 * n)
			copy(b, isFree)
			copy(b[n:], held)
			copy(b[2*n:], dedicated)
			isFree, held, dedicated = b[:n], b[n:2*n], b[2*n:]
		}
		ftail := 0
		// gslot is, by a phi group's root, the spill slot its spilled
		// members share, -1 for none; gown, by slot, 1 plus the root of
		// the group it was last given to, or 0. The members' intervals
		// meet none of each other's, so one slot holds each in turn, and
		// an edge's move between two spilled members -- the phi and its
		// argument -- is none (phiSchedule). A member's slot freed while
		// the group has members to come is held for them until the
		// group's last member ends (gends, a heap of each group's end,
		// shifted 32 bits, and root), when it joins the list. Another
		// value takes it only where no slot of the list may hold it, and
		// only for an interval that ends before the group's next member
		// begins, which gap holds by slot (lends): the group keeps it.
		// The held slots are found by gap, the greatest first (kept, a
		// heap of gaps shifted 32 bits and slots, an entry left behind
		// when its slot is taken or its gap changes): where the greatest
		// gap is too short, none is long enough. A member of another
		// group takes none, as its own group is to keep the slot it
		// takes. Given only a register's coalescing,
		// a large function's spilled phis were each a slot of its own,
		// and its edges copied them slot to slot -- Octane zlib's largest
		// asm.js function, 2.4 of its 3 MB of code edge moves. Given to
		// any other value where the first list had none, a group's slot
		// was lost where most values spilled, and zlib's edges moved
		// nearly as much as without the sharing; lent to members of
		// other groups too, it went to groups that could not keep it,
		// and they moved a sixth more than with none lent to them. Found
		// by a walk of every held slot, each looked at against its
		// group's members, the slots had taken Octane zlib's compiles
		// ten times as long to allocate, 256 ms a run against 24.
		gslot := c.ints(len(gm))
		for i := range gslot {
			gslot[i] = -1
		}
		gends := c.words(len(gm))[:0]
		gpush := func(e uint64) {
			gends = append(gends, e)
			for i := len(gends) - 1; i > 0; {
				up := (i - 1) / 2
				if gends[i] >= gends[up] {
					break
				}
				gends[i], gends[up] = gends[up], gends[i]
				i = up
			}
		}
		gpop := func() uint64 {
			e := gends[0]
			last := len(gends) - 1
			gends[0] = gends[last]
			gends = gends[:last]
			for i := 0; ; {
				m, l, r := i, 2*i+1, 2*i+2
				if l < len(gends) && gends[l] < gends[m] {
					m = l
				}
				if r < len(gends) && gends[r] < gends[m] {
					m = r
				}
				if m == i {
					break
				}
				gends[i], gends[m] = gends[m], gends[i]
				i = m
			}
			return e
		}
		kept := c.words(len(gm))[:0]
		kpush := func(e uint64) {
			kept = append(kept, e)
			for i := len(kept) - 1; i > 0; {
				up := (i - 1) / 2
				if kept[i] <= kept[up] {
					break
				}
				kept[i], kept[up] = kept[up], kept[i]
				i = up
			}
		}
		kpop := func() {
			last := len(kept) - 1
			kept[0] = kept[last]
			kept = kept[:last]
			for i := 0; ; {
				m, l, r := i, 2*i+1, 2*i+2
				if l < len(kept) && kept[l] > kept[m] {
					m = l
				}
				if r < len(kept) && kept[r] > kept[m] {
					m = r
				}
				if m == i {
					break
				}
				kept[i], kept[m] = kept[m], kept[i]
				i = m
			}
		}
		// freeSlot frees slot s, its last interval ended at end: held for
		// its group till the start of the group's next member, next, or
		// into the list for -1.
		freeSlot := func(s, end, next int) {
			fend[s], isFree[s], held[s] = end, true, next >= 0
			if next >= 0 {
				gap[s] = next
				kpush(uint64(next)<<32 | uint64(s))
				return
			}
			fprev[s], fnext[s] = ftail, 0
			if ftail != 0 {
				fnext[ftail-1] = s + 1
			}
			ftail = s + 1
		}
		takeSlot := func(s int) int {
			isFree[s] = false
			if held[s] {
				// Its entry in kept is left behind.
				held[s] = false
				return s
			}
			p, n := fprev[s], fnext[s]
			if p != 0 {
				fnext[p-1] = n
			}
			if n != 0 {
				fprev[n-1] = p
			} else {
				ftail = p
			}
			return s
		}
		// lends reports whether slot s, free, may hold an interval that
		// ends at end: not if it is held for a group (gslot) whose next
		// member begins by then, one the slot is to hold, spilled.
		lends := func(s, end int) bool {
			g := gown[s] - 1
			return !held[s] || g < 0 || gslot[g] != s || end < gap[s]
		}
		// Neither list outgrows the intervals. The spilled intervals, in
		// the order spilled, are found by when they end through a heap of
		// their ends, each shifted 32 bits, and indexes (soonest, then
		// first spilled, first); those that end before an interval starts
		// free their slots in the order spilled. The active intervals'
		// ends, registers and costs in a slot are beside them (aEnd, aReg,
		// aCost), and budgets counts the budget loops' registers among
		// them.
		cls, roots := ord[:n-floats], rootIn[:n-floats]
		if float {
			cls, roots = ord[n-floats:], rootIn[n-floats:]
		}
		active, spilled := c.intervalRefs(len(cls))[:0], c.intervalRefs(len(cls))[:0]
		aEnd, aReg, done := c.ints(len(cls))[:0], c.ints(len(cls))[:0], c.ints(len(cls))[:0]
		aCost := c.ints(len(cls))[:0]
		ends := c.words(len(cls))[:0]
		budgets := 0
		nf := c.f.NumValues()
		push := func(k int) {
			ends = append(ends, uint64(spilled[k].end)<<32|uint64(k))
			for i := len(ends) - 1; i > 0; {
				up := (i - 1) / 2
				if ends[i] >= ends[up] {
					break
				}
				ends[i], ends[up] = ends[up], ends[i]
				i = up
			}
		}
		pop := func() int {
			k := int(uint32(ends[0]))
			last := len(ends) - 1
			ends[0] = ends[last]
			ends = ends[:last]
			for i := 0; ; {
				m, l, r := i, 2*i+1, 2*i+2
				if l < len(ends) && ends[l] < ends[m] {
					m = l
				}
				if r < len(ends) && ends[r] < ends[m] {
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
		// slot is the spill slot of v, spilled from from: its phi group's
		// (gslot), else one that becomes the group's.
		var slotOf func(from int, v *ssa.Value) (int, error)
		slot := func(from int, v *ssa.Value) (int, error) {
			if v.ID < len(sclass) && sclass[v.ID] != 0 {
				// Its spill class's (spillClasses): the class's own, made
				// for it, which no other value takes.
				k := int(sclass[v.ID]) - 1
				if cslot[k] < 0 {
					if spills == len(fnext) {
						grow()
					}
					spills++
					cslot[k] = spills - 1
					dedicated[cslot[k]] = true
				}
				return cslot[k], nil
			}
			r := root(v)
			if r >= 0 && gslot[r] >= 0 {
				if s := gslot[r]; isFree[s] && fend[s] < from {
					return takeSlot(s), nil
				}
			}
			s, err := slotOf(from, v)
			if err == nil && r >= 0 {
				gslot[r], gown[s] = s, r+1
				gpush(uint64(gm[r].last)<<32 | uint64(r))
			}
			return s, err
		}
		slotOf = func(from int, v *ssa.Value) (int, error) {
			// The slot of a phi or an argument of it v is to share, if
			// free (hinted).
			end := iv[v.ID].end
			member := root(v) >= 0
			may := func(s int) bool {
				if member {
					// A member borrows no other group's: its own group
					// is to keep the slot it takes.
					return lends(s, math.MaxInt)
				}
				return lends(s, end)
			}
			related := func(x *ssa.Value) int {
				if x == nil || !c.hasLoc[x.ID] {
					return -1
				}
				if l := c.locAt(x); l.reg < 0 && l.spill >= 0 && isFree[l.spill] && fend[l.spill] < from && may(l.spill) {
					return l.spill
				}
				return -1
			}
			s := related(phiOf[v.ID])
			if v.Op == ssa.OpPhi {
				for _, a := range v.Args {
					if s < 0 {
						s = related(a)
					}
				}
			}
			if s >= 0 {
				return takeSlot(s), nil
			}
			for s := ftail; s != 0; s = fprev[s-1] {
				c.steps++
				if fend[s-1] < from {
					return takeSlot(s - 1), nil
				}
			}
			// A held slot, the one of the greatest gap, past the entries
			// of slots taken, regapped, or whose group gave them up (into
			// the list, where the walk above did not find them).
			for !member && len(kept) > 0 {
				c.steps++
				e := kept[0]
				s := int(uint32(e))
				if !isFree[s] || !held[s] || gap[s] != int(e>>32) {
					kpop()
					continue
				}
				if g := gown[s] - 1; g < 0 || gslot[g] != s {
					kpop()
					fe := fend[s]
					takeSlot(s)
					freeSlot(s, fe, -1)
					continue
				}
				if end < gap[s] && fend[s] < from {
					kpop()
					return takeSlot(s), nil
				}
				break
			}
			if spills == len(fnext) {
				grow()
			}
			spills++
			return spills - 1, nil
		}
		// inSlot is what x costs in a slot (useRec's cost), less, where a
		// phi it is an argument of, or an argument of it, is in a slot x
		// may share (slot's related), twice their edge's weight: the
		// edge's move, counted in cost as a slot's load or store, is none
		// if x shares the slot, and one if x stays in a register. What x
		// may share ended before x began, its place settled, so inSlot is
		// found once for each interval, as it begins. Spilling by cost
		// alone, Octane zlib's code read and wrote slots at its edges 5%
		// more often than spilling by ends had (counted in its code), and
		// ran 1.6% slower; so, 7% less often.
		inSlot := func(x *ssa.Value) int {
			// spilled reports whether y is in a slot that x may share: y's
			// interval ends before x's begins.
			spilled := func(y *ssa.Value) bool {
				if !c.hasLoc[y.ID] {
					return false
				}
				l := c.locAt(y)
				return l.reg < 0 && l.spill >= 0 && iv[y.ID].end < iv[x.ID].start
			}
			w := int(rec[x.ID].cost)
			if x.Op == ssa.OpPhi && x.ID < nf {
				for i, a := range x.Args {
					if a != x && inOrder[x.Block.Preds[i].ID] && spilled(a) {
						w -= 2 * weight(x.Block, i)
					}
				}
			} else if p := phiOf[x.ID]; p != nil && spilled(p) {
				for i, a := range p.Args {
					if a == x && inOrder[p.Block.Preds[i].ID] {
						w -= 2 * weight(p.Block, i)
					}
				}
			}
			return w
		}
		// splitCost is what splitting x at position at costs (below): its
		// slot's store where it is made, weighed as its block; and whether
		// it may be split there -- a value that is no phi, nor a call's
		// result or a keep's, made in a register by its own operation,
		// whose uses but frame states' are all in its block and before at
		// (useRec's real and slotFar).
		splitCost := func(x *ssa.Value, at int) (int, bool) {
			r := &rec[x.ID]
			if x.ID >= nf || x.Op == ssa.OpPhi || r.slot&(slotMade|slotFar) != 0 || int(r.real) >= at || c.isFused(x) {
				return 0, false
			}
			switch x.Op {
			case ssa.OpCall, ssa.OpMathCall, ssa.OpKeep, ssa.OpKept, ssa.OpBigBox, ssa.OpCallCell, ssa.OpBigCell:
				return 0, false
			}
			return edgeWeights[min(bits.OnesCount64(masks[r.def-1]), 3)], true
		}
		// minEnd is at most the soonest an active interval ends: none
		// ends before an interval that starts no later, and the active
		// are looked at only once one may.
		minEnd := math.MaxInt
		for j, it := range cls {
			if minEnd < it.start {
				kept := 0
				minEnd = math.MaxInt
				c.steps += len(active)
				for i, a := range active {
					if e := aEnd[i]; e < it.start {
						free = append(free, aReg[i])
						if a.v.ID >= nf {
							budgets--
						}
					} else {
						active[kept], aEnd[kept], aReg[kept], aCost[kept] = a, e, aReg[i], aCost[i]
						kept++
						minEnd = min(minEnd, e)
					}
				}
				active, aEnd, aReg, aCost = active[:kept], aEnd[:kept], aReg[:kept], aCost[:kept]
			}
			done = done[:0]
			for len(ends) > 0 && int(ends[0]>>32) < it.start {
				k := pop()
				// In the order spilled: insertion, as few end at once.
				done = append(done, k)
				for i := len(done) - 1; i > 0 && done[i-1] > done[i]; i-- {
					c.steps++
					done[i-1], done[i] = done[i], done[i-1]
				}
			}
			for _, k := range done {
				a := spilled[k]
				s := c.locs[a.v.ID].spill
				if s < len(dedicated) && dedicated[s] {
					// A spill class's, which its values keep.
					continue
				}
				// A group's, held for its members to come: till the next
				// to begin.
				next := -1
				if g := gown[s] - 1; g >= 0 && gslot[g] == s && int(gm[g].last) > a.end {
					ms := members(g)
					if i, _ := slices.BinarySearch(ms, uint64(a.end)<<32|math.MaxUint32); i < len(ms) {
						next = int(ms[i] >> 32)
					}
				}
				freeSlot(s, a.end, next)
			}
			for len(gends) > 0 && int(gends[0]>>32) < it.start {
				// The group ended: its slot, if still kept, is any value's.
				g := int(uint32(gpop()))
				if s := gslot[g]; s >= 0 && isFree[s] && held[s] && gown[s] == g+1 {
					e := fend[s]
					takeSlot(s)
					freeSlot(s, e, -1)
				}
			}
			r := roots[j]
			if it.v.ID < nf && memOperand(it.v) {
				// Only ever a memory operand: in its slot, with no register
				// -- its phi's and its argument's the same, their spill
				// class's, so that no edge moves it. Its accesses read it
				// where it is, as cheaply as from a register, and the
				// registers go to what is computed: given one, a heap's
				// base and length had cost a small loop its budget's
				// register, counted in memory at every back edge (asm_adler
				// 12% slower than with the heap read from its cell at each
				// access).
				s, err := slot(it.start, it.v)
				if err != nil {
					return err
				}
				c.setLoc(it.v, loc{reg: -1, spill: s})
				placed(k, -1, r)
				spilled = append(spilled, it)
				push(len(spilled) - 1)
				continue
			}
			if takesOperand(it.v) {
				// An operand whose interval ends here gives its register
				// to the result: the operation reads its operands before
				// it writes it (takesOperand). An int32 phi's sum had
				// needed a register of its own, beside the phi's, which
				// its back edge moved it into.
				kept := 0
				c.steps += len(active)
				for i, a := range active {
					if aEnd[i] == it.start && a.v.ID < nf && reads(it.v, a.v) {
						free = append(free, aReg[i])
						continue
					}
					active[kept], aEnd[kept], aReg[kept], aCost[kept] = a, aEnd[i], aReg[i], aCost[i]
					kept++
				}
				active, aEnd, aReg, aCost = active[:kept], aEnd[:kept], aReg[:kept], aCost[:kept]
			}
			if len(free) > 0 {
				i := hinted(it, k, r, free)
				if i < 0 {
					// The last freed, but one spoken for (spoken, for a
					// value with no hint): waited for by a group but its
					// own, whose register it is.
					i = len(free) - 1
					for j := len(free) - 1; j >= 0; j-- {
						reg := free[j]
						n := waiting[k][reg]
						if r >= 0 && gm[r].end == int32(reg)+1 && gm[r].room > 0 {
							n--
						}
						if n <= 0 {
							i = j
							break
						}
					}
				}
				reg := free[i]
				c.locs[it.v.ID], c.hasLoc[it.v.ID] = loc{reg: reg, spill: -1}, true
				c.used[k] |= 1 << reg
				placed(k, reg, r)
				active, aEnd, aReg = append(active, it), append(aEnd, it.end), append(aReg, reg)
				aCost = append(aCost, inSlot(it.v))
				if it.v.ID >= nf {
					budgets++
				}
				free = append(free[:i], free[i+1:]...)
				minEnd = min(minEnd, it.end)
				continue
			}
			// Spill the interval whose slot costs least for the register it
			// gives up -- but a budget loop's register first, whose loop
			// then counts in memory: the register costs no value of the
			// loop a spill. What an interval costs in a slot is its uses'
			// and definition's weight (useRec's cost, inSlot), what it
			// gives up is the rest of it, from here to its end, each
			// position weighed as its block (run); the one of least cost
			// for what it frees is spilled, and of those alike, the one
			// ending last, as LLVM's linear scan weighed intervals, by their
			// uses' frequency over their length. Spilling whichever ended
			// last, as a linear scan of ends alone does, a value read every
			// turn of an inner loop had gone to a slot, stored and loaded
			// back each turn, beside values only exits and the code after
			// the loop read: in AWFY List's tail, with isShorterThan's loop
			// inlined, the second property read's cell address, which
			// ended at the loop's back edge with the caller's values that
			// only the loop's exits read, and lost the tie.
			//
			// An active interval whose uses but frame states' are all
			// behind -- each in its own block, no call between that saves
			// the registers -- is split rather than spilled, as a linear
			// scan that splits an interval at a use (Wimmer's) or gives it
			// a second chance does: it keeps its register from its
			// definition to its last such use, and its slot, written as it
			// is made, holds it for the frame states, which alone read it
			// after (exitView). It costs the slot's store, not its uses'
			// loads. Spilled whole, a value made in a loop's turn, read by
			// the next instruction and then by the turn's frame states
			// alone, long and cheap, had been stored and loaded straight
			// back each turn: in a BigInt's hash (microbench's
			// bigint64_fnv1a), the loop's read of the BigInt global.
			victim, split := it, false
			vi := -1
			// The ratios are compared as products, in floating point, which
			// no function's weights overflow, each rounded (not fused).
			itCost := inSlot(it.v)
			vc, vr := float64(itCost), float64(run[it.end+1]-run[it.start])
			for i, e := range aEnd {
				ac, ar := float64(aCost[i]), float64(run[e+1]-run[it.start])
				// Split or spilled, whichever costs less: spilled, a phi's
				// argument may share its phi's slot (inSlot).
				w, sp := splitCost(active[i].v, it.start)
				if sp = sp && w < aCost[i]; sp {
					ac = float64(w)
				}
				// Of those alike, a split first, whose register goes with
				// nothing loaded back -- no use but frame states' is
				// ahead of it -- then the one ending last: ending last,
				// a value the turn still read twice had been spilled
				// whole beside one only frame states read, at the same
				// cost for the length each gave up (a BigInt's
				// xorshift, register code's, its number unboxed).
				if d := float64(ac*vr) - float64(vc*ar); d < 0 || d == 0 && (sp && !split || sp == split && e > victim.end) {
					victim, vi, vc, vr, split = active[i], i, ac, ar, sp
				}
			}
			if it.v.ID >= nf {
				victim, vi, split = it, -1, false
			}
			if budgets > 0 {
				c.steps += len(active)
				for i, a := range active {
					if a.v.ID >= nf {
						victim, vi, split = a, i, false
					}
				}
			}
			budget := victim.v.ID >= nf
			s := -1
			if !budget {
				// A budget's register given up takes no spill slot, which
				// another value's phi or argument may be hinted to.
				var err error
				if s, err = slot(victim.start, victim.v); err != nil {
					return err
				}
			}
			reg := -1
			if victim == it {
				c.setLoc(it.v, loc{reg: -1, spill: s})
			} else {
				reg = c.locAt(victim.v).reg
				c.setLoc(it.v, loc{reg: reg, spill: -1})
				if split {
					c.setLoc(victim.v, loc{reg: reg, spill: s})
				} else {
					c.setLoc(victim.v, loc{reg: -1, spill: s})
				}
				active[vi], aEnd[vi], aCost[vi] = it, it.end, itCost
				if budget {
					budgets--
				}
				if it.v.ID >= nf {
					budgets++
				}
				minEnd = min(minEnd, it.end)
			}
			placed(k, reg, r)
			if !budget {
				spilled = append(spilled, victim)
				push(len(spilled) - 1)
			}
		}
	}
	c.lap(PhaseScan)
	scanSpills := spills

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
	{
		for _, v := range kv {
			if !savingCall(v) {
				// A call, native or Go's; or a Math function's call of
				// Go, which saves what lives across it too (mir's
				// mathCall).
				continue
			}
			if v.Op == ssa.OpCall && iv[v.ID].v != nil && iv[v.ID].end == pos[v.ID] {
				// A native call's result nothing uses -- no value, state or
				// control, nor its keep cell's address (OpCallCell) -- is
				// stored nowhere (dropped).
				if c.dropped == nil {
					c.dropped = c.bools(nv)
				}
				c.dropped[v.ID] = true
			}
			if home == nil {
				home, c.saveAt = c.ints(nv), c.ints(2*nv)
			}
			c.saveAt[2*v.ID] = len(c.saves)
			p := pos[v.ID]
			for ; next < len(all) && all[next].start < p; next++ {
				// Only what is in a register is saved; nor is a budget
				// loop's, whose loop calls nothing.
				// Nor a split value's, which no such call follows before
				// its last use but a frame state's (useRec's slotFar).
				if it := all[next]; c.locs[it.v.ID].reg >= 0 && c.locs[it.v.ID].spill < 0 && it.v.ID < c.f.NumValues() {
					open = append(open, it)
				}
			}
			kept := open[:0]
			c.steps += len(open)
			for _, it := range open {
				if it.end > p {
					kept = append(kept, it)
				} else if s := home[it.v.ID]; s != 0 {
					free = append(free, s-1)
					home[it.v.ID] = 0
				}
			}
			open = kept
			c.steps += len(open)
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
					default:
						s, spills = spills, spills+1
					}
					home[it.v.ID] = s + 1
				}
				sv := saved{reg: l.reg, slot: s, float: isFloat(it.v), id: it.v.ID}
				if o, ok := c.frameResident(it.v, v); ok {
					sv.frame = o + 1
				}
				if r := &rec[it.v.ID]; len(v.Calls()) != 0 && v.Calls()[0].Go != 0 && it.v.Op != ssa.OpPhi && r.def == int32(v.Block.ID)+1 && it.end == p+1 && int(r.hot) <= p {
					// A call of Go's, defined in the call's block before
					// it and read after it only by its state, for its
					// refusal: restored there alone (saved's cold), the
					// code that goes on after the call not reading it
					// before the block's next pass makes it again.
					sv.cold = true
				}
				c.saves = c.appendSaved(c.saves, sv)
			}
			c.saveAt[2*v.ID+1] = len(c.saves)
		}
	}
	c.spills = spills
	if c.opts.CheckAllocations {
		c.checkAllocation(iv, rec, scanSpills)
	}
	err := c.colorKeeps(iv, pos, kv)
	c.keepKeepList(kv)
	return err
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
func (c *core) colorKeeps(iv []interval, pos []int, kv []*ssa.Value) error {
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
	writes := (*ssa.Value).WritesKeepCell
	// cell is, by value ID, 1 plus the cell an address is of.
	cell := c.ints(f.NumValues())
	var phis []*ssa.Value
	{
		for _, v := range kv {
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
	for _, v := range kv {
		if v.Op != ssa.OpBigBox || !c.isLazy(v) {
			continue
		}
		// The intervals are by value ID.
		if it := &iv[v.Args[0].ID]; it.v != nil {
			extend(v.Index, it.start, it.end)
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
	// Each cell taken is the first free: the cells live are a heap of
	// their ends, shifted 32 bits, and numbers, those that end before a
	// keep begins freed (free, a set of cells) as it is met.
	phys := c.ints(nk)
	live := c.words(min(nk, abi.MaxKeeps))[:0]
	var free [(abi.MaxKeeps + 63) / 64]uint64
	n := 0
	for _, k := range order {
		for len(live) > 0 && int(live[0]>>32) < lo[k] {
			cell := uint32(live[0])
			free[cell/64] |= 1 << (cell % 64)
			last := len(live) - 1
			live[0], live = live[last], live[:last]
			for i := 0; ; {
				m, l, r := i, 2*i+1, 2*i+2
				if l < len(live) && live[l] < live[m] {
					m = l
				}
				if r < len(live) && live[r] < live[m] {
					m = r
				}
				if m == i {
					break
				}
				live[i], live[m] = live[m], live[i]
				i = m
			}
		}
		at := -1
		for w, word := range free {
			if word != 0 {
				at = w*64 + bits.TrailingZeros64(word)
				free[w] &^= 1 << (at % 64)
				break
			}
		}
		if at < 0 {
			if n == abi.MaxKeeps {
				return ErrKeeps
			}
			at, n = n, n+1
		}
		phys[k] = at
		live = append(live, uint64(hi[k])<<32|uint64(at))
		for i := len(live) - 1; i > 0; {
			up := (i - 1) / 2
			if live[i] >= live[up] {
				break
			}
			live[i], live[up] = live[up], live[i]
			i = up
		}
	}
	for _, v := range kv {
		if writes(v) {
			v.Index = phys[v.Index]
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
	for i := ops; i < s.Own()-s.Args; i++ {
		if x := s.Slot(i); x == nil || x.Op == ssa.OpLoadSlot && c.isLazy(x) {
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
	case ssa.OpPropCell, ssa.OpHolderCell, ssa.OpElemCell, ssa.OpGlobalCell, ssa.OpLexCell, ssa.OpBindingCell, ssa.OpStringMethod, ssa.OpStringChar, ssa.OpCodeChar, ssa.OpCodeCharOr, ssa.OpTypeofCell, ssa.OpSubstringChar, ssa.OpStringAtOr, ssa.OpStringProp, ssa.OpCallCell, ssa.OpBigCell, ssa.OpConstCell, ssa.OpUpvalueCell, ssa.OpLocalCell, ssa.OpKeep,
		ssa.OpCursorCell:
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
	case ir.Word:
		// A word written as it is: an object the code does not make, its
		// shape, its prototype (abi.VirtualBase).
		return k.Bits
	}
	panic(fmt.Sprintf("tagged constant of kind %d", k.Kind))
}

// maxConstString is the longest string constant, in bytes, a strict
// equality compares a string with as immediates (strConstOf).
const maxConstString = 64

// eqConstOf is v's operands, an OpEqTagged's, where one is a constant
// whose strict equality is its word's -- a number, a boolean, null or
// undefined -- the other and the constant; false for any other.
func eqConstOf(v *ssa.Value) (*ssa.Value, ir.Value, bool) {
	x, y := v.Args[0], v.Args[1]
	if x.Op == ssa.OpConst {
		x, y = y, x
	}
	if y.Op != ssa.OpConst || x.Op == ssa.OpConst {
		return nil, ir.Value{}, false
	}
	switch y.Const.Kind {
	case ir.Number, ir.Boolean, ir.Null, ir.Undefined:
		return x, y.Const, true
	}
	return nil, ir.Value{}, false
}

// strConstOf is v's operands, an OpEqTagged's, where one is a string
// constant's cell loaded whose string is known (ssa.Value's ConstString),
// ASCII and of no more than maxConstString bytes: the other and the
// string; false for any other.
func strConstOf(v *ssa.Value) (*ssa.Value, string, bool) {
	for i, y := range v.Args[:2] {
		if y.Op != ssa.OpLoadCell || len(y.Args) != 1 {
			continue
		}
		k, ok := y.Args[0].ConstString()
		if !ok || len(k) > maxConstString {
			continue
		}
		ascii := true
		for j := 0; j < len(k); j++ {
			ascii = ascii && k[j] < 0x80
		}
		if ascii {
			return v.Args[1-i], k, true
		}
	}
	return nil, "", false
}

// strBytes compares n bytes as chunk compares a chunk of size bytes at at
// -- 8, 4, 2 or 1 -- in as few as cover them: words, the last overlapping
// the one before where n is not a multiple of 8; below a word, two halves
// that may overlap.
func strBytes(n int, chunk func(at, size int)) {
	switch {
	case n >= 8:
		for at := 0; at+8 <= n; at += 8 {
			chunk(at, 8)
		}
		if n%8 != 0 {
			chunk(n-8, 8)
		}
	case n >= 4:
		chunk(0, 4)
		if n > 4 {
			chunk(n-4, 4)
		}
	case n >= 2:
		chunk(0, 2)
		if n > 2 {
			chunk(n-1, 1)
		}
	case n == 1:
		chunk(0, 1)
	}
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
	// Past the locals, a register program's scratch slots (ssa.Func's
	// Scratch), where the operands, the loops' slots and the homes start.
	at := c.f.FrameLocals + c.f.Scratch
	n := at + c.f.StackSize
	if c.f.Loops {
		n = max(n, at+c.f.LoopsEnd-c.f.Locals)
	}
	if c.f.HomesEnd != 0 {
		// The homes of the objects it does not make, which its exits write
		// there (ssa's escape.go).
		n = max(n, at+c.f.HomesEnd-c.f.Locals)
	}
	return n
}

// callsAgain reports a call that may read its operands, and its state's
// values, after Go has had the registers: a call of Go, which Go may
// refuse, the call exiting after it; a push of several values, whose
// growth is push's leaf, which may refuse it too (pushGrow); or a call
// whose pool Go fills, made again from its start (ssa.RefillsPools). What
// it reads lives across it, saved and restored (allocate's saves).
func callsAgain(v *ssa.Value) bool {
	if v.Op != ssa.OpCall || len(v.Calls()) == 0 {
		return false
	}
	c := v.Calls()[0]
	return c.Go != 0 || c.Push && c.Argc > 1 || ssa.RefillsPools(v)
}

// savingCall reports a call that saves the registers live across it, and
// restores them after (allocate's saves): a native call, or a Math
// function's call of Go.
func savingCall(v *ssa.Value) bool {
	return v.Op == ssa.OpCall && v.Calls() != nil || v.Op == ssa.OpMathCall
}

// remat reports a value an exit recomputes from its operands rather than
// reads from its location. A poll exits on a back-edge, after the phi moves
// and before the header runs again, so a box the header computed of a phi
// would still hold the last iteration's value; its operand, the phi, holds
// this one's.
// i32Boxed is the int32 a box of its conversion boxes, or nil: what an
// exit, and a box made again in code (materialize), make it of -- a loop
// header's state names it before the conversion is made (ssa's intPhis),
// and one only boxes use is made by none (findLazy).
func i32Boxed(v *ssa.Value) *ssa.Value {
	if v.Op == ssa.OpBoxF64 && v.Args[0].Op == ssa.OpI32ToF64 {
		return v.Args[0].Args[0]
	}
	return nil
}

// memOperand reports whether v is only ever read as a memory operand: a
// typed code's heap's base or length, or a phi of them, which the heap's
// accesses compare and add where they are (amd64's heapAccess). Spilled,
// it costs its accesses no load of their own.
func memOperand(v *ssa.Value) bool {
	switch v.Op {
	case ssa.OpHeapBase, ssa.OpHeapLen:
		return true
	case ssa.OpPhi:
		return v.Type == ssa.Ptr
	}
	return false
}

// exactF64 reports whether x, a float64, is an integer's conversion, which
// is never NaN: a box of it needs no NaN made canonical.
func exactF64(x *ssa.Value) bool {
	switch x.Op {
	case ssa.OpI32ToF64, ssa.OpU32ToF64, ssa.OpBoolToF64:
		return true
	}
	return false
}

// takesOperand reports an operation whose result may take the register of
// an operand whose interval ends at it (allocate's scan): each backend's
// code for it reads its operands -- into scratch registers, or in its one
// instruction -- before it writes its result.
func takesOperand(v *ssa.Value) bool {
	switch v.Op {
	case ssa.OpAddI32, ssa.OpSubI32, ssa.OpAndI32, ssa.OpOrI32, ssa.OpXorI32, ssa.OpShlI32, ssa.OpSarI32, ssa.OpShrU32, ssa.OpMulI32, ssa.OpNotI32:
		return true
	}
	return false
}

// reads reports whether a is an operand of v.
func reads(v, a *ssa.Value) bool { return slices.Contains(v.Args, a) }

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
			if r, sl, split := c.split(v); split {
				l = loc{reg: r, spill: sl}
			}
			switch {
			case c.isLazy(v):
				out += fmt.Sprintf("%v:lazy ", v)
			case !ok:
			case l.reg >= 0 && l.spill >= 0:
				out += fmt.Sprintf("%v:%s%d+spill%d ", v, map[bool]string{false: "r", true: "x"}[isFloat(v)], l.reg, l.spill)
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

// useRec is what allocate's uses read and write of a value: end, its last
// use's position; def, 1 plus the ID of the block defining it, or -1 for a
// value that needs no location, 0 for one not in the function; xhead, 1
// plus the index in xuses of its last use in another block, or 0; mid, 1
// plus its number among the phis' groups' members, or 0 (allocate's);
// slot, what a use of it uses besides or instead (slotMade, slotShadow).
//
// hot is the last use's position but a call of Go's state's, which only its
// refusal reads (saved's cold).
//
// cost is what keeping the value in a spill slot costs (allocate's spill
// choice): its definition and each use that reads it -- an operand, a
// branch's control, a phi's argument at its edge, not a frame state, which
// only an exit reads -- each weighed by how often it runs, 16 to the power
// of the loops holding it (edgeWeights), at most math.MaxInt32.
//
// real is the last use's position but a frame state's: where the value's
// register may end if its slot is written as it is made (allocate's split).
type useRec struct {
	end, def, xhead, mid, hot, cost, real, start int32
	slot                                         uint8
}

// useRec's slot: a value made where needed or a BigInt's box, whose state
// slots use what it is made of; one with a shadow.
const (
	slotMade = 1 << iota
	slotShadow
	// slotFloat is no use's but the value's class: a floating-point
	// value's.
	slotFloat
	// slotFar marks a value that cannot be split (allocate's split): one
	// that a use but a frame state's reads in another block than its own,
	// or past a call after it that saves the registers (savingCall), or as
	// an operand of a call that may read it again (callsAgain); or that
	// its own state reads.
	slotFar
)

// gmember is a member of allocate's phis' groups: of the member, group, 1
// plus its group's root or 0 for a root, end, its interval's, and weight,
// its heaviest edge's; of a root, start and last, the span of its members'
// intervals, and n of its members in the groups' pool from off, with
// room for room. Once the groups are made, a root's room counts its
// members not yet placed, end is 1 plus the register its first was
// given, or 0, and weight the heaviest of its members'.
type gmember struct {
	group, end, weight, start, last, off, n, room int32
}

// spillClasses are the classes of a phi and its arguments that may share
// one spill slot, by value ID: 1 plus the root's value ID of the class of
// each value in one of more than one value, else 0. The groups (allocate)
// join a phi and an argument only where no intervals of theirs meet, as
// their register is to be one; a slot asks less: that no two of a class
// are live at once, as V8's allocator merges the spill ranges of a phi and
// its inputs where their uses' intervals, holes and all, do not meet. An
// interval spans every position from a value's first to its last; a
// large function's values of one variable meet each other's there --
// one live on a loop's exit, another on the path past the loop -- and its
// phis had each been given a slot of its own, every edge a move between
// two slots: Octane zlib's largest function, 70% of its code.
//
// Two values of a strict SSA function are live at once only if one's
// definition dominates the other's and it is live there (Budimlić et
// al.), and a class is joined to another only where no value of either is
// live at the definition of one of the other's that it dominates: with
// each class's values in the order of the dominator tree's walk, the two
// lists merged and walked with a stack of the values that dominate the
// one met, each is checked against the nearest of the other class's on
// the stack alone -- one further up live where it is would be live at
// that nearest one's definition too, which its class proved not
// (Boissinot et al.'s check of two classes). A value is live at a
// position of block b where it is live into b or made in b before it,
// and live out of b or used in b after it (xuse's at); at a phi's, b's
// start, where it is live into b, the other phis of b each live there.
// The pairs are pairs of members (allocate's mem), a phi's and an
// argument's of the same class of register.
func (c *core) spillClasses(rec []useRec, iv []interval, pos, start []int, xuses []xuse, liveIn, liveOut []ir.LiveSet, lb *ir.LiveBuilder, mem, pairs []int32) []int32 {
	nv := len(rec)
	class := c.int32s(nv)
	if len(pairs) == 0 {
		return class
	}
	pre, post := c.f.DomOrder()
	block := func(id int32) int { return int(rec[id].def) - 1 }
	isPhi := func(id int32) bool { return iv[id].v.Op == ssa.OpPhi }
	// at is a value's definition's position: a phi's, its block's start.
	at := func(id int32) int {
		if isPhi(id) {
			return start[block(id)]
		}
		return pos[id]
	}
	// key orders values as the dominator tree's walk meets their
	// definitions, a block's phis first.
	key := func(id int32) uint64 {
		return uint64(pre[block(id)])<<32 | uint64(at(id))
	}
	dominates := func(a, b int32) bool {
		ba, bb := block(a), block(b)
		if ba == bb {
			return at(a) <= at(b)
		}
		return pre[ba] <= pre[bb] && post[bb] <= post[ba]
	}
	cross := func(id int32) bool { return rec[id].xhead != 0 && lb != nil }
	// liveAt reports whether a, whose definition dominates b's, is live
	// where b is made.
	liveAt := func(a, b int32) bool {
		bb := block(b)
		if isPhi(b) {
			if block(a) == bb {
				return true
			}
			return cross(a) && lb.Has(liveIn[bb], int(a))
		}
		p := at(b)
		if !(block(a) == bb || cross(a) && lb.Has(liveIn[bb], int(a))) {
			return false
		}
		if cross(a) && lb.Has(liveOut[bb], int(a)) {
			return true
		}
		if block(a) == bb {
			// Used in its own block alone: its interval's end.
			return int(rec[a].end) > p
		}
		for u := rec[a].xhead; u != 0; u = xuses[u-1].next {
			c.steps++
			if xuses[u-1].b.ID == bb {
				return int(xuses[u-1].at) > p
			}
		}
		return false
	}
	// end is the first key past the definitions v dominates: those of the
	// blocks below v's in the tree, whose places in the walk's pre-order
	// lie before its block's in the post-order, the walks sharing one
	// clock, and those after v in its block.
	end := func(v int32) uint64 { return uint64(post[block(v)]) << 32 }
	// The classes' values in order of key: a root's list, n of them from
	// off in pool -- room for room -- or, for n 0, the root alone; and
	// parent, by value ID, the nearest of its class's values whose
	// definition dominates its own, or -1: the class's dominance forest.
	// Their tables are the workspace's, the pool grown where full.
	off, n, room, parent := c.int32s(nv), c.int32s(nv), c.int32s(nv), c.int32s(nv)
	for i := range parent {
		parent[i] = -1
	}
	pool := c.int32s(4*len(mem) + 64)
	used := 0
	find := func(id int32) int32 {
		if class[id] != 0 {
			return class[id] - 1
		}
		return id
	}
	// first is the index of the first of ms of key k or after.
	first := func(ms []int32, k uint64) int {
		lo, hi := 0, len(ms)
		for lo < hi {
			c.steps++
			m := int(uint(lo+hi) >> 1)
			if key(ms[m]) < k {
				lo = m + 1
			} else {
				hi = m
			}
		}
		return lo
	}
	// nearest is the nearest of ms whose definition dominates v's, which
	// is none of them, or -1.
	nearest := func(ms []int32, v int32) int32 {
		i := first(ms, key(v)) - 1
		if i < 0 {
			return -1
		}
		x := ms[i]
		for x >= 0 && !dominates(x, v) {
			c.steps++
			x = parent[x]
		}
		return x
	}
	var one [2]int32
	for p := 0; p < len(pairs); p += 2 {
		x, y := find(mem[pairs[p]]), find(mem[pairs[p+1]])
		if x == y {
			continue
		}
		if n[x] < n[y] {
			x, y = y, x
		}
		mx := pool[off[x] : off[x]+n[x]]
		if n[x] == 0 {
			one[0] = x
			mx = one[:1]
		}
		my := pool[off[y] : off[y]+n[y]]
		if n[y] == 0 {
			one[1] = y
			my = one[1:2]
		}
		// Each of y's values against the nearest of x's that dominates it,
		// and against those of x's it dominates with none of x's between
		// (a value one of those dominates is live where that one is made,
		// if v is where it is).
		clash := false
		for _, v := range my {
			if a := nearest(mx, v); a >= 0 && liveAt(a, v) {
				clash = true
				break
			}
			hi := end(v)
			for i := first(mx, key(v)); i < len(mx) && key(mx[i]) < hi && !clash; i = first(mx, end(mx[i])) {
				clash = liveAt(v, mx[i])
			}
			if clash {
				break
			}
		}
		if clash {
			continue
		}
		// x's list with room for y's, at the pool's end where it has
		// none.
		need := int(n[x]) + len(my)
		if n[x] == 0 {
			need++
		}
		if need > int(room[x]) {
			r := 2 * need
			if used+r > len(pool) {
				np := c.int32s(2*len(pool) + r)
				copy(np, pool[:used])
				pool = np
			}
			copy(pool[used:], mx)
			off[x], room[x] = int32(used), int32(r)
			used += r
			if n[x] == 0 {
				n[x] = 1
			}
		}
		ys := one[1:2]
		if n[y] != 0 {
			ys = pool[off[y] : off[y]+n[y]]
		}
		// y's values put among x's, each its nearest dominator's child
		// and the parent of those of x's it is now nearest to: in order of
		// key, so that a value of y's below another of y's is put after it.
		ms := pool[off[x] : off[x]+n[x] : off[x]+room[x]]
		for _, v := range ys {
			ms = slices.Insert(ms, first(ms, key(v)), v)
			parent[v] = nearest(ms, v)
			hi := end(v)
			for i := first(ms, key(v)) + 1; i < len(ms) && key(ms[i]) < hi; i = first(ms, end(ms[i])) {
				parent[ms[i]] = v
			}
			class[v] = x + 1
		}
		n[x], n[y] = int32(len(ms)), 0
		class[x] = x + 1
	}
	return class
}

// xuse is a use of a value in block b, another block than the value's
// (allocate), and next 1 plus the index of the value's use before it in
// another block, or 0.
type xuse struct {
	b    *ssa.Block
	next int32
	// at is the last position in b that uses the value (spillClasses).
	at int32
}

// interval is a value's live range over the linear instruction numbering.
type interval struct {
	v          *ssa.Value
	start, end int
}

func (c *core) spillDisp(slot int) int32 { return abi.OffSpill + int32(slot)*8 }

// spillMore reports whether slot is past the context's own (abi.Context's
// SpillMore), and if so its offset into SpillMore.
func spillMore(slot int) (int32, bool) {
	if slot < abi.SpillSlots {
		return 0, false
	}
	return int32(slot-abi.SpillSlots) * 8, true
}

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
	if n := 2 * (64 + max(abi.SpillSlots, c.spills)); len(c.sched.readers) < n {
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
		c.steps += len(moves)
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
			c.steps += len(moves) - i
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

// Options are the switches a compile runs with, the package's variables
// (Annotate, CheckAllocations, TableExits, Timing) as Settings reads them,
// which a host sets for the process: a host compiling on another
// goroutine reads them where it prepares the compile, so that what it
// sets meanwhile is not read as it changes. A test passes its own
// (CompileWith, CompileAMD64With, CompileARM64With) rather than set the
// package's, which every other compile in the process reads.
type Options struct {
	Annotate, CheckAllocations, TableExits bool
	// Timing is what the compile's phases are timed in (the package's
	// Timing), or nil.
	Timing *Times
}

// Settings are the switches the package's variables say now.
func Settings() Options {
	return Options{Annotate: Annotate, CheckAllocations: CheckAllocations, TableExits: TableExits, Timing: Timing}
}

// lap ends phase p of the compile, if it is timed (Options.Timing).
func (c *core) lap(p Phase) {
	if t := c.opts.Timing; t != nil {
		t.Lap(p)
		t.Steps[p] += int64(c.steps)
	}
	c.steps = 0
}

// TableExits has an exit that returns to Go leave its frame to Go to write
// from a description (abi.ExitDescriptor), as V8's deoptimizer reads a
// frame state's translation, rather than write it itself: the exit's code
// is a jump. It is a switch for tests to compare the two by.
var TableExits = true

// / exitDescriptor describes the exit to state s of the given kind, its
// frame's locals and operands at the registers localsReg and stackReg, as
// exitThen's code writes it; or nil where a value is where a description
// cannot say, and the exit's code writes the frame itself.
//
// Its slots are its state's tree's (ssa's SlotNode), each node described
// once (exitNode) and shared by the descriptions of every state that has
// it, as the states share it.
func (c *core) exitDescriptor(s *ssa.FrameState, kind uint64, localsReg, stackReg uint8) *abi.ExitDescriptor {
	defer c.viewExits()()
	f := c.exitLayout()
	d := abi.ExitDescriptor{Kind: uint32(kind), PC: uint32(s.PC), Depth: uint32(s.Depth), Site: int32(s.Site),
		Frame: f, LocalsReg: localsReg, StackReg: stackReg, Direct: true}
	root, h := s.Tree()
	if root != nil {
		if len(c.exitNodes) < c.f.NumSlotNodes() {
			c.exitNodes = c.exitNodeList(c.f.NumSlotNodes())
		}
		x := c.exitNode(root, h, 0)
		if x.fail {
			return nil
		}
		d.Root = x.off
		// Written one by one if no slot's reference may come from another
		// slot written here: by its origin, or a source that is one's
		// address -- a slot's, or one known only at run time.
		d.Direct = !x.indirect && (x.cross == 0 || !c.exitCrossWritten(s, root, h))
	}
	if s.Inline != nil {
		// The callees' frames, appended to the frame's, or the run the
		// description before that wrote them made, if alike: the guards
		// of an instruction, and those of a callee's stretch, exit to one
		// state, and so to the same frames.
		at := len(f.Inlines)
		for _, in := range inlineLevels(s) {
			k := uint64(abi.ExitHost)
			if in == s.Inline {
				k = kind
			}
			f.Inlines = append(f.Inlines, abi.ExitInline{Closure: uint64(in.Closure), Callee: uint64(in.Callee), Locals: uint64(in.Locals), Args: in.Args,
				ThisSlot: uint64(in.ThisSlot + 1), Kind: k, PC: uint64(in.PC), Depth: uint64(in.Depth),
				Site: int64(in.Site), Base: int64(in.Base)})
		}
		n := len(f.Inlines) - at
		if l := c.exitInlineLast; l >= 0 && l+n == at && slices.Equal(f.Inlines[l:at], f.Inlines[at:]) {
			f.Inlines, at = f.Inlines[:at], l
		}
		c.exitInlineLast = at
		d.InlineAt, d.InlineN = uint32(at), uint16(n)
		d.Kind = uint32(abi.ExitHost)
	}
	if len(c.exitDescs) == cap(c.exitDescs) {
		c.exitDescs = make([]abi.ExitDescriptor, 0, max(min(2*cap(c.exitDescs), 32), 4))
	}
	c.exitDescs = append(c.exitDescs, d)
	return &c.exitDescs[len(c.exitDescs)-1]
}

// exitNodeInfo is what exitNode found of a state's slots' node, by its
// ID: its node in the frame's Tree (off, 0 for none), or that a slot under
// it is where a description cannot say (fail); whether a slot under it
// has its reference where a description's slots may change it (indirect),
// and how many slots under it name another slot as their reference's
// source (cross), which a description that writes that slot is not
// written one by one for -- a leaf's own, crossN of the core's exitCross
// from crossAt.
type exitNodeInfo struct {
	off             uint32
	done, fail      bool
	indirect        bool
	cross           int32
	crossAt, crossN int32
}

// exitNode describes node nd of states' slots, of height h, whose first
// slot is base: once, the descriptions of every state that has it sharing
// what it made.
func (c *core) exitNode(nd *ssa.SlotNode, h, base int) *exitNodeInfo {
	x := &c.exitNodes[nd.ID()]
	if x.done {
		return x
	}
	x.done = true
	f := c.exitFrame
	at := len(c.exitKids)
	if h > 0 {
		m := nd.Mask()
		var indirect bool
		var cross int32
		for _, kid := range nd.Kids() {
			k := bits.TrailingZeros64(m)
			m &= m - 1
			y := c.exitNode(kid, h-1, base+k<<(ssa.SlotShift*h))
			if y.fail {
				c.exitKids = c.exitKids[:at]
				x.fail = true
				return x
			}
			if y.off != 0 {
				c.exitKids = append(c.exitKids, y.off)
			}
			indirect, cross = indirect || y.indirect, cross+y.cross
		}
		x.off = c.exitTreeNode(c.exitKids[at:], false)
		c.exitKids = c.exitKids[:at]
		x.indirect, x.cross = indirect, cross
		return x
	}
	// A leaf: the slots its values are written to, described.
	slots, words, crossAt := len(f.Slots), len(f.Words), len(c.exitCross)
	undo := func() *exitNodeInfo {
		// Each entry it added unchained from its slot's chain, wherever
		// it has come to be in it.
		for i := len(f.Slots) - 1; i >= slots; i-- {
			at, k := &c.exitHead[exitHash(&f.Slots[i])&uint64(len(c.exitHead)-1)], int32(i+1)
			for *at != k {
				at = &c.exitNext[*at-1]
			}
			*at = c.exitNext[i]
		}
		c.exitNext = c.exitNext[:slots]
		for _, w := range f.Words[words:] {
			delete(c.exitWords, w)
		}
		f.Slots, f.Words = f.Slots[:slots], f.Words[:words]
		c.exitKids, c.exitCross = c.exitKids[:at], c.exitCross[:crossAt]
		x.fail = true
		return x
	}
	m := nd.Mask()
	for _, v := range nd.Vals() {
		i := base + bits.TrailingZeros64(m)
		m &= m - 1
		if !c.exitWritten(i, v) {
			continue
		}
		e := abi.ExitSlot{Slot: int32(i), Origin: -1}
		var ok bool
		if b := bigKept(v); b != nil {
			// A BigInt's box, made of the BigInt unboxed, which a state
			// at a loop's header names before the box is made there.
			if e.Value, ok = c.exitBig(b); !ok {
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
		switch {
		case e.Shadow.Kind == abi.ExitReg || e.Shadow.Kind == abi.ExitSpill:
			x.indirect = true
		case e.Shadow.Kind == abi.ExitSlotAddr && e.Shadow.N != e.Slot:
			c.exitCross = append(c.exitCross, e.Shadow.N)
		}
		if e.Origin >= 0 && e.Origin != e.Slot {
			c.exitCross = append(c.exitCross, e.Origin)
		}
		if len(f.Slots) >= len(c.exitHead) {
			c.exitRehash(max(64, 2*len(c.exitHead)))
		}
		hh := exitHash(&e) & uint64(len(c.exitHead)-1)
		k := c.exitHead[hh]
		for k != 0 && f.Slots[k-1] != e {
			k = c.exitNext[k-1]
		}
		if k == 0 {
			f.Slots = append(f.Slots, e)
			c.exitNext = append(c.exitNext, c.exitHead[hh])
			k = int32(len(f.Slots))
			c.exitHead[hh] = k
		}
		c.exitKids = append(c.exitKids, uint32(k-1))
	}
	x.off = c.exitTreeNode(c.exitKids[at:], true)
	c.exitKids = c.exitKids[:at]
	x.crossAt, x.crossN = int32(crossAt), int32(len(c.exitCross)-crossAt)
	x.cross = x.crossN
	return x
}

// exitWritten reports whether an exit writes slot i, whose value in its
// state is v: not a captured binding's, in its cell, nor a slot's own load,
// which the frame has as it is.
func (c *core) exitWritten(i int, v *ssa.Value) bool {
	return v != nil && (v.Op != ssa.OpLoadSlot || v.Aux != i) && !c.captured(i)
}

// exitTreeNode adds a node of entries to the frame's Tree, a leaf's or an
// inner node's, and returns its offset; 0, the empty node, for none.
func (c *core) exitTreeNode(entries []uint32, leaf bool) uint32 {
	if len(entries) == 0 {
		return 0
	}
	f := c.exitFrame
	if len(f.Tree) == 0 {
		f.Tree = append(f.Tree, 0)
	}
	off := uint32(len(f.Tree))
	f.Tree = append(f.Tree, uint32(len(entries))<<1|uint32(b2u(leaf)))
	f.Tree = append(f.Tree, entries...)
	return off
}

// exitCrossWritten reports whether state s's exit writes a slot that one
// under its slots' node nd, of height h, names as its reference's source.
func (c *core) exitCrossWritten(s *ssa.FrameState, nd *ssa.SlotNode, h int) bool {
	x := &c.exitNodes[nd.ID()]
	if x.cross == 0 {
		return false
	}
	if h == 0 {
		for _, o := range c.exitCross[x.crossAt : x.crossAt+x.crossN] {
			if int(o) < s.Len() && c.exitWritten(int(o), s.Slot(int(o))) {
				return true
			}
		}
		return false
	}
	for _, kid := range nd.Kids() {
		if c.exitCrossWritten(s, kid, h-1) {
			return true
		}
	}
	return false
}

// exitHash is where e's chain is among exitHead's: a hash of all its
// fields.
func exitHash(e *abi.ExitSlot) uint64 {
	h := uint64(uint32(e.Slot)) | uint64(uint32(e.Origin))<<32
	h ^= (uint64(uint32(e.Value.N)) | uint64(e.Value.Kind)<<32 | b2u(e.Load)<<40) * 0x9E3779B97F4A7C15
	h ^= (uint64(uint32(e.Shadow.N)) | uint64(e.Shadow.Kind)<<32) * 0xC2B2AE3D27D4EB4F
	return (h * 0x9E3779B97F4A7C15) >> 32
}

// exitRehash makes n chains (a power of 2) of the frame's slot entries.
func (c *core) exitRehash(n int) {
	if cap(c.exitHead) < n {
		c.exitHead = make([]int32, n)
	}
	c.exitHead = c.exitHead[:n]
	clear(c.exitHead)
	for i := range c.exitFrame.Slots {
		h := exitHash(&c.exitFrame.Slots[i]) & uint64(n-1)
		c.exitNext[i], c.exitHead[h] = c.exitHead[h], int32(i+1)
	}
}

// exitLayout is what the code's descriptions share (abi.ExitFrame), made
// at the first.
func (c *core) exitLayout() *abi.ExitFrame {
	if c.exitFrame == nil {
		c.exitFrame = &abi.ExitFrame{FrameLocals: int32(c.f.FrameLocals), ThisSlot: int32(c.f.ThisSlot), Locals: int32(c.f.Locals), Enc: c.encoding(), Kinds: c.f.Kinds}
		c.exitInlineLast = -1
		if w := c.ws; w != nil {
			// The workspace's, kept from compile to compile.
			if w.exitWords == nil {
				w.exitWords = map[uint64]int32{}
			}
			clear(w.exitWords)
			c.exitWords = w.exitWords
			c.exitHead, c.exitNext = w.exitHeadBuf[:0], w.exitNextBuf[:0]
			c.exitFrame.Slots, c.exitFrame.Tree, c.exitFrame.Words = w.exitSlotsBuf[:0], w.exitTreeBuf[:0], w.exitWordsBuf[:0]
			c.exitKids, c.exitCross = w.exitKidsBuf[:0], w.exitCrossBuf[:0]
			c.exitFrame.Inlines = w.exitInlinesBuf[:0]
		} else {
			c.exitWords = map[uint64]int32{}
		}
	}
	return c.exitFrame
}

// encoding is the code's encoding, made once for what the code keeps --
// its exits' frame, its calls of Go -- to share.
func (c *core) encoding() *abi.Encoding {
	if c.encp == nil {
		e := c.enc
		c.encp = &e
	}
	return c.encp
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

// slotsEnd is past every slot the code names by number -- a record's or a
// run-time source's word below it is a slot, one at or above it an
// address (abi.MaxSlot) -- for a function of up to abi.MaxRecords slots,
// the room inlined callees' frames, the homes of objects left unmade and
// native loops' slots fit in; for one of more, which takes none of them,
// its own slots.
func (c *core) slotsEnd() int32 {
	return int32(max(abi.MaxRecords, c.f.Locals+c.f.StackSize))
}

// recordRoom panics, refusing the code, if an exit to state s may write
// more records than the context has room for (abi.MaxRecords): one for
// each slot it names that the frame does not hold as it is, which only a
// state of more slots than that, a function's that nothing else bounds,
// may have.
func (c *core) recordRoom(s *ssa.FrameState) {
	if s.Len() <= abi.MaxRecords {
		return
	}
	n := 0
	for i, v := range s.All() {
		if !(v.Op == ssa.OpLoadSlot && v.Aux == i || c.captured(i)) {
			n++
		}
	}
	if n > abi.MaxRecords {
		panic(fmt.Sprintf("mir: an exit of %d records", n))
	}
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
// pushGrows reports whether a push of t's values, made natively, has its
// array grown by push's leaf where it has no room for them (mir's
// pushGrow): of several values, as many as a leaf's call takes.
func pushGrows(t *ssa.CallSite, enc abi.Encoding) bool {
	return t.Argc > 1 && 2+t.Argc <= abi.MaxGoArgs && enc.LeafFast != 0
}

// windowCallees is, for register code's native call of sites (ssa.CallSite's
// Window) that leaves its function out of its callee's window, the
// function object each site's checks find, 0 for one checked by its code
// (abi.CallState's Callees); nil where the call writes the function to
// the window's register 0: a callee that reads it (ReadsFunction), any
// function past the sites (gen), a call through Function.prototype.call, a
// super(). A forwarding call, f.apply(x, arguments) (Forward), leaves f out
// as a plain call does its function: its sites are f's.
func windowCallees(site *ssa.CallSite, sites []*ssa.CallSite, gen bool) []uintptr {
	if site.Window == 0 || gen || len(sites) == 0 || site.Via != 0 || site.Super {
		return nil
	}
	out := make([]uintptr, len(sites))
	for i, t := range sites {
		if t.ReadsFunction || t.Via != 0 || t.Super {
			return nil
		}
		if t.ByCode == 0 {
			out[i] = t.Callee
		}
	}
	return out
}

func (c *core) callState(v *ssa.Value, s *ssa.FrameState, skip func(int) bool, operands int) (*abi.CallState, []callRecord) {
	defer c.viewExits()()
	saves := c.savesOf(v)
	if len(c.callSlab) == cap(c.callSlab) {
		c.callSlab = make([]abi.CallState, 0, max(min(2*cap(c.callSlab), 32), 4))
	}
	c.callSlab = append(c.callSlab, abi.CallState{PC: uint32(s.PC), Depth: uint32(s.Depth), Site: int32(s.Site), Frame: c.exitLayout()})
	st := &c.callSlab[len(c.callSlab)-1]
	cs := c.cs
	refs, rest, refRecs, restRecs := cs.refs[:0], cs.rest[:0], cs.refRecs[:0], cs.restRecs[:0]
	for i, x := range s.All() {
		if x.Op == ssa.OpLoadSlot && x.Aux == i || c.captured(i) || skip(i) {
			continue
		}
		rec, r := abi.CallRecord{Slot: int32(i), Origin: -1}, callRecord{slot: i, x: x}
		full := operands >= 0 && i >= operands && i < s.Own()
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
	cs.refs, cs.rest, cs.refRecs, cs.restRecs = refs, rest, refRecs, restRecs
	if len(refRecs)+len(restRecs) > abi.MaxRecords {
		// Past the context's room for them (abi.MaxRecords): only a
		// function of more slots than that, which nothing else bounds,
		// has such a state.
		panic(fmt.Sprintf("mir: a call's state of %d records", len(refRecs)+len(restRecs)))
	}
	start := len(cs.records)
	cs.records = append(append(cs.records, refRecs...), restRecs...)
	st.Records = cs.records[start:len(cs.records):len(cs.records)]
	c.calls = append(c.calls, st)
	cs.out = append(append(cs.out[:0], refs...), rest...)
	return st, cs.out
}

// finishCalls gives the code's calls' records an array of their own, as
// long as all they hold: they are built in the workspace, as a call's
// state's address is in the code but not its records'.
func (c *core) finishCalls() {
	all := make([]abi.CallRecord, len(c.cs.records))
	off := 0
	for _, st := range c.calls {
		n := copy(all[off:], st.Records)
		st.Records = all[off : off+n : off+n]
		off += n
	}
}

// goState describes a call of Go, op, with Go's result to keep cell keep
// (abi.GoCallState) -- for a call (ssa.GoCall), the PC and depth of v's
// state -- and its operands args, each where the code keeps it as the call
// starts, as an exit's description does (exitDescriptor), the registers
// the call's routine saves (goSaving) among them. It reports which
// operands the call writes itself: those a description cannot find.
func (c *core) goState(op uint64, keep int, v *ssa.Value, args []*ssa.Value) (*abi.GoCallState, []bool) {
	if len(c.goSlab) == cap(c.goSlab) {
		c.goSlab = make([]abi.GoCallState, 0, max(min(2*cap(c.goSlab), 32), 4))
	}
	c.goSlab = append(c.goSlab, abi.GoCallState{Op: op, Keep: uint64(keep), Enc: c.encoding()})
	st := &c.goSlab[len(c.goSlab)-1]
	if v != nil && (ssa.GoOp(op&0xff) == ssa.GoCall || ssa.GoOp(op&0xff) == ssa.GoHost) {
		// A call's, or an instruction's Go makes on the frame (GoHost):
		// the instruction Go finds at the PC.
		st.SetsPC, st.PC, st.Depth = true, uint32(v.State.PC), uint32(v.State.Depth)
		if in := v.State.Inline; in != nil {
			// Inside an inlined callee: its instruction, which Go makes
			// without a frame or not at all.
			st.Inline, st.InlinePC = in.Closure, in.PC
		}
	}
	if v != nil && v.State != nil && (ssa.GoOp(op&0xff) == ssa.GoClosure || ssa.GoOp(op&0xff) == ssa.GoClose) {
		// A closure's or a scope's end: the instruction at the PC, which
		// Go makes on a register frame whose captured registers are its
		// slots as the interpreter makes it there (the VM's
		// jitRegFrameWhole).
		st.SetsPC, st.PC, st.Depth = true, uint32(v.State.PC), uint32(v.State.Depth)
	}
	if v != nil && ssa.GoOp(op&0xff) == ssa.GoLeaf && len(v.Calls()) == 1 {
		// The leaf the call checked it calls, which Go takes from here,
		// and whether anything reads its result.
		st.Leaf, st.Unused, st.Give, st.Reuse = v.Calls()[0].Leaf, v.Calls()[0].Unused, v.Calls()[0].Give, v.Calls()[0].Reuse
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
		a.Kind, a.At = abi.GoArgWord, abi.SpillOffset(l.N)
	case abi.ExitF64Reg:
		a.Kind, a.At = abi.GoArgF64, abi.OffXRegs+(l.N&31)*8
	case abi.ExitF64Spill:
		a.Kind, a.At = abi.GoArgF64, abi.SpillOffset(l.N)
	case abi.ExitBoolReg:
		a.Kind, a.At = abi.GoArgBool, abi.OffRegs+(l.N&31)*8
	case abi.ExitBoolSpill:
		a.Kind, a.At = abi.GoArgBool, abi.SpillOffset(l.N)
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
			a.Ref, a.From = abi.GoRefSource, abi.SpillOffset(src.N)
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
	case abi.ExitSpill, abi.ExitF64Spill, abi.ExitBoolSpill, abi.ExitI32Spill:
		return l, true
	}
	float, kind := l.Kind == abi.ExitF64Reg, abi.ExitSpill
	switch l.Kind {
	case abi.ExitF64Reg:
		kind = abi.ExitF64Spill
	case abi.ExitBoolReg:
		kind = abi.ExitBoolSpill
	case abi.ExitI32Reg:
		kind = abi.ExitI32Spill
	}
	// The value whose register exitValue found: x, or the operand a box is
	// made of.
	in := x
	if remat(x) {
		in = x.Args[0]
		if y := i32Boxed(x); y != nil {
			in = y
		}
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
	c.finishCalls()
	if f := c.exitFrame; f != nil {
		if w := c.ws; w != nil {
			// Its buffers, for the next compile.
			w.exitSlotsBuf, w.exitTreeBuf, w.exitWordsBuf = f.Slots[:0], f.Tree[:0], f.Words[:0]
			w.exitKidsBuf, w.exitCrossBuf = c.exitKids[:0], c.exitCross[:0]
			w.exitHeadBuf, w.exitNextBuf = c.exitHead[:0], c.exitNext[:0]
			w.exitInlinesBuf = f.Inlines[:0]
		}
		f.Slots, f.Tree, f.Words, f.Inlines = exact(f.Slots), exact(f.Tree), exact(f.Words), exact(f.Inlines)
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
		if x := i32Boxed(v); x != nil {
			// The number of an int32: from the int32, where a loop
			// header's state names it before its conversion is made, or
			// where it is never made (findLazy).
			return at(x, abi.ExitI32Reg, abi.ExitI32Spill)
		}
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
	if site.Forward {
		// apply_arguments' (GoCallForward), of f, apply and the receiver.
		return goOpWord(&ssa.CallSite{Go: ssa.GoCall, Argc: site.Argc, Forward: true})
	}
	return goOpWord(&ssa.CallSite{Go: ssa.GoCall, Argc: site.Argc, Method: site.Method, New: site.Pool != 0})
}

// windowGoCall reports whether a call of Go, v, made where register code's
// native call of a function does not make it natively (ssa.CallSite's
// Window), has more operands than the context's (abi.MaxGoArgs): it
// writes them to the callee's window past the VM's stack's top -- the
// function in its register 0, a method call's receiver in 1, the
// arguments from 2 -- where Go reads them (the VM's jitRegGoCallOn),
// as the window's layout is the frame Go makes for the call. Register code,
// whose window holds the operands, makes such a call as it makes any other.
func windowGoCall(v *ssa.Value, site *ssa.CallSite) bool {
	return len(v.Args) > abi.MaxGoArgs && site.Window != 0 && !site.Super && !site.Forward && site.Accessor == ssa.AccessorNone
}

// windowGoDisp is where in the callee's window a wide call of Go
// (windowGoCall) writes its operand i: a method call's receiver, its first
// operand, in register 1, the function in 0, the arguments from 2.
func windowGoDisp(i int, method bool, vs int32) int32 {
	switch {
	case method && i == 0:
		return vs
	case method && i == 1:
		return 0
	case method:
		return int32(i) * vs
	case i == 0:
		return 0
	}
	return int32(i+1) * vs
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

// leafFast reports whether a call of Go, site, is a leaf's that native
// code makes through the VM's routine for them (abi.Encoding's LeafFast):
// one that knows its leaf (ssa.CallSite's Leaf), of an encoding that has
// the routine.
func leafFast(site *ssa.CallSite, enc abi.Encoding) bool {
	return site.Go == ssa.GoLeaf && site.Leaf != 0 && enc.LeafFast != 0
}

// lengthWrites reports whether a call of Go, site, of argc operands, is a
// property's write of length (abi.Encoding's LengthKey), which native code
// makes itself where it shortens an array (lengthWrite), the call Go's
// otherwise: the receiver and the value, the values 16 bytes.
func lengthWrites(site *ssa.CallSite, argc int, enc abi.Encoding) bool {
	return site.Go == ssa.GoCall && site.Accessor == ssa.AccessorSet && enc.LengthKey != 0 && site.Key == enc.LengthKey &&
		argc == 2 && enc.ValueSize == 16 && enc.WriteBarrier != 0
}

// closes reports whether a scope's end natively (cellsClose), site, closes
// the cell of x, one of its Captures: a local from its Argc on, and, where
// it closes a count of them (ssa.CallSite's Closes, register code's
// close_regs), before its end.
func closes(site *ssa.CallSite, x int32) bool {
	at := int(x >> 1)
	return at >= site.Argc && (site.Closes == 0 || at < site.Argc+site.Closes)
}

// goOpWord is the word a call of Go, site, passes Go (abi.Context's GoOp).
// forInStarts reports whether a call of Go of argc operands, of site, is a
// for...in's start native code makes where it can (forInStart): where the
// encoding says where the VM keeps a layout's keys, the realm's chains and
// the runtime's last cursor.
func forInStarts(site *ssa.CallSite, argc int, enc abi.Encoding) bool {
	return site.ForIn && argc == 1 && enc.ValueSize == 16 && enc.WriteBarrier != 0 && enc.IterState != 0 && enc.ForInState != 0 &&
		enc.HostForInLast != 0 && enc.SideForIn != 0
}

func goOpWord(site *ssa.CallSite) uint64 {
	op := uint64(site.Go)
	if site.Go == ssa.GoArith || site.Go == ssa.GoAdd || site.Go == ssa.GoAppend {
		// What of its operands' BigInts it may give back.
		op |= uint64(site.Release) << abi.GoOpKindShift
	}
	if site.Owned {
		op |= abi.GoOpOwned
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
	if site.Go == ssa.GoConcat || site.Go == ssa.GoArgument || site.Go == ssa.GoHost {
		// Its operands' count; an argument's read or the arguments' count.
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
		case site.ForIn:
			op |= abi.GoCallForIn << abi.GoOpKindShift
		case site.Rest:
			op |= abi.GoCallRest << abi.GoOpKindShift
		case site.Fill:
			op |= abi.GoCallFill << abi.GoOpKindShift
		case site.Step:
			op |= abi.GoCallStep << abi.GoOpKindShift
		case site.StepResult:
			op |= abi.GoCallStepResult << abi.GoOpKindShift
		case site.TypeOf:
			op |= abi.GoCallTypeOf << abi.GoOpKindShift
		case site.InstanceOf:
			op |= abi.GoCallInstanceOf << abi.GoOpKindShift
		case site.Super:
			op |= abi.GoCallSuper << abi.GoOpKindShift
		case site.Forward:
			op |= abi.GoCallForward << abi.GoOpKindShift
		case site.Whole:
			op |= abi.GoCallWhole << abi.GoOpKindShift
		case site.Instr != 0:
			op |= abi.GoCallOp<<abi.GoOpKindShift | uint64(site.Instr)<<abi.GoOpIndexShift
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

// genNexts reports whether v, a method call Go would make, of no argument
// or one, calls a generator's next natively (ssa.CallSite's Gens on a
// call, genNext): of 16-byte values, the encoding knowing a generator's
// layout, an object's properties and the collector's flag, the call its
// result objects' pool.
func genNexts(v *ssa.Value, enc abi.Encoding) bool {
	if enc.GenObject == 0 || enc.ValueSize != 16 || enc.WriteBarrier == 0 || enc.ObjectProps == 0 || len(v.Calls()) != 1 {
		return false
	}
	site := v.Calls()[0]
	return site.Go == ssa.GoCall && site.Method && !site.Step && len(site.Gens) != 0 && site.GenNext != 0 && site.Pool != 0 &&
		site.Argc <= 1 && len(v.Args) == site.Argc+2
}

// genForOfs reports whether v, a for...of's cursor's making Go would make
// (ssa.CallSite's ForOf), takes a generator's room's cursor natively
// (ssa.CallSite's GenIter, genForOf): of 16-byte values, the encoding
// knowing a generator's room, an object's layout and the collector's flag.
func genForOfs(v *ssa.Value, enc abi.Encoding) bool {
	if enc.GenObject == 0 || enc.GenRoomCursor == 0 || enc.IterState == 0 || enc.ValueSize != 16 || enc.WriteBarrier == 0 ||
		enc.ObjectProps == 0 || enc.ObjectShape == 0 || len(v.Calls()) != 1 {
		return false
	}
	site := v.Calls()[0]
	return site.Go == ssa.GoCall && site.ForOf && site.GenIter != 0 && site.GenIterFn != 0 && site.GenNext != 0 && len(v.Args) == 1
}

// createMakes reports whether v, a leaf's call (ssa.GoLeaf) of
// Object.create of a receiver and one argument, makes its object natively
// from its pool (ssa.CallSite's Pool, createMake): of 16-byte values, the
// encoding knowing the collector's flag and the VM's routine for leaves,
// which makes any other.
func createMakes(v *ssa.Value, enc abi.Encoding) bool {
	if len(v.Calls()) != 1 || enc.ValueSize != 16 || enc.WriteBarrier == 0 {
		return false
	}
	site := v.Calls()[0]
	return leafFast(site, enc) && site.Pool != 0 && site.Method && site.Argc == 1 && len(v.Args) == 3
}

// genMakes reports whether v, a call Go would make, makes a generator
// natively (ssa.CallSite's GenMake, genMake): of 16-byte values, the
// encoding knowing a generator's layout, an object's properties and the
// collector's flag, the call its generators' pool.
func genMakes(v *ssa.Value, enc abi.Encoding) bool {
	if enc.GenObject == 0 || enc.ValueSize != 16 || enc.WriteBarrier == 0 || enc.ObjectProps == 0 || enc.ShapeKeys == 0 || len(v.Calls()) != 1 {
		return false
	}
	site := v.Calls()[0]
	ops := site.Argc + 1
	if site.Method {
		ops++
	}
	return site.Go == ssa.GoCall && site.GenMake != 0 && site.Pool != 0 && !site.New && !site.Step && len(v.Args) == ops
}

// genMakeArgs has put write what a generator a call v makes natively
// (genMake) is given: the receiver of a method call, at the generator's
// GenThis, then each argument up to the function's parameters and locals,
// at its place in the generator's values (vals).
func genMakeArgs(v *ssa.Value, enc abi.Encoding, put func(x *ssa.Value, off int32, vals bool)) {
	site := v.Calls()[0]
	at := 0
	if site.Method {
		put(v.Args[0], enc.GenObject+enc.GenThis, false)
		if site.GenThisAt != 0 {
			// A register generator's register 1 too (CallSite's GenThisAt).
			put(v.Args[0], int32(site.GenThisAt-1)*int32(enc.ValueSize), true)
		}
		at = 1
	}
	for i := 0; i < site.Argc && i < site.Params && site.GenArgAt+i < site.LocalCount; i++ {
		put(v.Args[at+1+i], int32(site.GenArgAt+i)*int32(enc.ValueSize), true)
	}
}

// stepGens reports whether v, a loop's step (ssa.CallSite's Step), resumes
// generators natively (CallSite's Gens): of 16-byte values, the encoding
// knowing a generator's layout and the collector's flag.
func stepGens(v *ssa.Value, enc abi.Encoding) bool {
	if enc.GenObject == 0 || enc.ValueSize != 16 || enc.WriteBarrier == 0 || len(v.Calls()) != 1 {
		return false
	}
	site := v.Calls()[0]
	return len(site.Gens) != 0 && site.GenNext != 0
}

// laterUse is a state allocate's uses meet, put off to its block's end:
// where, at whose instruction's own, and whether Go refuses the call it
// is the state of (allocate's laterState).
type laterUse struct {
	s       *ssa.FrameState
	at, own int32
	refused bool
}

// stateAsk is what the states allocate meets in a block ask of a node of
// their slots: the last position, the last that Go does not refuse (-1
// for none), and the first instruction's own (MaxInt32 for none), the
// block's mark.
type stateAsk struct{ mark, end, hot, own int32 }
