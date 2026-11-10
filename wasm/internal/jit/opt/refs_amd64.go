package opt

import (
	"unsafe"

	"github.com/go-quickjs/go-quickjs/internal/jit/asm/amd64"
	wjit "github.com/go-quickjs/go-quickjs/wasm/internal/jit"
)

// References on amd64. A reference is in a reference slot of the frame
// (R, which the context holds, Ctx.R) and never in a register past the
// instruction that moves it: the reference instructions read and write the
// slots through s0 and s1, a pointer word passing through x0. Every store
// of a pointer word tests the write barrier's flag and, while the
// collector marks, stores through the function's barrier routine, which
// keeps every register around the runtime's store (internal/jit.StoreRef).

// The context's fields only the references use.
var (
	offFuncs  = int32(unsafe.Offsetof(wjit.Ctx{}.Funcs))
	offWBSlot = int32(unsafe.Offsetof(wjit.Ctx{}.WBSlot))
	offWBPtr  = int32(unsafe.Offsetof(wjit.Ctx{}.WBPtr))
	offWBRet  = int32(unsafe.Offsetof(wjit.Ctx{}.WBRet))
	offRArgs  = int32(unsafe.Offsetof(wjit.Ctx{}.RArgs))
)

// wbSaved are the general registers the barrier routine keeps across the
// runtime's store, which keeps only DI: every register the code keeps a
// value in, the scratch s1 among them. X0-X14 are kept too.
var wbSaved = [...]amd64.Reg{amd64.RAX, amd64.RCX, amd64.RDX, amd64.RBX, amd64.RSI,
	amd64.R8, amd64.R9, amd64.R10, amd64.R12, amd64.R13, amd64.R15}

// holdsRefs reports whether the function holds references: the engine's
// frame has reference slots, which a native call goes past (adv).
func (g *gen) holdsRefs() bool { return g.req.FrameRefSlots > 0 }

// adv is the bytes of the frame's reference slots.
func (g *gen) adv() int32 { return int32(16 * g.req.FrameRefSlots) }

// advanceR moves the context's R past the frame's reference slots, for a
// native call, if the function holds references; restoreR moves it back.
func (g *gen) advanceR() {
	if g.holdsRefs() {
		g.a.Load(s0, rCtx, offR)
		g.a.OpImm(amd64.Add, s0, g.adv(), true)
		g.a.Store(rCtx, offR, s0)
	}
}

func (g *gen) restoreR() {
	if g.holdsRefs() {
		g.a.Load(s0, rCtx, offR)
		g.a.OpImm(amd64.Sub, s0, g.adv(), true)
		g.a.Store(rCtx, offR, s0)
	}
}

// refOff is the offset of reference slot k from R.
func refOff(k uint32) int32 { return int32(16 * k) }

// posOff is the offset of stack position h's reference slot from R.
func (g *gen) posOff(h int) int32 { return int32(16 * (g.f.RLocals + h)) }

// calleeRefOff is the offset from R of the reference call_ref or
// return_call_ref v calls.
func (g *gen) calleeRefOff(v *Value) int32 {
	return g.posOff(int(v.Pos) + len(g.sigOf(v).Params))
}

// sigHasRefs reports whether the signature of call v has a reference
// parameter or result.
func (g *gen) sigHasRefs(v *Value) bool {
	ft := g.sigOf(v)
	for _, t := range ft.Params {
		if t.IsRef() {
			return true
		}
	}
	for _, t := range ft.Results {
		if t.IsRef() {
			return true
		}
	}
	return false
}

// barrierStore stores the pointer word in x0 at dst plus off: through the
// barrier routine while the collector marks, flag, not dst, being lost.
func (g *gen) barrierStore(dst amd64.Reg, off int32, flag amd64.Reg) {
	a := g.a
	slow, resume := a.NewLabel(), a.NewLabel()
	a.MovImm(flag, g.req.WriteBarrier)
	a.TestMem8(flag, 0, 0xff)
	a.Jcc(amd64.CondNE, slow)
	a.StoreSD(dst, off, x0)
	a.Bind(resume)
	g.ool = append(g.ool, stub{kind: stubBarrier, l: slow, to: resume, dst: dst, off: off, flag: flag})
}

// refMove copies the reference at src plus soff to dst plus doff, dst and
// src two of s0 and s1: the payload plainly, the pointer word through the
// barrier. Both are lost.
func (g *gen) refMove(dst amd64.Reg, doff int32, src amd64.Reg, soff int32) {
	a := g.a
	a.LoadSD(x0, src, soff+8)
	a.StoreSD(dst, doff+8, x0)
	a.LoadSD(x0, src, soff)
	g.barrierStore(dst, doff, src)
}

// slotCopy copies reference slot from to reference slot to.
func (g *gen) slotCopy(to, from int32) {
	if to == from {
		return
	}
	g.a.Load(s0, rCtx, offR)
	g.a.MovRR(s1, s0)
	g.refMove(s0, to, s1, from)
}

// wbRoutine returns the function's barrier routine, made after the body
// with its first use.
func (g *gen) wbRoutine() amd64.Label {
	if !g.hasWB {
		g.hasWB = true
		g.wb = g.a.NewLabel()
		g.ool = append(g.ool, stub{kind: stubWB, l: g.wb})
	}
	return g.wb
}

// emitWB emits the barrier routine at l: the slot's address in the
// context's WBSlot, the pointer in WBPtr, where to go on in s0. A nil
// stored over a nil is nothing to store or to shade; anything else is
// stored by the runtime's barrier, every register the code keeps saved
// around it.
func (g *gen) emitWB(l amd64.Label) {
	a := g.a
	a.Bind(l)
	g.mark("cold barrier routine")
	a.Store(rCtx, offWBRet, s0)
	store := a.NewLabel()
	a.Load(s0, rCtx, offWBSlot)
	a.Load(s0, s0, 0)
	a.Op(amd64.Test, s0, s0, true)
	a.Jcc(amd64.CondNE, store)
	a.Load(s0, rCtx, offWBPtr)
	a.Op(amd64.Test, s0, s0, true)
	a.Jcc(amd64.CondNE, store)
	a.JmpMem(rCtx, offWBRet)
	a.Bind(store)
	for i, r := range wbSaved {
		a.Store(rCtx, offSave+int32(8*i), r)
	}
	for x := amd64.XReg(0); x < 15; x++ {
		if g.v128 {
			a.StoreVec(rCtx, offSaveV+16*int32(x), x)
			continue
		}
		a.StoreSD(rCtx, offSave+int32(8*(len(wbSaved)+int(x))), x)
	}
	a.Load(amd64.RAX, rCtx, offWBSlot)
	a.Load(amd64.RCX, rCtx, offWBPtr)
	back := a.NewLabel()
	a.LeaLabel(s0, back)
	a.MovImm(amd64.RDX, g.req.StoreRef)
	a.JmpReg(amd64.RDX)
	a.Bind(back)
	for i, r := range wbSaved {
		a.Load(r, rCtx, offSave+int32(8*i))
	}
	for x := amd64.XReg(0); x < 15; x++ {
		if g.v128 {
			a.LoadVec(x, rCtx, offSaveV+16*int32(x))
			continue
		}
		a.LoadSD(x, rCtx, offSave+int32(8*(len(wbSaved)+int(x))))
	}
	a.JmpMem(rCtx, offWBRet)
}

// clearRefs makes every reference slot of the frame but the parameters'
// null, as the engine's frames are made, at the function's start, where
// no value is in a register yet.
func (g *gen) clearRefs() {
	from, to := g.f.RefParams, g.req.FrameRefSlots
	if from >= to {
		return
	}
	a := g.a
	g.mark("clear references")
	// The barrier's flag is tested once: while the collector does not
	// mark, the slots are cleared by plain stores, and while it marks each
	// pointer word through the barrier.
	n := to - from
	slow, done := a.NewLabel(), a.NewLabel()
	a.MovImm(s0, g.req.WriteBarrier)
	a.TestMem8(s0, 0, 0xff)
	a.Jcc(amd64.CondNE, slow)
	a.Load(amd64.RAX, rCtx, offR)
	if n <= 8 {
		for k := from; k < to; k++ {
			a.StoreImm(amd64.RAX, int32(16*k), 0)
			a.StoreImm(amd64.RAX, int32(16*k+8), 0)
		}
	} else {
		a.OpImm(amd64.Add, amd64.RAX, int32(16*from), true)
		a.MovImm(amd64.RCX, uint64(n))
		loop := a.NewLabel()
		a.Bind(loop)
		a.StoreImm(amd64.RAX, 0, 0)
		a.StoreImm(amd64.RAX, 8, 0)
		a.OpImm(amd64.Add, amd64.RAX, 16, true)
		a.OpImm(amd64.Sub, amd64.RCX, 1, true)
		a.Jcc(amd64.CondNE, loop)
	}
	a.Jmp(done)
	a.Bind(slow)
	a.Load(amd64.RAX, rCtx, offR)
	a.OpImm(amd64.Add, amd64.RAX, int32(16*from), true)
	a.MovImm(amd64.RCX, uint64(n))
	loop := a.NewLabel()
	a.Bind(loop)
	a.StoreImm(amd64.RAX, 8, 0)
	a.SSEOp(amd64.XorPD, x0, x0)
	g.barrierStore(amd64.RAX, 0, amd64.RDX)
	a.OpImm(amd64.Add, amd64.RAX, 16, true)
	a.OpImm(amd64.Sub, amd64.RCX, 1, true)
	a.Jcc(amd64.CondNE, loop)
	a.Bind(done)
}

// nullTrap traps where the reference call_ref or return_call_ref v calls
// is null.
func (g *gen) nullTrap(v *Value) {
	a := g.a
	a.Load(s0, rCtx, offR)
	a.Load(s0, s0, g.calleeRefOff(v))
	a.Op(amd64.Test, s0, s0, true)
	a.Jcc(amd64.CondE, g.trap(wjit.TrapNullDereference, v.At))
}

// refArgs copies the reference arguments of call or tail call v from
// their positions' slots into the callee's parameters, at adv bytes past
// R, in order: for a tail call each slot read is past each written.
func (g *gen) refArgs(v *Value, adv int32) {
	k := int32(0)
	for i, t := range g.sigOf(v).Params {
		if !t.IsRef() {
			continue
		}
		g.a.Load(s0, rCtx, offR)
		g.a.MovRR(s1, s0)
		g.refMove(s0, adv+16*k, s1, g.posOff(int(v.Pos)+i))
		k++
	}
}

// refResults copies the reference results of call v from the callee's
// frame, at adv bytes past R, into their positions' slots.
func (g *gen) refResults(v *Value, adv int32) {
	k := int32(0)
	for i, t := range g.sigOf(v).Results {
		if !t.IsRef() {
			continue
		}
		g.a.Load(s0, rCtx, offR)
		g.a.MovRR(s1, s0)
		g.refMove(s0, g.posOff(int(v.Pos)+i), s1, adv+16*k)
		k++
	}
}

// exitCall makes call v through Go: its numeric operands into the call
// area, each at its place among the signature's parameters, its
// references where Ctx.RArgs says, and its numeric results read back.
func (g *gen) exitCall(v *Value, res []*Value) {
	a := g.a
	g.argSlotsBuf = g.argSlots(v, g.argBase, g.argSlotsBuf)
	for i, x := range v.Args {
		g.move(loc{kind: lSlot, slot: g.argSlotsBuf[i]}, g.locOf(x), x.Type)
	}
	if g.hasRefs(v) {
		g.storeCtx(offRArgs, g.rargs(v))
	}
	base, at := uint64(g.argBase), uint64(v.At)
	switch v.Op {
	case OpCall:
		g.exit(wjit.ExitCall, a.NewLabel(), uint64(uint32(v.Aux)), base, at)
	case OpCallIndirect:
		g.exit(wjit.ExitCallIndirect, a.NewLabel(), v.Aux, v.Aux2, base, at)
	case OpCallRef:
		g.exit(wjit.ExitCallRef, a.NewLabel(), v.Aux, base, at)
	}
	g.takeResults(res, int32(g.argBase))
}

// tableElem leaves in s1 the address of the element of table Aux>>8 that
// v's index, Args[0], names, trapping past the table's end. s0 is lost.
func (g *gen) tableElem(v *Value) {
	a := g.a
	lay := g.req.Layout
	g.movG(s0, v.Args[0])
	a.Load(s1, rCtx, offTables)
	a.Load(s1, s1, int32(8*(v.Aux>>8)))
	a.CmpMem(s0, s1, lay.TableElems+8, true)
	a.Jcc(amd64.CondAE, g.trap(wjit.TrapTableOutOfBounds, v.At))
	a.Load(s1, s1, lay.TableElems)
	a.ShiftImm(amd64.Shl, s0, 4, true)
	a.Op(amd64.Add, s1, s0, true)
}

// refOp lowers a reference instruction (OpRef).
func (g *gen) refOp(v *Value) {
	a := g.a
	dst, src := refOff(uint32(v.Aux2>>32)), refOff(uint32(v.Aux2))
	idx := v.Aux >> 8
	switch v.Aux & 0xff {
	case refNull, refFunc:
		if v.Aux&0xff == refNull {
			a.SSEOp(amd64.XorPD, x0, x0)
		} else {
			a.Load(s1, rCtx, offFuncs)
			a.LoadSD(x0, s1, int32(8*idx))
		}
		a.Load(s0, rCtx, offR)
		a.StoreImm(s0, dst+8, 0)
		g.barrierStore(s0, dst, s1)
	case refCopy:
		g.slotCopy(dst, src)
	case refGlobalGet, refGlobalSet:
		a.Load(s1, rCtx, offGlobals)
		a.Load(s1, s1, int32(8*idx))
		a.Load(s0, rCtx, offR)
		if v.Aux&0xff == refGlobalGet {
			g.refMove(s0, dst, s1, 0)
		} else {
			g.refMove(s1, 0, s0, src)
		}
	case refTableGet:
		g.tableElem(v)
		a.Load(s0, rCtx, offR)
		g.refMove(s0, dst, s1, 0)
	case refTableSet:
		g.tableElem(v)
		a.Load(s0, rCtx, offR)
		g.refMove(s1, 0, s0, src)
	case refIsNull:
		t := g.defG(v)
		a.Load(s0, rCtx, offR)
		a.Load(s0, s0, src)
		a.Op(amd64.Test, s0, s0, true)
		a.Setcc(amd64.CondE, t)
		a.MovZX8(t, t)
		g.done(v, int(t))
	case refEq:
		ne := a.NewLabel()
		a.Load(s0, rCtx, offR)
		a.Load(s1, s0, dst)
		a.CmpMem(s1, s0, src, true)
		a.Jcc(amd64.CondNE, ne)
		a.Load(s1, s0, dst+8)
		a.CmpMem(s1, s0, src+8, true)
		a.Bind(ne)
		t := g.defG(v)
		a.Setcc(amd64.CondE, t)
		a.MovZX8(t, t)
		g.done(v, int(t))
	case refAsNonNull:
		a.Load(s0, rCtx, offR)
		a.Load(s0, s0, src)
		a.Op(amd64.Test, s0, s0, true)
		a.Jcc(amd64.CondE, g.trap(wjit.TrapNullDereference, v.At))
	case refSelect:
		keep := a.NewLabel()
		c := g.intoG(v.Args[0], s0)
		a.Op(amd64.Test, c, c, false)
		a.Jcc(amd64.CondNE, keep)
		g.slotCopy(dst, src)
		a.Bind(keep)
	case refTableSize:
		t := g.defG(v)
		a.Load(t, rCtx, offTables)
		a.Load(t, t, int32(8*idx))
		a.Load(t, t, g.req.Layout.TableElems+8)
		g.done(v, int(t))
	default:
		if !g.gcRef(v) {
			panic("wasm opt: no lowering of a reference instruction")
		}
	}
}

// tail lowers a tail call: in place of the frame, natively where the
// callee has native code and room -- its arguments into the frame's
// parameters, the frame's return stack entry renamed, a jump to the
// callee with R10 the return stack's next entry -- else through Go, whose
// resumption returns, the results in their slots. A call through a table
// or a reference is made natively as call_indirect is, where its
// signature has no reference.
func (g *gen) tail(v *Value) {
	a := g.a
	m := g.req.Module
	lay := g.req.Layout
	ref := v.Op == OpTailRef
	if ref {
		g.nullTrap(v)
	}
	native := true
	switch v.Op {
	case OpTail:
		native = !m.Functions[v.Aux].Imported
	default:
		native = lay != nil && int(v.Aux) < len(m.Canonical) && !g.sigHasRefs(v) &&
			(ref || !m.Tables[v.Aux2].Limits.Is64)
	}
	slow := a.NewLabel()
	if native {
		start, poll := a.NewLabel(), a.NewLabel()
		a.Bind(start)
		a.OpImm(amd64.Sub, rPoll, 1, true)
		a.Jcc(amd64.CondLE, poll)
		args := v.Args
		// A tail call of the function by itself goes to this code's entry,
		// in its own frame: the code runs, and its frame has room.
		self := v.Op == OpTail && uint32(v.Aux) == g.fn
		if self {
			g.refArgs(v, 0)
		} else if v.Op == OpTail {
			idx := uint32(v.Aux)
			a.Load(s0, rCtx, offLinks)
			a.Load(s0, s0, int32(8*idx))
			a.Op(amd64.Test, s0, s0, true)
			a.Jcc(amd64.CondE, slow)
			a.Lea(s0, rL, int32(8*g.req.FrameSlotsOf(idx)))
			a.CmpMem(s0, rCtx, offChunkEnd, true)
			a.Jcc(amd64.CondA, slow)
			if need := int32(16 * g.req.RefSlotsOf(idx)); need > 0 {
				a.Load(s0, rCtx, offR)
				a.OpImm(amd64.Add, s0, need, true)
				a.CmpMem(s0, rCtx, offRChunkEnd, true)
				a.Jcc(amd64.CondA, slow)
			}
			g.refArgs(v, 0)
		} else {
			if !ref {
				args = v.Args[:len(v.Args)-1]
			}
			g.indirectCallee(v, 0, 0, slow, 0, false)
			// The callee's index waits in x0 while the arguments move.
			a.MovQToX(x0, s0)
		}
		// The numeric arguments, into the parameters' slots, at once.
		moves := g.moves[:0]
		slot := int32(0)
		for _, x := range args {
			moves = append(moves, pmove{dst: loc{kind: lSlot, slot: slot}, src: g.locOf(x), t: x.Type})
			slot += x.Type.Slots()
		}
		if v.Op == OpTail {
			g.parallel(moves)
		} else {
			// x0 holds the callee's index.
			g.parallelKeeping(moves)
		}
		g.moves = moves[:0]
		g.loadRS()
		if self {
			// The frame's return stack entry names the function already.
			a.Jmp(g.entry)
			g.ool = append(g.ool, stub{kind: stubPoll, l: poll, to: start})
			return
		}
		if v.Op == OpTail {
			idx := uint32(v.Aux)
			a.StoreImm32(rRS, -4, int32(idx))
			a.Load(s0, rCtx, offLinks)
			a.Load(s0, s0, int32(8*idx))
		} else {
			a.MovQFromX(s0, x0)
			a.Store32(rRS, -4, s0)
			a.ShiftImm(amd64.Shl, s0, 3, true)
			a.AddMem(s0, rCtx, offLinks)
			a.Load(s0, s0, 0)
		}
		a.JmpReg(s0)
		g.ool = append(g.ool, stub{kind: stubPoll, l: poll, to: start})
		g.ool = append(g.ool, stub{kind: stubTail, l: slow, v: v, wh: g.keepWhere(v, nil)})
		return
	}
	a.Bind(slow)
	g.exitTail(v)
}

// exitTail makes tail call v through Go, and returns: the exit's
// resumption finds the results in their slots.
func (g *gen) exitTail(v *Value) {
	a := g.a
	g.argSlotsBuf = g.argSlots(v, g.argBase, g.argSlotsBuf)
	for i, x := range v.Args {
		g.move(loc{kind: lSlot, slot: g.argSlotsBuf[i]}, g.locOf(x), x.Type)
	}
	if g.hasRefs(v) {
		g.storeCtx(offRArgs, g.rargs(v))
	}
	base, at := uint64(g.argBase), uint64(v.At)
	switch v.Op {
	case OpTail:
		g.exit(wjit.ExitTailCall, a.NewLabel(), uint64(uint32(v.Aux)), base, at)
	case OpTailIndirect:
		g.exit(wjit.ExitTailIndirect, a.NewLabel(), v.Aux, v.Aux2, base, at)
	case OpTailRef:
		g.exit(wjit.ExitTailRef, a.NewLabel(), v.Aux, base, at)
	}
	a.Load(rRS, rCtx, offRSP)
	a.OpImm(amd64.Sub, rRS, wjit.RSEntryWords*8, true)
	a.Load(rL, rRS, 8)
	a.JmpMem(rRS, 0)
}
