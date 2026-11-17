package nativegen

import (
	"github.com/go-quickjs/go-quickjs/internal/bytecode"
	. "github.com/go-quickjs/go-quickjs/internal/jit/asm/arm64"
)

// typedARM64 is the typed variant's own handlers on arm64: amd64's
// (typedAMD64) in arm64's instructions.
func typedARM64(v *variant) (*block, error) {
	loadTypedOffsets()
	g := &armGen{a: &Asm{}, v: v}
	a := g.a
	g.exit, g.exitRet, g.exitAfter = a.NewLabel(), a.NewLabel(), a.NewLabel()
	g.genRet, g.genObj = g.exit, g.exit
	labels := map[bytecode.ROp]Label{}
	var order []bytecode.ROp
	h := func(op bytecode.ROp, body func()) {
		if !v.has(op) {
			return
		}
		l := a.NewLabel()
		a.Bind(l)
		labels[op] = l
		order = append(order, op)
		body()
	}
	// The operands' registers' addresses: A's in rD, B's in rS, C's in
	// r8.
	reg := func(which byte) Reg {
		switch which {
		case 'A':
			g.regA(rD)
			return rD
		case 'B':
			g.regB(rS)
			return rS
		}
		g.regC(r8)
		return r8
	}
	ldI := func(dst Reg, which byte) { a.Load(dst, reg(which), 0) }
	ldF := func(dst FReg, which byte) { a.LoadF(dst, reg(which), 0) }
	stI := func(src Reg) {
		g.regA(rD)
		a.Store(rD, 0, src)
		g.next()
	}
	stF := func(src FReg) {
		g.regA(rD)
		a.StoreF(rD, 0, src)
		g.next()
	}
	box := func(x FReg) {
		g.regA(rD)
		g.noRef(rD)
		a.StoreF(rD, 0, x)
		g.next()
	}
	operands := func(imm bool) {
		ldI(r9, 'B')
		if imm {
			g.immSC(rC)
		} else {
			ldI(rC, 'C')
		}
	}
	intOp := func(op ALU, imm bool) {
		operands(imm)
		a.Op(op, r9, r9, rC, false)
		stI(r9)
	}
	shiftOp := func(op Shift, imm bool) {
		operands(imm)
		a.ShiftReg(op, r9, r9, rC, false)
		stI(r9)
	}
	setOp := func(c Cond) {
		operands(false)
		a.Cmp(r9, rC, false)
		a.Cset(r9, c)
		stI(r9)
	}
	jump := func(c Cond) {
		tk := a.NewLabel()
		a.BCond(c, tk)
		g.next()
		a.Bind(tk)
		g.immSC(rC)
		g.taken(rC)
	}
	jReg := func(c Cond) {
		ldI(r9, 'A')
		ldI(rC, 'B')
		a.Cmp(r9, rC, false)
		jump(c)
	}
	jImm := func(c Cond) {
		ldI(r9, 'A')
		g.immSB(rC)
		a.Cmp(r9, rC, false)
		jump(c)
	}
	fOp := func(op FOp) {
		ldF(0, 'B')
		ldF(1, 'C')
		a.FArith(op, 0, 0, 1)
		stF(0)
	}
	// addr is amd64's: the element's address in rS, or out.
	addr := func(shift uint8, out Label) {
		ldI(r9, 'B')
		g.immSC(rC)
		a.Op(Add, r9, r9, rC, false)
		a.Sxtw(r9, r9)
		if shift > 0 {
			a.AndImm(r9, r9, ^uint64(1<<shift-1), true)
		}
		g.cmpMem(r9, rCtx, ncHeapLen)
		a.BCond(HS, out)
		a.Load(rS, rCtx, ncHeapBase)
		a.Op(Add, rS, rS, r9, true)
	}
	loadI := func(shift uint8, load func(dst, base Reg, disp int32)) {
		out := a.NewLabel()
		addr(shift, out)
		load(r8, rS, 0)
		stI(r8)
		a.Bind(out)
		stI(ZR)
	}
	storeI := func(shift uint8, store func(base Reg, disp int32, src Reg)) {
		out := a.NewLabel()
		addr(shift, out)
		ldI(r8, 'A')
		store(rS, 0, r8)
		a.Bind(out)
		g.next()
	}
	cell := func() {
		a.ShiftImm(Lsr, rC, rW, 32, true)
		g.upval(rS, rC)
	}

	h(bytecode.TMov, func() {
		ldI(r9, 'B')
		stI(r9)
	})
	h(bytecode.TIConst, func() {
		a.ShiftImm(Lsr, r9, rW, 32, true)
		stI(r9)
	})
	h(bytecode.TFConst, func() {
		a.Load(r8, rCtx, ncRC)
		a.Load(r8, r8, offRCFloats)
		a.ShiftImm(Lsr, rC, rW, 32, true)
		a.LoadIdx(r9, r8, rC, true)
		stI(r9)
	})
	h(bytecode.TConvI, func() {
		ldI(r9, 'B')
		a.Cmp(r9, rTag, true)
		a.BCond(HS, g.exit)
		a.FMovToF(0, r9)
		g.toInt32(r8, 0)
		stI(r8)
	})
	h(bytecode.TConvF, func() {
		ldI(r9, 'B')
		a.Cmp(r9, rTag, true)
		a.BCond(HS, g.exit)
		stI(r9)
	})
	h(bytecode.TIBox, func() {
		ldI(r9, 'B')
		a.Scvtf(0, r9, false)
		box(0)
	})
	h(bytecode.TUBox, func() {
		ldI(r9, 'B')
		a.Ucvtf(0, r9, false)
		box(0)
	})
	h(bytecode.TFBox, func() {
		ldF(0, 'B')
		g.canon(0)
		box(0)
	})
	h(bytecode.TAdd, func() { intOp(Add, false) })
	h(bytecode.TSub, func() { intOp(Sub, false) })
	h(bytecode.TMul, func() {
		operands(false)
		a.MulW(r9, r9, rC)
		stI(r9)
	})
	div := func(signed, rem bool) {
		zero := a.NewLabel()
		operands(false)
		if signed {
			a.Sxtw(r9, r9)
			a.Sxtw(rC, rC)
		} else {
			a.MovRR32(r9, r9)
			a.MovRR32(rC, rC)
		}
		a.Cbz(rC, zero, true)
		// 64 bits, so that MinInt32 / -1 is MinInt32's bits.
		if signed {
			a.Sdiv(r8, r9, rC)
		} else {
			a.Udiv(r8, r9, rC)
		}
		if rem {
			a.Msub(r8, r8, rC, r9)
		}
		stI(r8)
		a.Bind(zero)
		stI(ZR)
	}
	h(bytecode.TDivS, func() { div(true, false) })
	h(bytecode.TDivU, func() { div(false, false) })
	h(bytecode.TRemS, func() { div(true, true) })
	h(bytecode.TRemU, func() { div(false, true) })
	h(bytecode.TAnd, func() { intOp(And, false) })
	h(bytecode.TOr, func() { intOp(Orr, false) })
	h(bytecode.TXor, func() { intOp(Eor, false) })
	h(bytecode.TShl, func() { shiftOp(Lsl, false) })
	h(bytecode.TShr, func() { shiftOp(Asr, false) })
	h(bytecode.TUShr, func() { shiftOp(Lsr, false) })
	h(bytecode.TAddI, func() { intOp(Add, true) })
	h(bytecode.TAndI, func() { intOp(And, true) })
	h(bytecode.TOrI, func() { intOp(Orr, true) })
	h(bytecode.TXorI, func() { intOp(Eor, true) })
	h(bytecode.TShlI, func() { shiftOp(Lsl, true) })
	h(bytecode.TShrI, func() { shiftOp(Asr, true) })
	h(bytecode.TUShrI, func() { shiftOp(Lsr, true) })
	h(bytecode.TNeg, func() {
		ldI(r9, 'B')
		a.Neg(r9, r9, false)
		stI(r9)
	})
	h(bytecode.TNot, func() {
		ldI(r9, 'B')
		a.Mvn(r9, r9, false)
		stI(r9)
	})
	h(bytecode.TLNot, func() {
		ldI(r9, 'B')
		a.CmpImm(r9, 0, false)
		a.Cset(r9, EQ)
		stI(r9)
	})
	h(bytecode.TLtS, func() { setOp(LT) })
	h(bytecode.TLeS, func() { setOp(LE) })
	h(bytecode.TLtU, func() { setOp(LO) })
	h(bytecode.TLeU, func() { setOp(LS) })
	h(bytecode.TEq, func() { setOp(EQ) })
	h(bytecode.TNe, func() { setOp(NE) })
	h(bytecode.TJLtS, func() { jReg(LT) })
	h(bytecode.TJLeS, func() { jReg(LE) })
	h(bytecode.TJLtU, func() { jReg(LO) })
	h(bytecode.TJLeU, func() { jReg(LS) })
	h(bytecode.TJEq, func() { jReg(EQ) })
	h(bytecode.TJNe, func() { jReg(NE) })
	h(bytecode.TJLtSI, func() { jImm(LT) })
	h(bytecode.TJLeSI, func() { jImm(LE) })
	h(bytecode.TJGtSI, func() { jImm(GT) })
	h(bytecode.TJGeSI, func() { jImm(GE) })
	h(bytecode.TJEqI, func() { jImm(EQ) })
	h(bytecode.TJNeI, func() { jImm(NE) })
	h(bytecode.TJZ, func() {
		ldI(r9, 'A')
		a.CmpImm(r9, 0, false)
		jump(EQ)
	})
	h(bytecode.TJNZ, func() {
		ldI(r9, 'A')
		a.CmpImm(r9, 0, false)
		jump(NE)
	})
	h(bytecode.TFAdd, func() { fOp(FAdd) })
	h(bytecode.TFSub, func() { fOp(FSub) })
	h(bytecode.TFMul, func() { fOp(FMul) })
	h(bytecode.TFDiv, func() { fOp(FDiv) })
	h(bytecode.TFNeg, func() {
		ldF(0, 'B')
		a.FNeg(0, 0)
		stF(0)
	})
	h(bytecode.TFAbs, func() {
		ldF(0, 'B')
		a.FAbs(0, 0)
		stF(0)
	})
	h(bytecode.TFSqrt, func() {
		ldF(0, 'B')
		a.FSqrt(0, 0)
		stF(0)
	})
	h(bytecode.TFRound, func() {
		ldF(0, 'B')
		a.FCvtToSingle(1, 0)
		a.FCvtFromSingle(0, 1)
		stF(0)
	})
	// After FCMP: less is MI and less or equal LS, each false of a NaN;
	// their negations PL and HI true of one.
	fset := func(c Cond) {
		ldF(0, 'B')
		ldF(1, 'C')
		a.FCmp(0, 1)
		a.Cset(r9, c)
		stI(r9)
	}
	h(bytecode.TFLt, func() { fset(MI) })
	h(bytecode.TFLe, func() { fset(LS) })
	h(bytecode.TFEq, func() { fset(EQ) })
	h(bytecode.TFNe, func() { fset(NE) })
	fjump := func(c Cond) {
		ldF(0, 'A')
		ldF(1, 'B')
		a.FCmp(0, 1)
		jump(c)
	}
	h(bytecode.TJFLt, func() { fjump(MI) })
	h(bytecode.TJFNLt, func() { fjump(PL) })
	h(bytecode.TJFLe, func() { fjump(LS) })
	h(bytecode.TJFNLe, func() { fjump(HI) })
	h(bytecode.TJFEq, func() { fjump(EQ) })
	h(bytecode.TJFNe, func() { fjump(NE) })
	h(bytecode.TI2FS, func() {
		ldI(r9, 'B')
		a.Scvtf(0, r9, false)
		stF(0)
	})
	h(bytecode.TI2FU, func() {
		ldI(r9, 'B')
		a.Ucvtf(0, r9, false)
		stF(0)
	})
	h(bytecode.TF2I, func() {
		ldF(0, 'B')
		g.toInt32(r8, 0)
		stI(r8)
	})
	h(bytecode.TLd8S, func() { loadI(0, a.LoadS8) })
	h(bytecode.TLd8U, func() { loadI(0, a.LoadU8) })
	h(bytecode.TLd16S, func() { loadI(1, a.LoadS16) })
	h(bytecode.TLd16U, func() { loadI(1, a.LoadU16) })
	h(bytecode.TLd32, func() { loadI(2, a.LoadU32) })
	loadF := func(shift uint8, f32 bool) {
		out := a.NewLabel()
		addr(shift, out)
		if f32 {
			a.LoadS(1, rS, 0)
			a.FCvtFromSingle(0, 1)
		} else {
			a.LoadF(0, rS, 0)
		}
		stF(0)
		a.Bind(out)
		a.Load(r8, rCtx, ncNaN)
		stI(r8)
	}
	h(bytecode.TLdF32, func() { loadF(2, true) })
	h(bytecode.TLdF64, func() { loadF(3, false) })
	h(bytecode.TSt8, func() { storeI(0, a.Store8) })
	h(bytecode.TSt16, func() { storeI(1, a.Store16) })
	h(bytecode.TSt32, func() { storeI(2, a.Store32) })
	h(bytecode.TStF32, func() {
		out := a.NewLabel()
		addr(2, out)
		ldF(0, 'A')
		a.FCvtToSingle(1, 0)
		a.StoreS(rS, 0, 1)
		a.Bind(out)
		g.next()
	})
	h(bytecode.TStF64, func() { storeI(3, a.Store) })
	h(bytecode.TGGetI, func() {
		cell()
		a.Load(r9, rS, 0)
		a.Cmp(r9, rTag, true)
		a.BCond(HS, g.exit)
		a.FMovToF(0, r9)
		g.toInt32(r8, 0)
		stI(r8)
	})
	h(bytecode.TGGetF, func() {
		cell()
		a.Load(r9, rS, 0)
		a.Cmp(r9, rTag, true)
		a.BCond(HS, g.exit)
		stI(r9)
	})
	h(bytecode.TGSetI, func() {
		cell()
		a.Load(rT, rS, 8)
		a.Cbnz(rT, g.exit, true)
		ldI(r9, 'A')
		a.Scvtf(0, r9, false)
		a.StoreF(rS, 0, 0)
		g.next()
	})
	h(bytecode.TGSetF, func() {
		cell()
		a.Load(rT, rS, 8)
		a.Cbnz(rT, g.exit, true)
		ldF(0, 'A')
		g.canon(0)
		a.StoreF(rS, 0, 0)
		g.next()
	})

	for n, l := range g.exits {
		if n == 0 {
			continue
		}
		a.Bind(l)
		g.storeImm(rCtx, scr(15), int64(n))
		a.B(g.exit)
	}
	b := &block{at: map[bytecode.ROp]int{}, order: order}
	if len(order) == 0 {
		return b, nil
	}
	far1 := func(slot int32) {
		a.Load(rJ, rTab, 8*slot)
		a.Br(rJ)
	}
	a.Bind(g.exit)
	far1(slotExit)
	a.Bind(g.exitRet)
	far1(slotExitRet)
	code, err := a.Finish()
	if err != nil {
		return nil, err
	}
	b.code, b.exit, b.exitRet = code, a.Offset(g.exit), a.Offset(g.exitRet)
	for _, op := range order {
		b.at[op] = a.Offset(labels[op])
	}
	return b, nil
}
