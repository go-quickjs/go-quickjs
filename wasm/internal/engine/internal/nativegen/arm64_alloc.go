package main

import (
	. "github.com/go-quickjs/go-quickjs/internal/jit/asm/arm64"
	rd "github.com/go-quickjs/go-quickjs/wasm/internal/engine/internal/regdef"
)

// The allocations on arm64, as amd64's (amd64_alloc.go): R1 the record,
// R2 the type's index plus 1, R3 the object, R7 the address of its pool's
// word, R2, R4, R5, R0 and R6 the stores' scratch.

func (g *armGen) allocRecord() {
	a := g.a
	g.bc(aC, false)
	a.MovImm(aT, rd.GCSize)
	a.Mul(aC, aC, aT)
	a.Load(aT, aCtx, rd.CtxGC)
	a.Op(Add, aC, aC, aT, true)
	a.LoadU32(aD, aC, rd.GCNatTi)
	a.Cbz(aD, g.exit, false)
}

func (g *armGen) allocStub(kind int64, retry Label) {
	a := g.a
	back, exit, done := a.NewLabel(), a.NewLabel(), a.NewLabel()
	a.Load(aW, aIP, 0)
	g.bc(a8, false)
	a.ShiftImm(Lsl, a8, a8, 8, true)
	a.AddImm(a8, a8, kind, true)
	a.Adr(aJ, back)
	a.Load(aT, aCtx, rd.CtxX)
	a.Load(aT, aT, rd.XNatAlloc)
	a.Br(aT)
	a.Bind(back)
	a.Cbz(a8, exit, true)
	a.CmpImm(a8, 2, true)
	a.BCond(EQ, done)
	a.B(retry)
	a.Bind(exit)
	a.Load(aW, aIP, 0)
	a.B(g.exit)
	a.Bind(done)
	g.next(1)
}

func (g *armGen) poolTake(slow Label) {
	a := g.a
	a.Cbz(a8, slow, true)
	a.Load(a9, a8, rd.PoolCount)
	a.Cbz(a9, slow, true)
	a.AddImm(a9, a9, -1, true)
	a.Store(a8, rd.PoolCount, a9)
	a.AddLsl(a10, a8, a9, 3)
	a.AddImm(a10, a10, rd.PoolObjs, true)
	a.Load(aS, a10, 0)
}

func (g *armGen) putNew() {
	a := g.a
	a.LoadU32(a8, aC, rd.GCDst)
	a.Load(aT, aCtx, rd.CtxR)
	a.AddLsl(a8, aT, a8, 4)
	a.Store(a8, 8, ZR)
	a.MovRR(a9, aS)
	g.storeRef()
	a.MovRR(a8, a10)
	a.MovRR(a9, ZR)
	g.storeRef()
	g.next(1)
}

// storeVal stores the value of kind from the slot in R5 at [base + to],
// to the natVal at R2's. R0, R5 and R6 are lost.
func (g *armGen) storeVal(kind int32, base Reg) {
	a := g.a
	switch kind {
	case rd.NatRef:
		a.Load(aT, aCtx, rd.CtxR)
		a.AddLsl(a9, aT, a9, 4)
		a.LoadQ(V0, a9, 0)
		a.LoadU32(a9, aD, rd.NatTo)
		a.Op(Add, aT, base, a9, true)
		a.StoreQ(aT, 0, V0)
		return
	case rd.NatE128:
		a.AddLsl(a9, aL, a9, 3)
		a.LoadQ(V0, a9, 0)
		a.LoadU32(a9, aD, rd.NatTo)
		a.Op(Add, aT, base, a9, true)
		a.StoreQ(aT, 0, V0)
		return
	case rd.NatV128:
		a.AddLsl(a9, aL, a9, 3)
		a.Load(aW, a9, 0)
		a.LoadF(V0, a9, 8)
		a.LoadU32(a9, aD, rd.NatTo)
		a.Op(Add, aT, base, a9, true)
		a.Store(aT, 0, aW)
		a.StoreF(aT, 16, V0)
		return
	}
	a.LoadIdx(aW, aL, a9, true)
	a.LoadU32(a9, aD, rd.NatTo)
	a.Op(Add, aT, base, a9, true)
	switch kind {
	case rd.Nat64:
		a.Store(aT, 0, aW)
	case rd.NatW32:
		a.MovRR32(aW, aW)
		a.Store(aT, 0, aW)
	case rd.NatW16:
		a.LogicImm(And, aW, aW, 0xffff, false)
		a.Store(aT, 0, aW)
	case rd.NatW8:
		a.LogicImm(And, aW, aW, 0xff, false)
		a.Store(aT, 0, aW)
	case rd.NatE32:
		a.Store32(aT, 0, aW)
	case rd.NatE16:
		a.Store16(aT, 0, aW)
	case rd.NatE8:
		a.Store8(aT, 0, aW)
	}
}

// storeVals stores the record's natVals at base plus each one's offset:
// a struct's cells (R3), or an array's elements (base the elements' first).
func (g *armGen) storeVals(base Reg, kinds []int32) {
	a := g.a
	loop, end := a.NewLabel(), a.NewLabel()
	a.Load(aD, aC, rd.GCNatVals)
	a.Load(a8, aC, rd.GCNatVals+rd.SliceLen)
	a.Bind(loop)
	a.Cbz(a8, end, true)
	next := a.NewLabel()
	for _, k := range kinds {
		skip := a.NewLabel()
		a.LoadU32(a9, aD, rd.NatKind)
		a.CmpImm(a9, int64(k), false)
		a.BCond(NE, skip)
		a.LoadU32(a9, aD, rd.NatFrom)
		g.storeVal(k, base)
		a.B(next)
		a.Bind(skip)
	}
	a.Bind(next)
	a.AddImm(aD, aD, rd.NatSize, true)
	a.AddImm(a8, a8, -1, true)
	a.B(loop)
	a.Bind(end)
}

// arrayLength puts in R5 the length the record's array is made of: its
// last operand's numeric slot's, or, with no operand, its values'.
func (g *armGen) arrayLength() {
	a := g.a
	fixed, done := a.NewLabel(), a.NewLabel()
	a.Load(a9, aC, rd.GCOps+rd.SliceLen)
	a.Cbz(a9, fixed, true)
	a.Load(a8, aC, rd.GCOps)
	a.AddLsl(a8, a8, a9, 2)
	a.AddImm(a8, a8, -4, true)
	a.LoadU32(a9, a8, 0)
	a.AddLsl(a8, aL, a9, 3)
	a.LoadU32(a9, a8, 0)
	a.B(done)
	a.Bind(fixed)
	a.Load(a9, aC, rd.GCNatVals+rd.SliceLen)
	a.Bind(done)
}

func (g *armGen) alloc() {
	a := g.a
	for _, fields := range []bool{true, false} {
		x, name := rd.XStructNew, "StructNew"
		if !fields {
			x, name = rd.XStructNewDefault, "StructNewDefault"
		}
		g.hx(x, name, func() {
			retry, slow := a.NewLabel(), a.NewLabel()
			g.allocRecord()
			a.Bind(retry)
			a.Load(a8, aCtx, rd.CtxX)
			a.Load(a8, a8, rd.XNatPools)
			a.Cbz(a8, slow, true)
			a.AddLsl(a8, a8, aD, 3)
			a.Load(a8, a8, -8)
			g.poolTake(slow)
			if fields {
				g.storeVals(aS, natKinds)
			}
			g.putNew()
			a.Bind(slow)
			g.allocStub(1, retry)
		})
	}
	for _, x := range []int{rd.XArrayNew, rd.XArrayNewDefault, rd.XArrayNewFixed} {
		name := map[int]string{rd.XArrayNew: "ArrayNew", rd.XArrayNewDefault: "ArrayNewDefault", rd.XArrayNewFixed: "ArrayNewFixed"}[x]
		g.hx(x, name, func() {
			retry, slow, big := a.NewLabel(), a.NewLabel(), a.NewLabel()
			g.allocRecord()
			a.Bind(retry)
			g.arrayTake(slow, big)
			switch x {
			case rd.XArrayNewFixed:
				a.Load(aK, aS, rd.ArrayElems)
				g.storeVals(aK, []int32{rd.Nat64, rd.NatE32, rd.NatE16, rd.NatE8, rd.NatRef, rd.NatE128})
			case rd.XArrayNew:
				g.fillElems()
			}
			g.putNew()
			a.Bind(slow)
			g.allocStub(2, retry)
			a.Bind(big)
			g.allocStub(3, retry)
		})
	}
	g.hx(rd.XArrayNewSegment, "ArrayNewSegment", func() {
		retry, slow, big, out := a.NewLabel(), a.NewLabel(), a.NewLabel(), a.NewLabel()
		g.allocRecord()
		a.Bind(retry)
		g.segRange(out)
		g.arrayTake(slow, big)
		g.segCopy()
		g.putNew()
		a.Bind(slow)
		g.allocStub(2, retry)
		a.Bind(big)
		g.allocStub(3, retry)
		// Past the segment: the Go handler traps.
		a.Bind(out)
		a.Load(aW, aIP, 0)
		a.B(g.exit)
	})
}

// arrayTake is amd64's (amd64_alloc.go): the array into R3, R7 its pool's
// word's address, its length stored and left in R5.
func (g *armGen) arrayTake(slow, big Label) {
	a := g.a
	g.arrayLength()
	a.LoadU32(a8, aC, rd.GCNatShift)
	a.ShiftReg(Lsl, a9, a9, a8, true)
	a.CmpImm(a9, rd.ArrayBlockBytes, true)
	a.BCond(HI, big)
	a.AddImm(a9, a9, 15, true)
	a.ShiftImm(Lsr, a9, a9, 4, true)
	a.Load(a8, aCtx, rd.CtxX)
	a.Load(a8, a8, rd.XNatClasses)
	a.LoadU8Idx(a9, a8, a9)
	a.Load(a8, aCtx, rd.CtxX)
	a.Load(a8, a8, rd.XNatPools)
	a.Cbz(a8, slow, true)
	a.AddLsl(a8, a8, aD, 3)
	a.Load(a8, a8, -8)
	a.Cbz(a8, slow, true)
	a.LoadIdx(a8, a8, a9, true)
	g.poolTake(slow)
	g.arrayLength()
	a.Store(aS, rd.ArrayLen, a9)
}

// segEntry puts in R4 the address of the entry of array.new_data's or
// array.new_elem's segment, and in R0 the offset, zero-extended, as
// amd64's. R5 and R6 are lost.
func (g *armGen) segEntry() {
	a := g.a
	elems, join := a.NewLabel(), a.NewLabel()
	a.Load(a8, aC, rd.GCOps)
	a.LoadU32(a9, a8, 0)
	a.AddLsl(a9, aL, a9, 3)
	a.LoadU32(aW, a9, 0)
	a.Load(a8, aCtx, rd.CtxInst)
	a.LoadU8(aT, aC, rd.GCData)
	a.Cbz(aT, elems, false)
	a.Load(a8, a8, rd.InstDatas)
	a.B(join)
	a.Bind(elems)
	a.Load(a8, a8, rd.InstElems)
	a.Bind(join)
	a.LoadU32(aT, aC, rd.GCSeg)
	a.AddLsl(aT, aT, aT, 1)
	a.AddLsl(a8, a8, aT, 3)
}

// segRange goes to out past the segment, as amd64's. R0, R4, R5, R6, R16
// and R17 are lost.
func (g *armGen) segRange(out Label) {
	a := g.a
	elems := a.NewLabel()
	g.arrayLength()
	a.MovRR(aK, a9)
	g.segEntry()
	a.LoadU8(aT, aC, rd.GCData)
	a.Cbz(aT, elems, false)
	a.LoadU32(aT, aC, rd.GCNatShift)
	a.ShiftReg(Lsl, aK, aK, aT, true)
	a.Bind(elems)
	a.Op(Add, aK, aK, aW, true)
	a.Load(aT, a8, rd.SliceLen)
	a.Cmp(aK, aT, true)
	a.BCond(HI, out)
}

// segCopy copies the segment's elements into the array in R3, as many as
// R5 holds, as amd64's. R7 is kept; R0, R2, R4, R5, R6 and R17 are lost.
func (g *armGen) segCopy() {
	a := g.a
	data, words, bytes, done := a.NewLabel(), a.NewLabel(), a.NewLabel(), a.NewLabel()
	a.MovRR(aK, a9)
	g.segEntry()
	a.Load(a8, a8, 0)
	a.LoadU8(aT, aC, rd.GCData)
	a.Cbnz(aT, data, false)
	a.ShiftImm(Lsl, aW, aW, 4, true)
	a.Bind(data)
	a.Op(Add, a8, a8, aW, true)
	a.Load(aD, aS, rd.ArrayElems)
	a.LoadU32(aT, aC, rd.GCNatShift)
	a.ShiftReg(Lsl, aK, aK, aT, true)
	a.Bind(words)
	a.CmpImm(aK, 8, true)
	a.BCond(LO, bytes)
	a.Load(aT, a8, 0)
	a.Store(aD, 0, aT)
	a.AddImm(a8, a8, 8, true)
	a.AddImm(aD, aD, 8, true)
	a.AddImm(aK, aK, -8, true)
	a.B(words)
	a.Bind(bytes)
	a.Cbz(aK, done, true)
	a.LoadU8(aT, a8, 0)
	a.Store8(aD, 0, aT)
	a.AddImm(a8, a8, 1, true)
	a.AddImm(aD, aD, 1, true)
	a.AddImm(aK, aK, -1, true)
	a.B(bytes)
	a.Bind(done)
}

// fillElems stores array.new's value into each element of the array in
// R3, of the length the record's operand says.
func (g *armGen) fillElems() {
	a := g.a
	a.Load(aD, aC, rd.GCNatVals)
	a.LoadU32(aK, aD, rd.NatKind)
	end := a.NewLabel()
	for _, k := range []int32{rd.Nat64, rd.NatE32, rd.NatE16, rd.NatE8, rd.NatRef, rd.NatE128} {
		skip := a.NewLabel()
		a.CmpImm(aK, int64(k), false)
		a.BCond(NE, skip)
		a.LoadU32(a9, aD, rd.NatFrom)
		switch k {
		case rd.NatRef:
			a.Load(aT, aCtx, rd.CtxR)
			a.AddLsl(a9, aT, a9, 4)
			a.LoadQ(V0, a9, 0)
		case rd.NatE128:
			a.AddLsl(a9, aL, a9, 3)
			a.LoadQ(V0, a9, 0)
		default:
			a.LoadIdx(aW, aL, a9, true)
		}
		g.arrayLength()
		a.MovRR(a8, a9)
		a.Load(aD, aS, rd.ArrayElems)
		loop := a.NewLabel()
		a.Bind(loop)
		a.Cbz(a8, end, true)
		size := int64(8)
		switch k {
		case rd.NatRef, rd.NatE128:
			a.StoreQ(aD, 0, V0)
			size = 16
		case rd.Nat64:
			a.Store(aD, 0, aW)
		case rd.NatE32:
			a.Store32(aD, 0, aW)
			size = 4
		case rd.NatE16:
			a.Store16(aD, 0, aW)
			size = 2
		case rd.NatE8:
			a.Store8(aD, 0, aW)
			size = 1
		}
		a.AddImm(aD, aD, size, true)
		a.AddImm(a8, a8, -1, true)
		a.B(loop)
		a.Bind(skip)
	}
	a.Bind(end)
}
