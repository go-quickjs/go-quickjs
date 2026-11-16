package nativegen

import (
	. "github.com/go-quickjs/go-quickjs/internal/jit/asm/arm64"
)

// stepJ is amd64's (amd64_step.go): R[A] += k committed, then the
// partner's comparison, its jump to pc+1+SC, else on to pc+2; a counter
// that is not a number, or whose register holds a pointer, exits before
// anything is committed, and after the commit a bound that is not a
// number or a backward jump that finds the budget spent go on at the
// partner, pc+1 (cont).
func (g *armGen) stepJ(bound int, swap bool, cond cc) {
	a := g.a
	cont, tk, fwd := a.NewLabel(), a.NewLabel(), a.NewLabel()
	g.regA(rD)
	g.numMem(0, rD, g.exit)
	g.noRef(rD)
	// k: X's bits 8 to 14, sign-extended.
	a.Sbfx(rC, rW, 8, 7)
	a.Scvtf(1, rC, true)
	a.FArith(FAdd, 0, 0, 1)
	g.canon(0)
	a.StoreF(rD, 0, 0)
	switch bound {
	case stepReg:
		g.regB(rS)
		g.numOr(1, rS, cont)
	case stepImm:
		g.immSB(rC)
		a.Scvtf(1, rC, true)
	case stepUpval:
		// The upvalue, as upval and cmpU read it, a miss going on at the
		// partner rather than exiting.
		a.Ubfx(rC, rW, 32, 16)
		a.Load(rS, rCtx, ncNF)
		a.Load(rS, rS, offFCl)
		g.cmpMem(rC, rS, offClUpvals+8)
		a.BCond(HS, cont)
		a.Load(rS, rS, offClUpvals)
		a.LoadIdx(rS, rS, rC, true)
		a.Load(rS, rS, 0)
		g.numOr(1, rS, cont)
	}
	if swap {
		a.FCmp(1, 0)
	} else {
		a.FCmp(0, 1)
	}
	a.BCond(cond.f(), tk)
	a.AddImm(rIP, rIP, 16, true)
	g.dispatch()
	// Taken: as taken, the budget's end going on at the partner.
	a.Bind(tk)
	g.immSC(rC)
	a.CmpImm(rC, 0, true)
	a.BCond(PL, fwd)
	a.CmpImm(rBud, 1, true)
	a.BCond(LE, cont)
	a.AddImm(rBud, rBud, -1, true)
	a.Bind(fwd)
	a.AddLsl(rIP, rIP, rC, 3)
	a.AddImm(rIP, rIP, 8, true)
	g.dispatch()
	a.Bind(cont)
	g.next()
}
