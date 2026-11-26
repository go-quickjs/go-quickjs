//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64))

package vm

import (
	"math"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
	"github.com/go-quickjs/go-quickjs/internal/jit/ssa"
)

// Call frequency for register code, as V8's feedback has it, which V8's
// inliner orders its candidates by and cuts them at
// (ssa.FrequencyFeedback): how often a call site calls for each run of its
// function. The feedback dispatcher counts it
// (internal/vm/internal/nativegen, feedback.go), the dispatcher of a
// runtime that runs the JIT, into its function's closure's code's
// jitRegCounts -- slot 0 its runs, ended by a return the interpreter
// makes, slot 1+C the calls made at call site C, counted as the call
// instruction runs -- and nothing else counts: a runtime without the JIT
// runs the plain dispatcher, and native code counts neither its calls nor
// its returns, as V8's optimized code counts none. A run on-stack
// replacement takes into native code ends at a return of the
// interpreter's (jitRegLoop), and counts.

// jitCountsEnough is how many of a function's runs are counted before its
// frequencies are known (Frequency): the fewest in which a call made at
// V8's minimum frequency (ssa.MinInliningFrequency, 0.05) is made at least
// once 19 times in 20, 59 runs, so that a call counted none of the times is
// cold. A function compiled after fewer -- by on-stack replacement at a
// long loop, its first runs, or for a native caller -- has its calls
// ordered as before they were counted (ssa's inlineOrder). V8's optimizing
// compiler reads its feedback after its tier-up budget, rarely so few.
var jitCountsEnough = uint32(math.Ceil(math.Log(0.05) / math.Log(1-ssa.MinInliningFrequency)))

// newRegCallsCounted is newRegCalls with, where the runtime runs the JIT,
// the counts of cc's calls and runs (jitRegCountsMake): what a first call
// makes of its function's caches (regCallsMake) in one call, as a build
// without the JIT makes them.
//
//go:noinline
func (r *Runtime) newRegCallsCounted(cc *closureCode, n int) []regCallCache {
	calls := r.newRegCalls(cc, n)
	r.jitRegCountsMake(cc, n)
	return calls
}

// jitRegCountsMake makes cc's counts for its register code's calls sites
// sites calls, where the runtime runs the JIT: from its call sites' first
// caches on, both its calls and its runs.
func (r *Runtime) jitRegCountsMake(cc *closureCode, sites int) {
	if r.jitEnabled && cc.jitRegCounts == nil {
		cc.jitRegCounts = make([]uint64, 1+sites)
	}
}

// jitRegUncount takes back the call the feedback dispatcher counted at
// cc's call site site, where Go has the call handler make the call again
// rather than make it (nativeCallMiss): the dispatcher counts a call as
// it dispatches its instruction, and the instruction dispatched again
// counts it again. A count made before cc had its counts is none to take
// back.
func (cc *closureCode) jitRegUncount(site int) {
	if c := cc.jitRegCounts; 1+site < len(c) && c[1+site] != 0 {
		c[1+site]--
	}
}

// Frequency is ssa.FrequencyFeedback's: how often the call at pc, a call
// of the program's register code's, is made for each run of its function,
// from what the feedback dispatcher counted (jitRegCounts), once it has
// counted jitCountsEnough runs. Not a leaf's, whose calls the call path
// answers without a frame and so without its returns; nor a generator's or
// an async function's, whose runs end at suspensions as well; nor a
// construction's, which Go makes.
func (f *jitRegFeedback) Frequency(pc int) (float64, bool) {
	return f.native().frequency(pc)
}

// jitFramed reports whether the interpreter counts every call of x's
// callee it makes (jitRegCounts): a function of the script's, called with
// a frame of its own, through Function.prototype.call too, or a pure body
// or a forwarding constructor, which the call path answers without one
// where it can -- not another body the call path may answer without one
// (bytecode.LeafKind).
func jitFramed(x jitInline) bool {
	return x.cl != nil && jitCountedCallee(x.cl)
}

// jitCountedCallee reports whether the interpreter counts every call of
// cl's function it makes (jitFramed).
func jitCountedCallee(cl *closure) bool {
	switch leafKind(cl) {
	case bytecode.LeafNone, bytecode.LeafPure, bytecode.LeafForward:
		return true
	}
	return false
}

// nativeGrowUncount takes back the count of the call the frame stack had
// no room for: the feedback dispatcher counted it where it dispatched it,
// and counts it again as it dispatches it once the stack has grown, as
// nativeCallMiss's call made again is (jitRegUncount), so that the site's
// frequency, which the JIT inlines by, counts the call once; nothing where
// the plain dispatcher, which counts nothing, runs. Only a build with the
// JIT has it, and nativeDrive calls it before grow, the dispatcher's test
// here: called from grow, it made grow too large to inline there, and
// nativeDrive another function with the JIT built in than without it.
//
//go:noinline
func nativeGrowUncount(ctx *nativeCtx) {
	if len(ctx.recs) == 0 || ctx.disp != nativeFeedback {
		return
	}
	switch w := bytecode.RInstr(ctx.w); w.Op() {
	case bytecode.RCallW, bytecode.RCallMW, bytecode.RCallWT, bytecode.RCallMWT:
		if s := ctx.top; jitBuilt && s != nil && s.cl != nil {
			s.cl.closureCode.jitRegUncount(int(w.C()))
		}
	}
}
