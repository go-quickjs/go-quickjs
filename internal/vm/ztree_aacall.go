package vm

import "github.com/go-quickjs/go-quickjs/internal/bytecode"

// recv.name(a, b) with up to three arguments, as methodCall's node makes it
// but written out for the count. methodCall's node evaluates its arguments
// in a loop, and Go keeps everything the loop and the call after it use --
// the receiver, the method, the slots, the sites -- live across each
// argument's call, which saves no register: each turn reloaded about a
// dozen of them from the stack, and the call after it as many, about a
// third of the node's instructions for a call of two arguments. Here each
// argument is evaluated into its slot in turn, as there; a method
// planTreeCall has given a tree is called on it in the node, and any other
// call is methodApply's, which takes what it needs as its parameters. The
// node keeps what it was built with as one variable (methodNode).
// callOperands's node for f(...), and for a method call of a receiver in
// its slot, is written out the same way (callOperandsN).
//
// The code is closures assigned in this file's init, the package's first
// init with closures, which are laid out after every other closure (see
// ztree_aaeq.go).

var (
	// methodCallN is methodCall for up to three arguments, or nil for more.
	methodCallN func(recv tentry, get bytecode.Instr, gpc int, args []tval, p, pc int) tval

	// callOperandsN is callOperands for a call, or a call_method of a
	// receiver in its slot, of up to three arguments, or nil for anything
	// else.
	callOperandsN func(op bytecode.Op, f tval, args []tval, p, pc int) tval

	// callApply calls fv with recv as this and the arguments as, in their
	// slots, as callOperands's node calls it once it has found fv no
	// function planTreeCall has given a tree.
	callApply func(c *tctx, fv, recv Value, as []Value) Value

	// methodMiss is the read of the method key of o at gpc that the
	// site's cache did not answer.
	methodMiss func(c *tctx, o Value, key Atom, gpc int) Value

	// methodApply calls the method m with o as this and the arguments as,
	// in their slots, as methodCall's node calls it, and returns what it
	// returns or throws what it throws.
	methodApply func(c *tctx, m, o Value, as []Value) Value
)

func init() {
	callApply = func(c *tctx, fv, recv Value, as []Value) Value {
		if fv.IsObject() {
			o := fv.object()
			v, err := c.r.callRest(o, o.fn(), recv, as)
			if err != nil {
				c.throw(err)
			}
			return v
		}
		v, err := c.r.call(fv, recv, as)
		if err != nil {
			c.throw(err)
		}
		return v
	}

	methodMiss = func(c *tctx, o Value, key Atom, gpc int) Value {
		c.at(gpc)
		m, err := c.r.getValueProp(o, key)
		if err != nil {
			c.throw(err)
		}
		return m
	}

	methodApply = func(c *tctx, m, o Value, as []Value) Value {
		if m.IsObject() {
			f := m.object()
			fd := f.fn()
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
			v, err := c.r.callRest(f, fd, o, as)
			if err != nil {
				c.throw(err)
			}
			return v
		}
		v, err := c.r.call(m, o, as)
		if err != nil {
			c.throw(err)
		}
		return v
	}

	callOperandsN = func(op bytecode.Op, f tval, args []tval, p, pc int) tval {
		if op != bytecode.OpCall && op != bytecode.OpCallMethod || len(args) > 3 {
			return nil
		}
		var a [3]tval
		copy(a[:], args)
		// Made whole here, and never written after: the nodes keep it in
		// their own allocation, a copy each.
		nd := callsNode{f: f, p: p, pc: pc, method: op == bytecode.OpCallMethod, a0: a[0], a1: a[1], a2: a[2]}
		switch len(args) {
		case 0:
			return func(c *tctx) Value {
				fv := nd.f(c)
				c.at(nd.pc)
				as := c.stack[nd.p+1 : nd.p+1]
				recv := Undefined
				if nd.method {
					recv = c.stack[nd.p-1]
				}
				if fv.IsObject() {
					o := fv.object()
					if fd := o.fn(); fd != nil && fd.treeCall != nil && fd.closure.realm == c.r.Realm {
						r := c.r
						if err := r.tick(); err != nil {
							c.throw(err)
						}
						v, err := r.callTree(o, fd, recv, as, true)
						if err != nil {
							c.throw(err)
						}
						return v
					}
				}
				return callApply(c, fv, recv, as)
			}
		case 1:
			return func(c *tctx) Value {
				fv := nd.f(c)
				c.stack[nd.p+1] = nd.a0(c)
				c.at(nd.pc)
				as := c.stack[nd.p+1 : nd.p+2]
				recv := Undefined
				if nd.method {
					recv = c.stack[nd.p-1]
				}
				if fv.IsObject() {
					o := fv.object()
					if fd := o.fn(); fd != nil && fd.treeCall != nil && fd.closure.realm == c.r.Realm {
						r := c.r
						if err := r.tick(); err != nil {
							c.throw(err)
						}
						v, err := r.callTree(o, fd, recv, as, true)
						if err != nil {
							c.throw(err)
						}
						return v
					}
				}
				return callApply(c, fv, recv, as)
			}
		case 2:
			return func(c *tctx) Value {
				fv := nd.f(c)
				c.stack[nd.p+1] = nd.a0(c)
				c.stack[nd.p+2] = nd.a1(c)
				c.at(nd.pc)
				as := c.stack[nd.p+1 : nd.p+3]
				recv := Undefined
				if nd.method {
					recv = c.stack[nd.p-1]
				}
				if fv.IsObject() {
					o := fv.object()
					if fd := o.fn(); fd != nil && fd.treeCall != nil && fd.closure.realm == c.r.Realm {
						r := c.r
						if err := r.tick(); err != nil {
							c.throw(err)
						}
						v, err := r.callTree(o, fd, recv, as, true)
						if err != nil {
							c.throw(err)
						}
						return v
					}
				}
				return callApply(c, fv, recv, as)
			}
		}
		return func(c *tctx) Value {
			fv := nd.f(c)
			c.stack[nd.p+1] = nd.a0(c)
			c.stack[nd.p+2] = nd.a1(c)
			c.stack[nd.p+3] = nd.a2(c)
			c.at(nd.pc)
			as := c.stack[nd.p+1 : nd.p+4]
			recv := Undefined
			if nd.method {
				recv = c.stack[nd.p-1]
			}
			if fv.IsObject() {
				o := fv.object()
				if fd := o.fn(); fd != nil && fd.treeCall != nil && fd.closure.realm == c.r.Realm {
					r := c.r
					if err := r.tick(); err != nil {
						c.throw(err)
					}
					v, err := r.callTree(o, fd, recv, as, true)
					if err != nil {
						c.throw(err)
					}
					return v
				}
			}
			return callApply(c, fv, recv, as)
		}
	}

	methodCallN = func(recv tentry, get bytecode.Instr, gpc int, args []tval, p, pc int) tval {
		var a [3]tval
		copy(a[:], args)
		nd := methodNode{this: recv.this, k: recv.k, name: get.A, site: get.B, gpc: gpc, p: p, pc: pc, a0: a[0], a1: a[1], a2: a[2]}
		switch len(args) {
		case 0:
			return func(c *tctx) Value {
				o := c.f.this
				if !nd.this {
					o = c.locals[nd.k]
				}
				var m Value
				ok := false
				if o.IsObject() {
					m, ok = c.r.cachedProp(&c.ic[nd.site], o.object(), c.names[nd.name])
				}
				if !ok {
					m = methodMiss(c, o, c.names[nd.name], nd.gpc)
				}
				c.at(nd.pc)
				as := c.stack[nd.p+1 : nd.p+1]
				if m.IsObject() {
					f := m.object()
					if fd := f.fn(); fd != nil && fd.treeCall != nil && fd.closure.realm == c.r.Realm {
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
				}
				return methodApply(c, m, o, as)
			}
		case 1:
			return func(c *tctx) Value {
				o := c.f.this
				if !nd.this {
					o = c.locals[nd.k]
				}
				var m Value
				ok := false
				if o.IsObject() {
					m, ok = c.r.cachedProp(&c.ic[nd.site], o.object(), c.names[nd.name])
				}
				if !ok {
					m = methodMiss(c, o, c.names[nd.name], nd.gpc)
				}
				c.stack[nd.p+1] = nd.a0(c)
				c.at(nd.pc)
				as := c.stack[nd.p+1 : nd.p+2]
				if m.IsObject() {
					f := m.object()
					if fd := f.fn(); fd != nil && fd.treeCall != nil && fd.closure.realm == c.r.Realm {
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
				}
				return methodApply(c, m, o, as)
			}
		case 2:
			return func(c *tctx) Value {
				o := c.f.this
				if !nd.this {
					o = c.locals[nd.k]
				}
				var m Value
				ok := false
				if o.IsObject() {
					m, ok = c.r.cachedProp(&c.ic[nd.site], o.object(), c.names[nd.name])
				}
				if !ok {
					m = methodMiss(c, o, c.names[nd.name], nd.gpc)
				}
				c.stack[nd.p+1] = nd.a0(c)
				c.stack[nd.p+2] = nd.a1(c)
				c.at(nd.pc)
				as := c.stack[nd.p+1 : nd.p+3]
				if m.IsObject() {
					f := m.object()
					if fd := f.fn(); fd != nil && fd.treeCall != nil && fd.closure.realm == c.r.Realm {
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
				}
				return methodApply(c, m, o, as)
			}
		case 3:
			return func(c *tctx) Value {
				o := c.f.this
				if !nd.this {
					o = c.locals[nd.k]
				}
				var m Value
				ok := false
				if o.IsObject() {
					m, ok = c.r.cachedProp(&c.ic[nd.site], o.object(), c.names[nd.name])
				}
				if !ok {
					m = methodMiss(c, o, c.names[nd.name], nd.gpc)
				}
				c.stack[nd.p+1] = nd.a0(c)
				c.stack[nd.p+2] = nd.a1(c)
				c.stack[nd.p+3] = nd.a2(c)
				c.at(nd.pc)
				as := c.stack[nd.p+1 : nd.p+4]
				if m.IsObject() {
					f := m.object()
					if fd := f.fn(); fd != nil && fd.treeCall != nil && fd.closure.realm == c.r.Realm {
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
				}
				return methodApply(c, m, o, as)
			}
		}
		return nil
	}
}

// methodNode is what methodCallN's node is of: the receiver -- this, or
// local k -- the get_prop_this's name, site and pc, the slots the
// arguments go to (p+1...), the call's pc and the arguments' trees. The
// node captures it as one variable, made whole before the node and never
// written after, which Go keeps by value in the closure's own allocation:
// the node keeps only the closure across the calls it makes, and reads
// each field where it is used, where a closure's captured variables of
// their own are each loaded on entry and kept, every one of them spilled
// across the first call.
type methodNode struct {
	this       bool
	name, site uint32
	k          uint32
	gpc, p, pc int
	a0, a1, a2 tval
}

// callsNode is what callOperandsN's node is of: the callee's tree, the
// slots the arguments go to (p+1...), beneath them the receiver's for a
// call_method (p-1), the call's pc and the arguments' trees, captured as
// methodNode is.
type callsNode struct {
	method     bool
	f          tval
	p, pc      int
	a0, a1, a2 tval
}
