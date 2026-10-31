package vm

// An equality branch of a property of this or of a local, read in place,
// with null, undefined or a string constant -- this.next === null,
// this.storage !== null, this.direction === 'forward' -- as one node of the
// tree tier: eqLitJump's node for a tree, with getPropOperand's node read
// in it. The property is read as getPropOperand's node reads it -- the
// site's own entry, its scan, getPropSlow for anything else -- and tested
// as eqLitJump tests a value.
//
// Its functions are variables, each assigned a closure in init, none of
// which captures a variable of the init's; the file sorts before the tree
// tier's other files with an init, so its closures are laid out after
// every closure there was (see ztree_a_aheap.go).

// eqPropJump is eqLitJump's end for p, a property read of this or of a
// local, tested as test says -- litNull, litUndefined or litNullish, or
// for the string constant j where str is set -- going to yes where the
// test holds and to no where it does not; nil for any other test.
var eqPropJump func(p *propOf, test litTest, str bool, j uint32, yes, no int) tnext

func init() {
	eqPropJump = func(p *propOf, test litTest, str bool, j uint32, yes, no int) tnext {
		name, site, pc, this, k := p.name, p.site, p.pc, p.obj.this, p.obj.k
		switch {
		case str:
			return func(c *tctx) int {
				o := c.f.this
				if !this {
					o = c.locals[k]
				}
				var l Value
				if o.IsObject() {
					ob := o.object()
					if v, ok := c.ic[site].own(ob); ok {
						l = v
					} else if v, ok := ownScan(&c.ic[site], ob, c.names[name]); ok {
						l = v
					} else {
						l = c.getPropSlow(o, name, site, pc)
					}
				} else {
					l = c.getPropSlow(o, name, site, pc)
				}
				if l.IsString() {
					if s, t := l.String(), c.consts[j].String(); s == t || s.Equals(t) {
						return yes
					}
				}
				return no
			}
		case test == litNull:
			return func(c *tctx) int {
				o := c.f.this
				if !this {
					o = c.locals[k]
				}
				var l Value
				if o.IsObject() {
					ob := o.object()
					if v, ok := c.ic[site].own(ob); ok {
						l = v
					} else if v, ok := ownScan(&c.ic[site], ob, c.names[name]); ok {
						l = v
					} else {
						l = c.getPropSlow(o, name, site, pc)
					}
				} else {
					l = c.getPropSlow(o, name, site, pc)
				}
				if l.IsNull() {
					return yes
				}
				return no
			}
		case test == litUndefined:
			return func(c *tctx) int {
				o := c.f.this
				if !this {
					o = c.locals[k]
				}
				var l Value
				if o.IsObject() {
					ob := o.object()
					if v, ok := c.ic[site].own(ob); ok {
						l = v
					} else if v, ok := ownScan(&c.ic[site], ob, c.names[name]); ok {
						l = v
					} else {
						l = c.getPropSlow(o, name, site, pc)
					}
				} else {
					l = c.getPropSlow(o, name, site, pc)
				}
				if l.IsUndefined() {
					return yes
				}
				return no
			}
		case test == litNullish:
			return func(c *tctx) int {
				o := c.f.this
				if !this {
					o = c.locals[k]
				}
				var l Value
				if o.IsObject() {
					ob := o.object()
					if v, ok := c.ic[site].own(ob); ok {
						l = v
					} else if v, ok := ownScan(&c.ic[site], ob, c.names[name]); ok {
						l = v
					} else {
						l = c.getPropSlow(o, name, site, pc)
					}
				} else {
					l = c.getPropSlow(o, name, site, pc)
				}
				if l.IsNullish() || l.IsObject() && l.Object().flags&objHTMLDDA != 0 {
					return yes
				}
				return no
			}
		}
		return nil
	}
}
