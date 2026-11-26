package main

import (
	. "github.com/go-quickjs/go-quickjs/internal/jit/asm/amd64"
	rd "github.com/go-quickjs/go-quickjs/wasm/internal/engine/internal/regdef"
)

// The allocations: struct.new, struct.new_default, array.new,
// array.new_default and array.new_fixed of the types the record says
// native code takes (natTi), from the pool of the exec's table for the
// instance (the exec's natPools): a struct type's entry its pool, an array type's a
// table of pools by the class of the arrays' blocks. The object taken, its
// values stored by the record's natVals -- a reference plainly, as a new
// object's field is -- it goes into the record's reference slot by
// storeRef and its pool's word is cleared by storeRef. An empty pool, no
// table, and an array past the pools go to the allocation stub (the exec's natAlloc),
// which fills the pool, or makes the array into its slot, and answers to
// take again, to go on, or to leave the instruction to Go (the exit).
//
// In a handler: RCX the record, RDX the type's index plus 1, RSI the
// object, R10 the address of its pool's word, RDX, R8, R9 and RAX the
// stores' scratch.

// allocRecord puts in RCX the address of the instruction's record, and in
// RDX its natTi, exiting where native code leaves the instruction to Go.
func (g *gen) allocRecord() {
	a := g.a
	g.bc(RCX, false)
	a.ImulImm(RCX, RCX, rd.GCSize, true)
	a.OpMem(Add, RCX, RDI, rd.CtxGC, true)
	a.LoadU32(RDX, RCX, rd.GCNatTi)
	a.Op(Test, RDX, RDX, false)
	a.Jcc(CondE, g.exit)
}

// allocStub asks the allocation stub for request kind of the instruction's
// record, which goes on at retry, past the instruction, or to the exit.
func (g *gen) allocStub(kind int32, retry Label) {
	a := g.a
	back, exit, done := a.NewLabel(), a.NewLabel(), a.NewLabel()
	a.Load(RAX, RBX, 0)
	g.bc(R8, false)
	a.ShiftImm(Shl, R8, 8, true)
	a.OpImm(Or, R8, kind, true)
	a.LeaLabel(RAX, back)
	a.Load(R9, RDI, rd.CtxX)
	a.JmpMem(R9, rd.XNatAlloc)
	a.Bind(back)
	a.Op(Test, R8, R8, true)
	a.Jcc(CondE, exit)
	a.OpImm(Cmp, R8, 2, true)
	a.Jcc(CondE, done)
	a.Jmp(retry)
	a.Bind(exit)
	a.Load(RAX, RBX, 0)
	a.Jmp(g.exit)
	a.Bind(done)
	g.next(1)
}

// poolTake takes the last object of the pool at R8 into RSI, leaving in
// R10 the address of its word, or goes to slow where it is empty.
func (g *gen) poolTake(slow Label) {
	a := g.a
	a.Op(Test, R8, R8, true)
	a.Jcc(CondE, slow)
	a.Load(R9, R8, rd.PoolCount)
	a.Op(Test, R9, R9, true)
	a.Jcc(CondE, slow)
	a.OpImm(Sub, R9, 1, true)
	a.Store(R8, rd.PoolCount, R9)
	a.LeaIdx(R10, R8, R9, 8, rd.PoolObjs)
	a.Load(RSI, R10, 0)
}

// putNew stores the object in RSI into the record's reference slot, clears
// its pool's word at R10, both by storeRef, and goes on.
func (g *gen) putNew() {
	a := g.a
	a.LoadU32(R8, RCX, rd.GCDst)
	a.ShiftImm(Shl, R8, 4, true)
	a.OpMem(Add, R8, RDI, rd.CtxR, true)
	a.StoreImm(R8, 8, 0)
	a.MovRR(R9, RSI)
	g.storeRef()
	a.MovRR(R8, R10)
	a.Op(Xor, R9, R9, false)
	g.storeRef()
	g.next(1)
}

// storeVal stores the value of kind, from the slot in R9 -- a numeric
// slot, or a reference slot -- at [base + to], to the natVal at RDX's. RAX
// and R9 are lost.
func (g *gen) storeVal(kind int32, base Reg) {
	a := g.a
	switch kind {
	case rd.NatRef:
		a.ShiftImm(Shl, R9, 4, true)
		a.OpMem(Add, R9, RDI, rd.CtxR, true)
		a.LoadVec(XM0, R9, 0)
		a.LoadU32(R9, RDX, rd.NatTo)
		a.StoreVecIdx(base, R9, 0, XM0)
		return
	case rd.NatV128:
		a.LoadIdx(RAX, R12, R9, 8, 0)
		a.LeaIdx(R9, R12, R9, 8, 0)
		a.LoadSD(XM0, R9, 8)
		a.LoadU32(R9, RDX, rd.NatTo)
		a.StoreIdx(base, R9, 1, 0, RAX)
		a.StoreSDIdx(base, R9, 1, 16, XM0)
		return
	}
	a.LoadIdx(RAX, R12, R9, 8, 0)
	a.LoadU32(R9, RDX, rd.NatTo)
	switch kind {
	case rd.Nat64:
		a.StoreIdx(base, R9, 1, 0, RAX)
	case rd.NatW32:
		a.MovRR32(RAX, RAX)
		a.StoreIdx(base, R9, 1, 0, RAX)
	case rd.NatW16:
		a.OpImm(And, RAX, 0xffff, false)
		a.StoreIdx(base, R9, 1, 0, RAX)
	case rd.NatW8:
		a.MovZX8(RAX, RAX)
		a.StoreIdx(base, R9, 1, 0, RAX)
	case rd.NatE32:
		a.Store32Idx(base, R9, 0, RAX)
	case rd.NatE16:
		a.Store16Idx(base, R9, 0, RAX)
	case rd.NatE8:
		a.Store8Idx(base, R9, 0, RAX)
	}
}

// natKinds are the kinds a natVal is stored by.
var natKinds = []int32{rd.NatRef, rd.Nat64, rd.NatW32, rd.NatW16, rd.NatW8, rd.NatV128}

// storeVals stores the record's natVals into the object at base: a
// struct's cells (RSI) or an array's elements. RDX, R8, R9 and RAX are
// lost.
func (g *gen) storeVals(base Reg) {
	a := g.a
	loop, end := a.NewLabel(), a.NewLabel()
	a.Load(RDX, RCX, rd.GCNatVals)
	a.Load(R8, RCX, rd.GCNatVals+rd.SliceLen)
	a.Bind(loop)
	a.Op(Test, R8, R8, true)
	a.Jcc(CondE, end)
	next := a.NewLabel()
	for _, k := range natKinds {
		skip := a.NewLabel()
		a.LoadU32(R9, RDX, rd.NatKind)
		a.OpImm(Cmp, R9, k, false)
		a.Jcc(CondNE, skip)
		a.LoadU32(R9, RDX, rd.NatFrom)
		g.storeVal(k, base)
		a.Jmp(next)
		a.Bind(skip)
	}
	a.Bind(next)
	a.OpImm(Add, RDX, rd.NatSize, true)
	a.OpImm(Sub, R8, 1, true)
	a.Jmp(loop)
	a.Bind(end)
}

// arrayLength puts in R9 the length the record's array is made of: its
// last operand's numeric slot's, or, with no operand, its values'.
func (g *gen) arrayLength() {
	a := g.a
	fixed, done := a.NewLabel(), a.NewLabel()
	a.Load(R9, RCX, rd.GCOps+rd.SliceLen)
	a.Op(Test, R9, R9, true)
	a.Jcc(CondE, fixed)
	a.Load(R8, RCX, rd.GCOps)
	a.LeaIdx(R8, R8, R9, 4, -4)
	a.LoadU32(R9, R8, 0)
	a.LeaIdx(R8, R12, R9, 8, 0)
	a.LoadU32(R9, R8, 0)
	a.Jmp(done)
	a.Bind(fixed)
	a.Load(R9, RCX, rd.GCNatVals+rd.SliceLen)
	a.Bind(done)
}

func (g *gen) alloc() {
	a := g.a
	for _, fields := range []bool{true, false} {
		x, name := rd.XStructNew, "StructNew"
		if !fields {
			x, name = rd.XStructNewDefault, "StructNewDefault"
		}
		g.hx(x, name, func() {
			retry, slow := a.NewLabel(), a.NewLabel()
			g.allocRecord()
			a.Bind(retry)
			a.Load(R8, RDI, rd.CtxX)
			a.Load(R8, R8, rd.XNatPools)
			a.Op(Test, R8, R8, true)
			a.Jcc(CondE, slow)
			a.LoadIdx(R8, R8, RDX, 8, -8)
			g.poolTake(slow)
			if fields {
				g.storeVals(RSI)
			}
			g.putNew()
			a.Bind(slow)
			g.allocStub(1, retry)
		})
	}
	for _, x := range []int{rd.XArrayNew, rd.XArrayNewDefault, rd.XArrayNewFixed} {
		name := map[int]string{rd.XArrayNew: "ArrayNew", rd.XArrayNewDefault: "ArrayNewDefault", rd.XArrayNewFixed: "ArrayNewFixed"}[x]
		g.hx(x, name, func() {
			retry, slow, big := a.NewLabel(), a.NewLabel(), a.NewLabel()
			g.allocRecord()
			a.Bind(retry)
			// The class: the elements' bytes, 16 a unit, at most the
			// pools' arrays'.
			g.arrayLength()
			a.LoadU32(R8, RCX, rd.GCNatShift)
			a.Store(RDI, rd.CtxFn, RCX)
			a.MovRR(RCX, R8)
			a.ShiftCL(Shl, R9, true)
			a.Load(RCX, RDI, rd.CtxFn)
			a.OpImm(Cmp, R9, rd.ArrayBlockBytes, true)
			a.Jcc(CondA, big)
			a.OpImm(Add, R9, 15, true)
			a.ShiftImm(Shr, R9, 4, true)
			a.Load(R8, RDI, rd.CtxX)
			a.Load(R8, R8, rd.XNatClasses)
			a.LoadU8Idx(R9, R8, R9, 0)
			a.Load(R8, RDI, rd.CtxX)
			a.Load(R8, R8, rd.XNatPools)
			a.Op(Test, R8, R8, true)
			a.Jcc(CondE, slow)
			a.LoadIdx(R8, R8, RDX, 8, -8)
			a.Op(Test, R8, R8, true)
			a.Jcc(CondE, slow)
			a.LoadIdx(R8, R8, R9, 8, 0)
			g.poolTake(slow)
			g.arrayLength()
			a.Store(RSI, rd.ArrayLen, R9)
			switch x {
			case rd.XArrayNewFixed:
				// The elements' first, plus each value's offset.
				a.Load(RAX, RSI, rd.ArrayElems)
				a.Store(RDI, rd.CtxCd, RAX)
				g.storeElems()
			case rd.XArrayNew:
				g.fillElems()
			}
			g.putNew()
			a.Bind(slow)
			g.allocStub(2, retry)
			a.Bind(big)
			g.allocStub(3, retry)
		})
	}
}

// storeElems stores array.new_fixed's values into the elements of the
// array in RSI, their first in the context's cd word.
func (g *gen) storeElems() {
	a := g.a
	loop, end := a.NewLabel(), a.NewLabel()
	a.Load(RDX, RCX, rd.GCNatVals)
	a.Load(R8, RCX, rd.GCNatVals+rd.SliceLen)
	a.Bind(loop)
	a.Op(Test, R8, R8, true)
	a.Jcc(CondE, end)
	next := a.NewLabel()
	for _, k := range []int32{rd.Nat64, rd.NatE32, rd.NatE16, rd.NatE8, rd.NatRef, rd.NatE128} {
		skip := a.NewLabel()
		a.LoadU32(R9, RDX, rd.NatKind)
		a.OpImm(Cmp, R9, k, false)
		a.Jcc(CondNE, skip)
		a.LoadU32(R9, RDX, rd.NatFrom)
		g.storeValAt(k)
		a.Jmp(next)
		a.Bind(skip)
	}
	a.Bind(next)
	a.OpImm(Add, RDX, rd.NatSize, true)
	a.OpImm(Sub, R8, 1, true)
	a.Jmp(loop)
	a.Bind(end)
}

// storeValAt stores the value of kind from the slot in R9 at the
// elements' first (the context's cd word) plus the natVal's offset.
func (g *gen) storeValAt(kind int32) {
	a := g.a
	switch kind {
	case rd.NatRef:
		a.ShiftImm(Shl, R9, 4, true)
		a.OpMem(Add, R9, RDI, rd.CtxR, true)
		a.LoadVec(XM0, R9, 0)
	case rd.NatE128:
		a.LeaIdx(R9, R12, R9, 8, 0)
		a.LoadVec(XM0, R9, 0)
	default:
		a.LoadIdx(RAX, R12, R9, 8, 0)
	}
	a.LoadU32(R9, RDX, rd.NatTo)
	a.OpMem(Add, R9, RDI, rd.CtxCd, true)
	switch kind {
	case rd.NatRef, rd.NatE128:
		a.StoreVec(R9, 0, XM0)
	case rd.Nat64:
		a.Store(R9, 0, RAX)
	case rd.NatE32:
		a.Store32(R9, 0, RAX)
	case rd.NatE16:
		a.Store16(R9, 0, RAX)
	case rd.NatE8:
		a.Store8(R9, 0, RAX)
	}
}

// fillElems stores array.new's value into each element of the array in
// RSI, of the length the record's operand says.
func (g *gen) fillElems() {
	a := g.a
	a.Load(RDX, RCX, rd.GCNatVals)
	a.LoadU32(R9, RDX, rd.NatFrom)
	a.LoadU32(R8, RDX, rd.NatKind)
	a.Store(RDI, rd.CtxCd, R8)
	end := a.NewLabel()
	for _, k := range []int32{rd.Nat64, rd.NatE32, rd.NatE16, rd.NatE8, rd.NatRef, rd.NatE128} {
		skip := a.NewLabel()
		a.LoadU32(R8, RDI, rd.CtxCd)
		a.OpImm(Cmp, R8, k, false)
		a.Jcc(CondNE, skip)
		// The value, the elements' first in RDX, the count in R8.
		switch k {
		case rd.NatRef:
			a.ShiftImm(Shl, R9, 4, true)
			a.OpMem(Add, R9, RDI, rd.CtxR, true)
			a.LoadVec(XM0, R9, 0)
		case rd.NatE128:
			a.LeaIdx(R9, R12, R9, 8, 0)
			a.LoadVec(XM0, R9, 0)
		default:
			a.LoadIdx(RAX, R12, R9, 8, 0)
		}
		g.arrayLength()
		a.MovRR(R8, R9)
		a.Load(RDX, RSI, rd.ArrayElems)
		loop := a.NewLabel()
		a.Bind(loop)
		a.Op(Test, R8, R8, true)
		a.Jcc(CondE, end)
		size := int32(8)
		switch k {
		case rd.NatRef, rd.NatE128:
			a.StoreVec(RDX, 0, XM0)
			size = 16
		case rd.Nat64:
			a.Store(RDX, 0, RAX)
		case rd.NatE32:
			a.Store32(RDX, 0, RAX)
			size = 4
		case rd.NatE16:
			a.Store16(RDX, 0, RAX)
			size = 2
		case rd.NatE8:
			a.Store8(RDX, 0, RAX)
			size = 1
		}
		a.OpImm(Add, RDX, size, true)
		a.OpImm(Sub, R8, 1, true)
		a.Jmp(loop)
		a.Bind(skip)
	}
	a.Bind(end)
}
