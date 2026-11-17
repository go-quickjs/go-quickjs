package vm

import "github.com/go-quickjs/go-quickjs/internal/bytecode"

func init() {
	regTable[bytecode.RJNull], regSlowTable[bytecode.RJNull] = rhNull, rsNull
	regTable[bytecode.RJNotNull], regSlowTable[bytecode.RJNotNull] = rhNotNull, rsNull
}

func rhNull(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	if w.X()&bytecode.RNullStrict == 0 {
		return rhJNull(regs, w, pc, st)
	}
	if regs[w.A()].IsNull() {
		return regJump(st, pc, w.SC())
	}
	return pc + 1
}

func rhNotNull(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	if w.X()&bytecode.RNullStrict == 0 {
		return rhJNotNull(regs, w, pc, st)
	}
	if !regs[w.A()].IsNull() {
		return regJump(st, pc, w.SC())
	}
	return pc + 1
}

func rsNull(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	if w.X()&bytecode.RNullStrict == 0 {
		return rsCompare(regs, w, pc, st)
	}
	if regs[w.A()].IsNull() == (w.Op() == bytecode.RJNull) {
		return regJumpSlow(st, pc, w.SC())
	}
	return pc + 1
}
