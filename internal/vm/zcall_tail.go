package vm

import "github.com/go-quickjs/go-quickjs/internal/bytecode"

// The tree tier's calls in tail position are built here, in the file the
// package's code ends with -- the z puts it after with.go -- rather than
// beside op's calls: the tier's closures are laid out in the order they are
// written, and NavierStokes, whose code has no tail call, ran 6 to 10%
// slower with this built inside op and tree_build.go, and as fast as before
// with it here.

// tailCall builds a call in tail position as op builds an ordinary one: the
// callee, the receiver of a method and the arguments go to their slots.
func (b *tbuilder) tailCall(in bytecode.Instr, pc int) bool {
	if !b.spill() {
		return false
	}
	n := int(in.A)
	p := b.depth() - n - 1
	this := -1
	if in.Op == bytecode.OpTailCallMethod {
		this = p - 1
	}
	for i := 0; i <= n; i++ {
		b.pop()
	}
	if this >= 0 {
		b.pop()
	}
	b.push(tailCallNode(p, n, this, pc))
	return true
}

// tailCallNode is a call in tail position, after which nothing runs but
// jumps to a return and the return. Deep enough in the stack, the frame is
// given up for the callee's, as the interpreter's tail_call gives it up:
// the call is left in pendingTail for run to make, and treeTail tells
// runTree to leave with errTailCall rather than the value returned. The
// compiler emits no tail call under a handler or with a live cursor. Besides
// the depth, what decides is that the frame is not a constructor's, whose
// result is checked after its body, nor one that shares a derived
// constructor's this. Anything else is an ordinary call.
//
// It is kept out of line, as the node builders tbuilder.op calls are: a
// builder inlined into op makes its closure part of a function big
// enough that Go inlines almost nothing into it, and every helper the
// node calls at run time -- c.at, Value.IsObject -- becomes a call.
//
//go:noinline
func tailCallNode(p, n, this, pc int) tval {
	return func(c *tctx) Value {
		c.at(pc)
		r, f := c.r, c.f
		recv := Undefined
		if this >= 0 {
			recv = c.stack[this]
		}
		args := c.stack[p+1 : p+1+n]
		if r.frameDepth >= tailCallDepth && f.newTarget.IsUndefined() && f.thisRef == nil {
			// The arguments are copied: they are in this frame's window,
			// which the callee's frame is about to reuse.
			r.pendingTail = tailCall{callee: c.stack[p], this: recv, args: append([]Value(nil), args...)}
			r.treeTail = true
			return Undefined
		}
		v, err := r.tailCallShallow(c.stack[p], recv, args)
		if err != nil {
			c.throw(err)
		}
		return v
	}
}

// tailCallShallow is a call in tail position shallower than the depth a
// frame is given up at, an ordinary call, made as a call node makes one: a
// callee planTreeCall has given a tree on it, any other by callRest. A
// recursion through tail calls, each frame its own, had been made through
// callDirect and runFD down to the depth. It is called where tailCallNode
// had called callDirect, a call as long, so that the node's code, which
// the closures after it are laid out by, is as long as it was.
func (r *Runtime) tailCallShallow(callee, this Value, args []Value) (Value, error) {
	if !callee.IsObject() {
		return r.callDirect(callee, this, args)
	}
	o := callee.object()
	fd := o.fn()
	if fd != nil && fd.treeCall != nil && fd.closure.realm == r.Realm {
		if err := r.tick(); err != nil {
			return Undefined, err
		}
		return r.callTree(o, fd, this, args, true)
	}
	return r.callRest(o, fd, this, args)
}
