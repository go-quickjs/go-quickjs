//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64))

package vm

import (
	"github.com/go-quickjs/go-quickjs/internal/bytecode"
	"github.com/go-quickjs/go-quickjs/internal/jit/ir"
)

// Call frequency for register code, as V8's feedback has it and the stack
// form's tiers count it (zzjit_frequency.go): how often a call site calls
// for each run of its function. The feedback dispatcher counts it
// (internal/vm/internal/nativegen, feedback.go), the dispatcher of a
// runtime that runs the JIT, into its function's closure's code's
// jitRegCounts -- slot 0 its runs, ended by a return the interpreter
// makes, slot 1+C the calls made at call site C, counted as the call
// instruction runs -- and nothing else counts: a runtime without the JIT
// runs the plain dispatcher, and native code counts neither its calls nor
// its returns, as V8's optimized code counts none. A run on-stack
// replacement takes into native code ends at a return of the
// interpreter's (jitRegLoop), and counts.

// jitRegCountsMake makes cc's counts for its register code's calls sites
// sites calls, where the runtime runs the JIT: from its call sites' first
// caches on, both its calls and its runs.
func (r *Runtime) jitRegCountsMake(cc *closureCode, sites int) {
	if r.jitEnabled && cc.jitRegCounts == nil {
		cc.jitRegCounts = make([]uint64, 1+sites)
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
	fn, cl := f.fb.fn, f.fb.cl
	if fn.Leaf != bytecode.LeafNone || fn.Generator || fn.Async || cl == nil || cl.closureCode == nil {
		return 0, false
	}
	w, op, ok := f.at(pc)
	if !ok || op != ir.Host || !f.p.Code[pc].Window {
		return 0, false
	}
	switch w.Op() {
	case bytecode.RCallW, bytecode.RCallMW, bytecode.RCallWT, bytecode.RCallMWT:
	default:
		return 0, false
	}
	counts := cl.jitRegCounts
	if len(counts) == 0 || counts[0] < uint64(jitCountsEnough) || 1+int(w.C()) >= len(counts) {
		return 0, false
	}
	return float64(counts[1+int(w.C())]) / float64(counts[0]), true
}
