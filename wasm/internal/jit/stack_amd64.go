package jit

import (
	"math"

	"github.com/go-quickjs/go-quickjs/internal/jit/asm/amd64"
	"github.com/go-quickjs/go-quickjs/wasm/internal/module"
)

// The registers. DI holds the context, BX the frame's slots (L), R15
// memory 0's base, R12 the poll counter and R10 the return stack's next
// entry; R11 and X15 are scratch, never cached. SP, BP and R14 (g) are
// never written.
const (
	rCtx  = amd64.RDI
	rL    = amd64.RBX
	rMem  = amd64.R15
	rPoll = amd64.R12
	rRS   = amd64.R10
	rTmp  = amd64.R11
	xTmp  = amd64.XReg(15)
)

// gprOrder is the order the cache hands general registers out in: those no
// instruction needs for itself first, AX, CX and DX -- division's and
// shifts' -- last. The homes of locals (homeGPRs) are taken out of it.
var gprOrder = [...]amd64.Reg{amd64.RSI, amd64.R8, amd64.R9, amd64.R13, amd64.RAX, amd64.RCX, amd64.RDX}

// homeGPRs and homeXRegs are the registers locals live in, the most used
// local the first's.
var (
	homeGPRs  = [...]amd64.Reg{amd64.R13, amd64.R9, amd64.R8}
	homeXRegs = [...]amd64.XReg{14, 13, 12, 11}
)

// numXRegs is the number of SSE registers the cache holds values in, X0
// up.
const numXRegs = 15

// evictG frees general register r of the stack entry holding it, if one
// does, moving it to another register or spilling it.
func (c *compiler) evictG(r amd64.Reg, avoid uint16) {
	for h := range c.stack {
		v := c.stack[h]
		if v.k != kReg || isFloat(v.t) || v.gpr() != r {
			continue
		}
		for _, n := range gprOrder {
			if (c.gused|avoid|1<<r)&(1<<n) == 0 {
				c.gused |= 1 << n
				c.a.MovRR(n, r)
				c.freeG(r)
				c.stack[h].r = uint8(n)
				return
			}
		}
		c.spill(h)
		return
	}
}

// store stores v, of its type, into the word at [base + off], all 64 bits
// of it: an i32's or f32's upper half clear.
func (c *compiler) store(v value, base amd64.Reg, off int32) {
	a := c.a
	switch v.k {
	case kConst:
		bits := v.c
		if v.t == module.KindI32 || v.t == module.KindF32 {
			bits = uint64(uint32(bits))
		}
		if int64(bits) == int64(int32(bits)) {
			a.StoreImm(base, off, int32(bits))
			return
		}
		a.MovImm(rTmp, bits)
		a.Store(base, off, rTmp)
	case kReg:
		switch v.t {
		case module.KindF64:
			a.StoreSD(base, off, v.xmm())
		case module.KindF32:
			a.MovDFromX(rTmp, v.xmm())
			a.Store(base, off, rTmp)
		default:
			a.Store(base, off, v.gpr())
		}
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
func (c *compiler) loadG(r amd64.Reg, v value) {
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

// loadX loads v, a float not in a register, into SSE register x.
func (c *compiler) loadX(x amd64.XReg, v value) {
	a := c.a
	switch v.k {
	case kConst:
		if v.c == 0 {
			a.SSEOp(amd64.XorPD, x, x)
			return
		}
		if v.t == module.KindF32 {
			a.MovImm(rTmp, uint64(uint32(v.c)))
			a.MovDToX(x, rTmp)
			return
		}
		a.MovImm(rTmp, v.c)
		a.MovQToX(x, rTmp)
	case kSlot:
		if v.t == module.KindF32 {
			a.LoadSS(x, rL, v.off)
		} else {
			a.LoadSD(x, rL, v.off)
		}
	case kReg:
		a.SSEOp(amd64.MovAPD, x, v.xmm())
	}
}

// imm32 reports whether integer constant v is an immediate of a 32-bit
// operation, or, wide, of a 64-bit one, which sign-extends it.
func imm32(v value, wide bool) (int32, bool) {
	if v.k != kConst {
		return 0, false
	}
	if !wide {
		return int32(uint32(v.c)), true
	}
	return int32(v.c), int64(v.c) == int64(int32(v.c))
}

// loadXConst loads the double f into x.
func (c *compiler) loadXConst(x amd64.XReg, f float64) {
	c.a.MovImm(rTmp, math.Float64bits(f))
	c.a.MovQToX(x, rTmp)
}
