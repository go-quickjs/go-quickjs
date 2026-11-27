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

// Register callees that keep their arguments (regCode's args), inlined with
// their call's arguments (jitRegFeedback's Inline, forwardInline): one that
// reads them by index and counts them alone (jitRegReadsArguments) reads
// the call's (ssa.InlineSite's ReadsArguments), and one that hands them on,
// f.apply(x, arguments) (jitRegForwards), is inlined with them (Arguments),
// its apply_args the call of f with them, f inlined in its turn (Forward).
// The frame Go makes of such a callee's level at an exit inside it is given
// the arguments from its caller's call's window, or from the frame that
// forwarded them (jitRegInlineArgs).

// jitRegReadsArguments reports whether cl's register code keeps its
// arguments only to read them by index and count them (args_index,
// args_len): no arguments object made (arguments), none handed on
// (apply_args), no rest parameter, and no parameter a sloppy function's
// object would alias. A closure no call ran yet, a template of one its
// maker makes (madeInline), is asked by its function's register code, which
// its first call gets.
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
// alias.
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

// jitRegForwardTarget is the function the apply_args at register pc rpc of
// cl's code hands its arguments to, inlined where cl is with argc of them:
// for a construction of ctor, k of this.k.apply(this, arguments), a data
// property of ctor's prototype; else the one function cl's own code learned
// it calls there, its forwards settled (jitRegForwardSeen); else, for a
// construction, f as the read of it finds it now (jitRegForwardGuess). A
// plain call is never inlined for a guess: the forwarder's closures a call
// meets each hand their arguments to a function of their own --
// forward_poly's four -- and inlined for the f of the one the call's cache
// held, the call left native code at three turns in four. A function native
// callers may call, of cl's realm and scope; or nil.
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
	if depth <= 0 || r.jitNoInlined(cl.fn) {
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
// arguments on to (jitRegForwardTarget), inlined with the arguments its own
// call gave it (ssa.InlineSite's Forward) -- one that reads them, so
// (ReadsArguments), one that forwards them in turn, so too (Arguments).
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
	if tc.fn == fb.keep().fn || r.jitNoInlined(tc.fn) {
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

// forwardProperty is the property site of the read at register pc rpc of
// key, of a forwarding constructor's frame (bytecode.LeafForward,
// this.k.apply(this, arguments)) inlined in its construction (f.fwd), known
// from its receiver: this.k, the first read, as an object of the
// construction's pool finds it -- the next its filling makes -- and k's
// apply, the second, as k does (jitRegForwardTarget). Compiled for what the
// forwarder's own cache met -- the classes of every construction it made, a
// library's -- the inlined read left native code at each class it had not
// met, and the construction, once it had left often enough, was made by a
// native call of the forwarder, whose apply_args calls up to jitCallTargets
// methods natively and the rest by Go: RayTrace's IntersectionInfo's
// initialize, by Go at every construction from then on, in some runs.
func (f *jitRegFeedback) forwardProperty(rpc int, key Atom) (ssa.PropertySite, bool) {
	fb := f.fb
	r, cl, rc := fb.r, fb.cl, f.rc
	if cl == nil || rc.Leaf != bytecode.LeafForward || len(rc.Code) < 4 || rpc > 1 {
		return ssa.PropertySite{}, false
	}
	get, apply := rc.Code[0], rc.Code[1]
	if get.Op() != bytecode.RGetProp || get.B() != 1 || apply.Op() != bytecode.RGetProp || apply.B() != get.A() {
		return ssa.PropertySite{}, false
	}
	call := -1
	for i, w := range rc.Code {
		if w.Op() == bytecode.RApplyArgs {
			call = i
			break
		}
	}
	if call < 0 || rc.Code[call].B() != get.A() || rc.Code[call].B()+1 != apply.A() {
		return ssa.PropertySite{}, false
	}
	var recv *Object
	if rpc == 0 {
		recv = r.jitPoolObject(f.fwd.ctor)
	} else {
		recv = r.jitRegForwardTarget(cl, f.fwd.ctor, call, f.fwd.argc)
	}
	if recv == nil {
		return ssa.PropertySite{}, false
	}
	var c propCache
	r.fillPropCache(&c, recv, key, false)
	return fb.siteOf(key, false, &c)
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
// code, f.apply(x, arguments), as the read of f just before it finds it now
// -- a global's, a captured binding's: what a forwarding construction
// inlined hands its arguments to, its call inlined, which checks it is. Or
// nil.
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
// call or its entry wrote them to (jitForwardEntry) -- or the frame's own
// past what a context holds. What it calls the code learns, to call
// natively once compiled again (jitRegCallSeen).
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
	f.pc = uint32(rpc) + 1
	last, shared := s.ssaCtx, s.ssaSharedGen
	// A copy: a native call's next call writes the context over.
	i, n := len(r.argStack), len(args)
	r.argStack = append(r.argStack, args...)
	v, err := r.applyCall(target, this, r.argStack[i:i+n:i+n])
	clear(r.argStack[i:])
	r.argStack = r.argStack[:i]
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

// Reads of arguments objects as values -- a callee's returned, one kept in
// a local -- learned as V8's keyed loads and loads of length learn their
// receivers' kinds: an element read whose receiver has been an unmapped
// arguments object's, by an index it has an element at
// (jitRegArgsElemSeen), and a length read whose receiver has been an
// arguments object of the realm's layout (jitRegArgsLengthSeen), are
// marked jitArgsRead in their function's typedReads, which no other read's
// PC is, so that TypedRead and TypedBigRead take it for none, and what
// keeps and carries typedReads keeps and carries it too. Native code reads
// such an element by its cell, as an Array's, and such a length, its own
// data property, from the object's table (ssa.ArgumentsFeedback); a read
// that fails where it is marked is learned as any other receiver's -- the
// element Go's (keyedReads), the length generic. Each had been a call of
// Go, which cost native code more than the interpreter's read:
// microbench's arguments_object ran slower with the JIT than without.
const jitArgsRead = 1 << 28

// jitArgsElemReadable reports whether o[k] is an unmapped arguments
// object's element, k an index it has a dense element at: what native code
// reads of one (ssa.ArrayOfArguments).
func jitArgsElemReadable(o, k Value) bool {
	if !o.IsObject() || !k.IsNumber() {
		return false
	}
	obj := o.Object()
	if obj.class != ClassArguments || obj.flags&(objMappedArguments|objHasExt) != objHasExt {
		return false
	}
	i := uint32(k.num)
	return float64(i) == k.num && uint(i) < uint(len(obj.elemsOf())) && !isHole(obj.elemsOf()[i])
}

// jitRegArgsElemSeen learns the element read at register pc rpc, of
// receiver o at key k, in l, as one of an arguments object's
// (jitArgsRead), where it is one native code reads and the read is not
// marked so already. It reports whether it learned it.
func jitRegArgsElemSeen(l *jitEntry, rpc int, o, k Value) bool {
	pc := int32(rpc) | jitArgsRead
	if !jitArgsElemReadable(o, k) || slices.Contains(l.typedReads, pc) {
		return false
	}
	l.typedReads = append(l.typedReads, pc)
	return true
}

// jitRegArgsLengthSeen learns, at a deoptimization of register code at
// register pc rpc of c, whose frame's window at wb holds the state at the
// instruction's start, a get_length whose receiver was an arguments object
// of a shape the realm's arguments objects have (jitArgumentsLengths) --
// as one of an arguments object's (jitArgsRead), not generic -- where it
// is not marked so already. It reports whether it learned it.
func (r *Runtime) jitRegArgsLengthSeen(l *jitEntry, c *regCode, wb, rpc int) bool {
	if rpc < 0 || rpc >= len(c.rc.Code) {
		return false
	}
	w := c.rc.Code[rpc]
	if w.Op() != bytecode.RGetLength || wb+int(w.B()) >= len(r.stack) {
		return false
	}
	o := r.stack[wb+int(w.B())]
	pc := int32(rpc) | jitArgsRead
	if !o.IsObject() || o.Object().class != ClassArguments || slices.Contains(l.typedReads, pc) {
		return false
	}
	shape := o.Object().shape
	known := false
	for _, t := range r.argsLayouts {
		known = known || t != nil && t.shape == shape && jitArgsLengthAt(shape) >= 0
	}
	if !known {
		return false
	}
	l.typedReads = append(l.typedReads, pc)
	return true
}

// jitArgsLengthAt is where an arguments object of shape s has its length,
// a data property, in its table: -1 where s is an object's own (unique),
// which it changes in place, or has its length elsewhere, or as an
// accessor. An arguments object's shape is loose, no cache's to remember,
// but the realm's layout of them (argsLayout) is a template its objects
// share, which none changes in place: an object whose keys or their
// attributes change is given a copy of its own first (appendProp,
// layoutChanged), so that an object of the template's shape has its keys.
func jitArgsLengthAt(s *shape) int32 {
	if s == nil || s.unique || s.n == 0 {
		return -1
	}
	if k := s.keys[0]; k.key != atomLength || k.flags&(propAccessor|propDeleted|propPrivate) != 0 {
		return -1
	}
	return 0
}

// ArgumentsRead is ssa.ArgumentsFeedback's (argumentsAt), at the
// program's instruction pc.
func (f *jitRegFeedback) ArgumentsRead(pc int) (ssa.ArgumentsSite, bool) {
	if pc < 0 || pc >= len(f.p.Code) || pc >= len(f.p.Origin) {
		return ssa.ArgumentsSite{}, false
	}
	op := f.p.Code[pc].Op
	if op != ir.ArrayRead && op != ir.ArrayLength {
		return ssa.ArgumentsSite{}, false
	}
	rpc := int(f.p.Origin[pc])
	if rpc >= len(f.rc.Code) || (op == ir.ArrayLength) != (f.rc.Code[rpc].Op() == bytecode.RGetLength) {
		return ssa.ArgumentsSite{}, false
	}
	return f.argumentsAt(rpc)
}

// argumentsAt is ArgumentsRead's answer of the register code: whether the
// element read or the get_length at register pc rpc is marked as an
// arguments object's (jitArgsRead), and, a length's, the shapes of the
// realm's arguments objects, unmapped and mapped, that have it where the
// first has (jitArgsLengthAt), kept with the code.
func (f *jitRegFeedback) argumentsAt(rpc int) (ssa.ArgumentsSite, bool) {
	if l := f.fb.e; l == nil || !slices.Contains(l.typedReads, int32(rpc)|jitArgsRead) {
		return ssa.ArgumentsSite{}, false
	}
	if f.rc.Code[rpc].Op() != bytecode.RGetLength {
		return ssa.ArgumentsSite{}, true
	}
	var site ssa.ArgumentsSite
	k := f.fb.keep()
	for i, t := range f.fb.r.argsLayouts {
		if t == nil || jitArgsLengthAt(t.shape) < 0 {
			continue
		}
		// Kept for as long as the code is, which compares its address;
		// not remembered, as a cache's shape is: a template is never
		// changed in place.
		k.shapes = append(k.shapes, t.shape)
		site.Shapes[i] = uintptr(unsafe.Pointer(t.shape))
	}
	return site, site.Shapes != [2]uintptr{}
}
