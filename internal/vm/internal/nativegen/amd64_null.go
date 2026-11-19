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

// jBool is jbool: a jump by SC where R[A] is, or is not where X's
// RCmpTrue is clear, the boolean X&1, which only that boolean's word
// is: False's word with X's low bit at bit 8.
func (g *nativeGen) jBool() {
	a := g.a
	done := a.NewLabel()
	g.decA(RDX)
	a.LoadIdx(R9, R12, RDX, 1, 0)
	a.MovRR(RCX, RAX)
	a.OpImm(And, RCX, 1<<8, false)
	a.Load(R8, RDI, ncFalse)
	a.Op(Or, R8, RCX, true)
	a.Op(Cmp, R9, R8, true)
	a.Setcc(CondE, RCX)
	a.MovZX8(RCX, RCX)
	a.MovRR(R8, RAX)
	a.ShiftImm(Shr, R8, 15, true)
	a.OpImm(And, R8, 1, false)
	a.Op(Cmp, RCX, R8, false)
	a.Jcc(CondNE, done)
	g.immSC(RCX)
	g.taken(RCX)
	a.Bind(done)
	g.next()
}
