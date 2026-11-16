//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64))

package vm

import (
	"slices"
	"unsafe"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
	"github.com/go-quickjs/go-quickjs/internal/jit/abi"
	jitcompile "github.com/go-quickjs/go-quickjs/internal/jit/compile"
	"github.com/go-quickjs/go-quickjs/internal/jit/ir"
	"github.com/go-quickjs/go-quickjs/internal/jit/ssa"
)

// Register code's loops of built-ins' calls (README "Array loops"): a call
// of Array.prototype's forEach, reduce, map, filter, some, every, find or
// findIndex, or a typed array's method of those names, that its code's
// calls of Go have made on an array of the realm's, given a closure of the
// realm's as its callback, is made a loop of the code's own by the builder
// (ssa.ForEachSite), its callback called natively, as the stack form's is:
// the call learned at its call of Go (jitRegForEachSeen), answered by
// jitRegFeedback's ForEach, and a loop that leaves native code finished by
// Go from its index (jitRegLoopLeft), then native code after the call.

// jitRegForEachSeen learns, after Go made the call at register pc rpc of
// f's code e -- call_method_w of argc arguments, callee on recv -- that it
// is a loop's built-in given cb, as the stack form's code learns it
// (jitForEachSeen): the next compile makes it a loop of its own (ForEach),
// the code stale from here (jitRegReopt), the callback compiled for native
// callers (jitRegNativeCallee). One whose callback is another function's,
// or whose built-in or typed arrays' prototype is another, the call is
// Go's, as a call of too many targets is; one given a new closure of the
// same function each time calls it by its code (ssa.CallSite's ByCode). A
// sort, which native code makes only with its comparator inlined, is Go's.
func (r *Runtime) jitRegForEachSeen(f *frame, e *jitRegEntry, rpc int, callee, recv, cb Value, argc int) {
	if argc != 1 && argc != 2 || !callee.IsObject() || !cb.IsObject() || e.invalid {
		return
	}
	o := callee.Object()
	kind := r.jitLoopKind(o)
	var proto *Object
	switch {
	case kind == 0 || kind&abi.LoopKindMask == abi.LoopSort:
		return
	case kind&abi.LoopTyped != 0:
		if r.jitLoopTyped(recv) == nil {
			return
		}
		proto = recv.Object().proto()
	case r.plainArray(recv) == nil:
		return
	}
	fd := cb.Object().fn()
	if fd == nil || fd.native != nil || fd.bound || fd.closure == nil || fd.closure.realm != r.Realm || fd.closure.reg == nil {
		return
	}
	cl := fd.closure
	l := r.jit.regLearnedOf(r.jitKeyOf(f.cl.fn))
	for i := range l.nativeCalls {
		x := &l.nativeCalls[i]
		if int(x.pc) != rpc || !x.forEach {
			continue
		}
		if x.cb.fn != cl.fn || x.obj != o || x.proto != proto {
			return
		}
		if x.cbObj != cb.Object() && !x.byCode {
			x.byCode = true
			r.jitRegReopt(f.cl, e)
		}
		r.jitRegNativeCallee(cl, cl.reg)
		return
	}
	l.nativeCalls = append(l.nativeCalls, jitInline{pc: int32(rpc), obj: o, forEach: true, cb: cl, cbObj: cb.Object(), proto: proto})
	r.jitRegNativeCallee(cl, cl.reg)
	r.jitRegReopt(f.cl, e)
}

// ForEach is ssa.ForEachFeedback's: the loop of a built-in's call that the
// call_method_w at the program's instruction pc was seen to make
// (jitRegForEachSeen), its callback called natively, through its link, its
// window made from the loop's operands (ssa.CallSite's Window), as a
// register call's native call is (regCallSite), or inlined in the loop
// (callbackInline).
func (f *jitRegFeedback) ForEach(pc int) (ssa.ForEachSite, bool) {
	w, op, ok := f.at(pc)
	l := f.fb.e
	if !ok || op != ir.Host || !f.p.Code[pc].Window || w.Op() != bytecode.RCallMW || l == nil {
		return ssa.ForEachSite{}, false
	}
	r := f.fb.r
	rpc := int32(f.p.Origin[pc])
	for _, x := range l.nativeCalls {
		if x.pc != rpc || !x.forEach || x.cb == nil {
			continue
		}
		kind := r.jitLoopKind(x.obj)
		if kind == 0 || kind&abi.LoopKindMask == abi.LoopSort {
			return ssa.ForEachSite{}, false
		}
		var proto uintptr
		k := f.fb.keep()
		if kind&abi.LoopTyped != 0 {
			// A typed array's, of the type its prototype is.
			t := slices.Index(r.typedArrayProtos[:], x.proto)
			if x.proto == nil || t < 0 {
				return ssa.ForEachSite{}, false
			}
			kind |= t << abi.LoopTypedShift
			proto = uintptr(unsafe.Pointer(x.proto))
			k.holders = append(k.holders, x.proto)
		}
		argc := 3
		if kind&abi.LoopKindMask == abi.LoopReduce {
			argc = 4
		}
		site, ok := f.regCallSite(pc, x.cbObj, x.cb, x.cb.reg, argc, true, nil, x.byCode)
		if !ok {
			return ssa.ForEachSite{}, false
		}
		k.holders = append(k.holders, x.obj)
		fe := ssa.ForEachSite{Fn: uintptr(unsafe.Pointer(x.obj)), Kind: kind, Proto: proto, Callback: site}
		if in, ok := f.callbackInline(x, site, argc); ok {
			fe.Inline = &in
		}
		return fe, true
	}
	return ssa.ForEachSite{}, false
}

// callbackInline is the InlineSite of x's callback, a loop's (ForEach),
// for native code to inline in the loop, as V8 inlines a forEach's
// callback, under the rules a register callee's inlining follows
// (jitRegFeedback's Inline): the one closure the loop has been given,
// which the loop checks it is given, its window the loop's call's
// operands -- not one made anew at each call of its maker, nor an arrow
// that reads its this, nor one whose inlining at the call left native code
// too often (jitInlineLeft's notInline), called natively since.
func (f *jitRegFeedback) callbackInline(x jitInline, site ssa.CallSite, argc int) (ssa.InlineSite, bool) {
	cl, o := x.cb, x.cbObj
	l := f.fb.e
	if x.byCode || site.ByCode != 0 || site.ArrowThis || cl.fn == f.fb.fn || cl.fn == f.fb.keep().fn || jitNoInline != nil && jitNoInline(cl.fn) ||
		l != nil && slices.Contains(l.notInline, x.pc) {
		return ssa.InlineSite{}, false
	}
	p, err := jitcompile.LowerRegInline(cl.reg.rc, cl.fn)
	if err != nil || p.Validate() != nil {
		return ssa.InlineSite{}, false
	}
	r := f.fb.r
	r.jitRegSeenFeedback(cl, cl.reg)
	k := f.fb.keep()
	callee := &jitRegFeedback{fb: &jitFeedback{r: r, fn: cl.fn, cl: cl, e: r.jitState().regLearnedOf(r.jitKeyOf(cl.fn)), root: k}, rc: cl.fn.RegCode(), p: p}
	in := ssa.InlineSite{
		Program: p, Feedback: callee, Callee: site.Callee, Closure: site.Closure,
		Argc: argc, Method: true, Params: 2 + int(cl.reg.params), ThisSlot: -1, Window: true,
		Coerce: site.Coerce, Upvalues: jitUpvalueCells(cl),
	}
	k.holders = append(k.holders, o)
	k.inlined = append(k.inlined, cl)
	if l != nil && !slices.ContainsFunc(l.inlines, func(y jitInline) bool { return y.pc == x.pc && y.obj == o }) {
		// Where its exits are counted (jitInlineLeft).
		l.inlines = append(l.inlines, jitInline{pc: x.pc, cl: cl, obj: o})
	}
	return in, true
}

// jitRegLoopLeft is jitRegLeft for e's code, run on f's window at wb, that
// left in ctx inside a loop of a built-in's call (abi.Context's Loop) -- a
// check of the loop's, its back edge's poll, its callback's call, which
// Go made (regPending) or which had no native code to call: Go goes on
// with the loop from its index (jitRegLoopFinish), as V8's builtin
// continuation frames go on in the builtin, the callback's answer first
// where the call was made, then the call's result to its register, and
// native code goes on after the call where it has an entry there.
func (r *Runtime) jitRegLoopLeft(f *frame, c *regCode, e *jitRegEntry, wb int, ctx *abi.Context) (next, pc int, v Value, done bool, err error, native bool) {
	s := r.jit
	exitPC := int(ctx.ExitPC)
	loop := ctx.Loop
	ctx.Loop = 0
	r.jitRegMaterialize(f, e, wb)
	after, res := false, Undefined
	var lerr error
	if p := s.regPending; p.set && p.pc == exitPC && p.ctx == int(ctx.Level) && !p.whole {
		// The callback's call, which Go made or finished: its answer.
		s.regPending = jitGoPending{}
		after, res, lerr = true, p.v, p.err
	}
	switch ctx.ExitKind {
	case abi.ExitDeopt:
		s.guards++
		r.jitRegInvalidate(f.cl, e)
	case abi.ExitPoll:
		s.budgets++
		r.backEdges = s.backEdgeBudget()
		if lerr == nil {
			lerr = r.checkInterruptNow()
		}
	default:
		s.hosts++
	}
	res, lerr = r.jitRegLoopFinish(f, c, e, wb, exitPC, loop, after, res, lerr)
	next, ran, ferr := r.regFinishCall(f, c, wb, exitPC, res, lerr)
	switch {
	case !ran:
		s.interpreted++
		return 0, exitPC, Undefined, false, nil, false
	case ferr != nil:
		return 0, 0, Undefined, false, ferr, false
	case r.stopped != nil:
		return 0, 0, Undefined, false, r.stopped, false
	}
	return next, 0, Undefined, false, nil, true
}

// jitRegLoopFinish is jitLoopFinish on f's register frame, its window at
// wb, e's code having left at the loop's call, register pc rpc of c: the
// loop's slots past the window and the scratch slots, where native code
// keeps them (jitRegSlot), the frame's pc past the call meanwhile.
func (r *Runtime) jitRegLoopFinish(f *frame, c *regCode, e *jitRegEntry, wb, rpc int, loop uint64, after bool, v Value, err error) (Value, error) {
	p := e.p
	at := jitRegSlotIndex(p, wb, p.Locals+int(loop&0xffffffff)-1)
	return r.jitLoopFinishAt(f, at, regSrc(c, rpc)+1, loop, after, v, err)
}

// jitRegSlotIndex is where in the VM's stack slot i of p, past its Locals,
// is in a register frame whose window is at wb (jitRegSlot).
func jitRegSlotIndex(p *ir.Program, wb, i int) int {
	scratch := p.Locals - (p.UpvalueBase + p.Upvalues)
	return wb + p.Regs + scratch + i - p.Locals
}
