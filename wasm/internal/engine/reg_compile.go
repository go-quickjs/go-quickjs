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
//
// A value's slot is a numeric slot, the first of two for a v128, or a
// reference slot, by its type; position h's are numeric slot nlocals+2h and
// reference slot rlocals+h.

// regBail is what the register compiler panics with for a body it does not
// compile, which the closure tree then compiles: why.
type regBail string

// rentry is an entry of the symbolic operand stack.
type rentry struct {
	t     module.ValueType
	konst bool
	// c and hi are a constant's bits: a number's, a v128's low and high
	// halves; a reference constant is null.
	c, hi uint64
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
	// rec is, where def is a GC instruction, its record's index plus one:
	// its destination is the record's.
	rec int
}

// The kinds of a branch whose target is not known yet.
const (
	fixRel16 = iota // the offset in C
	fixRel32        // the offset in BC
	fixTable        // entry idx of table pc
	fixCatch        // clause idx of region pc's landing
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
	// region is a try_table's or a legacy try's region (regRegion), -1
	// for another construct; catching says a legacy try is in a catch.
	region   int
	catching bool
}

func (t *rctl) labelTypes() []module.ValueType {
	if t.op == opcode.Loop {
		return t.params
	}
	return t.results
}

// The marks of an instruction's field that names a slot, by its kind
// (field): emit checks it is near and strips the marks.
const (
	fNum  = 1 << 40
	fRef  = 1 << 41
	fV128 = 1 << 42
)

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
	rlocals   int
	results   []module.ValueType
	maxHeight int
	stack     []rentry
	ctrl      []*rctl
	out       regCode
	// refs says a reference was pushed; tries is the deepest nesting of
	// legacy try constructs, as the tree's compiler counts them.
	refs  bool
	tries int
	// retFixes are the catch clauses that land at the function's label.
	retFixes []rfix
	// at is the offset of the instruction being compiled; last the pc of
	// the last instruction emitted, -1 before the first or after a label;
	// farPCs the pcs of the instructions behind a far prefix, which are not
	// rewritten.
	at     uint32
	last   int
	farPCs map[int]bool
}

// regCompile compiles function index to register code, or returns why it
// did not.
func regCompile(code *Code, index uint32) (*compiled, string) {
	for _, far := range [2]bool{false, true} {
		c := &regCompiler{code: code, m: code.Module, fn: index, far: far, last: -1, farPCs: map[int]bool{}}
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
	f := &m.Functions[c.fn]
	sig := m.Types[f.TypeIndex].Func
	c.r = reader{b: m.Bytes, pc: int(f.Code.Offset)}
	end := int(f.Code.End())
	types := append([]module.ValueType(nil), sig.Params...)
	for n := c.r.u32(); n > 0; n-- {
		count := c.r.u32()
		t := c.r.valueType()
		for j := uint32(0); j < count; j++ {
			types = append(types, t)
		}
	}
	c.types = types
	c.locals, c.nlocals, c.rlocals = layout(types)
	_, rn, rr := layout(sig.Results)
	c.nlocals = max(c.nlocals, rn)
	c.rlocals = max(c.rlocals, rr)
	c.results = sig.Results
	c.out.loops = map[int32]uint32{}
	c.ctrl = append(c.ctrl, &rctl{results: sig.Results, region: -1})
	for c.r.pc < end && len(c.ctrl) > 0 {
		c.instr()
	}
	c.retStub()
	if len(c.out.code) == 0 {
		c.emit(rRet, 0, 0, 0, 0)
	}
	if uint64(len(c.out.code))+uint64(len(m.Bytes)) >= math.MaxUint32 {
		// A trap's pc is reported past the module's offsets (regAt), which
		// a module whose offsets do not fit 32 bits leaves no room for: no
		// part of the engine reports such a module's offsets.
		panic(regBail("a module past 4 GiB"))
	}
	p = newCompiled()
	p.nslots = c.nlocals + 2*(c.maxHeight+1)
	p.rslots = c.rlocals + c.maxHeight + 1 + c.tries
	p.nlocals, p.rlocals = c.nlocals, c.rlocals
	p.refs = c.refs || c.rlocals > 0 || c.tries > 0
	p.caught = c.rlocals + c.maxHeight + 1
	p.built = true
	out := c.out
	p.reg = &out
	p.params, _, _ = layout(sig.Params)
	p.results, _, _ = layout(sig.Results)
	return p, ""
}

func (c *regCompiler) numSlot(h int) int { return c.nlocals + 2*h }
func (c *regCompiler) refSlot(h int) int { return c.rlocals + h }

// posSlot returns position h's slot for a value of type t.
func (c *regCompiler) posSlot(t module.ValueType, h int) int {
	if t.IsRef() {
		return c.refSlot(h)
	}
	return c.numSlot(h)
}

// ns, nv and rf return slot s as an instruction's field: a number's, a
// v128's, a reference's; field the slot of a value of type t.
func ns(s int) uint64 { return uint64(s) | fNum }
func nv(s int) uint64 { return uint64(s) | fNum | fV128 }
func rf(s int) uint64 { return uint64(s) | fRef }

func field(t module.ValueType, s int) uint64 {
	switch {
	case t.IsRef():
		return rf(s)
	case t.Kind == module.KindV128:
		return nv(s)
	}
	return ns(s)
}

// strip returns a field without its marks, and whether it names a slot
// past the 16 bits a field holds.
func strip(f uint64) (uint64, bool) {
	if f&(fNum|fRef) == 0 {
		return f, false
	}
	s := f & math.MaxUint32
	limit := uint64(math.MaxUint16)
	if f&fV128 != 0 {
		limit--
	}
	return s, s > limit
}

// emit appends an instruction, and the words after it that some take, and
// returns its pc.
func (c *regCompiler) emit(op regOp, x uint8, a, b, cc uint64, ext ...uint64) int {
	return c.emitD(op, x, a, b, cc, 0, ext...)
}

// emitD is emit of an instruction with a fourth slot operand, d, in the
// word after it, before ext. An instruction naming a slot past what its
// field holds is emitted behind a far prefix (rFar): its slot fields name
// the frame's scratch slots, and the prefix the slots they stand for.
func (c *regCompiler) emitD(op regOp, x uint8, a, b, cc, d uint64, ext ...uint64) int {
	fields := [4]uint64{a, b, cc, d}
	far := false
	for _, f := range fields {
		if _, past := strip(f); past {
			far = true
		}
	}
	pc := len(c.out.code)
	if far {
		var kinds, slots [4]uint64
		for i, f := range fields {
			if f&(fNum|fRef) == 0 {
				continue
			}
			slots[i] = f & math.MaxUint32
			switch {
			case f&fRef != 0:
				kinds[i], fields[i] = 3, uint64(i)
			case f&fV128 != 0:
				kinds[i], fields[i] = 2, uint64(2*i)
			default:
				kinds[i], fields[i] = 1, uint64(2*i)
			}
		}
		k := kinds[0] | kinds[1]<<2 | kinds[2]<<4 | kinds[3]<<6
		c.out.code = append(c.out.code, uint64(rFar)|k<<16|slots[3]<<32, slots[0]|slots[1]<<32, slots[2])
		c.out.offs = append(c.out.offs, c.at, c.at, c.at)
		pc = len(c.out.code)
		c.farPCs[pc] = true
	} else {
		for i, f := range fields {
			fields[i], _ = strip(f)
		}
	}
	c.out.code = append(c.out.code, uint64(op)|uint64(x)<<8|fields[0]<<16|fields[1]<<32|fields[2]<<48)
	c.out.offs = append(c.out.offs, c.at)
	if d != 0 {
		ext = append([]uint64{fields[3]}, ext...)
	}
	for _, w := range ext {
		c.out.code = append(c.out.code, w)
		c.out.offs = append(c.out.offs, c.at)
	}
	c.last = pc
	return pc
}

// emitBC appends an instruction with a 32-bit BC.
func (c *regCompiler) emitBC(op regOp, x uint8, a uint64, bc uint32, ext ...uint64) int {
	return c.emit(op, x, a, uint64(bc), 0, ext...)
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
	if e.t.IsRef() {
		c.refs = true
	}
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

// pushSlot pushes the value of type t in position h's slot; pushDef the
// value instruction pc wrote there; pushNew the value the next instruction
// writes to the next position's slot, returning that slot's field.
func (c *regCompiler) pushSlot(t module.ValueType, h int) {
	c.push(rentry{t: t, slot: c.posSlot(t, h), local: -1, def: -1})
}

func (c *regCompiler) pushDef(t module.ValueType, h, pc int) {
	c.push(rentry{t: t, slot: c.posSlot(t, h), local: -1, def: pc})
}

func (c *regCompiler) pushConst(t module.ValueType, v uint64) {
	c.push(rentry{t: t, konst: true, c: v, local: -1, def: -1})
}

// dst returns the field of the slot of the value of type t an operation at
// position h writes; def pushes that value, written by instruction pc.
func (c *regCompiler) dst(t module.ValueType, h int) uint64 { return field(t, c.posSlot(t, h)) }

func (c *regCompiler) def(t module.ValueType, h, pc int) { c.pushDef(t, h, pc) }

// loadConst loads constant entry e into slot s.
func (c *regCompiler) loadConst(s int, e rentry) {
	switch {
	case e.t.IsRef():
		c.emit(rRefNull, 0, rf(s), 0, 0)
	case e.t.Kind == module.KindV128:
		c.emit(rConst128, 0, nv(s), 0, 0, e.c, e.hi)
	case e.c <= math.MaxUint32:
		c.emitBC(rConst32, 0, ns(s), uint32(e.c))
	case (e.t.Kind == module.KindI64 || e.t.Kind == module.KindF64) && int64(e.c) == int64(int32(e.c)):
		c.emitBC(rConst32S, 0, ns(s), uint32(e.c))
	default:
		c.emit(rConst64, 0, ns(s), 0, 0, e.c)
	}
}

// store stores entry e into slot s.
func (c *regCompiler) store(e rentry, s int) {
	switch {
	case e.konst:
		c.loadConst(s, e)
	case e.slot == s:
	case e.t.IsRef():
		c.emit(rRefMov, 0, rf(s), rf(e.slot), 0)
	case e.t.Kind == module.KindV128:
		c.emit(rMov2, 0, nv(s), nv(e.slot), 0)
	default:
		c.emit(rMov, 0, ns(s), ns(e.slot), 0)
	}
}

// read returns the field of the slot holding entry e, at position h: a
// constant is loaded into the position's slot.
func (c *regCompiler) read(e rentry, h int) uint64 {
	if e.konst {
		s := c.posSlot(e.t, h)
		c.loadConst(s, e)
		return field(e.t, s)
	}
	return field(e.t, e.slot)
}

// slotOf returns the slot holding entry e, at position h, as read does.
func (c *regCompiler) slotOf(e rentry, h int) int {
	if e.konst {
		s := c.posSlot(e.t, h)
		c.loadConst(s, e)
		return s
	}
	return e.slot
}

// materialize stores position h's entry into its slot, unless it is there.
func (c *regCompiler) materialize(h int) {
	e := c.stack[h]
	s := c.posSlot(e.t, h)
	if !e.konst && e.slot == s {
		c.stack[h].def = -1
		c.stack[h].local = -1
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
	if keep == nil || keep.def < 0 || keep.def != c.last || c.farPCs[c.last] {
		for h := range c.stack {
			c.materialize(h)
		}
		return
	}
	ws := append([]uint64(nil), c.out.code[c.last:]...)
	at := c.out.offs[c.last]
	c.out.code, c.out.offs = c.out.code[:c.last], c.out.offs[:c.last]
	c.last = -1
	for h := range c.stack {
		c.materialize(h)
	}
	keep.def = len(c.out.code)
	for _, w := range ws {
		c.out.code = append(c.out.code, w)
		c.out.offs = append(c.out.offs, at)
	}
	c.last = keep.def
}

// retarget makes entry e's instruction write slot s instead, reporting
// whether it could: e must be the value the last instruction wrote, with
// nothing on the stack reading local in place.
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
	if e.rec > 0 {
		c.out.gc[e.rec-1].dst = uint32(s)
		return true
	}
	if c.farPCs[e.def] {
		return false
	}
	a, far := strip(field(e.t, s))
	if far {
		return false
	}
	w := c.out.code[e.def]
	c.out.code[e.def] = w&^(0xffff<<16) | a<<16
	return true
}

func (c *regCompiler) top() *rctl { return c.ctrl[len(c.ctrl)-1] }

// begin pushes a construct whose parameters are on the stack, every entry
// in its position's slot first.
func (c *regCompiler) begin(op opcode.Opcode, params, results []module.ValueType, keep *rentry) *rctl {
	c.flushAll(keep)
	t := &rctl{op: op, base: len(c.stack) - len(params), params: params, results: results, region: -1}
	c.ctrl = append(c.ctrl, t)
	return t
}

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
		t := c.begin(op, params, results, nil)
		if op == opcode.Loop {
			t.head = len(c.out.code)
			if _, ok := c.out.loops[int32(t.head)]; !ok {
				c.out.loops[int32(t.head)] = c.at
			}
			// The head is where branches land: nothing before it may be
			// rewritten into what follows it.
			c.last = -1
		}
	case opcode.If:
		params, results := c.r.blockType(c.m)
		cond := c.pop()
		t := c.begin(op, params, results, &cond)
		t.elseFix = append(t.elseFix, c.branchIf(cond, false, len(c.stack)))
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
			c.r.valueType()
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
			c.push(rentry{t: c.types[k], slot: s, local: k, def: -1})
		}
	case opcode.GlobalGet:
		idx := c.r.u32()
		t := c.m.Globals[idx].Type
		h := len(c.stack)
		switch {
		case t.IsRef():
			c.def(t, h, c.emitBC(rExt, xGlobalGetRef, c.dst(t, h), idx))
		case t.Kind == module.KindV128:
			c.def(t, h, c.emitBC(rExt, xGlobalGet128, c.dst(t, h), idx))
		case t.Kind == module.KindI32 || t.Kind == module.KindF32:
			c.def(t, h, c.emitBC(rGlobalGet32, 0, c.dst(t, h), idx))
		default:
			c.def(t, h, c.emitBC(rGlobalGet64, 0, c.dst(t, h), idx))
		}
	case opcode.GlobalSet:
		idx := c.r.u32()
		e := c.pop()
		s := c.read(e, len(c.stack))
		switch {
		case e.t.IsRef():
			c.emitBC(rExt, xGlobalSetRef, s, idx)
		case e.t.Kind == module.KindV128:
			c.emitBC(rExt, xGlobalSet128, s, idx)
		default:
			c.emitBC(rGlobalSet, 0, s, idx)
		}
	case opcode.I32Const:
		c.pushConst(module.I32, uint64(uint32(c.r.s32())))
	case opcode.I64Const:
		c.pushConst(module.I64, uint64(c.r.s64()))
	case opcode.F32Const:
		c.pushConst(module.F32, uint64(c.r.fixed32()))
	case opcode.F64Const:
		c.pushConst(module.F64, c.r.fixed64())
	default:
		if n, ok := regNumeric(op); ok {
			if n.unary {
				c.unary(n)
			} else {
				c.binary(op, n)
			}
			return
		}
		if c.memoryOp(op) || c.refOp(op) || c.tableOp(op) || c.simdOp(op) || c.gcOp(op) || c.atomicOp(op) || c.exnOp(op) {
			return
		}
		panic(regBail(op.Name()))
	}
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
	dst := ns(c.numSlot(h))
	e := rentry{t: n.res, slot: c.numSlot(h), local: -1}
	switch {
	case n.imm != 0 && y.konst && !x.konst && fits16(y.c, x.t):
		e.def = c.emit(n.imm, 0, dst, ns(x.slot), uint64(uint16(y.c)))
		e.cmp, e.cmpImm = c.cmpOf(op, n), true
	case n.imm != 0 && x.konst && !y.konst && fits16(x.c, y.t) && (n.comm || n.br != 0):
		if n.br != 0 {
			op = n.swap
			n, _ = regNumeric(op)
		}
		e.def = c.emit(n.imm, 0, dst, ns(y.slot), uint64(uint16(x.c)))
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
	e.def = c.emitNum(n.op, n.ext, ns(c.numSlot(h)), xs, 0)
	e.eqz = !n.ext && (n.op == rI32Eqz || n.op == rI64Eqz)
	c.push(e)
}

func (c *regCompiler) selectOp() {
	cond := c.pop()
	b := c.pop()
	a := c.pop()
	h := len(c.stack)
	cs := c.read(cond, h+2)
	bs := c.read(b, h+1)
	dst := c.posSlot(a.t, h)
	c.store(a, dst)
	switch {
	case a.t.IsRef():
		c.emit(rExt, xSelectRef, rf(dst), bs, cs)
	case a.t.Kind == module.KindV128:
		c.emit(rExt, xSelect128, nv(dst), bs, cs)
	default:
		c.emit(rSelect, 0, ns(dst), bs, cs)
	}
	c.pushSlot(a.t, h)
}

// jump emits branch op of condition field a to a target not known yet.
func (c *regCompiler) jump(op regOp, a uint64) rfix {
	return rfix{pc: c.emitBC(op, 0, a, 0), kind: fixRel32}
}

// jumpExt emits a branch of the second table, of operand field a, to a
// target not known yet.
func (c *regCompiler) jumpExt(x uint8, a uint64) rfix {
	return rfix{pc: c.emitBC(rExt, x, a, 0), kind: fixRel32}
}

// branchIf emits a branch taken where cond, at position h, is nonzero (when)
// or zero (!when), to a target not known yet. A comparison or an eqz the
// last instruction computed becomes the branch.
func (c *regCompiler) branchIf(cond rentry, when bool, h int) rfix {
	if cond.def >= 0 && cond.def == c.last && !c.farPCs[c.last] {
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
	case fixCatch:
		c.out.regions[x.pc].clauses[x.idx].pc = int32(target)
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
		s := c.posSlot(e.t, t.base+i)
		if e.konst || e.slot != s {
			any = true
			if emit {
				c.store(e, s)
			}
		}
	}
	return any
}

// leaving reports whether a branch from the innermost construct to the one
// at index target leaves a legacy catch, whose exception it must drop; leave
// emits the drop.
func (c *regCompiler) leaving(target int) bool {
	for i := len(c.ctrl) - 1; i > target; i-- {
		if c.ctrl[i].catching {
			return true
		}
	}
	return false
}

func (c *regCompiler) leave(target int) {
	if target == 0 || !c.leaving(target) {
		return
	}
	k := 0
	for i := 1; i <= target; i++ {
		if c.ctrl[i].catching {
			k++
		}
	}
	c.emitBC(rExt, xCaughtTo, 0, uint32(k))
}

func (c *regCompiler) br(depth int) {
	ti := len(c.ctrl) - 1 - depth
	t := c.ctrl[ti]
	n := len(t.labelTypes())
	es := c.stack[len(c.stack)-n:]
	c.leave(ti)
	if ti == 0 {
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
	ti := len(c.ctrl) - 1 - depth
	t := c.ctrl[ti]
	n := len(t.labelTypes())
	es := c.stack[len(c.stack)-n:]
	if ti == 0 {
		skip := c.branchIf(cond, false, h)
		c.leave(0)
		c.ret(es)
		c.bind([]rfix{skip})
		return
	}
	if !c.moves(t, es, false) && !c.leaving(ti) {
		c.branchTo(t, c.branchIf(cond, true, h))
		return
	}
	skip := c.branchIf(cond, false, h)
	c.leave(ti)
	c.moves(t, es, true)
	c.branchTo(t, c.jump(rJmp, 0))
	c.bind([]rfix{skip})
}

// branchOut emits the moves and the branch that take values es, still on
// the stack, to the construct at index ti, from a path a branch has chosen:
// a return where ti is the function's.
func (c *regCompiler) branchOut(ti int, es []rentry) {
	c.leave(ti)
	if ti == 0 {
		c.ret(es)
		return
	}
	t := c.ctrl[ti]
	c.moves(t, es, true)
	c.branchTo(t, c.jump(rJmp, 0))
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
	// A target the values need moving to, or the function's, or one past a
	// legacy catch, is reached through a stub after the table, which moves
	// them and branches.
	type stub struct{ depth, pc int }
	var stubs []stub
	for i, d := range depths {
		k := len(c.ctrl) - 1 - d
		t := c.ctrl[k]
		if k == 0 || c.moves(t, es, false) || c.leaving(k) {
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
		c.branchOut(len(c.ctrl)-1-s.depth, es)
	}
	c.markDead()
}

// ret returns the function's results es, which stay on the stack.
func (c *regCompiler) ret(es []rentry) {
	dst, _, _ := layout(c.results)
	switch len(es) {
	case 0:
		c.emit(rRet, 0, 0, 0, 0)
		return
	case 1:
		e := es[0]
		switch {
		case e.konst:
			c.loadConst(dst[0].i, e)
		case c.retarget(e, dst[0].i, -1):
		case !e.t.IsRef() && e.t.Kind != module.KindV128 && e.slot <= math.MaxUint16:
			c.emit(rRet1, 0, ns(e.slot), 0, 0)
			return
		default:
			c.store(e, dst[0].i)
		}
		c.emit(rRet, 0, 0, 0, 0)
		return
	}
	// The results go to the locals' first slots, which another result may
	// be read from: each goes to its position's slot first. The stack is
	// left as it is, for a br_if's path that does not return.
	base := len(c.stack) - len(es)
	for i, e := range es {
		c.store(e, c.posSlot(e.t, base+i))
	}
	for i, e := range es {
		c.store(rentry{t: e.t, slot: c.posSlot(e.t, base+i), local: -1, def: -1}, dst[i].i)
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
	if t.op == opcode.TryTable || t.op == opcode.Try {
		c.endTry(t)
	} else {
		c.fallOut(t)
	}
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

// kindOf is how a value of type t is moved: one word, two, or a reference.
func kindOf(t module.ValueType) uint8 {
	switch {
	case t.IsRef():
		return vkRef
	case t.Kind == module.KindV128:
		return vkV128
	}
	return vkNum
}

// siteArgs pops the arguments of a call of signature sig and returns their
// moves into a callee's frame, and the position they began at.
func (c *regCompiler) siteArgs(sig *module.FuncType) ([]regMove, int) {
	n := len(sig.Params)
	base := len(c.stack) - n
	params, _, _ := layout(sig.Params)
	var args []regMove
	for i, e := range c.stack[base:] {
		args = append(args, regMove{from: uint32(c.slotOf(e, base+i)), to: uint32(params[i].i), k: kindOf(e.t)})
	}
	c.stack = c.stack[:base]
	return args, base
}

// siteRets returns the moves of a call's results out of the callee's frame
// into positions from base.
func (c *regCompiler) siteRets(sig *module.FuncType, base int) []regMove {
	results, _, _ := layout(sig.Results)
	var rets []regMove
	for i, r := range results {
		t := sig.Results[i]
		s := c.posSlot(t, base+i)
		rets = append(rets, regMove{from: uint32(r.i), to: uint32(s), k: kindOf(t)})
	}
	return rets
}

// call compiles a call of function idx, or a tail call.
func (c *regCompiler) call(idx uint32, tail bool) {
	sig := c.m.FuncType(idx)
	s := regSite{fn: idx, at: c.at}
	var base int
	s.args, base = c.siteArgs(sig)
	if !tail {
		s.rets = c.siteRets(sig, base)
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
