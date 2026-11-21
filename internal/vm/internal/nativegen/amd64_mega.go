package nativegen

import . "github.com/go-quickjs/go-quickjs/internal/jit/asm/amd64"

// The megamorphic paths of get_prop and set_prop: a site whose cache has
// given up (maxCacheFills) answered by the realm's caches of such sites,
// which Go fills as it makes their reads and writes -- the reads' by the
// object's shape and the key (vm's sharedProp, sharedCache), the
// additions' with the chain they were made along (vm's deepStore,
// deepAdds) -- as V8's interpreter's handlers probe its stub cache. A
// site that has not given up, or one the caches say nothing of, exits as
// before. They are laid out after every other code of the block, so that
// they move none of it: the handlers' misses reach them by jumps that had
// gone to the exit, of the same length.

// siteKey puts the key the instruction in RAX names, names[siteName[C]],
// in dst, as arrayLength reads it. R9 is its scratch.
func (g *nativeGen) siteKey(dst Reg) {
	a := g.a
	a.MovRR(dst, RAX)
	a.ShiftImm(Shr, dst, 48, true)
	a.Load(R9, RDI, ncRC)
	a.Load(R9, R9, offRCSiteName)
	a.LeaIdx(R9, R9, dst, 4, 0)
	a.LoadU32(dst, R9, 0)
	a.Load(R9, RDI, ncCC)
	a.Load(R9, R9, offCCNames)
	a.LeaIdx(R9, R9, dst, 4, 0)
	a.LoadU32(dst, R9, 0)
}

// sharedMix puts in idx the multiplication sharedProp and deepSetOf index
// their caches by, of the shape in s and the key in key:
// (s>>4 ^ key<<24) * 0x9e3779b97f4a7c15. tmp is its scratch.
func (g *nativeGen) sharedMix(idx, s, key, tmp Reg) {
	a := g.a
	a.MovRR(idx, s)
	a.ShiftImm(Shr, idx, 4, true)
	a.MovRR(tmp, key)
	a.ShiftImm(Shl, tmp, 24, true)
	a.Op(Xor, idx, tmp, true)
	a.MovImm(tmp, 0x9e3779b97f4a7c15)
	a.Imul(idx, tmp)
}

// megaReadSlot is readSlot at a read's site whose misses, the site having
// given up, are megaRead's: get_prop's and get_method's, get_method_w's and
// get_prop_next's two -- each a megaRead of its own, laid out after every
// other code, the index's finding after them all (megaIdx), as the
// handlers were given them.
func (g *nativeGen) megaReadSlot() {
	a := g.a
	miss, indexed := a.NewLabel(), a.NewLabel()
	g.megaMiss = miss
	g.readSlot()
	ret := g.megaRet
	g.megaMiss, g.megaRet = 0, 0
	g.mega = append(g.mega, func() { g.megaRead(miss, ret, indexed) })
	g.megaIdx = append(g.megaIdx, func() { g.megaReadIndexed(indexed, ret) })
}

// megaRead is a read's miss at a site that has given up (megaReadSlot),
// from readSlot at miss, the object in RSI and the instruction in RAX: the
// read answered by the realm's shared cache (vm's sharedProp), its pair of
// entries for the object's shape and the key, the entry's holder found as
// its holder method finds it, or, where it has none, by the object's own
// table, back at ret with the value's address in RCX, as readSlot leaves
// it. Anything else exits. R8, R9, RDX and RSI are its scratch.
func (g *nativeGen) megaRead(miss, ret, indexed Label) {
	a := g.a
	found, nocell, p1, have := a.NewLabel(), a.NewLabel(), a.NewLabel(), a.NewLabel()
	own, ownLoop, ownNext := a.NewLabel(), a.NewLabel(), a.NewLabel()
	esz := size("sharedEntry")
	a.Bind(miss)
	// The site again: its entries' probe has left another there.
	g.site(RCX)
	a.LoadU8(R8, RCX, off("propCache.fills"))
	a.OpImm(Cmp, R8, maxCacheFills, false)
	a.Jcc(CondB, g.exit)
	g.siteKey(RDX)
	// An index, which sharedProp answers nothing of.
	a.MovRR32(R9, RDX)
	a.ShiftImm(Shr, R9, 31, false)
	a.Jcc(CondNE, g.exit)
	a.Load(R8, RSI, off("Object.shape"))
	a.Op(Test, R8, R8, true)
	a.Jcc(CondE, g.exit)
	g.sharedMix(R9, R8, RDX, RCX)
	a.ShiftImm(Shr, R9, 64-sharedCacheBits, true)
	a.OpImm(And, R9, -2, true)
	a.MovImm(RCX, uint64(esz))
	a.Imul(R9, RCX)
	a.Load(RCX, RDI, ncRT)
	a.Load(RCX, RCX, rtRel(0))
	a.Load(RCX, RCX, offRealmShared)
	// None made yet: a site's own reads are ownScan's, which fill none.
	a.Op(Test, RCX, RCX, true)
	a.Jcc(CondE, own)
	a.Op(Add, R9, RCX, true)
	// The pair's entries, each of the shape and the key.
	for k := int32(0); k < 2; k++ {
		next := a.NewLabel()
		a.CmpMem(R8, R9, k*esz+off("sharedEntry.shape"), true)
		a.Jcc(CondNE, next)
		a.LoadU32(RCX, R9, k*esz+off("sharedEntry.key"))
		a.Op(Cmp, RCX, RDX, false)
		a.Jcc(CondNE, next)
		if k != 0 {
			a.OpImm(Add, R9, esz, true)
		}
		a.Jmp(found)
		a.Bind(next)
	}
	a.Jmp(own)
	// The holder: by the chain's cell, valid, its p2; or the object, or
	// its p1 of s1, or p1's p2 of s2.
	a.Bind(found)
	a.Load(RCX, R9, off("sharedEntry.cell"))
	a.Op(Test, RCX, RCX, true)
	a.Jcc(CondE, nocell)
	a.LoadU8(RCX, RCX, off("protoCell.valid"))
	a.Op(Test, RCX, RCX, false)
	a.Jcc(CondE, g.exit)
	a.Load(RSI, R9, off("sharedEntry.p2"))
	a.Op(Test, RSI, RSI, true)
	a.Jcc(CondE, g.exit)
	a.Jmp(have)
	a.Bind(nocell)
	a.Load(RCX, R9, off("sharedEntry.p1"))
	a.Op(Test, RCX, RCX, true)
	a.Jcc(CondE, have)
	a.Load(R8, RCX, off("Object.shape"))
	a.CmpMem(R8, R9, off("sharedEntry.s1"), true)
	a.Jcc(CondNE, g.exit)
	a.Load(R8, R9, off("sharedEntry.p2"))
	a.Op(Test, R8, R8, true)
	a.Jcc(CondE, p1)
	a.Load(RDX, R8, off("Object.shape"))
	a.CmpMem(RDX, R9, off("sharedEntry.s2"), true)
	a.Jcc(CondNE, g.exit)
	a.MovRR(RSI, R8)
	a.Jmp(have)
	a.Bind(p1)
	a.MovRR(RSI, RCX)
	a.Bind(have)
	a.LoadS32(RCX, R9, off("sharedEntry.idx"))
	a.ShiftImm(Shl, RCX, 4, true)
	a.AddMem(RCX, RSI, off("Object.valp"))
	a.Jmp(ret)
	// None for the shape and the key: the object's own plain data
	// property, of an ordinary object's table of no index, found by its
	// keys as plainOwnAt finds it -- a site's own reads, which the shared
	// cache never holds (vm's ownScan answers them first).
	a.Bind(own)
	a.LoadU8(R9, RSI, offObjClass)
	a.OpImm(Cmp, R9, int32(ClassObject), false)
	a.Jcc(CondNE, g.exit)
	a.CmpMemImm8(R8, off("shape.index"), 0)
	a.Jcc(CondNE, indexed)
	a.LoadU32(R9, RSI, off("Object.valn"))
	a.Load(RCX, R8, off("shape.keys"))
	a.Bind(ownLoop)
	a.Op(Test, R9, R9, true)
	a.Jcc(CondE, g.exit)
	a.LoadU32(R8, RCX, off("propKey.key"))
	a.Op(Cmp, R8, RDX, false)
	a.Jcc(CondNE, ownNext)
	a.LoadU8(R8, RCX, off("propKey.flags"))
	a.OpImm(And, R8, propOddKey, false)
	a.Jcc(CondNE, g.exit)
	a.LoadU32(RCX, RSI, off("Object.valn"))
	a.Op(Sub, RCX, R9, true)
	a.ShiftImm(Shl, RCX, 4, true)
	a.AddMem(RCX, RSI, off("Object.valp"))
	a.Jmp(ret)
	a.Bind(ownNext)
	a.OpImm(Add, RCX, size("propKey"), true)
	a.OpImm(Sub, R9, 1, true)
	a.Jmp(ownLoop)
}

// megaWrite is set_prop's miss at a site that has given up, from
// arrayLength at miss, an ordinary object in RSI and the value's address
// in RDX, as setPropCached makes such a write: a plain
// writable data property the object has in a table small enough to scan,
// written in place (where its layout is not one RegExp's methods' fast
// paths trust, pristine, which Go tells it is no longer); or the addition
// the realm's cache of additions holds for the object's shape and the key
// (vm's deepStore), where the object is extensible and its prototypes are
// the entry's, each of its shape, the last's prototype none -- made by
// addTransition's store at store, as the site's own transition's, the
// entry's next shape in R8, room and the barrier as there. Anything else
// exits. R8, R9, RAX and RCX are its scratch, and the scratch's sI keeps
// the value's address.
func (g *nativeGen) megaWrite(miss, store, indexed, deep Label) {
	a := g.a
	scan, scanNext, found, chain, end := a.NewLabel(), a.NewLabel(), a.NewLabel(), a.NewLabel(), a.NewLabel()
	dsz := size("deepAdd")
	ksz := size("propKey")
	a.Bind(miss)
	// The site again: its entries' probe has left another there.
	g.site(RCX)
	a.LoadU8(R8, RCX, off("propCache.fills"))
	a.OpImm(Cmp, R8, maxCacheFills, false)
	a.Jcc(CondB, g.exit)
	a.LoadU8(R8, RSI, offObjClass)
	a.OpImm(Cmp, R8, int32(ClassObject), false)
	a.Jcc(CondNE, g.exit)
	g.siteKey(RCX)
	a.MovRR32(R9, RCX)
	a.ShiftImm(Shr, R9, 31, false)
	a.Jcc(CondNE, g.exit)
	a.Load(R8, RSI, off("Object.shape"))
	a.Op(Test, R8, R8, true)
	a.Jcc(CondE, g.exit)
	// Its own, in a table of no index: the keys scanned.
	a.CmpMemImm8(R8, off("shape.index"), 0)
	a.Jcc(CondNE, indexed)
	a.LoadU32(R9, RSI, off("Object.valn"))
	a.Load(RAX, R8, off("shape.keys"))
	a.Bind(scan)
	a.Op(Test, R9, R9, true)
	a.Jcc(CondE, deep)
	a.LoadU32(R8, RAX, off("propKey.key"))
	a.Op(Cmp, R8, RCX, false)
	a.Jcc(CondNE, scanNext)
	a.LoadU8(R8, RAX, off("propKey.flags"))
	a.OpImm(And, R8, propOddKey|propWritable, false)
	a.OpImm(Cmp, R8, propWritable, false)
	a.Jcc(CondNE, g.exit)
	a.Load(R8, RSI, off("Object.shape"))
	a.LoadU8(R8, R8, off("shape.pristine"))
	a.Op(Test, R8, R8, false)
	a.Jcc(CondNE, g.exit)
	// The value's place: the table's length less the keys left, the one
	// found among them.
	a.LoadU32(RAX, RSI, off("Object.valn"))
	a.Op(Sub, RAX, R9, true)
	a.ShiftImm(Shl, RAX, 4, true)
	a.AddMem(RAX, RSI, off("Object.valp"))
	g.loadValue(RDX)
	g.storeValue()
	a.Bind(scanNext)
	a.OpImm(Add, RAX, ksz, true)
	a.OpImm(Sub, R9, 1, true)
	a.Jmp(scan)
	// An addition the realm's cache holds, to an object extensible and of
	// its prototype in its layout, which the cache holds none for else
	// (fillDeepStore): its set, deepWays entries, by the shape and the key.
	a.Bind(deep)
	a.LoadU16(RAX, RSI, offObjFlags)
	a.OpImm(And, RAX, int32(objExtensible|objPolyProto), false)
	a.OpImm(Cmp, RAX, int32(objExtensible), false)
	a.Jcc(CondNE, g.exit)
	a.Load(R8, RSI, off("Object.shape"))
	g.sharedMix(R9, R8, RCX, RAX)
	a.ShiftImm(Shr, R9, 64-sharedCacheBits+deepWaysLog, true)
	a.ShiftImm(Shl, R9, deepWaysLog, true)
	a.MovImm(RAX, uint64(dsz))
	a.Imul(R9, RAX)
	a.Load(RAX, RDI, ncRT)
	a.Load(RAX, RAX, rtRel(0))
	a.Load(RAX, RAX, offRealmDeep)
	a.Op(Test, RAX, RAX, true)
	a.Jcc(CondE, g.exit)
	a.Op(Add, R9, RAX, true)
	for w := int32(0); w < 1<<deepWaysLog; w++ {
		next := a.NewLabel()
		a.LoadU32(RAX, R9, w*dsz+off("deepAdd.key"))
		a.Op(Cmp, RAX, RCX, false)
		a.Jcc(CondNE, next)
		a.CmpMem(R8, R9, w*dsz+off("deepAdd.from"), true)
		a.Jcc(CondNE, next)
		if w != 0 {
			a.OpImm(Add, R9, w*dsz, true)
		}
		a.Jmp(found)
		a.Bind(next)
	}
	a.Jmp(g.exit)
	// Its chain the entry's: each prototype the one it names, of its
	// shape, none past the last.
	a.Bind(found)
	a.Store(RDI, scr(sI), RDX)
	a.Load(RAX, R8, off("shape.proto"))
	a.LoadS32(RCX, R9, off("deepAdd.n"))
	a.Lea(R8, R9, off("deepAdd.up"))
	a.Bind(chain)
	a.Op(Test, RCX, RCX, true)
	a.Jcc(CondE, end)
	a.CmpMem(RAX, R8, 0, true)
	a.Jcc(CondNE, g.exit)
	a.Load(RDX, RAX, off("Object.shape"))
	a.CmpMem(RDX, R8, off("deepAdd.shapes")-off("deepAdd.up"), true)
	a.Jcc(CondNE, g.exit)
	a.LoadU16(RDX, RAX, offObjFlags)
	a.OpImm(And, RDX, int32(objPolyProto), false)
	a.Jcc(CondNE, g.exit)
	// The prototype's own prototype, its layout's: a shape it has, which
	// the entry's says.
	a.Load(RAX, RAX, off("Object.shape"))
	a.Load(RAX, RAX, off("shape.proto"))
	a.OpImm(Add, R8, 8, true)
	a.OpImm(Sub, RCX, 1, true)
	a.Jmp(chain)
	a.Bind(end)
	a.Op(Test, RAX, RAX, true)
	a.Jcc(CondNE, g.exit)
	a.Load(R8, R9, off("deepAdd.next"))
	a.Load(RDX, RDI, scr(sI))
	a.Jmp(store)
}

// indexFind is vm's findOwn of the key in key in the table of the object in
// RSI, whose layout has an index (more than linearScanLimit properties):
// the index's recent entry for the key, what it says checked against the
// table, as findOwn checks it; or else the index's slots, probed as
// slotTable.get probes them, what they say remembered in the recent entry
// as findOwn remembers it -- a key and a position, no pointer. At found, i
// is the property's position and s the address of its key among the
// layout's; at absent, the object has no property of the key. key is kept;
// s, i and t are its scratch, and the scratch's sEnd holds the key while
// the slots are probed.
func (g *nativeGen) indexFind(key, s, i, t Reg, found, absent Label) {
	a := g.a
	slots, loop, hit, none, remember := a.NewLabel(), a.NewLabel(), a.NewLabel(), a.NewLabel(), a.NewLabel()
	recent := off("propIndex.recent")
	e := off("propIndex.slots") + off("slotTable.e")
	if size("recentKey") != 8 || size("slotEntry") != 8 || size("propKey") != 8 ||
		off("recentKey.key") != 0 || off("slotEntry.key") != 0 || off("slotEntry.idx") != 4 {
		panic("nativegen: an index's entries are not as indexFind reads them")
	}
	// The recent entry: a position, which the table's key there and its
	// length confirm, or none.
	a.Load(t, RSI, off("Object.shape"))
	a.Load(t, t, offShapeIndex)
	a.MovRR32(i, key)
	a.OpImm(And, i, 15, false)
	a.LeaIdx(s, t, i, 8, recent)
	a.CmpMem(key, s, off("recentKey.key"), false)
	a.Jcc(CondNE, slots)
	a.LoadS32(i, s, off("recentKey.idx"))
	a.Op(Test, i, i, true)
	a.Jcc(CondS, absent)
	a.LoadU32(t, RSI, off("Object.valn"))
	a.Op(Cmp, i, t, true)
	a.Jcc(CondAE, absent)
	a.Load(s, RSI, off("Object.shape"))
	a.Load(s, s, off("shape.keys"))
	a.LeaIdx(s, s, i, 8, 0)
	a.CmpMem(key, s, off("propKey.key"), false)
	a.Jcc(CondNE, slots)
	a.LoadU8(t, s, off("propKey.flags"))
	a.OpImm(And, t, propDeleted, false)
	a.Jcc(CondNE, slots)
	a.Jmp(found)
	// The slots, from the key's home on, to the key's entry or an empty
	// one: s their base, t their mask, i the entry, key the entry's word
	// while the key waits in sEnd.
	a.Bind(slots)
	a.Load(t, RSI, off("Object.shape"))
	a.Load(t, t, offShapeIndex)
	a.Load(s, t, e)
	a.Load(t, t, e+8)
	a.OpImm(Sub, t, 1, true)
	a.MovImm(i, slotHomeMul)
	a.Imul32(i, key)
	a.ShiftImm(Shr, i, uint8(slotHomeShift), false)
	a.Store(RDI, scr(sEnd), key)
	a.Bind(loop)
	a.Op(And, i, t, true)
	a.LoadIdx(key, s, i, 8, 0)
	a.CmpMem(key, RDI, scr(sEnd), false)
	a.Jcc(CondE, hit)
	a.ShiftImm(Shr, key, 32, true)
	a.OpImm(Cmp, key, slotEmpty, false)
	a.Jcc(CondE, none)
	a.OpImm(Add, i, 1, true)
	a.Jmp(loop)
	a.Bind(hit)
	a.LeaIdx(t, s, i, 8, 0)
	a.LoadS32(i, t, off("slotEntry.idx"))
	a.Op(Test, i, i, true)
	a.Jcc(CondNS, remember)
	a.Bind(none)
	a.MovImm(i, ^uint64(0))
	// What the slots say, remembered: the key, and its position or -1 --
	// a deleted entry's too, as get reports it missing -- then checked
	// against the table, as findOwn checks it.
	a.Bind(remember)
	a.Load(key, RDI, scr(sEnd))
	a.Load(t, RSI, off("Object.shape"))
	a.Load(t, t, offShapeIndex)
	a.MovRR32(s, key)
	a.OpImm(And, s, 15, false)
	a.LeaIdx(t, t, s, 8, recent)
	a.Store32(t, off("recentKey.key"), key)
	a.Store32(t, off("recentKey.idx"), i)
	a.Op(Test, i, i, true)
	a.Jcc(CondS, absent)
	a.LoadU32(t, RSI, off("Object.valn"))
	a.Op(Cmp, i, t, true)
	a.Jcc(CondAE, absent)
	a.Load(s, RSI, off("Object.shape"))
	a.Load(s, s, off("shape.keys"))
	a.LeaIdx(s, s, i, 8, 0)
	a.CmpMem(key, s, off("propKey.key"), false)
	a.Jcc(CondNE, absent)
	a.LoadU8(t, s, off("propKey.flags"))
	a.OpImm(And, t, propDeleted, false)
	a.Jcc(CondNE, absent)
	a.Jmp(found)
}

// megaReadIndexed is megaRead's read of the object's own property where
// its table has an index, from indexed, the object in RSI and the key in
// RDX: found as vm's findOwn finds it (indexFind), a plain data property
// back at ret with the value's address in RCX. A key the object has not,
// or of no plain data property, exits.
func (g *nativeGen) megaReadIndexed(indexed, ret Label) {
	a := g.a
	found := a.NewLabel()
	a.Bind(indexed)
	g.indexFind(RDX, R8, RCX, R9, found, g.exit)
	a.Bind(found)
	a.LoadU8(R9, R8, off("propKey.flags"))
	a.OpImm(And, R9, propOddKey, false)
	a.Jcc(CondNE, g.exit)
	a.ShiftImm(Shl, RCX, 4, true)
	a.AddMem(RCX, RSI, off("Object.valp"))
	a.Jmp(ret)
}

// megaWriteIndexed is megaWrite's write of the object's own property
// where its table has an index, from indexed, the object in RSI, the key
// in RCX and the value's address in RDX: a plain writable data property
// found as vm's findOwn finds it (indexFind) written in place, its layout
// not pristine, as megaWrite's scan writes one; a key the object has not
// goes on to deep, the realm's cache of additions; anything else exits.
func (g *nativeGen) megaWriteIndexed(indexed, deep Label) {
	a := g.a
	found := a.NewLabel()
	a.Bind(indexed)
	g.indexFind(RCX, R8, RAX, R9, found, deep)
	a.Bind(found)
	a.LoadU8(R9, R8, off("propKey.flags"))
	a.OpImm(And, R9, propOddKey|propWritable, false)
	a.OpImm(Cmp, R9, propWritable, false)
	a.Jcc(CondNE, g.exit)
	a.Load(R8, RSI, off("Object.shape"))
	a.LoadU8(R8, R8, off("shape.pristine"))
	a.Op(Test, R8, R8, false)
	a.Jcc(CondNE, g.exit)
	a.ShiftImm(Shl, RAX, 4, true)
	a.AddMem(RAX, RSI, off("Object.valp"))
	g.loadValue(RDX)
	g.storeValue()
}
