package jit

import (
	"github.com/go-quickjs/go-quickjs/internal/jit/asm/arm64"
	"github.com/go-quickjs/go-quickjs/wasm/internal/module"
)

// The front end's (compile.go's) names for arm64's assembler and
// registers.
type (
	asm      = arm64.Asm
	asmLabel = arm64.Label
	reg      = arm64.Reg
	freg     = arm64.FReg
	condCode = arm64.Cond
	regMask  = uint32
)

// CPU is the CPU's optional instructions this process's code may use:
// every A64 processor has CNT and the FRINT roundings.
var CPU = Features{Popcnt: true, Round: true}

// At a call every entry is in its slot and every local's home stored, so
// the cache's registers are free: the call sequences use these.
const (
	rCall0 = arm64.Reg(1)
	rCall1 = arm64.Reg(2)
)

// entries emits the return to the engine, at the start, then the engine's
// entry, which loads the registers the code keeps, then the natives'.
func (c *compiler) entries() (retStub int, goEntry, entry arm64.Label) {
	a := c.a
	retStub = a.Len()
	a.Store(rCtx, offPoll, rPoll)
	a.Store(rCtx, offRSP, rRS)
	c.storeCtx(offExit, ExitReturn)
	a.Ret()
	goEntry = a.NewLabel()
	entry = a.NewLabel()
	a.Bind(goEntry)
	c.loadKept()
	a.Bind(entry)
	return retStub, goEntry, entry
}

// loadKept loads the registers the code keeps from the context: L, memory
// 0's base and size, the poll counter and the return stack's next entry.
func (c *compiler) loadKept() {
	a := c.a
	a.Load(rL, rCtx, offL)
	a.Load(rMem, rCtx, offMemBase)
	a.Load(rMemSize, rCtx, offMemSize)
	a.Load(rPoll, rCtx, offPoll)
	a.Load(rRS, rCtx, offRSP)
}

func (c *compiler) zeroLocals() {
	a := c.a
	from, to := c.numParams, c.numLocals
	if from >= to {
		return
	}
	if to-from <= 16 {
		for k := from; k < to; k++ {
			a.Store(rL, int32(k*8), arm64.ZR)
		}
		return
	}
	a.AddImm(rTmp, rL, int64(from*8), true)
	a.MovImm(rTmp2, uint64(to-from))
	loop := a.NewLabel()
	a.Bind(loop)
	a.Store(rTmp, 0, arm64.ZR)
	a.AddImm(rTmp, rTmp, 8, true)
	a.SubsImm(rTmp2, rTmp2, 1, true)
	a.BCond(arm64.NE, loop)
}

// returnSequence binds the return label: it pops the return stack's entry
// and goes where it says, the caller's L restored.
func (c *compiler) returnSequence() {
	a := c.a
	a.Bind(c.retL)
	a.AddImm(rRS, rRS, -RSEntryWords*8, true)
	a.Load(rL, rRS, 8)
	a.Load(rTmp, rRS, 0)
	a.Br(rTmp)
}

func (c *compiler) jmp(l arm64.Label) { c.a.B(l) }

func (c *compiler) jcc(cond arm64.Cond, l arm64.Label) { c.a.BCond(cond, l) }

// pollBranch is a loop's back edge to head: the poll counter counted down,
// and poll taken when it runs out.
func (c *compiler) pollBranch(head, poll arm64.Label) {
	a := c.a
	a.SubsImm(rPoll, rPoll, 1, true)
	a.BCond(arm64.GT, head)
	a.B(poll)
}

// storeCtx stores v into the context's word at off.
func (c *compiler) storeCtx(off int32, v uint64) {
	if v == 0 {
		c.a.Store(rCtx, off, arm64.ZR)
		return
	}
	c.a.MovImm(rTmp, v)
	c.a.Store(rCtx, off, rTmp)
}

// exit returns to the engine for kind, with args, to resume at resume. The
// registers are lost: whatever lives past it is in its slot.
func (c *compiler) exit(kind uint64, resume arm64.Label, args ...uint64) {
	a := c.a
	c.saveHomes()
	a.Store(rCtx, offL, rL)
	a.Store(rCtx, offPoll, rPoll)
	a.Store(rCtx, offRSP, rRS)
	c.storeCtx(offExit, kind)
	for i, v := range args {
		c.storeCtx(offArg0+int32(8*i), v)
	}
	c.storeCtx(offExitFn, uint64(c.fn))
	a.Adr(rTmp, resume)
	a.Store(rCtx, offExitAddr, rTmp)
	a.Ret()
}

// resumeAt binds l, where the engine resumes the code, and reloads the
// registers an exit loses that the code keeps.
func (c *compiler) resumeAt(l arm64.Label) {
	c.a.Bind(l)
	c.loadKept()
	if c.refs {
		c.loadR()
	}
	c.reloadHomes()
}

// trapStub is the code's common stub for trap code: the exit, the trap's
// offset stored already by the site's.
func (c *compiler) trapStub(code uint64) {
	a := c.a
	a.Store(rCtx, offL, rL)
	a.Store(rCtx, offRSP, rRS)
	c.storeCtx(offExit, ExitTrap)
	c.storeCtx(offArg0, code)
	c.storeCtx(offExitFn, uint64(c.fn))
	a.Ret()
}

// trapSite is a trap site's stub: its offset stored, then the common stub.
func (c *compiler) trapSite(at uint32, to arm64.Label) {
	c.storeCtx(offArg0+8, uint64(at))
	c.a.B(to)
}

// condition pops an i32 condition, returning the condition under which it
// is true: the flags of a comparison made for it, or a test of its value.
// A constant's is the comparison of the zero register with itself, equal.
func (c *compiler) condition() arm64.Cond {
	v := c.pop()
	switch v.k {
	case kFlags:
		return arm64.Cond(v.c)
	case kConst:
		c.a.Cmp(arm64.ZR, arm64.ZR, false)
		if uint32(v.c) != 0 {
			return arm64.EQ
		}
		return arm64.NE
	}
	r := c.readG(v, rTmp)
	c.a.CmpImm(r, 0, false)
	c.free(v)
	return arm64.NE
}

// brTable compiles br_table: every entry stored, then a jump into a table
// of branches, each to its target's stub, which moves the values.
func (c *compiler) brTable() {
	a := c.a
	n := c.r.u32()
	depths := make([]int, n+1)
	for i := range depths {
		depths[i] = int(c.r.u32())
	}
	idx := c.intoG(c.pop(), 0)
	c.spillAll()
	arity := len(c.ctrl[len(c.ctrl)-1-depths[n]].labelTypes())
	src := len(c.stack) - arity
	// An index past the table takes its last entry, the default.
	a.MovImm(rTmp, uint64(n))
	a.Cmp(idx, rTmp, false)
	a.Csel(idx, idx, rTmp, arm64.LO, false)
	table := a.NewLabel()
	a.Adr(rTmp, table)
	a.AddLsl(rTmp, rTmp, idx, 2)
	a.Br(rTmp)
	c.freeG(idx)
	a.Bind(table)
	stubs := map[int]arm64.Label{}
	var order []int
	for _, d := range depths {
		l, ok := stubs[d]
		if !ok {
			l = a.NewLabel()
			stubs[d] = l
			order = append(order, d)
		}
		a.B(l)
	}
	for _, d := range order {
		a.Bind(stubs[d])
		t := c.ctrl[len(c.ctrl)-1-d]
		if d == len(c.ctrl)-1 {
			for i, rt := range c.sig.Results {
				c.copyPosResult(rt.Kind, src+i, i)
			}
			a.B(c.retL)
			continue
		}
		for i, lt := range t.labelTypes() {
			c.copyPos(lt.Kind, src+i, t.base+i)
		}
		c.jumpTo(t)
	}
	c.markDead()
}

// globalAddr loads global g's address into rTmp.
func (c *compiler) globalAddr(g uint32) {
	c.a.Load(rTmp, rCtx, offGlobals)
	c.a.Load(rTmp, rTmp, int32(8*g))
}

func (c *compiler) globalGet(g uint32) {
	t := c.m.Globals[g].Type.Kind
	c.globalAddr(g)
	if isFloat(t) {
		x := c.allocX(0)
		if t == module.KindF32 {
			c.a.LoadS(x, rTmp, 0)
		} else {
			c.a.LoadF(x, rTmp, 0)
		}
		c.pushReg(t, uint8(x))
		return
	}
	r := c.allocG(0)
	if t == module.KindI32 {
		c.a.LoadU32(r, rTmp, 0)
	} else {
		c.a.Load(r, rTmp, 0)
	}
	c.pushReg(t, uint8(r))
}

// globalSet stores the top of the stack into global g's word, all 64 bits
// of it, as store does a slot's.
func (c *compiler) globalSet(g uint32) {
	v := c.pop()
	c.globalAddr(g)
	c.store(c.inRegister(v), rTmp, 0)
	c.free(v)
}

// inRegister returns v, or v in rTmp2 if it is a slot's: a value store
// can store at an address other than a slot's without rTmp.
func (c *compiler) inRegister(v value) value {
	switch v.k {
	case kSlot:
		c.a.Load(rTmp2, rL, v.off)
		return value{t: module.KindI64, k: kReg, r: uint8(rTmp2)}
	case kConst:
		bits := v.c
		if v.t == module.KindI32 || v.t == module.KindF32 {
			bits = uint64(uint32(bits))
		}
		if bits == 0 {
			return value{t: module.KindI64, k: kReg, r: uint8(arm64.ZR)}
		}
		c.a.MovImm(rTmp2, bits)
		return value{t: module.KindI64, k: kReg, r: uint8(rTmp2)}
	}
	return v
}

// selectOp compiles select: the first operand if the condition is true.
func (c *compiler) selectOp() {
	cond := c.pop()
	b := c.pop()
	x := c.pop()
	a := c.a
	if isFloat(x.t) {
		xa := c.intoX(x)
		xb := c.readX(b)
		rc := c.readG(cond, rTmp)
		a.CmpImm(rc, 0, false)
		a.FCsel(xa, xa, xb, arm64.NE, x.t == module.KindF32)
		c.free(cond)
		c.free(b)
		c.pushReg(x.t, uint8(xa))
		return
	}
	wide := x.t == module.KindI64
	ra := c.intoG(x, 0)
	rb := c.readG(b, rTmp)
	rc := c.readG(cond, rTmp2)
	a.CmpImm(rc, 0, false)
	a.Csel(ra, ra, rb, arm64.NE, wide)
	c.free(b)
	c.free(cond)
	c.pushReg(x.t, uint8(ra))
}

// call compiles a call of function idx: natively where the callee has
// native code and there is room, else through the engine.
func (c *compiler) call(idx uint32, at uint32) {
	a := c.a
	ft := c.m.FuncType(idx)
	n := len(ft.Params)
	c.spillAll()
	base := len(c.stack) - n
	c.truncate(base)
	after := a.NewLabel()
	if c.m.Functions[idx].Imported {
		c.exit(ExitCall, after, uint64(idx), c.slotIndex(base), uint64(at))
		c.resumeAt(after)
		c.pushResults(ft.Results)
		return
	}
	start := a.NewLabel()
	slow := a.NewLabel()
	poll := a.NewLabel()
	c.saveHomes()
	a.Bind(start)
	a.SubsImm(rPoll, rPoll, 1, true)
	a.BCond(arm64.LE, poll)
	c.nativeCallChecks(idx, c.frameSlots, c.adv, false, slow)
	// The arguments, into the callee's parameters.
	frame := int32(8 * c.frameSlots)
	entry := rTmp2
	if c.refs {
		// A reference's store uses rTmp2.
		entry = rCall1
		a.MovRR(entry, rTmp2)
	}
	if hasRef(ft.Params) {
		c.argsTo(ft.Params, base, frame, c.adv)
	} else {
		for i := 0; i < n; i++ {
			a.Load(rTmp, rL, c.slotOff(base+i))
			a.Store(rL, frame+int32(8*i), rTmp)
		}
	}
	if c.refs {
		c.advanceR()
	}
	ret := a.NewLabel()
	a.Adr(rTmp, ret)
	a.Store(rRS, 0, rTmp)
	a.Store(rRS, 8, rL)
	a.MovImm(rTmp, Site(idx, at))
	a.Store(rRS, 16, rTmp)
	a.AddImm(rRS, rRS, RSEntryWords*8, true)
	a.AddImm(rL, rL, int64(frame), true)
	a.Br(entry)
	a.Bind(ret)
	if c.refs {
		c.restoreR()
	}
	c.reloadHomes()
	// The results, from the callee's frame.
	if hasRef(ft.Results) {
		c.resultsFrom(ft.Results, base, frame, c.adv)
	} else {
		for i := range ft.Results {
			a.Load(rTmp, rL, frame+int32(8*i))
			a.Store(rL, c.slotOff(base+i), rTmp)
		}
	}
	a.Bind(after)
	slotIdx := c.slotIndex(base)
	c.ool = append(c.ool, func() {
		a.Bind(poll)
		r1 := a.NewLabel()
		c.exit(ExitPoll, r1)
		c.resumeAt(r1)
		a.B(start)
		a.Bind(slow)
		r2 := a.NewLabel()
		c.exit(ExitCall, r2, uint64(idx), slotIdx, uint64(at))
		c.resumeAt(r2)
		a.B(after)
	})
	c.pushResults(ft.Results)
}

// nativeCallChecks leaves the callee's entry in rTmp2, or goes to slow:
// the callee has no native code, its frame no room in the chunk past
// frameSlots of the caller's -- nor, for one that holds references, room
// for its reference slots past adv bytes of R -- or -- for a call, not a
// tail call -- the return stack none for an entry, which it also has not
// where the call would pass the depth limit, which the call through Go
// then traps for.
func (c *compiler) nativeCallChecks(idx uint32, frameSlots int, adv int32, tail bool, slow arm64.Label) {
	a := c.a
	a.Load(rTmp2, rCtx, offLinks)
	a.Load(rTmp2, rTmp2, int32(8*idx))
	a.Cbz(rTmp2, slow, true)
	a.AddImm(rCall0, rL, int64(8*(frameSlots+c.req.FrameSlotsOf(idx))), true)
	a.Load(rCall1, rCtx, offChunkEnd)
	a.Cmp(rCall0, rCall1, true)
	a.BCond(arm64.HI, slow)
	if need := int32(16 * c.req.RefSlotsOf(idx)); need > 0 {
		if c.refs {
			a.AddImm(rCall0, rR, int64(adv+need), true)
		} else {
			a.Load(rCall0, rCtx, offR)
			a.AddImm(rCall0, rCall0, int64(adv+need), true)
		}
		a.Load(rCall1, rCtx, offRChunkEnd)
		a.Cmp(rCall0, rCall1, true)
		a.BCond(arm64.HI, slow)
	}
	if tail {
		return
	}
	a.Load(rCall1, rCtx, offRSEnd)
	a.Cmp(rRS, rCall1, true)
	a.BCond(arm64.HS, slow)
}

// returnCall compiles return_call of function idx: in place of the
// caller's frame, natively where it can be.
func (c *compiler) returnCall(idx uint32, at uint32) {
	a := c.a
	ft := c.m.FuncType(idx)
	n := len(ft.Params)
	c.spillAll()
	base := len(c.stack) - n
	c.truncate(base)
	retResume := a.NewLabel()
	slotIdx := c.slotIndex(base)
	if c.m.Functions[idx].Imported {
		c.exit(ExitTailCall, retResume, uint64(idx), slotIdx, uint64(at))
		c.resumeAt(retResume)
		a.B(c.retL)
		return
	}
	start := a.NewLabel()
	slow := a.NewLabel()
	poll := a.NewLabel()
	a.Bind(start)
	a.SubsImm(rPoll, rPoll, 1, true)
	a.BCond(arm64.LE, poll)
	c.nativeCallChecks(idx, 0, 0, true, slow)
	// The arguments, into the parameters: each slot read is past each
	// written before it.
	entry := rTmp2
	if hasRef(ft.Params) {
		// A reference's store uses rTmp2.
		entry = rCall1
		a.MovRR(entry, rTmp2)
		c.argsTo(ft.Params, base, 0, 0)
	} else {
		for i := 0; i < n; i++ {
			a.Load(rTmp, rL, c.slotOff(base+i))
			a.Store(rL, int32(8*i), rTmp)
		}
	}
	// The frame's return stack entry names the callee now: the upper half
	// of its third word.
	a.MovImm(rTmp, uint64(idx))
	a.Store32(rRS, -4, rTmp)
	a.Br(entry)
	c.ool = append(c.ool, func() {
		a.Bind(poll)
		r1 := a.NewLabel()
		c.exit(ExitPoll, r1)
		c.resumeAt(r1)
		a.B(start)
		a.Bind(slow)
		c.exit(ExitTailCall, retResume, uint64(idx), slotIdx, uint64(at))
		c.resumeAt(retResume)
		a.B(c.retL)
	})
}
