package nativegen

import (
	"github.com/go-quickjs/go-quickjs/internal/bytecode"
	. "github.com/go-quickjs/go-quickjs/internal/jit/asm/amd64"
)

// binaryLeaf is arm64's frameless numeric binary leaf. The cache in RCX
// is restored on a miss; RSI and RAX keep the window and instruction.
func (g *nativeGen) binaryLeaf(start, fallback Label) {
	a := g.a
	miss := a.NewLabel()
	a.Bind(start)
	a.Store(RDI, scr(sE), RCX)
	a.Load(RCX, R8, off("regCode.leafBinary"))
	a.Op(Test, RCX, RCX, true)
	a.Jcc(CondE, miss)
	a.MovRR(R8, R9)
	a.Load(R9, RDI, ncRT)
	a.CmpMemImm8(R9, offRtInterrupt, 1)
	a.Jcc(CondLE, miss)
	for i, shift := range []uint8{8, 32} {
		up, ready := a.NewLabel(), a.NewLabel()
		a.MovRR(RDX, RCX)
		a.ShiftImm(Shr, RDX, shift, true)
		a.OpImm(And, RDX, 0x1ffff, false)
		a.OpImm(Cmp, RDX, 1<<16, false)
		a.Jcc(CondAE, up)
		a.MovRR(R9, RAX)
		a.ShiftImm(Shr, R9, 8, true)
		a.MovZX8(R9, R9)
		a.Op(Cmp, RDX, R9, true)
		a.Jcc(CondAE, miss)
		a.ShiftImm(Shl, RDX, 4, true)
		a.LeaIdx(R9, RSI, RDX, 1, 32)
		a.Jmp(ready)
		a.Bind(up)
		a.OpImm(And, RDX, 0xffff, false)
		a.CmpMem(RDX, R8, offClUpvals+8, true)
		a.Jcc(CondAE, miss)
		a.Load(R9, R8, offClUpvals)
		a.LoadIdx(R9, R9, RDX, 8, 0)
		a.Load(R9, R9, offUpSlot)
		a.Bind(ready)
		a.CmpMem(R10, R9, 0, true)
		a.Jcc(CondBE, miss)
		a.LoadSD(XReg(i), R9, 0)
	}
	sub, mul, result := a.NewLabel(), a.NewLabel(), a.NewLabel()
	a.MovZX8(RDX, RCX)
	a.OpImm(Cmp, RDX, int32(bytecode.RSub), false)
	a.Jcc(CondE, sub)
	a.OpImm(Cmp, RDX, int32(bytecode.RMul), false)
	a.Jcc(CondE, mul)
	a.SSEOp(AddSD, 0, 1)
	a.Jmp(result)
	a.Bind(sub)
	a.SSEOp(SubSD, 0, 1)
	a.Jmp(result)
	a.Bind(mul)
	a.SSEOp(MulSD, 0, 1)
	a.Bind(result)
	g.canon(0)
	g.storeB(0)
	a.Bind(miss)
	a.Load(RCX, RDI, scr(sE))
	a.Jmp(fallback)
}
