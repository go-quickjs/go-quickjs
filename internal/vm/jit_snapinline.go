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

// The inline sites' answers -- Inline's, ForEach's and CalledClosure's --
// of one implementation over what they are derived from (jitInlineSrc):
// the live state on the runtime's goroutine, or a snapshot's copy of it,
// which the worker derives from (jit_snapwalk.go).

// jitInlineSrc is what an inline site's answer is derived from besides the
// call's targets (jitCallCtx's callTargets, callSite) and the function's
// code.
type jitInlineSrc interface {
	// forward is the answer of a forwarding call of the frame's own
	// arguments (forwardInline), handled where the frame is a forwarded
	// callee's.
	forward(pc int) (site ssa.InlineSite, ok, handled bool)
	// callee says the feedback is an inlined callee's (root); rootFn is the
	// compile's own function.
	callee() bool
	rootFn() *bytecode.Function
	// frequency is Frequency's answer at pc.
	frequency(pc int) (float64, bool)
	// noInlined is jitNoInlined's, megaGetter jitMegaGetter's and
	// forwardsInline jitRegForwardsInline's at the inliner's depth.
	noInlined(fn *bytecode.Function) bool
	megaGetter(cl *closure) bool
	forwardsInline(cl *closure, ctor *Object, argc int) bool
	// reg is cl's register code, nil for none yet (closure's reg); code
	// fn's register code, made where it has none (Function's RegCode).
	reg(cl *closure) *regCode
	code(fn *bytecode.Function) *bytecode.RegCode
	// readsArgs is jitRegReadsArguments' answer of cl, forwards
	// jitRegForwards', counted jitCountedCallee's.
	readsArgs(cl *closure) bool
	forwards(cl *closure) bool
	counted(cl *closure) bool
	// lower is cl's register code rc lowered for kind (jitRegLower).
	lower(cl *closure, rc *bytecode.RegCode, kind jitLowerKind) (*ir.Program, error)
	// seenInlined takes what cl's caches have seen besides their entries
	// into what its code learned (jitRegSeenInlined).
	seenInlined(cl *closure)
	// calleeOf is the feedback of cl's code lowered as p, inlined, its
	// forwarding frame fwd where it is a forwarder inlined with its
	// call's arguments (jitRegCalleeOf); madeOf, of cl's code rc lowered
	// as p, a loop's callback or a closure the function makes, inlined, a
	// feedback of its own.
	calleeOf(cl *closure, p *ir.Program, fwd *jitForwardFrame) ssa.Feedback
	madeOf(cl *closure, rc *bytecode.RegCode, p *ir.Program) ssa.Feedback
	// siteAtTop is jitRegSiteAtTop's answer at register pc rpc of the
	// function's learning; loopKind jitLoopKind's of o; typedProto the
	// index of proto among the realm's typed arrays' prototypes, -1 for
	// none; cbLeft whether the loop of x, the i-th of the learning's native
	// calls, has had its callback leave native code too often (jitInline's
	// cbLeft), which an exit writes in place.
	siteAtTop(rpc int) bool
	loopKind(o *Object) int
	typedProto(proto *Object) int
	cbLeft(i int, x *jitInline) bool
	// global is the realm's global object, a sloppy plain call's receiver.
	global() Value
	// kept is the call's native targets at q's program's instruction pc
	// where they were found ahead (callTargets) -- how many, and the one
	// where there is one -- what finding them appended put back in q's
	// source's keep, ok where they were: a snapshot's gather finds them by
	// register pc, which its derivation takes (jit_snapwalk.go). found
	// keeps those found, what finding them appended to q's source's keep
	// past before taken back.
	//
	// Each takes q by value: given its address, an interface's method
	// would put q on the heap at every ask, the rejected ones' too.
	kept(q jitCallCtx, pc int) (n int, site ssa.CallSite, o *Object, cl *closure, ok bool)
	found(q jitCallCtx, pc int, before jitSnapLens, n int, site ssa.CallSite, o *Object, cl *closure)
}

// jitInlineLive is the live state of f's function (jitInlineSrc).
type jitInlineLive struct{ f *jitRegFeedback }

func (l jitInlineLive) forward(pc int) (ssa.InlineSite, bool, bool) {
	if l.f.fwd == nil {
		return ssa.InlineSite{}, false, false
	}
	site, ok := l.f.forwardInline(pc)
	return site, ok, true
}

func (l jitInlineLive) callee() bool               { return l.f.fb.root != nil }
func (l jitInlineLive) rootFn() *bytecode.Function { return l.f.fb.keep().fn }

func (l jitInlineLive) frequency(pc int) (float64, bool) { return l.f.Frequency(pc) }

func (l jitInlineLive) noInlined(fn *bytecode.Function) bool { return l.f.fb.r.jitNoInlined(fn) }
func (l jitInlineLive) megaGetter(cl *closure) bool          { return l.f.fb.r.jitMegaGetter(cl) }

func (l jitInlineLive) forwardsInline(cl *closure, ctor *Object, argc int) bool {
	return l.f.fb.r.jitRegForwardsInline(cl, ctor, argc, jitInlineDepth)
}

func (l jitInlineLive) reg(cl *closure) *regCode                     { return cl.reg }
func (l jitInlineLive) code(fn *bytecode.Function) *bytecode.RegCode { return fn.RegCode() }
func (l jitInlineLive) readsArgs(cl *closure) bool                   { return jitRegReadsArguments(cl) }
func (l jitInlineLive) forwards(cl *closure) bool                    { return jitRegForwards(cl) }
func (l jitInlineLive) counted(cl *closure) bool                     { return jitCountedCallee(cl) }

func (l jitInlineLive) lower(cl *closure, rc *bytecode.RegCode, kind jitLowerKind) (*ir.Program, error) {
	return l.f.fb.r.jitRegLower(cl, rc, kind)
}

func (l jitInlineLive) seenInlined(cl *closure) { l.f.fb.r.jitRegSeenInlined(l.f.fb.keep(), cl) }

func (l jitInlineLive) calleeOf(cl *closure, p *ir.Program, fwd *jitForwardFrame) ssa.Feedback {
	r, k := l.f.fb.r, l.f.fb.keep()
	if fwd == nil {
		return r.jitRegCalleeOf(k, cl, p)
	}
	return &jitRegFeedback{fb: &jitFeedback{r: r, fn: cl.fn, cl: cl, e: r.jitState().regLearnedOf(r.jitRegKeyOf(cl)), root: k}, rc: cl.fn.RegCode(), p: p, fwd: fwd}
}

func (l jitInlineLive) madeOf(cl *closure, rc *bytecode.RegCode, p *ir.Program) ssa.Feedback {
	r := l.f.fb.r
	return &jitRegFeedback{fb: &jitFeedback{r: r, fn: cl.fn, cl: cl, e: r.jitState().regLearnedOf(r.jitRegKeyOf(cl)), root: l.f.fb.keep()}, rc: rc, p: p}
}

func (l jitInlineLive) siteAtTop(rpc int) bool { return jitRegSiteAtTop(l.f.fb.e, rpc) }
func (l jitInlineLive) loopKind(o *Object) int { return l.f.fb.r.jitLoopKind(o) }

func (l jitInlineLive) typedProto(proto *Object) int {
	return slices.Index(l.f.fb.r.typedArrayProtos[:], proto)
}

func (l jitInlineLive) cbLeft(_ int, x *jitInline) bool { return x.cbLeft }

func (l jitInlineLive) global() Value { return l.f.fb.r.globalThis }

func (l jitInlineLive) kept(jitCallCtx, int) (int, ssa.CallSite, *Object, *closure, bool) {
	return 0, ssa.CallSite{}, nil, nil, false
}

func (l jitInlineLive) found(jitCallCtx, int, jitSnapLens, int, ssa.CallSite, *Object, *closure) {}

// jitInlinePre is what Inline's answer at a call decides of its callee
// before it lowers it (jitInlinePrefix): the call's register pc, argument
// count and kind, its one native target -- its site, object and closure --
// and how the callee is lowered and given its arguments.
type jitInlinePre struct {
	rpc                       int32
	argc                      int
	method, construct         bool
	site                      ssa.CallSite
	o                         *Object
	cl                        *closure
	kind                      jitLowerKind
	reads, forwards, applyArg bool
}

// jitInlinePrefix is Inline's answer at the program's instruction pc of
// q's function as far as it goes before it lowers the callee: ok where it
// takes the callee it decides (pre), applyArg where the call is a
// forwarding one (forward). Where q has no program it is the answer of the
// call at register pc pc, as a snapshot's gather asks it to know which
// callees an answer may take (jit_snapwalk.go): the same decision, the
// program's instruction a call's own.
func jitInlinePrefix(q *jitCallCtx, pc int, src jitInlineSrc) (pre jitInlinePre, ok bool) {
	var w bytecode.RInstr
	if q.p == nil {
		if pc < 0 || pc >= len(q.rc.Code) {
			return pre, false
		}
		w = q.rc.Code[pc]
	} else {
		if pc < 0 || pc >= len(q.p.Code) || int(q.p.Origin[pc]) >= len(q.rc.Code) {
			return pre, false
		}
		var op ir.Op
		w, op = q.rc.Code[q.p.Origin[pc]], q.p.Code[pc].Op
		if op != ir.Host || !q.p.Code[pc].Window {
			return pre, false
		}
	}
	if w.Op() == bytecode.RApplyArgs {
		// A forwarding call of the frame's own arguments: inlined where the
		// frame is an inlined callee's given its call's (fwd), called
		// natively otherwise.
		pre.applyArg = true
		return pre, false
	}
	// A call in tail position (tail_call_w, tail_call_method_w) is inlined
	// as the ordinary call (jitRegCallOp): the builder guards first that
	// the stack is shallow, where the interpreter makes it as the ordinary
	// call too (ssa.OpTailSelf, OpTailRoom), and leaves for it to give the
	// frame up otherwise.
	call := jitRegCallOp(w.Op())
	method, construct := call == bytecode.RCallMW, call == bytecode.RNewW
	if call != bytecode.RCallW && !method && !construct {
		return pre, false
	}
	rpc := int32(q.rpcOf(pc))
	if int(rpc) < len(q.rc.Marks) && q.rc.Marks[rpc]&bytecode.RMarkTail != 0 {
		return pre, false
	}
	l := q.src.learned()
	if l != nil && (slices.Contains(l.notInline, rpc) || slices.Contains(l.genericCalls, rpc)) {
		// Nor one that met a function native callers never call
		// (jitRegGenericSeen), which an inlined call leaves native code
		// for: called natively, Go calling that one from native code
		// (ssa.CallSite's Generic).
		return pre, false
	}
	argc := int(w.X())
	n, site, o, cl, kept := src.kept(*q, pc)
	if !kept {
		var t jitRegTargets
		before := jitSnapLensOf(q.src.keep())
		q.callTargets(&t, pc, w, argc, method)
		n, site, o, cl = t.n, t.sites[0], t.objs[0], t.cls[0]
		src.found(*q, pc, before, n, site, o, cl)
	}
	if n != 1 || site.ByCode != 0 || site.ArrowThis || site.Via != 0 {
		// One through Function.prototype.call, G.call(x, ...), is called
		// natively, never inlined.
		return pre, false
	}
	if cl.fn == q.fn || cl.fn == src.rootFn() || src.noInlined(cl.fn) {
		return pre, false
	}
	if jitRegCallColdOf(src, l, pc, rpc, cl) {
		// One made too seldom to be inlined is called natively, whatever
		// its size, as V8's Turbofan decides it.
		return pre, false
	}
	if src.megaGetter(cl) {
		// One whose read at a megamorphic site has met a getter on its
		// receivers' prototypes is called natively, its own code making
		// the read on its frame: inlined, the read has no frame for Go to
		// call the getter on, and leaves native code, the interpreter
		// finishing the caller's invocation, at every receiver of the
		// getter.
		return pre, false
	}
	if site.Parent != 0 {
		// A construction with a class that has no constructor of its own,
		// run with its parent's code while the class's prototype is still
		// that parent, which the native call checks (ssa.CallSite's
		// Parent, mir's constructGuards) and an inlined frame does not:
		// inlined, a class given another parent by Object.setPrototypeOf
		// went on constructing with the old one's code, its objects given
		// h for q.
		return pre, false
	}
	kind, reads, forwards := jitLowerInline, false, false
	if site.FrameArgs {
		// A callee that keeps its arguments (ssa.CallSite's FrameArgs),
		// inlined: one that only reads them, its reads the call's arguments
		// (ReadsArguments); one that hands them on, with them, its
		// forwarding calls inlined with them in turn (Arguments,
		// forwardInline). Any other is called.
		var ctor *Object
		if construct {
			ctor = o
		}
		switch {
		case !construct && src.readsArgs(cl):
			reads = true
		case src.forwards(cl) && src.forwardsInline(cl, ctor, argc):
			kind, forwards = jitLowerForward, true
		default:
			return pre, false
		}
	}
	return jitInlinePre{rpc: rpc, argc: argc, method: method, construct: construct, site: site, o: o, cl: cl,
		kind: kind, reads: reads, forwards: forwards}, true
}

// jitInlineOf is Inline's answer at the program's instruction pc of q's
// function, from what src and q's source have: a plain call, a method's or
// a construction of a base constructor of one function, the one its site's
// cache holds and its code learned it calls (callTargets), no other, that
// runs register code native callers may call -- see Inline.
func jitInlineOf(q *jitCallCtx, pc int, src jitInlineSrc) (ssa.InlineSite, bool) {
	pre, ok := jitInlinePrefix(q, pc, src)
	if pre.applyArg {
		site, ok, _ := src.forward(pc)
		return site, ok
	}
	if !ok {
		return ssa.InlineSite{}, false
	}
	rpc, argc, method, construct := pre.rpc, pre.argc, pre.method, pre.construct
	site, o, cl, kind, reads, forwards := pre.site, pre.o, pre.cl, pre.kind, pre.reads, pre.forwards
	l := q.src.learned()
	reg := src.reg(cl)
	p, err := src.lower(cl, reg.rc, kind)
	if err != nil {
		return ssa.InlineSite{}, false
	}
	// What its sites' caches have seen besides their entries, as its own
	// compiles take it (jitRegSeenFeedback).
	src.seenInlined(cl)
	k := q.src.keep()
	var fwd *jitForwardFrame
	if forwards {
		fwd = &jitForwardFrame{argc: argc}
		if construct {
			fwd.ctor = o
		}
	}
	in := ssa.InlineSite{
		Program: p, Feedback: src.calleeOf(cl, p, fwd), Callee: site.Callee, Closure: site.Closure,
		Argc: argc, Method: method, Params: 2 + int(reg.params), ThisSlot: -1, Window: true,
		Coerce: site.Coerce, Upvalues: jitUpvalueCells(cl), ReadsArguments: reads, Arguments: forwards,
	}
	if construct {
		// A construction's (new_w's) of a base constructor, its receiver
		// from the pool of the objects its code learned it makes
		// (regCallSites, ssa.CallSite's Pool).
		in.Construct, in.Coerce = true, false
		in.Pool, in.ProtoIndex, in.ProtoKey = site.Pool, site.ProtoIndex, site.ProtoKey
	}
	if site.PlainThis {
		// A plain call's receiver, which the callee reads: the global
		// object a sloppy one's is, or undefined.
		cell := new(Value)
		*cell = Undefined
		if site.Global != 0 {
			*cell = src.global()
		}
		k.cells = append(k.cells, cell)
		in.ThisCell = uintptr(unsafe.Pointer(cell))
	}
	k.holders = append(k.holders, o)
	k.inlined = append(k.inlined, cl)
	if l != nil && !slices.ContainsFunc(l.inlines, func(x jitInline) bool { return x.pc == rpc && x.obj == o }) {
		// Where its exits are counted (jitInlineLeft).
		l.inlines = append(l.inlines, jitInline{pc: rpc, cl: cl, obj: o})
	}
	return in, true
}

// jitRegCallColdOf reports whether the call at the program's instruction pc,
// register pc rpc, of cl's function, which the interpreter counts every
// call of, is made too seldom for each invocation of the frame's function
// to be inlined (ssa.MinInliningFrequency), as V8's Turbofan asks the
// frequency before it forces a small function in -- not where the code
// learned a call there it does not count; nor a call inside a callee
// inlined, which is inlined however seldom it is made, as V8 inlines the
// callee and leaves such a call a call, which an inlined frame does not
// make: of src, and l, what the code learned.
func jitRegCallColdOf(src jitInlineSrc, l *jitEntry, pc int, rpc int32, cl *closure) bool {
	if cl == nil || src.callee() || !src.counted(cl) {
		return false
	}
	if l != nil {
		for _, list := range [...][]jitInline{l.inlines, l.nativeCalls} {
			for _, x := range list {
				// Not framed (jitFramed): no counted callee.
				if x.pc == rpc && (x.cl == nil || !src.counted(x.cl)) {
					return false
				}
			}
		}
	}
	fr, ok := src.frequency(pc)
	return ok && fr < ssa.MinInliningFrequency
}

// jitForEachOf is ForEach's answer at the program's instruction pc of q's
// function, from what src and q's source have -- see jitRegFeedback's
// ForEach.
func jitForEachOf(q *jitCallCtx, pc int, src jitInlineSrc) (ssa.ForEachSite, bool) {
	w, op, ok := jitRegAt(q.p, q.rc, pc)
	l := q.src.learned()
	if !ok || op != ir.Host || !q.p.Code[pc].Window || w.Op() != bytecode.RCallMW || l == nil {
		return ssa.ForEachSite{}, false
	}
	rpc := int32(q.p.Origin[pc])
	if src.siteAtTop(int(rpc)) {
		// A loop whose checks kept failing, at the top of its ladder
		// (jitRegSiteTop): Go's call.
		return ssa.ForEachSite{}, false
	}
	for i := range l.nativeCalls {
		x := &l.nativeCalls[i]
		if x.pc != rpc || !x.forEach || x.cb == nil {
			continue
		}
		kind := src.loopKind(x.obj)
		if kind == 0 {
			return ssa.ForEachSite{}, false
		}
		var proto uintptr
		k := q.src.keep()
		if kind&abi.LoopTyped != 0 {
			// A typed array's, of the type its prototype is.
			t := src.typedProto(x.proto)
			if x.proto == nil || t < 0 {
				return ssa.ForEachSite{}, false
			}
			kind |= t << abi.LoopTypedShift
			proto = uintptr(unsafe.Pointer(x.proto))
			k.holders = append(k.holders, x.proto)
		}
		reg := src.reg(x.cb)
		argc := 3
		switch kind & abi.LoopKindMask {
		case abi.LoopReduce:
			argc = 4
		case abi.LoopSort:
			// A comparator, called with two elements; native code sorts
			// only with one it may call again at will.
			argc = 2
			if !jitRegSortComparator(x.cb.fn, reg.rc) {
				return ssa.ForEachSite{}, false
			}
		}
		site, ok := q.callSite(pc, x.cbObj, x.cb, reg, argc, true, nil, nil, x.byCode, false)
		if !ok {
			return ssa.ForEachSite{}, false
		}
		k.holders = append(k.holders, x.obj)
		fe := ssa.ForEachSite{Fn: uintptr(unsafe.Pointer(x.obj)), Kind: kind, Proto: proto, Callback: site}
		if in, ok := q.callbackInline(src, i, x, site, argc); ok {
			fe.Inline = &in
			fe.Made = q.madeCallback(pc, x.cb)
		}
		return fe, true
	}
	return ssa.ForEachSite{}, false
}

// callbackInline is the InlineSite of x's callback, x the i-th of the
// learning's native calls, a loop's (ForEach), of src, for native code to
// inline in the loop, as V8 inlines a forEach's callback, under the rules
// a register callee's inlining follows (jitRegFeedback's Inline): the one
// closure the loop has been given, which the loop checks it is given, its
// window the loop's call's operands -- or, one made anew at each call of
// its maker (byCode), no object, for the builder to inline where the
// caller makes it just before the call (madeCallback, ssa's madeAt) -- not
// an arrow that reads its this, which may come from any frame, nor a
// sloppy function that reads its this, which the loop's call coerces --
// the global object for the undefined a loop without a thisArg gives it
// (ssa's Coerce with a ThisSlot): inlined, its this was the loop's
// undefined, this === globalThis false. Nor one whose inlining in the loop
// left native code too often (jitInlineLeft's cbLeft, counted on the
// loop's own entry), called natively since: kept among the call's
// inlines, its leaving had made the loop's call a native call of the
// callback, and the loop was built no more.
func (q *jitCallCtx) callbackInline(src jitInlineSrc, i int, x *jitInline, site ssa.CallSite, argc int) (ssa.InlineSite, bool) {
	cl, o := x.cb, x.cbObj
	byCode := x.byCode || site.ByCode != 0
	if site.ArrowThis || cl.fn == q.fn || cl.fn == src.rootFn() || src.noInlined(cl.fn) ||
		cl.fn.CoerceThis && cl.fn.UsesThis && cl.fn.Kind != bytecode.KindArrow || src.cbLeft(i, x) {
		return ssa.InlineSite{}, false
	}
	reg := src.reg(cl)
	p, err := src.lower(cl, reg.rc, jitLowerInline)
	if err != nil {
		return ssa.InlineSite{}, false
	}
	src.seenInlined(cl)
	k := q.src.keep()
	in := ssa.InlineSite{
		Program: p, Feedback: src.madeOf(cl, src.code(cl.fn), p), Callee: site.Callee, Closure: site.Closure,
		Argc: argc, Method: true, Params: 2 + int(reg.params), ThisSlot: -1, Window: true,
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
	return in, true
}

// madeCallback is the closure the method call at the program's instruction
// pc is given, if its code makes it just before the call -- a closure of
// cl's function, closure_w writing the call's first argument's register,
// one or two register instructions before it -- which native code inlining
// the call need not make (ssa.MadeCallback): its template, which the code
// keeps, as the inlined frame's closure an exit names, and what it
// captures. Not an arrow that reads its maker's new.target or super
// (jitNeedsMaker). Nothing between its making and the call closes what it
// captures (ssa's madeAt).
func (q *jitCallCtx) madeCallback(pc int, cl *closure) *ssa.MadeCallback {
	call := q.p.Code[pc]
	if q.cl == nil || !call.Window {
		return nil
	}
	rpc := q.p.Origin[pc]
	for at := pc - 1; at >= 0 && rpc-q.p.Origin[at] <= 2; at-- {
		in := q.p.Code[at]
		if in.Op != ir.Closure || in.Dest != call.Left.Slot+2 {
			continue
		}
		k := int(in.Key)
		if k >= len(q.cl.consts) || k >= len(q.rc.Caps) {
			return nil
		}
		kp := q.src.keep()
		c := jitTemplateCell[struct{}](kp, &q.cl.consts[k])
		t := c.template()
		if t == nil || t.fn != cl.fn || jitNeedsMaker(*c) {
			return nil
		}
		caps, ok := jitMadeCaptures(q.rc, t, k)
		if !ok {
			return nil
		}
		kp.inlined = append(kp.inlined, t)
		kp.made = append(kp.made, t)
		return &ssa.MadeCallback{PC: at, Template: uintptr(unsafe.Pointer(c)), Closure: uintptr(unsafe.Pointer(t)), Captures: caps}
	}
	return nil
}

// jitMadeCaptures are what t, the template of the maker's constant k, of
// the maker's register code rc, captures (ssa.MadeCallback's Captures): a
// register of the maker's, by its code's Caps, index<<1|1, or one of the
// maker's own captured bindings, index<<1.
func jitMadeCaptures(rc *bytecode.RegCode, t *closure, k int) ([]int32, bool) {
	caps := make([]int32, len(t.fn.Upvalues))
	for i, desc := range t.fn.Upvalues {
		if !desc.FromParent {
			caps[i] = int32(desc.Index) << 1
			continue
		}
		if k >= len(rc.Caps) || i >= len(rc.Caps[k]) {
			return nil, false
		}
		caps[i] = int32(rc.Caps[k][i])<<1 | 1
	}
	return caps, true
}

// jitCalledTemplate is the template closure_w w, of the maker's register
// code rc whose closure is cl, makes, which CalledClosure may answer
// with, nil for none: a template that reads nothing of its maker's frame
// a native callee's lacks (jitNeedsMaker), whose captures the maker's code
// names (jitMadeCaptures).
func jitCalledTemplate(rc *bytecode.RegCode, cl *closure, w bytecode.RInstr) (t *closure, k int) {
	if w.Op() != bytecode.RClosureW || cl == nil {
		return nil, 0
	}
	k = int(w.BC())
	if k >= len(cl.consts) || k >= len(rc.Caps) {
		return nil, 0
	}
	c := cl.consts[k]
	t = c.template()
	if t == nil || jitNeedsMaker(c) {
		return nil, 0
	}
	if _, ok := jitMadeCaptures(rc, t, k); !ok {
		return nil, 0
	}
	return t, k
}

// jitCalledClosureOf is CalledClosure's answer at the program's
// instruction pc of q's function, from what src and q's source have -- see
// jitRegFeedback's CalledClosure.
func jitCalledClosureOf(q *jitCallCtx, pc int, src jitInlineSrc) (ssa.MadeCallback, ssa.InlineSite, bool) {
	w, op, ok := jitRegAt(q.p, q.rc, pc)
	if !ok || op != ir.Closure || w.Op() != bytecode.RClosureW || q.src.learned() == nil || src.callee() || q.cl == nil {
		return ssa.MadeCallback{}, ssa.InlineSite{}, false
	}
	k := int(w.BC())
	if k >= len(q.cl.consts) || k >= len(q.rc.Caps) {
		return ssa.MadeCallback{}, ssa.InlineSite{}, false
	}
	kp := q.src.keep()
	c := jitTemplateCell[struct{}](kp, &q.cl.consts[k])
	t := c.template()
	if t == nil || jitNeedsMaker(*c) {
		return ssa.MadeCallback{}, ssa.InlineSite{}, false
	}
	caps, ok := jitMadeCaptures(q.rc, t, k)
	if !ok {
		return ssa.MadeCallback{}, ssa.InlineSite{}, false
	}
	site, ok := q.madeInline(src, t)
	if !ok {
		return ssa.MadeCallback{}, ssa.InlineSite{}, false
	}
	if !slices.Contains(kp.made, t) {
		kp.inlined = append(kp.inlined, t)
		kp.made = append(kp.made, t)
	}
	return ssa.MadeCallback{PC: pc, Template: uintptr(unsafe.Pointer(c)), Closure: uintptr(unsafe.Pointer(t)), Captures: caps}, site, true
}

// madeInline is the InlineSite of t's function, a closure the function
// makes (CalledClosure), of src, to inline as a plain call of it, under
// the rules a register callee's inlining follows (Inline): register code
// of the form's own layout a call enters, its arguments kept only to be
// read (ReadsArguments), not the function's own; its window the call's --
// the function, a plain call's receiver, which a callee reading it is
// given (InlineSite's ThisCell), the arguments -- and no closure object:
// the builder's to know. An arrow that reads its this reads the maker's,
// its register 1, as closure_w gives it the frame's register 1
// (InlineSite's LexThis), as V8's inlined arrow reads a context slot of
// its maker's -- not in a derived constructor, whose this may not be
// bound yet.
func (q *jitCallCtx) madeInline(src jitInlineSrc, t *closure) (ssa.InlineSite, bool) {
	fn := t.fn
	rc := src.code(fn)
	switch fn.Kind {
	case bytecode.KindNormal, bytecode.KindArrow, bytecode.KindMethod:
	default:
		return ssa.InlineSite{}, false
	}
	if rc == nil || !rc.Wide || fn.Async || fn.Generator || fn.HasDirectEval ||
		fn == q.fn || fn == src.rootFn() || src.noInlined(fn) {
		return ssa.InlineSite{}, false
	}
	// One that keeps its arguments only to read them by index and count
	// them (jitRegReadsArguments) reads the call's (InlineSite's
	// ReadsArguments), as Inline's callees do; any other that keeps them is
	// called.
	reads := false
	if rc.UsesArguments || rc.ForwardsArguments || rc.KeepsArgs {
		if !src.readsArgs(t) {
			return ssa.InlineSite{}, false
		}
		reads = true
	}
	lexThis := fn.Kind == bytecode.KindArrow && fn.UsesThis
	if lexThis && q.fn.Kind == bytecode.KindDerivedConstructor {
		return ssa.InlineSite{}, false
	}
	p, err := src.lower(t, rc, jitLowerInline)
	if err != nil {
		return ssa.InlineSite{}, false
	}
	if reg := src.reg(t); reg != nil && reg.rc == rc {
		// What its sites' caches have seen besides their entries, as an
		// inlined callee's (Inline).
		src.seenInlined(t)
	}
	k := q.src.keep()
	in := ssa.InlineSite{
		Program: p, Feedback: src.madeOf(t, rc, p),
		Closure: uintptr(unsafe.Pointer(t)), Params: 2 + rc.Params, ThisSlot: -1, Window: true, ReadsArguments: reads,
	}
	switch {
	case lexThis:
		in.LexThis, in.LexThisSlot = true, 1
	case fn.UsesThis && fn.Kind != bytecode.KindArrow:
		// A plain call's receiver: the global object a sloppy callee's is,
		// or undefined.
		cell := new(Value)
		*cell = Undefined
		if fn.CoerceThis {
			*cell = src.global()
		}
		k.cells = append(k.cells, cell)
		in.ThisCell = uintptr(unsafe.Pointer(cell))
	}
	return in, true
}
