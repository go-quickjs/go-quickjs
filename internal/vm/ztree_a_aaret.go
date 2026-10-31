package vm

// A return of nothing, of a local, of this or of a constant -- the end of
// every method that returns no value, return top, return this, return null
// -- as one end of the tree tier, the value read in place. buildBlock's
// return called the value's node, a local's read a call of its own and
// return_undef's a call to make undefined, before the checks of the
// return: for a frame with a derived constructor's this binding, the
// result derivedResult gives (an error where a derived constructor's this
// is still unbound). They are made here as they were. A return with
// values left on the stack, which closes iterators, and a return of a
// function with a call in tail position (restartReturn) are buildBlock's.
//
// Its functions are variables, each assigned a closure in init, none of
// which captures a variable of the init's; the file sorts before the tree
// tier's other files with an init, so its closures are laid out after
// every closure there was (see ztree_a_aheap.go).

// returnAt is the end of a block returning e, or undefined where undef is
// set, from a stack d deep, at pc; nil for anything but a return from an
// empty stack of nothing, a local, this or a constant, which buildBlock
// builds.
var returnAt func(e tentry, undef bool, d, pc int) tnext

func init() {
	returnAt = func(e tentry, undef bool, d, pc int) tnext {
		if d != 0 {
			return nil
		}
		if !undef && (e.v != nil || e.slot >= 0) {
			return nil
		}
		switch {
		case undef || e.literal || e.number:
			n := Undefined
			if !undef {
				n = e.n
			}
			return func(c *tctx) int {
				res := n
				if c.f.thisRef != nil {
					c.at(pc)
					var err error
					if res, err = c.r.derivedResult(c.f, c.cl, res); err != nil {
						c.throw(err)
					}
				}
				c.ret = res
				return -1
			}
		case e.local:
			k := e.k
			return func(c *tctx) int {
				res := c.locals[k]
				if c.f.thisRef != nil {
					c.at(pc)
					var err error
					if res, err = c.r.derivedResult(c.f, c.cl, res); err != nil {
						c.throw(err)
					}
				}
				c.ret = res
				return -1
			}
		case e.this:
			return func(c *tctx) int {
				res := c.f.this
				if c.f.thisRef != nil {
					c.at(pc)
					var err error
					if res, err = c.r.derivedResult(c.f, c.cl, res); err != nil {
						c.throw(err)
					}
				}
				c.ret = res
				return -1
			}
		}
		return nil
	}
}
