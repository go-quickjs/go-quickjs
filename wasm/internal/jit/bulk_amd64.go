package jit

import "github.com/go-quickjs/go-quickjs/internal/jit/asm/amd64"

// The bulk instructions on amd64 (bulk.go). A copy and a fill take AX, CX,
// DX and SI, free once every entry is in its slot -- SI is R in a function
// that holds references, loaded again after -- and REP MOVSB and REP STOSB
// take DI, the context, which waits in rTmp meanwhile.

// shiftRight shifts r right by n bits, unsigned.
func (c *compiler) shiftRight(r reg, n uint8) { c.a.ShiftImm(amd64.Shr, r, n, true) }

// memSizeInto loads memory mem's size in bytes into r: memory 0's from the
// context, another's from the memories' table, and a shared memory's where
// its grows store it.
func (c *compiler) memSizeInto(r reg, mem uint32) {
	a := c.a
	shared := c.m.Memories[mem].Shared
	switch {
	case mem == 0 && shared:
		a.Load(r, rCtx, offMemSizePtr)
		a.Load(r, r, 0)
	case mem == 0:
		a.Load(r, rCtx, offMemSize)
	case shared:
		d := c.memEntry(r, mem)
		a.Load(r, r, d+16)
		a.Load(r, r, 0)
	default:
		d := c.memEntry(r, mem)
		a.Load(r, r, d+8)
	}
}

// bulkCheck goes to trap unless start plus n, in registers, is within
// memory mem's size: for an i64 memory a sum that carries out of 64 bits
// traps too. tmp is lost.
func (c *compiler) bulkCheck(start, n, tmp reg, mem uint32, trap amd64.Label) {
	a := c.a
	a.MovRR(rTmp, start)
	a.Op(amd64.Add, rTmp, n, true)
	if c.m.Memories[mem].Limits.Is64 {
		a.Jcc(amd64.CondB, trap)
	}
	shared := c.m.Memories[mem].Shared
	switch {
	case mem == 0 && shared:
		a.Load(tmp, rCtx, offMemSizePtr)
		a.CmpMem(rTmp, tmp, 0, true)
	case mem == 0:
		a.CmpMem(rTmp, rCtx, offMemSize, true)
	case shared:
		d := c.memEntry(tmp, mem)
		a.Load(tmp, tmp, d+16)
		a.CmpMem(rTmp, tmp, 0, true)
	default:
		d := c.memEntry(tmp, mem)
		a.CmpMem(rTmp, tmp, d+8, true)
	}
	a.Jcc(amd64.CondA, trap)
}

// addBase adds memory mem's first byte's address to r. tmp is lost.
func (c *compiler) addBase(r, tmp reg, mem uint32) {
	if mem == 0 {
		c.a.Op(amd64.Add, r, rMem, true)
		return
	}
	d := c.memEntry(tmp, mem)
	c.a.AddMem(r, tmp, d)
}

// memoryCopy compiles memory.copy from memory sm to memory dm.
func (c *compiler) memoryCopy(dm, sm uint32, at uint32) {
	a := c.a
	const ax, cx, dx, si = amd64.RAX, amd64.RCX, amd64.RDX, amd64.RSI
	d, s, n := c.bulkOperands()
	c.loadG(cx, n)
	c.loadG(si, s)
	c.loadG(dx, d)
	oob := c.trap(TrapMemOutOfBounds, at)
	c.bulkCheck(si, cx, ax, sm, oob)
	c.bulkCheck(dx, cx, ax, dm, oob)
	c.addBase(si, ax, sm)
	c.addBase(dx, ax, dm)
	up, done := a.NewLabel(), a.NewLabel()
	if dm == sm {
		a.Op(amd64.Cmp, dx, si, true)
		a.Jcc(amd64.CondBE, up)
		a.LeaIdx(ax, si, cx, 1, 0)
		a.Op(amd64.Cmp, dx, ax, true)
		a.Jcc(amd64.CondAE, up)
		// Down from the end: each word, then each byte, read before the
		// bytes above it are written.
		words, bytes := a.NewLabel(), a.NewLabel()
		a.Bind(words)
		a.OpImm(amd64.Cmp, cx, 8, true)
		a.Jcc(amd64.CondB, bytes)
		a.OpImm(amd64.Sub, cx, 8, true)
		a.LoadIdx(ax, si, cx, 1, 0)
		a.StoreIdx(dx, cx, 1, 0, ax)
		a.Jmp(words)
		a.Bind(bytes)
		a.Op(amd64.Test, cx, cx, true)
		a.Jcc(amd64.CondE, done)
		a.OpImm(amd64.Sub, cx, 1, true)
		a.LoadU8Idx(ax, si, cx, 0)
		a.Store8Idx(dx, cx, 0, ax)
		a.Jmp(bytes)
	}
	a.Bind(up)
	a.MovRR(rTmp, rCtx)
	a.MovRR(rCtx, dx)
	a.RepMovsb()
	a.MovRR(rCtx, rTmp)
	a.Bind(done)
	if c.refs {
		c.loadR()
	}
}

// memoryFill compiles memory.fill of memory mem.
func (c *compiler) memoryFill(mem uint32, at uint32) {
	a := c.a
	const ax, cx, dx, si = amd64.RAX, amd64.RCX, amd64.RDX, amd64.RSI
	d, v, n := c.bulkOperands()
	c.loadG(cx, n)
	c.loadG(ax, v)
	c.loadG(dx, d)
	c.bulkCheck(dx, cx, si, mem, c.trap(TrapMemOutOfBounds, at))
	c.addBase(dx, si, mem)
	a.MovRR(rTmp, rCtx)
	a.MovRR(rCtx, dx)
	a.RepStosb()
	a.MovRR(rCtx, rTmp)
	if c.refs {
		c.loadR()
	}
}

// tableFill compiles table.fill of table: the range checked against the
// table's elements, then each element stored as a reference is, through
// the barrier routine while the collector marks, which keeps AX and CX.
func (c *compiler) tableFill(table uint32, at uint32) {
	a := c.a
	const ax, cx, dx = amd64.RAX, amd64.RCX, amd64.RDX
	d, v, n := c.bulkOperands()
	src := c.refOf(v)
	c.loadG(cx, n)
	c.loadG(dx, d)
	elems := c.req.Layout.TableElems
	c.tableEntry(ax, offTables, table)
	a.MovRR(rTmp, dx)
	a.Op(amd64.Add, rTmp, cx, true)
	trap := c.trap(TrapTableOutOfBounds, at)
	if c.m.Tables[table].Limits.Is64 {
		a.Jcc(amd64.CondB, trap)
	}
	a.CmpMem(rTmp, ax, elems+8, true)
	a.Jcc(amd64.CondA, trap)
	a.Load(ax, ax, elems)
	a.ShiftImm(amd64.Shl, dx, 4, true)
	a.Op(amd64.Add, ax, dx, true)
	loop, done := a.NewLabel(), a.NewLabel()
	a.Bind(loop)
	a.Op(amd64.Test, cx, cx, true)
	a.Jcc(amd64.CondE, done)
	c.refPut(mem{ax, 0}, memWord(src), memWord(mem{src.base, src.off + 8}))
	a.OpImm(amd64.Add, ax, 16, true)
	a.OpImm(amd64.Sub, cx, 1, true)
	a.Jmp(loop)
	a.Bind(done)
}
