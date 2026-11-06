//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))

package vm

import (
	"slices"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
	"github.com/go-quickjs/go-quickjs/internal/jit/abi"
)

// jitLearned is what a function's code has taught the JIT, which outlives
// its compiles, as opposed to what one compile made (jitEntry): the part of
// V8's feedback vector its bytecode's caches do not hold. Every code of the
// function shares it -- its code and its code for on-stack replacement --
// so that what one learns at its exits, the next compile of either is
// compiled for.
type jitLearned struct {
	// failed and failedEntries are the sites, by slot IR PC, and the
	// entries, by PC, whose speculation failed in the code (jitDeoptimized).
	failed        []int32
	failedEntries []uint32
	// bigints are the sites, by slot IR PC, of arithmetic and comparisons
	// whose guard failed at BigInts in the int64 range: the code compiled
	// again makes them on those natively (jitBigIntSeen), until that
	// speculation fails, and the site is generic (failed). bigwide are
	// those of them whose guard failed at BigInts from 0 to 2^64-1, some
	// past the int64 range: made natively as those (ssa's BigIntWide).
	bigints, bigwide []int32
	// resumed are the PCs after calls Go has gone on at (jitGoOn), which
	// the next compile has entries at (ssa.ResumeFeedback).
	resumed []uint32
	// literals are the object literals the code makes natively, each with
	// its pool, kept across compiles (jitFeedback.Literal).
	literals []jitLiteral
	// regexpLitCalls says, of each call of Go the code has made that
	// jitRegExpLitPool has asked about, the pc of the RegExp literal it
	// calls exec or test of, or -1.
	regexpLitCalls map[int32]int32
	// cellsPool is the pool the code takes its captured locals' cells
	// from at its entries, kept across compiles (jitFeedback.CellsPool).
	cellsPool *abi.ObjectPool
	// poly are the property sites' cases and accessors the code met that
	// their caches cannot hold, and whether each is megamorphic, which it
	// is compiled again for (jitDeoptSeen, jitPolySeen).
	poly []jitPolySite
	// typedReads are the element reads, by PC, whose result no native
	// operation takes as a number, whose receivers have been typed arrays
	// (ssa.ElementFeedback), stringReads those whose receivers have been
	// strings (ssa.StringElementFeedback), and keyedReads those Go makes,
	// whose receivers have been objects read by a name (ssa.KeyedFeedback);
	// keyedWrites are the element writes Go makes, whose receivers have
	// been objects native code does not store to (ssa.KeyedWriteFeedback).
	typedReads  []int32
	stringReads []int32
	keyedReads  []int32
	keyedWrites []int32
	// genSteps are the loops' steps that have met generators the code may
	// resume natively (ssa.GenStepFeedback), with the generators' closures,
	// which keep alive what the code compares their closures with.
	genSteps []jitGenStep
	// genResults are the pools of the calls of next the code makes
	// natively (jitGenResultPool); genMakes the calls of generator
	// functions whose generators it makes natively (jitGenMakeSeen).
	genResults []jitGenResult
	genMakes   []jitGenMake
	// strSites are the method reads that met a string receiver, which the
	// code reads off String.prototype once compiled again
	// (jitFeedback.StringProperty).
	strSites []uint32
	// bigSites are those that met a BigInt receiver, which the code reads
	// off BigInt.prototype (jitFeedback's StringProperty); numSites and
	// boolSites those that met a number or a boolean, off Number.prototype
	// or Boolean.prototype, which had the method (jitPrimitiveSeen).
	bigSites  []uint32
	numSites  []uint32
	boolSites []uint32
	// nativeCalls are the calls the code makes natively, or will when
	// compiled again, each with the function it was seen to call.
	nativeCalls []jitInline
	// inlines are the calls the code inlines, or will when compiled again,
	// each with the function it was seen to call (jitCallSeen); notInline
	// are those that stopped being: what the callee does kept leaving
	// native code.
	inlines   []jitInline
	notInline []int32
	// callSites is what jitCallSeen knows of each call, by PC: how often it
	// has left native code, up to jitCallsToInline, then jitCallDone or
	// jitCallInlined; nil until a call does.
	callSites []uint8
	// settling are the functions each call met at its exits before it was
	// decided (callSites' count), by PC, one of each code: as V8's call
	// feedback holds every target a call met before its function is
	// optimized, one that met more than one function is polymorphic, made
	// natively to each, not inlined for the one it met last (jitCallSeen).
	settling []jitInline
	// calls are what the existing tiers counted of the function's calls,
	// and of its invocations, since it was first given an entry
	// (jitCallCounts): its code, and the entries made for it after this
	// one, keep them (jitCompileFor, jitGhost); nil until it is invoked
	// with this entry.
	calls *jitCallCounts
}

// A function's code has no entry at a loop where escape analysis carries
// an object round it unmade (ssa.Func's Unentered): the entry would have
// the loop's objects made every turn, on every call, for a frame that
// needs it once. So a frame that does -- one found in the loop by two
// checks in a row (jitLearnLoopEntry) -- goes on in code compiled for it
// and kept apart, as V8 keeps code for on-stack replacement: the same
// function, with the loop's entry, that Go enters only at such a loop.
// Native callers go on calling the function's code, and later invocations
// enter it, its objects unmade.
//
// That code is an entry of its own, osr on the function's, so that all a
// run of code goes by -- what it holds alive, what it was compiled for, its
// stats, whether it is valid, whether it pays -- is that code's, and a run
// of it is a run like any other. It shares what the function learned
// (jitLearned), and is compiled again as any code is, at the budgets any
// code is. It is not in the cache: no closure's hint names it, and its
// bytes count in the function's entry's (measure), whose room it takes,
// which goes with it (dropEntry). Its link is its own, never in the
// runtime's: nothing calls through it, so that compiling it changes what
// no native caller calls.

// jitKeptStarts is how many times Go enters a function's code by its start
// before it gives up the entries kept for a frame (jitEntry's ssaKept): a
// function called at all more than that is one its objects' staying unmade
// pays in -- unless three of those calls in four left the code: an object
// left unmade for them is made at their exit, and they would ask for those
// entries back, in code of their own (jitLearnLoopEntry): a compile to give
// them up and one to have them again, for code that gains nothing.
// carried_exit_3000's 254 calls took 4% longer for those two compiles. One
// call would not do: where each call meets something new -- a loop reading a
// new shape at each -- each would compile twice.
const jitKeptStarts = 4

// setHomes has e's code's homes be homes (jitEntry's ssaHomes), once no run
// of the code it replaced can exit: until then, the more of the two, and
// the run's end brings them down (jitRunDone). Left at the most any compile
// had, one compile with objects unmade had every exit of the code after it,
// which has none, look through the frame for their words.
func (e *jitEntry) setHomes(s *jitState, homes int) {
	e.ssaHomesNow = homes
	if e.ssaRuns == 0 && !s.called(e) {
		e.ssaHomes = homes
	} else {
		e.ssaHomes = max(e.ssaHomes, homes)
	}
}

// jitFramesLeaving is how many calls of a function go on in the code kept
// for its loops before it is asked whether most of its calls do
// (jitFramesLeave): past a compile's worth of what leaving costs.
const jitFramesLeaving = 256

// keptCallsLeave reports whether three in four of e's calls since it last
// looked left its code (jitKeptStarts), and enough of them to tell: one call
// that left is no pattern.
func (e *jitEntry) keptCallsLeave() bool {
	calls := e.starts - e.keptAt
	if e.link != nil {
		calls += e.link.in
	}
	return e.keptLeft >= jitKeptStarts*3/4 && e.keptLeft*4 >= calls*3
}

// jitFrameAt is the loop's header a frame waits at for this compile of fn,
// e's code (ssa.Func's FrameAt), or 0: there is none, or the compile is of
// another function, one a run of the frame's got to, or native callers
// call e's code, whose calls would then make objects it could leave
// unmade, with no check of Go's to find out that they do.
func (r *Runtime) jitFrameAt(fn *bytecode.Function, e *jitEntry) int {
	if r.jitFrameFn != fn || e == nil || e.link != nil && e.link.refs != 0 {
		return 0
	}
	return r.jitFramePC
}

// jitCalleesKeptNoEntries has the functions e's code calls natively,
// compiled with entries for a frame that was running when they were
// (jitEntry's ssaKept), compiled again without them: they are called again
// now, and Go will not see it, native code calling them.
func (r *Runtime) jitCalleesKeptNoEntries(e *jitEntry) {
	for _, x := range e.nativeCalls {
		if x.cl == nil {
			continue
		}
		if ce := r.jit.entryFor(x.cl.fn); ce != nil && ce != e && len(ce.ssaKept) != 0 && !ce.invalid && !ce.keptCallsLeave() {
			r.jitKeptGone(x.cl, ce)
		}
	}
}

// jitKeptGone compiles e's code again, cl's function's, without the
// entries it has for a frame (jitEntry's ssaKept).
func (r *Runtime) jitKeptGone(cl *closure, e *jitEntry) {
	e.keptGone = true
	r.jitReoptimize(cl, e)
}

// jitFramesLeave counts a call of e's function going on in the code kept for
// the loop at pc -- once each: where it leaves again, it goes on in that
// run -- and reports whether e's code has been compiled with that loop's
// entry because most calls do. Objects left unmade for a call that
// leaves are made at its exit, and it enters code again for the rest of its
// loop: each call pays that, for the few turns before it left. So, as three
// calls in four leave, the entry is learned as one Go goes on at (jitLearned's
// resumed), the code keeps its objects made there, as if it had never left
// them unmade, and the code kept goes. carried_exit_100, each call leaving at
// its sixth turn, had been 10% slower than code that never tried; in
// carried_exit_mixed, where one call in two runs its loop through, those
// calls go on gaining more than the others cost.
func (r *Runtime) jitFramesLeave(cl *closure, e *jitEntry, pc int) bool {
	if e.osrFrames++; e.osrFrames < jitFramesLeaving {
		return false
	}
	calls := e.starts + e.link.in
	// Compiling the code counts its calls afresh: then, the count begins.
	most := calls >= e.osrCalls && (calls-e.osrCalls)*3 <= jitFramesLeaving*4
	e.osrFrames, e.osrCalls = 0, calls
	if !most || e.invalid || slices.Contains(e.resumed, uint32(pc)) {
		return false
	}
	e.resumed = append(e.resumed, uint32(pc))
	e.resumeReopt = true
	r.jit.account(e)
	r.jitReoptimize(cl, e)
	return e.ssa != nil && e.ssa.HasEntry(pc)
}

// osrEntry is e's entry for on-stack replacement, made if need be.
func (s *jitState) osrEntry(e *jitEntry) *jitEntry {
	if e.osr != nil {
		return e.osr
	}
	link := &jitLink{key: e.link.key, params: e.link.params, locals: e.link.locals, frame: e.link.frame, flags: e.link.flags}
	oe := &jitEntry{jitLearned: e.jitLearned, fnAddr: e.fnAddr, link: link, osrOf: e, ssaProg: e.ssaProg, ssaLoop: e.ssaLoop,
		ssaSerial: e.ssaSerial, ssaCallee: e.ssaCallee, this: e.this, hot: e.hot, invoked: e.invoked, used: e.used, serial: e.serial,
		reopts: e.reopts, polyReopts: e.polyReopts, inlineReopts: e.inlineReopts, upgradeReopts: e.upgradeReopts, unwindReopts: e.unwindReopts}
	link.owner = oe
	e.osr = oe
	return oe
}

// osrInUse reports whether what e's entry for on-stack replacement holds
// may not go: its code runs, or it is being compiled.
func (e *jitEntry) osrInUse() bool {
	oe := e.osr
	return oe != nil && (oe.ssaRuns != 0 || oe.busy != 0 || oe.pending != nil)
}

// osrBytes is what e's entry for on-stack replacement holds, which its
// function's entry counts: what the function learned, they share.
func (e *jitEntry) osrBytes() int {
	oe := e.osr
	if oe == nil {
		return 0
	}
	return jitEntryFixed + oe.ssa.Footprint() + oe.compiledBytes() + jitCapBytes(oe.osrLoops)
}

// dropOSR closes the code of e's entry for on-stack replacement, which no
// run may then go on in or compile for, reporting false if the OS would
// not release it.
func (s *jitState) dropOSR(e *jitEntry) bool {
	oe := e.osr
	oe.link.owner = nil
	oe.link.clearCode()
	if s.closeSSA(oe.ssa, oe) != nil {
		oe.link.owner = oe
		return false
	}
	// As for its function's code (dropEntry).
	for _, l := range oe.ssaLinks {
		if ce := l.owner; ce != nil {
			ce.nativeCallers = slices.DeleteFunc(ce.nativeCallers, func(c *jitEntry) bool { return c == oe })
		}
	}
	s.setLinks(oe, nil)
	e.osr, oe.osrOf = nil, nil
	return true
}

// osrReplaced has e's entry for on-stack replacement go, once no run is in
// it, e's code compiled again: frames in the function's loops from now on
// are the new code's to learn of, and what it was compiled for is out of
// date. Kept, it would hold a compile's bytes for frames that may never
// come back, as V8's cache of such code is emptied once code deoptimizes.
func (s *jitState) osrReplaced(e *jitEntry) {
	if e.osr == nil {
		return
	}
	if e.osrInUse() {
		e.osr.osrSpent = true
		return
	}
	s.dropOSR(e)
}

// osrTell tells the function's entry what oe, its entry for on-stack
// replacement, is to be compiled again for: what a code learns only its
// own exits tell it, and the learned being shared, the function's code,
// not compiled for it, would find a site noted before and never ask
// (jitDeoptimized, jitDeoptSeen, jitGoOn).
func (oe *jitEntry) osrTell() {
	m := oe.osrOf
	if m == nil {
		return
	}
	m.reopt = m.reopt || oe.reopt
	m.polyReopt = m.polyReopt || oe.polyReopt
	m.inlineReopt = m.inlineReopt || oe.inlineReopt
	m.inlinePending = m.inlinePending || oe.inlinePending
	m.upgradeReopt = m.upgradeReopt || oe.upgradeReopt
	m.unwindReopt = m.unwindReopt || oe.unwindReopt
	m.resumeReopt = m.resumeReopt || oe.resumeReopt
}

// osrRunDone ends a run of oe, an entry for on-stack replacement. What it
// is to be compiled for, its function's code is told, and oe no longer:
// that compile has oe go (osrReplaced), and a frame that comes back learns
// the loop for code compiled with all of it.
func (s *jitState) osrRunDone(oe *jitEntry) {
	oe.osrTell()
	oe.reopt, oe.polyReopt, oe.inlineReopt, oe.inlinePending, oe.upgradeReopt, oe.unwindReopt, oe.resumeReopt = false, false, false, false, false, false, false
	if m := oe.osrOf; m != nil && oe.osrSpent && !m.osrInUse() {
		s.dropOSR(m)
	}
}
