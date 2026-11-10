package opt

import "github.com/go-quickjs/go-quickjs/internal/jit/asm/arm64"

// The atomic accesses on arm64, the baseline's sequences (its README's
// Memory): a load is a load-acquire (LDAR), a store a store-release
// (STLR), a read-modify-write a loop of an exclusive load-acquire and
// store-release (LDAXR, STLXR) that goes round again when another thread
// broke the exclusive pair, register operations alone between the two, and
// atomic.fence DMB ISH. They take an address in one register: the memory's
// base plus the effective address. A store, a read-modify-write and a
// compare-and-exchange take X13 for the address, X14 for the operand or
// the expected value and X15 for the old value or the replacement, which
// the allocator keeps free of what lives across them (clobbers.atomic).

const (
	atomicAddr = bulkDst
	atomicArg  = bulkSrc
	atomicOld  = bulkLen
)

// atomicIn moves x into register r, for parallel.
func (g *gen) atomicIn(r arm64.Reg, x *Value) pmove {
	return pmove{dst: loc{kind: lGPR, r: int(r)}, src: g.locOf(x), t: x.Type}
}

// atomicAddress checks atomic access v, its address in own or, for noReg,
// where the allocator put it, and returns the register holding its
// address in memory: own, or s1.
func (g *gen) atomicAddress(v *Value, own arm64.Reg) arm64.Reg {
	m := g.memAccess(v, own, noReg, true)
	if m.idx == noReg {
		return m.base
	}
	p := own
	if p == noReg {
		p = s1
	}
	g.a.Op(arm64.Add, p, m.base, m.idx, true)
	return p
}

// atomic lowers an atomic access or atomic.fence.
func (g *gen) atomic(v *Value) {
	a := g.a
	width := v.Size
	wide := width == 8
	switch v.Op {
	case OpFence:
		a.DmbIsh()
		return
	case OpAtomicLoad:
		p := g.atomicAddress(v, noReg)
		t := g.defG(v)
		a.Ldar(t, p, width)
		g.done(v, int(t))
		return
	case OpAtomicCmpxchg:
		g.parallel([]pmove{g.atomicIn(atomicAddr, v.Args[0]), g.atomicIn(atomicArg, v.Args[1]), g.atomicIn(atomicOld, v.Args[2])})
		p := g.atomicAddress(v, atomicAddr)
		// The expected value's bits above the width are not compared:
		// the exclusive load clears the old value's.
		switch width {
		case 1:
			a.LogicImm(arm64.And, atomicArg, atomicArg, 0xFF, false)
		case 2:
			a.LogicImm(arm64.And, atomicArg, atomicArg, 0xFFFF, false)
		}
		loop, done := a.NewLabel(), a.NewLabel()
		a.Bind(loop)
		a.Ldaxr(s0, p, width)
		a.Cmp(s0, atomicArg, wide)
		a.BCond(arm64.NE, done)
		a.Stlxr(s1, atomicOld, p, width)
		a.Cbnz(s1, loop, false)
		a.Bind(done)
		t := g.defG(v)
		if t != s0 {
			a.MovRR(t, s0)
		}
		g.done(v, int(t))
		return
	}
	g.parallel([]pmove{g.atomicIn(atomicAddr, v.Args[0]), g.atomicIn(atomicArg, v.Args[1])})
	p := g.atomicAddress(v, atomicAddr)
	if v.Op == OpAtomicStore {
		a.Stlr(atomicArg, p, width)
		return
	}
	loop := a.NewLabel()
	a.Bind(loop)
	a.Ldaxr(atomicOld, p, width)
	src := s0
	switch v.Op {
	case OpAtomicAdd:
		a.Op(arm64.Add, s0, atomicOld, atomicArg, wide)
	case OpAtomicSub:
		a.Op(arm64.Sub, s0, atomicOld, atomicArg, wide)
	case OpAtomicAnd:
		a.Op(arm64.And, s0, atomicOld, atomicArg, wide)
	case OpAtomicOr:
		a.Op(arm64.Orr, s0, atomicOld, atomicArg, wide)
	case OpAtomicXor:
		a.Op(arm64.Eor, s0, atomicOld, atomicArg, wide)
	default:
		src = atomicArg
	}
	a.Stlxr(s1, src, p, width)
	a.Cbnz(s1, loop, false)
	t := g.defG(v)
	if t != atomicOld {
		a.MovRR(t, atomicOld)
	}
	g.done(v, int(t))
}
