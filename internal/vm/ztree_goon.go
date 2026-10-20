package vm

// Going on in a function's tree from one of its blocks, the frame as the
// interpreter has it at the block's first instruction: where native code
// gives an invocation back to Go for the rest (the JIT's
// jitInterpretAfter), the interpreter runs the frame only to its next loop
// header, and the tree the rest -- the tier the invocation would have run
// in without the JIT, not the bytecode interpreter, which runs a loop a few
// times as long. A plain tree's -- one of no handlers, no for...of cursors
// and no suspensions -- or one with handlers or for...of cursors, which
// handlerTree wraps (handledTree), whose blocks go on under
// runTreeHandlers' recover as they do after an exception a handler caught;
// not a generator's or an async function's (suspendTree). A block leaves
// every value in the frame (spill), so any block's first instruction is a
// frame state both tiers share, as an exception handler's block is entered
// at one: the frame's handlers, and its cursors in their slots, are the
// interpreter's.
//
// Package variables assigned closures, as ztree_zyield.go's are. The file
// sorts before every other ztree_ file with an init: Go lays the init
// closures out in descending init order, so this one's come after all of
// theirs, and move none of the hot ones.

// treeHandled is a tree that handlerTree wraps, t, every one of its blocks,
// and what runTreeHandlers runs it with, index and iters.
type treeHandled struct {
	t     *tree
	index []int
	iters bool
}

var (
	// plainTree keeps t's blocks' first instructions, in code order, from
	// index, the block each instruction begins, for treeBlockAt; the bool
	// is handlerTree's iters, false for a plain tree.
	plainTree func(t *tree, index []int, iters bool) *tree

	// handledTree gives w, the tree of one block that runs t, a tree with
	// handlers or for...of loops, through runTreeHandlers with index and
	// iters, t's blocks' first instructions, which plainTree kept, for
	// going on in t from one of them.
	handledTree func(w, t *tree, index []int, iters bool) *tree

	// treeBlockAt is the block of t, a plain tree, whose first instruction
	// is pc, or -1.
	treeBlockAt func(t *tree, pc int) int

	// runTreeFrom runs f's function as t, its tree, from block b, the
	// frame's operands depth high, to its return, as runTree runs it from
	// its first: an exception leaving the tree is returned, as runTree's
	// is.
	runTreeFrom func(r *Runtime, f *frame, t *tree, b, depth int) (Value, error)

	// runHandledFrom runs h's tree from block b as runTreeHandlers runs it
	// from its first, and from each handler an exception enters.
	runHandledFrom func(c *tctx, h *treeHandled, b int)
)

func init() {
	plainTree = func(t *tree, index []int, _ bool) *tree {
		// The builder numbers blocks in code order from their first
		// instructions, every other instruction's index 0: the k-th start
		// is the next instruction whose index is k.
		n := cap(t.blocks)
		if n < 2 || len(index) == 0 {
			return t
		}
		starts := make([]int32, 1, n)
		for pc := 1; pc < len(index)-1 && len(starts) < n; pc++ {
			if index[pc] == len(starts) {
				starts = append(starts, int32(pc))
			}
		}
		if len(starts) == n {
			t.starts, t.entries = starts, t.blocks[:n]
		}
		return t
	}

	handledTree = func(w, t *tree, index []int, iters bool) *tree {
		if t.starts != nil {
			// Its blocks all, those a structure absorbed too, which are
			// left as they were (structure).
			w.starts, w.entries = t.starts, t.entries
			w.handled = &treeHandled{t: &tree{blocks: t.entries}, index: index, iters: iters}
		}
		return w
	}

	treeBlockAt = func(t *tree, pc int) int {
		lo, hi := 0, len(t.starts)
		for lo < hi {
			m := int(uint(lo+hi) >> 1)
			if int(t.starts[m]) < pc {
				lo = m + 1
			} else {
				hi = m
			}
		}
		if lo < len(t.starts) && int(t.starts[lo]) == pc {
			return lo
		}
		return -1
	}

	runTreeFrom = func(r *Runtime, f *frame, t *tree, b, depth int) (v Value, err error) {
		if r.stopped != nil {
			return Undefined, r.stopped
		}
		c := &f.tc
		c.r, c.f, c.cl, c.locals = r, f, f.cl, f.locals
		c.closureCode = f.cl.closureCode
		c.stack = r.stack[f.base : f.base+f.cl.fn.MaxStack]
		frames := r.frameDepth
		defer func() {
			if p := recover(); p != nil {
				th, ok := p.(treeThrow)
				if !ok {
					if jitBuilt {
						if jv, jerr, native := jitTreeResult(p); native {
							v, err = jv, jerr
							return
						}
					}
					panic(p)
				}
				r.unwindTreeFrames(frames)
				v, err = Undefined, th.err
			}
		}()
		if h := t.handled; h != nil {
			if h.iters {
				// What is past the operands is no cursor of the frame's, as
				// runTreeHandlers clears the slots for its first block: an
				// exception closes those it finds in them.
				clear(c.stack[depth:])
			}
			runHandledFrom(c, h, b)
		} else {
			blocks := t.entries
			for b >= 0 {
				blk := &blocks[b]
				for _, s := range blk.body {
					s(c)
				}
				b = blk.next(c)
			}
		}
		if r.treeTail {
			r.treeTail = false
			return Undefined, errTailCall
		}
		return c.ret, nil
	}

	runHandledFrom = func(c *tctx, h *treeHandled, b int) {
		r := c.r
		depth, top := r.frameDepth, r.stackTop
		for {
			err := c.runTreeProtected(h.t, b)
			if err == nil {
				return
			}
			// As runTreeHandlers: the frames of the trees this one called
			// without a recover of their own are left, and the exception
			// goes to the handler, or out.
			r.unwindTreeFrames(depth)
			r.stackTop = top
			sp := c.f.base
			if h.iters {
				sp += len(c.stack)
			} else if hs := c.f.handlers; len(hs) != 0 {
				sp += hs[len(hs)-1].stackDepth
			}
			if _, caught := r.unwindToHandler(c.f, sp, err); !caught {
				if h.iters {
					closeTreeIterators(c, 0)
				}
				c.throw(err)
			}
			b = h.index[c.f.pc]
		}
	}
}
