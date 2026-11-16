package nativegen

import (
	"math"

	. "github.com/go-quickjs/go-quickjs/internal/jit/asm/amd64"
)

// nativeGen emits the handlers of a variant (nativeEmit).
type nativeGen struct {
	a *Asm
	// v is the variant emitted: a handler that differs in one tells it
	// by v.
	v    *variant
	exit Label
	// exitRet is the exit that returns to nativeRun whatever the state:
	// the base frame's return, a call the frame stack has no room for.
	exitRet Label
	// exitAfter is where the exit stub goes on once its Go handler has
	// returned, which a call of Go that keeps the exit's protocol returns
	// to too (light).
	exitAfter Label
	// exits are the call handler's numbered exits (ex), which say in
	// the scratch's last word which check left (QJS_DISPDBG).
	exits []Label
	// genRet is a generator's return from a frame genStep entered
	// (genReturn), where the variant has yield's handler; the exit
	// where not.
	genRet Label
	// genObj is genStep's entry with the generator's object in R8, which
	// a call of next() takes (genNext).
	genObj Label
	// late is code a handler leaves out of line, its rarer paths, laid
	// after the last handler, so that what a later round adds to an
	// early handler moves none of the handlers after it.
	late []func()
}

// ex is the call handler's exit number n.
func (g *nativeGen) ex(n int) Label {
	for len(g.exits) <= n {
		g.exits = append(g.exits, g.a.NewLabel())
	}
	return g.exits[n]
}

func (g *nativeGen) dispatch() {
	a := g.a
	a.Load(RAX, RBX, 0)
	a.MovZX8(RCX, RAX)
	a.JmpIdx(R13, RCX, 8, 0)
}

func (g *nativeGen) next() {
	g.a.OpImm(Add, RBX, 8, true)
	g.dispatch()
}

// decA, decB and decC are the operand's register times 16, its offset in
// the window.
func (g *nativeGen) decA(r Reg) {
	a := g.a
	a.MovRR32(r, RAX)
	a.ShiftImm(Shr, r, 12, false)
	a.OpImm(And, r, 0xFFFF0, false)
}

func (g *nativeGen) decB(r Reg) {
	a := g.a
	a.MovRR(r, RAX)
	a.ShiftImm(Shr, r, 28, true)
	a.OpImm(And, r, 0xFFFF0, false)
}

func (g *nativeGen) decC(r Reg) {
	a := g.a
	a.MovRR(r, RAX)
	a.ShiftImm(Shr, r, 44, true)
	a.OpImm(And, r, 0xFFFF0, false)
}

// num loads the number at the window's offset off into x, or exits.
func (g *nativeGen) num(x XReg, off Reg) {
	a := g.a
	a.LoadIdx(R9, R12, off, 1, 0)
	a.Op(Cmp, R9, R10, true)
	a.Jcc(CondAE, g.exit)
	a.MovQToX(x, R9)
}

// toInt32 makes dst's low half ToInt32 of the number in x, its
// truncation modulo 2^32, where the truncation is an int64; a NaN, an
// infinity or a number past 2^63 exits. RDX is its scratch.
func (g *nativeGen) toInt32(dst Reg, x XReg) {
	a := g.a
	a.Cvttsd2si(dst, x)
	a.MovImm(RDX, 1<<63)
	a.Op(Cmp, dst, RDX, true)
	a.Jcc(CondE, g.exit)
}

// upval puts in dst the address of the value of the frame closure's
// upvalue whose index is in idx, or exits past the closure's upvalues.
func (g *nativeGen) upval(dst, idx Reg) {
	a := g.a
	a.Load(dst, RDI, ncNF)
	a.Load(dst, dst, offFCl)
	a.CmpMem(idx, dst, offClUpvals+8, true)
	a.Jcc(CondAE, g.exit)
	a.Load(dst, dst, offClUpvals)
	a.LoadIdx(dst, dst, idx, 8, 0)
	a.Load(dst, dst, 0)
}

// cmpU is a compare-and-branch of A with upvalue B, as cmp's swapped
// form: the jump taken on cond.
func (g *nativeGen) cmpU(cond Cond) {
	a := g.a
	g.decA(RDX)
	g.num(0, RDX)
	g.decB(RCX)
	a.ShiftImm(Shr, RCX, 4, true)
	g.upval(RSI, RCX)
	a.Load(R9, RSI, 0)
	a.Op(Cmp, R9, R10, true)
	a.Jcc(CondAE, g.exit)
	a.MovQToX(1, R9)
	a.SSEOp(UcomiSD, 1, 0)
	tk := a.NewLabel()
	a.Jcc(cond, tk)
	g.next()
	a.Bind(tk)
	g.immSC(RCX)
	g.taken(RCX)
}

// nullish goes to yes if the word in x is undefined's or null's. RCX is
// its scratch.
func (g *nativeGen) nullish(x Reg, yes Label) {
	a := g.a
	no := a.NewLabel()
	a.MovRR(RCX, x)
	a.ShiftImm(Shr, RCX, 16, true)
	a.ShiftImm(Shl, RCX, 16, true)
	a.Op(Cmp, RCX, R10, true)
	a.Jcc(CondNE, no)
	a.MovZX8(RCX, x)
	a.OpImm(Cmp, RCX, int32(KindNull), false)
	a.Jcc(CondE, yes)
	a.OpImm(Cmp, RCX, int32(KindUndefined), false)
	a.Jcc(CondE, yes)
	a.Bind(no)
}

// looseEq is == and != (jeq, jne): two numbers; two values of one word
// kind, equal only if their words are; undefined or null and anything,
// equal only to undefined, null or document.all's kind, which exits; any
// other pair, which converts, exits.
func (g *nativeGen) looseEq(jumpIfEq bool) {
	a := g.a
	eq, ne, words, aNull, bNull, same := a.NewLabel(), a.NewLabel(), a.NewLabel(), a.NewLabel(), a.NewLabel(), a.NewLabel()
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
	g.nullish(R9, aNull)
	g.nullish(R8, bNull)
	// Neither: one word kind, the same word.
	a.Op(Cmp, R9, R8, true)
	a.Jcc(CondNE, g.exit)
	a.MovRR(RCX, R9)
	a.Op(Sub, RCX, R10, true)
	a.MovImm(RAX, heapBigBits-tagBase)
	a.Op(Cmp, RCX, RAX, true)
	a.Jcc(CondAE, g.exit)
	a.MovZX8(RCX, R9)
	a.MovImm(RAX, 1<<KindBool|1<<KindSymbol|1<<KindObject)
	a.Bt(RAX, RCX)
	a.Jcc(CondAE, g.exit)
	a.Bind(same)
	a.LoadIdx(R9, R12, RDX, 1, 8)
	a.LoadIdx(R8, R12, RSI, 1, 8)
	a.Op(Cmp, R9, R8, true)
	a.Jcc(CondNE, ne)
	a.Jmp(eq)
	// undefined or null against the other: equal if it is either, or
	// document.all's kind; not equal to anything else.
	for _, x := range []struct {
		l          Label
		other, off Reg
	}{{aNull, R8, RSI}, {bNull, R9, RDX}} {
		a.Bind(x.l)
		g.nullish(x.other, eq)
		a.CmpMem(x.other, RDI, ncObjBits, true)
		a.Jcc(CondNE, ne)
		a.LoadIdx(RCX, R12, x.off, 1, 8)
		a.LoadU16(RCX, RCX, offObjFlags)
		a.OpImm(And, RCX, int32(objHTMLDDA), false)
		a.Jcc(CondNE, g.exit)
		a.Jmp(ne)
	}
	a.Bind(eq)
	a.Load(RAX, RBX, 0)
	if jumpIfEq {
		g.immSC(RCX)
		g.taken(RCX)
	} else {
		g.next()
	}
	a.Bind(ne)
	a.Load(RAX, RBX, 0)
	if jumpIfEq {
		g.next()
	} else {
		g.immSC(RCX)
		g.taken(RCX)
	}
}

// int32 makes dst the int32 the number in x is exactly, or exits.
func (g *nativeGen) int32(dst Reg, x XReg) {
	a := g.a
	a.Cvttsd2si(dst, x)
	a.Cvtsi2sd(3, dst, false)
	a.SSEOp(UcomiSD, x, 3)
	a.Jcc(CondNE, g.exit)
	a.Jcc(CondP, g.exit)
}

// noRef exits unless the window's Value at off holds no pointer.
func (g *nativeGen) noRef(off Reg) {
	g.a.CmpIdxImm8(R12, off, 1, 8, 0)
	g.a.Jcc(CondNE, g.exit)
}

// canon makes a NaN in x the canonical one.
func (g *nativeGen) canon(x XReg) {
	a := g.a
	ok := a.NewLabel()
	a.SSEOp(UcomiSD, x, x)
	a.Jcc(CondNP, ok)
	a.LoadSD(x, RDI, ncNaN)
	a.Bind(ok)
}

// storeA stores the number in x to register A, its pointer word cleared
// by ptrStore where it held one, and goes on.
func (g *nativeGen) storeA(x XReg) {
	a := g.a
	done := a.NewLabel()
	g.decA(RDX)
	a.StoreSDIdx(R12, RDX, 1, 0, x)
	a.CmpIdxImm8(R12, RDX, 1, 8, 0)
	a.Jcc(CondE, done)
	// The register held a pointer: its word cleared as any pointer store.
	a.LeaIdx(RAX, R12, RDX, 1, 8)
	a.Op(Xor, RCX, RCX, false)
	g.ptrStore()
	a.Bind(done)
	g.next()
}

// storeB is storeA's of register B.
func (g *nativeGen) storeB(x XReg) {
	a := g.a
	done := a.NewLabel()
	g.decB(RDX)
	a.StoreSDIdx(R12, RDX, 1, 0, x)
	a.CmpIdxImm8(R12, RDX, 1, 8, 0)
	a.Jcc(CondE, done)
	a.LeaIdx(RAX, R12, RDX, 1, 8)
	a.Op(Xor, RCX, RCX, false)
	g.ptrStore()
	a.Bind(done)
	g.next()
}

// storeValue stores the Value whose words are R9 (num) and R8 (ref) at
// the address in RAX and goes on: the pointer word plainly while the
// collector does not mark, and by the JIT's storeRef routine while it
// does, which keeps DI alone -- the pinned registers are put back from
// the context after it. The same work marking or not but the barrier's.
func (g *nativeGen) storeValue() {
	a := g.a
	barrier, cont := a.NewLabel(), a.NewLabel()
	a.Store(RAX, 0, R9)
	a.Load(RSI, RDI, ncWB)
	a.LoadU8(RSI, RSI, 0)
	a.Op(Test, RSI, RSI, false)
	a.Jcc(CondNE, barrier)
	a.Store(RAX, 8, R8)
	g.next()
	a.Bind(barrier)
	a.Store(RDI, ncIP, RBX)
	a.Store(RDI, ncBudget, R11)
	a.OpImm(Add, RAX, 8, true)
	a.MovRR(RCX, R8)
	a.LeaLabel(R11, cont)
	a.JmpMem(RDI, ncStore)
	a.Bind(cont)
	a.Load(RBX, RDI, ncIP)
	a.Load(R12, RDI, ncRegs)
	a.Load(R13, RDI, ncTable)
	a.Load(R15, RDI, ncIC)
	a.Load(R11, RDI, ncBudget)
	a.Load(R10, RDI, ncTagBase)
	g.next()
}

// loadValue puts the Value at [base] in R9 (num) and R8 (ref).
func (g *nativeGen) loadValue(base Reg) {
	g.a.Load(R9, base, 0)
	g.a.Load(R8, base, 8)
}

// immSB and immSC put the signed 16-bit operand B or C in r.
func (g *nativeGen) immSB(r Reg) {
	a := g.a
	a.MovRR(r, RAX)
	a.ShiftImm(Shl, r, 16, true)
	a.ShiftImm(Sar, r, 48, true)
}

func (g *nativeGen) immSC(r Reg) {
	a := g.a
	a.MovRR(r, RAX)
	a.ShiftImm(Sar, r, 48, true)
}

// taken jumps by the instruction count in off, counting a backward one
// against the budget, whose end Go's handler meets.
func (g *nativeGen) taken(off Reg) {
	a := g.a
	fwd := a.NewLabel()
	a.Op(Test, off, off, true)
	a.Jcc(CondNS, fwd)
	a.OpImm(Cmp, R11, 1, true)
	a.Jcc(CondLE, g.exit)
	a.OpImm(Sub, R11, 1, true)
	a.Bind(fwd)
	a.LeaIdx(RBX, RBX, off, 8, 8)
	g.dispatch()
}

// cmp is a compare-and-branch: A with B, or with the immediate SB; swap
// compares B with A, and the jump is taken on cond (and on an unordered
// pair if nan).
func (g *nativeGen) cmp(imm, swap bool, cond Cond, nan bool) {
	a := g.a
	// Two registers' BigInts held in their Values are compared as
	// integers (bigCmp).
	big := g.exit
	if !imm {
		big = a.NewLabel()
	}
	g.decA(RDX)
	g.numOr(0, RDX, big)
	if imm {
		g.immSB(RCX)
		a.Cvtsi2sd(1, RCX, true)
	} else {
		g.decB(RSI)
		g.numOr(1, RSI, big)
	}
	if swap {
		a.SSEOp(UcomiSD, 1, 0)
	} else {
		a.SSEOp(UcomiSD, 0, 1)
	}
	tk := a.NewLabel()
	if nan {
		a.Jcc(CondP, tk)
	}
	a.Jcc(cond, tk)
	g.next()
	a.Bind(tk)
	g.immSC(RCX)
	g.taken(RCX)
	if big != g.exit {
		a.Bind(big)
		g.bigCmp(swap)
		a.Jcc(cond, tk)
		g.next()
	}
}

// bigCmp sets the flags as an unsigned compare of A with B (of B with A
// where swap) would for two BigInts held in their Values, which it
// compares as 128-bit integers; anything else exits. RAX is read again
// after.
func (g *nativeGen) bigCmp(swap bool) {
	a := g.a
	hiSame, done := a.NewLabel(), a.NewLabel()
	g.decA(R9)
	g.bigShort(R9, R8, R9, true)
	g.decB(RSI)
	g.bigShort(RSI, RAX, RSI, true)
	x0, x1, y0, y1 := R9, R8, RSI, RAX
	if swap {
		x0, x1, y0, y1 = RSI, RAX, R9, R8
	}
	a.Op(Cmp, x1, y1, true)
	a.Jcc(CondE, hiSame)
	// The high words differ: compared signed, as unsigned with the sign
	// bit flipped.
	a.MovImm(RCX, 1<<63)
	a.Op(Xor, x1, RCX, true)
	a.Op(Xor, y1, RCX, true)
	a.Op(Cmp, x1, y1, true)
	a.Jmp(done)
	a.Bind(hiSame)
	a.Op(Cmp, x0, y0, true)
	a.Bind(done)
	a.Load(RAX, RBX, 0)
}

// arith is R[A] = R[B] op R[C], or op an immediate SC, or op 1.
func (g *nativeGen) arith(op SSE, operand int) {
	a := g.a
	// Two registers' BigInts of the int64 range are added, subtracted
	// and multiplied here too (bigArith).
	big := g.exit
	if operand == 0 && (op == AddSD || op == SubSD || op == MulSD) {
		big = a.NewLabel()
	}
	g.decB(RSI)
	g.numMem(0, RSI, big)
	switch operand {
	case 0:
		g.decC(R8)
		g.numMem(1, R8, big)
	case 1:
		g.immSC(RCX)
		a.Cvtsi2sd(1, RCX, true)
	case 2:
		a.LoadSD(1, RDI, ncOne)
	}
	a.SSEOp(op, 0, 1)
	g.canon(0)
	g.storeA(0)
	if big != g.exit {
		a.Bind(big)
		g.bigArith(op)
	}
}

// exitStub is the exit, where the instruction at RBX is left to Go: its
// Go handler called, by nativeCallH, in the top frame's state where Go has
// made it (topOK), and where it gives back ^pc, its slow function; the
// code goes on at the pc they give, or, where the frame has ended
// (regDone), returns to nativeRun with ended set. Where Go has not made
// the top frame's state -- the first exit after a call or a return the
// code made itself -- it returns to nativeRun, which makes it and runs
// the instruction. The instruction, its index and the budget are written
// to the context either way; the budget is the runtime's backEdges across
// the call.
func (g *nativeGen) exitStub() {
	a := g.a
	slow, afterSlow, back, end, noCount := a.NewLabel(), a.NewLabel(), a.NewLabel(), a.NewLabel(), a.NewLabel()
	a.Align(16)
	a.Bind(g.exit)
	a.Store(RDI, ncIP, RBX)
	a.Store(RDI, ncBudget, R11)
	a.Load(RAX, RBX, 0)
	a.Store(RDI, ncW, RAX)
	a.MovRR(RCX, RBX)
	a.Load(RDX, RDI, ncCodeBase)
	a.Op(Sub, RCX, RDX, true)
	a.ShiftImm(Shr, RCX, 3, true)
	a.Store(RDI, ncPC, RCX)
	ready := a.NewLabel()
	a.CmpMemImm8(RDI, ncTopOK, 0)
	a.Jcc(CondNE, ready)
	g.makeTop(slow)
	a.Load(RAX, RDI, ncW)
	a.Load(RCX, RDI, ncPC)
	a.Bind(ready)
	// Counted where a test counts.
	a.Load(RDX, RDI, ncCntExits)
	a.Op(Test, RDX, RDX, true)
	a.Jcc(CondE, noCount)
	a.MovZX8(R8, RAX)
	a.LeaIdx(RDX, RDX, R8, 8, 0)
	a.LockIncMem(RDX, 0)
	a.Bind(noCount)
	a.Load(RDX, RDI, ncRT)
	a.Store(RDX, offRtBackEdges, R11)
	a.LeaLabel(R8, g.exitAfter)
	a.Store(RDI, ncAfter, R8)
	// handler(st.regs, w, pc, st): the func value in DX.
	a.MovRR(RBX, RAX)
	a.MovZX8(RDX, RAX)
	a.Load(R8, RDI, ncGoTable)
	a.LoadIdx(RDX, R8, RDX, 8, 0)
	// A call's callee other than its site cached: nativeCallMiss, which
	// the call handler asks for (ncMiss).
	have := a.NewLabel()
	a.CmpMemImm8(RDI, ncMiss, 0)
	a.Jcc(CondE, have)
	a.StoreImm(RDI, ncMiss, 0)
	a.Load(RDX, RDI, ncCallMissFn)
	a.Bind(have)
	a.Load(RSI, RDI, ncTop)
	a.Load(RAX, RSI, offStRegs)
	a.JmpMem(RDI, ncCallH)
	a.Bind(g.exitAfter)
	a.Op(Test, RAX, RAX, true)
	a.Jcc(CondNS, back)
	a.MovImm(RCX, uint64(1)<<63)
	a.Op(Cmp, RAX, RCX, true)
	a.Jcc(CondE, end)
	// ^pc: the slow function of the instruction at pc.
	a.LeaLabel(R8, afterSlow)
	a.Store(RDI, ncAfter, R8)
	a.Op(Xor, RCX, RCX, true)
	a.Op(Sub, RCX, RAX, true)
	a.OpImm(Sub, RCX, 1, true)
	a.Load(R8, RDI, ncCodeBase)
	a.LoadIdx(RBX, R8, RCX, 8, 0)
	a.MovZX8(RDX, RBX)
	a.Load(R8, RDI, ncSlowTable)
	a.LoadIdx(RDX, R8, RDX, 8, 0)
	a.Load(RSI, RDI, ncTop)
	a.Load(RAX, RSI, offStRegs)
	a.JmpMem(RDI, ncCallH)
	a.Bind(afterSlow)
	a.Op(Test, RAX, RAX, true)
	a.Jcc(CondS, end)
	// On at pc, the pinned registers put back, the budget the runtime's.
	a.Bind(back)
	a.Load(RDX, RDI, ncRT)
	a.Load(R11, RDX, offRtBackEdges)
	a.Load(RBX, RDI, ncCodeBase)
	a.LeaIdx(RBX, RBX, RAX, 8, 0)
	a.Load(R12, RDI, ncRegs)
	a.Load(R13, RDI, ncTable)
	a.Load(R15, RDI, ncIC)
	a.Load(R10, RDI, ncTagBase)
	g.dispatch()
	// The frame ended: nativeRun ends it, the budget the runtime's.
	a.Bind(end)
	a.StoreImm(RDI, ncEnded, 1)
	a.Load(RDX, RDI, ncRT)
	a.Load(R11, RDX, offRtBackEdges)
	a.Store(RDI, ncBudget, R11)
	a.Bind(slow)
	a.Ret()
	// exitRet: the instruction, its index and the budget, and back to
	// nativeRun.
	a.Align(16)
	a.Bind(g.exitRet)
	a.Store(RDI, ncIP, RBX)
	a.Store(RDI, ncBudget, R11)
	a.Load(RAX, RBX, 0)
	a.Store(RDI, ncW, RAX)
	a.MovRR(RCX, RBX)
	a.Load(RDX, RDI, ncCodeBase)
	a.Op(Sub, RCX, RDX, true)
	a.ShiftImm(Shr, RCX, 3, true)
	a.Store(RDI, ncPC, RCX)
	a.Ret()
}

// primMethod is get_method_w of a string's or a number's method: its
// prototype's data property that the realm's cache of them remembers
// (primProps), as regGetMethod reads it; R[A] the method, R[A+1] the
// receiver. The receiver's words are in R9 and at the window's offset in
// RSI; anything else exits.
func (g *nativeGen) primMethod() {
	a := g.a
	num, have := a.NewLabel(), a.NewLabel()
	a.Store(RDI, scr(sV0), R9)
	a.LoadIdx(R8, R12, RSI, 1, 8)
	a.Store(RDI, scr(sV1), R8)
	// The realm running now, the Runtime's first word.
	a.Load(RDX, RDI, ncRT)
	a.Load(RDX, RDX, rtRel(0))
	a.Op(Cmp, R9, R10, true)
	a.Jcc(CondB, num)
	a.MovRR(RCX, R9)
	a.Op(Xor, RCX, R10, true)
	a.ShiftImm(Shr, RCX, 16, true)
	a.Jcc(CondNE, g.exit)
	a.MovZX8(RCX, R9)
	a.OpImm(Cmp, RCX, int32(KindString), false)
	a.Jcc(CondNE, g.exit)
	a.Load(R8, RDI, ncProtoStr)
	a.Jmp(have)
	a.Bind(num)
	a.Load(R8, RDI, ncProtoNum)
	a.Bind(have)
	a.LoadIdx(R8, RDX, R8, 1, 0)
	a.Op(Test, R8, R8, true)
	a.Jcc(CondE, g.exit)
	g.primSlot()
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
	a.Load(R9, RDI, scr(sV0))
	a.Store(RAX, 0, R9)
	a.Lea(RAX, RAX, 8)
	a.Load(RCX, RDI, scr(sV1))
	g.ptrStore()
	g.next()
}

// makeTop makes the top frame's state for Go, as nativeTop makes it --
// the base frame's its own; a frame the code entered itself the state Go
// made for its depth before, its frame, code, closure and window written,
// and the callers' pcs (nativeFillPCs) -- or goes to slow where Go must:
// a depth Go has made no state for yet, one that holds an error.
func (g *nativeGen) makeTop(slow Label) {
	a := g.a
	deep, fill, filled, set := a.NewLabel(), a.NewLabel(), a.NewLabel(), a.NewLabel()
	a.Load(R8, RDI, ncDepth)
	a.Op(Test, R8, R8, true)
	a.Jcc(CondNE, deep)
	a.Load(RCX, RDI, ncSt)
	a.Jmp(set)
	a.Bind(deep)
	// The state Go made for the depth, holding no error.
	a.Load(RSI, RDI, ncStates)
	a.LoadIdx(RSI, RSI, R8, 8, -8)
	a.Op(Test, RSI, RSI, true)
	a.Jcc(CondE, slow)
	a.CmpMemImm8(RSI, offStErr, 0)
	a.Jcc(CondNE, slow)
	// The callers' pcs: frames[filled:depth].
	a.Load(R9, RDI, ncFilled)
	a.Bind(fill)
	a.Op(Cmp, R9, R8, true)
	a.Jcc(CondGE, filled)
	a.MovImm(RCX, uint64(recSize))
	a.MovRR(RDX, R9)
	a.Imul(RDX, RCX)
	a.AddMem(RDX, RDI, ncFrames)
	a.Load(RCX, RDX, recRetIP)
	a.Load(RAX, RDX, recCodeBaseOff)
	a.Op(Sub, RCX, RAX, true)
	a.ShiftImm(Shr, RCX, 1, true)
	a.Load(RAX, RDX, recRCOff)
	a.Load(RAX, RAX, offRCSrc)
	a.Op(Add, RAX, RCX, true)
	a.LoadU32(RCX, RAX, 0)
	a.OpImm(Add, RCX, 1, false)
	a.Load(RAX, RDX, recNFOff)
	a.Store32(RAX, offFPC, RCX)
	a.OpImm(Add, R9, 1, true)
	a.Jmp(fill)
	a.Bind(filled)
	a.Store(RDI, ncFilled, R8)
	// The depth among those the run has used (used), which its end
	// clears.
	used := a.NewLabel()
	a.CmpMem(R8, RDI, ncUsed, true)
	a.Jcc(CondLE, used)
	a.Store(RDI, ncUsed, R8)
	a.Bind(used)
	// Its frame, code, closure and window, each where it differs, and its
	// window's base.
	a.Store(RDI, scr(sV0), RSI)
	a.Load(RCX, RDI, ncNF)
	a.Load(RDX, RCX, offFBase)
	a.Load(RAX, RDI, ncRC)
	a.LoadS32(RAX, RAX, offRCLocals)
	a.Op(Sub, RDX, RAX, true)
	a.Store(RSI, offStWb, RDX)
	for _, f := range [][2]int32{{ncNF, offStF}, {ncRC, offStC}, {-2, offStRegs}, {-1, offStCl}} {
		a.Load(RSI, RDI, scr(sV0))
		switch f[0] {
		case -1:
			a.Load(RCX, RDI, ncNF)
			a.Load(RCX, RCX, offFCl)
		case -2:
			// regWindow(r.stack, wb).
			a.Load(RCX, RSI, offStWb)
			a.ShiftImm(Shl, RCX, 4, true)
			a.Load(RDX, RDI, ncRT)
			a.AddMem(RCX, RDX, offRtStack)
		default:
			a.Load(RCX, RDI, f[0])
		}
		a.Lea(RAX, RSI, f[1])
		g.ptrTo()
	}
	a.Load(RCX, RDI, scr(sV0))
	a.Bind(set)
	a.Lea(RAX, RDI, ncTop)
	g.ptrTo()
	a.StoreImm(RDI, ncTopOK, 1)
}

// primSlot puts in RCX the address of the data property of the prototype
// in R8 (of the realm in RDX) that the realm's cache of primitives'
// properties (primProps) remembers for the name of site C, or exits. R9
// and RSI are its scratch.
func (g *nativeGen) primSlot() {
	a := g.a
	// The name: names[siteName[C]].
	a.MovRR(RCX, RAX)
	a.ShiftImm(Shr, RCX, 48, true)
	a.Load(R9, RDI, ncRC)
	a.Load(R9, R9, offRCSiteName)
	a.LeaIdx(R9, R9, RCX, 4, 0)
	a.LoadU32(RCX, R9, 0)
	a.Load(R9, RDI, ncCC)
	a.Load(R9, R9, offCCNames)
	a.LeaIdx(R9, R9, RCX, 4, 0)
	a.LoadU32(RCX, R9, 0)
	// Its entry, primProps[name&63], of this prototype and its shape.
	a.MovRR(R9, RCX)
	a.OpImm(And, R9, 63, false)
	a.LeaIdx(R9, R9, R9, 2, 0)
	a.ShiftImm(Shl, R9, 3, true)
	a.Op(Add, R9, RDX, true)
	a.AddMem(R9, RDI, ncPrimProps)
	a.LoadU32(RSI, R9, offPEKey)
	a.Op(Cmp, RSI, RCX, false)
	a.Jcc(CondNE, g.exit)
	a.CmpMem(R8, R9, offPEProto, true)
	a.Jcc(CondNE, g.exit)
	a.Load(RSI, R9, offPEShape)
	a.CmpMem(RSI, R8, offObjShape, true)
	a.Jcc(CondNE, g.exit)
	a.LoadS32(RCX, R9, offPEIdx)
	a.ShiftImm(Shl, RCX, 4, true)
	a.AddMem(RCX, R8, off("Object.valp"))
}

// postStep is x++ and x-- (post_inc, post_dec) of a number: R[A] the
// old value where it holds no pointer, R[B] the new.
func (g *nativeGen) postStep(op SSE) {
	a := g.a
	g.decB(RSI)
	g.num(0, RSI)
	g.decA(RDX)
	g.noRef(RDX)
	a.LoadSD(1, RDI, ncOne)
	a.SSEOp(MovAPD, 3, 0)
	a.SSEOp(op, 3, 1)
	g.canon(3)
	a.StoreSDIdx(R12, RDX, 1, 0, 0)
	a.StoreSDIdx(R12, RSI, 1, 0, 3)
	g.next()
}

// numOr is num going to miss rather than exiting.
func (g *nativeGen) numOr(x XReg, off Reg, miss Label) {
	a := g.a
	a.LoadIdx(R9, R12, off, 1, 0)
	a.Op(Cmp, R9, R10, true)
	a.Jcc(CondAE, miss)
	a.MovQToX(x, R9)
}

// numMem is numOr for an operand of arithmetic, whose answer the next
// instruction often reads: x is loaded from memory, not moved from R9,
// so that a chain of operations through a register waits on the store's
// forwarding alone, not on a move between the register files too. R9
// holds the operand's address, not its bits.
func (g *nativeGen) numMem(x XReg, off Reg, miss Label) {
	a := g.a
	a.LeaIdx(R9, R12, off, 1, 0)
	a.CmpMem(R10, R9, 0, true)
	a.Jcc(CondBE, miss)
	a.LoadSD(x, R9, 0)
}

// bigShort puts in lo and hi, a 128-bit two's complement integer, the
// BigInt the window's Value at off holds in itself: one of the int64 range
// (shortBig), and where wide, one past it, below 2^64 in magnitude
// (shortBigMag); else it exits. RDX and RCX are its scratch; off may be
// lo.
func (g *nativeGen) bigShort(lo, hi, off Reg, wide bool) {
	a := g.a
	notInt, done := a.NewLabel(), a.NewLabel()
	a.LoadIdx(RDX, R12, off, 1, 8)
	a.LoadIdx(lo, R12, off, 1, 0)
	a.MovImm(RCX, shortIntBase)
	a.Op(Cmp, lo, RCX, true)
	a.Jcc(CondB, notInt)
	// The low 49 bits from num, the high 15 the byte of shortBigHigh
	// that ref points at.
	a.ShiftImm(Shl, lo, 64-shortBigLowBits, true)
	a.ShiftImm(Shr, lo, 64-shortBigLowBits, true)
	a.Load(RCX, RDI, ncBigHigh)
	a.Op(Sub, RDX, RCX, true)
	a.ShiftImm(Shl, RDX, shortBigLowBits, true)
	a.Op(Or, lo, RDX, true)
	a.MovRR(hi, lo)
	a.ShiftImm(Sar, hi, 63, true)
	a.Jmp(done)
	a.Bind(notInt)
	if !wide {
		a.Jmp(g.exit)
	} else {
		// One past the int64 range: the low 49 bits of its 65 from num,
		// the next 15 and the 65th the byte of shortBigHighX that ref
		// points at.
		a.MovImm(RCX, shortBigBase)
		a.Op(Cmp, lo, RCX, true)
		a.Jcc(CondB, g.exit)
		a.ShiftImm(Shl, lo, 64-shortBigLowBits, true)
		a.ShiftImm(Shr, lo, 64-shortBigLowBits, true)
		a.Load(RCX, RDI, ncBigHighX)
		a.Op(Sub, RDX, RCX, true)
		a.MovRR(hi, RDX)
		a.ShiftImm(Shr, hi, 64-shortBigLowBits, true)
		a.ShiftImm(Shl, hi, 63, true)
		a.ShiftImm(Sar, hi, 63, true)
		a.OpImm(And, RDX, 1<<(64-shortBigLowBits)-1, true)
		a.ShiftImm(Shl, RDX, shortBigLowBits, true)
		a.Op(Or, lo, RDX, true)
	}
	a.Bind(done)
}

// bigArith is R[A] = R[B] op R[C] of two BigInts held in their Values
// whose answer is one too, held as shortBigMag holds it: a sum or a
// difference of any two, a product of two of the int64 range. Anything
// else exits. The ref is a pointer outside Go's heap, stored as any.
func (g *nativeGen) bigArith(op SSE) {
	a := g.a
	wide := op != MulSD
	g.decB(RSI)
	g.bigShort(R9, R8, RSI, wide)
	g.decC(RSI)
	g.bigShort(RSI, RAX, RSI, wide)
	switch op {
	case AddSD:
		a.Op(Add, R9, RSI, true)
		a.Op(Adc, R8, RAX, true)
	case SubSD:
		a.Op(Sub, R9, RSI, true)
		a.Op(Sbb, R8, RAX, true)
	case MulSD:
		a.Imul(R9, RSI)
		a.Jcc(CondO, g.exit)
		a.MovRR(R8, R9)
		a.ShiftImm(Sar, R8, 63, true)
	}
	g.bigStoreA()
}

// bigStoreA stores the 128-bit integer R8:R9 to register A as a BigInt held
// in its Value, as shortBigMag holds it: of the int64 range where R8 is
// R9's sign, else one past it where R8 is 0 (2^63 up) or -1 (below -2^63,
// -2^64 not); anything wider exits. RAX is read again first.
func (g *nativeGen) bigStoreA() {
	a := g.a
	small, past, store := a.NewLabel(), a.NewLabel(), a.NewLabel()
	a.MovRR(RDX, R9)
	a.ShiftImm(Sar, RDX, 63, true)
	a.Op(Cmp, RDX, R8, true)
	a.Jcc(CondE, small)
	a.Op(Test, R8, R8, true)
	a.Jcc(CondE, past)
	a.OpImm(Cmp, R8, -1, true)
	a.Jcc(CondNE, g.exit)
	a.Op(Test, R9, R9, true)
	a.Jcc(CondE, g.exit)
	a.MovImm(R8, 1<<(64-shortBigLowBits))
	a.Jmp(store)
	a.Bind(past)
	a.Op(Xor, R8, R8, false)
	a.Bind(store)
	// One past: shortBigHighX's byte at the high 15 bits and the 65th.
	a.MovRR(RDX, R9)
	a.ShiftImm(Shr, RDX, shortBigLowBits, true)
	a.Op(Add, R8, RDX, true)
	a.AddMem(R8, RDI, ncBigHighX)
	a.MovImm(RCX, shortBigBase)
	done := a.NewLabel()
	a.Jmp(done)
	a.Bind(small)
	a.MovRR(R8, R9)
	a.ShiftImm(Shr, R8, shortBigLowBits, true)
	a.AddMem(R8, RDI, ncBigHigh)
	a.MovImm(RCX, shortIntBase)
	a.Bind(done)
	a.ShiftImm(Shl, R9, 64-shortBigLowBits, true)
	a.ShiftImm(Shr, R9, 64-shortBigLowBits, true)
	a.Op(Or, R9, RCX, true)
	a.Load(RAX, RBX, 0)
	g.decA(RDX)
	a.LeaIdx(RAX, R12, RDX, 1, 0)
	g.storeValue()
}

// bitImm is R[A] = int32 R[B] op SC, a shift's count in CL.
func (g *nativeGen) bitImm(emit func(), unsigned ...bool) {
	a := g.a
	g.decB(RSI)
	g.numMem(0, RSI, g.exit)
	g.toInt32(R9, 0)
	g.immSC(RCX)
	emit()
	// A 32-bit operation has cleared R9's upper half: an unsigned
	// result converts as the 64-bit integer it is.
	a.Cvtsi2sd(0, R9, len(unsigned) > 0)
	g.storeA(0)
}

// bitReg is R[A] = int32 R[B] op int32 R[C], R[C] in RCX.
func (g *nativeGen) bitReg(emit func(), unsigned ...bool) {
	a := g.a
	g.decB(RSI)
	g.numMem(0, RSI, g.exit)
	g.decC(R8)
	g.numMem(1, R8, g.exit)
	g.toInt32(RCX, 1)
	g.toInt32(R9, 0)
	emit()
	// A 32-bit operation has cleared R9's upper half: an unsigned
	// result converts as the 64-bit integer it is.
	a.Cvtsi2sd(0, R9, len(unsigned) > 0)
	g.storeA(0)
}

// object puts the object in the window at off in dst, or exits.
func (g *nativeGen) object(dst, off Reg) {
	a := g.a
	a.LoadIdx(R9, R12, off, 1, 0)
	a.CmpMem(R9, RDI, ncObjBits, true)
	a.Jcc(CondNE, g.exit)
	a.LoadIdx(dst, R12, off, 1, 8)
}

// site puts &ic[C] in dst.
func (g *nativeGen) site(dst Reg) {
	a := g.a
	a.MovRR(dst, RAX)
	a.ShiftImm(Shr, dst, 48, true)
	a.MovImm(R8, uint64(size("propCache")))
	a.Imul(dst, R8)
	a.Op(Add, dst, R15, true)
}

// ownSlot puts the address of the own data property the site at RCX
// remembers of the object in obj in dst, or exits.
func (g *nativeGen) ownSlot(dst, obj Reg) {
	a := g.a
	a.Load(R8, RCX, off("propCache.ownShape"))
	a.CmpMem(R8, obj, off("Object.shape"), true)
	a.Jcc(CondNE, g.exit)
	a.LoadS32(dst, RCX, off("propCache.idx"))
	a.ShiftImm(Shl, dst, 4, true)
	a.AddMem(dst, obj, off("Object.valp"))
}

// readSlot puts in RCX the address of the data property the site at RCX
// remembers of the object in RSI: its own, or a prototype's in the holder
// propCache.holder finds, as runReg's property read does; else it exits.
// R8, R9 and RDX are its scratch.
func (g *nativeGen) readSlot() {
	a := g.a
	own, holder, noCell, p1Holder := a.NewLabel(), a.NewLabel(), a.NewLabel(), a.NewLabel()
	poly, polyLoop, polyNext, shaped, done := a.NewLabel(), a.NewLabel(), a.NewLabel(), a.NewLabel(), a.NewLabel()
	shapeOff := off("Object.shape")
	a.Load(R8, RCX, off("propCache.ownShape"))
	a.CmpMem(R8, RSI, shapeOff, true)
	a.Jcc(CondE, own)
	a.Load(R8, RCX, off("propCache.shape"))
	a.CmpMem(R8, RSI, shapeOff, true)
	a.Jcc(CondNE, poly)
	a.Bind(shaped)
	a.LoadU8(R8, RCX, off("propCache.getter"))
	a.Op(Test, R8, R8, false)
	a.Jcc(CondNE, g.exit)
	a.Load(R8, RCX, off("propCache.cell"))
	a.Op(Test, R8, R8, true)
	a.Jcc(CondE, noCell)
	a.LoadU8(R8, R8, off("protoCell.valid"))
	a.Op(Test, R8, R8, false)
	a.Jcc(CondE, g.exit)
	a.Load(RSI, RCX, off("propCache.p2"))
	a.Op(Test, RSI, RSI, true)
	a.Jcc(CondE, g.exit)
	a.Jmp(holder)
	a.Bind(noCell)
	a.Load(R8, RCX, off("propCache.p1"))
	a.Op(Test, R8, R8, true)
	a.Jcc(CondE, holder) // the object itself
	a.Load(R9, R8, shapeOff)
	a.CmpMem(R9, RCX, off("propCache.s1"), true)
	a.Jcc(CondNE, g.exit)
	a.Load(R9, RCX, off("propCache.p2"))
	a.Op(Test, R9, R9, true)
	a.Jcc(CondE, p1Holder)
	a.Load(RDX, R9, shapeOff)
	a.CmpMem(RDX, RCX, off("propCache.s2"), true)
	a.Jcc(CondNE, g.exit)
	a.MovRR(RSI, R9)
	a.Jmp(holder)
	a.Bind(p1Holder)
	a.MovRR(RSI, R8)
	a.Bind(own)
	a.Bind(holder)
	a.LoadS32(R8, RCX, off("propCache.idx"))
	a.ShiftImm(Shl, R8, 4, true)
	a.AddMem(R8, RSI, off("Object.valp"))
	a.MovRR(RCX, R8)
	a.Jmp(done)
	// A site of several shapes: the entries it remembered before, an own
	// data property of the object's shape among them.
	a.Bind(poly)
	a.Load(R8, RCX, off("propCache.poly"))
	a.Op(Test, R8, R8, true)
	a.Jcc(CondE, g.exit)
	a.LoadU8(R9, R8, off("propPoly.n"))
	a.Op(Test, R9, R9, false)
	a.Jcc(CondE, g.exit)
	a.AddMem(R8, RDI, ncPolyEnt)
	a.Load(RDX, RSI, shapeOff)
	a.Bind(polyLoop)
	a.CmpMem(RDX, R8, off("propCache.ownShape"), true)
	a.MovRR(RCX, R8)
	a.Jcc(CondE, own)
	// An entry of the shape for a prototype's property: its holder found
	// as the site's own entry's is.
	a.CmpMem(RDX, R8, off("propCache.shape"), true)
	a.Jcc(CondNE, polyNext)
	a.Jmp(shaped)
	a.Bind(polyNext)
	a.OpImm(Add, R8, size("propCache"), true)
	a.OpImm(Sub, R9, 1, false)
	a.Jcc(CondNE, polyLoop)
	a.Jmp(g.exit)
	a.Bind(done)
}

// element puts the address of the dense element of the object in obj at
// the number key in x in dst, or exits; mask is the flags that must be
// clear but objHasExt. An index the object has no dense element at goes to
// miss with the index in dst (exit where miss is g.exit).
func (g *nativeGen) element(dst, obj Reg, x XReg, mask objFlags, miss Label) {
	a := g.a
	a.Cvttsd2si(dst, x)
	a.MovRR32(RDX, dst)
	a.Op(Cmp, RDX, dst, true)
	a.Jcc(CondNE, g.exit)
	a.Cvtsi2sd(3, dst, true)
	a.SSEOp(UcomiSD, x, 3)
	a.Jcc(CondNE, g.exit)
	a.Jcc(CondP, g.exit)
	g.dense(dst, obj, mask, miss)
}

// dense makes dst, an index below 2^32, the address of the dense element
// of the object in obj there, or goes to miss with dst as it was; mask is
// the flags that must be clear but objHasExt. A hole exits.
func (g *nativeGen) dense(dst, obj Reg, mask objFlags, miss Label) {
	a := g.a
	a.LoadU16(RDX, obj, offObjFlags)
	a.OpImm(And, RDX, int32(mask|objHasExt), false)
	a.OpImm(Cmp, RDX, int32(objHasExt), false)
	a.Jcc(CondNE, miss)
	a.CmpMem(dst, obj, sizeObj+8, true)
	a.Jcc(CondAE, miss)
	a.ShiftImm(Shl, dst, 4, true)
	a.AddMem(dst, obj, sizeObj)
	// No hole.
	a.Load(RDX, dst, 0)
	a.CmpMem(RDX, RDI, ncHole, true)
	a.Jcc(CondE, g.exit)
}

// typedView puts in R8 the typed array data (*typedArrayData) of the
// object in obj, a view good as of the runtime's typedEpoch with an
// element at the index in idx, or exits. RDX is its scratch.
func (g *nativeGen) typedView(obj, idx Reg) {
	a := g.a
	a.LoadU8(RDX, obj, offObjClass)
	a.OpImm(Cmp, RDX, int32(ClassTypedArray), false)
	a.Jcc(CondNE, g.exit)
	a.LoadU16(RDX, obj, offObjFlags)
	a.OpImm(And, RDX, int32(objHasData), false)
	a.Jcc(CondE, g.exit)
	a.Load(R8, obj, sizeObj+offExtData)
	a.CmpMem(R8, RDI, ncTypedT, true)
	a.Jcc(CondNE, g.exit)
	a.Load(R8, obj, sizeObj+offExtData+8)
	a.Load(RDX, RDI, ncTypedEp)
	a.Load(RDX, RDX, 0)
	a.CmpMem(RDX, R8, offTEpoch, true)
	a.Jcc(CondNE, g.exit)
	a.CmpMem(idx, R8, offTLength, true)
	a.Jcc(CondAE, g.exit)
}

// typedRead puts in X0 the element at the index in idx of the typed array
// in obj, a number of any kind but a float16 or a BigInt, as runReg's
// typed tail reads it; else it exits. R8, R9 and RDX are its scratch.
func (g *nativeGen) typedRead(obj, idx Reg, big *Label) {
	a := g.a
	g.typedView(obj, idx)
	a.LoadU8(RDX, R8, offTKind)
	a.Load(R8, R8, offTData)
	done := a.NewLabel()
	type load struct {
		kinds []elemType
		scale uint8
		emit  func()
	}
	for _, l := range []load{
		{[]elemType{elemFloat64}, 8, func() { a.LoadSD(0, R8, 0); g.canon(0) }},
		{[]elemType{elemInt32}, 4, func() { a.LoadS32(R9, R8, 0); a.Cvtsi2sd(0, R9, true) }},
		{[]elemType{elemUint8, elemUint8Clamped}, 1, func() { a.LoadU8(R9, R8, 0); a.Cvtsi2sd(0, R9, true) }},
		{[]elemType{elemFloat32}, 4, func() {
			a.LoadU32(R9, R8, 0)
			a.MovQToX(0, R9)
			a.Cvtss2sd(0, 0)
			g.canon(0)
		}},
		{[]elemType{elemUint32}, 4, func() { a.LoadU32(R9, R8, 0); a.Cvtsi2sd(0, R9, true) }},
		{[]elemType{elemInt16}, 2, func() { a.LoadS16(R9, R8, 0); a.Cvtsi2sd(0, R9, true) }},
		{[]elemType{elemUint16}, 2, func() { a.LoadU16(R9, R8, 0); a.Cvtsi2sd(0, R9, true) }},
		{[]elemType{elemInt8}, 1, func() { a.LoadS8(R9, R8, 0); a.Cvtsi2sd(0, R9, true) }},
	} {
		this, next := a.NewLabel(), a.NewLabel()
		for i, k := range l.kinds {
			a.OpImm(Cmp, RDX, int32(k), false)
			if i == len(l.kinds)-1 {
				a.Jcc(CondNE, next)
			} else {
				a.Jcc(CondE, this)
			}
		}
		a.Bind(this)
		a.LeaIdx(R8, R8, idx, l.scale, 0)
		l.emit()
		a.Jmp(done)
		a.Bind(next)
	}
	if big != nil {
		// A BigInt kind: big, the element's address in R8, the kind in RDX.
		notBig := a.NewLabel()
		a.OpImm(Cmp, RDX, int32(elemBigInt64), false)
		a.Jcc(CondB, notBig)
		a.OpImm(Cmp, RDX, int32(elemBigUint64), false)
		a.Jcc(CondA, notBig)
		a.LeaIdx(R8, R8, idx, 8, 0)
		a.Jmp(*big)
		a.Bind(notBig)
	}
	a.Jmp(g.exit)
	a.Bind(done)
}

// typedBigRead stores the element at R8 of a BigInt64Array or a
// BigUint64Array, its kind in RDX, to register A, a BigInt held in its
// Value.
func (g *nativeGen) typedBigRead() {
	a := g.a
	unsigned, enc := a.NewLabel(), a.NewLabel()
	a.Load(R9, R8, 0)
	a.OpImm(Cmp, RDX, int32(elemBigUint64), false)
	a.Jcc(CondE, unsigned)
	a.MovRR(R8, R9)
	a.ShiftImm(Sar, R8, 63, true)
	a.Jmp(enc)
	a.Bind(unsigned)
	a.Op(Xor, R8, R8, false)
	a.Bind(enc)
	g.bigStoreA()
}

// typedWrite stores the number in X1 as the element at the index in idx
// of the typed array in obj, as regTypedStore does: a float kind's any
// number, an integer kind's but Uint8ClampedArray's ToInt32 of it wrapped
// to its width where it is below 2^63 in magnitude; else it exits. R8, R9
// and RDX are its scratch.
func (g *nativeGen) typedWrite(obj, idx Reg) {
	a := g.a
	g.typedView(obj, idx)
	a.LoadU8(RDX, R8, offTReadonly)
	a.Op(Test, RDX, RDX, false)
	a.Jcc(CondNE, g.exit)
	a.LoadU8(RDX, R8, offTKind)
	a.Load(R8, R8, offTData)
	done := a.NewLabel()
	notF64, notF32 := a.NewLabel(), a.NewLabel()
	a.OpImm(Cmp, RDX, int32(elemFloat64), false)
	a.Jcc(CondNE, notF64)
	a.LeaIdx(R8, R8, idx, 8, 0)
	a.StoreSD(R8, 0, 1)
	a.Jmp(done)
	a.Bind(notF64)
	a.OpImm(Cmp, RDX, int32(elemFloat32), false)
	a.Jcc(CondNE, notF32)
	a.Cvtsd2ss(3, 1)
	a.MovQFromX(R9, 3)
	a.LeaIdx(R8, R8, idx, 4, 0)
	a.Store32(R8, 0, R9)
	a.Jmp(done)
	a.Bind(notF32)
	// An integer kind: the number's int64, which is 2^63's bits where it
	// is none (a NaN, an infinity, 2^63 or more in magnitude).
	a.Cvttsd2si(R9, 1)
	type store struct {
		kinds []elemType
		scale uint8
		emit  func()
	}
	for _, st := range []store{
		{[]elemType{elemInt32, elemUint32}, 4, func() { a.Store32(R8, 0, R9) }},
		{[]elemType{elemUint8, elemInt8}, 1, func() { a.Store8(R8, 0, R9) }},
		{[]elemType{elemInt16, elemUint16}, 2, func() { a.Store16(R8, 0, R9) }},
	} {
		this, next := a.NewLabel(), a.NewLabel()
		a.OpImm(Cmp, RDX, int32(st.kinds[0]), false)
		a.Jcc(CondE, this)
		a.OpImm(Cmp, RDX, int32(st.kinds[1]), false)
		a.Jcc(CondNE, next)
		a.Bind(this)
		// 2^63's bits: no int64 (or exactly -2^63, left to Go too).
		a.MovImm(RDX, 1<<63)
		a.Op(Cmp, R9, RDX, true)
		a.Jcc(CondE, g.exit)
		a.LeaIdx(R8, R8, idx, st.scale, 0)
		st.emit()
		a.Jmp(done)
		a.Bind(next)
	}
	// Uint8ClampedArray's ToUint8Clamp: NaN and what is not above 0 are
	// 0, what is not below 255 255, else the nearest integer, ties to the
	// even one.
	zero, top, put, up := a.NewLabel(), a.NewLabel(), a.NewLabel(), a.NewLabel()
	a.OpImm(Cmp, RDX, int32(elemUint8Clamped), false)
	a.Jcc(CondNE, g.exit)
	a.LeaIdx(R8, R8, idx, 1, 0)
	a.SSEOp(XorPD, 3, 3)
	a.SSEOp(UcomiSD, 1, 3)
	a.Jcc(CondP, zero)
	a.Jcc(CondBE, zero)
	a.MovImm(R9, 255)
	a.Cvtsi2sd(3, R9, true)
	a.SSEOp(UcomiSD, 1, 3)
	a.Jcc(CondAE, top)
	a.Cvttsd2si(R9, 1)
	a.Cvtsi2sd(3, R9, true)
	a.SSEOp(MovAPD, 0, 1)
	a.SSEOp(SubSD, 0, 3)
	a.MovImm(RDX, math.Float64bits(0.5))
	a.MovQToX(3, RDX)
	a.SSEOp(UcomiSD, 0, 3)
	a.Jcc(CondA, up)
	a.Jcc(CondB, put)
	a.MovRR(RDX, R9)
	a.OpImm(And, RDX, 1, false)
	a.Jcc(CondE, put)
	a.Bind(up)
	a.OpImm(Add, R9, 1, false)
	a.Jmp(put)
	a.Bind(zero)
	a.Op(Xor, R9, R9, false)
	a.Jmp(put)
	a.Bind(top)
	a.MovImm(R9, 255)
	a.Bind(put)
	a.Store8(R8, 0, R9)
	a.Bind(done)
}

// arrayAppend is an element write at the index in idx, the length, of the
// array in obj with room for one more, as appendElem appends it: the
// array extensible, its length writable, dense and of no index keys, and
// its prototype chain of ordinary objects and arrays with neither
// elements nor index keys; the value register C's. An object that is no
// array goes to notArray; a full array, anything else exits.
func (g *nativeGen) arrayAppend(obj, idx Reg, notArray Label) {
	a := g.a
	g.appendRoom(obj, idx, notArray, g.exit, false)
	// The value at elems[len], then the length past it.
	a.MovRR(RAX, idx)
	a.ShiftImm(Shl, RAX, 4, true)
	a.AddMem(RAX, obj, sizeObj)
	a.Lea(R9, idx, 1)
	a.Store(obj, sizeObj+8, R9)
	a.Load(RCX, RBX, 0)
	a.ShiftImm(Shr, RCX, 44, true)
	a.OpImm(And, RCX, 0xFFFF0, false)
	a.LoadIdx(R9, R12, RCX, 1, 0)
	a.LoadIdx(R8, R12, RCX, 1, 8)
	g.storeValue()
}

// appendRoom goes to notArray unless the object in obj is an array, and
// to miss unless an element appended at idx, its length, is stored as
// arrayAppend stores one: the array's own, extensible, of a writable
// length, dense, with room past its elements, and no index key on it or
// up its chain of ordinary objects and arrays of no elements. Where atLen
// is set, idx is loaded with the length rather than compared with it.
// RDX, R8 and R9 are its scratch.
func (g *nativeGen) appendRoom(obj, idx Reg, notArray, miss Label, atLen bool) {
	a := g.a
	loop, done := a.NewLabel(), a.NewLabel()
	a.LoadU8(RDX, obj, offObjClass)
	a.OpImm(Cmp, RDX, int32(ClassArray), false)
	a.Jcc(CondNE, notArray)
	want := objExtensible | objArrayLengthWritable | objHasExt
	a.LoadU16(RDX, obj, offObjFlags)
	a.OpImm(And, RDX, int32(want|objHasSparseElements|objMappedArguments|objSharedElems|objPolyProto), false)
	a.OpImm(Cmp, RDX, int32(want), false)
	a.Jcc(CondNE, miss)
	if atLen {
		a.Load(idx, obj, sizeObj+8)
	} else {
		a.CmpMem(idx, obj, sizeObj+8, true)
		a.Jcc(CondNE, miss)
	}
	a.CmpMem(idx, obj, sizeObj+16, true)
	a.Jcc(CondAE, miss)
	g.noIndexKeys(obj)
	// The chain: ordinary objects and arrays of no elements, no index
	// keys, no prototype of their own word.
	a.MovRR(R8, obj)
	a.Bind(loop)
	a.Load(R8, R8, offObjShape)
	a.Op(Test, R8, R8, true)
	a.Jcc(CondE, done)
	a.Load(R8, R8, offShapeProto)
	a.Op(Test, R8, R8, true)
	a.Jcc(CondE, done)
	a.LoadU8(RDX, R8, offObjClass)
	ok := a.NewLabel()
	a.OpImm(Cmp, RDX, int32(ClassObject), false)
	a.Jcc(CondE, ok)
	a.OpImm(Cmp, RDX, int32(ClassArray), false)
	a.Jcc(CondNE, miss)
	a.Bind(ok)
	a.LoadU16(RDX, R8, offObjFlags)
	a.MovRR(R9, RDX)
	a.OpImm(And, R9, int32(objPolyProto), false)
	a.Jcc(CondNE, miss)
	noElems := a.NewLabel()
	a.OpImm(And, RDX, int32(objHasExt), false)
	a.Jcc(CondE, noElems)
	a.CmpMemImm8(R8, sizeObj+8, 0)
	a.Jcc(CondNE, miss)
	a.Bind(noElems)
	g.noIndexKeys(R8)
	a.Jmp(loop)
	a.Bind(done)
}

// noIndexKeys exits unless the object in obj has no key that is an array
// index, as Object.noIndexKeys says: its shape's index says so, or it has
// no keys. RDX is its scratch.
func (g *nativeGen) noIndexKeys(obj Reg) {
	a := g.a
	noIdx, ok := a.NewLabel(), a.NewLabel()
	a.Load(RDX, obj, offObjShape)
	a.Op(Test, RDX, RDX, true)
	a.Jcc(CondE, noIdx)
	a.Load(RDX, RDX, offShapeIndex)
	a.Op(Test, RDX, RDX, true)
	a.Jcc(CondE, noIdx)
	a.LoadU8(RDX, RDX, offIdxIndexKeys)
	a.Op(Test, RDX, RDX, false)
	a.Jcc(CondNE, g.exit)
	a.Jmp(ok)
	a.Bind(noIdx)
	a.LoadU32(RDX, obj, off("Object.valn"))
	a.Op(Test, RDX, RDX, false)
	a.Jcc(CondNE, g.exit)
	a.Bind(ok)
}

// nativeFlatEqMax is the longest text flatEq compares, in bytes.
const nativeFlatEqMax = 1024

// flatEq goes to eq or ne as the strings at R9 and R8, of one length,
// are equal or not, where both are flat: their text compared, as Equals
// compares it, eight bytes at a time; a rope exits. RAX is read again
// before either.
func (g *nativeGen) flatEq(eq, ne Label) {
	a := g.a
	loop, tail, same, differ := a.NewLabel(), a.NewLabel(), a.NewLabel(), a.NewLabel()
	a.CmpMemImm8(R9, offStrLeft, 0)
	a.Jcc(CondNE, g.exit)
	a.CmpMemImm8(R8, offStrLeft, 0)
	a.Jcc(CondNE, g.exit)
	a.Load(RCX, R9, offStrS+8)
	a.CmpMem(RCX, R8, offStrS+8, true)
	a.Jcc(CondNE, differ)
	// A long text is Go's: the exit reads the runtime's counters again,
	// which a host's stop zeroes, as a loop that compares it each turn
	// would otherwise not notice until its budget ran out
	// (TestSlowTurnsAreInterruptible).
	a.OpImm(Cmp, RCX, nativeFlatEqMax, true)
	a.Jcc(CondA, g.exit)
	a.Load(RSI, R9, offStrS)
	a.Load(RDX, R8, offStrS)
	a.Bind(loop)
	a.OpImm(Cmp, RCX, 8, true)
	a.Jcc(CondB, tail)
	a.Load(RAX, RSI, 0)
	a.CmpMem(RAX, RDX, 0, true)
	a.Jcc(CondNE, differ)
	a.OpImm(Add, RSI, 8, true)
	a.OpImm(Add, RDX, 8, true)
	a.OpImm(Sub, RCX, 8, true)
	a.Jmp(loop)
	a.Bind(tail)
	a.Op(Test, RCX, RCX, true)
	a.Jcc(CondE, same)
	a.LoadU8(RAX, RSI, 0)
	a.LoadU8(R9, RDX, 0)
	a.Op(Cmp, RAX, R9, false)
	a.Jcc(CondNE, differ)
	a.OpImm(Add, RSI, 1, true)
	a.OpImm(Add, RDX, 1, true)
	a.OpImm(Sub, RCX, 1, true)
	a.Jmp(tail)
	a.Bind(same)
	a.Load(RAX, RBX, 0)
	a.Jmp(eq)
	a.Bind(differ)
	a.Load(RAX, RBX, 0)
	a.Jmp(ne)
}

// typedBigWrite stores register C, a BigInt held in its Value, as the
// element at the index in idx of the BigInt64Array or BigUint64Array in
// obj: its low 64 bits, ToBigInt64's and ToBigUint64's wrapping; anything
// else exits.
func (g *nativeGen) typedBigWrite(obj, idx Reg) {
	a := g.a
	a.Store(RDI, scr(sI), idx)
	g.decC(R9)
	g.bigShort(R9, R8, R9, true)
	a.Store(RDI, scr(sV0), R9)
	a.Load(idx, RDI, scr(sI))
	g.typedView(obj, idx)
	a.LoadU8(RDX, R8, offTReadonly)
	a.Op(Test, RDX, RDX, false)
	a.Jcc(CondNE, g.exit)
	a.LoadU8(RDX, R8, offTKind)
	a.OpImm(Cmp, RDX, int32(elemBigInt64), false)
	a.Jcc(CondB, g.exit)
	a.OpImm(Cmp, RDX, int32(elemBigUint64), false)
	a.Jcc(CondA, g.exit)
	a.Load(R8, R8, offTData)
	a.LeaIdx(R8, R8, idx, 8, 0)
	a.Load(R9, RDI, scr(sV0))
	a.Store(R8, 0, R9)
}

// ptrStore stores RCX, a pointer word, at [RAX]: plainly while the
// collector does not mark, and by the JIT's storeRef routine while it
// does. Every scratch register is gone after it; on the barrier's path
// the pinned ones are put back from the context, so that RBX, R12 and R15
// are what the context's ip, regs and ic say when it is reached.
func (g *nativeGen) ptrStore() {
	a := g.a
	barrier, cont := a.NewLabel(), a.NewLabel()
	a.Load(RSI, RDI, ncWB)
	a.LoadU8(RSI, RSI, 0)
	a.Op(Test, RSI, RSI, false)
	a.Jcc(CondNE, barrier)
	a.Store(RAX, 0, RCX)
	done := a.NewLabel()
	a.Jmp(done)
	a.Bind(barrier)
	a.Store(RDI, ncIP, RBX)
	a.Store(RDI, ncBudget, R11)
	a.LeaLabel(R11, cont)
	a.JmpMem(RDI, ncStore)
	a.Bind(cont)
	g.reload()
	a.Bind(done)
}

// reload puts the pinned registers back from the context.
func (g *nativeGen) reload() {
	a := g.a
	a.Load(RBX, RDI, ncIP)
	a.Load(R12, RDI, ncRegs)
	a.Load(R13, RDI, ncTable)
	a.Load(R15, RDI, ncIC)
	a.Load(R11, RDI, ncBudget)
	a.Load(R10, RDI, ncTagBase)
}

// typeIndex puts in RCX the index of typeof's type of the Value at the
// window's offset off (typeofNames): a number, a primitive, an object but
// a proxy and document.all; anything else exits. R8, R9 and RDX are its
// scratch.
func (g *nativeGen) typeIndex(off Reg) {
	a := g.a
	idx, obj, num, big, notFn := a.NewLabel(), a.NewLabel(), a.NewLabel(), a.NewLabel(), a.NewLabel()
	a.LoadIdx(R9, R12, off, 1, 0)
	a.Op(Cmp, R9, R10, true)
	a.Jcc(CondB, num)
	a.MovImm(R8, heapBigBits)
	a.Op(Cmp, R9, R8, true)
	a.Jcc(CondAE, big)
	a.MovZX8(RCX, R9)
	a.OpImm(Cmp, RCX, int32(KindObject), false)
	a.Jcc(CondE, obj)
	a.OpImm(Cmp, RCX, int32(KindUndefined), false)
	a.Jcc(CondB, g.exit)
	a.OpImm(Cmp, RCX, int32(KindBigInt), false)
	a.Jcc(CondA, g.exit)
	// undefined, null and a boolean one before their kind; a string,
	// a symbol and a BigInt at it.
	a.OpImm(Cmp, RCX, int32(KindString), false)
	a.Jcc(CondAE, idx)
	a.OpImm(Sub, RCX, 1, false)
	a.Jmp(idx)
	a.Bind(num)
	a.MovImm(RCX, 3)
	a.Jmp(idx)
	a.Bind(big)
	a.MovImm(RCX, 6)
	a.Jmp(idx)
	a.Bind(obj)
	a.LoadIdx(R8, R12, off, 1, 8)
	a.LoadU16(RDX, R8, offObjFlags)
	a.MovImm(RCX, 1)
	a.MovRR(R9, RDX)
	a.OpImm(And, R9, int32(objHTMLDDA), false)
	a.Jcc(CondNE, g.exit)
	a.OpImm(And, RDX, int32(objHasData), false)
	a.Jcc(CondE, idx)
	a.Load(R9, R8, sizeObj+offExtData)
	a.CmpMem(R9, RDI, ncFuncT, true)
	a.Jcc(CondNE, notFn)
	a.MovImm(RCX, 7)
	a.Jmp(idx)
	a.Bind(notFn)
	a.CmpMem(R9, RDI, ncProxyT, true)
	a.Jcc(CondE, g.exit)
	a.Bind(idx)
}
