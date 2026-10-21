package vm

import "github.com/go-quickjs/go-quickjs/internal/bytecode"

// Calls in tail position built as callAll builds a call: the callee and
// the arguments evaluated by the call's node, into the slots tailCall's
// spill would have put them in, rather than by a statement of the spill's
// for each before it. A strict function's return f(x) -- a class's
// methods', a module's -- had been a block of its own: a statement for
// each operand, then the return, of a value the call left in its slot;
// and return c ? a : f(x), whose f(x) was then no expression, three. Here
// the call is one node, the return's value, and the conditional around
// it one node too (ternaryNode). The call itself is made as tailCallNode
// makes it: deep enough, the frame is given up, the call left in
// pendingTail for the return to restart the function with or for run to
// make; shallower, it is made the ordinary way, by tailCallShallow. A
// call of recv.name(args) on a receiver read in place is methodCall's,
// the method looked up by the node.
//
// The code is closures assigned in this file's init, the package's first
// init with closures, whose closures are laid out after every other
// closure (see ztree_zkself.go): code added here moves nothing the tree
// tier or the interpreter runs.

var (
	// tailAll builds tail_call or tail_call_method at pc into
	// tailOperands's or tailMethod's node, as callAll and methodCall build
	// a call, and reports false where it does not -- a receiver that is
	// not in its slot -- which tailCall then builds after a spill.
	tailAll func(b *tbuilder, in bytecode.Instr, pc int) bool

	// tailOperands is the call in tail position at pc of the callee f
	// with args, evaluated into slots p+1..., and the receiver of a method
	// in slot this, or none where this is -1.
	tailOperands func(f tval, args []tval, p, this, pc int) tval

	// tailMethod is recv.name(args) in tail position, for a get_prop_this
	// at gpc of recv read in place, with the arguments to evaluate into
	// slots p+1..., and the tail_call_method at pc.
	tailMethod func(recv tentry, get bytecode.Instr, gpc int, args []tval, p, pc int) tval

	// tailGiveUp leaves the call of fv with this rv and args as in
	// pendingTail, past tailCallDepth, for the frame to give itself up
	// to, as tailCallNode does, and reports whether it did. The arguments
	// are in the frame's window, which the callee's frame reuses: for a
	// callee that takes them as it starts and never again -- strict, with
	// no arguments object and simple parameters, no default, pattern or
	// rest parameter among them -- they are copied into tailArgs, as
	// tailArgsOf copies them, one by one; anything else has a copy of its
	// own.
	tailGiveUp func(c *tctx, fv, rv Value, args []Value) bool
)

func init() {
	tailGiveUp = func(c *tctx, fv, rv Value, args []Value) bool {
		r, f := c.r, c.f
		if r.frameDepth < tailCallDepth || !f.newTarget.IsUndefined() || f.thisRef != nil {
			return false
		}
		var kept []Value
		if fv.IsObject() {
			if fd := fv.object().fn(); fd != nil && fd.closure != nil && !fd.bound {
				if fn := fd.closure.fn; fn.Strict && !fn.UsesArguments && fn.HasSimpleParams {
					kept = r.tailArgs[:0]
					for _, a := range args {
						kept = append(kept, a)
					}
					r.tailArgs = kept
					kept = kept[:len(args):len(args)]
				}
			}
		}
		if kept == nil {
			kept = append([]Value(nil), args...)
		}
		r.pendingTail = tailCall{callee: fv, this: rv, args: kept}
		r.treeTail = true
		return true
	}

	tailOperands = func(f tval, args []tval, p, this, pc int) tval {
		n := len(args)
		return func(c *tctx) Value {
			fv := f(c)
			for i, a := range args {
				c.stack[p+1+i] = a(c)
			}
			c.at(pc)
			rv := Undefined
			if this >= 0 {
				rv = c.stack[this]
			}
			as := c.stack[p+1 : p+1+n]
			if tailGiveUp(c, fv, rv, as) {
				return Undefined
			}
			v, err := c.r.tailCallShallow(fv, rv, as)
			if err != nil {
				c.throw(err)
			}
			return v
		}
	}

	tailMethod = func(recv tentry, get bytecode.Instr, gpc int, args []tval, p, pc int) tval {
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
			if tailGiveUp(c, m, o, as) {
				return Undefined
			}
			v, err := c.r.tailCallShallow(m, o, as)
			if err != nil {
				c.throw(err)
			}
			return v
		}
	}

	tailAll = func(b *tbuilder, in bytecode.Instr, pc int) bool {
		n := int(in.A)
		p := len(b.stack) - n - 1
		if in.Op == bytecode.OpTailCallMethod {
			if m := b.stack[p].method; m != nil {
				// recv.name(args), the receiver read in place beneath the
				// method entry.
				args := make([]tval, n)
				for i := n - 1; i >= 0; i-- {
					args[i] = b.pop().tree()
				}
				b.pop()
				recv := b.pop()
				b.push(tailMethod(recv, m.in, m.pc, args, b.depth()+1, pc))
				return true
			}
			if p < 1 || b.stack[p-1].slot != p-1 {
				return false
			}
		}
		args := make([]tval, n)
		for i := n - 1; i >= 0; i-- {
			args[i] = b.pop().tree()
		}
		f := b.pop().tree()
		this := -1
		if in.Op == bytecode.OpTailCallMethod {
			// The receiver, in its slot beneath the method, is read there.
			b.pop()
			this = p - 1
		}
		b.push(tailOperands(f, args, p, this, pc))
		return true
	}
}
