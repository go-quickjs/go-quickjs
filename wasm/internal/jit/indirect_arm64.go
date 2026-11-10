package jit

import "github.com/go-quickjs/go-quickjs/internal/jit/asm/arm64"

// The calls through a table or a reference on arm64 (indirect.go). Every
// entry is in its slot, so the cache's registers are free: the checks
// leave the callee's index in X4 and its native entry in X3, and use X1,
// X2 and X16.

const (
	rCallee = arm64.Reg(4)
	rEntry  = arm64.Reg(3)
)

// indirectCallee leaves in X4 the index of the function call d calls and
// in X3 its native entry, or goes to slow: the table's element past its
// end or null, a function of another type or instance, without native
// code, or whose frame has no room past frame bytes of L and, for its
// reference slots, adv bytes of R. A reference is not null here. A callee
// that holds references fails the numeric frame's check (Ctx.Frames), and
// is checked again out of line by both its sizes (Ctx.RFrames).
func (c *compiler) indirectCallee(d *indirect, frame, adv int32, slow arm64.Label) {
	a := c.a
	lay := c.req.Layout
	if d.ref {
		a.Load(rCall0, rR, c.refSlotOff(d.calleeAt()))
	} else {
		a.Load(rCall1, rL, c.slotOff(d.calleeAt()))
		c.tableEntry(rCall0, offTables, d.table)
		a.Load(rTmp, rCall0, lay.TableElems+8)
		a.Cmp(rCall1, rTmp, true)
		a.BCond(arm64.HS, slow)
		a.Load(rCall0, rCall0, lay.TableElems)
		a.AddLsl(rCall0, rCall0, rCall1, 4)
		a.Load(rCall0, rCall0, 0)
		a.Cbz(rCall0, slow, true)
	}
	a.LoadU32(rCall1, rCall0, lay.FuncType)
	a.CmpImm(rCall1, int64(c.m.Canonical[d.typeIndex]), false)
	a.BCond(arm64.NE, slow)
	a.Load(rCall1, rCall0, lay.FuncInst)
	a.Load(rTmp, rCtx, offInst)
	a.Cmp(rCall1, rTmp, true)
	a.BCond(arm64.NE, slow)
	a.LoadU32(rCallee, rCall0, lay.FuncIndex)
	a.Load(rEntry, rCtx, offLinks)
	a.AddLsl(rEntry, rEntry, rCallee, 3)
	a.Load(rEntry, rEntry, 0)
	a.Cbz(rEntry, slow, true)
	refs, back := a.NewLabel(), a.NewLabel()
	a.Load(rCall0, rCtx, offFrames)
	a.AddLsl(rCall0, rCall0, rCallee, 3)
	a.Load(rCall0, rCall0, 0)
	a.Op(arm64.Add, rCall0, rCall0, rL, true)
	if frame != 0 {
		a.AddImm(rCall0, rCall0, int64(frame), true)
	}
	a.Load(rTmp, rCtx, offChunkEnd)
	a.Cmp(rCall0, rTmp, true)
	a.BCond(arm64.HI, refs)
	a.Bind(back)
	c.later(func() {
		a.Bind(refs)
		a.Load(rCall1, rCtx, offRFrames)
		a.AddLsl(rCall1, rCall1, rCallee, 3)
		a.Load(rCall1, rCall1, 0)
		a.MovRR32(rCall0, rCall1)
		a.ShiftImm(arm64.Lsr, rCall1, rCall1, 32, true)
		a.Op(arm64.Add, rCall0, rCall0, rL, true)
		if frame != 0 {
			a.AddImm(rCall0, rCall0, int64(frame), true)
		}
		a.Load(rTmp, rCtx, offChunkEnd)
		a.Cmp(rCall0, rTmp, true)
		a.BCond(arm64.HI, slow)
		if c.refs {
			a.Op(arm64.Add, rCall1, rCall1, rR, true)
			if adv != 0 {
				a.AddImm(rCall1, rCall1, int64(adv), true)
			}
		} else {
			a.Load(rTmp, rCtx, offR)
			a.Op(arm64.Add, rCall1, rCall1, rTmp, true)
		}
		a.Load(rTmp, rCtx, offRChunkEnd)
		a.Cmp(rCall1, rTmp, true)
		a.BCond(arm64.HI, slow)
		a.B(back)
	})
}

// argsInto copies call d's arguments from their stack positions into the
// parameters of a frame frame bytes past L and adv bytes past R: each slot
// read is past each written before it where the frame is this one's.
func (c *compiler) argsInto(d *indirect, frame, adv int32) {
	a := c.a
	if hasRef(d.ft.Params) || hasV128(d.ft.Params) {
		c.argsTo(d.ft.Params, d.base, frame, adv)
		return
	}
	for i := range d.ft.Params {
		a.Load(rTmp, rL, c.slotOff(d.base+i))
		a.Store(rL, frame+int32(8*i), rTmp)
	}
}

// indirectNative makes call d natively: the poll counted down, the callee
// checked, the arguments into its frame past this one's, a return stack
// entry naming it, and the jump; then the results from its frame.
func (c *compiler) indirectNative(d *indirect, poll, slow arm64.Label) {
	a := c.a
	a.SubsImm(rPoll, rPoll, 1, true)
	a.BCond(arm64.LE, poll)
	frame := int32(8 * c.frameSlots)
	c.indirectCallee(d, frame, c.adv, slow)
	a.Load(rTmp, rCtx, offRSEnd)
	a.Cmp(rRS, rTmp, true)
	a.BCond(arm64.HS, slow)
	c.argsInto(d, frame, c.adv)
	if c.refs {
		c.advanceR()
	}
	ret := a.NewLabel()
	a.Adr(rTmp, ret)
	a.Store(rRS, 0, rTmp)
	a.Store(rRS, 8, rL)
	a.ShiftImm(arm64.Lsl, rTmp, rCallee, 32, true)
	if d.at != 0 {
		a.MovImm(rTmp2, uint64(d.at))
		a.Op(arm64.Orr, rTmp, rTmp, rTmp2, true)
	}
	a.Store(rRS, 16, rTmp)
	a.AddImm(rRS, rRS, RSEntryWords*8, true)
	a.AddImm(rL, rL, int64(frame), true)
	a.Br(rEntry)
	a.Bind(ret)
	if c.refs {
		c.restoreR()
	}
	c.reloadHomes()
	if hasRef(d.ft.Results) || hasV128(d.ft.Results) {
		c.resultsFrom(d.ft.Results, d.base, frame, c.adv)
		return
	}
	for i := range d.ft.Results {
		a.Load(rTmp, rL, frame+int32(8*i))
		a.Store(rL, c.slotOff(d.base+i), rTmp)
	}
}

// indirectTailNative makes tail call d natively in place of the frame: the
// poll counted down, the callee checked, the arguments into the frame's
// parameters, the frame's return stack entry renamed -- the upper half of
// its third word -- and the jump.
func (c *compiler) indirectTailNative(d *indirect, poll, slow arm64.Label) {
	a := c.a
	a.SubsImm(rPoll, rPoll, 1, true)
	a.BCond(arm64.LE, poll)
	c.indirectCallee(d, 0, 0, slow)
	c.argsInto(d, 0, 0)
	a.Store32(rRS, -4, rCallee)
	a.Br(rEntry)
}
