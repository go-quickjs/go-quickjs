//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64))

package vm

import (
	"slices"

	"github.com/go-quickjs/go-quickjs/internal/jit/ir"
)

// Register code's BigInts, as the stack form's code learns and makes them
// (jitBigIntSeen, ssa.BigIntFeedback): an operator whose guard failed, or
// which is generic, on BigInts is learned at its register pc, and the code
// compiled again makes it natively -- of BigInts in the int64 range, or
// from 0 to 2^64-1 -- or calls Go for it (ssa.GoArith), its blocks given
// back where an operand or a register holds its value alone
// (BigIntSole). Without it every BigInt operator of register code left
// native code for the interpreter.

// BigInt is ssa.BigIntFeedback's: the operator of the program's
// instruction pc met BigInts at an exit of code compiled before
// (jitRegBigIntSeen).
func (f *jitRegFeedback) BigInt(pc int) bool {
	l := f.fb.e
	return l != nil && pc >= 0 && pc < len(f.p.Origin) && slices.Contains(l.bigints, int32(f.p.Origin[pc]))
}

// BigIntWide is ssa.BigIntFeedback's: the operator of the program's
// instruction pc met BigInts from 0 to 2^64-1, some past the int64 range.
func (f *jitRegFeedback) BigIntWide(pc int) bool {
	l := f.fb.e
	return l != nil && pc >= 0 && pc < len(f.p.Origin) && slices.Contains(l.bigwide, int32(f.p.Origin[pc]))
}

// BigIntFailed is ssa.BigIntFeedback's: a guard of the operator of the
// program's instruction pc failed in code compiled before.
func (f *jitRegFeedback) BigIntFailed(pc int) bool {
	l := f.fb.e
	return l != nil && pc >= 0 && pc < len(f.p.Origin) && slices.Contains(l.failed, int32(f.p.Origin[pc]))
}

// jitRegBigIntSeen is jitBigIntSeen for e's code, f's register code run in
// the window at wb, at the instruction site of its program, whose guard
// failed or which is generic: what its operands met, learned at its
// register pc in what f's function's register code learned.
func (r *Runtime) jitRegBigIntSeen(f *frame, e *jitRegEntry, wb, site int) bool {
	p := e.p
	if site < 0 || p == nil || site >= len(p.Code) || site >= len(p.Origin) {
		return false
	}
	l := r.jit.regLearnedOf(r.jitKeyOf(f.cl.fn))
	return jitBigIntNote(l, int32(p.Origin[site]), p.Code[site], func(o ir.Operand) Value { return r.jitRegOperand(p, wb, o) }, false, false)
}

// jitRegBigIntExit is jitBigIntExit for e's code: at a host exit at site,
// an operator with no native form for BigInts, generic, that met two. It
// reports whether the site learned them, the code to be compiled again.
func (r *Runtime) jitRegBigIntExit(f *frame, e *jitRegEntry, wb, site int) bool {
	p := e.p
	if site < 0 || p == nil || site >= len(p.Code) || site >= len(p.Origin) {
		return false
	}
	in := p.Code[site]
	right := in.Right
	switch {
	case in.Op == ir.Binary:
		switch in.Operator {
		case ir.Sub, ir.Mul, ir.Div, ir.Mod, ir.Pow, ir.BitAnd, ir.BitOr, ir.BitXor, ir.Shl, ir.Shr:
		default:
			return false
		}
	case in.Op == ir.Unary && (in.Operator == ir.Neg || in.Operator == ir.BitNot):
		right = in.Left
	default:
		return false
	}
	l := r.jit.regLearnedOf(r.jitKeyOf(f.cl.fn))
	key := int32(p.Origin[site])
	if in.Left.Slot < 0 || right.Slot < 0 || !r.jitRegOperand(p, wb, in.Left).IsBigInt() || !r.jitRegOperand(p, wb, right).IsBigInt() ||
		!slices.Contains(l.failed, key) || slices.Contains(l.bigints, key) {
		return false
	}
	jitBigIntNote(l, key, in, func(o ir.Operand) Value { return r.jitRegOperand(p, wb, o) }, false, false)
	return slices.Contains(l.bigints, key)
}

// jitRegOperand is operand o of p's instruction, register code's run in
// the window at wb, as an exit left it: a register's value, in the window;
// a constant's; undefined for a scratch slot, which holds no operand an
// exit reads here.
func (r *Runtime) jitRegOperand(p *ir.Program, wb int, o ir.Operand) Value {
	switch {
	case o.Slot < 0:
		if o.Literal.Kind == ir.String {
			return Str(emptyString)
		}
		return Undefined
	case o.Slot < p.Regs && wb+o.Slot < len(r.stack):
		return r.stack[wb+o.Slot]
	}
	return Undefined
}
