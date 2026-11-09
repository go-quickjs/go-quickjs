package engine

import (
	"math"

	"github.com/go-quickjs/go-quickjs/wasm/internal/module"
	"github.com/go-quickjs/go-quickjs/wasm/internal/opcode"
)

// instr compiles one instruction.
func (c *compiler) instr() {
	at := uint32(c.r.pc)
	op := c.r.opcode()
	switch op {
	case opcode.Unreachable:
		c.flushBelow(0)
		c.emit(func(*frame) int32 { trap(TrapUnreachable); return 0 })
		c.markDead()
	case opcode.Nop:
	case opcode.Block, opcode.Loop:
		params, results := c.r.blockType(c.m)
		c.enter(op, params, results)
	case opcode.If:
		params, results := c.r.blockType(c.m)
		cond := c.pop()
		c.flushBelow(0)
		// The condition is evaluated before the branches; it is the if's.
		t := c.enter(op, params, results)
		t.cond = c.i32(cond)
	case opcode.Else:
		c.elseBegin()
	case opcode.End:
		c.end()
	case opcode.Br:
		depth := int(c.r.u32())
		ss := c.branchValues(depth)
		c.emit(seqStmt(ss, int32(depth+1)))
		c.markDead()
	case opcode.BrIf:
		depth := int(c.r.u32())
		c.brIf(depth)
	case opcode.BrTable:
		c.brTable()
	case opcode.Return:
		ss := c.returnValues()
		c.emit(seqStmt(ss, sigReturn))
		c.markDead()
	case opcode.CallFunction:
		c.call(c.r.u32(), at, false)
	case opcode.ReturnCall:
		c.call(c.r.u32(), at, true)
	case opcode.CallIndirect, opcode.ReturnCallIndirect:
		typeIndex := c.r.u32()
		table := c.r.u32()
		c.callIndirect(typeIndex, table, at, op == opcode.ReturnCallIndirect)
	case opcode.Drop:
		e := c.pop()
		if e.k == eTree {
			c.flushBelow(0)
			c.emit(c.discard(e))
		}
	case opcode.Select:
		c.selectOp(nil)
	case opcode.SelectWithType:
		var t module.ValueType
		for n := c.r.u32(); n > 0; n-- {
			t = c.r.valueType()
		}
		c.selectOp(&t)
	case opcode.LocalGet:
		k := int(c.r.u32())
		s := c.locals[k]
		c.push(entry{t: c.localTypes[k], k: eSlot, slot: s.i, local: k})
	case opcode.LocalSet, opcode.LocalTee:
		k := int(c.r.u32())
		e := c.pop()
		c.flushLocal(k)
		c.flushBelow(0)
		if !(e.k == eSlot && e.local == k) {
			c.emit(c.store(e, c.locals[k].i))
		}
		if op == opcode.LocalTee {
			c.push(entry{t: c.localTypes[k], k: eSlot, slot: c.locals[k].i, local: k})
		}
	case opcode.GlobalGet:
		c.globalGet(c.r.u32())
	case opcode.GlobalSet:
		c.globalSet(c.r.u32())
	case opcode.I32Const:
		c.push(entry{t: module.I32, k: eConst, c: uint64(uint32(c.r.s32())), local: -1})
	case opcode.I64Const:
		c.push(entry{t: module.I64, k: eConst, c: uint64(c.r.s64()), local: -1})
	case opcode.F32Const:
		c.push(entry{t: module.F32, k: eConst, c: uint64(c.r.fixed32()), local: -1})
	case opcode.F64Const:
		c.push(entry{t: module.F64, k: eConst, c: c.r.fixed64(), local: -1})
	case opcode.RefNull:
		h := c.r.heapType()
		c.push(entry{t: module.RefNull(h), k: eConst, local: -1})
	case opcode.RefIsNull:
		v := c.ref(c.pop())
		c.pushTree(module.I32, ti32(func(f *frame) uint32 {
			if v(f).IsNull() {
				return 1
			}
			return 0
		}))
	case opcode.RefFunc:
		idx := c.r.u32()
		t := module.Ref(module.HeapIndex(c.m.Functions[idx].TypeIndex))
		c.pushTree(t, tref(func(f *frame) Ref { return FuncRef(f.inst.Funcs[idx]) }))
	case opcode.TableGet, opcode.TableSet:
		c.tableGetSet(op, c.r.u32(), at)
	default:
		if c.numeric(op) || c.memory(op, at) || c.prefixed(op, at) || c.refOps(op, at) || c.gcOps(op, at) {
			return
		}
		panic("engine: unsupported instruction " + op.Name())
	}
}

// discard returns a statement evaluating e for its effects.
func (c *compiler) discard(e entry) stmt {
	switch e.t.Kind {
	case module.KindI32:
		v := c.i32(e)
		return func(f *frame) int32 { v(f); return 0 }
	case module.KindI64:
		v := c.i64(e)
		return func(f *frame) int32 { v(f); return 0 }
	case module.KindF32:
		v := c.f32(e)
		return func(f *frame) int32 { v(f); return 0 }
	case module.KindF64:
		v := c.f64(e)
		return func(f *frame) int32 { v(f); return 0 }
	case module.KindV128:
		v := c.v128(e)
		return func(f *frame) int32 { v(f); return 0 }
	}
	v := c.ref(e)
	return func(f *frame) int32 { v(f); return 0 }
}

func (c *compiler) brIf(depth int) {
	cond := c.i32(c.pop())
	t := c.ctrl[len(c.ctrl)-1-depth]
	types := t.labelTypes()
	// The values stay on the stack if the branch is not taken: they are
	// stored into their own slots, and copied to the target's if it is.
	n := len(types)
	c.flushBelow(0)
	base := len(c.stack) - n
	for h := base; h < len(c.stack); h++ {
		c.materialize(h)
	}
	var copies []stmt
	for i, vt := range types {
		if s := copySlot(vt, c.slotEntry(vt, base+i).slot, c.slotEntry(vt, t.base+i).slot); s != nil {
			copies = append(copies, s)
		}
	}
	sig := int32(depth + 1)
	switch len(copies) {
	case 0:
		c.emit(func(f *frame) int32 {
			if cond(f) != 0 {
				return sig
			}
			return 0
		})
	default:
		c.emit(func(f *frame) int32 {
			if cond(f) != 0 {
				for _, s := range copies {
					s(f)
				}
				return sig
			}
			return 0
		})
	}
}

func (c *compiler) brTable() {
	n := c.r.u32()
	depths := make([]int, n+1)
	for i := range depths {
		depths[i] = int(c.r.u32())
	}
	index := c.i32(c.pop())
	t := c.ctrl[len(c.ctrl)-1-depths[n]]
	arity := len(t.labelTypes())
	c.flushBelow(0)
	base := len(c.stack) - arity
	for h := base; h < len(c.stack); h++ {
		c.materialize(h)
	}
	// Each target's copies, then its signal.
	type target struct {
		copies []stmt
		sig    int32
	}
	targets := make([]target, len(depths))
	for i, d := range depths {
		tt := c.ctrl[len(c.ctrl)-1-d]
		for j, vt := range tt.labelTypes() {
			if s := copySlot(vt, c.slotEntry(vt, base+j).slot, c.slotEntry(vt, tt.base+j).slot); s != nil {
				targets[i].copies = append(targets[i].copies, s)
			}
		}
		targets[i].sig = int32(d + 1)
	}
	last := uint32(n)
	c.emit(func(f *frame) int32 {
		i := index(f)
		if i > last {
			i = last
		}
		tg := &targets[i]
		for _, s := range tg.copies {
			s(f)
		}
		return tg.sig
	})
	c.markDead()
}

func (c *compiler) selectOp(typed *module.ValueType) {
	cond := c.i32(c.pop())
	b := c.pop()
	a := c.pop()
	t := a.t
	if typed != nil {
		t = *typed
	}
	switch t.Kind {
	case module.KindI32:
		x, y := c.i32(a), c.i32(b)
		c.pushTree(t, ti32(func(f *frame) uint32 {
			va, vb := x(f), y(f)
			if cond(f) != 0 {
				return va
			}
			return vb
		}))
	case module.KindI64:
		x, y := c.i64(a), c.i64(b)
		c.pushTree(t, ti64(func(f *frame) uint64 {
			va, vb := x(f), y(f)
			if cond(f) != 0 {
				return va
			}
			return vb
		}))
	case module.KindF32:
		x, y := c.f32(a), c.f32(b)
		c.pushTree(t, tf32(func(f *frame) float32 {
			va, vb := x(f), y(f)
			if cond(f) != 0 {
				return va
			}
			return vb
		}))
	case module.KindF64:
		x, y := c.f64(a), c.f64(b)
		c.pushTree(t, tf64(func(f *frame) float64 {
			va, vb := x(f), y(f)
			if cond(f) != 0 {
				return va
			}
			return vb
		}))
	case module.KindV128:
		x, y := c.v128(a), c.v128(b)
		c.pushTree(t, tv128(func(f *frame) V128 {
			va, vb := x(f), y(f)
			if cond(f) != 0 {
				return va
			}
			return vb
		}))
	default:
		x, y := c.ref(a), c.ref(b)
		c.pushTree(t, tref(func(f *frame) Ref {
			va, vb := x(f), y(f)
			if cond(f) != 0 {
				return va
			}
			return vb
		}))
	}
}

func (c *compiler) globalGet(idx uint32) {
	g := c.m.Globals[idx]
	t := g.Type
	switch {
	case t.IsRef():
		c.pushTree(t, tref(func(f *frame) Ref { return f.inst.Globals[idx].Ref }))
	case t.Kind == module.KindI32:
		c.pushTree(t, ti32(func(f *frame) uint32 { return uint32(f.inst.Globals[idx].Num) }))
	case t.Kind == module.KindI64:
		c.pushTree(t, ti64(func(f *frame) uint64 { return f.inst.Globals[idx].Num }))
	case t.Kind == module.KindF32:
		c.pushTree(t, tf32(func(f *frame) float32 { return math.Float32frombits(uint32(f.inst.Globals[idx].Num)) }))
	case t.Kind == module.KindF64:
		c.pushTree(t, tf64(func(f *frame) float64 { return math.Float64frombits(f.inst.Globals[idx].Num) }))
	default:
		c.pushTree(t, tv128(func(f *frame) V128 {
			g := f.inst.Globals[idx]
			return V128{g.Num, g.Hi}
		}))
	}
}

func (c *compiler) globalSet(idx uint32) {
	e := c.pop()
	c.flushBelow(0)
	switch e.t.Kind {
	case module.KindI32:
		v := c.i32(e)
		c.emit(func(f *frame) int32 { f.inst.Globals[idx].Num = uint64(v(f)); return 0 })
	case module.KindI64:
		v := c.i64(e)
		c.emit(func(f *frame) int32 { f.inst.Globals[idx].Num = v(f); return 0 })
	case module.KindF32:
		v := c.f32(e)
		c.emit(func(f *frame) int32 { f.inst.Globals[idx].Num = uint64(math.Float32bits(v(f))); return 0 })
	case module.KindF64:
		v := c.f64(e)
		c.emit(func(f *frame) int32 { f.inst.Globals[idx].Num = math.Float64bits(v(f)); return 0 })
	case module.KindV128:
		v := c.v128(e)
		c.emit(func(f *frame) int32 {
			x := v(f)
			g := f.inst.Globals[idx]
			g.Num, g.Hi = x.Lo, x.Hi
			return 0
		})
	default:
		v := c.ref(e)
		c.emit(func(f *frame) int32 { f.inst.Globals[idx].Ref = v(f); return 0 })
	}
}

// argStore stores an argument into a callee's frame.
type argStore func(f *frame, l []uint64, r []Ref)

// argStores returns the stores of arguments es into a frame laid out by refs.
func (c *compiler) argStores(es []entry, refs []slotRef) []argStore {
	out := make([]argStore, len(es))
	for i, e := range es {
		s := refs[i].i
		switch e.t.Kind {
		case module.KindI32:
			v := c.i32(e)
			out[i] = func(f *frame, l []uint64, r []Ref) { l[s] = uint64(v(f)) }
		case module.KindI64:
			v := c.i64(e)
			out[i] = func(f *frame, l []uint64, r []Ref) { l[s] = v(f) }
		case module.KindF32:
			v := c.f32(e)
			out[i] = func(f *frame, l []uint64, r []Ref) { l[s] = uint64(math.Float32bits(v(f))) }
		case module.KindF64:
			v := c.f64(e)
			out[i] = func(f *frame, l []uint64, r []Ref) { l[s] = math.Float64bits(v(f)) }
		case module.KindV128:
			v := c.v128(e)
			out[i] = func(f *frame, l []uint64, r []Ref) { x := v(f); l[s], l[s+1] = x.Lo, x.Hi }
		default:
			v := c.ref(e)
			out[i] = func(f *frame, l []uint64, r []Ref) { r[s] = v(f) }
		}
	}
	return out
}

// pushCallResults pushes the results of a call, which the call leaves in
// the slots of stack positions from base.
func (c *compiler) pushCallResults(results []module.ValueType, base int) {
	for i, t := range results {
		c.push(c.slotEntry(t, base+i))
	}
}

// resultCopies returns, for results found in a callee's frame at refs, the
// copies into the caller's stack positions from base.
func (c *compiler) resultCopies(results []module.ValueType, base int) []func(f *frame, l []uint64, r []Ref) {
	refs, _, _ := layout(results)
	out := make([]func(f *frame, l []uint64, r []Ref), len(results))
	for i, t := range results {
		src := refs[i].i
		dst := c.slotEntry(t, base+i).slot
		switch {
		case t.IsRef():
			out[i] = func(f *frame, l []uint64, r []Ref) { f.r[dst] = r[src] }
		case t.Kind == module.KindV128:
			out[i] = func(f *frame, l []uint64, r []Ref) { f.l[dst], f.l[dst+1] = l[src], l[src+1] }
		default:
			out[i] = func(f *frame, l []uint64, r []Ref) { f.l[dst] = l[src] }
		}
	}
	return out
}

// call compiles a call of function idx, or a tail call.
func (c *compiler) call(idx uint32, at uint32, tail bool) {
	sig := c.m.FuncType(idx)
	es := c.popN(len(sig.Params))
	c.flushBelow(0)
	if tail {
		args := make([]func(*frame) Val, len(es))
		for i, e := range es {
			args[i] = c.val(e)
		}
		c.emit(func(f *frame) int32 {
			x := f.x
			x.tailArgs = x.tailArgs[:0]
			for _, a := range args {
				x.tailArgs = append(x.tailArgs, a(f))
			}
			x.tail = f.inst.Funcs[idx]
			return sigTail
		})
		c.markDead()
		return
	}
	base := len(c.stack)
	if c.m.Functions[idx].Imported {
		c.callGenericFn(func(f *frame) *Function { return f.inst.Funcs[idx] }, sig, es, base, at)
		return
	}
	params, _, _ := layout(sig.Params)
	stores := c.argStores(es, params)
	copies := c.resultCopies(sig.Results, base)
	code := c.code
	c.emit(func(f *frame) int32 {
		x := f.x
		cd := code.get(idx)
		nc, nsp, rc, rsp := x.nc, x.nsp, x.rc, x.rsp
		l, _, _ := x.takeNum(cd.nslots)
		r, _, _ := x.takeRef(cd.rslots)
		for _, s := range stores {
			s(f, l, r)
		}
		x.calls[len(x.calls)-1].offset = at
		x.run(f.inst.Funcs[idx], cd, l, r, nc, nsp, rc, rsp)
		for _, cp := range copies {
			cp(f, l, r)
		}
		x.nc, x.nsp, x.rc, x.rsp = nc, nsp, rc, rsp
		return 0
	})
	c.pushCallResults(sig.Results, base)
}

// callGenericFn compiles a call of the function target returns, which may
// be the host's or another instance's.
func (c *compiler) callGenericFn(target func(*frame) *Function, sig *module.FuncType, es []entry, base int, at uint32) {
	args := make([]func(*frame) Val, len(es))
	for i, e := range es {
		args[i] = c.val(e)
	}
	results := sig.Results
	copies := c.resultCopies(results, base)
	dsts := make([]int, len(results))
	for i, t := range results {
		dsts[i] = c.slotEntry(t, base+i).slot
	}
	c.emit(func(f *frame) int32 {
		x := f.x
		fn := target(f)
		x.calls[len(x.calls)-1].offset = at
		if fn.Host != nil {
			vals := evalArgs(f, args)
			res := x.hostCall(fn, vals)
			for i, t := range results {
				storeVal(f, t, dsts[i], res[i])
			}
			return 0
		}
		cd := codeOf(fn)
		nc, nsp, rc, rsp := x.nc, x.nsp, x.rc, x.rsp
		vals := evalArgs(f, args)
		l, _, _ := x.takeNum(cd.nslots)
		r, _, _ := x.takeRef(cd.rslots)
		putVals(l, r, cd.params, vals)
		x.run(fn, cd, l, r, nc, nsp, rc, rsp)
		for _, cp := range copies {
			cp(f, l, r)
		}
		x.nc, x.nsp, x.rc, x.rsp = nc, nsp, rc, rsp
		return 0
	})
	c.pushCallResults(results, base)
}

// storeVal stores v, of type t, into slot s of f.
func storeVal(f *frame, t module.ValueType, s int, v Val) {
	switch {
	case t.IsRef():
		f.r[s] = v.R
	case t.Kind == module.KindV128:
		f.l[s], f.l[s+1] = v.N, v.Hi
	default:
		f.l[s] = v.N
	}
}

// callIndirect compiles call_indirect and return_call_indirect.
func (c *compiler) callIndirect(typeIndex, table uint32, at uint32, tail bool) {
	sig := c.m.Types[typeIndex].Func
	want := c.m.Canonical[typeIndex]
	index := c.pop()
	is64 := c.m.Tables[table].Limits.Is64
	var idx func(*frame) uint64
	if is64 {
		idx = c.i64(index)
	} else {
		v := c.i32(index)
		idx = func(f *frame) uint64 { return uint64(v(f)) }
	}
	target := func(f *frame) *Function {
		i := idx(f)
		t := f.inst.Tables[table]
		if i >= uint64(len(t.Elems)) {
			trap(TrapTableOutOfBounds)
		}
		e := t.Elems[i]
		if e.IsNull() {
			trap(TrapNullFunc)
		}
		fn := e.Func()
		if fn.Type != want && !module.IsCanonicalSubtype(fn.Type, want) {
			trap(TrapFuncSigMismatch)
		}
		return fn
	}
	c.callTarget(sig, target, at, tail)
}

// callTarget compiles a call of the function target returns, which is
// evaluated after the arguments: call_indirect, call_ref and their tail
// calls.
func (c *compiler) callTarget(sig *module.FuncType, target func(*frame) *Function, at uint32, tail bool) {
	// The arguments are evaluated before the target: store them first.
	n := len(sig.Params)
	c.flushBelow(0)
	base := len(c.stack) - n
	for h := base; h < len(c.stack); h++ {
		c.materialize(h)
	}
	es := c.popN(n)
	if tail {
		args := make([]func(*frame) Val, n)
		for i, e := range es {
			args[i] = c.val(e)
		}
		c.emit(func(f *frame) int32 {
			x := f.x
			fn := target(f)
			x.tailArgs = x.tailArgs[:0]
			for _, a := range args {
				x.tailArgs = append(x.tailArgs, a(f))
			}
			x.tail = fn
			return sigTail
		})
		c.markDead()
		return
	}
	c.callGenericFn(target, sig, es, base, at)
}
