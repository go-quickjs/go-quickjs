package nativegen

import (
	"github.com/go-quickjs/go-quickjs/internal/bytecode"
	"github.com/go-quickjs/go-quickjs/internal/jit/abi"
	. "github.com/go-quickjs/go-quickjs/internal/jit/asm/amd64"
)

// latticeAMD64 records op's operands' kinds in the top frame's lattice
// byte of the instruction, RBX's (lattice.go), where the cc's lattice has
// one, as V8's interpreter updates a site's feedback: the byte's address
// in RCX, then for each register operand, its word loaded into R8, a test
// of the byte for the operand's kind, and a branch past the update where
// the byte has it, so that a site that has met what it meets costs no
// store. A number is so where the byte has any number's bit: whether it
// is an int32 is found, by converting it, only where it has none, the
// first time; an int32's site that meets a double later learns it where
// its speculation fails (package vm's jitLatticeWiden, at the
// deoptimization). What the byte lacks is learned out of line, in the
// code tail returns, which the caller lays after the handler's last
// jump. It uses RCX, RSI, R8, R9, X0 and X1, which plain's handlers set
// before they read them, and keeps RAX, the instruction, and the pinned
// registers. Where numbers is not nil, the code jumps there where the last
// operand is a number: for an operator whose BigInts' marks (feedbackBigAMD64)
// look for two BigInts, which then there are not.
func latticeAMD64(a *Asm, op bytecode.ROp, numbers *Label) (tail func()) {
	latticeKinds()
	lsbs, imm := latticeForm(op)
	ops := latticeOps()
	done := a.NewLabel()
	var tails []func()
	// The byte: the register pc's, (RBX - codeBase) / 8, if the lattice
	// has it.
	a.Load(R9, RDI, ncCC)
	a.MovRR(RCX, RBX)
	a.OpMem(Sub, RCX, RDI, ncCodeBase, true)
	a.ShiftImm(Shr, RCX, 3, true)
	a.CmpMem(RCX, R9, ops+8, true)
	a.Jcc(CondAE, done)
	a.AddMem(RCX, R9, ops)
	for _, lsb := range lsbs {
		next, tagged, number := a.NewLabel(), a.NewLabel(), a.NewLabel()
		// The register's word: its index times 16 in the window.
		a.MovRR(RSI, RAX)
		a.ShiftImm(Shr, RSI, lsb-4, true)
		a.OpImm(And, RSI, 0xFFFF0, false)
		a.LoadIdx(R8, R12, RSI, 1, 0)
		// A number, below the least tag, R10, whose low bit, the
		// barrier's flag, no tag's word is told by: none to learn where
		// the byte has a number's bit.
		a.Op(Cmp, R8, R10, true)
		a.Jcc(CondAE, tagged)
		a.TestMem8(RCX, 0, latticeInt32|latticeNumber)
		a.Jcc(CondE, number)
		a.Bind(next)
		tails = append(tails, func() {
			a.Bind(number)
			latticeNumberAMD64(a)
			latticeLearnAMD64(a, next)
			a.Bind(tagged)
			latticeTagAMD64(a)
			latticeLearnAMD64(a, next)
		})
	}
	if numbers != nil {
		a.Op(Cmp, R8, R10, true)
		a.Jcc(CondB, *numbers)
	}
	if imm {
		// The immediate, an int32.
		immediate := a.NewLabel()
		a.TestMem8(RCX, 0, latticeInt32)
		a.Jcc(CondE, immediate)
		tails = append(tails, func() {
			a.Bind(immediate)
			a.MovImm(RSI, uint64(latticeInt32))
			latticeLearnAMD64(a, done)
		})
	}
	a.Bind(done)
	return func() {
		for _, t := range tails {
			t()
		}
	}
}

// latticeLearnAMD64 ORs ESI's kind into the byte at RCX, where it lacks
// it, and jumps to next. It uses R9.
func latticeLearnAMD64(a *Asm, next Label) {
	a.LoadU8(R9, RCX, 0)
	a.Op(Test, R9, RSI, false)
	a.Jcc(CondNE, next)
	a.Op(Or, R9, RSI, false)
	a.Store8(RCX, 0, R9)
	a.Jmp(next)
}

// latticeNumberAMD64 puts in ESI the kind of the number whose word is R8,
// as package vm's jitLatticeKind finds it: an int32 where truncated to
// one and back it is itself, but -0, which an int32 has none of. It uses
// R9, X0 and X1.
func latticeNumberAMD64(a *Asm) {
	double, have := a.NewLabel(), a.NewLabel()
	a.MovQToX(0, R8)
	a.Cvttsd2si32(R9, 0)
	a.Cvtsi2sd(1, R9, false)
	a.SSEOp(UcomiSD, 0, 1)
	a.Jcc(CondNE, double)
	a.Jcc(CondP, double)
	a.MovImm(RSI, uint64(latticeInt32))
	a.Op(Test, R9, R9, false)
	a.Jcc(CondNE, have)
	a.Op(Test, R8, R8, true)
	a.Jcc(CondNS, have)
	a.Bind(double)
	a.MovImm(RSI, uint64(latticeNumber))
	a.Bind(have)
}

// latticeTagAMD64 puts in ESI the kind of the tagged value whose word is
// R8: a BigInt from heapBigBits up, in the int64 range from shortIntBase;
// another tag's kind by its low byte, the oddballs' in a row, a
// string's, or another. It uses R9.
func latticeTagAMD64(a *Asm) {
	have, notBig, other := a.NewLabel(), a.NewLabel(), a.NewLabel()
	a.MovImm(RSI, uint64(latticeBig64))
	a.MovImm(R9, shortIntBase)
	a.Op(Cmp, R8, R9, true)
	a.Jcc(CondAE, have)
	a.MovImm(RSI, uint64(latticeBig))
	a.MovImm(R9, heapBigBits)
	a.Op(Cmp, R8, R9, true)
	a.Jcc(CondAE, have)
	a.Bind(notBig)
	a.MovZX8(R9, R8)
	a.OpImm(Sub, R9, int32(KindUndefined), false)
	a.MovImm(RSI, uint64(latticeOddball))
	a.OpImm(Cmp, R9, int32(KindBool-KindUndefined), false)
	a.Jcc(CondBE, have)
	a.MovImm(RSI, uint64(latticeString))
	a.OpImm(Cmp, R9, int32(KindString-KindUndefined), false)
	a.Jcc(CondE, have)
	a.Bind(other)
	a.MovImm(RSI, uint64(latticeOther))
	a.Bind(have)
}

// latticeRanAMD64 counts, at a return, in the last byte of the lattice of
// the cc in R9, past its instructions', where it has one, the function's
// runs to their end, up to abi.OperandRuns (package vm's jitLatticeRan).
// It jumps to skip where there is none or the count is full, and uses
// RCX, RDX and R8.
func latticeRanAMD64(a *Asm, skip Label) {
	ops := latticeOps()
	a.Load(R8, R9, ops+8)
	a.Op(Test, R8, R8, true)
	a.Jcc(CondE, skip)
	a.Load(RCX, R9, ops)
	a.LoadU8Idx(RDX, RCX, R8, -1)
	a.OpImm(Cmp, RDX, abi.OperandRuns, false)
	a.Jcc(CondAE, skip)
	a.OpImm(Add, RDX, 1, false)
	a.Store8Idx(RCX, R8, -1, RDX)
}
