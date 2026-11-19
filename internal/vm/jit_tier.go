//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64))

package vm

import (
	"fmt"
	"math"
	"slices"
	"unsafe"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
	"github.com/go-quickjs/go-quickjs/internal/jit/ssa"
)

// Tier-up from the register interpreter (docs/register-interpreter.md,
// "Tiering to the JIT"): a runtime that runs the JIT compiles a register
// function's code once the interpreter has spent a budget on it, and enters
// it at its calls and at its loops' headers.
//
// The budget is jitCallThreshold invocations, each the function's length
// (jitRegBudget), each counted: the feedback dispatcher's returns count a
// function's runs down from what is left of its budget, and send its next
// call to Go once none is (jitRegCallTier, jitRegArm); the feedback
// dispatcher's backward jumps count it down by their distance
// (jitRegEdges), as V8's JumpLoop does, and end the back-edge budget once
// none is left, so that the jump's check (regBackEdge, which every
// dispatcher's jump leaves to its Go slow function) spends the work they
// counted (jitRegLoop). A function's first check begins its budget, and a
// change of its caches' feedback meanwhile delays the compile
// (jitRegWarmUp), as V8's NotifyICChanged does. Code a guard of which
// failed is invalidated and compiled again on a fresh budget, up to
// jitMaxDeopts times (jitRegInvalidate); code that learned at its exits
// runs on, valid, and is compiled again once it has spent a budget itself
// with nothing more learned (jitRegLearnedDue).
//
// Its code is entered:
//   - by every call of it the interpreter makes, its call handlers', Go's
//     and the native one's, which calls Go for it with no exit
//     (nativeJITCall): a function's code is its closures' template's
//     (jitEnters), which every call reads;
//   - by on-stack replacement at the loop whose back edge took the sample
//     (jitRegLoop), in the dispatcher that runs the frame, which goes on
//     where the code leaves it: at the register pc of a deoptimization or
//     of an instruction with no entry after it, or, where the code
//     returned, at a return of the code's own with what it returned;
//   - at the next back edge of a frame whose valid code left it where it
//     has no entry (jitRegGoOnAt), the interpreter's check made due there
//     at once: the interpreter runs the rest of that iteration, not the
//     rest of the invocation.

// jitRegTakes reports whether cl's register code c is compiled: a plain
// function's, a method's, an arrow's, an accessor's or a class's
// constructor's code of the form's own layout, a generator's and an async
// function's among them, and top-level code's -- a script's, a module's,
// an indirect eval's. Its frame is the call path's, an arrow's this, a
// constructor's object or a derived one's uninitialized this in its
// register 1 as the interpreter has them; a suspended frame's as
// regGenRun makes it again at each resumption. A leaf's -- a body the
// interpreter's calls answer without a frame -- is compiled for the calls
// that make one, native callers' among them (regCallSite). Only what
// jitRegCalled says is entered at its start; the rest at its loops, by
// on-stack replacement, and a suspended frame's code where it resumes.
func jitRegTakes(cl *closure, c *regCode) bool {
	switch cl.fn.Kind {
	case bytecode.KindNormal, bytecode.KindMethod, bytecode.KindArrow, bytecode.KindGetter, bytecode.KindSetter,
		bytecode.KindConstructor, bytecode.KindDerivedConstructor:
	default:
		return false
	}
	return c.wide && c.rc == cl.fn.RegCode()
}

// jitRegCalled reports whether cl's register code c, which jitRegTakes,
// is entered at its start by a call: that of neither a generator nor an
// async function, whose call makes its object and whose frame the
// interpreter suspends and makes again (regGenRun), nor top-level code,
// which runs once, as V8 enters top-level code only by on-stack
// replacement. Native callers call only such code, and inline only it.
func jitRegCalled(cl *closure, c *regCode) bool {
	return jitRegTakes(cl, c) && !c.suspends && !cl.fn.TopLevel && !cl.fn.IsModule
}

// setJITReg makes e the template's code, which calls enter (jitEnters),
// nil for none: then the next run's return counts the budget again
// (jitRegArm), from where the code was let go of.
func (cc *closureCode) setJITReg(e *jitRegEntry) {
	cc.jitReg, cc.jitEnters = e, e != nil
	if e == nil {
		cc.jitRegTick, cc.jitRegTickSet = 0, 0
		cc.jitRegEdges, cc.jitRegEdgesSet = 0, 0
	}
}

// regEdgeTick counts a backward jump by off of st's frame, in the Go
// table's loop, as the feedback dispatcher's jumps count one (nativegen's
// feedback.go): its distance of its function's budget's work left for back
// edges (closureCode's jitRegEdges), and, once none is, the interrupt
// budget ended, so that the jump's check spends the work counted
// (regBackEdge, jitRegLoop). Native handlers' jumps leave to the Go table's
// handlers having counted.
func regEdgeTick(st *regState, off int32) {
	if !regNativeOn {
		cc := st.cl.closureCode
		if cc.jitRegEdges += int64(off); cc.jitRegEdges <= 0 {
			st.r.backEdges = 1
		}
	}
}

// regEdgesOff has the back edges of cl's function's frames count nothing
// more of its budget, in a runtime that runs no JIT, whose Go table's
// jumps count it all the same (regEdgeTick): its first back edge's check
// (regBackEdge) has them stop. A check where the JIT runs counts again
// (jitRegArmEdges).
func regEdgesOff(cl *closure) {
	if cc := cl.closureCode; cc != nil {
		cc.jitRegEdges, cc.jitRegEdgesSet = math.MaxInt64, math.MaxInt64
	}
}

// jitRegEdgeWork is the work of back edges cl's function's frames did in
// the interpreter since its count began (jitRegArmEdges), each its
// distance, as V8's JumpLoop spends it: 0 where it is not counting.
func jitRegEdgeWork(cc *closureCode) int64 {
	if set := cc.jitRegEdgesSet; set > 0 && set < math.MaxInt64 {
		return set - cc.jitRegEdges
	}
	return 0
}

// jitRegArmEdges has the back edges of cl's function's frames count its
// budget's work left (jitRegEdges), its register code c, after a check
// spent what they had counted: what is left of the budget its first
// compile, or its compile after an invalidation, waits for; or a budget,
// for a function compiled for host operations alone once a budget more is
// spent. For code that runs they count with no end (jitRegCounted): what
// the interpreter did in its loops counts toward its code's work
// (jitRegSpent's osrWork) at each check the interrupt budget makes, and
// those checks alone look at the frame -- for its loop's entry, or a
// retry of a loop judged slow (jitRegRetrySlow) -- as before its compile.
// A function never compiled counts nothing more.
func (r *Runtime) jitRegArmEdges(cl *closure, c *regCode) {
	cc := cl.closureCode
	left := int64(math.MaxInt64)
	if r.jitEnabled && !cc.jitRegRefused && jitRegTakes(cl, c) {
		life := r.jitRegLifeOf(cl)
		switch {
		case cc.jitReg != nil && !cc.jitReg.invalid:
			left = jitRegCounted
		case life.warming || life.invalid:
			left = max(life.tierBudget, 1)
		default:
			left = jitRegBudget(r.jitCallThreshold, c)
		}
	}
	cc.jitRegEdges, cc.jitRegEdgesSet = left, left
}

// jitBudgetForFeedback is V8's interrupt_budget_for_feedback_allocation:
// the work a function's loops do before its budget begins (jitRegLoop).
const jitBudgetForFeedback = 940

// jitRegCounted is the work a function's back edges count from where its
// count has no end (jitRegArmEdges): more than any program's back edges.
const jitRegCounted = 1 << 62

// regTickRet counts a run of cc's function that returned, in the Go
// table's loop, as the feedback dispatcher's ret and ret_undef count one
// (nativegen's feedback.go): the budget's runs left, and, once none is,
// its next call sent to Go (jitRegCallTier).
func regTickRet(cc *closureCode) {
	if cc != nil {
		if cc.jitRegTick--; cc.jitRegTick <= 0 {
			cc.jitEnters = true
		}
	}
}

// jitRegHas reports whether cl's function has native code made from its
// register code: the template's, while the cache holds it (jitRegEntry's
// cached).
func (r *Runtime) jitRegHas(cl *closure) bool {
	return cl.closureCode != nil && cl.closureCode.jitReg != nil && cl.closureCode.jitReg.cached
}

// jitRegLetGoOf reports whether e, the code of cl's template (jitReg), is
// code the cache has let go of (regLetGo, dropReg), and, if it is, has the
// template's closures have none: their calls count the budget again from
// what they had counted (jitRegArm), and their native callers' next call of
// Go compiles it again for them (jitRegNativeCalleeN). The cache cannot
// clear the template itself: it knows the function by its key, not its
// templates. Kept, the template's code had said the function had code: its
// native callers' calls of Go never compiled it again, and every call --
// 4.2 million of AWFY's Havlak's hashFn, let go of during CD -- was Go's,
// each putting off the levels below it.
func (r *Runtime) jitRegLetGoOf(cl *closure, e *jitRegEntry) bool {
	if e == nil || e.cached {
		return false
	}
	if life := e.life; life != nil {
		// Its runs counted since its last budget, which the template's
		// counts no longer hold (jitRegInvoked).
		life.invoked = jitRegInvoked(cl, life)
	}
	cl.closureCode.setJITReg(nil)
	return true
}

// jitRegLifeOf is the state of cl's function's register code that outlives
// its compiles (jitRegEntry's life): what it learned (regLearned), its
// budget, its deoptimizations, what its runs did.
func (r *Runtime) jitRegLifeOf(cl *closure) *jitEntry {
	if e := cl.closureCode.jitReg; e != nil && e.life != nil {
		return e.life
	}
	return r.jitState().regLearnedOf(r.jitRegKeyOf(cl))
}

// jitRegLifeIfAny is jitRegLifeOf(cl) where cl's function has that state
// already, and nil where it has none -- nor the runtime the JIT's state --
// which it makes none of.
func (r *Runtime) jitRegLifeIfAny(cl *closure) *jitEntry {
	if e := cl.closureCode.jitReg; e != nil && e.life != nil {
		return e.life
	}
	if r.jit == nil {
		return nil
	}
	return r.jit.regLearned[r.jitRegKeyOf(cl)]
}

// jitRegNativeCallee compiles cl's function's register code c for the
// native callers that call it through its link, which has none yet: at
// their first call of it, not once it is hot -- they make no sample of it.
// Code invalidated is once its calls have spent a fresh budget
// (jitRegWarmUp), and so is a callee too long to inline that is warming up,
// unless caller, the closure of the native caller whose call Go made, calls
// it often (jitRegLongWarming, jitRegCalledOften).
func (r *Runtime) jitRegNativeCallee(cl *closure, c *regCode, caller *closure) {
	r.jitRegNativeCalleeN(cl, c, 1, caller)
}

// jitRegNativeCalleeN is jitRegNativeCallee after calls calls of the
// function Go made for its native callers, caller's among them.
func (r *Runtime) jitRegNativeCalleeN(cl *closure, c *regCode, calls int64, caller *closure) {
	cc := cl.closureCode
	if cc == nil || cc.jitRegRefused || !r.jitEnabled {
		return
	}
	if e := cc.jitReg; e != nil && !r.jitRegLetGoOf(cl, e) {
		return
	}
	if !jitRegCalled(cl, c) {
		return
	}
	life := r.jitRegLifeOf(cl)
	if life.invalid && c.leaf == regLeafNone && !r.jitRegWarmUp(cl, c, life, calls*int64(max(len(c.rc.Code), 1))) {
		// Invalidated: each call of it Go makes for its native callers an
		// invocation of the fresh budget it is compiled again after, and
		// the interpreter's samples the rest. Not a leaf's, which the
		// interpreter's calls answer without a frame, sampling none: it is
		// compiled again at its native callers' next ask, its
		// deoptimizations counted all the same (jitMaxDeopts).
		return
	}
	if !life.invalid && c.leaf == regLeafNone && r.jitRegLongWarming(cl, c, life) && !r.jitRegCalledOften(cl, c, life, caller) {
		// Too long to inline and warming up: compiled once its budget is
		// spent, as any function is -- the interpreter's runs of it Go
		// makes for its native callers count it down (jitRegArm) -- unless
		// the caller calls it often (jitRegCalledOften). Compiled at its
		// native callers' first call, Mandreel's set-up --
		// __mandreel_internal_init, BenchmarkDemo::initPhysics, the static
		// initializers, each run once an iteration -- was compiled at every
		// iteration after the first, 4.8 MB of code for initPhysics alone.
		// Not a leaf's, whose runs count nothing (above).
		return
	}
	e := r.jitRegForAs(cl, c, true)
	switch {
	case e == nil:
	case e.code == nil:
		cc.jitRegRefused = true
	default:
		cc.setJITReg(e)
	}
}

// jitRegInlineSize is the length of c's function in the instructions
// inlining's bounds count (ssa.MaxInline), whose unit is an instruction of
// the stack bytecode the front end once made: register code has 0.85 of
// its instructions, and
// each counts 20/17 of one, as ssa's inlineSize counts a register
// program's.
func jitRegInlineSize(c *regCode) int {
	return (len(c.rc.Code)*20 + 8) / 17
}

// jitRegLongWarming reports whether cl's function, c its register code and
// life its state, is too long to inline (jitRegInlineSize past
// ssa.MaxInline) and warming up, its budget not spent: begun and not spent,
// or not begun -- run fewer times than its first compile takes
// (jitCallThreshold) -- and not refused for host operations alone.
func (r *Runtime) jitRegLongWarming(cl *closure, c *regCode, life *jitEntry) bool {
	if jitRegInlineSize(c) <= ssa.MaxInline {
		return false
	}
	if e := r.jit.regCache[r.jitRegKeyOf(cl)]; e != nil && e.code == nil && e.hostOnly {
		// Refused, at its budget's end, for host operations alone
		// (jitRegHot): warmed up. The budget it warms on since is Go's
		// tier-up's, for code Go enters (jitRegCompile's LowerRegHot),
		// which, spent, is refused again: counted as its native callers'
		// warm-up, it had them call it through Go for good -- CD's
		// isInVoxel, let go of once, 3.2 million calls.
		return false
	}
	if life.warming {
		return life.tierBudget > 0
	}
	// Not begun: run fewer than jitCallThreshold times (jitRegInvoked).
	// Code the cache let go of had warmed up: compiled again at its native
	// callers' next call of Go.
	return int64(r.jitCallThreshold) > max(jitRegInvoked(cl, life), 1)
}

// jitRegCalledOften reports whether a function too long to inline, warming
// up (jitRegLongWarming), cl's, c its register code and life its state, is
// compiled at once for caller, the closure of the native caller whose call
// of it Go made, where V8 would inline it: with a budget begun, no longer
// than a function inlines in all (ssa.MaxInlineTotal), and invoked
// (jitRegInvoked) at least jitInliningFrequency hundredths as often as
// caller -- its runs while it had no code, and since Go's entries at its
// start, native callers' calls through its link and the calls Go made
// without entering it.
func (r *Runtime) jitRegCalledOften(cl *closure, c *regCode, life *jitEntry, caller *closure) bool {
	if caller == nil || caller.closureCode == nil || !life.warming || jitRegInlineSize(c) > ssa.MaxInlineTotal {
		return false
	}
	key := r.jitRegKeyOf(caller)
	l := r.jitRegLifeOf(caller)
	n := jitRegInvoked(caller, l) + int64(l.starts+l.slowCalls)
	if k := r.jit.regLinks[key]; k != nil {
		n += int64(k.in)
	}
	return jitRegInvoked(cl, life)*100 >= jitInliningFrequency*max(n, 1)
}

// jitRegInvoked is how many times cl's function, life its state, has run
// while it had no code: the runs its budget counted (jitRegCallTier), and
// those counted since (jitRegArm's ticks).
func jitRegInvoked(cl *closure, life *jitEntry) int64 {
	n := life.invoked
	if cc := cl.closureCode; cc != nil && cc.jitRegTickSet > 0 && cc.jitRegTickSet < math.MaxInt64 && cc.jitRegTick < cc.jitRegTickSet {
		n += cc.jitRegTickSet - max(cc.jitRegTick, 0)
	}
	return n
}

// jitRegCallsNatively reports whether a call cl's function's register code
// c makes would be a native call, of a callee its site's cache holds or its
// code learned (regCallSites): compiled for that alone, a function of host
// operations is worth its code. A call of a callee is native where the
// callee is cl's function itself, has no code and is not warming -- the
// native call compiles it (jitRegNativeCallee) -- or has code that native
// callers call: not code they call no more (notNative), whose calls native
// code Go entered would make through Go, as the interpreter makes them
// itself -- AWFY's Json's readChar, all of whose calls are of read, which
// leaves for Go at each substring.
func (r *Runtime) jitRegCallsNatively(cl *closure, c *regCode) bool {
	native := func(callee *closure) bool {
		if callee == nil || callee.fn == cl.fn {
			return callee != nil
		}
		l := r.jit.regLearned[r.jitRegKeyOf(callee)]
		if r.jitRegHas(callee) {
			return l == nil || !l.notNative
		}
		return l == nil || !l.warming && !l.invalid
	}
	if l := r.jit.regLearned[r.jitRegKeyOf(cl)]; l != nil {
		for _, x := range l.nativeCalls {
			// A built-in's made natively (a push, a charCodeAt), or a
			// function's.
			if x.cl == nil || native(x.cl) {
				return true
			}
		}
	}
	calls := r.regCallsRestored(cl, -1)
	for _, w := range c.rc.Code {
		if op := w.Op(); op != bytecode.RCallW && op != bytecode.RCallMW || uint(w.C()) >= uint(len(calls)) {
			continue
		}
		if e := &calls[w.C()]; jitRegCallKind(e) && r.jitRegCallable(e.callee, e.cl, e.rc, int(w.X())) && native(e.cl) {
			return true
		}
	}
	return false
}

// jitRegCallTier is the tier-up of cl's function, its register code c, at a
// call Go makes of it while it has no code (jitRegEnter): what its runs
// since the last spent of its budget -- each its length -- counted by the
// feedback dispatcher's returns (nativegen's feedback.go, regTickRet),
// which send its next call here once they have counted the runs the budget
// had left (jitRegArm). It returns the code compiled, nil where there is
// none yet.
func (r *Runtime) jitRegCallTier(cl *closure, c *regCode) *jitRegEntry {
	cc := cl.closureCode
	if !r.jitEnabled || cc.jitRegRefused || !jitRegCalled(cl, c) {
		// Never compiled for its calls: they count nothing more.
		cc.jitRegTick, cc.jitRegTickSet = math.MaxInt64, math.MaxInt64
		cc.jitEnters = cc.jitReg != nil
		return nil
	}
	runs := max(cc.jitRegTickSet-cc.jitRegTick, 1)
	e := r.jitRegHot(cl, c, runs*int64(max(len(c.rc.Code), 1)))
	if e == nil {
		r.jitRegArm(cl, c)
	}
	return e
}

// jitRegArm has the feedback dispatcher's returns count the runs of cl's
// function, its register code c, that are left of its budget, each its
// length, and its calls go on in their tiers until they have: then they
// send its next call to Go (jitEnters, jitRegCallTier). Where the budget
// is not counted yet -- no first call, a refusal of host operations
// alone -- the next run's return does.
func (r *Runtime) jitRegArm(cl *closure, c *regCode) {
	cc := cl.closureCode
	left := int64(1)
	life := r.jitRegLifeOf(cl)
	// The runs counted since the last, kept for the calls of it native
	// callers make while it warms (jitRegInvoked).
	life.invoked = jitRegInvoked(cl, life)
	if life.warming || life.invalid {
		n := int64(max(len(c.rc.Code), 1))
		left = max((life.tierBudget+n-1)/n, 1)
	}
	cc.jitRegTick, cc.jitRegTickSet = left, left
	cc.jitEnters = cc.jitReg != nil || r.jit.regPendingJobs[r.jitRegKeyOf(cl)] != nil
}

// jitRegHot spends work of the budget of cl's function, its register code
// c, at a sample, and returns its code: compiled once the budget is spent
// (jitRegWarmUp), nil before and where it is not compiled. Code that
// learned what to be compiled again for is, if that is due
// (jitRegLearnedDue).
func (r *Runtime) jitRegHot(cl *closure, c *regCode, work int64) *jitRegEntry {
	if r.jit != nil && r.jit.inFlight() {
		// A check: compiles in the background that are done installed
		// (jitDrain).
		r.jitDrain()
	}
	cc := cl.closureCode
	if cc.jitRegRefused {
		return nil
	}
	if e := cc.jitReg; e != nil {
		if !e.invalid && e.code != nil && e.cached {
			if e.life.reopt && r.jitRegLearnedDue(cl, e) {
				if ne := r.jitRegReoptimize(cl, c, e, -1); ne != nil {
					return ne
				}
			}
			return e
		}
		if !r.jitRegLetGoOf(cl, e) {
			cc.setJITReg(nil)
		}
	}
	life := r.jitRegLifeOf(cl)
	switch {
	case life.invalid:
		// Invalidated: compiled again once a fresh budget is spent.
		if !r.jitRegWarmUp(cl, c, life, work) {
			return nil
		}
	case !life.warming:
		// Its first sample: its budget begins, and the sample's work is
		// spent of it at once -- but where every call compiles. Not spent,
		// a call's first sample lost the run it counted, as a loop's had
		// lost a full budget of back edges before its back edges were
		// counted (jitRegEdgeWork).
		life.warming = true
		life.tierBudget, life.tierMark = jitRegBudget(r.jitCallThreshold, c), jitFeedbackMark(cl)
		if r.jitCallThreshold > 1 && !r.jitRegWarmUp(cl, c, life, work) {
			return nil
		}
	case !r.jitRegWarmUp(cl, c, life, work):
		return nil
	}
	if !jitRegTakes(cl, c) {
		cc.jitRegRefused = true
		return nil
	}
	e := r.jitRegFor(cl, c)
	switch {
	case e == nil:
		if life.deferred && life.generation == r.jit.generation || !jitCompiles(cl.fn, r.jitBudget()) {
			// No room for it now: it warms up again, and is asked for again
			// once it has spent another budget (jitRegArmEdges, jitRegArm)
			// -- which then costs a lookup until the cache lets code go
			// (jitRegForAs). Its budget left spent, every back edge's check
			// and every call Go made for it asked again: millions of asks
			// in a run whose JIT memory was full.
			life.tierBudget, life.tierMark = jitRegBudget(r.jitCallThreshold, c), jitFeedbackMark(cl)
			return nil
		}
		// Compiled in the background: installed at a later check, asked
		// again at the next sample, the budget spent.
		return nil
	case e.code == nil:
		// Refused for good, but for host operations alone, asked again once
		// a budget more is spent (jitRegCompile).
		cc.jitRegRefused = !e.hostOnly
		life.warming = false
		return nil
	}
	cc.setJITReg(e)
	return e
}

// jitRegBudget is the work the interpreter does with c's function before it
// is compiled, or compiled again: calls invocations, each its length.
func jitRegBudget(calls uint16, c *regCode) int64 {
	return int64(max(calls, 1)) * int64(max(len(c.rc.Code), 1))
}

// jitRegWarmUp spends work of the budget life holds for cl's function, its
// register code c, which has no valid code: before its first compile, or
// invalidated (jitRegInvalidate). It reports whether the budget is spent
// with the function's caches' feedback unchanged since it began -- a change
// raises what is left to jitICThreshold invocations' worth, as V8's
// NotifyICChanged does -- and the function deoptimized no more than
// jitMaxDeopts times.
func (r *Runtime) jitRegWarmUp(cl *closure, c *regCode, life *jitEntry, work int64) bool {
	if life.tierBudget -= work; life.tierBudget > 0 {
		return false
	}
	if m := jitFeedbackMark(cl); m != life.tierMark {
		life.tierBudget, life.tierMark = max(life.tierBudget, jitRegBudget(r.jitICThreshold, c)), m
		return false
	}
	return life.deopts <= jitMaxDeopts
}

// jitRegInvalidate invalidates e, cl's function's code, after a
// deoptimization: Go enters it no more, nor do native callers call it (its
// link), and the interpreter runs the function until it has spent a fresh
// budget (jitRegWarmUp), what its caches learn meanwhile delaying the
// compile. Runs of the code under way go on in it until they next leave it.
// Past jitMaxDeopts deoptimizations the function is compiled no more, its
// code let go of.
func (r *Runtime) jitRegInvalidate(cl *closure, e *jitRegEntry) {
	life := e.life
	if life == nil || e.code == nil {
		return
	}
	if life.deopts < ^uint16(0) {
		life.deopts++
	}
	s := r.jit
	cc := cl.closureCode
	if !e.invalid {
		e.invalid = true
		if s.regCache[e.key] == e {
			s.regLinkUpdate(e.key, nil)
		}
		s.invalidated++
		if cc != nil && cc.jitReg == e {
			cc.setJITReg(nil)
		}
		if c := cl.reg; c != nil {
			life.invalid, life.warming, life.reopt, life.learning = true, true, false, false
			life.tierBudget, life.tierMark = jitRegBudget(r.jitCallThreshold, c), jitFeedbackMark(cl)
		}
	}
	if life.deopts > jitMaxDeopts && cc != nil && !cc.jitRegRefused {
		cc.jitRegRefused = true
		if s.regCache[e.key] == e {
			s.regCache[e.key] = &jitRegEntry{key: e.key, life: life, refusal: fmt.Sprintf("deoptimized more than %d times", jitMaxDeopts)}
			s.dropReg(e.key, e)
		}
	}
}

// jitRegReopt marks e, cl's function's code, to be compiled again for what
// it learned at its calls of Go or its exits (jitRegCallSeen), no
// deoptimization: it stays valid, entered by Go and called by native
// callers, as V8's code runs on with feedback it was not compiled for, and
// is compiled again once it has spent a budget since with nothing learned
// meanwhile (jitRegLearnedDue) -- at a sample, or in a run, at an exit or a
// loop's poll (jitRegGoOnAt, jitRegLeft), which goes on in the new code.
func (r *Runtime) jitRegReopt(cl *closure, e *jitRegEntry) {
	if e.invalid || e.life == nil {
		return
	}
	e.life.reopt = true
}

// jitRegLearnedDue reports whether e's code, cl's function's, valid, is due
// to be compiled again for what it learned (jitRegReopt): once it has spent
// a budget since it began to learn (jitRegSpent), with nothing learned
// meanwhile (jitLearnedMark), which raises what is left. Not while a native
// call of it has not returned (regCalled), which finds its code in the
// cache (jitRegLevelAt), nor while it is compiled in the background; but
// while Go finishes levels of it, or runs it, which go on in their own
// code, as V8 installs new code while older frames are in the old.
func (r *Runtime) jitRegLearnedDue(cl *closure, e *jitRegEntry) bool {
	return r.jitRegLearnedDueWith(cl, e, len(r.jit.ssaCtxs), 0)
}

// jitRegLearnedDueWith is jitRegLearnedDue for a call of Go whose leaving
// unwinds the native calls in the contexts from limit on
// (jitRegCompileDueNow): not while a native call of e's code in one below
// limit has not returned. pending is the work the run's stretch under way
// has done natively, which its end will count (jitRegRunWork): counted at
// each call of Go, as V8's optimized code spends its function's budget at
// each JumpLoop, not only where the stretch ends -- at an exit, or at a
// poll the varied interrupt budget makes -- a loop that learned at its
// calls of Go and runs on natively was compiled again for it a budget
// after its first poll or later, wherever that came: Go made 4,237 of a
// loop's constructions of arrays at one sequence of budgets, 730 at
// another, before its code was compiled again to make them from pools.
func (r *Runtime) jitRegLearnedDueWith(cl *closure, e *jitRegEntry, limit int, pending int64) bool {
	life := e.life
	if e.invalid || e.code == nil || life == nil || !life.reopt || r.jit.regPendingJobs[e.key] != nil || r.jit.regCalledBelow(e, limit) {
		return false
	}
	c := cl.reg
	if c == nil {
		return false
	}
	spent := r.jitRegSpent(c, e) + pending
	if !life.learning {
		life.learning, life.tierBase, life.tierMark = true, spent, jitLearnedMark(cl, life)
		life.tierBudget, life.tierLearned = jitRegBudget(r.jitCallThreshold, c), jitLearnedCount(life)
		return false
	}
	if n := jitLearnedCount(life); n != life.tierLearned {
		// Learned more since its budget began: what is left raised to
		// jitICThreshold invocations' worth from now, as V8's
		// NotifyICChanged raises a function's budget when its feedback
		// changes. Noticed only once the budget was spent, the raise
		// counted from then: a loop that learned its second call of Go a
		// turn after its first spent a budget, then another, before it
		// was compiled again for both.
		life.tierBudget = max(life.tierBudget-(spent-life.tierBase), jitRegBudget(r.jitICThreshold, c))
		life.tierBase, life.tierMark, life.tierLearned = spent, jitLearnedMark(cl, life), n
		return false
	}
	if spent-life.tierBase < life.tierBudget {
		return false
	}
	if m := jitLearnedMark(cl, life); m != life.tierMark {
		life.tierBudget = max(life.tierBudget-(spent-life.tierBase), jitRegBudget(r.jitICThreshold, c))
		life.tierBase, life.tierMark = spent, m
		return false
	}
	return true
}

// jitRegSpent is the work e's function has done since its code was first
// compiled: its invocations, each its length -- Go's entries at its start,
// native callers' calls through its link, the runs the interpreter went on
// with after the code left, the calls Go made without entering it
// (slowCalls) -- the work its native stretches did (jitRegStretchWork) and
// its native calls between their calls of Go (jitRegCalleeProfit's
// calleeWork), and the interpreter's in its loops before Go entered them
// (jitEntry's osrWork).
func (r *Runtime) jitRegSpent(c *regCode, e *jitRegEntry) int64 {
	life := e.life
	calls := life.starts + life.goOns + life.slowCalls
	if l := r.jit.regLinkFor(e); l != nil {
		calls += l.in
	}
	st := &life.ssaStats
	return int64(calls)*int64(max(len(c.rc.Code), 1)) + int64(st.work+st.calleeWork) + life.osrWork
}

// jitRegReoptimize compiles cl's function's register code c again for what
// e, its code, learned, and returns the new code, which replaces e in the
// cache, its link and its closure's (jitRegCached): runs in e go on in it
// until they next leave it -- one Go goes on with now at register pc at,
// where at is not -1 (jitRegGoOnAt). nil where it was not compiled again:
// no room, or a compile in the background, after which it is asked again
// once a budget more is spent; or a refusal, which keeps e, not asked
// again for what it has learned so far.
func (r *Runtime) jitRegReoptimize(cl *closure, c *regCode, e *jitRegEntry, at int) *jitRegEntry {
	life := e.life
	// The frames that go on in the new code after the calls they are in,
	// at entries of it (resumed), as V8's compile for a frame is asked with
	// its bytecode offset: the code is built with no entry after a call
	// until Go has gone on there (ssa.Func's Unresumed). The one Go goes on
	// with now, at: wherever that is, as the call it went on after may be
	// one the new code makes natively, learned for it. Compiled without it,
	// it was compiled again at once with it (jitRegResumeSeen) -- zlib's
	// loop of ten thousand instructions twice, in a row, at each compile
	// for what it learned. And the runs of e's code Go is inside, after
	// their calls of Go, where e has no entry. Not a loop's header, which
	// this compile keeps for the frame where it is due (jitRegFrameAt), and
	// no later one: learned for good, its entry was never one escape
	// analysis could drop, its objects made at every turn, and a frame that
	// stayed there never went on in code of its own (jitRegLoopCode).
	if life != nil && at >= 0 && !slices.Contains(life.resumed, uint32(at)) && (at == 0 || regLoopAround(c, at) != at) {
		life.resumed = append(life.resumed, uint32(at))
	}
	s := r.jit
	for k := 0; k < s.ctxTop && k < len(s.regRuns) && k < len(s.ssaCtxs); k++ {
		if run := &s.regRuns[k]; run.e == e && run.f != nil && s.ssaCtxs[k].Live == 0 {
			e.resumeAt(int(s.ssaCtxs[k].ExitPC) + 1)
		}
	}
	ne := r.jitRegCompileFor(cl, c, e.key, e.callee)
	switch {
	case ne == nil:
		life.reopt, life.learning = true, false
		return nil
	case ne.code == nil:
		// The code it had stays (jitRegCompileFor).
		life.reopt, life.learning = false, false
		return nil
	}
	if cc := cl.closureCode; cc != nil && !cc.jitRegRefused {
		cc.setJITReg(ne)
	}
	return ne
}

// jitRegLearnedFor is what f's function's code learned (regLearned): e's,
// the code running f, without a key made -- a weak handle and a map lookup
// at each call of Go -- where e is f's function's code of this realm.
func (r *Runtime) jitRegLearnedFor(f *frame, e *jitRegEntry) *jitEntry {
	if e != nil && e.life != nil && f.cl.reg != nil && e.rc == f.cl.reg.rc && e.key.realm == r.jitRealmKey() {
		return e.life
	}
	return r.jitState().regLearnedOf(r.jitRegKeyOf(f.cl))
}

// regLearnedOf is what the code of the function whose key is key learned
// at its exits (regLearned), made if it has learned nothing yet.
func (s *jitState) regLearnedOf(key jitKey) *jitEntry {
	if l := s.regLearned[key]; l != nil {
		return l
	}
	if s.regLearned == nil {
		s.regLearned = map[jitKey]*jitEntry{}
	}
	// Kept in the room for it, the cache letting go of what it may
	// (makeRoom), other functions' state among it. Where there is none --
	// all the cache holds young code, say -- it is kept all the same,
	// counted once its function is compiled, which asks for its room, or at
	// the cache's next look (regAge), whose room it is then made among: it
	// is made at the function's first sample, after its first run, and a
	// loop driving a program's phases had none made for it, and so no code,
	// once the first phase had filled the budget.
	s.budget = s.rt.jitBudget()
	room := s.makeRoom(jitEntryFixed)
	// Numbered (serial), which makeRoom takes the least recently made of
	// first among those of an epoch.
	s.serial++
	l := &jitEntry{jitLearned: &jitLearned{reg: true, regLink: s.regLinks[key]}, used: s.epoch(), serial: s.serial, regCounted: room}
	s.regLearned[key] = l
	s.regAccount(l)
	return l
}

// jitRegPropOp is the named property operation of the register instruction
// at rpc of c, on the frame whose window is at wb, as the JIT's learning
// sees it (jitPropOp): get_prop's, get_method's and get_method_w's reads,
// set_prop's write, add_prop's read and store, which share the store's
// site; false for any other instruction.
func (r *Runtime) jitRegPropOp(f *frame, c *regCode, wb, rpc int) (jitPropOp, bool) {
	if rpc < 0 || rpc >= len(c.rc.Code) {
		return jitPropOp{}, false
	}
	w := c.rc.Code[rpc]
	recv := int(w.B())
	op := jitPropOp{}
	switch w.Op() {
	case bytecode.RGetProp:
	case bytecode.RGetMethod, bytecode.RGetMethodW:
		op.method = true
	case bytecode.RSetProp:
		op.write, recv = true, int(w.A())
	case bytecode.RAddProp, bytecode.RAddPropI:
		recv = int(w.A())
	default:
		return jitPropOp{}, false
	}
	site := int(w.C())
	if site >= len(c.rc.SiteName) || int(c.rc.SiteName[site]) >= len(f.cl.names) || wb+recv >= len(r.stack) {
		return jitPropOp{}, false
	}
	op.key, op.recv = f.cl.names[c.rc.SiteName[site]], r.stack[wb+recv]
	if site < len(f.cl.ic) {
		op.c = &f.cl.ic[site]
	}
	return op, true
}

// jitRegSeenFeedback adds to what cl's register code c learned (regLearned)
// what its property sites' caches have seen besides their entries, at each
// compile: the shapes the interpreter met at each, which the code is
// compiled for from the first, not learned one exit and one compile at a
// time.
func (r *Runtime) jitRegSeenFeedback(cl *closure, c *regCode) {
	l := r.jitState().regLearnedOf(r.jitRegKeyOf(cl))
	for rpc, w := range c.rc.Code {
		write := false
		switch w.Op() {
		case bytecode.RGetProp, bytecode.RGetMethod, bytecode.RGetMethodW:
		case bytecode.RSetProp:
			write = true
		default:
			// add_prop's site is its store's, not its read's.
			continue
		}
		site := int(w.C())
		if site >= len(cl.ic) || site >= len(c.rc.SiteName) || int(c.rc.SiteName[site]) >= len(cl.names) {
			continue
		}
		r.jitSeenSite(l, rpc, &cl.ic[site], write, cl.names[c.rc.SiteName[site]], true)
	}
}

// jitRegSeenInlined is jitRegSeenFeedback for cl, a callee a compile asks
// to inline, k the compile's root feedback: once a compile, as no script
// runs while it builds, which asks an inlined callee's sites at each of
// its calls and at each of its asks of a call (ssa's plan, inlineSiteAt).
// Asked again at each, the scans of its code had been a ninth of a
// register build's time.
func (r *Runtime) jitRegSeenInlined(k *jitFeedback, cl *closure) {
	if slices.Contains(k.regSeen, cl) {
		return
	}
	k.regSeen = append(k.regSeen, cl)
	r.jitRegSeenFeedback(cl, cl.reg)
}

// jitRegDeoptSeen learns from a deoptimization of e, code made from c, at
// register pc rpc, the frame's window at wb holding the state at its
// instruction's start, before the interpreter runs it: a property site's
// receiver of a shape its cases do not have, or that its cache cannot hold
// (jitDeoptSeenOp) -- the site's cache learning the shape as the
// interpreter makes the operation, the code compiled again with its cases
// -- and any other guard's failure as a register pc the next compiles build
// generic.
func (r *Runtime) jitRegDeoptSeen(f *frame, c *regCode, e *jitRegEntry, wb, rpc int) {
	l := r.jitRegLearnedFor(f, e)
	// An element read made as a number that met what is none, or an
	// addition that met a string (jitRegElemSeen, jitRegStringAddSeen):
	// read as they are, or Go's, from the next lowering on -- and generic
	// too.
	r.jitRegElemSeen(l, c, wb, rpc, false)
	r.jitRegStringAddSeen(l, c, wb, rpc)
	if op, ok := r.jitRegPropOp(f, c, wb, rpc); ok {
		r.jit.shapeDeopts++
		r.jitDeoptSeenOp(l, rpc, op, e.serial)
		return
	}
	if !slices.Contains(l.failed, int32(rpc)) {
		l.failed = append(l.failed, int32(rpc))
	}
}

// jitRegEntrySeen learns that an entry's speculation of f's function's code
// failed at register pc rpc -- a loop's header or the start, the registers
// it loads not of the kinds the code was compiled for -- which the next
// compiles enter generic there (jitRegFeedback's EntryGeneric, from
// failedEntries), the instructions there not made generic for it.
func (r *Runtime) jitRegEntrySeen(f *frame, rpc int) {
	l := r.jit.regLearnedOf(r.jitRegKeyOf(f.cl))
	if !slices.Contains(l.failedEntries, uint32(rpc)) {
		l.failedEntries = append(l.failedEntries, uint32(rpc))
	}
}

// resumeAt learns that a frame goes on in the function's code at register
// pc rpc, where e's code, built with no entry after the call before it,
// has none (unresumed): the function's next code is built with the entry
// (jitRegFeedback's Resumed).
func (e *jitRegEntry) resumeAt(rpc int) {
	if l := e.life; l != nil && rpc >= 0 && slices.Contains(e.unresumed, rpc) && !slices.Contains(l.resumed, uint32(rpc)) {
		l.resumed = append(l.resumed, uint32(rpc))
	}
}

// jitRegResumeSeen learns that Go goes on at register pc rpc of e's code,
// valid, which has no entry there: where that is after a call the code was
// built with no entry after (jitRegEntry's unresumed), the function is
// compiled again now with the entry (jitRegFeedback's Resumed), and the
// frame goes on in that code from here: it is returned, nil where there is
// none -- a native call in the code, a compile in the background, which the
// function's next code has the entry from.
func (r *Runtime) jitRegResumeSeen(f *frame, c *regCode, e *jitRegEntry, rpc int) *jitRegEntry {
	if e.invalid || e.code == nil || e.life == nil || f.cl.reg != c || !slices.Contains(e.unresumed, rpc) {
		return nil
	}
	l := e.life
	if slices.Contains(l.resumed, uint32(rpc)) {
		return nil
	}
	l.resumed = append(l.resumed, uint32(rpc))
	if r.jit.regPendingJobs[e.key] != nil || r.jit.regCalled(e) {
		r.jitRegReopt(f.cl, e)
		return nil
	}
	ne := r.jitRegReoptimize(f.cl, c, e, -1)
	if ne == nil {
		return nil
	}
	if ipc := ne.ipc(rpc); ipc < 0 || !ne.code.HasEntry(ipc) {
		return nil
	}
	return ne
}

// jitRegHostSeen learns from a host exit at register pc rpc of c, before Go
// runs its instruction: a property read native code left to Go -- a
// megamorphic site's receiver that neither its table nor the megamorphic
// cache had, a primitive's (jitPolySeenOp). A write's it learns after Go
// made it (jitRegWriteSeen), from what it returns: its operation, the
// object written and its shape before.
func (r *Runtime) jitRegHostSeen(f *frame, c *regCode, wb, rpc int) jitRegWrite {
	op, ok := r.jitRegPropOp(f, c, wb, rpc)
	if !ok {
		// An element's read or write native code left to Go: what it met,
		// read or stored natively, or by Go from native code, once
		// compiled again (jitRegElemSeen).
		return jitRegWrite{elem: r.jitRegElemSeen(r.jit.regLearnedOf(r.jitRegKeyOf(f.cl)), c, wb, rpc, true)}
	}
	if op.write {
		x := jitRegWrite{op: op, write: true}
		if op.recv.IsObject() {
			x.o = op.recv.Object()
			x.before = x.o.shape
		} else if !op.recv.IsNullish() && !r.jitStress.primLeave {
			// A write of a primitive's property -- a number's, a
			// string's, a boolean's -- which the code compiled again
			// leaves to Go from native code (jitPolySite's prim), as a
			// read of one: it had left native code at every such write,
			// with no entry after it, the interpreter running the rest of
			// the invocation.
			e := r.jit.regLearnedOf(r.jitRegKeyOf(f.cl))
			if p := jitPolySiteAt(e, rpc); !p.prim {
				p.prim, p.mega = true, true
				e.polyReopt = true
			}
		}
		return x
	}
	r.jitPolySeenOp(r.jit.regLearnedOf(r.jitRegKeyOf(f.cl)), rpc, op)
	return jitRegWrite{}
}

// jitRegPrivateSeen learns from a host exit of e's code at get_private,
// register pc rpc of c, before Go runs it: a member that is its object's
// own data but no number -- a private method's function -- the read met,
// which the code compiled again reads as a value (jitNumberMissed),
// compiled again from here (jitRegReopt). The shapes the read meets the
// interpreter's run of it adds to its site's cache, which the code compiled
// again is compiled for.
func (r *Runtime) jitRegPrivateSeen(f *frame, c *regCode, e *jitRegEntry, wb, rpc int) {
	if rpc < 0 || rpc >= len(c.rc.Code) {
		return
	}
	w := c.rc.Code[rpc]
	if w.Op() != bytecode.RGetPrivate || wb+int(w.B()) >= len(r.stack) || wb+int(w.C()) >= len(r.stack) {
		return
	}
	o, k := r.stack[wb+int(w.B())], r.stack[wb+int(w.C())]
	if !o.IsObject() {
		return
	}
	ob := o.Object()
	i := ob.findOwn(regPrivateKey(f, k))
	if i < 0 || ob.shape == nil || i >= ob.shape.n || int(i) >= int(ob.valn) || ob.shape.keys[i].isAccessor() || ob.vals()[i].IsNumber() {
		return
	}
	l := r.jit.regLearnedOf(r.jitRegKeyOf(f.cl))
	if slices.Contains(l.failed, int32(rpc)) {
		return
	}
	jitNumberMissed(l, rpc)
	r.jitRegReopt(f.cl, e)
}

// jitRegWrite is a property write a host exit is about to have Go make
// (jitRegHostSeen): its operation, and the object it writes, of shape
// before then.
type jitRegWrite struct {
	op     jitPropOp
	write  bool
	o      *Object
	before *shape
	// elem says an element's access learned what the code is to be
	// compiled again for (jitRegElemSeen).
	elem bool
}

// jitRegWriteSeen learns from the property write x at register pc rpc,
// which Go made at a host exit: the cases its cache now knows
// (jitPolyWriteSeenOp).
func (r *Runtime) jitRegWriteSeen(f *frame, c *regCode, e *jitRegEntry, rpc int, x jitRegWrite) {
	if x.op.c == nil {
		return
	}
	r.jitPolyWriteSeenOp(r.jitRegLearnedFor(f, e), rpc, x.op.c, x.op.key, true, x.o, x.before)
}

// jitRegEnter runs f, the frame regEnterW made for a call of cl, its
// register code c and its window at base, in cl's function's native code
// from register pc start -- its start, or, for the frame of a generator or
// an async function regGenRun made again, where it resumes -- and in the
// interpreter where the code leaves it, or has no entry there.
func (r *Runtime) jitRegEnter(f *frame, cl *closure, c *regCode, base, start int) (Value, error) {
	e := cl.closureCode.jitReg
	if r.jit != nil && r.jit.inFlight() {
		// A call is a check: compiles in the background that are done
		// installed now -- its first, or one replacing the code it has,
		// which goes on running until then -- and entered from this call
		// on.
		r.jitDrain()
		e = cl.closureCode.jitReg
	}
	resume := start != 0 || c.suspends
	if resume {
		cc := cl.closureCode
		if cc.jitResumeSkip != 0 {
			// One of the resumptions counted at the last look (below).
			cc.jitResumeSkip--
			return r.runRegFor(f, c, start)
		}
		if e != nil && e.life.entrySlow && !e.invalid && !e.life.reopt {
			// A resumption of a generator's or an async function's code
			// judged slow (jitRegStretchWork), which Go does not enter
			// until it retries it (jitRegRetrySlow): the next jitSSAProbe
			// resumptions, counted now, run in the interpreter without a
			// look at the code. Before, every resumption entered the code,
			// which left at once at its yield or await.
			e.life.slowCalls += jitSSAProbe
			cc.jitResumeSkip = jitSSAProbe
			r.jit.slowResumes++
			if r.jitRegRetrySlow(e); e.life.entrySlow {
				return r.runRegFor(f, c, start)
			}
		}
	}
	if e == nil && start == 0 {
		// Its calls' runs have spent what was left of its budget: compiled
		// now if it is spent, counted on otherwise (jitRegCallTier).
		e = r.jitRegCallTier(cl, c)
	}
	if e != nil && e.life.entrySlow && !resume && !e.life.reopt {
		// Code that does not pay (jitRegStretchWork): the interpreter runs
		// the call, until it is retried. Not code that learned what to be
		// compiled again for, which, never entered, would never be compiled
		// for it (below).
		if r.jitRegRetrySlow(e); e.life.entrySlow {
			return r.runRegFor(f, c, start)
		}
	}
	if e == nil || e.invalid || e.code == nil || !e.cached || !r.jitEnabled {
		if e != nil && !r.jitRegLetGoOf(cl, e) && e.code == nil {
			// Let go of by the cache (jitRegLetGoOf) -- code kept for a run
			// or a native call still in it (regClose) among it, which this
			// call is not -- or no code.
			cl.closureCode.setJITReg(nil)
		}
		return r.runRegFor(f, c, start)
	}
	if !r.jitRegContextFree() {
		// As deep in native code, through Go, as there are contexts: the
		// interpreter runs the frame, the code neither entered nor judged
		// for it.
		return r.runRegFor(f, c, start)
	}
	if start == 0 {
		e.life.starts++
		if e.life.notNative {
			r.jitRegRetryNative(e)
		}
		if len(e.kept) != 0 && !e.invalid && e.life.starts-e.life.keptAt >= jitKeptStarts {
			// Called again and again: objects left unmade pay now, unless
			// its calls leave the loop anyway. Kept, a loop compiled for
			// the frame waiting at it made its objects at every call for
			// good: carried_long allocated each turn's.
			leaving := r.jitRegKeptCallsLeave(e)
			e.life.keptAt, e.life.keptLeft = e.life.starts, 0
			if !leaving {
				if ne := r.jitRegKeptGone(cl, c, e); ne != nil {
					e = ne
				}
			}
		}
		// Code that learned what to be compiled again for is, once it has
		// spent a budget (jitRegLearnedDue), at a call as at a sample or an
		// exit: a function called rarely, whose code runs its loop natively
		// and leaves it nowhere, had never been compiled for the callbacks
		// it learned at its calls of Go.
		if e.life.reopt && r.jitRegLearnedDue(cl, e) {
			if ne := r.jitRegReoptimize(cl, c, e, -1); ne != nil {
				e = ne
			}
		}
		if e.life.entrySlow {
			// Judged slow, its compile again not due yet: the
			// interpreter's, as above.
			if r.jitRegRetrySlow(e); e.life.entrySlow {
				return r.runRegFor(f, c, start)
			}
		}
	} else if ipc := e.ipc(start); e.code != nil && ipc >= 0 && e.code.HasEntry(ipc) {
		r.jit.regResumes++
	}
	return r.jitRegRun(f, c, e, f.base-int(c.locals), start)
}

// jitRegLoop is the back edge to register pc target's sample, in the frame
// st runs (regBackEdge): its function's hotness, and its code entered at
// the loop's header where it has an entry there. It returns the pc the
// dispatcher goes on at: target where the code is not entered; where the
// code left the frame to it; a return of the code's own where the code
// returned, its register given the value (jitRegEntry's returns); regDone
// where the code threw what none of the frame's handlers takes, the error
// in st.
//
// The check jitRegGoOnAt made due at once, after native code left a frame
// where it has no entry (jitState's regGoOn), is no sample: no budget the
// interpreter spent, whatever frame's back edge it is. The frame Go goes on
// in enters its code at the loop's header -- the code of the run that left
// it, or the code that replaced it.
func (r *Runtime) jitRegLoop(st *regState, target int) int {
	s := r.jit
	r.backEdges = s.backEdgeBudget()
	forced := false
	if s != nil && s.regForced {
		forced, s.regForced = true, false
	}
	f, c := st.f, st.c
	cl := f.cl
	var from *jitRegEntry
	if forced {
		from = s.regGoOnFrom
		s.regGoOnFrom = nil
	}
	if !r.jitEnabled || cl == nil || cl.closureCode == nil || cl.reg != c {
		if forced {
			s.regGoOn = nil
		}
		switch {
		case cl == nil || cl.closureCode == nil:
		case !r.jitEnabled:
			regEdgesOff(cl)
		default:
			// Not compiled from this code: its back edges count a budget
			// before they look again.
			left := jitRegBudget(r.jitCallThreshold, c)
			cl.closureCode.jitRegEdges, cl.closureCode.jitRegEdgesSet = left, left
		}
		return target
	}
	var e *jitRegEntry
	// The run the frame goes on in is judged as the run that left it
	// (regGoOnOSR, jitRegGoOnAt's regGoOnRun); a sample's, as begun at the
	// loop.
	osr := true
	if forced {
		if s.regGoOn == f {
			e = cl.closureCode.jitReg
			osr = s.regGoOnOSR
		} else {
			from = nil
		}
		s.regGoOn = nil
		if e == nil || e.invalid || e.code == nil {
			return target
		}
		if from != nil && (from == e || from == e.osr) && !from.invalid && from.code != nil && from.goesOn && !jitRunSlow(from.life, osr) {
			if _, ok := from.loopEntry(target); ok && r.jitRegContextFree() {
				// The code whose run left the frame, which has an entry at
				// the loop: gone on in, nothing looked up again -- a frame
				// that goes on in the code kept for it, from that code, is
				// none jitRegFramesLeave counts.
				s.wentOn++
				return r.jitRegLoopGoOn(st, from, target, osr)
			}
		}
	} else {
		n := int(st.kept.pc) - target
		// The work its frames' back edges did since the last check, each
		// its distance (jitRegEdgeWork): counted, not sampled, so that
		// when a loop is compiled does not hang on where the interrupt
		// budget ends. Sampled -- a full budget of back edges times the
		// loop's length where it ended in the loop -- a loop whose turns
		// call functions with loops of their own, Crypto's bnModPow, was
		// sampled a few times a run, or none, each sample worth a third of
		// its budget or more: whether it was compiled in its third run or
		// its sixth hung on which samples landed. The check is where the
		// count ran out, its function's budget spent, or where the
		// interrupt budget did.
		cc := cl.closureCode
		work := jitRegEdgeWork(cc)
		if life := r.jitRegLifeIfAny(cl); (life == nil || !life.regFed && !life.warming && !life.invalid) && cc.jitReg == nil {
			// Its loops' first work, before its budget begins: V8's
			// interrupt budget for feedback allocation, whose end makes a
			// function's feedback vector, which holds its budget. Begun at
			// its first back edge, a function compiled at once (a test's
			// warm-up of one invocation) was compiled after a turn of its
			// loop, its caches' feedback that one turn's.
			if cc.jitRegEdgesSet == 0 {
				// No more than its budget, for a runtime that compiles a
				// function at once.
				left := min(int64(jitBudgetForFeedback), jitRegBudget(r.jitCallThreshold, c))
				cc.jitRegEdges, cc.jitRegEdgesSet = left, left
				return target
			}
			if cc.jitRegEdges > 0 {
				// The interrupt budget's check, before the feedback's work
				// is done.
				return target
			}
			if life == nil {
				// Its feedback's work done: its state made now -- a runtime
				// whose loops had done less, a cold one, has none.
				life = r.jitRegLifeOf(cl)
			}
			life.regFed, work = true, 0
		}
		// A compile this check asks for keeps the entry of the loop the
		// frame waits at (jitRegFrameAt).
		r.jitFrameFn, r.jitFramePC = cl.fn, target
		e = r.jitRegHot(cl, c, work)
		r.jitFrameFn = nil
		r.jitRegArmEdges(cl, c)
		if e != nil {
			e.loopPC, e.loopLen = target, max(n, 1)
			// The interpreter's work in the loop, which Go enters, counted
			// toward what the code spends (jitRegSpent).
			e.life.osrWork += work
		}
	}
	if e != nil && e.life.loopSlow && !forced {
		// Code whose runs begun at a loop do not pay (loopSlow): the
		// interpreter runs the loop, and Go enters it again once retried
		// (jitRegRetrySlow), which counts the call (slowCalls): counted
		// here too, each had been two, and the code retried at half the
		// calls.
		r.jitRegRetrySlow(e)
	}
	if e == nil || !e.goesOn || jitRunSlow(e.life, osr) {
		return target
	}
	if e = r.jitRegLoopCode(f, c, e, target, from); e == nil || !r.jitRegContextFree() {
		// Or as deep in native code as there are contexts
		// (jitRegContextFree).
		return target
	}
	if forced {
		s.wentOn++
	} else {
		// Entered by on-stack replacement (the state made since s was
		// read).
		r.jit.osrs++
	}
	return r.jitRegLoopGoOn(st, e, target, osr)
}

// jitRunSlow reports whether code, what its function's code learned being
// life, does not pay for a run judged as begun at a loop's header (osr) --
// found slow for its loops (loopSlow) -- or as begun at its start, where
// its entries do not pay (entrySlow).
func jitRunSlow(life *jitEntry, osr bool) bool {
	if osr {
		return life.loopSlow
	}
	return life.entrySlow
}

// jitRegLoopGoOn goes on in e's code, at the header of the loop at
// register pc target, with the frame st runs, in a run judged as begun at
// the loop (osr) or not, and returns the pc the dispatcher goes on at, as
// jitRegLoop does.
func (r *Runtime) jitRegLoopGoOn(st *regState, e *jitRegEntry, target int, osr bool) int {
	r.jit.regGoOnRun = 1
	if osr {
		r.jit.regGoOnRun = 2
	}
	pc, v, done, err := r.jitRegGoOn(st.f, st.c, e, st.wb, target)
	switch {
	case err != nil:
		st.err = err
		return regDone
	case !done:
		return pc
	case v.IsUndefined() && e.retUndef >= 0:
		return e.retUndef
	case e.retPC >= 0:
		st.regs[e.retReg] = v
		return e.retPC
	}
	// Unreachable: code that returns a value has a return of one.
	panic("vm: native code returned where its register code has no return")
}

// jitRegLoopCode is the code f, running c, goes on in at the header of its
// loop at register pc target, e being its function's: e where it has an
// entry there; or the code compiled for frames waiting at a loop whose
// entry escape analysis dropped from e (unentered): a frame found there
// with no entry at two checks in a row -- each a full budget of back edges,
// or the check made due at once after its code left it -- has the function
// compiled apart with that loop's entry kept (ssa.Func's FrameAt, its
// objects made), and goes on in that code, kept beside e (osr) for the
// frames that wait there next. nil where there is none. from is the code
// whose run left the frame to the interpreter, where the check is the one
// made due at once for it (jitRegLeaveTo), nil at a sample: a frame that
// goes on in the code kept for it, from other code, is counted as one that
// needs it (jitRegFramesLeave).
func (r *Runtime) jitRegLoopCode(f *frame, c *regCode, e *jitRegEntry, target int, from *jitRegEntry) *jitRegEntry {
	if _, ok := e.loopEntry(target); ok {
		return e
	}
	if o := e.osr; o != nil && !o.invalid && o.code != nil {
		if _, ok := o.loopEntry(target); ok {
			if from != o {
				if ne := r.jitRegFramesLeave(f.cl, c, e, target); ne != nil {
					return ne
				}
			}
			return o
		}
	}
	if e.invalid || !slices.Contains(e.unentered, target) || e.osrJob != nil {
		// No loop of its own to compile code for; or code for one being
		// compiled in the background, which the frame waits out in the
		// interpreter (jitRegInstall).
		return nil
	}
	if at := uintptr(unsafe.Pointer(f)); e.osrMiss != at || e.osrMissPC != target {
		e.osrMiss, e.osrMissPC = at, target
		return nil
	}
	e.osrMiss = 0
	if target <= 0 {
		return nil
	}
	// The JIT holds no more than its budget after, as after any compile,
	// the code the frame enters kept, with the function's code it goes
	// with (jitRegCompileFor, regKeepOSR).
	s := r.jit
	s.settleAfter++
	o := r.jitRegCompile(f.cl, c, e.key, false, target)
	defer func() {
		s.settleAfter--
		s.settleKeeping(o, e)
	}()
	if o == nil || o.code == nil {
		// No room, a refusal, or compiled in the background.
		return nil
	}
	r.jit.regKeepOSR(e, o)
	if _, ok := o.loopEntry(target); ok {
		return o
	}
	return nil
}

// regKeepOSR keeps o, code compiled for a frame waiting at a loop of e's
// function (jitRegLoopCode), beside e, the function's code, in place of
// what e kept before.
func (s *jitState) regKeepOSR(e, o *jitRegEntry) {
	if old := e.osr; old != nil {
		s.regHeld -= old.bytes
		old.dropped = true
		s.regClose(old)
	}
	o.osrOf, e.osr = e, o
	s.regHeld += o.bytes
	s.osrLearned++
}

// jitRegFramesLeave counts a frame of e's function, cl's, its register code
// c, going on in the code kept for its loop at register pc target
// (jitRegLoopCode) -- once each: where it leaves again, it goes on in that
// code -- and returns e's code compiled again with that loop's entry
// because most calls do, nil where it is not. Objects left unmade for a
// call that leaves are made at its exit, and it enters other code for the
// rest of its loop: each call pays that, and a compile of that code once
// its function's is compiled again, for the few turns before it left. So,
// as three calls in four leave, the header is learned as an entry Go goes
// on at (jitLearned's resumed, jitRegFeedback's Resumed), the function's
// code keeps its objects made there, as if it had never left them unmade,
// and the code kept goes with the code it was kept beside (dropReg).
func (r *Runtime) jitRegFramesLeave(cl *closure, c *regCode, e *jitRegEntry, target int) *jitRegEntry {
	life := e.life
	if life == nil || e.invalid {
		return nil
	}
	if life.osrFrames++; life.osrFrames < jitFramesLeaving {
		return nil
	}
	calls := life.starts
	if l := r.jit.regLinkFor(e); l != nil {
		calls += l.in
	}
	// Compiling the code counts its native callers' calls afresh
	// (jitRegCached): then, the count begins.
	most := calls >= life.osrCalls && (calls-life.osrCalls)*3 <= jitFramesLeaving*4
	life.osrFrames, life.osrCalls = 0, calls
	if !most || slices.Contains(life.resumed, uint32(target)) {
		return nil
	}
	life.resumed = append(life.resumed, uint32(target))
	if r.jit.regPendingJobs[e.key] != nil || r.jit.regCalled(e) {
		// Compiled in the background, or run by native calls under way:
		// compiled for it once due.
		r.jitRegReopt(cl, e)
		return nil
	}
	ne := r.jitRegReoptimize(cl, c, e, -1)
	if ne == nil {
		return nil
	}
	if _, ok := ne.loopEntry(target); !ok {
		return nil
	}
	return ne
}

// jitRegKeptCallsLeave reports whether three in four of the calls of e's
// function since it last looked (jitRegEnter) left its code, and enough of
// them to tell: its calls Go made and its native callers' through its link.
func (r *Runtime) jitRegKeptCallsLeave(e *jitRegEntry) bool {
	life := e.life
	calls := life.starts - life.keptAt
	if l := r.jit.regLinkFor(e); l != nil {
		calls += l.in
	}
	return life.keptLeft >= jitKeptStarts*3/4 && life.keptLeft*4 >= calls*3
}

// jitRegKeptGone compiles e's code, cl's function's, its register code c,
// again without the entries it has for a frame (jitRegEntry's kept): no
// frame waits at a loop for this compile. It returns the new code, nil
// where it was not compiled now -- run by native calls under way, or
// compiled in the background -- when it is once it has spent a budget
// (jitRegReopt).
func (r *Runtime) jitRegKeptGone(cl *closure, c *regCode, e *jitRegEntry) *jitRegEntry {
	life := e.life
	life.keptGone = true
	s := r.jit
	if s.regPendingJobs[e.key] != nil || s.regCalled(e) {
		r.jitRegReopt(cl, e)
		return nil
	}
	fn, pc := r.jitFrameFn, r.jitFramePC
	r.jitFrameFn = nil
	ne := r.jitRegReoptimize(cl, c, e, -1)
	r.jitFrameFn, r.jitFramePC = fn, pc
	return ne
}

// jitRegCalleesKeptNoEntries has the functions e's code calls natively,
// compiled with entries for a frame that was running when they were
// (jitRegEntry's kept), compiled again without them: they are called again
// now, and Go will not see it, native code calling them. Not inside a
// compile of one of them.
func (r *Runtime) jitRegCalleesKeptNoEntries(e *jitRegEntry) {
	s := r.jit
	if e.life == nil || s.regKeptSeeding {
		return
	}
	s.regKeptSeeding = true
	defer func() { s.regKeptSeeding = false }()
	for _, x := range e.life.nativeCalls {
		if x.cl == nil || x.cl.reg == nil || x.cl.realm != r.Realm {
			continue
		}
		key := r.jitRegKeyOf(x.cl)
		if ce := s.regCache[key]; ce != nil && ce != e && ce.code != nil && len(ce.kept) != 0 && !ce.invalid && !ce.life.keptGone && !r.jitRegKeptCallsLeave(ce) {
			if ne := r.jitRegKeptGone(x.cl, x.cl.reg, ce); ne != nil {
				if cc := x.cl.closureCode; cc != nil && cc.jitReg == ce {
					cc.setJITReg(ne)
				}
			}
		}
	}
}

// jitRegContextFree reports whether a run Go begins now has a context to
// run in, the JIT's contexts grown if it needs more and may have them: past
// the last, the interpreter runs the frame, which is no run of the code's
// -- no entry counted, nothing judged, nothing to go on in at its next back
// edge.
func (r *Runtime) jitRegContextFree() bool {
	s := r.jit
	return s.ctxTop < len(s.ssaCtxs)-1 || r.jitGrowContexts()
}

// findReturns finds the returns of e's register code rc that a frame the
// code returned from goes on at in the interpreter (jitRegLoop): a
// ret_undef, and a ret whose register no closure captures, which the
// value is written to -- the frame ends there, and nothing reads it after.
// Each is -1 where the code has none. goesOn says the interpreter can end
// every frame the code returns from so: the code has such a ret where it
// has a ret at all.
func (e *jitRegEntry) findReturns(rc *bytecode.RegCode) {
	e.retPC, e.retUndef = -1, -1
	rets := false
	for pc, w := range rc.Code {
		switch w.Op() {
		case bytecode.RRetUndef:
			if e.retUndef < 0 {
				e.retUndef = pc
			}
		case bytecode.RRet:
			rets = true
			if a := int(w.A()); e.retPC < 0 && (a >= len(e.p.Captured) || !e.p.Captured[a]) {
				e.retPC, e.retReg = pc, a
			}
		}
	}
	e.goesOn = e.retPC >= 0 || !rets && e.retUndef >= 0
}

// jitRegCompileDueNow reports whether a call of Go that run's code made is
// to leave the code for it to be compiled again: due for what it learned
// (jitRegLearnedDueWith), and compiled again once the run has left -- at
// its exit, or its caller's after the unwinding (jitRegGoOnAt) -- not while
// a native call of it in an outer run, which its leaving does not end, runs
// it. Code that only native callers call, and that learned at its calls of
// Go -- a derived constructor's super()'s parent -- had otherwise never
// been compiled again: neither sampled nor entered by Go, nor leaving.
func (r *Runtime) jitRegCompileDueNow(run *jitRegRun, idx int) bool {
	e := run.e
	if e == nil || e.life == nil || !e.life.reopt {
		return false
	}
	limit, pending := idx, r.jitRegRunWork(run)
	if run.own < 0 {
		// A native callee's: the calling level, and those below it of the
		// call's, from the run's context on, which its leaving unwinds.
		limit, pending = run.root, 0
	}
	if j := r.jit.regPendingJobs[e.key]; j != nil && j.waiting && !e.invalid && e.code != nil && !r.jit.regCalledBelow(e, limit) {
		// Compiled again in the background, the install waiting for the
		// native calls of this code under way (jitRegInstallWaits), of
		// which none is left once this run leaves: it leaves for it, the
		// exit installing it (jitRegGoOnAt) as it compiles one due on the
		// spot. Its installs had been tried only at the checks of calls
		// it made, each under way inside it: AWFY's DeltaBlue's
		// constructors waited out tens of thousands of them, their
		// super() calls Go's meanwhile.
		return true
	}
	return r.jitRegLearnedDueWith(run.f.cl, e, limit, pending)
}
