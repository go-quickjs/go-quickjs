package nativegen

import (
	"github.com/go-quickjs/go-quickjs/internal/bytecode"
	. "github.com/go-quickjs/go-quickjs/internal/jit/asm/amd64"
)

// typedAMD64 is the typed variant's own handlers on amd64: the typed
// operations' (typedNativeOps). An I register's low 32 bits are its
// int32, which an int operation reads and writes, the 32 above them
// unread; an F register is a float64's bits. No typed handler writes a
// register's pointer word: a box into a Value register that holds a
// pointer exits, as does anything a handler does not answer -- a
// conversion of what is no number, a module variable's cell of no
// number, a backward branch whose budget is spent -- to the typed table's
// Go handler. The heap is the context's (heapBase, heapLen), which Go
// keeps current across every exit.
func typedAMD64(v *variant) (*block, error) {
	loadTypedOffsets()
	g := &nativeGen{a: &Asm{}, v: v}
	a := g.a
	g.exit, g.exitRet, g.exitAfter = a.NewLabel(), a.NewLabel(), a.NewLabel()
	g.genRet, g.genObj = g.exit, g.exit
	labels := map[bytecode.ROp]Label{}
	var order []bytecode.ROp
	h := func(op bytecode.ROp, body func()) {
		if !v.has(op) {
			return
		}
		a.Align(16)
		l := a.NewLabel()
		a.Bind(l)
		labels[op] = l
		order = append(order, op)
		body()
	}
	// The operands' registers' offsets in the window: A's in RDX, B's in
	// RSI, C's in R8.
	ldI := func(dst Reg, which byte) {
		off := map[byte]Reg{'A': RDX, 'B': RSI, 'C': R8}[which]
		switch which {
		case 'A':
			g.decA(off)
		case 'B':
			g.decB(off)
		default:
			g.decC(off)
		}
		a.LoadIdx(dst, R12, off, 1, 0)
	}
	ldF := func(dst XReg, which byte) {
		ldI(R9, which)
		a.MovQToX(dst, R9)
	}
	// stI stores src's low half, an int32, to I register A.
	stI := func(src Reg) {
		g.decA(RDX)
		a.StoreIdx(R12, RDX, 1, 0, src)
		g.next()
	}
	stF := func(src XReg) {
		g.decA(RDX)
		a.StoreSDIdx(R12, RDX, 1, 0, src)
		g.next()
	}
	// Value A, where it holds no pointer, takes the number in x.
	box := func(x XReg) {
		g.decA(RDX)
		g.noRef(RDX)
		a.StoreSDIdx(R12, RDX, 1, 0, x)
		g.next()
	}
	intOp := func(op ALU, imm bool) {
		ldI(R9, 'B')
		if imm {
			g.immSC(RCX)
		} else {
			ldI(RCX, 'C')
		}
		a.Op(op, R9, RCX, false)
		stI(R9)
	}
	shiftOp := func(op Shift, imm bool) {
		ldI(R9, 'B')
		if imm {
			g.immSC(RCX)
		} else {
			ldI(RCX, 'C')
		}
		a.ShiftCL(op, R9, false)
		stI(R9)
	}
	setOp := func(c Cond) {
		ldI(R9, 'B')
		ldI(RCX, 'C')
		a.Op(Cmp, R9, RCX, false)
		a.Setcc(c, R9)
		a.MovZX8(R9, R9)
		stI(R9)
	}
	// jump: the branch taken where the flags say c.
	jump := func(c Cond) {
		tk := a.NewLabel()
		a.Jcc(c, tk)
		g.next()
		a.Bind(tk)
		g.immSC(RCX)
		g.taken(RCX)
	}
	jReg := func(c Cond) {
		ldI(R9, 'A')
		ldI(RCX, 'B')
		a.Op(Cmp, R9, RCX, false)
		jump(c)
	}
	jImm := func(c Cond) {
		ldI(R9, 'A')
		g.immSB(RCX)
		a.Op(Cmp, R9, RCX, false)
		jump(c)
	}
	// fcmp compares F registers: y with x where swap, x with y else.
	fcmp := func(x, y byte) {
		ldF(0, x)
		ldF(1, y)
		a.SSEOp(UcomiSD, 0, 1)
	}
	fOp := func(op SSE) {
		ldF(0, 'B')
		ldF(1, 'C')
		a.SSEOp(op, 0, 1)
		stF(0)
	}
	// addr puts in R9 the heap access's byte address, int32(R[B] + SC)
	// with its low bits of the element size cleared, sign-extended: a
	// negative one past heapLen as unsigned. Out of the heap it jumps to
	// out; else RSI is the element's address.
	addr := func(shift uint8, out Label) {
		ldI(R9, 'B')
		g.immSC(RCX)
		a.Op(Add, R9, RCX, false)
		a.Movsxd(R9, R9)
		if shift > 0 {
			a.OpImm(And, R9, -(int32(1) << shift), true)
		}
		a.CmpMem(R9, RDI, ncHeapLen, true)
		a.Jcc(CondAE, out)
		a.Load(RSI, RDI, ncHeapBase)
		a.Op(Add, RSI, R9, true)
	}
	loadI := func(shift uint8, load func(dst, base Reg, disp int32)) {
		out := a.NewLabel()
		addr(shift, out)
		load(R8, RSI, 0)
		stI(R8)
		a.Bind(out)
		a.Op(Xor, R8, R8, false)
		stI(R8)
	}
	storeI := func(shift uint8, store func(base Reg, disp int32, src Reg)) {
		out := a.NewLabel()
		addr(shift, out)
		ldI(R8, 'A')
		store(RSI, 0, R8)
		a.Bind(out)
		g.next()
	}
	// cell puts in RSI the address of the Value of upvalue BC.
	cell := func() {
		a.MovRR(RCX, RAX)
		a.ShiftImm(Shr, RCX, 32, true)
		g.upval(RSI, RCX)
	}

	h(bytecode.TMov, func() {
		ldI(R9, 'B')
		stI(R9)
	})
	h(bytecode.TIConst, func() {
		a.MovRR(R9, RAX)
		a.ShiftImm(Shr, R9, 32, true)
		stI(R9)
	})
	h(bytecode.TFConst, func() {
		a.Load(R8, RDI, ncRC)
		a.Load(R8, R8, offRCFloats)
		a.MovRR(RCX, RAX)
		a.ShiftImm(Shr, RCX, 32, true)
		a.LoadIdx(R9, R8, RCX, 8, 0)
		stI(R9)
	})
	h(bytecode.TConvI, func() {
		ldI(R9, 'B')
		a.Op(Cmp, R9, R10, true)
		a.Jcc(CondAE, g.exit)
		a.MovQToX(0, R9)
		g.toInt32(R8, 0)
		stI(R8)
	})
	h(bytecode.TConvF, func() {
		ldI(R9, 'B')
		a.Op(Cmp, R9, R10, true)
		a.Jcc(CondAE, g.exit)
		stI(R9)
	})
	h(bytecode.TIBox, func() {
		ldI(R9, 'B')
		a.Cvtsi2sd(0, R9, false)
		box(0)
	})
	h(bytecode.TUBox, func() {
		ldI(R9, 'B')
		a.MovRR32(R9, R9)
		a.Cvtsi2sd(0, R9, true)
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
		ldI(R9, 'B')
		ldI(RCX, 'C')
		a.Imul32(R9, RCX)
		stI(R9)
	})
	div := func(signed, rem bool) {
		zero := a.NewLabel()
		ldI(R9, 'B')
		ldI(RCX, 'C')
		if signed {
			a.Movsxd(R9, R9)
			a.Movsxd(RCX, RCX)
		} else {
			a.MovRR32(R9, R9)
			a.MovRR32(RCX, RCX)
		}
		a.Op(Test, RCX, RCX, true)
		a.Jcc(CondE, zero)
		// RAX and RDX the dividend; 64 bits, so that MinInt32 / -1 does
		// not trap and is MinInt32's bits; RAX is read again at dispatch.
		a.MovRR(RAX, R9)
		if signed {
			a.Cqo()
			a.Idiv(RCX)
		} else {
			a.Op(Xor, RDX, RDX, false)
			a.Div(RCX)
		}
		res := RAX
		if rem {
			res = RDX
		}
		a.MovRR(R9, res)
		a.Load(RAX, RBX, 0)
		stI(R9)
		a.Bind(zero)
		a.Op(Xor, R9, R9, false)
		stI(R9)
	}
	h(bytecode.TDivS, func() { div(true, false) })
	h(bytecode.TDivU, func() { div(false, false) })
	h(bytecode.TRemS, func() { div(true, true) })
	h(bytecode.TRemU, func() { div(false, true) })
	h(bytecode.TAnd, func() { intOp(And, false) })
	h(bytecode.TOr, func() { intOp(Or, false) })
	h(bytecode.TXor, func() { intOp(Xor, false) })
	h(bytecode.TShl, func() { shiftOp(Shl, false) })
	h(bytecode.TShr, func() { shiftOp(Sar, false) })
	h(bytecode.TUShr, func() { shiftOp(Shr, false) })
	h(bytecode.TAddI, func() { intOp(Add, true) })
	h(bytecode.TAndI, func() { intOp(And, true) })
	h(bytecode.TOrI, func() { intOp(Or, true) })
	h(bytecode.TXorI, func() { intOp(Xor, true) })
	h(bytecode.TShlI, func() { shiftOp(Shl, true) })
	h(bytecode.TShrI, func() { shiftOp(Sar, true) })
	h(bytecode.TUShrI, func() { shiftOp(Shr, true) })
	h(bytecode.TNeg, func() {
		ldI(R9, 'B')
		a.Op(Xor, RCX, RCX, false)
		a.Op(Sub, RCX, R9, false)
		stI(RCX)
	})
	h(bytecode.TNot, func() {
		ldI(R9, 'B')
		a.Not32(R9)
		stI(R9)
	})
	h(bytecode.TLNot, func() {
		ldI(R9, 'B')
		a.Op(Test, R9, R9, false)
		a.Setcc(CondE, R9)
		a.MovZX8(R9, R9)
		stI(R9)
	})
	h(bytecode.TLtS, func() { setOp(CondL) })
	h(bytecode.TLeS, func() { setOp(CondLE) })
	h(bytecode.TLtU, func() { setOp(CondB) })
	h(bytecode.TLeU, func() { setOp(CondBE) })
	h(bytecode.TEq, func() { setOp(CondE) })
	h(bytecode.TNe, func() { setOp(CondNE) })
	h(bytecode.TJLtS, func() { jReg(CondL) })
	h(bytecode.TJLeS, func() { jReg(CondLE) })
	h(bytecode.TJLtU, func() { jReg(CondB) })
	h(bytecode.TJLeU, func() { jReg(CondBE) })
	h(bytecode.TJEq, func() { jReg(CondE) })
	h(bytecode.TJNe, func() { jReg(CondNE) })
	h(bytecode.TJLtSI, func() { jImm(CondL) })
	h(bytecode.TJLeSI, func() { jImm(CondLE) })
	h(bytecode.TJGtSI, func() { jImm(CondG) })
	h(bytecode.TJGeSI, func() { jImm(CondGE) })
	h(bytecode.TJEqI, func() { jImm(CondE) })
	h(bytecode.TJNeI, func() { jImm(CondNE) })
	h(bytecode.TJZ, func() {
		ldI(R9, 'A')
		a.Op(Test, R9, R9, false)
		jump(CondE)
	})
	h(bytecode.TJNZ, func() {
		ldI(R9, 'A')
		a.Op(Test, R9, R9, false)
		jump(CondNE)
	})
	h(bytecode.TFAdd, func() { fOp(AddSD) })
	h(bytecode.TFSub, func() { fOp(SubSD) })
	h(bytecode.TFMul, func() { fOp(MulSD) })
	h(bytecode.TFDiv, func() { fOp(DivSD) })
	h(bytecode.TFNeg, func() {
		ldI(R9, 'B')
		a.MovImm(RCX, 1<<63)
		a.Op(Xor, R9, RCX, true)
		stI(R9)
	})
	h(bytecode.TFAbs, func() {
		ldI(R9, 'B')
		a.MovImm(RCX, 1<<63-1)
		a.Op(And, R9, RCX, true)
		stI(R9)
	})
	h(bytecode.TFSqrt, func() {
		ldF(0, 'B')
		a.SSEOp(SqrtSD, 0, 0)
		stF(0)
	})
	h(bytecode.TFRound, func() {
		ldF(0, 'B')
		a.Cvtsd2ss(1, 0)
		a.Cvtss2sd(0, 1)
		stF(0)
	})
	// The float compares as values: x < y is y > x, false of a NaN as
	// CondA is; x == y is ZF without PF.
	fset := func(x, y byte, c Cond) {
		fcmp(x, y)
		a.Setcc(c, R9)
		a.MovZX8(R9, R9)
		stI(R9)
	}
	h(bytecode.TFLt, func() { fset('C', 'B', CondA) })
	h(bytecode.TFLe, func() { fset('C', 'B', CondAE) })
	h(bytecode.TFEq, func() {
		fcmp('B', 'C')
		a.Setcc(CondE, R9)
		a.Setcc(CondNP, RCX)
		a.Op(And, R9, RCX, false)
		a.MovZX8(R9, R9)
		stI(R9)
	})
	h(bytecode.TFNe, func() {
		fcmp('B', 'C')
		a.Setcc(CondNE, R9)
		a.Setcc(CondP, RCX)
		a.Op(Or, R9, RCX, false)
		a.MovZX8(R9, R9)
		stI(R9)
	})
	// The float branches, A with B: x < y as y > x, its negation taken
	// on a NaN too; x == y taken with ZF and no PF, x != y on either.
	h(bytecode.TJFLt, func() { fcmp('B', 'A'); jump(CondA) })
	h(bytecode.TJFNLt, func() { fcmp('B', 'A'); jump(CondBE) })
	h(bytecode.TJFLe, func() { fcmp('B', 'A'); jump(CondAE) })
	h(bytecode.TJFNLe, func() { fcmp('B', 'A'); jump(CondB) })
	h(bytecode.TJFEq, func() {
		fcmp('A', 'B')
		no := a.NewLabel()
		a.Jcc(CondP, no)
		tk := a.NewLabel()
		a.Jcc(CondE, tk)
		a.Bind(no)
		g.next()
		a.Bind(tk)
		g.immSC(RCX)
		g.taken(RCX)
	})
	h(bytecode.TJFNe, func() {
		fcmp('A', 'B')
		tk := a.NewLabel()
		a.Jcc(CondP, tk)
		a.Jcc(CondNE, tk)
		g.next()
		a.Bind(tk)
		g.immSC(RCX)
		g.taken(RCX)
	})
	h(bytecode.TI2FS, func() {
		ldI(R9, 'B')
		a.Cvtsi2sd(0, R9, false)
		stF(0)
	})
	h(bytecode.TI2FU, func() {
		ldI(R9, 'B')
		a.MovRR32(R9, R9)
		a.Cvtsi2sd(0, R9, true)
		stF(0)
	})
	h(bytecode.TF2I, func() {
		ldF(0, 'B')
		g.toInt32(R8, 0)
		stI(R8)
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
			a.LoadU32(R8, RSI, 0)
			a.MovQToX(1, R8)
			a.Cvtss2sd(0, 1)
		} else {
			a.LoadSD(0, RSI, 0)
		}
		stF(0)
		a.Bind(out)
		a.Load(R8, RDI, ncNaN)
		stI(R8)
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
		a.Cvtsd2ss(1, 0)
		a.MovQFromX(R8, 1)
		a.Store32(RSI, 0, R8)
		a.Bind(out)
		g.next()
	})
	h(bytecode.TStF64, func() { storeI(3, a.Store) })
	// The module's variables: a cell's number word, while it holds a
	// number (TGGet*) or no pointer (TGSet*).
	h(bytecode.TGGetI, func() {
		cell()
		a.Load(R9, RSI, 0)
		a.Op(Cmp, R9, R10, true)
		a.Jcc(CondAE, g.exit)
		a.MovQToX(0, R9)
		g.toInt32(R8, 0)
		stI(R8)
	})
	h(bytecode.TGGetF, func() {
		cell()
		a.Load(R9, RSI, 0)
		a.Op(Cmp, R9, R10, true)
		a.Jcc(CondAE, g.exit)
		stI(R9)
	})
	h(bytecode.TGSetI, func() {
		cell()
		a.CmpMemImm8(RSI, 8, 0)
		a.Jcc(CondNE, g.exit)
		ldI(R9, 'A')
		a.Cvtsi2sd(0, R9, false)
		a.StoreSD(RSI, 0, 0)
		g.next()
	})
	h(bytecode.TGSetF, func() {
		cell()
		a.CmpMemImm8(RSI, 8, 0)
		a.Jcc(CondNE, g.exit)
		ldF(0, 'A')
		g.canon(0)
		a.StoreSD(RSI, 0, 0)
		g.next()
	})

	for i := 0; i < len(g.late); i++ {
		g.late[i]()
	}
	for n, l := range g.exits {
		if n == 0 {
			continue
		}
		a.Bind(l)
		a.StoreImm(RDI, scr(15), int32(n))
		a.Jmp(g.exit)
	}
	b := &block{at: map[bytecode.ROp]int{}, order: order}
	if len(order) == 0 {
		return b, nil
	}
	a.Bind(g.exit)
	a.JmpMem(R13, 8*slotExit)
	a.Bind(g.exitRet)
	a.JmpMem(R13, 8*slotExitRet)
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
