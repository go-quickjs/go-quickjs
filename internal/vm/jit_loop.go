//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64))

package vm

import (
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
// (ssa.ForEachSite), its callback called natively: the call learned at its
// call of Go (jitRegForEachSeen), answered by jitRegFeedback's ForEach, and
// a loop that leaves native code finished by Go from its index
// (jitRegLoopLeft), then native code after the call.

// jitRegForEachSeen learns, after Go made the call at register pc rpc of
// f's code e -- call_method_w of argc arguments, callee on recv -- that it
// is a loop's built-in given cb: the next compile makes it a loop of its
// own (ForEach), to be compiled again for it (jitRegReopt), the callback
// compiled for native callers (jitRegNativeCallee). One whose callback is
// another function's, or whose built-in or typed arrays' prototype is
// another, the call is Go's, as a call of too many targets is; one given a
// new closure of the same function each time calls it by its code
// (ssa.CallSite's ByCode). A sort, which native code makes only with its
// comparator inlined (ssa's sortCall), is learned so only of a comparator
// it may call again at will (jitRegSortComparator), and not of a BigInt
// typed array's, whose elements its loop does not compare.
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
	if fd == nil || fd.native != nil || fd.bound || fd.closure == nil || fd.closure.realm != r.Realm {
		return
	}
	cl := fd.closure
	if cl.reg == nil && r.regOf(cl) == nil {
		// A callback only the built-in has called, which made its frame
		// without the register code a call of it would get -- a sort's
		// comparator -- given it.
		return
	}
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
			l.nativeCalls = jitLearnedByCode(l.nativeCalls, i)
			r.jitRegReopt(f.cl, e)
		}
		r.jitRegNativeCalleeN(cl, cl.reg, calls, f.cl)
		return
	}
	l.nativeCalls = append(l.nativeCalls, jitInline{pc: int32(rpc), obj: o, forEach: true, cb: cl, cbObj: cb.Object(), proto: proto})
	// Decided: a loop of its own, marked so in callSites -- left undecided,
	// its callback's unwinding had the code compiled again (jitRegUnwound)
	// rather than stop calling it.
	if c := f.cl.reg; c != nil && rpc < len(c.rc.Code) {
		if len(l.callSites) != len(c.rc.Code) {
			l.callSites = make([]uint8, len(c.rc.Code))
		}
		l.callSites[rpc] = jitCallNative
	}
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
	q := f.callCtx()
	return jitForEachOf(&q, pc, jitInlineLive{f})
}

// jitRegSortComparator reports, of fn's register code rc, whether fn, a
// sort's comparator, is a body native code may call again at will (ssa's
// sortCall), given numbers -- a function, an arrow or a method a call runs
// the body of, of the register form's own frame layout, no binding of its
// own past its parameters, whose every instruction reads its parameters,
// the temporaries it wrote, constants that are numbers, computes with an
// operator, compares, branches forward or returns: no read of its function
// or its this, no call, no property, no global, no captured binding, no
// write but of a temporary. Called with numbers, it makes only numbers,
// booleans, undefined and null, runs nothing and changes nothing.
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
			bytecode.RAndI, bytecode.ROrI, bytecode.RXorI, bytecode.RShlI, bytecode.RShrI, bytecode.RUShrI, bytecode.RAddI, bytecode.RSubI,
			bytecode.RMulI, bytecode.RAddII32:
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
		case bytecode.RSub, bytecode.RMul, bytecode.RBitAnd, bytecode.RBitOr, bytecode.RBitXor, bytecode.RShl, bytecode.RShr, bytecode.RUShr,
			bytecode.RAddI32, bytecode.RSubI32:
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
			bytecode.RJSEqI, bytecode.RJSNeI, bytecode.RJEqI, bytecode.RJNeI, bytecode.RJNull, bytecode.RJNotNull, bytecode.RJBool:
			ok = read(w.A()) && forward(pc, w.SC())
		case bytecode.RStepJLt, bytecode.RStepJLe:
			// A step and its partner, which is checked as itself.
			ok = write(w.A()) && read(w.A()) && read(w.B()) && forward(pc, w.SC())
		case bytecode.RStepJLtI, bytecode.RStepJGtI, bytecode.RStepJGeI:
			ok = write(w.A()) && read(w.A()) && forward(pc, w.SC())
		default:
			ok = false
		}
		if !ok {
			return false
		}
	}
	return true
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
		// A check of the loop's: at its site's top, Go's call (ForEach).
		r.jitRegSiteDeopted(f, c, e, wb, exitPC, jitSiteGuard)
		r.jitRegInvalidate(f.cl, e, "loop", jitTierSite{nil, exitPC})
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

// jitRegLoopFinish is jitLoopFinishAt on f's register frame, its window at
// wb, e's code having left at the loop's call, register pc rpc of c: the
// loop's slots past the window and the scratch slots, where native code
// keeps them (jitRegSlot), the frame's pc past the call meanwhile.
func (r *Runtime) jitRegLoopFinish(f *frame, c *regCode, e *jitRegEntry, wb, rpc int, loop uint64, after bool, v Value, err error) (Value, error) {
	p := e.p
	at := jitRegSlotIndex(p, wb, p.Locals+int(loop&0xffffffff)-1)
	return r.jitLoopFinishAt(f, at, uint32(rpc)+1, loop, after, v, err)
}

// jitRegSlotIndex is where in the VM's stack slot i of p, past its Locals,
// is in a register frame whose window is at wb (jitRegSlot).
func jitRegSlotIndex(p *ir.Program, wb, i int) int {
	scratch := p.Locals - jitRegScratchBase(p)
	return wb + p.Regs + scratch + i - p.Locals
}

// jitRegPushLoopFrame pushes the frame of the built-in whose call register
// code makes a loop of (abi.Context's Loop, loop), its callback's caller,
// as Go's forEach has a frame while its callback runs (nativeFrame), for
// the frames Go makes of a callback inlined in the loop, which a stack
// trace shows: "at Array.forEach". Its receiver is arr, the loop's array,
// or, where the slot holds none, the prototype of the loop's kind, which
// the trace names the frame by all the same.
func (r *Runtime) jitRegPushLoopFrame(loop uint64, arr Value) {
	kind := int(loop >> 32)
	fe := r.jitLoopFn(kind)
	this := Obj(r.proto.array)
	if t := kind >> abi.LoopTypedShift; kind&abi.LoopTyped != 0 && t < len(r.typedArrayProtos) && r.typedArrayProtos[t] != nil {
		this = Obj(r.typedArrayProtos[t])
	}
	if arr.IsObject() {
		this = arr
	}
	nf := r.pushFrame()
	nf.cl, nf.native, nf.this, nf.newTarget, nf.callee, nf.args = nil, fe.fn().name, this, Undefined, fe, nil
	if len(nf.handlers) != 0 {
		nf.handlers = nf.handlers[:0]
	}
	if len(nf.openUpvalues) != 0 {
		nf.openUpvalues = nf.openUpvalues[:0]
	}
}
