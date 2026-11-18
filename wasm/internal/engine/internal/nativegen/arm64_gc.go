package main

import (
	. "github.com/go-quickjs/go-quickjs/internal/jit/asm/arm64"
	rd "github.com/go-quickjs/go-quickjs/wasm/internal/engine/internal/regdef"
)

// The reference and GC operations on arm64, as amd64's (amd64_gc.go).

// refAddr puts in dst the address of the reference slot field i names.
func (g *armGen) refAddr(dst Reg, i int) {
	g.field(dst, i)
	g.a.Load(aT, aCtx, rd.CtxR)
	g.a.AddLsl(dst, aT, dst, 4)
}

// obj puts in dst the object the reference slot field i names, exiting
// on null; R4 is left the slot's address.
func (g *armGen) obj(dst Reg, i int) {
	g.refAddr(a8, i)
	g.a.Load(dst, a8, 0)
	g.a.Cbz(dst, g.exit, true)
}

// castRec puts in R1 the address of the GC record whose index is in idx,
// exiting where native code does not test its type.
func (g *armGen) castRec(idx Reg) {
	a := g.a
	a.MovImm(aT, rd.GCSize)
	a.Mul(aC, idx, aT)
	a.Load(aT, aCtx, rd.CtxGC)
	a.Op(Add, aC, aC, aT, true)
	a.LoadU8(aT, aC, rd.GCMode)
	a.Cbz(aT, g.exit, false)
}

// castNull goes to yes or no, by record R1's nullable, where the
// reference R3 is null.
func (g *armGen) castNull(yes, no Label) {
	a := g.a
	notNull := a.NewLabel()
	a.Cbnz(aS, notNull, true)
	a.LoadU8(aT, aC, rd.GCNullable)
	a.Cbnz(aT, yes, false)
	a.B(no)
	a.Bind(notNull)
}

// castTest goes to yes or no by whether the reference R3, not null, is of
// record R1's heap type, as amd64's. R2 and R5 are its scratch.
func (g *armGen) castTest(yes, no Label) {
	a := g.a
	index, notI31 := a.NewLabel(), a.NewLabel()
	a.LoadU8(a9, aC, rd.GCMode)
	a.CmpImm(a9, rd.CastIndex, false)
	a.BCond(EQ, index)
	a.CmpImm(a9, rd.CastTop, false)
	a.BCond(EQ, yes)
	a.Load(aT, aCtx, rd.CtxI31)
	a.Cmp(aS, aT, true)
	a.BCond(NE, notI31)
	a.CmpImm(a9, rd.CastI31, false)
	a.BCond(EQ, yes)
	a.CmpImm(a9, rd.CastEq, false)
	a.BCond(EQ, yes)
	a.B(no)
	a.Bind(notI31)
	a.CmpImm(a9, rd.CastI31, false)
	a.BCond(EQ, no)
	a.LoadU8(aD, aS, rd.HeaderKind)
	for _, k := range []struct{ mode, kind int64 }{{rd.CastStruct, rd.ObjStruct}, {rd.CastArray, rd.ObjArray}} {
		other := a.NewLabel()
		a.CmpImm(a9, k.mode, false)
		a.BCond(NE, other)
		a.CmpImm(aD, k.kind, false)
		a.BCond(EQ, yes)
		a.B(no)
		a.Bind(other)
	}
	a.CmpImm(aD, rd.ObjStruct, false)
	a.BCond(EQ, yes)
	a.CmpImm(aD, rd.ObjArray, false)
	a.BCond(EQ, yes)
	a.B(no)
	a.Bind(index)
	a.Load(aT, aCtx, rd.CtxI31)
	a.Cmp(aS, aT, true)
	a.BCond(EQ, no)
	a.Load(aD, aS, 0)
	a.Cbz(aD, no, true)
	a.LoadU32(a9, aC, rd.GCDepth)
	a.Load(aT, aD, rd.RttSupers+rd.SliceLen)
	a.Cmp(a9, aT, true)
	a.BCond(HS, no)
	a.Load(aD, aD, rd.RttSupers)
	a.ShiftImm(Lsl, a9, a9, 2, true)
	a.LoadU32Idx(a9, aD, a9)
	a.LoadU32(aT, aC, rd.GCCanon)
	a.Cmp(a9, aT, false)
	a.BCond(NE, no)
	a.B(yes)
}

// loadSized is dst = the size bytes at [base+disp], sign-extended to 32
// bits (the upper half clear) where signed, else zero-extended.
func (g *armGen) loadSized(dst, base Reg, disp, size int32, signed bool) {
	a := g.a
	switch {
	case size == 1 && signed:
		a.LoadS8(dst, base, disp)
		a.MovRR32(dst, dst)
	case size == 1:
		a.LoadU8(dst, base, disp)
	case size == 2 && signed:
		a.LoadS16(dst, base, disp)
		a.MovRR32(dst, dst)
	case size == 2:
		a.LoadU16(dst, base, disp)
	case size == 4:
		a.LoadU32(dst, base, disp)
	default:
		a.Load(dst, base, disp)
	}
}

func (g *armGen) gc() {
	a := g.a
	// cellAddr is R3 += C * 16: the cell C of the struct at R3.
	cellAddr := func() {
		a.ShiftImm(Lsr, aC, aW, 48, true)
		a.AddLsl(aS, aS, aC, 4)
	}
	// refTo copies the reference at src to the reference slot field i,
	// the pointer by storeRef.
	refTo := func(i int, src Reg) {
		g.refAddr(a8, i)
		a.Load(aD, src, 8)
		a.Store(a8, 8, aD)
		a.Load(a9, src, 0)
		g.storeRef()
	}
	// refFrom copies the reference in slot field i to dst.
	refFrom := func(dst Reg, i int) {
		g.refAddr(a10, i)
		a.Load(aD, a10, 8)
		a.Store(dst, 8, aD)
		a.MovRR(a8, dst)
		a.Load(a9, a10, 0)
		g.storeRef()
	}
	for k, t := range gcGets {
		t := t
		g.hx(rd.XSGet8S+k, "SGet"+t.name, func() {
			g.obj(aS, 1)
			cellAddr()
			switch t.name {
			case "128":
				a.Load(aD, aS, 8)
				a.Load(aC, aS, 24)
				g.addr(a9, 0)
				a.Store(a9, 0, aD)
				a.Store(a9, 8, aC)
			case "Ref":
				refTo(0, aS)
			default:
				g.loadSized(aD, aS, 8, t.size, t.signed)
				g.store(0, aD)
			}
			g.next(1)
		})
	}
	for k, t := range gcSets {
		t := t
		g.hx(rd.XSSet8+k, "SSet"+t.name, func() {
			g.obj(aS, 0)
			cellAddr()
			switch t.name {
			case "128":
				g.addr(a9, 1)
				a.Load(aD, a9, 0)
				a.Load(aC, a9, 8)
				a.Store(aS, 8, aD)
				a.Store(aS, 24, aC)
			case "Ref":
				refFrom(aS, 1)
			default:
				g.load(aD, 1)
				switch t.size {
				case 1:
					a.Ubfx(aD, aD, 0, 8)
				case 2:
					a.Ubfx(aD, aD, 0, 16)
				case 4:
					a.MovRR32(aD, aD)
				}
				a.Store(aS, 8, aD)
			}
			g.next(1)
		})
	}
	// elem puts in R3 the address of the element, of size bytes, of the
	// array the reference slot field arr names at the index the numeric
	// slot field idx names, exiting out of bounds.
	elem := func(arr, idx int, size int32) {
		g.obj(aS, arr)
		g.load32(aC, idx)
		a.Load(aT, aS, rd.ArrayLen)
		a.Cmp(aC, aT, true)
		a.BCond(HS, g.exit)
		a.Load(aS, aS, rd.ArrayElems)
		sh := map[int32]uint8{1: 0, 2: 1, 4: 2, 8: 3, 16: 4}[size]
		a.AddLsl(aS, aS, aC, sh)
	}
	for k, t := range gcGets {
		t := t
		g.hx(rd.XAGet8S+k, "AGet"+t.name, func() {
			elem(1, 2, t.size)
			switch t.name {
			case "128":
				a.LoadQ(V0, aS, 0)
				g.addr(a9, 0)
				a.StoreQ(a9, 0, V0)
			case "Ref":
				refTo(0, aS)
			default:
				g.loadSized(aD, aS, 0, t.size, t.signed)
				g.store(0, aD)
			}
			g.next(1)
		})
	}
	for k, t := range gcSets {
		t := t
		g.hx(rd.XASet8+k, "ASet"+t.name, func() {
			elem(0, 1, t.size)
			switch t.name {
			case "128":
				g.addr(a9, 2)
				a.LoadQ(V0, a9, 0)
				a.StoreQ(aS, 0, V0)
			case "Ref":
				refFrom(aS, 2)
			default:
				g.load(aD, 2)
				switch t.size {
				case 1:
					a.Store8(aS, 0, aD)
				case 2:
					a.Store16(aS, 0, aD)
				case 4:
					a.Store32(aS, 0, aD)
				default:
					a.Store(aS, 0, aD)
				}
			}
			g.next(1)
		})
	}
	g.hx(rd.XALen, "ALen", func() {
		g.obj(aS, 1)
		a.LoadU32(aD, aS, rd.ArrayLen)
		g.store(0, aD)
		g.next(1)
	})
	g.hx(rd.XRefI31, "RefI31", func() {
		g.load(aD, 1)
		a.Ubfx(aD, aD, 0, 31)
		g.refAddr(a8, 0)
		a.Store(a8, 8, aD)
		a.Load(a9, aCtx, rd.CtxI31)
		g.storeRef()
		g.next(1)
	})
	for _, s := range []bool{true, false} {
		x, n := rd.XI31GetU, "I31GetU"
		if s {
			x, n = rd.XI31GetS, "I31GetS"
		}
		s := s
		g.hx(x, n, func() {
			g.obj(aS, 1)
			a.LoadU32(aD, a8, 8)
			if s {
				a.Sbfx(aD, aD, 0, 31)
				a.MovRR32(aD, aD)
			}
			g.store(0, aD)
			g.next(1)
		})
	}
	g.hx(rd.XRefTest, "RefTest", func() {
		yes, no, done := a.NewLabel(), a.NewLabel(), a.NewLabel()
		a.ShiftImm(Lsr, aC, aW, 48, true)
		g.castRec(aC)
		g.refAddr(a8, 1)
		a.Load(aS, a8, 0)
		g.castNull(yes, no)
		g.castTest(yes, no)
		a.Bind(yes)
		a.MovImm(aD, 1)
		a.B(done)
		a.Bind(no)
		a.MovRR(aD, ZR)
		a.Bind(done)
		g.store(0, aD)
		g.next(1)
	})
	g.hx(rd.XRefCast, "RefCast", func() {
		ok := a.NewLabel()
		a.ShiftImm(Lsr, aC, aW, 48, true)
		g.castRec(aC)
		g.refAddr(a8, 0)
		a.Load(aS, a8, 0)
		g.castNull(ok, g.exit)
		g.castTest(ok, g.exit)
		a.Bind(ok)
		g.next(1)
	})
	for _, fail := range []bool{false, true} {
		x, n := rd.XBrOnCast, "BrOnCast"
		if fail {
			x, n = rd.XBrOnCastFail, "BrOnCastFail"
		}
		fail := fail
		g.hx(x, n, func() {
			yes, no := a.NewLabel(), a.NewLabel()
			a.LoadU32(aD, aIP, 8)
			g.castRec(aD)
			g.refAddr(a8, 0)
			a.Load(aS, a8, 0)
			g.castNull(yes, no)
			g.castTest(yes, no)
			taken, not := yes, no
			if fail {
				taken, not = no, yes
			}
			a.Bind(not)
			g.next(2)
			a.Bind(taken)
			g.bc(aC, true)
			g.take(aC)
		})
	}
	g.hx(rd.XRefIsNull, "RefIsNull", func() {
		g.refAddr(a8, 1)
		a.Load(aS, a8, 0)
		a.CmpImm(aS, 0, true)
		a.Cset(aD, EQ)
		g.store(0, aD)
		g.next(1)
	})
	g.hx(rd.XRefEq, "RefEq", func() {
		ne := a.NewLabel()
		g.refAddr(a8, 1)
		g.refAddr(a10, 2)
		a.MovRR(aD, ZR)
		a.Load(aS, a8, 0)
		a.Load(aT, a10, 0)
		a.Cmp(aS, aT, true)
		a.BCond(NE, ne)
		a.Load(aS, a8, 8)
		a.Load(aT, a10, 8)
		a.Cmp(aS, aT, true)
		a.Cset(aD, EQ)
		a.Bind(ne)
		g.store(0, aD)
		g.next(1)
	})
	g.hx(rd.XRefAsNonNull, "RefAsNonNull", func() {
		g.obj(aS, 0)
		g.next(1)
	})
	for _, null := range []bool{true, false} {
		x, n := rd.XBrNonNull, "BrNonNull"
		if null {
			x, n = rd.XBrNull, "BrNull"
		}
		null := null
		g.hx(x, n, func() {
			tk := a.NewLabel()
			g.refAddr(a8, 0)
			a.Load(aS, a8, 0)
			if null {
				a.Cbz(aS, tk, true)
			} else {
				a.Cbnz(aS, tk, true)
			}
			g.next(1)
			a.Bind(tk)
			g.bc(aC, true)
			g.take(aC)
		})
	}
	g.hx(rd.XSelectRef, "SelectRef", func() {
		skip := a.NewLabel()
		g.load(aC, 2)
		a.Cbnz(aC, skip, true)
		g.refAddr(a10, 1)
		g.refAddr(a8, 0)
		a.Load(aD, a10, 8)
		a.Store(a8, 8, aD)
		a.Load(a9, a10, 0)
		g.storeRef()
		a.Bind(skip)
		g.next(1)
	})
	g.hx(rd.XSelect128, "Select128", func() {
		skip := a.NewLabel()
		g.load(aC, 2)
		a.Cbnz(aC, skip, true)
		g.vload(V0, 1)
		g.vstore(0, V0)
		a.Bind(skip)
		g.next(1)
	})
}
