package nativegen

import . "github.com/go-quickjs/go-quickjs/internal/jit/asm/arm64"

// strIntrinsic is amd64's (amd64_intrinsic.go): call_method_w's call of
// a String.prototype method its site cached, known by its elemOp and
// answered by its kernel. r9 is the funcData and rD its elemOp (push),
// rS the window, rW the instruction.
func (g *armGen) strIntrinsic(other, light Label) {
	a := g.a
	strIntrinsicOps()
	code := a.NewLabel()
	a.AddImm(r8, rD, -int64(elemStrAt), false)
	a.CmpImm(r8, int64(elemStrCodePointAt-elemStrAt), false)
	a.BCond(HI, other)
	a.LoadU8(rD, r9, offFDBound)
	a.Cbnz(rD, other, false)
	// The receiver a string, the argument a number.
	a.Load(rD, rS, 16)
	g.cmpMem(rD, rCtx, ncStrBits)
	a.BCond(NE, light)
	a.Load(rD, rS, 24)
	a.Store(rCtx, scr(sV0), rD)
	a.AddImm(rD, rS, 32, true)
	g.numOr(0, rD, light)
	a.CmpImm(r8, int64(elemStrCharCodeAt-elemStrAt), false)
	a.BCond(NE, code)
	g.charCodeUnit(light, g.storeB)
	a.Bind(code)
	point, char := a.NewLabel(), a.NewLabel()
	a.CmpImm(r8, int64(elemStrCodePointAt-elemStrAt), false)
	a.BCond(EQ, point)
	a.CmpImm(r8, int64(elemStrCharAt-elemStrAt), false)
	a.BCond(EQ, char)
	g.strUnitCall(elemStrAt, light)
	a.Bind(char)
	g.strUnitCall(elemStrCharAt, light)
	a.Bind(point)
	g.strUnitCall(elemStrCodePointAt, light)
}

// charCodeUnit is amd64's: the code unit of the flat string in the
// scratch's sV0 at the position the number in D0 gives, stored by store,
// or NaN; anything else goes to miss.
func (g *armGen) charCodeUnit(miss Label, store func(FReg)) {
	a := g.a
	nan, wide, have, idx := a.NewLabel(), a.NewLabel(), a.NewLabel(), a.NewLabel()
	a.FCmp(0, 0)
	a.BCond(VC, idx)
	a.FMovToF(0, ZR)
	a.Bind(idx)
	a.Fcvtzs(rC, 0)
	a.Load(rS, rCtx, scr(sV0))
	g.cmpMem(rC, rS, offStrLen)
	a.BCond(HS, nan)
	g.cmpMemImm(rS, offStrLeft, 0)
	a.BCond(NE, miss)
	a.LoadU8(rD, rS, offStrASCII)
	a.Cbz(rD, wide, false)
	a.Load(rD, rS, offStrS)
	a.Op(Add, rD, rD, rC, true)
	a.LoadU8(rC, rD, 0)
	a.B(have)
	a.Bind(wide)
	a.Load(rD, rS, offStrAux)
	a.Cbz(rD, miss, true)
	a.AddLsl(rD, rD, rC, 1)
	a.LoadU16(rC, rD, 0)
	a.Bind(have)
	a.Scvtf(0, rC, true)
	store(0)
	a.Bind(nan)
	a.LoadF(0, rCtx, ncNaN)
	store(0)
}

// strUnitCall is amd64's: codePointAt's, at's or charAt's answer, by op,
// at the position the number in D0 gives of the flat string in the
// scratch's sV0, stored to register B; anything else goes to miss.
// FCVTZS saturates, so a position of no int64 is past the string either
// way, at's below zero too.
func (g *armGen) strUnitCall(op int32, miss Label) {
	a := g.a
	out, wide, have, idx := a.NewLabel(), a.NewLabel(), a.NewLabel(), a.NewLabel()
	a.FCmp(0, 0)
	a.BCond(VC, idx)
	a.FMovToF(0, ZR)
	a.Bind(idx)
	a.Fcvtzs(rC, 0)
	a.Load(rS, rCtx, scr(sV0))
	if op == elemStrAt {
		pos := a.NewLabel()
		a.CmpImm(rC, 0, true)
		a.BCond(GE, pos)
		g.addMem(rC, rS, offStrLen)
		a.Bind(pos)
	}
	g.cmpMem(rC, rS, offStrLen)
	a.BCond(HS, out)
	g.cmpMemImm(rS, offStrLeft, 0)
	a.BCond(NE, miss)
	a.LoadU8(rD, rS, offStrASCII)
	a.Cbz(rD, wide, false)
	a.Load(rD, rS, offStrS)
	a.Op(Add, rD, rD, rC, true)
	a.LoadU8(rC, rD, 0)
	a.B(have)
	a.Bind(wide)
	a.Load(rD, rS, offStrAux)
	a.Cbz(rD, miss, true)
	a.AddLsl(rD, rD, rC, 1)
	if op == elemStrCodePointAt {
		// A high surrogate and a low one after it are a pair's code
		// point.
		a.AddImm(r9, rC, 1, true)
		a.LoadU16(rC, rD, 0)
		a.AddImm(r8, rC, -0xD800, false)
		a.CmpImm(r8, 0x400, false)
		a.BCond(HS, have)
		g.cmpMem(r9, rS, offStrLen)
		a.BCond(HS, have)
		a.LoadU16(r9, rD, 2)
		a.AddImm(r9, r9, -0xDC00, false)
		a.CmpImm(r9, 0x400, false)
		a.BCond(HS, have)
		a.ShiftImm(Lsl, r8, r8, 10, false)
		a.Op(Add, rC, r8, r9, true)
		a.AddImm(rC, rC, 0x10000, true)
	} else {
		a.LoadU16(rC, rD, 0)
	}
	a.Bind(have)
	if op == elemStrCodePointAt {
		a.Scvtf(0, rC, true)
		g.storeB(0)
		a.Bind(out)
		a.LoadF(0, rCtx, ncUndef)
		g.storeB(0)
		return
	}
	// The string of the code unit in rC, the runtime's.
	latin1, table := a.NewLabel(), a.NewLabel()
	a.Load(rD, rCtx, ncRT)
	a.CmpImm(rC, 128, false)
	a.BCond(HS, latin1)
	a.Load(rD, rD, offRtCharStrs)
	a.B(table)
	a.Bind(latin1)
	a.CmpImm(rC, 256, false)
	a.BCond(HS, miss)
	a.Load(rD, rD, offRtLatin1Strs)
	a.AddImm(rC, rC, -128, true)
	a.Bind(table)
	a.Cbz(rD, miss, true)
	a.LoadIdx(r8, rD, rC, true)
	a.Cbz(r8, miss, true)
	a.Load(r9, rCtx, ncStrBits)
	g.regB(rW)
	g.storeValue()
	a.Bind(out)
	if op == elemStrCharAt {
		a.Load(r9, rCtx, ncStrBits)
		a.Load(r8, rCtx, ncEmptyStr)
		g.regB(rW)
		g.storeValue()
		return
	}
	a.LoadF(0, rCtx, ncUndef)
	g.storeB(0)
}
