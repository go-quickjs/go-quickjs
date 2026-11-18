package main

import (
	"math"

	. "github.com/go-quickjs/go-quickjs/internal/jit/asm/arm64"
	rd "github.com/go-quickjs/go-quickjs/wasm/internal/engine/internal/regdef"
)

// storeRef stores the pointer in R5 at the address in R4, plainly while the
// collector does not mark and through the barrier routine while it does
// (the context's wb and storeRef), which goes on at R16: the same work
// either way but the barrier, and no leaving native code for the marking's
// sake. R6 and R16 are its scratch; the routine keeps every other
// register.
func (g *armGen) storeRef() {
	a := g.a
	slow, done := a.NewLabel(), a.NewLabel()
	a.Load(aT, aCtx, rd.CtxWB)
	a.LoadU8(aT, aT, 0)
	a.Cbnz(aT, slow, false)
	a.Store(a8, 0, a9)
	a.B(done)
	a.Bind(slow)
	a.Adr(aJ, done)
	a.Load(aT, aCtx, rd.CtxStoreRef)
	a.Br(aT)
	a.Bind(done)
}

// refs lays out the moves of references, a Ref being its pointer word
// and its payload: the pointer stored by storeRef.
func (g *armGen) refs() {
	a := g.a
	// refSlot puts in dst the address of the reference slot field i names.
	refSlot := func(dst Reg, i int) {
		g.field(dst, i)
		a.Load(aT, aCtx, rd.CtxR)
		a.AddLsl(dst, aT, dst, 4)
	}
	g.h(rd.RefMov, "RefMov", func() {
		refSlot(aS, 1)
		refSlot(a8, 0)
		a.Load(aD, aS, 8)
		a.Store(a8, 8, aD)
		a.Load(a9, aS, 0)
		g.storeRef()
		g.next(1)
	})
	g.h(rd.RefNull, "RefNull", func() {
		refSlot(a8, 0)
		a.Store(a8, 8, ZR)
		a.MovRR(a9, ZR)
		g.storeRef()
		g.next(1)
	})
}

// calls lays out the calls and the returns as amd64's (amd64_call.go).
func (g *armGen) calls() {
	a := g.a
	callFn := a.NewLabel()
	const cc = rd.FrameNctx // the callee's context, at R7
	// siteAddr puts site BC's address in R1.
	siteAddr := func() {
		g.bc(aC, false)
		a.MovImm(aT, rd.SiteSize)
		a.Mul(aC, aC, aT)
		a.Load(aT, aCtx, rd.CtxSites)
		a.Op(Add, aC, aC, aT, true)
	}
	g.hx(rd.XCallIndirect, "CallIndirect", func() {
		a.Load(aT, aCtx, rd.CtxCalls)
		a.Cbz(aT, g.exit, true)
		siteAddr()
		a.LoadU32(aD, aC, rd.SiteTable)
		a.Load(a8, aCtx, rd.CtxInst)
		a.Load(a8, a8, rd.InstTables)
		a.LoadIdx(a8, a8, aD, true)
		a.LoadU32(a9, aC, rd.SiteTarget)
		a.LoadIdx(aS, aL, a9, true)
		a.Load(aT, a8, rd.TableElems+rd.SliceLen)
		a.Cmp(aS, aT, true)
		a.BCond(HS, g.exit)
		a.Load(a8, a8, rd.TableElems)
		a.AddLsl(a8, a8, aS, 4)
		a.Load(a8, a8, 0)
		a.Cbz(a8, g.exit, true)
		unchecked := a.NewLabel()
		a.LoadU8(aD, aC, rd.SiteChecked)
		a.Cbz(aD, unchecked, false)
		a.LoadU32(aD, a8, rd.FnType)
		a.LoadU32(aS, aC, rd.SiteWant)
		a.Cmp(aD, aS, false)
		a.BCond(NE, g.exit)
		a.Bind(unchecked)
		a.B(callFn)
	})
	g.h(rd.Call, "Call", func() {
		a.Load(aT, aCtx, rd.CtxCalls)
		a.Cbz(aT, g.exit, true)
		// R1 the site, R4 the function, R5 its record, R2 its code.
		siteAddr()
		a.LoadU32(aD, aC, rd.SiteFn)
		a.Load(a8, aCtx, rd.CtxFuncs)
		a.LoadIdx(a8, a8, aD, true)
		// The function R4 of site R1, call_indirect's too.
		a.Bind(callFn)
		a.Load(aD, a8, rd.FnInst)
		a.Load(aT, aCtx, rd.CtxInst)
		a.Cmp(aD, aT, true)
		a.BCond(NE, g.exit)
		a.LoadU32(aD, a8, rd.FnIndex)
		a.Load(a9, aCtx, rd.CtxFns)
		a.LoadIdx(a9, a9, aD, true)
		a.Cbz(a9, g.exit, true)
		a.Load(aD, a9, rd.CdReg)
		a.Cbz(aD, g.exit, true)
		a.LoadU8(a10, aD, rd.RcCallee)
		a.Cbz(a10, g.exit, false)
		// R3 the exec; the depth past MaxCallDepth or past the frames
		// made is Go's.
		a.Load(aS, aCtx, rd.CtxX)
		a.Load(a10, aS, rd.XDepth)
		a.AddImm(a10, a10, 1, true)
		a.Load(aT, aCtx, rd.CtxMaxDepth)
		a.Cmp(a10, aT, true)
		a.BCond(GT, g.exit)
		a.Load(aT, aS, rd.XFrames+rd.SliceLen)
		a.Cmp(a10, aT, true)
		a.BCond(GE, g.exit)
		// R7 the callee's frame, whose context keeps the call's scratch:
		// no frame past the running ones is in use.
		a.Load(aT, aS, rd.XFrames)
		a.LoadIdx(a10, aT, a10, true)
		a.Store(a10, cc+rd.CtxSite, aC)
		a.Store(a10, cc+rd.CtxFn, a8)
		a.Store(a10, cc+rd.CtxCd, a9)
		a.Store(a10, cc+rd.CtxRc, aD)
		// The poll due, the calls full, the chunk out of room: Go's.
		a.LoadS32(aW, aS, rd.XPoll)
		a.CmpImm(aW, 1, false)
		a.BCond(LE, g.exit)
		a.Load(aW, aS, rd.XCalls+rd.SliceLen)
		a.Load(aT, aS, rd.XCalls+rd.SliceCap)
		a.Cmp(aW, aT, true)
		a.BCond(HS, g.exit)
		a.Load(aW, aS, rd.XNc)
		a.Load(aT, aS, rd.XNchunks+rd.SliceLen)
		a.Cmp(aW, aT, true)
		a.BCond(HS, g.exit)
		g.sliceAt(aW, aS, rd.XNchunks)
		a.Load(aC, aS, rd.XNsp)
		a.Load(aT, a9, rd.CdNslots)
		a.Op(Add, aC, aC, aT, true)
		a.Load(aT, aW, rd.SliceLen)
		a.Cmp(aC, aT, true)
		a.BCond(HI, g.exit)
		// The callee's reference slots past its chunk's room: Go's. A
		// body that holds no reference takes none.
		refsFit := a.NewLabel()
		a.LoadU8(aW, a9, rd.CdRefs)
		a.Cbz(aW, refsFit, false)
		a.Load(aW, aS, rd.XRc)
		a.Load(aT, aS, rd.XRchunks+rd.SliceLen)
		a.Cmp(aW, aT, true)
		a.BCond(HS, g.exit)
		g.sliceAt(aC, aS, rd.XRchunks)
		a.Load(aW, a9, rd.CdRslots)
		a.Load(aT, aS, rd.XRsp)
		a.Op(Add, aW, aW, aT, true)
		a.Load(aT, aC, rd.SliceLen)
		a.Cmp(aW, aT, true)
		a.BCond(HI, g.exit)
		a.Bind(refsFit)

		// The call is native's from here. The caller's tick, and its
		// call's offset in its record on the exec's calls.
		a.Load(aC, aCtx, rd.CtxF)
		a.Store32(aC, rd.FrameTick, aTk)
		a.Load(aC, aS, rd.XCalls)
		a.Load(aD, aS, rd.XCalls+rd.SliceLen)
		a.AddLsl(aC, aC, aD, 4)
		a.Load(aD, a10, cc+rd.CtxSite)
		a.LoadU32(aD, aD, rd.SiteAt)
		a.Store32(aC, -rd.CallSiteSize+rd.CallSiteOffset, aD)
		// The callee's record: its function, at offset 0.
		a.Store32(aC, rd.CallSiteOffset, ZR)
		a.MovRR(a8, aC)
		a.Load(a9, a10, cc+rd.CtxFn)
		g.storeRef()
		a.Load(aC, aS, rd.XCalls+rd.SliceLen)
		a.AddImm(aC, aC, 1, true)
		a.Store(aS, rd.XCalls+rd.SliceLen, aC)
		a.Load(aC, aS, rd.XDepth)
		a.AddImm(aC, aC, 1, true)
		a.Store(aS, rd.XDepth, aC)
		a.LoadS32(aC, aS, rd.XPoll)
		a.AddImm(aC, aC, -1, false)
		a.Store32(aS, rd.XPoll, aC)
		// The callee's slots: R1 their first, R2 their count, the chunk
		// and slot to give back kept.
		a.Load(aD, aS, rd.XNc)
		a.Store(a10, cc+rd.CtxNc, aD)
		g.sliceAt(aD, aS, rd.XNchunks)
		a.Load(aC, aD, 0)
		a.Load(aD, aS, rd.XNsp)
		a.Store(a10, cc+rd.CtxNsp, aD)
		a.AddLsl(aC, aC, aD, 3)
		a.Store(a10, cc+rd.CtxL, aC)
		a.Load(aD, a10, cc+rd.CtxCd)
		a.Load(aD, aD, rd.CdNslots)
		a.Load(a8, aS, rd.XNsp)
		a.Op(Add, a8, a8, aD, true)
		a.Store(aS, rd.XNsp, a8)
		clear, cleared := a.NewLabel(), a.NewLabel()
		a.Bind(clear)
		a.Cbz(aD, cleared, true)
		a.AddImm(aD, aD, -1, true)
		a.AddLsl(aT, aC, aD, 3)
		a.Store(aT, 0, ZR)
		a.B(clear)
		a.Bind(cleared)
		// The callee's frame: its slots, instance and code, no OSR (the
		// Code runs no JIT), its tick the poll's.
		a.Load(aD, a10, cc+rd.CtxCd)
		a.Load(aD, aD, rd.CdNslots)
		a.Store(a10, rd.FrameL+rd.SliceLen, aD)
		a.Store(a10, rd.FrameL+rd.SliceCap, aD)
		a.AddImm(a8, a10, rd.FrameL, true)
		a.MovRR(a9, aC)
		g.storeRef()
		for _, f := range []struct{ field, from int32 }{{rd.FrameInst, rd.CtxInst}, {rd.FrameRc, -1}} {
			same := a.NewLabel()
			if f.from < 0 {
				a.Load(a9, a10, cc+rd.CtxRc)
			} else {
				a.Load(a9, aCtx, f.from)
			}
			a.Load(aT, a10, f.field)
			a.Cmp(a9, aT, true)
			a.BCond(EQ, same)
			a.AddImm(a8, a10, int64(f.field), true)
			g.storeRef()
			a.Bind(same)
		}
		a.MovImm(aT, math.MaxInt32)
		a.Store32(a10, rd.FrameOsr, aT)
		a.Store32(a10, rd.FrameSig, ZR)
		a.LoadS32(aD, aS, rd.XPoll)
		a.Store32(a10, rd.FrameTick, aD)
		a.Store32(a10, rd.FrameTickSet, aD)
		// The callee's context: the instance's and the run's from the
		// caller's, its code's, and the way back.
		for _, f := range []int32{rd.CtxTable, rd.CtxExtTable, rd.CtxSIMDTable, rd.CtxGoTable, rd.CtxSlowTable, rd.CtxCallH, rd.CtxMem0,
			rd.CtxGlobals, rd.CtxExits, rd.CtxX, rd.CtxInst, rd.CtxFuncs, rd.CtxFns, rd.CtxCalls,
			rd.CtxMaxDepth, rd.CtxStoreRef, rd.CtxWB} {
			a.Load(aD, aCtx, f)
			a.Store(a10, cc+f, aD)
		}
		g.takeRefs()
		a.Store(a10, cc+rd.CtxF, a10)
		a.Load(a8, a10, cc+rd.CtxRc)
		a.Load(aD, a8, rd.RcCode)
		a.Store(a10, cc+rd.CtxCode, aD)
		for _, f := range []struct{ slice, ctx int32 }{{rd.RcTables, rd.CtxTables}, {rd.RcSites, rd.CtxSites}} {
			has := a.NewLabel()
			a.MovRR(aD, ZR)
			a.Load(aT, a8, f.slice+rd.SliceLen)
			a.Cbz(aT, has, true)
			a.Load(aD, a8, f.slice)
			a.Bind(has)
			a.Store(a10, cc+f.ctx, aD)
		}
		a.Store(a10, cc+rd.CtxCaller, aCtx)
		a.Store(a10, cc+rd.CtxRetAddr, aIP)
		// The arguments, from the caller's slots (R20 and its context's
		// r) to the callee's (R1 and its context's r).
		a.Load(aS, a10, cc+rd.CtxSite)
		a.Load(aD, aS, rd.SiteArgs+rd.SliceLen)
		a.Load(aS, aS, rd.SiteArgs)
		g.siteMoves(aC, aCtx, rd.CtxR, a10, cc+rd.CtxR)
		// Into the callee: its context, slots, code and tick.
		a.AddImm(aCtx, a10, cc, true)
		a.MovRR(aL, aC)
		a.Load(aIP, aCtx, rd.CtxCode)
		a.LoadS32(aTk, a10, rd.FrameTick)
		g.dispatch()
	})

	// retNative returns from a frame: to Go where Go entered it, else to
	// the frame that called it natively, the results moved to its slots
	// by the call's site, the exec's calls, depth and chunks as they were.
	a.Bind(g.retNative)
	toGo := a.NewLabel()
	a.Load(aC, aCtx, rd.CtxCaller)
	a.Cbz(aC, toGo, true)
	a.Load(aS, aCtx, rd.CtxSite)
	a.Load(aD, aS, rd.SiteRets+rd.SliceLen)
	a.Load(aS, aS, rd.SiteRets)
	a.Load(a10, aC, rd.CtxL)
	g.siteMoves(a10, aCtx, rd.CtxR, aC, rd.CtxR)
	a.Load(aS, aCtx, rd.CtxX)
	a.Load(aD, aS, rd.XCalls+rd.SliceLen)
	a.AddImm(aD, aD, -1, true)
	a.Store(aS, rd.XCalls+rd.SliceLen, aD)
	a.Load(aD, aS, rd.XDepth)
	a.AddImm(aD, aD, -1, true)
	a.Store(aS, rd.XDepth, aD)
	for _, f := range []struct{ ctx, x int32 }{{rd.CtxNc, rd.XNc}, {rd.CtxNsp, rd.XNsp}, {rd.CtxRch, rd.XRc}, {rd.CtxRsp, rd.XRsp}} {
		a.Load(aD, aCtx, f.ctx)
		a.Store(aS, f.x, aD)
	}
	a.Load(aIP, aCtx, rd.CtxRetAddr)
	a.AddImm(aIP, aIP, 8, true)
	a.MovRR(aCtx, aC)
	a.MovRR(aL, a10)
	a.Load(aD, aCtx, rd.CtxF)
	a.LoadS32(aTk, aD, rd.FrameTick)
	g.dispatch()
	a.Bind(toGo)
	a.Ret()
}

// sliceAt puts in dst the address of the exec x's current chunk of the
// kind chunks names: &x.nchunks[x.nc] or &x.rchunks[x.rc]. R6 is its
// scratch.
func (g *armGen) sliceAt(dst, x Reg, chunks int32) {
	a := g.a
	num := int32(rd.XNc)
	if chunks == rd.XRchunks {
		num = rd.XRc
	}
	a.Load(dst, x, num)
	a.MovImm(aT, rd.SliceSize)
	a.Mul(dst, dst, aT)
	a.Load(aT, x, chunks)
	a.Op(Add, dst, dst, aT, true)
}

// takeRefs is amd64's: the callee's reference slots from the exec's chunk,
// cleared, where its body holds a reference. R1, R3, R7 and R25 are kept.
func (g *armGen) takeRefs() {
	a := g.a
	const cc = rd.FrameNctx
	none, clear, cleared, same := a.NewLabel(), a.NewLabel(), a.NewLabel(), a.NewLabel()
	a.Load(aD, aS, rd.XRc)
	a.Store(a10, cc+rd.CtxRch, aD)
	a.Load(aD, aS, rd.XRsp)
	a.Store(a10, cc+rd.CtxRsp, aD)
	a.Store(a10, cc+rd.CtxR, ZR)
	a.Load(aD, a10, cc+rd.CtxCd)
	a.LoadU8(a8, aD, rd.CdRefs)
	a.Cbz(a8, none, false)
	a.Load(aK, aD, rd.CdRslots)
	// R4 the chunk's first slot, R5 the slot taken first.
	g.sliceAt(a8, aS, rd.XRchunks)
	a.Load(a8, a8, 0)
	a.Load(a9, aS, rd.XRsp)
	a.Op(Add, aW, a9, aK, true)
	a.Store(aS, rd.XRsp, aW)
	a.AddLsl(a8, a8, a9, 4)
	a.Store(a10, cc+rd.CtxR, a8)
	// Cleared from the last: each payload plainly, each pointer by
	// storeRef.
	a.MovRR(aD, aK)
	a.Bind(clear)
	a.Cbz(aD, cleared, true)
	a.AddImm(aD, aD, -1, true)
	a.Load(a8, a10, cc+rd.CtxR)
	a.AddLsl(a8, a8, aD, 4)
	a.Store(a8, 8, ZR)
	a.MovRR(a9, ZR)
	g.storeRef()
	a.B(clear)
	a.Bind(cleared)
	// The frame's r: the slots.
	a.Store(a10, rd.FrameR+rd.SliceLen, aK)
	a.Store(a10, rd.FrameR+rd.SliceCap, aK)
	a.Load(a9, a10, cc+rd.CtxR)
	a.Load(aT, a10, rd.FrameR)
	a.Cmp(a9, aT, true)
	a.BCond(EQ, same)
	a.AddImm(a8, a10, rd.FrameR, true)
	g.storeRef()
	a.Bind(same)
	a.Bind(none)
}

// siteMoves makes the R2 moves at R3 from the slots at R20 and the
// reference slots at [srcR+srcDisp] to the slots at dst and the reference
// slots at [dstR+dstDisp], as amd64's. R0, R2 to R6 and R16 are its
// scratch.
func (g *armGen) siteMoves(dst, srcR Reg, srcDisp int32, dstR Reg, dstDisp int32) {
	a := g.a
	loop, done, two, ref, next := a.NewLabel(), a.NewLabel(), a.NewLabel(), a.NewLabel(), a.NewLabel()
	a.Bind(loop)
	a.Cbz(aD, done, true)
	a.LoadU32(a8, aS, rd.MoveFrom)
	a.LoadU32(a9, aS, rd.MoveTo)
	a.LoadU8(aW, aS, rd.MoveK)
	a.CmpImm(aW, rd.MoveRef, false)
	a.BCond(EQ, ref)
	a.LoadIdx(aT, aL, a8, true)
	a.AddLsl(a9, dst, a9, 3)
	a.Store(a9, 0, aT)
	a.CmpImm(aW, rd.MoveV128, false)
	a.BCond(EQ, two)
	a.Bind(next)
	a.AddImm(aS, aS, rd.MoveSize, true)
	a.AddImm(aD, aD, -1, true)
	a.B(loop)
	a.Bind(two)
	a.AddLsl(a8, aL, a8, 3)
	a.Load(aT, a8, 8)
	a.Store(a9, 8, aT)
	a.B(next)
	a.Bind(ref)
	a.Load(aT, dstR, dstDisp)
	a.AddLsl(a9, aT, a9, 4)
	a.Load(aT, srcR, srcDisp)
	a.AddLsl(a8, aT, a8, 4)
	a.Load(aT, a8, 8)
	a.Store(a9, 8, aT)
	a.Load(aT, a8, 0)
	a.MovRR(a8, a9)
	a.MovRR(a9, aT)
	g.storeRef()
	a.B(next)
	a.Bind(done)
}
