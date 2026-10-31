package vm

import "github.com/go-quickjs/go-quickjs/internal/bytecode"

// An equality as a value -- return p == null, return this.direction !==
// null, (this.state & STATE_HELD) != 0 || ..., a predicate's body -- where
// one side is a literal, a string constant or a number. op's comparison
// had pushed compareNode's test under a node making the boolean: four
// calls, the operands' two among them, and for == null, whose operands
// are not two numbers, a call of looseEquals besides.
//
// Against undefined, null, a boolean or a string constant, the value is
// eqLitJump's test, which reads a local, an upvalue or a property of this
// or of a local in place and calls nothing: its end, made to give 1 where
// the comparison holds and 0 where it does not, and the boolean of that.
// Against a number, a number on the other side is compared as a float,
// as compareNode compares two; for ===, anything else is unequal, and for
// ==, it is compared by looseEquals, as compareNode does.
//
// Its functions are variables, each assigned a closure in init, none of
// which captures a variable of the init's; the file sorts before the tree
// tier's other files with an init, so its closures are laid out after
// every closure there was (see ztree_a_aheap.go).

// eqValue is op, an equality, of x and y at pc as a value, where one side
// is a literal, a string constant or a number and the other is not; nil
// for anything else, which compareNode's test builds.
var eqValue func(op bytecode.Op, x, y tentry, pc int) tval

func init() {
	eqValue = func(op bytecode.Op, x, y tentry, pc int) tval {
		switch op {
		case bytecode.OpEq, bytecode.OpNe, bytecode.OpStrictEq, bytecode.OpStrictNe:
		default:
			return nil
		}
		if t := eqLitJump(op, x, y, 0, 1); t != nil {
			return func(c *tctx) Value { return Bool(t(c) != 0) }
		}
		if x.number && !y.number {
			x, y = y, x
		}
		if !y.number || x.number || x.literal || x.rare != nil && x.rare.str {
			return nil
		}
		k, v := y.n.num, x.tree()
		want := op == bytecode.OpEq || op == bytecode.OpStrictEq
		if op == bytecode.OpStrictEq || op == bytecode.OpStrictNe {
			return func(c *tctx) Value {
				if a := v(c); a.IsNumber() {
					return Bool((a.num == k) == want)
				}
				return Bool(!want)
			}
		}
		n := y.n
		return func(c *tctx) Value {
			a := v(c)
			if a.IsNumber() {
				return Bool((a.num == k) == want)
			}
			c.at(pc)
			eq, err := c.r.looseEquals(a, n)
			if err != nil {
				c.throw(err)
			}
			return Bool(eq == want)
		}
	}
}
