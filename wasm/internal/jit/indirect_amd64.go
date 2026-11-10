package jit

import (
	"math"

	"github.com/go-quickjs/go-quickjs/internal/jit/asm/amd64"
)

// The calls through a table or a reference on amd64 (indirect.go). Every
// entry is in its slot, so the cache's registers are free: the checks
// leave the callee's index in DX and its native entry in AX, and use CX.

// indirectCallee leaves in DX the index of the function call d calls and
// in AX its native entry, or goes to slow: the table's element past its
// end or null, a function of another type or instance, without native
// code, or whose frame has no room past frame bytes of L and, for its
// reference slots, adv bytes of R. A reference is not null here. A callee
// that holds references fails the numeric frame's check (Ctx.Frames), and
// is checked again out of line by both its sizes (Ctx.RFrames).
func (c *compiler) indirectCallee(d *indirect, frame, adv int32, slow amd64.Label) {
	a := c.a
	const ax, cx, dx = amd64.RAX, amd64.RCX, amd64.RDX
	lay := c.req.Layout
	if d.ref {
		a.Load(ax, rR, c.refSlotOff(d.calleeAt()))
	} else {
		a.Load(cx, rL, c.slotOff(d.calleeAt()))
		c.tableEntry(ax, offTables, d.table)
		a.CmpMem(cx, ax, lay.TableElems+8, true)
		a.Jcc(amd64.CondAE, slow)
		a.Load(ax, ax, lay.TableElems)
		a.ShiftImm(amd64.Shl, cx, 4, true)
		a.LoadIdx(ax, ax, cx, 1, 0)
		a.Op(amd64.Test, ax, ax, true)
		a.Jcc(amd64.CondE, slow)
	}
	a.LoadU32(cx, ax, lay.FuncType)
	a.OpImm(amd64.Cmp, cx, int32(c.m.Canonical[d.typeIndex]), false)
	a.Jcc(amd64.CondNE, slow)
	a.Load(cx, ax, lay.FuncInst)
	a.CmpMem(cx, rCtx, offInst, true)
	a.Jcc(amd64.CondNE, slow)
	a.LoadU32(dx, ax, lay.FuncIndex)
	a.Load(ax, rCtx, offLinks)
	a.LoadIdxScaled(ax, ax, dx, 3, 0)
	a.Op(amd64.Test, ax, ax, true)
	a.Jcc(amd64.CondE, slow)
	refs, back := a.NewLabel(), a.NewLabel()
	a.Load(cx, rCtx, offFrames)
	a.LoadIdxScaled(cx, cx, dx, 3, 0)
	a.Op(amd64.Add, cx, rL, true)
	if frame != 0 {
		a.OpImm(amd64.Add, cx, frame, true)
	}
	a.CmpMem(cx, rCtx, offChunkEnd, true)
	a.Jcc(amd64.CondA, refs)
	a.Bind(back)
	c.later(func() {
		a.Bind(refs)
		a.Load(cx, rCtx, offRFrames)
		a.LoadIdxScaled(cx, cx, dx, 3, 0)
		a.MovRR(rTmp, cx)
		a.ShiftImm(amd64.Shr, rTmp, 32, true)
		a.MovRR32(cx, cx)
		a.Op(amd64.Add, cx, rL, true)
		if frame != 0 {
			a.OpImm(amd64.Add, cx, frame, true)
		}
		a.CmpMem(cx, rCtx, offChunkEnd, true)
		a.Jcc(amd64.CondA, slow)
		if c.refs {
			a.Op(amd64.Add, rTmp, rR, true)
			if adv != 0 {
				a.OpImm(amd64.Add, rTmp, adv, true)
			}
		} else {
			a.AddMem(rTmp, rCtx, offR)
		}
		a.CmpMem(rTmp, rCtx, offRChunkEnd, true)
		a.Jcc(amd64.CondA, slow)
		a.Jmp(back)
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
		a.Load(amd64.RCX, rL, c.slotOff(d.base+i))
		a.Store(rL, frame+int32(8*i), amd64.RCX)
	}
}

// indirectNative makes call d natively: the poll counted down, the callee
// checked, the arguments into its frame past this one's, a return stack
// entry naming it, and the jump; then the results from its frame.
func (c *compiler) indirectNative(d *indirect, poll, slow amd64.Label) {
	a := c.a
	const ax, cx, dx = amd64.RAX, amd64.RCX, amd64.RDX
	a.OpImm(amd64.Sub, rPoll, 1, true)
	a.Jcc(amd64.CondLE, poll)
	frame := int32(8 * c.frameSlots)
	c.indirectCallee(d, frame, c.adv, slow)
	a.CmpMem(rRS, rCtx, offRSEnd, true)
	a.Jcc(amd64.CondAE, slow)
	c.argsInto(d, frame, c.adv)
	if c.refs {
		c.advanceR()
	}
	ret := a.NewLabel()
	a.LeaLabel(cx, ret)
	a.Store(rRS, 0, cx)
	a.Store(rRS, 8, rL)
	c.siteOf(cx, dx, d.at)
	a.Store(rRS, 16, cx)
	a.OpImm(amd64.Add, rRS, RSEntryWords*8, true)
	a.OpImm(amd64.Add, rL, frame, true)
	a.JmpReg(ax)
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
		a.Load(cx, rL, frame+int32(8*i))
		a.Store(rL, c.slotOff(d.base+i), cx)
	}
}

// siteOf makes r the return stack entry's third word (Site) for the
// callee whose index is in idx and the call at offset at.
func (c *compiler) siteOf(r, idx reg, at uint32) {
	a := c.a
	a.MovRR(r, idx)
	a.ShiftImm(amd64.Shl, r, 32, true)
	if at == 0 {
		return
	}
	if at <= math.MaxInt32 {
		a.OpImm(amd64.Or, r, int32(at), true)
		return
	}
	a.MovImm(rTmp, uint64(at))
	a.Op(amd64.Or, r, rTmp, true)
}

// indirectTailNative makes tail call d natively in place of the frame: the
// poll counted down, the callee checked, the arguments into the frame's
// parameters, the frame's return stack entry renamed, and the jump.
func (c *compiler) indirectTailNative(d *indirect, poll, slow amd64.Label) {
	a := c.a
	const ax, cx, dx = amd64.RAX, amd64.RCX, amd64.RDX
	a.OpImm(amd64.Sub, rPoll, 1, true)
	a.Jcc(amd64.CondLE, poll)
	c.indirectCallee(d, 0, 0, slow)
	c.argsInto(d, 0, 0)
	a.Load(cx, rRS, -8)
	a.MovRR32(cx, cx)
	a.MovRR(rTmp, dx)
	a.ShiftImm(amd64.Shl, rTmp, 32, true)
	a.Op(amd64.Or, cx, rTmp, true)
	a.Store(rRS, -8, cx)
	a.JmpReg(ax)
}
