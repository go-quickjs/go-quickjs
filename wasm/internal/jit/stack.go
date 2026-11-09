//go:build amd64 || arm64

package jit

import "github.com/go-quickjs/go-quickjs/wasm/internal/module"

// vkind is where an operand stack entry's value is.
type vkind uint8

const (
	// kConst: the constant c (an i32's or f32's bits zero-extended).
	kConst vkind = iota
	// kReg: register r, a general one for an integer, a float one for a
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

func (v value) gpr() reg  { return reg(v.r) }
func (v value) xmm() freg { return freg(v.r) }

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

func (c *compiler) freeG(r reg)  { c.gused &^= 1 << r }
func (c *compiler) freeX(r freg) { c.xused &^= 1 << r }

// allocG returns a free general register not in avoid, spilling the lowest
// entry that holds one if none is free.
func (c *compiler) allocG(avoid regMask) reg {
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

// allocX returns a free float register, spilling as allocG does: the
// cache's are registers 0 up to numXRegs.
func (c *compiler) allocX(avoid regMask) freg {
	for r := freg(0); r < numXRegs; r++ {
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
	panic("wasm jit: no float register to spill")
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

// intoG returns a general register holding integer v that the instruction
// owns and may write: v's own, or a new one not in avoid.
func (c *compiler) intoG(v value, avoid regMask) reg {
	if v.k == kReg && avoid&(1<<v.r) == 0 {
		return v.gpr()
	}
	r := c.allocG(avoid)
	c.loadG(r, v)
	c.free(v)
	return r
}

// intoX returns a float register holding float v that the instruction
// owns.
func (c *compiler) intoX(v value) freg {
	if v.k == kReg {
		return v.xmm()
	}
	x := c.allocX(0)
	c.loadX(x, v)
	return x
}

// readX returns a float register holding float v for the instruction to
// read: v's own, or the scratch register.
func (c *compiler) readX(v value) freg {
	if v.k == kReg {
		return v.xmm()
	}
	c.loadX(xTmp, v)
	return xTmp
}
