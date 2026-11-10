package jit

import "github.com/go-quickjs/go-quickjs/internal/jit/asm/arm64"

// rR holds R, the frame's reference slots, in a function that holds
// references; the cache never hands it out there.
const rR = arm64.Reg(12)

// refHomeGPRs is the number of integer locals with registers of their own
// in a function that holds references: all of homeGPRs, the cache having
// registers to spare.
const refHomeGPRs = len(homeGPRs)

// condNonZero is the condition testCond's value is not zero under.
const condNonZero = arm64.NE

// wbSaved are the general registers the barrier routine keeps across the
// runtime's store, which keeps only X0 and the link register: every
// register the code keeps a value in. D0-D29 are kept too.
var wbSaved = [...]arm64.Reg{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15,
	19, 20, 21, 22, 23, 24, 25, 26}

// loadR loads R from the context.
func (c *compiler) loadR() { c.a.Load(rR, rCtx, offR) }

// advanceR moves the context's R past the frame, for a native call.
func (c *compiler) advanceR() {
	c.a.AddImm(rTmp, rR, int64(c.adv), true)
	c.a.Store(rCtx, offR, rTmp)
}

// restoreR moves the context's R back to the frame after a native call,
// and loads it.
func (c *compiler) restoreR() {
	a := c.a
	a.Load(rR, rCtx, offR)
	a.AddImm(rR, rR, -int64(c.adv), true)
	a.Store(rCtx, offR, rR)
}

// testBarrier goes to slow while the collector marks.
func (c *compiler) testBarrier(slow arm64.Label) {
	c.a.MovImm(rTmp, c.req.WriteBarrier)
	c.a.LoadU8(rTmp, rTmp, 0)
	c.a.Cbnz(rTmp, slow, false)
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

// wordIn returns a register holding w, loading it into tmp: the zero
// register for 0.
func (c *compiler) wordIn(w word, tmp arm64.Reg) arm64.Reg {
	switch w.k {
	case wReg:
		return w.r
	case wMem:
		c.a.Load(tmp, w.m.base, w.m.off)
		return tmp
	}
	if w.c == 0 {
		return arm64.ZR
	}
	c.a.MovImm(tmp, w.c)
	return tmp
}

// refPut stores the reference of pointer word p and payload word i at dst:
// the payload plainly, the pointer through the barrier routine while the
// collector marks. Neither dst's base nor a word's register is rTmp or
// rTmp2.
func (c *compiler) refPut(dst mem, p, i word) {
	a := c.a
	a.Store(dst.base, dst.off+8, c.wordIn(i, rTmp))
	slow, resume := a.NewLabel(), a.NewLabel()
	c.testBarrier(slow)
	a.Store(dst.base, dst.off, c.wordIn(p, rTmp))
	a.Bind(resume)
	wb := c.wbRoutine()
	c.later(func() {
		a.Bind(slow)
		a.Store(rCtx, offWBPtr, c.wordIn(p, rTmp))
		a.AddImm(rTmp, dst.base, int64(dst.off), true)
		a.Store(rCtx, offWBSlot, rTmp)
		a.Adr(rTmp, resume)
		a.B(wb)
	})
}

// emitWBRoutine emits the barrier routine at l: the slot's address in the
// context's WBSlot, the pointer in WBPtr, where to go on in rTmp. A nil
// stored over a nil is nothing to store or to shade; anything else is
// stored by the runtime's barrier, every register the code keeps saved
// around it.
func (c *compiler) emitWBRoutine(l arm64.Label) {
	a := c.a
	a.Bind(l)
	a.Store(rCtx, offWBRet, rTmp)
	store := a.NewLabel()
	a.Load(rTmp, rCtx, offWBSlot)
	a.Load(rTmp, rTmp, 0)
	a.Cbnz(rTmp, store, true)
	a.Load(rTmp, rCtx, offWBPtr)
	a.Cbnz(rTmp, store, true)
	a.Load(rTmp, rCtx, offWBRet)
	a.Br(rTmp)
	a.Bind(store)
	for i, r := range wbSaved {
		a.Store(rCtx, offSave+int32(8*i), r)
	}
	// The V registers whole: a v128 may be in one.
	for x := arm64.FReg(0); x < 30; x++ {
		a.StoreQ(rCtx, offSaveV+16*int32(x), x)
	}
	a.Load(arm64.Reg(15), rCtx, offWBSlot)
	a.Load(rTmp2, rCtx, offWBPtr)
	back := a.NewLabel()
	a.Adr(rTmp, back)
	a.MovImm(arm64.Reg(1), c.req.StoreRef)
	a.Br(arm64.Reg(1))
	a.Bind(back)
	for i, r := range wbSaved {
		a.Load(r, rCtx, offSave+int32(8*i))
	}
	for x := arm64.FReg(0); x < 30; x++ {
		a.LoadQ(x, rCtx, offSaveV+16*int32(x))
	}
	a.Load(rTmp, rCtx, offWBRet)
	a.Br(rTmp)
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
			a.Store(rR, int32(16*k), arm64.ZR)
			a.Store(rR, int32(16*k+8), arm64.ZR)
		}
	} else {
		a.AddImm(rCall0, rR, int64(16*from), true)
		a.MovImm(rCall1, uint64(n))
		loop := a.NewLabel()
		a.Bind(loop)
		a.Store(rCall0, 0, arm64.ZR)
		a.Store(rCall0, 8, arm64.ZR)
		a.AddImm(rCall0, rCall0, 16, true)
		a.SubsImm(rCall1, rCall1, 1, true)
		a.BCond(arm64.NE, loop)
	}
	a.Bind(done)
	wb := c.wbRoutine()
	c.later(func() {
		a.Bind(slow)
		a.AddImm(rCall0, rR, int64(16*from), true)
		a.MovImm(rCall1, uint64(n))
		loop, next := a.NewLabel(), a.NewLabel()
		a.Bind(loop)
		a.Store(rCall0, 8, arm64.ZR)
		a.Store(rCtx, offWBSlot, rCall0)
		a.Store(rCtx, offWBPtr, arm64.ZR)
		a.Adr(rTmp, next)
		a.B(wb)
		a.Bind(next)
		a.AddImm(rCall0, rCall0, 16, true)
		a.SubsImm(rCall1, rCall1, 1, true)
		a.BCond(arm64.NE, loop)
		a.B(done)
	})
}

// refNull sets the flags by the pointer word of the reference at src,
// returning the condition under which it is null.
func (c *compiler) refNull(src mem) arm64.Cond {
	c.a.Load(rTmp, src.base, src.off)
	c.a.CmpImm(rTmp, 0, true)
	return arm64.EQ
}

// refEqual sets the flags by comparing the references at x and y, both
// words, returning the condition under which they are the same.
func (c *compiler) refEqual(x, y mem) arm64.Cond {
	a := c.a
	ne := a.NewLabel()
	a.Load(rTmp, x.base, x.off)
	a.Load(rTmp2, y.base, y.off)
	a.Cmp(rTmp, rTmp2, true)
	a.BCond(arm64.NE, ne)
	a.Load(rTmp, x.base, x.off+8)
	a.Load(rTmp2, y.base, y.off+8)
	a.Cmp(rTmp, rTmp2, true)
	a.Bind(ne)
	return arm64.EQ
}

// tableEntry loads entry i of the context's table at off -- the globals',
// the functions' or the tables' addresses -- into r.
func (c *compiler) tableEntry(r arm64.Reg, off int32, i uint32) {
	c.a.Load(r, rCtx, off)
	c.a.Load(r, r, int32(8*i))
}

// elemAddr makes t, an object whose elements of two words are at off --
// their address, then their number -- the address of element r, going to
// trap past the last. r is lost.
func (c *compiler) elemAddr(t, r arm64.Reg, off int32, trap arm64.Label) {
	a := c.a
	a.Load(rTmp, t, off+8)
	a.Cmp(r, rTmp, true)
	a.BCond(arm64.HS, trap)
	a.Load(t, t, off)
	a.AddLsl(t, t, r, 4)
}

// testCond sets the flags by i32 v, popped, for condNonZero.
func (c *compiler) testCond(v value) {
	c.a.CmpImm(c.readG(v, rTmp), 0, false)
	c.free(v)
}
