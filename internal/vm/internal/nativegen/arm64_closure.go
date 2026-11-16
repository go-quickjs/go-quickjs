package nativegen

import (
	. "github.com/go-quickjs/go-quickjs/internal/jit/asm/arm64"
)

// Closures and the upvalues they capture, as amd64's (amd64_closure.go)
// in arm64's instructions.

// closeUps closes the upvalues open on the top frame whose slots lie in
// [scratch sV0, scratch sV1), as amd64's.
func (g *armGen) closeUps() {
	a := g.a
	loop, keep, moved, next, done, clr, clrDone := a.NewLabel(), a.NewLabel(), a.NewLabel(), a.NewLabel(), a.NewLabel(), a.NewLabel(), a.NewLabel()
	a.Store(rCtx, scr(sI), ZR)
	a.Store(rCtx, scr(sKa), ZR)
	a.Load(rS, rCtx, ncNF)
	a.Load(rC, rS, offFOpenUp+8)
	a.Store(rCtx, scr(sEnd), rC)
	a.Bind(loop)
	a.Load(rC, rCtx, scr(sI))
	g.cmpMem(rC, rCtx, scr(sEnd))
	a.BCond(GE, done)
	a.Load(rS, rCtx, ncNF)
	a.Load(rD, rS, offFOpenUp)
	a.LoadIdx(r8, rD, rC, true)
	a.Load(r9, r8, offUpSlot)
	g.cmpMem(r9, rCtx, scr(sV0))
	a.BCond(LO, keep)
	g.cmpMem(r9, rCtx, scr(sV1))
	a.BCond(HS, keep)
	// u.closed = *u.slot; u.slot = &u.closed.
	a.Store(rCtx, scr(sE), r8)
	a.Load(rC, r9, 0)
	a.Store(r8, offUpClosed, rC)
	a.Load(rC, r9, 8)
	a.AddImm(rW, r8, int64(offUpClosed+8), true)
	g.ptrTo()
	a.Load(r8, rCtx, scr(sE))
	a.AddImm(rC, r8, int64(offUpClosed), true)
	a.AddImm(rW, r8, int64(offUpSlot), true)
	g.ptrStore()
	a.B(next)
	// Kept: at the list's next kept place.
	a.Bind(keep)
	a.Load(rC, rCtx, scr(sKa))
	g.cmpMem(rC, rCtx, scr(sI))
	a.BCond(EQ, moved)
	a.AddLsl(rW, rD, rC, 3)
	a.MovRR(rC, r8)
	g.ptrTo()
	a.Bind(moved)
	a.Load(rC, rCtx, scr(sKa))
	a.AddImm(rC, rC, 1, true)
	a.Store(rCtx, scr(sKa), rC)
	a.Bind(next)
	a.Load(rC, rCtx, scr(sI))
	a.AddImm(rC, rC, 1, true)
	a.Store(rCtx, scr(sI), rC)
	a.B(loop)
	// The room past the kept cleared; the length the kept's.
	a.Bind(done)
	a.Load(rC, rCtx, scr(sKa))
	a.Store(rCtx, scr(sI), rC)
	a.Bind(clr)
	a.Load(rC, rCtx, scr(sI))
	g.cmpMem(rC, rCtx, scr(sEnd))
	a.BCond(GE, clrDone)
	a.Load(rS, rCtx, ncNF)
	a.Load(rD, rS, offFOpenUp)
	a.AddLsl(rW, rD, rC, 3)
	a.MovImm(rC, 0)
	g.ptrTo()
	a.Load(rC, rCtx, scr(sI))
	a.AddImm(rC, rC, 1, true)
	a.Store(rCtx, scr(sI), rC)
	a.B(clr)
	a.Bind(clrDone)
	a.Load(rS, rCtx, ncNF)
	a.Load(rC, rCtx, scr(sKa))
	a.Store(rS, offFOpenUp+8, rC)
}

// closeRegs is close_regs, as amd64's.
func (g *armGen) closeRegs() {
	a := g.a
	g.regA(rD)
	a.Store(rCtx, scr(sV0), rD)
	a.Ubfx(rC, rW, 32, 16)
	a.AddLsl(rC, rD, rC, 4)
	a.Store(rCtx, scr(sV1), rC)
	g.closeUps()
	g.next()
}

// closureW is closure_w, as amd64's: nativeClosure(regs, w, pc, ctx)
// called through callH, its func value in R26.
func (g *armGen) closureW() {
	a := g.a
	after := a.NewLabel()
	a.Store(rCtx, ncIP, rIP)
	a.Store(rCtx, ncBudget, rBud)
	a.Adr(r8, after)
	a.Store(rCtx, ncAfter, r8)
	a.MovRR(rC, rW)
	a.Load(rD, rCtx, ncCodeBase)
	a.Op(Sub, rD, rIP, rD, true)
	a.ShiftImm(Lsr, rD, rD, 3, true)
	a.MovRR(rS, rCtx)
	a.MovRR(rW, rRegs)
	a.Load(rFn, rCtx, ncClosureFn)
	a.Load(rJ, rCtx, ncCallH)
	a.Br(rJ)
	a.Bind(after)
	g.reload()
	a.MovRR(r8, rW)
	a.Load(rW, rIP, 0)
	a.Load(r9, rCtx, ncObjBits)
	g.regA(rW)
	g.storeValue()
}

// litCall is new_object_lit and new_array_e, as amd64's: nativeLit
// called through callH, its func value in R26.
func (g *armGen) litCall() {
	a := g.a
	after := a.NewLabel()
	a.Store(rCtx, ncIP, rIP)
	a.Store(rCtx, ncBudget, rBud)
	a.Adr(r8, after)
	a.Store(rCtx, ncAfter, r8)
	a.MovRR(rC, rW)
	a.Load(rD, rCtx, ncCodeBase)
	a.Op(Sub, rD, rIP, rD, true)
	a.ShiftImm(Lsr, rD, rD, 3, true)
	a.MovRR(rS, rCtx)
	a.MovRR(rW, rRegs)
	a.Load(rFn, rCtx, ncLitFn)
	a.Load(rJ, rCtx, ncCallH)
	a.Br(rJ)
	a.Bind(after)
	g.reload()
	g.next()
}
