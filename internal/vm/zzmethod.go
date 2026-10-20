package vm

import (
	"sync/atomic"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
)

// recv.name(args) on a receiver read in place -- a local or this -- was a
// get_prop_this that spilled every entry beneath it, so that the receiver
// sat in its slot for the call to read again, and then a call that spilled
// the method and the arguments to their slots too: p.get() in a loop took
// 45 ns where p.x took 16 ns, half of it the spills and the nodes between
// them. A get_prop_this on such a receiver leaves a method entry instead
// (methodRead), which the call_method of it builds into one node with the
// arguments (methodCall): the receiver is read once, the method looked up,
// the arguments evaluated into their slots, in the order the bytecode
// gives them, and the call made as callNode makes it. Anything that spills
// first turns the method entry into its slot's read, and the call is then
// callNode's.
//
// They are variables, assigned closures in this file's init, laid out after
// every closure of the tree tier's (see AGENTS.md).

var (
	// methodRead is a get_prop_this of a receiver read in place, recv,
	// for anything other than the call methodCall makes of it: a spill, or
	// an optional call's test.
	methodRead func(recv tentry, in bytecode.Instr, pc int) tval

	// methodCall is recv.name(args...) for a get_prop_this at gpc of
	// recv read in place, with the arguments to evaluate into slots
	// p+1..., and the call_method at pc.
	methodCall func(recv tentry, get bytecode.Instr, gpc int, args []tval, p, pc int) tval

	// callOperands is call or new at pc of the callee f, evaluated, with
	// the arguments to evaluate into slots p+1..., or call_method of the
	// method f and the receiver in slot p-1: callNode, but that it
	// evaluates what it calls with, rather than a spill before it.
	callOperands func(op bytecode.Op, f tval, args []tval, p, pc int) tval

	// leafRest is what callRest does with a call of the leaf fd's that
	// leafCall or pureCall has not answered, once it has ticked.
	leafRest func(r *Runtime, o *Object, fd *funcData, this Value, args []Value) (Value, error)
)

func init() {
	callOperands = func(op bytecode.Op, f tval, args []tval, p, pc int) tval {
		n := len(args)
		switch op {
		case bytecode.OpNew:
			return func(c *tctx) Value {
				fv := f(c)
				for i, a := range args {
					c.stack[p+1+i] = a(c)
				}
				c.at(pc)
				v, err := c.r.construct(fv, c.stack[p+1:p+1+n])
				if err != nil {
					c.throw(err)
				}
				return v
			}
		case bytecode.OpCallMethod:
			return func(c *tctx) Value {
				fv := f(c)
				for i, a := range args {
					c.stack[p+1+i] = a(c)
				}
				c.at(pc)
				recv, as := c.stack[p-1], c.stack[p+1:p+1+n]
				if fv.IsObject() {
					o := fv.object()
					fd := o.fn()
					if fd != nil && fd.treeCall != nil && fd.closure.realm == c.r.Realm {
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
					v, err := c.r.callRest(o, fd, recv, as)
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
		}
		return func(c *tctx) Value {
			fv := f(c)
			for i, a := range args {
				c.stack[p+1+i] = a(c)
			}
			c.at(pc)
			as := c.stack[p+1 : p+1+n]
			if fv.IsObject() {
				o := fv.object()
				fd := o.fn()
				if fd != nil && fd.treeCall != nil && fd.closure.realm == c.r.Realm {
					r := c.r
					if err := r.tick(); err != nil {
						c.throw(err)
					}
					v, err := r.callTree(o, fd, Undefined, as, true)
					if err != nil {
						c.throw(err)
					}
					return v
				}
				v, err := c.r.callRest(o, fd, Undefined, as)
				if err != nil {
					c.throw(err)
				}
				return v
			}
			v, err := c.r.call(fv, Undefined, as)
			if err != nil {
				c.throw(err)
			}
			return v
		}
	}

	leafRest = func(r *Runtime, o *Object, fd *funcData, this Value, args []Value) (Value, error) {
		fn := fd.closure.fn
		newTarget := Undefined
		if fd.arrow {
			this, newTarget = fd.extra.lexThis, fd.extra.lexNewTarget
		}
		if fd.treeRest && !(fn.CoerceThis && !this.IsObject()) {
			v, err := r.callTreeWith(o, fd, (*tree)(atomic.LoadPointer(&fn.VMCode)), this, args, newTarget, true)
			if err == errTailCall {
				return r.tailFromTree()
			}
			return v, err
		}
		v, err := r.runFD(fd.closure, this, args, newTarget, o, fd)
		if err == errNoSuper {
			err = r.throwError(errReference, "%s", errNoSuper.Error())
		}
		if !fd.treePlanned {
			planTreeCall(fd)
		}
		return v, err
	}

	methodRead = func(recv tentry, in bytecode.Instr, pc int) tval {
		name, site := in.A, in.B
		this, k := recv.this, recv.k
		return func(c *tctx) Value {
			o := c.f.this
			if !this {
				o = c.locals[k]
			}
			if o.IsObject() {
				if v, ok := c.r.cachedProp(&c.ic[site], o.object(), c.names[name]); ok {
					return v
				}
			}
			c.at(pc)
			v, err := c.r.getValueProp(o, c.names[name])
			if err != nil {
				c.throw(err)
			}
			return v
		}
	}

	methodCall = func(recv tentry, get bytecode.Instr, gpc int, args []tval, p, pc int) tval {
		name, site := get.A, get.B
		this, k, n := recv.this, recv.k, len(args)
		return func(c *tctx) Value {
			o := c.f.this
			if !this {
				o = c.locals[k]
			}
			var m Value
			ok := false
			if o.IsObject() {
				m, ok = c.r.cachedProp(&c.ic[site], o.object(), c.names[name])
			}
			if !ok {
				c.at(gpc)
				var err error
				if m, err = c.r.getValueProp(o, c.names[name]); err != nil {
					c.throw(err)
				}
			}
			for i, a := range args {
				c.stack[p+1+i] = a(c)
			}
			c.at(pc)
			as := c.stack[p+1 : p+1+n]
			// What callNode does, but that a leaf is called here, rather than
			// by callRest.
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
	}
}

// methodOf is what a method entry is of: the get_prop_this, and where.
type methodOf struct {
	in bytecode.Instr
	pc int
}

// methodCall builds call_method at pc of n arguments, over the method
// entry m and the receiver beneath it, into methodCall's node. It spills
// nothing: the entries beneath are evaluated by what takes them, before
// the node, as every entry is, in stack order, and a statement evaluates
// all of them before itself; and above the node there is nothing in a
// slot for the arguments to overwrite, as a spill would have evaluated the
// node itself.
func (b *tbuilder) methodCall(n int, m *methodOf, pc int) tval {
	args := make([]tval, n)
	for i := n - 1; i >= 0; i-- {
		args[i] = b.pop().tree()
	}
	b.pop()
	recv := b.pop()
	return methodCall(recv, m.in, m.pc, args, b.depth()+1, pc)
}

// callAll builds call, call_method or new at pc of n arguments into
// callOperands's node, which evaluates the callee and the arguments as
// methodCall's does, without a spill; for call_method, the receiver is
// in its slot, beneath the method, as get_prop_this or get_index_this
// leaves it. It reports false for a receiver that is not.
func (b *tbuilder) callAll(op bytecode.Op, n, pc int) bool {
	p := len(b.stack) - n - 1
	if op == bytecode.OpCallMethod && (p < 1 || b.stack[p-1].slot != p-1) {
		return false
	}
	if a := b.stack[len(b.stack)-1].arith; op == bytecode.OpCallMethod && n == 2 && a != nil && wrapsOperator(a) {
		// m(w, x op y), which BigInt.asUintN(64, x op y) is made of.
		b.pop()
		w := b.pop().tree()
		f := b.pop().tree()
		b.pop()
		b.push(wrapCall(f, w, a.x.tree(), a.y.tree(), a.op, a.pc, p, pc))
		return true
	}
	args := make([]tval, n)
	for i := n - 1; i >= 0; i-- {
		args[i] = b.pop().tree()
	}
	f := b.pop().tree()
	if op == bytecode.OpCallMethod {
		b.pop()
	}
	b.push(callOperands(op, f, args, p, pc))
	return true
}
