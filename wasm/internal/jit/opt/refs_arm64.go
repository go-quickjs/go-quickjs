package opt

import (
	"unsafe"

	"github.com/go-quickjs/go-quickjs/internal/jit/asm/arm64"
	wjit "github.com/go-quickjs/go-quickjs/wasm/internal/jit"
)

// References on arm64. A reference is in a reference slot of the frame
// (R, which the context holds, Ctx.R) and never in a register past the
// instruction that moves it: the reference instructions read and write the
// slots through s0 and s1, a pointer word passing through D31 (x0). Every
// store of a pointer word tests the write barrier's flag and, while the
// collector marks, stores through the function's barrier routine, which
// keeps every register the code keeps a value in around the runtime's
// store (internal/jit.StoreRef).

// The context's fields only the references use.
var (
	offFuncs  = int32(unsafe.Offsetof(wjit.Ctx{}.Funcs))
	offWBSlot = int32(unsafe.Offsetof(wjit.Ctx{}.WBSlot))
	offWBPtr  = int32(unsafe.Offsetof(wjit.Ctx{}.WBPtr))
	offWBRet  = int32(unsafe.Offsetof(wjit.Ctx{}.WBRet))
	offRArgs  = int32(unsafe.Offsetof(wjit.Ctx{}.RArgs))
)

// keptRegs are the registers every native frame of a level keeps: L,
// memory 0's base and size, the poll counter and the return stack's next
// entry, which the barrier routine keeps across the runtime's store too.
var keptRegs = [...]arm64.Reg{rL, rMem, rMemSize, rPoll, rRS}

// Every register the barrier routine keeps fits the context's save area.
var _ [wjit.SaveWords - 18 - len(keptRegs) - 30]struct{}

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
		g.a.AddImm(s0, s0, int64(g.adv()), true)
		g.a.Store(rCtx, offR, s0)
	}
}

func (g *gen) restoreR() {
	if g.holdsRefs() {
		g.a.Load(s0, rCtx, offR)
		g.a.AddImm(s0, s0, -int64(g.adv()), true)
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
func (g *gen) barrierStore(dst arm64.Reg, off int32, flag arm64.Reg) {
	a := g.a
	slow, resume := a.NewLabel(), a.NewLabel()
	a.MovImm(flag, g.req.WriteBarrier)
	a.LoadU8(flag, flag, 0)
	a.Cbnz(flag, slow, false)
	a.StoreF(dst, off, x0)
	a.Bind(resume)
	g.ool = append(g.ool, stub{kind: stubBarrier, l: slow, to: resume, dst: dst, off: off, flag: flag})
}

// refMove copies the reference at src plus soff to dst plus doff, dst and
// src two of s0 and s1: the payload plainly, the pointer word through the
// barrier. Both are lost.
func (g *gen) refMove(dst arm64.Reg, doff int32, src arm64.Reg, soff int32) {
	a := g.a
	a.LoadF(x0, src, soff+8)
	a.StoreF(dst, doff+8, x0)
	a.LoadF(x0, src, soff)
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
func (g *gen) wbRoutine() arm64.Label {
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
// stored by the runtime's barrier -- the address in X15, the pointer in
// X17, the return in X16 -- every register the allocator has and the
// registers the code keeps saved around it.
func (g *gen) emitWB(l arm64.Label) {
	a := g.a
	a.Bind(l)
	g.mark("cold barrier routine")
	a.Store(rCtx, offWBRet, s0)
	store := a.NewLabel()
	a.Load(s0, rCtx, offWBSlot)
	a.Load(s0, s0, 0)
	a.Cbnz(s0, store, true)
	a.Load(s0, rCtx, offWBPtr)
	a.Cbnz(s0, store, true)
	a.Load(s0, rCtx, offWBRet)
	a.Br(s0)
	a.Bind(store)
	for i, r := range wbRegs() {
		a.Store(rCtx, offSave+int32(8*i), r)
	}
	for i, x := range fprs {
		if g.v128 {
			a.StoreQ(rCtx, offSaveV+int32(16*x), arm64.FReg(x))
			continue
		}
		a.StoreF(rCtx, offSave+int32(8*(len(gprs)+len(keptRegs)+i)), arm64.FReg(x))
	}
	a.Load(arm64.Reg(15), rCtx, offWBSlot)
	a.Load(s1, rCtx, offWBPtr)
	back := a.NewLabel()
	a.Adr(s0, back)
	a.MovImm(arm64.Reg(1), g.req.StoreRef)
	a.Br(arm64.Reg(1))
	a.Bind(back)
	for i, r := range wbRegs() {
		a.Load(r, rCtx, offSave+int32(8*i))
	}
	for i, x := range fprs {
		if g.v128 {
			a.LoadQ(arm64.FReg(x), rCtx, offSaveV+int32(16*x))
			continue
		}
		a.LoadF(arm64.FReg(x), rCtx, offSave+int32(8*(len(gprs)+len(keptRegs)+i)))
	}
	a.Load(s0, rCtx, offWBRet)
	a.Br(s0)
}

// wbRegs are the general registers the barrier routine keeps: the
// allocator's, then the ones the code keeps.
func wbRegs() []arm64.Reg {
	out := make([]arm64.Reg, 0, len(gprs)+len(keptRegs))
	for _, r := range gprs {
		out = append(out, arm64.Reg(r))
	}
	return append(out, keptRegs[:]...)
}

// clearRefs makes every reference slot of the frame but the parameters'
// null, as the engine's frames are made, at the function's start, where
// no value is in a register yet: X1 walks the slots and X2 counts them.
func (g *gen) clearRefs() {
	from, to := g.f.RefParams, g.req.FrameRefSlots
	if from >= to {
		return
	}
	a := g.a
	g.mark("clear references")
	p, n := arm64.Reg(1), arm64.Reg(2)
	a.Load(p, rCtx, offR)
	a.AddImm(p, p, int64(16*from), true)
	a.MovImm(n, uint64(to-from))
	loop := a.NewLabel()
	a.Bind(loop)
	a.Store(p, 8, arm64.ZR)
	a.FMovToF(x0, arm64.ZR)
	g.barrierStore(p, 0, s1)
	a.AddImm(p, p, 16, true)
	a.SubsImm(n, n, 1, true)
	a.BCond(arm64.NE, loop)
}

// nullTrap traps where the reference call_ref or return_call_ref v calls
// is null.
func (g *gen) nullTrap(v *Value) {
	a := g.a
	a.Load(s0, rCtx, offR)
	a.Load(s0, s0, g.calleeRefOff(v))
	a.Cbz(s0, g.trap(wjit.TrapNullDereference, v.At), true)
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
	table := int32(8 * (v.Aux >> 8))
	g.movG(s0, v.Args[0])
	a.Load(s1, rCtx, offTables)
	a.Load(s1, s1, table)
	a.Load(s1, s1, lay.TableElems+8)
	a.Cmp(s0, s1, true)
	a.BCond(arm64.HS, g.trap(wjit.TrapTableOutOfBounds, v.At))
	a.Load(s1, rCtx, offTables)
	a.Load(s1, s1, table)
	a.Load(s1, s1, lay.TableElems)
	a.AddLsl(s1, s1, s0, 4)
}

// refOp lowers a reference instruction (OpRef).
func (g *gen) refOp(v *Value) {
	a := g.a
	dst, src := refOff(uint32(v.Aux2>>32)), refOff(uint32(v.Aux2))
	idx := v.Aux >> 8
	switch v.Aux & 0xff {
	case refNull, refFunc:
		if v.Aux&0xff == refNull {
			a.FMovToF(x0, arm64.ZR)
		} else {
			a.Load(s1, rCtx, offFuncs)
			a.LoadF(x0, s1, int32(8*idx))
		}
		a.Load(s0, rCtx, offR)
		a.Store(s0, dst+8, arm64.ZR)
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
		a.CmpImm(s0, 0, true)
		a.Cset(t, arm64.EQ)
		g.done(v, int(t))
	case refEq:
		ne := a.NewLabel()
		a.Load(s0, rCtx, offR)
		a.Load(s1, s0, dst)
		a.Load(s0, s0, src)
		a.Cmp(s1, s0, true)
		a.BCond(arm64.NE, ne)
		a.Load(s0, rCtx, offR)
		a.Load(s1, s0, dst+8)
		a.Load(s0, s0, src+8)
		a.Cmp(s1, s0, true)
		a.Bind(ne)
		t := g.defG(v)
		a.Cset(t, arm64.EQ)
		g.done(v, int(t))
	case refAsNonNull:
		a.Load(s0, rCtx, offR)
		a.Load(s0, s0, src)
		a.Cbz(s0, g.trap(wjit.TrapNullDereference, v.At), true)
	case refSelect:
		keep := a.NewLabel()
		c := g.readG(v.Args[0], s0)
		a.Cbnz(c, keep, false)
		g.slotCopy(dst, src)
		a.Bind(keep)
	case refTableSize:
		t := g.defG(v)
		a.Load(t, rCtx, offTables)
		a.Load(t, t, int32(8*idx))
		a.Load(t, t, g.req.Layout.TableElems+8)
		g.done(v, int(t))
	default:
		panic("wasm opt: no lowering of a reference instruction")
	}
}

// tail lowers a tail call: in place of the frame, natively where the
// callee has native code and room -- its arguments into the frame's
// parameters, the frame's return stack entry renamed, a jump to the
// callee -- else through Go, whose resumption returns, the results in
// their slots. A call through a table or a reference is made natively as
// call_indirect is, where its signature has no reference.
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
		a.SubsImm(rPoll, rPoll, 1, true)
		a.BCond(arm64.LE, poll)
		args := v.Args
		if v.Op == OpTail {
			idx := uint32(v.Aux)
			a.Load(s0, rCtx, offLinks)
			a.Load(s0, s0, int32(8*idx))
			a.Cbz(s0, slow, true)
			a.AddImm(s0, rL, int64(8*g.req.FrameSlotsOf(idx)), true)
			a.Load(s1, rCtx, offChunkEnd)
			a.Cmp(s0, s1, true)
			a.BCond(arm64.HI, slow)
			if need := int64(16 * g.req.RefSlotsOf(idx)); need > 0 {
				a.Load(s0, rCtx, offR)
				a.AddImm(s0, s0, need, true)
				a.Load(s1, rCtx, offRChunkEnd)
				a.Cmp(s0, s1, true)
				a.BCond(arm64.HI, slow)
			}
			g.refArgs(v, 0)
		} else {
			if !ref {
				args = v.Args[:len(v.Args)-1]
			}
			// The callee's index waits in x0 while the arguments move.
			g.indirectCallee(v, 0, 0, slow)
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
		if v.Op == OpTail {
			idx := uint32(v.Aux)
			a.MovImm(s0, uint64(idx))
			a.Store32(rRS, -4, s0)
			a.Load(s0, rCtx, offLinks)
			a.Load(s0, s0, int32(8*idx))
		} else {
			a.FMovFromF(s0, x0)
			a.Store32(rRS, -4, s0)
			a.Load(s1, rCtx, offLinks)
			a.AddLsl(s1, s1, s0, 3)
			a.Load(s0, s1, 0)
		}
		a.Br(s0)
		g.ool = append(g.ool, stub{kind: stubPoll, l: poll, to: start})
		g.ool = append(g.ool, stub{kind: stubTail, l: slow, v: v})
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
	a.AddImm(rRS, rRS, -wjit.RSEntryWords*8, true)
	a.Load(rL, rRS, 8)
	a.Load(s0, rRS, 0)
	a.Br(s0)
}
