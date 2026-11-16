package vm

import "sync/atomic"

// What the interpreter sees of BigInts that the JIT compiles for: the
// slow paths mark, in the function's register code (regCode's bigSeen),
// the sites whose operands were BigInts, and the JIT unboxes those it
// marks small (regBigMark).
//
// The marks are the function's code's, which every runtime running it
// shares: they are atomic, and say only what some runtime saw, which
// steers what native code speculates, never what it answers.

// bigSeenMarks is the sites of a function's register code whose
// arithmetic, bitwise operators or comparisons met two BigInts, a bit a
// register pc: bigs those of BigInts in the int64 range, whose result was
// one too, which native code makes unboxed (ssa.BigIntFeedback); wide
// those of BigInts from 0 to 2^64-1, some past the int64 range, and a
// result in that range too, which it makes unboxed as Uint64s (ssa's
// BigIntWide); and heap the others, a BigInt on the heap met or made, or
// one below -2^63, which Go makes from native code (ssa.GoArith); and
// recvs the method reads whose receivers were BigInts, which native code
// reads off BigInt.prototype (jitFeedback's StringProperty).
type bigSeenMarks struct {
	bigs, wide, heap, recvs []atomic.Uint32
}
