package nativegen

import . "github.com/go-quickjs/go-quickjs/internal/jit/asm/amd64"

// polyOwnStore finds an own writable slot among the site's other shapes.
// The source address stays in RDX; own shares the ordinary store and barrier.
func (g *nativeGen) polyOwnStore(start, own Label) {
	a := g.a
	loop, found := a.NewLabel(), a.NewLabel()
	a.Bind(start)
	a.Load(R8, RCX, off("propCache.poly"))
	a.Op(Test, R8, R8, true)
	a.Jcc(CondE, g.exit)
	a.LoadU8(R9, R8, off("propPoly.n"))
	a.Op(Test, R9, R9, false)
	a.Jcc(CondE, g.exit)
	a.AddMem(R8, RDI, ncPolyEnt)
	a.Load(RCX, RSI, off("Object.shape"))
	a.Bind(loop)
	a.CmpMem(RCX, R8, off("propCache.ownShape"), true)
	a.Jcc(CondE, found)
	a.OpImm(Add, R8, size("propCache"), true)
	a.OpImm(Sub, R9, 1, false)
	a.Jcc(CondNE, loop)
	a.Jmp(g.exit)
	a.Bind(found)
	a.MovRR(RCX, R8)
	a.Jmp(own)
}

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
