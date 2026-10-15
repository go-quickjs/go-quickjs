package vm

import (
	"sync/atomic"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
)

// Generators, async functions and async generators in the tree tier.
//
// The interpreter suspends one by saving where its frame is and what its
// operand stack holds, and resumes it by restoring both and going on from
// there (see generator.go). A tree can do the same, since what it leaves
// on the stack at the end of a block is in the frame's own slots, where the
// interpreter would have it. So yield and await end a block: the node
// records the value, the pc after it and the depth beneath it, as the
// interpreter's yield does, and returns without a panic. The block after
// is a resumption's way in, an entry of its own as a handler's block is, so
// that a loop the suspension is not in is still structured and one it is in
// is left to the blocks, which a resumption can enter anywhere.
//
// A resumption runs the tree from the block the frame's pc begins, under
// one recover, as a function with handlers runs (runTreeHandlers): a throw
// or a return injected at the suspension point unwinds as the interpreter
// unwinds it, and an exception the body throws goes to its handler or out.
// The parameter prologue, before end_params, still runs in the interpreter,
// when the generator is made.
//
// yield* suspends as yield does, its resumption pushing the kind of
// resumption as well as the value sent in, and for await awaits as await
// does (see ztree_zyieldstar.go).
//
// Its functions are variables, each assigned a closure in init, rather
// than functions or methods: Go lays out all of a package's functions
// before any of its closures, so code added here as functions would sit
// between the interpreter's hot functions and the tree nodes' hot
// closures, and move one against the other, where a closure is laid out
// after every closure already there (see AGENTS.md).

// tgen is what a generator's tree keeps for its resumptions: the block
// each pc begins, and whether the function has for...of loops, whose
// cursors an exception closes (see ztree_zof.go).
type tgen struct {
	index []int
	iters bool
}

var (
	// treeBuildsSuspend reports whether the tier builds op: end_params, or
	// yield, yield* or await, which end a block (see suspendEnd), or one
	// treeBuildsDelegate names.
	treeBuildsSuspend func(op bytecode.Op) bool

	// suspendOp builds end_params, which a tree runs as nothing: what comes
	// before it is the parameter prologue, which the interpreter runs when
	// the generator is made. It hands delegateOp the rest.
	suspendOp func(b *tbuilder, pc int, in bytecode.Instr) (int, bool)

	// suspendEnd ends block bi with yield, yield* or await: the value is
	// handed out and the frame suspended, to be resumed at the block after
	// with the value sent in where the yielded one was -- and for yield*,
	// the kind of resumption above it.
	suspendEnd func(b *tbuilder, t *tree, bi, pc int, in bytecode.Instr) ([]int, bool)

	// suspendTree makes t, a generator's or an async function's tree, one a
	// resumption can run: without handlerTree's wrapper, which runs from
	// the first block, and with the blocks pcs begin.
	suspendTree func(fn *bytecode.Function, t *tree, index []int, iters bool) *tree

	// runGenerator runs a generator's frame from its pc, with sp the depth
	// of its operand stack and pending what a resumption injects there. It
	// unwinds that first, for both tiers -- executeAt's own unwinding of an
	// exception nothing catches left open the iterators of the loops it
	// leaves -- and then runs the body as its tree, or in the interpreter
	// where it has none.
	runGenerator func(r *Runtime, f *frame, sp int, pending error) (Value, error)

	// runGeneratorTree runs t, the tree of the generator whose frame f is,
	// from block b, with its context set, to its next suspension or its
	// end: an exception the body catches is unwound to its handler, and the
	// run goes on there.
	runGeneratorTree func(r *Runtime, f *frame, t *tree, b int) (Value, error)

	// resumeRoots is, for f, a flow of a function that suspends, the loop
	// headers its resumptions reach first, going on from where they enter,
	// which structureExceptions structures on their own, each from its
	// header; or nil where f has no resumptions. A structure is entered
	// only at its first block, and a resumption enters a loop it was
	// suspended in partway round: structured from the function's first
	// block, the loop would be run from block to block after the first
	// suspension, every turn of it. Each a structure of its own, a loop a
	// resumption goes on into runs as one from its next turn.
	resumeRoots func(f *flow, done []bool) []bool
)

func init() {
	treeBuildsSuspend = func(op bytecode.Op) bool {
		switch op {
		case bytecode.OpEndParams, bytecode.OpYield, bytecode.OpAwait, bytecode.OpYieldStar:
			return true
		}
		return treeBuildsDelegate(op)
	}

	suspendOp = func(b *tbuilder, pc int, in bytecode.Instr) (int, bool) {
		if in.Op == bytecode.OpEndParams {
			return 0, true
		}
		return delegateOp(b, pc, in)
	}

	suspendEnd = func(b *tbuilder, t *tree, bi, pc int, in bytecode.Instr) ([]int, bool) {
		if b.depth() == 0 {
			return nil, false
		}
		v := b.pop().tree()
		if !b.spill() {
			return nil, false
		}
		d := b.depth()
		// The block after is entered with the value sent in where the
		// yielded one was, and a yield*'s with the kind of resumption above
		// it (see runDelegating).
		sent := 1
		if in.Op == bytecode.OpYieldStar {
			sent = 2
		}
		s, ok := b.target(pc+1, d+sent)
		if !ok {
			return nil, false
		}
		// Nothing goes on to it but a resumption, so it is kept apart, as a
		// handler's block is, with the handlers in force here.
		b.succ = b.succ[:len(b.succ)-1]
		b.handlerSucc = append(b.handlerSucc, thandlerEntry{block: s, pc: uint32(pc + 1), resume: true})
		if b.protected && b.entryHandlers[s] == nil {
			b.entryHandlers[s] = append([]thandlerEntry{}, b.handlers...)
		}
		await := in.Op == bytecode.OpAwait
		delegate := in.Op == bytecode.OpYieldStar
		raw := delegate && in.A == 1
		if delegate {
			t.blocks[bi] = tblock{body: b.takeBody(), next: func(c *tctx) int {
				x := v(c)
				c.f.pc = uint32(pc + 1)
				c.f.savedSP = c.f.base + d
				if d > 0 && x.IsObject() && x.ref == c.stack[d-1].ref {
					// The delegate's cursor, standing for the value above it
					// (see delegateNext), which is yielded on as a value.
					c.r.treeSig = suspendSignal{value: c.stack[d+1], delegate: true}
				} else {
					c.r.treeSig = suspendSignal{value: x, delegate: true, raw: raw}
				}
				c.r.treeSuspend = &c.r.treeSig
				return -1
			}}
			return b.succ, true
		}
		t.blocks[bi] = tblock{body: b.takeBody(), next: func(c *tctx) int {
			x := v(c)
			// The operand is popped before the depth is recorded, so that
			// the saved stack holds only what is still live.
			c.f.pc = uint32(pc + 1)
			c.f.savedSP = c.f.base + d
			c.r.treeSig = suspendSignal{value: x, await: await}
			c.r.treeSuspend = &c.r.treeSig
			return -1
		}}
		return b.succ, true
	}

	suspendTree = func(fn *bytecode.Function, t *tree, index []int, iters bool) *tree {
		if fn.HasTailCall {
			// A tail call ends a frame in runFD's loop, which a generator's
			// is not run by.
			return nil
		}
		// A body without a loop is given one too: what a resumption of a
		// tree costs (see resumeTree) is less than the interpreter's, whose
		// suspensions allocate, however few instructions the body runs.
		t.gen = &tgen{index: index, iters: iters}
		return t
	}

	runGenerator = func(r *Runtime, f *frame, sp int, pending error) (Value, error) {
		if r.stopped != nil {
			return Undefined, r.stopped
		}
		base := f.base
		if ret, ok := pending.(*returnSignal); ok {
			// generator.return() is a return at the suspension point: the
			// finally clauses between there and the top of the body run,
			// and a catch does not.
			var found bool
			if sp, found = r.unwindToFinally(f, sp, ret.value); !found {
				if err := r.closeIteratorsReturning(base, sp); err != nil {
					return Undefined, err
				}
				return ret.value, nil
			}
		} else if pending != nil {
			// An exception injected there -- generator.throw(), a rejected
			// await -- unwinds as if the suspended expression had thrown
			// it. Where nothing in the body catches it, it leaves the frame,
			// and every loop the body was suspended in with it.
			var caught bool
			if sp, caught = r.unwindToHandler(f, sp, pending); !caught {
				r.closeIteratorsIn(base, sp)
				return Undefined, pending
			}
		}
		fn := f.cl.fn
		t := (*tree)(atomic.LoadPointer(&fn.VMCode))
		if t == nil {
			t = firstTree(fn)
		}
		if t == noTree || t.gen == nil {
			return r.executeAt(f, sp, nil)
		}
		g := t.gen
		c := &f.tc
		c.r, c.f, c.cl, c.locals = r, f, f.cl, f.locals
		c.closureCode = f.cl.closureCode
		c.stack = r.stack[base : base+fn.MaxStack]
		if g.iters {
			// An exception looks for cursors in every slot of the frame's,
			// and what lies above the stack restored is another frame's.
			clear(r.stack[sp : base+fn.MaxStack])
		}
		return runGeneratorTree(r, f, t, g.index[f.pc])
	}

	resumeRoots = func(f *flow, done []bool) []bool {
		var stop, header, seen []bool
		var work []int
		for _, h := range f.entries {
			if !h.resume {
				continue
			}
			if stop == nil {
				// A loop goes back to its header, a block no later than the
				// one it goes back from, as structure finds loops.
				n := len(done)
				stop, header, seen = make([]bool, n), make([]bool, n), make([]bool, n)
				for i := range n {
					if done[i] {
						for _, y := range f.succOf(i) {
							if int(y) <= i {
								header[y] = true
							}
						}
					}
				}
			}
			work = append(work, h.block)
		}
		for len(work) != 0 {
			i := work[len(work)-1]
			work = work[:len(work)-1]
			if seen[i] || !done[i] {
				continue
			}
			seen[i] = true
			if header[i] {
				stop[i] = true
				continue
			}
			for _, y := range f.succOf(i) {
				work = append(work, int(y))
			}
		}
		return stop
	}

	runGeneratorTree = func(r *Runtime, f *frame, t *tree, b int) (Value, error) {
		if r.jitOn() {
			// Native code, where the JIT has the function's, from the
			// resumption's entry.
			if v, err, native := r.jitResume(f); native {
				return v, err
			}
		}
		c, g, base := &f.tc, t.gen, f.base
		depth, top := r.frameDepth, r.stackTop
		for {
			err := c.runTreeProtected(t, b)
			if s := r.treeSuspend; s != nil {
				r.treeSuspend = nil
				return Undefined, s
			}
			if err == nil {
				return c.ret, nil
			}
			// As runTreeHandlers: the frames of the trees this one called
			// without a recover of their own are left, and the exception
			// goes to the handler, or out.
			r.unwindTreeFrames(depth)
			r.stackTop = top
			esp := base
			if g.iters {
				esp += len(c.stack)
			} else if hs := f.handlers; len(hs) != 0 {
				esp += hs[len(hs)-1].stackDepth
			}
			if _, caught := r.unwindToHandler(f, esp, err); !caught {
				if g.iters {
					closeTreeIterators(c, 0)
				}
				return Undefined, err
			}
			b = g.index[f.pc]
		}
	}
}
