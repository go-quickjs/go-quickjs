package nativegen

import (
	. "github.com/go-quickjs/go-quickjs/internal/jit/asm/arm64"
)

// A cursor's close and a destructuring's step: amd64_cursor.go's, in
// arm64's instructions.

// iterClose is amd64's iterClose.
func (g *armGen) iterClose() {
	a := g.a
	forIn, done, release, out := a.NewLabel(), a.NewLabel(), a.NewLabel(), a.NewLabel()
	g.regB(rS)
	g.object(rS, rS)
	a.LoadU8(rC, rS, offObjClass)
	a.CmpImm(rC, int64(ClassCursor), false)
	a.BCond(NE, g.exit)
	a.LoadU8(rC, rS, offItForIn)
	a.Cbnz(rC, forIn, false)
	a.Load(r8, rS, offItArr)
	a.LoadU8(rC, rS, offItDone)
	a.Cbnz(rC, done, false)
	// An array's cursor not done: nothing to call.
	a.Cbz(r8, g.exit, true)
	a.Load(rC, rCtx, ncRT)
	a.Load(rC, rC, rtRel(0))
	a.Load(rC, rC, offRealmArrIterCell)
	a.Cbz(rC, g.exit, true)
	a.LoadU8(rC, rC, offCellValid)
	a.Cbz(rC, g.exit, false)
	a.B(release)
	a.Bind(done)
	a.Cbz(r8, out, true)
	a.Bind(release)
	a.MovImm(rC, 1)
	a.Store8(rS, offItReleased, rC)
	a.Bind(out)
	g.next()
	a.Bind(forIn)
	a.MovImm(rC, 1)
	a.Store8(rS, offItDone, rC)
	g.next()
}

// iterStep is amd64's iterStep.
func (g *armGen) iterStep() {
	a := g.a
	end, undef := a.NewLabel(), a.NewLabel()
	g.regB(rS)
	g.object(rS, rS)
	a.LoadU8(rC, rS, offObjClass)
	a.CmpImm(rC, int64(ClassCursor), false)
	a.BCond(NE, g.exit)
	a.LoadU8(rC, rS, offItDone)
	a.Cbnz(rC, undef, false)
	a.Load(r8, rS, offItArr)
	a.Cbz(r8, g.exit, true)
	a.Load(rC, rS, offItIdx)
	a.LoadU16(rD, r8, offObjFlags)
	a.AndImm(rD, rD, uint64(objHasExt|objHasSparseElements), false)
	a.CmpImm(rD, int64(objHasExt), false)
	a.BCond(NE, g.exit)
	g.cmpMem(rC, r8, sizeObj+8)
	a.BCond(HS, end)
	g.dense(rC, r8, 0, g.exit)
	g.incMem(rS, offItIdx, 1)
	g.loadValue(rC)
	g.regA(rW)
	g.storeValue()
	a.Bind(end)
	a.MovImm(rC, 1)
	a.Store8(rS, offItDone, rC)
	a.Bind(undef)
	a.LoadF(0, rCtx, ncUndef)
	g.storeA(0)
}
