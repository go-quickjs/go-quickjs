package vm

import "github.com/go-quickjs/go-quickjs/internal/bytecode"

// Property reads of a property, and writes to one, in the tree tier:
// this.emitState.column, ast.preComments.length, TypeScript.NodeType.Name,
// m.start.emittedLine = l -- the object a property of this, a local or an
// upvalue -- as one node. The inner read
// had been a node of its own, which the outer one called for its object;
// here the one node reads the base in place, then the inner property and
// the outer one as getPropOperand reads each: the site's own entry, its
// scan, and getPropSlow for anything else, each at its own site and pc.
//
// A get_prop whose object is read in place says so in its entry (rare.prop),
// whose tree is still getPropOperand's for every other consumer.
//
// Its functions are variables, each assigned a closure in init, none of
// which captures a variable of the init's; the file sorts before the tree
// tier's other files with an init, so its closures are laid out after
// every closure there was (see ztree_a_aheap.go).

// propOf is a property read of an object read in place: the object's entry
// and the read's name, site and pc.
type propOf struct {
	obj        tentry
	name, site uint32
	pc         int
}

var (
	// propEntry is get_prop of obj at pc: getPropOperand's tree, with what
	// it is of where obj is read in place, or one node of both reads where
	// obj is such a read itself.
	propEntry func(obj tentry, in bytecode.Instr, pc int) tentry
	// propChain is the property in of the property inner reads.
	propChain func(inner *propOf, in bytecode.Instr, pc int) tval
	// propSetChain is set_prop in of val to the property inner reads, as
	// setPropStmt stores it.
	propSetChain func(inner *propOf, val tval, in bytecode.Instr, pc int, strict bool) tstmt
)

func init() {
	propEntry = func(obj tentry, in bytecode.Instr, pc int) tentry {
		if x := obj.rare; x != nil && x.prop != nil && obj.v != nil && obj.slot < 0 {
			return tentry{slot: -1, v: propChain(x.prop, in, pc)}
		}
		v := getPropOperand(obj, in, pc)
		if obj.v == nil && obj.slot < 0 && (obj.this || obj.local || obj.upvalue) {
			return tentry{slot: -1, v: v, rare: &tentryRare{prop: &propOf{obj: obj, name: in.A, site: in.B, pc: pc}}}
		}
		return tentry{slot: -1, v: v}
	}

	propChain = func(inner *propOf, in bytecode.Instr, pc int) tval {
		iname, isite, ipc := inner.name, inner.site, inner.pc
		name, site := in.A, in.B
		k := inner.obj.k
		switch {
		case inner.obj.this:
			return func(c *tctx) Value {
				o := c.f.this
				var m Value
				if o.IsObject() {
					ob := o.object()
					if v, ok := c.ic[isite].own(ob); ok {
						m = v
					} else if v, ok := ownScan(&c.ic[isite], ob, c.names[iname]); ok {
						m = v
					} else {
						m = c.getPropSlow(o, iname, isite, ipc)
					}
				} else {
					m = c.getPropSlow(o, iname, isite, ipc)
				}
				if m.IsObject() {
					mb := m.object()
					if v, ok := c.ic[site].own(mb); ok {
						return v
					}
					if v, ok := ownScan(&c.ic[site], mb, c.names[name]); ok {
						return v
					}
				}
				return c.getPropSlow(m, name, site, pc)
			}
		case inner.obj.local:
			return func(c *tctx) Value {
				o := c.locals[k]
				var m Value
				if o.IsObject() {
					ob := o.object()
					if v, ok := c.ic[isite].own(ob); ok {
						m = v
					} else if v, ok := ownScan(&c.ic[isite], ob, c.names[iname]); ok {
						m = v
					} else {
						m = c.getPropSlow(o, iname, isite, ipc)
					}
				} else {
					m = c.getPropSlow(o, iname, isite, ipc)
				}
				if m.IsObject() {
					mb := m.object()
					if v, ok := c.ic[site].own(mb); ok {
						return v
					}
					if v, ok := ownScan(&c.ic[site], mb, c.names[name]); ok {
						return v
					}
				}
				return c.getPropSlow(m, name, site, pc)
			}
		}
		return func(c *tctx) Value {
			o := c.cl.upvalues[k].get()
			var m Value
			if o.IsObject() {
				ob := o.object()
				if v, ok := c.ic[isite].own(ob); ok {
					m = v
				} else if v, ok := ownScan(&c.ic[isite], ob, c.names[iname]); ok {
					m = v
				} else {
					m = c.getPropSlow(o, iname, isite, ipc)
				}
			} else {
				m = c.getPropSlow(o, iname, isite, ipc)
			}
			if m.IsObject() {
				mb := m.object()
				if v, ok := c.ic[site].own(mb); ok {
					return v
				}
				if v, ok := ownScan(&c.ic[site], mb, c.names[name]); ok {
					return v
				}
			}
			return c.getPropSlow(m, name, site, pc)
		}
	}

	propSetChain = func(inner *propOf, val tval, in bytecode.Instr, pc int, strict bool) tstmt {
		iname, isite, ipc := inner.name, inner.site, inner.pc
		name, site := in.A, in.B
		k := inner.obj.k
		switch {
		case inner.obj.this:
			return func(c *tctx) {
				o := c.f.this
				var m Value
				if o.IsObject() {
					ob := o.object()
					if v, ok := c.ic[isite].own(ob); ok {
						m = v
					} else if v, ok := ownScan(&c.ic[isite], ob, c.names[iname]); ok {
						m = v
					} else {
						m = c.getPropSlow(o, iname, isite, ipc)
					}
				} else {
					m = c.getPropSlow(o, iname, isite, ipc)
				}
				v := val(c)
				if m.IsObject() && c.ic[site].storeOwn(m.object(), v) {
					return
				}
				c.setProp(m, v, name, site, pc, strict)
			}
		case inner.obj.local:
			return func(c *tctx) {
				o := c.locals[k]
				var m Value
				if o.IsObject() {
					ob := o.object()
					if v, ok := c.ic[isite].own(ob); ok {
						m = v
					} else if v, ok := ownScan(&c.ic[isite], ob, c.names[iname]); ok {
						m = v
					} else {
						m = c.getPropSlow(o, iname, isite, ipc)
					}
				} else {
					m = c.getPropSlow(o, iname, isite, ipc)
				}
				v := val(c)
				if m.IsObject() && c.ic[site].storeOwn(m.object(), v) {
					return
				}
				c.setProp(m, v, name, site, pc, strict)
			}
		}
		return func(c *tctx) {
			o := c.cl.upvalues[k].get()
			var m Value
			if o.IsObject() {
				ob := o.object()
				if v, ok := c.ic[isite].own(ob); ok {
					m = v
				} else if v, ok := ownScan(&c.ic[isite], ob, c.names[iname]); ok {
					m = v
				} else {
					m = c.getPropSlow(o, iname, isite, ipc)
				}
			} else {
				m = c.getPropSlow(o, iname, isite, ipc)
			}
			v := val(c)
			if m.IsObject() && c.ic[site].storeOwn(m.object(), v) {
				return
			}
			c.setProp(m, v, name, site, pc, strict)
		}
	}
}
