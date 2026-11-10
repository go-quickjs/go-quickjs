package opt

import (
	"github.com/go-quickjs/go-quickjs/internal/jit/asm/arm64"
	wjit "github.com/go-quickjs/go-quickjs/wasm/internal/jit"
	"github.com/go-quickjs/go-quickjs/wasm/internal/module"
)

// GC objects on arm64. The object's pointer word is loaded from its slot
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
	a.Cbz(s1, g.trap(wjit.TrapNullDereference, at), true)
}

// loadNum makes v the number stored as storage at [base + off], a packed
// one extended as signed says.
func (g *gen) loadNum(v *Value, base arm64.Reg, off int32, storage module.Kind, signed bool) {
	a := g.a
	if v.Type.IsFloat() {
		t := g.defX(v)
		if v.Type == TF32 {
			a.LoadS(t, base, off)
		} else {
			a.LoadF(t, base, off)
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
func (g *gen) storeNum(x *Value, base arm64.Reg, off int32, storage module.Kind, word bool) {
	a := g.a
	l := g.locOf(x)
	var r arm64.Reg
	switch {
	case l.kind == lXMM && x.Type == TF64:
		a.StoreF(base, off, arm64.FReg(l.r))
		return
	case l.kind == lXMM:
		a.FMovFromS(s0, arm64.FReg(l.r))
		r = s0
	default:
		r = g.readG(x, s0)
	}
	switch storage {
	case module.KindI8:
		if word {
			a.LogicImm(arm64.And, s0, r, 0xff, false)
			a.Store(base, off, s0)
		} else {
			a.Store8(base, off, r)
		}
	case module.KindI16:
		if word {
			a.LogicImm(arm64.And, s0, r, 0xffff, false)
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
// and D31 are lost.
func (g *gen) element(v *Value, t module.Kind) {
	a := g.a
	lay := g.req.Layout
	oob := g.trap(wjit.TrapArrayOutOfBounds, v.At)
	i := s0
	if l := g.locOf(v.Args[0]); l.kind == lGPR {
		// The index in its register: s0 takes the length.
		i = arm64.Reg(l.r)
		a.Load(s0, s1, lay.ArrayLen)
		a.Cmp(i, s0, true)
		a.BCond(arm64.HS, oob)
	} else {
		// The array's address waits in D31 while s1 takes its length.
		a.FMovToF(x0, s1)
		a.Load(s1, s1, lay.ArrayLen)
		g.movG(s0, v.Args[0])
		a.Cmp(s0, s1, true)
		a.BCond(arm64.HS, oob)
		a.FMovFromF(s1, x0)
	}
	a.Load(s1, s1, lay.ArrayElems)
	shift := uint8(4)
	if t != module.KindRef {
		shift = map[int]uint8{1: 0, 2: 1, 4: 2, 8: 3}[storageSize(t)]
	}
	a.AddLsl(s1, s1, i, shift)
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
		a.LogicImm(arm64.And, s1, s1, 0x7fffffff, false)
		a.Load(s0, rCtx, offR)
		a.Store(s0, dst+8, s1)
		a.MovImm(s1, lay.I31Mark)
		a.FMovToF(x0, s1)
		g.barrierStore(s0, dst, s1)
	case refI31Get:
		g.object(src, v.At)
		t := g.defG(v)
		a.LoadU32(t, s0, src+8)
		if idx == 1 {
			a.ShiftImm(arm64.Lsl, t, t, 1, false)
			a.ShiftImm(arm64.Asr, t, t, 1, false)
		}
		g.done(v, int(t))
	case refTest, refCast:
		g.testRef(v, src)
	case refStructNew:
		g.structNew(v, dst)
	case refArrayNew:
		g.arrayNew(v, dst)
	default:
		return false
	}
	return true
}

// structNew lowers refStructNew v, its struct's reference into the slot
// at dst from R: the struct taken from the level's pool of its type's, its
// fields stored -- a reference plainly, as the baseline's (its structNew)
// -- the struct into dst, and last the pool's word of it cleared, both
// through the barrier, so that the collector sees the struct in the pool
// until it is in its slot. Where
// the pool is empty, or the level has no pools, the code exits for the
// engine to fill it, every register kept, and takes again.
func (g *gen) structNew(v *Value, dst int32) {
	a := g.a
	lay := g.req.Layout
	m := g.req.Module
	idx := v.Aux >> 8
	ti := uint32(idx >> 1)
	fs := newFields(m, idx)
	retry, slow := a.NewLabel(), a.NewLabel()
	g.ool = append(g.ool, stub{kind: stubAlloc, l: slow, to: retry, code: uint64(ti), at: v.At})
	a.Bind(retry)
	g.mark("struct.new %d", ti)
	a.Load(s1, rCtx, offPools)
	a.Cbz(s1, slow, true)
	a.Load(s1, s1, int32(8*ti))
	a.Cbz(s1, slow, true)
	a.Load(s0, s1, lay.PoolCount)
	a.Cbz(s0, slow, true)
	a.AddImm(s0, s0, -1, true)
	a.Store(s1, lay.PoolCount, s0)
	a.AddLsl(s1, s1, s0, 3)
	a.Load(s1, s1, lay.PoolObjs)
	j := 0
	for i, f := range fs {
		if !f.Type.IsRef() {
			g.storeNum(v.Args[j], s1, int32(16*(i+1))+8, f.Type.Kind, true)
			j++
		}
	}
	if hasRefField(fs) {
		a.Load(s0, rCtx, offR)
		for i, f := range fs {
			if f.Type.IsRef() {
				a.LoadF(x0, s0, dst+int32(16*i))
				a.StoreF(s1, int32(16*(i+1)), x0)
				a.LoadF(x0, s0, dst+int32(16*i)+8)
				a.StoreF(s1, int32(16*(i+1))+8, x0)
			}
		}
	}
	a.FMovToF(x0, s1)
	a.Load(s0, rCtx, offR)
	a.Store(s0, dst+8, arm64.ZR)
	g.barrierStore(s0, dst, s1)
	g.taken(ti, -1)
	a.FMovToF(x0, arm64.ZR)
	g.barrierStore(s1, 0, s0)
}

// arrayNew lowers refArrayNew v, its array's reference into the slot at
// dst from R, as structNew does a struct: the array taken from the level's
// pool of the arrays of its type and class, its length and its elements
// stored -- a reference plainly -- the array into dst, and last the pool's
// word of it cleared, both through the barrier.
func (g *gen) arrayNew(v *Value, dst int32) {
	a := g.a
	lay := g.req.Layout
	m := g.req.Module
	idx := v.Aux >> 8
	ti, n, mode := uint32(idx), uint32(idx>>32&0xffff), idx>>48
	et := m.Types[ti].Array.Elem.Type
	class, _ := arrayPooled(m, lay.ArrayClass, ti, n, false)
	retry, slow := a.NewLabel(), a.NewLabel()
	g.ool = append(g.ool, stub{kind: stubAlloc, l: slow, to: retry, code: uint64(ti) | uint64(class+1)<<32, at: v.At})
	a.Bind(retry)
	g.mark("array.new %d %d", ti, n)
	a.Load(s1, rCtx, offPools)
	a.Cbz(s1, slow, true)
	a.Load(s1, s1, int32(8*ti))
	a.Cbz(s1, slow, true)
	a.Load(s1, s1, int32(8*class))
	a.Cbz(s1, slow, true)
	a.Load(s0, s1, lay.PoolCount)
	a.Cbz(s0, slow, true)
	a.AddImm(s0, s0, -1, true)
	a.Store(s1, lay.PoolCount, s0)
	a.AddLsl(s1, s1, s0, 3)
	a.Load(s1, s1, lay.PoolObjs)
	a.MovImm(s0, uint64(n))
	a.Store(s1, lay.ArrayLen, s0)
	size := int32(elemSize(et.Kind))
	switch {
	case mode == arrayDefault:
	case et.IsRef():
		a.Load(s0, rCtx, offR)
		for w := int32(0); w < 16; w += 8 {
			for i := int32(0); i < int32(n); i++ {
				src := dst + w
				if mode == arrayFixed {
					src += 16 * i
				}
				if i == 0 || mode == arrayFixed {
					a.LoadF(x0, s0, src)
				}
				a.StoreF(s1, lay.ArrayBlock+16*i+w, x0)
			}
		}
	default:
		for i := int32(0); i < int32(n); i++ {
			x := v.Args[0]
			if mode == arrayFixed {
				x = v.Args[i]
			}
			g.storeNum(x, s1, lay.ArrayBlock+i*size, et.Kind, false)
		}
	}
	a.FMovToF(x0, s1)
	a.Load(s0, rCtx, offR)
	a.Store(s0, dst+8, arm64.ZR)
	g.barrierStore(s0, dst, s1)
	g.taken(ti, class)
	a.FMovToF(x0, arm64.ZR)
	g.barrierStore(s1, 0, s0)
}

// taken loads into s1 the address of the word of the level's pool of
// objects of type ti -- structs, or for class 0 or more arrays of that
// class -- that holds the one last taken; s0 is lost.
func (g *gen) taken(ti uint32, class int) {
	a := g.a
	lay := g.req.Layout
	a.Load(s1, rCtx, offPools)
	a.Load(s1, s1, int32(8*ti))
	if class >= 0 {
		a.Load(s1, s1, int32(8*class))
	}
	a.Load(s0, s1, lay.PoolCount)
	a.AddLsl(s1, s1, s0, 3)
	a.AddImm(s1, s1, int64(lay.PoolObjs), true)
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
	if nullable {
		a.Cbz(s1, ok, true)
	} else {
		a.Cbz(s1, fail, true)
	}
	isI31 := func(to arm64.Label) {
		a.MovImm(s0, lay.I31Mark)
		a.Cmp(s1, s0, true)
		a.BCond(arm64.EQ, to)
	}
	kindIs := func(k uint8, to arm64.Label) {
		a.LoadU8(s0, s1, lay.Kind)
		a.CmpImm(s0, int64(k), false)
		a.BCond(arm64.EQ, to)
	}
	switch test {
	case testAll:
		a.B(ok)
	case testNone:
		a.B(fail)
	case testI31:
		isI31(ok)
		a.B(fail)
	case testEq:
		isI31(ok)
		kindIs(lay.KindStruct, ok)
		kindIs(lay.KindArray, ok)
		a.B(fail)
	case testStruct, testArray:
		isI31(fail)
		k := lay.KindStruct
		if test == testArray {
			k = lay.KindArray
		}
		kindIs(k, ok)
		a.B(fail)
	case testIndex:
		isI31(fail)
		a.Load(s1, s1, lay.RTT)
		a.Cbz(s1, fail, true)
		// The supertypes' number, past depth; then the one at depth.
		a.Load(s0, s1, lay.Supers+8)
		a.CmpImm(s0, int64(depth), true)
		a.BCond(arm64.LS, fail)
		a.Load(s1, s1, lay.Supers)
		a.LoadU32(s0, s1, int32(4*depth))
		a.MovImm(s1, uint64(canon))
		a.Cmp(s0, s1, false)
		a.BCond(arm64.NE, fail)
		a.B(ok)
	}
	if cast {
		a.Bind(ok)
		return
	}
	done := a.NewLabel()
	t := g.defG(v)
	a.Bind(fail)
	a.MovRR(t, arm64.ZR)
	a.B(done)
	a.Bind(ok)
	a.MovImm(t, 1)
	a.Bind(done)
	g.done(v, int(t))
}
