package nativegen

import (
	. "github.com/go-quickjs/go-quickjs/internal/jit/asm/amd64"
)

// keepArgs puts an entered frame's arguments, the window's argc from
// register 2, on the runtime's argument stack, as regEnterW appends them,
// and its length past them; sKa is where they start.
func (g *nativeGen) keepArgs() {
	a := g.a
	done, loop, end := a.NewLabel(), a.NewLabel(), a.NewLabel()
	a.CmpMemImm8(RDI, scr(sEnt), 0)
	a.Jcc(CondE, done)
	a.Load(R9, RDI, ncRT)
	a.Load(RCX, R9, offRtArgStack+8)
	a.Store(RDI, scr(sKa), RCX)
	a.Op(Xor, RCX, RCX, true)
	a.Bind(loop)
	a.CmpMem(RCX, RDI, scr(sArgc), true)
	a.Jcc(CondGE, end)
	// The word of the number, plainly; the pointer word by ptrStore
	// where it differs.
	a.Store(RDI, scr(sI), RCX)
	a.MovRR(RDX, RCX)
	a.ShiftImm(Shl, RDX, 4, true)
	a.AddMem(RDX, RDI, scr(sWin))
	a.Load(R9, RDI, ncRT)
	a.Load(RAX, R9, offRtArgStack+8)
	a.Op(Add, RAX, RCX, true)
	a.ShiftImm(Shl, RAX, 4, true)
	a.AddMem(RAX, R9, offRtArgStack)
	a.Load(R8, RDX, 32)
	a.Store(RAX, 0, R8)
	a.Load(RCX, RDX, 40)
	a.Lea(RAX, RAX, 8)
	g.ptrTo()
	a.Load(RCX, RDI, scr(sI))
	a.OpImm(Add, RCX, 1, true)
	a.Jmp(loop)
	a.Bind(end)
	a.Load(R9, RDI, ncRT)
	a.Load(RCX, RDI, scr(sKa))
	a.AddMem(RCX, RDI, scr(sArgc))
	a.Store(R9, offRtArgStack+8, RCX)
	a.Bind(done)
}

// frameArgs writes the frame record's args: an entered frame's those
// keepArgs kept, its locals its parameters' registers, its new.target
// undefined and its kind regEnterW's; any other frame's cleared, as
// regClearFrame clears what a frame's last user left.
func (g *nativeGen) frameArgs() {
	a := g.a
	ent, done, zero, haveP, cleared := a.NewLabel(), a.NewLabel(), a.NewLabel(), a.NewLabel(), a.NewLabel()
	a.CmpMemImm8(RDI, scr(sEnt), 0)
	a.Jcc(CondNE, ent)
	a.Load(RSI, RDI, scr(sNF))
	a.CmpMemImm8(RSI, offFArgs, 0)
	a.Jcc(CondE, done)
	a.Op(Xor, RCX, RCX, true)
	a.Store(RSI, offFArgs+8, RCX)
	a.Store(RSI, offFArgs+16, RCX)
	a.Lea(RAX, RSI, offFArgs)
	g.ptrStore()
	a.Jmp(done)
	a.Bind(ent)
	a.Load(RSI, RDI, scr(sNF))
	a.MovImm(RCX, uint64(regFrameEnter))
	a.Store8(RSI, offFRegKind, RCX)
	// args: the argument stack from sKa, argc of them; a slice of no
	// room is the stack's own pointer, as Go makes it.
	a.Load(RCX, RDI, scr(sArgc))
	a.Store(RSI, offFArgs+8, RCX)
	a.Store(RSI, offFArgs+16, RCX)
	a.Load(R9, RDI, ncRT)
	a.Op(Test, RCX, RCX, true)
	a.Jcc(CondE, zero)
	a.Load(RCX, RDI, scr(sKa))
	a.ShiftImm(Shl, RCX, 4, true)
	a.AddMem(RCX, R9, offRtArgStack)
	a.Jmp(cleared)
	a.Bind(zero)
	a.Load(RCX, R9, offRtArgStack)
	a.Bind(cleared)
	a.Lea(RAX, RSI, offFArgs)
	g.ptrTo()
	// locals: the stack from base+2, params of them.
	a.Load(RSI, RDI, scr(sNF))
	a.Load(R8, RDI, scr(sRC))
	a.LoadS32(RCX, R8, offRCParams)
	a.Store(RSI, offFLocals+8, RCX)
	a.Store(RSI, offFLocals+16, RCX)
	a.Load(R9, RDI, ncRT)
	a.Op(Test, RCX, RCX, true)
	a.Load(RCX, R9, offRtStack)
	a.Jcc(CondE, haveP)
	a.Load(RDX, RDI, scr(sBase))
	a.OpImm(Add, RDX, 2, true)
	a.ShiftImm(Shl, RDX, 4, true)
	a.Op(Add, RCX, RDX, true)
	a.Bind(haveP)
	a.Lea(RAX, RSI, offFLocals)
	g.ptrTo()
	// new.target: undefined.
	a.Load(RSI, RDI, scr(sNF))
	a.Load(R8, RDI, ncUndef)
	a.Store(RSI, offFNewTgt, R8)
	a.Op(Xor, RCX, RCX, true)
	a.Lea(RAX, RSI, offFNewTgt+8)
	g.ptrTo()
	a.Bind(done)
}

// addProp is set_prop's addition of the property by the transition the
// site at RCX remembers, as rsSetProp makes it: the object in RSI of the
// site's shape, extensible, with room for one more value, and nothing up
// the prototype chain the site found changed; the value at the address in
// RDX appended, the shape the transition's. Anything else exits: a full
// table, an index to build, a setter's entry.
func (g *nativeGen) addProp() {
	a := g.a
	p1ok, p2ok, idxok := a.NewLabel(), a.NewLabel(), a.NewLabel()
	shapeOff := off("Object.shape")
	a.Load(R8, RCX, off("propCache.shape"))
	a.CmpMem(R8, RSI, shapeOff, true)
	a.Jcc(CondNE, g.exit)
	a.Load(R8, RCX, off("propCache.next"))
	a.Op(Test, R8, R8, true)
	a.Jcc(CondE, g.exit)
	a.CmpMem(R8, RDI, ncSetterNx, true)
	a.Jcc(CondE, g.exit)
	a.LoadU16(R9, RSI, offObjFlags)
	a.OpImm(And, R9, int32(objExtensible), false)
	a.Jcc(CondE, g.exit)
	a.Load(R9, RCX, off("propCache.p1"))
	a.Op(Test, R9, R9, true)
	a.Jcc(CondE, p1ok)
	a.Load(R9, R9, shapeOff)
	a.CmpMem(R9, RCX, off("propCache.s1"), true)
	a.Jcc(CondE, p1ok)
	a.Load(R9, RCX, off("propCache.cell"))
	a.Op(Test, R9, R9, true)
	a.Jcc(CondE, g.exit)
	a.LoadU8(R9, R9, offCellValid)
	a.Op(Test, R9, R9, false)
	a.Jcc(CondE, g.exit)
	a.Bind(p1ok)
	a.Load(R9, RCX, off("propCache.p2"))
	a.Op(Test, R9, R9, true)
	a.Jcc(CondE, p2ok)
	a.Load(R9, R9, shapeOff)
	a.CmpMem(R9, RCX, off("propCache.s2"), true)
	a.Jcc(CondNE, g.exit)
	a.Bind(p2ok)
	// Room for the value; a shape of no index past linearScanLimit
	// values is given one by Go.
	a.LoadU32(R9, RSI, off("Object.valn"))
	a.LoadU32(RCX, RSI, off("Object.valc"))
	a.Op(Cmp, R9, RCX, true)
	a.Jcc(CondAE, g.exit)
	a.CmpMemImm8(R8, off("shape.index"), 0)
	a.Jcc(CondNE, idxok)
	a.OpImm(Cmp, R9, linearScanLimit, true)
	a.Jcc(CondAE, g.exit)
	a.Bind(idxok)
	a.Store(RDI, scr(sE), RSI)
	a.Store(RDI, scr(sRC), R8)
	// The value at valn, then valn past it, then the shape.
	a.MovRR(RAX, R9)
	a.ShiftImm(Shl, RAX, 4, true)
	a.AddMem(RAX, RSI, off("Object.valp"))
	a.OpImm(Add, R9, 1, true)
	a.Load(RCX, RDX, 0)
	a.Store(RAX, 0, RCX)
	a.Load(RCX, RDX, 8)
	a.Store32(RSI, off("Object.valn"), R9)
	a.Lea(RAX, RAX, 8)
	g.ptrStore()
	a.Load(RSI, RDI, scr(sE))
	a.Load(RCX, RDI, scr(sRC))
	a.Lea(RAX, RSI, shapeOff)
	g.ptrStore()
	g.next()
}

// ptrTo stores the pointer in RCX at the address in RAX, by ptrStore,
// where the word there differs.
func (g *nativeGen) ptrTo() {
	a := g.a
	same := a.NewLabel()
	a.CmpMem(RCX, RAX, 0, true)
	a.Jcc(CondE, same)
	g.ptrStore()
	a.Bind(same)
}

// call is call_w and call_method_w: a callee the site's cache knows,
// entered as regCallW enters it -- its this, the frame record, the stack
// top and depth -- and run here, its caller's state pushed on the native
// frame stack. Anything regCallW does not answer at once (a leaf, a
// built-in, a generator, a frame that keeps its arguments or reads
// new.target, a first call), a full stack or frame stack, an interrupt
// due, a frame record its last user left set: Go's, by an exit before
// anything is written.
func (g *nativeGen) call() {
	a := g.a
	ok, noThis, noTop, loop, loopEnd, skip, high := a.NewLabel(), a.NewLabel(), a.NewLabel(), a.NewLabel(), a.NewLabel(), a.NewLabel(), a.NewLabel()

	// e := &calls[C].
	a.MovRR(RCX, RAX)
	a.ShiftImm(Shr, RCX, 48, true)
	a.Load(R9, RDI, ncCC)
	a.CmpMem(RCX, R9, offCCRegCalls+8, true)
	a.Jcc(CondAE, g.ex(1))
	a.MovImm(R8, uint64(sizeE))
	a.Imul(RCX, R8)
	a.AddMem(RCX, R9, offCCRegCalls)
	// The window, its callee the cache's.
	g.decA(RDX)
	a.LeaIdx(RSI, R12, RDX, 1, 0)
	a.Load(R9, RSI, 0)
	a.CmpMem(R9, RDI, ncObjBits, true)
	a.Jcc(CondNE, g.ex(2))
	a.Load(R9, RSI, 8)
	a.CmpMem(R9, RCX, offECallee, true)
	a.Jcc(CondNE, g.ex(3))
	// A leaf that returns a parameter or a constant first thing: its
	// answer read from the window or the code, as regCallW reads it,
	// with no frame.
	notLeaf, leafStore := a.NewLabel(), a.NewLabel()
	a.LoadU8(R9, RCX, offEKind)
	a.OpImm(And, R9, regCallLeaf, false)
	a.Jcc(CondE, notLeaf)
	a.Load(R8, RCX, offERC)
	a.LoadU8(R9, R8, offRCLeaf)
	a.OpImm(Cmp, R9, regLeafArg, false)
	a.Jcc(CondB, notLeaf)
	a.Load(RDX, R8, offRCLeafVal+8)
	a.Load(RCX, R8, offRCLeafVal)
	a.Jcc(CondNE, leafStore)
	// regLeafArg: the parameter where the call gave it, else undefined.
	a.Load(RCX, RDI, ncUndef)
	a.Op(Xor, RDX, RDX, false)
	a.LoadS32(R9, R8, offRCLeafArg)
	a.MovRR(R8, RAX)
	a.ShiftImm(Shr, R8, 8, true)
	a.MovZX8(R8, R8)
	a.Op(Cmp, R9, R8, true)
	a.Jcc(CondGE, leafStore)
	a.ShiftImm(Shl, R9, 4, true)
	a.Op(Add, R9, RSI, true)
	a.Load(RCX, R9, 32)
	a.Load(RDX, R9, 40)
	a.Bind(leafStore)
	a.MovRR(R9, RCX)
	a.MovRR(R8, RDX)
	g.decB(RDX)
	a.LeaIdx(RAX, R12, RDX, 1, 0)
	g.storeValue()
	a.Bind(notLeaf)
	// A callee rhCallW leaves to regCallMiss (regCallOut): one that keeps
	// its arguments, entered here as regEnterW enters it (sEnt), and a
	// leaf Go answers without a frame (regCallLeaf), a pure body or one
	// that reads or writes its this, which a frame here costs less than
	// an exit; its kind the cache's without the marks.
	plain := a.NewLabel()
	a.StoreImm(RDI, scr(sEnt), 0)
	a.LoadU8(R9, RCX, offEKind)
	a.MovRR(R8, R9)
	a.OpImm(And, R8, regCallOut|regCallLeaf, false)
	a.Jcc(CondE, plain)
	a.OpImm(And, R9, ^(regCallOut|regCallLeaf)&0xff, false)
	a.Load(R8, RCX, offERC)
	a.LoadU8(RDX, R8, offRCArgs)
	a.Op(Test, RDX, RDX, false)
	a.Jcc(CondE, plain)
	a.StoreImm(RDI, scr(sEnt), 1)
	a.Bind(plain)
	// Entered as it is, with the cache's this, or with an object this.
	a.OpImm(Cmp, R9, regCallEnter, false)
	a.Jcc(CondE, ok)
	a.OpImm(Cmp, R9, regCallThis, false)
	a.Jcc(CondE, ok)
	a.OpImm(Cmp, R9, regCallObjThis, false)
	a.Jcc(CondNE, g.ex(4))
	a.Load(R8, RSI, 16)
	a.CmpMem(R8, RDI, ncObjBits, true)
	a.Jcc(CondNE, g.ex(5))
	a.Bind(ok)
	a.Store(RDI, scr(sE), RCX)
	a.Store(RDI, scr(sWin), RSI)
	a.Store(RDI, scr(sKind), R9)
	a.Load(R8, RCX, offERC)
	a.Store(RDI, scr(sRC), R8)
	// The arguments where they lie: none past the parameters it keeps,
	// and no new.target.
	a.MovRR(RDX, RAX)
	a.ShiftImm(Shr, RDX, 8, true)
	a.MovZX8(RDX, RDX)
	a.Store(RDI, scr(sArgc), RDX)
	kept := a.NewLabel()
	a.CmpMemImm8(RDI, scr(sEnt), 0)
	a.Jcc(CondNE, kept)
	a.LoadU16(R9, R8, offRCKeep)
	a.Op(Cmp, RDX, R9, true)
	a.Jcc(CondA, g.ex(6))
	a.Bind(kept)
	a.LoadU8(R9, R8, offRCNewTarget)
	a.Op(Test, R9, R9, false)
	a.Jcc(CondNE, g.ex(7))
	// The window's base, and the frame's top, in the stack.
	a.Load(R9, RDI, ncRT)
	a.MovRR(RDX, RSI)
	a.Load(RCX, R9, offRtStack)
	a.Op(Sub, RDX, RCX, true)
	a.ShiftImm(Shr, RDX, 4, true)
	a.Store(RDI, scr(sBase), RDX)
	a.LoadS32(RCX, R8, offRCNumRegs)
	a.Op(Add, RCX, RDX, true)
	a.CmpMem(RCX, R9, offRtStack+8, true)
	a.Jcc(CondA, g.ex(8))
	a.Store(RDI, scr(sTop), RCX)
	// The depth, the interrupt budget, the frame record.
	a.Load(RDX, R9, offRtFrameDepth)
	a.CmpMem(RDX, R9, offRtMaxFrames, true)
	a.Jcc(CondGE, g.ex(9))
	a.CmpMemImm8(R9, offRtInterrupt, 1)
	a.Jcc(CondLE, g.ex(10))
	a.MovRR(RSI, RDX)
	a.Load(R8, R9, offRtFrameBase)
	a.Op(Sub, RSI, R8, true)
	a.CmpMem(RSI, R9, offRtCur+8, true)
	a.Jcc(CondAE, g.ex(11))
	a.MovImm(R8, uint64(sizeFrame))
	a.Imul(RSI, R8)
	a.AddMem(RSI, R9, offRtCur)
	// What a frame's last user left set: one test of the five words or'd.
	a.Load(R8, RSI, offFThisRef)
	for _, off := range []int32{offFEvalVars, offFWith, offFNative + 8} {
		a.Load(R9, RSI, off)
		a.Op(Or, R8, R9, true)
	}
	a.Op(Test, R8, R8, true)
	a.Jcc(CondNE, g.ex(12))
	a.Store(RDI, scr(sNF), RSI)
	// The arguments an entered frame keeps go on the runtime's argument
	// stack, which must have the room: Go grows it.
	noKeep := a.NewLabel()
	a.CmpMemImm8(RDI, scr(sEnt), 0)
	a.Jcc(CondE, noKeep)
	a.Load(R9, RDI, ncRT)
	a.CmpMemImm8(R9, offRtArgStack, 0)
	a.Jcc(CondE, g.ex(14))
	a.Load(RCX, R9, offRtArgStack+8)
	a.AddMem(RCX, RDI, scr(sArgc))
	a.CmpMem(RCX, R9, offRtArgStack+16, true)
	a.Jcc(CondA, g.ex(14))
	a.Bind(noKeep)
	a.Load(R8, RDI, ncDepth)
	a.CmpMem(R8, RDI, ncCap, true)
	a.Jcc(CondGE, g.exitRet)

	// Nothing is written before here.
	//
	// The caller's pc, which a stack trace reads, is not written here:
	// Go writes it when it next runs (nativeFillPCs), the only time anything
	// reads it.
	// The this the cache gives.
	a.Load(R9, RDI, scr(sKind))
	a.OpImm(Cmp, R9, regCallThis, false)
	a.Jcc(CondNE, noThis)
	a.Load(RCX, RDI, scr(sE))
	a.Load(R8, RCX, offEThis)
	a.Load(R9, RCX, offEThis+8)
	a.Load(RAX, RDI, scr(sWin))
	a.Store(RAX, 16, R8)
	a.Lea(RAX, RAX, 24)
	// The pointer word, where the window does not hold it already.
	a.CmpMem(R9, RAX, 0, true)
	a.Jcc(CondE, noThis)
	a.MovRR(RCX, R9)
	g.ptrStore()
	a.Bind(noThis)
	// The interrupt budget, the stack top.
	a.Load(R9, RDI, ncRT)
	a.DecMem(R9, offRtInterrupt)
	a.Load(RCX, R9, offRtStackTop)
	a.Store(RDI, scr(sSaved), RCX)
	a.Load(RDX, RDI, scr(sTop))
	a.Op(Cmp, RDX, RCX, true)
	a.Jcc(CondLE, noTop)
	a.Store(R9, offRtStackTop, RDX)
	a.CmpMem(RDX, R9, offRtStackHigh, true)
	a.Jcc(CondLE, noTop)
	a.Store(R9, offRtStackHigh, RDX)
	a.Bind(noTop)
	g.keepArgs()
	// Parameters no argument was given for, and the bindings past them:
	// undefined, from min(argc, params) to params+inits.
	a.Load(R8, RDI, scr(sRC))
	a.LoadS32(RCX, R8, offRCParams)
	a.Load(RDX, RDI, scr(sArgc))
	a.Op(Cmp, RDX, RCX, true)
	from := a.NewLabel()
	a.Jcc(CondGE, from)
	a.MovRR(RCX, RDX)
	a.Bind(from)
	a.LoadS32(R9, R8, offRCParams)
	a.LoadS32(RSI, R8, offRCInits)
	a.Op(Add, R9, RSI, true)
	// i in RCX and the end in R9, kept in the scratch only across a
	// store of a pointer word.
	a.Bind(loop)
	a.Op(Cmp, RCX, R9, true)
	a.Jcc(CondGE, loopEnd)
	a.MovRR(RAX, RCX)
	a.ShiftImm(Shl, RAX, 4, true)
	a.AddMem(RAX, RDI, scr(sWin))
	a.Load(R8, RDI, ncUndef)
	a.Store(RAX, 32, R8)
	a.CmpMemImm8(RAX, 40, 0)
	a.Jcc(CondE, skip)
	a.Store(RDI, scr(sI), RCX)
	a.Store(RDI, scr(sEnd), R9)
	a.Lea(RAX, RAX, 40)
	a.Op(Xor, RCX, RCX, true)
	g.ptrStore()
	a.Load(RCX, RDI, scr(sI))
	a.Load(R9, RDI, scr(sEnd))
	a.Bind(skip)
	a.OpImm(Add, RCX, 1, true)
	a.Jmp(loop)
	a.Bind(loopEnd)
	// The depth.
	a.Load(R9, RDI, ncRT)
	a.IncMem(R9, offRtFrameDepth)
	a.Load(RCX, R9, offRtFrameDepth)
	a.CmpMem(RCX, R9, offRtFrameHigh, true)
	a.Jcc(CondLE, high)
	a.Store(R9, offRtFrameHigh, RCX)
	a.Bind(high)
	// The frame record: its base, pc 0, no open upvalues, a call's kind;
	// its closure and callee, written where they differ.
	a.Load(RSI, RDI, scr(sNF))
	a.Load(RCX, RDI, scr(sBase))
	a.Store(RSI, offFBase, RCX)
	a.Op(Xor, RCX, RCX, false)
	a.Store32(RSI, offFPC, RCX)
	a.Store(RSI, offFOpenUp+8, RCX)
	a.MovImm(RCX, uint64(regFrameCall))
	a.Store8(RSI, offFRegKind, RCX)
	g.frameArgs()
	for _, f := range [][2]int32{{offECl, offFCl}, {offECallee, offFCallee}} {
		same := a.NewLabel()
		a.Load(RCX, RDI, scr(sE))
		a.Load(R8, RCX, f[0])
		a.Load(RSI, RDI, scr(sNF))
		a.Lea(RAX, RSI, f[1])
		a.CmpMem(R8, RAX, 0, true)
		a.Jcc(CondE, same)
		a.MovRR(RCX, R8)
		g.ptrStore()
		a.Bind(same)
	}
	// The caller's state, on the native frame stack.
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
	a.Load(RCX, RDI, scr(sSaved))
	a.Store(RSI, recSaved, RCX)
	a.Load(RAX, RBX, 0)
	g.decB(RCX)
	a.Store(RSI, recBOff, RCX)
	a.IncMem(RDI, ncDepth)
	a.StoreImm(RDI, ncTopOK, 0)
	// The callee's, the top frame's: its window, its closure's caches,
	// constants, epochs and call sites, its code and frame record.
	a.Load(R12, RDI, scr(sWin))
	a.Store(RDI, ncRegs, R12)
	a.Load(RCX, RDI, scr(sE))
	a.Load(R8, RCX, offECl)
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
	// The callee's feedback vector: none yet.
	a.StoreImm(RDI, ncFV, 0)
	// Run it, counted (made).
	a.IncMem(RDI, ncMade)
	g.dispatch()
}

// ret is ret and ret_undef of a frame the code entered itself: its
// caller's state off the native frame stack, the value in the caller's
// register, and the caller goes on. The base frame's return is Go's, as
// is a frame's with upvalues to close.
func (g *nativeGen) ret(undef bool) {
	a := g.a
	noCap, base := a.NewLabel(), a.NewLabel()
	a.CmpMemImm8(RDI, ncDepth, 0)
	a.Jcc(CondE, base)
	a.Load(RCX, RDI, ncRC)
	a.LoadS32(R8, RCX, offRCNcap)
	a.Op(Test, R8, R8, false)
	a.Jcc(CondE, noCap)
	a.Load(RSI, RDI, ncNF)
	a.CmpMemImm8(RSI, offFOpenUp+8, 0)
	a.Jcc(CondNE, g.exit)
	a.Bind(noCap)
	// The value.
	if undef {
		a.Load(R8, RDI, ncUndef)
		a.Op(Xor, R9, R9, false)
	} else {
		g.decA(RDX)
		a.LoadIdx(R8, R12, RDX, 1, 0)
		a.LoadIdx(R9, R12, RDX, 1, 8)
	}
	a.Store(RDI, scr(sV0), R8)
	a.Store(RDI, scr(sV1), R9)
	// The frame closed; an entered frame's arguments given back to the
	// argument stack, as regEnterW gives them.
	a.Load(RSI, RDI, ncNF)
	a.Load(R9, RDI, ncRT)
	notEnt := a.NewLabel()
	a.LoadU8(RCX, RSI, offFRegKind)
	a.OpImm(Cmp, RCX, regFrameEnter, false)
	a.Jcc(CondNE, notEnt)
	a.Load(RCX, RSI, offFArgs+8)
	a.Load(RDX, R9, offRtArgStack+8)
	a.Op(Sub, RDX, RCX, true)
	a.Store(R9, offRtArgStack+8, RDX)
	a.Bind(notEnt)
	a.Op(Xor, RCX, RCX, false)
	a.Store8(RSI, offFRegKind, RCX)
	a.DecMem(R9, offRtFrameDepth)
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
	// The caller at its call; the value in its register B.
	a.Load(RBX, RSI, recRetIP)
	a.Load(RAX, RDI, ncRegs)
	a.AddMem(RAX, RSI, recBOff)
	a.Load(RCX, RDI, scr(sV0))
	a.Store(RAX, 0, RCX)
	a.Lea(RAX, RAX, 8)
	a.Load(RCX, RDI, scr(sV1))
	same := a.NewLabel()
	a.CmpMem(RCX, RAX, 0, true)
	a.Jcc(CondE, same)
	g.ptrStore()
	a.Bind(same)
	a.Load(R12, RDI, ncRegs)
	a.Load(R15, RDI, ncIC)
	g.next()
	// The base frame's return: its value in the scratch, retDone set, and
	// nativeRun returns it, as runReg would.
	a.Bind(base)
	if undef {
		a.Load(R8, RDI, ncUndef)
		a.Op(Xor, R9, R9, false)
	} else {
		g.decA(RDX)
		a.LoadIdx(R8, R12, RDX, 1, 0)
		a.LoadIdx(R9, R12, RDX, 1, 8)
	}
	a.Store(RDI, scr(sV0), R8)
	a.Store(RDI, scr(sV1), R9)
	a.StoreImm(RDI, ncRetDone, 1)
	a.Jmp(g.exitRet)
}
