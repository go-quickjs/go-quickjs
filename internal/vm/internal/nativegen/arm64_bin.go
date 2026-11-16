package nativegen

import (
	"github.com/go-quickjs/go-quickjs/internal/bytecode"
	. "github.com/go-quickjs/go-quickjs/internal/jit/asm/arm64"
)

// bin is bin of a division or a remainder of two numbers, as amd64's
// (amd64_bin.go).
func (g *armGen) bin() {
	a := g.a
	div := a.NewLabel()
	a.Ubfx(rC, rW, 8, 8)
	a.CmpImm(rC, int64(bytecode.OpDiv), false)
	a.BCond(EQ, div)
	a.CmpImm(rC, int64(bytecode.OpMod), false)
	a.BCond(NE, g.exit)
	g.regB(rD)
	g.num(0, rD)
	g.regC(rD)
	g.num(1, rD)
	// a: an int32 above zero.
	a.Fcvtzs(r8, 0)
	a.Sxtw(r9, r8)
	a.Cmp(r9, r8, true)
	a.BCond(NE, g.exit)
	a.CmpImm(r8, 0, true)
	a.BCond(LE, g.exit)
	a.Scvtf(3, r8, true)
	a.FCmp(3, 0)
	a.BCond(NE, g.exit)
	// b: an int32 not zero.
	a.Fcvtzs(rC, 1)
	a.Sxtw(r9, rC)
	a.Cmp(r9, rC, true)
	a.BCond(NE, g.exit)
	a.Cbz(rC, g.exit, true)
	a.Scvtf(3, rC, true)
	a.FCmp(3, 1)
	a.BCond(NE, g.exit)
	a.Sdiv(r9, r8, rC)
	a.Msub(r9, r9, rC, r8)
	a.Scvtf(0, r9, true)
	g.storeA(0)
	a.Bind(div)
	g.regB(rD)
	g.num(0, rD)
	g.regC(rD)
	g.num(1, rD)
	a.FArith(FDiv, 0, 0, 1)
	g.canon(0)
	g.storeA(0)
}
