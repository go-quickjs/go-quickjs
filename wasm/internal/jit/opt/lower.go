//go:build amd64 || arm64

package opt

import (
	"fmt"
	"strings"

	"github.com/go-quickjs/go-quickjs/internal/jit/lsra"
	wjit "github.com/go-quickjs/go-quickjs/wasm/internal/jit"
	"github.com/go-quickjs/go-quickjs/wasm/internal/module"
)

// What the lowerings of both architectures share: the allocator's view of
// the function, where each value is, and the listing. Each architecture's
// gen embeds a lowering.

// trapKey is a trap stub's key: its code and its offset.
func trapKey(code uint64, at uint32) uint64 { return code<<32 | uint64(at) }

// allocScratch is the allocator's part of a workspace: the allocator, its
// description of the function, and the lists of its instructions'
// registers.
type allocScratch struct {
	al    lsra.Allocator
	lf    lsra.Func
	vregs arena[lsra.VReg]
}

// lowering is what the lowering keeps of the function it lowers, on every
// architecture.
type lowering struct {
	f   *Func
	req *wjit.Request
	fn  uint32
	ra  *lsra.Result
	// index is each block's index in layout order, by block ID.
	index []int32
	// inst is each call's instruction, as the allocator numbered it, by
	// its register's number.
	inst [][2]int32
	// fused says a comparison is made by the branch that uses it, by its
	// value ID.
	fused []bool
	// argBase is the first slot of the area at the frame's top where a
	// call through Go finds its arguments and leaves its results.
	argBase int
	// byID are the values the allocator sees by ID, byVreg by their
	// registers' numbers (Value.vreg).
	byID   []*Value
	byVreg []*Value
	// marks are what each stretch of the code was emitted for, where the
	// request asks for them (marking).
	marking bool
	marks   []wjit.Mark
	// v128 says the function has v128 values, which the poll's exit and
	// the barrier routine keep whole; temps is the first of each value's
	// temporaries' registers, by its register's number, where any value
	// has them (simdTemps).
	v128  bool
	temps []int32
}

// reset readies l, kept in a workspace, for function f, keeping the
// memory of its lists.
func (l *lowering) reset(f *Func, req *wjit.Request) {
	clear(l.byID)
	clear(l.byVreg)
	n := f.NumValues()
	*l = lowering{f: f, req: req, fn: req.Func, index: grown(l.index, int(f.nextBlock)), inst: l.inst,
		fused: grown(l.fused, n), byID: l.byID[:0], byVreg: l.byVreg[:0], marking: req.Marks, temps: l.temps[:0]}
	clear(l.fused)
}

// loc is where a value is: a register of its class, a slot, or a
// constant (a v128's high half in hi).
type loc struct {
	kind uint8
	r    int
	slot int32
	c    uint64
	hi   uint64
}

// The kinds of locations: a general register, a float register (an SSE
// register on amd64, an FP register on arm64), a slot, a constant.
const (
	lNone uint8 = iota
	lGPR
	lXMM
	lSlot
	lConst
)

// The allocator's classes: general registers and float registers.
const (
	classGPR = 0
	classXMM = 1
)

func classOf(t Type) int8 {
	switch t {
	case TI32, TI64:
		return classGPR
	case TF32, TF64, TV128:
		return classXMM
	}
	return lsra.NoClass
}

// clobbers are the registers instructions clobber, by class, where the
// architecture's instruction takes fixed registers: memory.copy's and
// memory.fill's, a division's and a shift's by a variable count, an atomic
// store's, read-modify-write's and compare-and-exchange's. The allocator
// only reads them.
type clobbers struct {
	bulk, div, shift, atomic []uint64
}

// listing prints the function as lowered: each value with its location,
// gpr and fpr naming a general and a float register.
func (g *lowering) listing(gpr, fpr func(r int) string) string {
	var sb strings.Builder
	where := func(v *Value) string {
		if v.Op == OpConst || g.fused[v.ID] || v.Type == TNone || v.Type == TTuple {
			return ""
		}
		var s string
		switch l := g.locOf(v); l.kind {
		case lGPR:
			s = gpr(l.r)
		case lXMM:
			s = fpr(l.r)
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
		case BTail:
			fmt.Fprintf(&sb, "  tail\n")
		}
	}
	return sb.String()
}

// allocate describes f to the allocator and allocates its registers, of
// classes, with the clobbers cl.
func (g *lowering) allocate(ws *allocScratch, classes [][]int, cl *clobbers) {
	f := g.f
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
	lf.Width = lf.Width[:0]
	area := 0
	for i := range lf.FixedSlot {
		lf.FixedSlot[i] = -1
		lf.Class[i] = lsra.NoClass
	}
	// A v128 takes two slots: Width is made with the function's first.
	wide := func(v *Value) {
		if v.Type != TV128 {
			return
		}
		if !g.v128 {
			g.v128 = true
			lf.Width = append(lf.Width[:0], make([]int8, len(lf.Class))...)
		}
		lf.Width[v.vreg] = 2
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
			wide(p)
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
				wide(v)
				continue
			}
			in := lsra.Inst{Uses: uses(v.Args), Hint: lsra.None}
			if c := classOf(v.Type); c != lsra.NoClass {
				lf.Class[v.vreg] = c
				in.Defs = append(ws.vregs.take(1), lsra.VReg(v.vreg))
				wide(v)
			}
			switch v.Op {
			case OpParam:
				lf.FixedSlot[v.vreg] = int32(v.Aux)
			case OpGoOp:
				if v.BulkInCode() {
					// The code copies or fills in registers of its own.
					in.Clobber = cl.bulk
					break
				}
				fallthrough
			case OpCall, OpCallIndirect, OpCallRef:
				in.Call = true
				for k := j + 1; k < len(b.Values) && b.Values[k].Op == OpResult && b.Values[k].Args[0] == v; k++ {
					in.Defs = appendA(&ws.vregs, in.Defs, lsra.VReg(b.Values[k].vreg))
				}
				nargs, nres := len(v.Args), len(in.Defs)
				if ft := g.sigOf(v); ft != nil {
					nargs, nres = len(ft.Params)+extraArgs(v.Op), len(ft.Results)
				} else {
					nargs = goOperandCount(v)
				}
				area = max(area, 2*max(nargs, nres))
				g.inst[v.vreg] = [2]int32{int32(i), int32(len(lb.Insts))}
			case OpTail, OpTailIndirect, OpTailRef:
				// A tail call through Go stages its arguments as a call
				// does.
				ft := g.sigOf(v)
				area = max(area, 2*(len(ft.Params)+extraArgs(v.Op)))
			case OpDivS, OpDivU, OpRemS, OpRemU:
				in.Clobber = cl.div
			case OpAtomicStore, OpAtomicAdd, OpAtomicSub, OpAtomicAnd, OpAtomicOr, OpAtomicXor, OpAtomicXchg, OpAtomicCmpxchg:
				in.Clobber = cl.atomic
			case OpShl, OpShrS, OpShrU, OpRotl, OpRotr:
				if v.Args[1].Op != OpConst {
					in.Clobber = cl.shift
				}
			case OpSIMD:
				if simdExits(v) {
					// Go runs it on its operands in the call area.
					in.Call = true
					area = max(area, 2*len(v.Args))
					g.inst[v.vreg] = [2]int32{int32(i), int32(len(lb.Insts))}
					break
				}
				if k := simdTemps(v); k > 0 {
					g.addTemps(lf, &in, &ws.vregs, v, k)
				}
				if a := v.Args[0]; a.Type == TV128 && v.Type == TV128 {
					in.Hint = vreg(a)
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
	if f.Refs {
		if g.req.FrameRefSlots == 0 || f.RLocals != g.req.FrameRefLocals {
			panic("wasm opt: the frame's reference slots are not the engine's")
		}
		if f.RLocals+f.MaxHeight > g.req.FrameRefSlots {
			refuse(RefuseFrame, "%d stack positions and %d reference locals in a frame of %d reference slots", f.MaxHeight, f.RLocals, g.req.FrameRefSlots)
		}
	}
	lap(PhaseDescribe)
	cfg := lsra.Config{Classes: classes, NumSlots: g.argBase}
	if Timing != nil {
		cfg.Lap = lsraLap
	}
	ra, err := ws.al.Allocate(lf, &cfg)
	if err != nil {
		refuse(RefuseFrame, "%v: %d slots", err, g.argBase)
	}
	g.ra = ra
}

// addTemps gives instruction in, value v's, k temporaries of the float
// class, numbered after every value's register.
func (g *lowering) addTemps(lf *lsra.Func, in *lsra.Inst, vregs *arena[lsra.VReg], v *Value, k int) {
	if len(g.temps) == 0 {
		g.temps = append(g.temps[:0], make([]int32, len(g.byVreg))...)
	}
	first := len(lf.Class)
	g.temps[v.vreg] = int32(first)
	in.Temps = vregs.take(k)
	for j := range k {
		lf.Class = append(lf.Class, classXMM)
		lf.FixedSlot = append(lf.FixedSlot, -1)
		if lf.Width != nil {
			lf.Width = append(lf.Width, 0)
		}
		in.Temps = append(in.Temps, lsra.VReg(first+j))
	}
}

// temp is the register of value v's k-th temporary.
func (g *lowering) temp(v *Value, k int) int {
	r := g.ra.Reg[g.temps[v.vreg]+int32(k)]
	if r < 0 {
		panic("wasm opt: a temporary without a register")
	}
	return int(r)
}

// locOf is where v is.
func (g *lowering) locOf(v *Value) loc {
	if v.Op == OpConst {
		return loc{kind: lConst, c: v.Aux, hi: v.Aux2}
	}
	if r := g.ra.Reg[v.vreg]; r >= 0 {
		if v.Type.InFloatReg() {
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

// pmove is a move of a parallel move.
type pmove struct {
	dst, src loc
	t        Type
}

// inSlot reports whether phi p's slot holds its value already where its
// block begins: every argument shares its register and slot -- the
// allocator coalesced them -- and stored itself there where it was made.
func (g *lowering) inSlot(p *Value) bool {
	for _, a := range p.Args {
		if a.Op == OpConst || g.ra.Reg[a.vreg] != g.ra.Reg[p.vreg] || g.ra.Slot[a.vreg] != g.ra.Slot[p.vreg] {
			return false
		}
	}
	return true
}

// sigOf is the signature of call or tail call v, or nil for an
// instruction Go runs.
func (g *lowering) sigOf(v *Value) *module.FuncType {
	m := g.req.Module
	switch v.Op {
	case OpCall, OpTail:
		return m.FuncType(uint32(v.Aux))
	case OpCallIndirect, OpCallRef, OpTailIndirect, OpTailRef:
		return m.Types[v.Aux].Func
	}
	return nil
}

// extraArgs is the number of operands a call or a tail call of op has past
// its arguments: call_indirect's index, call_ref's reference.
func extraArgs(op Op) int {
	switch op {
	case OpCallIndirect, OpCallRef, OpTailIndirect, OpTailRef:
		return 1
	}
	return 0
}

// argSlots is where call or tail call v's operands go in the call area from
// base, for a call through Go: each the slot of its place among the
// signature's parameters (and the index or the reference after them), two
// apart, the numbers in Args order.
func (g *lowering) argSlots(v *Value, base int, out []int32) []int32 {
	ft := g.sigOf(v)
	out = out[:0]
	for i, t := range ft.Params {
		if !t.IsRef() {
			out = append(out, int32(base+2*i))
		}
	}
	if v.Op == OpCallIndirect || v.Op == OpTailIndirect {
		out = append(out, int32(base+2*len(ft.Params)))
	}
	return out
}

// hasRefs reports whether call or tail call v moves a reference: a
// reference argument, result or callee.
func (g *lowering) hasRefs(v *Value) bool {
	ft := g.sigOf(v)
	if v.Op == OpCallRef || v.Op == OpTailRef {
		return true
	}
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

// rargs is the Ctx.RArgs of an exit of call v through Go, its operands
// from the call area at argBase: its references are in the reference
// slots of its arguments' positions.
func (g *lowering) rargs(v *Value) uint64 {
	k := int64(g.f.RLocals) + int64(v.Pos) - int64(g.argBase/2)
	return uint64(k + wjit.RArgsBias)
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

// hasV128 reports whether any of moves is of a v128.
func hasV128(moves []pmove) bool {
	for _, m := range moves {
		if m.t == TV128 {
			return true
		}
	}
	return false
}

// meets reports whether location a of a value of type at and b of bt
// share a register or a slot.
func meets(a loc, at Type, b loc, bt Type) bool {
	if a.kind != b.kind {
		return false
	}
	switch a.kind {
	case lGPR, lXMM:
		return a.r == b.r
	case lSlot:
		return a.slot < b.slot+bt.Slots() && b.slot < a.slot+at.Slots()
	}
	return false
}

// parallelV makes moves as if all at once where some are of v128s, whose
// two slots may meet another value's in part: a move waits for those whose
// sources meet its destination, and where every move waits, the source of
// one another waits for is moved to a register of scratch no move reads,
// and its readers read that. Within moves the destinations are apart, and
// two sources the same or apart. pending is a list to work in, returned.
func parallelV(moves, pending []pmove, move func(dst, src loc, t Type), scratch []loc) []pmove {
	for _, m := range moves {
		if m.dst != m.src {
			pending = append(pending, m)
		}
	}
	for len(pending) > 0 {
		progress := false
		for i := 0; i < len(pending); i++ {
			m := pending[i]
			blocked := false
			for j, o := range pending {
				if j != i && meets(o.src, o.t, m.dst, m.t) {
					blocked = true
					break
				}
			}
			if blocked {
				continue
			}
			move(m.dst, m.src, m.t)
			pending = append(pending[:i], pending[i+1:]...)
			i--
			progress = true
		}
		if progress {
			continue
		}
		var src loc
		var t Type
	find:
		for i, m := range pending {
			for j, o := range pending {
				if j != i && meets(o.src, o.t, m.dst, m.t) {
					src, t = o.src, o.t
					break find
				}
			}
		}
		tmp, ok := freeScratch(pending, scratch, t)
		if !ok {
			panic("wasm opt: a parallel move with no scratch register left")
		}
		move(tmp, src, t)
		for k := range pending {
			if pending[k].src == src {
				pending[k].src = tmp
			}
		}
	}
	return pending[:0]
}

// freeScratch is a register of scratch no pending move reads that holds a
// value of type t: an integer preferably in a general register, anything
// in a float one.
func freeScratch(pending []pmove, scratch []loc, t Type) (loc, bool) {
	for pass := range 2 {
		for _, sc := range scratch {
			if sc.kind == lGPR && (pass == 1 || !t.IsInt()) || sc.kind == lXMM && pass == 0 && t.IsInt() {
				continue
			}
			busy := false
			for _, p := range pending {
				if p.src == sc {
					busy = true
					break
				}
			}
			if !busy {
				return sc, true
			}
		}
	}
	return loc{}, false
}

// vpool is the code's v128 constants, each with the label it is emitted
// at after the code.
type vpool[L any] struct {
	keys   [][2]uint64
	labels []L
}

// reset empties the pool, keeping its lists' memory.
func (p vpool[L]) reset() vpool[L] { return vpool[L]{keys: p.keys[:0], labels: p.labels[:0]} }

// label is the label of the constant lo, hi, made with newLabel the first
// time.
func (p *vpool[L]) label(lo, hi uint64, newLabel func() L) L {
	k := [2]uint64{lo, hi}
	for i, x := range p.keys {
		if x == k {
			return p.labels[i]
		}
	}
	l := newLabel()
	p.keys = append(p.keys, k)
	p.labels = append(p.labels, l)
	return l
}
