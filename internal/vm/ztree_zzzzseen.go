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

var (
	// treeStringAdd marks the addition at pc in c's function as one that
	// met a string, where the JIT may compile it; nil without the JIT.
	treeStringAdd func(c *tctx, pc int)
)
