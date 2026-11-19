package nativegen

import . "github.com/go-quickjs/go-quickjs/internal/jit/asm/amd64"

// getPropNext reads two adjacent data properties without storing the
// intermediate temporary. A miss leaves both reads to their Go handlers.
func (g *nativeGen) getPropNext() {
	a := g.a
	g.decB(RDX)
	g.object(RSI, RDX)
	for i := 0; i < 2; i++ {
		g.site(RCX)
		g.readSlot()
		if i == 0 {
			a.Load(R9, RCX, 0)
			a.CmpMem(R9, RDI, ncObjBits, true)
			a.Jcc(CondNE, g.exit)
			a.Load(RSI, RCX, 8)
			a.Load(RAX, RBX, 8)
		}
	}
	g.loadValue(RCX)
	g.decA(RDX)
	a.LeaIdx(RAX, R12, RDX, 1, 0)
	a.OpImm(Add, RBX, 8, true)
	g.storeValue()
}
