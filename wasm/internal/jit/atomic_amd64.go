package jit

import (
	"github.com/go-quickjs/go-quickjs/internal/jit/asm/amd64"
	"github.com/go-quickjs/go-quickjs/wasm/internal/module"
)

// On amd64 every atomic store and read-modify-write is a locked
// instruction -- XCHG, LOCK XADD, LOCK CMPXCHG -- so a plain load is a
// sequentially consistent one.

// fence compiles atomic.fence.
func (c *compiler) fence() { c.a.Mfence() }

// zeroExtend clears r's bits above the low width bytes.
func (c *compiler) zeroExtend(r amd64.Reg, width uint64) {
	switch width {
	case 1:
		c.a.MovZX8(r, r)
	case 2:
		c.a.OpImm(amd64.And, r, 0xFFFF, false)
	case 4:
		c.a.MovRR32(r, r)
	}
}

// reserveAX takes AX for the instruction, moving out the stack entry that
// holds it: LOCK CMPXCHG compares with AX and loads into it. It is called
// before the instruction pops its operands, so that none is left in AX.
func (c *compiler) reserveAX() {
	c.evictG(amd64.RAX, 1<<amd64.RAX)
	c.gused |= 1 << amd64.RAX
}

// atomicLoad compiles an atomic load of width bytes, zero-extended to t.
func (c *compiler) atomicLoad(mem uint32, off, width uint64, t module.Kind, at uint32) {
	a := c.a
	base, idx, disp := c.address(c.pop(), mem, off, width, at, 0, true)
	switch width {
	case 1:
		a.LoadU8Idx(idx, base, idx, disp)
	case 2:
		a.LoadU16Idx(idx, base, idx, disp)
	case 4:
		a.LoadU32Idx(idx, base, idx, disp)
	default:
		a.LoadIdx(idx, base, idx, 1, disp)
	}
	c.freeBase(base)
	c.pushReg(t, uint8(idx))
}

// atomicStore compiles an atomic store of an integer's low width bytes: an
// exchange, whose old value is dropped.
func (c *compiler) atomicStore(mem uint32, off, width uint64, at uint32) {
	v := c.pop()
	addr := c.pop()
	rv := c.intoG(v, 0)
	base, idx, disp := c.address(addr, mem, off, width, at, 1<<rv, true)
	c.a.XchgIdx(base, idx, disp, rv, uint8(width))
	c.freeG(rv)
	c.freeG(idx)
	c.freeBase(base)
}

// atomicRMW compiles a read-modify-write of width bytes -- kind '+', '-',
// '&', '|', '^' or '=' -- whose result, of type t, is the old value
// zero-extended. Addition, subtraction and exchange are one instruction;
// the bitwise operations a compare-and-exchange loop.
func (c *compiler) atomicRMW(kind byte, mem uint32, off, width uint64, t module.Kind, at uint32) {
	a := c.a
	if kind == '&' || kind == '|' || kind == '^' {
		c.atomicLoop(kind, mem, off, width, t, at)
		return
	}
	v := c.pop()
	addr := c.pop()
	rv := c.intoG(v, 0)
	base, idx, disp := c.address(addr, mem, off, width, at, 1<<rv, true)
	switch kind {
	case '+':
		a.LockXaddIdx(base, idx, disp, rv, uint8(width))
	case '-':
		a.MovRR(rTmp, rv)
		a.Op(amd64.Xor, rv, rv, false)
		a.Op(amd64.Sub, rv, rTmp, true)
		a.LockXaddIdx(base, idx, disp, rv, uint8(width))
	default:
		a.XchgIdx(base, idx, disp, rv, uint8(width))
	}
	c.zeroExtend(rv, width)
	c.freeG(idx)
	c.freeBase(base)
	c.pushReg(t, uint8(rv))
}

// atomicLoop compiles a bitwise read-modify-write: the old value loaded
// into AX, the new one made from it in the scratch register, and LOCK
// CMPXCHG storing it if the memory still holds the old one, which loads
// the memory's value into AX if not, for the next turn.
func (c *compiler) atomicLoop(kind byte, mem uint32, off, width uint64, t module.Kind, at uint32) {
	a := c.a
	const ax = amd64.RAX
	c.reserveAX()
	v := c.pop()
	addr := c.pop()
	rv := c.intoG(v, 1<<ax)
	base, idx, disp := c.address(addr, mem, off, width, at, 1<<rv|1<<ax, true)
	switch width {
	case 1:
		a.LoadU8Idx(ax, base, idx, disp)
	case 2:
		a.LoadU16Idx(ax, base, idx, disp)
	case 4:
		a.LoadU32Idx(ax, base, idx, disp)
	default:
		a.LoadIdx(ax, base, idx, 1, disp)
	}
	op := amd64.And
	switch kind {
	case '|':
		op = amd64.Or
	case '^':
		op = amd64.Xor
	}
	loop := a.NewLabel()
	a.Bind(loop)
	a.MovRR(rTmp, ax)
	a.Op(op, rTmp, rv, true)
	a.LockCmpxchgIdx(base, idx, disp, rTmp, uint8(width))
	a.Jcc(amd64.CondNE, loop)
	c.freeG(rv)
	c.freeG(idx)
	c.freeBase(base)
	c.pushReg(t, uint8(ax))
}

// atomicCmpxchg compiles a compare-and-exchange of width bytes: LOCK
// CMPXCHG compares the expected value's low width bytes, in AX, with
// memory's; the result is memory's old value, zero-extended to t, which AX
// holds after either outcome, but for its upper bits after a store.
func (c *compiler) atomicCmpxchg(mem uint32, off, width uint64, t module.Kind, at uint32) {
	a := c.a
	const ax = amd64.RAX
	c.reserveAX()
	repl := c.pop()
	exp := c.pop()
	addr := c.pop()
	c.loadG(ax, exp)
	c.free(exp)
	c.gused |= 1 << ax
	rr := c.intoG(repl, 1<<ax)
	base, idx, disp := c.address(addr, mem, off, width, at, 1<<rr|1<<ax, true)
	a.LockCmpxchgIdx(base, idx, disp, rr, uint8(width))
	c.zeroExtend(ax, width)
	c.freeG(rr)
	c.freeG(idx)
	c.freeBase(base)
	c.pushReg(t, uint8(ax))
}
