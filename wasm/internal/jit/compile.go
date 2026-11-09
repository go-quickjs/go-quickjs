//go:build amd64 || arm64

package jit

import (
	"cmp"
	"slices"
	"unsafe"

	"github.com/go-quickjs/go-quickjs/wasm/internal/module"
	"github.com/go-quickjs/go-quickjs/wasm/internal/opcode"
)

// This file and stack.go are the compiler's front end, which every backend
// shares: the constructs, the branches, the locals, the operand stack's
// entries and calls through the engine. Each backend (compile_GOARCH.go,
// stack_GOARCH.go, ops_GOARCH.go) gives the assembler and the register
// types their names here (asm, asmLabel, reg, freg, condCode, regMask) and
// emits the instructions: the entries, exits, traps, native calls, the
// numeric and memory instructions, and the moves between registers, slots
// and constants.

// MaxCodeBytes is the most code the engine has a function compiled to: a
// function past it is refused (size) and runs in the interpreter.
const MaxCodeBytes = 1 << 20

// Request is a function to compile.
type Request struct {
	Module *module.Module
	Func   uint32
	// FrameSlots is the number of numeric slots of the function's frame,
	// as the engine makes it.
	FrameSlots int
	// FrameSlotsOf returns FrameSlots of another defined function of the
	// module, for its frame made by a native call.
	FrameSlotsOf func(fn uint32) int
	// MaxCodeBytes bounds the code; 0 is no bound.
	MaxCodeBytes int
	// FrameRefSlots is the number of reference slots of the function's
	// frame native code uses: the engine's, or 0 for a function that holds
	// no reference, whose code neither reads nor writes them. RefSlotsOf
	// returns another function's.
	FrameRefSlots int
	RefSlotsOf    func(fn uint32) int
	// FrameLocals and FrameRefLocals are the engine's numbers of numeric
	// and reference slots before the operand stack's, which the compiler's
	// must be.
	FrameLocals, FrameRefLocals int
	// WriteBarrier is the address of the byte that is not zero while the
	// collector marks, and StoreRef where native code jumps to store a
	// pointer word then (internal/jit's).
	WriteBarrier, StoreRef uint64
	// Layout is where the engine's objects keep what native code reads.
	Layout *Layout
	// CaughtSlot is the first of the reference slots the engine's frame
	// has past its own for the exceptions legacy catches caught, one for
	// each depth of legacy trys.
	CaughtSlot int
	// TierCounter, if not 0, is the address of the function's countdown
	// to the optimizing tier, an int64: the baseline code decrements it at
	// its entry and exits (ExitTierUp) when it goes below 0. amd64 only.
	TierCounter uint64
	// Marks asks the compiler for its code's marks and listing (Code.Marks,
	// Code.Listing), which a profile's samples in the code are read by.
	Marks bool
}

// Mark says what the code from offset Off, up to the next mark, was
// emitted for.
type Mark struct {
	Off int
	Tag string
}

// Layout is where the engine's objects keep what native code reads of
// them, by byte offset from the object's start.
type Layout struct {
	// TableElems is a table's elements: their address, then their number.
	TableElems int32
	// I31Mark is the pointer word of every i31.
	I31Mark uint64
	// An object begins with a header: Kind is its kind's byte, KindStruct
	// and KindArray the values that say a struct and an array; RTT the
	// pointer to its type's runtime information, nil for an object of no
	// defined type, whose Supers are the canonical indices, 4 bytes each,
	// of the type's supertypes by depth, its own last: their address, then
	// their number.
	Kind                  int32
	KindStruct, KindArray uint8
	RTT, Supers           int32
	// An array's ArrayLen is its length, a word; ArrayBytes the address
	// of its number elements, packed; ArrayRefs its reference elements:
	// their address, then their number.
	ArrayLen, ArrayBytes, ArrayRefs int32
}

// Code is a function's native code. Its offsets are from its start.
type Code struct {
	Bytes []byte
	// GoEntry is where the engine enters the function, Entry where native
	// callers do, and ReturnStub the code that returns to the engine,
	// which the engine gives as the return address of the frame it enters.
	GoEntry, Entry, ReturnStub int
	// Handlers are the try_tables' catch clauses, for the engine to find
	// an exception's landing pad by.
	Handlers []Handler
	// Loops are the entries at the loops' heads, by the offset of each
	// loop instruction in the module: entered from Go as GoEntry is, with
	// the frame as it is at the loop's head -- every operand stack entry in
	// its position's slot, every local in its own -- they load the
	// registers the code keeps and the locals' homes and go to the loop's
	// head. The code from there runs the function to its return.
	Loops map[uint32]int
	// Marks and Listing, where the request asked for them, are what each
	// stretch of the code was emitted for, in order of their offsets, and
	// the compiler's view of the function the marks name.
	Marks   []Mark
	Listing string
}

// The context's fields' offsets.
var (
	offL          = int32(unsafe.Offsetof(Ctx{}.L))
	offLinks      = int32(unsafe.Offsetof(Ctx{}.Links))
	offMemBase    = int32(unsafe.Offsetof(Ctx{}.MemBase))
	offMemSize    = int32(unsafe.Offsetof(Ctx{}.MemSize))
	offMemSizePtr = int32(unsafe.Offsetof(Ctx{}.MemSizePtr))
	offMems       = int32(unsafe.Offsetof(Ctx{}.Mems))
	offGlobals    = int32(unsafe.Offsetof(Ctx{}.Globals))
	offChunkEnd   = int32(unsafe.Offsetof(Ctx{}.ChunkEnd))
	offRSP        = int32(unsafe.Offsetof(Ctx{}.RSP))
	offRSEnd      = int32(unsafe.Offsetof(Ctx{}.RSEnd))
	offPoll       = int32(unsafe.Offsetof(Ctx{}.Poll))
	offR          = int32(unsafe.Offsetof(Ctx{}.R))
	offRChunkEnd  = int32(unsafe.Offsetof(Ctx{}.RChunkEnd))
	offFuncs      = int32(unsafe.Offsetof(Ctx{}.Funcs))
	offTables     = int32(unsafe.Offsetof(Ctx{}.Tables))
	offWBSlot     = int32(unsafe.Offsetof(Ctx{}.WBSlot))
	offWBPtr      = int32(unsafe.Offsetof(Ctx{}.WBPtr))
	offWBRet      = int32(unsafe.Offsetof(Ctx{}.WBRet))
	offSave       = int32(unsafe.Offsetof(Ctx{}.Save))
	offExit       = int32(unsafe.Offsetof(Ctx{}.Exit))
	offArg0       = int32(unsafe.Offsetof(Ctx{}.Arg0))
	offExitFn     = int32(unsafe.Offsetof(Ctx{}.ExitFn))
	offExitAddr   = int32(unsafe.Offsetof(Ctx{}.ExitAddr))
)

// Available reports whether this build compiles native code.
const Available = true

// ctl is a construct being compiled.
type ctl struct {
	op              opcode.Opcode
	base            int
	params, results []module.ValueType
	// label is where a branch to the construct goes: a loop's head, the
	// end of anything else, the return sequence for the function's body.
	label asmLabel
	// elseL is an if's else branch.
	elseL   asmLabel
	hasElse bool
	// dead says the rest of the construct cannot be reached.
	dead bool
	// poll is a loop's poll exit, made with its first back edge.
	poll    asmLabel
	hasPoll bool
	// start is a try_table's or a legacy try's body's first instruction's
	// offset, index its depth in the function's constructs, clauses its
	// catch clauses; tryEnd is a legacy try's first catch's offset, which
	// hasCatch says it has.
	start    uint32
	index    int
	clauses  []catchCtl
	tryEnd   uint32
	hasCatch bool
}

func (t *ctl) labelTypes() []module.ValueType {
	if t.op == opcode.Loop {
		return t.params
	}
	return t.results
}

type trapKey struct {
	code uint64
	at   uint32
}

type compiler struct {
	a   *asm
	req *Request
	m   *module.Module
	fn  uint32
	r   reader
	sig *module.FuncType

	locals []module.ValueType
	use    localUse
	// home is each local's register, or -1 for its slot.
	home []int8
	// slots is each local's slot, numeric or reference, and results each
	// result's. nlocals and rlocals are the numeric and reference slots
	// before the operand stack's; numParams and numLocals the numeric
	// slots of the parameters and of every local; refParams the reference
	// slots of the parameters.
	slots, results       []slotPos
	nlocals, rlocals     int
	numParams, numLocals int
	refParams            int
	frameSlots           int
	maxHeight            int
	// refs says the function holds references: R, its reference slots, is
	// in rR, which the cache never hands out, and a native call moves it
	// past the frame, adv bytes.
	refs bool
	adv  int32
	// handlers are the try_tables ended so far.
	handlers []handlerCtl
	// wb is the function's barrier routine, made with its first store of a
	// pointer word (wbRoutine).
	wb    asmLabel
	hasWB bool

	stack []value
	ctrl  []*ctl
	gused regMask
	xused regMask

	retL asmLabel
	ool  []func()
	// loops are the loop entries made so far, by the loop's offset.
	loops  map[uint32]asmLabel
	traps  map[trapKey]asmLabel
	trapTo map[uint64]asmLabel
	// vconsts are the v128 constants the code loads, by their bits, in the
	// order first used.
	vconsts     map[[2]uint64]asmLabel
	vconstOrder [][2]uint64
}

// Compile compiles function fn of a validated module, or refuses it with
// a *Refusal.
func Compile(req *Request) (code *Code, err error) {
	defer func() {
		if p := recover(); p != nil {
			if r, ok := p.(*Refusal); ok {
				code, err = nil, r
				return
			}
			panic(p)
		}
	}()
	m := req.Module
	c := &compiler{a: &asm{}, req: req, m: m, fn: req.Func, sig: m.FuncType(req.Func),
		frameSlots: req.FrameSlots, traps: map[trapKey]asmLabel{}, trapTo: map[uint64]asmLabel{}, loops: map[uint32]asmLabel{}}
	c.locals, c.use = scan(m, req.Func, CPU)
	c.layout()
	if c.nlocals != req.FrameLocals || c.rlocals != req.FrameRefLocals {
		panic("wasm jit: the frame's locals are not the engine's")
	}
	c.chooseHomes()
	f := &m.Functions[req.Func]
	c.r = reader{b: m.Bytes, pc: int(f.Code.Offset)}
	for n := c.r.u32(); n > 0; n-- {
		c.r.u32()
		c.r.valueType()
	}
	end := int(f.Code.End())
	a := c.a

	// The return to the engine, at the start, then the engine's entry and
	// the natives'.
	retStub, goEntry, entry := c.entries()
	c.prologue()

	c.retL = a.NewLabel()
	c.ctrl = append(c.ctrl, &ctl{op: opcode.Block, results: c.sig.Results, label: c.retL})
	for c.r.pc < end && len(c.ctrl) > 0 {
		c.instr()
	}
	c.returnSequence()
	for i := 0; i < len(c.ool); i++ {
		c.ool[i]()
	}
	c.emitVConsts()
	if 2*c.maxHeight+c.nlocals > c.frameSlots {
		refuse(RefuseFrame, "%d stack positions and %d locals in a frame of %d slots", c.maxHeight, c.nlocals, c.frameSlots)
	}
	if c.refs && c.maxHeight+c.rlocals > req.FrameRefSlots {
		refuse(RefuseFrame, "%d stack positions and %d reference locals in a frame of %d reference slots", c.maxHeight, c.rlocals, req.FrameRefSlots)
	}
	out, ferr := a.Finish()
	if ferr != nil {
		// A branch out of its range is code too large.
		if req.MaxCodeBytes > 0 && a.Len() > req.MaxCodeBytes {
			refuse(RefuseSize, "%d bytes of code", a.Len())
		}
		panic(ferr)
	}
	if req.MaxCodeBytes > 0 && len(out) > req.MaxCodeBytes {
		refuse(RefuseSize, "%d bytes of code", len(out))
	}
	code = &Code{Bytes: out, GoEntry: a.Offset(goEntry), Entry: a.Offset(entry), ReturnStub: retStub, Handlers: c.handlersOut()}
	if len(c.loops) > 0 {
		code.Loops = make(map[uint32]int, len(c.loops))
		for at, l := range c.loops {
			code.Loops[at] = a.Offset(l)
		}
	}
	return code, nil
}

// chooseHomes gives the most used locals registers of their own, for the
// whole function: the integers' general registers, the floats' float ones.
// A home is taken from the cache.
func (c *compiler) chooseHomes() {
	c.home = make([]int8, len(c.locals))
	order := make([]int, 0, len(c.locals))
	for k := range c.locals {
		c.home[k] = -1
		if c.use.weight[k] > 0 {
			order = append(order, k)
		}
	}
	slices.SortStableFunc(order, func(x, y int) int { return cmp.Compare(c.use.weight[y], c.use.weight[x]) })
	ng, nx := 0, 0
	for _, k := range order {
		if c.locals[k].IsRef() {
			continue
		}
		if isFloat(c.locals[k].Kind) {
			if nx < len(homeXRegs) {
				c.home[k] = int8(homeXRegs[nx])
				c.xused |= 1 << homeXRegs[nx]
				nx++
			}
		} else if ng < len(homeGPRs) && !(c.refs && ng >= refHomeGPRs) {
			c.home[k] = int8(homeGPRs[ng])
			c.gused |= 1 << homeGPRs[ng]
			ng++
		}
	}
}

// saveHomes stores the locals living in registers that the function ever
// writes into their slots, before a call or an exit loses the registers;
// the others' slots hold their values already.
func (c *compiler) saveHomes() {
	for k, h := range c.home {
		if h >= 0 && c.use.written[k] {
			c.store(value{t: c.locals[k].Kind, k: kReg, r: uint8(h)}, rL, c.localOff(k))
		}
	}
}

// reloadHomes loads the locals living in registers from their slots.
func (c *compiler) reloadHomes() {
	for k, h := range c.home {
		if h < 0 {
			continue
		}
		v := value{t: c.locals[k].Kind, k: kSlot, off: c.localOff(k)}
		if isFloat(v.t) {
			c.loadX(freg(h), v)
		} else {
			c.loadG(reg(h), v)
		}
	}
}

// prologue zeroes the declared locals, and loads the locals' homes; the
// parameters are the caller's. A function that holds references loads R
// and makes each of its reference slots but the parameters' null, as the
// engine's frames are made.
func (c *compiler) prologue() {
	if c.refs {
		c.loadR()
		c.clearRefs(c.refParams, c.req.FrameRefSlots)
	}
	c.zeroLocals()
	c.reloadHomes()
}

// trap returns a label that traps with code, at the instruction at offset
// at: a site's stub stores the offset, then the code's common stub the rest.
func (c *compiler) trap(code uint64, at uint32) asmLabel {
	k := trapKey{code, at}
	if l, ok := c.traps[k]; ok {
		return l
	}
	l := c.a.NewLabel()
	c.traps[k] = l
	to, ok := c.trapTo[code]
	if !ok {
		to = c.a.NewLabel()
		c.trapTo[code] = to
		c.ool = append(c.ool, func() {
			c.a.Bind(to)
			c.trapStub(code)
		})
	}
	c.ool = append(c.ool, func() {
		c.a.Bind(l)
		c.trapSite(at, to)
	})
	return l
}

func (c *compiler) top() *ctl { return c.ctrl[len(c.ctrl)-1] }

// enter begins a construct whose parameters are on the stack, every entry
// stored in its slot.
func (c *compiler) enter(op opcode.Opcode, params, results []module.ValueType) *ctl {
	c.spillAll()
	t := &ctl{op: op, base: len(c.stack) - len(params), params: params, results: results, label: c.a.NewLabel()}
	c.ctrl = append(c.ctrl, t)
	return t
}

// markDead says the rest of the innermost construct cannot be reached, and
// skips its instructions.
func (c *compiler) markDead() {
	t := c.top()
	t.dead = true
	c.truncate(t.base)
	c.r.skipDead()
}

// toSlots stores values, the top of the stack's, into the slots of the
// positions from base: a branch's, or a construct's results. They are
// popped.
func (c *compiler) toSlots(n, base int) {
	vals := c.stack[len(c.stack)-n:]
	for i, v := range vals {
		c.storeAt(v, base+i)
	}
	c.truncate(len(c.stack) - n)
}

// copyToSlots stores the top n entries into the slots of the positions from
// base, leaving them on the stack: a conditional branch's values.
func (c *compiler) copyToSlots(n, base int) {
	vals := c.stack[len(c.stack)-n:]
	for i, v := range vals {
		c.storeAt(v, base+i)
	}
}

// toResults stores the top n entries, the function's results, where the
// caller finds them: slots 0 up. They are popped.
func (c *compiler) toResults() {
	n := len(c.sig.Results)
	vals := c.stack[len(c.stack)-n:]
	for i, v := range vals {
		c.storeResult(v, i)
	}
	c.truncate(len(c.stack) - n)
}

// copyResults is toResults leaving the entries on the stack.
func (c *compiler) copyResults() {
	n := len(c.sig.Results)
	vals := c.stack[len(c.stack)-n:]
	for i, v := range vals {
		c.storeResult(v, i)
	}
}

// jumpTo jumps to construct t, its values already where it wants them: a
// loop's back edge counts down the poll counter first.
func (c *compiler) jumpTo(t *ctl) {
	if t.op != opcode.Loop {
		c.jmp(t.label)
		return
	}
	if !t.hasPoll {
		t.hasPoll = true
		t.poll = c.a.NewLabel()
		head, poll := t.label, t.poll
		c.ool = append(c.ool, func() {
			c.a.Bind(poll)
			resume := c.a.NewLabel()
			c.exit(ExitPoll, resume)
			c.resumeAt(resume)
			c.jmp(head)
		})
	}
	c.pollBranch(t.label, t.poll)
}

// loopEntry makes the entry of the loop at offset at, whose head is head:
// out of line, it loads what the code keeps in registers, as a resumption
// does, and goes to the head, where every entry is in its slot and no
// register but the locals' homes is live.
func (c *compiler) loopEntry(at uint32, head asmLabel) {
	l := c.a.NewLabel()
	c.loops[at] = l
	c.ool = append(c.ool, func() {
		c.resumeAt(l)
		c.jmp(head)
	})
}

// branch compiles br to depth.
func (c *compiler) branch(depth int) {
	t := c.ctrl[len(c.ctrl)-1-depth]
	if depth == len(c.ctrl)-1 {
		c.toResults()
		c.jmp(c.retL)
		return
	}
	c.toSlots(len(t.labelTypes()), t.base)
	c.jumpTo(t)
}

// brIf compiles br_if to depth.
func (c *compiler) brIf(depth int) {
	a := c.a
	cond := c.condition()
	t := c.ctrl[len(c.ctrl)-1-depth]
	ret := depth == len(c.ctrl)-1
	n := len(t.labelTypes())
	// A branch whose values are where its target wants them is one jump.
	inPlace := !ret && t.op != opcode.Loop
	for i, v := range c.stack[len(c.stack)-n:] {
		if !(v.k == kSlot && v.off == c.ownOff(v.t, t.base+i)) {
			inPlace = false
		}
	}
	if inPlace {
		c.jcc(cond, t.label)
		return
	}
	skip := a.NewLabel()
	c.jcc(cond.Invert(), skip)
	if ret {
		c.copyResults()
		c.jmp(c.retL)
	} else {
		c.copyToSlots(n, t.base)
		c.jumpTo(t)
	}
	a.Bind(skip)
}

// fallthroughResults stores a construct's results, on top of the stack, into
// their positions' slots and empties the stack to its base.
func (c *compiler) fallthroughResults(t *ctl) {
	if !t.dead {
		c.toSlots(len(t.results), t.base)
	}
	c.truncate(t.base)
}

// elseBegin switches an if from its then branch to its else branch.
func (c *compiler) elseBegin() {
	t := c.top()
	c.fallthroughResults(t)
	if !t.dead {
		c.jmp(t.label)
	}
	c.a.Bind(t.elseL)
	t.hasElse = true
	t.dead = false
	for _, p := range t.params {
		c.pushSlot(p.Kind)
	}
}

// end ends the innermost construct.
func (c *compiler) end() {
	t := c.top()
	if t.op == opcode.If && !t.hasElse {
		c.elseBegin()
	}
	if len(c.ctrl) == 1 {
		// The function's body: its results go where the caller finds them,
		// and the return sequence follows.
		if !t.dead {
			c.toResults()
		}
		c.truncate(0)
		c.ctrl = c.ctrl[:0]
		return
	}
	c.fallthroughResults(t)
	if t.op != opcode.Loop {
		c.a.Bind(t.label)
	}
	c.ctrl = c.ctrl[:len(c.ctrl)-1]
	if c.top().dead {
		return
	}
	for _, rt := range t.results {
		c.pushSlot(rt.Kind)
	}
}

// instr compiles one instruction.
func (c *compiler) instr() {
	a := c.a
	at := uint32(c.r.pc)
	op := c.r.opcode()
	switch op {
	case opcode.Unreachable:
		c.jmp(c.trap(TrapUnreachable, at))
		c.markDead()
	case opcode.Nop:
	case opcode.Block:
		params, results := c.r.blockType(c.m)
		c.enter(op, params, results)
	case opcode.Loop:
		params, results := c.r.blockType(c.m)
		t := c.enter(op, params, results)
		a.Bind(t.label)
		if !c.inTry() {
			c.loopEntry(at, t.label)
		}
	case opcode.If:
		params, results := c.r.blockType(c.m)
		cond := c.condition()
		t := c.enter(op, params, results)
		t.elseL = a.NewLabel()
		c.jcc(cond.Invert(), t.elseL)
	case opcode.Else:
		c.elseBegin()
	case opcode.End:
		switch c.top().op {
		case opcode.TryTable:
			c.closeTry(at)
		case opcode.Try:
			c.closeLegacy(at)
		}
		c.end()
	case opcode.TryTable:
		c.tryTable()
	case opcode.Try:
		c.legacyTry()
	case opcode.Catch:
		c.legacyCatch(at, c.r.u32(), false)
	case opcode.CatchAll:
		c.legacyCatch(at, 0, true)
	case opcode.Delegate:
		c.delegate(at, int(c.r.u32()))
	case opcode.Rethrow:
		c.rethrow(at, int(c.r.u32()))
	case opcode.Throw, opcode.ThrowRef:
		var tag uint64
		if op == opcode.Throw {
			tag = uint64(c.r.u32())
		}
		c.goOp(op, at, tag, 0)
		c.markDead()
	case opcode.Br:
		c.branch(int(c.r.u32()))
		c.markDead()
	case opcode.BrIf:
		c.brIf(int(c.r.u32()))
	case opcode.BrTable:
		c.brTable()
	case opcode.Return:
		c.branch(len(c.ctrl) - 1)
		c.markDead()
	case opcode.CallFunction:
		c.call(c.r.u32(), at)
	case opcode.ReturnCall:
		c.returnCall(c.r.u32(), at)
		c.markDead()
	case opcode.CallIndirect, opcode.ReturnCallIndirect:
		typeIndex := c.r.u32()
		table := c.r.u32()
		c.callIndirect(typeIndex, table, at, op == opcode.ReturnCallIndirect)
	case opcode.Drop:
		c.free(c.pop())
	case opcode.Select:
		c.selectOp()
	case opcode.SelectWithType:
		var t module.ValueType
		for n := c.r.u32(); n > 0; n-- {
			t = c.r.valueType()
		}
		if t.IsRef() {
			c.selectRef()
		} else {
			c.selectOp()
		}
	case opcode.LocalGet:
		c.localGet(int(c.r.u32()))
	case opcode.LocalSet, opcode.LocalTee:
		c.localSet(int(c.r.u32()), op == opcode.LocalTee)
	case opcode.GlobalGet:
		if g := c.r.u32(); c.m.Globals[g].Type.IsRef() {
			c.globalGetRef(g)
		} else {
			c.globalGet(g)
		}
	case opcode.GlobalSet:
		if g := c.r.u32(); c.m.Globals[g].Type.IsRef() {
			c.globalSetRef(g)
		} else {
			c.globalSet(g)
		}
	case opcode.I32Const:
		c.pushConst(module.KindI32, uint64(uint32(c.r.s32())))
	case opcode.I64Const:
		c.pushConst(module.KindI64, uint64(c.r.s64()))
	case opcode.F32Const:
		c.pushConst(module.KindF32, uint64(c.r.fixed32()))
	case opcode.F64Const:
		c.pushConst(module.KindF64, c.r.fixed64())
	case opcode.MemorySize, opcode.MemoryGrow:
		c.goOp(op, at, uint64(c.r.u32()), 0)
	case opcode.MemoryInit, opcode.MemoryCopy, opcode.TableInit, opcode.TableCopy:
		x := uint64(c.r.u32())
		c.goOp(op, at, x, uint64(c.r.u32()))
	case opcode.DataDrop, opcode.ElemDrop, opcode.MemoryFill, opcode.TableSize, opcode.TableGrow, opcode.TableFill:
		c.goOp(op, at, uint64(c.r.u32()), 0)
	default:
		if c.numeric(op, at) || c.memoryOp(op, at) || c.atomicOp(op, at) || c.refOp(op, at) || c.gcOp(op, at) || c.simd(op, at) {
			return
		}
		refuse(RefuseOther, "%s", op.Name())
	}
}

// localGet loads local k into a register.
func (c *compiler) localGet(k int) {
	t := c.locals[k].Kind
	if t == module.KindRef {
		c.localGetRef(k)
		return
	}
	v := value{t: t, k: kSlot, off: c.localOff(k)}
	if h := c.home[k]; h >= 0 {
		v = value{t: t, k: kReg, r: uint8(h)}
	}
	if isFloat(t) {
		x := c.allocX(0)
		c.loadX(x, v)
		c.pushReg(t, uint8(x))
		return
	}
	r := c.allocG(0)
	c.loadG(r, v)
	c.pushReg(t, uint8(r))
}

// localSet stores the top of the stack into local k; local.tee leaves it.
func (c *compiler) localSet(k int, tee bool) {
	if c.locals[k].IsRef() {
		c.localSetRef(k, tee)
		return
	}
	v := c.pop()
	if v.k == kSlot && tee {
		// Keep it in a register, as a local.get would have it.
		if isFloat(v.t) {
			x := c.intoX(v)
			v = value{t: v.t, k: kReg, r: uint8(x)}
		} else {
			r := c.intoG(v, 0)
			v = value{t: v.t, k: kReg, r: uint8(r)}
		}
	}
	if h := c.home[k]; h >= 0 {
		if isFloat(v.t) {
			c.loadX(freg(h), v)
		} else {
			c.loadG(reg(h), v)
		}
	} else {
		c.store(v, rL, c.localOff(k))
	}
	if tee {
		c.push(v)
		return
	}
	c.free(v)
}

// goOp runs instruction op in Go: its operands, and every other entry,
// stored in their slots, its result left in the first's.
func (c *compiler) goOp(op opcode.Opcode, at uint32, x, y uint64) {
	var nargs int
	var result module.Kind
	is64 := func(mem uint32) bool { return int(mem) < len(c.m.Memories) && c.m.Memories[mem].Limits.Is64 }
	tab64 := func(t uint32) bool { return int(t) < len(c.m.Tables) && c.m.Tables[t].Limits.Is64 }
	idx := func(b bool) module.Kind {
		if b {
			return module.KindI64
		}
		return module.KindI32
	}
	switch op {
	case opcode.MemorySize:
		result = idx(is64(uint32(x)))
	case opcode.MemoryGrow:
		nargs, result = 1, idx(is64(uint32(x)))
	case opcode.MemoryInit, opcode.MemoryCopy, opcode.MemoryFill, opcode.TableInit, opcode.TableCopy:
		nargs = 3
	case opcode.TableSize:
		result = idx(tab64(uint32(x)))
	case opcode.AtomicNotify:
		nargs, result = 2, module.KindI32
	case opcode.I32AtomicWait, opcode.I64AtomicWait:
		nargs, result = 3, module.KindI32
	case opcode.TableGrow:
		nargs, result = 2, idx(tab64(uint32(x)))
	case opcode.TableFill:
		nargs = 3
	case opcode.StructNew:
		nargs, result = len(c.m.Types[x].Struct.Fields), module.KindRef
	case opcode.StructNewDefault:
		result = module.KindRef
	case opcode.ArrayNew, opcode.ArrayNewData, opcode.ArrayNewElem:
		nargs, result = 2, module.KindRef
	case opcode.ArrayNewDefault:
		nargs, result = 1, module.KindRef
	case opcode.ArrayNewFixed:
		nargs, result = int(y), module.KindRef
	case opcode.ArrayFill, opcode.ArrayInitData, opcode.ArrayInitElem:
		nargs = 4
	case opcode.ArrayCopy:
		nargs = 5
	case opcode.Throw:
		nargs = len(c.m.TagType(uint32(x)).Params)
	case opcode.ThrowRef:
		nargs = 1
	}
	c.spillAll()
	base := len(c.stack) - nargs
	c.truncate(base)
	resume := c.a.NewLabel()
	c.exit(ExitOp, resume, uint64(op), x, y, c.slotIndex(base), uint64(at))
	c.resumeAt(resume)
	if result != 0 {
		c.pushSlot(result)
	}
}

// pushResults pushes a call's results, in the slots of the positions its
// arguments took.
func (c *compiler) pushResults(results []module.ValueType) {
	for _, t := range results {
		c.pushSlot(t.Kind)
	}
}

// callIndirect compiles call_indirect and return_call_indirect, which the
// engine makes: it finds the function and checks its type.
func (c *compiler) callIndirect(typeIndex, table uint32, at uint32, tail bool) {
	a := c.a
	ft := c.m.Types[typeIndex].Func
	n := len(ft.Params)
	c.spillAll()
	base := len(c.stack) - n - 1
	c.truncate(base)
	resume := a.NewLabel()
	if tail {
		c.exit(ExitTailIndirect, resume, uint64(typeIndex), uint64(table), c.slotIndex(base), uint64(at))
		c.resumeAt(resume)
		c.jmp(c.retL)
		c.markDead()
		return
	}
	c.exit(ExitCallIndirect, resume, uint64(typeIndex), uint64(table), c.slotIndex(base), uint64(at))
	c.resumeAt(resume)
	c.pushResults(ft.Results)
}
