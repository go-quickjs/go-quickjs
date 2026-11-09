package jit

import (
	"unsafe"

	"golang.org/x/sys/cpu"

	"github.com/go-quickjs/go-quickjs/internal/jit/asm/amd64"
	"github.com/go-quickjs/go-quickjs/wasm/internal/module"
	"github.com/go-quickjs/go-quickjs/wasm/internal/opcode"
)

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
	offDepth      = int32(unsafe.Offsetof(Ctx{}.Depth))
	offMaxDepth   = int32(unsafe.Offsetof(Ctx{}.MaxDepth))
	offPoll       = int32(unsafe.Offsetof(Ctx{}.Poll))
	offExit       = int32(unsafe.Offsetof(Ctx{}.Exit))
	offArg0       = int32(unsafe.Offsetof(Ctx{}.Arg0))
	offExitFn     = int32(unsafe.Offsetof(Ctx{}.ExitFn))
	offExitAddr   = int32(unsafe.Offsetof(Ctx{}.ExitAddr))
)

// CPU is the CPU's optional instructions this process's code may use.
var CPU = Features{Popcnt: cpu.X86.HasPOPCNT, Round: cpu.X86.HasSSE41}

// Available reports whether this build compiles native code.
const Available = true

// ctl is a construct being compiled.
type ctl struct {
	op              opcode.Opcode
	base            int
	params, results []module.ValueType
	// label is where a branch to the construct goes: a loop's head, the
	// end of anything else, the return sequence for the function's body.
	label amd64.Label
	// elseL is an if's else branch.
	elseL   amd64.Label
	hasElse bool
	// dead says the rest of the construct cannot be reached.
	dead bool
	// poll is a loop's poll exit, made with its first back edge.
	poll    amd64.Label
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
	a   *amd64.Asm
	req *Request
	m   *module.Module
	fn  uint32
	r   reader
	sig *module.FuncType

	locals     []module.ValueType
	nlocals    int
	frameSlots int
	maxHeight  int

	stack []value
	ctrl  []*ctl
	gused uint16
	xused uint16

	retL   amd64.Label
	ool    []func()
	traps  map[trapKey]amd64.Label
	trapTo map[uint64]amd64.Label
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
	c := &compiler{a: &amd64.Asm{}, req: req, m: m, fn: req.Func, sig: m.FuncType(req.Func),
		frameSlots: req.FrameSlots, traps: map[trapKey]amd64.Label{}, trapTo: map[uint64]amd64.Label{}}
	c.locals = scan(m, req.Func, CPU)
	c.nlocals = max(len(c.locals), len(c.sig.Results))
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

	// The return to the engine, at the start.
	retStub := a.Len()
	a.StoreImm(rCtx, offExit, ExitReturn)
	a.Ret()
	// The engine's entry, then the natives'.
	goEntry := a.NewLabel()
	entry := a.NewLabel()
	a.Bind(goEntry)
	a.Load(rL, rCtx, offL)
	a.Load(rMem, rCtx, offMemBase)
	a.Bind(entry)
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
		panic(ferr)
	}
	if req.MaxCodeBytes > 0 && len(out) > req.MaxCodeBytes {
		refuse(RefuseSize, "%d bytes of code", len(out))
	}
	return &Code{Bytes: out, GoEntry: a.Offset(goEntry), Entry: a.Offset(entry), ReturnStub: retStub}, nil
}

// prologue zeroes the declared locals; the parameters are the caller's.
func (c *compiler) prologue() {
	a := c.a
	from, to := len(c.sig.Params), len(c.locals)
	if from >= to {
		return
	}
	a.Op(amd64.Xor, amd64.RAX, amd64.RAX, false)
	if to-from <= 16 {
		for k := from; k < to; k++ {
			a.Store(rL, int32(k*8), amd64.RAX)
		}
		return
	}
	a.Lea(amd64.RCX, rL, int32(from*8))
	a.MovImm(amd64.RDX, uint64(to-from))
	loop := a.NewLabel()
	a.Bind(loop)
	a.Store(amd64.RCX, 0, amd64.RAX)
	a.OpImm(amd64.Add, amd64.RCX, 8, true)
	a.OpImm(amd64.Sub, amd64.RDX, 1, true)
	a.Jcc(amd64.CondNE, loop)
}

// returnSequence binds the return label: it pops the return stack's entry
// and goes where it says, the caller's L restored.
func (c *compiler) returnSequence() {
	a := c.a
	a.Bind(c.retL)
	a.Load(rTmp, rCtx, offRSP)
	a.OpImm(amd64.Sub, rTmp, RSEntryWords*8, true)
	a.Store(rCtx, offRSP, rTmp)
	a.DecMem(rCtx, offDepth)
	a.Load(rL, rTmp, 8)
	a.JmpMem(rTmp, 0)
}

// storeCtx stores v into the context's word at off.
func (c *compiler) storeCtx(off int32, v uint64) {
	if v <= 0x7fffffff {
		c.a.StoreImm(rCtx, off, int32(v))
		return
	}
	c.a.MovImm(rTmp, v)
	c.a.Store(rCtx, off, rTmp)
}

// exit returns to the engine for kind, with args, to resume at resume. The
// registers are lost: whatever lives past it is in its slot.
func (c *compiler) exit(kind uint64, resume amd64.Label, args ...uint64) {
	a := c.a
	a.Store(rCtx, offL, rL)
	c.storeCtx(offExit, kind)
	for i, v := range args {
		c.storeCtx(offArg0+int32(8*i), v)
	}
	c.storeCtx(offExitFn, uint64(c.fn))
	a.LeaLabel(rTmp, resume)
	a.Store(rCtx, offExitAddr, rTmp)
	a.Ret()
}

// resumeAt binds l, where the engine resumes the code, and reloads the
// registers an exit loses that the code keeps.
func (c *compiler) resumeAt(l amd64.Label) {
	c.a.Bind(l)
	c.a.Load(rL, rCtx, offL)
	c.a.Load(rMem, rCtx, offMemBase)
}

// trap returns a label that traps with code, at the instruction at offset
// at: a site's stub stores the offset, then the code's common stub the rest.
func (c *compiler) trap(code uint64, at uint32) amd64.Label {
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
			a := c.a
			a.Bind(to)
			a.Store(rCtx, offL, rL)
			c.storeCtx(offExit, ExitTrap)
			c.storeCtx(offArg0, code)
			c.storeCtx(offExitFn, uint64(c.fn))
			a.Ret()
		})
	}
	c.ool = append(c.ool, func() {
		c.a.Bind(l)
		c.storeCtx(offArg0+8, uint64(at))
		c.a.Jmp(to)
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
	a := c.a
	if t.op != opcode.Loop {
		a.Jmp(t.label)
		return
	}
	if !t.hasPoll {
		t.hasPoll = true
		t.poll = a.NewLabel()
		head, poll := t.label, t.poll
		c.ool = append(c.ool, func() {
			c.a.Bind(poll)
			resume := c.a.NewLabel()
			c.exit(ExitPoll, resume)
			c.resumeAt(resume)
			c.a.Jmp(head)
		})
	}
	a.DecMem(rCtx, offPoll)
	a.Jcc(amd64.CondG, t.label)
	a.Jmp(t.poll)
}

// branch compiles br to depth.
func (c *compiler) branch(depth int) {
	t := c.ctrl[len(c.ctrl)-1-depth]
	if depth == len(c.ctrl)-1 {
		c.toResults()
		c.a.Jmp(c.retL)
		return
	}
	c.toSlots(len(t.labelTypes()), t.base)
	c.jumpTo(t)
}

// condition pops an i32 condition, returning the condition under which it
// is true: the flags of a comparison made for it, or a test of its value.
func (c *compiler) condition() amd64.Cond {
	v := c.pop()
	switch v.k {
	case kFlags:
		return amd64.Cond(v.c)
	case kConst:
		r := c.allocG(0)
		c.a.MovImm(r, uint64(uint32(v.c)))
		c.a.Op(amd64.Test, r, r, false)
		c.freeG(r)
		return amd64.CondNE
	}
	r := c.intoG(v, 0)
	c.a.Op(amd64.Test, r, r, false)
	c.freeG(r)
	return amd64.CondNE
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
		a.Jcc(cond, t.label)
		return
	}
	skip := a.NewLabel()
	a.Jcc(cond.Invert(), skip)
	if ret {
		c.copyResults()
		a.Jmp(c.retL)
	} else {
		c.copyToSlots(n, t.base)
		c.jumpTo(t)
	}
	a.Bind(skip)
}

// brTable compiles br_table: every entry stored, then a jump through a
// table of jumps, each to its target's stub, which moves the values.
func (c *compiler) brTable() {
	a := c.a
	n := c.r.u32()
	depths := make([]int, n+1)
	for i := range depths {
		depths[i] = int(c.r.u32())
	}
	idx := c.intoG(c.pop(), 0)
	c.spillAll()
	arity := len(c.ctrl[len(c.ctrl)-1-depths[n]].labelTypes())
	src := len(c.stack) - arity
	a.MovImm(rTmp, uint64(n))
	a.Op(amd64.Cmp, idx, rTmp, false)
	a.Cmov(amd64.CondAE, idx, rTmp, false)
	table := a.NewLabel()
	a.LeaLabel(rTmp, table)
	a.LeaIdx(rTmp, rTmp, idx, 8, 0)
	a.JmpReg(rTmp)
	c.freeG(idx)
	a.Bind(table)
	stubs := map[int]amd64.Label{}
	var order []int
	for _, d := range depths {
		l, ok := stubs[d]
		if !ok {
			l = a.NewLabel()
			stubs[d] = l
			order = append(order, d)
		}
		a.JmpLong(l)
		a.Nop(3)
	}
	for _, d := range order {
		a.Bind(stubs[d])
		t := c.ctrl[len(c.ctrl)-1-d]
		if d == len(c.ctrl)-1 {
			for i := range c.sig.Results {
				a.Load(rTmp, rL, c.slotOff(src+i))
				a.Store(rL, int32(8*i), rTmp)
			}
			a.Jmp(c.retL)
			continue
		}
		for i := range t.labelTypes() {
			if from, to := c.slotOff(src+i), c.slotOff(t.base+i); from != to {
				a.Load(rTmp, rL, from)
				a.Store(rL, to, rTmp)
			}
		}
		c.jumpTo(t)
	}
	c.markDead()
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
		c.a.Jmp(t.label)
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
		a.Jmp(c.trap(TrapUnreachable, at))
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
		a.Jcc(cond.Invert(), t.elseL)
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
	c.store(v, rL, int32(8*k))
	if tee {
		c.push(v)
		return
	}
	c.free(v)
}

// globalAddr loads global g's address into rTmp.
func (c *compiler) globalAddr(g uint32) {
	c.a.Load(rTmp, rCtx, offGlobals)
	c.a.Load(rTmp, rTmp, int32(8*g))
}

func (c *compiler) globalGet(g uint32) {
	t := c.m.Globals[g].Type.Kind
	c.globalAddr(g)
	v := value{t: t}
	if isFloat(t) {
		x := c.allocX(0)
		if t == module.KindF32 {
			c.a.LoadSS(x, rTmp, 0)
		} else {
			c.a.LoadSD(x, rTmp, 0)
		}
		v.k, v.r = kReg, uint8(x)
	} else {
		r := c.allocG(0)
		if t == module.KindI32 {
			c.a.LoadU32(r, rTmp, 0)
		} else {
			c.a.Load(r, rTmp, 0)
		}
		v.k, v.r = kReg, uint8(r)
	}
	c.push(v)
}

func (c *compiler) globalSet(g uint32) {
	v := c.pop()
	// The value goes to a register first: storing a slot's or a large
	// constant goes through rTmp, which holds the address.
	var r amd64.Reg
	switch {
	case v.k == kReg && !isFloat(v.t):
		r = v.gpr()
	case v.k == kReg && v.t == module.KindF64:
		c.globalAddr(g)
		c.a.StoreSD(rTmp, 0, v.xmm())
		c.free(v)
		return
	case v.k == kReg:
		r = c.allocG(0)
		c.a.MovDFromX(r, v.xmm())
		c.free(v)
		v = value{t: v.t, k: kReg, r: uint8(r)}
	default:
		r = c.allocG(0)
		bits := v.c
		if v.k == kSlot {
			c.a.Load(r, rL, v.off)
		} else {
			if v.t == module.KindI32 || v.t == module.KindF32 {
				bits = uint64(uint32(bits))
			}
			c.a.MovImm(r, bits)
		}
		v = value{t: v.t, k: kReg, r: uint8(r)}
	}
	c.globalAddr(g)
	c.a.Store(rTmp, 0, r)
	c.free(v)
}

// selectOp compiles select: the first operand if the condition is true.
func (c *compiler) selectOp() {
	cond := c.pop()
	b := c.pop()
	x := c.pop()
	a := c.a
	if isFloat(x.t) {
		xa := c.intoX(x)
		xb := c.intoX(b)
		rc := c.intoG(cond, 0)
		a.Op(amd64.Test, rc, rc, false)
		c.freeG(rc)
		done := a.NewLabel()
		a.Jcc(amd64.CondNE, done)
		a.SSEOp(amd64.MovAPD, xa, xb)
		a.Bind(done)
		c.freeX(xb)
		c.pushReg(x.t, uint8(xa))
		return
	}
	wide := x.t == module.KindI64
	ra := c.intoG(x, 0)
	rb := c.intoG(b, 1<<ra)
	rc := c.intoG(cond, 1<<ra|1<<rb)
	a.Op(amd64.Test, rc, rc, false)
	a.Cmov(amd64.CondE, ra, rb, wide)
	c.freeG(rb)
	c.freeG(rc)
	c.pushReg(x.t, uint8(ra))
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

// call compiles a call of function idx: natively where the callee has
// native code and there is room, else through the engine.
func (c *compiler) call(idx uint32, at uint32) {
	a := c.a
	ft := c.m.FuncType(idx)
	n := len(ft.Params)
	c.spillAll()
	base := len(c.stack) - n
	c.truncate(base)
	after := a.NewLabel()
	if c.m.Functions[idx].Imported {
		c.exit(ExitCall, after, uint64(idx), c.slotIndex(base), uint64(at))
		c.resumeAt(after)
		c.pushResults(ft.Results)
		return
	}
	start := a.NewLabel()
	slow := a.NewLabel()
	poll := a.NewLabel()
	a.Bind(start)
	a.DecMem(rCtx, offPoll)
	a.Jcc(amd64.CondLE, poll)
	c.nativeCallChecks(idx, at, c.frameSlots, slow)
	// The arguments, into the callee's parameters.
	frame := int32(8 * c.frameSlots)
	for i := 0; i < n; i++ {
		a.Load(amd64.RCX, rL, c.slotOff(base+i))
		a.Store(rL, frame+int32(8*i), amd64.RCX)
	}
	ret := a.NewLabel()
	a.LeaLabel(amd64.RCX, ret)
	a.Store(amd64.RDX, 0, amd64.RCX)
	a.Store(amd64.RDX, 8, rL)
	a.MovImm(amd64.RCX, Site(idx, at))
	a.Store(amd64.RDX, 16, amd64.RCX)
	a.OpImm(amd64.Add, amd64.RDX, RSEntryWords*8, true)
	a.Store(rCtx, offRSP, amd64.RDX)
	a.IncMem(rCtx, offDepth)
	a.OpImm(amd64.Add, rL, frame, true)
	a.JmpReg(amd64.RAX)
	a.Bind(ret)
	// The results, from the callee's frame.
	for i := range ft.Results {
		a.Load(amd64.RCX, rL, frame+int32(8*i))
		a.Store(rL, c.slotOff(base+i), amd64.RCX)
	}
	a.Bind(after)
	slotIdx := c.slotIndex(base)
	c.ool = append(c.ool, func() {
		a.Bind(poll)
		r1 := a.NewLabel()
		c.exit(ExitPoll, r1)
		c.resumeAt(r1)
		a.Jmp(start)
		a.Bind(slow)
		r2 := a.NewLabel()
		c.exit(ExitCall, r2, uint64(idx), slotIdx, uint64(at))
		c.resumeAt(r2)
		a.Jmp(after)
	})
	c.pushResults(ft.Results)
}

// nativeCallChecks leaves the callee's entry in AX and the return stack's
// next entry in DX, or goes to slow: the callee has no native code, its
// frame no room in the chunk past the caller's frameSlots, or the return
// stack none for an entry. A call past the depth limit traps.
func (c *compiler) nativeCallChecks(idx, at uint32, frameSlots int, slow amd64.Label) {
	a := c.a
	a.Load(amd64.RAX, rCtx, offLinks)
	a.Load(amd64.RAX, amd64.RAX, int32(8*idx))
	a.Op(amd64.Test, amd64.RAX, amd64.RAX, true)
	a.Jcc(amd64.CondE, slow)
	a.Lea(amd64.RCX, rL, int32(8*(frameSlots+c.req.FrameSlotsOf(idx))))
	a.CmpMem(amd64.RCX, rCtx, offChunkEnd, true)
	a.Jcc(amd64.CondA, slow)
	a.Load(amd64.RCX, rCtx, offDepth)
	a.CmpMem(amd64.RCX, rCtx, offMaxDepth, true)
	a.Jcc(amd64.CondGE, c.trap(TrapStackOverflow, at))
	a.Load(amd64.RDX, rCtx, offRSP)
	a.CmpMem(amd64.RDX, rCtx, offRSEnd, true)
	a.Jcc(amd64.CondAE, slow)
}

// pushResults pushes a call's results, in the slots of the positions its
// arguments took.
func (c *compiler) pushResults(results []module.ValueType) {
	for _, t := range results {
		c.pushSlot(t.Kind)
	}
}

// returnCall compiles return_call of function idx: in place of the
// caller's frame, natively where it can be.
func (c *compiler) returnCall(idx uint32, at uint32) {
	a := c.a
	ft := c.m.FuncType(idx)
	n := len(ft.Params)
	c.spillAll()
	base := len(c.stack) - n
	c.truncate(base)
	retResume := a.NewLabel()
	slotIdx := c.slotIndex(base)
	if c.m.Functions[idx].Imported {
		c.exit(ExitTailCall, retResume, uint64(idx), slotIdx, uint64(at))
		c.resumeAt(retResume)
		a.Jmp(c.retL)
		return
	}
	start := a.NewLabel()
	slow := a.NewLabel()
	poll := a.NewLabel()
	a.Bind(start)
	a.DecMem(rCtx, offPoll)
	a.Jcc(amd64.CondLE, poll)
	a.Load(amd64.RAX, rCtx, offLinks)
	a.Load(amd64.RAX, amd64.RAX, int32(8*idx))
	a.Op(amd64.Test, amd64.RAX, amd64.RAX, true)
	a.Jcc(amd64.CondE, slow)
	a.Lea(amd64.RCX, rL, int32(8*c.req.FrameSlotsOf(idx)))
	a.CmpMem(amd64.RCX, rCtx, offChunkEnd, true)
	a.Jcc(amd64.CondA, slow)
	// The arguments, into the parameters: each slot read is past each
	// written before it.
	for i := 0; i < n; i++ {
		a.Load(amd64.RCX, rL, c.slotOff(base+i))
		a.Store(rL, int32(8*i), amd64.RCX)
	}
	// The frame's return stack entry names the callee now.
	a.Load(amd64.RDX, rCtx, offRSP)
	a.Load(amd64.RCX, amd64.RDX, -8)
	a.MovRR32(amd64.RCX, amd64.RCX)
	a.MovImm(rTmp, uint64(idx)<<32)
	a.Op(amd64.Or, amd64.RCX, rTmp, true)
	a.Store(amd64.RDX, -8, amd64.RCX)
	a.JmpReg(amd64.RAX)
	c.ool = append(c.ool, func() {
		a.Bind(poll)
		r1 := a.NewLabel()
		c.exit(ExitPoll, r1)
		c.resumeAt(r1)
		a.Jmp(start)
		a.Bind(slow)
		c.exit(ExitTailCall, retResume, uint64(idx), slotIdx, uint64(at))
		c.resumeAt(retResume)
		a.Jmp(c.retL)
	})
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
		a.Jmp(c.retL)
		c.markDead()
		return
	}
	c.exit(ExitCallIndirect, resume, uint64(typeIndex), uint64(table), c.slotIndex(base), uint64(at))
	c.resumeAt(resume)
	c.pushResults(ft.Results)
}
