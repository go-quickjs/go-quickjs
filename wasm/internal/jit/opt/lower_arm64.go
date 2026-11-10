package opt

import (
	"fmt"
	"math"
	"slices"
	"unsafe"

	"github.com/go-quickjs/go-quickjs/internal/jit/asm/arm64"
	wjit "github.com/go-quickjs/go-quickjs/wasm/internal/jit"
	"github.com/go-quickjs/go-quickjs/wasm/internal/opcode"
)

// Available says this architecture has the optimizing tier's lowering.
const Available = true

// The registers. The context, L, memory 0's base and size, the poll
// counter and the return stack's next entry are where the baseline keeps
// them (X0, X19, X20, X21, X22, X23), so that the tiers call each other.
// X16 and X17 are the general scratch registers, D31 and D30 the float
// ones; X27 is the encoder's temporary, which the code also takes for a
// remainder's quotient between two instructions that never need it. The
// allocator has the rest but SP, X18, X28, X29 and X30.
const (
	rCtx     = arm64.Reg(0)
	rL       = arm64.Reg(19)
	rMem     = arm64.Reg(20)
	rMemSize = arm64.Reg(21)
	rPoll    = arm64.Reg(22)
	rRS      = arm64.Reg(23)
	s0       = arm64.Reg(16)
	s1       = arm64.Reg(17)
	rQuot    = arm64.X27
	x0       = arm64.FReg(31)
	x1       = arm64.FReg(30)
)

// The allocator's registers: X1-X12 and X24-X26, then X13, X14 and X15,
// which memory.copy and memory.fill take (bulkDst, bulkSrc, bulkLen), and
// D0-D29.
var (
	gprs    = []int{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 24, 25, 26, 13, 14, 15}
	fprs    = []int{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16, 17, 18, 19, 20, 21, 22, 23, 24, 25, 26, 27, 28, 29}
	classes = [][]int{gprs, fprs}
)

const (
	bulkDst = arm64.Reg(13)
	bulkSrc = arm64.Reg(14)
	bulkLen = arm64.Reg(15)
)

// The registers instructions clobber, by class: memory.copy's and
// memory.fill's X13, X14 and X15, and an atomic store's,
// read-modify-write's and compare-and-exchange's. Division and shifts take
// any register.
var arm64Clobbers = clobbers{
	bulk:   []uint64{1<<bulkDst | 1<<bulkSrc | 1<<bulkLen},
	atomic: []uint64{1<<bulkDst | 1<<bulkSrc | 1<<bulkLen},
}

// The context's fields' offsets.
var (
	offL         = int32(unsafe.Offsetof(wjit.Ctx{}.L))
	offLinks     = int32(unsafe.Offsetof(wjit.Ctx{}.Links))
	offMemBase   = int32(unsafe.Offsetof(wjit.Ctx{}.MemBase))
	offMemSize   = int32(unsafe.Offsetof(wjit.Ctx{}.MemSize))
	offGlobals   = int32(unsafe.Offsetof(wjit.Ctx{}.Globals))
	offTables    = int32(unsafe.Offsetof(wjit.Ctx{}.Tables))
	offInst      = int32(unsafe.Offsetof(wjit.Ctx{}.Inst))
	offFrames    = int32(unsafe.Offsetof(wjit.Ctx{}.Frames))
	offRFrames   = int32(unsafe.Offsetof(wjit.Ctx{}.RFrames))
	offChunkEnd  = int32(unsafe.Offsetof(wjit.Ctx{}.ChunkEnd))
	offRSP       = int32(unsafe.Offsetof(wjit.Ctx{}.RSP))
	offRSEnd     = int32(unsafe.Offsetof(wjit.Ctx{}.RSEnd))
	offPoll      = int32(unsafe.Offsetof(wjit.Ctx{}.Poll))
	offR         = int32(unsafe.Offsetof(wjit.Ctx{}.R))
	offRChunkEnd = int32(unsafe.Offsetof(wjit.Ctx{}.RChunkEnd))
	offSave      = int32(unsafe.Offsetof(wjit.Ctx{}.Save))
	offSaveV     = int32(unsafe.Offsetof(wjit.Ctx{}.SaveV))
	offExit      = int32(unsafe.Offsetof(wjit.Ctx{}.Exit))
	offArg0      = int32(unsafe.Offsetof(wjit.Ctx{}.Arg0))
	offExitFn    = int32(unsafe.Offsetof(wjit.Ctx{}.ExitFn))
	offExitAddr  = int32(unsafe.Offsetof(wjit.Ctx{}.ExitAddr))
	offPools     = int32(unsafe.Offsetof(wjit.Ctx{}.Pools))
	offResume    = int32(unsafe.Offsetof(wjit.Ctx{}.Resume))
)

// Every register the allocator has fits the context's save area.
var _ [wjit.SaveWords - 18 - 30]struct{}

// genScratch is the lowering's part of a workspace: the lowering's state,
// the allocator, its description of the function, and the assembler.
type genScratch struct {
	g gen
	allocScratch
	asm arm64.Asm
}

// gen lowers an optimized function to arm64 code.
type gen struct {
	lowering
	ws *genScratch
	a  *arm64.Asm
	// labels is each block's label, by index.
	labels []arm64.Label
	// ool is the out-of-line code, emitted after the blocks.
	ool []stub
	// traps are the trap stubs by code and offset (trapKey), trapTo the
	// code that traps by code, polls the poll exits by block index.
	traps  u64map[arm64.Label]
	trapTo u64map[arm64.Label]
	polls  u64map[arm64.Label]
	// poll is the function's poll exit, which its sites share, made by the
	// first (hasPoll); usedG and usedX are the registers the allocator gave
	// out, which it keeps across the exit.
	poll         arm64.Label
	hasPoll      bool
	usedG, usedX uint64
	// moves and pending are the parallel moves' lists.
	moves, pending []pmove
	// wb is the function's barrier routine, which hasWB says is made;
	// argSlotsBuf is a call's arguments' slots in the call area.
	wb          arm64.Label
	hasWB       bool
	argSlotsBuf []int32
	// alloc is the function's exit for an empty pool of new structs,
	// which hasAlloc says is made.
	alloc    arm64.Label
	hasAlloc bool
	// vpool is the code's v128 constants, emitted after it.
	vpool vpool[arm64.Label]
}

// reset readies g, kept in a workspace, for function f, keeping the
// memory of its lists and tables.
func (g *gen) reset(ws *genScratch, f *Func, req *wjit.Request) {
	clear(g.ool[:cap(g.ool)])
	g.traps.reset(64)
	g.trapTo.reset(8)
	g.polls.reset(8)
	g.lowering.reset(f, req)
	*g = gen{lowering: g.lowering, ws: ws, a: &ws.asm, labels: g.labels[:0], ool: g.ool[:0], traps: g.traps, trapTo: g.trapTo,
		polls: g.polls, moves: g.moves[:0], pending: g.pending[:0], argSlotsBuf: g.argSlotsBuf[:0], vpool: g.vpool.reset()}
	ws.asm.Reset()
}

// mark says the code from here is emitted for the tag format makes.
func (g *gen) mark(format string, args ...any) {
	if g.marking {
		g.marks = append(g.marks, wjit.Mark{Off: g.a.Len(), Tag: fmt.Sprintf(format, args...)})
	}
}

// gprName and fprName are a general and a float register's names in the
// listing.
func gprName(r int) string { return fmt.Sprintf("x%d", r) }

func fprName(r int) string { return fmt.Sprintf("d%d", r) }

// lower lowers f, optimized, to native code for req's function.
func lower(f *Func, req *wjit.Request, ws *workspace) *wjit.Code {
	g := &ws.gen.g
	g.reset(&ws.gen, f, req)
	for i, b := range f.Blocks {
		g.index[b.ID] = int32(i)
	}
	g.allocate(&g.ws.allocScratch, classes, &arm64Clobbers)
	for i, r := range g.ra.Reg {
		// The temporaries past the values hold nothing across a poll.
		if r >= 0 && i < len(g.byVreg) {
			if g.byVreg[i].Type.InFloatReg() {
				g.usedX |= 1 << r
			} else {
				g.usedG |= 1 << r
			}
		}
	}
	a := g.a
	g.mark("return stub")
	retStub := a.Len()
	a.Store(rCtx, offPoll, rPoll)
	a.Store(rCtx, offRSP, rRS)
	g.storeCtx(offExit, wjit.ExitReturn)
	a.Ret()
	goEntry, entry := a.NewLabel(), a.NewLabel()
	a.Bind(goEntry)
	g.mark("entry")
	g.loadKept()
	a.Bind(entry)
	if g.holdsRefs() {
		g.clearRefs()
	}
	for range f.Blocks {
		g.labels = append(g.labels, a.NewLabel())
	}
	for i, b := range f.Blocks {
		a.Bind(g.labels[i])
		g.mark("%v", b)
		g.block(i, b)
	}
	for i := 0; i < len(g.ool); i++ {
		g.stub(&g.ool[i])
	}
	g.emitVConsts()
	lap(PhaseEmit)
	out, err := a.Finish()
	lap(PhaseEncode)
	if err != nil || (req.MaxCodeBytes > 0 && len(out) > req.MaxCodeBytes) {
		refuse(RefuseSize, "%d bytes of code", a.Len())
	}
	// The assembler's buffer is the workspace's: the code is a copy.
	code := &wjit.Code{Bytes: slices.Clone(out), GoEntry: a.Offset(goEntry), Entry: a.Offset(entry), ReturnStub: retStub}
	if len(f.sites) > 0 {
		code.Handlers = f.handlers(func(b *Block) int { return a.Offset(g.labels[g.index[b.ID]]) }, g.argBase, req.CaughtSlot)
	}
	if g.marking {
		code.Marks, code.Listing = g.marks, g.listing(gprName, fprName)
	}
	return code
}

// loadKept loads the registers the code keeps from the context: L, memory
// 0's base and size, the poll counter and the return stack's next entry.
func (g *gen) loadKept() {
	a := g.a
	a.Load(rL, rCtx, offL)
	a.Load(rMem, rCtx, offMemBase)
	a.Load(rMemSize, rCtx, offMemSize)
	a.Load(rPoll, rCtx, offPoll)
	a.Load(rRS, rCtx, offRSP)
}

// The moves between locations, of a value of type t. A slot's word holds
// an i32 or an f32 with its upper half clear, as the interpreter's frames
// do: Go reads the slots of calls through it. An f32 in a float register
// has the rest of the register clear -- every instruction that makes one
// writes its S view, which clears the rest -- so its D view is the word.

// move copies src to dst; s0 is its scratch.
func (g *gen) move(dst, src loc, t Type) {
	a := g.a
	if dst == src {
		return
	}
	if t == TV128 {
		g.moveV(dst, src)
		return
	}
	switch dst.kind {
	case lGPR:
		r := arm64.Reg(dst.r)
		switch src.kind {
		case lGPR:
			a.MovRR(r, arm64.Reg(src.r))
		case lSlot:
			if t == TI32 {
				a.LoadU32(r, rL, slotOff(src.slot))
			} else {
				a.Load(r, rL, slotOff(src.slot))
			}
		case lConst:
			a.MovImm(r, constBits(src.c, t))
		case lXMM:
			if t == TF32 || t == TI32 {
				a.FMovFromS(r, arm64.FReg(src.r))
			} else {
				a.FMovFromF(r, arm64.FReg(src.r))
			}
		}
	case lXMM:
		x := arm64.FReg(dst.r)
		switch src.kind {
		case lXMM:
			a.FMov(x, arm64.FReg(src.r))
		case lSlot:
			if t == TF32 {
				a.LoadS(x, rL, slotOff(src.slot))
			} else {
				a.LoadF(x, rL, slotOff(src.slot))
			}
		case lConst:
			c := constBits(src.c, t)
			if c == 0 {
				a.FMovToF(x, arm64.ZR)
				return
			}
			if imm, ok := arm64.FloatImm(c); ok && t == TF64 {
				a.FMovImm(x, imm)
				return
			}
			a.MovImm(s0, c)
			a.FMovToF(x, s0)
		case lGPR:
			if t == TF32 || t == TI32 {
				a.FMovToS(x, arm64.Reg(src.r))
			} else {
				a.FMovToF(x, arm64.Reg(src.r))
			}
		}
	case lSlot:
		off := slotOff(dst.slot)
		switch src.kind {
		case lGPR:
			a.Store(rL, off, arm64.Reg(src.r))
		case lXMM:
			a.StoreF(rL, off, arm64.FReg(src.r))
		case lSlot:
			a.Load(s0, rL, slotOff(src.slot))
			a.Store(rL, off, s0)
		case lConst:
			if c := constBits(src.c, t); c == 0 {
				a.Store(rL, off, arm64.ZR)
			} else {
				a.MovImm(s0, c)
				a.Store(rL, off, s0)
			}
		}
	}
}

// constBits are the bits of constant c of type t: an i32's and an f32's
// upper half clear.
func constBits(c uint64, t Type) uint64 {
	if t == TI32 || t == TF32 {
		return uint64(uint32(c))
	}
	return c
}

// parallel makes the moves as if all at once: a move waits for those that
// read its destination, and a cycle is broken through s1 or x1.
func (g *gen) parallel(moves []pmove) {
	if g.v128 && hasV128(moves) {
		g.pending = parallelV(moves, g.pending[:0], g.move, vscratch)
		return
	}
	pending := g.pending[:0]
	for _, m := range moves {
		if m.dst != m.src {
			pending = append(pending, m)
		}
	}
	// The list keeps its array for the next moves.
	g.pending = pending
	for len(pending) > 0 {
		progress := false
		for i := 0; i < len(pending); i++ {
			m := pending[i]
			blocked := false
			for j, o := range pending {
				if j != i && o.src == m.dst {
					blocked = true
					break
				}
			}
			if blocked {
				continue
			}
			g.move(m.dst, m.src, m.t)
			pending = append(pending[:i], pending[i+1:]...)
			i--
			progress = true
		}
		if progress {
			continue
		}
		// Every move is in a cycle: the first's destination goes to a
		// scratch register, and its readers read that.
		d := pending[0].dst
		var tmp loc
		var tt Type
		if d.kind == lXMM {
			tmp, tt = loc{kind: lXMM, r: int(x1)}, TF64
		} else {
			tmp, tt = loc{kind: lGPR, r: int(s1)}, TI64
		}
		g.move(tmp, d, tt)
		for j := range pending {
			if pending[j].src == d {
				pending[j].src = tmp
			}
		}
	}
}

// intoG returns a general register holding v: its own, or scratch loaded.
func (g *gen) intoG(v *Value, scratch arm64.Reg) arm64.Reg {
	l := g.locOf(v)
	if l.kind == lGPR {
		return arm64.Reg(l.r)
	}
	g.move(loc{kind: lGPR, r: int(scratch)}, l, v.Type)
	return scratch
}

// readG is intoG for an operand an instruction reads as a register of its
// register forms, where register 31 is the zero register: a constant 0 is
// that register. (The immediate forms of add, sub and compare read it as
// SP, so what they read is intoG's.)
func (g *gen) readG(v *Value, scratch arm64.Reg) arm64.Reg {
	if l := g.locOf(v); l.kind == lConst && constBits(l.c, v.Type) == 0 {
		return arm64.ZR
	}
	return g.intoG(v, scratch)
}

// intoX returns a float register holding v: its own, or scratch loaded.
func (g *gen) intoX(v *Value, scratch arm64.FReg) arm64.FReg {
	l := g.locOf(v)
	if l.kind == lXMM {
		return arm64.FReg(l.r)
	}
	g.move(loc{kind: lXMM, r: int(scratch)}, l, v.Type)
	return scratch
}

// movG sets a register to v.
func (g *gen) movG(r arm64.Reg, v *Value) { g.move(loc{kind: lGPR, r: int(r)}, g.locOf(v), v.Type) }

// defG and defX are the register v's result is computed in: its own, or
// the first scratch register for a value spilled.
func (g *gen) defG(v *Value) arm64.Reg {
	if r := g.ra.Reg[v.vreg]; r >= 0 {
		return arm64.Reg(r)
	}
	return s0
}

func (g *gen) defX(v *Value) arm64.FReg {
	if r := g.ra.Reg[v.vreg]; r >= 0 {
		return arm64.FReg(r)
	}
	return x0
}

// done stores v's result, computed in register r, into its slot, if it
// has one: its home if spilled, its copy if it lives across a call.
func (g *gen) done(v *Value, r int) {
	s := g.ra.Slot[v.vreg]
	if s < 0 {
		return
	}
	src := loc{kind: lGPR, r: r}
	if v.Type.InFloatReg() {
		src.kind = lXMM
	}
	if g.marking {
		g.mark("%v spill", v)
	}
	g.move(loc{kind: lSlot, slot: s}, src, v.Type)
}

// trap returns a label that traps with code at offset at.
func (g *gen) trap(code uint64, at uint32) arm64.Label {
	k := trapKey(code, at)
	if l, ok := g.traps.get(k); ok {
		return l
	}
	a := g.a
	l := a.NewLabel()
	g.traps.set(k, l)
	to, ok := g.trapTo.get(code)
	if !ok {
		to = a.NewLabel()
		g.trapTo.set(code, to)
		g.ool = append(g.ool, stub{kind: stubTrapCode, to: to, code: code})
	}
	g.ool = append(g.ool, stub{kind: stubTrapSite, l: l, to: to, at: at})
	return l
}

// stub is out-of-line code, emitted after the blocks: the exit of a trap
// with a code, a trap's site, its offset stored before it goes there, a
// poll's site, the function's poll exit, and a call's poll and its call
// through Go.
type stub struct {
	kind uint8
	// l is the stub's label, to where a trap's site goes, or a poll where
	// it goes back; start, slow and after are a call's.
	l, to              arm64.Label
	start, slow, after arm64.Label
	code               uint64
	at                 uint32
	// v is the call, res its results.
	v   *Value
	res []*Value
	// dst, off and flag are a barrier's store (stubBarrier): its slot's
	// base and offset, and the register its slow path has.
	dst, flag arm64.Reg
	off       int32
}

// The stubs' kinds.
const (
	stubTrapCode = iota
	stubTrapSite
	stubPoll
	stubPollExit
	stubCall
	stubCallIndirect
	stubCallRef
	stubBarrier
	stubWB
	stubTail
	stubRefRoom
	stubAlloc
	stubAllocExit
)

// stub emits out-of-line code.
func (g *gen) stub(st *stub) {
	a := g.a
	switch st.kind {
	case stubTrapCode:
		a.Bind(st.to)
		g.mark("cold trap")
		a.Store(rCtx, offL, rL)
		a.Store(rCtx, offRSP, rRS)
		g.storeCtx(offExit, wjit.ExitTrap)
		g.storeCtx(offArg0, st.code)
		g.storeCtx(offExitFn, uint64(g.fn))
		a.Ret()
	case stubTrapSite:
		a.Bind(st.l)
		g.mark("cold trap")
		g.storeCtx(offArg0+8, uint64(st.at))
		a.B(st.to)
	case stubPoll:
		a.Bind(st.l)
		g.mark("cold poll")
		a.Adr(s0, st.to)
		a.B(g.pollExit())
	case stubPollExit:
		// The function's poll exit: every register the allocator gave out
		// kept in the context's save area across it, and the code goes on
		// where its site left in s0.
		a.Bind(st.l)
		g.mark("cold poll exit")
		a.Store(rCtx, offResume, s0)
		g.saveAll()
		g.exit(wjit.ExitPoll, a.NewLabel())
		g.restoreAll()
		a.Load(s0, rCtx, offResume)
		a.Br(s0)
	case stubAlloc:
		// An empty pool of new structs of type code: the function's exit
		// for it, to take from it again at to.
		a.Bind(st.l)
		g.mark("cold alloc")
		g.storeCtx(offArg0, st.code)
		g.storeCtx(offArg0+8, uint64(st.at))
		a.Adr(s0, st.to)
		a.B(g.allocExit())
	case stubAllocExit:
		// The function's exit for an empty pool: the engine fills it,
		// every register the allocator gave out kept, and the code goes
		// on where its site left in s0, its arguments stored.
		a.Bind(st.l)
		g.mark("cold alloc exit")
		a.Store(rCtx, offResume, s0)
		g.saveAll()
		g.exit(wjit.ExitAlloc, a.NewLabel())
		g.restoreAll()
		a.Load(s0, rCtx, offResume)
		a.Br(s0)
	case stubCall, stubCallIndirect, stubCallRef:
		v := st.v
		a.Bind(st.l)
		g.mark("cold call")
		a.Adr(s0, st.start)
		a.B(g.pollExit())
		a.Bind(st.slow)
		g.exitCall(v, st.res)
		a.B(st.after)
	case stubBarrier:
		a.Bind(st.l)
		g.mark("cold barrier")
		a.FMovFromF(st.flag, x0)
		a.Store(rCtx, offWBPtr, st.flag)
		a.AddImm(st.flag, st.dst, int64(st.off), true)
		a.Store(rCtx, offWBSlot, st.flag)
		a.Adr(s0, st.to)
		a.B(g.wbRoutine())
	case stubWB:
		g.emitWB(st.l)
	case stubTail:
		a.Bind(st.l)
		g.mark("cold tail call")
		g.exitTail(st.v)
	case stubRefRoom:
		// A callee that holds references, whose Ctx.Frames is past any
		// chunk's end: its numeric slots' room past L plus code bytes,
		// and its reference slots' past R plus off, by Ctx.RFrames. The
		// callee's index is in x0.
		a.Bind(st.l)
		g.mark("cold reference room")
		a.FMovFromF(s0, x0)
		a.Load(s1, rCtx, offRFrames)
		a.AddLsl(s1, s1, s0, 3)
		a.Load(s1, s1, 0)
		a.MovRR32(s0, s1)
		a.Op(arm64.Add, s0, s0, rL, true)
		if st.code != 0 {
			a.AddImm(s0, s0, int64(st.code), true)
		}
		a.FMovToF(x1, s1)
		a.Load(s1, rCtx, offChunkEnd)
		a.Cmp(s0, s1, true)
		a.BCond(arm64.HI, st.slow)
		a.FMovFromF(s1, x1)
		a.ShiftImm(arm64.Lsr, s1, s1, 32, true)
		a.Load(s0, rCtx, offR)
		a.Op(arm64.Add, s1, s1, s0, true)
		if st.off != 0 {
			a.AddImm(s1, s1, int64(st.off), true)
		}
		a.Load(s0, rCtx, offRChunkEnd)
		a.Cmp(s1, s0, true)
		a.BCond(arm64.HI, st.slow)
		a.B(st.to)
	}
}

// pollExit returns the label of the function's poll exit, which its sites
// go to with where to go on in s0.
func (g *gen) pollExit() arm64.Label {
	if !g.hasPoll {
		g.hasPoll = true
		g.poll = g.a.NewLabel()
		g.ool = append(g.ool, stub{kind: stubPollExit, l: g.poll})
	}
	return g.poll
}

func (g *gen) storeCtx(off int32, v uint64) {
	if v == 0 {
		g.a.Store(rCtx, off, arm64.ZR)
		return
	}
	g.a.MovImm(s0, v)
	g.a.Store(rCtx, off, s0)
}

// exit returns to the engine for kind, with args, to resume at resume,
// where the registers the code keeps are loaded again: every value live
// past it is in its slot.
func (g *gen) exit(kind uint64, resume arm64.Label, args ...uint64) {
	a := g.a
	a.Store(rCtx, offL, rL)
	a.Store(rCtx, offPoll, rPoll)
	a.Store(rCtx, offRSP, rRS)
	g.storeCtx(offExit, kind)
	for i, v := range args {
		g.storeCtx(offArg0+int32(8*i), v)
	}
	g.storeCtx(offExitFn, uint64(g.fn))
	a.Adr(s0, resume)
	a.Store(rCtx, offExitAddr, s0)
	a.Ret()
	a.Bind(resume)
	g.loadKept()
}

// saveAll and restoreAll keep every register the allocator gave out in the
// context's save area across an exit that is not a call: a poll's. A
// function with v128 values keeps its V registers whole (Ctx.SaveV).
func (g *gen) saveAll() {
	a := g.a
	for i, r := range gprs {
		if g.usedG&(1<<r) != 0 {
			a.Store(rCtx, offSave+int32(8*i), arm64.Reg(r))
		}
	}
	for i, x := range fprs {
		switch {
		case g.usedX&(1<<x) == 0:
		case g.v128:
			a.StoreQ(rCtx, offSaveV+16*int32(x), arm64.FReg(x))
		default:
			a.StoreF(rCtx, offSave+int32(8*(len(gprs)+i)), arm64.FReg(x))
		}
	}
}

func (g *gen) restoreAll() {
	a := g.a
	for i, r := range gprs {
		if g.usedG&(1<<r) != 0 {
			a.Load(arm64.Reg(r), rCtx, offSave+int32(8*i))
		}
	}
	for i, x := range fprs {
		switch {
		case g.usedX&(1<<x) == 0:
		case g.v128:
			a.LoadQ(arm64.FReg(x), rCtx, offSaveV+16*int32(x))
		default:
			a.LoadF(arm64.FReg(x), rCtx, offSave+int32(8*(len(gprs)+i)))
		}
	}
}

// jump goes from block i to its successor s: a back edge counts down the
// poll counter.
func (g *gen) jump(i int, s *Block) {
	a := g.a
	j := int(g.index[s.ID])
	if j > i {
		if j != i+1 {
			a.B(g.labels[j])
		}
		return
	}
	l := g.pollSite(j)
	g.mark("poll %v", s)
	a.SubsImm(rPoll, rPoll, 1, true)
	a.BCond(arm64.GT, g.labels[j])
	a.B(l)
}

// allocExit returns the function's exit for an empty pool of new structs,
// made after the body with its first use.
func (g *gen) allocExit() arm64.Label {
	if !g.hasAlloc {
		g.hasAlloc = true
		g.alloc = g.a.NewLabel()
		g.ool = append(g.ool, stub{kind: stubAllocExit, l: g.alloc})
	}
	return g.alloc
}

// pollSite is the out-of-line code a back edge to block j goes to when the
// poll counter runs out: the function's poll exit, to go on at j.
func (g *gen) pollSite(j int) arm64.Label {
	l, ok := g.polls.get(uint64(j))
	if !ok {
		l = g.a.NewLabel()
		g.ool = append(g.ool, stub{kind: stubPoll, l: l, to: g.labels[j]})
		g.polls.set(uint64(j), l)
	}
	return l
}

// edgeMoves moves the arguments of s's phis for the edge from b.
func (g *gen) edgeMoves(b, s *Block) {
	if len(s.Phis) == 0 {
		return
	}
	g.mark("edge %v->%v", b, s)
	moves := g.phiMoves(g.moves[:0], b, s)
	g.parallel(moves)
	g.moves = moves[:0]
}

// block lowers block b, the i-th.
func (g *gen) block(i int, b *Block) {
	if b.Pad != nil {
		g.padEntry(b)
	}
	for _, p := range b.Phis {
		if r := g.ra.Reg[p.vreg]; r >= 0 && !g.inSlot(p) {
			g.done(p, int(r))
		}
	}
	for j := 0; j < len(b.Values); j++ {
		v := b.Values[j]
		if v.Op == OpConst || v.Op == OpResult || g.fused[v.ID] {
			continue
		}
		if g.marking {
			g.mark("%v %v %v", v, v.Op, v.Type)
		}
		g.value(v, b.Values[j+1:])
	}
	a := g.a
	if g.marking {
		g.mark("%v end %v", b, [...]string{"Plain", "If", "Switch", "Return", "Trap", "Tail", "Invoke"}[b.Kind])
	}
	switch b.Kind {
	case BPlain, BInvoke:
		g.edgeMoves(b, b.Succs[0])
		g.jump(i, b.Succs[0])
	case BIf:
		t, e := int(g.index[b.Succs[0].ID]), int(g.index[b.Succs[1].ID])
		if !g.fused[b.Control.ID] {
			// A test of the condition's register, or of a constant's.
			r := g.intoG(b.Control, s0)
			switch {
			case t == i+1:
				a.Cbz(r, g.labels[e], false)
			case e == i+1:
				a.Cbnz(r, g.labels[t], false)
			default:
				a.Cbnz(r, g.labels[t], false)
				a.B(g.labels[e])
			}
			return
		}
		cond := g.compare(b.Control)
		switch {
		case t == i+1:
			a.BCond(cond.Invert(), g.labels[e])
		case e == i+1:
			a.BCond(cond, g.labels[t])
		default:
			a.BCond(cond, g.labels[t])
			a.B(g.labels[e])
		}
	case BSwitch:
		if len(b.Succs) == 1 {
			g.edgeMoves(b, b.Succs[0])
			g.jump(i, b.Succs[0])
			return
		}
		g.switchTable(i, b)
	case BReturn:
		moves := g.moves[:0]
		for k, r := range b.Results {
			moves = append(moves, pmove{dst: loc{kind: lSlot, slot: g.f.ResultSlots[k]}, src: g.locOf(r), t: r.Type})
		}
		g.parallel(moves)
		g.moves = moves[:0]
		a.AddImm(rRS, rRS, -wjit.RSEntryWords*8, true)
		a.Load(rL, rRS, 8)
		a.Load(s0, rRS, 0)
		a.Br(s0)
	case BTrap:
		a.B(g.trap(wjit.TrapUnreachable, b.At))
	case BTail:
		// The tail call, its last value, left the function.
	}
}

// switchTable jumps through a table of branches: to each target, or for
// a back edge to its poll's count.
func (g *gen) switchTable(i int, b *Block) {
	a := g.a
	n := len(b.Table) - 1
	g.movG(s0, b.Control)
	a.MovImm(s1, uint64(n))
	a.Cmp(s0, s1, false)
	a.Csel(s0, s0, s1, arm64.LO, false)
	table := a.NewLabel()
	a.Adr(s1, table)
	a.AddLsl(s1, s1, s0, 2)
	a.Br(s1)
	a.Bind(table)
	var backs []arm64.Label
	for _, k := range b.Table {
		s := b.Succs[k]
		if j := int(g.index[s.ID]); j > i {
			a.B(g.labels[j])
			continue
		}
		if backs == nil {
			backs = make([]arm64.Label, len(b.Succs))
			for k := range backs {
				backs[k] = -1
			}
		}
		if backs[k] < 0 {
			backs[k] = a.NewLabel()
		}
		a.B(backs[k])
	}
	for k, l := range backs {
		if l >= 0 {
			a.Bind(l)
			g.jump(i, b.Succs[k])
		}
	}
}

// compare sets the flags for integer comparison v, returning the
// condition under which it holds.
func (g *gen) compare(v *Value) arm64.Cond {
	a := g.a
	x := v.Args[0]
	wide := x.Type == TI64
	if v.Op == OpEqz {
		a.CmpImm(g.intoG(x, s0), 0, wide)
		return arm64.EQ
	}
	y := v.Args[1]
	cond := cmpConds[v.Op]
	if x.Op == OpConst && y.Op != OpConst {
		x, y = y, x
		cond = mirror(cond)
	}
	r := g.intoG(x, s0)
	if l := g.locOf(y); l.kind == lConst {
		a.CmpImm(r, int64(constBits(l.c, y.Type)), wide)
	} else {
		a.Cmp(r, g.readG(y, s1), wide)
	}
	return cond
}

var cmpConds = [numOps]arm64.Cond{
	OpEq: arm64.EQ, OpNe: arm64.NE, OpLtS: arm64.LT, OpLtU: arm64.LO, OpGtS: arm64.GT,
	OpGtU: arm64.HI, OpLeS: arm64.LE, OpLeU: arm64.LS, OpGeS: arm64.GE, OpGeU: arm64.HS,
}

// mirror is the condition of a comparison with its operands swapped.
func mirror(cond arm64.Cond) arm64.Cond {
	switch cond {
	case arm64.LT:
		return arm64.GT
	case arm64.GT:
		return arm64.LT
	case arm64.LE:
		return arm64.GE
	case arm64.GE:
		return arm64.LE
	case arm64.LO:
		return arm64.HI
	case arm64.HI:
		return arm64.LO
	case arm64.LS:
		return arm64.HS
	case arm64.HS:
		return arm64.LS
	}
	return cond
}

// floatConds are the float comparisons' conditions after FCMP, false for
// unordered operands but for ne: MI for lt and LS for le, and GT and GE,
// which an unordered result's V flag makes false.
var floatConds = [numOps]arm64.Cond{
	OpFEq: arm64.EQ, OpFNe: arm64.NE, OpFLt: arm64.MI, OpFGt: arm64.GT, OpFLe: arm64.LS, OpFGe: arm64.GE,
}

var (
	alus   = [numOps]arm64.ALU{OpAdd: arm64.Add, OpSub: arm64.Sub, OpAnd: arm64.And, OpOr: arm64.Orr, OpXor: arm64.Eor}
	shifts = [numOps]arm64.Shift{OpShl: arm64.Lsl, OpShrS: arm64.Asr, OpShrU: arm64.Lsr, OpRotl: arm64.Ror, OpRotr: arm64.Ror}
)

// The float operations' instructions and FRINT's roundings of ceil, floor,
// trunc and nearest, which quiet a NaN as the interpreter's a + a does.
var (
	floatOps   = [numOps]arm64.FOp{OpFAdd: arm64.FAdd, OpFSub: arm64.FSub, OpFMul: arm64.FMul, OpFDiv: arm64.FDiv, OpFMin: arm64.FMin, OpFMax: arm64.FMax}
	roundModes = [numOps]arm64.FRound{OpFCeil: arm64.FRintP, OpFFloor: arm64.FRintM, OpFTrunc: arm64.FRintZ, OpFNearest: arm64.FRintN}
)

// value lowers v; rest are the values after it in its block.
func (g *gen) value(v *Value, rest []*Value) {
	a := g.a
	wide := v.Type == TI64
	switch v.Op {
	case OpParam:
		if r := g.ra.Reg[v.vreg]; r >= 0 {
			dst := loc{kind: lGPR, r: int(r)}
			if v.Type.InFloatReg() {
				dst.kind = lXMM
			}
			g.move(dst, loc{kind: lSlot, slot: int32(v.Aux)}, v.Type)
		}
	case OpAdd, OpSub, OpAnd, OpOr, OpXor:
		g.alu(v, alus[v.Op])
	case OpMul:
		t := g.defG(v)
		rx, ry := g.readG(v.Args[0], s0), g.readG(v.Args[1], s1)
		if wide {
			a.Mul(t, rx, ry)
		} else {
			a.MulW(t, rx, ry)
		}
		g.done(v, int(t))
	case OpShl, OpShrS, OpShrU, OpRotl, OpRotr:
		g.shift(v, shifts[v.Op])
	case OpDivS, OpDivU, OpRemS, OpRemU:
		g.div(v)
	case OpClz, OpCtz, OpPopcnt:
		t := g.defG(v)
		src := g.intoG(v.Args[0], s0)
		switch v.Op {
		case OpClz:
			a.Clz(t, src, wide)
		case OpCtz:
			a.Rbit(t, src, wide)
			a.Clz(t, t, wide)
		default:
			// The count of each byte's bits, summed; an i32's upper half
			// is clear.
			a.FMovToF(x0, src)
			a.Cnt8B(x0, x0)
			a.Addv8B(x0, x0)
			a.FMovFromS(t, x0)
		}
		g.done(v, int(t))
	case OpEqz, OpEq, OpNe, OpLtS, OpLtU, OpGtS, OpGtU, OpLeS, OpLeU, OpGeS, OpGeU:
		cond := g.compare(v)
		t := g.defG(v)
		a.Cset(t, cond)
		g.done(v, int(t))
	case OpWrap:
		t := g.defG(v)
		switch l := g.locOf(v.Args[0]); l.kind {
		case lGPR:
			a.MovRR32(t, arm64.Reg(l.r))
		case lSlot:
			a.LoadU32(t, rL, slotOff(l.slot))
		case lConst:
			a.MovImm(t, uint64(uint32(l.c)))
		}
		g.done(v, int(t))
	case OpExtendU:
		t := g.defG(v)
		g.move(loc{kind: lGPR, r: int(t)}, g.locOf(v.Args[0]), TI32)
		g.done(v, int(t))
	case OpExtendS, OpExtend8S, OpExtend16S, OpExtend32S:
		t := g.defG(v)
		src := g.intoG(v.Args[0], s0)
		switch v.Op {
		case OpExtendS, OpExtend32S:
			a.Sxtw(t, src)
		case OpExtend8S:
			a.Sxtb(t, src, wide)
		default:
			a.Sxth(t, src, wide)
		}
		g.done(v, int(t))
	case OpFAdd, OpFSub, OpFMul, OpFDiv, OpFMin, OpFMax:
		// The first operand the first source, for the interpreter's NaN
		// (pick32, pick64). min and max are FMIN and FMAX: a NaN operand
		// gives the NaN an addition of the operands does, and -0 is below
		// +0.
		t := g.defX(v)
		xa, xb := g.intoX(v.Args[0], x0), g.intoX(v.Args[1], x1)
		if v.Type == TF32 {
			a.FArithS(floatOps[v.Op], t, xa, xb)
		} else {
			a.FArith(floatOps[v.Op], t, xa, xb)
		}
		g.done(v, int(t))
	case OpFCopysign:
		g.copySign(v)
	case OpFAbs, OpFNeg:
		// Bit operations in a general register, which leave a NaN's
		// payload as it is.
		t := g.defX(v)
		single := v.Type == TF32
		g.move(loc{kind: lGPR, r: int(s0)}, g.locOf(v.Args[0]), v.Type)
		sign := uint64(1) << 63
		if single {
			sign = 1 << 31
		}
		if v.Op == OpFAbs {
			a.LogicImm(arm64.And, s0, s0, sign-1, !single)
		} else {
			a.LogicImm(arm64.Eor, s0, s0, sign, !single)
		}
		if single {
			a.FMovToS(t, s0)
		} else {
			a.FMovToF(t, s0)
		}
		g.done(v, int(t))
	case OpFSqrt, OpFCeil, OpFFloor, OpFTrunc, OpFNearest, OpPromote, OpDemote:
		t := g.defX(v)
		src := g.intoX(v.Args[0], x0)
		single := v.Type == TF32
		switch v.Op {
		case OpFSqrt:
			if single {
				a.FSqrtS(t, src)
			} else {
				a.FSqrt(t, src)
			}
		case OpPromote:
			a.FCvtFromSingle(t, src)
		case OpDemote:
			a.FCvtToSingle(t, src)
		default:
			if single {
				a.FRintS(roundModes[v.Op], t, src)
			} else {
				a.FRint(roundModes[v.Op], t, src)
			}
		}
		g.done(v, int(t))
	case OpFEq, OpFNe, OpFLt, OpFGt, OpFLe, OpFGe:
		xa, xb := g.intoX(v.Args[0], x0), g.intoX(v.Args[1], x1)
		if v.Args[0].Type == TF32 {
			a.FCmpS(xa, xb)
		} else {
			a.FCmp(xa, xb)
		}
		t := g.defG(v)
		a.Cset(t, floatConds[v.Op])
		g.done(v, int(t))
	case OpReinterpret:
		if v.Type.IsFloat() {
			t := g.defX(v)
			g.move(loc{kind: lXMM, r: int(t)}, g.locOf(v.Args[0]), v.Type)
			g.done(v, int(t))
		} else {
			t := g.defG(v)
			g.move(loc{kind: lGPR, r: int(t)}, g.locOf(v.Args[0]), v.Type)
			g.done(v, int(t))
		}
	case OpConvertS, OpConvertU:
		// SCVTF and UCVTF round once, the 64-bit unsigned conversions
		// included.
		t := g.defX(v)
		x := v.Args[0]
		a.Cvtf(t, g.intoG(x, s0), v.Op == OpConvertS, x.Type == TI64, v.Type == TF32)
		g.done(v, int(t))
	case OpTruncS, OpTruncU:
		g.truncate(v)
	case OpTruncSatS, OpTruncSatU:
		// FCVTZS and FCVTZU clamp to the result's range and give 0 for
		// NaN, as the interpreter's do.
		t := g.defG(v)
		x := v.Args[0]
		a.Fcvtz(t, g.intoX(x, x0), v.Op == OpTruncSatS, wide, x.Type == TF32)
		g.done(v, int(t))
	case OpSelect:
		g.selectOp(v)
	case OpLoad:
		g.load(v)
	case OpStore:
		g.store(v)
	case OpGlobalAddr:
		t := g.defG(v)
		a.Load(t, rCtx, offGlobals)
		a.Load(t, t, int32(8*v.Aux))
		g.done(v, int(t))
	case OpGlobalGet:
		p := g.intoG(v.Args[0], s0)
		if v.Type.InFloatReg() {
			t := g.defX(v)
			switch v.Type {
			case TF32:
				a.LoadS(t, p, 0)
			case TV128:
				a.LoadQ(t, p, 0)
			default:
				a.LoadF(t, p, 0)
			}
			g.done(v, int(t))
		} else {
			t := g.defG(v)
			if v.Type == TI32 {
				a.LoadU32(t, p, 0)
			} else {
				a.Load(t, p, 0)
			}
			g.done(v, int(t))
		}
	case OpGlobalSet:
		// The whole word: an i32's and an f32's upper half clear. The
		// value first, whose constant may take s0.
		if x := v.Args[1]; x.Type == TV128 {
			xv := g.intoX(x, x0)
			a.StoreQ(g.intoG(v.Args[0], s0), 0, xv)
		} else if x.Type.IsFloat() {
			xv := g.intoX(x, x0)
			a.StoreF(g.intoG(v.Args[0], s0), 0, xv)
		} else {
			rv := g.readG(x, s1)
			a.Store(g.intoG(v.Args[0], s0), 0, rv)
		}
	case OpCall:
		g.call(v, rest)
	case OpCallIndirect, OpCallRef:
		g.callIndirect(v, rest)
	case OpRef:
		g.refOp(v)
	case OpTail, OpTailIndirect, OpTailRef:
		g.tail(v)
	case OpSIMD:
		g.simd(v)
	case OpGoOp:
		if v.BulkInCode() {
			g.bulk(v)
		} else {
			g.callGo(v, rest)
		}
	case OpAtomicLoad, OpAtomicStore, OpAtomicAdd, OpAtomicSub, OpAtomicAnd, OpAtomicOr, OpAtomicXor, OpAtomicXchg,
		OpAtomicCmpxchg, OpFence:
		g.atomic(v)
	case OpExnVal:
		g.exnVal(v)
	default:
		panic("wasm opt: no lowering of " + v.Op.String())
	}
}

// alu lowers add, sub, and, or and xor: a constant second operand is an
// immediate where the instruction has one.
func (g *gen) alu(v *Value, op arm64.ALU) {
	a := g.a
	wide := v.Type == TI64
	x, y := v.Args[0], v.Args[1]
	if x.Op == OpConst && y.Op != OpConst && v.Commutes() {
		x, y = y, x
	}
	t := g.defG(v)
	rx := g.intoG(x, s0)
	if l := g.locOf(y); l.kind == lConst {
		c := constBits(l.c, y.Type)
		switch op {
		case arm64.Add:
			a.AddImm(t, rx, int64(c), wide)
			g.done(v, int(t))
			return
		case arm64.Sub:
			a.AddImm(t, rx, -int64(c), wide)
			g.done(v, int(t))
			return
		}
		if a.LogicImm(op, t, rx, c, wide) {
			g.done(v, int(t))
			return
		}
	}
	a.Op(op, t, rx, g.readG(y, s1), wide)
	g.done(v, int(t))
}

// shift lowers the shifts and rotations, by a constant or by a register's
// count, which the instructions take modulo the width. A left rotation is
// a right one by the count negated.
func (g *gen) shift(v *Value, op arm64.Shift) {
	a := g.a
	wide := v.Type == TI64
	x, y := v.Args[0], v.Args[1]
	t := g.defG(v)
	rx := g.intoG(x, s0)
	if y.Op == OpConst {
		n := y.Aux & shiftMask(v.Type)
		if v.Op == OpRotl {
			n = -n & shiftMask(v.Type)
		}
		a.ShiftImm(op, t, rx, uint8(n), wide)
		g.done(v, int(t))
		return
	}
	ry := g.intoG(y, s1)
	if v.Op == OpRotl {
		a.Neg(s1, ry, false)
		ry = s1
	}
	a.ShiftReg(op, t, rx, ry, wide)
	g.done(v, int(t))
}

// div lowers division and remainder. The division instructions never
// fault: a zero divisor traps first, as does the signed division of the
// least integer by -1, whose remainder, the quotient's product taken from
// the dividend, is 0 as the instruction leaves it.
func (g *gen) div(v *Value) {
	a := g.a
	wide := v.Type == TI64
	x, y := v.Args[0], v.Args[1]
	signed := v.Op == OpDivS || v.Op == OpRemS
	rem := v.Op == OpRemS || v.Op == OpRemU
	rx := g.intoG(x, s0)
	ry := g.intoG(y, s1)
	c := y.Aux
	isConst := y.Op == OpConst
	if !isConst || c == 0 || (!wide && uint32(c) == 0) {
		code := uint64(wjit.TrapDivByZero)
		if rem {
			code = wjit.TrapRemByZero
		}
		a.Cbz(ry, g.trap(code, v.At), wide)
	}
	minusOne := !isConst || (wide && c == math.MaxUint64) || (!wide && uint32(c) == math.MaxUint32)
	if signed && !rem && minusOne {
		ok := a.NewLabel()
		a.CmpImm(ry, -1, wide)
		a.BCond(arm64.NE, ok)
		if wide {
			a.CmpImm(rx, math.MinInt64, true)
		} else {
			a.CmpImm(rx, math.MinInt32, false)
		}
		a.BCond(arm64.EQ, g.trap(wjit.TrapDivUnrepresentable, v.At))
		a.Bind(ok)
	}
	t := g.defG(v)
	q := t
	if rem {
		q = rQuot
	}
	switch {
	case signed && wide:
		a.Sdiv(q, rx, ry)
	case signed:
		a.SdivW(q, rx, ry)
	case wide:
		a.Udiv(q, rx, ry)
	default:
		a.UdivW(q, rx, ry)
	}
	if rem {
		if wide {
			a.Msub(t, q, ry, rx)
		} else {
			a.MsubW(t, q, ry, rx)
		}
	}
	g.done(v, int(t))
}

// copySign lowers copysign in general registers: the first operand's
// magnitude, the second's sign.
func (g *gen) copySign(v *Value) {
	a := g.a
	x, y := v.Args[0], v.Args[1]
	t := g.defX(v)
	g.move(loc{kind: lGPR, r: int(s0)}, g.locOf(x), v.Type)
	g.move(loc{kind: lGPR, r: int(s1)}, g.locOf(y), v.Type)
	wide := v.Type == TF64
	sign := uint64(1) << 31
	if wide {
		sign = 1 << 63
	}
	a.LogicImm(arm64.And, s0, s0, sign-1, wide)
	a.LogicImm(arm64.And, s1, s1, sign, wide)
	a.Op(arm64.Orr, s0, s0, s1, wide)
	if wide {
		a.FMovToF(t, s0)
	} else {
		a.FMovToS(t, s0)
	}
	g.done(v, int(t))
}

// truncate lowers the trapping truncations: of a NaN, or a value out of
// the result's range, by the interpreter's bounds, each exact in the
// operand's type. In range, FCVTZS and FCVTZU truncate exactly.
func (g *gen) truncate(v *Value) {
	a := g.a
	arg := v.Args[0]
	single := arg.Type == TF32
	unsigned := v.Op == OpTruncU
	var lo, hi float64
	loIn := false
	switch {
	case v.Type == TI32 && !unsigned && single:
		lo, hi, loIn = -2147483648.0, 2147483648.0, true
	case v.Type == TI32 && !unsigned:
		lo, hi = -2147483649.0, 2147483648.0
	case v.Type == TI32:
		lo, hi = -1, 4294967296.0
	case !unsigned:
		lo, hi, loIn = -9223372036854775808.0, 9223372036854775808.0, true
	default:
		lo, hi = -1, 18446744073709551616.0
	}
	x := g.intoX(arg, x0)
	trap := g.trap(wjit.TrapFloatUnrepresentable, v.At)
	cmp := func(bound float64) {
		bits, t := math.Float64bits(bound), TF64
		if single {
			bits, t = uint64(math.Float32bits(float32(bound))), TF32
		}
		g.move(loc{kind: lXMM, r: int(x1)}, loc{kind: lConst, c: bits}, t)
		if single {
			a.FCmpS(x, x1)
		} else {
			a.FCmp(x, x1)
		}
	}
	// Below lo, or unordered: LT and LE hold for an unordered result.
	cmp(lo)
	if loIn {
		a.BCond(arm64.LT, trap)
	} else {
		a.BCond(arm64.LE, trap)
	}
	cmp(hi)
	a.BCond(arm64.GE, trap)
	t := g.defG(v)
	a.Fcvtz(t, x, !unsigned, v.Type == TI64, single)
	g.done(v, int(t))
}

// selectOp lowers select: the first operand if the condition is true.
func (g *gen) selectOp(v *Value) {
	a := g.a
	x, y, c := v.Args[0], v.Args[1], v.Args[2]
	a.CmpImm(g.intoG(c, s0), 0, false)
	// From here nothing may change the flags: moves and loads do not.
	if v.Type == TV128 {
		// No vector CSEL: a branch around a move.
		t := g.defX(v)
		skip := a.NewLabel()
		switch {
		case g.isX(x, t):
			a.BCond(arm64.NE, skip)
			g.movX(t, y)
		case g.isX(y, t):
			a.BCond(arm64.EQ, skip)
			g.movX(t, x)
		default:
			g.movX(t, y)
			a.BCond(arm64.EQ, skip)
			g.movX(t, x)
		}
		a.Bind(skip)
		g.done(v, int(t))
		return
	}
	if v.Type.IsFloat() {
		t := g.defX(v)
		a.FCsel(t, g.intoX(x, x0), g.intoX(y, x1), arm64.NE, v.Type == TF32)
		g.done(v, int(t))
		return
	}
	t := g.defG(v)
	a.Csel(t, g.readG(x, s0), g.readG(y, s1), arm64.NE, v.Type == TI64)
	g.done(v, int(t))
}

// address returns the register holding a plain access's address plus its
// offset, to add to memory 0's base, checking the access's end against
// memory 0's size unless the access needs no check. An i32 address is
// zero-extended and the offset added in 64 bits, which nothing wraps.
func (g *gen) address(v *Value) arm64.Reg {
	a := g.a
	addr := v.Args[0]
	off := int64(v.Aux)
	size := int64(v.Size)
	if addr.Op == OpConst {
		ea := int64(uint32(addr.Aux)) + off
		if !v.NoCheck {
			a.MovImm(s0, uint64(ea+size))
			a.Cmp(s0, rMemSize, true)
			a.BCond(arm64.HI, g.trap(wjit.TrapMemOutOfBounds, v.At))
		}
		a.MovImm(s1, uint64(ea))
		return s1
	}
	r := g.intoG(addr, s1)
	if off != 0 {
		a.AddImm(s1, r, off, true)
		r = s1
	}
	if !v.NoCheck {
		a.AddImm(s0, r, size, true)
		a.Cmp(s0, rMemSize, true)
		a.BCond(arm64.HI, g.trap(wjit.TrapMemOutOfBounds, v.At))
	}
	return r
}

func (g *gen) load(v *Value) {
	if !plainAccess(v) {
		g.loadAny(v)
		return
	}
	a := g.a
	r := g.address(v)
	if v.Type.InFloatReg() {
		t := g.defX(v)
		switch v.Type {
		case TF32:
			a.LoadSIdx(t, rMem, r)
		case TV128:
			a.LoadQIdx(t, rMem, r)
		default:
			a.LoadFIdx(t, rMem, r)
		}
		g.done(v, int(t))
		return
	}
	t := g.defG(v)
	wide := v.Type == TI64
	switch {
	case v.Size == 8:
		a.LoadIdx(t, rMem, r, false)
	case v.Size == 4 && v.Signed:
		a.LoadS32Idx(t, rMem, r)
	case v.Size == 4:
		a.LoadU32Idx(t, rMem, r)
	case v.Size == 2 && v.Signed:
		a.LoadS16Idx(t, rMem, r, wide)
	case v.Size == 2:
		a.LoadU16Idx(t, rMem, r)
	case v.Signed:
		a.LoadS8Idx(t, rMem, r, wide)
	default:
		a.LoadU8Idx(t, rMem, r)
	}
	g.done(v, int(t))
}

func (g *gen) store(v *Value) {
	if !plainAccess(v) {
		g.storeAny(v)
		return
	}
	a := g.a
	r := g.address(v)
	x := v.Args[1]
	if x.Type.InFloatReg() {
		xv := g.intoX(x, x0)
		switch v.Size {
		case 4:
			a.StoreSIdx(rMem, r, xv)
		case 16:
			a.StoreQIdx(rMem, r, xv)
		default:
			a.StoreFIdx(rMem, r, xv)
		}
		return
	}
	rv := g.readG(x, s0)
	switch v.Size {
	case 1:
		a.Store8Idx(rMem, r, rv)
	case 2:
		a.Store16Idx(rMem, r, rv)
	case 4:
		a.Store32Idx(rMem, r, rv)
	default:
		a.StoreIdx(rMem, r, rv)
	}
}

// takeResults moves a call's results from the slots from base, where the
// callee left them, to their values' locations.
func (g *gen) takeResults(res []*Value, base int32) {
	for _, r := range res {
		src := loc{kind: lSlot, slot: base + 2*int32(r.Aux)}
		if base >= int32(g.req.FrameSlots) {
			// The callee's frame, its numeric results one slot apart.
			src.slot = base + int32(r.Aux2)
		}
		dst := g.locOf(r)
		g.move(dst, src, r.Type)
		if dst.kind != lSlot {
			g.done(r, dst.r)
		}
	}
}

// reload loads the values a call clobbered from their slots.
func (g *gen) reload(v *Value) {
	key := g.inst[v.vreg]
	if key[0] < 0 {
		return
	}
	across := g.ra.Across(int(key[0]), int(key[1]))
	if g.marking && len(across) > 0 {
		g.mark("%v reload", v)
	}
	for _, r := range across {
		w := g.byVreg[r]
		g.move(g.locOf(w), loc{kind: lSlot, slot: g.ra.Slot[r]}, w.Type)
	}
}

// stage stores a call's arguments into the slots from base, two apart,
// where Go reads them.
func (g *gen) stage(args []*Value, base int) {
	for i, x := range args {
		g.move(loc{kind: lSlot, slot: int32(base + 2*i)}, g.locOf(x), x.Type)
	}
}

// pushReturn pushes the return stack's entry of a native call -- the
// address ret, the caller's L, and site in s1, or Site(idx, at) -- and
// moves L to the callee's frame.
func (g *gen) pushReturn(ret arm64.Label, site uint64, siteInS1 bool) {
	a := g.a
	a.Adr(s0, ret)
	a.Store(rRS, 0, s0)
	a.Store(rRS, 8, rL)
	if !siteInS1 {
		a.MovImm(s1, site)
	}
	a.Store(rRS, 16, s1)
	a.AddImm(rRS, rRS, wjit.RSEntryWords*8, true)
	a.AddImm(rL, rL, int64(8*g.req.FrameSlots), true)
}

// call lowers a call of a function: natively where the callee has native
// code and there is room, else through Go.
func (g *gen) call(v *Value, rest []*Value) {
	a := g.a
	idx := uint32(v.Aux)
	m := g.req.Module
	res := results(v, rest)
	after := a.NewLabel()
	if m.Functions[idx].Imported {
		g.exitCall(v, res)
		g.reload(v)
		return
	}
	start, slow, poll := a.NewLabel(), a.NewLabel(), a.NewLabel()
	a.Bind(start)
	a.SubsImm(rPoll, rPoll, 1, true)
	a.BCond(arm64.LE, poll)
	// The callee's native entry, its frame's room past this one's, and
	// an entry of the return stack.
	a.Load(s0, rCtx, offLinks)
	a.Load(s0, s0, int32(8*idx))
	a.Cbz(s0, slow, true)
	a.AddImm(s0, rL, int64(8*(g.req.FrameSlots+g.req.FrameSlotsOf(idx))), true)
	a.Load(s1, rCtx, offChunkEnd)
	a.Cmp(s0, s1, true)
	a.BCond(arm64.HI, slow)
	if need := int64(16 * g.req.RefSlotsOf(idx)); need > 0 {
		a.Load(s0, rCtx, offR)
		a.AddImm(s0, s0, int64(g.adv())+need, true)
		a.Load(s1, rCtx, offRChunkEnd)
		a.Cmp(s0, s1, true)
		a.BCond(arm64.HI, slow)
	}
	a.Load(s1, rCtx, offRSEnd)
	a.Cmp(rRS, s1, true)
	a.BCond(arm64.HS, slow)
	if g.sigHasRefs(v) {
		g.refArgs(v, g.adv())
	}
	// The arguments, into the callee's parameters: one slot each, a
	// v128 two.
	slot := int32(g.req.FrameSlots)
	for _, x := range v.Args {
		g.move(loc{kind: lSlot, slot: slot}, g.locOf(x), x.Type)
		slot += x.Type.Slots()
	}
	g.advanceR()
	ret := a.NewLabel()
	g.pushReturn(ret, wjit.Site(idx, v.At), false)
	a.Load(s0, rCtx, offLinks)
	a.Load(s0, s0, int32(8*idx))
	a.Br(s0)
	a.Bind(ret)
	g.restoreR()
	if g.sigHasRefs(v) {
		g.refResults(v, g.adv())
	}
	g.takeResults(res, int32(g.req.FrameSlots))
	a.Bind(after)
	g.reload(v)
	g.ool = append(g.ool, stub{kind: stubCall, l: poll, start: start, slow: slow, after: after, v: v, res: res})
}

// callGo lowers what Go runs for the code: call_indirect, and the
// instructions it leaves to Go.
func (g *gen) callGo(v *Value, rest []*Value) {
	a := g.a
	if v.Op == OpCallIndirect || v.Op == OpCallRef {
		g.exitCall(v, results(v, rest))
		g.reload(v)
		return
	}
	g.stageGo(v)
	resume := a.NewLabel()
	base := uint64(g.argBase)
	{
		op := opcode.Opcode(v.Aux)
		x, y := g.imms(v)
		g.exit(wjit.ExitOp, resume, uint64(op), x, y, base, uint64(v.At))
		if v.Type != TNone {
			dst := g.locOf(v)
			g.move(dst, loc{kind: lSlot, slot: int32(g.argBase)}, v.Type)
			if dst.kind != lSlot {
				g.done(v, dst.r)
			}
		}
	}
	g.reload(v)
}

// callIndirect lowers call_indirect and call_ref: natively where the
// table's element or the reference is a function of the call's type
// exactly, defined by the instance the code runs for, with native code and
// room for its frame; else through Go, which finds the callee, or traps,
// as the interpreter does. A null reference traps here. A call of a type
// with references goes through Go: its callee holds references, whose
// frames Ctx.Frames says no call through a table makes natively.
func (g *gen) callIndirect(v *Value, rest []*Value) {
	m := g.req.Module
	typeIndex := uint32(v.Aux)
	lay := g.req.Layout
	ref := v.Op == OpCallRef
	if ref {
		g.nullTrap(v)
	}
	if lay == nil || int(typeIndex) >= len(m.Canonical) || v.At > math.MaxInt32 || g.sigHasRefs(v) ||
		(!ref && m.Tables[v.Aux2].Limits.Is64) {
		g.callGo(v, rest)
		return
	}
	a := g.a
	res := results(v, rest)
	args := v.Args
	if !ref {
		args = v.Args[:len(v.Args)-1]
	}
	start, slow, poll, after := a.NewLabel(), a.NewLabel(), a.NewLabel(), a.NewLabel()
	a.Bind(start)
	a.SubsImm(rPoll, rPoll, 1, true)
	a.BCond(arm64.LE, poll)
	g.indirectCallee(v, int64(8*g.req.FrameSlots), g.adv(), slow)
	a.Load(s1, rCtx, offRSEnd)
	a.Cmp(rRS, s1, true)
	a.BCond(arm64.HS, slow)
	// The callee's index waits in x0 while the arguments go into its
	// parameters, one slot each, the moves taking s0.
	slot := int32(g.req.FrameSlots)
	for _, x := range args {
		g.move(loc{kind: lSlot, slot: slot}, g.locOf(x), x.Type)
		slot += x.Type.Slots()
	}
	g.advanceR()
	ret := a.NewLabel()
	a.FMovFromF(s1, x0)
	a.ShiftImm(arm64.Lsl, s1, s1, 32, true)
	if v.At != 0 {
		a.MovImm(s0, uint64(v.At))
		a.Op(arm64.Orr, s1, s1, s0, true)
	}
	g.pushReturn(ret, 0, true)
	a.FMovFromF(s0, x0)
	a.Load(s1, rCtx, offLinks)
	a.AddLsl(s1, s1, s0, 3)
	a.Load(s1, s1, 0)
	a.Br(s1)
	a.Bind(ret)
	g.restoreR()
	g.takeResults(res, int32(g.req.FrameSlots))
	a.Bind(after)
	g.reload(v)
	kind := uint8(stubCallIndirect)
	if ref {
		kind = stubCallRef
	}
	g.ool = append(g.ool, stub{kind: kind, l: poll, start: start, slow: slow, after: after, v: v, res: res})
}

// indirectCallee leaves in x0 the index of the function call_indirect,
// call_ref or their tail calls v calls, or goes to slow: the table's
// element past its end or null, a function of another type or instance,
// without native code, or whose frame has no room past frame bytes of L.
// A reference is not null here. The checks have s0 and s1, and keep what
// they need besides in x0 and x1. A callee that holds references fails
// the frame's check, and is checked again out of line by both its frames'
// sizes (Ctx.RFrames), its reference slots' past adv bytes of R.
func (g *gen) indirectCallee(v *Value, frame int64, adv int32, slow arm64.Label) {
	a := g.a
	m := g.req.Module
	lay := g.req.Layout
	if v.Op == OpCallRef || v.Op == OpTailRef {
		a.Load(s1, rCtx, offR)
		a.Load(s1, s1, g.calleeRefOff(v))
	} else {
		// The element, within the table and not null: a function.
		g.movG(s0, v.Args[len(v.Args)-1])
		a.Load(s1, rCtx, offTables)
		a.Load(s1, s1, int32(8*uint32(v.Aux2)))
		a.FMovToF(x1, s1)
		a.Load(s1, s1, lay.TableElems+8)
		a.Cmp(s0, s1, true)
		a.BCond(arm64.HS, slow)
		a.FMovFromF(s1, x1)
		a.Load(s1, s1, lay.TableElems)
		a.AddLsl(s1, s1, s0, 4)
		a.Load(s1, s1, 0)
		a.Cbz(s1, slow, true)
	}
	// Of the call's type, defined by this instance: its index is the
	// module's.
	a.LoadU32(s0, s1, lay.FuncType)
	a.CmpImm(s0, int64(m.Canonical[v.Aux]), false)
	a.BCond(arm64.NE, slow)
	a.LoadU32(s0, s1, lay.FuncIndex)
	a.FMovToF(x0, s0)
	a.Load(s1, s1, lay.FuncInst)
	a.Load(s0, rCtx, offInst)
	a.Cmp(s0, s1, true)
	a.BCond(arm64.NE, slow)
	// Its native entry, and its frame's room past this one's.
	a.FMovFromF(s0, x0)
	a.Load(s1, rCtx, offLinks)
	a.AddLsl(s1, s1, s0, 3)
	a.Load(s1, s1, 0)
	a.Cbz(s1, slow, true)
	refs, back := a.NewLabel(), a.NewLabel()
	a.Load(s1, rCtx, offFrames)
	a.AddLsl(s1, s1, s0, 3)
	a.Load(s1, s1, 0)
	a.Op(arm64.Add, s1, s1, rL, true)
	if frame != 0 {
		a.AddImm(s1, s1, frame, true)
	}
	a.Load(s0, rCtx, offChunkEnd)
	a.Cmp(s1, s0, true)
	a.BCond(arm64.HI, refs)
	a.Bind(back)
	g.ool = append(g.ool, stub{kind: stubRefRoom, l: refs, to: back, slow: slow, code: uint64(frame), off: adv})
}

// bulk lowers memory.copy and memory.fill of memory 0 (BulkInCode): the
// operands into X13, X14 and X15, which the allocator keeps free across,
// the ends checked against the memory's size, then loops of 32 bytes in
// a pair of vector registers, of 8 in s0 and of a byte -- from the end
// down for a copy to a higher address that overlaps its source, each
// block read before the bytes above it are written. Both ends are checked
// before a byte is written, as the interpreter's are.
func (g *gen) bulk(v *Value) {
	a := g.a
	in := func(r arm64.Reg, x *Value) pmove {
		return pmove{dst: loc{kind: lGPR, r: int(r)}, src: g.locOf(x), t: TI32}
	}
	oob := g.trap(wjit.TrapMemOutOfBounds, v.At)
	check := func(base arm64.Reg) {
		a.Op(arm64.Add, s0, base, bulkLen, true)
		a.Cmp(s0, rMemSize, true)
		a.BCond(arm64.HI, oob)
	}
	// loop emits a loop over blocks of step bytes while bulkLen has one:
	// body moves a block.
	loop := func(step int64, body func()) {
		top, next := a.NewLabel(), a.NewLabel()
		a.Bind(top)
		a.CmpImm(bulkLen, step, true)
		a.BCond(arm64.LO, next)
		body()
		a.AddImm(bulkLen, bulkLen, -step, true)
		a.B(top)
		a.Bind(next)
	}
	if opcode.Opcode(v.Aux) == opcode.MemoryFill {
		// Destination, value, count.
		g.parallel([]pmove{in(bulkDst, v.Args[0]), in(bulkSrc, v.Args[1]), in(bulkLen, v.Args[2])})
		check(bulkDst)
		a.Op(arm64.Add, bulkDst, bulkDst, rMem, true)
		a.Dup(arm64.V8, x0, bulkSrc)
		loop(32, func() { a.StorePairQPost(bulkDst, 32, x0, x0) })
		a.FMovFromF(s0, x0)
		loop(8, func() { a.StorePost(bulkDst, 8, s0, 8) })
		loop(1, func() { a.StorePost(bulkDst, 1, s0, 1) })
		return
	}
	// Destination, source, count.
	g.parallel([]pmove{in(bulkDst, v.Args[0]), in(bulkSrc, v.Args[1]), in(bulkLen, v.Args[2])})
	check(bulkSrc)
	check(bulkDst)
	up, done := a.NewLabel(), a.NewLabel()
	a.Cmp(bulkDst, bulkSrc, true)
	a.BCond(arm64.LS, up)
	a.Op(arm64.Add, s0, bulkSrc, bulkLen, true)
	a.Cmp(bulkDst, s0, true)
	a.BCond(arm64.HS, up)
	// Down from the ends.
	a.Op(arm64.Add, bulkSrc, bulkSrc, rMem, true)
	a.Op(arm64.Add, bulkSrc, bulkSrc, bulkLen, true)
	a.Op(arm64.Add, bulkDst, bulkDst, rMem, true)
	a.Op(arm64.Add, bulkDst, bulkDst, bulkLen, true)
	loop(32, func() {
		a.LoadPairQPre(x0, x1, bulkSrc, -32)
		a.StorePairQPre(bulkDst, -32, x0, x1)
	})
	loop(8, func() {
		a.LoadPre(s0, bulkSrc, -8, 8)
		a.StorePre(bulkDst, -8, s0, 8)
	})
	loop(1, func() {
		a.LoadPre(s0, bulkSrc, -1, 1)
		a.StorePre(bulkDst, -1, s0, 1)
	})
	a.B(done)
	a.Bind(up)
	a.Op(arm64.Add, bulkSrc, bulkSrc, rMem, true)
	a.Op(arm64.Add, bulkDst, bulkDst, rMem, true)
	loop(32, func() {
		a.LoadPairQPost(x0, x1, bulkSrc, 32)
		a.StorePairQPost(bulkDst, 32, x0, x1)
	})
	loop(8, func() {
		a.LoadPost(s0, bulkSrc, 8, 8)
		a.StorePost(bulkDst, 8, s0, 8)
	})
	loop(1, func() {
		a.LoadPost(s0, bulkSrc, 1, 1)
		a.StorePost(bulkDst, 1, s0, 1)
	})
	a.Bind(done)
}
