package vm

// A local given a property of an object read in place -- node = node.next,
// const top = this.top, v = ctx.value -- as one statement of the tree tier.
// The read had been getPropOperand's node, which the store's statement
// called: two calls for a load from the object's table. A walk of a list
// is made of them, as List's isShorterThan is: xTail = xTail.next;
// yTail = yTail.next. The statement reads the object and the property as
// getPropOperand's node does -- the site's own entry, its scan, and
// getPropSlow for anything else, getters and proxies among them -- and
// stores what it read, after the read, as the store did.
//
// Its functions are variables, each assigned a closure in init, none of
// which captures a variable of the init's; the file sorts before the tree
// tier's other files with an init, but for ztree_a_aalit.go, so its
// closures are laid out after every closure there was (see
// ztree_a_aheap.go).

// storeProp is local k = e, where e is a property read of an object read
// in place (propEntry's rare.prop), as one statement; nil for any other
// entry, which the store calls the tree of.
var storeProp func(k uint32, e tentry) tstmt

func init() {
	storeProp = func(k uint32, e tentry) tstmt {
		x := e.rare
		if x == nil || x.prop == nil || e.v == nil || e.slot >= 0 {
			return nil
		}
		p := x.prop
		name, site, pc, j := p.name, p.site, p.pc, p.obj.k
		switch {
		case p.obj.this:
			return func(c *tctx) {
				o := c.f.this
				if o.IsObject() {
					ob := o.object()
					if v, ok := c.ic[site].own(ob); ok {
						c.locals[k] = v
						return
					}
					if v, ok := ownScan(&c.ic[site], ob, c.names[name]); ok {
						c.locals[k] = v
						return
					}
				}
				c.locals[k] = c.getPropSlow(o, name, site, pc)
			}
		case p.obj.local:
			return func(c *tctx) {
				o := c.locals[j]
				if o.IsObject() {
					ob := o.object()
					if v, ok := c.ic[site].own(ob); ok {
						c.locals[k] = v
						return
					}
					if v, ok := ownScan(&c.ic[site], ob, c.names[name]); ok {
						c.locals[k] = v
						return
					}
				}
				c.locals[k] = c.getPropSlow(o, name, site, pc)
			}
		case p.obj.upvalue:
			return func(c *tctx) {
				o := c.cl.upvalues[j].get()
				if o.IsObject() {
					ob := o.object()
					if v, ok := c.ic[site].own(ob); ok {
						c.locals[k] = v
						return
					}
					if v, ok := ownScan(&c.ic[site], ob, c.names[name]); ok {
						c.locals[k] = v
						return
					}
				}
				c.locals[k] = c.getPropSlow(o, name, site, pc)
			}
		}
		return nil
	}
}
