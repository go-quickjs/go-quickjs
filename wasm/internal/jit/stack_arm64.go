package jit

import (
	"github.com/go-quickjs/go-quickjs/internal/jit/asm/arm64"
	"github.com/go-quickjs/go-quickjs/wasm/internal/module"
)

// The registers. X0 holds the context, X19 the frame's slots (L), X20
// memory 0's base, X21 its size, X22 the poll counter and X23 the return
// stack's next entry; X16, X17, D30 and D31 are scratch, never cached, and
// X27 is the encoder's own temporary for offsets an instruction cannot
// hold. SP, FP (X29), LR (X30), the platform's X18 and g (X28) are never
// written.
const (
	rCtx     = arm64.Reg(0)
	rL       = arm64.Reg(19)
	rMem     = arm64.Reg(20)
	rMemSize = arm64.Reg(21)
	rPoll    = arm64.Reg(22)
	rRS      = arm64.Reg(23)
	rTmp     = arm64.Reg(16)
	rTmp2    = arm64.Reg(17)
	xTmp     = arm64.FReg(31)
	xTmp2    = arm64.FReg(30)
)

// gprOrder is the order the cache hands general registers out in. The
// homes of locals (homeGPRs) are taken out of it, and are its last, so
// that a function with fewer locals in registers caches values in theirs.
var gprOrder = [...]arm64.Reg{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 24, 25, 26}

// homeGPRs and homeXRegs are the registers locals live in, the most used
// local the first's.
var (
	homeGPRs  = [...]arm64.Reg{26, 25, 24, 15, 14, 13}
	homeXRegs = [...]arm64.FReg{29, 28, 27, 26, 25, 24, 23, 22}
)

// numXRegs is the number of float registers the cache holds values in, D0
// up.
const numXRegs = 30

// store stores v, of its type, into the word at [base + off], all 64 bits
// of it: an i32's or f32's upper half clear. An f32 in a float register
// has the rest of the register clear -- every instruction that makes one
// writes its S view, which clears the rest, or copies a register that has
// -- so its D view is the word.
func (c *compiler) store(v value, base arm64.Reg, off int32) {
	a := c.a
	if v.t == module.KindV128 {
		c.storeVec(v, base, off)
		return
	}
	switch v.k {
	case kConst:
		bits := v.c
		if v.t == module.KindI32 || v.t == module.KindF32 {
			bits = uint64(uint32(bits))
		}
		if bits == 0 {
			a.Store(base, off, arm64.ZR)
			return
		}
		a.MovImm(rTmp, bits)
		a.Store(base, off, rTmp)
	case kReg:
		if isFloat(v.t) {
			a.StoreF(base, off, v.xmm())
			return
		}
		a.Store(base, off, v.gpr())
	case kSlot:
		if base == rL && off == v.off {
			return
		}
		a.Load(rTmp, rL, v.off)
		a.Store(base, off, rTmp)
	default:
		panic("wasm jit: flags stored")
	}
}

// loadG loads v, an integer not in a register, into general register r.
func (c *compiler) loadG(r arm64.Reg, v value) {
	switch v.k {
	case kConst:
		c.a.MovImm(r, v.c)
	case kSlot:
		if v.t == module.KindI64 {
			c.a.Load(r, rL, v.off)
		} else {
			c.a.LoadU32(r, rL, v.off)
		}
	case kReg:
		c.a.MovRR(r, v.gpr())
	default:
		panic("wasm jit: flags loaded")
	}
}

// loadX loads v, a float or a v128 not in a register, into float register
// x.
func (c *compiler) loadX(x arm64.FReg, v value) {
	a := c.a
	if v.t == module.KindV128 {
		c.loadVec(x, v)
		return
	}
	switch v.k {
	case kConst:
		bits := v.c
		if v.t == module.KindF32 {
			bits = uint64(uint32(bits))
		}
		if bits == 0 {
			a.FMovToF(x, arm64.ZR)
			return
		}
		if imm, ok := arm64.FloatImm(bits); ok && v.t == module.KindF64 {
			a.FMovImm(x, imm)
			return
		}
		a.MovImm(rTmp, bits)
		a.FMovToF(x, rTmp)
	case kSlot:
		if v.t == module.KindF32 {
			a.LoadS(x, rL, v.off)
		} else {
			a.LoadF(x, rL, v.off)
		}
	case kReg:
		a.FMov(x, v.xmm())
	}
}

// readG returns a general register holding integer v for the instruction
// to read: v's own, or tmp, loaded.
func (c *compiler) readG(v value, tmp arm64.Reg) arm64.Reg {
	if v.k == kReg {
		return v.gpr()
	}
	c.loadG(tmp, v)
	return tmp
}

// readXIn is readX with the scratch register tmp.
func (c *compiler) readXIn(v value, tmp arm64.FReg) arm64.FReg {
	if v.k == kReg {
		return v.xmm()
	}
	c.loadX(tmp, v)
	return tmp
}

// loadXConst loads the bits of a double, or of a float when single, into
// x.
func (c *compiler) loadXConst(x arm64.FReg, bits uint64, single bool) {
	t := module.KindF64
	if single {
		t = module.KindF32
	}
	c.loadX(x, value{t: t, k: kConst, c: bits})
}
