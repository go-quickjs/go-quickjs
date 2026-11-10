package opt

import "github.com/go-quickjs/go-quickjs/internal/jit/asm/amd64"

// The atomic accesses on amd64, the baseline's sequences (its README's
// Memory): every store and read-modify-write a locked instruction -- XCHG,
// LOCK XADD, LOCK CMPXCHG -- so a plain load is a sequentially consistent
// one, and atomic.fence MFENCE. A store, a read-modify-write and a
// compare-and-exchange take CX for the address, DX for the operand or the
// replacement and AX for the expected or old value, which the allocator
// keeps free of what lives across them (clobbers.atomic); a memory other
// than 0's base is in R10.

// zeroExtend clears r's bits above the low width bytes.
func (g *gen) zeroExtend(r amd64.Reg, width uint8) {
	switch width {
	case 1:
		g.a.MovZX8(r, r)
	case 2:
		g.a.OpImm(amd64.And, r, 0xFFFF, false)
	case 4:
		g.a.MovRR32(r, r)
	}
}

// atomicIn moves x into register r, for parallel.
func (g *gen) atomicIn(r amd64.Reg, x *Value) pmove {
	return pmove{dst: loc{kind: lGPR, r: int(r)}, src: g.locOf(x), t: x.Type}
}

// atomic lowers an atomic access or atomic.fence.
func (g *gen) atomic(v *Value) {
	a := g.a
	const ax, cx, dx = amd64.RAX, amd64.RCX, amd64.RDX
	width := v.Size
	res := amd64.Reg(noReg)
	switch v.Op {
	case OpFence:
		a.Mfence()
		return
	case OpAtomicLoad:
		m := g.memAccess(v, noReg, s0, true)
		t := g.defG(v)
		switch width {
		case 1:
			a.LoadU8Idx(t, m.base, m.idx, m.disp)
		case 2:
			a.LoadU16Idx(t, m.base, m.idx, m.disp)
		case 4:
			a.LoadU32Idx(t, m.base, m.idx, m.disp)
		default:
			a.LoadIdx(t, m.base, m.idx, 1, m.disp)
		}
		g.done(v, int(t))
		return
	case OpAtomicCmpxchg:
		g.parallel([]pmove{g.atomicIn(cx, v.Args[0]), g.atomicIn(ax, v.Args[1]), g.atomicIn(dx, v.Args[2])})
		m := g.memAccess(v, cx, s1, true)
		// AX holds memory's old value after either outcome, but for its
		// upper bits after a store.
		a.LockCmpxchgIdx(m.base, m.idx, m.disp, dx, width)
		g.zeroExtend(ax, width)
		res = ax
	default:
		g.parallel([]pmove{g.atomicIn(cx, v.Args[0]), g.atomicIn(dx, v.Args[1])})
		m := g.memAccess(v, cx, s1, true)
		switch v.Op {
		case OpAtomicStore:
			a.XchgIdx(m.base, m.idx, m.disp, dx, width)
			return
		case OpAtomicAdd, OpAtomicSub, OpAtomicXchg:
			if v.Op == OpAtomicSub {
				a.MovRR(s0, dx)
				a.Op(amd64.Xor, dx, dx, false)
				a.Op(amd64.Sub, dx, s0, true)
			}
			if v.Op == OpAtomicXchg {
				a.XchgIdx(m.base, m.idx, m.disp, dx, width)
			} else {
				a.LockXaddIdx(m.base, m.idx, m.disp, dx, width)
			}
			g.zeroExtend(dx, width)
			res = dx
		default:
			// The old value loaded into AX, the new one made from it in s0,
			// and LOCK CMPXCHG storing it if the memory still holds the old
			// one, which loads the memory's value into AX if not, for the
			// next turn: AX's bits above the width stay clear.
			switch width {
			case 1:
				a.LoadU8Idx(ax, m.base, m.idx, m.disp)
			case 2:
				a.LoadU16Idx(ax, m.base, m.idx, m.disp)
			case 4:
				a.LoadU32Idx(ax, m.base, m.idx, m.disp)
			default:
				a.LoadIdx(ax, m.base, m.idx, 1, m.disp)
			}
			op := amd64.And
			switch v.Op {
			case OpAtomicOr:
				op = amd64.Or
			case OpAtomicXor:
				op = amd64.Xor
			}
			loop := a.NewLabel()
			a.Bind(loop)
			a.MovRR(s0, ax)
			a.Op(op, s0, dx, true)
			a.LockCmpxchgIdx(m.base, m.idx, m.disp, s0, width)
			a.Jcc(amd64.CondNE, loop)
			res = ax
		}
	}
	t := g.defG(v)
	if t != res {
		a.MovRR(t, res)
	}
	g.done(v, int(t))
}
