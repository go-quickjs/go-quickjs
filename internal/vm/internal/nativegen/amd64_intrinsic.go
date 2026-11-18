package nativegen

import . "github.com/go-quickjs/go-quickjs/internal/jit/asm/amd64"

// strIntrinsic is call_method_w's call, of one argument, of a built-in
// its site cached that is one of String.prototype's methods the handlers
// answer themselves, on a string: the target, the site's cache entry,
// decides, as V8's call feedback does, not the instruction, so the call
// is the one call_method_w every other callee has -- the method read
// before the argument was evaluated, a script override or another
// built-in the cache's like any other callee -- and the JIT reads the
// same feedback. Each method is known by its elemOp, the built-in's own
// mark of a frameless application (setElemOp), and answered by the
// kernel its fused form has: charCodeAt by char_code_at's
// (charCodeUnit). Another built-in, or a bound one, goes on to other,
// push's own way on; a receiver that is not a string, an argument that
// is not a number or anything the kernel does not answer, to light,
// nativeBuiltin's call of the built-in. R9 is the funcData and RDX its
// elemOp (push), RSI the window, RAX the instruction.
func (g *nativeGen) strIntrinsic(other, light Label) {
	a := g.a
	a.OpImm(Cmp, RDX, int32(elemStrCharCodeAt), false)
	a.Jcc(CondNE, other)
	a.LoadU8(RDX, R9, offFDBound)
	a.Op(Test, RDX, RDX, false)
	a.Jcc(CondNE, other)
	// The receiver a string, the argument a number.
	a.Load(RDX, RSI, 16)
	a.CmpMem(RDX, RDI, ncStrBits, true)
	a.Jcc(CondNE, light)
	a.Load(RDX, RSI, 24)
	a.Store(RDI, scr(sV0), RDX)
	a.Load(R9, RSI, 32)
	a.Op(Cmp, R9, R10, true)
	a.Jcc(CondAE, light)
	a.MovQToX(0, R9)
	g.charCodeUnit(light, g.storeB)
}

// charCodeUnit is charCodeAt's kernel: the code unit of the flat string
// in the scratch's sV0 at the position the number in X0 gives, NaN as 0,
// truncated, stored by store as a number, or NaN past the string's
// length; a rope, or the code units of a string not ASCII not made yet,
// go to miss, before anything is written.
func (g *nativeGen) charCodeUnit(miss Label, store func(XReg)) {
	a := g.a
	nan, wide, have, idx := a.NewLabel(), a.NewLabel(), a.NewLabel(), a.NewLabel()
	a.SSEOp(UcomiSD, 0, 0)
	a.Jcc(CondNP, idx)
	a.SSEOp(XorPD, 0, 0)
	a.Bind(idx)
	a.Cvttsd2si(RCX, 0)
	a.Load(RSI, RDI, scr(sV0))
	a.CmpMem(RCX, RSI, offStrLen, true)
	a.Jcc(CondAE, nan)
	a.CmpMemImm8(RSI, offStrLeft, 0)
	a.Jcc(CondNE, miss)
	a.LoadU8(RDX, RSI, offStrASCII)
	a.Op(Test, RDX, RDX, false)
	a.Jcc(CondE, wide)
	a.Load(RDX, RSI, offStrS)
	a.LeaIdx(RDX, RDX, RCX, 1, 0)
	a.LoadU8(RCX, RDX, 0)
	a.Jmp(have)
	a.Bind(wide)
	a.Load(RDX, RSI, offStrAux)
	a.Op(Test, RDX, RDX, true)
	a.Jcc(CondE, miss)
	a.LeaIdx(RDX, RDX, RCX, 2, 0)
	a.LoadU16(RCX, RDX, 0)
	a.Bind(have)
	a.Cvtsi2sd(0, RCX, true)
	store(0)
	a.Bind(nan)
	a.LoadSD(0, RDI, ncNaN)
	store(0)
}
