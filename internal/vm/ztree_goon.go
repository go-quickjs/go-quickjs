package vm

// Going on in a function's tree from one of its blocks, the frame as the
// interpreter has it at the block's first instruction: where native code
// gives an invocation back to Go for the rest (the JIT's
// jitInterpretAfter), the interpreter runs the frame only to its next loop
// header, and the tree the rest -- the tier the invocation would have run
// in without the JIT, not the bytecode interpreter, which runs a loop a few
// times as long. Only a plain tree's: one of no handlers, no for...of
// cursors and no suspensions (handlerTree, suspendTree), whose blocks are
// its own; a block leaves every value in the frame (spill), so any
// block's first instruction is a frame state both tiers share, as an
// exception handler's block is entered at one.
//
// Package variables assigned closures, as ztree_zyield.go's are. The file
// sorts before every other ztree_ file with an init: Go lays the init
// closures out in descending init order, so this one's come after all of
// theirs, and move none of the hot ones.

var (
	// plainTree keeps t's blocks' first instructions, in code order, from
	// index, the block each instruction begins, for treeBlockAt; the bool
	// is handlerTree's iters, false for a plain tree.
	plainTree func(t *tree, index []int, iters bool) *tree

	// treeBlockAt is the block of t, a plain tree, whose first instruction
	// is pc, or -1.
	treeBlockAt func(t *tree, pc int) int

	// runTreeFrom runs f's function as t, its plain tree, from block b, to
	// its return, as runTree runs it from its first: an exception leaving
	// the tree is returned, as runTree's is.
	runTreeFrom func(r *Runtime, f *frame, t *tree, b int) (Value, error)
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

	runTreeFrom = func(r *Runtime, f *frame, t *tree, b int) (v Value, err error) {
		if r.stopped != nil {
			return Undefined, r.stopped
		}
		c := &f.tc
		c.r, c.f, c.cl, c.locals = r, f, f.cl, f.locals
		c.closureCode = f.cl.closureCode
		c.stack = r.stack[f.base : f.base+f.cl.fn.MaxStack]
		depth := r.frameDepth
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
				r.unwindTreeFrames(depth)
				v, err = Undefined, th.err
			}
		}()
		blocks := t.entries
		for {
			blk := &blocks[b]
			for _, s := range blk.body {
				s(c)
			}
			if b = blk.next(c); b < 0 {
				if r.treeTail {
					r.treeTail = false
					return Undefined, errTailCall
				}
				return c.ret, nil
			}
		}
	}
}
