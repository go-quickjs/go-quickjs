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
	RefuseSIMD       = "simd"
	RefuseAtomics    = "atomics"
	RefuseGC         = "gc"
	RefuseExceptions = "exceptions"
	RefuseRefs       = "references"
	RefuseMemory64   = "memory64"
	RefuseMultiMem   = "multi-memory"
	RefuseAsmJS      = "asmjs"
	RefuseF16        = "f16"
	RefuseCPU        = "cpu"
	RefuseFrame      = "frame"
	RefuseSize       = "size"
	RefuseOther      = "other"
)

func refuse(reason, format string, args ...any) {
	panic(&Refusal{Reason: reason, Detail: fmt.Sprintf(format, args...)})
}

// checkType refuses a function with a value of type t: v1 code holds
// numbers only.
func checkType(t module.ValueType, what string) {
	switch {
	case t.IsRef():
		refuse(RefuseRefs, "%s of type %s", what, t.Name())
	case t.Kind == module.KindV128:
		refuse(RefuseSIMD, "%s of type v128", what)
	}
}

func checkTypes(ts []module.ValueType, what string) {
	for _, t := range ts {
		checkType(t, what)
	}
}

// Features are the CPU's optional instructions the code may use.
type Features struct {
	// POPCNT, and SSE4.1's ROUNDSS and ROUNDSD.
	Popcnt, Round bool
}

// scan reads function fn's body, past its locals, and refuses it if any
// instruction is one the compiler does not compile. It returns the body's
// local types, the parameters first.
func scan(m *module.Module, fn uint32, feat Features) (locals []module.ValueType) {
	f := &m.Functions[fn]
	sig := m.Types[f.TypeIndex].Func
	checkTypes(sig.Params, "parameter")
	checkTypes(sig.Results, "result")
	r := reader{b: m.Bytes, pc: int(f.Code.Offset)}
	end := int(f.Code.End())
	locals = append(locals, sig.Params...)
	for n := r.u32(); n > 0; n-- {
		count := r.u32()
		t := r.valueType()
		checkType(t, "local")
		for j := uint32(0); j < count; j++ {
			locals = append(locals, t)
		}
	}
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
			refuse(RefuseAsmJS, "%s", op.Name())
		}
		switch op {
		case opcode.Try, opcode.Catch, opcode.CatchAll, opcode.Throw, opcode.Rethrow,
			opcode.TryTable, opcode.ThrowRef, opcode.Delegate:
			refuse(RefuseExceptions, "%s", op.Name())
		case opcode.RefNull, opcode.RefIsNull, opcode.RefFunc, opcode.RefAsNonNull, opcode.RefEq,
			opcode.BrOnNull, opcode.BrOnNonNull, opcode.CallRef, opcode.ReturnCallRef,
			opcode.TableGet, opcode.TableSet, opcode.TableGrow, opcode.TableFill:
			refuse(RefuseRefs, "%s", op.Name())
		case opcode.F32LoadMemF16, opcode.F32StoreMemF16:
			refuse(RefuseF16, "%s", op.Name())
		case opcode.Block, opcode.Loop, opcode.If:
			params, results := r.blockType(m)
			checkTypes(params, op.Name()+" parameter")
			checkTypes(results, op.Name()+" result")
			continue
		case opcode.SelectWithType:
			for n := r.u32(); n > 0; n-- {
				checkType(r.valueType(), "select")
			}
			continue
		case opcode.CallFunction, opcode.ReturnCall:
			callee := m.FuncType(r.u32())
			checkTypes(callee.Params, "callee parameter")
			checkTypes(callee.Results, "callee result")
			continue
		case opcode.CallIndirect, opcode.ReturnCallIndirect:
			ft := m.Types[r.u32()].Func
			r.u32()
			checkTypes(ft.Params, "callee parameter")
			checkTypes(ft.Results, "callee result")
			continue
		case opcode.GlobalGet, opcode.GlobalSet:
			checkType(m.Globals[r.u32()].Type, "global")
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
		if op >= opcode.I32LoadMem && op <= opcode.I64StoreMem32 {
			mem, _ := r.memarg()
			if mem != 0 {
				refuse(RefuseMultiMem, "%s of memory %d", op.Name(), mem)
			}
			if m.Memories[0].Limits.Is64 {
				refuse(RefuseMemory64, "%s", op.Name())
			}
			continue
		}
		r.skipImmediates(op)
	}
	return locals
}
