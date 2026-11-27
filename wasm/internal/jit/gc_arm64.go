package jit

import (
	"unsafe"

	"github.com/go-quickjs/go-quickjs/internal/jit/asm/arm64"
	"github.com/go-quickjs/go-quickjs/wasm/internal/module"
)

// loadWord loads the word at m into r.
func (c *compiler) loadWord(r arm64.Reg, m mem) { c.a.Load(r, m.base, m.off) }

// jumpZero goes to l if r is zero.
func (c *compiler) jumpZero(r arm64.Reg, l arm64.Label) { c.a.Cbz(r, l, true) }

// poolTake takes an object of type ti -- a struct, or for class 0 or
// more an array whose block is of the class -- from the level's pool of
// them into p, leaving in q the address of the pool's word of it, or goes
// to slow where there is none.
func (c *compiler) poolTake(p, q arm64.Reg, ti uint32, class int, slow arm64.Label) {
	a := c.a
	l := c.req.Layout
	a.Load(p, rCtx, offPools)
	a.Cbz(p, slow, true)
	a.Load(p, p, int32(8*ti))
	a.Cbz(p, slow, true)
	if class >= 0 {
		a.Load(p, p, int32(8*class))
		a.Cbz(p, slow, true)
	}
	a.Load(q, p, l.PoolCount)
	a.Cbz(q, slow, true)
	a.AddImm(q, q, -1, true)
	a.Store(p, l.PoolCount, q)
	a.AddLsl(q, p, q, 3)
	a.AddImm(q, q, int64(l.PoolObjs), true)
	a.Load(p, q, 0)
}

// poolTakeClass is poolTake of an array of type ti whose block's class is
// in k.
func (c *compiler) poolTakeClass(p, q, k arm64.Reg, ti uint32, slow arm64.Label) {
	a := c.a
	l := c.req.Layout
	a.Load(p, rCtx, offPools)
	a.Cbz(p, slow, true)
	a.Load(p, p, int32(8*ti))
	a.Cbz(p, slow, true)
	a.AddLsl(p, p, k, 3)
	a.Load(p, p, 0)
	a.Cbz(p, slow, true)
	a.Load(q, p, l.PoolCount)
	a.Cbz(q, slow, true)
	a.AddImm(q, q, -1, true)
	a.Store(p, l.PoolCount, q)
	a.AddLsl(q, p, q, 3)
	a.AddImm(q, q, int64(l.PoolObjs), true)
	a.Load(p, q, 0)
}

// loadLength loads into r the length an array is made of, v, an i32 in
// its slot, zero-extended; first says it is the site's first load, which
// leaves it in the context's Arg2 for the exits too.
func (c *compiler) loadLength(r arm64.Reg, v value, first bool) {
	if v.k == kConst {
		c.a.MovImm(r, uint64(uint32(v.c)))
	} else {
		c.a.LoadU32(r, rL, v.off)
	}
	if first {
		c.a.Store(rCtx, offArg0+16, r)
	}
}

// arrayClassAt makes r, the length of an array of elements of 1<<shift
// bytes, the class of its block (Layout.ArrayClass), or goes to big where
// the elements take more than the largest has room for.
func (c *compiler) arrayClassAt(r arm64.Reg, shift uint8, big arm64.Label) {
	a := c.a
	l := c.req.Layout
	if shift > 0 {
		a.ShiftImm(arm64.Lsl, r, r, shift, true)
	}
	a.CmpImm(r, int64(16*(len(l.ArrayClass)-1)), true)
	a.BCond(arm64.HI, big)
	a.AddImm(r, r, 15, true)
	a.ShiftImm(arm64.Lsr, r, r, 4, true)
	a.MovImm(rTmp, uint64(uintptr(unsafe.Pointer(&l.ArrayClass[0]))))
	a.LoadU8Idx(r, rTmp, r)
}

// segRange checks the range of k elements of 1<<unit bytes from src, an
// i32 zero-extended, of the segment whose entry is entry bytes into the
// table at the context's off (Ctx.Datas, Ctx.Elems), going to oob past the
// segment's length. e is lost.
func (c *compiler) segRange(e, k arm64.Reg, src value, off, entry int32, unit uint8, oob arm64.Label) {
	a := c.a
	c.loadLength(rTmp2, src, false)
	a.AddLsl(e, rTmp2, k, unit)
	a.Load(rTmp2, rCtx, off)
	a.Load(rTmp2, rTmp2, entry+8)
	a.Cmp(e, rTmp2, true)
	a.BCond(arm64.HI, oob)
}

// segCopy copies k elements of 1<<shift bytes of the segment whose entry
// is entry bytes into the table at the context's off, from its element src
// -- a byte offset for a data segment, an index of its references for an
// element segment -- to array p's elements: eight bytes at a time, then
// the bytes left, walking in s and e. k is lost.
func (c *compiler) segCopy(p, k, e, s arm64.Reg, src value, off, entry int32, shift uint8, data bool) {
	a := c.a
	c.loadLength(s, src, false)
	a.Load(e, rCtx, off)
	a.Load(e, e, entry)
	if data {
		a.Op(arm64.Add, s, e, s, true)
	} else {
		a.AddLsl(s, e, s, 4)
	}
	a.Load(e, p, c.req.Layout.ArrayElems)
	if shift > 0 {
		a.ShiftImm(arm64.Lsl, k, k, shift, true)
	}
	words, bytes, done := a.NewLabel(), a.NewLabel(), a.NewLabel()
	a.Bind(words)
	a.CmpImm(k, 8, true)
	a.BCond(arm64.LO, bytes)
	a.Load(rTmp, s, 0)
	a.Store(e, 0, rTmp)
	a.AddImm(s, s, 8, true)
	a.AddImm(e, e, 8, true)
	a.AddImm(k, k, -8, true)
	a.B(words)
	a.Bind(bytes)
	if data && shift < 3 {
		a.Cbz(k, done, true)
		a.LoadU8(rTmp, s, 0)
		a.Store8(e, 0, rTmp)
		a.AddImm(s, s, 1, true)
		a.AddImm(e, e, 1, true)
		a.AddImm(k, k, -1, true)
		a.B(bytes)
	}
	a.Bind(done)
}

// fillLoop runs body for each of the n elements of size bytes from e, e
// moving past each. n and e are lost.
func (c *compiler) fillLoop(e, n arm64.Reg, size int32, body func()) {
	a := c.a
	loop, done := a.NewLabel(), a.NewLabel()
	a.Bind(loop)
	a.Cbz(n, done, true)
	body()
	a.AddImm(e, e, int64(size), true)
	a.SubsImm(n, n, 1, true)
	a.B(loop)
	a.Bind(done)
}

// storeReg stores r at [base + off].
func (c *compiler) storeReg(base arm64.Reg, off int32, r arm64.Reg) { c.a.Store(base, off, r) }

// storeWord stores word v at [base + off].
func (c *compiler) storeWord(base arm64.Reg, off int32, v uint64) {
	c.a.MovImm(rTmp, v)
	c.a.Store(base, off, rTmp)
}

// andImm is r &= imm, 32 bits.
func (c *compiler) andImm(r arm64.Reg, imm int32) {
	if !c.a.LogicImm(arm64.And, r, r, uint64(uint32(imm)), false) {
		c.a.MovImm(rTmp, uint64(uint32(imm)))
		c.a.Op(arm64.And, r, r, rTmp, false)
	}
}

// signExtend31 sign-extends the i31 in r's low 31 bits to an i32.
func (c *compiler) signExtend31(r arm64.Reg) {
	c.a.ShiftImm(arm64.Lsl, r, r, 1, false)
	c.a.ShiftImm(arm64.Asr, r, r, 1, false)
}

// loadField pushes the value of type t stored as storage at [base + off]:
// a packed one extended as signed says.
func (c *compiler) loadField(t, storage module.Kind, signed bool, base arm64.Reg, off int32) {
	a := c.a
	if isFloat(t) {
		x := c.allocX(0)
		if t == module.KindF32 {
			a.LoadS(x, base, off)
		} else {
			a.LoadF(x, base, off)
		}
		c.pushReg(t, uint8(x))
		return
	}
	r := c.allocG(1 << base)
	switch {
	case storage == module.KindI8 && signed:
		a.LoadS8(r, base, off)
		a.MovRR32(r, r)
	case storage == module.KindI8:
		a.LoadU8(r, base, off)
	case storage == module.KindI16 && signed:
		a.LoadS16(r, base, off)
		a.MovRR32(r, r)
	case storage == module.KindI16:
		a.LoadU16(r, base, off)
	case t == module.KindI64:
		a.Load(r, base, off)
	default:
		a.LoadU32(r, base, off)
	}
	c.pushReg(t, uint8(r))
}

// storeField stores v, popped, as storage at [base + off]: its bytes, or,
// word, all 64 bits of the value zero-extended.
func (c *compiler) storeField(storage module.Kind, v value, base arm64.Reg, off int32, word bool) {
	a := c.a
	var r arm64.Reg
	switch {
	case v.k == kReg && isFloat(v.t):
		if v.t == module.KindF64 {
			a.StoreF(base, off, v.xmm())
			c.free(v)
			return
		}
		a.FMovFromS(rTmp, v.xmm())
		r = rTmp
	case v.k == kReg:
		r = v.gpr()
	default:
		r = c.inRegister(v).gpr()
	}
	switch storage {
	case module.KindI8:
		if word {
			a.LogicImm(arm64.And, rTmp, r, 0xff, false)
			a.Store(base, off, rTmp)
		} else {
			a.Store8(base, off, r)
		}
	case module.KindI16:
		if word {
			a.LogicImm(arm64.And, rTmp, r, 0xffff, false)
			a.Store(base, off, rTmp)
		} else {
			a.Store16(base, off, r)
		}
	case module.KindI32, module.KindF32:
		if word {
			a.MovRR32(rTmp, r)
			a.Store(base, off, rTmp)
		} else {
			a.Store32(base, off, r)
		}
	default:
		a.Store(base, off, r)
	}
	c.free(v)
}

// packedAddr makes p, an array, the address of its number element i of
// storage type t, going to oob past its length. i is lost.
func (c *compiler) packedAddr(p, i arm64.Reg, t module.Kind, oob arm64.Label) {
	a := c.a
	l := c.req.Layout
	a.Load(rTmp, p, l.ArrayLen)
	a.Cmp(i, rTmp, true)
	a.BCond(arm64.HS, oob)
	a.Load(p, p, l.ArrayElems)
	a.AddLsl(p, p, i, map[int]uint8{1: 0, 2: 1, 4: 2, 8: 3}[storageSize(t)])
}

// refTest sets the flags by whether the reference at src is of heap type
// h -- null is if nullable -- returning the condition under which it is.
func (c *compiler) refTest(src mem, h module.HeapType, nullable bool) arm64.Cond {
	a := c.a
	l := c.req.Layout
	test, canon, depth := c.heapTestOf(h)
	r := c.allocG(0)
	ok, fail, done := a.NewLabel(), a.NewLabel(), a.NewLabel()
	a.Load(r, src.base, src.off)
	if nullable {
		a.Cbz(r, ok, true)
	} else {
		a.Cbz(r, fail, true)
	}
	isI31 := func(to arm64.Label) {
		a.MovImm(rTmp, l.I31Mark)
		a.Cmp(r, rTmp, true)
		a.BCond(arm64.EQ, to)
	}
	kindIs := func(k uint8, to arm64.Label) {
		a.LoadU8(rTmp, r, l.Kind)
		a.CmpImm(rTmp, int64(k), false)
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
		kindIs(l.KindStruct, ok)
		kindIs(l.KindArray, ok)
		a.B(fail)
	case testStruct, testArray:
		isI31(fail)
		k := l.KindStruct
		if test == testArray {
			k = l.KindArray
		}
		kindIs(k, ok)
		a.B(fail)
	case testIndex:
		isI31(fail)
		a.Load(r, r, l.RTT)
		a.Cbz(r, fail, true)
		if depth >= l.SupersKept {
			SupersWalk(a, l, r, rTmp, canon, depth, ok, fail)
			break
		}
		a.Load(rTmp, r, l.Supers+8)
		a.MovImm(rTmp2, uint64(depth))
		a.Cmp(rTmp, rTmp2, true)
		a.BCond(arm64.LS, fail)
		a.Load(r, r, l.Supers)
		a.LoadU32(rTmp, r, int32(4*depth))
		a.MovImm(rTmp2, uint64(canon))
		a.Cmp(rTmp, rTmp2, false)
		a.BCond(arm64.NE, fail)
		a.B(ok)
	}
	a.Bind(fail)
	a.MovRR(r, arm64.ZR)
	a.B(done)
	a.Bind(ok)
	a.MovImm(r, 1)
	a.Bind(done)
	a.CmpImm(r, 0, true)
	c.freeG(r)
	return arm64.NE
}

// SupersWalk goes to yes or no by whether the type whose runtime
// information r points to has canonical type canon at depth depth of its
// chain of supertypes, depth past those the type keeps at hand
// (Layout.SupersKept): it walks the chain from the type, to the type's
// jump where the jump is not above depth and to its supertype otherwise,
// until the depth, in steps logarithmic in the chain's length -- no call,
// and too few steps to need a poll. r and t are its scratch.
func SupersWalk(a *arm64.Asm, l *Layout, r, t arm64.Reg, canon uint32, depth int, yes, no arm64.Label) {
	loop, up, at := a.NewLabel(), a.NewLabel(), a.NewLabel()
	a.Bind(loop)
	a.LoadU32(t, r, l.RTTDepth)
	a.CmpImm(t, int64(depth), false)
	a.BCond(arm64.LO, no)
	a.BCond(arm64.EQ, at)
	a.LoadU32(t, r, l.RTTJumpDepth)
	a.CmpImm(t, int64(depth), false)
	a.BCond(arm64.LO, up)
	a.Load(r, r, l.RTTJump)
	a.B(loop)
	a.Bind(up)
	a.Load(r, r, l.RTTSuper)
	a.B(loop)
	a.Bind(at)
	a.LoadU32(t, r, l.RTTCanon)
	a.CmpImm(t, int64(canon), false)
	a.BCond(arm64.NE, no)
	a.B(yes)
}
