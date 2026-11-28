package mir

import (
	"fmt"
	"math"
	"math/bits"
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
	// callV, callTop and allocDone are amd64's.
	callV     *ssa.Value
	callTop   arm64.Label
	allocDone arm64.Label
	// genDone is amd64's: where a generator a loop's step resumed natively
	// goes once it finishes (genFinish).
	genDone     arm64.Label
	genDoneUsed bool
	// tail is where exits that filled records go (recordsTail), if any
	// does; recorded marks the exit being emitted as one. tailMarked and
	// tailRet are amd64's.
	tail                arm64.Label
	tailMarked, tailRet arm64.Label
	tailUsed, recorded  bool
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
	// retTail is amd64's (returnFast).
	retTail     arm64.Label
	retTailUsed bool
	// calling, goFails, goCatch, goCatches and catches are amd64's.
	calling     arm64.Label
	callingUsed bool
	goFails     []a64GoFail
	goCatch     arm64.Label
	goCatches   []a64GoFail
	catches     []a64CatchStub
	live        arm64.Label
	liveUsed    bool
	// bigLive, bigLiveUsed and goCallBig are amd64's.
	bigLive                arm64.Label
	bigLiveUsed, goCallBig bool
	// saving is the routine every call of Go that saves every register
	// shares (goSaving), if any makes one.
	saving     arm64.Label
	savingUsed bool
	// stubSaving and stubSavingUsed are amd64's.
	stubSaving     arm64.Label
	stubSavingUsed bool
	// refStoring is the routine of the stores storeRef makes (refStore).
	refStoring     arm64.Label
	refStoringUsed bool
	// appending is the routine the element writes Go makes share where a
	// number at an array's length may be appended natively
	// (appendingCall), as amd64's.
	appending     arm64.Label
	appendingUsed bool
	// entered is amd64's.
	entered *ssa.Block
	// budget, budgetCold, budgetStubs and budgetReal are amd64's.
	budget     int
	budgetCold int
	// coldBudget is, by its index in cold, the budget a cold path is
	// emitted with (setBudget), or noBudget where it takes the one in force
	// when it is.
	coldBudget  []int
	budgetStubs map[budgetStub]arm64.Label
	budgetReal  map[arm64.Label]arm64.Label
	// keepCold and keeping are amd64's (keep).
	keepCold arm64.Label
	keeping  []*ssa.Value
}

// a64GoFail is amd64's goFail.
type a64GoFail struct {
	st       *abi.GoCallState
	back, to arm64.Label
}

// a64CatchStub is amd64's catchStub.
type a64CatchStub struct {
	state     *ssa.FrameState
	label     arm64.Label
	keep, reg int
	to        *ssa.Block
}

type a64Stub struct {
	key   stubKey
	label arm64.Label
}

// CompileARM64 compiles f for arm64, given the VM's value encoding.
func CompileARM64(f *ssa.Func, enc abi.Encoding) (*Code, error) {
	return compileARM64(nil, f, enc, Settings())
}

// CompileARM64With is CompileARM64 with the switches opts, in place of
// the package's (Settings).
func CompileARM64With(f *ssa.Func, enc abi.Encoding, opts Options) (*Code, error) {
	return compileARM64(nil, f, enc, opts)
}

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

func compileARM64(w *Workspace, f *ssa.Func, enc abi.Encoding, opts Options) (code *Code, err error) {
	defer func() {
		if v := recover(); v != nil {
			if e, ok := v.(*AllocationError); ok {
				panic(e)
			}
			code, err = nil, fmt.Errorf("%w: %v", ErrUnsupported, v)
		}
	}()
	k, err := prepare(w, f, enc, opts, arm64GPRs, arm64FPRs)
	if err != nil {
		return nil, err
	}
	var c *a64Compiler
	if w != nil {
		c = &w.arm64.c
	} else {
		c = new(a64Compiler)
	}
	*c = a64Compiler{core: k, consts: enc.ConstWords(), budget: -1}
	c.stubAt = k.ints(2 * f.NumStates())
	if w != nil {
		g := &w.arm64
		if g.stubs == nil {
			g.stubs = map[stubKey]arm64.Label{}
		}
		g.a.Reset()
		clear(g.stubs)
		c.a, c.stubs, c.stubFor, c.cold = &g.a, g.stubs, g.stubFor[:0], g.cold[:0]
		c.coldBudget = g.coldBudget[:0]
		c.goFails, c.exits = g.goFails[:0], g.exits[:0]
		c.labels = slices.Grow(g.labels[:0], numBlocks(f))[:numBlocks(f)]
		defer func() {
			g.labels, g.stubFor, g.cold, g.coldBudget = c.labels[:0], c.stubFor[:0], clearFuncs(c.cold), c.coldBudget[:0]
			clear(c.goFails)
			g.goFails = c.goFails
			clear(c.exits)
			g.exits = c.exits[:0]
		}()
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
		c.entered = nil
		if f.CallEntry && b == c.entryAt(0) && b.PC < 0 {
			// As amd64's: Go's entry reads the frame's registers and jumps
			// past native callers' entry, which makes them.
			c.a.Bind(c.labels[b.ID])
			for _, e := range f.Entries {
				if e.Block == b {
					entries[e.PC] = c.a.Len()
				}
			}
			start := c.a.NewLabel()
			c.a.Load(a64Locals, a64Ctx, abi.OffLocals)
			c.a.Load(a64Stack, a64Ctx, abi.OffStack)
			c.a.B(start)
			callAt = c.a.Len()
			c.callEntry()
			c.a.Bind(start)
			c.entered = b
		} else {
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
		c.coldPath(i)
	}
	// As amd64's: the continuations of calls of Go at handlers.
	for _, k := range c.catches {
		c.a.Bind(k.label)
		c.catchTo(k)
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
	tailAt := -1
	if c.f.TailEntry && c.f.CallEntry && c.entryAt(0) != nil {
		tailAt = c.a.Len()
		c.tailEntry()
	}
	genAt := -1
	if c.f.GenEntry {
		genAt = c.a.Len()
		c.genEntry()
	}
	if c.genDoneUsed {
		c.a.Bind(c.genDone)
		c.genFinish()
	}
	if c.appendingUsed {
		c.a.Bind(c.appending)
		c.appendingCall()
	}
	refStoring := c.refStoringUsed
	if refStoring {
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
		c.savingCall(false)
	}
	if c.stubSavingUsed {
		c.a.Bind(c.stubSaving)
		c.savingCall(true)
	}
	if c.tailUsed {
		// As amd64's: past every other routine.
		c.a.Bind(c.tailMarked)
		c.recordsTailMarked()
	}
	if c.refStoringUsed && !refStoring {
		// As amd64's: where nothing before it stored so.
		c.a.Bind(c.refStoring)
		c.refStoringCall()
	}
	for _, g := range c.goFails {
		g.st.Fail = int32(c.a.Offset(g.to) - c.a.Offset(g.back))
	}
	for _, g := range c.goCatches {
		g.st.Catch = int32(c.a.Offset(g.to) - c.a.Offset(g.back))
	}
	c.lap(PhaseEmit)
	var dst []byte
	if w != nil {
		dst = w.arm64.code
	}
	bytes, err := c.a.FinishInto(dst)
	if err != nil {
		return nil, err
	}
	if w != nil {
		w.arm64.code = bytes
	}
	c.finishExits()
	c.lap(PhaseEncode)
	if t := c.opts.Timing; t != nil {
		t.Compiles++
	}
	return &Code{Bytes: bytes, Entries: entries, CallEntry: callAt, Resume: resumeAt, GenEntry: genAt, TailEntry: tailAt, Text: len(bytes), Exits: keptExits(c.exits, w != nil),
		Calls: c.calls, CallRefs: c.callRefs(), Spills: c.spills, GoCalls: c.goCalls, core: c.core}, nil
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
	case slot < c.f.Locals-c.f.Scratch:
		c.a.Load(scratch, a64Ctx, abi.OffUpvalues)
		c.a.Load(scratch, scratch, int32(slot-c.f.FrameLocals)*8)
		c.a.Load(scratch, scratch, c.enc.UpvalueSlot)
		return scratch, off
	}
	// An operand's, or, below Locals, a register program's scratch slot
	// (ssa.Func's Scratch): the context's Stack is past those, which are
	// past the window.
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
	c.spillLoad(scratch, l.spill)
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
	c.spillLoadF(scratch, l.spill)
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
	c.spillLoad(dst, c.locAt(v).spill)
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
	c.spillStore(l.spill, src)
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
	c.spillStoreF(l.spill, src)
}

// A spill slot is the context's (abi.Context's Spill), at a displacement
// from the context register, or past them in its SpillMore, reached
// through the slice's address, loaded at each access into the encoder's
// temporary, X27, which holds nothing across instructions where code reads
// or writes a slot. Code of no more than the context's slots has none of
// this.

// spillAddr is the base register and displacement of spill slot.
func (c *a64Compiler) spillAddr(slot int) (arm64.Reg, int32) {
	off, more := spillMore(slot)
	if !more {
		return a64Ctx, c.spillDisp(slot)
	}
	c.a.Load(arm64.X27, a64Ctx, abi.OffSpillMore)
	// Past a scaled load's reach, the rest added to X27 in steps an
	// immediate holds: a sum through the temporary would overwrite it.
	rest := int64(off)
	for rest >= 1<<15 {
		step := min(rest&^0xFFF, 0xFFF000)
		c.a.AddImm(arm64.X27, arm64.X27, step, true)
		rest -= step
	}
	return arm64.X27, int32(rest)
}

// spillLoad loads spill slot's word into dst.
func (c *a64Compiler) spillLoad(dst arm64.Reg, slot int) {
	base, d := c.spillAddr(slot)
	c.a.Load(dst, base, d)
}

// spillStore stores src to spill slot.
func (c *a64Compiler) spillStore(slot int, src arm64.Reg) {
	base, d := c.spillAddr(slot)
	c.a.Store(base, d, src)
}

// spillLoadF loads spill slot's double into dst.
func (c *a64Compiler) spillLoadF(dst arm64.FReg, slot int) {
	base, d := c.spillAddr(slot)
	c.a.LoadF(dst, base, d)
}

// spillStoreF stores src's double to spill slot.
func (c *a64Compiler) spillStoreF(slot int, src arm64.FReg) {
	base, d := c.spillAddr(slot)
	c.a.StoreF(base, d, src)
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
		if x := i32Boxed(v); x != nil {
			// As amd64's: an int32's number, made of the int32.
			c.a.Scvtf(a64F1, c.gpr(x, dst), false)
			c.a.FMovFromF(dst, a64F1)
			break
		}
		c.boxF64(c.fpr(v.Args[0], a64F1), dst)
	case ssa.OpBoxBool:
		c.boxBool(c.gpr(v.Args[0], a64B), dst)
	case ssa.OpConstI32:
		c.movImm(dst, v.Const.Bits)
	case ssa.OpLoadSlot:
		// As amd64's: a slot an entry loads for its unboxing alone.
		base, disp := c.slotAddr(v.Aux, false, dst)
		c.a.Load(dst, base, disp)
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
	if c.budget >= 0 {
		// As amd64's: the budget's register stored back, then the exit.
		k := budgetStub{s, kind, c.budget}
		if l, ok := c.budgetStubs[k]; ok {
			return l
		}
		b := c.budget
		c.budget = -1
		real := c.stubLabel(s, kind)
		c.budget = b
		l := c.a.NewLabel()
		if c.budgetStubs == nil {
			c.budgetStubs, c.budgetReal = map[budgetStub]arm64.Label{}, map[arm64.Label]arm64.Label{}
		}
		c.budgetStubs[k], c.budgetReal[l] = l, real
		reg := arm64.Reg(c.budgetReg(b))
		c.cold = append(c.cold, func() {
			c.a.Bind(l)
			c.budgetFlush(reg)
			c.a.B(real)
		})
		return l
	}
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
	defer c.viewExits()()
	if then == nil && c.opts.TableExits {
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
	c.recordRoom(s)
	c.recorded = false
	c.a.Store(a64Ctx, abi.OffRecords, arm64.ZR)
	for i, v := range s.All() {
		if v.Op == ssa.OpLoadSlot && v.Aux == i || c.captured(i) {
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
	c.a.Load(at, a64Ctx, abi.OffStackBase)
	c.a.AddLsl(base, at, base, 4)
	c.a.Store(a64Ctx, abi.OffLocals, base)
	c.a.Load(at, a64CallLink, c.enc.LinkLocals)
	c.a.AddLsl(at, base, at, 4)
	c.a.Store(a64Ctx, abi.OffStack, at)
	c.a.Store(a64Ctx, abi.OffOpen, arm64.ZR)
	c.a.Store(a64Ctx, abi.OffCells, arm64.ZR)
	// No code runs the call: Go runs its callee (abi.Context's Code).
	c.a.Store(a64Ctx, abi.OffCode, arm64.ZR)
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

// storeCode names the code in ctx, the context of the native call that
// runs it (abi.Context's Code, ssa.Func's Self), by way of scratch, as
// amd64's entries do.
func (c *a64Compiler) storeCode(ctx, scratch arm64.Reg) {
	if c.f.Self == 0 {
		c.a.Store(ctx, abi.OffCode, arm64.ZR)
		return
	}
	c.a.MovImm(scratch, uint64(c.f.Self))
	c.a.Store(ctx, abi.OffCode, scratch)
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

// recordsTail applies an exit's records natively, whether the collector
// marks or not, as amd64's does. D counts the records; F1 keeps how many.
// While the collector marks, the record its passes are at is kept in the
// context (abi.Context's RecordAt), since storeRef keeps neither.
func (c *a64Compiler) recordsTail() {
	ret, write := c.a.NewLabel(), c.a.NewLabel()
	c.tailMarked, c.tailRet = c.a.NewLabel(), ret
	c.movImm(a64B, c.enc.WriteBarrier)
	c.a.LoadU8(a64B, a64B, 0)
	c.a.Cbnz(a64B, c.tailMarked, false)
	c.a.Load(a64D, a64Ctx, abi.OffRecords)
	c.a.Cbz(a64D, ret, true)
	c.recordsRead()
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

// recordsTailMarked is amd64's: the collector marking, the pointers the
// first pass read shaded through storeRef into the context's Shade, then
// the second pass's pointer words stored through it (ptrStore), and back
// to the tail's return.
func (c *a64Compiler) recordsTailMarked() {
	ret := c.tailRet
	shade, shaded, store := c.a.NewLabel(), c.a.NewLabel(), c.a.NewLabel()
	c.a.Load(a64D, a64Ctx, abi.OffRecords)
	c.a.Cbz(a64D, ret, true)
	c.recordsRead()
	c.a.Load(a64A, a64Ctx, abi.OffRecords)
	c.a.Store(a64Ctx, abi.OffRecordAt, a64A)
	c.a.Bind(shade)
	c.a.Load(a64A, a64Ctx, abi.OffRecordAt)
	c.a.AddImm(a64A, a64A, -1, true)
	c.a.Store(a64Ctx, abi.OffRecordAt, a64A)
	c.a.ShiftImm(arm64.Lsl, a64A, a64A, 5, true)
	c.a.Op(arm64.Add, a64A, a64A, a64Ctx, true)
	c.a.Load(a64C, a64A, abi.OffRecord+8)
	c.a.Cbz(a64C, shaded, true)
	c.a.AddImm(a64A, a64Ctx, int64(abi.OffShade), true)
	c.refStore()
	c.a.Bind(shaded)
	c.a.Load(a64A, a64Ctx, abi.OffRecordAt)
	c.a.Cbnz(a64A, shade, true)
	c.a.Store(a64Ctx, abi.OffShade, arm64.ZR)
	c.a.Load(a64A, a64Ctx, abi.OffRecords)
	c.a.Store(a64Ctx, abi.OffRecordAt, a64A)
	c.a.Bind(store)
	c.a.Load(a64A, a64Ctx, abi.OffRecordAt)
	c.a.AddImm(a64A, a64A, -1, true)
	c.a.Store(a64Ctx, abi.OffRecordAt, a64A)
	c.a.ShiftImm(arm64.Lsl, a64A, a64A, 5, true)
	c.a.Op(arm64.Add, a64A, a64A, a64Ctx, true)
	c.a.Load(a64C, a64A, abi.OffRecord)
	c.movImm(a64B, ^uint64(abi.RecordScalar|abi.RecordMaybe))
	c.a.Op(arm64.And, a64C, a64C, a64B, true)
	c.sourceAddr()
	c.a.Load(a64B, a64A, abi.OffRecord+16)
	c.a.Store(a64C, c.enc.NumOffset, a64B)
	c.a.Load(a64B, a64A, abi.OffRecord+8)
	c.a.AddImm(a64A, a64C, int64(c.enc.RefOffset), true)
	c.a.MovRR(a64C, a64B)
	c.ptrStore(true)
	c.a.Load(a64A, a64Ctx, abi.OffRecordAt)
	c.a.Cbnz(a64A, store, true)
	c.a.Store(a64Ctx, abi.OffRecords, arm64.ZR)
	c.a.B(ret)
}

// recordsRead is amd64's: recordsTail's first pass, the count of records,
// not 0, in D, which it leaves in F1. It uses A, B, C and D.
func (c *a64Compiler) recordsRead() {
	read, maybe, scalar, copyValue, next := c.a.NewLabel(), c.a.NewLabel(), c.a.NewLabel(), c.a.NewLabel(), c.a.NewLabel()
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

// returnFast returns to a native caller, if one called, as amd64's.
func (c *a64Compiler) returnFast(x *ssa.Value) {
	toGo := c.a.NewLabel()
	c.gprInto(x, a64RetNum)
	if c.scalar(x) {
		c.a.MovRR(a64RetRef, arm64.ZR)
	} else {
		c.pointerWord(x, a64RetNum)
		c.a.MovRR(a64RetRef, a64B)
	}
	c.a.Load(a64CallLink, a64Ctx, abi.OffReturnTo)
	c.a.Cbz(a64CallLink, toGo, true)
	if !c.retTailUsed {
		c.retTail, c.retTailUsed = c.a.NewLabel(), true
		c.a.Bind(c.retTail)
		if c.f.CellsBack && !(c.f.CellsKept && c.f.GenEntry) {
			c.cellsBack()
		}
		c.returnTail()
	} else {
		c.a.B(c.retTail)
	}
	c.a.Bind(toGo)
}

// cellsBack is amd64's: the cells a native call's frame took back to
// their pool as it returns natively, while the collector marks with Go's
// write barrier (cellsBackMarking). It keeps A, B and C, the result and
// where it returns to, using allocatable registers, none live at a return.
func (c *a64Compiler) cellsBack() {
	block, pool, count, cell, word, at := arm64.Reg(3), arm64.Reg(4), arm64.Reg(5), arm64.Reg(6), arm64.Reg(7), arm64.Reg(8)
	skip, marking := c.a.NewLabel(), c.a.NewLabel()
	if c.f.CellsKept {
		c.a.Load(block, a64Ctx, abi.OffCellsKept)
		c.a.Cbnz(block, skip, true)
	}
	c.a.Load(block, a64Ctx, abi.OffCells)
	c.a.Cbz(block, skip, true)
	c.movImm(pool, uint64(c.f.CellsPool))
	c.a.Load(count, pool, abi.OffPoolCount)
	c.a.CmpImm(count, abi.PoolCapacity, true)
	c.a.BCond(arm64.HS, skip)
	c.movImm(word, c.enc.WriteBarrier)
	c.a.LoadU8(word, word, 0)
	c.a.Cbnz(word, marking, false)
	c.movImm(word, c.enc.Undefined)
	n := int32(c.f.CellsCount)
	for k := range n {
		c.a.AddImm(cell, block, int64(8*n+k*c.enc.UpvalueSize), true)
		c.a.Store(block, k*8, cell)
		c.a.AddImm(at, cell, int64(c.enc.UpvalueClosed), true)
		c.a.Store(cell, c.enc.UpvalueSlot, at)
		c.a.Store(cell, c.enc.UpvalueClosed+c.enc.NumOffset, word)
		c.a.Store(cell, c.enc.UpvalueClosed+c.enc.RefOffset, arm64.ZR)
	}
	c.a.ShiftImm(arm64.Lsl, at, count, 3, true)
	c.a.Op(arm64.Add, at, at, pool, true)
	c.a.Store(at, abi.OffPoolObjects, block)
	c.a.AddImm(count, count, 1, true)
	c.a.Store(pool, abi.OffPoolCount, count)
	c.a.Store(a64Ctx, abi.OffCells, arm64.ZR)
	c.a.B(skip)
	c.a.Bind(marking)
	c.cellsBackMarking()
	c.a.Bind(skip)
}

// cellsBackMarking is amd64's: A, B and C, the result and where the call
// returns to, kept in the context's GoArgs while refStore uses them.
func (c *a64Compiler) cellsBackMarking() {
	ret, link := abi.OffGoArgs, abi.OffGoArgs+c.enc.ValueSize
	c.a.Store(a64Ctx, ret+c.enc.NumOffset, a64RetNum)
	c.a.Store(a64Ctx, ret+c.enc.RefOffset, a64RetRef)
	c.a.Store(a64Ctx, link+c.enc.NumOffset, a64CallLink)
	n := int32(c.f.CellsCount)
	for k := range n {
		cell := 8*n + k*c.enc.UpvalueSize
		c.a.Load(a64A, a64Ctx, abi.OffCells)
		c.a.AddImm(a64C, a64A, int64(cell), true)
		c.a.AddImm(a64A, a64A, int64(k*8), true)
		c.refStore()
		c.a.Load(a64A, a64Ctx, abi.OffCells)
		c.a.AddImm(a64C, a64A, int64(cell+c.enc.UpvalueClosed), true)
		c.a.AddImm(a64A, a64A, int64(cell+c.enc.UpvalueSlot), true)
		c.refStore()
		c.a.Load(a64A, a64Ctx, abi.OffCells)
		c.movImm(a64C, c.enc.Undefined)
		c.a.Store(a64A, cell+c.enc.UpvalueClosed+c.enc.NumOffset, a64C)
		c.a.AddImm(a64A, a64A, int64(cell+c.enc.UpvalueClosed+c.enc.RefOffset), true)
		c.movImm(a64C, 0)
		c.refStore()
	}
	c.movImm(a64B, uint64(c.f.CellsPool))
	c.a.Load(a64A, a64B, abi.OffPoolCount)
	c.a.ShiftImm(arm64.Lsl, a64A, a64A, 3, true)
	c.a.Op(arm64.Add, a64A, a64A, a64B, true)
	c.a.AddImm(a64A, a64A, int64(abi.OffPoolObjects), true)
	c.a.Load(a64C, a64Ctx, abi.OffCells)
	c.refStore()
	c.movImm(a64B, uint64(c.f.CellsPool))
	c.a.Load(a64A, a64B, abi.OffPoolCount)
	c.a.AddImm(a64A, a64A, 1, true)
	c.a.Store(a64B, abi.OffPoolCount, a64A)
	c.a.Store(a64Ctx, abi.OffCells, arm64.ZR)
	c.a.Load(a64RetNum, a64Ctx, ret+c.enc.NumOffset)
	c.a.Load(a64RetRef, a64Ctx, ret+c.enc.RefOffset)
	c.a.Load(a64CallLink, a64Ctx, link+c.enc.NumOffset)
	c.a.Store(a64Ctx, ret+c.enc.RefOffset, arm64.ZR)
}

// budgetStore and budgetLoad are amd64's. Each uses B.
func (c *a64Compiler) budgetStore(r arm64.Reg) {
	c.a.Load(a64B, a64Ctx, abi.OffBackEdges)
	c.a.Store(a64B, 0, r)
}

func (c *a64Compiler) budgetLoad(r arm64.Reg) {
	c.a.Load(r, a64Ctx, abi.OffBackEdges)
	c.a.Load(r, r, 0)
}

// budgetStart and budgetFlush are amd64's.
func (c *a64Compiler) budgetStart(r arm64.Reg) { c.a.MovRR(r, arm64.ZR) }

func (c *a64Compiler) budgetFlush(r arm64.Reg) {
	store, back := c.a.NewLabel(), c.a.NewLabel()
	c.a.Cbnz(r, store, true)
	c.a.Bind(back)
	c.cold = append(c.cold, func() {
		c.a.Bind(store)
		c.budgetStore(r)
		c.a.B(back)
	})
}

// setBudget is amd64's.
func (c *a64Compiler) setBudget(b int) {
	if b >= 0 && c.budgetReg(b) < 0 {
		b = -1
	}
	for len(c.coldBudget) < len(c.cold) {
		c.coldBudget = append(c.coldBudget, noBudget)
	}
	if old := c.budget; old >= 0 {
		for i := c.budgetCold; i < len(c.cold); i++ {
			if c.coldBudget[i] == noBudget {
				c.coldBudget[i] = old
			}
		}
	}
	c.budget, c.budgetCold = b, len(c.cold)
}

// coldPath emits the i'th cold path, with the budget it was appended under
// (setBudget's coldBudget), the one in force after it.
func (c *a64Compiler) coldPath(i int) {
	if i >= len(c.coldBudget) || c.coldBudget[i] == noBudget {
		c.cold[i]()
		return
	}
	saved := c.budget
	c.budget = c.coldBudget[i]
	c.cold[i]()
	c.budget = saved
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
	// As amd64's: the frame's registers made here, the locals' address in
	// base.
	const base, at, top = a64Locals, arm64.Reg(5), arm64.Reg(6)
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
	// The high water as amd64's: raised past the frame only where the
	// context found it lower (abi.Context's HighSeen).
	same, raise := c.a.NewLabel(), c.a.NewLabel()
	c.a.Load(a64A, a64Ctx, abi.OffHighSeen)
	c.a.Cmp(top, a64A, true)
	c.a.BCond(arm64.HI, raise)
	c.cold = append(c.cold, func() {
		c.a.Bind(raise)
		kept := c.a.NewLabel()
		c.a.Load(at, a64Ctx, abi.OffStackHigh)
		c.a.Load(a64A, at, 0)
		c.a.Cmp(top, a64A, true)
		c.a.BCond(arm64.LS, kept)
		c.a.Store(at, 0, top)
		c.a.MovRR(a64A, top)
		c.a.Bind(kept)
		c.a.Store(a64Ctx, abi.OffHighSeen, a64A)
		c.a.B(same)
	})
	c.a.Bind(same)
	c.a.Load(at, a64Ctx, abi.OffStackBase)
	c.a.AddLsl(base, at, base, 4)
	c.a.Store(a64Ctx, abi.OffLocals, base)
	c.a.AddImm(a64Stack, base, int64(c.f.FrameLocals+c.f.Scratch)*int64(vs), true)
	c.a.Store(a64Ctx, abi.OffStack, a64Stack)
	c.a.Store(a64Ctx, abi.OffOpen, arm64.ZR)
	c.a.Store(a64Ctx, abi.OffCells, arm64.ZR)
	if c.f.Loops {
		c.a.Store(a64Ctx, abi.OffLoop, arm64.ZR)
	}
	c.storeCode(a64Ctx, at)
	c.a.MovImm(at, abi.LiveCall)
	c.a.Store(a64Ctx, abi.OffLive, at)
	c.a.Load(at, a64CallLink, c.enc.LinkIn)
	c.a.AddImm(at, at, 1, true)
	c.a.Store(a64CallLink, c.enc.LinkIn, at)
	if !c.f.StartOnly && c.f.Params < c.f.InitLocals() {
		c.movImm(a64A, c.enc.Undefined)
		for i := c.f.Params; i < c.f.InitLocals(); i++ {
			c.a.Store(base, int32(i)*vs+c.enc.NumOffset, a64A)
			c.a.Store(base, int32(i)*vs+c.enc.RefOffset, arm64.ZR)
		}
	}
}

// block emits one block. next is the block laid out after it.
func (c *a64Compiler) block(b *ssa.Block, next *ssa.Block) {
	c.setBudget(c.loopBudget(b))
	defer c.setBudget(-1)
	if f := c.flushIn(b); f >= 0 {
		// As amd64's: a loop's way out to a block of its own.
		c.budgetFlush(arm64.Reg(c.budgetReg(f)))
	}
	if b.PC < 0 {
		if b != c.entered {
			c.a.Load(a64Locals, a64Ctx, abi.OffLocals)
			c.a.Load(a64Stack, a64Ctx, abi.OffStack)
		}
		if c.f.StartOnly && c.f.Params < c.f.InitLocals() && b == c.entryAt(0) {
			// As amd64's: the frame's locals past its parameters, undefined.
			vs := int32(c.enc.ValueSize)
			c.movImm(a64A, c.enc.Undefined)
			for i := c.f.Params; i < c.f.InitLocals(); i++ {
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
		if reg, slot, split := c.split(v); split {
			// Its slot, which the frame states read (core's split).
			if isFloat(v) {
				c.spillStoreF(slot, arm64.FReg(reg))
			} else {
				c.spillStore(slot, arm64.Reg(reg))
			}
		}
	}
	switch b.Kind {
	case ssa.BlockPlain:
		c.edge(b, b.Succs[0], next)
	case ssa.BlockIf:
		c.branch(b, next)
	case ssa.BlockReturn:
		if !c.f.GenEntry {
			c.returnFast(b.Control)
		}
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
		if c.f.GenEntry {
			// As amd64's: a generator a loop's step resumed natively
			// finishes there.
			c.genReturn()
			c.returnNative()
			break
		}
		// As amd64's: to Go.
		c.a.Ret()
	case ssa.BlockExit:
		if b.Yield && c.f.GenEntry {
			c.yieldExit(b)
			break
		}
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
	from_, to_ := c.budget, c.loopBudget(to)
	if c.budgetReg(to_) < 0 {
		to_ = -1
	}
	defer c.setBudget(from_)
	if from_ >= 0 && from_ != to_ {
		// As amd64's: out of a budget loop, its register stored back.
		if c.flushIn(to) != from_ {
			c.budgetFlush(arm64.Reg(c.budgetReg(from_)))
		}
		c.setBudget(-1)
	}
	c.phiMoves(to, idx)
	if to.LoopHeader && to.Backedge[idx] && to_ >= 0 && to_ == from_ {
		// As amd64's: round a budget loop, its register counted.
		r := arm64.Reg(c.budgetReg(to_))
		poll, turned := c.a.NewLabel(), c.a.NewLabel()
		c.a.AddImm(r, r, -1, true)
		c.a.CmpImm(r, 0, true)
		c.a.BCond(arm64.LE, poll)
		c.a.Bind(turned)
		header := to.Header
		c.cold = append(c.cold, func() {
			saved := c.budget
			c.budget = -1
			defer func() { c.budget = saved }()
			c.a.Bind(poll)
			// As amd64's: -1, the register held nothing; the budget read.
			exhausted := c.a.NewLabel()
			c.a.CmpImm(r, -1, true)
			c.a.BCond(arm64.NE, exhausted)
			c.budgetLoad(r)
			c.a.AddImm(r, r, -1, true)
			c.a.CmpImm(r, 0, true)
			c.a.BCond(arm64.GT, turned)
			c.a.Bind(exhausted)
			c.budgetStore(r)
			if c.enc.CallGo != 0 {
				stub := c.stubLabel(header, abi.ExitPoll)
				c.goCalling(c.pollState(to, uint8(a64Locals), uint8(a64Stack)), &stub)
				c.budgetLoad(r)
				c.a.B(c.labels[to.ID])
				return
			}
			c.exitTo(header, abi.ExitPoll)
		})
	} else if to.LoopHeader && to.Backedge[idx] {
		poll := c.a.NewLabel()
		c.a.Load(a64B, a64Ctx, abi.OffBackEdges)
		c.a.Load(a64A, a64B, 0)
		c.a.AddImm(a64A, a64A, -1, true)
		c.a.Store(a64B, 0, a64A)
		c.a.CmpImm(a64A, 0, true)
		c.a.BCond(arm64.LE, poll)
		header := to.Header
		// As amd64's: back from the poll to what follows it on the edge.
		polled := c.a.NewLabel()
		c.a.Bind(polled)
		c.cold = append(c.cold, func() {
			c.a.Bind(poll)
			if c.enc.CallGo != 0 {
				// Go gives the loop another budget (ssa.GoPoll).
				stub := c.stubLabel(header, abi.ExitPoll)
				c.goCalling(c.pollState(to, uint8(a64Locals), uint8(a64Stack)), &stub)
				c.a.B(polled)
				return
			}
			c.exitTo(header, abi.ExitPoll)
		})
	}
	if to_ >= 0 && to_ != from_ {
		// As amd64's: into a budget loop, its register empty.
		c.budgetStart(arm64.Reg(c.budgetReg(to_)))
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

// cmpI32 is amd64's: the W registers compared, a constant the
// immediate, and the condition returned. It uses A and B.
func (c *a64Compiler) cmpI32(v *ssa.Value) arm64.Cond {
	op, x, y := ir.Operator(v.Aux&^ssa.CmpUnsigned), v.Args[0], v.Args[1]
	if x.Op == ssa.OpConstI32 && y.Op != ssa.OpConstI32 {
		x, y, op = y, x, mirrored(op)
	}
	if y.Op == ssa.OpConstI32 {
		c.a.CmpImm(c.gpr(x, a64A), int64(int32(uint32(y.Const.Bits))), false)
	} else {
		c.a.Cmp(c.gpr(x, a64A), c.gpr(y, a64B), false)
	}
	switch op {
	case ir.Eq:
		return arm64.EQ
	case ir.Ne:
		return arm64.NE
	}
	return a64CmpCond(op, v.Aux&ssa.CmpUnsigned != 0)
}

// a64Target is where a branch's test goes, as amd64's target: to yes
// where its value is true, to no where it is false; fall is the label
// bound right after the test's code, or a64NoLabel.
type a64Target struct{ yes, no, fall arm64.Label }

// a64NoLabel is a target's fall where no label follows the test.
const a64NoLabel = arm64.Label(-1)

// not is t for the negation of its value.
func (t a64Target) not() a64Target { return a64Target{t.no, t.yes, t.fall} }

// jumpIf goes to t.yes where cond holds, and to t.no where it does not:
// one jump where either follows.
func (c *a64Compiler) jumpIf(t a64Target, cond arm64.Cond) {
	switch {
	case t.fall == t.no:
		c.a.BCond(cond, t.yes)
	case t.fall == t.yes:
		c.a.BCond(cond.Invert(), t.no)
	default:
		c.a.BCond(cond, t.yes)
		c.a.B(t.no)
	}
}

// jumpIfZero goes to t.yes where r is zero, and to t.no where it is not.
func (c *a64Compiler) jumpIfZero(t a64Target, r arm64.Reg, wide bool) {
	switch {
	case t.fall == t.no:
		c.a.Cbz(r, t.yes, wide)
	case t.fall == t.yes:
		c.a.Cbnz(r, t.no, wide)
	default:
		c.a.Cbz(r, t.yes, wide)
		c.a.B(t.no)
	}
}

// jumpTo goes to l, one of t's, unless it follows.
func (c *a64Compiler) jumpTo(t a64Target, l arm64.Label) {
	if l != t.fall {
		c.a.B(l)
	}
}

// branch emits an If block's end, as amd64's does: the test of its
// control, which jumps to each successor's edge, or to the successor
// itself where the edge moves nothing; then the edges' code.
func (c *a64Compiler) branch(b *ssa.Block, next *ssa.Block) {
	type way struct {
		to     *ssa.Block
		label  arm64.Label
		direct bool
	}
	at := func(to *ssa.Block) way {
		if c.emptyEdge(b, to) {
			return way{to, c.labels[to.ID], true}
		}
		return way{to, c.a.NewLabel(), false}
	}
	yes, no := at(b.Succs[0]), at(b.Succs[1])
	ways := [2]way{no, yes}
	if no.to == next && yes.to != next {
		ways[0], ways[1] = yes, no
	}
	// As amd64's: an edge out of a loop the other stays in, with the cold
	// code.
	var out [2]bool
	for i, w := range ways {
		o := ways[1-i]
		out[i] = !w.direct && c.leaves(b, w.to, o.to) && (!o.direct || o.to == next)
	}
	fall := a64NoLabel
	switch {
	case !ways[0].direct && !out[0]:
		fall = ways[0].label
	case !ways[1].direct && !out[1]:
		fall = ways[1].label
	case next != nil:
		fall = c.labels[next.ID]
	}
	c.test(b.Control, a64Target{yes.label, no.label, fall})
	for i, w := range ways {
		if w.direct {
			continue
		}
		if out[i] {
			c.cold = append(c.cold, func() {
				c.a.Bind(w.label)
				c.budgetCold = len(c.cold)
				c.edge(b, w.to, nil)
			})
			continue
		}
		c.a.Bind(w.label)
		to := next
		if i == 0 && !ways[1].direct && !out[1] {
			to = nil
		}
		c.edge(b, w.to, to)
	}
}

// test emits a branch's test of v, a boolean, as amd64's does: a value
// the branch computes (core.fused) is compared here; any other is in its
// location.
func (c *a64Compiler) test(v *ssa.Value, t a64Target) {
	if !c.isFused(v) {
		c.jumpIfZero(t.not(), c.gpr(v, a64A), false)
		return
	}
	arg := func(i int) *ssa.Value { return v.Args[i] }
	switch v.Op {
	case ssa.OpNot:
		c.test(arg(0), t.not())
	case ssa.OpCmpF64:
		c.a.FCmp(c.fpr(arg(0), a64F0), c.fpr(arg(1), a64F1))
		c.jumpIf(t, a64Compare(ir.Operator(v.Aux)))
	case ssa.OpCmpI64:
		c.a.Cmp(c.gpr(arg(0), a64A), c.gpr(arg(1), a64B), true)
		c.jumpIf(t, a64CmpCond(ir.Operator(v.Aux), arg(0).Type == ssa.Uint64))
	case ssa.OpCmpI32:
		c.jumpIf(t, c.cmpI32(v))
	case ssa.OpStrictNullish:
		w := c.gpr(arg(0), a64B)
		word := c.enc.Null
		if v.Aux == 1 {
			word = c.enc.Undefined
		}
		if v.Aux == 2 {
			c.movImm(a64A, c.enc.Undefined)
			c.a.Cmp(w, a64A, true)
			c.a.BCond(arm64.EQ, t.yes)
		}
		c.movImm(a64A, word)
		c.a.Cmp(w, a64A, true)
		c.jumpIf(t, arm64.EQ)
	case ssa.OpCellNone:
		c.jumpIfZero(t, c.gpr(arg(0), a64B), true)
	case ssa.OpLooseNullish:
		c.looseNullish(v, c.guardFor(v), &t)
	case ssa.OpEqTagged:
		c.eqTagged(v, c.guardFor(v), &t)
	case ssa.OpTypeIs:
		c.typeIs(v, c.guardFor(v), &t)
	case ssa.OpKeyIs:
		c.keyIs(v, &t)
	case ssa.OpInstanceOf:
		c.instanceOf(v, c.guardFor(v), &t)
	case ssa.OpTruth:
		c.truth(v, c.guardFor(v), &t)
	default:
		panic("test " + v.Op.String())
	}
}

// tests is the target a test's code goes to, as amd64's is: to's, or
// labels of its own, the false case following the code, or the true one
// if yesFirst.
func (c *a64Compiler) tests(to *a64Target, yesFirst bool) a64Target {
	if to != nil {
		return *to
	}
	t := a64Target{c.a.NewLabel(), c.a.NewLabel(), a64NoLabel}
	t.fall = t.no
	if yesFirst {
		t.fall = t.yes
	}
	return t
}

// setBool ends a test's code that makes a boolean, as amd64's does: yes
// at t.yes, its negation at t.no, where no branch tests it (to nil).
func (c *a64Compiler) setBool(v *ssa.Value, to *a64Target, t a64Target, yes uint64) {
	if to != nil {
		return
	}
	done := c.a.NewLabel()
	first, second := t.no, t.yes
	if t.fall == t.yes {
		first, second = t.yes, t.no
	}
	word := func(l arm64.Label) uint64 {
		if l == t.yes {
			return yes
		}
		return yes ^ 1
	}
	c.a.Bind(first)
	c.movImm(a64A, word(first))
	c.a.B(done)
	c.a.Bind(second)
	c.movImm(a64A, word(second))
	c.a.Bind(done)
	c.setG(v, a64A)
}

// looseNullish is x == null, as amd64's: null's or undefined's word, or an
// object with [[IsHTMLDDA]].
func (c *a64Compiler) looseNullish(v *ssa.Value, guard a64Guard, to *a64Target) {
	a := v.Args[0]
	t := c.tests(to, false)
	w := c.gpr(a, a64B)
	for _, word := range []uint64{c.enc.Null, c.enc.Undefined} {
		c.movImm(a64A, word)
		c.a.Cmp(w, a64A, true)
		c.a.BCond(arm64.EQ, t.yes)
	}
	c.movImm(a64A, c.enc.Object)
	c.a.Cmp(w, a64A, true)
	c.a.BCond(arm64.NE, t.no)
	if c.reference(v, a, c.enc.Object, guard) {
		c.a.LoadU8(a64A, a64C, c.enc.ObjectFlags)
		c.movImm(a64B, uint64(c.enc.FlagHTMLDDA))
		c.a.Tst(a64A, a64B, false)
		c.jumpIf(t, arm64.NE)
	}
	c.setBool(v, to, t, 1)
}

// strictConst is amd64's (x === k of a constant k's word, or of an ASCII
// string constant's bytes). It uses A, B, C and D.
func (c *a64Compiler) strictConst(v *ssa.Value, guard a64Guard, to *a64Target) bool {
	if x, k, ok := eqConstOf(v); ok {
		t := c.tests(to, true)
		w := c.gpr(x, a64A)
		switch {
		case k.Kind == ir.Number && k.Bits&^(1<<63) > 0x7FF0000000000000:
			// NaN equals nothing.
			c.jumpTo(t, t.no)
		case k.Kind == ir.Number && k.Bits&^(1<<63) == 0:
			// 0 and -0, whose words differ by the sign alone.
			c.a.ShiftImm(arm64.Lsl, a64B, w, 1, true)
			c.jumpIfZero(t, a64B, true)
		default:
			c.movImm(a64B, c.constWord(k))
			c.a.Cmp(w, a64B, true)
			c.jumpIf(t, arm64.EQ)
		}
		c.setBool(v, to, t, 1)
		return true
	}
	x, k, ok := strConstOf(v)
	if !ok {
		return false
	}
	t := c.tests(to, true)
	yes, no := t.yes, t.no
	w := c.gpr(x, a64A)
	c.movImm(a64B, c.enc.String)
	c.a.Cmp(w, a64B, true)
	c.a.BCond(arm64.NE, no)
	if x.Shadow == nil && c.origin.At(x) < 0 {
		// A primitive, never a string's reference.
		c.jumpTo(t, no)
		c.setBool(v, to, t, 1)
		return true
	}
	if !c.reference(v, x, c.enc.String, guard) {
		c.setBool(v, to, t, 1)
		return true
	}
	c.a.Load(a64A, a64C, c.enc.StringLength)
	c.a.CmpImm(a64A, int64(len(k)), true)
	c.a.BCond(arm64.NE, no)
	if len(k) > 0 {
		rope := c.a.NewLabel()
		c.a.Load(a64A, a64C, c.enc.StringLeft)
		c.a.Cbnz(a64A, rope, true)
		c.a.Load(a64B, a64C, c.enc.StringData)
		chunk := func(at, size int) {
			var u uint64
			for i := size - 1; i >= 0; i-- {
				u = u<<8 | uint64(k[at+i])
			}
			switch size {
			case 8:
				c.a.Load(a64A, a64B, int32(at))
			case 4:
				c.a.LoadU32(a64A, a64B, int32(at))
			case 2:
				c.a.LoadU16(a64A, a64B, int32(at))
			default:
				c.a.LoadU8(a64A, a64B, int32(at))
			}
			c.movImm(a64D, u)
			c.a.Cmp(a64A, a64D, true)
			c.a.BCond(arm64.NE, no)
		}
		strBytes(len(k), chunk)
		c.jumpTo(t, yes)
		exit := c.stubLabel(v.State, exitKind(v.Aux))
		c.cold = append(c.cold, func() {
			c.a.Bind(rope)
			if c.enc.CallGo != 0 && exitKind(v.Aux) == abi.ExitHost {
				// Go compares them (ssa.GoEqual), strictly.
				size := int32(unsafe.Sizeof(abi.GoArg{}))
				c.goArg(abi.OffGoArgs, v.Args[0])
				c.goArg(abi.OffGoArgs+size, v.Args[1])
				c.goSaving(uint64(ssa.GoEqual)|1<<abi.GoOpIndexShift, &exit)
				c.a.Load(a64A, a64Ctx, abi.OffGoArgs+c.enc.NumOffset)
				c.a.Cbnz(a64A, yes, true)
				c.a.B(no)
				return
			}
			c.a.B(exit)
		})
	} else {
		c.jumpTo(t, yes)
	}
	c.setBool(v, to, t, 1)
	return true
}

// eqTagged is OpEqTagged, as amd64's.
func (c *a64Compiler) eqTagged(v *ssa.Value, guard a64Guard, to *a64Target) {
	if v.Index == 1 && c.strictConst(v, guard, to) {
		return
	}
	x, y := v.Args[0], v.Args[1]
	t := c.tests(to, true)
	yes, no := t.yes, t.no
	differ, number := c.a.NewLabel(), c.a.NewLabel()
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
	// EQ is false when unordered.
	c.jumpIf(t, arm64.EQ)
	c.setBool(v, to, t, 1)
}

// arrayOf finds the array a value is and checks its class.
func (c *a64Compiler) arrayOf(v *ssa.Value, guard a64Guard) {
	if !c.objectOf(v, guard) {
		return
	}
	c.a.LoadU8(a64B, a64C, c.enc.ObjectClass)
	c.a.CmpImm(a64B, int64(c.enc.ClassArray), false)
	if v.Index == ssa.ArrayOfArguments && c.enc.ClassArguments != 0 {
		// Or an unmapped arguments object, as amd64's.
		ok := c.a.NewLabel()
		c.a.BCond(arm64.EQ, ok)
		c.a.CmpImm(a64B, int64(c.enc.ClassArguments), false)
		guard.jump(arm64.NE)
		c.a.LoadU8(a64B, a64C, c.enc.ObjectFlags)
		c.movImm(a64D, uint64(c.enc.FlagMapped))
		c.a.Tst(a64B, a64D, false)
		guard.jump(arm64.NE)
		c.a.Bind(ok)
		c.setG(v, a64C)
		return
	}
	guard.jump(arm64.NE)
	c.setG(v, a64C)
}

// objectOf finds the object a value is, into C.
// instanceOf is amd64's: whether its value has the object its second
// argument is among its prototypes (OpInstanceOf). It uses A, B and C.
func (c *a64Compiler) instanceOf(v *ssa.Value, guard a64Guard, to *a64Target) {
	a := v.Args[0]
	if a.Shadow == nil && c.origin.At(a) < 0 {
		if to != nil {
			c.jumpTo(*to, to.no)
			return
		}
		c.movImm(a64B, 0)
		c.setG(v, a64B)
		return
	}
	no, loop := c.a.NewLabel(), c.a.NewLabel()
	if to != nil {
		no = to.no
	}
	w := c.gpr(a, a64A)
	c.movImm(a64B, c.enc.Object)
	c.a.Cmp(w, a64B, true)
	c.a.BCond(arm64.NE, no)
	c.sourceRef(a, guard)
	c.a.CmpImm(a64C, 0, true)
	guard.jump(arm64.EQ)
	p := c.gpr(v.Args[1], a64A)
	c.a.Bind(loop)
	c.a.LoadU8(a64B, a64C, c.enc.ObjectClass)
	c.a.CmpImm(a64B, int64(c.enc.ClassProxy), false)
	guard.jump(arm64.EQ)
	c.loadProto(a64C, a64C)
	c.a.Cbz(a64C, no, true)
	c.a.Cmp(a64C, p, true)
	c.a.BCond(arm64.NE, loop)
	if to != nil {
		c.jumpTo(*to, to.yes)
		return
	}
	done := c.a.NewLabel()
	c.movImm(a64B, 1)
	c.a.B(done)
	c.a.Bind(no)
	c.movImm(a64B, 0)
	c.a.Bind(done)
	c.setG(v, a64B)
}

// primitive is amd64's (OpPrimitive). It uses A and B.
func (c *a64Compiler) primitive(v *ssa.Value, guard a64Guard) {
	ok := c.a.NewLabel()
	w := c.gpr(v.Args[0], a64B)
	c.movImm(a64A, abi.NumberLimit)
	c.a.Cmp(w, a64A, true)
	c.a.BCond(arm64.LO, ok)
	for _, word := range []uint64{c.enc.String, c.enc.True, c.enc.False, c.enc.Null} {
		c.movImm(a64A, word)
		c.a.Cmp(w, a64A, true)
		c.a.BCond(arm64.EQ, ok)
	}
	c.movImm(a64A, c.enc.Undefined)
	c.a.Cmp(w, a64A, true)
	guard.jump(arm64.NE)
	c.a.Bind(ok)
}

// typeIs is amd64's (OpTypeIs). It uses A, B and C.
func (c *a64Compiler) typeIs(v *ssa.Value, guard a64Guard, to *a64Target) {
	a := v.Args[0]
	t := c.tests(to, false)
	// it is the type's, as amd64's.
	it := t
	if to != nil && v.Const.Bits == 1 {
		it = t.not()
	}
	yes, no := it.yes, it.no
	w := c.gpr(a, a64B)
	cmp := func(word uint64) {
		c.movImm(a64A, word)
		c.a.Cmp(w, a64A, true)
	}
	is := func(word uint64) {
		cmp(word)
		c.a.BCond(arm64.EQ, yes)
	}
	switch v.Index {
	case ir.TypeNumber:
		cmp(abi.NumberLimit)
		c.jumpIf(it, arm64.LO)
	case ir.TypeString:
		cmp(c.enc.String)
		c.jumpIf(it, arm64.EQ)
	case ir.TypeBoolean:
		is(c.enc.True)
		cmp(c.enc.False)
		c.jumpIf(it, arm64.EQ)
	case ir.TypeSymbol:
		cmp(c.enc.Symbol)
		c.jumpIf(it, arm64.EQ)
	case ir.TypeBigInt:
		cmp(c.enc.HeapBigTag)
		c.jumpIf(it, arm64.HS)
	case ir.TypeNone:
		c.jumpTo(it, no)
	case ir.TypeUndefined, ir.TypeFunction, ir.TypeObject:
		switch v.Index {
		case ir.TypeUndefined:
			is(c.enc.Undefined)
		case ir.TypeObject:
			is(c.enc.Null)
		}
		cmp(c.enc.Object)
		c.a.BCond(arm64.NE, no)
		if c.reference(v, a, c.enc.Object, guard) {
			c.a.LoadU8(a64A, a64C, c.enc.ObjectFlags)
			c.movImm(a64B, uint64(c.enc.FlagHTMLDDA))
			c.a.Tst(a64A, a64B, false)
			if v.Index == ir.TypeUndefined {
				c.jumpIf(it, arm64.NE)
			} else {
				c.a.BCond(arm64.NE, no)
				fn, obj := yes, no
				if v.Index == ir.TypeObject {
					fn, obj = no, yes
				}
				c.a.LoadU8(a64A, a64C, c.enc.ObjectClass)
				c.a.CmpImm(a64A, int64(c.enc.ClassFunction), false)
				c.a.BCond(arm64.EQ, fn)
				c.a.CmpImm(a64A, int64(c.enc.ClassProxy), false)
				guard.jump(arm64.EQ)
				c.jumpTo(it, obj)
			}
		}
	}
	c.setBool(v, to, t, v.Const.Bits^1)
}

func (c *a64Compiler) objectOf(v *ssa.Value, guard a64Guard) bool {
	return c.reference(v, v.Args[0], c.enc.Object, guard)
}

// reference finds the reference a is, of the kind whose word is word, into
// C, as amd64's does. It uses A and B.
func (c *a64Compiler) reference(v, a *ssa.Value, word uint64, guard a64Guard) bool {
	return c.referenceTo(v, a, word, guard, c.stubLabel(v.State, exitKind(v.Aux)))
}

// referenceTo is reference failing at fail, as guard does.
func (c *a64Compiler) referenceTo(v, a *ssa.Value, word uint64, guard a64Guard, fail arm64.Label) bool {
	o := c.origin.At(a)
	if a.Shadow == nil && o < 0 {
		c.a.B(fail)
		return false
	}
	w := c.gpr(a, a64A)
	c.movImm(a64B, word)
	c.a.Cmp(w, a64B, true)
	guard.jump(arm64.NE)
	c.sourceRef(a, guard)
	c.a.Cbz(a64C, fail, true)
	return true
}

// sourceRef loads the pointer word of a, a value with a source, into C, as
// amd64's does. It uses A and B.
func (c *a64Compiler) sourceRef(a *ssa.Value, guard a64Guard) {
	o := c.origin.At(a)
	if s := a.Shadow; s != nil && cellSource(s) {
		c.a.Load(a64C, c.gpr(s, a64C), c.enc.RefOffset)
	} else if s != nil {
		r := c.gpr(s, a64C)
		c.a.CmpImm(r, 0, true)
		guard.jump(arm64.EQ)
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
	c.a.CmpImm(a64C, int64(c.slotsEnd()), true)
	c.a.BCond(arm64.HS, found)
	c.a.CmpImm(a64C, int64(c.f.FrameLocals), true)
	c.a.BCond(arm64.HS, captured)
	c.a.AddLsl(a64C, a64Locals, a64C, 4)
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
	c.a.AddLsl(a64C, a64Stack, a64C, 4)
	c.a.Bind(found)
}

// propStore stores a value in a property, as amd64's does. It uses every
// scratch register and F1.
func (c *a64Compiler) propStore(v *ssa.Value, guard a64Guard) {
	var miss arm64.Label
	if v.Type == ssa.Bool {
		miss = c.a.NewLabel()
		if o := v.Args[0]; o.Op == ssa.OpObjectOf && o.Index == ssa.ObjectOfTry {
			// As amd64's: a receiver that is no object is a miss.
			c.a.Cbz(c.gpr(o, a64B), miss, true)
		}
	}
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
		guard.jump(arm64.NE)
	} else if v.Type == ssa.Bool {
		// As amd64's: a megamorphic site's write whose misses Go makes.
		c.property(v, a64GuardLabel(c, miss))
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

// refStoringCall is amd64's: back to the site at once for a nil over a nil
// alone -- a pointer stored over itself goes through the barrier, which may
// be what shades it (amd64's refStoringCall says why); or every allocatable
// register saved, storeRef called, which comes back where B says, with the
// link register; the frame's registers read again, every allocatable one
// restored, and back to the site.
func (c *a64Compiler) refStoringCall() {
	none := c.a.NewLabel()
	c.a.Load(a64B, a64A, 0)
	c.a.Op(arm64.Orr, a64B, a64B, a64C, true)
	c.a.Cbz(a64B, none, true)
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
	c.a.Bind(none)
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
	reg := c.budgetReg(c.budget)
	if reg >= 0 {
		c.budgetFlush(arm64.Reg(reg))
	}
	c.a.Adr(a64A, back)
	c.a.Store(a64Ctx, abi.OffGoSite, a64A)
	c.a.B(c.appending)
	c.a.Bind(back)
	if reg >= 0 {
		c.budgetStart(arm64.Reg(reg))
	}
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
	fail := a64GuardLabel(c, toGo)
	// The key, an index in [0, 2**32), as index makes it, and the array's
	// length.
	c.a.Load(a64A, a64Ctx, abi.OffGoArgs+size+c.enc.NumOffset)
	c.a.FMovToF(a64F0, a64A)
	c.a.Fcvtzs(a64A, a64F0)
	c.a.Scvtf(a64F1, a64A, true)
	c.a.FCmp(a64F1, a64F0)
	fail.jump(arm64.NE)
	c.a.ShiftImm(arm64.Lsr, a64B, a64A, 32, true)
	c.a.CmpImm(a64B, 0, true)
	fail.jump(arm64.NE)
	c.a.Load(a64C, a64Ctx, abi.OffGoArgs+c.enc.RefOffset)
	c.a.Load(a64B, a64C, c.enc.ObjectElems+8)
	c.a.Cmp(a64A, a64B, true)
	hole := c.a.NewLabel()
	c.a.BCond(arm64.LO, hole)
	fail.jump(arm64.NE)
	c.a.Load(a64B, a64C, c.enc.ObjectElems+16)
	c.a.Cmp(a64A, a64B, true)
	fail.jump(arm64.HS)
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
	c.a.AddLsl(a64A, a64B, a64A, 4)
	c.a.Load(a64B, a64A, c.enc.NumOffset)
	c.movImm(a64D, c.enc.Uninitialized)
	c.a.Cmp(a64B, a64D, true)
	fail.jump(arm64.NE)
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
func (c *a64Compiler) arrayGuards(p arm64.Reg, protos [2]ssa.Holder, fail a64Guard) {
	c.a.LoadU8(a64B, p, c.enc.ObjectClass)
	c.a.CmpImm(a64B, int64(c.enc.ClassArray), false)
	fail.jump(arm64.NE)
	c.a.LoadU8(a64B, p, c.enc.ObjectFlags)
	want := c.enc.FlagExtensible | c.enc.FlagLengthWritable
	c.movImm(a64A, uint64(want|c.enc.FlagSparse|c.enc.FlagShared))
	c.a.Op(arm64.And, a64B, a64B, a64A, false)
	c.a.CmpImm(a64B, int64(want), false)
	fail.jump(arm64.NE)
	c.loadProtoPlain(a64B, p)
	c.movImm(a64A, uint64(protos[0].Object))
	c.a.Cmp(a64B, a64A, true)
	fail.jump(arm64.NE)
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
			c.spillStoreF(sv.slot, arm64.FReg(sv.reg))
		} else {
			c.spillStore(sv.slot, arm64.Reg(sv.reg))
		}
	}
	back, fail := c.a.NewLabel(), c.a.NewLabel()
	target := c.enc.GoFast
	if s := c.goStub(st); s != 0 {
		target = s
	}
	direct := target != 0 && !c.goCallBig
	c.movImm(a64C, uint64(uintptr(unsafe.Pointer(st))))
	c.a.Adr(a64A, back)
	if direct {
		// As amd64's: through the VM's routine for calls of Go, GoFast or
		// the operation's own.
		c.movImm(a64B, target)
		c.a.Br(a64B)
	} else {
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
		c.a.B(*routine)
	}
	c.a.Bind(back)
	restore := func(refused bool) {
		if direct {
			c.a.Load(a64Locals, a64Ctx, abi.OffLocals)
			c.a.Load(a64Stack, a64Ctx, abi.OffStack)
		}
		for _, sv := range saves {
			switch {
			case sv.cold && !refused:
				// Read after the call by its refusal alone.
			case sv.float:
				c.spillLoadF(arm64.FReg(sv.reg), sv.slot)
			default:
				c.spillLoad(arm64.Reg(sv.reg), sv.slot)
			}
		}
	}
	restore(false)
	c.goFails = append(c.goFails, a64GoFail{st, back, fail})
	c.cold = append(c.cold, func() {
		c.a.Bind(fail)
		restore(true)
		c.a.B(stub)
	})
	if catch := c.goCatch; catch != 0 {
		// As amd64's: a throw the frame's handler takes, the registers
		// restored, to its continuation.
		at := c.a.NewLabel()
		c.goCatches = append(c.goCatches, a64GoFail{st, back, at})
		c.cold = append(c.cold, func() {
			c.a.Bind(at)
			restore(true)
			c.a.B(catch)
		})
	}
}

// goLive is amd64's: Go called for v, what lives across it saved.
func (c *a64Compiler) goLive(v *ssa.Value, op uint64, stub arm64.Label) {
	if c.enc.GoFast == 0 || c.goCallBig {
		c.goSaving(op, &stub)
		return
	}
	st, _ := c.goState(op, 0, nil, nil)
	saves := c.savesOf(v)
	for _, sv := range saves {
		if sv.float {
			c.spillStoreF(sv.slot, arm64.FReg(sv.reg))
		} else {
			c.spillStore(sv.slot, arm64.Reg(sv.reg))
		}
	}
	reg := c.budgetReg(c.budget)
	if reg >= 0 {
		c.budgetFlush(arm64.Reg(reg))
	}
	back, fail := c.a.NewLabel(), c.a.NewLabel()
	c.movImm(a64C, uint64(uintptr(unsafe.Pointer(st))))
	c.a.Adr(a64A, back)
	c.movImm(a64B, c.enc.GoFast)
	c.a.Br(a64B)
	c.a.Bind(back)
	restore := func(refused bool) {
		c.a.Load(a64Locals, a64Ctx, abi.OffLocals)
		c.a.Load(a64Stack, a64Ctx, abi.OffStack)
		for _, sv := range saves {
			switch {
			case sv.cold && !refused:
				// What its state reads, read by its refusal's exit alone.
			case sv.float:
				c.spillLoadF(arm64.FReg(sv.reg), sv.slot)
			default:
				c.spillLoad(arm64.Reg(sv.reg), sv.slot)
			}
		}
	}
	restore(false)
	if reg >= 0 {
		c.budgetStart(arm64.Reg(reg))
	}
	to := stub
	if real, ok := c.budgetReal[to]; ok {
		to = real
	}
	c.goFails = append(c.goFails, a64GoFail{st, back, fail})
	c.cold = append(c.cold, func() {
		c.a.Bind(fail)
		restore(true)
		c.a.B(to)
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
	reg := c.budgetReg(c.budget)
	if reg >= 0 {
		// As amd64's: the budget stored back for Go, and read again.
		c.budgetFlush(arm64.Reg(reg))
	}
	c.movImm(a64C, uint64(uintptr(unsafe.Pointer(st))))
	c.a.Adr(a64A, back)
	c.a.B(c.calling)
	c.a.Bind(back)
	if reg >= 0 {
		c.budgetStart(arm64.Reg(reg))
	}
	if stub != nil {
		to := *stub
		if real, ok := c.budgetReal[to]; ok {
			to = real
		}
		c.goFails = append(c.goFails, a64GoFail{st, back, to})
	}
	if c.goCatch != 0 {
		// As amd64's: a throw the frame's handler takes to its
		// continuation, the routine having restored the registers.
		c.goCatches = append(c.goCatches, a64GoFail{st, back, c.goCatch})
	}
}

// catchStub is amd64's: the continuation of v, a call of Go of the code's
// own frame, at the handler in force at it, or 0.
func (c *a64Compiler) catchStub(v *ssa.Value) arm64.Label {
	f, s := c.f, v.State
	if s == nil || s.Inline != nil || s.Site < 0 || s.Site >= len(f.HandlerAt) || f.HandlerAt[s.Site] == 0 ||
		f.Virtuals != 0 || f.Unmade != 0 || f.Called != 0 || f.Kinds != nil || f.Cells {
		return 0
	}
	h := f.Handlers[f.HandlerAt[s.Site]-1]
	var to *ssa.Block
	for _, e := range f.Entries {
		if e.PC == int(h.PC) {
			to = e.Block
			break
		}
	}
	if to == nil || h.Reg < 0 || int(h.Reg) >= f.FrameLocals {
		return 0
	}
	l := c.a.NewLabel()
	c.catches = append(c.catches, a64CatchStub{state: s, label: l, keep: v.Index, reg: int(h.Reg), to: to})
	if c.budget < 0 {
		return l
	}
	flush, reg := c.a.NewLabel(), arm64.Reg(c.budgetReg(c.budget))
	c.cold = append(c.cold, func() {
		c.a.Bind(flush)
		c.budgetFlush(reg)
		c.a.B(l)
	})
	return flush
}

// catchTo is amd64's: k's continuation, the frame written (exitThen), what
// was thrown stored to the handler's register, its pointer word with Go's
// write barrier while the collector marks, and on at the handler's entry.
func (c *a64Compiler) catchTo(k a64CatchStub) {
	then := c.a.NewLabel()
	c.exitThen(k.state, abi.ExitHost, &then)
	c.a.Bind(then)
	keep := abi.OffKeep + int32(k.keep)*int32(c.enc.ValueSize)
	c.a.Load(a64B, a64Ctx, keep+c.enc.NumOffset)
	base, disp := c.slotAddr(k.reg, false, a64A)
	c.a.Store(base, disp, a64B)
	marked, done := c.a.NewLabel(), c.a.NewLabel()
	c.movImm(a64B, c.enc.WriteBarrier)
	c.a.LoadU8(a64B, a64B, 0)
	c.a.Cbnz(a64B, marked, false)
	for _, marking := range []bool{false, true} {
		if marking {
			c.a.Bind(marked)
		}
		c.a.Load(a64C, a64Ctx, keep+c.enc.RefOffset)
		base, disp := c.slotAddr(k.reg, true, a64A)
		c.a.AddImm(a64A, base, int64(disp), true)
		c.ptrStore(marking)
		c.a.B(done)
	}
	c.a.Bind(done)
	c.a.B(c.labels[k.to.ID])
}

// byCode is amd64's: the callee's function checked, its pointer word in C,
// and the object, its closure and its captured bindings kept in the
// context. It uses A, B and C.
func (c *a64Compiler) byCode(t *ssa.CallSite, next arm64.Label) {
	// A miss has the callee in C again, for the next site to check.
	miss, done := c.a.NewLabel(), c.a.NewLabel()
	c.a.Store(a64Ctx, abi.OffCallCallee, a64C)
	c.objectData(a64B, a64C, a64C, c.enc.FuncData, a64GuardLabel(c, miss))
	c.a.Load(a64B, a64B, c.enc.FuncClosure)
	c.a.Cbz(a64B, miss, true)
	c.a.Load(a64A, a64B, c.enc.ClosureFn)
	c.movImm(a64C, uint64(t.ByCode))
	c.a.Cmp(a64A, a64C, true)
	c.a.BCond(arm64.NE, miss)
	// Its environment, its template's, and its realm.
	for _, f := range [...]struct {
		off  int32
		code bool
		want uintptr
	}{{c.enc.CodeEnv, true, t.Env}, {c.enc.ClosureRealm, false, t.Realm}} {
		from := a64B
		if f.code {
			c.a.Load(a64A, a64B, c.enc.ClosureCode)
			from = a64A
		}
		c.a.Load(a64A, from, f.off)
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
func (c *a64Compiler) superGuards(t *ssa.CallSite, x *ssa.Value, guard a64Guard) {
	c.a.Load(a64A, a64Ctx, abi.OffFunction)
	c.movImm(a64B, uint64(t.Active))
	c.a.Cmp(a64A, a64B, true)
	guard.jump(arm64.NE)
	c.loadProtoPlain(a64A, a64A)
	c.movImm(a64B, uint64(t.Callee))
	c.a.Cmp(a64A, a64B, true)
	guard.jump(arm64.NE)
	if t.Parent != 0 {
		c.loadProtoPlain(a64A, a64A)
		c.movImm(a64B, uint64(t.Parent))
		c.a.Cmp(a64A, a64B, true)
		guard.jump(arm64.NE)
	}
	w := a64A
	if remat(x) {
		c.materialize(x, a64A)
	} else {
		w = c.gpr(x, a64A)
	}
	c.movImm(a64B, c.enc.Uninitialized)
	c.a.Cmp(w, a64B, true)
	guard.jump(arm64.NE)
}

// arrowBound is amd64's.
func (c *a64Compiler) arrowBound(ref, extra arm64.Reg, miss, done arm64.Label) {
	shared := c.a.NewLabel()
	c.a.Cbnz(ref, shared, true)
	c.a.AddImm(extra, extra, int64(c.enc.ExtraLexThis), true)
	c.a.Store(a64Ctx, abi.OffCallThis, extra)
	c.a.B(done)
	c.a.Bind(shared)
	c.a.LoadU8(extra, ref, c.enc.ThisBindingInit)
	c.a.Cbz(extra, miss, false)
	c.a.AddImm(ref, ref, int64(c.enc.ThisBindingValue), true)
	c.a.Store(a64Ctx, abi.OffCallThis, ref)
	c.a.B(done)
}

func (c *a64Compiler) arrowThis(t *ssa.CallSite, next arm64.Label) {
	miss, done := c.a.NewLabel(), c.a.NewLabel()
	if t.ByCode == 0 {
		c.a.Store(a64Ctx, abi.OffCallCallee, a64C)
	}
	c.a.Load(a64B, a64Ctx, abi.OffCallCallee)
	c.a.Load(a64B, a64B, c.enc.ObjectExt+c.enc.ObjectData+8)
	c.a.Load(a64B, a64B, c.enc.FuncExtra)
	c.a.Cbz(a64B, miss, true)
	c.a.Load(a64A, a64B, c.enc.ExtraLexThisRef)
	c.arrowBound(a64A, a64B, miss, done)
	c.a.Bind(miss)
	c.a.Load(a64C, a64Ctx, abi.OffCallCallee)
	c.a.B(next)
	c.a.Bind(done)
}

// mega is amd64's: v's property looked up by the probe the code's sites
// share (megaProbe), its value's address in A, or a failure.
func (c *a64Compiler) mega(v *ssa.Value, guard a64Guard, found arm64.Label) {
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
	guard.jump(arm64.EQ)
	c.a.B(found)
}

// megaProbe is amd64's: the probe a function's sites share, the receiver in
// A, back with the value's address in A, or 0 -- a write's addition too
// (megaAdd). It uses every scratch register, F2, and for a read F1: a
// write's value's pointer word waits there.
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
	var add arm64.Label
	if write {
		c.movImm(a64B, abi.MegaWritable)
		c.a.Tst(a64C, a64B, false)
		c.a.BCond(arm64.EQ, fail)
		if c.enc.ObjectPropsCap != 0 && c.enc.FlagExtensible != 0 && c.enc.ValueSize == 16 {
			// As amd64's: writable and absent, an addition.
			add = c.a.NewLabel()
			c.movImm(a64B, abi.MegaAbsent)
			c.a.Tst(a64C, a64B, false)
			c.a.BCond(arm64.NE, add)
		}
	} else {
		// As amd64's: a prototype's, along the chain, the last the holder;
		// or, absent, all of them, the last's prototype none. D holds the
		// object, B the link's word to compare.
		own, loop, holder, absent, end := c.a.NewLabel(), c.a.NewLabel(), c.a.NewLabel(), c.a.NewLabel(), c.a.NewLabel()
		c.a.FMovToF(a64F1, a64C)
		c.a.Load(a64A, a64A, abi.OffMegaChain)
		c.a.Cbz(a64A, own, true)
		c.a.FMovFromF(a64D, a64F2)
		c.loadProto(a64D, a64D)
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
		c.loadProto(a64D, a64D)
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
		c.loadProto(a64D, a64D)
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
	if add != 0 {
		c.a.Bind(add)
		c.megaAdd(fail)
	}
	c.a.Bind(fail)
	c.movImm(a64A, 0)
	back()
}

// megaAdd is amd64's: the entry's address in A, the receiver in F2, the
// write's value's pointer word in F1, which it keeps; the entry's address,
// and while the collector marks the value's and the pointer word, wait in
// the context's GoArgs. D holds the object, B the link's word to compare.
func (c *a64Compiler) megaAdd(fail arm64.Label) {
	absent, end, marked := c.a.NewLabel(), c.a.NewLabel(), c.a.NewLabel()
	back := func() {
		c.a.Load(a64B, a64Ctx, abi.OffProbeBack)
		c.a.Br(a64B)
	}
	c.a.Store(a64Ctx, abi.OffGoArgs+c.enc.RefOffset, a64A)
	c.a.Load(a64A, a64A, abi.OffMegaChain)
	c.a.FMovFromF(a64D, a64F2)
	c.loadProto(a64D, a64D)
	c.a.Bind(absent)
	c.a.Load(a64B, a64A, abi.OffLinkObject)
	c.a.Cbz(a64B, end, true)
	c.a.Cmp(a64D, a64B, true)
	c.a.BCond(arm64.NE, fail)
	c.a.Load(a64C, a64D, c.enc.ObjectShape)
	c.a.Load(a64B, a64A, abi.OffLinkShape)
	c.a.Cmp(a64C, a64B, true)
	c.a.BCond(arm64.NE, fail)
	c.loadProto(a64D, a64D)
	c.a.AddImm(a64A, a64A, int64(abi.MegaLinkSize), true)
	c.a.B(absent)
	c.a.Bind(end)
	c.a.Cbnz(a64D, fail, true)
	// Extensible, with room, as addAlong checks it.
	c.a.FMovFromF(a64D, a64F2)
	c.a.LoadU8(a64B, a64D, c.enc.ObjectFlags)
	c.movImm(a64C, uint64(c.enc.FlagExtensible))
	c.a.Tst(a64B, a64C, false)
	c.a.BCond(arm64.EQ, fail)
	c.a.LoadU32(a64B, a64D, c.enc.ObjectPropsLen)
	c.a.LoadU32(a64C, a64D, c.enc.ObjectPropsCap)
	c.a.Cmp(a64B, a64C, true)
	c.a.BCond(arm64.HS, fail)
	c.movImm(a64A, c.enc.WriteBarrier)
	c.a.LoadU8(a64A, a64A, 0)
	c.a.Cbnz(a64A, marked, false)
	// The length, then the shape, then the entry's address.
	c.a.AddImm(a64C, a64B, 1, true)
	c.a.Store32(a64D, c.enc.ObjectPropsLen, a64C)
	c.a.Load(a64C, a64Ctx, abi.OffGoArgs+c.enc.RefOffset)
	c.a.Load(a64C, a64C, abi.OffMegaNext)
	c.a.Store(a64D, c.enc.ObjectShape, a64C)
	c.a.ShiftImm(arm64.Lsl, a64B, a64B, 4, true)
	c.a.Load(a64A, a64D, c.enc.ObjectProps)
	c.a.Op(arm64.Add, a64A, a64A, a64B, true)
	back()
	c.a.Bind(marked)
	c.a.AddImm(a64C, a64B, 1, true)
	c.a.Store32(a64D, c.enc.ObjectPropsLen, a64C)
	c.a.ShiftImm(arm64.Lsl, a64B, a64B, 4, true)
	c.a.Load(a64C, a64D, c.enc.ObjectProps)
	c.a.Op(arm64.Add, a64C, a64C, a64B, true)
	c.a.Store(a64Ctx, abi.OffGoArgs+c.enc.NumOffset, a64C)
	c.a.FMovFromF(a64B, a64F1)
	c.a.Store(a64Ctx, abi.OffGoArgs+c.enc.ValueSize+c.enc.NumOffset, a64B)
	c.a.AddImm(a64A, a64D, int64(c.enc.ObjectShape), true)
	c.a.Load(a64C, a64Ctx, abi.OffGoArgs+c.enc.RefOffset)
	c.a.Load(a64C, a64C, abi.OffMegaNext)
	c.refStore()
	c.a.Load(a64B, a64Ctx, abi.OffGoArgs+c.enc.ValueSize+c.enc.NumOffset)
	c.a.FMovToF(a64F1, a64B)
	c.a.Load(a64A, a64Ctx, abi.OffGoArgs+c.enc.NumOffset)
	back()
}

// genericCheck is amd64's: call v, of a function none of its sites is, the
// callee in C, made through the function's link, or miss.
func (c *a64Compiler) genericCheck(v *ssa.Value, site *ssa.CallSite, target int, checked, miss arm64.Label) {
	c.a.Store(a64Ctx, abi.OffCallCallee, a64C)
	c.objectData(a64B, a64C, a64C, c.enc.FuncData, a64GuardLabel(c, miss))
	c.a.Load(a64B, a64B, c.enc.FuncClosure)
	c.a.Cbz(a64B, miss, true)
	// Its environment, its template's, and its realm.
	for _, f := range [...]struct {
		off  int32
		code bool
		want uintptr
	}{{c.enc.CodeEnv, true, site.GenericEnv}, {c.enc.ClosureRealm, false, site.GenericRealm}} {
		from := a64B
		if f.code {
			c.a.Load(a64A, a64B, c.enc.ClosureCode)
			from = a64A
		}
		c.a.Load(a64A, from, f.off)
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
	c.a.Load(a64B, a64B, c.enc.ObjectExt+c.enc.ObjectData+8)
	c.a.Load(a64B, a64B, c.enc.FuncExtra)
	c.a.Cbz(a64B, miss, true)
	c.a.Load(a64C, a64B, c.enc.ExtraLexThisRef)
	c.arrowBound(a64C, a64B, miss, notArrow)
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
	if site.Window != 0 {
		// As amd64's: the arguments the call writes past the window.
		c.a.Load(a64A, a64Ctx, abi.OffStackTop)
		c.a.Load(a64A, a64A, 0)
		c.a.AddImm(a64A, a64A, int64(2+site.Argc), true)
		c.a.Cmp(a64A, a64B, true)
		c.a.BCond(arm64.HI, miss)
	}
	c.movImm(a64B, uint64(target))
	c.a.Store(a64Ctx, abi.OffCallTarget, a64B)
	c.a.B(checked)
}

// tableCheck is amd64's: typed code's call through a function table, the
// callee in C, made through the link of the entry its index found.
func (c *a64Compiler) tableCheck(v *ssa.Value, site *ssa.CallSite, target int, checked, miss arm64.Label) {
	c.a.Store(a64Ctx, abi.OffCallCallee, a64C)
	c.a.Load(a64B, a64Ctx, abi.OffCallTable)
	c.a.Load(a64A, a64B, abi.OffTableCallee)
	c.a.Cmp(a64A, a64C, true)
	c.a.BCond(arm64.NE, miss)
	c.a.Load(a64A, a64B, abi.OffTableLink)
	c.a.Cbz(a64A, miss, true)
	c.a.Store(a64Ctx, abi.OffCallLink, a64A)
	c.a.Load(a64A, a64A, c.enc.LinkFlags)
	c.movImm(a64C, abi.LinkNotGeneric|abi.LinkArrow|abi.LinkCoerce)
	c.a.Tst(a64A, a64C, true)
	c.a.BCond(arm64.NE, miss)
	c.a.Load(a64A, a64B, abi.OffTableClosure)
	c.a.Store(a64Ctx, abi.OffCallClosure, a64A)
	c.a.Load(a64A, a64B, abi.OffTableUpvalues)
	c.a.Store(a64Ctx, abi.OffCallUpvalues, a64A)
	c.a.Load(a64A, a64Ctx, abi.OffStackTop)
	c.a.Load(a64A, a64A, 0)
	c.a.Load(a64C, a64Ctx, abi.OffCallLink)
	c.a.Load(a64B, a64C, c.enc.LinkFrame)
	c.a.Op(arm64.Add, a64A, a64A, a64B, true)
	c.a.Load(a64B, a64Ctx, abi.OffStackEnd)
	c.a.Cmp(a64A, a64B, true)
	c.a.BCond(arm64.HI, miss)
	if site.Window != 0 {
		c.a.Load(a64A, a64Ctx, abi.OffStackTop)
		c.a.Load(a64A, a64A, 0)
		c.a.AddImm(a64A, a64A, int64(2+site.Argc), true)
		c.a.Cmp(a64A, a64B, true)
		c.a.BCond(arm64.HI, miss)
	}
	c.movImm(a64B, uint64(target))
	c.a.Store(a64Ctx, abi.OffCallTarget, a64B)
	c.a.B(checked)
}

// objectPart is amd64's: what an object of class cls keeps off bytes
// past it, into dst. It uses t too, which must not be obj: the class read
// into it had made obj the class's number, and dst a field of address 21.
func (c *a64Compiler) objectPart(dst, obj, t arm64.Reg, cls uint8, off int32, fail a64Guard) {
	if t == obj {
		panic("mir: objectPart's scratch register is its object's")
	}
	c.a.LoadU8(t, obj, c.enc.ObjectClass)
	c.a.CmpImm(t, int64(cls), false)
	fail.jump(arm64.NE)
	c.a.AddImm(dst, obj, int64(off), true)
}

// loadProtoPlain is amd64's: loadProto without the test of FlagPoly.
func (c *a64Compiler) loadProtoPlain(dst, obj arm64.Reg) {
	none := c.a.NewLabel()
	c.a.Load(dst, obj, c.enc.ObjectShape)
	c.a.Cbz(dst, none, true)
	c.a.Load(dst, dst, c.enc.ShapeProto)
	c.a.Bind(none)
}

// loadProto is amd64's: dst the prototype of the object in obj, which may
// be dst, nil for one with no shape, or, for one that keeps it in its first
// property (FlagPoly), that value's pointer word: the flag's bit isolated
// in the encoder's temporary, which no caller keeps a value in, by two
// shifts of its byte. As amd64's, it appends no cold path.
func (c *a64Compiler) loadProto(dst, obj arm64.Reg) {
	none := c.a.NewLabel()
	if c.enc.FlagPoly != 0 {
		shaped := c.a.NewLabel()
		c.a.LoadU8(arm64.X27, obj, c.enc.ObjectFlags+1)
		c.a.ShiftImm(arm64.Lsr, arm64.X27, arm64.X27, uint8(bits.TrailingZeros8(c.enc.FlagPoly)), false)
		c.a.ShiftImm(arm64.Lsl, arm64.X27, arm64.X27, 31, false)
		c.a.Cbz(arm64.X27, shaped, false)
		c.a.Load(dst, obj, c.enc.ObjectProps)
		c.a.Load(dst, dst, c.enc.RefOffset)
		c.a.B(none)
		c.a.Bind(shaped)
	}
	c.a.Load(dst, obj, c.enc.ObjectShape)
	c.a.Cbz(dst, none, true)
	c.a.Load(dst, dst, c.enc.ShapeProto)
	c.a.Bind(none)
}

// objectData is amd64's: dst the pointer word of the data of the object in
// obj, which has its side fields and data of type typ. It uses t and A,
// which dst must be neither of; dst and t may each be obj, which, as in
// amd64's, only the first instruction reads: after it, it reads through
// dst, the side fields' address.
func (c *a64Compiler) objectData(dst, obj, t arm64.Reg, typ uint64, fail a64Guard) {
	c.a.AddImm(dst, obj, int64(c.enc.ObjectExt), true)
	c.a.LoadU8(t, dst, c.enc.ObjectFlags-c.enc.ObjectExt)
	c.movImm(a64A, uint64(c.enc.FlagData))
	c.a.Tst(t, a64A, false)
	fail.jump(arm64.EQ)
	c.a.Load(t, dst, c.enc.ObjectData)
	c.movImm(a64A, typ)
	c.a.Cmp(t, a64A, true)
	fail.jump(arm64.NE)
	c.a.Load(dst, dst, c.enc.ObjectData+8)
}

// savingCall is the routine the code's calls of Go that save every register
// share, as amd64's; with stub, stubSaving, the routine's address in B,
// its return in A.
func (c *a64Compiler) savingCall(stub bool) {
	for _, r := range c.usedRegs(c.gprs, false) {
		c.a.Store(a64Ctx, abi.OffRegs+int32(r)*8, arm64.Reg(r))
	}
	for _, r := range c.usedRegs(c.fprs, true) {
		c.a.StoreF(a64Ctx, abi.OffXRegs+int32(r)*8, arm64.FReg(r))
	}
	back := c.a.NewLabel()
	c.a.Adr(a64A, back)
	if stub {
		c.a.Br(a64B)
	} else {
		c.a.Store(a64Ctx, abi.OffGoResume, a64A)
		c.movImm(a64A, c.enc.CallGo)
		c.a.Br(a64A)
	}
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
	// Each prototype's layout, as amd64's addAlong checks them.
	if !add.Define {
		for _, h := range add.Chain() {
			c.movImm(a64C, uint64(h.Object))
			// Not D, which may be the object's (o).
			c.a.Load(a64B, a64C, c.enc.ObjectShape)
			c.movImm(a64A, uint64(h.Shape))
			c.a.Cmp(a64B, a64A, true)
			c.a.BCond(arm64.NE, miss)
		}
	}
	c.a.LoadU32(a64B, o, c.enc.ObjectPropsLen)
	c.a.LoadU32(a64C, o, c.enc.ObjectPropsCap)
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
	c.a.Store32(o, c.enc.ObjectPropsLen, a64C)
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
	c.a.Store32(o, c.enc.ObjectPropsLen, a64C)
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
func (c *a64Compiler) property(v *ssa.Value, guard a64Guard) {
	if v.Holders() != nil || v.Cases() != nil {
		c.holder(v, guard)
		return
	}
	found := c.a.NewLabel()
	if v.Const.Bits != 0 {
		// As amd64's: the shape the site knows inline, unless an earlier
		// operation checked it (ssa.Value's ShapeKnown); any other out of
		// line.
		p := c.gpr(v.Args[0], a64A)
		if !v.ShapeKnown {
			scan := c.a.NewLabel()
			c.a.Load(a64B, p, c.enc.ObjectShape)
			c.movImm(a64C, v.Const.Bits)
			c.a.Cmp(a64B, a64C, true)
			if !v.Megamorphic() {
				guard.jump(arm64.NE)
			} else {
				c.a.BCond(arm64.NE, scan)
				c.cold = append(c.cold, func() {
					c.a.Bind(scan)
					c.scan(v, guard, found, 0)
				})
			}
		}
		c.a.Load(a64A, p, c.enc.ObjectProps)
		if v.Index != 0 {
			c.a.AddImm(a64A, a64A, int64(int32(v.Index)*c.enc.ValueSize), true)
		}
		c.a.Bind(found)
		return
	}
	if !v.Megamorphic() && !v.Scans() {
		// As amd64's: every receiver fails.
		c.a.Cmp(a64B, a64B, true)
		guard.jump(arm64.EQ)
		c.a.Bind(found)
		return
	}
	c.scan(v, guard, found, 0)
	c.a.Bind(found)
}

// scan searches an object's small table for the key a property operation
// names, as amd64's does, going to found with the value's address in A.
func (c *a64Compiler) scan(v *ssa.Value, guard a64Guard, found, mega arm64.Label) {
	p := c.gpr(v.Args[0], a64A)
	c.a.LoadU8(a64B, p, c.enc.ObjectClass)
	c.a.CmpImm(a64B, int64(c.enc.ClassObject), false)
	if v.Op == ssa.OpPropWrite {
		guard.jump(arm64.NE)
	} else {
		// As amd64's: a read searches a function's table too.
		object := c.a.NewLabel()
		c.a.BCond(arm64.EQ, object)
		c.a.CmpImm(a64B, int64(c.enc.ClassFunction), false)
		guard.jump(arm64.NE)
		c.a.Bind(object)
	}
	c.a.LoadU32(a64B, p, c.enc.ObjectPropsLen)
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
		guard.jump(arm64.HI)
	}
	// No properties, no shape -- but the megamorphic cache's, a read's or
	// a write's addition, as amd64's.
	c.a.CmpImm(a64B, 0, true)
	if mega != 0 && v.Megamorphic() {
		c.a.BCond(arm64.EQ, mega)
	} else {
		guard.jump(arm64.EQ)
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
	if mega != 0 && v.Megamorphic() {
		// As amd64's: not its own, a prototype's, which the megamorphic
		// cache may have, or a write's addition.
		c.a.BCond(arm64.EQ, mega)
	} else {
		guard.jump(arm64.EQ)
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
	guard.jump(arm64.NE)
	// As amd64's: the value at the key's position.
	p = c.gpr(v.Args[0], a64C)
	c.a.LoadU32(a64C, p, c.enc.ObjectPropsLen)
	c.a.Op(arm64.Sub, a64C, a64C, a64B, true)
	c.a.ShiftImm(arm64.Lsl, a64C, a64C, 4, true)
	p = c.gpr(v.Args[0], a64A)
	c.a.Load(a64A, p, c.enc.ObjectProps)
	c.a.Op(arm64.Add, a64A, a64A, a64C, true)
	c.a.B(found)
}

// keyAt is amd64's: the object in A has a property at v's index, of v's
// key, its shape's keys left in B. It uses C.
func (c *a64Compiler) keyAt(v *ssa.Value, guard a64Guard) {
	c.a.LoadU32(a64B, a64A, c.enc.ObjectPropsLen)
	c.a.CmpImm(a64B, int64(v.Index), true)
	guard.jump(arm64.LS)
	c.a.Load(a64B, a64A, c.enc.ObjectShape)
	c.a.Load(a64B, a64B, c.enc.ShapeKeys)
	c.a.LoadU32(a64C, a64B, int32(v.Index)*c.enc.KeySize+c.enc.PropertyKey)
	c.a.CmpImm(a64C, int64(v.Key), false)
	guard.jump(arm64.NE)
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
// arrayLiteral is amd64's: the pool's next array, held in F2, the operands
// written in as its elements, its length set, then the array kept in the
// call's keep cell, which an operand's pointer word may be read from, and
// the result set last.
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
	c.a.FMovToF(a64F2, a64C)
	c.arrayElements(v, site, false)
	c.a.FMovFromF(a64C, a64F2)
	c.a.Store(a64Ctx, at+c.enc.RefOffset, a64C)
	c.movImm(a64A, c.enc.Object)
	c.a.Store(a64Ctx, at+c.enc.NumOffset, a64A)
	c.setG(v, a64A)
}

// arrayElements is amd64's: the elements written into the array in F2,
// or, by Go while the collector marks (viaGo), into the pool's next, read
// again from the pool at each, and the length set.
func (c *a64Compiler) arrayElements(v *ssa.Value, site *ssa.CallSite, viaGo bool) {
	vs := int32(c.enc.ValueSize)
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
			c.a.MovRR(a64D, a64B)
			c.poolNext(site.Pool)
			c.a.Load(a64A, a64C, c.enc.ObjectElems)
			c.a.AddImm(a64A, a64A, int64(int32(i)*vs), true)
			c.a.MovRR(a64C, a64D)
			c.valueStore()
			continue
		}
		c.a.FMovFromF(a64C, a64F2)
		c.a.Load(a64A, a64C, c.enc.ObjectElems)
		c.a.Store(a64A, int32(i)*vs+c.enc.NumOffset, a64D)
		c.a.Store(a64A, int32(i)*vs+c.enc.RefOffset, a64B)
	}
	if viaGo {
		c.poolNext(site.Pool)
	} else {
		c.a.FMovFromF(a64C, a64F2)
	}
	c.movImm(a64B, uint64(len(v.Args)))
	c.a.Store(a64C, c.enc.ObjectElems+8, a64B)
}

// poolNext is amd64's: the next object of the pool at pool, which the pool
// still holds, into C. It uses B.
func (c *a64Compiler) poolNext(pool uintptr) {
	c.movImm(a64C, uint64(pool))
	c.a.Load(a64B, a64C, abi.OffPoolCount)
	c.a.ShiftImm(arm64.Lsl, a64B, a64B, 3, true)
	c.a.Op(arm64.Add, a64B, a64B, a64C, true)
	c.a.Load(a64C, a64B, abi.OffPoolObjects-8)
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

// callRecords is amd64's: a native call's records written.
func (c *a64Compiler) callRecords(v *ssa.Value, recs []callRecord) {
	defer c.viewExits()()
	for k, r := range recs {
		at := abi.OffRecord + int32(k)*32
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
		case remat(r.x) && !c.held(r.x, v):
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
}

// genEntry is amd64's: the code's generator entry, the step's
// context in the context register, the call entry's registers, where the
// step goes on if the code cannot resume the generator in X10, the
// generator and the step's cursor in the next context. It uses every
// register.
func (c *a64Compiler) genEntry() {
	const next, g, t, u = arm64.Reg(12), arm64.Reg(13), arm64.Reg(11), arm64.Reg(8)
	miss := c.a.NewLabel()
	c.a.Load(next, a64Ctx, abi.OffNext)
	c.a.Load(g, next, abi.OffGenObject)
	// Room for the window and what is past it, as a call entry makes it.
	c.a.Load(t, a64Ctx, abi.OffStackTop)
	c.a.Load(t, t, 0)
	c.a.AddImm(t, t, int64(c.frameSize()), true)
	c.a.Load(u, a64Ctx, abi.OffStackEnd)
	c.a.Cmp(t, u, true)
	c.a.BCond(arm64.HI, miss)
	c.a.Load(u, g, c.enc.GenSaved)
	c.a.Cbnz(u, miss, true)
	c.a.LoadU32(t, g, c.enc.GenPC)
	for _, gr := range c.f.GenResumes {
		blk := c.entryAt(gr.PC)
		if blk == nil {
			continue
		}
		skip := c.a.NewLabel()
		c.a.CmpImm(t, int64(gr.Place), false)
		c.a.BCond(arm64.NE, skip)
		c.a.LoadU8(u, g, c.enc.GenStarted)
		c.a.CmpImm(u, int64(b2i(gr.Started)), false)
		c.a.BCond(arm64.NE, miss)
		c.genFrame(gr, blk)
		c.a.Bind(skip)
	}
	c.a.Bind(miss)
	c.a.Br(arm64.Reg(10))
}

// genFrame is amd64's: the generator's frame its values, which keep
// its whole register file, nothing copied; room past the VM's stack's top
// for what the code keeps past its registers and for the window Go makes
// of them where the code leaves native code; the value sent in written to
// the yield's register, its pointer word through storeRef while the
// collector marks; the context made, the generator running, and on at blk.
func (c *a64Compiler) genFrame(gr ssa.GenResume, blk *ssa.Block) {
	vs := int32(c.enc.ValueSize)
	const next, g, t, u = arm64.Reg(12), arm64.Reg(13), arm64.Reg(11), arm64.Reg(8)
	c.a.Store(next, abi.OffReturnTo, a64CallRet)
	c.a.Store(next, abi.OffClosure, a64CallClosure)
	c.a.Load(t, a64CallClosure, c.enc.ClosureUpvalues)
	c.a.Store(next, abi.OffUpvalues, t)
	c.a.Load(a64A, a64CallLink, c.enc.LinkIn)
	c.a.AddImm(a64A, a64A, 1, true)
	c.a.Store(a64CallLink, c.enc.LinkIn, a64A)
	// The room past the VM's stack's top, which moves past it, its high
	// water with it.
	c.a.Load(u, a64Ctx, abi.OffStackTop)
	c.a.Load(t, u, 0)
	c.a.Store(next, abi.OffBase, t)
	c.a.AddImm(a64A, t, int64(c.frameSize()), true)
	c.a.Store(u, 0, a64A)
	same := c.a.NewLabel()
	c.a.Load(u, a64Ctx, abi.OffStackHigh)
	c.a.Load(a64C, u, 0)
	c.a.Cmp(a64A, a64C, true)
	c.a.BCond(arm64.LS, same)
	c.a.Store(u, 0, a64A)
	c.a.Bind(same)
	c.a.Load(u, a64Ctx, abi.OffStackBase)
	c.a.AddLsl(t, u, t, 4)
	c.a.AddImm(a64A, t, int64(c.f.FrameLocals+c.f.Scratch)*int64(vs), true)
	c.a.Store(next, abi.OffStack, a64A)
	// Its registers, the generator's values.
	c.a.Load(u, g, c.enc.GenVals)
	c.a.Store(next, abi.OffLocals, u)
	c.a.Store(next, abi.OffOpen, arm64.ZR)
	c.a.Store(next, abi.OffCells, arm64.ZR)
	if c.f.Loops {
		c.a.Store(next, abi.OffLoop, arm64.ZR)
	}
	c.storeCode(next, a64A)
	c.a.MovImm(a64A, abi.LiveCall)
	c.a.Store(next, abi.OffLive, a64A)
	c.a.MovImm(a64A, uint64(c.enc.GenExecuting))
	c.a.Store8(g, c.enc.GenState, a64A)
	c.a.MovRR(a64Ctx, next)
	if gr.Sent >= 0 {
		// The value sent in, the context's (abi.Context's GenSent), stored
		// in the generator as amd64's.
		at := int32(gr.Sent) * vs
		c.a.Load(a64A, a64Ctx, abi.OffGenSent+c.enc.NumOffset)
		c.a.Store(u, at+c.enc.NumOffset, a64A)
		marked, stored := c.a.NewLabel(), c.a.NewLabel()
		c.movImm(a64C, c.enc.WriteBarrier)
		c.a.LoadU8(a64C, a64C, 0)
		c.a.Cbnz(a64C, marked, false)
		c.a.Load(a64A, a64Ctx, abi.OffGenSent+c.enc.RefOffset)
		c.a.Store(u, at+c.enc.RefOffset, a64A)
		c.a.B(stored)
		c.a.Bind(marked)
		c.a.Load(a64A, a64Ctx, abi.OffLocals)
		c.a.AddImm(a64A, a64A, int64(at+c.enc.RefOffset), true)
		c.a.Load(a64C, a64Ctx, abi.OffGenSent+c.enc.RefOffset)
		c.refStore()
		c.a.Bind(stored)
	}
	c.a.B(c.labels[blk.ID])
}

// copyValues is amd64's: n values, both words of each, from src+soff to
// dst+doff, one by one for a few and by a loop over X5, X6 and X7 past
// them. It uses A, and moves neither src nor dst.
func (c *a64Compiler) copyValues(dst arm64.Reg, doff int32, src arm64.Reg, soff int32, n int32) {
	vs := int32(c.enc.ValueSize)
	words := [2]int32{c.enc.NumOffset, c.enc.RefOffset}
	if n <= 16 {
		for i := range n {
			for _, w := range words {
				c.a.Load(a64A, src, soff+i*vs+w)
				c.a.Store(dst, doff+i*vs+w, a64A)
			}
		}
		return
	}
	const from, to, end = arm64.Reg(5), arm64.Reg(6), arm64.Reg(7)
	c.a.AddImm(from, src, int64(soff), true)
	c.a.AddImm(to, dst, int64(doff), true)
	c.a.AddImm(end, from, int64(n*vs), true)
	loop := c.a.NewLabel()
	c.a.Bind(loop)
	for _, w := range words {
		c.a.Load(a64A, from, w)
		c.a.Store(to, w, a64A)
	}
	c.a.AddImm(from, from, int64(vs), true)
	c.a.AddImm(to, to, int64(vs), true)
	c.a.Cmp(from, end, true)
	c.a.BCond(arm64.LO, loop)
}

// yieldExit is amd64's: a generator's yield, suspended natively in
// code a loop's step or a call of next resumed natively, a window Go made
// of the generator's values written back to them, the value in the
// yield's register returned to the step; the exit otherwise.
func (c *a64Compiler) yieldExit(b *ssa.Block) {
	s := b.State
	vs := int32(c.enc.ValueSize)
	c.a.Load(a64A, a64Ctx, abi.OffReturnTo)
	c.a.Cbz(a64A, c.stubLabel(s, abi.ExitHost), true)
	then := c.a.NewLabel()
	c.exitThen(s, abi.ExitHost, &then)
	c.a.Bind(then)
	if n := int32(c.f.FrameLocals); n > 0 {
		// As amd64's: written back from a window Go made of them alone.
		own := c.a.NewLabel()
		c.a.Load(a64B, a64Ctx, abi.OffGenObject)
		c.a.Load(a64B, a64B, c.enc.GenVals)
		c.a.Cmp(a64B, a64Locals, true)
		c.a.BCond(arm64.EQ, own)
		c.windowWriteBack(0, n)
		c.a.Bind(own)
	}
	c.a.Load(a64B, a64Ctx, abi.OffGenObject)
	c.a.MovImm(a64A, uint64(s.PC)+1)
	c.a.Store32(a64B, c.enc.GenPC, a64A)
	c.a.MovImm(a64A, uint64(c.enc.GenSuspendedYield))
	c.a.Store8(a64B, c.enc.GenState, a64A)
	c.a.MovImm(a64A, 1)
	c.a.Store8(a64B, c.enc.GenStarted, a64A)
	slot := int32(b.YieldSlot)
	c.a.Load(a64RetNum, a64Locals, slot*vs+c.enc.NumOffset)
	c.a.Load(a64RetRef, a64Locals, slot*vs+c.enc.RefOffset)
	c.a.Load(a64CallLink, a64Ctx, abi.OffReturnTo)
	c.returnTail()
}

// windowWriteBack is amd64's: the window's n registers from from written
// back to the generator's values, their pointer words with Go's write
// barrier while the collector marks.
func (c *a64Compiler) windowWriteBack(from, n int32) {
	vs := int32(c.enc.ValueSize)
	marked, copied := c.a.NewLabel(), c.a.NewLabel()
	c.movImm(a64C, c.enc.WriteBarrier)
	c.a.LoadU8(a64C, a64C, 0)
	c.a.Cbnz(a64C, marked, false)
	c.a.Load(a64B, a64Ctx, abi.OffGenObject)
	c.a.Load(a64B, a64B, c.enc.GenVals)
	c.copyValues(a64B, 0, a64Locals, from*vs, n)
	c.a.Bind(copied)
	c.cold = append(c.cold, func() {
		// As amd64's: the offset the copy is at in the context (RecordAt),
		// for storeRef keeps no scratch register.
		c.a.Bind(marked)
		loop := c.a.NewLabel()
		c.a.Store(a64Ctx, abi.OffRecordAt, arm64.ZR)
		c.a.Bind(loop)
		c.a.Load(a64B, a64Ctx, abi.OffRecordAt)
		c.a.Load(a64C, a64Ctx, abi.OffGenObject)
		c.a.Load(a64C, a64C, c.enc.GenVals)
		c.a.Op(arm64.Add, a64C, a64C, a64B, true)
		c.a.Op(arm64.Add, a64B, a64B, a64Locals, true)
		c.a.Load(a64A, a64B, from*vs+c.enc.NumOffset)
		c.a.Store(a64C, c.enc.NumOffset, a64A)
		c.a.AddImm(a64A, a64C, int64(c.enc.RefOffset), true)
		c.a.Load(a64C, a64B, from*vs+c.enc.RefOffset)
		c.refStore()
		c.a.Load(a64B, a64Ctx, abi.OffRecordAt)
		c.a.AddImm(a64B, a64B, int64(vs), true)
		c.a.Store(a64Ctx, abi.OffRecordAt, a64B)
		c.a.CmpImm(a64B, int64(n*vs), true)
		c.a.BCond(arm64.LO, loop)
		c.a.B(copied)
	})
}

// genReturn is amd64's: a return of a generator a loop's step resumed
// natively to genFinish.
func (c *a64Compiler) genReturn() {
	if !c.genDoneUsed {
		c.genDone, c.genDoneUsed = c.a.NewLabel(), true
	}
	c.a.Load(a64A, a64Ctx, abi.OffReturnTo)
	c.a.Cbnz(a64A, c.genDone, true)
}

// genFinish is amd64's: the generator done, its step's cursor too, the
// step's result its done.
func (c *a64Compiler) genFinish() {
	c.a.Load(a64B, a64Ctx, abi.OffGenObject)
	c.a.MovImm(a64A, uint64(c.enc.GenCompleted))
	c.a.Store8(a64B, c.enc.GenState, a64A)
	// As amd64's: a call of next, no cursor, its value the body's, the
	// context's generator none.
	call := c.a.NewLabel()
	c.a.Load(a64C, a64Ctx, abi.OffGenCursor)
	c.a.Cbz(a64C, call, true)
	c.a.MovImm(a64A, 1)
	c.a.Store8(a64C, c.enc.IterDone, a64A)
	c.movImm(a64RetNum, c.enc.Uninitialized)
	c.a.MovRR(a64RetRef, arm64.ZR)
	c.a.Load(a64CallLink, a64Ctx, abi.OffReturnTo)
	c.returnTail()
	c.a.Bind(call)
	c.a.Store(a64Ctx, abi.OffGenObject, arm64.ZR)
	word := c.a.NewLabel()
	c.a.Load(a64RetNum, a64Ctx, abi.OffRet)
	c.a.Load(a64RetRef, a64Ctx, abi.OffRetFrom)
	c.a.Cbz(a64RetRef, word, true)
	c.a.Load(a64RetRef, a64RetRef, c.enc.RefOffset)
	c.a.Bind(word)
	c.a.Load(a64CallLink, a64Ctx, abi.OffReturnTo)
	c.returnTail()
}

func (c *a64Compiler) call(v *ssa.Value, guard a64Guard) {
	sites, s := v.Calls(), v.State
	stub := c.stubLabel(s, exitKind(v.Aux))
	if len(sites) == 0 {
		c.a.B(stub)
		return
	}
	c.callV, c.callTop = v, c.a.NewLabel()
	c.a.Bind(c.callTop)
	defer func() { c.callV, c.allocDone = nil, 0 }()
	site := sites[0]
	vs := int32(c.enc.ValueSize)
	if site.Alloc && site.Argc == 1 && c.enc.CallGo != 0 && len(v.Args) <= abi.MaxGoArgs {
		c.allocDone = c.a.NewLabel()
	}
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
	// The call's operands end the state's slots but for the arguments a
	// parameter list's prologue keeps past them (ssa.FrameState's Args).
	sp := s.Own() - s.Args
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
	// As amd64's: one cold call Go makes in the call's place, which every
	// way the call does not make it natively goes to.
	var goCold arm64.Label
	goColdUsed := false
	goCall := func() arm64.Label {
		if !goColdUsed {
			goCold, goColdUsed = c.a.NewLabel(), true
			op := goCallWord(site)
			c.cold = append(c.cold, func() {
				c.a.Bind(goCold)
				if windowGoCall(v, site) {
					c.goCallWindow(v, op)
				} else {
					c.goCallOp(v, op, true)
				}
				c.a.B(join)
			})
		}
		return goCold
	}
	if !site.Alloc && !site.Receiver {
		// As amd64's: natively while the collector marks, its result stored
		// with Go's barrier then, a receiver from a pool shaded first.
		if c.enc.CallGo != 0 && (len(v.Args) <= abi.MaxGoArgs || windowGoCall(v, site)) {
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
	if generic {
		// As amd64's: none past the deepest context, the call Go's.
		c.a.BCond(arm64.HS, goCall())
	} else {
		guard.jump(arm64.HS)
	}
	if site.Forward {
		// As amd64's: the frame's arguments, past the context's room for
		// them, Go's.
		c.a.Load(a64B, a64Ctx, abi.OffArgc)
		c.a.CmpImm(a64B, abi.MaxGoArgs, true)
		if generic {
			c.a.BCond(arm64.HI, goCall())
		} else {
			guard.jump(arm64.HI)
		}
	}
	if callee == nil {
		c.movImm(a64C, uint64(site.Callee))
	} else {
		if callee.Shadow == nil && c.origin.At(callee) < 0 || site.Via != 0 && (len(sites) != 1 || site.Window == 0 && !c.operandsLive(s, calleeSlot-1)) {
			c.a.B(stub)
			return
		}
		c.movImm(a64B, c.enc.Object)
		c.a.Cmp(c.gpr(callee, a64A), a64B, true)
		guard.jump(arm64.NE)
		c.sourceRef(callee, guard)
	}
	checked := c.a.NewLabel()
	// As amd64's: past its sites, any other function (genericCheck).
	gen := generic && site.Generic && site.Via == 0 && site.Pool == 0 && !site.Push && !site.Pop && !site.Alloc && !site.Receiver &&
		!site.Literal && !site.Forward && vs == 16 && c.enc.HintSlotSize == 32
	// As amd64's: a call of one function writes its operands where the
	// callee has them, unrecorded.
	direct, ops := len(sites) == 1 && !gen && !site.ArrowThis && !site.FrameArgs && !site.Forward && (site.ThisSlot < 0 || site.Method || site.Pool != 0 || site.Super || site.PlainThis), calleeSlot
	if site.Method {
		ops--
	}
	direct = direct && c.operandsLive(s, ops)
	// As amd64's: register code's call (ssa.CallSite's Window) writes its
	// own operands to the callee's window, whatever function the checks
	// find; one Go would make where it does not make it natively leaves.
	win := site.Window != 0
	if win {
		if !generic {
			c.a.B(stub)
			return
		}
		direct = false
	}
	// As amd64's: such a call's room check leaves the VM's stack's top in
	// D, where nothing after it reads the callee.
	keepTop := (direct || win && len(sites) == 1 && !gen && !site.Push && !site.Pop && !site.Alloc && !site.Receiver) && site.Parent == 0 && site.Pool == 0
	// As amd64's: a forwarding call's miss past its functions, or of a
	// receiver Go coerces, Go's.
	forwardGo := site.Forward && generic
	miss := stub
	if forwardGo && site.Generic {
		miss = goCall()
	}
	var grow arm64.Label
	growUsed := false
	for k, t := range sites {
		// As amd64's: the last function's checks fail straight to the stub.
		last := k == len(sites)-1 && !gen
		next := miss
		if !last {
			next = c.a.NewLabel()
		}
		done := func() {
			if !last {
				c.a.B(checked)
				c.a.Bind(next)
			}
		}
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
			done()
			continue
		}
		if t.Push {
			// As amd64's: no room for its values, push's leaf.
			room := guard
			if pushGrows(t, c.enc) {
				grow, growUsed = c.a.NewLabel(), true
				room = a64GuardLabel(c, grow)
			}
			c.pushGuards(t, v.Args[0], guard, room)
			done()
			continue
		}
		if t.Alloc || t.Receiver {
			c.constructGuards(t, guard)
			if t.Alloc && t.Argc == 1 {
				c.lengthGuards(t, v.Args[len(v.Args)-1], guard)
			}
			done()
			continue
		}
		// No code native callers may call is no reason to leave: Go runs
		// the callee once its frame is made (enterExit).
		if t.Coerce {
			recv := v.Args[0]
			if t.Via != 0 || t.Forward {
				recv = v.Args[2]
			}
			c.movImm(a64B, c.enc.Object)
			c.a.Cmp(c.gpr(recv, a64A), a64B, true)
			if forwardGo || win && generic {
				// As amd64's: Go coerces it.
				c.a.BCond(arm64.NE, goCall())
			} else {
				guard.jump(arm64.NE)
			}
		}
		if win {
			// As amd64's: the callee's window, or the arguments the call
			// writes where they are more than its registers, a constant,
			// the room its code takes past the window left below the
			// context's StackEnd. C keeps the callee, which a
			// construction's checks read; the stack's top is kept in D.
			room := int64(max(t.LocalCount, 2+site.Argc))
			if keepTop {
				c.a.Load(a64D, a64Ctx, abi.OffStackTop)
				c.a.Load(a64D, a64D, 0)
				c.a.AddImm(a64B, a64D, room, true)
			} else {
				c.a.Load(a64B, a64Ctx, abi.OffStackTop)
				c.a.Load(a64B, a64B, 0)
				c.a.AddImm(a64B, a64B, room, true)
			}
		} else if keepTop {
			c.a.Load(a64D, a64Ctx, abi.OffStackTop)
			c.a.Load(a64D, a64D, 0)
			c.a.AddImm(a64B, a64D, int64(t.LocalCount+t.MaxStack), true)
		} else {
			c.a.Load(a64B, a64Ctx, abi.OffStackTop)
			c.a.Load(a64B, a64B, 0)
			c.a.AddImm(a64B, a64B, int64(t.LocalCount+t.MaxStack), true)
		}
		c.a.Load(a64A, a64Ctx, abi.OffStackEnd)
		c.a.Cmp(a64B, a64A, true)
		guard.jump(arm64.HI)
		if t.Parent != 0 {
			// As amd64's: the callee's prototype still the parent.
			c.loadProtoPlain(a64B, a64C)
			c.movImm(a64A, uint64(t.Parent))
			c.a.Cmp(a64B, a64A, true)
			guard.jump(arm64.NE)
		}
		if t.Pool != 0 {
			c.constructGuards(t, guard)
		}
		done()
	}
	if gen {
		// As amd64's: a function none of them is, called through its link,
		// or by Go from native code.
		miss := c.a.NewLabel()
		if site.Table != 0 {
			c.tableCheck(v, site, len(sites), checked, miss)
		} else {
			c.genericCheck(v, site, len(sites), checked, miss)
		}
		c.a.Bind(miss)
		c.a.B(goCall())
	}
	c.a.Bind(checked)
	if site.Push {
		c.push(v)
		if growUsed {
			pushed := c.a.NewLabel()
			c.a.Bind(pushed)
			c.cold = append(c.cold, func() {
				c.a.Bind(grow)
				c.pushGrow(v, site)
				c.a.B(pushed)
			})
		}
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
		if c.allocDone != 0 {
			c.a.Bind(c.allocDone)
		}
		return
	}
	// As amd64's: the state, described (callState), the call writing only
	// what the caller's code keeps nowhere else.
	operands := -1
	if !direct && !win {
		operands = calleeSlot - 1
	}
	if site.Forward {
		// As amd64's: f, apply and the receiver, nothing below them.
		operands = calleeSlot
	}
	// As amd64's: register code's call leaves out the operands it
	// consumes (ssa.Value's Consumes).
	st, recs := c.callState(v, s, func(i int) bool {
		return direct && i >= ops && i < sp || win && v.Consumes(i-(site.Window-1))
	}, operands)
	// As amd64's: the function left out of the window where no callee
	// reads it (windowCallees).
	callees := windowCallees(site, sites, gen)
	st.Callees, st.Which = callees, len(sites) > 1 || gen
	c.callRecords(v, recs)
	c.movImm(a64A, uint64(uintptr(unsafe.Pointer(st))))
	c.a.Store(a64Ctx, abi.OffCallState, a64A)
	args, this := sp-site.Argc, calleeSlot-1
	if site.Via != 0 {
		args, this = args+1, args
	}
	if site.Forward {
		this = sp - 1
	}
	// A receiver written straight into the callee's context: one it reads,
	// or a method call's, which a callee that does not read it has too --
	// a frame Go makes for it (jitRegGoCallNested, jitRegUnwind) shows it
	// in a stack trace, as the interpreter's does.
	receiver := direct && site.Pool == 0 && !site.Super && !site.PlainThis && (site.ThisSlot >= 0 || site.Method && this < sp)
	if win {
		// As amd64's: the callee's window past the VM's stack's top, in D,
		// which pointerWord does not use, given the call's operands
		// straight from where the code has them -- the function in its
		// register 0, a method call's receiver in 1, the arguments from 2
		// -- and, in setup, what each callee's own window needs besides:
		// made of the stack's top the room check left in D, if no record
		// was written since (callRecords).
		if !keepTop || len(recs) != 0 {
			c.a.Load(a64D, a64Ctx, abi.OffStackTop)
			c.a.Load(a64D, a64D, 0)
		}
		c.a.Load(a64B, a64Ctx, abi.OffStackBase)
		c.a.AddLsl(a64D, a64B, a64D, 4)
		m := 0
		switch {
		case site.Forward:
			// As amd64's: f, where a callee reads it, and its receiver; the
			// arguments the frame's.
			if callees == nil {
				c.directOperand(v, v.Args[0], a64D, 0)
			}
			c.directOperand(v, v.Args[2], a64D, vs)
		case site.Via != 0:
			// As amd64's: G its function, x its receiver.
			c.directOperand(v, v.Args[0], a64D, 0)
			c.directOperand(v, v.Args[2], a64D, vs)
			for i := range site.Argc - 1 {
				c.directOperand(v, v.Args[3+i], a64D, int32(2+i)*vs)
			}
		case site.Super:
			// As amd64's: the arguments, past the this slot; the function
			// and the instance, in setup (windowSuper).
			for i := range site.Argc {
				c.directOperand(v, v.Args[1+i], a64D, int32(2+i)*vs)
			}
		case site.Accessor != ssa.AccessorNone:
			// As amd64's (windowAccessor): a getter's or a setter's, the
			// function, where the callee reads it, the site's, the
			// receiver in 1, a setter's value in 2.
			if callees == nil {
				c.movImm(a64A, c.enc.Object)
				c.a.Store(a64D, c.enc.NumOffset, a64A)
				c.movImm(a64A, uint64(site.Callee))
				c.a.Store(a64D, c.enc.RefOffset, a64A)
			}
			c.directOperand(v, v.Args[0], a64D, vs)
			if site.Argc == 1 {
				c.directOperand(v, v.Args[1], a64D, 2*vs)
			}
		default:
			if site.Method {
				m = 1
				c.directOperand(v, v.Args[0], a64D, vs)
			}
			if callees == nil {
				c.directOperand(v, v.Args[m], a64D, 0)
			}
			for i := range site.Argc {
				c.directOperand(v, v.Args[m+1+i], a64D, int32(2+i)*vs)
			}
		}
	}
	if direct {
		// The callee's frame's address, in D, which pointerWord does not
		// use: made of the stack's top the room check left there, if no
		// record was written since (callRecords).
		if !keepTop || len(recs) != 0 {
			c.a.Load(a64D, a64Ctx, abi.OffStackTop)
			c.a.Load(a64D, a64D, 0)
		}
		c.a.Load(a64B, a64Ctx, abi.OffStackBase)
		c.a.AddLsl(a64D, a64B, a64D, 4)
		// From the call's state, as its records are (exitView).
		back := c.viewExits()
		for i := 0; i < site.Params && args+i < sp && i < site.LocalCount; i++ {
			c.directOperand(v, s.Slot(args+i), a64D, int32(i)*vs)
		}
		if receiver {
			// The callee's context's, the next.
			c.a.Load(a64D, a64Ctx, abi.OffNext)
			c.directOperand(v, s.Slot(this), a64D, abi.OffThis)
		}
		back()
	}
	for _, sv := range c.savesOf(v) {
		switch {
		case sv.frame != 0:
			// As amd64's: what the frame holds throughout, not saved.
		case sv.float:
			c.spillStoreF(sv.slot, arm64.FReg(sv.reg))
		default:
			c.spillStore(sv.slot, arm64.Reg(sv.reg))
		}
	}
	// As amd64's: the callee makes its own frame and context at its call
	// entry, the call writing what only it knows; a callee with none Go
	// makes, from native code in code that calls Go, or once its frame is
	// made (enterExit).
	const calleeCtx, locals, tmp, top, high2 = arm64.Reg(3), arm64.Reg(5), arm64.Reg(6), arm64.Reg(7), arm64.Reg(8)
	back := c.a.NewLabel()
	c.resumeUsed = true
	// setup is amd64's: it keeps a64CallLink and a64D, the callee's entry.
	setup := func(t *ssa.CallSite) {
		if win {
			c.callFrame(locals, tmp)
			if t.Forward {
				c.windowForward(t, locals, calleeCtx)
			} else {
				c.windowSetup(t, locals, top, high2)
				if t.FrameArgs {
					c.windowArgs(t, locals, calleeCtx, top)
				}
			}
			if t.Tail {
				// As amd64's: whether its calls in tail position may give its
				// frame up, set at every call of it.
				c.a.Load(calleeCtx, a64Ctx, abi.OffNext)
				c.movImm(top, b2u(t.TailFrame))
				c.a.Store(calleeCtx, abi.OffTailFrame, top)
				c.a.Store(calleeCtx, abi.OffFrameThis, arm64.ZR)
			}
			if t.Pool != 0 {
				// As amd64's: a construction's receiver, the pool's last
				// object, its cell cleared, in the window's register 1 and
				// the callee's context's This, its new.target there.
				c.a.Load(calleeCtx, a64Ctx, abi.OffNext)
				c.movImm(tmp, uint64(t.Pool))
				c.a.Load(top, tmp, abi.OffPoolCount)
				c.a.AddImm(top, top, -1, true)
				c.a.Store(tmp, abi.OffPoolCount, top)
				c.a.ShiftImm(arm64.Lsl, top, top, 3, true)
				c.a.Op(arm64.Add, top, top, tmp, true)
				c.a.Load(high2, top, abi.OffPoolObjects)
				c.a.Store(top, abi.OffPoolObjects, arm64.ZR)
				if t.Derived {
					// As amd64's: a derived constructor's instance, its
					// this unbound, the function whose code it is.
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
				c.movImm(tmp, c.enc.Object)
				c.a.Store(locals, vs+c.enc.RefOffset, high2)
				c.a.Store(locals, vs+c.enc.NumOffset, tmp)
				c.movImm(top, uint64(t.Callee))
				c.a.Store(calleeCtx, abi.OffNewTarget, top)
				if t.Parent != 0 {
					c.movImm(tmp, uint64(t.Parent))
					c.a.Store(locals, c.enc.RefOffset, tmp)
				}
			}
			if t.Super {
				c.windowSuper(t, locals, calleeCtx, top, tmp)
			}
			if t.ByCode != 0 {
				c.a.Load(a64CallClosure, a64Ctx, abi.OffCallClosure)
			} else {
				c.movImm(a64CallClosure, uint64(t.Closure))
			}
			c.a.Adr(a64CallRet, back)
			return
		}
		framed := false
		argc, first := t.Argc, sp-t.Argc
		if t.Via != 0 {
			argc, first = argc-1, first+1
		}
		if t.Forward {
			// As amd64's: the frame's own arguments (forwardArgs).
			argc = 0
			c.callFrame(locals, tmp)
			framed = true
			if t.FrameArgs {
				c.a.Load(calleeCtx, a64Ctx, abi.OffNext)
			}
			c.forwardArgs(t, locals, calleeCtx)
		}
		for i := 0; i < t.Params && i < t.LocalCount && !t.Forward; i++ {
			at := int32(i) * vs
			if i < argc && direct {
				continue
			}
			if !framed {
				c.callFrame(locals, tmp)
				framed = true
			}
			if i < argc {
				nb, nd, rb, rd := c.callWords(st, first+i)
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
		if t.Tail || t.Pool != 0 || t.Super || t.ArrowThis || t.PlainThis || t.FrameArgs || t.Function || (t.ThisSlot >= 0 || site.Method) && !direct || !receiver {
			c.a.Load(calleeCtx, a64Ctx, abi.OffNext)
		}
		if t.Function && !t.Derived && !t.Super {
			// As amd64's: its function object.
			if t.ByCode != 0 {
				c.a.Load(top, a64Ctx, abi.OffCallCallee)
			} else {
				c.movImm(top, uint64(t.Callee))
			}
			c.a.Store(calleeCtx, abi.OffFunction, top)
		}
		if t.FrameArgs && !t.Forward {
			// As amd64's: all its arguments and their count.
			c.movImm(top, uint64(argc))
			c.a.Store(calleeCtx, abi.OffArgc, top)
			for i := range argc {
				at := abi.OffArgs + int32(i)*int32(unsafe.Sizeof(abi.GoArg{}))
				nb, nd, rb, rd := c.callWords(st, first+i)
				c.a.Load(top, nb, nd)
				c.a.Store(calleeCtx, at+int32(unsafe.Offsetof(abi.GoArg{}.Num)), top)
				c.a.Load(top, rb, rd)
				c.a.Store(calleeCtx, at+int32(unsafe.Offsetof(abi.GoArg{}.Ref)), top)
			}
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
		} else if (t.ThisSlot >= 0 || site.Method || t.Forward) && !direct {
			nb, nd, rb, rd := c.callWords(st, this)
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
		c.movImm(a64CallLink, uint64(t.Entry)-uint64(c.enc.LinkEntry))
		c.a.Load(a64D, a64CallLink, c.enc.LinkEntry)
		if generic {
			c.a.Cbz(a64D, goCall(), true)
			setup(t)
			c.a.Br(a64D)
			c.a.Bind(next)
			continue
		}
		none := c.a.NewLabel()
		c.a.Cbz(a64D, none, true)
		setup(t)
		c.a.Br(a64D)
		c.cold = append(c.cold, func() {
			c.a.Bind(none)
			setup(t)
			if t.ByCode != 0 {
				c.a.Load(a64CallObject, a64Ctx, abi.OffCallCallee)
			} else {
				c.movImm(a64CallObject, uint64(t.Callee))
			}
			c.callEnter()
		})
		c.a.Bind(next)
	}
	if gen {
		// As amd64's: its frame made as its link says.
		setupGen := func() {
			c.callFrame(locals, tmp)
			if win {
				// As amd64's (genWindowSetup).
				c.genWindowSetup(site, locals, calleeCtx, top, tmp, high2)
				c.a.Load(a64CallClosure, a64Ctx, abi.OffCallClosure)
				c.a.Adr(a64CallRet, back)
				return
			}
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
				nb, nd, rb, rd := c.callWords(st, first+i)
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
				nb, nd, rb, rd := c.callWords(st, calleeSlot-1)
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
		if win {
			// As amd64's: no code, Go's call.
			c.a.Cbz(a64D, goCall(), true)
		} else {
			c.a.Cbz(a64D, none, true)
		}
		setupGen()
		c.a.Br(a64D)
		c.cold = append(c.cold, func() {
			c.a.Bind(none)
			setupGen()
			c.a.Load(a64CallObject, a64Ctx, abi.OffCallCallee)
			c.callEnter()
		})
	}
	// As amd64's: the callee returned, its result's words in a64RetNum and
	// a64RetRef.
	c.a.Bind(back)
	at := abi.OffKeep + int32(v.Index)*vs
	if c.resultDropped(v) && !site.Super {
		// As amd64's: a result nothing uses, stored nowhere.
		c.restoreSaves(v)
		return
	}
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
	c.restoreSaves(v)
	c.setG(v, a64RetNum)
}

// callWords locates a slot's words in the call's records or the frame.
func (c *a64Compiler) callWords(st *abi.CallState, slot int) (nb arm64.Reg, nd int32, rb arm64.Reg, rd int32) {
	for i := range st.Records {
		if int(st.Records[i].Slot) == slot {
			k := int32(i)
			return a64Ctx, abi.OffRecord + k*32 + 16, a64Ctx, abi.OffRecordRef + k*8
		}
	}
	nb, nd = c.slotAddr(slot, false, a64C)
	rb, rd = c.slotAddr(slot, true, a64C)
	return nb, nd, rb, rd
}

// callEnter branches to the shared fallback that enters a callee through Go.
func (c *a64Compiler) callEnter() {
	if !c.enterUsed {
		c.enter, c.enterUsed = c.a.NewLabel(), true
	}
	c.a.B(c.enter)
}

// callFrame computes the callee's frame address past the stack's top.
func (c *a64Compiler) callFrame(locals, tmp arm64.Reg) {
	c.a.Load(locals, a64Ctx, abi.OffStackTop)
	c.a.Load(locals, locals, 0)
	c.a.Load(tmp, a64Ctx, abi.OffStackBase)
	c.a.AddLsl(locals, tmp, locals, 4)
}

// restoreSaves is amd64's.
func (c *a64Compiler) restoreSaves(v *ssa.Value) { c.restoreSavesOf(v, true) }

// restoreSavesOf is amd64's.
func (c *a64Compiler) restoreSavesOf(v *ssa.Value, refused bool) {
	for _, sv := range c.savesOf(v) {
		if sv.cold && !refused {
			continue
		}
		if sv.frame != 0 {
			base, disp := c.slotAddr(sv.frame-1, false, a64A)
			if sv.float {
				c.a.LoadF(arm64.FReg(sv.reg), base, disp)
			} else {
				c.a.Load(arm64.Reg(sv.reg), base, disp)
			}
			continue
		}
		if sv.float {
			c.spillLoadF(arm64.FReg(sv.reg), sv.slot)
		} else {
			c.spillLoad(arm64.Reg(sv.reg), sv.slot)
		}
	}
}

// pushGuards checks a call of Array.prototype.push may append, as amd64's
// does. It uses A, B and C.
func (c *a64Compiler) pushGuards(t *ssa.CallSite, recv *ssa.Value, guard, room a64Guard) {
	if recv.Shadow == nil && c.origin.At(recv) < 0 {
		c.a.CmpImm(a64C, 0, true)
		guard.jump(arm64.NE)
		return
	}
	c.movImm(a64B, c.enc.Object)
	c.a.Cmp(c.gpr(recv, a64A), a64B, true)
	guard.jump(arm64.NE)
	c.sourceRef(recv, guard)
	c.a.LoadU8(a64B, a64C, c.enc.ObjectClass)
	c.a.CmpImm(a64B, int64(c.enc.ClassArray), false)
	guard.jump(arm64.NE)
	c.a.LoadU8(a64B, a64C, c.enc.ObjectFlags)
	want := c.enc.FlagExtensible | c.enc.FlagLengthWritable
	c.movImm(a64A, uint64(want|c.enc.FlagSparse|c.enc.FlagShared))
	c.a.Op(arm64.And, a64B, a64B, a64A, false)
	c.a.CmpImm(a64B, int64(want), false)
	guard.jump(arm64.NE)
	// Room for every argument, as amd64's.
	c.a.Load(a64B, a64C, c.enc.ObjectElems+8)
	if t.Argc > 1 {
		c.a.AddImm(a64B, a64B, int64(t.Argc-1), true)
	}
	c.a.Load(a64A, a64C, c.enc.ObjectElems+16)
	c.a.Cmp(a64B, a64A, true)
	room.jump(arm64.HS)
	c.loadProtoPlain(a64B, a64C)
	c.movImm(a64A, uint64(t.Protos[0].Object))
	c.a.Cmp(a64B, a64A, true)
	guard.jump(arm64.NE)
	c.protoGuards(t.Protos, guard)
}

// protoGuards checks an array's prototypes are as they were, as amd64's
// does. It uses A, B and C.
func (c *a64Compiler) protoGuards(protos [2]ssa.Holder, guard a64Guard) {
	for _, h := range protos {
		c.movImm(a64A, uint64(h.Object))
		c.a.Load(a64B, a64A, c.enc.ObjectShape)
		c.movImm(a64C, uint64(h.Shape))
		c.a.Cmp(a64B, a64C, true)
		guard.jump(arm64.NE)
		// No elements: no side fields, or none there. It need not be an
		// array.
		none := c.a.NewLabel()
		c.a.LoadU8(a64B, a64A, c.enc.ObjectFlags)
		c.movImm(a64C, uint64(c.enc.FlagExt))
		c.a.Tst(a64B, a64C, false)
		c.a.BCond(arm64.EQ, none)
		c.a.Load(a64B, a64A, c.enc.ObjectExt+c.enc.ExtElems+8)
		c.a.CmpImm(a64B, 0, true)
		guard.jump(arm64.NE)
		c.a.Bind(none)
		c.a.LoadU8(a64B, a64A, c.enc.ObjectFlags)
		c.movImm(a64C, uint64(c.enc.FlagSparse))
		c.a.Tst(a64B, a64C, false)
		guard.jump(arm64.NE)
		// Its prototype, its layout says, as amd64's.
	}
}

// popGuards checks a call of Array.prototype.pop may pop, as amd64's
// does. It uses A, B and C.
func (c *a64Compiler) popGuards(t *ssa.CallSite, recv *ssa.Value, guard a64Guard) {
	if recv.Shadow == nil && c.origin.At(recv) < 0 {
		c.a.CmpImm(a64C, 0, true)
		guard.jump(arm64.NE)
		return
	}
	c.movImm(a64B, c.enc.Object)
	c.a.Cmp(c.gpr(recv, a64A), a64B, true)
	guard.jump(arm64.NE)
	c.sourceRef(recv, guard)
	c.a.LoadU8(a64B, a64C, c.enc.ObjectClass)
	c.a.CmpImm(a64B, int64(c.enc.ClassArray), false)
	guard.jump(arm64.NE)
	c.a.LoadU8(a64B, a64C, c.enc.ObjectFlags)
	c.movImm(a64A, uint64(c.enc.FlagLengthWritable|c.enc.FlagSparse|c.enc.FlagShared))
	c.a.Op(arm64.And, a64B, a64B, a64A, false)
	c.a.CmpImm(a64B, int64(c.enc.FlagLengthWritable), false)
	guard.jump(arm64.NE)
	c.loadProtoPlain(a64B, a64C)
	c.movImm(a64A, uint64(t.Protos[0].Object))
	c.a.Cmp(a64B, a64A, true)
	guard.jump(arm64.NE)
	c.a.Load(a64B, a64C, c.enc.ObjectElems+8)
	c.a.CmpImm(a64B, 0, true)
	guard.jump(arm64.EQ)
	c.a.AddImm(a64B, a64B, -1, true)
	c.a.ShiftImm(arm64.Lsl, a64B, a64B, 4, true)
	c.a.Load(a64A, a64C, c.enc.ObjectElems)
	c.a.Op(arm64.Add, a64A, a64A, a64B, true)
	c.a.Load(a64B, a64A, c.enc.NumOffset)
	c.movImm(a64C, c.enc.Uninitialized)
	c.a.Cmp(a64B, a64C, true)
	guard.jump(arm64.EQ)
}

// pop pops a call of Array.prototype.pop's receiver's last element, as
// amd64's does. It uses A, B and C.
func (c *a64Compiler) pop(v *ssa.Value) {
	if c.enc.ValueSize != 16 {
		panic("mir: a value that is not 16 bytes")
	}
	c.sourceRef(v.Args[0], a64Guard{})
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

// push appends a call of Array.prototype.push's arguments, as amd64's
// does. It uses A, B, C, D, F0, F1 and F2.
func (c *a64Compiler) push(v *ssa.Value) {
	vs := int32(c.enc.ValueSize)
	recv, xs := v.Args[0], v.Args[2:]
	if vs != 16 {
		panic("mir: a value that is not 16 bytes")
	}
	at := abi.OffKeep + int32(v.Index)*vs
	for k, x := range xs {
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
		c.sourceRef(recv, a64Guard{})
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
		last := k == len(xs)-1
		if last {
			c.a.Scvtf(a64F0, a64B, true)
			c.a.FMovFromF(a64A, a64F0)
			c.a.Store(a64Ctx, at+c.enc.NumOffset, a64A)
			c.a.Store(a64Ctx, at+c.enc.RefOffset, arm64.ZR)
		}
		// As amd64's: while the collector marks, the element's pointer
		// word stored again with Go's write barrier.
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
			if last {
				c.a.Load(a64A, a64Ctx, at+c.enc.NumOffset)
			}
			c.a.B(done)
		})
	}
	c.setG(v, a64A)
}

// viaGuards checks a call through Function.prototype.call calls its
// function, as amd64's does. It uses A, B and C.
// forwardArgs is amd64's: a forwarding call's arguments, the frame's own,
// to the callee's frame at locals and, where it reads them past its
// parameters, to its context. It uses x6, x7, x8, a64A and a64C.
func (c *a64Compiler) forwardArgs(t *ssa.CallSite, locals, calleeCtx arm64.Reg) {
	const n, w, x = arm64.Reg(6), arm64.Reg(7), arm64.Reg(8)
	if unsafe.Sizeof(abi.GoArg{}) != 16 {
		panic("mir: an argument that is not 16 bytes")
	}
	vs := int32(c.enc.ValueSize)
	num, ref := int32(unsafe.Offsetof(abi.GoArg{}.Num)), int32(unsafe.Offsetof(abi.GoArg{}.Ref))
	c.a.Load(n, a64Ctx, abi.OffArgc)
	if t.FrameArgs {
		c.a.Store(calleeCtx, abi.OffArgc, n)
		loop, done := c.a.NewLabel(), c.a.NewLabel()
		c.a.AddImm(a64A, a64Ctx, int64(abi.OffArgs), true)
		c.a.AddImm(a64C, calleeCtx, int64(abi.OffArgs), true)
		c.a.AddLsl(w, a64A, n, 4)
		c.a.Bind(loop)
		c.a.Cmp(a64A, w, true)
		c.a.BCond(arm64.HS, done)
		c.a.Load(x, a64A, 0)
		c.a.Store(a64C, 0, x)
		c.a.Load(x, a64A, 8)
		c.a.Store(a64C, 8, x)
		c.a.AddImm(a64A, a64A, 16, true)
		c.a.AddImm(a64C, a64C, 16, true)
		c.a.B(loop)
		c.a.Bind(done)
	}
	for i := 0; i < t.Params && i < t.LocalCount; i++ {
		at := int32(i) * vs
		undef, next := c.a.NewLabel(), c.a.NewLabel()
		if i < abi.MaxGoArgs {
			c.a.CmpImm(n, int64(i), true)
			c.a.BCond(arm64.LS, undef)
			off := abi.OffArgs + int32(i)*16
			c.a.Load(w, a64Ctx, off+num)
			c.a.Store(locals, at+c.enc.NumOffset, w)
			c.a.Load(w, a64Ctx, off+ref)
			c.a.Store(locals, at+c.enc.RefOffset, w)
			c.a.B(next)
		}
		c.a.Bind(undef)
		c.movImm(w, c.enc.Undefined)
		c.a.Store(locals, at+c.enc.NumOffset, w)
		c.a.Store(locals, at+c.enc.RefOffset, arm64.ZR)
		c.a.Bind(next)
	}
}

func (c *a64Compiler) viaGuards(t *ssa.CallSite, recv *ssa.Value, guard a64Guard) {
	if recv.Shadow == nil && c.origin.At(recv) < 0 {
		c.a.CmpImm(a64C, 0, true)
		guard.jump(arm64.NE)
		return
	}
	c.movImm(a64B, c.enc.Object)
	c.a.Cmp(c.gpr(recv, a64A), a64B, true)
	guard.jump(arm64.NE)
	c.sourceRef(recv, guard)
	c.movImm(a64B, uint64(t.Callee))
	c.a.Cmp(a64C, a64B, true)
	guard.jump(arm64.NE)
}

// lengthGuards checks a `new Array(n)` can take its array from its pool,
// as amd64's does: n, x, an integer in [0, the pool's Room], past
// abi.PoolSmallRoom more than half of it, or up to abi.PoolRoomLimit once
// Go has filled the pool again with arrays of n's room. It uses A, B, C,
// F0 and F1.
func (c *a64Compiler) lengthGuards(t *ssa.CallSite, x *ssa.Value, guard a64Guard) {
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
		guard.jump(arm64.HS)
	}
	c.a.FMovToF(a64F0, w)
	c.a.Fcvtzs(a64A, a64F0)
	c.a.Scvtf(a64F1, a64A, true)
	c.a.FCmp(a64F1, a64F0)
	guard.jump(arm64.NE)
	c.movImm(a64B, uint64(t.Pool))
	c.a.Load(a64C, a64B, abi.OffPoolRoom)
	c.a.Cmp(a64A, a64C, true)
	fits, past := c.a.NewLabel(), c.a.NewLabel()
	c.a.BCond(arm64.HI, past)
	// Within its room: any n of a small room, else one of its class, more
	// than half of it, as amd64's.
	c.a.CmpImm(a64C, abi.PoolSmallRoom, true)
	c.a.BCond(arm64.LS, fits)
	c.a.ShiftImm(arm64.Lsr, a64C, a64C, 1, true)
	c.a.Cmp(a64A, a64C, true)
	c.a.BCond(arm64.HI, fits)
	v, top := c.callV, c.callTop
	var grow arm64.Label
	if v == nil {
		// No call to make again: Go makes it.
		guard.jump(arm64.LS)
	} else {
		grow = c.a.NewLabel()
		c.a.B(grow)
	}
	c.a.Bind(past)
	// Past its room: a negative n, unsigned, is past every room. Past
	// what any pool's arrays have room for, as amd64's: Go's.
	c.a.CmpImm(a64A, abi.PoolRoomLimit, true)
	if done := c.allocDone; done != 0 {
		big, op, v := c.a.NewLabel(), goOpWord(&ssa.CallSite{Go: ssa.GoCall, Argc: t.Argc, New: true}), c.callV
		c.a.BCond(arm64.HI, big)
		c.cold = append(c.cold, func() {
			c.a.Bind(big)
			c.goCallOp(v, op, true)
			c.a.B(done)
		})
	} else {
		guard.jump(arm64.HI)
	}
	if v == nil {
		// No call to make again: Go makes it. n is at most the limit, as
		// the flags still say.
		guard.jump(arm64.LS)
	} else {
		c.a.B(grow)
		pool := t.Pool
		stub := c.stubLabel(v.State, exitKind(v.Aux))
		c.cold = append(c.cold, func() {
			c.a.Bind(grow)
			c.a.AddImm(a64A, a64A, 1, true)
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
func (c *a64Compiler) constructGuards(t *ssa.CallSite, guard a64Guard) {
	c.movImm(a64A, uint64(t.Pool))
	c.a.Load(a64B, a64A, abi.OffPoolCount)
	if c.callV != nil {
		c.refillOnEmpty(t.Pool, c.stubLabel(c.callV.State, exitKind(c.callV.Aux)))
	} else {
		c.a.CmpImm(a64B, 0, true)
		guard.jump(arm64.EQ)
	}
	if t.Alloc {
		return
	}
	c.a.LoadU32(a64B, a64C, c.enc.ObjectPropsLen)
	c.a.CmpImm(a64B, int64(t.ProtoIndex), true)
	guard.jump(arm64.LS)
	c.a.Load(a64B, a64C, c.enc.ObjectShape)
	c.a.Load(a64B, a64B, c.enc.ShapeKeys)
	c.a.LoadU32(a64B, a64B, int32(t.ProtoIndex)*c.enc.KeySize+c.enc.PropertyKey)
	c.movImm(a64A, uint64(t.ProtoKey))
	c.a.Cmp(a64B, a64A, false)
	guard.jump(arm64.NE)
	c.a.Load(a64B, a64C, c.enc.ObjectProps)
	c.a.Load(a64C, a64B, int32(t.ProtoIndex)*c.enc.ValueSize+c.enc.RefOffset)
	c.movImm(a64A, uint64(t.Pool))
	c.a.Load(a64B, a64A, abi.OffPoolProto)
	c.a.Cmp(a64C, a64B, true)
	guard.jump(arm64.NE)
}

// directOperand writes a call's operand x, both words, at disp from base
// -- the callee's frame or this context -- as amd64's does. It uses A, B
// and C.
func (c *a64Compiler) directOperand(v, x *ssa.Value, base arm64.Reg, disp int32) {
	var w arm64.Reg
	if remat(x) && !c.held(x, v) {
		c.materialize(x, a64A)
		w = a64A
	} else {
		w = c.gpr(x, a64A)
	}
	c.a.Store(base, disp+c.enc.NumOffset, w)
	if c.scalar(x) {
		// As amd64's: no pointer word to read.
		c.a.Store(base, disp+c.enc.RefOffset, arm64.ZR)
		return
	}
	c.pointerWord(x, w)
	c.a.Store(base, disp+c.enc.RefOffset, a64B)
}

// windowSuper is amd64's: register code's super(...), its callee's window
// at locals given the function whose code runs and the instance, its
// context, ctx, what a super()'s has. It uses a and b.
func (c *a64Compiler) windowSuper(t *ssa.CallSite, locals, ctx, a, b arm64.Reg) {
	vs := int32(c.enc.ValueSize)
	fn := derivedFunction(t)
	c.a.Load(ctx, a64Ctx, abi.OffNext)
	c.movImm(b, c.enc.Object)
	c.a.Store(locals, c.enc.NumOffset, b)
	c.a.Store(locals, vs+c.enc.NumOffset, b)
	c.movImm(b, uint64(fn))
	c.a.Store(locals, c.enc.RefOffset, b)
	c.a.Load(a, a64Ctx, abi.OffInstance+c.enc.RefOffset)
	c.a.Store(locals, vs+c.enc.RefOffset, a)
	if t.Derived {
		c.a.Store(ctx, abi.OffInstance+c.enc.RefOffset, a)
		c.movImm(b, c.enc.Object)
		c.a.Store(ctx, abi.OffInstance+c.enc.NumOffset, b)
		c.a.Store(ctx, abi.OffThis+c.enc.RefOffset, arm64.ZR)
		c.movImm(b, c.enc.Uninitialized)
		c.a.Store(ctx, abi.OffThis+c.enc.NumOffset, b)
	} else {
		c.a.Store(ctx, abi.OffThis+c.enc.RefOffset, a)
		c.movImm(b, c.enc.Object)
		c.a.Store(ctx, abi.OffThis+c.enc.NumOffset, b)
	}
	c.a.Load(a, a64Ctx, abi.OffNewTarget)
	c.a.Store(ctx, abi.OffNewTarget, a)
	c.movImm(a, uint64(fn))
	c.a.Store(ctx, abi.OffFunction, a)
}

// windowForward is amd64's: register code's f.apply(x, arguments), its
// callee's parameters the frame's arguments. It uses registers 6 to 8,
// a64A and a64C.
func (c *a64Compiler) windowForward(t *ssa.CallSite, locals, ctx arm64.Reg) {
	vs := int32(c.enc.ValueSize)
	c.a.Load(ctx, a64Ctx, abi.OffNext)
	c.a.AddImm(locals, locals, int64(2*vs), true)
	param := *t
	param.Params, param.LocalCount = t.Params-2, t.LocalCount-2
	c.forwardArgs(&param, locals, ctx)
	c.a.AddImm(locals, locals, int64(-2*vs), true)
	if t.ArrowThis {
		a, b := arm64.Reg(7), arm64.Reg(8)
		c.a.Load(b, a64Ctx, abi.OffCallThis)
		c.a.Load(a, b, c.enc.NumOffset)
		c.a.Store(locals, vs+c.enc.NumOffset, a)
		c.a.Load(a, b, c.enc.RefOffset)
		c.a.Store(locals, vs+c.enc.RefOffset, a)
	}
}

// windowArgs is amd64's: a register callee that keeps its arguments
// given them in its context, ctx, from its window at locals. It uses a.
func (c *a64Compiler) windowArgs(t *ssa.CallSite, locals, ctx, a arm64.Reg) {
	vs := int32(c.enc.ValueSize)
	num, ref := int32(unsafe.Offsetof(abi.GoArg{}.Num)), int32(unsafe.Offsetof(abi.GoArg{}.Ref))
	c.a.Load(ctx, a64Ctx, abi.OffNext)
	argc := windowArgc(t)
	c.movImm(a, uint64(argc))
	c.a.Store(ctx, abi.OffArgc, a)
	for i := range argc {
		at := abi.OffArgs + int32(i)*int32(unsafe.Sizeof(abi.GoArg{}))
		c.a.Load(a, locals, int32(2+i)*vs+c.enc.NumOffset)
		c.a.Store(ctx, at+num, a)
		c.a.Load(a, locals, int32(2+i)*vs+c.enc.RefOffset)
		c.a.Store(ctx, at+ref, a)
	}
}

// genWindowSetup is amd64's: a generic call's of register code, its
// callee's window at locals, its link in a64CallLink. It uses ctx, a, b
// and u.
func (c *a64Compiler) genWindowSetup(site *ssa.CallSite, locals, ctx, a, b, u arm64.Reg) {
	vs := int32(c.enc.ValueSize)
	loop, filled := c.a.NewLabel(), c.a.NewLabel()
	c.movImm(a64A, uint64(2+site.Argc))
	c.movImm(u, c.enc.Undefined)
	c.a.Bind(loop)
	c.a.Load(b, a64CallLink, c.enc.LinkParams)
	c.a.Cmp(a64A, b, true)
	c.a.BCond(arm64.HS, filled)
	c.a.ShiftImm(arm64.Lsl, a, a64A, 4, true)
	c.a.Op(arm64.Add, a, a, locals, true)
	c.a.Store(a, c.enc.NumOffset, u)
	c.a.Store(a, c.enc.RefOffset, arm64.ZR)
	c.a.AddImm(a64A, a64A, 1, true)
	c.a.B(loop)
	c.a.Bind(filled)
	notail := c.a.NewLabel()
	c.a.Load(ctx, a64Ctx, abi.OffNext)
	c.a.Load(a, a64CallLink, c.enc.LinkFlags)
	c.movImm(b, abi.LinkTail)
	c.a.Tst(a, b, true)
	c.a.BCond(arm64.EQ, notail)
	c.movImm(a, 1)
	c.a.Store(ctx, abi.OffTailFrame, a)
	c.a.Store(ctx, abi.OffFrameThis, arm64.ZR)
	c.a.Bind(notail)
	arrow, done := c.a.NewLabel(), c.a.NewLabel()
	c.a.Load(a, a64CallLink, c.enc.LinkFlags)
	c.movImm(b, abi.LinkArrow)
	c.a.Tst(a, b, true)
	c.a.BCond(arm64.NE, arrow)
	if !site.Method {
		c.a.Store(locals, vs+c.enc.NumOffset, u)
		c.a.Store(locals, vs+c.enc.RefOffset, arm64.ZR)
	}
	c.a.B(done)
	c.a.Bind(arrow)
	c.a.Load(b, a64Ctx, abi.OffCallThis)
	c.a.Load(a, b, c.enc.NumOffset)
	c.a.Store(locals, vs+c.enc.NumOffset, a)
	c.a.Load(a, b, c.enc.RefOffset)
	c.a.Store(locals, vs+c.enc.RefOffset, a)
	c.a.Bind(done)
}

// windowSetup is amd64's: what register code's native call of t
// (ssa.CallSite's Window) gives its callee's window, at locals, besides
// the operands the call wrote there -- undefined in its parameters past
// the arguments, and a plain call's receiver: an arrow's captured this,
// the global object or undefined. It uses a and b.
func (c *a64Compiler) windowSetup(t *ssa.CallSite, locals, a, b arm64.Reg) {
	vs := int32(c.enc.ValueSize)
	if n := windowArgc(t); 2+n < t.Params && 2+n < t.LocalCount {
		c.movImm(a, c.enc.Undefined)
	}
	for i := 2 + windowArgc(t); i < t.Params && i < t.LocalCount; i++ {
		c.a.Store(locals, int32(i)*vs+c.enc.NumOffset, a)
		c.a.Store(locals, int32(i)*vs+c.enc.RefOffset, arm64.ZR)
	}
	switch {
	case t.Pool != 0:
		// A construction's, from its pool (call's setup).
	case t.ArrowThis:
		c.a.Load(b, a64Ctx, abi.OffCallThis)
		c.a.Load(a, b, c.enc.NumOffset)
		c.a.Store(locals, vs+c.enc.NumOffset, a)
		c.a.Load(a, b, c.enc.RefOffset)
		c.a.Store(locals, vs+c.enc.RefOffset, a)
	case t.Method:
	case t.Global != 0:
		c.movImm(a, c.enc.Object)
		c.a.Store(locals, vs+c.enc.NumOffset, a)
		c.movImm(a, uint64(t.Global))
		c.a.Store(locals, vs+c.enc.RefOffset, a)
	default:
		c.movImm(a, c.enc.Undefined)
		c.a.Store(locals, vs+c.enc.NumOffset, a)
		c.a.Store(locals, vs+c.enc.RefOffset, arm64.ZR)
	}
}

// pointerWord leaves in B the pointer word of a value whose number word is
// in w, as amd64's does. It uses C.
func (c *a64Compiler) pointerWord(x *ssa.Value, w arm64.Reg) {
	switch o, static := c.origin.Of(x); {
	case x.Shadow != nil && cellSource(x.Shadow):
		c.a.Load(a64B, c.gpr(x.Shadow, a64C), c.enc.RefOffset)
	case x.Shadow != nil:
		done := c.a.NewLabel()
		c.movImm(a64B, 0)
		r := c.gpr(x.Shadow, a64C)
		c.a.Cbz(r, done, true)
		c.a.Load(a64B, r, c.enc.RefOffset)
		c.a.Bind(done)
	case static && o >= 0 && x.Op == ssa.OpLoadSlot:
		// As amd64's: what the slot held at entry, its pointer word read
		// once.
		base, disp := c.slotAddr(o, true, a64C)
		c.a.Load(a64B, base, disp)
	case static && o >= 0:
		done := c.a.NewLabel()
		c.movImm(a64B, 0)
		c.isReference(x, w, o, done)
		base, disp := c.slotAddr(o, true, a64C)
		c.a.Load(a64B, base, disp)
		c.a.Bind(done)
	default:
		c.movImm(a64B, 0)
	}
}

// goCall calls Go, as amd64's does.
func (c *a64Compiler) goCall(v *ssa.Value, site *ssa.CallSite, guard a64Guard) {
	if (site.Copies != 0 || site.DefineNext || site.ArrayIter != 0 || site.Append) && len(v.Args) == 2 && c.enc.ValueSize == 16 && c.enc.WriteBarrier != 0 && c.enc.ShapeNext != 0 {
		// As amd64's: an object spread by the layouts where the VM has
		// made one so, a literal's field along its shape's first
		// transition, an array spread by its elements, an element
		// appended, any other Go's.
		// Each alike whether the collector marks or not (byMarking).
		slow, join := c.a.NewLabel(), c.a.NewLabel()
		size := int32(unsafe.Sizeof(abi.GoArg{}))
		c.goArg(abi.OffGoArgs, v.Args[0])
		c.goArg(abi.OffGoArgs+size, v.Args[1])
		c.byMarking(join, func(marking bool) {
			switch {
			case site.DefineNext:
				c.defineNext(v, site, slow, marking)
			case site.ArrayIter != 0:
				c.spreadElems(v, site, slow, marking)
			case site.Append:
				c.appendElem(v, slow, marking)
			default:
				c.copyProps(v, site, slow, marking)
			}
		})
		c.a.Bind(slow)
		c.goCallOp(v, goOpWord(site), false)
		c.a.Bind(join)
		c.a.Load(a64A, a64Ctx, abi.OffKeep+int32(v.Index)*int32(c.enc.ValueSize)+c.enc.NumOffset)
		c.setG(v, a64A)
		return
	}
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
	if argReads(site, c.f, c.enc) {
		slow, join := c.a.NewLabel(), c.a.NewLabel()
		c.argRead(v, site, slow, join)
		c.a.Bind(slow)
		c.goCallOp(v, goOpWord(site), false)
		c.a.Bind(join)
		c.a.Load(a64A, a64Ctx, abi.OffKeep+int32(v.Index)*int32(c.enc.ValueSize)+c.enc.NumOffset)
		c.setG(v, a64A)
		return
	}
	if keyedReads(site, len(v.Args), c.f, c.enc) {
		slow, join := c.a.NewLabel(), c.a.NewLabel()
		c.keyedRead(v, slow)
		c.a.B(join)
		c.a.Bind(slow)
		c.goCallOp(v, goOpWord(site), false)
		c.a.Bind(join)
		c.a.Load(a64A, a64Ctx, abi.OffKeep+int32(v.Index)*int32(c.enc.ValueSize)+c.enc.NumOffset)
		c.setG(v, a64A)
		return
	}
	if site.Go == ssa.GoHas && len(v.Args) == 2 && c.enc.ValueSize == 16 {
		slow, join := c.a.NewLabel(), c.a.NewLabel()
		c.keyedHas(v, slow)
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
	if genForOfs(v, c.enc) {
		// As amd64's: a for...of's cursor, a generator's room's (genForOf).
		slow, join := c.a.NewLabel(), c.a.NewLabel()
		c.genForOf(v, slow, join)
		c.a.Bind(slow)
		c.goCallOp(v, goOpWord(site), false)
		c.a.Bind(join)
		c.a.Load(a64A, a64Ctx, abi.OffKeep+int32(v.Index)*int32(c.enc.ValueSize)+c.enc.NumOffset)
		c.setG(v, a64A)
		return
	}
	if genMakes(v, c.enc) {
		// As amd64's: a call of a generator function, its generator from
		// its pool (genMake).
		slow, join := c.a.NewLabel(), c.a.NewLabel()
		c.genMake(v, slow, join)
		c.a.Bind(slow)
		c.goCallOp(v, goOpWord(site), false)
		c.a.Bind(join)
		c.a.Load(a64A, a64Ctx, abi.OffKeep+int32(v.Index)*int32(c.enc.ValueSize)+c.enc.NumOffset)
		c.setG(v, a64A)
		return
	}
	if genNexts(v, c.enc) {
		// As amd64's: a call of a generator's next natively (genNext).
		slow, join := c.a.NewLabel(), c.a.NewLabel()
		c.genNext(v, slow, join)
		c.a.Bind(slow)
		c.goCallOp(v, goOpWord(site), false)
		c.a.Bind(join)
		c.a.Load(a64A, a64Ctx, abi.OffKeep+int32(v.Index)*int32(c.enc.ValueSize)+c.enc.NumOffset)
		c.setG(v, a64A)
		return
	}
	if site.StepResult && site.ResultShape != 0 && c.enc.IterState != 0 {
		// As amd64's: a loop's step's answer native code reads itself.
		slow, join := c.a.NewLabel(), c.a.NewLabel()
		c.stepResult(v, slow, join)
		c.a.Bind(slow)
		c.goCallOp(v, goOpWord(site), false)
		c.a.Bind(join)
		c.a.Load(a64A, a64Ctx, abi.OffKeep+int32(v.Index)*int32(c.enc.ValueSize)+c.enc.NumOffset)
		c.setG(v, a64A)
		return
	}
	if forInStarts(site, len(v.Args), c.enc) {
		// As amd64's: a for...in's cursor taken from the runtime's last.
		slow, join := c.a.NewLabel(), c.a.NewLabel()
		c.forInStart(v, slow, join)
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
	if frameMades(site, c.f, c.enc) {
		slow, join := c.a.NewLabel(), c.a.NewLabel()
		c.frameMade(v, site, slow)
		c.a.B(join)
		c.a.Bind(slow)
		c.goCallOp(v, goOpWord(site), false)
		c.a.Bind(join)
		return
	}
	if site.Go == ssa.GoCells && site.Pool != 0 {
		// As amd64's: natively from the function's pool where it can be,
		// by Go otherwise.
		slow, join := c.a.NewLabel(), c.a.NewLabel()
		c.cellsTake(v, site, slow)
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
		// can be, by Go otherwise; nothing in a native call's frame of code
		// whose cells go back to their pool.
		slow, join := c.a.NewLabel(), c.a.NewLabel()
		if c.f.CellsBack && !c.f.CellsKept {
			c.a.Load(a64A, a64Ctx, abi.OffLive)
			c.a.CmpImm(a64A, abi.LiveCall, true)
			c.a.BCond(arm64.EQ, join)
		}
		c.cellsClose(v, site, slow)
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
	if createMakes(v, c.enc) {
		// As amd64's: Object.create's object from its pool (createMake).
		slow, join := c.a.NewLabel(), c.a.NewLabel()
		c.createMake(v, slow, join)
		c.a.Bind(slow)
		c.leafCall(v, site)
		c.a.Bind(join)
		c.a.Load(a64A, a64Ctx, abi.OffKeep+int32(v.Index)*int32(c.enc.ValueSize)+c.enc.NumOffset)
		c.setG(v, a64A)
		return
	}
	if leafFast(site, c.enc) {
		c.leafCall(v, site)
		return
	}
	c.goCallOp(v, goOpWord(site), false)
}

// createMake is amd64's: the argument an object, the pool's prototype, the
// pool not empty, its next object the result; anything else to slow, the
// leaf's call. It uses every scratch register.
func (c *a64Compiler) createMake(v *ssa.Value, slow, done arm64.Label) {
	site := v.Calls()[0]
	x := v.Args[len(v.Args)-1]
	if x.Shadow == nil && c.origin.At(x) < 0 {
		c.a.B(slow)
		return
	}
	fail := a64GuardLabel(c, slow)
	c.movImm(a64B, c.enc.Object)
	c.a.Cmp(c.gpr(x, a64A), a64B, true)
	fail.jump(arm64.NE)
	c.sourceRef(x, fail)
	c.movImm(a64A, uint64(site.Pool))
	c.a.Load(a64B, a64A, abi.OffPoolProto)
	c.a.Cmp(a64C, a64B, true)
	fail.jump(arm64.NE)
	c.a.Load(a64B, a64A, abi.OffPoolCount)
	c.a.Cbz(a64B, slow, true)
	join := c.markingTake(v, site)
	c.poolTake(v, site)
	c.a.Bind(join)
	c.a.B(done)
}

// leafCall is amd64's: the description's address in C, where to come back
// in A, the result's number word back in B.
func (c *a64Compiler) leafCall(v *ssa.Value, site *ssa.CallSite) {
	st, _ := c.goState(goOpWord(site), v.Index, v, nil)
	c.leafCallOf(v, st, v.Args)
}

// pushGrow is amd64's: a push with no room for its values, push's leaf.
func (c *a64Compiler) pushGrow(v *ssa.Value, site *ssa.CallSite) {
	st, _ := c.goState(goOpWord(&ssa.CallSite{Go: ssa.GoLeaf, Argc: site.Argc, Method: true}), v.Index, v, nil)
	st.Leaf, st.Unused, st.Give = site.Callee, false, 0
	c.leafCallOf(v, st, append([]*ssa.Value{v.Args[1], v.Args[0]}, v.Args[2:]...))
}

// leafCallOf is amd64's: leafCall of st's leaf, its operands ops.
func (c *a64Compiler) leafCallOf(v *ssa.Value, st *abi.GoCallState, ops []*ssa.Value) {
	size := int32(unsafe.Sizeof(abi.GoArg{}))
	for i, x := range ops {
		if i != 0 {
			c.goArg(abi.OffGoArgs+int32(i)*size, x)
		}
	}
	for _, sv := range c.savesOf(v) {
		switch {
		case sv.frame != 0:
			// Read back from the frame's slot, which holds it.
		case sv.float:
			c.spillStoreF(sv.slot, arm64.FReg(sv.reg))
		default:
			c.spillStore(sv.slot, arm64.Reg(sv.reg))
		}
	}
	back, fail := c.a.NewLabel(), c.a.NewLabel()
	c.movImm(a64C, uint64(uintptr(unsafe.Pointer(st))))
	c.a.Adr(a64A, back)
	c.movImm(a64B, c.enc.LeafFast)
	c.a.Br(a64B)
	c.a.Bind(back)
	restore := func(refused bool) {
		c.a.Load(a64Locals, a64Ctx, abi.OffLocals)
		c.a.Load(a64Stack, a64Ctx, abi.OffStack)
		c.restoreSavesOf(v, refused)
	}
	restore(false)
	c.setG(v, a64B)
	stub := c.stubLabel(v.State, exitKind(v.Aux))
	c.goFails = append(c.goFails, a64GoFail{st, back, fail})
	c.cold = append(c.cold, func() {
		c.a.Bind(fail)
		restore(true)
		c.a.B(stub)
	})
}

// closureTake is amd64's: a closure taken from its pool, its captured
// bindings' cells and an arrow's this written. It uses A, B and C.
// cellsTake is amd64's: an entry's cells from the function's pool, where
// the collector does not mark and the context is a native call's. It uses
// A, B and C.
func (c *a64Compiler) cellsTake(v *ssa.Value, site *ssa.CallSite, slow arm64.Label) {
	marked, done, top := c.a.NewLabel(), c.a.NewLabel(), c.a.NewLabel()
	c.a.Bind(top)
	empty := c.poolRefill(v, site.Pool, top, slow)
	c.movImm(a64A, c.enc.WriteBarrier)
	c.a.LoadU8(a64A, a64A, 0)
	c.a.Cbnz(a64A, marked, false)
	c.cold = append(c.cold, func() {
		c.a.Bind(marked)
		c.cellsTakeMarking(site, slow, empty)
		c.a.B(done)
	})
	defer c.a.Bind(done)
	c.a.Load(a64A, a64Ctx, abi.OffLive)
	c.a.CmpImm(a64A, abi.LiveCall, true)
	c.a.BCond(arm64.NE, slow)
	c.movImm(a64A, uint64(site.Pool))
	c.a.Load(a64B, a64A, abi.OffPoolCount)
	c.a.Cbz(a64B, empty, true)
	c.a.AddImm(a64B, a64B, -1, true)
	c.a.Store(a64A, abi.OffPoolCount, a64B)
	c.a.ShiftImm(arm64.Lsl, a64B, a64B, 3, true)
	c.a.Op(arm64.Add, a64B, a64B, a64A, true)
	c.a.Load(a64C, a64B, abi.OffPoolObjects)
	c.a.Store(a64B, abi.OffPoolObjects, arm64.ZR)
	c.a.Store(a64Ctx, abi.OffCells, a64C)
	if c.f.CellsKept {
		c.a.Store(a64Ctx, abi.OffCellsKept, arm64.ZR)
	}
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
func (c *a64Compiler) cellsClose(v *ssa.Value, site *ssa.CallSite, slow arm64.Label) {
	marked, done, top := c.a.NewLabel(), c.a.NewLabel(), c.a.NewLabel()
	c.a.Bind(top)
	empty := c.poolRefill(v, site.Pool, top, slow)
	c.movImm(a64A, c.enc.WriteBarrier)
	c.a.LoadU8(a64A, a64A, 0)
	c.a.Cbnz(a64A, marked, false)
	c.cold = append(c.cold, func() {
		c.a.Bind(marked)
		c.cellsCloseMarking(site, empty)
		c.a.B(done)
	})
	defer c.a.Bind(done)
	c.movImm(a64A, uint64(site.Pool))
	c.a.Load(a64B, a64A, abi.OffPoolCount)
	c.a.Cbz(a64B, empty, true)
	c.a.AddImm(a64B, a64B, -1, true)
	c.a.Store(a64A, abi.OffPoolCount, a64B)
	c.a.ShiftImm(arm64.Lsl, a64B, a64B, 3, true)
	c.a.Op(arm64.Add, a64B, a64B, a64A, true)
	c.a.Load(a64C, a64B, abi.OffPoolObjects)
	c.a.Store(a64B, abi.OffPoolObjects, arm64.ZR)
	n := int32(len(site.Captures))
	for k, x := range site.Captures {
		if !closes(site, x) {
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
func (c *a64Compiler) cellsCloseMarking(site *ssa.CallSite, empty arm64.Label) {
	block := abi.OffGoArgs + c.enc.ValueSize + c.enc.RefOffset
	c.movImm(a64A, uint64(site.Pool))
	c.a.Load(a64B, a64A, abi.OffPoolCount)
	c.a.Cbz(a64B, empty, true)
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
		if !closes(site, x) {
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
func (c *a64Compiler) cellsTakeMarking(site *ssa.CallSite, slow, empty arm64.Label) {
	c.a.Load(a64A, a64Ctx, abi.OffLive)
	c.a.CmpImm(a64A, abi.LiveCall, true)
	c.a.BCond(arm64.NE, slow)
	c.movImm(a64A, uint64(site.Pool))
	c.a.Load(a64B, a64A, abi.OffPoolCount)
	c.a.Cbz(a64B, empty, true)
	c.a.AddImm(a64B, a64B, -1, true)
	c.a.Store(a64A, abi.OffPoolCount, a64B)
	c.a.ShiftImm(arm64.Lsl, a64B, a64B, 3, true)
	c.a.Op(arm64.Add, a64B, a64B, a64A, true)
	c.a.Load(a64C, a64B, abi.OffPoolObjects)
	c.a.Store(a64Ctx, abi.OffCells, a64C)
	if c.f.CellsKept {
		c.a.Store(a64Ctx, abi.OffCellsKept, arm64.ZR)
	}
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
func (c *a64Compiler) closureTakeMarking(v *ssa.Value, site *ssa.CallSite, empty arm64.Label) {
	at := abi.OffKeep + int32(v.Index)*int32(c.enc.ValueSize)
	c.movImm(a64A, uint64(site.Pool))
	c.a.Load(a64B, a64A, abi.OffPoolCount)
	c.a.Cbz(a64B, empty, true)
	c.poolTakeMarking(v, site)
	data := func() {
		c.a.Load(a64C, a64Ctx, at+c.enc.RefOffset)
		c.a.Load(a64C, a64C, c.enc.ObjectExt+c.enc.ObjectData+8)
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
		base, at := c.lexThisAt(site)
		data()
		c.a.Load(a64B, a64C, c.enc.FuncExtra)
		c.a.Load(a64A, base, at+c.enc.NumOffset)
		c.a.Store(a64B, c.enc.ExtraLexThis+c.enc.NumOffset, a64A)
		c.a.AddImm(a64A, a64B, int64(c.enc.ExtraLexThis+c.enc.RefOffset), true)
		c.a.Load(a64C, base, at+c.enc.RefOffset)
		c.refStore()
	}
	c.movImm(a64A, c.enc.Object)
	c.setG(v, a64A)
}

func (c *a64Compiler) closureTake(v *ssa.Value, site *ssa.CallSite, slow arm64.Label) {
	if c.f.CellsKept && capturesCells(site) {
		c.movImm(a64A, 1)
		c.a.Store(a64Ctx, abi.OffCellsKept, a64A)
	}
	marked, done, top := c.a.NewLabel(), c.a.NewLabel(), c.a.NewLabel()
	c.a.Bind(top)
	empty := c.poolRefill(v, site.Pool, top, slow)
	c.movImm(a64A, c.enc.WriteBarrier)
	c.a.LoadU8(a64A, a64A, 0)
	c.a.Cbnz(a64A, marked, false)
	c.cold = append(c.cold, func() {
		c.a.Bind(marked)
		c.closureTakeMarking(v, site, empty)
		c.a.B(done)
	})
	defer c.a.Bind(done)
	c.movImm(a64A, uint64(site.Pool))
	c.a.Load(a64B, a64A, abi.OffPoolCount)
	c.a.Cbz(a64B, empty, true)
	c.poolTake(v, site)
	c.a.Load(a64C, a64C, c.enc.ObjectExt+c.enc.ObjectData+8)
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
		base, at := c.lexThisAt(site)
		c.a.Load(a64B, a64C, c.enc.FuncExtra)
		for w := int32(0); w < c.enc.ValueSize; w += 8 {
			c.a.Load(a64A, base, at+w)
			c.a.Store(a64B, c.enc.ExtraLexThis+w, a64A)
		}
	}
}

// lexThisAt is amd64's: the frame's register 1 for register code's
// (ssa.CallSite's LexThisReg), or the context's This.
func (c *a64Compiler) lexThisAt(site *ssa.CallSite) (arm64.Reg, int32) {
	if site.LexThisReg {
		return a64Locals, c.enc.ValueSize
	}
	return a64Ctx, abi.OffThis
}

// genStep is amd64's, the cursor's state in D: a cursor over a generator
// of one of the functions the step has met, resumed by a native call of
// its code (genCall), or on to notGen, D as it was, where its next is not
// the realm's generators'. It uses A, B, C and D, and no other, until the
// call.
func (c *a64Compiler) genStep(v *ssa.Value, notGen, slow, done arm64.Label) {
	site := v.Calls()[0]
	fail := a64GuardLabel(c, slow)
	c.a.Load(a64B, a64D, c.enc.IterNext+c.enc.RefOffset)
	c.movImm(a64C, uint64(site.GenNext))
	c.a.Cmp(a64B, a64C, true)
	c.a.BCond(arm64.NE, notGen)
	c.a.Load(a64C, a64D, c.enc.IterIter+c.enc.NumOffset)
	c.movImm(a64B, c.enc.Object)
	c.a.Cmp(a64C, a64B, true)
	fail.jump(arm64.NE)
	c.a.Load(a64C, a64D, c.enc.IterIter+c.enc.RefOffset)
	c.objectPart(a64B, a64C, a64A, c.enc.ClassGenerator, c.enc.GenObject, fail)
	c.a.LoadU8(a64C, a64B, c.enc.GenState)
	c.a.CmpImm(a64C, int64(c.enc.GenSuspendedYield), false)
	fail.jump(arm64.HI)
	c.a.LoadU8(a64C, a64B, c.enc.GenDelegating)
	c.a.Cbnz(a64C, slow, false)
	c.a.Load(a64C, a64B, c.enc.GenRare)
	c.a.Cbnz(a64C, slow, true)
	// A context for the generator's frame.
	c.a.Load(a64C, a64Ctx, abi.OffLevel)
	c.a.AddImm(a64C, a64C, 1, true)
	c.a.Load(a64A, a64Ctx, abi.OffBounds)
	c.a.Load(a64A, a64A, abi.OffBoundsLevelLimit)
	c.a.Cmp(a64C, a64A, true)
	fail.jump(arm64.HS)
	// The generator and the cursor, where the generator's code reads them
	// (genEntry): the next context's, which no call runs in yet. There is
	// one only below the level limit, so they are stored after its check,
	// as amd64's genCall stores them.
	c.a.Load(a64C, a64Ctx, abi.OffNext)
	c.a.Store(a64C, abi.OffGenObject, a64B)
	c.a.Store(a64C, abi.OffGenCursor, a64D)
	// A loop sends undefined.
	c.movImm(a64A, c.enc.Undefined)
	c.a.Store(a64C, abi.OffGenSent+c.enc.NumOffset, a64A)
	c.a.Store(a64C, abi.OffGenSent+c.enc.RefOffset, arm64.ZR)
	c.a.Load(a64D, a64B, c.enc.GenClosure)
	for _, t := range site.Gens {
		next := c.a.NewLabel()
		c.a.Load(a64C, a64D, c.enc.ClosureFn)
		c.movImm(a64A, uint64(t.Fn))
		c.a.Cmp(a64C, a64A, true)
		c.a.BCond(arm64.NE, next)
		// Its environment, its template's, and its realm.
		for _, f := range [...]struct {
			off  int32
			code bool
			want uintptr
		}{{c.enc.CodeEnv, true, t.Env}, {c.enc.ClosureRealm, false, t.Realm}} {
			from := a64D
			if f.code {
				c.a.Load(a64C, a64D, c.enc.ClosureCode)
				from = a64C
			}
			c.a.Load(a64C, from, f.off)
			c.movImm(a64A, uint64(f.want))
			c.a.Cmp(a64C, a64A, true)
			fail.jump(arm64.NE)
		}
		c.genCall(v, t, slow, done)
		c.a.Bind(next)
	}
	c.a.B(slow)
}

// genCall is amd64's, the generator's closure in D: the call of the
// generator's code at its generator entry, its state described, what is
// live saved; its result to the step's keep cell, or, at its miss, Go's
// step.
func (c *a64Compiler) genCall(v *ssa.Value, t ssa.GenStep, slow, done arm64.Label) {
	site := v.Calls()[0]
	vs := int32(c.enc.ValueSize)
	c.movImm(a64C, uint64(t.Entry))
	c.a.Load(a64C, a64C, 0)
	c.a.Cbz(a64C, slow, true)
	c.a.Store(a64Ctx, abi.OffGenEntry, a64C)
	c.a.Store(a64Ctx, abi.OffCallClosure, a64D)
	st, recs := c.callState(v, v.State, func(int) bool { return false }, -1)
	c.callRecords(v, recs)
	c.movImm(a64A, uint64(uintptr(unsafe.Pointer(st))))
	c.a.Store(a64Ctx, abi.OffCallState, a64A)
	saves := c.savesOf(v)
	for _, sv := range saves {
		if sv.float {
			c.spillStoreF(sv.slot, arm64.FReg(sv.reg))
		} else {
			c.spillStore(sv.slot, arm64.Reg(sv.reg))
		}
	}
	// As amd64's: the cursor's word, where a register holds it, for the
	// miss.
	x, kept := v.Args[0], -1
	if l := c.locAt(x); !c.isLazy(x) && l.reg >= 0 {
		kept = l.reg
		c.a.Store(a64Ctx, abi.OffGoArgs+c.enc.NumOffset, arm64.Reg(kept))
	}
	restore := func() {
		for _, sv := range saves {
			if sv.float {
				c.spillLoadF(arm64.FReg(sv.reg), sv.slot)
			} else {
				c.spillLoad(arm64.Reg(sv.reg), sv.slot)
			}
		}
	}
	back, miss := c.a.NewLabel(), c.a.NewLabel()
	c.resumeUsed = true
	c.a.Adr(a64CallRet, back)
	c.a.Load(a64CallClosure, a64Ctx, abi.OffCallClosure)
	c.movImm(a64CallLink, uint64(t.Entry)-uint64(c.enc.LinkGen))
	c.a.Adr(arm64.Reg(10), miss)
	c.a.Load(a64D, a64Ctx, abi.OffGenEntry)
	c.a.Br(a64D)
	// The generator returned, or Go finished it: its result's words in
	// a64RetNum and a64RetRef.
	c.a.Bind(back)
	if site.Fill {
		value := c.a.NewLabel()
		c.movImm(a64B, c.enc.Uninitialized)
		c.a.Cmp(a64RetNum, a64B, true)
		c.a.BCond(arm64.NE, value)
		c.movImm(a64RetNum, c.enc.Undefined)
		c.a.MovImm(a64RetRef, 0)
		c.a.Bind(value)
	}
	at := abi.OffKeep + int32(v.Index)*vs
	marked, stored := c.a.NewLabel(), c.a.NewLabel()
	c.movImm(a64B, c.enc.WriteBarrier)
	c.a.LoadU8(a64B, a64B, 0)
	c.a.Cbnz(a64B, marked, false)
	c.a.Store(a64Ctx, at+c.enc.RefOffset, a64RetRef)
	c.a.Store(a64Ctx, at+c.enc.NumOffset, a64RetNum)
	c.a.Bind(stored)
	restore()
	c.a.B(done)
	c.cold = append(c.cold, func() {
		c.a.Bind(marked)
		c.a.Store(a64Ctx, at+c.enc.NumOffset, a64RetNum)
		c.a.AddImm(a64A, a64Ctx, int64(at+c.enc.RefOffset), true)
		c.refStore()
		c.a.B(stored)
	})
	// Its code could not resume it: Go steps it, from the registers as
	// they were.
	c.a.Bind(miss)
	restore()
	if kept >= 0 {
		c.a.Load(arm64.Reg(kept), a64Ctx, abi.OffGoArgs+c.enc.NumOffset)
	}
	c.a.B(slow)
}

// genForOf is amd64's: a for...of's cursor over a generator, the one in its
// room. It uses A, B, C and D.
func (c *a64Compiler) genForOf(v *ssa.Value, slow, done arm64.Label) {
	site := v.Calls()[0]
	fail := a64GuardLabel(c, slow)
	x := v.Args[0]
	r := c.gpr(x, a64A)
	c.movImm(a64C, c.enc.Object)
	c.a.Cmp(r, a64C, true)
	fail.jump(arm64.NE)
	c.pointerWord(x, r)
	c.a.Cbz(a64B, slow, true)
	c.a.MovRR(a64D, a64B)
	c.a.LoadU8(a64A, a64D, c.enc.ObjectClass)
	c.a.CmpImm(a64A, int64(c.enc.ClassGenerator), false)
	fail.jump(arm64.NE)
	c.a.LoadU8(a64A, a64D, c.enc.GenObject+c.enc.GenRoom)
	c.a.Cbz(a64A, slow, false)
	c.a.LoadU32(a64A, a64D, c.enc.ObjectPropsLen)
	c.a.Cbnz(a64A, slow, false)
	c.loadProtoPlain(a64C, a64D)
	c.a.Cbz(a64C, slow, true)
	c.a.LoadU8(a64A, a64C, c.enc.ObjectClass)
	c.a.CmpImm(a64A, int64(c.enc.ClassObject), false)
	fail.jump(arm64.NE)
	c.a.LoadU32(a64A, a64C, c.enc.ObjectPropsLen)
	c.a.Cbnz(a64A, slow, true)
	c.loadProtoPlain(a64C, a64C)
	c.a.Cbz(a64C, slow, true)
	c.movImm(a64A, uint64(site.GenIter))
	c.a.Load(a64B, a64A, c.enc.GenIterFrom)
	c.a.Cmp(a64C, a64B, true)
	fail.jump(arm64.NE)
	c.a.Load(a64C, a64A, c.enc.GenIterCell)
	c.a.Cbz(a64C, slow, true)
	c.a.LoadU8(a64C, a64C, 0)
	c.a.Cbz(a64C, slow, false)
	for _, k := range [...]struct {
		obj, idx int32
		want     uintptr
	}{{c.enc.GenIterNextObj, c.enc.GenIterNextIdx, site.GenNext}, {c.enc.GenIterIterObj, c.enc.GenIterIterIdx, site.GenIterFn}} {
		c.movImm(a64A, uint64(site.GenIter))
		c.a.Load(a64C, a64A, k.obj)
		c.a.Cbz(a64C, slow, true)
		c.a.LoadU32(a64A, a64A, k.idx)
		c.a.LoadU32(a64B, a64C, c.enc.ObjectPropsLen)
		c.a.Cmp(a64A, a64B, true)
		fail.jump(arm64.HS)
		c.a.Load(a64B, a64C, c.enc.ObjectProps)
		c.a.ShiftImm(arm64.Lsl, a64A, a64A, 4, true)
		c.a.Op(arm64.Add, a64B, a64B, a64A, true)
		c.a.Load(a64B, a64B, c.enc.RefOffset)
		c.movImm(a64A, uint64(k.want))
		c.a.Cmp(a64B, a64A, true)
		fail.jump(arm64.NE)
	}
	c.a.AddImm(a64C, a64D, int64(c.enc.GenRoomCursor), true)
	c.a.MovImm(a64A, uint64(c.enc.ClassCursor))
	c.a.Store8(a64C, c.enc.ObjectClass, a64A)
	st := c.enc.IterState
	c.movImm(a64A, c.enc.Object)
	c.a.Store(a64C, st+c.enc.IterIter+c.enc.NumOffset, a64A)
	c.a.Store(a64C, st+c.enc.IterNext+c.enc.NumOffset, a64A)
	c.a.Store8(a64D, c.enc.GenObject+c.enc.GenRoom, arm64.ZR)
	keep := abi.OffKeep + int32(v.Index)*int32(c.enc.ValueSize)
	c.a.Store(a64Ctx, keep+c.enc.NumOffset, a64A)
	// As amd64's: the pointer words, and the cursor to the keep cell, with
	// Go's write barrier while the collector marks, the cursor read again
	// from the keep cell.
	marked := c.a.NewLabel()
	c.movImm(a64A, c.enc.WriteBarrier)
	c.a.LoadU8(a64A, a64A, 0)
	c.a.Cbnz(a64A, marked, false)
	c.a.Store(a64C, st+c.enc.IterIter+c.enc.RefOffset, a64D)
	c.movImm(a64A, uint64(site.GenNext))
	c.a.Store(a64C, st+c.enc.IterNext+c.enc.RefOffset, a64A)
	c.a.Store(a64Ctx, keep+c.enc.RefOffset, a64C)
	c.a.B(done)
	c.cold = append(c.cold, func() {
		c.a.Bind(marked)
		c.a.AddImm(a64A, a64Ctx, int64(keep+c.enc.RefOffset), true)
		c.refStore()
		c.a.Load(a64C, a64Ctx, keep+c.enc.RefOffset)
		c.a.AddImm(a64A, a64C, int64(st+c.enc.IterIter+c.enc.RefOffset), true)
		c.a.AddImm(a64C, a64C, -int64(c.enc.GenRoomCursor), true)
		c.refStore()
		c.a.Load(a64C, a64Ctx, keep+c.enc.RefOffset)
		c.a.AddImm(a64A, a64C, int64(st+c.enc.IterNext+c.enc.RefOffset), true)
		c.movImm(a64C, uint64(site.GenNext))
		c.refStore()
		c.a.B(done)
	})
}

// genMake is amd64's: a call of a generator function, the pool's next
// generator, the receiver and the arguments written to it. It uses A, B,
// C, D and F2, which holds the generator while the arguments are read: not
// F1, which gpr loads a spilled number into to box it (materialize).
func (c *a64Compiler) genMake(v *ssa.Value, slow, done arm64.Label) {
	site := v.Calls()[0]
	vs := int32(c.enc.ValueSize)
	fail := a64GuardLabel(c, slow)
	at := 0
	if site.Method {
		at = 1
	}
	callee := v.Args[at]
	c.pointerWord(callee, c.gpr(callee, a64A))
	c.movImm(a64C, uint64(site.GenMake))
	c.a.Cmp(a64B, a64C, true)
	fail.jump(arm64.NE)
	// As amd64's: its prototype the pool's, a generator in the pool.
	c.a.LoadU32(a64A, a64B, c.enc.ObjectPropsLen)
	c.a.CmpImm(a64A, int64(site.ProtoIndex), true)
	fail.jump(arm64.LS)
	c.a.Load(a64A, a64B, c.enc.ObjectShape)
	c.a.Load(a64A, a64A, c.enc.ShapeKeys)
	c.a.LoadU32(a64A, a64A, int32(site.ProtoIndex)*c.enc.KeySize+c.enc.PropertyKey)
	c.movImm(a64C, uint64(site.ProtoKey))
	c.a.Cmp(a64A, a64C, false)
	fail.jump(arm64.NE)
	c.a.Load(a64A, a64B, c.enc.ObjectProps)
	c.a.Load(a64C, a64A, int32(site.ProtoIndex)*c.enc.ValueSize+c.enc.RefOffset)
	c.movImm(a64A, uint64(site.Pool))
	c.a.Load(a64B, a64A, abi.OffPoolProto)
	c.a.Cmp(a64C, a64B, true)
	fail.jump(arm64.NE)
	c.a.Load(a64B, a64A, abi.OffPoolCount)
	c.a.Cbz(a64B, slow, true)
	marked := c.a.NewLabel()
	c.movImm(a64C, c.enc.WriteBarrier)
	c.a.LoadU8(a64C, a64C, 0)
	c.a.Cbnz(a64C, marked, false)
	c.a.AddImm(a64B, a64B, -1, true)
	c.a.Store(a64A, abi.OffPoolCount, a64B)
	c.a.ShiftImm(arm64.Lsl, a64B, a64B, 3, true)
	c.a.Op(arm64.Add, a64B, a64B, a64A, true)
	c.a.Load(a64D, a64B, abi.OffPoolObjects)
	c.a.Store(a64B, abi.OffPoolObjects, arm64.ZR)
	c.a.FMovToF(a64F2, a64D)
	put := func(x *ssa.Value, off int32, vals bool) {
		r := c.gpr(x, a64A)
		if c.scalar(x) {
			c.movImm(a64B, 0)
		} else {
			c.pointerWord(x, r)
		}
		c.a.FMovFromF(a64D, a64F2)
		if vals {
			c.a.Load(a64D, a64D, c.enc.GenObject+c.enc.GenVals)
		}
		c.a.Store(a64D, off+c.enc.NumOffset, r)
		c.a.Store(a64D, off+c.enc.RefOffset, a64B)
	}
	genMakeArgs(v, c.enc, put)
	keep := abi.OffKeep + int32(v.Index)*vs
	c.a.FMovFromF(a64D, a64F2)
	c.a.Store(a64Ctx, keep+c.enc.RefOffset, a64D)
	c.movImm(a64A, c.enc.Object)
	c.a.Store(a64Ctx, keep+c.enc.NumOffset, a64A)
	c.a.B(done)
	c.cold = append(c.cold, func() {
		c.a.Bind(marked)
		c.genMakeMarking(v)
		c.a.B(done)
	})
}

// genMakeMarking is amd64's: the receiver's and the arguments' pointer
// words written with Go's write barrier (refStore) into the generator
// while the pool still holds it, read again from the pool at each, then
// the generator taken (poolTakeMarking) and kept.
func (c *a64Compiler) genMakeMarking(v *ssa.Value) {
	site := v.Calls()[0]
	genMakeArgs(v, c.enc, func(x *ssa.Value, off int32, vals bool) {
		r := c.gpr(x, a64A)
		if c.scalar(x) {
			c.movImm(a64B, 0)
		} else {
			c.pointerWord(x, r)
		}
		c.a.MovRR(a64D, r)
		c.a.FMovToF(a64F0, a64B)
		c.poolNext(site.Pool)
		if vals {
			c.a.Load(a64C, a64C, c.enc.GenObject+c.enc.GenVals)
		}
		c.a.Store(a64C, off+c.enc.NumOffset, a64D)
		c.a.AddImm(a64A, a64C, int64(off+c.enc.RefOffset), true)
		c.a.FMovFromF(a64C, a64F0)
		c.refStore()
	})
	c.poolTakeMarking(v, site)
}

// genNext is amd64's: a call of the realm's generators' next on a generator
// of one of the functions the call has met, by a native call of its code
// (genNextCall); anything else to slow, Go's. The value sent in waits in
// F0 and F2 for the checks: not F1, which gpr loads a spilled number into
// to box it (materialize).
func (c *a64Compiler) genNext(v *ssa.Value, slow, done arm64.Label) {
	site := v.Calls()[0]
	fail := a64GuardLabel(c, slow)
	recv, callee := v.Args[0], v.Args[1]
	if site.Argc == 1 {
		x := v.Args[2]
		r := c.gpr(x, a64A)
		c.a.FMovToF(a64F0, r)
		if c.scalar(x) {
			c.movImm(a64B, 0)
		} else {
			c.pointerWord(x, r)
		}
		c.a.FMovToF(a64F2, a64B)
	}
	// The function called, the realm's next.
	c.pointerWord(callee, c.gpr(callee, a64A))
	c.movImm(a64C, uint64(site.GenNext))
	c.a.Cmp(a64B, a64C, true)
	fail.jump(arm64.NE)
	// The receiver, a generator.
	r := c.gpr(recv, a64A)
	c.movImm(a64C, c.enc.Object)
	c.a.Cmp(r, a64C, true)
	fail.jump(arm64.NE)
	c.pointerWord(recv, r)
	c.a.Cbz(a64B, slow, true)
	c.objectPart(a64B, a64B, a64C, c.enc.ClassGenerator, c.enc.GenObject, fail)
	c.a.LoadU8(a64C, a64B, c.enc.GenState)
	c.a.CmpImm(a64C, int64(c.enc.GenSuspendedYield), false)
	fail.jump(arm64.HI)
	c.a.LoadU8(a64C, a64B, c.enc.GenDelegating)
	c.a.Cbnz(a64C, slow, false)
	c.a.Load(a64C, a64B, c.enc.GenRare)
	c.a.Cbnz(a64C, slow, true)
	// A context for the generator's frame.
	c.a.Load(a64C, a64Ctx, abi.OffLevel)
	c.a.AddImm(a64C, a64C, 1, true)
	c.a.Load(a64A, a64Ctx, abi.OffBounds)
	c.a.Load(a64A, a64A, abi.OffBoundsLevelLimit)
	c.a.Cmp(a64C, a64A, true)
	fail.jump(arm64.HS)
	// The generator, no cursor and the value sent in, where the
	// generator's code reads them (genEntry): the next context's, there
	// below the level limit.
	c.a.Load(a64C, a64Ctx, abi.OffNext)
	c.a.Store(a64C, abi.OffGenObject, a64B)
	c.a.Store(a64C, abi.OffGenCursor, arm64.ZR)
	if site.Argc == 1 {
		c.a.FMovFromF(a64A, a64F0)
		c.a.Store(a64C, abi.OffGenSent+c.enc.NumOffset, a64A)
		c.a.FMovFromF(a64A, a64F2)
		c.a.Store(a64C, abi.OffGenSent+c.enc.RefOffset, a64A)
	} else {
		c.movImm(a64A, c.enc.Undefined)
		c.a.Store(a64C, abi.OffGenSent+c.enc.NumOffset, a64A)
		c.a.Store(a64C, abi.OffGenSent+c.enc.RefOffset, arm64.ZR)
	}
	c.a.Load(a64D, a64B, c.enc.GenClosure)
	for _, t := range site.Gens {
		next := c.a.NewLabel()
		c.a.Load(a64C, a64D, c.enc.ClosureFn)
		c.movImm(a64A, uint64(t.Fn))
		c.a.Cmp(a64C, a64A, true)
		c.a.BCond(arm64.NE, next)
		// Its environment, its template's, and its realm.
		for _, f := range [...]struct {
			off  int32
			code bool
			want uintptr
		}{{c.enc.CodeEnv, true, t.Env}, {c.enc.ClosureRealm, false, t.Realm}} {
			from := a64D
			if f.code {
				c.a.Load(a64C, a64D, c.enc.ClosureCode)
				from = a64C
			}
			c.a.Load(a64C, from, f.off)
			c.movImm(a64A, uint64(f.want))
			c.a.Cmp(a64C, a64A, true)
			fail.jump(arm64.NE)
		}
		c.genNextCall(v, t, slow, done)
		c.a.Bind(next)
	}
	c.a.B(slow)
}

// genNextCall is amd64's, the generator's closure in D: the call of the
// generator's code at its generator entry, its state described, what is
// live saved; at its return, the call's result made -- the object Go made,
// or { value, done } from the pool, or by Go (ssa.GoIterResult) while the
// collector marks or the pool is empty -- or, at its miss, Go's call.
func (c *a64Compiler) genNextCall(v *ssa.Value, t ssa.GenStep, slow, done arm64.Label) {
	site := v.Calls()[0]
	vs := int32(c.enc.ValueSize)
	c.movImm(a64C, uint64(t.Entry))
	c.a.Load(a64C, a64C, 0)
	c.a.Cbz(a64C, slow, true)
	c.a.Store(a64Ctx, abi.OffGenEntry, a64C)
	c.a.Store(a64Ctx, abi.OffCallClosure, a64D)
	st, recs := c.callState(v, v.State, func(int) bool { return false }, -1)
	c.callRecords(v, recs)
	c.movImm(a64A, uint64(uintptr(unsafe.Pointer(st))))
	c.a.Store(a64Ctx, abi.OffCallState, a64A)
	saves := c.savesOf(v)
	for _, sv := range saves {
		if sv.float {
			c.spillStoreF(sv.slot, arm64.FReg(sv.reg))
		} else {
			c.spillStore(sv.slot, arm64.Reg(sv.reg))
		}
	}
	restore := func() {
		for _, sv := range saves {
			if sv.float {
				c.spillLoadF(arm64.FReg(sv.reg), sv.slot)
			} else {
				c.spillLoad(arm64.Reg(sv.reg), sv.slot)
			}
		}
	}
	back, miss := c.a.NewLabel(), c.a.NewLabel()
	c.resumeUsed = true
	c.a.Adr(a64CallRet, back)
	c.a.Load(a64CallClosure, a64Ctx, abi.OffCallClosure)
	c.movImm(a64CallLink, uint64(t.Entry)-uint64(c.enc.LinkGen))
	c.a.Adr(arm64.Reg(10), miss)
	c.a.Load(a64D, a64Ctx, abi.OffGenEntry)
	c.a.Br(a64D)
	// The generator returned, or Go finished it: what it gave to the
	// call's keep cell first.
	c.a.Bind(back)
	at := abi.OffKeep + int32(v.Index)*vs
	marked, stored := c.a.NewLabel(), c.a.NewLabel()
	c.movImm(a64B, c.enc.WriteBarrier)
	c.a.LoadU8(a64B, a64B, 0)
	c.a.Cbnz(a64B, marked, false)
	c.a.Store(a64Ctx, at+c.enc.RefOffset, a64RetRef)
	c.a.Store(a64Ctx, at+c.enc.NumOffset, a64RetNum)
	c.a.Bind(stored)
	restore()
	// The object Go made, the context's generator 1; or done, 0, or not.
	c.a.Load(a64A, a64Ctx, abi.OffNext)
	c.a.Load(a64A, a64A, abi.OffGenObject)
	c.a.CmpImm(a64A, 1, true)
	c.a.BCond(arm64.EQ, done)
	word := c.a.NewLabel()
	c.movImm(a64C, c.enc.False)
	c.a.Cbnz(a64A, word, true)
	c.movImm(a64C, c.enc.True)
	c.a.Bind(word)
	c.a.FMovToF(a64F1, a64C)
	// As amd64's: the pool's next object, with Go's write barrier while
	// the collector marks (marking); Go's, the pool run out (goMake).
	goMake, marking := c.a.NewLabel(), c.a.NewLabel()
	c.movImm(a64A, uint64(site.Pool))
	c.a.Load(a64B, a64A, abi.OffPoolCount)
	c.a.Cbz(a64B, goMake, true)
	c.movImm(a64C, c.enc.WriteBarrier)
	c.a.LoadU8(a64C, a64C, 0)
	c.a.Cbnz(a64C, marking, false)
	c.a.AddImm(a64B, a64B, -1, true)
	c.a.Store(a64A, abi.OffPoolCount, a64B)
	c.a.ShiftImm(arm64.Lsl, a64B, a64B, 3, true)
	c.a.Op(arm64.Add, a64B, a64B, a64A, true)
	c.a.Load(a64C, a64B, abi.OffPoolObjects)
	c.a.Store(a64B, abi.OffPoolObjects, arm64.ZR)
	c.a.Load(a64A, a64C, c.enc.ObjectProps)
	c.a.Load(a64B, a64Ctx, at+c.enc.NumOffset)
	c.a.Store(a64A, c.enc.NumOffset, a64B)
	c.a.Load(a64B, a64Ctx, at+c.enc.RefOffset)
	c.a.Store(a64A, c.enc.RefOffset, a64B)
	c.a.FMovFromF(a64B, a64F1)
	c.a.Store(a64A, vs+c.enc.NumOffset, a64B)
	c.a.Store(a64A, vs+c.enc.RefOffset, arm64.ZR)
	c.a.Store(a64Ctx, at+c.enc.RefOffset, a64C)
	c.movImm(a64A, c.enc.Object)
	c.a.Store(a64Ctx, at+c.enc.NumOffset, a64A)
	c.a.B(done)
	c.cold = append(c.cold, func() {
		c.a.Bind(marked)
		c.a.Store(a64Ctx, at+c.enc.NumOffset, a64RetNum)
		c.a.AddImm(a64A, a64Ctx, int64(at+c.enc.RefOffset), true)
		c.refStore()
		c.a.B(stored)
		// As amd64's: while the collector marks, the pool's next object
		// given the value while the pool holds it, its pointer word with
		// the barrier, then taken (poolTakeMarking).
		c.a.Bind(marking)
		c.a.ShiftImm(arm64.Lsl, a64B, a64B, 3, true)
		c.a.Op(arm64.Add, a64B, a64B, a64A, true)
		c.a.Load(a64C, a64B, abi.OffPoolObjects-8)
		c.a.Load(a64C, a64C, c.enc.ObjectProps)
		c.a.Load(a64A, a64Ctx, at+c.enc.NumOffset)
		c.a.Store(a64C, c.enc.NumOffset, a64A)
		c.a.FMovFromF(a64A, a64F1)
		c.a.Store(a64C, vs+c.enc.NumOffset, a64A)
		c.a.Store(a64C, vs+c.enc.RefOffset, arm64.ZR)
		c.a.AddImm(a64A, a64C, int64(c.enc.RefOffset), true)
		c.a.Load(a64C, a64Ctx, at+c.enc.RefOffset)
		c.refStore()
		c.poolTakeMarking(v, site)
		c.a.B(done)
		// Go makes the object, of the value the keep cell holds, and
		// fills the pool again if it ran out.
		c.a.Bind(goMake)
		c.a.Load(a64A, a64Ctx, at+c.enc.NumOffset)
		c.a.Store(a64Ctx, abi.OffGoArgs+c.enc.NumOffset, a64A)
		c.a.Load(a64A, a64Ctx, at+c.enc.RefOffset)
		c.a.Store(a64Ctx, abi.OffGoArgs+c.enc.RefOffset, a64A)
		c.a.FMovFromF(a64A, a64F1)
		c.a.Store(a64Ctx, abi.OffGoArgs+vs+c.enc.NumOffset, a64A)
		c.movImm(a64A, uint64(site.Pool))
		c.a.Store(a64Ctx, abi.OffGoArgs+2*vs+c.enc.RefOffset, a64A)
		c.callGo(v, uint64(ssa.GoIterResult), v.Index)
		c.a.B(done)
	})
	// Its code could not resume it: Go makes the call, from the registers
	// as they were.
	c.a.Bind(miss)
	restore()
	c.a.B(slow)
}

// iterStep is amd64's: a loop's step over an array, made in place. It
// uses A, B, C and D, F0 and F1, and no other.
// stepResult is a loop's step's answer read natively, as amd64's. It uses
// A, B, C and D.
func (c *a64Compiler) stepResult(v *ssa.Value, slow, join arm64.Label) {
	site := v.Calls()[0]
	cur, res := v.Args[0], v.Args[1]
	vs := int32(c.enc.ValueSize)
	keep := abi.OffKeep + int32(v.Index)*vs
	w := c.gpr(res, a64A)
	c.movImm(a64C, c.enc.Object)
	c.a.Cmp(w, a64C, true)
	c.a.BCond(arm64.NE, slow)
	c.pointerWord(res, w)
	c.a.Load(a64C, a64B, c.enc.ObjectShape)
	c.movImm(a64D, uint64(site.ResultShape))
	c.a.Cmp(a64C, a64D, true)
	c.a.BCond(arm64.NE, slow)
	c.a.Load(a64D, a64B, c.enc.ObjectProps)
	c.a.Load(a64C, a64D, int32(site.DoneAt)*vs+c.enc.NumOffset)
	value := c.a.NewLabel()
	c.movImm(a64B, c.enc.False)
	c.a.Cmp(a64C, a64B, true)
	c.a.BCond(arm64.EQ, value)
	c.movImm(a64B, c.enc.True)
	c.a.Cmp(a64C, a64B, true)
	c.a.BCond(arm64.NE, slow)
	// Done: no value, the cursor's state done.
	c.pointerWord(cur, c.gpr(cur, a64A))
	c.movImm(a64C, 1)
	c.a.Store8(a64B, c.enc.IterState+c.enc.IterDone, a64C)
	c.movImm(a64A, c.enc.Uninitialized)
	c.a.Store(a64Ctx, keep+c.enc.NumOffset, a64A)
	c.a.Store(a64Ctx, keep+c.enc.RefOffset, arm64.ZR)
	c.a.B(join)
	c.a.Bind(value)
	c.a.Load(a64A, a64D, int32(site.ValueAt)*vs+c.enc.NumOffset)
	c.a.Load(a64C, a64D, int32(site.ValueAt)*vs+c.enc.RefOffset)
	c.a.Store(a64Ctx, keep+c.enc.NumOffset, a64A)
	marked := c.a.NewLabel()
	c.movImm(a64B, c.enc.WriteBarrier)
	c.a.LoadU8(a64B, a64B, 0)
	c.a.Cbnz(a64B, marked, false)
	c.a.Store(a64Ctx, keep+c.enc.RefOffset, a64C)
	c.a.B(join)
	c.cold = append(c.cold, func() {
		c.a.Bind(marked)
		c.a.AddImm(a64A, a64Ctx, int64(keep+c.enc.RefOffset), true)
		c.refStore()
		c.a.B(join)
	})
}

func (c *a64Compiler) iterStep(v *ssa.Value, slow arm64.Label) {
	x := v.Args[0]
	c.pointerWord(x, c.gpr(x, a64A))
	c.a.Cbz(a64B, slow, true)
	c.objectPart(a64D, a64B, a64C, c.enc.ClassCursor, c.enc.IterState, a64GuardLabel(c, slow))
	c.a.LoadU8(a64C, a64D, c.enc.IterDone)
	c.a.Cbnz(a64C, slow, false)
	c.a.Load(a64A, a64D, c.enc.IterIdx)
	c.a.Load(a64C, a64D, c.enc.IterArr)
	// No array: a for...in's, which forInStep steps, a generator, which
	// genStep resumes natively, or a Map's or a Set's iterator, which
	// mapStep steps.
	nexts, gens, forIn := stepNexts(v, c.enc), stepGens(v, c.enc), c.enc.ForInState != 0
	others := slow
	if nexts != ([2]uintptr{}) || gens || forIn {
		others = c.a.NewLabel()
		done := c.a.NewLabel()
		defer c.a.Bind(done)
		c.cold = append(c.cold, func() {
			c.a.Bind(others)
			if forIn {
				notForIn := slow
				if nexts != ([2]uintptr{}) || gens {
					notForIn = c.a.NewLabel()
				}
				c.forInStep(v, notForIn, slow, done)
				if notForIn == slow {
					return
				}
				c.a.Bind(notForIn)
			}
			if gens {
				notGen := slow
				if nexts != ([2]uintptr{}) {
					notGen = c.a.NewLabel()
				}
				c.genStep(v, notGen, slow, done)
				if nexts != ([2]uintptr{}) {
					c.a.Bind(notGen)
				}
			}
			if nexts != ([2]uintptr{}) {
				c.mapStep(v, nexts, slow)
				c.a.B(done)
			}
		})
	}
	c.a.Cbz(a64C, others, true)
	c.a.Load(a64B, a64C, c.enc.ObjectElems+8)
	c.a.Cmp(a64A, a64B, true)
	c.a.BCond(arm64.HS, slow)
	c.a.Load(a64B, a64C, c.enc.ObjectElems)
	c.a.AddLsl(a64A, a64B, a64A, 4)
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

// forInStep is amd64's: a for...in's step over the keys of its object's
// layout or over an array's indices (forInIndex), the cursor's state in D,
// or on to notForIn for another cursor. It uses A, B, C and D, and F0.
func (c *a64Compiler) forInStep(v *ssa.Value, notForIn, slow, done arm64.Label) {
	fail := a64GuardLabel(c, slow)
	in := c.enc.ForInState
	c.a.LoadU8(a64C, a64D, c.enc.IterForIn)
	c.a.Cbz(a64C, notForIn, false)
	// An array's indices, walked first.
	c.a.Load(a64A, a64D, c.enc.IterIdx)
	c.a.Load(a64C, a64D, in+c.enc.ForInIndexKeys)
	c.a.Cmp(a64A, a64C, true)
	indices := slow
	if forInIndices(c.f) {
		indices = c.a.NewLabel()
		defer func() {
			c.a.Bind(indices)
			c.forInIndex(v, slow, done)
		}()
	}
	c.a.BCond(arm64.LT, indices)
	// Past the last key, done, whatever the object.
	keep := abi.OffKeep + int32(v.Index)*int32(c.enc.ValueSize)
	end := c.a.NewLabel()
	c.a.Load(a64C, a64D, in+c.enc.ForInKeys+8)
	c.a.Cmp(a64A, a64C, true)
	c.a.BCond(arm64.GE, end)
	// The object of the keys' layout still: nothing deleted from it.
	c.a.Load(a64C, a64D, in+c.enc.ForInShape)
	c.a.Cbz(a64C, slow, true)
	c.a.Load(a64B, a64D, in+c.enc.ForInObj)
	c.a.Load(a64B, a64B, c.enc.ObjectShape)
	c.a.Cmp(a64B, a64C, true)
	fail.jump(arm64.NE)
	c.a.Load(a64C, a64D, in+c.enc.ForInKeys)
	c.a.AddLsl(a64A, a64C, a64A, 4)
	c.a.Load(a64B, a64A, c.enc.NumOffset)
	c.a.Store(a64Ctx, keep+c.enc.NumOffset, a64B)
	c.a.Load(a64C, a64A, c.enc.RefOffset)
	c.a.Store(a64Ctx, keep+c.enc.RefOffset, a64C)
	c.a.Load(a64C, a64D, c.enc.IterIdx)
	c.a.AddImm(a64C, a64C, 1, true)
	c.a.Store(a64D, c.enc.IterIdx, a64C)
	c.a.B(done)
	// Done: the cursor marked so, the answer the uninitialized value.
	c.a.Bind(end)
	c.movImm(a64A, 1)
	c.a.Store8(a64D, c.enc.IterDone, a64A)
	c.movImm(a64A, c.enc.Uninitialized)
	c.a.Store(a64Ctx, keep+c.enc.NumOffset, a64A)
	c.movImm(a64A, 0)
	c.a.Store(a64Ctx, keep+c.enc.RefOffset, a64A)
	c.a.B(done)
}

// forInStart is amd64's: a for...in's cursor of its operand taken from the
// runtime's last, done, walking the keys of its object's layout, or slow.
// It uses A, B, C and D, and F0.
func (c *a64Compiler) forInStart(v *ssa.Value, slow, join arm64.Label) {
	x := v.Args[0]
	in := c.enc.IterState + c.enc.ForInState
	keep := abi.OffKeep + int32(v.Index)*int32(c.enc.ValueSize)
	w := c.gpr(x, a64A)
	c.movImm(a64C, c.enc.Object)
	c.a.Cmp(w, a64C, true)
	c.a.BCond(arm64.NE, slow)
	c.pointerWord(x, w)
	c.a.Cbz(a64B, slow, true)
	c.a.LoadU8(a64C, a64B, c.enc.ObjectClass)
	c.a.CmpImm(a64C, int64(c.enc.ClassObject), false)
	c.a.BCond(arm64.NE, slow)
	c.forInPlain(a64B, slow)
	// As amd64's: the keys of its layout, a shared one's.
	c.a.Load(a64C, a64B, c.enc.ObjectShape)
	c.a.Cbz(a64C, slow, true)
	c.a.LoadU8(a64A, a64C, c.enc.ShapeUnique)
	c.a.Cbnz(a64A, slow, false)
	c.a.Load(a64A, a64C, c.enc.ShapeSide)
	c.a.Cbz(a64A, slow, true)
	c.a.Load(a64D, a64A, c.enc.SideForIn)
	c.a.Cbz(a64D, slow, true)
	c.a.FMovToF(a64F0, a64B)
	// As amd64's: its prototypes, none or a chain the realm found no
	// enumerable key on, each with no elements.
	protos, walk := c.a.NewLabel(), c.a.NewLabel()
	c.a.Load(a64A, a64C, c.enc.ShapeProto)
	c.a.Cbz(a64A, protos, true)
	c.a.Load(a64B, a64Ctx, abi.OffHost)
	c.a.Load(a64B, a64B, c.enc.HostRealm)
	for i := range int32(2) {
		next := c.a.NewLabel()
		at := c.enc.ForInChains + i*c.enc.ChainSize
		c.a.Load(a64C, a64B, at+c.enc.ChainProto)
		c.a.Cmp(a64A, a64C, true)
		c.a.BCond(arm64.NE, next)
		c.a.Load(a64C, a64B, at+c.enc.ChainCell)
		c.a.LoadU8(a64C, a64C, c.enc.CellValid)
		c.a.Cbnz(a64C, walk, false)
		c.a.Bind(next)
	}
	c.a.B(slow)
	c.a.Bind(walk)
	c.forInPlain(a64A, slow)
	c.a.Load(a64C, a64A, c.enc.ObjectShape)
	c.a.Cbz(a64C, protos, true)
	c.a.Load(a64A, a64C, c.enc.ShapeProto)
	c.a.Cbnz(a64A, walk, true)
	c.a.Bind(protos)
	// As amd64's: the runtime's last cursor, done, used again.
	c.a.Load(a64A, a64Ctx, abi.OffHost)
	c.a.Load(a64A, a64A, c.enc.HostForInLast)
	c.a.Cbz(a64A, slow, true)
	c.a.LoadU8(a64C, a64A, c.enc.IterState+c.enc.IterDone)
	c.a.Cbz(a64C, slow, false)
	c.a.Store8(a64A, c.enc.IterState+c.enc.IterDone, arm64.ZR)
	c.a.Store(a64A, c.enc.IterState+c.enc.IterIdx, arm64.ZR)
	c.a.Store(a64A, in+c.enc.ForInIndexKeys, arm64.ZR)
	for _, f := range [][2]int32{{c.enc.EnumKeys, c.enc.ForInKeys}, {c.enc.EnumAtoms, c.enc.ForInAtoms}} {
		for _, word := range []int32{8, 16} {
			c.a.Load(a64C, a64D, f[0]+word)
			c.a.Store(a64A, in+f[1]+word, a64C)
		}
	}
	c.movImm(a64C, c.enc.Object)
	c.a.Store(a64Ctx, keep+c.enc.NumOffset, a64C)
	marked := c.a.NewLabel()
	c.movImm(a64C, c.enc.WriteBarrier)
	c.a.LoadU8(a64C, a64C, 0)
	c.a.Cbnz(a64C, marked, false)
	c.a.Load(a64C, a64D, c.enc.EnumKeys)
	c.a.Store(a64A, in+c.enc.ForInKeys, a64C)
	c.a.Load(a64C, a64D, c.enc.EnumAtoms)
	c.a.Store(a64A, in+c.enc.ForInAtoms, a64C)
	c.a.FMovFromF(a64B, a64F0)
	c.a.Store(a64A, in+c.enc.ForInObj, a64B)
	c.a.Load(a64C, a64B, c.enc.ObjectShape)
	c.a.Store(a64A, in+c.enc.ForInShape, a64C)
	c.a.Store(a64Ctx, keep+c.enc.RefOffset, a64A)
	c.a.B(join)
	c.cold = append(c.cold, func() {
		// As amd64's: each pointer word through storeRef, what it stores
		// found again.
		c.a.Bind(marked)
		cursor := func(field int32) {
			c.a.Load(a64A, a64Ctx, abi.OffHost)
			c.a.Load(a64A, a64A, c.enc.HostForInLast)
			c.a.AddImm(a64A, a64A, int64(in+field), true)
		}
		object := func() {
			c.pointerWord(x, c.gpr(x, a64A))
		}
		object()
		c.a.MovRR(a64C, a64B)
		cursor(c.enc.ForInObj)
		c.refStore()
		object()
		c.a.Load(a64C, a64B, c.enc.ObjectShape)
		cursor(c.enc.ForInShape)
		c.refStore()
		for _, f := range [][2]int32{{c.enc.EnumKeys, c.enc.ForInKeys}, {c.enc.EnumAtoms, c.enc.ForInAtoms}} {
			object()
			c.a.Load(a64C, a64B, c.enc.ObjectShape)
			c.a.Load(a64C, a64C, c.enc.ShapeSide)
			c.a.Load(a64C, a64C, c.enc.SideForIn)
			c.a.Load(a64C, a64C, f[0])
			cursor(f[1])
			c.refStore()
		}
		c.a.Load(a64C, a64Ctx, abi.OffHost)
		c.a.Load(a64C, a64C, c.enc.HostForInLast)
		c.a.AddImm(a64A, a64Ctx, int64(keep+c.enc.RefOffset), true)
		c.refStore()
		c.a.B(join)
	})
}

// forInPlain is amd64's: slow unless the object at o has no elements and
// its prototype is its layout's. It uses C.
func (c *a64Compiler) forInPlain(o arm64.Reg, slow arm64.Label) {
	none := c.a.NewLabel()
	c.a.LoadU8(a64C, o, c.enc.ObjectFlags+1)
	c.a.TstImm(a64C, uint64(c.enc.FlagPoly), false)
	c.a.BCond(arm64.NE, slow)
	c.a.LoadU8(a64C, o, c.enc.ObjectFlags)
	c.a.TstImm(a64C, uint64(c.enc.FlagExt), false)
	c.a.BCond(arm64.EQ, none)
	c.a.Load(a64C, o, c.enc.ObjectExt+c.enc.ExtElems+8)
	c.a.Cbnz(a64C, slow, true)
	c.a.Bind(none)
}

// forInIndex is amd64's: forInStep's step over an array's indices, the
// cursor's state in D and the walk's index in A, its key the VM's string
// of it, recorded in the VM's table of key strings, the entry's pointer
// through the write barrier while the collector marks. It uses A, B, C and
// D, and F0.
func (c *a64Compiler) forInIndex(v *ssa.Value, slow, done arm64.Label) {
	fail := a64GuardLabel(c, slow)
	keep := abi.OffKeep + int32(v.Index)*int32(c.enc.ValueSize)
	// The element, an array's dense one, no hole.
	c.a.Load(a64C, a64D, c.enc.ForInState+c.enc.ForInObj)
	c.a.Cbz(a64C, slow, true)
	c.a.LoadU8(a64B, a64C, c.enc.ObjectClass)
	c.a.CmpImm(a64B, int64(c.enc.ClassArray), false)
	fail.jump(arm64.NE)
	c.a.Load(a64B, a64C, c.enc.ObjectElems+8)
	c.a.Cmp(a64A, a64B, true)
	fail.jump(arm64.HS)
	c.a.Load(a64C, a64C, c.enc.ObjectElems)
	c.a.ShiftImm(arm64.Lsl, a64B, a64A, 4, true)
	c.a.Op(arm64.Add, a64C, a64C, a64B, true)
	c.a.Load(a64C, a64C, c.enc.NumOffset)
	c.movImm(a64B, c.enc.Uninitialized)
	c.a.Cmp(a64C, a64B, true)
	fail.jump(arm64.EQ)
	// Its string, made already.
	c.a.CmpImm(a64A, abi.IntStrings, true)
	fail.jump(arm64.HS)
	c.movImm(a64C, uint64(c.f.IntStrings))
	c.a.ShiftImm(arm64.Lsl, a64B, a64A, 3, true)
	c.a.Op(arm64.Add, a64C, a64C, a64B, true)
	c.a.Load(a64C, a64C, 0)
	c.a.Cbz(a64C, slow, true)
	// The key, and the walk advanced.
	c.a.Store(a64Ctx, keep+c.enc.RefOffset, a64C)
	c.movImm(a64B, c.enc.String)
	c.a.Store(a64Ctx, keep+c.enc.NumOffset, a64B)
	c.a.AddImm(a64B, a64A, 1, true)
	c.a.Store(a64D, c.enc.IterIdx, a64B)
	// The string's entry in the table of key strings, in D, and its atom,
	// the index's, in B.
	c.a.FMovToF(a64F0, a64A)
	c.keyAtomSet(a64D, a64C, a64B)
	c.a.FMovFromF(a64A, a64F0)
	c.movImm(a64B, abi.AtomIndexTag)
	c.a.Op(arm64.Orr, a64B, a64A, a64B, false)
	// The string's entry, if its set has one: its atom and none written
	// where they are not the index's.
	write, fix := c.a.NewLabel(), c.a.NewLabel()
	c.keyAtomFind(a64D, a64C, a64A, write)
	c.a.LoadU32(a64A, a64D, abi.KeyAtomAtom)
	c.a.Cmp(a64A, a64B, false)
	c.a.BCond(arm64.NE, fix)
	c.a.LoadU8(a64A, a64D, abi.KeyAtomNone)
	c.a.Cbz(a64A, done, false)
	c.a.Bind(fix)
	c.a.Store32(a64D, abi.KeyAtomAtom, a64B)
	c.movImm(a64A, 0)
	c.a.Store8(a64D, abi.KeyAtomNone, a64A)
	c.a.B(done)
	// Else the way the set's next says, which moves on.
	c.a.Bind(write)
	c.a.LoadU8(a64A, a64D, abi.KeyAtomNext)
	c.a.AddImm(a64A, a64A, 1, false)
	c.a.AndImm(a64A, a64A, 1<<abi.KeyAtomsWaysLog-1, false)
	c.a.Store8(a64D, abi.KeyAtomNext, a64A)
	c.a.AddImm(a64A, a64A, 1<<abi.KeyAtomsWaysLog-1, false)
	c.a.AndImm(a64A, a64A, 1<<abi.KeyAtomsWaysLog-1, false)
	c.a.AddLsl(a64D, a64D, a64A, keyAtomShift)
	c.a.Store32(a64D, abi.KeyAtomAtom, a64B)
	c.movImm(a64A, 0)
	c.a.Store8(a64D, abi.KeyAtomNone, a64A)
	c.a.AddImm(a64A, a64D, abi.KeyAtomString, true)
	c.storeByFlag()
	c.a.B(done)
}

// keyAtomSet is amd64's: the first entry of the set of the VM's table of
// key strings where the string whose pointer is in s is, in dst; tmp is
// its scratch, s kept.
func (c *a64Compiler) keyAtomSet(dst, s, tmp arm64.Reg) {
	c.a.ShiftImm(arm64.Lsr, dst, s, abi.KeyAtomsAddrShift, true)
	c.movImm(tmp, abi.KeyAtomsMul)
	c.a.Mul(dst, dst, tmp)
	c.a.ShiftImm(arm64.Lsr, dst, dst, 64-abi.KeyAtomsBits+abi.KeyAtomsWaysLog, true)
	c.a.ShiftImm(arm64.Lsl, dst, dst, keyAtomShift+abi.KeyAtomsWaysLog, true)
	c.movImm(tmp, uint64(c.f.KeyAtoms))
	c.a.Op(arm64.Add, dst, dst, tmp, true)
}

// keyAtomFind is amd64's: set moved to the entry of the string whose
// pointer is in s, or miss, set as it was, where the set has none; tmp is
// its scratch.
func (c *a64Compiler) keyAtomFind(set, s, tmp arm64.Reg, miss arm64.Label) {
	const ways = 1 << abi.KeyAtomsWaysLog
	var hit [ways]arm64.Label
	found := c.a.NewLabel()
	for w := range ways {
		hit[w] = c.a.NewLabel()
		c.a.Load(tmp, set, int32(w)*abi.KeyAtomSize+abi.KeyAtomString)
		c.a.Cmp(tmp, s, true)
		c.a.BCond(arm64.EQ, hit[w])
	}
	c.a.B(miss)
	for w := range ways {
		c.a.Bind(hit[w])
		if w > 0 {
			c.a.AddImm(set, set, int64(w)*abi.KeyAtomSize, true)
		}
		if w < ways-1 {
			c.a.B(found)
		}
	}
	c.a.Bind(found)
}

// keyIs is amd64's: whether its value is a string the VM's table of key
// strings names atom Const.Bits by, or any atom but an index's. It uses
// A, B and C.
func (c *a64Compiler) keyIs(v *ssa.Value, to *a64Target) {
	t := c.tests(to, false)
	key := v.Args[0]
	if c.f.KeyAtoms == 0 || c.enc.ValueSize != 16 {
		c.jumpTo(t, t.no)
		c.setBool(v, to, t, 1)
		return
	}
	miss := t.no
	fill := v.Index&ssa.KeyIsFill != 0 && c.enc.CallGo != 0
	if fill {
		miss = c.a.NewLabel()
	}
	if w := c.gpr(key, a64A); w != a64A {
		c.a.MovRR(a64A, w)
	}
	c.movImm(a64B, c.enc.String)
	c.a.Cmp(a64A, a64B, true)
	c.a.BCond(arm64.NE, t.no)
	c.pointerWord(key, a64A)
	c.a.Cbz(a64B, t.no, true)
	c.keyAtomSet(a64A, a64B, a64C)
	c.keyAtomFind(a64A, a64B, a64C, miss)
	c.a.LoadU8(a64C, a64A, abi.KeyAtomNone)
	c.a.Cbnz(a64C, miss, false)
	c.a.LoadU32(a64C, a64A, abi.KeyAtomAtom)
	if fill {
		budget := c.budget
		c.cold = append(c.cold, func() {
			saved := c.budget
			c.budget = budget
			c.a.Bind(miss)
			size := int32(unsafe.Sizeof(abi.GoArg{}))
			c.goArg(abi.OffGoArgs, key)
			c.movImm(a64A, uint64(uint32(v.Const.Bits)))
			c.a.Store(a64Ctx, abi.OffGoArgs+size+c.enc.NumOffset, a64A)
			c.goSaving(uint64(ssa.GoKeyFill)|uint64(v.Index&ssa.KeyIsAny)<<abi.GoOpIndexShift, nil)
			c.a.Load(a64A, a64Ctx, abi.OffGoArgs+c.enc.NumOffset)
			c.a.Cbnz(a64A, t.yes, true)
			c.a.B(t.no)
			c.budget = saved
		})
	}
	if v.Index&ssa.KeyIsAny != 0 {
		// Any atom but an index's, whose top bit is set.
		c.a.ShiftImm(arm64.Lsr, a64C, a64C, 31, false)
		c.a.CmpImm(a64C, 0, false)
		c.jumpIf(t, arm64.EQ)
	} else {
		c.movImm(a64B, uint64(uint32(v.Const.Bits)))
		c.a.Cmp(a64C, a64B, false)
		c.jumpIf(t, arm64.EQ)
	}
	c.setBool(v, to, t, 1)
}

// keyedRead is amd64's: an array's dense element, no hole, read natively
// by an index or by a key string the VM handed out for one, at a site
// whose reads Go makes for other receivers and keys. It uses A, B, C, D
// and F2, and no other.
func (c *a64Compiler) keyedRead(v *ssa.Value, slow arm64.Label) {
	fail := a64GuardLabel(c, slow)
	recv, key := v.Args[0], v.Args[1]
	named, have := c.a.NewLabel(), c.a.NewLabel()
	if w := c.gpr(key, a64A); w != a64A {
		c.a.MovRR(a64A, w)
	}
	c.movImm(a64B, abi.NumberLimit)
	c.a.Cmp(a64A, a64B, true)
	c.a.BCond(arm64.HS, named)
	c.a.FMovToF(a64F2, a64A)
	c.a.Fcvtzs(a64B, a64F2)
	c.a.Scvtf(a64F2, a64B, true)
	c.a.FMovFromF(a64C, a64F2)
	c.a.Cmp(a64C, a64A, true)
	fail.jump(arm64.NE)
	c.a.FMovToF(a64F2, a64B)
	c.a.B(have)
	c.a.Bind(named)
	c.movImm(a64B, c.enc.String)
	c.a.Cmp(a64A, a64B, true)
	fail.jump(arm64.NE)
	c.pointerWord(key, a64A)
	c.a.Cbz(a64B, slow, true)
	c.keyAtomSet(a64A, a64B, a64C)
	c.keyAtomFind(a64A, a64B, a64C, slow)
	c.a.LoadU8(a64C, a64A, abi.KeyAtomNone)
	c.a.Cbnz(a64C, slow, false)
	c.a.LoadU32(a64B, a64A, abi.KeyAtomAtom)
	c.a.ShiftImm(arm64.Lsr, a64C, a64B, 31, false)
	c.a.Cbz(a64C, slow, false)
	c.movImm(a64C, abi.AtomIndexTag-1)
	c.a.Op(arm64.And, a64B, a64B, a64C, false)
	c.a.FMovToF(a64F2, a64B)
	c.a.Bind(have)
	w := c.gpr(recv, a64A)
	c.movImm(a64C, c.enc.Object)
	c.a.Cmp(w, a64C, true)
	fail.jump(arm64.NE)
	c.pointerWord(recv, w)
	c.a.Cbz(a64B, slow, true)
	c.a.LoadU8(a64C, a64B, c.enc.ObjectClass)
	c.a.CmpImm(a64C, int64(c.enc.ClassArray), false)
	fail.jump(arm64.NE)
	c.a.FMovFromF(a64A, a64F2)
	c.a.Load(a64C, a64B, c.enc.ObjectElems+8)
	c.a.Cmp(a64A, a64C, true)
	fail.jump(arm64.HS)
	c.a.Load(a64B, a64B, c.enc.ObjectElems)
	c.a.AddLsl(a64A, a64B, a64A, 4)
	c.a.Load(a64C, a64A, c.enc.NumOffset)
	c.movImm(a64D, c.enc.Uninitialized)
	c.a.Cmp(a64C, a64D, true)
	fail.jump(arm64.EQ)
	keep := abi.OffKeep + int32(v.Index)*int32(c.enc.ValueSize)
	c.a.Store(a64Ctx, keep+c.enc.NumOffset, a64C)
	c.a.Load(a64C, a64A, c.enc.RefOffset)
	c.a.Store(a64Ctx, keep+c.enc.RefOffset, a64C)
}

// argRead is amd64's: arguments.length or arguments[k] of the function's
// own arguments, read natively from its context where its call wrote
// them, the object not made; its pointer word stored through storeRef
// while the collector marks (byMarking). It goes on at join; anything else
// goes to slow. It uses A, B, C, D and F2.
func (c *a64Compiler) argRead(v *ssa.Value, site *ssa.CallSite, slow, join arm64.Label) {
	fail := a64GuardLabel(c, slow)
	keep := abi.OffKeep + int32(v.Index)*int32(c.enc.ValueSize)
	c.a.Load(a64A, a64Ctx, abi.OffLive)
	c.a.CmpImm(a64A, abi.LiveCall, true)
	fail.jump(arm64.NE)
	local := v.Args[0]
	if site.Argc != 1 {
		local = v.Args[1]
	}
	w := c.gpr(local, a64A)
	c.movImm(a64C, c.enc.Object)
	c.a.Cmp(w, a64C, true)
	fail.jump(arm64.EQ)
	if site.Argc == 1 {
		c.a.Load(a64A, a64Ctx, abi.OffArgc)
		c.a.CmpImm(a64A, abi.MaxGoArgs, true)
		fail.jump(arm64.HI)
		c.a.Scvtf(a64F2, a64A, true)
		c.a.FMovFromF(a64C, a64F2)
		c.a.Store(a64Ctx, keep+c.enc.NumOffset, a64C)
		c.a.Store(a64Ctx, keep+c.enc.RefOffset, arm64.ZR)
		c.a.B(join)
		return
	}
	if w := c.gpr(v.Args[0], a64A); w != a64A {
		c.a.MovRR(a64A, w)
	}
	c.movImm(a64B, abi.NumberLimit)
	c.a.Cmp(a64A, a64B, true)
	fail.jump(arm64.HS)
	c.a.FMovToF(a64F2, a64A)
	c.a.Fcvtzs(a64B, a64F2)
	c.a.Scvtf(a64F2, a64B, true)
	c.a.FMovFromF(a64C, a64F2)
	c.a.Cmp(a64C, a64A, true)
	fail.jump(arm64.NE)
	if n := site.Argc - 2; n > 0 {
		c.a.CmpImm(a64B, int64(n), true)
		fail.jump(arm64.LO)
	}
	c.a.Load(a64C, a64Ctx, abi.OffArgc)
	c.a.Cmp(a64B, a64C, true)
	fail.jump(arm64.HS)
	c.a.CmpImm(a64B, abi.MaxGoArgs, true)
	fail.jump(arm64.HS)
	num, ref := int32(unsafe.Offsetof(abi.GoArg{}.Num)), int32(unsafe.Offsetof(abi.GoArg{}.Ref))
	c.a.AddLsl(a64D, a64Ctx, a64B, 4)
	c.a.Load(a64C, a64D, abi.OffArgs+num)
	c.a.Store(a64Ctx, keep+c.enc.NumOffset, a64C)
	c.a.Load(a64C, a64D, abi.OffArgs+ref)
	c.byMarking(join, func(marking bool) {
		c.a.AddImm(a64A, a64Ctx, int64(keep+c.enc.RefOffset), true)
		c.ptrStore(marking)
	})
}

// frameMade is amd64's: a rest parameter's array or an unmapped arguments
// object taken from its pool, of the arguments a native call wrote to the
// context, their count its length; a count past the pool's room, or an
// empty pool, has Go fill it again and the take made again from its
// start; the elements' pointer words stored with Go's write barrier while
// the collector marks. It uses A, B, C, D and F2.
func (c *a64Compiler) frameMade(v *ssa.Value, site *ssa.CallSite, slow arm64.Label) {
	vs := int32(c.enc.ValueSize)
	from := int32(0)
	if site.Made == ssa.MadeRest {
		from = int32(site.MadeFrom)
	}
	marked, done, top, grow := c.a.NewLabel(), c.a.NewLabel(), c.a.NewLabel(), c.a.NewLabel()
	c.a.Bind(top)
	empty := c.poolRefill(v, site.Pool, top, slow)
	c.a.Load(a64A, a64Ctx, abi.OffLive)
	c.a.CmpImm(a64A, abi.LiveCall, true)
	c.a.BCond(arm64.NE, slow)
	count := func(r arm64.Reg) {
		c.a.Load(r, a64Ctx, abi.OffArgc)
		if from > 0 {
			zero, pos := c.a.NewLabel(), c.a.NewLabel()
			c.a.CmpImm(r, int64(from), true)
			c.a.BCond(arm64.LO, zero)
			c.a.AddImm(r, r, -int64(from), true)
			c.a.B(pos)
			c.a.Bind(zero)
			c.movImm(r, 0)
			c.a.Bind(pos)
		}
	}
	c.a.Load(a64B, a64Ctx, abi.OffArgc)
	c.a.CmpImm(a64B, abi.MaxGoArgs, true)
	c.a.BCond(arm64.HI, slow)
	count(a64B)
	c.movImm(a64A, uint64(site.Pool))
	c.a.Load(a64C, a64A, abi.OffPoolRoom)
	c.a.Cmp(a64B, a64C, true)
	c.a.BCond(arm64.HI, grow)
	c.a.Load(a64C, a64A, abi.OffPoolCount)
	c.a.Cbz(a64C, empty, true)
	c.movImm(a64C, c.enc.WriteBarrier)
	c.a.LoadU8(a64C, a64C, 0)
	c.a.Cbnz(a64C, marked, false)
	keep := abi.OffKeep + int32(v.Index)*vs
	num, ref := int32(unsafe.Offsetof(abi.GoArg{}.Num)), int32(unsafe.Offsetof(abi.GoArg{}.Ref))
	c.cold = append(c.cold, func() {
		c.a.Bind(grow)
		c.a.AddImm(a64B, a64B, 1, true)
		c.a.Store(a64A, abi.OffPoolWant, a64B)
		c.a.B(empty)
	})
	c.cold = append(c.cold, func() {
		c.a.Bind(marked)
		c.poolTakeMarking(v, site)
		for k := int32(0); from+k < abi.MaxGoArgs; k++ {
			count(a64B)
			c.a.CmpImm(a64B, int64(k), true)
			c.a.BCond(arm64.LS, done)
			c.a.Load(a64A, a64Ctx, keep+c.enc.RefOffset)
			c.a.Load(a64A, a64A, c.enc.ObjectElems)
			c.a.Load(a64C, a64Ctx, abi.OffArgs+(from+k)*16+num)
			c.a.Store(a64A, k*vs+c.enc.NumOffset, a64C)
			c.a.AddImm(a64A, a64A, int64(k*vs+c.enc.RefOffset), true)
			c.a.Load(a64C, a64Ctx, abi.OffArgs+(from+k)*16+ref)
			c.refStore()
		}
		c.a.B(done)
	})
	c.poolTake(v, site)
	// The elements: A walks the object's, B the context's, to D.
	loop, copied := c.a.NewLabel(), c.a.NewLabel()
	count(a64B)
	c.a.Load(a64A, a64Ctx, keep+c.enc.RefOffset)
	c.a.Load(a64A, a64A, c.enc.ObjectElems)
	c.a.AddImm(a64C, a64Ctx, int64(abi.OffArgs+from*16), true)
	c.a.AddLsl(a64D, a64C, a64B, 4)
	c.a.MovRR(a64B, a64C)
	c.a.Bind(loop)
	c.a.Cmp(a64B, a64D, true)
	c.a.BCond(arm64.HS, copied)
	c.a.Load(a64C, a64B, num)
	c.a.Store(a64A, c.enc.NumOffset, a64C)
	c.a.Load(a64C, a64B, ref)
	c.a.Store(a64A, c.enc.RefOffset, a64C)
	c.a.AddImm(a64A, a64A, int64(vs), true)
	c.a.AddImm(a64B, a64B, 16, true)
	c.a.B(loop)
	c.a.Bind(copied)
	c.a.Bind(done)
	count(a64B)
	c.a.Load(a64C, a64Ctx, keep+c.enc.RefOffset)
	c.a.Store(a64C, c.enc.ObjectElems+8, a64B)
	if site.Made == ssa.MadeArguments {
		c.a.Scvtf(a64F2, a64B, true)
		c.a.Load(a64A, a64C, c.enc.ObjectProps)
		c.a.FMovFromF(a64B, a64F2)
		c.a.Store(a64A, c.enc.NumOffset, a64B)
	}
}

// keyedStore is amd64's: an array's dense element stored natively at a
// site whose stores Go makes for other receivers. It uses A, B, C, D, F0,
// F1 and F2, and no other.
func (c *a64Compiler) keyedStore(v *ssa.Value, slow arm64.Label) {
	fail := a64GuardLabel(c, slow)
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
	fail.jump(arm64.HS)
	c.a.FMovToF(a64F2, a64A)
	c.a.Fcvtzs(a64B, a64F2)
	c.a.Scvtf(a64F2, a64B, true)
	c.a.FMovFromF(a64C, a64F2)
	c.a.Cmp(a64C, a64A, true)
	fail.jump(arm64.NE)
	c.a.FMovToF(a64F2, a64B)
	w = c.gpr(recv, a64A)
	c.movImm(a64C, c.enc.Object)
	c.a.Cmp(w, a64C, true)
	fail.jump(arm64.NE)
	c.pointerWord(recv, w)
	c.a.Cbz(a64B, slow, true)
	c.a.LoadU8(a64C, a64B, c.enc.ObjectClass)
	c.a.CmpImm(a64C, int64(c.enc.ClassArray), false)
	fail.jump(arm64.NE)
	// Elements an array literal's arrays share are Go's to copy first.
	c.a.LoadU8(a64C, a64B, c.enc.ObjectFlags)
	c.movImm(a64D, uint64(c.enc.FlagShared))
	c.a.Tst(a64C, a64D, false)
	fail.jump(arm64.NE)
	c.movImm(a64C, c.enc.WriteBarrier)
	c.a.LoadU8(a64C, a64C, 0)
	c.a.Cbnz(a64C, slow, false)
	c.a.FMovFromF(a64A, a64F2)
	c.a.Load(a64C, a64B, c.enc.ObjectElems+8)
	c.a.Cmp(a64A, a64C, true)
	fail.jump(arm64.HS)
	c.a.Load(a64B, a64B, c.enc.ObjectElems)
	c.a.AddLsl(a64A, a64B, a64A, 4)
	c.a.Load(a64C, a64A, c.enc.NumOffset)
	c.movImm(a64D, c.enc.Uninitialized)
	c.a.Cmp(a64C, a64D, true)
	fail.jump(arm64.EQ)
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

// keyedHas is amd64's: key in obj natively where obj is an array and key
// an index, true where it names one of its dense elements that is no hole,
// false where it names none of a dense array whose prototypes are as they
// were; and where key is a string the VM's cache answers for obj. It uses
// A, B, C, D, F1 and F2, and no other.
// byMarking is amd64's: made for the collector idle, and out of line for
// it marking, either going on at join. It uses a64A.
func (c *a64Compiler) byMarking(join arm64.Label, made func(marking bool)) {
	marked := c.a.NewLabel()
	c.movImm(a64A, c.enc.WriteBarrier)
	c.a.LoadU8(a64A, a64A, 0)
	c.a.Cbnz(a64A, marked, false)
	made(false)
	c.a.B(join)
	c.cold = append(c.cold, func() {
		c.a.Bind(marked)
		made(true)
		c.a.B(join)
	})
}

// ptrStore is amd64's: the pointer word in a64C to the address in a64A,
// as it is, or, marking, through storeRef (refStore) unless both it and
// the word it overwrites are nil. Marking, it uses a64A, a64B, a64C and
// a64D.
func (c *a64Compiler) ptrStore(marking bool) {
	if !marking {
		c.a.Store(a64A, 0, a64C)
		return
	}
	c.refStore()
}

// keepObject is amd64's: the object whose pointer word is at the
// context's from as call v's result, in its keep cell (ptrStore).
func (c *a64Compiler) keepObject(v *ssa.Value, from int32, marking bool) {
	keep := abi.OffKeep + int32(v.Index)*int32(c.enc.ValueSize)
	c.movImm(a64B, c.enc.Object)
	c.a.Store(a64Ctx, keep+c.enc.NumOffset, a64B)
	c.a.Load(a64C, a64Ctx, from)
	c.a.AddImm(a64A, a64Ctx, int64(keep+c.enc.RefOffset), true)
	c.ptrStore(marking)
}

// copyProps is amd64's: the object spread v by the VM's table of spreads
// by layout, or slow, having written nothing; while the collector marks,
// its pointer words through storeRef (ptrStore). The values left are
// counted in a64D, kept in the context's GoArgs across a store marking.
func (c *a64Compiler) copyProps(v *ssa.Value, site *ssa.CallSite, slow arm64.Label, marking bool) {
	size := int32(unsafe.Sizeof(abi.GoArg{}))
	tgt, src := abi.OffGoArgs, abi.OffGoArgs+size
	at := func(i int32) int32 { return abi.OffGoArgs + 2*size + i*8 }
	entry, left, next, dst, vals, length := at(0), at(1), at(2), at(3), at(4), at(5)
	c.movImm(a64C, c.enc.Object)
	for _, o := range []int32{tgt, src} {
		c.a.Load(a64A, a64Ctx, o+c.enc.NumOffset)
		c.a.Cmp(a64A, a64C, true)
		c.a.BCond(arm64.NE, slow)
		c.a.Load(a64B, a64Ctx, o+c.enc.RefOffset)
		c.a.Cbz(a64B, slow, true)
	}
	c.a.Load(a64A, a64B, c.enc.ObjectShape)
	c.a.Load(a64C, a64Ctx, tgt+c.enc.RefOffset)
	c.a.Load(a64C, a64C, c.enc.ObjectShape)
	c.a.ShiftImm(arm64.Lsr, a64B, a64A, 4, true)
	c.a.ShiftImm(arm64.Lsr, a64C, a64C, 6, true)
	c.a.Op(arm64.Eor, a64B, a64B, a64C, true)
	c.movImm(a64C, abi.CopyMul)
	c.a.Mul(a64B, a64B, a64C)
	c.a.ShiftImm(arm64.Lsr, a64B, a64B, 64-abi.CopyBits, true)
	c.movImm(a64C, abi.CopyEntrySize)
	c.a.Mul(a64B, a64B, a64C)
	c.movImm(a64C, uint64(site.Copies))
	c.a.Op(arm64.Add, a64B, a64B, a64C, true)
	c.a.Load(a64C, a64B, abi.CopySrc)
	c.a.Cmp(a64A, a64C, true)
	c.a.BCond(arm64.NE, slow)
	c.a.Load(a64A, a64Ctx, tgt+c.enc.RefOffset)
	c.a.Load(a64C, a64A, c.enc.ObjectShape)
	c.a.Load(a64D, a64B, abi.CopyFrom)
	c.a.Cmp(a64C, a64D, true)
	c.a.BCond(arm64.NE, slow)
	c.a.Store(a64Ctx, entry, a64B)
	// Room: the target's length and the values', its capacity or less.
	c.a.Load(a64D, a64B, abi.CopySlots+8)
	c.a.LoadU32(a64B, a64A, c.enc.ObjectPropsLen)
	c.a.Op(arm64.Add, a64C, a64D, a64B, true)
	c.a.LoadU32(a64A, a64A, c.enc.ObjectPropsCap)
	c.a.Cmp(a64C, a64A, true)
	c.a.BCond(arm64.HI, slow)
	c.a.Store(a64Ctx, length, a64C)
	c.a.Load(a64A, a64Ctx, tgt+c.enc.RefOffset)
	c.a.Load(a64C, a64A, c.enc.ObjectProps)
	c.a.AddLsl(a64B, a64C, a64B, 4)
	c.a.Store(a64Ctx, dst, a64B)
	c.a.Load(a64A, a64Ctx, src+c.enc.RefOffset)
	c.a.Load(a64A, a64A, c.enc.ObjectProps)
	c.a.Store(a64Ctx, vals, a64A)
	c.a.Load(a64B, a64Ctx, entry)
	c.a.Load(a64C, a64B, abi.CopySlots)
	c.a.Store(a64Ctx, next, a64C)
	loop, done := c.a.NewLabel(), c.a.NewLabel()
	c.a.Cbz(a64D, done, true)
	c.a.Bind(loop)
	c.a.Load(a64A, a64Ctx, next)
	c.a.LoadU32(a64B, a64A, 0)
	c.a.AddImm(a64A, a64A, 4, true)
	c.a.Store(a64Ctx, next, a64A)
	c.a.Load(a64A, a64Ctx, vals)
	c.a.AddLsl(a64B, a64A, a64B, 4)
	c.a.Load(a64A, a64B, c.enc.NumOffset)
	c.a.Load(a64C, a64B, c.enc.RefOffset)
	c.a.Load(a64B, a64Ctx, dst)
	c.a.Store(a64B, c.enc.NumOffset, a64A)
	c.a.AddImm(a64A, a64B, int64(c.enc.RefOffset), true)
	if marking {
		c.a.Store(a64Ctx, left, a64D)
	}
	c.ptrStore(marking)
	if marking {
		c.a.Load(a64D, a64Ctx, left)
	}
	c.a.Load(a64B, a64Ctx, dst)
	c.a.AddImm(a64B, a64B, 16, true)
	c.a.Store(a64Ctx, dst, a64B)
	c.a.AddImm(a64D, a64D, -1, true)
	c.a.Cbnz(a64D, loop, true)
	c.a.Bind(done)
	c.a.Load(a64A, a64Ctx, tgt+c.enc.RefOffset)
	c.a.Load(a64C, a64Ctx, length)
	c.a.Store32(a64A, c.enc.ObjectPropsLen, a64C)
	c.a.Load(a64B, a64Ctx, entry)
	c.a.Load(a64C, a64B, abi.CopyTo)
	c.a.AddImm(a64A, a64A, int64(c.enc.ObjectShape), true)
	c.ptrStore(marking)
	c.keepObject(v, tgt+c.enc.RefOffset, marking)
}

// defineNext is amd64's: a literal's field along its object's shape's
// first transition, or slow, having written nothing; while the collector
// marks, its pointer words through storeRef (ptrStore). The next shape is
// kept in the context's GoArgs across a store marking.
func (c *a64Compiler) defineNext(v *ssa.Value, site *ssa.CallSite, slow arm64.Label, marking bool) {
	size := int32(unsafe.Sizeof(abi.GoArg{}))
	obj, val, next := abi.OffGoArgs, abi.OffGoArgs+size, abi.OffGoArgs+2*size
	c.a.Load(a64A, a64Ctx, obj+c.enc.NumOffset)
	c.movImm(a64B, c.enc.Object)
	c.a.Cmp(a64A, a64B, true)
	c.a.BCond(arm64.NE, slow)
	c.a.Load(a64A, a64Ctx, obj+c.enc.RefOffset)
	c.a.Cbz(a64A, slow, true)
	c.a.LoadU8(a64B, a64A, c.enc.ObjectFlags)
	c.movImm(a64C, uint64(c.enc.FlagExtensible))
	c.a.Op(arm64.And, a64B, a64B, a64C, false)
	c.a.Cbz(a64B, slow, false)
	c.a.Load(a64B, a64A, c.enc.ObjectShape)
	c.a.Cbz(a64B, slow, true)
	c.a.Load(a64D, a64B, c.enc.ShapeNext)
	c.a.Cbz(a64D, slow, true)
	c.a.LoadU32(a64C, a64B, c.enc.ShapeNextKey)
	c.movImm(a64B, uint64(site.Key))
	c.a.Cmp(a64C, a64B, false)
	c.a.BCond(arm64.NE, slow)
	c.a.Load(a64B, a64A, c.enc.ObjectShape)
	c.a.LoadU8(a64C, a64B, c.enc.ShapeNextFlags)
	c.a.CmpImm(a64C, int64(c.enc.PropDefault), false)
	c.a.BCond(arm64.NE, slow)
	c.a.LoadU32(a64B, a64A, c.enc.ObjectPropsLen)
	c.a.LoadU32(a64C, a64A, c.enc.ObjectPropsCap)
	c.a.Cmp(a64B, a64C, true)
	c.a.BCond(arm64.HS, slow)
	indexed := c.a.NewLabel()
	c.a.CmpImm(a64B, int64(c.enc.LinearScan), true)
	c.a.BCond(arm64.LO, indexed)
	c.a.Load(a64C, a64D, c.enc.ShapeIndex)
	c.a.Cbz(a64C, slow, true)
	c.a.Bind(indexed)
	c.a.Store(a64Ctx, next, a64D)
	c.a.Load(a64C, a64A, c.enc.ObjectProps)
	c.a.ShiftImm(arm64.Lsl, a64B, a64B, 4, true)
	c.a.Op(arm64.Add, a64C, a64C, a64B, true)
	c.a.Load(a64B, a64Ctx, val+c.enc.NumOffset)
	c.a.Store(a64C, c.enc.NumOffset, a64B)
	c.a.AddImm(a64A, a64C, int64(c.enc.RefOffset), true)
	c.a.Load(a64C, a64Ctx, val+c.enc.RefOffset)
	c.ptrStore(marking)
	c.a.Load(a64A, a64Ctx, obj+c.enc.RefOffset)
	c.a.LoadU32(a64B, a64A, c.enc.ObjectPropsLen)
	c.a.AddImm(a64B, a64B, 1, true)
	c.a.Store32(a64A, c.enc.ObjectPropsLen, a64B)
	c.a.Load(a64C, a64Ctx, next)
	c.a.AddImm(a64A, a64A, int64(c.enc.ObjectShape), true)
	c.ptrStore(marking)
	c.keepObject(v, obj+c.enc.RefOffset, marking)
}

// arrayOperand is amd64's: a64B holds the pointer of the operand at the
// context's at, an array with no element kept as a property, or slow. It
// uses a64A and a64C.
func (c *a64Compiler) arrayOperand(at int32, slow arm64.Label) {
	c.a.Load(a64A, a64Ctx, at+c.enc.NumOffset)
	c.movImm(a64C, c.enc.Object)
	c.a.Cmp(a64A, a64C, true)
	c.a.BCond(arm64.NE, slow)
	c.a.Load(a64B, a64Ctx, at+c.enc.RefOffset)
	c.a.Cbz(a64B, slow, true)
	c.a.LoadU8(a64A, a64B, c.enc.ObjectClass)
	c.a.CmpImm(a64A, int64(c.enc.ClassArray), false)
	c.a.BCond(arm64.NE, slow)
	c.a.LoadU8(a64A, a64B, c.enc.ObjectFlags)
	c.movImm(a64C, uint64(c.enc.FlagSparse))
	c.a.Op(arm64.And, a64A, a64A, a64C, false)
	c.a.Cbnz(a64A, slow, false)
}

// spreadElems is amd64's: an array spread by its elements, or slow, the
// target's length as it was; while the collector marks, its pointer words
// through storeRef (ptrStore). The elements left are counted in a64D, kept
// in the context's GoArgs across a store marking.
func (c *a64Compiler) spreadElems(v *ssa.Value, site *ssa.CallSite, slow arm64.Label, marking bool) {
	size := int32(unsafe.Sizeof(abi.GoArg{}))
	tgt, src := abi.OffGoArgs, abi.OffGoArgs+size
	at := func(i int32) int32 { return abi.OffGoArgs + 2*size + i*8 }
	from, left, cur, dst, length := at(0), at(1), at(2), at(3), at(4)
	c.arrayOperand(src, slow)
	c.a.Store(a64Ctx, from, a64B)
	c.a.LoadU32(a64A, a64B, c.enc.ObjectPropsLen)
	c.a.Cbnz(a64A, slow, true)
	c.movImm(a64C, uint64(site.ArrayIter))
	c.loadProtoPlain(a64A, a64B)
	c.a.Load(a64D, a64C, abi.IterProto)
	c.a.Cmp(a64A, a64D, true)
	c.a.BCond(arm64.NE, slow)
	for _, cell := range []int32{abi.IterCellArray, abi.IterCellIter} {
		c.a.Load(a64A, a64C, cell)
		c.a.Cbz(a64A, slow, true)
		c.a.LoadU8(a64A, a64A, 0)
		c.a.Cbz(a64A, slow, false)
	}
	for _, m := range [][3]int32{{abi.IterValuesObj, abi.IterValuesAt, abi.IterValuesFn}, {abi.IterNextObj, abi.IterNextAt, abi.IterNextFn}} {
		c.a.Load(a64A, a64C, m[0])
		c.a.Load(a64A, a64A, c.enc.ObjectProps)
		c.a.LoadU32(a64B, a64C, m[1])
		c.a.ShiftImm(arm64.Lsl, a64B, a64B, 4, true)
		c.a.Op(arm64.Add, a64A, a64A, a64B, true)
		c.a.Load(a64A, a64A, c.enc.RefOffset)
		c.a.Load(a64D, a64C, m[2])
		c.a.Cmp(a64A, a64D, true)
		c.a.BCond(arm64.NE, slow)
	}
	c.arrayOperand(tgt, slow)
	c.a.Load(a64C, a64Ctx, from)
	c.a.Load(a64D, a64C, c.enc.ObjectElems+8)
	c.a.Load(a64C, a64C, c.enc.ObjectElems)
	c.a.Store(a64Ctx, cur, a64C)
	c.a.Load(a64C, a64B, c.enc.ObjectElems+8)
	c.a.Op(arm64.Add, a64A, a64D, a64C, true)
	c.a.Store(a64Ctx, length, a64A)
	c.a.Load(a64A, a64B, c.enc.ObjectElems+16)
	c.a.Load(a64B, a64Ctx, length)
	c.a.Cmp(a64B, a64A, true)
	c.a.BCond(arm64.HI, slow)
	c.a.Load(a64A, a64Ctx, tgt+c.enc.RefOffset)
	c.a.Load(a64A, a64A, c.enc.ObjectElems)
	c.a.AddLsl(a64C, a64A, a64C, 4)
	c.a.Store(a64Ctx, dst, a64C)
	loop, done := c.a.NewLabel(), c.a.NewLabel()
	c.a.Cbz(a64D, done, true)
	c.a.Bind(loop)
	c.a.Load(a64A, a64Ctx, cur)
	c.a.Load(a64B, a64A, c.enc.NumOffset)
	c.movImm(a64C, c.enc.Uninitialized)
	c.a.Cmp(a64B, a64C, true)
	c.a.BCond(arm64.EQ, slow)
	c.a.Load(a64C, a64A, c.enc.RefOffset)
	c.a.AddImm(a64A, a64A, 16, true)
	c.a.Store(a64Ctx, cur, a64A)
	c.a.Load(a64A, a64Ctx, dst)
	c.a.Store(a64A, c.enc.NumOffset, a64B)
	c.a.AddImm(a64A, a64A, int64(c.enc.RefOffset), true)
	if marking {
		c.a.Store(a64Ctx, left, a64D)
	}
	c.ptrStore(marking)
	if marking {
		c.a.Load(a64D, a64Ctx, left)
	}
	c.a.Load(a64A, a64Ctx, dst)
	c.a.AddImm(a64A, a64A, 16, true)
	c.a.Store(a64Ctx, dst, a64A)
	c.a.AddImm(a64D, a64D, -1, true)
	c.a.Cbnz(a64D, loop, true)
	c.a.Bind(done)
	c.a.Load(a64A, a64Ctx, tgt+c.enc.RefOffset)
	c.a.Load(a64C, a64Ctx, length)
	c.a.Store(a64A, c.enc.ObjectElems+8, a64C)
	c.keepObject(v, tgt+c.enc.RefOffset, marking)
}

// appendElem is amd64's: an element appended where its array has room,
// or slow, having written nothing; while the collector marks, its pointer
// words through storeRef (ptrStore).
func (c *a64Compiler) appendElem(v *ssa.Value, slow arm64.Label, marking bool) {
	size := int32(unsafe.Sizeof(abi.GoArg{}))
	tgt, val := abi.OffGoArgs, abi.OffGoArgs+size
	c.arrayOperand(tgt, slow)
	c.a.Load(a64A, a64B, c.enc.ObjectElems+8)
	c.a.Load(a64C, a64B, c.enc.ObjectElems+16)
	c.a.Cmp(a64A, a64C, true)
	c.a.BCond(arm64.HS, slow)
	c.a.Load(a64C, a64B, c.enc.ObjectElems)
	c.a.ShiftImm(arm64.Lsl, a64D, a64A, 4, true)
	c.a.Op(arm64.Add, a64C, a64C, a64D, true)
	c.a.AddImm(a64A, a64A, 1, true)
	c.a.Store(a64B, c.enc.ObjectElems+8, a64A)
	c.a.Load(a64A, a64Ctx, val+c.enc.NumOffset)
	c.a.Store(a64C, c.enc.NumOffset, a64A)
	c.a.AddImm(a64A, a64C, int64(c.enc.RefOffset), true)
	c.a.Load(a64C, a64Ctx, val+c.enc.RefOffset)
	c.ptrStore(marking)
	c.keepObject(v, tgt+c.enc.RefOffset, marking)
}

func (c *a64Compiler) keyedHas(v *ssa.Value, slow arm64.Label) {
	fail := a64GuardLabel(c, slow)
	key, recv := v.Args[0], v.Args[1]
	protos := c.f.ArrayProtos
	absent, named := slow, slow
	if protos[0].Object != 0 {
		absent = c.a.NewLabel()
	}
	if c.f.HasCache != 0 {
		named = c.a.NewLabel()
	}
	if w := c.gpr(key, a64A); w != a64A {
		c.a.MovRR(a64A, w)
	}
	c.movImm(a64B, abi.NumberLimit)
	c.a.Cmp(a64A, a64B, true)
	c.a.BCond(arm64.HS, named)
	c.a.FMovToF(a64F2, a64A)
	c.a.Fcvtzs(a64B, a64F2)
	c.a.Scvtf(a64F2, a64B, true)
	c.a.FMovFromF(a64C, a64F2)
	c.a.Cmp(a64C, a64A, true)
	fail.jump(arm64.NE)
	c.a.FMovToF(a64F2, a64B)
	w := c.gpr(recv, a64A)
	c.movImm(a64C, c.enc.Object)
	c.a.Cmp(w, a64C, true)
	fail.jump(arm64.NE)
	c.pointerWord(recv, w)
	c.a.Cbz(a64B, slow, true)
	c.a.LoadU8(a64C, a64B, c.enc.ObjectClass)
	c.a.CmpImm(a64C, int64(c.enc.ClassArray), false)
	fail.jump(arm64.NE)
	c.a.FMovToF(a64F1, a64B)
	c.a.FMovFromF(a64A, a64F2)
	c.a.Load(a64C, a64B, c.enc.ObjectElems+8)
	c.a.Cmp(a64A, a64C, true)
	c.a.BCond(arm64.HS, absent)
	c.a.Load(a64B, a64B, c.enc.ObjectElems)
	c.a.AddLsl(a64A, a64B, a64A, 4)
	c.a.Load(a64C, a64A, c.enc.NumOffset)
	c.movImm(a64D, c.enc.Uninitialized)
	c.a.Cmp(a64C, a64D, true)
	c.a.BCond(arm64.EQ, absent)
	keep := abi.OffKeep + int32(v.Index)*int32(c.enc.ValueSize)
	answer := func(yes bool) {
		word := c.enc.False
		if yes {
			word = c.enc.True
		}
		c.movImm(a64C, word)
		c.a.Store(a64Ctx, keep+c.enc.NumOffset, a64C)
		c.movImm(a64C, 0)
		c.a.Store(a64Ctx, keep+c.enc.RefOffset, a64C)
	}
	answer(true)
	done := c.a.NewLabel()
	if protos[0].Object != 0 {
		c.a.B(done)
		c.a.Bind(absent)
		c.a.FMovFromF(a64A, a64F2)
		c.a.ShiftImm(arm64.Lsr, a64A, a64A, 31, true)
		c.a.Cbnz(a64A, slow, true)
		c.a.FMovFromF(a64D, a64F1)
		c.a.LoadU8(a64B, a64D, c.enc.ObjectFlags)
		c.movImm(a64C, uint64(c.enc.FlagSparse))
		c.a.Tst(a64B, a64C, false)
		fail.jump(arm64.NE)
		c.loadProtoPlain(a64B, a64D)
		c.movImm(a64C, uint64(protos[0].Object))
		c.a.Cmp(a64B, a64C, true)
		fail.jump(arm64.NE)
		c.protoGuards(protos, fail)
		answer(false)
	}
	if c.f.HasCache != 0 {
		c.a.B(done)
		// A string, its pointer in F2, of an object, its pointer in F1
		// and its shape in C, which the cache's entry at its slot, in A,
		// answers.
		c.a.Bind(named)
		c.movImm(a64B, c.enc.String)
		c.a.Cmp(a64A, a64B, true)
		fail.jump(arm64.NE)
		c.pointerWord(key, a64A)
		c.a.Cbz(a64B, slow, true)
		c.a.FMovToF(a64F2, a64B)
		w := c.gpr(recv, a64A)
		c.movImm(a64C, c.enc.Object)
		c.a.Cmp(w, a64C, true)
		fail.jump(arm64.NE)
		c.pointerWord(recv, w)
		c.a.Cbz(a64B, slow, true)
		c.a.FMovToF(a64F1, a64B)
		c.a.Load(a64C, a64B, c.enc.ObjectShape)
		c.a.Cbz(a64C, slow, true)
		c.a.FMovFromF(a64A, a64F2)
		c.a.ShiftImm(arm64.Lsr, a64A, a64A, 4, true)
		c.a.ShiftImm(arm64.Lsr, a64D, a64C, 4, true)
		c.a.Op(arm64.Eor, a64A, a64A, a64D, true)
		c.movImm(a64D, abi.HasCacheMul)
		c.a.Mul(a64A, a64A, a64D)
		c.a.ShiftImm(arm64.Lsr, a64A, a64A, 64-abi.HasCacheBits+abi.HasCacheWaysLog, true)
		c.a.ShiftImm(arm64.Lsl, a64A, a64A, hasEntryShift+abi.HasCacheWaysLog, true)
		c.movImm(a64D, uint64(c.f.HasCache))
		c.a.Op(arm64.Add, a64A, a64A, a64D, true)
		hit := c.a.NewLabel()
		for way := range abi.HasCacheWays {
			next := slow
			if way < abi.HasCacheWays-1 {
				next = c.a.NewLabel()
			}
			c.a.Load(a64D, a64A, abi.HasEntryKey)
			c.a.FMovFromF(a64B, a64F2)
			c.a.Cmp(a64D, a64B, true)
			c.a.BCond(arm64.NE, next)
			c.a.Load(a64D, a64A, abi.HasEntryShape)
			c.a.Cmp(a64D, a64C, true)
			c.a.BCond(arm64.NE, next)
			c.a.FMovFromF(a64B, a64F1)
			c.loadProtoPlain(a64B, a64B)
			c.a.Load(a64D, a64A, abi.HasEntryProto)
			c.a.Cmp(a64B, a64D, true)
			c.a.BCond(arm64.NE, next)
			if way < abi.HasCacheWays-1 {
				c.a.B(hit)
				c.a.Bind(next)
				c.a.AddImm(a64A, a64A, abi.HasEntrySize, true)
			}
		}
		c.a.Bind(hit)
		c.a.Load(a64B, a64A, abi.HasEntryCell)
		c.a.LoadU8(a64B, a64B, 0)
		c.a.Cbz(a64B, slow, false)
		c.a.LoadU8(a64B, a64A, abi.HasEntryHas)
		no := c.a.NewLabel()
		c.a.Cbz(a64B, no, false)
		answer(true)
		c.a.B(done)
		c.a.Bind(no)
		answer(false)
	}
	c.a.Bind(done)
}

// lengthWrite is amd64's: an array's length written natively where it
// shortens a dense array or leaves it as it is. It uses A, B, C, D, F1 and
// F2, and no other.
func (c *a64Compiler) lengthWrite(v *ssa.Value, slow arm64.Label) {
	fail := a64GuardLabel(c, slow)
	recv, x := v.Args[0], v.Args[1]
	if w := c.gpr(x, a64A); w != a64A {
		c.a.MovRR(a64A, w)
	}
	c.movImm(a64B, abi.NumberLimit)
	c.a.Cmp(a64A, a64B, true)
	fail.jump(arm64.HS)
	c.a.FMovToF(a64F1, a64A)
	c.a.Fcvtzs(a64B, a64F1)
	c.a.Scvtf(a64F1, a64B, true)
	c.a.FMovFromF(a64C, a64F1)
	c.a.Cmp(a64C, a64A, true)
	fail.jump(arm64.NE)
	c.a.FMovToF(a64F2, a64B)
	w := c.gpr(recv, a64A)
	c.movImm(a64C, c.enc.Object)
	c.a.Cmp(w, a64C, true)
	fail.jump(arm64.NE)
	c.pointerWord(recv, w)
	c.a.Cbz(a64B, slow, true)
	c.a.LoadU8(a64C, a64B, c.enc.ObjectClass)
	c.a.CmpImm(a64C, int64(c.enc.ClassArray), false)
	fail.jump(arm64.NE)
	c.a.LoadU8(a64C, a64B, c.enc.ObjectFlags)
	c.movImm(a64D, uint64(c.enc.FlagLengthWritable|c.enc.FlagSparse|c.enc.FlagShared))
	c.a.Op(arm64.And, a64C, a64C, a64D, false)
	c.a.CmpImm(a64C, int64(c.enc.FlagLengthWritable), false)
	fail.jump(arm64.NE)
	c.a.LoadU32(a64C, a64B, c.enc.ObjectPropsLen)
	c.a.Cbnz(a64C, slow, true)
	c.a.FMovFromF(a64A, a64F2)
	c.a.Load(a64C, a64B, c.enc.ObjectElems+8)
	c.a.Cmp(a64A, a64C, true)
	fail.jump(arm64.HI)
	c.a.Store(a64B, c.enc.ObjectElems+8, a64A)
	c.a.Load(a64B, a64B, c.enc.ObjectElems)
	c.a.ShiftImm(arm64.Lsl, a64A, a64A, 4, true)
	c.a.ShiftImm(arm64.Lsl, a64C, a64C, 4, true)
	c.a.Op(arm64.Add, a64A, a64A, a64B, true)
	c.a.Op(arm64.Add, a64C, a64C, a64B, true)
	loop, cleared, marked := c.a.NewLabel(), c.a.NewLabel(), c.a.NewLabel()
	c.movImm(a64B, c.enc.WriteBarrier)
	c.a.LoadU8(a64B, a64B, 0)
	c.a.Cbnz(a64B, marked, false)
	c.cold = append(c.cold, func() {
		c.a.Bind(marked)
		c.lengthClear()
		c.a.B(cleared)
	})
	c.movImm(a64D, 0)
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
	c.a.AddImm(a64A, a64Ctx, int64(keep+c.enc.RefOffset), true)
	c.movImm(a64C, 0)
	c.storeByFlag()
}

// lengthClear is amd64's: the elements from a64A to a64C cleared while the
// collector marks, each pointer word through storeRef (ptrStore), where
// it is and where it ends kept in the context's GoArgs.
func (c *a64Compiler) lengthClear() {
	cur, end := abi.OffGoArgs, abi.OffGoArgs+8
	c.a.Store(a64Ctx, cur, a64A)
	c.a.Store(a64Ctx, end, a64C)
	loop, done := c.a.NewLabel(), c.a.NewLabel()
	c.a.Bind(loop)
	c.a.Load(a64A, a64Ctx, cur)
	c.a.Load(a64C, a64Ctx, end)
	c.a.Cmp(a64A, a64C, true)
	c.a.BCond(arm64.HS, done)
	c.movImm(a64C, 0)
	c.a.Store(a64A, c.enc.NumOffset, a64C)
	c.a.AddImm(a64A, a64A, int64(c.enc.RefOffset), true)
	c.ptrStore(true)
	c.a.Load(a64A, a64Ctx, cur)
	c.a.AddImm(a64A, a64A, int64(c.enc.ValueSize), true)
	c.a.Store(a64Ctx, cur, a64A)
	c.a.B(loop)
	c.a.Bind(done)
}

// storeByFlag is amd64's: ptrStore, plainly or marking as the collector's
// flag says, the pointer word in a64C to the address in a64A.
func (c *a64Compiler) storeByFlag() {
	marked, done := c.a.NewLabel(), c.a.NewLabel()
	c.movImm(a64B, c.enc.WriteBarrier)
	c.a.LoadU8(a64B, a64B, 0)
	c.a.Cbnz(a64B, marked, false)
	c.ptrStore(false)
	c.a.Bind(done)
	c.cold = append(c.cold, func() {
		c.a.Bind(marked)
		c.ptrStore(true)
		c.a.B(done)
	})
}

// mapStep is amd64's: a for...of's step of a Map's or a Set's iterator,
// the cursor's state in D. It uses A, B, C, D and F1, and no other.
func (c *a64Compiler) mapStep(v *ssa.Value, nexts [2]uintptr, slow arm64.Label) {
	fail := a64GuardLabel(c, slow)
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
	fail.jump(arm64.NE)
	c.a.Load(a64B, a64D, c.enc.IterIter+c.enc.RefOffset)
	c.objectData(a64D, a64B, a64C, c.enc.MapIterData, fail)
	c.a.LoadU8(a64C, a64D, c.enc.MapIterDone)
	c.a.Cbnz(a64C, slow, false)
	kind := c.a.NewLabel()
	c.a.LoadU8(a64C, a64D, c.enc.MapIterKind)
	c.a.CmpImm(a64C, int64(c.enc.MapKeys), false)
	c.a.BCond(arm64.EQ, kind)
	c.a.CmpImm(a64C, int64(c.enc.MapValues), false)
	fail.jump(arm64.NE)
	c.a.Bind(kind)
	c.a.Load(a64A, a64D, c.enc.MapIterMap)
	c.a.Cbz(a64A, slow, true)
	c.a.LoadU32(a64B, a64D, c.enc.MapIterGen)
	c.a.LoadU32(a64C, a64A, c.enc.MapGen)
	c.a.Cmp(a64B, a64C, false)
	fail.jump(arm64.NE)
	c.a.Load(a64B, a64D, c.enc.MapIterAt)
	c.a.Load(a64C, a64A, c.enc.MapEntries+8)
	c.a.Cmp(a64B, a64C, true)
	fail.jump(arm64.HS)
	c.a.Load(a64A, a64A, c.enc.MapEntries)
	c.movImm(a64C, uint64(c.enc.MapEntrySize))
	c.a.Mul(a64C, a64C, a64B)
	c.a.Op(arm64.Add, a64A, a64A, a64C, true)
	c.a.FMovToF(a64F1, a64B)
	c.a.Load(a64C, a64A, c.enc.NumOffset)
	c.movImm(a64B, c.enc.MapDeleted)
	c.a.Cmp(a64C, a64B, true)
	fail.jump(arm64.EQ)
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
		fail.jump(arm64.EQ)
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
// shaded first. Register code's writes its window to its registers, as
// amd64's (ssa.TailWindow), and polls only where its start does not
// (ssa.TailPolled). All but the depth's check is out of line (tailDeep),
// as amd64's.
func (c *a64Compiler) tailSelf(v *ssa.Value, guard a64Guard) {
	shallow, deep := c.a.NewLabel(), c.a.NewLabel()
	c.a.Load(a64A, a64Ctx, abi.OffLevel)
	c.a.Load(a64B, a64Ctx, abi.OffBounds)
	c.a.Load(a64B, a64B, abi.OffBoundsTailLevel)
	c.a.Cmp(a64A, a64B, true)
	c.a.BCond(arm64.HS, deep)
	c.cold = append(c.cold, func() {
		c.a.Bind(deep)
		c.tailDeep(v, guard)
		c.a.B(shallow)
	})
	c.a.Bind(shallow)
}

// tailDeep is tailSelf's call deep in the stack, out of line, as amd64's.
func (c *a64Compiler) tailDeep(v *ssa.Value, guard a64Guard) {
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
	if v.Key&ssa.TailPolled == 0 {
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
	}
	c.a.Load(a64A, a64Ctx, abi.OffTailFrame)
	c.a.CmpImm(a64A, 0, true)
	guard.jump(arm64.EQ)
	if !c.reference(v, v.Args[0], c.enc.Object, guard) {
		return
	}
	c.a.Store(a64Ctx, abi.OffCallCallee, a64C)
	size := int32(unsafe.Sizeof(abi.GoArg{}))
	// As amd64's: the window's function and a plain call's receiver not
	// staged (tailUnstaged).
	fnDirect, thisUndef := tailUnstaged(v, args)
	unstaged := func(i int) bool { return i == 0 && fnDirect || i == 1 && thisUndef }
	for i, x := range args {
		if !unstaged(i) {
			c.goArg(abi.OffGoArgs+int32(i)*size, x)
		}
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
			switch {
			case i == 0 && fnDirect:
				c.a.Load(a64C, a64Ctx, abi.OffCallCallee)
			case unstaged(i):
				continue
			default:
				c.a.Load(a64C, a64Ctx, abi.OffGoArgs+int32(i)*size+c.enc.RefOffset)
			}
			c.refStore()
		}
		// Shaded, kept no longer (abi.Context's Shade).
		c.a.Store(a64Ctx, abi.OffShade, arm64.ZR)
		c.a.B(shaded)
	})
	other := c.a.NewLabel()
	c.a.Load(a64C, a64Ctx, abi.OffCallCallee)
	c.objectData(a64B, a64C, a64C, c.enc.FuncData, guard)
	c.a.Load(a64B, a64B, c.enc.FuncClosure)
	c.a.Load(a64A, a64Ctx, abi.OffClosure)
	c.a.Cmp(a64A, a64B, true)
	c.a.BCond(arm64.NE, other)
	slots := int(v.Const.Bits)
	if v.Key&ssa.TailWindow != 0 {
		// Register code's window: its function, receiver, parameters and
		// bindings, which the interpreter's call makes and its code's
		// start reads -- not its temporaries, which the code writes before
		// it reads them, as a native call leaves them (windowSetup).
		slots = min(slots, c.f.InitLocals())
	}
	for i := range slots {
		nb, nd := c.slotAddr(i, false, a64C)
		rb, rd := c.slotAddr(i, true, a64C)
		if i == 0 && fnDirect {
			c.movImm(a64A, c.enc.Object)
			c.a.Store(nb, nd, a64A)
			c.a.Load(a64A, a64Ctx, abi.OffCallCallee)
			c.a.Store(rb, rd, a64A)
			continue
		}
		if i < v.Index && i < len(args) && !unstaged(i) {
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
	if v.Key&ssa.TailWindow != 0 {
		// Register code's receiver, its register 1: the VM's frame's too,
		// if Go entered the code with it (amd64's).
		at := abi.OffGoArgs + size
		none := c.a.NewLabel()
		c.a.Load(a64C, a64Ctx, abi.OffFrameThis)
		c.a.Cbz(a64C, none, true)
		if thisUndef {
			c.movImm(a64A, c.enc.Undefined)
			c.a.Store(a64C, c.enc.NumOffset, a64A)
			c.a.Store(a64C, c.enc.RefOffset, arm64.ZR)
		} else {
			c.a.Load(a64A, a64Ctx, at+c.enc.NumOffset)
			c.a.Store(a64C, c.enc.NumOffset, a64A)
			c.a.Load(a64A, a64Ctx, at+c.enc.RefOffset)
			c.a.Store(a64C, c.enc.RefOffset, a64A)
		}
		c.a.Bind(none)
	}
	c.a.B(c.labels[entry.ID])
	c.a.Bind(other)
	window := v.Key&ssa.TailWindow != 0
	var sites []*ssa.CallSite
	for _, t := range v.Calls() {
		if t.Go == 0 && !t.Literal && !t.Push && !t.Pop && !t.Alloc && !t.Receiver && t.Via == 0 && t.Pool == 0 && !t.Coerce && t.Entry != 0 && !t.FrameArgs && !t.Function &&
			(t.Window != 0) == window && (!window || !t.ArrowThis && t.Global == 0 && !t.Generic && !t.Forward && !t.Super) {
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
		if window {
			c.windowTailJump(t, len(args), fnDirect, thisUndef, guard)
		} else {
			c.tailJump(t, len(args), this != nil, guard)
		}
		c.a.Bind(next)
	}
	c.a.B(stub)
}

// tailEntry is register code's entry for another function's call in tail
// position (ssa.Func's TailEntry), as amd64's: what its call entry makes
// past the window, of its own frame's size -- the context's Stack, the
// VM's stack's top and high water -- its bindings past its parameters
// undefined, then the start.
func (c *a64Compiler) tailEntry() {
	vs := int32(c.enc.ValueSize)
	c.storeCode(a64Ctx, a64B)
	c.a.AddImm(a64Stack, a64Locals, int64(c.f.FrameLocals+c.f.Scratch)*int64(vs), true)
	c.a.Store(a64Ctx, abi.OffStack, a64Stack)
	c.a.Load(a64B, a64Ctx, abi.OffBase)
	c.a.AddImm(a64B, a64B, int64(c.frameSize()), true)
	c.a.Load(a64C, a64Ctx, abi.OffStackTop)
	c.a.Store(a64C, 0, a64B)
	same := c.a.NewLabel()
	c.a.Load(a64C, a64Ctx, abi.OffStackHigh)
	c.a.Load(a64D, a64C, 0)
	c.a.Cmp(a64B, a64D, true)
	c.a.BCond(arm64.LS, same)
	c.a.Store(a64C, 0, a64B)
	c.a.Bind(same)
	if c.tailInits() {
		c.movImm(a64A, c.enc.Undefined)
		for i := c.f.Params; i < c.f.InitLocals(); i++ {
			c.a.Store(a64Locals, int32(i)*vs+c.enc.NumOffset, a64A)
			c.a.Store(a64Locals, int32(i)*vs+c.enc.RefOffset, arm64.ZR)
		}
	}
	c.a.B(c.labels[c.entryAt(0).ID])
}

// windowTailJump is tailSelf's call of t, register code's callee, as
// amd64's.
func (c *a64Compiler) windowTailJump(t *ssa.CallSite, n int, fn, this bool, guard a64Guard) {
	vs, size := int32(c.enc.ValueSize), int32(unsafe.Sizeof(abi.GoArg{}))
	link := uint64(t.Entry) - uint64(c.enc.LinkEntry)
	c.movImm(a64A, link)
	c.a.Load(a64B, a64A, c.enc.LinkBody)
	c.a.CmpImm(a64B, 0, true)
	guard.jump(arm64.EQ)
	// As amd64's: the room for its window, a constant.
	c.a.Load(a64B, a64Ctx, abi.OffBase)
	c.a.AddImm(a64B, a64B, int64(t.LocalCount), true)
	c.a.Load(a64A, a64Ctx, abi.OffStackEnd)
	c.a.Cmp(a64B, a64A, true)
	guard.jump(arm64.HI)
	// As amd64's: its function, receiver and parameters alone, the
	// function the callee and the receiver undefined where fn and this.
	for i := 0; i < t.Params && i < t.LocalCount; i++ {
		at := int32(i) * vs
		switch {
		case i == 0 && fn:
			c.movImm(a64A, c.enc.Object)
			c.a.Store(a64Locals, at+c.enc.NumOffset, a64A)
			c.a.Load(a64A, a64Ctx, abi.OffCallCallee)
			c.a.Store(a64Locals, at+c.enc.RefOffset, a64A)
		case i < n && !(i == 1 && this):
			g := abi.OffGoArgs + int32(i)*size
			c.a.Load(a64A, a64Ctx, g+c.enc.NumOffset)
			c.a.Store(a64Locals, at+c.enc.NumOffset, a64A)
			c.a.Load(a64A, a64Ctx, g+c.enc.RefOffset)
			c.a.Store(a64Locals, at+c.enc.RefOffset, a64A)
		default:
			c.movImm(a64A, c.enc.Undefined)
			c.a.Store(a64Locals, at+c.enc.NumOffset, a64A)
			c.a.Store(a64Locals, at+c.enc.RefOffset, arm64.ZR)
		}
	}
	if t.ByCode != 0 {
		c.a.Load(a64A, a64Ctx, abi.OffCallClosure)
		c.a.Store(a64Ctx, abi.OffClosure, a64A)
		c.a.Load(a64A, a64Ctx, abi.OffCallUpvalues)
		c.a.Store(a64Ctx, abi.OffUpvalues, a64A)
	} else {
		// As amd64's: its closure's captured bindings.
		c.movImm(a64A, uint64(t.Closure))
		c.a.Store(a64Ctx, abi.OffClosure, a64A)
		c.a.Load(a64A, a64A, c.enc.ClosureUpvalues)
		c.a.Store(a64Ctx, abi.OffUpvalues, a64A)
	}
	c.a.Store(a64Ctx, abi.OffOpen, arm64.ZR)
	c.a.Store(a64Ctx, abi.OffCells, arm64.ZR)
	c.a.Store(a64Ctx, abi.OffLoop, arm64.ZR)
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
	// As amd64's: on at its tail entry, which makes what its call entry
	// makes past the window.
	c.movImm(a64A, link)
	c.a.Load(a64A, a64A, c.enc.LinkBody)
	c.a.Br(a64A)
}

// awaitReady is amd64's: an async function's await that goes on in place
// (ssa.OpAwaitReady).
func (c *a64Compiler) awaitReady(v *ssa.Value, guard a64Guard) {
	c.a.Load(a64A, a64Ctx, abi.OffAwaitJob)
	c.a.CmpImm(a64A, 0, true)
	guard.jump(arm64.EQ)
	c.a.Load(a64A, a64Ctx, abi.OffJobHead)
	c.a.Load(a64A, a64A, 0)
	c.a.Load(a64B, a64Ctx, abi.OffJobCount)
	c.a.Load(a64B, a64B, 0)
	c.a.Cmp(a64A, a64B, true)
	guard.jump(arm64.NE)
	w := c.gpr(v.Args[0], a64B)
	c.movImm(a64A, c.enc.Object)
	c.a.Cmp(w, a64A, true)
	d := c.gdst(v)
	c.a.Cset(d, arm64.EQ)
	c.setG(v, d)
}

// entryPoll is register code's poll at its start (ssa.OpPoll), as
// amd64's.
func (c *a64Compiler) entryPoll(v *ssa.Value) {
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
}

// tailJump is tailSelf's call of t, as amd64's.
func (c *a64Compiler) tailJump(t *ssa.CallSite, n int, this bool, guard a64Guard) {
	vs, size := int32(c.enc.ValueSize), int32(unsafe.Sizeof(abi.GoArg{}))
	c.movImm(a64A, uint64(t.Entry))
	c.a.Load(a64A, a64A, 0)
	c.a.CmpImm(a64A, 0, true)
	guard.jump(arm64.EQ)
	c.a.Load(a64B, a64Ctx, abi.OffBase)
	c.a.AddImm(a64B, a64B, int64(t.LocalCount+t.MaxStack), true)
	c.a.Load(a64A, a64Ctx, abi.OffStackEnd)
	c.a.Cmp(a64B, a64A, true)
	guard.jump(arm64.HI)
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
	c.goCatch = c.catchStub(v)
	if saving {
		c.goCalling(st, &stub)
	} else {
		c.goCallingLive(v, st, stub)
	}
	c.goCatch = 0
	c.a.Load(a64A, a64Ctx, abi.OffKeep+int32(v.Index)*int32(c.enc.ValueSize)+c.enc.NumOffset)
	c.setG(v, a64A)
}

// goCallWindow is amd64's: a call of Go of more operands than the context
// has room for (windowGoCall), written to the callee's window past the
// VM's stack's top, in D, the room checked first.
func (c *a64Compiler) goCallWindow(v *ssa.Value, op uint64) {
	vs := int32(c.enc.ValueSize)
	method := op>>abi.GoOpKindShift&0xff == abi.GoCallMethod
	st, _ := c.goState(op, v.Index, v, nil)
	stub := c.stubLabel(v.State, exitKind(v.Aux))
	room := int64(len(v.Args) + 1)
	if method {
		room--
	}
	c.a.Load(a64D, a64Ctx, abi.OffStackTop)
	c.a.Load(a64D, a64D, 0)
	c.a.AddImm(a64B, a64D, room, true)
	c.a.Load(a64A, a64Ctx, abi.OffStackEnd)
	c.a.Cmp(a64B, a64A, true)
	c.a.BCond(arm64.HI, stub)
	c.a.Load(a64B, a64Ctx, abi.OffStackBase)
	c.a.AddLsl(a64D, a64B, a64D, 4)
	for i, x := range v.Args {
		c.directOperand(v, x, a64D, windowGoDisp(i, method, vs))
	}
	c.goCatch = c.catchStub(v)
	c.goCalling(st, &stub)
	c.goCatch = 0
	c.a.Load(a64A, a64Ctx, abi.OffKeep+int32(v.Index)*vs+c.enc.NumOffset)
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
			c.spillStoreF(sv.slot, arm64.FReg(sv.reg))
		} else {
			c.spillStore(sv.slot, arm64.Reg(sv.reg))
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
			c.spillLoadF(arm64.FReg(sv.reg), sv.slot)
		} else {
			c.spillLoad(arm64.Reg(sv.reg), sv.slot)
		}
	}
}

// markingTake is amd64's: while the collector marks, the take with Go's
// write barrier (arrayElements, then poolTakeMarking); native code goes on
// at the label returned.
func (c *a64Compiler) markingTake(v *ssa.Value, site *ssa.CallSite) arm64.Label {
	c.movImm(a64B, c.enc.WriteBarrier)
	c.a.LoadU8(a64B, a64B, 0)
	marked, join := c.a.NewLabel(), c.a.NewLabel()
	c.a.Cbnz(a64B, marked, false)
	c.cold = append(c.cold, func() {
		c.a.Bind(marked)
		if site.Literal && site.Argc != 0 {
			c.arrayElements(v, site, true)
		}
		c.poolTakeMarking(v, site)
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

// poolRefill is amd64's: where a take from an empty pool goes, out of
// line, to have Go fill it (ssa.GoRefill) and take again from top, or to
// slow where Go cannot.
func (c *a64Compiler) poolRefill(v *ssa.Value, pool uintptr, top, slow arm64.Label) arm64.Label {
	empty := c.a.NewLabel()
	c.cold = append(c.cold, func() {
		c.a.Bind(empty)
		c.movImm(a64A, uint64(pool))
		c.a.Store(a64Ctx, abi.OffGoArgs+c.enc.RefOffset, a64A)
		c.callGo(v, uint64(ssa.GoRefill), 0)
		c.a.Load(a64B, a64Ctx, abi.OffGoStatus)
		c.a.Cbnz(a64B, slow, true)
		c.a.B(top)
	})
	return empty
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
	c.pointerWord(x, c.gpr(x, a64A))
	c.setG(v, a64B)
}

// keep copies a value into a keep cell, as amd64's: a run of keeps tests
// the write-barrier flag once.
func (c *a64Compiler) keep(v *ssa.Value) {
	run := c.keepRun(v)
	if run&keepFirst != 0 {
		c.keepCold, c.keeping = c.a.NewLabel(), c.keeping[:0]
		c.movImm(a64B, c.enc.WriteBarrier)
		c.a.LoadU8(a64B, a64B, 0)
		c.a.Cbnz(a64B, c.keepCold, false)
	}
	c.keeping = append(c.keeping, v)
	c.keepStore(v, false)
	if run&keepLast != 0 {
		cold, done, keeps := c.keepCold, c.a.NewLabel(), slices.Clone(c.keeping)
		c.a.Bind(done)
		c.cold = append(c.cold, func() {
			c.a.Bind(cold)
			for _, k := range keeps {
				c.keepStore(k, true)
			}
			c.a.B(done)
		})
	}
}

// keepStore is amd64's.
func (c *a64Compiler) keepStore(v *ssa.Value, marking bool) {
	at := abi.OffKeep + int32(v.Index)*int32(c.enc.ValueSize)
	if len(v.Args) == 1 {
		// As amd64's: its pointer word read here, into B.
		x := v.Args[0]
		w := c.gpr(x, a64A)
		c.pointerWord(x, w)
		c.a.Store(a64Ctx, at+c.enc.NumOffset, w)
		if marking {
			c.a.MovRR(a64C, a64B)
			c.a.AddImm(a64A, a64Ctx, int64(at+c.enc.RefOffset), true)
			c.refStore()
		} else {
			c.a.Store(a64Ctx, at+c.enc.RefOffset, a64B)
		}
		c.cellAddr(v, at)
		return
	}
	c.a.Store(a64Ctx, at+c.enc.NumOffset, c.gpr(v.Args[0], a64B))
	if marking {
		c.gprInto(v.Args[1], a64C)
		c.a.AddImm(a64A, a64Ctx, int64(at+c.enc.RefOffset), true)
		c.refStore()
	} else {
		c.a.Store(a64Ctx, at+c.enc.RefOffset, c.gpr(v.Args[1], a64C))
	}
	c.cellAddr(v, at)
}

// cellAddr is amd64's.
func (c *a64Compiler) cellAddr(v *ssa.Value, at int32) {
	if _, ok := c.loc(v); !ok {
		return
	}
	d := c.gdst(v)
	c.a.AddImm(d, a64Ctx, int64(at), true)
	c.setG(v, d)
}

// bigUnbox is the BigInt a tagged value is, as amd64's.
func (c *a64Compiler) bigUnbox(v *ssa.Value, guard a64Guard) {
	x := v.Args[0]
	low := 64 - c.enc.ShortBigLowBits
	c.a.ShiftImm(arm64.Lsr, a64A, c.gpr(x, a64B), c.enc.ShortBigLowBits, true)
	c.movImm(a64B, c.enc.ShortBigTag>>c.enc.ShortBigLowBits)
	c.a.Cmp(a64A, a64B, true)
	guard.jump(arm64.NE)
	c.keepSource(x)
	c.a.CmpImm(a64C, 0, true)
	guard.jump(arm64.EQ)
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
func (c *a64Compiler) holder(v *ssa.Value, guard a64Guard) {
	var first ssa.HolderChain
	if v.Holders() != nil {
		first = *v.Holders()
	}
	cases := v.Cases()
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
			guard.jump(arm64.NE)
		}
	}
	for i := 0; i <= len(cases); i++ {
		k := ssa.PropertyCase{Shape: uintptr(v.Const.Bits), Index: int32(v.Index), Holders: first.Holders, More: first.More}
		if i > 0 {
			k = cases[i-1]
		}
		next := c.a.NewLabel()
		p := c.gpr(v.Args[0], a64A)
		c.a.Load(a64B, p, c.enc.ObjectShape)
		c.movImm(a64C, uint64(k.Shape))
		c.a.Cmp(a64B, a64C, true)
		if i == len(cases) {
			miss(scan)
		} else {
			c.a.BCond(arm64.NE, next)
		}
		if p != a64A {
			c.a.MovRR(a64A, p)
		}
		// Only the holders' layouts, each saying the next's prototype, as
		// amd64's holder compares them.
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
			c.a.Load(a64B, a64A, c.enc.ObjectShape)
			c.movImm(a64C, uint64(h.Shape))
			c.a.Cmp(a64B, a64C, true)
			miss(stale)
		}
		c.a.Load(a64A, a64A, c.enc.ObjectProps)
		c.a.AddImm(a64A, a64A, int64(k.Index*c.enc.ValueSize), true)
		if i < len(cases) {
			c.a.B(done)
			c.a.Bind(next)
		}
	}
	c.a.Bind(done)
}

// protoChain checks the prototypes chain is, as amd64's does, the last left
// in A. It uses A, B and C.
func (c *a64Compiler) protoChain(chain []ssa.Holder, guard a64Guard, null bool) {
	// Each object's layout, which says its prototype, as amd64's.
	for _, h := range chain {
		c.movImm(a64A, uint64(h.Object))
		c.a.Load(a64B, a64A, c.enc.ObjectShape)
		c.movImm(a64C, uint64(h.Shape))
		c.a.Cmp(a64B, a64C, true)
		guard.jump(arm64.NE)
	}
}

// integer converts the double x to an integer in r, failing unless it is
// one an int64 holds exactly: FCVTZS saturates, so a result of 2**63-1,
// which no double converts to exactly, means x is 2**63 or more. It uses
// D and F2.
func (c *a64Compiler) integer(r arm64.Reg, x arm64.FReg, guard a64Guard) {
	c.a.Fcvtzs(r, x)
	c.a.Scvtf(a64F2, r, true)
	c.a.FCmp(a64F2, x)
	guard.jump(arm64.NE)
	c.movImm(a64D, math.MaxInt64)
	c.a.Cmp(r, a64D, true)
	guard.jump(arm64.EQ)
}

// remainder is JavaScript's % of two integers, as amd64's is. SDIV never
// faults -- -2**63 / -1 is -2**63, whose MSUB remainder is 0 -- so no
// divisor needs a case of its own.
func (c *a64Compiler) remainder(v *ssa.Value, guard a64Guard) {
	x, y := c.fpr(v.Args[0], a64F0), c.fpr(v.Args[1], a64F1)
	zero, done := c.a.NewLabel(), c.a.NewLabel()
	divisor := c.stubLabel(v.State, exitKind(v.Aux))
	if stub := c.mathStub(ssa.GoMod); stub != 0 {
		// As amd64's: through the remainder's own routine, the operands in
		// F0 and F1 from the registers the checks below leave as they are
		// -- y is never in F0, nor x in F1.
		slow := c.a.NewLabel()
		guard, divisor = a64GuardLabel(c, slow), slow
		c.cold = append(c.cold, func() {
			c.a.Bind(slow)
			if x != a64F0 {
				c.a.FMov(a64F0, x)
			}
			if y != a64F1 {
				c.a.FMov(a64F1, y)
			}
			c.mathStubSaving(stub)
			c.a.B(done)
		})
	} else if c.enc.CallGo != 0 {
		// What is not integers Go computes (ssa.GoMod), the operands' bits
		// in the context's GoArgs, from registers the checks below leave as
		// they are.
		size := int32(unsafe.Sizeof(abi.GoArg{}))
		slow, stub := c.a.NewLabel(), divisor
		guard, divisor = a64GuardLabel(c, slow), slow
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
	if stub := c.mathStub(ssa.GoOp(v.Index)); stub != 0 {
		// As amd64's: the operands in F0 and F1, the result in F0.
		if x := c.fpr(v.Args[0], a64F0); x != a64F0 {
			c.a.FMov(a64F0, x)
		}
		if len(v.Args) > 1 {
			if y := c.fpr(v.Args[1], a64F1); y != a64F1 {
				c.a.FMov(a64F1, y)
			}
		}
		if ssa.GoOp(v.Index) == ssa.GoMath {
			c.movImm(a64C, uint64(v.Aux))
		}
		c.mathStubLive(v, stub)
		c.setF(v, a64F0)
		return
	}
	size := int32(unsafe.Sizeof(abi.GoArg{}))
	for i, x := range v.Args {
		c.a.FMovFromF(a64A, c.fpr(x, a64F0))
		c.a.Store(a64Ctx, abi.OffGoArgs+int32(i)*size+c.enc.NumOffset, a64A)
	}
	if ssa.GoOp(v.Index) == ssa.GoMath {
		c.movImm(a64A, uint64(v.Aux))
		c.a.Store(a64Ctx, abi.OffGoArgs+size+c.enc.NumOffset, a64A)
	}
	if c.enc.GoFast != 0 {
		// As amd64's: what lives across it saved, through GoFast.
		st, _ := c.goState(uint64(v.Index), 0, nil, nil)
		saves := c.savesOf(v)
		for _, sv := range saves {
			if sv.float {
				c.spillStoreF(sv.slot, arm64.FReg(sv.reg))
			} else {
				c.spillStore(sv.slot, arm64.Reg(sv.reg))
			}
		}
		reg := c.budgetReg(c.budget)
		if reg >= 0 {
			c.budgetFlush(arm64.Reg(reg))
		}
		back := c.a.NewLabel()
		c.movImm(a64C, uint64(uintptr(unsafe.Pointer(st))))
		c.a.Adr(a64A, back)
		c.movImm(a64B, c.enc.GoFast)
		c.a.Br(a64B)
		c.a.Bind(back)
		c.a.Load(a64Locals, a64Ctx, abi.OffLocals)
		c.a.Load(a64Stack, a64Ctx, abi.OffStack)
		for _, sv := range saves {
			if sv.float {
				c.spillLoadF(arm64.FReg(sv.reg), sv.slot)
			} else {
				c.spillLoad(arm64.Reg(sv.reg), sv.slot)
			}
		}
		if reg >= 0 {
			c.budgetStart(arm64.Reg(reg))
		}
	} else {
		c.goSaving(uint64(v.Index), nil)
	}
	c.a.LoadF(a64F0, a64Ctx, abi.OffGoArgs+c.enc.NumOffset)
	c.setF(v, a64F0)
}

// mathStubLive is amd64's: the operands in F0 and F1, which function in C
// for ssa.GoMath, where to come back in A, the routine's address in B; the
// result comes back in F0.
func (c *a64Compiler) mathStubLive(v *ssa.Value, stub uint64) {
	for _, sv := range c.savesOf(v) {
		switch {
		case sv.frame != 0, sv.cold:
		case sv.float:
			c.spillStoreF(sv.slot, arm64.FReg(sv.reg))
		default:
			c.spillStore(sv.slot, arm64.Reg(sv.reg))
		}
	}
	reg := c.budgetReg(c.budget)
	if reg >= 0 {
		// Its store uses B, which takes the routine's address after.
		c.budgetFlush(arm64.Reg(reg))
	}
	back := c.a.NewLabel()
	c.a.Adr(a64A, back)
	c.movImm(a64B, stub)
	c.a.Br(a64B)
	c.a.Bind(back)
	c.a.Load(a64Locals, a64Ctx, abi.OffLocals)
	c.a.Load(a64Stack, a64Ctx, abi.OffStack)
	c.restoreSavesOf(v, false)
	if reg >= 0 {
		c.budgetStart(arm64.Reg(reg))
	}
}

// mathStubSaving is amd64's: every allocatable register saved around the
// routine, by stubSaving.
func (c *a64Compiler) mathStubSaving(stub uint64) {
	if !c.stubSavingUsed {
		c.stubSaving, c.stubSavingUsed = c.a.NewLabel(), true
	}
	reg := c.budgetReg(c.budget)
	if reg >= 0 {
		c.budgetFlush(arm64.Reg(reg))
	}
	back := c.a.NewLabel()
	c.a.Adr(a64A, back)
	c.a.Store(a64Ctx, abi.OffGoSite, a64A)
	c.movImm(a64B, stub)
	c.a.B(c.stubSaving)
	c.a.Bind(back)
	if reg >= 0 {
		c.budgetStart(arm64.Reg(reg))
	}
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
func (c *a64Compiler) power(v *ssa.Value, guard a64Guard) {
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
	if stub := c.mathStub(ssa.GoPow); stub != 0 {
		// As amd64's: through pow's own routine, the operands in F0 and F1,
		// read back from the context's GoArgs.
		size := int32(unsafe.Sizeof(abi.GoArg{}))
		c.a.FMovFromF(a64A, a64F0)
		c.a.Store(a64Ctx, abi.OffGoArgs+c.enc.NumOffset, a64A)
		c.a.FMovFromF(a64A, a64F1)
		c.a.Store(a64Ctx, abi.OffGoArgs+size+c.enc.NumOffset, a64A)
		slow := c.a.NewLabel()
		guard = a64GuardLabel(c, slow)
		c.cold = append(c.cold, func() {
			c.a.Bind(slow)
			c.a.LoadF(a64F0, a64Ctx, abi.OffGoArgs+c.enc.NumOffset)
			c.a.LoadF(a64F1, a64Ctx, abi.OffGoArgs+size+c.enc.NumOffset)
			c.mathStubSaving(stub)
			c.a.B(done)
		})
	} else if c.enc.CallGo != 0 {
		// What is not exact Go computes (ssa.GoPow).
		size := int32(unsafe.Sizeof(abi.GoArg{}))
		c.a.FMovFromF(a64A, a64F0)
		c.a.Store(a64Ctx, abi.OffGoArgs+c.enc.NumOffset, a64A)
		c.a.FMovFromF(a64A, a64F1)
		c.a.Store(a64Ctx, abi.OffGoArgs+size+c.enc.NumOffset, a64A)
		slow, stub := c.a.NewLabel(), c.stubLabel(v.State, exitKind(v.Aux))
		guard = a64GuardLabel(c, slow)
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
	guard.jump(arm64.HI)
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
	guard.jump(arm64.HI)
	c.a.FMov(a64F0, a64F2)
	c.a.Bind(done)
	c.setF(v, a64F0)
}

// length is x.length, as amd64's is.
func (c *a64Compiler) length(v *ssa.Value, guard a64Guard) {
	a := v.Args[0]
	if a.Shadow == nil && c.origin.At(a) < 0 {
		c.a.B(c.stubLabel(v.State, exitKind(v.Aux)))
		return
	}
	array, have, done := c.a.NewLabel(), c.a.NewLabel(), c.a.NewLabel()
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
		if v.IC != nil && len(v.IC.Cases) != 0 && c.enc.ClassArguments != 0 {
			// Or an arguments object's, as amd64's.
			args := c.a.NewLabel()
			c.a.BCond(arm64.NE, args)
			c.cold = append(c.cold, func() {
				c.a.Bind(args)
				c.argumentsLength(v, done, guard)
			})
		} else {
			guard.jump(arm64.NE)
		}
		if c.enc.TypedEpoch == 0 {
			guard.jump(arm64.EQ)
			return
		}
		retry, stale := c.a.NewLabel(), c.a.NewLabel()
		c.a.Bind(retry)
		c.a.Load(a64B, a64C, c.enc.ObjectExt+c.enc.ObjectData+8)
		c.typedView(a64B, a64A, a64D, stale, guard)
		// Where the copy's is no length to give, Go's.
		c.a.LoadU8(a64A, a64B, c.enc.TypedKind+2)
		c.a.CmpImm(a64A, 0, false)
		guard.jump(arm64.NE)
		c.a.Load(a64C, a64B, c.enc.TypedLength)
		c.a.B(have)
		c.typedStale(stale, retry, guard, func() {
			c.a.Store(a64Ctx, abi.OffGoArgs+c.enc.RefOffset, a64C)
		}, func() {
			// The typed array again, which Go leaves where it was.
			c.a.Load(a64C, a64Ctx, abi.OffGoArgs+c.enc.RefOffset)
		})
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
	c.a.Bind(done)
	c.setF(v, a64F0)
}

// argumentsLength reads the length of the arguments object in C, its class
// in B, into F0, then on to done, as amd64's. It uses A, B and D.
func (c *a64Compiler) argumentsLength(v *ssa.Value, done arm64.Label, guard a64Guard) {
	c.a.CmpImm(a64B, int64(c.enc.ClassArguments), false)
	guard.jump(arm64.NE)
	c.a.Load(a64A, a64C, c.enc.ObjectShape)
	cases := v.IC.Cases
	for i, cs := range cases {
		next := c.a.NewLabel()
		c.movImm(a64D, uint64(cs.Shape))
		c.a.Cmp(a64A, a64D, true)
		if i == len(cases)-1 {
			guard.jump(arm64.NE)
		} else {
			c.a.BCond(arm64.NE, next)
		}
		c.a.Load(a64B, a64C, c.enc.ObjectProps)
		c.a.Load(a64B, a64B, int32(cs.Index)*c.enc.ValueSize+c.enc.NumOffset)
		c.movImm(a64D, abi.NumberLimit)
		c.a.Cmp(a64B, a64D, true)
		guard.jump(arm64.HS)
		c.a.FMovToF(a64F0, a64B)
		c.a.B(done)
		c.a.Bind(next)
	}
}

// stringCode is charCodeAt called on a string, as amd64's is.
func (c *a64Compiler) stringCode(v *ssa.Value, guard a64Guard) {
	if !c.reference(v, v.Args[0], c.enc.Object, guard) {
		return
	}
	c.a.Load(a64B, a64Ctx, abi.OffCharCode+c.enc.RefOffset)
	c.a.Cmp(a64C, a64B, true)
	guard.jump(arm64.NE)
	if !c.stringUnit(v, v.Args[1], v.Args[2], guard) {
		return
	}
	c.a.Scvtf(a64F0, a64A, true)
	c.setF(v, a64F0)
}

// stringChar is s[i] (ssa.OpStringChar), as amd64's: past the string's
// end, the undefined cell where Go says so (ssa.GoStringAbsent).
func (c *a64Compiler) stringChar(v *ssa.Value, guard a64Guard) {
	var past a64Guard
	done := arm64.Label(0)
	if c.enc.CallGo != 0 && c.enc.UndefinedCell != 0 {
		slow, stub := c.a.NewLabel(), c.stubLabel(v.State, exitKind(v.Aux))
		done = c.a.NewLabel()
		past = a64GuardLabel(c, slow)
		c.cold = append(c.cold, func() {
			// The string in C, its index in A (stringUnitAt).
			c.a.Bind(slow)
			size := int32(unsafe.Sizeof(abi.GoArg{}))
			c.a.Store(a64Ctx, abi.OffGoArgs+c.enc.RefOffset, a64C)
			c.a.Scvtf(a64F0, a64A, true)
			c.a.FMovFromF(a64A, a64F0)
			c.a.Store(a64Ctx, abi.OffGoArgs+size+c.enc.NumOffset, a64A)
			c.goSaving(uint64(ssa.GoStringAbsent), &stub)
			c.movImm(a64A, c.enc.UndefinedCell)
			c.a.B(done)
		})
	}
	if !c.reference(v, v.Args[0], c.enc.String, guard) {
		return
	}
	c.stringUnitPast(v.Args[1], guard, past, c.stubLabel(v.State, exitKind(v.Aux)))
	c.charAddr(v, guard)
	if done != 0 {
		c.a.Bind(done)
	}
	c.setG(v, a64A)
}

// codeChar is String.fromCharCode(c) (ssa.OpCodeChar), as amd64's.
func (c *a64Compiler) codeChar(v *ssa.Value, guard a64Guard) {
	c.index(v.Args[0], guard)
	c.charCell(v, guard)
}

// stringKeyUnit is amd64's: the index in A, the unit's address in B and
// the unit in A, the units left from it in C where units. It uses A, B, C,
// F0 and F1.
func (c *a64Compiler) stringKeyUnit(v *ssa.Value, rel, units bool, none arm64.Label) {
	fail := a64GuardLabel(c, none)
	s, key := v.Args[0], v.Args[1]
	if s.Shadow == nil && c.origin.At(s) < 0 {
		c.a.B(none)
		return
	}
	if key.Op == ssa.OpBoxF64 {
		if k := c.fpr(key.Args[0], a64F0); k != a64F0 {
			c.a.FMov(a64F0, k)
		}
	} else {
		var w arm64.Reg
		if remat(key) {
			c.materialize(key, a64A)
			w = a64A
		} else {
			w = c.gpr(key, a64A)
		}
		c.movImm(a64B, abi.NumberLimit)
		c.a.Cmp(w, a64B, true)
		fail.jump(arm64.HS)
		c.a.FMovToF(a64F0, w)
	}
	c.referenceTo(v, s, c.enc.String, fail, none)
	c.a.Load(a64B, a64C, c.enc.StringLeft)
	c.a.Cbnz(a64B, none, true)
	// An integer: FCVTZS and back, the key itself; unordered is not equal.
	c.a.Fcvtzs(a64A, a64F0)
	c.a.Scvtf(a64F1, a64A, true)
	c.a.FCmp(a64F1, a64F0)
	fail.jump(arm64.NE)
	c.a.Load(a64B, a64C, c.enc.StringLength)
	if rel {
		nonNegative := c.a.NewLabel()
		c.a.CmpImm(a64A, 0, true)
		c.a.BCond(arm64.GE, nonNegative)
		c.a.Op(arm64.Add, a64A, a64A, a64B, true)
		c.a.Bind(nonNegative)
	}
	c.a.Cmp(a64A, a64B, true)
	fail.jump(arm64.HS)
	ascii, done := c.a.NewLabel(), c.a.NewLabel()
	c.a.LoadU8(a64B, a64C, c.enc.StringASCII)
	c.a.Cbnz(a64B, ascii, false)
	c.a.Load(a64B, a64C, c.enc.StringU16)
	c.a.Cbz(a64B, none, true)
	if units {
		c.a.Load(a64C, a64C, c.enc.StringLength)
		c.a.Op(arm64.Sub, a64C, a64C, a64A, true)
	}
	c.a.Op(arm64.Add, a64B, a64B, a64A, true)
	c.a.Op(arm64.Add, a64B, a64B, a64A, true)
	c.a.LoadU16(a64A, a64B, 0)
	c.a.B(done)
	c.a.Bind(ascii)
	c.a.Load(a64B, a64C, c.enc.StringData)
	c.a.Op(arm64.Add, a64B, a64B, a64A, true)
	c.a.LoadU8(a64A, a64B, 0)
	if units {
		c.movImm(a64C, 1)
	}
	c.a.Bind(done)
}

// stringCodePointOr is ssa.OpStringCodePointOr, as amd64's. It uses A, B,
// C, F0 and F1.
func (c *a64Compiler) stringCodePointOr(v *ssa.Value) {
	none, unit, done := c.a.NewLabel(), c.a.NewLabel(), c.a.NewLabel()
	c.stringKeyUnit(v, false, true, none)
	c.a.CmpImm(a64C, 1, true)
	c.a.BCond(arm64.LS, unit)
	c.a.AddImm(a64C, a64A, -0xD800, false)
	c.a.CmpImm(a64C, 0x400, false)
	c.a.BCond(arm64.HS, unit)
	c.a.LoadU16(a64B, a64B, 2)
	c.a.AddImm(a64B, a64B, -0xDC00, false)
	c.a.CmpImm(a64B, 0x400, false)
	c.a.BCond(arm64.HS, unit)
	c.a.ShiftImm(arm64.Lsl, a64C, a64C, 10, false)
	c.a.Op(arm64.Add, a64C, a64C, a64B, false)
	c.a.AddImm(a64A, a64C, 0x10000, false)
	c.a.Bind(unit)
	c.a.Scvtf(a64F0, a64A, true)
	c.a.B(done)
	c.a.Bind(none)
	c.movImm(a64A, math.Float64bits(-1))
	c.a.FMovToF(a64F0, a64A)
	c.a.Bind(done)
	c.setF(v, a64F0)
}

// stringAtOr is ssa.OpStringAtOr, as amd64's. It uses A, B, C, F0 and F1.
func (c *a64Compiler) stringAtOr(v *ssa.Value) {
	none, done := c.a.NewLabel(), c.a.NewLabel()
	c.stringKeyUnit(v, true, false, none)
	c.a.CmpImm(a64A, 256, true)
	c.a.BCond(arm64.HS, none)
	c.a.Load(a64B, a64Ctx, abi.OffCharStrings)
	c.a.Cbz(a64B, none, true)
	c.a.AddLsl(a64A, a64B, a64A, 4)
	c.a.B(done)
	c.a.Bind(none)
	c.movImm(a64A, 0)
	c.a.Bind(done)
	c.setG(v, a64A)
}

// cursorCell is ssa.OpCursorCell, as amd64's. It uses A, B, C and D.
func (c *a64Compiler) cursorCell(v *ssa.Value) {
	x := v.Args[0]
	none, done := c.a.NewLabel(), c.a.NewLabel()
	c.pointerWord(x, c.gpr(x, a64A))
	if v.Index == 1 {
		c.a.AddImm(a64A, a64B, int64(c.enc.IterState+c.enc.IterNext), true)
		c.setG(v, a64A)
		return
	}
	c.a.Cbz(a64B, none, true)
	c.objectPart(a64D, a64B, a64C, c.enc.ClassCursor, c.enc.IterState, a64GuardLabel(c, none))
	c.a.LoadU8(a64C, a64D, c.enc.IterDone)
	c.a.Cbnz(a64C, none, false)
	c.a.LoadU8(a64C, a64D, c.enc.IterForIn)
	c.a.Cbnz(a64C, none, false)
	c.a.Load(a64C, a64D, c.enc.IterArr)
	c.a.Cbnz(a64C, none, true)
	c.a.Load(a64C, a64D, c.enc.IterNext+c.enc.RefOffset)
	c.movImm(a64B, v.Const.Bits)
	c.a.Cmp(a64C, a64B, true)
	c.a.BCond(arm64.NE, none)
	c.a.AddImm(a64A, a64D, int64(c.enc.IterIter), true)
	c.a.B(done)
	c.a.Bind(none)
	c.movImm(a64A, 0)
	c.a.Bind(done)
	c.setG(v, a64A)
}

// typeofCell is ssa.OpTypeofCell, as amd64's. It uses A, B and C.
func (c *a64Compiler) typeofCell(v *ssa.Value) {
	a := v.Args[0]
	none, done := c.a.NewLabel(), c.a.NewLabel()
	var at [8]arm64.Label
	for i := range at {
		at[i] = c.a.NewLabel()
	}
	w := c.gpr(a, a64B)
	cmp := func(word uint64) {
		c.movImm(a64A, word)
		c.a.Cmp(w, a64A, true)
	}
	cmp(abi.NumberLimit)
	c.a.BCond(arm64.LO, at[ssa.TypeofNumber])
	for _, k := range []struct {
		word uint64
		i    int
	}{{c.enc.String, ssa.TypeofString}, {c.enc.Undefined, ssa.TypeofUndefined}, {c.enc.Null, ssa.TypeofObject},
		{c.enc.True, ssa.TypeofBoolean}, {c.enc.False, ssa.TypeofBoolean}, {c.enc.Symbol, ssa.TypeofSymbol}} {
		cmp(k.word)
		c.a.BCond(arm64.EQ, at[k.i])
	}
	object := c.a.NewLabel()
	cmp(c.enc.Object)
	c.a.BCond(arm64.EQ, object)
	cmp(c.enc.HeapBigTag)
	c.a.BCond(arm64.HS, at[ssa.TypeofBigInt])
	c.a.B(none)
	c.a.Bind(object)
	if a.Shadow == nil && c.origin.At(a) < 0 {
		// A primitive, never an object's reference.
		c.a.B(none)
	} else {
		fail := a64GuardLabel(c, none)
		c.sourceRef(a, fail)
		c.a.Cbz(a64C, none, true)
		c.a.LoadU8(a64A, a64C, c.enc.ObjectFlags)
		c.movImm(a64B, uint64(c.enc.FlagHTMLDDA))
		c.a.Tst(a64A, a64B, false)
		c.a.BCond(arm64.NE, at[ssa.TypeofUndefined])
		c.a.LoadU8(a64A, a64C, c.enc.ObjectClass)
		c.a.CmpImm(a64A, int64(c.enc.ClassFunction), false)
		c.a.BCond(arm64.EQ, at[ssa.TypeofFunction])
		c.a.CmpImm(a64A, int64(c.enc.ClassProxy), false)
		c.a.BCond(arm64.EQ, none)
		c.a.B(at[ssa.TypeofObject])
	}
	for i, l := range at {
		c.a.Bind(l)
		c.movImm(a64A, v.Const.Bits+uint64(i)*uint64(c.enc.ValueSize))
		c.a.B(done)
	}
	c.a.Bind(none)
	c.movImm(a64A, 0)
	c.a.Bind(done)
	c.setG(v, a64A)
}

// numberIs is ssa.OpNumberIs, as amd64's. It uses A, B, F0 and F1.
func (c *a64Compiler) numberIs(v *ssa.Value) {
	x := v.Args[0]
	no, done := c.a.NewLabel(), c.a.NewLabel()
	if x.Op == ssa.OpBoxF64 {
		if k := c.fpr(x.Args[0], a64F0); k != a64F0 {
			c.a.FMov(a64F0, k)
		}
		c.a.FMovFromF(a64A, a64F0)
	} else {
		if remat(x) {
			c.materialize(x, a64A)
		} else if w := c.gpr(x, a64A); w != a64A {
			c.a.MovRR(a64A, w)
		}
		c.movImm(a64B, abi.NumberLimit)
		c.a.Cmp(a64A, a64B, true)
		c.a.BCond(arm64.HS, no)
		c.a.FMovToF(a64F0, a64A)
	}
	switch v.Aux {
	case ssa.NumberIsNaN:
		c.a.FCmp(a64F0, a64F0)
		c.a.Cset(a64A, arm64.VS)
	case ssa.NumberIsFinite:
		c.a.FArith(arm64.FSub, a64F1, a64F0, a64F0)
		c.a.FCmp(a64F1, a64F1)
		c.a.Cset(a64A, arm64.VC)
	default:
		c.a.ShiftImm(arm64.Lsl, a64A, a64A, 1, true)
		c.a.ShiftImm(arm64.Lsr, a64A, a64A, 1, true)
		if v.Aux == ssa.NumberIsSafeInteger {
			c.movImm(a64B, math.Float64bits(1<<53-1))
			c.a.Cmp(a64A, a64B, true)
			c.a.BCond(arm64.HI, no)
		} else {
			small := c.a.NewLabel()
			c.movImm(a64B, math.Float64bits(1<<53))
			c.a.Cmp(a64A, a64B, true)
			c.a.BCond(arm64.LO, small)
			c.movImm(a64B, math.Float64bits(math.Inf(1)))
			c.a.Cmp(a64A, a64B, true)
			c.a.Cset(a64A, arm64.LO)
			c.a.B(done)
			c.a.Bind(small)
		}
		c.a.Fcvtzs(a64A, a64F0)
		c.a.Scvtf(a64F1, a64A, true)
		c.a.FCmp(a64F1, a64F0)
		c.a.Cset(a64A, arm64.EQ)
	}
	c.a.B(done)
	c.a.Bind(no)
	c.movImm(a64A, 0)
	c.a.Bind(done)
	c.setG(v, a64A)
}

// codeCharOr is ssa.OpCodeCharOr, as amd64's. It uses A, B, F0 and F1.
func (c *a64Compiler) codeCharOr(v *ssa.Value) {
	none, done := c.a.NewLabel(), c.a.NewLabel()
	fail := a64GuardLabel(c, none)
	c.index(v.Args[0], fail)
	c.a.CmpImm(a64A, 256, true)
	fail.jump(arm64.HS)
	c.a.Load(a64B, a64Ctx, abi.OffCharStrings)
	c.a.Cbz(a64B, none, true)
	c.a.AddLsl(a64A, a64B, a64A, 4)
	c.a.B(done)
	c.a.Bind(none)
	c.movImm(a64A, 0)
	c.a.Bind(done)
	c.setG(v, a64A)
}

// iterRelease is amd64's, of the state at a64B. It uses a64C.
func (c *a64Compiler) iterRelease() {
	if c.enc.IterState != 0 {
		c.movImm(a64C, 1)
		c.a.Store8(a64B, c.enc.IterDone+abi.IterReleased, a64C)
	}
}

// oddball is ssa.OpOddballF64, as amd64's. It uses A.
func (c *a64Compiler) oddball(v *ssa.Value, fail a64Guard) {
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
		// As amd64's: a string of a few decimal digits converted natively
		// (digits), anything else by Go (ssa.GoNumber).
		slow := c.a.NewLabel()
		c.a.BCond(arm64.NE, slow)
		c.cold = append(c.cold, func() {
			c.a.Bind(slow)
			other := c.a.NewLabel()
			c.digits(v.Args[0], d, other)
			c.a.B(done)
			c.a.Bind(other)
			stub := c.stubLabel(v.State, exitKind(v.Aux))
			c.goArg(abi.OffGoArgs, v.Args[0])
			c.goSaving(uint64(ssa.GoNumber), &stub)
			c.a.LoadF(d, a64Ctx, abi.OffGoArgs+c.enc.NumOffset)
			c.a.B(done)
		})
	} else {
		fail.jump(arm64.NE)
	}
	c.a.Bind(zero)
	c.movImm(a64A, 0)
	c.a.FMovToF(d, a64A)
	c.a.Bind(done)
	c.setF(v, d)
}

// digits is amd64's, its number in d, the end of the string's bytes kept
// in D. It uses A to D and F1.
func (c *a64Compiler) digits(x *ssa.Value, d arm64.FReg, other arm64.Label) {
	if x.Shadow == nil && c.origin.At(x) < 0 {
		c.a.B(other)
		return
	}
	dot := abi.OffGoArgs + int32(unsafe.Sizeof(abi.GoArg{}))
	w := c.gpr(x, a64A)
	c.movImm(a64B, c.enc.String)
	c.a.Cmp(w, a64B, true)
	c.a.BCond(arm64.NE, other)
	c.sourceRef(x, a64GuardLabel(c, other))
	c.a.Cbz(a64C, other, true)
	c.a.Load(a64A, a64C, c.enc.StringLeft)
	c.a.Cbnz(a64A, other, true)
	c.a.LoadU8(a64A, a64C, c.enc.StringASCII)
	c.a.Cbz(a64A, other, false)
	c.a.Load(a64A, a64C, c.enc.StringLength)
	c.a.AddImm(a64A, a64A, -1, true)
	c.a.CmpImm(a64A, maxDigits-1, true)
	c.a.BCond(arm64.HI, other)
	c.a.Load(a64B, a64C, c.enc.StringData)
	c.a.Op(arm64.Add, a64D, a64B, a64A, true)
	c.a.AddImm(a64D, a64D, 1, true)
	c.movImm(a64A, 0)
	c.a.Store(a64Ctx, dot, a64A)
	loop, next, point := c.a.NewLabel(), c.a.NewLabel(), c.a.NewLabel()
	c.a.Bind(loop)
	c.a.LoadU8(a64C, a64B, 0)
	c.a.AddImm(a64C, a64C, -'0', false)
	c.a.CmpImm(a64C, 9, false)
	c.a.BCond(arm64.HI, point)
	// m = m*10 + d: m + m<<2, then d + that<<1.
	c.a.AddLsl(a64A, a64A, a64A, 2)
	c.a.AddLsl(a64A, a64C, a64A, 1)
	c.a.Bind(next)
	c.a.AddImm(a64B, a64B, 1, true)
	c.a.Cmp(a64B, a64D, true)
	c.a.BCond(arm64.LO, loop)
	c.a.Scvtf(d, a64A, true)
	end := c.a.NewLabel()
	c.a.Load(a64C, a64Ctx, dot)
	c.a.Cbz(a64C, end, true)
	c.a.Op(arm64.Subs, a64B, a64B, a64C, true)
	c.a.BCond(arm64.EQ, other)
	c.movImm(a64C, 1)
	tens := c.a.NewLabel()
	c.a.Bind(tens)
	c.a.AddLsl(a64C, a64C, a64C, 2)
	c.a.AddLsl(a64C, a64C, a64C, 0)
	c.a.SubsImm(a64B, a64B, 1, true)
	c.a.BCond(arm64.NE, tens)
	c.a.Scvtf(a64F1, a64C, true)
	c.a.FArith(arm64.FDiv, d, d, a64F1)
	c.a.B(end)
	c.a.Bind(point)
	c.a.CmpImm(a64C, '.'-'0', false)
	c.a.BCond(arm64.NE, other)
	c.a.Load(a64C, a64Ctx, dot)
	c.a.Cbnz(a64C, other, true)
	c.a.AddImm(a64C, a64B, 1, true)
	c.a.Store(a64Ctx, dot, a64C)
	c.a.B(next)
	c.a.Bind(end)
}

// forInDone is ssa.OpForInDone, as amd64's. It uses A to C.
func (c *a64Compiler) forInDone(v *ssa.Value) {
	if c.enc.IterState == 0 {
		return
	}
	x := v.Args[0]
	var w arm64.Reg
	if remat(x) {
		c.materialize(x, a64A)
		w = a64A
	} else {
		w = c.gpr(x, a64A)
	}
	done := c.a.NewLabel()
	c.movImm(a64B, c.enc.Object)
	c.a.Cmp(w, a64B, true)
	c.a.BCond(arm64.NE, done)
	c.pointerWord(x, w)
	c.a.Cbz(a64B, done, true)
	c.objectPart(a64B, a64B, a64C, c.enc.ClassCursor, c.enc.IterState, a64GuardLabel(c, done))
	c.a.LoadU8(a64C, a64B, c.enc.IterForIn)
	c.a.Cbz(a64C, done, false)
	c.movImm(a64A, 1)
	c.a.Store8(a64B, c.enc.IterDone, a64A)
	c.a.Bind(done)
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
	c.a.LoadU8(a64C, a64B, c.enc.StringBuilding)
	c.a.Cbz(a64C, done, true)
	c.movImm(a64C, 0)
	c.a.Store8(a64B, c.enc.StringBuilding, a64C)
	c.a.Bind(done)
}

// charCell is the cell of the runtime's string of the code unit in A,
// below 256, as amd64's.
func (c *a64Compiler) charCell(v *ssa.Value, guard a64Guard) {
	c.charAddr(v, guard)
	c.setG(v, a64A)
}

// charAddr is charCell's cell, in A.
func (c *a64Compiler) charAddr(v *ssa.Value, guard a64Guard) {
	c.a.CmpImm(a64A, 256, true)
	guard.jump(arm64.HS)
	c.a.Load(a64B, a64Ctx, abi.OffCharStrings)
	c.a.Cbz(a64B, c.stubLabel(v.State, exitKind(v.Aux)), true)
	c.a.AddLsl(a64A, a64B, a64A, 4)
}

// stringUnit reads the code unit at index key of str, a flat string, into
// A, as amd64's, and reports whether the code goes on. It uses B and C.
func (c *a64Compiler) stringUnit(v, str, key *ssa.Value, guard a64Guard) bool {
	if !c.reference(v, str, c.enc.String, guard) {
		return false
	}
	c.stringUnitAt(key, guard, c.stubLabel(v.State, exitKind(v.Aux)))
	return true
}

// stringUnitAt is stringUnit once the string is in C, failing at fail, as
// guard does.
func (c *a64Compiler) stringUnitAt(key *ssa.Value, guard a64Guard, fail arm64.Label) {
	c.stringUnitPast(key, guard, a64Guard{}, fail)
}

// stringUnitPast is stringUnitAt, an index past the string's end going to
// past, if not nil, the string in C and the index in A.
func (c *a64Compiler) stringUnitPast(key *ssa.Value, guard, past a64Guard, fail arm64.Label) {
	if past.c == nil {
		past = guard
	}
	c.a.Load(a64B, a64C, c.enc.StringLeft)
	c.a.Cbnz(a64B, fail, true)
	c.index(key, guard)
	c.a.Load(a64B, a64C, c.enc.StringLength)
	c.a.Cmp(a64A, a64B, true)
	past.jump(arm64.HS)
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
	fail := a64GuardLabel(c, none)
	if v.Aux != ssa.SubstrAt {
		c.movImm(a64D, math.Float64bits(1))
		c.a.FMovToF(a64F2, a64D)
		if v.Aux == ssa.SubstrLength {
			c.a.FCmp(c.fpr(v.Args[2], a64F1), a64F2)
		} else {
			c.a.FArith(arm64.FAdd, a64F2, a64F2, c.fpr(v.Args[1], a64F0))
			c.a.FCmp(a64F2, c.fpr(v.Args[2], a64F1))
		}
		fail.jump(arm64.NE)
	}
	if s := v.Args[0]; s.Shadow == nil && c.origin.At(s) < 0 {
		c.a.B(none)
	} else {
		c.referenceTo(v, s, c.enc.String, fail, none)
		c.stringUnitAt(v.Args[1], fail, none)
	}
	c.a.CmpImm(a64A, 256, true)
	fail.jump(arm64.HS)
	c.a.Load(a64B, a64Ctx, abi.OffCharStrings)
	c.a.Cbz(a64B, none, true)
	c.a.AddLsl(a64A, a64B, a64A, 4)
	c.a.B(done)
	c.a.Bind(none)
	c.movImm(a64A, 0)
	c.a.Bind(done)
	c.setG(v, a64A)
}

// index converts an element's key to an index in A, failing unless it is an
// integer in [0, 2**32). FCVTZS saturates, so a key of 2**63 or more gives
// 2**63-1, whose top half is not clear. It uses B and F1.
func (c *a64Compiler) index(key *ssa.Value, guard a64Guard) {
	if key.Type == ssa.Int32 {
		// As amd64's: an int32 sign-extended, a negative one past any
		// length (element's unsigned comparison).
		c.a.Sxtw(a64A, c.gpr(key, a64A))
		return
	}
	if i, ok := constIndex(key); ok {
		// As amd64's: a constant index, nothing to check.
		c.movImm(a64A, i)
		return
	}
	k := c.fpr(key, a64F0)
	c.a.Fcvtzs(a64A, k)
	c.a.Scvtf(a64F1, a64A, true)
	c.a.FCmp(a64F1, k)
	guard.jump(arm64.NE)
	c.a.ShiftImm(arm64.Lsr, a64B, a64A, 32, true)
	c.a.CmpImm(a64B, 0, true)
	guard.jump(arm64.NE)
}

// keyBits is amd64's: the key's double's bits in A. It uses F0.
func (c *a64Compiler) keyBits(key *ssa.Value) {
	if key.Type == ssa.Int32 {
		c.a.Scvtf(a64F0, c.gpr(key, a64A), false)
		c.a.FMovFromF(a64A, a64F0)
		return
	}
	c.a.FMovFromF(a64A, c.fpr(key, a64F0))
}

// element turns the index in A into the address of an array's element in
// A, and its number word in B, as amd64's does. It uses C.
func (c *a64Compiler) element(array *ssa.Value, guard a64Guard) {
	p := c.gpr(array, a64C)
	c.a.Load(a64B, p, c.enc.ObjectElems+8)
	c.a.Cmp(a64A, a64B, true)
	guard.jump(arm64.HS)
	c.a.Load(a64B, p, c.enc.ObjectElems)
	c.a.AddLsl(a64A, a64B, a64A, 4)
	c.a.Load(a64B, a64A, c.enc.NumOffset)
	c.movImm(a64C, abi.NumberLimit)
	c.a.Cmp(a64B, a64C, true)
	guard.jump(arm64.HS)
}

// indexedOf finds an array or a typed array (OpIndexedOf), as amd64's.
func (c *a64Compiler) indexedOf(v *ssa.Value, guard a64Guard) {
	if !c.objectOf(v, guard) {
		return
	}
	ok := c.a.NewLabel()
	c.a.LoadU8(a64B, a64C, c.enc.ObjectClass)
	c.a.CmpImm(a64B, int64(c.enc.ClassArray), false)
	c.a.BCond(arm64.EQ, ok)
	c.a.CmpImm(a64B, int64(c.enc.ClassTypedArray), false)
	guard.jump(arm64.NE)
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

// arrayClass jumps to array if the object of an element read or write of
// a number compiled for a typed array's element type, from OpIndexedOf,
// is an Array, as amd64's. It uses B.
func (c *a64Compiler) arrayClass(of *ssa.Value, array arm64.Label) {
	c.a.LoadU8(a64B, c.gpr(of, a64B), c.enc.ObjectClass)
	c.a.CmpImm(a64B, int64(c.enc.ClassArray), false)
	c.a.BCond(arm64.EQ, array)
}

// typedClass fails unless the object array, from OpIndexedOf, is a typed
// array, as amd64's. It uses B.
func (c *a64Compiler) typedClass(array *ssa.Value, fail a64Guard) {
	c.a.LoadU8(a64B, c.gpr(array, a64B), c.enc.ObjectClass)
	c.a.CmpImm(a64B, int64(c.enc.ClassTypedArray), false)
	fail.jump(arm64.NE)
}

// typedElement finds a typed array's element, as amd64's: its address in
// A, the first element's in B, the view's element type in C -- for a
// store, with the byte after it. It uses A, B, C, D and F1. Once Go has
// made the view's copy anew, it starts again at again, if not nil, as
// amd64's.
func (c *a64Compiler) typedElement(array, key *ssa.Value, store bool, fail a64Guard, again *arm64.Label) {
	if c.enc.TypedEpoch == 0 {
		// An encoding with no typed arrays.
		if to, ok := fail.label(); ok {
			c.a.B(to)
		}
		return
	}
	stale := c.a.NewLabel()
	var retry arm64.Label
	if again != nil {
		retry = *again
	} else {
		retry = c.a.NewLabel()
		c.a.Bind(retry)
	}
	c.a.Load(a64C, c.gpr(array, a64C), c.enc.ObjectExt+c.enc.ObjectData+8)
	c.typedView(a64C, a64B, a64D, stale, fail)
	// The key an integer -- FCMP of NaN is unordered, not equal -- below
	// the length, compared unsigned.
	if i, ok := constIndex(key); ok {
		// A constant index, nothing to check (index's).
		c.movImm(a64A, i)
	} else if key.Type == ssa.Int32 {
		c.a.Sxtw(a64A, c.gpr(key, a64A))
	} else {
		k := c.fpr(key, a64F0)
		c.a.Fcvtzs(a64A, k)
		c.a.Scvtf(a64F1, a64A, true)
		c.a.FCmp(a64F1, k)
		fail.jump(arm64.NE)
	}
	c.a.Load(a64D, a64C, c.enc.TypedLength)
	c.a.Cmp(a64A, a64D, true)
	fail.jump(arm64.HS)
	c.a.Load(a64B, a64C, c.enc.TypedElems)
	if store {
		c.a.LoadU16(a64C, a64C, c.enc.TypedKind)
	} else {
		c.a.LoadU8(a64C, a64C, c.enc.TypedKind)
	}
	c.typedStale(stale, retry, fail, func() {
		c.a.Store(a64Ctx, abi.OffGoArgs+c.enc.RefOffset, c.gpr(array, a64C))
	}, nil)
}

// typedView jumps to stale unless the view whose pointer is in view has
// its copy as of the context's epoch, as amd64's. It uses t and u.
func (c *a64Compiler) typedView(view, t, u arm64.Reg, stale arm64.Label, fail a64Guard) {
	c.a.Load(t, a64Ctx, abi.OffTypedEpoch)
	c.a.Load(t, t, 0)
	c.a.Load(u, view, c.enc.TypedEpoch)
	c.a.Cmp(t, u, true)
	if c.enc.CallGo == 0 {
		fail.jump(arm64.NE)
		return
	}
	c.a.BCond(arm64.NE, stale)
}

// typedStale is amd64's: out of line, Go makes the view's copy anew, and
// the code goes back to retry.
func (c *a64Compiler) typedStale(stale, retry arm64.Label, fail a64Guard, arg, after func()) {
	if c.enc.CallGo == 0 {
		return
	}
	budget := c.budget
	c.cold = append(c.cold, func() {
		saved := c.budget
		c.budget = budget
		c.a.Bind(stale)
		arg()
		if to, ok := fail.label(); ok {
			c.goSaving(uint64(ssa.GoTypedView), &to)
		} else {
			c.goSaving(uint64(ssa.GoTypedView), nil)
		}
		if after != nil {
			after()
		}
		c.a.B(retry)
		c.budget = saved
	})
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

// typedAt turns the index in A into the address of the element of type
// kind whose view's first element is at B, as amd64's.
func (c *a64Compiler) typedAt(kind int32) {
	if shift := typedShift(kind); shift != 0 {
		c.a.ShiftImm(arm64.Lsl, a64A, a64A, shift, true)
	}
	c.a.Op(arm64.Add, a64A, a64A, a64B, true)
}

// typedSeen has v learn the element type in C where its site's cell has
// no bit of it yet, as amd64's, starting again at again once Go has set
// it. It keeps A, B and C, and uses D.
func (c *a64Compiler) typedSeen(v *ssa.Value, again arm64.Label) {
	cell := ssa.TypedCellOf(v)
	if cell == 0 || c.enc.CallGo == 0 {
		return
	}
	known := c.a.NewLabel()
	c.movImm(a64D, uint64(cell))
	c.a.Load(a64D, a64D, c.enc.NumOffset)
	c.a.ShiftReg(arm64.Lsr, a64D, a64D, a64C, true)
	c.a.ShiftImm(arm64.Lsl, a64D, a64D, 63, true)
	c.a.Cbnz(a64D, known, true)
	size := int32(unsafe.Sizeof(abi.GoArg{}))
	c.a.Store(a64Ctx, abi.OffGoArgs+c.enc.RefOffset, c.gpr(v.Args[0], a64C))
	c.movImm(a64A, uint64(cell))
	c.a.Store(a64Ctx, abi.OffGoArgs+size+c.enc.RefOffset, a64A)
	stub := c.stubLabel(v.State, exitKind(v.Aux))
	c.goSaving(uint64(ssa.GoTypedSeen), &stub)
	c.a.B(again)
	c.a.Bind(known)
}

// elemStore stores any value in an Array's element, as amd64's, the
// value's pointer word in D while the element's cell is found. It uses A,
// B, C, D, F0 and F1.
func (c *a64Compiler) elemStore(v *ssa.Value) {
	x := v.Args[2]
	stub := c.stubLabel(v.State, exitKind(v.Aux))
	slow, done := c.a.NewLabel(), c.a.NewLabel()
	checked := len(v.Args) > 3
	if c.enc.CallGo == 0 || checked && c.enc.ValueSize != 16 {
		slow = stub
	}
	fail := a64GuardLabel(c, slow)
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
		c.storeCell(v.Args[0], v.Args[3:], stub, fail, found)
	}
	c.a.Bind(found)
	c.cellChecks(v.Args[3:], stub)
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
			again := c.a.NewLabel()
			c.a.Bind(again)
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
			c.typedWrite(v, done, slow, again, func() arm64.FReg { return a64F2 }, false)
		})
	}
	c.cold = append(c.cold, func() {
		c.a.Bind(viaGo)
		c.valueStore()
		c.a.B(done)
	})
	if slow == stub {
		return
	}
	c.cold = append(c.cold, func() {
		// Any other store Go makes if it can (ssa.GoElemStore), as amd64's.
		c.a.Bind(slow)
		c.storageFree(v.Args[0], v.Args[3:], stub)
		size := int32(unsafe.Sizeof(abi.GoArg{}))
		c.a.Store(a64Ctx, abi.OffGoArgs+c.enc.RefOffset, c.gpr(v.Args[0], a64C))
		c.keyBits(v.Args[1])
		c.a.Store(a64Ctx, abi.OffGoArgs+size+c.enc.NumOffset, a64A)
		words()
		c.a.Store(a64Ctx, abi.OffGoArgs+2*size+c.enc.RefOffset, a64B)
		c.a.Store(a64Ctx, abi.OffGoArgs+2*size+c.enc.NumOffset, c.gpr(x, a64A))
		c.movImm(a64A, uint64(ssa.TypedCellOf(v)))
		c.a.Store(a64Ctx, abi.OffGoArgs+3*size+c.enc.RefOffset, a64A)
		c.goSaving(uint64(ssa.GoElemStore)|uint64(v.Index&ssa.StoreFresh)<<abi.GoOpIndexShift, &stub)
		c.a.B(done)
	})
}

// typedRead reads a typed array's element into F0, as amd64's: inline
// for the one element type its site met, if hot.
func (c *a64Compiler) typedRead(v *ssa.Value, done arm64.Label, hot bool) {
	stub := c.stubLabel(v.State, exitKind(v.Aux))
	slow := stub
	if c.enc.CallGo != 0 {
		slow = c.a.NewLabel()
	}
	again := c.a.NewLabel()
	c.a.Bind(again)
	c.typedElement(v.Args[0], v.Args[1], false, a64GuardLabel(c, slow), &again)
	read := func(kind int32) {
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
			return
		case abi.ElemFloat64:
			c.a.LoadF(a64F0, a64A, 0)
			return
		}
		c.a.Scvtf(a64F0, a64A, true)
	}
	generic := func() {
		c.typedSeen(v, again)
		c.typedCases(slow, func(kind int32) {
			read(kind)
			c.a.B(done)
		})
		if c.enc.CallGo == 0 {
			return
		}
		c.a.Bind(slow)
		size := int32(unsafe.Sizeof(abi.GoArg{}))
		c.a.Store(a64Ctx, abi.OffGoArgs+c.enc.RefOffset, c.gpr(v.Args[0], a64C))
		c.keyBits(v.Args[1])
		c.a.Store(a64Ctx, abi.OffGoArgs+size+c.enc.NumOffset, a64A)
		c.goSaving(uint64(ssa.GoTypedRead), &stub)
		c.a.LoadF(a64F0, a64Ctx, abi.OffGoArgs+c.enc.NumOffset)
		c.a.B(done)
	}
	kind := ssa.TypedKindOf(v)
	if kind < 0 {
		generic()
		return
	}
	other := c.a.NewLabel()
	c.a.CmpImm(a64C, int64(kind), false)
	c.a.BCond(arm64.NE, other)
	c.typedAt(kind)
	read(kind)
	if hot {
		c.cold = append(c.cold, func() {
			c.a.Bind(other)
			generic()
		})
		return
	}
	c.a.B(done)
	c.a.Bind(other)
	generic()
}

// typedWrite stores a number in a typed array's element, as amd64's.
// The value is in the register value gives, a double; again is where the
// store may start again.
func (c *a64Compiler) typedWrite(v *ssa.Value, done, slow, again arm64.Label, value func() arm64.FReg, hot bool) {
	fail := a64GuardLabel(c, slow)
	c.typedElement(v.Args[0], v.Args[1], true, fail, &again)
	write := func(kind int32) {
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
			fail.jump(arm64.EQ)
			c.movImm(a64C, 1<<63)
			c.a.Cmp(a64B, a64C, true)
			fail.jump(arm64.EQ)
			switch kind {
			case abi.ElemInt8, abi.ElemUint8:
				c.a.Store8(a64A, 0, a64B)
			case abi.ElemInt16, abi.ElemUint16:
				c.a.Store16(a64A, 0, a64B)
			default:
				c.a.Store32(a64A, 0, a64B)
			}
		}
	}
	generic := func() {
		c.typedSeen(v, again)
		c.typedCases(slow, func(kind int32) {
			write(kind)
			c.a.B(done)
		})
	}
	kind := ssa.TypedKindOf(v)
	if kind < 0 {
		generic()
		return
	}
	other := c.a.NewLabel()
	c.a.CmpImm(a64C, int64(kind), false)
	c.a.BCond(arm64.NE, other)
	c.typedAt(kind)
	write(kind)
	if hot {
		c.cold = append(c.cold, func() {
			c.a.Bind(other)
			generic()
		})
		return
	}
	c.a.B(done)
	c.a.Bind(other)
	generic()
}

// elementCell turns the index in A into the address of an array's element
// there, in A, failing unless it is within the dense elements and not a
// hole, as amd64's does. It uses B and C.
func (c *a64Compiler) elementCell(array *ssa.Value, guard a64Guard) {
	p := c.gpr(array, a64C)
	c.a.Load(a64B, p, c.enc.ObjectElems+8)
	c.a.Cmp(a64A, a64B, true)
	guard.jump(arm64.HS)
	c.a.Load(a64B, p, c.enc.ObjectElems)
	c.a.AddLsl(a64A, a64B, a64A, 4)
	c.a.Load(a64B, a64A, c.enc.NumOffset)
	c.movImm(a64C, c.enc.Uninitialized)
	c.a.Cmp(a64B, a64C, true)
	guard.jump(arm64.EQ)
}

// cellChecks is amd64's, of the cell in A: only a pointer word there is
// lost. It keeps A, and uses B.
func (c *a64Compiler) cellChecks(checks []*ssa.Value, stub arm64.Label) {
	for _, s := range checks {
		other := c.a.NewLabel()
		c.a.Cmp(c.gpr(s, a64B), a64A, true)
		c.a.BCond(arm64.NE, other)
		c.a.Load(a64B, a64A, c.enc.RefOffset)
		c.a.Cbnz(a64B, stub, true)
		c.a.Bind(other)
	}
}

// storageFree is amd64's: a store with checks Go is to make goes to stub
// where one of them points into the array's storage, up to its capacity.
// It uses A, B and C.
func (c *a64Compiler) storageFree(array *ssa.Value, checks []*ssa.Value, stub arm64.Label) {
	if len(checks) == 0 {
		return
	}
	free := c.a.NewLabel()
	p := c.gpr(array, a64C)
	c.a.LoadU8(a64A, p, c.enc.ObjectClass)
	c.a.CmpImm(a64A, int64(c.enc.ClassArray), false)
	c.a.BCond(arm64.NE, free)
	c.a.Load(a64A, p, c.enc.ObjectElems)
	c.a.Load(a64B, p, c.enc.ObjectElems+16)
	c.a.AddLsl(a64B, a64A, a64B, 4)
	for _, s := range checks {
		next := c.a.NewLabel()
		r := c.gpr(s, a64C)
		c.a.Cmp(r, a64A, true)
		c.a.BCond(arm64.LO, next)
		c.a.Cmp(r, a64B, true)
		c.a.BCond(arm64.LO, stub)
		c.a.Bind(next)
	}
	c.a.Bind(free)
}

// storeCell is amd64's: elementCell for elemStore, a hole stored into
// where the array may take it, or a store at its length where it has room
// appended -- the checks made on the cell before the length grows
// (cellChecks) -- the cell's address in A at found. It keeps D, the
// value's pointer word.
func (c *a64Compiler) storeCell(array *ssa.Value, checks []*ssa.Value, stub arm64.Label, fail a64Guard, found arm64.Label) {
	// Elements an array literal's arrays share (FlagShared) are written
	// by none of them: Go gives the array a copy of its own first. The
	// mask goes in C, not D, which holds the value's pointer word; C is
	// the array's again below.
	c.a.LoadU8(a64B, c.gpr(array, a64C), c.enc.ObjectFlags)
	c.movImm(a64C, uint64(c.enc.FlagShared))
	c.a.Tst(a64B, a64C, false)
	fail.jump(arm64.NE)
	protos := c.f.ArrayProtos
	if protos[0].Object == 0 || c.enc.ValueSize != 16 {
		c.elementCell(array, fail)
		return
	}
	p := c.gpr(array, a64C)
	c.a.Load(a64B, p, c.enc.ObjectElems+8)
	c.a.Cmp(a64A, a64B, true)
	end := c.a.NewLabel()
	c.a.BCond(arm64.HS, end)
	c.cold = append(c.cold, func() {
		c.a.Bind(end)
		fail.jump(arm64.NE)
		c.a.Load(a64B, c.gpr(array, a64C), c.enc.ObjectElems+16)
		c.a.Cmp(a64A, a64B, true)
		fail.jump(arm64.HS)
		c.a.FMovToF(a64F1, a64A)
		c.arrayGuards(c.gpr(array, a64C), protos, fail)
		c.a.FMovFromF(a64A, a64F1)
		p := c.gpr(array, a64C)
		if len(checks) != 0 {
			// The cell first, which the checks compare, the index kept in
			// F1.
			c.a.Load(a64B, p, c.enc.ObjectElems)
			c.a.AddLsl(a64A, a64B, a64A, 4)
			c.cellChecks(checks, stub)
			c.a.FMovFromF(a64B, a64F1)
			c.a.AddImm(a64B, a64B, 1, true)
			c.a.Store(c.gpr(array, a64C), c.enc.ObjectElems+8, a64B)
			c.a.B(found)
			return
		}
		c.a.AddImm(a64B, a64A, 1, true)
		c.a.Store(p, c.enc.ObjectElems+8, a64B)
		c.a.Load(a64B, p, c.enc.ObjectElems)
		c.a.AddLsl(a64A, a64B, a64A, 4)
		c.a.B(found)
	})
	c.a.Load(a64B, p, c.enc.ObjectElems)
	c.a.AddLsl(a64A, a64B, a64A, 4)
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
func (c *a64Compiler) freshCell(array *ssa.Value, fail a64Guard) {
	p := c.gpr(array, a64C)
	c.a.Load(a64B, p, c.enc.ObjectElems+8)
	c.a.Cmp(a64A, a64B, true)
	fail.jump(arm64.NE)
	c.a.Load(a64B, p, c.enc.ObjectElems+16)
	c.a.Cmp(a64A, a64B, true)
	fail.jump(arm64.HS)
	c.a.AddImm(a64B, a64A, 1, true)
	c.a.Store(p, c.enc.ObjectElems+8, a64B)
	c.a.Load(a64B, p, c.enc.ObjectElems)
	c.a.AddLsl(a64A, a64B, a64A, 4)
}

// numberCell checks the word at A's cell is a number, into B.
func (c *a64Compiler) numberCell(guard a64Guard) {
	c.a.Load(a64B, a64A, c.enc.NumOffset)
	c.movImm(a64C, abi.NumberLimit)
	c.a.Cmp(a64B, a64C, true)
	guard.jump(arm64.HS)
}

// guardFor is a guard of v's for code that may keep it: a jump to the stub
// that exits to v's state.
func (c *a64Compiler) guardFor(v *ssa.Value) a64Guard { return a64Guard{c: c, v: v} }

// iterClosed is amd64's.
func (c *a64Compiler) iterClosed(v *ssa.Value, guard a64Guard) {
	x, ok := v.Args[0], c.a.NewLabel()
	if c.enc.IterState == 0 {
		c.a.B(c.stubLabel(v.State, exitKind(v.Aux)))
		return
	}
	closed := a64NoLabel
	if v.Index == 1 && c.enc.CallGo != 0 && exitKind(v.Aux) == abi.ExitHost {
		open, stub := c.a.NewLabel(), c.stubLabel(v.State, exitKind(v.Aux))
		closed = c.a.NewLabel()
		c.cold = append(c.cold, func() {
			c.a.Bind(open)
			c.goArg(abi.OffGoArgs, x)
			c.goLive(v, uint64(ssa.GoIterClose), stub)
			c.a.B(closed)
		})
		guard = a64GuardLabel(c, open)
	}
	fail, _ := guard.label()
	c.pointerWord(x, c.gpr(x, a64A))
	c.a.CmpImm(a64B, 0, true)
	guard.jump(arm64.EQ)
	c.objectPart(a64B, a64B, a64C, c.enc.ClassCursor, c.enc.IterState, guard)
	c.a.LoadU8(a64C, a64B, c.enc.IterDone)
	c.a.Cbnz(a64C, ok, false)
	c.a.Load(a64C, a64B, c.enc.IterArr)
	c.a.CmpImm(a64C, 0, true)
	guard.jump(arm64.EQ)
	if v.Const.Bits == 0 {
		// No cell: a done cursor's alone, which jumps to ok.
		c.a.B(fail)
	} else {
		c.movImm(a64C, v.Const.Bits)
		c.a.Load(a64C, a64C, 0)
		c.a.CmpImm(a64C, 0, true)
		guard.jump(arm64.EQ)
		c.a.LoadU8(a64C, a64C, 0)
		c.a.CmpImm(a64C, 0, false)
		guard.jump(arm64.EQ)
	}
	c.a.Bind(ok)
	c.iterRelease()
	if closed != a64NoLabel {
		c.a.Bind(closed)
	}
}

// a64Guard is where a guard that fails goes: v's exit (guardFor), a label
// (a64GuardLabel), or, the zero value, nowhere, as on amd64.
type a64Guard struct {
	c  *a64Compiler
	v  *ssa.Value
	to arm64.Label
}

// a64GuardLabel is a guard that jumps to l.
func a64GuardLabel(c *a64Compiler, l arm64.Label) a64Guard { return a64Guard{c: c, to: l} }

// label is where the guard jumps, and false for one that never does.
func (g a64Guard) label() (arm64.Label, bool) {
	switch {
	case g.v != nil:
		return g.c.stubLabel(g.v.State, exitKind(g.v.Aux)), true
	case g.c != nil:
		return g.to, true
	}
	return 0, false
}

// jump emits the guard's jump, taken on cond.
func (g a64Guard) jump(cond arm64.Cond) {
	switch {
	case g.v != nil:
		g.c.a.BCond(cond, g.c.stubLabel(g.v.State, exitKind(g.v.Aux)))
	case g.c != nil:
		g.c.a.BCond(cond, g.to)
	}
}

// value emits one value.
func (c *a64Compiler) value(v *ssa.Value, b *ssa.Block) {
	arg := func(i int) *ssa.Value { return v.Args[i] }
	// guard is for the guards emitted here; one passed on, which may be
	// kept for cold code, is made only for the values that pass one
	// (guardFor), not for every value.
	guard := c.guardFor(v)
	switch v.Op {
	case ssa.OpLoadSlot:
		if isFloat(v) {
			// Typed code's raw float64 slot, its number word's bits.
			base, disp := c.slotAddr(v.Aux, false, a64A)
			c.a.LoadF(a64F0, base, disp)
			c.setF(v, a64F0)
			break
		}
		d := c.gdst(v)
		base, disp := c.slotAddr(v.Aux, false, d)
		c.a.Load(d, base, disp)
		c.setG(v, d)
	case ssa.OpHeapLoad, ssa.OpHeapStore:
		c.heapAccess(v)
	case ssa.OpTableCallee:
		// As amd64's: the entry's address, for the call right after it.
		c.a.MovRR32(a64A, c.gpr(arg(0), a64A))
		c.a.ShiftImm(arm64.Lsl, a64A, a64A, uint8(abi.TableEntryShift), true)
		c.movImm(a64B, v.Const.Bits)
		c.a.Op(arm64.Add, a64A, a64A, a64B, true)
		c.a.Store(a64Ctx, abi.OffCallTable, a64A)
	case ssa.OpHeapBase, ssa.OpHeapLen:
		d := c.gdst(v)
		c.movImm(d, v.Const.Bits)
		off := int32(abi.OffHeapBase)
		if v.Op == ssa.OpHeapLen {
			off = abi.OffHeapLen
		}
		c.a.Load(d, d, off)
		c.setG(v, d)
	case ssa.OpDivI32, ssa.OpDivU32, ssa.OpRemI32, ssa.OpRemU32:
		c.divide32(v)
	case ssa.OpCellNumLoad:
		c.a.LoadF(a64F0, c.gpr(arg(0), a64A), c.enc.NumOffset)
		c.setF(v, a64F0)
	case ssa.OpCellNumStore:
		// The number word alone: the pointer word is none, a number's.
		w := c.gpr(arg(1), a64B)
		c.a.Store(c.gpr(arg(0), a64A), c.enc.NumOffset, w)
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
		guard.jump(arm64.EQ)
		d := c.fdst(v)
		c.a.FMovToF(d, r)
		c.setF(v, d)
	case ssa.OpNumberF64:
		// As amd64's: the number word of a value proven a number.
		d := c.fdst(v)
		c.a.FMovToF(d, c.gpr(arg(0), a64B))
		c.setF(v, d)
	case ssa.OpOddballF64:
		c.oddball(v, guard)
	case ssa.OpCheckInit:
		r := c.gpr(arg(0), a64B)
		c.movImm(a64A, c.enc.Uninitialized)
		c.a.Cmp(r, a64A, true)
		guard.jump(arm64.EQ)
	case ssa.OpCheckTrue:
		r := c.gpr(arg(0), a64B)
		c.movImm(a64A, c.enc.True)
		c.a.Cmp(r, a64A, true)
		guard.jump(arm64.NE)
	case ssa.OpTruth:
		c.truth(v, c.guardFor(v), nil)
	case ssa.OpArrayOf:
		c.arrayOf(v, c.guardFor(v))
	case ssa.OpIndexedOf:
		c.indexedOf(v, c.guardFor(v))
	case ssa.OpObjectOf:
		if v.Index == ssa.ObjectOfTry {
			// As amd64's: 0 where it is no object.
			zero, done := c.a.NewLabel(), c.a.NewLabel()
			if c.referenceTo(v, v.Args[0], c.enc.Object, a64GuardLabel(c, zero), zero) {
				c.a.B(done)
			}
			c.a.Bind(zero)
			c.movImm(a64C, 0)
			c.a.Bind(done)
			c.setG(v, a64C)
			break
		}
		if c.objectOf(v, c.guardFor(v)) {
			c.setG(v, a64C)
		}
	case ssa.OpInstanceOf:
		c.instanceOf(v, c.guardFor(v), nil)
	case ssa.OpTypeIs:
		c.typeIs(v, c.guardFor(v), nil)
	case ssa.OpKeyIs:
		c.keyIs(v, nil)
	case ssa.OpCall:
		c.call(v, c.guardFor(v))
	case ssa.OpCallCell:
		c.cellAddr(v, abi.OffKeep+int32(v.Args[0].Index)*int32(c.enc.ValueSize))
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
	case ssa.OpBigElemRead:
		c.bigElemRead(v, c.guardFor(v))
	case ssa.OpBigElemWrite:
		c.bigElemWrite(v, c.guardFor(v))
	case ssa.OpConstI64:
		d := c.gdst(v)
		c.movImm(d, v.Const.Bits)
		c.setG(v, d)
	case ssa.OpBigCell:
		c.cellAddr(v, abi.OffKeep+int32(v.Args[0].Index)*int32(c.enc.ValueSize))
	case ssa.OpAddI64, ssa.OpSubI64:
		op := arm64.Adds
		if v.Op == ssa.OpSubI64 {
			op = arm64.Subs
		}
		c.a.Op(op, a64A, c.gpr(arg(0), a64A), c.gpr(arg(1), a64B), true)
		// Outside the int64 range: a BigInt on the heap, Go's.
		guard.jump(arm64.VS)
		c.setG(v, a64A)
	case ssa.OpMulI64:
		// The product's high word is its low word's sign unless it is
		// outside the int64 range.
		x, y := c.gpr(arg(0), a64A), c.gpr(arg(1), a64B)
		c.a.Mul(a64C, x, y)
		c.a.Smulh(a64B, x, y)
		c.a.ShiftImm(arm64.Asr, a64A, a64C, 63, true)
		c.a.Cmp(a64B, a64A, true)
		guard.jump(arm64.NE)
		c.setG(v, a64C)
	case ssa.OpCmpI64:
		c.a.Cmp(c.gpr(arg(0), a64A), c.gpr(arg(1), a64B), true)
		c.a.Cset(a64A, a64CmpCond(ir.Operator(v.Aux), arg(0).Type == ssa.Uint64))
		c.setG(v, a64A)
	case ssa.OpCmpI32:
		c.a.Cset(a64A, c.cmpI32(v))
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
		guard.jump(arm64.HI)
		if v.Key == 1 {
			// Room for homes alone, as amd64's.
			break
		}
		c.a.Load(a64A, a64Ctx, abi.OffLevel)
		c.a.AddImm(a64A, a64A, int64(max(v.Const.Bits, 1)), true)
		c.a.Load(a64B, a64Ctx, abi.OffBounds)
		c.a.Load(a64B, a64B, abi.OffBoundsLevelLimit)
		c.a.Cmp(a64A, a64B, true)
		guard.jump(arm64.HS)
	case ssa.OpTailSelf:
		c.tailSelf(v, guard)
	case ssa.OpIterClosed:
		c.iterClosed(v, guard)
	case ssa.OpSameSymbol:
		if c.reference(v, v.Args[0], c.enc.Symbol, guard) {
			c.movImm(a64B, v.Const.Bits)
			c.a.Cmp(a64C, a64B, true)
			guard.jump(arm64.NE)
		}
	case ssa.OpPoll:
		c.entryPoll(v)
	case ssa.OpAwaitReady:
		c.awaitReady(v, guard)
	case ssa.OpTailRoom:
		c.a.Load(a64A, a64Ctx, abi.OffLevel)
		if v.Index != 0 {
			// The frames inlined in the level, its call's included.
			c.a.AddImm(a64A, a64A, int64(v.Index), true)
		}
		c.a.Load(a64B, a64Ctx, abi.OffBounds)
		c.a.Load(a64B, a64B, abi.OffBoundsTailLevel)
		c.a.Cmp(a64A, a64B, true)
		guard.jump(arm64.HS)
	case ssa.OpSameObject:
		if v.Index == 1 {
			// A typed array of the prototype at Const.Bits, as amd64's.
			p := c.gpr(arg(0), a64A)
			c.a.LoadU8(a64B, p, c.enc.ObjectClass)
			c.a.CmpImm(a64B, int64(c.enc.ClassTypedArray), false)
			guard.jump(arm64.NE)
			c.loadProtoPlain(a64B, p)
			c.movImm(a64C, v.Const.Bits)
			c.a.Cmp(a64B, a64C, true)
			guard.jump(arm64.NE)
			break
		}
		c.movImm(a64B, v.Const.Bits)
		c.a.Cmp(c.gpr(arg(0), a64A), a64B, true)
		guard.jump(arm64.NE)
	case ssa.OpHolderCell:
		// As amd64's: the chain from the prototype on, then the last one's
		// table.
		c.protoChain(v.Holders().List(), guard, false)
		c.a.Load(a64A, a64A, c.enc.ObjectProps)
		c.a.AddImm(a64A, a64A, int64(int32(v.Index)*c.enc.ValueSize), true)
		c.setG(v, a64A)
	case ssa.OpChainCheck:
		c.protoChain(v.Add().Chain(), guard, true)
	case ssa.OpProtoIs:
		// As amd64's, as constructGuards checks the prototype.
		p := c.gpr(arg(0), a64C)
		if p != a64C {
			c.a.MovRR(a64C, p)
		}
		c.a.LoadU32(a64B, a64C, c.enc.ObjectPropsLen)
		c.a.CmpImm(a64B, int64(v.Index), true)
		guard.jump(arm64.LS)
		c.a.Load(a64B, a64C, c.enc.ObjectShape)
		c.a.Load(a64B, a64B, c.enc.ShapeKeys)
		c.a.LoadU32(a64B, a64B, int32(v.Index)*c.enc.KeySize+c.enc.PropertyKey)
		c.movImm(a64A, uint64(v.Key))
		c.a.Cmp(a64B, a64A, false)
		guard.jump(arm64.NE)
		c.a.Load(a64B, a64C, c.enc.ObjectProps)
		c.a.Load(a64C, a64B, int32(v.Index)*c.enc.ValueSize+c.enc.RefOffset)
		c.movImm(a64A, v.Const.Bits)
		c.a.Cmp(a64C, a64A, true)
		guard.jump(arm64.NE)
	case ssa.OpAccessorIs:
		// The cell's accessor pair, and its getter's or its setter's
		// function.
		p := c.gpr(arg(0), a64A)
		c.a.Load(a64C, p, c.enc.NumOffset)
		c.movImm(a64B, c.enc.Accessor)
		c.a.Cmp(a64C, a64B, true)
		guard.jump(arm64.NE)
		c.a.Load(a64C, p, c.enc.RefOffset)
		at := c.enc.AccessorGetter
		if v.Index == 1 {
			at = c.enc.AccessorSetter
		}
		c.a.Load(a64C, a64C, at)
		c.movImm(a64B, v.Const.Bits)
		c.a.Cmp(a64C, a64B, true)
		guard.jump(arm64.NE)
	case ssa.OpPropRead:
		c.property(v, c.guardFor(v))
		c.numberCell(guard)
		c.a.FMovToF(a64F0, a64B)
		c.setF(v, a64F0)
	case ssa.OpPropCell:
		if v.Tries() {
			// As amd64's: none where it finds no property.
			none, found := c.a.NewLabel(), c.a.NewLabel()
			if v.Args[0].Op == ssa.OpObjectOf && v.Args[0].Index == ssa.ObjectOfTry {
				// No object: none.
				c.a.Cbz(c.gpr(v.Args[0], a64A), none, true)
			}
			c.property(v, a64GuardLabel(c, none))
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
		if v.Op == ssa.OpGlobalCell && v.Epoch != 0 {
			// As amd64's: the count unchanged and the scope's object the
			// one compiled for, the binding at its position; the count
			// compared whole.
			c.a.Load(a64A, a64Ctx, abi.OffGlobalEpoch)
			c.a.Load(a64B, a64A, 0)
			c.a.CmpImm(a64B, int64(v.Epoch-1), true)
			guard.jump(arm64.NE)
			c.movImm(a64A, v.Const.Bits)
			c.a.Load(a64B, a64Ctx, abi.OffGlobal)
			c.a.Cmp(a64A, a64B, true)
			guard.jump(arm64.NE)
			c.a.Load(a64A, a64A, c.enc.ObjectProps)
			c.a.AddImm(a64A, a64A, int64(int32(v.Index)*c.enc.ValueSize), true)
			c.setG(v, a64A)
			break
		}
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
			guard.jump(arm64.NE)
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
		guard.jump(arm64.NE)
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
		guard.jump(arm64.EQ)
		c.setG(v, a64A)
	case ssa.OpStringMethod:
		// As amd64's: where String.prototype has it now, its shape checked.
		c.movImm(a64B, c.enc.String)
		c.a.Cmp(c.gpr(arg(0), a64A), a64B, true)
		guard.jump(arm64.NE)
		c.a.Load(a64A, a64Ctx, abi.OffStringProto)
		c.a.Cbz(a64A, c.stubLabel(v.State, exitKind(v.Aux)), true)
		c.a.Load(a64B, a64A, c.enc.ObjectShape)
		c.a.Load(a64C, a64Ctx, abi.OffStringShape)
		c.a.Cmp(a64B, a64C, true)
		guard.jump(arm64.NE)
		c.a.Load(a64A, a64A, c.enc.ObjectProps)
		c.a.Load(a64B, a64Ctx, abi.OffCharCodeAtOffset)
		c.a.Op(arm64.Add, a64A, a64A, a64B, true)
		c.setG(v, a64A)
	case ssa.OpStringProp:
		// As amd64's: a string's method where String.prototype has it, its
		// shape the one the code was compiled for; or a BigInt's, a
		// number's or a boolean's; or one a prototype of the kind's has.
		var chain []ssa.Holder
		if h := v.Holders(); h != nil {
			chain = h.List()
		}
		switch w := c.gpr(arg(0), a64A); v.Primitive() {
		case ssa.PrimitiveBigInt:
			c.movImm(a64B, c.enc.HeapBigTag)
			c.a.Cmp(w, a64B, true)
			guard.jump(arm64.LO)
		case ssa.PrimitiveNumber:
			c.movImm(a64B, abi.NumberLimit)
			c.a.Cmp(w, a64B, true)
			guard.jump(arm64.HS)
		case ssa.PrimitiveBoolean:
			yes := c.a.NewLabel()
			c.movImm(a64B, c.enc.True)
			c.a.Cmp(w, a64B, true)
			c.a.BCond(arm64.EQ, yes)
			c.movImm(a64B, c.enc.False)
			c.a.Cmp(w, a64B, true)
			guard.jump(arm64.NE)
			c.a.Bind(yes)
		default:
			c.movImm(a64B, c.enc.String)
			c.a.Cmp(w, a64B, true)
			guard.jump(arm64.NE)
			if len(chain) == 0 {
				c.a.Load(a64A, a64Ctx, abi.OffStringMethods)
				c.a.Cbz(a64A, c.stubLabel(v.State, exitKind(v.Aux)), true)
			}
		}
		if len(chain) != 0 {
			c.protoChain(chain[:len(chain)-1], guard, false)
			c.movImm(a64A, uint64(chain[len(chain)-1].Object))
		}
		c.a.Load(a64B, a64A, c.enc.ObjectShape)
		c.movImm(a64C, v.Const.Bits)
		c.a.Cmp(a64B, a64C, true)
		guard.jump(arm64.NE)
		c.a.Load(a64B, a64A, c.enc.ObjectShape)
		c.a.Load(a64B, a64B, c.enc.ShapeKeys)
		c.a.LoadU32(a64B, a64B, int32(v.Index)*c.enc.KeySize+c.enc.PropertyKey)
		c.a.CmpImm(a64B, int64(v.Key), false)
		guard.jump(arm64.NE)
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
	case ssa.OpCodeCharOr:
		c.codeCharOr(v)
	case ssa.OpStringCodePointOr:
		c.stringCodePointOr(v)
	case ssa.OpStringAtOr:
		c.stringAtOr(v)
	case ssa.OpNumberIs:
		c.numberIs(v)
	case ssa.OpTypeofCell:
		c.typeofCell(v)
	case ssa.OpCursorCell:
		c.cursorCell(v)
	case ssa.OpLoopMark:
		// As amd64's: abi.Context's Loop.
		c.movImm(a64A, v.Const.Bits)
		c.a.Store(a64Ctx, abi.OffLoop, a64A)
	case ssa.OpFreeze:
		c.freeze(v)
	case ssa.OpForInDone:
		c.forInDone(v)
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
		if ssa.TypedKindOf(v) >= 0 {
			// A site that met typed arrays of one element type: theirs
			// inline, an Array's out of line.
			guard := c.guardFor(v)
			c.arrayClass(arg(0), typed)
			c.typedRead(v, done, true)
			c.a.Bind(done)
			c.cold = append(c.cold, func() {
				c.a.Bind(typed)
				c.index(v.Args[1], guard)
				c.element(v.Args[0], guard)
				c.a.FMovToF(a64F0, a64B)
				c.a.B(done)
			})
			c.setF(v, a64F0)
			break
		}
		c.typedArray(arg(0), typed)
		c.index(arg(1), c.guardFor(v))
		c.element(arg(0), c.guardFor(v))
		c.a.FMovToF(a64F0, a64B)
		c.a.Bind(done)
		c.cold = append(c.cold, func() {
			c.a.Bind(typed)
			c.typedRead(v, done, false)
		})
		c.setF(v, a64F0)
	case ssa.OpElemCell:
		fail, done := c.guardFor(v), c.a.NewLabel()
		if c.enc.CallGo != 0 && c.enc.UndefinedCell != 0 && v.Index != ssa.ElemPresent {
			// An element it does not find Go says is undefined, where it
			// can (ssa.GoElemCell), but for a loop's (ssa.ElemPresent).
			slow, stub := c.a.NewLabel(), c.stubLabel(v.State, exitKind(v.Aux))
			fail = a64GuardLabel(c, slow)
			c.cold = append(c.cold, func() {
				c.a.Bind(slow)
				size := int32(unsafe.Sizeof(abi.GoArg{}))
				c.a.Store(a64Ctx, abi.OffGoArgs+c.enc.RefOffset, c.gpr(v.Args[0], a64C))
				c.keyBits(v.Args[1])
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
		if c.enc.CallGo != 0 && (len(v.Args) == 3 || c.enc.ValueSize == 16) {
			// Any other store Go makes if it can (ssa.GoElemWrite), one with
			// checks only where none points into the array's storage
			// (storageFree), as amd64's.
			stub := slow
			slow = c.a.NewLabel()
			c.cold = append(c.cold, func() {
				c.a.Bind(slow)
				c.storageFree(v.Args[0], v.Args[3:], stub)
				size := int32(unsafe.Sizeof(abi.GoArg{}))
				c.a.Store(a64Ctx, abi.OffGoArgs+c.enc.RefOffset, c.gpr(v.Args[0], a64C))
				c.keyBits(v.Args[1])
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
		fail := a64GuardLabel(c, slow)
		value := func() arm64.FReg { return c.fpr(v.Args[2], a64F0) }
		if ssa.TypedKindOf(v) >= 0 {
			// A site that met typed arrays of one element type: theirs
			// inline, an Array's out of line.
			again := c.a.NewLabel()
			c.arrayClass(arg(0), typed)
			c.a.Bind(again)
			c.typedWrite(v, done, slow, again, value, true)
			c.cold = append(c.cold, func() {
				c.a.Bind(typed)
				c.index(v.Args[1], fail)
				c.a.LoadU8(a64B, c.gpr(v.Args[0], a64C), c.enc.ObjectFlags)
				c.movImm(a64C, uint64(c.enc.FlagShared))
				c.a.Tst(a64B, a64C, false)
				fail.jump(arm64.NE)
				c.element(v.Args[0], fail)
				c.boxF64(c.fpr(v.Args[2], a64F0), a64B)
				c.a.Store(a64A, c.enc.NumOffset, a64B)
				c.a.B(done)
			})
			c.a.Bind(done)
			break
		}
		c.cold = append(c.cold, func() {
			c.a.Bind(typed)
			again := c.a.NewLabel()
			c.a.Bind(again)
			c.typedWrite(v, done, slow, again, value, false)
		})
		c.typedArray(arg(0), typed)
		c.index(arg(1), fail)
		// Shared elements (FlagShared) are Go's to copy before the store.
		c.a.LoadU8(a64B, c.gpr(arg(0), a64C), c.enc.ObjectFlags)
		c.movImm(a64C, uint64(c.enc.FlagShared))
		c.a.Tst(a64B, a64C, false)
		fail.jump(arm64.NE)
		c.element(arg(0), fail)
		c.boxF64(c.fpr(arg(2), a64F0), a64B)
		c.a.Store(a64A, c.enc.NumOffset, a64B)
		c.a.Bind(done)
	case ssa.OpBoxF64:
		if x := arg(0); exactF64(x) {
			// As amd64's: an integer's number is no NaN.
			c.a.FMovFromF(a64A, c.fpr(x, a64F0))
		} else {
			c.boxF64(c.fpr(x, a64F0), a64A)
		}
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
		c.looseNullish(v, c.guardFor(v), nil)
	case ssa.OpEqTagged:
		c.eqTagged(v, c.guardFor(v), nil)
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
		// The W forms take the count modulo 32, as JavaScript does; a
		// constant count is the instruction's.
		d := c.gdst(v)
		if k := arg(1); k.Op == ssa.OpConstI32 {
			c.a.ShiftImm(a64Shift(v.Op), d, c.gpr(arg(0), a64A), uint8(k.Const.Bits&31), false)
		} else {
			c.a.ShiftReg(a64Shift(v.Op), d, c.gpr(arg(0), a64A), c.gpr(k, a64C), false)
		}
		c.setG(v, d)
	case ssa.OpAddI32, ssa.OpSubI32:
		// As amd64's: the low 32 bits, a constant operand the immediate.
		d := c.gdst(v)
		x := c.gpr(arg(0), a64A)
		k := arg(1)
		switch {
		case k.Op == ssa.OpConstI32 && v.Op == ssa.OpAddI32:
			c.a.AddImm(d, x, int64(int32(uint32(k.Const.Bits))), false)
		case k.Op == ssa.OpConstI32:
			c.a.AddImm(d, x, -int64(int32(uint32(k.Const.Bits))), false)
		case v.Op == ssa.OpAddI32:
			c.a.Op(arm64.Add, d, x, c.gpr(k, a64B), false)
		default:
			c.a.Op(arm64.Sub, d, x, c.gpr(k, a64B), false)
		}
		c.setG(v, d)
	case ssa.OpConstI32:
		d := c.gdst(v)
		c.movImm(d, v.Const.Bits)
		c.setG(v, d)
	case ssa.OpCheckI32:
		// As amd64's: converted, its low 32 bits converted back, the same
		// -- NaN, unordered, is not equal -- and not -0.
		x := c.fpr(arg(0), a64F0)
		c.a.Fcvtzs(a64A, x)
		c.a.Scvtf(a64F1, a64A, false)
		c.a.FCmp(a64F1, x)
		guard.jump(arm64.NE)
		c.a.FMovFromF(a64B, x)
		c.movImm(a64C, 1<<63)
		c.a.Cmp(a64B, a64C, true)
		guard.jump(arm64.EQ)
		c.setG(v, a64A)

	case ssa.OpNotI32:
		c.a.Mvn(a64A, c.gpr(arg(0), a64A), false)
		c.setG(v, a64A)
	case ssa.OpMulI32:
		d := c.gdst(v)
		c.a.MulW(d, c.gpr(arg(0), a64A), c.gpr(arg(1), a64B))
		c.setG(v, d)
	case ssa.OpPrimitive:
		c.primitive(v, c.guardFor(v))
	case ssa.OpClz32:
		d := c.gdst(v)
		c.a.ClzW(d, c.gpr(arg(0), a64A))
		c.setG(v, d)
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
func (c *a64Compiler) truth(v *ssa.Value, guard a64Guard, to *a64Target) {
	r := c.gpr(v.Args[0], a64B)
	t := c.tests(to, false)
	number, yes, no := c.a.NewLabel(), t.yes, t.no
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
	guard.jump(arm64.NE)
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
	if to != nil {
		// Below zero or above it: neither holds when unordered.
		if t.fall == t.yes {
			c.a.BCond(arm64.EQ, t.no)
			c.a.BCond(arm64.VS, t.no)
		} else {
			c.a.BCond(arm64.MI, t.yes)
			c.jumpIf(t, arm64.GT)
		}
		return
	}
	done := c.a.NewLabel()
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
	if v.Aux == ssa.ToInt32Exact {
		// As amd64's: an int32's number, truncated.
		c.a.Fcvtzs(a64A, c.fpr(xv, a64F0))
		c.a.MovRR32(a64A, a64A)
		c.setG(v, a64A)
		return
	}
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

// heapAccess is amd64's heapAccess: typed code's heap load or store.
func (c *a64Compiler) heapAccess(v *ssa.Value) {
	kind := v.Aux
	size := abi.ElemSize(kind)
	float := kind == abi.ElemFloat32 || kind == abi.ElemFloat64
	out, done := c.a.NewLabel(), c.a.NewLabel()
	c.a.Sxtw(a64A, c.gpr(v.Args[0], a64A))
	if size > 1 {
		c.a.AndImm(a64A, a64A, uint64(int64(-size)), true)
	}
	n := len(v.Args)
	c.a.Cmp(a64A, c.gpr(v.Args[n-1], a64B), true)
	c.a.BCond(arm64.HS, out)
	c.a.Op(arm64.Add, a64A, a64A, c.gpr(v.Args[n-2], a64B), true)
	if v.Op == ssa.OpHeapStore {
		if float {
			x := c.fpr(v.Args[1], a64F0)
			if kind == abi.ElemFloat64 {
				c.a.StoreF(a64A, 0, x)
			} else {
				c.a.FCvtToSingle(a64F1, x)
				c.a.StoreS(a64A, 0, a64F1)
			}
		} else {
			x := c.gpr(v.Args[1], a64B)
			switch size {
			case 1:
				c.a.Store8(a64A, 0, x)
			case 2:
				c.a.Store16(a64A, 0, x)
			default:
				c.a.Store32(a64A, 0, x)
			}
		}
		c.a.Bind(out)
		c.a.Bind(done)
		return
	}
	switch kind {
	case abi.ElemInt8:
		c.a.LoadS8(a64A, a64A, 0)
	case abi.ElemUint8:
		c.a.LoadU8(a64A, a64A, 0)
	case abi.ElemInt16:
		c.a.LoadS16(a64A, a64A, 0)
	case abi.ElemUint16:
		c.a.LoadU16(a64A, a64A, 0)
	case abi.ElemFloat32:
		c.a.LoadS(a64F0, a64A, 0)
		c.a.FCvtFromSingle(a64F0, a64F0)
	case abi.ElemFloat64:
		c.a.LoadF(a64F0, a64A, 0)
	default:
		c.a.LoadS32(a64A, a64A, 0)
	}
	c.a.B(done)
	c.a.Bind(out)
	if float {
		c.movImm(a64A, math.Float64bits(math.NaN()))
		c.a.FMovToF(a64F0, a64A)
	} else {
		c.movImm(a64A, 0)
	}
	c.a.Bind(done)
	if float {
		c.setF(v, a64F0)
	} else {
		c.setG(v, a64A)
	}
}

// divide32 is amd64's divide32: the operands widened to 64 bits, sign- or
// zero-extended, so that MinInt32 / -1 is 2^31, whose low half is
// MinInt32, and MinInt32 % -1 0; SDIV and UDIV fault at nothing and give
// 0 for a zero divisor, and a remainder of one is 0 too, not MSUB's x.
func (c *a64Compiler) divide32(v *ssa.Value) {
	signed := v.Op == ssa.OpDivI32 || v.Op == ssa.OpRemI32
	rem := v.Op == ssa.OpRemI32 || v.Op == ssa.OpRemU32
	x, y := c.gpr(v.Args[0], a64C), c.gpr(v.Args[1], a64B)
	if signed {
		c.a.Sxtw(a64C, x)
		c.a.Sxtw(a64B, y)
		c.a.Sdiv(a64A, a64C, a64B)
	} else {
		c.a.MovRR32(a64C, x)
		c.a.MovRR32(a64B, y)
		c.a.Udiv(a64A, a64C, a64B)
	}
	if rem {
		done := c.a.NewLabel()
		c.a.Cbz(a64B, done, true)
		c.a.Msub(a64A, a64A, a64B, a64C)
		c.a.Bind(done)
	}
	c.setG(v, a64A)
}
