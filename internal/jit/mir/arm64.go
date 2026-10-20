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

// A native call's registers, as amd64's (callRet...): at its callee's call
// entry, where it returns to, the callee's closure and its link, and, for
// enterExit, the callee's function object; at its return, the result's
// words, and where it goes on in a64CallLink.
const (
	a64CallRet     = a64A
	a64CallClosure = a64C
	a64CallLink    = a64B
	a64CallObject  = arm64.Reg(9)
	a64RetNum      = a64A
	a64RetRef      = a64C
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
	// probe and probeUsed are amd64's.
	probe     [2]arm64.Label
	probeUsed [2]bool
	// ret and resume are amd64's.
	ret                 arm64.Label
	retUsed, resumeUsed bool
	// calling and goFails are amd64's.
	calling     arm64.Label
	callingUsed bool
	goFails     []a64GoFail
	live        arm64.Label
	liveUsed    bool
	// bigLive, bigLiveUsed and goCallBig are amd64's.
	bigLive                arm64.Label
	bigLiveUsed, goCallBig bool
	// saving is the routine every call of Go that saves every register
	// shares (goSaving), if any makes one.
	saving     arm64.Label
	savingUsed bool
	// refStoring is the routine of the stores storeRef makes (refStore).
	refStoring     arm64.Label
	refStoringUsed bool
	// appending is the routine the element writes Go makes share where a
	// number at an array's length may be appended natively
	// (appendingCall), as amd64's.
	appending     arm64.Label
	appendingUsed bool
}

// a64GoFail is amd64's goFail.
type a64GoFail struct {
	st       *abi.GoCallState
	back, to arm64.Label
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
	callAt, resumeAt := -1, -1
	for i, b := range c.order {
		if f.CallEntry && b == c.entryAt(0) {
			// As amd64's: native callers' entry.
			callAt = c.a.Len()
			c.callEntry()
		}
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
	if c.resumeUsed {
		resumeAt = c.a.Len()
		c.resumeCall()
	}
	if c.appendingUsed {
		c.a.Bind(c.appending)
		c.appendingCall()
	}
	if c.refStoringUsed {
		c.a.Bind(c.refStoring)
		c.refStoringCall()
	}
	if c.liveUsed {
		c.a.Bind(c.live)
		c.liveCall(c.enc.CallGo)
	}
	if c.bigLiveUsed {
		// As amd64's: a call the leaf has given up goes the way any other
		// does.
		c.a.Bind(c.bigLive)
		c.a.LoadU8(a64B, a64C, abi.OffGoCallBigOff)
		c.a.Cbnz(a64B, c.live, false)
		c.liveCall(c.enc.BigFast)
	}
	if c.callingUsed {
		// As amd64's: laid out just before saving.
		c.a.Bind(c.calling)
		c.a.Store(a64Ctx, abi.OffGoState, a64C)
		c.a.Store(a64Ctx, abi.OffGoSite, a64A)
	}
	if c.savingUsed {
		c.a.Bind(c.saving)
		c.savingCall()
	}
	for _, g := range c.goFails {
		g.st.Fail = int32(c.a.Offset(g.to) - c.a.Offset(g.back))
	}
	bytes, err := c.a.Finish()
	if err != nil {
		return nil, err
	}
	c.finishExits()
	return &Code{Bytes: bytes, Entries: entries, CallEntry: callAt, Resume: resumeAt, Text: len(bytes), Exits: c.exits,
		Calls: c.calls, CallRefs: c.callRefs(), GoCalls: c.goCalls, core: c.core}, nil
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
		if bigKept(v) != nil {
			// A BigInt's box, made of the BigInt in its cell (bigRebox),
			// which the record copies, as amd64's.
			w := c.bigRebox(v)
			at := abi.OffKeep + int32(v.Args[1].Index)*int32(c.enc.ValueSize)
			c.appendRecord(uint64(i)|abi.RecordMaybe, func() arm64.Reg {
				c.a.AddImm(a64B, a64Ctx, int64(at), true)
				return a64B
			}, 0, false, &w)
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
				{abi.OffInlineCallee, uint64(in.Callee)}, {abi.OffInlineArgs, in.Args},
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
// enterExit is where a native call goes whose callee has no call entry,
// as amd64's: with the call entry's registers, and the callee's function
// object in a64CallObject.
func (c *a64Compiler) enterExit() {
	const base, at, top = arm64.Reg(4), arm64.Reg(5), arm64.Reg(6)
	c.a.Load(a64Ctx, a64Ctx, abi.OffNext)
	c.a.Store(a64Ctx, abi.OffReturnTo, a64CallRet)
	c.a.Store(a64Ctx, abi.OffClosure, a64CallClosure)
	c.a.Load(a64CallClosure, a64CallClosure, c.enc.ClosureUpvalues)
	c.a.Store(a64Ctx, abi.OffUpvalues, a64CallClosure)
	c.a.Store(a64Ctx, abi.OffEnterCallee, a64CallObject)
	c.a.Load(at, a64Ctx, abi.OffStackTop)
	c.a.Load(base, at, 0)
	c.a.Store(a64Ctx, abi.OffBase, base)
	c.a.Load(top, a64CallLink, c.enc.LinkFrame)
	c.a.Op(arm64.Add, top, top, base, true)
	c.a.Store(at, 0, top)
	same := c.a.NewLabel()
	c.a.Load(at, a64Ctx, abi.OffStackHigh)
	c.a.Load(a64A, at, 0)
	c.a.Cmp(top, a64A, true)
	c.a.BCond(arm64.LS, same)
	c.a.Store(at, 0, top)
	c.a.Bind(same)
	c.a.ShiftImm(arm64.Lsl, base, base, 4, true)
	c.a.Load(at, a64Ctx, abi.OffStackBase)
	c.a.Op(arm64.Add, base, base, at, true)
	c.a.Store(a64Ctx, abi.OffLocals, base)
	c.a.Load(at, a64CallLink, c.enc.LinkLocals)
	c.a.ShiftImm(arm64.Lsl, at, at, 4, true)
	c.a.Op(arm64.Add, at, at, base, true)
	c.a.Store(a64Ctx, abi.OffStack, at)
	c.a.Store(a64Ctx, abi.OffOpen, arm64.ZR)
	c.a.Store(a64Ctx, abi.OffCells, arm64.ZR)
	c.a.MovImm(at, abi.LiveCall)
	c.a.Store(a64Ctx, abi.OffLive, at)
	c.a.Load(at, a64CallLink, c.enc.LinkIn)
	c.a.AddImm(at, at, 1, true)
	c.a.Store(a64CallLink, c.enc.LinkIn, at)
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

// returnNative returns, to the native caller, if there is one, with the
// result's words in a64RetNum and a64RetRef, or to Go, as amd64's: the
// code's first return makes it, the others jump there.
func (c *a64Compiler) returnNative() {
	if c.retUsed {
		c.a.B(c.ret)
		return
	}
	c.ret, c.retUsed = c.a.NewLabel(), true
	c.a.Bind(c.ret)
	toGo, word := c.a.NewLabel(), c.a.NewLabel()
	c.a.Load(a64CallLink, a64Ctx, abi.OffReturnTo)
	c.a.Cbz(a64CallLink, toGo, true)
	c.a.Load(a64RetNum, a64Ctx, abi.OffRet)
	c.a.Load(a64RetRef, a64Ctx, abi.OffRetFrom)
	c.a.Cbz(a64RetRef, word, true)
	c.a.Load(a64RetRef, a64RetRef, c.enc.RefOffset)
	c.a.Bind(word)
	c.returnTail()
	c.a.Bind(toGo)
	c.a.Ret()
}

// returnTail ends a native call, from the callee's context, as amd64's.
func (c *a64Compiler) returnTail() {
	c.a.Load(a64Stack, a64Ctx, abi.OffStackTop)
	c.a.Load(a64Locals, a64Ctx, abi.OffBase)
	c.a.Store(a64Stack, 0, a64Locals)
	c.a.Store(a64Ctx, abi.OffLive, arm64.ZR)
	c.a.Store(a64Ctx, abi.OffReturnTo, arm64.ZR)
	c.a.Load(a64Ctx, a64Ctx, abi.OffPrev)
	c.a.Load(a64Locals, a64Ctx, abi.OffLocals)
	c.a.Load(a64Stack, a64Ctx, abi.OffStack)
	c.a.Br(a64CallLink)
}

// resumeCall is where Go goes on after a native call it finished, as
// amd64's.
func (c *a64Compiler) resumeCall() {
	c.a.Load(a64CallLink, a64Ctx, abi.OffReturnTo)
	c.a.Load(a64RetNum, a64Ctx, abi.OffRetValue+c.enc.NumOffset)
	c.a.Load(a64RetRef, a64Ctx, abi.OffRetValue+c.enc.RefOffset)
	c.returnTail()
}

// callEntry is the function's call entry, as amd64's. It uses every
// register.
func (c *a64Compiler) callEntry() {
	vs := int32(c.enc.ValueSize)
	const base, at, top = arm64.Reg(4), arm64.Reg(5), arm64.Reg(6)
	c.a.Load(a64Ctx, a64Ctx, abi.OffNext)
	c.a.Store(a64Ctx, abi.OffReturnTo, a64CallRet)
	c.a.Store(a64Ctx, abi.OffClosure, a64CallClosure)
	c.a.Load(a64CallClosure, a64CallClosure, c.enc.ClosureUpvalues)
	c.a.Store(a64Ctx, abi.OffUpvalues, a64CallClosure)
	c.a.Load(at, a64Ctx, abi.OffStackTop)
	c.a.Load(base, at, 0)
	c.a.Store(a64Ctx, abi.OffBase, base)
	c.a.AddImm(top, base, int64(c.frameSize()), true)
	c.a.Store(at, 0, top)
	same := c.a.NewLabel()
	c.a.Load(at, a64Ctx, abi.OffStackHigh)
	c.a.Load(a64A, at, 0)
	c.a.Cmp(top, a64A, true)
	c.a.BCond(arm64.LS, same)
	c.a.Store(at, 0, top)
	c.a.Bind(same)
	c.a.ShiftImm(arm64.Lsl, base, base, 4, true)
	c.a.Load(at, a64Ctx, abi.OffStackBase)
	c.a.Op(arm64.Add, base, base, at, true)
	c.a.Store(a64Ctx, abi.OffLocals, base)
	c.a.AddImm(at, base, int64(c.f.FrameLocals)*int64(vs), true)
	c.a.Store(a64Ctx, abi.OffStack, at)
	c.a.Store(a64Ctx, abi.OffOpen, arm64.ZR)
	c.a.Store(a64Ctx, abi.OffCells, arm64.ZR)
	if c.f.Loops {
		c.a.Store(a64Ctx, abi.OffLoop, arm64.ZR)
	}
	c.a.MovImm(at, abi.LiveCall)
	c.a.Store(a64Ctx, abi.OffLive, at)
	c.a.Load(at, a64CallLink, c.enc.LinkIn)
	c.a.AddImm(at, at, 1, true)
	c.a.Store(a64CallLink, c.enc.LinkIn, at)
	if !c.f.StartOnly && c.f.Params < c.f.FrameLocals {
		c.movImm(a64A, c.enc.Undefined)
		for i := c.f.Params; i < c.f.FrameLocals; i++ {
			c.a.Store(base, int32(i)*vs+c.enc.NumOffset, a64A)
			c.a.Store(base, int32(i)*vs+c.enc.RefOffset, arm64.ZR)
		}
	}
}

// block emits one block. next is the block laid out after it.
func (c *a64Compiler) block(b *ssa.Block, next *ssa.Block) {
	if b.PC < 0 {
		c.a.Load(a64Locals, a64Ctx, abi.OffLocals)
		c.a.Load(a64Stack, a64Ctx, abi.OffStack)
		if c.f.StartOnly && c.f.Params < c.f.FrameLocals && b == c.entryAt(0) {
			// As amd64's: the frame's locals past its parameters, undefined.
			vs := int32(c.enc.ValueSize)
			c.movImm(a64A, c.enc.Undefined)
			for i := c.f.Params; i < c.f.FrameLocals; i++ {
				c.a.Store(a64Locals, int32(i)*vs+c.enc.NumOffset, a64A)
				c.a.Store(a64Locals, int32(i)*vs+c.enc.RefOffset, arm64.ZR)
			}
		}
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
	if c.enc.CallGo != 0 && exitKind(v.Aux) == abi.ExitHost {
		// As amd64's: what native code does not compare, Go does
		// (ssa.GoEqual).
		stub := exit
		exit = c.a.NewLabel()
		c.cold = append(c.cold, func() {
			c.a.Bind(exit)
			size := int32(unsafe.Sizeof(abi.GoArg{}))
			c.goArg(abi.OffGoArgs, x)
			c.goArg(abi.OffGoArgs+size, y)
			c.goSaving(uint64(ssa.GoEqual)|uint64(v.Index&1)<<abi.GoOpIndexShift, &stub)
			c.a.Load(a64A, a64Ctx, abi.OffGoArgs+c.enc.NumOffset)
			c.a.Cbnz(a64A, yes, true)
			c.a.B(no)
		})
	}
	wx, wy := c.gpr(x, a64A), c.gpr(y, a64B)
	c.a.Cmp(wx, wy, true)
	c.a.BCond(arm64.NE, differ)
	c.movImm(a64C, abi.NumberLimit)
	c.a.Cmp(wx, a64C, true)
	c.a.BCond(arm64.LO, number)
	for _, w := range []uint64{c.enc.Null, c.enc.Undefined, c.enc.True, c.enc.False, c.enc.Uninitialized} {
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
		// [[IsHTMLDDA]] alone, and two booleans are as strictly, as
		// amd64's.
		other = exit
		numbers, xNullish, yNullish := c.a.NewLabel(), c.a.NewLabel(), c.a.NewLabel()
		for _, w := range []uint64{c.enc.Null, c.enc.Undefined} {
			c.movImm(a64C, w)
			c.a.Cmp(wx, a64C, true)
			c.a.BCond(arm64.EQ, xNullish)
			c.a.Cmp(wy, a64C, true)
			c.a.BCond(arm64.EQ, yNullish)
		}
		xBool := c.a.NewLabel()
		for _, w := range []uint64{c.enc.True, c.enc.False} {
			c.movImm(a64C, w)
			c.a.Cmp(wx, a64C, true)
			c.a.BCond(arm64.EQ, xBool)
		}
		c.a.B(numbers)
		c.a.Bind(xBool)
		for _, w := range []uint64{c.enc.True, c.enc.False} {
			c.movImm(a64C, w)
			c.a.Cmp(wy, a64C, true)
			c.a.BCond(arm64.EQ, no)
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
	return c.referenceTo(v, a, word, guard, c.stubLabel(v.State, exitKind(v.Aux)))
}

// referenceTo is reference failing at fail, as guard does.
func (c *a64Compiler) referenceTo(v, a *ssa.Value, word uint64, guard func(arm64.Cond), fail arm64.Label) bool {
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
	var miss arm64.Label
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
		// As amd64's: a global binding's cell, writable, its attributes
		// its object's shape's.
		if r := c.gpr(v.Args[0], a64A); r != a64A {
			c.a.MovRR(a64A, r)
		}
		cell := v.Args[0]
		switch cell.Op {
		case ssa.OpGlobalCell:
			c.a.Load(a64B, a64Ctx, abi.OffGlobal)
		case ssa.OpLexCell:
			c.a.Load(a64B, a64Ctx, abi.OffGlobalLex)
		case ssa.OpBindingCell:
			c.movImm(a64B, cell.Const.Bits)
		default:
			panic("mir: a global binding's write of no binding's cell")
		}
		c.a.Load(a64B, a64B, c.enc.ObjectShape)
		c.a.Load(a64B, a64B, c.enc.ShapeKeys)
		c.a.LoadU8(a64B, a64B, int32(cell.Index)*c.enc.KeySize+c.enc.PropertyFlags)
		c.movImm(a64C, uint64(c.enc.PropNotWritable))
		c.a.Op(arm64.And, a64B, a64B, a64C, false)
		c.a.CmpImm(a64B, int64(c.enc.PropWritable), false)
		guard(arm64.NE)
	} else if v.Type == ssa.Bool {
		// As amd64's: a megamorphic site's write whose misses Go makes.
		miss = c.a.NewLabel()
		c.property(v, func(cond arm64.Cond) { c.a.BCond(cond, miss) })
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
	// As amd64's: while the collector marks, with Go's write barrier.
	c.a.FMovToF(a64F0, c.gpr(x, a64B))
	c.movImm(a64B, c.enc.WriteBarrier)
	c.a.LoadU8(a64B, a64B, 0)
	c.a.Cbnz(a64B, viaGo, false)
	c.a.Bind(scalar)
	c.a.Store(a64A, c.enc.NumOffset, c.gpr(x, a64B))
	c.a.Store(a64A, c.enc.RefOffset, a64C)
	c.a.Bind(done)
	c.a.Bind(added)
	if v.Type == ssa.Bool {
		// As amd64's: stored, false; a miss, true.
		stored := c.a.NewLabel()
		c.movImm(a64A, 0)
		c.setG(v, a64A)
		c.a.Bind(stored)
		c.cold = append(c.cold, func() {
			c.a.Bind(miss)
			c.movImm(a64A, 1)
			c.setG(v, a64A)
			c.a.B(stored)
		})
	}
	c.cold = append(c.cold, func() {
		c.a.Bind(viaGo)
		c.valueStore()
		c.a.B(done)
	})
}

// valueStore is amd64's: the cell's address in A, the value's words in F0
// and C, the pointer word stored through storeRef (refStore).
func (c *a64Compiler) valueStore() {
	c.a.FMovFromF(a64B, a64F0)
	c.a.Store(a64A, c.enc.NumOffset, a64B)
	c.a.AddImm(a64A, a64A, int64(c.enc.RefOffset), true)
	c.refStore()
}

// refStore is amd64's: the word's address in A, the pointer in C, through
// the routine the stores share (refStoringCall), which comes back to the
// site, at GoSite.
func (c *a64Compiler) refStore() {
	if !c.refStoringUsed {
		c.refStoring, c.refStoringUsed = c.a.NewLabel(), true
	}
	back := c.a.NewLabel()
	c.a.Adr(a64B, back)
	c.a.Store(a64Ctx, abi.OffGoSite, a64B)
	c.a.B(c.refStoring)
	c.a.Bind(back)
}

// refStoringCall is amd64's: every allocatable register saved, storeRef
// called, which comes back where B says, with the link register; the
// frame's registers read again, every allocatable one restored, and back
// to the site.
func (c *a64Compiler) refStoringCall() {
	for _, r := range c.usedRegs(c.gprs, false) {
		c.a.Store(a64Ctx, abi.OffRegs+int32(r)*8, arm64.Reg(r))
	}
	for _, r := range c.usedRegs(c.fprs, true) {
		c.a.StoreF(a64Ctx, abi.OffXRegs+int32(r)*8, arm64.FReg(r))
	}
	back := c.a.NewLabel()
	c.a.Adr(a64B, back)
	// The frame's locals, read again below, are the jump's register.
	c.movImm(a64Locals, c.enc.StoreRef)
	c.a.Br(a64Locals)
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

// goAppending is amd64's: an element write's store, its operands in the
// context's GoArgs, through appendingCall; if Go refuses, stub.
func (c *a64Compiler) goAppending(stub *arm64.Label) {
	if !c.appendingUsed {
		c.appending, c.appendingUsed = c.a.NewLabel(), true
	}
	back := c.a.NewLabel()
	c.a.Adr(a64A, back)
	c.a.Store(a64Ctx, abi.OffGoSite, a64A)
	c.a.B(c.appending)
	c.a.Bind(back)
	c.a.Load(a64B, a64Ctx, abi.OffGoStatus)
	c.a.Cbnz(a64B, *stub, true)
}

// appendingCall is the routine goAppending's sites share, as amd64's: a
// number at its array's length appended natively where the array and its
// prototypes let it, anything else Go's, through savingCall. It uses A, B,
// C, D, F0 and F1.
func (c *a64Compiler) appendingCall() {
	size := int32(unsafe.Sizeof(abi.GoArg{}))
	protos := c.f.ArrayProtos
	toGo := c.a.NewLabel()
	fail := func(cond arm64.Cond) { c.a.BCond(cond, toGo) }
	// The key, an index in [0, 2**32), as index makes it, and the array's
	// length.
	c.a.Load(a64A, a64Ctx, abi.OffGoArgs+size+c.enc.NumOffset)
	c.a.FMovToF(a64F0, a64A)
	c.a.Fcvtzs(a64A, a64F0)
	c.a.Scvtf(a64F1, a64A, true)
	c.a.FCmp(a64F1, a64F0)
	fail(arm64.NE)
	c.a.ShiftImm(arm64.Lsr, a64B, a64A, 32, true)
	c.a.CmpImm(a64B, 0, true)
	fail(arm64.NE)
	c.a.Load(a64C, a64Ctx, abi.OffGoArgs+c.enc.RefOffset)
	c.a.Load(a64B, a64C, c.enc.ObjectElems+8)
	c.a.Cmp(a64A, a64B, true)
	hole := c.a.NewLabel()
	c.a.BCond(arm64.LO, hole)
	fail(arm64.NE)
	c.a.Load(a64B, a64C, c.enc.ObjectElems+16)
	c.a.Cmp(a64A, a64B, true)
	fail(arm64.HS)
	c.arrayGuards(a64C, protos, fail)
	// The element at the length, then the length one more.
	c.a.Load(a64C, a64Ctx, abi.OffGoArgs+c.enc.RefOffset)
	c.a.Load(a64B, a64C, c.enc.ObjectElems+8)
	c.a.AddImm(a64D, a64B, 1, true)
	c.a.Store(a64C, c.enc.ObjectElems+8, a64D)
	c.a.ShiftImm(arm64.Lsl, a64B, a64B, 4, true)
	c.a.Load(a64A, a64C, c.enc.ObjectElems)
	c.a.Op(arm64.Add, a64A, a64A, a64B, true)
	c.a.Load(a64B, a64Ctx, abi.OffGoArgs+2*size+c.enc.NumOffset)
	c.a.FMovToF(a64F0, a64B)
	c.boxF64(a64F0, a64B)
	c.a.Store(a64A, c.enc.NumOffset, a64B)
	c.a.Store(a64A, c.enc.RefOffset, arm64.ZR)
	c.a.Store(a64Ctx, abi.OffGoStatus, arm64.ZR)
	c.a.Load(a64A, a64Ctx, abi.OffGoSite)
	c.a.Br(a64A)
	// As amd64's: a key below the length, a hole the number fills.
	c.a.Bind(hole)
	c.a.Load(a64B, a64C, c.enc.ObjectElems)
	c.a.ShiftImm(arm64.Lsl, a64A, a64A, 4, true)
	c.a.Op(arm64.Add, a64A, a64A, a64B, true)
	c.a.Load(a64B, a64A, c.enc.NumOffset)
	c.movImm(a64D, c.enc.Uninitialized)
	c.a.Cmp(a64B, a64D, true)
	fail(arm64.NE)
	c.a.MovRR(a64D, a64A)
	c.arrayGuards(a64C, protos, fail)
	c.a.Load(a64B, a64Ctx, abi.OffGoArgs+2*size+c.enc.NumOffset)
	c.a.FMovToF(a64F0, a64B)
	c.boxF64(a64F0, a64B)
	c.a.Store(a64D, c.enc.NumOffset, a64B)
	c.a.Store(a64Ctx, abi.OffGoStatus, arm64.ZR)
	c.a.Load(a64A, a64Ctx, abi.OffGoSite)
	c.a.Br(a64A)
	c.a.Bind(toGo)
	c.movImm(a64A, uint64(ssa.GoElemWrite))
	c.a.Store(a64Ctx, abi.OffGoOp, a64A)
	c.a.Store(a64Ctx, abi.OffGoKeep, arm64.ZR)
	if !c.savingUsed {
		c.saving, c.savingUsed = c.a.NewLabel(), true
	}
	c.a.B(c.saving)
}

// arrayGuards is amd64's: array p may take an element where it has none.
// It uses A, B and C.
func (c *a64Compiler) arrayGuards(p arm64.Reg, protos [2]ssa.Holder, fail func(arm64.Cond)) {
	c.a.LoadU8(a64B, p, c.enc.ObjectClass)
	c.a.CmpImm(a64B, int64(c.enc.ClassArray), false)
	fail(arm64.NE)
	c.a.LoadU8(a64B, p, c.enc.ObjectFlags)
	want := c.enc.FlagExtensible | c.enc.FlagLengthWritable
	c.movImm(a64A, uint64(want|c.enc.FlagSparse))
	c.a.Op(arm64.And, a64B, a64B, a64A, false)
	c.a.CmpImm(a64B, int64(want), false)
	fail(arm64.NE)
	c.a.Load(a64B, p, c.enc.ObjectProto)
	c.movImm(a64A, uint64(protos[0].Object))
	c.a.Cmp(a64B, a64A, true)
	fail(arm64.NE)
	c.protoGuards(protos, fail)
}

// goSaving calls Go, op, every allocatable register saved, as amd64's
// does.
func (c *a64Compiler) goSaving(op uint64, stub *arm64.Label) {
	c.goSavingKeep(op, 0, stub)
}

// goSavingKeep is goSaving whose result Go writes to keep cell keep.
func (c *a64Compiler) goSavingKeep(op uint64, keep int, stub *arm64.Label) {
	st, _ := c.goState(op, keep, nil, nil)
	c.goCalling(st, stub)
}

// goCallingLive is amd64's.
func (c *a64Compiler) goCallingLive(v *ssa.Value, st *abi.GoCallState, stub arm64.Label) {
	saves := c.savesOf(v)
	for _, sv := range saves {
		if sv.float {
			c.a.StoreF(a64Ctx, c.spillDisp(sv.slot), arm64.FReg(sv.reg))
		} else {
			c.a.Store(a64Ctx, c.spillDisp(sv.slot), arm64.Reg(sv.reg))
		}
	}
	if !c.liveUsed {
		c.live, c.liveUsed = c.a.NewLabel(), true
	}
	routine := &c.live
	if c.goCallBig {
		if !c.bigLiveUsed {
			c.bigLive, c.bigLiveUsed = c.a.NewLabel(), true
		}
		routine = &c.bigLive
	}
	back, fail := c.a.NewLabel(), c.a.NewLabel()
	c.movImm(a64C, uint64(uintptr(unsafe.Pointer(st))))
	c.a.Adr(a64A, back)
	c.a.B(*routine)
	c.a.Bind(back)
	restore := func() {
		for _, sv := range saves {
			if sv.float {
				c.a.LoadF(arm64.FReg(sv.reg), a64Ctx, c.spillDisp(sv.slot))
			} else {
				c.a.Load(arm64.Reg(sv.reg), a64Ctx, c.spillDisp(sv.slot))
			}
		}
	}
	restore()
	c.goFails = append(c.goFails, a64GoFail{st, back, fail})
	c.cold = append(c.cold, func() {
		c.a.Bind(fail)
		restore()
		c.a.B(stub)
	})
}

// liveCall is amd64's.
func (c *a64Compiler) liveCall(target uint64) {
	resume := c.a.NewLabel()
	c.a.Store(a64Ctx, abi.OffGoState, a64C)
	c.a.Store(a64Ctx, abi.OffGoSite, a64A)
	c.a.Adr(a64A, resume)
	c.a.Store(a64Ctx, abi.OffGoResume, a64A)
	c.movImm(a64A, target)
	c.a.Br(a64A)
	c.a.Bind(resume)
	c.a.Load(a64Locals, a64Ctx, abi.OffLocals)
	c.a.Load(a64Stack, a64Ctx, abi.OffStack)
	c.a.Load(a64A, a64Ctx, abi.OffGoSite)
	c.a.Br(a64A)
}

// goCalling is amd64's: the description's address in C, where the call
// comes back in A, through the routine the calls of Go share.
func (c *a64Compiler) goCalling(st *abi.GoCallState, stub *arm64.Label) {
	if !c.callingUsed {
		c.calling, c.callingUsed = c.a.NewLabel(), true
	}
	if !c.savingUsed {
		c.saving, c.savingUsed = c.a.NewLabel(), true
	}
	back := c.a.NewLabel()
	c.movImm(a64C, uint64(uintptr(unsafe.Pointer(st))))
	c.a.Adr(a64A, back)
	c.a.B(c.calling)
	c.a.Bind(back)
	if stub != nil {
		c.goFails = append(c.goFails, a64GoFail{st, back, *stub})
	}
}

// byCode is amd64's: the callee's function checked, its pointer word in C,
// and the object, its closure and its captured bindings kept in the
// context. It uses A, B and C.
func (c *a64Compiler) byCode(t *ssa.CallSite, next arm64.Label) {
	// A miss has the callee in C again, for the next site to check.
	miss, done := c.a.NewLabel(), c.a.NewLabel()
	c.a.Store(a64Ctx, abi.OffCallCallee, a64C)
	c.objectData(a64B, a64C, a64C, c.enc.FuncData, func(cond arm64.Cond) { c.a.BCond(cond, miss) })
	c.a.Load(a64B, a64B, c.enc.FuncClosure)
	c.a.Cbz(a64B, miss, true)
	for _, f := range [...]struct {
		off  int32
		want uintptr
	}{{c.enc.ClosureFn, t.ByCode}, {c.enc.ClosureEnv, t.Env}, {c.enc.ClosureRealm, t.Realm}} {
		c.a.Load(a64A, a64B, f.off)
		c.movImm(a64C, uint64(f.want))
		c.a.Cmp(a64A, a64C, true)
		c.a.BCond(arm64.NE, miss)
	}
	c.a.Store(a64Ctx, abi.OffCallClosure, a64B)
	c.a.Load(a64A, a64B, c.enc.ClosureUpvalues)
	c.a.Store(a64Ctx, abi.OffCallUpvalues, a64A)
	c.a.B(done)
	c.a.Bind(miss)
	c.a.Load(a64C, a64Ctx, abi.OffCallCallee)
	c.a.B(next)
	c.a.Bind(done)
}

// arrowThis is amd64's: the address of the this arrow t captured, the
// callee in C unless t checks its function, or next, the callee in C again.
// superGuards is amd64's: the function running and its prototype as the
// code was compiled for, the this unbound. It uses A and B.
func (c *a64Compiler) superGuards(t *ssa.CallSite, x *ssa.Value, guard func(arm64.Cond)) {
	c.a.Load(a64A, a64Ctx, abi.OffFunction)
	c.movImm(a64B, uint64(t.Active))
	c.a.Cmp(a64A, a64B, true)
	guard(arm64.NE)
	c.a.Load(a64A, a64A, c.enc.ObjectProto)
	c.movImm(a64B, uint64(t.Callee))
	c.a.Cmp(a64A, a64B, true)
	guard(arm64.NE)
	if t.Parent != 0 {
		c.a.Load(a64A, a64A, c.enc.ObjectProto)
		c.movImm(a64B, uint64(t.Parent))
		c.a.Cmp(a64A, a64B, true)
		guard(arm64.NE)
	}
	w := a64A
	if remat(x) {
		c.materialize(x, a64A)
	} else {
		w = c.gpr(x, a64A)
	}
	c.movImm(a64B, c.enc.Uninitialized)
	c.a.Cmp(w, a64B, true)
	guard(arm64.NE)
}

func (c *a64Compiler) arrowThis(t *ssa.CallSite, next arm64.Label) {
	miss, done := c.a.NewLabel(), c.a.NewLabel()
	if t.ByCode == 0 {
		c.a.Store(a64Ctx, abi.OffCallCallee, a64C)
	}
	c.a.Load(a64B, a64Ctx, abi.OffCallCallee)
	c.a.Load(a64B, a64B, c.enc.ObjectExt)
	c.a.Load(a64B, a64B, c.enc.ObjectData+8)
	c.a.Load(a64B, a64B, c.enc.FuncExtra)
	c.a.Cbz(a64B, miss, true)
	c.a.Load(a64A, a64B, c.enc.ExtraLexThisRef)
	c.a.Cbnz(a64A, miss, true)
	c.a.AddImm(a64B, a64B, int64(c.enc.ExtraLexThis), true)
	c.a.Store(a64Ctx, abi.OffCallThis, a64B)
	c.a.B(done)
	c.a.Bind(miss)
	c.a.Load(a64C, a64Ctx, abi.OffCallCallee)
	c.a.B(next)
	c.a.Bind(done)
}

// mega is amd64's: v's property looked up by the probe the code's sites
// share (megaProbe), its value's address in A, or a failure.
func (c *a64Compiler) mega(v *ssa.Value, guard func(arm64.Cond), found arm64.Label) {
	write := b2i(v.Op == ssa.OpPropWrite)
	if !c.probeUsed[write] {
		c.probe[write], c.probeUsed[write] = c.a.NewLabel(), true
		c.cold = append(c.cold, func() {
			c.a.Bind(c.probe[write])
			c.megaProbe(write == 1)
		})
	}
	back := c.a.NewLabel()
	c.movImm(a64C, uint64(uint32(v.Key)))
	c.a.Store(a64Ctx, abi.OffProbeKey, a64C)
	c.movImm(a64C, uint64(abi.MegaKeyHash(uint32(v.Key))))
	c.a.Store(a64Ctx, abi.OffProbeHash, a64C)
	c.a.Adr(a64C, back)
	c.a.Store(a64Ctx, abi.OffProbeBack, a64C)
	if p := c.gpr(v.Args[0], a64A); p != a64A {
		c.a.MovRR(a64A, p)
	}
	c.a.B(c.probe[write])
	c.a.Bind(back)
	c.a.CmpImm(a64A, 0, true)
	guard(arm64.EQ)
	c.a.B(found)
}

// megaProbe is amd64's: the probe a function's sites share, the receiver in
// A, back with the value's address in A, or 0. It uses every scratch
// register, F2, and for a read F1.
func (c *a64Compiler) megaProbe(write bool) {
	fail := c.a.NewLabel()
	back := func() {
		c.a.Load(a64B, a64Ctx, abi.OffProbeBack)
		c.a.Br(a64B)
	}
	c.a.FMovToF(a64F2, a64A)
	c.a.Load(a64B, a64A, c.enc.ObjectShape)
	c.a.ShiftImm(arm64.Lsr, a64C, a64B, 4, true)
	c.a.ShiftImm(arm64.Lsr, a64A, a64B, 15, true)
	c.a.Op(arm64.Eor, a64C, a64C, a64A, true)
	c.a.ShiftImm(arm64.Lsr, a64A, a64B, 26, true)
	c.a.Op(arm64.Eor, a64C, a64C, a64A, true)
	c.a.Load(a64A, a64Ctx, abi.OffProbeHash)
	c.a.Op(arm64.Eor, a64C, a64C, a64A, false)
	c.movImm(a64A, abi.MegaSets-1)
	c.a.Op(arm64.And, a64C, a64C, a64A, true)
	c.a.ShiftImm(arm64.Lsl, a64C, a64C, abi.MegaSetShift, true)
	c.a.Load(a64A, a64Ctx, abi.OffMega)
	c.a.Op(arm64.Add, a64A, a64A, a64C, true)
	// As amd64's: the set's ways, in turn; D holds the key.
	hit := c.a.NewLabel()
	c.a.Load(a64D, a64Ctx, abi.OffProbeKey)
	for w := int32(0); w < abi.MegaWays; w++ {
		next := c.a.NewLabel()
		c.a.Load(a64C, a64A, w*abi.MegaSize+abi.OffMegaShape)
		c.a.Cmp(a64B, a64C, true)
		c.a.BCond(arm64.NE, next)
		c.a.LoadU32(a64C, a64A, w*abi.MegaSize+abi.OffMegaKey)
		c.a.Cmp(a64C, a64D, false)
		c.a.BCond(arm64.NE, next)
		c.a.AddImm(a64A, a64A, int64(w*abi.MegaSize), true)
		c.a.B(hit)
		c.a.Bind(next)
	}
	c.a.B(fail)
	c.a.Bind(hit)
	c.a.LoadU32(a64C, a64A, abi.OffMegaIndex)
	if write {
		c.movImm(a64B, abi.MegaWritable)
		c.a.Tst(a64C, a64B, false)
		c.a.BCond(arm64.EQ, fail)
	} else {
		// As amd64's: a prototype's, along the chain, the last the holder;
		// or, absent, all of them, the last's prototype none. D holds the
		// object, B the link's word to compare.
		own, loop, holder, absent, end := c.a.NewLabel(), c.a.NewLabel(), c.a.NewLabel(), c.a.NewLabel(), c.a.NewLabel()
		c.a.FMovToF(a64F1, a64C)
		c.a.Load(a64A, a64A, abi.OffMegaChain)
		c.a.Cbz(a64A, own, true)
		c.a.FMovFromF(a64D, a64F2)
		c.a.Load(a64D, a64D, c.enc.ObjectProto)
		c.movImm(a64B, abi.MegaAbsent)
		c.a.Cmp(a64C, a64B, false)
		c.a.BCond(arm64.HS, absent)
		c.a.Bind(loop)
		c.a.Load(a64B, a64A, abi.OffLinkObject)
		c.a.Cmp(a64D, a64B, true)
		c.a.BCond(arm64.NE, fail)
		c.a.Load(a64C, a64D, c.enc.ObjectShape)
		c.a.Load(a64B, a64A, abi.OffLinkShape)
		c.a.Cmp(a64C, a64B, true)
		c.a.BCond(arm64.NE, fail)
		c.a.Load(a64B, a64A, abi.MegaLinkSize+abi.OffLinkObject)
		c.a.Cbz(a64B, holder, true)
		c.a.AddImm(a64A, a64A, int64(abi.MegaLinkSize), true)
		c.a.Load(a64D, a64D, c.enc.ObjectProto)
		c.a.B(loop)
		c.a.Bind(holder)
		c.a.FMovFromF(a64C, a64F1)
		c.a.ShiftImm(arm64.Lsl, a64C, a64C, 4, true)
		c.a.Load(a64A, a64D, c.enc.ObjectProps)
		c.a.Op(arm64.Add, a64A, a64A, a64C, true)
		back()
		c.a.Bind(absent)
		c.a.Load(a64B, a64A, abi.OffLinkObject)
		c.a.Cbz(a64B, end, true)
		c.a.Cmp(a64D, a64B, true)
		c.a.BCond(arm64.NE, fail)
		c.a.Load(a64C, a64D, c.enc.ObjectShape)
		c.a.Load(a64B, a64A, abi.OffLinkShape)
		c.a.Cmp(a64C, a64B, true)
		c.a.BCond(arm64.NE, fail)
		c.a.Load(a64D, a64D, c.enc.ObjectProto)
		c.a.AddImm(a64A, a64A, int64(abi.MegaLinkSize), true)
		c.a.B(absent)
		c.a.Bind(end)
		c.a.Cbnz(a64D, fail, true)
		c.movImm(a64A, c.enc.UndefinedCell)
		back()
		c.a.Bind(own)
		c.a.FMovFromF(a64C, a64F1)
	}
	c.movImm(a64B, ^uint64(abi.MegaWritable)&0xFFFFFFFF)
	c.a.Op(arm64.And, a64C, a64C, a64B, false)
	c.a.ShiftImm(arm64.Lsl, a64C, a64C, 4, true)
	c.a.FMovFromF(a64A, a64F2)
	c.a.Load(a64A, a64A, c.enc.ObjectProps)
	c.a.Op(arm64.Add, a64A, a64A, a64C, true)
	back()
	c.a.Bind(fail)
	c.movImm(a64A, 0)
	back()
}

// genericCheck is amd64's: call v, of a function none of its sites is, the
// callee in C, made through the function's link, or miss.
func (c *a64Compiler) genericCheck(v *ssa.Value, site *ssa.CallSite, target int, checked, miss arm64.Label) {
	c.a.Store(a64Ctx, abi.OffCallCallee, a64C)
	c.objectData(a64B, a64C, a64C, c.enc.FuncData, func(cond arm64.Cond) { c.a.BCond(cond, miss) })
	c.a.Load(a64B, a64B, c.enc.FuncClosure)
	c.a.Cbz(a64B, miss, true)
	for _, f := range [...]struct {
		off  int32
		want uintptr
	}{{c.enc.ClosureEnv, site.GenericEnv}, {c.enc.ClosureRealm, site.GenericRealm}} {
		c.a.Load(a64A, a64B, f.off)
		c.movImm(a64C, uint64(f.want))
		c.a.Cmp(a64A, a64C, true)
		c.a.BCond(arm64.NE, miss)
	}
	c.a.Store(a64Ctx, abi.OffCallClosure, a64B)
	c.a.Load(a64A, a64B, c.enc.ClosureUpvalues)
	c.a.Store(a64Ctx, abi.OffCallUpvalues, a64A)
	c.a.Load(a64A, a64B, c.enc.ClosureCode)
	c.a.Cbz(a64A, miss, true)
	c.a.LoadU32(a64A, a64A, c.enc.ClosureHint)
	c.a.Load(a64C, a64Ctx, abi.OffHints)
	c.a.Load(a64C, a64C, abi.OffHintLen)
	c.a.Cmp(a64A, a64C, true)
	c.a.BCond(arm64.HS, miss)
	c.a.ShiftImm(arm64.Lsl, a64A, a64A, 5, true)
	c.a.Load(a64C, a64Ctx, abi.OffHints)
	c.a.Load(a64C, a64C, abi.OffHintSlots)
	c.a.Op(arm64.Add, a64C, a64C, a64A, true)
	c.a.Load(a64A, a64B, c.enc.ClosureFn)
	c.a.Load(a64B, a64C, c.enc.HintSlotFn)
	c.a.Cmp(a64A, a64B, true)
	c.a.BCond(arm64.NE, miss)
	c.a.Load(a64C, a64C, c.enc.HintSlotLink)
	c.a.Cbz(a64C, miss, true)
	c.a.Store(a64Ctx, abi.OffCallLink, a64C)
	c.a.Load(a64A, a64C, c.enc.LinkFlags)
	c.movImm(a64B, abi.LinkNotGeneric)
	c.a.Tst(a64A, a64B, true)
	c.a.BCond(arm64.NE, miss)
	// As amd64's: an arrow, given the this it captured.
	notArrow := c.a.NewLabel()
	c.movImm(a64B, abi.LinkArrow)
	c.a.Tst(a64A, a64B, true)
	c.a.BCond(arm64.EQ, notArrow)
	c.a.Load(a64B, a64Ctx, abi.OffCallCallee)
	c.a.Load(a64B, a64B, c.enc.ObjectExt)
	c.a.Load(a64B, a64B, c.enc.ObjectData+8)
	c.a.Load(a64B, a64B, c.enc.FuncExtra)
	c.a.Cbz(a64B, miss, true)
	c.a.Load(a64C, a64B, c.enc.ExtraLexThisRef)
	c.a.Cbnz(a64C, miss, true)
	c.a.AddImm(a64B, a64B, int64(c.enc.ExtraLexThis), true)
	c.a.Store(a64Ctx, abi.OffCallThis, a64B)
	c.a.Bind(notArrow)
	c.movImm(a64B, abi.LinkCoerce)
	c.a.Tst(a64A, a64B, true)
	if site.Method {
		object := c.a.NewLabel()
		c.a.BCond(arm64.EQ, object)
		c.movImm(a64B, c.enc.Object)
		c.a.Cmp(c.gpr(v.Args[0], a64A), a64B, true)
		c.a.BCond(arm64.NE, miss)
		c.a.Bind(object)
	} else {
		c.a.BCond(arm64.NE, miss)
	}
	c.a.Load(a64A, a64Ctx, abi.OffStackTop)
	c.a.Load(a64A, a64A, 0)
	c.a.Load(a64C, a64Ctx, abi.OffCallLink)
	c.a.Load(a64B, a64C, c.enc.LinkFrame)
	c.a.Op(arm64.Add, a64A, a64A, a64B, true)
	c.a.Load(a64B, a64Ctx, abi.OffStackEnd)
	c.a.Cmp(a64A, a64B, true)
	c.a.BCond(arm64.HI, miss)
	c.movImm(a64B, uint64(target))
	c.a.Store(a64Ctx, abi.OffCallTarget, a64B)
	c.a.B(checked)
}

// objectData is amd64's: dst the pointer word of the data of the object in
// obj, which has its side fields and data of type typ. It uses t, which
// may be obj, and A; dst must be neither.
func (c *a64Compiler) objectData(dst, obj, t arm64.Reg, typ uint64, fail func(arm64.Cond)) {
	c.a.Load(dst, obj, c.enc.ObjectExt)
	c.a.CmpImm(dst, 0, true)
	fail(arm64.EQ)
	c.a.Load(t, dst, c.enc.ObjectData)
	c.movImm(a64A, typ)
	c.a.Cmp(t, a64A, true)
	fail(arm64.NE)
	c.a.Load(dst, dst, c.enc.ObjectData+8)
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
	if c.enc.ValueSize != 16 {
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
		c.addAlongMarking(v, add, done)
	})
	c.a.AddImm(a64C, a64B, 1, true)
	c.a.Store(o, c.enc.ObjectProps+8, a64C)
	c.movImm(a64C, uint64(add.Next))
	c.a.Store(o, c.enc.ObjectShape, a64C)
	// The value's address: the old length times 16, past the table's
	// start; the shape has the key.
	c.a.ShiftImm(arm64.Lsl, a64B, a64B, 4, true)
	c.a.Load(a64C, o, c.enc.ObjectProps)
	c.a.Op(arm64.Add, a64C, a64C, a64B, true)
	c.a.FMovFromF(a64B, a64F1)
	c.a.Store(a64C, c.enc.RefOffset, a64B)
	c.a.Store(a64C, c.enc.NumOffset, c.gpr(x, a64A))
	c.a.Bind(done)
}

// addAlongMarking is amd64's: the table's length in B, the value's pointer
// word in F1; the entry's address and the pointer word wait in GoArgs.
func (c *a64Compiler) addAlongMarking(v *ssa.Value, add *ssa.PropertyAdd, done arm64.Label) {
	x := v.Args[1]
	o := c.gpr(v.Args[0], a64D)
	c.a.AddImm(a64C, a64B, 1, true)
	c.a.Store(o, c.enc.ObjectProps+8, a64C)
	c.a.ShiftImm(arm64.Lsl, a64B, a64B, 4, true)
	c.a.Load(a64C, o, c.enc.ObjectProps)
	c.a.Op(arm64.Add, a64C, a64C, a64B, true)
	c.a.Store(a64Ctx, abi.OffGoArgs+c.enc.NumOffset, a64C)
	c.a.FMovFromF(a64B, a64F1)
	c.a.Store(a64Ctx, abi.OffGoArgs+c.enc.RefOffset, a64B)
	c.a.AddImm(a64A, o, int64(c.enc.ObjectShape), true)
	c.movImm(a64C, uint64(add.Next))
	c.refStore()
	c.a.Load(a64C, a64Ctx, abi.OffGoArgs+c.enc.NumOffset)
	c.a.Store(a64C, c.enc.NumOffset, c.gpr(x, a64A))
	c.a.Load(a64A, a64Ctx, abi.OffGoArgs+c.enc.NumOffset)
	c.a.AddImm(a64A, a64A, int64(c.enc.RefOffset), true)
	c.a.Load(a64C, a64Ctx, abi.OffGoArgs+c.enc.RefOffset)
	c.refStore()
	c.a.B(done)
}

// property finds the property a property operation names and leaves the
// address of its value in A, as amd64's does: any other shape searched at
// a megamorphic site, failing at any other. It uses B, C and D.
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
		if !v.Megamorphic() {
			guard(arm64.NE)
		} else {
			c.a.BCond(arm64.NE, scan)
			c.cold = append(c.cold, func() {
				c.a.Bind(scan)
				c.scan(v, guard, found, 0)
			})
		}
		c.a.Load(a64A, p, c.enc.ObjectProps)
		c.a.AddImm(a64A, a64A, int64(int32(v.Index)*c.enc.ValueSize), true)
		c.a.Bind(found)
		return
	}
	if !v.Megamorphic() && !v.Scans() {
		// As amd64's: every receiver fails.
		c.a.Cmp(a64B, a64B, true)
		guard(arm64.EQ)
		c.a.Bind(found)
		return
	}
	c.scan(v, guard, found, 0)
	c.a.Bind(found)
}

// scan searches an object's small table for the key a property operation
// names, as amd64's does, going to found with the value's address in A.
func (c *a64Compiler) scan(v *ssa.Value, guard func(arm64.Cond), found, mega arm64.Label) {
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
	if c.enc.Mega && mega == 0 && v.Megamorphic() {
		mega = c.a.NewLabel()
		c.cold = append(c.cold, func() {
			c.a.Bind(mega)
			c.mega(v, guard, found)
		})
	}
	if mega != 0 {
		// As amd64's: too large to search, the megamorphic cache's.
		c.a.BCond(arm64.HI, mega)
	} else {
		guard(arm64.HI)
	}
	// No properties, no shape -- but for a read the megamorphic cache's,
	// as amd64's.
	c.a.CmpImm(a64B, 0, true)
	if mega != 0 && v.Op != ssa.OpPropWrite && v.Megamorphic() {
		c.a.BCond(arm64.EQ, mega)
	} else {
		guard(arm64.EQ)
	}
	c.a.Load(a64A, p, c.enc.ObjectShape)
	c.a.Load(a64A, a64A, c.enc.ShapeKeys)
	mask, want := int64(c.enc.PropNotData), int64(0)
	if v.Op == ssa.OpPropWrite {
		mask, want = int64(c.enc.PropNotWritable), int64(c.enc.PropWritable)
	}
	c.movImm(a64D, uint64(mask))
	// A loop over the keys, as amd64's.
	loop, hit := c.a.NewLabel(), c.a.NewLabel()
	c.a.Bind(loop)
	c.a.CmpImm(a64B, 0, true)
	if mega != 0 && v.Op != ssa.OpPropWrite && v.Megamorphic() {
		// As amd64's: not its own, a prototype's, which the megamorphic
		// cache may have.
		c.a.BCond(arm64.EQ, mega)
	} else {
		guard(arm64.EQ)
	}
	c.a.LoadU32(a64C, a64A, c.enc.PropertyKey)
	c.a.CmpImm(a64C, int64(v.Key), false)
	c.a.BCond(arm64.EQ, hit)
	c.a.AddImm(a64A, a64A, int64(c.enc.KeySize), true)
	c.a.AddImm(a64B, a64B, -1, true)
	c.a.B(loop)
	c.a.Bind(hit)
	c.a.LoadU8(a64C, a64A, c.enc.PropertyFlags)
	c.a.Op(arm64.And, a64C, a64C, a64D, false)
	c.a.CmpImm(a64C, want, false)
	guard(arm64.NE)
	// As amd64's: the value at the key's position.
	p = c.gpr(v.Args[0], a64C)
	c.a.Load(a64C, p, c.enc.ObjectProps+8)
	c.a.Op(arm64.Sub, a64C, a64C, a64B, true)
	c.a.ShiftImm(arm64.Lsl, a64C, a64C, 4, true)
	p = c.gpr(v.Args[0], a64A)
	c.a.Load(a64A, p, c.enc.ObjectProps)
	c.a.Op(arm64.Add, a64A, a64A, a64C, true)
	c.a.B(found)
}

// keyAt is amd64's: the object in A has a property at v's index, of v's
// key, its shape's keys left in B. It uses C.
func (c *a64Compiler) keyAt(v *ssa.Value, guard func(arm64.Cond)) {
	c.a.Load(a64B, a64A, c.enc.ObjectProps+8)
	c.a.CmpImm(a64B, int64(v.Index), true)
	guard(arm64.LS)
	c.a.Load(a64B, a64A, c.enc.ObjectShape)
	c.a.Load(a64B, a64B, c.enc.ShapeKeys)
	c.a.LoadU32(a64C, a64B, int32(v.Index)*c.enc.KeySize+c.enc.PropertyKey)
	c.a.CmpImm(a64C, int64(v.Key), false)
	guard(arm64.NE)
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
			// valueStore's: the cell's address in A, the words in F0 and C.
			c.a.FMovToF(a64F0, a64D)
			c.a.MovRR(a64C, a64B)
			c.a.Load(a64A, a64Ctx, at+c.enc.RefOffset)
			c.a.Load(a64A, a64A, c.enc.ObjectElems)
			c.a.AddImm(a64A, a64A, int64(int32(i)*vs), true)
			c.valueStore()
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

// poolTakeMarking is amd64's: poolTake, the pool's cell cleared with Go's
// write barrier, which shades the object.
func (c *a64Compiler) poolTakeMarking(v *ssa.Value, site *ssa.CallSite) {
	at := abi.OffKeep + int32(v.Index)*int32(c.enc.ValueSize)
	c.movImm(a64A, uint64(site.Pool))
	c.a.Load(a64B, a64A, abi.OffPoolCount)
	c.a.AddImm(a64B, a64B, -1, true)
	c.a.Store(a64A, abi.OffPoolCount, a64B)
	c.a.ShiftImm(arm64.Lsl, a64B, a64B, 3, true)
	c.a.Op(arm64.Add, a64B, a64B, a64A, true)
	c.a.Load(a64C, a64B, abi.OffPoolObjects)
	c.a.Store(a64Ctx, at+c.enc.RefOffset, a64C)
	c.movImm(a64A, c.enc.Object)
	c.a.Store(a64Ctx, at+c.enc.NumOffset, a64A)
	c.a.AddImm(a64A, a64B, int64(abi.OffPoolObjects), true)
	c.movImm(a64C, 0)
	c.refStore()
	c.movImm(a64A, c.enc.Object)
	c.setG(v, a64A)
}

// poolShade is amd64's: while the collector marks, the next object of each
// pool a call of sites takes its receiver from shaded, stored where it is
// with Go's write barrier.
func (c *a64Compiler) poolShade(sites []*ssa.CallSite) {
	var pools []uintptr
	for _, t := range sites {
		if t.Pool != 0 && !slices.Contains(pools, t.Pool) {
			pools = append(pools, t.Pool)
		}
	}
	if len(pools) == 0 {
		return
	}
	marked, back := c.a.NewLabel(), c.a.NewLabel()
	c.movImm(a64B, c.enc.WriteBarrier)
	c.a.LoadU8(a64B, a64B, 0)
	c.a.Cbnz(a64B, marked, false)
	c.a.Bind(back)
	c.cold = append(c.cold, func() {
		c.a.Bind(marked)
		for _, pool := range pools {
			empty := c.a.NewLabel()
			c.movImm(a64A, uint64(pool))
			c.a.Load(a64B, a64A, abi.OffPoolCount)
			c.a.Cbz(a64B, empty, true)
			c.a.ShiftImm(arm64.Lsl, a64B, a64B, 3, true)
			c.a.Op(arm64.Add, a64A, a64A, a64B, true)
			c.a.AddImm(a64A, a64A, int64(abi.OffPoolObjects-8), true)
			c.a.Load(a64C, a64A, 0)
			c.refStore()
			c.a.Bind(empty)
		}
		c.a.B(back)
	})
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
		defer c.a.Bind(c.markingTake(v, site))
		if site.Argc == 0 {
			c.poolTake(v, site)
			return
		}
		c.arrayLiteral(v, site)
		return
	}
	sp := len(s.Slots)
	calleeSlot := sp - site.Argc - 1
	var callee *ssa.Value
	if site.Accessor != ssa.AccessorNone || site.Super {
		// As amd64's: an accessor's or a super()'s, no function among the
		// operands.
		calleeSlot = sp - site.Argc
	} else {
		callee = v.Args[len(v.Args)-site.Argc-1]
	}
	join, generic := arm64.Label(0), false
	if !site.Alloc && !site.Receiver {
		// As amd64's: natively while the collector marks, its result stored
		// with Go's barrier then, a receiver from a pool shaded first.
		if c.enc.CallGo != 0 && len(v.Args) <= abi.MaxGoArgs {
			join, generic = c.a.NewLabel(), true
		}
		c.poolShade(sites)
		if generic {
			defer c.a.Bind(join)
		}
	}
	c.a.Load(a64B, a64Ctx, abi.OffLevel)
	c.a.AddImm(a64B, a64B, 1, true)
	c.a.Load(a64C, a64Ctx, abi.OffBounds)
	c.a.Load(a64C, a64C, abi.OffBoundsLevelLimit)
	c.a.Cmp(a64B, a64C, true)
	guard(arm64.HS)
	if callee == nil {
		c.movImm(a64C, uint64(site.Callee))
	} else {
		if callee.Shadow == nil && c.origin.At(callee) < 0 || site.Via != 0 && (len(sites) != 1 || !c.operandsLive(s, calleeSlot-1)) {
			c.a.B(stub)
			return
		}
		c.movImm(a64B, c.enc.Object)
		c.a.Cmp(c.gpr(callee, a64A), a64B, true)
		guard(arm64.NE)
		c.sourceRef(callee, guard)
	}
	checked := c.a.NewLabel()
	// As amd64's: past its sites, any other function (genericCheck).
	gen := generic && site.Generic && site.Via == 0 && site.Pool == 0 && !site.Push && !site.Pop && !site.Alloc && !site.Receiver &&
		!site.Literal && vs == 16 && c.enc.HintSlotSize == 32
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
		if len(sites) > 1 || gen {
			// As amd64's: which one (abi.Context's CallTarget).
			c.movImm(a64B, uint64(k))
			c.a.Store(a64Ctx, abi.OffCallTarget, a64B)
		}
		if t.Via != 0 {
			c.viaGuards(t, v.Args[0], guard)
		}
		if t.ArrowThis {
			c.arrowThis(t, next)
		}
		if t.Super {
			c.superGuards(t, v.Args[0], guard)
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
			if t.Alloc && t.Argc == 1 {
				c.lengthGuards(t, v.Args[len(v.Args)-1], guard)
			}
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
		if t.Parent != 0 {
			// As amd64's: the callee's prototype still the parent.
			c.a.Load(a64B, a64C, c.enc.ObjectProto)
			c.movImm(a64A, uint64(t.Parent))
			c.a.Cmp(a64B, a64A, true)
			guard(arm64.NE)
		}
		if t.Pool != 0 {
			c.constructGuards(t, guard)
		}
		c.a.B(checked)
		c.a.Bind(next)
	}
	if gen {
		// As amd64's: a function none of them is, called through its link,
		// or by Go from native code.
		miss := c.a.NewLabel()
		c.genericCheck(v, site, len(sites), checked, miss)
		c.a.Bind(miss)
		cold, op := c.a.NewLabel(), goCallWord(site)
		c.a.B(cold)
		c.cold = append(c.cold, func() {
			c.a.Bind(cold)
			c.goCallOp(v, op, true)
			c.a.B(join)
		})
	} else {
		c.a.B(stub)
	}
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
		join := c.markingTake(v, site)
		c.poolTake(v, site)
		c.a.Bind(join)
		if site.Alloc && site.Argc == 1 {
			c.allocLength(v, v.Args[len(v.Args)-1])
		}
		return
	}
	// As amd64's: a call of one function writes its operands where the
	// callee has them, unrecorded.
	direct, ops := len(sites) == 1 && !gen && !site.ArrowThis && (site.ThisSlot < 0 || site.Method || site.Pool != 0 || site.Super || site.PlainThis), calleeSlot
	if site.Method {
		ops--
	}
	direct = direct && c.operandsLive(s, ops)
	// As amd64's: the state, described (callState), the call writing only
	// what the caller's code keeps nowhere else.
	operands := -1
	if !direct {
		operands = calleeSlot - 1
	}
	st, recs := c.callState(v, s, func(i int) bool { return direct && i >= ops }, operands)
	record := map[int]int32{}
	for k, r := range recs {
		at := abi.OffRecord + int32(k)*32
		record[r.slot] = int32(k)
		if r.big {
			// A BigInt's box's words, made of the BigInt.
			c.bigBoxWords(r.x.Args[1])
			c.a.Store(a64Ctx, at+16, a64A)
			c.a.Store(a64Ctx, abi.OffRecordRef+int32(k)*8, a64C)
			continue
		}
		w := a64A
		switch {
		case !r.num && (!r.ref || r.x.Shadow != nil):
		case remat(r.x):
			c.materialize(r.x, a64A)
		default:
			w = c.gpr(r.x, a64A)
		}
		if r.num {
			c.a.Store(a64Ctx, at+16, w)
		}
		if r.ref {
			c.pointerWord(r.x, w)
			c.a.Store(a64Ctx, abi.OffRecordRef+int32(k)*8, a64B)
		}
	}
	c.movImm(a64A, uint64(uintptr(unsafe.Pointer(st))))
	c.a.Store(a64Ctx, abi.OffCallState, a64A)
	args, this := sp-site.Argc, calleeSlot-1
	if site.Via != 0 {
		args, this = args+1, args
	}
	// A receiver written straight into the callee's context: one it reads,
	// or a method call's, which a callee that does not read it has too --
	// a frame Go makes for it (jitGoCallNested, jitUnwindNative) shows it
	// in a stack trace, as the interpreter's does.
	receiver := direct && site.Pool == 0 && !site.Super && !site.PlainThis && (site.ThisSlot >= 0 || site.Method && this < sp)
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
	// As amd64's: the callee makes its own frame and context at its call
	// entry, the call writing what only it knows; a callee with none Go
	// makes, from native code in code that calls Go, or once its frame is
	// made (enterExit).
	const calleeCtx, locals, tmp, top, high2 = arm64.Reg(3), arm64.Reg(5), arm64.Reg(6), arm64.Reg(7), arm64.Reg(8)
	back := c.a.NewLabel()
	c.resumeUsed = true
	op := goCallWord(site)
	enter := func() {
		if !c.enterUsed {
			c.enter, c.enterUsed = c.a.NewLabel(), true
		}
		c.a.B(c.enter)
	}
	// frame has locals be the callee's frame's address, past the VM's
	// stack's top.
	frame := func() {
		c.a.Load(locals, a64Ctx, abi.OffStackTop)
		c.a.Load(locals, locals, 0)
		c.a.ShiftImm(arm64.Lsl, locals, locals, 4, true)
		c.a.Load(tmp, a64Ctx, abi.OffStackBase)
		c.a.Op(arm64.Add, locals, locals, tmp, true)
	}
	// setup is amd64's: it keeps a64CallLink and a64D, the callee's entry.
	setup := func(t *ssa.CallSite) {
		framed := false
		argc, first := t.Argc, sp-t.Argc
		if t.Via != 0 {
			argc, first = argc-1, first+1
		}
		for i := 0; i < t.Params && i < t.LocalCount; i++ {
			at := int32(i) * vs
			if i < argc && direct {
				continue
			}
			if !framed {
				frame()
				framed = true
			}
			if i < argc {
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
		if t.Tail || t.Pool != 0 || t.Super || t.ArrowThis || t.PlainThis || (t.ThisSlot >= 0 || site.Method && site.Via == 0) && !direct || !receiver {
			c.a.Load(calleeCtx, a64Ctx, abi.OffNext)
		}
		if t.Tail {
			c.movImm(top, b2u(t.TailFrame))
			c.a.Store(calleeCtx, abi.OffTailFrame, top)
			c.a.Store(calleeCtx, abi.OffFrameThis, arm64.ZR)
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
			if t.Derived {
				// As amd64's: a derived constructor's instance, its this
				// unbound, the function whose code it is.
				c.a.Store(calleeCtx, abi.OffInstance+c.enc.RefOffset, high2)
				c.movImm(tmp, c.enc.Object)
				c.a.Store(calleeCtx, abi.OffInstance+c.enc.NumOffset, tmp)
				c.a.Store(calleeCtx, abi.OffThis+c.enc.RefOffset, arm64.ZR)
				c.movImm(tmp, c.enc.Uninitialized)
				c.a.Store(calleeCtx, abi.OffThis+c.enc.NumOffset, tmp)
				c.movImm(tmp, uint64(derivedFunction(t)))
				c.a.Store(calleeCtx, abi.OffFunction, tmp)
			} else {
				c.a.Store(calleeCtx, abi.OffThis+c.enc.RefOffset, high2)
				c.movImm(tmp, c.enc.Object)
				c.a.Store(calleeCtx, abi.OffThis+c.enc.NumOffset, tmp)
			}
			// A construction's new.target, for Go to make its frame with.
			c.movImm(top, uint64(t.Callee))
			c.a.Store(calleeCtx, abi.OffNewTarget, top)
		} else if t.Super {
			// As amd64's: the instance, a derived parent's to bind, new.target
			// handed on, the function whose code runs.
			c.a.Load(top, a64Ctx, abi.OffInstance+c.enc.RefOffset)
			c.movImm(tmp, c.enc.Object)
			if t.Derived {
				c.a.Store(calleeCtx, abi.OffInstance+c.enc.RefOffset, top)
				c.a.Store(calleeCtx, abi.OffInstance+c.enc.NumOffset, tmp)
				c.a.Store(calleeCtx, abi.OffThis+c.enc.RefOffset, arm64.ZR)
				c.movImm(tmp, c.enc.Uninitialized)
				c.a.Store(calleeCtx, abi.OffThis+c.enc.NumOffset, tmp)
			} else {
				c.a.Store(calleeCtx, abi.OffThis+c.enc.RefOffset, top)
				c.a.Store(calleeCtx, abi.OffThis+c.enc.NumOffset, tmp)
			}
			c.a.Load(top, a64Ctx, abi.OffNewTarget)
			c.a.Store(calleeCtx, abi.OffNewTarget, top)
			c.movImm(top, uint64(derivedFunction(t)))
			c.a.Store(calleeCtx, abi.OffFunction, top)
		} else if t.ArrowThis {
			// As amd64's: the this it captured.
			c.a.Load(high2, a64Ctx, abi.OffCallThis)
			c.a.Load(top, high2, c.enc.NumOffset)
			c.a.Store(calleeCtx, abi.OffThis+c.enc.NumOffset, top)
			c.a.Load(top, high2, c.enc.RefOffset)
			c.a.Store(calleeCtx, abi.OffThis+c.enc.RefOffset, top)
		} else if t.PlainThis && t.Global != 0 {
			// As amd64's: a sloppy callee's, the global object.
			c.movImm(top, c.enc.Object)
			c.a.Store(calleeCtx, abi.OffThis+c.enc.NumOffset, top)
			c.movImm(top, uint64(t.Global))
			c.a.Store(calleeCtx, abi.OffThis+c.enc.RefOffset, top)
		} else if t.PlainThis {
			// A strict callee's: undefined.
			c.movImm(top, c.enc.Undefined)
			c.a.Store(calleeCtx, abi.OffThis+c.enc.NumOffset, top)
			c.a.Store(calleeCtx, abi.OffThis+c.enc.RefOffset, arm64.ZR)
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
			c.a.Load(a64CallClosure, a64Ctx, abi.OffCallClosure)
		} else {
			c.movImm(a64CallClosure, uint64(t.Closure))
		}
		c.a.Adr(a64CallRet, back)
	}
	for i, t := range sites {
		next := c.a.NewLabel()
		if i < len(sites)-1 || gen {
			c.a.Load(a64A, a64Ctx, abi.OffCallTarget)
			c.a.CmpImm(a64A, int64(i), true)
			c.a.BCond(arm64.NE, next)
		}
		if t.Entry == 0 {
			// Not a function's (push, pop, a construction with nothing to
			// run), which the checks do not send here.
			c.a.B(stub)
			c.a.Bind(next)
			continue
		}
		none := c.a.NewLabel()
		c.movImm(a64CallLink, uint64(t.Entry)-uint64(c.enc.LinkEntry))
		c.a.Load(a64D, a64CallLink, c.enc.LinkEntry)
		c.a.Cbz(a64D, none, true)
		setup(t)
		c.a.Br(a64D)
		c.cold = append(c.cold, func() {
			c.a.Bind(none)
			if generic {
				c.goCallOp(v, op, true)
				c.a.B(join)
				return
			}
			setup(t)
			if t.ByCode != 0 {
				c.a.Load(a64CallObject, a64Ctx, abi.OffCallCallee)
			} else {
				c.movImm(a64CallObject, uint64(t.Callee))
			}
			enter()
		})
		c.a.Bind(next)
	}
	if gen {
		// As amd64's: its frame made as its link says.
		setupGen := func() {
			frame()
			c.a.Load(calleeCtx, a64Ctx, abi.OffNext)
			notail := c.a.NewLabel()
			c.a.Load(top, a64CallLink, c.enc.LinkFlags)
			c.movImm(tmp, abi.LinkTail)
			c.a.Tst(top, tmp, true)
			c.a.BCond(arm64.EQ, notail)
			c.movImm(top, 1)
			c.a.Store(calleeCtx, abi.OffTailFrame, top)
			c.a.Store(calleeCtx, abi.OffFrameThis, arm64.ZR)
			c.a.Bind(notail)
			fill := c.a.NewLabel()
			first := sp - site.Argc
			for i := 0; i < site.Argc; i++ {
				c.a.Load(top, a64CallLink, c.enc.LinkParams)
				c.a.CmpImm(top, int64(i), true)
				c.a.BCond(arm64.LS, fill)
				nb, nd, rb, rd := words(first + i)
				c.a.Load(top, nb, nd)
				c.a.Store(locals, int32(i)*vs+c.enc.NumOffset, top)
				c.a.Load(top, rb, rd)
				c.a.Store(locals, int32(i)*vs+c.enc.RefOffset, top)
			}
			c.a.Bind(fill)
			// Undefined in its other parameters, from the arguments on.
			loop, filled := c.a.NewLabel(), c.a.NewLabel()
			c.movImm(a64A, uint64(site.Argc))
			c.movImm(high2, c.enc.Undefined)
			c.a.Bind(loop)
			c.a.Load(tmp, a64CallLink, c.enc.LinkParams)
			c.a.Cmp(a64A, tmp, true)
			c.a.BCond(arm64.HS, filled)
			c.a.ShiftImm(arm64.Lsl, top, a64A, 4, true)
			c.a.Op(arm64.Add, top, top, locals, true)
			c.a.Store(top, c.enc.NumOffset, high2)
			c.a.Store(top, c.enc.RefOffset, arm64.ZR)
			c.a.AddImm(a64A, a64A, 1, true)
			c.a.B(loop)
			c.a.Bind(filled)
			// As amd64's: an arrow's this the one it captured.
			arrow, thisDone := c.a.NewLabel(), c.a.NewLabel()
			c.a.Load(top, a64CallLink, c.enc.LinkFlags)
			c.movImm(tmp, abi.LinkArrow)
			c.a.Tst(top, tmp, true)
			c.a.BCond(arm64.NE, arrow)
			if site.Method {
				nb, nd, rb, rd := words(calleeSlot - 1)
				c.a.Load(top, nb, nd)
				c.a.Store(calleeCtx, abi.OffThis+c.enc.NumOffset, top)
				c.a.Load(top, rb, rd)
				c.a.Store(calleeCtx, abi.OffThis+c.enc.RefOffset, top)
			} else {
				c.movImm(top, c.enc.Undefined)
				c.a.Store(calleeCtx, abi.OffThis+c.enc.NumOffset, top)
				c.a.Store(calleeCtx, abi.OffThis+c.enc.RefOffset, arm64.ZR)
			}
			c.a.B(thisDone)
			c.a.Bind(arrow)
			c.a.Load(tmp, a64Ctx, abi.OffCallThis)
			c.a.Load(top, tmp, c.enc.NumOffset)
			c.a.Store(calleeCtx, abi.OffThis+c.enc.NumOffset, top)
			c.a.Load(top, tmp, c.enc.RefOffset)
			c.a.Store(calleeCtx, abi.OffThis+c.enc.RefOffset, top)
			c.a.Bind(thisDone)
			c.a.Load(a64CallClosure, a64Ctx, abi.OffCallClosure)
			c.a.Adr(a64CallRet, back)
		}
		none := c.a.NewLabel()
		c.a.Load(a64CallLink, a64Ctx, abi.OffCallLink)
		c.a.Load(a64D, a64CallLink, c.enc.LinkEntry)
		c.a.Cbz(a64D, none, true)
		setupGen()
		c.a.Br(a64D)
		c.cold = append(c.cold, func() {
			c.a.Bind(none)
			setupGen()
			c.a.Load(a64CallObject, a64Ctx, abi.OffCallCallee)
			enter()
		})
	}
	// As amd64's: the callee returned, its result's words in a64RetNum and
	// a64RetRef.
	c.a.Bind(back)
	at := abi.OffKeep + int32(v.Index)*vs
	if site.Pool != 0 || site.Super {
		// As amd64's: a result that is not an object is the receiver.
		object := c.a.NewLabel()
		c.movImm(a64B, c.enc.Object)
		c.a.Cmp(a64RetNum, a64B, true)
		c.a.BCond(arm64.EQ, object)
		c.a.MovRR(a64RetNum, a64B)
		c.a.Load(a64RetRef, a64Ctx, abi.OffNext)
		c.a.Load(a64RetRef, a64RetRef, abi.OffThis+c.enc.RefOffset)
		c.a.Bind(object)
	}
	// As amd64's: while the collector marks, the result's pointer word with
	// Go's write barrier.
	marked, stored := c.a.NewLabel(), c.a.NewLabel()
	c.movImm(a64B, c.enc.WriteBarrier)
	c.a.LoadU8(a64B, a64B, 0)
	c.a.Cbnz(a64B, marked, false)
	if site.Super {
		// As amd64's: the this it binds, the context's This.
		c.a.Store(a64Ctx, abi.OffThis+c.enc.RefOffset, a64RetRef)
		c.a.Store(a64Ctx, abi.OffThis+c.enc.NumOffset, a64RetNum)
	}
	c.a.Store(a64Ctx, at+c.enc.RefOffset, a64RetRef)
	c.a.Store(a64Ctx, at+c.enc.NumOffset, a64RetNum)
	c.a.Bind(stored)
	c.cold = append(c.cold, func() {
		c.a.Bind(marked)
		c.a.Store(a64Ctx, at+c.enc.NumOffset, a64RetNum)
		c.a.AddImm(a64A, a64Ctx, int64(at+c.enc.RefOffset), true)
		c.refStore()
		if site.Super {
			c.a.Load(a64A, a64Ctx, at+c.enc.NumOffset)
			c.a.Store(a64Ctx, abi.OffThis+c.enc.NumOffset, a64A)
			c.a.Load(a64C, a64Ctx, at+c.enc.RefOffset)
			c.a.AddImm(a64A, a64Ctx, int64(abi.OffThis+c.enc.RefOffset), true)
			c.refStore()
		}
		c.a.Load(a64RetNum, a64Ctx, at+c.enc.NumOffset)
		c.a.B(stored)
	})
	for _, sv := range c.savesOf(v) {
		if sv.float {
			c.a.LoadF(arm64.FReg(sv.reg), a64Ctx, c.spillDisp(sv.slot))
		} else {
			c.a.Load(arm64.Reg(sv.reg), a64Ctx, c.spillDisp(sv.slot))
		}
	}
	c.setG(v, a64RetNum)
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
	c.protoGuards(t.Protos, guard)
}

// protoGuards checks an array's prototypes are as they were, as amd64's
// does. It uses A, B and C.
func (c *a64Compiler) protoGuards(protos [2]ssa.Holder, guard func(arm64.Cond)) {
	for i, h := range protos {
		c.movImm(a64A, uint64(h.Object))
		c.a.Load(a64B, a64A, c.enc.ObjectShape)
		c.movImm(a64C, uint64(h.Shape))
		c.a.Cmp(a64B, a64C, true)
		guard(arm64.NE)
		// No elements: no side fields, or none there. It need not be an
		// array.
		none := c.a.NewLabel()
		c.a.Load(a64B, a64A, c.enc.ObjectExt)
		c.a.Cbz(a64B, none, true)
		c.a.Load(a64B, a64B, c.enc.ExtElems+8)
		c.a.CmpImm(a64B, 0, true)
		guard(arm64.NE)
		c.a.Bind(none)
		c.a.LoadU8(a64B, a64A, c.enc.ObjectFlags)
		c.movImm(a64C, uint64(c.enc.FlagSparse))
		c.a.Tst(a64B, a64C, false)
		guard(arm64.NE)
		c.a.Load(a64B, a64A, c.enc.ObjectProto)
		if i == 0 {
			c.movImm(a64C, uint64(protos[1].Object))
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
	// As amd64's: while the collector marks, the element's pointer word
	// cleared with Go's write barrier, which shades the value popped.
	marked, done := c.a.NewLabel(), c.a.NewLabel()
	c.movImm(a64C, c.enc.WriteBarrier)
	c.a.LoadU8(a64C, a64C, 0)
	c.a.Cbnz(a64C, marked, false)
	c.a.Store(a64A, c.enc.RefOffset, arm64.ZR)
	c.a.Bind(done)
	c.cold = append(c.cold, func() {
		c.a.Bind(marked)
		c.a.AddImm(a64A, a64A, int64(c.enc.RefOffset), true)
		c.movImm(a64C, 0)
		c.refStore()
		c.a.Load(a64B, a64Ctx, at+c.enc.NumOffset)
		c.a.B(done)
	})
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
	c.a.FMovToF(a64F2, a64A)
	c.a.Scvtf(a64F0, a64B, true)
	c.a.FMovFromF(a64A, a64F0)
	at := abi.OffKeep + int32(v.Index)*vs
	c.a.Store(a64Ctx, at+c.enc.NumOffset, a64A)
	c.a.Store(a64Ctx, at+c.enc.RefOffset, arm64.ZR)
	// As amd64's: while the collector marks, the element's pointer word
	// stored again with Go's write barrier.
	marked, done := c.a.NewLabel(), c.a.NewLabel()
	c.movImm(a64B, c.enc.WriteBarrier)
	c.a.LoadU8(a64B, a64B, 0)
	c.a.Cbnz(a64B, marked, false)
	c.a.Bind(done)
	c.cold = append(c.cold, func() {
		c.a.Bind(marked)
		c.a.FMovFromF(a64A, a64F2)
		c.a.AddImm(a64A, a64A, int64(c.enc.RefOffset), true)
		c.a.FMovFromF(a64C, a64F1)
		c.refStore()
		c.a.Load(a64A, a64Ctx, at+c.enc.NumOffset)
		c.a.B(done)
	})
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

// lengthGuards checks a `new Array(n)` can take its array from its pool,
// as amd64's does: n, x, an integer in [0, the pool's Room], or up to
// abi.PoolRoomLimit once Go has filled the pool again with arrays of more
// room. It uses A, B, C, F0 and F1.
func (c *a64Compiler) lengthGuards(t *ssa.CallSite, x *ssa.Value, guard func(arm64.Cond)) {
	var w arm64.Reg
	if remat(x) {
		c.materialize(x, a64A)
		w = a64A
	} else {
		w = c.gpr(x, a64A)
	}
	if x.Shadow != nil || c.origin.At(x) >= 0 {
		c.movImm(a64B, abi.NumberLimit)
		c.a.Cmp(w, a64B, true)
		guard(arm64.HS)
	}
	c.a.FMovToF(a64F0, w)
	c.a.Fcvtzs(a64A, a64F0)
	c.a.Scvtf(a64F1, a64A, true)
	c.a.FCmp(a64F1, a64F0)
	guard(arm64.NE)
	c.movImm(a64B, uint64(t.Pool))
	c.a.Load(a64C, a64B, abi.OffPoolRoom)
	c.a.Cmp(a64A, a64C, true)
	fits := c.a.NewLabel()
	c.a.BCond(arm64.LS, fits)
	// Past its room: a negative n, unsigned, is past every room.
	c.a.CmpImm(a64A, abi.PoolRoomLimit, true)
	guard(arm64.HI)
	v, top := c.callV, c.callTop
	if v == nil {
		// No call to make again: Go makes it. n is at most the limit, as
		// the flags still say.
		guard(arm64.LS)
	} else {
		grow := c.a.NewLabel()
		c.a.B(grow)
		pool := t.Pool
		stub := c.stubLabel(v.State, exitKind(v.Aux))
		c.cold = append(c.cold, func() {
			c.a.Bind(grow)
			c.a.Store(a64B, abi.OffPoolWant, a64A)
			c.movImm(a64A, uint64(pool))
			c.a.Store(a64Ctx, abi.OffGoArgs+c.enc.RefOffset, a64A)
			c.callGo(v, uint64(ssa.GoRefill), 0)
			c.a.Load(a64B, a64Ctx, abi.OffGoStatus)
			c.a.Cbnz(a64B, stub, true)
			c.a.B(top)
		})
	}
	c.a.Bind(fits)
}

// allocLength makes the length of `new Array(n)`'s array, call v's
// result, n, as amd64's does. It uses A, B, C and F0.
func (c *a64Compiler) allocLength(v, x *ssa.Value) {
	var w arm64.Reg
	if remat(x) {
		c.materialize(x, a64A)
		w = a64A
	} else {
		w = c.gpr(x, a64A)
	}
	c.a.FMovToF(a64F0, w)
	c.a.Fcvtzs(a64B, a64F0)
	c.a.Load(a64C, a64Ctx, abi.OffKeep+int32(v.Index)*int32(c.enc.ValueSize)+c.enc.RefOffset)
	c.a.Store(a64C, c.enc.ObjectElems+8, a64B)
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
	c.a.Load(a64B, a64C, c.enc.ObjectShape)
	c.a.Load(a64B, a64B, c.enc.ShapeKeys)
	c.a.LoadU32(a64B, a64B, int32(t.ProtoIndex)*c.enc.KeySize+c.enc.PropertyKey)
	c.movImm(a64A, uint64(t.ProtoKey))
	c.a.Cmp(a64B, a64A, false)
	guard(arm64.NE)
	c.a.Load(a64B, a64C, c.enc.ObjectProps)
	c.a.Load(a64C, a64B, int32(t.ProtoIndex)*c.enc.ValueSize+c.enc.RefOffset)
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
	if site.Go == ssa.GoKeyedWrite && len(v.Args) == 3 && c.enc.ValueSize == 16 && c.enc.WriteBarrier != 0 {
		slow, join := c.a.NewLabel(), c.a.NewLabel()
		c.keyedStore(v, slow)
		c.a.B(join)
		c.a.Bind(slow)
		c.goCallOp(v, goOpWord(site), false)
		c.a.Bind(join)
		c.a.Load(a64A, a64Ctx, abi.OffKeep+int32(v.Index)*int32(c.enc.ValueSize)+c.enc.NumOffset)
		c.setG(v, a64A)
		return
	}
	if lengthWrites(site, len(v.Args), c.enc) {
		slow, join := c.a.NewLabel(), c.a.NewLabel()
		c.lengthWrite(v, slow)
		c.a.B(join)
		c.a.Bind(slow)
		c.goCallOp(v, goOpWord(site), false)
		c.a.Bind(join)
		c.a.Load(a64A, a64Ctx, abi.OffKeep+int32(v.Index)*int32(c.enc.ValueSize)+c.enc.NumOffset)
		c.setG(v, a64A)
		return
	}
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
	if site.Go == ssa.GoClosure && site.Pool != 0 {
		// As amd64's: natively from its pool where it can be, by Go
		// otherwise.
		slow, join := c.a.NewLabel(), c.a.NewLabel()
		c.closureTake(v, site, slow)
		c.a.B(join)
		c.a.Bind(slow)
		c.movImm(a64A, uint64(site.Pool))
		c.a.Store(a64Ctx, abi.OffGoArgs+c.enc.RefOffset, a64A)
		c.goCallOp(v, goOpWord(site), false)
		c.a.Bind(join)
		return
	}
	if site.Go == ssa.GoCells && site.Pool != 0 {
		// As amd64's: natively from the function's pool where it can be,
		// by Go otherwise.
		slow, join := c.a.NewLabel(), c.a.NewLabel()
		c.cellsTake(site, slow)
		c.a.Load(a64A, a64Ctx, abi.OffKeep+int32(v.Index)*int32(c.enc.ValueSize)+c.enc.NumOffset)
		c.setG(v, a64A)
		c.a.B(join)
		c.a.Bind(slow)
		c.movImm(a64A, uint64(site.Pool))
		c.a.Store(a64Ctx, abi.OffGoArgs+c.enc.RefOffset, a64A)
		c.goCallOp(v, goOpWord(site), false)
		c.a.Bind(join)
		return
	}
	if site.Go == ssa.GoClose && site.Pool != 0 {
		// As amd64's: natively from the function's pool of cells where it
		// can be, by Go otherwise.
		slow, join := c.a.NewLabel(), c.a.NewLabel()
		c.cellsClose(site, slow)
		c.a.B(join)
		c.a.Bind(slow)
		c.movImm(a64A, uint64(site.Pool))
		c.a.Store(a64Ctx, abi.OffGoArgs+c.enc.RefOffset, a64A)
		c.goCallOp(v, goOpWord(site), false)
		c.a.Bind(join)
		return
	}
	if site.Go == ssa.GoClose && !c.f.Cells {
		// As amd64's: only while closures have captured the frame's locals,
		// in code whose captured locals are in its frame.
		skip := c.a.NewLabel()
		c.a.Load(a64A, a64Ctx, abi.OffOpen)
		c.a.Cbz(a64A, skip, true)
		c.goCallOp(v, goOpWord(site), false)
		c.a.Bind(skip)
		return
	}
	if site.Big && c.enc.BigFast != 0 {
		// As amd64's: through the VM's routine for BigInts.
		c.goCallBig = true
		defer func() { c.goCallBig = false }()
	}
	c.goCallOp(v, goOpWord(site), false)
}

// closureTake is amd64's: a closure taken from its pool, its captured
// bindings' cells and an arrow's this written. It uses A, B and C.
// cellsTake is amd64's: an entry's cells from the function's pool, where
// the collector does not mark and the context is a native call's. It uses
// A, B and C.
func (c *a64Compiler) cellsTake(site *ssa.CallSite, slow arm64.Label) {
	marked, done := c.a.NewLabel(), c.a.NewLabel()
	c.movImm(a64A, c.enc.WriteBarrier)
	c.a.LoadU8(a64A, a64A, 0)
	c.a.Cbnz(a64A, marked, false)
	c.cold = append(c.cold, func() {
		c.a.Bind(marked)
		c.cellsTakeMarking(site, slow)
		c.a.B(done)
	})
	defer c.a.Bind(done)
	c.a.Load(a64A, a64Ctx, abi.OffLive)
	c.a.CmpImm(a64A, abi.LiveCall, true)
	c.a.BCond(arm64.NE, slow)
	c.movImm(a64A, uint64(site.Pool))
	c.a.Load(a64B, a64A, abi.OffPoolCount)
	c.a.Cbz(a64B, slow, true)
	c.a.AddImm(a64B, a64B, -1, true)
	c.a.Store(a64A, abi.OffPoolCount, a64B)
	c.a.ShiftImm(arm64.Lsl, a64B, a64B, 3, true)
	c.a.Op(arm64.Add, a64B, a64B, a64A, true)
	c.a.Load(a64C, a64B, abi.OffPoolObjects)
	c.a.Store(a64B, abi.OffPoolObjects, arm64.ZR)
	c.a.Store(a64Ctx, abi.OffCells, a64C)
	vs, n := c.enc.ValueSize, int32(len(site.Captures))
	for k, x := range site.Captures {
		at := 8*n + int32(k)*c.enc.UpvalueSize + c.enc.UpvalueClosed
		if x&1 == 0 {
			c.movImm(a64A, c.enc.Undefined)
			c.a.Store(a64C, at+c.enc.NumOffset, a64A)
			c.a.Store(a64C, at+c.enc.RefOffset, arm64.ZR)
			continue
		}
		slot := x >> 1
		c.a.Load(a64A, a64Locals, slot*vs+c.enc.NumOffset)
		c.a.Store(a64C, at+c.enc.NumOffset, a64A)
		c.a.Load(a64A, a64Locals, slot*vs+c.enc.RefOffset)
		c.a.Store(a64C, at+c.enc.RefOffset, a64A)
	}
}

// cellsClose is amd64's: a scope's end, each local from Argc on given its
// block's cell, holding what the old one holds, which is closed. It uses
// A, B, C and D.
func (c *a64Compiler) cellsClose(site *ssa.CallSite, slow arm64.Label) {
	marked, done := c.a.NewLabel(), c.a.NewLabel()
	c.movImm(a64A, c.enc.WriteBarrier)
	c.a.LoadU8(a64A, a64A, 0)
	c.a.Cbnz(a64A, marked, false)
	c.cold = append(c.cold, func() {
		c.a.Bind(marked)
		c.cellsCloseMarking(site, slow)
		c.a.B(done)
	})
	defer c.a.Bind(done)
	c.movImm(a64A, uint64(site.Pool))
	c.a.Load(a64B, a64A, abi.OffPoolCount)
	c.a.Cbz(a64B, slow, true)
	c.a.AddImm(a64B, a64B, -1, true)
	c.a.Store(a64A, abi.OffPoolCount, a64B)
	c.a.ShiftImm(arm64.Lsl, a64B, a64B, 3, true)
	c.a.Op(arm64.Add, a64B, a64B, a64A, true)
	c.a.Load(a64C, a64B, abi.OffPoolObjects)
	c.a.Store(a64B, abi.OffPoolObjects, arm64.ZR)
	n := int32(len(site.Captures))
	for k, x := range site.Captures {
		if int(x>>1) < site.Argc {
			continue
		}
		at := 8*n + int32(k)*c.enc.UpvalueSize
		c.a.Load(a64A, a64Ctx, abi.OffCells)
		c.a.Load(a64A, a64A, int32(k)*8)
		c.a.Load(a64B, a64A, c.enc.UpvalueSlot)
		for _, w := range []int32{c.enc.NumOffset, c.enc.RefOffset} {
			c.a.Load(a64D, a64B, w)
			c.a.Store(a64A, c.enc.UpvalueClosed+w, a64D)
			c.a.Store(a64C, at+c.enc.UpvalueClosed+w, a64D)
		}
		c.a.AddImm(a64B, a64A, int64(c.enc.UpvalueClosed), true)
		c.a.Store(a64A, c.enc.UpvalueSlot, a64B)
		c.a.AddImm(a64B, a64C, int64(at), true)
		c.a.Load(a64A, a64Ctx, abi.OffCells)
		c.a.Store(a64A, int32(k)*8, a64B)
	}
}

// cellsCloseMarking is amd64's: each pointer word written with Go's
// barrier, the block kept in the context's second GoArgs meanwhile.
func (c *a64Compiler) cellsCloseMarking(site *ssa.CallSite, slow arm64.Label) {
	block := abi.OffGoArgs + c.enc.ValueSize + c.enc.RefOffset
	c.movImm(a64A, uint64(site.Pool))
	c.a.Load(a64B, a64A, abi.OffPoolCount)
	c.a.Cbz(a64B, slow, true)
	c.a.AddImm(a64B, a64B, -1, true)
	c.a.Store(a64A, abi.OffPoolCount, a64B)
	c.a.ShiftImm(arm64.Lsl, a64B, a64B, 3, true)
	c.a.Op(arm64.Add, a64B, a64B, a64A, true)
	c.a.Load(a64C, a64B, abi.OffPoolObjects)
	c.a.Store(a64Ctx, block, a64C)
	c.a.AddImm(a64A, a64B, int64(abi.OffPoolObjects), true)
	c.movImm(a64C, 0)
	c.refStore()
	old := func(k int) {
		c.a.Load(a64A, a64Ctx, abi.OffCells)
		c.a.Load(a64A, a64A, int32(k)*8)
	}
	n := int32(len(site.Captures))
	for k, x := range site.Captures {
		if int(x>>1) < site.Argc {
			continue
		}
		at := 8*n + int32(k)*c.enc.UpvalueSize
		old(k)
		c.a.Load(a64B, a64A, c.enc.UpvalueSlot)
		c.a.Load(a64C, a64B, c.enc.NumOffset)
		c.a.Store(a64A, c.enc.UpvalueClosed+c.enc.NumOffset, a64C)
		c.a.Load(a64C, a64B, c.enc.RefOffset)
		c.a.AddImm(a64A, a64A, int64(c.enc.UpvalueClosed+c.enc.RefOffset), true)
		c.refStore()
		old(k)
		c.a.AddImm(a64C, a64A, int64(c.enc.UpvalueClosed), true)
		c.a.AddImm(a64A, a64A, int64(c.enc.UpvalueSlot), true)
		c.refStore()
		old(k)
		c.a.Load(a64B, a64Ctx, block)
		c.a.Load(a64C, a64A, c.enc.UpvalueClosed+c.enc.NumOffset)
		c.a.Store(a64B, at+c.enc.UpvalueClosed+c.enc.NumOffset, a64C)
		c.a.Load(a64C, a64A, c.enc.UpvalueClosed+c.enc.RefOffset)
		c.a.AddImm(a64A, a64B, int64(at+c.enc.UpvalueClosed+c.enc.RefOffset), true)
		c.refStore()
		c.a.Load(a64B, a64Ctx, block)
		c.a.AddImm(a64C, a64B, int64(at), true)
		c.a.Load(a64A, a64Ctx, abi.OffCells)
		c.a.AddImm(a64A, a64A, int64(k)*8, true)
		c.refStore()
	}
	c.a.Store(a64Ctx, block, arm64.ZR)
}

// cellsTakeMarking is amd64's: the pool's cell cleared, each cell's
// argument's pointer word written, with Go's barrier.
func (c *a64Compiler) cellsTakeMarking(site *ssa.CallSite, slow arm64.Label) {
	c.a.Load(a64A, a64Ctx, abi.OffLive)
	c.a.CmpImm(a64A, abi.LiveCall, true)
	c.a.BCond(arm64.NE, slow)
	c.movImm(a64A, uint64(site.Pool))
	c.a.Load(a64B, a64A, abi.OffPoolCount)
	c.a.Cbz(a64B, slow, true)
	c.a.AddImm(a64B, a64B, -1, true)
	c.a.Store(a64A, abi.OffPoolCount, a64B)
	c.a.ShiftImm(arm64.Lsl, a64B, a64B, 3, true)
	c.a.Op(arm64.Add, a64B, a64B, a64A, true)
	c.a.Load(a64C, a64B, abi.OffPoolObjects)
	c.a.Store(a64Ctx, abi.OffCells, a64C)
	c.a.AddImm(a64A, a64B, int64(abi.OffPoolObjects), true)
	c.movImm(a64C, 0)
	c.refStore()
	vs, n := c.enc.ValueSize, int32(len(site.Captures))
	for k, x := range site.Captures {
		at := 8*n + int32(k)*c.enc.UpvalueSize + c.enc.UpvalueClosed
		c.a.Load(a64B, a64Ctx, abi.OffCells)
		if x&1 == 0 {
			c.movImm(a64A, c.enc.Undefined)
			c.a.Store(a64B, at+c.enc.NumOffset, a64A)
			c.a.Store(a64B, at+c.enc.RefOffset, arm64.ZR)
			continue
		}
		slot := x >> 1
		c.a.Load(a64A, a64Locals, slot*vs+c.enc.NumOffset)
		c.a.Store(a64B, at+c.enc.NumOffset, a64A)
		c.a.AddImm(a64A, a64B, int64(at+c.enc.RefOffset), true)
		c.a.Load(a64C, a64Locals, slot*vs+c.enc.RefOffset)
		c.refStore()
	}
}

// closureTakeMarking is amd64's: the pool's cell cleared, each cell and an
// arrow's this written into what the closure keeps, with Go's barrier.
func (c *a64Compiler) closureTakeMarking(v *ssa.Value, site *ssa.CallSite, slow arm64.Label) {
	at := abi.OffKeep + int32(v.Index)*int32(c.enc.ValueSize)
	c.movImm(a64A, uint64(site.Pool))
	c.a.Load(a64B, a64A, abi.OffPoolCount)
	c.a.Cbz(a64B, slow, true)
	c.poolTakeMarking(v, site)
	data := func() {
		c.a.Load(a64C, a64Ctx, at+c.enc.RefOffset)
		c.a.Load(a64B, a64C, c.enc.ObjectExt)
		c.a.Load(a64C, a64B, c.enc.ObjectData+8)
	}
	for k, x := range site.Captures {
		base := abi.OffUpvalues
		if x&1 != 0 {
			base = abi.OffCells
		}
		data()
		c.a.Load(a64B, a64C, c.enc.FuncClosure)
		c.a.Load(a64B, a64B, c.enc.ClosureUpvalues)
		c.a.AddImm(a64A, a64B, int64(k)*8, true)
		c.a.Load(a64C, a64Ctx, base)
		c.a.Load(a64C, a64C, (x>>1)*8)
		c.refStore()
	}
	if site.LexThis {
		data()
		c.a.Load(a64B, a64C, c.enc.FuncExtra)
		c.a.Load(a64A, a64Ctx, abi.OffThis+c.enc.NumOffset)
		c.a.Store(a64B, c.enc.ExtraLexThis+c.enc.NumOffset, a64A)
		c.a.AddImm(a64A, a64B, int64(c.enc.ExtraLexThis+c.enc.RefOffset), true)
		c.a.Load(a64C, a64Ctx, abi.OffThis+c.enc.RefOffset)
		c.refStore()
	}
	c.movImm(a64A, c.enc.Object)
	c.setG(v, a64A)
}

func (c *a64Compiler) closureTake(v *ssa.Value, site *ssa.CallSite, slow arm64.Label) {
	marked, done := c.a.NewLabel(), c.a.NewLabel()
	c.movImm(a64A, c.enc.WriteBarrier)
	c.a.LoadU8(a64A, a64A, 0)
	c.a.Cbnz(a64A, marked, false)
	c.cold = append(c.cold, func() {
		c.a.Bind(marked)
		c.closureTakeMarking(v, site, slow)
		c.a.B(done)
	})
	defer c.a.Bind(done)
	c.movImm(a64A, uint64(site.Pool))
	c.a.Load(a64B, a64A, abi.OffPoolCount)
	c.a.Cbz(a64B, slow, true)
	c.poolTake(v, site)
	c.a.Load(a64B, a64C, c.enc.ObjectExt)
	c.a.Load(a64C, a64B, c.enc.ObjectData+8)
	c.a.Load(a64B, a64C, c.enc.FuncClosure)
	c.a.Load(a64B, a64B, c.enc.ClosureUpvalues)
	for k, x := range site.Captures {
		base := abi.OffUpvalues
		if x&1 != 0 {
			base = abi.OffCells
		}
		c.a.Load(a64A, a64Ctx, base)
		c.a.Load(a64A, a64A, (x>>1)*8)
		c.a.Store(a64B, int32(k)*8, a64A)
	}
	if site.LexThis {
		c.a.Load(a64B, a64C, c.enc.FuncExtra)
		for w := int32(0); w < c.enc.ValueSize; w += 8 {
			c.a.Load(a64A, a64Ctx, abi.OffThis+w)
			c.a.Store(a64B, c.enc.ExtraLexThis+w, a64A)
		}
	}
}

// iterStep is amd64's: a loop's step over an array, made in place. It
// uses A, B, C and D, and no other.
func (c *a64Compiler) iterStep(v *ssa.Value, slow arm64.Label) {
	x := v.Args[0]
	c.pointerWord(x, c.gpr(x, a64A))
	c.a.Cbz(a64B, slow, true)
	c.objectData(a64D, a64B, a64C, c.enc.IterState, func(cond arm64.Cond) { c.a.BCond(cond, slow) })
	c.a.LoadU8(a64C, a64D, c.enc.IterDone)
	c.a.Cbnz(a64C, slow, false)
	c.a.Load(a64A, a64D, c.enc.IterIdx)
	c.a.Load(a64C, a64D, c.enc.IterArr)
	// No array: a Map's or a Set's iterator, which mapStep steps.
	nexts := stepNexts(v, c.enc)
	maps := slow
	if nexts != ([2]uintptr{}) {
		maps = c.a.NewLabel()
		done := c.a.NewLabel()
		defer c.a.Bind(done)
		c.cold = append(c.cold, func() {
			c.a.Bind(maps)
			c.mapStep(v, nexts, slow)
			c.a.B(done)
		})
	}
	c.a.Cbz(a64C, maps, true)
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

// keyedStore is amd64's: an array's dense element stored natively at a
// site whose stores Go makes for other receivers. It uses A, B, C, D, F0,
// F1 and F2, and no other.
func (c *a64Compiler) keyedStore(v *ssa.Value, slow arm64.Label) {
	fail := func(cond arm64.Cond) { c.a.BCond(cond, slow) }
	recv, key, x := v.Args[0], v.Args[1], v.Args[2]
	w := c.gpr(x, a64A)
	c.a.FMovToF(a64F0, w)
	c.pointerWord(x, w)
	c.a.FMovToF(a64F1, a64B)
	if w := c.gpr(key, a64A); w != a64A {
		c.a.MovRR(a64A, w)
	}
	c.movImm(a64B, abi.NumberLimit)
	c.a.Cmp(a64A, a64B, true)
	fail(arm64.HS)
	c.a.FMovToF(a64F2, a64A)
	c.a.Fcvtzs(a64B, a64F2)
	c.a.Scvtf(a64F2, a64B, true)
	c.a.FMovFromF(a64C, a64F2)
	c.a.Cmp(a64C, a64A, true)
	fail(arm64.NE)
	c.a.FMovToF(a64F2, a64B)
	w = c.gpr(recv, a64A)
	c.movImm(a64C, c.enc.Object)
	c.a.Cmp(w, a64C, true)
	fail(arm64.NE)
	c.pointerWord(recv, w)
	c.a.Cbz(a64B, slow, true)
	c.a.LoadU8(a64C, a64B, c.enc.ObjectClass)
	c.a.CmpImm(a64C, int64(c.enc.ClassArray), false)
	fail(arm64.NE)
	c.movImm(a64C, c.enc.WriteBarrier)
	c.a.LoadU8(a64C, a64C, 0)
	c.a.Cbnz(a64C, slow, false)
	c.a.FMovFromF(a64A, a64F2)
	c.a.Load(a64C, a64B, c.enc.ObjectElems+8)
	c.a.Cmp(a64A, a64C, true)
	fail(arm64.HS)
	c.a.Load(a64B, a64B, c.enc.ObjectElems)
	c.a.ShiftImm(arm64.Lsl, a64A, a64A, 4, true)
	c.a.Op(arm64.Add, a64A, a64A, a64B, true)
	c.a.Load(a64C, a64A, c.enc.NumOffset)
	c.movImm(a64D, c.enc.Uninitialized)
	c.a.Cmp(a64C, a64D, true)
	fail(arm64.EQ)
	c.a.FMovFromF(a64C, a64F0)
	c.a.Store(a64A, c.enc.NumOffset, a64C)
	c.a.FMovFromF(a64C, a64F1)
	c.a.Store(a64A, c.enc.RefOffset, a64C)
	keep := abi.OffKeep + int32(v.Index)*int32(c.enc.ValueSize)
	c.movImm(a64C, c.enc.Undefined)
	c.a.Store(a64Ctx, keep+c.enc.NumOffset, a64C)
	c.movImm(a64C, 0)
	c.a.Store(a64Ctx, keep+c.enc.RefOffset, a64C)
}

// lengthWrite is amd64's: an array's length written natively where it
// shortens a dense array or leaves it as it is. It uses A, B, C, D, F1 and
// F2, and no other.
func (c *a64Compiler) lengthWrite(v *ssa.Value, slow arm64.Label) {
	fail := func(cond arm64.Cond) { c.a.BCond(cond, slow) }
	recv, x := v.Args[0], v.Args[1]
	if w := c.gpr(x, a64A); w != a64A {
		c.a.MovRR(a64A, w)
	}
	c.movImm(a64B, abi.NumberLimit)
	c.a.Cmp(a64A, a64B, true)
	fail(arm64.HS)
	c.a.FMovToF(a64F1, a64A)
	c.a.Fcvtzs(a64B, a64F1)
	c.a.Scvtf(a64F1, a64B, true)
	c.a.FMovFromF(a64C, a64F1)
	c.a.Cmp(a64C, a64A, true)
	fail(arm64.NE)
	c.a.FMovToF(a64F2, a64B)
	w := c.gpr(recv, a64A)
	c.movImm(a64C, c.enc.Object)
	c.a.Cmp(w, a64C, true)
	fail(arm64.NE)
	c.pointerWord(recv, w)
	c.a.Cbz(a64B, slow, true)
	c.a.LoadU8(a64C, a64B, c.enc.ObjectClass)
	c.a.CmpImm(a64C, int64(c.enc.ClassArray), false)
	fail(arm64.NE)
	c.a.LoadU8(a64C, a64B, c.enc.ObjectFlags)
	c.movImm(a64D, uint64(c.enc.FlagLengthWritable|c.enc.FlagSparse))
	c.a.Op(arm64.And, a64C, a64C, a64D, false)
	c.a.CmpImm(a64C, int64(c.enc.FlagLengthWritable), false)
	fail(arm64.NE)
	c.a.Load(a64C, a64B, c.enc.ObjectProps+8)
	c.a.Cbnz(a64C, slow, true)
	c.movImm(a64C, c.enc.WriteBarrier)
	c.a.LoadU8(a64C, a64C, 0)
	c.a.Cbnz(a64C, slow, false)
	c.a.FMovFromF(a64A, a64F2)
	c.a.Load(a64C, a64B, c.enc.ObjectElems+8)
	c.a.Cmp(a64A, a64C, true)
	fail(arm64.HI)
	c.a.Store(a64B, c.enc.ObjectElems+8, a64A)
	c.a.Load(a64B, a64B, c.enc.ObjectElems)
	c.a.ShiftImm(arm64.Lsl, a64A, a64A, 4, true)
	c.a.ShiftImm(arm64.Lsl, a64C, a64C, 4, true)
	c.a.Op(arm64.Add, a64A, a64A, a64B, true)
	c.a.Op(arm64.Add, a64C, a64C, a64B, true)
	c.movImm(a64D, 0)
	loop, cleared := c.a.NewLabel(), c.a.NewLabel()
	c.a.Bind(loop)
	c.a.Cmp(a64A, a64C, true)
	c.a.BCond(arm64.HS, cleared)
	for at := int32(0); at < c.enc.ValueSize; at += 8 {
		c.a.Store(a64A, at, a64D)
	}
	c.a.AddImm(a64A, a64A, int64(c.enc.ValueSize), true)
	c.a.B(loop)
	c.a.Bind(cleared)
	keep := abi.OffKeep + int32(v.Index)*int32(c.enc.ValueSize)
	c.movImm(a64C, c.enc.Undefined)
	c.a.Store(a64Ctx, keep+c.enc.NumOffset, a64C)
	c.a.Store(a64Ctx, keep+c.enc.RefOffset, a64D)
}

// mapStep is amd64's: a for...of's step of a Map's or a Set's iterator,
// the cursor's state in D. It uses A, B, C, D and F1, and no other.
func (c *a64Compiler) mapStep(v *ssa.Value, nexts [2]uintptr, slow arm64.Label) {
	fail := func(cond arm64.Cond) { c.a.BCond(cond, slow) }
	have := c.a.NewLabel()
	c.a.Load(a64B, a64D, c.enc.IterNext+c.enc.RefOffset)
	for _, n := range nexts {
		if n != 0 {
			c.movImm(a64C, uint64(n))
			c.a.Cmp(a64B, a64C, true)
			c.a.BCond(arm64.EQ, have)
		}
	}
	c.a.B(slow)
	c.a.Bind(have)
	c.a.Load(a64C, a64D, c.enc.IterIter+c.enc.NumOffset)
	c.movImm(a64B, c.enc.Object)
	c.a.Cmp(a64C, a64B, true)
	fail(arm64.NE)
	c.a.Load(a64B, a64D, c.enc.IterIter+c.enc.RefOffset)
	c.objectData(a64D, a64B, a64C, c.enc.MapIterData, fail)
	c.a.LoadU8(a64C, a64D, c.enc.MapIterDone)
	c.a.Cbnz(a64C, slow, false)
	kind := c.a.NewLabel()
	c.a.LoadU8(a64C, a64D, c.enc.MapIterKind)
	c.a.CmpImm(a64C, int64(c.enc.MapKeys), false)
	c.a.BCond(arm64.EQ, kind)
	c.a.CmpImm(a64C, int64(c.enc.MapValues), false)
	fail(arm64.NE)
	c.a.Bind(kind)
	c.a.Load(a64A, a64D, c.enc.MapIterMap)
	c.a.Cbz(a64A, slow, true)
	c.a.LoadU32(a64B, a64D, c.enc.MapIterGen)
	c.a.LoadU32(a64C, a64A, c.enc.MapGen)
	c.a.Cmp(a64B, a64C, false)
	fail(arm64.NE)
	c.a.Load(a64B, a64D, c.enc.MapIterAt)
	c.a.Load(a64C, a64A, c.enc.MapEntries+8)
	c.a.Cmp(a64B, a64C, true)
	fail(arm64.HS)
	c.a.Load(a64A, a64A, c.enc.MapEntries)
	c.movImm(a64C, uint64(c.enc.MapEntrySize))
	c.a.Mul(a64C, a64C, a64B)
	c.a.Op(arm64.Add, a64A, a64A, a64C, true)
	c.a.FMovToF(a64F1, a64B)
	c.a.Load(a64C, a64A, c.enc.NumOffset)
	c.movImm(a64B, c.enc.MapDeleted)
	c.a.Cmp(a64C, a64B, true)
	fail(arm64.EQ)
	keep := abi.OffKeep + int32(v.Index)*int32(c.enc.ValueSize)
	values, stored := c.a.NewLabel(), c.a.NewLabel()
	c.a.LoadU8(a64C, a64D, c.enc.MapIterKind)
	c.a.CmpImm(a64C, int64(c.enc.MapKeys), false)
	c.a.BCond(arm64.NE, values)
	for i, at := range [2]int32{0, c.enc.MapEntryValue} {
		if i == 1 {
			c.a.B(stored)
			c.a.Bind(values)
		}
		c.a.Load(a64C, a64A, at+c.enc.NumOffset)
		c.movImm(a64B, c.enc.Uninitialized)
		c.a.Cmp(a64C, a64B, true)
		fail(arm64.EQ)
		c.a.Store(a64Ctx, keep+c.enc.NumOffset, a64C)
		c.a.Load(a64C, a64A, at+c.enc.RefOffset)
		c.a.Store(a64Ctx, keep+c.enc.RefOffset, a64C)
	}
	c.a.Bind(stored)
	c.a.FMovFromF(a64B, a64F1)
	c.a.AddImm(a64B, a64B, 1, true)
	c.a.Store(a64D, c.enc.MapIterAt, a64B)
	c.a.Load(a64B, a64A, c.enc.MapEntrySeq)
	c.a.Store(a64D, c.enc.MapIterLast, a64B)
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
// place (tailJump). While the collector marks, each operand's pointer is
// shaded first.
func (c *a64Compiler) tailSelf(v *ssa.Value, guard func(arm64.Cond)) {
	shallow := c.a.NewLabel()
	c.a.Load(a64A, a64Ctx, abi.OffLevel)
	c.a.Load(a64B, a64Ctx, abi.OffBounds)
	c.a.Load(a64B, a64B, abi.OffBoundsTailLevel)
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
	marked, shaded := c.a.NewLabel(), c.a.NewLabel()
	c.movImm(a64B, c.enc.WriteBarrier)
	c.a.LoadU8(a64B, a64B, 0)
	c.a.Cbnz(a64B, marked, false)
	c.a.Bind(shaded)
	staged := len(args)
	if this != nil {
		staged++
	}
	c.cold = append(c.cold, func() {
		c.a.Bind(marked)
		for i := range staged {
			c.a.AddImm(a64A, a64Ctx, int64(abi.OffShade), true)
			c.a.Load(a64C, a64Ctx, abi.OffGoArgs+int32(i)*size+c.enc.RefOffset)
			c.refStore()
		}
		c.a.B(shaded)
	})
	other := c.a.NewLabel()
	c.a.Load(a64C, a64Ctx, abi.OffCallCallee)
	c.objectData(a64B, a64C, a64C, c.enc.FuncData, guard)
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
	c.a.Store(a64Ctx, abi.OffCells, arm64.ZR)
	if c.f.Loops {
		c.a.Store(a64Ctx, abi.OffLoop, arm64.ZR)
	}
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
	c.movImm(a64A, uint64(t.Entry)-uint64(c.enc.LinkEntry))
	c.a.Load(a64A, a64A, c.enc.LinkBody)
	c.a.Br(a64A)
}

func (c *a64Compiler) goCallOp(v *ssa.Value, op uint64, saving bool) {
	size := int32(unsafe.Sizeof(abi.GoArg{}))
	// As amd64's: its operands written, the rest described.
	st, _ := c.goState(op, v.Index, v, nil)
	for i, x := range v.Args {
		c.goArg(abi.OffGoArgs+int32(i)*size, x)
	}
	stub := c.stubLabel(v.State, exitKind(v.Aux))
	if saving {
		c.goCalling(st, &stub)
	} else {
		c.goCallingLive(v, st, stub)
	}
	c.a.Load(a64A, a64Ctx, abi.OffKeep+int32(v.Index)*int32(c.enc.ValueSize)+c.enc.NumOffset)
	c.setG(v, a64A)
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

// markingTake is amd64's: while the collector marks, the take with Go's
// write barrier (poolTakeMarking, arrayElements); native code goes on at
// the label returned.
func (c *a64Compiler) markingTake(v *ssa.Value, site *ssa.CallSite) arm64.Label {
	c.movImm(a64B, c.enc.WriteBarrier)
	c.a.LoadU8(a64B, a64B, 0)
	marked, join := c.a.NewLabel(), c.a.NewLabel()
	c.a.Cbnz(a64B, marked, false)
	c.cold = append(c.cold, func() {
		c.a.Bind(marked)
		c.poolTakeMarking(v, site)
		if site.Literal && site.Argc != 0 {
			c.arrayElements(v, true)
		}
		c.a.B(join)
	})
	return join
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
		c.valueStore()
		c.a.B(done)
	})
}

// bigUnbox is the BigInt a tagged value is, as amd64's.
func (c *a64Compiler) bigUnbox(v *ssa.Value, guard func(arm64.Cond)) {
	x := v.Args[0]
	low := 64 - c.enc.ShortBigLowBits
	c.a.ShiftImm(arm64.Lsr, a64A, c.gpr(x, a64B), c.enc.ShortBigLowBits, true)
	c.movImm(a64B, c.enc.ShortBigTag>>c.enc.ShortBigLowBits)
	c.a.Cmp(a64A, a64B, true)
	guard(arm64.NE)
	c.keepSource(x)
	c.a.CmpImm(a64C, 0, true)
	guard(arm64.EQ)
	c.a.Load(a64A, a64C, c.enc.RefOffset)
	c.movImm(a64B, c.enc.ShortBigHigh)
	c.a.Op(arm64.Sub, a64A, a64A, a64B, true)
	c.a.ShiftImm(arm64.Lsl, a64A, a64A, c.enc.ShortBigLowBits, true)
	c.a.ShiftImm(arm64.Lsl, a64C, c.gpr(x, a64C), low, true)
	c.a.ShiftImm(arm64.Lsr, a64C, a64C, low, true)
	c.a.Op(arm64.Orr, a64A, a64A, a64C, true)
	c.setG(v, a64A)
}

// bigBox is a BigInt in the int64 range as a value, written to its keep
// cell, as amd64's.
func (c *a64Compiler) bigBox(v *ssa.Value) {
	viaGo, done := c.a.NewLabel(), c.a.NewLabel()
	at := abi.OffKeep + int32(v.Index)*int32(c.enc.ValueSize)
	c.bigBoxWords(v)
	c.a.FMovToF(a64F0, a64A)
	c.movImm(a64B, c.enc.WriteBarrier)
	c.a.LoadU8(a64B, a64B, 0)
	c.a.Cbnz(a64B, viaGo, false)
	c.a.Store(a64Ctx, at+c.enc.RefOffset, a64C)
	c.a.Store(a64Ctx, at+c.enc.NumOffset, a64A)
	c.a.Bind(done)
	c.setG(v, a64A)
	c.cold = append(c.cold, func() {
		c.a.Bind(viaGo)
		c.a.AddImm(a64A, a64Ctx, int64(at), true)
		c.valueStore()
		// The scratch registers are Go's across its write barrier.
		c.a.Load(a64A, a64Ctx, at+c.enc.NumOffset)
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
	// As amd64's: at a megamorphic site, a receiver of a shape not among
	// the cases has its own table searched, and so does one whose
	// prototypes are no longer the case's; at any other, either fails.
	wide := v.Megamorphic()
	done, scan, mega := c.a.NewLabel(), c.a.NewLabel(), arm64.Label(0)
	stale := scan
	if wide && c.enc.Mega && v.Op != ssa.OpPropWrite {
		// As amd64's: prototypes no longer the case's, the megamorphic
		// cache's chain.
		mega = c.a.NewLabel()
		stale = mega
		c.cold = append(c.cold, func() {
			c.a.Bind(mega)
			c.mega(v, guard, done)
		})
	}
	if wide {
		c.cold = append(c.cold, func() {
			c.a.Bind(scan)
			c.scan(v, guard, done, mega)
		})
	}
	miss := func(to arm64.Label) {
		if wide {
			c.a.BCond(arm64.NE, to)
		} else {
			guard(arm64.NE)
		}
	}
	for i, k := range cases {
		next := c.a.NewLabel()
		p := c.gpr(v.Args[0], a64A)
		c.a.Load(a64B, p, c.enc.ObjectShape)
		c.movImm(a64C, uint64(k.Shape))
		c.a.Cmp(a64B, a64C, true)
		if i == len(cases)-1 {
			miss(scan)
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
			miss(stale)
			c.a.Load(a64B, a64A, c.enc.ObjectShape)
			c.movImm(a64C, uint64(h.Shape))
			c.a.Cmp(a64B, a64C, true)
			miss(stale)
			c.a.Load(a64B, a64A, c.enc.ObjectProto)
		}
		c.a.Load(a64A, a64A, c.enc.ObjectProps)
		c.a.AddImm(a64A, a64A, int64(k.Index*c.enc.ValueSize), true)
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

// random is Math.random(), as amd64's is.
func (c *a64Compiler) random(v *ssa.Value) {
	fill, done := c.a.NewLabel(), c.a.NewLabel()
	c.movImm(a64C, v.Const.Bits)
	c.a.Load(a64A, a64C, c.enc.RandomCount)
	c.a.Cbz(a64A, fill, true)
	c.a.AddImm(a64A, a64A, -1, true)
	c.a.Store(a64C, c.enc.RandomCount, a64A)
	c.a.ShiftImm(arm64.Lsl, a64A, a64A, 3, true)
	c.a.Op(arm64.Add, a64A, a64A, a64C, true)
	c.a.LoadF(a64F0, a64A, c.enc.RandomNumbers)
	c.a.Bind(done)
	c.setF(v, a64F0)
	c.cold = append(c.cold, func() {
		c.a.Bind(fill)
		c.goSaving(uint64(ssa.GoRandom), nil)
		c.a.LoadF(a64F0, a64Ctx, abi.OffGoArgs+c.enc.NumOffset)
		c.a.B(done)
	})
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
	typed := c.a.NewLabel()
	c.a.BCond(arm64.NE, typed)
	c.cold = append(c.cold, func() {
		// A typed array's, as amd64's.
		c.a.Bind(typed)
		c.a.CmpImm(a64B, int64(c.enc.ClassTypedArray), false)
		guard(arm64.NE)
		c.objectData(a64C, a64C, a64B, c.enc.TypedData, guard)
		c.a.Load(a64B, a64C, c.enc.TypedStorage)
		c.a.CmpImm(a64B, 0, true)
		guard(arm64.EQ)
		c.a.Load(a64A, a64C, c.enc.TypedEnd)
		c.a.Load(a64D, a64B, c.enc.BufferBytes+8)
		c.a.Cmp(a64A, a64D, true)
		guard(arm64.HI)
		c.a.Load(a64C, a64C, c.enc.TypedLength)
		c.a.B(have)
	})
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
	if !c.stringUnit(v, v.Args[1], v.Args[2], guard) {
		return
	}
	c.a.Scvtf(a64F0, a64A, true)
	c.setF(v, a64F0)
}

// stringChar is s[i] (ssa.OpStringChar), as amd64's.
func (c *a64Compiler) stringChar(v *ssa.Value, guard func(arm64.Cond)) {
	if !c.stringUnit(v, v.Args[0], v.Args[1], guard) {
		return
	}
	c.charCell(v, guard)
}

// codeChar is String.fromCharCode(c) (ssa.OpCodeChar), as amd64's.
func (c *a64Compiler) codeChar(v *ssa.Value, guard func(arm64.Cond)) {
	c.index(v.Args[0], guard)
	c.charCell(v, guard)
}

// oddball is ssa.OpOddballF64, as amd64's. It uses A.
func (c *a64Compiler) oddball(v *ssa.Value, fail func(arm64.Cond)) {
	r := c.gpr(v.Args[0], a64B)
	d := c.fdst(v)
	done, tagged, zero := c.a.NewLabel(), c.a.NewLabel(), c.a.NewLabel()
	c.a.ShiftImm(arm64.Lsr, a64A, r, 51, true)
	c.a.CmpImm(a64A, 0x1FFF, false)
	c.a.BCond(arm64.EQ, tagged)
	c.a.FMovToF(d, r)
	c.a.B(done)
	c.a.Bind(tagged)
	for _, k := range []struct {
		word, bits uint64
	}{{c.enc.Undefined, 0x7FF8000000000000}, {c.enc.True, 0x3FF0000000000000}} {
		next := c.a.NewLabel()
		c.movImm(a64A, k.word)
		c.a.Cmp(r, a64A, true)
		c.a.BCond(arm64.NE, next)
		c.movImm(a64A, k.bits)
		c.a.FMovToF(d, a64A)
		c.a.B(done)
		c.a.Bind(next)
	}
	c.movImm(a64A, c.enc.Null)
	c.a.Cmp(r, a64A, true)
	c.a.BCond(arm64.EQ, zero)
	c.movImm(a64A, c.enc.False)
	c.a.Cmp(r, a64A, true)
	if v.Index == 1 && c.enc.CallGo != 0 && exitKind(v.Aux) == abi.ExitHost {
		// As amd64's: anything else converted by Go (ssa.GoNumber).
		slow := c.a.NewLabel()
		c.a.BCond(arm64.NE, slow)
		c.cold = append(c.cold, func() {
			c.a.Bind(slow)
			stub := c.stubLabel(v.State, exitKind(v.Aux))
			c.goArg(abi.OffGoArgs, v.Args[0])
			c.goSaving(uint64(ssa.GoNumber), &stub)
			c.a.LoadF(d, a64Ctx, abi.OffGoArgs+c.enc.NumOffset)
			c.a.B(done)
		})
	} else {
		fail(arm64.NE)
	}
	c.a.Bind(zero)
	c.movImm(a64A, 0)
	c.a.FMovToF(d, a64A)
	c.a.Bind(done)
	c.setF(v, d)
}

// freeze is ssa.OpFreeze, as amd64's. It uses A to C.
func (c *a64Compiler) freeze(v *ssa.Value) {
	x := v.Args[0]
	var w arm64.Reg
	if remat(x) {
		c.materialize(x, a64A)
		w = a64A
	} else {
		w = c.gpr(x, a64A)
	}
	done := c.a.NewLabel()
	c.movImm(a64B, c.enc.String)
	c.a.Cmp(w, a64B, true)
	c.a.BCond(arm64.NE, done)
	c.pointerWord(x, w)
	c.a.Cbz(a64B, done, true)
	c.movImm(a64C, 0)
	c.a.Store8(a64B, c.enc.StringBuilding, a64C)
	c.a.Bind(done)
}

// charCell is the cell of the runtime's string of the code unit in A,
// below 256, as amd64's.
func (c *a64Compiler) charCell(v *ssa.Value, guard func(arm64.Cond)) {
	c.a.CmpImm(a64A, 256, true)
	guard(arm64.HS)
	c.a.Load(a64B, a64Ctx, abi.OffCharStrings)
	c.a.Cbz(a64B, c.stubLabel(v.State, exitKind(v.Aux)), true)
	c.a.ShiftImm(arm64.Lsl, a64A, a64A, 4, true)
	c.a.Op(arm64.Add, a64A, a64A, a64B, true)
	c.setG(v, a64A)
}

// stringUnit reads the code unit at index key of str, a flat string, into
// A, as amd64's, and reports whether the code goes on. It uses B and C.
func (c *a64Compiler) stringUnit(v, str, key *ssa.Value, guard func(arm64.Cond)) bool {
	if !c.reference(v, str, c.enc.String, guard) {
		return false
	}
	c.stringUnitAt(key, guard, c.stubLabel(v.State, exitKind(v.Aux)))
	return true
}

// stringUnitAt is stringUnit once the string is in C, failing at fail, as
// guard does.
func (c *a64Compiler) stringUnitAt(key *ssa.Value, guard func(arm64.Cond), fail arm64.Label) {
	c.a.Load(a64B, a64C, c.enc.StringLeft)
	c.a.Cbnz(a64B, fail, true)
	c.index(key, guard)
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
}

// substringChar is s.substring(i, j), s.slice(i, j), s.substr(i, j) or
// s.charAt(i) (ssa.OpSubstringChar), as amd64's. It uses A, B, C, D, F0,
// F1 and F2.
func (c *a64Compiler) substringChar(v *ssa.Value) {
	none, done := c.a.NewLabel(), c.a.NewLabel()
	fail := func(cond arm64.Cond) { c.a.BCond(cond, none) }
	if v.Aux != ssa.SubstrAt {
		c.movImm(a64D, math.Float64bits(1))
		c.a.FMovToF(a64F2, a64D)
		if v.Aux == ssa.SubstrLength {
			c.a.FCmp(c.fpr(v.Args[2], a64F1), a64F2)
		} else {
			c.a.FArith(arm64.FAdd, a64F2, a64F2, c.fpr(v.Args[1], a64F0))
			c.a.FCmp(a64F2, c.fpr(v.Args[2], a64F1))
		}
		fail(arm64.NE)
	}
	if s := v.Args[0]; s.Shadow == nil && c.origin.At(s) < 0 {
		c.a.B(none)
	} else {
		c.referenceTo(v, s, c.enc.String, fail, none)
		c.stringUnitAt(v.Args[1], fail, none)
	}
	c.a.CmpImm(a64A, 256, true)
	fail(arm64.HS)
	c.a.Load(a64B, a64Ctx, abi.OffCharStrings)
	c.a.Cbz(a64B, none, true)
	c.a.ShiftImm(arm64.Lsl, a64A, a64A, 4, true)
	c.a.Op(arm64.Add, a64A, a64A, a64B, true)
	c.a.B(done)
	c.a.Bind(none)
	c.movImm(a64A, 0)
	c.a.Bind(done)
	c.setG(v, a64A)
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

// indexedOf finds an array or a typed array (OpIndexedOf), as amd64's.
func (c *a64Compiler) indexedOf(v *ssa.Value, guard func(arm64.Cond)) {
	if !c.objectOf(v, guard) {
		return
	}
	ok := c.a.NewLabel()
	c.a.LoadU8(a64B, a64C, c.enc.ObjectClass)
	c.a.CmpImm(a64B, int64(c.enc.ClassArray), false)
	c.a.BCond(arm64.EQ, ok)
	c.a.CmpImm(a64B, int64(c.enc.ClassTypedArray), false)
	guard(arm64.NE)
	c.a.Bind(ok)
	c.setG(v, a64C)
}

// typedArray jumps to typed unless the object array, from OpIndexedOf, is
// an Array. It uses B.
func (c *a64Compiler) typedArray(array *ssa.Value, typed arm64.Label) {
	c.a.LoadU8(a64B, c.gpr(array, a64B), c.enc.ObjectClass)
	c.a.CmpImm(a64B, int64(c.enc.ClassArray), false)
	c.a.BCond(arm64.NE, typed)
}

// typedElement finds a typed array's element, as amd64's: its address in
// A, the view's element type in C. It uses A, B, C, D and F1.
func (c *a64Compiler) typedElement(array, key *ssa.Value, store bool, fail func(arm64.Cond)) {
	c.objectData(a64C, c.gpr(array, a64C), a64B, c.enc.TypedData, fail)
	c.a.Load(a64B, a64C, c.enc.TypedStorage)
	c.a.CmpImm(a64B, 0, true)
	fail(arm64.EQ)
	c.a.Load(a64A, a64C, c.enc.TypedEnd)
	c.a.Load(a64D, a64B, c.enc.BufferBytes+8)
	c.a.Cmp(a64A, a64D, true)
	fail(arm64.HI)
	if store {
		c.a.LoadU8(a64A, a64B, c.enc.BufferImmutable)
		c.a.CmpImm(a64A, 0, false)
		fail(arm64.NE)
	}
	c.a.Load(a64B, a64B, c.enc.BufferBytes)
	c.a.Load(a64A, a64C, c.enc.TypedOffset)
	c.a.Op(arm64.Add, a64B, a64B, a64A, true)
	// The key an integer -- FCMP of NaN is unordered, not equal -- below
	// the length, compared unsigned.
	k := c.fpr(key, a64F0)
	c.a.Fcvtzs(a64A, k)
	c.a.Scvtf(a64F1, a64A, true)
	c.a.FCmp(a64F1, k)
	fail(arm64.NE)
	c.a.Load(a64D, a64C, c.enc.TypedLength)
	c.a.Cmp(a64A, a64D, true)
	fail(arm64.HS)
	c.a.LoadU8(a64C, a64C, c.enc.TypedKind)
}

// typedCases is amd64's: for each element type native code makes, a case
// of the type in C, the element's address into A first.
func (c *a64Compiler) typedCases(other arm64.Label, each func(kind int32)) {
	for _, t := range typedKinds {
		next := c.a.NewLabel()
		c.a.CmpImm(a64C, int64(t.kind), false)
		c.a.BCond(arm64.NE, next)
		if t.shift != 0 {
			c.a.ShiftImm(arm64.Lsl, a64A, a64A, t.shift, true)
		}
		c.a.Op(arm64.Add, a64A, a64A, a64B, true)
		each(t.kind)
		c.a.Bind(next)
	}
	c.a.B(other)
}

// elemStore stores any value in an Array's element, as amd64's, the
// value's pointer word in D while the element's cell is found. It uses A,
// B, C, D, F0 and F1.
func (c *a64Compiler) elemStore(v *ssa.Value) {
	x := v.Args[2]
	stub := c.stubLabel(v.State, exitKind(v.Aux))
	slow, done := c.a.NewLabel(), c.a.NewLabel()
	checked := len(v.Args) > 3
	if c.enc.CallGo == 0 || checked {
		slow = stub
	}
	fail := func(cond arm64.Cond) { c.a.BCond(cond, slow) }
	typed := c.a.NewLabel()
	fresh := v.Index == ssa.StoreFresh && c.enc.ValueSize == 16
	if !fresh {
		c.typedArray(v.Args[0], typed)
	}
	words := func() {
		var w arm64.Reg
		if remat(x) {
			c.materialize(x, a64A)
			w = a64A
		} else {
			w = c.gpr(x, a64A)
		}
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
	}
	words()
	c.a.MovRR(a64D, a64B)
	c.index(v.Args[1], fail)
	found := c.a.NewLabel()
	if fresh {
		c.freshCell(v.Args[0], fail)
	} else {
		c.storeCell(v.Args[0], !checked, fail, found)
	}
	c.a.Bind(found)
	for _, s := range v.Args[3:] {
		// As amd64's: only a pointer word there is lost.
		other := c.a.NewLabel()
		c.a.Cmp(c.gpr(s, a64B), a64A, true)
		c.a.BCond(arm64.NE, other)
		c.a.Load(a64B, a64A, c.enc.RefOffset)
		c.a.Cbnz(a64B, stub, true)
		c.a.Bind(other)
	}
	scalar, viaGo := c.a.NewLabel(), c.a.NewLabel()
	c.a.MovRR(a64C, a64D)
	c.a.Load(a64B, a64A, c.enc.RefOffset)
	c.a.Op(arm64.Orr, a64B, a64B, a64C, true)
	c.a.Cbz(a64B, scalar, true)
	// As amd64's: while the collector marks, with Go's write barrier.
	c.a.FMovToF(a64F0, c.gpr(x, a64B))
	c.movImm(a64B, c.enc.WriteBarrier)
	c.a.LoadU8(a64B, a64B, 0)
	c.a.Cbnz(a64B, viaGo, false)
	c.a.Bind(scalar)
	c.a.Store(a64A, c.enc.NumOffset, c.gpr(x, a64B))
	c.a.Store(a64A, c.enc.RefOffset, a64C)
	c.a.Bind(done)
	if !fresh {
		c.cold = append(c.cold, func() {
			// A typed array's element: a number stored natively, as amd64's.
			c.a.Bind(typed)
			var tw arm64.Reg
			if remat(x) {
				c.materialize(x, a64A)
				tw = a64A
			} else {
				tw = c.gpr(x, a64A)
			}
			c.movImm(a64B, abi.NumberLimit)
			c.a.Cmp(tw, a64B, true)
			c.a.BCond(arm64.HS, slow)
			c.a.FMovToF(a64F2, tw)
			c.typedWrite(v, done, slow, func() arm64.FReg { return a64F2 })
		})
	}
	c.cold = append(c.cold, func() {
		c.a.Bind(viaGo)
		c.valueStore()
		c.a.B(done)
	})
	if c.enc.CallGo == 0 || checked {
		return
	}
	c.cold = append(c.cold, func() {
		// Any other store Go makes if it can (ssa.GoElemStore).
		c.a.Bind(slow)
		size := int32(unsafe.Sizeof(abi.GoArg{}))
		c.a.Store(a64Ctx, abi.OffGoArgs+c.enc.RefOffset, c.gpr(v.Args[0], a64C))
		c.a.FMovFromF(a64A, c.fpr(v.Args[1], a64F0))
		c.a.Store(a64Ctx, abi.OffGoArgs+size+c.enc.NumOffset, a64A)
		words()
		c.a.Store(a64Ctx, abi.OffGoArgs+2*size+c.enc.RefOffset, a64B)
		c.a.Store(a64Ctx, abi.OffGoArgs+2*size+c.enc.NumOffset, c.gpr(x, a64A))
		c.goSaving(uint64(ssa.GoElemStore)|uint64(v.Index&ssa.StoreFresh)<<abi.GoOpIndexShift, &stub)
		c.a.B(done)
	})
}

// typedRead reads a typed array's element into F0, as amd64's.
func (c *a64Compiler) typedRead(v *ssa.Value, done arm64.Label) {
	stub := c.stubLabel(v.State, exitKind(v.Aux))
	slow := stub
	if c.enc.CallGo != 0 {
		slow = c.a.NewLabel()
	}
	c.typedElement(v.Args[0], v.Args[1], false, func(cond arm64.Cond) { c.a.BCond(cond, slow) })
	c.typedCases(slow, func(kind int32) {
		switch kind {
		case abi.ElemInt8:
			c.a.LoadS8(a64A, a64A, 0)
		case abi.ElemUint8, abi.ElemUint8Clamped:
			c.a.LoadU8(a64A, a64A, 0)
		case abi.ElemInt16:
			c.a.LoadS16(a64A, a64A, 0)
		case abi.ElemUint16:
			c.a.LoadU16(a64A, a64A, 0)
		case abi.ElemInt32:
			c.a.LoadS32(a64A, a64A, 0)
		case abi.ElemUint32:
			c.a.LoadU32(a64A, a64A, 0)
		case abi.ElemFloat32:
			c.a.LoadS(a64F0, a64A, 0)
			c.a.FCvtFromSingle(a64F0, a64F0)
			c.a.B(done)
			return
		case abi.ElemFloat64:
			c.a.LoadF(a64F0, a64A, 0)
			c.a.B(done)
			return
		}
		c.a.Scvtf(a64F0, a64A, true)
		c.a.B(done)
	})
	if c.enc.CallGo == 0 {
		return
	}
	c.a.Bind(slow)
	size := int32(unsafe.Sizeof(abi.GoArg{}))
	c.a.Store(a64Ctx, abi.OffGoArgs+c.enc.RefOffset, c.gpr(v.Args[0], a64C))
	c.a.FMovFromF(a64A, c.fpr(v.Args[1], a64F0))
	c.a.Store(a64Ctx, abi.OffGoArgs+size+c.enc.NumOffset, a64A)
	c.goSaving(uint64(ssa.GoTypedRead), &stub)
	c.a.LoadF(a64F0, a64Ctx, abi.OffGoArgs+c.enc.NumOffset)
	c.a.B(done)
}

// typedWrite stores a number in a typed array's element, as amd64's.
// The value is in the register value gives, a double.
func (c *a64Compiler) typedWrite(v *ssa.Value, done, slow arm64.Label, value func() arm64.FReg) {
	fail := func(cond arm64.Cond) { c.a.BCond(cond, slow) }
	c.typedElement(v.Args[0], v.Args[1], true, fail)
	c.typedCases(slow, func(kind int32) {
		x := value()
		switch kind {
		case abi.ElemFloat64:
			c.a.StoreF(a64A, 0, x)
		case abi.ElemFloat32:
			c.a.FCvtToSingle(a64F1, x)
			c.a.StoreS(a64A, 0, a64F1)
		case abi.ElemUint8Clamped:
			// Saturated, and rounded half to even: LE after FCMP holds for
			// an unordered pair, NaN's.
			low, high, store := c.a.NewLabel(), c.a.NewLabel(), c.a.NewLabel()
			c.a.FMovToF(a64F1, arm64.ZR)
			c.a.FCmp(x, a64F1)
			c.a.BCond(arm64.LE, low)
			c.movImm(a64B, math.Float64bits(255))
			c.a.FMovToF(a64F1, a64B)
			c.a.FCmp(x, a64F1)
			c.a.BCond(arm64.GE, high)
			c.a.FRint(arm64.FRintN, a64F1, x)
			c.a.Fcvtzs(a64B, a64F1)
			c.a.B(store)
			c.a.Bind(low)
			c.movImm(a64B, 0)
			c.a.B(store)
			c.a.Bind(high)
			c.movImm(a64B, 255)
			c.a.Bind(store)
			c.a.Store8(a64A, 0, a64B)
		default:
			// ToInt32's bits: FCVTZS's where |x| < 2**63, NaN's 0 among
			// them; Go's where it saturates.
			c.a.Fcvtzs(a64B, x)
			c.movImm(a64C, math.MaxInt64)
			c.a.Cmp(a64B, a64C, true)
			fail(arm64.EQ)
			c.movImm(a64C, 1<<63)
			c.a.Cmp(a64B, a64C, true)
			fail(arm64.EQ)
			switch kind {
			case abi.ElemInt8, abi.ElemUint8:
				c.a.Store8(a64A, 0, a64B)
			case abi.ElemInt16, abi.ElemUint16:
				c.a.Store16(a64A, 0, a64B)
			default:
				c.a.Store32(a64A, 0, a64B)
			}
		}
		c.a.B(done)
	})
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

// storeCell is amd64's: elementCell for elemStore, a hole stored into
// where the array may take it, or, where appends, a store at its length
// where it has room appended, the cell's address in A at found. It keeps
// D, the value's pointer word.
func (c *a64Compiler) storeCell(array *ssa.Value, appends bool, fail func(arm64.Cond), found arm64.Label) {
	protos := c.f.ArrayProtos
	if protos[0].Object == 0 || c.enc.ValueSize != 16 {
		c.elementCell(array, fail)
		return
	}
	p := c.gpr(array, a64C)
	c.a.Load(a64B, p, c.enc.ObjectElems+8)
	c.a.Cmp(a64A, a64B, true)
	if appends {
		end := c.a.NewLabel()
		c.a.BCond(arm64.HS, end)
		c.cold = append(c.cold, func() {
			c.a.Bind(end)
			fail(arm64.NE)
			c.a.Load(a64B, c.gpr(array, a64C), c.enc.ObjectElems+16)
			c.a.Cmp(a64A, a64B, true)
			fail(arm64.HS)
			c.a.FMovToF(a64F1, a64A)
			c.arrayGuards(c.gpr(array, a64C), protos, fail)
			c.a.FMovFromF(a64A, a64F1)
			p := c.gpr(array, a64C)
			c.a.AddImm(a64B, a64A, 1, true)
			c.a.Store(p, c.enc.ObjectElems+8, a64B)
			c.a.Load(a64B, p, c.enc.ObjectElems)
			c.a.ShiftImm(arm64.Lsl, a64A, a64A, 4, true)
			c.a.Op(arm64.Add, a64A, a64A, a64B, true)
			c.a.B(found)
		})
	} else {
		fail(arm64.HS)
	}
	c.a.Load(a64B, p, c.enc.ObjectElems)
	c.a.ShiftImm(arm64.Lsl, a64A, a64A, 4, true)
	c.a.Op(arm64.Add, a64A, a64A, a64B, true)
	c.a.Load(a64B, a64A, c.enc.NumOffset)
	c.movImm(a64C, c.enc.Uninitialized)
	c.a.Cmp(a64B, a64C, true)
	hole := c.a.NewLabel()
	c.a.BCond(arm64.EQ, hole)
	c.cold = append(c.cold, func() {
		c.a.Bind(hole)
		c.a.FMovToF(a64F1, a64A)
		c.arrayGuards(c.gpr(array, a64C), protos, fail)
		c.a.FMovFromF(a64A, a64F1)
		c.a.B(found)
	})
}

// freshCell is amd64's: storeCell for a store into an array a loop made
// (ssa.StoreFresh), at its length where it has room, the cell's address in
// A. It keeps D.
func (c *a64Compiler) freshCell(array *ssa.Value, fail func(arm64.Cond)) {
	p := c.gpr(array, a64C)
	c.a.Load(a64B, p, c.enc.ObjectElems+8)
	c.a.Cmp(a64A, a64B, true)
	fail(arm64.NE)
	c.a.Load(a64B, p, c.enc.ObjectElems+16)
	c.a.Cmp(a64A, a64B, true)
	fail(arm64.HS)
	c.a.AddImm(a64B, a64A, 1, true)
	c.a.Store(p, c.enc.ObjectElems+8, a64B)
	c.a.Load(a64B, p, c.enc.ObjectElems)
	c.a.ShiftImm(arm64.Lsl, a64A, a64A, 4, true)
	c.a.Op(arm64.Add, a64A, a64A, a64B, true)
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
		// As amd64's: the binding's cell, then where its value is now, a
		// captured local's of the code's own with Aux 1.
		d := c.gdst(v)
		base := abi.OffUpvalues
		if v.Aux == 1 {
			base = abi.OffCells
		}
		if v.Aux == 2 {
			c.movImm(d, v.Const.Bits)
		} else {
			c.a.Load(d, a64Ctx, base)
		}
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
	case ssa.OpOddballF64:
		c.oddball(v, guard)
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
	case ssa.OpIndexedOf:
		c.indexedOf(v, c.guardFor(v))
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
	case ssa.OpBigUnbox:
		c.bigUnbox(v, guard)
	case ssa.OpBigUnboxU64, ssa.OpBigLow, ssa.OpBigSign, ssa.OpAddW, ssa.OpSubW, ssa.OpMulW, ssa.OpAndW, ssa.OpOrW, ssa.OpXorW,
		ssa.OpShlW, ssa.OpShrW, ssa.OpSarW, ssa.OpZextW, ssa.OpSextW, ssa.OpAddU64, ssa.OpSubU64, ssa.OpMulU64, ssa.OpShlI64, ssa.OpShlU64,
		ssa.OpDivI64, ssa.OpModI64, ssa.OpDivU64, ssa.OpModU64, ssa.OpBigOfF64, ssa.OpPowI64:
		c.bigWord(v, c.guardFor(v))
	case ssa.OpBigBox:
		c.bigBox(v)
	case ssa.OpConstI64:
		d := c.gdst(v)
		c.movImm(d, v.Const.Bits)
		c.setG(v, d)
	case ssa.OpBigCell:
		c.a.AddImm(a64A, a64Ctx, int64(abi.OffKeep+int32(v.Args[0].Index)*int32(c.enc.ValueSize)), true)
		c.setG(v, a64A)
	case ssa.OpAddI64, ssa.OpSubI64:
		op := arm64.Adds
		if v.Op == ssa.OpSubI64 {
			op = arm64.Subs
		}
		c.a.Op(op, a64A, c.gpr(arg(0), a64A), c.gpr(arg(1), a64B), true)
		// Outside the int64 range: a BigInt on the heap, Go's.
		guard(arm64.VS)
		c.setG(v, a64A)
	case ssa.OpMulI64:
		// The product's high word is its low word's sign unless it is
		// outside the int64 range.
		x, y := c.gpr(arg(0), a64A), c.gpr(arg(1), a64B)
		c.a.Mul(a64C, x, y)
		c.a.Smulh(a64B, x, y)
		c.a.ShiftImm(arm64.Asr, a64A, a64C, 63, true)
		c.a.Cmp(a64B, a64A, true)
		guard(arm64.NE)
		c.setG(v, a64C)
	case ssa.OpCmpI64:
		c.a.Cmp(c.gpr(arg(0), a64A), c.gpr(arg(1), a64B), true)
		c.a.Cset(a64A, a64CmpCond(ir.Operator(v.Aux), arg(0).Type == ssa.Uint64))
		c.setG(v, a64A)
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
		c.a.Load(a64B, a64Ctx, abi.OffBounds)
		c.a.Load(a64B, a64B, abi.OffBoundsLevelLimit)
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
		c.objectData(a64B, a64B, a64C, c.enc.IterState, guard)
		c.a.LoadU8(a64C, a64B, c.enc.IterDone)
		c.a.Cbnz(a64C, ok, false)
		c.a.Load(a64C, a64B, c.enc.IterArr)
		c.a.CmpImm(a64C, 0, true)
		guard(arm64.EQ)
		if v.Const.Bits == 0 {
			c.a.B(c.stubLabel(v.State, exitKind(v.Aux)))
			break
		}
		c.movImm(a64C, v.Const.Bits)
		c.a.Load(a64C, a64C, 0)
		c.a.CmpImm(a64C, 0, true)
		guard(arm64.EQ)
		c.a.LoadU8(a64C, a64C, 0)
		c.a.CmpImm(a64C, 0, false)
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
		c.a.Load(a64B, a64Ctx, abi.OffBounds)
		c.a.Load(a64B, a64B, abi.OffBoundsTailLevel)
		c.a.Cmp(a64A, a64B, true)
		guard(arm64.HS)
	case ssa.OpSameObject:
		if v.Index == 1 {
			// A typed array of the prototype at Const.Bits, as amd64's.
			p := c.gpr(arg(0), a64A)
			c.a.LoadU8(a64B, p, c.enc.ObjectClass)
			c.a.CmpImm(a64B, int64(c.enc.ClassTypedArray), false)
			guard(arm64.NE)
			c.a.Load(a64B, p, c.enc.ObjectProto)
			c.movImm(a64C, v.Const.Bits)
			c.a.Cmp(a64B, a64C, true)
			guard(arm64.NE)
			break
		}
		c.movImm(a64B, v.Const.Bits)
		c.a.Cmp(c.gpr(arg(0), a64A), a64B, true)
		guard(arm64.NE)
	case ssa.OpAccessorIs:
		// The cell's accessor pair, and its getter's or its setter's
		// function.
		p := c.gpr(arg(0), a64A)
		c.a.Load(a64C, p, c.enc.NumOffset)
		c.movImm(a64B, c.enc.Accessor)
		c.a.Cmp(a64C, a64B, true)
		guard(arm64.NE)
		c.a.Load(a64C, p, c.enc.RefOffset)
		at := c.enc.AccessorGetter
		if v.Index == 1 {
			at = c.enc.AccessorSetter
		}
		c.a.Load(a64C, a64C, at)
		c.movImm(a64B, v.Const.Bits)
		c.a.Cmp(a64C, a64B, true)
		guard(arm64.NE)
	case ssa.OpPropRead:
		c.property(v, c.guardFor(v))
		c.numberCell(guard)
		c.a.FMovToF(a64F0, a64B)
		c.setF(v, a64F0)
	case ssa.OpPropCell:
		if v.Tries() {
			// As amd64's: none where it finds no property.
			none, found := c.a.NewLabel(), c.a.NewLabel()
			c.property(v, func(cond arm64.Cond) { c.a.BCond(cond, none) })
			c.a.Bind(found)
			c.setG(v, a64A)
			c.cold = append(c.cold, func() {
				c.a.Bind(none)
				c.movImm(a64A, 0)
				c.a.B(found)
			})
			break
		}
		c.property(v, c.guardFor(v))
		c.setG(v, a64A)
	case ssa.OpCellNone:
		c.a.CmpImm(c.gpr(arg(0), a64B), 0, true)
		c.a.Cset(a64A, arm64.EQ)
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
		c.keyAt(v, guard)
		c.a.LoadU8(a64B, a64B, int32(v.Index)*c.enc.KeySize+c.enc.PropertyFlags)
		c.movImm(a64C, uint64(c.enc.PropNotData|c.enc.PropUninit))
		c.a.Tst(a64B, a64C, false)
		guard(arm64.NE)
		c.a.Load(a64A, a64A, c.enc.ObjectProps)
		c.a.AddImm(a64A, a64A, int64(int32(v.Index)*c.enc.ValueSize), true)
		c.setG(v, a64A)
	case ssa.OpLexCell:
		// As amd64's: the bindings' object, the binding's key, and out of
		// its temporal dead zone.
		c.a.Load(a64A, a64Ctx, abi.OffGlobalLex)
		c.a.Cbz(a64A, c.stubLabel(v.State, exitKind(v.Aux)), true)
		c.keyAt(v, guard)
		c.a.Load(a64A, a64A, c.enc.ObjectProps)
		c.a.AddImm(a64A, a64A, int64(int32(v.Index)*c.enc.ValueSize), true)
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
		// shape the one the code was compiled for; or a BigInt's.
		if h := v.Holders(); h != nil {
			c.movImm(a64B, c.enc.HeapBigTag)
			c.a.Cmp(c.gpr(arg(0), a64A), a64B, true)
			guard(arm64.LO)
			c.movImm(a64A, uint64(h.Holders[0].Object))
		} else {
			c.movImm(a64B, c.enc.String)
			c.a.Cmp(c.gpr(arg(0), a64A), a64B, true)
			guard(arm64.NE)
			c.a.Load(a64A, a64Ctx, abi.OffStringMethods)
			c.a.Cbz(a64A, c.stubLabel(v.State, exitKind(v.Aux)), true)
		}
		c.a.Load(a64B, a64A, c.enc.ObjectShape)
		c.movImm(a64C, v.Const.Bits)
		c.a.Cmp(a64B, a64C, true)
		guard(arm64.NE)
		c.a.Load(a64B, a64A, c.enc.ObjectShape)
		c.a.Load(a64B, a64B, c.enc.ShapeKeys)
		c.a.LoadU32(a64B, a64B, int32(v.Index)*c.enc.KeySize+c.enc.PropertyKey)
		c.a.CmpImm(a64B, int64(v.Key), false)
		guard(arm64.NE)
		c.a.Load(a64A, a64A, c.enc.ObjectProps)
		c.a.AddImm(a64A, a64A, int64(int32(v.Index)*c.enc.ValueSize), true)
		c.setG(v, a64A)
	case ssa.OpStringCode:
		c.stringCode(v, c.guardFor(v))
	case ssa.OpStringChar:
		c.stringChar(v, c.guardFor(v))
	case ssa.OpSubstringChar:
		c.substringChar(v)
	case ssa.OpCodeChar:
		c.codeChar(v, c.guardFor(v))
	case ssa.OpLoopMark:
		// As amd64's: abi.Context's Loop.
		c.movImm(a64A, v.Const.Bits)
		c.a.Store(a64Ctx, abi.OffLoop, a64A)
	case ssa.OpFreeze:
		c.freeze(v)
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
		typed, done := c.a.NewLabel(), c.a.NewLabel()
		c.typedArray(arg(0), typed)
		c.index(arg(1), c.guardFor(v))
		c.element(arg(0), c.guardFor(v))
		c.a.FMovToF(a64F0, a64B)
		c.a.Bind(done)
		c.cold = append(c.cold, func() {
			c.a.Bind(typed)
			c.typedRead(v, done)
		})
		c.setF(v, a64F0)
	case ssa.OpElemCell:
		fail, done := c.guardFor(v), c.a.NewLabel()
		if c.enc.CallGo != 0 && c.enc.UndefinedCell != 0 && v.Index != ssa.ElemPresent {
			// An element it does not find Go says is undefined, where it
			// can (ssa.GoElemCell), but for a loop's (ssa.ElemPresent).
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
	case ssa.OpElemStore:
		c.elemStore(v)
	case ssa.OpElemWrite:
		done, typed := c.a.NewLabel(), c.a.NewLabel()
		slow := c.stubLabel(v.State, exitKind(v.Aux))
		if c.enc.CallGo != 0 && len(v.Args) == 3 {
			// Any other store Go makes if it can (ssa.GoElemWrite), but
			// for one with checks, as amd64's.
			stub := slow
			slow = c.a.NewLabel()
			c.cold = append(c.cold, func() {
				c.a.Bind(slow)
				size := int32(unsafe.Sizeof(abi.GoArg{}))
				c.a.Store(a64Ctx, abi.OffGoArgs+c.enc.RefOffset, c.gpr(v.Args[0], a64C))
				c.a.FMovFromF(a64A, c.fpr(v.Args[1], a64F0))
				c.a.Store(a64Ctx, abi.OffGoArgs+size+c.enc.NumOffset, a64A)
				c.a.FMovFromF(a64A, c.fpr(v.Args[2], a64F0))
				c.a.Store(a64Ctx, abi.OffGoArgs+2*size+c.enc.NumOffset, a64A)
				if c.f.ArrayProtos[0].Object != 0 && c.enc.ValueSize == 16 {
					c.goAppending(&stub)
				} else {
					c.goSaving(uint64(ssa.GoElemWrite), &stub)
				}
				c.a.B(done)
			})
		}
		fail := func(cond arm64.Cond) { c.a.BCond(cond, slow) }
		c.cold = append(c.cold, func() {
			c.a.Bind(typed)
			c.typedWrite(v, done, slow, func() arm64.FReg { return c.fpr(v.Args[2], a64F0) })
		})
		c.typedArray(arg(0), typed)
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
	case ssa.OpRandomF64:
		c.random(v)
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
		if v.Aux == 2 {
			// Null, or undefined: both compared before the result, which
			// may be the operand's register, is written.
			c.movImm(a64A, c.enc.Undefined)
			c.a.Cmp(w, a64A, true)
			c.a.Cset(a64C, arm64.EQ)
			c.movImm(a64A, word)
			c.a.Cmp(w, a64A, true)
			c.a.Cset(a64A, arm64.EQ)
			d := c.gdst(v)
			c.a.Op(arm64.Orr, d, a64A, a64C, false)
			c.setG(v, d)
			break
		}
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
	case ssa.OpBoolToI32, ssa.OpBoolToF64:
		// As amd64's: 1 or 0.
		c.a.CmpImm(c.gpr(arg(0), a64A), 0, false)
		c.a.Cset(a64A, arm64.NE)
		if v.Op == ssa.OpBoolToI32 {
			c.setG(v, a64A)
			break
		}
		c.a.Scvtf(a64F0, a64A, false)
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
	if c.enc.HeapBigTag != 0 {
		// A BigInt, true unless it is zero, as amd64's is.
		notBig := c.a.NewLabel()
		c.movImm(a64A, c.enc.HeapBigTag)
		c.a.Cmp(r, a64A, true)
		c.a.BCond(arm64.EQ, yes)
		c.movImm(a64A, c.enc.ShortBigWideTag)
		c.a.Cmp(r, a64A, true)
		c.a.BCond(arm64.LO, notBig)
		c.movImm(a64A, c.enc.ShortBigTag)
		c.a.Cmp(r, a64A, true)
		c.a.BCond(arm64.NE, yes)
		if c.reference(v, v.Args[0], c.enc.ShortBigTag, guard) {
			c.movImm(a64A, c.enc.ShortBigHigh)
			c.a.Cmp(a64C, a64A, true)
			c.a.BCond(arm64.EQ, no)
			c.a.B(yes)
		}
		c.a.Bind(notBig)
	}
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
