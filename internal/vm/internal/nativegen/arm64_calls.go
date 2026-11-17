package nativegen

import (
	"github.com/go-quickjs/go-quickjs/internal/bytecode"
	. "github.com/go-quickjs/go-quickjs/internal/jit/asm/arm64"
)

// keepArgs puts an entered frame's arguments on the runtime's argument
// stack, as amd64's.
func (g *armGen) keepArgs() {
	a := g.a
	done, loop, end := a.NewLabel(), a.NewLabel(), a.NewLabel()
	g.cmpMemImm(rCtx, scr(sEnt), 0)
	a.BCond(EQ, done)
	a.Load(r9, rCtx, ncRT)
	a.Load(rC, r9, offRtArgStack+8)
	a.Store(rCtx, scr(sKa), rC)
	a.MovImm(rC, 0)
	a.Bind(loop)
	g.cmpMem(rC, rCtx, scr(sArgc))
	a.BCond(GE, end)
	// The word of the number, plainly; the pointer word by ptrStore
	// where it differs.
	a.Store(rCtx, scr(sI), rC)
	a.ShiftImm(Lsl, rD, rC, 4, true)
	g.addMem(rD, rCtx, scr(sWin))
	a.Load(r9, rCtx, ncRT)
	a.Load(rW, r9, offRtArgStack+8)
	a.Op(Add, rW, rW, rC, true)
	a.ShiftImm(Lsl, rW, rW, 4, true)
	g.addMem(rW, r9, offRtArgStack)
	a.Load(r8, rD, 32)
	a.Store(rW, 0, r8)
	a.Load(rC, rD, 40)
	a.AddImm(rW, rW, 8, true)
	g.ptrTo()
	a.Load(rC, rCtx, scr(sI))
	a.AddImm(rC, rC, 1, true)
	a.B(loop)
	a.Bind(end)
	a.Load(r9, rCtx, ncRT)
	a.Load(rC, rCtx, scr(sKa))
	g.addMem(rC, rCtx, scr(sArgc))
	a.Store(r9, offRtArgStack+8, rC)
	a.Bind(done)
}

// frameArgs writes the frame record's args, locals and new.target, as
// amd64's.
func (g *armGen) frameArgs() {
	a := g.a
	ent, done, zero, haveP, cleared := a.NewLabel(), a.NewLabel(), a.NewLabel(), a.NewLabel(), a.NewLabel()
	g.cmpMemImm(rCtx, scr(sEnt), 0)
	a.BCond(NE, ent)
	a.Load(rS, rCtx, scr(sNF))
	g.cmpMemImm(rS, offFArgs, 0)
	a.BCond(EQ, done)
	a.Store(rS, offFArgs+8, ZR)
	a.Store(rS, offFArgs+16, ZR)
	a.MovImm(rC, 0)
	a.AddImm(rW, rS, int64(offFArgs), true)
	g.ptrStore()
	a.B(done)
	a.Bind(ent)
	a.Load(rS, rCtx, scr(sNF))
	a.MovImm(rC, uint64(regFrameEnter))
	a.Store8(rS, offFRegKind, rC)
	// args: the argument stack from sKa, argc of them; a slice of no
	// room is the stack's own pointer, as Go makes it.
	a.Load(rC, rCtx, scr(sArgc))
	a.Store(rS, offFArgs+8, rC)
	a.Store(rS, offFArgs+16, rC)
	a.Load(r9, rCtx, ncRT)
	a.Cbz(rC, zero, true)
	a.Load(rC, rCtx, scr(sKa))
	a.ShiftImm(Lsl, rC, rC, 4, true)
	g.addMem(rC, r9, offRtArgStack)
	a.B(cleared)
	a.Bind(zero)
	a.Load(rC, r9, offRtArgStack)
	a.Bind(cleared)
	a.AddImm(rW, rS, int64(offFArgs), true)
	g.ptrTo()
	// locals: the stack from base+2, params of them.
	a.Load(rS, rCtx, scr(sNF))
	a.Load(r8, rCtx, scr(sRC))
	a.LoadS32(rC, r8, offRCParams)
	a.Store(rS, offFLocals+8, rC)
	a.Store(rS, offFLocals+16, rC)
	a.Load(r9, rCtx, ncRT)
	a.MovRR(rD, rC)
	a.Load(rC, r9, offRtStack)
	a.Cbz(rD, haveP, true)
	a.Load(rD, rCtx, scr(sBase))
	a.AddImm(rD, rD, 2, true)
	a.ShiftImm(Lsl, rD, rD, 4, true)
	a.Op(Add, rC, rC, rD, true)
	a.Bind(haveP)
	a.AddImm(rW, rS, int64(offFLocals), true)
	g.ptrTo()
	// new.target: undefined.
	a.Load(rS, rCtx, scr(sNF))
	a.Load(r8, rCtx, ncUndef)
	a.Store(rS, offFNewTgt, r8)
	a.MovImm(rC, 0)
	a.AddImm(rW, rS, int64(offFNewTgt+8), true)
	g.ptrTo()
	a.Bind(done)
}

// addProp is set_prop's addition of the property by the transition the
// site at rC remembers, as amd64's: the object in rS, the value at the
// address in rD.
func (g *armGen) addProp() {
	a := g.a
	p1ok, p2ok, idxok := a.NewLabel(), a.NewLabel(), a.NewLabel()
	shapeOff := off("Object.shape")
	a.Load(r8, rC, off("propCache.shape"))
	g.cmpMem(r8, rS, shapeOff)
	a.BCond(NE, g.exit)
	a.Load(r8, rC, off("propCache.next"))
	a.Cbz(r8, g.exit, true)
	g.cmpMem(r8, rCtx, ncSetterNx)
	a.BCond(EQ, g.exit)
	a.LoadU16(r9, rS, offObjFlags)
	a.TstImm(r9, uint64(objExtensible), false)
	a.BCond(EQ, g.exit)
	a.Load(r9, rC, off("propCache.p1"))
	a.Cbz(r9, p1ok, true)
	a.Load(r9, r9, shapeOff)
	g.cmpMem(r9, rC, off("propCache.s1"))
	a.BCond(EQ, p1ok)
	a.Load(r9, rC, off("propCache.cell"))
	a.Cbz(r9, g.exit, true)
	a.LoadU8(r9, r9, offCellValid)
	a.Cbz(r9, g.exit, false)
	a.Bind(p1ok)
	a.Load(r9, rC, off("propCache.p2"))
	a.Cbz(r9, p2ok, true)
	a.Load(r9, r9, shapeOff)
	g.cmpMem(r9, rC, off("propCache.s2"))
	a.BCond(NE, g.exit)
	a.Bind(p2ok)
	// Room for the value; a shape of no index past linearScanLimit
	// values is given one by Go.
	a.LoadU32(r9, rS, off("Object.valn"))
	a.LoadU32(rC, rS, off("Object.valc"))
	a.Cmp(r9, rC, true)
	a.BCond(HS, g.exit)
	g.cmpMemImm(r8, off("shape.index"), 0)
	a.BCond(NE, idxok)
	a.CmpImm(r9, int64(linearScanLimit), true)
	a.BCond(HS, g.exit)
	a.Bind(idxok)
	a.Store(rCtx, scr(sE), rS)
	a.Store(rCtx, scr(sRC), r8)
	// The value at valn, then valn past it, then the shape.
	a.ShiftImm(Lsl, rW, r9, 4, true)
	g.addMem(rW, rS, off("Object.valp"))
	a.AddImm(r9, r9, 1, true)
	a.Load(rC, rD, 0)
	a.Store(rW, 0, rC)
	a.Load(rC, rD, 8)
	a.Store32(rS, off("Object.valn"), r9)
	a.AddImm(rW, rW, 8, true)
	g.ptrStore()
	a.Load(rS, rCtx, scr(sE))
	a.Load(rC, rCtx, scr(sRC))
	a.AddImm(rW, rS, int64(shapeOff), true)
	g.ptrStore()
	g.next()
}

// ptrTo stores the pointer in rC at the address in rW, by ptrStore,
// where the word there differs.
func (g *armGen) ptrTo() {
	a := g.a
	same := a.NewLabel()
	g.cmpMem(rC, rW, 0)
	a.BCond(EQ, same)
	g.ptrStore()
	a.Bind(same)
}

// call is call_w and call_method_w, as amd64's: a callee the site's cache
// knows, entered as regCallW enters it and run here, its caller's state
// pushed on the native frame stack; anything else Go's, by an exit before
// anything is written.
func (g *armGen) call() {
	a := g.a
	ok := a.NewLabel()
	start, tmpl, builtin, miss := a.NewLabel(), a.NewLabel(), a.NewLabel(), a.NewLabel()
	a.Bind(start)

	// e := &calls[C].
	a.ShiftImm(Lsr, rC, rW, 48, true)
	a.Load(r9, rCtx, ncCC)
	g.cmpMem(rC, r9, offCCRegCalls+8)
	a.BCond(HS, g.ex(1))
	a.MovImm(r8, uint64(sizeE))
	a.Mul(rC, rC, r8)
	g.addMem(rC, r9, offCCRegCalls)
	// The window, its callee the cache's.
	g.regA(rS)
	a.Load(r9, rS, 0)
	g.cmpMem(r9, rCtx, ncObjBits)
	a.BCond(NE, g.ex(2))
	a.Load(r9, rS, 8)
	g.cmpMem(r9, rC, offECallee)
	a.BCond(NE, tmpl)
	// A leaf that returns a parameter, a constant or an upvalue first
	// thing: its answer read from the window, the code or the closure,
	// with no frame.
	notLeaf, leafStore := a.NewLabel(), a.NewLabel()
	a.LoadU8(r9, rC, offEKind)
	a.TstImm(r9, uint64(regCallLeaf), false)
	a.BCond(EQ, notLeaf)
	a.Load(r8, rC, offERC)
	a.LoadU8(r9, r8, offRCLeaf)
	upLeaf := a.NewLabel()
	a.CmpImm(r9, int64(regLeafCode), false)
	a.BCond(EQ, upLeaf)
	a.CmpImm(r9, int64(regLeafArg), false)
	thisLeaf := notLeaf
	if g.v == plain {
		thisLeaf = upLeaf
	}
	a.BCond(LO, thisLeaf)
	a.Load(rD, r8, offRCLeafVal+8)
	a.Load(rC, r8, offRCLeafVal)
	a.BCond(NE, leafStore)
	// regLeafArg: the parameter where the call gave it, else undefined.
	a.Load(rC, rCtx, ncUndef)
	a.MovImm(rD, 0)
	a.LoadS32(r9, r8, offRCLeafArg)
	a.Ubfx(r8, rW, 8, 8)
	a.Cmp(r9, r8, true)
	a.BCond(GE, leafStore)
	a.AddLsl(r9, rS, r9, 4)
	a.Load(rC, r9, 32)
	a.Load(rD, r9, 40)
	a.Bind(leafStore)
	a.MovRR(r9, rC)
	a.MovRR(r8, rD)
	g.regB(rW)
	g.storeValue()
	// A regLeafCode body that returns an upvalue first thing (leafUp).
	a.Bind(upLeaf)
	a.Load(r9, rC, offECl)
	a.LoadS32(rD, r8, offRCLeafUp)
	a.SubsImm(rD, rD, 1, true)
	mathLeaf := notLeaf
	if g.v == plain {
		mathLeaf = a.NewLabel()
		g.after = append(g.after, func() { g.binaryLeaf(mathLeaf, notLeaf) })
	}
	a.BCond(MI, mathLeaf)
	g.cmpMem(rD, r9, offClUpvals+8)
	a.BCond(HS, notLeaf)
	a.Load(r9, r9, offClUpvals)
	a.LoadIdx(r9, r9, rD, true)
	a.Load(r9, r9, offUpSlot)
	a.Load(rC, r9, 0)
	a.Load(rD, r9, 8)
	a.B(leafStore)
	a.Bind(notLeaf)
	// A callee rhCallW leaves to regCallMiss (regCallOut) that keeps its
	// arguments, entered as regEnterW enters it (sEnt), and a leaf Go
	// answers without a frame (regCallLeaf); its kind the cache's
	// without the marks.
	plainK := a.NewLabel()
	g.storeImm(rCtx, scr(sEnt), 0)
	a.LoadU8(r9, rC, offEKind)
	a.TstImm(r9, uint64(regCallOut|regCallLeaf), false)
	a.BCond(EQ, plainK)
	a.AndImm(r9, r9, uint64(^(regCallOut|regCallLeaf)&0xff), false)
	a.Load(r8, rC, offERC)
	a.LoadU8(rD, r8, offRCArgs)
	a.Cbz(rD, plainK, false)
	g.storeImm(rCtx, scr(sEnt), 1)
	a.Bind(plainK)
	// Entered as it is, with the cache's this, or with an object this.
	a.CmpImm(r9, int64(regCallEnter), false)
	a.BCond(EQ, ok)
	a.CmpImm(r9, int64(regCallThis), false)
	a.BCond(EQ, ok)
	a.CmpImm(r9, int64(regCallObjThis), false)
	a.BCond(NE, builtin)
	a.Load(r8, rS, 16)
	g.cmpMem(r8, rCtx, ncObjBits)
	a.BCond(NE, g.ex(5))
	a.Bind(ok)
	jitCall := a.NewLabel()
	g.enter(enterCall, jitCall)

	// Another closure of the cached callee's template, as amd64's: the
	// cache made the new callee's and the call made again.
	a.Bind(tmpl)
	a.LoadU8(r8, rC, offEKind)
	a.AndImm(r8, r8, uint64(^(regCallOut|regCallLeaf)&0xff), false)
	a.CmpImm(r8, int64(regCallEnter), false)
	a.BCond(LO, miss)
	a.CmpImm(r8, int64(regCallObjThis), false)
	a.BCond(HI, miss)
	a.LoadU16(rD, r9, offObjFlags)
	a.TstImm(rD, uint64(objHasData), false)
	a.BCond(EQ, miss)
	a.Load(rD, r9, sizeObj+offExtData)
	g.cmpMem(rD, rCtx, ncFuncT)
	a.BCond(NE, miss)
	a.Load(rD, r9, sizeObj+offExtData+8)
	a.LoadU8(r8, rD, offFDBound)
	a.Cbnz(r8, miss, false)
	a.LoadU8(r8, rD, off("funcData.arrow"))
	a.Cbnz(r8, miss, false)
	a.Load(r8, rD, off("funcData.extra"))
	a.Cbnz(r8, miss, true)
	a.Load(rD, rD, off("funcData.closure"))
	a.Cbz(rD, miss, true)
	a.Load(r8, rC, offECl)
	a.Cbz(r8, miss, true)
	a.Load(rJ, rD, offClCode)
	g.cmpMem(rJ, r8, offClCode)
	a.BCond(NE, miss)
	a.Load(rJ, rD, off("closure.realm"))
	g.cmpMem(rJ, r8, off("closure.realm"))
	a.BCond(NE, miss)
	a.Load(rJ, rD, offClCode)
	g.addMem(rJ, rCtx, ncCCBase)
	a.Load(rJ, rJ, offCCReg)
	a.Cbz(rJ, miss, true)
	g.cmpMem(rJ, rC, offERC)
	a.BCond(NE, miss)
	a.Store(rCtx, scr(sE), rC)
	a.Store(rCtx, scr(sV0), rD)
	a.AddImm(rW, rC, int64(offECallee), true)
	a.MovRR(rC, r9)
	g.ptrTo()
	a.Load(rW, rCtx, scr(sE))
	a.AddImm(rW, rW, int64(offECl), true)
	a.Load(rC, rCtx, scr(sV0))
	g.ptrTo()
	a.Load(rW, rIP, 0)
	a.B(start)

	// A built-in the site cached (regCallNative), as amd64's: push of one
	// argument is the code's own, any other called with no exit.
	light := a.NewLabel()
	a.Bind(builtin)
	a.CmpImm(r9, int64(regCallNative), false)
	a.BCond(NE, g.ex(4))
	notPush := a.NewLabel()
	g.push(notPush)
	reflect := a.NewLabel()
	a.Bind(notPush)
	g.genNext(reflect)
	a.Bind(reflect)
	g.reflect(light)
	a.Bind(light)
	g.light(ncBuiltinFn)
	// Another callee than the cache's, of another template: the exit to
	// nativeCallMiss, as amd64's.
	a.Bind(miss)
	g.through()
	// A callee whose calls go to the JIT, as amd64's: nativeJITCall called
	// with no exit.
	a.Bind(jitCall)
	g.light(ncJITCallFn)
}

// enter is the call handler's entry of the callee its site's cache knows,
// rC the cache's entry, rS the window, r9 the kind it is entered as and
// the scratch's sEnt whether it keeps its arguments, as amd64's; a
// construction's frame (new_w's) is regFrameConstruct, its new.target the
// callee, and a super()'s regFrameSuper, as amd64's.
func (g *armGen) enter(mode int, jitCall Label) {
	a := g.a
	noThis, noTop, loop, loopEnd, skip, high := a.NewLabel(), a.NewLabel(), a.NewLabel(), a.NewLabel(), a.NewLabel(), a.NewLabel()
	// A callee whose calls go to the JIT (closureCode's jitEnters), as
	// amd64's: nativeJITCall's, called with no exit.
	a.Load(r8, rC, offECl)
	a.Load(r8, r8, offClCode)
	g.addMem(r8, rCtx, ncCCBase)
	a.LoadU8(rD, r8, offCCJITEnters)
	a.Cbnz(rD, jitCall, false)
	a.Store(rCtx, scr(sE), rC)
	a.Store(rCtx, scr(sWin), rS)
	a.Store(rCtx, scr(sKind), r9)
	a.Load(r8, rC, offERC)
	a.Store(rCtx, scr(sRC), r8)
	// The arguments where they lie: none past the parameters it keeps,
	// and no new.target.
	if mode == enterSuper || mode == enterThrough || mode == enterApply || mode == enterReflect {
		a.Load(rD, rCtx, scr(sArgc))
	} else {
		a.Ubfx(rD, rW, 8, 8)
		a.Store(rCtx, scr(sArgc), rD)
	}
	kept := a.NewLabel()
	g.cmpMemImm(rCtx, scr(sEnt), 0)
	a.BCond(NE, kept)
	a.LoadU16(r9, r8, offRCKeep)
	a.Cmp(rD, r9, true)
	a.BCond(HI, g.ex(6))
	a.Bind(kept)
	if mode == enterCall || mode == enterThrough || mode == enterApply {
		a.LoadU8(r9, r8, offRCNewTarget)
		a.Cbnz(r9, g.ex(7), false)
	}
	// The window's base, and the frame's top, in the stack.
	a.Load(r9, rCtx, ncRT)
	a.Load(rC, r9, offRtStack)
	a.Op(Sub, rD, rS, rC, true)
	a.ShiftImm(Lsr, rD, rD, 4, true)
	a.Store(rCtx, scr(sBase), rD)
	a.LoadS32(rC, r8, offRCNumRegs)
	a.Op(Add, rC, rC, rD, true)
	g.cmpMem(rC, r9, offRtStack+8)
	a.BCond(HI, g.ex(8))
	a.Store(rCtx, scr(sTop), rC)
	// The depth, the interrupt budget, the frame record.
	a.Load(rD, r9, offRtFrameDepth)
	g.cmpMem(rD, r9, offRtMaxFrames)
	a.BCond(GE, g.ex(9))
	g.cmpMemImm(r9, offRtInterrupt, 1)
	a.BCond(LE, g.ex(10))
	a.Load(r8, r9, offRtFrameBase)
	a.Op(Sub, rS, rD, r8, true)
	g.cmpMem(rS, r9, offRtCur+8)
	a.BCond(HS, g.ex(11))
	a.MovImm(r8, uint64(sizeFrame))
	a.Mul(rS, rS, r8)
	g.addMem(rS, r9, offRtCur)
	// What a frame's last user left set: one test of the five words or'd.
	a.Load(r8, rS, offFThisRef)
	for _, off := range []int32{offFEvalVars, offFWith, offFNative + 8} {
		a.Load(r9, rS, off)
		a.Op(Orr, r8, r8, r9, true)
	}
	a.Cbnz(r8, g.ex(12), true)
	a.Store(rCtx, scr(sNF), rS)
	// The arguments an entered frame keeps go on the runtime's argument
	// stack, which must have the room: Go grows it.
	noKeep := a.NewLabel()
	g.cmpMemImm(rCtx, scr(sEnt), 0)
	a.BCond(EQ, noKeep)
	a.Load(r9, rCtx, ncRT)
	g.cmpMemImm(r9, offRtArgStack, 0)
	a.BCond(EQ, g.ex(14))
	a.Load(rC, r9, offRtArgStack+8)
	g.addMem(rC, rCtx, scr(sArgc))
	g.cmpMem(rC, r9, offRtArgStack+16)
	a.BCond(HI, g.ex(14))
	a.Bind(noKeep)
	a.Load(r8, rCtx, ncDepth)
	g.cmpMem(r8, rCtx, ncCap)
	a.BCond(GE, g.exitRet)

	// Nothing is written before here; the caller's pc is Go's to write
	// (nativeFillPCs). The this the cache gives.
	a.Load(r9, rCtx, scr(sKind))
	a.CmpImm(r9, int64(regCallThis), false)
	a.BCond(NE, noThis)
	a.Load(rC, rCtx, scr(sE))
	a.Load(r8, rC, offEThis)
	a.Load(r9, rC, offEThis+8)
	a.Load(rW, rCtx, scr(sWin))
	a.Store(rW, 16, r8)
	a.AddImm(rW, rW, 24, true)
	// The pointer word, where the window does not hold it already.
	g.cmpMem(r9, rW, 0)
	a.BCond(EQ, noThis)
	a.MovRR(rC, r9)
	g.ptrStore()
	a.Bind(noThis)
	// The interrupt budget, the stack top.
	a.Load(r9, rCtx, ncRT)
	g.incMem(r9, offRtInterrupt, -1)
	a.Load(rC, r9, offRtStackTop)
	a.Store(rCtx, scr(sSaved), rC)
	a.Load(rD, rCtx, scr(sTop))
	a.Cmp(rD, rC, true)
	a.BCond(LE, noTop)
	a.Store(r9, offRtStackTop, rD)
	g.cmpMem(rD, r9, offRtStackHigh)
	a.BCond(LE, noTop)
	a.Store(r9, offRtStackHigh, rD)
	a.Bind(noTop)
	g.keepArgs()
	// Parameters no argument was given for, and the bindings past them:
	// undefined, from min(argc, params) to params+inits.
	a.Load(r8, rCtx, scr(sRC))
	a.LoadS32(rC, r8, offRCParams)
	a.Load(rD, rCtx, scr(sArgc))
	a.Cmp(rD, rC, true)
	from := a.NewLabel()
	a.BCond(GE, from)
	a.MovRR(rC, rD)
	a.Bind(from)
	a.LoadS32(r9, r8, offRCParams)
	a.LoadS32(rS, r8, offRCInits)
	a.Op(Add, r9, r9, rS, true)
	// i in rC and the end in r9, kept in the scratch only across a store
	// of a pointer word.
	a.Bind(loop)
	a.Cmp(rC, r9, true)
	a.BCond(GE, loopEnd)
	a.ShiftImm(Lsl, rW, rC, 4, true)
	g.addMem(rW, rCtx, scr(sWin))
	a.Load(r8, rCtx, ncUndef)
	a.Store(rW, 32, r8)
	a.Load(rT, rW, 40)
	a.Cbz(rT, skip, true)
	a.Store(rCtx, scr(sI), rC)
	a.Store(rCtx, scr(sEnd), r9)
	a.AddImm(rW, rW, 40, true)
	a.MovImm(rC, 0)
	g.ptrStore()
	a.Load(rC, rCtx, scr(sI))
	a.Load(r9, rCtx, scr(sEnd))
	a.Bind(skip)
	a.AddImm(rC, rC, 1, true)
	a.B(loop)
	a.Bind(loopEnd)
	// The depth.
	a.Load(r9, rCtx, ncRT)
	g.incMem(r9, offRtFrameDepth, 1)
	a.Load(rC, r9, offRtFrameDepth)
	g.cmpMem(rC, r9, offRtFrameHigh)
	a.BCond(LE, high)
	a.Store(r9, offRtFrameHigh, rC)
	a.Bind(high)
	// The frame record: its base, pc 0, no open upvalues, a call's kind;
	// its closure and callee, written where they differ.
	a.Load(rS, rCtx, scr(sNF))
	a.Load(rC, rCtx, scr(sBase))
	a.Store(rS, offFBase, rC)
	a.Store32(rS, offFPC, ZR)
	a.Store(rS, offFOpenUp+8, ZR)
	a.MovImm(rC, uint64(regFrameCall))
	a.Store8(rS, offFRegKind, rC)
	g.frameArgs()
	if mode == enterNew || mode == enterSuper || mode == enterReflect {
		// A construction's or a super()'s, as amd64's.
		kind := regFrameConstruct
		if mode == enterSuper {
			kind = regFrameSuper
		}
		a.Load(rS, rCtx, scr(sNF))
		a.MovImm(rC, uint64(kind))
		a.Store8(rS, offFRegKind, rC)
		a.Load(r8, rCtx, ncObjBits)
		a.Store(rS, offFNewTgt, r8)
		if mode == enterSuper || mode == enterReflect {
			a.Load(rC, rCtx, scr(sV1))
		} else {
			a.Load(rC, rCtx, scr(sE))
			a.Load(rC, rC, offECallee)
		}
		a.AddImm(rW, rS, int64(offFNewTgt+8), true)
		g.ptrTo()
		noRef := a.NewLabel()
		a.Load(rT, rCtx, scr(sV0))
		a.Cbz(rT, noRef, true)
		a.Load(rS, rCtx, scr(sNF))
		a.Load(rC, rCtx, scr(sV0))
		a.AddImm(rW, rS, int64(offFThisRef), true)
		g.ptrStore()
		a.Load(rS, rCtx, scr(sNF))
		a.Load(rD, rCtx, scr(sWin))
		a.Load(r8, rD, 16)
		a.Store(rS, offFThis, r8)
		a.Load(rC, rD, 24)
		a.AddImm(rW, rS, int64(offFThis+8), true)
		g.ptrTo()
		a.Bind(noRef)
	}
	for _, f := range [][2]int32{{offECl, offFCl}, {offECallee, offFCallee}} {
		same := a.NewLabel()
		a.Load(rC, rCtx, scr(sE))
		a.Load(r8, rC, f[0])
		a.Load(rS, rCtx, scr(sNF))
		a.AddImm(rW, rS, int64(f[1]), true)
		g.cmpMem(r8, rW, 0)
		a.BCond(EQ, same)
		a.MovRR(rC, r8)
		g.ptrStore()
		a.Bind(same)
	}
	// The caller's state, on the native frame stack.
	a.Load(r8, rCtx, ncDepth)
	a.MovImm(r9, uint64(recSize))
	a.Mul(r8, r8, r9)
	g.addMem(r8, rCtx, ncFrames)
	a.MovRR(rS, r8)
	a.Store(rS, recRetIP, rIP)
	for _, f := range nativeTopFields {
		a.Load(rC, rCtx, f[1])
		a.Store(rS, f[0], rC)
	}
	a.Load(rC, rCtx, scr(sSaved))
	a.Store(rS, recSaved, rC)
	a.Load(rW, rIP, 0)
	if mode == enterSuper || mode == enterApply {
		// super()'s R[A], which its return writes (superRet), and
		// apply_args's.
		a.Ubfx(rC, rW, 16, 16)
	} else {
		a.Ubfx(rC, rW, 32, 16)
	}
	a.ShiftImm(Lsl, rC, rC, 4, true)
	a.Store(rS, recBOff, rC)
	g.incMem(rCtx, ncDepth, 1)
	g.storeImm(rCtx, ncTopOK, 0)
	// The callee's, the top frame's: its window, its closure's caches,
	// constants, epochs and call sites, its code and frame record.
	a.Load(rRegs, rCtx, scr(sWin))
	a.Store(rCtx, ncRegs, rRegs)
	a.Load(rC, rCtx, scr(sE))
	a.Load(r8, rC, offECl)
	a.Load(r8, r8, offClCode)
	g.addMem(r8, rCtx, ncCCBase)
	a.Store(rCtx, ncCC, r8)
	a.Load(rIC, r8, offCCIC)
	a.Store(rCtx, ncIC, rIC)
	a.Load(r8, rCtx, scr(sRC))
	a.Store(rCtx, ncRC, r8)
	a.Load(rIP, r8, offRCCode)
	a.Store(rCtx, ncCodeBase, rIP)
	a.Load(rC, rCtx, scr(sNF))
	a.Store(rCtx, ncNF, rC)
	// The callee's feedback vector: none yet.
	g.storeImm(rCtx, ncFV, 0)
	// Run it, counted (made).
	g.incMem(rCtx, ncMade, 1)
	g.dispatch()
}

// newW is new_w of the constructor its site's cache holds, as amd64's:
// nativeNew called through callH with no exit, then the constructor
// entered as call_w enters a callee, its frame a construction's.
func (g *armGen) newW() {
	a := g.a
	after, done := a.NewLabel(), a.NewLabel()
	g.newSite()
	a.LoadU8(r9, rC, offEKind)
	a.CmpImm(r9, int64(regCallConstruct), false)
	a.BCond(NE, g.exit)
	a.Load(r8, rC, offECl)
	a.Load(r8, r8, offClCode)
	g.addMem(r8, rCtx, ncCCBase)
	a.LoadU8(rD, r8, offCCJITEnters)
	a.Cbnz(rD, g.exit, false)
	// nativeNew(regs, w, pc, ctx), as litCall calls nativeLit.
	a.Store(rCtx, ncIP, rIP)
	a.Store(rCtx, ncBudget, rBud)
	a.Adr(r8, after)
	a.Store(rCtx, ncAfter, r8)
	a.MovRR(rC, rW)
	a.Load(rD, rCtx, ncCodeBase)
	a.Op(Sub, rD, rIP, rD, true)
	a.ShiftImm(Lsr, rD, rD, 3, true)
	a.MovRR(rS, rCtx)
	a.MovRR(rW, rRegs)
	a.Load(rFn, rCtx, ncNewFn)
	a.Load(rJ, rCtx, ncCallH)
	a.Br(rJ)
	a.Bind(after)
	g.reload()
	// 0: Go's; 2: done, a body leafCall answered; 1: entered.
	a.Cbz(rW, g.exit, true)
	a.CmpImm(rW, 2, true)
	a.BCond(EQ, done)
	a.Load(rW, rIP, 0)
	g.newSite()
	plain := a.NewLabel()
	g.storeImm(rCtx, scr(sEnt), 0)
	a.Load(r8, rC, offERC)
	a.LoadU8(rD, r8, offRCArgs)
	a.Cbz(rD, plain, false)
	g.storeImm(rCtx, scr(sEnt), 1)
	a.Bind(plain)
	a.MovImm(r9, uint64(regCallEnter))
	g.enter(enterNew, g.exit)
	a.Bind(done)
	g.next()
}

// through is call_method_w of the realm's Function.prototype.call whose
// callee the site's cache holds, as amd64's; anything else the exit to
// nativeCallMiss.
func (g *armGen) through() {
	a := g.a
	miss, ok := a.NewLabel(), a.NewLabel()
	a.LoadU8(r8, rC, offEKind)
	a.TstImm(r8, uint64(regCallThrough), false)
	a.BCond(EQ, miss)
	a.Load(rD, rCtx, ncRT)
	a.Load(rD, rD, rtRel(0))
	g.addMem(rD, rCtx, ncCallFn)
	a.Load(rD, rD, 0)
	g.cmpMem(rD, rS, 8)
	a.BCond(NE, miss)
	a.Load(r9, rS, 16)
	g.cmpMem(r9, rCtx, ncObjBits)
	a.BCond(NE, miss)
	a.Load(r9, rS, 24)
	g.cmpMem(r9, rC, offECallee)
	a.BCond(NE, miss)
	a.Load(rW, rIP, 0)
	a.Ubfx(rD, rW, 8, 8)
	a.SubsImm(rD, rD, 1, true)
	a.BCond(MI, miss)
	a.Store(rCtx, scr(sArgc), rD)
	a.AndImm(r9, r8, uint64(^regCallThrough&0xff), false)
	notNative := a.NewLabel()
	a.CmpImm(r9, int64(regCallNative), false)
	a.BCond(NE, notNative)
	a.MovImm(rT, 0x10000-0x100)
	a.Op(Add, rW, rW, rT, true)
	g.lightW(ncBuiltinFn, true)
	a.Bind(notNative)
	a.CmpImm(r9, int64(regCallEnter), false)
	a.BCond(EQ, ok)
	a.CmpImm(r9, int64(regCallThis), false)
	a.BCond(EQ, ok)
	a.CmpImm(r9, int64(regCallObjThis), false)
	a.BCond(NE, miss)
	a.Load(r8, rS, 32)
	g.cmpMem(r8, rCtx, ncObjBits)
	a.BCond(NE, miss)
	a.Bind(ok)
	a.Load(r8, rC, offERC)
	a.LoadU8(rD, r8, offRCArgs)
	a.Store(rCtx, scr(sEnt), rD)
	a.AddImm(rS, rS, 16, true)
	g.enter(enterThrough, g.exit)
	a.Bind(miss)
	g.storeImm(rCtx, ncMiss, 1)
	a.B(g.exit)
}

// reflect is a call of the realm's Reflect.construct, as amd64's:
// nativeReflect called through callH with no exit, then the constructor
// entered; anything else the built-in's call, at light.
func (g *armGen) reflect(light Label) {
	a := g.a
	after, done := a.NewLabel(), a.NewLabel()
	a.ShiftImm(Lsr, rC, rW, 48, true)
	a.Load(r9, rCtx, ncCC)
	a.MovImm(r8, uint64(sizeE))
	a.Mul(rC, rC, r8)
	g.addMem(rC, r9, offCCRegCalls)
	a.Load(r8, rC, offECallee)
	a.Load(rD, rCtx, ncRT)
	a.Load(rD, rD, rtRel(0))
	g.addMem(rD, rCtx, ncReflect)
	g.cmpMem(r8, rD, 0)
	a.BCond(NE, light)
	a.Store(rCtx, ncIP, rIP)
	a.Store(rCtx, ncBudget, rBud)
	a.Adr(r8, after)
	a.Store(rCtx, ncAfter, r8)
	a.MovRR(rC, rW)
	a.Load(rD, rCtx, ncCodeBase)
	a.Op(Sub, rD, rIP, rD, true)
	a.ShiftImm(Lsr, rD, rD, 3, true)
	a.MovRR(rS, rCtx)
	a.MovRR(rW, rRegs)
	a.Load(rFn, rCtx, ncReflectFn)
	a.Load(rJ, rCtx, ncCallH)
	a.Br(rJ)
	a.Bind(after)
	g.reload()
	a.MovRR(rC, rW)
	a.Load(rW, rIP, 0)
	a.Cbz(rC, light, true)
	a.CmpImm(rC, 2, true)
	a.BCond(EQ, done)
	a.Load(rC, rCtx, scr(sE))
	a.Load(rS, rCtx, scr(sWin))
	a.Load(r8, rC, offERC)
	a.LoadU8(rD, r8, offRCArgs)
	a.Store(rCtx, scr(sEnt), rD)
	a.MovImm(r9, uint64(regCallEnter))
	g.enter(enterReflect, g.exit)
	a.Bind(done)
	g.next()
}

// applyW is apply_args, as amd64's: nativeApply called through callH
// with no exit, then the target entered.
func (g *armGen) applyW() {
	a := g.a
	after := a.NewLabel()
	// A target of no closure exits at once, as amd64's.
	g.regB(rD)
	g.object(rS, rD)
	a.LoadU16(rD, rS, offObjFlags)
	a.TstImm(rD, uint64(objHasData), false)
	a.BCond(EQ, g.exit)
	a.Load(rD, rS, sizeObj+offExtData)
	g.cmpMem(rD, rCtx, ncFuncT)
	a.BCond(NE, g.exit)
	a.Load(rD, rS, sizeObj+offExtData+8)
	a.Load(rT, rD, off("funcData.closure"))
	a.Cbz(rT, g.exit, true)
	a.Store(rCtx, ncIP, rIP)
	a.Store(rCtx, ncBudget, rBud)
	a.Adr(r8, after)
	a.Store(rCtx, ncAfter, r8)
	a.MovRR(rC, rW)
	a.Load(rD, rCtx, ncCodeBase)
	a.Op(Sub, rD, rIP, rD, true)
	a.ShiftImm(Lsr, rD, rD, 3, true)
	a.MovRR(rS, rCtx)
	a.MovRR(rW, rRegs)
	a.Load(rFn, rCtx, ncApplyFn)
	a.Load(rJ, rCtx, ncCallH)
	a.Br(rJ)
	a.Bind(after)
	g.reload()
	a.Cbz(rW, g.exit, true)
	a.Load(rW, rIP, 0)
	a.Load(rC, rCtx, scr(sE))
	a.Load(rS, rCtx, scr(sWin))
	a.Load(r8, rC, offERC)
	a.LoadU8(rD, r8, offRCArgs)
	a.Store(rCtx, scr(sEnt), rD)
	a.MovImm(r9, uint64(regCallEnter))
	g.enter(enterApply, g.exit)
}

// superW is super_call and super_forward, as amd64's: nativeSuper called
// through callH with no exit, then the parent entered, its frame a
// super()'s.
func (g *armGen) superW() {
	a := g.a
	after, plain := a.NewLabel(), a.NewLabel()
	a.Store(rCtx, ncIP, rIP)
	a.Store(rCtx, ncBudget, rBud)
	a.Adr(r8, after)
	a.Store(rCtx, ncAfter, r8)
	a.MovRR(rC, rW)
	a.Load(rD, rCtx, ncCodeBase)
	a.Op(Sub, rD, rIP, rD, true)
	a.ShiftImm(Lsr, rD, rD, 3, true)
	a.MovRR(rS, rCtx)
	a.MovRR(rW, rRegs)
	a.Load(rFn, rCtx, ncSuperFn)
	a.Load(rJ, rCtx, ncCallH)
	a.Br(rJ)
	a.Bind(after)
	g.reload()
	a.Cbz(rW, g.exit, true)
	a.Load(rW, rIP, 0)
	a.Load(rC, rCtx, scr(sE))
	a.Load(rS, rCtx, scr(sWin))
	g.storeImm(rCtx, scr(sEnt), 0)
	a.Load(r8, rC, offERC)
	a.LoadU8(rD, r8, offRCArgs)
	a.Cbz(rD, plain, false)
	g.storeImm(rCtx, scr(sEnt), 1)
	a.Bind(plain)
	a.MovImm(r9, uint64(regCallEnter))
	g.enter(enterSuper, g.exit)
}

// newSite puts in rC new_w's site's cache entry and in rS its window,
// whose callee is the entry's, or exits.
func (g *armGen) newSite() {
	a := g.a
	a.ShiftImm(Lsr, rC, rW, 48, true)
	a.Load(r9, rCtx, ncCC)
	g.cmpMem(rC, r9, offCCRegCalls+8)
	a.BCond(HS, g.exit)
	a.MovImm(r8, uint64(sizeE))
	a.Mul(rC, rC, r8)
	g.addMem(rC, r9, offCCRegCalls)
	g.regA(rS)
	a.Load(r9, rS, 0)
	g.cmpMem(r9, rCtx, ncObjBits)
	a.BCond(NE, g.exit)
	a.Load(r9, rS, 8)
	g.cmpMem(r9, rC, offECallee)
	a.BCond(NE, g.exit)
}

// light is amd64's (amd64_calls.go): nativeBuiltin called through callH
// with the exit's protocol, returning to the exit stub's code after its
// call, its func value in R26.
func (g *armGen) light(fn int32) { g.lightW(fn, false) }

// lightW is light, the instruction its Go function is given rW where
// given, as amd64's.
func (g *armGen) lightW(fn int32, given bool) {
	a := g.a
	a.Load(rT, rCtx, ncTopOK)
	a.Cbz(rT, g.exit, true)
	if given {
		a.MovRR(rK, rW)
	}
	g.exitState()
	if given {
		a.MovRR(rW, rK)
	}
	a.Load(rD, rCtx, ncRT)
	a.Store(rD, offRtBackEdges, rBud)
	a.Adr(r8, g.exitAfter)
	a.Store(rCtx, ncAfter, r8)
	// nativeBuiltin(st.regs, w, pc, st): w in R1, pc in R2.
	a.MovRR(rD, rC)
	a.MovRR(rC, rW)
	a.Load(rFn, rCtx, fn)
	a.Load(rS, rCtx, ncTop)
	a.Load(rW, rS, offStRegs)
	a.Load(rJ, rCtx, ncCallH)
	a.Br(rJ)
}

// push is amd64's (amd64_calls.go): call_method_w of push, of one
// argument, appended to an array with room for it. rC is the site's
// cache entry, rS the window, rW the instruction.
func (g *armGen) push(light Label) {
	a := g.a
	a.Ubfx(rD, rW, 0, 8)
	a.CmpImm(rD, int64(bytecode.RCallMW), false)
	a.BCond(NE, light)
	a.Ubfx(rD, rW, 8, 8)
	a.CmpImm(rD, 1, false)
	a.BCond(NE, light)
	// The callee: push itself, not bound.
	a.Load(r9, rC, offECallee)
	a.LoadU16(rD, r9, offObjFlags)
	a.TstImm(rD, uint64(objHasData), false)
	a.BCond(EQ, light)
	a.Load(rD, r9, sizeObj+offExtData)
	g.cmpMem(rD, rCtx, ncFuncT)
	a.BCond(NE, light)
	a.Load(r9, r9, sizeObj+offExtData+8)
	a.LoadU8(rD, r9, offFDElemOp)
	a.CmpImm(rD, int64(elemPush), false)
	a.BCond(NE, light)
	a.LoadU8(rD, r9, offFDBound)
	a.Cbnz(rD, light, false)
	// The receiver, an array with room.
	a.Load(r9, rS, 16)
	g.cmpMem(r9, rCtx, ncObjBits)
	a.BCond(NE, light)
	a.Store(rCtx, scr(sWin), rS)
	a.Load(rS, rS, 24)
	g.appendRoom(rS, rC, light, light, true)
	// The argument at elems[len], then the length past it.
	a.AddImm(r9, rC, 1, true)
	a.Store(rCtx, scr(sI), r9)
	a.Load(rW, rS, sizeObj)
	a.AddLsl(rW, rW, rC, 4)
	a.Store(rS, sizeObj+8, r9)
	a.Load(rD, rCtx, scr(sWin))
	a.Load(r9, rD, 32)
	a.Store(rW, 0, r9)
	a.Load(rC, rD, 40)
	a.AddImm(rW, rW, 8, true)
	g.ptrStore()
	// R[B], the new length.
	a.Load(rW, rIP, 0)
	a.Load(rC, rCtx, scr(sI))
	a.Scvtf(0, rC, true)
	g.storeB(0)
}

// ret is ret and ret_undef of a frame the code entered itself, as
// amd64's: its caller's state off the native frame stack, the value in
// the caller's register. The base frame's return is Go's, as is a
// frame's with upvalues to close.
func (g *armGen) ret(undef bool) {
	a := g.a
	noCap, base, open := a.NewLabel(), a.NewLabel(), a.NewLabel()
	g.cmpMemImm(rCtx, ncDepth, 0)
	a.BCond(EQ, base)
	// A generator's step ends by genReturn, and a construction's frame
	// as constructed (ctorRet): kinds past the others.
	ctor := a.NewLabel()
	a.Load(rS, rCtx, ncNF)
	a.LoadU8(r8, rS, offFRegKind)
	a.CmpImm(r8, int64(regFrameGenStep), false)
	a.BCond(HS, ctor)
	// Upvalues open on the frame are closed here, as amd64's.
	a.Load(rC, rCtx, ncRC)
	a.LoadS32(r8, rC, offRCNcap)
	a.Cbnz(r8, open, false)
	a.LoadU8(r8, rS, offFRegKind)
	a.CmpImm(r8, int64(regFrameEnter), false)
	a.BCond(NE, noCap)
	a.Bind(open)
	g.cmpMemImm(rS, offFOpenUp+8, 0)
	a.BCond(EQ, noCap)
	a.Store(rCtx, scr(sV0), ZR)
	a.MovImm(rC, ^uint64(0))
	a.Store(rCtx, scr(sV1), rC)
	g.closeUps()
	a.Load(rW, rIP, 0)
	a.Bind(noCap)
	// The value.
	value := func() {
		if undef {
			a.Load(r8, rCtx, ncUndef)
			a.MovImm(r9, 0)
		} else {
			g.regA(rD)
			a.Load(r8, rD, 0)
			a.Load(r9, rD, 8)
		}
		a.Store(rCtx, scr(sV0), r8)
		a.Store(rCtx, scr(sV1), r9)
	}
	value()
	valued := a.NewLabel()
	a.Bind(valued)
	g.retPop(nil)
	// The base frame's return: its value in the scratch, retDone set, and
	// nativeRun returns it, as runReg would.
	a.Bind(base)
	value()
	g.storeImm(rCtx, ncRetDone, 1)
	a.B(g.exitRet)
	superRet := a.NewLabel()
	a.Bind(ctor)
	a.BCond(EQ, g.genRet)
	g.ctorRet(undef, valued, superRet)
	a.Bind(superRet)
	g.superRet(undef)
}

// retPop is ret's end of a frame the code entered, its value in the
// scratch's sV0 and sV1, and post's code, as amd64's.
func (g *armGen) retPop(post func()) {
	a := g.a
	// The frame closed; an entered frame's arguments given back to the
	// argument stack, as regEnterW gives them.
	a.Load(rS, rCtx, ncNF)
	a.Load(r9, rCtx, ncRT)
	notEnt := a.NewLabel()
	a.LoadU8(rC, rS, offFRegKind)
	a.CmpImm(rC, int64(regFrameEnter), false)
	a.BCond(NE, notEnt)
	a.Load(rC, rS, offFArgs+8)
	a.Load(rD, r9, offRtArgStack+8)
	a.Op(Sub, rD, rD, rC, true)
	a.Store(r9, offRtArgStack+8, rD)
	a.Bind(notEnt)
	a.Store8(rS, offFRegKind, ZR)
	g.incMem(r9, offRtFrameDepth, -1)
	a.Load(r8, rCtx, ncDepth)
	a.AddImm(r8, r8, -1, true)
	a.Store(rCtx, ncDepth, r8)
	g.storeImm(rCtx, ncTopOK, 0)
	low := a.NewLabel()
	g.cmpMem(r8, rCtx, ncFilled)
	a.BCond(GE, low)
	a.Store(rCtx, ncFilled, r8)
	a.Bind(low)
	a.MovImm(rC, uint64(recSize))
	a.Mul(r8, r8, rC)
	g.addMem(r8, rCtx, ncFrames)
	a.MovRR(rS, r8)
	a.Load(rC, rS, recSaved)
	a.Store(r9, offRtStackTop, rC)
	for _, f := range nativeTopFields {
		a.Load(rC, rS, f[0])
		a.Store(rCtx, f[1], rC)
	}
	// The caller at its call; the value in its register B.
	a.Load(rIP, rS, recRetIP)
	a.Load(rW, rCtx, ncRegs)
	g.addMem(rW, rS, recBOff)
	a.Load(rC, rCtx, scr(sV0))
	a.Store(rW, 0, rC)
	a.AddImm(rW, rW, 8, true)
	a.Load(rC, rCtx, scr(sV1))
	same := a.NewLabel()
	g.cmpMem(rC, rW, 0)
	a.BCond(EQ, same)
	g.ptrStore()
	a.Bind(same)
	a.Load(rRegs, rCtx, ncRegs)
	a.Load(rIC, rCtx, ncIC)
	if post != nil {
		post()
	}
	g.next()
}

// ctorRet is ret's of a construction's frame (regFrameConstruct), rS its
// frame record, as amd64's: the upvalues open on it closed, its value what
// it returns where that is an object, its this where not, and the frame
// closed as an entered one from valued on.
func (g *armGen) ctorRet(undef bool, valued, superRet Label) {
	a := g.a
	noCap, have, plain := a.NewLabel(), a.NewLabel(), a.NewLabel()
	a.CmpImm(r8, int64(regFrameSuper), false)
	a.BCond(EQ, superRet)
	// A derived class's, with its this binding: an object returned, or
	// Go's, as amd64's.
	a.Load(rT, rS, offFThisRef)
	a.Cbz(rT, plain, true)
	if undef {
		a.B(g.exit)
	} else {
		g.regA(rD)
		a.Load(r8, rD, 0)
		g.cmpMem(r8, rCtx, ncObjBits)
		a.BCond(NE, g.exit)
	}
	a.Bind(plain)
	g.cmpMemImm(rS, offFOpenUp+8, 0)
	a.BCond(EQ, noCap)
	a.Store(rCtx, scr(sV0), ZR)
	a.MovImm(rC, ^uint64(0))
	a.Store(rCtx, scr(sV1), rC)
	g.closeUps()
	a.Load(rW, rIP, 0)
	a.Bind(noCap)
	g.endThisRef()
	if !undef {
		g.regA(rD)
		a.Load(r8, rD, 0)
		a.Load(r9, rD, 8)
		g.cmpMem(r8, rCtx, ncObjBits)
		a.BCond(EQ, have)
	}
	a.Load(r8, rRegs, 16)
	a.Load(r9, rRegs, 24)
	a.Bind(have)
	a.Store(rCtx, scr(sV0), r8)
	a.Store(rCtx, scr(sV1), r9)
	a.Load(rS, rCtx, ncNF)
	a.MovImm(rC, uint64(regFrameEnter))
	a.Store8(rS, offFRegKind, rC)
	a.B(valued)
}

// endThisRef lets go of the top frame's this binding, as amd64's. rW is
// the instruction after it.
func (g *armGen) endThisRef() {
	a := g.a
	none, noCell := a.NewLabel(), a.NewLabel()
	a.Load(rS, rCtx, ncNF)
	a.Load(rT, rS, offFThisRef)
	a.Cbz(rT, none, true)
	a.Load(r8, rCtx, ncRC)
	a.LoadU8(r9, r8, off("regCode.cellThis"))
	a.Cbz(r9, noCell, false)
	a.Load(rW, rS, offFThisRef)
	a.Load(r8, rCtx, ncUndef)
	a.Store(rW, off("thisBinding.value"), r8)
	a.AddImm(rW, rW, int64(off("thisBinding.value")+8), true)
	a.MovImm(rC, 0)
	g.ptrTo()
	a.Bind(noCell)
	a.Load(rS, rCtx, ncNF)
	a.AddImm(rW, rS, int64(offFThisRef), true)
	a.MovImm(rC, 0)
	g.ptrStore()
	a.Load(rW, rIP, 0)
	a.Bind(none)
}

// superRet is ret's of a super()'s frame, as amd64's.
func (g *armGen) superRet(undef bool) {
	a := g.a
	noCap, keep := a.NewLabel(), a.NewLabel()
	a.Load(r8, rCtx, ncDepth)
	a.AddImm(r8, r8, -1, true)
	a.MovImm(rC, uint64(recSize))
	a.Mul(r8, r8, rC)
	g.addMem(r8, rCtx, ncFrames)
	a.Load(r8, r8, recNFOff)
	a.Load(r8, r8, offFThisRef)
	a.Cbz(r8, g.exit, true)
	a.LoadU8(r9, r8, off("thisBinding.init"))
	a.Cbnz(r9, g.exit, false)
	g.cmpMemImm(rS, offFOpenUp+8, 0)
	a.BCond(EQ, noCap)
	a.Store(rCtx, scr(sV0), ZR)
	a.MovImm(rC, ^uint64(0))
	a.Store(rCtx, scr(sV1), rC)
	g.closeUps()
	a.Load(rW, rIP, 0)
	a.Bind(noCap)
	g.endThisRef()
	if undef {
		a.Load(r8, rCtx, ncUndef)
		a.MovImm(r9, 0)
	} else {
		g.regA(rD)
		a.Load(r8, rD, 0)
		a.Load(r9, rD, 8)
	}
	a.Store(rCtx, scr(sV0), r8)
	a.Store(rCtx, scr(sV1), r9)
	a.Load(rS, rCtx, ncNF)
	a.MovImm(rC, uint64(regFrameEnter))
	a.Store8(rS, offFRegKind, rC)
	g.retPop(func() {
		a.Load(rS, rCtx, ncNF)
		a.Load(r8, rCtx, scr(sV0))
		g.cmpMem(r8, rCtx, ncObjBits)
		a.BCond(NE, keep)
		a.Load(rC, rCtx, scr(sV1))
		g.cmpMem(rC, rS, offFThis+8)
		a.BCond(EQ, keep)
		a.Store(rS, offFThis, r8)
		a.AddImm(rW, rS, int64(offFThis+8), true)
		g.ptrStore()
		a.Bind(keep)
		a.Load(rS, rCtx, ncNF)
		a.Load(rW, rS, offFThisRef)
		a.Load(r8, rS, offFThis)
		a.Store(rW, off("thisBinding.value"), r8)
		a.Load(rC, rS, offFThis+8)
		a.AddImm(rW, rW, int64(off("thisBinding.value")+8), true)
		g.ptrTo()
		a.Load(rS, rCtx, ncNF)
		a.Load(rW, rS, offFThisRef)
		a.MovImm(rC, 1)
		a.Store8(rW, off("thisBinding.init"), rC)
		for _, dst := range []bool{true, false} {
			if dst {
				a.Load(rW, rIP, 0)
				g.regA(rD)
			} else {
				a.AddImm(rD, rRegs, 16, true)
			}
			a.Load(rS, rCtx, ncNF)
			a.Load(r8, rS, offFThis)
			a.Store(rD, 0, r8)
			a.Load(rC, rS, offFThis+8)
			a.AddImm(rW, rD, 8, true)
			g.ptrTo()
		}
	})
}

// tailTo is tail_call_w's and tail_call_method_w's call once the stack is
// deep, as amd64's: the frame the code entered by call_w given to the
// callee its site's cache knows in place, as regTailTo gives it; anything
// else exits before anything is written.
func (g *armGen) tailTo() {
	a := g.a
	thisOK, loop, loopEnd, undef, undefEnd, skip, noTop, argc := a.NewLabel(), a.NewLabel(), a.NewLabel(), a.NewLabel(), a.NewLabel(), a.NewLabel(), a.NewLabel(), a.NewLabel()
	g.cmpMemImm(rCtx, ncDepth, 0)
	a.BCond(EQ, g.exit)
	a.Load(rS, rCtx, ncNF)
	a.LoadU8(r8, rS, offFRegKind)
	a.CmpImm(r8, int64(regFrameCall), false)
	a.BCond(NE, g.exit)
	a.Load(r8, rS, offFThisRef)
	for _, o := range []int32{offFArgs, offFWith, offFEvalVars, offFOpenUp + 8} {
		a.Load(r9, rS, o)
		a.Op(Orr, r8, r8, r9, true)
	}
	a.Cbnz(r8, g.exit, true)
	// e := &calls[C], its callee the window's.
	a.ShiftImm(Lsr, rC, rW, 48, true)
	a.Load(r9, rCtx, ncCC)
	g.cmpMem(rC, r9, offCCRegCalls+8)
	a.BCond(HS, g.exit)
	a.MovImm(r8, uint64(sizeE))
	a.Mul(rC, rC, r8)
	g.addMem(rC, r9, offCCRegCalls)
	g.regA(rS)
	a.Load(r9, rS, 0)
	g.cmpMem(r9, rCtx, ncObjBits)
	a.BCond(NE, g.exit)
	a.Load(r9, rS, 8)
	g.cmpMem(r9, rC, offECallee)
	a.BCond(NE, g.exit)
	// The this: the receiver as it is, or the cache's.
	a.Load(r8, rS, 16)
	a.Load(r9, rS, 24)
	a.LoadU8(rD, rC, offEKind)
	a.CmpImm(rD, int64(regCallEnter), false)
	a.BCond(EQ, thisOK)
	a.CmpImm(rD, int64(regCallThis), false)
	a.BCond(NE, g.exit)
	a.Load(r8, rC, offEThis)
	a.Load(r9, rC, offEThis+8)
	a.Bind(thisOK)
	// Its code, taking the frame in place, and the stack's room for it.
	a.Load(rD, rC, offERC)
	a.LoadU8(rT, rD, offRCTailTo)
	a.Cbz(rT, g.exit, false)
	a.Store(rCtx, scr(sE), rC)
	a.Store(rCtx, scr(sWin), rS)
	a.Store(rCtx, scr(sRC), rD)
	a.Store(rCtx, scr(sV0), r8)
	a.Store(rCtx, scr(sV1), r9)
	a.Load(rS, rCtx, ncNF)
	a.Load(rC, rS, offFBase)
	a.LoadS32(r9, rD, offRCNumRegs)
	a.Op(Add, r9, r9, rC, true)
	a.Load(rC, rCtx, ncRT)
	g.cmpMem(r9, rC, offRtStack+8)
	a.BCond(HI, g.exit)
	a.Store(rCtx, scr(sTop), r9)
	g.cmpMemImm(rC, offRtInterrupt, 1)
	a.BCond(LE, g.exit)
	// The arguments the parameters take: min(argc, params).
	a.Ubfx(r8, rW, 8, 8)
	a.LoadS32(r9, rD, offRCParams)
	a.Cmp(r8, r9, true)
	a.BCond(LE, argc)
	a.MovRR(r8, r9)
	a.Bind(argc)
	a.Store(rCtx, scr(sArgc), r8)

	// Nothing is written before here. The arguments moved down to the
	// parameters, in order, as amd64's.
	a.MovImm(rC, 0)
	a.Bind(loop)
	g.cmpMem(rC, rCtx, scr(sArgc))
	a.BCond(GE, loopEnd)
	a.Store(rCtx, scr(sI), rC)
	a.ShiftImm(Lsl, rC, rC, 4, true)
	a.Load(rS, rCtx, scr(sWin))
	a.Op(Add, rS, rS, rC, true)
	a.Op(Add, rW, rRegs, rC, true)
	a.Load(r8, rS, 32)
	a.Store(rW, 32, r8)
	a.Load(rC, rS, 40)
	a.AddImm(rW, rW, 40, true)
	g.ptrTo()
	a.Load(rC, rCtx, scr(sI))
	a.AddImm(rC, rC, 1, true)
	a.B(loop)
	a.Bind(loopEnd)
	// Register 0 the callee, 1 the this.
	a.Load(r8, rCtx, ncObjBits)
	a.Store(rRegs, 0, r8)
	a.Load(rC, rCtx, scr(sE))
	a.Load(rC, rC, offECallee)
	a.AddImm(rW, rRegs, 8, true)
	g.ptrTo()
	a.Load(r8, rCtx, scr(sV0))
	a.Store(rRegs, 16, r8)
	a.Load(rC, rCtx, scr(sV1))
	a.AddImm(rW, rRegs, 24, true)
	g.ptrTo()
	// Parameters no argument was given for, and the bindings past them:
	// undefined, from min(argc, params) to params+inits.
	a.Load(rC, rCtx, scr(sArgc))
	a.Load(r8, rCtx, scr(sRC))
	a.LoadS32(r9, r8, offRCParams)
	a.LoadS32(rS, r8, offRCInits)
	a.Op(Add, r9, r9, rS, true)
	a.Bind(undef)
	a.Cmp(rC, r9, true)
	a.BCond(GE, undefEnd)
	a.AddLsl(rW, rRegs, rC, 4)
	a.Load(r8, rCtx, ncUndef)
	a.Store(rW, 32, r8)
	a.Load(rT, rW, 40)
	a.Cbz(rT, skip, true)
	a.Store(rCtx, scr(sI), rC)
	a.Store(rCtx, scr(sEnd), r9)
	a.AddImm(rW, rW, 40, true)
	a.MovImm(rC, 0)
	g.ptrStore()
	a.Load(rC, rCtx, scr(sI))
	a.Load(r9, rCtx, scr(sEnd))
	a.Bind(skip)
	a.AddImm(rC, rC, 1, true)
	a.B(undef)
	a.Bind(undefEnd)
	// The stack's top, the interrupt budget.
	a.Load(r9, rCtx, ncRT)
	g.incMem(r9, offRtInterrupt, -1)
	a.Load(rD, rCtx, scr(sTop))
	g.cmpMem(rD, r9, offRtStackTop)
	a.BCond(LE, noTop)
	a.Store(r9, offRtStackTop, rD)
	g.cmpMem(rD, r9, offRtStackHigh)
	a.BCond(LE, noTop)
	a.Store(r9, offRtStackHigh, rD)
	a.Bind(noTop)
	// The frame record the callee's: pc 0, its this, closure and callee.
	a.Load(rS, rCtx, ncNF)
	a.Store32(rS, offFPC, ZR)
	a.Load(r8, rCtx, scr(sV0))
	a.Store(rS, offFThis, r8)
	a.Load(rC, rCtx, scr(sV1))
	a.AddImm(rW, rS, int64(offFThis+8), true)
	g.ptrTo()
	for _, f := range [][2]int32{{offECl, offFCl}, {offECallee, offFCallee}} {
		a.Load(rC, rCtx, scr(sE))
		a.Load(rC, rC, f[0])
		a.Load(rS, rCtx, ncNF)
		a.AddImm(rW, rS, int64(f[1]), true)
		g.ptrTo()
	}
	// new.target undefined, where the callee reads it, as call_w's.
	nt := a.NewLabel()
	a.Load(r8, rCtx, scr(sRC))
	a.LoadU8(rC, r8, offRCNewTarget)
	a.Cbz(rC, nt, false)
	a.Load(rS, rCtx, ncNF)
	a.Load(r8, rCtx, ncUndef)
	a.Store(rS, offFNewTgt, r8)
	a.MovImm(rC, 0)
	a.AddImm(rW, rS, int64(offFNewTgt+8), true)
	g.ptrTo()
	a.Bind(nt)
	// The callee's code, caches and constants the top frame's; Go makes
	// the frame's state again at its next exit (topOK).
	a.Load(rC, rCtx, scr(sE))
	a.Load(r8, rC, offECl)
	a.Load(r8, r8, offClCode)
	g.addMem(r8, rCtx, ncCCBase)
	a.Store(rCtx, ncCC, r8)
	a.Load(rIC, r8, offCCIC)
	a.Store(rCtx, ncIC, rIC)
	a.Load(r8, rCtx, scr(sRC))
	a.Store(rCtx, ncRC, r8)
	a.Load(rIP, r8, offRCCode)
	a.Store(rCtx, ncCodeBase, rIP)
	g.storeImm(rCtx, ncFV, 0)
	g.storeImm(rCtx, ncTopOK, 0)
	g.dispatch()
}
