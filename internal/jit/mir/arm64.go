package mir

import (
	"fmt"
	"math"
	"slices"
	"unsafe"

	"github.com/go-quickjs/go-quickjs/internal/jit/abi"
	"github.com/go-quickjs/go-quickjs/internal/jit/asm/arm64"
	"github.com/go-quickjs/go-quickjs/internal/jit/ir"
	"github.com/go-quickjs/go-quickjs/internal/jit/ssa"
)

// arm64's fixed registers. SP, X18 (the platform's), X28 (Go's g), X29 (the
// frame pointer) and X30 (the link register) are never touched; X27 is the
// encoder's temporary.
const (
	a64Ctx    = arm64.Reg(0)
	a64Locals = arm64.Reg(1)
	a64Stack  = arm64.Reg(2)
	a64A      = arm64.Reg(15)
	a64B      = arm64.Reg(16)
	a64C      = arm64.Reg(17)
	a64D      = arm64.Reg(26)
	a64F0     = arm64.FReg(0)
	a64F1     = arm64.FReg(1)
	a64F2     = arm64.FReg(2)
)

// a64Compiler is arm64's code generator over the shared core. It follows
// amd64's (amd64.go) step for step; where the two differ, the comments say
// why.
type a64Compiler struct {
	*core
	a *arm64.Asm
	// consts are the words every context holds (abi.Context's Consts).
	consts  [abi.NumConsts]uint64
	labels  []arm64.Label // by block ID
	stubs   map[stubKey]arm64.Label
	stubAt  []int
	stubFor []a64Stub
	cold    []func()
	// callV and callTop are amd64's.
	callV   *ssa.Value
	callTop arm64.Label
	// tail is where exits that filled records go (recordsTail), if any
	// does; recorded marks the exit being emitted as one.
	tail               arm64.Label
	tailUsed, recorded bool
	// table is where exits that leave their frames to Go go (tableExit),
	// if any does, with exits their descriptions.
	table     arm64.Label
	tableUsed bool
	exits     []*abi.ExitDescriptor
	// enter is where native calls to callees with no native code go
	// (enterExit), if any does.
	enter     arm64.Label
	enterUsed bool
	// saving is the routine every call of Go that saves every register
	// shares (goSaving), if any makes one.
	saving     arm64.Label
	savingUsed bool
	// storing is the routine the stores Go makes share (goStore), as
	// amd64's.
	storing     arm64.Label
	storingUsed bool
}

type a64Stub struct {
	key   stubKey
	label arm64.Label
}

// CompileARM64 compiles f for arm64, given the VM's value encoding.
func CompileARM64(f *ssa.Func, enc abi.Encoding) (*Code, error) { return compileARM64(nil, f, enc) }

// arm64GPRs and arm64FPRs are the registers arm64's allocator assigns,
// never changed.
var arm64GPRs, arm64FPRs = func() (gprs, fprs []int) {
	for r := 3; r <= 14; r++ {
		gprs = append(gprs, r)
	}
	for r := 19; r <= 25; r++ {
		gprs = append(gprs, r)
	}
	for r := 3; r <= 31; r++ {
		fprs = append(fprs, r)
	}
	return gprs, fprs
}()

func compileARM64(w *Workspace, f *ssa.Func, enc abi.Encoding) (code *Code, err error) {
	defer func() {
		if v := recover(); v != nil {
			code, err = nil, fmt.Errorf("%w: %v", ErrUnsupported, v)
		}
	}()
	k, err := prepare(w, f, enc, arm64GPRs, arm64FPRs)
	if err != nil {
		return nil, err
	}
	c := &a64Compiler{core: k, consts: enc.ConstWords()}
	c.stubAt = k.ints(2 * f.NumStates())
	if w != nil {
		g := &w.arm64
		if g.stubs == nil {
			g.stubs = map[stubKey]arm64.Label{}
		}
		g.a.Reset()
		clear(g.stubs)
		c.a, c.stubs, c.stubFor, c.cold = &g.a, g.stubs, g.stubFor[:0], g.cold[:0]
		c.labels = slices.Grow(g.labels[:0], numBlocks(f))[:numBlocks(f)]
		defer func() { g.labels, g.stubFor, g.cold = c.labels[:0], c.stubFor[:0], clearFuncs(c.cold) }()
	} else {
		c.a, c.stubs = &arm64.Asm{}, map[stubKey]arm64.Label{}
		c.labels = make([]arm64.Label, numBlocks(f))
	}
	entries := map[int]int{}
	for _, b := range c.order {
		c.labels[b.ID] = c.a.NewLabel()
	}
	for i, b := range c.order {
		c.a.Bind(c.labels[b.ID])
		if b.PC < 0 {
			for _, e := range f.Entries {
				if e.Block == b {
					entries[e.PC] = c.a.Len()
				}
			}
		}
		var next *ssa.Block
		if i+1 < len(c.order) {
			next = c.order[i+1]
		}
		c.block(b, next)
	}
	for i := 0; i < len(c.stubFor); i++ {
		s := c.stubFor[i]
		c.a.Bind(s.label)
		c.exitTo(s.key.state, s.key.kind)
	}
	stubs := len(c.stubFor)
	for i := 0; i < len(c.cold); i++ {
		c.cold[i]()
	}
	// Cold code's guards' exits.
	for i := stubs; i < len(c.stubFor); i++ {
		s := c.stubFor[i]
		c.a.Bind(s.label)
		c.exitTo(s.key.state, s.key.kind)
	}
	// Last: cold code's exits may come here too.
	if c.tailUsed {
		c.a.Bind(c.tail)
		c.recordsTail()
	}
	if c.tableUsed {
		c.a.Bind(c.table)
		c.tableExit()
	}
	if c.enterUsed {
		c.a.Bind(c.enter)
		c.enterExit()
	}
	if c.storingUsed {
		c.a.Bind(c.storing)
		c.storingCall()
	}
	if c.savingUsed {
		c.a.Bind(c.saving)
		c.savingCall()
	}
	bytes, err := c.a.Finish()
	if err != nil {
		return nil, err
	}
	return &Code{Bytes: bytes, Entries: entries, Exits: c.exits, core: c.core}, nil
}

// slotAddr is the base register and displacement of a frame slot's word.
func (c *a64Compiler) slotAddr(slot int, ref bool, scratch arm64.Reg) (arm64.Reg, int32) {
	off := c.enc.NumOffset
	if ref {
		off = c.enc.RefOffset
	}
	switch {
	case slot < c.f.FrameLocals:
		return a64Locals, int32(slot)*c.enc.ValueSize + off
	case slot == c.f.ThisSlot:
		return a64Ctx, abi.OffThis + off
	case slot < c.f.Locals:
		c.a.Load(scratch, a64Ctx, abi.OffUpvalues)
		c.a.Load(scratch, scratch, int32(slot-c.f.FrameLocals)*8)
		c.a.Load(scratch, scratch, c.enc.UpvalueSlot)
		return scratch, off
	}
	return a64Stack, int32(slot-c.f.Locals)*c.enc.ValueSize + off
}

// slotSource puts in dst a slot's source, as amd64's does.
func (c *a64Compiler) slotSource(slot int, dst arm64.Reg) {
	base, disp := c.slotAddr(slot, false, dst)
	c.a.AddImm(dst, base, int64(disp-c.enc.NumOffset), true)
}

// gpr returns a register holding v's word, loading a spilled or lazy value
// into scratch.
func (c *a64Compiler) gpr(v *ssa.Value, scratch arm64.Reg) arm64.Reg {
	if c.isLazy(v) {
		c.materialize(v, scratch)
		return scratch
	}
	l, ok := c.loc(v)
	if !ok {
		panic(fmt.Sprintf("no location for %v (%v)", v, v.Op))
	}
	if l.reg >= 0 {
		return arm64.Reg(l.reg)
	}
	c.a.Load(scratch, a64Ctx, c.spillDisp(l.spill))
	return scratch
}

// fpr returns a floating-point register holding v.
func (c *a64Compiler) fpr(v *ssa.Value, scratch arm64.FReg) arm64.FReg {
	l, ok := c.loc(v)
	if !ok {
		panic(fmt.Sprintf("no location for %v (%v)", v, v.Op))
	}
	if l.reg >= 0 {
		return arm64.FReg(l.reg)
	}
	c.a.LoadF(scratch, a64Ctx, c.spillDisp(l.spill))
	return scratch
}

// gdst and fdst are the register an instruction computing v writes: v's
// own, or scratch A or F0 when v is spilled, which setG and setF then
// store. A64's instructions read their operands before they write, so the
// register may be an operand's.
func (c *a64Compiler) gdst(v *ssa.Value) arm64.Reg {
	if l := c.locAt(v); l.reg >= 0 {
		return arm64.Reg(l.reg)
	}
	return a64A
}

// gprInto puts v's word in dst, as amd64's does.
func (c *a64Compiler) gprInto(v *ssa.Value, dst arm64.Reg) {
	if c.isLazy(v) {
		c.materialize(v, dst)
		return
	}
	if l := c.locAt(v); l.reg >= 0 {
		if arm64.Reg(l.reg) != dst {
			c.a.MovRR(dst, arm64.Reg(l.reg))
		}
		return
	}
	c.a.Load(dst, a64Ctx, c.spillDisp(c.locAt(v).spill))
}

func (c *a64Compiler) fdst(v *ssa.Value) arm64.FReg {
	if l := c.locAt(v); l.reg >= 0 {
		return arm64.FReg(l.reg)
	}
	return a64F0
}

// constF64 puts a double's bits in dst: from the zero register, as an
// FMOV immediate, or through A.
func (c *a64Compiler) constF64(dst arm64.FReg, bits uint64) {
	if bits == 0 {
		c.a.FMovToF(dst, arm64.ZR)
		return
	}
	if imm, ok := arm64.FloatImm(bits); ok {
		c.a.FMovImm(dst, imm)
		return
	}
	c.movImm(a64A, bits)
	c.a.FMovToF(dst, a64A)
}

// setG stores src into v's location.
func (c *a64Compiler) setG(v *ssa.Value, src arm64.Reg) {
	l := c.locAt(v)
	if l.reg >= 0 {
		if arm64.Reg(l.reg) != src {
			c.a.MovRR(arm64.Reg(l.reg), src)
		}
		return
	}
	c.a.Store(a64Ctx, c.spillDisp(l.spill), src)
}

// setF stores src into v's location.
func (c *a64Compiler) setF(v *ssa.Value, src arm64.FReg) {
	l := c.locAt(v)
	if l.reg >= 0 {
		if arm64.FReg(l.reg) != src {
			c.a.FMov(arm64.FReg(l.reg), src)
		}
		return
	}
	c.a.StoreF(a64Ctx, c.spillDisp(l.spill), src)
}

// materialize computes a lazy value's word into dst.
func (c *a64Compiler) materialize(v *ssa.Value, dst arm64.Reg) {
	switch v.Op {
	case ssa.OpConst:
		c.movImm(dst, c.constWord(v.Const))
	case ssa.OpConstSource:
		if v.Aux < 0 {
			c.movImm(dst, 0)
		} else {
			c.slotSource(v.Aux, dst)
		}
	case ssa.OpBoxF64:
		c.boxF64(c.fpr(v.Args[0], a64F1), dst)
	case ssa.OpBoxBool:
		c.boxBool(c.gpr(v.Args[0], a64B), dst)
	default:
		panic("materialize " + v.Op.String())
	}
}

// boxF64 puts x's bits in dst, with NaN canonical.
func (c *a64Compiler) boxF64(x arm64.FReg, dst arm64.Reg) {
	done := c.a.NewLabel()
	c.a.FMovFromF(dst, x)
	c.a.FCmp(x, x)
	c.a.BCond(arm64.VC, done)
	c.movImm(dst, c.enc.CanonicalNaN)
	c.a.Bind(done)
}

// boxBool puts the word for b (0 or 1) in dst. dst may be b.
func (c *a64Compiler) boxBool(b, dst arm64.Reg) {
	tr, done := c.a.NewLabel(), c.a.NewLabel()
	c.a.Cbnz(b, tr, false)
	c.movImm(dst, c.enc.False)
	c.a.B(done)
	c.a.Bind(tr)
	c.movImm(dst, c.enc.True)
	c.a.Bind(done)
}

// stubLabel returns the exit stub for a state and kind: found by the
// state's ID (stubAt), an exit to Go or a deoptimization, or else in a
// map, which a guard asked for the one it shares with others of its
// instruction had cost.
func (c *a64Compiler) stubLabel(s *ssa.FrameState, kind uint64) arm64.Label {
	i := -1
	switch kind {
	case abi.ExitHost:
		i = 2 * s.ID()
	case abi.ExitDeopt:
		i = 2*s.ID() + 1
	}
	if i >= 0 && i < len(c.stubAt) {
		if l := c.stubAt[i]; l != 0 {
			return arm64.Label(l - 1)
		}
	}
	k := stubKey{s, kind}
	if i < 0 || i >= len(c.stubAt) {
		if l, ok := c.stubs[k]; ok {
			return l
		}
	}
	l := c.a.NewLabel()
	if i >= 0 && i < len(c.stubAt) {
		c.stubAt[i] = int(l) + 1
	} else {
		c.stubs[k] = l
	}
	c.stubFor = append(c.stubFor, a64Stub{k, l})
	return l
}

// exitTo writes a frame state into the frame and returns with an exit
// record, as amd64's does.
func (c *a64Compiler) exitTo(s *ssa.FrameState, kind uint64) {
	c.exitThen(s, kind, nil)
}

// exitThen is exitTo, going on at then, if it is not nil, instead of
// returning to Go, as amd64's does.
func (c *a64Compiler) exitThen(s *ssa.FrameState, kind uint64, then *arm64.Label) {
	if then == nil && TableExits {
		if d := c.exitDescriptor(s, kind, uint8(a64Locals), uint8(a64Stack)); d != nil {
			c.exits = append(c.exits, d)
			c.a.MovAddr(a64A, uint64(uintptr(unsafe.Pointer(d))))
			if !c.tableUsed {
				c.table, c.tableUsed = c.a.NewLabel(), true
			}
			c.a.B(c.table)
			return
		}
	}
	c.recorded = false
	c.a.Store(a64Ctx, abi.OffRecords, arm64.ZR)
	for i, v := range s.Slots {
		if v == nil || v.Op == ssa.OpLoadSlot && v.Aux == i || c.captured(i) {
			continue
		}
		var w arm64.Reg
		if remat(v) {
			c.materialize(v, a64A)
			w = a64A
		} else {
			w = c.gpr(v, a64A)
		}
		next := c.a.NewLabel()
		if v.Shadow != nil {
			// As amd64's: a record unless the value is a primitive or this
			// slot's reference.
			scalar := c.a.NewLabel()
			from := c.gpr(v.Shadow, a64B)
			c.a.Cbz(from, scalar, true)
			c.slotSource(i, a64C)
			c.a.Cmp(from, a64C, true)
			c.a.BCond(arm64.EQ, next)
			c.appendRecord(uint64(i)|abi.RecordMaybe, c.gprAfter(v.Shadow), 0, false, &w)
			c.a.B(next)
			c.a.Bind(scalar)
		} else if o, ok := c.origin.Of(v); ok && o >= 0 {
			scalar := c.a.NewLabel()
			c.isReference(v, w, o, scalar)
			if o != i {
				c.appendRecord(uint64(i), nil, uint64(o), true, nil)
			}
			c.a.B(next)
			c.a.Bind(scalar)
		}
		record := c.a.NewLabel()
		base, disp := c.slotAddr(i, true, a64C)
		c.a.Load(a64C, base, disp)
		c.a.Cbnz(a64C, record, true)
		base, disp = c.slotAddr(i, false, a64C)
		c.a.Store(base, disp, w)
		c.a.B(next)
		c.a.Bind(record)
		c.appendRecord(uint64(i)|abi.RecordScalar, nil, 0, true, &w)
		c.a.Bind(next)
	}
	if s.Inline != nil {
		// As amd64's: each inlined frame is said in a context of its own,
		// the outermost's the next.
		c.a.Load(a64C, a64Ctx, abi.OffStackBase)
		c.a.Op(arm64.Sub, a64C, a64Stack, a64C, true)
		c.a.ShiftImm(arm64.Lsr, a64C, a64C, 4, true)
		c.a.MovRR(a64B, a64Ctx)
		for _, in := range inlineLevels(s) {
			c.a.Load(a64B, a64B, abi.OffNext)
			k := uint64(abi.ExitHost)
			if in == s.Inline {
				k = kind
			}
			for _, f := range []struct {
				off int32
				v   uint64
			}{{abi.OffInlineClosure, uint64(in.Closure)}, {abi.OffInlineLocals, uint64(in.Locals)}, {abi.OffInlineThis, uint64(in.ThisSlot + 1)},
				{abi.OffInlineCallee, uint64(in.Callee)},
				{abi.OffExitKind, k}, {abi.OffExitPC, uint64(in.PC)}, {abi.OffExitDepth, uint64(in.Depth)}, {abi.OffExitSite, uint64(int64(in.Site))},
				{abi.OffLive, abi.LiveInline}} {
				c.movImm(a64A, f.v)
				c.a.Store(a64B, f.off, a64A)
			}
			c.a.AddImm(a64A, a64C, int64(in.Base-c.f.Locals), true)
			c.a.Store(a64B, abi.OffBase, a64A)
		}
		kind = abi.ExitHost
	}
	c.record(kind, uint64(s.PC), uint64(s.Depth), uint64(int64(s.Site)), then)
}

// tableExit is where exits that leave their frames to Go go, as amd64's:
// with the description's address in A, it saves the registers a
// description may name and returns to Go with an abi.ExitTable exit.
// enterExit is where a native call goes whose callee, its function object
// in A, has no native code native callers may call, as amd64's.
func (c *a64Compiler) enterExit() {
	c.a.Store(a64Ctx, abi.OffEnterCallee, a64A)
	for _, off := range []int32{abi.OffRecords, abi.OffExitPC, abi.OffExitDepth} {
		c.a.Store(a64Ctx, off, arm64.ZR)
	}
	c.movImm(a64A, ^uint64(0))
	c.a.Store(a64Ctx, abi.OffExitSite, a64A)
	c.movImm(a64A, abi.ExitEnter)
	c.a.Store(a64Ctx, abi.OffExitKind, a64A)
	c.a.Ret()
}

// movImm sets dst to the word v: a load of the context's copy where v is
// one of its constants and a move would take more than one instruction,
// as a tag's does, and otherwise the move.
func (c *a64Compiler) movImm(dst arm64.Reg, v uint64) {
	zeros, ones := 0, 0
	for i := 0; i < 4; i++ {
		switch uint16(v >> (16 * i)) {
		case 0:
			zeros++
		case 0xFFFF:
			ones++
		}
	}
	if max(zeros, ones) < 3 {
		for i, w := range c.consts {
			if w == v {
				c.a.Load(dst, a64Ctx, abi.OffConsts+int32(i)*8)
				return
			}
		}
	}
	c.a.MovImm(dst, v)
}

func (c *a64Compiler) tableExit() {
	c.a.Store(a64Ctx, abi.OffExitDesc, a64A)
	for _, r := range append([]int{int(a64Locals), int(a64Stack)}, c.usedRegs(c.gprs, false)...) {
		c.a.Store(a64Ctx, abi.OffRegs+int32(r)*8, arm64.Reg(r))
	}
	for _, x := range c.usedRegs(c.fprs, true) {
		c.a.StoreF(a64Ctx, abi.OffXRegs+int32(x)*8, arm64.FReg(x))
	}
	c.movImm(a64A, abi.ExitTable)
	c.a.Store(a64Ctx, abi.OffExitKind, a64A)
	c.a.Ret()
}

// isReference falls through when v, whose word is in w, is the reference
// slot o held at entry, and jumps to primitive when it is not. It uses C.
func (c *a64Compiler) isReference(v *ssa.Value, w arm64.Reg, o int, primitive arm64.Label) {
	base, disp := c.slotAddr(o, true, a64C)
	c.a.Load(a64C, base, disp)
	c.a.Cbz(a64C, primitive, true)
	if v.Op != ssa.OpLoadSlot {
		base, disp = c.slotAddr(o, false, a64C)
		c.a.Load(a64C, base, disp)
		c.a.Cmp(w, a64C, true)
		c.a.BCond(arm64.NE, primitive)
	}
}

// appendRecord adds an abi.Record for slot, as amd64's does. It uses B
// and C.
func (c *a64Compiler) appendRecord(slot uint64, arg func() arm64.Reg, imm uint64, useImm bool, word *arm64.Reg) {
	c.recorded = true
	c.a.Load(a64C, a64Ctx, abi.OffRecords)
	c.a.ShiftImm(arm64.Lsl, a64C, a64C, 5, true)
	c.a.Op(arm64.Add, a64C, a64C, a64Ctx, true)
	if word != nil {
		c.a.Store(a64C, abi.OffRecord+16, *word)
	}
	c.movImm(a64B, slot)
	c.a.Store(a64C, abi.OffRecord, a64B)
	if useImm {
		c.movImm(a64B, imm)
		c.a.Store(a64C, abi.OffRecord+8, a64B)
	} else {
		c.a.Store(a64C, abi.OffRecord+8, arg())
	}
	c.a.Load(a64C, a64Ctx, abi.OffRecords)
	c.a.AddImm(a64C, a64C, 1, true)
	c.a.Store(a64Ctx, abi.OffRecords, a64C)
}

// gprAfter returns a function yielding a register holding v, loading it
// into B if it has none.
func (c *a64Compiler) gprAfter(v *ssa.Value) func() arm64.Reg {
	return func() arm64.Reg { return c.gpr(v, a64B) }
}

// record fills the exit record and returns to Go, or goes on at then, as
// amd64's does.
func (c *a64Compiler) record(kind, pc, depth, site uint64, then *arm64.Label) {
	c.movImm(a64A, kind)
	c.a.Store(a64Ctx, abi.OffExitKind, a64A)
	c.movImm(a64A, pc)
	c.a.Store(a64Ctx, abi.OffExitPC, a64A)
	c.movImm(a64A, depth)
	c.a.Store(a64Ctx, abi.OffExitDepth, a64A)
	c.movImm(a64A, site)
	c.a.Store(a64Ctx, abi.OffExitSite, a64A)
	switch {
	case !c.recorded && then == nil:
		c.a.Ret()
		return
	case !c.recorded:
		c.a.B(*then)
		return
	case then != nil:
		c.a.Adr(a64A, *then)
		c.a.Store(a64Ctx, abi.OffTailReturn, a64A)
	}
	if !c.tailUsed {
		c.tail, c.tailUsed = c.a.NewLabel(), true
	}
	c.a.B(c.tail)
}

// recordsTail applies an exit's records natively while the collector is
// not marking, as amd64's does. D counts the records; F1 keeps how many.
func (c *a64Compiler) recordsTail() {
	ret, read, write, maybe, scalar, copyValue, next := c.a.NewLabel(), c.a.NewLabel(), c.a.NewLabel(),
		c.a.NewLabel(), c.a.NewLabel(), c.a.NewLabel(), c.a.NewLabel()
	c.movImm(a64B, c.enc.WriteBarrier)
	c.a.LoadU8(a64B, a64B, 0)
	c.a.Cbnz(a64B, ret, false)
	c.a.Load(a64D, a64Ctx, abi.OffRecords)
	c.a.Cbz(a64D, ret, true)
	c.a.FMovToF(a64F1, a64D)
	c.a.Bind(read)
	c.a.AddImm(a64D, a64D, -1, true)
	c.a.ShiftImm(arm64.Lsl, a64A, a64D, 5, true)
	c.a.Op(arm64.Add, a64A, a64A, a64Ctx, true)
	c.a.Load(a64C, a64A, abi.OffRecord)
	c.movImm(a64B, abi.RecordScalar)
	c.a.Tst(a64C, a64B, true)
	c.a.BCond(arm64.NE, scalar)
	c.movImm(a64B, abi.RecordMaybe)
	c.a.Tst(a64C, a64B, true)
	c.a.BCond(arm64.NE, maybe)
	c.a.Load(a64C, a64A, abi.OffRecord+8)
	c.sourceAddr()
	c.a.Bind(copyValue)
	c.a.Load(a64B, a64C, c.enc.NumOffset)
	c.a.Store(a64A, abi.OffRecord+16, a64B)
	c.a.Load(a64B, a64C, c.enc.RefOffset)
	c.a.Store(a64A, abi.OffRecord+8, a64B)
	c.a.B(next)
	c.a.Bind(maybe)
	c.a.Load(a64C, a64A, abi.OffRecord+8)
	c.a.Cbz(a64C, scalar, true)
	c.a.Load(a64B, a64C, c.enc.RefOffset)
	c.a.Cbnz(a64B, copyValue, true)
	c.a.Bind(scalar)
	c.a.Store(a64A, abi.OffRecord+8, arm64.ZR)
	c.a.Bind(next)
	c.a.Cbnz(a64D, read, true)
	c.a.FMovFromF(a64D, a64F1)
	c.a.Bind(write)
	c.a.AddImm(a64D, a64D, -1, true)
	c.a.ShiftImm(arm64.Lsl, a64A, a64D, 5, true)
	c.a.Op(arm64.Add, a64A, a64A, a64Ctx, true)
	c.a.Load(a64C, a64A, abi.OffRecord)
	c.movImm(a64B, ^uint64(abi.RecordScalar|abi.RecordMaybe))
	c.a.Op(arm64.And, a64C, a64C, a64B, true)
	c.sourceAddr()
	c.a.Load(a64B, a64A, abi.OffRecord+16)
	c.a.Store(a64C, c.enc.NumOffset, a64B)
	c.a.Load(a64B, a64A, abi.OffRecord+8)
	c.a.Store(a64C, c.enc.RefOffset, a64B)
	c.a.Cbnz(a64D, write, true)
	c.a.Store(a64Ctx, abi.OffRecords, arm64.ZR)
	c.a.Bind(ret)
	// A native call's writing of its caller's state goes on in the
	// caller's code (exitThen).
	toGo := c.a.NewLabel()
	c.a.Load(a64B, a64Ctx, abi.OffTailReturn)
	c.a.Cbz(a64B, toGo, true)
	c.a.Store(a64Ctx, abi.OffTailReturn, arm64.ZR)
	c.a.Br(a64B)
	c.a.Bind(toGo)
	c.a.Ret()
}

// returnNative returns to the native caller, if there is one, with the
// result, as amd64's does; otherwise it falls through to return to Go.
func (c *a64Compiler) returnNative() {
	toGo, word := c.a.NewLabel(), c.a.NewLabel()
	c.a.Load(a64A, a64Ctx, abi.OffReturnTo)
	c.a.Cbz(a64A, toGo, true)
	c.a.Load(a64A, a64Ctx, abi.OffRet)
	c.a.Store(a64Ctx, abi.OffRetValue+c.enc.NumOffset, a64A)
	c.a.Store(a64Ctx, abi.OffRetValue+c.enc.RefOffset, arm64.ZR)
	c.a.Load(a64C, a64Ctx, abi.OffRetFrom)
	c.a.Cbz(a64C, word, true)
	c.a.Load(a64C, a64C, c.enc.RefOffset)
	c.a.Store(a64Ctx, abi.OffRetValue+c.enc.RefOffset, a64C)
	c.a.Bind(word)
	c.a.Load(a64A, a64Ctx, abi.OffReturnTo)
	c.a.Br(a64A)
	c.a.Bind(toGo)
}

// block emits one block. next is the block laid out after it.
func (c *a64Compiler) block(b *ssa.Block, next *ssa.Block) {
	if b.PC < 0 {
		c.a.Load(a64Locals, a64Ctx, abi.OffLocals)
		c.a.Load(a64Stack, a64Ctx, abi.OffStack)
	}
	for _, v := range b.Values {
		if v.Op == ssa.OpPhi || c.isLazy(v) {
			continue
		}
		c.value(v, b)
	}
	switch b.Kind {
	case ssa.BlockPlain:
		c.edge(b, b.Succs[0], next)
	case ssa.BlockIf:
		c.branch(b, next)
	case ssa.BlockReturn:
		r := c.gpr(b.Control, a64A)
		c.a.Store(a64Ctx, abi.OffRet, r)
		c.a.Store(a64Ctx, abi.OffRetFrom, arm64.ZR)
		if s := b.Control.Shadow; s != nil {
			// RetFrom is the source, 0 for a primitive's.
			c.a.Store(a64Ctx, abi.OffRetFrom, c.gpr(s, a64C))
		} else if o, ok := c.origin.Of(b.Control); ok && o >= 0 {
			done := c.a.NewLabel()
			c.isReference(b.Control, r, o, done)
			c.slotSource(o, a64C)
			c.a.Store(a64Ctx, abi.OffRetFrom, a64C)
			c.a.Bind(done)
		}
		c.movImm(a64A, abi.ExitReturn)
		c.a.Store(a64Ctx, abi.OffExitKind, a64A)
		c.movImm(a64A, uint64(b.PC))
		c.a.Store(a64Ctx, abi.OffExitSite, a64A)
		c.returnNative()
		c.a.Ret()
	case ssa.BlockExit:
		c.exitTo(b.State, exitKind(int(b.ExitKind)))
	}
}

// edge emits the moves for an edge's phis, a poll on a back-edge, and a jump
// unless the target is laid out next.
func (c *a64Compiler) edge(from, to, next *ssa.Block) {
	idx := -1
	for i, p := range to.Preds {
		if p == from {
			idx = i
		}
	}
	c.phiMoves(to, idx)
	if to.LoopHeader && to.Backedge[idx] {
		poll := c.a.NewLabel()
		c.a.Load(a64B, a64Ctx, abi.OffBackEdges)
		c.a.Load(a64A, a64B, 0)
		c.a.AddImm(a64A, a64A, -1, true)
		c.a.Store(a64B, 0, a64A)
		c.a.CmpImm(a64A, 0, true)
		c.a.BCond(arm64.LE, poll)
		header := to.Header
		c.cold = append(c.cold, func() {
			c.a.Bind(poll)
			if c.enc.CallGo != 0 {
				// Go gives the loop another budget (ssa.GoPoll).
				stub := c.stubLabel(header, abi.ExitPoll)
				c.goSaving(uint64(ssa.GoPoll), &stub)
				c.a.B(c.labels[to.ID])
				return
			}
			c.exitTo(header, abi.ExitPoll)
		})
	}
	if to != next {
		c.a.B(c.labels[to.ID])
	}
}

// phiMoves performs the parallel move of an edge's phi arguments, as the
// core schedules it.
func (c *a64Compiler) phiMoves(to *ssa.Block, idx int) {
	for _, st := range c.phiSchedule(to, idx) {
		switch {
		case st.park == nil:
			c.moveValue(st.dst, st.src, st.parked)
		case isFloat(st.park):
			c.a.FMov(a64F0, c.fpr(st.park, a64F0))
		default:
			c.a.MovRR(a64C, c.gpr(st.park, a64C))
		}
	}
}

// moveValue copies src into dst's location; parked sources are in scratch.
func (c *a64Compiler) moveValue(dst, src *ssa.Value, parked bool) {
	if isFloat(dst) {
		x := a64F0
		if !parked {
			x = c.fpr(src, a64F1)
		}
		c.setF(dst, x)
		return
	}
	if l := c.locAt(dst); l.reg >= 0 && !parked {
		c.gprInto(src, arm64.Reg(l.reg))
		return
	}
	r := a64C
	if !parked {
		r = c.gpr(src, a64A)
	}
	c.setG(dst, r)
}

// a64Compare is the condition an FCMP of x with y leaves for a comparison
// operator: each is false when unordered, but NE, which JavaScript's !=
// wants true for NaN.
func a64Compare(op ir.Operator) arm64.Cond {
	switch op {
	case ir.Lt:
		return arm64.MI
	case ir.Le:
		return arm64.LS
	case ir.Gt:
		return arm64.GT
	case ir.Ge:
		return arm64.GE
	case ir.Eq:
		return arm64.EQ
	}
	return arm64.NE
}

// branch emits an If block's end, fusing a float comparison into the jump.
func (c *a64Compiler) branch(b *ssa.Block, next *ssa.Block) {
	yes, no := c.a.NewLabel(), c.a.NewLabel()
	ctl := b.Control
	if ctl.Op == ssa.OpCmpF64 && ctl.Uses == 1 && ctl.Block == b {
		c.a.FCmp(c.fpr(ctl.Args[0], a64F0), c.fpr(ctl.Args[1], a64F1))
		c.a.BCond(a64Compare(ir.Operator(ctl.Aux)), yes)
	} else {
		c.a.Cbnz(c.gpr(ctl, a64A), yes, false)
	}
	// The false edge follows.
	c.a.Bind(no)
	c.edge(b, b.Succs[1], nil)
	c.a.Bind(yes)
	c.edge(b, b.Succs[0], next)
}

// looseNullish is x == null, as amd64's: null's or undefined's word, or an
// object with [[IsHTMLDDA]].
func (c *a64Compiler) looseNullish(v *ssa.Value, guard func(arm64.Cond)) {
	a := v.Args[0]
	yes, no, done := c.a.NewLabel(), c.a.NewLabel(), c.a.NewLabel()
	w := c.gpr(a, a64B)
	for _, word := range []uint64{c.enc.Null, c.enc.Undefined} {
		c.movImm(a64A, word)
		c.a.Cmp(w, a64A, true)
		c.a.BCond(arm64.EQ, yes)
	}
	c.movImm(a64A, c.enc.Object)
	c.a.Cmp(w, a64A, true)
	c.a.BCond(arm64.NE, no)
	if c.reference(v, a, c.enc.Object, guard) {
		c.a.LoadU8(a64A, a64C, c.enc.ObjectFlags)
		c.movImm(a64B, uint64(c.enc.FlagHTMLDDA))
		c.a.Tst(a64A, a64B, false)
		c.a.BCond(arm64.NE, yes)
	}
	c.a.Bind(no)
	c.movImm(a64A, 0)
	c.a.B(done)
	c.a.Bind(yes)
	c.movImm(a64A, 1)
	c.a.Bind(done)
	c.setG(v, a64A)
}

// eqTagged is OpEqTagged, as amd64's.
func (c *a64Compiler) eqTagged(v *ssa.Value, guard func(arm64.Cond)) {
	x, y := v.Args[0], v.Args[1]
	yes, no, done, differ, number := c.a.NewLabel(), c.a.NewLabel(), c.a.NewLabel(), c.a.NewLabel(), c.a.NewLabel()
	exit := c.stubLabel(v.State, exitKind(v.Aux))
	wx, wy := c.gpr(x, a64A), c.gpr(y, a64B)
	c.a.Cmp(wx, wy, true)
	c.a.BCond(arm64.NE, differ)
	c.movImm(a64C, abi.NumberLimit)
	c.a.Cmp(wx, a64C, true)
	c.a.BCond(arm64.LO, number)
	for _, w := range []uint64{c.enc.Null, c.enc.Undefined, c.enc.True, c.enc.False} {
		c.movImm(a64C, w)
		c.a.Cmp(wx, a64C, true)
		c.a.BCond(arm64.EQ, yes)
	}
	strings := c.a.NewLabel()
	c.movImm(a64C, c.enc.String)
	c.a.Cmp(wx, a64C, true)
	c.a.BCond(arm64.EQ, strings)
	c.movImm(a64C, c.enc.Object)
	c.a.Cmp(wx, a64C, true)
	c.a.BCond(arm64.NE, exit)
	if c.reference(v, x, c.enc.Object, guard) {
		c.a.FMovToF(a64F1, a64C)
		if c.reference(v, y, c.enc.Object, guard) {
			c.a.FMovFromF(a64A, a64F1)
			c.a.Cmp(a64A, a64C, true)
			c.a.BCond(arm64.EQ, yes)
			c.a.B(no)
		}
	}
	c.a.Bind(strings)
	if c.reference(v, x, c.enc.String, guard) {
		c.a.FMovToF(a64F1, a64C)
		if c.reference(v, y, c.enc.String, guard) {
			c.a.FMovFromF(a64A, a64F1)
			c.a.Cmp(a64A, a64C, true)
			c.a.BCond(arm64.EQ, yes)
			c.a.Load(a64A, a64A, c.enc.StringLength)
			c.a.Load(a64B, a64C, c.enc.StringLength)
			c.a.Cmp(a64A, a64B, true)
			c.a.BCond(arm64.NE, no)
			c.stringBytes(exit, yes, no)
		}
	}
	c.a.Bind(number)
	c.a.FMovToF(a64F0, wx)
	c.a.FCmp(a64F0, a64F0)
	c.a.BCond(arm64.VS, no)
	c.a.B(yes)
	c.a.Bind(differ)
	other := no
	if v.Index != 1 {
		// Loosely, null and undefined equal each other and an object with
		// [[IsHTMLDDA]] alone, as amd64's.
		other = exit
		numbers, xNullish, yNullish := c.a.NewLabel(), c.a.NewLabel(), c.a.NewLabel()
		for _, w := range []uint64{c.enc.Null, c.enc.Undefined} {
			c.movImm(a64C, w)
			c.a.Cmp(wx, a64C, true)
			c.a.BCond(arm64.EQ, xNullish)
			c.a.Cmp(wy, a64C, true)
			c.a.BCond(arm64.EQ, yNullish)
		}
		c.a.B(numbers)
		for _, k := range []struct {
			at    arm64.Label
			other *ssa.Value
			w     arm64.Reg
		}{{xNullish, y, wy}, {yNullish, x, wx}} {
			c.a.Bind(k.at)
			for _, w := range []uint64{c.enc.Null, c.enc.Undefined} {
				c.movImm(a64C, w)
				c.a.Cmp(k.w, a64C, true)
				c.a.BCond(arm64.EQ, yes)
			}
			c.movImm(a64C, c.enc.Object)
			c.a.Cmp(k.w, a64C, true)
			c.a.BCond(arm64.NE, no)
			if c.reference(v, k.other, c.enc.Object, guard) {
				c.a.LoadU8(a64C, a64C, c.enc.ObjectFlags)
				c.movImm(a64A, uint64(c.enc.FlagHTMLDDA))
				c.a.Tst(a64C, a64A, false)
				c.a.BCond(arm64.NE, yes)
			}
			c.a.B(no)
		}
		c.a.Bind(numbers)
	}
	c.movImm(a64C, abi.NumberLimit)
	c.a.Cmp(wx, a64C, true)
	c.a.BCond(arm64.HS, other)
	c.a.Cmp(wy, a64C, true)
	c.a.BCond(arm64.HS, other)
	c.a.FMovToF(a64F0, wx)
	c.a.FMovToF(a64F1, wy)
	c.a.FCmp(a64F0, a64F1)
	c.a.BCond(arm64.NE, no)
	c.a.BCond(arm64.VS, no)
	c.a.Bind(yes)
	c.movImm(a64A, 1)
	c.a.B(done)
	c.a.Bind(no)
	c.movImm(a64A, 0)
	c.a.Bind(done)
	c.setG(v, a64A)
}

// arrayOf finds the array a value is and checks its class.
func (c *a64Compiler) arrayOf(v *ssa.Value, guard func(arm64.Cond)) {
	if !c.objectOf(v, guard) {
		return
	}
	c.a.LoadU8(a64B, a64C, c.enc.ObjectClass)
	c.a.CmpImm(a64B, int64(c.enc.ClassArray), false)
	guard(arm64.NE)
	c.setG(v, a64C)
}

// objectOf finds the object a value is, into C.
// instanceOf is amd64's: whether its value has the object its second
// argument is among its prototypes (OpInstanceOf). It uses A, B and C.
func (c *a64Compiler) instanceOf(v *ssa.Value, guard func(arm64.Cond)) {
	a := v.Args[0]
	if a.Shadow == nil && c.origin.At(a) < 0 {
		c.movImm(a64B, 0)
		c.setG(v, a64B)
		return
	}
	no, loop, done := c.a.NewLabel(), c.a.NewLabel(), c.a.NewLabel()
	w := c.gpr(a, a64A)
	c.movImm(a64B, c.enc.Object)
	c.a.Cmp(w, a64B, true)
	c.a.BCond(arm64.NE, no)
	c.sourceRef(a, guard)
	c.a.CmpImm(a64C, 0, true)
	guard(arm64.EQ)
	p := c.gpr(v.Args[1], a64A)
	c.a.Bind(loop)
	c.a.LoadU8(a64B, a64C, c.enc.ObjectClass)
	c.a.CmpImm(a64B, int64(c.enc.ClassProxy), false)
	guard(arm64.EQ)
	c.a.Load(a64C, a64C, c.enc.ObjectProto)
	c.a.Cbz(a64C, no, true)
	c.a.Cmp(a64C, p, true)
	c.a.BCond(arm64.NE, loop)
	c.movImm(a64B, 1)
	c.a.B(done)
	c.a.Bind(no)
	c.movImm(a64B, 0)
	c.a.Bind(done)
	c.setG(v, a64B)
}

// typeIs is amd64's (OpTypeIs). It uses A, B and C.
func (c *a64Compiler) typeIs(v *ssa.Value, guard func(arm64.Cond)) {
	a := v.Args[0]
	yes, no, done := c.a.NewLabel(), c.a.NewLabel(), c.a.NewLabel()
	w := c.gpr(a, a64B)
	is := func(word uint64) {
		c.movImm(a64A, word)
		c.a.Cmp(w, a64A, true)
		c.a.BCond(arm64.EQ, yes)
	}
	switch v.Index {
	case ir.TypeNumber:
		c.movImm(a64A, abi.NumberLimit)
		c.a.Cmp(w, a64A, true)
		c.a.BCond(arm64.LO, yes)
	case ir.TypeString:
		is(c.enc.String)
	case ir.TypeBoolean:
		is(c.enc.True)
		is(c.enc.False)
	case ir.TypeUndefined, ir.TypeFunction:
		if v.Index == ir.TypeUndefined {
			is(c.enc.Undefined)
		}
		c.movImm(a64A, c.enc.Object)
		c.a.Cmp(w, a64A, true)
		c.a.BCond(arm64.NE, no)
		if c.reference(v, a, c.enc.Object, guard) {
			c.a.LoadU8(a64A, a64C, c.enc.ObjectFlags)
			c.movImm(a64B, uint64(c.enc.FlagHTMLDDA))
			c.a.Tst(a64A, a64B, false)
			if v.Index == ir.TypeUndefined {
				c.a.BCond(arm64.NE, yes)
				c.a.B(no)
			} else {
				c.a.BCond(arm64.NE, no)
				c.a.LoadU8(a64A, a64C, c.enc.ObjectClass)
				c.a.CmpImm(a64A, int64(c.enc.ClassFunction), false)
				c.a.BCond(arm64.EQ, yes)
				c.a.CmpImm(a64A, int64(c.enc.ClassObject), false)
				c.a.BCond(arm64.EQ, no)
				c.a.CmpImm(a64A, int64(c.enc.ClassArray), false)
				guard(arm64.NE)
			}
		}
	}
	c.a.Bind(no)
	c.movImm(a64A, v.Const.Bits)
	c.a.B(done)
	c.a.Bind(yes)
	c.movImm(a64A, v.Const.Bits^1)
	c.a.Bind(done)
	c.setG(v, a64A)
}

func (c *a64Compiler) objectOf(v *ssa.Value, guard func(arm64.Cond)) bool {
	return c.reference(v, v.Args[0], c.enc.Object, guard)
}

// reference finds the reference a is, of the kind whose word is word, into
// C, as amd64's does. It uses A and B.
func (c *a64Compiler) reference(v, a *ssa.Value, word uint64, guard func(arm64.Cond)) bool {
	fail := c.stubLabel(v.State, exitKind(v.Aux))
	o := c.origin.At(a)
	if a.Shadow == nil && o < 0 {
		c.a.B(fail)
		return false
	}
	w := c.gpr(a, a64A)
	c.movImm(a64B, word)
	c.a.Cmp(w, a64B, true)
	guard(arm64.NE)
	c.sourceRef(a, guard)
	c.a.Cbz(a64C, fail, true)
	return true
}

// sourceRef loads the pointer word of a, a value with a source, into C, as
// amd64's does. It uses A and B.
func (c *a64Compiler) sourceRef(a *ssa.Value, guard func(arm64.Cond)) {
	o := c.origin.At(a)
	if s := a.Shadow; s != nil && cellSource(s) {
		c.a.Load(a64C, c.gpr(s, a64C), c.enc.RefOffset)
	} else if s != nil {
		r := c.gpr(s, a64C)
		c.a.CmpImm(r, 0, true)
		guard(arm64.EQ)
		c.a.Load(a64C, r, c.enc.RefOffset)
	} else {
		base, disp := c.slotAddr(o, true, a64C)
		c.a.Load(a64C, base, disp)
	}
}

// sourceAddr turns the slot in C, a record's, into the address of its
// value, as amd64's does. It uses B.
func (c *a64Compiler) sourceAddr() {
	captured, stack, found := c.a.NewLabel(), c.a.NewLabel(), c.a.NewLabel()
	c.a.CmpImm(a64C, abi.MaxRecords, true)
	c.a.BCond(arm64.HS, found)
	c.a.CmpImm(a64C, int64(c.f.FrameLocals), true)
	c.a.BCond(arm64.HS, captured)
	c.a.ShiftImm(arm64.Lsl, a64C, a64C, 4, true)
	c.a.Op(arm64.Add, a64C, a64C, a64Locals, true)
	c.a.B(found)
	c.a.Bind(captured)
	c.a.CmpImm(a64C, int64(c.f.Locals), true)
	c.a.BCond(arm64.HS, stack)
	if t := c.f.ThisSlot; t >= 0 {
		notThis := c.a.NewLabel()
		c.a.CmpImm(a64C, int64(t), true)
		c.a.BCond(arm64.NE, notThis)
		c.a.AddImm(a64C, a64Ctx, int64(abi.OffThis), true)
		c.a.B(found)
		c.a.Bind(notThis)
	}
	c.a.AddImm(a64C, a64C, -int64(c.f.FrameLocals), true)
	c.a.ShiftImm(arm64.Lsl, a64C, a64C, 3, true)
	c.a.Load(a64B, a64Ctx, abi.OffUpvalues)
	c.a.Op(arm64.Add, a64C, a64C, a64B, true)
	c.a.Load(a64C, a64C, 0)
	c.a.Load(a64C, a64C, c.enc.UpvalueSlot)
	c.a.B(found)
	c.a.Bind(stack)
	c.a.AddImm(a64C, a64C, -int64(c.f.Locals), true)
	c.a.ShiftImm(arm64.Lsl, a64C, a64C, 4, true)
	c.a.Op(arm64.Add, a64C, a64C, a64Stack, true)
	c.a.Bind(found)
}

// propStore stores a value in a property, as amd64's does. It uses every
// scratch register and F1.
func (c *a64Compiler) propStore(v *ssa.Value, guard func(arm64.Cond)) {
	x := v.Args[1]
	var w arm64.Reg
	if remat(x) {
		c.materialize(x, a64A)
		w = a64A
	} else {
		w = c.gpr(x, a64A)
	}
	// As amd64's: a number has none.
	number, have := c.a.NewLabel(), c.a.NewLabel()
	if x.Shadow != nil || c.origin.At(x) >= 0 {
		c.movImm(a64B, abi.NumberLimit)
		c.a.Cmp(w, a64B, true)
		c.a.BCond(arm64.LO, number)
	}
	c.pointerWord(x, w)
	c.a.B(have)
	c.a.Bind(number)
	c.movImm(a64B, 0)
	c.a.Bind(have)
	c.a.FMovToF(a64F1, a64B)
	added := c.a.NewLabel()
	if v.Add() != nil || len(v.Adds()) != 0 {
		// As amd64's: along each transition the site met.
		adds := v.Adds()
		if v.Add() != nil {
			adds = append([]*ssa.PropertyAdd{v.Add()}, adds...)
		}
		for _, add := range adds {
			next := c.a.NewLabel()
			c.addAlong(v, add, next)
			c.a.B(added)
			c.a.Bind(next)
		}
		if v.Add() != nil && v.Add().Define {
			// As amd64's: a literal's field is added so, or by Go.
			c.a.B(c.stubLabel(v.State, exitKind(v.Aux)))
			c.a.Bind(added)
			return
		}
	}
	if v.Upvalue {
		// As amd64's: a captured binding's cell.
		if r := c.gpr(v.Args[0], a64A); r != a64A {
			c.a.MovRR(a64A, r)
		}
	} else if v.Global {
		// As amd64's: a global binding's cell, writable.
		if r := c.gpr(v.Args[0], a64A); r != a64A {
			c.a.MovRR(a64A, r)
		}
		c.a.LoadU8(a64B, a64A, c.enc.PropertyFlags-c.enc.PropertyValue)
		c.movImm(a64C, uint64(c.enc.PropNotWritable))
		c.a.Op(arm64.And, a64B, a64B, a64C, false)
		c.a.CmpImm(a64B, int64(c.enc.PropWritable), false)
		guard(arm64.NE)
	} else {
		c.property(v, guard)
	}
	for _, s := range v.Args[2:] {
		// As amd64's: only a pointer word there is lost.
		other := c.a.NewLabel()
		c.a.Cmp(c.gpr(s, a64B), a64A, true)
		c.a.BCond(arm64.NE, other)
		c.a.Load(a64B, a64A, c.enc.RefOffset)
		c.a.Cbnz(a64B, c.stubLabel(v.State, exitKind(v.Aux)), true)
		c.a.Bind(other)
	}
	scalar, viaGo, done := c.a.NewLabel(), c.a.NewLabel(), c.a.NewLabel()
	c.a.FMovFromF(a64C, a64F1)
	c.a.Load(a64B, a64A, c.enc.RefOffset)
	c.a.Op(arm64.Orr, a64B, a64B, a64C, true)
	c.a.Cbz(a64B, scalar, true)
	// As amd64's: while the collector marks, Go stores it.
	c.a.FMovToF(a64F0, c.gpr(x, a64B))
	c.movImm(a64B, c.enc.WriteBarrier)
	c.a.LoadU8(a64B, a64B, 0)
	if c.enc.CallGo == 0 {
		c.a.CmpImm(a64B, 0, false)
		guard(arm64.NE)
	} else {
		c.a.Cbnz(a64B, viaGo, false)
	}
	c.a.Bind(scalar)
	c.a.Store(a64A, c.enc.NumOffset, c.gpr(x, a64B))
	c.a.Store(a64A, c.enc.RefOffset, a64C)
	c.a.Bind(done)
	c.a.Bind(added)
	if c.enc.CallGo == 0 {
		return
	}
	stub := c.stubLabel(v.State, exitKind(v.Aux))
	c.cold = append(c.cold, func() {
		c.a.Bind(viaGo)
		c.goStore(&stub)
		c.a.B(done)
	})
}

// goStore is amd64's: the cell's address in A, the value's words in F0
// and C, which the routine the stores share writes (storingCall).
func (c *a64Compiler) goStore(stub *arm64.Label) {
	if !c.storingUsed {
		c.storing, c.storingUsed = c.a.NewLabel(), true
	}
	back := c.a.NewLabel()
	c.a.Adr(a64B, back)
	c.a.Store(a64Ctx, abi.OffGoSite, a64B)
	c.a.B(c.storing)
	c.a.Bind(back)
	if stub != nil {
		c.a.Load(a64B, a64Ctx, abi.OffGoStatus)
		c.a.Cbnz(a64B, *stub, true)
	}
}

// storingCall is the routine goStore's sites share, as amd64's.
func (c *a64Compiler) storingCall() {
	size := int32(unsafe.Sizeof(abi.GoArg{}))
	c.a.FMovFromF(a64B, a64F0)
	c.a.Store(a64Ctx, abi.OffGoArgs+c.enc.NumOffset, a64B)
	c.a.Store(a64Ctx, abi.OffGoArgs+c.enc.RefOffset, a64C)
	c.a.Store(a64Ctx, abi.OffGoArgs+size+c.enc.RefOffset, a64A)
	c.movImm(a64A, uint64(ssa.GoStore))
	c.a.Store(a64Ctx, abi.OffGoOp, a64A)
	c.a.Store(a64Ctx, abi.OffGoKeep, arm64.ZR)
	if !c.savingUsed {
		c.saving, c.savingUsed = c.a.NewLabel(), true
	}
	c.a.B(c.saving)
}

// goSaving calls Go, op, every allocatable register saved, as amd64's
// does.
func (c *a64Compiler) goSaving(op uint64, stub *arm64.Label) {
	c.goSavingKeep(op, 0, stub)
}

// goSavingKeep is goSaving whose result Go writes to keep cell keep.
func (c *a64Compiler) goSavingKeep(op uint64, keep int, stub *arm64.Label) {
	c.movImm(a64A, op)
	c.a.Store(a64Ctx, abi.OffGoOp, a64A)
	if keep == 0 {
		c.a.Store(a64Ctx, abi.OffGoKeep, arm64.ZR)
	} else {
		c.movImm(a64A, uint64(keep))
		c.a.Store(a64Ctx, abi.OffGoKeep, a64A)
	}
	// The routine the code's calls of Go share (savingCall), as amd64's.
	if !c.savingUsed {
		c.saving, c.savingUsed = c.a.NewLabel(), true
	}
	back := c.a.NewLabel()
	c.a.Adr(a64A, back)
	c.a.Store(a64Ctx, abi.OffGoSite, a64A)
	c.a.B(c.saving)
	c.a.Bind(back)
	if stub != nil {
		c.a.Load(a64B, a64Ctx, abi.OffGoStatus)
		c.a.Cbnz(a64B, *stub, true)
	}
}

// byCode is amd64's: the callee's function checked, its pointer word in C,
// and the object, its closure and its captured bindings kept in the
// context. It uses A, B and C.
func (c *a64Compiler) byCode(t *ssa.CallSite, next arm64.Label) {
	c.a.Store(a64Ctx, abi.OffCallCallee, a64C)
	c.a.Load(a64B, a64C, c.enc.ObjectData)
	c.movImm(a64A, c.enc.FuncData)
	c.a.Cmp(a64B, a64A, true)
	c.a.BCond(arm64.NE, next)
	c.a.Load(a64B, a64C, c.enc.ObjectData+8)
	c.a.Load(a64B, a64B, c.enc.FuncClosure)
	c.a.Cbz(a64B, next, true)
	for _, f := range [...]struct {
		off  int32
		want uintptr
	}{{c.enc.ClosureFn, t.ByCode}, {c.enc.ClosureEnv, t.Env}, {c.enc.ClosureRealm, t.Realm}} {
		c.a.Load(a64A, a64B, f.off)
		c.movImm(a64C, uint64(f.want))
		c.a.Cmp(a64A, a64C, true)
		c.a.BCond(arm64.NE, next)
	}
	c.a.Store(a64Ctx, abi.OffCallClosure, a64B)
	c.a.Load(a64A, a64B, c.enc.ClosureUpvalues)
	c.a.Store(a64Ctx, abi.OffCallUpvalues, a64A)
}

// savingCall is the routine the code's calls of Go that save every register
// share, as amd64's.
func (c *a64Compiler) savingCall() {
	for _, r := range c.usedRegs(c.gprs, false) {
		c.a.Store(a64Ctx, abi.OffRegs+int32(r)*8, arm64.Reg(r))
	}
	for _, r := range c.usedRegs(c.fprs, true) {
		c.a.StoreF(a64Ctx, abi.OffXRegs+int32(r)*8, arm64.FReg(r))
	}
	back := c.a.NewLabel()
	c.a.Adr(a64A, back)
	c.a.Store(a64Ctx, abi.OffGoResume, a64A)
	c.movImm(a64A, c.enc.CallGo)
	c.a.Br(a64A)
	c.a.Bind(back)
	c.a.Load(a64Locals, a64Ctx, abi.OffLocals)
	c.a.Load(a64Stack, a64Ctx, abi.OffStack)
	for _, r := range c.usedRegs(c.gprs, false) {
		c.a.Load(arm64.Reg(r), a64Ctx, abi.OffRegs+int32(r)*8)
	}
	for _, r := range c.usedRegs(c.fprs, true) {
		c.a.LoadF(arm64.FReg(r), a64Ctx, abi.OffXRegs+int32(r)*8)
	}
	c.a.Load(a64A, a64Ctx, abi.OffGoSite)
	c.a.Br(a64A)
}

// addAlong adds the property a write's cache adds, as amd64's does, or
// goes to miss. It uses A, B, C and D.
func (c *a64Compiler) addAlong(v *ssa.Value, add *ssa.PropertyAdd, miss arm64.Label) {
	x := v.Args[1]
	if c.enc.PropertyFlags != c.enc.PropertyKey+4 || c.enc.PropertySize != 24 {
		c.a.B(miss)
		return
	}
	o := c.gpr(v.Args[0], a64D)
	c.a.Load(a64B, o, c.enc.ObjectShape)
	c.movImm(a64C, uint64(add.From))
	c.a.Cmp(a64B, a64C, true)
	c.a.BCond(arm64.NE, miss)
	c.a.LoadU8(a64B, o, c.enc.ObjectFlags)
	c.movImm(a64C, uint64(c.enc.FlagExtensible))
	c.a.Tst(a64B, a64C, false)
	c.a.BCond(arm64.EQ, miss)
	c.a.Load(a64B, o, c.enc.ObjectProto)
	if !add.Define {
		for _, h := range add.Chain() {
			c.movImm(a64C, uint64(h.Object))
			c.a.Cmp(a64B, a64C, true)
			c.a.BCond(arm64.NE, miss)
			// Not D, which may be the object's (o).
			c.a.Load(a64B, a64C, c.enc.ObjectShape)
			c.movImm(a64A, uint64(h.Shape))
			c.a.Cmp(a64B, a64A, true)
			c.a.BCond(arm64.NE, miss)
			c.a.Load(a64B, a64C, c.enc.ObjectProto)
		}
		c.a.Cbnz(a64B, miss, true)
	}
	c.a.Load(a64B, o, c.enc.ObjectProps+8)
	c.a.Load(a64C, o, c.enc.ObjectProps+16)
	c.a.Cmp(a64B, a64C, true)
	c.a.BCond(arm64.HS, miss)
	// As amd64's: while the collector marks, Go adds it. Not D, which may
	// be the object's (o).
	c.movImm(a64A, c.enc.WriteBarrier)
	c.a.LoadU8(a64A, a64A, 0)
	marked, done := c.a.NewLabel(), c.a.NewLabel()
	c.a.Cbnz(a64A, marked, false)
	c.cold = append(c.cold, func() {
		c.a.Bind(marked)
		if c.enc.CallGo == 0 {
			c.movImm(a64B, 1)
			c.a.Store(a64Ctx, abi.OffExitMarking, a64B)
			c.a.B(miss)
			return
		}
		size := int32(unsafe.Sizeof(abi.GoArg{}))
		c.a.FMovFromF(a64B, a64F1)
		c.a.Store(a64Ctx, abi.OffGoArgs+2*size+c.enc.RefOffset, a64B)
		c.a.Store(a64Ctx, abi.OffGoArgs+2*size+c.enc.NumOffset, c.gpr(x, a64A))
		c.a.Store(a64Ctx, abi.OffGoArgs+c.enc.RefOffset, c.gpr(v.Args[0], a64A))
		c.movImm(a64B, uint64(v.Key)|uint64(add.Flags)<<32)
		c.a.Store(a64Ctx, abi.OffGoArgs+c.enc.NumOffset, a64B)
		c.movImm(a64B, uint64(add.Next))
		c.a.Store(a64Ctx, abi.OffGoArgs+size+c.enc.RefOffset, a64B)
		c.goSaving(uint64(ssa.GoAddAlong), &miss)
		c.a.B(done)
	})
	c.a.AddImm(a64C, a64B, 1, true)
	c.a.Store(o, c.enc.ObjectProps+8, a64C)
	c.movImm(a64C, uint64(add.Next))
	c.a.Store(o, c.enc.ObjectShape, a64C)
	// The entry's address: the old length times 24, past the table's start.
	c.a.ShiftImm(arm64.Lsl, a64C, a64B, 1, true)
	c.a.Op(arm64.Add, a64B, a64B, a64C, true)
	c.a.ShiftImm(arm64.Lsl, a64B, a64B, 3, true)
	c.a.Load(a64C, o, c.enc.ObjectProps)
	c.a.Op(arm64.Add, a64C, a64C, a64B, true)
	c.movImm(a64B, uint64(v.Key)|uint64(add.Flags)<<32)
	c.a.Store(a64C, c.enc.PropertyKey, a64B)
	c.a.FMovFromF(a64B, a64F1)
	c.a.Store(a64C, c.enc.PropertyValue+c.enc.RefOffset, a64B)
	c.a.Store(a64C, c.enc.PropertyValue+c.enc.NumOffset, c.gpr(x, a64A))
	c.a.Bind(done)
}

// property finds the property a property operation names and leaves the
// address of its value in A, as amd64's does. It uses B, C and D.
func (c *a64Compiler) property(v *ssa.Value, guard func(arm64.Cond)) {
	if v.Holders() != nil || v.Cases() != nil {
		c.holder(v, guard)
		return
	}
	found := c.a.NewLabel()
	if v.Const.Bits != 0 {
		// As amd64's: the shape the site knows inline, any other out of line.
		scan := c.a.NewLabel()
		p := c.gpr(v.Args[0], a64A)
		c.a.Load(a64B, p, c.enc.ObjectShape)
		c.movImm(a64C, v.Const.Bits)
		c.a.Cmp(a64B, a64C, true)
		c.a.BCond(arm64.NE, scan)
		c.a.Load(a64A, p, c.enc.ObjectProps)
		c.a.AddImm(a64A, a64A, int64(int32(v.Index)*c.enc.PropertySize+c.enc.PropertyValue), true)
		c.cold = append(c.cold, func() {
			c.a.Bind(scan)
			c.scan(v, guard, found)
		})
		c.a.Bind(found)
		return
	}
	c.scan(v, guard, found)
	c.a.Bind(found)
}

// scan searches an object's small table for the key a property operation
// names, as amd64's does, going to found with the value's address in A.
func (c *a64Compiler) scan(v *ssa.Value, guard func(arm64.Cond), found arm64.Label) {
	p := c.gpr(v.Args[0], a64A)
	c.a.LoadU8(a64B, p, c.enc.ObjectClass)
	c.a.CmpImm(a64B, int64(c.enc.ClassObject), false)
	if v.Op == ssa.OpPropWrite {
		guard(arm64.NE)
	} else {
		// As amd64's: a read searches a function's table too.
		object := c.a.NewLabel()
		c.a.BCond(arm64.EQ, object)
		c.a.CmpImm(a64B, int64(c.enc.ClassFunction), false)
		guard(arm64.NE)
		c.a.Bind(object)
	}
	c.a.Load(a64B, p, c.enc.ObjectProps+8)
	c.a.CmpImm(a64B, abi.MaxScan, true)
	guard(arm64.HI)
	c.a.Load(a64A, p, c.enc.ObjectProps)
	mask, want := int64(c.enc.PropNotData), int64(0)
	if v.Op == ssa.OpPropWrite {
		mask, want = int64(c.enc.PropNotWritable), int64(c.enc.PropWritable)
	}
	c.movImm(a64D, uint64(mask))
	// A loop over the table, as amd64's.
	loop, hit := c.a.NewLabel(), c.a.NewLabel()
	c.a.Bind(loop)
	c.a.CmpImm(a64B, 0, true)
	guard(arm64.EQ)
	c.a.LoadU32(a64C, a64A, c.enc.PropertyKey)
	c.a.CmpImm(a64C, int64(v.Key), false)
	c.a.BCond(arm64.EQ, hit)
	c.a.AddImm(a64A, a64A, int64(c.enc.PropertySize), true)
	c.a.AddImm(a64B, a64B, -1, true)
	c.a.B(loop)
	c.a.Bind(hit)
	c.a.LoadU8(a64C, a64A, c.enc.PropertyFlags)
	c.a.Op(arm64.And, a64C, a64C, a64D, false)
	c.a.CmpImm(a64C, want, false)
	guard(arm64.NE)
	c.a.AddImm(a64A, a64A, int64(c.enc.PropertyValue), true)
	c.a.B(found)
}

// stringBytes compares two strings of one length by their bytes, as
// amd64's does: their pointers in F1 and C, the length in A. The loop
// borrows the frame's operand base, which it reloads from the context
// before it leaves.
func (c *a64Compiler) stringBytes(exit, yes, no arm64.Label) {
	c.a.CmpImm(a64A, abi.MaxEqualUnits, true)
	c.a.BCond(arm64.HI, exit)
	c.a.FMovFromF(a64A, a64F1)
	for _, s := range []arm64.Reg{a64A, a64C} {
		c.a.Load(a64B, s, c.enc.StringLeft)
		c.a.Cbnz(a64B, exit, true)
	}
	c.a.Load(a64B, a64C, c.enc.StringData+8)
	c.a.Load(a64D, a64A, c.enc.StringData+8)
	c.a.Cmp(a64D, a64B, true)
	c.a.BCond(arm64.NE, no)
	c.a.Load(a64A, a64A, c.enc.StringData)
	c.a.Load(a64C, a64C, c.enc.StringData)
	words, bytes, same, differ := c.a.NewLabel(), c.a.NewLabel(), c.a.NewLabel(), c.a.NewLabel()
	c.a.Bind(words)
	c.a.CmpImm(a64B, 8, true)
	c.a.BCond(arm64.LO, bytes)
	c.a.Load(a64D, a64A, 0)
	c.a.Load(a64Stack, a64C, 0)
	c.a.Cmp(a64D, a64Stack, true)
	c.a.BCond(arm64.NE, differ)
	c.a.AddImm(a64A, a64A, 8, true)
	c.a.AddImm(a64C, a64C, 8, true)
	c.a.AddImm(a64B, a64B, -8, true)
	c.a.B(words)
	c.a.Bind(bytes)
	c.a.Cbz(a64B, same, true)
	c.a.LoadU8(a64D, a64A, 0)
	c.a.LoadU8(a64Stack, a64C, 0)
	c.a.Cmp(a64D, a64Stack, false)
	c.a.BCond(arm64.NE, differ)
	c.a.AddImm(a64A, a64A, 1, true)
	c.a.AddImm(a64C, a64C, 1, true)
	c.a.AddImm(a64B, a64B, -1, true)
	c.a.B(bytes)
	for _, l := range []struct{ at, to arm64.Label }{{same, yes}, {differ, no}} {
		c.a.Bind(l.at)
		c.a.Load(a64Stack, a64Ctx, abi.OffStack)
		c.a.B(l.to)
	}
}

// call calls natively the function a call calls, as amd64's does. Once the
// state is recorded and what is live across the call saved, the frame is
// made in registers the allocator gives values, X3 to X8.
// arrayLiteral is amd64's: the pool's next array, the operands written in
// as its elements, its length set, the result set last.
func (c *a64Compiler) arrayLiteral(v *ssa.Value, site *ssa.CallSite) {
	vs := int32(c.enc.ValueSize)
	if vs != 16 {
		panic("mir: a value that is not 16 bytes")
	}
	at := abi.OffKeep + int32(v.Index)*vs
	c.movImm(a64A, uint64(site.Pool))
	c.a.Load(a64B, a64A, abi.OffPoolCount)
	c.a.AddImm(a64B, a64B, -1, true)
	c.a.Store(a64A, abi.OffPoolCount, a64B)
	c.a.ShiftImm(arm64.Lsl, a64B, a64B, 3, true)
	c.a.Op(arm64.Add, a64B, a64B, a64A, true)
	c.a.Load(a64C, a64B, abi.OffPoolObjects)
	c.a.Store(a64B, abi.OffPoolObjects, arm64.ZR)
	c.a.Store(a64Ctx, at+c.enc.RefOffset, a64C)
	c.movImm(a64A, c.enc.Object)
	c.a.Store(a64Ctx, at+c.enc.NumOffset, a64A)
	c.arrayElements(v, false)
}

// arrayElements is amd64's: the elements written, by Go while the
// collector marks (viaGo), the length set, the result set last.
func (c *a64Compiler) arrayElements(v *ssa.Value, viaGo bool) {
	vs := int32(c.enc.ValueSize)
	at := abi.OffKeep + int32(v.Index)*vs
	for i, x := range v.Args {
		var w arm64.Reg
		if remat(x) {
			c.materialize(x, a64A)
			w = a64A
		} else {
			w = c.gpr(x, a64A)
		}
		c.a.MovRR(a64D, w)
		number, have := c.a.NewLabel(), c.a.NewLabel()
		if x.Shadow != nil || c.origin.At(x) >= 0 {
			c.movImm(a64B, abi.NumberLimit)
			c.a.Cmp(w, a64B, true)
			c.a.BCond(arm64.LO, number)
		}
		c.pointerWord(x, w)
		c.a.B(have)
		c.a.Bind(number)
		c.movImm(a64B, 0)
		c.a.Bind(have)
		if viaGo {
			// goStore's: the cell's address in A, the words in F0 and C.
			c.a.FMovToF(a64F0, a64D)
			c.a.MovRR(a64C, a64B)
			c.a.Load(a64A, a64Ctx, at+c.enc.RefOffset)
			c.a.Load(a64A, a64A, c.enc.ObjectElems)
			c.a.AddImm(a64A, a64A, int64(int32(i)*vs), true)
			c.goStore(nil)
			continue
		}
		c.a.Load(a64C, a64Ctx, at+c.enc.RefOffset)
		c.a.Load(a64A, a64C, c.enc.ObjectElems)
		c.a.Store(a64A, int32(i)*vs+c.enc.NumOffset, a64D)
		c.a.Store(a64A, int32(i)*vs+c.enc.RefOffset, a64B)
	}
	c.a.Load(a64C, a64Ctx, at+c.enc.RefOffset)
	c.movImm(a64B, uint64(len(v.Args)))
	c.a.Store(a64C, c.enc.ObjectElems+8, a64B)
	c.movImm(a64A, c.enc.Object)
	c.setG(v, a64A)
}

// poolTake is amd64's: the pool's last object is the result.
func (c *a64Compiler) poolTake(v *ssa.Value, site *ssa.CallSite) {
	vs := int32(c.enc.ValueSize)
	c.movImm(a64A, uint64(site.Pool))
	c.a.Load(a64B, a64A, abi.OffPoolCount)
	c.a.AddImm(a64B, a64B, -1, true)
	c.a.Store(a64A, abi.OffPoolCount, a64B)
	c.a.ShiftImm(arm64.Lsl, a64B, a64B, 3, true)
	c.a.Op(arm64.Add, a64B, a64B, a64A, true)
	c.a.Load(a64C, a64B, abi.OffPoolObjects)
	c.a.Store(a64B, abi.OffPoolObjects, arm64.ZR)
	at := abi.OffKeep + int32(v.Index)*vs
	c.a.Store(a64Ctx, at+c.enc.RefOffset, a64C)
	c.movImm(a64A, c.enc.Object)
	c.a.Store(a64Ctx, at+c.enc.NumOffset, a64A)
	c.setG(v, a64A)
}

func (c *a64Compiler) call(v *ssa.Value, guard func(arm64.Cond)) {
	sites, s := v.Calls(), v.State
	stub := c.stubLabel(s, exitKind(v.Aux))
	if len(sites) == 0 {
		c.a.B(stub)
		return
	}
	c.callV, c.callTop = v, c.a.NewLabel()
	c.a.Bind(c.callTop)
	defer func() { c.callV = nil }()
	site := sites[0]
	vs := int32(c.enc.ValueSize)
	if site.Go != 0 {
		c.goCall(v, site, c.guardFor(v))
		return
	}
	if site.Literal {
		// As amd64's: an object literal's object, its pool's next, by Go
		// while the collector marks.
		c.movImm(a64A, uint64(site.Pool))
		c.a.Load(a64B, a64A, abi.OffPoolCount)
		c.refillOnEmpty(site.Pool, stub)
		if join, ok := c.markingTake(v, site, stub); ok {
			defer c.a.Bind(join)
		}
		if site.Argc == 0 {
			c.poolTake(v, site)
			return
		}
		c.arrayLiteral(v, site)
		return
	}
	sp := len(s.Slots)
	calleeSlot := sp - site.Argc - 1
	callee := v.Args[len(v.Args)-site.Argc-1]
	if !site.Alloc && !site.Receiver {
		// As amd64's: while the collector marks, Go makes the call.
		if join, ok := c.markingCall(v, site, stub); ok {
			defer c.a.Bind(join)
		}
	}
	c.a.Load(a64B, a64Ctx, abi.OffLevel)
	c.a.AddImm(a64B, a64B, 1, true)
	c.a.Load(a64C, a64Ctx, abi.OffLevelLimit)
	c.a.Cmp(a64B, a64C, true)
	guard(arm64.HS)
	if callee.Shadow == nil && c.origin.At(callee) < 0 || site.Via != 0 && (len(sites) != 1 || !c.operandsLive(s, calleeSlot-1)) {
		c.a.B(stub)
		return
	}
	c.movImm(a64B, c.enc.Object)
	c.a.Cmp(c.gpr(callee, a64A), a64B, true)
	guard(arm64.NE)
	c.sourceRef(callee, guard)
	checked := c.a.NewLabel()
	for k, t := range sites {
		next := c.a.NewLabel()
		want := t.Callee
		if t.Via != 0 {
			want = t.Via
		}
		if t.ByCode != 0 {
			c.byCode(t, next)
		} else {
			c.movImm(a64B, uint64(want))
			c.a.Cmp(a64C, a64B, true)
			c.a.BCond(arm64.NE, next)
		}
		if len(sites) > 1 {
			// As amd64's: which one (abi.Context's CallTarget).
			c.movImm(a64B, uint64(k))
			c.a.Store(a64Ctx, abi.OffCallTarget, a64B)
		}
		if t.Via != 0 {
			c.viaGuards(t, v.Args[0], guard)
		}
		if t.Pop {
			c.popGuards(t, v.Args[0], guard)
			c.a.B(checked)
			c.a.Bind(next)
			continue
		}
		if t.Push {
			c.pushGuards(t, v.Args[0], guard)
			c.a.B(checked)
			c.a.Bind(next)
			continue
		}
		if t.Alloc || t.Receiver {
			c.constructGuards(t, guard)
			c.a.B(checked)
			c.a.Bind(next)
			continue
		}
		// No code native callers may call is no reason to leave: Go runs
		// the callee once its frame is made (enterExit).
		if t.Coerce {
			recv := v.Args[0]
			if t.Via != 0 {
				recv = v.Args[2]
			}
			c.movImm(a64B, c.enc.Object)
			c.a.Cmp(c.gpr(recv, a64A), a64B, true)
			guard(arm64.NE)
		}
		c.a.Load(a64B, a64Ctx, abi.OffStackTop)
		c.a.Load(a64B, a64B, 0)
		c.a.AddImm(a64B, a64B, int64(t.LocalCount+t.MaxStack), true)
		c.a.Load(a64A, a64Ctx, abi.OffStackEnd)
		c.a.Cmp(a64B, a64A, true)
		guard(arm64.HI)
		if t.Pool != 0 {
			c.constructGuards(t, guard)
		}
		c.a.B(checked)
		c.a.Bind(next)
	}
	c.a.B(stub)
	c.a.Bind(checked)
	if site.Push {
		c.push(v)
		return
	}
	if site.Pop {
		c.pop(v)
		return
	}
	if site.Alloc || site.Receiver {
		if join, ok := c.markingTake(v, site, stub); ok {
			defer c.a.Bind(join)
		}
		c.poolTake(v, site)
		return
	}
	// As amd64's: a call of one function writes its operands where the
	// callee has them, unrecorded.
	direct, ops := len(sites) == 1 && (site.ThisSlot < 0 || site.Method || site.Pool != 0), calleeSlot
	if site.Method {
		ops--
	}
	direct = direct && c.operandsLive(s, ops)
	record := map[int]int32{}
	k := int32(0)
	for i, x := range s.Slots {
		if x == nil || x.Op == ssa.OpLoadSlot && x.Aux == i || c.captured(i) || direct && i >= ops {
			continue
		}
		var w arm64.Reg
		if remat(x) {
			c.materialize(x, a64A)
			w = a64A
		} else {
			w = c.gpr(x, a64A)
		}
		at := abi.OffRecord + k*32
		c.a.Store(a64Ctx, at+16, w)
		c.pointerWord(x, w)
		c.a.Store(a64Ctx, abi.OffRecordRef+k*8, a64B)
		c.movImm(a64B, uint64(i)|abi.RecordDirect)
		c.a.Store(a64Ctx, at, a64B)
		record[i] = k
		k++
	}
	high := c.a.NewLabel()
	c.movImm(a64A, uint64(k))
	c.a.Store(a64Ctx, abi.OffRecords, a64A)
	c.a.Load(a64B, a64Ctx, abi.OffRecordHi)
	c.a.Cmp(a64B, a64A, true)
	c.a.BCond(arm64.HS, high)
	c.a.Store(a64Ctx, abi.OffRecordHi, a64A)
	c.a.Bind(high)
	for _, f := range []struct {
		off int32
		v   uint64
	}{{abi.OffExitKind, abi.ExitHost}, {abi.OffExitPC, uint64(s.PC)}, {abi.OffExitDepth, uint64(s.Depth)}, {abi.OffExitSite, uint64(int64(s.Site))}} {
		c.movImm(a64A, f.v)
		c.a.Store(a64Ctx, f.off, a64A)
	}
	args, this := sp-site.Argc, calleeSlot-1
	if site.Via != 0 {
		args, this = args+1, args
	}
	// A receiver written straight into the callee's context: one it reads,
	// or a method call's, which a callee that does not read it has too --
	// a frame Go makes for it (jitGoCallNested, jitUnwindNative) shows it
	// in a stack trace, as the interpreter's does.
	receiver := direct && site.Pool == 0 && (site.ThisSlot >= 0 || site.Method && this < sp)
	if direct {
		// The callee's frame's address, in D, which pointerWord does not
		// use.
		c.a.Load(a64D, a64Ctx, abi.OffStackTop)
		c.a.Load(a64D, a64D, 0)
		c.a.ShiftImm(arm64.Lsl, a64D, a64D, 4, true)
		c.a.Load(a64B, a64Ctx, abi.OffStackBase)
		c.a.Op(arm64.Add, a64D, a64D, a64B, true)
		for i := 0; i < site.Params && args+i < sp && i < site.LocalCount; i++ {
			c.directOperand(s.Slots[args+i], a64D, int32(i)*vs)
		}
		if receiver {
			// The callee's context's, the next.
			c.a.Load(a64D, a64Ctx, abi.OffNext)
			c.directOperand(s.Slots[this], a64D, abi.OffThis)
		}
	}
	words := func(slot int) (nb arm64.Reg, nd int32, rb arm64.Reg, rd int32) {
		if k, ok := record[slot]; ok {
			return a64Ctx, abi.OffRecord + k*32 + 16, a64Ctx, abi.OffRecordRef + k*8
		}
		nb, nd = c.slotAddr(slot, false, a64C)
		rb, rd = c.slotAddr(slot, true, a64C)
		return nb, nd, rb, rd
	}
	for _, sv := range c.savesOf(v) {
		if sv.float {
			c.a.StoreF(a64Ctx, c.spillDisp(sv.slot), arm64.FReg(sv.reg))
		} else {
			c.a.Store(a64Ctx, c.spillDisp(sv.slot), arm64.Reg(sv.reg))
		}
	}
	const calleeCtx, base, locals, tmp, top, high2 = arm64.Reg(3), arm64.Reg(4), arm64.Reg(5), arm64.Reg(6), arm64.Reg(7), arm64.Reg(8)
	c.a.Load(calleeCtx, a64Ctx, abi.OffNext)
	c.a.Load(base, a64Ctx, abi.OffStackTop)
	c.a.Load(base, base, 0)
	c.a.Load(locals, a64Ctx, abi.OffStackBase)
	c.a.ShiftImm(arm64.Lsl, tmp, base, 4, true)
	c.a.Op(arm64.Add, locals, locals, tmp, true)
	c.a.Store(calleeCtx, abi.OffLocals, locals)
	// As amd64's: what the callee's context shares, Go wrote.
	c.a.Store(calleeCtx, abi.OffUpvalues, arm64.ZR)
	c.a.Store(calleeCtx, abi.OffOpen, arm64.ZR)
	c.a.Store(calleeCtx, abi.OffTailReturn, arm64.ZR)
	c.a.Store(calleeCtx, abi.OffBase, base)
	c.movImm(tmp, 1)
	c.a.Store(calleeCtx, abi.OffLive, tmp)
	back := c.a.NewLabel()
	c.a.Adr(tmp, back)
	c.a.Store(calleeCtx, abi.OffReturnTo, tmp)
	for i, t := range sites {
		next := c.a.NewLabel()
		if i < len(sites)-1 {
			c.a.Load(top, a64Ctx, abi.OffCallTarget)
			c.a.CmpImm(top, int64(i), true)
			c.a.BCond(arm64.NE, next)
		}
		c.a.AddImm(top, locals, int64(t.LocalCount)*int64(vs), true)
		c.a.Store(calleeCtx, abi.OffStack, top)
		if t.Tail {
			c.movImm(top, b2u(t.TailFrame))
			c.a.Store(calleeCtx, abi.OffTailFrame, top)
			c.a.Store(calleeCtx, abi.OffFrameThis, arm64.ZR)
		}
		for i := 0; i < t.LocalCount; i++ {
			at := int32(i) * vs
			// The arguments, past the receiver of a call through
			// Function.prototype.call.
			argc, first := t.Argc, sp-t.Argc
			if t.Via != 0 {
				argc, first = argc-1, first+1
			}
			if i < t.Params && i < argc && direct {
				continue
			}
			if i < t.Params && i < argc {
				nb, nd, rb, rd := words(first + i)
				c.a.Load(top, nb, nd)
				c.a.Store(locals, at+c.enc.NumOffset, top)
				c.a.Load(top, rb, rd)
				c.a.Store(locals, at+c.enc.RefOffset, top)
				continue
			}
			c.movImm(top, c.enc.Undefined)
			c.a.Store(locals, at+c.enc.NumOffset, top)
			c.a.Store(locals, at+c.enc.RefOffset, arm64.ZR)
		}
		if t.Pool != 0 {
			// As amd64's: the pool's last object, its cell cleared.
			c.movImm(tmp, uint64(t.Pool))
			c.a.Load(top, tmp, abi.OffPoolCount)
			c.a.AddImm(top, top, -1, true)
			c.a.Store(tmp, abi.OffPoolCount, top)
			c.a.ShiftImm(arm64.Lsl, top, top, 3, true)
			c.a.Op(arm64.Add, top, top, tmp, true)
			c.a.Load(high2, top, abi.OffPoolObjects)
			c.a.Store(top, abi.OffPoolObjects, arm64.ZR)
			c.a.Store(calleeCtx, abi.OffThis+c.enc.RefOffset, high2)
			c.movImm(tmp, c.enc.Object)
			c.a.Store(calleeCtx, abi.OffThis+c.enc.NumOffset, tmp)
		} else if (t.ThisSlot >= 0 || site.Method && site.Via == 0) && !direct {
			nb, nd, rb, rd := words(calleeSlot - 1)
			c.a.Load(top, nb, nd)
			c.a.Store(calleeCtx, abi.OffThis+c.enc.NumOffset, top)
			c.a.Load(top, rb, rd)
			c.a.Store(calleeCtx, abi.OffThis+c.enc.RefOffset, top)
		} else if !receiver {
			// A plain call's receiver, undefined, as the interpreter's frame
			// has it.
			c.movImm(top, c.enc.Undefined)
			c.a.Store(calleeCtx, abi.OffThis+c.enc.NumOffset, top)
			c.a.Store(calleeCtx, abi.OffThis+c.enc.RefOffset, arm64.ZR)
		}
		if t.ByCode != 0 {
			// As amd64's: the closure the checks found, and its captured
			// bindings.
			c.a.Load(top, a64Ctx, abi.OffCallClosure)
			c.a.Store(calleeCtx, abi.OffClosure, top)
			c.a.Load(top, a64Ctx, abi.OffCallUpvalues)
			c.a.Store(calleeCtx, abi.OffUpvalues, top)
		} else {
			c.movImm(top, uint64(t.Closure))
			c.a.Store(calleeCtx, abi.OffClosure, top)
			if t.Upvalues != 0 {
				c.movImm(top, uint64(t.Upvalues))
				c.a.Store(calleeCtx, abi.OffUpvalues, top)
			}
		}
		if t.Pool != 0 {
			// A construction's new.target, for Go to make its frame with.
			c.movImm(top, uint64(t.Callee))
			c.a.Store(calleeCtx, abi.OffNewTarget, top)
		}
		if t.Count != 0 {
			c.movImm(top, uint64(t.Count))
			c.a.Load(high2, top, 0)
			c.a.AddImm(high2, high2, 1, true)
			c.a.Store(top, 0, high2)
		}
		same := c.a.NewLabel()
		c.a.AddImm(top, base, int64(t.LocalCount+t.MaxStack), true)
		c.a.Load(tmp, a64Ctx, abi.OffStackTop)
		c.a.Store(tmp, 0, top)
		c.a.Load(tmp, a64Ctx, abi.OffStackHigh)
		c.a.Load(high2, tmp, 0)
		c.a.Cmp(top, high2, true)
		c.a.BCond(arm64.LS, same)
		c.a.Store(tmp, 0, top)
		c.a.Bind(same)
		if t.ByCode != 0 {
			c.a.Load(high2, a64Ctx, abi.OffCallCallee)
		}
		c.movImm(a64A, uint64(t.Entry))
		c.a.Load(a64A, a64A, 0)
		c.a.MovRR(a64Ctx, calleeCtx)
		enter := c.a.NewLabel()
		c.a.Cbz(a64A, enter, true)
		c.a.Br(a64A)
		c.a.Bind(enter)
		if t.ByCode != 0 {
			c.a.MovRR(a64A, high2)
		} else {
			c.movImm(a64A, uint64(t.Callee))
		}
		if !c.enterUsed {
			c.enter, c.enterUsed = c.a.NewLabel(), true
		}
		c.a.B(c.enter)
		c.a.Bind(next)
	}
	c.a.Bind(back)
	c.a.MovRR(calleeCtx, a64Ctx)
	c.a.Load(a64Ctx, a64Ctx, abi.OffPrev)
	c.a.Load(a64Locals, a64Ctx, abi.OffLocals)
	c.a.Load(a64Stack, a64Ctx, abi.OffStack)
	at := abi.OffKeep + int32(v.Index)*vs
	if site.Pool != 0 {
		// As amd64's: a result that is not an object is the receiver.
		object := c.a.NewLabel()
		c.a.Load(a64A, calleeCtx, abi.OffRetValue+c.enc.NumOffset)
		c.movImm(a64B, c.enc.Object)
		c.a.Cmp(a64A, a64B, true)
		c.a.BCond(arm64.EQ, object)
		c.a.Load(tmp, calleeCtx, abi.OffThis+c.enc.RefOffset)
		c.a.Store(calleeCtx, abi.OffRetValue+c.enc.RefOffset, tmp)
		c.a.Load(tmp, calleeCtx, abi.OffThis+c.enc.NumOffset)
		c.a.Store(calleeCtx, abi.OffRetValue+c.enc.NumOffset, tmp)
		c.a.Bind(object)
	}
	c.a.Load(tmp, calleeCtx, abi.OffRetValue+c.enc.RefOffset)
	c.a.Store(a64Ctx, at+c.enc.RefOffset, tmp)
	c.a.Load(a64A, calleeCtx, abi.OffRetValue+c.enc.NumOffset)
	c.a.Store(a64Ctx, at+c.enc.NumOffset, a64A)
	c.a.Load(tmp, a64Ctx, abi.OffStackTop)
	c.a.Load(top, calleeCtx, abi.OffBase)
	c.a.Store(tmp, 0, top)
	c.a.Store(calleeCtx, abi.OffLive, arm64.ZR)
	c.a.Store(calleeCtx, abi.OffReturnTo, arm64.ZR)
	for _, sv := range c.savesOf(v) {
		if sv.float {
			c.a.LoadF(arm64.FReg(sv.reg), a64Ctx, c.spillDisp(sv.slot))
		} else {
			c.a.Load(arm64.Reg(sv.reg), a64Ctx, c.spillDisp(sv.slot))
		}
	}
	c.setG(v, a64A)
}

// pushGuards checks a call of Array.prototype.push may append, as amd64's
// does. It uses A, B and C.
func (c *a64Compiler) pushGuards(t *ssa.CallSite, recv *ssa.Value, guard func(arm64.Cond)) {
	if recv.Shadow == nil && c.origin.At(recv) < 0 {
		c.a.CmpImm(a64C, 0, true)
		guard(arm64.NE)
		return
	}
	c.movImm(a64B, c.enc.Object)
	c.a.Cmp(c.gpr(recv, a64A), a64B, true)
	guard(arm64.NE)
	c.sourceRef(recv, guard)
	c.a.LoadU8(a64B, a64C, c.enc.ObjectClass)
	c.a.CmpImm(a64B, int64(c.enc.ClassArray), false)
	guard(arm64.NE)
	c.a.LoadU8(a64B, a64C, c.enc.ObjectFlags)
	want := c.enc.FlagExtensible | c.enc.FlagLengthWritable
	c.movImm(a64A, uint64(want|c.enc.FlagSparse))
	c.a.Op(arm64.And, a64B, a64B, a64A, false)
	c.a.CmpImm(a64B, int64(want), false)
	guard(arm64.NE)
	c.a.Load(a64B, a64C, c.enc.ObjectElems+8)
	c.a.Load(a64A, a64C, c.enc.ObjectElems+16)
	c.a.Cmp(a64B, a64A, true)
	guard(arm64.HS)
	c.a.Load(a64B, a64C, c.enc.ObjectProto)
	c.movImm(a64A, uint64(t.Protos[0].Object))
	c.a.Cmp(a64B, a64A, true)
	guard(arm64.NE)
	for i, h := range t.Protos {
		c.movImm(a64A, uint64(h.Object))
		c.a.Load(a64B, a64A, c.enc.ObjectShape)
		c.movImm(a64C, uint64(h.Shape))
		c.a.Cmp(a64B, a64C, true)
		guard(arm64.NE)
		c.a.Load(a64B, a64A, c.enc.ObjectElems+8)
		c.a.CmpImm(a64B, 0, true)
		guard(arm64.NE)
		c.a.LoadU8(a64B, a64A, c.enc.ObjectFlags)
		c.movImm(a64C, uint64(c.enc.FlagSparse))
		c.a.Tst(a64B, a64C, false)
		guard(arm64.NE)
		c.a.Load(a64B, a64A, c.enc.ObjectProto)
		if i == 0 {
			c.movImm(a64C, uint64(t.Protos[1].Object))
			c.a.Cmp(a64B, a64C, true)
		} else {
			c.a.CmpImm(a64B, 0, true)
		}
		guard(arm64.NE)
	}
}

// popGuards checks a call of Array.prototype.pop may pop, as amd64's
// does. It uses A, B and C.
func (c *a64Compiler) popGuards(t *ssa.CallSite, recv *ssa.Value, guard func(arm64.Cond)) {
	if recv.Shadow == nil && c.origin.At(recv) < 0 {
		c.a.CmpImm(a64C, 0, true)
		guard(arm64.NE)
		return
	}
	c.movImm(a64B, c.enc.Object)
	c.a.Cmp(c.gpr(recv, a64A), a64B, true)
	guard(arm64.NE)
	c.sourceRef(recv, guard)
	c.a.LoadU8(a64B, a64C, c.enc.ObjectClass)
	c.a.CmpImm(a64B, int64(c.enc.ClassArray), false)
	guard(arm64.NE)
	c.a.LoadU8(a64B, a64C, c.enc.ObjectFlags)
	c.movImm(a64A, uint64(c.enc.FlagLengthWritable|c.enc.FlagSparse))
	c.a.Op(arm64.And, a64B, a64B, a64A, false)
	c.a.CmpImm(a64B, int64(c.enc.FlagLengthWritable), false)
	guard(arm64.NE)
	c.a.Load(a64B, a64C, c.enc.ObjectProto)
	c.movImm(a64A, uint64(t.Protos[0].Object))
	c.a.Cmp(a64B, a64A, true)
	guard(arm64.NE)
	c.a.Load(a64B, a64C, c.enc.ObjectElems+8)
	c.a.CmpImm(a64B, 0, true)
	guard(arm64.EQ)
	c.a.AddImm(a64B, a64B, -1, true)
	c.a.ShiftImm(arm64.Lsl, a64B, a64B, 4, true)
	c.a.Load(a64A, a64C, c.enc.ObjectElems)
	c.a.Op(arm64.Add, a64A, a64A, a64B, true)
	c.a.Load(a64B, a64A, c.enc.NumOffset)
	c.movImm(a64C, c.enc.Uninitialized)
	c.a.Cmp(a64B, a64C, true)
	guard(arm64.EQ)
}

// pop pops a call of Array.prototype.pop's receiver's last element, as
// amd64's does. It uses A, B and C.
func (c *a64Compiler) pop(v *ssa.Value) {
	if c.enc.ValueSize != 16 {
		panic("mir: a value that is not 16 bytes")
	}
	c.sourceRef(v.Args[0], func(arm64.Cond) {})
	c.a.Load(a64B, a64C, c.enc.ObjectElems+8)
	c.a.AddImm(a64B, a64B, -1, true)
	c.a.Store(a64C, c.enc.ObjectElems+8, a64B)
	c.a.ShiftImm(arm64.Lsl, a64B, a64B, 4, true)
	c.a.Load(a64A, a64C, c.enc.ObjectElems)
	c.a.Op(arm64.Add, a64A, a64A, a64B, true)
	at := abi.OffKeep + int32(v.Index)*c.enc.ValueSize
	c.a.Load(a64B, a64A, c.enc.RefOffset)
	c.a.Store(a64Ctx, at+c.enc.RefOffset, a64B)
	c.a.Load(a64B, a64A, c.enc.NumOffset)
	c.a.Store(a64Ctx, at+c.enc.NumOffset, a64B)
	c.movImm(a64C, c.enc.Undefined)
	c.a.Store(a64A, c.enc.NumOffset, a64C)
	c.a.Store(a64A, c.enc.RefOffset, arm64.ZR)
	c.setG(v, a64B)
}

// push appends a call of Array.prototype.push's argument, as amd64's
// does. It uses A, B, C, D and F1.
func (c *a64Compiler) push(v *ssa.Value) {
	vs := int32(c.enc.ValueSize)
	recv, x := v.Args[0], v.Args[2]
	if vs != 16 {
		panic("mir: a value that is not 16 bytes")
	}
	var w arm64.Reg
	if remat(x) {
		c.materialize(x, a64A)
		w = a64A
	} else {
		w = c.gpr(x, a64A)
	}
	c.a.MovRR(a64D, w)
	number, have := c.a.NewLabel(), c.a.NewLabel()
	if x.Shadow != nil || c.origin.At(x) >= 0 {
		c.movImm(a64B, abi.NumberLimit)
		c.a.Cmp(w, a64B, true)
		c.a.BCond(arm64.LO, number)
	}
	c.pointerWord(x, w)
	c.a.B(have)
	c.a.Bind(number)
	c.movImm(a64B, 0)
	c.a.Bind(have)
	c.a.FMovToF(a64F1, a64B)
	c.sourceRef(recv, func(arm64.Cond) {})
	c.a.Load(a64B, a64C, c.enc.ObjectElems+8)
	c.a.AddImm(a64B, a64B, 1, true)
	c.a.Store(a64C, c.enc.ObjectElems+8, a64B)
	c.a.Load(a64A, a64C, c.enc.ObjectElems)
	c.a.AddImm(a64C, a64B, -1, true)
	c.a.ShiftImm(arm64.Lsl, a64C, a64C, 4, true)
	c.a.Op(arm64.Add, a64A, a64A, a64C, true)
	c.a.Store(a64A, c.enc.NumOffset, a64D)
	c.a.FMovFromF(a64C, a64F1)
	c.a.Store(a64A, c.enc.RefOffset, a64C)
	c.a.Scvtf(a64F1, a64B, true)
	c.a.FMovFromF(a64A, a64F1)
	at := abi.OffKeep + int32(v.Index)*vs
	c.a.Store(a64Ctx, at+c.enc.NumOffset, a64A)
	c.a.Store(a64Ctx, at+c.enc.RefOffset, arm64.ZR)
	c.setG(v, a64A)
}

// viaGuards checks a call through Function.prototype.call calls its
// function, as amd64's does. It uses A, B and C.
func (c *a64Compiler) viaGuards(t *ssa.CallSite, recv *ssa.Value, guard func(arm64.Cond)) {
	if recv.Shadow == nil && c.origin.At(recv) < 0 {
		c.a.CmpImm(a64C, 0, true)
		guard(arm64.NE)
		return
	}
	c.movImm(a64B, c.enc.Object)
	c.a.Cmp(c.gpr(recv, a64A), a64B, true)
	guard(arm64.NE)
	c.sourceRef(recv, guard)
	c.movImm(a64B, uint64(t.Callee))
	c.a.Cmp(a64C, a64B, true)
	guard(arm64.NE)
}

// constructGuards checks a construction can take its receiver from its
// pool, as amd64's does; the function's pointer is in C. It uses A, B and
// C.
func (c *a64Compiler) constructGuards(t *ssa.CallSite, guard func(arm64.Cond)) {
	c.movImm(a64A, uint64(t.Pool))
	c.a.Load(a64B, a64A, abi.OffPoolCount)
	if c.callV != nil {
		c.refillOnEmpty(t.Pool, c.stubLabel(c.callV.State, exitKind(c.callV.Aux)))
	} else {
		c.a.CmpImm(a64B, 0, true)
		guard(arm64.EQ)
	}
	if t.Alloc {
		return
	}
	c.a.Load(a64B, a64C, c.enc.ObjectProps+8)
	c.a.CmpImm(a64B, int64(t.ProtoIndex), true)
	guard(arm64.LS)
	at := int32(t.ProtoIndex) * c.enc.PropertySize
	c.a.Load(a64B, a64C, c.enc.ObjectProps)
	c.a.LoadU32(a64C, a64B, at+c.enc.PropertyKey)
	c.movImm(a64A, uint64(t.ProtoKey))
	c.a.Cmp(a64C, a64A, false)
	guard(arm64.NE)
	c.a.Load(a64C, a64B, at+c.enc.PropertyValue+c.enc.RefOffset)
	c.movImm(a64A, uint64(t.Pool))
	c.a.Load(a64B, a64A, abi.OffPoolProto)
	c.a.Cmp(a64C, a64B, true)
	guard(arm64.NE)
}

// directOperand writes a call's operand x, both words, at disp from base
// -- the callee's frame or this context -- as amd64's does. It uses A, B
// and C.
func (c *a64Compiler) directOperand(x *ssa.Value, base arm64.Reg, disp int32) {
	var w arm64.Reg
	if remat(x) {
		c.materialize(x, a64A)
		w = a64A
	} else {
		w = c.gpr(x, a64A)
	}
	c.a.Store(base, disp+c.enc.NumOffset, w)
	c.pointerWord(x, w)
	c.a.Store(base, disp+c.enc.RefOffset, a64B)
}

// pointerWord leaves in B the pointer word of a value whose number word is
// in w, as amd64's does. It uses C.
func (c *a64Compiler) pointerWord(x *ssa.Value, w arm64.Reg) {
	done := c.a.NewLabel()
	c.movImm(a64B, 0)
	switch o, static := c.origin.Of(x); {
	case x.Shadow != nil && cellSource(x.Shadow):
		c.a.Load(a64B, c.gpr(x.Shadow, a64C), c.enc.RefOffset)
	case x.Shadow != nil:
		r := c.gpr(x.Shadow, a64C)
		c.a.Cbz(r, done, true)
		c.a.Load(a64B, r, c.enc.RefOffset)
	case static && o >= 0:
		c.isReference(x, w, o, done)
		base, disp := c.slotAddr(o, true, a64C)
		c.a.Load(a64B, base, disp)
	}
	c.a.Bind(done)
}

// goCall calls Go, as amd64's does.
func (c *a64Compiler) goCall(v *ssa.Value, site *ssa.CallSite, guard func(arm64.Cond)) {
	if site.Step && c.enc.IterState != 0 {
		slow, join := c.a.NewLabel(), c.a.NewLabel()
		c.iterStep(v, slow)
		c.a.B(join)
		c.a.Bind(slow)
		c.goCallOp(v, goOpWord(site), false)
		c.a.Bind(join)
		c.a.Load(a64A, a64Ctx, abi.OffKeep+int32(v.Index)*int32(c.enc.ValueSize)+c.enc.NumOffset)
		c.setG(v, a64A)
		return
	}
	if site.Go == ssa.GoClose {
		// As amd64's: only while closures have captured the frame's locals.
		skip := c.a.NewLabel()
		c.a.Load(a64A, a64Ctx, abi.OffOpen)
		c.a.Cbz(a64A, skip, true)
		c.goCallOp(v, goOpWord(site), false)
		c.a.Bind(skip)
		return
	}
	c.goCallOp(v, goOpWord(site), false)
}

// iterStep is amd64's: a loop's step over an array, made in place. It
// uses A, B, C and D, and no other.
func (c *a64Compiler) iterStep(v *ssa.Value, slow arm64.Label) {
	x := v.Args[0]
	c.pointerWord(x, c.gpr(x, a64A))
	c.a.Cbz(a64B, slow, true)
	c.movImm(a64C, c.enc.WriteBarrier)
	c.a.LoadU8(a64C, a64C, 0)
	c.a.Cbnz(a64C, slow, false)
	c.a.Load(a64C, a64B, c.enc.ObjectData)
	c.movImm(a64A, c.enc.IterState)
	c.a.Cmp(a64C, a64A, true)
	c.a.BCond(arm64.NE, slow)
	c.a.Load(a64D, a64B, c.enc.ObjectData+8)
	c.a.LoadU8(a64C, a64D, c.enc.IterDone)
	c.a.Cbnz(a64C, slow, false)
	c.a.Load(a64A, a64D, c.enc.IterIdx)
	c.a.Load(a64C, a64D, c.enc.IterArr)
	c.a.Cbz(a64C, slow, true)
	c.a.Load(a64B, a64C, c.enc.ObjectElems+8)
	c.a.Cmp(a64A, a64B, true)
	c.a.BCond(arm64.HS, slow)
	c.a.Load(a64B, a64C, c.enc.ObjectElems)
	c.a.ShiftImm(arm64.Lsl, a64A, a64A, 4, true)
	c.a.Op(arm64.Add, a64A, a64A, a64B, true)
	c.a.Load(a64B, a64A, c.enc.NumOffset)
	c.movImm(a64C, c.enc.Uninitialized)
	c.a.Cmp(a64B, a64C, true)
	c.a.BCond(arm64.EQ, slow)
	keep := abi.OffKeep + int32(v.Index)*int32(c.enc.ValueSize)
	c.a.Store(a64Ctx, keep+c.enc.NumOffset, a64B)
	c.a.Load(a64C, a64A, c.enc.RefOffset)
	c.a.Store(a64Ctx, keep+c.enc.RefOffset, a64C)
	c.a.Load(a64C, a64D, c.enc.IterIdx)
	c.a.AddImm(a64C, a64C, 1, true)
	c.a.Store(a64D, c.enc.IterIdx, a64C)
}

// goCallOp is goCall's, of the call of Go op, as amd64's, every
// allocatable register saved across it if saving.
// goArg writes x's words to the context's operand at at, as amd64's does.
func (c *a64Compiler) goArg(at int32, x *ssa.Value) {
	var w arm64.Reg
	if remat(x) {
		c.materialize(x, a64A)
		w = a64A
	} else {
		w = c.gpr(x, a64A)
	}
	c.a.Store(a64Ctx, at+c.enc.NumOffset, w)
	number, have := c.a.NewLabel(), c.a.NewLabel()
	if x.Shadow != nil || c.origin.At(x) >= 0 {
		c.movImm(a64B, abi.NumberLimit)
		c.a.Cmp(w, a64B, true)
		c.a.BCond(arm64.LO, number)
	}
	c.pointerWord(x, w)
	c.a.B(have)
	c.a.Bind(number)
	c.movImm(a64B, 0)
	c.a.Bind(have)
	c.a.Store(a64Ctx, at+c.enc.RefOffset, a64B)
}

// tailSelf is a call in tail position, as amd64's: deep in the stack, a
// call of the frame's own closure is a jump to the code's start, its
// arguments the frame's parameters; in a native callee's frame, one of a
// function the call has, a jump to its code, its frame in this one's
// place (tailJump).
func (c *a64Compiler) tailSelf(v *ssa.Value, guard func(arm64.Cond)) {
	shallow := c.a.NewLabel()
	c.a.Load(a64A, a64Ctx, abi.OffLevel)
	c.a.Load(a64B, a64Ctx, abi.OffTailLevel)
	c.a.Cmp(a64A, a64B, true)
	c.a.BCond(arm64.LO, shallow)
	defer c.a.Bind(shallow)
	args, entry := v.Args[1:], c.entryAt(0)
	var this *ssa.Value
	if v.Key&1 != 0 {
		args, this = args[:len(args)-1], args[len(args)-1]
	}
	stub := c.stubLabel(v.State, exitKind(v.Aux))
	if entry == nil || len(args)+1 > abi.MaxGoArgs {
		c.a.B(stub)
		return
	}
	poll, polled := c.a.NewLabel(), c.a.NewLabel()
	c.a.Load(a64B, a64Ctx, abi.OffBackEdges)
	c.a.Load(a64A, a64B, 0)
	c.a.AddImm(a64A, a64A, -1, true)
	c.a.Store(a64B, 0, a64A)
	c.a.CmpImm(a64A, 0, true)
	c.a.BCond(arm64.LE, poll)
	state := v.State
	c.cold = append(c.cold, func() {
		c.a.Bind(poll)
		if c.enc.CallGo != 0 {
			stub := c.stubLabel(state, abi.ExitPoll)
			c.goSaving(uint64(ssa.GoPoll), &stub)
			c.a.B(polled)
			return
		}
		c.exitTo(state, abi.ExitPoll)
	})
	c.a.Bind(polled)
	c.a.Load(a64A, a64Ctx, abi.OffTailFrame)
	c.a.CmpImm(a64A, 0, true)
	guard(arm64.EQ)
	c.movImm(a64B, c.enc.WriteBarrier)
	c.a.LoadU8(a64B, a64B, 0)
	c.a.CmpImm(a64B, 0, false)
	guard(arm64.NE)
	if !c.reference(v, v.Args[0], c.enc.Object, guard) {
		return
	}
	c.a.Store(a64Ctx, abi.OffCallCallee, a64C)
	size := int32(unsafe.Sizeof(abi.GoArg{}))
	for i, x := range args {
		c.goArg(abi.OffGoArgs+int32(i)*size, x)
	}
	if this != nil {
		c.goArg(abi.OffGoArgs+int32(len(args))*size, this)
	}
	other := c.a.NewLabel()
	c.a.Load(a64C, a64Ctx, abi.OffCallCallee)
	c.a.Load(a64B, a64C, c.enc.ObjectData)
	c.movImm(a64A, c.enc.FuncData)
	c.a.Cmp(a64B, a64A, true)
	guard(arm64.NE)
	c.a.Load(a64B, a64C, c.enc.ObjectData+8)
	c.a.Load(a64B, a64B, c.enc.FuncClosure)
	c.a.Load(a64A, a64Ctx, abi.OffClosure)
	c.a.Cmp(a64A, a64B, true)
	c.a.BCond(arm64.NE, other)
	for i := range int(v.Const.Bits) {
		nb, nd := c.slotAddr(i, false, a64C)
		rb, rd := c.slotAddr(i, true, a64C)
		if i < v.Index && i < len(args) {
			at := abi.OffGoArgs + int32(i)*size
			c.a.Load(a64A, a64Ctx, at+c.enc.NumOffset)
			c.a.Store(nb, nd, a64A)
			c.a.Load(a64A, a64Ctx, at+c.enc.RefOffset)
			c.a.Store(rb, rd, a64A)
			continue
		}
		c.movImm(a64A, c.enc.Undefined)
		c.a.Store(nb, nd, a64A)
		c.a.Store(rb, rd, arm64.ZR)
	}
	if v.Key&2 != 0 {
		at := abi.OffGoArgs + int32(len(args))*size
		none := c.a.NewLabel()
		c.a.Load(a64A, a64Ctx, at+c.enc.NumOffset)
		c.a.Load(a64B, a64Ctx, at+c.enc.RefOffset)
		c.a.Store(a64Ctx, abi.OffThis+c.enc.NumOffset, a64A)
		c.a.Store(a64Ctx, abi.OffThis+c.enc.RefOffset, a64B)
		c.a.Load(a64C, a64Ctx, abi.OffFrameThis)
		c.a.Cbz(a64C, none, true)
		c.a.Store(a64C, c.enc.NumOffset, a64A)
		c.a.Store(a64C, c.enc.RefOffset, a64B)
		c.a.Bind(none)
	}
	c.a.B(c.labels[entry.ID])
	c.a.Bind(other)
	var sites []*ssa.CallSite
	for _, t := range v.Calls() {
		if t.Go == 0 && !t.Literal && !t.Push && !t.Pop && !t.Alloc && !t.Receiver && t.Via == 0 && t.Pool == 0 && !t.Coerce && t.Entry != 0 {
			sites = append(sites, t)
		}
	}
	if len(sites) == 0 {
		c.a.B(stub)
		return
	}
	c.a.Load(a64A, a64Ctx, abi.OffReturnTo)
	c.a.Cbz(a64A, stub, true)
	for _, t := range sites {
		next := c.a.NewLabel()
		c.a.Load(a64C, a64Ctx, abi.OffCallCallee)
		if t.ByCode != 0 {
			c.byCode(t, next)
		} else {
			c.movImm(a64B, uint64(t.Callee))
			c.a.Cmp(a64C, a64B, true)
			c.a.BCond(arm64.NE, next)
		}
		c.tailJump(t, len(args), this != nil, guard)
		c.a.Bind(next)
	}
	c.a.B(stub)
}

// tailJump is tailSelf's call of t, as amd64's.
func (c *a64Compiler) tailJump(t *ssa.CallSite, n int, this bool, guard func(arm64.Cond)) {
	vs, size := int32(c.enc.ValueSize), int32(unsafe.Sizeof(abi.GoArg{}))
	c.movImm(a64A, uint64(t.Entry))
	c.a.Load(a64A, a64A, 0)
	c.a.CmpImm(a64A, 0, true)
	guard(arm64.EQ)
	c.a.Load(a64B, a64Ctx, abi.OffBase)
	c.a.AddImm(a64B, a64B, int64(t.LocalCount+t.MaxStack), true)
	c.a.Load(a64A, a64Ctx, abi.OffStackEnd)
	c.a.Cmp(a64B, a64A, true)
	guard(arm64.HI)
	for i := 0; i < t.LocalCount; i++ {
		at := int32(i) * vs
		if i < t.Params && i < n {
			g := abi.OffGoArgs + int32(i)*size
			c.a.Load(a64A, a64Ctx, g+c.enc.NumOffset)
			c.a.Store(a64Locals, at+c.enc.NumOffset, a64A)
			c.a.Load(a64A, a64Ctx, g+c.enc.RefOffset)
			c.a.Store(a64Locals, at+c.enc.RefOffset, a64A)
			continue
		}
		c.movImm(a64A, c.enc.Undefined)
		c.a.Store(a64Locals, at+c.enc.NumOffset, a64A)
		c.a.Store(a64Locals, at+c.enc.RefOffset, arm64.ZR)
	}
	if this {
		g := abi.OffGoArgs + int32(n)*size
		c.a.Load(a64A, a64Ctx, g+c.enc.NumOffset)
		c.a.Store(a64Ctx, abi.OffThis+c.enc.NumOffset, a64A)
		c.a.Load(a64A, a64Ctx, g+c.enc.RefOffset)
		c.a.Store(a64Ctx, abi.OffThis+c.enc.RefOffset, a64A)
	} else {
		c.movImm(a64A, c.enc.Undefined)
		c.a.Store(a64Ctx, abi.OffThis+c.enc.NumOffset, a64A)
		c.a.Store(a64Ctx, abi.OffThis+c.enc.RefOffset, arm64.ZR)
	}
	if t.ByCode != 0 {
		c.a.Load(a64A, a64Ctx, abi.OffCallClosure)
		c.a.Store(a64Ctx, abi.OffClosure, a64A)
		c.a.Load(a64A, a64Ctx, abi.OffCallUpvalues)
		c.a.Store(a64Ctx, abi.OffUpvalues, a64A)
	} else {
		c.movImm(a64A, uint64(t.Closure))
		c.a.Store(a64Ctx, abi.OffClosure, a64A)
		c.movImm(a64A, uint64(t.Upvalues))
		c.a.Store(a64Ctx, abi.OffUpvalues, a64A)
	}
	c.a.AddImm(a64A, a64Locals, int64(t.LocalCount)*int64(vs), true)
	c.a.Store(a64Ctx, abi.OffStack, a64A)
	c.a.Store(a64Ctx, abi.OffOpen, arm64.ZR)
	c.a.Store(a64Ctx, abi.OffTailReturn, arm64.ZR)
	if t.Tail {
		c.movImm(a64A, b2u(t.TailFrame))
		c.a.Store(a64Ctx, abi.OffTailFrame, a64A)
	}
	if t.Count != 0 {
		c.movImm(a64A, uint64(t.Count))
		c.a.Load(a64B, a64A, 0)
		c.a.AddImm(a64B, a64B, 1, true)
		c.a.Store(a64A, 0, a64B)
	}
	same := c.a.NewLabel()
	c.a.Load(a64B, a64Ctx, abi.OffBase)
	c.a.AddImm(a64B, a64B, int64(t.LocalCount+t.MaxStack), true)
	c.a.Load(a64A, a64Ctx, abi.OffStackTop)
	c.a.Store(a64A, 0, a64B)
	c.a.Load(a64A, a64Ctx, abi.OffStackHigh)
	c.a.Load(a64C, a64A, 0)
	c.a.Cmp(a64B, a64C, true)
	c.a.BCond(arm64.LS, same)
	c.a.Store(a64A, 0, a64B)
	c.a.Bind(same)
	c.movImm(a64A, uint64(t.Entry))
	c.a.Load(a64A, a64A, 0)
	c.a.Br(a64A)
}

func (c *a64Compiler) goCallOp(v *ssa.Value, op uint64, saving bool) {
	size := int32(unsafe.Sizeof(abi.GoArg{}))
	defer func() {
		if !saving {
			c.a.Load(a64B, a64Ctx, abi.OffGoStatus)
			c.a.Cbnz(a64B, c.stubLabel(v.State, exitKind(v.Aux)), true)
		}
		c.a.Load(a64A, a64Ctx, abi.OffKeep+int32(v.Index)*int32(c.enc.ValueSize)+c.enc.NumOffset)
		c.setG(v, a64A)
	}()
	for i, x := range v.Args {
		c.goArg(abi.OffGoArgs+int32(i)*size, x)
	}
	if ssa.GoOp(op&0xff) == ssa.GoCall {
		// Which call, as an exit at it says.
		c.movImm(a64A, uint64(v.State.PC))
		c.a.Store(a64Ctx, abi.OffExitPC, a64A)
		c.movImm(a64A, uint64(v.State.Depth))
		c.a.Store(a64Ctx, abi.OffExitDepth, a64A)
	}
	if saving {
		stub := c.stubLabel(v.State, exitKind(v.Aux))
		c.goSavingKeep(op, v.Index, &stub)
		return
	}
	c.callGo(v, op, v.Index)
}

// callGo is a call of Go, as amd64's is.
func (c *a64Compiler) callGo(v *ssa.Value, op uint64, keep int) {
	c.movImm(a64A, op)
	c.a.Store(a64Ctx, abi.OffGoOp, a64A)
	c.movImm(a64A, uint64(keep))
	c.a.Store(a64Ctx, abi.OffGoKeep, a64A)
	for _, sv := range c.savesOf(v) {
		if sv.float {
			c.a.StoreF(a64Ctx, c.spillDisp(sv.slot), arm64.FReg(sv.reg))
		} else {
			c.a.Store(a64Ctx, c.spillDisp(sv.slot), arm64.Reg(sv.reg))
		}
	}
	back := c.a.NewLabel()
	c.a.Adr(a64A, back)
	c.a.Store(a64Ctx, abi.OffGoResume, a64A)
	c.movImm(a64A, c.enc.CallGo)
	c.a.Br(a64A)
	c.a.Bind(back)
	c.a.Load(a64Locals, a64Ctx, abi.OffLocals)
	c.a.Load(a64Stack, a64Ctx, abi.OffStack)
	for _, sv := range c.savesOf(v) {
		if sv.float {
			c.a.LoadF(arm64.FReg(sv.reg), a64Ctx, c.spillDisp(sv.slot))
		} else {
			c.a.Load(arm64.Reg(sv.reg), a64Ctx, c.spillDisp(sv.slot))
		}
	}
}

// markingExit is amd64's, the collector's flag in B, stub the call's exit.
func (c *a64Compiler) markingExit(stub arm64.Label) {
	marked := c.a.NewLabel()
	c.a.Cbnz(a64B, marked, false)
	c.cold = append(c.cold, func() {
		c.a.Bind(marked)
		c.movImm(a64B, 1)
		c.a.Store(a64Ctx, abi.OffExitMarking, a64B)
		c.a.B(stub)
	})
}

// markingCall is amd64's, stub the call's exit: while the collector
// marks, Go makes native call v (ssa.GoCall, abi.GoOpMarking), and native
// code goes on at the label returned.
func (c *a64Compiler) markingCall(v *ssa.Value, site *ssa.CallSite, stub arm64.Label) (arm64.Label, bool) {
	c.movImm(a64B, c.enc.WriteBarrier)
	c.a.LoadU8(a64B, a64B, 0)
	if c.enc.CallGo == 0 || len(v.Args) > abi.MaxGoArgs {
		c.markingExit(stub)
		return 0, false
	}
	marked, join := c.a.NewLabel(), c.a.NewLabel()
	c.a.Cbnz(a64B, marked, false)
	op := goOpWord(&ssa.CallSite{Go: ssa.GoCall, Argc: site.Argc, Method: site.Method, New: site.Pool != 0}) | abi.GoOpMarking
	c.cold = append(c.cold, func() {
		c.a.Bind(marked)
		c.goCallOp(v, op, true)
		c.a.B(join)
	})
	return join, true
}

// markingTake is amd64's, stub the call's exit: while the collector
// marks, Go takes call v's pool's object (ssa.GoTake) and stores an array
// literal's elements (goStore), and native code goes on at the label
// returned.
func (c *a64Compiler) markingTake(v *ssa.Value, site *ssa.CallSite, stub arm64.Label) (arm64.Label, bool) {
	c.movImm(a64B, c.enc.WriteBarrier)
	c.a.LoadU8(a64B, a64B, 0)
	if c.enc.CallGo == 0 {
		c.markingExit(stub)
		return 0, false
	}
	marked, join := c.a.NewLabel(), c.a.NewLabel()
	c.a.Cbnz(a64B, marked, false)
	c.cold = append(c.cold, func() {
		c.a.Bind(marked)
		c.movImm(a64A, uint64(site.Pool))
		c.a.Store(a64Ctx, abi.OffGoArgs+c.enc.RefOffset, a64A)
		c.goSavingKeep(uint64(ssa.GoTake), v.Index, &stub)
		if site.Literal && site.Argc != 0 {
			c.arrayElements(v, true)
		} else {
			c.movImm(a64A, c.enc.Object)
			c.setG(v, a64A)
		}
		c.a.B(join)
	})
	return join, true
}

// refillOnEmpty, its pool's count in B, is amd64's: stub is the call's
// exit, for a pool Go cannot fill.
func (c *a64Compiler) refillOnEmpty(pool uintptr, stub arm64.Label) {
	v, top := c.callV, c.callTop
	if v == nil {
		c.a.Cbz(a64B, stub, true)
		return
	}
	refill := c.a.NewLabel()
	c.a.Cbz(a64B, refill, true)
	c.cold = append(c.cold, func() {
		c.a.Bind(refill)
		c.movImm(a64A, uint64(pool))
		c.a.Store(a64Ctx, abi.OffGoArgs+c.enc.RefOffset, a64A)
		c.callGo(v, uint64(ssa.GoRefill), 0)
		c.a.Load(a64B, a64Ctx, abi.OffGoStatus)
		c.a.Cbnz(a64B, stub, true)
		c.a.B(top)
	})
}

// keepSource leaves in C where a tagged value came from, as amd64's does.
// It uses A.
func (c *a64Compiler) keepSource(x *ssa.Value) {
	switch o, static := c.origin.Of(x); {
	case x.Shadow != nil:
		if r := c.gpr(x.Shadow, a64C); r != a64C {
			c.a.MovRR(a64C, r)
		}
	case static && o >= 0:
		scalar, done := c.a.NewLabel(), c.a.NewLabel()
		c.isReference(x, c.gpr(x, a64A), o, scalar)
		c.slotSource(o, a64C)
		c.a.B(done)
		c.a.Bind(scalar)
		c.movImm(a64C, 0)
		c.a.Bind(done)
	default:
		c.movImm(a64C, 0)
	}
}

// keepRef is a tagged value's pointer word, as amd64's.
func (c *a64Compiler) keepRef(v *ssa.Value) {
	x := v.Args[0]
	scalar := c.a.NewLabel()
	c.keepSource(x)
	c.movImm(a64A, 0)
	c.a.Cbz(a64C, scalar, true)
	c.a.Load(a64A, a64C, c.enc.RefOffset)
	c.a.Bind(scalar)
	c.setG(v, a64A)
}

// keep copies a value into a keep cell, as amd64's.
func (c *a64Compiler) keep(v *ssa.Value) {
	x := v.Args[0]
	viaGo, done := c.a.NewLabel(), c.a.NewLabel()
	at := abi.OffKeep + int32(v.Index)*int32(c.enc.ValueSize)
	if c.enc.CallGo == 0 {
		// As amd64's: no calls of Go, the keep writes nothing then.
		c.keepSource(x)
		c.a.MovRR(a64A, a64C)
		c.movImm(a64B, c.enc.WriteBarrier)
		c.a.LoadU8(a64B, a64B, 0)
		c.a.Cbnz(a64B, done, false)
		c.a.Store(a64Ctx, at+c.enc.RefOffset, c.gpr(v.Args[1], a64B))
		c.a.Store(a64Ctx, at+c.enc.NumOffset, c.gpr(x, a64B))
		c.a.AddImm(a64A, a64Ctx, int64(at), true)
		c.a.Bind(done)
		c.setG(v, a64A)
		return
	}
	c.a.FMovToF(a64F0, c.gpr(x, a64B))
	if r := c.gpr(v.Args[1], a64C); r != a64C {
		c.a.MovRR(a64C, r)
	}
	c.movImm(a64B, c.enc.WriteBarrier)
	c.a.LoadU8(a64B, a64B, 0)
	c.a.Cbnz(a64B, viaGo, false)
	c.a.Store(a64Ctx, at+c.enc.RefOffset, a64C)
	c.a.FMovFromF(a64B, a64F0)
	c.a.Store(a64Ctx, at+c.enc.NumOffset, a64B)
	c.a.Bind(done)
	c.a.AddImm(a64A, a64Ctx, int64(at), true)
	c.setG(v, a64A)
	c.cold = append(c.cold, func() {
		c.a.Bind(viaGo)
		c.a.AddImm(a64A, a64Ctx, int64(at), true)
		c.goStore(nil)
		c.a.B(done)
	})
}

// holder finds the property a read whose receiver's shape it knows names,
// for whichever of the shapes it met the receiver's is, into A, as amd64's
// does.
func (c *a64Compiler) holder(v *ssa.Value, guard func(arm64.Cond)) {
	var first ssa.HolderChain
	if v.Holders() != nil {
		first = *v.Holders()
	}
	cases := append([]ssa.PropertyCase{{Shape: uintptr(v.Const.Bits), Index: int32(v.Index), Holders: first.Holders, More: first.More}}, v.Cases()...)
	// As amd64's: a receiver of a shape not among the cases has its own
	// table searched.
	done := c.a.NewLabel()
	for i, k := range cases {
		next := c.a.NewLabel()
		p := c.gpr(v.Args[0], a64A)
		c.a.Load(a64B, p, c.enc.ObjectShape)
		c.movImm(a64C, uint64(k.Shape))
		c.a.Cmp(a64B, a64C, true)
		if i == len(cases)-1 {
			scan := c.a.NewLabel()
			c.a.BCond(arm64.NE, scan)
			c.cold = append(c.cold, func() {
				c.a.Bind(scan)
				c.scan(v, guard, done)
			})
		} else {
			c.a.BCond(arm64.NE, next)
		}
		if p != a64A {
			c.a.MovRR(a64A, p)
		}
		c.a.Load(a64B, a64A, c.enc.ObjectProto)
		for j := 0; j < len(k.Holders)+len(k.More); j++ {
			var h ssa.Holder
			if j < len(k.Holders) {
				h = k.Holders[j]
			} else {
				h = k.More[j-len(k.Holders)]
			}
			if h.Object == 0 {
				break
			}
			c.movImm(a64A, uint64(h.Object))
			c.a.Cmp(a64B, a64A, true)
			guard(arm64.NE)
			c.a.Load(a64B, a64A, c.enc.ObjectShape)
			c.movImm(a64C, uint64(h.Shape))
			c.a.Cmp(a64B, a64C, true)
			guard(arm64.NE)
			c.a.Load(a64B, a64A, c.enc.ObjectProto)
		}
		c.a.Load(a64A, a64A, c.enc.ObjectProps)
		c.a.AddImm(a64A, a64A, int64(k.Index*c.enc.PropertySize+c.enc.PropertyValue), true)
		if i < len(cases)-1 {
			c.a.B(done)
			c.a.Bind(next)
		}
	}
	c.a.Bind(done)
}

// integer converts the double x to an integer in r, failing unless it is
// one an int64 holds exactly: FCVTZS saturates, so a result of 2**63-1,
// which no double converts to exactly, means x is 2**63 or more. It uses
// D and F2.
func (c *a64Compiler) integer(r arm64.Reg, x arm64.FReg, guard func(arm64.Cond)) {
	c.a.Fcvtzs(r, x)
	c.a.Scvtf(a64F2, r, true)
	c.a.FCmp(a64F2, x)
	guard(arm64.NE)
	c.movImm(a64D, math.MaxInt64)
	c.a.Cmp(r, a64D, true)
	guard(arm64.EQ)
}

// remainder is JavaScript's % of two integers, as amd64's is. SDIV never
// faults -- -2**63 / -1 is -2**63, whose MSUB remainder is 0 -- so no
// divisor needs a case of its own.
func (c *a64Compiler) remainder(v *ssa.Value, guard func(arm64.Cond)) {
	x, y := c.fpr(v.Args[0], a64F0), c.fpr(v.Args[1], a64F1)
	zero, done := c.a.NewLabel(), c.a.NewLabel()
	divisor := c.stubLabel(v.State, exitKind(v.Aux))
	if c.enc.CallGo != 0 {
		// What is not integers Go computes (ssa.GoMod), the operands' bits
		// in the context's GoArgs, from registers the checks below leave as
		// they are.
		size := int32(unsafe.Sizeof(abi.GoArg{}))
		slow, stub := c.a.NewLabel(), divisor
		guard, divisor = func(cond arm64.Cond) { c.a.BCond(cond, slow) }, slow
		c.cold = append(c.cold, func() {
			c.a.Bind(slow)
			c.a.FMovFromF(a64A, x)
			c.a.Store(a64Ctx, abi.OffGoArgs+c.enc.NumOffset, a64A)
			c.a.FMovFromF(a64A, y)
			c.a.Store(a64Ctx, abi.OffGoArgs+size+c.enc.NumOffset, a64A)
			c.goSaving(uint64(ssa.GoMod), &stub)
			c.a.LoadF(a64F0, a64Ctx, abi.OffGoArgs+c.enc.NumOffset)
			c.a.B(done)
		})
	}
	c.integer(a64A, x, guard)
	c.integer(a64C, y, guard)
	c.a.Cbz(a64C, divisor, true)
	c.a.Sdiv(a64B, a64A, a64C)
	c.a.Msub(a64B, a64B, a64C, a64A)
	c.a.Cbz(a64B, zero, true)
	c.a.Scvtf(a64F0, a64B, true)
	c.a.B(done)
	c.a.Bind(zero)
	c.a.FMovFromF(a64A, x)
	c.movImm(a64B, 1<<63)
	c.a.Op(arm64.And, a64A, a64A, a64B, true)
	c.a.FMovToF(a64F0, a64A)
	c.a.Bind(done)
	c.setF(v, a64F0)
}

// round is Math.round(x), as amd64's is.
func (c *a64Compiler) round(v *ssa.Value) {
	if x := c.fpr(v.Args[0], a64F1); x != a64F1 {
		c.a.FMov(a64F1, x)
	}
	down, done := c.a.NewLabel(), c.a.NewLabel()
	c.a.FRint(arm64.FRintP, a64F0, a64F1)
	c.constF64(a64F2, math.Float64bits(0.5))
	c.a.FArith(arm64.FSub, a64F0, a64F0, a64F2)
	c.a.FCmp(a64F0, a64F1)
	c.a.BCond(arm64.GT, down)
	c.a.FRint(arm64.FRintP, a64F0, a64F1)
	c.a.B(done)
	c.a.Bind(down)
	c.a.FArith(arm64.FSub, a64F0, a64F0, a64F2)
	c.a.Bind(done)
	c.setF(v, a64F0)
}

// sign is Math.sign(x), as amd64's is.
func (c *a64Compiler) sign(v *ssa.Value) {
	if x := c.fpr(v.Args[0], a64F0); x != a64F0 {
		c.a.FMov(a64F0, x)
	}
	above, done := c.a.NewLabel(), c.a.NewLabel()
	c.constF64(a64F1, 0)
	c.a.FCmp(a64F0, a64F1)
	c.a.BCond(arm64.VS, done)
	c.a.BCond(arm64.EQ, done)
	c.a.BCond(arm64.GT, above)
	c.constF64(a64F0, math.Float64bits(-1))
	c.a.B(done)
	c.a.Bind(above)
	c.constF64(a64F0, math.Float64bits(1))
	c.a.Bind(done)
	c.setF(v, a64F0)
}

// mathCall has Go compute a Math function of numbers, as amd64's does.
func (c *a64Compiler) mathCall(v *ssa.Value) {
	size := int32(unsafe.Sizeof(abi.GoArg{}))
	for i, x := range v.Args {
		c.a.FMovFromF(a64A, c.fpr(x, a64F0))
		c.a.Store(a64Ctx, abi.OffGoArgs+int32(i)*size+c.enc.NumOffset, a64A)
	}
	if ssa.GoOp(v.Index) == ssa.GoMath {
		c.movImm(a64A, uint64(v.Aux))
		c.a.Store(a64Ctx, abi.OffGoArgs+size+c.enc.NumOffset, a64A)
	}
	c.goSaving(uint64(v.Index), nil)
	c.a.LoadF(a64F0, a64Ctx, abi.OffGoArgs+c.enc.NumOffset)
	c.setF(v, a64F0)
}

// power is Math.pow(x, y) where its answer is exact, as amd64's is.
func (c *a64Compiler) power(v *ssa.Value, guard func(arm64.Cond)) {
	if x := c.fpr(v.Args[0], a64F0); x != a64F0 {
		c.a.FMov(a64F0, x)
	}
	if y := c.fpr(v.Args[1], a64F1); y != a64F1 {
		c.a.FMov(a64F1, y)
	}
	general, loop, skip, last, done := c.a.NewLabel(), c.a.NewLabel(), c.a.NewLabel(), c.a.NewLabel(), c.a.NewLabel()
	c.constF64(a64F2, math.Float64bits(2))
	c.a.FCmp(a64F1, a64F2)
	c.a.BCond(arm64.NE, general)
	c.a.FArith(arm64.FMul, a64F0, a64F0, a64F0)
	c.a.B(done)
	c.a.Bind(general)
	if c.enc.CallGo != 0 {
		// What is not exact Go computes (ssa.GoPow).
		size := int32(unsafe.Sizeof(abi.GoArg{}))
		c.a.FMovFromF(a64A, a64F0)
		c.a.Store(a64Ctx, abi.OffGoArgs+c.enc.NumOffset, a64A)
		c.a.FMovFromF(a64A, a64F1)
		c.a.Store(a64Ctx, abi.OffGoArgs+size+c.enc.NumOffset, a64A)
		slow, stub := c.a.NewLabel(), c.stubLabel(v.State, exitKind(v.Aux))
		guard = func(cond arm64.Cond) { c.a.BCond(cond, slow) }
		c.cold = append(c.cold, func() {
			c.a.Bind(slow)
			c.goSaving(uint64(ssa.GoPow), &stub)
			c.a.LoadF(a64F0, a64Ctx, abi.OffGoArgs+c.enc.NumOffset)
			c.a.B(done)
		})
	}
	// y an integer from 0 to 64 (unsigned, so none below), x an integer.
	c.integer(a64C, a64F1, guard)
	c.a.CmpImm(a64C, 64, true)
	guard(arm64.HI)
	c.integer(a64A, a64F0, guard)
	// The result in F2, x's powers in F0.
	c.constF64(a64F2, math.Float64bits(1))
	c.a.Cbz(a64C, last, true)
	c.movImm(a64D, 1)
	c.a.Bind(loop)
	c.a.Tst(a64C, a64D, true)
	c.a.BCond(arm64.EQ, skip)
	c.a.FArith(arm64.FMul, a64F2, a64F2, a64F0)
	c.a.Bind(skip)
	c.a.ShiftImm(arm64.Lsr, a64C, a64C, 1, true)
	c.a.Cbz(a64C, last, true)
	c.a.FArith(arm64.FMul, a64F0, a64F0, a64F0)
	c.a.B(loop)
	c.a.Bind(last)
	// At most 2**53, its bits compared as an integer's, the sign cleared.
	c.a.FAbs(a64F1, a64F2)
	c.a.FMovFromF(a64A, a64F1)
	c.movImm(a64B, math.Float64bits(1<<53))
	c.a.Cmp(a64A, a64B, true)
	guard(arm64.HI)
	c.a.FMov(a64F0, a64F2)
	c.a.Bind(done)
	c.setF(v, a64F0)
}

// length is x.length, as amd64's is.
func (c *a64Compiler) length(v *ssa.Value, guard func(arm64.Cond)) {
	a := v.Args[0]
	if a.Shadow == nil && c.origin.At(a) < 0 {
		c.a.B(c.stubLabel(v.State, exitKind(v.Aux)))
		return
	}
	array, have := c.a.NewLabel(), c.a.NewLabel()
	c.movImm(a64B, c.enc.String)
	c.a.Cmp(c.gpr(a, a64A), a64B, true)
	c.a.BCond(arm64.NE, array)
	c.reference(v, a, c.enc.String, guard)
	c.a.Load(a64C, a64C, c.enc.StringLength)
	c.a.B(have)
	c.a.Bind(array)
	c.reference(v, a, c.enc.Object, guard)
	c.a.LoadU8(a64B, a64C, c.enc.ObjectClass)
	c.a.CmpImm(a64B, int64(c.enc.ClassArray), false)
	guard(arm64.NE)
	p := a64A
	c.a.MovRR(p, a64C)
	c.a.Load(a64C, p, c.enc.ObjectElems+8)
	c.a.LoadU8(a64B, p, c.enc.ObjectFlags)
	c.movImm(a64D, uint64(c.enc.FlagSparse))
	c.a.Tst(a64B, a64D, false)
	c.a.BCond(arm64.EQ, have)
	c.a.LoadU32(a64B, p, c.enc.ObjectArrayLen)
	c.a.Cmp(a64B, a64C, true)
	c.a.BCond(arm64.LS, have)
	c.a.MovRR(a64C, a64B)
	c.a.Bind(have)
	c.a.Scvtf(a64F0, a64C, true)
	c.setF(v, a64F0)
}

// stringCode is charCodeAt called on a string, as amd64's is.
func (c *a64Compiler) stringCode(v *ssa.Value, guard func(arm64.Cond)) {
	if !c.reference(v, v.Args[0], c.enc.Object, guard) {
		return
	}
	c.a.Load(a64B, a64Ctx, abi.OffCharCode+c.enc.RefOffset)
	c.a.Cmp(a64C, a64B, true)
	guard(arm64.NE)
	if !c.reference(v, v.Args[1], c.enc.String, guard) {
		return
	}
	fail := c.stubLabel(v.State, exitKind(v.Aux))
	c.a.Load(a64B, a64C, c.enc.StringLeft)
	c.a.Cbnz(a64B, fail, true)
	c.index(v.Args[2], guard)
	c.a.Load(a64B, a64C, c.enc.StringLength)
	c.a.Cmp(a64A, a64B, true)
	guard(arm64.HS)
	units, done := c.a.NewLabel(), c.a.NewLabel()
	c.a.LoadU8(a64B, a64C, c.enc.StringASCII)
	c.a.Cbz(a64B, units, false)
	c.a.Load(a64B, a64C, c.enc.StringData)
	c.a.Op(arm64.Add, a64B, a64B, a64A, true)
	c.a.LoadU8(a64A, a64B, 0)
	c.a.B(done)
	c.a.Bind(units)
	c.a.Load(a64B, a64C, c.enc.StringU16)
	c.a.Cbz(a64B, fail, true)
	c.a.Op(arm64.Add, a64A, a64A, a64A, true)
	c.a.Op(arm64.Add, a64B, a64B, a64A, true)
	c.a.LoadU16(a64A, a64B, 0)
	c.a.Bind(done)
	c.a.Scvtf(a64F0, a64A, true)
	c.setF(v, a64F0)
}

// index converts an element's key to an index in A, failing unless it is an
// integer in [0, 2**32). FCVTZS saturates, so a key of 2**63 or more gives
// 2**63-1, whose top half is not clear. It uses B and F1.
func (c *a64Compiler) index(key *ssa.Value, guard func(arm64.Cond)) {
	k := c.fpr(key, a64F0)
	c.a.Fcvtzs(a64A, k)
	c.a.Scvtf(a64F1, a64A, true)
	c.a.FCmp(a64F1, k)
	guard(arm64.NE)
	c.a.ShiftImm(arm64.Lsr, a64B, a64A, 32, true)
	c.a.CmpImm(a64B, 0, true)
	guard(arm64.NE)
}

// element turns the index in A into the address of an array's element in
// A, and its number word in B, as amd64's does. It uses C.
func (c *a64Compiler) element(array *ssa.Value, guard func(arm64.Cond)) {
	p := c.gpr(array, a64C)
	c.a.Load(a64B, p, c.enc.ObjectElems+8)
	c.a.Cmp(a64A, a64B, true)
	guard(arm64.HS)
	c.a.Load(a64B, p, c.enc.ObjectElems)
	c.a.ShiftImm(arm64.Lsl, a64A, a64A, 4, true)
	c.a.Op(arm64.Add, a64A, a64A, a64B, true)
	c.a.Load(a64B, a64A, c.enc.NumOffset)
	c.movImm(a64C, abi.NumberLimit)
	c.a.Cmp(a64B, a64C, true)
	guard(arm64.HS)
}

// elementCell turns the index in A into the address of an array's element
// there, in A, failing unless it is within the dense elements and not a
// hole, as amd64's does. It uses B and C.
func (c *a64Compiler) elementCell(array *ssa.Value, guard func(arm64.Cond)) {
	p := c.gpr(array, a64C)
	c.a.Load(a64B, p, c.enc.ObjectElems+8)
	c.a.Cmp(a64A, a64B, true)
	guard(arm64.HS)
	c.a.Load(a64B, p, c.enc.ObjectElems)
	c.a.ShiftImm(arm64.Lsl, a64A, a64A, 4, true)
	c.a.Op(arm64.Add, a64A, a64A, a64B, true)
	c.a.Load(a64B, a64A, c.enc.NumOffset)
	c.movImm(a64C, c.enc.Uninitialized)
	c.a.Cmp(a64B, a64C, true)
	guard(arm64.EQ)
}

// numberCell checks the word at A's cell is a number, into B.
func (c *a64Compiler) numberCell(guard func(arm64.Cond)) {
	c.a.Load(a64B, a64A, c.enc.NumOffset)
	c.movImm(a64C, abi.NumberLimit)
	c.a.Cmp(a64B, a64C, true)
	guard(arm64.HS)
}

// guardFor is a guard of v's for code that may keep it: a jump to the stub
// that exits to v's state.
func (c *a64Compiler) guardFor(v *ssa.Value) func(arm64.Cond) {
	return func(cond arm64.Cond) {
		c.a.BCond(cond, c.stubLabel(v.State, exitKind(v.Aux)))
	}
}

// value emits one value.
func (c *a64Compiler) value(v *ssa.Value, b *ssa.Block) {
	arg := func(i int) *ssa.Value { return v.Args[i] }
	// guard is for the guards emitted here; one passed on, which may be
	// kept for cold code, is made only for the values that pass one
	// (guardFor), not for every value.
	guard := func(cond arm64.Cond) {
		c.a.BCond(cond, c.stubLabel(v.State, exitKind(v.Aux)))
	}
	switch v.Op {
	case ssa.OpLoadSlot:
		d := c.gdst(v)
		base, disp := c.slotAddr(v.Aux, false, d)
		c.a.Load(d, base, disp)
		c.setG(v, d)
	case ssa.OpConst:
		d := c.gdst(v)
		c.movImm(d, c.constWord(v.Const))
		c.setG(v, d)
	case ssa.OpConstCell:
		d := c.gdst(v)
		c.movImm(d, v.Const.Bits)
		c.setG(v, d)
	case ssa.OpLocalCell:
		// As amd64's: the captured local's slot in the frame.
		d := c.gdst(v)
		c.a.AddImm(d, a64Locals, int64(v.Index)*int64(c.enc.ValueSize), true)
		c.setG(v, d)
	case ssa.OpUpvalueCell:
		// As amd64's: the binding's cell, then where its value is now.
		d := c.gdst(v)
		c.a.Load(d, a64Ctx, abi.OffUpvalues)
		c.a.Load(d, d, int32(v.Index)*8)
		c.a.Load(d, d, c.enc.UpvalueSlot)
		c.setG(v, d)
	case ssa.OpConstF64:
		d := c.fdst(v)
		c.constF64(d, v.Const.Bits)
		c.setF(v, d)
	case ssa.OpUnboxF64:
		r := c.gpr(arg(0), a64B)
		c.a.ShiftImm(arm64.Lsr, a64A, r, 51, true)
		c.a.CmpImm(a64A, 0x1FFF, false)
		guard(arm64.EQ)
		d := c.fdst(v)
		c.a.FMovToF(d, r)
		c.setF(v, d)
	case ssa.OpCheckInit:
		r := c.gpr(arg(0), a64B)
		c.movImm(a64A, c.enc.Uninitialized)
		c.a.Cmp(r, a64A, true)
		guard(arm64.EQ)
	case ssa.OpCheckTrue:
		r := c.gpr(arg(0), a64B)
		c.movImm(a64A, c.enc.True)
		c.a.Cmp(r, a64A, true)
		guard(arm64.NE)
	case ssa.OpTruth:
		c.truth(v, c.guardFor(v))
	case ssa.OpArrayOf:
		c.arrayOf(v, c.guardFor(v))
	case ssa.OpObjectOf:
		if c.objectOf(v, c.guardFor(v)) {
			c.setG(v, a64C)
		}
	case ssa.OpInstanceOf:
		c.instanceOf(v, c.guardFor(v))
	case ssa.OpTypeIs:
		c.typeIs(v, c.guardFor(v))
	case ssa.OpCall:
		c.call(v, c.guardFor(v))
	case ssa.OpCallCell:
		c.a.AddImm(a64A, a64Ctx, int64(abi.OffKeep+int32(v.Args[0].Index)*int32(c.enc.ValueSize)), true)
		c.setG(v, a64A)
	case ssa.OpKeepRef:
		c.keepRef(v)
	case ssa.OpKeep:
		c.keep(v)
	case ssa.OpKept:
		c.setG(v, c.gpr(arg(1), a64A))
	case ssa.OpFrameRoom:
		c.a.Load(a64B, a64Ctx, abi.OffStackBase)
		c.a.Op(arm64.Sub, a64A, a64Stack, a64B, true)
		c.a.ShiftImm(arm64.Lsr, a64A, a64A, 4, true)
		c.a.AddImm(a64A, a64A, int64(v.Index), true)
		c.a.Load(a64B, a64Ctx, abi.OffStackEnd)
		c.a.Cmp(a64A, a64B, true)
		guard(arm64.HI)
		c.a.Load(a64A, a64Ctx, abi.OffLevel)
		c.a.AddImm(a64A, a64A, int64(max(v.Const.Bits, 1)), true)
		c.a.Load(a64B, a64Ctx, abi.OffLevelLimit)
		c.a.Cmp(a64A, a64B, true)
		guard(arm64.HS)
	case ssa.OpTailSelf:
		c.tailSelf(v, guard)
	case ssa.OpIterClosed:
		// As amd64's.
		x, ok := v.Args[0], c.a.NewLabel()
		if c.enc.IterState == 0 {
			c.a.B(c.stubLabel(v.State, exitKind(v.Aux)))
			break
		}
		c.pointerWord(x, c.gpr(x, a64A))
		c.a.CmpImm(a64B, 0, true)
		guard(arm64.EQ)
		c.a.Load(a64C, a64B, c.enc.ObjectData)
		c.movImm(a64A, c.enc.IterState)
		c.a.Cmp(a64C, a64A, true)
		guard(arm64.NE)
		c.a.Load(a64B, a64B, c.enc.ObjectData+8)
		c.a.LoadU8(a64C, a64B, c.enc.IterDone)
		c.a.Cbnz(a64C, ok, false)
		c.a.Load(a64C, a64B, c.enc.IterArr)
		c.a.CmpImm(a64C, 0, true)
		guard(arm64.EQ)
		c.a.Bind(ok)
	case ssa.OpSameSymbol:
		if c.reference(v, v.Args[0], c.enc.Symbol, guard) {
			c.movImm(a64B, v.Const.Bits)
			c.a.Cmp(a64C, a64B, true)
			guard(arm64.NE)
		}
	case ssa.OpTailRoom:
		c.a.Load(a64A, a64Ctx, abi.OffLevel)
		c.a.Load(a64B, a64Ctx, abi.OffTailLevel)
		c.a.Cmp(a64A, a64B, true)
		guard(arm64.HS)
	case ssa.OpSameObject:
		c.movImm(a64B, v.Const.Bits)
		c.a.Cmp(c.gpr(arg(0), a64A), a64B, true)
		guard(arm64.NE)
	case ssa.OpPropRead:
		c.property(v, c.guardFor(v))
		c.numberCell(guard)
		c.a.FMovToF(a64F0, a64B)
		c.setF(v, a64F0)
	case ssa.OpPropCell:
		c.property(v, c.guardFor(v))
		c.setG(v, a64A)
	case ssa.OpGlobalCell, ssa.OpBindingCell:
		// As amd64's: unshadowed, then the binding in the scope's object
		// or the one at Const's address.
		if v.Op == ssa.OpGlobalCell || v.Global {
			unshadowed := c.a.NewLabel()
			word := int64(v.Key >> 6)
			c.a.Load(a64B, a64Ctx, abi.OffLexNames)
			c.a.Load(a64A, a64B, 8)
			c.a.CmpImm(a64A, word, true)
			c.a.BCond(arm64.LS, unshadowed)
			c.a.Load(a64B, a64B, 0)
			c.a.Load(a64B, a64B, int32(word*8))
			c.movImm(a64A, 1<<(v.Key&63))
			c.a.Tst(a64B, a64A, true)
			guard(arm64.NE)
			c.a.Bind(unshadowed)
		}
		if v.Op == ssa.OpBindingCell {
			c.movImm(a64A, v.Const.Bits)
		} else {
			c.a.Load(a64A, a64Ctx, abi.OffGlobal)
			c.a.Cbz(a64A, c.stubLabel(v.State, exitKind(v.Aux)), true)
		}
		c.a.Load(a64B, a64A, c.enc.ObjectProps+8)
		c.a.CmpImm(a64B, int64(v.Index), true)
		guard(arm64.LS)
		c.a.Load(a64A, a64A, c.enc.ObjectProps)
		c.a.AddImm(a64A, a64A, int64(int32(v.Index)*c.enc.PropertySize), true)
		c.a.LoadU32(a64B, a64A, c.enc.PropertyKey)
		c.a.CmpImm(a64B, int64(v.Key), false)
		guard(arm64.NE)
		c.a.LoadU8(a64B, a64A, c.enc.PropertyFlags)
		c.movImm(a64C, uint64(c.enc.PropNotData|c.enc.PropUninit))
		c.a.Tst(a64B, a64C, false)
		guard(arm64.NE)
		c.a.AddImm(a64A, a64A, int64(c.enc.PropertyValue), true)
		c.setG(v, a64A)
	case ssa.OpLexCell:
		// As amd64's: the bindings' object, the binding's key, and out of
		// its temporal dead zone.
		c.a.Load(a64A, a64Ctx, abi.OffGlobalLex)
		c.a.Cbz(a64A, c.stubLabel(v.State, exitKind(v.Aux)), true)
		c.a.Load(a64B, a64A, c.enc.ObjectProps+8)
		c.a.CmpImm(a64B, int64(v.Index), true)
		guard(arm64.LS)
		c.a.Load(a64A, a64A, c.enc.ObjectProps)
		c.a.AddImm(a64A, a64A, int64(int32(v.Index)*c.enc.PropertySize), true)
		c.a.LoadU32(a64B, a64A, c.enc.PropertyKey)
		c.a.CmpImm(a64B, int64(v.Key), false)
		guard(arm64.NE)
		c.a.AddImm(a64A, a64A, int64(c.enc.PropertyValue), true)
		c.a.Load(a64B, a64A, c.enc.NumOffset)
		c.movImm(a64C, c.enc.Uninitialized)
		c.a.Cmp(a64B, a64C, true)
		guard(arm64.EQ)
		c.setG(v, a64A)
	case ssa.OpStringMethod:
		// As amd64's: where String.prototype has it now, its shape checked.
		c.movImm(a64B, c.enc.String)
		c.a.Cmp(c.gpr(arg(0), a64A), a64B, true)
		guard(arm64.NE)
		c.a.Load(a64A, a64Ctx, abi.OffStringProto)
		c.a.Cbz(a64A, c.stubLabel(v.State, exitKind(v.Aux)), true)
		c.a.Load(a64B, a64A, c.enc.ObjectShape)
		c.a.Load(a64C, a64Ctx, abi.OffStringShape)
		c.a.Cmp(a64B, a64C, true)
		guard(arm64.NE)
		c.a.Load(a64A, a64A, c.enc.ObjectProps)
		c.a.Load(a64B, a64Ctx, abi.OffCharCodeAtOffset)
		c.a.Op(arm64.Add, a64A, a64A, a64B, true)
		c.setG(v, a64A)
	case ssa.OpStringProp:
		// As amd64's: a string's method where String.prototype has it, its
		// shape the one the code was compiled for.
		c.movImm(a64B, c.enc.String)
		c.a.Cmp(c.gpr(arg(0), a64A), a64B, true)
		guard(arm64.NE)
		c.a.Load(a64A, a64Ctx, abi.OffStringMethods)
		c.a.Cbz(a64A, c.stubLabel(v.State, exitKind(v.Aux)), true)
		c.a.Load(a64B, a64A, c.enc.ObjectShape)
		c.movImm(a64C, v.Const.Bits)
		c.a.Cmp(a64B, a64C, true)
		guard(arm64.NE)
		c.a.Load(a64A, a64A, c.enc.ObjectProps)
		c.a.AddImm(a64A, a64A, int64(int32(v.Index)*c.enc.PropertySize), true)
		c.a.LoadU32(a64B, a64A, c.enc.PropertyKey)
		c.a.CmpImm(a64B, int64(v.Key), false)
		guard(arm64.NE)
		c.a.AddImm(a64A, a64A, int64(c.enc.PropertyValue), true)
		c.setG(v, a64A)
	case ssa.OpStringCode:
		c.stringCode(v, c.guardFor(v))
	case ssa.OpLoadCell:
		d := c.gdst(v)
		c.a.Load(d, c.gpr(arg(0), a64A), c.enc.NumOffset)
		c.setG(v, d)
	case ssa.OpPropWrite:
		c.propStore(v, c.guardFor(v))
	case ssa.OpLength:
		c.length(v, c.guardFor(v))
	case ssa.OpElemKey:
		c.index(arg(0), c.guardFor(v))
	case ssa.OpElemRead:
		c.index(arg(1), c.guardFor(v))
		c.element(arg(0), c.guardFor(v))
		c.a.FMovToF(a64F0, a64B)
		c.setF(v, a64F0)
	case ssa.OpElemCell:
		fail, done := c.guardFor(v), c.a.NewLabel()
		if c.enc.CallGo != 0 && c.enc.UndefinedCell != 0 {
			// An element it does not find Go says is undefined, where it
			// can (ssa.GoElemCell).
			slow, stub := c.a.NewLabel(), c.stubLabel(v.State, exitKind(v.Aux))
			fail = func(cond arm64.Cond) { c.a.BCond(cond, slow) }
			c.cold = append(c.cold, func() {
				c.a.Bind(slow)
				size := int32(unsafe.Sizeof(abi.GoArg{}))
				c.a.Store(a64Ctx, abi.OffGoArgs+c.enc.RefOffset, c.gpr(v.Args[0], a64C))
				c.a.FMovFromF(a64A, c.fpr(v.Args[1], a64F0))
				c.a.Store(a64Ctx, abi.OffGoArgs+size+c.enc.NumOffset, a64A)
				c.goSaving(uint64(ssa.GoElemCell), &stub)
				c.movImm(a64A, c.enc.UndefinedCell)
				c.a.B(done)
			})
		}
		c.index(arg(1), fail)
		c.elementCell(arg(0), fail)
		c.a.Bind(done)
		c.setG(v, a64A)
	case ssa.OpElemWrite:
		fail, done := c.guardFor(v), c.a.NewLabel()
		if c.enc.CallGo != 0 {
			// Any other store Go makes if it can (ssa.GoElemWrite).
			slow, stub := c.a.NewLabel(), c.stubLabel(v.State, exitKind(v.Aux))
			fail = func(cond arm64.Cond) { c.a.BCond(cond, slow) }
			c.cold = append(c.cold, func() {
				c.a.Bind(slow)
				size := int32(unsafe.Sizeof(abi.GoArg{}))
				c.a.Store(a64Ctx, abi.OffGoArgs+c.enc.RefOffset, c.gpr(v.Args[0], a64C))
				c.a.FMovFromF(a64A, c.fpr(v.Args[1], a64F0))
				c.a.Store(a64Ctx, abi.OffGoArgs+size+c.enc.NumOffset, a64A)
				c.a.FMovFromF(a64A, c.fpr(v.Args[2], a64F0))
				c.a.Store(a64Ctx, abi.OffGoArgs+2*size+c.enc.NumOffset, a64A)
				c.goSaving(uint64(ssa.GoElemWrite), &stub)
				c.a.B(done)
			})
		}
		c.index(arg(1), fail)
		c.element(arg(0), fail)
		c.boxF64(c.fpr(arg(2), a64F0), a64B)
		c.a.Store(a64A, c.enc.NumOffset, a64B)
		c.a.Bind(done)
	case ssa.OpBoxF64:
		c.boxF64(c.fpr(arg(0), a64F0), a64A)
		c.setG(v, a64A)
	case ssa.OpBoxBool:
		c.boxBool(c.gpr(arg(0), a64B), a64A)
		c.setG(v, a64A)
	case ssa.OpAddF64, ssa.OpSubF64, ssa.OpMulF64, ssa.OpDivF64:
		d := c.fdst(v)
		c.a.FArith(a64FOp(v.Op), d, c.fpr(arg(0), a64F0), c.fpr(arg(1), a64F1))
		c.setF(v, d)
	case ssa.OpModF64:
		c.remainder(v, c.guardFor(v))
	case ssa.OpPowF64:
		c.power(v, c.guardFor(v))
	case ssa.OpNegF64:
		d := c.fdst(v)
		c.a.FNeg(d, c.fpr(arg(0), a64F0))
		c.setF(v, d)
	case ssa.OpSqrtF64:
		d := c.fdst(v)
		c.a.FSqrt(d, c.fpr(arg(0), a64F0))
		c.setF(v, d)
	case ssa.OpAbsF64:
		d := c.fdst(v)
		c.a.FAbs(d, c.fpr(arg(0), a64F0))
		c.setF(v, d)
	case ssa.OpFloorF64, ssa.OpCeilF64, ssa.OpTruncF64:
		op := arm64.FRintM
		if v.Op == ssa.OpCeilF64 {
			op = arm64.FRintP
		} else if v.Op == ssa.OpTruncF64 {
			op = arm64.FRintZ
		}
		d := c.fdst(v)
		c.a.FRint(op, d, c.fpr(arg(0), a64F0))
		c.setF(v, d)
	case ssa.OpRoundF64:
		c.round(v)
	case ssa.OpSignF64:
		c.sign(v)
	case ssa.OpFroundF64:
		c.a.FCvtToSingle(a64F0, c.fpr(arg(0), a64F0))
		d := c.fdst(v)
		c.a.FCvtFromSingle(d, a64F0)
		c.setF(v, d)
	case ssa.OpMathCall:
		c.mathCall(v)
	case ssa.OpMaxF64, ssa.OpMinF64:
		// FMAX and FMIN are Math.max's and Math.min's, NaN and zeros alike.
		op := arm64.FMax
		if v.Op == ssa.OpMinF64 {
			op = arm64.FMin
		}
		d := c.fdst(v)
		c.a.FArith(op, d, c.fpr(arg(0), a64F0), c.fpr(arg(1), a64F1))
		c.setF(v, d)
	case ssa.OpCmpF64:
		if v.Uses == 1 && b.Control == v && b.Kind == ssa.BlockIf {
			return // fused into the branch
		}
		c.a.FCmp(c.fpr(arg(0), a64F0), c.fpr(arg(1), a64F1))
		d := c.gdst(v)
		c.a.Cset(d, a64Compare(ir.Operator(v.Aux)))
		c.setG(v, d)
	case ssa.OpStrictNullish:
		word := c.enc.Null
		if v.Aux == 1 {
			word = c.enc.Undefined
		}
		w := c.gpr(arg(0), a64B)
		c.movImm(a64A, word)
		c.a.Cmp(w, a64A, true)
		d := c.gdst(v)
		c.a.Cset(d, arm64.EQ)
		c.setG(v, d)
	case ssa.OpLooseNullish:
		c.looseNullish(v, c.guardFor(v))
	case ssa.OpEqTagged:
		c.eqTagged(v, c.guardFor(v))
	case ssa.OpNot:
		c.a.CmpImm(c.gpr(arg(0), a64A), 0, false)
		d := c.gdst(v)
		c.a.Cset(d, arm64.EQ)
		c.setG(v, d)
	case ssa.OpToInt32:
		c.toInt32(v)
	case ssa.OpAndI32, ssa.OpOrI32, ssa.OpXorI32:
		d := c.gdst(v)
		c.a.Op(a64ALU(v.Op), d, c.gpr(arg(0), a64A), c.gpr(arg(1), a64B), false)
		c.setG(v, d)
	case ssa.OpShlI32, ssa.OpSarI32, ssa.OpShrU32:
		// The W forms take the count modulo 32, as JavaScript does.
		d := c.gdst(v)
		c.a.ShiftReg(a64Shift(v.Op), d, c.gpr(arg(0), a64A), c.gpr(arg(1), a64C), false)
		c.setG(v, d)
	case ssa.OpNotI32:
		c.a.Mvn(a64A, c.gpr(arg(0), a64A), false)
		c.setG(v, a64A)
	case ssa.OpI32ToF64:
		c.a.Scvtf(a64F0, c.gpr(arg(0), a64A), false)
		c.setF(v, a64F0)
	case ssa.OpU32ToF64:
		c.a.Ucvtf(a64F0, c.gpr(arg(0), a64A), false)
		c.setF(v, a64F0)
	default:
		panic("value " + v.Op.String())
	}
}

// truth computes JavaScript truthiness for the kinds the slot IR decides it
// for, exiting for the rest.
func (c *a64Compiler) truth(v *ssa.Value, guard func(arm64.Cond)) {
	r := c.gpr(v.Args[0], a64B)
	number, yes, no, done := c.a.NewLabel(), c.a.NewLabel(), c.a.NewLabel(), c.a.NewLabel()
	c.a.ShiftImm(arm64.Lsr, a64A, r, 51, true)
	c.a.CmpImm(a64A, 0x1FFF, false)
	c.a.BCond(arm64.NE, number)
	for _, w := range []struct {
		word uint64
		to   arm64.Label
	}{{c.enc.True, yes}, {c.enc.False, no}, {c.enc.Undefined, no}, {c.enc.Null, no}} {
		c.movImm(a64A, w.word)
		c.a.Cmp(r, a64A, true)
		c.a.BCond(arm64.EQ, w.to)
	}
	// An object is true unless it is [[IsHTMLDDA]], a string unless it is
	// empty, as amd64's are; anything else is not decided here.
	notObject := c.a.NewLabel()
	c.movImm(a64A, c.enc.Object)
	c.a.Cmp(r, a64A, true)
	c.a.BCond(arm64.NE, notObject)
	if c.reference(v, v.Args[0], c.enc.Object, guard) {
		c.a.LoadU8(a64A, a64C, c.enc.ObjectFlags)
		c.movImm(a64D, uint64(c.enc.FlagHTMLDDA))
		c.a.Tst(a64A, a64D, false)
		c.a.BCond(arm64.NE, no)
		c.a.B(yes)
	}
	c.a.Bind(notObject)
	c.movImm(a64A, c.enc.String)
	c.a.Cmp(r, a64A, true)
	guard(arm64.NE)
	if c.reference(v, v.Args[0], c.enc.String, guard) {
		c.a.Load(a64A, a64C, c.enc.StringLength)
		c.a.Cbnz(a64A, yes, true)
		c.a.B(no)
	}
	c.a.Bind(number)
	// A number is true unless zero or NaN: not equal to zero, and ordered.
	c.a.FMovToF(a64F0, r)
	c.a.FMovToF(a64F1, arm64.ZR)
	c.a.FCmp(a64F0, a64F1)
	c.a.Cset(a64A, arm64.NE)
	c.a.Cset(a64C, arm64.VC)
	c.a.Op(arm64.And, a64A, a64A, a64C, false)
	c.a.B(done)
	c.a.Bind(yes)
	c.movImm(a64A, 1)
	c.a.B(done)
	c.a.Bind(no)
	c.movImm(a64A, 0)
	c.a.Bind(done)
	c.setG(v, a64A)
}

// toInt32 is ToUint32's bits. FCVTZS is exact for |x| < 2**63 and gives 0
// for NaN, as ToUint32 does; it saturates past that, to 2**63-1 or -2**63,
// which a cold path redoes from the exponent and significand, as amd64's
// does.
func (c *a64Compiler) toInt32(v *ssa.Value) {
	xv := v.Args[0]
	slow, back := c.a.NewLabel(), c.a.NewLabel()
	x := c.fpr(xv, a64F0)
	c.a.Fcvtzs(a64A, x)
	c.movImm(a64B, math.MaxInt64)
	c.a.Cmp(a64A, a64B, true)
	c.a.BCond(arm64.EQ, slow)
	c.movImm(a64B, 1<<63)
	c.a.Cmp(a64A, a64B, true)
	c.a.BCond(arm64.EQ, slow)
	c.a.Bind(back)
	c.a.MovRR32(a64A, a64A)
	c.setG(v, a64A)
	c.cold = append(c.cold, func() {
		zero, positive := c.a.NewLabel(), c.a.NewLabel()
		c.a.Bind(slow)
		c.a.FMovFromF(a64A, c.fpr(xv, a64F0))
		// e = exponent; NaN and the infinities give 0.
		c.a.ShiftImm(arm64.Lsr, a64C, a64A, 52, true)
		c.movImm(a64D, 0x7FF)
		c.a.Op(arm64.And, a64C, a64C, a64D, false)
		c.a.CmpImm(a64C, 0x7FF, false)
		c.a.BCond(arm64.EQ, zero)
		c.a.AddImm(a64C, a64C, -1075, false)
		c.a.CmpImm(a64C, 32, false)
		c.a.BCond(arm64.HS, zero)
		c.a.MovRR(a64B, a64A) // the sign is bit 63
		c.a.ShiftImm(arm64.Lsl, a64A, a64A, 12, true)
		c.a.ShiftImm(arm64.Lsr, a64A, a64A, 12, true)
		c.a.ShiftReg(arm64.Lsl, a64A, a64A, a64C, true)
		c.a.CmpImm(a64B, 0, true)
		c.a.BCond(arm64.PL, positive)
		c.a.Neg(a64A, a64A, false)
		c.a.Bind(positive)
		c.a.B(back)
		c.a.Bind(zero)
		c.movImm(a64A, 0)
		c.a.B(back)
	})
}

// a64FOp, a64ALU and a64Shift are the instructions for SSA's arithmetic,
// bitwise and shift operations.
func a64FOp(op ssa.Op) arm64.FOp {
	switch op {
	case ssa.OpAddF64:
		return arm64.FAdd
	case ssa.OpSubF64:
		return arm64.FSub
	case ssa.OpMulF64:
		return arm64.FMul
	}
	return arm64.FDiv
}

func a64ALU(op ssa.Op) arm64.ALU {
	switch op {
	case ssa.OpAndI32:
		return arm64.And
	case ssa.OpOrI32:
		return arm64.Orr
	}
	return arm64.Eor
}

func a64Shift(op ssa.Op) arm64.Shift {
	switch op {
	case ssa.OpShlI32:
		return arm64.Lsl
	case ssa.OpSarI32:
		return arm64.Asr
	}
	return arm64.Lsr
}
