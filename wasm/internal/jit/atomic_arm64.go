package jit

import (
	"github.com/go-quickjs/go-quickjs/internal/jit/asm/arm64"
	"github.com/go-quickjs/go-quickjs/wasm/internal/module"
)

// On arm64 an atomic load is a load-acquire (LDAR), a store a
// store-release (STLR), and a read-modify-write a loop of an exclusive
// load-acquire and store-release (LDAXR, STLXR) that goes round again when
// another thread's access broke the exclusive pair; between the two are
// register operations only. These take an address in one register: the
// memory's base plus the effective address.

// fence compiles atomic.fence.
func (c *compiler) fence() { c.a.DmbIsh() }

// atomicAddress checks an atomic access at addr plus off as address does,
// and returns a register of the instruction's own holding its address in
// memory: the memory's base plus the effective address.
func (c *compiler) atomicAddress(addr value, mem uint32, off, width uint64, at uint32) arm64.Reg {
	base, idx := c.address(addr, mem, off, width, at, true)
	c.a.Op(arm64.Add, idx, base, idx, true)
	c.freeBase(base)
	return idx
}

// atomicLoad compiles an atomic load of width bytes, zero-extended to t.
func (c *compiler) atomicLoad(mem uint32, off, width uint64, t module.Kind, at uint32) {
	p := c.atomicAddress(c.pop(), mem, off, width, at)
	c.a.Ldar(p, p, uint8(width))
	c.pushReg(t, uint8(p))
}

// atomicStore compiles an atomic store of an integer's low width bytes.
func (c *compiler) atomicStore(mem uint32, off, width uint64, at uint32) {
	v := c.pop()
	p := c.atomicAddress(c.pop(), mem, off, width, at)
	rv := arm64.ZR
	if v.k != kConst || v.c != 0 {
		rv = c.readG(v, rTmp)
	}
	c.a.Stlr(rv, p, uint8(width))
	c.free(v)
	c.freeG(p)
}

// atomicRMW compiles a read-modify-write of width bytes -- kind '+', '-',
// '&', '|', '^' or '=' -- whose result, of type t, is the old value,
// zero-extended by the load.
func (c *compiler) atomicRMW(kind byte, mem uint32, off, width uint64, t module.Kind, at uint32) {
	a := c.a
	v := c.pop()
	p := c.atomicAddress(c.pop(), mem, off, width, at)
	rv := c.intoG(v, 1<<p)
	old := c.allocG(1<<p | 1<<rv)
	wide := width == 8
	loop := a.NewLabel()
	a.Bind(loop)
	a.Ldaxr(old, p, uint8(width))
	src := rTmp
	switch kind {
	case '+':
		a.Op(arm64.Add, rTmp, old, rv, wide)
	case '-':
		a.Op(arm64.Sub, rTmp, old, rv, wide)
	case '&':
		a.Op(arm64.And, rTmp, old, rv, wide)
	case '|':
		a.Op(arm64.Orr, rTmp, old, rv, wide)
	case '^':
		a.Op(arm64.Eor, rTmp, old, rv, wide)
	default:
		src = rv
	}
	a.Stlxr(rTmp2, src, p, uint8(width))
	a.Cbnz(rTmp2, loop, false)
	c.freeG(rv)
	c.freeG(p)
	c.pushReg(t, uint8(old))
}

// atomicCmpxchg compiles a compare-and-exchange of width bytes: memory's
// old value, zero-extended to t, is compared with the expected value's low
// width bytes and the replacement's stored if they are equal; the result
// is the old value.
func (c *compiler) atomicCmpxchg(mem uint32, off, width uint64, t module.Kind, at uint32) {
	a := c.a
	repl := c.pop()
	exp := c.pop()
	p := c.atomicAddress(c.pop(), mem, off, width, at)
	re := c.intoG(exp, 1<<p)
	switch width {
	case 1:
		a.LogicImm(arm64.And, re, re, 0xFF, false)
	case 2:
		a.LogicImm(arm64.And, re, re, 0xFFFF, false)
	}
	rr := c.intoG(repl, 1<<p|1<<re)
	old := c.allocG(1<<p | 1<<re | 1<<rr)
	wide := width == 8
	loop, done := a.NewLabel(), a.NewLabel()
	a.Bind(loop)
	a.Ldaxr(old, p, uint8(width))
	a.Cmp(old, re, wide)
	a.BCond(arm64.NE, done)
	a.Stlxr(rTmp2, rr, p, uint8(width))
	a.Cbnz(rTmp2, loop, false)
	a.Bind(done)
	c.freeG(re)
	c.freeG(rr)
	c.freeG(p)
	c.pushReg(t, uint8(old))
}
