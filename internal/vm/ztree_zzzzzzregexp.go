package vm

import "github.com/go-quickjs/go-quickjs/internal/bytecode"

// A RegExp literal is a new RegExp each time it is evaluated, and one in a
// loop, /x/.test(s) or /x/.exec(s), made one each time round, which was
// then garbage as soon as the call returned: 144 bytes and the work of
// making them, the most of what such a loop allocates. Where the call is
// of the realm's own exec or test, given a primitive, it runs nothing of
// the script's -- it converts the primitive, reads the RegExp's lastIndex,
// which is the 0 it was made with, and matches -- and the RegExp, only
// ever its receiver, is reachable from nothing once the call returns. The
// literal is given it back (regexpLiteralDone), to be its next evaluation,
// lastIndex 0 again: what a new one would be, and the literal's next
// RegExp is then made with no allocation. The method is the built-in only
// where the realm's RegExp.prototype holds it as it made it, which reading
// it from the new RegExp finds without running anything, and test reads
// the RegExp's exec as it calls it, which must then be the built-in too.
//
// So does a string's replace given a string to replace with, its split, and
// its match, s.replace(/x/g, "y"), where the RegExp and its prototype are
// the realm's as it made them (regexpArgCall): each searches with the
// RegExp's pattern, writing its lastIndex, and keeps nothing of it.
//
// They are variables, assigned closures in this file's init, laid out after
// every closure of the tree tier's (see AGENTS.md).

var (
	// regexpLitCall builds call_method at pc of n arguments of a RegExp
	// literal's exec or test, the get_prop_this's entry beneath them
	// (tentry's regexpCall) and the RegExp in its slot beneath that, into
	// regexpLitCallNode's node, and reports whether it did.
	regexpLitCall func(b *tbuilder, n, pc int) bool

	// regexpLitCallNode is the call at pc of f, the get_prop_this of the
	// literal's exec or test, on the RegExp of constant k in slot p-1,
	// with the arguments to evaluate into slots p+1..., which gives the
	// literal its RegExp back where the call was the built-in's.
	regexpLitCallNode func(f tval, k uint32, test bool, args []tval, p, pc int) tval

	// regexpArgCall builds call_method at pc of n arguments, the first a
	// RegExp literal's tree, the method's entry beneath them and its
	// receiver in its slot beneath that, into a node that gives the literal
	// its RegExp back where the call was a string's replace, split or match
	// that keeps nothing of it, and reports whether it did.
	regexpArgCall func(b *tbuilder, n, pc int) bool

	// regexpArgMethodCall is methodCall's node for a method call whose
	// first argument is a RegExp literal's, of constant k, which it gives
	// the literal back as regexpArgCall's does.
	regexpArgMethodCall func(recv tentry, get bytecode.Instr, gpc int, args []tval, p, pc int, k uint32) tval

	// regexpCallMethod is fv called on recv with the arguments as, as
	// callOperands's node calls a method.
	regexpCallMethod func(c *tctx, fv, recv Value, as []Value) Value
)

func init() {
	regexpLitCall = func(b *tbuilder, n, pc int) bool {
		p := len(b.stack) - n - 1
		if p < 1 {
			return false
		}
		m := b.stack[p]
		if m.regexpCall == nil || int(m.slot) == p || int(b.stack[p-1].slot) != p-1 {
			return false
		}
		args := make([]tval, n)
		for i := n - 1; i >= 0; i-- {
			args[i] = b.pop().tree()
		}
		f := b.pop().tree()
		b.pop()
		b.push(regexpLitCallNode(f, m.regexpCall.k, m.regexpCall.test, args, p, pc))
		return true
	}

	regexpCallMethod = func(c *tctx, fv, recv Value, as []Value) Value {
		r := c.r
		if fv.IsObject() {
			o := fv.object()
			fd := o.fn()
			if fd != nil && fd.treeCall != nil && fd.closure.realm == r.Realm {
				if err := r.tick(); err != nil {
					c.throw(err)
				}
				v, err := r.callTree(o, fd, recv, as, true)
				if err != nil {
					c.throw(err)
				}
				return v
			}
			v, err := r.callRest(o, fd, recv, as)
			if err != nil {
				c.throw(err)
			}
			return v
		}
		v, err := r.call(fv, recv, as)
		if err != nil {
			c.throw(err)
		}
		return v
	}

	regexpArgCall = func(b *tbuilder, n, pc int) bool {
		p := len(b.stack) - n - 1
		if n == 0 || p < 1 || int(b.stack[p-1].slot) != p-1 || b.fn.Generator || b.fn.Async {
			return false
		}
		if lit := b.stack[p+1]; !lit.regexp || lit.slot >= 0 {
			return false
		}
		k := b.stack[p+1].k
		args := make([]tval, n)
		for i := n - 1; i >= 0; i-- {
			args[i] = b.pop().tree()
		}
		f := b.pop().tree()
		b.pop()
		b.push(func(c *tctx) Value {
			fv := f(c)
			for i, a := range args {
				c.stack[p+1+i] = a(c)
			}
			c.at(pc)
			r := c.r
			recv, as := c.stack[p-1], c.stack[p+1:p+1+n]
			rx := as[0]
			done := fv.IsObject() && rx.IsObject() && r.regexpArgCall(fv.object(), recv, as)
			v := regexpCallMethod(c, fv, recv, as)
			if done {
				r.regexpLiteralDone(&c.cl.fn.Constants[k], rx.object())
			}
			return v
		})
		return true
	}

	regexpArgMethodCall = func(recv tentry, get bytecode.Instr, gpc int, args []tval, p, pc int, k uint32) tval {
		name, site := get.A, get.B
		this, local, n := recv.this, recv.k, len(args)
		return func(c *tctx) Value {
			o := c.f.this
			if !this {
				o = c.locals[local]
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
			r := c.r
			as := c.stack[p+1 : p+1+n]
			rx := as[0]
			done := m.IsObject() && rx.IsObject() && r.regexpArgCall(m.object(), o, as)
			v := regexpCallMethod(c, m, o, as)
			if done {
				r.regexpLiteralDone(&c.cl.fn.Constants[k], rx.object())
			}
			return v
		}
	}

	regexpLitCallNode = func(f tval, k uint32, test bool, args []tval, p, pc int) tval {
		n := len(args)
		return func(c *tctx) Value {
			r := c.r
			// The method is read before the arguments are evaluated, as
			// get_prop_this reads it: where it is the built-in, with
			// nothing run, it is not read at all.
			builtin := r.regexpLitMethod(test)
			var fv Value
			if builtin != nil {
				fv = Obj(builtin)
			} else {
				fv = f(c)
			}
			for i, a := range args {
				c.stack[p+1+i] = a(c)
			}
			c.at(pc)
			recv, as := c.stack[p-1], c.stack[p+1:p+1+n]
			// What the call runs: nothing of the script's where its
			// argument is a primitive, and, for test, its exec is the
			// built-in now, which it reads as it is called.
			done := builtin != nil && (n == 0 || !as[0].IsObject()) && (!test || r.regexpLitMethod(false) != nil)
			if fv.IsObject() {
				o := fv.object()
				fd := o.fn()
				if fd != nil && fd.treeCall != nil && fd.closure.realm == r.Realm {
					if err := r.tick(); err != nil {
						c.throw(err)
					}
					v, err := r.callTree(o, fd, recv, as, true)
					if err != nil {
						c.throw(err)
					}
					return v
				}
				v, err := r.callRest(o, fd, recv, as)
				if err != nil {
					c.throw(err)
				}
				if done && recv.IsObject() {
					r.regexpLiteralDone(&c.cl.fn.Constants[k], recv.object())
				}
				return v
			}
			v, err := r.call(fv, recv, as)
			if err != nil {
				c.throw(err)
			}
			return v
		}
	}
}
