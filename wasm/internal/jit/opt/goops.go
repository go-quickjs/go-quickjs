package opt

import "github.com/go-quickjs/go-quickjs/wasm/internal/opcode"

// The instructions Go runs (OpGoOp): memory.size, memory.grow,
// memory.copy and memory.fill but where the code does them (InCode),
// memory.init, data.drop, table.init, table.copy, elem.drop, table.grow,
// table.fill, memory.atomic.wait32, wait64 and notify. Their numeric
// operands are staged in the call area, two slots apart, each at its place
// among the instruction's operands (goArgIndex); table.grow's and
// table.fill's reference is in the reference slot of its stack position,
// which Ctx.RArgs tells the engine from Pos, the first operand's.

// goImms are the two immediates an instruction Go runs passes to the
// engine's exit (Ctx.Arg1, Arg2): memory.copy's destination and source,
// memory.init's and table.init's segment and memory or table, and
// table.copy's destination and source, Aux2's low and high words; wait's
// and notify's memory, Aux's upper half, and offset, Aux2; any other's
// Aux2 and 0.
func goImms(v *Value) (x, y uint64) {
	switch opcode.Opcode(v.Aux) {
	case opcode.MemoryCopy, opcode.MemoryInit, opcode.TableInit, opcode.TableCopy:
		return v.Aux2 & 0xffffffff, v.Aux2 >> 32
	case opcode.AtomicNotify, opcode.I32AtomicWait, opcode.I64AtomicWait:
		return v.Aux >> 32, v.Aux2
	}
	return v.Aux2, 0
}

// goOperands is the number of operands of instruction op, Go's to run, of
// nargs numeric ones: table.grow's and table.fill's references among them.
func goOperands(op opcode.Opcode, nargs int) int {
	switch op {
	case opcode.TableGrow:
		return 2
	case opcode.TableFill:
		return 3
	}
	return nargs
}

// goOperandCount is the number of operands of instruction Go runs v.
func goOperandCount(v *Value) int { return goOperands(opcode.Opcode(v.Aux), len(v.Args)) }

// goRefs reports whether instruction Go runs v has a reference operand.
func goRefs(v *Value) bool {
	op := opcode.Opcode(v.Aux)
	return op == opcode.TableGrow || op == opcode.TableFill
}

// goArgIndex is the place among its operands of numeric operand i of
// instruction Go runs v: table.grow's count follows its reference, and
// table.fill's count its reference.
func goArgIndex(v *Value, i int) int {
	switch opcode.Opcode(v.Aux) {
	case opcode.TableGrow:
		return 1
	case opcode.TableFill:
		return 2 * i
	}
	return i
}
