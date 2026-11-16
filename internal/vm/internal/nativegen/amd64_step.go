package nativegen

import (
	. "github.com/go-quickjs/go-quickjs/internal/jit/asm/amd64"
)

// The step-and-branch's bound: a register B, the immediate SB, or the
// frame closure's upvalue B, as its partner's.
const (
	stepReg = iota
	stepImm
	stepUpval
)

// stepJ is a step-and-branch (step_jlt and its kin): R[A] += k, X's
// signed low 7 bits, committed, then R[A] compared with the bound as its
// partner, the compare-and-branch at pc+1, compares it -- swap and cond
// as cmp's -- the jump taken to pc+1+SC, else on to pc+2, past the
// partner. A counter that is not a number, or whose register holds a
// pointer, exits before anything is committed, to the slow function
// (vm's nativeSlowOnMiss), which makes the step and goes on at pc+1.
// Once the step is committed nothing exits: a bound that is not a number
// and a backward jump that finds the budget spent go on at the partner
// (cont), natively, whose handler compares, or exits, as it does
// unfused.
func (g *nativeGen) stepJ(bound int, swap bool, cond Cond) {
	a := g.a
	cont, tk, fwd := a.NewLabel(), a.NewLabel(), a.NewLabel()
	g.decA(RDX)
	g.numMem(0, RDX, g.exit)
	g.noRef(RDX)
	// k: X's bits 8 to 14, sign-extended.
	a.MovRR(RCX, RAX)
	a.ShiftImm(Shl, RCX, 49, true)
	a.ShiftImm(Sar, RCX, 57, true)
	a.Cvtsi2sd(1, RCX, true)
	a.SSEOp(AddSD, 0, 1)
	g.canon(0)
	a.StoreSDIdx(R12, RDX, 1, 0, 0)
	switch bound {
	case stepReg:
		g.decB(RSI)
		g.numOr(1, RSI, cont)
	case stepImm:
		g.immSB(RCX)
		a.Cvtsi2sd(1, RCX, true)
	case stepUpval:
		// The upvalue, as upval and cmpU read it, a miss going on at the
		// partner rather than exiting.
		g.decB(RCX)
		a.ShiftImm(Shr, RCX, 4, true)
		a.Load(RSI, RDI, ncNF)
		a.Load(RSI, RSI, offFCl)
		a.CmpMem(RCX, RSI, offClUpvals+8, true)
		a.Jcc(CondAE, cont)
		a.Load(RSI, RSI, offClUpvals)
		a.LoadIdx(RSI, RSI, RCX, 8, 0)
		a.Load(RSI, RSI, 0)
		a.Load(R9, RSI, 0)
		a.Op(Cmp, R9, R10, true)
		a.Jcc(CondAE, cont)
		a.MovQToX(1, R9)
	}
	if swap {
		a.SSEOp(UcomiSD, 1, 0)
	} else {
		a.SSEOp(UcomiSD, 0, 1)
	}
	a.Jcc(cond, tk)
	a.OpImm(Add, RBX, 16, true)
	g.dispatch()
	// Taken: as taken, the budget's end going on at the partner.
	a.Bind(tk)
	g.immSC(RCX)
	a.Op(Test, RCX, RCX, true)
	a.Jcc(CondNS, fwd)
	a.OpImm(Cmp, R11, 1, true)
	a.Jcc(CondLE, cont)
	a.OpImm(Sub, R11, 1, true)
	a.Bind(fwd)
	a.LeaIdx(RBX, RBX, RCX, 8, 8)
	g.dispatch()
	a.Bind(cont)
	g.next()
}
