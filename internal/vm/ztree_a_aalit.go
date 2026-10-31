package vm

import (
	"math"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
)

// Equality branches of a value against undefined, null, true or false in
// the tree tier -- x === null, p !== undefined, x == null, done === false
// -- where the value is a local or an upvalue, read in place, or a tree.
// eqJump had called the operand's tree, a local's read a call of its own,
// and then its end, a closure that picked the block, for each: three calls
// where the test is one comparison. A list's walk is made of them, while
// (node !== null) and if (node.next === null), as List's isShorterThan and
// length are, and so is each search through a chain.
//
// The answers are eqJump's: === against undefined or null asks the other's
// kind, against a boolean its bits, which a boolean has one spelling of;
// == against undefined or null asks whether it is one of them, which
// nothing else equals loosely but document.all (Annex B's [[IsHTMLDDA]]).
// == against a boolean converts, and is left to eqJump.
//
// So is x === 'forward', x !== 'BB_DEAD', against a string constant: the
// constant's node, StrictEquals asking both kinds and then the strings,
// and the end had been three calls more. Here a string equals the
// constant where it is the same string -- a constant stored and compared
// again mostly is -- or one of the same code units, and nothing else
// does: DeltaBlue's constraints test their direction so at each step.
//
// Its functions are variables, each assigned a closure in init, none of
// which captures a variable of the init's; the file sorts before the tree
// tier's other files with an init, so its closures are laid out after
// every closure there was (see ztree_a_aheap.go).

// eqLitJump is eqJump's end for op, an equality, of x and y, for the jump
// at pc to taken or fall, where one side is a literal -- undefined, null,
// true or false -- and the other is not; nil for anything else, which
// eqJump builds.
var eqLitJump func(op bytecode.Op, x, y tentry, taken, fall int) tnext

// strConstRare is the rare of a string constant's push_const entry, of
// constant k, which every such entry shares: an equality with it reads
// the constant in place (eqLitJump).
var strConstRare = &tentryRare{str: true}

// litTest says which test eqLitJump's node makes of the value: whether it
// is null, undefined, either of them or document.all, or a boolean of the
// bits given.
type litTest uint8

const (
	litNull litTest = iota
	litUndefined
	litNullish
	litBits
)

func init() {
	eqLitJump = func(op bytecode.Op, x, y tentry, taken, fall int) tnext {
		strict := op == bytecode.OpStrictEq || op == bytecode.OpStrictNe
		if x.rare == strConstRare && x.slot < 0 && y.rare != strConstRare {
			x, y = y, x
		}
		if strict && y.rare == strConstRare && y.slot < 0 && x.rare != strConstRare && !x.literal {
			// A string constant, which === finds equal to a string of its
			// code units and to nothing else: the same string, as a
			// constant stored and compared mostly is, without comparing
			// them.
			yes, no := fall, taken
			if op == bytecode.OpStrictNe {
				yes, no = taken, fall
			}
			j := y.k
			if x.local && x.v == nil && x.slot < 0 {
				k := x.k
				return func(c *tctx) int {
					if l := c.locals[k]; l.IsString() {
						if s, t := l.String(), c.consts[j].String(); s == t || s.Equals(t) {
							return yes
						}
					}
					return no
				}
			}
			if p := propIn(x); p != nil {
				return eqPropJump(p, 0, true, j, yes, no)
			}
			v := x.tree()
			return func(c *tctx) int {
				if l := v(c); l.IsString() {
					if s, t := l.String(), c.consts[j].String(); s == t || s.Equals(t) {
						return yes
					}
				}
				return no
			}
		}
		if x.literal && !y.literal {
			x, y = y, x
		}
		if !y.literal || x.literal {
			return nil
		}
		var test litTest
		lit := y.n
		switch {
		case strict && lit.IsNull():
			test = litNull
		case strict && lit.IsUndefined():
			test = litUndefined
		case strict:
			test = litBits
		case lit.IsNullish():
			test = litNullish
		default:
			return nil
		}
		// yes is where a comparison that holds goes, no where one that
		// does not.
		yes, no := fall, taken
		if op == bytecode.OpNe || op == bytecode.OpStrictNe {
			yes, no = taken, fall
		}
		bits := math.Float64bits(lit.num)
		switch {
		case x.local && x.v == nil && x.slot < 0:
			k := x.k
			switch test {
			case litNull:
				return func(c *tctx) int {
					if c.locals[k].IsNull() {
						return yes
					}
					return no
				}
			case litUndefined:
				return func(c *tctx) int {
					if c.locals[k].IsUndefined() {
						return yes
					}
					return no
				}
			case litBits:
				return func(c *tctx) int {
					if math.Float64bits(c.locals[k].num) == bits {
						return yes
					}
					return no
				}
			}
			return func(c *tctx) int {
				if l := c.locals[k]; l.IsNullish() || l.IsObject() && l.Object().flags&objHTMLDDA != 0 {
					return yes
				}
				return no
			}
		case x.upvalue && x.v == nil && x.slot < 0:
			k := x.k
			switch test {
			case litNull:
				return func(c *tctx) int {
					if c.cl.upvalues[k].get().IsNull() {
						return yes
					}
					return no
				}
			case litUndefined:
				return func(c *tctx) int {
					if c.cl.upvalues[k].get().IsUndefined() {
						return yes
					}
					return no
				}
			}
		}
		if p := propIn(x); p != nil && test != litBits {
			return eqPropJump(p, test, false, 0, yes, no)
		}
		v := x.tree()
		switch test {
		case litNull:
			return func(c *tctx) int {
				if v(c).IsNull() {
					return yes
				}
				return no
			}
		case litUndefined:
			return func(c *tctx) int {
				if v(c).IsUndefined() {
					return yes
				}
				return no
			}
		case litBits:
			return func(c *tctx) int {
				if math.Float64bits(v(c).num) == bits {
					return yes
				}
				return no
			}
		}
		return func(c *tctx) int {
			if l := v(c); l.IsNullish() || l.IsObject() && l.Object().flags&objHTMLDDA != 0 {
				return yes
			}
			return no
		}
	}
}
