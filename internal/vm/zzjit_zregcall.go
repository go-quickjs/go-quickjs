//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64))

package vm

import (
	"slices"
	"unsafe"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
	"github.com/go-quickjs/go-quickjs/internal/jit"
	"github.com/go-quickjs/go-quickjs/internal/jit/abi"
	"github.com/go-quickjs/go-quickjs/internal/jit/ssa"
)

// Native calls between register functions (docs/register-interpreter.md,
// Stage 3, A5): native code made from register code calls a register
// function's native code directly, as the stack form's native calls do --
// through the function's link (jitLink), which holds its code's call entry
// while it has code native callers may call, the callee's window made at
// the VM's stack's top past the caller's frame (ssa.CallSite's Window), its
// context the caller's next. No frame of the VM's is made for a native
// call: where Go runs while native callees are under way -- a call of Go
// one of them makes (jitRegGoCallNested), a callee that left native code
// (jitRegUnwind) -- it makes each level's frame as regEnterW would, and
// pins each level's code (jitRegEntry's levels), which the cache then
// neither drops nor compiles again until the level is done: the code each
// level runs is its function's in the cache for as long as it runs.

// jitRegFrameOps are the calls of Go on the frame (ssa.GoHost,
// GoArgument, GoCells, GoClosure, GoClose) that Go makes for native code
// of a register frame (jitRegFrameOp); it leaves the others to an exit.
var jitRegFrameOps = map[ssa.GoOp]bool{ssa.GoHost: true, ssa.GoArgument: true, ssa.GoCells: true, ssa.GoClosure: true, ssa.GoClose: true}

// regLinkOf is the link native callers call cl's function's register code
// c through (ssa.CallSite's Entry): its code's call entry while it has
// code they may call (regSetLink), none otherwise -- a call of it then
// Go's, from native code.
func (s *jitState) regLinkOf(cl *closure, c *regCode) *jitLink {
	key := s.rt.jitKeyOf(cl.fn)
	if l := s.regLinks[key]; l != nil {
		return l
	}
	if s.regLinks == nil {
		s.regLinks = map[jitKey]*jitLink{}
	}
	l := &jitLink{key: key, params: 2 + uint64(c.params), locals: uint64(c.numRegs), frame: uint64(c.numRegs)}
	if e := s.regCache[key]; e != nil && e.code != nil && !e.invalid {
		l.regSet(e)
	}
	s.regLinks[key] = l
	return l
}

// regSet has native callers call e's code through l: its call entry, and
// the room its frame takes past the VM's stack's top, which a call checks
// (abi.Encoding's LinkFrame) -- never less than any code it had took.
func (l *jitLink) regSet(e *jitRegEntry) {
	l.setCode(e.code)
	l.frame = max(l.frame, uint64(e.p.Regs+e.scratch+e.frame))
}

// regLinkUpdate sets the link of the function whose key is key, if it has
// one, to call e's code, or none where e has none or is invalid.
func (s *jitState) regLinkUpdate(key jitKey, e *jitRegEntry) {
	l := s.regLinks[key]
	if l == nil {
		return
	}
	if e == nil || e.code == nil || e.invalid {
		l.clearCode()
		return
	}
	l.regSet(e)
}

// regCallSites are the native calls jitRegFeedback's NativeCalls answers
// for register instruction w, a call, at the program's instruction pc:
// of the function its site's cache holds, where native callers may call
// it, and of those the code learned the call makes at its calls of Go and
// its exits (jitRegCallSeen), up to jitCallTargets of them, as the stack
// form's code calls natively what jitCallSeen saw its calls call. What the
// cache holds is learned too, so that a call of it Go makes, its code not
// compiled yet, is not taken for news.
func (f *jitRegFeedback) regCallSites(pc int, w bytecode.RInstr, argc int, method bool) []ssa.CallSite {
	sites, _, _ := f.regCallTargets(pc, w, argc, method)
	return sites
}

// regCallTargets is regCallSites with each site's function object and
// closure, by index.
func (f *jitRegFeedback) regCallTargets(pc int, w bytecode.RInstr, argc int, method bool) ([]ssa.CallSite, []*Object, []*closure) {
	l := f.fb.e
	rpc := int32(f.p.Origin[pc])
	newW := w.Op() == bytecode.RNewW
	var sites []ssa.CallSite
	var objs []*Object
	var closures []*closure
	add := func(o *Object, cl *closure, rc *regCode, pool *abi.ObjectPool, byCode bool) {
		for i, s := range sites {
			if s.Callee == uintptr(unsafe.Pointer(o)) || s.ByCode != 0 && jitRegSameCode(s, cl) {
				return
			}
			if byCode && jitRegSameClosureCode(closures[i], cl) {
				// The same function's, checked by its code from here.
				sites, objs, closures = slices.Delete(sites, i, i+1), slices.Delete(objs, i, i+1), slices.Delete(closures, i, i+1)
				break
			}
		}
		if len(sites) < jitCallTargets {
			if site, ok := f.regCallSite(pc, o, cl, rc, argc, method, pool, byCode); ok {
				sites, objs, closures = append(sites, site), append(objs, o), append(closures, cl)
			}
		}
	}
	if calls := f.fb.cl.regCalls; !newW && uint(w.C()) < uint(len(calls)) {
		if e := &calls[w.C()]; jitRegCallKind(e) {
			add(e.callee, e.cl, e.rc, nil, false)
			if len(sites) != 0 && l != nil && !jitRegCallKnown(l, rpc, e.callee) {
				l.nativeCalls = append(l.nativeCalls, jitInline{pc: rpc, cl: e.cl, obj: e.callee})
			}
		}
	}
	if l != nil {
		for _, x := range l.nativeCalls {
			if x.pc == rpc && (x.pool != nil) == newW && !x.forEach {
				add(x.obj, x.cl, x.cl.reg, x.pool, x.byCode)
			}
		}
	}
	return sites, objs, closures
}

// jitRegSameClosureCode reports whether closures a and b run the same
// function in the same environment and realm.
func jitRegSameClosureCode(a, b *closure) bool {
	return a.fn == b.fn && a.env == b.env && a.realm == b.realm
}

// jitRegSameCode reports whether site, checked by its function's code,
// calls cl's.
func jitRegSameCode(site ssa.CallSite, cl *closure) bool {
	return site.ByCode == uintptr(unsafe.Pointer(cl.fn)) && site.Env == uintptr(unsafe.Pointer(cl.env)) && site.Realm == uintptr(unsafe.Pointer(cl.realm))
}

// jitRegCallKind reports whether e, a call site's cache, holds a callee the
// call path enters as a frame of its own where it does (regCallEnter,
// regCallThis, regCallObjThis): a leaf's too, which its calls of the
// interpreter answer without one.
func jitRegCallKind(e *regCallCache) bool {
	switch e.kind &^ regCallMarks {
	case regCallEnter, regCallThis, regCallObjThis:
		return e.callee != nil && e.cl != nil
	}
	return false
}

// jitRegCallKnown reports whether l, what a function's register code
// learned, knows that its call at register pc rpc calls o.
func jitRegCallKnown(l *jitEntry, rpc int32, o *Object) bool {
	for _, x := range l.nativeCalls {
		if x.pc == rpc && x.obj == o {
			return true
		}
	}
	return false
}

// jitRegCallSeen learns, after Go made the call at register pc rpc of c
// -- a call of Go native code made (jitRegGoCall), or one a host exit
// left to it -- the function its site's cache holds now, as the stack
// form's code learns a call's targets at its calls of Go (jitCallSeen):
// one native callers may call that the call was not known to make is the
// next compile's to call natively (regCallSites), the code stale from
// here (jitRegReopt); up to jitCallTargets functions a call. It reports whether it
// learned one.
func (r *Runtime) jitRegCallSeen(f *frame, c *regCode, e *jitRegEntry, rpc int, callee Value) bool {
	if e.invalid || rpc >= len(c.rc.Code) {
		return false
	}
	w := regAt(c.code, rpc)
	var o *Object
	var cl *closure
	switch w.Op() {
	case bytecode.RCallW, bytecode.RCallMW, bytecode.RCallWT, bytecode.RCallMWT:
		calls := f.cl.regCalls
		if uint(w.C()) >= uint(len(calls)) {
			return false
		}
		ce := &calls[w.C()]
		if !jitRegCallKind(ce) || ce.rc != ce.cl.reg || !r.jitRegCallable(ce.callee, ce.cl, ce.rc, int(w.X())) {
			return false
		}
		o, cl = ce.callee, ce.cl
	case bytecode.RNewW:
		// A construction's function, which no cache of the call's holds:
		// the one it constructed with, callee.
		if !callee.IsObject() {
			return false
		}
		o = callee.Object()
		if !r.jitRegConstructable(o, int(w.X())) {
			return false
		}
		cl = o.fn().closure
	default:
		return false
	}
	l := r.jit.regLearnedOf(r.jitKeyOf(f.cl.fn))
	n := 0
	for i := range l.nativeCalls {
		x := &l.nativeCalls[i]
		if int(x.pc) == rpc && x.pool == nil && x.obj != o && jitRegSameClosureCode(x.cl, cl) {
			// Another object of a function the call has called, a closure
			// its maker makes anew: called by its code from here
			// (ssa.CallSite's ByCode), as the stack form's call learns it
			// (jitSameCode).
			if x.byCode {
				r.jitRegNativeCallee(cl, cl.reg)
				return false
			}
			x.byCode = true
			r.jitRegReopt(f.cl, e)
			return true
		}
	}
	for _, x := range l.nativeCalls {
		if int(x.pc) == rpc {
			if x.obj == o {
				// Known: a call of it Go made for want of its code, which
				// native callers call once it has some.
				r.jitRegNativeCallee(cl, cl.reg)
				return false
			}
			n++
		}
	}
	if n >= jitCallTargets {
		return false
	}
	x := jitInline{pc: int32(rpc), cl: cl, obj: o}
	if w.Op() == bytecode.RNewW {
		// Its objects, made as the construction makes them, from a pool
		// (ssa.CallSite's Pool).
		if x.pool = r.jitNewPool(o); x.pool.Count == 0 {
			return false
		}
	}
	l.nativeCalls = append(l.nativeCalls, x)
	r.jitRegReopt(f.cl, e)
	return true
}

// jitRegConstructable reports whether native callers may construct with o
// natively, of argc arguments (regCallSite's pool): a plain function's
// object, a base constructor, of register code native callers may call,
// that does not read new.target and does not hand its arguments on, its
// prototype its own property, as regConstruct runs a construction in a
// register frame.
func (r *Runtime) jitRegConstructable(o *Object, argc int) bool {
	fd := o.fn()
	if fd == nil || fd.closure == nil || fd.bound || fd.extra != nil || fd.ctorKind != ctorBase || proxyOf(o) != nil {
		return false
	}
	cl := fd.closure
	if cl.fn.Kind != bytecode.KindNormal || cl.fn.Leaf == bytecode.LeafForward || cl.reg == nil || cl.reg.newTarget ||
		o.findOwn(atomPrototype) < 0 {
		return false
	}
	return r.jitRegCallable(o, cl, cl.reg, argc)
}

// jitRegCallable reports whether native callers may call o, cl's function
// object, whose register code is rc, with argc arguments (regCallSite).
func (r *Runtime) jitRegCallable(o *Object, cl *closure, rc *regCode, argc int) bool {
	fd := o.fn()
	if fd == nil || fd.closure != cl || fd.bound || cl.realm != r.Realm || rc == nil || rc != cl.reg || !jitRegCalled(cl, rc) ||
		rc.args || isClassConstructorKind(cl.fn.Kind) || cl.fn.HasDirectEval {
		return false
	}
	if cc := cl.closureCode; cc == nil || cc.jitRegRefused {
		return false
	}
	if r.nodeQuirks && argc > int(rc.keep) {
		return false
	}
	if x := fd.extra; x != nil && (!fd.arrow || x.lexThisRef != nil || x.rare != nil) {
		return false
	}
	return true
}

// regCallSite is jitRegFeedback's native call of o, cl's function object,
// whose register code is rc, at the call of the program's instruction pc,
// of argc arguments, a method's where method says (NativeCalls): through
// its link, its window made from the call's (ssa.CallSite's Window) -- or
// false for one Go makes: a callee of another realm, of a frame the call
// path makes otherwise (one that keeps its arguments, a leaf's, a class's
// constructor's, an arrow's sharing a derived constructor's this), one the
// JIT does not compile, or past the arguments a sloppy callee's frame keeps
// for f.arguments.
func (f *jitRegFeedback) regCallSite(pc int, o *Object, cl *closure, rc *regCode, argc int, method bool, pool *abi.ObjectPool, byCode bool) (ssa.CallSite, bool) {
	fb := f.fb
	r := fb.r
	if !r.jitRegCallable(o, cl, rc, argc) || pool != nil && (!r.jitRegConstructable(o, argc) || pool.Count == 0) {
		return ssa.CallSite{}, false
	}
	arrow, this := o.fn().arrow, cl.fn.UsesThis
	l := r.jitState().regLinkOf(cl, rc)
	k := fb.keep()
	k.links = append(k.links, l)
	k.holders = append(k.holders, o)
	site := ssa.CallSite{
		Callee: uintptr(unsafe.Pointer(o)), Closure: uintptr(unsafe.Pointer(cl)),
		Entry: uintptr(unsafe.Pointer(&l.entry)), Count: uintptr(unsafe.Pointer(&l.in)),
		Argc: argc, Method: method, Params: 2 + int(rc.params), LocalCount: int(rc.numRegs), ThisSlot: -1,
		Window: f.p.Code[pc].Left.Slot + 1,
	}
	if byCode && pool == nil {
		// Checked by its code, its environment and its realm, not its
		// object: the closure is the callee's at run time (mir's byCode).
		site.ByCode, site.Env, site.Realm = uintptr(unsafe.Pointer(cl.fn)), uintptr(unsafe.Pointer(cl.env)), uintptr(unsafe.Pointer(cl.realm))
	}
	if pool != nil {
		// A construction: its receiver from the pool, made as regConstruct
		// makes one, while the function's prototype is still the object
		// the pool's are of (mir's constructGuards).
		k.pools = append(k.pools, pool)
		site.Pool, site.ProtoIndex, site.ProtoKey = uintptr(unsafe.Pointer(pool)), int(o.findOwn(atomPrototype)), uint32(atomPrototype)
		return site, true
	}
	// A receiver, for a callee that reads one, as the call path gives it --
	// the window's register 1 is undefined otherwise, but for a method
	// call's (mir's windowSetup).
	switch {
	case !this:
	case arrow:
		// The this it captured (regCallThis).
		site.ArrowThis = true
	case !method:
		// Undefined, or, sloppy, the global object (regCallSlow).
		site.PlainThis = true
		if rc.coerceThis {
			if !r.globalThis.IsObject() {
				return ssa.CallSite{}, false
			}
			g := r.globalThis.Object()
			site.Global = uintptr(unsafe.Pointer(g))
			k.holders = append(k.holders, g)
		}
	case rc.coerceThis:
		// A receiver to coerce is Go's (regCallObjThis).
		site.Coerce = true
	}
	return site, true
}

// jitRegLevel is a native call under way whose callee runs register code,
// in context ctx: its closure cl, the callee's register code c and its
// code's entry e, its window at wb; pc the register pc it is at -- the
// call it makes, or, the innermost, where it left -- and back where it
// returns to in its caller's code, base its context's Base.
type jitRegLevel struct {
	ctx  int
	cl   *closure
	c    *regCode
	e    *jitRegEntry
	wb   int
	pc   int
	back uintptr
	base uint64
	// construct marks a construction's callee (its caller's new_w), this
	// its receiver, which the construction's result is if not an object.
	construct bool
	this      Value
}

// jitRegConstructs marks l, the level whose caller's code c makes the call
// at register pc rpc, a construction's where that is new_w: its receiver
// the one the call gave its context (abi.Context's This).
func (r *Runtime) jitRegConstructs(l *jitRegLevel, c *regCode, rpc int) {
	if rpc < len(c.rc.Code) && regAt(c.code, rpc).Op() == bytecode.RNewW {
		l.construct, l.this = true, *(*Value)(unsafe.Pointer(&r.jit.ssaCtxs[l.ctx].This))
	}
}

// jitRegLevelAt is the level of the native call running in context i,
// its code pinned (jitRegEntry's levels), or false where it is none of
// register code's.
func (r *Runtime) jitRegLevelAt(i int) (jitRegLevel, bool) {
	s := r.jit
	c := s.ssaCtxs[i]
	cl := (*closure)(c.Closure)
	if cl == nil || cl.reg == nil {
		return jitRegLevel{}, false
	}
	e := s.regCache[r.jitKeyOf(cl.fn)]
	if e == nil || e.code == nil || e.p == nil {
		return jitRegLevel{}, false
	}
	e.levels++
	return jitRegLevel{ctx: i, cl: cl, c: cl.reg, e: e, wb: int(c.Base), back: c.ReturnTo, base: c.Base}, true
}

// jitRegPushLevel makes the VM's frame of a level, as regEnterW makes a
// call's: its function, its window, its pc, its new.target undefined.
func (r *Runtime) jitRegPushLevel(l *jitRegLevel, pc uint32) *frame {
	off := r.frameDepth - r.frameBase
	if uint(off) >= uint(len(r.cur)) {
		r.seekFrameBlock()
		off = r.frameDepth - r.frameBase
	}
	r.frameDepth++
	if r.frameDepth > r.frameHigh {
		r.frameHigh = r.frameDepth
	}
	f := &r.cur[off]
	f.cl = l.cl
	f.callee = nil
	if v := r.stack[l.wb]; v.IsObject() {
		f.callee = v.Object()
	}
	f.base = l.wb + int(l.c.locals)
	f.pc = pc
	if len(f.openUpvalues) != 0 {
		f.openUpvalues = f.openUpvalues[:0]
	}
	if f.thisRef != nil || f.withScopes != nil || f.evalVars != nil || f.native != "" || f.args != nil {
		regClearFrame(f)
	}
	f.newTarget = Undefined
	if l.construct && f.callee != nil {
		f.newTarget = Obj(f.callee)
	}
	f.regKind = regFrameEnter
	return f
}

// jitRegPopLevel ends a level's frame f, as regEnterW ends a call's: what
// closures captured of it closed, the VM's stack's top back at its window.
func (r *Runtime) jitRegPopLevel(f *frame, wb int) {
	f.regKind = 0
	if len(f.openUpvalues) != 0 {
		regCloseUpvalues(f)
	}
	r.frameDepth--
	r.stackTop = wb
}

// jitRegRootOf is the context of the run of register code whose native
// calls run in the contexts up to idx, or -1 where there is none.
func (r *Runtime) jitRegRootOf(idx int) int {
	s := r.jit
	root := idx - 1
	for root >= 0 && s.ssaCtxs[root].Live == abi.LiveCall {
		root--
	}
	if root < 0 || s.ssaCtxs[root].Live != 0 || root >= len(s.regRuns) || s.regRuns[root].f == nil {
		return -1
	}
	return root
}

// jitRegGoCallNested is jitRegGoCall for a native callee's code, in
// context idx, of the run in context root: each level from the run's on
// is given the VM's frame while the call runs -- its function, its window,
// past the call it makes -- as the interpreter's frames are during a
// call, for a stack trace, and the depth of calls, to show; the call is
// made on the calling level's frame; and every level's code is pinned
// meanwhile. A call that threw, or after which the level's code is
// invalid, Go finishes at the exit native code then takes (regPending),
// once the callee levels have left native code (jitRegUnwind).
func (r *Runtime) jitRegGoCallNested(ctx *abi.Context, idx, root int) bool {
	s := r.jit
	if idx >= len(s.ssaCtxs) || s.ssaCtxs[idx] != ctx {
		return false
	}
	n := idx - root
	var buf [8]jitRegLevel
	levels := buf[:0]
	if n > len(buf) {
		levels = make([]jitRegLevel, 0, n)
	}
	ok := true
	for i := root + 1; i <= idx; i++ {
		l, found := r.jitRegLevelAt(i)
		if !found {
			ok = false
			break
		}
		levels = append(levels, l)
	}
	if ok {
		run := s.regRuns[root]
		rpc, _ := jit.CallPC(s.ssaCtxs[root])
		run.f.pc = regSrc(run.c, int(rpc)) + 1
		caller, at := run.c, int(rpc)
		for k := range levels {
			l := &levels[k]
			r.jitRegConstructs(l, caller, at)
			if k < len(levels)-1 {
				p, _ := jit.CallPC(s.ssaCtxs[l.ctx])
				caller, at = l.c, int(p)
			}
		}
		var fbuf [8]*frame
		frames := fbuf[:0]
		if n > len(fbuf) {
			frames = make([]*frame, 0, n)
		}
		for k := range levels {
			l := &levels[k]
			pc := uint32(0)
			if k < len(levels)-1 {
				rpc, _ := jit.CallPC(s.ssaCtxs[l.ctx])
				pc = regSrc(l.c, int(rpc)) + 1
			}
			frames = append(frames, r.jitRegPushLevel(l, pc))
		}
		ctxTop := s.ctxTop
		s.ctxTop = max(ctxTop, idx+1)
		last := &levels[len(levels)-1]
		top := r.stackTop
		ok = r.jitRegGoCallOn(ctx, idx, jitRegRun{f: frames[len(frames)-1], c: last.c, e: last.e, wb: last.wb})
		s.ctxTop = ctxTop
		for k := len(frames) - 1; k >= 0; k-- {
			r.jitRegPopLevel(frames[k], levels[k].wb)
		}
		r.stackTop = top
	}
	for k := range levels {
		levels[k].e.levels--
	}
	return ok
}

// jitRegUnwind finishes the native calls the code running in context idx
// made, the innermost of which left native code, and returns what the
// outermost returned or threw, as jitUnwindNative finishes the stack
// form's: each level is given the VM's frame, as regEnterW would have made
// it, from the outermost; the innermost is finished from where it left, as
// the run of a frame's code is (jitRegLeft); then each level from the call
// it made, with what its callee answered: in its native code, where the
// call returns to, if its code is still its function's and valid -- its
// context, spills and keeps as it left them -- or, its call's state written
// to its window (jit.ExpandCall), by Go, the answer to the call's register
// or the exception to the level's handlers (regPending), and on from there.
// Each level's code is pinned until the level is done.
func (r *Runtime) jitRegUnwind(idx int) (Value, error) {
	s := r.jit
	var levels []jitRegLevel
	for i := idx + 1; i < len(s.ssaCtxs) && s.ssaCtxs[i].Live == abi.LiveCall; i++ {
		l, ok := r.jitRegLevelAt(i)
		if !ok {
			// Native code calls only register code natively from register
			// code, and its levels' code stays in the cache while they run.
			panic("vm: a native call of register code whose callee has no code")
		}
		levels = append(levels, l)
	}
	n := len(levels)
	for k := range levels {
		l := &levels[k]
		c := s.ssaCtxs[l.ctx]
		if k < n-1 {
			rpc, _ := jit.CallPC(c)
			l.pc = int(rpc)
		} else {
			if c.ExitKind == abi.ExitTable {
				// An exit that left its frame and its fields to Go, which
				// writes them before they are read.
				s.ssaRecords += uint64(s.exitScratch.Apply(c))
			}
			l.pc = int(c.ExitPC)
		}
		c.Live, c.ReturnTo = 0, 0
	}
	if n != 0 {
		rpc, _ := jit.CallPC(s.ssaCtxs[idx])
		caller, at := s.regRuns[idx].c, int(rpc)
		for k := range levels {
			r.jitRegConstructs(&levels[k], caller, at)
			caller, at = levels[k].c, levels[k].pc
		}
	}
	s.unwound += uint64(n)
	// What Go runs to finish a level runs in contexts past every level's:
	// a level's native code, resumed after its call, finds its spills and
	// keeps where it left them.
	ctxTop := s.ctxTop
	s.ctxTop = max(ctxTop, idx+n+1)
	frames := make([]*frame, n)
	for k := range levels {
		l := &levels[k]
		pc := regSrc(l.c, l.pc)
		if k < n-1 {
			// Past the call it makes, as the interpreter's frame is during
			// one, which a stack trace shows.
			pc++
		}
		frames[k] = r.jitRegPushLevel(l, pc)
		// Its captured registers' cells, closed while it ran natively, open
		// on its window as the interpreter's are.
		r.jitRegReopenCells(frames[k], l.e, l.wb, s.ssaCtxs[l.ctx], false)
	}
	var v Value
	var err error
	for k := n - 1; k >= 0; k-- {
		l, f := &levels[k], frames[k]
		c := s.ssaCtxs[l.ctx]
		switch {
		case k == n-1:
			// Its records first, which the objects its code did not make
			// are materialized from with its inlined callees' (jitRegInlined);
			// applied, none is left for jitRegFinishLevel.
			r.jitRegApplyRecords(f, l.e.p, l.wb, c)
			if iv, ierr, ok := r.jitRegInlined(l.ctx, l.cl, l.e, l.pc, l.wb); ok {
				// It left inside a callee its code inlined, at the call
				// it is inlined at: Go finishes the callee, and the call
				// goes on with what it answered (regPending).
				s.regPending = jitGoPending{set: true, pc: l.pc, ctx: l.ctx, v: iv, err: ierr}
			}
			v, err = r.jitRegFinishLevel(f, l, c)
		case err == nil && r.stopped == nil && !l.e.invalid && l.e.code != nil && levels[k+1].back != 0 && l.e.code.Contains(levels[k+1].back):
			// On in its native code where the call returns to.
			callee := s.ssaCtxs[l.ctx+1]
			callee.Base, callee.Live, callee.ReturnTo = levels[k+1].base, 0, 0
			*(*Value)(unsafe.Pointer(&callee.RetValue)) = v
			s.resumed++
			s.resumedLevels++
			var pc int
			var done bool
			pc, v, done, err = r.jitRegGoOnAt(f, l.c, l.e, l.wb, 0, l.ctx, levels[k+1].back)
			if err == nil && !done {
				v, err = r.runReg(f, l.c, pc)
			}
		default:
			jit.ExpandCall(c)
			s.regPending = jitGoPending{set: true, pc: l.pc, ctx: l.ctx, v: v, err: err}
			v, err = r.jitRegFinishLevel(f, l, c)
		}
		if err == errRegRestart {
			// Its frame given to a callee in place by a call in tail
			// position (regRestartRun).
			v, err = r.regRestartRun(f)
		}
		r.jitRegPopLevel(f, l.wb)
		l.e.levels--
		if err == errTailCall {
			// Ended in a call in tail position, its frame given up: the
			// call made here.
			v, err = r.regTailRest()
		}
		if l.construct && err == nil && !v.IsObject() {
			// A construction's result is its receiver, unless an object.
			v = l.this
		}
	}
	s.ctxTop = ctxTop
	return v, err
}

// jitRegFinishLevel finishes level l, whose frame is f, from where its code
// left in its context c: its records written, then on as a run of the
// frame's code goes on from an exit (jitRegLeft) -- in native code, at the
// next entry, or in the interpreter -- to its end.
func (r *Runtime) jitRegFinishLevel(f *frame, l *jitRegLevel, c *abi.Context) (Value, error) {
	s := r.jit
	if c.ExitKind == abi.ExitTable {
		s.ssaRecords += uint64(s.exitScratch.Apply(c))
	}
	r.jitRegApplyRecords(f, l.e.p, l.wb, c)
	next, pc, v, done, err, native := r.jitRegLeft(f, l.c, l.e, l.wb, c, Undefined)
	if native {
		pc, v, done, err = r.jitRegGoOn(f, l.c, l.e, l.wb, next)
	}
	if err == nil && !done {
		v, err = r.runReg(f, l.c, pc)
	}
	return v, err
}
