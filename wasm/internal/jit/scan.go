package jit

import (
	"fmt"

	"github.com/go-quickjs/go-quickjs/wasm/internal/module"
	"github.com/go-quickjs/go-quickjs/wasm/internal/opcode"
)

// Refusal is why the compiler leaves a function to the interpreter: Reason
// is one of the README's list, Detail what in the function made it.
type Refusal struct {
	Reason string
	Detail string
}

func (r *Refusal) Error() string {
	if r.Detail == "" {
		return "wasm jit: refused: " + r.Reason
	}
	return "wasm jit: refused: " + r.Reason + ": " + r.Detail
}

// The reasons a function is refused.
const (
	RefuseSIMD    = "simd"
	RefuseAtomics = "atomics"
	RefuseGC      = "gc"
	RefuseAsmJS   = "asmjs"
	RefuseF16     = "f16"
	RefuseCPU     = "cpu"
	RefuseFrame   = "frame"
	RefuseSize    = "size"
	RefuseOther   = "other"
)

func refuse(reason, format string, args ...any) {
	panic(&Refusal{Reason: reason, Detail: fmt.Sprintf(format, args...)})
}

// checkType refuses a function with a value of type t where the CPU has
// not the instructions vectors need.
func checkType(t module.ValueType, what string, feat Features) {
	if t.Kind == module.KindV128 && !feat.SIMD {
		refuse(RefuseCPU, "%s of type v128 without SSE4.2", what)
	}
}

func checkTypes(ts []module.ValueType, what string, feat Features) {
	for _, t := range ts {
		checkType(t, what, feat)
	}
}

// Features are the CPU's optional instructions the code may use, and the
// relaxed SIMD results the interpreter gives on it.
type Features struct {
	// POPCNT, and SSE4.1's ROUNDSS and ROUNDSD.
	Popcnt, Round bool
	// SIMD is the vector instructions of SSSE3, SSE4.1 and SSE4.2 on
	// amd64; arm64 always has its own.
	SIMD bool
	// RelaxedFused says relaxed_madd and relaxed_nmadd are fused, one
	// rounding; RelaxedDotSigned and RelaxedDotVNNI say which products
	// relaxed_dot_i8x16_i7x16_add_s sums (see engine/relaxed_x86.go).
	RelaxedFused, RelaxedDotSigned, RelaxedDotVNNI bool
}

// localUse is what the body does with its locals: how much each is used,
// a use in a loop counting eight times one outside it, and which are ever
// written.
type localUse struct {
	weight  []uint64
	written []bool
}

// scan reads function fn's body, past its locals, and refuses it if any
// instruction is one the compiler does not compile. It returns the body's
// local types, the parameters first, and their uses.
func scan(m *module.Module, fn uint32, feat Features) (locals []module.ValueType, use localUse) {
	f := &m.Functions[fn]
	sig := m.Types[f.TypeIndex].Func
	checkTypes(sig.Params, "parameter", feat)
	checkTypes(sig.Results, "result", feat)
	r := reader{b: m.Bytes, pc: int(f.Code.Offset)}
	end := int(f.Code.End())
	locals = append(locals, sig.Params...)
	for n := r.u32(); n > 0; n-- {
		count := r.u32()
		t := r.valueType()
		checkType(t, "local", feat)
		for j := uint32(0); j < count; j++ {
			locals = append(locals, t)
		}
	}
	use.weight = make([]uint64, len(locals))
	use.written = make([]bool, len(locals))
	// loops says, for each construct open, whether it is a loop.
	loops := []bool{false}
	depth := 0
	for r.pc < end {
		op := r.opcode()
		switch op.Prefix() {
		case opcode.PrefixSIMD:
			if !feat.SIMD {
				refuse(RefuseCPU, "%s without SSE4.2", op.Name())
			}
		case opcode.PrefixAtomic:
			if op >= opcode.StructAtomicGet {
				refuse(RefuseAtomics, "%s", op.Name())
			}
		case opcode.PrefixGC:
			if op < opcode.StructNew || op > opcode.I31GetU {
				refuse(RefuseGC, "%s", op.Name())
			}
			switch op {
			case opcode.StructGet, opcode.StructGetS, opcode.StructGetU, opcode.StructSet:
				ti := r.u32()
				if m.Types[ti].Struct.Fields[r.u32()].Type.Kind == module.KindV128 {
					refuse(RefuseSIMD, "%s of a v128 field", op.Name())
				}
				continue
			case opcode.ArrayGet, opcode.ArrayGetS, opcode.ArrayGetU, opcode.ArraySet:
				if m.Types[r.u32()].Array.Elem.Type.Kind == module.KindV128 {
					refuse(RefuseSIMD, "%s of v128 elements", op.Name())
				}
				continue
			}
		case opcode.PrefixAsmJS:
			refuse(RefuseAsmJS, "%s", op.Name())
		}
		switch op {
		case opcode.F32LoadMemF16, opcode.F32StoreMemF16:
			refuse(RefuseF16, "%s", op.Name())
		case opcode.Block, opcode.Loop, opcode.If, opcode.Try:
			params, results := r.blockType(m)
			checkTypes(params, op.Name()+" parameter", feat)
			checkTypes(results, op.Name()+" result", feat)
			loops = append(loops, op == opcode.Loop)
			if op == opcode.Loop {
				depth++
			}
			continue
		case opcode.TryTable:
			params, results := r.blockType(m)
			checkTypes(params, op.Name()+" parameter", feat)
			checkTypes(results, op.Name()+" result", feat)
			for n := r.u32(); n > 0; n-- {
				if kind := r.byte(); kind < 2 {
					r.u32()
				}
				r.u32()
			}
			loops = append(loops, false)
			continue
		case opcode.End, opcode.Delegate:
			if op == opcode.Delegate {
				r.u32()
			}
			if loops[len(loops)-1] {
				depth--
			}
			loops = loops[:len(loops)-1]
			continue
		case opcode.LocalGet, opcode.LocalSet, opcode.LocalTee:
			k := r.u32()
			use.weight[k] += 1 << (3 * min(depth, 6))
			if op != opcode.LocalGet {
				use.written[k] = true
			}
			continue
		case opcode.SelectWithType:
			for n := r.u32(); n > 0; n-- {
				checkType(r.valueType(), "select", feat)
			}
			continue
		case opcode.CallFunction, opcode.ReturnCall:
			callee := m.FuncType(r.u32())
			checkTypes(callee.Params, "callee parameter", feat)
			checkTypes(callee.Results, "callee result", feat)
			continue
		case opcode.CallIndirect, opcode.ReturnCallIndirect:
			ft := m.Types[r.u32()].Func
			r.u32()
			checkTypes(ft.Params, "callee parameter", feat)
			checkTypes(ft.Results, "callee result", feat)
			continue
		case opcode.GlobalGet, opcode.GlobalSet:
			checkType(m.Globals[r.u32()].Type, "global", feat)
			continue
		case opcode.I32Popcnt, opcode.I64Popcnt:
			if !feat.Popcnt {
				refuse(RefuseCPU, "%s without POPCNT", op.Name())
			}
		case opcode.F32Ceil, opcode.F32Floor, opcode.F32Trunc, opcode.F32NearestInt,
			opcode.F64Ceil, opcode.F64Floor, opcode.F64Trunc, opcode.F64NearestInt:
			if !feat.Round {
				refuse(RefuseCPU, "%s without SSE4.1", op.Name())
			}
		}
		r.skipImmediates(op)
	}
	return locals, use
}
