package opt

import "math"

// What bce proves of addresses from their values: an access of memory 0,
// unshared and of i32 addresses, whose address is at most a bound the
// values show, and whose end, the bound plus the offset and the size, is
// within the memory's least size -- the minimum the module declares, which
// an imported memory has at least when the module is linked, and which a
// memory never goes below -- is in bounds wherever it runs, and is not
// checked.
//
// A bound is the address's greatest value as an unsigned i32: a constant's,
// a mask's, a byte or a half loaded unsigned, a shift right's, a sum's
// where it cannot wrap, a phi's greatest argument's, and a loop's induction
// variable's -- a phi whose back edge brings it plus a constant step,
// taken only where a comparison with a constant bounds that sum: the phi
// is then the value it enters with or one the comparison let through, and,
// where the sum cannot wrap, as many steps from the value it enters with.

// maxU32 is the bound of an i32 nothing is known of.
const maxU32 = math.MaxUint32

// ubState is the bounds of i32 values, by ID, found as they are asked for:
// state 1 while a value's is being found, a phi's cycle bounded by nothing,
// 2 once found.
type ubState struct {
	bound []uint64
	state []uint8
}

func (u *ubState) reset(n int) {
	u.bound = grown(u.bound, n)
	u.state = grown(u.state, n)
	clear(u.state)
}

// upper is the greatest value of i32 v as an unsigned integer.
func (u *ubState) upper(v *Value) uint64 {
	if v.Type != TI32 {
		return maxU32
	}
	if v.Op == OpConst {
		return uint64(uint32(v.Aux))
	}
	switch u.state[v.ID] {
	case 1:
		return maxU32
	case 2:
		return u.bound[v.ID]
	}
	u.state[v.ID] = 1
	b := u.find(v)
	u.bound[v.ID], u.state[v.ID] = b, 2
	return b
}

func (u *ubState) find(v *Value) uint64 {
	switch v.Op {
	case OpAnd:
		return min(u.upper(v.Args[0]), u.upper(v.Args[1]))
	case OpShrU:
		if c := v.Args[1]; c.Op == OpConst {
			return u.upper(v.Args[0]) >> (c.Aux & 31)
		}
	case OpAdd:
		if s := u.upper(v.Args[0]) + u.upper(v.Args[1]); s <= maxU32 {
			return s
		}
	case OpLoad:
		if !v.Signed && v.Size < 4 {
			return 1<<(8*v.Size) - 1
		}
	case OpPhi:
		return u.phi(v)
	}
	return maxU32
}

// phi is the bound of phi p: of the greatest of its arguments', its
// induction step's from where it enters with a constant.
func (u *ubState) phi(p *Value) uint64 {
	h := p.Block
	var hi uint64
	var step, last uint64
	signed := false
	steps := 0
	for i, a := range p.Args {
		if a.Op == OpAdd && a.Args[0] == p && a.Args[1].Op == OpConst {
			k := uint64(uint32(a.Args[1].Aux))
			lim, sgn, ok := edgeBound(a, h.Preds[i], h)
			if !ok || k == 0 || k >= 1<<31 {
				return maxU32
			}
			if steps > 0 && (k != step || lim != last || sgn != signed) {
				return maxU32
			}
			step, last, signed = k, lim, sgn
			steps++
			continue
		}
		hi = max(hi, u.upper(a))
	}
	if steps == 0 {
		return hi
	}
	// The values the phi enters with are below the step's bound, the
	// arguments not steps constants of one value: the sum of the bound and
	// the step does not wrap, so every value the phi takes is that
	// constant plus some steps.
	var init *Value
	for _, a := range p.Args {
		if a.Op == OpAdd && a.Args[0] == p {
			continue
		}
		if a.Op != OpConst || init != nil && uint32(a.Aux) != uint32(init.Aux) {
			init = nil
			if signed {
				return maxU32
			}
			break
		}
		init = a
	}
	if signed {
		// The values below a signed bound are not negative where the
		// constant entered with is not and nothing wraps.
		if init == nil || uint64(uint32(init.Aux)) > math.MaxInt32 || last+step > math.MaxInt32 {
			return maxU32
		}
	}
	if init == nil || last+step > maxU32 {
		return max(hi, last)
	}
	c := uint64(uint32(init.Aux))
	if c > last {
		return c
	}
	return last - (last-c)%step
}

// edgeBound is the bound the branches before the edge from block p to h
// put on a, where a comparison of a with a constant decides the way to it:
// whether the comparison is signed, and whether there is one. The way is
// followed up through blocks of one predecessor that jump.
func edgeBound(a *Value, p, h *Block) (bound uint64, signed, ok bool) {
	cur, succ := p, h
	for range 4 {
		if cur.Kind == BIf {
			return cmpBound(cur.Control, a, succ == cur.Succs[0])
		}
		if cur.Kind != BPlain || len(cur.Preds) != 1 {
			return 0, false, false
		}
		cur, succ = cur.Preds[0], cur
	}
	return 0, false, false
}

// cmpBound is the bound comparison c of a with a constant puts on a where
// it is taken (taken) or not.
func cmpBound(c, a *Value, taken bool) (bound uint64, signed, ok bool) {
	if len(c.Args) != 2 || c.Args[0] != a || c.Args[1].Op != OpConst || a.Type != TI32 {
		return 0, false, false
	}
	k := uint64(uint32(c.Args[1].Aux))
	sk := int64(int32(uint32(c.Args[1].Aux)))
	switch {
	case c.Op == OpLtU && taken, c.Op == OpGeU && !taken:
		return k - 1, false, k > 0
	case c.Op == OpLeU && taken, c.Op == OpGtU && !taken:
		return k, false, true
	case c.Op == OpLtS && taken, c.Op == OpGeS && !taken:
		return uint64(sk - 1), true, sk > 0
	case c.Op == OpLeS && taken, c.Op == OpGtS && !taken:
		return uint64(sk), true, sk >= 0
	}
	return 0, false, false
}
