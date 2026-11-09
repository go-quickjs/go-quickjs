package jit

import (
	"math"

	"github.com/go-quickjs/go-quickjs/internal/jit/asm/amd64"
	"github.com/go-quickjs/go-quickjs/wasm/internal/module"
)

// The registers. DI holds the context, BX the frame's slots (L) and R15
// memory 0's base; R11 and X15 are scratch, never cached. SP, BP and R14
// (g) are never written.
const (
	rCtx = amd64.RDI
	rL   = amd64.RBX
	rMem = amd64.R15
	rTmp = amd64.R11
	xTmp = amd64.XReg(15)
)

// gprOrder is the order the cache hands general registers out in: those no
// instruction needs for itself first, AX, CX and DX -- division's and
// shifts' -- last.
var gprOrder = [...]amd64.Reg{amd64.RSI, amd64.R8, amd64.R9, amd64.R10, amd64.R12, amd64.R13, amd64.RAX, amd64.RCX, amd64.RDX}

// numXRegs is the number of SSE registers the cache holds values in, X0
// up.
const numXRegs = 15

// vkind is where an operand stack entry's value is.
type vkind uint8

const (
	// kConst: the constant c (an i32's or f32's bits zero-extended).
	kConst vkind = iota
	// kReg: register r, a general one for an integer, an SSE one for a
	// float. An i32 in a general register has its upper half clear.
	kReg
	// kSlot: the frame slot at off, the entry's own position's.
	kSlot
	// kFlags: the flags, true where condition c holds; made only for the
	// br_if or if that follows.
	kFlags
)

// value is an operand stack entry.
type value struct {
	t   module.Kind
	k   vkind
	r   uint8
	off int32
	c   uint64
}

func isFloat(t module.Kind) bool { return t == module.KindF32 || t == module.KindF64 }

func (v value) gpr() amd64.Reg  { return amd64.Reg(v.r) }
func (v value) xmm() amd64.XReg { return amd64.XReg(v.r) }

// slotOff is the byte offset of stack position h's slot from L.
func (c *compiler) slotOff(h int) int32 { return int32((c.nlocals + 2*h) * 8) }

// slotIndex is stack position h's slot.
func (c *compiler) slotIndex(h int) uint64 { return uint64(c.nlocals + 2*h) }

func (c *compiler) push(v value) {
	c.stack = append(c.stack, v)
	if h := len(c.stack); h > c.maxHeight {
		c.maxHeight = h
	}
}

func (c *compiler) pushReg(t module.Kind, r uint8) { c.push(value{t: t, k: kReg, r: r}) }

func (c *compiler) pushConst(t module.Kind, bits uint64) { c.push(value{t: t, k: kConst, c: bits}) }

// pushSlot pushes the value of type t in the slot of the position it takes.
func (c *compiler) pushSlot(t module.Kind) {
	c.push(value{t: t, k: kSlot, off: c.slotOff(len(c.stack))})
}

// pop pops the top entry; a register it holds stays taken until the
// instruction frees it or pushes it.
func (c *compiler) pop() value {
	v := c.stack[len(c.stack)-1]
	c.stack = c.stack[:len(c.stack)-1]
	return v
}

// truncate drops the entries from position h up, freeing their registers.
func (c *compiler) truncate(h int) {
	for _, v := range c.stack[h:] {
		c.free(v)
	}
	c.stack = c.stack[:h]
}

// free gives back v's register, if it has one.
func (c *compiler) free(v value) {
	if v.k != kReg {
		return
	}
	if isFloat(v.t) {
		c.xused &^= 1 << v.r
	} else {
		c.gused &^= 1 << v.r
	}
}

func (c *compiler) freeG(r amd64.Reg)  { c.gused &^= 1 << r }
func (c *compiler) freeX(r amd64.XReg) { c.xused &^= 1 << r }

// allocG returns a free general register not in avoid, spilling the lowest
// entry that holds one if none is free.
func (c *compiler) allocG(avoid uint16) amd64.Reg {
	for _, r := range gprOrder {
		if (c.gused|avoid)&(1<<r) == 0 {
			c.gused |= 1 << r
			return r
		}
	}
	for h := range c.stack {
		v := c.stack[h]
		if v.k == kReg && !isFloat(v.t) && avoid&(1<<v.r) == 0 {
			c.spill(h)
			c.gused |= 1 << v.r
			return v.gpr()
		}
	}
	panic("wasm jit: no general register to spill")
}

// allocX returns a free SSE register, spilling as allocG does.
func (c *compiler) allocX(avoid uint16) amd64.XReg {
	for r := amd64.XReg(0); r < numXRegs; r++ {
		if (c.xused|avoid)&(1<<r) == 0 {
			c.xused |= 1 << r
			return r
		}
	}
	for h := range c.stack {
		v := c.stack[h]
		if v.k == kReg && isFloat(v.t) && avoid&(1<<v.r) == 0 {
			c.spill(h)
			c.xused |= 1 << v.r
			return v.xmm()
		}
	}
	panic("wasm jit: no SSE register to spill")
}

// spill stores position h's entry into its slot.
func (c *compiler) spill(h int) {
	v := c.stack[h]
	off := c.slotOff(h)
	if v.k == kSlot && v.off == off {
		return
	}
	c.store(v, rL, off)
	c.free(v)
	c.stack[h] = value{t: v.t, k: kSlot, off: off}
}

// spillAll stores every entry into its slot, as control merges and calls
// want them. It emits nothing that changes the flags.
func (c *compiler) spillAll() {
	for h := range c.stack {
		c.spill(h)
	}
}

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

// intoG returns a general register holding integer v that the instruction
// owns and may write: v's own, or a new one not in avoid.
func (c *compiler) intoG(v value, avoid uint16) amd64.Reg {
	if v.k == kReg && avoid&(1<<v.r) == 0 {
		return v.gpr()
	}
	r := c.allocG(avoid)
	c.loadG(r, v)
	c.free(v)
	return r
}

// intoX returns an SSE register holding float v that the instruction owns.
func (c *compiler) intoX(v value) amd64.XReg {
	if v.k == kReg {
		return v.xmm()
	}
	x := c.allocX(0)
	c.loadX(x, v)
	return x
}

// readX returns an SSE register holding float v for the instruction to
// read: v's own, or the scratch register.
func (c *compiler) readX(v value) amd64.XReg {
	if v.k == kReg {
		return v.xmm()
	}
	c.loadX(xTmp, v)
	return xTmp
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
