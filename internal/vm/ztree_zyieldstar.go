package vm

import "github.com/go-quickjs/go-quickjs/internal/bytecode"

// yield* and for await in the tree tier.
//
// Both are loops the compiler writes out as instructions around a cursor in
// a stack slot, as for...of is (see ztree_zof.go), each turn of which
// suspends the frame at least once.
//
// yield* starts a cursor over its operand, and then, from a stack of the
// cursor, the value sent in and the kind of resumption, iter_resume calls
// the delegate's next, throw or return with the value, an async generator
// awaits what that returned, iter_unpack_delegate reads the result, and
// yield_star hands it out and suspends: a suspension that ends a block as
// yield's does (see suspendEnd), whose resumption pushes the kind of
// resumption above the value sent in rather than acting on it
// (runDelegating). The jump back to iter_resume follows it. Each of the
// three is a node that does what the interpreter's case does, calling the
// helper it calls, and the two that can finish the delegation end a block,
// going on to the code after it, which decides what the delegate's last
// value means, or on round the loop.
//
// for await starts a cursor as for...of does, and each turn async_iter_next
// asks for a promise of the next result, await suspends until it settles,
// and iter_result_or_jump ends a block, going on with the value or to
// where the loop ends. A loop left abruptly closes its cursor as a for...of
// loop's is closed.
//
// Its functions are variables, each assigned a closure in init, rather
// than functions or methods: Go lays out all of a package's functions
// before any of its closures, so code added here as functions would sit
// between the interpreter's hot functions and the tree nodes' hot
// closures, and move one against the other, where a closure is laid out
// after every closure already there (see AGENTS.md).
var (
	// treeBuildsDelegate reports whether the tier builds op, one of
	// yield*'s or for await's instructions other than the suspensions, or
	// one treeBuildsUsing names.
	treeBuildsDelegate func(op bytecode.Op) bool

	// delegateOp builds for_await_of_start, async_iter_next and
	// async_iter_close's check and mark, each of which runs code, and is a
	// statement: what it pushes is written to its slot. It hands usingOp
	// what treeBuildsUsing names.
	delegateOp func(b *tbuilder, pc int, in bytecode.Instr) (int, bool)

	// delegateEnd ends block bi with iter_resume, iter_unpack_delegate,
	// iter_result_or_jump or async_iter_close, which go on to the next
	// instruction or, where the delegation or the loop is over, or an
	// iterator has nothing to close with, jump forward.
	delegateEnd func(b *tbuilder, t *tree, bi, pc int, in bytecode.Instr) ([]int, bool)

	// asyncIterClose is async_iter_close, for both tiers: with mode 0, the
	// call of the return method of the iterator of v, a for await's cursor,
	// which reports what it returned, or false where an asynchronous
	// iterator has none; with mode 1, the check of the result awaited, v;
	// with mode 2, v marked done, unclosed.
	asyncIterClose func(r *Runtime, v Value, mode uint32) (Value, bool, error)
)

func init() {
	treeBuildsDelegate = func(op bytecode.Op) bool {
		switch op {
		case bytecode.OpForAwaitOfStart, bytecode.OpAsyncIterNext, bytecode.OpAsyncIterClose,
			bytecode.OpIterResume, bytecode.OpIterUnpackDelegate, bytecode.OpIterResultOrJump:
			return true
		}
		return treeBuildsUsing(op)
	}

	delegateOp = func(b *tbuilder, pc int, in bytecode.Instr) (int, bool) {
		switch in.Op {
		case bytecode.OpForAwaitOfStart:
			v := b.pop().tree()
			if !b.spill() {
				return 0, false
			}
			d := b.depth()
			b.body = append(b.body, func(c *tctx) {
				x := v(c)
				c.at(pc)
				cur, err := c.r.startForAwaitOf(x)
				if err != nil {
					c.throw(err)
				}
				c.stack[d] = cur
			})
			b.pushSlot(d)
		case bytecode.OpAsyncIterNext:
			// The cursor stays beneath the promise pushed.
			if !b.spill() || b.depth() == 0 {
				return 0, false
			}
			d := b.depth()
			b.body = append(b.body, func(c *tctx) {
				c.at(pc)
				v, err := c.r.asyncIterNext(c.stack[d-1])
				if err != nil {
					c.throw(err)
				}
				c.stack[d] = v
			})
			b.pushSlot(d)
		case bytecode.OpAsyncIterClose:
			// The check of a result awaited, or a cursor marked done; the
			// call, which may jump, ends its block (see delegateEnd).
			if in.B == 0 {
				return 0, false
			}
			v, mode := b.pop().tree(), in.B
			return 0, b.stmt(func(c *tctx) {
				x := v(c)
				c.at(pc)
				if _, _, err := asyncIterClose(c.r, x, mode); err != nil {
					c.throw(err)
				}
			})
		default:
			return usingOp(b, pc, in)
		}
		return 0, true
	}

	delegateEnd = func(b *tbuilder, t *tree, bi, pc int, in bytecode.Instr) ([]int, bool) {
		switch in.Op {
		case bytecode.OpIterResume:
			// stack: cursor sent kind. The result replaces the two, going on
			// to the next instruction, or, where a return ended the
			// delegation, to the code after it.
			if b.depth() < 3 || int(in.B) <= pc {
				return nil, false
			}
			kind := b.pop().tree()
			sent := b.pop().tree()
			if !b.spill() {
				return nil, false
			}
			d := b.depth()
			taken, ok := b.target(int(in.B), d+1)
			if !ok {
				return nil, false
			}
			fall, ok := b.target(pc+1, d+1)
			if !ok {
				return nil, false
			}
			async := in.A == 1
			t.blocks[bi] = tblock{body: b.takeBody(), next: func(c *tctx) int {
				s := sent(c)
				k := kind(c)
				c.at(pc)
				mode := resumeMode(k.Number())
				if !async && mode == resumeNext && delegateNext(c, d, s) {
					// A generator stepped directly (see ztree_zzzdelegate.go).
					return fall
				}
				res, done, err := c.r.iterResume(c.stack[d-1], s, mode, async)
				if err != nil {
					c.throw(err)
				}
				c.stack[d] = res
				if done {
					return taken
				}
				return fall
			}}
			return b.succ, true
		case bytecode.OpIterUnpackDelegate:
			// The delegate's result, above the cursor, is replaced by what is
			// yielded -- the result itself in a synchronous delegation -- or,
			// once the delegate is done, by its value, at the code after the
			// delegation.
			if b.depth() < 2 || int(in.A) <= pc {
				return nil, false
			}
			res := b.pop().tree()
			if !b.spill() {
				return nil, false
			}
			d := b.depth()
			taken, ok := b.target(int(in.A), d+1)
			if !ok {
				return nil, false
			}
			fall, ok := b.target(pc+1, d+1)
			if !ok {
				return nil, false
			}
			raw := in.B&1 == 1
			t.blocks[bi] = tblock{body: b.takeBody(), next: func(c *tctx) int {
				x := res(c)
				c.at(pc)
				if !x.IsObject() {
					c.throw(c.r.throwTypeError("an iterator result must be an object"))
				}
				if cur := c.stack[d-1]; x.ref == cur.ref {
					// The cursor, standing for what delegateNext stepped the
					// delegate to: the value is above it, and a delegate that
					// has finished has its cursor marked done. A raw yield
					// is left the mark, for yield_star to yield the value.
					v := c.stack[d+1]
					if iterStateOf(cur).done {
						c.stack[d] = v
						return taken
					}
					if !raw {
						c.stack[d] = v
					}
					return fall
				}
				done, err := c.r.getValueProp(x, atomDone)
				if err != nil {
					c.throw(err)
				}
				if !done.Truthy() {
					if !raw {
						if x, err = c.r.getValueProp(x, atomValue); err != nil {
							c.throw(err)
						}
					}
					c.stack[d] = x
					return fall
				}
				v, err := c.r.getValueProp(x, atomValue)
				if err != nil {
					c.throw(err)
				}
				if st := iterStateOf(c.stack[d-1]); st != nil {
					st.done = true
				}
				c.stack[d] = v
				return taken
			}}
			return b.succ, true
		case bytecode.OpIterResultOrJump:
			// The result a for await awaited, above the cursor, is replaced by
			// its value, or, once the iterator is done, the loop ends with
			// the cursor still there.
			if b.depth() < 2 || int(in.A) <= pc {
				return nil, false
			}
			res := b.pop().tree()
			if !b.spill() {
				return nil, false
			}
			d := b.depth()
			taken, ok := b.target(int(in.A), d)
			if !ok {
				return nil, false
			}
			fall, ok := b.target(pc+1, d+1)
			if !ok {
				return nil, false
			}
			t.blocks[bi] = tblock{body: b.takeBody(), next: func(c *tctx) int {
				x := res(c)
				c.at(pc)
				if !x.IsObject() {
					c.throw(c.r.throwTypeError("an iterator result must be an object"))
				}
				done, err := c.r.getValueProp(x, atomDone)
				if err != nil {
					c.throw(err)
				}
				if done.Truthy() {
					return taken
				}
				v, err := c.r.getValueProp(x, atomValue)
				if err != nil {
					c.throw(err)
				}
				c.stack[d] = v
				return fall
			}}
			return b.succ, true
		case bytecode.OpAsyncIterClose:
			// The cursor is taken, and what its iterator's return method
			// returned takes its place, or there is nothing to close with.
			if b.depth() == 0 || int(in.A) <= pc {
				return nil, false
			}
			cur := b.pop().tree()
			if !b.spill() {
				return nil, false
			}
			d := b.depth()
			taken, ok := b.target(int(in.A), d)
			if !ok {
				return nil, false
			}
			fall, ok := b.target(pc+1, d+1)
			if !ok {
				return nil, false
			}
			t.blocks[bi] = tblock{body: b.takeBody(), next: func(c *tctx) int {
				x := cur(c)
				c.at(pc)
				v, has, err := asyncIterClose(c.r, x, 0)
				if err != nil {
					c.throw(err)
				}
				if !has {
					return taken
				}
				c.stack[d] = v
				return fall
			}}
			return b.succ, true
		}
		return nil, false
	}

	asyncIterClose = func(r *Runtime, v Value, mode uint32) (Value, bool, error) {
		switch mode {
		case 1:
			if !v.IsObject() {
				return Undefined, false, r.throwTypeError("the iterator's return method must return an object")
			}
			return Undefined, false, nil
		case 2:
			if st := iterStateOf(v); st != nil {
				st.done = true
			}
			return Undefined, false, nil
		}
		st := iterStateOf(v)
		if st == nil || st.done {
			return Undefined, false, nil
		}
		st.done = true
		if st.asyncIter {
			ret, err := r.getValueProp(st.iter, atomReturn)
			if err != nil {
				return Undefined, false, err
			}
			if ret.IsNullish() {
				return Undefined, false, nil
			}
			if !isCallable(ret) {
				return Undefined, false, r.throwTypeError("the iterator's return method is not callable")
			}
			res, err := r.call(ret, st.iter, nil)
			if err != nil {
				return Undefined, false, err
			}
			return res, true, nil
		}
		// A synchronous iterator is driven through the spec's wrapper of it,
		// whose return method always answers: with what the iterator's own
		// return gives, its value awaited, or, where it has none, a result
		// saying it is done.
		if st.arr == nil {
			ret, err := r.getValueProp(st.iter, atomReturn)
			if err != nil {
				return Undefined, false, err
			}
			if !ret.IsNullish() {
				if !isCallable(ret) {
					return Undefined, false, r.throwTypeError("the iterator's return method is not callable")
				}
				res, err := r.call(ret, st.iter, nil)
				if err != nil {
					return Undefined, false, err
				}
				return r.asyncResult(st, res), true, nil
			}
		}
		return Obj(r.iterResult(Undefined, true)), true, nil
	}
}
