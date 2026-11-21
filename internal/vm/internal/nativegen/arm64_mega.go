package nativegen

import . "github.com/go-quickjs/go-quickjs/internal/jit/asm/arm64"

// The megamorphic paths of get_prop and set_prop, as amd64's
// (amd64_mega.go): a site that has given up answered by the realm's caches
// of such sites, laid out after every other code of the block.

// siteKey puts the key the instruction in rW names in dst, as arrayLength
// reads it. r9 is its scratch.
func (g *armGen) siteKey(dst Reg) {
	a := g.a
	a.ShiftImm(Lsr, dst, rW, 48, true)
	a.Load(r9, rCtx, ncRC)
	a.Load(r9, r9, offRCSiteName)
	a.AddLsl(r9, r9, dst, 2)
	a.LoadU32(dst, r9, 0)
	a.Load(r9, rCtx, ncCC)
	a.Load(r9, r9, offCCNames)
	a.AddLsl(r9, r9, dst, 2)
	a.LoadU32(dst, r9, 0)
}

// sharedMix is amd64's: idx the multiplication of the shape in s and the
// key in key the caches are indexed by. tmp is its scratch.
func (g *armGen) sharedMix(idx, s, key, tmp Reg) {
	a := g.a
	a.ShiftImm(Lsr, idx, s, 4, true)
	a.ShiftImm(Lsl, tmp, key, 24, true)
	a.Op(Eor, idx, idx, tmp, true)
	a.MovImm(tmp, 0x9e3779b97f4a7c15)
	a.Mul(idx, idx, tmp)
}

// megaReadSlot is amd64's: readSlot whose misses at a site that has
// given up are a megaRead's of their own.
func (g *armGen) megaReadSlot() {
	a := g.a
	miss, indexed := a.NewLabel(), a.NewLabel()
	g.megaMiss = miss
	g.readSlot()
	ret := g.megaRet
	g.megaMiss, g.megaRet = 0, 0
	g.mega = append(g.mega, func() { g.megaRead(miss, ret, indexed) })
	g.megaIdx = append(g.megaIdx, func() { g.megaReadIndexed(indexed, ret) })
}

// megaRead is amd64's: get_prop's miss at a site that has given up, the
// object in rS, answered by the realm's shared cache or the object's own
// table, back at ret with the value's address in rC; anything else exits. rW keeps the instruction;
// r8, r9, rD, rT and rK are its scratch.
func (g *armGen) megaRead(miss, ret, indexed Label) {
	a := g.a
	found, nocell, p1, have := a.NewLabel(), a.NewLabel(), a.NewLabel(), a.NewLabel()
	own, ownLoop, ownNext := a.NewLabel(), a.NewLabel(), a.NewLabel()
	esz := size("sharedEntry")
	a.Bind(miss)
	g.site(rC)
	a.LoadU8(r8, rC, off("propCache.fills"))
	a.CmpImm(r8, int64(maxCacheFills), false)
	a.BCond(LO, g.exit)
	g.siteKey(rD)
	a.ShiftImm(Lsr, r9, rD, 31, false)
	a.Cbnz(r9, g.exit, false)
	a.Load(r8, rS, off("Object.shape"))
	a.Cbz(r8, g.exit, true)
	g.sharedMix(r9, r8, rD, rK)
	a.ShiftImm(Lsr, r9, r9, 64-sharedCacheBits, true)
	a.AndImm(r9, r9, ^uint64(1), true)
	a.MovImm(rK, uint64(esz))
	a.Mul(r9, r9, rK)
	a.Load(rC, rCtx, ncRT)
	a.Load(rC, rC, rtRel(0))
	a.Load(rC, rC, offRealmShared)
	a.Cbz(rC, own, true)
	a.Op(Add, r9, r9, rC, true)
	for k := int32(0); k < 2; k++ {
		next := a.NewLabel()
		a.Load(rC, r9, k*esz+off("sharedEntry.shape"))
		a.Cmp(r8, rC, true)
		a.BCond(NE, next)
		a.LoadU32(rC, r9, k*esz+off("sharedEntry.key"))
		a.Cmp(rC, rD, false)
		a.BCond(NE, next)
		if k != 0 {
			a.AddImm(r9, r9, int64(esz), true)
		}
		a.B(found)
		a.Bind(next)
	}
	a.B(own)
	a.Bind(found)
	a.Load(rC, r9, off("sharedEntry.cell"))
	a.Cbz(rC, nocell, true)
	a.LoadU8(rC, rC, off("protoCell.valid"))
	a.Cbz(rC, g.exit, false)
	a.Load(rS, r9, off("sharedEntry.p2"))
	a.Cbz(rS, g.exit, true)
	a.B(have)
	a.Bind(nocell)
	a.Load(rC, r9, off("sharedEntry.p1"))
	a.Cbz(rC, have, true)
	a.Load(r8, rC, off("Object.shape"))
	a.Load(rK, r9, off("sharedEntry.s1"))
	a.Cmp(r8, rK, true)
	a.BCond(NE, g.exit)
	a.Load(r8, r9, off("sharedEntry.p2"))
	a.Cbz(r8, p1, true)
	a.Load(rD, r8, off("Object.shape"))
	a.Load(rK, r9, off("sharedEntry.s2"))
	a.Cmp(rD, rK, true)
	a.BCond(NE, g.exit)
	a.MovRR(rS, r8)
	a.B(have)
	a.Bind(p1)
	a.MovRR(rS, rC)
	a.Bind(have)
	a.LoadS32(rC, r9, off("sharedEntry.idx"))
	a.ShiftImm(Lsl, rC, rC, 4, true)
	a.Load(rK, rS, off("Object.valp"))
	a.Op(Add, rC, rC, rK, true)
	a.B(ret)
	// As amd64's: none for the shape and the key, the object's own plain
	// data property of a table of no index.
	a.Bind(own)
	a.LoadU8(r9, rS, offObjClass)
	a.CmpImm(r9, int64(ClassObject), false)
	a.BCond(NE, g.exit)
	a.Load(r9, r8, off("shape.index"))
	a.Cbnz(r9, indexed, true)
	a.LoadU32(r9, rS, off("Object.valn"))
	a.Load(rC, r8, off("shape.keys"))
	a.Bind(ownLoop)
	a.Cbz(r9, g.exit, false)
	a.LoadU32(r8, rC, off("propKey.key"))
	a.Cmp(r8, rD, false)
	a.BCond(NE, ownNext)
	a.LoadU8(r8, rC, off("propKey.flags"))
	a.MovImm(rK, uint64(uint32(propOddKey)))
	a.Tst(r8, rK, false)
	a.BCond(NE, g.exit)
	a.LoadU32(rC, rS, off("Object.valn"))
	a.Op(Sub, rC, rC, r9, true)
	a.ShiftImm(Lsl, rC, rC, 4, true)
	a.Load(rK, rS, off("Object.valp"))
	a.Op(Add, rC, rC, rK, true)
	a.B(ret)
	a.Bind(ownNext)
	a.AddImm(rC, rC, int64(size("propKey")), true)
	a.SubsImm(r9, r9, 1, false)
	a.B(ownLoop)
}

// megaWrite is amd64's: set_prop's miss at a site that has given up, an
// ordinary object in rS and the value's address in rD -- an own plain
// writable data property of a table of no index written in place, or the
// realm's cache's addition made by addTransition's store at store, the
// next shape in r8. Anything else exits. r8, r9, rC, rT, rK and rW are its
// scratch, and the scratch's sI keeps the value's address.
func (g *armGen) megaWrite(miss, store, indexed, deep Label) {
	a := g.a
	scan, scanNext, found, chain, end := a.NewLabel(), a.NewLabel(), a.NewLabel(), a.NewLabel(), a.NewLabel()
	dsz := size("deepAdd")
	ksz := size("propKey")
	a.Bind(miss)
	g.site(rC)
	a.LoadU8(r8, rC, off("propCache.fills"))
	a.CmpImm(r8, int64(maxCacheFills), false)
	a.BCond(LO, g.exit)
	a.LoadU8(r8, rS, offObjClass)
	a.CmpImm(r8, int64(ClassObject), false)
	a.BCond(NE, g.exit)
	g.siteKey(rC)
	a.ShiftImm(Lsr, r9, rC, 31, false)
	a.Cbnz(r9, g.exit, false)
	a.Load(r8, rS, off("Object.shape"))
	a.Cbz(r8, g.exit, true)
	// Its own, in a table of no index: the keys scanned.
	a.Load(r9, r8, off("shape.index"))
	a.Cbnz(r9, indexed, true)
	a.LoadU32(r9, rS, off("Object.valn"))
	a.Load(rK, r8, off("shape.keys"))
	a.Bind(scan)
	a.Cbz(r9, deep, false)
	a.LoadU32(rT, rK, off("propKey.key"))
	a.Cmp(rT, rC, false)
	a.BCond(NE, scanNext)
	a.LoadU8(rT, rK, off("propKey.flags"))
	a.MovImm(r8, uint64(uint32(propOddKey|propWritable)))
	a.Op(And, rT, rT, r8, false)
	a.CmpImm(rT, int64(propWritable), false)
	a.BCond(NE, g.exit)
	a.Load(r8, rS, off("Object.shape"))
	a.LoadU8(r8, r8, off("shape.pristine"))
	a.Cbnz(r8, g.exit, false)
	a.LoadU32(rW, rS, off("Object.valn"))
	a.Op(Sub, rW, rW, r9, true)
	a.ShiftImm(Lsl, rW, rW, 4, true)
	g.addMem(rW, rS, off("Object.valp"))
	g.loadValue(rD)
	g.storeValue()
	a.Bind(scanNext)
	a.AddImm(rK, rK, int64(ksz), true)
	a.SubsImm(r9, r9, 1, false)
	a.B(scan)
	// An addition the realm's cache holds, as amd64's: the object's flags
	// first.
	a.Bind(deep)
	a.LoadU16(rK, rS, offObjFlags)
	a.MovImm(rT, uint64(objExtensible|objPolyProto))
	a.Op(And, rK, rK, rT, false)
	a.CmpImm(rK, int64(objExtensible), false)
	a.BCond(NE, g.exit)
	a.Load(r8, rS, off("Object.shape"))
	g.sharedMix(r9, r8, rC, rK)
	a.ShiftImm(Lsr, r9, r9, 64-sharedCacheBits+deepWaysLog, true)
	a.ShiftImm(Lsl, r9, r9, deepWaysLog, true)
	a.MovImm(rK, uint64(dsz))
	a.Mul(r9, r9, rK)
	a.Load(rK, rCtx, ncRT)
	a.Load(rK, rK, rtRel(0))
	a.Load(rK, rK, offRealmDeep)
	a.Cbz(rK, g.exit, true)
	a.Op(Add, r9, r9, rK, true)
	for w := int32(0); w < 1<<deepWaysLog; w++ {
		next := a.NewLabel()
		a.LoadU32(rK, r9, w*dsz+off("deepAdd.key"))
		a.Cmp(rK, rC, false)
		a.BCond(NE, next)
		a.Load(rK, r9, w*dsz+off("deepAdd.from"))
		a.Cmp(r8, rK, true)
		a.BCond(NE, next)
		if w != 0 {
			a.AddImm(r9, r9, int64(w*dsz), true)
		}
		a.B(found)
		a.Bind(next)
	}
	a.B(g.exit)
	a.Bind(found)
	a.Store(rCtx, scr(sI), rD)
	a.Load(rW, r8, off("shape.proto"))
	a.LoadS32(rC, r9, off("deepAdd.n"))
	a.AddImm(r8, r9, int64(off("deepAdd.up")), true)
	a.Bind(chain)
	a.Cbz(rC, end, false)
	a.Load(rK, r8, 0)
	a.Cmp(rW, rK, true)
	a.BCond(NE, g.exit)
	a.Load(rD, rW, off("Object.shape"))
	a.Load(rK, r8, off("deepAdd.shapes")-off("deepAdd.up"))
	a.Cmp(rD, rK, true)
	a.BCond(NE, g.exit)
	a.LoadU16(rD, rW, offObjFlags)
	a.MovImm(rT, uint64(objPolyProto))
	a.Tst(rD, rT, false)
	a.BCond(NE, g.exit)
	a.Load(rW, rW, off("Object.shape"))
	a.Load(rW, rW, off("shape.proto"))
	a.AddImm(r8, r8, 8, true)
	a.SubsImm(rC, rC, 1, false)
	a.B(chain)
	a.Bind(end)
	a.Cbnz(rW, g.exit, true)
	a.Load(r8, r9, off("deepAdd.next"))
	a.Load(rD, rCtx, scr(sI))
	a.B(store)
}

// indexFind is amd64's: vm's findOwn of the key in key in the table of the
// object in rS, whose layout has an index -- its recent entry, else its
// slots, what they say remembered in the recent entry. At found, i is the
// property's position and s the address of its key among the layout's;
// at absent, the object has none of the key. key is kept; s, i, t, u and v
// are its scratch.
func (g *armGen) indexFind(key, s, i, t, u, v Reg, found, absent Label) {
	a := g.a
	slots, loop, hit, none, remember := a.NewLabel(), a.NewLabel(), a.NewLabel(), a.NewLabel(), a.NewLabel()
	recent := off("propIndex.recent")
	e := off("propIndex.slots") + off("slotTable.e")
	if size("recentKey") != 8 || size("slotEntry") != 8 || size("propKey") != 8 ||
		off("recentKey.key") != 0 || off("slotEntry.key") != 0 || off("slotEntry.idx") != 4 {
		panic("nativegen: an index's entries are not as indexFind reads them")
	}
	// The recent entry, confirmed by the table's key there and its length.
	a.Load(t, rS, off("Object.shape"))
	a.Load(t, t, offShapeIndex)
	a.AndImm(i, key, 15, false)
	a.AddLsl(s, t, i, 3)
	a.AddImm(s, s, int64(recent), true)
	a.LoadU32(u, s, off("recentKey.key"))
	a.Cmp(u, key, false)
	a.BCond(NE, slots)
	a.LoadS32(i, s, off("recentKey.idx"))
	a.CmpImm(i, 0, true)
	a.BCond(LT, absent)
	a.LoadU32(t, rS, off("Object.valn"))
	a.Cmp(i, t, true)
	a.BCond(HS, absent)
	a.Load(s, rS, off("Object.shape"))
	a.Load(s, s, off("shape.keys"))
	a.AddLsl(s, s, i, 3)
	a.LoadU32(u, s, off("propKey.key"))
	a.Cmp(u, key, false)
	a.BCond(NE, slots)
	a.LoadU8(u, s, off("propKey.flags"))
	a.TstImm(u, uint64(uint32(propDeleted)), false)
	a.BCond(NE, slots)
	a.B(found)
	// The slots, from the key's home on: s their base, t their mask, u
	// the entry's address.
	a.Bind(slots)
	a.Load(t, rS, off("Object.shape"))
	a.Load(t, t, offShapeIndex)
	a.Load(s, t, e)
	a.Load(t, t, e+8)
	a.SubsImm(t, t, 1, true)
	a.MovImm(i, slotHomeMul)
	a.MulW(i, i, key)
	a.ShiftImm(Lsr, i, i, uint8(slotHomeShift), false)
	a.Bind(loop)
	a.Op(And, i, i, t, true)
	a.AddLsl(u, s, i, 3)
	a.LoadU32(v, u, off("slotEntry.key"))
	a.Cmp(v, key, false)
	a.BCond(EQ, hit)
	a.LoadS32(v, u, off("slotEntry.idx"))
	a.CmpImm(v, int64(slotEmpty), false)
	a.BCond(EQ, none)
	a.AddImm(i, i, 1, true)
	a.B(loop)
	a.Bind(hit)
	a.LoadS32(i, u, off("slotEntry.idx"))
	a.CmpImm(i, 0, true)
	a.BCond(GE, remember)
	a.Bind(none)
	a.MovImm(i, ^uint64(0))
	// Remembered, then checked against the table, as amd64's.
	a.Bind(remember)
	a.Load(t, rS, off("Object.shape"))
	a.Load(t, t, offShapeIndex)
	a.AndImm(u, key, 15, false)
	a.AddLsl(t, t, u, 3)
	a.Store32(t, recent+off("recentKey.key"), key)
	a.Store32(t, recent+off("recentKey.idx"), i)
	a.CmpImm(i, 0, true)
	a.BCond(LT, absent)
	a.LoadU32(t, rS, off("Object.valn"))
	a.Cmp(i, t, true)
	a.BCond(HS, absent)
	a.Load(s, rS, off("Object.shape"))
	a.Load(s, s, off("shape.keys"))
	a.AddLsl(s, s, i, 3)
	a.LoadU32(u, s, off("propKey.key"))
	a.Cmp(u, key, false)
	a.BCond(NE, absent)
	a.LoadU8(u, s, off("propKey.flags"))
	a.TstImm(u, uint64(uint32(propDeleted)), false)
	a.BCond(NE, absent)
	a.B(found)
}

// megaReadIndexed is amd64's: megaRead's own property of a table that has
// an index, the object in rS and the key in rD, back at ret with the
// value's address in rC.
func (g *armGen) megaReadIndexed(indexed, ret Label) {
	a := g.a
	found := a.NewLabel()
	a.Bind(indexed)
	g.indexFind(rD, r8, rC, r9, rT, rK, found, g.exit)
	a.Bind(found)
	a.LoadU8(r9, r8, off("propKey.flags"))
	a.TstImm(r9, uint64(uint32(propOddKey)), false)
	a.BCond(NE, g.exit)
	a.ShiftImm(Lsl, rC, rC, 4, true)
	a.Load(r9, rS, off("Object.valp"))
	a.Op(Add, rC, rC, r9, true)
	a.B(ret)
}

// megaWriteIndexed is amd64's: megaWrite's own property of a table that
// has an index, the object in rS, the key in rC and the value's address in
// rD, written in place; a key the object has not goes on to deep.
func (g *armGen) megaWriteIndexed(indexed, deep Label) {
	a := g.a
	found := a.NewLabel()
	a.Bind(indexed)
	g.indexFind(rC, r8, rW, r9, rT, rK, found, deep)
	a.Bind(found)
	a.LoadU8(r9, r8, off("propKey.flags"))
	a.MovImm(rT, uint64(uint32(propOddKey|propWritable)))
	a.Op(And, r9, r9, rT, false)
	a.CmpImm(r9, int64(propWritable), false)
	a.BCond(NE, g.exit)
	a.Load(r8, rS, off("Object.shape"))
	a.LoadU8(r8, r8, off("shape.pristine"))
	a.Cbnz(r8, g.exit, false)
	a.ShiftImm(Lsl, rW, rW, 4, true)
	g.addMem(rW, rS, off("Object.valp"))
	g.loadValue(rD)
	g.storeValue()
}
