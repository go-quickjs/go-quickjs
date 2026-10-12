package vm

import "github.com/go-quickjs/go-quickjs/internal/bytecode"

// Using declarations in the tree tier.
//
// The compiler makes a scope with using declarations a try statement whose
// finally clause disposes of what the scope bound (internal/compiler's
// using.go), which the tree tier already builds; what is left is the three
// instructions of the resources themselves. Each node does what executeAt's
// case does: a new capability, a value added to it, and the disposal, with
// the exception in flight that a finally clause's record says is leaving
// folded into what it throws, and, for await using, a promise of the
// disposal for the await after it.
//
// Its functions are variables, each assigned a closure in init, rather
// than functions or methods: Go lays out all of a package's functions
// before any of its closures, so code added here as functions would sit
// between the interpreter's hot functions and the tree nodes' hot
// closures, and move one against the other, where a closure is laid out
// after every closure already there (see AGENTS.md). The zz sorts it after
// the tier's other files, so that its closures follow theirs.

var (
	// treeBuildsUsing reports whether the tier builds op, one of a using
	// declaration's.
	treeBuildsUsing func(op bytecode.Op) bool

	// usingOp builds new_dispose_capability, add_disposable and dispose.
	usingOp func(b *tbuilder, pc int, in bytecode.Instr) (int, bool)
)

func init() {
	treeBuildsUsing = func(op bytecode.Op) bool {
		switch op {
		case bytecode.OpNewDisposeCapability, bytecode.OpAddDisposable, bytecode.OpDispose:
			return true
		}
		return false
	}

	usingOp = func(b *tbuilder, pc int, in bytecode.Instr) (int, bool) {
		switch in.Op {
		case bytecode.OpNewDisposeCapability:
			b.push(func(*tctx) Value {
				o := newObject(nil, ClassObject)
				o.data = &disposeCapability{}
				return Obj(o)
			})
		case bytecode.OpAddDisposable:
			// The capability on top goes; the value beneath it stays.
			if !b.spill() || b.depth() < 2 {
				return 0, false
			}
			d, async := b.depth(), in.A == 1
			b.pop()
			return 0, b.stmt(func(c *tctx) {
				cp := c.stack[d-1].Object().data.(*disposeCapability)
				c.at(pc)
				if err := c.r.addDisposableResource(cp, c.stack[d-2], async); err != nil {
					c.throw(err)
				}
			})
		case bytecode.OpDispose:
			// The capability on top goes. A finally clause's record, where
			// the clause is one, is beneath it, and stays.
			record, async := in.A&bytecode.DisposeRecord != 0, in.A&bytecode.DisposeAsync != 0
			if !b.spill() || b.depth() < 1 || record && b.depth() < 3 {
				return 0, false
			}
			d := b.depth()
			b.pop()
			b.body = append(b.body, func(c *tctx) {
				cp := c.stack[d-1].Object().data.(*disposeCapability)
				c.at(pc)
				var completion error
				if record && completionKind(c.stack[d-2].Number()) == completionThrow {
					completion = c.r.throw(c.stack[d-3])
				}
				// A scope whose await using was never reached disposes of
				// what it has without awaiting anything, and says so with
				// undefined.
				if async && cp.hasAsync() {
					r := c.r
					p := r.newPromise()
					r.disposeResourcesAsync(cp, completion, func(err error) {
						// The exception in flight is still what the record
						// says; only a new one rejects.
						if err == nil || err == completion {
							r.resolvePromise(p, Undefined)
							return
						}
						r.rejectPromise(p, thrownValue(err))
					})
					c.stack[d-1] = Obj(p)
					return
				}
				if err := c.r.disposeResources(cp, completion); err != nil && err != completion {
					c.throw(err)
				}
				if async {
					c.stack[d-1] = Undefined
				}
			})
			if async {
				b.pushSlot(d - 1)
			}
		default:
			return 0, false
		}
		return 0, true
	}
}
