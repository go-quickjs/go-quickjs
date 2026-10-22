package vm

import "github.com/go-quickjs/go-quickjs/internal/bytecode"

// Equality branches over numbers in the tree tier: n === 0, i !== j, k == 1,
// where a local is compared with a number constant or with another local.
// eqJump had called each operand's tree and StrictEquals, which asks both
// kinds first, for every comparison not of a literal -- the test that ends
// most recursions, n === 0, and most loops' searches. Here a local is read
// in place, and where both sides are numbers the answer is the float
// comparison, which gives NaN's and -0's answers as === and == do; a
// strict comparison of a number constant with anything else is false
// without more, and anything else goes the way eqJump went.
//
// The code is closures assigned in this file's init, the package's first
// init with closures, whose closures are laid out after every other
// closure (see ztree_zkself.go): code added here moves nothing the tree
// tier or the interpreter runs.

// eqNumJump is eqJump's end for op, an equality, of x and y, for the jump
// at pc to taken or fall, where one side is a number constant or both are
// locals; nil for anything else, which eqJump builds.
var eqNumJump func(op bytecode.Op, x, y tentry, pc, taken, fall int) tnext

func init() {
	eqNumJump = func(op bytecode.Op, x, y tentry, pc, taken, fall int) tnext {
		if x.number && !y.number {
			// Equality is symmetric, and a constant has nothing to evaluate.
			x, y = y, x
		}
		// yes is where a comparison that holds goes, no where one that
		// does not.
		yes, no := fall, taken
		if op == bytecode.OpNe || op == bytecode.OpStrictNe {
			yes, no = taken, fall
		}
		strict := op == bytecode.OpStrictEq || op == bytecode.OpStrictNe
		switch {
		case y.number && !x.number && x.local:
			k, n := x.k, y.n.num
			if strict {
				return func(c *tctx) int {
					if l := c.locals[k]; l.IsNumber() && l.num == n {
						return yes
					}
					return no
				}
			}
			nv := y.n
			return func(c *tctx) int {
				l := c.locals[k]
				if l.IsNumber() {
					if l.num == n {
						return yes
					}
					return no
				}
				c.at(pc)
				eq, err := c.r.looseEquals(l, nv)
				if err != nil {
					c.throw(err)
				}
				if eq {
					return yes
				}
				return no
			}
		case y.number && !x.number:
			v, n := x.tree(), y.n.num
			if strict {
				return func(c *tctx) int {
					if l := v(c); l.IsNumber() && l.num == n {
						return yes
					}
					return no
				}
			}
			nv := y.n
			return func(c *tctx) int {
				l := v(c)
				if l.IsNumber() {
					if l.num == n {
						return yes
					}
					return no
				}
				c.at(pc)
				eq, err := c.r.looseEquals(l, nv)
				if err != nil {
					c.throw(err)
				}
				if eq {
					return yes
				}
				return no
			}
		case x.local && y.local && strict:
			a, b := x.k, y.k
			return func(c *tctx) int {
				l := c.locals[a]
				r := c.locals[b]
				if l.IsNumber() && r.IsNumber() {
					if l.num == r.num {
						return yes
					}
					return no
				}
				if l.StrictEquals(r) {
					return yes
				}
				return no
			}
		}
		return nil
	}
}
