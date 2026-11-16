package nativegen

import (
	"github.com/go-quickjs/go-quickjs/internal/bytecode"
	. "github.com/go-quickjs/go-quickjs/internal/jit/asm/amd64"
)

// bin is bin of a division or a remainder of two numbers, as regBinary
// computes them: a / b by the machine's division, its NaN the canonical
// one; a % b of two int32s, a positive, b not zero, by an integer
// division, as jsMod; any other remainder, any other operator or operand
// Go's.
func (g *nativeGen) bin() {
	a := g.a
	div := a.NewLabel()
	a.MovRR(RCX, RAX)
	a.ShiftImm(Shr, RCX, 8, true)
	a.MovZX8(RCX, RCX)
	a.OpImm(Cmp, RCX, int32(bytecode.OpDiv), false)
	a.Jcc(CondE, div)
	a.OpImm(Cmp, RCX, int32(bytecode.OpMod), false)
	a.Jcc(CondNE, g.exit)
	g.decB(RDX)
	g.num(0, RDX)
	g.decC(RDX)
	g.num(1, RDX)
	// a: an int32 above zero.
	a.Cvttsd2si(R8, 0)
	a.Movsxd(R9, R8)
	a.Op(Cmp, R9, R8, true)
	a.Jcc(CondNE, g.exit)
	a.Op(Test, R8, R8, true)
	a.Jcc(CondLE, g.exit)
	a.Cvtsi2sd(3, R8, true)
	a.SSEOp(UcomiSD, 3, 0)
	a.Jcc(CondNE, g.exit)
	a.Jcc(CondP, g.exit)
	// b: an int32 not zero.
	a.Cvttsd2si(RCX, 1)
	a.Movsxd(R9, RCX)
	a.Op(Cmp, R9, RCX, true)
	a.Jcc(CondNE, g.exit)
	a.Op(Test, RCX, RCX, true)
	a.Jcc(CondE, g.exit)
	a.Cvtsi2sd(3, RCX, true)
	a.SSEOp(UcomiSD, 3, 1)
	a.Jcc(CondNE, g.exit)
	a.Jcc(CondP, g.exit)
	a.MovRR(RAX, R8)
	a.Cqo()
	a.Idiv(RCX)
	a.Cvtsi2sd(0, RDX, true)
	a.Load(RAX, RBX, 0)
	g.storeA(0)
	a.Bind(div)
	g.decB(RDX)
	g.num(0, RDX)
	g.decC(RDX)
	g.num(1, RDX)
	a.SSEOp(DivSD, 0, 1)
	g.canon(0)
	g.storeA(0)
}
