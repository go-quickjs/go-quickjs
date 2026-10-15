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
// The node is a variable, assigned a closure in this file's init, laid out
// after every closure of the tree tier's (see AGENTS.md).

var logicalNode func(x, y tval, or bool) tval

func init() {
	logicalNode = func(x, y tval, or bool) tval {
		if or {
			return func(c *tctx) Value {
				if v := x(c); truthy(v) {
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
// goes, where y ends, and x is what the jump kept.
type tlogic struct {
	at int
	x  tentry
	or bool
}

// maxLogical is the most instructions a y may take: what is longer stays
// blocks, its scan bounded.
const maxLogical = 64

// logicalJumps is which of code's jump_if_false_keep and jump_if_true_keep
// need not end a block, with where each goes: one whose y, the code from
// it to where it goes, is all logicalOp's, or an && or || that is one
// itself and ends there or before, and whose y and target nothing else
// jumps to. targets counts the jumps to each instruction.
func logicalJumps(code []bytecode.Instr, targets []int) map[int]int {
	var out map[int]int
	for pc := len(code) - 1; pc >= 0; pc-- {
		in := code[pc]
		if in.Op != bytecode.OpJumpIfFalseKeep && in.Op != bytecode.OpJumpIfTrueKeep {
			continue
		}
		to := int(in.A)
		if to <= pc+1 || to >= len(code) || to-pc > maxLogical {
			continue
		}
		ok := true
		inner := map[int]int{}
		for q := pc + 1; q < to && ok; q++ {
			if t, ok2 := out[q]; ok2 && t <= to {
				inner[t]++
				continue
			}
			ok = logicalOp(code[q].Op)
		}
		for q := pc + 1; q < to && ok; q++ {
			ok = targets[q] == inner[q]
		}
		if ok && targets[to] == 1+inner[to] {
			if out == nil {
				out = map[int]int{}
			}
			out[pc] = to
		}
	}
	return out
}

// logicalOp reports whether op is one an && or ||'s y may be made of: one
// the tier builds as a tree it pushes, with no statement and nothing
// spilled, and that is no jump.
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
		bytecode.OpNot, bytecode.OpTypeOf:
		return true
	}
	return false
}
