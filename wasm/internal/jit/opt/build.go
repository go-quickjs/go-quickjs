package opt

import (
	"fmt"
	"math"

	wjit "github.com/go-quickjs/go-quickjs/wasm/internal/jit"
	"github.com/go-quickjs/go-quickjs/wasm/internal/module"
	"github.com/go-quickjs/go-quickjs/wasm/internal/opcode"
)

// The reasons this tier refuses a function, which then keeps its baseline
// code.
const (
	RefuseRef        = "ref"
	RefuseV128       = "v128"
	RefuseSIMD       = "simd"
	RefuseAtomics    = "atomics"
	RefuseGC         = "gc"
	RefuseExceptions = "exceptions"
	RefuseTail       = "tail"
	RefuseTable      = "table"
	RefuseBulk       = "bulk"
	RefuseMemory     = "memory"
	RefuseOffset     = "offset"
	RefuseOther      = "other"
	RefuseFrame      = "frame"
	RefuseSize       = "size"
)

func refuse(reason, format string, args ...any) {
	panic(&wjit.Refusal{Reason: reason, Detail: fmt.Sprintf(format, args...)})
}

// typeOf is the SSA type of a numeric value type, refusing others.
func typeOf(t module.ValueType, what string) Type {
	switch t.Kind {
	case module.KindI32:
		return TI32
	case module.KindI64:
		return TI64
	case module.KindF32:
		return TF32
	case module.KindF64:
		return TF64
	case module.KindV128:
		refuse(RefuseV128, "%s of type v128", what)
	}
	refuse(RefuseRef, "%s of a reference type", what)
	return TNone
}

// typesOf is the SSA types of ts, in the workspace's arena.
func (bd *builder) typesOf(ts []module.ValueType, what string) []Type {
	if len(ts) == 0 {
		return nil
	}
	out := bd.ws.types.take(len(ts))
	for _, t := range ts {
		out = append(out, typeOf(t, what))
	}
	return out
}

// checkNumeric refuses ts if one is not numeric, as typeOf does, for what,
// after op's name if named.
func checkNumeric(ts []module.ValueType, op opcode.Opcode, named bool, what string) {
	for _, t := range ts {
		switch t.Kind {
		case module.KindI32, module.KindI64, module.KindF32, module.KindF64:
		default:
			if named {
				what = op.Name() + " " + what
			}
			typeOf(t, what)
		}
	}
}

// ctl is a construct being built.
type ctl struct {
	op opcode.Opcode
	// base is the stack's height under the construct's parameters.
	base            int
	params, results []Type
	// label is where a branch to the construct goes: a loop's header or
	// the block after anything else; nil for the function's body, whose
	// branches return. phis are the label's values: a loop's parameters,
	// or the results of anything else.
	label *Block
	phis  []*Value
	// elseB is an if's else branch, entered with its parameters elseArgs.
	elseB    *Block
	elseArgs []*Value
	hasElse  bool
	// dead says the rest of the construct cannot be reached.
	dead bool
}

func (t *ctl) labelTypes() []Type {
	if t.op == opcode.Loop {
		return t.params
	}
	return t.results
}

// builder builds a function's SSA from its body, as Braun et al.'s "Simple
// and Efficient Construction of Static Single Assignment Form": each local
// is a variable whose value is looked up through the predecessors, a
// block's phis made as the lookup needs them, and a block sealed once every
// predecessor is known. The operand stack holds values; a label's values
// are phis made with it.
type builder struct {
	f      *Func
	ws     *workspace
	m      *module.Module
	r      reader
	locals []Type
	stack  []*Value
	ctrl   []*ctl
	b      *Block
	// zero is each type's zero, for the declared locals.
	zero [TTuple]*Value
	// globals are the values of the mutable globals known in the current
	// block: the last one read or written there since its start and its
	// last call.
	globals []globalFact
	// mem are the values of memory accesses known in the current block:
	// the last loads, and the stores of a whole value of its type.
	mem []memFact
	// globalAddrs are the globals' addresses, made in the entry block.
	globalAddrs map[uint32]*Value
	// depths and succOf are br_table's: its depths, and the successor
	// each depth is, or -1.
	depths []int
	succOf []int32
}

// globalFact is that global g holds val.
type globalFact struct {
	g   uint32
	val *Value
}

// reset empties the builder for the next function, keeping its memory.
func (bd *builder) reset() {
	clear(bd.stack[:cap(bd.stack)])
	clear(bd.ctrl[:cap(bd.ctrl)])
	clear(bd.globals[:cap(bd.globals)])
	clear(bd.mem[:cap(bd.mem)])
	clear(bd.globalAddrs)
	*bd = builder{locals: bd.locals[:0], stack: bd.stack[:0], ctrl: bd.ctrl[:0], globals: bd.globals[:0], mem: bd.mem[:0],
		globalAddrs: bd.globalAddrs, depths: bd.depths[:0], succOf: bd.succOf[:0]}
}

// memFact is that the bytes at addr + off, size of them, read as typ,
// are val.
type memFact struct {
	addr   *Value
	off    uint64
	size   uint8
	signed bool
	typ    Type
	val    *Value
}

// Build builds function fn of a validated module as SSA, or refuses it
// with a *jit.Refusal.
func Build(m *module.Module, fn uint32) (f *Func, err error) {
	defer func() {
		if p := recover(); p != nil {
			if r, ok := p.(*wjit.Refusal); ok {
				f, err = nil, r
				return
			}
			panic(p)
		}
	}()
	return build(new(workspace), m, fn), nil
}

// build builds function fn of a validated module as SSA in workspace ws,
// or panics with a *jit.Refusal.
func build(ws *workspace, m *module.Module, fn uint32) *Func {
	if m.Functions[fn].Imported {
		refuse(RefuseOther, "an imported function")
	}
	check(m, fn)
	lap(PhaseCheck)
	ws.reset()
	bd := &ws.bd
	bd.ws, bd.m, bd.f = ws, m, &ws.f
	bd.f.ws = ws
	// About an entry for every two bytes of the body.
	ws.defs.reset(int(m.Functions[fn].Code.Len) / 2)
	f := bd.build(fn)
	lap(PhaseBuild)
	return f
}

func (bd *builder) build(fn uint32) *Func {
	m, f := bd.m, bd.f
	sig := m.FuncType(fn)
	f.Params = bd.typesOf(sig.Params, "parameter")
	f.Results = bd.typesOf(sig.Results, "result")
	code := m.Functions[fn].Code
	bd.r = reader{b: m.Bytes, pc: int(code.Offset)}
	bd.locals = append(bd.locals, f.Params...)
	for n := bd.r.u32(); n > 0; n-- {
		count := bd.r.u32()
		t := typeOf(bd.r.valueType(), "local")
		for j := uint32(0); j < count; j++ {
			bd.locals = append(bd.locals, t)
		}
	}
	entry := f.newBlock()
	entry.sealed = true
	f.Entry = entry
	bd.b = entry
	for i, t := range f.Params {
		p := f.newValue(entry, OpParam, t)
		p.Aux = uint64(i)
		bd.writeVar(int32(i), entry, p)
	}
	for k := len(f.Params); k < len(bd.locals); k++ {
		t := bd.locals[k]
		z := bd.zero[t]
		if z == nil {
			z = f.newValue(entry, OpConst, t)
			bd.zero[t] = z
		}
		bd.writeVar(int32(k), entry, z)
	}
	body := bd.ws.ctls.new()
	*body = ctl{op: opcode.Block, results: f.Results}
	bd.ctrl = append(bd.ctrl, body)
	end := int(code.End())
	for bd.r.pc < end && len(bd.ctrl) > 0 {
		bd.instr()
	}
	removeUnreachable(f)
	return f
}

// Variables: the locals, by index.

// defKey is the workspace's key of local k's value at block b's end.
func defKey(k int32, b *Block) uint64 { return uint64(b.ID)<<32 | uint64(uint32(k)) }

func (bd *builder) writeVar(k int32, b *Block, v *Value) {
	bd.ws.defs.set(defKey(k, b), v)
}

func (bd *builder) readVar(k int32, b *Block) *Value {
	if v, ok := bd.ws.defs.get(defKey(k, b)); ok {
		return v
	}
	var v *Value
	switch {
	case !b.sealed:
		v = bd.f.newValue(b, OpPhi, bd.locals[k])
		b.incomplete = appendA(&bd.ws.incomplete, b.incomplete, incompletePhi{k, v})
	case len(b.Preds) == 1:
		v = bd.readVar(k, b.Preds[0])
	default:
		v = bd.f.newValue(b, OpPhi, bd.locals[k])
		bd.writeVar(k, b, v)
		bd.phiOperands(k, v)
	}
	bd.writeVar(k, b, v)
	return v
}

func (bd *builder) phiOperands(k int32, phi *Value) {
	for _, p := range phi.Block.Preds {
		a := bd.readVar(k, p)
		bd.f.addArg(phi, a)
	}
}

// seal says every predecessor of b is known, completing its phis.
func (bd *builder) seal(b *Block) {
	// Completing a phi may make more of b's: they are appended, and
	// completed in turn.
	for i := 0; i < len(b.incomplete); i++ {
		bd.phiOperands(b.incomplete[i].k, b.incomplete[i].phi)
	}
	b.incomplete = nil
	b.sealed = true
}

// The operand stack.

func (bd *builder) push(v *Value) { bd.stack = append(bd.stack, v) }

func (bd *builder) pop() *Value {
	v := bd.stack[len(bd.stack)-1]
	bd.stack = bd.stack[:len(bd.stack)-1]
	return v
}

func (bd *builder) top() *ctl { return bd.ctrl[len(bd.ctrl)-1] }

func (bd *builder) value(op Op, t Type, args ...*Value) *Value {
	return bd.f.newValue(bd.b, op, t, args...)
}

func (bd *builder) constant(t Type, bits uint64) *Value {
	v := bd.value(OpConst, t)
	v.Aux = bits
	return v
}

// newBlock begins a new block, forgetting what the last knew of globals
// and memory.
func (bd *builder) startBlock(b *Block) {
	bd.b = b
	bd.forget()
}

// forget forgets what is known of globals and memory: at a new block, or
// after a call, which may change them.
func (bd *builder) forget() {
	bd.globals = bd.globals[:0]
	bd.mem = bd.mem[:0]
}

// markDead says the rest of the innermost construct cannot be reached,
// and skips its instructions.
func (bd *builder) markDead() {
	t := bd.top()
	t.dead = true
	bd.stack = bd.stack[:t.base]
	bd.b = nil
	bd.r.skipDead()
}

// edge adds an edge from the current block to construct t's label, with
// vals, the label's values.
func (bd *builder) edge(from *Block, t *ctl, vals []*Value) {
	bd.f.addEdge(from, t.label)
	for i, p := range t.phis {
		bd.f.addArg(p, vals[i])
	}
}

// returnBlock makes block b return vals.
func (bd *builder) returnBlock(b *Block, vals []*Value) {
	b.Kind = BReturn
	b.Results = append(bd.ws.vals.take(len(vals)), vals...)
}

// branch ends the current block with a branch to depth.
func (bd *builder) branch(depth int) {
	t := bd.ctrl[len(bd.ctrl)-1-depth]
	n := len(t.labelTypes())
	vals := bd.stack[len(bd.stack)-n:]
	if t.label == nil {
		bd.returnBlock(bd.b, vals)
		return
	}
	bd.b.Kind = BPlain
	bd.edge(bd.b, t, vals)
}

func (bd *builder) brIf(depth int) {
	cond := bd.pop()
	t := bd.ctrl[len(bd.ctrl)-1-depth]
	n := len(t.labelTypes())
	vals := bd.stack[len(bd.stack)-n:]
	b := bd.b
	b.Kind = BIf
	b.Control = cond
	if t.label == nil {
		ret := bd.f.newBlock()
		ret.sealed = true
		bd.f.addEdge(b, ret)
		bd.returnBlock(ret, vals)
	} else {
		bd.edge(b, t, vals)
	}
	cont := bd.f.newBlock()
	bd.f.addEdge(b, cont)
	cont.sealed = true
	bd.startBlock(cont)
}

func (bd *builder) brTable() {
	n := bd.r.u32()
	depths := bd.depths[:0]
	for range n + 1 {
		depths = append(depths, int(bd.r.u32()))
	}
	bd.depths = depths
	idx := bd.pop()
	b := bd.b
	b.Kind = BSwitch
	b.Control = idx
	succ := bd.succOf
	for len(succ) < len(bd.ctrl) {
		succ = append(succ, -1)
	}
	bd.succOf = succ
	b.Table = bd.ws.i32.take(len(depths))
	for _, d := range depths {
		s := succ[d]
		if s < 0 {
			s = int32(len(b.Succs))
			succ[d] = s
			t := bd.ctrl[len(bd.ctrl)-1-d]
			vals := bd.stack[len(bd.stack)-len(t.labelTypes()):]
			if t.label == nil {
				ret := bd.f.newBlock()
				ret.sealed = true
				bd.f.addEdge(b, ret)
				bd.returnBlock(ret, vals)
			} else {
				bd.edge(b, t, vals)
			}
		}
		b.Table = append(b.Table, s)
	}
	for _, d := range depths {
		succ[d] = -1
	}
	bd.markDead()
}

// enter begins a construct whose label is label, with phis of types for
// its values.
func (bd *builder) enter(op opcode.Opcode, params, results []Type, label *Block) *ctl {
	t := bd.ws.ctls.new()
	*t = ctl{op: op, base: len(bd.stack) - len(params), params: params, results: results, label: label}
	if lts := t.labelTypes(); len(lts) > 0 {
		t.phis = bd.ws.vals.take(len(lts))
		for _, lt := range lts {
			t.phis = append(t.phis, bd.f.newValue(label, OpPhi, lt))
		}
	}
	bd.ctrl = append(bd.ctrl, t)
	return t
}

// fallthroughEnd ends the current block with its edge to t's label, its
// results on the stack.
func (bd *builder) fallthroughEnd(t *ctl) {
	if t.dead {
		return
	}
	n := len(t.results)
	bd.b.Kind = BPlain
	bd.edge(bd.b, t, bd.stack[len(bd.stack)-n:])
}

func (bd *builder) elseBegin() {
	t := bd.top()
	bd.fallthroughEnd(t)
	bd.stack = append(bd.stack[:t.base], t.elseArgs...)
	t.hasElse = true
	t.dead = false
	bd.startBlock(t.elseB)
}

// end ends the innermost construct.
func (bd *builder) end() {
	t := bd.top()
	if t.op == opcode.If && !t.hasElse {
		bd.elseBegin()
	}
	if len(bd.ctrl) == 1 {
		if !t.dead {
			bd.returnBlock(bd.b, bd.stack[len(bd.stack)-len(t.results):])
		}
		bd.ctrl = bd.ctrl[:0]
		return
	}
	bd.ctrl = bd.ctrl[:len(bd.ctrl)-1]
	if t.op == opcode.Loop {
		bd.seal(t.label)
		if t.dead {
			bd.deadAfter()
			return
		}
		// The loop's fallthrough leaves its results on the stack; the code
		// after it begins a block of its own.
		results := append(bd.ws.vals.take(len(t.results)), bd.stack[len(bd.stack)-len(t.results):]...)
		next := bd.f.newBlock()
		next.sealed = true
		bd.b.Kind = BPlain
		bd.f.addEdge(bd.b, next)
		bd.startBlock(next)
		bd.stack = append(bd.stack[:t.base], results...)
		return
	}
	bd.fallthroughEnd(t)
	bd.seal(t.label)
	bd.stack = bd.stack[:t.base]
	if len(t.label.Preds) == 0 {
		bd.deadAfter()
		return
	}
	bd.startBlock(t.label)
	bd.stack = append(bd.stack, t.phis...)
}

// deadAfter says the code after a construct that falls through nowhere
// cannot be reached.
func (bd *builder) deadAfter() {
	if len(bd.ctrl) > 0 {
		bd.markDead()
	}
}

// instr builds one instruction.
func (bd *builder) instr() {
	r := &bd.r
	at := uint32(r.pc)
	op := r.opcode()
	switch op {
	case opcode.Unreachable:
		bd.b.Kind = BTrap
		bd.b.At = at
		bd.markDead()
	case opcode.Nop:
	case opcode.Block:
		params, results := r.blockType(bd.m)
		label := bd.f.newBlock()
		bd.enter(op, bd.typesOf(params, "block parameter"), bd.typesOf(results, "block result"), label)
	case opcode.Loop:
		params, results := r.blockType(bd.m)
		header := bd.f.newBlock()
		t := bd.enter(op, bd.typesOf(params, "loop parameter"), bd.typesOf(results, "loop result"), header)
		bd.b.Kind = BPlain
		bd.edge(bd.b, t, bd.stack[t.base:])
		bd.startBlock(header)
		bd.stack = append(bd.stack[:t.base], t.phis...)
	case opcode.If:
		params, results := r.blockType(bd.m)
		cond := bd.pop()
		label := bd.f.newBlock()
		t := bd.enter(op, bd.typesOf(params, "if parameter"), bd.typesOf(results, "if result"), label)
		b := bd.b
		b.Kind = BIf
		b.Control = cond
		then := bd.f.newBlock()
		then.sealed = true
		t.elseB = bd.f.newBlock()
		t.elseB.sealed = true
		bd.f.addEdge(b, then)
		bd.f.addEdge(b, t.elseB)
		t.elseArgs = append(bd.ws.vals.take(len(bd.stack)-t.base), bd.stack[t.base:]...)
		bd.startBlock(then)
	case opcode.Else:
		bd.elseBegin()
	case opcode.End:
		bd.end()
	case opcode.Br:
		bd.branch(int(r.u32()))
		bd.markDead()
	case opcode.BrIf:
		bd.brIf(int(r.u32()))
	case opcode.BrTable:
		bd.brTable()
	case opcode.Return:
		bd.branch(len(bd.ctrl) - 1)
		bd.markDead()
	case opcode.CallFunction:
		idx := r.u32()
		bd.call(OpCall, bd.m.FuncType(idx), uint64(idx), 0, at, 0)
	case opcode.CallIndirect:
		typeIndex := r.u32()
		table := r.u32()
		bd.call(OpCallIndirect, bd.m.Types[typeIndex].Func, uint64(typeIndex), uint64(table), at, 1)
	case opcode.Drop:
		bd.pop()
	case opcode.Select, opcode.SelectWithType:
		if op == opcode.SelectWithType {
			for n := r.u32(); n > 0; n-- {
				typeOf(r.valueType(), "select")
			}
		}
		cond := bd.pop()
		y := bd.pop()
		x := bd.pop()
		bd.push(bd.value(OpSelect, x.Type, x, y, cond))
	case opcode.LocalGet:
		bd.push(bd.readVar(int32(r.u32()), bd.b))
	case opcode.LocalSet:
		bd.writeVar(int32(r.u32()), bd.b, bd.pop())
	case opcode.LocalTee:
		bd.writeVar(int32(r.u32()), bd.b, bd.stack[len(bd.stack)-1])
	case opcode.GlobalGet:
		bd.globalGet(r.u32())
	case opcode.GlobalSet:
		g := r.u32()
		v := bd.pop()
		s := bd.value(OpGlobalSet, TNone, bd.globalAddr(g), v)
		s.Aux = uint64(g)
		bd.knowGlobal(g, v)
	case opcode.I32Const:
		bd.push(bd.constant(TI32, uint64(uint32(r.s64()))))
	case opcode.I64Const:
		bd.push(bd.constant(TI64, uint64(r.s64())))
	case opcode.F32Const:
		bd.push(bd.constant(TF32, uint64(r.fixed32())))
	case opcode.F64Const:
		bd.push(bd.constant(TF64, r.fixed64()))
	case opcode.MemorySize:
		mem := r.u32()
		bd.goOp(op, uint64(mem), 0, bd.indexType(mem), at)
	case opcode.MemoryGrow:
		mem := r.u32()
		bd.goOp(op, uint64(mem), 1, bd.indexType(mem), at)
	case opcode.MemoryCopy:
		dst, src := r.u32(), r.u32()
		bd.goOp(op, uint64(dst)|uint64(src)<<32, 3, TNone, at)
	case opcode.MemoryFill:
		mem := r.u32()
		bd.goOp(op, uint64(mem), 3, TNone, at)
	default:
		if bd.memoryOp(op, at) || bd.numeric(op, at) {
			return
		}
		refuse(RefuseOther, "%s", op.Name())
	}
}

func (bd *builder) indexType(mem uint32) Type {
	if bd.m.Memories[mem].Limits.Is64 {
		return TI64
	}
	return TI32
}

// call builds a call of signature ft, its arguments on the stack, and
// extra values past them (call_indirect's index).
func (bd *builder) call(op Op, ft *module.FuncType, aux, aux2 uint64, at uint32, extra int) {
	n := len(ft.Params) + extra
	args := bd.stack[len(bd.stack)-n:]
	v := bd.value(op, TTuple, args...)
	v.Aux, v.Aux2, v.At = aux, aux2, at
	bd.stack = bd.stack[:len(bd.stack)-n]
	for i, rt := range ft.Results {
		t := typeOf(rt, "callee result")
		res := bd.value(OpResult, t, v)
		res.Aux = uint64(i)
		bd.push(res)
	}
	bd.forget()
}

// goOp builds an instruction Go runs, of nargs operands.
func (bd *builder) goOp(op opcode.Opcode, imm uint64, nargs int, result Type, at uint32) {
	args := bd.stack[len(bd.stack)-nargs:]
	v := bd.value(OpGoOp, result, args...)
	v.Aux, v.Aux2, v.At = uint64(op), imm, at
	bd.stack = bd.stack[:len(bd.stack)-nargs]
	if result != TNone {
		bd.push(v)
	}
	bd.forget()
}

func (bd *builder) globalGet(g uint32) {
	gl := &bd.m.Globals[g]
	t := typeOf(gl.Type, "global")
	for _, k := range bd.globals {
		if k.g == g {
			bd.push(k.val)
			return
		}
	}
	v := bd.value(OpGlobalGet, t, bd.globalAddr(g))
	v.Aux = uint64(g)
	if !gl.Mutable {
		// An immutable global's value never changes: the read is pure.
		v.Aux2 = 1
		bd.push(v)
		return
	}
	bd.knowGlobal(g, v)
	bd.push(v)
}

// knowGlobal knows that global g holds v in the current block.
func (bd *builder) knowGlobal(g uint32, v *Value) {
	for i := range bd.globals {
		if bd.globals[i].g == g {
			bd.globals[i].val = v
			return
		}
	}
	bd.globals = append(bd.globals, globalFact{g, v})
}

// globalAddr is global g's address, made once, in the entry block after
// the parameters, where it dominates every use.
func (bd *builder) globalAddr(g uint32) *Value {
	if v, ok := bd.globalAddrs[g]; ok {
		return v
	}
	e := bd.f.Entry
	v := bd.f.value(e, OpGlobalAddr, TI64, nil)
	v.Aux = uint64(g)
	i := 0
	for i < len(e.Values) && e.Values[i].Op == OpParam {
		i++
	}
	e.Values = appendA(&bd.ws.vals, e.Values, nil)
	copy(e.Values[i+1:], e.Values[i:])
	e.Values[i] = v
	if bd.globalAddrs == nil {
		bd.globalAddrs = map[uint32]*Value{}
	}
	bd.globalAddrs[g] = v
	return v
}

// memoryOp builds a load or a store, reporting whether op is one.
func (bd *builder) memoryOp(op opcode.Opcode, at uint32) bool {
	if op < opcode.I32LoadMem || op > opcode.I64StoreMem32 {
		return false
	}
	_, off := bd.r.memarg()
	var size uint8
	var t Type
	signed := false
	switch op {
	case opcode.I32LoadMem:
		size, t = 4, TI32
	case opcode.I64LoadMem:
		size, t = 8, TI64
	case opcode.F32LoadMem:
		size, t = 4, TF32
	case opcode.F64LoadMem:
		size, t = 8, TF64
	case opcode.I32LoadMem8S:
		size, t, signed = 1, TI32, true
	case opcode.I32LoadMem8U:
		size, t = 1, TI32
	case opcode.I32LoadMem16S:
		size, t, signed = 2, TI32, true
	case opcode.I32LoadMem16U:
		size, t = 2, TI32
	case opcode.I64LoadMem8S:
		size, t, signed = 1, TI64, true
	case opcode.I64LoadMem8U:
		size, t = 1, TI64
	case opcode.I64LoadMem16S:
		size, t, signed = 2, TI64, true
	case opcode.I64LoadMem16U:
		size, t = 2, TI64
	case opcode.I64LoadMem32S:
		size, t, signed = 4, TI64, true
	case opcode.I64LoadMem32U:
		size, t = 4, TI64
	case opcode.I32StoreMem8, opcode.I64StoreMem8:
		size = 1
	case opcode.I32StoreMem16, opcode.I64StoreMem16:
		size = 2
	case opcode.I32StoreMem, opcode.F32StoreMem, opcode.I64StoreMem32:
		size = 4
	default:
		size = 8
	}
	if off+uint64(size) > math.MaxInt32 {
		refuse(RefuseOffset, "%s at offset %d", op.Name(), off)
	}
	if op >= opcode.I32StoreMem {
		v := bd.pop()
		addr := bd.pop()
		s := bd.value(OpStore, TNone, addr, v)
		s.Aux, s.Size, s.At = off, size, at
		bd.stored(addr, off, size, v)
		return true
	}
	addr := bd.pop()
	for _, k := range bd.mem {
		if k.addr == addr && k.off == off && k.size == size && k.signed == signed && k.typ == t {
			bd.push(k.val)
			return true
		}
	}
	v := bd.value(OpLoad, t, addr)
	v.Aux, v.Size, v.Signed, v.At = off, size, signed, at
	bd.mem = append(bd.mem, memFact{addr: addr, off: off, size: size, signed: signed, typ: t, val: v})
	bd.push(v)
	return true
}

// stored forgets what is known of the bytes a store may write, and knows
// what a store of a whole value wrote: a later load of the same bytes, as
// the same type, reads v.
func (bd *builder) stored(addr *Value, off uint64, size uint8, v *Value) {
	keep := bd.mem[:0]
	for _, k := range bd.mem {
		// Accesses at one address value, the offsets apart, are disjoint;
		// accesses at constant addresses are disjoint where their bytes
		// are.
		disjoint := false
		if k.addr == addr {
			disjoint = k.off+uint64(k.size) <= off || off+uint64(size) <= k.off
		} else if k.addr.Op == OpConst && addr.Op == OpConst {
			a := uint64(uint32(k.addr.Aux)) + k.off
			b := uint64(uint32(addr.Aux)) + off
			disjoint = a+uint64(k.size) <= b || b+uint64(size) <= a
		}
		if disjoint {
			keep = append(keep, k)
		}
	}
	bd.mem = keep
	full := (v.Type == TI32 || v.Type == TF32) && size == 4 || (v.Type == TI64 || v.Type == TF64) && size == 8
	if full {
		bd.mem = append(bd.mem, memFact{addr: addr, off: off, size: size, typ: v.Type, val: v})
	}
}

// numeric builds a numeric instruction, reporting whether op is one.
func (bd *builder) numeric(op opcode.Opcode, at uint32) bool {
	var n numericOp
	if op < 256 {
		n = numerics1[op]
	} else {
		n = numerics[op]
	}
	if n.arity == 0 {
		return false
	}
	var buf [2]*Value
	args := buf[:n.arity]
	for i := n.arity - 1; i >= 0; i-- {
		args[i] = bd.pop()
	}
	v := bd.value(n.op, n.t, args...)
	if opInfos[n.op].flags&fTrap != 0 {
		v.At = at
	}
	bd.push(v)
	return true
}

type numericOp struct {
	op    Op
	t     Type
	arity int
}

// numerics are the numeric instructions: each one's operation, result
// type and number of operands.
var numerics = func() map[opcode.Opcode]numericOp {
	m := map[opcode.Opcode]numericOp{}
	bin := func(op Op, t Type, ops ...opcode.Opcode) {
		for _, o := range ops {
			m[o] = numericOp{op, t, 2}
		}
	}
	un := func(op Op, t Type, ops ...opcode.Opcode) {
		for _, o := range ops {
			m[o] = numericOp{op, t, 1}
		}
	}
	bin(OpAdd, TI32, opcode.I32Add)
	bin(OpSub, TI32, opcode.I32Sub)
	bin(OpMul, TI32, opcode.I32Mul)
	bin(OpAnd, TI32, opcode.I32And)
	bin(OpOr, TI32, opcode.I32Ior)
	bin(OpXor, TI32, opcode.I32Xor)
	bin(OpShl, TI32, opcode.I32Shl)
	bin(OpShrS, TI32, opcode.I32ShrS)
	bin(OpShrU, TI32, opcode.I32ShrU)
	bin(OpRotl, TI32, opcode.I32Rol)
	bin(OpRotr, TI32, opcode.I32Ror)
	bin(OpDivS, TI32, opcode.I32DivS)
	bin(OpDivU, TI32, opcode.I32DivU)
	bin(OpRemS, TI32, opcode.I32RemS)
	bin(OpRemU, TI32, opcode.I32RemU)
	bin(OpAdd, TI64, opcode.I64Add)
	bin(OpSub, TI64, opcode.I64Sub)
	bin(OpMul, TI64, opcode.I64Mul)
	bin(OpAnd, TI64, opcode.I64And)
	bin(OpOr, TI64, opcode.I64Ior)
	bin(OpXor, TI64, opcode.I64Xor)
	bin(OpShl, TI64, opcode.I64Shl)
	bin(OpShrS, TI64, opcode.I64ShrS)
	bin(OpShrU, TI64, opcode.I64ShrU)
	bin(OpRotl, TI64, opcode.I64Rol)
	bin(OpRotr, TI64, opcode.I64Ror)
	bin(OpDivS, TI64, opcode.I64DivS)
	bin(OpDivU, TI64, opcode.I64DivU)
	bin(OpRemS, TI64, opcode.I64RemS)
	bin(OpRemU, TI64, opcode.I64RemU)
	un(OpClz, TI32, opcode.I32Clz)
	un(OpCtz, TI32, opcode.I32Ctz)
	un(OpPopcnt, TI32, opcode.I32Popcnt)
	un(OpClz, TI64, opcode.I64Clz)
	un(OpCtz, TI64, opcode.I64Ctz)
	un(OpPopcnt, TI64, opcode.I64Popcnt)
	un(OpEqz, TI32, opcode.I32Eqz, opcode.I64Eqz)
	bin(OpEq, TI32, opcode.I32Eq, opcode.I64Eq)
	bin(OpNe, TI32, opcode.I32Ne, opcode.I64Ne)
	bin(OpLtS, TI32, opcode.I32LtS, opcode.I64LtS)
	bin(OpLtU, TI32, opcode.I32LtU, opcode.I64LtU)
	bin(OpGtS, TI32, opcode.I32GtS, opcode.I64GtS)
	bin(OpGtU, TI32, opcode.I32GtU, opcode.I64GtU)
	bin(OpLeS, TI32, opcode.I32LeS, opcode.I64LeS)
	bin(OpLeU, TI32, opcode.I32LeU, opcode.I64LeU)
	bin(OpGeS, TI32, opcode.I32GeS, opcode.I64GeS)
	bin(OpGeU, TI32, opcode.I32GeU, opcode.I64GeU)
	un(OpWrap, TI32, opcode.I32ConvertI64)
	un(OpExtendS, TI64, opcode.I64SConvertI32)
	un(OpExtendU, TI64, opcode.I64UConvertI32)
	un(OpExtend8S, TI32, opcode.I32SExtendI8)
	un(OpExtend16S, TI32, opcode.I32SExtendI16)
	un(OpExtend8S, TI64, opcode.I64SExtendI8)
	un(OpExtend16S, TI64, opcode.I64SExtendI16)
	un(OpExtend32S, TI64, opcode.I64SExtendI32)
	for _, t := range []Type{TF32, TF64} {
		pick := func(a, b opcode.Opcode) opcode.Opcode {
			if t == TF32 {
				return a
			}
			return b
		}
		bin(OpFAdd, t, pick(opcode.F32Add, opcode.F64Add))
		bin(OpFSub, t, pick(opcode.F32Sub, opcode.F64Sub))
		bin(OpFMul, t, pick(opcode.F32Mul, opcode.F64Mul))
		bin(OpFDiv, t, pick(opcode.F32Div, opcode.F64Div))
		bin(OpFMin, t, pick(opcode.F32Min, opcode.F64Min))
		bin(OpFMax, t, pick(opcode.F32Max, opcode.F64Max))
		bin(OpFCopysign, t, pick(opcode.F32CopySign, opcode.F64CopySign))
		un(OpFAbs, t, pick(opcode.F32Abs, opcode.F64Abs))
		un(OpFNeg, t, pick(opcode.F32Neg, opcode.F64Neg))
		un(OpFSqrt, t, pick(opcode.F32Sqrt, opcode.F64Sqrt))
		un(OpFCeil, t, pick(opcode.F32Ceil, opcode.F64Ceil))
		un(OpFFloor, t, pick(opcode.F32Floor, opcode.F64Floor))
		un(OpFTrunc, t, pick(opcode.F32Trunc, opcode.F64Trunc))
		un(OpFNearest, t, pick(opcode.F32NearestInt, opcode.F64NearestInt))
		bin(OpFEq, TI32, pick(opcode.F32Eq, opcode.F64Eq))
		bin(OpFNe, TI32, pick(opcode.F32Ne, opcode.F64Ne))
		bin(OpFLt, TI32, pick(opcode.F32Lt, opcode.F64Lt))
		bin(OpFGt, TI32, pick(opcode.F32Gt, opcode.F64Gt))
		bin(OpFLe, TI32, pick(opcode.F32Le, opcode.F64Le))
		bin(OpFGe, TI32, pick(opcode.F32Ge, opcode.F64Ge))
	}
	un(OpDemote, TF32, opcode.F32ConvertF64)
	un(OpPromote, TF64, opcode.F64ConvertF32)
	un(OpReinterpret, TI32, opcode.I32ReinterpretF32)
	un(OpReinterpret, TI64, opcode.I64ReinterpretF64)
	un(OpReinterpret, TF32, opcode.F32ReinterpretI32)
	un(OpReinterpret, TF64, opcode.F64ReinterpretI64)
	un(OpConvertS, TF32, opcode.F32SConvertI32, opcode.F32SConvertI64)
	un(OpConvertU, TF32, opcode.F32UConvertI32, opcode.F32UConvertI64)
	un(OpConvertS, TF64, opcode.F64SConvertI32, opcode.F64SConvertI64)
	un(OpConvertU, TF64, opcode.F64UConvertI32, opcode.F64UConvertI64)
	un(OpTruncS, TI32, opcode.I32SConvertF32, opcode.I32SConvertF64)
	un(OpTruncU, TI32, opcode.I32UConvertF32, opcode.I32UConvertF64)
	un(OpTruncS, TI64, opcode.I64SConvertF32, opcode.I64SConvertF64)
	un(OpTruncU, TI64, opcode.I64UConvertF32, opcode.I64UConvertF64)
	un(OpTruncSatS, TI32, opcode.I32SConvertSatF32, opcode.I32SConvertSatF64)
	un(OpTruncSatU, TI32, opcode.I32UConvertSatF32, opcode.I32UConvertSatF64)
	un(OpTruncSatS, TI64, opcode.I64SConvertSatF32, opcode.I64SConvertSatF64)
	un(OpTruncSatU, TI64, opcode.I64UConvertSatF32, opcode.I64UConvertSatF64)
	return m
}()

// numerics1 are numerics' of one byte, by opcode.
var numerics1 = func() (t [256]numericOp) {
	for op, n := range numerics {
		if op < 256 {
			t[op] = n
		}
	}
	return t
}()

// check refuses function fn if its body has an instruction this tier does
// not build, or a memory access it does not compile.
func check(m *module.Module, fn uint32) {
	code := m.Functions[fn].Code
	r := reader{b: m.Bytes, pc: int(code.Offset)}
	for n := r.u32(); n > 0; n-- {
		r.u32()
		typeOf(r.valueType(), "local")
	}
	end := int(code.End())
	for r.pc < end {
		op := r.opcode()
		switch op.Prefix() {
		case opcode.PrefixSIMD:
			refuse(RefuseSIMD, "%s", op.Name())
		case opcode.PrefixAtomic:
			refuse(RefuseAtomics, "%s", op.Name())
		case opcode.PrefixGC:
			refuse(RefuseGC, "%s", op.Name())
		case opcode.PrefixAsmJS:
			refuse(RefuseOther, "%s", op.Name())
		}
		switch op {
		case opcode.Try, opcode.TryTable, opcode.Catch, opcode.CatchAll, opcode.Throw, opcode.ThrowRef,
			opcode.Rethrow, opcode.Delegate:
			refuse(RefuseExceptions, "%s", op.Name())
		case opcode.ReturnCall, opcode.ReturnCallIndirect, opcode.ReturnCallRef:
			refuse(RefuseTail, "%s", op.Name())
		case opcode.CallRef, opcode.RefNull, opcode.RefIsNull, opcode.RefFunc, opcode.RefAsNonNull,
			opcode.RefEq, opcode.BrOnNull, opcode.BrOnNonNull:
			refuse(RefuseRef, "%s", op.Name())
		case opcode.TableGet, opcode.TableSet, opcode.TableSize, opcode.TableGrow, opcode.TableFill,
			opcode.TableCopy, opcode.TableInit, opcode.ElemDrop:
			refuse(RefuseTable, "%s", op.Name())
		case opcode.MemoryInit, opcode.DataDrop:
			refuse(RefuseBulk, "%s", op.Name())
		case opcode.F32LoadMemF16, opcode.F32StoreMemF16:
			refuse(RefuseOther, "%s", op.Name())
		case opcode.Block, opcode.Loop, opcode.If:
			params, results := r.blockType(m)
			checkNumeric(params, op, true, "parameter")
			checkNumeric(results, op, true, "result")
			continue
		case opcode.CallFunction:
			ft := m.FuncType(r.u32())
			checkNumeric(ft.Params, op, false, "callee parameter")
			checkNumeric(ft.Results, op, false, "callee result")
			continue
		case opcode.CallIndirect:
			ft := m.Types[r.u32()].Func
			r.u32()
			checkNumeric(ft.Params, op, false, "callee parameter")
			checkNumeric(ft.Results, op, false, "callee result")
			continue
		case opcode.GlobalGet, opcode.GlobalSet:
			typeOf(m.Globals[r.u32()].Type, "global")
			continue
		case opcode.SelectWithType:
			for n := r.u32(); n > 0; n-- {
				typeOf(r.valueType(), "select")
			}
			continue
		case opcode.MemorySize, opcode.MemoryGrow, opcode.MemoryFill:
			checkMemory(m, r.u32(), op, false)
			continue
		case opcode.MemoryCopy:
			checkMemory(m, r.u32(), op, false)
			checkMemory(m, r.u32(), op, false)
			continue
		}
		if op >= opcode.I32LoadMem && op <= opcode.I64StoreMem32 {
			mem, _ := r.memarg()
			checkMemory(m, mem, op, true)
			continue
		}
		r.skipImmediates(op)
	}
}

// checkMemory refuses an instruction on memory mem that this tier does not
// compile: an access of a memory other than 0, of an i64 memory or of a
// shared one.
func checkMemory(m *module.Module, mem uint32, op opcode.Opcode, access bool) {
	mm := &m.Memories[mem]
	switch {
	case mm.Limits.Is64:
		refuse(RefuseMemory, "%s of an i64 memory", op.Name())
	case mm.Shared:
		refuse(RefuseMemory, "%s of a shared memory", op.Name())
	case access && mem != 0:
		refuse(RefuseMemory, "%s of memory %d", op.Name(), mem)
	}
}
