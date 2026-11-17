package nativegen

import (
	"github.com/go-quickjs/go-quickjs/internal/bytecode"
	. "github.com/go-quickjs/go-quickjs/internal/jit/asm/arm64"
)

// binaryLeaf answers a numeric binary leaf without a frame. rC is the
// cache, rS the window, r8 the code and r9 the closure, as at call's
// upvalue-leaf check. Every failure leaves the call's window untouched.
func (g *armGen) binaryLeaf(start, miss Label) {
	a := g.a
	a.Bind(start)
	a.Load(rJ, r8, off("regCode.leafBinary"))
	a.Cbz(rJ, miss, true)
	a.MovRR(rK, r9)
	a.Load(r8, rCtx, ncRT)
	g.cmpMemImm(r8, offRtInterrupt, 1)
	a.BCond(LE, miss)
	for i, shift := range []uint8{8, 32} {
		up, ready := a.NewLabel(), a.NewLabel()
		a.Ubfx(rD, rJ, shift, 17)
		a.TstImm(rD, 1<<16, true)
		a.BCond(NE, up)
		a.Ubfx(r9, rW, 8, 8)
		a.Cmp(rD, r9, true)
		a.BCond(HS, miss)
		a.AddLsl(r8, rS, rD, 4)
		a.AddImm(r8, r8, 32, true)
		a.B(ready)
		a.Bind(up)
		a.AndImm(rD, rD, 0xffff, true)
		g.cmpMem(rD, rK, offClUpvals+8)
		a.BCond(HS, miss)
		a.Load(r8, rK, offClUpvals)
		a.LoadIdx(r8, r8, rD, true)
		a.Load(r8, r8, offUpSlot)
		a.Bind(ready)
		g.numMem(FReg(i), r8, miss)
	}
	sub, mul, result := a.NewLabel(), a.NewLabel(), a.NewLabel()
	a.Ubfx(rD, rJ, 0, 8)
	a.CmpImm(rD, int64(bytecode.RSub), true)
	a.BCond(EQ, sub)
	a.CmpImm(rD, int64(bytecode.RMul), true)
	a.BCond(EQ, mul)
	a.FArith(FAdd, 0, 0, 1)
	a.B(result)
	a.Bind(sub)
	a.FArith(FSub, 0, 0, 1)
	a.B(result)
	a.Bind(mul)
	a.FArith(FMul, 0, 0, 1)
	a.Bind(result)
	g.canon(0)
	g.storeB(0)
}
