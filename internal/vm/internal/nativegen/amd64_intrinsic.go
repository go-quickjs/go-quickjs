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
// mark of a frameless application (setElemOp): charCodeAt answered by
// the kernel its fused form has (charCodeUnit), codePointAt, at and
// charAt by their own (strUnitCall), whose four elemOps are in a row,
// so that any other built-in is told by one compare. Another built-in,
// or a bound one, goes on to other, push's own way on; a receiver that
// is not a string, an argument that is not a number or anything the
// kernel does not answer, to light, nativeBuiltin's call of the
// built-in. R9 is the funcData and RDX its elemOp (push), RSI the
// window, RAX the instruction, which other and the kernels' stores read
// as the call left it.
func (g *nativeGen) strIntrinsic(other, light Label) {
	a := g.a
	strIntrinsicOps()
	code := a.NewLabel()
	a.MovRR(R8, RDX)
	a.OpImm(Sub, R8, elemStrAt, false)
	a.OpImm(Cmp, R8, elemStrCodePointAt-elemStrAt, false)
	a.Jcc(CondA, other)
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
	a.OpImm(Cmp, R8, elemStrCharCodeAt-elemStrAt, false)
	a.Jcc(CondNE, code)
	g.charCodeUnit(light, g.storeB)
	a.Bind(code)
	point, char := a.NewLabel(), a.NewLabel()
	a.OpImm(Cmp, R8, elemStrCodePointAt-elemStrAt, false)
	a.Jcc(CondE, point)
	a.OpImm(Cmp, R8, elemStrCharAt-elemStrAt, false)
	a.Jcc(CondE, char)
	g.strUnitCall(elemStrAt, light)
	a.Bind(char)
	g.strUnitCall(elemStrCharAt, light)
	a.Bind(point)
	g.strUnitCall(elemStrCodePointAt, light)
}

// strIntrinsicOps checks that at's, charAt's, charCodeAt's and
// codePointAt's elemOps are in a row, in that order, as strIntrinsic
// tells them apart.
func strIntrinsicOps() {
	if elemStrCharAt != elemStrAt+1 || elemStrCharCodeAt != elemStrAt+2 || elemStrCodePointAt != elemStrAt+3 {
		panic("nativegen: String.prototype's at, charAt, charCodeAt and codePointAt not in a row")
	}
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

// strUnitCall is the kernel of codePointAt, at or charAt, by op, as the
// built-in answers it: the position the number in X0 gives, NaN as 0,
// truncated -- for at, one below zero counted from the end -- of the
// flat string in the scratch's sV0; at it codePointAt's code point, a
// surrogate pair's where one starts there, and at's and charAt's string
// of the code unit, the runtime's own of a character below 256
// (unitString's); past the string undefined, or charAt's empty string.
// Stored to register B. A rope, the code units of a string not ASCII not
// made yet, a code unit of 256 or more for a string or one whose string
// the runtime has not made yet, go to miss, before anything is written.
func (g *nativeGen) strUnitCall(op int32, miss Label) {
	a := g.a
	out, wide, have, idx := a.NewLabel(), a.NewLabel(), a.NewLabel(), a.NewLabel()
	a.SSEOp(UcomiSD, 0, 0)
	a.Jcc(CondNP, idx)
	a.SSEOp(XorPD, 0, 0)
	a.Bind(idx)
	a.Cvttsd2si(RCX, 0)
	a.Load(RSI, RDI, scr(sV0))
	if op == elemStrAt {
		// A position below zero is the length's past it; one of no int64
		// is 2^63's bits, below zero, past it as well.
		pos := a.NewLabel()
		a.Op(Test, RCX, RCX, true)
		a.Jcc(CondNS, pos)
		a.AddMem(RCX, RSI, offStrLen)
		a.Bind(pos)
	}
	a.CmpMem(RCX, RSI, offStrLen, true)
	a.Jcc(CondAE, out)
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
	if op == elemStrCodePointAt {
		// A high surrogate and a low one after it are a pair's code
		// point.
		a.MovRR(R9, RCX)
		a.LeaIdx(RDX, RDX, RCX, 2, 0)
		a.LoadU16(RCX, RDX, 0)
		a.MovRR(R8, RCX)
		a.OpImm(Sub, R8, 0xD800, false)
		a.OpImm(Cmp, R8, 0x400, false)
		a.Jcc(CondAE, have)
		a.Lea(R9, R9, 1)
		a.CmpMem(R9, RSI, offStrLen, true)
		a.Jcc(CondAE, have)
		a.LoadU16(R9, RDX, 2)
		a.OpImm(Sub, R9, 0xDC00, false)
		a.OpImm(Cmp, R9, 0x400, false)
		a.Jcc(CondAE, have)
		a.ShiftImm(Shl, R8, 10, false)
		a.LeaIdx(RCX, R8, R9, 1, 0x10000)
	} else {
		a.LeaIdx(RDX, RDX, RCX, 2, 0)
		a.LoadU16(RCX, RDX, 0)
	}
	a.Bind(have)
	if op == elemStrCodePointAt {
		a.Cvtsi2sd(0, RCX, true)
		g.storeB(0)
		a.Bind(out)
		a.LoadSD(0, RDI, ncUndef)
		g.storeB(0)
		return
	}
	// The string of the code unit in RCX, the runtime's.
	latin1, table := a.NewLabel(), a.NewLabel()
	a.Load(RDX, RDI, ncRT)
	a.OpImm(Cmp, RCX, 128, false)
	a.Jcc(CondAE, latin1)
	a.Load(RDX, RDX, offRtCharStrs)
	a.Jmp(table)
	a.Bind(latin1)
	a.OpImm(Cmp, RCX, 256, false)
	a.Jcc(CondAE, miss)
	a.Load(RDX, RDX, offRtLatin1Strs)
	a.OpImm(Sub, RCX, 128, false)
	a.Bind(table)
	a.Op(Test, RDX, RDX, true)
	a.Jcc(CondE, miss)
	a.LoadIdx(R8, RDX, RCX, 8, 0)
	a.Op(Test, R8, R8, true)
	a.Jcc(CondE, miss)
	a.Load(R9, RDI, ncStrBits)
	g.decB(RDX)
	a.LeaIdx(RAX, R12, RDX, 1, 0)
	g.storeValue()
	a.Bind(out)
	if op == elemStrCharAt {
		a.Load(R9, RDI, ncStrBits)
		a.Load(R8, RDI, ncEmptyStr)
		g.decB(RDX)
		a.LeaIdx(RAX, R12, RDX, 1, 0)
		g.storeValue()
		return
	}
	a.LoadSD(0, RDI, ncUndef)
	g.storeB(0)
}
