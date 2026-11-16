package nativegen

import (
	. "github.com/go-quickjs/go-quickjs/internal/jit/asm/arm64"
)

// forInStep is iter_next's step of a for-in's cursor, the cursor object in
// rS, as amd64's.
func (g *armGen) forInStep() {
	a := g.a
	keys, same, store, end, have := a.NewLabel(), a.NewLabel(), a.NewLabel(), a.NewLabel(), a.NewLabel()
	a.Load(rC, rS, offItIdx)
	g.cmpMem(rC, rS, offFiIndexKeys)
	a.BCond(GE, keys)
	// An index key: the element there in the dense storage, no hole.
	a.Load(r8, rS, offFiObj)
	a.MovRR(r9, rC)
	g.dense(r9, r8, 0, g.exit)
	// Its string, made already.
	a.MovImm(rD, intStringsLen)
	a.Cmp(rC, rD, true)
	a.BCond(HS, g.exit)
	a.Load(r9, rCtx, ncRT)
	a.Load(r9, r9, offRtIntStrs)
	a.Cbz(r9, g.exit, true)
	a.LoadIdx(r9, r9, rC, true)
	a.Cbz(r9, g.exit, true)
	// keyAtoms[keySlot(s)] = {s, the index's atom}, where it is not.
	g.keySlot(r8, r9)
	a.MovImm(rD, uint64(uint32(atomIndexTag)))
	a.Op(Orr, rD, rD, rC, true)
	g.cmpMem(r9, r8, offKAS)
	a.BCond(NE, store)
	a.LoadU32(rC, r8, offKAAtom)
	a.Cmp(rC, rD, false)
	a.BCond(NE, store)
	a.LoadU8(rC, r8, offKANone)
	a.Cbz(rC, same, false)
	a.Bind(store)
	// Nothing is written before here.
	a.Store32(r8, offKAAtom, rD)
	a.Store8(r8, offKANone, ZR)
	a.Store(rCtx, scr(sE), rS)
	a.Store(rCtx, scr(sI), r9)
	a.AddImm(rW, r8, int64(offKAS), true)
	a.MovRR(rC, r9)
	g.ptrStore()
	a.Load(rS, rCtx, scr(sE))
	a.Load(r9, rCtx, scr(sI))
	a.Load(rW, rIP, 0)
	a.Bind(same)
	// The cursor stepped; R[A] the string.
	g.incMem(rS, offItIdx, 1)
	a.MovRR(r8, r9)
	a.Load(r9, rCtx, ncStrBits)
	g.regA(rW)
	g.storeValue()

	// The snapshot's next key, while its object keeps the shape the keys
	// came from.
	a.Bind(keys)
	g.cmpMem(rC, rS, offFiKeys+8)
	a.BCond(GE, end)
	a.Load(r8, rS, offFiObj)
	a.Cbz(r8, have, true)
	a.Load(r9, rS, offFiShape)
	a.Cbz(r9, g.exit, true)
	g.cmpMem(r9, r8, offObjShape)
	a.BCond(NE, g.exit)
	a.Bind(have)
	a.ShiftImm(Lsl, rC, rC, 4, true)
	g.addMem(rC, rS, offFiKeys)
	g.incMem(rS, offItIdx, 1)
	g.loadValue(rC)
	g.regA(rW)
	g.storeValue()

	// Past the end: the cursor done and the loop left.
	a.Bind(end)
	a.MovImm(rC, 1)
	a.Store8(rS, offItDone, rC)
	g.immSC(rC)
	g.taken(rC)
}

// keySlot puts in dst the address of the runtime's keyAtoms entry of the
// string s (keySlot). rD is its scratch.
func (g *armGen) keySlot(dst, s Reg) {
	a := g.a
	a.MovImm(rD, keySlotMul)
	a.Mul(dst, s, rD)
	a.ShiftImm(Lsr, dst, dst, 56, true)
	a.MovImm(rD, uint64(sizeKA))
	a.Mul(dst, dst, rD)
	a.Load(rD, rCtx, ncRT)
	a.Op(Add, dst, dst, rD, true)
	a.AddImm(dst, dst, int64(offRtKeyAtoms), true)
}

// keyIndex puts in rC the index the string at the register address addr
// names, its num word in r9, as amd64's: a flat ASCII string of up to
// nine digits in canonical form, read digit by digit; anything else
// exits. rS is kept.
func (g *armGen) keyIndex(addr Reg) {
	a := g.a
	digits, loop := a.NewLabel(), a.NewLabel()
	g.kindIs(r9, KindString)
	a.Load(r9, addr, 8)
	a.Load(rC, r9, offStrLen)
	a.Cbz(rC, g.exit, true)
	a.CmpImm(rC, 9, true)
	a.BCond(HI, g.exit)
	a.Load(rD, r9, offStrLeft)
	a.Cbnz(rD, g.exit, true)
	a.LoadU8(rD, r9, offStrASCII)
	a.Cbz(rD, g.exit, false)
	a.Load(r8, r9, offStrS)
	// No leading zero but "0" itself.
	a.LoadU8(r9, r8, 0)
	a.CmpImm(r9, '0', false)
	a.BCond(NE, digits)
	a.CmpImm(rC, 1, true)
	a.BCond(NE, g.exit)
	a.Bind(digits)
	a.MovImm(rD, 0)
	a.Bind(loop)
	a.LoadU8(r9, r8, 0)
	a.SubsImm(r9, r9, '0', true)
	a.CmpImm(r9, 9, true)
	a.BCond(HI, g.exit)
	a.AddLsl(rD, rD, rD, 2)
	a.AddLsl(rD, r9, rD, 1)
	a.AddImm(r8, r8, 1, true)
	a.SubsImm(rC, rC, 1, true)
	a.BCond(NE, loop)
	a.MovRR(rC, rD)
}
