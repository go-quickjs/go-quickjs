package ssa

import (
	"github.com/go-quickjs/go-quickjs/internal/jit/abi"
	"github.com/go-quickjs/go-quickjs/internal/jit/ir"
)

// OperandFeedback is Feedback that knows what the operands of an
// arithmetic, comparison or bitwise site met in the interpreter: the VM's
// operand lattice (abi.Operands), V8's BinaryOperationHint and
// CompareOperationHint, by the program's PC. ok is false where it knows
// nothing of the site -- its function has no lattice, or the site is none
// of the lattice's -- which the builder then speculates on as it would
// with no lattice: numbers, until a guard's failure marks it generic.
//
// Where it knows, the builder speculates exactly what was met, as V8's
// optimizer does:
//
//   - nothing: the site never ran, and its block ends at it with a host
//     exit, Go making the instruction whole and the interpreter going on
//     to the next loop header, with no invalidation (V8's soft
//     deoptimization for insufficient feedback) -- what Go meets there
//     the lattice learns, and the code is compiled again for it;
//   - numbers alone: numbers, as before;
//   - numbers and oddballs: numbers, as before, an oddball learned at a
//     guard's failure: the oddballs are booleans as often as not, which
//     the code makes into numbers natively where it knows them for
//     booleans, and code generic there had Go make, among others, the
//     sums of built-in loops' booleans that native loops answer;
//   - BigInts, alone or beside numbers and oddballs: as the BigInt
//     feedback says (BigIntFeedback), whose marks know the operation's
//     results;
//   - a string or anything else -- an object, a symbol -- generic (abi's
//     Operands.Generic): Go makes what native code does not, a string's
//     addition among it, with no guard to fail.
type OperandFeedback interface {
	Operands(pc int) (Operands, bool)
}

// Operands is abi's, the lattice's byte, named here for what answers
// OperandFeedback.
type Operands = abi.Operands

// operandSite reports whether in is one the lattice decides for: an
// arithmetic or bitwise operator, a relational comparison, an update, a
// unary operator but not, and none of typed code's.
func operandSite(in *ir.Instruction) bool {
	if in.Raw != ir.RawNone {
		return false
	}
	switch in.Op {
	case ir.Binary:
		return in.Operator != ir.Eq && in.Operator != ir.Ne
	case ir.Branch:
		return relational(in.Operator)
	case ir.Unary:
		return in.Operator != ir.Not
	case ir.Update:
		return true
	}
	return false
}

// operands is what the feedback knows of the operands of the instruction
// at pc (OperandFeedback), false where it knows nothing or the
// instruction is none the lattice decides for.
func (b *builder) operands(pc int) (abi.Operands, bool) {
	fb, ok := b.fb.(OperandFeedback)
	if !ok || !operandSite(&b.p.Code[pc]) {
		return 0, false
	}
	return fb.Operands(pc)
}

// unseen reports whether the instruction at pc is a site whose operands
// met nothing: one that never ran, left to Go at a host exit that ends its
// block (unfed). Only the first instruction of its register instruction
// (ir.Program's Origin), before which nothing of it was made, nor one
// after a commit (ir's Generic), which may not leave.
func (b *builder) unseen(pc int) bool {
	in := &b.p.Code[pc]
	if in.Generic {
		return false
	}
	if o := b.p.Origin; o == nil || pc > 0 && o[pc-1] == o[pc] {
		return false
	}
	k, ok := b.operands(pc)
	return ok && k == 0
}

// operandsGeneric reports whether the operands of the instruction at pc
// met what no speculation on numbers holds -- a string or anything else
// (abi's Operands.Generic): built generic from the first compile, as one
// whose guard failed at what it met is from its second, each operand
// converted natively or by Go, an addition or a comparison Go's.
func (b *builder) operandsGeneric(pc int) bool {
	k, ok := b.operands(pc)
	return ok && k.Generic()
}
