package nativegen

import (
	. "github.com/go-quickjs/go-quickjs/internal/jit/asm/amd64"
)

// Closures and the upvalues they capture (docs/register-interpreter.md,
// "Closures and upvalues"): close_regs and a return's closing of the
// upvalues open on a frame, done here as regCloseRegs and
// regCloseUpvalues do them, and closure_w's closure, made by Go's
// regClosure through a call of nativeClosure with no exit.

// closeUps closes the upvalues open on the top frame whose slots lie in
// [scratch sV0, scratch sV1), as regCloseRegs does: each one's value
// copied into it and its slot pointed at it, the others kept in order and
// the list's room past them cleared. Every pointer is stored by ptrTo,
// so that its loop's state is in the scratch: sI the upvalue at hand, sEnd
// how many there were, sKa how many are kept, sE the one being closed.
func (g *nativeGen) closeUps() {
	a := g.a
	loop, keep, moved, next, done, clr, clrDone := a.NewLabel(), a.NewLabel(), a.NewLabel(), a.NewLabel(), a.NewLabel(), a.NewLabel(), a.NewLabel()
	a.StoreImm(RDI, scr(sI), 0)
	a.StoreImm(RDI, scr(sKa), 0)
	a.Load(RSI, RDI, ncNF)
	a.Load(RCX, RSI, offFOpenUp+8)
	a.Store(RDI, scr(sEnd), RCX)
	a.Bind(loop)
	a.Load(RCX, RDI, scr(sI))
	a.CmpMem(RCX, RDI, scr(sEnd), true)
	a.Jcc(CondGE, done)
	a.Load(RSI, RDI, ncNF)
	a.Load(RDX, RSI, offFOpenUp)
	a.LoadIdx(R8, RDX, RCX, 8, 0)
	a.Load(R9, R8, offUpSlot)
	a.CmpMem(R9, RDI, scr(sV0), true)
	a.Jcc(CondB, keep)
	a.CmpMem(R9, RDI, scr(sV1), true)
	a.Jcc(CondAE, keep)
	// u.closed = *u.slot; u.slot = &u.closed.
	a.Store(RDI, scr(sE), R8)
	a.Load(RCX, R9, 0)
	a.Store(R8, offUpClosed, RCX)
	a.Load(RCX, R9, 8)
	a.Lea(RAX, R8, offUpClosed+8)
	g.ptrTo()
	a.Load(R8, RDI, scr(sE))
	a.Lea(RCX, R8, offUpClosed)
	a.Lea(RAX, R8, offUpSlot)
	g.ptrStore()
	a.Jmp(next)
	// Kept: at the list's next kept place.
	a.Bind(keep)
	a.Load(RCX, RDI, scr(sKa))
	a.CmpMem(RCX, RDI, scr(sI), true)
	a.Jcc(CondE, moved)
	a.LeaIdx(RAX, RDX, RCX, 8, 0)
	a.MovRR(RCX, R8)
	g.ptrTo()
	a.Bind(moved)
	a.Load(RCX, RDI, scr(sKa))
	a.OpImm(Add, RCX, 1, true)
	a.Store(RDI, scr(sKa), RCX)
	a.Bind(next)
	a.Load(RCX, RDI, scr(sI))
	a.OpImm(Add, RCX, 1, true)
	a.Store(RDI, scr(sI), RCX)
	a.Jmp(loop)
	// The room past the kept cleared, as clear clears it; the length the
	// kept's.
	a.Bind(done)
	a.Load(RCX, RDI, scr(sKa))
	a.Store(RDI, scr(sI), RCX)
	a.Bind(clr)
	a.Load(RCX, RDI, scr(sI))
	a.CmpMem(RCX, RDI, scr(sEnd), true)
	a.Jcc(CondGE, clrDone)
	a.Load(RSI, RDI, ncNF)
	a.Load(RDX, RSI, offFOpenUp)
	a.LeaIdx(RAX, RDX, RCX, 8, 0)
	a.Op(Xor, RCX, RCX, true)
	g.ptrTo()
	a.Load(RCX, RDI, scr(sI))
	a.OpImm(Add, RCX, 1, true)
	a.Store(RDI, scr(sI), RCX)
	a.Jmp(clr)
	a.Bind(clrDone)
	a.Load(RSI, RDI, ncNF)
	a.Load(RCX, RDI, scr(sKa))
	a.Store(RSI, offFOpenUp+8, RCX)
}

// closeRegs is close_regs: the upvalues open on the B registers from
// R[A] closed (closeUps).
func (g *nativeGen) closeRegs() {
	a := g.a
	g.decA(RDX)
	a.Op(Add, RDX, R12, true)
	a.Store(RDI, scr(sV0), RDX)
	a.MovRR(RCX, RAX)
	a.ShiftImm(Shr, RCX, 32, true)
	a.OpImm(And, RCX, 0xFFFF, false)
	a.ShiftImm(Shl, RCX, 4, true)
	a.Op(Add, RCX, RDX, true)
	a.Store(RDI, scr(sV1), RCX)
	g.closeUps()
	g.next()
}

// closureW is closure_w: nativeClosure called through callH, as an exit
// calls a Go handler but with no exit's state -- the instruction and the
// budget kept in the context, the pinned registers put back after -- and
// the closure it makes stored in R[A].
func (g *nativeGen) closureW() {
	a := g.a
	after := a.NewLabel()
	a.Store(RDI, ncIP, RBX)
	a.Store(RDI, ncBudget, R11)
	a.LeaLabel(R8, after)
	a.Store(RDI, ncAfter, R8)
	// nativeClosure(regs, w, pc, ctx): the func value in DX.
	a.MovRR(RBX, RAX)
	a.Load(RCX, RDI, ncIP)
	a.Load(RDX, RDI, ncCodeBase)
	a.Op(Sub, RCX, RDX, true)
	a.ShiftImm(Shr, RCX, 3, true)
	a.MovRR(RSI, RDI)
	a.MovRR(RAX, R12)
	a.Load(RDX, RDI, ncClosureFn)
	a.JmpMem(RDI, ncCallH)
	a.Bind(after)
	g.reload()
	a.MovRR(R8, RAX)
	a.Load(RAX, RBX, 0)
	a.Load(R9, RDI, ncObjBits)
	g.decA(RDX)
	a.LeaIdx(RAX, R12, RDX, 1, 0)
	g.storeValue()
}

// litCall is new_object_lit and new_array_e: nativeLit(regs, w, pc, ctx)
// called through callH as closureW calls nativeClosure, with no exit's
// state, Go storing the literal it makes in R[A]; the pinned registers
// put back after, and on.
func (g *nativeGen) litCall() {
	a := g.a
	after := a.NewLabel()
	a.Store(RDI, ncIP, RBX)
	a.Store(RDI, ncBudget, R11)
	a.LeaLabel(R8, after)
	a.Store(RDI, ncAfter, R8)
	a.MovRR(RBX, RAX)
	a.Load(RCX, RDI, ncIP)
	a.Load(RDX, RDI, ncCodeBase)
	a.Op(Sub, RCX, RDX, true)
	a.ShiftImm(Shr, RCX, 3, true)
	a.MovRR(RSI, RDI)
	a.MovRR(RAX, R12)
	a.Load(RDX, RDI, ncLitFn)
	a.JmpMem(RDI, ncCallH)
	a.Bind(after)
	g.reload()
	g.next()
}
