package nativegen

import (
	. "github.com/go-quickjs/go-quickjs/internal/jit/asm/amd64"
)

// int32Of is add_i32's, sub_i32's and add_imm_i32's R[A] = ToInt32(R[B]
// op R[C]), or op the immediate SC: arith's front half and bitImm's back
// half, no intermediate stored -- the two numbers' sum or difference,
// its truncation's low 32 bits, sign-extended as the int32 it is. A NaN,
// an infinity or a sum of 2^63 or more in magnitude exits (toInt32), and
// Go's slow path makes it (rsAddI32), as it does an operand that is no
// number.
func (g *nativeGen) int32Of(op SSE, imm bool) {
	a := g.a
	g.decB(RSI)
	g.numMem(0, RSI, g.exit)
	if imm {
		g.immSC(RCX)
		a.Cvtsi2sd(1, RCX, true)
	} else {
		g.decC(R8)
		g.numMem(1, R8, g.exit)
	}
	a.SSEOp(op, 0, 1)
	g.toInt32(R9, 0)
	a.Cvtsi2sd(0, R9, false)
	g.storeA(0)
}
