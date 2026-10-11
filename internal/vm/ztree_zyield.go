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
// What a tree does not build -- yield*, for await -- leaves the function to
// the interpreter as before.
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
	// yield or await, which end a block (see suspendEnd).
	treeBuildsSuspend func(op bytecode.Op) bool

	// suspendOp builds end_params, which a tree runs as nothing: what comes
	// before it is the parameter prologue, which the interpreter runs when
	// the generator is made.
	suspendOp func(b *tbuilder, pc int, in bytecode.Instr) (int, bool)

	// suspendEnd ends block bi with yield or await: the value is handed out
	// and the frame suspended, to be resumed at the block after with the
	// value sent in where the yielded one was.
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
)

func init() {
	treeBuildsSuspend = func(op bytecode.Op) bool {
		switch op {
		case bytecode.OpEndParams, bytecode.OpYield, bytecode.OpAwait:
			return true
		}
		return false
	}

	suspendOp = func(b *tbuilder, pc int, in bytecode.Instr) (int, bool) {
		return 0, in.Op == bytecode.OpEndParams
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
		// yielded one was.
		s, ok := b.target(pc+1, d+1)
		if !ok {
			return nil, false
		}
		// Nothing goes on to it but a resumption, so it is kept apart, as a
		// handler's block is, with the handlers in force here.
		b.succ = b.succ[:len(b.succ)-1]
		b.handlerSucc = append(b.handlerSucc, thandlerEntry{block: s, pc: uint32(pc + 1)})
		if b.protected && b.entryHandlers[s] == nil {
			b.entryHandlers[s] = append([]thandlerEntry{}, b.handlers...)
		}
		await := in.Op == bytecode.OpAwait
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
		c.stack = r.stack[base : base+fn.MaxStack]
		if g.iters {
			// An exception looks for cursors in every slot of the frame's,
			// and what lies above the stack restored is another frame's.
			clear(r.stack[sp : base+fn.MaxStack])
		}
		return runGeneratorTree(r, f, t, g.index[f.pc])
	}

	runGeneratorTree = func(r *Runtime, f *frame, t *tree, b int) (Value, error) {
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
