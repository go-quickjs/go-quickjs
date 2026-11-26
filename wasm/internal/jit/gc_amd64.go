package jit

import (
	"unsafe"

	"github.com/go-quickjs/go-quickjs/internal/jit/asm/amd64"
	"github.com/go-quickjs/go-quickjs/wasm/internal/module"
)

// loadWord loads the word at m into r.
func (c *compiler) loadWord(r amd64.Reg, m mem) { c.a.Load(r, m.base, m.off) }

// jumpZero goes to l if r is zero.
func (c *compiler) jumpZero(r amd64.Reg, l amd64.Label) {
	c.a.Op(amd64.Test, r, r, true)
	c.a.Jcc(amd64.CondE, l)
}

// poolTake takes an object of type ti -- a struct, or for class 0 or
// more an array whose block is of the class -- from the level's pool of
// them into p, leaving in q the address of the pool's word of it, or goes
// to slow where there is none.
func (c *compiler) poolTake(p, q amd64.Reg, ti uint32, class int, slow amd64.Label) {
	a := c.a
	l := c.req.Layout
	a.Load(p, rCtx, offPools)
	c.jumpZero(p, slow)
	a.Load(p, p, int32(8*ti))
	c.jumpZero(p, slow)
	if class >= 0 {
		a.Load(p, p, int32(8*class))
		c.jumpZero(p, slow)
	}
	a.Load(q, p, l.PoolCount)
	c.jumpZero(q, slow)
	a.OpImm(amd64.Sub, q, 1, true)
	a.Store(p, l.PoolCount, q)
	a.LeaIdx(q, p, q, 8, l.PoolObjs)
	a.Load(p, q, 0)
}

// poolTakeClass is poolTake of an array of type ti whose block's class is
// in k.
func (c *compiler) poolTakeClass(p, q, k amd64.Reg, ti uint32, slow amd64.Label) {
	a := c.a
	l := c.req.Layout
	a.Load(p, rCtx, offPools)
	c.jumpZero(p, slow)
	a.Load(p, p, int32(8*ti))
	c.jumpZero(p, slow)
	a.LoadIdxScaled(p, p, k, 3, 0)
	c.jumpZero(p, slow)
	a.Load(q, p, l.PoolCount)
	c.jumpZero(q, slow)
	a.OpImm(amd64.Sub, q, 1, true)
	a.Store(p, l.PoolCount, q)
	a.LeaIdx(q, p, q, 8, l.PoolObjs)
	a.Load(p, q, 0)
}

// loadLength loads into r the length an array is made of, v, an i32 in
// its slot, zero-extended; first says it is the site's first load, which
// leaves it in the context's Arg2 for the exits too.
func (c *compiler) loadLength(r amd64.Reg, v value, first bool) {
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
func (c *compiler) arrayClassAt(r amd64.Reg, shift uint8, big amd64.Label) {
	a := c.a
	l := c.req.Layout
	if shift > 0 {
		a.ShiftImm(amd64.Shl, r, shift, true)
	}
	a.OpImm(amd64.Cmp, r, int32(16*(len(l.ArrayClass)-1)), true)
	a.Jcc(amd64.CondA, big)
	a.OpImm(amd64.Add, r, 15, true)
	a.ShiftImm(amd64.Shr, r, 4, true)
	a.MovImm(rTmp, uint64(uintptr(unsafe.Pointer(&l.ArrayClass[0]))))
	a.Op(amd64.Add, r, rTmp, true)
	a.LoadU8(r, r, 0)
}

// segRange checks the range of k elements of 1<<unit bytes from src, an
// i32 zero-extended, of the segment whose entry is entry bytes into the
// table at the context's off (Ctx.Datas, Ctx.Elems), going to oob past the
// segment's length. e is lost.
func (c *compiler) segRange(e, k amd64.Reg, src value, off, entry int32, unit uint8, oob amd64.Label) {
	a := c.a
	a.MovRR(e, k)
	if unit > 0 {
		a.ShiftImm(amd64.Shl, e, unit, true)
	}
	c.loadLength(rTmp, src, false)
	a.Op(amd64.Add, e, rTmp, true)
	a.Load(rTmp, rCtx, off)
	a.CmpMem(e, rTmp, entry+8, true)
	a.Jcc(amd64.CondA, oob)
}

// segCopy copies k elements of 1<<shift bytes of the segment whose entry
// is entry bytes into the table at the context's off, from its element src
// -- a byte offset for a data segment, an index of its references for an
// element segment -- to array p's elements: eight bytes at a time, then
// the bytes left, walking in s and e. k is lost.
func (c *compiler) segCopy(p, k, e, s amd64.Reg, src value, off, entry int32, shift uint8, data bool) {
	a := c.a
	c.loadLength(s, src, false)
	if !data {
		a.ShiftImm(amd64.Shl, s, 4, true)
	}
	a.Load(e, rCtx, off)
	a.Load(e, e, entry)
	a.Op(amd64.Add, s, e, true)
	a.Load(e, p, c.req.Layout.ArrayElems)
	if shift > 0 {
		a.ShiftImm(amd64.Shl, k, shift, true)
	}
	words, bytes, done := a.NewLabel(), a.NewLabel(), a.NewLabel()
	a.Bind(words)
	a.OpImm(amd64.Cmp, k, 8, true)
	a.Jcc(amd64.CondB, bytes)
	a.Load(rTmp, s, 0)
	a.Store(e, 0, rTmp)
	a.OpImm(amd64.Add, s, 8, true)
	a.OpImm(amd64.Add, e, 8, true)
	a.OpImm(amd64.Sub, k, 8, true)
	a.Jmp(words)
	a.Bind(bytes)
	if data && shift < 3 {
		c.jumpZero(k, done)
		a.LoadU8(rTmp, s, 0)
		a.Store8(e, 0, rTmp)
		a.OpImm(amd64.Add, s, 1, true)
		a.OpImm(amd64.Add, e, 1, true)
		a.OpImm(amd64.Sub, k, 1, true)
		a.Jmp(bytes)
	}
	a.Bind(done)
}

// fillLoop runs body for each of the n elements of size bytes from e, e
// moving past each. n and e are lost.
func (c *compiler) fillLoop(e, n amd64.Reg, size int32, body func()) {
	a := c.a
	loop, done := a.NewLabel(), a.NewLabel()
	a.Bind(loop)
	c.jumpZero(n, done)
	body()
	a.OpImm(amd64.Add, e, size, true)
	a.OpImm(amd64.Sub, n, 1, true)
	a.Jmp(loop)
	a.Bind(done)
}

// storeReg stores r at [base + off].
func (c *compiler) storeReg(base amd64.Reg, off int32, r amd64.Reg) { c.a.Store(base, off, r) }

// storeWord stores word v at [base + off].
func (c *compiler) storeWord(base amd64.Reg, off int32, v uint64) {
	if int64(v) == int64(int32(v)) {
		c.a.StoreImm(base, off, int32(v))
		return
	}
	c.a.MovImm(rTmp, v)
	c.a.Store(base, off, rTmp)
}

// andImm is r &= imm, 32 bits.
func (c *compiler) andImm(r amd64.Reg, imm int32) { c.a.OpImm(amd64.And, r, imm, false) }

// signExtend31 sign-extends the i31 in r's low 31 bits to an i32.
func (c *compiler) signExtend31(r amd64.Reg) {
	c.a.ShiftImm(amd64.Shl, r, 1, false)
	c.a.ShiftImm(amd64.Sar, r, 1, false)
}

// loadField pushes the value of type t stored as storage at [base + off]:
// a packed one extended as signed says.
func (c *compiler) loadField(t, storage module.Kind, signed bool, base amd64.Reg, off int32) {
	a := c.a
	if isFloat(t) {
		x := c.allocX(0)
		if t == module.KindF32 {
			a.LoadSS(x, base, off)
		} else {
			a.LoadSD(x, base, off)
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
func (c *compiler) storeField(storage module.Kind, v value, base amd64.Reg, off int32, word bool) {
	a := c.a
	if isFloat(v.t) && v.k == kReg {
		if v.t == module.KindF64 {
			a.StoreSD(base, off, v.xmm())
		} else {
			a.MovDFromX(rTmp, v.xmm())
			if word {
				a.Store(base, off, rTmp)
			} else {
				a.Store32(base, off, rTmp)
			}
		}
		c.free(v)
		return
	}
	r := rTmp
	switch v.k {
	case kReg:
		r = v.gpr()
	case kConst:
		a.MovImm(rTmp, v.c)
	case kSlot:
		a.Load(rTmp, rL, v.off)
	}
	switch storage {
	case module.KindI8:
		if word {
			a.MovZX8(rTmp, r)
			a.Store(base, off, rTmp)
		} else {
			a.Store8(base, off, r)
		}
	case module.KindI16:
		if word {
			a.MovRR32(rTmp, r)
			a.OpImm(amd64.And, rTmp, 0xffff, false)
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
func (c *compiler) packedAddr(p, i amd64.Reg, t module.Kind, oob amd64.Label) {
	a := c.a
	l := c.req.Layout
	a.CmpMem(i, p, l.ArrayLen, true)
	a.Jcc(amd64.CondAE, oob)
	a.Load(p, p, l.ArrayElems)
	if s := storageSize(t); s > 1 {
		a.ShiftImm(amd64.Shl, i, map[int]uint8{2: 1, 4: 2, 8: 3}[s], true)
	}
	a.Op(amd64.Add, p, i, true)
}

// refTest sets the flags by whether the reference at src is of heap type
// h -- null is if nullable -- returning the condition under which it is.
func (c *compiler) refTest(src mem, h module.HeapType, nullable bool) amd64.Cond {
	a := c.a
	l := c.req.Layout
	test, canon, depth := c.heapTestOf(h)
	r := c.allocG(0)
	ok, fail, done := a.NewLabel(), a.NewLabel(), a.NewLabel()
	a.Load(r, src.base, src.off)
	a.Op(amd64.Test, r, r, true)
	if nullable {
		a.Jcc(amd64.CondE, ok)
	} else {
		a.Jcc(amd64.CondE, fail)
	}
	isI31 := func(to amd64.Label) {
		a.MovImm(rTmp, l.I31Mark)
		a.Op(amd64.Cmp, r, rTmp, true)
		a.Jcc(amd64.CondE, to)
	}
	kindIs := func(k uint8, to amd64.Label) {
		a.LoadU8(rTmp, r, l.Kind)
		a.OpImm(amd64.Cmp, rTmp, int32(k), false)
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
		kindIs(l.KindStruct, ok)
		kindIs(l.KindArray, ok)
		a.Jmp(fail)
	case testStruct, testArray:
		isI31(fail)
		k := l.KindStruct
		if test == testArray {
			k = l.KindArray
		}
		kindIs(k, ok)
		a.Jmp(fail)
	case testIndex:
		isI31(fail)
		a.Load(r, r, l.RTT)
		a.Op(amd64.Test, r, r, true)
		a.Jcc(amd64.CondE, fail)
		a.Load(rTmp, r, l.Supers+8)
		a.OpImm(amd64.Cmp, rTmp, int32(depth), true)
		a.Jcc(amd64.CondBE, fail)
		a.Load(r, r, l.Supers)
		a.LoadU32(rTmp, r, int32(4*depth))
		a.OpImm(amd64.Cmp, rTmp, int32(canon), false)
		a.Jcc(amd64.CondNE, fail)
		a.Jmp(ok)
	}
	a.Bind(fail)
	a.Op(amd64.Xor, r, r, false)
	a.Jmp(done)
	a.Bind(ok)
	a.MovImm(r, 1)
	a.Bind(done)
	a.Op(amd64.Test, r, r, false)
	c.freeG(r)
	return amd64.CondNE
}
