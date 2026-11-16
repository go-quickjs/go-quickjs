package nativegen

import (
	"github.com/go-quickjs/go-quickjs/internal/bytecode"
)

// The feedback dispatcher's own handlers (the feedback variant): what a
// runtime that runs the JIT records as its register code runs, for the
// JIT's call frequencies (ssa.FrequencyFeedback), as V8's interpreter
// counts in a function's feedback vector the calls made at each call
// bytecode and the function's invocations. Each does its count and goes on
// in plain's handler of its operation (variant's own, slotPlain), so that
// plain's handlers, which a runtime without the JIT runs, have no part of
// it.
//
// The counts are the function's closure's code's jitRegCounts (package
// vm, zzjit_zregfreq.go), which the top frame's cc reaches: slot 0 its
// runs, counted at each return its frame makes -- the runs that end so,
// a return being where a run of the interpreter's ends whoever began it,
// Go or a native call; slot 1+C the calls made at call site C, counted at
// each call instruction the handlers run, whatever it calls. Where the
// slice is shorter -- nil before the function's call sites have their
// caches, or a runtime that has none -- nothing is counted.

// feedbackOps are the feedback variant's own handlers: the calls --
// call_w, call_method_w and those in tail position -- and the returns, in
// handlerOps' order.
var feedbackOps = []bytecode.ROp{
	bytecode.RCallW, bytecode.RCallMW, bytecode.RRet, bytecode.RRetUndef,
	bytecode.RCallWT, bytecode.RCallMWT,
}

// feedbackCountsReturn reports whether op's handler counts a run of its
// frame's function, not a call at its site.
func feedbackCountsReturn(op bytecode.ROp) bool {
	return op == bytecode.RRet || op == bytecode.RRetUndef
}

// feedbackCounts is the offset of the counts' slice from the context's
// cc (ccRel): a field of the JIT's, before the fields plain's handlers
// read.
func feedbackCounts() int32 {
	return ccRel(off("closureCode.jitRegCounts"))
}

// feedbackBlock is a variant's block of the handlers emit laid out for v's
// overrides in handlerOps' order, each starting at its label.
func feedbackBlock(v *variant, code []byte, at map[bytecode.ROp]int) *block {
	b := &block{at: at}
	for _, op := range handlerOps {
		if _, ok := at[op]; ok && v.has(op) {
			b.order = append(b.order, op)
		}
	}
	b.code = code
	return b
}
