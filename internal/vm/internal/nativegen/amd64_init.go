package nativegen

import . "github.com/go-quickjs/go-quickjs/internal/jit/asm/amd64"

// The native initializer. A construction whose constructor's code has an
// initializer's plan (package vm's regCode initPlan: the register
// compiler's RegCode.Stores) -- a body that only stores its parameters in
// this, some made 0 where falsy -- has its stores made here, without the
// constructor's frame; so has one through a forwarder (fwdKind), whose
// method, this.k, has a plan and whose apply or call is the realm's, and an
// apply_args whose target has one. nativeNew and nativeApply say where the
// handler may try (3): the JIT not running, no interrupt due, the frames the
// bodies would have had within the limit. Every store is checked first, as
// set_prop's handler checks it (an own writable data property of the
// shape the stores before it leave, or an addition addProp would make), and
// a parameter made 0 is decided; anything else, and the body runs, in its
// frame, with nothing written. The stores are then made in order, each
// at its slot -- an own property's, or the one its addition's layout gives
// it, in the room the check found -- their pointers through ptrStore; and
// the layout the last leaves and its count of values are given the object
// once, as addProp gives each addition's, with no layout between them
// that any code could have seen.

// initEntry is the label of initRun, emitted once after the handlers.
func (g *nativeGen) initEntry() Label {
	if !g.initMade {
		g.initMade = true
		g.initL = g.a.NewLabel()
		g.after = append(g.after, g.initRun)
	}
	return g.initL
}

// initRun makes the stores of the plan of the code of the closureCode at
// sV1 (relative to cc, as ncCCBase makes it) in the object at sKa, of the
// arguments at sI, sEnd of them, and goes on at the address in sKind, RAX
// 1 where it made them and 0 where it made none. It keeps in the scratch
// the caches (sBase), the plan and its length (sNF, sSaved), the shape and
// count of values the stores so far leave (sV1, sTop) -- then the last
// one's shape and the store made -- and the stores whose parameter is made
// 0 (sRC).
func (g *nativeGen) initRun() {
	a := g.a
	shapeOff := off("Object.shape")
	loop1, existing, next1, phase2, loop2, zero, have, own, slot, ok, made, miss := a.NewLabel(), a.NewLabel(), a.NewLabel(),
		a.NewLabel(), a.NewLabel(), a.NewLabel(), a.NewLabel(), a.NewLabel(), a.NewLabel(), a.NewLabel(), a.NewLabel(), a.NewLabel()
	a.Bind(g.initL)
	a.Load(R9, RDI, scr(sV1))
	a.Load(RAX, R9, offCCIC)
	a.Store(RDI, scr(sBase), RAX)
	a.Load(R9, R9, offCCReg)
	a.Load(RDX, R9, offRCInitPlan)
	a.Store(RDI, scr(sNF), RDX)
	a.Load(RCX, R9, offRCInitPlan+8)
	a.Store(RDI, scr(sSaved), RCX)
	a.Load(RSI, RDI, scr(sKa))
	a.Load(R8, RSI, shapeOff)
	a.Op(Test, R8, R8, true)
	a.Jcc(CondE, miss)
	a.Store(RDI, scr(sV1), R8)
	a.LoadU32(R9, RSI, off("Object.valn"))
	a.Store(RDI, scr(sTop), R9)
	a.StoreImm(RDI, scr(sRC), 0)
	a.Op(Xor, RAX, RAX, false)

	// Each store checked: RSI the object, RDX the plan, RAX the store's
	// index.
	a.Bind(loop1)
	a.CmpMem(RAX, RDI, scr(sSaved), true)
	a.Jcc(CondAE, phase2)
	g.initSite()
	a.Load(R8, RDI, scr(sV1))
	a.CmpMem(R8, RCX, off("propCache.ownShape"), true)
	a.Jcc(CondE, existing)
	g.initAdds(miss)
	a.Bind(existing)
	// A parameter made 0 where falsy: decided now.
	a.LeaIdx(RCX, RDX, RAX, 4, 0)
	a.LoadU32(R9, RCX, 0)
	a.TestMem8(RCX, 0, 1)
	a.Jcc(CondE, next1)
	g.initArg()
	g.initFalsy(next1, miss)
	a.MovRR(RCX, RAX)
	a.MovImm(R9, 1)
	a.ShiftCL(Shl, R9, true)
	a.Load(R8, RDI, scr(sRC))
	a.Op(Or, R8, R9, true)
	a.Store(RDI, scr(sRC), R8)
	a.Bind(next1)
	a.OpImm(Add, RAX, 1, true)
	a.Jmp(loop1)

	// Each store made, its index at sTop, at its slot: an own property's
	// idx, an addition's the last of its transition's layout -- the values
	// past valn in the room phase one found. sV1 keeps the layout the
	// stores leave, given the object once they are all made.
	a.Bind(phase2)
	a.StoreImm(RDI, scr(sTop), 0)
	a.Bind(loop2)
	a.Load(RAX, RDI, scr(sTop))
	a.CmpMem(RAX, RDI, scr(sSaved), true)
	a.Jcc(CondAE, ok)
	a.Load(RDX, RDI, scr(sNF))
	g.initSite()
	a.Load(R8, RCX, off("propCache.next"))
	a.Op(Test, R8, R8, true)
	a.Jcc(CondE, own)
	a.LoadU32(RSI, R8, off("shape.n"))
	a.OpImm(Sub, RSI, 1, true)
	a.Jmp(slot)
	a.Bind(own)
	a.LoadS32(RSI, RCX, off("propCache.idx"))
	a.Bind(slot)
	a.ShiftImm(Shl, RSI, 4, true)
	a.Load(R8, RDI, scr(sKa))
	a.AddMem(RSI, R8, off("Object.valp"))
	a.Load(R8, RDI, scr(sRC))
	a.Bt(R8, RAX)
	a.Jcc(CondB, zero)
	a.LeaIdx(RCX, RDX, RAX, 4, 0)
	a.LoadU32(R9, RCX, 0)
	g.initArg()
	a.Jmp(have)
	a.Bind(zero)
	a.Op(Xor, R9, R9, false)
	a.Op(Xor, R8, R8, false)
	a.Bind(have)
	a.Store(RSI, 0, R9)
	a.MovRR(RCX, R8)
	a.Lea(RAX, RSI, 8)
	g.ptrStore()
	a.IncMem(RDI, scr(sTop))
	a.Jmp(loop2)

	// The layout the stores leave, as addProp leaves its addition's: the
	// count of values past them, then the layout.
	a.Bind(ok)
	a.Load(RSI, RDI, scr(sKa))
	a.Load(RCX, RDI, scr(sV1))
	a.CmpMem(RCX, RSI, shapeOff, true)
	a.Jcc(CondE, made)
	a.LoadU32(RDX, RCX, off("shape.n"))
	a.Store32(RSI, off("Object.valn"), RDX)
	a.Lea(RAX, RSI, shapeOff)
	g.ptrStore()
	a.Bind(made)
	a.MovImm(RAX, 1)
	a.JmpMem(RDI, scr(sKind))
	a.Bind(miss)
	a.Op(Xor, RAX, RAX, false)
	a.JmpMem(RDI, scr(sKind))
}

// initSite puts in RCX the cache of the store at index RAX of the plan at
// RDX. R8 is its scratch.
func (g *nativeGen) initSite() {
	a := g.a
	a.LeaIdx(RCX, RDX, RAX, 4, 0)
	a.LoadU32(RCX, RCX, 0)
	a.ShiftImm(Shr, RCX, 16, false)
	a.MovImm(R8, uint64(size("propCache")))
	a.Imul(RCX, R8)
	a.AddMem(RCX, RDI, scr(sBase))
}

// initAdds checks the addition the cache at RCX makes to the object at
// RSI of the shape at sV1, as addProp checks it, its count of values at
// sTop; and makes the transition's shape and the count past it the
// stores' so far. Anything else goes to miss. R8 and R9 are its scratch.
func (g *nativeGen) initAdds(miss Label) {
	a := g.a
	p1ok, p2ok, idxok := a.NewLabel(), a.NewLabel(), a.NewLabel()
	shapeOff := off("Object.shape")
	a.CmpMem(R8, RCX, off("propCache.shape"), true)
	a.Jcc(CondNE, miss)
	a.Load(R8, RCX, off("propCache.next"))
	a.Op(Test, R8, R8, true)
	a.Jcc(CondE, miss)
	a.CmpMem(R8, RDI, ncSetterNx, true)
	a.Jcc(CondE, miss)
	a.LoadU16(R9, RSI, offObjFlags)
	a.OpImm(And, R9, int32(objExtensible), false)
	a.Jcc(CondE, miss)
	a.Load(R9, RCX, off("propCache.p1"))
	a.Op(Test, R9, R9, true)
	a.Jcc(CondE, p1ok)
	a.Load(R9, R9, shapeOff)
	a.CmpMem(R9, RCX, off("propCache.s1"), true)
	a.Jcc(CondE, p1ok)
	a.Load(R9, RCX, off("propCache.cell"))
	a.Op(Test, R9, R9, true)
	a.Jcc(CondE, miss)
	a.LoadU8(R9, R9, offCellValid)
	a.Op(Test, R9, R9, false)
	a.Jcc(CondE, miss)
	a.Bind(p1ok)
	a.Load(R9, RCX, off("propCache.p2"))
	a.Op(Test, R9, R9, true)
	a.Jcc(CondE, p2ok)
	a.Load(R9, R9, shapeOff)
	a.CmpMem(R9, RCX, off("propCache.s2"), true)
	a.Jcc(CondNE, miss)
	a.Bind(p2ok)
	a.Load(R9, RDI, scr(sTop))
	a.LoadU32(RCX, RSI, off("Object.valc"))
	a.Op(Cmp, R9, RCX, true)
	a.Jcc(CondAE, miss)
	a.CmpMemImm8(R8, off("shape.index"), 0)
	a.Jcc(CondNE, idxok)
	a.OpImm(Cmp, R9, linearScanLimit, true)
	a.Jcc(CondAE, miss)
	a.Bind(idxok)
	a.IncMem(RDI, scr(sTop))
	a.Store(RDI, scr(sV1), R8)
}

// initArg puts in R9 and R8 the argument the plan's word in R9 stores:
// its parameter's, of the sEnd at sI, or undefined past them. RCX is its
// scratch.
func (g *nativeGen) initArg() {
	a := g.a
	undef, done := a.NewLabel(), a.NewLabel()
	a.ShiftImm(Shr, R9, 1, false)
	a.OpImm(And, R9, 0x7FFF, false)
	a.CmpMem(R9, RDI, scr(sEnd), true)
	a.Jcc(CondAE, undef)
	a.ShiftImm(Shl, R9, 4, true)
	a.AddMem(R9, RDI, scr(sI))
	a.Load(R8, R9, 8)
	a.Load(R9, R9, 0)
	a.Jmp(done)
	a.Bind(undef)
	a.Load(R9, RDI, ncUndef)
	a.Op(Xor, R8, R8, false)
	a.Bind(done)
}

// initFalsy goes on past it where the value in R9 and R8 is falsy, to
// truthy where it is not, and to miss where it is a string, a symbol, a
// BigInt or document.all's stand-in, whose body decides. RCX is its
// scratch.
func (g *nativeGen) initFalsy(truthy, miss Label) {
	a := g.a
	notNum, falsy := a.NewLabel(), a.NewLabel()
	a.Op(Cmp, R9, R10, true)
	a.Jcc(CondAE, notNum)
	// 0, -0 and NaN: equal to 0, or unordered.
	a.MovQToX(0, R9)
	a.Op(Xor, RCX, RCX, false)
	a.Cvtsi2sd(1, RCX, true)
	a.SSEOp(UcomiSD, 0, 1)
	a.Jcc(CondE, falsy)
	a.Jmp(truthy)
	a.Bind(notNum)
	for _, w := range []int32{ncUndef, ncNull, ncFalse} {
		a.CmpMem(R9, RDI, w, true)
		a.Jcc(CondE, falsy)
	}
	a.CmpMem(R9, RDI, ncTrue, true)
	a.Jcc(CondE, truthy)
	a.CmpMem(R9, RDI, ncObjBits, true)
	a.Jcc(CondNE, miss)
	a.LoadU16(RCX, R8, offObjFlags)
	a.OpImm(And, RCX, int32(objHTMLDDA), false)
	a.Jcc(CondNE, miss)
	a.Jmp(truthy)
	a.Bind(falsy)
}

// newInit is new_w's way to the native initializer, at hook where
// nativeNew reported 2 or more: 2, done, a body leafCall answered; 3, the
// constructor's plan tried, or its forwarder's method's, and done where
// it made its stores, R[B] the object, or the constructor entered at
// resume where it made none.
func (g *nativeGen) newInit(hook, resume, done Label) {
	a := g.a
	fwd, back := a.NewLabel(), a.NewLabel()
	a.Bind(hook)
	a.Jcc(CondE, done)
	a.Load(RAX, RBX, 0)
	g.newSite()
	a.Load(R8, RCX, offERC)
	a.LoadU32(R9, R8, offRCFwdKind)
	a.Op(Test, R9, R9, false)
	a.Jcc(CondNE, fwd)
	// The constructor's own plan: the object R[A+1], the arguments the
	// window's.
	a.Load(R9, RSI, 24)
	a.Store(RDI, scr(sKa), R9)
	a.Load(R9, RCX, offECl)
	a.Load(R9, R9, offClCode)
	a.AddMem(R9, RDI, ncCCBase)
	a.Store(RDI, scr(sV1), R9)
	g.initWindowArgs()
	a.LeaLabel(R9, back)
	a.Store(RDI, scr(sKind), R9)
	a.Jmp(g.initEntry())
	a.Bind(back)
	a.Op(Test, RAX, RAX, true)
	a.Jcc(CondE, resume)
	a.Load(RAX, RBX, 0)
	g.decA(RDX)
	a.LoadIdx(R9, R12, RDX, 1, 16)
	a.LoadIdx(R8, R12, RDX, 1, 24)
	g.decB(RDX)
	a.LeaIdx(RAX, R12, RDX, 1, 0)
	g.storeValue()
	a.Bind(fwd)
	g.forwardInit(back, resume)
}

// initWindowArgs makes the arguments of the instruction in RAX, whose
// window is at RSI, the native initializer's: sI and sEnd.
func (g *nativeGen) initWindowArgs() {
	a := g.a
	a.Lea(R9, RSI, 32)
	a.Store(RDI, scr(sI), R9)
	a.MovRR(R9, RAX)
	a.ShiftImm(Shr, R9, 8, true)
	a.MovZX8(R9, R9)
	a.Store(RDI, scr(sEnd), R9)
}

// forwardInit is a construction through a forwarder, the entry at RCX,
// its window at RSI and its code at R8: this.k read as the forwarder's
// body reads it, at its site, or in its table (vm's fwdSite) where the
// site has given up, a plain data property holding a function of a
// closure, not bound, whose code has a plan; its apply's or call's read,
// the realm's built-in; then the plan made on the object R[A+1], given
// the forwarder's arguments as apply hands them on, or as call does,
// laid at the context's fwdArgs; back at back. Anything else enters the
// forwarder at resume.
func (g *nativeGen) forwardInit(back, resume Label) {
	a := g.a
	have, isCall, cmpFn, callArgs, argLoop, konst, undef, put, run := a.NewLabel(), a.NewLabel(), a.NewLabel(),
		a.NewLabel(), a.NewLabel(), a.NewLabel(), a.NewLabel(), a.NewLabel(), a.NewLabel()
	propCache := uint64(size("propCache"))
	saved := g.exit
	g.exit = resume
	a.Load(R9, RSI, 24)
	a.Store(RDI, scr(sKa), R9)
	a.LoadU32(R9, R8, offRCFwdKind)
	a.Store(RDI, scr(sRC), R9)
	a.LoadU32(R9, R8, offRCFwdSites)
	a.Store(RDI, scr(sTop), R9)
	a.Store(RDI, scr(sNF), R8)
	a.Load(R9, RCX, offECl)
	a.Load(R9, R9, offClCode)
	a.AddMem(R9, RDI, ncCCBase)
	a.Store(RDI, scr(sBase), R9)
	// The method's cache: the site's, or the table's entry of the
	// object's shape.
	a.Load(RCX, R9, offCCIC)
	a.Load(RDX, RDI, scr(sTop))
	a.OpImm(And, RDX, 0xFFFF, false)
	a.MovImm(R8, propCache)
	a.Imul(RDX, R8)
	a.Op(Add, RCX, RDX, true)
	a.LoadU8(RDX, RCX, off("propCache.fills"))
	a.OpImm(Cmp, RDX, maxCacheFills, false)
	a.Jcc(CondB, have)
	a.Load(RCX, R9, offCCFwd)
	a.Op(Test, RCX, RCX, true)
	a.Jcc(CondE, resume)
	a.Load(RSI, RDI, scr(sKa))
	a.Load(RDX, RSI, off("Object.shape"))
	a.ShiftImm(Shr, RDX, uint8(fwdShift), true)
	a.OpImm(And, RDX, fwdWays-1, false)
	a.Imul(RDX, R8)
	a.Op(Add, RCX, RDX, true)
	a.Bind(have)
	a.Load(RSI, RDI, scr(sKa))
	g.readSlot()
	a.Load(R9, RCX, 0)
	a.CmpMem(R9, RDI, ncObjBits, true)
	a.Jcc(CondNE, resume)
	a.Load(RSI, RCX, 8)
	// A function of a closure, not bound, whose code has a plan.
	a.LoadU16(RDX, RSI, offObjFlags)
	a.OpImm(And, RDX, int32(objHasData), false)
	a.Jcc(CondE, resume)
	a.Load(RDX, RSI, sizeObj+offExtData)
	a.CmpMem(RDX, RDI, ncFuncT, true)
	a.Jcc(CondNE, resume)
	a.Load(RDX, RSI, sizeObj+offExtData+8)
	a.LoadU8(R8, RDX, offFDBound)
	a.Op(Test, R8, R8, false)
	a.Jcc(CondNE, resume)
	a.Load(RDX, RDX, offFdClosure)
	a.Op(Test, RDX, RDX, true)
	a.Jcc(CondE, resume)
	a.Load(RDX, RDX, offClCode)
	a.AddMem(RDX, RDI, ncCCBase)
	a.Store(RDI, scr(sV1), RDX)
	a.Load(R8, RDX, offCCReg)
	a.Op(Test, R8, R8, true)
	a.Jcc(CondE, resume)
	a.CmpMemImm8(R8, offRCInitPlan+8, 0)
	a.Jcc(CondE, resume)
	// Its apply or call, the realm's.
	a.Load(R9, RDI, scr(sBase))
	a.Load(RCX, R9, offCCIC)
	a.Load(RDX, RDI, scr(sTop))
	a.ShiftImm(Shr, RDX, 16, false)
	a.MovImm(R8, propCache)
	a.Imul(RDX, R8)
	a.Op(Add, RCX, RDX, true)
	g.readSlot()
	a.Load(R9, RCX, 0)
	a.CmpMem(R9, RDI, ncObjBits, true)
	a.Jcc(CondNE, resume)
	a.Load(R8, RCX, 8)
	a.Load(RDX, RDI, ncRT)
	a.Load(RDX, RDX, rtRel(0))
	a.CmpMemImm8(RDI, scr(sRC), int8(fwdCall))
	a.Jcc(CondE, isCall)
	a.AddMem(RDX, RDI, ncApplyAt)
	a.Jmp(cmpFn)
	a.Bind(isCall)
	a.AddMem(RDX, RDI, ncCallFn)
	a.Bind(cmpFn)
	a.CmpMem(R8, RDX, 0, true)
	a.Jcc(CondNE, resume)
	g.exit = saved
	// The arguments: apply's, the window's; call's, laid at fwdArgs.
	a.Load(RAX, RBX, 0)
	g.decA(RDX)
	a.LeaIdx(RSI, R12, RDX, 1, 0)
	a.CmpMemImm8(RDI, scr(sRC), int8(fwdCall))
	a.Jcc(CondE, callArgs)
	g.initWindowArgs()
	a.Jmp(run)
	a.Bind(callArgs)
	// RDX the next argument's plan, RCX twice its index, R8 the
	// forwarder's argument count.
	a.Load(R8, RDI, scr(sNF))
	a.Load(RDX, R8, offRCFwdArgs)
	a.Load(RCX, R8, offRCFwdArgs+8)
	a.Store(RDI, scr(sEnd), RCX)
	a.Op(Add, RCX, RCX, true)
	a.Store(RDI, scr(sSaved), RCX)
	a.MovRR(R8, RAX)
	a.ShiftImm(Shr, R8, 8, true)
	a.MovZX8(R8, R8)
	a.Op(Xor, RCX, RCX, false)
	a.Bind(argLoop)
	a.CmpMem(RCX, RDI, scr(sSaved), true)
	a.Jcc(CondAE, run)
	a.Load(RAX, RDX, off("fwdArg.param"))
	a.Op(Test, RAX, RAX, true)
	a.Jcc(CondS, konst)
	a.Op(Cmp, RAX, R8, true)
	a.Jcc(CondAE, undef)
	a.ShiftImm(Shl, RAX, 4, true)
	a.Op(Add, RAX, RSI, true)
	a.Load(R9, RAX, 32)
	a.Load(RAX, RAX, 40)
	a.Jmp(put)
	a.Bind(konst)
	a.Load(R9, RDX, off("fwdArg.k"))
	a.Load(RAX, RDX, off("fwdArg.k")+8)
	a.Jmp(put)
	a.Bind(undef)
	a.Load(R9, RDI, ncUndef)
	a.Op(Xor, RAX, RAX, false)
	a.Bind(put)
	a.StoreIdx(RDI, RCX, 8, ncFwdArgs, R9)
	a.StoreIdx(RDI, RCX, 8, ncFwdArgs+8, RAX)
	a.OpImm(Add, RCX, 2, true)
	a.OpImm(Add, RDX, size("fwdArg"), true)
	a.Jmp(argLoop)
	a.Bind(run)
	a.CmpMemImm8(RDI, scr(sRC), int8(fwdCall))
	isApply := a.NewLabel()
	a.Jcc(CondNE, isApply)
	a.Lea(R9, RDI, ncFwdArgs)
	a.Store(RDI, scr(sI), R9)
	a.Bind(isApply)
	a.LeaLabel(R9, back)
	a.Store(RDI, scr(sKind), R9)
	a.Jmp(g.initEntry())
}

// applyInit is apply_args's way to the native initializer, at hook where
// nativeApply reported other than 1: 0, Go's; 3, its target's plan tried
// on the this of the window nativeApply made, and done where it made its
// stores, R[A] undefined, or the target entered at resume where it made
// none.
func (g *nativeGen) applyInit(hook, resume Label) {
	a := g.a
	back := a.NewLabel()
	a.Bind(hook)
	a.Op(Test, RAX, RAX, true)
	a.Jcc(CondE, g.exit)
	a.Load(RCX, RDI, scr(sE))
	a.Load(RSI, RDI, scr(sWin))
	a.Load(R9, RSI, 24)
	a.Store(RDI, scr(sKa), R9)
	a.Load(R9, RCX, offECl)
	a.Load(R9, R9, offClCode)
	a.AddMem(R9, RDI, ncCCBase)
	a.Store(RDI, scr(sV1), R9)
	a.Lea(R9, RSI, 32)
	a.Store(RDI, scr(sI), R9)
	a.Load(R9, RDI, scr(sArgc))
	a.Store(RDI, scr(sEnd), R9)
	a.LeaLabel(R9, back)
	a.Store(RDI, scr(sKind), R9)
	a.Jmp(g.initEntry())
	a.Bind(back)
	a.Op(Test, RAX, RAX, true)
	a.Jcc(CondE, resume)
	a.Load(RAX, RBX, 0)
	g.decA(RDX)
	a.LeaIdx(RAX, R12, RDX, 1, 0)
	a.Load(R9, RDI, ncUndef)
	a.Op(Xor, R8, R8, false)
	g.storeValue()
}
