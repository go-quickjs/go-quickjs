//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64))

package vm

import (
	"slices"
	"unsafe"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
	"github.com/go-quickjs/go-quickjs/internal/jit/abi"
	"github.com/go-quickjs/go-quickjs/internal/jit/ir"
	"github.com/go-quickjs/go-quickjs/internal/jit/ssa"
)

// Register code's loops of built-ins' calls (README "Array loops"): a call
// of Array.prototype's forEach, reduce, map, filter, some, every, find or
// findIndex, or a typed array's method of those names -- or a sort, its
// comparator inlined (ssa's sortCall) -- that its code's calls of Go have
// made on an array of the realm's, given a closure of the realm's as its
// callback, is made a loop of the code's own by the builder
// (ssa.ForEachSite), its callback called natively, as the stack form's is:
// the call learned at its call of Go (jitRegForEachSeen), answered by
// jitRegFeedback's ForEach, and a loop that leaves native code finished by
// Go from its index (jitRegLoopLeft), then native code after the call.

// jitRegForEachSeen learns, after Go made the call at register pc rpc of
// f's code e -- call_method_w of argc arguments, callee on recv -- that it
// is a loop's built-in given cb, as the stack form's code learns it
// (jitForEachSeen): the next compile makes it a loop of its own (ForEach),
// to be compiled again for it (jitRegReopt), the callback compiled for native
// callers (jitRegNativeCallee). One whose callback is another function's,
// or whose built-in or typed arrays' prototype is another, the call is
// Go's, as a call of too many targets is; one given a new closure of the
// same function each time calls it by its code (ssa.CallSite's ByCode). A
// sort, which native code makes only with its comparator inlined (ssa's
// sortCall), is learned so only of a comparator it may call again at will
// (jitRegSortComparator), and not of a BigInt typed array's, whose
// elements its loop does not compare, as the stack form's (jitForEaches).
func (r *Runtime) jitRegForEachSeen(f *frame, e *jitRegEntry, rpc int, callee, recv, cb Value, argc int) {
	if argc != 1 && argc != 2 || !callee.IsObject() || !cb.IsObject() || e.invalid {
		return
	}
	o := callee.Object()
	kind := r.jitLoopKind(o)
	var proto *Object
	// The callback's calls the built-in makes, about: one for each
	// element, which an invalidated callback spends its budget by
	// (jitRegNativeCalleeN).
	var calls int64
	sort := kind&abi.LoopKindMask == abi.LoopSort
	switch {
	case kind == 0:
		return
	case kind&abi.LoopTyped != 0:
		t := r.jitLoopTyped(recv)
		if t == nil || sort && t.info().big {
			return
		}
		proto = recv.Object().proto()
		calls = int64(t.length)
	case r.plainArray(recv) == nil:
		return
	default:
		calls = int64(len(recv.Object().elemsOf()))
	}
	calls = max(calls, 1)
	fd := cb.Object().fn()
	if fd == nil || fd.native != nil || fd.bound || fd.closure == nil || fd.closure.realm != r.Realm || fd.closure.reg == nil {
		return
	}
	cl := fd.closure
	if sort && !jitRegSortComparator(cl.fn, cl.reg.rc) {
		return
	}
	l := r.jitRegLearnedFor(f, e)
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
		r.jitRegNativeCalleeN(cl, cl.reg, calls, f.cl)
		return
	}
	l.nativeCalls = append(l.nativeCalls, jitInline{pc: int32(rpc), obj: o, forEach: true, cb: cl, cbObj: cb.Object(), proto: proto})
	r.jitRegNativeCalleeN(cl, cl.reg, calls, f.cl)
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
		if kind == 0 {
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
		switch kind & abi.LoopKindMask {
		case abi.LoopReduce:
			argc = 4
		case abi.LoopSort:
			// A comparator, called with two elements; native code sorts
			// only with one it may call again at will.
			argc = 2
			if !jitRegSortComparator(x.cb.fn, x.cb.reg.rc) {
				return ssa.ForEachSite{}, false
			}
		}
		site, ok := f.regCallSite(pc, x.cbObj, x.cb, x.cb.reg, argc, true, nil, nil, x.byCode, false)
		if !ok {
			return ssa.ForEachSite{}, false
		}
		k.holders = append(k.holders, x.obj)
		fe := ssa.ForEachSite{Fn: uintptr(unsafe.Pointer(x.obj)), Kind: kind, Proto: proto, Callback: site}
		if in, ok := f.callbackInline(x, site, argc); ok {
			fe.Inline = &in
			fe.Made = f.madeCallback(pc, x.cb)
		}
		return fe, true
	}
	return ssa.ForEachSite{}, false
}

// jitRegSortComparator is jitSortComparator over fn's register code rc:
// whether fn, a sort's comparator, is a body native code may call again
// at will (ssa's sortCall), given numbers -- a function, an arrow or a
// method a call runs the body of, of the register form's own frame
// layout, no binding of its own past its parameters, whose every
// instruction reads its parameters, the temporaries it wrote, constants
// that are numbers, computes with an operator, compares, branches
// forward or returns: no read of its function or its this, no call, no
// property, no global, no captured binding, no write but of a
// temporary. Called with numbers, it makes only numbers, booleans,
// undefined and null, runs nothing and changes nothing.
func jitRegSortComparator(fn *bytecode.Function, rc *bytecode.RegCode) bool {
	if fn == nil || rc == nil || !rc.Wide || !fn.DirectCall || fn.Kind != bytecode.KindNormal && fn.Kind != bytecode.KindArrow && fn.Kind != bytecode.KindMethod ||
		fn.Generator || fn.Async || fn.HasRest || fn.HasDirectEval || !fn.HasSimpleParams || fn.ParamsAreLexical ||
		fn.UsesArguments || fn.UsesThis || fn.LocalCount != fn.ParamCount || rc.Params != fn.ParamCount || rc.Inits != 0 ||
		rc.This >= 0 || rc.ThisRef || rc.UsesArguments || rc.ForwardsArguments || rc.KeepsArgs || len(rc.Handlers) != 0 ||
		rc.LocalReg != nil || len(rc.Code) > 64 {
		return false
	}
	consts := fn.Constants
	if rc.Constants != nil {
		consts = rc.Constants
	}
	temp := 2 + rc.Params
	// A register read: a parameter or a temporary; one written: a
	// temporary.
	read := func(x uint16) bool { return int(x) >= 2 && int(x) < rc.NumRegs }
	write := func(x uint16) bool { return int(x) >= temp && int(x) < rc.NumRegs }
	operator := func(op bytecode.Op) bool {
		switch op {
		case bytecode.OpAdd, bytecode.OpSub, bytecode.OpMul, bytecode.OpDiv, bytecode.OpMod,
			bytecode.OpBitAnd, bytecode.OpBitOr, bytecode.OpBitXor, bytecode.OpShl, bytecode.OpShr, bytecode.OpUShr,
			bytecode.OpLt, bytecode.OpLe, bytecode.OpGt, bytecode.OpGe, bytecode.OpEq, bytecode.OpNe, bytecode.OpStrictEq, bytecode.OpStrictNe:
			return true
		}
		return false
	}
	forward := func(pc int, off int32) bool { return off >= 0 && pc+1+int(off) <= len(rc.Code) }
	for pc, w := range rc.Code {
		ok := true
		switch w.Op() {
		case bytecode.RNop, bytecode.RRetUndef:
		case bytecode.RLoadInt, bytecode.RLoadUndef, bytecode.RLoadNull, bytecode.RLoadTrue, bytecode.RLoadFalse:
			ok = write(w.A())
		case bytecode.RLoadConst:
			k := int(w.BC())
			ok = write(w.A()) && k < len(consts) && consts[k].Kind == bytecode.ConstNumber
		case bytecode.RMove, bytecode.RInc, bytecode.RDec,
			bytecode.RAndI, bytecode.ROrI, bytecode.RXorI, bytecode.RShlI, bytecode.RShrI, bytecode.RUShrI, bytecode.RAddI, bytecode.RSubI:
			ok = write(w.A()) && read(w.B())
		case bytecode.RUnary:
			switch bytecode.Op(w.X()) {
			case bytecode.OpNeg, bytecode.OpPos, bytecode.OpToNumeric, bytecode.OpNot, bytecode.OpBitNot:
				ok = write(w.A()) && read(w.B())
			default:
				ok = false
			}
		case bytecode.RAdd:
			ok = w.X()&bytecode.RAddAppend == 0 && write(w.A()) && read(w.B()) && read(w.C())
		case bytecode.RSub, bytecode.RMul, bytecode.RBitAnd, bytecode.RBitOr, bytecode.RBitXor, bytecode.RShl, bytecode.RShr, bytecode.RUShr:
			ok = write(w.A()) && read(w.B()) && read(w.C())
		case bytecode.RBin:
			ok = operator(bytecode.Op(w.X())) && write(w.A()) && read(w.B()) && read(w.C())
		case bytecode.RRet:
			ok = read(w.A())
		case bytecode.RJmp:
			ok = forward(pc, int32(w.BC()))
		case bytecode.RJmpF, bytecode.RJmpT:
			ok = read(w.A()) && forward(pc, int32(w.BC()))
		case bytecode.RJLt, bytecode.RJNLt, bytecode.RJLe, bytecode.RJNLe, bytecode.RJGt, bytecode.RJNGt, bytecode.RJGe, bytecode.RJNGe,
			bytecode.RJSEq, bytecode.RJSNe, bytecode.RJEq, bytecode.RJNe:
			ok = read(w.A()) && read(w.B()) && forward(pc, w.SC())
		case bytecode.RJLtI, bytecode.RJNLtI, bytecode.RJLeI, bytecode.RJNLeI, bytecode.RJGtI, bytecode.RJNGtI, bytecode.RJGeI, bytecode.RJNGeI,
			bytecode.RJSEqI, bytecode.RJSNeI, bytecode.RJEqI, bytecode.RJNeI, bytecode.RJNull, bytecode.RJNotNull:
			ok = read(w.A()) && forward(pc, w.SC())
		default:
			ok = false
		}
		if !ok {
			return false
		}
	}
	return true
}

// callbackInline is the InlineSite of x's callback, a loop's (ForEach),
// for native code to inline in the loop, as V8 inlines a forEach's
// callback, under the rules a register callee's inlining follows
// (jitRegFeedback's Inline): the one closure the loop has been given,
// which the loop checks it is given, its window the loop's call's
// operands -- or, one made anew at each call of its maker (byCode), no
// object, for the builder to inline where the caller makes it just before
// the call (madeCallback, ssa's madeAt), as the stack form's -- not an
// arrow that reads its this, which may come from any frame, nor one whose
// inlining at the call left native code too often (jitInlineLeft's
// notInline), called natively since.
func (f *jitRegFeedback) callbackInline(x jitInline, site ssa.CallSite, argc int) (ssa.InlineSite, bool) {
	cl, o := x.cb, x.cbObj
	l := f.fb.e
	byCode := x.byCode || site.ByCode != 0
	if site.ArrowThis || cl.fn == f.fb.fn || cl.fn == f.fb.keep().fn || jitNoInline != nil && jitNoInline(cl.fn) ||
		l != nil && slices.Contains(l.notInline, x.pc) {
		return ssa.InlineSite{}, false
	}
	p, err := f.fb.r.jitRegLower(cl, cl.reg.rc, jitLowerInline)
	if err != nil {
		return ssa.InlineSite{}, false
	}
	r := f.fb.r
	r.jitRegSeenInlined(f.fb.keep(), cl)
	k := f.fb.keep()
	callee := &jitRegFeedback{fb: &jitFeedback{r: r, fn: cl.fn, cl: cl, e: r.jitState().regLearnedOf(r.jitRegKeyOf(cl)), root: k}, rc: cl.fn.RegCode(), p: p}
	in := ssa.InlineSite{
		Program: p, Feedback: callee, Callee: site.Callee, Closure: site.Closure,
		Argc: argc, Method: true, Params: 2 + int(cl.reg.params), ThisSlot: -1, Window: true,
		Coerce: site.Coerce, Upvalues: jitUpvalueCells(cl),
	}
	if byCode {
		// No object, the closure the caller makes: its captured bindings
		// the caller's own slots (ssa.InlineSite's Captured), the builder's.
		in.Callee, in.Closure, in.Upvalues = 0, uintptr(unsafe.Pointer(cl)), 0
		k.inlined = append(k.inlined, cl)
		return in, true
	}
	k.holders = append(k.holders, o)
	k.inlined = append(k.inlined, cl)
	if l != nil && !slices.ContainsFunc(l.inlines, func(y jitInline) bool { return y.pc == x.pc && y.obj == o }) {
		// Where its exits are counted (jitInlineLeft).
		l.inlines = append(l.inlines, jitInline{pc: x.pc, cl: cl, obj: o})
	}
	return in, true
}

// madeCallback is the closure the method call at the program's instruction
// pc is given, if its code makes it just before the call -- a closure of
// cl's function, closure_w writing the call's first argument's register,
// one or two register instructions before it -- which native code inlining
// the call need not make (ssa.MadeCallback), as the stack form's
// (jitFeedback's madeCallback): its template, which the code keeps, as the
// inlined frame's closure an exit names, and what it captures. Not an
// arrow that reads its maker's new.target or super (jitNeedsMaker). Nothing
// between its making and the call closes what it captures (ssa's madeAt).
func (f *jitRegFeedback) madeCallback(pc int, cl *closure) *ssa.MadeCallback {
	fb := f.fb
	call := f.p.Code[pc]
	if fb.cl == nil || !call.Window {
		return nil
	}
	rpc := f.p.Origin[pc]
	for q := pc - 1; q >= 0 && rpc-f.p.Origin[q] <= 2; q-- {
		in := f.p.Code[q]
		if in.Op != ir.Closure || in.Dest != call.Left.Slot+2 {
			continue
		}
		k := int(in.Key)
		if k >= len(fb.cl.consts) || k >= len(f.rc.Caps) {
			return nil
		}
		c := jitTemplateCell[struct{}](fb, &fb.cl.consts[k])
		t := c.template()
		if t == nil || t.fn != cl.fn || jitNeedsMaker(*c) {
			return nil
		}
		caps, ok := f.madeCaptures(t, k)
		if !ok {
			return nil
		}
		kp := fb.keep()
		kp.inlined = append(kp.inlined, t)
		kp.made = append(kp.made, t)
		return &ssa.MadeCallback{PC: q, Template: uintptr(unsafe.Pointer(c)), Closure: uintptr(unsafe.Pointer(t)), Captures: caps}
	}
	return nil
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
		e.life.ssaStats.guards++
		r.jitRegInvalidate(f.cl, e)
	case abi.ExitPoll:
		s.budgets++
		r.backEdges = s.backEdgeBudget()
		if lerr == nil {
			lerr = r.checkInterruptPoll()
		}
	default:
		s.hosts++
		e.life.ssaStats.hosts++
	}
	// The call's function, receiver and callback, in its window, as the
	// loop was given them.
	var callee, recv, cb Value
	w := regAt(c.code, exitPC)
	if a := wb + int(w.A()); a+2 < len(r.stack) {
		callee, recv, cb = r.stack[a], r.stack[a+1], r.stack[a+2]
	}
	res, lerr = r.jitRegLoopFinish(f, c, e, wb, exitPC, loop, after, res, lerr)
	if lerr == nil && r.stopped == nil {
		// What the loop was given, which the code learns as at a call of
		// Go that made it (jitRegForEachSeen): a callback made anew at
		// each call of the code, which the loop's check of its object
		// had left at; and the callback's calls Go made finishing the
		// loop, which it spends its budget by where it is invalidated.
		r.jitRegForEachSeen(f, e, exitPC, callee, recv, cb, int(w.X()))
	}
	next, ran, ferr := r.regFinishCall(f, c, wb, exitPC, res, lerr)
	switch {
	case !ran:
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
	scratch := p.Locals - jitRegScratchBase(p)
	return wb + p.Regs + scratch + i - p.Locals
}
