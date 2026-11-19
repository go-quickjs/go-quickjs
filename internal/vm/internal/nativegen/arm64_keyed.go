package nativegen

import . "github.com/go-quickjs/go-quickjs/internal/jit/asm/arm64"

// keyedStore is amd64's: set_index's and define_index's store of a key
// string by the site's keyed store cache. The object is in rS, the key's
// address in r8 and its number word in r9; the key's atom is kept in rK
// and the poly's count in rJ.
func (g *armGen) keyedStore() {
	a := g.a
	poly, loop, next, own, trans := a.NewLabel(), a.NewLabel(), a.NewLabel(), a.NewLabel(), a.NewLabel()
	// The key, its atom in rK.
	g.kindIs(r9, KindString)
	a.Load(r9, r8, 8)
	g.keySet(r8, r9)
	g.keyFind(r8, r9, g.exit)
	a.LoadU8(rC, r8, offKANone)
	a.Cbnz(rC, g.exit, false)
	a.LoadU32(rK, r8, offKAAtom)
	a.ShiftImm(Lsr, rC, rK, 31, false)
	a.Cbnz(rC, g.exit, false)
	// The site, Aux[pc]: &ic[Aux[pc]], in rC.
	a.Load(r8, rCtx, ncRC)
	a.Load(r8, r8, offRCAux)
	a.Cbz(r8, g.exit, true)
	a.Load(r9, rCtx, ncCodeBase)
	a.Op(Sub, rC, rIP, r9, true)
	a.ShiftImm(Lsr, rC, rC, 1, true)
	a.Op(Add, r8, r8, rC, true)
	a.LoadU32(rC, r8, 0)
	a.Cbz(rC, g.exit, false)
	a.MovImm(rT, uint64(size("propCache")))
	a.Mul(rC, rC, rT)
	a.Op(Add, rC, rC, rIC, true)
	// The object's shape, in rD.
	a.Load(rD, rS, off("Object.shape"))
	a.Cbz(rD, g.exit, true)
	g.keyedEntry(own, trans, poly)
	// The poly's entries, their count left in rJ.
	a.Bind(poly)
	a.Load(r8, rC, off("propCache.poly"))
	a.Cbz(r8, g.exit, true)
	a.LoadU8(rJ, r8, off("propPoly.n"))
	a.Cbz(rJ, g.exit, false)
	g.addMem(r8, rCtx, ncPolyEnt)
	a.MovRR(rC, r8)
	a.Bind(loop)
	g.keyedEntry(own, trans, next)
	a.Bind(next)
	a.AddImm(rC, rC, int64(size("propCache")), true)
	a.SubsImm(rJ, rJ, 1, true)
	a.BCond(NE, loop)
	a.B(g.exit)
	// An own slot: the value at idx.
	a.Bind(own)
	g.regC(r8)
	g.loadValue(r8)
	a.LoadS32(rW, rC, off("propCache.idx"))
	a.ShiftImm(Lsl, rW, rW, 4, true)
	g.addMem(rW, rS, off("Object.valp"))
	g.storeValue()
	// A transition: the value at the address in rD added.
	a.Bind(trans)
	g.regC(rD)
	g.addTransition()
}

// keyedEntry is amd64's: to own or trans where the entry at rC is of the
// object's shape, in rD, and of the key whose atom is in rK, to miss
// otherwise. r8, r9 and rT are its scratch.
func (g *armGen) keyedEntry(own, trans, miss Label) {
	a := g.a
	ownKey := a.NewLabel()
	g.cmpMem(rD, rC, off("propCache.shape"))
	a.BCond(NE, miss)
	a.Load(r8, rC, off("propCache.next"))
	a.Cbz(r8, ownKey, true)
	g.cmpMem(r8, rCtx, ncSetterNx)
	a.BCond(EQ, miss)
	a.LoadU32(r9, r8, off("shape.key"))
	a.Cmp(r9, rK, false)
	a.BCond(EQ, trans)
	a.B(miss)
	a.Bind(ownKey)
	a.LoadS32(r9, rC, off("propCache.idx"))
	a.Load(r8, rD, off("shape.keys"))
	a.MovImm(rT, uint64(size("propKey")))
	a.Mul(r9, r9, rT)
	a.Op(Add, r8, r8, r9, true)
	a.LoadU32(r9, r8, off("propKey.key"))
	a.Cmp(r9, rK, false)
	a.BCond(EQ, own)
	a.B(miss)
}
