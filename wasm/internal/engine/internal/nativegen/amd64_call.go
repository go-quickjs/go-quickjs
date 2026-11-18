package main

import (
	"math"

	. "github.com/go-quickjs/go-quickjs/internal/jit/asm/amd64"
	rd "github.com/go-quickjs/go-quickjs/wasm/internal/engine/internal/regdef"
)

// storeRef stores the pointer in R9 at the address in R8, plainly while the
// collector does not mark and through the barrier routine while it does
// (the context's wb and storeRef): the same work either way but the
// barrier, and no leaving native code for the marking's sake. RAX is its
// scratch; the routine keeps every other register.
func (g *gen) storeRef() {
	a := g.a
	slow, done := a.NewLabel(), a.NewLabel()
	a.Load(RAX, RDI, rd.CtxWB)
	a.TestMem8(RAX, 0, 0xff)
	a.Jcc(CondNE, slow)
	a.Store(R8, 0, R9)
	a.Jmp(done)
	a.Bind(slow)
	a.LeaLabel(RAX, done)
	a.JmpMem(RDI, rd.CtxStoreRef)
	a.Bind(done)
}

// refs lays out the moves of references, a Ref being its pointer word
// and its payload: the pointer stored by storeRef.
func (g *gen) refs() {
	a := g.a
	// refSlot puts in dst the address of the reference slot field i names.
	refSlot := func(dst Reg, i int) {
		g.off(dst, i)
		a.Op(Add, dst, dst, true) // 16 bytes a reference
		a.OpMem(Add, dst, RDI, rd.CtxR, true)
	}
	g.h(rd.RefMov, "RefMov", func() {
		refSlot(RSI, 1)
		refSlot(R8, 0)
		a.Load(RDX, RSI, 8)
		a.Store(R8, 8, RDX)
		a.Load(R9, RSI, 0)
		g.storeRef()
		g.next(1)
	})
	g.h(rd.RefNull, "RefNull", func() {
		refSlot(R8, 0)
		a.Op(Xor, R9, R9, false)
		a.Store(R8, 8, R9)
		g.storeRef()
		g.next(1)
	})
}

// calls lays out the call of a function and the returns. A call native
// code makes itself is of a function of the same instance with register
// code that may be called so (regCode.callee: no try region, no tail call,
// no reference in its frame), where the Code runs no JIT, the depth, the
// exec's calls, frames and chunk have room, and the poll is not due; any
// other leaves to Go's call. It does what Go's call does: the caller's
// tick and call offset kept, the callee's record on the exec's calls, the
// depth and the poll counted, its slots taken from the chunk and cleared,
// the arguments moved, its frame and context made; and the callee returns
// natively to it.
func (g *gen) calls() {
	a := g.a
	retNative := g.retNative
	callFn := a.NewLabel()
	// call_indirect: the function the table's entry names, where the
	// entry is in the table, not null, and of the site's type exactly
	// where the site checks it; then the call as call's. A null entry, an
	// index out of bounds, a type to check further (a subtype) leave to
	// Go, which traps or calls.
	g.hx(rd.XCallIndirect, "CallIndirect", func() {
		a.CmpMemImm8(RDI, rd.CtxCalls, 0)
		a.Jcc(CondE, g.exit)
		g.bc(RCX, false)
		a.ImulImm(RCX, RCX, rd.SiteSize, true)
		a.OpMem(Add, RCX, RDI, rd.CtxSites, true)
		a.LoadU32(RDX, RCX, rd.SiteTable)
		a.Load(R8, RDI, rd.CtxInst)
		a.Load(R8, R8, rd.InstTables)
		a.LoadIdx(R8, R8, RDX, 8, 0)
		a.LoadU32(R9, RCX, rd.SiteTarget)
		a.LoadIdx(RSI, R12, R9, 8, 0)
		a.CmpMem(RSI, R8, rd.TableElems+rd.SliceLen, true)
		a.Jcc(CondAE, g.exit)
		a.Load(R8, R8, rd.TableElems)
		a.ShiftImm(Shl, RSI, 4, true)
		a.LoadIdx(R8, R8, RSI, 1, 0)
		a.Op(Test, R8, R8, true)
		a.Jcc(CondE, g.exit)
		unchecked := a.NewLabel()
		a.LoadU8(RDX, RCX, rd.SiteChecked)
		a.Op(Test, RDX, RDX, false)
		a.Jcc(CondE, unchecked)
		a.LoadU32(RDX, R8, rd.FnType)
		a.LoadU32(RSI, RCX, rd.SiteWant)
		a.Op(Cmp, RDX, RSI, false)
		a.Jcc(CondNE, g.exit)
		a.Bind(unchecked)
		a.Jmp(callFn)
	})
	g.h(rd.Call, "Call", func() {
		const cc = rd.FrameNctx // the callee's context, at R10
		a.CmpMemImm8(RDI, rd.CtxCalls, 0)
		a.Jcc(CondE, g.exit)
		// RCX the site, R8 the function, R9 its record, RDX its code.
		g.bc(RCX, false)
		a.ImulImm(RCX, RCX, rd.SiteSize, true)
		a.OpMem(Add, RCX, RDI, rd.CtxSites, true)
		a.LoadU32(RDX, RCX, rd.SiteFn)
		a.Load(R8, RDI, rd.CtxFuncs)
		a.LoadIdx(R8, R8, RDX, 8, 0)
		// The function R8 of site RCX, call_indirect's too.
		a.Bind(callFn)
		a.Load(RDX, R8, rd.FnInst)
		a.CmpMem(RDX, RDI, rd.CtxInst, true)
		a.Jcc(CondNE, g.exit)
		a.LoadU32(RDX, R8, rd.FnIndex)
		a.Load(R9, RDI, rd.CtxFns)
		a.LoadIdx(R9, R9, RDX, 8, 0)
		a.Op(Test, R9, R9, true)
		a.Jcc(CondE, g.exit)
		a.Load(RDX, R9, rd.CdReg)
		a.Op(Test, RDX, RDX, true)
		a.Jcc(CondE, g.exit)
		a.LoadU8(R10, RDX, rd.RcCallee)
		a.Op(Test, R10, R10, false)
		a.Jcc(CondE, g.exit)
		// RSI the exec; the depth past MaxCallDepth or past the frames
		// made is Go's.
		a.Load(RSI, RDI, rd.CtxX)
		a.Load(R10, RSI, rd.XDepth)
		a.OpImm(Add, R10, 1, true)
		a.CmpMem(R10, RDI, rd.CtxMaxDepth, true)
		a.Jcc(CondG, g.exit)
		a.CmpMem(R10, RSI, rd.XFrames+rd.SliceLen, true)
		a.Jcc(CondAE, g.exit)
		// R10 the callee's frame, whose context keeps the call's
		// scratch: no frame past the running ones is in use.
		a.Load(RAX, RSI, rd.XFrames)
		a.LoadIdx(R10, RAX, R10, 8, 0)
		a.Store(R10, cc+rd.CtxSite, RCX)
		a.Store(R10, cc+rd.CtxFn, R8)
		a.Store(R10, cc+rd.CtxCd, R9)
		a.Store(R10, cc+rd.CtxRc, RDX)
		// The poll due, the calls full, the chunk out of room: Go's.
		a.LoadS32(RAX, RSI, rd.XPoll)
		a.OpImm(Cmp, RAX, 1, false)
		a.Jcc(CondLE, g.exit)
		a.Load(RAX, RSI, rd.XCalls+rd.SliceLen)
		a.CmpMem(RAX, RSI, rd.XCalls+rd.SliceCap, true)
		a.Jcc(CondAE, g.exit)
		a.Load(RAX, RSI, rd.XNc)
		a.CmpMem(RAX, RSI, rd.XNchunks+rd.SliceLen, true)
		a.Jcc(CondAE, g.exit)
		a.LeaIdx(RAX, RAX, RAX, 2, 0)
		a.ShiftImm(Shl, RAX, 3, true)
		a.OpMem(Add, RAX, RSI, rd.XNchunks, true)
		a.Load(RCX, RSI, rd.XNsp)
		a.OpMem(Add, RCX, R9, rd.CdNslots, true)
		a.CmpMem(RCX, RAX, rd.SliceLen, true)
		a.Jcc(CondA, g.exit)
		// The callee's reference slots past its chunk's room: Go's. A
		// body that holds no reference takes none.
		refsFit := a.NewLabel()
		a.LoadU8(RAX, R9, rd.CdRefs)
		a.Op(Test, RAX, RAX, false)
		a.Jcc(CondE, refsFit)
		a.Load(RAX, R9, rd.CdRslots)
		a.Load(RCX, RSI, rd.XRc)
		a.CmpMem(RCX, RSI, rd.XRchunks+rd.SliceLen, true)
		a.Jcc(CondAE, g.exit)
		a.LeaIdx(RCX, RCX, RCX, 2, 0)
		a.ShiftImm(Shl, RCX, 3, true)
		a.OpMem(Add, RCX, RSI, rd.XRchunks, true)
		a.OpMem(Add, RAX, RSI, rd.XRsp, true)
		a.CmpMem(RAX, RCX, rd.SliceLen, true)
		a.Jcc(CondA, g.exit)
		a.Bind(refsFit)

		// The call is native's from here. The caller's tick, and its
		// call's offset in its record on the exec's calls.
		a.Load(RCX, RDI, rd.CtxF)
		a.Store32(RCX, rd.FrameTick, R11)
		a.Load(RCX, RSI, rd.XCalls)
		a.Load(RDX, RSI, rd.XCalls+rd.SliceLen)
		a.ShiftImm(Shl, RDX, 4, true)
		a.Op(Add, RCX, RDX, true)
		a.Load(RDX, R10, cc+rd.CtxSite)
		a.LoadU32(RDX, RDX, rd.SiteAt)
		a.Store32(RCX, -rd.CallSiteSize+rd.CallSiteOffset, RDX)
		// The callee's record: its function, at offset 0.
		a.StoreImm32(RCX, rd.CallSiteOffset, 0)
		a.MovRR(R8, RCX)
		a.Load(R9, R10, cc+rd.CtxFn)
		g.storeRef()
		a.Load(RCX, RSI, rd.XCalls+rd.SliceLen)
		a.OpImm(Add, RCX, 1, true)
		a.Store(RSI, rd.XCalls+rd.SliceLen, RCX)
		a.Load(RCX, RSI, rd.XDepth)
		a.OpImm(Add, RCX, 1, true)
		a.Store(RSI, rd.XDepth, RCX)
		a.LoadS32(RCX, RSI, rd.XPoll)
		a.OpImm(Sub, RCX, 1, false)
		a.Store32(RSI, rd.XPoll, RCX)
		// The callee's slots: RCX their first, RDX their count, the chunk
		// and slot to give back kept.
		a.Load(RDX, RSI, rd.XNc)
		a.Store(R10, cc+rd.CtxNc, RDX)
		a.LeaIdx(RDX, RDX, RDX, 2, 0)
		a.ShiftImm(Shl, RDX, 3, true)
		a.OpMem(Add, RDX, RSI, rd.XNchunks, true)
		a.Load(RCX, RDX, 0)
		a.Load(RDX, RSI, rd.XNsp)
		a.Store(R10, cc+rd.CtxNsp, RDX)
		a.LeaIdx(RCX, RCX, RDX, 8, 0)
		a.Store(R10, cc+rd.CtxL, RCX)
		a.Load(RDX, R10, cc+rd.CtxCd)
		a.Load(RDX, RDX, rd.CdNslots)
		a.Load(R8, RSI, rd.XNsp)
		a.Op(Add, R8, RDX, true)
		a.Store(RSI, rd.XNsp, R8)
		clear, cleared := a.NewLabel(), a.NewLabel()
		a.Op(Xor, R9, R9, false)
		a.Bind(clear)
		a.Op(Test, RDX, RDX, true)
		a.Jcc(CondE, cleared)
		a.StoreIdx(RCX, RDX, 8, -8, R9)
		a.OpImm(Sub, RDX, 1, true)
		a.Jmp(clear)
		a.Bind(cleared)
		// The callee's frame: its slots, instance and code, no OSR (the
		// Code runs no JIT), its tick the poll's.
		a.Load(RDX, R10, cc+rd.CtxCd)
		a.Load(RDX, RDX, rd.CdNslots)
		a.Store(R10, rd.FrameL+rd.SliceLen, RDX)
		a.Store(R10, rd.FrameL+rd.SliceCap, RDX)
		a.Lea(R8, R10, rd.FrameL)
		a.MovRR(R9, RCX)
		g.storeRef()
		for _, f := range []struct{ field, from int32 }{{rd.FrameInst, rd.CtxInst}, {rd.FrameRc, -1}} {
			same := a.NewLabel()
			if f.from < 0 {
				a.Load(R9, R10, cc+rd.CtxRc)
			} else {
				a.Load(R9, RDI, f.from)
			}
			a.CmpMem(R9, R10, f.field, true)
			a.Jcc(CondE, same)
			a.Lea(R8, R10, f.field)
			g.storeRef()
			a.Bind(same)
		}
		a.StoreImm32(R10, rd.FrameOsr, math.MaxInt32)
		a.StoreImm32(R10, rd.FrameSig, 0)
		a.LoadS32(RDX, RSI, rd.XPoll)
		a.Store32(R10, rd.FrameTick, RDX)
		a.Store32(R10, rd.FrameTickSet, RDX)
		// The callee's context: the instance's and the run's from the
		// caller's, its code's, and the way back.
		for _, f := range []int32{rd.CtxTable, rd.CtxExtTable, rd.CtxSIMDTable, rd.CtxGoTable, rd.CtxSlowTable, rd.CtxCallH, rd.CtxMem0,
			rd.CtxGlobals, rd.CtxExits, rd.CtxX, rd.CtxInst, rd.CtxFuncs, rd.CtxFns, rd.CtxCalls,
			rd.CtxMaxDepth, rd.CtxStoreRef, rd.CtxWB} {
			a.Load(RDX, RDI, f)
			a.Store(R10, cc+f, RDX)
		}
		g.takeRefs()
		a.Store(R10, cc+rd.CtxF, R10)
		a.Load(R8, R10, cc+rd.CtxRc)
		a.Load(RDX, R8, rd.RcCode)
		a.Store(R10, cc+rd.CtxCode, RDX)
		for _, f := range []struct{ slice, ctx int32 }{{rd.RcTables, rd.CtxTables}, {rd.RcSites, rd.CtxSites}} {
			has := a.NewLabel()
			a.Op(Xor, RDX, RDX, false)
			a.CmpMemImm8(R8, f.slice+rd.SliceLen, 0)
			a.Jcc(CondE, has)
			a.Load(RDX, R8, f.slice)
			a.Bind(has)
			a.Store(R10, cc+f.ctx, RDX)
		}
		a.Store(R10, cc+rd.CtxCaller, RDI)
		a.Store(R10, cc+rd.CtxRetAddr, RBX)
		// The arguments, from the caller's slots (R12 and its context's
		// r) to the callee's (RCX and its context's r).
		a.Load(RSI, R10, cc+rd.CtxSite)
		a.Load(RDX, RSI, rd.SiteArgs+rd.SliceLen)
		a.Load(RSI, RSI, rd.SiteArgs)
		g.siteMoves(RCX, RDI, rd.CtxR, R10, cc+rd.CtxR)
		// Into the callee: its context, slots, code and tick.
		a.Lea(RDI, R10, cc)
		a.MovRR(R12, RCX)
		a.Load(RBX, RDI, rd.CtxCode)
		a.LoadS32(R11, R10, rd.FrameTick)
		g.dispatch()
	})

	// retNative returns from a frame: to Go where Go entered it, else to
	// the frame that called it natively, the results moved to its slots
	// by the call's site, the exec's calls, depth and chunk as they were.
	a.Bind(retNative)
	toGo := a.NewLabel()
	a.Load(RCX, RDI, rd.CtxCaller)
	a.Op(Test, RCX, RCX, true)
	a.Jcc(CondE, toGo)
	a.Load(RSI, RDI, rd.CtxSite)
	a.Load(RDX, RSI, rd.SiteRets+rd.SliceLen)
	a.Load(RSI, RSI, rd.SiteRets)
	a.Load(R10, RCX, rd.CtxL)
	g.siteMoves(R10, RDI, rd.CtxR, RCX, rd.CtxR)
	a.Load(RSI, RDI, rd.CtxX)
	a.Load(RDX, RSI, rd.XCalls+rd.SliceLen)
	a.OpImm(Sub, RDX, 1, true)
	a.Store(RSI, rd.XCalls+rd.SliceLen, RDX)
	a.Load(RDX, RSI, rd.XDepth)
	a.OpImm(Sub, RDX, 1, true)
	a.Store(RSI, rd.XDepth, RDX)
	a.Load(RDX, RDI, rd.CtxNc)
	a.Store(RSI, rd.XNc, RDX)
	a.Load(RDX, RDI, rd.CtxNsp)
	a.Store(RSI, rd.XNsp, RDX)
	a.Load(RDX, RDI, rd.CtxRch)
	a.Store(RSI, rd.XRc, RDX)
	a.Load(RDX, RDI, rd.CtxRsp)
	a.Store(RSI, rd.XRsp, RDX)
	a.Load(RBX, RDI, rd.CtxRetAddr)
	a.OpImm(Add, RBX, 8, true)
	a.MovRR(RDI, RCX)
	a.MovRR(R12, R10)
	a.Load(RDX, RDI, rd.CtxF)
	a.LoadS32(R11, RDX, rd.FrameTick)
	g.dispatch()
	a.Bind(toGo)
	a.Ret()
}

// takeRefs gives the callee of a native call, its frame at R10 and the
// exec at RSI, its reference slots from the exec's chunk, cleared, as
// Go's call does, the chunk and slot to give back kept in its context
// (R10's): the slots in the context's r and the frame's r. A body that
// holds no reference (compiled.refs) reads and writes none, and takes
// none: its context's r is 0 and its frame's r is left as it was. RCX,
// RSI, R10 and RDI are kept.
func (g *gen) takeRefs() {
	a := g.a
	const cc = rd.FrameNctx
	none, clear, cleared, same := a.NewLabel(), a.NewLabel(), a.NewLabel(), a.NewLabel()
	a.Load(RDX, RSI, rd.XRc)
	a.Store(R10, cc+rd.CtxRch, RDX)
	a.Load(RDX, RSI, rd.XRsp)
	a.Store(R10, cc+rd.CtxRsp, RDX)
	a.StoreImm(R10, cc+rd.CtxR, 0)
	a.Load(RDX, R10, cc+rd.CtxCd)
	a.LoadU8(R8, RDX, rd.CdRefs)
	a.Op(Test, R8, R8, false)
	a.Jcc(CondE, none)
	a.Load(RDX, RDX, rd.CdRslots)
	// R8 the chunk's first slot, R9 the slot taken first.
	a.Load(R8, RSI, rd.XRc)
	a.LeaIdx(R8, R8, R8, 2, 0)
	a.ShiftImm(Shl, R8, 3, true)
	a.OpMem(Add, R8, RSI, rd.XRchunks, true)
	a.Load(R8, R8, 0)
	a.Load(R9, RSI, rd.XRsp)
	a.LeaIdx(RAX, R9, RDX, 1, 0)
	a.Store(RSI, rd.XRsp, RAX)
	a.ShiftImm(Shl, R9, 4, true)
	a.Op(Add, R8, R9, true)
	a.Store(R10, cc+rd.CtxR, R8)
	// Cleared from the last: each payload plainly, each pointer by
	// storeRef.
	a.Bind(clear)
	a.Op(Test, RDX, RDX, true)
	a.Jcc(CondE, cleared)
	a.OpImm(Sub, RDX, 1, true)
	a.MovRR(R8, RDX)
	a.ShiftImm(Shl, R8, 4, true)
	a.OpMem(Add, R8, R10, cc+rd.CtxR, true)
	a.StoreImm(R8, 8, 0)
	a.Op(Xor, R9, R9, false)
	g.storeRef()
	a.Jmp(clear)
	a.Bind(cleared)
	// The frame's r: the slots.
	a.Load(RDX, R10, cc+rd.CtxCd)
	a.Load(RDX, RDX, rd.CdRslots)
	a.Store(R10, rd.FrameR+rd.SliceLen, RDX)
	a.Store(R10, rd.FrameR+rd.SliceCap, RDX)
	a.Load(R9, R10, cc+rd.CtxR)
	a.CmpMem(R9, R10, rd.FrameR, true)
	a.Jcc(CondE, same)
	a.Lea(R8, R10, rd.FrameR)
	g.storeRef()
	a.Bind(same)
	a.Bind(none)
}

// siteMoves makes the RDX moves at RSI from the slots at R12 and the
// reference slots at [srcR+srcDisp] to the slots at dst and the reference
// slots at [dstR+dstDisp]: a number's word, a v128's two, a reference's
// pointer by storeRef and its payload. RAX, RDX, RSI, R8 and R9 are its
// scratch.
func (g *gen) siteMoves(dst, srcR Reg, srcDisp int32, dstR Reg, dstDisp int32) {
	a := g.a
	loop, done, two, ref, next := a.NewLabel(), a.NewLabel(), a.NewLabel(), a.NewLabel(), a.NewLabel()
	a.Bind(loop)
	a.Op(Test, RDX, RDX, true)
	a.Jcc(CondE, done)
	a.LoadU32(R8, RSI, rd.MoveFrom)
	a.LoadU32(R9, RSI, rd.MoveTo)
	a.LoadU8(RAX, RSI, rd.MoveK)
	a.OpImm(Cmp, RAX, rd.MoveRef, false)
	a.Jcc(CondE, ref)
	a.OpImm(Cmp, RAX, rd.MoveV128, false)
	a.LoadIdx(RAX, R12, R8, 8, 0)
	a.StoreIdx(dst, R9, 8, 0, RAX)
	a.Jcc(CondE, two)
	a.Bind(next)
	a.OpImm(Add, RSI, rd.MoveSize, true)
	a.OpImm(Sub, RDX, 1, true)
	a.Jmp(loop)
	a.Bind(two)
	a.LoadIdx(RAX, R12, R8, 8, 8)
	a.StoreIdx(dst, R9, 8, 8, RAX)
	a.Jmp(next)
	a.Bind(ref)
	a.ShiftImm(Shl, R9, 4, true)
	a.OpMem(Add, R9, dstR, dstDisp, true)
	a.ShiftImm(Shl, R8, 4, true)
	a.OpMem(Add, R8, srcR, srcDisp, true)
	a.Load(RAX, R8, 8)
	a.Store(R9, 8, RAX)
	a.Load(RAX, R8, 0)
	a.MovRR(R8, R9)
	a.MovRR(R9, RAX)
	g.storeRef()
	a.Jmp(next)
	a.Bind(done)
}
