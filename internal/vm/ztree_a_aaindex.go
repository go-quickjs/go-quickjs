package vm

// Element reads and writes of an array that is a property of this or of a
// local -- this.storage[i], this.piles[pile] = disk, q.items[q.first] --
// as one node of the tree tier. The property read had been
// getPropOperand's node, which the element's node called for its object,
// and the key a local's node of its own: three calls for two loads.
// Methods of a class keeping its elements in an array are made of them,
// as Towers' piles, som's Vector, and Havlak's nodes and headers are.
//
// The node reads what the nodes did, in their order: the object, its
// property as getPropOperand's node reads it -- the site's own entry, its
// scan, and getPropSlow for anything else, a getter that may assign to
// the key's local among them -- then the key, then, for a write, the
// value; the element as getIndexOperands's node reads it and as
// setIndexStmt's writes it.
//
// Its functions are variables, each assigned a closure in init, none of
// which captures a variable of the init's; the file sorts before the tree
// tier's other files with an init, but for ztree_a_aalit.go and
// ztree_a_aaprop.go, so its closures are laid out after every closure
// there was (see ztree_a_aheap.go).

var (
	// propIndexGet is obj[key] where obj is a property read of this or
	// of a local, read in place (propEntry's rare.prop), as one node; nil
	// for any other object, which getIndexOperands reads.
	propIndexGet func(obj, key tentry, pc int) tval
	// propIndexSet is obj[key] = val, as setIndexStmt stores it, where
	// obj is what propIndexGet reads; nil for any other object.
	propIndexSet func(obj, key tentry, val tval, pc int, strict bool) tstmt
	// propIn is the property read obj is, where it is one of this or of a
	// local read in place, or nil.
	propIn func(obj tentry) *propOf
)

func init() {
	propIn = func(obj tentry) *propOf {
		x := obj.rare
		if x == nil || x.prop == nil || obj.v == nil || obj.slot >= 0 {
			return nil
		}
		if p := x.prop; p.obj.this || p.obj.local {
			return p
		}
		return nil
	}

	propIndexGet = func(obj, key tentry, pc int) tval {
		p := propIn(obj)
		if p == nil {
			return nil
		}
		name, site, ppc, this, j := p.name, p.site, p.pc, p.obj.this, p.obj.k
		if key.local && key.v == nil && key.slot < 0 {
			k := key.k
			return func(c *tctx) Value {
				o := c.f.this
				if !this {
					o = c.locals[j]
				}
				var a Value
				if o.IsObject() {
					ob := o.object()
					if v, ok := c.ic[site].own(ob); ok {
						a = v
					} else if v, ok := ownScan(&c.ic[site], ob, c.names[name]); ok {
						a = v
					} else {
						a = c.getPropSlow(o, name, site, ppc)
					}
				} else {
					a = c.getPropSlow(o, name, site, ppc)
				}
				i := c.locals[k]
				if v, ok := elemAt(a, i); ok {
					return v
				}
				return c.getIndexMiss(a, i, pc)
			}
		}
		t := key.tree()
		return func(c *tctx) Value {
			o := c.f.this
			if !this {
				o = c.locals[j]
			}
			var a Value
			if o.IsObject() {
				ob := o.object()
				if v, ok := c.ic[site].own(ob); ok {
					a = v
				} else if v, ok := ownScan(&c.ic[site], ob, c.names[name]); ok {
					a = v
				} else {
					a = c.getPropSlow(o, name, site, ppc)
				}
			} else {
				a = c.getPropSlow(o, name, site, ppc)
			}
			i := t(c)
			if v, ok := elemAt(a, i); ok {
				return v
			}
			return c.getIndexMiss(a, i, pc)
		}
	}

	propIndexSet = func(obj, key tentry, val tval, pc int, strict bool) tstmt {
		p := propIn(obj)
		if p == nil {
			return nil
		}
		name, site, ppc, this, j := p.name, p.site, p.pc, p.obj.this, p.obj.k
		if key.local && key.v == nil && key.slot < 0 {
			k := key.k
			return func(c *tctx) {
				o := c.f.this
				if !this {
					o = c.locals[j]
				}
				var a Value
				if o.IsObject() {
					ob := o.object()
					if v, ok := c.ic[site].own(ob); ok {
						a = v
					} else if v, ok := ownScan(&c.ic[site], ob, c.names[name]); ok {
						a = v
					} else {
						a = c.getPropSlow(o, name, site, ppc)
					}
				} else {
					a = c.getPropSlow(o, name, site, ppc)
				}
				i := c.locals[k]
				if v := val(c); !setElem(a, i, v) {
					c.setIndexMiss(a, i, v, pc, strict)
				}
			}
		}
		t := key.tree()
		return func(c *tctx) {
			o := c.f.this
			if !this {
				o = c.locals[j]
			}
			var a Value
			if o.IsObject() {
				ob := o.object()
				if v, ok := c.ic[site].own(ob); ok {
					a = v
				} else if v, ok := ownScan(&c.ic[site], ob, c.names[name]); ok {
					a = v
				} else {
					a = c.getPropSlow(o, name, site, ppc)
				}
			} else {
				a = c.getPropSlow(o, name, site, ppc)
			}
			i := t(c)
			if v := val(c); !setElem(a, i, v) {
				c.setIndexMiss(a, i, v, pc, strict)
			}
		}
	}
}
