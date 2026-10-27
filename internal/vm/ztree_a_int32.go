package vm

import (
	"math"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
)

// Int32 trees: code compiled from C -- ((a ^ b) & c) >>> 8, (x >>> 0) <
// 262, (h | 0) == (g | 0) -- keeps its integers in int32s from one bitwise
// operator to the next, as a Value never does: every node gives a Value,
// whose number is a float64, so each bitwise operator converted its
// operands to int32 and its result back, though the operand was the int32
// the operator beneath it had made. Octane's zlib converted 870 million
// times a run so, a fifth of its time in toInt32.
//
// The value of a bitwise operator is a number that is an int32 -- a uint32
// for >>> -- wherever one of its operands is a number: a BigInt operand
// then throws, and anything else converts to a number. So x op k, for an
// integer constant k, is always one, and so is x op y where x or y is a
// number constant or itself such an operator (i32Sure). Such an entry has
// a tree of its value as the int32 it is (ti32, by i32Of), built from its
// arithOf, which says what it is of; the entry's own tree, a Value's, is
// built as before. A node that takes the entry as an operand calls its
// int32 tree:
//
//   - another bitwise operator with a constant (bitwiseImmTree), whose
//     own int32 tree (i32Imm) and Value are made of x's int32;
//   - a bitwise operator's int32 tree over two operands (i32Bin);
//   - a comparison (cmpJumpAt, eqJumpAt), of integers: of int64s for two
//     int32 trees, unsigned or not, and of float64s for anything else.
//
// An operand that is a local is read in place, and one that is no int32
// converted as toInt32 does, its test that the float64 is an int32 also
// the test that it is a number: what is no number has a NaN's bits. Where
// an operand is not a number, the node does what the operator does, with
// what it has evaluated, in the same order: bitwiseSlow and compareSlow,
// given the int32 operand as the number it is, convert, call valueOf and
// throw as they did. An entry whose operands may both be BigInts is built
// as before.
//
// ztree_a_int32elem.go reads and writes elements at int32 keys, and reads
// an element under an operator as an int32; ztree_a_int32sum.go makes a
// sum under an operator in place.
//
// The code is closures assigned in the inits of these files, which sort
// before every other tree file with an init: their closures are laid out
// after every other closure of the package (see ztree_zkself.go). Each
// builder has an init of its own, which keeps every function small enough
// for its closures to inline what they call.

// ti32 is a tree whose value is a number that is an int32, or a uint32 for
// >>>'s, as its 32 bits.
type ti32 func(c *tctx) int32

var (
	// bitwiseImmTree is x op k for an x that is no local: bitwiseImmNode's,
	// or, for an x that is an int32, a node of x's int32.
	bitwiseImmTree func(op bytecode.Op, x tentry, k int32, pc int) tval
	// cmpJumpAt is cmpJump, with nodes for an int32 operand.
	cmpJumpAt func(op bytecode.Op, x, y tentry, pc, taken, fall int) tnext
	// eqJumpAt is eqNumJump, which eqJump calls it for, with nodes for an
	// int32 operand.
	eqJumpAt func(op bytecode.Op, x, y tentry, pc, taken, fall int) tnext

	// i32Sure reports whether e is a bitwise operator's tree whose value is
	// always an int32 or a uint32, as the comment above says.
	i32Sure func(e tentry) bool
	// i32Of is the int32 tree of an entry i32Sure reports, and whether its
	// value is a uint32.
	i32Of func(e tentry) (ti32, bool)
	// i32Slow is op of a and b where one is not an int32, for a node whose
	// value is an int32: the operator of their ToInt32s, or bitwiseSlow's
	// number where one is no number, as its 32 bits.
	i32Slow func(c *tctx, op bytecode.Op, a, b Value, pc int) int32

	// i32Imm is x op k as an int32 tree, i32Bin x op y, and i32Value an
	// int32 tree as a tree of its Value.
	i32Imm   func(op bytecode.Op, x tentry, k int32, pc int) ti32
	i32Bin   func(op bytecode.Op, x, y tentry, pc int) ti32
	i32Value func(p ti32, uns bool) tval
)

// i32Box is the number i is, a uint32 where uns is set.
func i32Box(i int32, uns bool) Value {
	if uns {
		return Uint32(uint32(i))
	}
	return Int32(i)
}

// i32Wide is the integer i is, a uint32 where uns is set.
func i32Wide(i int32, uns bool) int64 {
	if uns {
		return int64(uint32(i))
	}
	return int64(i)
}

// i32Do is the bitwise operator op of a and b, ToInt32's bits of each.
func i32Do(op bytecode.Op, a, b int32) int32 {
	if op == bytecode.OpBitOr {
		// x | 0, every int32 of code compiled from C, tested first.
		return a | b
	}
	switch op {
	case bytecode.OpBitAnd:
		return a & b
	case bytecode.OpBitXor:
		return a ^ b
	case bytecode.OpShl:
		return a << (uint32(b) & 31)
	case bytecode.OpShr:
		return a >> (uint32(b) & 31)
	}
	return int32(uint32(a) >> (uint32(b) & 31))
}

// i32Mask is what an int32 tree's 32 bits, sign-extended to an int64, are
// masked with to give its value: all of them for an int32, the low 32 for
// a uint32.
func i32Mask(uns bool) int64 {
	if uns {
		return 1<<32 - 1
	}
	return -1
}

// f64Rel is the relational operator op of a and b, false where either is
// NaN, as the operator's is.
func f64Rel(op bytecode.Op, a, b float64) bool {
	switch op {
	case bytecode.OpLt:
		return a < b
	case bytecode.OpLe:
		return a <= b
	case bytecode.OpGt:
		return a > b
	}
	return a >= b
}

func init() {
	i32Sure = func(e tentry) bool {
		a := e.arith
		if a == nil || e.v == nil || e.slot >= 0 || !isBitwise(a.op) {
			return false
		}
		return a.imm || a.x.number || a.y.number || i32Sure(a.x) || i32Sure(a.y)
	}

	i32Slow = func(c *tctx, op bytecode.Op, a, b Value, pc int) int32 {
		if a.IsNumber() && b.IsNumber() {
			// Numbers that are no int32s, which the nodes leave here
			// rather than asking twice what they are.
			return i32Do(op, toInt32(a.num), toInt32(b.num))
		}
		v := c.bitwiseSlow(op, a, b, pc)
		if op == bytecode.OpUShr {
			return int32(uint32(v.num))
		}
		return int32(v.num)
	}

}

func init() {
	// i32Imm is x op k as an int32 tree.
	i32Imm = func(op bytecode.Op, x tentry, k int32, pc int) ti32 {
		n, kv := uint32(k)&31, Int32(k)
		if i32Sure(x) {
			p, _ := i32Of(x)
			switch op {
			case bytecode.OpBitAnd:
				return func(c *tctx) int32 { return p(c) & k }
			case bytecode.OpBitOr:
				return func(c *tctx) int32 { return p(c) | k }
			case bytecode.OpBitXor:
				return func(c *tctx) int32 { return p(c) ^ k }
			case bytecode.OpShl:
				return func(c *tctx) int32 { return p(c) << (n & 31) }
			case bytecode.OpShr:
				return func(c *tctx) int32 { return p(c) >> (n & 31) }
			}
			return func(c *tctx) int32 { return int32(uint32(p(c)) >> (n & 31)) }
		}
		if x.local {
			j := x.k
			switch op {
			case bytecode.OpBitAnd:
				return func(c *tctx) int32 {
					if a := c.locals[j]; float64(int32(a.num)) == a.num {
						return int32(a.num) & k
					} else {
						return i32Slow(c, op, a, kv, pc)
					}
				}
			case bytecode.OpBitOr:
				return func(c *tctx) int32 {
					if a := c.locals[j]; float64(int32(a.num)) == a.num {
						return int32(a.num) | k
					} else {
						return i32Slow(c, op, a, kv, pc)
					}
				}
			case bytecode.OpBitXor:
				return func(c *tctx) int32 {
					if a := c.locals[j]; float64(int32(a.num)) == a.num {
						return int32(a.num) ^ k
					} else {
						return i32Slow(c, op, a, kv, pc)
					}
				}
			case bytecode.OpShl:
				return func(c *tctx) int32 {
					if a := c.locals[j]; float64(int32(a.num)) == a.num {
						return int32(a.num) << (n & 31)
					} else {
						return i32Slow(c, op, a, kv, pc)
					}
				}
			case bytecode.OpShr:
				return func(c *tctx) int32 {
					if a := c.locals[j]; float64(int32(a.num)) == a.num {
						return int32(a.num) >> (n & 31)
					} else {
						return i32Slow(c, op, a, kv, pc)
					}
				}
			}
			return func(c *tctx) int32 {
				if a := c.locals[j]; float64(int32(a.num)) == a.num {
					return int32(uint32(int32(a.num)) >> (n & 31))
				} else {
					return i32Slow(c, op, a, kv, pc)
				}
			}
		}
		if f := i32Sum(op, x, k, pc); f != nil {
			return f
		}
		t := x.tree()
		switch op {
		case bytecode.OpBitAnd:
			return func(c *tctx) int32 {
				if a := t(c); float64(int32(a.num)) == a.num {
					return int32(a.num) & k
				} else {
					return i32Slow(c, op, a, kv, pc)
				}
			}
		case bytecode.OpBitOr:
			return func(c *tctx) int32 {
				if a := t(c); float64(int32(a.num)) == a.num {
					return int32(a.num) | k
				} else {
					return i32Slow(c, op, a, kv, pc)
				}
			}
		case bytecode.OpBitXor:
			return func(c *tctx) int32 {
				if a := t(c); float64(int32(a.num)) == a.num {
					return int32(a.num) ^ k
				} else {
					return i32Slow(c, op, a, kv, pc)
				}
			}
		case bytecode.OpShl:
			return func(c *tctx) int32 {
				if a := t(c); float64(int32(a.num)) == a.num {
					return int32(a.num) << (n & 31)
				} else {
					return i32Slow(c, op, a, kv, pc)
				}
			}
		case bytecode.OpShr:
			return func(c *tctx) int32 {
				if a := t(c); float64(int32(a.num)) == a.num {
					return int32(a.num) >> (n & 31)
				} else {
					return i32Slow(c, op, a, kv, pc)
				}
			}
		}
		return func(c *tctx) int32 {
			if a := t(c); float64(int32(a.num)) == a.num {
				return int32(uint32(int32(a.num)) >> (n & 31))
			} else {
				return i32Slow(c, op, a, kv, pc)
			}
		}
	}

}

func init() {
	// i32Bin is x op y as an int32 tree, where one of them is an int32 or a
	// number constant. Each is evaluated before the operator converts
	// either, x first.
	i32Bin = func(op bytecode.Op, x, y tentry, pc int) ti32 {
		xs, ys := i32Sure(x), i32Sure(y)
		switch {
		case xs && ys:
			p, _ := i32Of(x)
			q, _ := i32Of(y)
			return func(c *tctx) int32 {
				a := p(c)
				return i32Do(op, a, q(c))
			}
		case xs && y.local:
			p, xu := i32Of(x)
			j := y.k
			return func(c *tctx) int32 {
				a := p(c)
				if b := c.locals[j]; float64(int32(b.num)) == b.num {
					return i32Do(op, a, int32(b.num))
				} else {
					return i32Slow(c, op, i32Box(a, xu), b, pc)
				}
			}
		case xs:
			p, xu := i32Of(x)
			t := y.tree()
			return func(c *tctx) int32 {
				a := p(c)
				if b := t(c); float64(int32(b.num)) == b.num {
					return i32Do(op, a, int32(b.num))
				} else {
					return i32Slow(c, op, i32Box(a, xu), b, pc)
				}
			}
		case ys && x.local:
			q, yu := i32Of(y)
			j := x.k
			return func(c *tctx) int32 {
				a := c.locals[j]
				b := q(c)
				if float64(int32(a.num)) == a.num {
					return i32Do(op, int32(a.num), b)
				}
				return i32Slow(c, op, a, i32Box(b, yu), pc)
			}
		case ys:
			q, yu := i32Of(y)
			t := x.tree()
			return func(c *tctx) int32 {
				a := t(c)
				b := q(c)
				if float64(int32(a.num)) == a.num {
					return i32Do(op, int32(a.num), b)
				}
				return i32Slow(c, op, a, i32Box(b, yu), pc)
			}
		}
		// A number constant and a tree.
		t, u := x.tree(), y.tree()
		return func(c *tctx) int32 {
			a := t(c)
			if b := u(c); a.IsNumber() && b.IsNumber() {
				return i32Do(op, toInt32(a.num), toInt32(b.num))
			} else {
				return i32Slow(c, op, a, b, pc)
			}
		}
	}

}

func init() {
	i32Of = func(e tentry) (ti32, bool) {
		a := e.arith
		uns := a.op == bytecode.OpUShr
		if g := a.y.arith; a.imm && g != nil && g.op == bytecode.OpGetIndex {
			// An element read under the operator, which getIndexAt
			// made one entry: its object and key are kept in y, which
			// an operator with a constant has no use for.
			return i32Elem(g.x, g.y, &i32Site{op: a.op, k: a.k, kv: Int32(a.k), gpc: g.pc, bpc: a.pc}), uns
		}
		if a.imm {
			return i32Imm(a.op, a.x, a.k, a.pc), uns
		}
		return i32Bin(a.op, a.x, a.y, a.pc), uns
	}

	// i32Value is an int32 tree as a tree of its Value.
	i32Value = func(p ti32, uns bool) tval {
		if uns {
			return func(c *tctx) Value { return Uint32(uint32(p(c))) }
		}
		return func(c *tctx) Value { return Int32(p(c)) }
	}

	bitwiseImmTree = func(op bytecode.Op, x tentry, k int32, pc int) tval {
		if !i32Sure(x) {
			if i32Summed(x) {
				return i32Value(i32Imm(op, x, k, pc), op == bytecode.OpUShr)
			}
			return bitwiseImmNode(op, x.tree(), k, pc)
		}
		p, _ := i32Of(x)
		n := uint32(k) & 31
		switch op {
		case bytecode.OpBitAnd:
			return func(c *tctx) Value { return Int32(p(c) & k) }
		case bytecode.OpBitOr:
			return func(c *tctx) Value { return Int32(p(c) | k) }
		case bytecode.OpBitXor:
			return func(c *tctx) Value { return Int32(p(c) ^ k) }
		case bytecode.OpShl:
			return func(c *tctx) Value { return Int32(p(c) << (n & 31)) }
		case bytecode.OpShr:
			return func(c *tctx) Value { return Int32(p(c) >> (n & 31)) }
		}
		return func(c *tctx) Value { return Uint32(uint32(p(c)) >> (n & 31)) }
	}

}

func init() {
	cmpJumpAt = func(op bytecode.Op, x, y tentry, pc, taken, fall int) tnext {
		xs, ys := i32Sure(x), i32Sure(y)
		switch op {
		case bytecode.OpLt, bytecode.OpLe, bytecode.OpGt, bytecode.OpGe:
		default:
			xs, ys = false, false
		}
		switch {
		case xs && ys:
			// Two int32 trees as int64s: a uint32's bits masked, an
			// int32's sign-extended.
			p, xu := i32Of(x)
			q, yu := i32Of(y)
			xm, ym := i32Mask(xu), i32Mask(yu)
			switch op {
			case bytecode.OpLt:
				return func(c *tctx) int {
					if a := int64(p(c)) & xm; a < int64(q(c))&ym {
						return fall
					}
					return taken
				}
			case bytecode.OpLe:
				return func(c *tctx) int {
					if a := int64(p(c)) & xm; a <= int64(q(c))&ym {
						return fall
					}
					return taken
				}
			case bytecode.OpGt:
				return func(c *tctx) int {
					if a := int64(p(c)) & xm; a > int64(q(c))&ym {
						return fall
					}
					return taken
				}
			}
			return func(c *tctx) int {
				if a := int64(p(c)) & xm; a >= int64(q(c))&ym {
					return fall
				}
				return taken
			}
		case xs && y.number:
			// An integer against a number constant, as against the
			// integer bound that gives the same answer: x < n where x <
			// ceil(n), x <= n where x <= floor(n). A NaN gives false, the
			// int32 tree evaluated all the same.
			p, xu := i32Of(x)
			m, n := i32Mask(xu), y.n.num
			if n != n {
				return func(c *tctx) int { p(c); return taken }
			}
			n = max(-1<<40, min(n, 1<<40))
			lo, hi := int64(math.Ceil(n)), int64(math.Floor(n))
			switch op {
			case bytecode.OpLt:
				return func(c *tctx) int {
					if int64(p(c))&m < lo {
						return fall
					}
					return taken
				}
			case bytecode.OpLe:
				return func(c *tctx) int {
					if int64(p(c))&m <= hi {
						return fall
					}
					return taken
				}
			case bytecode.OpGt:
				return func(c *tctx) int {
					if int64(p(c))&m > hi {
						return fall
					}
					return taken
				}
			}
			return func(c *tctx) int {
				if int64(p(c))&m >= lo {
					return fall
				}
				return taken
			}
		case xs:
			p, xu := i32Of(x)
			t := y.tree()
			return func(c *tctx) int {
				a := p(c)
				b := t(c)
				if b.IsNumber() {
					if f64Rel(op, float64(i32Wide(a, xu)), b.num) {
						return fall
					}
					return taken
				}
				if c.compareSlow(op, i32Box(a, xu), b, pc) {
					return fall
				}
				return taken
			}
		case ys:
			q, yu := i32Of(y)
			t := x.tree()
			return func(c *tctx) int {
				a := t(c)
				b := q(c)
				if a.IsNumber() {
					if f64Rel(op, a.num, float64(i32Wide(b, yu))) {
						return fall
					}
					return taken
				}
				if c.compareSlow(op, a, i32Box(b, yu), pc) {
					return fall
				}
				return taken
			}
		}
		return cmpJump(op, x, y, pc, taken, fall)
	}

}

func init() {
	eqJumpAt = func(op bytecode.Op, x, y tentry, pc, taken, fall int) tnext {
		xs, ys := i32Sure(x), i32Sure(y)
		if !xs && !ys {
			return eqNumJump(op, x, y, pc, taken, fall)
		}
		if x.number && !y.number {
			// Equality is symmetric, and a constant has nothing to
			// evaluate.
			x, y, xs, ys = y, x, ys, xs
		}
		yes, no := fall, taken
		if op == bytecode.OpNe || op == bytecode.OpStrictNe {
			yes, no = taken, fall
		}
		strict := op == bytecode.OpStrictEq || op == bytecode.OpStrictNe
		switch {
		case xs && ys:
			p, xu := i32Of(x)
			q, yu := i32Of(y)
			return func(c *tctx) int {
				if a := i32Wide(p(c), xu); a == i32Wide(q(c), yu) {
					return yes
				}
				return no
			}
		case xs && y.number:
			p, xu := i32Of(x)
			n := y.n.num
			return func(c *tctx) int {
				if float64(i32Wide(p(c), xu)) == n {
					return yes
				}
				return no
			}
		}
		// An int32 and a tree, in their order: a number is equal
		// to the int32's number, anything else strictly never and
		// loosely as looseEquals says.
		var p ti32
		var t tval
		var uns bool
		if xs {
			p, uns = i32Of(x)
			t = y.tree()
		} else {
			p, uns = i32Of(y)
			t = x.tree()
		}
		return func(c *tctx) int {
			var a int32
			var b Value
			if xs {
				a = p(c)
				b = t(c)
			} else {
				b = t(c)
				a = p(c)
			}
			if b.IsNumber() {
				if float64(i32Wide(a, uns)) == b.num {
					return yes
				}
				return no
			}
			if strict {
				return no
			}
			c.at(pc)
			eq, err := c.r.looseEquals(i32Box(a, uns), b)
			if err != nil {
				c.throw(err)
			}
			if eq {
				return yes
			}
			return no
		}
	}
}
