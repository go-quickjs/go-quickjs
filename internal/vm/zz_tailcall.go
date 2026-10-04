package vm

import "github.com/go-quickjs/go-quickjs/internal/bytecode"

// The tree tier's calls in tail position are built here, in the file the
// package's code ends with, rather than beside op's calls: the tier's
// closures are laid out in the order they are written, and NavierStokes,
// whose code has no tail call, ran 6 to 10% slower with this built inside
// op and tree_build.go, and as fast as before with it here.

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
// runTree to leave with errTailCall rather than the value returned. A tree
// has no handlers and no saved stack, so what decides, besides the depth,
// is that the frame is not a constructor's, whose result is checked after
// its body, nor one that shares a derived constructor's this. Anything
// else is an ordinary call.
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
		v, err := r.callFromLoop(c.stack[p], recv, args)
		if err != nil {
			c.throw(err)
		}
		return v
	}
}
