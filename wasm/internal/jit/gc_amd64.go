package jit

import (
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
	a.Load(p, p, l.ArrayBytes)
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
