package nativegen

import . "github.com/go-quickjs/go-quickjs/internal/jit/asm/arm64"

// polyOwnStore is amd64's probe, retaining the source address in rD: an
// entry of the object's own slot goes to own, one of an addition to add
// with the entry in rC; a site with no other shapes, or none of the
// object's, goes to none.
func (g *armGen) polyOwnStore(start, own, add, none Label) {
	a := g.a
	loop, found, trans, next := a.NewLabel(), a.NewLabel(), a.NewLabel(), a.NewLabel()
	a.Bind(start)
	a.Load(r8, rC, off("propCache.poly"))
	a.Cbz(r8, none, true)
	a.LoadU8(r9, r8, off("propPoly.n"))
	a.Cbz(r9, none, false)
	g.addMem(r8, rCtx, ncPolyEnt)
	a.Load(rC, rS, off("Object.shape"))
	a.Bind(loop)
	g.cmpMem(rC, r8, off("propCache.ownShape"))
	a.BCond(EQ, found)
	g.cmpMem(rC, r8, off("propCache.shape"))
	a.BCond(EQ, trans)
	a.Bind(next)
	a.AddImm(r8, r8, int64(size("propCache")), true)
	a.SubsImm(r9, r9, 1, false)
	a.BCond(NE, loop)
	a.B(none)
	a.Bind(found)
	a.MovRR(rC, r8)
	a.B(own)
	// As amd64's: an addition's entry, where it has a transition.
	a.Bind(trans)
	a.Load(rT, r8, off("propCache.next"))
	a.Cbz(rT, next, true)
	a.MovRR(rC, r8)
	a.B(add)
}

// getPropNext reads two adjacent data properties without storing the
// intermediate temporary. A miss leaves both reads to their Go handlers.
func (g *armGen) getPropNext() {
	a := g.a
	g.regB(rD)
	g.object(rS, rD)
	for i := 0; i < 2; i++ {
		g.site(rC)
		g.readSlot()
		if i == 0 {
			a.Load(r9, rC, 0)
			g.cmpMem(r9, rCtx, ncObjBits)
			a.BCond(NE, g.exit)
			a.Load(rS, rC, 8)
			a.Load(rW, rIP, 8)
		}
	}
	g.loadValue(rC)
	g.regA(rW)
	a.AddImm(rIP, rIP, 8, true)
	g.storeValue()
}

// arrayLength is amd64's set_prop of an array's own length: the object in
// rS, the value's address in rD, the instruction in rW.
//
// Any other class goes to other, as amd64's.
func (g *armGen) arrayLength(start, other Label) {
	a := g.a
	loop, cleared, done := a.NewLabel(), a.NewLabel(), a.NewLabel()
	a.Bind(start)
	a.LoadU8(r8, rS, offObjClass)
	a.CmpImm(r8, int64(ClassArray), false)
	a.BCond(NE, other)
	// The name: names[siteName[C]].
	a.ShiftImm(Lsr, rC, rW, 48, true)
	a.Load(r9, rCtx, ncRC)
	a.Load(r9, r9, offRCSiteName)
	a.AddLsl(r9, r9, rC, 2)
	a.LoadU32(rC, r9, 0)
	a.Load(r9, rCtx, ncCC)
	a.Load(r9, r9, offCCNames)
	a.AddLsl(r9, r9, rC, 2)
	a.LoadU32(rC, r9, 0)
	a.MovImm(rT, uint64(uint32(atomLength)))
	a.Cmp(rC, rT, false)
	a.BCond(NE, g.exit)
	want := objArrayLengthWritable | objHasExt
	a.LoadU16(r8, rS, offObjFlags)
	a.AndImm(r8, r8, uint64(want|objHasSparseElements|objSharedElems), false)
	a.CmpImm(r8, int64(want), false)
	a.BCond(NE, g.exit)
	// The value: a number whose uint32 is itself.
	a.Load(r9, rD, 0)
	a.Cmp(r9, rTag, true)
	a.BCond(HS, g.exit)
	a.FMovToF(1, r9)
	a.Fcvtzs(r9, 1)
	a.ShiftImm(Lsr, rT, r9, 32, true)
	a.Cbnz(rT, g.exit, true)
	a.Scvtf(3, r9, true)
	a.FCmp(1, 3)
	a.BCond(NE, g.exit)
	// No longer than the elements: growing is Go's.
	g.cmpMem(r9, rS, sizeObj+8)
	a.BCond(HI, g.exit)
	g.noIndexKeys(rS)
	// From the last element down to the new length, as amd64's.
	a.Store(rCtx, scr(sE), rS)
	a.Store(rCtx, scr(sRC), r9)
	a.Bind(loop)
	a.Load(rS, rCtx, scr(sE))
	a.Load(r9, rS, sizeObj+8)
	g.cmpMem(r9, rCtx, scr(sRC))
	a.BCond(LS, done)
	a.AddImm(r9, r9, -1, true)
	a.ShiftImm(Lsl, rW, r9, 4, true)
	g.addMem(rW, rS, sizeObj)
	a.Store(rW, 0, ZR)
	a.Load(rC, rW, 8)
	a.Cbz(rC, cleared, true)
	a.AddImm(rW, rW, 8, true)
	a.MovRR(rC, ZR)
	g.ptrStore()
	a.Load(rS, rCtx, scr(sE))
	a.Load(r9, rS, sizeObj+8)
	a.AddImm(r9, r9, -1, true)
	a.Bind(cleared)
	a.Store(rS, sizeObj+8, r9)
	a.B(loop)
	a.Bind(done)
	g.next()
}
