package nativegen

import (
	"github.com/go-quickjs/go-quickjs/internal/bytecode"
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
	start, tmpl, builtin, miss := a.NewLabel(), a.NewLabel(), a.NewLabel(), a.NewLabel()
	a.Bind(start)

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
	a.Jcc(CondNE, tmpl)
	// A leaf that returns a parameter, a constant or an upvalue first
	// thing: its answer read from the window, the code or the callee's
	// closure, as regCallW and regLeafRun read it, with no frame.
	notLeaf, leafStore := a.NewLabel(), a.NewLabel()
	a.LoadU8(R9, RCX, offEKind)
	a.OpImm(And, R9, regCallLeaf, false)
	a.Jcc(CondE, notLeaf)
	a.Load(R8, RCX, offERC)
	a.LoadU8(R9, R8, offRCLeaf)
	upLeaf := a.NewLabel()
	a.OpImm(Cmp, R9, regLeafCode, false)
	a.Jcc(CondE, upLeaf)
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
	// A regLeafCode body that returns an upvalue first thing (leafUp):
	// the callee's upvalue, as regLeafRun reads it.
	a.Bind(upLeaf)
	a.Load(R9, RCX, offECl)
	a.LoadS32(RDX, R8, offRCLeafUp)
	a.OpImm(Sub, RDX, 1, true)
	a.Jcc(CondS, notLeaf)
	a.CmpMem(RDX, R9, offClUpvals+8, true)
	a.Jcc(CondAE, notLeaf)
	a.Load(R9, R9, offClUpvals)
	a.LoadIdx(R9, R9, RDX, 8, 0)
	a.Load(R9, R9, offUpSlot)
	a.Load(RCX, R9, 0)
	a.Load(RDX, R9, 8)
	a.Jmp(leafStore)
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
	a.Jcc(CondNE, builtin)
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

	// Another closure of the template of the callee the site cached, as
	// rhCallW's regSameTemplate finds it: a function of the cached one's
	// closureCode, register code and realm, neither bound nor an arrow
	// nor with more of its own (extra), at a site whose kind is the
	// template's. The cache is made the new callee's, the closure stored
	// with the barrier's routine while the collector marks, and the call
	// made again, which the cache now answers; anything else exits, before
	// anything is written. A closure made anew each time its maker runs
	// is called with no exit.
	a.Bind(tmpl)
	a.LoadU8(R8, RCX, offEKind)
	a.OpImm(And, R8, int32(^(regCallOut|regCallLeaf)&0xff), false)
	a.OpImm(Cmp, R8, regCallEnter, false)
	a.Jcc(CondB, miss)
	a.OpImm(Cmp, R8, regCallObjThis, false)
	a.Jcc(CondA, miss)
	a.LoadU16(RDX, R9, offObjFlags)
	a.OpImm(And, RDX, int32(objHasData), false)
	a.Jcc(CondE, miss)
	a.Load(RDX, R9, sizeObj+offExtData)
	a.CmpMem(RDX, RDI, ncFuncT, true)
	a.Jcc(CondNE, miss)
	a.Load(RDX, R9, sizeObj+offExtData+8)
	a.LoadU8(R8, RDX, offFDBound)
	a.Op(Test, R8, R8, false)
	a.Jcc(CondNE, miss)
	a.LoadU8(R8, RDX, off("funcData.arrow"))
	a.Op(Test, R8, R8, false)
	a.Jcc(CondNE, miss)
	a.Load(R8, RDX, off("funcData.extra"))
	a.Op(Test, R8, R8, true)
	a.Jcc(CondNE, miss)
	a.Load(RDX, RDX, off("funcData.closure"))
	a.Op(Test, RDX, RDX, true)
	a.Jcc(CondE, miss)
	a.Load(R8, RCX, offECl)
	a.Op(Test, R8, R8, true)
	a.Jcc(CondE, miss)
	a.Load(RAX, RDX, offClCode)
	a.CmpMem(RAX, R8, offClCode, true)
	a.Jcc(CondNE, miss)
	a.Load(RAX, RDX, off("closure.realm"))
	a.CmpMem(RAX, R8, off("closure.realm"), true)
	a.Jcc(CondNE, miss)
	// Its register code, its closureCode's, the cached one.
	a.Load(RAX, RDX, offClCode)
	a.AddMem(RAX, RDI, ncCCBase)
	a.Load(RAX, RAX, offCCReg)
	a.Op(Test, RAX, RAX, true)
	a.Jcc(CondE, miss)
	a.CmpMem(RAX, RCX, offERC, true)
	a.Jcc(CondNE, miss)
	a.Store(RDI, scr(sE), RCX)
	a.Store(RDI, scr(sV0), RDX)
	a.Lea(RAX, RCX, offECallee)
	a.MovRR(RCX, R9)
	g.ptrTo()
	a.Load(RAX, RDI, scr(sE))
	a.Lea(RAX, RAX, offECl)
	a.Load(RCX, RDI, scr(sV0))
	g.ptrTo()
	a.Load(RAX, RBX, 0)
	a.Jmp(start)

	// A built-in the site cached (regCallNative): Array.prototype.push of
	// one argument is the code's own (push); any other is called in Go
	// with no exit (light).
	light := a.NewLabel()
	a.Bind(builtin)
	a.OpImm(Cmp, R9, regCallNative, false)
	a.Jcc(CondNE, g.ex(4))
	notPush := a.NewLabel()
	g.push(notPush)
	a.Bind(light)
	g.light(ncBuiltinFn)
	// Another callee than the cache's, of another template: the exit, its
	// Go function nativeCallMiss (ncMiss), which makes the cache the
	// callee's where the code enters it with a frame and has the call made
	// again.
	a.Bind(miss)
	a.StoreImm(RDI, ncMiss, 1)
	a.Jmp(g.exit)
	g.late = append(g.late, func() {
		a.Bind(notPush)
		g.genNext(light)
	})
}

// light is call_w's and call_method_w's call of the built-in its site
// cached, made by nativeBuiltin, a Go function of a handler's form,
// called through callH as the exit stub calls a handler and returning
// to the stub's code after its call (exitAfter), which goes on at the pc
// it gives or ends the frame: the exit's protocol but none of its work
// before the call -- no state to make, no count, no handler to look up,
// and no search of the site's cache again in rhCallW. A frame whose
// state Go has not made yet (topOK) exits.
func (g *nativeGen) light(fn int32) {
	a := g.a
	a.CmpMemImm8(RDI, ncTopOK, 0)
	a.Jcc(CondE, g.exit)
	a.Store(RDI, ncIP, RBX)
	a.Store(RDI, ncBudget, R11)
	a.Load(RAX, RBX, 0)
	a.Store(RDI, ncW, RAX)
	a.MovRR(RCX, RBX)
	a.Load(RDX, RDI, ncCodeBase)
	a.Op(Sub, RCX, RDX, true)
	a.ShiftImm(Shr, RCX, 3, true)
	a.Store(RDI, ncPC, RCX)
	a.Load(RDX, RDI, ncRT)
	a.Store(RDX, offRtBackEdges, R11)
	a.LeaLabel(R8, g.exitAfter)
	a.Store(RDI, ncAfter, R8)
	// nativeBuiltin(st.regs, w, pc, st), or fn's: the func value in DX.
	a.MovRR(RBX, RAX)
	a.Load(RDX, RDI, fn)
	a.Load(RSI, RDI, ncTop)
	a.Load(RAX, RSI, offStRegs)
	a.JmpMem(RDI, ncCallH)
}

// push is call_method_w of Array.prototype.push, the built-in its site
// cached, of one argument, on an array with room for it, as pureElemOp's
// push appends it: the receiver an array an element appended at its
// length is stored in as arrayAppend stores one (appendRoom), the
// argument at elems[len], the length past it, and R[B] the new length. A
// bound function, another built-in, another count of arguments, a full
// array or anything else exits, before anything is written. RCX is the
// site's cache entry, RSI the window, RAX the instruction.
func (g *nativeGen) push(light Label) {
	a := g.a
	a.MovZX8(RDX, RAX)
	a.OpImm(Cmp, RDX, int32(bytecode.RCallMW), false)
	a.Jcc(CondNE, light)
	a.MovRR(RDX, RAX)
	a.ShiftImm(Shr, RDX, 8, true)
	a.MovZX8(RDX, RDX)
	a.OpImm(Cmp, RDX, 1, false)
	a.Jcc(CondNE, light)
	// The callee: push itself, not bound.
	a.Load(R9, RCX, offECallee)
	a.LoadU16(RDX, R9, offObjFlags)
	a.OpImm(And, RDX, int32(objHasData), false)
	a.Jcc(CondE, light)
	a.Load(RDX, R9, sizeObj+offExtData)
	a.CmpMem(RDX, RDI, ncFuncT, true)
	a.Jcc(CondNE, light)
	a.Load(R9, R9, sizeObj+offExtData+8)
	a.LoadU8(RDX, R9, offFDElemOp)
	a.OpImm(Cmp, RDX, int32(elemPush), false)
	a.Jcc(CondNE, light)
	a.LoadU8(RDX, R9, offFDBound)
	a.Op(Test, RDX, RDX, false)
	a.Jcc(CondNE, light)
	// The receiver, an array with room.
	a.Load(R9, RSI, 16)
	a.CmpMem(R9, RDI, ncObjBits, true)
	a.Jcc(CondNE, light)
	a.Store(RDI, scr(sWin), RSI)
	a.Load(RSI, RSI, 24)
	g.appendRoom(RSI, RCX, light, light, true)
	// The argument at elems[len], then the length past it.
	a.Lea(R9, RCX, 1)
	a.Store(RDI, scr(sI), R9)
	a.ShiftImm(Shl, RCX, 4, true)
	a.Load(RAX, RSI, sizeObj)
	a.Op(Add, RAX, RCX, true)
	a.Store(RSI, sizeObj+8, R9)
	a.Load(RDX, RDI, scr(sWin))
	a.Load(R9, RDX, 32)
	a.Store(RAX, 0, R9)
	a.Load(RCX, RDX, 40)
	a.Lea(RAX, RAX, 8)
	g.ptrStore()
	// R[B], the new length.
	a.Load(RAX, RBX, 0)
	a.Load(RCX, RDI, scr(sI))
	a.Cvtsi2sd(0, RCX, true)
	g.storeB(0)
}

// ret is ret and ret_undef of a frame the code entered itself: its
// caller's state off the native frame stack, the value in the caller's
// register, and the caller goes on; the upvalues open on it closed first
// (closeUps), as nativeEnd closes them. The base frame's return is Go's.
func (g *nativeGen) ret(undef bool) {
	a := g.a
	noCap, base, open := a.NewLabel(), a.NewLabel(), a.NewLabel()
	a.CmpMemImm8(RDI, ncDepth, 0)
	a.Jcc(CondE, base)
	// A generator's step ends by genReturn.
	a.Load(RSI, RDI, ncNF)
	a.LoadU8(R8, RSI, offFRegKind)
	a.OpImm(Cmp, R8, regFrameGenStep, false)
	a.Jcc(CondE, g.genRet)
	// Upvalues open on the frame are closed here, as nativeEnd closes
	// them: a frame of captured registers, or an entered one's, whose
	// arguments object a sloppy body maps onto its parameters.
	a.Load(RCX, RDI, ncRC)
	a.LoadS32(R8, RCX, offRCNcap)
	a.Op(Test, R8, R8, false)
	a.Jcc(CondNE, open)
	a.LoadU8(R8, RSI, offFRegKind)
	a.OpImm(Cmp, R8, regFrameEnter, false)
	a.Jcc(CondNE, noCap)
	a.Bind(open)
	a.CmpMemImm8(RSI, offFOpenUp+8, 0)
	a.Jcc(CondE, noCap)
	a.StoreImm(RDI, scr(sV0), 0)
	a.StoreImm(RDI, scr(sV1), -1)
	g.closeUps()
	a.Load(RAX, RBX, 0)
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

// tailTo is tail_call_w's and tail_call_method_w's call once the stack is
// tailCallDepth deep, made by a frame the code entered itself by call_w:
// the frame given to the callee in place, as regTailTo gives it -- the
// arguments moved to the parameters, the rest of the bindings undefined,
// the frame record the callee's -- and the callee's code run in it, its
// caller's state on the native frame stack where call_w put it, so that
// its return goes to the frame's caller. The callee is the one the site's
// cache knows, entered as call_w enters it (regCallEnter, regCallThis),
// whose code may take a frame in place (regCode.tailTo). The frame must
// be call_w's, which has no new.target, with nothing of its own set --
// no this binding, arguments, with or eval scope, or captures open on it
// -- as regTailTo requires. Anything else exits before anything is written: Go gives the
// frame up or makes the call (rhCallWT).
func (g *nativeGen) tailTo() {
	a := g.a
	thisOK, loop, loopEnd, undef, undefEnd, skip, noTop := a.NewLabel(), a.NewLabel(), a.NewLabel(), a.NewLabel(), a.NewLabel(), a.NewLabel(), a.NewLabel()
	a.CmpMemImm8(RDI, ncDepth, 0)
	a.Jcc(CondE, g.exit)
	a.Load(RSI, RDI, ncNF)
	a.LoadU8(R8, RSI, offFRegKind)
	a.OpImm(Cmp, R8, regFrameCall, false)
	a.Jcc(CondNE, g.exit)
	a.Load(R8, RSI, offFThisRef)
	for _, o := range []int32{offFArgs, offFWith, offFEvalVars, offFOpenUp + 8} {
		a.Load(R9, RSI, o)
		a.Op(Or, R8, R9, true)
	}
	a.Op(Test, R8, R8, true)
	a.Jcc(CondNE, g.exit)
	// e := &calls[C], its callee the window's.
	a.MovRR(RCX, RAX)
	a.ShiftImm(Shr, RCX, 48, true)
	a.Load(R9, RDI, ncCC)
	a.CmpMem(RCX, R9, offCCRegCalls+8, true)
	a.Jcc(CondAE, g.exit)
	a.MovImm(R8, uint64(sizeE))
	a.Imul(RCX, R8)
	a.AddMem(RCX, R9, offCCRegCalls)
	g.decA(RDX)
	a.LeaIdx(RSI, R12, RDX, 1, 0)
	a.Load(R9, RSI, 0)
	a.CmpMem(R9, RDI, ncObjBits, true)
	a.Jcc(CondNE, g.exit)
	a.Load(R9, RSI, 8)
	a.CmpMem(R9, RCX, offECallee, true)
	a.Jcc(CondNE, g.exit)
	// The this: the receiver as it is, or the cache's.
	a.Load(R8, RSI, 16)
	a.Load(R9, RSI, 24)
	a.LoadU8(RDX, RCX, offEKind)
	a.OpImm(Cmp, RDX, regCallEnter, false)
	a.Jcc(CondE, thisOK)
	a.OpImm(Cmp, RDX, regCallThis, false)
	a.Jcc(CondNE, g.exit)
	a.Load(R8, RCX, offEThis)
	a.Load(R9, RCX, offEThis+8)
	a.Bind(thisOK)
	// Its code, taking the frame in place, and the stack's room for it.
	a.Load(RDX, RCX, offERC)
	a.Store(RDI, scr(sE), RCX)
	a.LoadU8(RCX, RDX, offRCTailTo)
	a.Op(Test, RCX, RCX, false)
	a.Jcc(CondE, g.exit)
	a.Store(RDI, scr(sWin), RSI)
	a.Store(RDI, scr(sRC), RDX)
	a.Store(RDI, scr(sV0), R8)
	a.Store(RDI, scr(sV1), R9)
	a.Load(RSI, RDI, ncNF)
	a.Load(RCX, RSI, offFBase)
	a.LoadS32(R9, RDX, offRCNumRegs)
	a.Op(Add, R9, RCX, true)
	a.Load(RCX, RDI, ncRT)
	a.CmpMem(R9, RCX, offRtStack+8, true)
	a.Jcc(CondA, g.exit)
	a.Store(RDI, scr(sTop), R9)
	a.CmpMemImm8(RCX, offRtInterrupt, 1)
	a.Jcc(CondLE, g.exit)
	// The arguments the parameters take: min(argc, params).
	a.MovRR(R8, RAX)
	a.ShiftImm(Shr, R8, 8, true)
	a.MovZX8(R8, R8)
	a.LoadS32(R9, RDX, offRCParams)
	argc := a.NewLabel()
	a.Op(Cmp, R8, R9, true)
	a.Jcc(CondLE, argc)
	a.MovRR(R8, R9)
	a.Bind(argc)
	a.Store(RDI, scr(sArgc), R8)

	// Nothing is written before here.
	//
	// The arguments moved down to the parameters, in order: each source
	// lies past its destination (the window is the frame's or past it),
	// read before anything at or past it is written.
	a.Op(Xor, RCX, RCX, true)
	a.Bind(loop)
	a.CmpMem(RCX, RDI, scr(sArgc), true)
	a.Jcc(CondGE, loopEnd)
	a.Store(RDI, scr(sI), RCX)
	a.ShiftImm(Shl, RCX, 4, true)
	a.Load(RSI, RDI, scr(sWin))
	a.Op(Add, RSI, RCX, true)
	a.LeaIdx(RAX, R12, RCX, 1, 32)
	a.Load(R8, RSI, 32)
	a.Store(RAX, 0, R8)
	a.Load(RCX, RSI, 40)
	a.Lea(RAX, RAX, 8)
	g.ptrTo()
	a.Load(RCX, RDI, scr(sI))
	a.OpImm(Add, RCX, 1, true)
	a.Jmp(loop)
	a.Bind(loopEnd)
	// Register 0 the callee, 1 the this.
	a.Load(R8, RDI, ncObjBits)
	a.Store(R12, 0, R8)
	a.Load(RCX, RDI, scr(sE))
	a.Load(RCX, RCX, offECallee)
	a.Lea(RAX, R12, 8)
	g.ptrTo()
	a.Load(R8, RDI, scr(sV0))
	a.Store(R12, 16, R8)
	a.Load(RCX, RDI, scr(sV1))
	a.Lea(RAX, R12, 24)
	g.ptrTo()
	// Parameters no argument was given for, and the bindings past them:
	// undefined, from min(argc, params) to params+inits.
	a.Load(RCX, RDI, scr(sArgc))
	a.Load(R8, RDI, scr(sRC))
	a.LoadS32(R9, R8, offRCParams)
	a.LoadS32(RSI, R8, offRCInits)
	a.Op(Add, R9, RSI, true)
	a.Bind(undef)
	a.Op(Cmp, RCX, R9, true)
	a.Jcc(CondGE, undefEnd)
	a.MovRR(RAX, RCX)
	a.ShiftImm(Shl, RAX, 4, true)
	a.Op(Add, RAX, R12, true)
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
	a.Jmp(undef)
	a.Bind(undefEnd)
	// The stack's top, the interrupt budget.
	a.Load(R9, RDI, ncRT)
	a.DecMem(R9, offRtInterrupt)
	a.Load(RDX, RDI, scr(sTop))
	a.CmpMem(RDX, R9, offRtStackTop, true)
	a.Jcc(CondLE, noTop)
	a.Store(R9, offRtStackTop, RDX)
	a.CmpMem(RDX, R9, offRtStackHigh, true)
	a.Jcc(CondLE, noTop)
	a.Store(R9, offRtStackHigh, RDX)
	a.Bind(noTop)
	// The frame record the callee's: pc 0, its this, closure and callee.
	a.Load(RSI, RDI, ncNF)
	a.Op(Xor, RCX, RCX, false)
	a.Store32(RSI, offFPC, RCX)
	a.Load(R8, RDI, scr(sV0))
	a.Store(RSI, offFThis, R8)
	a.Load(RCX, RDI, scr(sV1))
	a.Lea(RAX, RSI, offFThis+8)
	g.ptrTo()
	for _, f := range [][2]int32{{offECl, offFCl}, {offECallee, offFCallee}} {
		a.Load(RCX, RDI, scr(sE))
		a.Load(RCX, RCX, f[0])
		a.Load(RSI, RDI, ncNF)
		a.Lea(RAX, RSI, f[1])
		g.ptrTo()
	}
	// new.target undefined, where the callee reads it, as call_w's.
	nt := a.NewLabel()
	a.Load(R8, RDI, scr(sRC))
	a.LoadU8(RCX, R8, offRCNewTarget)
	a.Op(Test, RCX, RCX, false)
	a.Jcc(CondE, nt)
	a.Load(RSI, RDI, ncNF)
	a.Load(R8, RDI, ncUndef)
	a.Store(RSI, offFNewTgt, R8)
	a.Op(Xor, RCX, RCX, true)
	a.Lea(RAX, RSI, offFNewTgt+8)
	g.ptrTo()
	a.Bind(nt)
	// The callee's code, caches and constants the top frame's; Go makes
	// the frame's state again at its next exit (topOK).
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
	a.StoreImm(RDI, ncFV, 0)
	a.StoreImm(RDI, ncTopOK, 0)
	g.dispatch()
}
