package vm

import "github.com/go-quickjs/go-quickjs/internal/bytecode"

func init() {
	regTable[bytecode.RGetPropNext], regSlowTable[bytecode.RGetPropNext] = rhGetPropNext, rsGetProp
}

func rhGetPropNext(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	if next := rhGetProp(regs, w, pc, st); next == pc+1 {
		partner := regAt(st.c.code, pc+1)
		if after := rhGetProp(regs, partner, pc+1, st); after == pc+2 {
			return after
		}
		return pc + 1
	}
	return ^pc
}
