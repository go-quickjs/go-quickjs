package vm

// The tree tier's start again of a function calling itself in tail
// position, past tailCallDepth: its frame is the callee's, its parameters
// rebound (selfRestart), and its return goes back to its first block. A
// recursion through such calls is then a loop, as the JIT's OpTailSelf
// makes it, where each call had left its frame for one of its own.
//
// The call is left as any call in tail position past the depth is, in
// pendingTail (tailCallNode), and the return it is in (restartReturn, the
// return buildBlock builds in a function with a call in tail position)
// takes it from there when it is the frame's own function's. Nothing the
// other calls and returns run is changed: tailCallNode, tailArgsOf and
// every other function's return are as they were, so that no hot code
// moves against other hot code. Code grown in zcall_tail.go moved
// zcall_tree.go's callTree and runTreeNested against runTree and the
// closures, and a return closure resized moved the closures after it
// against those before, which the placements do not vary: Queens, which
// makes no call in tail position, was 4% slower.
//
// Both are closures assigned in this file's init, whose closures are laid
// out after every other closure of the package's: the compiler lays out the
// closures of the inits last of all, the last init's first, so the first
// init with closures -- this, before ztree_zlexical.go's -- has its
// closures last.

// selfRestart starts the function c's frame runs again, with this and args,
// where it may be, and reports whether it did. It may be where the function
// is strict, with no arguments object and no rest parameter, which would
// read the frame's args after it starts, and simple parameters, which its
// start takes from its locals rather than, past a default, from its args,
// which are the first call's; no arrow, whose this is not the call's; no
// generator or async function; and a frame with no binding a closure has
// open, which the started body would write over, no new.target and no
// derived constructor's this.
var selfRestart func(r *Runtime, c *tctx, this Value, args []Value) bool

// restartReturn is buildBlock's return of v at pc, from a stack d deep, in
// a function with a call in tail position: where the value is that of a
// call of the frame's own function left in pendingTail, and selfRestart
// starts it again, it goes back to the first block rather than return.
var restartReturn func(v tval, d, pc int) func(*tctx) int

func init() {
	selfRestart = func(r *Runtime, c *tctx, this Value, args []Value) bool {
		f := c.f
		if f.thisRef != nil || !f.newTarget.IsUndefined() || len(f.openUpvalues) != 0 {
			return false
		}
		fd := f.callee.fn()
		fn := c.cl.fn
		if fd == nil || fd.closure != c.cl || fd.arrow || !fn.Strict || fn.UsesArguments || fn.HasRest ||
			!fn.HasSimpleParams || fn.Generator || fn.Async {
			return false
		}
		if err := r.tick(); err != nil {
			c.throw(err)
		}
		m := min(fn.ParamCount, len(args))
		copy(c.locals[:m], args[:m])
		for i := m; i < len(c.locals); i++ {
			c.locals[i] = Undefined
		}
		f.this = this
		return true
	}
	restartReturn = func(v tval, d, pc int) func(*tctx) int {
		return func(c *tctx) int {
			res := v(c)
			if r := c.r; r.treeTail {
				if t := &r.pendingTail; t.callee.IsObject() && t.callee.object() == c.f.callee &&
					selfRestart(r, c, t.this, t.args) {
					r.treeTail = false
					*t = tailCall{}
					return 0
				}
			}
			if d > 0 {
				c.at(pc)
				if err := c.r.closeIteratorsReturning(c.f.base, c.f.base+d); err != nil {
					c.throw(err)
				}
			}
			if c.f.thisRef != nil {
				c.at(pc)
				var err error
				if res, err = c.r.derivedResult(c.f, c.cl, res); err != nil {
					c.throw(err)
				}
			}
			c.ret = res
			return -1
		}
	}
}
