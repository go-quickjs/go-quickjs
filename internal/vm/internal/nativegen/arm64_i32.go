package nativegen

import (
	. "github.com/go-quickjs/go-quickjs/internal/jit/asm/arm64"
)

// int32Of is add_i32's, sub_i32's and add_imm_i32's R[A] = ToInt32(R[B]
// op R[C]), or op the immediate SC, as amd64's (amd64_i32.go): FCVTZS
// to 64 bits, whose low half SCVTF converts as a signed 32-bit integer. A
// NaN converts to 0, its ToInt32; an infinity or a sum of 2^63 or more in
// magnitude saturates, which exits (toInt32).
func (g *armGen) int32Of(op FOp, imm bool) {
	a := g.a
	g.regB(rS)
	g.numMem(0, rS, g.exit)
	if imm {
		g.immSC(rC)
		a.Scvtf(1, rC, true)
	} else {
		g.regC(r8)
		g.numMem(1, r8, g.exit)
	}
	a.FArith(op, 0, 0, 1)
	g.toInt32(r9, 0)
	g.int32Out(0, r9, false)
	g.storeA(0)
}
