package vm

import "github.com/go-quickjs/go-quickjs/internal/bytecode"

// A strict equality branch of a local or a tree with a string constant --
// k === 'section', type !== 'label' -- as one node of the tree tier that
// compares the strings in place. eqLitJump's node had asked the value's
// kind and the constant's again, through Value.String, and then called
// String.Equals, which asks once more whether they are the same string
// before it compares their lengths: a call for every string that is not
// the constant, which is most of those such a test meets, a parser's token
// against each keyword. Here the same string, a constant stored and
// compared again, is equal, as there; a string of another length is not,
// without a call; a flat one of the same length is equal where its WTF-8
// is the constant's, which spells each string one way; and only a rope,
// which has to be flattened, is left to Equals.
//
// A property read of this or of a local is eqLitJump's still (see
// ztree_a_aaeqprop.go), and so is == against a string, which converts.
//
// It is generic, instantiated with struct{} alone, and asked by
// typeofJump, eqJumpAt's first try (see zzz_typeof.go), so that every
// function and closure there was stays at its address.

// strEqJump is eqJump's end for op, === or !==, of x and y, where one is
// a string constant and the other a local or a tree, for the jump to
// taken, or falling to fall; nil for anything else, which eqJump builds.
func strEqJump[_ any](op bytecode.Op, x, y tentry, taken, fall int) tnext {
	if op != bytecode.OpStrictEq && op != bytecode.OpStrictNe {
		return nil
	}
	if x.rare == strConstRare && x.slot < 0 {
		x, y = y, x
	}
	if y.rare != strConstRare || y.slot >= 0 || x.rare == strConstRare && x.slot < 0 || x.literal {
		return nil
	}
	if propIn(x) != nil {
		return nil
	}
	// yes is where a comparison that holds goes, no where one that
	// does not.
	yes, no := fall, taken
	if op == bytecode.OpStrictNe {
		yes, no = taken, fall
	}
	j := y.k
	if x.local && x.v == nil && x.slot < 0 {
		k := x.k
		return func(c *tctx) int {
			if l := c.locals[k]; l.IsString() {
				// The constant, flat, as every constant is.
				s, t := (*String)(l.ref), (*String)(c.consts[j].ref)
				if s == t || s.length == t.length && (s.left == nil && s.s == t.s || s.left != nil && s.Equals(t)) {
					return yes
				}
			}
			return no
		}
	}
	v := x.tree()
	return func(c *tctx) int {
		if l := v(c); l.IsString() {
			s, t := (*String)(l.ref), (*String)(c.consts[j].ref)
			if s == t || s.length == t.length && (s.left == nil && s.s == t.s || s.left != nil && s.Equals(t)) {
				return yes
			}
		}
		return no
	}
}
