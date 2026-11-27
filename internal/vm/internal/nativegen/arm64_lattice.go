package nativegen

import (
	"github.com/go-quickjs/go-quickjs/internal/bytecode"
	"github.com/go-quickjs/go-quickjs/internal/jit/abi"
	. "github.com/go-quickjs/go-quickjs/internal/jit/asm/arm64"
)

// latticeARM64 is latticeAMD64 on arm64: the byte's address in rC and
// the byte in rD, the register operands' words loaded -- the last's into
// r8, the first of two's into rK, where they stay for the BigInts' marks
// -- numbers tested against the byte once for both, BigInts in the int64
// range by their words ANDed, out of line, and anything else learned out
// of line, operand by operand, in the code tail returns, which the caller
// lays after the handler's last jump. It uses rC, rD, rS, rK, r8, r9, D0
// and D1, which plain's handlers set before they read them, and keeps rW,
// the instruction, and the pinned registers. It goes on as latticeAMD64
// does, at x's labels where x is not nil.
func latticeARM64(a *Asm, op bytecode.ROp, x *latticeExitsARM64) (tail func()) {
	latticeKinds()
	lsbs, imm := latticeForm(op)
	words := latticeWordsARM64(len(lsbs))
	ops := latticeOps()
	done := a.NewLabel()
	none, known, check := done, done, done
	if x != nil {
		none, known, check = x.none, x.known, x.check
	}
	tagged, numbers := a.NewLabel(), a.NewLabel()
	// The byte: the register pc's, (rIP - codeBase) / 8, if the lattice
	// has it.
	a.Load(r9, rCtx, ncCC)
	a.Load(rS, r9, ops+8)
	a.Load(rC, rCtx, ncCodeBase)
	a.Op(Sub, rC, rIP, rC, true)
	a.ShiftImm(Lsr, rC, rC, 3, true)
	a.Cmp(rC, rS, true)
	a.BCond(HS, none)
	a.Load(rS, r9, ops)
	a.Op(Add, rC, rS, rC, true)
	a.LoadU8(rD, rC, 0)
	for i, lsb := range lsbs {
		a.Ubfx(rS, rW, lsb, 16)
		a.AddLsl(rS, rRegs, rS, 4)
		a.Load(words[i], rS, 0)
	}
	// Numbers, below the least tag.
	for _, w := range words {
		a.Cmp(w, rTag, true)
		a.BCond(HS, tagged)
	}
	numberBits := latticeInt32 | latticeNumber
	if imm {
		numberBits = latticeInt32
	}
	a.TstImm(rD, uint64(numberBits), false)
	a.BCond(EQ, numbers)
	a.Bind(done)
	return func() {
		// An operand that is no number: BigInts in the int64 range that
		// the byte has met, or each operand's kind, learned as numbers
		// the byte lacks a bit of are, operand by operand.
		general := a.NewLabel()
		a.Bind(tagged)
		w := words[0]
		if len(words) == 2 {
			a.Op(And, r9, rK, r8, true)
			w = r9
		}
		a.MovImm(rS, shortIntBase)
		a.Cmp(w, rS, true)
		a.BCond(LO, general)
		a.TstImm(rD, uint64(latticeBig64), false)
		a.BCond(EQ, general)
		if imm {
			a.TstImm(rD, uint64(latticeInt32), false)
			a.BCond(EQ, general)
		}
		a.B(known)
		a.Bind(general)
		a.Bind(numbers)
		for _, w := range words {
			next, tag := a.NewLabel(), a.NewLabel()
			a.Cmp(w, rTag, true)
			a.BCond(HS, tag)
			a.TstImm(rD, uint64(latticeInt32|latticeNumber), false)
			a.BCond(NE, next)
			latticeNumberARM64(a, w)
			latticeLearnARM64(a, next)
			a.Bind(tag)
			latticeTagARM64(a, w)
			latticeLearnARM64(a, next)
			a.Bind(next)
		}
		latticeImmARM64(a, imm)
		a.B(check)
	}
}

// latticeExitsARM64 are latticeExitsAMD64 on arm64.
type latticeExitsARM64 struct {
	none, known, check Label
}

// latticeWordsARM64 are the registers latticeARM64 loads n operands'
// words into: the last's r8, the first of two's rK.
func latticeWordsARM64(n int) []Reg {
	if n == 2 {
		return []Reg{rK, r8}
	}
	return []Reg{r8}
}

// latticeImmARM64 learns, where imm, the immediate's kind, an int32's,
// where the byte, rD, lacks it.
func latticeImmARM64(a *Asm, imm bool) {
	if !imm {
		return
	}
	next := a.NewLabel()
	a.TstImm(rD, uint64(latticeInt32), false)
	a.BCond(NE, next)
	a.MovImm(rS, uint64(latticeInt32))
	latticeLearnARM64(a, next)
	a.Bind(next)
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

// latticeNumberARM64 puts in rS the kind of the number whose word is w,
// as latticeNumberAMD64 does: FCVTZS saturates, and gives NaN 0, neither
// of which converts back to what it came from but 0. It uses r9, D0 and
// D1.
func latticeNumberARM64(a *Asm, w Reg) {
	double, have := a.NewLabel(), a.NewLabel()
	a.FMovToF(0, w)
	a.Fcvtz(r9, 0, true, false, false)
	a.Scvtf(1, r9, false)
	a.FCmp(0, 1)
	a.BCond(NE, double)
	a.MovImm(rS, uint64(latticeInt32))
	a.Cbnz(r9, have, false)
	a.TstImm(w, 1<<63, true)
	a.BCond(EQ, have)
	a.Bind(double)
	a.MovImm(rS, uint64(latticeNumber))
	a.Bind(have)
}

// latticeTagARM64 puts in rS the kind of the tagged value whose word is
// w, as latticeTagAMD64 does. It uses r9.
func latticeTagARM64(a *Asm, w Reg) {
	have := a.NewLabel()
	a.MovImm(rS, uint64(latticeBig64))
	a.MovImm(r9, shortIntBase)
	a.Cmp(w, r9, true)
	a.BCond(HS, have)
	a.MovImm(rS, uint64(latticeBig))
	a.MovImm(r9, heapBigBits)
	a.Cmp(w, r9, true)
	a.BCond(HS, have)
	a.Ubfx(r9, w, 0, 8)
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
