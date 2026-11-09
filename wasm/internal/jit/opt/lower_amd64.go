package opt

import (
	"fmt"
	"math"
	"slices"
	"strings"
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

// The allocator's classes: general registers, AX, CX and DX last
// (division's and shifts'), and X0-X13.
const (
	classGPR = 0
	classXMM = 1
)

var (
	gprs    = []int{int(amd64.RSI), int(amd64.R8), int(amd64.R9), int(amd64.R13), int(amd64.RAX), int(amd64.RCX), int(amd64.RDX)}
	xmms    = []int{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13}
	classes = [][]int{gprs, xmms}
)

// The registers instructions clobber, by class: memory.copy's and
// memory.fill's AX, CX and SI, a division's AX and DX, and a shift's CX
// for its count. The allocator only reads them.
var (
	clobberBulk  = []uint64{1<<amd64.RAX | 1<<amd64.RCX | 1<<amd64.RSI}
	clobberDiv   = []uint64{1<<amd64.RAX | 1<<amd64.RDX}
	clobberShift = []uint64{1 << amd64.RCX}
)

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
	offChunkEnd  = int32(unsafe.Offsetof(wjit.Ctx{}.ChunkEnd))
	offRSP       = int32(unsafe.Offsetof(wjit.Ctx{}.RSP))
	offRSEnd     = int32(unsafe.Offsetof(wjit.Ctx{}.RSEnd))
	offPoll      = int32(unsafe.Offsetof(wjit.Ctx{}.Poll))
	offR         = int32(unsafe.Offsetof(wjit.Ctx{}.R))
	offRChunkEnd = int32(unsafe.Offsetof(wjit.Ctx{}.RChunkEnd))
	offSave      = int32(unsafe.Offsetof(wjit.Ctx{}.Save))
	offExit      = int32(unsafe.Offsetof(wjit.Ctx{}.Exit))
	offArg0      = int32(unsafe.Offsetof(wjit.Ctx{}.Arg0))
	offExitFn    = int32(unsafe.Offsetof(wjit.Ctx{}.ExitFn))
	offExitAddr  = int32(unsafe.Offsetof(wjit.Ctx{}.ExitAddr))
)

// trapKey is a trap stub's key: its code and its offset.
func trapKey(code uint64, at uint32) uint64 { return code<<32 | uint64(at) }

// genScratch is the lowering's part of a workspace: the lowering's state,
// the allocator, its description of the function, and the assembler.
type genScratch struct {
	g     gen
	al    lsra.Allocator
	lf    lsra.Func
	vregs arena[lsra.VReg]
	asm   amd64.Asm
}

// gen lowers an optimized function to amd64 code.
type gen struct {
	ws  *genScratch
	a   *amd64.Asm
	f   *Func
	req *wjit.Request
	fn  uint32
	ra  *lsra.Result
	// index is each block's index in layout order, by block ID; labels
	// is each block's label, by index.
	index  []int32
	labels []amd64.Label
	// inst is each call's instruction, as the allocator numbered it, by
	// its register's number.
	inst [][2]int32
	// fused says a comparison is made by the branch that uses it, by its
	// value ID.
	fused []bool
	// argBase is the first slot of the area at the frame's top where a
	// call through Go finds its arguments and leaves its results.
	argBase int
	ool     []func()
	// traps are the trap stubs by code and offset (trapKey), trapTo the
	// code that traps by code, polls the poll exits by block index.
	traps  u64map[amd64.Label]
	trapTo u64map[amd64.Label]
	polls  u64map[amd64.Label]
	// byID are the values the allocator sees by ID, byVreg by their
	// registers' numbers (Value.vreg).
	byID   []*Value
	byVreg []*Value
	// moves and pending are the parallel moves' lists.
	moves, pending []pmove
	// marks are what each stretch of the code was emitted for, where the
	// request asks for them (marking).
	marking bool
	marks   []wjit.Mark
}

// reset readies g, kept in a workspace, for function f, keeping the
// memory of its lists and tables.
func (g *gen) reset(ws *genScratch, f *Func, req *wjit.Request) {
	clear(g.ool[:cap(g.ool)])
	clear(g.byID)
	clear(g.byVreg)
	g.traps.reset(64)
	g.trapTo.reset(8)
	g.polls.reset(8)
	n := f.NumValues()
	*g = gen{ws: ws, a: &ws.asm, f: f, req: req, fn: req.Func, index: grown(g.index, int(f.nextBlock)), labels: g.labels[:0],
		inst: g.inst, fused: grown(g.fused, n), ool: g.ool[:0], traps: g.traps, trapTo: g.trapTo, polls: g.polls,
		byID: g.byID[:0], byVreg: g.byVreg[:0], moves: g.moves[:0], pending: g.pending[:0], marking: req.Marks}
	clear(g.fused)
	ws.asm.Reset()
}

// mark says the code from here is emitted for the tag format makes.
func (g *gen) mark(format string, args ...any) {
	if g.marking {
		g.marks = append(g.marks, wjit.Mark{Off: g.a.Len(), Tag: fmt.Sprintf(format, args...)})
	}
}

// loc is where a value is: a register of its class, a slot, or a
// constant.
type loc struct {
	kind uint8
	r    int
	slot int32
	c    uint64
}

const (
	lNone uint8 = iota
	lGPR
	lXMM
	lSlot
	lConst
)

func classOf(t Type) int8 {
	switch t {
	case TI32, TI64:
		return classGPR
	case TF32, TF64:
		return classXMM
	}
	return lsra.NoClass
}

// lower lowers f, optimized, to native code for req's function.
func lower(f *Func, req *wjit.Request, ws *workspace) *wjit.Code {
	g := &ws.gen.g
	g.reset(&ws.gen, f, req)
	for i, b := range f.Blocks {
		g.index[b.ID] = int32(i)
	}
	g.allocate()
	a := g.a
	g.mark("return stub")
	retStub := a.Len()
	a.Store(rCtx, offPoll, rPoll)
	a.Store(rCtx, offRSP, rRS)
	a.StoreImm(rCtx, offExit, wjit.ExitReturn)
	a.Ret()
	goEntry, entry := a.NewLabel(), a.NewLabel()
	a.Bind(goEntry)
	g.mark("entry")
	a.Load(rL, rCtx, offL)
	a.Load(rMem, rCtx, offMemBase)
	a.Load(rPoll, rCtx, offPoll)
	a.Load(rRS, rCtx, offRSP)
	a.Bind(entry)
	a.Store(rCtx, offRSP, rRS)
	for range f.Blocks {
		g.labels = append(g.labels, a.NewLabel())
	}
	for i, b := range f.Blocks {
		a.Bind(g.labels[i])
		g.mark("%v", b)
		g.block(i, b)
	}
	for i := 0; i < len(g.ool); i++ {
		g.ool[i]()
	}
	lap(PhaseEmit)
	out, err := a.Finish()
	lap(PhaseEncode)
	if err != nil || (req.MaxCodeBytes > 0 && len(out) > req.MaxCodeBytes) {
		refuse(RefuseSize, "%d bytes of code", a.Len())
	}
	// The assembler's buffer is the workspace's: the code is a copy.
	code := &wjit.Code{Bytes: slices.Clone(out), GoEntry: a.Offset(goEntry), Entry: a.Offset(entry), ReturnStub: retStub}
	if g.marking {
		code.Marks, code.Listing = g.marks, g.listing()
	}
	return code
}

// listing prints the function as lowered: each value with its location.
func (g *gen) listing() string {
	var sb strings.Builder
	where := func(v *Value) string {
		if v.Op == OpConst || g.fused[v.ID] || v.Type == TNone || v.Type == TTuple {
			return ""
		}
		var s string
		switch l := g.locOf(v); l.kind {
		case lGPR:
			s = [...]string{"ax", "cx", "dx", "bx", "sp", "bp", "si", "di", "r8", "r9", "r10", "r11", "r12", "r13", "r14", "r15"}[l.r]
		case lXMM:
			s = fmt.Sprintf("x%d", l.r)
		case lSlot:
			s = fmt.Sprintf("slot%d", l.slot)
		}
		if l := g.locOf(v); l.kind != lSlot && g.ra.Slot[v.vreg] >= 0 {
			s += fmt.Sprintf(" (slot%d)", g.ra.Slot[v.vreg])
		}
		return "  ; " + s
	}
	for _, b := range g.f.Blocks {
		fmt.Fprintf(&sb, "%v: preds", b)
		for _, p := range b.Preds {
			fmt.Fprintf(&sb, " %v", p)
		}
		if b.Loop != nil {
			fmt.Fprintf(&sb, " (loop %v depth %d)", b.Loop, b.Depth)
		} else if b.Depth > 0 {
			fmt.Fprintf(&sb, " (depth %d)", b.Depth)
		}
		sb.WriteByte('\n')
		for _, v := range b.Phis {
			fmt.Fprintf(&sb, "  %s%s\n", v.long(), where(v))
		}
		for _, v := range b.Values {
			fmt.Fprintf(&sb, "  %s%s\n", v.long(), where(v))
		}
		switch b.Kind {
		case BPlain:
			fmt.Fprintf(&sb, "  jump %v\n", b.Succs[0])
		case BIf:
			fmt.Fprintf(&sb, "  if %v %v %v\n", b.Control, b.Succs[0], b.Succs[1])
		case BSwitch:
			fmt.Fprintf(&sb, "  switch %v", b.Control)
			for _, i := range b.Table {
				fmt.Fprintf(&sb, " %v", b.Succs[i])
			}
			sb.WriteByte('\n')
		case BReturn:
			fmt.Fprintf(&sb, "  return %v\n", b.Results)
		case BTrap:
			fmt.Fprintf(&sb, "  trap @%d\n", b.At)
		}
	}
	return sb.String()
}

// allocate describes f to the allocator and allocates its registers.
func (g *gen) allocate() {
	f := g.f
	ws := g.ws
	ws.vregs.reset()
	// The values the allocator sees are numbered densely, in the order of
	// their IDs, so that it orders them as it would by ID: the constants
	// are made where they are used, and the IDs of the values the passes
	// removed are left out.
	byID := grown(g.byID, f.NumValues())
	clear(byID)
	for _, b := range f.Blocks {
		for _, v := range b.Phis {
			byID[v.ID] = v
		}
		for _, v := range b.Values {
			if v.Op != OpConst {
				byID[v.ID] = v
			}
		}
	}
	g.byID = byID
	byVreg := g.byVreg[:0]
	for _, v := range byID {
		if v != nil {
			v.vreg = int32(len(byVreg))
			byVreg = append(byVreg, v)
		}
	}
	g.byVreg = byVreg
	n := len(byVreg)
	lf := &ws.lf
	lf.Class = grown(lf.Class, n)
	lf.FixedSlot = grown(lf.FixedSlot, n)
	lf.Blocks = grown(lf.Blocks, len(f.Blocks))
	area := 0
	for i := range lf.FixedSlot {
		lf.FixedSlot[i] = -1
		lf.Class[i] = lsra.NoClass
	}
	vreg := func(v *Value) lsra.VReg {
		if v.Op == OpConst {
			return lsra.None
		}
		return lsra.VReg(v.vreg)
	}
	uses := func(vs []*Value) []lsra.VReg {
		out := ws.vregs.take(len(vs))
		for _, v := range vs {
			if r := vreg(v); r != lsra.None {
				out = append(out, r)
			}
		}
		if len(out) == 0 {
			return nil
		}
		return out
	}
	g.inst = grown(g.inst, n)
	for i := range g.inst {
		g.inst[i] = [2]int32{-1, -1}
	}
	for i, b := range f.Blocks {
		lb := &lf.Blocks[i]
		// The block's lists keep their arrays from the last function.
		*lb = lsra.Block{Preds: lb.Preds[:0], Succs: lb.Succs[:0], Depth: int(b.Depth), Phis: lb.Phis[:0], Insts: lb.Insts[:0]}
		for _, p := range b.Preds {
			lb.Preds = append(lb.Preds, int(g.index[p.ID]))
		}
		for _, s := range b.Succs {
			lb.Succs = append(lb.Succs, int(g.index[s.ID]))
		}
		for _, p := range b.Phis {
			lf.Class[p.vreg] = classOf(p.Type)
			ph := lsra.Phi{Def: lsra.VReg(p.vreg), Args: ws.vregs.take(len(p.Args))}
			for _, a := range p.Args {
				ph.Args = append(ph.Args, vreg(a))
			}
			lb.Phis = append(lb.Phis, ph)
		}
		// A comparison that is the block's last value and its branch's
		// condition alone is made by the branch.
		if b.Kind == BIf && len(b.Values) > 0 {
			c := b.Control
			last := b.Values[len(b.Values)-1]
			if c == last && c.Uses == 1 && (c.Op == OpEqz || (c.Op >= OpEq && c.Op <= OpGeU)) {
				g.fused[c.ID] = true
			}
		}
		for j := 0; j < len(b.Values); j++ {
			v := b.Values[j]
			switch {
			case v.Op == OpConst || g.fused[v.ID]:
				continue
			case v.Op == OpResult:
				// Defined by its call.
				lf.Class[v.vreg] = classOf(v.Type)
				continue
			}
			in := lsra.Inst{Uses: uses(v.Args), Hint: lsra.None}
			if c := classOf(v.Type); c != lsra.NoClass {
				lf.Class[v.vreg] = c
				in.Defs = append(ws.vregs.take(1), lsra.VReg(v.vreg))
			}
			switch v.Op {
			case OpParam:
				lf.FixedSlot[v.vreg] = int32(v.Aux)
			case OpGoOp:
				if v.BulkInCode() {
					// The code copies or fills in AX, CX and SI.
					in.Clobber = clobberBulk
					break
				}
				fallthrough
			case OpCall, OpCallIndirect:
				in.Call = true
				for k := j + 1; k < len(b.Values) && b.Values[k].Op == OpResult && b.Values[k].Args[0] == v; k++ {
					in.Defs = appendA(&ws.vregs, in.Defs, lsra.VReg(b.Values[k].vreg))
				}
				nres := len(in.Defs)
				if v.Op == OpCall {
					nres = len(g.req.Module.FuncType(uint32(v.Aux)).Results)
				} else if v.Op == OpCallIndirect {
					nres = len(g.req.Module.Types[v.Aux].Func.Results)
				}
				area = max(area, 2*max(len(v.Args), nres))
				g.inst[v.vreg] = [2]int32{int32(i), int32(len(lb.Insts))}
			case OpDivS, OpDivU, OpRemS, OpRemU:
				in.Clobber = clobberDiv
			case OpShl, OpShrS, OpShrU, OpRotl, OpRotr:
				if v.Args[1].Op != OpConst {
					in.Clobber = clobberShift
				}
			}
			switch v.Op {
			case OpAdd, OpSub, OpMul, OpAnd, OpOr, OpXor, OpFAdd, OpFSub, OpFMul, OpFDiv, OpFMin, OpFMax,
				OpFAbs, OpFNeg, OpFSqrt, OpFCeil, OpFFloor, OpFTrunc, OpFNearest, OpPromote, OpDemote,
				OpExtendS, OpExtendU, OpExtend8S, OpExtend16S, OpExtend32S, OpWrap:
				if r := vreg(v.Args[0]); r != lsra.None && classOf(v.Args[0].Type) == classOf(v.Type) {
					in.Hint = r
				}
			}
			lb.Insts = append(lb.Insts, in)
		}
		// The block's end: its branch's condition or its results.
		var end lsra.Inst
		end.Hint = lsra.None
		switch b.Kind {
		case BIf, BSwitch:
			if g.fused[b.Control.ID] {
				end.Uses = uses(b.Control.Args)
			} else {
				end.Uses = uses([]*Value{b.Control})
			}
		case BReturn:
			end.Uses = uses(b.Results)
		}
		lb.Insts = append(lb.Insts, end)
	}
	frame := g.req.FrameSlots
	g.argBase = frame - area
	if g.argBase < len(f.Params) {
		refuse(RefuseFrame, "a frame of %d slots for %d parameters and calls of %d", frame, len(f.Params), area)
	}
	lap(PhaseDescribe)
	cfg := lsra.Config{Classes: classes, NumSlots: g.argBase}
	if Timing != nil {
		cfg.Lap = lsraLap
	}
	ra, err := g.ws.al.Allocate(lf, &cfg)
	if err != nil {
		refuse(RefuseFrame, "%v: %d slots", err, g.argBase)
	}
	g.ra = ra
}

// locOf is where v is.
func (g *gen) locOf(v *Value) loc {
	if v.Op == OpConst {
		return loc{kind: lConst, c: v.Aux}
	}
	if r := g.ra.Reg[v.vreg]; r >= 0 {
		if v.Type.IsFloat() {
			return loc{kind: lXMM, r: int(r)}
		}
		return loc{kind: lGPR, r: int(r)}
	}
	if s := g.ra.Slot[v.vreg]; s >= 0 {
		return loc{kind: lSlot, slot: s}
	}
	panic("wasm opt: a value with no location")
}

func slotOff(s int32) int32 { return 8 * s }

// The moves between locations, of a value of type t. A slot's word holds
// an i32 or an f32 with its upper half clear, as the interpreter's frames
// do: Go reads the slots of calls through it.

// move copies src to dst; s0 is its scratch.
func (g *gen) move(dst, src loc, t Type) {
	a := g.a
	if dst == src {
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
			a.MovImm(s0, c)
			if t == TF32 {
				a.MovDToX(x, s0)
			} else {
				a.MovQToX(x, s0)
			}
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

// pmove is a move of a parallel move.
type pmove struct {
	dst, src loc
	t        Type
}

// parallel makes the moves as if all at once: a move waits for those that
// read its destination, and a cycle is broken through s1 or x1.
func (g *gen) parallel(moves []pmove) {
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
	if r := g.ra.Reg[v.vreg]; r >= 0 {
		return amd64.Reg(r)
	}
	return s0
}

func (g *gen) defX(v *Value) amd64.XReg {
	if r := g.ra.Reg[v.vreg]; r >= 0 {
		return amd64.XReg(r)
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
	if v.Type.IsFloat() {
		src.kind = lXMM
	}
	if g.marking {
		g.mark("%v spill", v)
	}
	g.move(loc{kind: lSlot, slot: s}, src, v.Type)
}

// inSlot reports whether phi p's slot holds its value already where its
// block begins: every argument shares its register and slot -- the
// allocator coalesced them -- and stored itself there where it was made.
func (g *gen) inSlot(p *Value) bool {
	for _, a := range p.Args {
		if a.Op == OpConst || g.ra.Reg[a.vreg] != g.ra.Reg[p.vreg] || g.ra.Slot[a.vreg] != g.ra.Slot[p.vreg] {
			return false
		}
	}
	return true
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
		g.ool = append(g.ool, func() {
			a.Bind(to)
			g.mark("cold trap")
			a.Store(rCtx, offL, rL)
			g.storeCtx(offExit, wjit.ExitTrap)
			g.storeCtx(offArg0, code)
			g.storeCtx(offExitFn, uint64(g.fn))
			a.Ret()
		})
	}
	g.ool = append(g.ool, func() {
		a.Bind(l)
		g.mark("cold trap")
		g.storeCtx(offArg0+8, uint64(at))
		a.Jmp(to)
	})
	return l
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
}

// saveAll and restoreAll keep every allocatable register in the context's
// save area across an exit that is not a call: a poll's.
func (g *gen) saveAll() {
	a := g.a
	for i, r := range gprs {
		a.Store(rCtx, offSave+int32(8*i), amd64.Reg(r))
	}
	for i, x := range xmms {
		a.StoreSD(rCtx, offSave+int32(8*(len(gprs)+i)), amd64.XReg(x))
	}
}

func (g *gen) restoreAll() {
	a := g.a
	for i, r := range gprs {
		a.Load(amd64.Reg(r), rCtx, offSave+int32(8*i))
	}
	for i, x := range xmms {
		a.LoadSD(amd64.XReg(x), rCtx, offSave+int32(8*(len(gprs)+i)))
	}
}

// pollExit is out-of-line code that exits for a poll, every register kept,
// and goes to back.
func (g *gen) pollExit(back amd64.Label) amd64.Label {
	a := g.a
	l := a.NewLabel()
	g.ool = append(g.ool, func() {
		a.Bind(l)
		g.mark("cold poll")
		g.saveAll()
		g.exit(wjit.ExitPoll, a.NewLabel())
		g.restoreAll()
		a.Jmp(back)
	})
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

// edgeMoves moves the arguments of s's phis for the edge from b.
func (g *gen) edgeMoves(b, s *Block) {
	if len(s.Phis) == 0 {
		return
	}
	k := s.predIndex(b)
	g.mark("edge %v->%v", b, s)
	moves := g.moves[:0]
	for _, p := range s.Phis {
		moves = append(moves, pmove{dst: g.locOf(p), src: g.locOf(p.Args[k]), t: p.Type})
	}
	g.parallel(moves)
	g.moves = moves[:0]
}

// block lowers block b, the i-th.
func (g *gen) block(i int, b *Block) {
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
		g.mark("%v end %v", b, [...]string{"Plain", "If", "Switch", "Return", "Trap"}[b.Kind])
	}
	switch b.Kind {
	case BPlain:
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
			moves = append(moves, pmove{dst: loc{kind: lSlot, slot: int32(k)}, src: g.locOf(r), t: r.Type})
		}
		g.parallel(moves)
		g.moves = moves[:0]
		a.Load(rRS, rCtx, offRSP)
		a.OpImm(amd64.Sub, rRS, wjit.RSEntryWords*8, true)
		a.Load(rL, rRS, 8)
		a.JmpMem(rRS, 0)
	case BTrap:
		a.Jmp(g.trap(wjit.TrapUnreachable, b.At))
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
		if r := g.ra.Reg[v.vreg]; r >= 0 {
			dst := loc{kind: lGPR, r: int(r)}
			if v.Type.IsFloat() {
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
		a.MovImm(s0, m)
		a.MovQToX(x1, s0)
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
		if v.Type.IsFloat() {
			t := g.defX(v)
			if v.Type == TF32 {
				a.LoadSS(t, p, 0)
			} else {
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
		p := g.intoG(v.Args[0], s0)
		x := v.Args[1]
		switch {
		case x.Type == TF64:
			a.StoreSD(p, 0, g.intoX(x, x0))
		case x.Type == TF32:
			a.StoreSS(p, 0, g.intoX(x, x0))
			a.StoreImm32(p, 4, 0)
		default:
			a.Store(p, 0, g.intoG(x, s1))
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
	// From here nothing may change the flags: moves and loads do not.
	if v.Type.IsFloat() {
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

// address returns the register holding a memory access's address, checking
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
	if !v.NoCheck {
		a.Lea(s0, r, int32(end))
		a.CmpMem(s0, rCtx, offMemSize, true)
		a.Jcc(amd64.CondA, g.trap(wjit.TrapMemOutOfBounds, v.At))
	}
	return r
}

func (g *gen) load(v *Value) {
	a := g.a
	r := g.address(v)
	disp := int32(v.Aux)
	if v.Type.IsFloat() {
		t := g.defX(v)
		if v.Type == TF32 {
			a.LoadSSIdx(t, rMem, r, disp)
		} else {
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
	a := g.a
	r := g.address(v)
	disp := int32(v.Aux)
	x := v.Args[1]
	if x.Type.IsFloat() {
		xv := g.intoX(x, x0)
		if v.Size == 4 {
			a.StoreSSIdx(rMem, r, disp, xv)
		} else {
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

// results are the values of call v's results, which follow it.
func results(v *Value, rest []*Value) []*Value {
	n := 0
	for _, r := range rest {
		if r.Op != OpResult || r.Args[0] != v {
			break
		}
		n++
	}
	return rest[:n:n]
}

// takeResults moves a call's results from the slots from base, where the
// callee left them, to their values' locations.
func (g *gen) takeResults(res []*Value, base int32) {
	for _, r := range res {
		src := loc{kind: lSlot, slot: base + 2*int32(r.Aux)}
		if base >= int32(g.req.FrameSlots) {
			// The callee's frame, its results one slot apart.
			src.slot = base + int32(r.Aux)
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
	// an entry of the return stack.
	a.Load(s0, rCtx, offLinks)
	a.Load(s0, s0, int32(8*idx))
	a.Op(amd64.Test, s0, s0, true)
	a.Jcc(amd64.CondE, slow)
	a.Lea(s0, rL, int32(8*(g.req.FrameSlots+g.req.FrameSlotsOf(idx))))
	a.CmpMem(s0, rCtx, offChunkEnd, true)
	a.Jcc(amd64.CondA, slow)
	if need := int32(16 * g.req.RefSlotsOf(idx)); need > 0 {
		a.Load(s0, rCtx, offR)
		a.OpImm(amd64.Add, s0, need, true)
		a.CmpMem(s0, rCtx, offRChunkEnd, true)
		a.Jcc(amd64.CondA, slow)
	}
	a.Load(rRS, rCtx, offRSP)
	a.CmpMem(rRS, rCtx, offRSEnd, true)
	a.Jcc(amd64.CondAE, slow)
	// The arguments, into the callee's parameters: one slot each.
	for i, x := range v.Args {
		g.move(loc{kind: lSlot, slot: int32(g.req.FrameSlots + i)}, g.locOf(x), x.Type)
	}
	ret := a.NewLabel()
	a.LeaLabel(s0, ret)
	a.Store(rRS, 0, s0)
	a.Store(rRS, 8, rL)
	a.MovImm(s0, wjit.Site(idx, v.At))
	a.Store(rRS, 16, s0)
	a.OpImm(amd64.Add, rRS, wjit.RSEntryWords*8, true)
	a.OpImm(amd64.Add, rL, frame, true)
	a.Load(s0, rCtx, offLinks)
	a.Load(s0, s0, int32(8*idx))
	a.JmpReg(s0)
	a.Bind(ret)
	a.Store(rCtx, offRSP, rRS)
	g.takeResults(res, int32(g.req.FrameSlots))
	a.Bind(after)
	g.reload(v)
	g.ool = append(g.ool, func() {
		a.Bind(poll)
		g.mark("cold call")
		g.saveAll()
		g.exit(wjit.ExitPoll, a.NewLabel())
		g.restoreAll()
		a.Jmp(start)
		a.Bind(slow)
		g.stage(v.Args, g.argBase)
		g.exit(wjit.ExitCall, a.NewLabel(), uint64(idx), uint64(g.argBase), uint64(v.At))
		g.takeResults(res, int32(g.argBase))
		a.Jmp(after)
	})
}

// callGo lowers what Go runs for the code: call_indirect, and the
// instructions it leaves to Go.
func (g *gen) callGo(v *Value, rest []*Value) {
	a := g.a
	g.stage(v.Args, g.argBase)
	resume := a.NewLabel()
	base := uint64(g.argBase)
	if v.Op == OpCallIndirect {
		g.exit(wjit.ExitCallIndirect, resume, v.Aux, v.Aux2, base, uint64(v.At))
		g.takeResults(results(v, rest), int32(g.argBase))
	} else {
		op := opcode.Opcode(v.Aux)
		x, y := v.Aux2, uint64(0)
		if op == opcode.MemoryCopy {
			x, y = v.Aux2&0xffffffff, v.Aux2>>32
		}
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

// callIndirect lowers call_indirect: natively where the table's element is
// a function of the call's type exactly, defined by the instance the code
// runs for, with native code and room for its frame; else through Go,
// which finds the callee, or traps, as the interpreter does.
func (g *gen) callIndirect(v *Value, rest []*Value) {
	m := g.req.Module
	typeIndex, table := uint32(v.Aux), uint32(v.Aux2)
	lay := g.req.Layout
	if lay == nil || int(typeIndex) >= len(m.Canonical) || v.At > math.MaxInt32 || m.Tables[table].Limits.Is64 {
		g.callGo(v, rest)
		return
	}
	a := g.a
	res := results(v, rest)
	args, index := v.Args[:len(v.Args)-1], v.Args[len(v.Args)-1]
	start, slow, poll, after := a.NewLabel(), a.NewLabel(), a.NewLabel(), a.NewLabel()
	frame := int32(8 * g.req.FrameSlots)
	a.Bind(start)
	a.OpImm(amd64.Sub, rPoll, 1, true)
	a.Jcc(amd64.CondLE, poll)
	// The element, within the table and not null: a function.
	g.movG(s0, index)
	a.Load(s1, rCtx, offTables)
	a.Load(s1, s1, int32(8*table))
	a.CmpMem(s0, s1, lay.TableElems+8, true)
	a.Jcc(amd64.CondAE, slow)
	a.Load(s1, s1, lay.TableElems)
	a.ShiftImm(amd64.Shl, s0, 4, true)
	a.LoadIdx(s1, s1, s0, 1, 0)
	a.Op(amd64.Test, s1, s1, true)
	a.Jcc(amd64.CondE, slow)
	// Of the call's type, defined by this instance: its index is the
	// module's.
	a.LoadU32(s0, s1, lay.FuncType)
	a.OpImm(amd64.Cmp, s0, int32(m.Canonical[typeIndex]), false)
	a.Jcc(amd64.CondNE, slow)
	a.Load(s0, s1, lay.FuncInst)
	a.CmpMem(s0, rCtx, offInst, true)
	a.Jcc(amd64.CondNE, slow)
	a.LoadU32(s0, s1, lay.FuncIndex)
	// Its native entry, its frame's room past this one's, and an entry of
	// the return stack.
	a.Load(s1, rCtx, offLinks)
	a.LoadIdxScaled(s1, s1, s0, 3, 0)
	a.Op(amd64.Test, s1, s1, true)
	a.Jcc(amd64.CondE, slow)
	a.Load(s1, rCtx, offFrames)
	a.LoadIdxScaled(s1, s1, s0, 3, 0)
	a.Op(amd64.Add, s1, rL, true)
	a.OpImm(amd64.Add, s1, frame, true)
	a.CmpMem(s1, rCtx, offChunkEnd, true)
	a.Jcc(amd64.CondA, slow)
	a.Load(rRS, rCtx, offRSP)
	a.CmpMem(rRS, rCtx, offRSEnd, true)
	a.Jcc(amd64.CondAE, slow)
	// The callee's index waits in x0 while the arguments go into its
	// parameters, one slot each, the moves taking s0.
	a.MovQToX(x0, s0)
	for i, x := range args {
		g.move(loc{kind: lSlot, slot: int32(g.req.FrameSlots + i)}, g.locOf(x), x.Type)
	}
	ret := a.NewLabel()
	a.LeaLabel(s0, ret)
	a.Store(rRS, 0, s0)
	a.Store(rRS, 8, rL)
	a.MovQFromX(s0, x0)
	a.ShiftImm(amd64.Shl, s0, 32, true)
	a.OpImm(amd64.Or, s0, int32(v.At), true)
	a.Store(rRS, 16, s0)
	a.OpImm(amd64.Add, rRS, wjit.RSEntryWords*8, true)
	a.OpImm(amd64.Add, rL, frame, true)
	a.MovQFromX(s0, x0)
	a.ShiftImm(amd64.Shl, s0, 3, true)
	a.AddMem(s0, rCtx, offLinks)
	a.Load(s0, s0, 0)
	a.JmpReg(s0)
	a.Bind(ret)
	a.Store(rCtx, offRSP, rRS)
	g.takeResults(res, int32(g.req.FrameSlots))
	a.Bind(after)
	g.reload(v)
	g.ool = append(g.ool, func() {
		a.Bind(poll)
		g.mark("cold call")
		g.saveAll()
		g.exit(wjit.ExitPoll, a.NewLabel())
		g.restoreAll()
		a.Jmp(start)
		a.Bind(slow)
		g.stage(v.Args, g.argBase)
		g.exit(wjit.ExitCallIndirect, a.NewLabel(), v.Aux, v.Aux2, uint64(g.argBase), uint64(v.At))
		g.takeResults(res, int32(g.argBase))
		a.Jmp(after)
	})
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
