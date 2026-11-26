//go:build amd64 || arm64

package vm

import "github.com/go-quickjs/go-quickjs/internal/bytecode"

// native_run.go's, from the native dispatcher's calls of built-ins on, in a
// file of its own after the layout lock's pad BU (native_run_b_layoutpad.go),
// so that what the JIT's hooks add before it in native_run.go -- the frame
// stack's growth, a restart's and an end's tests of the JIT's entry --
// moves none of it (docs/optimizing.md, "The layout lock").

// nativeBuiltin is call_w's and call_method_w's call of the built-in its
// site cached (regCallNative) as the call handler makes it, a handler of
// the exit's protocol called with no exit (nativegen's light): rhCallW's
// call of it, regNative, in the top frame's state, which the code has
// made, without rhCallW's search of the cache the code has searched.
//
// A built-in it applies without a frame -- Math's functions, push and pop,
// the native leaves whose checks pass (regNative's first cases) -- is
// applied first, with none of the state an exit makes: it throws nothing,
// runs no code and reads nothing of the frame. Anything else is
// regNative's, which asks again.
func nativeBuiltin(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	if r := st.r; r.meter == nil {
		fd := st.cl.regCalls[w.C()].callee.fn()
		if fd.mathOp != 0 || fd.elemOp != 0 {
			base := st.wb + int(w.A())
			args := r.stack[base+2 : base+2+int(w.X())]
			v, ok := Undefined, false
			if fd.mathOp != 0 {
				v, ok = mathCall(fd.mathOp, args)
			} else {
				this := Undefined
				if w.Op().MethodCall() {
					this = r.stack[base+1]
				}
				if op := fd.elemOp; op < elemNativeFirst {
					v, ok = r.pureElemOp(op, this, args, true)
				} else if leafChecks[op](r, this, args) {
					var err error
					v, err = fd.native(r, this, args)
					ok = err == nil
				}
			}
			if ok {
				r.stack[st.wb+int(w.B())] = v
				return pc + 1
			}
		}
	}
	st.kept.pc = pc + 1
	st.f.pc = uint32(pc) + 1
	return st.done(st.r.regNative(st.cl.regCalls[w.C()].callee, w, st.wb))
}

// nativeBuiltinFn is nativeBuiltin's func value, set by init.
var nativeBuiltinFn func(*[1 << 16]Value, bytecode.RInstr, int, *regState) int

// nativeIterRes is the result of a generator's next(), called with no
// exit as nativeLit is, where the call handler stepped the generator
// itself (nativegen's genNext) and its yield or return gave the value to
// the call's register B: the value made the { value, done } object next
// returns (iterResult), done where pc is not 0. It throws nothing and runs
// no code.
func nativeIterRes(regs *[1 << 16]Value, w bytecode.RInstr, pc int, ctx *nativeCtx) {
	regs[w.B()] = Obj(ctx.st.r.iterResult(regs[w.B()], pc != 0))
}

// nativeCallMiss is call_w's and call_method_w's exit where the handler
// meets a callee other than its site's cache holds and of another
// template (ctx.miss): where it is a closure the handler
// enters (regCallKind), the cache is made the new callee's and the call
// made again, which the code then enters itself, rather than Go entering
// it in a code entry of its own (regCallMiss) -- a site that calls several
// functions in turn, an array of them or a method of several layouts --
// where the handler enters the callee with a frame or reads its leaf's
// answer itself.
// The cache holds the callee the second time, so that a call the handler
// leaves to Go anyway is its Go handler's then (rhCallW, rhCallWT for the
// calls in tail position whose handlers are call_w's). Anything else is
// the Go handler's.
func nativeCallMiss(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	r, f, c := st.r, st.f, st.c
	win := regWindow(r.stack, st.wb+int(w.A()))
	calls := f.cl.regCalls
	if uint(w.C()) >= uint(len(calls)) || !regIsObject(&win[0]) {
		return regTable[w.Op()](regs, w, pc, st)
	}
	e, o := &calls[w.C()], (*Object)(win[0].ref)
	method := w.Op().MethodCall()
	if e.callee == o || e.kind == regCallGen || e.kind == regCallMarked || method && pc < len(c.rc.Marks) && c.rc.Marks[pc] != 0 {
		return regTable[w.Op()](regs, w, pc, st)
	}
	if fd := o.fn(); fd != nil && fd.closure != nil && !fd.bound {
		// A callee the handler enters with a frame, and a leaf whose
		// answer the handler reads itself, from the regcomp's
		// classification -- a parameter, a constant, a captured binding
		// (regLeafArg, regLeafConst, leafUp) -- which the handler then
		// answers with no exit, where Go's would have run the body. Other
		// leaves are rhCallW's miss's (pureCall, leafCall, regLeafRun),
		// and a frame that keeps its arguments is entered by regEnterW,
		// each cheaper than the exit the call would come back to. One with
		// native code the handler has Go enter (nativeJITCall), as
		// rhCallW's hit does. The feedback dispatcher of a runtime that
		// runs the JIT counted the call where it dispatched it, and counts
		// the call made again as it dispatches it again: the first count
		// is taken back (jitRegUncount), so that the site's frequency, which
		// the JIT inlines by, counts the call once.
		if cl, rc, kind, this, enter := r.regCallKind(fd, method, c); enter && kind != 0 && (rc.leaf == regLeafNone || rc.leaf >= regLeafArg || rc.leafUp != 0) && !rc.args && !regTypedCross(rc, c) && (kind != regCallObjThis || win[1].IsObject()) {
			if !f.cl.regCallsHeld {
				r.regCallsHold(f.cl.closureCode)
			}
			regCacheCallee(e, o, cl, rc, this, kind, c)
			if jitBuilt && r.jitOn() {
				f.cl.closureCode.jitRegUncount(int(w.C()))
			}
			return pc
		}
	}
	return regTable[w.Op()](regs, w, pc, st)
}

// nativeCallMissFn is nativeCallMiss's func value, set by init.
var nativeCallMissFn func(*[1 << 16]Value, bytecode.RInstr, int, *regState) int

// nativeJITCall is call_w's and call_method_w's call of a callee its
// site's cache holds whose calls go to the JIT (closureCode's jitEnters):
// one with native code made from its register code, or whose runs have
// spent what was left of its budget (jitRegCallTier). The call handler
// calls it through callH with no exit, as it calls nativeBuiltin, its
// checks of the callee and its this made: rhCallW's entry of the callee
// -- its frame, then jitRegEnter -- without rhCallW's search of the cache
// the code has searched. A callee rhCallW leaves to regCallMiss (a leaf, a
// frame that keeps its arguments) is rhCallW's.
func nativeJITCall(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	r, f, wb := st.r, st.f, st.wb
	e := &f.cl.regCalls[w.C()]
	if e.kind&(regCallOut|regCallLeaf) != 0 {
		return rhCallW(regs, w, pc, st)
	}
	st.kept.pc = pc + 1
	f.pc = uint32(pc) + 1
	base := wb + int(w.A())
	win := regWindow(r.stack, base)
	if e.kind == regCallThis {
		win[1] = e.this
	}
	o, cl, rc := e.callee, e.cl, e.rc
	argc := int(w.X())
	top := base + int(rc.numRegs)
	if top > len(r.stack) || r.frameDepth >= r.maxFrames {
		return st.done(r.throwRangeError("maximum call stack size exceeded"))
	}
	if r.interruptCounter--; r.interruptCounter <= 0 {
		if err := r.checkInterruptNow(); err != nil {
			return st.done(err)
		}
	}
	saved := r.stackTop
	if top > saved {
		r.stackTop = top
		if top > r.stackHigh {
			r.stackHigh = top
		}
	}
	if argc > int(rc.keep) {
		r.regKeepWindow(base, int(rc.keep), argc)
	}
	for i := min(argc, int(rc.params)); i < int(rc.params+rc.inits); i++ {
		win[2+i] = Undefined
	}
	off := r.frameDepth - r.frameBase
	if uint(off) >= uint(len(r.cur)) {
		r.seekFrameBlock()
		off = r.frameDepth - r.frameBase
	}
	r.frameDepth++
	if r.frameDepth > r.frameHigh {
		r.frameHigh = r.frameDepth
	}
	nf := &r.cur[off]
	nf.cl = cl
	nf.callee = o
	nf.base = base
	nf.pc = 0
	if len(nf.openUpvalues) != 0 {
		nf.openUpvalues = nf.openUpvalues[:0]
	}
	if nf.thisRef != nil || nf.withScopes != nil || nf.evalVars != nil || nf.native != "" || nf.args != nil {
		regClearFrame(nf)
	}
	if rc.newTarget {
		nf.newTarget = Undefined
	}
	nf.regKind = regFrameCall
	v, err := r.jitRegEnter(nf, cl, rc, base, 0)
	nf.regKind = 0
	if rc.ncap != 0 && len(nf.openUpvalues) != 0 {
		regCloseUpvalues(nf)
	}
	r.frameDepth--
	r.stackTop = saved
	if err != nil {
		return st.done(r.regCallWEnd(nf, regFrameCall, err, saved, wb+int(w.B())))
	}
	regWindow(r.stack, wb)[w.B()] = v
	return pc + 1
}

// nativeJITCallFn is nativeJITCall's func value, set by init.
var nativeJITCallFn func(*[1 << 16]Value, bytecode.RInstr, int, *regState) int
