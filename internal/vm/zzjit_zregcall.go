//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64))

package vm

import (
	"slices"
	"unsafe"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
	"github.com/go-quickjs/go-quickjs/internal/jit"
	"github.com/go-quickjs/go-quickjs/internal/jit/abi"
	jitcompile "github.com/go-quickjs/go-quickjs/internal/jit/compile"
	"github.com/go-quickjs/go-quickjs/internal/jit/ir"
	"github.com/go-quickjs/go-quickjs/internal/jit/mir"
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
	key := s.rt.jitRegKeyOf(cl)
	if l := s.regLinks[key]; l != nil {
		s.regHint(cl, l)
		return l
	}
	if s.regLinks == nil {
		s.regLinks = map[jitKey]*jitLink{}
	}
	l := &jitLink{key: key, params: 2 + uint64(c.params), locals: uint64(c.numRegs), frame: uint64(c.numRegs),
		flags: s.rt.jitRegLinkFlags(cl, c)}
	if e := s.regCache[key]; e != nil && e.code != nil && !e.invalid && (e.life == nil || !e.life.notNative) {
		l.regSet(s, e)
	}
	s.regLinks[key] = l
	s.regHint(cl, l)
	return l
}

// jitRegLinkFlags are the flags of the link of cl's function, whose
// register code is c (abi.LinkCoerce...), as jitLinkFlags are the stack
// form's: what a generic call reads of a function it has no site for
// (ssa.CallSite's Generic). LinkParams counts the window's function and
// receiver. A function a generic call may not make is one native callers
// do not call natively at a call compiled for it either (jitRegCallable):
// a class's constructor, a getter's, a setter's; one whose call is not its
// body, a generator's or an async function's; one that keeps its
// arguments, or runs a direct eval; top-level code; and, under
// WithNodeQuirks, a sloppy function, whose f.arguments reads arguments
// past its parameters a native call does not keep.
func (r *Runtime) jitRegLinkFlags(cl *closure, c *regCode) uint64 {
	fn := cl.fn
	var f uint64
	if c.coerceThis {
		f |= abi.LinkCoerce
	}
	if fn.HasTailCall {
		f |= abi.LinkTail
	}
	if fn.Kind == bytecode.KindArrow {
		f |= abi.LinkArrow
	}
	if fn.Kind != bytecode.KindNormal && fn.Kind != bytecode.KindArrow && fn.Kind != bytecode.KindMethod ||
		!jitRegCalled(cl, c) || c.args || c.newTarget || fn.HasDirectEval || len(fn.EvalScopes) != 0 || r.nodeQuirks && !fn.Strict {
		f |= abi.LinkNotGeneric
	}
	return f
}

// regHint gives cl's template a slot of the hint table holding l, its
// function's link, as the stack form's entries have one (remember): what
// a generic call finds a function's link by (mir's genericCheck). A link
// keeps its slot until its function is gone (regBytes, regUnhint).
func (s *jitState) regHint(cl *closure, l *jitLink) {
	if l.hint == 0 {
		var i uint32
		if n := len(s.hintFree); n != 0 {
			i, s.hintFree = s.hintFree[n-1], s.hintFree[:n-1]
		} else {
			if n := s.hintsCap(); n != cap(s.hints) {
				s.hints = append(make([]jitHintSlot, 0, n), s.hints...)
			}
			if len(s.hints) == 0 {
				// Slot 0 is no hint.
				s.hints = append(s.hints, jitHintSlot{})
			}
			i = uint32(len(s.hints))
			s.hints = append(s.hints, jitHintSlot{})
		}
		s.hints[i] = jitHintSlot{fn: uintptr(unsafe.Pointer(cl.fn)), link: l}
		s.hintTable = abi.HintTable{Slots: unsafe.Pointer(unsafe.SliceData(s.hints)), Len: uint64(len(s.hints))}
		l.hint = i
	}
	if cl.hint() != l.hint {
		cl.setHint(l.hint)
	}
}

// regUnhint frees l's slot of the hint table, its function gone.
func (s *jitState) regUnhint(l *jitLink) {
	if l.hint != 0 {
		s.hints[l.hint] = jitHintSlot{}
		s.hintFree = append(s.hintFree, l.hint)
		l.hint = 0
	}
}

// regSet has native callers call e's code through l: its call entry, and
// the room its frame takes past the VM's stack's top, which a call checks
// (abi.Encoding's LinkFrame) -- never less than any code it had took; a
// generator's steps its generator entry, from which on s's native callees
// may be generators' resumptions (jitState's regGens).
func (l *jitLink) regSet(s *jitState, e *jitRegEntry) {
	l.setCode(e.code)
	if l.entry != 0 {
		// A call in tail position goes on at the code's tail entry, which
		// makes the frame past its window as the code's call entry would
		// (mir's windowTailJump, tailEntry).
		l.body = e.code.TailEntry()
	}
	l.frame = max(l.frame, uint64(e.p.Regs+e.scratch+e.frame))
	if room := uint64(e.scratch + e.frame); room > s.regRoom {
		// Room for its frame past its window below every context's
		// StackEnd before any native caller calls it.
		s.regRoom = room
		s.rt.jitStackRoom()
	}
	if l.gen != 0 {
		s.regGens = true
	}
}

// jitStackRoom writes into every context, and into what they share
// (jitShared), the end of the VM's stack native code checks a call's room
// against (abi.Context's StackEnd): its length, less the most any register
// code takes past its window (jitState's regRoom). A native call of
// register code then checks the callee's window alone, a constant, as the
// stack form's call checks its callee's locals and operands -- the
// register frame's size past its window is no constant of its function,
// as the stack form's is, but its compile's: its scratch slots, its loops'
// slots and its objects' homes (mir's frameSize).
func (r *Runtime) jitStackRoom() {
	s := r.jit
	end := r.jitStackEnd()
	s.ssaShared.stackEnd = end
	for _, c := range s.ssaCtxs {
		c.StackEnd = end
	}
}

// jitStackEnd is the contexts' StackEnd (jitStackRoom).
func (r *Runtime) jitStackEnd() uint64 {
	n := uint64(len(r.stack))
	if room := r.jit.regRoom; room < n {
		return n - room
	}
	return 0
}

// regLinkUpdate sets the link of the function whose key is key, if it has
// one, to call e's code, or none where e has none or is invalid.
func (s *jitState) regLinkUpdate(key jitKey, e *jitRegEntry) {
	l := s.regLinks[key]
	if l == nil {
		return
	}
	if e == nil || e.code == nil || e.invalid || e.life != nil && e.life.notNative {
		l.clearCode()
		return
	}
	l.regSet(s, e)
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
	var t jitRegTargets
	f.regCallTargets(&t, pc, w, argc, method)
	if t.n == 0 {
		return nil
	}
	return slices.Clone(t.sites[:t.n])
}

// jitRegTargets is regCallTargets' answer: up to jitCallTargets native
// calls, each with its function object and closure, by index, where the
// asker keeps it. Three lists grown a call at a time, and the closure that
// grew them, had been a fifth of what a register build allocated.
type jitRegTargets struct {
	n     int
	sites [jitCallTargets]ssa.CallSite
	objs  [jitCallTargets]*Object
	cls   [jitCallTargets]*closure
}

// regCallTargets is regCallSites with each site's function object and
// closure, in t.
func (f *jitRegFeedback) regCallTargets(t *jitRegTargets, pc int, w bytecode.RInstr, argc int, method bool) {
	l := f.fb.e
	rpc := int32(f.p.Origin[pc])
	newW := w.Op() == bytecode.RNewW
	if calls := f.fb.cl.regCalls; !newW && w.Op() != bytecode.RApplyArgs && uint(w.C()) < uint(len(calls)) {
		if e := &calls[w.C()]; jitRegCallKind(e) {
			f.regAddTarget(t, pc, argc, method, e.callee, e.cl, e.rc, nil, nil, false, false)
			if t.n != 0 && l != nil && !jitRegCallKnown(l, rpc, e.callee) {
				l.nativeCalls = append(l.nativeCalls, jitInline{pc: rpc, cl: e.cl, obj: e.callee})
			}
		}
	}
	if method && l != nil && f.rc != nil {
		// The other functions the call's method read has met, of the other
		// shapes its cache saw, each called natively from the first compile
		// and learned so, as the stack form's code seeds a polymorphic
		// method call (jitSeenCallees, jitSeedNativeCalls): left to the site's
		// cache's one, the code knew one function of a call of several,
		// learned the others an exit at a time and waited a budget for each
		// compile -- V8's Richards' TaskControlBlock.run, compiled for its
		// native callers knowing one of its four tasks' run, left native
		// code at most of its first run's calls.
		for _, c := range f.fb.r.jitRegSeenCallees(f.fb.cl, f.rc, int(rpc), w) {
			if c.cl.reg == nil {
				continue
			}
			n := t.n
			f.regAddTarget(t, pc, argc, method, c.o, c.cl, c.cl.reg, nil, nil, false, false)
			if t.n > n && !jitRegCallKnown(l, rpc, c.o) {
				l.nativeCalls = append(l.nativeCalls, jitInline{pc: rpc, cl: c.cl, obj: c.o})
			}
		}
	}
	if newW && l != nil && !slices.ContainsFunc(l.nativeCalls, func(x jitInline) bool { return x.pc == rpc && x.pool != nil }) {
		// A construction no call of Go has taught the code yet, of the
		// function the binding read before it holds now, as the stack
		// form's compiles seed one (jitSeedCalls, jitCalleeObjectAt): its
		// objects from a pool, from the first compile, the construction
		// checking its function -- and in a callee inlined, whose own code
		// may never be compiled, as the stack form's callee compiled for
		// its native callers is seeded so. Learned only at calls of Go,
		// a callee inlined from the first made each of its constructions
		// by Go until one left the code that inlined it.
		if o := f.regConstructorNow(int(rpc), int(w.A())); o != nil {
			f.fb.r.jitRegSeedConstruct(l, rpc, o, argc)
		}
	}
	if l != nil {
		for _, x := range l.nativeCalls {
			if x.pc == rpc && (x.pool != nil) == newW && !x.forEach && x.cl != nil && x.active == nil {
				f.regAddTarget(t, pc, argc, method, x.obj, x.cl, x.cl.reg, x.pool, x.parent, x.byCode && !x.via, x.via)
			}
		}
	}
}

// regAddTarget adds to t the native call of o, cl's, running rc, at the
// program's instruction pc (regCallSite), unless t has it: the same object
// called by its object, or the same function's code called by its code, or
// one called through Function.prototype.call where the other is not (via).
// A call by code takes the place of one of an object of the same function.
func (f *jitRegFeedback) regAddTarget(t *jitRegTargets, pc, argc int, method bool, o *Object, cl *closure, rc *regCode, pool *abi.ObjectPool, parent *Object, byCode, via bool) {
	for i := 0; i < t.n; i++ {
		s := &t.sites[i]
		if s.ByCode != 0 && jitRegSameCode(*s, cl) || s.Callee == uintptr(unsafe.Pointer(o)) && !byCode || (s.Via != 0) != via {
			return
		}
		if byCode && jitRegSameClosureCode(t.cls[i], cl) {
			// The same function's -- the same object, which the site's
			// cache holds, among them -- checked by its code from here.
			copy(t.sites[i:t.n], t.sites[i+1:t.n])
			copy(t.objs[i:t.n], t.objs[i+1:t.n])
			copy(t.cls[i:t.n], t.cls[i+1:t.n])
			t.n--
			t.sites[t.n], t.objs[t.n], t.cls[t.n] = ssa.CallSite{}, nil, nil
			break
		}
	}
	if t.n < jitCallTargets {
		if site, ok := f.regCallSite(pc, o, cl, rc, argc, method, pool, parent, byCode, via); ok {
			t.sites[t.n], t.objs[t.n], t.cls[t.n] = site, o, cl
			t.n++
		}
	}
}

// jitRegSeenCallees are the functions the method call w at register pc rpc
// of cl's register code rc has called as its method read's cache saw them,
// as jitSeenCallees finds the stack form's: the read is the get_method_w
// before the call that puts the method in the call's window's first
// register, and of each shape its cache met -- its entry, those it keeps
// for other shapes (propCache.poly) and those it had before (seen) -- the
// method a prototype holds now, each a function native callers may call.
// A call checks its callee, so one that turns out wrong costs an exit.
func (r *Runtime) jitRegSeenCallees(cl *closure, rc *bytecode.RegCode, rpc int, w bytecode.RInstr) []jitCallee {
	if w.Op() != bytecode.RCallMW && w.Op() != bytecode.RCallMWT || rpc <= 0 || rpc > len(rc.Code) {
		return nil
	}
	fa := int(w.A())
	read := -1
	for q := rpc - 1; q >= 0 && q >= rpc-1-jitRegSeenReach; q-- {
		if x := rc.Code[q]; x.Op() == bytecode.RGetMethodW && int(x.A()) == fa {
			read = q
			break
		}
	}
	if read < 0 {
		return nil
	}
	site := int(rc.Code[read].C())
	if site >= len(cl.ic) || site >= len(rc.SiteName) || int(rc.SiteName[site]) >= len(cl.names) {
		return nil
	}
	key := cl.names[rc.SiteName[site]]
	var out []jitCallee
	take := func(c *propCache) {
		h := c.p1
		if c.p2 != nil {
			h = c.p2
		}
		if c.shape == nil || c.shape == noShape || c.shape == dictShape || c.getter || h == nil || c.idx < 0 || int(c.idx) >= int(h.valn) {
			return
		}
		prop := h.propAt(c.idx)
		if prop.key != key || prop.flags&(propAccessor|propPrivate|propDeleted|propUninit) != 0 || !prop.value.IsObject() {
			return
		}
		mo, mc := r.jitCallableClosure(cl, prop.value.Object())
		if mc == nil || slices.ContainsFunc(out, func(x jitCallee) bool { return x.o == mo }) {
			return
		}
		out = append(out, jitCallee{mo, mc})
	}
	c := &cl.ic[site]
	take(c)
	if p := c.poly; p != nil {
		for i := range p.entries[:p.n] {
			take(&p.entries[i])
		}
	}
	if seen := c.seenOf(); seen != nil {
		for i := range seen.entries[:seen.n] {
			take(&seen.entries[i])
		}
	}
	return out
}

// jitRegSeenReach is how far before a method call jitRegSeenCallees looks
// for the read of its method: past the instructions that put the call's
// arguments in its window.
const jitRegSeenReach = 32

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
// next compile's to call natively (regCallSites), the code to be compiled
// again for it (jitRegReopt); up to jitCallTargets functions a call. It reports whether it
// learned one.
func (r *Runtime) jitRegCallSeen(f *frame, c *regCode, e *jitRegEntry, rpc int, callee Value) bool {
	return r.jitRegCallSeenOf(f, c, e, rpc, callee, false)
}

// jitRegCallSeenOf is jitRegCallSeen, where applied says the call, an
// apply_args, was through the realm's apply, as its call of Go checked
// (jitRegForwardMade): its window's registers native code may not have
// written.
func (r *Runtime) jitRegCallSeenOf(f *frame, c *regCode, e *jitRegEntry, rpc int, callee Value, applied bool) bool {
	if rpc >= len(c.rc.Code) {
		return false
	}
	if l := r.jitRegLearnedFor(f, e); l != nil {
		// Made: a call no longer seldom made (InlineExit), as the stack
		// form's code counts its calls (jitCallSeen's callSites).
		if len(l.regCallsMade) != len(c.rc.Code) {
			l.regCallsMade = make([]uint8, len(c.rc.Code))
		}
		if l.regCallsMade[rpc] < ^uint8(0) {
			l.regCallsMade[rpc]++
		}
	}
	if e.invalid {
		return false
	}
	w := regAt(c.code, rpc)
	var o, parent *Object
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
	case bytecode.RApplyArgs:
		// The function f.apply(x, arguments) called, which no cache holds:
		// callee, if apply was the realm's -- learned once the forwards
		// settle (jitRegForwardSeen).
		if !callee.IsObject() || r.applyFn == nil || !applied && (!r.stack[jitRegWindowOf(f, c)+int(w.B())+1].IsObject() ||
			r.stack[jitRegWindowOf(f, c)+int(w.B())+1].Object() != r.applyFn) {
			return false
		}
		o = callee.Object()
		if fd := o.fn(); fd != nil && fd.closure != nil && r.jitRegCallable(o, fd.closure, fd.closure.reg, 0) {
			cl = fd.closure
		}
		if learned, done := r.jitRegForwardSeen(f, c, e, rpc, o, cl); done || cl == nil {
			return learned
		}
	case bytecode.RNewW:
		// A construction's function, which no cache of the call's holds:
		// the one it constructed with, callee.
		if !callee.IsObject() {
			return false
		}
		o = callee.Object()
		if o == r.proto.arrayCtor {
			// new Array() or new Array(n): made from a pool (jitRegAllocSeen).
			return r.jitRegAllocSeen(f, c, e, rpc, w, o)
		}
		var ok bool
		if cl, parent, ok = r.jitRegConstructTarget(o, int(w.X())); !ok {
			return false
		}
	default:
		return false
	}
	l := r.jitRegLearnedFor(f, e)
	n := 0
	for i := range l.nativeCalls {
		x := &l.nativeCalls[i]
		if int(x.pc) == rpc && x.pool == nil && x.obj != o && jitRegSameClosureCode(x.cl, cl) {
			// Another object of a function the call has called, a closure
			// its maker makes anew: called by its code from here
			// (ssa.CallSite's ByCode), as the stack form's call learns it
			// (jitSameCode).
			if x.byCode {
				// Known by its code -- or past the functions the code calls
				// natively, which a generic call calls through its link,
				// found by its hint (regHint), given it here as for one
				// known by its object (below). Without it, a callback made
				// anew at each call of its maker, the fifth a call made --
				// AWFY's Havlak's Vector.forEach's of stepD's arrow -- was
				// made by Go at every call.
				if w.Op() != bytecode.RNewW {
					r.jitState().regLinkOf(cl, cl.reg)
				}
				r.jitRegKnownCallee(cl, f.cl)
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
				// native callers call once it has some -- or one known
				// past the functions the code calls natively, which
				// regCallTargets fills with the site's cache's first: a
				// generic call calls it through its link, which it finds
				// by its hint (regHint), given it here as below.
				if w.Op() != bytecode.RNewW {
					r.jitState().regLinkOf(cl, cl.reg)
				}
				r.jitRegKnownCallee(cl, f.cl)
				return false
			}
			n++
		}
	}
	if n >= jitCallTargets {
		if w.Op() != bytecode.RNewW {
			// Past the functions it calls natively, one a generic call
			// calls through its link (ssa.CallSite's Generic), which it
			// finds by its hint (regHint): given one, and code native
			// callers may call, as the stack form's generic calls have
			// their functions compiled (jitCallTarget). Go had made every
			// call of one no site was compiled for.
			r.jitState().regLinkOf(cl, cl.reg)
			r.jitRegKnownCallee(cl, f.cl)
		}
		return false
	}
	x := jitInline{pc: int32(rpc), cl: cl, obj: o, parent: parent}
	if n == 0 && w.Op() != bytecode.RNewW {
		// A call that has learned no function yet: settling, as the stack
		// form's jitCallSeen has it, the functions it meets in its first
		// jitCallsToInline calls of Go are decided together, the code
		// compiled again once for them, not once for each. A construction
		// is learned at its first, its objects made from a pool from then
		// on (jitRegCallTarget): settled, a factory's two constructions a
		// run had stayed Go's for the runs settling took.
		met, settled := r.jitRegCallSettling(l, c, rpc, x)
		if !settled {
			return false
		}
		learned := r.jitRegCallTarget(l, w, x)
		for _, m := range met {
			if jitCountAt(l.nativeCalls, rpc) >= jitCallTargets {
				break
			}
			if k := slices.IndexFunc(l.nativeCalls, func(y jitInline) bool {
				return int(y.pc) == rpc && y.pool == nil && jitRegSameClosureCode(y.cl, m.cl)
			}); k >= 0 {
				// Another object of a function it met, called by its code
				// (ssa.CallSite's ByCode), as below.
				l.nativeCalls[k].byCode = true
				continue
			}
			learned = r.jitRegCallTarget(l, w, m) || learned
		}
		if learned {
			r.jitRegReopt(f.cl, e)
		}
		return learned
	}
	if !r.jitRegCallTarget(l, w, x) {
		return false
	}
	r.jitRegReopt(f.cl, e)
	return true
}

// jitRegCallRepeats reports whether the call at register pc rpc of c,
// which e's code left to Go at a host exit, Go having made it -- callee
// the function a construction constructed with -- calls a target an
// earlier exit there taught the function (its native calls) that the
// code was not compiled for (e's targets), while the code waits to be
// compiled again for what it learned, as the stack form's
// jitCallRepeats: an exit that teaches nothing, which the code would make
// at every such call until then.
func (r *Runtime) jitRegCallRepeats(f *frame, c *regCode, e *jitRegEntry, rpc int, callee Value) bool {
	if e == nil || e.life == nil || e.invalid || e.code == nil || !e.life.reopt || rpc >= len(c.rc.Code) {
		return false
	}
	var o *Object
	var cl *closure
	switch w := regAt(c.code, rpc); w.Op() {
	case bytecode.RCallW, bytecode.RCallMW:
		calls := f.cl.regCalls
		if uint(w.C()) >= uint(len(calls)) {
			return false
		}
		ce := &calls[w.C()]
		if !jitRegCallKind(ce) {
			return false
		}
		o, cl = ce.callee, ce.cl
	case bytecode.RNewW:
		if !callee.IsObject() {
			return false
		}
		o = callee.Object()
		if fd := o.fn(); fd != nil {
			cl = fd.closure
		}
	default:
		return false
	}
	if o == nil || cl == nil {
		return false
	}
	is := func(p int32, byCode bool, obj *Object, tcl *closure) bool {
		return int(p) == rpc && (obj == o || byCode && tcl != nil && jitRegSameClosureCode(tcl, cl))
	}
	l := r.jitRegLearnedFor(f, e)
	return l != nil && slices.ContainsFunc(l.nativeCalls, func(x jitInline) bool { return is(x.pc, x.byCode, x.obj, x.cl) }) &&
		!slices.ContainsFunc(e.targets, func(t jitTarget) bool { return is(t.pc, t.byCode, t.obj, t.cl) })
}

// jitRegCallSettling counts the call of Go of x's function at register pc
// rpc of c toward its call's settling (jitLearned's callSites), as the
// stack form's jitCallSeen counts it: one of the first jitCallsToInline is
// kept with the others the call met (settling), unsettled; the last
// settles the call (jitCallNative), and the functions it met before, but
// x's, are returned, met, to be decided with it. A call decided before has
// settled.
func (r *Runtime) jitRegCallSettling(l *jitEntry, c *regCode, rpc int, x jitInline) (met []jitInline, settled bool) {
	if len(l.callSites) != len(c.rc.Code) {
		l.callSites = make([]uint8, len(c.rc.Code))
	}
	switch n := l.callSites[rpc]; {
	case n == jitCallNative || n == jitCallDone:
		return nil, true
	case n+1 < jitCallsToInline:
		l.callSites[rpc]++
		if !slices.ContainsFunc(l.settling, func(y jitInline) bool { return int(y.pc) == rpc && y.obj == x.obj }) {
			l.settling = append(l.settling, x)
		}
		return nil, false
	}
	l.callSites[rpc] = jitCallNative
	for i := 0; i < len(l.settling); i++ {
		if y := l.settling[i]; int(y.pc) == rpc {
			if y.obj != x.obj {
				met = append(met, y)
			}
			l.settling = slices.Delete(l.settling, i, i+1)
			i--
		}
	}
	return met, true
}

// jitRegCallTarget adds x, a function the call w at x's register pc calls,
// to l's native calls (regCallSites), a construction's with the pool its
// objects are made from (ssa.CallSite's Pool). It reports whether it did:
// not a construction whose pool is empty.
func (r *Runtime) jitRegCallTarget(l *jitEntry, w bytecode.RInstr, x jitInline) bool {
	if w.Op() == bytecode.RNewW {
		if x.pool = r.jitNewPool(x.obj); x.pool.Count == 0 {
			return false
		}
	}
	l.nativeCalls = append(l.nativeCalls, x)
	return true
}

// regConstructorNow is the function the construction at register pc rpc,
// whose function is in register reg, constructs with if the binding read
// into reg last before it -- a global's, a captured one's -- still holds
// it, as the stack form's jitCalleeObjectAt finds a call's function past
// its arguments' instructions: the nearest instruction before rpc that
// writes reg (its A) is that read (regDefined). nil for any other. What it
// finds is a guess the construction checks, as the stack form's seed is.
func (f *jitRegFeedback) regConstructorNow(rpc, reg int) *Object {
	for q := rpc - 1; q >= 0 && q < len(f.rc.Code); q-- {
		if int(f.rc.Code[q].A()) != reg {
			continue
		}
		v, ok := f.regDefined(q+1, reg)
		if !ok || !v.IsObject() {
			return nil
		}
		return v.Object()
	}
	return nil
}

// jitRegSeedConstruct has l, what a function's register code learned,
// know that its construction at register pc rpc, of argc arguments,
// constructs with o, its objects from a pool, as a call of Go there would
// teach it (jitRegCallSeen) and as the stack form's compiles seed a
// construction (jitSeedCalls): not new Array(), whose arrays native code
// makes (jitRegAllocSeen), nor a function native code does not construct
// with (jitRegConstructTarget).
func (r *Runtime) jitRegSeedConstruct(l *jitEntry, rpc int32, o *Object, argc int) {
	if o == r.proto.arrayCtor {
		return
	}
	cl, parent, ok := r.jitRegConstructTarget(o, argc)
	if !ok {
		return
	}
	pool := r.jitNewPool(o)
	if pool.Count == 0 {
		return
	}
	l.nativeCalls = append(l.nativeCalls, jitInline{pc: rpc, cl: cl, obj: o, parent: parent, pool: pool})
}

// jitRegForwardSeen is jitRegCallSeen's for the apply_args at register
// pc rpc of f's code, c, e's, that Go made of o, a function native callers
// may call, cl's, or one they do not, cl nil: settling, as the stack
// form's jitForwardSeen has it, the functions it meets in its first
// jitCallsToInline calls are decided together (jitLearned's callSites and
// settling), the code compiled again once for them, not once for each,
// those native callers may call learned as its native calls, up to
// jitCallTargets. It reports, done, whether the call is settled here --
// learned saying whether it learned something -- and not, decided before
// as native, for jitRegCallSeen to learn of o as of any call. Learned one
// at a time, and guessed before any was, the first target a forwarder met
// had it inlined in its callers for that target alone
// (jitRegForwardTarget).
func (r *Runtime) jitRegForwardSeen(f *frame, c *regCode, e *jitRegEntry, rpc int, o *Object, cl *closure) (learned, done bool) {
	l := r.jitRegLearnedFor(f, e)
	if l == nil {
		return false, true
	}
	if len(l.callSites) != len(c.rc.Code) {
		l.callSites = make([]uint8, len(c.rc.Code))
	}
	switch l.callSites[rpc] {
	case jitCallNative:
		return false, false
	case jitCallDone:
		return false, true
	}
	if n := l.callSites[rpc]; n+1 < jitCallsToInline {
		l.callSites[rpc]++
		if cl != nil && !slices.ContainsFunc(l.settling, func(x jitInline) bool { return int(x.pc) == rpc && x.obj == o }) {
			l.settling = append(l.settling, jitInline{pc: int32(rpc), cl: cl, obj: o, forward: true})
		}
		return false, true
	}
	l.callSites[rpc] = jitCallDone
	var met []jitInline
	for i := 0; i < len(l.settling); i++ {
		if x := l.settling[i]; int(x.pc) == rpc {
			if x.obj != o {
				met = append(met, x)
			}
			l.settling = slices.Delete(l.settling, i, i+1)
			i--
		}
	}
	if cl != nil {
		l.nativeCalls = append(l.nativeCalls, jitInline{pc: int32(rpc), cl: cl, obj: o})
		l.callSites[rpc] = jitCallNative
	}
	for _, x := range met {
		if l.callSites[rpc] != jitCallNative || jitCountAt(l.nativeCalls, rpc) >= jitCallTargets {
			break
		}
		l.nativeCalls = append(l.nativeCalls, jitInline{pc: int32(rpc), cl: x.cl, obj: x.obj})
	}
	if l.callSites[rpc] != jitCallNative {
		return false, true
	}
	r.jitRegReopt(f.cl, e)
	if jitCountAt(l.nativeCalls, rpc) == 1 && jitRegForwards(f.cl) {
		// Decided on one function, which a call of the forwarder may be
		// inlined with now: its callers, compiled to call it, are
		// compiled again to inline it.
		r.jitRegCallersReopt(l, jitInlineDepth)
	}
	return true, true
}

// regJoinCallers has the function whose key is key, its register code
// calling through links, join the native callers of each link's function
// (jitEntry's regCallers), as the stack form's setLinks has its code join
// theirs: not its own, nor one there already, nor past jitNativeCallers.
func (s *jitState) regJoinCallers(key jitKey, links []*jitLink) {
	for _, l := range links {
		if l.key == key {
			continue
		}
		ce := s.regLearned[l.key]
		if ce == nil || len(ce.regCallers) >= jitNativeCallers || slices.Contains(ce.regCallers, key) {
			continue
		}
		ce.regCallers = append(ce.regCallers, key)
	}
}

// jitRegCallersReopt has the register code of l's function's native
// callers (jitEntry's regCallers), depth levels of them, compiled again
// once it has spent a budget (jitRegReopt), to inline the function now
// that a call of its is decided -- a forwarder's forward settled on one
// target (jitRegForwardSeen), which jitRegForwardTarget inlines it with
// -- as the stack form's jitCallersReopt has its native callers compiled
// again, each no more than jitInlineReoptimizations times for it: a
// caller compiled before its callee's calls were decided called it
// natively, not inlined, for good.
func (r *Runtime) jitRegCallersReopt(l *jitEntry, depth int) {
	if depth == 0 || l == nil {
		return
	}
	s := r.jit
	for _, key := range l.regCallers {
		ce := s.regCache[key]
		if ce == nil || ce.code == nil || ce.invalid || ce.life == nil {
			continue
		}
		if ce.life.upgradeReopts < jitInlineReoptimizations {
			ce.life.upgradeReopts++
			ce.life.reopt = true
		}
		r.jitRegCallersReopt(ce.life, depth-1)
	}
}

// jitRegKnownCallee answers a call Go made of cl's function for a native
// caller that calls it natively -- one it was compiled to call, or calls
// through its link: one whose code native callers no longer call
// (notNative) counts toward their calling it again (jitRegRetryNative), as
// the stack form's jitKnownTargetSeen and jitCallTarget count a native
// caller's call of such a callee, which left for Go; one with no code is
// compiled for them (jitRegNativeCalleeN), unless it is long and its call
// seldom made (jitRegCalledOften). Counted only where Go entered
// the callee (jitRegEnter), a callee judged slow as well, which Go does
// not enter, was never called natively again: CD's Vector, Havlak's
// Dictionary.at, their native callers' calls made by Go, a million of
// them, each run by the interpreter.
func (r *Runtime) jitRegKnownCallee(cl, caller *closure) {
	if cc := cl.closureCode; cc != nil {
		if e := cc.jitReg; e != nil && e.life != nil && e.life.notNative && !e.invalid && e.code != nil && e.cached {
			r.jitRegRetryNative(e)
			return
		}
	}
	r.jitRegNativeCallee(cl, cl.reg, caller)
}

// jitRegViaSeen learns, after Go made the method call at register pc rpc
// of f's code -- a call of Go, or one an exit left to it -- of callee on
// recv with argc arguments, a call through the realm's
// Function.prototype.call, G.call(x, ...) -- TypeScript's
// _super.call(this, ...) -- of G, recv, a function native callers may
// call: the next compile's to call natively (regCallSite's via), as the
// stack form's code learns it (jitCalledVia). It reports whether it
// learned one.
func (r *Runtime) jitRegViaSeen(f *frame, e *jitRegEntry, rpc int, callee, recv Value, argc int) bool {
	if e == nil || e.invalid || r.callFn == nil || argc < 1 || !callee.IsObject() || callee.Object() != r.callFn || !recv.IsObject() {
		return false
	}
	o := recv.Object()
	fd := o.fn()
	if !r.jitNativeTargetOf(fd, f.cl, false) {
		return false
	}
	cl := fd.closure
	if cl.reg == nil || cl.reg.args || !r.jitRegCallable(o, cl, cl.reg, argc-1) {
		return false
	}
	l := r.jitRegLearnedFor(f, e)
	n := 0
	for _, x := range l.nativeCalls {
		if int(x.pc) == rpc {
			if x.obj == o && x.via {
				r.jitRegNativeCallee(cl, cl.reg, f.cl)
				return false
			}
			n++
		}
	}
	if n >= jitCallTargets {
		return false
	}
	l.nativeCalls = append(l.nativeCalls, jitInline{pc: int32(rpc), cl: cl, obj: o, via: true})
	r.jitRegNativeCallee(cl, cl.reg, f.cl)
	r.jitRegReopt(f.cl, e)
	return true
}

// jitRegConstructable reports whether native callers may construct with o
// natively, of argc arguments, its own code running (regCallSite's pool):
// a base constructor -- a plain function's object, or a class's of no
// field to initialize, as the stack form's constructions are made
// (jitConstructs) -- or a derived class's own constructor, its instance
// from the pool, its super() native or Go's, which gives the class's
// fields (jitConstructsDerived) -- of register code native callers may
// call, that does not read new.target -- one that hands its arguments on,
// this.initialize.apply(this, arguments), among them, whose forwarding
// call its code makes natively (ssa.CallSite's Forward) -- its prototype
// its own property, as regConstruct runs a construction in a register
// frame. A class's had been constructed by Go at every construction,
// AWFY's CD, Json and Havlak hundreds of thousands of times a run, and a
// derived class's at every construction until now: AWFY's Json and
// DeltaBlue.
func (r *Runtime) jitRegConstructable(o *Object, argc int) bool {
	fd := o.fn()
	if fd == nil || fd.closure == nil || fd.bound || fd.extra != nil || proxyOf(o) != nil {
		return false
	}
	cl := fd.closure
	switch fd.ctorKind {
	case ctorBase:
		if fd.fieldInit() != nil || cl.fn.Kind != bytecode.KindNormal && cl.fn.Kind != bytecode.KindConstructor {
			return false
		}
	case ctorDerived:
		// A class without a constructor of its own whose parent native
		// code constructs is constructed with that parent's code
		// (jitRegDerivedBase).
		if cl.fn.Kind != bytecode.KindDerivedConstructor || fd.superCtor() != o || o.class != ClassFunction ||
			jitRegForwardsSuper(cl.reg) && r.jitRegDerivedBase(o, argc) != nil {
			return false
		}
	default:
		return false
	}
	if cl.reg == nil || jitRegReadsNewTarget(cl, cl.reg) || o.findOwn(atomPrototype) < 0 {
		return false
	}
	return r.jitRegTarget(o, cl, cl.reg, argc)
}

// jitRegReadsNewTarget reports whether cl's register code c reads
// new.target, which a frame Go makes of a native callee's level has not --
// but a class's constructor's, only ever constructed, whose construction,
// or super(), writes its new.target to its context (abi.Context's
// NewTarget), where the frames Go makes of its level, and the stand-in
// its new_target is made on, find it (jitRegPushLevel, jitRegFrameRun).
func jitRegReadsNewTarget(cl *closure, c *regCode) bool {
	return c.newTarget && !isClassConstructorKind(cl.fn.Kind)
}

// jitRegConstructTarget is the closure native code constructs o with, of
// argc arguments: o's own (jitRegConstructable), or, where o's
// constructor is the one a class without one has, the closure of the
// class it extends, parent (jitRegDerivedBase); ok false for a
// construction Go makes.
func (r *Runtime) jitRegConstructTarget(o *Object, argc int) (cl *closure, parent *Object, ok bool) {
	if r.jitRegConstructable(o, argc) {
		return o.fn().closure, nil, true
	}
	if p := r.jitRegDerivedBase(o, argc); p != nil {
		return p.fn().closure, p, true
	}
	return nil, nil, false
}

// jitRegDerivedBase is the class a class o extends, if o's constructor is
// the one a class without one has, constructor(...args) { super(...args) }
// (jitRegForwardsSuper), o has no fields to give its instances, and native
// code constructs with that class (jitRegConstructable); or nil. new o(...)
// then does what a construction with that class does, its object made as
// o makes one -- o's prototype, o's root shape and room, as
// constructWithTarget makes it for super() to hand on -- and its result
// o's, as the stack form's (jitDerivedBase) and V8, which skips a default
// derived constructor (FindNonDefaultConstructorOrConstruct). AWFY's Havlak
// constructs a DictIdEntry, a DictEntry's subclass, for each entry of its
// dictionaries, and DeltaBlue an EditConstraint, a UnaryConstraint's.
func (r *Runtime) jitRegDerivedBase(o *Object, argc int) *Object {
	fd := o.fn()
	if fd == nil || fd.closure == nil || fd.native != nil || fd.bound || fd.ctorKind != ctorDerived || fd.fieldInit() != nil ||
		fd.closure.realm != r.Realm || o.class != ClassFunction || o.findOwn(atomPrototype) < 0 || !jitRegForwardsSuper(fd.closure.reg) {
		return nil
	}
	if p := o.proto(); p != nil && r.jitRegConstructable(p, argc) {
		return p
	}
	return nil
}

// jitRegForwardsSuper reports whether c is the register code of the
// constructor a derived class without one has: super(...args), then the
// this it bound returned.
func jitRegForwardsSuper(c *regCode) bool {
	if c == nil || len(c.rc.Code) == 0 || c.rc.Code[0].Op() != bytecode.RSuperForward {
		return false
	}
	for _, w := range c.rc.Code[1:] {
		switch w.Op() {
		case bytecode.RLoadUndef, bytecode.RMove, bytecode.RDerivedRet, bytecode.RRet, bytecode.RNop:
		default:
			return false
		}
	}
	return true
}

// jitRegCallable reports whether native callers may call o, cl's function
// object, whose register code is rc, with argc arguments (regCallSite): a
// function a call does not throw for, which a class's constructor is.
func (r *Runtime) jitRegCallable(o *Object, cl *closure, rc *regCode, argc int) bool {
	return !isClassConstructorKind(cl.fn.Kind) && r.jitRegTarget(o, cl, rc, argc)
}

// jitRegTarget reports whether native callers may call or construct with
// o, cl's function object, whose register code is rc, with argc arguments
// (jitRegCallable, jitRegConstructable).
func (r *Runtime) jitRegTarget(o *Object, cl *closure, rc *regCode, argc int) bool {
	fd := o.fn()
	if fd == nil || fd.closure != cl || fd.bound || cl.realm != r.Realm || rc == nil || rc != cl.reg || !jitRegCalled(cl, rc) ||
		cl.fn.HasDirectEval {
		return false
	}
	if rc.args && argc > abi.MaxGoArgs {
		// One that keeps its arguments is given them in its context
		// (ssa.CallSite's FrameArgs), as many as it holds, as the stack
		// form's (closureCallSite): a sloppy one's arguments object that
		// aliases its parameters among them, which its code makes only
		// where nothing sees the aliasing (compile's argumentsOwn).
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
func (f *jitRegFeedback) regCallSite(pc int, o *Object, cl *closure, rc *regCode, argc int, method bool, pool *abi.ObjectPool, parent *Object, byCode, via bool) (ssa.CallSite, bool) {
	fb := f.fb
	r := fb.r
	if via {
		// G.call(x, ...) through the realm's Function.prototype.call, a
		// method call's (ssa.CallSite's Via): G called natively with x its
		// receiver and the rest its arguments, as the stack form's
		// (jitCalledVia) -- not one that keeps its arguments, nor an arrow
		// reading its this, nor a construction.
		if !method || argc < 1 || pool != nil || byCode || r.callFn == nil || rc.args || o.fn().arrow && cl.fn.UsesThis ||
			!r.jitRegCallable(o, cl, rc, argc-1) {
			return ssa.CallSite{}, false
		}
		l := r.jitState().regLinkOf(cl, rc)
		k := fb.keep()
		k.links = append(k.links, l)
		k.holders = append(k.holders, o, r.callFn)
		site := ssa.CallSite{
			Callee: uintptr(unsafe.Pointer(o)), Closure: uintptr(unsafe.Pointer(cl)),
			Entry: uintptr(unsafe.Pointer(&l.entry)), Count: uintptr(unsafe.Pointer(&l.in)),
			Argc: argc, Method: true, Params: 2 + int(rc.params), LocalCount: int(rc.numRegs), ThisSlot: -1,
			Window: f.p.Code[pc].Left.Slot + 1, Via: uintptr(unsafe.Pointer(r.callFn)),
			// x may be a primitive, which a sloppy G's this is coerced
			// from by Go.
			Coerce: rc.coerceThis,
		}
		if cl.fn.HasTailCall {
			site.Tail, site.TailFrame = true, true
		}
		return site, true
	}
	forward := false
	if w, _, ok := f.at(pc); ok && w.Op() == bytecode.RApplyArgs {
		// f.apply(x, arguments) (ssa.CallSite's Forward): f's receiver x,
		// as a method call's; its arguments the frame's, as many as its
		// context holds (mir's checks), none known here.
		forward, method, argc = true, true, 0
	}
	if pool == nil && !r.jitRegCallable(o, cl, rc, argc) {
		return ssa.CallSite{}, false
	}
	if pool != nil {
		if tc, tp, ok := r.jitRegConstructTarget(o, argc); !ok || tc != cl || tp != parent || rc != cl.reg || pool.Count == 0 {
			return ssa.CallSite{}, false
		}
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
		// A callee that keeps its arguments -- reads arguments, has a rest
		// parameter -- is given all of them and their count in its context
		// (abi.Context's Args), as the stack form's native call gives them
		// (jitFrameArgs), which the frames Go makes of its level have
		// (jitRegPushLevel) and its arguments' reads answer from
		// (jitRegFrameOp's GoArgument).
		FrameArgs: rc.args,
		// Its function in its window's register 0 only for one that reads
		// it, as the stack form's call writes it (jitFrameCallee); Go finds
		// it for the frames it makes of another's level
		// (jitRegLevelCallee).
		ReadsFunction: jitFrameCallee(cl.fn),
	}
	if byCode && pool == nil {
		// Checked by its code, its environment and its realm, not its
		// object: the closure is the callee's at run time (mir's byCode).
		site.ByCode, site.Env, site.Realm = uintptr(unsafe.Pointer(cl.fn)), uintptr(unsafe.Pointer(cl.env)), uintptr(unsafe.Pointer(cl.realm))
	}
	if forward {
		site.Argc, site.Forward, site.Apply = 2, true, uintptr(unsafe.Pointer(r.applyFn))
		k.holders = append(k.holders, r.applyFn)
	}
	if cl.fn.HasTailCall {
		// Whether its calls of itself in tail position may give its frame
		// up (ssa.OpTailSelf), which the call writes to its context at
		// every call, as the stack form's (abi.Context's TailFrame): a
		// call's frame, not a construction's, as the interpreter gives up
		// one (regTailTo) -- an arrow's sharing a derived constructor's
		// this is no callee of a native call (jitRegTarget).
		site.Tail, site.TailFrame = true, pool == nil
	}
	if pool != nil {
		// A construction: its receiver from the pool, made as regConstruct
		// makes one, while the function's prototype is still the object
		// the pool's are of (mir's constructGuards).
		k.pools = append(k.pools, pool)
		site.Pool, site.ProtoIndex, site.ProtoKey = uintptr(unsafe.Pointer(pool)), int(o.findOwn(atomPrototype)), uint32(atomPrototype)
		// A derived constructor's code: the pool's object its instance,
		// its this unbound until its super() binds it (ssa.CallSite's
		// Derived); a class's with no constructor of its own, run with its
		// parent's code, while o's prototype is still that parent
		// (Parent), as the stack form's.
		site.Derived = cl.fn.Kind == bytecode.KindDerivedConstructor
		if parent != nil {
			site.Parent = uintptr(unsafe.Pointer(parent))
			k.holders = append(k.holders, parent)
		}
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
	if forward {
		// Its receiver the third operand (mir's windowForward), no
		// method call's.
		site.Method = false
	}
	return site, true
}

// regSuperSites are the calls jitRegFeedback's NativeCalls answers for
// register instruction w, super_call's of B arguments, at the program's
// instruction pc, of a derived constructor's code: a native call of the
// parent its code learned the super() constructs with (jitRegSuperSeen) --
// called as the stack form's is (ssa.CallSite's Super), its window made
// past the frame's, the instance its receiver -- or Go's call
// (ssa.GoCall's Super), on the frame, which binds its this, of a class that
// may make an arrow before a super() (jitRegArrowBeforeSuper), whose
// binding the arrow shares, or of a parent it learned none of.
func (f *jitRegFeedback) regSuperSites(pc int, w bytecode.RInstr) []ssa.CallSite {
	fb := f.fb
	argc := int(w.B())
	if fb.fn.Kind != bytecode.KindDerivedConstructor || !f.p.This {
		return nil
	}
	if l, rpc := fb.e, int32(f.p.Origin[pc]); l != nil && !jitRegArrowBeforeSuper(f.rc, fb.fn) {
		for _, x := range l.nativeCalls {
			if x.pc != rpc || x.active == nil {
				continue
			}
			if site, ok := f.regSuperSite(pc, x, argc); ok {
				return []ssa.CallSite{site}
			}
			break
		}
	}
	if argc+1 > abi.MaxGoArgs {
		return nil
	}
	return []ssa.CallSite{{Go: ssa.GoCall, Super: true, Argc: argc, ThisSlot: -1}}
}

// regSuperSite is the native super() at the program's instruction pc of x,
// a parent the code learned it constructs with (jitRegSuperSeen), of argc
// arguments: the code of the class whose prototype it is still -- x.obj,
// or past one with no constructor of its own, x.parent -- called through
// its link, while the function running is x.active, the one the code was
// learned for (mir's superGuards); or false where that code is no longer
// one native callers construct with.
func (f *jitRegFeedback) regSuperSite(pc int, x jitInline, argc int) (ssa.CallSite, bool) {
	fb := f.fb
	r := fb.r
	p, skip, cl := r.jitRegSuperTarget(x.active, argc)
	if p != x.obj || skip != x.parent || cl != x.cl || cl == nil {
		return ssa.CallSite{}, false
	}
	rc := cl.reg
	l := r.jitState().regLinkOf(cl, rc)
	k := fb.keep()
	k.links = append(k.links, l)
	k.holders = append(k.holders, x.obj, x.active)
	site := ssa.CallSite{
		Callee: uintptr(unsafe.Pointer(x.obj)), Closure: uintptr(unsafe.Pointer(cl)),
		Entry: uintptr(unsafe.Pointer(&l.entry)), Count: uintptr(unsafe.Pointer(&l.in)),
		Argc: argc, Params: 2 + int(rc.params), LocalCount: int(rc.numRegs), ThisSlot: -1,
		Window: f.p.Code[pc].Left.Slot + 1,
		Super:  true, Active: uintptr(unsafe.Pointer(x.active)), Derived: cl.fn.Kind == bytecode.KindDerivedConstructor,
		// A parent that keeps its arguments is given them in its context,
		// as any native callee (FrameArgs).
		FrameArgs: rc.args,
	}
	if x.parent != nil {
		site.Parent = uintptr(unsafe.Pointer(x.parent))
		k.holders = append(k.holders, x.parent)
	}
	return site, true
}

// jitRegSuperTarget is the parent d's super() of argc arguments constructs
// with natively (ssa.CallSite's Super), and the closure whose code it runs,
// as the stack form's (jitSuperTarget): d a derived class of no fields --
// which super() would give the instance -- its prototype p a constructor
// native callers construct with -- a base one of no fields, or a derived
// one's own, neither reading new.target, which the frames Go makes of a
// native callee's level do not have -- or, p a class with no constructor
// of its own, the code of the one it extends, skip, as V8 skips a default
// derived constructor. nil where there is none.
func (r *Runtime) jitRegSuperTarget(d *Object, argc int) (p, skip *Object, cl *closure) {
	dfd := d.fn()
	if dfd == nil || dfd.closure == nil || dfd.ctorKind != ctorDerived || dfd.superCtor() != d || dfd.fieldInit() != nil {
		return nil, nil, nil
	}
	code := func(o *Object) *closure {
		fd := o.fn()
		if fd == nil || fd.native != nil || o.class != ClassFunction || !r.jitRegConstructable(o, argc) {
			return nil
		}
		return fd.closure
	}
	if p = d.proto(); p == nil {
		return nil, nil, nil
	}
	if cl = code(p); cl != nil {
		return p, nil, cl
	}
	if q := r.jitRegDerivedBase(p, argc); q != nil {
		return p, q, q.fn().closure
	}
	return nil, nil, nil
}

// jitRegArrowBeforeSuper reports whether fn, a derived constructor whose
// register code is rc, may make an arrow before a super() of its runs, as
// the stack form's (jitArrowBeforeSuper): an arrow shares the
// constructor's this binding, which a super() native code makes binds in
// the context (its This), not in a binding of the VM's an arrow made
// before shares. No arrow at a pc before a super()'s, and no jump back from
// one to a super(): every arrow fn makes is made once any super() it runs
// has run, Go making it then on a binding bound (jitBindThis), or, on a
// native callee's level, of the this its context holds (jitRegFrameOp).
func jitRegArrowBeforeSuper(rc *bytecode.RegCode, fn *bytecode.Function) bool {
	lastSuper, firstArrow := -1, len(rc.Code)
	consts := fn.Constants
	if rc.Constants != nil {
		consts = rc.Constants
	}
	for pc, w := range rc.Code {
		switch w.Op() {
		case bytecode.RSuperCall, bytecode.RSuperForward, bytecode.RSuperSpread:
			lastSuper = pc
		case bytecode.RClosureW:
			if k := int(w.BC()); k < len(consts) {
				if c := consts[k]; c.Kind == bytecode.ConstFunction && c.Fn != nil && c.Fn.Kind == bytecode.KindArrow {
					firstArrow = min(firstArrow, pc)
				}
			}
		}
	}
	if firstArrow == len(rc.Code) || lastSuper < 0 {
		return false
	}
	if firstArrow < lastSuper {
		return true
	}
	for pc, w := range rc.Code {
		for _, t := range jitcompile.RegSuccessors(rc, pc, w) {
			if pc >= firstArrow && t <= lastSuper {
				return true
			}
		}
	}
	return false
}

// jitRegSuperSeen notes the super() at register pc rpc of f's code, e's,
// that Go made -- at a host exit, or a call of Go native code made -- the
// parent it constructs with, which native code calls from the next
// compile on (regSuperSites), as the stack form's (jitSuperSeen).
func (r *Runtime) jitRegSuperSeen(f *frame, c *regCode, e *jitRegEntry, rpc int) {
	if e == nil || e.invalid || f.callee == nil || rpc >= len(c.rc.Code) || regAt(c.code, rpc).Op() != bytecode.RSuperCall {
		return
	}
	l := r.jit.regLearnedOf(r.jitRegKeyOf(f.cl))
	for _, x := range l.nativeCalls {
		if int(x.pc) == rpc && x.active != nil {
			// Known: a super() Go made for want of the parent's code,
			// which native callers call once it has some.
			r.jitRegNativeCallee(x.cl, x.cl.reg, f.cl)
			return
		}
	}
	p, skip, cl := r.jitRegSuperTarget(f.callee, int(regAt(c.code, rpc).B()))
	if p == nil || cl.reg == nil {
		return
	}
	r.jitRegNativeCallee(cl, cl.reg, f.cl)
	l.nativeCalls = append(l.nativeCalls, jitInline{pc: int32(rpc), cl: cl, obj: p, parent: skip, active: f.callee})
	r.jitRegReopt(f.cl, e)
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
	// construct marks a construction's callee (its caller's new_w, or a
	// super()'s parent, super_call's), this its receiver, which the
	// construction's result is if not an object -- a derived
	// constructor's, an object always (jitDerivedResult).
	construct bool
	this      Value
	// gen is, for the resumption of a generator a loop's step or a call of
	// next made natively, the generator, and cursor the step's, nil for a
	// call of next's (jitRegGenLevel).
	gen    *generator
	cursor *iterState
	// inline marks an inlined callee's level (jitRegInlined), which has
	// no context of its own: args are its call's arguments, for one that
	// keeps them.
	inline bool
	args   []Value
}

// jitRegConstructs marks l, the level whose caller's code c makes the call
// at register pc rpc, a construction's where that is new_w or super_call:
// its receiver the one the call gave its context (abi.Context's This).
// A plain call's receiver is written for the frame Go makes of the level
// (jitRegPlainReceiver).
func (r *Runtime) jitRegConstructs(l *jitRegLevel, c *regCode, rpc int) {
	if rpc >= len(c.rc.Code) {
		return
	}
	switch regAt(c.code, rpc).Op() {
	case bytecode.RNewW, bytecode.RSuperCall:
		l.construct, l.this = true, *(*Value)(unsafe.Pointer(&r.jit.ssaCtxs[l.ctx].This))
	case bytecode.RCallW, bytecode.RCallWT:
		if l.wb+1 < len(r.stack) {
			r.jitRegLevelCallee(l)
			r.jitRegPlainReceiver(r.stack[l.wb], l.cl, l.wb)
		}
	}
}

// jitRegLevelCallee is the function object of l's level, a native call's
// callee, which it writes to its window's register 0 where the call left
// it out (ssa.CallSite's ReadsFunction): the function its call's checks
// found (jit.CalledFunction), if its closure is the one running -- not
// where the callee's own call in tail position gave its frame to another,
// whose call wrote it -- as the stack form's frames Go makes of a level
// find theirs from the context or the call (jitLevelFrameFill). Otherwise,
// or for an inlined callee or a generator's resumption, the window's.
func (r *Runtime) jitRegLevelCallee(l *jitRegLevel) *Object {
	if !l.inline && l.gen == nil && l.ctx > 0 && l.ctx-1 < len(r.jit.ssaCtxs) {
		if p := jit.CalledFunction(r.jit.ssaCtxs[l.ctx-1]); p != nil {
			o := (*Object)(p)
			if fd := o.fn(); fd != nil && fd.closure == l.cl {
				r.stack[l.wb] = Obj(o)
				return o
			}
		}
	}
	if v := r.stack[l.wb]; v.IsObject() {
		return v.Object()
	}
	return nil
}

// jitRegPlainReceiver writes to register 1 of the window at wb, a frame of
// closure cl that callee, its function object, is called by a plain call
// in, the receiver the interpreter's call gives it (regCallKind):
// undefined, the global object for a sloppy callee, an arrow's captured
// this. A native call, or a callee inlined, writes it only for a callee
// that reads it, and the frame Go makes of the level reads it for a
// stack trace: left as the window held it, the trace named the frame a
// method of whatever was there, "String.boom".
func (r *Runtime) jitRegPlainReceiver(callee Value, cl *closure, wb int) {
	if !callee.IsObject() || cl == nil || cl.reg == nil {
		return
	}
	fd := callee.Object().fn()
	if fd == nil || fd.closure != cl {
		return
	}
	this := Undefined
	if fd.arrow {
		x := fd.extra
		if x == nil || x.lexThisRef != nil {
			return
		}
		this = x.lexThis
	} else if cl.reg.coerceThis {
		this = r.globalThis
	}
	r.stack[wb+1] = this
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
	// Its closure's code, while the cache holds it for this realm
	// (jitRegEntry's cached): a key made and a map searched at each level
	// of each call of Go from deep native recursion had cost EarleyBoyer
	// most of its time once its recursion ran natively, and been a tenth
	// of AWFY's Json.
	var e *jitRegEntry
	if cc := cl.closureCode; cc != nil {
		e = cc.jitReg
	}
	if e == nil || !e.cached || e.key.realm != r.jitRealmKey() {
		e = s.regCache[r.jitRegKeyOf(cl)]
	}
	if (e == nil || e.code == nil || e.p == nil) && len(s.regRetired) != 0 {
		// Let go of by the cache while the call ran it -- compiled again,
		// once Go found it invalid at a call of Go from the level -- and
		// kept for it (regClose): the one its callee returns into, if the
		// level made a call.
		key := r.jitRegKeyOf(cl)
		var ret uintptr
		if i+1 < len(s.ssaCtxs) && s.ssaCtxs[i+1].Live == abi.LiveCall {
			ret = s.ssaCtxs[i+1].ReturnTo
		}
		e = nil
		for _, x := range s.regRetired {
			if x.key == key && x.code != nil && x.p != nil && (e == nil || ret != 0 && x.code.Contains(ret)) {
				e = x
			}
		}
	}
	if e == nil || e.code == nil || e.p == nil {
		return jitRegLevel{}, false
	}
	e.levels++
	s.regLevelsFound++
	l := jitRegLevel{ctx: i, cl: cl, c: cl.reg, e: e, wb: int(c.Base), back: c.ReturnTo, base: c.Base}
	if cl.fn.Generator {
		r.jitRegGenLevel(&l, c)
	}
	return l, true
}

// jitRegPushLevel makes the VM's frame of a level, as regEnterW makes a
// call's: its function, its window, its pc, its new.target undefined.
func (r *Runtime) jitRegPushLevel(l *jitRegLevel, pc uint32) *frame {
	if l.gen != nil {
		// A generator's resumption: the frame of the realm's next that
		// resumed it below its own, as regGenStep makes them.
		r.jitPushGenNext(l.gen)
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
	f := &r.cur[off]
	f.cl = l.cl
	f.callee = r.jitRegLevelCallee(l)
	f.base = l.wb + int(l.c.locals)
	f.pc = pc
	if len(f.openUpvalues) != 0 {
		f.openUpvalues = f.openUpvalues[:0]
	}
	if f.thisRef != nil || f.withScopes != nil || f.evalVars != nil || f.native != "" || f.args != nil {
		regClearFrame(f)
	}
	f.newTarget = Undefined
	if l.construct {
		// Its new.target, which the construction wrote to its context --
		// a super()'s, the one it was given (mir's windowSuper) -- and a
		// derived constructor's frame as runFD makes it: its function,
		// its instance and its this binding, bound if its super() bound
		// it (jitDerivedFrame).
		if l.inline {
			// Inlined at its construction, of the function in its window.
			if f.callee != nil {
				f.newTarget = Obj(f.callee)
			}
		} else {
			c := r.jit.ssaCtxs[l.ctx]
			if c.NewTarget != 0 {
				f.newTarget = Obj(*(**Object)(unsafe.Pointer(&c.NewTarget)))
			} else if f.callee != nil {
				f.newTarget = Obj(f.callee)
			}
			if l.cl.fn.Kind == bytecode.KindDerivedConstructor {
				jitDerivedFrame(f, c)
			}
		}
	}
	if l.c.args {
		// The arguments its native call gave its context (ssa.CallSite's
		// FrameArgs), as regEnterW keeps a call's.
		if l.inline {
			f.args = l.args
		} else {
			f.args = jitCtxArgs(r.jit.ssaCtxs[l.ctx])
		}
		p := int(l.c.params)
		f.locals = r.stack[l.wb+2 : l.wb+2+p : l.wb+2+p]
	}
	f.regKind = regFrameEnter
	if l.gen != nil {
		// Its this, function and arguments the generator's, as regGenRun
		// makes a resumption's frame.
		r.jitRegGenFill(f, l)
	}
	return f
}

// jitCtxArgs is a copy of the arguments context c holds (abi.Context's
// Argc and Args), which a native call of a function that keeps its
// arguments wrote there.
func jitCtxArgs(c *abi.Context) []Value {
	n := min(int(c.Argc), len(c.Args))
	args := make([]Value, n)
	for i := range n {
		args[i] = *(*Value)(unsafe.Pointer(&c.Args[i]))
	}
	return args
}

// jitRegPopLevel ends a level's frame f, as regEnterW ends a call's: what
// closures captured of it closed, the VM's stack's top back at its window
// -- a generator's resumption's, the frame of the next below it too.
func (r *Runtime) jitRegPopLevel(f *frame, wb int, gen bool) {
	f.regKind = 0
	if len(f.openUpvalues) != 0 {
		regCloseUpvalues(f)
	}
	r.frameDepth--
	if gen {
		r.frameDepth--
	}
	r.stackTop = wb
}

// jitRegRootOf is the context of the run of register code whose native
// calls run in the contexts up to idx, or -1 where there is none: the run
// whose native code runs now (jitState's regRunAt), as calls of Go come
// only from it, its native callees' levels between -- asked at each call
// of Go from a native callee's code, it walks none of them.
func (r *Runtime) jitRegRootOf(idx int) int {
	s := r.jit
	root := s.regRunAt - 1
	if root < 0 || root >= idx || idx >= len(s.ssaCtxs) || s.ssaCtxs[root].Live != 0 || s.ssaCtxs[root+1].Live != abi.LiveCall || root >= len(s.regRuns) || s.regRuns[root].f == nil {
		return -1
	}
	return root
}

// jitRegGoCallNested is jitRegGoCall for a native callee's code, in
// context idx, of the run in context root, as the stack form's
// jitGoCallNested makes one: the calling level is given the VM's frame
// while the call runs -- its function, its window, past the call it makes
// -- and the call is made on it, its code pinned meanwhile; the levels
// between the run's and it have no frames of the VM's until one of them
// is read (jitLazyLevels): a stack trace, a function's caller. Room is left
// for them below the calling level's, so that the depth of calls is what
// it would be, and their code is held by the contexts they run in, which
// the cache asks before it compiles any again or closes it
// (jitRegLevelsOf). Making and pinning every level's frame at each call
// had cost a call of Go from deep in a recursion of native calls work for
// each level: EarleyBoyer's, two dozen levels deep, about a fifth of its
// time. Levels a generator's resumption may be among, which have two
// frames each, are given theirs now (jitRegGoCallLevels). A call that
// threw, or after which the level's code is invalid, Go finishes at the
// exit native code then takes (regPending), once the callee levels have
// left native code (jitRegUnwind).
func (r *Runtime) jitRegGoCallNested(ctx *abi.Context, idx, root int) bool {
	s := r.jit
	if idx >= len(s.ssaCtxs) || s.ssaCtxs[idx] != ctx {
		return false
	}
	s.regNestedCalls++
	if s.regGens && r.jitRegGensIn(root, idx) {
		return r.jitRegGoCallLevels(ctx, idx, root)
	}
	run := s.regRuns[root]
	rpc, _ := jit.CallPC(s.ssaCtxs[root])
	// Its caller's call, which says whether it is a construction's.
	caller, at := run.c, int(rpc)
	if idx-1 > root {
		cc := s.ssaCtxs[idx-1]
		cl := (*closure)(cc.Closure)
		if cl == nil || cl.reg == nil {
			return false
		}
		p, _ := jit.CallPC(cc)
		caller, at = cl.reg, int(p)
	}
	l, found := r.jitRegLevelAt(idx)
	if !found {
		return false
	}
	run.f.pc = regSrc(run.c, int(rpc)) + 1
	r.jitRegConstructs(&l, caller, at)
	depthBefore := r.frameDepth
	lazy := idx - root - 1
	if lazy != 0 {
		r.jitFrameBlocks(depthBefore + lazy)
		r.frameDepth = depthBefore + lazy
	}
	f := r.jitRegPushLevel(&l, 0)
	if lazy != 0 {
		s.lazy = append(s.lazy, jitLazyLevels{root: root, idx: idx, depth: depthBefore, runF: run.f, reg: true, realm: r.Realm})
		s.lazyTop = depthBefore + lazy
		s.lazyCalls++
		s.regLazyLevelsPut += uint64(lazy)
	}
	ctxTop := s.ctxTop
	s.ctxTop = max(ctxTop, idx+1)
	top := r.stackTop
	// A native callee's call of Go, judged as the stack form's
	// (jitCalleeProfit), the next one's work counted from it.
	cpc := int(ctx.ExitPC)
	r.jitRegCalleeProfit(l.e, cpc)
	ok := r.jitRegGoCallOn(ctx, idx, jitRegRun{f: f, c: l.c, e: l.e, wb: l.wb, own: -1, root: root})
	s.regGoMark = jitRegGoMark{e: l.e, stretch: s.entries, edges: r.backEdges - s.pollCredit, rpc: cpc}
	s.ctxTop = ctxTop
	r.jitRegPopLevel(f, l.wb, false)
	if lazy != 0 {
		r.jitRegLazyDone()
	}
	r.frameDepth = depthBefore
	r.stackTop = top
	// Gone on in natively once the call is done, or unwound by Go at the
	// exit it leaves at.
	r.jitRegPinLevel(root, l.e)
	l.e.levels--
	s.regClose(l.e)
	return ok
}

// jitRegGensIn reports whether a level of the native calls in the contexts
// from root+1 to idx is a generator's resumption (jitRegGenLevel), which
// has a frame of its next's below its own.
func (r *Runtime) jitRegGensIn(root, idx int) bool {
	s := r.jit
	for i := root + 1; i <= idx; i++ {
		c := s.ssaCtxs[i]
		if g := jitGenLevel(c, (*closure)(c.Closure), abi.ExitHost); g != nil && g.reg {
			return true
		}
	}
	return false
}

// jitRegGoCallLevels is jitRegGoCallNested where a generator's resumption
// is among the levels: each level from the run's on is given the VM's
// frame while the call runs, a generator's its next's below it, as the
// interpreter's frames are during a call, and every level's code is pinned
// meanwhile.
func (r *Runtime) jitRegGoCallLevels(ctx *abi.Context, idx, root int) bool {
	s := r.jit
	// Its levels and their frames on the state's stacks of them, past an
	// enclosing call's: a call of Go deep in a recursion of native calls,
	// past eight levels, had allocated both.
	lbase, fbase := len(s.regNestLevels), len(s.regNestFrames)
	ok := true
	for i := root + 1; i <= idx; i++ {
		l, found := r.jitRegLevelAt(i)
		if !found {
			ok = false
			break
		}
		s.regNestLevels = append(s.regNestLevels, l)
	}
	levels := s.regNestLevels[lbase:]
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
		for k := range levels {
			l := &levels[k]
			pc := uint32(0)
			if k < len(levels)-1 {
				rpc, _ := jit.CallPC(s.ssaCtxs[l.ctx])
				pc = regSrc(l.c, int(rpc)) + 1
			}
			s.regNestFrames = append(s.regNestFrames, r.jitRegPushLevel(l, pc))
		}
		frames := s.regNestFrames[fbase:]
		ctxTop := s.ctxTop
		s.ctxTop = max(ctxTop, idx+1)
		last := &levels[len(levels)-1]
		top := r.stackTop
		// A native callee's call of Go, judged as the stack form's
		// (jitCalleeProfit), the next one's work counted from it.
		cpc := int(ctx.ExitPC)
		r.jitRegCalleeProfit(last.e, cpc)
		ok = r.jitRegGoCallOn(ctx, idx, jitRegRun{f: frames[len(frames)-1], c: last.c, e: last.e, wb: last.wb, own: -1, root: root})
		s.regGoMark = jitRegGoMark{e: last.e, stretch: s.entries, edges: r.backEdges - s.pollCredit, rpc: cpc}
		s.ctxTop = ctxTop
		for k := len(frames) - 1; k >= 0; k-- {
			r.jitRegPopLevel(frames[k], levels[k].wb, levels[k].gen != nil)
		}
		r.stackTop = top
	}
	for k := range levels {
		// Gone on in natively once the call is done, or unwound by Go at
		// the exit it leaves at.
		r.jitRegPinLevel(root, levels[k].e)
		levels[k].e.levels--
		s.regClose(levels[k].e)
	}
	clear(s.regNestLevels[lbase:])
	clear(s.regNestFrames[fbase:])
	s.regNestLevels, s.regNestFrames = s.regNestLevels[:lbase], s.regNestFrames[:fbase]
	return ok
}

// jitRegMakeLazy makes the frames of the levels lz, a call of Go from a
// native callee's code, put off (jitLazyLevels), at the depth it left
// room at, as jitRegGoCallLevels would have made them: each level's code
// found in the realm the call was made in and pinned, until the call is
// done (jitRegLazyDone). A level whose code Go does not find -- which the
// cache keeps while the contexts hold it (jitRegLevelsOf) -- is the room
// of a frame of no function's, which a stack trace passes over.
func (r *Runtime) jitRegMakeLazy(lz *jitLazyLevels) {
	s := r.jit
	ctxs := s.ssaCtxs
	lz.from = len(s.regLazyLevels)
	s.regLazyMade++
	realm := r.Realm
	r.Realm = lz.realm
	run := s.regRuns[lz.root]
	p, _ := jit.CallPC(ctxs[lz.root])
	caller, at := run.c, int(p)
	for i := lz.root + 1; i < lz.idx; i++ {
		l, found := r.jitRegLevelAt(i)
		p, _ := jit.CallPC(ctxs[i])
		if !found {
			f := r.pushFrame()
			*f = frame{openUpvalues: f.openUpvalues[:0], handlers: f.handlers[:0]}
			caller = nil
			continue
		}
		if caller != nil {
			r.jitRegConstructs(&l, caller, at)
		}
		f := r.jitRegPushLevel(&l, regSrc(l.c, int(p))+1)
		s.regLazyLevels = append(s.regLazyLevels, l)
		s.regLazyFrames = append(s.regLazyFrames, f)
		caller, at = l.c, int(p)
	}
	r.Realm = realm
}

// jitRegLazyDone ends the innermost call of Go's put-off levels
// (jitLazyDone), as the call returns: the frames made of them, if any
// were, ended, and their code let go.
func (r *Runtime) jitRegLazyDone() {
	s := r.jit
	from := len(s.regLazyLevels)
	lz := &s.lazy[len(s.lazy)-1]
	if lz.made {
		from = lz.from
	}
	root := lz.root
	r.jitLazyDone()
	for k := from; k < len(s.regLazyLevels); k++ {
		f := s.regLazyFrames[k]
		f.regKind = 0
		if len(f.openUpvalues) != 0 {
			regCloseUpvalues(f)
		}
		e := s.regLazyLevels[k].e
		// Gone on in natively once the call is done.
		r.jitRegPinLevel(root, e)
		e.levels--
		s.regClose(e)
	}
	clear(s.regLazyLevels[from:])
	clear(s.regLazyFrames[from:])
	s.regLazyLevels, s.regLazyFrames = s.regLazyLevels[:from], s.regLazyFrames[:from]
	if len(s.regRetired) != 0 {
		// Code the levels put off ran, let go of meanwhile (regClose).
		s.jitRegCloseRetired()
	}
}

// regLevelsOf is how many native calls under way run e's code that Go
// runs beside: those whose levels Go found it for (jitRegEntry's levels),
// and those below calls of Go that put their levels off (jitLazyLevels),
// whose contexts say what they run -- where a level's callee returns to --
// counted only when asked: the cache asks before it compiles e's function
// again or lets go of e's code.
func (s *jitState) regLevelsOf(e *jitRegEntry) int {
	n := e.levels
	if len(s.lazy) == 0 || e.code == nil {
		return n
	}
	for j := range s.lazy {
		if lz := &s.lazy[j]; lz.reg && !lz.made {
			n += s.regLevelsIn(e, lz.root, lz.idx)
		}
	}
	return n
}

// regCalled reports whether a native call of e's function has not
// returned, as the stack form's called does: a context a native call's
// callee runs in (abi.LiveCall) is its closure's -- native code marks the
// context, its closure stored, before the call, and clears it as the
// callee returns -- a callee that called Go, or called a function that
// did, those whose levels a call of Go put off (jitLazyLevels) among them.
// None is while no run's native code is (ctxTop). Go finds the code of
// such a call by its function's (jitRegLevelAt), which is the code it runs
// only while the function is not compiled again: the cache compiles it
// again once none is (jitRegLearnedDueWith), and keeps its code meanwhile
// (regInUse). A level Go unwinds is no such call -- its context is cleared
// as Go takes it (jitRegUnwind), and it goes on in the code it holds,
// which is kept until it is done -- nor is a run Go entered, which goes on
// in its own code. Counted as levels instead, every level of a recursion
// Go was finishing kept its function from being compiled again: Octane
// Typescript's AstWalker.walk left its code at the visitors it had
// learned of some 285,000 times a run, never compiled for them.
func (s *jitState) regCalled(e *jitRegEntry) bool {
	return s.regCalledBelow(e, len(s.ssaCtxs))
}

// regCalledBelow is regCalled for the native calls in the contexts below
// limit: those a call of Go whose leaving unwinds the contexts from limit
// on does not unwind (jitRegCompileDueNow).
func (s *jitState) regCalledBelow(e *jitRegEntry, limit int) bool {
	if s.ctxTop == 0 {
		return false
	}
	fn := e.key.Value()
	if fn == nil {
		return false
	}
	limit = min(limit, len(s.ssaCtxs))
	for i := 0; i < limit; i++ {
		c := s.ssaCtxs[i]
		if c.Live == abi.LiveCall && c.Closure != nil && (*closure)(c.Closure).fn == fn {
			return true
		}
	}
	return false
}

// regLevelsIn counts the levels of the native calls in the contexts from
// root+1 to idx-1 that run e's code: those whose callee returns to it.
func (s *jitState) regLevelsIn(e *jitRegEntry, root, idx int) int {
	n := 0
	for i := root + 1; i < idx; i++ {
		if e.code.Contains(s.ssaCtxs[i+1].ReturnTo) {
			n++
		}
	}
	return n
}

// jitRegUnwound counts a native call of e's code that left native code
// (jitEntry's nativeOut), as the stack form's jitUnwound does: code that
// leaves on more than one in jitUnwindShare of its native callers' calls
// is compiled again, if it is learning, up to jitUnwindReopts times, its
// native callers calling it still; past that, or learning nothing, native
// callers no longer call it (notNative) -- they call Go at the call, which
// makes it -- until Go has made jitRetryNative calls of it, more each time
// (jitRegRetryNative). Before, a callee that left at every call was
// unwound at every call, its callers' levels finished by Go each time:
// AWFY Havlak's size, at and hashFn, a million times each.
func (r *Runtime) jitRegUnwound(e *jitRegEntry) {
	life := e.life
	if life == nil || e.invalid {
		return
	}
	l := r.jit.regLinks[e.key]
	if l == nil {
		return
	}
	if life.nativeOut++; life.nativeOut%jitUnwindProbe != 0 || life.nativeOut*jitUnwindShare <= l.in {
		return
	}
	if life.unwindReopts < jitUnwindReopts && jitLearning(life) {
		life.unwindReopts++
		l.in, life.nativeOut = 0, 0
		life.ssaStats.calleeGoCalls, life.ssaStats.calleeWork = 0, 0
		return
	}
	life.notNative, life.nativeRetry = true, 0
	l.clearCode()
	life.nativeBackoff = min(life.nativeBackoff+1, jitNativeBackoffs)
}

// jitRegRetryNative counts a call Go makes of e's code, which native
// callers no longer call (notNative), and has them call it again after
// jitNativeRetry such calls, doubled for each time it was found to leave
// too often, its leaving counted afresh, as jitRetryNative does.
func (r *Runtime) jitRegRetryNative(e *jitRegEntry) {
	life := e.life
	if life.nativeRetry++; life.nativeRetry < jitNativeRetry<<(life.nativeBackoff-1) {
		return
	}
	life.notNative, life.nativeOut = false, 0
	life.ssaStats.calleeGoCalls, life.ssaStats.calleeWork = 0, 0
	if l := r.jit.regLinks[e.key]; l != nil {
		l.in = 0
		if r.jit.regCache[e.key] == e && e.code != nil && !e.invalid {
			l.regSet(r.jit, e)
		}
	}
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
// Each level's code is pinned until the level is done. gen says the
// outermost was a generator's resumption, whose answer is its step's
// (jitRegGenDone).
func (r *Runtime) jitRegUnwind(idx int) (_ Value, _ error, gen bool) {
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
	if n != 0 {
		// The innermost left native code: counted against its native
		// callers' calling it (jitRegUnwound).
		r.jitRegUnwound(levels[n-1].e)
		// A call from the last context, which the contexts, not the
		// frames the VM allows, limited: more of them, as the stack form's
		// unwinding has them made (jitUnwindNative).
		if l := &levels[n-1]; l.ctx == len(s.ssaCtxs)-1 && s.bounds.LevelLimit == uint64(len(s.ssaCtxs)) && s.ssaCtxs[l.ctx].ExitKind == abi.ExitHost {
			r.jitGrowContexts()
		}
	}
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
			if iv, ierr, ok := r.jitRegInlined(l.ctx, f, l.e, l.pc, l.wb); ok {
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
			// The callee's answer, its step's where a loop's step called
			// its iterator's next (jitRegStepCalled), not a generator's,
			// whose answer is its step's already.
			s.regPending = jitGoPending{set: true, pc: l.pc, ctx: l.ctx, v: v, err: err, step: levels[k+1].gen == nil}
			v, err = r.jitRegFinishLevel(f, l, c)
		}
		if err == errRegRestart {
			// Its frame given to a callee in place by a call in tail
			// position (regRestartRun).
			v, err = r.regRestartRun(f)
		}
		if l.gen != nil {
			// A generator's resumption: its suspension or its end saved,
			// and the step's result (jitRegGenDone).
			v, err = r.jitRegGenDone(f, l, v, err)
		}
		r.jitRegPopLevel(f, l.wb, l.gen != nil)
		l.e.levels--
		s.regClose(l.e)
		if l.e.life != nil && l.e.life.reopt && l.cl.reg == l.c && r.jitRegLearnedDue(l.cl, l.e) {
			// Due to be compiled again for what it learned -- a call of Go
			// left for it (jitRegCompileDueNow) -- now that no native call
			// of it under way finds its code by its function's (regCalled):
			// compiled here, as the stack form's code is once its run has
			// left (jitRecompileInRun). The levels of it Go is finishing,
			// a recursion's, go on in the code they hold.
			r.jitRegReoptimize(l.cl, l.c, l.e)
		}
		if err == errTailCall {
			// Ended in a call in tail position, its frame given up: the
			// call made here.
			v, err = r.regTailRest()
		}
		switch {
		case l.construct && l.cl.fn.Kind == bytecode.KindDerivedConstructor:
			// A derived construction's: an object, or what it throws
			// (jitDerivedResult).
			v, err = r.jitDerivedResult(v, err)
		case l.construct && err == nil && !v.IsObject():
			// A construction's result is its receiver, unless an object.
			v = l.this
		}
	}
	s.ctxTop = ctxTop
	return v, err, n != 0 && levels[0].gen != nil
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

// InlineExit is ssa.InlineExitFeedback's: of an inlined callee's
// feedback, whether native code may leave the inlined frame at the
// program's instruction pc, as the stack form's (jitInlineExit): a throw,
// which leaves its frame anyway, and the call, or the construction, whose
// result it throws -- throw new Error(...) -- as seldom run as the throw,
// as V8's code inlines a function whose cold paths call the runtime to
// throw. One taken after all counts toward the call's inlining's exits
// (jitInlineLeft), which stop it, the callee then called natively; a
// callee with such a path had been called natively, never inlined.
func (f *jitRegFeedback) InlineExit(pc int) bool {
	w, _, ok := f.at(pc)
	if !ok || f.fb.root == nil {
		return false
	}
	rpc := int(f.p.Origin[pc])
	l := f.fb.e
	made := l != nil && rpc < len(l.regCallsMade) && l.regCallsMade[rpc] != 0
	switch jitRegCallOp(w.Op()) {
	case bytecode.RThrow, bytecode.RThrowRef:
		return true
	case bytecode.RCallW, bytecode.RCallMW, bytecode.RNewW:
		next := rpc + 1
		if next < len(f.rc.Code) && f.rc.Code[next].Op() == bytecode.RThrow && f.rc.Code[next].A() == w.B() {
			// What it makes is thrown: as seldom as the throw, unless its
			// own code has been seen to make it, a throw taken often.
			return !made
		}
		// A call its own code leaves to Go (HostsLeft) and has never been
		// seen to make in runs enough to tell (jitRegCodeRan), as V8's
		// code deoptimizes at a call that has no feedback.
		return l != nil && !made && slices.Contains(l.ssaHostsLeft, int32(rpc)) && f.fb.r.jitRegCodeRan(f.fb.cl, l)
	}
	return false
}

// jitRegCodeRan reports whether cl's function's register code, whose
// learning is l, has run often enough -- entered by Go or called natively,
// jitRunsSeen times -- for a call it has not made to be one seldom made,
// as the stack form's jitCodeRan.
func (r *Runtime) jitRegCodeRan(cl *closure, l *jitEntry) bool {
	calls := l.starts + l.goOns
	if k := r.jit.regLinks[r.jitRegKeyOf(cl)]; k != nil {
		calls += k.in
	}
	return calls >= jitRunsSeen
}

// KeepLimit is ssa.KeepFeedback's: the build's keep cells' limit, 0 for
// the default (ssa.VirtualKeeps), as the stack form's: abi.MaxKeeps for a
// build again of code whose keep cells live at once were more than the
// context has (mir.ErrKeeps, jitRegBuild).
func (f *jitRegFeedback) KeepLimit() int { return f.fb.keepLimit }

// jitRegBuild builds cl's register code c lowered to p, from its feedback
// asked now, with its keep cells limited to keepLimit (KeepLimit), its
// entry kept at frameAt (ssa.Func's FrameAt), into w's workspaces: its
// feedback, its SSA, its loops' headers and the backend's code, or why it
// refused.
func (r *Runtime) jitRegBuild(w *jit.Workspace, cl *closure, c *regCode, p *ir.Program, keepLimit, frameAt int) (*jitRegFeedback, *ssa.Func, map[uint32]bool, *mir.Code, error) {
	rf := r.jitRegFeedbackFor(cl, p)
	rf.fb.keepLimit = keepLimit
	if t := mir.Timing; t != nil {
		t.Start()
	}
	f, err := ssa.BuildIn(&w.SSA, p, rf)
	if err != nil {
		return rf, nil, nil, nil, err
	}
	f.FrameAt = frameAt
	looped := jitRegShape(f, p, cl, c)
	jitRegLap(mir.PhaseBuild)
	ssa.Optimize(f)
	jitRegLap(mir.PhaseOptimize)
	mc, err := mir.CompileWith(&w.MIR, f, jitEncoding, mir.Settings())
	return rf, f, looped, mc, err
}

// jitRegWindowOf is where the window of f, running c, is in the VM's stack.
func jitRegWindowOf(f *frame, c *regCode) int { return f.base - int(c.locals) }
