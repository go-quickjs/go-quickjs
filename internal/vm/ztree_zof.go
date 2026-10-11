package vm

import "github.com/go-quickjs/go-quickjs/internal/bytecode"

// for...of and for...in loops in the tree tier, and the iterator a
// destructuring pattern steps.
//
// A loop's cursor is kept in its stack slot for as long as the loop runs,
// where the interpreter keeps it, and each instruction is a node that does
// what the interpreter's does with it: for_of_start and for_in_start make
// it, iter_next_or_jump ends a block, going on with the next value or to
// where the loop ends, and iter_close closes it where a break or a continue
// leaves the loop. iter_step, iter_rest and iter_close_normal are a
// destructuring pattern's.
//
// A loop left abruptly closes its iterator. A return does so already, as
// the interpreter's does, from the slots below the value returned. An
// exception is caught as a function with handlers catches one: a function
// with a for...of loop runs under handlerTree's recover, try statement or
// not, and runTreeHandlers closes the cursors the exception leaves -- every
// one in the frame's slots where nothing catches it, and those above the
// handler's depth where something does. A tree does not know the depth of
// its stack where an exception was thrown, so it closes what it finds in
// all of the frame's slots: they are cleared when the function is entered,
// so that a cursor it finds there is one of its own, which a loop that is
// over has marked done.
//
// Its functions are variables, each assigned a closure in init, rather
// than functions or methods: Go lays out all of a package's functions
// before any of its closures, so code added here as functions would sit
// between the interpreter's hot functions and the tree nodes' hot
// closures, and move one against the other, where a closure is laid out
// after every closure already there (see AGENTS.md).
var (
	// treeBuildsIter reports whether the tier builds op, one of a loop's or a
	// destructuring pattern's iterator instructions, or one treeBuildsOps
	// names.
	treeBuildsIter func(op bytecode.Op) bool

	// iterOp builds the instructions treeBuildsIter names other than
	// iter_next_or_jump, which ends a block (see iterNextOrJump), and hands
	// opsOp those treeBuildsOps does. Each runs code, and is a statement:
	// what it pushes is written to its slot.
	iterOp func(b *tbuilder, pc int, in bytecode.Instr) (int, bool)

	// iterNextOrJump ends block bi with iter_next_or_jump: the cursor on top of
	// the stack is stepped, and the block goes on to the next one with the
	// value pushed, or, once the iterator is done, to where the loop ends with
	// the cursor still there. A loop's jump is forward; its way back is the
	// jump at the end of its body.
	iterNextOrJump func(b *tbuilder, t *tree, bi, pc int, in bytecode.Instr) ([]int, bool)

	// closeTreeIterators closes the for...of cursors in the frame's slots from
	// slot from up that an exception leaving them abandons, as the
	// interpreter's unwinding does from the depth it was thrown at. Nothing is
	// closed once the host has stopped the runtime: a return method is the
	// script's code.
	closeTreeIterators func(c *tctx, from int)
)

func init() {
	treeBuildsIter = func(op bytecode.Op) bool {
		switch op {
		case bytecode.OpForOfStart, bytecode.OpForInStart, bytecode.OpIterNextOrJump, bytecode.OpIterClose,
			bytecode.OpIterStep, bytecode.OpIterRest, bytecode.OpIterCloseNormal:
			return true
		}
		return treeBuildsOps(op)
	}

	iterOp = func(b *tbuilder, pc int, in bytecode.Instr) (int, bool) {
		switch in.Op {
		case bytecode.OpForOfStart, bytecode.OpForInStart:
			v := b.pop().tree()
			if !b.spill() {
				return 0, false
			}
			d := b.depth()
			start := (*Runtime).startForOf
			if in.Op == bytecode.OpForInStart {
				start = (*Runtime).startForIn
			}
			b.body = append(b.body, func(c *tctx) {
				x := v(c)
				c.at(pc)
				cur, err := start(c.r, x)
				if err != nil {
					c.throw(err)
				}
				c.stack[d] = cur
			})
			b.pushSlot(d)
		case bytecode.OpIterStep, bytecode.OpIterRest:
			// The cursor sits A slots below the top: a target's reference may
			// have been pushed above it.
			if !b.spill() || int(in.A) >= b.depth() {
				return 0, false
			}
			d := b.depth()
			at := d - 1 - int(in.A)
			step := (*Runtime).iterStep
			if in.Op == bytecode.OpIterRest {
				step = (*Runtime).iterRest
			}
			b.body = append(b.body, func(c *tctx) {
				c.at(pc)
				v, err := step(c.r, c.stack[at])
				if err != nil {
					c.throw(err)
				}
				c.stack[d] = v
			})
			b.pushSlot(d)
		case bytecode.OpIterClose, bytecode.OpIterCloseNormal:
			// iter_close leaves the cursor for a drop; iter_close_normal takes
			// it. Leaving a loop or a pattern is not a throw, so a failure in
			// the iterator's return method is the result.
			if !b.spill() || b.depth() == 0 {
				return 0, false
			}
			at := b.depth() - 1
			if in.Op == bytecode.OpIterCloseNormal {
				b.pop()
			}
			b.body = append(b.body, func(c *tctx) {
				c.at(pc)
				if err := c.r.iterCloseNormal(c.stack[at]); err != nil {
					c.throw(err)
				}
			})
		default:
			return opsOp(b, pc, in)
		}
		return 0, true
	}

	iterNextOrJump = func(b *tbuilder, t *tree, bi, pc int, in bytecode.Instr) ([]int, bool) {
		if !b.spill() || b.depth() == 0 || int(in.A) <= pc {
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
			c.at(pc)
			v, more, err := c.r.iterNext(c.stack[d-1])
			if err != nil {
				c.throw(err)
			}
			if !more {
				return taken
			}
			c.stack[d] = v
			return fall
		}}
		return b.succ, true
	}

	closeTreeIterators = func(c *tctx, from int) {
		if c.r.stopped == nil {
			c.r.closeIteratorsIn(c.f.base+from, c.f.base+len(c.stack))
		}
	}
}
