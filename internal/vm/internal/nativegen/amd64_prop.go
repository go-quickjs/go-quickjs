package nativegen

import . "github.com/go-quickjs/go-quickjs/internal/jit/asm/amd64"

// polyOwnStore finds the object's shape among the site's other shapes, as
// setPropCached's walk of them does: an entry of its own writable slot goes
// to own, which shares the ordinary store and barrier, and an entry of an
// addition to add, the entry in RCX, whose transition addProp makes. The
// source address stays in RDX. A site with no other shapes, or none of the
// object's, goes to none: an array's length, or the realm's caches where
// the site has given up (megaWrite).
func (g *nativeGen) polyOwnStore(start, own, add, none Label) {
	a := g.a
	loop, found, trans, next := a.NewLabel(), a.NewLabel(), a.NewLabel(), a.NewLabel()
	a.Bind(start)
	a.Load(R8, RCX, off("propCache.poly"))
	a.Op(Test, R8, R8, true)
	a.Jcc(CondE, none)
	a.LoadU8(R9, R8, off("propPoly.n"))
	a.Op(Test, R9, R9, false)
	a.Jcc(CondE, none)
	a.AddMem(R8, RDI, ncPolyEnt)
	a.Load(RCX, RSI, off("Object.shape"))
	a.Bind(loop)
	a.CmpMem(RCX, R8, off("propCache.ownShape"), true)
	a.Jcc(CondE, found)
	a.CmpMem(RCX, R8, off("propCache.shape"), true)
	a.Jcc(CondE, trans)
	a.Bind(next)
	a.OpImm(Add, R8, size("propCache"), true)
	a.OpImm(Sub, R9, 1, false)
	a.Jcc(CondNE, loop)
	a.Jmp(none)
	a.Bind(found)
	a.MovRR(RCX, R8)
	a.Jmp(own)
	// An entry of the object's shape that is no own slot's: an addition's
	// where it has a transition, which addTransition checks; any other,
	// a setter's or a prototype's, is Go's.
	a.Bind(trans)
	a.CmpMemImm8(R8, off("propCache.next"), 0)
	a.Jcc(CondE, next)
	a.MovRR(RCX, R8)
	a.Jmp(add)
}

// getPropNext reads two adjacent data properties without storing the
// intermediate temporary. A miss leaves both reads to their Go handlers.
func (g *nativeGen) getPropNext() {
	a := g.a
	g.decB(RDX)
	g.object(RSI, RDX)
	for i := 0; i < 2; i++ {
		g.site(RCX)
		g.readSlot()
		if i == 0 {
			a.Load(R9, RCX, 0)
			a.CmpMem(R9, RDI, ncObjBits, true)
			a.Jcc(CondNE, g.exit)
			a.Load(RSI, RCX, 8)
			a.Load(RAX, RBX, 8)
		}
	}
	g.loadValue(RCX)
	g.decA(RDX)
	a.LeaIdx(RAX, R12, RDX, 1, 0)
	a.OpImm(Add, RBX, 8, true)
	g.storeValue()
}

// arrayLength is set_prop of an array's own length, which no entry of a
// site holds, as rsSetPropLength and setProp's first case write it: from
// addProp, the object in RSI, the value's address in RDX, the instruction
// in RAX. An ordinary array of its own dense elements (no index key, its
// length writable, its storage not shared with a literal's) given a number
// that is a length no more than its own: setArrayLength's truncation, the
// slots past the new length cleared for the collector -- the number's
// word stored, the pointer's by ptrStore where it holds one -- then the
// length; the same length writes nothing. Anything else exits before it
// writes: a longer length, storage to unshare, a value that is not a
// length (whose RangeError is Go's), another name or object. R8, R9, RCX
// and RDX are its scratch.
//
// Any other class goes to other, a site's megamorphic write (megaWrite).
func (g *nativeGen) arrayLength(start, other Label) {
	a := g.a
	loop, cleared, done := a.NewLabel(), a.NewLabel(), a.NewLabel()
	a.Bind(start)
	a.LoadU8(R8, RSI, offObjClass)
	a.OpImm(Cmp, R8, int32(ClassArray), false)
	a.Jcc(CondNE, other)
	// The name: names[siteName[C]].
	a.MovRR(RCX, RAX)
	a.ShiftImm(Shr, RCX, 48, true)
	a.Load(R9, RDI, ncRC)
	a.Load(R9, R9, offRCSiteName)
	a.LeaIdx(R9, R9, RCX, 4, 0)
	a.LoadU32(RCX, R9, 0)
	a.Load(R9, RDI, ncCC)
	a.Load(R9, R9, offCCNames)
	a.LeaIdx(R9, R9, RCX, 4, 0)
	a.LoadU32(RCX, R9, 0)
	a.OpImm(Cmp, RCX, atomLength, false)
	a.Jcc(CondNE, g.exit)
	want := objArrayLengthWritable | objHasExt
	a.LoadU16(R8, RSI, offObjFlags)
	a.OpImm(And, R8, int32(want|objHasSparseElements|objSharedElems), false)
	a.OpImm(Cmp, R8, int32(want), false)
	a.Jcc(CondNE, g.exit)
	// The value: a number whose uint32 is itself.
	a.Load(R9, RDX, 0)
	a.Op(Cmp, R9, R10, true)
	a.Jcc(CondAE, g.exit)
	a.MovQToX(1, R9)
	a.Cvttsd2si(R9, 1)
	a.MovRR32(R8, R9)
	a.Op(Cmp, R8, R9, true)
	a.Jcc(CondNE, g.exit)
	a.Cvtsi2sd(3, R9, true)
	a.SSEOp(UcomiSD, 1, 3)
	a.Jcc(CondNE, g.exit)
	a.Jcc(CondP, g.exit)
	// No longer than the elements: growing is Go's.
	a.CmpMem(R9, RSI, sizeObj+8, true)
	a.Jcc(CondA, g.exit)
	g.noIndexKeys(RSI)
	// From the last element down to the new length, each slot cleared and
	// then the length one less, as ptrStore leaves no scratch register.
	a.Store(RDI, scr(sE), RSI)
	a.Store(RDI, scr(sRC), R9)
	a.Bind(loop)
	a.Load(RSI, RDI, scr(sE))
	a.Load(R9, RSI, sizeObj+8)
	a.CmpMem(R9, RDI, scr(sRC), true)
	a.Jcc(CondBE, done)
	a.OpImm(Sub, R9, 1, true)
	a.MovRR(RAX, R9)
	a.ShiftImm(Shl, RAX, 4, true)
	a.AddMem(RAX, RSI, sizeObj)
	a.Op(Xor, RCX, RCX, false)
	a.Store(RAX, 0, RCX)
	a.CmpMem(RCX, RAX, 8, true)
	a.Jcc(CondE, cleared)
	a.Lea(RAX, RAX, 8)
	g.ptrStore()
	a.Load(RSI, RDI, scr(sE))
	a.Load(R9, RSI, sizeObj+8)
	a.OpImm(Sub, R9, 1, true)
	a.Bind(cleared)
	a.Store(RSI, sizeObj+8, R9)
	a.Jmp(loop)
	a.Bind(done)
	g.next()
}
