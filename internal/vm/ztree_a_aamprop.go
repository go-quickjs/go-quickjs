package vm

import "github.com/go-quickjs/go-quickjs/internal/bytecode"

// obj.p.name(args) where obj is this or a local -- this.currentTcb.run(),
// this.scheduler.queue(v), this.task.run(packet), the calls an object makes
// of the objects it holds -- as one node: the receiver's property read in
// place, the method looked up by the site's cache, the arguments evaluated
// into their slots in the bytecode's order, and the call made as
// methodCallN's node makes it. A get_prop_this of such a receiver had spilled
// every entry beneath it, so that the receiver sat in its slot, and then
// read the method from the slot (getPropThisNode), and the call spilled the
// method and the arguments: a statement, the receiver's node, the method's
// and callNode, where a local's receiver is one node.
//
// The get_prop_this now leaves a method entry over the receiver's entry,
// whose own tree is getPropThisNode's, reading the receiver from its slot,
// and the receiver's tree, which stays a read in place, also writes what it
// reads to that slot, its own (recvToSlot): whatever takes the two other
// than their call -- a spill at a block's end, an optional call's test,
// apply_arguments -- evaluates them in stack order, as every entry is, so
// the slot holds the receiver when the method's tree runs, read once. The
// call of them (b.methodCall) is methodProp's node, which runs neither tree:
// it reads the receiver once, in place, and keeps it out of its slot. A call of it in tail position spills first (propTailSpill), and is
// made as it was. charCodeAt keeps its own node, which reads any receiver
// by its tree.
//
// Its functions are variables, each assigned a closure in init, none of
// which captures a variable of the init's; the file sorts before the tree
// tier's other files with an init, so its closures are laid out after
// every closure there was (see ztree_a_aheap.go).

var (
	// methodProp is recv.name(args) for a get_prop_this at gpc of recv, the
	// property p reads in place, with the arguments to evaluate into slots
	// p+1..., and the call_method at pc.
	methodProp func(p *propOf, get bytecode.Instr, gpc int, args []tval, slot, pc int) tval

	// propTailSpill spills the entries of a tail_call_method of n arguments
	// whose method entry is over a property read in place (methodProp's),
	// which tailAll does not read, and reports false where the spill cannot
	// be made.
	propTailSpill func(b *tbuilder, n int) bool

	// recvToSlot is the receiver's tree v, which also writes what it gives
	// to slot d, its own.
	recvToSlot func(v tval, d int) tval
)

func init() {
	recvToSlot = func(v tval, d int) tval {
		return func(c *tctx) Value {
			x := v(c)
			c.stack[d] = x
			return x
		}
	}

	propTailSpill = func(b *tbuilder, n int) bool {
		if p := len(b.stack) - n - 1; p >= 1 && b.stack[p].method != nil && propIn(b.stack[p-1]) != nil {
			return b.spill()
		}
		return true
	}

	methodProp = func(p *propOf, get bytecode.Instr, gpc int, args []tval, slot, pc int) tval {
		nd := propMethodNode{name: p.name, site: p.site, ppc: p.pc, this: p.obj.this, k: p.obj.k,
			mname: get.A, msite: get.B, gpc: gpc, p: slot, pc: pc}
		switch len(args) {
		case 0:
			return func(c *tctx) Value {
				o := c.f.this
				if !nd.this {
					o = c.locals[nd.k]
				}
				// The receiver, read as getPropOperand's node reads it.
				if o.IsObject() {
					ob := o.object()
					if v, ok := c.ic[nd.site].own(ob); ok {
						o = v
					} else if v, ok := ownScan(&c.ic[nd.site], ob, c.names[nd.name]); ok {
						o = v
					} else {
						o = c.getPropSlow(o, nd.name, nd.site, nd.ppc)
					}
				} else {
					o = c.getPropSlow(o, nd.name, nd.site, nd.ppc)
				}
				// The method, read as getPropThisNode reads it.
				var m Value
				ok := false
				key := c.names[nd.mname]
				if o.IsObject() {
					m, ok = c.r.cachedProp(&c.ic[nd.msite], o.object(), key)
				}
				if !ok {
					c.at(nd.gpc)
					if jitBuilt && o.IsBigInt() {
						treeBigRecv(c, nd.gpc)
					}
					var err error
					if m, err = c.r.getValueProp(o, key); err != nil {
						c.throw(err)
					}
				}
				c.at(nd.pc)
				as := c.stack[nd.p+1 : nd.p+1]
				var f *Object
				var fd *funcData
				if m.IsObject() {
					f = m.object()
					fd = f.fn()
					if fd != nil && fd.treeCall != nil && fd.closure.realm == c.r.Realm {
						r := c.r
						if err := r.tick(); err != nil {
							c.throw(err)
						}
						v, err := r.callTree(f, fd, o, as, true)
						if err != nil {
							c.throw(err)
						}
						return v
					}
					if fd != nil && fd.native == nil && !fd.bound && fd.closure != nil &&
						fd.closure.realm == c.r.Realm && fd.closure.fn.Leaf != bytecode.LeafNone && fd.closure.fn.DirectCall {
						r := c.r
						if err := r.tick(); err != nil {
							c.throw(err)
						}
						if fn := fd.closure.fn; fn.Leaf == bytecode.LeafPure {
							if pureFirst(fd, fn) {
								if v, ok := r.pureCallCounted(fd, o, as); ok {
									return v
								}
							}
						} else if o.IsObject() {
							if v, ok := r.leafCall(fd.closure, o.object(), as); ok {
								return v
							}
						}
						v, err := leafRest(r, f, fd, o, as)
						if err != nil {
							c.throw(err)
						}
						return v
					}
				}
				return methodRest(c, m, f, fd, o, as)
			}
		case 1:
			a0 := args[0]
			return func(c *tctx) Value {
				o := c.f.this
				if !nd.this {
					o = c.locals[nd.k]
				}
				// The receiver, read as getPropOperand's node reads it.
				if o.IsObject() {
					ob := o.object()
					if v, ok := c.ic[nd.site].own(ob); ok {
						o = v
					} else if v, ok := ownScan(&c.ic[nd.site], ob, c.names[nd.name]); ok {
						o = v
					} else {
						o = c.getPropSlow(o, nd.name, nd.site, nd.ppc)
					}
				} else {
					o = c.getPropSlow(o, nd.name, nd.site, nd.ppc)
				}
				// The method, read as getPropThisNode reads it.
				var m Value
				ok := false
				key := c.names[nd.mname]
				if o.IsObject() {
					m, ok = c.r.cachedProp(&c.ic[nd.msite], o.object(), key)
				}
				if !ok {
					c.at(nd.gpc)
					if jitBuilt && o.IsBigInt() {
						treeBigRecv(c, nd.gpc)
					}
					var err error
					if m, err = c.r.getValueProp(o, key); err != nil {
						c.throw(err)
					}
				}
				c.stack[nd.p+1] = a0(c)
				c.at(nd.pc)
				as := c.stack[nd.p+1 : nd.p+2]
				var f *Object
				var fd *funcData
				if m.IsObject() {
					f = m.object()
					fd = f.fn()
					if fd != nil && fd.treeCall != nil && fd.closure.realm == c.r.Realm {
						r := c.r
						if err := r.tick(); err != nil {
							c.throw(err)
						}
						v, err := r.callTree(f, fd, o, as, true)
						if err != nil {
							c.throw(err)
						}
						return v
					}
					if fd != nil && fd.native == nil && !fd.bound && fd.closure != nil &&
						fd.closure.realm == c.r.Realm && fd.closure.fn.Leaf != bytecode.LeafNone && fd.closure.fn.DirectCall {
						r := c.r
						if err := r.tick(); err != nil {
							c.throw(err)
						}
						if fn := fd.closure.fn; fn.Leaf == bytecode.LeafPure {
							if pureFirst(fd, fn) {
								if v, ok := r.pureCallCounted(fd, o, as); ok {
									return v
								}
							}
						} else if o.IsObject() {
							if v, ok := r.leafCall(fd.closure, o.object(), as); ok {
								return v
							}
						}
						v, err := leafRest(r, f, fd, o, as)
						if err != nil {
							c.throw(err)
						}
						return v
					}
				}
				return methodRest(c, m, f, fd, o, as)
			}
		case 2:
			a0, a1 := args[0], args[1]
			return func(c *tctx) Value {
				o := c.f.this
				if !nd.this {
					o = c.locals[nd.k]
				}
				// The receiver, read as getPropOperand's node reads it.
				if o.IsObject() {
					ob := o.object()
					if v, ok := c.ic[nd.site].own(ob); ok {
						o = v
					} else if v, ok := ownScan(&c.ic[nd.site], ob, c.names[nd.name]); ok {
						o = v
					} else {
						o = c.getPropSlow(o, nd.name, nd.site, nd.ppc)
					}
				} else {
					o = c.getPropSlow(o, nd.name, nd.site, nd.ppc)
				}
				// The method, read as getPropThisNode reads it.
				var m Value
				ok := false
				key := c.names[nd.mname]
				if o.IsObject() {
					m, ok = c.r.cachedProp(&c.ic[nd.msite], o.object(), key)
				}
				if !ok {
					c.at(nd.gpc)
					if jitBuilt && o.IsBigInt() {
						treeBigRecv(c, nd.gpc)
					}
					var err error
					if m, err = c.r.getValueProp(o, key); err != nil {
						c.throw(err)
					}
				}
				c.stack[nd.p+1] = a0(c)
				c.stack[nd.p+2] = a1(c)
				c.at(nd.pc)
				as := c.stack[nd.p+1 : nd.p+3]
				var f *Object
				var fd *funcData
				if m.IsObject() {
					f = m.object()
					fd = f.fn()
					if fd != nil && fd.treeCall != nil && fd.closure.realm == c.r.Realm {
						r := c.r
						if err := r.tick(); err != nil {
							c.throw(err)
						}
						v, err := r.callTree(f, fd, o, as, true)
						if err != nil {
							c.throw(err)
						}
						return v
					}
					if fd != nil && fd.native == nil && !fd.bound && fd.closure != nil &&
						fd.closure.realm == c.r.Realm && fd.closure.fn.Leaf != bytecode.LeafNone && fd.closure.fn.DirectCall {
						r := c.r
						if err := r.tick(); err != nil {
							c.throw(err)
						}
						if fn := fd.closure.fn; fn.Leaf == bytecode.LeafPure {
							if pureFirst(fd, fn) {
								if v, ok := r.pureCallCounted(fd, o, as); ok {
									return v
								}
							}
						} else if o.IsObject() {
							if v, ok := r.leafCall(fd.closure, o.object(), as); ok {
								return v
							}
						}
						v, err := leafRest(r, f, fd, o, as)
						if err != nil {
							c.throw(err)
						}
						return v
					}
				}
				return methodRest(c, m, f, fd, o, as)
			}
		}
		n := len(args)
		return func(c *tctx) Value {
			o := c.f.this
			if !nd.this {
				o = c.locals[nd.k]
			}
			// The receiver, read as getPropOperand's node reads it.
			if o.IsObject() {
				ob := o.object()
				if v, ok := c.ic[nd.site].own(ob); ok {
					o = v
				} else if v, ok := ownScan(&c.ic[nd.site], ob, c.names[nd.name]); ok {
					o = v
				} else {
					o = c.getPropSlow(o, nd.name, nd.site, nd.ppc)
				}
			} else {
				o = c.getPropSlow(o, nd.name, nd.site, nd.ppc)
			}
			// The method, read as getPropThisNode reads it.
			var m Value
			ok := false
			key := c.names[nd.mname]
			if o.IsObject() {
				m, ok = c.r.cachedProp(&c.ic[nd.msite], o.object(), key)
			}
			if !ok {
				c.at(nd.gpc)
				if jitBuilt && o.IsBigInt() {
					treeBigRecv(c, nd.gpc)
				}
				var err error
				if m, err = c.r.getValueProp(o, key); err != nil {
					c.throw(err)
				}
			}
			for i, a := range args {
				c.stack[nd.p+1+i] = a(c)
			}
			c.at(nd.pc)
			as := c.stack[nd.p+1 : nd.p+1+n]
			var f *Object
			var fd *funcData
			if m.IsObject() {
				f = m.object()
				fd = f.fn()
				if fd != nil && fd.treeCall != nil && fd.closure.realm == c.r.Realm {
					r := c.r
					if err := r.tick(); err != nil {
						c.throw(err)
					}
					v, err := r.callTree(f, fd, o, as, true)
					if err != nil {
						c.throw(err)
					}
					return v
				}
				if fd != nil && fd.native == nil && !fd.bound && fd.closure != nil &&
					fd.closure.realm == c.r.Realm && fd.closure.fn.Leaf != bytecode.LeafNone && fd.closure.fn.DirectCall {
					r := c.r
					if err := r.tick(); err != nil {
						c.throw(err)
					}
					if fn := fd.closure.fn; fn.Leaf == bytecode.LeafPure {
						if pureFirst(fd, fn) {
							if v, ok := r.pureCallCounted(fd, o, as); ok {
								return v
							}
						}
					} else if o.IsObject() {
						if v, ok := r.leafCall(fd.closure, o.object(), as); ok {
							return v
						}
					}
					v, err := leafRest(r, f, fd, o, as)
					if err != nil {
						c.throw(err)
					}
					return v
				}
			}
			return methodRest(c, m, f, fd, o, as)
		}
	}
}

// propMethodNode is what methodProp's node reads: the receiver's property,
// name at site, read at ppc of this or of the local k; the method, mname at
// msite, read at gpc; the arguments' slots, from p+1; and the call at pc.
type propMethodNode struct {
	name, site   uint32
	ppc          int
	this         bool
	k            uint32
	mname, msite uint32
	gpc, p, pc   int
}
