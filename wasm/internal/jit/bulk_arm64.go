package jit

import (
	"math"

	"github.com/go-quickjs/go-quickjs/internal/jit/asm/arm64"
)

// The bulk instructions on arm64 (bulk.go). A copy and a fill take X1, X2
// and X3, free once every entry is in its slot, and loop over blocks of 32
// bytes in Q31 and Q30 (LDP and STP, the address moving past each block),
// then of 8 in X16, then bytes.

// The registers of a copy's or a fill's operands.
const (
	bulkDst = arm64.Reg(1)
	bulkSrc = arm64.Reg(2)
	bulkLen = arm64.Reg(3)
)

// shiftRight shifts r right by n bits, unsigned.
func (c *compiler) shiftRight(r reg, n uint8) { c.a.ShiftImm(arm64.Lsr, r, r, n, true) }

// memSizeInto loads memory mem's size in bytes into r: memory 0's from the
// register the code keeps it in, another's from the memories' table, and a
// shared memory's where its grows store it, by a load-acquire.
func (c *compiler) memSizeInto(r reg, mem uint32) {
	a := c.a
	shared := c.m.Memories[mem].Shared
	switch {
	case mem == 0 && shared:
		a.Load(r, rCtx, offMemSizePtr)
		a.Ldar(r, r, 8)
	case mem == 0:
		a.MovRR(r, rMemSize)
	default:
		d := c.memEntryInto(r, mem)
		if shared {
			a.Load(r, r, d+16)
			a.Ldar(r, r, 8)
		} else {
			a.Load(r, r, d+8)
		}
	}
}

// memEntryInto loads into p the address of memory mem's entry in the
// memories' table (Ctx.Mems), less the displacement it returns.
func (c *compiler) memEntryInto(p reg, mem uint32) int32 {
	a := c.a
	e := uint64(mem) * MemEntryWords * 8
	if e+16 <= math.MaxInt32 {
		a.Load(p, rCtx, offMems)
		return int32(e)
	}
	a.MovImm(p, e)
	a.Load(rTmp2, rCtx, offMems)
	a.Op(arm64.Add, p, p, rTmp2, true)
	return 0
}

// bulkBase goes to trap unless start plus n, in registers, is within memory
// mem's size -- for an i64 memory a sum that carries out of 64 bits traps
// too -- and returns the register holding the memory's base (boundsCheck),
// not in avoid.
func (c *compiler) bulkBase(start, n reg, mem uint32, trap arm64.Label, avoid regMask) reg {
	a := c.a
	if c.m.Memories[mem].Limits.Is64 {
		a.Op(arm64.Adds, rTmp, start, n, true)
		a.BCond(arm64.HS, trap)
	} else {
		a.Op(arm64.Add, rTmp, start, n, true)
	}
	return c.boundsCheck(mem, trap, avoid)
}

// bulkLoop emits a loop over blocks of step bytes while bulkLen has one:
// body moves a block.
func (c *compiler) bulkLoop(step int64, body func()) {
	a := c.a
	top, next := a.NewLabel(), a.NewLabel()
	a.Bind(top)
	a.CmpImm(bulkLen, step, true)
	a.BCond(arm64.LO, next)
	body()
	a.AddImm(bulkLen, bulkLen, -step, true)
	a.B(top)
	a.Bind(next)
}

// memoryCopy compiles memory.copy from memory sm to memory dm.
func (c *compiler) memoryCopy(dm, sm uint32, at uint32) {
	a := c.a
	d, s, n := c.bulkOperands()
	c.loadG(bulkDst, d)
	c.loadG(bulkSrc, s)
	c.loadG(bulkLen, n)
	oob := c.trap(TrapMemOutOfBounds, at)
	ops := regMask(1<<bulkDst | 1<<bulkSrc | 1<<bulkLen)
	sb := c.bulkBase(bulkSrc, bulkLen, sm, oob, ops)
	db := c.bulkBase(bulkDst, bulkLen, dm, oob, ops|1<<sb)
	a.Op(arm64.Add, bulkSrc, bulkSrc, sb, true)
	a.Op(arm64.Add, bulkDst, bulkDst, db, true)
	c.freeBase(sb)
	c.freeBase(db)
	up, done := a.NewLabel(), a.NewLabel()
	if dm == sm {
		a.Cmp(bulkDst, bulkSrc, true)
		a.BCond(arm64.LS, up)
		a.Op(arm64.Add, rTmp, bulkSrc, bulkLen, true)
		a.Cmp(bulkDst, rTmp, true)
		a.BCond(arm64.HS, up)
		// Down from the ends, each block read before it is written.
		a.Op(arm64.Add, bulkSrc, bulkSrc, bulkLen, true)
		a.Op(arm64.Add, bulkDst, bulkDst, bulkLen, true)
		c.bulkLoop(32, func() {
			a.LoadPairQPre(xTmp, xTmp2, bulkSrc, -32)
			a.StorePairQPre(bulkDst, -32, xTmp, xTmp2)
		})
		c.bulkLoop(8, func() {
			a.LoadPre(rTmp, bulkSrc, -8, 8)
			a.StorePre(bulkDst, -8, rTmp, 8)
		})
		c.bulkLoop(1, func() {
			a.LoadPre(rTmp, bulkSrc, -1, 1)
			a.StorePre(bulkDst, -1, rTmp, 1)
		})
		a.B(done)
	}
	a.Bind(up)
	c.bulkLoop(32, func() {
		a.LoadPairQPost(xTmp, xTmp2, bulkSrc, 32)
		a.StorePairQPost(bulkDst, 32, xTmp, xTmp2)
	})
	c.bulkLoop(8, func() {
		a.LoadPost(rTmp, bulkSrc, 8, 8)
		a.StorePost(bulkDst, 8, rTmp, 8)
	})
	c.bulkLoop(1, func() {
		a.LoadPost(rTmp, bulkSrc, 1, 1)
		a.StorePost(bulkDst, 1, rTmp, 1)
	})
	a.Bind(done)
}

// memoryFill compiles memory.fill of memory mem: the byte DUPed into each
// of Q31's.
func (c *compiler) memoryFill(mem uint32, at uint32) {
	a := c.a
	d, v, n := c.bulkOperands()
	c.loadG(bulkDst, d)
	c.loadG(bulkSrc, v)
	c.loadG(bulkLen, n)
	ops := regMask(1<<bulkDst | 1<<bulkSrc | 1<<bulkLen)
	base := c.bulkBase(bulkDst, bulkLen, mem, c.trap(TrapMemOutOfBounds, at), ops)
	a.Op(arm64.Add, bulkDst, bulkDst, base, true)
	c.freeBase(base)
	a.Dup(arm64.V8, xTmp, bulkSrc)
	c.bulkLoop(32, func() { a.StorePairQPost(bulkDst, 32, xTmp, xTmp) })
	a.FMovFromF(rTmp, xTmp)
	c.bulkLoop(8, func() { a.StorePost(bulkDst, 8, rTmp, 8) })
	c.bulkLoop(1, func() { a.StorePost(bulkDst, 1, rTmp, 1) })
}

// tableFill compiles table.fill of table: the range checked against the
// table's elements, then each element stored as a reference is, through
// the barrier routine while the collector marks, which keeps X1, X2 and
// X3.
func (c *compiler) tableFill(table uint32, at uint32) {
	a := c.a
	d, v, n := c.bulkOperands()
	src := c.refOf(v)
	c.loadG(bulkDst, d)
	c.loadG(bulkLen, n)
	elems := c.req.Layout.TableElems
	c.tableEntry(bulkSrc, offTables, table)
	trap := c.trap(TrapTableOutOfBounds, at)
	if c.m.Tables[table].Limits.Is64 {
		a.Op(arm64.Adds, rTmp, bulkDst, bulkLen, true)
		a.BCond(arm64.HS, trap)
	} else {
		a.Op(arm64.Add, rTmp, bulkDst, bulkLen, true)
	}
	a.Load(rTmp2, bulkSrc, elems+8)
	a.Cmp(rTmp, rTmp2, true)
	a.BCond(arm64.HI, trap)
	a.Load(bulkSrc, bulkSrc, elems)
	a.AddLsl(bulkDst, bulkSrc, bulkDst, 4)
	loop, done := a.NewLabel(), a.NewLabel()
	a.Bind(loop)
	a.Cbz(bulkLen, done, true)
	c.refPut(mem{bulkDst, 0}, memWord(src), memWord(mem{src.base, src.off + 8}))
	a.AddImm(bulkDst, bulkDst, 16, true)
	a.SubsImm(bulkLen, bulkLen, 1, true)
	a.B(loop)
	a.Bind(done)
}
