package nativegen

import . "github.com/go-quickjs/go-quickjs/internal/jit/asm/arm64"

// polyOwnStore is amd64's probe, retaining the source address in rD.
func (g *armGen) polyOwnStore(start, own Label) {
	a := g.a
	loop, found := a.NewLabel(), a.NewLabel()
	a.Bind(start)
	a.Load(r8, rC, off("propCache.poly"))
	a.Cbz(r8, g.exit, true)
	a.LoadU8(r9, r8, off("propPoly.n"))
	a.Cbz(r9, g.exit, false)
	g.addMem(r8, rCtx, ncPolyEnt)
	a.Load(rC, rS, off("Object.shape"))
	a.Bind(loop)
	g.cmpMem(rC, r8, off("propCache.ownShape"))
	a.BCond(EQ, found)
	a.AddImm(r8, r8, int64(size("propCache")), true)
	a.SubsImm(r9, r9, 1, false)
	a.BCond(NE, loop)
	a.B(g.exit)
	a.Bind(found)
	a.MovRR(rC, r8)
	a.B(own)
}

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
