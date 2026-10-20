package mir

import (
	"fmt"
	"math"
	"slices"
	"unsafe"

	"github.com/go-quickjs/go-quickjs/internal/jit/abi"
	"github.com/go-quickjs/go-quickjs/internal/jit/asm/amd64"
	"github.com/go-quickjs/go-quickjs/internal/jit/ir"
	"github.com/go-quickjs/go-quickjs/internal/jit/ssa"
)

// The fixed registers. RSP, RBP and R14 (Go's g) are never touched.
const (
	regCtx    = amd64.RDI
	regLocals = amd64.RSI
	regStack  = amd64.RDX
	scratchA  = amd64.RAX
	scratchC  = amd64.RCX // also shift counts
	scratchB  = amd64.R11
	xScratch0 = amd64.XReg(0)
	xScratch1 = amd64.XReg(1)
	xScratch2 = amd64.XReg(15)
)

// A native call's registers (abi.Context's ReturnTo): at its callee's call
// entry (callEntry), where it returns to, the callee's closure and its
// link -- and, for enterExit, the callee's function object in callObject;
// at its return (returnNative), the result's words, and where it goes on in
// callLink.
const (
	callRet     = scratchA
	callClosure = scratchC
	callLink    = scratchB
	callObject  = amd64.R9
	retNum      = scratchA
	retRef      = scratchC
)

// compiler is amd64's code generator over the shared core.
type compiler struct {
	*core
	a *amd64.Asm
	// consts are the words every context holds (abi.Context's Consts).
	consts [abi.NumConsts]uint64
	// pool is the code's own constants, placed after its last instruction
	// (movImm).
	pool []poolConst
	// label of each block's code.
	labels []amd64.Label // by block ID
	// stubs: one exit per frame state and kind, by state ID and kind
	// (stubAt, 1 plus the label, or 0), or in the map.
	stubs   map[stubKey]amd64.Label
	stubAt  []int
	stubFor []stub
	// cold code, emitted after everything else.
	cold []func()
	// callV is the call being emitted, callTop where it starts: a pool it
	// takes from that ran out is filled by a call of Go, and the call made
	// again from there (refillOnEmpty).
	callV   *ssa.Value
	callTop amd64.Label
	// tail is where exits that filled records go (recordsTail), if any
	// does; recorded marks the exit being emitted as one.
	tail               amd64.Label
	tailUsed, recorded bool
	// table is where an exit that leaves its frame to Go goes (tableExit),
	// if any does; exits are those exits' descriptions.
	table     amd64.Label
	tableUsed bool
	exits     []*abi.ExitDescriptor
	// enter is where native calls to callees with no native code go
	// (enterExit), if any does.
	enter     amd64.Label
	enterUsed bool
	// ret is the code's return (returnNative), which its first return
	// makes and every other jumps to; resume is where Go goes on after a
	// native call it finished (resumeCall), if the code makes any.
	ret                 amd64.Label
	retUsed, resumeUsed bool
	// calling is where every call of Go goes (goCalling), which goes on
	// to saving; goFails are the calls whose refusals Go sends to a label
	// of their own (abi.GoCallState's Fail), set once the code is laid out.
	calling     amd64.Label
	callingUsed bool
	goFails     []goFail
	// probe is the megamorphic probe of the code's reads, and of its
	// writes, which its sites share (megaProbe), if any uses one.
	probe     [2]amd64.Label
	probeUsed [2]bool
	// live is the routine of the calls of Go that save what they need
	// themselves (goCallingLive).
	live     amd64.Label
	liveUsed bool
	// bigLive is the routine of goCallingLive's calls of BigInts' operators
	// (ssa.CallSite's Big), liveCall's to abi.Encoding's BigFast, and
	// goCallBig says the call being made is one.
	bigLive                amd64.Label
	bigLiveUsed, goCallBig bool
	// saving is the routine every call of Go that saves every register
	// shares (goSaving), if any makes one.
	saving     amd64.Label
	savingUsed bool
	// refStoring is the routine of the stores of pointer words storeRef
	// makes (refStore).
	refStoring     amd64.Label
	refStoringUsed bool
	// appending is the routine the element writes Go makes share where a
	// number at an array's length may be appended natively (appendingCall),
	// which goes on to saving for the rest.
	appending     amd64.Label
	appendingUsed bool
}

// goFail is a call of Go's description, where it goes on (back), and
// where Go sends it if it does nothing (to).
type goFail struct {
	st       *abi.GoCallState
	back, to amd64.Label
}

type stub struct {
	key   stubKey
	label amd64.Label
}

// CompileAMD64 compiles f for amd64, given the VM's value encoding.
func CompileAMD64(f *ssa.Func, enc abi.Encoding) (*Code, error) { return compileAMD64(nil, f, enc) }

// amd64GPRs and amd64XMMs are the registers amd64's allocator assigns,
// never changed.
var (
	amd64GPRs = []int{int(amd64.RBX), int(amd64.R8), int(amd64.R9), int(amd64.R10),
		int(amd64.R12), int(amd64.R13), int(amd64.R15)}
	amd64XMMs = func() (xmms []int) {
		for r := amd64.XReg(2); r < xScratch2; r++ {
			xmms = append(xmms, int(r))
		}
		return xmms
	}()
)

func compileAMD64(w *Workspace, f *ssa.Func, enc abi.Encoding) (code *Code, err error) {
	defer func() {
		if v := recover(); v != nil {
			code, err = nil, fmt.Errorf("%w: %v", ErrUnsupported, v)
		}
	}()
	k, err := prepare(w, f, enc, amd64GPRs, amd64XMMs)
	if err != nil {
		return nil, err
	}
	c := &compiler{core: k, consts: enc.ConstWords()}
	c.stubAt = k.ints(2 * f.NumStates())
	if w != nil {
		g := &w.amd64
		if g.stubs == nil {
			g.stubs = map[stubKey]amd64.Label{}
		}
		g.a.Reset()
		clear(g.stubs)
		c.a, c.stubs, c.stubFor, c.cold = &g.a, g.stubs, g.stubFor[:0], g.cold[:0]
		c.labels = slices.Grow(g.labels[:0], numBlocks(f))[:numBlocks(f)]
		defer func() { g.labels, g.stubFor, g.cold = c.labels[:0], c.stubFor[:0], clearFuncs(c.cold) }()
	} else {
		c.a, c.stubs = &amd64.Asm{}, map[stubKey]amd64.Label{}
		c.labels = make([]amd64.Label, numBlocks(f))
	}
	entries := map[int]int{}
	for _, b := range c.order {
		c.labels[b.ID] = c.a.NewLabel()
	}
	callAt, resumeAt := -1, -1
	for i, b := range c.order {
		if f.CallEntry && b == c.entryAt(0) {
			// Native callers' entry, which makes the frame and goes on at
			// the start.
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
		// A call the leaf has given up (abi.GoCallState's BigOff) goes the
		// way any other does.
		c.a.Bind(c.bigLive)
		c.a.LoadU8(scratchB, scratchC, abi.OffGoCallBigOff)
		c.a.Op(amd64.Test, scratchB, scratchB, false)
		c.a.Jcc(amd64.CondNE, c.live)
		c.liveCall(c.enc.BigFast)
	}
	if c.callingUsed {
		// Laid out just before saving, which it goes on to.
		c.a.Bind(c.calling)
		c.a.Store(regCtx, abi.OffGoState, scratchC)
		c.a.Store(regCtx, abi.OffGoSite, scratchA)
	}
	if c.savingUsed {
		c.a.Bind(c.saving)
		c.savingCall()
	}
	// Last: the code's own constants (movImm), past every instruction.
	text := c.a.Len()
	for _, k := range c.pool {
		c.a.Bind(k.label)
		c.a.Quad(k.v)
	}
	bytes, err := c.a.Finish()
	if err != nil {
		return nil, err
	}
	for pc, at := range entries {
		// Where the jumps shortened before it moved it.
		entries[pc] = c.a.Moved(at)
	}
	if callAt >= 0 {
		callAt = c.a.Moved(callAt)
	}
	for _, g := range c.goFails {
		// Finish moved the labels where the code has them.
		g.st.Fail = int32(c.a.Offset(g.to) - c.a.Offset(g.back))
	}
	if resumeAt >= 0 {
		resumeAt = c.a.Moved(resumeAt)
	}
	c.finishExits()
	return &Code{Bytes: bytes, Entries: entries, CallEntry: callAt, Resume: resumeAt, Text: c.a.Moved(text), Exits: c.exits,
		Calls: c.calls, CallRefs: c.callRefs(), GoCalls: c.goCalls, core: c.core}, nil
}

// movImm sets dst to the word v: a load of the context's copy where v is
// one of its constants, four bytes where a 64-bit immediate takes ten, and
// otherwise the shortest move of the immediate.
func (c *compiler) movImm(dst amd64.Reg, v uint64) {
	if v>>32 != 0 && int64(v) != int64(int32(v)) {
		for i, w := range c.consts {
			if w == v {
				c.a.Load(dst, regCtx, abi.OffConsts+int32(i)*8)
				return
			}
		}
		// Any other, from the code's own constants, after its last
		// instruction: seven bytes, read through the data cache, where ten
		// bytes of instruction had filled the op cache -- a shape's or a
		// prototype's address in each guard, a tenth of the code's hottest
		// bytes, whose size decides how much of it the op cache holds.
		c.a.LoadLabel(dst, c.poolLabel(v))
		return
	}
	c.a.MovImm(dst, v)
}

// poolLabel is where the code's own constant v is (movImm), made if it is
// not one yet.
func (c *compiler) poolLabel(v uint64) amd64.Label {
	for _, k := range c.pool {
		if k.v == v {
			return k.label
		}
	}
	k := poolConst{v: v, label: c.a.NewLabel()}
	c.pool = append(c.pool, k)
	return k.label
}

// cmpWord compares r with the word v, the flags as a compare with a
// register holding v sets them, in one instruction, which leaves no
// register holding v: v as an immediate where it is a 32-bit value
// sign-extended; else the context's copy, where it is one of its
// constants (four bytes); else the code's own (movImm), seven -- where a
// constant's load and a compare had taken seven or ten, a shape's guard
// among them, the code's most frequent.
func (c *compiler) cmpWord(r amd64.Reg, v uint64) {
	if int64(v) == int64(int32(v)) {
		c.a.OpImm(amd64.Cmp, r, int32(v), true)
		return
	}
	for i, w := range c.consts {
		if w == v {
			c.a.CmpMem(r, regCtx, abi.OffConsts+int32(i)*8, true)
			return
		}
	}
	c.a.CmpLabel(r, c.poolLabel(v))
}

// poolConst is a constant of a function's code, a word placed after its
// last instruction, at label (movImm).
type poolConst struct {
	v     uint64
	label amd64.Label
}

// storeImm stores the word v at [base + disp]: as an immediate, in one
// instruction, where it is a 32-bit value sign-extended -- a context's
// field, an exit's PC, a record's slot -- else through scratch, which
// holds v after; where an immediate does, scratch is as it was.
func (c *compiler) storeImm(base amd64.Reg, disp int32, v uint64, scratch amd64.Reg) {
	if int64(v) == int64(int32(v)) {
		c.a.StoreImm(base, disp, int32(v))
		return
	}
	c.movImm(scratch, v)
	c.a.Store(base, disp, scratch)
}

// slotAddr is the base register and displacement of a frame slot's word.
// A captured binding's value is found through its cell, into scratch.
func (c *compiler) slotAddr(slot int, ref bool, scratch amd64.Reg) (amd64.Reg, int32) {
	off := c.enc.NumOffset
	if ref {
		off = c.enc.RefOffset
	}
	switch {
	case slot < c.f.FrameLocals:
		return regLocals, int32(slot)*c.enc.ValueSize + off
	case slot == c.f.ThisSlot:
		return regCtx, abi.OffThis + off
	case slot < c.f.Locals:
		c.a.Load(scratch, regCtx, abi.OffUpvalues)
		c.a.Load(scratch, scratch, int32(slot-c.f.FrameLocals)*8)
		c.a.Load(scratch, scratch, c.enc.UpvalueSlot)
		return scratch, off
	}
	return regStack, int32(slot-c.f.Locals)*c.enc.ValueSize + off
}

// slotSource puts in dst a slot's source (ssa's origin.go): the address
// of its value -- a local's or an operand's in the frame, the receiver's in
// the context, a captured binding's in its cell.
func (c *compiler) slotSource(slot int, dst amd64.Reg) {
	base, disp := c.slotAddr(slot, false, dst)
	c.a.Lea(dst, base, disp-c.enc.NumOffset)
}

// gpr returns a register holding v's word, loading a spilled or lazy value
// into scratch.
func (c *compiler) gpr(v *ssa.Value, scratch amd64.Reg) amd64.Reg {
	if c.isLazy(v) {
		c.materialize(v, scratch)
		return scratch
	}
	l, ok := c.loc(v)
	if !ok {
		panic(fmt.Sprintf("no location for %v (%v)", v, v.Op))
	}
	if l.reg >= 0 {
		return amd64.Reg(l.reg)
	}
	c.a.Load(scratch, regCtx, c.spillDisp(l.spill))
	return scratch
}

// xmm returns an SSE register holding v.
func (c *compiler) xmm(v *ssa.Value, scratch amd64.XReg) amd64.XReg {
	l, ok := c.loc(v)
	if !ok {
		panic(fmt.Sprintf("no location for %v (%v)", v, v.Op))
	}
	if l.reg >= 0 {
		return amd64.XReg(l.reg)
	}
	c.a.LoadSD(scratch, regCtx, c.spillDisp(l.spill))
	return scratch
}

// gdst is the register v's word is made in: its own, or scratchA if it is
// spilled, which setG then stores.
func (c *compiler) gdst(v *ssa.Value) amd64.Reg {
	if l := c.locAt(v); l.reg >= 0 {
		return amd64.Reg(l.reg)
	}
	return scratchA
}

// gprInto puts v's word in dst: moved, loaded from its spill slot, or
// made there if it is lazy.
func (c *compiler) gprInto(v *ssa.Value, dst amd64.Reg) {
	if c.isLazy(v) {
		c.materialize(v, dst)
		return
	}
	if l := c.locAt(v); l.reg >= 0 {
		if amd64.Reg(l.reg) != dst {
			c.a.MovRR(dst, amd64.Reg(l.reg))
		}
		return
	}
	c.a.Load(dst, regCtx, c.spillDisp(c.locAt(v).spill))
}

// setG stores src into v's location.
func (c *compiler) setG(v *ssa.Value, src amd64.Reg) {
	l := c.locAt(v)
	if l.reg >= 0 {
		if amd64.Reg(l.reg) != src {
			c.a.MovRR(amd64.Reg(l.reg), src)
		}
		return
	}
	c.a.Store(regCtx, c.spillDisp(l.spill), src)
}

// setX stores src into v's location.
func (c *compiler) setX(v *ssa.Value, src amd64.XReg) {
	l := c.locAt(v)
	if l.reg >= 0 {
		if amd64.XReg(l.reg) != src {
			c.a.SSEOp(amd64.MovAPD, amd64.XReg(l.reg), src)
		}
		return
	}
	c.a.StoreSD(regCtx, c.spillDisp(l.spill), src)
}

// materialize computes a lazy value's word into dst.
func (c *compiler) materialize(v *ssa.Value, dst amd64.Reg) {
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
		c.boxF64(c.xmm(v.Args[0], xScratch1), dst)
	case ssa.OpBoxBool:
		c.boxBool(c.gpr(v.Args[0], scratchB), dst)
	default:
		panic("materialize " + v.Op.String())
	}
}

// boxF64 puts x's bits in dst, with NaN canonical.
func (c *compiler) boxF64(x amd64.XReg, dst amd64.Reg) {
	done := c.a.NewLabel()
	c.a.MovQFromX(dst, x)
	c.a.SSEOp(amd64.UcomiSD, x, x)
	c.a.Jcc(amd64.CondNP, done)
	c.movImm(dst, c.enc.CanonicalNaN)
	c.a.Bind(done)
}

// boxBool puts the word for b (0 or 1) in dst. dst may be b.
func (c *compiler) boxBool(b, dst amd64.Reg) {
	tr, done := c.a.NewLabel(), c.a.NewLabel()
	c.a.Op(amd64.Test, b, b, false)
	c.a.Jcc(amd64.CondNE, tr)
	c.movImm(dst, c.enc.False)
	c.a.Jmp(done)
	c.a.Bind(tr)
	c.movImm(dst, c.enc.True)
	c.a.Bind(done)
}

// stubLabel returns the exit stub for a state and kind: found by the
// state's ID (stubAt), an exit to Go or a deoptimization, or else in a
// map, which a guard asked for the one it shares with others of its
// instruction had cost.
func (c *compiler) stubLabel(s *ssa.FrameState, kind uint64) amd64.Label {
	i := -1
	switch kind {
	case abi.ExitHost:
		i = 2 * s.ID()
	case abi.ExitDeopt:
		i = 2*s.ID() + 1
	}
	if i >= 0 && i < len(c.stubAt) {
		if l := c.stubAt[i]; l != 0 {
			return amd64.Label(l - 1)
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
	c.stubFor = append(c.stubFor, stub{k, l})
	return l
}

// returnNative returns, the result in the context's Ret and RetFrom: to a
// native caller, if one called (call), with the result's words in retNum
// and retRef -- its pointer word from where RetFrom says the reference is
// -- its frame gone (returnTail); otherwise to Go. The code's first return
// makes it; the others jump there.
func (c *compiler) returnNative() {
	if c.retUsed {
		c.a.Jmp(c.ret)
		return
	}
	c.ret, c.retUsed = c.a.NewLabel(), true
	c.a.Bind(c.ret)
	toGo, word := c.a.NewLabel(), c.a.NewLabel()
	c.a.Load(callLink, regCtx, abi.OffReturnTo)
	c.a.Op(amd64.Test, callLink, callLink, true)
	c.a.Jcc(amd64.CondE, toGo)
	c.a.Load(retNum, regCtx, abi.OffRet)
	c.a.Load(retRef, regCtx, abi.OffRetFrom)
	c.a.Op(amd64.Test, retRef, retRef, true)
	c.a.Jcc(amd64.CondE, word)
	c.a.Load(retRef, retRef, c.enc.RefOffset)
	c.a.Bind(word)
	c.returnTail()
	c.a.Bind(toGo)
	c.a.Ret()
}

// returnTail ends a native call, from the callee's context, the result in
// retNum and retRef, as its caller's code goes on: the VM's stack's top
// back where the callee's frame began, the context no call's, the caller's
// context and frame's registers; then to where the call returns, in
// callLink. The caller stores the result's pointer -- in its keep cell,
// and, a super()'s, its This -- with no write barrier, the collector
// marking or not, for the callee may have called Go and the collector
// begun marking meanwhile, or Go have finished the callee (resumeCall):
// the result is a source's, or Go's, written with its barrier (RetValue),
// and the cell's old value one nothing native reads again -- a value live
// across the call is copied out of its cell first (ssa's keep.go).
func (c *compiler) returnTail() {
	c.a.Load(regStack, regCtx, abi.OffStackTop)
	c.a.Load(regLocals, regCtx, abi.OffBase)
	c.a.Store(regStack, 0, regLocals)
	c.a.StoreImm(regCtx, abi.OffLive, 0)
	c.a.StoreImm(regCtx, abi.OffReturnTo, 0)
	c.a.Load(regCtx, regCtx, abi.OffPrev)
	c.a.Load(regLocals, regCtx, abi.OffLocals)
	c.a.Load(regStack, regCtx, abi.OffStack)
	c.a.JmpReg(callLink)
}

// resumeCall is where Go goes on in the code after a native call whose
// callee it finished (jit's SSACode.Resume), in the callee's context: the
// result from its RetValue, and on as a callee's return goes on, to its
// ReturnTo.
func (c *compiler) resumeCall() {
	c.a.Load(callLink, regCtx, abi.OffReturnTo)
	c.a.Load(retNum, regCtx, abi.OffRetValue+c.enc.NumOffset)
	c.a.Load(retRef, regCtx, abi.OffRetValue+c.enc.RefOffset)
	c.returnTail()
}

// callEntry is the function's call entry, where its native callers jump
// (call), which makes its frame and its context, as V8's callee makes its
// own: the context the caller's next, which a call of the function runs
// in; the frame past the VM's stack's top, which the caller wrote the
// arguments to, the stack's top and high water past it; where the call
// returns to and the callee's closure, in callRet and callClosure, and the
// closure's captured bindings; the call counted in the function's link,
// in callLink; and the locals past the parameters undefined, unless the
// start does it (ssa.Func's StartOnly). It goes on at the start, laid out
// next. It uses every register.
func (c *compiler) callEntry() {
	vs := int32(c.enc.ValueSize)
	const base, at, top = amd64.R8, amd64.R9, amd64.R10
	c.a.Load(regCtx, regCtx, abi.OffNext)
	c.a.Store(regCtx, abi.OffReturnTo, callRet)
	c.a.Store(regCtx, abi.OffClosure, callClosure)
	c.a.Load(callClosure, callClosure, c.enc.ClosureUpvalues)
	c.a.Store(regCtx, abi.OffUpvalues, callClosure)
	c.a.Load(at, regCtx, abi.OffStackTop)
	c.a.Load(base, at, 0)
	c.a.Store(regCtx, abi.OffBase, base)
	c.a.Lea(top, base, int32(c.frameSize()))
	c.a.Store(at, 0, top)
	same := c.a.NewLabel()
	c.a.Load(at, regCtx, abi.OffStackHigh)
	c.a.CmpMem(top, at, 0, true)
	c.a.Jcc(amd64.CondBE, same)
	c.a.Store(at, 0, top)
	c.a.Bind(same)
	c.a.ShiftImm(amd64.Shl, base, 4, true)
	c.a.Load(at, regCtx, abi.OffStackBase)
	c.a.Op(amd64.Add, base, at, true)
	c.a.Store(regCtx, abi.OffLocals, base)
	c.a.Lea(at, base, int32(c.f.FrameLocals)*vs)
	c.a.Store(regCtx, abi.OffStack, at)
	c.a.StoreImm(regCtx, abi.OffOpen, 0)
	// No cells of its captured locals until its start makes them
	// (ssa.GoCells), whatever ran in the context before: a pointer cleared
	// without a write barrier, as the closure's is stored, an earlier
	// call's, which nothing reads again (call).
	c.a.StoreImm(regCtx, abi.OffCells, 0)
	if c.f.Loops {
		// No loop of a built-in's call under way (abi.Context's Loop).
		c.storeImm(regCtx, abi.OffLoop, 0, scratchA)
	}
	c.a.StoreImm(regCtx, abi.OffLive, abi.LiveCall)
	c.a.IncMem(callLink, c.enc.LinkIn)
	if !c.f.StartOnly && c.f.Params < c.f.FrameLocals {
		c.movImm(scratchA, c.enc.Undefined)
		c.movImm(scratchB, 0)
		for i := c.f.Params; i < c.f.FrameLocals; i++ {
			c.a.Store(base, int32(i)*vs+c.enc.NumOffset, scratchA)
			c.a.Store(base, int32(i)*vs+c.enc.RefOffset, scratchB)
		}
	}
}

// call calls natively the function a call calls, if it is one of those it
// was seen to call (ssa's OpCall), as V8's code calls another's, and goes
// on after it with the result: the callee's frame is made in the VM's
// stack and its context is the next one; it returns here (returnNative).
// Nothing is written to this frame: what is used after the call is in
// memory, spilled (allocate), and kept, if read from a cell (ssa's
// keep.go). The call's state is recorded instead, each slot that is not
// the frame's as it is -- its number word, and its pointer word read where
// it came from (abi.RecordDirect) -- for Go to make the frame from if the
// callee leaves native code (abi.Context.Live); the callee's arguments are
// copied from there. Which function it calls is looked for twice: before,
// among what each needs, and after, from the callee's record. Anything
// that does not allow the call fails the guard: Go makes it.
func (c *compiler) call(v *ssa.Value, guard func(amd64.Cond)) {
	sites, s := v.Calls(), v.State
	if len(sites) == 0 {
		c.a.Jmp(c.stubLabel(s, exitKind(v.Aux)))
		return
	}
	c.callV, c.callTop = v, c.a.NewLabel()
	c.a.Bind(c.callTop)
	defer func() { c.callV = nil }()
	site := sites[0]
	vs := int32(c.enc.ValueSize)
	if site.Go != 0 {
		c.goCall(v, site, guard)
		return
	}
	if site.Literal {
		// An object literal's object: its pool's next, as a built-in's
		// construction takes it (Alloc) -- its pool's cell cleared with Go's
		// write barrier while the collector marks (markingTake).
		c.movImm(scratchA, uint64(site.Pool))
		c.a.Load(scratchB, scratchA, abi.OffPoolCount)
		c.refillOnEmpty(site.Pool, guard)
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
		// An accessor's getter or setter (ssa.AccessorSite): no function
		// among the operands, which are the receiver and a setter's value
		// -- the guards before the call found it is Callee's. A super()'s
		// (ssa.CallSite's Super): its arguments the operands on the stack,
		// its parent found by the running function's prototype
		// (superGuards).
		calleeSlot = sp - site.Argc
	} else {
		callee = v.Args[len(v.Args)-site.Argc-1]
	}
	join, generic := amd64.Label(0), false
	if !site.Alloc && !site.Receiver {
		// Natively, the collector marking or not: what the call writes --
		// the records, the callee's context and frame -- outlives none of
		// what it is written from, and overwrites what nothing reads again;
		// the result, which outlives the callee's frame it comes from, is
		// stored with Go's barrier while the collector marks (below), as is
		// what a push or a pop moves (push, pop). A receiver taken from a
		// pool is shaded first (poolShade). A call that does not make it
		// natively Go makes, where the code may call Go (join).
		if c.enc.CallGo != 0 && len(v.Args) <= abi.MaxGoArgs {
			join, generic = c.a.NewLabel(), true
		}
		c.poolShade(sites)
		if generic {
			defer c.a.Bind(join)
		}
	}
	// A context.
	c.a.Load(scratchB, regCtx, abi.OffLevel)
	c.a.OpImm(amd64.Add, scratchB, 1, true)
	c.a.Load(scratchC, regCtx, abi.OffBounds)
	c.a.Load(scratchC, scratchC, abi.OffBoundsLevelLimit)
	c.a.Op(amd64.Cmp, scratchB, scratchC, true)
	guard(amd64.CondAE)
	// The callee is one of the functions, which has native code. A call
	// through Function.prototype.call is made of its one function, its
	// operands written where the callee has them (directOperand).
	if callee == nil {
		c.movImm(scratchC, uint64(site.Callee))
	} else {
		if callee.Shadow == nil && c.origin.At(callee) < 0 || site.Via != 0 && (len(sites) != 1 || !c.operandsLive(s, calleeSlot-1)) {
			c.a.Jmp(c.stubLabel(s, exitKind(v.Aux)))
			return
		}
		c.cmpWord(c.gpr(callee, scratchA), c.enc.Object)
		guard(amd64.CondNE)
		c.sourceRef(callee, guard)
	}
	checked := c.a.NewLabel()
	// Past its sites, any other function, through its link
	// (genericCheck).
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
			c.cmpWord(scratchC, uint64(want))
			c.a.Jcc(amd64.CondNE, next)
		}
		if len(sites) > 1 || gen {
			// Which one, for its frame to be made for (abi.Context's
			// CallTarget): a closure checked by its code is no one object.
			c.a.StoreImm(regCtx, abi.OffCallTarget, int32(k))
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
			// Array.prototype.pop, its fast path's.
			c.popGuards(t, v.Args[0], guard)
			c.a.Jmp(checked)
			c.a.Bind(next)
			continue
		}
		if t.Push {
			// Array.prototype.push, its fast path's.
			c.pushGuards(t, v.Args[0], guard)
			c.a.Jmp(checked)
			c.a.Bind(next)
			continue
		}
		if t.Alloc || t.Receiver {
			// A built-in's construction with nothing to run: its object;
			// or an inlined construction's receiver.
			c.constructGuards(t, guard)
			if t.Alloc && t.Argc == 1 {
				c.lengthGuards(t, v.Args[len(v.Args)-1], guard)
			}
			c.a.Jmp(checked)
			c.a.Bind(next)
			continue
		}
		// No code native callers may call (abi.Context's nativeEntry 0) is
		// no reason to leave: Go runs the callee once its frame is made
		// (enterExit).
		if t.Coerce {
			// A receiver that is not an object is coerced, which Go does:
			// the method call's, or the first argument of a call through
			// Function.prototype.call.
			recv := v.Args[0]
			if t.Via != 0 {
				recv = v.Args[2]
			}
			c.cmpWord(c.gpr(recv, scratchA), c.enc.Object)
			guard(amd64.CondNE)
		}
		// Room in the VM's stack.
		c.a.Load(scratchB, regCtx, abi.OffStackTop)
		c.a.Load(scratchB, scratchB, 0)
		c.a.OpImm(amd64.Add, scratchB, int32(t.LocalCount+t.MaxStack), true)
		c.a.Load(scratchA, regCtx, abi.OffStackEnd)
		c.a.Op(amd64.Cmp, scratchB, scratchA, true)
		guard(amd64.CondA)
		if t.Parent != 0 {
			// A derived class's, made with its parent's code: the parent
			// super() would read, the callee's prototype, still that one.
			c.a.Load(scratchB, scratchC, c.enc.ObjectProto)
			c.cmpWord(scratchB, uint64(t.Parent))
			guard(amd64.CondNE)
		}
		if t.Pool != 0 {
			c.constructGuards(t, guard)
		}
		c.a.Jmp(checked)
		c.a.Bind(next)
	}
	if gen {
		// A function none of them is, at a call that has met as many as it
		// calls natively: called natively all the same, through its link
		// (genericCheck), as V8's code makes a call its feedback has seen
		// too many functions at through the code the function points to;
		// or, one a generic call does not make, by Go from native code
		// (ssa.GoCall). Each had left native code: Vector's forEach, given
		// more callbacks than jitCallTargets in AWFY's DeltaBlue, at each
		// call of them. A miss at any other call leaves, as before: Go
		// learns there what to call natively, and a call Go made in its
		// place had AWFY's List run a tenth slower.
		miss := c.a.NewLabel()
		c.genericCheck(v, site, len(sites), checked, miss)
		c.a.Bind(miss)
		cold, op := c.a.NewLabel(), goCallWord(site)
		c.a.Jmp(cold)
		c.cold = append(c.cold, func() {
			c.a.Bind(cold)
			c.goCallOp(v, op, guard, true)
			c.a.Jmp(join)
		})
	} else {
		c.a.Jmp(c.stubLabel(s, exitKind(v.Aux)))
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
		// Its checks made, nothing written: while the collector marks, its
		// pool's cell cleared with Go's write barrier.
		join := c.markingTake(v, site)
		c.poolTake(v, site)
		c.a.Bind(join)
		if site.Alloc && site.Argc == 1 {
			c.allocLength(v, v.Args[len(v.Args)-1])
		}
		return
	}
	// A call of one function has its operands -- the function, the
	// receiver, the arguments -- written straight where the callee has
	// them (directOperand), not recorded: Go never reads them, as a callee
	// that leaves native code is finished from its own frame, and its
	// result replaces them (the VM's jitCallResult).
	direct, ops := len(sites) == 1 && !gen && !site.ArrowThis && (site.ThisSlot < 0 || site.Method || site.Pool != 0 || site.Super || site.PlainThis), calleeSlot
	if site.Method {
		ops--
	}
	direct = direct && c.operandsLive(s, ops)
	// The state, described (callState): every slot the frame does not hold
	// as it is, for Go to make the frame from if the callee leaves; the
	// call writes only the words of its records the caller's code keeps
	// nowhere else -- all of a call's operands' it does not write straight
	// where the callee has them, which it copies from there (words) -- and
	// the description's address.
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
			c.a.Store(regCtx, at+16, scratchA)
			c.a.Store(regCtx, abi.OffRecordRef+int32(k)*8, scratchC)
			continue
		}
		w := scratchA
		switch {
		case !r.num && (!r.ref || r.x.Shadow != nil):
			// Its pointer word from its source, which needs no number word.
		case remat(r.x):
			c.materialize(r.x, scratchA)
		default:
			w = c.gpr(r.x, scratchA)
		}
		if r.num {
			c.a.Store(regCtx, at+16, w)
		}
		if r.ref {
			c.pointerWord(r.x, w)
			c.a.Store(regCtx, abi.OffRecordRef+int32(k)*8, scratchB)
		}
	}
	c.movImm(scratchA, uint64(uintptr(unsafe.Pointer(st))))
	c.a.Store(regCtx, abi.OffCallState, scratchA)
	args, this := sp-site.Argc, calleeSlot-1
	if site.Via != 0 {
		// Through Function.prototype.call: the first argument is the
		// receiver.
		args, this = args+1, args
	}
	// A receiver written straight into the callee's context: one it reads,
	// or a method call's, which a callee that does not read it has too --
	// a frame Go makes for it (jitGoCallNested, jitUnwindNative) shows it
	// in a stack trace, as the interpreter's does.
	receiver := direct && site.Pool == 0 && !site.Super && !site.PlainThis && (site.ThisSlot >= 0 || site.Method && this < sp)
	if direct {
		// The callee's frame, past the VM's stack's top: xScratch2 holds
		// its address, which pointerWord does not use.
		c.a.Load(scratchA, regCtx, abi.OffStackTop)
		c.a.Load(scratchA, scratchA, 0)
		c.a.ShiftImm(amd64.Shl, scratchA, 4, true)
		c.a.Load(scratchB, regCtx, abi.OffStackBase)
		c.a.Op(amd64.Add, scratchA, scratchB, true)
		c.a.MovQToX(xScratch2, scratchA)
		for i := 0; i < site.Params && args+i < sp && i < site.LocalCount; i++ {
			c.directOperand(s.Slots[args+i], true, int32(i)*vs)
		}
		if receiver {
			c.directOperand(s.Slots[this], false, abi.OffThis)
		}
	}
	// A slot's words: its record's, or the frame's, which holds it still.
	words := func(slot int) (nb amd64.Reg, nd int32, rb amd64.Reg, rd int32) {
		if k, ok := record[slot]; ok {
			return regCtx, abi.OffRecord + k*32 + 16, regCtx, abi.OffRecordRef + k*8
		}
		nb, nd = c.slotAddr(slot, false, scratchC)
		rb, rd = c.slotAddr(slot, true, scratchC)
		return nb, nd, rb, rd
	}
	// What is live across the call, in registers, is saved; nothing else
	// is in a register from here on but what the call makes.
	for _, sv := range c.savesOf(v) {
		if sv.float {
			c.a.StoreSD(regCtx, c.spillDisp(sv.slot), amd64.XReg(sv.reg))
		} else {
			c.a.Store(regCtx, c.spillDisp(sv.slot), amd64.Reg(sv.reg))
		}
	}
	// The callee makes its own frame and context at its call entry
	// (callEntry): the call writes what only it knows -- the arguments not
	// written yet, the receiver, a construction's, whether a call in tail
	// position may give the frame up -- and jumps there, with where it
	// returns to, the callee's closure and its link (abi.Context's
	// ReturnTo). A callee with no call entry (its link's entry 0: one not
	// compiled yet, or that native callers no longer call, the VM's
	// notNative) Go makes: called from native code, in code that calls Go,
	// native code going on after it at join -- entered from Go at its
	// start instead, the callee
	// had Go make the frame of every native level below it and finish each,
	// a call a few levels deep costing what all of them do; otherwise, and
	// for a generic call's, once its frame is made (enterExit).
	const calleeCtx, locals, tmp, top, high2 = amd64.R8, amd64.R10, amd64.RBX, amd64.R12, amd64.R13
	back := c.a.NewLabel()
	c.resumeUsed = true
	op := goCallWord(site)
	enter := func() {
		if !c.enterUsed {
			c.enter, c.enterUsed = c.a.NewLabel(), true
		}
		c.a.Jmp(c.enter)
	}
	// frame has locals be the callee's frame's address, past the VM's
	// stack's top.
	frame := func() {
		if direct {
			c.a.MovQFromX(locals, xScratch2)
			return
		}
		c.a.Load(locals, regCtx, abi.OffStackTop)
		c.a.Load(locals, locals, 0)
		c.a.ShiftImm(amd64.Shl, locals, 4, true)
		c.a.Load(tmp, regCtx, abi.OffStackBase)
		c.a.Op(amd64.Add, locals, tmp, true)
	}
	// setup writes what the call of t knows of its callee's frame and
	// context. It keeps callLink, the callee's link.
	setup := func(t *ssa.CallSite) {
		framed := false
		// The arguments, its parameters', past the receiver of a call
		// through Function.prototype.call; undefined in the others.
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
			c.storeImm(locals, at+c.enc.NumOffset, c.enc.Undefined, top)
			c.storeImm(locals, at+c.enc.RefOffset, 0, top)
		}
		if t.Tail || t.Pool != 0 || t.Super || t.ArrowThis || t.PlainThis || (t.ThisSlot >= 0 || site.Method && site.Via == 0) && !direct || !receiver {
			c.a.Load(calleeCtx, regCtx, abi.OffNext)
		}
		if t.Tail {
			// Whether its calls in tail position may give its frame up;
			// its frame Go makes, from its context, if it leaves.
			c.storeImm(calleeCtx, abi.OffTailFrame, b2u(t.TailFrame), top)
			c.storeImm(calleeCtx, abi.OffFrameThis, 0, top)
		}
		if t.Pool != 0 {
			// The receiver: the pool's last object, its cell cleared.
			c.movImm(tmp, uint64(t.Pool))
			c.a.Load(top, tmp, abi.OffPoolCount)
			c.a.OpImm(amd64.Sub, top, 1, true)
			c.a.Store(tmp, abi.OffPoolCount, top)
			c.a.ShiftImm(amd64.Shl, top, 3, true)
			c.a.Op(amd64.Add, top, tmp, true)
			c.a.Load(high2, top, abi.OffPoolObjects)
			c.storeImm(top, abi.OffPoolObjects, 0, tmp)
			if t.Derived {
				// A derived constructor's: its instance, its this unbound
				// until its super() binds it, and the function whose code
				// it is, whose parent its super() constructs with.
				c.a.Store(calleeCtx, abi.OffInstance+c.enc.RefOffset, high2)
				c.storeImm(calleeCtx, abi.OffInstance+c.enc.NumOffset, c.enc.Object, tmp)
				c.storeImm(calleeCtx, abi.OffThis+c.enc.RefOffset, 0, tmp)
				c.storeImm(calleeCtx, abi.OffThis+c.enc.NumOffset, c.enc.Uninitialized, tmp)
				c.storeImm(calleeCtx, abi.OffFunction, uint64(derivedFunction(t)), tmp)
			} else {
				c.a.Store(calleeCtx, abi.OffThis+c.enc.RefOffset, high2)
				c.storeImm(calleeCtx, abi.OffThis+c.enc.NumOffset, c.enc.Object, tmp)
			}
			// A construction's new.target, for Go to make its frame with.
			c.storeImm(calleeCtx, abi.OffNewTarget, uint64(t.Callee), top)
		} else if t.Super {
			// A super()'s: the instance its construction made, the
			// parent's receiver -- a derived parent's to bind, its this
			// unbound -- new.target handed on, and the function whose
			// code runs, the parent or the class it runs the code of.
			c.a.Load(top, regCtx, abi.OffInstance+c.enc.RefOffset)
			if t.Derived {
				c.a.Store(calleeCtx, abi.OffInstance+c.enc.RefOffset, top)
				c.storeImm(calleeCtx, abi.OffInstance+c.enc.NumOffset, c.enc.Object, tmp)
				c.storeImm(calleeCtx, abi.OffThis+c.enc.RefOffset, 0, tmp)
				c.storeImm(calleeCtx, abi.OffThis+c.enc.NumOffset, c.enc.Uninitialized, tmp)
			} else {
				c.a.Store(calleeCtx, abi.OffThis+c.enc.RefOffset, top)
				c.storeImm(calleeCtx, abi.OffThis+c.enc.NumOffset, c.enc.Object, tmp)
			}
			c.a.Load(top, regCtx, abi.OffNewTarget)
			c.a.Store(calleeCtx, abi.OffNewTarget, top)
			c.storeImm(calleeCtx, abi.OffFunction, uint64(derivedFunction(t)), top)
		} else if t.ArrowThis {
			// The this it captured (arrowThis).
			c.a.Load(high2, regCtx, abi.OffCallThis)
			c.a.Load(top, high2, c.enc.NumOffset)
			c.a.Store(calleeCtx, abi.OffThis+c.enc.NumOffset, top)
			c.a.Load(top, high2, c.enc.RefOffset)
			c.a.Store(calleeCtx, abi.OffThis+c.enc.RefOffset, top)
		} else if t.PlainThis && t.Global != 0 {
			// A sloppy callee's, called plainly: the global object.
			c.storeImm(calleeCtx, abi.OffThis+c.enc.NumOffset, c.enc.Object, top)
			c.storeImm(calleeCtx, abi.OffThis+c.enc.RefOffset, uint64(t.Global), top)
		} else if t.PlainThis {
			// A strict callee's, called plainly: undefined.
			c.storeImm(calleeCtx, abi.OffThis+c.enc.NumOffset, c.enc.Undefined, top)
			c.storeImm(calleeCtx, abi.OffThis+c.enc.RefOffset, 0, top)
		} else if (t.ThisSlot >= 0 || site.Method && site.Via == 0) && !direct {
			nb, nd, rb, rd := words(calleeSlot - 1)
			c.a.Load(top, nb, nd)
			c.a.Store(calleeCtx, abi.OffThis+c.enc.NumOffset, top)
			c.a.Load(top, rb, rd)
			c.a.Store(calleeCtx, abi.OffThis+c.enc.RefOffset, top)
		} else if !receiver {
			// A plain call's receiver, undefined, as the interpreter's frame
			// has it.
			c.storeImm(calleeCtx, abi.OffThis+c.enc.NumOffset, c.enc.Undefined, top)
			c.storeImm(calleeCtx, abi.OffThis+c.enc.RefOffset, 0, top)
		}
		// The closure the checks found, or t's.
		if t.ByCode != 0 {
			c.a.Load(callClosure, regCtx, abi.OffCallClosure)
		} else {
			c.movImm(callClosure, uint64(t.Closure))
		}
		c.a.LeaLabel(callRet, back)
	}
	// Which one the checks found (abi.Context's CallTarget).
	for i, t := range sites {
		next := c.a.NewLabel()
		if i < len(sites)-1 || gen {
			c.a.Load(scratchA, regCtx, abi.OffCallTarget)
			c.a.OpImm(amd64.Cmp, scratchA, int32(i), true)
			c.a.Jcc(amd64.CondNE, next)
		}
		if t.Entry == 0 {
			// Not a function's (push, pop, a construction with nothing to
			// run), which the checks do not send here.
			c.a.Jmp(c.stubLabel(s, exitKind(v.Aux)))
			c.a.Bind(next)
			continue
		}
		none := c.a.NewLabel()
		c.movImm(callLink, uint64(t.Entry)-uint64(c.enc.LinkEntry))
		c.a.Load(scratchA, callLink, c.enc.LinkEntry)
		c.a.Op(amd64.Test, scratchA, scratchA, true)
		c.a.Jcc(amd64.CondE, none)
		setup(t)
		c.a.JmpMem(callLink, c.enc.LinkEntry)
		c.cold = append(c.cold, func() {
			c.a.Bind(none)
			if generic {
				c.goCallOp(v, op, guard, true)
				c.a.Jmp(join)
				return
			}
			setup(t)
			if t.ByCode != 0 {
				c.a.Load(callObject, regCtx, abi.OffCallCallee)
			} else {
				c.movImm(callObject, uint64(t.Callee))
			}
			enter()
		})
		c.a.Bind(next)
	}
	if gen {
		// A function none of them is (genericCheck): its frame made as its
		// link says, read from its link at run time.
		setupGen := func() {
			frame()
			c.a.Load(calleeCtx, regCtx, abi.OffNext)
			notail := c.a.NewLabel()
			c.a.Load(top, callLink, c.enc.LinkFlags)
			c.a.OpImm(amd64.And, top, abi.LinkTail, true)
			c.a.Jcc(amd64.CondE, notail)
			// No construction's (abi.LinkNotGeneric), nor an arrow's that
			// shares a derived constructor's this (genericCheck): its frame is
			// one a call in tail position gives up.
			c.storeImm(calleeCtx, abi.OffTailFrame, 1, top)
			c.storeImm(calleeCtx, abi.OffFrameThis, 0, top)
			c.a.Bind(notail)
			// The arguments, its parameters'.
			fill := c.a.NewLabel()
			first := sp - site.Argc
			for i := 0; i < site.Argc; i++ {
				c.a.Load(top, callLink, c.enc.LinkParams)
				c.a.OpImm(amd64.Cmp, top, int32(i), true)
				c.a.Jcc(amd64.CondBE, fill)
				nb, nd, rb, rd := words(first + i)
				c.a.Load(top, nb, nd)
				c.a.Store(locals, int32(i)*vs+c.enc.NumOffset, top)
				c.a.Load(top, rb, rd)
				c.a.Store(locals, int32(i)*vs+c.enc.RefOffset, top)
			}
			c.a.Bind(fill)
			// Undefined in its other parameters, from the arguments on; its
			// call entry makes its other locals undefined.
			loop, filled := c.a.NewLabel(), c.a.NewLabel()
			c.movImm(scratchA, uint64(site.Argc))
			c.a.Bind(loop)
			c.a.Load(tmp, callLink, c.enc.LinkParams)
			c.a.Op(amd64.Cmp, scratchA, tmp, true)
			c.a.Jcc(amd64.CondAE, filled)
			c.a.MovRR(top, scratchA)
			c.a.ShiftImm(amd64.Shl, top, 4, true)
			c.a.Op(amd64.Add, top, locals, true)
			c.storeImm(top, c.enc.NumOffset, c.enc.Undefined, high2)
			c.storeImm(top, c.enc.RefOffset, 0, high2)
			c.a.OpImm(amd64.Add, scratchA, 1, true)
			c.a.Jmp(loop)
			c.a.Bind(filled)
			// A method call's receiver, which its body may read; undefined for
			// a plain call's, a function that would coerce it not called so
			// (genericCheck); an arrow's, the this it captured.
			arrow, thisDone := c.a.NewLabel(), c.a.NewLabel()
			c.a.Load(top, callLink, c.enc.LinkFlags)
			c.a.OpImm(amd64.And, top, abi.LinkArrow, true)
			c.a.Jcc(amd64.CondNE, arrow)
			if site.Method {
				nb, nd, rb, rd := words(calleeSlot - 1)
				c.a.Load(top, nb, nd)
				c.a.Store(calleeCtx, abi.OffThis+c.enc.NumOffset, top)
				c.a.Load(top, rb, rd)
				c.a.Store(calleeCtx, abi.OffThis+c.enc.RefOffset, top)
			} else {
				c.storeImm(calleeCtx, abi.OffThis+c.enc.NumOffset, c.enc.Undefined, top)
				c.storeImm(calleeCtx, abi.OffThis+c.enc.RefOffset, 0, top)
			}
			c.a.Jmp(thisDone)
			c.a.Bind(arrow)
			c.a.Load(tmp, regCtx, abi.OffCallThis)
			c.a.Load(top, tmp, c.enc.NumOffset)
			c.a.Store(calleeCtx, abi.OffThis+c.enc.NumOffset, top)
			c.a.Load(top, tmp, c.enc.RefOffset)
			c.a.Store(calleeCtx, abi.OffThis+c.enc.RefOffset, top)
			c.a.Bind(thisDone)
			c.a.Load(callClosure, regCtx, abi.OffCallClosure)
			c.a.LeaLabel(callRet, back)
		}
		none := c.a.NewLabel()
		c.a.Load(callLink, regCtx, abi.OffCallLink)
		c.a.Load(scratchA, callLink, c.enc.LinkEntry)
		c.a.Op(amd64.Test, scratchA, scratchA, true)
		c.a.Jcc(amd64.CondE, none)
		setupGen()
		c.a.JmpMem(callLink, c.enc.LinkEntry)
		c.cold = append(c.cold, func() {
			c.a.Bind(none)
			setupGen()
			c.a.Load(callObject, regCtx, abi.OffCallCallee)
			enter()
		})
	}
	// The callee returned (returnNative), its result's words in retNum and
	// retRef: v, its pointer word kept (OpCallCell).
	c.a.Bind(back)
	at := abi.OffKeep + int32(v.Index)*vs
	if site.Pool != 0 || site.Super {
		// A construction's result, if not an object, is its receiver: a
		// super()'s, the instance, a base parent's receiver.
		object := c.a.NewLabel()
		c.movImm(scratchB, c.enc.Object)
		c.a.Op(amd64.Cmp, retNum, scratchB, true)
		c.a.Jcc(amd64.CondE, object)
		c.a.MovRR(retNum, scratchB)
		c.a.Load(retRef, regCtx, abi.OffNext)
		c.a.Load(retRef, retRef, abi.OffThis+c.enc.RefOffset)
		c.a.Bind(object)
	}
	// While the collector marks, the result's pointer word is stored with
	// Go's write barrier (refStore): the keep cell, and a super()'s This,
	// outlive the callee's frame the result comes from, which the next
	// native call overwrites with none.
	marked, stored := c.a.NewLabel(), c.a.NewLabel()
	c.movImm(scratchB, c.enc.WriteBarrier)
	c.a.LoadU8(scratchB, scratchB, 0)
	c.a.Op(amd64.Test, scratchB, scratchB, false)
	c.a.Jcc(amd64.CondNE, marked)
	if site.Super {
		// The this it binds, the context's This, which an exit does not
		// write (the VM's jitDerivedFrame, jitBindThis).
		c.a.Store(regCtx, abi.OffThis+c.enc.RefOffset, retRef)
		c.a.Store(regCtx, abi.OffThis+c.enc.NumOffset, retNum)
	}
	c.a.Store(regCtx, at+c.enc.RefOffset, retRef)
	c.a.Store(regCtx, at+c.enc.NumOffset, retNum)
	c.a.Bind(stored)
	c.cold = append(c.cold, func() {
		// The number word first, read back after: refStore keeps no
		// scratch register.
		c.a.Bind(marked)
		c.a.Store(regCtx, at+c.enc.NumOffset, retNum)
		c.a.Lea(scratchA, regCtx, at+c.enc.RefOffset)
		c.refStore()
		if site.Super {
			c.a.Load(scratchA, regCtx, at+c.enc.NumOffset)
			c.a.Store(regCtx, abi.OffThis+c.enc.NumOffset, scratchA)
			c.a.Load(scratchC, regCtx, at+c.enc.RefOffset)
			c.a.Lea(scratchA, regCtx, abi.OffThis+c.enc.RefOffset)
			c.refStore()
		}
		c.a.Load(retNum, regCtx, at+c.enc.NumOffset)
		c.a.Jmp(stored)
	})
	for _, sv := range c.savesOf(v) {
		if sv.float {
			c.a.LoadSD(amd64.XReg(sv.reg), regCtx, c.spillDisp(sv.slot))
		} else {
			c.a.Load(amd64.Reg(sv.reg), regCtx, c.spillDisp(sv.slot))
		}
	}
	c.setG(v, retNum)
}

// goCall calls Go (ssa.CallSite's Go), as V8's code calls a runtime
// function: the operands' words to the context's GoArgs, as integers -- no
// pointer is stored, so the collector may mark meanwhile; each operand's
// source still holds it -- and what is live in registers saved, it jumps
// to callGo (abi.Encoding's CallGo), which comes back to back. Go wrote
// the result to the call's keep cell (OpCallCell); if it did nothing, the
// guard fails, and Go makes the operation. Go may have done anything, the
// collector included: the frame's registers are read again, and every
// store after it tests the collector's flag, as every store does.
func (c *compiler) goCall(v *ssa.Value, site *ssa.CallSite, guard func(amd64.Cond)) {
	if site.Step && c.enc.IterState != 0 {
		// A loop's step over an array native code makes itself; any other
		// Go makes.
		slow, join := c.a.NewLabel(), c.a.NewLabel()
		c.iterStep(v, slow)
		c.a.Jmp(join)
		c.a.Bind(slow)
		c.goCallOp(v, goOpWord(site), guard, false)
		c.a.Bind(join)
		c.a.Load(scratchA, regCtx, abi.OffKeep+int32(v.Index)*int32(c.enc.ValueSize)+c.enc.NumOffset)
		c.setG(v, scratchA)
		return
	}
	if site.Go == ssa.GoKeyedWrite && len(v.Args) == 3 && c.enc.ValueSize == 16 && c.enc.WriteBarrier != 0 {
		// An array's element in its dense elements natively (keyedStore),
		// any other store Go's.
		slow, join := c.a.NewLabel(), c.a.NewLabel()
		c.keyedStore(v, slow)
		c.a.Jmp(join)
		c.a.Bind(slow)
		c.goCallOp(v, goOpWord(site), guard, false)
		c.a.Bind(join)
		c.a.Load(scratchA, regCtx, abi.OffKeep+int32(v.Index)*int32(c.enc.ValueSize)+c.enc.NumOffset)
		c.setG(v, scratchA)
		return
	}
	if lengthWrites(site, len(v.Args), c.enc) {
		// An array's length, natively where it shortens a dense array or
		// leaves it as it is (lengthWrite), by Go otherwise.
		slow, join := c.a.NewLabel(), c.a.NewLabel()
		c.lengthWrite(v, slow)
		c.a.Jmp(join)
		c.a.Bind(slow)
		c.goCallOp(v, goOpWord(site), guard, false)
		c.a.Bind(join)
		c.a.Load(scratchA, regCtx, abi.OffKeep+int32(v.Index)*int32(c.enc.ValueSize)+c.enc.NumOffset)
		c.setG(v, scratchA)
		return
	}
	if site.Go == ssa.GoClosure && site.Pool != 0 {
		// Natively from its pool where it can be (closureTake), by Go
		// otherwise, which fills the pool again.
		slow, join := c.a.NewLabel(), c.a.NewLabel()
		c.closureTake(v, site, slow)
		c.a.Jmp(join)
		c.a.Bind(slow)
		c.storeImm(regCtx, abi.OffGoArgs+c.enc.RefOffset, uint64(site.Pool), scratchA)
		c.goCallOp(v, goOpWord(site), guard, false)
		c.a.Bind(join)
		return
	}
	if site.Go == ssa.GoCells && site.Pool != 0 {
		// Natively from the function's pool where it can be (cellsTake),
		// by Go otherwise, which fills the pool again.
		slow, join := c.a.NewLabel(), c.a.NewLabel()
		c.cellsTake(site, slow)
		c.a.Load(scratchA, regCtx, abi.OffKeep+int32(v.Index)*int32(c.enc.ValueSize)+c.enc.NumOffset)
		c.setG(v, scratchA)
		c.a.Jmp(join)
		c.a.Bind(slow)
		c.storeImm(regCtx, abi.OffGoArgs+c.enc.RefOffset, uint64(site.Pool), scratchA)
		c.goCallOp(v, goOpWord(site), guard, false)
		c.a.Bind(join)
		return
	}
	if site.Go == ssa.GoClose && site.Pool != 0 {
		// Natively from the function's pool of cells where it can be
		// (cellsClose), by Go otherwise, which fills the pool again.
		slow, join := c.a.NewLabel(), c.a.NewLabel()
		c.cellsClose(site, slow)
		c.a.Jmp(join)
		c.a.Bind(slow)
		c.storeImm(regCtx, abi.OffGoArgs+c.enc.RefOffset, uint64(site.Pool), scratchA)
		c.goCallOp(v, goOpWord(site), guard, false)
		c.a.Bind(join)
		return
	}
	if site.Go == ssa.GoClose && !c.f.Cells {
		// Only while closures have captured the frame's locals
		// (abi.Context's Open): its result nothing reads. Code whose
		// captured locals are in cells of their own gives them fresh
		// ones at every scope's end, Open or not: a native callee's
		// closures capture cells no frame has open.
		skip := c.a.NewLabel()
		c.a.Load(scratchA, regCtx, abi.OffOpen)
		c.a.Op(amd64.Test, scratchA, scratchA, true)
		c.a.Jcc(amd64.CondE, skip)
		c.goCallOp(v, goOpWord(site), guard, false)
		c.a.Bind(skip)
		return
	}
	if site.Big && c.enc.BigFast != 0 {
		// Through the VM's routine for BigInts (abi.Encoding's BigFast).
		c.goCallBig = true
		defer func() { c.goCallBig = false }()
	}
	c.goCallOp(v, goOpWord(site), guard, false)
}

// superGuards checks a super() may call t's parent natively
// (ssa.CallSite's Super): the function running is the one the code was
// compiled for (abi.Context's Function), its prototype -- the parent, read
// at every super(), as the VM's parentConstructorOf reads it -- still
// t's Callee, and, a class with no constructor of its own skipped, its
// prototype still Parent; and the this, x, unbound: a second super() Go
// makes, which runs the parent and then throws. It uses scratchA and
// scratchB.
func (c *compiler) superGuards(t *ssa.CallSite, x *ssa.Value, guard func(amd64.Cond)) {
	c.a.Load(scratchA, regCtx, abi.OffFunction)
	c.cmpWord(scratchA, uint64(t.Active))
	guard(amd64.CondNE)
	c.a.Load(scratchB, scratchA, c.enc.ObjectProto)
	c.cmpWord(scratchB, uint64(t.Callee))
	guard(amd64.CondNE)
	if t.Parent != 0 {
		c.a.Load(scratchB, scratchB, c.enc.ObjectProto)
		c.cmpWord(scratchB, uint64(t.Parent))
		guard(amd64.CondNE)
	}
	if remat(x) {
		c.materialize(x, scratchA)
		c.cmpWord(scratchA, c.enc.Uninitialized)
	} else {
		c.cmpWord(c.gpr(x, scratchA), c.enc.Uninitialized)
	}
	guard(amd64.CondNE)
}

// closureTake makes call v, a closure Go makes from its pool natively
// (ssa.CallSite's Captures), as V8's code allocates one: where the
// collector does not mark and the pool has one, its next object, kept in the
// call's keep cell as a literal's is, its captured bindings' cells written
// into its closure's, from the context's Cells and Upvalues, and an
// arrow's this into what it captured. Anything else goes to slow, where Go
// makes it. It uses every scratch register.
func (c *compiler) closureTake(v *ssa.Value, site *ssa.CallSite, slow amd64.Label) {
	marked, done := c.a.NewLabel(), c.a.NewLabel()
	c.movImm(scratchA, c.enc.WriteBarrier)
	c.a.LoadU8(scratchA, scratchA, 0)
	c.a.Op(amd64.Test, scratchA, scratchA, false)
	c.a.Jcc(amd64.CondNE, marked)
	c.movImm(scratchA, uint64(site.Pool))
	c.a.Load(scratchB, scratchA, abi.OffPoolCount)
	c.a.Op(amd64.Test, scratchB, scratchB, true)
	c.a.Jcc(amd64.CondE, slow)
	c.cold = append(c.cold, func() {
		c.a.Bind(marked)
		c.closureTakeMarking(v, site, slow)
		c.a.Jmp(done)
	})
	defer c.a.Bind(done)
	c.poolTake(v, site)
	// The object in scratchC: its function data, then its closure's
	// captured bindings' cells.
	c.a.Load(scratchB, scratchC, c.enc.ObjectExt)
	c.a.Load(scratchC, scratchB, c.enc.ObjectData+8)
	c.a.Load(scratchB, scratchC, c.enc.FuncClosure)
	c.a.Load(scratchB, scratchB, c.enc.ClosureUpvalues)
	for k, x := range site.Captures {
		base := abi.OffUpvalues
		if x&1 != 0 {
			base = abi.OffCells
		}
		c.a.Load(scratchA, regCtx, base)
		c.a.Load(scratchA, scratchA, (x>>1)*8)
		c.a.Store(scratchB, int32(k)*8, scratchA)
	}
	if site.LexThis {
		c.a.Load(scratchB, scratchC, c.enc.FuncExtra)
		for w := int32(0); w < c.enc.ValueSize; w += 8 {
			c.a.Load(scratchA, regCtx, abi.OffThis+w)
			c.a.Store(scratchB, c.enc.ExtraLexThis+w, scratchA)
		}
	}
}

// cellsTake makes an entry's cells (ssa.GoCells) natively, as V8's code
// allocates a function's context: where the collector does not mark, the
// context is a native call's (abi.Context's Live) -- the frame Go entered
// the code with has its cells open on its slots, which Go makes -- and
// the pool has a block, its next, linked when Go made it, is the context's
// Cells, each cell holding its argument or undefined (CallSite's
// Captures), as the VM's jitMakeCells makes them. Anything else goes to
// slow, where Go makes them. Each entry of such code had been a call of
// Go: two million a run in AWFY's Havlak, 260,000 in DeltaBlue. It uses
// every scratch register.
func (c *compiler) cellsTake(site *ssa.CallSite, slow amd64.Label) {
	marked, done := c.a.NewLabel(), c.a.NewLabel()
	c.movImm(scratchA, c.enc.WriteBarrier)
	c.a.LoadU8(scratchA, scratchA, 0)
	c.a.Op(amd64.Test, scratchA, scratchA, false)
	c.a.Jcc(amd64.CondNE, marked)
	c.cold = append(c.cold, func() {
		c.a.Bind(marked)
		c.cellsTakeMarking(site, slow)
		c.a.Jmp(done)
	})
	defer c.a.Bind(done)
	c.a.Load(scratchA, regCtx, abi.OffLive)
	c.a.OpImm(amd64.Cmp, scratchA, abi.LiveCall, true)
	c.a.Jcc(amd64.CondNE, slow)
	c.movImm(scratchA, uint64(site.Pool))
	c.a.Load(scratchB, scratchA, abi.OffPoolCount)
	c.a.Op(amd64.Test, scratchB, scratchB, true)
	c.a.Jcc(amd64.CondE, slow)
	// The pool's last block, its cell cleared.
	c.a.OpImm(amd64.Sub, scratchB, 1, true)
	c.a.Store(scratchA, abi.OffPoolCount, scratchB)
	c.a.ShiftImm(amd64.Shl, scratchB, 3, true)
	c.a.Op(amd64.Add, scratchB, scratchA, true)
	c.a.Load(scratchC, scratchB, abi.OffPoolObjects)
	c.storeImm(scratchB, abi.OffPoolObjects, 0, scratchA)
	c.a.Store(regCtx, abi.OffCells, scratchC)
	vs, n := c.enc.ValueSize, int32(len(site.Captures))
	for k, x := range site.Captures {
		at := 8*n + int32(k)*c.enc.UpvalueSize + c.enc.UpvalueClosed
		if x&1 == 0 {
			c.storeImm(scratchC, at+c.enc.NumOffset, c.enc.Undefined, scratchA)
			c.storeImm(scratchC, at+c.enc.RefOffset, 0, scratchA)
			continue
		}
		slot := x >> 1
		c.a.Load(scratchA, regLocals, slot*vs+c.enc.NumOffset)
		c.a.Store(scratchC, at+c.enc.NumOffset, scratchA)
		c.a.Load(scratchA, regLocals, slot*vs+c.enc.RefOffset)
		c.a.Store(scratchC, at+c.enc.RefOffset, scratchA)
	}
}

// cellsClose ends a scope natively (ssa.GoClose) in code whose captured
// locals are in cells of their own, as V8's code allocates a block's
// context at each iteration: where the pool has a block, its next, each
// local from the call's Argc on gets the block's cell of it, holding what
// the old cell holds, in the context's Cells, and the old one is closed
// -- what it held copied into it, its slot pointing there -- as the VM's
// closeUpvaluesFrom closes it. The closures that captured it keep it; the
// next iteration's capture the new one. The frame Go entered the code
// with has its cells open on its slots: one closed so is no longer, and
// Go opens its new one there once native code has left it
// (jitReopenRunCells). Anything else goes to slow, where Go makes them.
// It uses every scratch register, and xScratch0 and xScratch1.
func (c *compiler) cellsClose(site *ssa.CallSite, slow amd64.Label) {
	marked, done := c.a.NewLabel(), c.a.NewLabel()
	c.movImm(scratchA, c.enc.WriteBarrier)
	c.a.LoadU8(scratchA, scratchA, 0)
	c.a.Op(amd64.Test, scratchA, scratchA, false)
	c.a.Jcc(amd64.CondNE, marked)
	c.cold = append(c.cold, func() {
		c.a.Bind(marked)
		c.cellsCloseMarking(site, slow)
		c.a.Jmp(done)
	})
	defer c.a.Bind(done)
	c.movImm(scratchA, uint64(site.Pool))
	c.a.Load(scratchB, scratchA, abi.OffPoolCount)
	c.a.Op(amd64.Test, scratchB, scratchB, true)
	c.a.Jcc(amd64.CondE, slow)
	// The pool's last block, its cell cleared, in scratchC.
	c.a.OpImm(amd64.Sub, scratchB, 1, true)
	c.a.Store(scratchA, abi.OffPoolCount, scratchB)
	c.a.ShiftImm(amd64.Shl, scratchB, 3, true)
	c.a.Op(amd64.Add, scratchB, scratchA, true)
	c.a.Load(scratchC, scratchB, abi.OffPoolObjects)
	c.storeImm(scratchB, abi.OffPoolObjects, 0, scratchA)
	n := int32(len(site.Captures))
	for k, x := range site.Captures {
		if int(x>>1) < site.Argc {
			continue
		}
		at := 8*n + int32(k)*c.enc.UpvalueSize
		// The old cell, and what it holds.
		c.a.Load(scratchA, regCtx, abi.OffCells)
		c.a.Load(scratchA, scratchA, int32(k)*8)
		c.a.Load(scratchB, scratchA, c.enc.UpvalueSlot)
		c.a.LoadSD(xScratch0, scratchB, c.enc.NumOffset)
		c.a.LoadSD(xScratch1, scratchB, c.enc.RefOffset)
		// Closed.
		c.a.StoreSD(scratchA, c.enc.UpvalueClosed+c.enc.NumOffset, xScratch0)
		c.a.StoreSD(scratchA, c.enc.UpvalueClosed+c.enc.RefOffset, xScratch1)
		c.a.Lea(scratchB, scratchA, c.enc.UpvalueClosed)
		c.a.Store(scratchA, c.enc.UpvalueSlot, scratchB)
		// The new one, which points at what it holds already
		// (jitFillCells), the context's.
		c.a.StoreSD(scratchC, at+c.enc.UpvalueClosed+c.enc.NumOffset, xScratch0)
		c.a.StoreSD(scratchC, at+c.enc.UpvalueClosed+c.enc.RefOffset, xScratch1)
		c.a.Lea(scratchB, scratchC, at)
		c.a.Load(scratchA, regCtx, abi.OffCells)
		c.a.Store(scratchA, int32(k)*8, scratchB)
	}
}

// cellsCloseMarking is cellsClose while the collector marks: the pool's
// cell cleared, and each pointer word written -- what a cell holds, the
// old one's slot, the context's cell -- with Go's write barrier
// (refStore). The block is kept in the context's second GoArgs while it
// runs; the old cell is read again from the context's Cells after each.
func (c *compiler) cellsCloseMarking(site *ssa.CallSite, slow amd64.Label) {
	block := abi.OffGoArgs + c.enc.ValueSize + c.enc.RefOffset
	c.movImm(scratchA, uint64(site.Pool))
	c.a.Load(scratchB, scratchA, abi.OffPoolCount)
	c.a.Op(amd64.Test, scratchB, scratchB, true)
	c.a.Jcc(amd64.CondE, slow)
	c.a.OpImm(amd64.Sub, scratchB, 1, true)
	c.a.Store(scratchA, abi.OffPoolCount, scratchB)
	c.a.ShiftImm(amd64.Shl, scratchB, 3, true)
	c.a.Op(amd64.Add, scratchB, scratchA, true)
	c.a.Load(scratchC, scratchB, abi.OffPoolObjects)
	c.a.Store(regCtx, block, scratchC)
	c.a.Lea(scratchA, scratchB, abi.OffPoolObjects)
	c.movImm(scratchC, 0)
	c.refStore()
	old := func(k int) {
		c.a.Load(scratchA, regCtx, abi.OffCells)
		c.a.Load(scratchA, scratchA, int32(k)*8)
	}
	n := int32(len(site.Captures))
	for k, x := range site.Captures {
		if int(x>>1) < site.Argc {
			continue
		}
		at := 8*n + int32(k)*c.enc.UpvalueSize
		// The old cell closed: what it holds, then its slot.
		old(k)
		c.a.Load(scratchB, scratchA, c.enc.UpvalueSlot)
		c.a.Load(scratchC, scratchB, c.enc.NumOffset)
		c.a.Store(scratchA, c.enc.UpvalueClosed+c.enc.NumOffset, scratchC)
		c.a.Load(scratchC, scratchB, c.enc.RefOffset)
		c.a.Lea(scratchA, scratchA, c.enc.UpvalueClosed+c.enc.RefOffset)
		c.refStore()
		old(k)
		c.a.Lea(scratchC, scratchA, c.enc.UpvalueClosed)
		c.a.Lea(scratchA, scratchA, c.enc.UpvalueSlot)
		c.refStore()
		// The new one holding it.
		old(k)
		c.a.Load(scratchB, regCtx, block)
		c.a.Load(scratchC, scratchA, c.enc.UpvalueClosed+c.enc.NumOffset)
		c.a.Store(scratchB, at+c.enc.UpvalueClosed+c.enc.NumOffset, scratchC)
		c.a.Load(scratchC, scratchA, c.enc.UpvalueClosed+c.enc.RefOffset)
		c.a.Lea(scratchA, scratchB, at+c.enc.UpvalueClosed+c.enc.RefOffset)
		c.refStore()
		// The context's.
		c.a.Load(scratchB, regCtx, block)
		c.a.Lea(scratchC, scratchB, at)
		c.a.Load(scratchA, regCtx, abi.OffCells)
		c.a.Lea(scratchA, scratchA, int32(k)*8)
		c.refStore()
	}
	// The block is the context's now; the GoArgs keep nothing.
	c.storeImm(regCtx, block, 0, scratchA)
}

// cellsTakeMarking is cellsTake while the collector marks: the pool's cell
// cleared, and each cell's argument's pointer word written, with Go's
// write barrier (refStore) -- the cells outlive the frame the arguments
// come from, in the closures that capture them. The block is read again
// from the context's Cells after each.
func (c *compiler) cellsTakeMarking(site *ssa.CallSite, slow amd64.Label) {
	c.a.Load(scratchA, regCtx, abi.OffLive)
	c.a.OpImm(amd64.Cmp, scratchA, abi.LiveCall, true)
	c.a.Jcc(amd64.CondNE, slow)
	c.movImm(scratchA, uint64(site.Pool))
	c.a.Load(scratchB, scratchA, abi.OffPoolCount)
	c.a.Op(amd64.Test, scratchB, scratchB, true)
	c.a.Jcc(amd64.CondE, slow)
	c.a.OpImm(amd64.Sub, scratchB, 1, true)
	c.a.Store(scratchA, abi.OffPoolCount, scratchB)
	c.a.ShiftImm(amd64.Shl, scratchB, 3, true)
	c.a.Op(amd64.Add, scratchB, scratchA, true)
	c.a.Load(scratchC, scratchB, abi.OffPoolObjects)
	c.a.Store(regCtx, abi.OffCells, scratchC)
	c.a.Lea(scratchA, scratchB, abi.OffPoolObjects)
	c.movImm(scratchC, 0)
	c.refStore()
	vs, n := c.enc.ValueSize, int32(len(site.Captures))
	for k, x := range site.Captures {
		at := 8*n + int32(k)*c.enc.UpvalueSize + c.enc.UpvalueClosed
		c.a.Load(scratchB, regCtx, abi.OffCells)
		if x&1 == 0 {
			c.storeImm(scratchB, at+c.enc.NumOffset, c.enc.Undefined, scratchA)
			c.storeImm(scratchB, at+c.enc.RefOffset, 0, scratchA)
			continue
		}
		slot := x >> 1
		c.a.Load(scratchA, regLocals, slot*vs+c.enc.NumOffset)
		c.a.Store(scratchB, at+c.enc.NumOffset, scratchA)
		c.a.Lea(scratchA, scratchB, at+c.enc.RefOffset)
		c.a.Load(scratchC, regLocals, slot*vs+c.enc.RefOffset)
		c.refStore()
	}
}

// closureTakeMarking is closureTake while the collector marks: the pool's
// cell cleared, and each cell and an arrow's this written into what the
// closure keeps, with Go's write barrier (refStore) -- the closure outlives
// the context they come from. What refStore keeps in no register is read
// again from where it was written: the closure from the call's keep cell.
func (c *compiler) closureTakeMarking(v *ssa.Value, site *ssa.CallSite, slow amd64.Label) {
	at := abi.OffKeep + int32(v.Index)*int32(c.enc.ValueSize)
	c.movImm(scratchA, uint64(site.Pool))
	c.a.Load(scratchB, scratchA, abi.OffPoolCount)
	c.a.Op(amd64.Test, scratchB, scratchB, true)
	c.a.Jcc(amd64.CondE, slow)
	c.poolTakeMarking(v, site)
	// The function's data, from the object in the keep cell, into scratchC.
	data := func() {
		c.a.Load(scratchC, regCtx, at+c.enc.RefOffset)
		c.a.Load(scratchB, scratchC, c.enc.ObjectExt)
		c.a.Load(scratchC, scratchB, c.enc.ObjectData+8)
	}
	for k, x := range site.Captures {
		base := abi.OffUpvalues
		if x&1 != 0 {
			base = abi.OffCells
		}
		data()
		c.a.Load(scratchB, scratchC, c.enc.FuncClosure)
		c.a.Load(scratchB, scratchB, c.enc.ClosureUpvalues)
		c.a.Lea(scratchA, scratchB, int32(k)*8)
		c.a.Load(scratchC, regCtx, base)
		c.a.Load(scratchC, scratchC, (x>>1)*8)
		c.refStore()
	}
	if site.LexThis {
		data()
		c.a.Load(scratchB, scratchC, c.enc.FuncExtra)
		c.a.Load(scratchA, regCtx, abi.OffThis+c.enc.NumOffset)
		c.a.Store(scratchB, c.enc.ExtraLexThis+c.enc.NumOffset, scratchA)
		c.a.Lea(scratchA, scratchB, c.enc.ExtraLexThis+c.enc.RefOffset)
		c.a.Load(scratchC, regCtx, abi.OffThis+c.enc.RefOffset)
		c.refStore()
	}
	c.movImm(scratchA, c.enc.Object)
	c.setG(v, scratchA)
}

// iterStep makes call v, a loop's step (ssa.CallSite's Step), where the
// cursor's state, read in place (abi.Encoding's IterState), walks an array
// whose next element is within its dense elements and no hole, and the
// collector does not mark: the element is copied into the call's keep cell,
// both words, and the index advanced, as iterNext does -- as V8's code
// iterates an array without a call. Any other step goes to slow, where Go
// makes it. It uses every scratch register and xScratch2, and no other.
func (c *compiler) iterStep(v *ssa.Value, slow amd64.Label) {
	x := v.Args[0]
	c.pointerWord(x, c.gpr(x, scratchA))
	c.a.Op(amd64.Test, scratchB, scratchB, true)
	c.a.Jcc(amd64.CondE, slow)
	c.objectData(scratchB, scratchB, scratchC, c.enc.IterState, func(cond amd64.Cond) { c.a.Jcc(cond, slow) })
	c.a.LoadU8(scratchC, scratchB, c.enc.IterDone)
	c.a.Op(amd64.Test, scratchC, scratchC, false)
	c.a.Jcc(amd64.CondNE, slow)
	c.a.MovQToX(xScratch2, scratchB)
	c.a.Load(scratchA, scratchB, c.enc.IterIdx)
	c.a.Load(scratchC, scratchB, c.enc.IterArr)
	c.a.Op(amd64.Test, scratchC, scratchC, true)
	// No array: a Map's or a Set's iterator, which mapStep steps, out of
	// the array's way, with the code's cold paths.
	nexts := stepNexts(v, c.enc)
	maps := slow
	if nexts != ([2]uintptr{}) {
		maps = c.a.NewLabel()
		done := c.a.NewLabel()
		defer c.a.Bind(done)
		c.cold = append(c.cold, func() {
			c.a.Bind(maps)
			c.mapStep(v, nexts, slow)
			c.a.Jmp(done)
		})
	}
	c.a.Jcc(amd64.CondE, maps)
	c.a.Load(scratchB, scratchC, c.enc.ObjectElems+8)
	c.a.Op(amd64.Cmp, scratchA, scratchB, true)
	c.a.Jcc(amd64.CondAE, slow)
	c.a.Load(scratchB, scratchC, c.enc.ObjectElems)
	c.a.ShiftImm(amd64.Shl, scratchA, 4, true)
	c.a.Op(amd64.Add, scratchA, scratchB, true)
	c.a.Load(scratchB, scratchA, c.enc.NumOffset)
	c.cmpWord(scratchB, c.enc.Uninitialized)
	c.a.Jcc(amd64.CondE, slow)
	keep := abi.OffKeep + int32(v.Index)*int32(c.enc.ValueSize)
	c.a.Store(regCtx, keep+c.enc.NumOffset, scratchB)
	c.a.Load(scratchC, scratchA, c.enc.RefOffset)
	c.a.Store(regCtx, keep+c.enc.RefOffset, scratchC)
	c.a.MovQFromX(scratchB, xScratch2)
	c.a.Load(scratchC, scratchB, c.enc.IterIdx)
	c.a.OpImm(amd64.Add, scratchC, 1, true)
	c.a.Store(scratchB, c.enc.IterIdx, scratchC)
}

// mapStep is iterStep's step of a for...of over a Map's or a Set's
// iterator whose next is one of nexts (ssa.CallSite's MapNexts), the
// cursor's state in scratchB: the entry at the iterator's place, where it
// is live and the iterator has not lost its place -- its key, or its value,
// to the call's keep cell, and its place moved past it, as jsMap's next
// moves it -- as V8 inlines a Map iterator's next. Anything else goes to
// slow, where Go steps it: a deleted entry, the end, a collection
// compacted since, an iterator of entries, whose step makes an array. It
// uses every scratch register and xScratch2, and no other.
func (c *compiler) mapStep(v *ssa.Value, nexts [2]uintptr, slow amd64.Label) {
	fail := func(cond amd64.Cond) { c.a.Jcc(cond, slow) }
	// The iterator's next, one of the realm's.
	have := c.a.NewLabel()
	c.a.Load(scratchA, scratchB, c.enc.IterNext+c.enc.RefOffset)
	for _, n := range nexts {
		if n != 0 {
			c.cmpWord(scratchA, uint64(n))
			c.a.Jcc(amd64.CondE, have)
		}
	}
	c.a.Jmp(slow)
	c.a.Bind(have)
	// The iterator, an object whose data is a Map's or a Set's iterator's,
	// not done, of keys or of values.
	c.a.Load(scratchC, scratchB, c.enc.IterIter+c.enc.NumOffset)
	c.cmpWord(scratchC, c.enc.Object)
	fail(amd64.CondNE)
	c.a.Load(scratchA, scratchB, c.enc.IterIter+c.enc.RefOffset)
	c.objectData(scratchB, scratchA, scratchC, c.enc.MapIterData, fail)
	c.a.LoadU8(scratchC, scratchB, c.enc.MapIterDone)
	c.a.Op(amd64.Test, scratchC, scratchC, false)
	fail(amd64.CondNE)
	kind := c.a.NewLabel()
	c.a.LoadU8(scratchC, scratchB, c.enc.MapIterKind)
	c.a.OpImm(amd64.Cmp, scratchC, int32(c.enc.MapKeys), false)
	c.a.Jcc(amd64.CondE, kind)
	c.a.OpImm(amd64.Cmp, scratchC, int32(c.enc.MapValues), false)
	fail(amd64.CondNE)
	c.a.Bind(kind)
	c.a.MovQToX(xScratch2, scratchB)
	// Its collection, not compacted since it found its place, and its
	// place within the entries.
	c.a.Load(scratchA, scratchB, c.enc.MapIterMap)
	c.a.Op(amd64.Test, scratchA, scratchA, true)
	fail(amd64.CondE)
	c.a.LoadU32(scratchC, scratchB, c.enc.MapIterGen)
	c.a.CmpMem(scratchC, scratchA, c.enc.MapGen, false)
	fail(amd64.CondNE)
	c.a.Load(scratchB, scratchB, c.enc.MapIterAt)
	c.a.CmpMem(scratchB, scratchA, c.enc.MapEntries+8, true)
	fail(amd64.CondAE)
	// The entry, live, and its key or value no hole.
	c.a.Load(scratchA, scratchA, c.enc.MapEntries)
	c.movImm(scratchC, uint64(c.enc.MapEntrySize))
	c.a.Imul(scratchC, scratchB)
	c.a.Op(amd64.Add, scratchA, scratchC, true)
	c.a.Load(scratchC, scratchA, c.enc.NumOffset)
	c.cmpWord(scratchC, c.enc.MapDeleted)
	fail(amd64.CondE)
	// The key, or the value, to the keep cell.
	keep := abi.OffKeep + int32(v.Index)*int32(c.enc.ValueSize)
	values, stored := c.a.NewLabel(), c.a.NewLabel()
	c.a.MovQFromX(scratchC, xScratch2)
	c.a.LoadU8(scratchC, scratchC, c.enc.MapIterKind)
	c.a.OpImm(amd64.Cmp, scratchC, int32(c.enc.MapKeys), false)
	c.a.Jcc(amd64.CondNE, values)
	for i, at := range [2]int32{0, c.enc.MapEntryValue} {
		if i == 1 {
			c.a.Jmp(stored)
			c.a.Bind(values)
		}
		c.a.Load(scratchC, scratchA, at+c.enc.NumOffset)
		c.cmpWord(scratchC, c.enc.Uninitialized)
		fail(amd64.CondE)
		c.a.Store(regCtx, keep+c.enc.NumOffset, scratchC)
		c.a.Load(scratchC, scratchA, at+c.enc.RefOffset)
		c.a.Store(regCtx, keep+c.enc.RefOffset, scratchC)
	}
	c.a.Bind(stored)
	// Its place past the entry, which is its last.
	c.a.OpImm(amd64.Add, scratchB, 1, true)
	c.a.MovQFromX(scratchC, xScratch2)
	c.a.Store(scratchC, c.enc.MapIterAt, scratchB)
	c.a.Load(scratchB, scratchA, c.enc.MapEntrySeq)
	c.a.Store(scratchC, c.enc.MapIterLast, scratchB)
}

// lengthWrite makes call v, an array's length written by a call of Go
// (lengthWrites): its receiver an array, dense, no property of its own,
// whose length may be written, and the value a number that is an integer
// from 0 to its length -- the length set, the elements past it cleared, as
// the VM's setArrayLength shortens it -- while the collector does not
// mark, which wants the pointers cleared shaded. Its result, undefined, is
// in the call's keep cell. Anything else goes to slow, where Go makes it.
// It uses every scratch register and xScratch2, and no other.
func (c *compiler) lengthWrite(v *ssa.Value, slow amd64.Label) {
	fail := func(cond amd64.Cond) { c.a.Jcc(cond, slow) }
	recv, x := v.Args[0], v.Args[1]
	// The value: a number whose truncation is itself, kept in xScratch2.
	if w := c.gpr(x, scratchA); w != scratchA {
		c.a.MovRR(scratchA, w)
	}
	c.cmpWord(scratchA, abi.NumberLimit)
	fail(amd64.CondAE)
	c.a.MovQToX(xScratch2, scratchA)
	c.a.Cvttsd2si(scratchB, xScratch2)
	c.a.Cvtsi2sd(xScratch2, scratchB, true)
	c.a.MovQFromX(scratchC, xScratch2)
	c.a.Op(amd64.Cmp, scratchC, scratchA, true)
	fail(amd64.CondNE)
	c.a.MovQToX(xScratch2, scratchB)
	// The receiver: a dense array of no property of its own, whose length
	// may be written.
	w := c.gpr(recv, scratchA)
	c.cmpWord(w, c.enc.Object)
	fail(amd64.CondNE)
	c.pointerWord(recv, w)
	c.a.Op(amd64.Test, scratchB, scratchB, true)
	fail(amd64.CondE)
	c.a.LoadU8(scratchC, scratchB, c.enc.ObjectClass)
	c.a.OpImm(amd64.Cmp, scratchC, int32(c.enc.ClassArray), false)
	fail(amd64.CondNE)
	c.a.LoadU8(scratchC, scratchB, c.enc.ObjectFlags)
	c.a.OpImm(amd64.And, scratchC, int32(c.enc.FlagLengthWritable|c.enc.FlagSparse), false)
	c.a.OpImm(amd64.Cmp, scratchC, int32(c.enc.FlagLengthWritable), false)
	fail(amd64.CondNE)
	c.a.Load(scratchC, scratchB, c.enc.ObjectProps+8)
	c.a.Op(amd64.Test, scratchC, scratchC, true)
	fail(amd64.CondNE)
	// The collector not marking.
	c.movImm(scratchC, c.enc.WriteBarrier)
	c.a.LoadU8(scratchC, scratchC, 0)
	c.a.Op(amd64.Test, scratchC, scratchC, false)
	fail(amd64.CondNE)
	// The length, no more than the array's, which becomes it; the
	// elements from it to the old one cleared.
	c.a.MovQFromX(scratchA, xScratch2)
	c.a.Load(scratchC, scratchB, c.enc.ObjectElems+8)
	c.a.Op(amd64.Cmp, scratchA, scratchC, true)
	fail(amd64.CondA)
	c.a.Store(scratchB, c.enc.ObjectElems+8, scratchA)
	c.a.Load(scratchB, scratchB, c.enc.ObjectElems)
	// A value is 16 bytes (lengthWrites).
	c.a.ShiftImm(amd64.Shl, scratchA, 4, true)
	c.a.ShiftImm(amd64.Shl, scratchC, 4, true)
	c.a.Op(amd64.Add, scratchA, scratchB, true)
	c.a.Op(amd64.Add, scratchC, scratchB, true)
	loop, cleared := c.a.NewLabel(), c.a.NewLabel()
	c.a.Bind(loop)
	c.a.Op(amd64.Cmp, scratchA, scratchC, true)
	c.a.Jcc(amd64.CondAE, cleared)
	for at := int32(0); at < c.enc.ValueSize; at += 8 {
		c.a.StoreImm(scratchA, at, 0)
	}
	c.a.OpImm(amd64.Add, scratchA, c.enc.ValueSize, true)
	c.a.Jmp(loop)
	c.a.Bind(cleared)
	keep := abi.OffKeep + int32(v.Index)*int32(c.enc.ValueSize)
	c.storeImm(regCtx, keep+c.enc.NumOffset, c.enc.Undefined, scratchA)
	c.storeImm(regCtx, keep+c.enc.RefOffset, 0, scratchA)
}

// keyedStore makes call v, an element's write Go makes from native code
// (ssa.GoKeyedWrite), where the receiver is an array and the key one of
// its dense elements', no hole, while the collector does not mark: the
// value's words stored over the element's, as native code stores an
// array's element, so that a site whose stores Go makes for the objects it
// meets still stores to arrays as it had. Its result, undefined, is in the
// call's keep cell. Anything else goes to slow, where Go stores it. It
// uses every scratch register and every scratch XMM register, and no other.
func (c *compiler) keyedStore(v *ssa.Value, slow amd64.Label) {
	fail := func(cond amd64.Cond) { c.a.Jcc(cond, slow) }
	recv, key, x := v.Args[0], v.Args[1], v.Args[2]
	// The value's words, in xScratch0 and xScratch1.
	w := c.gpr(x, scratchA)
	c.a.MovQToX(xScratch0, w)
	c.pointerWord(x, w)
	c.a.MovQToX(xScratch1, scratchB)
	// The key, a number whose truncation is itself, in xScratch2.
	if w := c.gpr(key, scratchA); w != scratchA {
		c.a.MovRR(scratchA, w)
	}
	c.cmpWord(scratchA, abi.NumberLimit)
	fail(amd64.CondAE)
	c.a.MovQToX(xScratch2, scratchA)
	c.a.Cvttsd2si(scratchB, xScratch2)
	c.a.Cvtsi2sd(xScratch2, scratchB, true)
	c.a.MovQFromX(scratchC, xScratch2)
	c.a.Op(amd64.Cmp, scratchC, scratchA, true)
	fail(amd64.CondNE)
	c.a.MovQToX(xScratch2, scratchB)
	// The receiver, an array, the collector not marking.
	w = c.gpr(recv, scratchA)
	c.cmpWord(w, c.enc.Object)
	fail(amd64.CondNE)
	c.pointerWord(recv, w)
	c.a.Op(amd64.Test, scratchB, scratchB, true)
	fail(amd64.CondE)
	c.a.LoadU8(scratchC, scratchB, c.enc.ObjectClass)
	c.a.OpImm(amd64.Cmp, scratchC, int32(c.enc.ClassArray), false)
	fail(amd64.CondNE)
	c.movImm(scratchC, c.enc.WriteBarrier)
	c.a.LoadU8(scratchC, scratchC, 0)
	c.a.Op(amd64.Test, scratchC, scratchC, false)
	fail(amd64.CondNE)
	// The element, within the dense ones and no hole: a writable data
	// property's.
	c.a.MovQFromX(scratchA, xScratch2)
	c.a.CmpMem(scratchA, scratchB, c.enc.ObjectElems+8, true)
	fail(amd64.CondAE)
	c.a.Load(scratchB, scratchB, c.enc.ObjectElems)
	c.a.ShiftImm(amd64.Shl, scratchA, 4, true)
	c.a.Op(amd64.Add, scratchA, scratchB, true)
	c.a.Load(scratchC, scratchA, c.enc.NumOffset)
	c.cmpWord(scratchC, c.enc.Uninitialized)
	fail(amd64.CondE)
	c.a.MovQFromX(scratchC, xScratch0)
	c.a.Store(scratchA, c.enc.NumOffset, scratchC)
	c.a.MovQFromX(scratchC, xScratch1)
	c.a.Store(scratchA, c.enc.RefOffset, scratchC)
	keep := abi.OffKeep + int32(v.Index)*int32(c.enc.ValueSize)
	c.storeImm(regCtx, keep+c.enc.NumOffset, c.enc.Undefined, scratchA)
	c.storeImm(regCtx, keep+c.enc.RefOffset, 0, scratchA)
}

// stepNexts are the Map's and Set's iterators' next functions a loop's
// step v natively steps the iterators of (ssa.CallSite's MapNexts), none
// where the encoding has no Map iterators' layout.
func stepNexts(v *ssa.Value, enc abi.Encoding) [2]uintptr {
	if enc.MapIterData == 0 || len(v.Calls()) != 1 {
		return [2]uintptr{}
	}
	return v.Calls()[0].MapNexts
}

// goCallOp is goCall's, of the call of Go op (ssa.CallSite's Go, and what
// goOpWord adds), described (goState). saving -- a native call's, made by
// Go -- it goes through the routine that saves every allocatable register
// across it (goCalling), not only what lives past it: a native call's
// state, which its exit reads, has its operands in registers that need not
// live past it (allocate), as a call of Go's state does not. Otherwise it
// saves what lives past it itself (goCallingLive), which costs a call
// less.
func (c *compiler) goCallOp(v *ssa.Value, op uint64, guard func(amd64.Cond), saving bool) {
	size := int32(unsafe.Sizeof(abi.GoArg{}))
	// Its operands, which it writes: a description's, which Go would read
	// at every call (goState), had cost more than the stores they save.
	st, _ := c.goState(op, v.Index, v, nil)
	for i, x := range v.Args {
		c.goArg(abi.OffGoArgs+int32(i)*size, x)
	}
	// Refused, it leaves, as its guard does: Go sends it there.
	stub := c.stubLabel(v.State, exitKind(v.Aux))
	if saving {
		c.goCalling(st, &stub)
	} else {
		c.goCallingLive(v, st, stub)
	}
	c.a.Load(scratchA, regCtx, abi.OffKeep+int32(v.Index)*int32(c.enc.ValueSize)+c.enc.NumOffset)
	c.setG(v, scratchA)
}

// goArg writes x's words to the context's operand at at (abi.GoArg): its
// number word, and its pointer word read where it came from, or 0.
func (c *compiler) goArg(at int32, x *ssa.Value) {
	var w amd64.Reg
	if remat(x) {
		c.materialize(x, scratchA)
		w = scratchA
	} else {
		w = c.gpr(x, scratchA)
	}
	c.a.Store(regCtx, at+c.enc.NumOffset, w)
	number, have := c.a.NewLabel(), c.a.NewLabel()
	if x.Shadow != nil || c.origin.At(x) >= 0 {
		c.cmpWord(w, abi.NumberLimit)
		c.a.Jcc(amd64.CondB, number)
	}
	c.pointerWord(x, w)
	c.a.Jmp(have)
	c.a.Bind(number)
	c.movImm(scratchB, 0)
	c.a.Bind(have)
	c.a.Store(regCtx, at+c.enc.RefOffset, scratchB)
}

// tailSelf is a call in tail position (ssa.OpTailSelf). Where the stack is
// shallow the call is made after it. Deeper, the frame is given up, as
// the interpreter gives it up, from a frame the interpreter gives up: a
// call of the closure the frame runs writes its arguments to the frame's
// parameters, undefined to its other locals, and enters the code at its
// start again, a loop in the frame; in a native callee's frame, a call of
// a function the call has (tailJump) puts that function's frame in this
// one's place and goes on in its code. Each argument's words are read into
// the context's operands first, since a parameter may be where one is --
// where the collector does not see them: while it marks, each pointer is
// stored through Go's write barrier first (refStore, into the context's
// Shade), which shades it, whatever of the frame it has scanned as the
// frame is written. The call polls, as a back-edge does. Any other call
// leaves for Go to make.
func (c *compiler) tailSelf(v *ssa.Value, guard func(amd64.Cond)) {
	shallow := c.a.NewLabel()
	c.a.Load(scratchA, regCtx, abi.OffLevel)
	c.a.Load(scratchB, regCtx, abi.OffBounds)
	c.a.Load(scratchB, scratchB, abi.OffBoundsTailLevel)
	c.a.Op(amd64.Cmp, scratchA, scratchB, true)
	c.a.Jcc(amd64.CondB, shallow)
	defer c.a.Bind(shallow)
	args, entry := v.Args[1:], c.entryAt(0)
	var this *ssa.Value
	if v.Key&1 != 0 {
		args, this = args[:len(args)-1], args[len(args)-1]
	}
	stub := c.stubLabel(v.State, exitKind(v.Aux))
	if entry == nil || len(args)+1 > abi.MaxGoArgs {
		c.a.Jmp(stub)
		return
	}
	poll, polled := c.a.NewLabel(), c.a.NewLabel()
	c.a.Load(scratchB, regCtx, abi.OffBackEdges)
	c.a.DecMem(scratchB, 0)
	c.a.Jcc(amd64.CondLE, poll)
	state := v.State
	c.cold = append(c.cold, func() {
		c.a.Bind(poll)
		if c.enc.CallGo != 0 {
			stub := c.stubLabel(state, abi.ExitPoll)
			c.goSaving(uint64(ssa.GoPoll), &stub)
			c.a.Jmp(polled)
			return
		}
		c.exitTo(state, abi.ExitPoll)
	})
	c.a.Bind(polled)
	c.a.Load(scratchA, regCtx, abi.OffTailFrame)
	c.a.Op(amd64.Test, scratchA, scratchA, true)
	guard(amd64.CondE)
	if !c.reference(v, v.Args[0], c.enc.Object, guard) {
		return
	}
	c.a.Store(regCtx, abi.OffCallCallee, scratchC)
	size := int32(unsafe.Sizeof(abi.GoArg{}))
	for i, x := range args {
		c.goArg(abi.OffGoArgs+int32(i)*size, x)
	}
	if this != nil {
		c.goArg(abi.OffGoArgs+int32(len(args))*size, this)
	}
	marked, shaded := c.a.NewLabel(), c.a.NewLabel()
	c.movImm(scratchB, c.enc.WriteBarrier)
	c.a.LoadU8(scratchB, scratchB, 0)
	c.a.Op(amd64.Test, scratchB, scratchB, false)
	c.a.Jcc(amd64.CondNE, marked)
	c.a.Bind(shaded)
	staged := len(args)
	if this != nil {
		staged++
	}
	c.cold = append(c.cold, func() {
		c.a.Bind(marked)
		for i := range staged {
			c.a.Lea(scratchA, regCtx, abi.OffShade)
			c.a.Load(scratchC, regCtx, abi.OffGoArgs+int32(i)*size+c.enc.RefOffset)
			c.refStore()
		}
		c.a.Jmp(shaded)
	})
	other := c.a.NewLabel()
	c.a.Load(scratchC, regCtx, abi.OffCallCallee)
	c.objectData(scratchB, scratchC, scratchC, c.enc.FuncData, guard)
	c.a.Load(scratchB, scratchB, c.enc.FuncClosure)
	c.a.Load(scratchA, regCtx, abi.OffClosure)
	c.a.Op(amd64.Cmp, scratchA, scratchB, true)
	c.a.Jcc(amd64.CondNE, other)
	for i := range int(v.Const.Bits) {
		nb, nd := c.slotAddr(i, false, scratchC)
		rb, rd := c.slotAddr(i, true, scratchC)
		if i < v.Index && i < len(args) {
			at := abi.OffGoArgs + int32(i)*size
			c.a.Load(scratchA, regCtx, at+c.enc.NumOffset)
			c.a.Store(nb, nd, scratchA)
			c.a.Load(scratchA, regCtx, at+c.enc.RefOffset)
			c.a.Store(rb, rd, scratchA)
			continue
		}
		c.movImm(scratchA, c.enc.Undefined)
		c.a.Store(nb, nd, scratchA)
		c.storeImm(rb, rd, 0, scratchB)
	}
	if v.Key&2 != 0 {
		// The receiver, the context's and the VM's frame's, if Go
		// entered the code with it.
		at := abi.OffGoArgs + int32(len(args))*size
		none := c.a.NewLabel()
		c.a.Load(scratchA, regCtx, at+c.enc.NumOffset)
		c.a.Load(scratchB, regCtx, at+c.enc.RefOffset)
		c.a.Store(regCtx, abi.OffThis+c.enc.NumOffset, scratchA)
		c.a.Store(regCtx, abi.OffThis+c.enc.RefOffset, scratchB)
		c.a.Load(scratchC, regCtx, abi.OffFrameThis)
		c.a.Op(amd64.Test, scratchC, scratchC, true)
		c.a.Jcc(amd64.CondE, none)
		c.a.Store(scratchC, c.enc.NumOffset, scratchA)
		c.a.Store(scratchC, c.enc.RefOffset, scratchB)
		c.a.Bind(none)
	}
	c.a.Jmp(c.labels[entry.ID])
	c.a.Bind(other)
	var sites []*ssa.CallSite
	for _, t := range v.Calls() {
		if t.Go == 0 && !t.Literal && !t.Push && !t.Pop && !t.Alloc && !t.Receiver && t.Via == 0 && t.Pool == 0 && !t.Coerce && t.Entry != 0 {
			sites = append(sites, t)
		}
	}
	if len(sites) == 0 {
		c.a.Jmp(stub)
		return
	}
	// A native callee's frame, which returns to native code.
	c.a.Load(scratchA, regCtx, abi.OffReturnTo)
	c.a.Op(amd64.Test, scratchA, scratchA, true)
	guard(amd64.CondE)
	for _, t := range sites {
		next := c.a.NewLabel()
		c.a.Load(scratchC, regCtx, abi.OffCallCallee)
		if t.ByCode != 0 {
			c.byCode(t, next)
		} else {
			c.cmpWord(scratchC, uint64(t.Callee))
			c.a.Jcc(amd64.CondNE, next)
		}
		c.tailJump(t, len(args), this != nil, guard)
		c.a.Bind(next)
	}
	c.a.Jmp(stub)
}

// tailJump is tailSelf's call of t, its n arguments, and its receiver if
// this, in the context's operands: its code, which native callers may call,
// is entered at its start (its link's body, past its call entry) with its
// frame in this one's place -- the arguments its parameters, undefined its
// other locals -- and this context its own, as a native call's call entry
// makes them (callEntry), returning where this frame would have.
func (c *compiler) tailJump(t *ssa.CallSite, n int, this bool, guard func(amd64.Cond)) {
	vs, size := int32(c.enc.ValueSize), int32(unsafe.Sizeof(abi.GoArg{}))
	c.movImm(scratchA, uint64(t.Entry))
	c.a.Load(scratchA, scratchA, 0)
	c.a.Op(amd64.Test, scratchA, scratchA, true)
	guard(amd64.CondE)
	c.a.Load(scratchB, regCtx, abi.OffBase)
	c.a.OpImm(amd64.Add, scratchB, int32(t.LocalCount+t.MaxStack), true)
	c.a.Load(scratchA, regCtx, abi.OffStackEnd)
	c.a.Op(amd64.Cmp, scratchB, scratchA, true)
	guard(amd64.CondA)
	for i := 0; i < t.LocalCount; i++ {
		at := int32(i) * vs
		if i < t.Params && i < n {
			g := abi.OffGoArgs + int32(i)*size
			c.a.Load(scratchA, regCtx, g+c.enc.NumOffset)
			c.a.Store(regLocals, at+c.enc.NumOffset, scratchA)
			c.a.Load(scratchA, regCtx, g+c.enc.RefOffset)
			c.a.Store(regLocals, at+c.enc.RefOffset, scratchA)
			continue
		}
		c.storeImm(regLocals, at+c.enc.NumOffset, c.enc.Undefined, scratchA)
		c.storeImm(regLocals, at+c.enc.RefOffset, 0, scratchA)
	}
	if this {
		g := abi.OffGoArgs + int32(n)*size
		c.a.Load(scratchA, regCtx, g+c.enc.NumOffset)
		c.a.Store(regCtx, abi.OffThis+c.enc.NumOffset, scratchA)
		c.a.Load(scratchA, regCtx, g+c.enc.RefOffset)
		c.a.Store(regCtx, abi.OffThis+c.enc.RefOffset, scratchA)
	} else {
		c.storeImm(regCtx, abi.OffThis+c.enc.NumOffset, c.enc.Undefined, scratchA)
		c.storeImm(regCtx, abi.OffThis+c.enc.RefOffset, 0, scratchA)
	}
	if t.ByCode != 0 {
		c.a.Load(scratchA, regCtx, abi.OffCallClosure)
		c.a.Store(regCtx, abi.OffClosure, scratchA)
		c.a.Load(scratchA, regCtx, abi.OffCallUpvalues)
		c.a.Store(regCtx, abi.OffUpvalues, scratchA)
	} else {
		c.storeImm(regCtx, abi.OffClosure, uint64(t.Closure), scratchA)
		c.storeImm(regCtx, abi.OffUpvalues, uint64(t.Upvalues), scratchA)
	}
	c.a.Lea(scratchA, regLocals, int32(t.LocalCount)*vs)
	c.a.Store(regCtx, abi.OffStack, scratchA)
	c.storeImm(regCtx, abi.OffOpen, 0, scratchA)
	c.storeImm(regCtx, abi.OffTailReturn, 0, scratchA)
	c.storeImm(regCtx, abi.OffCells, 0, scratchA)
	if c.f.Loops {
		// No loop of a built-in's call under way (abi.Context's Loop).
		c.storeImm(regCtx, abi.OffLoop, 0, scratchA)
	}
	if t.Tail {
		c.storeImm(regCtx, abi.OffTailFrame, b2u(t.TailFrame), scratchA)
	}
	if t.Count != 0 {
		c.movImm(scratchA, uint64(t.Count))
		c.a.Load(scratchB, scratchA, 0)
		c.a.OpImm(amd64.Add, scratchB, 1, true)
		c.a.Store(scratchA, 0, scratchB)
	}
	// The VM's stack's top past its frame.
	same := c.a.NewLabel()
	c.a.Load(scratchB, regCtx, abi.OffBase)
	c.a.OpImm(amd64.Add, scratchB, int32(t.LocalCount+t.MaxStack), true)
	c.a.Load(scratchA, regCtx, abi.OffStackTop)
	c.a.Store(scratchA, 0, scratchB)
	c.a.Load(scratchA, regCtx, abi.OffStackHigh)
	c.a.Load(scratchC, scratchA, 0)
	c.a.Op(amd64.Cmp, scratchB, scratchC, true)
	c.a.Jcc(amd64.CondBE, same)
	c.a.Store(scratchA, 0, scratchB)
	c.a.Bind(same)
	c.movImm(scratchA, uint64(t.Entry)-uint64(c.enc.LinkEntry))
	c.a.JmpMem(scratchA, c.enc.LinkBody)
}

// byCode checks that the callee, the function object whose pointer word is
// in scratchC, runs t's function, in t's environment and realm, jumping to
// next if not (ssa.CallSite's ByCode); and keeps the object, its closure
// and its captured bindings' first cell in the context (abi.Context's
// CallCallee), for the call to give its callee. It uses every scratch
// register.
func (c *compiler) byCode(t *ssa.CallSite, next amd64.Label) {
	// A miss has the callee in scratchC again, for the next site to check.
	miss, done := c.a.NewLabel(), c.a.NewLabel()
	c.a.Store(regCtx, abi.OffCallCallee, scratchC)
	c.objectData(scratchB, scratchC, scratchC, c.enc.FuncData, func(cond amd64.Cond) { c.a.Jcc(cond, miss) })
	c.a.Load(scratchB, scratchB, c.enc.FuncClosure)
	c.a.Op(amd64.Test, scratchB, scratchB, true)
	c.a.Jcc(amd64.CondE, miss)
	for _, f := range [...]struct {
		off  int32
		want uintptr
	}{{c.enc.ClosureFn, t.ByCode}, {c.enc.ClosureEnv, t.Env}, {c.enc.ClosureRealm, t.Realm}} {
		c.a.Load(scratchA, scratchB, f.off)
		c.cmpWord(scratchA, uint64(f.want))
		c.a.Jcc(amd64.CondNE, miss)
	}
	c.a.Store(regCtx, abi.OffCallClosure, scratchB)
	c.a.Load(scratchA, scratchB, c.enc.ClosureUpvalues)
	c.a.Store(regCtx, abi.OffCallUpvalues, scratchA)
	c.a.Jmp(done)
	c.a.Bind(miss)
	c.a.Load(scratchC, regCtx, abi.OffCallCallee)
	c.a.Jmp(next)
	c.a.Bind(done)
}

// arrowThis has a call of site t, an arrow that reads this, which the
// call's checks found, the callee in scratchC unless t checks the function
// (ByCode), keep the address of the this the arrow captured (abi.Context's
// CallThis), which its frame is given in place of the call's receiver --
// or go to next, the callee in scratchC again, for an arrow that shares a
// derived constructor's binding of this, which Go calls.
func (c *compiler) arrowThis(t *ssa.CallSite, next amd64.Label) {
	miss, done := c.a.NewLabel(), c.a.NewLabel()
	if t.ByCode == 0 {
		c.a.Store(regCtx, abi.OffCallCallee, scratchC)
	}
	c.a.Load(scratchB, regCtx, abi.OffCallCallee)
	c.a.Load(scratchB, scratchB, c.enc.ObjectExt)
	c.a.Load(scratchB, scratchB, c.enc.ObjectData+8)
	c.a.Load(scratchB, scratchB, c.enc.FuncExtra)
	c.a.Op(amd64.Test, scratchB, scratchB, true)
	c.a.Jcc(amd64.CondE, miss)
	c.a.Load(scratchA, scratchB, c.enc.ExtraLexThisRef)
	c.a.Op(amd64.Test, scratchA, scratchA, true)
	c.a.Jcc(amd64.CondNE, miss)
	c.a.OpImm(amd64.Add, scratchB, c.enc.ExtraLexThis, true)
	c.a.Store(regCtx, abi.OffCallThis, scratchB)
	c.a.Jmp(done)
	c.a.Bind(miss)
	c.a.Load(scratchC, regCtx, abi.OffCallCallee)
	c.a.Jmp(next)
	c.a.Bind(done)
}

// genericCheck has call v, of a function none of its sites is, the callee
// in scratchC, make it through the function's link (ssa.CallSite's
// Generic): the callee a closure of the caller's environment and realm,
// whose hint names a slot of the hint table (abi.HintTable) holding its
// function and its link, a function a generic call makes -- and, if it
// coerces its receiver, a method call's receiver an object -- with room in
// the VM's stack for its frame. Then it is the call's target, past its
// sites; otherwise it goes to miss, where Go makes the call.
func (c *compiler) genericCheck(v *ssa.Value, site *ssa.CallSite, target int, checked, miss amd64.Label) {
	c.a.Store(regCtx, abi.OffCallCallee, scratchC)
	c.objectData(scratchB, scratchC, scratchC, c.enc.FuncData, func(cond amd64.Cond) { c.a.Jcc(cond, miss) })
	c.a.Load(scratchB, scratchB, c.enc.FuncClosure)
	c.a.Op(amd64.Test, scratchB, scratchB, true)
	c.a.Jcc(amd64.CondE, miss)
	for _, f := range [...]struct {
		off  int32
		want uintptr
	}{{c.enc.ClosureEnv, site.GenericEnv}, {c.enc.ClosureRealm, site.GenericRealm}} {
		c.a.Load(scratchA, scratchB, f.off)
		c.cmpWord(scratchA, uint64(f.want))
		c.a.Jcc(amd64.CondNE, miss)
	}
	c.a.Store(regCtx, abi.OffCallClosure, scratchB)
	c.a.Load(scratchA, scratchB, c.enc.ClosureUpvalues)
	c.a.Store(regCtx, abi.OffCallUpvalues, scratchA)
	// Its hint's slot, its template's closures': its function's, holding
	// its link.
	c.a.Load(scratchA, scratchB, c.enc.ClosureCode)
	c.a.Op(amd64.Test, scratchA, scratchA, true)
	c.a.Jcc(amd64.CondE, miss)
	c.a.LoadU32(scratchA, scratchA, c.enc.ClosureHint)
	c.a.Load(scratchC, regCtx, abi.OffHints)
	c.a.Load(scratchC, scratchC, abi.OffHintLen)
	c.a.Op(amd64.Cmp, scratchA, scratchC, true)
	c.a.Jcc(amd64.CondAE, miss)
	c.a.ShiftImm(amd64.Shl, scratchA, 5, true)
	c.a.Load(scratchC, regCtx, abi.OffHints)
	c.a.Load(scratchC, scratchC, abi.OffHintSlots)
	c.a.Op(amd64.Add, scratchC, scratchA, true)
	c.a.Load(scratchA, scratchB, c.enc.ClosureFn)
	c.a.Load(scratchB, scratchC, c.enc.HintSlotFn)
	c.a.Op(amd64.Cmp, scratchA, scratchB, true)
	c.a.Jcc(amd64.CondNE, miss)
	c.a.Load(scratchC, scratchC, c.enc.HintSlotLink)
	c.a.Op(amd64.Test, scratchC, scratchC, true)
	c.a.Jcc(amd64.CondE, miss)
	c.a.Store(regCtx, abi.OffCallLink, scratchC)
	c.a.Load(scratchA, scratchC, c.enc.LinkFlags)
	c.a.MovRR(scratchB, scratchA)
	c.a.OpImm(amd64.And, scratchB, abi.LinkNotGeneric, true)
	c.a.Jcc(amd64.CondNE, miss)
	// An arrow, given the this it captured (CallThis), as a call of one the
	// VM has seen gives it (arrowThis): one that shares a derived
	// constructor's binding, Go calls. Arrows that read this, or whose
	// bodies end in a call, had been no generic call's, their calls past a
	// site's targets each made by Go: AWFY's DeltaBlue's forEach, of
	// (c) => c.execute(), at each.
	notArrow := c.a.NewLabel()
	c.a.MovRR(scratchB, scratchA)
	c.a.OpImm(amd64.And, scratchB, abi.LinkArrow, true)
	c.a.Jcc(amd64.CondE, notArrow)
	c.a.Load(scratchB, regCtx, abi.OffCallCallee)
	c.a.Load(scratchB, scratchB, c.enc.ObjectExt)
	c.a.Load(scratchB, scratchB, c.enc.ObjectData+8)
	c.a.Load(scratchB, scratchB, c.enc.FuncExtra)
	c.a.Op(amd64.Test, scratchB, scratchB, true)
	c.a.Jcc(amd64.CondE, miss)
	c.a.Load(scratchC, scratchB, c.enc.ExtraLexThisRef)
	c.a.Op(amd64.Test, scratchC, scratchC, true)
	c.a.Jcc(amd64.CondNE, miss)
	c.a.OpImm(amd64.Add, scratchB, c.enc.ExtraLexThis, true)
	c.a.Store(regCtx, abi.OffCallThis, scratchB)
	c.a.Bind(notArrow)
	c.a.OpImm(amd64.And, scratchA, abi.LinkCoerce, true)
	if site.Method {
		object := c.a.NewLabel()
		c.a.Jcc(amd64.CondE, object)
		c.cmpWord(c.gpr(v.Args[0], scratchA), c.enc.Object)
		c.a.Jcc(amd64.CondNE, miss)
		c.a.Bind(object)
	} else {
		c.a.Jcc(amd64.CondNE, miss)
	}
	// Room in the VM's stack.
	c.a.Load(scratchA, regCtx, abi.OffStackTop)
	c.a.Load(scratchA, scratchA, 0)
	c.a.Load(scratchC, regCtx, abi.OffCallLink)
	c.a.Load(scratchB, scratchC, c.enc.LinkFrame)
	c.a.Op(amd64.Add, scratchA, scratchB, true)
	c.a.Load(scratchB, regCtx, abi.OffStackEnd)
	c.a.Op(amd64.Cmp, scratchA, scratchB, true)
	c.a.Jcc(amd64.CondA, miss)
	c.a.StoreImm(regCtx, abi.OffCallTarget, int32(target))
	c.a.Jmp(checked)
}

// objectData has dst be the pointer word of the data (abi.Encoding's
// ObjectData) of the object in obj, failing unless it has its side fields
// (ObjectExt) and its data's type word is typ. It uses t, which may be
// obj, and scratchA; dst must be neither.
func (c *compiler) objectData(dst, obj, t amd64.Reg, typ uint64, fail func(amd64.Cond)) {
	c.a.Load(dst, obj, c.enc.ObjectExt)
	c.a.Op(amd64.Test, dst, dst, true)
	fail(amd64.CondE)
	c.a.Load(t, dst, c.enc.ObjectData)
	c.cmpWord(t, typ)
	fail(amd64.CondNE)
	c.a.Load(dst, dst, c.enc.ObjectData+8)
}

// callGo is a call of Go, op, its operands in the context's GoArgs: what
// is live saved, then to callGo (abi.Encoding's CallGo), which comes back
// here; the frame's registers read again, what was live restored. The
// caller tests GoStatus.
func (c *compiler) callGo(v *ssa.Value, op uint64, keep int) {
	c.storeImm(regCtx, abi.OffGoOp, op, scratchA)
	c.storeImm(regCtx, abi.OffGoKeep, uint64(keep), scratchA)
	for _, sv := range c.savesOf(v) {
		if sv.float {
			c.a.StoreSD(regCtx, c.spillDisp(sv.slot), amd64.XReg(sv.reg))
		} else {
			c.a.Store(regCtx, c.spillDisp(sv.slot), amd64.Reg(sv.reg))
		}
	}
	back := c.a.NewLabel()
	c.a.LeaLabel(scratchA, back)
	c.a.Store(regCtx, abi.OffGoResume, scratchA)
	c.movImm(scratchA, c.enc.CallGo)
	c.a.JmpReg(scratchA)
	c.a.Bind(back)
	c.a.Load(regLocals, regCtx, abi.OffLocals)
	c.a.Load(regStack, regCtx, abi.OffStack)
	for _, sv := range c.savesOf(v) {
		if sv.float {
			c.a.LoadSD(amd64.XReg(sv.reg), regCtx, c.spillDisp(sv.slot))
		} else {
			c.a.Load(amd64.Reg(sv.reg), regCtx, c.spillDisp(sv.slot))
		}
	}
}

// markingTake tests the collector's flag before call v takes its pool's
// next object (poolTake, arrayLiteral), which stores pointers -- the
// pool's cell cleared, the object kept, an array literal's elements --
// with no write barrier: while the collector marks, the take is made with
// one (poolTakeMarking, arrayElements), and native code goes on at the
// label returned, which the caller binds after its take.
func (c *compiler) markingTake(v *ssa.Value, site *ssa.CallSite) amd64.Label {
	c.movImm(scratchB, c.enc.WriteBarrier)
	c.a.LoadU8(scratchB, scratchB, 0)
	c.a.Op(amd64.Test, scratchB, scratchB, false)
	marked, join := c.a.NewLabel(), c.a.NewLabel()
	c.a.Jcc(amd64.CondNE, marked)
	c.cold = append(c.cold, func() {
		c.a.Bind(marked)
		c.poolTakeMarking(v, site)
		if site.Literal && site.Argc != 0 {
			c.arrayElements(v, true)
		}
		c.a.Jmp(join)
	})
	return join
}

// refillOnEmpty, its pool's count in scratchB, goes on if the pool has an
// object; if not, Go fills it (ssa.GoRefill), called from native code as
// V8's code calls its runtime when its space runs out, and the call is
// made again from its start, every check again: the collector may have
// begun marking meanwhile. Only if Go cannot does the guard fail.
func (c *compiler) refillOnEmpty(pool uintptr, guard func(amd64.Cond)) {
	v, top := c.callV, c.callTop
	if v == nil {
		c.a.Op(amd64.Test, scratchB, scratchB, true)
		guard(amd64.CondE)
		return
	}
	refill := c.a.NewLabel()
	c.a.Op(amd64.Test, scratchB, scratchB, true)
	c.a.Jcc(amd64.CondE, refill)
	c.cold = append(c.cold, func() {
		c.a.Bind(refill)
		c.storeImm(regCtx, abi.OffGoArgs+c.enc.RefOffset, uint64(pool), scratchA)
		c.callGo(v, uint64(ssa.GoRefill), 0)
		c.a.Load(scratchB, regCtx, abi.OffGoStatus)
		c.a.Op(amd64.Test, scratchB, scratchB, true)
		c.a.Jcc(amd64.CondNE, c.stubLabel(v.State, exitKind(v.Aux)))
		c.a.Jmp(top)
	})
}

// pushGuards checks a call of Array.prototype.push may append as its fast
// path does (ssa.CallSite's Push): the receiver, recv, a dense array,
// extensible, its length writable, with room, of the first of the
// prototypes, which are as they were, with no elements. It uses every
// scratch register.
func (c *compiler) pushGuards(t *ssa.CallSite, recv *ssa.Value, guard func(amd64.Cond)) {
	if recv.Shadow == nil && c.origin.At(recv) < 0 {
		// A primitive, never an array: scratchC, the callee, is not 0.
		c.a.Op(amd64.Test, scratchC, scratchC, true)
		guard(amd64.CondNE)
		return
	}
	c.cmpWord(c.gpr(recv, scratchA), c.enc.Object)
	guard(amd64.CondNE)
	c.sourceRef(recv, guard)
	c.a.LoadU8(scratchB, scratchC, c.enc.ObjectClass)
	c.a.OpImm(amd64.Cmp, scratchB, int32(c.enc.ClassArray), false)
	guard(amd64.CondNE)
	c.a.LoadU8(scratchB, scratchC, c.enc.ObjectFlags)
	want := c.enc.FlagExtensible | c.enc.FlagLengthWritable
	c.a.OpImm(amd64.And, scratchB, int32(want|c.enc.FlagSparse), false)
	c.a.OpImm(amd64.Cmp, scratchB, int32(want), false)
	guard(amd64.CondNE)
	c.a.Load(scratchB, scratchC, c.enc.ObjectElems+8)
	c.a.Load(scratchA, scratchC, c.enc.ObjectElems+16)
	c.a.Op(amd64.Cmp, scratchB, scratchA, true)
	guard(amd64.CondAE)
	c.a.Load(scratchB, scratchC, c.enc.ObjectProto)
	c.cmpWord(scratchB, uint64(t.Protos[0].Object))
	guard(amd64.CondNE)
	c.protoGuards(t.Protos, guard)
}

// protoGuards checks an array's prototypes, protos, the first of which is
// its own, are as they were: each with its shape, with no elements, the
// second the first's prototype and the last of the chain -- none, then,
// with an indexed property a setter could be asked about (the VM gives
// protos only where neither has an index in its table). It uses every
// scratch register.
func (c *compiler) protoGuards(protos [2]ssa.Holder, guard func(amd64.Cond)) {
	for i, h := range protos {
		c.movImm(scratchA, uint64(h.Object))
		c.a.Load(scratchB, scratchA, c.enc.ObjectShape)
		c.cmpWord(scratchB, uint64(h.Shape))
		guard(amd64.CondNE)
		// No elements: no side fields, or none there. It need not be an
		// array.
		none := c.a.NewLabel()
		c.a.Load(scratchB, scratchA, c.enc.ObjectExt)
		c.a.Op(amd64.Test, scratchB, scratchB, true)
		c.a.Jcc(amd64.CondE, none)
		c.a.Load(scratchB, scratchB, c.enc.ExtElems+8)
		c.a.Op(amd64.Test, scratchB, scratchB, true)
		guard(amd64.CondNE)
		c.a.Bind(none)
		c.a.LoadU8(scratchB, scratchA, c.enc.ObjectFlags)
		c.a.OpImm(amd64.And, scratchB, int32(c.enc.FlagSparse), false)
		guard(amd64.CondNE)
		// Its prototype: the next, or none.
		c.a.Load(scratchB, scratchA, c.enc.ObjectProto)
		if i == 0 {
			c.cmpWord(scratchB, uint64(protos[1].Object))
		} else {
			c.a.Op(amd64.Test, scratchB, scratchB, true)
		}
		guard(amd64.CondNE)
	}
}

// popGuards checks a call of Array.prototype.pop may pop as its fast path
// does (ssa.CallSite's Pop): the receiver, recv, a dense array of the
// realm's prototype, its length writable, whose last element is not a
// hole. It uses every scratch register.
func (c *compiler) popGuards(t *ssa.CallSite, recv *ssa.Value, guard func(amd64.Cond)) {
	if recv.Shadow == nil && c.origin.At(recv) < 0 {
		// A primitive, never an array: scratchC, the callee, is not 0.
		c.a.Op(amd64.Test, scratchC, scratchC, true)
		guard(amd64.CondNE)
		return
	}
	c.cmpWord(c.gpr(recv, scratchA), c.enc.Object)
	guard(amd64.CondNE)
	c.sourceRef(recv, guard)
	c.a.LoadU8(scratchB, scratchC, c.enc.ObjectClass)
	c.a.OpImm(amd64.Cmp, scratchB, int32(c.enc.ClassArray), false)
	guard(amd64.CondNE)
	c.a.LoadU8(scratchB, scratchC, c.enc.ObjectFlags)
	c.a.OpImm(amd64.And, scratchB, int32(c.enc.FlagLengthWritable|c.enc.FlagSparse), false)
	c.a.OpImm(amd64.Cmp, scratchB, int32(c.enc.FlagLengthWritable), false)
	guard(amd64.CondNE)
	c.a.Load(scratchB, scratchC, c.enc.ObjectProto)
	c.cmpWord(scratchB, uint64(t.Protos[0].Object))
	guard(amd64.CondNE)
	// Not empty, and its last element no hole.
	c.a.Load(scratchB, scratchC, c.enc.ObjectElems+8)
	c.a.Op(amd64.Test, scratchB, scratchB, true)
	guard(amd64.CondE)
	c.a.OpImm(amd64.Sub, scratchB, 1, true)
	c.a.ShiftImm(amd64.Shl, scratchB, 4, true)
	c.a.Load(scratchA, scratchC, c.enc.ObjectElems)
	c.a.Op(amd64.Add, scratchA, scratchB, true)
	c.a.Load(scratchB, scratchA, c.enc.NumOffset)
	c.cmpWord(scratchB, c.enc.Uninitialized)
	guard(amd64.CondE)
}

// pop pops a call of Array.prototype.pop's receiver's last element,
// checked (popGuards): the length one less, the element's words the
// call's result, kept (OpCallCell), its cell undefined. It uses every
// scratch register.
func (c *compiler) pop(v *ssa.Value) {
	if c.enc.ValueSize != 16 {
		panic("mir: a value that is not 16 bytes")
	}
	c.sourceRef(v.Args[0], func(amd64.Cond) {})
	c.a.Load(scratchB, scratchC, c.enc.ObjectElems+8)
	c.a.OpImm(amd64.Sub, scratchB, 1, true)
	c.a.Store(scratchC, c.enc.ObjectElems+8, scratchB)
	c.a.ShiftImm(amd64.Shl, scratchB, 4, true)
	c.a.Load(scratchA, scratchC, c.enc.ObjectElems)
	c.a.Op(amd64.Add, scratchA, scratchB, true)
	at := abi.OffKeep + int32(v.Index)*c.enc.ValueSize
	c.a.Load(scratchB, scratchA, c.enc.RefOffset)
	c.a.Store(regCtx, at+c.enc.RefOffset, scratchB)
	c.a.Load(scratchB, scratchA, c.enc.NumOffset)
	c.a.Store(regCtx, at+c.enc.NumOffset, scratchB)
	c.storeImm(scratchA, c.enc.NumOffset, c.enc.Undefined, scratchC)
	// While the collector marks, the element's pointer word cleared with
	// Go's write barrier (refStore), which shades the value popped: the
	// keep cell it goes to may be what the collector has scanned, the array
	// what it has not.
	marked, done := c.a.NewLabel(), c.a.NewLabel()
	c.movImm(scratchC, c.enc.WriteBarrier)
	c.a.LoadU8(scratchC, scratchC, 0)
	c.a.Op(amd64.Test, scratchC, scratchC, false)
	c.a.Jcc(amd64.CondNE, marked)
	c.storeImm(scratchA, c.enc.RefOffset, 0, scratchC)
	c.a.Bind(done)
	c.cold = append(c.cold, func() {
		c.a.Bind(marked)
		c.a.OpImm(amd64.Add, scratchA, c.enc.RefOffset, true)
		c.movImm(scratchC, 0)
		c.refStore()
		c.a.Load(scratchB, regCtx, at+c.enc.NumOffset)
		c.a.Jmp(done)
	})
	c.setG(v, scratchB)
}

// push appends a call of Array.prototype.push's argument to its receiver,
// checked (pushGuards): the value's words past the last element, the
// length one more, which is the call's result, kept (OpCallCell). It uses
// every scratch register, xScratch0 and xScratch1.
func (c *compiler) push(v *ssa.Value) {
	vs := int32(c.enc.ValueSize)
	recv, x := v.Args[0], v.Args[2]
	if vs != 16 {
		panic("mir: a value that is not 16 bytes")
	}
	// The value's words: its pointer word in xScratch1, its number word
	// in xScratch0.
	var w amd64.Reg
	if remat(x) {
		c.materialize(x, scratchA)
		w = scratchA
	} else {
		w = c.gpr(x, scratchA)
	}
	c.a.MovQToX(xScratch0, w)
	number, have := c.a.NewLabel(), c.a.NewLabel()
	if x.Shadow != nil || c.origin.At(x) >= 0 {
		c.cmpWord(w, abi.NumberLimit)
		c.a.Jcc(amd64.CondB, number)
	}
	c.pointerWord(x, w)
	c.a.Jmp(have)
	c.a.Bind(number)
	c.movImm(scratchB, 0)
	c.a.Bind(have)
	c.a.MovQToX(xScratch1, scratchB)
	// The array, checked: its length grown, then the element's address.
	c.sourceRef(recv, func(amd64.Cond) {})
	c.a.Load(scratchB, scratchC, c.enc.ObjectElems+8)
	c.a.OpImm(amd64.Add, scratchB, 1, true)
	c.a.Store(scratchC, c.enc.ObjectElems+8, scratchB)
	c.a.Load(scratchA, scratchC, c.enc.ObjectElems)
	c.a.MovRR(scratchC, scratchB)
	c.a.OpImm(amd64.Sub, scratchC, 1, true)
	c.a.ShiftImm(amd64.Shl, scratchC, 4, true)
	c.a.Op(amd64.Add, scratchA, scratchC, true)
	c.a.MovQFromX(scratchC, xScratch0)
	c.a.Store(scratchA, c.enc.NumOffset, scratchC)
	c.a.MovQFromX(scratchC, xScratch1)
	c.a.Store(scratchA, c.enc.RefOffset, scratchC)
	c.a.MovQToX(xScratch2, scratchA)
	// The result: the length, a number.
	c.a.Cvtsi2sd(xScratch0, scratchB, true)
	c.a.MovQFromX(scratchA, xScratch0)
	at := abi.OffKeep + int32(v.Index)*vs
	c.a.Store(regCtx, at+c.enc.NumOffset, scratchA)
	c.storeImm(regCtx, at+c.enc.RefOffset, 0, scratchB)
	// While the collector marks, the element's pointer word stored again
	// with Go's write barrier (refStore), which shades it: the array
	// outlives where the value came from.
	marked, done := c.a.NewLabel(), c.a.NewLabel()
	c.movImm(scratchB, c.enc.WriteBarrier)
	c.a.LoadU8(scratchB, scratchB, 0)
	c.a.Op(amd64.Test, scratchB, scratchB, false)
	c.a.Jcc(amd64.CondNE, marked)
	c.a.Bind(done)
	c.cold = append(c.cold, func() {
		c.a.Bind(marked)
		c.a.MovQFromX(scratchA, xScratch2)
		c.a.OpImm(amd64.Add, scratchA, c.enc.RefOffset, true)
		c.a.MovQFromX(scratchC, xScratch1)
		c.refStore()
		c.a.Load(scratchA, regCtx, at+c.enc.NumOffset)
		c.a.Jmp(done)
	})
	c.setG(v, scratchA)
}

// viaGuards checks a call through Function.prototype.call, whose callee was
// found to be it, calls its function: the receiver, recv, is that one. It
// uses every scratch register.
func (c *compiler) viaGuards(t *ssa.CallSite, recv *ssa.Value, guard func(amd64.Cond)) {
	if recv.Shadow == nil && c.origin.At(recv) < 0 {
		// A primitive, never the function.
		c.a.Op(amd64.Test, scratchC, scratchC, true)
		guard(amd64.CondNE)
		return
	}
	c.cmpWord(c.gpr(recv, scratchA), c.enc.Object)
	guard(amd64.CondNE)
	c.sourceRef(recv, guard)
	c.cmpWord(scratchC, uint64(t.Callee))
	guard(amd64.CondNE)
}

// arrayLiteral makes an array literal's array: the pool's next, whose room
// holds the elements, the call's operands (arrayElements). The array is
// fresh, so nothing else sees it meanwhile, and the collector is not
// marking.
func (c *compiler) arrayLiteral(v *ssa.Value, site *ssa.CallSite) {
	vs := int32(c.enc.ValueSize)
	if vs != 16 {
		panic("mir: a value that is not 16 bytes")
	}
	at := abi.OffKeep + int32(v.Index)*vs
	c.movImm(scratchA, uint64(site.Pool))
	c.a.Load(scratchB, scratchA, abi.OffPoolCount)
	c.a.OpImm(amd64.Sub, scratchB, 1, true)
	c.a.Store(scratchA, abi.OffPoolCount, scratchB)
	c.a.ShiftImm(amd64.Shl, scratchB, 3, true)
	c.a.Op(amd64.Add, scratchB, scratchA, true)
	c.a.Load(scratchC, scratchB, abi.OffPoolObjects)
	c.storeImm(scratchB, abi.OffPoolObjects, 0, scratchA)
	c.a.Store(regCtx, at+c.enc.RefOffset, scratchC)
	c.storeImm(regCtx, at+c.enc.NumOffset, c.enc.Object, scratchA)
	c.arrayElements(v, false)
}

// arrayElements writes an array literal's elements, the call's operands,
// into its array, kept in the call's keep cell, both words of each, and
// sets its length; viaGo, while the collector marks, each with Go's write
// barrier (valueStore). The result is set last: its register may be an
// operand's.
func (c *compiler) arrayElements(v *ssa.Value, viaGo bool) {
	vs := int32(c.enc.ValueSize)
	at := abi.OffKeep + int32(v.Index)*vs
	for i, x := range v.Args {
		// The element's words: its number word in xScratch0, its pointer
		// word in xScratch1, as push has them.
		var w amd64.Reg
		if remat(x) {
			c.materialize(x, scratchA)
			w = scratchA
		} else {
			w = c.gpr(x, scratchA)
		}
		c.a.MovQToX(xScratch0, w)
		number, have := c.a.NewLabel(), c.a.NewLabel()
		if x.Shadow != nil || c.origin.At(x) >= 0 {
			c.cmpWord(w, abi.NumberLimit)
			c.a.Jcc(amd64.CondB, number)
		}
		c.pointerWord(x, w)
		c.a.Jmp(have)
		c.a.Bind(number)
		c.movImm(scratchB, 0)
		c.a.Bind(have)
		c.a.MovQToX(xScratch1, scratchB)
		c.a.Load(scratchC, regCtx, at+c.enc.RefOffset)
		c.a.Load(scratchA, scratchC, c.enc.ObjectElems)
		if viaGo {
			// The cell's address in scratchA, the words in xScratch0 and
			// scratchC.
			c.a.OpImm(amd64.Add, scratchA, int32(i)*vs, true)
			c.a.MovQFromX(scratchC, xScratch1)
			c.valueStore()
			continue
		}
		c.a.MovQFromX(scratchB, xScratch0)
		c.a.Store(scratchA, int32(i)*vs+c.enc.NumOffset, scratchB)
		c.a.MovQFromX(scratchB, xScratch1)
		c.a.Store(scratchA, int32(i)*vs+c.enc.RefOffset, scratchB)
	}
	c.a.Load(scratchC, regCtx, at+c.enc.RefOffset)
	c.storeImm(scratchC, c.enc.ObjectElems+8, uint64(len(v.Args)), scratchB)
	c.movImm(scratchA, c.enc.Object)
	c.setG(v, scratchA)
}

// poolTake has a call's result be its pool's last object, its cell
// cleared, kept (OpCallCell): nothing runs, so nothing is recorded or
// saved. The pool has one, and the collector is not marking.
func (c *compiler) poolTake(v *ssa.Value, site *ssa.CallSite) {
	vs := int32(c.enc.ValueSize)
	c.movImm(scratchA, uint64(site.Pool))
	c.a.Load(scratchB, scratchA, abi.OffPoolCount)
	c.a.OpImm(amd64.Sub, scratchB, 1, true)
	c.a.Store(scratchA, abi.OffPoolCount, scratchB)
	c.a.ShiftImm(amd64.Shl, scratchB, 3, true)
	c.a.Op(amd64.Add, scratchB, scratchA, true)
	c.a.Load(scratchC, scratchB, abi.OffPoolObjects)
	c.storeImm(scratchB, abi.OffPoolObjects, 0, scratchA)
	at := abi.OffKeep + int32(v.Index)*vs
	c.a.Store(regCtx, at+c.enc.RefOffset, scratchC)
	c.movImm(scratchA, c.enc.Object)
	c.a.Store(regCtx, at+c.enc.NumOffset, scratchA)
	c.setG(v, scratchA)
}

// poolTakeMarking is poolTake while the collector marks: the pool's cell
// cleared with Go's write barrier (refStore), which shades the object it
// overwrites -- the pool may be what the collector has not scanned yet,
// the keep cell the object goes to what it has.
func (c *compiler) poolTakeMarking(v *ssa.Value, site *ssa.CallSite) {
	at := abi.OffKeep + int32(v.Index)*int32(c.enc.ValueSize)
	c.movImm(scratchA, uint64(site.Pool))
	c.a.Load(scratchB, scratchA, abi.OffPoolCount)
	c.a.OpImm(amd64.Sub, scratchB, 1, true)
	c.a.Store(scratchA, abi.OffPoolCount, scratchB)
	c.a.ShiftImm(amd64.Shl, scratchB, 3, true)
	c.a.Op(amd64.Add, scratchB, scratchA, true)
	c.a.Load(scratchC, scratchB, abi.OffPoolObjects)
	c.a.Store(regCtx, at+c.enc.RefOffset, scratchC)
	c.storeImm(regCtx, at+c.enc.NumOffset, c.enc.Object, scratchA)
	c.a.Lea(scratchA, scratchB, abi.OffPoolObjects)
	c.movImm(scratchC, 0)
	c.refStore()
	c.movImm(scratchA, c.enc.Object)
	c.setG(v, scratchA)
}

// poolShade has, while the collector marks, the next object of each pool
// a call of sites takes its receiver from (CallSite's Pool) shaded: stored
// where it is with Go's write barrier (refStore), which shades what it
// overwrites, the object itself -- the call's setup then takes it, its
// cell cleared, with none. It is made before anything else of the call,
// nothing in a scratch register.
func (c *compiler) poolShade(sites []*ssa.CallSite) {
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
	c.movImm(scratchB, c.enc.WriteBarrier)
	c.a.LoadU8(scratchB, scratchB, 0)
	c.a.Op(amd64.Test, scratchB, scratchB, false)
	c.a.Jcc(amd64.CondNE, marked)
	c.a.Bind(back)
	c.cold = append(c.cold, func() {
		c.a.Bind(marked)
		for _, pool := range pools {
			empty := c.a.NewLabel()
			c.movImm(scratchA, uint64(pool))
			c.a.Load(scratchB, scratchA, abi.OffPoolCount)
			c.a.Op(amd64.Test, scratchB, scratchB, true)
			c.a.Jcc(amd64.CondE, empty)
			c.a.ShiftImm(amd64.Shl, scratchB, 3, true)
			c.a.Op(amd64.Add, scratchA, scratchB, true)
			c.a.Lea(scratchA, scratchA, abi.OffPoolObjects-8)
			c.a.Load(scratchC, scratchA, 0)
			c.refStore()
			c.a.Bind(empty)
		}
		c.a.Jmp(back)
	})
}

// lengthGuards checks a `new Array(n)` can take its array from its pool:
// n, the operand x, a number that is an integer in [0, the pool's Room]
// (abi.ObjectPool). One past Room, up to abi.PoolRoomLimit, has Go fill
// the pool again with arrays of more room (Want), and the call is made
// again from its start, every check again, as an empty pool is
// (refillOnEmpty); any other n fails the guard: Go makes the array, as
// V8's code calls the Array constructor past the length it allocates
// inline. It uses every scratch register.
func (c *compiler) lengthGuards(t *ssa.CallSite, x *ssa.Value, guard func(amd64.Cond)) {
	var w amd64.Reg
	if remat(x) {
		c.materialize(x, scratchA)
		w = scratchA
	} else {
		w = c.gpr(x, scratchA)
	}
	if x.Shadow != nil || c.origin.At(x) >= 0 {
		// It may hold a reference, whose word is no number's.
		c.cmpWord(w, abi.NumberLimit)
		guard(amd64.CondAE)
	}
	c.a.MovQToX(xScratch0, w)
	c.a.Cvttsd2si(scratchA, xScratch0)
	c.a.Cvtsi2sd(xScratch1, scratchA, true)
	c.a.SSEOp(amd64.UcomiSD, xScratch1, xScratch0)
	guard(amd64.CondNE)
	guard(amd64.CondP)
	c.movImm(scratchB, uint64(t.Pool))
	c.a.Load(scratchC, scratchB, abi.OffPoolRoom)
	c.a.Op(amd64.Cmp, scratchA, scratchC, true)
	fits := c.a.NewLabel()
	c.a.Jcc(amd64.CondBE, fits)
	// Past its room: a negative n, unsigned, is past every room.
	c.a.OpImm(amd64.Cmp, scratchA, abi.PoolRoomLimit, true)
	guard(amd64.CondA)
	v, top := c.callV, c.callTop
	if v == nil {
		// No call to make again: Go makes it. n is at most the limit, as
		// the flags still say.
		guard(amd64.CondBE)
	} else {
		grow := c.a.NewLabel()
		c.a.Jmp(grow)
		pool := t.Pool
		c.cold = append(c.cold, func() {
			c.a.Bind(grow)
			c.a.Store(scratchB, abi.OffPoolWant, scratchA)
			c.storeImm(regCtx, abi.OffGoArgs+c.enc.RefOffset, uint64(pool), scratchA)
			c.callGo(v, uint64(ssa.GoRefill), 0)
			c.a.Load(scratchB, regCtx, abi.OffGoStatus)
			c.a.Op(amd64.Test, scratchB, scratchB, true)
			c.a.Jcc(amd64.CondNE, c.stubLabel(v.State, exitKind(v.Aux)))
			c.a.Jmp(top)
		})
	}
	c.a.Bind(fits)
}

// allocLength makes the length of `new Array(n)`'s array, call v's
// result, n, the operand x lengthGuards checked: its element count, the
// elements below it holes already. It uses every scratch register.
func (c *compiler) allocLength(v, x *ssa.Value) {
	var w amd64.Reg
	if remat(x) {
		c.materialize(x, scratchA)
		w = scratchA
	} else {
		w = c.gpr(x, scratchA)
	}
	c.a.MovQToX(xScratch0, w)
	c.a.Cvttsd2si(scratchB, xScratch0)
	c.a.Load(scratchC, regCtx, abi.OffKeep+int32(v.Index)*int32(c.enc.ValueSize)+c.enc.RefOffset)
	c.a.Store(scratchC, c.enc.ObjectElems+8, scratchB)
}

// constructGuards checks a construction can take its receiver from its
// pool (abi.ObjectPool): the pool has one, and the function, whose pointer
// is in scratchC, has the prototype the pool's were made with still, its
// own property where it was. It uses every scratch register.
func (c *compiler) constructGuards(t *ssa.CallSite, guard func(amd64.Cond)) {
	c.movImm(scratchA, uint64(t.Pool))
	c.a.Load(scratchB, scratchA, abi.OffPoolCount)
	c.refillOnEmpty(t.Pool, guard)
	if t.Alloc {
		// A built-in's, whose prototype the realm fixed.
		return
	}
	c.a.Load(scratchB, scratchC, c.enc.ObjectProps+8)
	c.a.OpImm(amd64.Cmp, scratchB, int32(t.ProtoIndex), true)
	guard(amd64.CondBE)
	c.a.Load(scratchB, scratchC, c.enc.ObjectShape)
	c.a.Load(scratchB, scratchB, c.enc.ShapeKeys)
	c.a.LoadU32(scratchB, scratchB, int32(t.ProtoIndex)*c.enc.KeySize+c.enc.PropertyKey)
	c.a.OpImm(amd64.Cmp, scratchB, int32(t.ProtoKey), false)
	guard(amd64.CondNE)
	c.a.Load(scratchB, scratchC, c.enc.ObjectProps)
	c.a.Load(scratchC, scratchB, int32(t.ProtoIndex)*c.enc.ValueSize+c.enc.RefOffset)
	c.a.Load(scratchB, scratchA, abi.OffPoolProto)
	c.a.Op(amd64.Cmp, scratchC, scratchB, true)
	guard(amd64.CondNE)
}

// directOperand writes a call's operand x, both words, where its callee
// has it: at disp in the callee's frame, whose address is in xScratch2, if
// local, and otherwise at disp from the context. It uses every scratch
// register.
func (c *compiler) directOperand(x *ssa.Value, local bool, disp int32) {
	const base = scratchC
	at := func() {
		if local {
			c.a.MovQFromX(scratchC, xScratch2)
		} else {
			c.a.Load(scratchC, regCtx, abi.OffNext)
		}
	}
	var w amd64.Reg
	if remat(x) {
		c.materialize(x, scratchA)
		w = scratchA
	} else {
		w = c.gpr(x, scratchA)
	}
	// The pointer word first, which uses scratchC: the address once.
	c.pointerWord(x, w)
	at()
	c.a.Store(base, disp+c.enc.NumOffset, w)
	c.a.Store(base, disp+c.enc.RefOffset, scratchB)
}

// pointerWord leaves in scratchB the pointer word of a value whose number
// word is in w, read where it came from -- its shadow, or the slot its
// origin is -- or 0 for a primitive's. It uses scratchC.
func (c *compiler) pointerWord(x *ssa.Value, w amd64.Reg) {
	done := c.a.NewLabel()
	c.movImm(scratchB, 0)
	switch o, static := c.origin.Of(x); {
	case x.Shadow != nil && cellSource(x.Shadow):
		c.a.Load(scratchB, c.gpr(x.Shadow, scratchC), c.enc.RefOffset)
	case x.Shadow != nil:
		r := c.gpr(x.Shadow, scratchC)
		c.a.Op(amd64.Test, r, r, true)
		c.a.Jcc(amd64.CondE, done)
		c.a.Load(scratchB, r, c.enc.RefOffset)
	case static && o >= 0:
		c.isReference(x, w, o, done)
		base, disp := c.slotAddr(o, true, scratchC)
		c.a.Load(scratchB, base, disp)
	}
	c.a.Bind(done)
}

// exitTo writes a frame state into the frame and returns with an exit
// record. A slot holding what was loaded from it at this entry is left as
// it is. Every other slot gets its value's word, boxes and constants
// recomputed from their operands -- unless the value is the reference its
// origin slot held, which Go copies, or the slot holds a reference, whose
// pointer word Go clears: those are records (abi.Record). The frame is as
// it was at entry until here, so the origin slot still holds the
// reference, and a slot this stub has written held none. So a value whose
// origin is known only at run time (a shadow) needs no record when it
// turns out to be a primitive, stored where no reference is, or the
// reference the slot itself held.
func (c *compiler) exitTo(s *ssa.FrameState, kind uint64) {
	c.exitThen(s, kind, nil)
}

// exitThen is exitTo, going on at then, if it is not nil, instead of
// returning to Go: the records are applied first (recordsTail), which a
// caller of it makes sure they are, the collector not marking.
func (c *compiler) exitThen(s *ssa.FrameState, kind uint64, then *amd64.Label) {
	if then == nil && TableExits {
		// To Go, which writes the frame from the exit's description.
		if d := c.exitDescriptor(s, kind, uint8(regLocals), uint8(regStack)); d != nil {
			c.exits = append(c.exits, d)
			c.a.MovAddr(scratchA, uint64(uintptr(unsafe.Pointer(d))))
			if !c.tableUsed {
				c.table, c.tableUsed = c.a.NewLabel(), true
			}
			c.a.Jmp(c.table)
			return
		}
	}
	c.recorded = false
	c.movImm(scratchC, 0)
	c.a.Store(regCtx, abi.OffRecords, scratchC)
	for i, v := range s.Slots {
		if v == nil || v.Op == ssa.OpLoadSlot && v.Aux == i || c.captured(i) {
			// Unchanged: a captured binding's own value (capturedUnchanged);
			// or between a caller's slots and an inlined callee's.
			continue
		}
		if bigKept(v) != nil {
			// A BigInt's box, made of the BigInt in its cell (bigRebox) --
			// a loop header's state names it before it is made there, and
			// one only states name is made nowhere else -- which the
			// record copies.
			w := c.bigRebox(v)
			at := abi.OffKeep + int32(v.Args[1].Index)*int32(c.enc.ValueSize)
			c.appendRecord(uint64(i)|abi.RecordMaybe, func() amd64.Reg {
				c.a.MovRR(scratchB, regCtx)
				c.a.OpImm(amd64.Add, scratchB, at, true)
				return scratchB
			}, 0, false, &w)
			continue
		}
		var w amd64.Reg
		if remat(v) {
			c.materialize(v, scratchA)
			w = scratchA
		} else {
			w = c.gpr(v, scratchA)
		}
		next := c.a.NewLabel()
		if v.Shadow != nil {
			// Where it came from is known only at run time: Go looks,
			// unless it is nowhere or this slot.
			scalar := c.a.NewLabel()
			from := c.gpr(v.Shadow, scratchB)
			c.a.Op(amd64.Test, from, from, true)
			c.a.Jcc(amd64.CondE, scalar)
			c.slotSource(i, scratchC)
			c.a.Op(amd64.Cmp, from, scratchC, true)
			c.a.Jcc(amd64.CondE, next)
			c.appendRecord(uint64(i)|abi.RecordMaybe, c.gprAfter(v.Shadow), 0, false, &w)
			c.a.Jmp(next)
			c.a.Bind(scalar)
		} else if o, ok := c.origin.Of(v); ok && o >= 0 {
			scalar := c.a.NewLabel()
			c.isReference(v, w, o, scalar)
			if o != i {
				c.appendRecord(uint64(i), nil, uint64(o), true, nil)
			}
			c.a.Jmp(next)
			c.a.Bind(scalar)
		}
		record := c.a.NewLabel()
		base, disp := c.slotAddr(i, true, scratchC)
		c.a.Load(scratchC, base, disp)
		c.a.Op(amd64.Test, scratchC, scratchC, true)
		c.a.Jcc(amd64.CondNE, record)
		base, disp = c.slotAddr(i, false, scratchC)
		c.a.Store(base, disp, w)
		c.a.Jmp(next)
		c.a.Bind(record)
		c.appendRecord(uint64(i)|abi.RecordScalar, nil, 0, true, &w)
		c.a.Bind(next)
	}
	if s.Inline != nil {
		// Inlined callees' frames, written past their callers' operands: a
		// context for each, the outermost's next to this one, says where,
		// for Go to make it from; this one's exit is at the call, as each
		// but the innermost's is.
		const next = scratchB
		c.a.MovRR(next, regCtx)
		for i, in := range inlineLevels(s) {
			c.a.Load(next, next, abi.OffNext)
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
				c.storeImm(next, f.off, f.v, scratchA)
			}
			if i == 0 {
				// The callers' frames' first slot, in the VM's stack.
				c.a.MovRR(scratchC, regStack)
				c.a.Load(scratchA, regCtx, abi.OffStackBase)
				c.a.Op(amd64.Sub, scratchC, scratchA, true)
				c.a.ShiftImm(amd64.Shr, scratchC, 4, true)
				c.a.OpImm(amd64.Sub, scratchC, int32(c.f.Locals), true)
			}
			c.a.MovRR(scratchA, scratchC)
			c.a.OpImm(amd64.Add, scratchA, int32(in.Base), true)
			c.a.Store(next, abi.OffBase, scratchA)
		}
		kind = abi.ExitHost
	}
	c.record(kind, uint64(s.PC), uint64(s.Depth), uint64(int64(s.Site)), then)
}

// tableExit is where every exit that leaves its frame to Go goes, its
// description's address in scratchA: it saves the registers values may be
// in, and the frame's, in the context, and returns to Go (abi.ExitTable),
// which writes the frame (jit.ApplyExit).
func (c *compiler) tableExit() {
	c.a.Store(regCtx, abi.OffExitDesc, scratchA)
	for _, r := range append([]int{int(regLocals), int(regStack)}, c.usedRegs(c.gprs, false)...) {
		c.a.Store(regCtx, abi.OffRegs+int32(r)*8, amd64.Reg(r))
	}
	for _, x := range c.usedRegs(c.fprs, true) {
		c.a.StoreSD(regCtx, abi.OffXRegs+int32(x)*8, amd64.XReg(x))
	}
	c.storeImm(regCtx, abi.OffExitKind, abi.ExitTable, scratchA)
	c.a.Ret()
}

// enterExit is where a native call goes whose callee has no call entry
// (callEntry): with the call entry's registers, and the callee's function
// object in callObject, it makes the callee's frame and context as a call
// entry does, from the callee's link, and returns to Go with
// abi.ExitEnter, for Go to run the callee and resume the caller after the
// call (ReturnTo) -- not a host exit, which writes the caller's frame and
// has Go make the call.
func (c *compiler) enterExit() {
	const base, at, top = amd64.R8, amd64.R10, amd64.R12
	c.a.Load(regCtx, regCtx, abi.OffNext)
	c.a.Store(regCtx, abi.OffReturnTo, callRet)
	c.a.Store(regCtx, abi.OffClosure, callClosure)
	c.a.Load(callClosure, callClosure, c.enc.ClosureUpvalues)
	c.a.Store(regCtx, abi.OffUpvalues, callClosure)
	c.a.Store(regCtx, abi.OffEnterCallee, callObject)
	c.a.Load(at, regCtx, abi.OffStackTop)
	c.a.Load(base, at, 0)
	c.a.Store(regCtx, abi.OffBase, base)
	c.a.Load(top, callLink, c.enc.LinkFrame)
	c.a.Op(amd64.Add, top, base, true)
	c.a.Store(at, 0, top)
	same := c.a.NewLabel()
	c.a.Load(at, regCtx, abi.OffStackHigh)
	c.a.CmpMem(top, at, 0, true)
	c.a.Jcc(amd64.CondBE, same)
	c.a.Store(at, 0, top)
	c.a.Bind(same)
	c.a.ShiftImm(amd64.Shl, base, 4, true)
	c.a.Load(at, regCtx, abi.OffStackBase)
	c.a.Op(amd64.Add, base, at, true)
	c.a.Store(regCtx, abi.OffLocals, base)
	c.a.Load(at, callLink, c.enc.LinkLocals)
	c.a.ShiftImm(amd64.Shl, at, 4, true)
	c.a.Op(amd64.Add, at, base, true)
	c.a.Store(regCtx, abi.OffStack, at)
	c.a.StoreImm(regCtx, abi.OffOpen, 0)
	c.a.StoreImm(regCtx, abi.OffCells, 0)
	c.a.StoreImm(regCtx, abi.OffLive, abi.LiveCall)
	c.a.IncMem(callLink, c.enc.LinkIn)
	c.movImm(scratchA, 0)
	for _, off := range []int32{abi.OffRecords, abi.OffExitPC, abi.OffExitDepth} {
		c.a.Store(regCtx, off, scratchA)
	}
	c.storeImm(regCtx, abi.OffExitSite, ^uint64(0), scratchA)
	c.storeImm(regCtx, abi.OffExitKind, abi.ExitEnter, scratchA)
	c.a.Ret()
}

// isReference falls through when v, whose word is in w, is the reference
// slot o held at entry, and jumps to primitive when it is not: when the
// slot held none, or when v is not a load of it and its word is not the
// slot's. Native code makes no word of a reference's kind, so an equal
// word is the slot's value. It uses scratchC.
func (c *compiler) isReference(v *ssa.Value, w amd64.Reg, o int, primitive amd64.Label) {
	base, disp := c.slotAddr(o, true, scratchC)
	c.a.Load(scratchC, base, disp)
	c.a.Op(amd64.Test, scratchC, scratchC, true)
	c.a.Jcc(amd64.CondE, primitive)
	if v.Op != ssa.OpLoadSlot {
		base, disp = c.slotAddr(o, false, scratchC)
		c.a.Load(scratchC, base, disp)
		c.a.Op(amd64.Cmp, w, scratchC, true)
		c.a.Jcc(amd64.CondNE, primitive)
	}
}

// appendRecord adds an abi.Record for slot: its Arg from arg, which
// yields a register once the record's address is in scratchC, or imm when
// useImm; and its Word from word, if not nil, which must not be scratchB
// or scratchC. It uses scratchB and scratchC.
func (c *compiler) appendRecord(slot uint64, arg func() amd64.Reg, imm uint64, useImm bool, word *amd64.Reg) {
	c.recorded = true
	c.a.Load(scratchC, regCtx, abi.OffRecords)
	c.a.ShiftImm(amd64.Shl, scratchC, 5, true)
	c.a.Op(amd64.Add, scratchC, regCtx, true)
	if word != nil {
		c.a.Store(scratchC, abi.OffRecord+16, *word)
	}
	c.storeImm(scratchC, abi.OffRecord, slot, scratchB)
	if useImm {
		c.storeImm(scratchC, abi.OffRecord+8, imm, scratchB)
	} else {
		c.a.Store(scratchC, abi.OffRecord+8, arg())
	}
	c.a.Load(scratchC, regCtx, abi.OffRecords)
	c.a.OpImm(amd64.Add, scratchC, 1, true)
	c.a.Store(regCtx, abi.OffRecords, scratchC)
}

// gprAfter returns a function yielding a register holding v, loading it
// into scratchB if it has none; for appendRecord, which has freed scratchB
// by then.
func (c *compiler) gprAfter(v *ssa.Value) func() amd64.Reg {
	return func() amd64.Reg { return c.gpr(v, scratchB) }
}

// record fills the exit record and returns to Go.
func (c *compiler) record(kind, pc, depth, site uint64, then *amd64.Label) {
	c.storeImm(regCtx, abi.OffExitKind, kind, scratchA)
	c.storeImm(regCtx, abi.OffExitPC, pc, scratchA)
	c.storeImm(regCtx, abi.OffExitDepth, depth, scratchA)
	c.storeImm(regCtx, abi.OffExitSite, site, scratchA)
	switch {
	case !c.recorded && then == nil:
		c.a.Ret()
		return
	case !c.recorded:
		c.a.Jmp(*then)
		return
	case then != nil:
		c.a.LeaLabel(scratchA, *then)
		c.a.Store(regCtx, abi.OffTailReturn, scratchA)
	}
	if !c.tailUsed {
		c.tail, c.tailUsed = c.a.NewLabel(), true
	}
	c.a.Jmp(c.tail)
}

// recordsTail applies an exit's records natively while the collector is
// not marking (abi.Encoding's WriteBarrier), as the VM's jitApplyRecords
// does in Go, and returns; while it marks, Go applies them. It reads every
// value the records write before writing any, since one may read a slot
// another writes: the first pass leaves each record's value in its Word
// and, for its pointer word, its Arg; the second writes them. The pointers
// are held there only meanwhile, where the collector, which cannot run,
// would not see them.
func (c *compiler) recordsTail() {
	ret, read, write, maybe, scalar, copyValue, next := c.a.NewLabel(), c.a.NewLabel(), c.a.NewLabel(),
		c.a.NewLabel(), c.a.NewLabel(), c.a.NewLabel(), c.a.NewLabel()
	c.movImm(scratchB, c.enc.WriteBarrier)
	c.a.LoadU8(scratchB, scratchB, 0)
	c.a.Op(amd64.Test, scratchB, scratchB, false)
	c.a.Jcc(amd64.CondNE, ret)
	c.a.Load(scratchA, regCtx, abi.OffRecords)
	c.a.Op(amd64.Test, scratchA, scratchA, true)
	c.a.Jcc(amd64.CondE, ret)
	c.a.MovQToX(xScratch2, scratchA)
	// From the last record to the first: scratchA its address, xScratch1
	// its index.
	c.a.Bind(read)
	c.a.OpImm(amd64.Sub, scratchA, 1, true)
	c.a.MovQToX(xScratch1, scratchA)
	c.a.ShiftImm(amd64.Shl, scratchA, 5, true)
	c.a.Op(amd64.Add, scratchA, regCtx, true)
	c.a.Load(scratchC, scratchA, abi.OffRecord)
	c.movImm(scratchB, abi.RecordScalar)
	c.a.Op(amd64.Test, scratchC, scratchB, true)
	c.a.Jcc(amd64.CondNE, scalar)
	c.movImm(scratchB, abi.RecordMaybe)
	c.a.Op(amd64.Test, scratchC, scratchB, true)
	c.a.Jcc(amd64.CondNE, maybe)
	// Another slot's value, a reference.
	c.a.Load(scratchC, scratchA, abi.OffRecord+8)
	c.sourceAddr()
	c.a.Bind(copyValue)
	c.a.Load(scratchB, scratchC, c.enc.NumOffset)
	c.a.Store(scratchA, abi.OffRecord+16, scratchB)
	c.a.Load(scratchB, scratchC, c.enc.RefOffset)
	c.a.Store(scratchA, abi.OffRecord+8, scratchB)
	c.a.Jmp(next)
	// Its source's value if that holds a reference, else the primitive.
	c.a.Bind(maybe)
	c.a.Load(scratchC, scratchA, abi.OffRecord+8)
	c.a.Op(amd64.Test, scratchC, scratchC, true)
	c.a.Jcc(amd64.CondE, scalar)
	c.a.Load(scratchB, scratchC, c.enc.RefOffset)
	c.a.Op(amd64.Test, scratchB, scratchB, true)
	c.a.Jcc(amd64.CondNE, copyValue)
	// The primitive Word, with no pointer.
	c.a.Bind(scalar)
	c.storeImm(scratchA, abi.OffRecord+8, 0, scratchB)
	c.a.Bind(next)
	c.a.MovQFromX(scratchA, xScratch1)
	c.a.Op(amd64.Test, scratchA, scratchA, true)
	c.a.Jcc(amd64.CondNE, read)
	c.a.MovQFromX(scratchA, xScratch2)
	c.a.Bind(write)
	c.a.OpImm(amd64.Sub, scratchA, 1, true)
	c.a.MovQToX(xScratch1, scratchA)
	c.a.ShiftImm(amd64.Shl, scratchA, 5, true)
	c.a.Op(amd64.Add, scratchA, regCtx, true)
	c.a.Load(scratchC, scratchA, abi.OffRecord)
	c.movImm(scratchB, ^uint64(abi.RecordScalar|abi.RecordMaybe))
	c.a.Op(amd64.And, scratchC, scratchB, true)
	c.sourceAddr()
	c.a.Load(scratchB, scratchA, abi.OffRecord+16)
	c.a.Store(scratchC, c.enc.NumOffset, scratchB)
	c.a.Load(scratchB, scratchA, abi.OffRecord+8)
	c.a.Store(scratchC, c.enc.RefOffset, scratchB)
	c.a.MovQFromX(scratchA, xScratch1)
	c.a.Op(amd64.Test, scratchA, scratchA, true)
	c.a.Jcc(amd64.CondNE, write)
	c.storeImm(regCtx, abi.OffRecords, 0, scratchB)
	c.a.Bind(ret)
	// A native call's writing of its caller's state goes on in the
	// caller's code (exitThen).
	toGo := c.a.NewLabel()
	c.a.Load(scratchB, regCtx, abi.OffTailReturn)
	c.a.Op(amd64.Test, scratchB, scratchB, true)
	c.a.Jcc(amd64.CondE, toGo)
	c.storeImm(regCtx, abi.OffTailReturn, 0, scratchA)
	c.a.JmpReg(scratchB)
	c.a.Bind(toGo)
	c.a.Ret()
}

// block emits one block. next is the block laid out after it.
func (c *compiler) block(b *ssa.Block, next *ssa.Block) {
	if b.PC < 0 {
		c.a.Load(regLocals, regCtx, abi.OffLocals)
		c.a.Load(regStack, regCtx, abi.OffStack)
		if c.f.StartOnly && c.f.Params < c.f.FrameLocals && b == c.entryAt(0) {
			// The frame's locals past its parameters, undefined, which
			// native callers leave to the code (ssa.Func's StartOnly).
			vs := int32(c.enc.ValueSize)
			c.movImm(scratchA, c.enc.Undefined)
			c.movImm(scratchB, 0)
			for i := c.f.Params; i < c.f.FrameLocals; i++ {
				c.a.Store(regLocals, int32(i)*vs+c.enc.NumOffset, scratchA)
				c.a.Store(regLocals, int32(i)*vs+c.enc.RefOffset, scratchB)
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
		r := c.gpr(b.Control, scratchA)
		c.a.Store(regCtx, abi.OffRet, r)
		c.movImm(scratchC, 0)
		c.a.Store(regCtx, abi.OffRetFrom, scratchC)
		if s := b.Control.Shadow; s != nil {
			// RetFrom is the source, 0 for a primitive's; Go checks that
			// the slot or cell holds a reference.
			c.a.Store(regCtx, abi.OffRetFrom, c.gpr(s, scratchC))
		} else if o, ok := c.origin.Of(b.Control); ok && o >= 0 {
			done := c.a.NewLabel()
			c.isReference(b.Control, r, o, done)
			c.slotSource(o, scratchC)
			c.a.Store(regCtx, abi.OffRetFrom, scratchC)
			c.a.Bind(done)
		}
		c.storeImm(regCtx, abi.OffExitKind, abi.ExitReturn, scratchA)
		// Where it returned from, roughly -- its block's PC -- for the work
		// the stretch did (the VM's jitSSAProfit).
		c.storeImm(regCtx, abi.OffExitSite, uint64(b.PC), scratchA)
		c.returnNative()
	case ssa.BlockExit:
		c.exitTo(b.State, exitKind(int(b.ExitKind)))
	}
}

// edge emits the moves for an edge's phis, a poll on a back-edge, and a jump
// unless the target is laid out next.
func (c *compiler) edge(from, to, next *ssa.Block) {
	idx := -1
	for i, p := range to.Preds {
		if p == from {
			idx = i
		}
	}
	c.phiMoves(to, idx)
	if to.LoopHeader && to.Backedge[idx] {
		poll := c.a.NewLabel()
		c.a.Load(scratchB, regCtx, abi.OffBackEdges)
		c.a.DecMem(scratchB, 0)
		c.a.Jcc(amd64.CondLE, poll)
		header := to.Header
		c.cold = append(c.cold, func() {
			c.a.Bind(poll)
			if c.enc.CallGo != 0 {
				// Go gives the loop another budget (ssa.GoPoll): a native
				// callee's poll had left native code, and Go finished every
				// level of its callers.
				stub := c.stubLabel(header, abi.ExitPoll)
				c.goSaving(uint64(ssa.GoPoll), &stub)
				c.a.Jmp(c.labels[to.ID])
				return
			}
			// The phis hold the header's values now; the state names them,
			// or values live across the loop.
			c.exitTo(header, abi.ExitPoll)
		})
	}
	if to != next {
		c.a.Jmp(c.labels[to.ID])
	}
}

// phiMoves performs the parallel move of an edge's phi arguments into the
// phis' locations, as the core schedules it.
func (c *compiler) phiMoves(to *ssa.Block, idx int) {
	for _, st := range c.phiSchedule(to, idx) {
		switch {
		case st.park == nil:
			c.moveValue(st.dst, st.src, st.parked)
		case isFloat(st.park):
			c.a.SSEOp(amd64.MovAPD, xScratch0, c.xmm(st.park, xScratch0))
		default:
			c.a.MovRR(scratchC, c.gpr(st.park, scratchC))
		}
	}
}

// moveValue copies src into dst's location; parked sources are in scratch.
func (c *compiler) moveValue(dst, src *ssa.Value, parked bool) {
	if isFloat(dst) {
		// A spilled source loads through xScratch1: xScratch0 may hold a
		// parked value still to be moved.
		x := xScratch0
		if !parked {
			x = c.xmm(src, xScratch1)
		}
		c.setX(dst, x)
		return
	}
	if l := c.locAt(dst); l.reg >= 0 && !parked {
		// Straight into its register.
		c.gprInto(src, amd64.Reg(l.reg))
		return
	}
	r := scratchC
	if !parked {
		r = c.gpr(src, scratchA)
	}
	c.setG(dst, r)
}

// branch emits an If block's end, fusing a float comparison into the jump.
func (c *compiler) branch(b *ssa.Block, next *ssa.Block) {
	yes, no := c.a.NewLabel(), c.a.NewLabel()
	ctl := b.Control
	if ctl.Op == ssa.OpCmpF64 && ctl.Uses == 1 && ctl.Block == b {
		x, y := c.xmm(ctl.Args[0], xScratch0), c.xmm(ctl.Args[1], xScratch1)
		switch ir.Operator(ctl.Aux) {
		case ir.Lt:
			c.a.SSEOp(amd64.UcomiSD, y, x)
			c.a.Jcc(amd64.CondA, yes)
		case ir.Le:
			c.a.SSEOp(amd64.UcomiSD, y, x)
			c.a.Jcc(amd64.CondAE, yes)
		case ir.Gt:
			c.a.SSEOp(amd64.UcomiSD, x, y)
			c.a.Jcc(amd64.CondA, yes)
		case ir.Ge:
			c.a.SSEOp(amd64.UcomiSD, x, y)
			c.a.Jcc(amd64.CondAE, yes)
		case ir.Eq:
			c.a.SSEOp(amd64.UcomiSD, x, y)
			c.a.Jcc(amd64.CondNE, no)
			c.a.Jcc(amd64.CondNP, yes)
		case ir.Ne:
			c.a.SSEOp(amd64.UcomiSD, x, y)
			c.a.Jcc(amd64.CondNE, yes)
			c.a.Jcc(amd64.CondP, yes)
		}
	} else {
		r := c.gpr(ctl, scratchA)
		c.a.Op(amd64.Test, r, r, false)
		c.a.Jcc(amd64.CondNE, yes)
	}
	// The false edge follows.
	c.a.Bind(no)
	c.edge(b, b.Succs[1], nil)
	c.a.Bind(yes)
	c.edge(b, b.Succs[0], next)
}

// arrayOf finds the array a value is, as objectOf finds an object, and
// checks its class.
func (c *compiler) arrayOf(v *ssa.Value, guard func(amd64.Cond)) {
	if !c.objectOf(v, guard) {
		return
	}
	c.a.LoadU8(scratchB, scratchC, c.enc.ObjectClass)
	c.a.OpImm(amd64.Cmp, scratchB, int32(c.enc.ClassArray), false)
	guard(amd64.CondNE)
	c.setG(v, scratchC)
}

// instanceOf answers whether its value has the object its second argument
// is among its prototypes (OpInstanceOf): false for a value not an object;
// otherwise along the chain from the object's own prototype, leaving to Go
// at a proxy. It uses every scratch register.
func (c *compiler) instanceOf(v *ssa.Value, guard func(amd64.Cond)) {
	a := v.Args[0]
	if a.Shadow == nil && c.origin.At(a) < 0 {
		// A primitive, never an instance.
		c.movImm(scratchB, 0)
		c.setG(v, scratchB)
		return
	}
	no, loop, done := c.a.NewLabel(), c.a.NewLabel(), c.a.NewLabel()
	w := c.gpr(a, scratchA)
	c.cmpWord(w, c.enc.Object)
	c.a.Jcc(amd64.CondNE, no)
	c.sourceRef(a, guard)
	c.a.Op(amd64.Test, scratchC, scratchC, true)
	guard(amd64.CondE)
	p := c.gpr(v.Args[1], scratchA)
	c.a.Bind(loop)
	c.a.LoadU8(scratchB, scratchC, c.enc.ObjectClass)
	c.a.OpImm(amd64.Cmp, scratchB, int32(c.enc.ClassProxy), false)
	guard(amd64.CondE)
	c.a.Load(scratchC, scratchC, c.enc.ObjectProto)
	c.a.Op(amd64.Test, scratchC, scratchC, true)
	c.a.Jcc(amd64.CondE, no)
	c.a.Op(amd64.Cmp, scratchC, p, true)
	c.a.Jcc(amd64.CondNE, loop)
	c.movImm(scratchB, 1)
	c.a.Jmp(done)
	c.a.Bind(no)
	c.movImm(scratchB, 0)
	c.a.Bind(done)
	c.setG(v, scratchB)
}

// typeIs tells whether typeof its value is the type v.Index says
// (OpTypeIs): a number's word is below NumberLimit, a string's, a
// boolean's and undefined's are theirs; an object with [[IsHTMLDDA]] is
// undefined, one of ClassFunction a function and one of ClassObject or
// ClassArray neither; another exits. It uses every scratch register.
func (c *compiler) typeIs(v *ssa.Value, guard func(amd64.Cond)) {
	a := v.Args[0]
	yes, no, done := c.a.NewLabel(), c.a.NewLabel(), c.a.NewLabel()
	w := c.gpr(a, scratchB)
	is := func(word uint64) {
		c.cmpWord(w, word)
		c.a.Jcc(amd64.CondE, yes)
	}
	switch v.Index {
	case ir.TypeNumber:
		c.cmpWord(w, abi.NumberLimit)
		c.a.Jcc(amd64.CondB, yes)
	case ir.TypeString:
		is(c.enc.String)
	case ir.TypeBoolean:
		is(c.enc.True)
		is(c.enc.False)
	case ir.TypeUndefined, ir.TypeFunction:
		if v.Index == ir.TypeUndefined {
			is(c.enc.Undefined)
		}
		c.cmpWord(w, c.enc.Object)
		c.a.Jcc(amd64.CondNE, no)
		if c.reference(v, a, c.enc.Object, guard) {
			c.a.LoadU8(scratchA, scratchC, c.enc.ObjectFlags)
			c.a.OpImm(amd64.And, scratchA, int32(c.enc.FlagHTMLDDA), false)
			if v.Index == ir.TypeUndefined {
				c.a.Jcc(amd64.CondNE, yes)
				c.a.Jmp(no)
			} else {
				c.a.Jcc(amd64.CondNE, no)
				c.a.LoadU8(scratchA, scratchC, c.enc.ObjectClass)
				c.a.OpImm(amd64.Cmp, scratchA, int32(c.enc.ClassFunction), false)
				c.a.Jcc(amd64.CondE, yes)
				c.a.OpImm(amd64.Cmp, scratchA, int32(c.enc.ClassObject), false)
				c.a.Jcc(amd64.CondE, no)
				c.a.OpImm(amd64.Cmp, scratchA, int32(c.enc.ClassArray), false)
				guard(amd64.CondNE)
			}
		}
	}
	c.a.Bind(no)
	c.movImm(scratchA, v.Const.Bits)
	c.a.Jmp(done)
	c.a.Bind(yes)
	c.movImm(scratchA, v.Const.Bits^1)
	c.a.Bind(done)
	c.setG(v, scratchA)
}

// objectOf finds the object a value is, into scratchC: the pointer word of
// the slot it came from, which still holds it (origin.go). A value with an
// object's word is its origin's object, since native code makes no such
// word. It reports false when the value can never be one, having emitted
// the jump to the exit.
func (c *compiler) objectOf(v *ssa.Value, guard func(amd64.Cond)) bool {
	return c.reference(v, v.Args[0], c.enc.Object, guard)
}

// reference finds the reference a is, of the kind whose word is word, into
// scratchC, as objectOf finds an object, exiting to v's state when a is not
// one. It uses scratchA and scratchB.
func (c *compiler) reference(v, a *ssa.Value, word uint64, guard func(amd64.Cond)) bool {
	o := c.origin.At(a)
	if a.Shadow == nil && o < 0 {
		// A primitive is never a reference.
		c.a.Jmp(c.stubLabel(v.State, exitKind(v.Aux)))
		return false
	}
	w := c.gpr(a, scratchA)
	c.movImm(scratchB, word)
	c.a.Op(amd64.Cmp, w, scratchB, true)
	guard(amd64.CondNE)
	c.sourceRef(a, guard)
	c.a.Op(amd64.Test, scratchC, scratchC, true)
	guard(amd64.CondE)
	return true
}

// sourceRef loads the pointer word of a, a value with a source -- an origin
// that is a slot, or a shadow -- from there into scratchC. A shadow of 0,
// a primitive's, exits. It uses scratchA and scratchB.
func (c *compiler) sourceRef(a *ssa.Value, guard func(amd64.Cond)) {
	o := c.origin.At(a)
	if s := a.Shadow; s != nil && cellSource(s) {
		c.a.Load(scratchC, c.gpr(s, scratchC), c.enc.RefOffset)
	} else if s != nil {
		// The source is known at run time: a local's, a captured
		// binding's, the receiver's, an operand's or a heap cell's address
		// (origin.go).
		r := c.gpr(s, scratchC)
		c.a.Op(amd64.Test, r, r, true)
		guard(amd64.CondE)
		c.a.Load(scratchC, r, c.enc.RefOffset)
	} else {
		base, disp := c.slotAddr(o, true, scratchC)
		c.a.Load(scratchC, base, disp)
	}
}

// sourceAddr turns the slot in scratchC, a record's, into the address of
// its value (slotSource); a heap cell's address, at or above
// abi.MaxRecords, it leaves as it is. It uses scratchB.
func (c *compiler) sourceAddr() {
	captured, stack, found := c.a.NewLabel(), c.a.NewLabel(), c.a.NewLabel()
	c.a.OpImm(amd64.Cmp, scratchC, abi.MaxRecords, true)
	c.a.Jcc(amd64.CondAE, found)
	c.a.OpImm(amd64.Cmp, scratchC, int32(c.f.FrameLocals), false)
	c.a.Jcc(amd64.CondGE, captured)
	c.a.ShiftImm(amd64.Shl, scratchC, 4, true)
	c.a.Op(amd64.Add, scratchC, regLocals, true)
	c.a.Jmp(found)
	c.a.Bind(captured)
	c.a.OpImm(amd64.Cmp, scratchC, int32(c.f.Locals), false)
	c.a.Jcc(amd64.CondGE, stack)
	if t := c.f.ThisSlot; t >= 0 {
		notThis := c.a.NewLabel()
		c.a.OpImm(amd64.Cmp, scratchC, int32(t), false)
		c.a.Jcc(amd64.CondNE, notThis)
		c.a.MovRR(scratchC, regCtx)
		c.a.OpImm(amd64.Add, scratchC, abi.OffThis, true)
		c.a.Jmp(found)
		c.a.Bind(notThis)
	}
	c.a.OpImm(amd64.Sub, scratchC, int32(c.f.FrameLocals), false)
	c.a.ShiftImm(amd64.Shl, scratchC, 3, true)
	c.a.Load(scratchB, regCtx, abi.OffUpvalues)
	c.a.Op(amd64.Add, scratchC, scratchB, true)
	c.a.Load(scratchC, scratchC, 0)
	c.a.Load(scratchC, scratchC, c.enc.UpvalueSlot)
	c.a.Jmp(found)
	c.a.Bind(stack)
	c.a.OpImm(amd64.Sub, scratchC, int32(c.f.Locals), false)
	c.a.ShiftImm(amd64.Shl, scratchC, 4, true)
	c.a.Op(amd64.Add, scratchC, regStack, true)
	c.a.Bind(found)
}

// propStore stores a value in a property, as the VM's setPropCached does
// in an own writable data property: its number word, and its pointer
// word, which it finds where the value came from, none for a primitive
// (pointerWord). A store that changes a pointer word -- the value's, or the
// one it replaces -- is left to Go while the collector marks
// (abi.Encoding's WriteBarrier), as is a store to a cell a live reference
// was loaded from (the operands after the value: ssa's storeChecks).
// Everything that exits comes before the first write. It uses every
// scratch register and xScratch1.
func (c *compiler) propStore(v *ssa.Value, guard func(amd64.Cond)) {
	x := v.Args[1]
	var w amd64.Reg
	if remat(x) {
		c.materialize(x, scratchA)
		w = scratchA
	} else {
		w = c.gpr(x, scratchA)
	}
	// A number, the usual value, has none, which a compare says; anything
	// else's is read where it came from.
	number, have := c.a.NewLabel(), c.a.NewLabel()
	if x.Shadow != nil || c.origin.At(x) >= 0 {
		c.cmpWord(w, abi.NumberLimit)
		c.a.Jcc(amd64.CondB, number)
	}
	c.pointerWord(x, w)
	c.a.Jmp(have)
	c.a.Bind(number)
	c.movImm(scratchB, 0)
	c.a.Bind(have)
	c.a.MovQToX(xScratch1, scratchB)
	added := c.a.NewLabel()
	var miss amd64.Label
	if v.Add() != nil || len(v.Adds()) != 0 {
		// A property the cache adds, along the transition of the object's
		// shape among those the site met, or else one the object has.
		adds := v.Adds()
		if v.Add() != nil {
			adds = append([]*ssa.PropertyAdd{v.Add()}, adds...)
		}
		for _, add := range adds {
			next := c.a.NewLabel()
			c.addAlong(v, add, next)
			c.a.Jmp(added)
			c.a.Bind(next)
		}
		if v.Add() != nil && v.Add().Define {
			// A literal's field: added so, or by Go.
			c.a.Jmp(c.stubLabel(v.State, exitKind(v.Aux)))
			c.a.Bind(added)
			return
		}
	}
	if v.Upvalue {
		// A captured binding's cell, which holds any value.
		if r := c.gpr(v.Args[0], scratchA); r != scratchA {
			c.a.MovRR(scratchA, r)
		}
	} else if v.Global {
		// A global binding's cell, found as a read finds it, and
		// writable: its attributes are its object's shape's, at the cell's
		// position.
		if r := c.gpr(v.Args[0], scratchA); r != scratchA {
			c.a.MovRR(scratchA, r)
		}
		cell := v.Args[0]
		switch cell.Op {
		case ssa.OpGlobalCell:
			c.a.Load(scratchB, regCtx, abi.OffGlobal)
		case ssa.OpLexCell:
			c.a.Load(scratchB, regCtx, abi.OffGlobalLex)
		case ssa.OpBindingCell:
			c.movImm(scratchB, cell.Const.Bits)
		default:
			panic("mir: a global binding's write of no binding's cell")
		}
		c.a.Load(scratchB, scratchB, c.enc.ObjectShape)
		c.a.Load(scratchB, scratchB, c.enc.ShapeKeys)
		c.a.LoadU8(scratchB, scratchB, int32(cell.Index)*c.enc.KeySize+c.enc.PropertyFlags)
		c.a.OpImm(amd64.And, scratchB, int32(c.enc.PropNotWritable), false)
		c.a.OpImm(amd64.Cmp, scratchB, int32(c.enc.PropWritable), false)
		guard(amd64.CondNE)
	} else if v.Type == ssa.Bool {
		// A megamorphic site's write whose misses Go makes, on the branch
		// its result, true there, controls (ssa's OpPropWrite).
		miss = c.a.NewLabel()
		c.property(v, func(cond amd64.Cond) { c.a.Jcc(cond, miss) })
	} else {
		c.property(v, guard)
	}
	for _, s := range v.Args[2:] {
		// A live value read from this cell keeps its pointer word there:
		// unless it is none, a primitive's (storeChecks).
		other := c.a.NewLabel()
		c.a.Op(amd64.Cmp, c.gpr(s, scratchB), scratchA, true)
		c.a.Jcc(amd64.CondNE, other)
		c.a.Load(scratchB, scratchA, c.enc.RefOffset)
		c.a.Op(amd64.Test, scratchB, scratchB, true)
		guard(amd64.CondNE)
		c.a.Bind(other)
	}
	scalar, viaGo, done := c.a.NewLabel(), c.a.NewLabel(), c.a.NewLabel()
	c.a.MovQFromX(scratchC, xScratch1)
	c.a.Load(scratchB, scratchA, c.enc.RefOffset)
	c.a.Op(amd64.Or, scratchB, scratchC, true)
	c.a.Jcc(amd64.CondE, scalar)
	// A pointer word written or overwritten: while the collector marks,
	// with Go's write barrier (valueStore).
	c.a.MovQToX(xScratch0, c.gpr(x, scratchB))
	c.movImm(scratchB, c.enc.WriteBarrier)
	c.a.LoadU8(scratchB, scratchB, 0)
	c.a.Op(amd64.Test, scratchB, scratchB, false)
	c.a.Jcc(amd64.CondNE, viaGo)
	c.a.Bind(scalar)
	c.a.Store(scratchA, c.enc.NumOffset, c.gpr(x, scratchB))
	c.a.Store(scratchA, c.enc.RefOffset, scratchC)
	c.a.Bind(done)
	c.a.Bind(added)
	if v.Type == ssa.Bool {
		// Stored: false; a miss, true.
		stored := c.a.NewLabel()
		c.movImm(scratchA, 0)
		c.setG(v, scratchA)
		c.a.Bind(stored)
		c.cold = append(c.cold, func() {
			c.a.Bind(miss)
			c.movImm(scratchA, 1)
			c.setG(v, scratchA)
			c.a.Jmp(stored)
		})
	}
	c.cold = append(c.cold, func() {
		c.a.Bind(viaGo)
		c.valueStore()
		c.a.Jmp(done)
	})
}

// valueStore stores a value into a cell while the collector marks, as
// V8's code calls its write barrier: the cell's address in scratchA, the
// value's words in xScratch0 and scratchC. Its number word is stored as any
// is, its pointer word with Go's write barrier (refStore). Every
// allocatable register is saved across it, and none of the scratch ones.
func (c *compiler) valueStore() {
	c.a.MovQFromX(scratchB, xScratch0)
	c.a.Store(scratchA, c.enc.NumOffset, scratchB)
	c.a.OpImm(amd64.Add, scratchA, c.enc.RefOffset, true)
	c.refStore()
}

// refStore stores a pointer word while the collector marks, with Go's
// write barrier, which shades the pointer it overwrites and the one it
// writes, as V8's code calls its write barrier: the word's address in
// scratchA, the pointer in scratchC. Every allocatable register is saved
// across it, and none of the scratch ones. The stores share a routine
// (refStoring), which saves them and jumps to storeRef (abi.Encoding's
// StoreRef); a site is where it comes back to, which Go never refuses.
func (c *compiler) refStore() {
	if !c.refStoringUsed {
		c.refStoring, c.refStoringUsed = c.a.NewLabel(), true
	}
	back := c.a.NewLabel()
	c.a.LeaLabel(scratchB, back)
	c.a.Store(regCtx, abi.OffGoSite, scratchB)
	c.a.Jmp(c.refStoring)
	c.a.Bind(back)
}

// refStoringCall is the routine refStore's sites share: every allocatable
// register saved in the context's register area, as savingCall saves
// them; storeRef, which comes back to it, where scratchB says; the frame's
// registers read again, and every allocatable one restored; then back to
// the site, at GoSite.
func (c *compiler) refStoringCall() {
	for _, r := range c.usedRegs(c.gprs, false) {
		c.a.Store(regCtx, abi.OffRegs+int32(r)*8, amd64.Reg(r))
	}
	for _, r := range c.usedRegs(c.fprs, true) {
		c.a.StoreSD(regCtx, abi.OffXRegs+int32(r)*8, amd64.XReg(r))
	}
	back := c.a.NewLabel()
	c.a.LeaLabel(scratchB, back)
	// The frame's locals, read again below, are the jump's register.
	c.movImm(regLocals, c.enc.StoreRef)
	c.a.JmpReg(regLocals)
	c.a.Bind(back)
	c.a.Load(regLocals, regCtx, abi.OffLocals)
	c.a.Load(regStack, regCtx, abi.OffStack)
	for _, r := range c.usedRegs(c.gprs, false) {
		c.a.Load(amd64.Reg(r), regCtx, abi.OffRegs+int32(r)*8)
	}
	for _, r := range c.usedRegs(c.fprs, true) {
		c.a.LoadSD(amd64.XReg(r), regCtx, abi.OffXRegs+int32(r)*8)
	}
	c.a.JmpMem(regCtx, abi.OffGoSite)
}

// goAppending is goSaving of an element write's store (ssa.GoElemWrite),
// its operands in the context's GoArgs, through appendingCall, which
// appends a number at an array's length natively where it may and calls
// Go for the rest: if Go refuses, stub.
func (c *compiler) goAppending(stub *amd64.Label) {
	if !c.appendingUsed {
		c.appending, c.appendingUsed = c.a.NewLabel(), true
	}
	back := c.a.NewLabel()
	c.a.LeaLabel(scratchA, back)
	c.a.Store(regCtx, abi.OffGoSite, scratchA)
	c.a.Jmp(c.appending)
	c.a.Bind(back)
	c.a.Load(scratchB, regCtx, abi.OffGoStatus)
	c.a.Op(amd64.Test, scratchB, scratchB, true)
	c.a.Jcc(amd64.CondNE, *stub)
}

// appendingCall is the routine the element writes Go makes share
// (goAppending), their array, key and number in the context's GoArgs: a
// number at its array's length it appends, as Go's jitStoreElem would and
// a push does natively (pushGuards, push), where the array has room, is
// extensible, its length writable and dense, and its prototypes are the
// function's (ssa.Func's ArrayProtos), as they were -- the element's words
// the number's and a nil pointer, the length one more, neither a pointer
// written, so no barrier -- and goes back to the site, at GoSite, with
// nothing refused; anything else Go stores, through savingCall, which goes
// back there. Go had made each such append, a call of it from native code
// for every element an array was filled with: one routine of the code's
// makes them, as savingCall makes its calls of Go, so that a site, which
// any element write, a typed array's included, has, is no longer.
func (c *compiler) appendingCall() {
	size := int32(unsafe.Sizeof(abi.GoArg{}))
	protos := c.f.ArrayProtos
	toGo := c.a.NewLabel()
	fail := func(cond amd64.Cond) { c.a.Jcc(cond, toGo) }
	// The key, an index in [0, 2**32), as index makes it, and the array's
	// length.
	c.a.Load(scratchA, regCtx, abi.OffGoArgs+size+c.enc.NumOffset)
	c.a.MovQToX(xScratch0, scratchA)
	c.a.Cvttsd2si(scratchA, xScratch0)
	c.a.Cvtsi2sd(xScratch1, scratchA, true)
	c.a.SSEOp(amd64.UcomiSD, xScratch1, xScratch0)
	fail(amd64.CondNE)
	fail(amd64.CondP)
	c.a.MovRR(scratchB, scratchA)
	c.a.ShiftImm(amd64.Shr, scratchB, 32, true)
	fail(amd64.CondNE)
	c.a.Load(scratchC, regCtx, abi.OffGoArgs+c.enc.RefOffset)
	c.a.Load(scratchB, scratchC, c.enc.ObjectElems+8)
	c.a.Op(amd64.Cmp, scratchA, scratchB, true)
	hole := c.a.NewLabel()
	c.a.Jcc(amd64.CondB, hole)
	fail(amd64.CondNE)
	c.a.Load(scratchB, scratchC, c.enc.ObjectElems+16)
	c.a.Op(amd64.Cmp, scratchA, scratchB, true)
	fail(amd64.CondAE)
	c.arrayGuards(scratchC, protos, fail)
	// The length one more, then the element at the old one: the address
	// past it, less a value.
	c.a.Load(scratchC, regCtx, abi.OffGoArgs+c.enc.RefOffset)
	c.a.Load(scratchA, scratchC, c.enc.ObjectElems+8)
	c.a.OpImm(amd64.Add, scratchA, 1, true)
	c.a.Store(scratchC, c.enc.ObjectElems+8, scratchA)
	c.a.ShiftImm(amd64.Shl, scratchA, 4, true)
	c.a.Load(scratchB, scratchC, c.enc.ObjectElems)
	c.a.Op(amd64.Add, scratchA, scratchB, true)
	c.a.Load(scratchB, regCtx, abi.OffGoArgs+2*size+c.enc.NumOffset)
	c.a.MovQToX(xScratch0, scratchB)
	c.boxF64(xScratch0, scratchB)
	c.a.Store(scratchA, c.enc.NumOffset-16, scratchB)
	c.storeImm(scratchA, c.enc.RefOffset-16, 0, scratchB)
	c.storeImm(regCtx, abi.OffGoStatus, 0, scratchB)
	c.a.Load(scratchA, regCtx, abi.OffGoSite)
	c.a.JmpReg(scratchA)
	// A key below the length: a hole, which the number fills as V8's code
	// stores into a holey array's, where the array may take an append; any
	// other element Go writes. A hole has no pointer word, nor does the
	// number: no barrier.
	c.a.Bind(hole)
	c.a.Load(scratchB, scratchC, c.enc.ObjectElems)
	c.a.ShiftImm(amd64.Shl, scratchA, 4, true)
	c.a.Op(amd64.Add, scratchA, scratchB, true)
	c.a.Load(scratchB, scratchA, c.enc.NumOffset)
	c.cmpWord(scratchB, c.enc.Uninitialized)
	fail(amd64.CondNE)
	c.a.MovQToX(xScratch1, scratchA)
	c.arrayGuards(scratchC, protos, fail)
	c.a.MovQFromX(scratchA, xScratch1)
	c.a.Load(scratchB, regCtx, abi.OffGoArgs+2*size+c.enc.NumOffset)
	c.a.MovQToX(xScratch0, scratchB)
	c.boxF64(xScratch0, scratchB)
	c.a.Store(scratchA, c.enc.NumOffset, scratchB)
	c.storeImm(regCtx, abi.OffGoStatus, 0, scratchB)
	c.a.Load(scratchA, regCtx, abi.OffGoSite)
	c.a.JmpReg(scratchA)
	c.a.Bind(toGo)
	c.storeImm(regCtx, abi.OffGoOp, uint64(ssa.GoElemWrite), scratchA)
	c.storeImm(regCtx, abi.OffGoKeep, 0, scratchA)
	if !c.savingUsed {
		c.saving, c.savingUsed = c.a.NewLabel(), true
	}
	c.a.Jmp(c.saving)
}

// arrayGuards checks that array p may take an element where it has none
// -- appended at its length, or stored into a hole -- as Go's jitStoreElem
// would store it: an Array, extensible, its length writable, its elements
// dense, so that no index is a property of its own, and its prototypes the
// function's (ssa.Func's ArrayProtos), as they were, holding no element nor
// index that could intercept the store. It uses scratchA and scratchB.
func (c *compiler) arrayGuards(p amd64.Reg, protos [2]ssa.Holder, fail func(amd64.Cond)) {
	c.a.LoadU8(scratchB, p, c.enc.ObjectClass)
	c.a.OpImm(amd64.Cmp, scratchB, int32(c.enc.ClassArray), false)
	fail(amd64.CondNE)
	c.a.LoadU8(scratchB, p, c.enc.ObjectFlags)
	want := c.enc.FlagExtensible | c.enc.FlagLengthWritable
	c.a.OpImm(amd64.And, scratchB, int32(want|c.enc.FlagSparse), false)
	c.a.OpImm(amd64.Cmp, scratchB, int32(want), false)
	fail(amd64.CondNE)
	c.a.Load(scratchB, p, c.enc.ObjectProto)
	c.cmpWord(scratchB, uint64(protos[0].Object))
	fail(amd64.CondNE)
	c.protoGuards(protos, fail)
}

// goSaving calls Go, op, its operands in the context's GoArgs, every
// allocatable register saved across it in the context's register area,
// which only an exit uses; if Go refuses, stub, if any.
func (c *compiler) goSaving(op uint64, stub *amd64.Label) {
	c.goSavingKeep(op, 0, stub)
}

// goSavingKeep is goSaving whose result Go writes to keep cell keep.
func (c *compiler) goSavingKeep(op uint64, keep int, stub *amd64.Label) {
	st, _ := c.goState(op, keep, nil, nil)
	c.goCalling(st, stub)
}

// goCalling calls Go as st describes it (abi.GoCallState): through the
// routine the code's calls of Go share (calling, then savingCall), which
// comes back here, the description's address in scratchC and where it
// comes back in scratchA -- a site is little more than its description, not
// every register's save and restore, nor its GoOp, GoKeep and operands'
// stores, as V8's code calls its RecordWrite and CallRuntime builtins. If
// Go does nothing, it goes on at stub instead, if not nil, which Go sends
// it to (Fail): no site tests GoStatus.
func (c *compiler) goCalling(st *abi.GoCallState, stub *amd64.Label) {
	if !c.callingUsed {
		c.calling, c.callingUsed = c.a.NewLabel(), true
	}
	if !c.savingUsed {
		c.saving, c.savingUsed = c.a.NewLabel(), true
	}
	back := c.a.NewLabel()
	c.movImm(scratchC, uint64(uintptr(unsafe.Pointer(st))))
	c.a.LeaLabel(scratchA, back)
	c.a.Jmp(c.calling)
	c.a.Bind(back)
	if stub != nil {
		c.goFails = append(c.goFails, goFail{st, back, *stub})
	}
}

// goCallingLive calls Go as st describes it, for call v, whose state's
// values live past it (allocate): what is live in registers saved and
// restored here, through the routine such calls share (goLive), which
// saves nothing. Refused, it goes to stub once it has restored them.
func (c *compiler) goCallingLive(v *ssa.Value, st *abi.GoCallState, stub amd64.Label) {
	saves := c.savesOf(v)
	for _, sv := range saves {
		if sv.float {
			c.a.StoreSD(regCtx, c.spillDisp(sv.slot), amd64.XReg(sv.reg))
		} else {
			c.a.Store(regCtx, c.spillDisp(sv.slot), amd64.Reg(sv.reg))
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
	c.movImm(scratchC, uint64(uintptr(unsafe.Pointer(st))))
	c.a.LeaLabel(scratchA, back)
	c.a.Jmp(*routine)
	c.a.Bind(back)
	restore := func() {
		for _, sv := range saves {
			if sv.float {
				c.a.LoadSD(amd64.XReg(sv.reg), regCtx, c.spillDisp(sv.slot))
			} else {
				c.a.Load(amd64.Reg(sv.reg), regCtx, c.spillDisp(sv.slot))
			}
		}
	}
	restore()
	c.goFails = append(c.goFails, goFail{st, back, fail})
	c.cold = append(c.cold, func() {
		c.a.Bind(fail)
		restore()
		c.a.Jmp(stub)
	})
}

// liveCall is the routine of goCallingLive's calls: the description and
// where the call goes on (GoSite) in the context, Go called -- through
// target, CallGo or BigFast -- its return to the code's resumeLive -- the
// frame's registers read again -- and on to GoSite.
func (c *compiler) liveCall(target uint64) {
	resume := c.a.NewLabel()
	c.a.Store(regCtx, abi.OffGoState, scratchC)
	c.a.Store(regCtx, abi.OffGoSite, scratchA)
	c.a.LeaLabel(scratchA, resume)
	c.a.Store(regCtx, abi.OffGoResume, scratchA)
	c.movImm(scratchA, target)
	c.a.JmpReg(scratchA)
	c.a.Bind(resume)
	c.a.Load(regLocals, regCtx, abi.OffLocals)
	c.a.Load(regStack, regCtx, abi.OffStack)
	c.a.JmpMem(regCtx, abi.OffGoSite)
}

// savingCall is the routine the code's calls of Go that save every register
// share (goSaving): every allocatable register saved in the context's
// register area, which only an exit uses; Go called (callGo), which comes
// back to it; the frame's registers read again, and every allocatable one
// restored; then back to the call, at GoSite.
func (c *compiler) savingCall() {
	for _, r := range c.usedRegs(c.gprs, false) {
		c.a.Store(regCtx, abi.OffRegs+int32(r)*8, amd64.Reg(r))
	}
	for _, r := range c.usedRegs(c.fprs, true) {
		c.a.StoreSD(regCtx, abi.OffXRegs+int32(r)*8, amd64.XReg(r))
	}
	back := c.a.NewLabel()
	c.a.LeaLabel(scratchA, back)
	c.a.Store(regCtx, abi.OffGoResume, scratchA)
	c.movImm(scratchA, c.enc.CallGo)
	c.a.JmpReg(scratchA)
	c.a.Bind(back)
	c.a.Load(regLocals, regCtx, abi.OffLocals)
	c.a.Load(regStack, regCtx, abi.OffStack)
	for _, r := range c.usedRegs(c.gprs, false) {
		c.a.Load(amd64.Reg(r), regCtx, abi.OffRegs+int32(r)*8)
	}
	for _, r := range c.usedRegs(c.fprs, true) {
		c.a.LoadSD(amd64.XReg(r), regCtx, abi.OffXRegs+int32(r)*8)
	}
	c.a.Load(scratchA, regCtx, abi.OffGoSite)
	c.a.JmpReg(scratchA)
}

// addAlong adds the property a write's cache adds (ssa.PropertyAdd), as
// the VM's appendTransition does, when the object may take it -- of the
// shape it is added to, extensible, its prototypes those the cache found
// nothing on that intercepts the write, room in its table -- and goes to
// miss otherwise, before it writes anything. It writes the length, the
// next shape, a pointer, and the entry past the last, the value's pointer
// word from xScratch1: while the collector marks, the pointers with Go's
// write barrier (addAlongMarking). It uses every scratch register.
func (c *compiler) addAlong(v *ssa.Value, add *ssa.PropertyAdd, miss amd64.Label) {
	x := v.Args[1]
	if c.enc.ValueSize != 16 {
		// The offset is a shift.
		c.a.Jmp(miss)
		return
	}
	o := c.gpr(v.Args[0], scratchA)
	c.a.Load(scratchB, o, c.enc.ObjectShape)
	c.cmpWord(scratchB, uint64(add.From))
	c.a.Jcc(amd64.CondNE, miss)
	c.a.LoadU8(scratchB, o, c.enc.ObjectFlags)
	c.a.OpImm(amd64.And, scratchB, int32(c.enc.FlagExtensible), false)
	c.a.Jcc(amd64.CondE, miss)
	// The prototype chain the cache found, object by object, then none;
	// none for a literal's field, which its prototypes do not intercept.
	c.a.Load(scratchB, o, c.enc.ObjectProto)
	if !add.Define {
		for _, h := range add.Chain() {
			c.movImm(scratchC, uint64(h.Object))
			c.a.Op(amd64.Cmp, scratchB, scratchC, true)
			c.a.Jcc(amd64.CondNE, miss)
			c.a.Load(scratchB, scratchC, c.enc.ObjectShape)
			c.cmpWord(scratchB, uint64(h.Shape))
			c.a.Jcc(amd64.CondNE, miss)
			c.movImm(scratchC, uint64(h.Object))
			c.a.Load(scratchB, scratchC, c.enc.ObjectProto)
		}
		c.a.Op(amd64.Test, scratchB, scratchB, true)
		c.a.Jcc(amd64.CondNE, miss)
	}
	// Room: the table's length below its capacity.
	c.a.Load(scratchB, o, c.enc.ObjectProps+8)
	c.a.Load(scratchC, o, c.enc.ObjectProps+16)
	c.a.Op(amd64.Cmp, scratchB, scratchC, true)
	c.a.Jcc(amd64.CondAE, miss)
	// The collector not marking; while it marks, Go adds it.
	c.movImm(scratchC, c.enc.WriteBarrier)
	c.a.LoadU8(scratchC, scratchC, 0)
	c.a.Op(amd64.Test, scratchC, scratchC, false)
	marked, done := c.a.NewLabel(), c.a.NewLabel()
	c.a.Jcc(amd64.CondNE, marked)
	c.cold = append(c.cold, func() {
		c.a.Bind(marked)
		c.addAlongMarking(v, add, done)
	})
	// The length, then the shape, which has the key, then the value, at
	// the old length times a value's size, 16.
	c.a.MovRR(scratchC, scratchB)
	c.a.OpImm(amd64.Add, scratchB, 1, true)
	c.a.Store(o, c.enc.ObjectProps+8, scratchB)
	c.storeImm(o, c.enc.ObjectShape, uint64(add.Next), scratchB)
	c.a.ShiftImm(amd64.Shl, scratchC, 4, true)
	c.a.Load(scratchB, o, c.enc.ObjectProps)
	c.a.Op(amd64.Add, scratchC, scratchB, true)
	// The pointer word first: making a lazy value's number word may use
	// xScratch1.
	c.a.MovQFromX(scratchB, xScratch1)
	c.a.Store(scratchC, c.enc.RefOffset, scratchB)
	c.a.Store(scratchC, c.enc.NumOffset, c.gpr(x, scratchA))
	c.a.Bind(done)
}

// addAlongMarking is addAlong's add while the collector marks, the table's
// length in scratchB, the value's pointer word in xScratch1: the length,
// then the shape and the value's pointer word with Go's write barrier
// (refStore), which keeps neither a scratch register nor xScratch1 -- the
// entry's address and the pointer word wait in the context's GoArgs,
// integers the collector does not read, the value kept by its source --
// then done.
func (c *compiler) addAlongMarking(v *ssa.Value, add *ssa.PropertyAdd, done amd64.Label) {
	x := v.Args[1]
	o := c.gpr(v.Args[0], scratchA)
	c.a.MovRR(scratchC, scratchB)
	c.a.OpImm(amd64.Add, scratchB, 1, true)
	c.a.Store(o, c.enc.ObjectProps+8, scratchB)
	c.a.ShiftImm(amd64.Shl, scratchC, 4, true)
	c.a.Load(scratchB, o, c.enc.ObjectProps)
	c.a.Op(amd64.Add, scratchC, scratchB, true)
	c.a.Store(regCtx, abi.OffGoArgs+c.enc.NumOffset, scratchC)
	c.a.MovQFromX(scratchB, xScratch1)
	c.a.Store(regCtx, abi.OffGoArgs+c.enc.RefOffset, scratchB)
	c.a.Lea(scratchA, o, c.enc.ObjectShape)
	c.movImm(scratchC, uint64(add.Next))
	c.refStore()
	// The number word, then the pointer word: making a lazy value's number
	// word may use xScratch1.
	c.a.Load(scratchC, regCtx, abi.OffGoArgs+c.enc.NumOffset)
	c.a.Store(scratchC, c.enc.NumOffset, c.gpr(x, scratchA))
	c.a.Load(scratchA, regCtx, abi.OffGoArgs+c.enc.NumOffset)
	c.a.OpImm(amd64.Add, scratchA, c.enc.RefOffset, true)
	c.a.Load(scratchC, regCtx, abi.OffGoArgs+c.enc.RefOffset)
	c.refStore()
	c.a.Jmp(done)
}

// property finds the property a property operation names and leaves the
// address of its value in scratchA. An object of the shape the site's cache
// knows has it at the cached index: the shape settles where it is and what
// it is. At a megamorphic site (ssa.Value's Megamorphic) any other ordinary
// object with a small table is searched for the key, as the VM's own small
// objects are, the entry plain data, and writable for a write, and a
// larger one looked for in the megamorphic cache (scan); at any other site
// it fails, as V8's map checks deoptimize, the interpreter making the
// operation and its cache learning the shape. It uses scratchB and
// scratchC.
func (c *compiler) property(v *ssa.Value, guard func(amd64.Cond)) {
	if v.Holders() != nil || v.Cases() != nil {
		c.holder(v, guard)
		return
	}
	found := c.a.NewLabel()
	if v.Const.Bits != 0 {
		// The shape the site knows, inline; any other, out of line.
		scan := c.a.NewLabel()
		p := c.gpr(v.Args[0], scratchA)
		c.a.Load(scratchB, p, c.enc.ObjectShape)
		c.movImm(scratchA, v.Const.Bits)
		c.a.Op(amd64.Cmp, scratchB, scratchA, true)
		if !v.Megamorphic() {
			guard(amd64.CondNE)
		} else {
			c.a.Jcc(amd64.CondNE, scan)
			c.cold = append(c.cold, func() {
				c.a.Bind(scan)
				c.scan(v, guard, found, 0)
			})
		}
		p = c.gpr(v.Args[0], scratchA)
		c.a.Load(scratchA, p, c.enc.ObjectProps)
		c.a.OpImm(amd64.Add, scratchA, int32(v.Index)*c.enc.ValueSize, true)
		c.a.Bind(found)
		return
	}
	if !v.Megamorphic() && !v.Scans() {
		// No shape the site knows, nor a megamorphic cache's: every
		// receiver fails -- a write's past the additions it tried.
		c.a.Op(amd64.Cmp, scratchB, scratchB, true)
		guard(amd64.CondE)
		c.a.Bind(found)
		return
	}
	c.scan(v, guard, found, 0)
	c.a.Bind(found)
}

// scan searches an object's small table for the key a property operation
// names, as the VM's own small objects are searched, unrolled, and goes to
// found with the address of its value in scratchA; the entry must be plain
// data, and writable for a write. A read searches a function's too, whose
// prototype the VM's caches never learn (synthesized): RayTrace calls
// methods as Flog.RayTracer.Vector.prototype.subtract(...), and the read
// left native code at every call. A property the VM makes on demand, which
// the table may not hold yet, is not found, and Go reads it. Anything else
// fails.
//
// Too large to search, the megamorphic cache's (mega) -- at mega, if not
// 0, the site's probe -- and, for a read that has met more than its cases
// hold (ssa.Value's Megamorphic), not its own either: one that may yet
// learn a case leaves, to learn it.
func (c *compiler) scan(v *ssa.Value, guard func(amd64.Cond), found, mega amd64.Label) {
	p := c.gpr(v.Args[0], scratchA)
	c.a.LoadU8(scratchB, p, c.enc.ObjectClass)
	c.a.OpImm(amd64.Cmp, scratchB, int32(c.enc.ClassObject), false)
	if v.Op == ssa.OpPropWrite {
		guard(amd64.CondNE)
	} else {
		object := c.a.NewLabel()
		c.a.Jcc(amd64.CondE, object)
		c.a.OpImm(amd64.Cmp, scratchB, int32(c.enc.ClassFunction), false)
		guard(amd64.CondNE)
		c.a.Bind(object)
	}
	c.a.Load(scratchB, p, c.enc.ObjectProps+8)
	c.a.OpImm(amd64.Cmp, scratchB, abi.MaxScan, true)
	if c.enc.Mega && mega == 0 && v.Megamorphic() {
		mega = c.a.NewLabel()
		c.cold = append(c.cold, func() {
			c.a.Bind(mega)
			c.mega(v, guard, found)
		})
	}
	if mega != 0 {
		// Too large to search: the megamorphic cache's (mega).
		c.a.Jcc(amd64.CondA, mega)
	} else {
		guard(amd64.CondA)
	}
	// No properties, no shape -- but for a read the megamorphic cache's,
	// which names a property its prototypes have or none has: a leaf's
	// child, read of an empty object.
	c.a.Op(amd64.Test, scratchB, scratchB, true)
	if mega != 0 && v.Op != ssa.OpPropWrite && v.Megamorphic() {
		c.a.Jcc(amd64.CondE, mega)
	} else {
		guard(amd64.CondE)
	}
	c.a.Load(scratchA, p, c.enc.ObjectShape)
	c.a.Load(scratchA, scratchA, c.enc.ShapeKeys)
	mask, want := int32(c.enc.PropNotData), int32(0)
	if v.Op == ssa.OpPropWrite {
		mask, want = int32(c.enc.PropNotWritable), int32(c.enc.PropWritable)
	}
	// A loop over the keys, those left in scratchB: the slow path's few
	// dozen bytes, where it had been unrolled into hundreds, at every site
	// -- a third of all the code made.
	loop, hit := c.a.NewLabel(), c.a.NewLabel()
	c.a.Bind(loop)
	c.a.Op(amd64.Test, scratchB, scratchB, true)
	if mega != 0 && v.Op != ssa.OpPropWrite && v.Megamorphic() {
		// Not its own: a prototype's, which the megamorphic cache may
		// have, as V8's megamorphic loads look further rather than
		// deoptimize. Box2D's contacts, of seven classes, each read
		// IsSensor of their base class's prototype, and the read left
		// native code for the shapes past its cases.
		c.a.Jcc(amd64.CondE, mega)
	} else {
		guard(amd64.CondE)
	}
	c.a.LoadU32(scratchC, scratchA, c.enc.PropertyKey)
	c.a.OpImm(amd64.Cmp, scratchC, int32(v.Key), false)
	c.a.Jcc(amd64.CondE, hit)
	c.a.OpImm(amd64.Add, scratchA, c.enc.KeySize, true)
	c.a.OpImm(amd64.Sub, scratchB, 1, true)
	c.a.Jmp(loop)
	c.a.Bind(hit)
	c.a.LoadU8(scratchC, scratchA, c.enc.PropertyFlags)
	c.a.OpImm(amd64.And, scratchC, mask, false)
	c.a.OpImm(amd64.Cmp, scratchC, want, false)
	guard(amd64.CondNE)
	// The value at the key's position: the table's length less the keys
	// that were left, the one found among them.
	p = c.gpr(v.Args[0], scratchC)
	c.a.Load(scratchC, p, c.enc.ObjectProps+8)
	c.a.Op(amd64.Sub, scratchC, scratchB, true)
	c.a.ShiftImm(amd64.Shl, scratchC, 4, true)
	p = c.gpr(v.Args[0], scratchA)
	c.a.Load(scratchA, p, c.enc.ObjectProps)
	c.a.Op(amd64.Add, scratchA, scratchC, true)
	c.a.Jmp(found)
}

// mega looks the property v names up in the megamorphic cache
// (abi.MegaCache) for the receiver's shape, by the probe the code's sites
// share (megaProbe), and goes to found with the address of its value in
// scratchA, or fails: Go fills the entry at the exit. A probe of its own
// at every site, unrolled, had been a few hundred bytes of each read's.
func (c *compiler) mega(v *ssa.Value, guard func(amd64.Cond), found amd64.Label) {
	write := b2i(v.Op == ssa.OpPropWrite)
	if !c.probeUsed[write] {
		c.probe[write], c.probeUsed[write] = c.a.NewLabel(), true
		c.cold = append(c.cold, func() {
			c.a.Bind(c.probe[write])
			c.megaProbe(write == 1)
		})
	}
	back := c.a.NewLabel()
	c.a.StoreImm(regCtx, abi.OffProbeKey, int32(v.Key))
	c.a.StoreImm(regCtx, abi.OffProbeHash, int32(abi.MegaKeyHash(uint32(v.Key))))
	c.a.LeaLabel(scratchC, back)
	c.a.Store(regCtx, abi.OffProbeBack, scratchC)
	if p := c.gpr(v.Args[0], scratchA); p != scratchA {
		c.a.MovRR(scratchA, p)
	}
	c.a.Jmp(c.probe[write])
	c.a.Bind(back)
	c.a.Op(amd64.Test, scratchA, scratchA, true)
	guard(amd64.CondE)
	c.a.Jmp(found)
}

// megaProbe is the probe of the megamorphic cache a function's sites share
// (mega), for a read or a write: the receiver in scratchA, the key and its
// hash's part in the context (abi.Context's ProbeKey, ProbeHash), it goes
// back (ProbeBack) with the address of the value in scratchA -- the
// receiver's own data property, plain, and writable for a write; or, for a
// read, a prototype's, along the entry's chain, each prototype the one it
// names with its shape, or none of them, the value undefined
// (abi.MegaAbsent) -- or 0. It uses every scratch register, xScratch2, and
// for a read xScratch1.
func (c *compiler) megaProbe(write bool) {
	fail := c.a.NewLabel()
	c.a.MovQToX(xScratch2, scratchA)
	c.a.Load(scratchB, scratchA, c.enc.ObjectShape)
	// abi.MegaSet's hash.
	c.a.MovRR(scratchC, scratchB)
	c.a.ShiftImm(amd64.Shr, scratchC, 4, true)
	c.a.MovRR(scratchA, scratchB)
	c.a.ShiftImm(amd64.Shr, scratchA, 15, true)
	c.a.Op(amd64.Xor, scratchC, scratchA, true)
	c.a.MovRR(scratchA, scratchB)
	c.a.ShiftImm(amd64.Shr, scratchA, 26, true)
	c.a.Op(amd64.Xor, scratchC, scratchA, true)
	c.a.LoadU32(scratchA, regCtx, abi.OffProbeHash)
	c.a.Op(amd64.Xor, scratchC, scratchA, false)
	c.a.OpImm(amd64.And, scratchC, abi.MegaSets-1, false)
	c.a.ShiftImm(amd64.Shl, scratchC, abi.MegaSetShift, true)
	c.a.Load(scratchA, regCtx, abi.OffMega)
	c.a.Op(amd64.Add, scratchA, scratchC, true)
	// The set's ways, in turn.
	hit := c.a.NewLabel()
	for w := int32(0); w < abi.MegaWays; w++ {
		next := c.a.NewLabel()
		c.a.Load(scratchC, scratchA, w*abi.MegaSize+abi.OffMegaShape)
		c.a.Op(amd64.Cmp, scratchB, scratchC, true)
		c.a.Jcc(amd64.CondNE, next)
		c.a.LoadU32(scratchC, scratchA, w*abi.MegaSize+abi.OffMegaKey)
		c.a.CmpMem(scratchC, regCtx, abi.OffProbeKey, false)
		c.a.Jcc(amd64.CondNE, next)
		c.a.OpImm(amd64.Add, scratchA, w*abi.MegaSize, true)
		c.a.Jmp(hit)
		c.a.Bind(next)
	}
	c.a.Jmp(fail)
	c.a.Bind(hit)
	c.a.LoadU32(scratchC, scratchA, abi.OffMegaIndex)
	if write {
		c.a.Op(amd64.Test, scratchC, scratchC, false)
		c.a.Jcc(amd64.CondNS, fail)
	} else {
		// A prototype's: the chain's objects, each of its shape, from the
		// receiver's prototype on, the last the holder; or, absent, all of
		// them, the last's prototype none.
		own, loop, holder, absent, end := c.a.NewLabel(), c.a.NewLabel(), c.a.NewLabel(), c.a.NewLabel(), c.a.NewLabel()
		c.a.MovQToX(xScratch1, scratchC)
		c.a.Load(scratchA, scratchA, abi.OffMegaChain)
		c.a.Op(amd64.Test, scratchA, scratchA, true)
		c.a.Jcc(amd64.CondE, own)
		c.a.MovQFromX(scratchB, xScratch2)
		c.a.Load(scratchB, scratchB, c.enc.ObjectProto)
		c.a.OpImm(amd64.Cmp, scratchC, abi.MegaAbsent, false)
		c.a.Jcc(amd64.CondAE, absent)
		c.a.Bind(loop)
		c.a.CmpMem(scratchB, scratchA, abi.OffLinkObject, true)
		c.a.Jcc(amd64.CondNE, fail)
		c.a.Load(scratchC, scratchB, c.enc.ObjectShape)
		c.a.CmpMem(scratchC, scratchA, abi.OffLinkShape, true)
		c.a.Jcc(amd64.CondNE, fail)
		c.a.Load(scratchC, scratchA, abi.MegaLinkSize+abi.OffLinkObject)
		c.a.Op(amd64.Test, scratchC, scratchC, true)
		c.a.Jcc(amd64.CondE, holder)
		c.a.OpImm(amd64.Add, scratchA, abi.MegaLinkSize, true)
		c.a.Load(scratchB, scratchB, c.enc.ObjectProto)
		c.a.Jmp(loop)
		c.a.Bind(holder)
		c.a.MovQFromX(scratchC, xScratch1)
		c.a.ShiftImm(amd64.Shl, scratchC, 4, true)
		c.a.Load(scratchA, scratchB, c.enc.ObjectProps)
		c.a.Op(amd64.Add, scratchA, scratchC, true)
		c.a.JmpMem(regCtx, abi.OffProbeBack)
		c.a.Bind(absent)
		c.a.Load(scratchC, scratchA, abi.OffLinkObject)
		c.a.Op(amd64.Test, scratchC, scratchC, true)
		c.a.Jcc(amd64.CondE, end)
		c.a.Op(amd64.Cmp, scratchB, scratchC, true)
		c.a.Jcc(amd64.CondNE, fail)
		c.a.Load(scratchC, scratchB, c.enc.ObjectShape)
		c.a.CmpMem(scratchC, scratchA, abi.OffLinkShape, true)
		c.a.Jcc(amd64.CondNE, fail)
		c.a.Load(scratchB, scratchB, c.enc.ObjectProto)
		c.a.OpImm(amd64.Add, scratchA, abi.MegaLinkSize, true)
		c.a.Jmp(absent)
		c.a.Bind(end)
		c.a.Op(amd64.Test, scratchB, scratchB, true)
		c.a.Jcc(amd64.CondNE, fail)
		c.movImm(scratchA, c.enc.UndefinedCell)
		c.a.JmpMem(regCtx, abi.OffProbeBack)
		c.a.Bind(own)
		c.a.MovQFromX(scratchC, xScratch1)
	}
	c.a.OpImm(amd64.And, scratchC, abi.MegaWritable-1, false)
	c.a.ShiftImm(amd64.Shl, scratchC, 4, true)
	c.a.MovQFromX(scratchA, xScratch2)
	c.a.Load(scratchA, scratchA, c.enc.ObjectProps)
	c.a.Op(amd64.Add, scratchA, scratchC, true)
	c.a.JmpMem(regCtx, abi.OffProbeBack)
	c.a.Bind(fail)
	c.movImm(scratchA, 0)
	c.a.JmpMem(regCtx, abi.OffProbeBack)
}

// keyAt checks that the object in scratchA has a property at v's index
// (ssa.Value's Index), of v's key, leaving its shape's keys in scratchB for
// the attributes there. It uses scratchC.
func (c *compiler) keyAt(v *ssa.Value, guard func(amd64.Cond)) {
	c.a.Load(scratchB, scratchA, c.enc.ObjectProps+8)
	c.a.OpImm(amd64.Cmp, scratchB, int32(v.Index), true)
	guard(amd64.CondBE)
	c.a.Load(scratchB, scratchA, c.enc.ObjectShape)
	c.a.Load(scratchB, scratchB, c.enc.ShapeKeys)
	c.a.LoadU32(scratchC, scratchB, int32(v.Index)*c.enc.KeySize+c.enc.PropertyKey)
	c.a.OpImm(amd64.Cmp, scratchC, int32(v.Key), false)
	guard(amd64.CondNE)
}

// stringBytes compares two strings of one length, in code units, by their
// bytes, as the VM does (String.Equals): their pointers are in xScratch1
// and scratchC, the length in scratchA. A rope, which Go flattens, and a
// string longer than abi.MaxEqualUnits exit; strings of different byte
// lengths differ. The loop borrows regLocals and regStack, as remainder
// does, and reloads them from the context before it leaves.
func (c *compiler) stringBytes(exit, yes, no amd64.Label) {
	c.a.OpImm(amd64.Cmp, scratchA, abi.MaxEqualUnits, true)
	c.a.Jcc(amd64.CondA, exit)
	c.a.MovQFromX(scratchA, xScratch1)
	for _, s := range []amd64.Reg{scratchA, scratchC} {
		c.a.Load(scratchB, s, c.enc.StringLeft)
		c.a.Op(amd64.Test, scratchB, scratchB, true)
		c.a.Jcc(amd64.CondNE, exit)
	}
	c.a.Load(scratchB, scratchC, c.enc.StringData+8)
	c.a.Load(scratchA, scratchA, c.enc.StringData+8)
	c.a.Op(amd64.Cmp, scratchA, scratchB, true)
	c.a.Jcc(amd64.CondNE, no)
	c.a.MovQFromX(scratchA, xScratch1)
	c.a.Load(scratchA, scratchA, c.enc.StringData)
	c.a.Load(scratchC, scratchC, c.enc.StringData)
	words, bytes, same, differ := c.a.NewLabel(), c.a.NewLabel(), c.a.NewLabel(), c.a.NewLabel()
	c.a.Bind(words)
	c.a.OpImm(amd64.Cmp, scratchB, 8, true)
	c.a.Jcc(amd64.CondB, bytes)
	c.a.Load(regLocals, scratchA, 0)
	c.a.Load(regStack, scratchC, 0)
	c.a.Op(amd64.Cmp, regLocals, regStack, true)
	c.a.Jcc(amd64.CondNE, differ)
	c.a.OpImm(amd64.Add, scratchA, 8, true)
	c.a.OpImm(amd64.Add, scratchC, 8, true)
	c.a.OpImm(amd64.Sub, scratchB, 8, true)
	c.a.Jmp(words)
	c.a.Bind(bytes)
	c.a.Op(amd64.Test, scratchB, scratchB, true)
	c.a.Jcc(amd64.CondE, same)
	c.a.LoadU8(regLocals, scratchA, 0)
	c.a.LoadU8(regStack, scratchC, 0)
	c.a.Op(amd64.Cmp, regLocals, regStack, false)
	c.a.Jcc(amd64.CondNE, differ)
	c.a.OpImm(amd64.Add, scratchA, 1, true)
	c.a.OpImm(amd64.Add, scratchC, 1, true)
	c.a.OpImm(amd64.Sub, scratchB, 1, true)
	c.a.Jmp(bytes)
	for _, l := range []struct{ at, to amd64.Label }{{same, yes}, {differ, no}} {
		c.a.Bind(l.at)
		c.a.Load(regLocals, regCtx, abi.OffLocals)
		c.a.Load(regStack, regCtx, abi.OffStack)
		c.a.Jmp(l.to)
	}
}

// keepSource leaves in scratchC where a tagged value came from, as an
// exit's record has it: its shadow; the source of the slot its origin is,
// if the value is that slot's reference at entry; or 0 for a primitive. It
// uses scratchA.
func (c *compiler) keepSource(x *ssa.Value) {
	switch o, static := c.origin.Of(x); {
	case x.Shadow != nil:
		if r := c.gpr(x.Shadow, scratchC); r != scratchC {
			c.a.MovRR(scratchC, r)
		}
	case static && o >= 0:
		scalar, done := c.a.NewLabel(), c.a.NewLabel()
		c.isReference(x, c.gpr(x, scratchA), o, scalar)
		c.slotSource(o, scratchC)
		c.a.Jmp(done)
		c.a.Bind(scalar)
		c.movImm(scratchC, 0)
		c.a.Bind(done)
	default:
		c.movImm(scratchC, 0)
	}
}

// keepRef is a tagged value's pointer word, read where it came from
// (keepSource), or 0 for a primitive's (ssa's OpKeepRef); read while the
// collector marks too, which only writes would need a barrier for.
func (c *compiler) keepRef(v *ssa.Value) {
	x := v.Args[0]
	scalar := c.a.NewLabel()
	c.keepSource(x)
	c.movImm(scratchA, 0)
	c.a.Op(amd64.Test, scratchC, scratchC, true)
	c.a.Jcc(amd64.CondE, scalar)
	c.a.Load(scratchA, scratchC, c.enc.RefOffset)
	c.a.Bind(scalar)
	c.setG(v, scratchA)
}

// keep copies a value, its pointer word read before (keepRef), into the
// context's keep cell v.Index, and leaves the cell's address as v (ssa's
// OpKeep); while the collector marks, with Go's write barrier
// (valueStore).
func (c *compiler) keep(v *ssa.Value) {
	x := v.Args[0]
	viaGo, done := c.a.NewLabel(), c.a.NewLabel()
	at := abi.OffKeep + int32(v.Index)*int32(c.enc.ValueSize)
	c.a.MovQToX(xScratch0, c.gpr(x, scratchB))
	if r := c.gpr(v.Args[1], scratchC); r != scratchC {
		c.a.MovRR(scratchC, r)
	}
	c.movImm(scratchB, c.enc.WriteBarrier)
	c.a.LoadU8(scratchB, scratchB, 0)
	c.a.Op(amd64.Test, scratchB, scratchB, false)
	c.a.Jcc(amd64.CondNE, viaGo)
	c.a.Store(regCtx, at+c.enc.RefOffset, scratchC)
	c.a.MovQFromX(scratchB, xScratch0)
	c.a.Store(regCtx, at+c.enc.NumOffset, scratchB)
	c.a.Bind(done)
	c.a.MovRR(scratchA, regCtx)
	c.a.OpImm(amd64.Add, scratchA, at, true)
	c.setG(v, scratchA)
	c.cold = append(c.cold, func() {
		c.a.Bind(viaGo)
		c.a.MovRR(scratchA, regCtx)
		c.a.OpImm(amd64.Add, scratchA, at, true)
		c.valueStore()
		c.a.Jmp(done)
	})
}

// bigUnbox is the BigInt a tagged value is (ssa's OpBigUnbox), one held in
// the value, which it guards: its number word's high bits the tag, and a
// source its pointer word is read from (keepSource), whose distance from
// ShortBigHigh is the BigInt's high bits.
func (c *compiler) bigUnbox(v *ssa.Value, guard func(amd64.Cond)) {
	x := v.Args[0]
	low := 64 - c.enc.ShortBigLowBits
	r := c.gpr(x, scratchB)
	c.a.MovRR(scratchA, r)
	c.a.ShiftImm(amd64.Shr, scratchA, c.enc.ShortBigLowBits, true)
	c.a.OpImm(amd64.Cmp, scratchA, int32(c.enc.ShortBigTag>>c.enc.ShortBigLowBits), true)
	guard(amd64.CondNE)
	c.keepSource(x)
	c.a.Op(amd64.Test, scratchC, scratchC, true)
	guard(amd64.CondE)
	c.a.Load(scratchA, scratchC, c.enc.RefOffset)
	c.movImm(scratchB, c.enc.ShortBigHigh)
	c.a.Op(amd64.Sub, scratchA, scratchB, true)
	c.a.ShiftImm(amd64.Shl, scratchA, c.enc.ShortBigLowBits, true)
	if r = c.gpr(x, scratchC); r != scratchC {
		c.a.MovRR(scratchC, r)
	}
	c.a.ShiftImm(amd64.Shl, scratchC, low, true)
	c.a.ShiftImm(amd64.Shr, scratchC, low, true)
	c.a.Op(amd64.Or, scratchA, scratchC, true)
	c.setG(v, scratchA)
}

// bigBox is an unboxed BigInt as a value (ssa's OpBigBox): its low bits and
// the tag its number word, which is v, and ShortBigHigh past its high bits
// its pointer word -- a Uint64's past the int64 range in the wide encoding
// (bigBoxWords) -- both written to the context's keep cell
// v.Index; while the collector marks, with Go's write barrier
// (valueStore), as keep writes one.
func (c *compiler) bigBox(v *ssa.Value) {
	viaGo, done := c.a.NewLabel(), c.a.NewLabel()
	at := abi.OffKeep + int32(v.Index)*int32(c.enc.ValueSize)
	c.bigBoxWords(v)
	c.a.MovQToX(xScratch0, scratchA)
	c.movImm(scratchB, c.enc.WriteBarrier)
	c.a.LoadU8(scratchB, scratchB, 0)
	c.a.Op(amd64.Test, scratchB, scratchB, false)
	c.a.Jcc(amd64.CondNE, viaGo)
	c.a.Store(regCtx, at+c.enc.RefOffset, scratchC)
	c.a.Store(regCtx, at+c.enc.NumOffset, scratchA)
	c.a.Bind(done)
	c.setG(v, scratchA)
	c.cold = append(c.cold, func() {
		c.a.Bind(viaGo)
		c.a.MovRR(scratchA, regCtx)
		c.a.OpImm(amd64.Add, scratchA, at, true)
		c.valueStore()
		// The scratch registers are Go's across its write barrier.
		c.a.Load(scratchA, regCtx, at+c.enc.NumOffset)
		c.a.Jmp(done)
	})
}

// holder finds the property a read whose receiver's shape it knows names
// -- on a prototype, which it checks, or the receiver's own -- into
// scratchA, the address of its value; and, for a read that met objects of
// several shapes, as V8's polymorphic inline caches do, for whichever of
// them the receiver's is (ssa.PropertyCase). Any other shape fails.
func (c *compiler) holder(v *ssa.Value, guard func(amd64.Cond)) {
	var first ssa.HolderChain
	if v.Holders() != nil {
		first = *v.Holders()
	}
	cases := append([]ssa.PropertyCase{{Shape: uintptr(v.Const.Bits), Index: int32(v.Index), Holders: first.Holders, More: first.More}}, v.Cases()...)
	// At a megamorphic site (ssa.Value's Megamorphic), a receiver of a
	// shape not among the cases has its own table searched (scan), as V8's
	// megamorphic loads look further rather than deoptimize, and leaves
	// only if the property is not its own: RayTrace's IntersectionInfo,
	// given its properties in more orders than the cases hold, some on its
	// prototype, left its readers at every one. A receiver whose
	// prototypes are no longer the case's -- a method added to one, or
	// taken off -- is searched so too, rather than leave: the megamorphic
	// cache has the chain as it is now (mega). At any other site either
	// fails, as V8's map checks deoptimize: the interpreter makes the
	// operation, and its cache learns the shape.
	wide := v.Megamorphic()
	done, scan, mega := c.a.NewLabel(), c.a.NewLabel(), amd64.Label(0)
	stale := scan
	if wide && c.enc.Mega && v.Op != ssa.OpPropWrite {
		// Prototypes no longer the case's, the megamorphic cache's chain;
		// the receiver's own table, the case's shape says, has no key.
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
	miss := func(to amd64.Label) {
		if wide {
			c.a.Jcc(amd64.CondNE, to)
		} else {
			guard(amd64.CondNE)
		}
	}
	for i, k := range cases {
		next := c.a.NewLabel()
		p := c.gpr(v.Args[0], scratchA)
		c.a.Load(scratchB, p, c.enc.ObjectShape)
		c.cmpWord(scratchB, uint64(k.Shape))
		if i == len(cases)-1 {
			miss(scan)
		} else {
			c.a.Jcc(amd64.CondNE, next)
		}
		if p != scratchA {
			c.a.MovRR(scratchA, p)
		}
		c.a.Load(scratchB, scratchA, c.enc.ObjectProto)
		// The holders, the prototypes from the receiver's on: those a cache
		// holds, then those past them (More).
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
			c.movImm(scratchA, uint64(h.Object))
			c.a.Op(amd64.Cmp, scratchB, scratchA, true)
			miss(stale)
			c.a.Load(scratchB, scratchA, c.enc.ObjectShape)
			c.cmpWord(scratchB, uint64(h.Shape))
			miss(stale)
			c.a.Load(scratchB, scratchA, c.enc.ObjectProto)
		}
		c.a.Load(scratchA, scratchA, c.enc.ObjectProps)
		c.a.OpImm(amd64.Add, scratchA, k.Index*c.enc.ValueSize, true)
		if i < len(cases)-1 {
			c.a.Jmp(done)
			c.a.Bind(next)
		}
	}
	c.a.Bind(done)
}

// remainder is JavaScript's % of two integers, by integer division: both
// exact integers below 2**63 in magnitude, the divisor not zero. A zero
// remainder takes the dividend's sign, -0 included, as fmod's does; a
// divisor of -1 leaves exactly that without dividing, which could fault.
// Anything else Go computes as math.Mod does -- SSE has no remainder, and
// Go computes one in software -- called from here (ssa.GoMod), as V8's
// code calls its fmod, or, from code that cannot call Go, after an exit.
func (c *compiler) remainder(v *ssa.Value, guard func(amd64.Cond)) {
	if x := c.xmm(v.Args[0], xScratch0); x != xScratch0 {
		c.a.SSEOp(amd64.MovAPD, xScratch0, x)
	}
	if y := c.xmm(v.Args[1], xScratch1); y != xScratch1 {
		c.a.SSEOp(amd64.MovAPD, xScratch1, y)
	}
	zero, done := c.a.NewLabel(), c.a.NewLabel()
	if c.enc.CallGo != 0 {
		// The operands' bits in the context's GoArgs, from the scratch
		// registers the guards below leave as they are.
		size := int32(unsafe.Sizeof(abi.GoArg{}))
		slow, stub := c.a.NewLabel(), c.stubLabel(v.State, exitKind(v.Aux))
		guard = func(cond amd64.Cond) { c.a.Jcc(cond, slow) }
		c.cold = append(c.cold, func() {
			c.a.Bind(slow)
			c.a.MovQFromX(scratchA, xScratch0)
			c.a.Store(regCtx, abi.OffGoArgs+c.enc.NumOffset, scratchA)
			c.a.MovQFromX(scratchA, xScratch1)
			c.a.Store(regCtx, abi.OffGoArgs+size+c.enc.NumOffset, scratchA)
			c.goSaving(uint64(ssa.GoMod), &stub)
			c.a.LoadSD(xScratch0, regCtx, abi.OffGoArgs+c.enc.NumOffset)
			c.a.Jmp(done)
		})
	}
	for _, p := range []struct {
		r amd64.Reg
		x amd64.XReg
	}{{scratchA, xScratch0}, {scratchC, xScratch1}} {
		c.a.Cvttsd2si(p.r, p.x)
		c.a.Cvtsi2sd(xScratch2, p.r, true)
		c.a.SSEOp(amd64.UcomiSD, xScratch2, p.x)
		guard(amd64.CondNE)
		guard(amd64.CondP)
	}
	c.a.Op(amd64.Test, scratchC, scratchC, true)
	guard(amd64.CondE)
	c.a.OpImm(amd64.Cmp, scratchC, -1, true)
	c.a.Jcc(amd64.CondE, zero)
	// IDIV takes RDX, the operands' base, which is reloaded after it.
	c.a.Cqo()
	c.a.Idiv(scratchC)
	c.a.MovRR(scratchB, regStack)
	c.a.Load(regStack, regCtx, abi.OffStack)
	c.a.Op(amd64.Test, scratchB, scratchB, true)
	c.a.Jcc(amd64.CondE, zero)
	c.a.Cvtsi2sd(xScratch0, scratchB, true)
	c.a.Jmp(done)
	c.a.Bind(zero)
	c.a.MovQFromX(scratchA, xScratch0)
	c.movImm(scratchB, 1<<63)
	c.a.Op(amd64.And, scratchA, scratchB, true)
	c.a.MovQToX(xScratch0, scratchA)
	c.a.Bind(done)
	c.setX(v, xScratch0)
}

// power is Math.pow(x, y) where its answer is exact (ssa.ExactPow): x*x
// for y 2; an integer x to an integer power from 0 to 64 by repeated
// squaring, its result at most 2**53. Anything else Go computes, called
// from here, or, from code that cannot call Go, fails the guard: Go makes
// the call.
//
// extremum is Math.max(x, y) or Math.min(x, y) (ssa.OpMaxF64,
// ssa.OpMinF64): NaN if either is -- the sum -- the larger or the smaller,
// and of equal operands, which are zeros of either sign or the same
// number, their bits and'ed for max (+0 if either is), or'ed for min.
func (c *compiler) extremum(v *ssa.Value) {
	if x := c.xmm(v.Args[0], xScratch0); x != xScratch0 {
		c.a.SSEOp(amd64.MovAPD, xScratch0, x)
	}
	y := c.xmm(v.Args[1], xScratch1)
	nan, differ, done := c.a.NewLabel(), c.a.NewLabel(), c.a.NewLabel()
	c.a.SSEOp(amd64.UcomiSD, xScratch0, y)
	c.a.Jcc(amd64.CondP, nan)
	c.a.Jcc(amd64.CondNE, differ)
	if v.Op == ssa.OpMaxF64 {
		c.a.SSEOp(amd64.AndPD, xScratch0, y)
	} else {
		c.a.SSEOp(amd64.OrPD, xScratch0, y)
	}
	c.a.Jmp(done)
	c.a.Bind(nan)
	c.a.SSEOp(amd64.AddSD, xScratch0, y)
	c.a.Jmp(done)
	c.a.Bind(differ)
	// x above y (CF and ZF clear) keeps x for max; below (CF), for min.
	if v.Op == ssa.OpMaxF64 {
		c.a.Jcc(amd64.CondA, done)
	} else {
		c.a.Jcc(amd64.CondB, done)
	}
	c.a.SSEOp(amd64.MovAPD, xScratch0, y)
	c.a.Bind(done)
	c.setX(v, xScratch0)
}

// round is Math.round(x) (ssa.OpRoundF64), as ssa.Round has it: the
// ceiling, less one if the ceiling less a half is above x -- computed
// again otherwise, which keeps its -0.
func (c *compiler) round(v *ssa.Value) {
	if !c.enc.Round {
		panic("rounding without SSE4.1")
	}
	if x := c.xmm(v.Args[0], xScratch1); x != xScratch1 {
		c.a.SSEOp(amd64.MovAPD, xScratch1, x)
	}
	down, done := c.a.NewLabel(), c.a.NewLabel()
	c.a.RoundSD(xScratch0, xScratch1, amd64.RoundCeil)
	c.movImm(scratchA, math.Float64bits(0.5))
	c.a.MovQToX(xScratch2, scratchA)
	c.a.SSEOp(amd64.SubSD, xScratch0, xScratch2)
	c.a.SSEOp(amd64.UcomiSD, xScratch0, xScratch1)
	c.a.Jcc(amd64.CondA, down)
	c.a.RoundSD(xScratch0, xScratch1, amd64.RoundCeil)
	c.a.Jmp(done)
	c.a.Bind(down)
	c.a.SSEOp(amd64.SubSD, xScratch0, xScratch2)
	c.a.Bind(done)
	c.setX(v, xScratch0)
}

// sign is Math.sign(x) (ssa.OpSignF64): 1 above zero, -1 below, x itself
// for a zero or NaN.
func (c *compiler) sign(v *ssa.Value) {
	if x := c.xmm(v.Args[0], xScratch0); x != xScratch0 {
		c.a.SSEOp(amd64.MovAPD, xScratch0, x)
	}
	above, done := c.a.NewLabel(), c.a.NewLabel()
	c.a.SSEOp(amd64.XorPD, xScratch2, xScratch2)
	c.a.SSEOp(amd64.UcomiSD, xScratch0, xScratch2)
	c.a.Jcc(amd64.CondP, done)
	c.a.Jcc(amd64.CondE, done)
	c.a.Jcc(amd64.CondA, above)
	c.movImm(scratchA, math.Float64bits(-1))
	c.a.MovQToX(xScratch0, scratchA)
	c.a.Jmp(done)
	c.a.Bind(above)
	c.movImm(scratchA, math.Float64bits(1))
	c.a.MovQToX(xScratch0, scratchA)
	c.a.Bind(done)
	c.setX(v, xScratch0)
}

// random is Math.random() (ssa.OpRandomF64), as V8's code takes it: the
// count of the runtime's cache, at Const, less one, and the number there.
// An empty cache has Go fill it (ssa.GoRandom), out of line, its next
// number Go's result.
func (c *compiler) random(v *ssa.Value) {
	fill, done := c.a.NewLabel(), c.a.NewLabel()
	c.movImm(scratchC, v.Const.Bits)
	c.a.Load(scratchA, scratchC, c.enc.RandomCount)
	c.a.Op(amd64.Test, scratchA, scratchA, true)
	c.a.Jcc(amd64.CondE, fill)
	c.a.OpImm(amd64.Sub, scratchA, 1, true)
	c.a.Store(scratchC, c.enc.RandomCount, scratchA)
	c.a.ShiftImm(amd64.Shl, scratchA, 3, true)
	c.a.Op(amd64.Add, scratchA, scratchC, true)
	c.a.LoadSD(xScratch0, scratchA, c.enc.RandomNumbers)
	c.a.Bind(done)
	c.setX(v, xScratch0)
	c.cold = append(c.cold, func() {
		c.a.Bind(fill)
		c.goSaving(uint64(ssa.GoRandom), nil)
		c.a.LoadSD(xScratch0, regCtx, abi.OffGoArgs+c.enc.NumOffset)
		c.a.Jmp(done)
	})
}

// mathCall has Go compute a Math function of numbers (ssa.OpMathCall), as
// V8's code calls its ieee754 functions: the operands' bits in the
// context's GoArgs and, for ssa.GoMath, which function in the second's
// number word; the result in the first's. Go refuses none.
func (c *compiler) mathCall(v *ssa.Value) {
	size := int32(unsafe.Sizeof(abi.GoArg{}))
	for i, x := range v.Args {
		c.a.MovQFromX(scratchA, c.xmm(x, xScratch0))
		c.a.Store(regCtx, abi.OffGoArgs+int32(i)*size+c.enc.NumOffset, scratchA)
	}
	if ssa.GoOp(v.Index) == ssa.GoMath {
		c.storeImm(regCtx, abi.OffGoArgs+size+c.enc.NumOffset, uint64(v.Aux), scratchA)
	}
	c.goSaving(uint64(v.Index), nil)
	c.a.LoadSD(xScratch0, regCtx, abi.OffGoArgs+c.enc.NumOffset)
	c.setX(v, xScratch0)
}

// power is Math.pow(x, y) where its answer is exact (ssa.ExactPow): x*x
// for y 2; an integer x to an integer power from 0 to 64 by repeated
// squaring, its result at most 2**53. Anything else Go computes, called
// from here (ssa.GoPow), or, from code that cannot call Go, fails the
// guard: Go makes the call.
func (c *compiler) power(v *ssa.Value, guard func(amd64.Cond)) {
	if x := c.xmm(v.Args[0], xScratch0); x != xScratch0 {
		c.a.SSEOp(amd64.MovAPD, xScratch0, x)
	}
	if y := c.xmm(v.Args[1], xScratch1); y != xScratch1 {
		c.a.SSEOp(amd64.MovAPD, xScratch1, y)
	}
	general, loop, skip, last, done := c.a.NewLabel(), c.a.NewLabel(), c.a.NewLabel(), c.a.NewLabel(), c.a.NewLabel()
	c.movImm(scratchA, math.Float64bits(2))
	c.a.MovQToX(xScratch2, scratchA)
	c.a.SSEOp(amd64.UcomiSD, xScratch1, xScratch2)
	c.a.Jcc(amd64.CondP, general)
	c.a.Jcc(amd64.CondNE, general)
	c.a.SSEOp(amd64.MulSD, xScratch0, xScratch0)
	c.a.Jmp(done)
	c.a.Bind(general)
	if c.enc.CallGo != 0 {
		// What is not exact Go computes (ssa.GoPow), as V8's code calls
		// its ieee754 pow: the operands' bits in the context's GoArgs.
		size := int32(unsafe.Sizeof(abi.GoArg{}))
		c.a.MovQFromX(scratchA, xScratch0)
		c.a.Store(regCtx, abi.OffGoArgs+c.enc.NumOffset, scratchA)
		c.a.MovQFromX(scratchA, xScratch1)
		c.a.Store(regCtx, abi.OffGoArgs+size+c.enc.NumOffset, scratchA)
		slow, stub := c.a.NewLabel(), c.stubLabel(v.State, exitKind(v.Aux))
		guard = func(cond amd64.Cond) { c.a.Jcc(cond, slow) }
		c.cold = append(c.cold, func() {
			c.a.Bind(slow)
			c.goSaving(uint64(ssa.GoPow), &stub)
			c.a.LoadSD(xScratch0, regCtx, abi.OffGoArgs+c.enc.NumOffset)
			c.a.Jmp(done)
		})
	}
	// y an integer from 0 to 64 (unsigned, so none below), x an integer.
	for _, p := range []struct {
		r amd64.Reg
		x amd64.XReg
	}{{scratchC, xScratch1}, {scratchA, xScratch0}} {
		c.a.Cvttsd2si(p.r, p.x)
		c.a.Cvtsi2sd(xScratch2, p.r, true)
		c.a.SSEOp(amd64.UcomiSD, xScratch2, p.x)
		guard(amd64.CondNE)
		guard(amd64.CondP)
	}
	c.a.OpImm(amd64.Cmp, scratchC, 64, true)
	guard(amd64.CondA)
	// The result in xScratch2, x's powers in xScratch0.
	c.movImm(scratchA, math.Float64bits(1))
	c.a.MovQToX(xScratch2, scratchA)
	c.a.Op(amd64.Test, scratchC, scratchC, true)
	c.a.Jcc(amd64.CondE, last)
	c.a.Bind(loop)
	c.a.MovRR(scratchB, scratchC)
	c.a.OpImm(amd64.And, scratchB, 1, true)
	c.a.Jcc(amd64.CondE, skip)
	c.a.SSEOp(amd64.MulSD, xScratch2, xScratch0)
	c.a.Bind(skip)
	c.a.ShiftImm(amd64.Shr, scratchC, 1, true)
	c.a.Jcc(amd64.CondE, last)
	c.a.SSEOp(amd64.MulSD, xScratch0, xScratch0)
	c.a.Jmp(loop)
	c.a.Bind(last)
	// At most 2**53, its bits compared as an integer's, the sign cleared.
	c.a.MovQFromX(scratchA, xScratch2)
	c.movImm(scratchB, 1<<63-1)
	c.a.Op(amd64.And, scratchA, scratchB, true)
	c.cmpWord(scratchA, math.Float64bits(1<<53))
	guard(amd64.CondA)
	c.a.SSEOp(amd64.MovAPD, xScratch0, xScratch2)
	c.a.Bind(done)
	c.setX(v, xScratch0)
}

// length is x.length: a string's, which it keeps rope or not, or an
// array's, the dense count or a sparse array's length when that is larger,
// as Object.arrayLength has it.
func (c *compiler) length(v *ssa.Value, guard func(amd64.Cond)) {
	a := v.Args[0]
	if a.Shadow == nil && c.origin.At(a) < 0 {
		c.a.Jmp(c.stubLabel(v.State, exitKind(v.Aux)))
		return
	}
	array, have := c.a.NewLabel(), c.a.NewLabel()
	c.cmpWord(c.gpr(a, scratchA), c.enc.String)
	c.a.Jcc(amd64.CondNE, array)
	c.reference(v, a, c.enc.String, guard)
	c.a.Load(scratchC, scratchC, c.enc.StringLength)
	c.a.Jmp(have)
	c.a.Bind(array)
	c.reference(v, a, c.enc.Object, guard)
	c.a.LoadU8(scratchB, scratchC, c.enc.ObjectClass)
	c.a.OpImm(amd64.Cmp, scratchB, int32(c.enc.ClassArray), false)
	typed := c.a.NewLabel()
	c.a.Jcc(amd64.CondNE, typed)
	c.cold = append(c.cold, func() {
		// A typed array's, its length in elements, where its view is in its
		// buffer, as an element read finds it (typedElement).
		c.a.Bind(typed)
		c.a.OpImm(amd64.Cmp, scratchB, int32(c.enc.ClassTypedArray), false)
		guard(amd64.CondNE)
		c.objectData(scratchC, scratchC, scratchB, c.enc.TypedData, guard)
		c.a.Load(scratchB, scratchC, c.enc.TypedStorage)
		c.a.Op(amd64.Test, scratchB, scratchB, true)
		guard(amd64.CondE)
		c.a.Load(scratchA, scratchC, c.enc.TypedEnd)
		c.a.CmpMem(scratchA, scratchB, c.enc.BufferBytes+8, true)
		guard(amd64.CondA)
		c.a.Load(scratchC, scratchC, c.enc.TypedLength)
		c.a.Jmp(have)
	})
	p := scratchA
	c.a.MovRR(p, scratchC)
	c.a.Load(scratchC, p, c.enc.ObjectElems+8)
	c.a.LoadU8(scratchB, p, c.enc.ObjectFlags)
	c.a.OpImm(amd64.And, scratchB, int32(c.enc.FlagSparse), false)
	c.a.Jcc(amd64.CondE, have)
	c.a.LoadU32(scratchB, p, c.enc.ObjectArrayLen)
	c.a.Op(amd64.Cmp, scratchB, scratchC, true)
	c.a.Jcc(amd64.CondBE, have)
	c.a.MovRR(scratchC, scratchB)
	c.a.Bind(have)
	c.a.Cvtsi2sd(xScratch0, scratchC, true)
	c.setX(v, xScratch0)
}

// stringCode is charCodeAt called on a string: the callee must be the
// intrinsic, the string flat, and the index an integer below its length;
// the code unit is a byte of an ASCII string's UTF-8, or one of the code
// units a string caches, and anything else exits.
func (c *compiler) stringCode(v *ssa.Value, guard func(amd64.Cond)) {
	if !c.reference(v, v.Args[0], c.enc.Object, guard) {
		return
	}
	c.a.Load(scratchB, regCtx, abi.OffCharCode+c.enc.RefOffset)
	c.a.Op(amd64.Cmp, scratchC, scratchB, true)
	guard(amd64.CondNE)
	if !c.stringUnit(v, v.Args[1], v.Args[2], guard) {
		return
	}
	c.a.Cvtsi2sd(xScratch0, scratchA, true)
	c.setX(v, xScratch0)
}

// stringChar is s[i] (ssa.OpStringChar): the cell of the runtime's string
// of the code unit at the index, if it is below 256, into v's register.
func (c *compiler) stringChar(v *ssa.Value, guard func(amd64.Cond)) {
	if !c.stringUnit(v, v.Args[0], v.Args[1], guard) {
		return
	}
	c.charCell(v, guard)
}

// substringChar is s.substring(i, j) or s.slice(i, j), j being i + 1,
// s.substr(i, j), j being 1, or s.charAt(i) (ssa.OpSubstringChar, its Aux
// SubstrLength, SubstrAt): the cell of the runtime's string of the code
// unit at i, below 256, of a flat string s, into v's register; 0 for
// anything else, which Go makes -- a negative i too, which slice and
// substr count from the end. It uses every scratch register, xScratch0
// and xScratch1.
func (c *compiler) substringChar(v *ssa.Value) {
	none, done := c.a.NewLabel(), c.a.NewLabel()
	fail := func(cond amd64.Cond) { c.a.Jcc(cond, none) }
	if v.Aux != ssa.SubstrAt {
		c.movImm(scratchA, math.Float64bits(1))
		c.a.MovQToX(xScratch1, scratchA)
		if v.Aux != ssa.SubstrLength {
			c.a.SSEOp(amd64.AddSD, xScratch1, c.xmm(v.Args[1], xScratch0))
		}
		c.a.SSEOp(amd64.UcomiSD, xScratch1, c.xmm(v.Args[2], xScratch0))
		fail(amd64.CondNE)
		fail(amd64.CondP)
	}
	if s := v.Args[0]; s.Shadow == nil && c.origin.At(s) < 0 {
		// A primitive, never a string's reference (reference, which would
		// leave where this has no state to leave with).
		c.a.Jmp(none)
	} else {
		c.stringUnit(v, s, v.Args[1], fail)
	}
	c.a.OpImm(amd64.Cmp, scratchA, 256, true)
	fail(amd64.CondAE)
	c.a.Load(scratchB, regCtx, abi.OffCharStrings)
	c.a.Op(amd64.Test, scratchB, scratchB, true)
	fail(amd64.CondE)
	c.a.ShiftImm(amd64.Shl, scratchA, 4, true)
	c.a.Op(amd64.Add, scratchA, scratchB, true)
	c.a.Jmp(done)
	c.a.Bind(none)
	c.movImm(scratchA, 0)
	c.a.Bind(done)
	c.setG(v, scratchA)
}

// codeChar is String.fromCharCode(c) (ssa.OpCodeChar): the cell of the
// runtime's string of code unit c, an integer below 256, into v's
// register.
func (c *compiler) codeChar(v *ssa.Value, guard func(amd64.Cond)) {
	c.index(v.Args[0], guard)
	c.charCell(v, guard)
}

// oddball is ssa.OpOddballF64: v's operand's number, an oddball's as
// ToNumber makes it -- NaN for undefined, 0 for null and false, 1 for true
// -- into xScratch0 and v's register; anything else fails. It uses
// scratchA.
func (c *compiler) oddball(v *ssa.Value, fail func(amd64.Cond)) {
	r := c.gpr(v.Args[0], scratchB)
	done, tagged, zero := c.a.NewLabel(), c.a.NewLabel(), c.a.NewLabel()
	c.a.MovRR(scratchA, r)
	c.a.ShiftImm(amd64.Shr, scratchA, 51, true)
	c.a.OpImm(amd64.Cmp, scratchA, 0x1FFF, false)
	c.a.Jcc(amd64.CondE, tagged)
	c.a.MovQToX(xScratch0, r)
	c.a.Jmp(done)
	c.a.Bind(tagged)
	for _, k := range []struct {
		word, bits uint64
	}{{c.enc.Undefined, 0x7FF8000000000000}, {c.enc.True, 0x3FF0000000000000}} {
		next := c.a.NewLabel()
		c.cmpWord(r, k.word)
		c.a.Jcc(amd64.CondNE, next)
		c.movImm(scratchA, k.bits)
		c.a.MovQToX(xScratch0, scratchA)
		c.a.Jmp(done)
		c.a.Bind(next)
	}
	c.cmpWord(r, c.enc.Null)
	c.a.Jcc(amd64.CondE, zero)
	c.cmpWord(r, c.enc.False)
	if v.Index == 1 && c.enc.CallGo != 0 && exitKind(v.Aux) == abi.ExitHost {
		// Anything else -- a string -- converted by Go (ssa.GoNumber),
		// out of line; what it refuses leaves.
		slow := c.a.NewLabel()
		c.a.Jcc(amd64.CondNE, slow)
		c.cold = append(c.cold, func() {
			c.a.Bind(slow)
			stub := c.stubLabel(v.State, exitKind(v.Aux))
			c.goArg(abi.OffGoArgs, v.Args[0])
			c.goSaving(uint64(ssa.GoNumber), &stub)
			c.a.LoadSD(xScratch0, regCtx, abi.OffGoArgs+c.enc.NumOffset)
			c.a.Jmp(done)
		})
	} else {
		fail(amd64.CondNE)
	}
	c.a.Bind(zero)
	c.movImm(scratchA, 0)
	c.a.MovQToX(xScratch0, scratchA)
	c.a.Bind(done)
	c.setX(v, xScratch0)
}

// freeze is ssa.OpFreeze: the building byte of the string v's operand is,
// if it is one, cleared; nothing else is touched. It uses scratchA to
// scratchC.
func (c *compiler) freeze(v *ssa.Value) {
	x := v.Args[0]
	var w amd64.Reg
	if remat(x) {
		c.materialize(x, scratchA)
		w = scratchA
	} else {
		w = c.gpr(x, scratchA)
	}
	done := c.a.NewLabel()
	c.movImm(scratchB, c.enc.String)
	c.a.Op(amd64.Cmp, w, scratchB, true)
	c.a.Jcc(amd64.CondNE, done)
	c.pointerWord(x, w)
	c.a.Op(amd64.Test, scratchB, scratchB, true)
	c.a.Jcc(amd64.CondE, done)
	c.movImm(scratchC, 0)
	c.a.Store8(scratchB, c.enc.StringBuilding, scratchC)
	c.a.Bind(done)
}

// charCell is the cell of the runtime's string of the code unit in
// scratchA, below 256 (abi.Context.CharStrings), into v's register.
func (c *compiler) charCell(v *ssa.Value, guard func(amd64.Cond)) {
	c.a.OpImm(amd64.Cmp, scratchA, 256, true)
	guard(amd64.CondAE)
	c.a.Load(scratchB, regCtx, abi.OffCharStrings)
	c.a.Op(amd64.Test, scratchB, scratchB, true)
	guard(amd64.CondE)
	c.a.ShiftImm(amd64.Shl, scratchA, 4, true)
	c.a.Op(amd64.Add, scratchA, scratchB, true)
	c.setG(v, scratchA)
}

// stringUnit reads the code unit at index key of str, a flat string, into
// scratchA, failing for any other value, a string that is not flat, or an
// index past its end, and reports whether the code goes on. It uses
// scratchB and scratchC.
func (c *compiler) stringUnit(v, str, key *ssa.Value, guard func(amd64.Cond)) bool {
	if !c.reference(v, str, c.enc.String, guard) {
		return false
	}
	c.a.Load(scratchB, scratchC, c.enc.StringLeft)
	c.a.Op(amd64.Test, scratchB, scratchB, true)
	guard(amd64.CondNE)
	c.index(key, guard)
	c.a.Load(scratchB, scratchC, c.enc.StringLength)
	c.a.Op(amd64.Cmp, scratchA, scratchB, true)
	guard(amd64.CondAE)
	units, done := c.a.NewLabel(), c.a.NewLabel()
	c.a.LoadU8(scratchB, scratchC, c.enc.StringASCII)
	c.a.Op(amd64.Test, scratchB, scratchB, false)
	c.a.Jcc(amd64.CondE, units)
	c.a.Load(scratchB, scratchC, c.enc.StringData)
	c.a.Op(amd64.Add, scratchB, scratchA, true)
	c.a.LoadU8(scratchA, scratchB, 0)
	c.a.Jmp(done)
	c.a.Bind(units)
	c.a.Load(scratchB, scratchC, c.enc.StringU16)
	c.a.Op(amd64.Test, scratchB, scratchB, true)
	guard(amd64.CondE)
	c.a.Op(amd64.Add, scratchA, scratchA, true)
	c.a.Op(amd64.Add, scratchB, scratchA, true)
	c.a.LoadU16(scratchA, scratchB, 0)
	c.a.Bind(done)
	return true
}

// index converts an element's key, a double, to an index in scratchA,
// failing unless it is an integer in [0, 2**32): truncation and back must
// give the key (NaN and anything past 2**63 do not), and the top half must
// be clear. Negative zero is index 0. It uses scratchB and xScratch1.
func (c *compiler) index(key *ssa.Value, fail func(amd64.Cond)) {
	k := c.xmm(key, xScratch0)
	c.a.Cvttsd2si(scratchA, k)
	c.a.Cvtsi2sd(xScratch1, scratchA, true)
	c.a.SSEOp(amd64.UcomiSD, xScratch1, k)
	fail(amd64.CondNE)
	fail(amd64.CondP)
	c.a.MovRR(scratchB, scratchA)
	c.a.ShiftImm(amd64.Shr, scratchB, 32, true)
	fail(amd64.CondNE)
}

// element turns the index in scratchA into the address of an array's
// element there, in scratchA, and its number word, in scratchB, failing
// unless the element is present and holds a number. It uses scratchC.
func (c *compiler) element(array *ssa.Value, fail func(amd64.Cond)) {
	p := c.gpr(array, scratchC)
	c.a.Load(scratchB, p, c.enc.ObjectElems+8)
	c.a.Op(amd64.Cmp, scratchA, scratchB, true)
	fail(amd64.CondAE)
	c.a.Load(scratchB, p, c.enc.ObjectElems)
	c.a.ShiftImm(amd64.Shl, scratchA, 4, true)
	c.a.Op(amd64.Add, scratchA, scratchB, true)
	c.a.Load(scratchB, scratchA, c.enc.NumOffset)
	c.cmpWord(scratchB, abi.NumberLimit)
	fail(amd64.CondAE)
}

// indexedOf finds an array or a typed array (OpIndexedOf), into v's
// register: what an element read or written as a number takes, telling
// the two apart as it runs (typedElement).
func (c *compiler) indexedOf(v *ssa.Value, guard func(amd64.Cond)) {
	if !c.objectOf(v, guard) {
		return
	}
	ok := c.a.NewLabel()
	c.a.LoadU8(scratchB, scratchC, c.enc.ObjectClass)
	c.a.OpImm(amd64.Cmp, scratchB, int32(c.enc.ClassArray), false)
	c.a.Jcc(amd64.CondE, ok)
	c.a.OpImm(amd64.Cmp, scratchB, int32(c.enc.ClassTypedArray), false)
	guard(amd64.CondNE)
	c.a.Bind(ok)
	c.setG(v, scratchC)
}

// typedArray jumps to typed unless the object array, from OpIndexedOf, is
// an Array. It uses scratchB.
func (c *compiler) typedArray(array *ssa.Value, typed amd64.Label) {
	c.a.LoadU8(scratchB, c.gpr(array, scratchB), c.enc.ObjectClass)
	c.a.OpImm(amd64.Cmp, scratchB, int32(c.enc.ClassArray), false)
	c.a.Jcc(amd64.CondNE, typed)
}

// typedElement finds the element of the typed array array that key's
// number names, as the VM's typedWrite does, failing unless the view is in
// its buffer -- Go has looked at it, it does not track a resizable
// buffer's length, its buffer is not detached and has not shrunk past it
// -- and, for a store, its bytes may be written, and the key is an integer
// below its length: the element's address in scratchA, and the view's
// element type in scratchC. It uses every scratch register.
func (c *compiler) typedElement(array, key *ssa.Value, store bool, fail func(amd64.Cond)) {
	c.objectData(scratchC, c.gpr(array, scratchC), scratchB, c.enc.TypedData, fail)
	c.a.Load(scratchB, scratchC, c.enc.TypedStorage)
	c.a.Op(amd64.Test, scratchB, scratchB, true)
	fail(amd64.CondE)
	c.a.Load(scratchA, scratchC, c.enc.TypedEnd)
	c.a.CmpMem(scratchA, scratchB, c.enc.BufferBytes+8, true)
	fail(amd64.CondA)
	if store {
		c.a.LoadU8(scratchA, scratchB, c.enc.BufferImmutable)
		c.a.Op(amd64.Test, scratchA, scratchA, false)
		fail(amd64.CondNE)
	}
	c.a.Load(scratchB, scratchB, c.enc.BufferBytes)
	c.a.Load(scratchA, scratchC, c.enc.TypedOffset)
	c.a.Op(amd64.Add, scratchB, scratchA, true)
	// The key an integer -- not NaN, which converts to 1<<63 -- below the
	// length, compared unsigned: a negative one is past it.
	k := c.xmm(key, xScratch0)
	c.a.Cvttsd2si(scratchA, k)
	c.a.Cvtsi2sd(xScratch1, scratchA, true)
	c.a.SSEOp(amd64.UcomiSD, xScratch1, k)
	fail(amd64.CondNE)
	fail(amd64.CondP)
	c.a.CmpMem(scratchA, scratchC, c.enc.TypedLength, true)
	fail(amd64.CondAE)
	c.a.LoadU8(scratchC, scratchC, c.enc.TypedKind)
	// The address of the element, which the element type's size scales:
	// its shift is the type's (elemShift), the shifted index added to the
	// first element's address, in scratchB, at each type's case.
}

// typedKinds are the element types native code reads and writes, the most
// common in compiled code first, and their sizes' shifts.
var typedKinds = [...]struct {
	kind  int32
	shift uint8
}{
	{abi.ElemInt32, 2}, {abi.ElemUint8, 0}, {abi.ElemFloat64, 3}, {abi.ElemInt8, 0},
	{abi.ElemUint16, 1}, {abi.ElemInt16, 1}, {abi.ElemUint32, 2}, {abi.ElemFloat32, 2},
	{abi.ElemUint8Clamped, 0},
}

// typedCases emits, for each element type native code makes, a case of
// the type in scratchC: the element's address into scratchA, then each's
// own code, which ends where it goes on. Another type jumps to other.
func (c *compiler) typedCases(other amd64.Label, each func(kind int32)) {
	for _, t := range typedKinds {
		next := c.a.NewLabel()
		c.a.OpImm(amd64.Cmp, scratchC, t.kind, false)
		c.a.Jcc(amd64.CondNE, next)
		if t.shift != 0 {
			c.a.ShiftImm(amd64.Shl, scratchA, t.shift, true)
		}
		c.a.Op(amd64.Add, scratchA, scratchB, true)
		each(t.kind)
		c.a.Bind(next)
	}
	c.a.Jmp(other)
}

// elemStore stores any value in an Array's element (OpElemStore), as
// propStore stores one in a property: the element there, or a hole the
// array may take an element at (storeCell), the cells the store's checks
// name compared with its cell first, a pointer word written or overwritten
// with Go's write barrier while the collector marks (valueStore). What it
// does not store -- a typed array's element, past the dense elements -- Go
// stores (ssa.GoElemStore), or, where
// the store has checks, a reference live after it read from an element,
// which Go's store could move as it grows the array, the store exits. It
// uses every scratch register and xScratch0 to xScratch2.
func (c *compiler) elemStore(v *ssa.Value) {
	x := v.Args[2]
	stub := c.stubLabel(v.State, exitKind(v.Aux))
	slow, done := c.a.NewLabel(), c.a.NewLabel()
	checked := len(v.Args) > 3
	if c.enc.CallGo == 0 || checked {
		slow = stub
	}
	fail := func(cond amd64.Cond) { c.a.Jcc(cond, slow) }
	typed := c.a.NewLabel()
	fresh := v.Index == ssa.StoreFresh && c.enc.ValueSize == 16
	if !fresh {
		c.typedArray(v.Args[0], typed)
	}
	// The value's pointer word, as propStore finds it, in xScratch2.
	var w amd64.Reg
	if remat(x) {
		c.materialize(x, scratchA)
		w = scratchA
	} else {
		w = c.gpr(x, scratchA)
	}
	number, have := c.a.NewLabel(), c.a.NewLabel()
	if x.Shadow != nil || c.origin.At(x) >= 0 {
		c.cmpWord(w, abi.NumberLimit)
		c.a.Jcc(amd64.CondB, number)
	}
	c.pointerWord(x, w)
	c.a.Jmp(have)
	c.a.Bind(number)
	c.movImm(scratchB, 0)
	c.a.Bind(have)
	c.a.MovQToX(xScratch2, scratchB)
	// The element's cell, in scratchA.
	c.index(v.Args[1], fail)
	found := c.a.NewLabel()
	if fresh {
		c.freshCell(v.Args[0], fail)
	} else {
		c.storeCell(v.Args[0], !checked, fail, found)
	}
	c.a.Bind(found)
	for _, s := range v.Args[3:] {
		// A live value read from this cell keeps its pointer word there:
		// unless it is none, a primitive's (storeChecks).
		other := c.a.NewLabel()
		c.a.Op(amd64.Cmp, c.gpr(s, scratchB), scratchA, true)
		c.a.Jcc(amd64.CondNE, other)
		c.a.Load(scratchB, scratchA, c.enc.RefOffset)
		c.a.Op(amd64.Test, scratchB, scratchB, true)
		c.a.Jcc(amd64.CondNE, stub)
		c.a.Bind(other)
	}
	scalar, viaGo := c.a.NewLabel(), c.a.NewLabel()
	c.a.MovQFromX(scratchC, xScratch2)
	c.a.Load(scratchB, scratchA, c.enc.RefOffset)
	c.a.Op(amd64.Or, scratchB, scratchC, true)
	c.a.Jcc(amd64.CondE, scalar)
	// A pointer word written or overwritten: while the collector marks,
	// with Go's write barrier (valueStore).
	c.a.MovQToX(xScratch0, c.gpr(x, scratchB))
	c.movImm(scratchB, c.enc.WriteBarrier)
	c.a.LoadU8(scratchB, scratchB, 0)
	c.a.Op(amd64.Test, scratchB, scratchB, false)
	c.a.Jcc(amd64.CondNE, viaGo)
	c.a.Bind(scalar)
	c.a.Store(scratchA, c.enc.NumOffset, c.gpr(x, scratchB))
	c.a.Store(scratchA, c.enc.RefOffset, scratchC)
	c.a.Bind(done)
	if !fresh {
		c.cold = append(c.cold, func() {
			// A typed array's element: a number stored natively, converted
			// to its type, as OpElemWrite stores one.
			c.a.Bind(typed)
			var tw amd64.Reg
			if remat(x) {
				c.materialize(x, scratchA)
				tw = scratchA
			} else {
				tw = c.gpr(x, scratchA)
			}
			c.cmpWord(tw, abi.NumberLimit)
			c.a.Jcc(amd64.CondAE, slow)
			c.a.MovQToX(xScratch2, tw)
			c.typedWrite(v, done, slow, func() amd64.XReg { return xScratch2 })
		})
	}
	c.cold = append(c.cold, func() {
		c.a.Bind(viaGo)
		c.valueStore()
		c.a.Jmp(done)
	})
	if c.enc.CallGo == 0 || checked {
		return
	}
	c.cold = append(c.cold, func() {
		// Any other store Go makes if it can (ssa.GoElemStore): the array,
		// the key and the value's words in the context's GoArgs.
		c.a.Bind(slow)
		size := int32(unsafe.Sizeof(abi.GoArg{}))
		c.a.Store(regCtx, abi.OffGoArgs+c.enc.RefOffset, c.gpr(v.Args[0], scratchC))
		c.a.MovQFromX(scratchA, c.xmm(v.Args[1], xScratch0))
		c.a.Store(regCtx, abi.OffGoArgs+size+c.enc.NumOffset, scratchA)
		var w amd64.Reg
		if remat(x) {
			c.materialize(x, scratchA)
			w = scratchA
		} else {
			w = c.gpr(x, scratchA)
		}
		c.a.Store(regCtx, abi.OffGoArgs+2*size+c.enc.NumOffset, w)
		number, have := c.a.NewLabel(), c.a.NewLabel()
		if x.Shadow != nil || c.origin.At(x) >= 0 {
			c.cmpWord(w, abi.NumberLimit)
			c.a.Jcc(amd64.CondB, number)
		}
		c.pointerWord(x, w)
		c.a.Jmp(have)
		c.a.Bind(number)
		c.movImm(scratchB, 0)
		c.a.Bind(have)
		c.a.Store(regCtx, abi.OffGoArgs+2*size+c.enc.RefOffset, scratchB)
		c.goSaving(uint64(ssa.GoElemStore)|uint64(v.Index&ssa.StoreFresh)<<abi.GoOpIndexShift, &stub)
		c.a.Jmp(done)
	})
}

// typedRead reads a typed array's element (OpElemRead) into xScratch0,
// then jumps to done; Go reads one native code does not (ssa.GoTypedRead),
// which, if it refuses, exits to v's state.
func (c *compiler) typedRead(v *ssa.Value, done amd64.Label) {
	stub := c.stubLabel(v.State, exitKind(v.Aux))
	slow := stub
	if c.enc.CallGo != 0 {
		slow = c.a.NewLabel()
	}
	c.typedElement(v.Args[0], v.Args[1], false, func(cond amd64.Cond) { c.a.Jcc(cond, slow) })
	c.typedCases(slow, func(kind int32) {
		switch kind {
		case abi.ElemInt8:
			c.a.LoadS8(scratchA, scratchA, 0)
		case abi.ElemUint8, abi.ElemUint8Clamped:
			c.a.LoadU8(scratchA, scratchA, 0)
		case abi.ElemInt16:
			c.a.LoadS16(scratchA, scratchA, 0)
		case abi.ElemUint16:
			c.a.LoadU16(scratchA, scratchA, 0)
		case abi.ElemInt32:
			c.a.LoadS32(scratchA, scratchA, 0)
		case abi.ElemUint32:
			c.a.LoadU32(scratchA, scratchA, 0)
		case abi.ElemFloat32:
			c.a.LoadU32(scratchA, scratchA, 0)
			c.a.MovQToX(xScratch0, scratchA)
			c.a.Cvtss2sd(xScratch0, xScratch0)
			c.a.Jmp(done)
			return
		case abi.ElemFloat64:
			c.a.LoadSD(xScratch0, scratchA, 0)
			c.a.Jmp(done)
			return
		}
		c.a.Cvtsi2sd(xScratch0, scratchA, true)
		c.a.Jmp(done)
	})
	if c.enc.CallGo == 0 {
		return
	}
	c.a.Bind(slow)
	size := int32(unsafe.Sizeof(abi.GoArg{}))
	c.a.Store(regCtx, abi.OffGoArgs+c.enc.RefOffset, c.gpr(v.Args[0], scratchC))
	c.a.MovQFromX(scratchA, c.xmm(v.Args[1], xScratch0))
	c.a.Store(regCtx, abi.OffGoArgs+size+c.enc.NumOffset, scratchA)
	c.goSaving(uint64(ssa.GoTypedRead), &stub)
	c.a.LoadSD(xScratch0, regCtx, abi.OffGoArgs+c.enc.NumOffset)
	c.a.Jmp(done)
}

// typedWrite stores a number in a typed array's element (OpElemWrite),
// converted to its type, then jumps to done; what it does not store --
// a conversion it does not make, a view it does not find in its buffer --
// goes to slow.
// The value is in the register value gives, a double.
func (c *compiler) typedWrite(v *ssa.Value, done, slow amd64.Label, value func() amd64.XReg) {
	fail := func(cond amd64.Cond) { c.a.Jcc(cond, slow) }
	c.typedElement(v.Args[0], v.Args[1], true, fail)
	c.typedCases(slow, func(kind int32) {
		x := value()
		switch kind {
		case abi.ElemFloat64:
			c.a.StoreSD(scratchA, 0, x)
		case abi.ElemFloat32:
			c.a.Cvtsd2ss(xScratch1, x)
			c.a.MovQFromX(scratchB, xScratch1)
			c.a.Store32(scratchA, 0, scratchB)
		case abi.ElemUint8Clamped:
			if !c.enc.Round {
				c.a.Jmp(slow)
				return
			}
			// Saturated, and rounded half to even: 0 for NaN and what is
			// not above 0, 255 for what is not below it.
			low, high, store := c.a.NewLabel(), c.a.NewLabel(), c.a.NewLabel()
			c.a.SSEOp(amd64.XorPD, xScratch1, xScratch1)
			c.a.SSEOp(amd64.UcomiSD, x, xScratch1)
			c.a.Jcc(amd64.CondBE, low)
			c.movImm(scratchB, math.Float64bits(255))
			c.a.MovQToX(xScratch1, scratchB)
			c.a.SSEOp(amd64.UcomiSD, x, xScratch1)
			c.a.Jcc(amd64.CondAE, high)
			c.a.RoundSD(xScratch1, x, amd64.RoundNearest)
			c.a.Cvttsd2si(scratchB, xScratch1)
			c.a.Jmp(store)
			c.a.Bind(low)
			c.a.Op(amd64.Xor, scratchB, scratchB, false)
			c.a.Jmp(store)
			c.a.Bind(high)
			c.movImm(scratchB, 255)
			c.a.Bind(store)
			c.a.Store8(scratchA, 0, scratchB)
		default:
			// ToInt32's bits, whose low ones each integer type keeps:
			// CVTTSD2SI's where |x| < 2**63, Go's beyond, and for NaN and
			// the infinities.
			c.a.Cvttsd2si(scratchB, x)
			c.movImm(scratchC, 1<<63)
			c.a.Op(amd64.Cmp, scratchB, scratchC, true)
			fail(amd64.CondE)
			switch kind {
			case abi.ElemInt8, abi.ElemUint8:
				c.a.Store8(scratchA, 0, scratchB)
			case abi.ElemInt16, abi.ElemUint16:
				c.a.Store16(scratchA, 0, scratchB)
			default:
				c.a.Store32(scratchA, 0, scratchB)
			}
		}
		c.a.Jmp(done)
	})
}

// elementCell turns the index in scratchA into the address of an array's
// element there, in scratchA, failing unless it is within the dense
// elements and not a hole (whose word is the uninitialized marker's). It
// uses scratchB and scratchC.
func (c *compiler) elementCell(array *ssa.Value, fail func(amd64.Cond)) {
	p := c.gpr(array, scratchC)
	c.a.Load(scratchB, p, c.enc.ObjectElems+8)
	c.a.Op(amd64.Cmp, scratchA, scratchB, true)
	fail(amd64.CondAE)
	c.a.Load(scratchB, p, c.enc.ObjectElems)
	c.a.ShiftImm(amd64.Shl, scratchA, 4, true)
	c.a.Op(amd64.Add, scratchA, scratchB, true)
	c.a.Load(scratchB, scratchA, c.enc.NumOffset)
	c.cmpWord(scratchB, c.enc.Uninitialized)
	fail(amd64.CondE)
}

// storeCell is elementCell for a store of any value (elemStore): a hole
// is stored into as well, as V8's code stores into a holey array's, where
// the array may take an element it has none of (arrayGuards), the cell's
// address in scratchA at found. A hole has no pointer word: the store's
// barrier is the value's alone. Holes had each been a call of Go
// (ssa.GoElemStore): AWFY's Storage fills new Array(4) with its trees,
// 5.5 million such stores a run, Havlak 3.6 million. So, where appends, is
// a store at the array's length where it has room, as appendingCall
// appends a number: the length one more, the cell the one past the old
// last element -- whatever an earlier element there left in it, which the
// store's barrier overwrites as Go's append would. A store with checks
// does not append: one of them exiting after it would leave the length
// grown.
func (c *compiler) storeCell(array *ssa.Value, appends bool, fail func(amd64.Cond), found amd64.Label) {
	protos := c.f.ArrayProtos
	if protos[0].Object == 0 || c.enc.ValueSize != 16 {
		c.elementCell(array, fail)
		return
	}
	p := c.gpr(array, scratchC)
	c.a.Load(scratchB, p, c.enc.ObjectElems+8)
	c.a.Op(amd64.Cmp, scratchA, scratchB, true)
	if appends {
		end := c.a.NewLabel()
		c.a.Jcc(amd64.CondAE, end)
		c.cold = append(c.cold, func() {
			c.a.Bind(end)
			fail(amd64.CondNE)
			c.a.Load(scratchB, c.gpr(array, scratchC), c.enc.ObjectElems+16)
			c.a.Op(amd64.Cmp, scratchA, scratchB, true)
			fail(amd64.CondAE)
			c.a.MovQToX(xScratch0, scratchA)
			c.arrayGuards(c.gpr(array, scratchC), protos, fail)
			c.a.MovQFromX(scratchA, xScratch0)
			p := c.gpr(array, scratchC)
			c.a.MovRR(scratchB, scratchA)
			c.a.OpImm(amd64.Add, scratchB, 1, true)
			c.a.Store(p, c.enc.ObjectElems+8, scratchB)
			c.a.Load(scratchB, p, c.enc.ObjectElems)
			c.a.ShiftImm(amd64.Shl, scratchA, 4, true)
			c.a.Op(amd64.Add, scratchA, scratchB, true)
			c.a.Jmp(found)
		})
	} else {
		fail(amd64.CondAE)
	}
	c.a.Load(scratchB, p, c.enc.ObjectElems)
	c.a.ShiftImm(amd64.Shl, scratchA, 4, true)
	c.a.Op(amd64.Add, scratchA, scratchB, true)
	c.a.Load(scratchB, scratchA, c.enc.NumOffset)
	c.cmpWord(scratchB, c.enc.Uninitialized)
	hole := c.a.NewLabel()
	c.a.Jcc(amd64.CondE, hole)
	c.cold = append(c.cold, func() {
		c.a.Bind(hole)
		c.a.MovQToX(xScratch0, scratchA)
		c.arrayGuards(c.gpr(array, scratchC), protos, fail)
		c.a.MovQFromX(scratchA, xScratch0)
		c.a.Jmp(found)
	})
}

// freshCell is storeCell for a store into an array a loop made
// (ssa.StoreFresh), the index in scratchA: at the length, where the array
// has room, the length one more and the cell the one past the old last
// element, in scratchA; anything else fails. Nothing else is checked: the
// array is an Array no code but the loop's has, and the element is defined
// there whatever its prototypes hold.
func (c *compiler) freshCell(array *ssa.Value, fail func(amd64.Cond)) {
	p := c.gpr(array, scratchC)
	c.a.Load(scratchB, p, c.enc.ObjectElems+8)
	c.a.Op(amd64.Cmp, scratchA, scratchB, true)
	fail(amd64.CondNE)
	c.a.Load(scratchB, p, c.enc.ObjectElems+16)
	c.a.Op(amd64.Cmp, scratchA, scratchB, true)
	fail(amd64.CondAE)
	c.a.MovRR(scratchB, scratchA)
	c.a.OpImm(amd64.Add, scratchB, 1, true)
	c.a.Store(p, c.enc.ObjectElems+8, scratchB)
	c.a.Load(scratchB, p, c.enc.ObjectElems)
	c.a.ShiftImm(amd64.Shl, scratchA, 4, true)
	c.a.Op(amd64.Add, scratchA, scratchB, true)
}

// guardFor is a guard of v's for code that may keep it: a jump to the stub
// that exits to v's state.
func (c *compiler) guardFor(v *ssa.Value) func(amd64.Cond) {
	return func(cond amd64.Cond) {
		c.a.Jcc(cond, c.stubLabel(v.State, exitKind(v.Aux)))
	}
}

// value emits one value.
func (c *compiler) value(v *ssa.Value, b *ssa.Block) {
	arg := func(i int) *ssa.Value { return v.Args[i] }
	// guard is for the guards emitted here; one passed on, which may be
	// kept for cold code, is made only for the values that pass one
	// (guardFor), not for every value.
	guard := func(cond amd64.Cond) {
		c.a.Jcc(cond, c.stubLabel(v.State, exitKind(v.Aux)))
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
		// The captured local's slot in the frame.
		d := c.gdst(v)
		c.a.Lea(d, regLocals, int32(v.Index)*c.enc.ValueSize)
		c.setG(v, d)
	case ssa.OpUpvalueCell:
		// The binding's cell, then where its value is now: a captured
		// binding's, or, Aux 1, a captured local's of the code's own, or,
		// Aux 2, a captured binding of the closure whose cells are at
		// Const.Bits.
		d := c.gdst(v)
		base := abi.OffUpvalues
		if v.Aux == 1 {
			base = abi.OffCells
		}
		if v.Aux == 2 {
			c.movImm(d, v.Const.Bits)
		} else {
			c.a.Load(d, regCtx, base)
		}
		c.a.Load(d, d, int32(v.Index)*8)
		c.a.Load(d, d, c.enc.UpvalueSlot)
		c.setG(v, d)
	case ssa.OpConstF64:
		c.movImm(scratchA, v.Const.Bits)
		c.a.MovQToX(xScratch0, scratchA)
		c.setX(v, xScratch0)
	case ssa.OpUnboxF64:
		r := c.gpr(arg(0), scratchB)
		c.a.MovRR(scratchA, r)
		c.a.ShiftImm(amd64.Shr, scratchA, 51, true)
		c.a.OpImm(amd64.Cmp, scratchA, 0x1FFF, false)
		guard(amd64.CondE)
		c.a.MovQToX(xScratch0, r)
		c.setX(v, xScratch0)
	case ssa.OpOddballF64:
		c.oddball(v, guard)
	case ssa.OpCheckInit:
		r := c.gpr(arg(0), scratchB)
		c.cmpWord(r, c.enc.Uninitialized)
		guard(amd64.CondE)
	case ssa.OpCheckTrue:
		r := c.gpr(arg(0), scratchB)
		c.movImm(scratchA, c.enc.True)
		c.a.Op(amd64.Cmp, r, scratchA, true)
		guard(amd64.CondNE)
	case ssa.OpTruth:
		c.truth(v, c.guardFor(v))
	case ssa.OpArrayOf:
		c.arrayOf(v, c.guardFor(v))
	case ssa.OpIndexedOf:
		c.indexedOf(v, c.guardFor(v))
	case ssa.OpObjectOf:
		if c.objectOf(v, c.guardFor(v)) {
			c.setG(v, scratchC)
		}
	case ssa.OpInstanceOf:
		c.instanceOf(v, c.guardFor(v))
	case ssa.OpTypeIs:
		c.typeIs(v, c.guardFor(v))
	case ssa.OpSameObject:
		if v.Index == 1 {
			// A typed array of the prototype at Const.Bits.
			p := c.gpr(arg(0), scratchA)
			c.a.LoadU8(scratchB, p, c.enc.ObjectClass)
			c.a.OpImm(amd64.Cmp, scratchB, int32(c.enc.ClassTypedArray), false)
			guard(amd64.CondNE)
			c.a.Load(scratchB, p, c.enc.ObjectProto)
			c.cmpWord(scratchB, v.Const.Bits)
			guard(amd64.CondNE)
			break
		}
		c.cmpWord(c.gpr(arg(0), scratchA), v.Const.Bits)
		guard(amd64.CondNE)
	case ssa.OpAccessorIs:
		// The cell's accessor pair, and its getter's or its setter's
		// function.
		p := c.gpr(arg(0), scratchA)
		c.a.Load(scratchB, p, c.enc.NumOffset)
		c.cmpWord(scratchB, c.enc.Accessor)
		guard(amd64.CondNE)
		c.a.Load(scratchB, p, c.enc.RefOffset)
		at := c.enc.AccessorGetter
		if v.Index == 1 {
			at = c.enc.AccessorSetter
		}
		c.a.Load(scratchB, scratchB, at)
		c.cmpWord(scratchB, v.Const.Bits)
		guard(amd64.CondNE)
	case ssa.OpCall:
		c.call(v, c.guardFor(v))
	case ssa.OpCallCell:
		c.a.MovRR(scratchA, regCtx)
		c.a.OpImm(amd64.Add, scratchA, abi.OffKeep+int32(v.Args[0].Index)*int32(c.enc.ValueSize), true)
		c.setG(v, scratchA)
	case ssa.OpKeepRef:
		c.keepRef(v)
	case ssa.OpKeep:
		c.keep(v)
	case ssa.OpKept:
		c.setG(v, c.gpr(arg(1), scratchA))
	case ssa.OpBigUnbox:
		c.bigUnbox(v, c.guardFor(v))
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
		c.a.MovRR(scratchA, regCtx)
		c.a.OpImm(amd64.Add, scratchA, abi.OffKeep+int32(v.Args[0].Index)*int32(c.enc.ValueSize), true)
		c.setG(v, scratchA)
	case ssa.OpAddI64, ssa.OpSubI64, ssa.OpMulI64:
		x, y := c.gpr(arg(0), scratchA), c.gpr(arg(1), scratchB)
		if x != scratchA {
			c.a.MovRR(scratchA, x)
		}
		switch v.Op {
		case ssa.OpAddI64:
			c.a.Op(amd64.Add, scratchA, y, true)
		case ssa.OpSubI64:
			c.a.Op(amd64.Sub, scratchA, y, true)
		default:
			c.a.Imul(scratchA, y)
		}
		// Outside the int64 range: a BigInt on the heap, Go's.
		c.guardFor(v)(amd64.CondO)
		c.setG(v, scratchA)
	case ssa.OpCmpI64:
		c.a.Op(amd64.Cmp, c.gpr(arg(0), scratchA), c.gpr(arg(1), scratchB), true)
		cond := bigCmpCond(ir.Operator(v.Aux), arg(0).Type == ssa.Uint64)
		c.a.Setcc(cond, scratchA)
		c.a.MovZX8(scratchA, scratchA)
		c.setG(v, scratchA)
	case ssa.OpFrameRoom:
		c.a.MovRR(scratchA, regStack)
		c.a.Load(scratchB, regCtx, abi.OffStackBase)
		c.a.Op(amd64.Sub, scratchA, scratchB, true)
		c.a.ShiftImm(amd64.Shr, scratchA, 4, true)
		c.a.OpImm(amd64.Add, scratchA, int32(v.Index), true)
		c.a.Load(scratchB, regCtx, abi.OffStackEnd)
		c.a.Op(amd64.Cmp, scratchA, scratchB, true)
		guard(amd64.CondA)
		c.a.Load(scratchA, regCtx, abi.OffLevel)
		c.a.OpImm(amd64.Add, scratchA, int32(max(v.Const.Bits, 1)), true)
		c.a.Load(scratchB, regCtx, abi.OffBounds)
		c.a.Load(scratchB, scratchB, abi.OffBoundsLevelLimit)
		c.a.Op(amd64.Cmp, scratchA, scratchB, true)
		guard(amd64.CondAE)
	case ssa.OpTailSelf:
		c.tailSelf(v, guard)
	case ssa.OpIterClosed:
		// The cursor's state: done, or an array's walked in place.
		x, ok := v.Args[0], c.a.NewLabel()
		if c.enc.IterState == 0 {
			c.a.Jmp(c.stubLabel(v.State, exitKind(v.Aux)))
			break
		}
		c.pointerWord(x, c.gpr(x, scratchA))
		c.a.Op(amd64.Test, scratchB, scratchB, true)
		guard(amd64.CondE)
		c.objectData(scratchB, scratchB, scratchC, c.enc.IterState, guard)
		c.a.LoadU8(scratchC, scratchB, c.enc.IterDone)
		c.a.Op(amd64.Test, scratchC, scratchC, false)
		c.a.Jcc(amd64.CondNE, ok)
		c.a.Load(scratchC, scratchB, c.enc.IterArr)
		c.a.Op(amd64.Test, scratchC, scratchC, true)
		guard(amd64.CondE)
		// An array's, while nothing up its iterator's chain has a return
		// method: the chain's validity cell, found at Const's address, is
		// there and valid (ssa.WalkedCloseFeedback).
		if v.Const.Bits == 0 {
			c.a.Jmp(c.stubLabel(v.State, exitKind(v.Aux)))
			break
		}
		c.movImm(scratchC, v.Const.Bits)
		c.a.Load(scratchC, scratchC, 0)
		c.a.Op(amd64.Test, scratchC, scratchC, true)
		guard(amd64.CondE)
		c.a.LoadU8(scratchC, scratchC, 0)
		c.a.Op(amd64.Test, scratchC, scratchC, false)
		guard(amd64.CondE)
		c.a.Bind(ok)
	case ssa.OpSameSymbol:
		if c.reference(v, v.Args[0], c.enc.Symbol, guard) {
			c.cmpWord(scratchC, v.Const.Bits)
			guard(amd64.CondNE)
		}
	case ssa.OpTailRoom:
		c.a.Load(scratchA, regCtx, abi.OffLevel)
		c.a.Load(scratchB, regCtx, abi.OffBounds)
		c.a.Load(scratchB, scratchB, abi.OffBoundsTailLevel)
		c.a.Op(amd64.Cmp, scratchA, scratchB, true)
		guard(amd64.CondAE)
	case ssa.OpPropRead:
		c.property(v, c.guardFor(v))
		c.a.Load(scratchB, scratchA, c.enc.NumOffset)
		c.cmpWord(scratchB, abi.NumberLimit)
		guard(amd64.CondAE)
		c.a.MovQToX(xScratch0, scratchB)
		c.setX(v, xScratch0)
	case ssa.OpPropCell:
		if v.Tries() {
			// A megamorphic read's cell: none where it finds no
			// property, for Go to read on a branch of its own.
			none, found := c.a.NewLabel(), c.a.NewLabel()
			c.property(v, func(cond amd64.Cond) { c.a.Jcc(cond, none) })
			c.a.Bind(found)
			c.setG(v, scratchA)
			c.cold = append(c.cold, func() {
				c.a.Bind(none)
				c.movImm(scratchA, 0)
				c.a.Jmp(found)
			})
			break
		}
		c.property(v, c.guardFor(v))
		c.setG(v, scratchA)
	case ssa.OpCellNone:
		r := c.gpr(arg(0), scratchB)
		c.a.Op(amd64.Test, r, r, true)
		c.a.Setcc(amd64.CondE, scratchA)
		c.a.MovZX8(scratchA, scratchA)
		c.setG(v, scratchA)
	case ssa.OpGlobalCell, ssa.OpBindingCell:
		// No script-level lexical binding of the name, which would shadow
		// it: its bit clear, or past the set's words. Then the binding
		// where it was, the name's, and plain, initialized data: in the
		// scope's object, or in the one the VM keeps at Const's address.
		if v.Op == ssa.OpGlobalCell || v.Global {
			unshadowed := c.a.NewLabel()
			word := int32(v.Key >> 6)
			c.a.Load(scratchB, regCtx, abi.OffLexNames)
			c.a.Load(scratchA, scratchB, 8)
			c.a.OpImm(amd64.Cmp, scratchA, word, true)
			c.a.Jcc(amd64.CondBE, unshadowed)
			c.a.Load(scratchB, scratchB, 0)
			c.a.Load(scratchB, scratchB, word*8)
			c.movImm(scratchA, 1<<(v.Key&63))
			c.a.Op(amd64.Test, scratchB, scratchA, true)
			guard(amd64.CondNE)
			c.a.Bind(unshadowed)
		}
		if v.Op == ssa.OpBindingCell {
			c.movImm(scratchA, v.Const.Bits)
		} else {
			c.a.Load(scratchA, regCtx, abi.OffGlobal)
			c.a.Op(amd64.Test, scratchA, scratchA, true)
			guard(amd64.CondE)
		}
		c.keyAt(v, guard)
		c.a.LoadU8(scratchB, scratchB, int32(v.Index)*c.enc.KeySize+c.enc.PropertyFlags)
		c.a.OpImm(amd64.And, scratchB, int32(c.enc.PropNotData|c.enc.PropUninit), false)
		guard(amd64.CondNE)
		c.a.Load(scratchA, scratchA, c.enc.ObjectProps)
		c.a.OpImm(amd64.Add, scratchA, int32(v.Index)*c.enc.ValueSize, true)
		c.setG(v, scratchA)
	case ssa.OpLexCell:
		// The script-level lexical bindings' object, where the global
		// scope is the realm's; then the binding where it was, the
		// name's, and out of its temporal dead zone.
		c.a.Load(scratchA, regCtx, abi.OffGlobalLex)
		c.a.Op(amd64.Test, scratchA, scratchA, true)
		guard(amd64.CondE)
		c.keyAt(v, guard)
		c.a.Load(scratchA, scratchA, c.enc.ObjectProps)
		c.a.OpImm(amd64.Add, scratchA, int32(v.Index)*c.enc.ValueSize, true)
		c.a.Load(scratchB, scratchA, c.enc.NumOffset)
		c.cmpWord(scratchB, c.enc.Uninitialized)
		guard(amd64.CondE)
		c.setG(v, scratchA)
	case ssa.OpStringMethod:
		// A string's charCodeAt where String.prototype has it now, its shape
		// the one Go found the intrinsic in: the call compares what it
		// holds with the intrinsic (stringCode), which a call made
		// meanwhile may have replaced.
		c.cmpWord(c.gpr(arg(0), scratchA), c.enc.String)
		guard(amd64.CondNE)
		c.a.Load(scratchA, regCtx, abi.OffStringProto)
		c.a.Op(amd64.Test, scratchA, scratchA, true)
		guard(amd64.CondE)
		c.a.Load(scratchB, scratchA, c.enc.ObjectShape)
		c.a.Load(scratchC, regCtx, abi.OffStringShape)
		c.a.Op(amd64.Cmp, scratchB, scratchC, true)
		guard(amd64.CondNE)
		c.a.Load(scratchA, scratchA, c.enc.ObjectProps)
		c.a.Load(scratchB, regCtx, abi.OffCharCodeAtOffset)
		c.a.Op(amd64.Add, scratchA, scratchB, true)
		c.setG(v, scratchA)
	case ssa.OpStringProp:
		// A string's method where String.prototype has it, its shape the
		// one the code was compiled for, which says where; or a BigInt's,
		// its number word from HeapBigTag up, where BigInt.prototype has it.
		if h := v.Holders(); h != nil {
			c.cmpWord(c.gpr(arg(0), scratchA), c.enc.HeapBigTag)
			guard(amd64.CondB)
			c.movImm(scratchA, uint64(h.Holders[0].Object))
		} else {
			c.cmpWord(c.gpr(arg(0), scratchA), c.enc.String)
			guard(amd64.CondNE)
			c.a.Load(scratchA, regCtx, abi.OffStringMethods)
			c.a.Op(amd64.Test, scratchA, scratchA, true)
			guard(amd64.CondE)
		}
		c.a.Load(scratchB, scratchA, c.enc.ObjectShape)
		c.cmpWord(scratchB, v.Const.Bits)
		guard(amd64.CondNE)
		c.a.Load(scratchB, scratchA, c.enc.ObjectShape)
		c.a.Load(scratchB, scratchB, c.enc.ShapeKeys)
		c.a.LoadU32(scratchB, scratchB, int32(v.Index)*c.enc.KeySize+c.enc.PropertyKey)
		c.a.OpImm(amd64.Cmp, scratchB, int32(v.Key), false)
		guard(amd64.CondNE)
		c.a.Load(scratchA, scratchA, c.enc.ObjectProps)
		c.a.OpImm(amd64.Add, scratchA, int32(v.Index)*c.enc.ValueSize, true)
		c.setG(v, scratchA)
	case ssa.OpStringCode:
		c.stringCode(v, c.guardFor(v))
	case ssa.OpStringChar:
		c.stringChar(v, c.guardFor(v))
	case ssa.OpCodeChar:
		c.codeChar(v, c.guardFor(v))
	case ssa.OpSubstringChar:
		c.substringChar(v)
	case ssa.OpLoopMark:
		// A loop of a built-in's call under way, or done (abi.Context's
		// Loop).
		c.storeImm(regCtx, abi.OffLoop, v.Const.Bits, scratchA)
	case ssa.OpFreeze:
		c.freeze(v)
	case ssa.OpLoadCell:
		d := c.gdst(v)
		c.a.Load(d, c.gpr(arg(0), scratchA), c.enc.NumOffset)
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
		c.a.MovQToX(xScratch0, scratchB)
		c.a.Bind(done)
		c.cold = append(c.cold, func() {
			c.a.Bind(typed)
			c.typedRead(v, done)
		})
		c.setX(v, xScratch0)
	case ssa.OpElemCell:
		fail, done := c.guardFor(v), c.a.NewLabel()
		if c.enc.CallGo != 0 && c.enc.UndefinedCell != 0 && v.Index != ssa.ElemPresent {
			// An element it does not find Go says is undefined, where it
			// can (ssa.GoElemCell): the read is then of a cell that is --
			// but for a loop's, which a hole leaves (ssa.ElemPresent).
			slow, stub := c.a.NewLabel(), c.stubLabel(v.State, exitKind(v.Aux))
			fail = func(cond amd64.Cond) { c.a.Jcc(cond, slow) }
			c.cold = append(c.cold, func() {
				c.a.Bind(slow)
				size := int32(unsafe.Sizeof(abi.GoArg{}))
				c.a.Store(regCtx, abi.OffGoArgs+c.enc.RefOffset, c.gpr(v.Args[0], scratchC))
				c.a.MovQFromX(scratchA, c.xmm(v.Args[1], xScratch0))
				c.a.Store(regCtx, abi.OffGoArgs+size+c.enc.NumOffset, scratchA)
				c.goSaving(uint64(ssa.GoElemCell), &stub)
				c.movImm(scratchA, c.enc.UndefinedCell)
				c.a.Jmp(done)
			})
		}
		c.index(arg(1), fail)
		c.elementCell(arg(0), fail)
		c.a.Bind(done)
		c.setG(v, scratchA)
	case ssa.OpElemStore:
		c.elemStore(v)
	case ssa.OpElemWrite:
		done, typed := c.a.NewLabel(), c.a.NewLabel()
		slow := c.stubLabel(v.State, exitKind(v.Aux))
		if c.enc.CallGo != 0 && len(v.Args) == 3 {
			// Any other store Go makes if it can (ssa.GoElemWrite): the
			// array, the key and the value in the context's GoArgs. Not
			// one with checks (ssa's storeChecks): a reference live after
			// it read from an element, which Go's store could write over,
			// or move as it grows the array -- the store exits, as
			// elemStore's does.
			stub := slow
			slow = c.a.NewLabel()
			c.cold = append(c.cold, func() {
				c.a.Bind(slow)
				size := int32(unsafe.Sizeof(abi.GoArg{}))
				c.a.Store(regCtx, abi.OffGoArgs+c.enc.RefOffset, c.gpr(v.Args[0], scratchC))
				c.a.MovQFromX(scratchA, c.xmm(v.Args[1], xScratch0))
				c.a.Store(regCtx, abi.OffGoArgs+size+c.enc.NumOffset, scratchA)
				c.a.MovQFromX(scratchA, c.xmm(v.Args[2], xScratch0))
				c.a.Store(regCtx, abi.OffGoArgs+2*size+c.enc.NumOffset, scratchA)
				if c.f.ArrayProtos[0].Object != 0 && c.enc.ValueSize == 16 {
					c.goAppending(&stub)
				} else {
					c.goSaving(uint64(ssa.GoElemWrite), &stub)
				}
				c.a.Jmp(done)
			})
		}
		fail := func(cond amd64.Cond) { c.a.Jcc(cond, slow) }
		c.cold = append(c.cold, func() {
			c.a.Bind(typed)
			c.typedWrite(v, done, slow, func() amd64.XReg { return c.xmm(v.Args[2], xScratch0) })
		})
		c.typedArray(arg(0), typed)
		c.index(arg(1), fail)
		c.element(arg(0), fail)
		c.boxF64(c.xmm(arg(2), xScratch0), scratchB)
		c.a.Store(scratchA, c.enc.NumOffset, scratchB)
		c.a.Bind(done)
	case ssa.OpBoxF64:
		c.boxF64(c.xmm(arg(0), xScratch0), scratchA)
		c.setG(v, scratchA)
	case ssa.OpBoxBool:
		c.boxBool(c.gpr(arg(0), scratchB), scratchA)
		c.setG(v, scratchA)
	case ssa.OpAddF64, ssa.OpSubF64, ssa.OpMulF64, ssa.OpDivF64:
		// SSE's form is d op= y: d is v's own register, unless v is spilled
		// or that register is y's, which d's copy of x would overwrite.
		x, y := c.xmm(arg(0), xScratch0), c.xmm(arg(1), xScratch1)
		d := xScratch0
		if l := c.locAt(v); l.reg >= 0 && (amd64.XReg(l.reg) != y || y == x) {
			d = amd64.XReg(l.reg)
		}
		if x != d {
			c.a.SSEOp(amd64.MovAPD, d, x)
		}
		c.a.SSEOp(sseOp(v.Op), d, y)
		c.setX(v, d)
	case ssa.OpModF64:
		c.remainder(v, c.guardFor(v))
	case ssa.OpPowF64:
		c.power(v, c.guardFor(v))
	case ssa.OpMaxF64, ssa.OpMinF64:
		c.extremum(v)
	case ssa.OpFloorF64, ssa.OpCeilF64, ssa.OpTruncF64:
		if !c.enc.Round {
			panic("rounding without SSE4.1")
		}
		mode := uint8(amd64.RoundFloor)
		if v.Op == ssa.OpCeilF64 {
			mode = amd64.RoundCeil
		} else if v.Op == ssa.OpTruncF64 {
			mode = amd64.RoundTrunc
		}
		c.a.RoundSD(xScratch0, c.xmm(arg(0), xScratch0), mode)
		c.setX(v, xScratch0)
	case ssa.OpRoundF64:
		c.round(v)
	case ssa.OpSignF64:
		c.sign(v)
	case ssa.OpFroundF64:
		c.a.Cvtsd2ss(xScratch0, c.xmm(arg(0), xScratch0))
		c.a.Cvtss2sd(xScratch0, xScratch0)
		c.setX(v, xScratch0)
	case ssa.OpMathCall:
		c.mathCall(v)
	case ssa.OpRandomF64:
		c.random(v)
	case ssa.OpNegF64:
		x := c.xmm(arg(0), xScratch0)
		if x != xScratch0 {
			c.a.SSEOp(amd64.MovAPD, xScratch0, x)
		}
		c.movImm(scratchA, 1<<63)
		c.a.MovQToX(xScratch1, scratchA)
		c.a.SSEOp(amd64.XorPD, xScratch0, xScratch1)
		c.setX(v, xScratch0)
	case ssa.OpSqrtF64:
		c.a.SSEOp(amd64.SqrtSD, xScratch0, c.xmm(arg(0), xScratch0))
		c.setX(v, xScratch0)
	case ssa.OpAbsF64:
		x := c.xmm(arg(0), xScratch0)
		if x != xScratch0 {
			c.a.SSEOp(amd64.MovAPD, xScratch0, x)
		}
		c.movImm(scratchA, 1<<63-1)
		c.a.MovQToX(xScratch1, scratchA)
		c.a.SSEOp(amd64.AndPD, xScratch0, xScratch1)
		c.setX(v, xScratch0)
	case ssa.OpCmpF64:
		if v.Uses == 1 && b.Control == v && b.Kind == ssa.BlockIf {
			return // fused into the branch
		}
		c.compare(v)
	case ssa.OpStrictNullish:
		word := c.enc.Null
		if v.Aux == 1 {
			word = c.enc.Undefined
		}
		w := c.gpr(arg(0), scratchB)
		c.movImm(scratchA, word)
		c.a.Op(amd64.Cmp, w, scratchA, true)
		c.a.Setcc(amd64.CondE, scratchA)
		if v.Aux == 2 {
			// Null, or undefined.
			c.movImm(scratchC, c.enc.Undefined)
			c.a.Op(amd64.Cmp, w, scratchC, true)
			c.a.Setcc(amd64.CondE, scratchC)
			c.a.Op(amd64.Or, scratchA, scratchC, false)
		}
		c.a.MovZX8(scratchA, scratchA)
		c.setG(v, scratchA)
	case ssa.OpLooseNullish:
		c.looseNullish(v, c.guardFor(v))
	case ssa.OpEqTagged:
		c.eqTagged(v, c.guardFor(v))
	case ssa.OpNot:
		c.a.MovRR32(scratchA, c.gpr(arg(0), scratchA))
		c.a.OpImm(amd64.Xor, scratchA, 1, false)
		c.setG(v, scratchA)
	case ssa.OpToInt32:
		c.toInt32(v)
	case ssa.OpAndI32, ssa.OpOrI32, ssa.OpXorI32:
		op := aluOp(v.Op)
		c.a.MovRR32(scratchA, c.gpr(arg(0), scratchA))
		c.a.Op(op, scratchA, c.gpr(arg(1), scratchB), false)
		c.setG(v, scratchA)
	case ssa.OpShlI32, ssa.OpSarI32, ssa.OpShrU32:
		op := shiftOp(v.Op)
		c.a.MovRR32(scratchC, c.gpr(arg(1), scratchC))
		c.a.MovRR32(scratchA, c.gpr(arg(0), scratchA))
		c.a.ShiftCL(op, scratchA, false)
		c.setG(v, scratchA)
	case ssa.OpNotI32:
		c.a.MovRR32(scratchA, c.gpr(arg(0), scratchA))
		c.a.Not32(scratchA)
		c.setG(v, scratchA)
	case ssa.OpI32ToF64:
		c.a.Cvtsi2sd(xScratch0, c.gpr(arg(0), scratchA), false)
		c.setX(v, xScratch0)
	case ssa.OpBoolToI32, ssa.OpBoolToF64:
		// Any word but 0 is true (boxBool): 1 or 0.
		b := c.gpr(arg(0), scratchA)
		c.a.Op(amd64.Test, b, b, false)
		c.a.Setcc(amd64.CondNE, scratchA)
		c.a.MovZX8(scratchA, scratchA)
		if v.Op == ssa.OpBoolToI32 {
			c.setG(v, scratchA)
			break
		}
		c.a.Cvtsi2sd(xScratch0, scratchA, false)
		c.setX(v, xScratch0)
	case ssa.OpU32ToF64:
		c.a.MovRR32(scratchA, c.gpr(arg(0), scratchA))
		c.a.Cvtsi2sd(xScratch0, scratchA, true)
		c.setX(v, xScratch0)
	default:
		panic("value " + v.Op.String())
	}
}

// compare materializes a float comparison as 0 or 1.
func (c *compiler) compare(v *ssa.Value) {
	x, y := c.xmm(v.Args[0], xScratch0), c.xmm(v.Args[1], xScratch1)
	switch ir.Operator(v.Aux) {
	case ir.Lt:
		c.a.SSEOp(amd64.UcomiSD, y, x)
		c.a.Setcc(amd64.CondA, scratchA)
	case ir.Le:
		c.a.SSEOp(amd64.UcomiSD, y, x)
		c.a.Setcc(amd64.CondAE, scratchA)
	case ir.Gt:
		c.a.SSEOp(amd64.UcomiSD, x, y)
		c.a.Setcc(amd64.CondA, scratchA)
	case ir.Ge:
		c.a.SSEOp(amd64.UcomiSD, x, y)
		c.a.Setcc(amd64.CondAE, scratchA)
	case ir.Eq:
		c.a.SSEOp(amd64.UcomiSD, x, y)
		c.a.Setcc(amd64.CondE, scratchA)
		c.a.Setcc(amd64.CondNP, scratchC)
		c.a.Op(amd64.And, scratchA, scratchC, false)
	case ir.Ne:
		c.a.SSEOp(amd64.UcomiSD, x, y)
		c.a.Setcc(amd64.CondNE, scratchA)
		c.a.Setcc(amd64.CondP, scratchC)
		c.a.Op(amd64.Or, scratchA, scratchC, false)
	}
	c.a.MovZX8(scratchA, scratchA)
	c.setG(v, scratchA)
}

// looseNullish is x == null: x's word is null's or undefined's, or x is an
// object with [[IsHTMLDDA]], found through its origin as objectOf finds it.
func (c *compiler) looseNullish(v *ssa.Value, guard func(amd64.Cond)) {
	a := v.Args[0]
	yes, no, done := c.a.NewLabel(), c.a.NewLabel(), c.a.NewLabel()
	w := c.gpr(a, scratchB)
	for _, word := range []uint64{c.enc.Null, c.enc.Undefined} {
		c.cmpWord(w, word)
		c.a.Jcc(amd64.CondE, yes)
	}
	c.cmpWord(w, c.enc.Object)
	c.a.Jcc(amd64.CondNE, no)
	if c.reference(v, a, c.enc.Object, guard) {
		c.a.LoadU8(scratchA, scratchC, c.enc.ObjectFlags)
		c.a.OpImm(amd64.And, scratchA, int32(c.enc.FlagHTMLDDA), false)
		c.a.Jcc(amd64.CondNE, yes)
	}
	c.a.Bind(no)
	c.movImm(scratchA, 0)
	c.a.Jmp(done)
	c.a.Bind(yes)
	c.movImm(scratchA, 1)
	c.a.Bind(done)
	c.setG(v, scratchA)
}

// eqTagged is OpEqTagged: x == y or x === y by the two words, and for two
// objects by their pointers, found as objectOf finds an object's. What
// most comparisons meet is inline -- one word, a number or an object; two
// words, numbers or not -- and the rest out of line: two strings, the
// constants a word may be, loose equality's null and undefined. All of it
// inline had made the comparison 500 bytes a site, of the code's hottest,
// whose size decides how much of it the op cache holds.
func (c *compiler) eqTagged(v *ssa.Value, guard func(amd64.Cond)) {
	x, y := v.Args[0], v.Args[1]
	yes, no, done, differ, number := c.a.NewLabel(), c.a.NewLabel(), c.a.NewLabel(), c.a.NewLabel(), c.a.NewLabel()
	exit := c.stubLabel(v.State, exitKind(v.Aux))
	if c.enc.CallGo != 0 && exitKind(v.Aux) == abi.ExitHost {
		// What native code does not compare Go does (ssa.GoEqual), out
		// of line; what it refuses leaves.
		stub := exit
		exit = c.a.NewLabel()
		c.cold = append(c.cold, func() {
			c.a.Bind(exit)
			size := int32(unsafe.Sizeof(abi.GoArg{}))
			c.goArg(abi.OffGoArgs, x)
			c.goArg(abi.OffGoArgs+size, y)
			c.goSaving(uint64(ssa.GoEqual)|uint64(v.Index&1)<<abi.GoOpIndexShift, &stub)
			c.a.Load(scratchA, regCtx, abi.OffGoArgs+c.enc.NumOffset)
			c.a.Op(amd64.Test, scratchA, scratchA, true)
			c.a.Jcc(amd64.CondNE, yes)
			c.a.Jmp(no)
		})
	}
	wx, wy := c.gpr(x, scratchA), c.gpr(y, scratchB)
	c.a.Op(amd64.Cmp, wx, wy, true)
	c.a.Jcc(amd64.CondNE, differ)
	// One word: a number (NaN unequal to itself), objects, or, out of
	// line, a primitive or strings.
	c.cmpWord(wx, abi.NumberLimit)
	c.a.Jcc(amd64.CondB, number)
	notObject := c.a.NewLabel()
	c.movImm(scratchC, c.enc.Object)
	c.a.Op(amd64.Cmp, wx, scratchC, true)
	c.a.Jcc(amd64.CondNE, notObject)
	if c.reference(v, x, c.enc.Object, guard) {
		c.a.MovQToX(xScratch1, scratchC)
		if c.reference(v, y, c.enc.Object, guard) {
			c.a.MovQFromX(scratchA, xScratch1)
			c.a.Op(amd64.Cmp, scratchA, scratchC, true)
			c.a.Jcc(amd64.CondE, yes)
			c.a.Jmp(no)
		}
	}
	// Two strings, inline as far as one string, which an interned
	// property name or literal compared with itself is; their lengths and
	// units out of line, with every other word's constants.
	c.a.Bind(notObject)
	for _, w := range []uint64{c.enc.Null, c.enc.Undefined} {
		// x === undefined, which most tests of a value against one are.
		c.cmpWord(wx, w)
		c.a.Jcc(amd64.CondE, yes)
	}
	notString, units := c.a.NewLabel(), c.a.NewLabel()
	c.movImm(scratchC, c.enc.String)
	c.a.Op(amd64.Cmp, wx, scratchC, true)
	c.a.Jcc(amd64.CondNE, notString)
	if c.reference(v, x, c.enc.String, guard) {
		c.a.MovQToX(xScratch1, scratchC)
		if c.reference(v, y, c.enc.String, guard) {
			c.a.MovQFromX(scratchA, xScratch1)
			c.a.Op(amd64.Cmp, scratchA, scratchC, true)
			c.a.Jcc(amd64.CondE, yes)
			c.a.Jmp(units)
			c.cold = append(c.cold, func() {
				c.a.Bind(units)
				c.a.Load(scratchA, scratchA, c.enc.StringLength)
				c.a.Load(scratchB, scratchC, c.enc.StringLength)
				c.a.Op(amd64.Cmp, scratchA, scratchB, true)
				c.a.Jcc(amd64.CondNE, no)
				c.stringBytes(exit, yes, no)
			})
		}
	}
	c.cold = append(c.cold, func() {
		c.a.Bind(notString)
		wx := c.gpr(x, scratchA)
		// A loop's step's answer compared with the uninitialized value, a
		// cursor done (ssa's iterStep), is one word too.
		for _, w := range []uint64{c.enc.True, c.enc.False, c.enc.Uninitialized} {
			c.cmpWord(wx, w)
			c.a.Jcc(amd64.CondE, yes)
		}
		c.a.Jmp(exit)
	})
	c.a.Bind(number)
	c.a.MovQToX(xScratch0, wx)
	c.a.SSEOp(amd64.UcomiSD, xScratch0, xScratch0)
	c.a.Jcc(amd64.CondP, no)
	c.a.Jmp(yes)
	// Two words: two numbers compare as numbers; anything else is
	// strictly unequal. Loosely, null and undefined equal each other and
	// an object with [[IsHTMLDDA]] alone, with nothing converted, and two
	// booleans are as strictly: x == false of a boolean x, which had left
	// for Go each time x was true; any other two are Go's -- out of line.
	c.a.Bind(differ)
	other := no
	if v.Index != 1 {
		loose := c.a.NewLabel()
		other = loose
		c.cold = append(c.cold, func() {
			c.a.Bind(loose)
			wx, wy := c.gpr(x, scratchA), c.gpr(y, scratchB)
			xNullish, yNullish := c.a.NewLabel(), c.a.NewLabel()
			for _, w := range []uint64{c.enc.Null, c.enc.Undefined} {
				c.movImm(scratchC, w)
				c.a.Op(amd64.Cmp, wx, scratchC, true)
				c.a.Jcc(amd64.CondE, xNullish)
				c.a.Op(amd64.Cmp, wy, scratchC, true)
				c.a.Jcc(amd64.CondE, yNullish)
			}
			xBool := c.a.NewLabel()
			for _, w := range []uint64{c.enc.True, c.enc.False} {
				c.cmpWord(wx, w)
				c.a.Jcc(amd64.CondE, xBool)
			}
			c.a.Jmp(exit)
			// x a boolean, and y the other boolean -- the words differ --
			// or what converts.
			c.a.Bind(xBool)
			for _, w := range []uint64{c.enc.True, c.enc.False} {
				c.cmpWord(wy, w)
				c.a.Jcc(amd64.CondE, no)
			}
			c.a.Jmp(exit)
			// One is null or undefined, the other of another word.
			for _, k := range []struct {
				at    amd64.Label
				other *ssa.Value
				w     amd64.Reg
			}{{xNullish, y, wy}, {yNullish, x, wx}} {
				c.a.Bind(k.at)
				for _, w := range []uint64{c.enc.Null, c.enc.Undefined} {
					c.cmpWord(k.w, w)
					c.a.Jcc(amd64.CondE, yes)
				}
				c.cmpWord(k.w, c.enc.Object)
				c.a.Jcc(amd64.CondNE, no)
				if c.reference(v, k.other, c.enc.Object, guard) {
					c.a.LoadU8(scratchC, scratchC, c.enc.ObjectFlags)
					c.a.OpImm(amd64.And, scratchC, int32(c.enc.FlagHTMLDDA), false)
					c.a.Jcc(amd64.CondNE, yes)
				}
				c.a.Jmp(no)
			}
		})
	}
	c.movImm(scratchC, abi.NumberLimit)
	c.a.Op(amd64.Cmp, wx, scratchC, true)
	c.a.Jcc(amd64.CondAE, other)
	c.a.Op(amd64.Cmp, wy, scratchC, true)
	c.a.Jcc(amd64.CondAE, other)
	c.a.MovQToX(xScratch0, wx)
	c.a.MovQToX(xScratch1, wy)
	c.a.SSEOp(amd64.UcomiSD, xScratch0, xScratch1)
	c.a.Jcc(amd64.CondNE, no)
	c.a.Jcc(amd64.CondP, no)
	c.a.Bind(yes)
	c.movImm(scratchA, 1)
	c.a.Jmp(done)
	c.a.Bind(no)
	c.movImm(scratchA, 0)
	c.a.Bind(done)
	c.setG(v, scratchA)
}

// truth computes JavaScript truthiness for the kinds the slot IR decides it
// for, exiting for the rest: a number, an object, a boolean and null inline,
// as most tests meet them, the rest out of line.
func (c *compiler) truth(v *ssa.Value, guard func(amd64.Cond)) {
	r := c.gpr(v.Args[0], scratchB)
	number, yes, no, done := c.a.NewLabel(), c.a.NewLabel(), c.a.NewLabel(), c.a.NewLabel()
	c.a.MovRR(scratchA, r)
	c.a.ShiftImm(amd64.Shr, scratchA, 51, true)
	c.a.OpImm(amd64.Cmp, scratchA, 0x1FFF, false)
	c.a.Jcc(amd64.CondNE, number)
	// A boolean, which most tests meet that are not of numbers; an object,
	// true unless it is Annex B's [[IsHTMLDDA]], through its pointer, where
	// it came from; null.
	for _, w := range []struct {
		word uint64
		to   amd64.Label
	}{{c.enc.True, yes}, {c.enc.False, no}} {
		c.cmpWord(r, w.word)
		c.a.Jcc(amd64.CondE, w.to)
	}
	notObject := c.a.NewLabel()
	c.cmpWord(r, c.enc.Object)
	c.a.Jcc(amd64.CondNE, notObject)
	if c.reference(v, v.Args[0], c.enc.Object, guard) {
		c.a.LoadU8(scratchA, scratchC, c.enc.ObjectFlags)
		c.a.OpImm(amd64.And, scratchA, int32(c.enc.FlagHTMLDDA), false)
		c.a.Jcc(amd64.CondNE, no)
		c.a.Jmp(yes)
	}
	c.a.Bind(notObject)
	other := c.a.NewLabel()
	c.cmpWord(r, c.enc.Null)
	c.a.Jcc(amd64.CondE, no)
	c.a.Jmp(other)
	// Undefined; a BigInt, true unless it is zero, which is held in its
	// value, in the int64 range's encoding, its high bits' word
	// ShortBigHigh, as a loop's bit test, if (e & 1n), meets it; and a
	// string, true unless it is empty. Anything else -- the uninitialized
	// marker, a symbol -- is not decided here.
	c.cold = append(c.cold, func() {
		c.a.Bind(other)
		r := c.gpr(v.Args[0], scratchB)
		c.cmpWord(r, c.enc.Undefined)
		c.a.Jcc(amd64.CondE, no)
		if c.enc.HeapBigTag != 0 {
			notBig := c.a.NewLabel()
			c.cmpWord(r, c.enc.HeapBigTag)
			c.a.Jcc(amd64.CondE, yes)
			c.movImm(scratchA, c.enc.ShortBigWideTag)
			c.a.Op(amd64.Cmp, r, scratchA, true)
			c.a.Jcc(amd64.CondB, notBig)
			c.cmpWord(r, c.enc.ShortBigTag)
			c.a.Jcc(amd64.CondNE, yes)
			if c.reference(v, v.Args[0], c.enc.ShortBigTag, guard) {
				c.cmpWord(scratchC, c.enc.ShortBigHigh)
				c.a.Jcc(amd64.CondE, no)
				c.a.Jmp(yes)
			}
			c.a.Bind(notBig)
		}
		c.cmpWord(r, c.enc.String)
		guard(amd64.CondNE)
		if c.reference(v, v.Args[0], c.enc.String, guard) {
			c.a.Load(scratchA, scratchC, c.enc.StringLength)
			c.a.Op(amd64.Test, scratchA, scratchA, true)
			c.a.Jcc(amd64.CondNE, yes)
			c.a.Jmp(no)
		}
	})
	c.a.Bind(number)
	// A number is true unless zero or NaN: ucomisd sets ZF for zero and for
	// NaN, and PF only for NaN.
	c.a.MovQToX(xScratch0, r)
	c.a.SSEOp(amd64.XorPD, xScratch1, xScratch1)
	c.a.SSEOp(amd64.UcomiSD, xScratch0, xScratch1)
	c.a.Setcc(amd64.CondNE, scratchA)
	c.a.Setcc(amd64.CondNP, scratchC)
	c.a.Op(amd64.And, scratchA, scratchC, false)
	c.a.MovZX8(scratchA, scratchA)
	c.a.Jmp(done)
	c.a.Bind(yes)
	c.movImm(scratchA, 1)
	c.a.Jmp(done)
	c.a.Bind(no)
	c.movImm(scratchA, 0)
	c.a.Bind(done)
	c.setG(v, scratchA)
}

// toInt32 is ToUint32's bits: CVTTSD2SI covers |x| < 2^63; beyond, and for
// NaN and the infinities, a cold path works from the exponent and
// significand.
func (c *compiler) toInt32(v *ssa.Value) {
	xv := v.Args[0]
	slow, back := c.a.NewLabel(), c.a.NewLabel()
	x := c.xmm(xv, xScratch0)
	c.a.Cvttsd2si(scratchA, x)
	c.cmpWord(scratchA, 1<<63)
	c.a.Jcc(amd64.CondE, slow)
	c.a.Bind(back)
	c.a.MovRR32(scratchA, scratchA)
	c.setG(v, scratchA)
	c.cold = append(c.cold, func() {
		zero, positive := c.a.NewLabel(), c.a.NewLabel()
		c.a.Bind(slow)
		c.a.MovQFromX(scratchA, c.xmm(xv, xScratch0))
		// e = exponent; NaN and the infinities give 0.
		c.a.MovRR(scratchC, scratchA)
		c.a.ShiftImm(amd64.Shr, scratchC, 52, true)
		c.a.OpImm(amd64.And, scratchC, 0x7FF, false)
		c.a.OpImm(amd64.Cmp, scratchC, 0x7FF, false)
		c.a.Jcc(amd64.CondE, zero)
		// |x| >= 2^63 here, so e = exponent - 1075 >= 11, and the low 32
		// bits of the integer are the significand's low bits shifted by e,
		// zero once e >= 32; the implicit leading bit lands past bit 62.
		c.a.OpImm(amd64.Sub, scratchC, 1075, false)
		c.a.OpImm(amd64.Cmp, scratchC, 32, false)
		c.a.Jcc(amd64.CondAE, zero)
		c.a.MovRR(scratchB, scratchA) // the sign is bit 63
		c.a.ShiftImm(amd64.Shl, scratchA, 12, true)
		c.a.ShiftImm(amd64.Shr, scratchA, 12, true)
		c.a.ShiftCL(amd64.Shl, scratchA, true)
		c.a.Op(amd64.Test, scratchB, scratchB, true)
		c.a.Jcc(amd64.CondNS, positive)
		c.a.MovRR32(scratchC, scratchA)
		c.a.Op(amd64.Xor, scratchA, scratchA, false)
		c.a.Op(amd64.Sub, scratchA, scratchC, false)
		c.a.Bind(positive)
		c.a.Jmp(back)
		c.a.Bind(zero)
		c.a.Op(amd64.Xor, scratchA, scratchA, false)
		c.a.Jmp(back)
	})
}

// sseOp, aluOp and shiftOp are the instructions for SSA's arithmetic,
// bitwise and shift operations.
func sseOp(op ssa.Op) amd64.SSE {
	switch op {
	case ssa.OpAddF64:
		return amd64.AddSD
	case ssa.OpSubF64:
		return amd64.SubSD
	case ssa.OpMulF64:
		return amd64.MulSD
	}
	return amd64.DivSD
}

func aluOp(op ssa.Op) amd64.ALU {
	switch op {
	case ssa.OpAndI32:
		return amd64.And
	case ssa.OpOrI32:
		return amd64.Or
	}
	return amd64.Xor
}

func shiftOp(op ssa.Op) amd64.Shift {
	switch op {
	case ssa.OpShlI32:
		return amd64.Shl
	case ssa.OpSarI32:
		return amd64.Sar
	}
	return amd64.Shr
}
