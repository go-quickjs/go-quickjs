//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64))

package vm

import (
	"slices"
	"sync/atomic"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
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
//
// The first compile knows them too, as the stack form's knows them from
// the tree tier (jitTreeBig, jitTreeWide): the register interpreter's slow
// paths, which make every operator on BigInts, mark its register pc as the
// tree marks its sites (regBigOf, regCode's bigSeen) -- BigInts in the
// int64 range, from 0 to 2^64-1, or others, on the heap -- and the
// feedback answers from the marks as from what exits learned.

func init() {
	regBigOf = func(r *Runtime, c *regCode, pc int, a, b, v Value) {
		if !r.jitEnabled || !a.IsBigInt() || !b.IsBigInt() {
			return
		}
		// From 0 to 2^64-1: held in its value, of no sign but zero's.
		unsigned := func(v Value) bool {
			if !v.isShortBig() {
				return false
			}
			neg, mag := v.shortBigParts()
			return !neg || mag == 0
		}
		kind := func(s *treeBigSeen) []atomic.Uint32 { return s.heap }
		switch {
		case a.isInt64Big() && b.isInt64Big() && v.isInt64Big():
			kind = func(s *treeBigSeen) []atomic.Uint32 { return s.bigs }
		case unsigned(a) && unsigned(b) && unsigned(v):
			kind = func(s *treeBigSeen) []atomic.Uint32 { return s.wide }
		}
		regBigMark(c, pc, kind)
	}
}

// regBigMark marks register pc of c in the marks kind picks of its
// bigSeen, made if it has none: register code may be shared by runtimes,
// as a tree is, which mark it alike.
func regBigMark(c *regCode, pc int, kind func(*treeBigSeen) []atomic.Uint32) {
	if pc < 0 || pc >= len(c.rc.Code) {
		return
	}
	s := c.bigSeen.Load()
	if s == nil {
		n := (len(c.rc.Code) + 31) / 32
		s = &treeBigSeen{bigs: make([]atomic.Uint32, n), wide: make([]atomic.Uint32, n), heap: make([]atomic.Uint32, n), recvs: make([]atomic.Uint32, n)}
		if !c.bigSeen.CompareAndSwap(nil, s) {
			s = c.bigSeen.Load()
		}
	}
	if w, bit := &kind(s)[pc>>5], uint32(1)<<(pc&31); w.Load()&bit == 0 {
		w.Or(bit)
	}
}

// regBigSeenAt reports what the register interpreter marked of the
// operator at register pc rpc of f's code (regBigOf), as regBigSeen.
func (f *jitRegFeedback) regBigSeenAt(rpc int) (big, wide, heap bool) {
	return regBigSeen(f.fb.cl, f.rc, rpc)
}

// regBigSeen reports what the register interpreter marked of the operator
// at register pc rpc of cl's register code, rc (regBigOf): whether it met
// BigInts, whether some from 0 to 2^64-1 past the int64 range, and
// whether one on the heap, as jitTreeBig and jitTreeWide report the
// tree's marks.
func regBigSeen(cl *closure, rc *bytecode.RegCode, rpc int) (big, wide, heap bool) {
	if cl == nil || cl.reg == nil || cl.reg.rc != rc || rpc < 0 {
		return false, false, false
	}
	s := cl.reg.bigSeen.Load()
	if s == nil || rpc>>5 >= len(s.bigs) {
		return false, false, false
	}
	bit := uint32(1) << (rpc & 31)
	heap = s.heap[rpc>>5].Load()&bit != 0
	wide = s.wide[rpc>>5].Load()&bit != 0
	return heap || wide || s.bigs[rpc>>5].Load()&bit != 0, wide, heap
}

// BigInt is ssa.BigIntFeedback's: the operator of the program's
// instruction pc met BigInts at an exit of code compiled before
// (jitRegBigIntSeen).
func (f *jitRegFeedback) BigInt(pc int) bool {
	if pc < 0 || pc >= len(f.p.Origin) {
		return false
	}
	if l := f.fb.e; l != nil && slices.Contains(l.bigints, int32(f.p.Origin[pc])) {
		return true
	}
	big, _, _ := f.regBigSeenAt(int(f.p.Origin[pc]))
	return big
}

// BigIntWide is ssa.BigIntFeedback's: the operator of the program's
// instruction pc met BigInts from 0 to 2^64-1, some past the int64 range.
func (f *jitRegFeedback) BigIntWide(pc int) bool {
	if pc < 0 || pc >= len(f.p.Origin) {
		return false
	}
	if l := f.fb.e; l != nil && slices.Contains(l.bigwide, int32(f.p.Origin[pc])) {
		return true
	}
	_, wide, _ := f.regBigSeenAt(int(f.p.Origin[pc]))
	return wide
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
	l := r.jitRegLearnedFor(f, e)
	// What the interpreter marked of it too, as the stack form's passes
	// the tree's marks (jitBigIntSeen).
	big, wide, _ := regBigSeen(f.cl, e.rc, int(p.Origin[site]))
	return jitBigIntNote(l, int32(p.Origin[site]), p.Code[site], func(o ir.Operand) Value { return r.jitRegOperand(p, wb, o) }, big, wide)
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
	l := r.jitRegLearnedFor(f, e)
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
