package nativegen

import . "github.com/go-quickjs/go-quickjs/internal/jit/asm/arm64"

// getPropNext reads two adjacent data properties without storing the
// intermediate temporary. A miss leaves both reads to their Go handlers.
func (g *armGen) getPropNext() {
	a := g.a
	g.regB(rD)
	g.object(rS, rD)
	for i := 0; i < 2; i++ {
		g.site(rC)
		g.readSlot()
		if i == 0 {
			a.Load(r9, rC, 0)
			g.cmpMem(r9, rCtx, ncObjBits)
			a.BCond(NE, g.exit)
			a.Load(rS, rC, 8)
			a.Load(rW, rIP, 8)
		}
	}
	g.loadValue(rC)
	g.regA(rW)
	a.AddImm(rIP, rIP, 8, true)
	g.storeValue()
}
