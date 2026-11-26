package jit

import (
	"golang.org/x/sys/cpu"

	"github.com/go-quickjs/go-quickjs/internal/jit/asm/amd64"
	"github.com/go-quickjs/go-quickjs/wasm/internal/module"
)

// The front end's (compile.go's) names for amd64's assembler and registers.
type (
	asm      = amd64.Asm
	asmLabel = amd64.Label
	reg      = amd64.Reg
	freg     = amd64.XReg
	condCode = amd64.Cond
	regMask  = uint16
)

// CPU is the CPU's optional instructions this process's code may use, and
// the relaxed SIMD results the interpreter gives on it, by the conditions
// engine/relaxed_x86.go chooses them by.
var CPU = Features{
	Popcnt:           cpu.X86.HasPOPCNT,
	Round:            cpu.X86.HasSSE41,
	SIMD:             cpu.X86.HasSSSE3 && cpu.X86.HasSSE41 && cpu.X86.HasSSE42,
	RelaxedFused:     cpu.X86.HasFMA && cpu.X86.HasAVX,
	RelaxedDotSigned: cpu.X86.HasAVXVNNIInt8,
	RelaxedDotVNNI:   cpu.X86.HasAVXVNNI,
}

// entries emits the return to the engine, at the start, then the engine's
// entry, which loads the registers the code keeps, then the natives'.
func (c *compiler) entries() (retStub int, goEntry, entry amd64.Label) {
	a := c.a
	retStub = a.Len()
	a.Store(rCtx, offPoll, rPoll)
	a.Store(rCtx, offRSP, rRS)
	a.StoreImm(rCtx, offExit, ExitReturn)
	a.Ret()
	goEntry = a.NewLabel()
	entry = a.NewLabel()
	a.Bind(goEntry)
	a.Load(rL, rCtx, offL)
	a.Load(rMem, rCtx, offMemBase)
	a.Load(rPoll, rCtx, offPoll)
	a.Load(rRS, rCtx, offRSP)
	a.Bind(entry)
	if c.req.TierCounter != 0 {
		c.tierCount()
	}
	return retStub, goEntry, entry
}

// tierCount counts a call toward the optimizing tier, and exits for it
// when the count runs out. The exit is at the entry, before the prologue:
// no local has a home loaded yet, and the engine may enter the optimized
// code in the baseline's place.
func (c *compiler) tierCount() {
	a := c.a
	tier, back := a.NewLabel(), a.NewLabel()
	a.MovImm(rTmp, c.req.TierCounter)
	a.DecMem(rTmp, 0)
	a.Jcc(amd64.CondS, tier)
	a.Bind(back)
	c.later(func() {
		a.Bind(tier)
		a.Store(rCtx, offL, rL)
		a.Store(rCtx, offPoll, rPoll)
		a.Store(rCtx, offRSP, rRS)
		c.storeCtx(offExit, ExitTierUp)
		c.storeCtx(offExitFn, uint64(c.fn))
		resume := a.NewLabel()
		a.LeaLabel(rTmp, resume)
		a.Store(rCtx, offExitAddr, rTmp)
		a.Ret()
		a.Bind(resume)
		a.Load(rL, rCtx, offL)
		a.Load(rMem, rCtx, offMemBase)
		a.Load(rPoll, rCtx, offPoll)
		a.Load(rRS, rCtx, offRSP)
		a.Jmp(back)
	})
}

func (c *compiler) zeroLocals() {
	a := c.a
	from, to := c.numParams, c.numLocals
	if from >= to {
		return
	}
	a.Op(amd64.Xor, amd64.RAX, amd64.RAX, false)
	if to-from <= 16 {
		for k := from; k < to; k++ {
			a.Store(rL, int32(k*8), amd64.RAX)
		}
		return
	}
	a.Lea(amd64.RCX, rL, int32(from*8))
	a.MovImm(amd64.RDX, uint64(to-from))
	loop := a.NewLabel()
	a.Bind(loop)
	a.Store(amd64.RCX, 0, amd64.RAX)
	a.OpImm(amd64.Add, amd64.RCX, 8, true)
	a.OpImm(amd64.Sub, amd64.RDX, 1, true)
	a.Jcc(amd64.CondNE, loop)
}

// returnSequence binds the return label: it pops the return stack's entry
// and goes where it says, with L still this frame's, which the caller's
// return site takes back by its own frame's size.
func (c *compiler) returnSequence() {
	a := c.a
	a.Bind(c.retL)
	a.OpImm(amd64.Sub, rRS, RSEntryWords*8, true)
	a.JmpMem(rRS, 0)
}

func (c *compiler) jmp(l amd64.Label) { c.a.Jmp(l) }

func (c *compiler) jumpReg(r reg) { c.a.JmpReg(r) }

// sharedExit exits for kind through the function's exit its sites share
// (sharedStub), to go on at resume; the exit's arguments are stored.
func (c *compiler) sharedExit(kind uint64, resume amd64.Label) {
	c.a.LeaLabel(rTmp, resume)
	c.a.Jmp(c.sharedStub(kind))
}

func (c *compiler) jcc(cond amd64.Cond, l amd64.Label) { c.a.Jcc(cond, l) }

// pollBranch is a loop's back edge to head: the poll counter counted down,
// and poll taken when it runs out.
func (c *compiler) pollBranch(head, poll amd64.Label) {
	a := c.a
	a.OpImm(amd64.Sub, rPoll, 1, true)
	a.Jcc(amd64.CondG, head)
	a.Jmp(poll)
}

// storeCtx stores v into the context's word at off.
func (c *compiler) storeCtx(off int32, v uint64) {
	if v <= 0x7fffffff {
		c.a.StoreImm(rCtx, off, int32(v))
		return
	}
	c.a.MovImm(rTmp, v)
	c.a.Store(rCtx, off, rTmp)
}

// exit returns to the engine for kind, with args, to resume at resume. The
// registers are lost: whatever lives past it is in its slot.
func (c *compiler) exit(kind uint64, resume amd64.Label, args ...uint64) {
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
	a.LeaLabel(rTmp, resume)
	a.Store(rCtx, offExitAddr, rTmp)
	a.Ret()
}

// exitAlloc is exit's ExitAlloc by way of the allocation stub, where the
// request has one (exitStub).
func (c *compiler) exitAlloc(resume amd64.Label, args ...uint64) {
	c.exitStub(ExitAlloc, resume, args...)
}

// exitStub is exit for kind, an ExitAlloc or the ExitOp of an instruction
// that allocates (StubOp), by way of the allocation stub, where the request
// has one (Request.AllocStub): the exit's record stored, the code jumps to
// the stub, which keeps only the context, and goes on at resume, which
// reloads what the code keeps, where the engine made the object, else
// takes the exit.
func (c *compiler) exitStub(kind uint64, resume amd64.Label, args ...uint64) {
	stub := c.req.AllocStub
	if stub == 0 {
		c.exit(kind, resume, args...)
		return
	}
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
	a.LeaLabel(rTmp, resume)
	a.Store(rCtx, offExitAddr, rTmp)
	back := a.NewLabel()
	a.LeaLabel(amd64.R11, back)
	a.MovImm(amd64.RAX, stub)
	a.JmpReg(amd64.RAX)
	a.Bind(back)
	a.Op(amd64.Test, amd64.RAX, amd64.RAX, true)
	a.Jcc(amd64.CondNE, resume)
	a.Ret()
}

// resumeAt binds l, where the engine resumes the code, and reloads the
// registers an exit loses that the code keeps.
func (c *compiler) resumeAt(l amd64.Label) {
	c.a.Bind(l)
	c.a.Load(rL, rCtx, offL)
	c.a.Load(rMem, rCtx, offMemBase)
	c.a.Load(rPoll, rCtx, offPoll)
	c.a.Load(rRS, rCtx, offRSP)
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
func (c *compiler) trapSite(at uint32, to amd64.Label) {
	c.storeCtx(offArg0+8, uint64(at))
	c.a.Jmp(to)
}

// condition pops an i32 condition, returning the condition under which it
// is true: the flags of a comparison made for it, or a test of its value.
func (c *compiler) condition() amd64.Cond {
	v := c.pop()
	switch v.k {
	case kFlags:
		return amd64.Cond(v.c)
	case kConst:
		r := c.allocG(0)
		c.a.MovImm(r, uint64(uint32(v.c)))
		c.a.Op(amd64.Test, r, r, false)
		c.freeG(r)
		return amd64.CondNE
	}
	r := c.intoG(v, 0)
	c.a.Op(amd64.Test, r, r, false)
	c.freeG(r)
	return amd64.CondNE
}

// brTable compiles br_table: every entry stored, then a jump through a
// table of jumps, each to its target's stub, which moves the values.
func (c *compiler) brTable() {
	a := c.a
	n := c.r.u32()
	depths := grow(c.depths, int(n)+1)
	c.depths = depths
	for i := range depths {
		depths[i] = int(c.r.u32())
	}
	idx := c.intoG(c.pop(), 0)
	c.spillAll()
	arity := len(c.ctrl[len(c.ctrl)-1-depths[n]].labelTypes())
	src := len(c.stack) - arity
	a.MovImm(rTmp, uint64(n))
	a.Op(amd64.Cmp, idx, rTmp, false)
	a.Cmov(amd64.CondAE, idx, rTmp, false)
	table := a.NewLabel()
	a.LeaLabel(rTmp, table)
	a.LeaIdx(rTmp, rTmp, idx, 8, 0)
	a.JmpReg(rTmp)
	c.freeG(idx)
	a.Bind(table)
	stubs := grow(c.stubs, len(c.ctrl))
	order := c.stubOrder[:0]
	for _, d := range depths {
		if stubs[d] == 0 {
			stubs[d] = a.NewLabel() + 1
			order = append(order, d)
		}
		l := stubs[d] - 1
		a.JmpLong(l)
		a.Nop(3)
	}
	c.stubs, c.stubOrder = stubs, order
	for _, d := range order {
		a.Bind(stubs[d] - 1)
		t := c.ctrl[len(c.ctrl)-1-d]
		if d == len(c.ctrl)-1 {
			for i, rt := range c.sig.Results {
				c.copyPosResult(rt.Kind, src+i, i)
			}
			a.Jmp(c.retL)
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
	v := value{t: t}
	if t == module.KindV128 {
		x := c.allocX(0)
		c.a.LoadVec(x, rTmp, 0)
		c.pushReg(t, uint8(x))
		return
	}
	if isFloat(t) {
		x := c.allocX(0)
		if t == module.KindF32 {
			c.a.LoadSS(x, rTmp, 0)
		} else {
			c.a.LoadSD(x, rTmp, 0)
		}
		v.k, v.r = kReg, uint8(x)
	} else {
		r := c.allocG(0)
		if t == module.KindI32 {
			c.a.LoadU32(r, rTmp, 0)
		} else {
			c.a.Load(r, rTmp, 0)
		}
		v.k, v.r = kReg, uint8(r)
	}
	c.push(v)
}

func (c *compiler) globalSet(g uint32) {
	v := c.pop()
	// The value goes to a register first: storing a slot's or a large
	// constant goes through rTmp, which holds the address.
	var r amd64.Reg
	switch {
	case v.t == module.KindV128:
		x := c.intoX(v)
		c.globalAddr(g)
		c.a.StoreVec(rTmp, 0, x)
		c.freeX(x)
		return
	case v.k == kReg && !isFloat(v.t):
		r = v.gpr()
	case v.k == kReg && v.t == module.KindF64:
		c.globalAddr(g)
		c.a.StoreSD(rTmp, 0, v.xmm())
		c.free(v)
		return
	case v.k == kReg:
		r = c.allocG(0)
		c.a.MovDFromX(r, v.xmm())
		c.free(v)
		v = value{t: v.t, k: kReg, r: uint8(r)}
	default:
		r = c.allocG(0)
		bits := v.c
		if v.k == kSlot {
			c.a.Load(r, rL, v.off)
		} else {
			if v.t == module.KindI32 || v.t == module.KindF32 {
				bits = uint64(uint32(bits))
			}
			c.a.MovImm(r, bits)
		}
		v = value{t: v.t, k: kReg, r: uint8(r)}
	}
	c.globalAddr(g)
	c.a.Store(rTmp, 0, r)
	c.free(v)
}

// selectOp compiles select: the first operand if the condition is true.
func (c *compiler) selectOp() {
	cond := c.pop()
	b := c.pop()
	x := c.pop()
	a := c.a
	if isFloat(x.t) {
		xa := c.intoX(x)
		xb := c.intoX(b)
		rc := c.intoG(cond, 0)
		a.Op(amd64.Test, rc, rc, false)
		c.freeG(rc)
		done := a.NewLabel()
		a.Jcc(amd64.CondNE, done)
		a.SSEOp(amd64.MovAPD, xa, xb)
		a.Bind(done)
		c.freeX(xb)
		c.pushReg(x.t, uint8(xa))
		return
	}
	wide := x.t == module.KindI64
	ra := c.intoG(x, 0)
	rb := c.intoG(b, 1<<ra)
	rc := c.intoG(cond, 1<<ra|1<<rb)
	a.Op(amd64.Test, rc, rc, false)
	a.Cmov(amd64.CondE, ra, rb, wide)
	c.freeG(rb)
	c.freeG(rc)
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
	a.OpImm(amd64.Sub, rPoll, 1, true)
	a.Jcc(amd64.CondLE, poll)
	c.nativeCallChecks(idx, c.frameSlots, c.adv, slow)
	// The arguments, into the callee's parameters.
	frame := int32(8 * c.frameSlots)
	if hasRef(ft.Params) || hasV128(ft.Params) {
		c.argsTo(ft.Params, base, frame, c.adv)
	} else {
		for i := 0; i < n; i++ {
			a.Load(amd64.RCX, rL, c.slotOff(base+i))
			a.Store(rL, frame+int32(8*i), amd64.RCX)
		}
	}
	if c.refs {
		c.advanceR()
	}
	ret := a.NewLabel()
	a.LeaLabel(amd64.RCX, ret)
	a.Store(rRS, 0, amd64.RCX)
	a.Store(rRS, 8, rL)
	a.MovImm(amd64.RCX, Site(idx, at))
	a.Store(rRS, 16, amd64.RCX)
	a.OpImm(amd64.Add, rRS, RSEntryWords*8, true)
	a.OpImm(amd64.Add, rL, frame, true)
	a.JmpReg(amd64.RAX)
	a.Bind(ret)
	a.OpImm(amd64.Sub, rL, frame, true)
	if c.refs {
		c.restoreR()
	}
	c.reloadHomes()
	// The results, from the callee's frame.
	if hasRef(ft.Results) || hasV128(ft.Results) {
		c.resultsFrom(ft.Results, base, frame, c.adv)
	} else {
		for i := range ft.Results {
			a.Load(amd64.RCX, rL, frame+int32(8*i))
			a.Store(rL, c.slotOff(base+i), amd64.RCX)
		}
	}
	a.Bind(after)
	slotIdx := c.slotIndex(base)
	c.later(func() {
		a.Bind(poll)
		c.sharedExit(ExitPoll, start)
		a.Bind(slow)
		c.storeCtx(offArg0, uint64(idx))
		c.storeCtx(offArg0+8, slotIdx)
		c.storeCtx(offArg0+16, uint64(at))
		c.sharedExit(ExitCall, after)
	})
	c.pushResults(ft.Results)
}

// nativeCallChecks leaves the callee's entry in AX, or goes to slow: the
// callee has no native code, its frame no room in the chunk past the
// caller's frameSlots -- nor, for one that holds references, room for its
// reference slots past adv bytes of R -- or the return stack none for an
// entry -- which it also has not where the call would pass the depth
// limit, which the call through Go then traps for.
func (c *compiler) nativeCallChecks(idx uint32, frameSlots int, adv int32, slow amd64.Label) {
	a := c.a
	a.Load(amd64.RAX, rCtx, offLinks)
	a.Load(amd64.RAX, amd64.RAX, int32(8*idx))
	a.Op(amd64.Test, amd64.RAX, amd64.RAX, true)
	a.Jcc(amd64.CondE, slow)
	a.Lea(amd64.RCX, rL, int32(8*(frameSlots+c.req.FrameSlotsOf(idx))))
	a.CmpMem(amd64.RCX, rCtx, offChunkEnd, true)
	a.Jcc(amd64.CondA, slow)
	c.refRoomCheck(idx, adv, slow)
	a.CmpMem(rRS, rCtx, offRSEnd, true)
	a.Jcc(amd64.CondAE, slow)
}

// refRoomCheck goes to slow unless function idx's reference slots, if it
// holds references, fit in R's chunk past adv bytes of R.
func (c *compiler) refRoomCheck(idx uint32, adv int32, slow amd64.Label) {
	need := int32(16 * c.req.RefSlotsOf(idx))
	if need == 0 {
		return
	}
	a := c.a
	if c.refs {
		a.Lea(amd64.RCX, rR, adv+need)
	} else {
		a.Load(amd64.RCX, rCtx, offR)
		a.OpImm(amd64.Add, amd64.RCX, adv+need, true)
	}
	a.CmpMem(amd64.RCX, rCtx, offRChunkEnd, true)
	a.Jcc(amd64.CondA, slow)
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
		a.Jmp(c.retL)
		return
	}
	start := a.NewLabel()
	slow := a.NewLabel()
	poll := a.NewLabel()
	a.Bind(start)
	a.OpImm(amd64.Sub, rPoll, 1, true)
	a.Jcc(amd64.CondLE, poll)
	a.Load(amd64.RAX, rCtx, offLinks)
	a.Load(amd64.RAX, amd64.RAX, int32(8*idx))
	a.Op(amd64.Test, amd64.RAX, amd64.RAX, true)
	a.Jcc(amd64.CondE, slow)
	a.Lea(amd64.RCX, rL, int32(8*c.req.FrameSlotsOf(idx)))
	a.CmpMem(amd64.RCX, rCtx, offChunkEnd, true)
	a.Jcc(amd64.CondA, slow)
	c.refRoomCheck(idx, 0, slow)
	// The arguments, into the parameters: each slot read is past each
	// written before it.
	if hasRef(ft.Params) || hasV128(ft.Params) {
		c.argsTo(ft.Params, base, 0, 0)
	} else {
		for i := 0; i < n; i++ {
			a.Load(amd64.RCX, rL, c.slotOff(base+i))
			a.Store(rL, int32(8*i), amd64.RCX)
		}
	}
	// The frame's return stack entry names the callee now.
	a.Load(amd64.RCX, rRS, -8)
	a.MovRR32(amd64.RCX, amd64.RCX)
	a.MovImm(rTmp, uint64(idx)<<32)
	a.Op(amd64.Or, amd64.RCX, rTmp, true)
	a.Store(rRS, -8, amd64.RCX)
	a.JmpReg(amd64.RAX)
	c.later(func() {
		a.Bind(poll)
		c.sharedExit(ExitPoll, start)
		a.Bind(slow)
		c.exit(ExitTailCall, retResume, uint64(idx), slotIdx, uint64(at))
		c.resumeAt(retResume)
		a.Jmp(c.retL)
	})
}
