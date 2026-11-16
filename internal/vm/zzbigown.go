package vm

import (
	"math/bits"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
)

// A BigInt a local holds alone, given back when the local is assigned the
// next: `sum = sum + a * a` and `a = a + 1n` in a loop make a result each
// turn and leave the one before to the collector, which C QuickJS frees
// as it goes, its count of references at zero. The tree tier has no such
// count; it asks instead of the function's code which locals' values go
// nowhere but into operators (soleLocals): a local's value read only as an
// operand of arithmetic, a bitwise operator or a comparison, whose result
// is a value of its own, is never seen by anything but the local. A
// BigInt an operator makes and a store to such a local keeps is marked so
// (bigSoleBit, treeOwnRes), and when the local is assigned the result of
// an operator of two BigInts, the value it held, so marked, is given back
// once the operator has made the next (bigRelease).
//
// The question is asked of the bytecode, as a stack machine runs it, over
// the instructions it knows: a function with any other has no such local.
// A value a local pushes is followed to the instruction that takes it: an
// operator's operand, a test, a drop, or anything else, which it escapes
// to -- a store of the value itself to a local or a global, a call, a
// property's read on it, a return, a jump past which it is still on the
// stack.

// bigSoleBit marks, on a 64-bit machine, a BigInt on the heap that one
// local holds and nothing else has seen: one of soleLocals', stored there
// as an operator's result.
const bigSoleBit = uint(1) << (bits.UintSize - 2)

// soleLocals is the locals of fn, of the first 64, that hold their values
// alone (bytecode.SoleLocals), on a 64-bit machine, whose BigInts have
// room in their headers for the mark.
func soleLocals(fn *bytecode.Function) uint64 {
	if bits.UintSize != 64 {
		return 0
	}
	return bytecode.SoleLocals(fn)
}

// ownedPC is the pc of the operator a, an arithmetic or bitwise node,
// whose result the store at pc at stores to local k, marked to mark the
// result as k's alone and to give back the value k held (treeOwnRes) --
// where k is one of soleLocals'.
func (b *tbuilder) ownedPC(k uint32, a *arithOf, at int) (int, bool) {
	if a.imm || k >= 64 || at > treePCMask {
		return a.pc, false
	}
	if !b.soleDone {
		b.sole, b.soleDone = soleLocals(b.fn), true
	}
	if b.sole&(1<<k) == 0 {
		return a.pc, false
	}
	return a.pc | treeOwnRes | int(k)<<treeOwnShift, true
}

// ownedStore is e, which a store to local k takes, made again with
// ownedPC's marks, where it is an arithmetic or bitwise node of two
// operands and k one of soleLocals'; e as it is otherwise.
func (b *tbuilder) ownedStore(k uint32, e tentry, at int) tentry {
	a := e.arith
	if a == nil || e.v == nil || e.slot >= 0 {
		return e
	}
	pc, ok := b.ownedPC(k, a, at)
	if !ok {
		return e
	}
	o := *a
	o.pc = pc
	if isBitwise(o.op) {
		e.v = bitwiseNode(o.op, o.x.tree(), o.y.tree(), pc)
	} else {
		e.v = arithEntries(o.op, o.x, o.y, pc)
	}
	e.arith = &o
	return e
}
