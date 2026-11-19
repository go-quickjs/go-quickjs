package vm

import (
	"math"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
)

// The branches fused with their test. This file sorts before
// reg_table_cold.go, so that the layout lock's K pad takes up what it adds.
//
// instanceof_next's Go handler is instanceof's: it stores the boolean the
// native handler branches on without, and goes on at the partner, which
// branches on it (bytecode.RInstanceOfNext).
func init() {
	regTable[bytecode.RInstanceOfNext] = rsInstanceOf
	regTable[bytecode.RJBool], regSlowTable[bytecode.RJBool] = rhJBool, rsJBool
}

// rhJBool is jbool: a jump where R[A] is, or is not where X's RCmpTrue is
// clear, the boolean X&1, which only that boolean's word is.
func rhJBool(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	if (math.Float64bits(regs[w.A()].num) == math.Float64bits(Bool(w.X()&1 != 0).num)) == (w.X()&bytecode.RCmpTrue != 0) {
		return regJump(st, pc, w.SC())
	}
	return pc + 1
}

// rsJBool is jbool's slow function, by StrictEquals.
func rsJBool(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	if regs[w.A()].StrictEquals(Bool(w.X()&1 != 0)) == (w.X()&bytecode.RCmpTrue != 0) {
		return regJumpSlow(st, pc, w.SC())
	}
	return pc + 1
}
