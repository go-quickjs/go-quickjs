//go:build amd64

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
}

// Code is a function's native code. Its offsets are from its start.
type Code struct {
	Bytes []byte
	// GoEntry is where the engine enters the function, Entry where native
	// callers do, and ReturnStub the code that returns to the engine,
	// which the engine gives as the return address of the frame it enters.
	GoEntry, Entry, ReturnStub int
}

// The context's fields' offsets.
var (
	offL          = int32(unsafe.Offsetof(Ctx{}.L))
	offLinks      = int32(unsafe.Offsetof(Ctx{}.Links))
	offMemBase    = int32(unsafe.Offsetof(Ctx{}.MemBase))
	offMemSize    = int32(unsafe.Offsetof(Ctx{}.MemSize))
	offMemSizePtr = int32(unsafe.Offsetof(Ctx{}.MemSizePtr))
	offGlobals    = int32(unsafe.Offsetof(Ctx{}.Globals))
	offChunkEnd   = int32(unsafe.Offsetof(Ctx{}.ChunkEnd))
	offRSP        = int32(unsafe.Offsetof(Ctx{}.RSP))
	offRSEnd      = int32(unsafe.Offsetof(Ctx{}.RSEnd))
	offPoll       = int32(unsafe.Offsetof(Ctx{}.Poll))
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
	home       []int8
	nlocals    int
	frameSlots int
	maxHeight  int

	stack []value
	ctrl  []*ctl
	gused regMask
	xused regMask

	retL   asmLabel
	ool    []func()
	traps  map[trapKey]asmLabel
	trapTo map[uint64]asmLabel
	shared bool
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
		frameSlots: req.FrameSlots, traps: map[trapKey]asmLabel{}, trapTo: map[uint64]asmLabel{}}
	c.locals, c.use = scan(m, req.Func, CPU)
	c.nlocals = max(len(c.locals), len(c.sig.Results))
	c.chooseHomes()
	if len(m.Memories) > 0 {
		c.shared = m.Memories[0].Shared
	}
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
	if 2*c.maxHeight+c.nlocals > c.frameSlots {
		refuse(RefuseFrame, "%d stack positions and %d locals in a frame of %d slots", c.maxHeight, c.nlocals, c.frameSlots)
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
	return &Code{Bytes: out, GoEntry: a.Offset(goEntry), Entry: a.Offset(entry), ReturnStub: retStub}, nil
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
		if isFloat(c.locals[k].Kind) {
			if nx < len(homeXRegs) {
				c.home[k] = int8(homeXRegs[nx])
				c.xused |= 1 << homeXRegs[nx]
				nx++
			}
		} else if ng < len(homeGPRs) {
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
			c.store(value{t: c.locals[k].Kind, k: kReg, r: uint8(h)}, rL, int32(8*k))
		}
	}
}

// reloadHomes loads the locals living in registers from their slots.
func (c *compiler) reloadHomes() {
	for k, h := range c.home {
		if h < 0 {
			continue
		}
		v := value{t: c.locals[k].Kind, k: kSlot, off: int32(8 * k)}
		if isFloat(v.t) {
			c.loadX(freg(h), v)
		} else {
			c.loadG(reg(h), v)
		}
	}
}

// prologue zeroes the declared locals, and loads the locals' homes; the
// parameters are the caller's.
func (c *compiler) prologue() {
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
		c.store(v, rL, c.slotOff(base+i))
	}
	c.truncate(len(c.stack) - n)
}

// copyToSlots stores the top n entries into the slots of the positions from
// base, leaving them on the stack: a conditional branch's values.
func (c *compiler) copyToSlots(n, base int) {
	vals := c.stack[len(c.stack)-n:]
	for i, v := range vals {
		c.store(v, rL, c.slotOff(base+i))
	}
}

// toResults stores the top n entries, the function's results, where the
// caller finds them: slots 0 up. They are popped.
func (c *compiler) toResults() {
	n := len(c.sig.Results)
	vals := c.stack[len(c.stack)-n:]
	for i, v := range vals {
		c.store(v, rL, int32(8*i))
	}
	c.truncate(len(c.stack) - n)
}

// copyResults is toResults leaving the entries on the stack.
func (c *compiler) copyResults() {
	n := len(c.sig.Results)
	vals := c.stack[len(c.stack)-n:]
	for i, v := range vals {
		c.store(v, rL, int32(8*i))
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
		if !(v.k == kSlot && v.off == c.slotOff(t.base+i)) {
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
	case opcode.If:
		params, results := c.r.blockType(c.m)
		cond := c.condition()
		t := c.enter(op, params, results)
		t.elseL = a.NewLabel()
		c.jcc(cond.Invert(), t.elseL)
	case opcode.Else:
		c.elseBegin()
	case opcode.End:
		c.end()
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
		for n := c.r.u32(); n > 0; n-- {
			c.r.valueType()
		}
		c.selectOp()
	case opcode.LocalGet:
		c.localGet(int(c.r.u32()))
	case opcode.LocalSet, opcode.LocalTee:
		c.localSet(int(c.r.u32()), op == opcode.LocalTee)
	case opcode.GlobalGet:
		c.globalGet(c.r.u32())
	case opcode.GlobalSet:
		c.globalSet(c.r.u32())
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
	case opcode.DataDrop, opcode.ElemDrop, opcode.MemoryFill, opcode.TableSize:
		c.goOp(op, at, uint64(c.r.u32()), 0)
	default:
		if c.numeric(op, at) || c.memoryOp(op, at) {
			return
		}
		refuse(RefuseOther, "%s", op.Name())
	}
}

// localGet loads local k into a register.
func (c *compiler) localGet(k int) {
	t := c.locals[k].Kind
	v := value{t: t, k: kSlot, off: int32(8 * k)}
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
		c.store(v, rL, int32(8*k))
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
