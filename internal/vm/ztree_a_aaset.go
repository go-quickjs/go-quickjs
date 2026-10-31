package vm

import "github.com/go-quickjs/go-quickjs/internal/bytecode"

// A property of this or of a local given a local or a constant -- this.val
// = v, disk.next = top, top.next = null, this.count = 0 -- as one
// statement of the tree tier. setPropStmt's statement had called the
// value's node, a local's read or a constant's a call of its own, for a
// store into the object's table: here the value is read in place. What
// the statement does is setPropStmt's, in its order: the object read, then
// the value, then the store by the site's own entry (storeOwn), and
// anything else -- a setter, a proxy, a new property, a primitive, a
// frozen object -- by setProp.
//
// Its functions are variables, each assigned a closure in init, none of
// which captures a variable of the init's; the file sorts before the tree
// tier's other files with an init, so its closures are laid out after
// every closure there was (see ztree_a_aheap.go).

// setPropAt is obj.name = val, for set_prop in at pc, as one statement
// where obj is this or a local and val a local or a constant, each read
// in place; nil for anything else, which setPropStmt stores.
var setPropAt func(obj, val tentry, in bytecode.Instr, pc int, strict bool) tstmt

func init() {
	setPropAt = func(obj, val tentry, in bytecode.Instr, pc int, strict bool) tstmt {
		if obj.v != nil || obj.slot >= 0 || !obj.this && !obj.local || val.v != nil || val.slot >= 0 {
			return nil
		}
		name, site, this, k := in.A, in.B, obj.this, obj.k
		switch {
		case val.local:
			j := val.k
			return func(c *tctx) {
				o := c.f.this
				if !this {
					o = c.locals[k]
				}
				v := c.locals[j]
				if o.IsObject() && c.ic[site].storeOwn(o.object(), v) {
					return
				}
				c.setProp(o, v, name, site, pc, strict)
			}
		case val.literal || val.number:
			v := val.n
			return func(c *tctx) {
				o := c.f.this
				if !this {
					o = c.locals[k]
				}
				if o.IsObject() && c.ic[site].storeOwn(o.object(), v) {
					return
				}
				c.setProp(o, v, name, site, pc, strict)
			}
		}
		return nil
	}
}
