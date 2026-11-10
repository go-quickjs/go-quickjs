package opt

import (
	"math"
	"math/bits"
	"unsafe"

	"github.com/go-quickjs/go-quickjs/internal/jit/asm/arm64"
	wjit "github.com/go-quickjs/go-quickjs/wasm/internal/jit"
)

// The accesses of any memory on arm64 -- another than 0, one of i64
// addresses, a shared one -- and at any offset: what plainAccess does not
// say of an access. The effective address, the address plus the offset in
// 64 bits, is in a register; its end is checked against the memory's size,
// read where the baseline reads it (its README's Memory): memory 0's in
// X21, another's from the memories' table (Ctx.Mems), a shared one's
// through the address of its size, at each access. The base is X20 for
// memory 0, else loaded from the table after the check.

// The context's fields only these accesses use.
var (
	offMems       = int32(unsafe.Offsetof(wjit.Ctx{}.Mems))
	offMemSizePtr = int32(unsafe.Offsetof(wjit.Ctx{}.MemSizePtr))
)

// noReg says no register.
const noReg = arm64.Reg(0xff)

// memOp is where an access is: [base + idx], or [base] where idx is noReg.
type memOp struct{ base, idx arm64.Reg }

// memEntry loads into p the address of memory mem's entry in the memories'
// table, less the displacement it returns, at which the entry's three
// words are -- the base, the size, the address of the size -- from p.
func (g *gen) memEntry(p arm64.Reg, mem uint32) int32 {
	e := uint64(mem) * wjit.MemEntryWords * 8
	if e+16 > math.MaxInt32 {
		refuse(RefuseOther, "memory %d", mem)
	}
	g.a.Load(p, rCtx, offMems)
	return int32(e)
}

// sizeInto loads the size in bytes of access v's memory into s0, but
// memory 0's unshared, which X21 holds, and returns the register holding
// it.
func (g *gen) sizeInto(v *Value) arm64.Reg {
	a := g.a
	mem := memIndex(v)
	shared := v.Aux2&memShared != 0
	switch {
	case mem == 0 && shared:
		a.Load(s0, rCtx, offMemSizePtr)
		a.Load(s0, s0, 0)
	case mem == 0:
		return rMemSize
	case shared:
		d := g.memEntry(s0, mem)
		a.Load(s0, s0, d+16)
		a.Load(s0, s0, 0)
	default:
		d := g.memEntry(s0, mem)
		a.Load(s0, s0, d+8)
	}
	return s0
}

// memAccess checks memory access v and returns its operand. An atomic
// access (align) traps first where its effective address is not a
// multiple of its size, after an i64 memory's address and offset trap
// where their sum carries, as the interpreter's do; then the end is checked
// unless the access needs no check.
//
// own is a register the instruction has for the effective address, which
// it may lose, or noReg, s1 then being taken where the address's register
// cannot be. base is the register a memory other than 0's base is loaded
// into -- s0 -- or noReg to add the base to the effective address in s1
// or own, leaving s0 to the instruction.
func (g *gen) memAccess(v *Value, own, base arm64.Reg, align bool) memOp {
	a := g.a
	addr := v.Args[0]
	mem := memIndex(v)
	wide := v.Aux2&memWide != 0
	off, size := v.Aux, uint64(v.Size)
	align = align && size > 1
	oob := g.trap(wjit.TrapMemOutOfBounds, v.At)
	dst := own
	if dst == noReg {
		dst = s1
	}
	var ea arm64.Reg
	if addr.Op == OpConst {
		c := constAddr(addr)
		e, carry := bits.Add64(c, off, 0)
		end, carry2 := bits.Add64(e, size, 0)
		switch {
		case carry != 0:
			a.B(oob)
		case align && e&(size-1) != 0:
			a.B(g.trap(wjit.TrapUnalignedAccess, v.At))
		case carry2 != 0:
			a.B(oob)
		}
		// Past a branch to a trap, nothing below is reached.
		if !v.NoCheck {
			sz := g.sizeInto(v)
			a.MovImm(dst, end)
			a.Cmp(sz, dst, true)
			a.BCond(arm64.LO, oob)
		}
		a.MovImm(dst, e)
		ea = dst
	} else {
		ea = g.intoG(addr, dst)
		switch {
		case off == 0:
		case !wide:
			// The zero-extended address plus the offset does not wrap.
			a.AddImm(dst, ea, int64(off), true)
			ea = dst
		default:
			if off < 1<<63 {
				a.AddsImm(dst, ea, int64(off), true)
			} else {
				a.MovImm(s0, off)
				a.Op(arm64.Adds, dst, ea, s0, true)
			}
			ea = dst
			if !v.NoCheck {
				a.BCond(arm64.HS, oob)
			}
		}
		if align {
			a.LogicImm(arm64.Ands, arm64.ZR, ea, size-1, true)
			a.BCond(arm64.NE, g.trap(wjit.TrapUnalignedAccess, v.At))
		}
		if !v.NoCheck {
			// The end is past the size where the size less the access's
			// size borrows, or is below ea: no sum here carries.
			sz := g.sizeInto(v)
			a.SubsImm(s0, sz, int64(size), true)
			a.BCond(arm64.LO, oob)
			a.Cmp(ea, s0, true)
			a.BCond(arm64.HI, oob)
		}
	}
	if mem == 0 {
		return memOp{base: rMem, idx: ea}
	}
	if base != noReg {
		d := g.memEntry(base, mem)
		a.Load(base, base, d)
		return memOp{base: base, idx: ea}
	}
	// The base added to the effective address, in a register of the
	// instruction's own.
	d := g.memEntry(s0, mem)
	a.Load(s0, s0, d)
	a.Op(arm64.Add, dst, s0, ea, true)
	return memOp{base: dst, idx: noReg}
}

// loadAny lowers a load that is not a plain access.
func (g *gen) loadAny(v *Value) {
	a := g.a
	m := g.memAccess(v, noReg, s0, false)
	if v.Type.IsFloat() {
		t := g.defX(v)
		if v.Type == TF32 {
			a.LoadSIdx(t, m.base, m.idx)
		} else {
			a.LoadFIdx(t, m.base, m.idx)
		}
		g.done(v, int(t))
		return
	}
	t := g.defG(v)
	wide := v.Type == TI64
	switch {
	case v.Size == 8:
		a.LoadIdx(t, m.base, m.idx, false)
	case v.Size == 4 && v.Signed:
		a.LoadS32Idx(t, m.base, m.idx)
	case v.Size == 4:
		a.LoadU32Idx(t, m.base, m.idx)
	case v.Size == 2 && v.Signed:
		a.LoadS16Idx(t, m.base, m.idx, wide)
	case v.Size == 2:
		a.LoadU16Idx(t, m.base, m.idx)
	case v.Signed:
		a.LoadS8Idx(t, m.base, m.idx, wide)
	default:
		a.LoadU8Idx(t, m.base, m.idx)
	}
	g.done(v, int(t))
}

// storeAny lowers a store that is not a plain access. An integer value not
// in a register is loaded into s0, so a memory's base is then added to the
// effective address.
func (g *gen) storeAny(v *Value) {
	a := g.a
	x := v.Args[1]
	if x.Type.IsFloat() {
		// A constant is made through s0: before the base is there.
		xv := g.intoX(x, x0)
		m := g.memAccess(v, noReg, s0, false)
		if v.Size == 4 {
			a.StoreSIdx(m.base, m.idx, xv)
		} else {
			a.StoreFIdx(m.base, m.idx, xv)
		}
		return
	}
	base := s0
	if l := g.locOf(x); l.kind != lGPR && (l.kind != lConst || constBits(l.c, x.Type) != 0) {
		base = noReg
	}
	m := g.memAccess(v, noReg, base, false)
	rv := g.readG(x, s0)
	if m.idx == noReg {
		switch v.Size {
		case 1:
			a.Store8(m.base, 0, rv)
		case 2:
			a.Store16(m.base, 0, rv)
		case 4:
			a.Store32(m.base, 0, rv)
		default:
			a.Store(m.base, 0, rv)
		}
		return
	}
	switch v.Size {
	case 1:
		a.Store8Idx(m.base, m.idx, rv)
	case 2:
		a.Store16Idx(m.base, m.idx, rv)
	case 4:
		a.Store32Idx(m.base, m.idx, rv)
	default:
		a.StoreIdx(m.base, m.idx, rv)
	}
}
