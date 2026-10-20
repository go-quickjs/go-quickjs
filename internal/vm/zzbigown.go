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
// alone: each a let or var of its own -- not a parameter, nor captured --
// whose value fn's code reads only as an operand of an operator that
// makes a value of its own, in a function that is no generator, no async
// function, has no direct eval and no arguments object, of instructions
// that soleEffect knows.
func soleLocals(fn *bytecode.Function) uint64 {
	if bits.UintSize != 64 || fn.Generator || fn.Async || fn.HasDirectEval || fn.UsesArguments {
		return 0
	}
	code := fn.Code
	// The candidates, and the instructions control can come to other than
	// from the one before, where nothing pushed before may still be on the
	// stack.
	var cand uint64
	for k := fn.ParamCount; k < len(fn.Locals) && k < 64; k++ {
		if !fn.Locals[k].Captured {
			cand |= 1 << uint(k)
		}
	}
	if cand == 0 {
		return 0
	}
	target := make([]bool, len(code)+1)
	for _, in := range code {
		switch in.Op {
		case bytecode.OpJump, bytecode.OpJumpIfFalse, bytecode.OpJumpIfTrue, bytecode.OpJumpIfCmpFalse:
			if int(in.A) > len(code) {
				return 0
			}
			target[in.A] = true
		}
	}
	// stack holds, for each value the code has pushed, the local it is a
	// read of plus one, or zero for any other value; below its bottom is
	// what was pushed before a jump's target, none of it a read.
	var stack []uint8
	escaped := uint64(0)
	escape := func(tag uint8) {
		if tag != 0 {
			escaped |= 1 << uint(tag-1)
		}
	}
	pop := func() uint8 {
		if len(stack) == 0 {
			return 0
		}
		t := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		return t
	}
	read := func(k uint32) uint8 {
		if k < 64 && cand&(1<<k) != 0 {
			return uint8(k + 1)
		}
		return 0
	}
	settle := func() {
		for _, t := range stack {
			escape(t)
		}
		stack = stack[:0]
	}
	// A store to a local whose read is still on the stack, an operand
	// yet to be taken -- s = s * (s = s * 3n, 5n) -- leaves the value it
	// held to that operand: given back at the store, its block would be
	// made the next result in while the operand is still to be read.
	stored := func(k uint32) {
		if tag := read(k); tag != 0 {
			for _, t := range stack {
				if t == tag {
					escape(tag)
				}
			}
		}
	}
	for pc, in := range code {
		if target[pc] {
			settle()
		}
		switch in.Op {
		case bytecode.OpGetLocal, bytecode.OpGetLocalCheck:
			stack = append(stack, read(in.A))
		case bytecode.OpGetLocal2:
			stack = append(stack, read(in.A), read(in.B))
		case bytecode.OpSetLocal, bytecode.OpInitLocal, bytecode.OpSetLocalCheck:
			escape(pop())
			stored(in.A)
		case bytecode.OpPutLocal:
			if len(stack) != 0 {
				escape(stack[len(stack)-1])
			}
			stored(in.A)
		case bytecode.OpSetLocalGet:
			escape(pop())
			stored(in.A)
			stack = append(stack, read(in.B))
		case bytecode.OpClearLocal, bytecode.OpIncLocal, bytecode.OpDecLocal:
			// A store of undefined, a step that reads the value as an
			// operand.
			stored(in.A)
		case bytecode.OpBinLocal, bytecode.OpBinImm, bytecode.OpNeg, bytecode.OpNot, bytecode.OpBitNot:
			// An operand, and the result; bin_local's local an operand too.
			pop()
			stack = append(stack, 0)
		case bytecode.OpLocalBinImm:
			stack = append(stack, 0)
		case bytecode.OpAdd, bytecode.OpSub, bytecode.OpMul, bytecode.OpDiv, bytecode.OpMod,
			bytecode.OpBitAnd, bytecode.OpBitOr, bytecode.OpBitXor, bytecode.OpShl, bytecode.OpShr, bytecode.OpUShr,
			bytecode.OpLt, bytecode.OpLe, bytecode.OpGt, bytecode.OpGe,
			bytecode.OpEq, bytecode.OpNe, bytecode.OpStrictEq, bytecode.OpStrictNe:
			pop()
			pop()
			stack = append(stack, 0)
		case bytecode.OpPushConst, bytecode.OpPushInt, bytecode.OpPushUndef, bytecode.OpPushNull,
			bytecode.OpPushTrue, bytecode.OpPushFalse, bytecode.OpPushEmptyString, bytecode.OpGetGlobal:
			stack = append(stack, 0)
		case bytecode.OpDup:
			t := pop()
			stack = append(stack, t, t)
		case bytecode.OpDrop:
			pop()
		case bytecode.OpSwap:
			b, a := pop(), pop()
			stack = append(stack, b, a)
		case bytecode.OpSetGlobal, bytecode.OpSetGlobalStrict:
			escape(pop())
		case bytecode.OpGetPropThis:
			// The receiver stays, and the method is read of it.
			if len(stack) != 0 {
				escape(stack[len(stack)-1])
			}
			stack = append(stack, 0)
		case bytecode.OpCall, bytecode.OpCallMethod:
			n := int(in.A) + 1
			if in.Op == bytecode.OpCallMethod {
				n++
			}
			for i := 0; i < n; i++ {
				escape(pop())
			}
			stack = append(stack, 0)
		case bytecode.OpJumpIfFalse, bytecode.OpJumpIfTrue:
			pop()
			settle()
		case bytecode.OpJumpIfCmpFalse:
			pop()
			pop()
			settle()
		case bytecode.OpJump, bytecode.OpReturnUndef:
			settle()
		case bytecode.OpReturn:
			escape(pop())
			settle()
		default:
			return 0
		}
	}
	return cand &^ escaped
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
