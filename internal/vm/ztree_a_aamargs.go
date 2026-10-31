package vm

import "github.com/go-quickjs/go-quickjs/internal/bytecode"

// recv.name(a) and recv.name(a, b) on a receiver read in place whose
// arguments are locals -- this.moveTopDisk(fromPile, toPile),
// v.addConstraint(c) -- as methodCallN's node makes the call, but with the
// arguments read in place. methodCallN's node called each argument's node,
// a local's read a call of its own, to put it in its slot. Here each local
// is read into its slot, in the order the bytecode gives them, and the rest
// is methodCallN's node's: the method looked up by the site's cache, a
// function planTreeCall has given a tree called on it, a leaf or a pure
// body called here, anything else by methodRest.
//
// Its functions are variables, each assigned a closure in init, none of
// which captures a variable of the init's; the file sorts before the tree
// tier's other files with an init, so its closures are laid out after
// every closure there was (see ztree_a_aheap.go).

// methodLocals is recv.name(args) for a get_prop_this at gpc of recv read
// in place, where the arguments are one or two locals, ks, to read into
// slots p+1..., and the call_method at pc; nil for any other count.
var methodLocals func(recv tentry, get bytecode.Instr, gpc int, ks []uint32, p, pc int) tval

// argLocals is the locals args are reads of, where they are one or two
// locals read in place; nil for anything else.
var argLocals func(args []tentry) []uint32

func init() {
	argLocals = func(args []tentry) []uint32 {
		if len(args) == 0 || len(args) > 2 {
			return nil
		}
		ks := make([]uint32, len(args))
		for i, e := range args {
			if !e.local || e.v != nil || e.slot >= 0 {
				return nil
			}
			ks[i] = e.k
		}
		return ks
	}

	methodLocals = func(recv tentry, get bytecode.Instr, gpc int, ks []uint32, p, pc int) tval {
		nd := methodNode{this: recv.this, k: recv.k, name: get.A, site: get.B, gpc: gpc, p: p, pc: pc}
		switch len(ks) {
		case 1:
			k0 := ks[0]
			return func(c *tctx) Value {
				o := c.f.this
				if !nd.this {
					o = c.locals[nd.k]
				}
				var m Value
				ok := false
				key := c.names[nd.name]
				if o.IsObject() {
					m, ok = c.r.cachedProp(&c.ic[nd.site], o.object(), key)
				} else {
					m, ok = primMethod(c.r, o, key)
				}
				if !ok {
					m = methodMiss(c, o, key, nd.gpc)
				}
				c.stack[nd.p+1] = c.locals[k0]
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
								if v, ok := r.pureCall(fd, o, as); ok {
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
			k0, k1 := ks[0], ks[1]
			return func(c *tctx) Value {
				o := c.f.this
				if !nd.this {
					o = c.locals[nd.k]
				}
				var m Value
				ok := false
				key := c.names[nd.name]
				if o.IsObject() {
					m, ok = c.r.cachedProp(&c.ic[nd.site], o.object(), key)
				} else {
					m, ok = primMethod(c.r, o, key)
				}
				if !ok {
					m = methodMiss(c, o, key, nd.gpc)
				}
				c.stack[nd.p+1] = c.locals[k0]
				c.stack[nd.p+2] = c.locals[k1]
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
								if v, ok := r.pureCall(fd, o, as); ok {
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
		return nil
	}
}
