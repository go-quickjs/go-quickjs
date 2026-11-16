//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64))

package vm

import (
	"slices"
	"unsafe"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
	"github.com/go-quickjs/go-quickjs/internal/jit/abi"
)

// Register callees inlined (jitRegFeedback's Inline, ssa's InlineSite's
// Window): an exit inside one -- or inside one inlined in it -- writes the
// caller's state at the call, a host exit there, and each inlined callee's
// window past the caller's slots, saying in the contexts after the
// caller's where (abi.LiveInline): its closure, its window's first slot
// in the VM's stack (Base), the register pc it left at and how. Go makes
// each callee's frame there, as regEnterW would have -- the outermost
// first -- finishes the innermost from its exit in the interpreter, as a
// frame's code goes on from one (jitRegLeft), each other from its call
// with what its callee answered (regFinishCall), and hands the outermost's
// answer to the caller's call (regPending), as a native callee's.

// jitRegInlineLevel is an inlined callee's frame an exit wrote: its
// closure, its window at wb, the register pc it left at and how.
type jitRegInlineLevel struct {
	cl   *closure
	wb   int
	pc   int
	kind uint64
	site int64
	// construct marks a construction's callee (its caller's new_w), this
	// its receiver, its window's register 1, which every state inside it
	// holds: the construction's result where the callee returns no object.
	construct bool
	this      Value
	// callee is the function its caller's call called, where it is one,
	// for a plain call's receiver: the object the code inlined it for
	// (abi.Context's InlineCallee), else the call's window's. known is
	// the former, nil where the code has none: a closure it makes.
	callee Value
	known  *Object
	// threw marks the innermost level whose operation Go made at its host
	// exit threw (jitRegInlineLeft).
	threw bool
}

// jitRegInlined finishes the inlined callees whose frames an exit of the
// code of owner, the entry of the function frame f runs, run in context
// idx, wrote in the contexts past it (abi.LiveInline), and returns what the
// outermost answered, or false where there are none. at is the register pc
// of the owner's call the outermost is inlined at, wb the owner's window:
// the objects and closures its code did not make, which its frame's
// registers and the callees' windows may name, are made first, once for
// both (jitRegMaterializeSpans). A callee that is a closure the owner did
// not make (ssa's calledClosures), named by its template, is the closure
// made in its place, which its caller's call's window holds.
func (r *Runtime) jitRegInlined(idx int, f *frame, owner *jitRegEntry, at, wb int) (Value, error, bool) {
	cl := f.cl
	s := r.jit
	if idx+1 >= len(s.ssaCtxs) || s.ssaCtxs[idx+1].Live != abi.LiveInline {
		return Undefined, nil, false
	}
	var buf [4]jitRegInlineLevel
	levels := buf[:0]
	for i := idx + 1; i < len(s.ssaCtxs) && s.ssaCtxs[i].Live == abi.LiveInline; i++ {
		c := s.ssaCtxs[i]
		var in *closure
		if k := owner.keep; k != nil {
			for _, x := range k.inlined {
				if uintptr(unsafe.Pointer(x)) == uintptr(c.InlineClosure) {
					in = x
				}
			}
		}
		if in != nil && in.reg == nil {
			// A template of a closure the owner makes (madeInline), which
			// no call has run yet where the owner was compiled before the
			// closure's first call, as at QJS_JIT_STRESS's threshold: its
			// register code, as a call of it gets it.
			r.regOf(in)
		}
		if in == nil || in.reg == nil {
			panic("vm: an inlined register callee's closure is not its caller's")
		}
		lv := jitRegInlineLevel{cl: in, wb: int(c.Base), pc: int(c.ExitPC), kind: c.ExitKind, site: int64(c.ExitSite)}
		if c.InlineCallee != 0 {
			// The object, which the owner's code keeps alive.
			lv.known = *(**Object)(unsafe.Pointer(&c.InlineCallee))
		}
		levels = append(levels, lv)
		c.Live, c.ReturnTo = 0, 0
	}
	n := len(levels)
	if idx+n == len(s.ssaCtxs)-1 && s.bounds.LevelLimit == uint64(len(s.ssaCtxs)) && levels[n-1].kind == abi.ExitHost {
		// The innermost level's state in the last context, which the
		// contexts, not the frames the VM allows, limited: an inlined
		// frame's room check (ssa.OpFrameRoom) left for want of contexts
		// past it, as the stack form's unwinding finds it (jitUnwindNative)
		// and has more made. Left so at every call, the code had been
		// compiled again with the call not inlined, the closures it gives
		// the callee made.
		r.jitGrowContexts()
	}
	if owner.frame != 0 || owner.keep != nil && len(owner.keep.made) != 0 {
		spans := make([][2]int, 0, n)
		for _, x := range levels {
			spans = append(spans, [2]int{x.wb, x.wb + int(x.cl.reg.numRegs)})
		}
		r.jitRegMaterializeSpans(f, owner, wb, spans)
	}
	for k := range levels {
		// The closure its caller's call called, the function in the call's
		// window: the one made in place of a template.
		caller, cwb, cpc := cl.reg, wb, at
		if k > 0 {
			caller, cwb, cpc = levels[k-1].cl.reg, levels[k-1].wb, levels[k-1].pc
		}
		if cpc >= len(caller.rc.Code) {
			continue
		}
		called := Undefined
		switch w := regAt(caller.code, cpc); jitRegCallOp(w.Op()) {
		case bytecode.RCallW, bytecode.RCallMW, bytecode.RNewW:
			called = r.stack[cwb+int(w.A())]
		case bytecode.RApplyArgs:
			// f of f.apply(x, arguments), inlined with the arguments
			// (ssa.InlineSite's Forward).
			called = r.stack[cwb+int(w.B())]
		}
		if loop := s.ssaCtxs[idx].Loop; k == 0 && loop != 0 {
			// A loop's callback inlined in it: the loop's callback slot
			// (jitRegLoopFinish's), which Go goes on calling.
			called = r.stack[jitRegSlotIndex(owner.p, wb, owner.p.Locals+int(loop&0xffffffff)-1)+1]
		}
		if o := levels[k].known; o != nil {
			// The function object the code inlined the callee for, which
			// its call's checks found, as the stack form's frame of the
			// level has it (jitUnwindNative's InlineCallee): the window's
			// register, which inlined code writes only where the callee
			// reads it, held whatever was there -- a stack trace named
			// the frame by the function a call made after the window's
			// last use had there.
			called = Obj(o)
			r.stack[levels[k].wb] = called
		}
		if called.IsObject() {
			if fd := called.Object().fn(); fd != nil && fd.closure != nil && fd.closure.fn == levels[k].cl.fn && fd.closure.reg != nil {
				levels[k].cl = fd.closure
			}
		}
		levels[k].callee = called
		if kp := owner.keep; kp != nil && slices.Contains(kp.made, levels[k].cl) {
			panic("vm: an inlined closure the code did not make is not where its call is")
		}
	}
	s.unwound += uint64(n)
	saved := r.stackTop
	var fbuf [4]*frame
	frames := fbuf[:0]
	// A loop's callback inlined in it (abi.Context's Loop): the loop's
	// slots, its array, callback and thisArg (jitRegLoopFinish's), and
	// whether the built-in's frame was made below the callback's.
	loop, loopSlots, loopFrame := s.ssaCtxs[idx].Loop, 0, false
	if loop != 0 {
		loopSlots = jitRegSlotIndex(owner.p, wb, owner.p.Locals+int(loop&0xffffffff)-1)
	}
	for k := range levels {
		x := &levels[k]
		rc := x.cl.reg
		if top := x.wb + int(rc.numRegs); top > r.stackTop {
			r.stackTop = top
			r.stackHigh = max(r.stackHigh, top)
		}
		pc := regSrc(rc, x.pc)
		if k < n-1 {
			// Past the call it makes, as the interpreter's frame is during
			// one, which a stack trace shows.
			pc++
		}
		// A construction's, where its caller's call is new_w: the owner's
		// at the call the outermost is inlined at, or the level's below.
		caller, at := cl.reg, at
		if k > 0 {
			caller, at = levels[k-1].cl.reg, levels[k-1].pc
		}
		if k == 0 && loop != 0 {
			// The loop's callback: its receiver the loop's thisArg, as
			// Go's forEach calls it (jitRegForwardReceiver), an arrow's
			// the one it captured (jitRegPlainReceiver); and the
			// built-in's frame below it, as the stack form's unwinding
			// makes it (jitPushLoopFrame), which a stack trace shows: "at
			// Array.forEach". Neither had been made, the callback named
			// "Number.throwAt" by what its window's register 1 held, and
			// the built-in left out.
			if x.callee.IsObject() && x.callee.Object().fn() != nil && x.callee.Object().fn().arrow {
				r.jitRegPlainReceiver(x.callee, x.cl, x.wb)
			} else {
				r.jitRegForwardReceiver(x.cl, x.wb, r.stack[x.wb+1])
			}
			r.jitRegPushLoopFrame(loop, r.stack[loopSlots])
			loopFrame = true
		} else if at < len(caller.rc.Code) {
			switch regAt(caller.code, at).Op() {
			case bytecode.RNewW:
				x.construct, x.this = true, r.stack[x.wb+1]
			case bytecode.RCallW, bytecode.RCallWT:
				// A plain call's receiver, for the frame (jitRegPlainReceiver).
				r.jitRegPlainReceiver(x.callee, x.cl, x.wb)
			case bytecode.RApplyArgs:
				// f.apply(x, arguments)'s x, which the state gave the
				// level's register 1, coerced for the frame as the
				// interpreter's apply coerces it (jitRegForwardReceiver).
				r.jitRegForwardReceiver(x.cl, x.wb, r.stack[x.wb+1])
			}
		}
		lv := jitRegLevel{cl: x.cl, c: rc, wb: x.wb, construct: x.construct, inline: true}
		if rc.args {
			// Its call's arguments, which it keeps (ssa.InlineSite's
			// ReadsArguments and Arguments): its caller's call's window's,
			// or those its caller hands on.
			cwb, cargs := wb, f.args
			if k > 0 {
				cwb, cargs = levels[k-1].wb, frames[k-1].args
			}
			lv.args = r.jitRegInlineArgs(caller, cwb, at, cargs)
		}
		frames = append(frames, r.jitRegPushLevel(&lv, pc))
	}
	var v Value
	var err error
	for k := n - 1; k >= 0; k-- {
		x, f := &levels[k], frames[k]
		if k == n-1 {
			// Told before Go runs the instruction, which may fill the pool
			// it finds empty.
			atSite := r.jitRegLeftAtSite(x)
			miss := r.jitRegShapeMiss(x)
			var learned bool
			v, err, learned = r.jitRegInlineLeft(f, x, owner, miss)
			l := s.regLearnedOf(r.jitRegKeyOf(cl))
			switch {
			case x.kind == abi.ExitDeopt || miss:
				// A guard's failure: the owner's code invalid from here,
				// as at one of its own (jitRegLeft), the stack form's
				// code's at one inside a callee it inlined. So too a
				// property site's shape the code was not compiled for,
				// which the site's learning took (jitRegInlineLeft), as
				// the stack form's (jitFinishExit).
				r.jitRegInvalidate(cl, owner)
			case learned:
				// The callee's feedback learned what the owner's code was
				// not compiled for: compiled again for it (jitRegReopt).
				r.jitRegReopt(cl, owner)
			case x.threw && !atSite && !owner.life.reopt && slices.ContainsFunc(l.nativeCalls, func(y jitInline) bool { return int(y.pc) == at && y.forEach && !y.cbLeft }):
				// A loop's callback that threw, inlined in the loop: counted
				// against its inlining there (jitInlineLeft's cbLeft), as the
				// stack form's unwinding of the loop's callback counts it --
				// left inlined, a callback that throws at every call left
				// native code at each.
				r.jitInlineLeft(l, at)
				if l.inlineReopt {
					l.inlineReopt = false
					r.jitRegReopt(cl, owner)
				}
			case atSite || x.threw || owner.life.reopt:
				// Left at a site of the callee's -- or by code to be
				// compiled again, which may be what it leaves for -- not
				// for being inlined, as the stack form's unwinding counts
				// none such; or left to throw, which the callee's own code,
				// called natively, leaves native code for just the same: a
				// read of null's property, at a site the callee's own code
				// made megamorphic deoptimizing for it (jitDeoptSeenOp),
				// had had constructs' Vs called natively after 16 throws.
			default:
				// Left with nothing to learn: counted against inlining the
				// callee at the call, by the stack form's own rule
				// (jitInlineLeft, jitInlineExits): past it the call is a
				// native call in the code compiled next. A closure the
				// owner makes, inlined where it calls it and not made
				// (madeInline), is counted at that call as Inline's
				// callees are: past the rule it is made and called, its
				// call's shape no longer given (CallShape's notInline) --
				// one whose reads of its arguments past the call's count
				// are Go's had left native code at every call.
				if k := owner.keep; k != nil && !slices.ContainsFunc(l.inlines, func(y jitInline) bool { return int(y.pc) == at }) &&
					slices.ContainsFunc(k.made, func(t *closure) bool { return t.fn == levels[0].cl.fn }) {
					l.inlines = append(l.inlines, jitInline{pc: int32(at), cl: levels[0].cl})
				}
				r.jitInlineLeft(l, at)
				if l.inlineReopt {
					l.inlineReopt = false
					r.jitRegReopt(cl, owner)
				}
			}
		} else {
			var next int
			next, _, err = r.regFinishCall(f, x.cl.reg, x.wb, x.pc, v, err)
			if err == nil {
				v, err = r.runReg(f, x.cl.reg, next)
			}
		}
		if err == errRegRestart {
			// Its frame given to a callee in place by a call in tail
			// position (regRestartRun).
			v, err = r.regRestartRun(f)
		}
		r.jitRegPopLevel(f, x.wb, false)
		if k == 0 && loopFrame {
			// Its callee finished: the built-in's frame goes.
			r.frameDepth--
		}
		if err == errTailCall {
			v, err = r.regTailRest()
		}
		if x.construct && err == nil && !v.IsObject() {
			// A construction's result is its receiver, unless an object.
			v = x.this
		}
	}
	r.stackTop = saved
	return v, err, true
}

// jitRegLeftAtSite reports whether the innermost inlined level x left,
// at a host exit, for a site of its own rather than for being inlined, as
// the stack form's unwinding tells (jitShapeMiss, jitPoolEmptied): a
// property's read or write whose site is not megamorphic -- a shape its
// cases lack, a receiver whose read throws -- or a construction whose
// pool ran out, which Go fills again. Neither counts against inlining the
// callee (jitInlineLeft): a callee reading a property of the null its
// caller passes now and then, a TypeError the caller catches, had been
// called natively after 16 of them.
func (r *Runtime) jitRegLeftAtSite(x *jitRegInlineLevel) bool {
	rc := x.cl.reg.rc
	if x.kind != abi.ExitHost || x.pc < 0 || x.pc >= len(rc.Code) {
		return false
	}
	w := rc.Code[x.pc]
	l := r.jit.regLearnedOf(r.jitRegKeyOf(x.cl))
	switch w.Op() {
	case bytecode.RGetProp, bytecode.RGetMethod, bytecode.RGetMethodW, bytecode.RSetProp, bytecode.RAddProp, bytecode.RAddPropI:
		return r.jitRegShapeMiss(x)
	case bytecode.RNewW:
		for _, t := range jitTargets(l) {
			if int(t.pc) == x.pc && t.pool != nil && t.pool.Count == 0 {
				return true
			}
		}
	}
	return false
}

// jitRegShapeMiss reports whether the innermost inlined level x left at a
// host exit at a property's read or write whose site is not megamorphic,
// as the stack form's jitShapeMiss asks: a shape its cases lack, a site
// that knew none, a receiver whose read throws. It is a map check's
// failure, as a guard at a site of the code's own is (jitRegDeoptSeen),
// which the stack form's unwinding learns as one, the code that inlined
// the callee invalidated, compiled again with the site's cases once the
// interpreter has spent a budget (jitFinishExit). Taken for the callee's
// site alone, not the owner's code's, AWFY's Dictionary.at, compiled while
// DeltaBlue ran with hashFn inlined for its keys' customHash, left at its
// read for Havlak's keys at every call, never compiled again; its native
// callers stopped calling it (jitRegUnwound), and the interpreter ran it,
// two million calls a run.
func (r *Runtime) jitRegShapeMiss(x *jitRegInlineLevel) bool {
	rc := x.cl.reg.rc
	if x.kind != abi.ExitHost || x.pc < 0 || x.pc >= len(rc.Code) {
		return false
	}
	w := rc.Code[x.pc]
	switch w.Op() {
	case bytecode.RGetProp, bytecode.RGetMethod, bytecode.RGetMethodW, bytecode.RSetProp, bytecode.RAddProp, bytecode.RAddPropI:
	default:
		return false
	}
	if site := int(w.C()); site < len(x.cl.ic) && x.cl.ic[site].fills >= maxCacheFills {
		return false
	}
	l := r.jit.regLearnedOf(r.jitRegKeyOf(x.cl))
	for i := range l.poly {
		if p := &l.poly[i]; int(p.pc) == x.pc && p.mega {
			return false
		}
	}
	return true
}

// jitRegInlineLeft finishes f, the innermost inlined callee's frame, from
// where it left (x), in the interpreter, learning from it as from an exit
// of its own code (jitRegLeft), what it learns its function's own: a
// guard's failure; a property site's shape the code was not compiled for
// (miss, jitRegShapeMiss), learned as a guard's; the cases of a property
// site Go ran a host operation at; a loop's poll. learned says it learned
// what the code inlining it, owner, is to be compiled again for.
func (r *Runtime) jitRegInlineLeft(f *frame, x *jitRegInlineLevel, owner *jitRegEntry, miss bool) (v Value, err error, learned bool) {
	s := r.jit
	c := x.cl.reg
	pc := x.pc
	switch x.kind {
	case abi.ExitDeopt:
		s.guards++
		if x.site < 0 {
			r.jitRegEntrySeen(f, pc)
		} else {
			r.jitRegDeoptSeen(f, c, owner, x.wb, pc)
		}
		learned = true
	case abi.ExitHost:
		// Counted once, where the run left (jitRegLeft), as the stack
		// form counts an inlined callee's exit (runSSALoop): counted here
		// too, each such exit had been two.
		f.pc = regSrc(c, pc) + 1
		s.noteRegHost(c, pc)
		if miss {
			// Learned as a guard's failure at the site, before Go makes the
			// read or the write, which fills its cache.
			s.shapeHosts++
			r.jitRegDeoptSeen(f, c, owner, x.wb, pc)
		}
		l := s.regLearnedOf(r.jitRegKeyOf(x.cl))
		before, failed := l.polyReopt, len(l.failed)
		// An addition that met a string, learned as at the callee's own
		// host exit (jitRegStringAddSeen), as the stack form's unwinding
		// of an inlined level learns it (jitStringAddSeen): Go's from the
		// next lowering on. Not learned, a closure's s += x inlined had
		// left native code at each call until the call was no longer
		// inlined.
		str := r.jitRegStringAddSeen(l, c, x.wb, pc)
		seen := r.jitRegHostSeen(f, c, x.wb, pc)
		next, ran, err := r.regWhole(f, c, x.wb, pc)
		if !ran {
			break
		}
		if seen.write && err == nil {
			r.jitRegWriteSeen(f, c, owner, pc, seen)
		}
		// The site met what the code inlining it was not compiled for --
		// its own code's to be compiled again for too (polyReopt) -- or
		// a value not a number at a read the code takes for a number's
		// (jitNumberMissed), which a guard of the callee's own code
		// deoptimizes for but one inlined leaves to Go at, the read
		// generic once compiled again: str's in callsGo, of a string, had
		// left at each. A change of its caches alone is not: the stack
		// form's code is compiled again for none (its mark only delays a
		// compile, jitLearnedMark), and counted so, each exit had the
		// code inlining it compiled again for what the interpreter's run
		// of the callee filled.
		learned = !before && l.polyReopt || len(l.failed) != failed || seen.elem || str
		if err != nil {
			x.threw = true
			return Undefined, err, learned
		}
		if r.stopped != nil {
			return Undefined, r.stopped, learned
		}
		pc = next
	case abi.ExitPoll:
		s.budgets++
		r.backEdges = s.backEdgeBudget()
		if err := r.checkInterruptPoll(); err != nil {
			return Undefined, err, false
		}
	}
	s.interpreted++
	if s.reporting {
		s.regEndWhy = "inlined callee left"
		s.noteRegEnded(c, pc)
	}
	v, err = r.runReg(f, c, pc)
	return v, err, learned
}
