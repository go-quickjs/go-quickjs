package opt

import (
	"github.com/go-quickjs/go-quickjs/internal/jit/asm/amd64"
	wjit "github.com/go-quickjs/go-quickjs/wasm/internal/jit"
	"github.com/go-quickjs/go-quickjs/wasm/internal/module"
)

// GC objects on amd64. The object's pointer word is loaded from its slot
// into s1, tested for null, and the field or element read or written in
// place, at the offsets the engine gives (Layout): a number into or from
// the value's location, a reference copied between the object and a slot
// through the barrier, as every reference is. s0 holds R, the index, or
// a number on its way.

// object loads the pointer word of the reference in the slot at off from R
// into s1, trapping at at on null; s0 is left R.
func (g *gen) object(off int32, at uint32) {
	a := g.a
	a.Load(s0, rCtx, offR)
	a.Load(s1, s0, off)
	a.Op(amd64.Test, s1, s1, true)
	a.Jcc(amd64.CondE, g.trap(wjit.TrapNullDereference, at))
}

// loadNum makes v the number stored as storage at [base + off], a packed
// one extended as signed says.
func (g *gen) loadNum(v *Value, base amd64.Reg, off int32, storage module.Kind, signed bool) {
	a := g.a
	if v.Type.IsFloat() {
		t := g.defX(v)
		if v.Type == TF32 {
			a.LoadSS(t, base, off)
		} else {
			a.LoadSD(t, base, off)
		}
		g.done(v, int(t))
		return
	}
	t := g.defG(v)
	switch {
	case storage == module.KindI8 && signed:
		a.LoadS8(t, base, off)
		a.MovRR32(t, t)
	case storage == module.KindI8:
		a.LoadU8(t, base, off)
	case storage == module.KindI16 && signed:
		a.LoadS16(t, base, off)
		a.MovRR32(t, t)
	case storage == module.KindI16:
		a.LoadU16(t, base, off)
	case v.Type == TI64:
		a.Load(t, base, off)
	default:
		a.LoadU32(t, base, off)
	}
	g.done(v, int(t))
}

// storeNum stores x as storage at [base + off], base not s0: its bytes, or,
// word, all 64 bits of the value zero-extended. s0 is lost.
func (g *gen) storeNum(x *Value, base amd64.Reg, off int32, storage module.Kind, word bool) {
	a := g.a
	l := g.locOf(x)
	if l.kind == lXMM {
		xr := amd64.XReg(l.r)
		if x.Type == TF64 {
			a.StoreSD(base, off, xr)
			return
		}
		a.MovDFromX(s0, xr)
		if word {
			a.Store(base, off, s0)
		} else {
			a.Store32(base, off, s0)
		}
		return
	}
	r := s0
	switch l.kind {
	case lGPR:
		r = amd64.Reg(l.r)
	case lSlot:
		a.Load(s0, rL, slotOff(l.slot))
	case lConst:
		a.MovImm(s0, l.c)
	}
	switch storage {
	case module.KindI8:
		if word {
			a.MovZX8(s0, r)
			a.Store(base, off, s0)
		} else {
			a.Store8(base, off, r)
		}
	case module.KindI16:
		if word {
			a.MovRR32(s0, r)
			a.OpImm(amd64.And, s0, 0xffff, false)
			a.Store(base, off, s0)
		} else {
			a.Store16(base, off, r)
		}
	case module.KindI32, module.KindF32:
		if word {
			a.MovRR32(s0, r)
			a.Store(base, off, s0)
		} else {
			a.Store32(base, off, r)
		}
	default:
		a.Store(base, off, r)
	}
}

// element leaves in s1 the address of element v.Args[0] of the array whose
// pointer word is in s1, of storage type t, trapping past its length; s0
// is lost.
func (g *gen) element(v *Value, t module.Kind) {
	a := g.a
	lay := g.req.Layout
	g.movG(s0, v.Args[0])
	a.CmpMem(s0, s1, lay.ArrayLen, true)
	a.Jcc(amd64.CondAE, g.trap(wjit.TrapArrayOutOfBounds, v.At))
	shift := uint8(4)
	if t == module.KindRef {
		a.Load(s1, s1, lay.ArrayRefs)
	} else {
		a.Load(s1, s1, lay.ArrayBytes)
		shift = map[int]uint8{1: 0, 2: 1, 4: 2, 8: 3}[storageSize(t)]
	}
	if shift > 0 {
		a.ShiftImm(amd64.Shl, s0, shift, true)
	}
	a.Op(amd64.Add, s1, s0, true)
}

// gcRef lowers a GC instruction on objects (OpRef), reporting whether v is
// one.
func (g *gen) gcRef(v *Value) bool {
	a := g.a
	lay := g.req.Layout
	dst, src := refOff(uint32(v.Aux2>>32)), refOff(uint32(v.Aux2))
	idx := v.Aux >> 8
	switch v.Aux & 0xff {
	case refStructGet:
		g.object(src, v.At)
		if gcStorage(idx) == module.KindRef {
			g.refMove(s0, dst, s1, gcOff(idx))
			break
		}
		g.loadNum(v, s1, gcOff(idx)+8, gcStorage(idx), gcSigned(idx))
	case refStructSet:
		g.object(dst, v.At)
		if gcStorage(idx) == module.KindRef {
			g.refMove(s1, gcOff(idx), s0, src)
			break
		}
		g.storeNum(v.Args[0], s1, gcOff(idx)+8, gcStorage(idx), true)
	case refArrayGet:
		g.object(src, v.At)
		g.element(v, gcStorage(idx))
		if gcStorage(idx) == module.KindRef {
			a.Load(s0, rCtx, offR)
			g.refMove(s0, dst, s1, 0)
			break
		}
		g.loadNum(v, s1, 0, gcStorage(idx), gcSigned(idx))
	case refArraySet:
		g.object(dst, v.At)
		g.element(v, gcStorage(idx))
		if gcStorage(idx) == module.KindRef {
			a.Load(s0, rCtx, offR)
			g.refMove(s1, 0, s0, src)
			break
		}
		g.storeNum(v.Args[1], s1, 0, gcStorage(idx), false)
	case refArrayLen:
		g.object(src, v.At)
		t := g.defG(v)
		a.LoadU32(t, s1, lay.ArrayLen)
		g.done(v, int(t))
	case refI31:
		g.movG(s1, v.Args[0])
		a.OpImm(amd64.And, s1, 0x7fffffff, false)
		a.Load(s0, rCtx, offR)
		a.Store(s0, dst+8, s1)
		a.MovImm(s1, lay.I31Mark)
		a.MovQToX(x0, s1)
		g.barrierStore(s0, dst, s1)
	case refI31Get:
		g.object(src, v.At)
		t := g.defG(v)
		a.LoadU32(t, s0, src+8)
		if idx == 1 {
			a.ShiftImm(amd64.Shl, t, 1, false)
			a.ShiftImm(amd64.Sar, t, 1, false)
		}
		g.done(v, int(t))
	case refTest, refCast:
		g.testRef(v, src)
	default:
		return false
	}
	return true
}

// testRef lowers ref.test, a 1 or a 0, and ref.cast, which traps where the
// test fails, of the reference in the slot at src from R. s1 holds its
// pointer word, s0 what is read of the object.
func (g *gen) testRef(v *Value, src int32) {
	a := g.a
	lay := g.req.Layout
	idx := v.Aux >> 8
	nullable := idx>>32&1 != 0
	test, canon, depth := heapTestOf(g.req.Module, module.HeapType(uint32(idx)))
	cast := v.Aux&0xff == refCast
	ok, fail := a.NewLabel(), a.NewLabel()
	if cast {
		fail = g.trap(wjit.TrapIllegalCast, v.At)
	}
	a.Load(s1, rCtx, offR)
	a.Load(s1, s1, src)
	a.Op(amd64.Test, s1, s1, true)
	if nullable {
		a.Jcc(amd64.CondE, ok)
	} else {
		a.Jcc(amd64.CondE, fail)
	}
	isI31 := func(to amd64.Label) {
		a.MovImm(s0, lay.I31Mark)
		a.Op(amd64.Cmp, s1, s0, true)
		a.Jcc(amd64.CondE, to)
	}
	kindIs := func(k uint8, to amd64.Label) {
		a.LoadU8(s0, s1, lay.Kind)
		a.OpImm(amd64.Cmp, s0, int32(k), false)
		a.Jcc(amd64.CondE, to)
	}
	switch test {
	case testAll:
		a.Jmp(ok)
	case testNone:
		a.Jmp(fail)
	case testI31:
		isI31(ok)
		a.Jmp(fail)
	case testEq:
		isI31(ok)
		kindIs(lay.KindStruct, ok)
		kindIs(lay.KindArray, ok)
		a.Jmp(fail)
	case testStruct, testArray:
		isI31(fail)
		k := lay.KindStruct
		if test == testArray {
			k = lay.KindArray
		}
		kindIs(k, ok)
		a.Jmp(fail)
	case testIndex:
		isI31(fail)
		a.Load(s1, s1, lay.RTT)
		a.Op(amd64.Test, s1, s1, true)
		a.Jcc(amd64.CondE, fail)
		a.Load(s0, s1, lay.Supers+8)
		a.OpImm(amd64.Cmp, s0, int32(depth), true)
		a.Jcc(amd64.CondBE, fail)
		a.Load(s1, s1, lay.Supers)
		a.LoadU32(s0, s1, int32(4*depth))
		a.OpImm(amd64.Cmp, s0, int32(canon), false)
		a.Jcc(amd64.CondNE, fail)
		a.Jmp(ok)
	}
	if cast {
		a.Bind(ok)
		return
	}
	done := a.NewLabel()
	t := g.defG(v)
	a.Bind(fail)
	a.Op(amd64.Xor, t, t, false)
	a.Jmp(done)
	a.Bind(ok)
	a.MovImm(t, 1)
	a.Bind(done)
	g.done(v, int(t))
}
