package opt

import (
	wjit "github.com/go-quickjs/go-quickjs/wasm/internal/jit"
	"github.com/go-quickjs/go-quickjs/wasm/internal/opcode"
)

// The instructions the interpreter's code runs for the tier's (OpGoOp
// with goInterp): what the tier does not build natively -- an access of a
// v128 field or element, struct.new of a type with v128 fields and the
// array instructions of v128 elements that allocate or fill, popcnt and
// the roundings on a CPU without POPCNT or SSE4.1 -- and, for tests
// (Request.ExitAll), every instruction an exit can run (wjit.Effect). Go
// runs the interpreter's code for the instruction at its offset
// (wjit.ExitInstr), as the baseline's exit does: its numeric operands
// staged in the call area, each at its place among its operands, its
// references in their positions' reference slots (Ctx.RArgs), its number
// read back from the first operand's slot, its reference left in the
// first operand's position's reference slot. So a function with such an
// instruction is optimized all the same, the instruction a call of Go's.

// goInterp marks, in Aux above the opcode, an instruction the
// interpreter's code runs; its Aux2 is the instruction's offset in the
// module's bytes, which its At, an inlined site's in an inlined body, may
// not be.
const goInterp = 1 << 40

// isInterp reports whether v is an instruction the interpreter's code runs.
func isInterp(v *Value) bool { return v.Op == OpGoOp && v.Aux&goInterp != 0 }

// goInstr builds the instruction at offset pc of the body, reported at at,
// as one the interpreter's code runs, reporting whether an exit can run it
// (wjit.Effect).
func (bd *builder) goInstr(pc, at uint32) bool {
	e, ok := wjit.Effect(bd.m, pc, bd.ws.effParams[:0])
	if !ok {
		return false
	}
	bd.ws.effParams = e.Params
	n := len(e.Params)
	pos := len(bd.stack) - n
	args := bd.args[:0]
	refs := e.HasResult() && e.Result.IsRef()
	for i, a := range bd.stack[pos:] {
		if e.Params[i].IsRef() {
			refs = true
			continue
		}
		args = append(args, a)
	}
	bd.args = args
	t := TNone
	if e.HasResult() {
		t = typeOf(e.Result, e.Op.Name()+" result")
	}
	if t == TRef {
		t = TNone
	}
	v := bd.value(OpGoOp, t, args...)
	v.Aux, v.Aux2, v.At, v.Pos = uint64(e.Op)|goInterp, uint64(pc), at, int32(pos)
	if refs {
		bd.f.Refs = true
	}
	bd.stack = bd.stack[:pos]
	switch {
	case !e.HasResult():
	case e.Result.IsRef():
		bd.pushRef()
	default:
		bd.push(v)
	}
	// What the instruction writes -- memory, a global, a table -- is not
	// known here.
	bd.forget()
	bd.r.pc = e.End
	return true
}

// cpuExit reports whether instruction op needs a feature the CPU has not
// (Request.Features), and so is the interpreter's to run: popcnt without
// POPCNT, a rounding without SSE4.1.
func (bd *builder) cpuExit(op opcode.Opcode) bool {
	switch op {
	case opcode.I32Popcnt, opcode.I64Popcnt:
		return !bd.ws.features().Popcnt
	case opcode.F32Ceil, opcode.F32Floor, opcode.F32Trunc, opcode.F32NearestInt,
		opcode.F64Ceil, opcode.F64Floor, opcode.F64Trunc, opcode.F64NearestInt:
		return !bd.ws.features().Round
	}
	return false
}
