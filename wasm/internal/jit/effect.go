package jit

import (
	"github.com/go-quickjs/go-quickjs/wasm/internal/module"
	"github.com/go-quickjs/go-quickjs/wasm/internal/opcode"
)

// Instr is what an instruction takes from the operand stack and leaves on
// it, for a run of it by the interpreter's code (ExitInstr): its opcode,
// its operands' types, the deepest first, its result's (Kind 0 for none),
// and the offset past its immediates. A reference's type says only that it
// is one: the code that runs it reads no more of it.
type Instr struct {
	Op     opcode.Opcode
	Params []module.ValueType
	Result module.ValueType
	End    int
}

// HasResult reports whether the instruction leaves a value.
func (e *Instr) HasResult() bool { return e.Result.Kind != 0 }

// anyRef is the type Effect gives a reference.
var anyRef = module.RefNull(module.HeapAny)

// Effect returns the effect of the instruction at offset at of module m,
// and whether the interpreter's code for it alone may stand for it in
// native code: an instruction of a straight line -- one that neither
// branches, calls, returns, throws nor reads or writes a local -- whose
// operands' types its opcode and immediates decide, that leaves at most
// one value. A constant, which native code keeps as it is, is not one
// either. params is where the operands' types go where it has room.
func Effect(m *module.Module, at uint32, params []module.ValueType) (e Instr, ok bool) {
	r := reader{b: m.Bytes, pc: int(at)}
	op := r.opcode()
	e.Op = op
	ps := params[:0]
	is64 := func(mem uint32) bool { return int(mem) < len(m.Memories) && m.Memories[mem].Limits.Is64 }
	tab64 := func(t uint32) bool { return int(t) < len(m.Tables) && m.Tables[t].Limits.Is64 }
	idx := func(b bool) module.ValueType {
		if b {
			return module.I64
		}
		return module.I32
	}
	res := func(t module.ValueType) {
		if t.IsRef() {
			t = anyRef
		}
		e.Result = t.Unpacked()
	}
	ok = true
	switch {
	case op >= opcode.I32LoadMem && op <= opcode.I64StoreMem32:
		mem, _ := r.memarg()
		ps = append(ps, idx(is64(mem)))
		if t, load := plainAccess(op); load {
			res(t)
		} else {
			ps = append(ps, t)
		}
	case op.Prefix() == 0 && op >= opcode.I32Eqz && op <= opcode.I64SExtendI32:
		sig := opcode.Signature(op)
		if sig == nil {
			return e, false
		}
		ps = append(ps, sig.Params...)
		if len(sig.Results) > 0 {
			res(sig.Results[0])
		}
	case op.Prefix() == 0:
		switch op {
		case opcode.SelectWithType:
			var t module.ValueType
			for n := r.u32(); n > 0; n-- {
				t = r.valueType()
			}
			if t.IsRef() {
				t = anyRef
			}
			ps = append(ps, t, t, module.I32)
			res(t)
		case opcode.GlobalGet:
			res(m.Globals[r.u32()].Type)
		case opcode.GlobalSet:
			t := m.Globals[r.u32()].Type
			if t.IsRef() {
				t = anyRef
			}
			ps = append(ps, t)
		case opcode.TableGet:
			t := r.u32()
			ps = append(ps, idx(tab64(t)))
			res(anyRef)
		case opcode.TableSet:
			t := r.u32()
			ps = append(ps, idx(tab64(t)), anyRef)
		case opcode.MemorySize:
			res(idx(is64(r.u32())))
		case opcode.MemoryGrow:
			t := idx(is64(r.u32()))
			ps = append(ps, t)
			res(t)
		case opcode.RefIsNull:
			ps = append(ps, anyRef)
			res(module.I32)
		case opcode.RefFunc:
			r.u32()
			res(anyRef)
		case opcode.RefEq:
			ps = append(ps, anyRef, anyRef)
			res(module.I32)
		case opcode.RefAsNonNull:
			ps = append(ps, anyRef)
			res(anyRef)
		default:
			return e, false
		}
	case op.Prefix() == opcode.PrefixNumeric:
		switch op {
		case opcode.MemoryInit:
			r.u32()
			ps = append(ps, idx(is64(r.u32())), module.I32, module.I32)
		case opcode.DataDrop, opcode.ElemDrop:
			r.u32()
		case opcode.MemoryCopy:
			d, s := is64(r.u32()), is64(r.u32())
			ps = append(ps, idx(d), idx(s), idx(d && s))
		case opcode.MemoryFill:
			t := idx(is64(r.u32()))
			ps = append(ps, t, module.I32, t)
		case opcode.TableInit:
			r.u32()
			ps = append(ps, idx(tab64(r.u32())), module.I32, module.I32)
		case opcode.TableCopy:
			d, s := tab64(r.u32()), tab64(r.u32())
			ps = append(ps, idx(d), idx(s), idx(d && s))
		case opcode.TableGrow:
			t := idx(tab64(r.u32()))
			ps = append(ps, anyRef, t)
			res(t)
		case opcode.TableSize:
			res(idx(tab64(r.u32())))
		case opcode.TableFill:
			t := idx(tab64(r.u32()))
			ps = append(ps, t, anyRef, t)
		default:
			sig := opcode.Signature(op)
			if sig == nil {
				return e, false
			}
			ps = append(ps, sig.Params...)
			res(sig.Results[0])
		}
	case op.Prefix() == opcode.PrefixSIMD:
		switch {
		case op == opcode.S128Const:
			return e, false
		case op >= opcode.S128LoadMem && op < opcode.S128StoreMem,
			op == opcode.S128Load32Zero || op == opcode.S128Load64Zero:
			mem, _ := r.memarg()
			ps = append(ps, idx(is64(mem)))
			res(module.V128)
		case op == opcode.S128StoreMem:
			mem, _ := r.memarg()
			ps = append(ps, idx(is64(mem)), module.V128)
		case op >= opcode.S128Load8Lane && op <= opcode.S128Store64Lane:
			mem, _ := r.memarg()
			r.pc++
			ps = append(ps, idx(is64(mem)), module.V128)
			if op <= opcode.S128Load64Lane {
				res(module.V128)
			}
		case op == opcode.I8x16Shuffle:
			r.pc += 16
			ps = append(ps, module.V128, module.V128)
			res(module.V128)
		case isLaneOp(op):
			r.pc++
			k := module.ValueType{Kind: laneKind(op)}
			ps = append(ps, module.V128)
			if isReplaceLane(op) {
				ps = append(ps, k)
				res(module.V128)
			} else {
				res(k)
			}
		default:
			sig := opcode.Signature(op)
			if sig == nil || len(sig.Results) != 1 {
				return e, false
			}
			ps = append(ps, sig.Params...)
			res(sig.Results[0])
		}
	case op.Prefix() == opcode.PrefixAtomic:
		switch op {
		case opcode.AtomicFence:
			r.pc++
		case opcode.Pause:
		default:
			mem, _ := r.memarg()
			sig := opcode.AtomicSignature(op, is64(mem))
			if sig == nil {
				return e, false
			}
			ps = append(ps, sig.Params...)
			if len(sig.Results) > 0 {
				res(sig.Results[0])
			}
		}
	case op.Prefix() == opcode.PrefixGC:
		if !gcEffect(m, &r, op, &ps, res) {
			return e, false
		}
	default:
		return e, false
	}
	e.Params, e.End = ps, r.pc
	return e, ok
}

// gcEffect is Effect of GC instruction op, its opcode read from r.
func gcEffect(m *module.Module, r *reader, op opcode.Opcode, ps *[]module.ValueType, res func(module.ValueType)) bool {
	field := func(ti, fi uint32) module.ValueType {
		t := m.Types[ti].Struct.Fields[fi].Type.Unpacked()
		if t.IsRef() {
			return anyRef
		}
		return t
	}
	elem := func(ti uint32) module.ValueType {
		t := m.Types[ti].Array.Elem.Type.Unpacked()
		if t.IsRef() {
			return anyRef
		}
		return t
	}
	switch op {
	case opcode.StructNew:
		ti := r.u32()
		for fi := range m.Types[ti].Struct.Fields {
			*ps = append(*ps, field(ti, uint32(fi)))
		}
		res(anyRef)
	case opcode.StructNewDefault:
		r.u32()
		res(anyRef)
	case opcode.StructGet, opcode.StructGetS, opcode.StructGetU:
		ti, fi := r.u32(), r.u32()
		*ps = append(*ps, anyRef)
		res(field(ti, fi))
	case opcode.StructSet:
		ti, fi := r.u32(), r.u32()
		*ps = append(*ps, anyRef, field(ti, fi))
	case opcode.ArrayNew:
		*ps = append(*ps, elem(r.u32()), module.I32)
		res(anyRef)
	case opcode.ArrayNewDefault:
		r.u32()
		*ps = append(*ps, module.I32)
		res(anyRef)
	case opcode.ArrayNewFixed:
		ti, n := r.u32(), r.u32()
		for ; n > 0; n-- {
			*ps = append(*ps, elem(ti))
		}
		res(anyRef)
	case opcode.ArrayNewData, opcode.ArrayNewElem:
		r.u32()
		r.u32()
		*ps = append(*ps, module.I32, module.I32)
		res(anyRef)
	case opcode.ArrayGet, opcode.ArrayGetS, opcode.ArrayGetU:
		t := elem(r.u32())
		*ps = append(*ps, anyRef, module.I32)
		res(t)
	case opcode.ArraySet:
		*ps = append(*ps, anyRef, module.I32, elem(r.u32()))
	case opcode.ArrayLen:
		*ps = append(*ps, anyRef)
		res(module.I32)
	case opcode.ArrayFill:
		*ps = append(*ps, anyRef, module.I32, elem(r.u32()), module.I32)
	case opcode.ArrayCopy:
		r.u32()
		r.u32()
		*ps = append(*ps, anyRef, module.I32, anyRef, module.I32, module.I32)
	case opcode.ArrayInitData, opcode.ArrayInitElem:
		r.u32()
		r.u32()
		*ps = append(*ps, anyRef, module.I32, module.I32, module.I32)
	case opcode.RefTest, opcode.RefTestNull:
		r.heapType()
		*ps = append(*ps, anyRef)
		res(module.I32)
	case opcode.RefCast, opcode.RefCastNull:
		r.heapType()
		*ps = append(*ps, anyRef)
		res(anyRef)
	case opcode.AnyConvertExtern, opcode.ExternConvertAny:
		*ps = append(*ps, anyRef)
		res(anyRef)
	case opcode.RefI31:
		*ps = append(*ps, module.I32)
		res(anyRef)
	case opcode.I31GetS, opcode.I31GetU:
		*ps = append(*ps, anyRef)
		res(module.I32)
	default:
		return false
	}
	return true
}

// plainAccess is the type a load of the numeric memory instructions
// (i32.load to i64.store32) leaves, or a store takes, and whether it is a
// load.
func plainAccess(op opcode.Opcode) (module.ValueType, bool) {
	switch op {
	case opcode.I32LoadMem, opcode.I32LoadMem8S, opcode.I32LoadMem8U, opcode.I32LoadMem16S, opcode.I32LoadMem16U:
		return module.I32, true
	case opcode.I64LoadMem, opcode.I64LoadMem8S, opcode.I64LoadMem8U, opcode.I64LoadMem16S, opcode.I64LoadMem16U,
		opcode.I64LoadMem32S, opcode.I64LoadMem32U:
		return module.I64, true
	case opcode.F32LoadMem:
		return module.F32, true
	case opcode.F64LoadMem:
		return module.F64, true
	case opcode.I32StoreMem, opcode.I32StoreMem8, opcode.I32StoreMem16:
		return module.I32, false
	case opcode.I64StoreMem, opcode.I64StoreMem8, opcode.I64StoreMem16, opcode.I64StoreMem32:
		return module.I64, false
	case opcode.F32StoreMem:
		return module.F32, false
	}
	return module.F64, false
}

// isLaneOp reports whether op is an extract_lane or a replace_lane.
func isLaneOp(op opcode.Opcode) bool {
	return op >= opcode.I8x16ExtractLaneS && op <= opcode.F64x2ReplaceLane
}

func isReplaceLane(op opcode.Opcode) bool {
	switch op {
	case opcode.I8x16ReplaceLane, opcode.I16x8ReplaceLane, opcode.I32x4ReplaceLane,
		opcode.I64x2ReplaceLane, opcode.F32x4ReplaceLane, opcode.F64x2ReplaceLane:
		return true
	}
	return false
}

// laneKind is the type of the scalar an extract_lane or replace_lane takes
// or gives.
func laneKind(op opcode.Opcode) module.Kind {
	switch op {
	case opcode.I64x2ExtractLane, opcode.I64x2ReplaceLane:
		return module.KindI64
	case opcode.F32x4ExtractLane, opcode.F32x4ReplaceLane:
		return module.KindF32
	case opcode.F64x2ExtractLane, opcode.F64x2ReplaceLane:
		return module.KindF64
	}
	return module.KindI32
}
