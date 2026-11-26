package nativegen

import (
	"github.com/go-quickjs/go-quickjs/internal/bytecode"
	"github.com/go-quickjs/go-quickjs/internal/jit/abi"
	. "github.com/go-quickjs/go-quickjs/internal/jit/asm/arm64"
)

// latticeARM64 is latticeAMD64 on arm64: the byte's address in rC and
// the byte in rD, each register operand's word loaded into r8 and tested
// against it, a kind the byte lacks learned out of line, in the code tail
// returns, which the caller lays after the handler's last jump. It uses
// rC, rD, rS, r8, r9, D0 and D1, which plain's handlers set before they
// read them, and keeps rW, the instruction, and the pinned registers.
// Where numbers is not nil, the code jumps there where the last operand
// is a number.
func latticeARM64(a *Asm, op bytecode.ROp, numbers *Label) (tail func()) {
	latticeKinds()
	lsbs, imm := latticeForm(op)
	ops := latticeOps()
	done := a.NewLabel()
	var tails []func()
	// The byte: the register pc's, (rIP - codeBase) / 8, if the lattice
	// has it.
	a.Load(r9, rCtx, ncCC)
	a.Load(rS, r9, ops+8)
	a.Load(rC, rCtx, ncCodeBase)
	a.Op(Sub, rC, rIP, rC, true)
	a.ShiftImm(Lsr, rC, rC, 3, true)
	a.Cmp(rC, rS, true)
	a.BCond(HS, done)
	a.Load(rS, r9, ops)
	a.Op(Add, rC, rS, rC, true)
	a.LoadU8(rD, rC, 0)
	for _, lsb := range lsbs {
		next, tagged, number := a.NewLabel(), a.NewLabel(), a.NewLabel()
		a.Ubfx(rS, rW, lsb, 16)
		a.AddLsl(rS, rRegs, rS, 4)
		a.Load(r8, rS, 0)
		// A number, below the least tag: none to learn where the byte
		// has a number's bit.
		a.Cmp(r8, rTag, true)
		a.BCond(HS, tagged)
		a.TstImm(rD, uint64(latticeInt32|latticeNumber), false)
		a.BCond(EQ, number)
		a.Bind(next)
		tails = append(tails, func() {
			a.Bind(number)
			latticeNumberARM64(a)
			latticeLearnARM64(a, next)
			a.Bind(tagged)
			latticeTagARM64(a)
			latticeLearnARM64(a, next)
		})
	}
	if numbers != nil {
		a.Cmp(r8, rTag, true)
		a.BCond(LO, *numbers)
	}
	if imm {
		immediate := a.NewLabel()
		a.TstImm(rD, uint64(latticeInt32), false)
		a.BCond(EQ, immediate)
		tails = append(tails, func() {
			a.Bind(immediate)
			a.MovImm(rS, uint64(latticeInt32))
			latticeLearnARM64(a, done)
		})
	}
	a.Bind(done)
	return func() {
		for _, t := range tails {
			t()
		}
	}
}

// latticeLearnARM64 ORs rS's kind into rD and the byte at rC, where it
// lacks it, and jumps to next.
func latticeLearnARM64(a *Asm, next Label) {
	a.Tst(rD, rS, false)
	a.BCond(NE, next)
	a.Op(Orr, rD, rD, rS, false)
	a.Store8(rC, 0, rD)
	a.B(next)
}

// latticeNumberARM64 puts in rS the kind of the number whose word is r8,
// as latticeNumberAMD64 does: FCVTZS saturates, and gives NaN 0, neither
// of which converts back to what it came from but 0. It uses r9, D0 and
// D1.
func latticeNumberARM64(a *Asm) {
	double, have := a.NewLabel(), a.NewLabel()
	a.FMovToF(0, r8)
	a.Fcvtz(r9, 0, true, false, false)
	a.Scvtf(1, r9, false)
	a.FCmp(0, 1)
	a.BCond(NE, double)
	a.MovImm(rS, uint64(latticeInt32))
	a.Cbnz(r9, have, false)
	a.TstImm(r8, 1<<63, true)
	a.BCond(EQ, have)
	a.Bind(double)
	a.MovImm(rS, uint64(latticeNumber))
	a.Bind(have)
}

// latticeTagARM64 puts in rS the kind of the tagged value whose word is
// r8, as latticeTagAMD64 does. It uses r9.
func latticeTagARM64(a *Asm) {
	have := a.NewLabel()
	a.MovImm(rS, uint64(latticeBig64))
	a.MovImm(r9, shortIntBase)
	a.Cmp(r8, r9, true)
	a.BCond(HS, have)
	a.MovImm(rS, uint64(latticeBig))
	a.MovImm(r9, heapBigBits)
	a.Cmp(r8, r9, true)
	a.BCond(HS, have)
	a.Ubfx(r9, r8, 0, 8)
	a.SubsImm(r9, r9, int64(KindUndefined), false)
	a.MovImm(rS, uint64(latticeOddball))
	a.CmpImm(r9, int64(KindBool-KindUndefined), false)
	a.BCond(LS, have)
	a.MovImm(rS, uint64(latticeString))
	a.CmpImm(r9, int64(KindString-KindUndefined), false)
	a.BCond(EQ, have)
	a.MovImm(rS, uint64(latticeOther))
	a.Bind(have)
}

// latticeRanARM64 is latticeRanAMD64 on arm64: the runs counted in the
// last byte of the lattice of the cc in r9, where it has one, up to
// abi.OperandRuns. It uses rC, rD and rS.
func latticeRanARM64(a *Asm) {
	none := a.NewLabel()
	ops := latticeOps()
	a.Load(rD, r9, ops+8)
	a.Cbz(rD, none, true)
	a.Load(rS, r9, ops)
	a.AddImm(rS, rS, -1, true)
	a.LoadU8Idx(rC, rS, rD)
	a.CmpImm(rC, abi.OperandRuns, false)
	a.BCond(HS, none)
	a.AddImm(rC, rC, 1, false)
	a.Store8Idx(rS, rD, rC)
	a.Bind(none)
}
