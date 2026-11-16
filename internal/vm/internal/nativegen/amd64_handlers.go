package nativegen

import (
	"fmt"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
	. "github.com/go-quickjs/go-quickjs/internal/jit/asm/amd64"
)

// nativeEmit makes amd64's handlers' code for variant v:
// position-independent, every address it needs given by the context, so
// that it may be placed anywhere. plain's is the handlers' block
// (zzzyn_native_handlers_amd64.s): the entry, the exit stub and every
// handler. Another variant's is its own handlers alone, which reach
// plain's code through the variant's table, R13: the exits through its
// slots after the operations' (slotExit, slotExitRet), another handler
// through that operation's entry.
func nativeEmit(v *variant) (*block, error) {
	g := &nativeGen{a: &Asm{}, v: v}
	a := g.a
	g.exit, g.exitRet, g.exitAfter = a.NewLabel(), a.NewLabel(), a.NewLabel()
	g.genRet, g.genObj = g.exit, g.exit
	if v.has(bytecode.RYield) {
		g.genRet, g.genObj = a.NewLabel(), a.NewLabel()
	}
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
	// label is op's handler's label, for a jump to it: in a variant's
	// block, where op's is plain's, a jump through op's table entry.
	var far []bytecode.ROp
	farLabels := map[bytecode.ROp]Label{}
	var missing []bytecode.ROp
	label := func(op bytecode.ROp) Label {
		if l, ok := labels[op]; ok {
			return l
		}
		if v.has(op) {
			// Its handler not laid out yet: a jump to it would need a
			// label bound later, which no handler has needed.
			missing = append(missing, op)
			return a.NewLabel()
		}
		if l, ok := farLabels[op]; ok {
			return l
		}
		farLabels[op] = a.NewLabel()
		far = append(far, op)
		return farLabels[op]
	}

	if v == plain {
		// The entry, at the code's start: the pinned registers from the
		// context, and the first dispatch.
		a.Load(RBX, RDI, ncIP)
		a.Load(R12, RDI, ncRegs)
		a.Load(R13, RDI, ncTable)
		a.Load(R15, RDI, ncIC)
		a.Load(R11, RDI, ncBudget)
		a.Load(R10, RDI, ncTagBase)
		g.dispatch()
		g.exitStub()
	}

	extra := func() {
		h(bytecode.RBitOr, func() { g.bitReg(func() { a.Op(Or, R9, RCX, false) }) })
		h(bytecode.RBitXor, func() { g.bitReg(func() { a.Op(Xor, R9, RCX, false) }) })
		h(bytecode.RShl, func() { g.bitReg(func() { a.ShiftCL(Shl, R9, false) }) })
		h(bytecode.RShr, func() { g.bitReg(func() { a.ShiftCL(Sar, R9, false) }) })
		h(bytecode.RUShr, func() {
			g.bitReg(func() { a.ShiftCL(Shr, R9, false) }, true)
		})
		h(bytecode.RXorI, func() { g.bitImm(func() { a.Op(Xor, R9, RCX, false) }) })
		h(bytecode.RUShrI, func() { g.bitImm(func() { a.ShiftCL(Shr, R9, false) }, true) })
		h(bytecode.RJLe, func() { g.cmp(false, true, CondAE, false) })
		h(bytecode.RJNLe, func() { g.cmp(false, true, CondB, false) })
		h(bytecode.RJGt, func() { g.cmp(false, false, CondA, false) })
	}

	h(bytecode.RNop, func() { g.next() })
	h(bytecode.RMove, func() {
		// Any value: its pointer word by storeValue.
		g.decB(RSI)
		a.LeaIdx(RCX, R12, RSI, 1, 0)
		g.loadValue(RCX)
		g.decA(RDX)
		a.LeaIdx(RAX, R12, RDX, 1, 0)
		g.storeValue()
	})
	h(bytecode.RLoadConst, func() {
		a.MovRR(RCX, RAX)
		a.ShiftImm(Shr, RCX, 32, true)
		a.ShiftImm(Shl, RCX, 4, true)
		a.Load(R8, RDI, ncCC)
		a.AddMem(RCX, R8, offCCConsts)
		g.loadValue(RCX)
		g.decA(RDX)
		a.LeaIdx(RAX, R12, RDX, 1, 0)
		g.storeValue()
	})
	h(bytecode.RGetGlobal, func() {
		// The slot the site remembers while the global epoch is its.
		a.MovRR(RCX, RAX)
		a.ShiftImm(Shr, RCX, 48, true)
		a.Load(R8, RDI, ncCC)
		a.CmpMem(RCX, R8, offCCRegEpochs+8, true)
		a.Jcc(CondAE, g.exit)
		a.ShiftImm(Shl, RCX, 3, true)
		a.AddMem(RCX, R8, offCCRegEpochs)
		a.Load(RCX, RCX, 0)
		a.Load(RSI, RDI, ncEpoch)
		a.Load(RSI, RSI, 0)
		a.OpImm(Add, RSI, 1, true)
		a.Op(Cmp, RCX, RSI, true)
		a.Jcc(CondNE, g.exit)
		g.site(RCX)
		a.Load(RSI, RCX, off("propCache.p1"))
		a.LoadS32(R9, RCX, off("propCache.idx"))
		a.LoadU32(R8, RSI, off("Object.valn"))
		a.Op(Cmp, R9, R8, true)
		a.Jcc(CondAE, g.exit)
		a.ShiftImm(Shl, R9, 4, true)
		a.AddMem(R9, RSI, off("Object.valp"))
		a.MovRR(RCX, R9)
		g.loadValue(RCX)
		g.decA(RDX)
		a.LeaIdx(RAX, R12, RDX, 1, 0)
		g.storeValue()
	})
	h(bytecode.RLoadInt, func() {
		a.MovRR(RCX, RAX)
		a.ShiftImm(Sar, RCX, 32, true)
		a.Cvtsi2sd(0, RCX, true)
		g.storeA(0)
	})
	h(bytecode.RLoadUndef, func() { a.LoadSD(0, RDI, ncUndef); g.storeA(0) })
	h(bytecode.RLoadNull, func() { a.LoadSD(0, RDI, ncNull); g.storeA(0) })
	h(bytecode.RLoadTrue, func() { a.LoadSD(0, RDI, ncTrue); g.storeA(0) })
	h(bytecode.RLoadFalse, func() { a.LoadSD(0, RDI, ncFalse); g.storeA(0) })
	h(bytecode.RLoadUninit, func() { a.LoadSD(0, RDI, ncUninit); g.storeA(0) })
	h(bytecode.RLoadEmpty, func() {
		a.Load(R9, RDI, ncStrBits)
		a.Load(R8, RDI, ncEmptyStr)
		g.decA(RDX)
		a.LeaIdx(RAX, R12, RDX, 1, 0)
		g.storeValue()
	})
	h(bytecode.RGetUpval, func() {
		a.MovRR(RCX, RAX)
		a.ShiftImm(Shr, RCX, 32, true)
		g.upval(RSI, RCX)
		g.loadValue(RSI)
		g.decA(RDX)
		a.LeaIdx(RAX, R12, RDX, 1, 0)
		g.storeValue()
	})
	h(bytecode.RSetUpval, func() {
		g.decA(RDX)
		a.LoadIdx(R9, R12, RDX, 1, 0)
		a.LoadIdx(R8, R12, RDX, 1, 8)
		a.MovRR(RCX, RAX)
		a.ShiftImm(Shr, RCX, 32, true)
		g.upval(RAX, RCX)
		g.storeValue()
	})
	h(bytecode.RJLtU, func() { g.cmpU(CondA) })
	h(bytecode.RSetGlobal, func() {
		// The slot the site remembers while the global epoch is its, as
		// get_global reads it, written.
		a.MovRR(RCX, RAX)
		a.ShiftImm(Shr, RCX, 48, true)
		a.Load(R8, RDI, ncCC)
		a.CmpMem(RCX, R8, offCCRegEpochs+8, true)
		a.Jcc(CondAE, g.exit)
		a.ShiftImm(Shl, RCX, 3, true)
		a.AddMem(RCX, R8, offCCRegEpochs)
		a.Load(RCX, RCX, 0)
		a.Load(RSI, RDI, ncEpoch)
		a.Load(RSI, RSI, 0)
		a.OpImm(Add, RSI, 1, true)
		a.Op(Cmp, RCX, RSI, true)
		a.Jcc(CondNE, g.exit)
		g.site(RCX)
		a.Load(RSI, RCX, off("propCache.p1"))
		a.LoadS32(RCX, RCX, off("propCache.idx"))
		a.LoadU32(R8, RSI, off("Object.valn"))
		a.Op(Cmp, RCX, R8, true)
		a.Jcc(CondAE, g.exit)
		a.ShiftImm(Shl, RCX, 4, true)
		a.AddMem(RCX, RSI, off("Object.valp"))
		g.decA(RDX)
		a.LoadIdx(R9, R12, RDX, 1, 0)
		a.LoadIdx(R8, R12, RDX, 1, 8)
		a.MovRR(RAX, RCX)
		g.storeValue()
	})
	// o.n += R[B] and o.n += SB of an own number the site remembers:
	// its word alone written.
	for _, imm := range []bool{false, true} {
		op := bytecode.RAddProp
		if imm {
			op = bytecode.RAddPropI
		}
		h(op, func() {
			g.decA(RDX)
			g.object(RSI, RDX)
			if imm {
				g.immSB(R9)
				a.Cvtsi2sd(1, R9, true)
			} else {
				g.decB(RDX)
				g.num(1, RDX)
			}
			g.site(RCX)
			g.ownSlot(RDX, RSI)
			a.Load(R9, RDX, 0)
			a.Op(Cmp, R9, R10, true)
			a.Jcc(CondAE, g.exit)
			a.MovQToX(0, R9)
			a.SSEOp(AddSD, 0, 1)
			g.canon(0)
			a.StoreSD(RDX, 0, 0)
			g.next()
		})
	}
	h(bytecode.RJNLtU, func() { g.cmpU(CondBE) })
	h(bytecode.RJLeU, func() { g.cmpU(CondAE) })
	h(bytecode.RJNLeU, func() { g.cmpU(CondB) })
	h(bytecode.RJEq, func() { g.looseEq(true) })
	h(bytecode.RJNe, func() { g.looseEq(false) })
	h(bytecode.RKeyOfBase, func() {
		// A string, a number or a symbol is its own key.
		key := a.NewLabel()
		g.decC(RSI)
		a.LeaIdx(RSI, R12, RSI, 1, 0)
		a.Load(R9, RSI, 0)
		a.Op(Cmp, R9, R10, true)
		a.Jcc(CondB, key)
		a.MovRR(RCX, R9)
		a.ShiftImm(Shr, RCX, 16, true)
		a.ShiftImm(Shl, RCX, 16, true)
		a.Op(Cmp, RCX, R10, true)
		a.Jcc(CondNE, g.exit)
		a.MovZX8(RCX, R9)
		a.OpImm(Cmp, RCX, int32(KindString), false)
		a.Jcc(CondE, key)
		a.OpImm(Cmp, RCX, int32(KindSymbol), false)
		a.Jcc(CondNE, g.exit)
		a.Bind(key)
		a.Load(R8, RSI, 8)
		g.decA(RDX)
		a.LeaIdx(RAX, R12, RDX, 1, 0)
		g.storeValue()
	})
	h(bytecode.RAdd, func() { g.arith(AddSD, 0) })
	h(bytecode.RSub, func() { g.arith(SubSD, 0) })
	h(bytecode.RMul, func() { g.arith(MulSD, 0) })
	h(bytecode.RAddI, func() { g.arith(AddSD, 1) })
	h(bytecode.RSubI, func() { g.arith(SubSD, 1) })
	h(bytecode.RInc, func() { g.arith(AddSD, 2) })
	h(bytecode.RDec, func() { g.arith(SubSD, 2) })
	h(bytecode.RPostInc, func() { g.postStep(AddSD) })
	h(bytecode.RAndI, func() { g.bitImm(func() { a.Op(And, R9, RCX, false) }) })
	h(bytecode.ROrI, func() { g.bitImm(func() { a.Op(Or, R9, RCX, false) }) })
	h(bytecode.RShlI, func() { g.bitImm(func() { a.ShiftCL(Shl, R9, false) }) })
	h(bytecode.RShrI, func() { g.bitImm(func() { a.ShiftCL(Sar, R9, false) }) })
	h(bytecode.RBitAnd, func() { g.bitReg(func() { a.Op(And, R9, RCX, false) }) })
	h(bytecode.RJmp, func() {
		a.MovRR(RCX, RAX)
		a.ShiftImm(Sar, RCX, 32, true)
		g.taken(RCX)
	})
	// Compares: A op B is ucomisd A, B (swap false) or B, A (swap true).
	h(bytecode.RJLt, func() { g.cmp(false, true, CondA, false) })
	h(bytecode.RJNLt, func() { g.cmp(false, true, CondBE, false) })
	h(bytecode.RJNGt, func() { g.cmp(false, false, CondBE, false) })
	h(bytecode.RJGe, func() { g.cmp(false, false, CondAE, false) })
	h(bytecode.RJNGe, func() { g.cmp(false, false, CondB, false) })
	h(bytecode.RJLtI, func() { g.cmp(true, true, CondA, false) })
	h(bytecode.RJNLtI, func() { g.cmp(true, true, CondBE, false) })
	h(bytecode.RJLeI, func() { g.cmp(true, true, CondAE, false) })
	h(bytecode.RJNLeI, func() { g.cmp(true, true, CondB, false) })
	h(bytecode.RJGtI, func() { g.cmp(true, false, CondA, false) })
	h(bytecode.RJNGtI, func() { g.cmp(true, false, CondBE, false) })
	h(bytecode.RJGeI, func() { g.cmp(true, false, CondAE, false) })
	h(bytecode.RJNGeI, func() { g.cmp(true, false, CondB, false) })
	h(bytecode.RJNeI, func() { g.cmp(true, false, CondNE, true) })
	for _, op := range []bytecode.ROp{bytecode.RJmpF, bytecode.RJmpT} {
		jumpIf := op == bytecode.RJmpT
		h(op, func() {
			// ToBoolean of a boolean, an object or a number, else Go's.
			isTrue, isFalse, notBool, notObj := a.NewLabel(), a.NewLabel(), a.NewLabel(), a.NewLabel()
			g.decA(RDX)
			a.LoadIdx(R9, R12, RDX, 1, 0)
			a.CmpMem(R9, RDI, ncTrue, true)
			a.Jcc(CondNE, notBool)
			a.Jmp(isTrue)
			a.Bind(notBool)
			a.CmpMem(R9, RDI, ncFalse, true)
			a.Jcc(CondE, isFalse)
			a.CmpMem(R9, RDI, ncObjBits, true)
			a.Jcc(CondNE, notObj)
			a.LoadIdx(R9, R12, RDX, 1, 8)
			a.LoadU16(RCX, R9, off("Object.flags"))
			a.OpImm(And, RCX, int32(objHTMLDDA), false)
			a.Jcc(CondNE, isFalse)
			a.Jmp(isTrue)
			a.Bind(notObj)
			tagged := a.NewLabel()
			a.Op(Cmp, R9, R10, true)
			a.Jcc(CondAE, tagged)
			a.MovQToX(0, R9)
			a.SSEOp(XorPD, 1, 1)
			a.SSEOp(UcomiSD, 0, 1)
			a.Jcc(CondP, isFalse)
			a.Jcc(CondE, isFalse)
			a.Bind(isTrue)
			if jumpIf {
				a.MovRR(RCX, RAX)
				a.ShiftImm(Sar, RCX, 32, true)
				g.taken(RCX)
				a.Bind(isFalse)
				g.next()
			} else {
				g.next()
				a.Bind(isFalse)
				a.MovRR(RCX, RAX)
				a.ShiftImm(Sar, RCX, 32, true)
				g.taken(RCX)
			}
			// undefined and null are false, a string true unless empty;
			// anything else is Go's.
			a.Bind(tagged)
			g.nullish(R9, isFalse)
			a.MovRR(RCX, R9)
			a.ShiftImm(Shr, RCX, 16, true)
			a.ShiftImm(Shl, RCX, 16, true)
			a.Op(Cmp, RCX, R10, true)
			a.Jcc(CondNE, g.exit)
			a.MovZX8(RCX, R9)
			a.OpImm(Cmp, RCX, int32(KindString), false)
			a.Jcc(CondNE, g.exit)
			a.LoadIdx(R9, R12, RDX, 1, 8)
			a.CmpMemImm8(R9, offStrLen, 0)
			a.Jcc(CondE, isFalse)
			a.Jmp(isTrue)
		})
	}
	// == of the immediate SB: a number's, else Go's.
	h(bytecode.RJEqI, func() {
		ne := a.NewLabel()
		g.decA(RDX)
		g.num(0, RDX)
		g.immSB(RCX)
		a.Cvtsi2sd(1, RCX, true)
		a.SSEOp(UcomiSD, 0, 1)
		a.Jcc(CondP, ne)
		a.Jcc(CondNE, ne)
		g.immSC(RCX)
		g.taken(RCX)
		a.Bind(ne)
		g.next()
	})
	// -x, +x, ~x and ToNumeric of a number; anything else, and !x, Go's.
	h(bytecode.RUnary, func() {
		pos, not := a.NewLabel(), a.NewLabel()
		g.decB(RSI)
		g.num(0, RSI)
		a.MovRR(R8, RAX)
		a.ShiftImm(Shr, R8, 8, true)
		a.MovZX8(R8, R8)
		a.OpImm(Cmp, R8, int32(bytecode.OpPos), false)
		a.Jcc(CondE, pos)
		a.OpImm(Cmp, R8, int32(bytecode.OpToNumeric), false)
		a.Jcc(CondE, pos)
		a.OpImm(Cmp, R8, int32(bytecode.OpBitNot), false)
		a.Jcc(CondE, not)
		a.OpImm(Cmp, R8, int32(bytecode.OpNeg), false)
		a.Jcc(CondNE, g.exit)
		a.MovImm(RCX, 1<<63)
		a.Op(Xor, R9, RCX, true)
		a.MovQToX(0, R9)
		a.Bind(pos)
		g.canon(0)
		g.storeA(0)
		a.Bind(not)
		g.toInt32(R9, 0)
		a.OpImm(Xor, R9, -1, false)
		a.Cvtsi2sd(0, R9, false)
		g.storeA(0)
	})
	for _, op := range []bytecode.ROp{bytecode.RJNull, bytecode.RJNotNull} {
		jumpIfNull := op == bytecode.RJNull
		h(op, func() {
			isNull, notNull, notObj := a.NewLabel(), a.NewLabel(), a.NewLabel()
			g.decA(RDX)
			a.LoadIdx(R9, R12, RDX, 1, 0)
			a.CmpMem(R9, RDI, ncObjBits, true)
			a.Jcc(CondNE, notObj)
			a.LoadIdx(R9, R12, RDX, 1, 8)
			a.LoadU16(RCX, R9, off("Object.flags"))
			a.OpImm(And, RCX, int32(objHTMLDDA), false)
			a.Jcc(CondNE, g.exit)
			a.Jmp(notNull)
			a.Bind(notObj)
			a.MovRR(RCX, R9)
			a.ShiftImm(Shr, RCX, 16, true)
			a.ShiftImm(Shl, RCX, 16, true)
			a.Op(Cmp, RCX, R10, true)
			a.Jcc(CondNE, notNull)
			a.MovZX8(RCX, R9)
			a.OpImm(Cmp, RCX, int32(KindNull), false)
			a.Jcc(CondE, isNull)
			a.OpImm(Cmp, RCX, int32(KindUndefined), false)
			a.Jcc(CondE, isNull)
			a.Jmp(notNull)
			if jumpIfNull {
				a.Bind(notNull)
				g.next()
				a.Bind(isNull)
			} else {
				a.Bind(isNull)
				g.next()
				a.Bind(notNull)
			}
			g.immSC(RCX)
			g.taken(RCX)
		})
	}
	getProp := func() {
		g.decB(RSI)
		g.object(RSI, RSI)
		g.site(RCX)
		g.readSlot()
		g.loadValue(RCX)
		g.decA(RDX)
		a.LeaIdx(RAX, R12, RDX, 1, 0)
		g.storeValue()
	}
	h(bytecode.RGetMethodW, func() {
		// R[A] = the method, R[A+1] = the receiver: two stores of
		// pointers, the receiver kept across the first. A string's or a
		// number's method is primMethod's.
		prim := a.NewLabel()
		g.decB(RSI)
		a.LoadIdx(R9, R12, RSI, 1, 0)
		a.CmpMem(R9, RDI, ncObjBits, true)
		a.Jcc(CondNE, prim)
		a.LoadIdx(RSI, R12, RSI, 1, 8)
		a.Store(RDI, scr(sV0), RSI)
		g.site(RCX)
		g.readSlot()
		g.loadValue(RCX)
		g.decA(RDX)
		a.LeaIdx(RAX, R12, RDX, 1, 0)
		a.Store(RAX, 0, R9)
		a.Lea(RAX, RAX, 8)
		a.MovRR(RCX, R8)
		g.ptrStore()
		a.Load(RAX, RBX, 0)
		g.decA(RDX)
		a.LeaIdx(RAX, R12, RDX, 1, 16)
		a.Load(R9, RDI, ncObjBits)
		a.Store(RAX, 0, R9)
		a.Lea(RAX, RAX, 8)
		a.Load(RCX, RDI, scr(sV0))
		g.ptrStore()
		g.next()
		a.Bind(prim)
		g.primMethod()
	})
	// === against an immediate: only a number is equal to one.
	h(bytecode.RJSEqI, func() {
		notNum := a.NewLabel()
		g.decA(RDX)
		a.LoadIdx(R9, R12, RDX, 1, 0)
		a.Op(Cmp, R9, R10, true)
		a.Jcc(CondAE, notNum)
		a.MovQToX(0, R9)
		g.immSB(RCX)
		a.Cvtsi2sd(1, RCX, true)
		a.SSEOp(UcomiSD, 0, 1)
		a.Jcc(CondP, notNum)
		a.Jcc(CondNE, notNum)
		g.immSC(RCX)
		g.taken(RCX)
		a.Bind(notNum)
		g.next()
	})
	// === and !== of two numbers, or of two values one of which is
	// undefined, null, a boolean, a symbol or an object, equal only if
	// their words are (runReg's case); two strings or BigInts are Go's.
	for _, op := range []bytecode.ROp{bytecode.RJSEq, bytecode.RJSNe} {
		jumpIfEq := op == bytecode.RJSEq
		h(op, func() {
			eq, ne, words, same := a.NewLabel(), a.NewLabel(), a.NewLabel(), a.NewLabel()
			g.decA(RDX)
			g.decB(RSI)
			a.LoadIdx(R9, R12, RDX, 1, 0)
			a.LoadIdx(R8, R12, RSI, 1, 0)
			a.Op(Cmp, R9, R10, true)
			a.Jcc(CondAE, words)
			a.Op(Cmp, R8, R10, true)
			a.Jcc(CondAE, words)
			a.MovQToX(0, R9)
			a.MovQToX(1, R8)
			a.SSEOp(UcomiSD, 0, 1)
			a.Jcc(CondP, ne)
			a.Jcc(CondNE, ne)
			a.Jmp(eq)
			a.Bind(words)
			// A word kind: tagged, below the BigInts, of a kind in the
			// mask. RAX is scratch, the instruction read again after.
			for _, x := range []Reg{R9, R8} {
				notWord := a.NewLabel()
				a.MovRR(RCX, x)
				a.Op(Sub, RCX, R10, true)
				a.MovImm(RAX, heapBigBits-tagBase)
				a.Op(Cmp, RCX, RAX, true)
				a.Jcc(CondAE, notWord)
				a.MovZX8(RCX, x)
				a.MovImm(RAX, 1<<KindUndefined|1<<KindNull|1<<KindBool|1<<KindSymbol|1<<KindObject)
				a.Bt(RAX, RCX)
				a.Jcc(CondB, same)
				a.Bind(notWord)
			}
			// Two strings: the same one, of different lengths, or two
			// flat ones compared by their text (flatEq); any other pair
			// Go's.
			for _, x := range []Reg{R9, R8} {
				a.MovRR(RCX, x)
				a.ShiftImm(Shr, RCX, 16, true)
				a.ShiftImm(Shl, RCX, 16, true)
				a.Op(Cmp, RCX, R10, true)
				a.Jcc(CondNE, g.exit)
				a.MovZX8(RCX, x)
				a.OpImm(Cmp, RCX, int32(KindString), false)
				a.Jcc(CondNE, g.exit)
			}
			a.Load(RAX, RBX, 0)
			a.LoadIdx(R9, R12, RDX, 1, 8)
			a.LoadIdx(R8, R12, RSI, 1, 8)
			a.Op(Cmp, R9, R8, true)
			a.Jcc(CondE, eq)
			a.Load(RCX, R9, offStrLen)
			a.CmpMem(RCX, R8, offStrLen, true)
			a.Jcc(CondNE, ne)
			g.flatEq(eq, ne)
			a.Bind(same)
			a.Load(RAX, RBX, 0)
			a.Op(Cmp, R9, R8, true)
			a.Jcc(CondNE, ne)
			a.LoadIdx(R9, R12, RDX, 1, 8)
			a.LoadIdx(R8, R12, RSI, 1, 8)
			a.Op(Cmp, R9, R8, true)
			a.Jcc(CondNE, ne)
			a.Jmp(eq)
			if jumpIfEq {
				a.Bind(eq)
				g.immSC(RCX)
				g.taken(RCX)
				a.Bind(ne)
				g.next()
			} else {
				a.Bind(ne)
				g.immSC(RCX)
				g.taken(RCX)
				a.Bind(eq)
				g.next()
			}
		})
	}
	h(bytecode.RJSNeI, func() {
		tk := a.NewLabel()
		g.decA(RDX)
		a.LoadIdx(R9, R12, RDX, 1, 0)
		a.Op(Cmp, R9, R10, true)
		a.Jcc(CondAE, tk)
		a.MovQToX(0, R9)
		g.immSB(RCX)
		a.Cvtsi2sd(1, RCX, true)
		a.SSEOp(UcomiSD, 0, 1)
		a.Jcc(CondP, tk)
		a.Jcc(CondNE, tk)
		g.next()
		a.Bind(tk)
		g.immSC(RCX)
		g.taken(RCX)
	})
	h(bytecode.RGetProp, getProp)
	h(bytecode.RGetMethod, getProp)
	h(bytecode.RSetProp, func() {
		add := a.NewLabel()
		g.decA(RDX)
		g.object(RSI, RDX)
		g.decB(RDX)
		a.LeaIdx(RDX, R12, RDX, 1, 0)
		g.site(RCX)
		// The own slot's address in RAX, the instruction's last use past.
		a.Load(R8, RCX, off("propCache.ownShape"))
		a.CmpMem(R8, RSI, off("Object.shape"), true)
		a.Jcc(CondNE, add)
		a.LoadS32(RAX, RCX, off("propCache.idx"))
		a.ShiftImm(Shl, RAX, 4, true)
		a.AddMem(RAX, RSI, off("Object.valp"))
		g.loadValue(RDX)
		g.storeValue()
		a.Bind(add)
		g.addProp()
	})
	// An element read: a number of a dense array's where the register
	// holds none, any value of one by storeValue, a typed array's.
	getElem := func(miss Label) {
		ref := a.NewLabel()
		a.Load(R9, RCX, 0)
		a.CmpMemImm8(RCX, 8, 0)
		a.Jcc(CondNE, ref)
		g.decA(RDX)
		a.CmpIdxImm8(R12, RDX, 1, 8, 0)
		a.Jcc(CondNE, ref)
		a.StoreIdx(R12, RDX, 1, 0, R9)
		g.next()
		a.Bind(ref)
		a.Load(R8, RCX, 8)
		g.decA(RDX)
		a.LeaIdx(RAX, R12, RDX, 1, 0)
		g.storeValue()
		a.Bind(miss)
		big := a.NewLabel()
		g.typedRead(RSI, RCX, &big)
		g.storeA(0)
		a.Bind(big)
		g.typedBigRead()
	}
	h(bytecode.RGetIndex, func() {
		// A key string a for-in handed out names its index (keyIndex).
		miss, str, have := a.NewLabel(), a.NewLabel(), a.NewLabel()
		g.decB(RSI)
		g.object(RSI, RSI)
		g.decC(R8)
		g.numOr(0, R8, str)
		g.element(RCX, RSI, 0, objMappedArguments, miss)
		a.Bind(have)
		getElem(miss)
		g.late = append(g.late, func() {
			a.Bind(str)
			g.keyIndex(R8)
			g.dense(RCX, RSI, objMappedArguments, g.exit)
			a.Jmp(have)
		})
	})
	h(bytecode.RGetIndexI, func() {
		miss := a.NewLabel()
		g.decB(RSI)
		g.object(RSI, RSI)
		a.MovRR(RCX, RAX)
		a.ShiftImm(Shr, RCX, 48, true)
		g.dense(RCX, RSI, objMappedArguments, miss)
		getElem(miss)
	})
	h(bytecode.RGetIndexPre, func() {
		// The key stepped by X, a signed byte, then the read at it.
		g.decB(RSI)
		g.object(RSI, RSI)
		g.decC(R8)
		g.num(0, R8)
		a.MovRR(RCX, RAX)
		a.ShiftImm(Shl, RCX, 48, true)
		a.ShiftImm(Sar, RCX, 56, true)
		a.Cvtsi2sd(1, RCX, true)
		a.SSEOp(AddSD, 1, 0)
		g.element(RCX, RSI, 1, objMappedArguments, g.exit)
		a.CmpMemImm8(RCX, 8, 0)
		a.Jcc(CondNE, g.exit)
		g.decA(RDX)
		g.noRef(RDX)
		g.decC(R8)
		a.StoreSDIdx(R12, R8, 1, 0, 1)
		a.Load(R9, RCX, 0)
		a.StoreIdx(R12, RDX, 1, 0, R9)
		g.next()
	})
	h(bytecode.RGetIndexPost, func() {
		g.decB(RSI)
		g.object(RSI, RSI)
		g.decC(R8)
		g.num(0, R8)
		g.element(RCX, RSI, 0, objMappedArguments, g.exit)
		a.CmpMemImm8(RCX, 8, 0)
		a.Jcc(CondNE, g.exit)
		g.decA(RDX)
		g.noRef(RDX)
		// The key stepped by X, a signed byte.
		a.MovRR(RSI, RAX)
		a.ShiftImm(Shl, RSI, 48, true)
		a.ShiftImm(Sar, RSI, 56, true)
		a.Cvtsi2sd(1, RSI, true)
		a.SSEOp(AddSD, 1, 0)
		a.StoreSDIdx(R12, R8, 1, 0, 1)
		a.Load(R9, RCX, 0)
		a.StoreIdx(R12, RDX, 1, 0, R9)
		g.next()
	})
	// An element write: any value over a dense array's element, a number
	// to a typed array.
	setElem := func(miss Label) {
		// Any value over a dense array's element, its pointer word by
		// storeValue.
		g.decC(R8)
		a.LoadIdx(R9, R12, R8, 1, 0)
		a.LoadIdx(R8, R12, R8, 1, 8)
		a.MovRR(RAX, RCX)
		g.storeValue()
		a.Bind(miss)
		bigW, typed := a.NewLabel(), a.NewLabel()
		g.arrayAppend(RSI, RCX, typed)
		a.Bind(typed)
		g.decC(R8)
		g.numOr(1, R8, bigW)
		g.typedWrite(RSI, RCX)
		g.next()
		a.Bind(bigW)
		g.typedBigWrite(RSI, RCX)
		g.next()
	}
	h(bytecode.RSetIndex, func() {
		miss := a.NewLabel()
		g.decA(RDX)
		g.object(RSI, RDX)
		g.decB(R8)
		g.num(0, R8)
		g.element(RCX, RSI, 0, objMappedArguments|objSharedElems, miss)
		setElem(miss)
	})
	h(bytecode.RSetIndexI, func() {
		miss := a.NewLabel()
		g.decA(RDX)
		g.object(RSI, RDX)
		a.MovRR(RCX, RAX)
		a.ShiftImm(Shr, RCX, 32, true)
		a.MovRR32(RCX, RCX)
		a.OpImm(And, RCX, 0xFFFF, false)
		g.dense(RCX, RSI, objMappedArguments|objSharedElems, miss)
		setElem(miss)
	})
	h(bytecode.RTypeof, func() {
		// typeof's string, the runtime's (typeofStrs) where it has made
		// it: of a number, a primitive, an object but a proxy and
		// document.all's kind.
		g.decB(RSI)
		g.typeIndex(RSI)
		a.Load(R8, RDI, ncTypeofs)
		a.LoadIdx(R8, R8, RCX, 8, 0)
		a.Op(Test, R8, R8, true)
		a.Jcc(CondE, g.exit)
		a.Load(R9, RDI, ncStrBits)
		g.decA(RDX)
		a.LeaIdx(RAX, R12, RDX, 1, 0)
		g.storeValue()
	})
	h(bytecode.RIn, func() {
		// R[A] = R[B] in R[C]: a string key's answer the realm's in
		// cache remembers of an object's layout and chain (hasCached);
		// anything else Go's, which fills the cache.
		found, index := a.NewLabel(), a.NewLabel()
		g.decC(RSI)
		g.object(RSI, RSI)
		g.decB(RDX)
		a.LoadIdx(R9, R12, RDX, 1, 0)
		a.Op(Cmp, R9, R10, true)
		a.Jcc(CondB, index)
		a.MovRR(RCX, R9)
		a.Op(Xor, RCX, R10, true)
		a.ShiftImm(Shr, RCX, 16, true)
		a.Jcc(CondNE, g.exit)
		a.MovZX8(RCX, R9)
		a.OpImm(Cmp, RCX, int32(KindString), false)
		a.Jcc(CondNE, g.exit)
		a.LoadIdx(R8, R12, RDX, 1, 8)
		a.LoadU16(RCX, RSI, offObjFlags)
		a.OpImm(And, RCX, int32(objPolyProto), false)
		a.Jcc(CondNE, g.exit)
		a.Load(R9, RSI, offObjShape)
		a.Op(Test, R9, R9, true)
		a.Jcc(CondE, g.exit)
		// The cache of the realm running now, the Runtime's first word,
		// at the offset the context gives (ncHas): a context lives on
		// from realm to realm, and the one it was made in may be gone.
		a.Load(RCX, RDI, ncRT)
		a.Load(RCX, RCX, rtRel(0))
		a.AddMem(RCX, RDI, ncHas)
		a.Load(RCX, RCX, 0)
		a.Op(Test, RCX, RCX, true)
		a.Jcc(CondE, g.exit)
		// The set: ((key>>4) ^ (layout>>4)) * hasCacheMul, its top bits;
		// RAX, the instruction, read again after.
		a.MovRR(RDX, R8)
		a.ShiftImm(Shr, RDX, 4, true)
		a.MovRR(RAX, R9)
		a.ShiftImm(Shr, RAX, 4, true)
		a.Op(Xor, RDX, RAX, true)
		a.MovImm(RAX, hasCacheMul)
		a.Imul(RDX, RAX)
		a.ShiftImm(Shr, RDX, 64-hasCacheBits+1, true)
		a.MovImm(RAX, uint64(2*sizeH))
		a.Imul(RDX, RAX)
		a.Op(Add, RCX, RDX, true)
		for i := int32(0); i < 2; i++ {
			next := a.NewLabel()
			e := i * sizeH
			a.CmpMem(R8, RCX, e+offHKey, true)
			a.Jcc(CondNE, next)
			a.CmpMem(R9, RCX, e+offHShape, true)
			a.Jcc(CondNE, next)
			a.Load(RAX, RCX, e+offHCell)
			a.LoadU8(RAX, RAX, offCellValid)
			a.Op(Test, RAX, RAX, false)
			a.Jcc(CondE, next)
			a.Load(RAX, R9, offShapeProto)
			a.CmpMem(RAX, RCX, e+offHProto, true)
			a.Jcc(CondNE, next)
			a.LoadU8(RDX, RCX, e+offHHas)
			a.Jmp(found)
			a.Bind(next)
		}
		a.Jmp(g.exit)
		a.Bind(found)
		store := a.NewLabel()
		a.Load(R9, RDI, ncFalse)
		a.Op(Test, RDX, RDX, false)
		a.Jcc(CondE, store)
		a.Load(R9, RDI, ncTrue)
		a.Bind(store)
		a.Load(RAX, RBX, 0)
		a.MovQToX(0, R9)
		g.storeA(0)
		// A number key, an index an atom holds: its element of an array or
		// an ordinary object, or of one up their chain, as hasIndexQuick
		// finds it, an object of other kinds or of index keys exiting.
		yes, no, loop, noElems := a.NewLabel(), a.NewLabel(), a.NewLabel(), a.NewLabel()
		a.Bind(index)
		a.MovQToX(0, R9)
		a.Cvttsd2si(RCX, 0)
		a.Op(Test, RCX, RCX, true)
		a.Jcc(CondS, g.exit)
		a.MovImm(RDX, uint64(atomIndexTag))
		a.Op(Cmp, RCX, RDX, true)
		a.Jcc(CondAE, g.exit)
		a.Cvtsi2sd(1, RCX, true)
		a.SSEOp(UcomiSD, 0, 1)
		a.Jcc(CondNE, g.exit)
		a.Jcc(CondP, g.exit)
		a.MovRR(R8, RSI)
		a.Bind(loop)
		a.LoadU8(RDX, R8, offObjClass)
		a.OpImm(Cmp, RDX, int32(ClassArray), false)
		a.Jcc(CondA, g.exit)
		a.LoadU16(RDX, R8, offObjFlags)
		a.MovRR(R9, RDX)
		a.OpImm(And, R9, int32(objPolyProto), false)
		a.Jcc(CondNE, g.exit)
		a.OpImm(And, RDX, int32(objHasExt), false)
		a.Jcc(CondE, noElems)
		a.CmpMem(RCX, R8, sizeObj+8, true)
		a.Jcc(CondAE, noElems)
		a.MovRR(R9, RCX)
		a.ShiftImm(Shl, R9, 4, true)
		a.AddMem(R9, R8, sizeObj)
		a.Load(R9, R9, 0)
		a.CmpMem(R9, RDI, ncHole, true)
		a.Jcc(CondNE, yes)
		a.Bind(noElems)
		g.noIndexKeys(R8)
		a.Load(R8, R8, offObjShape)
		a.Op(Test, R8, R8, true)
		a.Jcc(CondE, no)
		a.Load(R8, R8, offShapeProto)
		a.Op(Test, R8, R8, true)
		a.Jcc(CondE, no)
		a.Jmp(loop)
		a.Bind(yes)
		a.MovImm(RDX, 1)
		a.Jmp(found)
		a.Bind(no)
		a.Op(Xor, RDX, RDX, false)
		a.Jmp(found)
	})
	h(bytecode.RGetLength, func() {
		// An array's length but a sparse one's, a string's.
		str, num := a.NewLabel(), a.NewLabel()
		g.decB(RSI)
		a.LoadIdx(R9, R12, RSI, 1, 0)
		a.LoadIdx(R8, R12, RSI, 1, 8)
		a.CmpMem(R9, RDI, ncObjBits, true)
		a.Jcc(CondNE, str)
		a.LoadU8(RCX, R8, offObjClass)
		a.OpImm(Cmp, RCX, int32(ClassArray), false)
		a.Jcc(CondNE, g.exit)
		a.LoadU16(RCX, R8, offObjFlags)
		a.OpImm(And, RCX, int32(objHasSparseElements|objHasExt), false)
		a.OpImm(Cmp, RCX, int32(objHasExt), false)
		a.Jcc(CondNE, g.exit)
		a.Load(RCX, R8, sizeObj+8)
		a.Jmp(num)
		a.Bind(str)
		a.MovRR(RCX, R9)
		a.Op(Xor, RCX, R10, true)
		a.ShiftImm(Shr, RCX, 16, true)
		a.Jcc(CondNE, g.exit)
		a.MovZX8(RCX, R9)
		a.OpImm(Cmp, RCX, int32(KindString), false)
		a.Jcc(CondNE, g.exit)
		a.Load(RCX, R8, offStrLen)
		a.Bind(num)
		a.Cvtsi2sd(0, RCX, true)
		g.storeA(0)
	})
	{
		h(bytecode.RCallW, g.call)
		h(bytecode.RCallMW, g.call)
		h(bytecode.RRet, func() { g.ret(false) })
		h(bytecode.RRetUndef, func() { g.ret(true) })
	}
	// The bitwise operations and the compares of no kernel's, after the
	// others.
	extra()
	// What came later goes last, so that it moves none of the above.
	h(bytecode.RPostDec, func() { g.postStep(SubSD) })
	genStep := a.NewLabel()
	h(bytecode.RIterNext, func() {
		// A for-of's step over a dense array by the intrinsic iterator
		// (iterState.arr): its next element but a hole, the cursor's
		// index stepped; over a generator, genStep's (amd64_gen.go).
		// Anything else is Go's.
		g.decB(RSI)
		g.object(RSI, RSI)
		a.LoadU8(RCX, RSI, offObjClass)
		a.OpImm(Cmp, RCX, int32(ClassCursor), false)
		a.Jcc(CondNE, g.exit)
		a.LoadU8(RCX, RSI, offItDone)
		a.Op(Test, RCX, RCX, false)
		a.Jcc(CondNE, g.exit)
		a.Load(R8, RSI, offItArr)
		a.Op(Test, R8, R8, true)
		a.Jcc(CondE, genStep)
		a.Load(RCX, RSI, offItIdx)
		// Past the end of an array of no sparse elements, its length:
		// the cursor done and the loop left, as iterNext leaves it.
		end := a.NewLabel()
		a.LoadU16(RDX, R8, offObjFlags)
		a.OpImm(And, RDX, int32(objHasExt|objHasSparseElements), false)
		a.OpImm(Cmp, RDX, int32(objHasExt), false)
		a.Jcc(CondNE, g.exit)
		a.CmpMem(RCX, R8, sizeObj+8, true)
		a.Jcc(CondAE, end)
		g.dense(RCX, R8, 0, g.exit)
		a.IncMem(RSI, offItIdx)
		g.loadValue(RCX)
		g.decA(RDX)
		a.LeaIdx(RAX, R12, RDX, 1, 0)
		g.storeValue()
		a.Bind(end)
		a.MovImm(RCX, 1)
		a.Store8(RSI, offItDone, RCX)
		g.immSC(RCX)
		g.taken(RCX)
	})
	// A call in tail position while the stack is shallow is call_w's or
	// call_method_w's (rhCallWT); deeper, the frame given to the callee in
	// place (tailTo), or Go's.
	tailIn := a.NewLabel()
	for i, op := range []bytecode.ROp{bytecode.RCallWT, bytecode.RCallMWT} {
		h(op, func() {
			a.Load(R9, RDI, ncRT)
			a.Load(RCX, R9, offRtFrameDepth)
			a.OpImm(Cmp, RCX, tailCallDepth, true)
			a.Jcc(CondGE, tailIn)
			a.Jmp(label(bytecode.RCallW))
			if i == 0 {
				g.late = append(g.late, func() {
					a.Bind(tailIn)
					g.tailTo()
				})
			}
		})
	}
	h(bytecode.RCharCodeAt, func() {
		// s.charCodeAt(i) of a flat string, the method the built-in read
		// from the realm's cache of String.prototype's properties and i a
		// number, as regCharCodeAt reads it; a rope, the code units of a
		// string not ASCII not made yet, anything else Go's.
		nan, wide, have, idx := a.NewLabel(), a.NewLabel(), a.NewLabel(), a.NewLabel()
		g.decB(RSI)
		a.LoadIdx(R9, R12, RSI, 1, 0)
		a.MovRR(RCX, R9)
		a.Op(Xor, RCX, R10, true)
		a.ShiftImm(Shr, RCX, 16, true)
		a.Jcc(CondNE, g.exit)
		a.MovZX8(RCX, R9)
		a.OpImm(Cmp, RCX, int32(KindString), false)
		a.Jcc(CondNE, g.exit)
		a.LoadIdx(RSI, R12, RSI, 1, 8)
		a.Store(RDI, scr(sV0), RSI)
		a.Load(RDX, RDI, ncRT)
		a.Load(RDX, RDX, rtRel(0))
		a.Load(R8, RDI, ncProtoStr)
		a.LoadIdx(R8, RDX, R8, 1, 0)
		a.Op(Test, R8, R8, true)
		a.Jcc(CondE, g.exit)
		g.primSlot()
		// The built-in itself, not bound.
		a.Load(R9, RCX, 0)
		a.CmpMem(R9, RDI, ncObjBits, true)
		a.Jcc(CondNE, g.exit)
		a.Load(R9, RCX, 8)
		a.LoadU16(RDX, R9, offObjFlags)
		a.OpImm(And, RDX, int32(objHasData), false)
		a.Jcc(CondE, g.exit)
		a.Load(RDX, R9, sizeObj+offExtData)
		a.CmpMem(RDX, RDI, ncFuncT, true)
		a.Jcc(CondNE, g.exit)
		a.Load(R9, R9, sizeObj+offExtData+8)
		a.LoadU8(RDX, R9, offFDElemOp)
		a.OpImm(Cmp, RDX, int32(elemStrCharCodeAt), false)
		a.Jcc(CondNE, g.exit)
		a.LoadU8(RDX, R9, offFDBound)
		a.Op(Test, RDX, RDX, false)
		a.Jcc(CondNE, g.exit)
		// The position, NaN as 0, truncated: one below the length, or NaN.
		a.MovRR(RDX, RAX)
		a.ShiftImm(Shr, RDX, 8, true)
		a.MovZX8(RDX, RDX)
		a.ShiftImm(Shl, RDX, 4, true)
		g.num(0, RDX)
		a.SSEOp(UcomiSD, 0, 0)
		a.Jcc(CondNP, idx)
		a.SSEOp(XorPD, 0, 0)
		a.Bind(idx)
		a.Cvttsd2si(RCX, 0)
		a.Load(RSI, RDI, scr(sV0))
		a.CmpMem(RCX, RSI, offStrLen, true)
		a.Jcc(CondAE, nan)
		a.CmpMemImm8(RSI, offStrLeft, 0)
		a.Jcc(CondNE, g.exit)
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
		a.Jcc(CondE, g.exit)
		a.LeaIdx(RDX, RDX, RCX, 2, 0)
		a.LoadU16(RCX, RDX, 0)
		a.Bind(have)
		a.Cvtsi2sd(0, RCX, true)
		g.storeA(0)
		a.Bind(nan)
		a.LoadSD(0, RDI, ncNaN)
		g.storeA(0)
	})
	// o[k] += v, a statement's update of a dense array's element that
	// holds a number, by a number: its word alone written, as
	// rhAddIndexK writes it. The object in RSI, the value in X1, the key
	// in X0 or RCX; anything else exits.
	elemAdd := func() {
		a.Load(R9, RCX, 0)
		a.Op(Cmp, R9, R10, true)
		a.Jcc(CondAE, g.exit)
		a.MovQToX(0, R9)
		a.SSEOp(AddSD, 0, 1)
		g.canon(0)
		a.StoreSD(RCX, 0, 0)
		g.next()
	}
	elemMask := objMappedArguments | objSharedElems
	h(bytecode.RAddIndex, func() {
		g.decA(RDX)
		g.object(RSI, RDX)
		g.decC(RDX)
		g.num(1, RDX)
		g.decB(R8)
		g.num(0, R8)
		g.element(RCX, RSI, 0, elemMask, g.exit)
		elemAdd()
	})
	h(bytecode.RAddIndexK, func() {
		g.decA(RDX)
		g.object(RSI, RDX)
		g.decB(RDX)
		g.num(1, RDX)
		a.MovRR(RCX, RAX)
		a.ShiftImm(Shr, RCX, 48, true)
		g.dense(RCX, RSI, elemMask, g.exit)
		elemAdd()
	})
	h(bytecode.RAddIndexI, func() {
		g.decA(RDX)
		g.object(RSI, RDX)
		g.immSC(R9)
		a.Cvtsi2sd(1, R9, true)
		g.decB(R8)
		g.num(0, R8)
		g.element(RCX, RSI, 0, elemMask, g.exit)
		elemAdd()
	})
	// A private member read or written by its cache site (Aux's high 16
	// bits), as regGetPrivate and regSetPrivate read and write it: an
	// object of the shape the site remembers, its key there the private
	// name in R[C], a symbol interned in the runtime's atoms. privSlot
	// puts the slot's address in RDX, the object in RSI; anything else
	// exits.
	privSlot := func() {
		a.Load(R8, RDI, ncRC)
		a.Load(R8, R8, offRCAux)
		a.Op(Test, R8, R8, true)
		a.Jcc(CondE, g.exit)
		a.MovRR(RCX, RBX)
		a.Load(R9, RDI, ncCodeBase)
		a.Op(Sub, RCX, R9, true)
		a.ShiftImm(Shr, RCX, 1, true)
		a.Op(Add, R8, RCX, true)
		a.LoadU32(RCX, R8, 0)
		a.ShiftImm(Shr, RCX, 16, false)
		a.Op(Test, RCX, RCX, false)
		a.Jcc(CondE, g.exit)
		a.MovImm(R8, uint64(size("propCache")))
		a.Imul(RCX, R8)
		a.Op(Add, RCX, R15, true)
		g.ownSlot(RDX, RSI)
		g.decC(R8)
		a.LoadIdx(R9, R12, R8, 1, 0)
		a.MovImm(RAX, tagBase|uint64(KindSymbol))
		a.Op(Cmp, R9, RAX, true)
		a.Jcc(CondNE, g.exit)
		a.LoadIdx(R9, R12, R8, 1, 8)
		a.Load(R8, R9, offSymAtoms)
		a.Load(RAX, RDI, ncRT)
		a.CmpMem(R8, RAX, offRtAtoms, true)
		a.Jcc(CondNE, g.exit)
		a.LoadU32(R8, R9, offSymAtom)
		a.Load(R9, RSI, off("Object.shape"))
		a.Load(R9, R9, off("shape.keys"))
		a.LoadS32(RAX, RCX, off("propCache.idx"))
		a.LeaIdx(R9, R9, RAX, uint8(size("propKey")), 0)
		a.LoadU32(RAX, R9, off("propKey.key"))
		a.Op(Cmp, R8, RAX, false)
		a.Jcc(CondNE, g.exit)
		a.Load(RAX, RBX, 0)
	}
	h(bytecode.RGetPrivate, func() {
		g.decB(RSI)
		g.object(RSI, RSI)
		privSlot()
		a.Load(R9, RDX, 0)
		a.Load(R8, RDX, 8)
		g.decA(RDX)
		a.LeaIdx(RAX, R12, RDX, 1, 0)
		g.storeValue()
	})
	h(bytecode.RSetPrivate, func() {
		g.decA(RSI)
		g.object(RSI, RSI)
		privSlot()
		g.decB(R8)
		a.LoadIdx(R9, R12, R8, 1, 0)
		a.LoadIdx(R8, R12, R8, 1, 8)
		a.MovRR(RAX, RDX)
		g.storeValue()
	})
	// A generator's yield in a frame iter_next entered, and that step,
	// after it (amd64_gen.go).
	h(bytecode.RYield, func() {
		g.genYield()
		a.Bind(genStep)
		g.genStep()
		a.Bind(g.genRet)
		g.genReturn()
	})
	// Closures and the upvalues they capture (amd64_closure.go).
	h(bytecode.RCloseRegs, g.closeRegs)
	h(bytecode.RClosureW, g.closureW)
	// A switch's case found by its table (amd64_switch.go).
	h(bytecode.RSwitch, g.switchW)
	// A division or a remainder of numbers (amd64_bin.go).
	h(bytecode.RBin, g.bin)
	// str_freeze: R[A], where it is a string an append owns (building),
	// no longer grown in place, as rhStrFreeze freezes it: its flag, a
	// byte of no pointer, cleared where it is set, which leaves a string
	// another runtime may share unwritten. Anything else is left as it is.
	h(bytecode.RNewObjectLit, g.litCall)
	h(bytecode.RNewArrayE, g.litCall)
	h(bytecode.RStrFreeze, func() {
		done := a.NewLabel()
		g.decA(RDX)
		a.LoadIdx(R9, R12, RDX, 1, 0)
		a.MovRR(RCX, R9)
		a.Op(Xor, RCX, R10, true)
		a.ShiftImm(Shr, RCX, 16, true)
		a.Jcc(CondNE, done)
		a.MovZX8(RCX, R9)
		a.OpImm(Cmp, RCX, int32(KindString), false)
		a.Jcc(CondNE, done)
		a.LoadIdx(RSI, R12, RDX, 1, 8)
		a.LoadU8(RCX, RSI, offStrBuilding)
		a.Op(Test, RCX, RCX, false)
		a.Jcc(CondE, done)
		a.Op(Xor, RCX, RCX, false)
		a.Store8(RSI, offStrBuilding, RCX)
		a.Bind(done)
		g.next()
	})
	// jtypeof: the jump taken where typeof R[A]'s type is, or is not as
	// X's RCmpTrue says, the type of index X&^RCmpTrue, as rhJTypeof
	// takes it (typeIndex); a proxy, document.all and anything else
	// exits.
	h(bytecode.RJTypeof, func() {
		take, ne, eq := a.NewLabel(), a.NewLabel(), a.NewLabel()
		g.decA(RSI)
		g.typeIndex(RSI)
		a.MovRR(R8, RAX)
		a.ShiftImm(Shr, R8, 8, true)
		a.MovZX8(R8, R8)
		a.MovRR(R9, R8)
		a.OpImm(And, R9, int32(^bytecode.RCmpTrue&0xFF), false)
		a.OpImm(And, R8, int32(bytecode.RCmpTrue), false)
		a.Op(Cmp, RCX, R9, true)
		a.Jcc(CondE, eq)
		a.Op(Test, R8, R8, false)
		a.Jcc(CondE, take)
		a.Jmp(ne)
		a.Bind(eq)
		a.Op(Test, R8, R8, false)
		a.Jcc(CondE, ne)
		a.Bind(take)
		g.immSC(RCX)
		g.taken(RCX)
		a.Bind(ne)
		g.next()
	})
	// str_part: a part of a template that is a string or a number goes
	// on as it is, as rhStrPart lets it; anything else exits, for its
	// conversion.
	h(bytecode.RStrPart, func() {
		done := a.NewLabel()
		g.decA(RDX)
		a.LoadIdx(R9, R12, RDX, 1, 0)
		a.Op(Cmp, R9, R10, true)
		a.Jcc(CondB, done)
		a.MovRR(RCX, R9)
		a.Op(Xor, RCX, R10, true)
		a.ShiftImm(Shr, RCX, 16, true)
		a.Jcc(CondNE, g.exit)
		a.MovZX8(RCX, R9)
		a.OpImm(Cmp, RCX, int32(KindString), false)
		a.Jcc(CondNE, g.exit)
		a.Bind(done)
		g.next()
	})
	// gen_entry of a frame's first run: nothing to do, as regGenEntry
	// does nothing then; a throw injected (savedSP negative) is Go's.
	h(bytecode.RGenEntry, func() {
		zero := a.NewLabel()
		a.Load(RSI, RDI, ncNF)
		a.Load(RCX, RSI, offFSavedSP)
		a.Op(Test, RCX, RCX, true)
		a.Jcc(CondS, g.exit)
		a.Jcc(CondE, zero)
		a.StoreImm(RSI, offFSavedSP, 0)
		a.Bind(zero)
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
	if len(missing) > 0 {
		return nil, fmt.Errorf("%s: a jump to %v before its handler", v.name, missing)
	}
	b := &block{at: map[bytecode.ROp]int{}, order: order}
	if v != plain {
		if len(order) == 0 {
			// No handler of its own: its table is plain's.
			return b, nil
		}
		// The jumps to plain's code, through the table.
		a.Bind(g.exit)
		a.JmpMem(R13, 8*slotExit)
		a.Bind(g.exitRet)
		a.JmpMem(R13, 8*slotExitRet)
		for _, op := range far {
			a.Bind(farLabels[op])
			a.JmpMem(R13, 8*int32(op))
		}
	}
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
