package vm

import "sync/atomic"

// The tree tier's call in tail position of another function, past
// tailCallDepth, made in the frame that gives itself up: its function,
// its locals and its operand window become the callee's, and the callee's
// tree runs here, as a function calling itself starts again in its frame
// (ztree_zkself.go). Two functions calling each other in tail position --
// a state machine's states, a parser's mutually recursive rules, even and
// odd -- had each call leave its frame, through runTreeNested's errTailCall
// and tailFromTree, for a frame of the callee's own, made and let go of at
// every call.
//
// The return a frame gives itself up at is restartReturn, which calls
// tailSwap where its call is not of the frame's own function. tailSwap
// runs the callee's tree, and the callee's return in tail position, back
// in tailSwap's loop, goes on to the next callee: tailSwap marks the frame
// it runs (Runtime.tailSwapDepth), and a return under it leaves its call to
// it. A callee it does not make so is left as before, in pendingTail, for
// the frame's caller to make.
//
// The code is closures assigned in this file's init, the package's first
// init with closures, whose closures are laid out after every other
// closure (see ztree_zkself.go): code added here moves nothing the tree
// tier or the interpreter runs.

// tailSwap makes the call in tail position in pendingTail in c's frame,
// where the callee may run there, its result left in c.ret and treeTail
// cleared; anything else is left in pendingTail, with treeTail set, for
// the frame's caller to make. The callee may run in the frame where it is a closure of
// this realm run as a tree (planTreeCall's treeCall or treeRest) and, as
// selfRestart asks of a function started again, strict, with no arguments
// object, no rest parameter and simple parameters -- whose start takes
// them from its locals, not from args -- no arrow, generator, async
// function or class constructor; and where the frame is no constructor's
// and shares no derived constructor's this, which its caller checks after
// it returns, and native code does not run (the JIT's frames are its own).
var tailSwap func(c *tctx)

func init() {
	tailSwap = func(c *tctx) {
		r, f := c.r, c.f
		depth := int32(r.frameDepth)
		if r.tailSwapDepth == depth || r.jitOn() || f.thisRef != nil || !f.newTarget.IsUndefined() {
			// Under tailSwap's loop, which makes the call; or not a frame
			// to give up so.
			return
		}
		saved := r.tailSwapDepth
		r.tailSwapDepth = depth
		defer func() { r.tailSwapDepth = saved }()
		for r.treeTail {
			t := &r.pendingTail
			if !t.callee.IsObject() {
				return
			}
			o := t.callee.object()
			fd := o.fn()
			if fd == nil || fd.native != nil || fd.bound || fd.arrow || fd.closure == nil ||
				fd.closure.realm != r.Realm {
				return
			}
			cl := fd.closure
			fn := cl.fn
			if !fn.Strict || fn.UsesArguments || fn.HasRest || !fn.HasSimpleParams ||
				fn.Generator || fn.Async || isClassConstructorKind(fn.Kind) {
				return
			}
			if !fd.treePlanned {
				// A callee first called in this chain, whose calls are
				// planned once the first returns.
				planTreeCall(fd)
			}
			tr := fd.treeCall
			if tr == nil {
				if !fd.treeRest {
					return
				}
				tr = (*tree)(atomic.LoadPointer(&fn.VMCode))
				if tr == nil || tr == noTree {
					return
				}
			}
			// The frame's window: its locals, then its operand stack.
			base := f.base - len(f.locals)
			if base < 0 || len(f.locals) != 0 && &f.locals[0] != &r.stack[base] {
				return
			}
			top := base + fn.LocalCount + fn.MaxStack
			if top > len(r.stack) {
				return
			}
			if err := r.tick(); err != nil {
				c.throw(err)
			}
			if r.stopped != nil {
				c.throw(r.stopped)
			}
			this, args := t.this, t.args
			r.treeTail = false
			*t = tailCall{}

			// The frame given up, as popFrameOf gives it up: the bindings
			// closures captured are closed before its slots are reused.
			for _, u := range f.openUpvalues {
				u.close()
			}
			if len(f.openUpvalues) != 0 {
				f.openUpvalues = f.openUpvalues[:0]
			}
			if len(f.handlers) != 0 {
				f.handlers = f.handlers[:0]
			}
			// And the callee's, as callTreeWith makes it. The arguments are
			// not in the window: tailGiveUp copied them out.
			r.stackTop = top
			if top > r.stackHigh {
				r.stackHigh = top
			}
			locals := r.stack[base : base+fn.LocalCount : base+fn.LocalCount]
			m := min(fn.ParamCount, len(locals), len(args))
			for i := 0; i < m; i++ {
				locals[i] = args[i]
			}
			for i := m; i < len(locals); i++ {
				locals[i] = Undefined
			}
			f.cl = cl
			f.locals = locals
			f.base = base + fn.LocalCount
			f.pc = 0
			f.this = this
			f.callee = o
			f.args = args
			c.cl, c.locals = cl, locals
			c.closureCode = cl.closureCode
			c.stack = r.stack[f.base : f.base+fn.MaxStack]

			// The callee's tree, run as runTreeNested runs one: its own call
			// in tail position, if it ends in one, is the loop's next turn.
			blocks := tr.blocks
			for b := 0; b >= 0; {
				blk := &blocks[b]
				for _, s := range blk.body {
					s(c)
				}
				b = blk.next(c)
			}
		}
	}
}
