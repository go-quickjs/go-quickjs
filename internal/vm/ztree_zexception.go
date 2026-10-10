package vm

import "github.com/go-quickjs/go-quickjs/internal/bytecode"

// Exception handlers in the tree tier.
//
// A function with a try statement is built as a tree like any other, its
// handlers registered and dropped on the frame by statements, as the
// interpreter's push_catch, push_finally and pop_catch do, and its finally
// clauses ending in a node for rethrow. What is different is how it is run:
// buildTree wraps such a tree in one block (handlerTree) whose statement runs
// the tree under a recover (runTreeProtected). A throw anywhere in it, or in
// a tree it calls, is caught there; the interpreter's unwindToHandler finds
// the handler and leaves the frame's pc at it, and the tree goes on from the
// block that begins there. So the recover is set up once each time the
// function runs and once more after each exception it catches, not once a
// statement, a block or a turn of a loop -- and a function without handlers,
// a nested call included, does not set one up at all.
//
// A throw statement with a handler of its own function around it does not
// panic: like the interpreter's throw, its node finds the handler and goes
// on to its block (throwToHandler), which the builder knows statically from
// the handlers in force along each path (enterHandlers). An exception from
// anything else -- a call, a getter, an operator -- is a panic as before.
//
// What a handler cannot catch -- an interrupt, a memory limit, anything not
// a JavaScript exception -- goes on past it as before, finally clauses not
// run, as in the interpreter.
//
// It is in the package's last file, after ztree_flow.go, so that its code
// sits after everything else's.

// thandlerEntry is a handler: the block it enters, a catch clause's or a
// finally clause's, which the structuring takes as an entry of its own, and
// the pc it is registered with.
type thandlerEntry struct {
	block   int
	pc      uint32
	finally bool
}

// enterHandlers sets the handlers in force at the start of block bi, in a
// function with handlers: those its first predecessor left it, or none for
// the first block. The code is structured, so every way into a block has
// the same ones.
func (b *tbuilder) enterHandlers(bi int) {
	if b.protected {
		b.handlers = append(b.handlers[:0], b.entryHandlers[bi]...)
	}
}

// leaveHandlers gives the blocks succ the handlers in force where the block
// just built ended, those not given theirs already.
func (b *tbuilder) leaveHandlers(succ []int) {
	if !b.protected {
		return
	}
	for _, s := range succ {
		if b.entryHandlers[s] == nil {
			b.entryHandlers[s] = append([]thandlerEntry{}, b.handlers...)
		}
	}
}

// treeBuildsException reports whether the tier builds op, one of the
// instructions of exception handling or of a scope's closing.
func treeBuildsException(op bytecode.Op) bool {
	switch op {
	case bytecode.OpPushCatch, bytecode.OpPushFinally, bytecode.OpPopCatch,
		bytecode.OpRethrow, bytecode.OpCloseUpvalues:
		return true
	}
	return false
}

// exceptionOp builds the instructions treeBuildsException names other than
// rethrow, which ends a block (see rethrow). A handler's block is not a
// successor of the block that registers it -- nothing goes on to it but a
// throw -- so it is kept apart, in handlerSucc, built as a successor is, at
// the depth the interpreter enters it at: the thrown value above the
// handler's depth, or for a finally the completion record's two slots.
func (b *tbuilder) exceptionOp(pc int, in bytecode.Instr) (int, bool) {
	switch in.Op {
	case bytecode.OpPushCatch, bytecode.OpPushFinally:
		if !b.spill() {
			return 0, false
		}
		d, finally := b.depth(), in.Op == bytecode.OpPushFinally
		n := 1
		if finally {
			n = 2
		}
		s, ok := b.target(int(in.A), d+n)
		if !ok {
			return 0, false
		}
		b.succ = b.succ[:len(b.succ)-1]
		h := thandlerEntry{block: s, pc: in.A, finally: finally}
		b.handlerSucc = append(b.handlerSucc, h)
		// The handler's block runs with the handlers outside this one: the
		// interpreter drops a handler before entering it.
		if b.entryHandlers[s] == nil {
			b.entryHandlers[s] = append([]thandlerEntry{}, b.handlers...)
		}
		b.handlers = append(b.handlers, h)
		b.body = append(b.body, pushTreeHandler(in.A, d, finally))
	case bytecode.OpPopCatch:
		if len(b.handlers) == 0 {
			return 0, false
		}
		b.handlers = b.handlers[:len(b.handlers)-1]
		return 0, b.stmt(popTreeHandler)
	case bytecode.OpCloseUpvalues:
		return 0, b.stmt(closeTreeUpvalues(in.A))
	default:
		return 0, false
	}
	return 0, true
}

// pushTreeHandler is push_catch or push_finally: a handler at pc for the
// stack's depth.
func pushTreeHandler(pc uint32, depth int, finally bool) tstmt {
	return func(c *tctx) {
		c.f.handlers = append(c.f.handlers, handler{pc: pc, stackDepth: depth, isFinally: finally})
	}
}

// popTreeHandler is pop_catch.
func popTreeHandler(c *tctx) { c.f.handlers = c.f.handlers[:len(c.f.handlers)-1] }

// closeTreeUpvalues is close_upvalues: the variables from slot up that
// closures captured are closed, so that the next turn of a loop, or the
// next entry of a block, has bindings of its own.
func closeTreeUpvalues(slot uint32) tstmt {
	return func(c *tctx) { c.r.closeUpvaluesFrom(c.f, int(slot)) }
}

// throwToHandler ends block bi with a throw statement of the value v, which
// the innermost handler in force catches: it goes on to the handler's block
// as an edge of the graph, without a panic. The node checks that the frame's
// innermost handler is the one the builder found, and throws as usual if
// it is not, which runTreeHandlers then catches.
func (b *tbuilder) throwToHandler(t *tree, bi, pc int, v tval) ([]int, bool) {
	h := b.handlers[len(b.handlers)-1]
	b.succ = append(b.succ, h.block)
	index := b.index
	t.blocks[bi] = tblock{body: b.takeBody(), next: func(c *tctx) int {
		e := v(c)
		c.at(pc)
		err := c.r.throw(e)
		if hs := c.f.handlers; len(hs) != 0 && hs[len(hs)-1].pc == h.pc {
			if _, ok := c.r.unwindToHandler(c.f, c.f.base+hs[len(hs)-1].stackDepth, err); ok {
				return index[c.f.pc]
			}
		}
		c.throw(err)
		return -1
	}}
	return b.succ, true
}

// rethrow ends block bi with rethrow, at the end of a finally clause: the
// completion record its two slots hold is carried on.
func (b *tbuilder) rethrow(t *tree, bi, pc int) ([]int, bool) {
	if b.depth() < 2 || !b.spill() {
		return nil, false
	}
	d := b.depth() - 2
	b.stack = b.stack[:d]
	fall, ok := b.target(pc+1, d)
	if !ok {
		return nil, false
	}
	t.blocks[bi] = tblock{body: b.takeBody(), next: rethrowTreeNode(pc, d, fall, b.index)}
	b.how = endCompletion
	return b.succ, true
}

// rethrowTreeNode carries a finally clause's completion record on, as the
// interpreter's rethrow does: a throw is thrown again, a return goes to the
// next finally clause out or else returns, and a normal completion goes on
// to the block after.
func rethrowTreeNode(pc, depth, fall int, index []int) tnext {
	return func(c *tctx) int {
		val, kind := c.stack[depth], completionKind(c.stack[depth+1].Number())
		switch kind {
		case completionThrow:
			c.at(pc)
			c.throw(c.r.throw(val))
		case completionReturn:
			c.at(pc)
			if _, ok := c.r.unwindToFinally(c.f, c.f.base+depth, val); ok {
				return index[c.f.pc]
			}
			if err := c.r.closeIteratorsReturning(c.f.base, c.f.base+depth); err != nil {
				c.throw(err)
			}
			if c.f.thisRef != nil {
				var err error
				if val, err = c.r.derivedResult(c.f, c.cl, val); err != nil {
					c.throw(err)
				}
			}
			c.ret = val
			return -1
		}
		return fall
	}
}

// handlerTree wraps t, a tree with handlers, in a tree of one block that
// runs it through runTreeHandlers.
func handlerTree(t *tree, index []int) *tree {
	return &tree{blocks: []tblock{{
		body: []tstmt{func(c *tctx) { c.runTreeHandlers(t, index) }},
		next: func(*tctx) int { return -1 },
	}}}
}

// runTreeHandlers runs t from its first block, and from each handler an
// exception enters, until it returns or an exception no handler catches
// leaves it.
func (c *tctx) runTreeHandlers(t *tree, index []int) {
	r := c.r
	depth, top := r.frameDepth, r.stackTop
	for b := 0; ; {
		err := c.runTreeProtected(t, b)
		if err == nil {
			return
		}
		// A tree this one called through runTreeNested has no recover of its
		// own and leaves its frames when it throws; the handler resumes this
		// frame, with the stack this frame had.
		r.unwindTreeFrames(depth)
		r.stackTop = top
		// Trees do not build iterator instructions, so only the handler's
		// saved depth matters; the slots above it are stale.
		sp := c.f.base
		if hs := c.f.handlers; len(hs) != 0 {
			sp += hs[len(hs)-1].stackDepth
		}
		if _, caught := r.unwindToHandler(c.f, sp, err); !caught {
			c.throw(err)
		}
		b = index[c.f.pc]
	}
}

// runTreeProtected runs t from block b as runTreeNested does, but under a
// recover: an exception is returned rather than passed on.
func (c *tctx) runTreeProtected(t *tree, b int) (err error) {
	defer func() {
		if p := recover(); p != nil {
			if th, ok := p.(treeThrow); ok {
				err = th.err
			} else {
				panic(p)
			}
		}
	}()
	for {
		blk := &t.blocks[b]
		for _, s := range blk.body {
			s(c)
		}
		if b = blk.next(c); b < 0 {
			return nil
		}
	}
}

// structureExceptions is structure for a function with handlers. The
// blocks ordinary flow reaches are structured from the first block, and
// those reachable only from a handler's block from that block, each part on
// its own: a handler's block is entered by an exception, and taken as an
// ordinary successor it would give a protected loop a second way in. The
// blocks a structure absorbs are left as they were, so that a handler can
// resume at any of them, as ordinary structuring leaves them for runTree.
func (f *flow) structureExceptions(t *tree, done []bool) {
	defer flowPool.Put(f)
	n := len(t.blocks)
	seen, region := make([]bool, n), make([]bool, n)
	roots := []int{0}
	for _, h := range f.entries {
		roots = append(roots, h.block)
	}
	for _, root := range roots {
		if seen[root] || !done[root] {
			continue
		}
		clear(region)
		work := []int{root}
		for len(work) != 0 {
			i := work[len(work)-1]
			work = work[:len(work)-1]
			if seen[i] || !done[i] {
				continue
			}
			seen[i], region[i] = true, true
			for _, s := range f.succOf(i) {
				work = append(work, int(s))
			}
		}
		part := takeFlow(n)
		part.entry = root
		part.entries = append(part.entries, f.entries...)
		for i := range n {
			if !region[i] {
				continue
			}
			var succ []int
			for _, s := range f.succOf(i) {
				succ = append(succ, int(s))
			}
			part.record(i, succ, f.how[i], f.cmp[i], f.steps[i])
		}
		part.structure(t, region)
	}
}

// completionSuccessors is what block i, which ends in rethrow, can go on to:
// the block after it, and the blocks of the finally clauses that enclose it,
// which a return goes to. Its own handler has been dropped before its
// finally clause runs, and an enclosing finally clause comes after it in the
// code.
func (f *flow) completionSuccessors(i int, raw []int32) []int32 {
	raw = f.copyList(raw)
	for _, h := range f.entries {
		if h.finally && h.block > i {
			raw = f.add(raw, h.block)
		}
	}
	return raw
}
