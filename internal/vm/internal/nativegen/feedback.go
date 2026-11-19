package nativegen

import (
	"slices"

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
// A return also counts down the runs left of its function's budget
// (feedbackTick), and a backward jump the work left of it for back edges
// (feedbackEdges), as V8's JumpLoop spends its function's budget.
//
// The counts are the function's closure's code's jitRegCounts (package
// vm, jit_freq.go), which the top frame's cc reaches: slot 0 its
// runs, counted at each return its frame makes -- the runs that end so,
// a return being where a run of the interpreter's ends whoever began it,
// Go or a native call; slot 1+C the calls made at call site C, counted at
// each call instruction the handlers run, whatever it calls. Where the
// slice is shorter -- nil before the function's call sites have their
// caches, or a runtime that has none -- nothing is counted.

// feedbackOps are the feedback variant's own handlers: the calls --
// call_w, call_method_w and those in tail position -- and the returns;
// and the operators plain's handlers make of two BigInts held in their
// Values (feedbackBigOps), in handlerOps' order; the jumps; and the fused
// operations whose partner is a jump (feedbackUnfused).
var feedbackOps = append(append([]bytecode.ROp{
	bytecode.RAdd, bytecode.RSub, bytecode.RMul,
	bytecode.RJLt, bytecode.RJNLt, bytecode.RJNGt, bytecode.RJGe, bytecode.RJNGe,
	bytecode.RCallW, bytecode.RCallMW, bytecode.RRet, bytecode.RRetUndef,
	bytecode.RJLe, bytecode.RJNLe, bytecode.RJGt,
	bytecode.RCallWT, bytecode.RCallMWT,
}, feedbackOtherJumps...), bytecode.RInstanceOfNext)

// feedbackUnfused is, of each fused operation whose partner is a jump that
// plain's handler takes itself, the operation unfused: the feedback
// variant's handler of it is plain's handler of that, through the table,
// which leaves the partner to run as its own instruction and count its
// back edge (feedbackBCJumps). instanceof_next's plain handler branches
// on its partner's offset where the partner would, which counts nothing,
// so a loop whose condition is an instanceof never spent its function's
// budget.
var feedbackUnfused = map[bytecode.ROp]bytecode.ROp{
	bytecode.RInstanceOfNext: bytecode.RInstanceOf,
}

// The back edges. Each of feedbackBCJumps and feedbackSCJumps -- every
// operation that jumps, by int32(BC) or by SC -- counts a backward jump's
// distance of the work its function's budget has left for back edges
// (package vm's closureCode's jitRegEdges), before plain's handler makes
// it, taken or not: the count is the function's, which a loop's turns
// spend, and its end -- the work left none -- ends the interrupt budget
// (R11 on amd64), so that plain's handler, the jump taken, leaves it to
// Go's check (regBackEdge), which spends the work counted toward the
// function's compile (jitRegLoop) and has it counted again. V8's
// interpreter spends a function's budget so at each JumpLoop; the
// interrupt budget alone, which ends wherever a program's loops have run
// it out, had a loop's work sampled.
var (
	feedbackBCJumps = []bytecode.ROp{bytecode.RJmp, bytecode.RJmpF, bytecode.RJmpT}
	feedbackSCJumps = []bytecode.ROp{
		bytecode.RJLt, bytecode.RJNLt, bytecode.RJLe, bytecode.RJNLe,
		bytecode.RJGt, bytecode.RJNGt, bytecode.RJGe, bytecode.RJNGe,
		bytecode.RJEq, bytecode.RJNe,
		bytecode.RJLtI, bytecode.RJNLtI, bytecode.RJLeI, bytecode.RJNLeI,
		bytecode.RJGtI, bytecode.RJNGtI, bytecode.RJGeI, bytecode.RJNGeI,
		bytecode.RJEqI, bytecode.RJNeI,
		bytecode.RJLtU, bytecode.RJNLtU, bytecode.RJLeU, bytecode.RJNLeU,
		bytecode.RJNull, bytecode.RJNotNull,
		bytecode.RJSEq, bytecode.RJSNe, bytecode.RJSEqI, bytecode.RJSNeI,
		bytecode.RJTypeof,
		// The step-and-branches jump by SC as their partners do, from
		// the step's own handler where the partner's comparison holds:
		// their backward jumps are the loops' back edges as the partners'
		// are where they are not fused.
		bytecode.RStepJLt, bytecode.RStepJLe, bytecode.RStepJLtI, bytecode.RStepJGtI,
		bytecode.RStepJGeI, bytecode.RStepJLeU,
	}
	// feedbackOtherJumps are the jumps feedbackOps has for nothing else.
	feedbackOtherJumps = func() []bytecode.ROp {
		var ops []bytecode.ROp
		for _, op := range append(slices.Clone(feedbackBCJumps), feedbackSCJumps...) {
			if !slices.Contains(feedbackBigOps, op) {
				ops = append(ops, op)
			}
		}
		return ops
	}()
)

// feedbackJumps reports whether op's handler counts a back edge, and
// whether its offset is int32(BC) rather than SC.
func feedbackJumps(op bytecode.ROp) (jumps, bc bool) {
	if slices.Contains(feedbackBCJumps, op) {
		return true, true
	}
	return slices.Contains(feedbackSCJumps, op), false
}

// feedbackEdges is the offset of the work left of the function's budget
// for back edges from the context's cc (closureCode's jitRegEdges), which
// a backward jump counts down.
func feedbackEdges() int32 {
	return ccRel(off("closureCode.jitRegEdges"))
}

// The BigInt marks. plain's handlers add, subtract, multiply and compare
// two BigInts held in their Values (bigArith, bigCmp) without the
// interpreter's Go, whose slow path marks the operator's register pc as
// one that met BigInts (package vm's regBigOf, regCode's bigSeen), which
// the JIT's first compile reads (regBigSeen): unmarked, a site met BigInts
// only at its first exit from native code -- bigint_pidigits' code
// compiled for numbers, deoptimized and compiled again a site at a time.
// The feedback variant's handler of such an operator, before plain's,
// looks at its two operands: where both are BigInts held in their Values
// and the site has no mark of any kind yet, it leaves the instruction to
// Go (the exit stub), whose handler makes the operation and the mark it
// sees, by its operands and its result, as for any BigInt; every other
// time it goes on in plain's handler. A site is left to Go once, plain's
// handlers, which a runtime without the JIT runs, doing no part of it.

// feedbackBigOps are the feedback variant's own handlers that mark
// BigInts: of R[B] and R[C], the arithmetic, and of R[A] and R[B], the
// compare-and-branches of two registers.
var feedbackBigOps = []bytecode.ROp{
	bytecode.RAdd, bytecode.RSub, bytecode.RMul,
	bytecode.RJLt, bytecode.RJNLt, bytecode.RJNGt, bytecode.RJGe, bytecode.RJNGe,
	bytecode.RJLe, bytecode.RJNLe, bytecode.RJGt,
}

// feedbackMarksBig reports whether op's handler marks BigInts, and
// whether its operands are A and B rather than B and C.
func feedbackMarksBig(op bytecode.ROp) (marks, ab bool) {
	if !slices.Contains(feedbackBigOps, op) {
		return false, false
	}
	return true, op != bytecode.RAdd && op != bytecode.RSub && op != bytecode.RMul
}

// feedbackBigSeen is the offset of the marks' pointer in the context's
// register code (regCode's bigSeen, an atomic.Pointer whose word is at
// its start), and feedbackBigKinds those of the three kinds' slices'
// data in the marks (bigSeenMarks's bigs, wide and heap), each a word of
// 32 sites' bits.
func feedbackBigSeen() int32 {
	return off("regCode.bigSeen")
}

func feedbackBigKinds() [3]int32 {
	return [3]int32{off("bigSeenMarks.bigs"), off("bigSeenMarks.wide"), off("bigSeenMarks.heap")}
}

// feedbackCountsReturn reports whether op's handler counts a run of its
// frame's function, not a call at its site.
func feedbackCountsReturn(op bytecode.ROp) bool {
	return op == bytecode.RRet || op == bytecode.RRetUndef
}

// feedbackTick is the offset of the runs left of the function's budget
// from the context's cc (closureCode's jitRegTick), which a return counts
// down, and feedbackEnters its jitEnters, which a return sets once none
// is left, so that the function's next call goes to the JIT's tier-up
// (package vm's jitRegCallTier).
func feedbackTick() int32 {
	return ccRel(off("closureCode.jitRegTick"))
}

func feedbackEnters() int32 {
	return ccRel(off("closureCode.jitEnters"))
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
