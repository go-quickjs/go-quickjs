package nativegen

import (
	"github.com/go-quickjs/go-quickjs/internal/bytecode"
	. "github.com/go-quickjs/go-quickjs/internal/jit/asm/amd64"
)

// A for-of's step over a generator of register code, and its yield, done
// here (docs/register-interpreter.md, "Generators and async functions"):
// the iter_next handler enters the generator's frame as regGenStep and
// regGenRun enter it -- next's frame and the generator's pushed, the
// generator's window copied to the stack's top, the caller's state on the
// native frame stack, its kind regFrameGenStep -- and goes on in its code
// at the yield's continuation; the yield handler suspends it as regGenRun
// does -- the window copied back, the frames popped -- and goes on in the
// caller past iter_next with the value. Go keeps the rest: a first step, a
// generator suspended elsewhere than at a yield, one with captures kept or
// arguments, a return, a throw, a yield with captures open, yield*; a
// frame the code entered that Go ends, nativeGenEnd ends.

// genPtr stores the pointer word load puts in RCX at the field off of the
// object whose address is scratch word base, where it differs (ptrTo).
func (g *nativeGen) genPtr(base int, off int32, load func()) {
	a := g.a
	load()
	a.Load(RAX, RDI, scr(base))
	a.Lea(RAX, RAX, off)
	g.ptrTo()
}

// genNil clears the pointer word at off of the object at scratch word
// base, and the words after it, n in all (a slice's length and capacity,
// a string's length).
func (g *nativeGen) genNil(base int, off int32, n int) {
	a := g.a
	g.genPtr(base, off, func() { a.Op(Xor, RCX, RCX, true) })
	if n > 1 {
		a.Load(RSI, RDI, scr(base))
		for i := 1; i < n; i++ {
			a.StoreImm(RSI, off+int32(8*i), 0)
		}
	}
}

// genCopy copies the Values from the address in scratch word src to the
// one in dst, count of them in scratch word sEnd: each number word
// plainly, each pointer word where it differs (ptrTo).
func (g *nativeGen) genCopy(dst, src int) {
	a := g.a
	loop, done := a.NewLabel(), a.NewLabel()
	a.StoreImm(RDI, scr(sI), 0)
	a.Bind(loop)
	a.Load(RCX, RDI, scr(sI))
	a.CmpMem(RCX, RDI, scr(sEnd), true)
	a.Jcc(CondGE, done)
	a.ShiftImm(Shl, RCX, 4, true)
	a.Load(RDX, RDI, scr(src))
	a.Op(Add, RDX, RCX, true)
	a.Load(RAX, RDI, scr(dst))
	a.Op(Add, RAX, RCX, true)
	a.Load(R8, RDX, 0)
	a.Store(RAX, 0, R8)
	a.Load(RCX, RDX, 8)
	a.Lea(RAX, RAX, 8)
	g.ptrTo()
	a.Load(RCX, RDI, scr(sI))
	a.OpImm(Add, RCX, 1, true)
	a.Store(RDI, scr(sI), RCX)
	a.Jmp(loop)
	a.Bind(done)
}

// genStep is iter_next's step of a generator's cursor, the cursor in RSI,
// the instruction in RAX; anything it does not answer exits before
// anything is written.
func (g *nativeGen) genStep() {
	a := g.a
	realmOK := a.NewLabel()
	// Scratch: sE the generator, sV0 next, sRC its code, sKind the pc it
	// goes on at, sKa the register a resumption by next sends undefined
	// to, sBase the window's base, sNF the generator's frame, sV1 where
	// the value goes, sEnt genFrom.
	g.decA(RDX)
	a.Store(RDI, scr(sV1), RDX)
	// Not a for-in's (forInStep), and stepped by the realm's next.
	forIn := a.NewLabel()
	a.LoadU8(RCX, RSI, offItForIn)
	a.Op(Test, RCX, RCX, false)
	a.Jcc(CondNE, forIn)
	a.Load(RDX, RDI, ncRT)
	a.Load(RDX, RDX, rtRel(0))
	a.Load(RDX, RDX, offRealmNext)
	a.Op(Test, RDX, RDX, true)
	a.Jcc(CondE, g.exit)
	a.CmpMem(RDX, RSI, offItNext+8, true)
	a.Jcc(CondNE, g.exit)
	// next's name, which its frame shows: a function's data.
	a.LoadU16(RCX, RDX, offObjFlags)
	a.OpImm(And, RCX, int32(objHasData), false)
	a.Jcc(CondE, g.exit)
	a.Load(RCX, RDX, sizeObj+offExtData)
	a.CmpMem(RCX, RDI, ncFuncT, true)
	a.Jcc(CondNE, g.exit)
	a.Store(RDI, scr(sV0), RDX)
	// A generator of register code suspended at a yield, with nothing
	// kept but its window: no captures, no arguments, nothing rare.
	a.Load(R9, RSI, offItIter)
	a.CmpMem(R9, RDI, ncObjBits, true)
	a.Jcc(CondNE, g.exit)
	a.Load(R8, RSI, offItIter+8)
	a.Bind(g.genObj)
	a.LoadU8(RCX, R8, offObjClass)
	a.OpImm(Cmp, RCX, int32(ClassGenerator), false)
	a.Jcc(CondNE, g.exit)
	a.Lea(R8, R8, offGen)
	a.LoadU8(RCX, R8, offGReg)
	a.Op(Test, RCX, RCX, false)
	a.Jcc(CondE, g.exit)
	a.LoadU8(RCX, R8, offGState)
	a.OpImm(Cmp, RCX, genSuspendedYield, false)
	a.Jcc(CondA, g.exit)
	a.LoadU8(RCX, R8, offGAwaitJob)
	a.Op(Test, RCX, RCX, false)
	a.Jcc(CondNE, g.exit)
	a.CmpMemImm8(R8, offGSaved, 0)
	a.Jcc(CondNE, g.exit)
	a.CmpMemImm8(R8, offGRare, 0)
	a.Jcc(CondNE, g.exit)
	a.LoadU32(RCX, R8, offGNargs)
	a.Op(Test, RCX, RCX, false)
	a.Jcc(CondNE, g.exit)
	a.Store(RDI, scr(sE), R8)
	// Of this realm, or of none.
	a.Load(R9, R8, offGCl)
	a.Load(RCX, R9, offClRealm)
	a.Op(Test, RCX, RCX, true)
	a.Jcc(CondE, realmOK)
	a.Load(RDX, RDI, ncRT)
	a.CmpMem(RCX, RDX, rtRel(0), true)
	a.Jcc(CondNE, g.exit)
	a.Bind(realmOK)
	// Its register code, which keeps no arguments.
	a.Load(RDX, R9, offClCode)
	a.AddMem(RDX, RDI, ncCCBase)
	a.Load(RCX, RDX, offCCReg)
	a.Op(Test, RCX, RCX, true)
	a.Jcc(CondE, g.exit)
	a.LoadU8(RDX, RCX, offRCArgs)
	a.Op(Test, RDX, RDX, false)
	a.Jcc(CondNE, g.exit)
	a.Store(RDI, scr(sRC), RCX)
	// Its pc: past a yield, at the jump past the yield's return path,
	// which a resumption by next takes; or past end_params, its body's
	// first step, which sends nothing (regGenResume).
	first, pcOK := a.NewLabel(), a.NewLabel()
	a.LoadU32(RDX, R8, offGPC)
	a.Op(Test, RDX, RDX, false)
	a.Jcc(CondE, g.exit)
	a.Load(RSI, RCX, offRCCode)
	a.LoadIdx(R9, RSI, RDX, 8, -8)
	a.MovZX8(R8, R9)
	a.OpImm(Cmp, R8, int32(bytecode.REndParams), false)
	a.Jcc(CondE, first)
	a.OpImm(Cmp, R8, int32(bytecode.RYield), false)
	a.Jcc(CondNE, g.exit)
	// The yield's register, less genFrom, in vals.
	a.ShiftImm(Shr, R9, 16, true)
	a.OpImm(And, R9, 0xFFFF, false)
	a.LoadS32(R8, RCX, offRCGenFrom)
	a.Store(RDI, scr(sEnt), R8)
	a.Op(Sub, R9, R8, true)
	a.Store(RDI, scr(sKa), R9)
	a.LoadIdx(R9, RSI, RDX, 8, 0)
	a.MovZX8(R8, R9)
	a.OpImm(Cmp, R8, int32(bytecode.RJmp), false)
	a.Jcc(CondNE, g.exit)
	a.ShiftImm(Sar, R9, 32, true)
	a.Op(Add, R9, RDX, true)
	a.OpImm(Add, R9, 1, true)
	a.Store(RDI, scr(sKind), R9)
	a.Jmp(pcOK)
	a.Bind(first)
	a.Store(RDI, scr(sKind), RDX)
	a.StoreImm(RDI, scr(sKa), -1)
	a.LoadS32(R8, RCX, offRCGenFrom)
	a.Store(RDI, scr(sEnt), R8)
	a.Bind(pcOK)
	// The window at the stack's top, with room.
	a.Load(R9, RDI, ncRT)
	a.Load(RDX, R9, offRtStackTop)
	a.Store(RDI, scr(sBase), RDX)
	a.LoadS32(R8, RCX, offRCNumRegs)
	a.Op(Add, R8, RDX, true)
	a.CmpMem(R8, R9, offRtStack+8, true)
	a.Jcc(CondA, g.exit)
	// The depth for next's frame and the generator's, the interrupt
	// budget, both frame records in the block at hand.
	a.Load(RDX, R9, offRtFrameDepth)
	a.Lea(RCX, RDX, 1)
	a.CmpMem(RCX, R9, offRtMaxFrames, true)
	a.Jcc(CondGE, g.exit)
	a.CmpMemImm8(R9, offRtInterrupt, 1)
	a.Jcc(CondLE, g.exit)
	a.Load(R8, R9, offRtFrameBase)
	a.Op(Sub, RDX, R8, true)
	a.Jcc(CondS, g.exit)
	a.Lea(RCX, RDX, 1)
	a.CmpMem(RCX, R9, offRtCur+8, true)
	a.Jcc(CondAE, g.exit)
	a.MovImm(R8, uint64(sizeFrame))
	a.Imul(RCX, R8)
	a.AddMem(RCX, R9, offRtCur)
	a.Store(RDI, scr(sNF), RCX)
	// Room on the native frame stack: Go grows it.
	a.Load(R8, RDI, ncDepth)
	a.CmpMem(R8, RDI, ncCap, true)
	a.Jcc(CondGE, g.exitRet)

	// Nothing is written before here.
	//
	// The interrupt budget, the depth, the stack top.
	a.DecMem(R9, offRtInterrupt)
	a.Load(RCX, R9, offRtFrameDepth)
	a.OpImm(Add, RCX, 2, true)
	a.Store(R9, offRtFrameDepth, RCX)
	high, top := a.NewLabel(), a.NewLabel()
	a.CmpMem(RCX, R9, offRtFrameHigh, true)
	a.Jcc(CondLE, high)
	a.Store(R9, offRtFrameHigh, RCX)
	a.Bind(high)
	a.Load(R8, RDI, scr(sRC))
	a.LoadS32(RCX, R8, offRCNumRegs)
	a.AddMem(RCX, RDI, scr(sBase))
	a.Store(R9, offRtStackTop, RCX)
	a.CmpMem(RCX, R9, offRtStackHigh, true)
	a.Jcc(CondLE, top)
	a.Store(R9, offRtStackHigh, RCX)
	a.Bind(top)
	// The generator executing, undefined sent to its yield's register.
	sent := a.NewLabel()
	a.Load(R8, RDI, scr(sE))
	a.MovImm(RCX, uint64(genExecuting))
	a.Store8(R8, offGState, RCX)
	a.Load(RCX, RDI, scr(sKa))
	a.Op(Test, RCX, RCX, true)
	a.Jcc(CondS, sent)
	a.ShiftImm(Shl, RCX, 4, true)
	a.AddMem(RCX, R8, offGVals)
	a.Load(RDX, RDI, ncUndef)
	a.Store(RCX, 0, RDX)
	a.Lea(RAX, RCX, 8)
	a.Op(Xor, RCX, RCX, true)
	g.ptrTo()
	a.Bind(sent)
	// next's frame record, as regGenStep writes it: no closure, next's
	// name, the cursor's iterator its this, undefined new.target, next
	// its callee, no arguments, no handlers or captures.
	a.Load(RCX, RDI, scr(sNF))
	a.OpImm(Sub, RCX, sizeFrame, true)
	a.Store(RDI, scr(sSaved), RCX)
	g.genNil(sSaved, offFCl, 1)
	g.genPtr(sSaved, offFNative, func() {
		a.Load(RCX, RDI, scr(sV0))
		a.Load(RCX, RCX, sizeObj+offExtData+8)
		a.Load(RDX, RCX, offFDName+8)
		a.Load(RCX, RCX, offFDName)
		a.Load(RSI, RDI, scr(sSaved))
		a.Store(RSI, offFNative+8, RDX)
	})
	g.genPtr(sSaved, offFThis+8, func() {
		a.Load(RSI, RDI, scr(sSaved))
		a.Load(RCX, RDI, ncObjBits)
		a.Store(RSI, offFThis, RCX)
		a.Load(RCX, RDI, scr(sE))
		a.OpImm(Sub, RCX, offGen, true)
	})
	g.genPtr(sSaved, offFNewTgt+8, func() {
		a.Load(RSI, RDI, scr(sSaved))
		a.Load(RCX, RDI, ncUndef)
		a.Store(RSI, offFNewTgt, RCX)
		a.Op(Xor, RCX, RCX, true)
	})
	g.genPtr(sSaved, offFCallee, func() { a.Load(RCX, RDI, scr(sV0)) })
	g.genNil(sSaved, offFArgs, 3)
	a.Load(RSI, RDI, scr(sSaved))
	a.StoreImm(RSI, offFHandlers+8, 0)
	a.StoreImm(RSI, offFOpenUp+8, 0)
	// The generator's, as regGenRun writes it: its closure, its base, pc
	// 0, its this and callee, nothing kept, nothing rare; its kind the
	// step's.
	a.Load(RSI, RDI, scr(sNF))
	a.Load(RCX, RDI, scr(sBase))
	a.Store(RSI, offFBase, RCX)
	a.StoreImm(RSI, offFSavedSP, 0)
	a.Op(Xor, RCX, RCX, false)
	a.Store32(RSI, offFPC, RCX)
	a.Store8(RSI, offFParams, RCX)
	a.MovImm(RCX, uint64(regFrameGenStep))
	a.Store8(RSI, offFRegKind, RCX)
	g.genPtr(sNF, offFCl, func() {
		a.Load(RCX, RDI, scr(sE))
		a.Load(RCX, RCX, offGCl)
	})
	g.genPtr(sNF, offFThis+8, func() {
		a.Load(RDX, RDI, scr(sE))
		a.Load(RSI, RDI, scr(sNF))
		a.Load(RCX, RDX, offGThis)
		a.Store(RSI, offFThis, RCX)
		a.Load(RCX, RDX, offGThis+8)
	})
	g.genPtr(sNF, offFNewTgt+8, func() {
		a.Load(RSI, RDI, scr(sNF))
		a.Load(RCX, RDI, ncUndef)
		a.Store(RSI, offFNewTgt, RCX)
		a.Op(Xor, RCX, RCX, true)
	})
	g.genPtr(sNF, offFCallee, func() {
		a.Load(RCX, RDI, scr(sE))
		a.Load(RCX, RCX, offGCallee)
	})
	g.genNil(sNF, offFLocals, 3)
	g.genNil(sNF, offFArgs, 3)
	g.genNil(sNF, offFOpenUp, 3)
	g.genNil(sNF, offFHandlers, 3)
	g.genNil(sNF, offFThisRef, 1)
	g.genNil(sNF, offFEvalVars, 1)
	g.genNil(sNF, offFWith, 3)
	g.genNil(sNF, offFNative, 2)
	// The window: vals to the registers from genFrom; below it, the
	// callee's register and this's, which a resumption makes again.
	a.Load(R9, RDI, ncRT)
	a.Load(RCX, RDI, scr(sBase))
	a.ShiftImm(Shl, RCX, 4, true)
	a.AddMem(RCX, R9, offRtStack)
	a.Store(RDI, scr(sWin), RCX)
	a.Load(R8, RDI, scr(sRC))
	a.LoadS32(RDX, R8, offRCNumRegs)
	a.Load(R9, RDI, scr(sEnt))
	a.Op(Sub, RDX, R9, true)
	a.Store(RDI, scr(sEnd), RDX)
	a.ShiftImm(Shl, R9, 4, true)
	a.Op(Add, RCX, R9, true)
	a.Store(RDI, scr(sTop), RCX)
	a.Load(R8, RDI, scr(sE))
	a.Load(RCX, R8, offGVals)
	a.Store(RDI, scr(sArgc), RCX)
	g.genCopy(sTop, sArgc)
	whole, noCallee := a.NewLabel(), a.NewLabel()
	a.CmpMemImm8(RDI, scr(sEnt), 0)
	a.Jcc(CondE, whole)
	g.genPtr(sWin, 8, func() {
		a.Load(RDX, RDI, scr(sE))
		a.Load(RSI, RDI, scr(sWin))
		a.Load(RCX, RDX, offGCallee)
		a.Load(R8, RDI, ncObjBits)
		a.Op(Test, RCX, RCX, true)
		a.Jcc(CondNE, noCallee)
		a.Load(R8, RDI, ncUndef)
		a.Bind(noCallee)
		a.Store(RSI, 0, R8)
	})
	g.genPtr(sWin, 24, func() {
		a.Load(RDX, RDI, scr(sE))
		a.Load(RSI, RDI, scr(sWin))
		a.Load(RCX, RDX, offGThis)
		a.Store(RSI, 16, RCX)
		a.Load(RCX, RDX, offGThis+8)
	})
	a.Bind(whole)
	// The caller's state on the native frame stack, the value to go to
	// its register A, the generator kept for the yield.
	a.Load(R8, RDI, ncDepth)
	a.MovImm(R9, uint64(recSize))
	a.Imul(R8, R9)
	a.AddMem(R8, RDI, ncFrames)
	a.MovRR(RSI, R8)
	a.Store(RSI, recRetIP, RBX)
	for _, f := range nativeTopFields {
		a.Load(RCX, RDI, f[1])
		a.Store(RSI, f[0], RCX)
	}
	a.Load(RCX, RDI, scr(sBase))
	a.Store(RSI, recSaved, RCX)
	a.Load(RCX, RDI, scr(sV1))
	a.Store(RSI, recBOff, RCX)
	a.Load(RCX, RDI, scr(sE))
	a.Store(RSI, recGen, RCX)
	a.IncMem(RDI, ncDepth)
	a.StoreImm(RDI, ncTopOK, 0)
	// The generator's frame the top: its window, its closure's caches,
	// constants, epochs and call sites, its code and frame record; on at
	// the continuation.
	a.Load(R12, RDI, scr(sWin))
	a.Store(RDI, ncRegs, R12)
	a.Load(RCX, RDI, scr(sE))
	a.Load(R8, RCX, offGCl)
	a.Load(R8, R8, offClCode)
	a.AddMem(R8, RDI, ncCCBase)
	a.Store(RDI, ncCC, R8)
	a.Load(R15, R8, offCCIC)
	a.Store(RDI, ncIC, R15)
	a.Load(R8, RDI, scr(sRC))
	a.Store(RDI, ncRC, R8)
	a.Load(RBX, R8, offRCCode)
	a.Store(RDI, ncCodeBase, RBX)
	a.Load(RCX, RDI, scr(sNF))
	a.Store(RDI, ncNF, RCX)
	a.StoreImm(RDI, ncFV, 0)
	a.Load(RCX, RDI, scr(sKind))
	a.LeaIdx(RBX, RBX, RCX, 8, 0)
	g.dispatch()
	g.late = append(g.late, func() {
		a.Bind(forIn)
		g.forInStep()
	})
}

// genNext is call_method_w of the realm's generator next with no
// argument, its site's cached built-in (RCX the cache's entry, RSI the
// window, RAX the instruction): the generator its receiver stepped as
// iter_next steps a cursor's (genStep, from genObj), the call's register B
// where the value goes, and the step's yield and return making next's
// result object of it (iterResult) by nativeIterRes, as next makes it. A
// step genStep does not take, any other call, goes to miss.
func (g *nativeGen) genNext(miss Label) {
	a := g.a
	a.MovZX8(RDX, RAX)
	a.OpImm(Cmp, RDX, int32(bytecode.RCallMW), false)
	a.Jcc(CondNE, miss)
	a.MovRR(RDX, RAX)
	a.ShiftImm(Shr, RDX, 8, true)
	a.MovZX8(RDX, RDX)
	a.Op(Test, RDX, RDX, false)
	a.Jcc(CondNE, miss)
	a.Load(R9, RCX, offECallee)
	a.Load(RDX, RDI, ncRT)
	a.Load(RDX, RDX, rtRel(0))
	a.CmpMem(R9, RDX, offRealmNext, true)
	a.Jcc(CondNE, miss)
	a.Load(R8, RSI, 16)
	a.CmpMem(R8, RDI, ncObjBits, true)
	a.Jcc(CondNE, miss)
	a.Load(R8, RSI, 24)
	a.Store(RDI, scr(sV0), R9)
	g.decB(RDX)
	a.Store(RDI, scr(sV1), RDX)
	a.Jmp(g.genObj)
}

// genWrap makes the value a step that next() entered gave to its caller's
// register B next's result object, done or not, where the instruction
// before RBX, the caller's call, is call_method_w (genNext's); RBX is
// where the caller goes on, written to the context. Then on.
func (g *nativeGen) genWrap(done bool) {
	a := g.a
	after, on := a.NewLabel(), a.NewLabel()
	a.Load(RAX, RBX, -8)
	a.MovZX8(RCX, RAX)
	a.OpImm(Cmp, RCX, int32(bytecode.RCallMW), false)
	a.Jcc(CondNE, on)
	// nativeIterRes(regs, w, done, ctx) through callH, as litCall calls
	// nativeLit.
	a.Store(RDI, ncBudget, R11)
	a.LeaLabel(R8, after)
	a.Store(RDI, ncAfter, R8)
	a.MovRR(RBX, RAX)
	a.Op(Xor, RCX, RCX, false)
	if done {
		a.MovImm(RCX, 1)
	}
	a.MovRR(RSI, RDI)
	a.MovRR(RAX, R12)
	a.Load(RDX, RDI, ncIterResFn)
	a.JmpMem(RDI, ncCallH)
	a.Bind(after)
	g.reload()
	a.Bind(on)
	g.dispatch()
}

// genYield is yield in a generator's frame genStep entered: the frame
// suspended as regGenRun suspends it -- its pc past the yield, its window
// copied back to vals, next's frame and its own popped -- and the caller
// on past its iter_next with the value. Any other frame's yield, and one
// with captures open on its registers or inside a with, is Go's.
func (g *nativeGen) genYield() {
	a := g.a
	base := a.NewLabel()
	a.CmpMemImm8(RDI, ncDepth, 0)
	a.Jcc(CondE, base)
	a.Load(RSI, RDI, ncNF)
	a.LoadU8(RCX, RSI, offFRegKind)
	a.OpImm(Cmp, RCX, regFrameGenStep, false)
	a.Jcc(CondNE, g.exit)
	a.CmpMemImm8(RSI, offFOpenUp, 0)
	a.Jcc(CondNE, g.exit)
	// The with objects it is inside, which the generator keeps (saveWith).
	a.CmpMemImm8(RSI, offFWith+8, 0)
	a.Jcc(CondNE, g.exit)
	// The value.
	g.decB(R8)
	a.LoadIdx(R9, R12, R8, 1, 0)
	a.Store(RDI, scr(sV0), R9)
	a.LoadIdx(R9, R12, R8, 1, 8)
	a.Store(RDI, scr(sV1), R9)
	// The generator, its pc and state.
	a.Load(R8, RDI, ncDepth)
	a.OpImm(Sub, R8, 1, true)
	a.MovImm(R9, uint64(recSize))
	a.Imul(R8, R9)
	a.AddMem(R8, RDI, ncFrames)
	a.Load(RDX, R8, recGen)
	a.Store(RDI, scr(sE), RDX)
	a.MovRR(RCX, RBX)
	a.Load(R9, RDI, ncCodeBase)
	a.Op(Sub, RCX, R9, true)
	a.ShiftImm(Shr, RCX, 3, true)
	a.OpImm(Add, RCX, 1, false)
	a.Store32(RDX, offGPC, RCX)
	a.MovImm(RCX, uint64(genSuspendedYield))
	a.Store8(RDX, offGState, RCX)
	a.MovImm(RCX, 1)
	a.Store8(RDX, offGStarted, RCX)
	a.Op(Xor, RCX, RCX, false)
	a.Store8(RDX, offGDeleg, RCX)
	// The window from genFrom back to vals.
	a.Load(R8, RDI, ncRC)
	a.LoadS32(RCX, R8, offRCNumRegs)
	a.LoadS32(R9, R8, offRCGenFrom)
	a.Op(Sub, RCX, R9, true)
	a.Store(RDI, scr(sEnd), RCX)
	a.ShiftImm(Shl, R9, 4, true)
	a.Op(Add, R9, R12, true)
	a.Store(RDI, scr(sWin), R9)
	a.Load(RCX, RDX, offGVals)
	a.Store(RDI, scr(sTop), RCX)
	g.genCopy(sTop, sWin)
	// The frame no step's; next's and its own popped, the stack top
	// given back.
	a.Load(RSI, RDI, ncNF)
	a.Op(Xor, RCX, RCX, false)
	a.Store8(RSI, offFRegKind, RCX)
	a.Load(R9, RDI, ncRT)
	a.Load(RCX, R9, offRtFrameDepth)
	a.OpImm(Sub, RCX, 2, true)
	a.Store(R9, offRtFrameDepth, RCX)
	a.Load(R8, RDI, ncDepth)
	a.OpImm(Sub, R8, 1, true)
	a.Store(RDI, ncDepth, R8)
	a.StoreImm(RDI, ncTopOK, 0)
	low := a.NewLabel()
	a.CmpMem(R8, RDI, ncFilled, true)
	a.Jcc(CondGE, low)
	a.Store(RDI, ncFilled, R8)
	a.Bind(low)
	a.MovImm(RCX, uint64(recSize))
	a.Imul(R8, RCX)
	a.AddMem(R8, RDI, ncFrames)
	a.MovRR(RSI, R8)
	a.Load(RCX, RSI, recSaved)
	a.Store(R9, offRtStackTop, RCX)
	for _, f := range nativeTopFields {
		a.Load(RCX, RSI, f[0])
		a.Store(RDI, f[1], RCX)
	}
	// The caller past its iter_next, the value in its register A.
	a.Load(RBX, RSI, recRetIP)
	a.OpImm(Add, RBX, 8, true)
	a.Store(RDI, ncIP, RBX)
	a.Load(R12, RDI, ncRegs)
	a.Load(R15, RDI, ncIC)
	a.MovRR(RAX, R12)
	a.AddMem(RAX, RSI, recBOff)
	a.Load(RCX, RDI, scr(sV0))
	a.Store(RAX, 0, RCX)
	a.Lea(RAX, RAX, 8)
	a.Load(RCX, RDI, scr(sV1))
	g.ptrTo()
	g.genWrap(false)
	// The base frame's, a generator's body Go resumed (regGenRun): its
	// value in the scratch, retDone 2, and nativeRun suspends it, as
	// rhYield and regYield would.
	a.Bind(base)
	g.decB(R8)
	a.LoadIdx(R9, R12, R8, 1, 0)
	a.Store(RDI, scr(sV0), R9)
	a.LoadIdx(R9, R12, R8, 1, 8)
	a.Store(RDI, scr(sV1), R9)
	a.StoreImm(RDI, ncRetDone, 2)
	a.Jmp(g.exitRet)
}

// genReturn is ret and ret_undef in a generator's frame genStep entered:
// the generator done, as regGenRun and regGenNext end it, next's frame
// and its own popped, and the caller's loop left as regGenStep and
// regIterNext leave it -- the cursor done, iter_next's jump taken -- the
// value dropped, as a for-of drops it. A frame with captures open on its
// registers is Go's (nativeGenEnd).
func (g *nativeGen) genReturn() {
	a := g.a
	a.Load(RSI, RDI, ncNF)
	a.CmpMemImm8(RSI, offFOpenUp, 0)
	a.Jcc(CondNE, g.exit)
	// The value, which a step next() entered gives its caller.
	haveVal := a.NewLabel()
	a.Load(R8, RDI, ncUndef)
	a.Op(Xor, R9, R9, true)
	a.MovZX8(RCX, RAX)
	a.OpImm(Cmp, RCX, int32(bytecode.RRetUndef), false)
	a.Jcc(CondE, haveVal)
	g.decA(RDX)
	a.LoadIdx(R8, R12, RDX, 1, 0)
	a.LoadIdx(R9, R12, RDX, 1, 8)
	a.Bind(haveVal)
	a.Store(RDI, scr(sV0), R8)
	a.Store(RDI, scr(sV1), R9)
	a.Op(Xor, RCX, RCX, false)
	a.Store8(RSI, offFRegKind, RCX)
	a.Load(R8, RDI, ncDepth)
	a.OpImm(Sub, R8, 1, true)
	a.Store(RDI, ncDepth, R8)
	a.StoreImm(RDI, ncTopOK, 0)
	low := a.NewLabel()
	a.CmpMem(R8, RDI, ncFilled, true)
	a.Jcc(CondGE, low)
	a.Store(RDI, ncFilled, R8)
	a.Bind(low)
	a.MovImm(RCX, uint64(recSize))
	a.Imul(R8, RCX)
	a.AddMem(R8, RDI, ncFrames)
	a.MovRR(RSI, R8)
	a.Load(RDX, RSI, recGen)
	a.MovImm(RCX, uint64(genCompleted))
	a.Store8(RDX, offGState, RCX)
	a.Load(R9, RDI, ncRT)
	a.Load(RCX, R9, offRtFrameDepth)
	a.OpImm(Sub, RCX, 2, true)
	a.Store(R9, offRtFrameDepth, RCX)
	a.Load(RCX, RSI, recSaved)
	a.Store(R9, offRtStackTop, RCX)
	for _, f := range nativeTopFields {
		a.Load(RCX, RSI, f[0])
		a.Store(RDI, f[1], RCX)
	}
	// The caller at its iter_next: the cursor done, the loop left; at a
	// call of next(), on past it with the value made its result.
	cursor := a.NewLabel()
	a.Load(RBX, RSI, recRetIP)
	a.Load(R12, RDI, ncRegs)
	a.Load(R15, RDI, ncIC)
	a.Load(RAX, RBX, 0)
	a.MovZX8(RCX, RAX)
	a.OpImm(Cmp, RCX, int32(bytecode.RCallMW), false)
	a.Jcc(CondNE, cursor)
	a.OpImm(Add, RBX, 8, true)
	a.Store(RDI, ncIP, RBX)
	a.MovRR(RAX, R12)
	a.AddMem(RAX, RSI, recBOff)
	a.Load(RCX, RDI, scr(sV0))
	a.Store(RAX, 0, RCX)
	a.Lea(RAX, RAX, 8)
	a.Load(RCX, RDI, scr(sV1))
	g.ptrTo()
	g.genWrap(true)
	a.Bind(cursor)
	g.decB(RSI)
	a.LoadIdx(RSI, R12, RSI, 1, 8)
	a.MovImm(RCX, 1)
	a.Store8(RSI, offItDone, RCX)
	g.immSC(RCX)
	a.LeaIdx(RBX, RBX, RCX, 8, 8)
	g.dispatch()
}
