package nativegen

import (
	"fmt"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
	. "github.com/go-quickjs/go-quickjs/internal/jit/asm/arm64"
)

// armEmit makes arm64's handlers' code for variant v, as nativeEmit makes
// amd64's: position-independent, every address it needs given by the
// context in R25. plain's is the handlers' block
// (zzzyn_native_handlers_arm64.s): the entry, the exit stub and every
// handler, in handlerOps' order. Another variant's is its own handlers
// alone, which reach plain's code through the variant's table, R21, by a
// load into R16 and a jump through it: arm64 has no jump through memory.
func armEmit(v *variant) (*block, error) {
	g := &armGen{a: &Asm{}, v: v}
	a := g.a
	g.exit, g.exitRet = a.NewLabel(), a.NewLabel()
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
	var far []bytecode.ROp
	farLabels := map[bytecode.ROp]Label{}
	var missing []bytecode.ROp
	label := func(op bytecode.ROp) Label {
		if l, ok := labels[op]; ok {
			return l
		}
		if v.has(op) {
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
		g.reload()
		g.dispatch()
		g.exitStub()
	}

	extra := func() {
		h(bytecode.RBitOr, func() { g.bitReg(func() { a.Op(Orr, r9, r9, rC, false) }) })
		h(bytecode.RBitXor, func() { g.bitReg(func() { a.Op(Eor, r9, r9, rC, false) }) })
		h(bytecode.RShl, func() { g.bitReg(func() { a.ShiftReg(Lsl, r9, r9, rC, false) }) })
		h(bytecode.RShr, func() { g.bitReg(func() { a.ShiftReg(Asr, r9, r9, rC, false) }) })
		h(bytecode.RUShr, func() {
			g.bitReg(func() { a.ShiftReg(Lsr, r9, r9, rC, false) }, true)
		})
		h(bytecode.RXorI, func() { g.bitImm(func() { a.Op(Eor, r9, r9, rC, false) }) })
		h(bytecode.RUShrI, func() { g.bitImm(func() { a.ShiftReg(Lsr, r9, r9, rC, false) }, true) })
		h(bytecode.RJLe, func() { g.cmp(false, true, ccAE, false) })
		h(bytecode.RJNLe, func() { g.cmp(false, true, ccB, false) })
		h(bytecode.RJGt, func() { g.cmp(false, false, ccA, false) })
	}

	h(bytecode.RNop, func() { g.next() })
	h(bytecode.RMove, func() {
		// Any value: its pointer word by storeValue.
		g.regB(rS)
		g.loadValue(rS)
		g.regA(rW)
		g.storeValue()
	})
	h(bytecode.RLoadConst, func() {
		a.ShiftImm(Lsr, rC, rW, 32, true)
		a.ShiftImm(Lsl, rC, rC, 4, true)
		a.Load(r8, rCtx, ncCC)
		g.addMem(rC, r8, offCCConsts)
		g.loadValue(rC)
		g.regA(rW)
		g.storeValue()
	})
	// The slot a global site remembers while the global epoch is its, in
	// rC, or an exit.
	globalSlot := func() {
		a.ShiftImm(Lsr, rC, rW, 48, true)
		a.Load(r8, rCtx, ncCC)
		g.cmpMem(rC, r8, offCCRegEpochs+8)
		a.BCond(HS, g.exit)
		a.ShiftImm(Lsl, rC, rC, 3, true)
		g.addMem(rC, r8, offCCRegEpochs)
		a.Load(rC, rC, 0)
		a.Load(rS, rCtx, ncEpoch)
		a.Load(rS, rS, 0)
		a.AddImm(rS, rS, 1, true)
		a.Cmp(rC, rS, true)
		a.BCond(NE, g.exit)
		g.site(rC)
		a.Load(rS, rC, off("propCache.p1"))
		a.LoadS32(rC, rC, off("propCache.idx"))
		a.LoadU32(r8, rS, off("Object.valn"))
		a.Cmp(rC, r8, true)
		a.BCond(HS, g.exit)
		a.ShiftImm(Lsl, rC, rC, 4, true)
		g.addMem(rC, rS, off("Object.valp"))
	}
	h(bytecode.RGetGlobal, func() {
		globalSlot()
		g.loadValue(rC)
		g.regA(rW)
		g.storeValue()
	})
	h(bytecode.RLoadInt, func() {
		a.ShiftImm(Asr, rC, rW, 32, true)
		a.Scvtf(0, rC, true)
		g.storeA(0)
	})
	h(bytecode.RLoadUndef, func() { a.LoadF(0, rCtx, ncUndef); g.storeA(0) })
	h(bytecode.RLoadNull, func() { a.LoadF(0, rCtx, ncNull); g.storeA(0) })
	h(bytecode.RLoadTrue, func() { a.LoadF(0, rCtx, ncTrue); g.storeA(0) })
	h(bytecode.RLoadFalse, func() { a.LoadF(0, rCtx, ncFalse); g.storeA(0) })
	h(bytecode.RLoadUninit, func() { a.LoadF(0, rCtx, ncUninit); g.storeA(0) })
	h(bytecode.RLoadEmpty, func() {
		a.Load(r9, rCtx, ncStrBits)
		a.Load(r8, rCtx, ncEmptyStr)
		g.regA(rW)
		g.storeValue()
	})
	h(bytecode.RGetUpval, func() {
		a.ShiftImm(Lsr, rC, rW, 32, true)
		g.upval(rS, rC)
		g.loadValue(rS)
		g.regA(rW)
		g.storeValue()
	})
	h(bytecode.RSetUpval, func() {
		g.regA(rD)
		g.loadValue(rD)
		a.ShiftImm(Lsr, rC, rW, 32, true)
		g.upval(rW, rC)
		g.storeValue()
	})
	h(bytecode.RJLtU, func() { g.cmpU(ccA) })
	h(bytecode.RSetGlobal, func() {
		globalSlot()
		g.regA(rD)
		g.loadValue(rD)
		a.MovRR(rW, rC)
		g.storeValue()
	})
	// o.n += R[B] and o.n += SB of an own number the site remembers: its
	// word alone written.
	for _, imm := range []bool{false, true} {
		op := bytecode.RAddProp
		if imm {
			op = bytecode.RAddPropI
		}
		h(op, func() {
			g.regA(rD)
			g.object(rS, rD)
			if imm {
				g.immSB(r9)
				a.Scvtf(1, r9, true)
			} else {
				g.regB(rD)
				g.num(1, rD)
			}
			g.site(rC)
			g.ownSlot(rD, rS)
			a.Load(r9, rD, 0)
			a.Cmp(r9, rTag, true)
			a.BCond(HS, g.exit)
			a.FMovToF(0, r9)
			a.FArith(FAdd, 0, 0, 1)
			g.canon(0)
			a.StoreF(rD, 0, 0)
			g.next()
		})
	}
	h(bytecode.RJNLtU, func() { g.cmpU(ccBE) })
	h(bytecode.RJLeU, func() { g.cmpU(ccAE) })
	h(bytecode.RJNLeU, func() { g.cmpU(ccB) })
	h(bytecode.RJEq, func() { g.looseEq(true) })
	h(bytecode.RJNe, func() { g.looseEq(false) })
	h(bytecode.RKeyOfBase, func() {
		// A string, a number or a symbol is its own key.
		key := a.NewLabel()
		g.regC(rS)
		a.Load(r9, rS, 0)
		a.Cmp(r9, rTag, true)
		a.BCond(LO, key)
		g.tagged(r9, g.exit)
		a.CmpImm(rC, int64(KindString), false)
		a.BCond(EQ, key)
		a.CmpImm(rC, int64(KindSymbol), false)
		a.BCond(NE, g.exit)
		a.Bind(key)
		a.Load(r8, rS, 8)
		g.regA(rW)
		g.storeValue()
	})
	h(bytecode.RAdd, func() { g.arith(FAdd, 0) })
	h(bytecode.RSub, func() { g.arith(FSub, 0) })
	h(bytecode.RMul, func() { g.arith(FMul, 0) })
	h(bytecode.RAddI, func() { g.arith(FAdd, 1) })
	h(bytecode.RSubI, func() { g.arith(FSub, 1) })
	h(bytecode.RInc, func() { g.arith(FAdd, 2) })
	h(bytecode.RDec, func() { g.arith(FSub, 2) })
	h(bytecode.RPostInc, func() { g.postStep(FAdd) })
	h(bytecode.RAndI, func() { g.bitImm(func() { a.Op(And, r9, r9, rC, false) }) })
	h(bytecode.ROrI, func() { g.bitImm(func() { a.Op(Orr, r9, r9, rC, false) }) })
	h(bytecode.RShlI, func() { g.bitImm(func() { a.ShiftReg(Lsl, r9, r9, rC, false) }) })
	h(bytecode.RShrI, func() { g.bitImm(func() { a.ShiftReg(Asr, r9, r9, rC, false) }) })
	h(bytecode.RBitAnd, func() { g.bitReg(func() { a.Op(And, r9, r9, rC, false) }) })
	h(bytecode.RJmp, func() {
		a.ShiftImm(Asr, rC, rW, 32, true)
		g.taken(rC)
	})
	// Compares: A op B is FCMP A, B (swap false) or B, A (swap true).
	h(bytecode.RJLt, func() { g.cmp(false, true, ccA, false) })
	h(bytecode.RJNLt, func() { g.cmp(false, true, ccBE, false) })
	h(bytecode.RJNGt, func() { g.cmp(false, false, ccBE, false) })
	h(bytecode.RJGe, func() { g.cmp(false, false, ccAE, false) })
	h(bytecode.RJNGe, func() { g.cmp(false, false, ccB, false) })
	h(bytecode.RJLtI, func() { g.cmp(true, true, ccA, false) })
	h(bytecode.RJNLtI, func() { g.cmp(true, true, ccBE, false) })
	h(bytecode.RJLeI, func() { g.cmp(true, true, ccAE, false) })
	h(bytecode.RJNLeI, func() { g.cmp(true, true, ccB, false) })
	h(bytecode.RJGtI, func() { g.cmp(true, false, ccA, false) })
	h(bytecode.RJNGtI, func() { g.cmp(true, false, ccBE, false) })
	h(bytecode.RJGeI, func() { g.cmp(true, false, ccAE, false) })
	h(bytecode.RJNGeI, func() { g.cmp(true, false, ccB, false) })
	h(bytecode.RJNeI, func() { g.cmp(true, false, ccNE, true) })
	for _, op := range []bytecode.ROp{bytecode.RJmpF, bytecode.RJmpT} {
		jumpIf := op == bytecode.RJmpT
		h(op, func() {
			// ToBoolean of a boolean, an object or a number, else Go's.
			isTrue, isFalse, notBool, notObj := a.NewLabel(), a.NewLabel(), a.NewLabel(), a.NewLabel()
			g.regA(rD)
			a.Load(r9, rD, 0)
			g.cmpMem(r9, rCtx, ncTrue)
			a.BCond(NE, notBool)
			a.B(isTrue)
			a.Bind(notBool)
			g.cmpMem(r9, rCtx, ncFalse)
			a.BCond(EQ, isFalse)
			g.cmpMem(r9, rCtx, ncObjBits)
			a.BCond(NE, notObj)
			a.Load(r9, rD, 8)
			a.LoadU16(rC, r9, off("Object.flags"))
			a.TstImm(rC, uint64(objHTMLDDA), false)
			a.BCond(NE, isFalse)
			a.B(isTrue)
			a.Bind(notObj)
			tagged := a.NewLabel()
			a.Cmp(r9, rTag, true)
			a.BCond(HS, tagged)
			a.FMovToF(0, r9)
			a.FMovToF(1, ZR)
			a.FCmp(0, 1)
			a.BCond(VS, isFalse)
			a.BCond(EQ, isFalse)
			a.Bind(isTrue)
			if jumpIf {
				a.ShiftImm(Asr, rC, rW, 32, true)
				g.taken(rC)
				a.Bind(isFalse)
				g.next()
			} else {
				g.next()
				a.Bind(isFalse)
				a.ShiftImm(Asr, rC, rW, 32, true)
				g.taken(rC)
			}
			// undefined and null are false, a string true unless empty;
			// anything else is Go's.
			a.Bind(tagged)
			g.nullish(r9, isFalse)
			g.tagged(r9, g.exit)
			a.CmpImm(rC, int64(KindString), false)
			a.BCond(NE, g.exit)
			a.Load(r9, rD, 8)
			g.cmpMemImm(r9, offStrLen, 0)
			a.BCond(EQ, isFalse)
			a.B(isTrue)
		})
	}
	// == of the immediate SB: a number's, else Go's.
	h(bytecode.RJEqI, func() {
		ne := a.NewLabel()
		g.regA(rD)
		g.num(0, rD)
		g.immSB(rC)
		a.Scvtf(1, rC, true)
		a.FCmp(0, 1)
		a.BCond(NE, ne)
		g.immSC(rC)
		g.taken(rC)
		a.Bind(ne)
		g.next()
	})
	// -x, +x and ~x of a number; anything else, and !x, Go's.
	h(bytecode.RUnary, func() {
		pos, not := a.NewLabel(), a.NewLabel()
		g.regB(rS)
		g.num(0, rS)
		a.Ubfx(r8, rW, 8, 8)
		a.CmpImm(r8, int64(bytecode.OpPos), false)
		a.BCond(EQ, pos)
		a.CmpImm(r8, int64(bytecode.OpBitNot), false)
		a.BCond(EQ, not)
		a.CmpImm(r8, int64(bytecode.OpNeg), false)
		a.BCond(NE, g.exit)
		a.FNeg(0, 0)
		a.Bind(pos)
		g.canon(0)
		g.storeA(0)
		a.Bind(not)
		g.toInt32(r9, 0)
		a.Mvn(r9, r9, false)
		a.Scvtf(0, r9, false)
		g.storeA(0)
	})
	for _, op := range []bytecode.ROp{bytecode.RJNull, bytecode.RJNotNull} {
		jumpIfNull := op == bytecode.RJNull
		h(op, func() {
			isNull, notNull, notObj := a.NewLabel(), a.NewLabel(), a.NewLabel()
			g.regA(rD)
			a.Load(r9, rD, 0)
			g.cmpMem(r9, rCtx, ncObjBits)
			a.BCond(NE, notObj)
			a.Load(r9, rD, 8)
			a.LoadU16(rC, r9, off("Object.flags"))
			a.TstImm(rC, uint64(objHTMLDDA), false)
			a.BCond(NE, g.exit)
			a.B(notNull)
			a.Bind(notObj)
			g.tagged(r9, notNull)
			a.CmpImm(rC, int64(KindNull), false)
			a.BCond(EQ, isNull)
			a.CmpImm(rC, int64(KindUndefined), false)
			a.BCond(EQ, isNull)
			a.B(notNull)
			if jumpIfNull {
				a.Bind(notNull)
				g.next()
				a.Bind(isNull)
			} else {
				a.Bind(isNull)
				g.next()
				a.Bind(notNull)
			}
			g.immSC(rC)
			g.taken(rC)
		})
	}
	getProp := func() {
		g.regB(rS)
		g.object(rS, rS)
		g.site(rC)
		g.readSlot()
		g.loadValue(rC)
		g.regA(rW)
		g.storeValue()
	}
	h(bytecode.RGetMethodW, func() {
		// R[A] = the method, R[A+1] = the receiver: two stores of
		// pointers, the receiver kept across the first. A string's or a
		// number's method is primMethod's.
		prim := a.NewLabel()
		g.regB(rS)
		a.Load(r9, rS, 0)
		g.cmpMem(r9, rCtx, ncObjBits)
		a.BCond(NE, prim)
		a.Load(rS, rS, 8)
		a.Store(rCtx, scr(sV0), rS)
		g.site(rC)
		g.readSlot()
		g.loadValue(rC)
		g.regA(rW)
		a.Store(rW, 0, r9)
		a.AddImm(rW, rW, 8, true)
		a.MovRR(rC, r8)
		g.ptrStore()
		a.Load(rW, rIP, 0)
		g.regA(rW)
		a.AddImm(rW, rW, 16, true)
		a.Load(r9, rCtx, ncObjBits)
		a.Store(rW, 0, r9)
		a.AddImm(rW, rW, 8, true)
		a.Load(rC, rCtx, scr(sV0))
		g.ptrStore()
		g.next()
		a.Bind(prim)
		g.primMethod()
	})
	// === against an immediate: only a number is equal to one.
	h(bytecode.RJSEqI, func() {
		notNum := a.NewLabel()
		g.regA(rD)
		a.Load(r9, rD, 0)
		a.Cmp(r9, rTag, true)
		a.BCond(HS, notNum)
		a.FMovToF(0, r9)
		g.immSB(rC)
		a.Scvtf(1, rC, true)
		a.FCmp(0, 1)
		a.BCond(NE, notNum)
		g.immSC(rC)
		g.taken(rC)
		a.Bind(notNum)
		g.next()
	})
	// === and !== of two numbers, or of two values one of which is
	// undefined, null, a boolean, a symbol or an object, equal only if
	// their words are; two strings by identity, length and flat text.
	for _, op := range []bytecode.ROp{bytecode.RJSEq, bytecode.RJSNe} {
		jumpIfEq := op == bytecode.RJSEq
		h(op, func() {
			eq, ne, words, same := a.NewLabel(), a.NewLabel(), a.NewLabel(), a.NewLabel()
			g.regA(rD)
			g.regB(rS)
			a.Load(r9, rD, 0)
			a.Load(r8, rS, 0)
			a.Cmp(r9, rTag, true)
			a.BCond(HS, words)
			a.Cmp(r8, rTag, true)
			a.BCond(HS, words)
			a.FMovToF(0, r9)
			a.FMovToF(1, r8)
			a.FCmp(0, 1)
			a.BCond(NE, ne)
			a.B(eq)
			a.Bind(words)
			// A word kind: tagged, below the BigInts, of a kind in the
			// mask. rW is scratch, the instruction read again after.
			for _, x := range []Reg{r9, r8} {
				notWord := a.NewLabel()
				g.kindIn(x, 1<<KindUndefined|1<<KindNull|1<<KindBool|1<<KindSymbol|1<<KindObject, same, notWord)
				a.Bind(notWord)
			}
			// Two strings: the same one, of different lengths, or two
			// flat ones compared by their text (flatEq); any other pair
			// Go's.
			for _, x := range []Reg{r9, r8} {
				g.tagged(x, g.exit)
				a.CmpImm(rC, int64(KindString), false)
				a.BCond(NE, g.exit)
			}
			a.Load(rW, rIP, 0)
			a.Load(r9, rD, 8)
			a.Load(r8, rS, 8)
			a.Cmp(r9, r8, true)
			a.BCond(EQ, eq)
			a.Load(rC, r9, offStrLen)
			g.cmpMem(rC, r8, offStrLen)
			a.BCond(NE, ne)
			g.flatEq(eq, ne)
			a.Bind(same)
			a.Load(rW, rIP, 0)
			a.Cmp(r9, r8, true)
			a.BCond(NE, ne)
			a.Load(r9, rD, 8)
			a.Load(r8, rS, 8)
			a.Cmp(r9, r8, true)
			a.BCond(NE, ne)
			a.B(eq)
			if jumpIfEq {
				a.Bind(eq)
				g.immSC(rC)
				g.taken(rC)
				a.Bind(ne)
				g.next()
			} else {
				a.Bind(ne)
				g.immSC(rC)
				g.taken(rC)
				a.Bind(eq)
				g.next()
			}
		})
	}
	h(bytecode.RJSNeI, func() {
		tk := a.NewLabel()
		g.regA(rD)
		a.Load(r9, rD, 0)
		a.Cmp(r9, rTag, true)
		a.BCond(HS, tk)
		a.FMovToF(0, r9)
		g.immSB(rC)
		a.Scvtf(1, rC, true)
		a.FCmp(0, 1)
		a.BCond(NE, tk)
		g.next()
		a.Bind(tk)
		g.immSC(rC)
		g.taken(rC)
	})
	h(bytecode.RGetProp, getProp)
	h(bytecode.RGetMethod, getProp)
	h(bytecode.RSetProp, func() {
		add := a.NewLabel()
		g.regA(rD)
		g.object(rS, rD)
		g.regB(rD)
		g.site(rC)
		// The own slot's address in rW, the instruction's last use past.
		a.Load(r8, rC, off("propCache.ownShape"))
		g.cmpMem(r8, rS, off("Object.shape"))
		a.BCond(NE, add)
		a.LoadS32(rW, rC, off("propCache.idx"))
		a.ShiftImm(Lsl, rW, rW, 4, true)
		g.addMem(rW, rS, off("Object.valp"))
		g.loadValue(rD)
		g.storeValue()
		a.Bind(add)
		g.addProp()
	})
	// An element read: a number of a dense array's where the register
	// holds none, any value of one by storeValue, a typed array's.
	getElem := func(miss Label) {
		ref := a.NewLabel()
		a.Load(r9, rC, 0)
		g.cmpMemImm(rC, 8, 0)
		a.BCond(NE, ref)
		g.regA(rD)
		a.Load(rT, rD, 8)
		a.Cbnz(rT, ref, true)
		a.Store(rD, 0, r9)
		g.next()
		a.Bind(ref)
		a.Load(r8, rC, 8)
		g.regA(rW)
		g.storeValue()
		a.Bind(miss)
		big := a.NewLabel()
		g.typedRead(rS, rC, &big)
		g.storeA(0)
		a.Bind(big)
		g.typedBigRead()
	}
	h(bytecode.RGetIndex, func() {
		miss := a.NewLabel()
		g.regB(rS)
		g.object(rS, rS)
		g.regC(r8)
		g.num(0, r8)
		g.element(rC, rS, 0, objMappedArguments, miss)
		getElem(miss)
	})
	h(bytecode.RGetIndexI, func() {
		miss := a.NewLabel()
		g.regB(rS)
		g.object(rS, rS)
		a.ShiftImm(Lsr, rC, rW, 48, true)
		g.dense(rC, rS, objMappedArguments, miss)
		getElem(miss)
	})
	h(bytecode.RGetIndexPre, func() {
		// The key stepped by X, a signed byte, then the read at it.
		g.regB(rS)
		g.object(rS, rS)
		g.regC(r8)
		g.num(0, r8)
		a.Sbfx(rC, rW, 8, 8)
		a.Scvtf(1, rC, true)
		a.FArith(FAdd, 1, 1, 0)
		g.element(rC, rS, 1, objMappedArguments, g.exit)
		g.cmpMemImm(rC, 8, 0)
		a.BCond(NE, g.exit)
		g.regA(rD)
		g.noRef(rD)
		g.regC(r8)
		a.StoreF(r8, 0, 1)
		a.Load(r9, rC, 0)
		a.Store(rD, 0, r9)
		g.next()
	})
	h(bytecode.RGetIndexPost, func() {
		g.regB(rS)
		g.object(rS, rS)
		g.regC(r8)
		g.num(0, r8)
		g.element(rC, rS, 0, objMappedArguments, g.exit)
		g.cmpMemImm(rC, 8, 0)
		a.BCond(NE, g.exit)
		g.regA(rD)
		g.noRef(rD)
		// The key stepped by X, a signed byte.
		a.Sbfx(rS, rW, 8, 8)
		a.Scvtf(1, rS, true)
		a.FArith(FAdd, 1, 1, 0)
		a.StoreF(r8, 0, 1)
		a.Load(r9, rC, 0)
		a.Store(rD, 0, r9)
		g.next()
	})
	// An element write: any value over a dense array's element, a number
	// to a typed array.
	setElem := func(miss Label) {
		// Any value over a dense array's element, its pointer word by
		// storeValue.
		g.regC(r8)
		g.loadValue(r8)
		a.MovRR(rW, rC)
		g.storeValue()
		a.Bind(miss)
		bigW, typed := a.NewLabel(), a.NewLabel()
		g.arrayAppend(rS, rC, typed)
		a.Bind(typed)
		g.regC(r8)
		g.numOr(1, r8, bigW)
		g.typedWrite(rS, rC)
		g.next()
		a.Bind(bigW)
		g.typedBigWrite(rS, rC)
		g.next()
	}
	h(bytecode.RSetIndex, func() {
		miss := a.NewLabel()
		g.regA(rD)
		g.object(rS, rD)
		g.regB(r8)
		g.num(0, r8)
		g.element(rC, rS, 0, objMappedArguments|objSharedElems, miss)
		setElem(miss)
	})
	h(bytecode.RSetIndexI, func() {
		miss := a.NewLabel()
		g.regA(rD)
		g.object(rS, rD)
		a.Ubfx(rC, rW, 32, 16)
		g.dense(rC, rS, objMappedArguments|objSharedElems, miss)
		setElem(miss)
	})
	h(bytecode.RTypeof, func() {
		// typeof's string, the runtime's (typeofStrs) where it has made
		// it: of a number, a primitive, an object but a proxy and
		// document.all's kind.
		idx, obj, num, big, notFn := a.NewLabel(), a.NewLabel(), a.NewLabel(), a.NewLabel(), a.NewLabel()
		g.regB(rS)
		a.Load(r9, rS, 0)
		a.Cmp(r9, rTag, true)
		a.BCond(LO, num)
		a.MovImm(r8, heapBigBits)
		a.Cmp(r9, r8, true)
		a.BCond(HS, big)
		a.Ubfx(rC, r9, 0, 8)
		a.CmpImm(rC, int64(KindObject), false)
		a.BCond(EQ, obj)
		a.CmpImm(rC, int64(KindUndefined), false)
		a.BCond(LO, g.exit)
		a.CmpImm(rC, int64(KindBigInt), false)
		a.BCond(HI, g.exit)
		// undefined, null and a boolean one before their kind; a string,
		// a symbol and a BigInt at it.
		a.CmpImm(rC, int64(KindString), false)
		a.BCond(HS, idx)
		a.AddImm(rC, rC, -1, false)
		a.B(idx)
		a.Bind(num)
		a.MovImm(rC, 3)
		a.B(idx)
		a.Bind(big)
		a.MovImm(rC, 6)
		a.B(idx)
		a.Bind(obj)
		a.Load(r8, rS, 8)
		a.LoadU16(rD, r8, offObjFlags)
		a.MovImm(rC, 1)
		a.TstImm(rD, uint64(objHTMLDDA), false)
		a.BCond(NE, g.exit)
		a.TstImm(rD, uint64(objHasData), false)
		a.BCond(EQ, idx)
		a.Load(r9, r8, sizeObj+offExtData)
		g.cmpMem(r9, rCtx, ncFuncT)
		a.BCond(NE, notFn)
		a.MovImm(rC, 7)
		a.B(idx)
		a.Bind(notFn)
		g.cmpMem(r9, rCtx, ncProxyT)
		a.BCond(EQ, g.exit)
		a.Bind(idx)
		a.Load(r8, rCtx, ncTypeofs)
		a.LoadIdx(r8, r8, rC, true)
		a.Cbz(r8, g.exit, true)
		a.Load(r9, rCtx, ncStrBits)
		g.regA(rW)
		g.storeValue()
	})
	h(bytecode.RIn, func() {
		// R[A] = R[B] in R[C]: a string key's answer the realm's in
		// cache remembers of an object's layout and chain (hasCached);
		// anything else Go's, which fills the cache.
		found := a.NewLabel()
		g.regC(rS)
		g.object(rS, rS)
		g.regB(rD)
		a.Load(r9, rD, 0)
		g.kindIs(r9, KindString)
		a.Load(r8, rD, 8)
		a.LoadU16(rC, rS, offObjFlags)
		a.TstImm(rC, uint64(objPolyProto), false)
		a.BCond(NE, g.exit)
		a.Load(r9, rS, offObjShape)
		a.Cbz(r9, g.exit, true)
		// The cache of the realm running now, the Runtime's first word,
		// at the offset the context gives (ncHas).
		a.Load(rC, rCtx, ncRT)
		a.Load(rC, rC, rtRel(0))
		g.addMem(rC, rCtx, ncHas)
		a.Load(rC, rC, 0)
		a.Cbz(rC, g.exit, true)
		// The set: ((key>>4) ^ (layout>>4)) * hasCacheMul, its top bits;
		// rW, the instruction, read again after.
		a.ShiftImm(Lsr, rD, r8, 4, true)
		a.ShiftImm(Lsr, rW, r9, 4, true)
		a.Op(Eor, rD, rD, rW, true)
		a.MovImm(rW, hasCacheMul)
		a.Mul(rD, rD, rW)
		a.ShiftImm(Lsr, rD, rD, 64-hasCacheBits+1, true)
		a.MovImm(rW, uint64(2*sizeH))
		a.Mul(rD, rD, rW)
		a.Op(Add, rC, rC, rD, true)
		for i := int32(0); i < 2; i++ {
			next := a.NewLabel()
			e := i * sizeH
			g.cmpMem(r8, rC, e+offHKey)
			a.BCond(NE, next)
			g.cmpMem(r9, rC, e+offHShape)
			a.BCond(NE, next)
			a.Load(rW, rC, e+offHCell)
			a.LoadU8(rW, rW, offCellValid)
			a.Cbz(rW, next, false)
			a.Load(rW, r9, offShapeProto)
			g.cmpMem(rW, rC, e+offHProto)
			a.BCond(NE, next)
			a.LoadU8(rD, rC, e+offHHas)
			a.B(found)
			a.Bind(next)
		}
		a.B(g.exit)
		a.Bind(found)
		store := a.NewLabel()
		a.LoadF(0, rCtx, ncFalse)
		a.Cbz(rD, store, false)
		a.LoadF(0, rCtx, ncTrue)
		a.Bind(store)
		a.Load(rW, rIP, 0)
		g.storeA(0)
	})
	h(bytecode.RGetLength, func() {
		// An array's length but a sparse one's, a string's.
		str, num := a.NewLabel(), a.NewLabel()
		g.regB(rS)
		a.Load(r9, rS, 0)
		a.Load(r8, rS, 8)
		g.cmpMem(r9, rCtx, ncObjBits)
		a.BCond(NE, str)
		a.LoadU8(rC, r8, offObjClass)
		a.CmpImm(rC, int64(ClassArray), false)
		a.BCond(NE, g.exit)
		a.LoadU16(rC, r8, offObjFlags)
		a.AndImm(rC, rC, uint64(objHasSparseElements|objHasExt), false)
		a.CmpImm(rC, int64(objHasExt), false)
		a.BCond(NE, g.exit)
		a.Load(rC, r8, sizeObj+8)
		a.B(num)
		a.Bind(str)
		g.kindIs(r9, KindString)
		a.Load(rC, r8, offStrLen)
		a.Bind(num)
		a.Scvtf(0, rC, true)
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
	// What came later goes last, as on amd64.
	h(bytecode.RPostDec, func() { g.postStep(FSub) })
	h(bytecode.RIterNext, func() {
		// A for-of's step over a dense array by the intrinsic iterator
		// (iterState.arr): its next element but a hole, the cursor's
		// index stepped. Anything else is Go's.
		g.regB(rS)
		g.object(rS, rS)
		a.LoadU8(rC, rS, offObjClass)
		a.CmpImm(rC, int64(ClassCursor), false)
		a.BCond(NE, g.exit)
		a.LoadU8(rC, rS, offItDone)
		a.Cbnz(rC, g.exit, false)
		a.Load(r8, rS, offItArr)
		a.Cbz(r8, g.exit, true)
		a.Load(rC, rS, offItIdx)
		// Past the end of an array of no sparse elements, its length: the
		// cursor done and the loop left, as iterNext leaves it.
		end := a.NewLabel()
		a.LoadU16(rD, r8, offObjFlags)
		a.AndImm(rD, rD, uint64(objHasExt|objHasSparseElements), false)
		a.CmpImm(rD, int64(objHasExt), false)
		a.BCond(NE, g.exit)
		g.cmpMem(rC, r8, sizeObj+8)
		a.BCond(HS, end)
		g.dense(rC, r8, 0, g.exit)
		g.incMem(rS, offItIdx, 1)
		g.loadValue(rC)
		g.regA(rW)
		g.storeValue()
		a.Bind(end)
		a.MovImm(rC, 1)
		a.Store8(rS, offItDone, rC)
		g.immSC(rC)
		g.taken(rC)
	})
	// A call in tail position while the stack is shallow is call_w's or
	// call_method_w's (rhCallWT); deeper, Go's, which gives the frame to
	// the callee in place.
	for _, op := range []bytecode.ROp{bytecode.RCallWT, bytecode.RCallMWT} {
		h(op, func() {
			a.Load(r9, rCtx, ncRT)
			a.Load(rC, r9, offRtFrameDepth)
			a.CmpImm(rC, int64(tailCallDepth), true)
			a.BCond(GE, g.exit)
			a.B(label(bytecode.RCallW))
		})
	}
	h(bytecode.RCharCodeAt, func() {
		// s.charCodeAt(i) of a flat string, the method the built-in read
		// from the realm's cache of String.prototype's properties and i a
		// number, as regCharCodeAt reads it; a rope, the code units of a
		// string not ASCII not made yet, anything else Go's.
		nan, wide, have, idx := a.NewLabel(), a.NewLabel(), a.NewLabel(), a.NewLabel()
		g.regB(rS)
		a.Load(r9, rS, 0)
		g.kindIs(r9, KindString)
		a.Load(rS, rS, 8)
		a.Store(rCtx, scr(sV0), rS)
		a.Load(rD, rCtx, ncRT)
		a.Load(rD, rD, rtRel(0))
		a.Load(r8, rCtx, ncProtoStr)
		a.LoadIdx(r8, rD, r8, false)
		a.Cbz(r8, g.exit, true)
		g.primSlot()
		// The built-in itself, not bound.
		a.Load(r9, rC, 0)
		g.cmpMem(r9, rCtx, ncObjBits)
		a.BCond(NE, g.exit)
		a.Load(r9, rC, 8)
		a.LoadU16(rD, r9, offObjFlags)
		a.TstImm(rD, uint64(objHasData), false)
		a.BCond(EQ, g.exit)
		a.Load(rD, r9, sizeObj+offExtData)
		g.cmpMem(rD, rCtx, ncFuncT)
		a.BCond(NE, g.exit)
		a.Load(r9, r9, sizeObj+offExtData+8)
		a.LoadU8(rD, r9, offFDElemOp)
		a.CmpImm(rD, int64(elemStrCharCodeAt), false)
		a.BCond(NE, g.exit)
		a.LoadU8(rD, r9, offFDBound)
		a.Cbnz(rD, g.exit, false)
		// The position, NaN as 0, truncated: one below the length, or NaN.
		g.regX(rD)
		g.num(0, rD)
		a.FCmp(0, 0)
		a.BCond(VC, idx)
		a.FMovToF(0, ZR)
		a.Bind(idx)
		a.Fcvtzs(rC, 0)
		a.Load(rS, rCtx, scr(sV0))
		g.cmpMem(rC, rS, offStrLen)
		a.BCond(HS, nan)
		g.cmpMemImm(rS, offStrLeft, 0)
		a.BCond(NE, g.exit)
		a.LoadU8(rD, rS, offStrASCII)
		a.Cbz(rD, wide, false)
		a.Load(rD, rS, offStrS)
		a.Op(Add, rD, rD, rC, true)
		a.LoadU8(rC, rD, 0)
		a.B(have)
		a.Bind(wide)
		a.Load(rD, rS, offStrAux)
		a.Cbz(rD, g.exit, true)
		a.AddLsl(rD, rD, rC, 1)
		a.LoadU16(rC, rD, 0)
		a.Bind(have)
		a.Scvtf(0, rC, true)
		g.storeA(0)
		a.Bind(nan)
		a.LoadF(0, rCtx, ncNaN)
		g.storeA(0)
	})

	for n, l := range g.exits {
		if n == 0 {
			continue
		}
		a.Bind(l)
		g.storeImm(rCtx, scr(15), int64(n))
		a.B(g.exit)
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
		far1 := func(slot int32) {
			a.Load(rJ, rTab, 8*slot)
			a.Br(rJ)
		}
		a.Bind(g.exit)
		far1(slotExit)
		a.Bind(g.exitRet)
		far1(slotExitRet)
		for _, op := range far {
			a.Bind(farLabels[op])
			far1(int32(op))
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
