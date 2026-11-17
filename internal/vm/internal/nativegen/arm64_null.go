package nativegen

import . "github.com/go-quickjs/go-quickjs/internal/jit/asm/arm64"

// nullJump tests the complete tag, without materializing a second value.
// Only loose equality can need document.all's stand-in handled by Go.
func (g *armGen) nullJump(yes, no Label) {
	a := g.a
	g.regA(rD)
	a.Load(r9, rD, 0)
	g.cmpMem(r9, rCtx, ncNull)
	a.BCond(EQ, yes)
	a.TstImm(rW, 1<<8, false)
	a.BCond(NE, no)
	g.cmpMem(r9, rCtx, ncUndef)
	a.BCond(EQ, yes)
	g.cmpMem(r9, rCtx, ncObjBits)
	a.BCond(NE, no)
	a.Load(r9, rD, 8)
	a.LoadU16(rC, r9, off("Object.flags"))
	a.TstImm(rC, uint64(objHTMLDDA), false)
	a.BCond(NE, g.exit)
	a.B(no)
}
