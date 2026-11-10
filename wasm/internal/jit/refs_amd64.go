package jit

import "github.com/go-quickjs/go-quickjs/internal/jit/asm/amd64"

// rR holds R, the frame's reference slots, in a function that holds
// references; the cache never hands it out there.
const rR = amd64.RSI

// refHomeGPRs is the number of integer locals with registers of their own
// in a function that holds references: rR takes one of the cache's seven,
// and a division holds three of what is left while it needs a fourth.
const refHomeGPRs = 2

// condNonZero is the condition testCond's value is not zero under.
const condNonZero = amd64.CondNE

// wbSaved are the general registers the barrier routine keeps across the
// runtime's store, which keeps only DI: every register the code keeps a
// value in. X0-X14 are kept too.
var wbSaved = [...]amd64.Reg{amd64.RAX, amd64.RCX, amd64.RDX, amd64.RBX, amd64.RSI,
	amd64.R8, amd64.R9, amd64.R10, amd64.R12, amd64.R13, amd64.R15}

// loadR loads R from the context.
func (c *compiler) loadR() { c.a.Load(rR, rCtx, offR) }

// advanceR moves the context's R past the frame, for a native call.
func (c *compiler) advanceR() {
	c.a.Lea(amd64.RCX, rR, c.adv)
	c.a.Store(rCtx, offR, amd64.RCX)
}

// restoreR moves the context's R back to the frame after a native call,
// and loads it.
func (c *compiler) restoreR() {
	a := c.a
	a.Load(rR, rCtx, offR)
	a.OpImm(amd64.Sub, rR, c.adv, true)
	a.Store(rCtx, offR, rR)
}

// testBarrier goes to slow while the collector marks.
func (c *compiler) testBarrier(slow amd64.Label) {
	c.a.MovImm(rTmp, c.req.WriteBarrier)
	c.a.TestMem8(rTmp, 0, 0xff)
	c.a.Jcc(amd64.CondNE, slow)
}

// copyWord copies the word at src to dst.
func (c *compiler) copyWord(dst, src mem) {
	c.a.Load(rTmp, src.base, src.off)
	c.a.Store(dst.base, dst.off, rTmp)
}

// refCopy copies the reference at src to dst.
func (c *compiler) refCopy(dst, src mem) {
	if dst == src {
		return
	}
	c.refPut(dst, memWord(src), memWord(mem{src.base, src.off + 8}))
}

// refPut stores the reference of pointer word p and payload word i at dst:
// the payload plainly, the pointer through the barrier routine while the
// collector marks. Neither dst's base nor a word's register is rTmp.
func (c *compiler) refPut(dst mem, p, i word) {
	a := c.a
	switch i.k {
	case wImm:
		if int64(i.c) == int64(int32(i.c)) {
			a.StoreImm(dst.base, dst.off+8, int32(i.c))
		} else {
			a.MovImm(rTmp, i.c)
			a.Store(dst.base, dst.off+8, rTmp)
		}
	case wReg:
		a.Store(dst.base, dst.off+8, i.r)
	case wMem:
		a.LoadSD(xTmp, i.m.base, i.m.off)
		a.StoreSD(dst.base, dst.off+8, xTmp)
	}
	slow, resume := a.NewLabel(), a.NewLabel()
	c.testBarrier(slow)
	switch p.k {
	case wImm:
		if int64(p.c) == int64(int32(p.c)) {
			a.StoreImm(dst.base, dst.off, int32(p.c))
		} else {
			a.MovImm(rTmp, p.c)
			a.Store(dst.base, dst.off, rTmp)
		}
	case wReg:
		a.Store(dst.base, dst.off, p.r)
	case wMem:
		a.Load(rTmp, p.m.base, p.m.off)
		a.Store(dst.base, dst.off, rTmp)
	}
	a.Bind(resume)
	wb := c.wbRoutine()
	c.later(func() {
		a.Bind(slow)
		switch p.k {
		case wImm:
			c.storeCtx(offWBPtr, p.c)
		case wReg:
			a.Store(rCtx, offWBPtr, p.r)
		case wMem:
			a.Load(rTmp, p.m.base, p.m.off)
			a.Store(rCtx, offWBPtr, rTmp)
		}
		a.Lea(rTmp, dst.base, dst.off)
		a.Store(rCtx, offWBSlot, rTmp)
		a.LeaLabel(rTmp, resume)
		a.Jmp(wb)
	})
}

// emitWBRoutine emits the barrier routine at l: the slot's address in the
// context's WBSlot, the pointer in WBPtr, where to go on in rTmp. A nil
// stored over a nil is nothing to store or to shade; anything else is
// stored by the runtime's barrier, every register the code keeps saved
// around it.
func (c *compiler) emitWBRoutine(l amd64.Label) {
	a := c.a
	a.Bind(l)
	a.Store(rCtx, offWBRet, rTmp)
	store := a.NewLabel()
	a.Load(rTmp, rCtx, offWBSlot)
	a.Load(rTmp, rTmp, 0)
	a.Op(amd64.Test, rTmp, rTmp, true)
	a.Jcc(amd64.CondNE, store)
	a.Load(rTmp, rCtx, offWBPtr)
	a.Op(amd64.Test, rTmp, rTmp, true)
	a.Jcc(amd64.CondNE, store)
	a.JmpMem(rCtx, offWBRet)
	a.Bind(store)
	for i, r := range wbSaved {
		a.Store(rCtx, offSave+int32(8*i), r)
	}
	// The SSE registers whole: a v128 may be in one.
	for x := amd64.XReg(0); x < 15; x++ {
		a.StoreVec(rCtx, offSaveV+16*int32(x), x)
	}
	a.Load(amd64.RAX, rCtx, offWBSlot)
	a.Load(amd64.RCX, rCtx, offWBPtr)
	back := a.NewLabel()
	a.LeaLabel(rTmp, back)
	a.MovImm(amd64.RDX, c.req.StoreRef)
	a.JmpReg(amd64.RDX)
	a.Bind(back)
	for i, r := range wbSaved {
		a.Load(r, rCtx, offSave+int32(8*i))
	}
	for x := amd64.XReg(0); x < 15; x++ {
		a.LoadVec(x, rCtx, offSaveV+16*int32(x))
	}
	a.JmpMem(rCtx, offWBRet)
}

// clearRefs makes the frame's reference slots from up to to null, through
// the barrier routine while the collector marks. Nothing but the
// registers the code keeps is live.
func (c *compiler) clearRefs(from, to int) {
	if from >= to {
		return
	}
	a := c.a
	n := to - from
	slow, done := a.NewLabel(), a.NewLabel()
	c.testBarrier(slow)
	if n <= 8 {
		for k := from; k < to; k++ {
			a.StoreImm(rR, int32(16*k), 0)
			a.StoreImm(rR, int32(16*k+8), 0)
		}
	} else {
		a.Lea(amd64.RCX, rR, int32(16*from))
		a.MovImm(amd64.RDX, uint64(n))
		loop := a.NewLabel()
		a.Bind(loop)
		a.StoreImm(amd64.RCX, 0, 0)
		a.StoreImm(amd64.RCX, 8, 0)
		a.OpImm(amd64.Add, amd64.RCX, 16, true)
		a.OpImm(amd64.Sub, amd64.RDX, 1, true)
		a.Jcc(amd64.CondNE, loop)
	}
	a.Bind(done)
	wb := c.wbRoutine()
	c.later(func() {
		a.Bind(slow)
		a.Lea(amd64.RCX, rR, int32(16*from))
		a.MovImm(amd64.RDX, uint64(n))
		loop, next := a.NewLabel(), a.NewLabel()
		a.Bind(loop)
		a.StoreImm(amd64.RCX, 8, 0)
		a.Store(rCtx, offWBSlot, amd64.RCX)
		a.StoreImm(rCtx, offWBPtr, 0)
		a.LeaLabel(rTmp, next)
		a.Jmp(wb)
		a.Bind(next)
		a.OpImm(amd64.Add, amd64.RCX, 16, true)
		a.OpImm(amd64.Sub, amd64.RDX, 1, true)
		a.Jcc(amd64.CondNE, loop)
		a.Jmp(done)
	})
}

// refNull sets the flags by the pointer word of the reference at src,
// returning the condition under which it is null.
func (c *compiler) refNull(src mem) amd64.Cond {
	c.a.Load(rTmp, src.base, src.off)
	c.a.Op(amd64.Test, rTmp, rTmp, true)
	return amd64.CondE
}

// refEqual sets the flags by comparing the references at x and y, both
// words, returning the condition under which they are the same.
func (c *compiler) refEqual(x, y mem) amd64.Cond {
	a := c.a
	ne := a.NewLabel()
	a.Load(rTmp, x.base, x.off)
	a.CmpMem(rTmp, y.base, y.off, true)
	a.Jcc(amd64.CondNE, ne)
	a.Load(rTmp, x.base, x.off+8)
	a.CmpMem(rTmp, y.base, y.off+8, true)
	a.Bind(ne)
	return amd64.CondE
}

// tableEntry loads entry i of the context's table at off -- the globals',
// the functions' or the tables' addresses -- into r.
func (c *compiler) tableEntry(r amd64.Reg, off int32, i uint32) {
	c.a.Load(r, rCtx, off)
	c.a.Load(r, r, int32(8*i))
}

// elemAddr makes t, an object whose elements of two words are at off --
// their address, then their number -- the address of element r, going to
// trap past the last. r is lost.
func (c *compiler) elemAddr(t, r amd64.Reg, off int32, trap amd64.Label) {
	a := c.a
	a.CmpMem(r, t, off+8, true)
	a.Jcc(amd64.CondAE, trap)
	a.Load(t, t, off)
	a.ShiftImm(amd64.Shl, r, 4, true)
	a.Op(amd64.Add, t, r, true)
}

// testCond sets the flags by i32 v, popped, for condNonZero.
func (c *compiler) testCond(v value) {
	r := rTmp
	if v.k == kReg {
		r = v.gpr()
	} else {
		c.loadG(rTmp, v)
	}
	c.a.Op(amd64.Test, r, r, false)
	c.free(v)
}
