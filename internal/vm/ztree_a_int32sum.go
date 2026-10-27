package vm

import (
	"math"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
)

// Sums under a bitwise operator: (a + b) | 0, (p + 8) >> 2, (h - l) >>> 0,
// what code compiled from C makes of every integer addition. The sum had
// been a node of its own, whose Value the operator converted: a call, two
// checks that the operands are numbers, Float's NaN test, and the
// conversion's. Here the operator's int32 tree makes the sum in place, of
// operands that are locals, number constants and int32 trees (i32Of's),
// and converts it with one test: where a + b, made of their float64s, is
// an int32 exactly, both were numbers -- what is no number has a NaN's
// bits, and a NaN makes the sum one -- and the int32 is the operator's
// operand, as ToInt32 of the sum would be. Two int32 trees are summed as
// int64s, exactly as the float64s would be, and wrapped to 32 bits, as
// ToInt32 wraps.
//
// Anything else -- a sum that is no int32, a string, an object with
// valueOf, a BigInt -- is the sum's arithSlow, of the operands in their
// order, then the operator's, as the two nodes made them.
//
// The statement of a store to a local of such an operator, C = b + 1 | 0,
// is bitwiseImmStoreAt's, which tbuilder.op calls through storeArith: the
// int32 tree's value stored, for one of i32Sure's or a sum here, and
// bitwiseImmStore's statement otherwise.
//
// The code is closures assigned in this file's inits, laid out with the
// rest of the int32 trees' (see ztree_a_int32.go).

// i32SumSite is a sum's operator aop at apc, and the bitwise operator op
// at bpc with its constant k, kv as a Value, over it.
type i32SumSite struct {
	aop  bytecode.Op
	apc  int
	op   bytecode.Op
	k    int32
	kv   Value
	bpc  int
	swap bool
}

var (
	// i32Summed reports whether x is a sum or a difference i32Sum makes in
	// place: of locals, number constants and int32 trees, not of two
	// constants.
	i32Summed func(x tentry) bool
	// i32Sum is x op k as an int32 tree for a sum x i32Summed reports, or
	// nil.
	i32Sum func(op bytecode.Op, x tentry, k int32, pc int) ti32
	// i32SumSlow is s's operators of a and b, the sum's operands in their
	// order -- swapped where s.swap is set -- where the sum is no int32.
	i32SumSlow func(c *tctx, s *i32SumSite, a, b Value) int32
	// bitwiseImmStoreAt is bitwiseImmStore, local k = x op n, with a
	// statement of the int32 tree for an x that is an int32 or a sum
	// i32Sum makes.
	bitwiseImmStoreAt func(op bytecode.Op, k uint32, x tentry, n int32, pc int) tstmt
)

// i32Exact is f as an int32, where it is one exactly.
func i32Exact(f float64) (int32, bool) {
	i := int32(f)
	return i, float64(i) == f
}

// i32SumOperand reports whether e is an operand of a sum made in place.
func i32SumOperand(e tentry) bool {
	return e.v == nil && e.slot < 0 && (e.local || e.number) || i32Sure(e)
}

func init() {
	i32Summed = func(x tentry) bool {
		a := x.arith
		if a == nil || x.v == nil || x.slot >= 0 || a.imm || a.op != bytecode.OpAdd && a.op != bytecode.OpSub {
			return false
		}
		return i32SumOperand(a.x) && i32SumOperand(a.y) && !(a.x.number && a.y.number)
	}

	i32SumSlow = func(c *tctx, s *i32SumSite, a, b Value) int32 {
		if s.swap {
			a, b = b, a
		}
		if a.IsNumber() && b.IsNumber() {
			f := a.num + b.num
			if s.aop == bytecode.OpSub {
				f = a.num - b.num
			}
			return i32Do(s.op, toInt32(f), s.k)
		}
		v := c.arithSlow(s.aop, a, b, s.apc)
		if v.IsNumber() {
			return i32Do(s.op, toInt32(v.num), s.k)
		}
		return i32Slow(c, s.op, v, s.kv, s.bpc)
	}
}

func init() {
	i32Sum = func(op bytecode.Op, x tentry, k int32, pc int) ti32 {
		if !i32Summed(x) {
			return nil
		}
		a := x.arith
		sub := a.op == bytecode.OpSub
		s := &i32SumSite{aop: a.op, apc: a.pc, op: op, k: k, kv: Int32(k), bpc: pc}
		l, r := a.x, a.y
		switch {
		case l.local && r.local:
			i, j := l.k, r.k
			if sub {
				return func(c *tctx) int32 {
					a, b := c.locals[i], c.locals[j]
					if n, ok := i32Exact(a.num - b.num); ok {
						return i32Do(op, n, k)
					}
					return i32SumSlow(c, s, a, b)
				}
			}
			return func(c *tctx) int32 {
				a, b := c.locals[i], c.locals[j]
				if n, ok := i32Exact(a.num + b.num); ok {
					return i32Do(op, n, k)
				}
				return i32SumSlow(c, s, a, b)
			}
		case l.local && r.number, l.number && r.local && !sub:
			// x + n, x - n as x + -n, which IEEE 754 makes the same, and n
			// + x, whose slow path takes them in their order.
			j, nv := l.k, r.n
			if l.number {
				j, nv, s.swap = r.k, l.n, true
			}
			n := nv.num
			if sub {
				n = -n
			}
			return func(c *tctx) int32 {
				a := c.locals[j]
				if m, ok := i32Exact(a.num + n); ok {
					return i32Do(op, m, k)
				}
				return i32SumSlow(c, s, a, nv)
			}
		case l.number && r.local:
			j, nv := r.k, l.n
			n := nv.num
			return func(c *tctx) int32 {
				b := c.locals[j]
				if m, ok := i32Exact(n - b.num); ok {
					return i32Do(op, m, k)
				}
				return i32SumSlow(c, s, nv, b)
			}
		}
		return i32SumOf(op, k, s, l, r)
	}
}

// i32SumOf is i32Sum's tree for a sum one of whose operands is an int32
// tree: with a local, in place; with a constant or another int32 tree, as
// integers, which the sum of an int32 and an integer below 2^52 in
// magnitude is exactly.
var i32SumOf func(op bytecode.Op, k int32, s *i32SumSite, l, r tentry) ti32

func init() {
	i32SumOf = func(op bytecode.Op, k int32, s *i32SumSite, l, r tentry) ti32 {
		sub := s.aop == bytecode.OpSub
		switch {
		case l.local:
			j := l.k
			q, uns := i32Of(r)
			if sub {
				return func(c *tctx) int32 {
					a := c.locals[j]
					b := q(c)
					if m, ok := i32Exact(a.num - float64(i32Wide(b, uns))); ok {
						return i32Do(op, m, k)
					}
					return i32SumSlow(c, s, a, i32Box(b, uns))
				}
			}
			return func(c *tctx) int32 {
				a := c.locals[j]
				b := q(c)
				if m, ok := i32Exact(a.num + float64(i32Wide(b, uns))); ok {
					return i32Do(op, m, k)
				}
				return i32SumSlow(c, s, a, i32Box(b, uns))
			}
		case r.local:
			j := r.k
			p, uns := i32Of(l)
			if sub {
				return func(c *tctx) int32 {
					a := p(c)
					b := c.locals[j]
					if m, ok := i32Exact(float64(i32Wide(a, uns)) - b.num); ok {
						return i32Do(op, m, k)
					}
					return i32SumSlow(c, s, i32Box(a, uns), b)
				}
			}
			return func(c *tctx) int32 {
				a := p(c)
				b := c.locals[j]
				if m, ok := i32Exact(float64(i32Wide(a, uns)) + b.num); ok {
					return i32Do(op, m, k)
				}
				return i32SumSlow(c, s, i32Box(a, uns), b)
			}
		case l.number || r.number:
			// An int32 tree and a constant, the constant negated for x - n.
			e, n, neg := l, r.n.num, sub
			if l.number {
				e, n, neg = r, l.n.num, false
			}
			p, uns := i32Of(e)
			if n != math.Trunc(n) || math.Abs(n) > 1<<52 {
				// A float64's sum, which rounds as the operator's does.
				switch {
				case neg:
					n = -n
				case sub:
					return func(c *tctx) int32 { return i32Do(op, toInt32(n-float64(i32Wide(p(c), uns))), k) }
				}
				return func(c *tctx) int32 { return i32Do(op, toInt32(float64(i32Wide(p(c), uns))+n), k) }
			}
			m := int64(n)
			switch {
			case neg:
				m = -m
			case sub:
				return func(c *tctx) int32 { return i32Do(op, int32(m-i32Wide(p(c), uns)), k) }
			}
			return func(c *tctx) int32 { return i32Do(op, int32(i32Wide(p(c), uns)+m), k) }
		}
		p, xu := i32Of(l)
		q, yu := i32Of(r)
		if sub {
			return func(c *tctx) int32 {
				a := i32Wide(p(c), xu)
				return i32Do(op, int32(a-i32Wide(q(c), yu)), k)
			}
		}
		return func(c *tctx) int32 {
			a := i32Wide(p(c), xu)
			return i32Do(op, int32(a+i32Wide(q(c), yu)), k)
		}
	}
}

func init() {
	bitwiseImmStoreAt = func(op bytecode.Op, k uint32, x tentry, n int32, pc int) tstmt {
		if x.local || !i32Sure(x) && !i32Summed(x) {
			return bitwiseImmStore(op, k, x, n, pc)
		}
		p := i32Imm(op, x, n, pc)
		if op == bytecode.OpUShr {
			return func(c *tctx) { l := &c.locals[k]; *l = Uint32(uint32(p(c))) }
		}
		return func(c *tctx) { l := &c.locals[k]; *l = Int32(p(c)) }
	}
}
