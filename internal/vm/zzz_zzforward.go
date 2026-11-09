//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))

package vm

import (
	"slices"
	"unsafe"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
	"github.com/go-quickjs/go-quickjs/internal/jit/abi"
	jitcompile "github.com/go-quickjs/go-quickjs/internal/jit/compile"
	"github.com/go-quickjs/go-quickjs/internal/jit/ssa"
)

// Forwarding calls: f.apply(x, arguments) in a function whose arguments
// object is never made (bytecode's ForwardsArguments, apply_arguments),
// made by native code as V8's TurboFan makes JSCallForwardVarargs -- a
// call of f with the arguments the frame was given, copied from where they
// are to the callee's frame, their count read at run time
// (ssa.CallSite's Forward). A native callee's are in its context, where
// its call wrote them (jitFrameArgs); a frame Go enters the code with has
// them in the VM's frame, which Go copies to the context at each entry
// (jitForwardEntry).

// forwardCalls is NativeCalls' answer for the apply_arguments at pc: the
// functions it has been seen to call that native code may call
// (closureCallSite, as a forwarding call), or the call of Go that makes
// it from the frame's arguments; none where the code does not hand its
// arguments on natively (compile's ForwardsNatively), whose
// apply_arguments leaves native code, as before.
func (fb *jitFeedback) forwardCalls(pc int) []ssa.CallSite {
	r := fb.r
	if r.applyFn == nil || fb.cl == nil || !jitcompile.ForwardsNatively(fb.fn) {
		return nil
	}
	call := fb.fn.Code[pc]
	targets := fb.targetsAt(pc)
	var sites []ssa.CallSite
	for _, x := range targets {
		in := *x
		if int(in.pc) != pc || in.cl == nil || in.obj == nil {
			continue
		}
		if slicesHasSite(sites, in) {
			continue
		}
		site, ok := fb.closureCallSite(pc, in, call, targets)
		if !ok {
			continue
		}
		sites = append(sites, site)
	}
	k := fb.keep()
	k.holders = append(k.holders, r.applyFn)
	apply := uintptr(unsafe.Pointer(r.applyFn))
	if len(sites) != 0 {
		if jitCountAt(fb.e.nativeCalls, pc) >= jitCallTargets {
			// As many as it calls natively: any other, Go's from native
			// code, which had left at each.
			sites[0].Generic = true
		}
		return sites
	}
	return []ssa.CallSite{{Go: ssa.GoCall, Forward: true, Apply: apply, Argc: 2, ThisSlot: -1}}
}

// slicesHasSite reports whether sites calls in's function object already,
// or its function by its code.
func slicesHasSite(sites []ssa.CallSite, in jitInline) bool {
	for _, s := range sites {
		if s.Callee == uintptr(unsafe.Pointer(in.obj)) || s.ByCode != 0 && s.ByCode == uintptr(unsafe.Pointer(in.cl.fn)) &&
			s.Env == uintptr(unsafe.Pointer(in.cl.env)) && s.Realm == uintptr(unsafe.Pointer(in.cl.realm)) {
			return true
		}
	}
	return false
}

// jitForwardSeen is jitCallSeen's for an apply_arguments at pc, its
// operands on the VM's stack up to sp, that native code left to Go or to
// the interpreter: the function it calls, where native code may call it,
// is one it calls natively from the next compile on (forwardCalls), up to
// jitCallTargets, as a call's target is learned (jitCallTarget); a
// built-in, or a function native code does not call, leaves it Go's.
func (r *Runtime) jitForwardSeen(f *frame, e *jitEntry, pc, sp int, in bytecode.Instr) {
	if !jitcompile.ForwardsNatively(f.cl.fn) {
		return
	}
	if e.callSites == nil {
		e.callSites = make([]uint8, len(f.cl.fn.Code))
	}
	if r.jitKnownTargetSeen(f, e, pc, sp, in) {
		return
	}
	switch e.callSites[pc] {
	case jitCallNative:
		r.jitCallTarget(f, e, pc, sp, in)
		if e.inlinePending && e.inlineReopts < 2*jitInlineReoptimizations {
			e.inlinePending, e.inlineReopt = false, true
		}
		return
	case jitCallDone:
		return
	case jitCallWaiting:
		e.callSites[pc] = 0
	}
	o, cl := r.jitCalled(f, sp, in)
	if cl != nil && r.jitCalleeAwaited(cl) {
		// Compiled in the background, or about to be: decided once it is.
		e.callSites[pc] = jitCallWaiting
		return
	}
	if n := e.callSites[pc]; n+1 < jitCallsToInline {
		// Settling, as a call does (jitCallSeen): the functions it meets
		// meanwhile -- a class library's constructor forwards to each
		// class's initialize -- are decided together, the code compiled
		// again once for them, not once for each.
		e.callSites[pc]++
		if cl != nil && !slices.ContainsFunc(e.settling, func(x jitInline) bool { return int(x.pc) == pc && x.obj == o }) {
			e.settling = append(e.settling, jitInline{pc: int32(pc), cl: cl, obj: o, forward: true})
			r.jit.account(e)
		}
		return
	}
	was := r.jit.deciding
	r.jit.deciding = e
	defer func() { r.jit.deciding = was }()
	e.callSites[pc] = jitCallDone
	r.jit.callsDecided++
	jitCountLearned(e, pc)
	var met []jitInline
	for i := 0; i < len(e.settling); i++ {
		if x := e.settling[i]; int(x.pc) == pc {
			if x.obj != o {
				met = append(met, x)
			}
			e.settling = slices.Delete(e.settling, i, i+1)
			i--
		}
	}
	if cl != nil && r.jitNativeOrLinked(cl) {
		e.nativeCalls = append(e.nativeCalls, r.jitNativeTarget(int32(pc), cl, o, in))
		e.callSites[pc] = jitCallNative
		e.inlinePending = true
	}
	for _, x := range met {
		if e.callSites[pc] != jitCallNative || jitCountAt(e.nativeCalls, pc) >= jitCallTargets {
			break
		}
		if r.jitNativeOrLinked(x.cl) {
			e.nativeCalls = append(e.nativeCalls, r.jitNativeTarget(int32(pc), x.cl, x.obj, in))
		}
	}
}

// jitForwardApplies reports whether the call of Go the code in ctx makes
// at pc of fn, an apply_arguments (abi.GoCallForward), is one Go makes
// from the frame's arguments: its apply the realm's, in code that hands
// them on as they were given (compile's ForwardsNatively). Any other the
// interpreter makes, which makes the arguments object.
func (r *Runtime) jitForwardApplies(ctx *abi.Context, fn *bytecode.Function, pc int) bool {
	if pc >= len(fn.Code) || fn.Code[pc].Op != bytecode.OpApplyArguments || !jitcompile.ForwardsNatively(fn) {
		return false
	}
	ap := jitGoArg(ctx, 1)
	return r.applyFn != nil && ap.IsObject() && ap.Object() == r.applyFn
}

// jitForwardMade makes f.apply(x, arguments), its operands at the top of
// the VM's stack up to sp, with args, the frame's arguments, as the
// interpreter's applyArguments makes it: through the built-in apply's
// frame (applyCall). Arguments a native call wrote to a context, which
// its next call there overwrites, are copied first (copied).
func (r *Runtime) jitForwardMade(args []Value, sp int, copied bool) (Value, error) {
	target, this := r.stack[sp-3], r.stack[sp-1]
	if !copied {
		return r.applyCall(target, this, args)
	}
	i, n := len(r.argStack), len(args)
	r.argStack = append(r.argStack, args...)
	v, err := r.applyCall(target, this, r.argStack[i:i+n:i+n])
	clear(r.argStack[i:])
	r.argStack = r.argStack[:i]
	return v, err
}

// jitForwardEntry writes args, the arguments of the frame Go enters code
// that hands them on with (ir.Program's Forwards), to its context, as a
// native call writes its callee's (abi.Context's Argc and Args): their
// count, which past the context's room for them has the code's forwarding
// calls made by Go, from the frame's; and their words, as the context
// holds them. The frame keeps them alive while the code runs.
func jitForwardEntry(ctx *abi.Context, args []Value) {
	ctx.Argc = uint64(len(args))
	if len(args) > len(ctx.Args) {
		return
	}
	for i := range args {
		ctx.Args[i] = *(*abi.GoArg)(unsafe.Pointer(&args[i]))
	}
}

// jitForwardPlain makes Array.apply(null, arguments) -- a forwarding call
// (abi.GoCallForward) of the realm's Array through its apply -- as the
// constructor called so makes it, an array of the frame's arguments,
// without a frame: what runs no script and cannot throw. Array(n) of one
// number, which makes an array of that length or throws, it leaves to
// jitGoCall, as it does more arguments than the context holds -- the
// frame's, read there -- and a runtime that meters its memory.
func (r *Runtime) jitForwardPlain(ctx *abi.Context) bool {
	if ctx.Argc > uint64(len(ctx.Args)) || r.meter != nil || r.applyFn == nil {
		return false
	}
	t, ap := jitGoArg(ctx, 0), jitGoArg(ctx, 1)
	if !t.IsObject() || t.Object() != r.proto.arrayCtor || !ap.IsObject() || ap.Object() != r.applyFn {
		return false
	}
	args := jitContextArgs(ctx)
	if len(args) == 1 && args[0].IsNumber() {
		return false
	}
	*(*Value)(unsafe.Pointer(&ctx.Keep[ctx.GoKeep])) = Obj(r.newArrayFrom(args))
	return true
}

// jitForwardLearned is the function the apply_arguments at pc of cl's
// code calls natively, as that code's own forwards learned it
// (jitForwardSeen), where it is one function object a native caller may
// call: what a call of cl inlined in a caller forwards its arguments to,
// inlined in turn (forwardInline). Or nil.
func (r *Runtime) jitForwardLearned(cl *closure, pc int) *Object {
	e := r.jit.entryFor(cl.fn)
	if e == nil || e.callSites == nil || pc >= len(e.callSites) || e.callSites[pc] != jitCallNative {
		return nil
	}
	var t *Object
	for _, x := range e.nativeCalls {
		if int(x.pc) != pc {
			continue
		}
		if t != nil || x.byCode || x.cl == nil || x.obj == nil {
			return nil
		}
		t = x.obj
	}
	if t == nil || !r.jitNativeTargetOf(t.fn(), cl, false) {
		return nil
	}
	return t
}

// jitForwardsCall reports whether fn forwards its arguments,
// f.apply(this, arguments), its arguments object made for nothing else
// (bytecode's ForwardsArguments): a call of it, not a construction, is
// inlined where jitForwardsCallInline says, as V8 inlines a callee whose
// arguments its caller knows, the forward a call with them.
func jitForwardsCall(fn *bytecode.Function) bool {
	return (fn.ForwardsArguments || fn.Leaf == bytecode.LeafForward) && !jitReadsArguments(fn)
}

// jitForwardsCallInline reports whether a call -- not a construction -- of
// cl, a forwarding function (jitForwardsCall), may be inlined with the
// call's arguments: its code hands them on as they were given (compile's
// ForwardsNatively), assigns no binding it captured, and each function it
// forwards them to is the one its own code learned it calls there
// (jitForwardLearned), not one that forwards them in turn, which may be
// inlined in it with its calls, depth levels on.
func (r *Runtime) jitForwardsCallInline(cl *closure, depth int) bool {
	fn := cl.fn
	if depth <= 0 || !jitForwardsCall(fn) || !jitcompile.ForwardsNatively(fn) || jitNoInline != nil && jitNoInline(fn) || jitWritesUpvalues(cl) {
		return false
	}
	if _, err := r.jitLower(fn, jitLowerForward); err != nil {
		return false
	}
	forwards := false
	for pc, in := range fn.Code {
		if in.Op != bytecode.OpApplyArguments {
			continue
		}
		forwards = true
		t := r.jitForwardLearned(cl, pc)
		if t == nil {
			return false
		}
		tc := t.fn().closure
		if tc.fn == fn || tc.fn.ForwardsArguments || tc.fn.Leaf == bytecode.LeafForward || jitWritesUpvalues(tc) ||
			!r.jitInlinable(tc.fn) && !r.jitInlinesCalls(tc, depth-1) {
			return false
		}
	}
	return forwards
}

// jitPlainThisCell is a cell holding the receiver a plain call of cl's
// function gives it, for its frame inlined (ssa.InlineSite's ThisCell): the
// realm's global object, a sloppy function's this coerced, or undefined, a
// strict one's, as callFunction gives it; kept by fb's compile. False for
// a function of another realm, whose global object is its own.
func (fb *jitFeedback) jitPlainThisCell(cl *closure) (uintptr, bool) {
	cell := new(Value)
	*cell = Undefined
	if cl.fn.CoerceThis {
		if cl.realm != fb.r.Realm || !fb.r.globalThis.IsObject() {
			return 0, false
		}
		*cell = fb.r.globalThis
	}
	k := fb.keep()
	k.cells = append(k.cells, cell)
	return uintptr(unsafe.Pointer(cell)), true
}

// jitForwardsCoerced reports whether a function cl's code forwards its
// arguments to (jitForwardLearned) coerces its receiver: a sloppy one.
func (r *Runtime) jitForwardsCoerced(cl *closure) bool {
	for pc, in := range cl.fn.Code {
		if in.Op != bytecode.OpApplyArguments {
			continue
		}
		if t := r.jitForwardLearned(cl, pc); t == nil || t.fn().closure.fn.CoerceThis {
			return true
		}
	}
	return false
}
