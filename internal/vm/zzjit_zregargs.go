//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64))

package vm

import (
	"unsafe"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
	"github.com/go-quickjs/go-quickjs/internal/jit/abi"
	jitcompile "github.com/go-quickjs/go-quickjs/internal/jit/compile"
	"github.com/go-quickjs/go-quickjs/internal/jit/ssa"
)

// Register callees that keep their arguments (regCode's args), inlined
// with their call's arguments, as the stack form inlines them (jitFeedback's
// Inline, forwardInline): one that reads them by index and counts them
// alone (jitRegReadsArguments) reads the call's (ssa.InlineSite's
// ReadsArguments), and one that hands them on, f.apply(x, arguments)
// (jitRegForwards), is inlined with them (Arguments), its apply_args the
// call of f with them, f inlined in its turn (Forward). The frame Go makes
// of such a callee's level at an exit inside it is given the arguments
// from its caller's call's window, or from the frame that forwarded them
// (jitRegInlineArgs).

// jitRegReadsArguments reports whether cl's register code keeps its
// arguments only to read them by index and count them (args_index,
// args_len): no arguments object made (arguments), none handed on
// (apply_args), no rest parameter, and no parameter a sloppy function's
// object would alias -- as the stack form's jitReadsArguments. A closure
// no call ran yet, a template of one its maker makes (madeInline), is
// asked by its function's register code, which its first call gets.
func jitRegReadsArguments(cl *closure) bool {
	rc := cl.fn.RegCode()
	if cl.reg != nil {
		rc = cl.reg.rc
	}
	if rc == nil || !(rc.UsesArguments || rc.ForwardsArguments) || rc.KeepsArgs || cl.fn.MappedArguments && cl.fn.ParamCount > 0 {
		return false
	}
	for _, w := range rc.Code {
		switch w.Op() {
		case bytecode.RArguments, bytecode.RApplyArgs:
			return false
		}
	}
	return true
}

// jitRegForwards reports whether cl's register code hands its arguments on,
// f.apply(x, arguments) (apply_args), as they were given (compile's
// ForwardsNatively), and makes no arguments object: what reads them besides
// reads them by index and counts them, of no parameter an object would
// alias -- the stack form's jitForwardsCall.
func jitRegForwards(cl *closure) bool {
	c := cl.reg
	if c == nil || !c.args || c.rc.KeepsArgs || !jitcompile.ForwardsNatively(cl.fn) {
		return false
	}
	forwards, reads := false, false
	for _, w := range c.rc.Code {
		switch w.Op() {
		case bytecode.RArguments:
			return false
		case bytecode.RApplyArgs:
			forwards = true
		case bytecode.RArgsIndex:
			reads = true
		}
	}
	return forwards && !(reads && cl.fn.MappedArguments && cl.fn.ParamCount > 0)
}

// jitRegForwardTarget is the function the apply_args at register pc rpc
// of cl's code hands its arguments to, inlined where cl is with argc of
// them: for a construction of ctor, k of this.k.apply(this, arguments), a
// data property of ctor's prototype, as the stack form's
// jitForwardSiteTarget finds it; else the one function cl's own code
// learned it calls there, its forwards settled (jitRegForwardSeen), as
// jitForwardLearned; else, for a construction, f as the read of it finds
// it now (jitRegForwardGuess), as jitForwardSiteTarget's jitChainValue. A
// plain call is never inlined for a guess, as the stack form's is not:
// the forwarder's closures a call meets each hand their arguments to a
// function of their own -- forward_poly's four -- and inlined for the f
// of the one the call's cache held, the call left native code at three
// turns in four. A function native callers may call, of cl's realm and
// scope; or nil.
func (r *Runtime) jitRegForwardTarget(cl *closure, ctor *Object, rpc, argc int) *Object {
	c := cl.reg
	if c == nil || rpc >= len(c.rc.Code) || c.rc.Code[rpc].Op() != bytecode.RApplyArgs || r.applyFn == nil {
		return nil
	}
	var t *Object
	w := c.rc.Code[rpc]
	if ctor != nil && rpc >= 3 {
		if get := c.rc.Code[rpc-3]; get.Op() == bytecode.RGetProp && get.A() == w.B() && get.B() == 1 {
			// this.k, found on the construction's prototype.
			if key, ok := jitRegSiteName(cl, c.rc, int(get.C())); ok {
				if p := ctor.getOwnVisible(atomPrototype); p.ok() && p.flags()&propAccessor == 0 && p.value().IsObject() {
					if m := p.value().Object().getOwnVisible(key); m.ok() && m.flags()&propAccessor == 0 && m.value().IsObject() {
						t = m.value().Object()
					}
				}
			}
		}
	}
	if t == nil {
		var learned []jitInline
		if l := r.jit.regLearned[r.jitRegKeyOf(cl)]; l != nil && rpc < len(l.callSites) && l.callSites[rpc] == jitCallNative {
			learned = l.nativeCalls
		}
		for _, x := range learned {
			if int(x.pc) != rpc {
				continue
			}
			if t != nil || x.byCode || x.cl == nil || x.obj == nil {
				return nil
			}
			t = x.obj
		}
	}
	if t == nil && ctor != nil {
		t = r.jitRegForwardGuess(cl, rpc)
	}
	if t == nil || !r.jitNativeTargetOf(t.fn(), cl, false) {
		return nil
	}
	tc := t.fn().closure
	if tc.fn == cl.fn || tc.reg == nil || tc.reg.rc != tc.fn.RegCode() || !r.jitRegCallable(t, tc, tc.reg, argc) {
		return nil
	}
	return t
}

// jitRegForwardsInline reports whether cl, a forwarding function
// (jitRegForwards), may be inlined with argc arguments of its call, a
// construction's of ctor or, nil, a plain call's: each function it hands
// them on to is known (jitRegForwardTarget) and keeps none of them but to
// read them or hand them on, in its turn, depth levels on.
func (r *Runtime) jitRegForwardsInline(cl *closure, ctor *Object, argc, depth int) bool {
	if depth <= 0 || jitNoInline != nil && jitNoInline(cl.fn) {
		return false
	}
	any := false
	for rpc, w := range cl.reg.rc.Code {
		if w.Op() != bytecode.RApplyArgs {
			continue
		}
		any = true
		t := r.jitRegForwardTarget(cl, ctor, rpc, argc)
		if t == nil {
			return false
		}
		tc := t.fn().closure
		if tc.reg.args && !jitRegReadsArguments(tc) && (!jitRegForwards(tc) || !r.jitRegForwardsInline(tc, nil, argc, depth-1)) {
			return false
		}
	}
	return any
}

// jitRegSiteName is the key of property site n of cl's register code rc
// (RegCode's SiteName).
func jitRegSiteName(cl *closure, rc *bytecode.RegCode, n int) (Atom, bool) {
	if n >= len(rc.SiteName) || int(rc.SiteName[n]) >= len(cl.names) {
		return 0, false
	}
	return cl.names[rc.SiteName[n]], true
}

// forwardInline is the call apply_args at the program's instruction pc of
// an inlined forwarding callee (f.fwd) makes: of the function it hands its
// arguments on to (jitRegForwardTarget), inlined with the arguments its
// own call gave it (ssa.InlineSite's Forward), as the stack form's
// forwardInline -- one that reads them, so (ReadsArguments), one that
// forwards them in turn, so too (Arguments).
func (f *jitRegFeedback) forwardInline(pc int) (ssa.InlineSite, bool) {
	fb := f.fb
	r := fb.r
	fwd := f.fwd
	rpc := int(f.p.Origin[pc])
	t := r.jitRegForwardTarget(fb.cl, fwd.ctor, rpc, fwd.argc)
	if t == nil {
		return ssa.InlineSite{}, false
	}
	tc := t.fn().closure
	if tc.fn == fb.keep().fn || jitNoInline != nil && jitNoInline(tc.fn) {
		return ssa.InlineSite{}, false
	}
	kind, reads, forwards := jitLowerInline, false, false
	if tc.reg.args {
		switch {
		case jitRegReadsArguments(tc):
			reads = true
		case jitRegForwards(tc) && r.jitRegForwardsInline(tc, nil, fwd.argc, jitInlineDepth):
			kind, forwards = jitLowerForward, true
		default:
			return ssa.InlineSite{}, false
		}
	}
	p, err := r.jitRegLower(tc, tc.reg.rc, kind)
	if err != nil {
		return ssa.InlineSite{}, false
	}
	r.jitRegSeenInlined(fb.keep(), tc)
	k := fb.keep()
	callee := &jitRegFeedback{fb: &jitFeedback{r: r, fn: tc.fn, cl: tc, e: r.jitState().regLearnedOf(r.jitRegKeyOf(tc)), root: k}, rc: tc.fn.RegCode(), p: p}
	if forwards {
		callee.fwd = &jitForwardFrame{argc: fwd.argc}
	}
	k.holders = append(k.holders, t, r.applyFn)
	k.inlined = append(k.inlined, tc)
	return ssa.InlineSite{
		Program: p, Feedback: callee, Callee: uintptr(unsafe.Pointer(t)), Closure: uintptr(unsafe.Pointer(tc)),
		Argc: fwd.argc, Params: 2 + int(tc.reg.params), ThisSlot: -1, Window: true,
		Forward: true, Apply: uintptr(unsafe.Pointer(r.applyFn)), Arguments: forwards, ReadsArguments: reads,
		Upvalues: jitUpvalueCells(tc),
		// x may be a primitive, or undefined, which a sloppy callee's this
		// is coerced from by Go; a construction's is its object.
		Coerce: tc.reg.coerceThis,
	}, true
}

// jitRegInlineArgs is the arguments of the frame Go makes of an inlined
// callee's level that keeps them (regCode's args), whose caller's code c,
// its window at cwb, made the call at register pc at: those of the call's
// window -- call_w's, call_method_w's, new_w's -- or, for apply_args, the
// arguments of the caller's own frame, args, which it hands on.
func (r *Runtime) jitRegInlineArgs(c *regCode, cwb, at int, args []Value) []Value {
	if at >= len(c.rc.Code) {
		return nil
	}
	switch w := regAt(c.code, at); jitRegCallOp(w.Op()) {
	case bytecode.RCallW, bytecode.RCallMW, bytecode.RNewW:
		a := cwb + int(w.A()) + 2
		return append([]Value(nil), r.stack[a:a+int(w.X())]...)
	case bytecode.RApplyArgs:
		return append([]Value(nil), args...)
	}
	return nil
}

// jitRegForwardGuess is f of the apply_args at register pc rpc of cl's
// code, f.apply(x, arguments), as the read of f just before it finds it
// now -- a global's, a captured binding's -- as the stack form guesses a
// forwarding constructor's (jitForwardSiteTarget's jitChainValue): what a
// forwarding construction inlined hands its arguments to, its call
// inlined, which checks it is. Or nil.
func (r *Runtime) jitRegForwardGuess(cl *closure, rpc int) *Object {
	rc := cl.reg.rc
	if rpc < 3 {
		return nil
	}
	w, apply, get := rc.Code[rpc], rc.Code[rpc-2], rc.Code[rpc-3]
	if apply.Op() != bytecode.RGetProp || apply.A() != w.B()+1 || apply.B() != w.B() || get.A() != w.B() {
		return nil
	}
	var v Value
	switch get.Op() {
	case bytecode.RGetGlobal:
		i := uint32(get.B())
		if get.B() == bytecode.RNameFar {
			if rpc-3 >= len(rc.FarNames) {
				return nil
			}
			i = rc.FarNames[rpc-3]
		}
		if int(i) >= len(cl.names) {
			return nil
		}
		var ok bool
		if v, ok = r.jitGlobalValue(cl, cl.names[i]); !ok {
			return nil
		}
	case bytecode.RGetUpval, bytecode.RGetUpvalCheck:
		k := int(get.BC())
		if k >= len(cl.upvalues) || cl.upvalues[k] == nil {
			return nil
		}
		v = cl.upvalues[k].get()
	default:
		return nil
	}
	if !v.IsObject() {
		return nil
	}
	return v.Object()
}

// jitRegForwardMade makes, for native code, the apply_args at register pc
// rpc of run's frame, f.apply(x, arguments) of the realm's apply, which the
// code checked, f, apply and x its call's operands (ssa.CallSite's Forward
// of Go): f called with x and the frame's arguments, from the context its
// call or its entry wrote them to (jitForwardEntry), as the stack form's
// call of Go makes it (jitForwardMade) -- or the frame's own past what a
// context holds. What it calls the code learns, to call natively once
// compiled again (jitRegCallSeen).
func (r *Runtime) jitRegForwardMade(ctx *abi.Context, idx int, run jitRegRun, rpc int) bool {
	s := r.jit
	f, c := run.f, run.c
	var args []Value
	switch {
	case ctx.Argc <= uint64(len(ctx.Args)):
		args = jitContextArgs(ctx)
	case f.args != nil && len(f.args) == int(ctx.Argc):
		args = f.args
	default:
		return false
	}
	target, this := jitGoArg(ctx, 0), jitGoArg(ctx, 2)
	if run.e != nil && run.e.life != nil {
		run.e.life.ssaStats.goCalls++
	}
	s.goCalls++
	r.noteRegGoCall(regAt(c.code, rpc), false, target)
	f.pc = regSrc(c, rpc) + 1
	last, shared := s.ssaCtx, s.ssaSharedGen
	goCallDepth := s.goCallDepth
	s.goCallDepth = r.frameDepth + 1
	// A copy: a native call's next call writes the context over.
	i, n := len(r.argStack), len(args)
	r.argStack = append(r.argStack, args...)
	v, err := r.applyCall(target, this, r.argStack[i:i+n:i+n])
	clear(r.argStack[i:])
	r.argStack = r.argStack[:i]
	s.goCallDepth = goCallDepth
	s.ssaCtx = last
	if s.ssaSharedGen != shared {
		r.jitShareContexts(idx, f)
	}
	if err == nil && r.stopped == nil {
		r.jitRegCallSeenOf(f, c, run.e, rpc, target, true)
	}
	if err != nil || r.stopped != nil || run.e.invalid || run.leave || r.jitRegCompileDueNow(&run, idx) {
		s.regPending = jitGoPending{set: true, pc: rpc, ctx: idx, v: v, err: err}
		return false
	}
	*(*Value)(unsafe.Pointer(&ctx.Keep[ctx.GoKeep])) = v
	return true
}
