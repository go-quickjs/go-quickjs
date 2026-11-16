package nativegen

import (
	. "github.com/go-quickjs/go-quickjs/internal/jit/asm/amd64"
)

// A cursor's close (iter_close) and a destructuring's step (iter_step),
// natively, as rhIterClose and regIterStep make them
// (docs/register-interpreter.md, "Native handlers").

// iterClose is iter_close of the cursor in register B: a for-in's marked
// done, so that the next loop takes it (newForInObject); an array's, done
// or with nothing up %ArrayIteratorPrototype%'s chain to call -- the
// realm's cell of that answer valid (arrayIterNoReturn) -- released, so
// that the next cursor over an array takes it (arrCursor); any other done
// cursor left as it is. Anything else is Go's.
func (g *nativeGen) iterClose() {
	a := g.a
	forIn, done, release, out := a.NewLabel(), a.NewLabel(), a.NewLabel(), a.NewLabel()
	g.decB(RSI)
	g.object(RSI, RSI)
	a.LoadU8(RCX, RSI, offObjClass)
	a.OpImm(Cmp, RCX, int32(ClassCursor), false)
	a.Jcc(CondNE, g.exit)
	a.LoadU8(RCX, RSI, offItForIn)
	a.Op(Test, RCX, RCX, false)
	a.Jcc(CondNE, forIn)
	a.Load(R8, RSI, offItArr)
	a.LoadU8(RCX, RSI, offItDone)
	a.Op(Test, RCX, RCX, false)
	a.Jcc(CondNE, done)
	// An array's cursor not done: nothing to call.
	a.Op(Test, R8, R8, true)
	a.Jcc(CondE, g.exit)
	a.Load(RCX, RDI, ncRT)
	a.Load(RCX, RCX, rtRel(0))
	a.Load(RCX, RCX, offRealmArrIterCell)
	a.Op(Test, RCX, RCX, true)
	a.Jcc(CondE, g.exit)
	a.LoadU8(RCX, RCX, offCellValid)
	a.Op(Test, RCX, RCX, false)
	a.Jcc(CondE, g.exit)
	a.Jmp(release)
	a.Bind(done)
	a.Op(Test, R8, R8, true)
	a.Jcc(CondE, out)
	a.Bind(release)
	a.MovImm(RCX, 1)
	a.Store8(RSI, offItReleased, RCX)
	a.Bind(out)
	g.next()
	a.Bind(forIn)
	a.MovImm(RCX, 1)
	a.Store8(RSI, offItDone, RCX)
	g.next()
}

// iterStep is iter_step, R[A] the next value of the cursor in register
// B, undefined once it is done: an array's by the intrinsic iterator
// (iterState.arr), its next element but a hole, the cursor's index
// stepped, or past the end of an array of no sparse elements, its length,
// the cursor done, as iterNext steps it. Anything else is Go's.
func (g *nativeGen) iterStep() {
	a := g.a
	end, undef := a.NewLabel(), a.NewLabel()
	g.decB(RSI)
	g.object(RSI, RSI)
	a.LoadU8(RCX, RSI, offObjClass)
	a.OpImm(Cmp, RCX, int32(ClassCursor), false)
	a.Jcc(CondNE, g.exit)
	a.LoadU8(RCX, RSI, offItDone)
	a.Op(Test, RCX, RCX, false)
	a.Jcc(CondNE, undef)
	a.Load(R8, RSI, offItArr)
	a.Op(Test, R8, R8, true)
	a.Jcc(CondE, g.exit)
	a.Load(RCX, RSI, offItIdx)
	a.LoadU16(RDX, R8, offObjFlags)
	a.OpImm(And, RDX, int32(objHasExt|objHasSparseElements), false)
	a.OpImm(Cmp, RDX, int32(objHasExt), false)
	a.Jcc(CondNE, g.exit)
	a.CmpMem(RCX, R8, sizeObj+8, true)
	a.Jcc(CondAE, end)
	g.dense(RCX, R8, 0, g.exit)
	a.IncMem(RSI, offItIdx)
	g.loadValue(RCX)
	g.decA(RDX)
	a.LeaIdx(RAX, R12, RDX, 1, 0)
	g.storeValue()
	a.Bind(end)
	a.MovImm(RCX, 1)
	a.Store8(RSI, offItDone, RCX)
	a.Bind(undef)
	a.LoadSD(0, RDI, ncUndef)
	g.storeA(0)
}
