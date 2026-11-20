//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64))

package vm

import (
	"github.com/go-quickjs/go-quickjs/internal/jit/abi"
	jitcompile "github.com/go-quickjs/go-quickjs/internal/jit/compile"
)

// jitLearned is what a function's code has taught the JIT, which outlives
// its compiles, as opposed to what one compile made (jitEntry): the part of
// V8's feedback vector its bytecode's caches do not hold. Every code of the
// function shares it -- its code and its code for on-stack replacement --
// so that what one learns at its exits, the next compile of either is
// compiled for.
type jitLearned struct {
	// reg marks what a function's register code's native code learned
	// (jitState's regLearned), whose pcs are register pcs throughout;
	// regLink is the link native callers call its code through
	// (jitState's regLinks, regLinkOf), nil until a caller asks for one,
	// read without a key made.
	reg     bool
	regLink *jitLink
	// failed and failedEntries are the sites, by slot IR PC, and the
	// entries, by PC, whose speculation failed in the code (jitRegDeoptSeen).
	failed        []int32
	failedEntries []uint32
	// bigints are the sites, by slot IR PC, of arithmetic and comparisons
	// whose guard failed at BigInts in the int64 range: the code compiled
	// again makes them on those natively (jitRegBigIntSeen), until that
	// speculation fails, and the site is generic (failed). bigwide are
	// those of them whose guard failed at BigInts from 0 to 2^64-1, some
	// past the int64 range: made natively as those (ssa's BigIntWide).
	bigints, bigwide []int32
	// resumed are the PCs after calls Go has gone on at (jitRegGoOn), which
	// the next compile has entries at (ssa.ResumeFeedback).
	resumed []uint32
	// literals are the object literals the code makes natively, each with
	// its pool, kept across compiles (jitFeedback.Literal).
	literals []jitLiteral
	// regLitReads are, for register code, the caches of regexp_lit's
	// reads of its method, by register pc (jitRegFeedback's
	// regLitProperty).
	regLitReads map[int32]*propCache
	// cellsPool is the pool the code takes its captured locals' cells
	// from at its entries, kept across compiles (jitFeedback.CellsPool).
	cellsPool *abi.ObjectPool
	// poly are the property sites' cases and accessors the code met that
	// their caches cannot hold, and whether each is megamorphic, which it
	// is compiled again for (jitRegDeoptSeen, jitPolySeenOp).
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
	// regHints are, for register code, the hints its lowerings are made
	// with, by register pc (jitRegLower): the additions that met strings,
	// and the element reads made as numbers that met what is none
	// (jitRegElemSeen, jitRegStringAddSeen).
	regHints jitcompile.Hints
	// genSteps are the loops' steps that have met generators the code may
	// resume natively (ssa.GenStepFeedback), with the generators' closures,
	// which keep alive what the code compares their closures with.
	genSteps []jitGenStep
	// stepResults are, for register code, the loops' steps that have met
	// an iterator written in script, whose next the code calls natively
	// (its nativeCalls at the step, jitRegStepNextSeen), with the layout
	// its results were of, which the code reads natively
	// (ssa.CallSite's StepResult).
	stepResults []jitStepResult
	// genResults are the pools of the calls of next the code makes
	// natively (jitGenResultPool); genMakes the calls of generator
	// functions whose generators it makes natively (jitRegGenMakeSeen).
	genResults []jitGenResult
	genMakes   []jitGenMake
	// creates are the calls of Object.create whose objects the code makes
	// natively, with their pools (jitCreatePool).
	creates []jitCreate
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
	// each with the function it was seen to call (jitRegCallSeen); notInline
	// are those that stopped being: what the callee does kept leaving
	// native code.
	inlines   []jitInline
	notInline []int32
	// callSites is what jitRegCallSettling knows of each call, by PC: how
	// often it
	// has left native code, up to jitCallsToInline, then jitCallDone or
	// jitCallInlined; nil until a call does.
	callSites []uint8
	// settling are the functions each call met at its exits before it was
	// decided (callSites' count), by PC, one of each code: as V8's call
	// feedback holds every target a call met before its function is
	// optimized, one that met more than one function is polymorphic, made
	// natively to each, not inlined for the one it met last (jitRegCallSeen).
	settling []jitInline
}

// A function's code has no entry at a loop where escape analysis carries
// an object round it unmade (ssa.Func's Unentered): the entry would have
// the loop's objects made every turn, on every call, for a frame that
// needs it once. So a frame that does -- one found in the loop by two
// checks in a row (jitRegLoopCode) -- goes on in code compiled for it
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
// which goes with it (dropReg). Its link is its own, never in the
// runtime's: nothing calls through it, so that compiling it changes what
// no native caller calls.

// jitKeptStarts is how many times Go enters a function's code by its start
// before it gives up the entries kept for a frame (jitEntry's ssaKept): a
// function called at all more than that is one its objects' staying unmade
// pays in -- unless three of those calls in four left the code: an object
// left unmade for them is made at their exit, and they would ask for those
// entries back, in code of their own (jitRegLoopCode): a compile to give
// them up and one to have them again, for code that gains nothing.
// carried_exit_3000's 254 calls took 4% longer for those two compiles. One
// call would not do: where each call meets something new -- a loop reading a
// new shape at each -- each would compile twice.
const jitKeptStarts = 4

// jitFramesLeaving is how many calls of a function go on in the code kept
// for its loops before it is asked whether most of its calls do
// (jitRegFramesLeave): past a compile's worth of what leaving costs.
const jitFramesLeaving = 256
