package nativegen

import (
	"fmt"

	. "github.com/go-quickjs/go-quickjs/internal/jit/asm/arm64"
)

// arm64's handlers are amd64's (amd64.go) in arm64's instructions: the
// same handlers, the same context, frame records and exits, each helper
// here the one of amd64.go of its name. What differs is the machine's:
//
//   - The pinned registers: rIP the instruction's address, rRegs the
//     frame's registers, rTab the native table, rIC the closure's caches,
//     rBud the back-edge budget, rTag the least tag and rCtx the context;
//     rW holds the instruction word, as RAX does.
//   - An operand is decoded to its register's address (regA, regB, regC),
//     not to its offset in the window: arm64 has no base-plus-index
//     addressing with a displacement.
//   - What amd64 does to memory in one instruction (a compare, an add, an
//     increment) is a load into rT and the operation: rT is the helpers'
//     own, never kept across one.
//   - A float compare sets the flags as FCMP does: the conditions are
//     amd64's by cond, mapped for FCMP (f) or an unsigned integer compare
//     (i).
//   - FCVTZS saturates where CVTTSD2SI gives 2^63's bits: a number of no
//     int64 is told by the two limits (toInt32, wrapped), and NaN gives 0.
//
// Registers the code never touches: R18, the platform's; R26 but as the
// func value an exit's call gives Go; R27, the encoder's temporary; R28,
// the goroutine; R29, the frame pointer; R30, the link register, which
// holds nativeEnter's return to nativeRun throughout, so that a profile
// taken in the code and the trampolines' frames read it. R15 is kept
// free for the feedback vector, as amd64 keeps R14.
const (
	rW    Reg = 0  // the instruction word; a store's slot (ptrStore)
	rC    Reg = 1  // amd64's RCX; a stored pointer (ptrStore)
	rD    Reg = 2  // RDX
	rS    Reg = 3  // RSI
	r8    Reg = 4  // R8
	r9    Reg = 5  // R9
	rT    Reg = 6  // the helpers' temporary
	rJ    Reg = 16 // a jump's address; ptrStore's way back
	rK    Reg = 17 // a jump's address; an exclusive store's status
	rIP   Reg = 19 // amd64's RBX
	rRegs Reg = 20 // R12
	rTab  Reg = 21 // R13
	rIC   Reg = 22 // R15
	rBud  Reg = 23 // R11
	rTag  Reg = 24 // R10
	rCtx  Reg = 25 // RDI
	rFn   Reg = 26 // the func value of an exit's call (ABIInternal)
)

// cc is a condition as amd64's handlers name it after a compare: above,
// above or equal, below, below or equal and not equal, which after
// UCOMISD are a float compare's, an unordered pair below.
type cc int

const (
	ccA cc = iota
	ccAE
	ccB
	ccBE
	ccNE
)

// f is c after FCMP: an unordered pair is below, and not equal, as after
// UCOMISD.
func (c cc) f() Cond {
	return [...]Cond{ccA: GT, ccAE: GE, ccB: LT, ccBE: LE, ccNE: NE}[c]
}

// i is c after an unsigned integer compare.
func (c cc) i() Cond {
	return [...]Cond{ccA: HI, ccAE: HS, ccB: LO, ccBE: LS, ccNE: NE}[c]
}

// armGen emits arm64's handlers of a variant (armEmit), as nativeGen does
// amd64's.
type armGen struct {
	a       *Asm
	v       *variant
	exit    Label
	exitRet Label
	// exitAfter is amd64's.
	exitAfter Label
	exits     []Label
	genRet    Label
	// genObj is amd64's.
	genObj Label
}

func (g *armGen) ex(n int) Label {
	for len(g.exits) <= n {
		g.exits = append(g.exits, g.a.NewLabel())
	}
	return g.exits[n]
}

func (g *armGen) dispatch() {
	a := g.a
	a.Load(rW, rIP, 0)
	a.Ubfx(rJ, rW, 0, 8)
	a.LoadIdx(rJ, rTab, rJ, true)
	a.Br(rJ)
}

func (g *armGen) next() {
	g.a.AddImm(rIP, rIP, 8, true)
	g.dispatch()
}

// regA, regB and regC put in r the address of the operand's register;
// regX the address of the register in the instruction's second byte.
func (g *armGen) regA(r Reg) { g.a.Ubfx(r, rW, 16, 16); g.a.AddLsl(r, rRegs, r, 4) }

func (g *armGen) regB(r Reg) { g.a.Ubfx(r, rW, 32, 16); g.a.AddLsl(r, rRegs, r, 4) }

func (g *armGen) regC(r Reg) { g.a.ShiftImm(Lsr, r, rW, 48, true); g.a.AddLsl(r, rRegs, r, 4) }

func (g *armGen) regX(r Reg) { g.a.Ubfx(r, rW, 8, 8); g.a.AddLsl(r, rRegs, r, 4) }

// immSB and immSC put the signed 16-bit operand B or C in r.
func (g *armGen) immSB(r Reg) { g.a.Sbfx(r, rW, 32, 16) }

func (g *armGen) immSC(r Reg) { g.a.ShiftImm(Asr, r, rW, 48, true) }

// cmpMem compares r with the word at [base+disp]; addMem adds it to r;
// cmpMemImm compares the word with imm; storeImm stores imm there; incMem
// and decMem step it. Each through rT.
func (g *armGen) cmpMem(r, base Reg, disp int32) {
	g.a.Load(rT, base, disp)
	g.a.Cmp(r, rT, true)
}

func (g *armGen) addMem(r, base Reg, disp int32) {
	g.a.Load(rT, base, disp)
	g.a.Op(Add, r, r, rT, true)
}

func (g *armGen) cmpMemImm(base Reg, disp int32, imm int64) {
	g.a.Load(rT, base, disp)
	g.a.CmpImm(rT, imm, true)
}

func (g *armGen) storeImm(base Reg, disp int32, imm int64) {
	if imm == 0 {
		g.a.Store(base, disp, ZR)
		return
	}
	g.a.MovImm(rT, uint64(imm))
	g.a.Store(base, disp, rT)
}

func (g *armGen) incMem(base Reg, disp int32, by int64) {
	g.a.Load(rT, base, disp)
	g.a.AddImm(rT, rT, by, true)
	g.a.Store(base, disp, rT)
}

// tagged goes to no unless the word in x is a tagged word's of no payload
// past its low 16 bits (its high 48 tagBase's), with its kind, the low
// byte, in rC.
func (g *armGen) tagged(x Reg, no Label) {
	a := g.a
	a.AndImm(rC, x, ^uint64(0xFFFF), true)
	a.Cmp(rC, rTag, true)
	a.BCond(NE, no)
	a.Ubfx(rC, x, 0, 8)
}

// kindIs exits unless the word in x is a tag of kind k: (x^tagBase)>>16
// zero, its low byte k. rC is its scratch.
func (g *armGen) kindIs(x Reg, k Kind) {
	a := g.a
	a.Op(Eor, rC, x, rTag, true)
	a.ShiftImm(Lsr, rC, rC, 16, true)
	a.Cbnz(rC, g.exit, true)
	a.Ubfx(rC, x, 0, 8)
	a.CmpImm(rC, int64(k), false)
	a.BCond(NE, g.exit)
}

// num loads the number at the register address addr into x, or exits.
func (g *armGen) num(x FReg, addr Reg) { g.numOr(x, addr, g.exit) }

// numOr is num going to miss rather than exiting.
func (g *armGen) numOr(x FReg, addr Reg, miss Label) {
	a := g.a
	a.Load(r9, addr, 0)
	a.Cmp(r9, rTag, true)
	a.BCond(HS, miss)
	a.FMovToF(x, r9)
}

// numMem is numOr for an operand of arithmetic, as amd64's: x is loaded
// from memory rather than moved from r9, so that a chain of operations
// through a register waits on the store's forwarding alone.
func (g *armGen) numMem(x FReg, addr Reg, miss Label) {
	a := g.a
	a.Load(r9, addr, 0)
	a.Cmp(r9, rTag, true)
	a.BCond(HS, miss)
	a.LoadF(x, addr, 0)
}

// noInt64 exits where r, FCVTZS's answer, is either limit: a number of
// no int64 (or exactly -2^63, as on amd64), which saturates.
func (g *armGen) noInt64(r Reg) {
	a := g.a
	a.CmpImm(r, 1, true)
	a.BCond(VS, g.exit)
	a.CmpImm(r, -1, true)
	a.BCond(VS, g.exit)
}

// toInt32 makes dst's low half ToInt32 of the number in x, its truncation
// modulo 2^32, where the truncation is an int64; an infinity or a number
// past 2^63 exits, and NaN gives 0, its ToInt32.
func (g *armGen) toInt32(dst Reg, x FReg) {
	g.a.Fcvtzs(dst, x)
	g.noInt64(dst)
}

// upval puts in dst the address of the value of the frame closure's
// upvalue whose index is in idx, or exits past the closure's upvalues.
func (g *armGen) upval(dst, idx Reg) {
	a := g.a
	a.Load(dst, rCtx, ncNF)
	a.Load(dst, dst, offFCl)
	g.cmpMem(idx, dst, offClUpvals+8)
	a.BCond(HS, g.exit)
	a.Load(dst, dst, offClUpvals)
	a.LoadIdx(dst, dst, idx, true)
	a.Load(dst, dst, 0)
}

// cmpU is a compare-and-branch of A with upvalue B, as cmp's swapped
// form: the jump taken on cond.
func (g *armGen) cmpU(cond cc) {
	a := g.a
	g.regA(rD)
	g.num(0, rD)
	a.Ubfx(rC, rW, 32, 16)
	g.upval(rS, rC)
	a.Load(r9, rS, 0)
	a.Cmp(r9, rTag, true)
	a.BCond(HS, g.exit)
	a.FMovToF(1, r9)
	a.FCmp(1, 0)
	tk := a.NewLabel()
	a.BCond(cond.f(), tk)
	g.next()
	a.Bind(tk)
	g.immSC(rC)
	g.taken(rC)
}

// nullish goes to yes if the word in x is undefined's or null's. rC is
// its scratch.
func (g *armGen) nullish(x Reg, yes Label) {
	a := g.a
	no := a.NewLabel()
	g.tagged(x, no)
	a.CmpImm(rC, int64(KindNull), false)
	a.BCond(EQ, yes)
	a.CmpImm(rC, int64(KindUndefined), false)
	a.BCond(EQ, yes)
	a.Bind(no)
}

// kindIn goes to yes if the word in x is a word kind of one of mask's
// kinds: tagged, below the BigInts. rC and rW are its scratch.
func (g *armGen) kindIn(x Reg, mask uint64, yes, no Label) {
	a := g.a
	a.Op(Sub, rC, x, rTag, true)
	a.MovImm(rW, heapBigBits-tagBase)
	a.Cmp(rC, rW, true)
	a.BCond(HS, no)
	a.Ubfx(rC, x, 0, 8)
	a.MovImm(rW, mask)
	a.ShiftReg(Lsr, rW, rW, rC, true)
	a.TstImm(rW, 1, true)
	a.BCond(NE, yes)
	a.B(no)
}

// looseEq is == and != (jeq, jne), as amd64's.
func (g *armGen) looseEq(jumpIfEq bool) {
	a := g.a
	eq, ne, words, aNull, bNull, same := a.NewLabel(), a.NewLabel(), a.NewLabel(), a.NewLabel(), a.NewLabel(), a.NewLabel()
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
	g.nullish(r9, aNull)
	g.nullish(r8, bNull)
	// Neither: one word kind, the same word.
	a.Cmp(r9, r8, true)
	a.BCond(NE, g.exit)
	g.kindIn(r9, 1<<KindBool|1<<KindSymbol|1<<KindObject, same, g.exit)
	a.Bind(same)
	a.Load(r9, rD, 8)
	a.Load(r8, rS, 8)
	a.Cmp(r9, r8, true)
	a.BCond(NE, ne)
	a.B(eq)
	// undefined or null against the other: equal if it is either, or
	// document.all's kind; not equal to anything else.
	for _, x := range []struct {
		l           Label
		other, addr Reg
	}{{aNull, r8, rS}, {bNull, r9, rD}} {
		a.Bind(x.l)
		g.nullish(x.other, eq)
		g.cmpMem(x.other, rCtx, ncObjBits)
		a.BCond(NE, ne)
		a.Load(rC, x.addr, 8)
		a.LoadU16(rC, rC, offObjFlags)
		a.TstImm(rC, uint64(objHTMLDDA), false)
		a.BCond(NE, g.exit)
		a.B(ne)
	}
	a.Bind(eq)
	a.Load(rW, rIP, 0)
	if jumpIfEq {
		g.immSC(rC)
		g.taken(rC)
	} else {
		g.next()
	}
	a.Bind(ne)
	a.Load(rW, rIP, 0)
	if jumpIfEq {
		g.next()
	} else {
		g.immSC(rC)
		g.taken(rC)
	}
}

// noRef exits unless the Value at the register address addr holds no
// pointer.
func (g *armGen) noRef(addr Reg) {
	g.a.Load(rT, addr, 8)
	g.a.Cbnz(rT, g.exit, true)
}

// canon makes a NaN in x the canonical one.
func (g *armGen) canon(x FReg) {
	a := g.a
	ok := a.NewLabel()
	a.FCmp(x, x)
	a.BCond(VC, ok)
	a.LoadF(x, rCtx, ncNaN)
	a.Bind(ok)
}

// storeA stores the number in x to register A, its pointer word cleared
// by ptrStore where it held one, and goes on.
func (g *armGen) storeA(x FReg) {
	a := g.a
	done := a.NewLabel()
	g.regA(rD)
	a.StoreF(rD, 0, x)
	a.Load(rT, rD, 8)
	a.Cbz(rT, done, true)
	// The register held a pointer: its word cleared as any pointer store.
	a.AddImm(rW, rD, 8, true)
	a.MovImm(rC, 0)
	g.ptrStore()
	a.Bind(done)
	g.next()
}

// storeB is storeA's of register B.
func (g *armGen) storeB(x FReg) {
	a := g.a
	done := a.NewLabel()
	g.regB(rD)
	a.StoreF(rD, 0, x)
	a.Load(rT, rD, 8)
	a.Cbz(rT, done, true)
	a.AddImm(rW, rD, 8, true)
	a.MovImm(rC, 0)
	g.ptrStore()
	a.Bind(done)
	g.next()
}

// storeValue stores the Value whose words are r9 (num) and r8 (ref) at
// the address in rW and goes on, as amd64's: the pointer word by
// ptrStore's routine while the collector marks.
func (g *armGen) storeValue() {
	a := g.a
	barrier, cont := a.NewLabel(), a.NewLabel()
	a.Store(rW, 0, r9)
	a.Load(rS, rCtx, ncWB)
	a.LoadU8(rS, rS, 0)
	a.Cbnz(rS, barrier, false)
	a.Store(rW, 8, r8)
	g.next()
	a.Bind(barrier)
	a.Store(rCtx, ncIP, rIP)
	a.Store(rCtx, ncBudget, rBud)
	a.AddImm(rW, rW, 8, true)
	a.MovRR(rC, r8)
	a.Adr(rJ, cont)
	a.Load(rK, rCtx, ncStore)
	a.Br(rK)
	a.Bind(cont)
	g.reload()
	g.next()
}

// loadValue puts the Value at [base] in r9 (num) and r8 (ref).
func (g *armGen) loadValue(base Reg) {
	g.a.Load(r9, base, 0)
	g.a.Load(r8, base, 8)
}

// taken jumps by the instruction count in off, counting a backward one
// against the budget, whose end Go's handler meets.
func (g *armGen) taken(off Reg) {
	a := g.a
	fwd := a.NewLabel()
	a.CmpImm(off, 0, true)
	a.BCond(PL, fwd)
	a.CmpImm(rBud, 1, true)
	a.BCond(LE, g.exit)
	a.AddImm(rBud, rBud, -1, true)
	a.Bind(fwd)
	a.AddLsl(rIP, rIP, off, 3)
	a.AddImm(rIP, rIP, 8, true)
	g.dispatch()
}

// cmp is a compare-and-branch: A with B, or with the immediate SB; swap
// compares B with A, and the jump is taken on cond (and on an unordered
// pair if nan, which NE is after FCMP).
func (g *armGen) cmp(imm, swap bool, cond cc, nan bool) {
	a := g.a
	big := g.exit
	if !imm {
		big = a.NewLabel()
	}
	g.regA(rD)
	g.numOr(0, rD, big)
	if imm {
		g.immSB(rC)
		a.Scvtf(1, rC, true)
	} else {
		g.regB(rS)
		g.numOr(1, rS, big)
	}
	if swap {
		a.FCmp(1, 0)
	} else {
		a.FCmp(0, 1)
	}
	tk := a.NewLabel()
	if nan && cond != ccNE {
		a.BCond(VS, tk)
	}
	a.BCond(cond.f(), tk)
	g.next()
	a.Bind(tk)
	g.immSC(rC)
	g.taken(rC)
	if big != g.exit {
		a.Bind(big)
		g.bigCmp(swap)
		a.BCond(cond.i(), tk)
		g.next()
	}
}

// bigCmp sets the flags as an unsigned compare of A with B (of B with A
// where swap) would for two BigInts held in their Values, which it
// compares as 128-bit integers; anything else exits. rW is read again
// after.
func (g *armGen) bigCmp(swap bool) {
	a := g.a
	hiSame, done := a.NewLabel(), a.NewLabel()
	g.regA(r9)
	g.bigShort(r9, r8, r9, true)
	g.regB(rS)
	g.bigShort(rS, rW, rS, true)
	x0, x1, y0, y1 := r9, r8, rS, rW
	if swap {
		x0, x1, y0, y1 = rS, rW, r9, r8
	}
	a.Cmp(x1, y1, true)
	a.BCond(EQ, hiSame)
	// The high words differ: compared signed, as unsigned with the sign
	// bit flipped.
	a.MovImm(rC, 1<<63)
	a.Op(Eor, x1, x1, rC, true)
	a.Op(Eor, y1, y1, rC, true)
	a.Cmp(x1, y1, true)
	a.B(done)
	a.Bind(hiSame)
	a.Cmp(x0, y0, true)
	a.Bind(done)
	a.Load(rW, rIP, 0)
}

// arith is R[A] = R[B] op R[C], or op an immediate SC, or op 1.
func (g *armGen) arith(op FOp, operand int) {
	a := g.a
	big := g.exit
	if operand == 0 && (op == FAdd || op == FSub || op == FMul) {
		big = a.NewLabel()
	}
	g.regB(rS)
	g.numMem(0, rS, big)
	switch operand {
	case 0:
		g.regC(r8)
		g.numMem(1, r8, big)
	case 1:
		g.immSC(rC)
		a.Scvtf(1, rC, true)
	case 2:
		a.LoadF(1, rCtx, ncOne)
	}
	a.FArith(op, 0, 0, 1)
	g.canon(0)
	g.storeA(0)
	if big != g.exit {
		a.Bind(big)
		g.bigArith(op)
	}
}

// exitStub is the exit, as amd64's: the instruction at rIP left to Go,
// its Go handler called by nativeCallH -- its arguments in R0 to R3, its
// func value in R26 -- and where it gives back ^pc its slow function;
// the code goes on at the pc they give, or returns to nativeRun.
func (g *armGen) exitStub() {
	a := g.a
	slow, afterSlow, back, end, noCount, count := a.NewLabel(), a.NewLabel(), a.NewLabel(), a.NewLabel(), a.NewLabel(), a.NewLabel()
	a.Bind(g.exit)
	g.exitState()
	ready := a.NewLabel()
	a.Load(rT, rCtx, ncTopOK)
	a.Cbnz(rT, ready, true)
	g.makeTop(slow)
	a.Load(rW, rCtx, ncW)
	a.Load(rC, rCtx, ncPC)
	a.Bind(ready)
	// Counted where a test counts, atomically: runtimes on other
	// goroutines count too.
	a.Load(rD, rCtx, ncCntExits)
	a.Cbz(rD, noCount, true)
	a.Ubfx(r8, rW, 0, 8)
	a.AddLsl(rD, rD, r8, 3)
	a.Bind(count)
	a.Ldxr(rJ, rD)
	a.AddImm(rJ, rJ, 1, true)
	a.Stxr(rK, rJ, rD)
	a.Cbnz(rK, count, false)
	a.Bind(noCount)
	a.Load(rD, rCtx, ncRT)
	a.Store(rD, offRtBackEdges, rBud)
	a.Adr(r8, g.exitAfter)
	a.Store(rCtx, ncAfter, r8)
	// handler(st.regs, w, pc, st): w in R1, pc in R2, the func value in
	// R26.
	a.MovRR(rD, rC)
	a.MovRR(rC, rW)
	a.Ubfx(r8, rW, 0, 8)
	a.Load(r9, rCtx, ncGoTable)
	a.LoadIdx(rFn, r9, r8, true)
	// nativeCallMiss where the call handler asks for it (ncMiss).
	have := a.NewLabel()
	a.Load(rT, rCtx, ncMiss)
	a.Cbz(rT, have, true)
	a.Store(rCtx, ncMiss, ZR)
	a.Load(rFn, rCtx, ncCallMissFn)
	a.Bind(have)
	a.Load(rS, rCtx, ncTop)
	a.Load(rW, rS, offStRegs)
	a.Load(rJ, rCtx, ncCallH)
	a.Br(rJ)
	a.Bind(g.exitAfter)
	a.CmpImm(rW, 0, true)
	a.BCond(GE, back)
	a.MovImm(rC, uint64(1)<<63)
	a.Cmp(rW, rC, true)
	a.BCond(EQ, end)
	// ^pc: the slow function of the instruction at pc.
	a.Adr(r8, afterSlow)
	a.Store(rCtx, ncAfter, r8)
	a.Mvn(rD, rW, true)
	a.Load(r8, rCtx, ncCodeBase)
	a.LoadIdx(rC, r8, rD, true)
	a.Ubfx(r8, rC, 0, 8)
	a.Load(r9, rCtx, ncSlowTable)
	a.LoadIdx(rFn, r9, r8, true)
	a.Load(rS, rCtx, ncTop)
	a.Load(rW, rS, offStRegs)
	a.Load(rJ, rCtx, ncCallH)
	a.Br(rJ)
	a.Bind(afterSlow)
	a.CmpImm(rW, 0, true)
	a.BCond(LT, end)
	// On at pc, the pinned registers put back, the budget the runtime's.
	a.Bind(back)
	a.Load(rD, rCtx, ncRT)
	a.Load(rBud, rD, offRtBackEdges)
	a.Load(rIP, rCtx, ncCodeBase)
	a.AddLsl(rIP, rIP, rW, 3)
	a.Load(rRegs, rCtx, ncRegs)
	a.Load(rTab, rCtx, ncTable)
	a.Load(rIC, rCtx, ncIC)
	a.Load(rTag, rCtx, ncTagBase)
	g.dispatch()
	// The frame ended: nativeRun ends it, the budget the runtime's.
	a.Bind(end)
	g.storeImm(rCtx, ncEnded, 1)
	a.Load(rD, rCtx, ncRT)
	a.Load(rBud, rD, offRtBackEdges)
	a.Store(rCtx, ncBudget, rBud)
	a.Bind(slow)
	a.Ret()
	// exitRet: the instruction, its index and the budget, and back to
	// nativeRun.
	a.Bind(g.exitRet)
	g.exitState()
	a.Ret()
}

// exitState writes the instruction at rIP, its address, its index and
// the budget to the context; the word is left in rW, the index in rC.
func (g *armGen) exitState() {
	a := g.a
	a.Store(rCtx, ncIP, rIP)
	a.Store(rCtx, ncBudget, rBud)
	a.Load(rW, rIP, 0)
	a.Store(rCtx, ncW, rW)
	a.Load(rD, rCtx, ncCodeBase)
	a.Op(Sub, rC, rIP, rD, true)
	a.ShiftImm(Lsr, rC, rC, 3, true)
	a.Store(rCtx, ncPC, rC)
}

// primMethod is get_method_w of a string's or a number's method, as
// amd64's: the receiver's words in r9 and at the register address in rS.
func (g *armGen) primMethod() {
	a := g.a
	num, have := a.NewLabel(), a.NewLabel()
	a.Store(rCtx, scr(sV0), r9)
	a.Load(r8, rS, 8)
	a.Store(rCtx, scr(sV1), r8)
	// The realm running now, the Runtime's first word.
	a.Load(rD, rCtx, ncRT)
	a.Load(rD, rD, rtRel(0))
	a.Cmp(r9, rTag, true)
	a.BCond(LO, num)
	g.kindIs(r9, KindString)
	a.Load(r8, rCtx, ncProtoStr)
	a.B(have)
	a.Bind(num)
	a.Load(r8, rCtx, ncProtoNum)
	a.Bind(have)
	a.LoadIdx(r8, rD, r8, false)
	a.Cbz(r8, g.exit, true)
	g.primSlot()
	g.loadValue(rC)
	g.regA(rW)
	a.Store(rW, 0, r9)
	a.AddImm(rW, rW, 8, true)
	a.MovRR(rC, r8)
	g.ptrStore()
	a.Load(rW, rIP, 0)
	g.regA(rW)
	a.AddImm(rW, rW, 16, true)
	a.Load(r9, rCtx, scr(sV0))
	a.Store(rW, 0, r9)
	a.AddImm(rW, rW, 8, true)
	a.Load(rC, rCtx, scr(sV1))
	g.ptrStore()
	g.next()
}

// makeTop makes the top frame's state for Go, as amd64's, or goes to
// slow where Go must.
func (g *armGen) makeTop(slow Label) {
	a := g.a
	deep, fill, filled, set := a.NewLabel(), a.NewLabel(), a.NewLabel(), a.NewLabel()
	a.Load(r8, rCtx, ncDepth)
	a.Cbnz(r8, deep, true)
	a.Load(rC, rCtx, ncSt)
	a.B(set)
	a.Bind(deep)
	// The state Go made for the depth, holding no error.
	a.Load(rS, rCtx, ncStates)
	a.AddLsl(rS, rS, r8, 3)
	a.Load(rS, rS, -8)
	a.Cbz(rS, slow, true)
	a.Load(rT, rS, offStErr)
	a.Cbnz(rT, slow, true)
	// The callers' pcs: frames[filled:depth].
	a.Load(r9, rCtx, ncFilled)
	a.Bind(fill)
	a.Cmp(r9, r8, true)
	a.BCond(GE, filled)
	a.MovImm(rC, uint64(recSize))
	a.Mul(rD, r9, rC)
	g.addMem(rD, rCtx, ncFrames)
	a.Load(rC, rD, recRetIP)
	a.Load(rW, rD, recCodeBaseOff)
	a.Op(Sub, rC, rC, rW, true)
	a.ShiftImm(Lsr, rC, rC, 1, true)
	a.Load(rW, rD, recRCOff)
	a.Load(rW, rW, offRCSrc)
	a.Op(Add, rW, rW, rC, true)
	a.LoadU32(rC, rW, 0)
	a.AddImm(rC, rC, 1, false)
	a.Load(rW, rD, recNFOff)
	a.Store32(rW, offFPC, rC)
	a.AddImm(r9, r9, 1, true)
	a.B(fill)
	a.Bind(filled)
	a.Store(rCtx, ncFilled, r8)
	// The depth among those the run has used (used), which its end
	// clears.
	used := a.NewLabel()
	a.Load(rC, rCtx, ncUsed)
	a.Cmp(r8, rC, true)
	a.BCond(LE, used)
	a.Store(rCtx, ncUsed, r8)
	a.Bind(used)
	// Its frame, code, closure and window, each where it differs, and its
	// window's base.
	a.Store(rCtx, scr(sV0), rS)
	a.Load(rC, rCtx, ncNF)
	a.Load(rD, rC, offFBase)
	a.Load(rW, rCtx, ncRC)
	a.LoadS32(rW, rW, offRCLocals)
	a.Op(Sub, rD, rD, rW, true)
	a.Store(rS, offStWb, rD)
	for _, f := range [][2]int32{{ncNF, offStF}, {ncRC, offStC}, {-2, offStRegs}, {-1, offStCl}} {
		a.Load(rS, rCtx, scr(sV0))
		switch f[0] {
		case -1:
			a.Load(rC, rCtx, ncNF)
			a.Load(rC, rC, offFCl)
		case -2:
			// regWindow(r.stack, wb).
			a.Load(rC, rS, offStWb)
			a.ShiftImm(Lsl, rC, rC, 4, true)
			a.Load(rD, rCtx, ncRT)
			g.addMem(rC, rD, offRtStack)
		default:
			a.Load(rC, rCtx, f[0])
		}
		a.AddImm(rW, rS, int64(f[1]), true)
		g.ptrTo()
	}
	a.Load(rC, rCtx, scr(sV0))
	a.Bind(set)
	a.AddImm(rW, rCtx, int64(ncTop), true)
	g.ptrTo()
	g.storeImm(rCtx, ncTopOK, 1)
}

// primSlot puts in rC the address of the data property of the prototype
// in r8 (of the realm in rD) that the realm's cache of primitives'
// properties remembers for the name of site C, or exits. r9 and rS are
// its scratch.
func (g *armGen) primSlot() {
	a := g.a
	// The name: names[siteName[C]].
	a.ShiftImm(Lsr, rC, rW, 48, true)
	a.Load(r9, rCtx, ncRC)
	a.Load(r9, r9, offRCSiteName)
	a.AddLsl(r9, r9, rC, 2)
	a.LoadU32(rC, r9, 0)
	a.Load(r9, rCtx, ncCC)
	a.Load(r9, r9, offCCNames)
	a.AddLsl(r9, r9, rC, 2)
	a.LoadU32(rC, r9, 0)
	// Its entry, primProps[name&63], of this prototype and its shape.
	a.AndImm(r9, rC, 63, true)
	a.AddLsl(r9, r9, r9, 1)
	a.ShiftImm(Lsl, r9, r9, 3, true)
	a.Op(Add, r9, r9, rD, true)
	g.addMem(r9, rCtx, ncPrimProps)
	a.LoadU32(rS, r9, offPEKey)
	a.Cmp(rS, rC, false)
	a.BCond(NE, g.exit)
	g.cmpMem(r8, r9, offPEProto)
	a.BCond(NE, g.exit)
	a.Load(rS, r9, offPEShape)
	g.cmpMem(rS, r8, offObjShape)
	a.BCond(NE, g.exit)
	a.LoadS32(rC, r9, offPEIdx)
	a.ShiftImm(Lsl, rC, rC, 4, true)
	g.addMem(rC, r8, off("Object.valp"))
}

// postStep is x++ and x-- (post_inc, post_dec) of a number: R[A] the
// old value where it holds no pointer, R[B] the new.
func (g *armGen) postStep(op FOp) {
	a := g.a
	g.regB(rS)
	g.num(0, rS)
	g.regA(rD)
	g.noRef(rD)
	a.LoadF(1, rCtx, ncOne)
	a.FArith(op, 3, 0, 1)
	g.canon(3)
	a.StoreF(rD, 0, 0)
	a.StoreF(rS, 0, 3)
	g.next()
}

// bigShort puts in lo and hi, a 128-bit two's complement integer, the
// BigInt the Value at the register address addr holds in itself, as
// amd64's; else it exits. rD and rC are its scratch; addr may be lo.
func (g *armGen) bigShort(lo, hi, addr Reg, wide bool) {
	a := g.a
	notInt, done := a.NewLabel(), a.NewLabel()
	a.Load(rD, addr, 8)
	a.Load(lo, addr, 0)
	a.MovImm(rC, shortIntBase)
	a.Cmp(lo, rC, true)
	a.BCond(LO, notInt)
	// The low 49 bits from num, the high 15 the byte of shortBigHigh
	// that ref points at.
	a.Ubfx(lo, lo, 0, shortBigLowBits)
	a.Load(rC, rCtx, ncBigHigh)
	a.Op(Sub, rD, rD, rC, true)
	a.ShiftImm(Lsl, rD, rD, shortBigLowBits, true)
	a.Op(Orr, lo, lo, rD, true)
	a.ShiftImm(Asr, hi, lo, 63, true)
	a.B(done)
	a.Bind(notInt)
	if !wide {
		a.B(g.exit)
	} else {
		// One past the int64 range: the low 49 bits of its 65 from num,
		// the next 15 and the 65th the byte of shortBigHighX that ref
		// points at.
		a.MovImm(rC, shortBigBase)
		a.Cmp(lo, rC, true)
		a.BCond(LO, g.exit)
		a.Ubfx(lo, lo, 0, shortBigLowBits)
		a.Load(rC, rCtx, ncBigHighX)
		a.Op(Sub, rD, rD, rC, true)
		a.Sbfx(hi, rD, 64-shortBigLowBits, 1)
		a.Ubfx(rD, rD, 0, 64-shortBigLowBits)
		a.ShiftImm(Lsl, rD, rD, shortBigLowBits, true)
		a.Op(Orr, lo, lo, rD, true)
	}
	a.Bind(done)
}

// bigArith is R[A] = R[B] op R[C] of two BigInts held in their Values, as
// amd64's.
func (g *armGen) bigArith(op FOp) {
	a := g.a
	wide := op != FMul
	g.regB(rS)
	g.bigShort(r9, r8, rS, wide)
	g.regC(rS)
	g.bigShort(rS, rW, rS, wide)
	switch op {
	case FAdd:
		a.Op(Adds, r9, r9, rS, true)
		a.Cset(rC, HS)
		a.Op(Add, r8, r8, rW, true)
		a.Op(Add, r8, r8, rC, true)
	case FSub:
		a.Op(Subs, r9, r9, rS, true)
		a.Cset(rC, LO)
		a.Op(Sub, r8, r8, rW, true)
		a.Op(Sub, r8, r8, rC, true)
	case FMul:
		a.Smulh(rC, r9, rS)
		a.Mul(r9, r9, rS)
		a.ShiftImm(Asr, r8, r9, 63, true)
		a.Cmp(rC, r8, true)
		a.BCond(NE, g.exit)
	}
	g.bigStoreA()
}

// bigStoreA stores the 128-bit integer r8:r9 to register A as a BigInt
// held in its Value, as amd64's; anything wider exits. rW is read again
// first.
func (g *armGen) bigStoreA() {
	a := g.a
	small, past, store, done := a.NewLabel(), a.NewLabel(), a.NewLabel(), a.NewLabel()
	a.ShiftImm(Asr, rD, r9, 63, true)
	a.Cmp(rD, r8, true)
	a.BCond(EQ, small)
	a.Cbz(r8, past, true)
	a.CmpImm(r8, -1, true)
	a.BCond(NE, g.exit)
	a.Cbz(r9, g.exit, true)
	a.MovImm(r8, 1<<(64-shortBigLowBits))
	a.B(store)
	a.Bind(past)
	a.MovImm(r8, 0)
	a.Bind(store)
	// One past: shortBigHighX's byte at the high 15 bits and the 65th.
	a.ShiftImm(Lsr, rD, r9, shortBigLowBits, true)
	a.Op(Add, r8, r8, rD, true)
	g.addMem(r8, rCtx, ncBigHighX)
	a.MovImm(rC, shortBigBase)
	a.B(done)
	a.Bind(small)
	a.ShiftImm(Lsr, r8, r9, shortBigLowBits, true)
	g.addMem(r8, rCtx, ncBigHigh)
	a.MovImm(rC, shortIntBase)
	a.Bind(done)
	a.Ubfx(r9, r9, 0, shortBigLowBits)
	a.Op(Orr, r9, r9, rC, true)
	a.Load(rW, rIP, 0)
	g.regA(rW)
	g.storeValue()
}

// bitImm is R[A] = int32 R[B] op SC, the count of a shift in rC.
func (g *armGen) bitImm(emit func(), unsigned ...bool) {
	g.regB(rS)
	g.numMem(0, rS, g.exit)
	g.toInt32(r9, 0)
	g.immSC(rC)
	emit()
	g.int32Out(0, r9, len(unsigned) > 0)
	g.storeA(0)
}

// bitReg is R[A] = int32 R[B] op int32 R[C], R[C] in rC.
func (g *armGen) bitReg(emit func(), unsigned ...bool) {
	g.regB(rS)
	g.numMem(0, rS, g.exit)
	g.regC(r8)
	g.numMem(1, r8, g.exit)
	g.toInt32(rC, 1)
	g.toInt32(r9, 0)
	emit()
	g.int32Out(0, r9, len(unsigned) > 0)
	g.storeA(0)
}

// int32Out makes x the 32-bit integer in r's low half, signed or not.
func (g *armGen) int32Out(x FReg, r Reg, unsigned bool) {
	if unsigned {
		g.a.Ucvtf(x, r, false)
	} else {
		g.a.Scvtf(x, r, false)
	}
}

// object puts the object at the register address addr in dst, or exits.
func (g *armGen) object(dst, addr Reg) {
	a := g.a
	a.Load(r9, addr, 0)
	g.cmpMem(r9, rCtx, ncObjBits)
	a.BCond(NE, g.exit)
	a.Load(dst, addr, 8)
}

// site puts &ic[C] in dst.
func (g *armGen) site(dst Reg) {
	a := g.a
	a.ShiftImm(Lsr, dst, rW, 48, true)
	a.MovImm(rT, uint64(size("propCache")))
	a.Mul(dst, dst, rT)
	a.Op(Add, dst, dst, rIC, true)
}

// ownSlot puts the address of the own data property the site at rC
// remembers of the object in obj in dst, or exits.
func (g *armGen) ownSlot(dst, obj Reg) {
	a := g.a
	a.Load(r8, rC, off("propCache.ownShape"))
	g.cmpMem(r8, obj, off("Object.shape"))
	a.BCond(NE, g.exit)
	a.LoadS32(dst, rC, off("propCache.idx"))
	a.ShiftImm(Lsl, dst, dst, 4, true)
	g.addMem(dst, obj, off("Object.valp"))
}

// readSlot puts in rC the address of the data property the site at rC
// remembers of the object in rS, as amd64's; else it exits. r8, r9 and
// rD are its scratch.
func (g *armGen) readSlot() {
	a := g.a
	own, holder, noCell, p1Holder := a.NewLabel(), a.NewLabel(), a.NewLabel(), a.NewLabel()
	poly, polyLoop, polyNext, shaped, done := a.NewLabel(), a.NewLabel(), a.NewLabel(), a.NewLabel(), a.NewLabel()
	shapeOff := off("Object.shape")
	a.Load(r8, rC, off("propCache.ownShape"))
	g.cmpMem(r8, rS, shapeOff)
	a.BCond(EQ, own)
	a.Load(r8, rC, off("propCache.shape"))
	g.cmpMem(r8, rS, shapeOff)
	a.BCond(NE, poly)
	a.Bind(shaped)
	a.LoadU8(r8, rC, off("propCache.getter"))
	a.Cbnz(r8, g.exit, false)
	a.Load(r8, rC, off("propCache.cell"))
	a.Cbz(r8, noCell, true)
	a.LoadU8(r8, r8, off("protoCell.valid"))
	a.Cbz(r8, g.exit, false)
	a.Load(rS, rC, off("propCache.p2"))
	a.Cbz(rS, g.exit, true)
	a.B(holder)
	a.Bind(noCell)
	a.Load(r8, rC, off("propCache.p1"))
	a.Cbz(r8, holder, true) // the object itself
	a.Load(r9, r8, shapeOff)
	g.cmpMem(r9, rC, off("propCache.s1"))
	a.BCond(NE, g.exit)
	a.Load(r9, rC, off("propCache.p2"))
	a.Cbz(r9, p1Holder, true)
	a.Load(rD, r9, shapeOff)
	g.cmpMem(rD, rC, off("propCache.s2"))
	a.BCond(NE, g.exit)
	a.MovRR(rS, r9)
	a.B(holder)
	a.Bind(p1Holder)
	a.MovRR(rS, r8)
	a.Bind(own)
	a.Bind(holder)
	a.LoadS32(r8, rC, off("propCache.idx"))
	a.ShiftImm(Lsl, r8, r8, 4, true)
	g.addMem(r8, rS, off("Object.valp"))
	a.MovRR(rC, r8)
	a.B(done)
	// A site of several shapes: the entries it remembered before, an own
	// data property of the object's shape among them.
	a.Bind(poly)
	a.Load(r8, rC, off("propCache.poly"))
	a.Cbz(r8, g.exit, true)
	a.LoadU8(r9, r8, off("propPoly.n"))
	a.Cbz(r9, g.exit, false)
	g.addMem(r8, rCtx, ncPolyEnt)
	a.Load(rD, rS, shapeOff)
	a.Bind(polyLoop)
	a.MovRR(rC, r8)
	g.cmpMem(rD, r8, off("propCache.ownShape"))
	a.BCond(EQ, own)
	// An entry of the shape for a prototype's property: its holder found
	// as the site's own entry's is.
	g.cmpMem(rD, r8, off("propCache.shape"))
	a.BCond(EQ, shaped)
	a.Bind(polyNext)
	a.AddImm(r8, r8, int64(size("propCache")), true)
	a.SubsImm(r9, r9, 1, false)
	a.BCond(NE, polyLoop)
	a.B(g.exit)
	a.Bind(done)
}

// element puts the address of the dense element of the object in obj at
// the number key in x in dst, or exits; mask is the flags that must be
// clear but objHasExt. An index the object has no dense element at goes
// to miss with the index in dst.
func (g *armGen) element(dst, obj Reg, x FReg, mask objFlags, miss Label) {
	a := g.a
	a.Fcvtzs(dst, x)
	a.ShiftImm(Lsr, rT, dst, 32, true)
	a.Cbnz(rT, g.exit, true)
	a.Scvtf(3, dst, true)
	a.FCmp(x, 3)
	a.BCond(NE, g.exit)
	g.dense(dst, obj, mask, miss)
}

// dense makes dst, an index below 2^32, the address of the dense element
// of the object in obj there, or goes to miss with dst as it was; mask is
// the flags that must be clear but objHasExt. A hole exits.
func (g *armGen) dense(dst, obj Reg, mask objFlags, miss Label) {
	a := g.a
	a.LoadU16(rD, obj, offObjFlags)
	a.AndImm(rD, rD, uint64(mask|objHasExt), false)
	a.CmpImm(rD, int64(objHasExt), false)
	a.BCond(NE, miss)
	g.cmpMem(dst, obj, sizeObj+8)
	a.BCond(HS, miss)
	a.ShiftImm(Lsl, dst, dst, 4, true)
	g.addMem(dst, obj, sizeObj)
	// No hole.
	a.Load(rD, dst, 0)
	g.cmpMem(rD, rCtx, ncHole)
	a.BCond(EQ, g.exit)
}

// typedView puts in r8 the typed array data of the object in obj, a view
// good as of the runtime's typedEpoch with an element at the index in
// idx, or exits. rD is its scratch.
func (g *armGen) typedView(obj, idx Reg) {
	a := g.a
	a.LoadU8(rD, obj, offObjClass)
	a.CmpImm(rD, int64(ClassTypedArray), false)
	a.BCond(NE, g.exit)
	a.LoadU16(rD, obj, offObjFlags)
	a.TstImm(rD, uint64(objHasData), false)
	a.BCond(EQ, g.exit)
	a.Load(r8, obj, sizeObj+offExtData)
	g.cmpMem(r8, rCtx, ncTypedT)
	a.BCond(NE, g.exit)
	a.Load(r8, obj, sizeObj+offExtData+8)
	a.Load(rD, rCtx, ncTypedEp)
	a.Load(rD, rD, 0)
	g.cmpMem(rD, r8, offTEpoch)
	a.BCond(NE, g.exit)
	g.cmpMem(idx, r8, offTLength)
	a.BCond(HS, g.exit)
}

// typedRead puts in D0 the element at the index in idx of the typed array
// in obj, as amd64's; a BigInt kind's goes to big, its address in r8 and
// its kind in rD; else it exits. r8, r9 and rD are its scratch.
func (g *armGen) typedRead(obj, idx Reg, big *Label) {
	a := g.a
	g.typedView(obj, idx)
	a.LoadU8(rD, r8, offTKind)
	a.Load(r8, r8, offTData)
	done := a.NewLabel()
	type load struct {
		kinds []elemType
		shift uint8
		emit  func()
	}
	for _, l := range []load{
		{[]elemType{elemFloat64}, 3, func() { a.LoadF(0, r8, 0); g.canon(0) }},
		{[]elemType{elemInt32}, 2, func() { a.LoadS32(r9, r8, 0); a.Scvtf(0, r9, true) }},
		{[]elemType{elemUint8, elemUint8Clamped}, 0, func() { a.LoadU8(r9, r8, 0); a.Scvtf(0, r9, true) }},
		{[]elemType{elemFloat32}, 2, func() {
			a.LoadS(0, r8, 0)
			a.FCvtFromSingle(0, 0)
			g.canon(0)
		}},
		{[]elemType{elemUint32}, 2, func() { a.LoadU32(r9, r8, 0); a.Scvtf(0, r9, true) }},
		{[]elemType{elemInt16}, 1, func() { a.LoadS16(r9, r8, 0); a.Scvtf(0, r9, true) }},
		{[]elemType{elemUint16}, 1, func() { a.LoadU16(r9, r8, 0); a.Scvtf(0, r9, true) }},
		{[]elemType{elemInt8}, 0, func() { a.LoadS8(r9, r8, 0); a.Scvtf(0, r9, true) }},
	} {
		this, next := a.NewLabel(), a.NewLabel()
		for i, k := range l.kinds {
			a.CmpImm(rD, int64(k), false)
			if i == len(l.kinds)-1 {
				a.BCond(NE, next)
			} else {
				a.BCond(EQ, this)
			}
		}
		a.Bind(this)
		a.AddLsl(r8, r8, idx, l.shift)
		l.emit()
		a.B(done)
		a.Bind(next)
	}
	if big != nil {
		// A BigInt kind: big, the element's address in r8, the kind in rD.
		notBig := a.NewLabel()
		a.CmpImm(rD, int64(elemBigInt64), false)
		a.BCond(LO, notBig)
		a.CmpImm(rD, int64(elemBigUint64), false)
		a.BCond(HI, notBig)
		a.AddLsl(r8, r8, idx, 3)
		a.B(*big)
		a.Bind(notBig)
	}
	a.B(g.exit)
	a.Bind(done)
}

// typedBigRead stores the element at r8 of a BigInt64Array or a
// BigUint64Array, its kind in rD, to register A, a BigInt held in its
// Value.
func (g *armGen) typedBigRead() {
	a := g.a
	unsigned, enc := a.NewLabel(), a.NewLabel()
	a.Load(r9, r8, 0)
	a.CmpImm(rD, int64(elemBigUint64), false)
	a.BCond(EQ, unsigned)
	a.ShiftImm(Asr, r8, r9, 63, true)
	a.B(enc)
	a.Bind(unsigned)
	a.MovImm(r8, 0)
	a.Bind(enc)
	g.bigStoreA()
}

// typedWrite stores the number in D1 as the element at the index in idx
// of the typed array in obj, as regTypedStore does: a float kind's any
// number, an integer kind's but Uint8ClampedArray's ToInt32 of it wrapped
// to its width where it is below 2^63 in magnitude, NaN's 0; else it
// exits. r8, r9 and rD are its scratch.
func (g *armGen) typedWrite(obj, idx Reg) {
	a := g.a
	g.typedView(obj, idx)
	a.LoadU8(rD, r8, offTReadonly)
	a.Cbnz(rD, g.exit, false)
	a.LoadU8(rD, r8, offTKind)
	a.Load(r8, r8, offTData)
	done := a.NewLabel()
	notF64, notF32 := a.NewLabel(), a.NewLabel()
	a.CmpImm(rD, int64(elemFloat64), false)
	a.BCond(NE, notF64)
	a.AddLsl(r8, r8, idx, 3)
	a.StoreF(r8, 0, 1)
	a.B(done)
	a.Bind(notF64)
	a.CmpImm(rD, int64(elemFloat32), false)
	a.BCond(NE, notF32)
	a.FCvtToSingle(3, 1)
	a.AddLsl(r8, r8, idx, 2)
	a.StoreS(r8, 0, 3)
	a.B(done)
	a.Bind(notF32)
	// An integer kind: the number's int64, a limit where it has none.
	a.Fcvtzs(r9, 1)
	type store struct {
		kinds []elemType
		shift uint8
		emit  func()
	}
	for _, st := range []store{
		{[]elemType{elemInt32, elemUint32}, 2, func() { a.Store32(r8, 0, r9) }},
		{[]elemType{elemUint8, elemInt8}, 0, func() { a.Store8(r8, 0, r9) }},
		{[]elemType{elemInt16, elemUint16}, 1, func() { a.Store16(r8, 0, r9) }},
	} {
		this, next := a.NewLabel(), a.NewLabel()
		a.CmpImm(rD, int64(st.kinds[0]), false)
		a.BCond(EQ, this)
		a.CmpImm(rD, int64(st.kinds[1]), false)
		a.BCond(NE, next)
		a.Bind(this)
		g.noInt64(r9)
		a.AddLsl(r8, r8, idx, st.shift)
		st.emit()
		a.B(done)
		a.Bind(next)
	}
	// Uint8ClampedArray's ToUint8Clamp: NaN and what is not above 0 are
	// 0, what is not below 255 255, else the nearest integer, ties to the
	// even one (FRINTN).
	zero, top, put := a.NewLabel(), a.NewLabel(), a.NewLabel()
	a.CmpImm(rD, int64(elemUint8Clamped), false)
	a.BCond(NE, g.exit)
	a.Op(Add, r8, r8, idx, true)
	a.FMovToF(3, ZR)
	a.FCmp(1, 3)
	a.BCond(LE, zero)
	a.MovImm(r9, 255)
	a.Scvtf(3, r9, true)
	a.FCmp(1, 3)
	a.BCond(GE, top)
	a.FRint(FRintN, 0, 1)
	a.Fcvtzs(r9, 0)
	a.B(put)
	a.Bind(zero)
	a.MovImm(r9, 0)
	a.B(put)
	a.Bind(top)
	a.MovImm(r9, 255)
	a.Bind(put)
	a.Store8(r8, 0, r9)
	a.Bind(done)
}

// arrayAppend is an element write at the index in idx, the length, of
// the array in obj with room for one more, as amd64's; the value register
// C's. An object that is no array goes to notArray; a full array,
// anything else exits.
func (g *armGen) arrayAppend(obj, idx Reg, notArray Label) {
	a := g.a
	g.appendRoom(obj, idx, notArray, g.exit, false)
	// The value at elems[len], then the length past it.
	a.ShiftImm(Lsl, rW, idx, 4, true)
	g.addMem(rW, obj, sizeObj)
	a.AddImm(r9, idx, 1, true)
	a.Store(obj, sizeObj+8, r9)
	a.Load(rC, rIP, 0)
	a.ShiftImm(Lsr, rC, rC, 48, true)
	a.AddLsl(rC, rRegs, rC, 4)
	g.loadValue(rC)
	g.storeValue()
}

// appendRoom is amd64's (amd64.go): the checks of an element appended at
// idx, the array's length, which it loads where atLen is set. rD and r8
// are its scratch.
func (g *armGen) appendRoom(obj, idx Reg, notArray, miss Label, atLen bool) {
	a := g.a
	loop, done := a.NewLabel(), a.NewLabel()
	a.LoadU8(rD, obj, offObjClass)
	a.CmpImm(rD, int64(ClassArray), false)
	a.BCond(NE, notArray)
	want := objExtensible | objArrayLengthWritable | objHasExt
	a.LoadU16(rD, obj, offObjFlags)
	a.AndImm(rD, rD, uint64(want|objHasSparseElements|objMappedArguments|objSharedElems|objPolyProto), false)
	a.CmpImm(rD, int64(want), false)
	a.BCond(NE, miss)
	if atLen {
		a.Load(idx, obj, sizeObj+8)
	} else {
		g.cmpMem(idx, obj, sizeObj+8)
		a.BCond(NE, miss)
	}
	g.cmpMem(idx, obj, sizeObj+16)
	a.BCond(HS, miss)
	g.noIndexKeys(obj)
	// The chain: ordinary objects and arrays of no elements, no index
	// keys, no prototype of their own word.
	a.MovRR(r8, obj)
	a.Bind(loop)
	a.Load(r8, r8, offObjShape)
	a.Cbz(r8, done, true)
	a.Load(r8, r8, offShapeProto)
	a.Cbz(r8, done, true)
	a.LoadU8(rD, r8, offObjClass)
	ok := a.NewLabel()
	a.CmpImm(rD, int64(ClassObject), false)
	a.BCond(EQ, ok)
	a.CmpImm(rD, int64(ClassArray), false)
	a.BCond(NE, miss)
	a.Bind(ok)
	a.LoadU16(rD, r8, offObjFlags)
	a.TstImm(rD, uint64(objPolyProto), false)
	a.BCond(NE, miss)
	noElems := a.NewLabel()
	a.TstImm(rD, uint64(objHasExt), false)
	a.BCond(EQ, noElems)
	g.cmpMemImm(r8, sizeObj+8, 0)
	a.BCond(NE, miss)
	a.Bind(noElems)
	g.noIndexKeys(r8)
	a.B(loop)
	a.Bind(done)
}

// noIndexKeys exits unless the object in obj has no key that is an array
// index, as Object.noIndexKeys says. rD is its scratch.
func (g *armGen) noIndexKeys(obj Reg) {
	a := g.a
	noIdx, ok := a.NewLabel(), a.NewLabel()
	a.Load(rD, obj, offObjShape)
	a.Cbz(rD, noIdx, true)
	a.Load(rD, rD, offShapeIndex)
	a.Cbz(rD, noIdx, true)
	a.LoadU8(rD, rD, offIdxIndexKeys)
	a.Cbnz(rD, g.exit, false)
	a.B(ok)
	a.Bind(noIdx)
	a.LoadU32(rD, obj, off("Object.valn"))
	a.Cbnz(rD, g.exit, false)
	a.Bind(ok)
}

// flatEq goes to eq or ne as the strings at r9 and r8, of one length, are
// equal or not, where both are flat, as amd64's; a rope exits. rW is
// read again before either.
func (g *armGen) flatEq(eq, ne Label) {
	a := g.a
	loop, tail, same, differ := a.NewLabel(), a.NewLabel(), a.NewLabel(), a.NewLabel()
	g.cmpMemImm(r9, offStrLeft, 0)
	a.BCond(NE, g.exit)
	g.cmpMemImm(r8, offStrLeft, 0)
	a.BCond(NE, g.exit)
	a.Load(rC, r9, offStrS+8)
	g.cmpMem(rC, r8, offStrS+8)
	a.BCond(NE, differ)
	// A long text is Go's, as on amd64 (TestSlowTurnsAreInterruptible).
	a.CmpImm(rC, nativeFlatEqMax, true)
	a.BCond(HI, g.exit)
	a.Load(rS, r9, offStrS)
	a.Load(rD, r8, offStrS)
	a.Bind(loop)
	a.CmpImm(rC, 8, true)
	a.BCond(LO, tail)
	a.Load(rW, rS, 0)
	g.cmpMem(rW, rD, 0)
	a.BCond(NE, differ)
	a.AddImm(rS, rS, 8, true)
	a.AddImm(rD, rD, 8, true)
	a.AddImm(rC, rC, -8, true)
	a.B(loop)
	a.Bind(tail)
	a.Cbz(rC, same, true)
	a.LoadU8(rW, rS, 0)
	a.LoadU8(r9, rD, 0)
	a.Cmp(rW, r9, false)
	a.BCond(NE, differ)
	a.AddImm(rS, rS, 1, true)
	a.AddImm(rD, rD, 1, true)
	a.AddImm(rC, rC, -1, true)
	a.B(tail)
	a.Bind(same)
	a.Load(rW, rIP, 0)
	a.B(eq)
	a.Bind(differ)
	a.Load(rW, rIP, 0)
	a.B(ne)
}

// typedBigWrite stores register C, a BigInt held in its Value, as the
// element at the index in idx of the BigInt64Array or BigUint64Array in
// obj, as amd64's; anything else exits.
func (g *armGen) typedBigWrite(obj, idx Reg) {
	a := g.a
	a.Store(rCtx, scr(sI), idx)
	g.regC(r9)
	g.bigShort(r9, r8, r9, true)
	a.Store(rCtx, scr(sV0), r9)
	a.Load(idx, rCtx, scr(sI))
	g.typedView(obj, idx)
	a.LoadU8(rD, r8, offTReadonly)
	a.Cbnz(rD, g.exit, false)
	a.LoadU8(rD, r8, offTKind)
	a.CmpImm(rD, int64(elemBigInt64), false)
	a.BCond(LO, g.exit)
	a.CmpImm(rD, int64(elemBigUint64), false)
	a.BCond(HI, g.exit)
	a.Load(r8, r8, offTData)
	a.AddLsl(r8, r8, idx, 3)
	a.Load(r9, rCtx, scr(sV0))
	a.Store(r8, 0, r9)
}

// ptrStore stores rC, a pointer word, at [rW]: plainly while the
// collector does not mark, and by nativeStoreRef while it does, the slot
// in R0 and the pointer in R1, its way back in R16. Every scratch
// register is gone after it; on the barrier's path the pinned ones are
// put back from the context.
func (g *armGen) ptrStore() {
	a := g.a
	barrier, cont, done := a.NewLabel(), a.NewLabel(), a.NewLabel()
	a.Load(rS, rCtx, ncWB)
	a.LoadU8(rS, rS, 0)
	a.Cbnz(rS, barrier, false)
	a.Store(rW, 0, rC)
	a.B(done)
	a.Bind(barrier)
	a.Store(rCtx, ncIP, rIP)
	a.Store(rCtx, ncBudget, rBud)
	a.Adr(rJ, cont)
	a.Load(rK, rCtx, ncStore)
	a.Br(rK)
	a.Bind(cont)
	g.reload()
	a.Bind(done)
}

// reload puts the pinned registers back from the context.
func (g *armGen) reload() {
	a := g.a
	a.Load(rIP, rCtx, ncIP)
	a.Load(rRegs, rCtx, ncRegs)
	a.Load(rTab, rCtx, ncTable)
	a.Load(rIC, rCtx, ncIC)
	a.Load(rBud, rCtx, ncBudget)
	a.Load(rTag, rCtx, ncTagBase)
}

// tableAddrARM64 is arm64's func name() uintptr returning sym's address.
func tableAddrARM64(name, sym string) string {
	return fmt.Sprintf("TEXT ·%s(SB), NOSPLIT, $0-8\n\tMOVD $·%s(SB), R0\n\tMOVD R0, ret+0(FP)\n\tRET\n", name, sym)
}

// typeIndex is amd64's: the index of typeof's type in rC, of the Value at
// the address in addr. r8, r9 and rD are its scratch.
func (g *armGen) typeIndex(addr Reg) {
	a := g.a
	idx, obj, num, big, notFn := a.NewLabel(), a.NewLabel(), a.NewLabel(), a.NewLabel(), a.NewLabel()
	a.Load(r9, addr, 0)
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
	a.Load(r8, addr, 8)
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
}
