package vm

import "sync/atomic"

// What the tree tier sees that the JIT compiles for.
//
// A function the JIT compiles has, as a rule, run in the tree tier first,
// and had its operations' slow paths taken there: an addition that met a
// string, which native code compiled for numbers would leave at. The JIT
// compiled such a function blind, its code deoptimizing at each such
// addition the first time it ran, and compiled again for it, a site at a
// time -- 21 of PDF.js's 24 additions that deoptimized had added strings
// in the tree tier first. As V8's lower tier collects the feedback its
// optimizing compiler reads, the tree tier's slow path marks the site, in
// the function's tree (treeSeen), and the JIT lowers it as an addition of
// strings from its first compile (jitLower).
//
// The tree is the function's own, which every runtime running it shares,
// as its bytecode is: the marks are atomic, and say only what some runtime
// saw, which steers what native code speculates, never what it answers.
//
// It is a variable, which the JIT's build assigns a closure
// (zzjit_treeseen.go), laid out after every closure of the tree tier's
// (see AGENTS.md); its call, in arithSlow, is the JIT's build's alone
// (jitBuilt).

// treeSeen is the sites of a function's tree whose slow paths took what
// native code would not: adds marks additions that met a string, a bit a
// bytecode PC, and n counts the marks, so that a lowering knows when to
// look again.
type treeSeen struct {
	adds []atomic.Uint32
	n    atomic.Int32
}

// treeBigSeen is the sites of a function's tree whose arithmetic,
// bitwise operators or comparisons met two BigInts, a bit a bytecode PC:
// bigs those of BigInts in the int64 range, whose result was one too,
// which native code makes unboxed (ssa.BigIntFeedback); wide those of
// BigInts from 0 to 2^64-1, some past the int64 range, and a result in
// that range too, which it makes unboxed as Uint64s (ssa's BigIntWide);
// and heap the others, a BigInt on the heap met or made, or one below
// -2^63, which Go makes from native code (ssa.GoArith); and recvs the
// method reads whose receivers were BigInts, which native code reads off
// BigInt.prototype (jitFeedback's StringProperty).
type treeBigSeen struct {
	bigs, wide, heap, recvs []atomic.Uint32
}

var (
	// treeStringAdd marks the addition at pc in c's function as one that
	// met a string, where the JIT may compile it; nil without the JIT.
	treeStringAdd func(c *tctx, pc int)

	// treeBigInt marks the arithmetic at pc in c's function as one that
	// met two BigInts, one on the heap or made there if heap (treeBigSeen),
	// where the JIT may compile it; nil without the JIT.
	treeBigInt func(c *tctx, pc int, heap bool)

	// treeBigOf marks the operator at pc in c's function, of a and b, two
	// BigInts held in their values, whose result was v, as treeBigInt does:
	// as one in the int64 range, one from 0 to 2^64-1, or one on the heap,
	// as the three were (treeBigSeen); nil without the JIT.
	treeBigOf func(c *tctx, pc int, a, b, v Value)

	// treeBigRecv marks the method read at pc in c's function as one of a
	// BigInt's (treeBigSeen's recvs); nil without the JIT.
	treeBigRecv func(c *tctx, pc int)
)
