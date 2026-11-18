package main

import (
	. "github.com/go-quickjs/go-quickjs/internal/jit/asm/amd64"
	rd "github.com/go-quickjs/go-quickjs/wasm/internal/engine/internal/regdef"
)

// The reference and GC operations: the moves and tests of references, a
// struct's fields and an array's elements by their storage kind, an
// array's length, i31s, and the tests and casts to a type index. A null
// reference where the operation needs an object, an index out of bounds,
// a failed cast and a test of an abstract type leave to Go, which traps or
// answers; a reference stored, to a slot or to an object, is stored by
// storeRef. An object is read in place: a struct's field in its cell's
// payload (a v128 in two cells', a reference the whole cell), an array's
// element at its elements' first plus the index times its size.

// gcGets and gcSets are the storage kinds of the get and set operations,
// in regdef's order from XSGet8S and XSSet8 (XAGet8S and XASet8): the
// size of an array's element, and for a get whether it is sign-extended.
var gcGets = []struct {
	name   string
	size   int32
	signed bool
}{{"8S", 1, true}, {"8U", 1, false}, {"16S", 2, true}, {"16U", 2, false}, {"32", 4, false}, {"64", 8, false}, {"128", 16, false}, {"Ref", 16, false}}

var gcSets = []struct {
	name string
	size int32
}{{"8", 1}, {"16", 2}, {"32", 4}, {"64", 8}, {"128", 16}, {"Ref", 16}}

// refAddr puts in dst the address of the reference slot field i names.
func (g *gen) refAddr(dst Reg, i int) {
	g.off(dst, i)
	g.a.Op(Add, dst, dst, true)
	g.a.OpMem(Add, dst, RDI, rd.CtxR, true)
}

// obj puts in dst the object the reference slot field i names, exiting
// on null; R8 is its scratch.
func (g *gen) obj(dst Reg, i int) {
	g.refAddr(R8, i)
	g.a.Load(dst, R8, 0)
	g.a.Op(Test, dst, dst, true)
	g.a.Jcc(CondE, g.exit)
}

// castTest goes to yes or no by whether the reference RSI, not null, is
// of record RCX's heap type, by the record's mode: for a type index, its
// type's supertype at the record's depth is the record's canonical type;
// for i31, struct, array and eq, what it points to. RDX and R9 are its
// scratch.
func (g *gen) castTest(yes, no Label) {
	a := g.a
	index, notI31 := a.NewLabel(), a.NewLabel()
	a.LoadU8(R9, RCX, rd.GCMode)
	a.OpImm(Cmp, R9, rd.CastIndex, false)
	a.Jcc(CondE, index)
	a.OpImm(Cmp, R9, rd.CastTop, false)
	a.Jcc(CondE, yes)
	a.CmpMem(RSI, RDI, rd.CtxI31, true)
	a.Jcc(CondNE, notI31)
	// An i31: of i31 and eq.
	a.OpImm(Cmp, R9, rd.CastI31, false)
	a.Jcc(CondE, yes)
	a.OpImm(Cmp, R9, rd.CastEq, false)
	a.Jcc(CondE, yes)
	a.Jmp(no)
	a.Bind(notI31)
	a.OpImm(Cmp, R9, rd.CastI31, false)
	a.Jcc(CondE, no)
	a.LoadU8(RDX, RSI, rd.HeaderKind)
	for _, k := range []struct{ mode, kind int32 }{{rd.CastStruct, rd.ObjStruct}, {rd.CastArray, rd.ObjArray}} {
		other := a.NewLabel()
		a.OpImm(Cmp, R9, k.mode, false)
		a.Jcc(CondNE, other)
		a.OpImm(Cmp, RDX, k.kind, false)
		a.Jcc(CondE, yes)
		a.Jmp(no)
		a.Bind(other)
	}
	// eq: a struct or an array.
	a.OpImm(Cmp, RDX, rd.ObjStruct, false)
	a.Jcc(CondE, yes)
	a.OpImm(Cmp, RDX, rd.ObjArray, false)
	a.Jcc(CondE, yes)
	a.Jmp(no)
	a.Bind(index)
	a.CmpMem(RSI, RDI, rd.CtxI31, true)
	a.Jcc(CondE, no)
	a.Load(RDX, RSI, 0)
	a.Op(Test, RDX, RDX, true)
	a.Jcc(CondE, no)
	a.LoadU32(R9, RCX, rd.GCDepth)
	a.CmpMem(R9, RDX, rd.RttSupers+rd.SliceLen, true)
	a.Jcc(CondAE, no)
	a.Load(RDX, RDX, rd.RttSupers)
	a.ShiftImm(Shl, R9, 2, true)
	a.Op(Add, RDX, R9, true)
	a.LoadU32(R9, RDX, 0)
	a.CmpMem(R9, RCX, rd.GCCanon, false)
	a.Jcc(CondNE, no)
	a.Jmp(yes)
}

// castRec puts in RCX the address of GC record index, in idx, exiting
// where native code does not test its type.
func (g *gen) castRec(idx Reg) {
	a := g.a
	a.ImulImm(RCX, idx, rd.GCSize, true)
	a.OpMem(Add, RCX, RDI, rd.CtxGC, true)
	a.TestMem8(RCX, rd.GCMode, 0xff)
	a.Jcc(CondE, g.exit)
}

// castNull goes to yes or no, by the record RCX's nullable, where the
// reference RSI is null.
func (g *gen) castNull(yes, no Label) {
	a := g.a
	notNull := a.NewLabel()
	a.Op(Test, RSI, RSI, true)
	a.Jcc(CondNE, notNull)
	a.TestMem8(RCX, rd.GCNullable, 0xff)
	a.Jcc(CondNE, yes)
	a.Jmp(no)
	a.Bind(notNull)
}

func (g *gen) gc() {
	a := g.a
	cellAddr := func() {
		// RSI += C * 16: the cell C of the struct at RSI.
		a.MovRR(RCX, RAX)
		a.ShiftImm(Shr, RCX, 48, true)
		a.ShiftImm(Shl, RCX, 4, true)
		a.Op(Add, RSI, RCX, true)
	}
	for k, t := range gcGets {
		t := t
		g.hx(rd.XSGet8S+k, "SGet"+t.name, func() {
			g.obj(RSI, 1)
			cellAddr()
			switch t.name {
			case "128":
				a.Load(RDX, RSI, 8)
				a.Load(RCX, RSI, 24)
				g.off(R9, 0)
				a.StoreIdx(R12, R9, 1, 0, RDX)
				a.StoreIdx(R12, R9, 1, 8, RCX)
			case "Ref":
				g.refAddr(R8, 0)
				a.Load(RDX, RSI, 8)
				a.Store(R8, 8, RDX)
				a.Load(R9, RSI, 0)
				g.storeRef()
			default:
				g.loadSized(RDX, RSI, 8, t.size, t.signed)
				g.store(0, RDX)
			}
			g.next(1)
		})
	}
	for k, t := range gcSets {
		t := t
		g.hx(rd.XSSet8+k, "SSet"+t.name, func() {
			g.obj(RSI, 0)
			cellAddr()
			switch t.name {
			case "128":
				g.off(R9, 1)
				a.LoadIdx(RDX, R12, R9, 1, 0)
				a.LoadIdx(RCX, R12, R9, 1, 8)
				a.Store(RSI, 8, RDX)
				a.Store(RSI, 24, RCX)
			case "Ref":
				g.refAddr(R10, 1)
				a.Load(RDX, R10, 8)
				a.Store(RSI, 8, RDX)
				a.MovRR(R8, RSI)
				a.Load(R9, R10, 0)
				g.storeRef()
			default:
				g.load(RDX, 1)
				switch t.size {
				case 1:
					a.MovZX8(RDX, RDX)
				case 2:
					a.OpImm(And, RDX, 0xffff, false)
				case 4:
					a.MovRR32(RDX, RDX)
				}
				a.Store(RSI, 8, RDX)
			}
			g.next(1)
		})
	}
	// elem puts in RSI the address of the element, of size bytes, of the
	// array the reference slot field arr names at the index the numeric
	// slot field idx names, exiting out of bounds.
	elem := func(arr, idx int, size int32) {
		g.obj(RSI, arr)
		g.off(R9, idx)
		a.LoadU32Idx(RCX, R12, R9, 0)
		a.CmpMem(RCX, RSI, rd.ArrayLen, true)
		a.Jcc(CondAE, g.exit)
		a.Load(RSI, RSI, rd.ArrayElems)
		if sh := map[int32]uint8{1: 0, 2: 1, 4: 2, 8: 3, 16: 4}[size]; sh > 0 {
			a.ShiftImm(Shl, RCX, sh, true)
		}
		a.Op(Add, RSI, RCX, true)
	}
	for k, t := range gcGets {
		t := t
		g.hx(rd.XAGet8S+k, "AGet"+t.name, func() {
			elem(1, 2, t.size)
			switch t.name {
			case "128":
				a.Load(RDX, RSI, 0)
				a.Load(RCX, RSI, 8)
				g.off(R9, 0)
				a.StoreIdx(R12, R9, 1, 0, RDX)
				a.StoreIdx(R12, R9, 1, 8, RCX)
			case "Ref":
				g.refAddr(R8, 0)
				a.Load(RDX, RSI, 8)
				a.Store(R8, 8, RDX)
				a.Load(R9, RSI, 0)
				g.storeRef()
			default:
				g.loadSized(RDX, RSI, 0, t.size, t.signed)
				g.store(0, RDX)
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
				g.off(R9, 2)
				a.LoadIdx(RDX, R12, R9, 1, 0)
				a.LoadIdx(RCX, R12, R9, 1, 8)
				a.Store(RSI, 0, RDX)
				a.Store(RSI, 8, RCX)
			case "Ref":
				g.refAddr(R10, 2)
				a.Load(RDX, R10, 8)
				a.Store(RSI, 8, RDX)
				a.MovRR(R8, RSI)
				a.Load(R9, R10, 0)
				g.storeRef()
			default:
				g.load(RDX, 2)
				switch t.size {
				case 1:
					a.Store8(RSI, 0, RDX)
				case 2:
					a.Store16(RSI, 0, RDX)
				case 4:
					a.Store32(RSI, 0, RDX)
				default:
					a.Store(RSI, 0, RDX)
				}
			}
			g.next(1)
		})
	}
	g.hx(rd.XALen, "ALen", func() {
		g.obj(RSI, 1)
		a.LoadU32(RDX, RSI, rd.ArrayLen)
		g.store(0, RDX)
		g.next(1)
	})
	g.hx(rd.XRefI31, "RefI31", func() {
		g.load(RDX, 1)
		a.OpImm(And, RDX, 0x7fffffff, false)
		g.refAddr(R8, 0)
		a.Store(R8, 8, RDX)
		a.Load(R9, RDI, rd.CtxI31)
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
			g.obj(RSI, 1)
			a.LoadU32(RDX, R8, 8)
			if s {
				a.ShiftImm(Shl, RDX, 1, false)
				a.ShiftImm(Sar, RDX, 1, false)
			}
			g.store(0, RDX)
			g.next(1)
		})
	}
	g.hx(rd.XRefTest, "RefTest", func() {
		yes, no, done := a.NewLabel(), a.NewLabel(), a.NewLabel()
		a.MovRR(RCX, RAX)
		a.ShiftImm(Shr, RCX, 48, true)
		g.castRec(RCX)
		g.refAddr(R8, 1)
		a.Load(RSI, R8, 0)
		g.castNull(yes, no)
		g.castTest(yes, no)
		a.Bind(yes)
		a.MovImm(RDX, 1)
		a.Jmp(done)
		a.Bind(no)
		a.Op(Xor, RDX, RDX, false)
		a.Bind(done)
		g.store(0, RDX)
		g.next(1)
	})
	g.hx(rd.XRefCast, "RefCast", func() {
		ok := a.NewLabel()
		a.MovRR(RCX, RAX)
		a.ShiftImm(Shr, RCX, 48, true)
		g.castRec(RCX)
		g.refAddr(R8, 0)
		a.Load(RSI, R8, 0)
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
			a.LoadU32(RCX, RBX, 8)
			g.castRec(RCX)
			g.refAddr(R8, 0)
			a.Load(RSI, R8, 0)
			g.castNull(yes, no)
			g.castTest(yes, no)
			a.Bind(yes)
			if fail {
				g.next(2)
			} else {
				g.bc(RCX, true)
				g.take(RCX)
			}
			a.Bind(no)
			if fail {
				g.bc(RCX, true)
				g.take(RCX)
			} else {
				g.next(2)
			}
		})
	}
	g.hx(rd.XRefIsNull, "RefIsNull", func() {
		g.refAddr(R8, 1)
		a.CmpMemImm8(R8, 0, 0)
		a.Setcc(CondE, RDX)
		a.MovZX8(RDX, RDX)
		g.store(0, RDX)
		g.next(1)
	})
	g.hx(rd.XRefEq, "RefEq", func() {
		ne := a.NewLabel()
		g.refAddr(R8, 1)
		g.refAddr(R10, 2)
		a.Op(Xor, RDX, RDX, false)
		a.Load(RSI, R8, 0)
		a.CmpMem(RSI, R10, 0, true)
		a.Jcc(CondNE, ne)
		a.Load(RSI, R8, 8)
		a.CmpMem(RSI, R10, 8, true)
		a.Jcc(CondNE, ne)
		a.MovImm(RDX, 1)
		a.Bind(ne)
		g.store(0, RDX)
		g.next(1)
	})
	g.hx(rd.XRefAsNonNull, "RefAsNonNull", func() {
		g.obj(RSI, 0)
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
			g.refAddr(R8, 0)
			a.CmpMemImm8(R8, 0, 0)
			if null {
				a.Jcc(CondE, tk)
			} else {
				a.Jcc(CondNE, tk)
			}
			g.next(1)
			a.Bind(tk)
			g.bc(RCX, true)
			g.take(RCX)
		})
	}
	g.hx(rd.XSelectRef, "SelectRef", func() {
		skip := a.NewLabel()
		g.load(RCX, 2)
		a.Op(Test, RCX, RCX, true)
		a.Jcc(CondNE, skip)
		g.refAddr(R10, 1)
		g.refAddr(R8, 0)
		a.Load(RDX, R10, 8)
		a.Store(R8, 8, RDX)
		a.Load(R9, R10, 0)
		g.storeRef()
		a.Bind(skip)
		g.next(1)
	})
	g.hx(rd.XSelect128, "Select128", func() {
		skip := a.NewLabel()
		g.load(RCX, 2)
		a.Op(Test, RCX, RCX, true)
		a.Jcc(CondNE, skip)
		g.off(R9, 1)
		a.LoadIdx(RDX, R12, R9, 1, 0)
		a.LoadIdx(RSI, R12, R9, 1, 8)
		g.off(R9, 0)
		a.StoreIdx(R12, R9, 1, 0, RDX)
		a.StoreIdx(R12, R9, 1, 8, RSI)
		a.Bind(skip)
		g.next(1)
	})
}

// loadSized is dst = the size bytes at [base+disp], sign-extended to 32
// bits (the upper half clear) where signed, else zero-extended.
func (g *gen) loadSized(dst, base Reg, disp, size int32, signed bool) {
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
