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
// folded into what it throws. An await using scope's disposal is a loop of
// steps with the compiler's awaits between them, a step that may end the
// loop ending its block.
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
	// declaration's, or one treeBuildsImport names.
	treeBuildsUsing func(op bytecode.Op) bool

	// usingOp builds new_dispose_capability, add_disposable and dispose,
	// but for the steps of an await using scope's disposal that may end its
	// loop (see usingEnd), and hands importOp what treeBuildsImport names.
	usingOp func(b *tbuilder, pc int, in bytecode.Instr) (int, bool)

	// usingEnd ends block bi with a step of an await using scope's disposal
	// that goes on to the await after it with the value to await, or, with
	// nothing left to await, jumps to where the disposal ends.
	usingEnd func(b *tbuilder, t *tree, bi, pc int, in bytecode.Instr) ([]int, bool)

	// usingStep is a step of an await using scope's disposal, for both
	// tiers, as the specification's DisposeResources walks it, the awaits
	// it makes being the compiler's between the steps. cp is the scope's
	// capability. The first step, with DisposeStart in mode, takes the
	// resources, and, with DisposeRecord, the completion the finally
	// clause's record beneath -- a the value, b the kind -- says the scope
	// is leaving with. One with DisposeFail folds a, what the await before
	// it threw, into the completion. Each runs the disposers, last added
	// first, until one leaves a value to await, which it reports, or none
	// is left, when it reports false. The end, with DisposeFinish, throws
	// what the disposers threw where it is not what the scope was already
	// leaving with.
	usingStep func(r *Runtime, cp, a, b Value, mode uint32) (Value, bool, error)
)

func init() {
	treeBuildsUsing = func(op bytecode.Op) bool {
		switch op {
		case bytecode.OpNewDisposeCapability, bytecode.OpAddDisposable, bytecode.OpDispose:
			return true
		}
		return treeBuildsImport(op)
	}

	usingOp = func(b *tbuilder, pc int, in bytecode.Instr) (int, bool) {
		switch in.Op {
		case bytecode.OpNewDisposeCapability:
			b.push(func(*tctx) Value {
				o := newObject(nil, ClassObject)
				o.extOf().data = &disposeCapability{}
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
				cp := c.stack[d-1].Object().dataOf().(*disposeCapability)
				c.at(pc)
				if err := c.r.addDisposableResource(cp, c.stack[d-2], async); err != nil {
					c.throw(err)
				}
			})
		case bytecode.OpDispose:
			if in.B&bytecode.DisposeStep != 0 {
				// The end of an await using scope's disposal.
				if in.B&bytecode.DisposeFinish == 0 || !b.spill() || b.depth() < 1 {
					return 0, false
				}
				d, mode := b.depth(), in.B
				b.pop()
				return 0, b.stmt(func(c *tctx) {
					c.at(pc)
					if _, _, err := usingStep(c.r, c.stack[d-1], Undefined, Undefined, mode); err != nil {
						c.throw(err)
					}
				})
			}
			// The capability on top goes. A finally clause's record, where
			// the clause is one, is beneath it, and stays.
			record, async := in.A&bytecode.DisposeRecord != 0, in.A&bytecode.DisposeAsync != 0
			if !b.spill() || b.depth() < 1 || record && b.depth() < 3 {
				return 0, false
			}
			d := b.depth()
			b.pop()
			b.body = append(b.body, func(c *tctx) {
				cp := c.stack[d-1].Object().dataOf().(*disposeCapability)
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
			return importOp(b, pc, in)
		}
		return 0, true
	}

	usingEnd = func(b *tbuilder, t *tree, bi, pc int, in bytecode.Instr) ([]int, bool) {
		mode := in.B
		fail, record := mode&bytecode.DisposeFail != 0, mode&bytecode.DisposeStart != 0 && mode&bytecode.DisposeRecord != 0
		n := 1
		if fail {
			n = 2
		}
		if !b.spill() || b.depth() < n || record && b.depth() < 3 || int(in.A) <= pc {
			return nil, false
		}
		d := b.depth()
		for range n {
			b.pop()
		}
		taken, ok := b.target(int(in.A), d-n)
		if !ok {
			return nil, false
		}
		fall, ok := b.target(pc+1, d-n+1)
		if !ok {
			return nil, false
		}
		t.blocks[bi] = tblock{body: b.takeBody(), next: func(c *tctx) int {
			var x, y Value
			if fail {
				x = c.stack[d-2]
			} else if record {
				x, y = c.stack[d-3], c.stack[d-2]
			}
			c.at(pc)
			v, more, err := usingStep(c.r, c.stack[d-1], x, y, mode)
			if err != nil {
				c.throw(err)
			}
			if !more {
				return taken
			}
			c.stack[d-n] = v
			return fall
		}}
		return b.succ, true
	}

	usingStep = func(r *Runtime, cp, a, b Value, mode uint32) (Value, bool, error) {
		c := cp.Object().dataOf().(*disposeCapability)
		switch {
		case mode&bytecode.DisposeStart != 0:
			c.walk, c.stack = c.stack, nil
			c.next = len(c.walk) - 1
			c.needsAwait, c.hasAwaited, c.completion = false, false, nil
			if mode&bytecode.DisposeRecord != 0 && completionKind(b.Number()) == completionThrow {
				c.completion = r.throw(a)
			}
			c.leaving = c.completion
		case mode&bytecode.DisposeFail != 0:
			// What an async disposer returned rejected.
			c.completion = r.suppress(c.completion, r.throw(a))
		case mode&bytecode.DisposeFinish != 0:
			err, leaving := c.completion, c.leaving
			c.walk, c.completion, c.leaving = nil, nil, nil
			if err != nil && err != leaving {
				return Undefined, false, err
			}
			return Undefined, false, nil
		}
		for c.next >= 0 {
			res := c.walk[c.next]
			if !res.async && c.needsAwait && !c.hasAwaited {
				// A sync disposer after an await using of nothing waits
				// for the await it owed.
				c.needsAwait = false
				return Undefined, true, nil
			}
			c.next--
			if res.method.IsUndefined() {
				c.needsAwait = true
				continue
			}
			v, err := r.call(res.method, res.value, nil)
			if err != nil {
				c.completion = r.suppress(c.completion, err)
				if _, ok := err.(*Thrown); !ok {
					c.next = -1
					return Undefined, false, err
				}
				continue
			}
			if res.async {
				c.hasAwaited = true
				return v, true, nil
			}
		}
		if c.needsAwait && !c.hasAwaited {
			// An await using of nothing still awaits once.
			c.needsAwait = false
			return Undefined, true, nil
		}
		return Undefined, false, nil
	}
}
