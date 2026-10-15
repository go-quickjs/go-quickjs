package vm

import "github.com/go-quickjs/go-quickjs/internal/bytecode"

// x && y and x || y as values -- return this.a[r] && this.b[c + r], the
// test of Queens' getRowColumn -- were each a block for x, ended by the
// jump that keeps it, and a block for y, so that x was spilled to its slot
// and the block's end chose where to go on. Where y is an expression the
// tier builds as a tree with nothing to spill -- reads, operators and
// comparisons, an && or || of its own -- and nothing else jumps into it,
// the jump does not end a block (logicalJumps), and x and y are one node
// (logicalNode): x, and y where x does not decide.
//
// c ? a : b as a value is so too, where a and b are such expressions:
// the test's jump and the jump past b end no block, and the three are one
// node (ternaryNode).
//
// The nodes are variables, assigned closures in this file's init, laid out
// after every closure of the tree tier's (see AGENTS.md).

var (
	logicalNode func(x, y tval, kind uint8) tval
	ternaryNode func(test func(*tctx) bool, a, b tval) tval
)

// The kinds of logicalNode: x && y, x || y, x ?? y.
const (
	logicAnd = iota
	logicOr
	logicNullish
)

func init() {
	ternaryNode = func(test func(*tctx) bool, a, b tval) tval {
		return func(c *tctx) Value {
			if test(c) {
				return a(c)
			}
			return b(c)
		}
	}
	logicalNode = func(x, y tval, kind uint8) tval {
		switch kind {
		case logicOr:
			return func(c *tctx) Value {
				if v := x(c); truthy(v) {
					return v
				}
				return y(c)
			}
		case logicNullish:
			// Null or undefined itself: document.all, which == null, is not
			// nullish to ??.
			return func(c *tctx) Value {
				if v := x(c); !v.IsNullish() {
					return v
				}
				return y(c)
			}
		}
		return func(c *tctx) Value {
			if v := x(c); !truthy(v) {
				return v
			}
			return y(c)
		}
	}
}

// tlogic is an && or || whose y is being built: at is where the jump
// goes, where y ends, and x is what the jump kept. Or it is a c ? a : b,
// for which test is set: at is the jump past b until a is built, then
// end, where b ends.
type tlogic struct {
	at   int
	x    tentry
	kind uint8
	// jump is the instruction it starts at, and body how long the block's
	// statements were there: a y, an a or a b that adds one -- a call
	// whose receiver is spilled -- is not an expression of its own, and the
	// tree is built again with jump ending a block (buildTree).
	jump, body int

	test func(*tctx) bool
	a    tval
	end  int
}

// maxLogical is the most instructions a y may take: what is longer stays
// blocks, its scan bounded.
const maxLogical = 64

// logicalJumps is which of code's jump_if_false_keep and jump_if_true_keep
// need not end a block, with where each goes: one whose y, the code from
// it to where it goes, is all logicalOp's and && and || and ternaries
// that are such themselves, ending there or before. A c ? a : b's test --
// jump_if_false, jump_if_true or jump_if_cmp_false to b, whose instruction
// before is the jump past it -- is one, with where b starts, where a and b
// are such code. And no jump that is not one of them may go into any of
// them or to where one ends: targets counts the jumps to each
// instruction. Nested ones end where those around them do -- x ? a : y ?
// b : c, x && (y ? a : b) -- so those are found first, from the
// instructions alone, and then each is let go whose instructions another
// jump goes to, until none is, as letting one go leaves those around it
// with a jump that is not one of them.
func logicalJumps(code []bytecode.Instr, targets []int, skip map[int]bool) map[int]int {
	var out map[int]int
	// fits reports whether code[from:to] is all logicalOp's and the && and
	// || and ternaries in out, which end at to or before.
	fits := func(from, to int) bool {
		pass := map[int]bool{}
		for q := from; q < to; q++ {
			if t, ok := out[q]; ok {
				e := ternaryEnd(code, q, t)
				if e > 0 {
					// Its jump past its b is its own.
					pass[t-1] = true
				} else {
					e = t
				}
				if e > to {
					return false
				}
				continue
			}
			if !pass[q] && !logicalOp(code[q].Op) {
				return false
			}
		}
		return true
	}
	// span is where the one at pc ends: past its y, or past its b.
	span := func(pc int) int {
		if e := ternaryEnd(code, pc, out[pc]); e > 0 {
			return e
		}
		return out[pc]
	}
	add := func(pc, to int) {
		if out == nil {
			out = map[int]int{}
		}
		out[pc] = to
	}
	for pc := len(code) - 1; pc >= 0; pc-- {
		in := code[pc]
		if skip[pc] {
			continue
		}
		switch in.Op {
		case bytecode.OpJumpIfFalse, bytecode.OpJumpIfTrue, bytecode.OpJumpIfCmpFalse:
			// c ? a : b: a from pc+1 to the jump past b at els-1, b from
			// els to where that jump goes.
			els := int(in.A)
			if els <= pc+2 || els >= len(code) || code[els-1].Op != bytecode.OpJump {
				continue
			}
			end := int(code[els-1].A)
			if end <= els || end >= len(code) || end-pc > maxLogical {
				continue
			}
			if fits(pc+1, els-1) && fits(els, end) {
				add(pc, els)
			}
		case bytecode.OpJumpIfFalseKeep, bytecode.OpJumpIfTrueKeep, bytecode.OpJumpIfNotNullish:
			to := int(in.A)
			if to <= pc+1 || to >= len(code) || to-pc > maxLogical {
				continue
			}
			if fits(pc+1, to) {
				add(pc, to)
			}
		}
	}
	for changed := true; changed && len(out) > 0; {
		changed = false
		// own counts the jumps to each instruction of the ones in out.
		own := map[int]int{}
		for pc, t := range out {
			own[t]++
			if e := ternaryEnd(code, pc, t); e > 0 {
				own[e]++
			}
		}
		for pc, t := range out {
			e := span(pc)
			ok := fits(pc+1, e)
			if e != t {
				// A ternary is its a and its b, apart.
				ok = fits(pc+1, t-1) && fits(t, e)
			}
			for q := pc + 1; q <= e && ok; q++ {
				ok = targets[q] == own[q]
			}
			if !ok {
				delete(out, pc)
				changed = true
			}
		}
	}
	return out
}

// logicalOp reports whether op is one an && or ||'s y may be made of: one
// the tier builds as a tree it pushes, mostly with no statement and
// nothing spilled -- where it does, buildBlock finds so -- and that is no
// jump.
func logicalOp(op bytecode.Op) bool {
	switch op {
	case bytecode.OpPushConst, bytecode.OpPushUndef, bytecode.OpPushNull,
		bytecode.OpPushTrue, bytecode.OpPushFalse, bytecode.OpPushInt, bytecode.OpPushThis,
		bytecode.OpGetLocal, bytecode.OpGetLocal2, bytecode.OpGetUpvalue,
		bytecode.OpGetProp, bytecode.OpGetIndex, bytecode.OpGetLocalIndex,
		bytecode.OpBinImm, bytecode.OpBinLocal, bytecode.OpLocalBinImm,
		bytecode.OpAdd, bytecode.OpSub, bytecode.OpMul, bytecode.OpDiv, bytecode.OpMod,
		bytecode.OpNeg, bytecode.OpPos,
		bytecode.OpBitAnd, bytecode.OpBitOr, bytecode.OpBitXor, bytecode.OpBitNot,
		bytecode.OpShl, bytecode.OpShr, bytecode.OpUShr,
		bytecode.OpEq, bytecode.OpNe, bytecode.OpStrictEq, bytecode.OpStrictNe,
		bytecode.OpLt, bytecode.OpLe, bytecode.OpGt, bytecode.OpGe,
		bytecode.OpNot, bytecode.OpTypeOf, bytecode.OpIn, bytecode.OpInstanceOf,
		bytecode.OpGetGlobal, bytecode.OpGetLocalCheck,
		// A call is mostly built without a statement: one that is not
		// is found as it is built.
		bytecode.OpGetPropThis, bytecode.OpCall, bytecode.OpCallMethod, bytecode.OpNew:
		return true
	}
	return false
}

// ternaryEnd is, for a c ? a : b found at pc whose b starts at els, where
// b ends; 0 for an && or ||, whose jump is no test.
func ternaryEnd(code []bytecode.Instr, pc, els int) int {
	switch code[pc].Op {
	case bytecode.OpJumpIfFalse, bytecode.OpJumpIfTrue, bytecode.OpJumpIfCmpFalse:
		return int(code[els-1].A)
	}
	return 0
}

// ternaryTest is the test of the c ? a : b whose jump at pc, in, the block
// being built has got to, taken from the stack: a is taken where it holds.
func (b *tbuilder) ternaryTest(pc int, in bytecode.Instr) func(*tctx) bool {
	switch in.Op {
	case bytecode.OpJumpIfCmpFalse:
		y, x := b.pop(), b.pop()
		return compareNode(bytecode.Op(in.B), x.tree(), y.tree(), pc)
	case bytecode.OpJumpIfFalse:
		v := b.pop().tree()
		return func(c *tctx) bool { return truthy(v(c)) }
	}
	v := b.pop().tree()
	return func(c *tctx) bool { return !truthy(v(c)) }
}
