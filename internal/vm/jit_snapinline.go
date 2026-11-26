//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64))

package vm

import (
	"slices"
	"unsafe"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
	"github.com/go-quickjs/go-quickjs/internal/jit/ir"
	"github.com/go-quickjs/go-quickjs/internal/jit/ssa"
)

// Inline's answer, of one implementation over what it is derived from
// (jitInlineSrc): the live state on the runtime's goroutine, or a
// snapshot's copy of it, which the worker derives from (jit_snapderive.go).

// jitInlineSrc is what an inline site's answer is derived from besides the
// call's targets (jitCallCtx's callTargets) and the function's code.
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
	// lower is cl's register code rc lowered for kind (jitRegLower).
	lower(cl *closure, rc *bytecode.RegCode, kind jitLowerKind) (*ir.Program, error)
	// seenInlined takes what cl's caches have seen besides their entries
	// into what its code learned (jitRegSeenInlined).
	seenInlined(cl *closure)
	// calleeOf is the feedback of cl's code lowered as p, inlined, its
	// forwarding frame fwd where it is a forwarder inlined with its
	// call's arguments (jitRegCalleeOf).
	calleeOf(cl *closure, p *ir.Program, fwd *jitForwardFrame) ssa.Feedback
	// global is the realm's global object, a sloppy plain call's receiver.
	global() Value
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

func (l jitInlineLive) global() Value { return l.f.fb.r.globalThis }

// jitInlineOf is Inline's answer at the program's instruction pc of q's
// function, from what src and q's source have: a plain call, a method's or
// a construction of a base constructor of one function, the one its site's
// cache holds and its code learned it calls (callTargets), no other, that
// runs register code native callers may call -- see Inline.
func jitInlineOf(q *jitCallCtx, pc int, src jitInlineSrc) (ssa.InlineSite, bool) {
	if pc < 0 || pc >= len(q.p.Code) || int(q.p.Origin[pc]) >= len(q.rc.Code) {
		return ssa.InlineSite{}, false
	}
	w, op := q.rc.Code[q.p.Origin[pc]], q.p.Code[pc].Op
	if op != ir.Host || !q.p.Code[pc].Window {
		return ssa.InlineSite{}, false
	}
	if w.Op() == bytecode.RApplyArgs {
		// A forwarding call of the frame's own arguments: inlined where the
		// frame is an inlined callee's given its call's (fwd), called
		// natively otherwise.
		site, ok, _ := src.forward(pc)
		return site, ok
	}
	// A call in tail position (tail_call_w, tail_call_method_w) is inlined
	// as the ordinary call (jitRegCallOp): the builder guards first that
	// the stack is shallow, where the interpreter makes it as the ordinary
	// call too (ssa.OpTailSelf, OpTailRoom), and leaves for it to give the
	// frame up otherwise.
	call := jitRegCallOp(w.Op())
	method, construct := call == bytecode.RCallMW, call == bytecode.RNewW
	if call != bytecode.RCallW && !method && !construct {
		return ssa.InlineSite{}, false
	}
	rpc := int32(q.p.Origin[pc])
	if int(rpc) < len(q.rc.Marks) && q.rc.Marks[rpc]&bytecode.RMarkTail != 0 {
		return ssa.InlineSite{}, false
	}
	l := q.src.learned()
	if l != nil && (slices.Contains(l.notInline, rpc) || slices.Contains(l.genericCalls, rpc)) {
		// Nor one that met a function native callers never call
		// (jitRegGenericSeen), which an inlined call leaves native code
		// for: called natively, Go calling that one from native code
		// (ssa.CallSite's Generic).
		return ssa.InlineSite{}, false
	}
	argc := int(w.X())
	var t jitRegTargets
	q.callTargets(&t, pc, w, argc, method)
	if t.n != 1 || t.sites[0].ByCode != 0 || t.sites[0].ArrowThis || t.sites[0].Via != 0 {
		// One through Function.prototype.call, G.call(x, ...), is called
		// natively, never inlined.
		return ssa.InlineSite{}, false
	}
	site, o, cl := t.sites[0], t.objs[0], t.cls[0]
	if cl.fn == q.fn || cl.fn == src.rootFn() || src.noInlined(cl.fn) {
		return ssa.InlineSite{}, false
	}
	if jitRegCallColdOf(src, l, pc, rpc, cl) {
		// One made too seldom to be inlined is called natively, whatever
		// its size, as V8's Turbofan decides it.
		return ssa.InlineSite{}, false
	}
	if src.megaGetter(cl) {
		// One whose read at a megamorphic site has met a getter on its
		// receivers' prototypes is called natively, its own code making
		// the read on its frame: inlined, the read has no frame for Go to
		// call the getter on, and leaves native code, the interpreter
		// finishing the caller's invocation, at every receiver of the
		// getter.
		return ssa.InlineSite{}, false
	}
	if site.Parent != 0 {
		// A construction with a class that has no constructor of its own,
		// run with its parent's code while the class's prototype is still
		// that parent, which the native call checks (ssa.CallSite's
		// Parent, mir's constructGuards) and an inlined frame does not:
		// inlined, a class given another parent by Object.setPrototypeOf
		// went on constructing with the old one's code, its objects given
		// h for q.
		return ssa.InlineSite{}, false
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
		case !construct && jitRegReadsArguments(cl):
			reads = true
		case jitRegForwards(cl) && src.forwardsInline(cl, ctor, argc):
			kind, forwards = jitLowerForward, true
		default:
			return ssa.InlineSite{}, false
		}
	}
	p, err := src.lower(cl, cl.reg.rc, kind)
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
		Argc: argc, Method: method, Params: 2 + int(cl.reg.params), ThisSlot: -1, Window: true,
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
	if cl == nil || src.callee() || !jitCountedCallee(cl) {
		return false
	}
	if l != nil {
		for _, list := range [...][]jitInline{l.inlines, l.nativeCalls} {
			for _, x := range list {
				if x.pc == rpc && !jitFramed(x) {
					return false
				}
			}
		}
	}
	fr, ok := src.frequency(pc)
	return ok && fr < ssa.MinInliningFrequency
}
