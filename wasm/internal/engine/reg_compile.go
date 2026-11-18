package engine

import (
	"math"

	"github.com/go-quickjs/go-quickjs/wasm/internal/module"
	"github.com/go-quickjs/go-quickjs/wasm/internal/opcode"
)

// The register compiler compiles a validated function body to register code
// in one pass, following the operand stack symbolically as the closure tree's
// compiler does (README.md, "The compiler"). An entry of the stack is a
// constant, or a slot holding the value: a local's, read in place, or its
// stack position's. Every instruction is emitted as it is read, so entries
// are never pending work; what is deferred is only where a value lives:
//
//   - A constant is an immediate of the operation that takes it where one
//     has a form for it, and is loaded into its position's slot otherwise.
//   - A local is read in place until it is written: local.set and local.tee
//     first copy the stack's reads of the local into their positions.
//   - An operation writes its result to its position's slot, and the entry
//     remembers the instruction (def). A local.set or local.tee, or a
//     return, that takes the value straight after rewrites the
//     instruction's destination instead of copying it (copy elision).
//   - A comparison of integers that a br_if or an if takes straight after
//     is rewritten into a compare-and-branch.
//
// Every construct begins with each entry in its position's slot, so that a
// branch's values go to fixed slots, and a loop's head is the state native
// code is entered with there (osr).

// regBail is what the register compiler panics with for a body it does not
// compile, which the closure tree then compiles: why.
type regBail string

// rentry is an entry of the symbolic operand stack.
type rentry struct {
	t     module.ValueType
	konst bool
	c     uint64
	// slot is the slot holding the value, unless konst: local's where it
	// reads a local in place, local -1 otherwise.
	slot  int
	local int
	// def is the pc of the instruction that wrote the value to slot, its
	// destination A, or -1; cmp is the integer comparison it computed, with
	// an immediate if cmpImm, and eqz says it computed an eqz.
	def    int
	cmp    opcode.Opcode
	cmpImm bool
	eqz    bool
}

// The kinds of a branch whose target is not known yet.
const (
	fixRel16 = iota // the offset in C
	fixRel32        // the offset in BC
	fixTable        // entry idx of table pc
)

type rfix struct {
	pc   int
	kind uint8
	idx  int
}

// rctl is an enclosing construct being compiled.
type rctl struct {
	op              opcode.Opcode
	base            int
	params, results []module.ValueType
	dead            bool
	// head is a loop's first pc, where its branches go; fixes are the
	// branches to a block's or an if's end, and elseFix an if's branch to
	// its else until the else is compiled.
	head    int
	fixes   []rfix
	elseFix []rfix
	hasElse bool
}

func (t *rctl) labelTypes() []module.ValueType {
	if t.op == opcode.Loop {
		return t.params
	}
	return t.results
}

type regCompiler struct {
	code *Code
	m    *module.Module
	fn   uint32
	r    reader
	// far says every conditional branch's offset is 32 bits; retry that a
	// 16-bit offset did not reach, and the body is compiled again far.
	far, retry bool

	types     []module.ValueType
	locals    []slotRef
	nlocals   int
	results   []module.ValueType
	maxHeight int
	stack     []rentry
	ctrl      []*rctl
	out       regCode
	// at is the offset of the instruction being compiled; last the pc of
	// the last instruction emitted, -1 before the first.
	at   uint32
	last int
}

// regCompile compiles function index to register code, or returns why it
// did not.
func regCompile(code *Code, index uint32) (*compiled, string) {
	for _, far := range [2]bool{false, true} {
		c := &regCompiler{code: code, m: code.Module, fn: index, far: far, last: -1}
		p, why := c.compile()
		if why != "" || !c.retry {
			return p, why
		}
	}
	panic("wasm: register code whose branches do not reach")
}

func (c *regCompiler) compile() (p *compiled, why string) {
	defer func() {
		if r := recover(); r != nil {
			b, ok := r.(regBail)
			if !ok {
				panic(r)
			}
			p, why = nil, string(b)
		}
	}()
	m := c.m
	if len(m.Bytes) > math.MaxInt32 {
		// A trap's offset is marked as a pc by bit 31 (regAt).
		panic(regBail("a module past 2 GiB"))
	}
	f := &m.Functions[c.fn]
	sig := m.Types[f.TypeIndex].Func
	c.r = reader{b: m.Bytes, pc: int(f.Code.Offset)}
	end := int(f.Code.End())
	types := append([]module.ValueType(nil), sig.Params...)
	for n := c.r.u32(); n > 0; n-- {
		count := c.r.u32()
		t := c.r.valueType()
		if count > 1<<16 {
			panic(regBail("more than 65,536 locals"))
		}
		for j := uint32(0); j < count; j++ {
			types = append(types, t)
		}
	}
	c.numeric(types)
	c.numeric(sig.Results)
	c.types = types
	var rlocals int
	c.locals, c.nlocals, rlocals = layout(types)
	_, rn, _ := layout(sig.Results)
	c.nlocals = max(c.nlocals, rn)
	c.results = sig.Results
	c.out.loops = map[int32]uint32{}
	c.ctrl = append(c.ctrl, &rctl{results: sig.Results})
	for c.r.pc < end && len(c.ctrl) > 0 {
		c.instr()
	}
	if len(c.out.code) == 0 {
		c.emit(rRet, 0, 0, 0, 0)
	}
	if len(c.out.code) > math.MaxInt32/2 {
		panic(regBail("a body past 2^30 instructions"))
	}
	p = newCompiled()
	p.nslots = c.nlocals + 2*(c.maxHeight+1)
	p.rslots = rlocals + c.maxHeight + 1
	p.nlocals, p.rlocals = c.nlocals, rlocals
	p.caught = rlocals + c.maxHeight + 1
	p.built = true
	out := c.out
	p.reg = &out
	p.params, _, _ = layout(sig.Params)
	p.results, _, _ = layout(sig.Results)
	return p, ""
}

// numeric bails out of types that are not all i32, i64, f32 or f64.
func (c *regCompiler) numeric(ts []module.ValueType) {
	for _, t := range ts {
		switch t.Kind {
		case module.KindI32, module.KindI64, module.KindF32, module.KindF64:
		default:
			panic(regBail("a reference or vector value"))
		}
	}
}

func (c *regCompiler) numSlot(h int) int { return c.nlocals + 2*h }

// s16 returns slot s as an instruction's field.
func s16(s int) uint64 {
	if s > math.MaxUint16 {
		panic(regBail("a slot past 65,535"))
	}
	return uint64(s)
}

// emit appends an instruction and returns its pc.
func (c *regCompiler) emit(op regOp, x uint8, a, b, cc uint64) int {
	pc := len(c.out.code)
	c.out.code = append(c.out.code, uint64(op)|uint64(x)<<8|a<<16|b<<32|cc<<48)
	c.out.offs = append(c.out.offs, c.at)
	c.last = pc
	return pc
}

// emitBC appends an instruction with a 32-bit BC.
func (c *regCompiler) emitBC(op regOp, x uint8, a uint64, bc uint32) int {
	return c.emit(op, x, a, uint64(bc), 0)
}

// emitNum appends a numeric operation, by its escape where it is in the
// second table.
func (c *regCompiler) emitNum(op regOp, ext bool, a, b, cc uint64) int {
	if ext {
		return c.emit(rExt, op, a, b, cc)
	}
	return c.emit(op, 0, a, b, cc)
}

func (c *regCompiler) push(e rentry) {
	c.stack = append(c.stack, e)
	if h := len(c.stack); h > c.maxHeight {
		c.maxHeight = h
	}
}

func (c *regCompiler) pop() rentry {
	e := c.stack[len(c.stack)-1]
	c.stack = c.stack[:len(c.stack)-1]
	return e
}

// pushSlot pushes the value in position h's slot; pushDef the value
// instruction pc wrote there.
func (c *regCompiler) pushSlot(t module.ValueType, h int) {
	c.push(rentry{t: t, slot: c.numSlot(h), local: -1, def: -1})
}

func (c *regCompiler) pushDef(t module.ValueType, h, pc int) {
	c.push(rentry{t: t, slot: c.numSlot(h), local: -1, def: pc})
}

func (c *regCompiler) pushConst(t module.ValueType, v uint64) {
	c.push(rentry{t: t, konst: true, c: v, local: -1, def: -1})
}

// loadConst loads constant v of type t into slot s.
func (c *regCompiler) loadConst(s int, t module.ValueType, v uint64) {
	switch {
	case v <= math.MaxUint32:
		c.emitBC(rConst32, 0, s16(s), uint32(v))
	case (t.Kind == module.KindI64 || t.Kind == module.KindF64) && int64(v) == int64(int32(v)):
		c.emitBC(rConst32S, 0, s16(s), uint32(v))
	default:
		pc := c.emit(rConst64, 0, s16(s), 0, 0)
		c.out.code = append(c.out.code, v)
		c.out.offs = append(c.out.offs, c.at)
		c.last = pc
	}
}

// store stores entry e into slot s.
func (c *regCompiler) store(e rentry, s int) {
	switch {
	case e.konst:
		c.loadConst(s, e.t, e.c)
	case e.slot != s:
		c.emit(rMov, 0, s16(s), s16(e.slot), 0)
	}
}

// read returns the slot holding entry e, at position h: a constant is loaded
// into the position's slot.
func (c *regCompiler) read(e rentry, h int) uint64 {
	if e.konst {
		s := c.numSlot(h)
		c.loadConst(s, e.t, e.c)
		return s16(s)
	}
	return s16(e.slot)
}

// materialize stores position h's entry into its slot, unless it is there.
func (c *regCompiler) materialize(h int) {
	e := c.stack[h]
	s := c.numSlot(h)
	if !e.konst && e.slot == s {
		c.stack[h].def = -1
		return
	}
	c.store(e, s)
	c.stack[h] = rentry{t: e.t, slot: s, local: -1, def: -1}
}

// flushLocal stores the stack's reads of local k, before k is written.
func (c *regCompiler) flushLocal(k int) {
	for h := range c.stack {
		if c.stack[h].local == k {
			c.materialize(h)
		}
	}
}

// flushAll stores every entry into its position's slot. Where the last
// instruction wrote keep, the entry just popped, it is emitted again after
// the stores: they write only the slots of positions below keep's, which it
// does not read, so that keep's instruction is still the last.
func (c *regCompiler) flushAll(keep *rentry) {
	if keep == nil || keep.def < 0 || keep.def != c.last || c.wide(c.last) {
		for h := range c.stack {
			c.materialize(h)
		}
		return
	}
	w, at := c.out.code[c.last], c.out.offs[c.last]
	c.out.code, c.out.offs = c.out.code[:c.last], c.out.offs[:c.last]
	c.last = -1
	for h := range c.stack {
		c.materialize(h)
	}
	keep.def = len(c.out.code)
	c.out.code = append(c.out.code, w)
	c.out.offs = append(c.out.offs, at)
	c.last = keep.def
}

// wide reports whether the instruction at pc is two words.
func (c *regCompiler) wide(pc int) bool { return uint8(c.out.code[pc]) == rConst64 }

// retarget makes entry e's instruction write slot s instead, reporting
// whether it could: e must be the value the last instruction wrote, with
// nothing on the stack reading s in place.
func (c *regCompiler) retarget(e rentry, s, local int) bool {
	if e.def < 0 || e.def != c.last {
		return false
	}
	if local >= 0 {
		for _, o := range c.stack {
			if o.local == local {
				return false
			}
		}
	}
	w := c.out.code[e.def]
	c.out.code[e.def] = w&^(0xffff<<16) | s16(s)<<16
	return true
}

func (c *regCompiler) top() *rctl { return c.ctrl[len(c.ctrl)-1] }

// instr compiles one instruction.
func (c *regCompiler) instr() {
	c.at = uint32(c.r.pc)
	op := c.r.opcode()
	switch op {
	case opcode.Unreachable:
		c.emit(rExt, xUnreachable, 0, 0, 0)
		c.markDead()
	case opcode.Nop:
	case opcode.Block, opcode.Loop:
		params, results := c.r.blockType(c.m)
		c.numeric(params)
		c.numeric(results)
		c.flushAll(nil)
		t := &rctl{op: op, base: len(c.stack) - len(params), params: params, results: results}
		if op == opcode.Loop {
			t.head = len(c.out.code)
			if _, ok := c.out.loops[int32(t.head)]; !ok {
				c.out.loops[int32(t.head)] = c.at
			}
			// The head is where branches land: nothing before it may be
			// rewritten into what follows it.
			c.last = -1
		}
		c.ctrl = append(c.ctrl, t)
	case opcode.If:
		params, results := c.r.blockType(c.m)
		c.numeric(params)
		c.numeric(results)
		cond := c.pop()
		c.flushAll(&cond)
		t := &rctl{op: op, base: len(c.stack) - len(params), params: params, results: results}
		t.elseFix = append(t.elseFix, c.branchIf(cond, false, len(c.stack)))
		c.ctrl = append(c.ctrl, t)
	case opcode.Else:
		t := c.top()
		c.fallOut(t)
		if !t.dead {
			t.fixes = append(t.fixes, c.jump(rJmp, 0))
		}
		c.bind(t.elseFix)
		t.elseFix, t.hasElse, t.dead = nil, true, false
		for i, pt := range t.params {
			c.pushSlot(pt, t.base+i)
		}
	case opcode.End:
		c.end()
	case opcode.Br:
		c.br(int(c.r.u32()))
	case opcode.BrIf:
		c.brIf(int(c.r.u32()))
	case opcode.BrTable:
		c.brTable()
	case opcode.Return:
		c.ret(c.stack[len(c.stack)-len(c.results):])
		c.markDead()
	case opcode.CallFunction:
		c.call(c.r.u32(), false)
	case opcode.ReturnCall:
		c.call(c.r.u32(), true)
	case opcode.Drop:
		c.pop()
	case opcode.Select:
		c.selectOp()
	case opcode.SelectWithType:
		for n := c.r.u32(); n > 0; n-- {
			c.numeric([]module.ValueType{c.r.valueType()})
		}
		c.selectOp()
	case opcode.LocalGet:
		k := int(c.r.u32())
		c.push(rentry{t: c.types[k], slot: c.locals[k].i, local: k, def: -1})
	case opcode.LocalSet, opcode.LocalTee:
		k := int(c.r.u32())
		e := c.pop()
		s := c.locals[k].i
		if !c.retarget(e, s, k) {
			c.flushLocal(k)
			c.store(e, s)
		}
		if op == opcode.LocalTee {
			c.push(rentry{t: e.t, slot: s, local: k, def: -1})
		}
	case opcode.GlobalGet:
		idx := c.r.u32()
		t := c.m.Globals[idx].Type
		c.numeric([]module.ValueType{t})
		rop := rGlobalGet64
		if t.Kind == module.KindI32 || t.Kind == module.KindF32 {
			rop = rGlobalGet32
		}
		h := len(c.stack)
		c.pushDef(t, h, c.emitBC(rop, 0, s16(c.numSlot(h)), idx))
	case opcode.GlobalSet:
		idx := c.r.u32()
		e := c.pop()
		c.numeric([]module.ValueType{e.t})
		c.emitBC(rGlobalSet, 0, c.read(e, len(c.stack)), idx)
	case opcode.I32Const:
		c.pushConst(module.I32, uint64(uint32(c.r.s32())))
	case opcode.I64Const:
		c.pushConst(module.I64, uint64(c.r.s64()))
	case opcode.F32Const:
		c.pushConst(module.F32, uint64(c.r.fixed32()))
	case opcode.F64Const:
		c.pushConst(module.F64, c.r.fixed64())
	case opcode.MemorySize:
		c.memory0(c.r.u32())
		h := len(c.stack)
		c.pushDef(module.I32, h, c.emit(rExt, xMemorySize, s16(c.numSlot(h)), 0, 0))
	case opcode.MemoryGrow:
		c.memory0(c.r.u32())
		d := c.pop()
		h := len(c.stack)
		ds := c.read(d, h)
		c.pushDef(module.I32, h, c.emit(rExt, xMemoryGrow, s16(c.numSlot(h)), ds, 0))
	default:
		if n, ok := regNumeric(op); ok {
			if n.unary {
				c.unary(n)
			} else {
				c.binary(op, n)
			}
			return
		}
		if rop, store, ok := regMemOp(op); ok {
			c.access(op, rop, store)
			return
		}
		panic(regBail(op.Name()))
	}
}

// memory0 bails out of an instruction of a memory other than memory 0, or of
// a 64-bit memory.
func (c *regCompiler) memory0(mem uint32) {
	if mem != 0 || c.m.Memories[0].Limits.Is64 {
		panic(regBail("a memory other than an i32 memory 0"))
	}
}

// access compiles a load or a store of memory 0.
func (c *regCompiler) access(op opcode.Opcode, rop regOp, store bool) {
	mem, off := c.r.memarg()
	c.memory0(mem)
	if off > math.MaxUint16 {
		panic(regBail("a memory offset past 65,535"))
	}
	if store {
		v := c.pop()
		a := c.pop()
		h := len(c.stack)
		as := c.read(a, h)
		vs := c.read(v, h+1)
		c.emit(rop, 0, as, vs, off)
		return
	}
	a := c.pop()
	h := len(c.stack)
	as := c.read(a, h)
	t := module.I32
	switch op {
	case opcode.I64LoadMem, opcode.I64LoadMem8S, opcode.I64LoadMem8U, opcode.I64LoadMem16S,
		opcode.I64LoadMem16U, opcode.I64LoadMem32S, opcode.I64LoadMem32U:
		t = module.I64
	case opcode.F32LoadMem:
		t = module.F32
	case opcode.F64LoadMem:
		t = module.F64
	}
	c.pushDef(t, h, c.emit(rop, 0, s16(c.numSlot(h)), as, off))
}

// fits16 reports whether v, of type t, is a 16-bit immediate sign-extended.
func fits16(v uint64, t module.ValueType) bool {
	if t.Kind == module.KindI64 {
		return int64(v) == int64(int16(v))
	}
	return uint32(v) == uint32(int32(int16(v)))
}

func (c *regCompiler) binary(op opcode.Opcode, n regNum) {
	y := c.pop()
	x := c.pop()
	h := len(c.stack)
	dst := s16(c.numSlot(h))
	e := rentry{t: n.res, slot: c.numSlot(h), local: -1}
	switch {
	case n.imm != 0 && y.konst && !x.konst && fits16(y.c, x.t):
		e.def = c.emit(n.imm, 0, dst, s16(x.slot), uint64(uint16(y.c)))
		e.cmp, e.cmpImm = c.cmpOf(op, n), true
	case n.imm != 0 && x.konst && !y.konst && fits16(x.c, y.t) && (n.comm || n.br != 0):
		if n.br != 0 {
			op = n.swap
			n, _ = regNumeric(op)
		}
		e.def = c.emit(n.imm, 0, dst, s16(y.slot), uint64(uint16(x.c)))
		e.cmp, e.cmpImm = c.cmpOf(op, n), true
	default:
		xs := c.read(x, h)
		ys := c.read(y, h+1)
		e.def = c.emitNum(n.op, n.ext, dst, xs, ys)
		e.cmp = c.cmpOf(op, n)
	}
	c.push(e)
}

// cmpOf returns op where it is a comparison that fuses with a branch.
func (c *regCompiler) cmpOf(op opcode.Opcode, n regNum) opcode.Opcode {
	if n.br != 0 && !c.far {
		return op
	}
	return 0
}

func (c *regCompiler) unary(n regNum) {
	x := c.pop()
	if n.op == rMov {
		x.t = n.res
		x.cmp, x.eqz = 0, false
		c.push(x)
		return
	}
	h := len(c.stack)
	xs := c.read(x, h)
	e := rentry{t: n.res, slot: c.numSlot(h), local: -1}
	e.def = c.emitNum(n.op, n.ext, s16(c.numSlot(h)), xs, 0)
	e.eqz = n.op == rI32Eqz || n.op == rI64Eqz
	c.push(e)
}

func (c *regCompiler) selectOp() {
	cond := c.pop()
	b := c.pop()
	a := c.pop()
	h := len(c.stack)
	cs := c.read(cond, h+2)
	bs := c.read(b, h+1)
	dst := c.numSlot(h)
	c.store(a, dst)
	c.emit(rSelect, 0, s16(dst), bs, cs)
	c.pushSlot(a.t, h)
}

// jump emits branch op of condition slot a to a target not known yet.
func (c *regCompiler) jump(op regOp, a uint64) rfix {
	return rfix{pc: c.emitBC(op, 0, a, 0), kind: fixRel32}
}

// branchIf emits a branch taken where cond, at position h, is nonzero (when)
// or zero (!when), to a target not known yet. A comparison or an eqz the
// last instruction computed becomes the branch.
func (c *regCompiler) branchIf(cond rentry, when bool, h int) rfix {
	if cond.def >= 0 && cond.def == c.last {
		w := c.out.code[c.last]
		b, cc := w>>32&0xffff, w>>48
		switch {
		case cond.cmp != 0:
			op := cond.cmp
			if !when {
				n, _ := regNumeric(op)
				op = n.neg
			}
			n, _ := regNumeric(op)
			rop := n.br
			if cond.cmpImm {
				rop = n.brI
			}
			c.out.code[c.last] = uint64(rop) | b<<16 | cc<<32
			return rfix{pc: c.last, kind: fixRel16}
		case cond.eqz:
			rop := rBrNZ
			if when {
				rop = rBrZ
			}
			c.out.code[c.last] = uint64(rop) | b<<16
			return rfix{pc: c.last, kind: fixRel32}
		}
	}
	rop := rBrZ
	if when {
		rop = rBrNZ
	}
	return c.jump(rop, c.read(cond, h))
}

// patch points branch x at target.
func (c *regCompiler) patch(x rfix, target int) {
	rel := target - x.pc
	switch x.kind {
	case fixRel16:
		if rel != int(int16(rel)) {
			c.retry = true
			return
		}
		c.out.code[x.pc] |= uint64(uint16(rel)) << 48
	case fixRel32:
		c.out.code[x.pc] |= uint64(uint32(int32(rel))) << 32
	case fixTable:
		c.out.tables[x.pc][x.idx] = int32(target)
	}
}

// bind points branches xs at the next instruction, where they land: nothing
// before it may be rewritten into what follows.
func (c *regCompiler) bind(xs []rfix) {
	for _, x := range xs {
		c.patch(x, len(c.out.code))
	}
	if len(xs) > 0 {
		c.last = -1
	}
}

// branchTo points branch x at construct t's label: a loop's head now, a
// block's end when it is compiled.
func (c *regCompiler) branchTo(t *rctl, x rfix) {
	if t.op == opcode.Loop {
		c.patch(x, t.head)
		return
	}
	t.fixes = append(t.fixes, x)
}

// moves stores the values a branch to t carries, es, into t's slots,
// reporting whether any needed a store.
func (c *regCompiler) moves(t *rctl, es []rentry, emit bool) bool {
	any := false
	for i, e := range es {
		s := c.numSlot(t.base + i)
		if e.konst || e.slot != s {
			any = true
			if emit {
				c.store(e, s)
			}
		}
	}
	return any
}

func (c *regCompiler) br(depth int) {
	t := c.ctrl[len(c.ctrl)-1-depth]
	n := len(t.labelTypes())
	es := c.stack[len(c.stack)-n:]
	if depth == len(c.ctrl)-1 {
		c.ret(es)
	} else {
		c.moves(t, es, true)
		c.branchTo(t, c.jump(rJmp, 0))
	}
	c.markDead()
}

func (c *regCompiler) brIf(depth int) {
	cond := c.pop()
	h := len(c.stack)
	t := c.ctrl[len(c.ctrl)-1-depth]
	n := len(t.labelTypes())
	es := c.stack[len(c.stack)-n:]
	if depth == len(c.ctrl)-1 {
		skip := c.branchIf(cond, false, h)
		c.ret(es)
		c.bind([]rfix{skip})
		return
	}
	if !c.moves(t, es, false) {
		c.branchTo(t, c.branchIf(cond, true, h))
		return
	}
	skip := c.branchIf(cond, false, h)
	c.moves(t, es, true)
	c.branchTo(t, c.jump(rJmp, 0))
	c.bind([]rfix{skip})
}

func (c *regCompiler) brTable() {
	n := c.r.u32()
	depths := make([]int, n+1)
	for i := range depths {
		depths[i] = int(c.r.u32())
	}
	idx := c.pop()
	h := len(c.stack)
	is := c.read(idx, h)
	ti := len(c.out.tables)
	table := make([]int32, len(depths))
	c.out.tables = append(c.out.tables, table)
	c.emitBC(rBrTable, 0, is, uint32(ti))
	arity := len(c.ctrl[len(c.ctrl)-1-depths[n]].labelTypes())
	es := c.stack[len(c.stack)-arity:]
	// A target the values need moving to, or the function's, is reached
	// through a stub after the table, which moves them and branches.
	type stub struct{ depth, pc int }
	var stubs []stub
	for i, d := range depths {
		t := c.ctrl[len(c.ctrl)-1-d]
		if d == len(c.ctrl)-1 || c.moves(t, es, false) {
			stubs = append(stubs, stub{d, i})
			continue
		}
		c.branchTo(t, rfix{pc: ti, kind: fixTable, idx: i})
	}
	done := map[int]int{}
	for _, s := range stubs {
		if pc, ok := done[s.depth]; ok {
			table[s.pc] = int32(pc)
			continue
		}
		pc := len(c.out.code)
		done[s.depth] = pc
		table[s.pc] = int32(pc)
		t := c.ctrl[len(c.ctrl)-1-s.depth]
		if s.depth == len(c.ctrl)-1 {
			c.ret(es)
			continue
		}
		c.moves(t, es, true)
		c.branchTo(t, c.jump(rJmp, 0))
	}
	c.markDead()
}

// ret returns the function's results es, which stay on the stack.
func (c *regCompiler) ret(es []rentry) {
	switch len(es) {
	case 0:
		c.emit(rRet, 0, 0, 0, 0)
		return
	case 1:
		e := es[0]
		switch {
		case e.konst:
			c.loadConst(0, e.t, e.c)
		case c.retarget(e, 0, -1):
		default:
			c.emit(rRet1, 0, s16(e.slot), 0, 0)
			return
		}
		c.emit(rRet, 0, 0, 0, 0)
		return
	}
	// The results go to the locals' first slots, which another result may
	// be read from: each goes to its position's slot first. The stack is
	// left as it is, for a br_if's path that does not return.
	base := len(c.stack) - len(es)
	for i, e := range es {
		c.store(e, c.numSlot(base+i))
	}
	dst, _, _ := layout(c.results)
	for i := range es {
		if s := c.numSlot(base + i); s != dst[i].i {
			c.emit(rMov, 0, s16(dst[i].i), s16(s), 0)
		}
	}
	c.emit(rRet, 0, 0, 0, 0)
}

// fallOut stores a construct's results, on top of the stack, into its
// positions' slots, and empties the stack to its base.
func (c *regCompiler) fallOut(t *rctl) {
	if !t.dead {
		n := len(t.results)
		c.moves(t, c.stack[len(c.stack)-n:], true)
	}
	c.stack = c.stack[:t.base]
}

func (c *regCompiler) end() {
	t := c.top()
	if len(c.ctrl) == 1 {
		if !t.dead {
			c.ret(c.stack[len(c.stack)-len(t.results):])
		}
		c.ctrl = c.ctrl[:0]
		return
	}
	c.fallOut(t)
	c.ctrl = c.ctrl[:len(c.ctrl)-1]
	c.bind(t.fixes)
	c.bind(t.elseFix)
	if t.op == opcode.If && !t.hasElse || t.op == opcode.Loop {
		c.last = -1
	}
	if c.top().dead {
		return
	}
	for i, rt := range t.results {
		c.pushSlot(rt, t.base+i)
	}
}

// markDead says the rest of the innermost construct cannot be reached, and
// skips its instructions.
func (c *regCompiler) markDead() {
	t := c.top()
	t.dead = true
	c.stack = c.stack[:t.base]
	c.r.skipDead()
}

// call compiles a call of function idx, or a tail call.
func (c *regCompiler) call(idx uint32, tail bool) {
	sig := c.m.FuncType(idx)
	c.numeric(sig.Params)
	c.numeric(sig.Results)
	n := len(sig.Params)
	base := len(c.stack) - n
	params, _, _ := layout(sig.Params)
	results, _, _ := layout(sig.Results)
	s := regSite{fn: idx, at: c.at}
	for i, e := range c.stack[base:] {
		s.args = append(s.args, regMove{from: uint32(c.read(e, base+i)), to: uint32(params[i].i)})
	}
	c.stack = c.stack[:base]
	if !tail {
		for i, r := range results {
			s.rets = append(s.rets, regMove{from: uint32(r.i), to: uint32(s16(c.numSlot(base + i)))})
		}
	}
	si := len(c.out.sites)
	c.out.sites = append(c.out.sites, s)
	if tail {
		c.emitBC(rReturnCall, 0, 0, uint32(si))
		c.markDead()
		return
	}
	c.emitBC(rCall, 0, 0, uint32(si))
	for i, t := range sig.Results {
		c.pushSlot(t, base+i)
	}
}
