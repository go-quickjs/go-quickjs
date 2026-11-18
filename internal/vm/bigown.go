package vm

import (
	"math/bits"
)

// A BigInt a local holds alone, given back when the local is assigned the
// next: `sum = sum + a * a` and `a = a + 1n` in a loop make a result each
// turn and leave the one before to the collector, which C QuickJS frees as
// it goes, its count of references at zero. The interpreter has no such
// count; it asks instead of the function's code which locals' values go
// nowhere but into operators (bytecode.SoleLocals): a local's value read
// only as an operand of arithmetic, a bitwise operator or a comparison,
// whose result is a value of its own, is never seen by anything but the
// local. A BigInt an operator makes and a store to such a local keeps is
// marked so (bigSoleBit, bytecode.RArithOwnA), and when the local is
// assigned the result of an operator of two BigInts, the value it held, so
// marked, is given back once the operator has made the next (regBigRelease).
//
// The question is asked of the bytecode, as a stack machine runs it, over
// the instructions it knows: a function with any other has no such local.
// A value a local pushes is followed to the instruction that takes it: an
// operator's operand, a test, a drop, or anything else, which it escapes
// to -- a store of the value itself to a local or a global, a call, a
// property's read on it, a return, a jump past which it is still on the
// stack.

// bigSoleBit marks, on a 64-bit machine, a BigInt on the heap that one local
// holds and nothing else has seen: one of bytecode.SoleLocals', stored there
// as an operator's result.
const bigSoleBit = uint(1) << (bits.UintSize - 2)
