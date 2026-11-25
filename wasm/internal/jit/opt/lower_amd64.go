package opt

import (
	"fmt"
	"math"
	"slices"
	"unsafe"

	"github.com/go-quickjs/go-quickjs/internal/jit/asm/amd64"
	"github.com/go-quickjs/go-quickjs/internal/jit/lsra"
	wjit "github.com/go-quickjs/go-quickjs/wasm/internal/jit"
	"github.com/go-quickjs/go-quickjs/wasm/internal/opcode"
)

// Available says this architecture has the optimizing tier's lowering.
const Available = true

// The registers. The context, L, memory 0's base and the poll counter are
// where the baseline keeps them (DI, BX, R15, R12), so that the tiers call
// each other; the return stack's next entry is in the context while the
// code runs (it arrives and leaves in R10). R11 and R10 are the general
// scratch registers, X15 and X14 the float ones; the allocator has the
// rest but SP, BP and R14.
const (
	rCtx  = amd64.RDI
	rL    = amd64.RBX
	rMem  = amd64.R15
	rPoll = amd64.R12
	rRS   = amd64.R10
	s0    = amd64.R11
	s1    = amd64.R10
	x0    = amd64.XReg(15)
	x1    = amd64.XReg(14)
)

var (
	gprs    = []int{int(amd64.RSI), int(amd64.R8), int(amd64.R9), int(amd64.R13), int(amd64.RAX), int(amd64.RCX), int(amd64.RDX)}
	xmms    = []int{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13}
	classes = [][]int{gprs, xmms}
)

// The registers instructions clobber, by class: memory.copy's and
// memory.fill's AX, CX and SI, a division's AX and DX, a shift's CX for
// its count, and an atomic store's, read-modify-write's and
// compare-and-exchange's AX, CX and DX. The allocator only reads them.
var amd64Clobbers = clobbers{
	bulk:   []uint64{1<<amd64.RAX | 1<<amd64.RCX | 1<<amd64.RSI},
	div:    []uint64{1<<amd64.RAX | 1<<amd64.RDX},
	shift:  []uint64{1 << amd64.RCX},
	atomic: []uint64{1<<amd64.RAX | 1<<amd64.RCX | 1<<amd64.RDX},
}

// gprName and xmmName are a general and an SSE register's names in the
// listing.
func gprName(r int) string {
	return [...]string{"ax", "cx", "dx", "bx", "sp", "bp", "si", "di", "r8", "r9", "r10", "r11", "r12", "r13", "r14", "r15"}[r]
}

func xmmName(r int) string { return fmt.Sprintf("x%d", r) }

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

// genScratch is the lowering's part of a workspace: the lowering's state,
// the allocator, its description of the function, and the assembler.
type genScratch struct {
	g gen
	allocScratch
	asm amd64.Asm
}

// gen lowers an optimized function to amd64 code.
type gen struct {
	lowering
	ws *genScratch
	a  *amd64.Asm
	// labels is each block's label, by index.
	labels []amd64.Label
	// ool is the out-of-line code, emitted after the blocks.
	ool []stub
	// traps are the trap stubs by code and offset (trapKey), trapTo the
	// code that traps by code, polls the poll exits by block index.
	traps  u64map[amd64.Label]
	trapTo u64map[amd64.Label]
	polls  u64map[amd64.Label]
	// moves and pending are the parallel moves' lists.
	moves, pending []pmove
	// wb is the function's barrier routine, which hasWB says is made;
	// argSlotsBuf is a call's arguments' slots in the call area.
	wb          amd64.Label
	hasWB       bool
	argSlotsBuf []int32
	// alloc is the function's exit for an empty pool of new structs,
	// which hasAlloc says is made.
	alloc    amd64.Label
	hasAlloc bool
	// vpool is the code's v128 constants, emitted after it.
	vpool vpool[amd64.Label]
	// entry is the code's native entry, where a call of the function by
	// itself goes.
	entry amd64.Label
	// rs is the register the return stack's next entry is kept in through
	// the code, where rsKept: one of the allocator's that the function
	// leaves unused (keepRS). Else the context's RSP holds it.
	rs     amd64.Reg
	rsKept bool
	// wbG and wbX are the allocator's registers the barrier routine keeps:
	// those the allocator gave a value, those a sequence took for its own
	// while it may store through the barrier (freeGPR, clearRefs), and
	// the one that keeps the return stack's entry.
	wbG, wbX uint64
}

// keepRS chooses where the code keeps the return stack's next entry: in a
// register of the allocator's that it gave to no value and that no
// instruction of the function writes besides -- a division's, a shift's,
// memory.copy's, an atomic's (their clobbers), and the clearing of the
// reference slots at the entry -- else in the context.
func (g *gen) keepRS() {
	var used uint64
	for vr, c := range g.ws.lf.Class {
		if c == classGPR && vr < len(g.ra.Reg) && g.ra.Reg[vr] >= 0 {
			used |= 1 << g.ra.Reg[vr]
		}
	}
	lf := &g.ws.lf
	for _, b := range lf.Blocks {
		for k := range b.Insts {
			if m := lf.ClobberOf(&b.Insts[k]); len(m) > classGPR {
				used |= m[classGPR]
			}
		}
	}
	if g.holdsRefs() {
		used |= 1<<amd64.RAX | 1<<amd64.RCX | 1<<amd64.RDX
	}
	for _, r := range [...]amd64.Reg{amd64.R13, amd64.R9, amd64.R8, amd64.RSI, amd64.RDX, amd64.RCX, amd64.RAX} {
		if used&(1<<r) == 0 {
			g.rs, g.rsKept = r, true
			return
		}
	}
}

// loadRS sets R10 to the return stack's next entry: a copy of the
// register that keeps it, or a load from the context.
func (g *gen) loadRS() {
	if g.rsKept {
		g.a.MovRR(rRS, g.rs)
		return
	}
	g.a.Load(rRS, rCtx, offRSP)
}

// reset readies g, kept in a workspace, for function f, keeping the
// memory of its lists and tables.
func (g *gen) reset(ws *genScratch, f *Func, req *wjit.Request) {
	// The out-of-line code is only appended to: what the last compile
	// made is its length.
	clear(g.ool)
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

// displace has the allocator take a register for a web from those in its
// way whatever their density, splitting them (lsra.Config.Displace): on
// amd64, where most uses from a slot are memory operands, regexp's and
// sort's code gained 4-5% by it, the other programs level.
const displace = true

// lower lowers f, optimized, to native code for req's function.
func lower(f *Func, req *wjit.Request, ws *workspace) *wjit.Code {
	g := &ws.gen.g
	g.reset(&ws.gen, f, req)
	for i, b := range f.Blocks {
		g.index[b.ID] = int32(i)
	}
	g.allocate(&g.ws.allocScratch, classes, &amd64Clobbers)
	g.keepRS()
	for i, r := range g.ra.Reg {
		// The temporaries past the values hold nothing across a store.
		if r >= 0 && i < len(g.byVreg) {
			if g.byVreg[i].Type.InFloatReg() {
				g.wbX |= 1 << r
			} else {
				g.wbG |= 1 << r
			}
		}
	}
	if g.rsKept {
		g.wbG |= 1 << g.rs
	}
	a := g.a
	g.mark("return stub")
	retStub := a.Len()
	a.Store(rCtx, offPoll, rPoll)
	a.Store(rCtx, offRSP, rRS)
	a.StoreImm(rCtx, offExit, wjit.ExitReturn)
	a.Ret()
	goEntry, entry := a.NewLabel(), a.NewLabel()
	g.entry = entry
	a.Bind(goEntry)
	g.mark("entry")
	a.Load(rL, rCtx, offL)
	a.Load(rMem, rCtx, offMemBase)
	a.Load(rPoll, rCtx, offPoll)
	a.Load(rRS, rCtx, offRSP)
	a.Bind(entry)
	if g.rsKept {
		a.MovRR(g.rs, rRS)
	} else {
		a.Store(rCtx, offRSP, rRS)
	}
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
		// The marks were taken before Finish shortened the jumps.
		for i := range g.marks {
			g.marks[i].Off = a.Moved(g.marks[i].Off)
		}
		code.Marks, code.Listing = g.marks, g.listing(gprName, xmmName)
	}
	return code
}

// The moves between locations, of a value of type t. A slot's word holds
// an i32 or an f32 with its upper half clear, as the interpreter's frames
// do: Go reads the slots of calls through it.

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
		r := amd64.Reg(dst.r)
		switch src.kind {
		case lGPR:
			a.MovRR(r, amd64.Reg(src.r))
		case lSlot:
			if t == TI32 {
				a.LoadU32(r, rL, slotOff(src.slot))
			} else {
				a.Load(r, rL, slotOff(src.slot))
			}
		case lConst:
			c := src.c
			if t == TI32 || t == TF32 {
				c = uint64(uint32(c))
			}
			a.MovImm(r, c)
		case lXMM:
			if t == TF32 || t == TI32 {
				a.MovDFromX(r, amd64.XReg(src.r))
			} else {
				a.MovQFromX(r, amd64.XReg(src.r))
			}
		}
	case lXMM:
		x := amd64.XReg(dst.r)
		switch src.kind {
		case lXMM:
			a.SSEOp(amd64.MovAPD, x, amd64.XReg(src.r))
		case lSlot:
			if t == TF32 {
				a.LoadSS(x, rL, slotOff(src.slot))
			} else {
				a.LoadSD(x, rL, slotOff(src.slot))
			}
		case lConst:
			c := src.c
			if t == TF32 {
				c = uint64(uint32(c))
			}
			if c == 0 {
				a.SSEOp(amd64.XorPD, x, x)
				return
			}
			// From the code's pool of constants, the upper bits clear.
			a.LoadSDLabel(x, g.vpool.label(c, 0, a.NewLabel))
		case lGPR:
			if t == TF32 {
				a.MovDToX(x, amd64.Reg(src.r))
			} else {
				a.MovQToX(x, amd64.Reg(src.r))
			}
		}
	case lSlot:
		off := slotOff(dst.slot)
		switch src.kind {
		case lGPR:
			a.Store(rL, off, amd64.Reg(src.r))
		case lXMM:
			if t == TF32 {
				a.StoreSS(rL, off, amd64.XReg(src.r))
				a.StoreImm32(rL, off+4, 0)
			} else {
				a.StoreSD(rL, off, amd64.XReg(src.r))
			}
		case lSlot:
			a.Load(s0, rL, slotOff(src.slot))
			a.Store(rL, off, s0)
		case lConst:
			c := src.c
			if t == TI32 || t == TF32 {
				c = uint64(uint32(c))
			}
			if int64(c) == int64(int32(c)) {
				a.StoreImm(rL, off, int32(c))
			} else {
				a.MovImm(s0, c)
				a.Store(rL, off, s0)
			}
		}
	}
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
func (g *gen) intoG(v *Value, scratch amd64.Reg) amd64.Reg {
	l := g.locOf(v)
	if l.kind == lGPR {
		return amd64.Reg(l.r)
	}
	g.move(loc{kind: lGPR, r: int(scratch)}, l, v.Type)
	return scratch
}

// intoX returns a float register holding v: its own, or scratch loaded.
func (g *gen) intoX(v *Value, scratch amd64.XReg) amd64.XReg {
	l := g.locOf(v)
	if l.kind == lXMM {
		return amd64.XReg(l.r)
	}
	g.move(loc{kind: lXMM, r: int(scratch)}, l, v.Type)
	return scratch
}

// movG and movX set a register to v.
func (g *gen) movG(r amd64.Reg, v *Value) { g.move(loc{kind: lGPR, r: int(r)}, g.locOf(v), v.Type) }

func (g *gen) movX(x amd64.XReg, v *Value) { g.move(loc{kind: lXMM, r: int(x)}, g.locOf(v), v.Type) }

// defG and defX are the register v's result is computed in: its own, or
// the first scratch register for a value spilled.
func (g *gen) defG(v *Value) amd64.Reg {
	if r := g.where[v.vreg]; r >= 0 {
		return amd64.Reg(r)
	}
	return s0
}

func (g *gen) defX(v *Value) amd64.XReg {
	if r := g.where[v.vreg]; r >= 0 {
		return amd64.XReg(r)
	}
	return x0
}

// done stores v's result, computed in register r, into its slot where it
// is there: its home if spilled or in a hole of a split one, its copy if
// it lives across a call.
func (g *gen) done(v *Value, r int) {
	s := g.ra.Slot[v.vreg]
	if s < 0 || (g.where[v.vreg] >= 0 && !g.stores(v)) {
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

// imm32 reports whether constant c is an immediate of a 32-bit operation,
// or, wide, of a 64-bit one, which sign-extends it.
func imm32(c uint64, wide bool) (int32, bool) {
	if !wide {
		return int32(uint32(c)), true
	}
	return int32(c), int64(c) == int64(int32(c))
}

// trap returns a label that traps with code at offset at.
func (g *gen) trap(code uint64, at uint32) amd64.Label {
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
// poll's exit, and a call's poll and its call through Go.
type stub struct {
	kind uint8
	// l is the stub's label, to where a trap's site goes, or a poll where
	// it goes back; start, slow and after are a call's.
	l, to              amd64.Label
	start, slow, after amd64.Label
	code               uint64
	at                 uint32
	// v is the call, res its results, and wh where its operands and
	// results are (keepWhere).
	v   *Value
	res []*Value
	wh  []int16
	// dst, off and flag are a barrier's store (stubBarrier): its slot's
	// base and offset, and the register its slow path has.
	dst, flag amd64.Reg
	off       int32
}

// The stubs' kinds.
const (
	stubTrapCode = iota
	stubTrapSite
	stubPoll
	stubCall
	stubCallIndirect
	stubCallRef
	stubBarrier
	stubWB
	stubTail
	stubRefRoom
	stubAlloc
	stubAllocExit
	// stubFill fills the array the engine made past the pools for
	// refArrayNew v, into the slot past off from R, as code's type, and
	// goes on at to.
	stubFill
	// stubTrapWide is where the widened check of access v fails, s0
	// the farthest end it checked: it traps at the first of the accesses
	// the check covers whose end is past the memory's.
	stubTrapWide
)

// stub emits out-of-line code.
func (g *gen) stub(st *stub) {
	a := g.a
	switch st.kind {
	case stubTrapCode:
		a.Bind(st.to)
		g.mark("cold trap")
		a.Store(rCtx, offL, rL)
		if g.rsKept {
			a.Store(rCtx, offRSP, g.rs)
		}
		g.storeCtx(offExit, wjit.ExitTrap)
		g.storeCtx(offArg0, st.code)
		g.storeCtx(offExitFn, uint64(g.fn))
		a.Ret()
	case stubTrapSite:
		a.Bind(st.l)
		g.mark("cold trap")
		g.storeCtx(offArg0+8, uint64(st.at))
		a.Jmp(st.to)
	case stubTrapWide:
		a.Bind(st.l)
		g.mark("cold trap")
		w := wideChecks(g.f, st.v)
		at := w[len(w)-1].end
		for _, c := range w[:len(w)-1] {
			a.Lea(s0, s0, int32(c.end-at))
			at = c.end
			a.CmpMem(s0, rCtx, offMemSize, true)
			a.Jcc(amd64.CondA, g.trap(wjit.TrapMemOutOfBounds, c.v.At))
		}
		a.Jmp(g.trap(wjit.TrapMemOutOfBounds, w[len(w)-1].v.At))
	case stubPoll:
		a.Bind(st.l)
		g.mark("cold poll")
		g.saveAll()
		g.exit(wjit.ExitPoll, a.NewLabel())
		g.restoreAll()
		a.Jmp(st.to)
	case stubAlloc:
		// An empty pool of new structs of type code: the function's exit
		// for it, to take from it again at to.
		a.Bind(st.l)
		g.mark("cold alloc")
		g.storeCtx(offArg0, st.code)
		g.storeCtx(offArg0+8, uint64(st.at))
		a.LeaLabel(s0, st.to)
		a.Jmp(g.allocExit())
	case stubAllocExit:
		// The function's exit for an empty pool: the engine fills it,
		// every register kept, and the code goes on where its site left
		// in s0, its arguments stored.
		a.Bind(st.l)
		g.mark("cold alloc exit")
		a.Store(rCtx, offResume, s0)
		g.saveAll()
		g.exitAlloc(a.NewLabel())
		g.restoreAll()
		a.JmpMem(rCtx, offResume)
	case stubFill:
		a.Bind(st.l)
		g.mark("cold array fill")
		g.fillMade(st.v, st.off, uint32(st.code))
		a.Jmp(st.to)
	case stubCall, stubCallIndirect, stubCallRef:
		v := st.v
		a.Bind(st.l)
		g.mark("cold call")
		g.saveAll()
		g.exit(wjit.ExitPoll, a.NewLabel())
		g.restoreAll()
		a.Jmp(st.start)
		a.Bind(st.slow)
		g.putWhere(v, st.res, st.wh)
		g.exitCall(v, st.res)
		a.Jmp(st.after)
	case stubBarrier:
		a.Bind(st.l)
		g.mark("cold barrier")
		a.MovQFromX(st.flag, x0)
		a.Store(rCtx, offWBPtr, st.flag)
		a.Lea(st.flag, st.dst, st.off)
		a.Store(rCtx, offWBSlot, st.flag)
		a.LeaLabel(s0, st.to)
		a.Jmp(g.wbRoutine())
	case stubWB:
		g.emitWB(st.l)
	case stubTail:
		a.Bind(st.l)
		g.mark("cold tail call")
		g.putWhere(st.v, nil, st.wh)
		g.exitTail(st.v)
	case stubRefRoom:
		// A callee that holds references, whose Ctx.Frames is past any
		// chunk's end: its numeric slots' room past L plus code bytes,
		// and its reference slots' past R plus off, by Ctx.RFrames. The
		// callee's index is in s0, and kept in x0 meanwhile.
		a.Bind(st.l)
		g.mark("cold reference room")
		a.MovQToX(x0, s0)
		a.Load(s1, rCtx, offRFrames)
		a.LoadIdxScaled(s0, s1, s0, 3, 0)
		a.MovRR32(s1, s0)
		a.Op(amd64.Add, s1, rL, true)
		if st.code != 0 {
			a.OpImm(amd64.Add, s1, int32(st.code), true)
		}
		a.CmpMem(s1, rCtx, offChunkEnd, true)
		a.Jcc(amd64.CondA, st.slow)
		a.ShiftImm(amd64.Shr, s0, 32, true)
		a.AddMem(s0, rCtx, offR)
		if st.off != 0 {
			a.OpImm(amd64.Add, s0, st.off, true)
		}
		a.CmpMem(s0, rCtx, offRChunkEnd, true)
		a.Jcc(amd64.CondA, st.slow)
		a.MovQFromX(s0, x0)
		a.Jmp(st.to)
	}
}

func (g *gen) storeCtx(off int32, v uint64) {
	if v <= math.MaxInt32 {
		g.a.StoreImm(rCtx, off, int32(v))
		return
	}
	g.a.MovImm(s0, v)
	g.a.Store(rCtx, off, s0)
}

// exit returns to the engine for kind, with args, to resume at resume,
// where the registers the code keeps are loaded again: every value live
// past it is in its slot.
func (g *gen) exit(kind uint64, resume amd64.Label, args ...uint64) {
	a := g.a
	a.Store(rCtx, offL, rL)
	a.Store(rCtx, offPoll, rPoll)
	if g.rsKept {
		a.Store(rCtx, offRSP, g.rs)
	}
	g.storeCtx(offExit, kind)
	for i, v := range args {
		g.storeCtx(offArg0+int32(8*i), v)
	}
	g.storeCtx(offExitFn, uint64(g.fn))
	a.LeaLabel(s0, resume)
	a.Store(rCtx, offExitAddr, s0)
	a.Ret()
	a.Bind(resume)
	a.Load(rL, rCtx, offL)
	a.Load(rMem, rCtx, offMemBase)
	a.Load(rPoll, rCtx, offPoll)
	if g.rsKept {
		a.Load(g.rs, rCtx, offRSP)
	}
}

// exitAlloc is exit's ExitAlloc, its arguments stored, by way of the
// allocation stub where the request has one (wjit.Request.AllocStub): the
// exit's record stored, the code jumps to the stub, which keeps only the
// context, and goes on at resume where the engine made the object, else
// takes the exit.
func (g *gen) exitAlloc(resume amd64.Label) {
	stub := g.req.AllocStub
	if stub == 0 {
		g.exit(wjit.ExitAlloc, resume)
		return
	}
	a := g.a
	a.Store(rCtx, offL, rL)
	a.Store(rCtx, offPoll, rPoll)
	if g.rsKept {
		a.Store(rCtx, offRSP, g.rs)
	}
	g.storeCtx(offExit, wjit.ExitAlloc)
	g.storeCtx(offExitFn, uint64(g.fn))
	a.LeaLabel(s0, resume)
	a.Store(rCtx, offExitAddr, s0)
	back := a.NewLabel()
	a.LeaLabel(s0, back)
	a.MovImm(amd64.RAX, stub)
	a.JmpReg(amd64.RAX)
	a.Bind(back)
	a.Op(amd64.Test, amd64.RAX, amd64.RAX, true)
	a.Jcc(amd64.CondNE, resume)
	a.Ret()
	a.Bind(resume)
	a.Load(rL, rCtx, offL)
	a.Load(rMem, rCtx, offMemBase)
	a.Load(rPoll, rCtx, offPoll)
	if g.rsKept {
		a.Load(g.rs, rCtx, offRSP)
	}
}

// saveAll and restoreAll keep every allocatable register in the context's
// save area across an exit that is not a call: a poll's. A function with
// v128 values keeps its SSE registers whole (Ctx.SaveV).
func (g *gen) saveAll() {
	a := g.a
	for i, r := range gprs {
		if g.rsKept && amd64.Reg(r) == g.rs {
			continue
		}
		a.Store(rCtx, offSave+int32(8*i), amd64.Reg(r))
	}
	for i, x := range xmms {
		if g.v128 {
			a.StoreVec(rCtx, offSaveV+16*int32(x), amd64.XReg(x))
			continue
		}
		a.StoreSD(rCtx, offSave+int32(8*(len(gprs)+i)), amd64.XReg(x))
	}
}

func (g *gen) restoreAll() {
	a := g.a
	for i, r := range gprs {
		if g.rsKept && amd64.Reg(r) == g.rs {
			// The exit's resumption loaded it.
			continue
		}
		a.Load(amd64.Reg(r), rCtx, offSave+int32(8*i))
	}
	for i, x := range xmms {
		if g.v128 {
			a.LoadVec(amd64.XReg(x), rCtx, offSaveV+16*int32(x))
			continue
		}
		a.LoadSD(amd64.XReg(x), rCtx, offSave+int32(8*(len(gprs)+i)))
	}
}

// allocExit returns the function's exit for an empty pool of new structs,
// made after the body with its first use.
func (g *gen) allocExit() amd64.Label {
	if !g.hasAlloc {
		g.hasAlloc = true
		g.alloc = g.a.NewLabel()
		g.ool = append(g.ool, stub{kind: stubAllocExit, l: g.alloc})
	}
	return g.alloc
}

// pollExit is out-of-line code that exits for a poll, every register kept,
// and goes to back.
func (g *gen) pollExit(back amd64.Label) amd64.Label {
	l := g.a.NewLabel()
	g.ool = append(g.ool, stub{kind: stubPoll, l: l, to: back})
	return l
}

// jump goes from block i to its successor s: a back edge counts down the
// poll counter.
func (g *gen) jump(i int, s *Block) {
	a := g.a
	j := int(g.index[s.ID])
	if j > i {
		if j != i+1 {
			a.Jmp(g.labels[j])
		}
		return
	}
	l, ok := g.polls.get(uint64(j))
	if !ok {
		l = g.pollExit(g.labels[j])
		g.polls.set(uint64(j), l)
	}
	g.mark("poll %v", s)
	a.OpImm(amd64.Sub, rPoll, 1, true)
	a.Jcc(amd64.CondG, g.labels[j])
	a.Jmp(l)
}

// edgeMoves moves the arguments of s's phis for the edge from b, and the
// split values that move on it where b has no other.
func (g *gen) edgeMoves(b, s *Block) {
	var ms []lsra.Move
	if len(b.Succs) == 1 {
		ms = g.ra.Edge(int(g.index[b.ID]), 0)
	}
	if len(s.Phis) == 0 && len(ms) == 0 {
		return
	}
	g.mark("edge %v->%v", b, s)
	moves := g.phiMoves(g.moves[:0], b, s)
	moves = g.splitMoves(moves, ms, false)
	g.parallel(moves)
	g.moves = moves[:0]
}

// predMoves makes, where block b, the i-th, begins, the moves of the split
// values on the edge from its predecessor where that has other successors.
func (g *gen) predMoves(b *Block) {
	if len(b.Preds) != 1 || len(b.Preds[0].Succs) < 2 {
		return
	}
	p := b.Preds[0]
	ms := g.ra.Edge(int(g.index[p.ID]), slices.Index(p.Succs, b))
	if len(ms) == 0 {
		return
	}
	g.mark("edge %v->%v", p, b)
	moves := g.splitMoves(g.moves[:0], ms, false)
	g.parallel(moves)
	g.moves = moves[:0]
}

// gapMoves makes the moves of split values before instruction j, as the
// allocator numbers them, of block i.
func (g *gen) gapMoves(i, j int) { g.gap(g.ra.Moves(i, j)) }

// gap makes ms, the moves of split values before an instruction.
func (g *gen) gap(ms []lsra.Move) {
	if len(ms) == 0 {
		return
	}
	moves := g.splitMoves(g.moves[:0], ms, true)
	if len(moves) > 0 {
		g.mark("split")
		g.parallel(moves)
	}
	g.moves = moves[:0]
}

// block lowers block b, the i-th.
func (g *gen) block(i int, b *Block) {
	if b.Pad != nil {
		g.padEntry(b)
	}
	split := g.ra.Splits()
	if split {
		g.blockWhere(i)
		g.predMoves(b)
		g.blockGaps(i)
	}
	for _, p := range b.Phis {
		if r := g.where[p.vreg]; r >= 0 && g.stores(p) && !g.phiStore[p.vreg] {
			g.done(p, int(r))
		}
	}
	inst := 0
	for j := 0; j < len(b.Values); j++ {
		v := b.Values[j]
		if v.Op == OpConst || v.Op == OpResult || g.fused[v.ID] {
			continue
		}
		if split {
			g.gap(g.gapAt(inst))
		}
		inst++
		if g.marking {
			g.mark("%v %v %v", v, v.Op, v.Type)
		}
		g.value(v, b.Values[j+1:])
	}
	if split {
		g.gap(g.gapAt(inst))
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
		cond := g.condition(b.Control)
		t, e := int(g.index[b.Succs[0].ID]), int(g.index[b.Succs[1].ID])
		switch {
		case t == i+1:
			a.Jcc(cond.Invert(), g.labels[e])
		case e == i+1:
			a.Jcc(cond, g.labels[t])
		default:
			a.Jcc(cond, g.labels[t])
			a.Jmp(g.labels[e])
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
		if g.rsKept {
			a.Lea(rRS, g.rs, -wjit.RSEntryWords*8)
		} else {
			a.Load(rRS, rCtx, offRSP)
			a.OpImm(amd64.Sub, rRS, wjit.RSEntryWords*8, true)
		}
		a.JmpMem(rRS, 0)
	case BTrap:
		a.Jmp(g.trap(wjit.TrapUnreachable, b.At))
	case BTail:
		// The tail call, its last value, left the function.
	}
}

// switchTable jumps through a table of jumps, each to its target's stub.
func (g *gen) switchTable(i int, b *Block) {
	a := g.a
	n := len(b.Table) - 1
	g.movG(s0, b.Control)
	a.MovImm(s1, uint64(n))
	a.Op(amd64.Cmp, s0, s1, false)
	a.Cmov(amd64.CondAE, s0, s1, false)
	table := a.NewLabel()
	a.LeaLabel(s1, table)
	a.LeaIdx(s1, s1, s0, 8, 0)
	a.JmpReg(s1)
	a.Bind(table)
	stubs := make([]amd64.Label, len(b.Succs))
	for k := range stubs {
		stubs[k] = a.NewLabel()
	}
	for _, k := range b.Table {
		a.JmpLong(stubs[k])
		a.Nop(3)
	}
	for k, s := range b.Succs {
		a.Bind(stubs[k])
		if j := int(g.index[s.ID]); j > i {
			a.Jmp(g.labels[j])
		} else {
			g.jump(i, s)
		}
	}
}

// condition sets the flags for the branch on c and returns the condition
// under which it is true.
func (g *gen) condition(c *Value) amd64.Cond {
	if g.fused[c.ID] {
		return g.compare(c)
	}
	r := g.intoG(c, s0)
	g.a.Op(amd64.Test, r, r, false)
	return amd64.CondNE
}

// compare sets the flags for integer comparison v, returning the
// condition under which it holds.
func (g *gen) compare(v *Value) amd64.Cond {
	a := g.a
	x := v.Args[0]
	if fusesFloat(v.Op) {
		// UCOMISD leaves an unordered result CF, ZF and PF set: above
		// and above or equal are false for it, and their inverses true.
		xa, xb := g.intoX(x, x0), g.intoX(v.Args[1], x1)
		if v.Op == OpFLt || v.Op == OpFLe {
			xa, xb = xb, xa
		}
		g.sse(x.Type == TF32, amd64.UcomiSD, xa, xb)
		if v.Op == OpFGt || v.Op == OpFLt {
			return amd64.CondA
		}
		return amd64.CondAE
	}
	wide := x.Type == TI64
	if v.Op == OpEqz {
		r := g.intoG(x, s0)
		a.Op(amd64.Test, r, r, wide)
		return amd64.CondE
	}
	y := v.Args[1]
	cond := cmpConds[v.Op]
	if x.Op == OpConst && y.Op != OpConst {
		x, y = y, x
		cond = mirror(cond)
	}
	r := g.intoG(x, s0)
	switch l := g.locOf(y); l.kind {
	case lConst:
		if imm, ok := imm32(l.c, wide); ok {
			a.OpImm(amd64.Cmp, r, imm, wide)
		} else {
			a.MovImm(s1, l.c)
			a.Op(amd64.Cmp, r, s1, wide)
		}
	case lGPR:
		a.Op(amd64.Cmp, r, amd64.Reg(l.r), wide)
	case lSlot:
		a.OpMem(amd64.Cmp, r, rL, slotOff(l.slot), wide)
	}
	return cond
}

var cmpConds = [numOps]amd64.Cond{
	OpEq: amd64.CondE, OpNe: amd64.CondNE, OpLtS: amd64.CondL, OpLtU: amd64.CondB, OpGtS: amd64.CondG,
	OpGtU: amd64.CondA, OpLeS: amd64.CondLE, OpLeU: amd64.CondBE, OpGeS: amd64.CondGE, OpGeU: amd64.CondAE,
}

// mirror is the condition of a comparison with its operands swapped.
func mirror(cond amd64.Cond) amd64.Cond {
	switch cond {
	case amd64.CondL:
		return amd64.CondG
	case amd64.CondG:
		return amd64.CondL
	case amd64.CondLE:
		return amd64.CondGE
	case amd64.CondGE:
		return amd64.CondLE
	case amd64.CondB:
		return amd64.CondA
	case amd64.CondA:
		return amd64.CondB
	case amd64.CondBE:
		return amd64.CondAE
	case amd64.CondAE:
		return amd64.CondBE
	}
	return cond
}

var alus = [numOps]amd64.ALU{OpAdd: amd64.Add, OpSub: amd64.Sub, OpAnd: amd64.And, OpOr: amd64.Or, OpXor: amd64.Xor}

var shifts = [numOps]amd64.Shift{OpShl: amd64.Shl, OpShrS: amd64.Sar, OpShrU: amd64.Shr, OpRotl: amd64.Rol, OpRotr: amd64.Ror}

// roundModes are the roundings' modes, floatOps the float arithmetic's
// instructions.
var (
	roundModes = [numOps]uint8{OpFCeil: amd64.RoundCeil, OpFFloor: amd64.RoundFloor, OpFTrunc: amd64.RoundTrunc, OpFNearest: amd64.RoundNearest}
	floatOps   = [numOps]amd64.SSE{OpFAdd: amd64.AddSD, OpFSub: amd64.SubSD, OpFMul: amd64.MulSD, OpFDiv: amd64.DivSD}
)

// value lowers v; rest are the values after it in its block.
func (g *gen) value(v *Value, rest []*Value) {
	a := g.a
	wide := v.Type == TI64
	switch v.Op {
	case OpParam:
		if r := g.where[v.vreg]; r >= 0 {
			dst := loc{kind: lGPR, r: int(r)}
			if v.Type.InFloatReg() {
				dst.kind = lXMM
			}
			g.move(dst, loc{kind: lSlot, slot: int32(v.Aux)}, v.Type)
		}
	case OpAdd, OpSub, OpAnd, OpOr, OpXor:
		g.alu(v, alus[v.Op])
	case OpMul:
		g.mul(v)
	case OpShl, OpShrS, OpShrU, OpRotl, OpRotr:
		g.shift(v, shifts[v.Op])
	case OpDivS, OpDivU, OpRemS, OpRemU:
		g.div(v)
	case OpMulHU, OpMulHS:
		g.mulHigh(v)
	case OpClz, OpCtz, OpPopcnt:
		t := g.defG(v)
		w := g.width(v.Args[0])
		src := g.intoG(v.Args[0], t)
		switch v.Op {
		case OpClz:
			a.MovImm(s1, 2*w-1)
			a.Bsr(t, src, wide)
			a.Cmov(amd64.CondE, t, s1, wide)
			a.OpImm(amd64.Xor, t, int32(w-1), wide)
		case OpCtz:
			a.MovImm(s1, w)
			a.Bsf(t, src, wide)
			a.Cmov(amd64.CondE, t, s1, wide)
		default:
			a.Popcnt(t, src, wide)
		}
		g.done(v, int(t))
	case OpEqz, OpEq, OpNe, OpLtS, OpLtU, OpGtS, OpGtU, OpLeS, OpLeU, OpGeS, OpGeU:
		cond := g.compare(v)
		t := g.defG(v)
		a.Setcc(cond, t)
		a.MovZX8(t, t)
		g.done(v, int(t))
	case OpWrap:
		t := g.defG(v)
		switch l := g.locOf(v.Args[0]); l.kind {
		case lGPR:
			a.MovRR32(t, amd64.Reg(l.r))
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
		g.move(loc{kind: lGPR, r: int(t)}, g.locOf(v.Args[0]), v.Args[0].Type)
		switch v.Op {
		case OpExtendS, OpExtend32S:
			a.Movsxd(t, t)
		case OpExtend8S:
			a.Movsx8(t, t, wide)
		default:
			a.Movsx16(t, t, wide)
		}
		g.done(v, int(t))
	case OpFAdd, OpFSub, OpFMul, OpFDiv:
		g.floatOp(v)
	case OpFMin, OpFMax:
		g.minMax(v)
	case OpFCopysign:
		g.copySign(v)
	case OpFAbs, OpFNeg:
		t := g.defX(v)
		g.movX(t, v.Args[0])
		var m uint64
		op := amd64.AndPD
		switch {
		case v.Op == OpFAbs && v.Type == TF32:
			m = 0x7fffffff
		case v.Op == OpFAbs:
			m = 0x7fffffffffffffff
		case v.Type == TF32:
			m, op = 0x80000000, amd64.XorPD
		default:
			m, op = 1<<63, amd64.XorPD
		}
		a.LoadSDLabel(x1, g.vpool.label(m, 0, a.NewLabel))
		a.SSEOp(op, t, x1)
		g.done(v, int(t))
	case OpFSqrt, OpFCeil, OpFFloor, OpFTrunc, OpFNearest, OpPromote, OpDemote:
		t := g.defX(v)
		g.movX(t, v.Args[0])
		single := v.Type == TF32
		switch v.Op {
		case OpFSqrt:
			if single {
				a.SSEOpS(amd64.SqrtSD, t, t)
			} else {
				a.SSEOp(amd64.SqrtSD, t, t)
			}
		case OpPromote:
			a.Cvtss2sd(t, t)
		case OpDemote:
			a.Cvtsd2ss(t, t)
		default:
			mode := roundModes[v.Op]
			if single {
				a.RoundSS(t, t, mode)
			} else {
				a.RoundSD(t, t, mode)
			}
		}
		g.done(v, int(t))
	case OpFEq, OpFNe, OpFLt, OpFGt, OpFLe, OpFGe:
		g.floatCompare(v)
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
		g.intToFloat(v)
	case OpTruncS, OpTruncU:
		g.truncate(v)
	case OpTruncSatS, OpTruncSatU:
		g.saturate(v)
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
				a.LoadSS(t, p, 0)
			case TV128:
				a.LoadVec(t, p, 0)
			default:
				a.LoadSD(t, p, 0)
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
		// The value first: moving a float constant into a register goes
		// through s0, which the global's address may be loaded into.
		x := v.Args[1]
		switch {
		case x.Type == TV128:
			xv := g.intoX(x, x0)
			a.StoreVec(g.intoG(v.Args[0], s0), 0, xv)
		case x.Type == TF64:
			xv := g.intoX(x, x0)
			a.StoreSD(g.intoG(v.Args[0], s0), 0, xv)
		case x.Type == TF32:
			xv := g.intoX(x, x0)
			p := g.intoG(v.Args[0], s0)
			a.StoreSS(p, 0, xv)
			a.StoreImm32(p, 4, 0)
		default:
			xv := g.intoG(x, s1)
			a.Store(g.intoG(v.Args[0], s0), 0, xv)
		}
	case OpCall:
		g.call(v, rest)
	case OpCallIndirect:
		g.callIndirect(v, rest)
	case OpGoOp:
		if v.BulkInCode() {
			g.bulk(v)
		} else {
			g.callGo(v, rest)
		}
	case OpCallRef:
		g.callIndirect(v, rest)
	case OpRef:
		g.refOp(v)
	case OpTail, OpTailIndirect, OpTailRef:
		g.tail(v)
	case OpAtomicLoad, OpAtomicStore, OpAtomicAdd, OpAtomicSub, OpAtomicAnd, OpAtomicOr, OpAtomicXor, OpAtomicXchg,
		OpAtomicCmpxchg, OpFence:
		g.atomic(v)
	case OpSIMD:
		g.simd(v, rest)
	case OpExnVal:
		g.exnVal(v)
	default:
		panic("wasm opt: no lowering of " + v.Op.String())
	}
}

// width is an integer value's width in bits.
func (g *gen) width(v *Value) uint64 {
	if v.Type == TI64 {
		return 64
	}
	return 32
}

// alu lowers a two-operand operation of the ALU.
func (g *gen) alu(v *Value, op amd64.ALU) {
	a := g.a
	wide := v.Type == TI64
	x, y := v.Args[0], v.Args[1]
	t := g.defG(v)
	if l := g.locOf(y); l.kind == lGPR && amd64.Reg(l.r) == t && x != y {
		if v.Commutes() {
			g.aluOperand(op, t, x, wide)
		} else {
			g.movG(s1, x)
			a.Op(op, s1, t, wide)
			a.MovRR(t, s1)
		}
		g.done(v, int(t))
		return
	}
	g.movG(t, x)
	g.aluOperand(op, t, y, wide)
	g.done(v, int(t))
}

// aluOperand is t op= y.
func (g *gen) aluOperand(op amd64.ALU, t amd64.Reg, y *Value, wide bool) {
	a := g.a
	switch l := g.locOf(y); l.kind {
	case lConst:
		if imm, ok := imm32(l.c, wide); ok {
			a.OpImm(op, t, imm, wide)
		} else {
			a.MovImm(s1, l.c)
			a.Op(op, t, s1, wide)
		}
	case lGPR:
		a.Op(op, t, amd64.Reg(l.r), wide)
	case lSlot:
		a.OpMem(op, t, rL, slotOff(l.slot), wide)
	}
}

func (g *gen) mul(v *Value) {
	a := g.a
	wide := v.Type == TI64
	x, y := v.Args[0], v.Args[1]
	t := g.defG(v)
	if l := g.locOf(y); l.kind == lConst {
		if imm, ok := imm32(l.c, wide); ok {
			a.ImulImm(t, g.intoG(x, t), imm, wide)
			g.done(v, int(t))
			return
		}
	}
	if l := g.locOf(y); l.kind == lGPR && amd64.Reg(l.r) == t && x != y {
		x, y = y, x
	}
	g.movG(t, x)
	switch l := g.locOf(y); l.kind {
	case lGPR:
		g.imul(t, amd64.Reg(l.r), wide)
	case lSlot:
		a.ImulMem(t, rL, slotOff(l.slot), wide)
	case lConst:
		a.MovImm(s1, l.c)
		g.imul(t, s1, wide)
	}
	g.done(v, int(t))
}

func (g *gen) imul(dst, src amd64.Reg, wide bool) {
	if wide {
		g.a.Imul(dst, src)
	} else {
		g.a.Imul32(dst, src)
	}
}

// shift lowers the shifts and rotations, whose count is a constant or CL.
func (g *gen) shift(v *Value, op amd64.Shift) {
	a := g.a
	wide := v.Type == TI64
	x, y := v.Args[0], v.Args[1]
	if y.Op == OpConst {
		t := g.defG(v)
		g.movG(t, x)
		a.ShiftImm(op, t, uint8(y.Aux&shiftMask(v.Type)), wide)
		g.done(v, int(t))
		return
	}
	// CX is the count's; the allocator keeps it free across.
	g.movG(s0, x)
	g.movG(amd64.RCX, y)
	a.ShiftCL(op, s0, wide)
	t := g.defG(v)
	if t != s0 {
		a.MovRR(t, s0)
	}
	g.done(v, int(t))
}

// div lowers division and remainder, which take the dividend in AX and
// leave the quotient there and the remainder in DX; the allocator keeps
// both free across.
func (g *gen) div(v *Value) {
	a := g.a
	wide := v.Type == TI64
	x, y := v.Args[0], v.Args[1]
	const ax, dx = amd64.RAX, amd64.RDX
	d := s0
	if l := g.locOf(y); l.kind == lGPR && amd64.Reg(l.r) != ax && amd64.Reg(l.r) != dx {
		d = amd64.Reg(l.r)
	} else {
		g.movG(s0, y)
	}
	g.movG(ax, x)
	signed := v.Op == OpDivS || v.Op == OpRemS
	rem := v.Op == OpRemS || v.Op == OpRemU
	c := y.Aux
	isConst := y.Op == OpConst
	if !isConst || c == 0 || (!wide && uint32(c) == 0) {
		a.Op(amd64.Test, d, d, wide)
		code := uint64(wjit.TrapDivByZero)
		if rem {
			code = wjit.TrapRemByZero
		}
		a.Jcc(amd64.CondE, g.trap(code, v.At))
	}
	done := a.NewLabel()
	minusOne := !isConst || (wide && c == math.MaxUint64) || (!wide && uint32(c) == math.MaxUint32)
	if signed && minusOne {
		ok := a.NewLabel()
		a.OpImm(amd64.Cmp, d, -1, wide)
		a.Jcc(amd64.CondNE, ok)
		if rem {
			a.Op(amd64.Xor, dx, dx, false)
			a.Jmp(done)
		} else {
			if wide {
				a.MovImm(s1, 1<<63)
				a.Op(amd64.Cmp, ax, s1, true)
			} else {
				a.OpImm(amd64.Cmp, ax, math.MinInt32, false)
			}
			a.Jcc(amd64.CondE, g.trap(wjit.TrapDivUnrepresentable, v.At))
		}
		a.Bind(ok)
	}
	switch {
	case signed && wide:
		a.Cqo()
		a.Idiv(d)
	case signed:
		a.Cdq()
		a.Idiv32(d)
	case wide:
		a.Op(amd64.Xor, dx, dx, false)
		a.Div(d)
	default:
		a.Op(amd64.Xor, dx, dx, false)
		a.Div32(d)
	}
	a.Bind(done)
	res := ax
	if rem {
		res = dx
	}
	t := g.defG(v)
	if t != res {
		a.MovRR(t, res)
	}
	g.done(v, int(t))
}

// mulHigh lowers the high half of an i64 product, which the one-operand
// multiplication leaves in DX from a factor in AX; the allocator keeps
// both free across.
func (g *gen) mulHigh(v *Value) {
	a := g.a
	x, y := v.Args[0], v.Args[1]
	const ax, dx = amd64.RAX, amd64.RDX
	m := s0
	if l := g.locOf(y); l.kind == lGPR && amd64.Reg(l.r) != ax && amd64.Reg(l.r) != dx {
		m = amd64.Reg(l.r)
	} else {
		g.movG(s0, y)
	}
	g.movG(ax, x)
	if v.Op == OpMulHS {
		a.ImulWide(m)
	} else {
		a.Mul(m)
	}
	t := g.defG(v)
	if t != dx {
		a.MovRR(t, dx)
	}
	g.done(v, int(t))
}

func (g *gen) sse(single bool, op amd64.SSE, dst, src amd64.XReg) {
	if single {
		g.a.SSEOpS(op, dst, src)
	} else {
		g.a.SSEOp(op, dst, src)
	}
}

// floatOp lowers add, sub, mul and div, the first operand the
// instruction's destination, as the interpreter's code has it.
func (g *gen) floatOp(v *Value) {
	a := g.a
	x, y := v.Args[0], v.Args[1]
	op := floatOps[v.Op]
	single := v.Type == TF32
	t := g.defX(v)
	if l := g.locOf(y); l.kind == lXMM && amd64.XReg(l.r) == t && x != y {
		g.movX(x1, x)
		g.sse(single, op, x1, t)
		a.SSEOp(amd64.MovAPD, t, x1)
		g.done(v, int(t))
		return
	}
	g.movX(t, x)
	g.sse(single, op, t, g.intoX(y, x1))
	g.done(v, int(t))
}

// minMax lowers min and max: NaN if either operand is, -0 below +0.
func (g *gen) minMax(v *Value) {
	a := g.a
	x, y := v.Args[0], v.Args[1]
	isMax := v.Op == OpFMax
	single := v.Type == TF32
	t := g.defX(v)
	xa := t
	if l := g.locOf(y); l.kind == lXMM && amd64.XReg(l.r) == t && x != y {
		xa = x0
	}
	g.movX(xa, x)
	xb := g.intoX(y, x1)
	nan, ne, done := a.NewLabel(), a.NewLabel(), a.NewLabel()
	g.sse(single, amd64.UcomiSD, xa, xb)
	a.Jcc(amd64.CondP, nan)
	a.Jcc(amd64.CondNE, ne)
	if isMax {
		a.SSEOp(amd64.AndPD, xa, xb)
	} else {
		a.SSEOp(amd64.OrPD, xa, xb)
	}
	a.Jmp(done)
	a.Bind(ne)
	if isMax {
		a.Jcc(amd64.CondA, done)
	} else {
		a.Jcc(amd64.CondB, done)
	}
	a.SSEOp(amd64.MovAPD, xa, xb)
	a.Jmp(done)
	a.Bind(nan)
	g.sse(single, amd64.AddSD, xa, xb)
	a.Bind(done)
	if xa != t {
		a.SSEOp(amd64.MovAPD, t, xa)
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
	if v.Type == TF32 {
		a.OpImm(amd64.And, s0, 0x7fffffff, false)
		a.OpImm(amd64.And, s1, math.MinInt32, false)
		a.Op(amd64.Or, s0, s1, false)
		a.MovDToX(t, s0)
	} else {
		a.ShiftImm(amd64.Shl, s0, 1, true)
		a.ShiftImm(amd64.Shr, s0, 1, true)
		a.ShiftImm(amd64.Shr, s1, 63, true)
		a.ShiftImm(amd64.Shl, s1, 63, true)
		a.Op(amd64.Or, s0, s1, true)
		a.MovQToX(t, s0)
	}
	g.done(v, int(t))
}

func (g *gen) floatCompare(v *Value) {
	a := g.a
	x, y := v.Args[0], v.Args[1]
	single := x.Type == TF32
	xa := g.intoX(x, x0)
	xb := g.intoX(y, x1)
	t := g.defG(v)
	a.Op(amd64.Xor, t, t, false)
	switch v.Op {
	case OpFEq:
		a.Op(amd64.Xor, s1, s1, false)
		g.sse(single, amd64.UcomiSD, xa, xb)
		a.Setcc(amd64.CondE, t)
		a.Setcc(amd64.CondNP, s1)
		a.Op(amd64.And, t, s1, false)
	case OpFNe:
		a.Op(amd64.Xor, s1, s1, false)
		g.sse(single, amd64.UcomiSD, xa, xb)
		a.Setcc(amd64.CondNE, t)
		a.Setcc(amd64.CondP, s1)
		a.Op(amd64.Or, t, s1, false)
	case OpFGt:
		g.sse(single, amd64.UcomiSD, xa, xb)
		a.Setcc(amd64.CondA, t)
	case OpFGe:
		g.sse(single, amd64.UcomiSD, xa, xb)
		a.Setcc(amd64.CondAE, t)
	case OpFLt:
		g.sse(single, amd64.UcomiSD, xb, xa)
		a.Setcc(amd64.CondA, t)
	default:
		g.sse(single, amd64.UcomiSD, xb, xa)
		a.Setcc(amd64.CondAE, t)
	}
	g.done(v, int(t))
}

// intToFloat lowers the conversions of integers to floats.
func (g *gen) intToFloat(v *Value) {
	a := g.a
	x := v.Args[0]
	single := v.Type == TF32
	t := g.defX(v)
	g.movG(s0, x)
	cvt := func(wide bool) {
		if single {
			a.Cvtsi2ss(t, s0, wide)
		} else {
			a.Cvtsi2sd(t, s0, wide)
		}
	}
	switch {
	case x.Type == TI32 && v.Op == OpConvertS:
		cvt(false)
	case x.Type == TI32 || v.Op == OpConvertS:
		// An unsigned i32 is zero-extended: as an i64 it converts exactly
		// as itself.
		cvt(true)
	default:
		// At 2^63 and above, halved, its lowest bit kept as a sticky bit,
		// converted and doubled: one rounding.
		big, done := a.NewLabel(), a.NewLabel()
		a.Op(amd64.Test, s0, s0, true)
		a.Jcc(amd64.CondS, big)
		cvt(true)
		a.Jmp(done)
		a.Bind(big)
		a.MovRR(s1, s0)
		a.OpImm(amd64.And, s1, 1, false)
		a.ShiftImm(amd64.Shr, s0, 1, true)
		a.Op(amd64.Or, s0, s1, true)
		cvt(true)
		g.sse(single, amd64.AddSD, t, t)
		a.Bind(done)
	}
	g.done(v, int(t))
}

// loadXConst loads the double f into x through tmp.
func (g *gen) loadXConst(x amd64.XReg, f float64, tmp amd64.Reg) {
	g.a.MovImm(tmp, math.Float64bits(f))
	g.a.MovQToX(x, tmp)
}

// toDouble loads float v into x1 as a double.
func (g *gen) toDouble(v *Value) amd64.XReg {
	g.movX(x1, v)
	if v.Type == TF32 {
		g.a.Cvtss2sd(x1, x1)
	}
	return x1
}

// truncate lowers the trapping truncations: of a NaN, or a value out of
// the result's range, by the interpreter's bounds.
func (g *gen) truncate(v *Value) {
	a := g.a
	x := g.toDouble(v.Args[0])
	var lo, hi float64
	loIn := false
	unsigned := v.Op == OpTruncU
	switch {
	case v.Type == TI32 && !unsigned && v.Args[0].Type == TF32:
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
	trap := g.trap(wjit.TrapFloatUnrepresentable, v.At)
	g.loadXConst(x0, lo, s0)
	a.SSEOp(amd64.UcomiSD, x, x0)
	if loIn {
		a.Jcc(amd64.CondB, trap)
	} else {
		a.Jcc(amd64.CondBE, trap)
	}
	g.loadXConst(x0, hi, s0)
	a.SSEOp(amd64.UcomiSD, x, x0)
	a.Jcc(amd64.CondAE, trap)
	t := g.defG(v)
	switch {
	case v.Type == TI32 && !unsigned:
		a.Cvttsd2si32(t, x)
	case v.Type == TI32:
		a.Cvttsd2si(t, x)
		a.MovRR32(t, t)
	case !unsigned:
		a.Cvttsd2si(t, x)
	default:
		g.f64ToU64(t, x)
	}
	g.done(v, int(t))
}

// f64ToU64 converts the double in x, in [0, 2^64), to an integer in r,
// truncating; x is lost.
func (g *gen) f64ToU64(r amd64.Reg, x amd64.XReg) {
	a := g.a
	tmp := s0
	if r == s0 {
		tmp = s1
	}
	big, done := a.NewLabel(), a.NewLabel()
	g.loadXConst(x0, 9223372036854775808.0, tmp)
	a.SSEOp(amd64.UcomiSD, x, x0)
	a.Jcc(amd64.CondAE, big)
	a.Cvttsd2si(r, x)
	a.Jmp(done)
	a.Bind(big)
	a.SSEOp(amd64.SubSD, x, x0)
	a.Cvttsd2si(r, x)
	a.MovImm(tmp, 1<<63)
	a.Op(amd64.Or, r, tmp, true)
	a.Bind(done)
}

// saturate lowers the saturating truncations: NaN to 0, a value past the
// result's range to its bound.
func (g *gen) saturate(v *Value) {
	a := g.a
	x := g.toDouble(v.Args[0])
	t := g.defG(v)
	tmp := s0
	if t == s0 {
		tmp = s1
	}
	var lo, hi float64
	var loVal, hiVal uint64
	signed := v.Op == OpTruncSatS
	switch {
	case v.Type == TI32 && signed:
		lo, hi, loVal, hiVal = -2147483648.0, 2147483647.0, 0x80000000, 0x7fffffff
	case v.Type == TI32:
		lo, hi, loVal, hiVal = -1, 4294967295.0, 0, 0xffffffff
	case signed:
		lo, hi, loVal, hiVal = -9223372036854775808.0, 9223372036854775808.0, 1<<63, 1<<63-1
	default:
		lo, hi, loVal, hiVal = -1, 18446744073709551616.0, 0, math.MaxUint64
	}
	low, high, done := a.NewLabel(), a.NewLabel(), a.NewLabel()
	a.MovImm(t, 0)
	a.SSEOp(amd64.UcomiSD, x, x)
	a.Jcc(amd64.CondP, done)
	g.loadXConst(x0, lo, tmp)
	a.SSEOp(amd64.UcomiSD, x, x0)
	a.Jcc(amd64.CondBE, low)
	g.loadXConst(x0, hi, tmp)
	a.SSEOp(amd64.UcomiSD, x, x0)
	a.Jcc(amd64.CondAE, high)
	switch {
	case v.Type == TI32 && signed:
		a.Cvttsd2si32(t, x)
	case v.Type == TI32:
		a.Cvttsd2si(t, x)
		a.MovRR32(t, t)
	case signed:
		a.Cvttsd2si(t, x)
	default:
		g.f64ToU64(t, x)
	}
	a.Jmp(done)
	a.Bind(low)
	a.MovImm(t, loVal)
	a.Jmp(done)
	a.Bind(high)
	a.MovImm(t, hiVal)
	a.Bind(done)
	g.done(v, int(t))
}

// selectOp lowers select: the first operand if the condition is true.
func (g *gen) selectOp(v *Value) {
	a := g.a
	x, y, c := v.Args[0], v.Args[1], v.Args[2]
	rc := g.intoG(c, s1)
	a.Op(amd64.Test, rc, rc, false)
	// From here nothing may change the flags: moves and loads do not (a
	// v128 constant's is a load from the pool, or one that makes it of
	// registers alone).
	if v.Type.InFloatReg() {
		t := g.defX(v)
		skip := a.NewLabel()
		switch {
		case g.isX(x, t):
			a.Jcc(amd64.CondNE, skip)
			g.movX(t, y)
		case g.isX(y, t):
			a.Jcc(amd64.CondE, skip)
			g.movX(t, x)
		default:
			g.movX(t, y)
			a.Jcc(amd64.CondE, skip)
			g.movX(t, x)
		}
		a.Bind(skip)
		g.done(v, int(t))
		return
	}
	wide := v.Type == TI64
	t := g.defG(v)
	switch {
	case g.isG(x, t):
		a.Cmov(amd64.CondE, t, g.intoG(y, s1), wide)
	case g.isG(y, t):
		a.Cmov(amd64.CondNE, t, g.intoG(x, s1), wide)
	default:
		g.movG(t, y)
		a.Cmov(amd64.CondNE, t, g.intoG(x, s1), wide)
	}
	g.done(v, int(t))
}

func (g *gen) isG(v *Value, r amd64.Reg) bool {
	l := g.locOf(v)
	return l.kind == lGPR && amd64.Reg(l.r) == r
}

func (g *gen) isX(v *Value, x amd64.XReg) bool {
	l := g.locOf(v)
	return l.kind == lXMM && amd64.XReg(l.r) == x
}

// address returns the register holding a plain access's address, checking
// its end against memory 0's size unless the access needs no check.
func (g *gen) address(v *Value) amd64.Reg {
	a := g.a
	addr := v.Args[0]
	end := v.Aux + uint64(v.Size)
	var r amd64.Reg
	if addr.Op == OpConst {
		ea := uint64(uint32(addr.Aux))
		if !v.NoCheck {
			a.MovImm(s0, ea+end)
			a.CmpMem(s0, rCtx, offMemSize, true)
			a.Jcc(amd64.CondA, g.trap(wjit.TrapMemOutOfBounds, v.At))
		}
		a.MovImm(s1, ea)
		return s1
	}
	r = g.intoG(addr, s1)
	if v.Wide != 0 {
		// The check of the accesses after it too: s0 their farthest end,
		// which the stub takes back to each's.
		w := wideChecks(g.f, v)
		l := a.NewLabel()
		g.ool = append(g.ool, stub{kind: stubTrapWide, l: l, v: v})
		a.Lea(s0, r, int32(w[len(w)-1].end))
		a.CmpMem(s0, rCtx, offMemSize, true)
		a.Jcc(amd64.CondA, l)
	} else if !v.NoCheck {
		a.Lea(s0, r, int32(end))
		a.CmpMem(s0, rCtx, offMemSize, true)
		a.Jcc(amd64.CondA, g.trap(wjit.TrapMemOutOfBounds, v.At))
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
	disp := int32(v.Aux)
	if v.Type.InFloatReg() {
		t := g.defX(v)
		switch v.Type {
		case TF32:
			a.LoadSSIdx(t, rMem, r, disp)
		case TV128:
			a.LoadVecIdx(t, rMem, r, disp)
		default:
			a.LoadSDIdx(t, rMem, r, disp)
		}
		g.done(v, int(t))
		return
	}
	t := g.defG(v)
	wide := v.Type == TI64
	switch {
	case v.Size == 8:
		a.LoadIdx(t, rMem, r, 1, disp)
	case v.Size == 4 && v.Signed:
		a.LoadS32Idx(t, rMem, r, disp)
	case v.Size == 4:
		a.LoadU32Idx(t, rMem, r, disp)
	case v.Size == 2 && v.Signed:
		a.LoadS16Idx(t, rMem, r, disp, wide)
	case v.Size == 2:
		a.LoadU16Idx(t, rMem, r, disp)
	case v.Signed:
		a.LoadS8Idx(t, rMem, r, disp, wide)
	default:
		a.LoadU8Idx(t, rMem, r, disp)
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
	disp := int32(v.Aux)
	x := v.Args[1]
	if x.Type.InFloatReg() {
		xv := g.intoX(x, x0)
		switch v.Size {
		case 4:
			a.StoreSSIdx(rMem, r, disp, xv)
		case 16:
			a.StoreVecIdx(rMem, r, disp, xv)
		default:
			a.StoreSDIdx(rMem, r, 1, disp, xv)
		}
		return
	}
	rv := g.intoG(x, s0)
	switch v.Size {
	case 1:
		a.Store8Idx(rMem, r, disp, rv)
	case 2:
		a.Store16Idx(rMem, r, disp, rv)
	case 4:
		a.Store32Idx(rMem, r, disp, rv)
	default:
		a.StoreIdx(rMem, r, 1, disp, rv)
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
		g.move(g.regLoc(w, g.ra.Reg[r]), loc{kind: lSlot, slot: g.ra.Slot[r]}, w.Type)
	}
}

// stage stores a call's arguments into the slots from base, two apart,
// where Go reads them.
func (g *gen) stage(args []*Value, base int) {
	for i, x := range args {
		g.move(loc{kind: lSlot, slot: int32(base + 2*i)}, g.locOf(x), x.Type)
	}
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
		g.stage(v.Args, g.argBase)
		g.exit(wjit.ExitCall, a.NewLabel(), uint64(idx), uint64(g.argBase), uint64(v.At))
		g.takeResults(res, int32(g.argBase))
		g.reload(v)
		return
	}
	start, slow, poll := a.NewLabel(), a.NewLabel(), a.NewLabel()
	frame := int32(8 * g.req.FrameSlots)
	a.Bind(start)
	a.OpImm(amd64.Sub, rPoll, 1, true)
	a.Jcc(amd64.CondLE, poll)
	// The callee's native entry, its frame's room past this one's, and
	// an entry of the return stack. A call of the function by itself goes
	// to this code's entry: the code runs, so it is placed, and it is the
	// function's last tier. Another callee's entry waits in a register of
	// the allocator's no argument is in: at a call every other value of
	// the function is in its slot or dead.
	self := idx == g.fn
	// A call made k inlined calls deep has its entry k entries past the
	// next one, those of the frames the inlined calls would have made.
	k := int32(v.Aux2)
	link, kept := g.freeGPR(v.Args, 0)
	if !self {
		if !kept {
			link = s0
		}
		a.Load(link, rCtx, offLinks)
		a.Load(link, link, int32(8*idx))
		a.Op(amd64.Test, link, link, true)
		a.Jcc(amd64.CondE, slow)
	}
	a.Lea(s0, rL, int32(8*(g.req.FrameSlots+g.req.FrameSlotsOf(idx))))
	a.CmpMem(s0, rCtx, offChunkEnd, true)
	a.Jcc(amd64.CondA, slow)
	if need := int32(16 * g.req.RefSlotsOf(idx)); need > 0 {
		a.Load(s0, rCtx, offR)
		a.OpImm(amd64.Add, s0, g.adv()+need, true)
		a.CmpMem(s0, rCtx, offRChunkEnd, true)
		a.Jcc(amd64.CondA, slow)
	}
	rs := g.checkRS(slow, k)
	if g.sigHasRefs(v) {
		// The copies take s1, which is R10.
		g.refArgs(v, g.adv())
		rs = g.rsInto()
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
	e := k * rsEntry
	a.LeaLabel(s0, ret)
	a.Store(rs, e, s0)
	a.Store(rs, e+8, rL)
	a.MovImm(s0, wjit.Site(idx, v.At))
	a.Store(rs, e+16, s0)
	g.pushRS(k)
	a.OpImm(amd64.Add, rL, frame, true)
	switch {
	case self:
		a.Jmp(g.entry)
	case kept:
		a.JmpReg(link)
	default:
		a.Load(s0, rCtx, offLinks)
		a.Load(s0, s0, int32(8*idx))
		a.JmpReg(s0)
	}
	a.Bind(ret)
	a.OpImm(amd64.Sub, rL, frame, true)
	g.poppedRS(k)
	g.restoreR()
	g.refResults(v, g.adv())
	g.takeResults(res, int32(g.req.FrameSlots))
	a.Bind(after)
	g.reload(v)
	g.ool = append(g.ool, stub{kind: stubCall, l: poll, start: start, slow: slow, after: after, v: v, res: res, wh: g.keepWhere(v, res)})
}

// rsEntry is the bytes of a return stack entry.
const rsEntry = wjit.RSEntryWords * 8

// checkRS goes to slow where the return stack has no room for an entry k
// entries past the next one, and returns the register that holds its next
// entry: the one that keeps it, or R10 loaded from the context.
func (g *gen) checkRS(slow amd64.Label, k int32) amd64.Reg {
	rs := g.rsInto()
	at := rs
	if k > 0 {
		g.a.Lea(s0, rs, k*rsEntry)
		at = s0
	}
	g.a.CmpMem(at, rCtx, offRSEnd, true)
	g.a.Jcc(amd64.CondAE, slow)
	return rs
}

// rsInto returns the register that holds the return stack's next entry:
// the one that keeps it, or R10 loaded from the context.
func (g *gen) rsInto() amd64.Reg {
	if g.rsKept {
		return g.rs
	}
	g.a.Load(rRS, rCtx, offRSP)
	return rRS
}

// pushRS leaves in R10 the return stack's next entry past the one a call
// wrote k entries past the next, as the callee takes it.
func (g *gen) pushRS(k int32) {
	if g.rsKept {
		g.a.Lea(rRS, g.rs, (k+1)*rsEntry)
		return
	}
	g.a.OpImm(amd64.Add, rRS, (k+1)*rsEntry, true)
}

// poppedRS keeps the return stack's next entry a native callee returned
// with in R10, the entry k entries past it that the call wrote: in the
// register that keeps it, or in the context, where an exit of the callee
// left another.
func (g *gen) poppedRS(k int32) {
	if g.rsKept {
		if k > 0 {
			g.a.Lea(g.rs, rRS, -k*rsEntry)
			return
		}
		g.a.MovRR(g.rs, rRS)
		return
	}
	if k > 0 {
		g.a.Lea(rRS, rRS, -k*rsEntry)
	}
	g.a.Store(rCtx, offRSP, rRS)
}

// freeGPR returns a general register of the allocator's that none of args
// is in, that does not keep the return stack's entry and that is not in
// taken, if there is one.
func (g *gen) freeGPR(args []*Value, taken uint64) (amd64.Reg, bool) {
	used := taken
	if g.rsKept {
		used |= 1 << g.rs
	}
	for _, x := range args {
		if l := g.locOf(x); l.kind == lGPR {
			used |= 1 << l.r
		}
	}
	for _, r := range gprs {
		if used&(1<<r) == 0 {
			g.wbG |= 1 << r
			return amd64.Reg(r), true
		}
	}
	return 0, false
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
	frame := int32(8 * g.req.FrameSlots)
	a.Bind(start)
	a.OpImm(amd64.Sub, rPoll, 1, true)
	a.Jcc(amd64.CondLE, poll)
	// The callee's entry and index wait in registers of the allocator's
	// no operand is in -- the table's index too, which a call through Go
	// reads -- where there are two, while the arguments go into its
	// parameters; else the index waits in x0, and the entry is read again.
	link, kept := g.freeGPR(v.Args, 0)
	idx, keptIdx := g.freeGPR(v.Args, 1<<link)
	kept = kept && keptIdx
	g.indirectCallee(v, frame, g.adv(), slow, link, kept)
	rs := g.checkRS(slow, 0)
	if kept {
		a.MovRR(idx, s0)
	} else {
		a.MovQToX(x0, s0)
	}
	slot := int32(g.req.FrameSlots)
	for _, x := range args {
		g.move(loc{kind: lSlot, slot: slot}, g.locOf(x), x.Type)
		slot += x.Type.Slots()
	}
	g.advanceR()
	ret := a.NewLabel()
	a.LeaLabel(s0, ret)
	a.Store(rs, 0, s0)
	a.Store(rs, 8, rL)
	if kept {
		a.StoreImm32(rs, 16, int32(v.At))
		a.Store32(rs, 20, idx)
	} else {
		a.MovQFromX(s0, x0)
		a.ShiftImm(amd64.Shl, s0, 32, true)
		a.OpImm(amd64.Or, s0, int32(v.At), true)
		a.Store(rs, 16, s0)
	}
	g.pushRS(0)
	a.OpImm(amd64.Add, rL, frame, true)
	if kept {
		a.JmpReg(link)
	} else {
		a.MovQFromX(s0, x0)
		a.ShiftImm(amd64.Shl, s0, 3, true)
		a.AddMem(s0, rCtx, offLinks)
		a.Load(s0, s0, 0)
		a.JmpReg(s0)
	}
	a.Bind(ret)
	a.OpImm(amd64.Sub, rL, frame, true)
	g.poppedRS(0)
	g.restoreR()
	g.takeResults(res, int32(g.req.FrameSlots))
	a.Bind(after)
	g.reload(v)
	kind := uint8(stubCallIndirect)
	if ref {
		kind = stubCallRef
	}
	g.ool = append(g.ool, stub{kind: kind, l: poll, start: start, slow: slow, after: after, v: v, res: res, wh: g.keepWhere(v, res)})
}

// indirectCallee leaves in s0 the index of the function call_indirect,
// call_ref or their tail calls v calls, and, where keep, its native entry
// in link, or goes to slow: the table's element past its end or null, a
// function of another type or instance, without native code, or whose
// frame has no room past frame bytes of L. A reference is not null here. A
// callee that holds references fails the frame's check, and is checked
// again out of line by both its frames' sizes (Ctx.RFrames), its
// reference slots' past adv bytes of R.
func (g *gen) indirectCallee(v *Value, frame, adv int32, slow amd64.Label, link amd64.Reg, keep bool) {
	a := g.a
	m := g.req.Module
	lay := g.req.Layout
	if v.Op == OpCallRef || v.Op == OpTailRef {
		a.Load(s1, rCtx, offR)
		a.Load(s1, s1, g.calleeRefOff(v))
	} else {
		// The element, within the table and not null: a function.
		table := uint32(v.Aux2)
		g.movG(s0, v.Args[len(v.Args)-1])
		a.Load(s1, rCtx, offTables)
		a.Load(s1, s1, int32(8*table))
		a.CmpMem(s0, s1, lay.TableElems+8, true)
		a.Jcc(amd64.CondAE, slow)
		a.Load(s1, s1, lay.TableElems)
		a.ShiftImm(amd64.Shl, s0, 4, true)
		a.LoadIdx(s1, s1, s0, 1, 0)
		a.Op(amd64.Test, s1, s1, true)
		a.Jcc(amd64.CondE, slow)
	}
	// Of the call's type, defined by this instance: its index is the
	// module's.
	a.LoadU32(s0, s1, lay.FuncType)
	a.OpImm(amd64.Cmp, s0, int32(m.Canonical[v.Aux]), false)
	a.Jcc(amd64.CondNE, slow)
	a.Load(s0, s1, lay.FuncInst)
	a.CmpMem(s0, rCtx, offInst, true)
	a.Jcc(amd64.CondNE, slow)
	a.LoadU32(s0, s1, lay.FuncIndex)
	// Its native entry, and its frame's room past this one's.
	if !keep {
		link = s1
	}
	a.Load(link, rCtx, offLinks)
	a.LoadIdxScaled(link, link, s0, 3, 0)
	a.Op(amd64.Test, link, link, true)
	a.Jcc(amd64.CondE, slow)
	refs, back := a.NewLabel(), a.NewLabel()
	a.Load(s1, rCtx, offFrames)
	a.LoadIdxScaled(s1, s1, s0, 3, 0)
	a.Op(amd64.Add, s1, rL, true)
	if frame != 0 {
		a.OpImm(amd64.Add, s1, frame, true)
	}
	a.CmpMem(s1, rCtx, offChunkEnd, true)
	a.Jcc(amd64.CondA, refs)
	a.Bind(back)
	g.ool = append(g.ool, stub{kind: stubRefRoom, l: refs, to: back, slow: slow, code: uint64(frame), off: adv})
}

// bulk lowers memory.copy and memory.fill of memory 0 (BulkInCode): the
// operands into AX, CX and SI, which the allocator keeps free across, the
// ends checked against the memory's size, then REP MOVSB or REP STOSB to
// DI, the context kept in s0 meanwhile -- or, for a copy to a higher
// address that overlaps its source, a copy from the end down, words then
// bytes. Both ends are checked before a byte is written, as the
// interpreter's are.
func (g *gen) bulk(v *Value) {
	a := g.a
	const ax, cx, si = amd64.RAX, amd64.RCX, amd64.RSI
	in := func(r amd64.Reg, x *Value) pmove {
		return pmove{dst: loc{kind: lGPR, r: int(r)}, src: g.locOf(x), t: TI32}
	}
	oob := g.trap(wjit.TrapMemOutOfBounds, v.At)
	check := func(base amd64.Reg) {
		a.LeaIdx(s0, base, cx, 1, 0)
		a.CmpMem(s0, rCtx, offMemSize, true)
		a.Jcc(amd64.CondA, oob)
	}
	if opcode.Opcode(v.Aux) == opcode.MemoryFill {
		// Destination, value, count.
		g.parallel([]pmove{in(si, v.Args[0]), in(ax, v.Args[1]), in(cx, v.Args[2])})
		check(si)
		a.MovRR(s0, rCtx)
		a.LeaIdx(rCtx, si, rMem, 1, 0)
		a.RepStosb()
		a.MovRR(rCtx, s0)
		return
	}
	if n, ok := smallCopy(v); ok {
		g.copySmall(v, n, oob)
		return
	}
	// Destination, source, count.
	g.parallel([]pmove{in(ax, v.Args[0]), in(si, v.Args[1]), in(cx, v.Args[2])})
	check(si)
	check(ax)
	up, done := a.NewLabel(), a.NewLabel()
	a.Op(amd64.Cmp, ax, si, true)
	a.Jcc(amd64.CondBE, up)
	a.LeaIdx(s0, si, cx, 1, 0)
	a.Op(amd64.Cmp, ax, s0, true)
	a.Jcc(amd64.CondAE, up)
	// Down from the end: each word, then each byte, read before the bytes
	// above it are written.
	a.Op(amd64.Add, si, rMem, true)
	a.Op(amd64.Add, ax, rMem, true)
	words, bytes := a.NewLabel(), a.NewLabel()
	a.Bind(words)
	a.OpImm(amd64.Cmp, cx, 8, true)
	a.Jcc(amd64.CondB, bytes)
	a.OpImm(amd64.Sub, cx, 8, true)
	a.LoadIdx(s1, si, cx, 1, 0)
	a.StoreIdx(ax, cx, 1, 0, s1)
	a.Jmp(words)
	a.Bind(bytes)
	a.Op(amd64.Test, cx, cx, true)
	a.Jcc(amd64.CondE, done)
	a.OpImm(amd64.Sub, cx, 1, true)
	a.LoadU8Idx(s1, si, cx, 0)
	a.Store8Idx(ax, cx, 0, s1)
	a.Jmp(bytes)
	a.Bind(up)
	a.Op(amd64.Add, si, rMem, true)
	a.MovRR(s0, rCtx)
	a.LeaIdx(rCtx, ax, rMem, 1, 0)
	a.RepMovsb()
	a.MovRR(rCtx, s0)
	a.Bind(done)
}

// copyTemps is the number of float temporaries memory.copy v copies in: a
// small copy's 16-byte pieces past the two scratch registers.
func copyTemps(v *Value) int {
	n, ok := smallCopy(v)
	if !ok {
		return 0
	}
	return max(0, int((n+15)/16)-2)
}

// copySmall copies a memory.copy's constant count n, from 1 to
// maxSmallCopy bytes, once both ends are checked: every piece is loaded,
// then every piece stored, so that the copy is right however its source
// and destination overlap. Pieces of 16 bytes go through X15, X14 and the
// instruction's temporaries, the last ending at the count's end; fewer
// than 16 bytes are two pieces of the largest width within the count, one
// at each end, through CX and R11. The operands are moved into AX and SI,
// which the allocator keeps free as for every copy.
func (g *gen) copySmall(v *Value, n uint64, oob amd64.Label) {
	a := g.a
	const ax, cx, si = amd64.RAX, amd64.RCX, amd64.RSI
	g.parallel([]pmove{
		{dst: loc{kind: lGPR, r: int(ax)}, src: g.locOf(v.Args[0]), t: TI32},
		{dst: loc{kind: lGPR, r: int(si)}, src: g.locOf(v.Args[1]), t: TI32},
	})
	for _, r := range []amd64.Reg{si, ax} {
		a.Lea(s0, r, int32(n))
		a.CmpMem(s0, rCtx, offMemSize, true)
		a.Jcc(amd64.CondA, oob)
	}
	end := int32(n)
	if n >= 16 {
		var buf [maxSmallCopy / 16]int32
		offs := buf[:0]
		for off := int32(0); off+16 <= end; off += 16 {
			offs = append(offs, off)
		}
		if end%16 != 0 {
			offs = append(offs, end-16)
		}
		var xbuf [maxSmallCopy / 16]amd64.XReg
		xs := xbuf[:len(offs)]
		for i := range xs {
			switch i {
			case 0:
				xs[i] = x0
			case 1:
				xs[i] = x1
			default:
				xs[i] = amd64.XReg(g.temp(v, i-2))
			}
		}
		for i, off := range offs {
			a.LoadVecIdx(xs[i], rMem, si, off)
		}
		for i, off := range offs {
			a.StoreVecIdx(rMem, ax, off, xs[i])
		}
		return
	}
	var w int32
	switch {
	case n >= 8:
		w = 8
		a.LoadIdx(cx, rMem, si, 1, 0)
		a.LoadIdx(s0, rMem, si, 1, end-w)
		a.StoreIdx(rMem, ax, 1, 0, cx)
		a.StoreIdx(rMem, ax, 1, end-w, s0)
	case n >= 4:
		w = 4
		a.LoadU32Idx(cx, rMem, si, 0)
		a.LoadU32Idx(s0, rMem, si, end-w)
		a.Store32Idx(rMem, ax, 0, cx)
		a.Store32Idx(rMem, ax, end-w, s0)
	case n >= 2:
		w = 2
		a.LoadU16Idx(cx, rMem, si, 0)
		a.LoadU16Idx(s0, rMem, si, end-w)
		a.Store16Idx(rMem, ax, 0, cx)
		a.Store16Idx(rMem, ax, end-w, s0)
	default:
		a.LoadU8Idx(cx, rMem, si, 0)
		a.Store8Idx(rMem, ax, 0, cx)
	}
}

// fusesFloat reports whether a float comparison of op is made by the branch
// on it: gt, ge, lt and le, each one condition of UCOMISD's flags; eq and
// ne need the parity flag besides.
func fusesFloat(op Op) bool { return op == OpFGt || op == OpFGe || op == OpFLt || op == OpFLe }
