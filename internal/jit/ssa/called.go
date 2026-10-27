package ssa

import (
	"maps"
	"slices"

	"github.com/go-quickjs/go-quickjs/internal/jit/ir"
)

// Closures only called.
//
// A closure the function makes (ir.Closure) that nothing but the
// function's own calls of it uses -- `const sq = (x) => x * x + n` and
// `sq(i)` in a loop; `var f = function () { return k }; f()` in a loop's
// turn -- is a call whose target the function knows, as V8's graph knows a
// call's target once the closure is a constant of it: the calls are
// inlined, and the closure, which nothing is then left to see, is not
// made, as a loop's callback the function makes is not (MadeCallback): its
// slots hold its template, which Go makes the closure from at an exit
// (jitMaterialize). The function's first build says which closures they
// are (calledClosures), their uses being in its SSA, and a second build
// inlines their calls (calledSite) and leaves them unmade (unmakeCalled).

// CalledClosureFeedback is Feedback that knows the closures the function
// makes and how its calls are made: CalledClosure is the closure the code
// makes at pc (ir.Closure), as MadeCallback describes one -- its template,
// what it captures -- and its function to inline where a call calls it,
// its program and feedback (InlineSite; the builder sets the rest), or
// false for one that may not be inlined so; CallShape is the call at pc's
// argument count and whether it is a method's.
type CalledClosureFeedback interface {
	CalledClosure(pc int) (MadeCallback, InlineSite, bool)
	CallShape(pc int) (argc int, method, ok bool)
}

// calledClosures finds, in the function's first build, the closures it
// makes that nothing uses but the function's own calls of them, as their
// callee: by the PC of each call, the PC of the closure it calls.
//
// A closure's value -- the call of Go that makes it (GoClosure), which the
// function's own code makes -- may be what a frame state names, whose exit
// Go makes it at; checked initialized, and checked to be a call's callee;
// and merged by phis, where the closure is made once an invocation, out of
// every loop, with nothing but itself and the slot an entry loads of the
// local it is kept in: that local holds nothing else at an entry, the
// closure being all it is ever given but its dead zone's marker. Any other
// use -- an argument, a store, a comparison, a return, a copy kept across a
// loop's header -- has it made. A call of a value of it a phi merges, made
// past a loop's header, is of the one closure; one of a closure made in a
// loop's turn is of that turn's own. And the captured bindings the inlined
// code reads in the function's slots must be the ones the closure took: no
// scope's end closes them (CloseUpvalues) between its making and a call.
//
// A closure given to a callee inlined in the first build that calls what
// it is given (InlineCallsGiven) is in the callee's slots, no value's
// operand: the call, an exit there (frame's pending), is of it where the
// exit's state has one of its values as the call's callee. Those calls,
// by their paths (frame's path), are given: the second build inlines them,
// and checks what the first could not see past those exits (unmadeAlone).
func (b *builder) calledClosures() (map[int]int, map[string]int) {
	cf, ok := b.fb.(CalledClosureFeedback)
	if !ok {
		return nil, nil
	}
	f, p := b.f, b.root.p
	type cand struct {
		pc   int
		call *Value
	}
	var cands []cand
	for _, blk := range f.Blocks {
		for _, v := range blk.Values {
			if v.Op != OpCall || len(v.Calls()) != 1 || v.Calls()[0].Go != GoClosure || v.State == nil || v.State.Inline != nil {
				continue
			}
			if pc := v.State.Site; pc >= 0 && pc+1 < len(p.Code) && p.Code[pc].Op == ir.Closure && b.made[pc] == 0 {
				cands = append(cands, cand{pc: pc, call: v})
			}
		}
	}
	if len(cands) == 0 {
		return nil, nil
	}
	// Each value's users, by ID, in one slice -- those of the value of ID
	// i are all[start[i]:start[i+1]] -- and the values blocks branch on
	// or return.
	n := f.nextID
	start, controls := make([]int32, n+1), make([]bool, n)
	for _, blk := range f.Blocks {
		for _, v := range blk.Values {
			for _, a := range v.Args {
				start[a.ID+1]++
			}
		}
		if blk.Control != nil {
			controls[blk.Control.ID] = true
		}
	}
	for i := range n {
		start[i+1] += start[i]
	}
	all, fill := make([]*Value, start[n]), slices.Clone(start[:n])
	for _, blk := range f.Blocks {
		for _, v := range blk.Values {
			for _, a := range v.Args {
				all[fill[a.ID]] = v
				fill[a.ID]++
			}
		}
	}
	users := func(v *Value) []*Value { return all[start[v.ID]:start[v.ID+1]] }
	// The root's own frame states, by the PC of their operation, once a
	// closure's uses ask for them.
	var states map[int]*FrameState
	state := func(pc int) *FrameState {
		if states == nil {
			states = map[int]*FrameState{}
			for _, blk := range f.Blocks {
				for _, v := range blk.Values {
					if s := v.State; s != nil && s.Inline == nil && s.Site >= 0 {
						states[s.Site] = s
					}
				}
				if s := blk.State; s != nil && s.Inline == nil && s.Site >= 0 {
					states[s.Site] = s
				}
			}
		}
		return states[pc]
	}
	// callee is the value the call at pc calls, the slot below its
	// arguments, if it is a call of the root's own code, no method's.
	callee := func(pc int) (*Value, bool) {
		argc, method, ok := cf.CallShape(pc)
		if !ok || method {
			return nil, false
		}
		st := state(pc)
		if st == nil {
			return nil, false
		}
		at := p.Locals + st.Depth - argc - 1
		if at < p.Locals || at >= st.Own() {
			return nil, false
		}
		return st.Slots[at], true
	}
	// inLoop reports whether a backward branch's loop holds pc.
	inLoop := func(pc int) bool {
		for q, in := range p.Code {
			if (in.Op == ir.Jump || in.Op == ir.Branch) && in.Target <= pc && pc <= q {
				return true
			}
		}
		return false
	}
	// The given calls' callees, by exit: the callee's slot in the state, and
	// the root's call the callee's frame is inlined at, that of the frame
	// the root inlined.
	type givenCall struct {
		path   string
		callee *Value
		root   int
	}
	var givens []givenCall
	for _, gs := range b.givenStates {
		at, ok := gs.fr.fb.(CalledClosureFeedback)
		if !ok {
			continue
		}
		argc, method, ok := at.CallShape(gs.pc)
		slot := gs.fr.base + gs.fr.p.Locals + gs.fr.p.Maps[gs.pc].Depth - argc - 1
		if !ok || method || slot < gs.fr.base || slot >= len(gs.state.Slots) || gs.state.Slots[slot] == nil {
			continue
		}
		top := gs.fr
		for top.parent != nil && top.parent != b.root {
			top = top.parent
		}
		if top.parent != b.root || b.inlined[top.at] != top {
			// Inside a loop's callback (inlineCallback): its frame is
			// planned after the closures are (unmakeCalled).
			continue
		}
		givens = append(givens, givenCall{path: givenPath(gs.fr.path, gs.pc), callee: gs.state.Slots[slot], root: top.at})
	}
	called := map[int]int{}
	var given map[string]int
	for _, c := range cands {
		var obj *Value
		for _, u := range users(c.call) {
			if u.Op == OpKept && u.Args[1] == c.call {
				obj = u
			}
		}
		if obj == nil {
			continue
		}
		// The local the closure is kept in, the instruction after it.
		home := -1
		if next := p.Code[c.pc+1]; (next.Op == ir.Copy || next.Op == ir.StoreLoad) && next.Left.Slot == p.Code[c.pc].Dest && next.Dest < f.FrameLocals {
			home = next.Dest
		}
		// The entry after it, which the second build drops (unmakeCalled):
		// the closure the slot it loads holds is the one made.
		var after *Block
		for _, e := range f.Entries {
			if e.PC == c.pc+1 && !looped(p, e.PC) {
				after = e.Block
			}
		}
		// set is its values: it, and the phis that merge it with nothing
		// but itself, the slot the entry after it loads, the slot an entry
		// loads of its local and the local's dead zone -- found as the
		// largest such set, phis merging each other in a loop's cycle
		// taken until one merges something else; same those of it as made
		// here, merged with no value of it from before.
		set := map[*Value]bool{obj: true}
		for work := []*Value{obj}; len(work) != 0; {
			x := work[len(work)-1]
			work = work[:len(work)-1]
			for _, u := range users(x) {
				if u.Op == OpPhi && !set[u] {
					set[u] = true
					work = append(work, u)
				}
			}
		}
		// arg is what a's being an argument of a phi of it is: one of it
		// (in), one of it as made here (here), or neither.
		arg := func(a *Value, in map[*Value]bool) (ok, here bool) {
			switch {
			case in[a]:
				return true, true
			case set[a]:
				return true, false
			case a.Op == OpLoadSlot && a.Block == after && after != nil && a.Aux == p.Code[c.pc].Dest:
				return true, true
			case a.Op == OpLoadSlot && a.Block.PC < 0 && a.Aux == home && home >= 0,
				a.Op == OpConst && a.Const.Kind == ir.Uninitialized:
				return true, false
			}
			return false, false
		}
		for changed := true; changed; {
			changed = false
			for x := range set {
				for _, a := range x.Args {
					if ok, _ := arg(a, set); x.Op == OpPhi && !ok {
						delete(set, x)
						changed = true
						break
					}
				}
			}
		}
		same := maps.Clone(set)
		for changed := true; changed; {
			changed = false
			for x := range same {
				for _, a := range x.Args {
					if _, here := arg(a, same); x.Op == OpPhi && !here {
						delete(same, x)
						changed = true
						break
					}
				}
			}
		}
		phis, good := len(same) != len(set), true
		for x := range set {
			if controls[x.ID] {
				good = false
			}
			for _, u := range users(x) {
				switch {
				case set[u], u.Op == OpCheckInit, u.Op == OpCall:
					// One of it; its check; a call, which must be one of it
					// (below).
				case u.Op == OpObjectOf:
					// A call's check of its callee, which the calls' inlining
					// makes of the closure (below).
					for _, w := range users(u) {
						if w.Op != OpSameObject {
							good = false
						}
					}
				default:
					good = false
				}
			}
		}
		if !good || phis && (home < 0 || p.IsCaptured(home) || inLoop(c.pc)) {
			continue
		}
		made, _, ok := cf.CalledClosure(c.pc)
		if !ok {
			continue
		}
		// Its calls: every call one of its values is an operand of calls
		// it, as its callee, and only so; and the calls Go makes at an exit
		// whose state names it as the callee.
		var calls []int
		for x := range set {
			for _, u := range users(x) {
				if u.Op != OpCall || u == c.call {
					continue
				}
				pc := -1
				if u.State != nil && u.State.Inline == nil {
					pc = u.State.Site
				}
				if v, ok := callee(pc); !ok || v != x || len(u.Args) == 0 || u.Args[0] != x || countOf(u.Args, x) != 1 {
					good = false
					continue
				}
				if !containsInt(calls, pc) {
					calls = append(calls, pc)
				}
			}
		}
		for pc, in := range p.Code {
			if in.Op != ir.Host || !reachable(p, pc) || containsInt(calls, pc) {
				continue
			}
			if v, ok := callee(pc); ok && set[v] {
				calls = append(calls, pc)
			}
		}
		var mine []givenCall
		for _, g := range givens {
			if set[g.callee] {
				mine = append(mine, g)
			}
		}
		if !good || len(calls) == 0 && len(mine) == 0 {
			continue
		}
		// No scope's end closes what it captured between its making and a
		// call -- a given one's, the root's call the callee is inlined at.
		closes := func(v *Value, pc int) bool {
			lo, hi := c.pc, pc
			if !same[v] || pc < c.pc {
				lo, hi = 0, len(p.Code)
			}
			for q := lo; q < hi; q++ {
				if in := p.Code[q]; in.Op == ir.CloseUpvalues && reachable(p, q) && closesCaptured(int(in.Key), made.Captures) {
					return true
				}
			}
			return false
		}
		for _, pc := range calls {
			v, _ := callee(pc)
			good = good && !closes(v, pc)
		}
		for _, g := range mine {
			good = good && !closes(g.callee, g.root)
		}
		if good {
			for _, pc := range calls {
				called[pc] = c.pc
			}
			for _, g := range mine {
				if given == nil {
					given = map[string]int{}
				}
				given[g.path] = c.pc
			}
		}
	}
	return called, given
}

// closesCaptured reports whether a scope's end closing the locals from key
// on closes any of captures (MadeCallback's Captures): a local's,
// index<<1|1.
func closesCaptured(key int, captures []int32) bool {
	for _, c := range captures {
		if c&1 != 0 && int(c>>1) >= key {
			return true
		}
	}
	return false
}

// countOf is how many of vs are x.
func countOf(vs []*Value, x *Value) int {
	n := 0
	for _, v := range vs {
		if v == x {
			n++
		}
	}
	return n
}

// containsInt reports whether xs has x.
func containsInt(xs []int, x int) bool {
	for _, y := range xs {
		if y == x {
			return true
		}
	}
	return false
}
