package bytecode

// LatticeForm is which operands of a register instruction its operand
// lattice records (the JIT's operand-type feedback: jit/abi's Operands),
// as V8's BinaryOperation and CompareOperation feedback slots record a
// bytecode's: none for an operation that is no arithmetic, comparison or
// bitwise site.
type LatticeForm uint8

const (
	// LatticeNone is an operation with no lattice.
	LatticeNone LatticeForm = iota
	// LatticeBC is R[B] op R[C].
	LatticeBC
	// LatticeBImm is R[B] op an immediate, an int32's number.
	LatticeBImm
	// LatticeB is op R[B].
	LatticeB
	// LatticeAB is R[A] op R[B], a compare-and-branch's.
	LatticeAB
	// LatticeAImm is R[A] op an immediate, a compare-and-branch's.
	LatticeAImm
)

// Lattice is op's lattice form. The equalities -- ==, !=, === and !==
// that branch -- have none: they are compared whole, natively or by Go,
// whatever their operands are, and speculate nothing. Nor do the
// compares with an upvalue, the step-and-branches, and the updates of a
// property or an element, whose operands the instruction reads from
// where no register holds them.
func (op ROp) Lattice() LatticeForm {
	switch op {
	case RAdd, RSub, RMul, RBin, RBitAnd, RBitOr, RBitXor, RShl, RShr, RUShr,
		RAddI32, RSubI32:
		return LatticeBC
	case RAddI, RSubI, RMulI, RAndI, ROrI, RXorI, RShlI, RShrI, RUShrI, RAddII32:
		return LatticeBImm
	case RInc, RDec, RPostInc, RPostDec, RUnary:
		return LatticeB
	case RJLt, RJNLt, RJLe, RJNLe, RJGt, RJNGt, RJGe, RJNGe:
		return LatticeAB
	case RJLtI, RJNLtI, RJLeI, RJNLeI, RJGtI, RJNGtI, RJGeI, RJNGeI:
		return LatticeAImm
	}
	return LatticeNone
}

// LatticeOps are the operations with a lattice, in their values' order.
var LatticeOps = func() []ROp {
	var ops []ROp
	for op := range ROpCount {
		if op.Lattice() != LatticeNone {
			ops = append(ops, op)
		}
	}
	return ops
}()
