package opt

import "github.com/go-quickjs/go-quickjs/wasm/internal/opcode"

// goImms are the two immediates an instruction Go runs (OpGoOp) passes to
// the engine's exit (Ctx.Arg1, Arg2): memory.copy's destination and
// source, Aux2's low and high words; wait's and notify's memory, Aux's
// upper half, and offset, Aux2; any other's Aux2 and 0.
func goImms(v *Value) (x, y uint64) {
	switch opcode.Opcode(v.Aux) {
	case opcode.MemoryCopy:
		return v.Aux2 & 0xffffffff, v.Aux2 >> 32
	case opcode.AtomicNotify, opcode.I32AtomicWait, opcode.I64AtomicWait:
		return v.Aux >> 32, v.Aux2
	}
	return v.Aux2, 0
}
