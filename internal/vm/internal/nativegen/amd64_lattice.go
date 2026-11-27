package nativegen

import (
	"github.com/go-quickjs/go-quickjs/internal/bytecode"
	"github.com/go-quickjs/go-quickjs/internal/jit/abi"
	. "github.com/go-quickjs/go-quickjs/internal/jit/asm/amd64"
)

// latticeAMD64 records op's operands' kinds in the top frame's lattice
// byte of the instruction, RBX's (lattice.go), where the cc's lattice has
// one, as V8's interpreter updates a site's feedback: the byte's address
// in RCX, each register operand's word loaded -- the last's into R8, the
// first of two's into RDX, where they stay for the BigInts' marks -- and
// a test of the byte for what they are, so that a site that has met what
// it meets costs no store. Numbers are so where the byte has any number's
// bit, or an int32's where the other operand is an immediate, which is
// one: one test for both operands. BigInts in the int64 range, both where
// there are two, are so where the byte has their bit: one compare of the
// words ANDed, out of line, and one test. Anything else is learned out of
// line, operand by operand, in the code tail returns, which the caller
// lays after the handler's last jump: whether a number is an int32 is
// found, by converting it, only where the byte has no number's bit, the
// first time; an int32's site that meets a double later learns it where
// its speculation fails (package vm's jitLatticeWiden, at the
// deoptimization). It uses RCX, RDX, RSI, R8, R9, X0 and X1, which plain's
// handlers set before they read them, and keeps RAX, the instruction, and
// the pinned registers.
//
// The code goes on after it where the cc has no lattice of the
// instruction, where the operands are numbers, and where it has found or
// learned anything else -- but where x, an operator's whose BigInts' marks
// (feedbackBigAMD64) look for two BigInts, says: x.none where there is no
// lattice, the words not loaded; x.known where both are BigInts held in
// their Values; x.check where the operands are not numbers, their words
// to be looked at. Where they are numbers, which are not two BigInts, the
// caller goes on in plain's handler.
func latticeAMD64(a *Asm, op bytecode.ROp, x *latticeExitsAMD64) (tail func()) {
	latticeKinds()
	lsbs, imm := latticeForm(op)
	words := latticeWordsAMD64(len(lsbs))
	ops := latticeOps()
	done := a.NewLabel()
	none, known, check := done, done, done
	if x != nil {
		none, known, check = x.none, x.known, x.check
	}
	tagged, numbers := a.NewLabel(), a.NewLabel()
	// The byte: the register pc's, (RBX - codeBase) / 8, if the lattice
	// has it.
	a.Load(R9, RDI, ncCC)
	a.MovRR(RCX, RBX)
	a.OpMem(Sub, RCX, RDI, ncCodeBase, true)
	a.ShiftImm(Shr, RCX, 3, true)
	a.CmpMem(RCX, R9, ops+8, true)
	a.Jcc(CondAE, none)
	a.AddMem(RCX, R9, ops)
	for i, lsb := range lsbs {
		// The register's word: its index times 16 in the window.
		a.MovRR(RSI, RAX)
		a.ShiftImm(Shr, RSI, lsb-4, true)
		a.OpImm(And, RSI, 0xFFFF0, false)
		a.LoadIdx(words[i], R12, RSI, 1, 0)
	}
	// Numbers, below the least tag, R10, whose low bit, the barrier's
	// flag, no tag's word is told by.
	for _, w := range words {
		a.Op(Cmp, w, R10, true)
		a.Jcc(CondAE, tagged)
	}
	numberBits := latticeInt32 | latticeNumber
	if imm {
		numberBits = latticeInt32
	}
	a.TestMem8(RCX, 0, numberBits)
	a.Jcc(CondE, numbers)
	a.Bind(done)
	return func() {
		// An operand that is no number: BigInts in the int64 range that
		// the byte has met, or each operand's kind, learned as numbers
		// the byte lacks a bit of are, operand by operand, a number where
		// the byte has no number's bit by then.
		general := a.NewLabel()
		a.Bind(tagged)
		w := words[0]
		if len(words) == 2 {
			a.MovRR(R9, RDX)
			a.Op(And, R9, R8, true)
			w = R9
		}
		a.MovImm(RSI, shortIntBase)
		a.Op(Cmp, w, RSI, true)
		a.Jcc(CondB, general)
		a.TestMem8(RCX, 0, latticeBig64)
		a.Jcc(CondE, general)
		if imm {
			a.TestMem8(RCX, 0, latticeInt32)
			a.Jcc(CondE, general)
		}
		a.Jmp(known)
		a.Bind(general)
		a.Bind(numbers)
		for _, w := range words {
			next, tag := a.NewLabel(), a.NewLabel()
			a.Op(Cmp, w, R10, true)
			a.Jcc(CondAE, tag)
			a.TestMem8(RCX, 0, latticeInt32|latticeNumber)
			a.Jcc(CondNE, next)
			latticeNumberAMD64(a, w)
			latticeLearnAMD64(a, next)
			a.Bind(tag)
			latticeTagAMD64(a, w)
			latticeLearnAMD64(a, next)
			a.Bind(next)
		}
		latticeImmAMD64(a, imm)
		a.Jmp(check)
	}
}

// latticeExitsAMD64 are where an operator's lattice code (latticeAMD64)
// goes on to its BigInts' marks.
type latticeExitsAMD64 struct {
	// none: the cc has no lattice of the instruction; the operands'
	// words are not loaded.
	none Label
	// known: both operands are BigInts held in their Values.
	known Label
	// check: the operands' words are loaded, not both numbers.
	check Label
}

// latticeWordsAMD64 are the registers latticeAMD64 loads n operands'
// words into: the last's R8, the first of two's RDX.
func latticeWordsAMD64(n int) []Reg {
	if n == 2 {
		return []Reg{RDX, R8}
	}
	return []Reg{R8}
}

// latticeImmAMD64 learns, where imm, the immediate's kind, an int32's,
// where the byte at RCX lacks it.
func latticeImmAMD64(a *Asm, imm bool) {
	if !imm {
		return
	}
	next := a.NewLabel()
	a.TestMem8(RCX, 0, latticeInt32)
	a.Jcc(CondNE, next)
	a.MovImm(RSI, uint64(latticeInt32))
	latticeLearnAMD64(a, next)
	a.Bind(next)
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

// latticeNumberAMD64 puts in ESI the kind of the number whose word is w,
// as package vm's jitLatticeKind finds it: an int32 where truncated to
// one and back it is itself, but -0, which an int32 has none of. It uses
// R9, X0 and X1.
func latticeNumberAMD64(a *Asm, w Reg) {
	double, have := a.NewLabel(), a.NewLabel()
	a.MovQToX(0, w)
	a.Cvttsd2si32(R9, 0)
	a.Cvtsi2sd(1, R9, false)
	a.SSEOp(UcomiSD, 0, 1)
	a.Jcc(CondNE, double)
	a.Jcc(CondP, double)
	a.MovImm(RSI, uint64(latticeInt32))
	a.Op(Test, R9, R9, false)
	a.Jcc(CondNE, have)
	a.Op(Test, w, w, true)
	a.Jcc(CondNS, have)
	a.Bind(double)
	a.MovImm(RSI, uint64(latticeNumber))
	a.Bind(have)
}

// latticeTagAMD64 puts in ESI the kind of the tagged value whose word is
// w: a BigInt from heapBigBits up, in the int64 range from shortIntBase;
// another tag's kind by its low byte, the oddballs' in a row, a
// string's, or another. It uses R9.
func latticeTagAMD64(a *Asm, w Reg) {
	have, notBig, other := a.NewLabel(), a.NewLabel(), a.NewLabel()
	a.MovImm(RSI, uint64(latticeBig64))
	a.MovImm(R9, shortIntBase)
	a.Op(Cmp, w, R9, true)
	a.Jcc(CondAE, have)
	a.MovImm(RSI, uint64(latticeBig))
	a.MovImm(R9, heapBigBits)
	a.Op(Cmp, w, R9, true)
	a.Jcc(CondAE, have)
	a.Bind(notBig)
	a.MovZX8(R9, w)
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
