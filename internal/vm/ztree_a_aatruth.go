package vm

// A forward jump on whether a local or an upvalue is truthy -- if (top),
// if (!node), while (cursor) -- as one node of the tree tier, the value
// read in place. The jump had called the value's node, a local's read a
// call of its own, and then truthy, whose answer for anything but a
// boolean is Value.Truthy's switch, a call too: an object, which every
// value tested so mostly is, and null and undefined, which end the tests
// that fail, are answered here, as Truthy answers them -- every object is
// truthy but document.all (Annex B's [[IsHTMLDDA]]) -- and the rest by
// truthy.
//
// Its functions are variables, each assigned a closure in init, none of
// which captures a variable of the init's; the file sorts before the tree
// tier's other files with an init, so its closures are laid out after
// every closure there was (see ztree_a_aheap.go).

// truthJump is the end of a block jumping forward to taken, or falling
// to fall, on x, for jump_if_true where onTrue is set and jump_if_false
// where it is not, where x is a local or an upvalue read in place; nil
// for anything else, which buildBlock builds.
//
// It is truthJumpHook, which asks for a switch's lookup first (see
// zzz_switch.go), and then truthJumpLocal, this file's.
var truthJump = truthJumpHook[struct{}]

// truthJumpLocal is truthJump's end of a local or an upvalue.
var truthJumpLocal func(x tentry, onTrue bool, taken, fall int) tnext

func init() {
	truthJumpLocal = func(x tentry, onTrue bool, taken, fall int) tnext {
		if x.v != nil || x.slot >= 0 {
			return nil
		}
		// yes is where a truthy value goes, no where a falsy one does.
		yes, no := fall, taken
		if onTrue {
			yes, no = taken, fall
		}
		k := x.k
		switch {
		case x.local:
			return func(c *tctx) int {
				l := c.locals[k]
				if l.IsObject() {
					if l.object().flags&objHTMLDDA == 0 {
						return yes
					}
					return no
				}
				if l.IsNullish() || !truthy(l) {
					return no
				}
				return yes
			}
		case x.upvalue:
			return func(c *tctx) int {
				l := c.cl.upvalues[k].get()
				if l.IsObject() {
					if l.object().flags&objHTMLDDA == 0 {
						return yes
					}
					return no
				}
				if l.IsNullish() || !truthy(l) {
					return no
				}
				return yes
			}
		}
		return nil
	}
}
