package nativegen

import (
	"slices"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
	"github.com/go-quickjs/go-quickjs/internal/jit/abi"
)

// The operand lattice (package vm's jit_lattice.go): the feedback
// variant's handler of each arithmetic, comparison and bitwise operation
// (bytecode's LatticeOps) ORs the kinds of the operands it reads
// (abi.Operands) into its instruction's byte of the top frame's lattice,
// the cc's jitRegOps, a byte for each of its register code's
// instructions, by the instruction's register pc, before it goes on in
// plain's handler: V8's interpreter's BinaryOperation and CompareOperation
// feedback, which the JIT's builder speculates on. A kind is found as
// package vm's jitLatticeKind finds it: a number below the least tag,
// an int32 where truncating it to one and back gives it, but -0; a BigInt
// from heapBigBits up, in the int64 range from shortIntBase up; a tag's
// kind otherwise. The handler tests the byte for the kind before it
// stores, as V8's feedback update does, and a number is in it where the
// byte has any number's bit: whether a number is an int32 is found only
// the first time the site meets a number (latticeAMD64). Where the frame's cc has no lattice -- nil, its length 0
// -- nothing is recorded. Plain's handlers have no part of it.

// latticeForm is where in the instruction word op's registers are, by its
// lattice form: each the bit its 16-bit index starts at, A 16, B 32, C
// 48; and whether its other operand is an immediate, an int32's number.
func latticeForm(op bytecode.ROp) (lsbs []uint8, imm bool) {
	switch op.Lattice() {
	case bytecode.LatticeBC:
		return []uint8{32, 48}, false
	case bytecode.LatticeBImm:
		return []uint8{32}, true
	case bytecode.LatticeB:
		return []uint8{32}, false
	case bytecode.LatticeAB:
		return []uint8{16, 32}, false
	case bytecode.LatticeAImm:
		return []uint8{16}, true
	}
	return nil, false
}

// latticeRecords reports whether op's feedback handler records the
// lattice, after its other part (a back edge's count) and before its
// BigInts' marks: one of bytecode's LatticeOps, each of which has a
// native handler of plain's.
func latticeRecords(op bytecode.ROp) bool {
	return op.Lattice() != bytecode.LatticeNone
}

// latticeNewOps are the lattice's operations feedbackOps has for nothing
// else, appended after the others so that no slot of the table moves
// (slotPlain).
var latticeNewOps = func() []bytecode.ROp {
	var ops []bytecode.ROp
	for _, op := range bytecode.LatticeOps {
		if !slices.Contains(feedbackBaseOps, op) {
			ops = append(ops, op)
		}
	}
	return ops
}()

// latticeOps is the offset of the lattice's slice from the context's cc
// (ccRel): closureCode's jitRegOps, a field of the JIT's.
func latticeOps() int32 {
	return ccRel(off("closureCode.jitRegOps"))
}

// The kinds' bits, abi's.
const (
	latticeInt32   = uint8(abi.OperandInt32)
	latticeNumber  = uint8(abi.OperandNumber)
	latticeOddball = uint8(abi.OperandOddball)
	latticeBig64   = uint8(abi.OperandBigInt64)
	latticeBig     = uint8(abi.OperandBigInt)
	latticeString  = uint8(abi.OperandString)
	latticeOther   = uint8(abi.OperandOther)
)

// latticeKinds checks what the handlers assume of vm's kinds: the
// oddballs' three in a row, which one compare tells.
func latticeKinds() {
	if KindNull != KindUndefined+1 || KindBool != KindUndefined+2 {
		panic("nativegen: the oddballs' kinds are not undefined's, null's and boolean's in a row")
	}
}
