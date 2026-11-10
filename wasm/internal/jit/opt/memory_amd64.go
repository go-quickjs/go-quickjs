package opt

import (
	"math"
	"math/bits"
	"unsafe"

	"github.com/go-quickjs/go-quickjs/internal/jit/asm/amd64"
	wjit "github.com/go-quickjs/go-quickjs/wasm/internal/jit"
)

// The accesses of any memory on amd64 -- another than 0, one of i64
// addresses, a shared one -- and at any offset: what plainAccess does not
// say of an access. The effective address is in a register, or a register
// and a displacement; its end is checked against the memory's size, read
// where the baseline reads it (its README's Memory): memory 0's from the
// context, another's from the memories' table (Ctx.Mems), a shared one's
// through the address of its size, at each access. The base is R15 for
// memory 0, else loaded from the table after the check.

// The context's fields only these accesses use.
var (
	offMems       = int32(unsafe.Offsetof(wjit.Ctx{}.Mems))
	offMemSizePtr = int32(unsafe.Offsetof(wjit.Ctx{}.MemSizePtr))
)

// noReg says no register.
const noReg = amd64.Reg(0xff)

// memOp is where an access is: [base + idx + disp], or [base + disp] where
// idx is noReg.
type memOp struct {
	base, idx amd64.Reg
	disp      int32
}

// memEntry loads into p the address of memory mem's entry in the memories'
// table, less the displacement it returns, at which the entry's three
// words are -- the base, the size, the address of the size -- from p.
func (g *gen) memEntry(p amd64.Reg, mem uint32) int32 {
	e := uint64(mem) * wjit.MemEntryWords * 8
	if e+16 <= math.MaxInt32 {
		g.a.Load(p, rCtx, offMems)
		return int32(e)
	}
	g.a.MovImm(p, e)
	g.a.AddMem(p, rCtx, offMems)
	return 0
}

// sizeInto loads the size in bytes of access v's memory into s0.
func (g *gen) sizeInto(v *Value) {
	a := g.a
	mem := memIndex(v)
	shared := v.Aux2&memShared != 0
	switch {
	case mem == 0 && shared:
		a.Load(s0, rCtx, offMemSizePtr)
		a.Load(s0, s0, 0)
	case mem == 0:
		a.Load(s0, rCtx, offMemSize)
	case shared:
		d := g.memEntry(s0, mem)
		a.Load(s0, s0, d+16)
		a.Load(s0, s0, 0)
	default:
		d := g.memEntry(s0, mem)
		a.Load(s0, s0, d+8)
	}
}

// memAccess checks memory access v and returns its operand. An atomic
// access (align) traps first where its effective address is not a
// multiple of its size, after an i64 memory's address and offset trap
// where their sum carries, as the interpreter's do; then the end is checked
// unless the access needs no check.
//
// own is a register the instruction has for the effective address, which
// it may lose, or noReg, s1 then being taken where the address's register
// cannot be: for an i64 memory's address plus its offset, or an offset
// past 2^31-1, which are made in 64 bits. base is the register a memory
// other than 0's base is loaded into -- s0 or s1, which the effective
// address is then not in -- or noReg to add the base to the effective
// address in s1 or own, leaving s0 to the instruction.
func (g *gen) memAccess(v *Value, own, base amd64.Reg, align bool) memOp {
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
	var ea amd64.Reg
	var disp int32
	// rest is how far past ea the access's end is: its offset and its
	// size, or its size where the offset is in ea.
	var rest uint64
	if addr.Op == OpConst {
		c := constAddr(addr)
		e, carry := bits.Add64(c, off, 0)
		end, carry2 := bits.Add64(e, size, 0)
		switch {
		case carry != 0:
			a.Jmp(oob)
		case align && e&(size-1) != 0:
			a.Jmp(g.trap(wjit.TrapUnalignedAccess, v.At))
		case carry2 != 0:
			a.Jmp(oob)
		}
		// Past a jump to a trap, nothing below is reached.
		if !v.NoCheck {
			g.sizeInto(v)
			if end <= math.MaxInt32 {
				a.OpImm(amd64.Cmp, s0, int32(end), true)
			} else {
				a.MovImm(dst, end)
				a.Op(amd64.Cmp, s0, dst, true)
			}
			a.Jcc(amd64.CondB, oob)
		}
		a.MovImm(dst, e)
		ea = dst
	} else {
		r := g.intoG(addr, dst)
		if !wide && off+size <= math.MaxInt32 {
			// The offset is the displacement: the zero-extended address plus
			// it does not wrap.
			ea, disp, rest = r, int32(off), off+size
			if align {
				a.Lea(s0, r, int32(off&(size-1)))
				a.OpImm(amd64.And, s0, int32(size-1), false)
				a.Jcc(amd64.CondNE, g.trap(wjit.TrapUnalignedAccess, v.At))
			}
		} else {
			if r != dst {
				a.MovRR(dst, r)
			}
			if off != 0 {
				if off <= math.MaxInt32 {
					a.OpImm(amd64.Add, dst, int32(off), true)
				} else {
					a.MovImm(s0, off)
					a.Op(amd64.Add, dst, s0, true)
				}
				if wide && !v.NoCheck {
					a.Jcc(amd64.CondB, oob)
				}
			}
			ea, rest = dst, size
			if align {
				a.MovRR(s0, dst)
				a.OpImm(amd64.And, s0, int32(size-1), false)
				a.Jcc(amd64.CondNE, g.trap(wjit.TrapUnalignedAccess, v.At))
			}
		}
		if !v.NoCheck {
			// The end is past the size where the size less rest borrows, or
			// is below ea: no sum here carries.
			g.sizeInto(v)
			a.OpImm(amd64.Sub, s0, int32(rest), true)
			a.Jcc(amd64.CondB, oob)
			a.Op(amd64.Cmp, ea, s0, true)
			a.Jcc(amd64.CondA, oob)
		}
	}
	if mem == 0 {
		return memOp{base: rMem, idx: ea, disp: disp}
	}
	if base != noReg {
		d := g.memEntry(base, mem)
		a.Load(base, base, d)
		return memOp{base: base, idx: ea, disp: disp}
	}
	// The base added to the effective address, in a register of the
	// instruction's own.
	if ea != dst {
		a.MovRR(dst, ea)
	}
	d := g.memEntry(s0, mem)
	a.AddMem(dst, s0, d)
	return memOp{base: dst, idx: noReg, disp: disp}
}

// loadAny lowers a load that is not a plain access.
func (g *gen) loadAny(v *Value) {
	a := g.a
	m := g.memAccess(v, noReg, s0, false)
	if v.Type.IsFloat() {
		t := g.defX(v)
		if v.Type == TF32 {
			a.LoadSSIdx(t, m.base, m.idx, m.disp)
		} else {
			a.LoadSDIdx(t, m.base, m.idx, m.disp)
		}
		g.done(v, int(t))
		return
	}
	t := g.defG(v)
	wide := v.Type == TI64
	switch {
	case v.Size == 8:
		a.LoadIdx(t, m.base, m.idx, 1, m.disp)
	case v.Size == 4 && v.Signed:
		a.LoadS32Idx(t, m.base, m.idx, m.disp)
	case v.Size == 4:
		a.LoadU32Idx(t, m.base, m.idx, m.disp)
	case v.Size == 2 && v.Signed:
		a.LoadS16Idx(t, m.base, m.idx, m.disp, wide)
	case v.Size == 2:
		a.LoadU16Idx(t, m.base, m.idx, m.disp)
	case v.Signed:
		a.LoadS8Idx(t, m.base, m.idx, m.disp, wide)
	default:
		a.LoadU8Idx(t, m.base, m.idx, m.disp)
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
			a.StoreSSIdx(m.base, m.idx, m.disp, xv)
		} else {
			a.StoreSDIdx(m.base, m.idx, 1, m.disp, xv)
		}
		return
	}
	base := s0
	if g.locOf(x).kind != lGPR {
		base = noReg
	}
	m := g.memAccess(v, noReg, base, false)
	rv := g.intoG(x, s0)
	if m.idx == noReg {
		switch v.Size {
		case 1:
			a.Store8(m.base, m.disp, rv)
		case 2:
			a.Store16(m.base, m.disp, rv)
		case 4:
			a.Store32(m.base, m.disp, rv)
		default:
			a.Store(m.base, m.disp, rv)
		}
		return
	}
	switch v.Size {
	case 1:
		a.Store8Idx(m.base, m.idx, m.disp, rv)
	case 2:
		a.Store16Idx(m.base, m.idx, m.disp, rv)
	case 4:
		a.Store32Idx(m.base, m.idx, m.disp, rv)
	default:
		a.StoreIdx(m.base, m.idx, 1, m.disp, rv)
	}
}
