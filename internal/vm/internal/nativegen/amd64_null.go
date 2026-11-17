package nativegen

import . "github.com/go-quickjs/go-quickjs/internal/jit/asm/amd64"

// nullJump is arm64_null.go's test of the complete null or undefined tag.
func (g *nativeGen) nullJump(yes, no Label) {
	a := g.a
	g.decA(RDX)
	a.LoadIdx(R9, R12, RDX, 1, 0)
	a.CmpMem(R9, RDI, ncNull, true)
	a.Jcc(CondE, yes)
	a.MovRR(RCX, RAX)
	a.OpImm(And, RCX, 1<<8, false)
	a.Jcc(CondNE, no)
	a.CmpMem(R9, RDI, ncUndef, true)
	a.Jcc(CondE, yes)
	a.CmpMem(R9, RDI, ncObjBits, true)
	a.Jcc(CondNE, no)
	a.LoadIdx(R9, R12, RDX, 1, 8)
	a.LoadU16(RCX, R9, off("Object.flags"))
	a.OpImm(And, RCX, int32(objHTMLDDA), false)
	a.Jcc(CondNE, g.exit)
	a.Jmp(no)
}
