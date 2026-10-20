package ssa

import "github.com/go-quickjs/go-quickjs/internal/jit/ir"

// BigInts of 64 bits, made natively, as V8's code makes its BigInt64
// values: an operator of BigInts held in their values, unboxed, Int64s
// (OpBigUnbox) or, past the int64 range, Uint64s from 0 to 2^64-1
// (OpBigUnboxU64), which a 64-bit hash or generator holds; and
// BigInt.asUintN(64, ...) and asIntN's of operators made on their operands'
// low 64 bits alone (wraps), as V8 truncates them to machine words.

// BigIntFeedback is Feedback that knows an arithmetic or relational
// operator at a PC met BigInts held in their values, as V8's feedback
// names BigInt64 operands: the code makes it on them unboxed, natively,
// until a speculation fails -- an operand of another type, or a result
// outside the range -- and the site is generic. BigInt is a site's that
// met BigInts at all; BigIntWide one's that met some from 2^63 to 2^64-1,
// and none below -2^63, which the code makes as Uint64s; BigIntFailed
// one's whose speculation failed in code compiled before (Feedback's
// Generic, but for what the tree tier saw of BigInts on the heap).
type BigIntFeedback interface {
	BigInt(pc int) bool
	BigIntWide(pc int) bool
	BigIntFailed(pc int) bool
}

// BigInt constants' kinds (BigIntConstFeedback): one on the heap, one in
// the int64 range, and one from 2^63 to 2^64-1, held in its value past
// the int64 range.
const (
	BigConstHeap = iota
	BigConstInt64
	BigConstUint64
)

// bigFeedback is the builder's BigIntFeedback, or nil.
func (b *builder) bigFeedback() BigIntFeedback {
	fb, _ := b.fb.(BigIntFeedback)
	return fb
}

// bigConst is the bits and kind of v, a BigInt constant's load from its
// cell (ir.BigIntConst), if it is one held in its value.
func bigConst(v *Value) (uint64, int, bool) {
	if v.Op == OpLoadCell && (v.Aux == BigConstInt64 || v.Aux == BigConstUint64) && v.Args[0].Op == OpConstCell {
		return v.Const.Bits, v.Aux, true
	}
	return 0, 0, false
}

// boxedBig is the unboxed BigInt v boxes (OpBigBox), or nil.
func boxedBig(v *Value) *Value {
	if v.Op == OpKept && v.Args[1].Op == OpBigBox {
		return v.Args[1].Args[0]
	}
	return nil
}

// bigOperator reports whether op is one native code makes of two unboxed
// BigInts (bigNative), and whether it makes a BigInt, which needs a keep
// cell for its box.
func bigOperator(op ir.Operator) (native, arith bool) {
	switch op {
	case ir.Add, ir.Sub, ir.Mul, ir.Div, ir.Mod, ir.Pow, ir.BitAnd, ir.BitOr, ir.BitXor, ir.Shl, ir.Shr:
		return true, true
	case ir.Lt, ir.Le, ir.Gt, ir.Ge:
		return true, false
	}
	return false, false
}

// bigType is the type an operator of x and y, BigInts, is made in
// (bigNative): Int64, Uint64, or None where neither holds both -- a
// negative constant with one past the int64 range. An operand already
// unboxed (a box's) or a constant says what it is, but for the site's
// feedback: Uint64 where it met BigInts past the int64 range, operands or
// result (BigIntWide) -- a sum of Int64s that grows past 2^63 -- unless a
// constant is negative, and Int64 otherwise. An unboxed operand of the
// other type is checked to be in both ranges (OpBigSign).
func bigType(wide bool, xs ...*Value) Type {
	unsigned, signed, negative, past := false, false, false, false
	for _, x := range xs {
		if u := boxedBig(x); u != nil {
			unsigned, signed = unsigned || u.Type == Uint64, signed || u.Type == Int64
			continue
		}
		if bits, kind, ok := bigConst(x); ok {
			switch {
			case kind == BigConstUint64:
				past = true
			case int64(bits) < 0:
				negative = true
			}
		}
	}
	switch {
	case past && negative:
		return None
	case past:
		return Uint64
	case negative:
		return Int64
	case wide, unsigned && !signed:
		return Uint64
	}
	return Int64
}

// bigAs is x, a BigInt, unboxed as t: a box's value, checked to be in
// t's range where it is of the other type; a constant's bits; or any
// other value's guarded unboxing, which exits as kind says.
func (b *builder) bigAs(blk *Block, x *Value, t Type, guard func(Op, Type, ir.ExitKind, ...*Value) *Value) *Value {
	if u := boxedBig(x); u != nil {
		if u.Type == t {
			return u
		}
		return guard(OpBigSign, t, ir.GuardExit, u)
	}
	if t == Uint64 {
		return guard(OpBigUnboxU64, Uint64, ir.GuardExit, x)
	}
	return guard(OpBigUnbox, Int64, ir.GuardExit, x)
}

// bigBoxed is x, an unboxed BigInt, as a value: boxed in a keep cell of
// its own (OpBigBox), as a call's result is kept.
func (b *builder) bigBoxed(blk *Block, x *Value) *Value {
	f := b.f
	box := f.newValue(blk, OpBigBox, Tagged, x)
	box.Index = f.Keeps
	f.Keeps++
	cell := f.newValue(blk, OpBigCell, Source, box)
	v := f.newValue(blk, OpKept, Tagged, cell, box)
	v.Shadow = cell
	return v
}

// bigCount is the count of a BigInt shift whose right operand is y, a
// constant from 0 to 63, or -1.
func bigCount(y *Value) int {
	if bits, kind, ok := bigConst(y); ok && kind == BigConstInt64 && bits < 64 {
		return int(bits)
	}
	return -1
}

// bigNative is in, an operator at pc of the root, made on BigInts unboxed
// where the site met BigInts held in their values (BigIntFeedback) and its
// speculation has not failed: an arithmetic or bitwise operator's result
// boxed, which exits where it is outside its type's range, a shift by a
// constant from 0 to 63, a division by none but zero; or a comparison's
// boolean. It is nil for any other, which goArith makes, or the number's.
func (b *builder) bigNative(blk *Block, pc int, in ir.Instruction, operand func(ir.Operand) *Value, guard func(Op, Type, ir.ExitKind, ...*Value) *Value) *Value {
	op := in.Operator
	native, arith := bigOperator(op)
	fb := b.bigFeedback()
	if !native || fb == nil || b.cur != b.root || b.generic(pc) || !fb.BigInt(pc) || arith && b.f.Keeps >= b.f.keepLimit() ||
		in.Left.Slot < 0 || in.Right.Slot < 0 {
		return nil
	}
	f := b.f
	x, y := operand(in.Left), operand(in.Right)
	count := 0
	if op == ir.Shl || op == ir.Shr {
		if count = bigCount(y); count < 0 {
			return nil
		}
	}
	var t Type
	if op == ir.Shl || op == ir.Shr {
		t = bigType(fb.BigIntWide(pc), x)
	} else {
		// A power has no Uint64 form: its operands are Int64s, or it
		// exits.
		t = bigType(fb.BigIntWide(pc) && op != ir.Pow, x, y)
	}
	if t == None || op == ir.Pow && t != Int64 {
		return nil
	}
	xi := b.bigAs(blk, x, t, guard)
	if op == ir.Shl || op == ir.Shr {
		var r *Value
		switch {
		case op == ir.Shr && t == Uint64:
			r = f.newValue(blk, OpShrW, t, xi)
		case op == ir.Shr:
			r = f.newValue(blk, OpSarW, t, xi)
		case t == Uint64:
			r = guard(OpShlU64, t, ir.GuardExit, xi)
		default:
			r = guard(OpShlI64, t, ir.GuardExit, xi)
		}
		r.Index = count
		return b.bigBoxed(blk, r)
	}
	yi := b.bigAs(blk, y, t, guard)
	var r *Value
	signed := t == Int64
	switch op {
	case ir.Add:
		r = guard(pick(signed, OpAddI64, OpAddU64), t, ir.GuardExit, xi, yi)
	case ir.Sub:
		r = guard(pick(signed, OpSubI64, OpSubU64), t, ir.GuardExit, xi, yi)
	case ir.Mul:
		r = guard(pick(signed, OpMulI64, OpMulU64), t, ir.GuardExit, xi, yi)
	case ir.Div:
		r = guard(pick(signed, OpDivI64, OpDivU64), t, ir.GuardExit, xi, yi)
	case ir.Mod:
		r = guard(pick(signed, OpModI64, OpModU64), t, ir.GuardExit, xi, yi)
	case ir.Pow:
		r = guard(OpPowI64, t, ir.GuardExit, xi, yi)
	case ir.BitAnd:
		r = f.newValue(blk, OpAndW, t, xi, yi)
	case ir.BitOr:
		r = f.newValue(blk, OpOrW, t, xi, yi)
	case ir.BitXor:
		r = f.newValue(blk, OpXorW, t, xi, yi)
	default:
		c := f.newValue(blk, OpCmpI64, Bool, xi, yi)
		c.Aux = int(op)
		return c
	}
	return b.bigBoxed(blk, r)
}

// bigUnary is in, a negation or a bitwise not at pc of the root, made on a
// BigInt unboxed as an Int64 where the site met BigInts held in their
// values (BigIntFeedback) and its speculation has not failed: -x exits for
// the most negative int64, ~x nowhere; or nil.
func (b *builder) bigUnary(blk *Block, pc int, in ir.Instruction, operand func(ir.Operand) *Value, guard func(Op, Type, ir.ExitKind, ...*Value) *Value) *Value {
	fb := b.bigFeedback()
	if in.Operator != ir.Neg && in.Operator != ir.BitNot || fb == nil || b.cur != b.root || b.generic(pc) || !fb.BigInt(pc) ||
		b.f.Keeps >= b.f.keepLimit() || in.Left.Slot < 0 {
		return nil
	}
	f := b.f
	x := b.bigAs(blk, operand(in.Left), Int64, guard)
	c := f.newValue(blk, OpConstI64, Int64)
	var r *Value
	if in.Operator == ir.Neg {
		r = guard(OpSubI64, Int64, ir.GuardExit, c, x)
	} else {
		c.Const = ir.Value{Bits: ^uint64(0)}
		r = f.newValue(blk, OpXorW, Int64, x, c)
	}
	return b.bigBoxed(blk, r)
}

// relationalBig is bigNative's comparison, for a branch on one, or nil.
func (b *builder) relationalBig(blk *Block, pc int, in ir.Instruction, operand func(ir.Operand) *Value, guard func(Op, Type, ir.ExitKind, ...*Value) *Value) *Value {
	if !relational(in.Operator) {
		return nil
	}
	return b.bigNative(blk, pc, in, operand, guard)
}

// pick is a if signed, b if not.
func pick(signed bool, a, b Op) Op {
	if signed {
		return a
	}
	return b
}

// wrapOperator reports whether op's result's low 64 bits are those of its
// operands' low 64 bits under it: the sum, the difference, the product,
// the bitwise operators, and a left shift by a constant below 64.
func wrapOperator(op ir.Operator) bool {
	switch op {
	case ir.Add, ir.Sub, ir.Mul, ir.BitAnd, ir.BitOr, ir.BitXor, ir.Shl:
		return true
	}
	return false
}

// wraps reports whether the operator at pc of the root is made on its
// operands' low 64 bits, a word, as V8 truncates BigInt.asUintN(64, x op y)
// to machine words: one wrapOperator names, whose result only such an
// operator or BigInt.asUintN or asIntN of 64 bits or fewer takes (asN),
// before which nothing runs but reads of locals and constants and other
// such operators -- what nothing has changed by, so that their guards and
// the call's exit to the state before the first of them, and Go makes
// them over again; no state has the operator's result, which is no value.
// Nor is any speculation of theirs one that failed (BigIntFailed).
func (b *builder) wraps(pc int) bool {
	if b.cur != b.root {
		return false
	}
	if b.wrapped == nil {
		b.wrapped = b.f.bytes(len(b.p.Code))
	}
	switch b.wrapped[pc] {
	case 1:
		return true
	case 2:
		return false
	}
	// What it takes is after it: no cycle comes back here.
	b.wrapped[pc] = 2
	if b.wrapsAt(pc) {
		b.wrapped[pc] = 1
		return true
	}
	return false
}

func (b *builder) wrapsAt(pc int) bool {
	p := b.p
	in := p.Code[pc]
	fb := b.bigFeedback()
	if fb == nil || in.Op != ir.Binary || !wrapOperator(in.Operator) || in.Left.Slot < 0 || in.Right.Slot < 0 ||
		!reachable(p, pc) || pc+1 >= len(p.Code) || fb.BigIntFailed(pc) {
		return false
	}
	if in.Operator == ir.Shl && b.wrapCount(pc, in.Right.Slot) < 0 {
		return false
	}
	d := in.Dest
	if d != p.Locals+p.Maps[pc+1].Depth-1 {
		return false
	}
	for k := pc + 1; k < len(p.Code); k++ {
		if !reachable(p, k) || b.blockAt[k] != nil {
			return false
		}
		c := p.Code[k]
		if b.wrapReads(k, d) {
			return b.wrapTakes(k, d)
		}
		switch c.Op {
		case ir.Nop:
		case ir.Copy:
			if c.Check || c.Dest <= d || !b.wrapLocal(c.Left) {
				return false
			}
		case ir.CopyPair:
			if c.Dest <= d || !b.wrapLocal(c.Left) || !b.wrapLocal(c.Right) {
				return false
			}
		case ir.BigIntConst:
			cf, ok := b.fb.(BigIntConstFeedback)
			if !ok || c.Dest <= d {
				return false
			}
			if cell, _, _, ok := cf.BigIntCell(k); !ok || cell == 0 {
				return false
			}
		case ir.Binary:
			if c.Dest <= d || !b.wraps(k) {
				return false
			}
		default:
			return false
		}
	}
	return false
}

// wrapLocal reports whether o is a literal or a local's read that makes
// nothing and can exit nowhere: not a captured binding's, nor a captured
// local's.
func (b *builder) wrapLocal(o ir.Operand) bool {
	p := b.p
	return o.Slot < 0 || o.Slot < p.Locals && !p.IsCaptured(o.Slot) && !(o.Slot >= p.UpvalueBase && o.Slot < p.UpvalueBase+p.Upvalues)
}

// wrapReads reports whether the instruction at k reads slot d, a value a
// wrapped operator made: an operand of an operator, or of a call.
func (b *builder) wrapReads(k, d int) bool {
	c := b.p.Code[k]
	switch c.Op {
	case ir.Host, ir.Call:
		return d >= b.p.Locals && d < b.p.Locals+b.p.Maps[k].Depth
	}
	return c.Left.Slot == d || c.Right.Slot == d || c.Third.Slot == d
}

// wrapTakes reports whether the instruction at k, which reads slot d, a
// value a wrapped operator made, takes only its low 64 bits: a wrapped
// operator's operand -- not a shift's count -- or the last argument of a
// call of BigInt.asUintN or asIntN (asN).
func (b *builder) wrapTakes(k, d int) bool {
	c := b.p.Code[k]
	switch c.Op {
	case ir.Binary:
		if c.Operator == ir.Shl && c.Right.Slot == d || c.Left.Slot == c.Right.Slot {
			return false
		}
		return b.wraps(k)
	case ir.Host:
		_, ok := b.asN(k)
		return ok && d == b.p.Locals+b.p.Maps[k].Depth-1 && b.f.Keeps < b.f.keepLimit()
	}
	return false
}

// wrapCount is the count of the shift at pc whose right operand is slot s,
// the constant the instruction before it pushes, from 0 to 63, or -1: one
// it alone comes after, not a join's -- x << (c ? 70n : 1n).
func (b *builder) wrapCount(pc, s int) int {
	p := b.p
	if pc == 0 || b.blockAt[pc] != nil || p.Code[pc-1].Op != ir.BigIntConst || p.Code[pc-1].Dest != s {
		return -1
	}
	cf, ok := b.fb.(BigIntConstFeedback)
	if !ok {
		return -1
	}
	if _, bits, kind, ok := cf.BigIntCell(pc - 1); ok && kind == BigConstInt64 && bits < 64 {
		return int(bits)
	}
	return -1
}

// asN is the intrinsic of the call at pc if it is BigInt.asUintN's or
// asIntN's (OpZextW, OpSextW), whose width Fn is.
func (b *builder) asN(pc int) (Intrinsic, bool) {
	k, ok := b.intrinsic(pc)
	return k, ok && (k.Op == OpZextW || k.Op == OpSextW)
}

// wordOf is the word of a wrapped operator's operand x: the word the
// operator before it made, or its low 64 bits, guarded to be a BigInt's.
func (b *builder) wordOf(x *Value, guard func(Op, Type, ir.ExitKind, ...*Value) *Value) *Value {
	if w := b.words[x]; w != nil {
		return w
	}
	return guard(OpBigLow, Uint64, ir.GuardExit, x)
}

// wrapOp makes in, at pc, a wrapped operator (wraps), of its operands'
// words: its slot holds a placeholder no state may name until the
// operator or the call that takes it does (frameState). The first of a
// region's begins it, its state the one its guards and the call's exit to.
func (b *builder) wrapOp(blk *Block, pc int, in ir.Instruction, guard func(Op, Type, ir.ExitKind, ...*Value) *Value) {
	f := b.f
	if b.region == nil {
		b.region = b.rootState()
	}
	if b.words == nil {
		b.words = map[*Value]*Value{}
	}
	x := b.wordOf(b.read(in.Left.Slot, blk), guard)
	var w *Value
	if in.Operator == ir.Shl {
		w = f.newValue(blk, OpShlW, Uint64, x)
		w.Index = b.wrapCount(pc, in.Right.Slot)
	} else {
		y := b.wordOf(b.read(in.Right.Slot, blk), guard)
		op := OpAddW
		switch in.Operator {
		case ir.Sub:
			op = OpSubW
		case ir.Mul:
			op = OpMulW
		case ir.BitAnd:
			op = OpAndW
		case ir.BitOr:
			op = OpOrW
		case ir.BitXor:
			op = OpXorW
		}
		w = f.newValue(blk, op, Uint64, x, y)
	}
	ph := f.newValue(blk, OpConst, Tagged)
	ph.Const = ir.Value{Kind: ir.Uninitialized}
	b.words[ph] = w
	b.assign(in.Dest, blk, ph)
}

// asNCall makes BigInt.asUintN(n, x) or asIntN's, the call at pc (asN), as
// V8 reduces it: checked to call the function, x's low bits -- a wrapped
// operator's word, or x's, guarded to be a BigInt -- cut to the width,
// boxed. It ends a region of wrapped operators (wraps).
func (b *builder) asNCall(blk *Block, pc int, k Intrinsic, guard func(Op, Type, ir.ExitKind, ...*Value) *Value) {
	f := b.f
	sp := b.cur.base + b.p.Locals + b.p.Maps[pc].Depth
	x := b.read(sp-1, blk)
	w := b.words[x]
	kind := ir.HostExit
	if w != nil {
		// Its state is the region's, before the operators: Go makes them
		// over again, and the call, only once the code is compiled for
		// the function it calls now.
		kind = ir.GuardExit
	}
	object := guard(OpObjectOf, Ptr, kind, b.read(sp-3, blk))
	same := guard(OpSameObject, None, kind, object)
	same.Const = ir.Value{Bits: uint64(k.Callee)}
	if w == nil {
		w = guard(OpBigLow, Uint64, ir.HostExit, x)
	}
	b.region, b.words = nil, nil
	var r *Value
	switch {
	case k.Fn == 0:
		r = f.newValue(blk, OpConstI64, Int64)
	case k.Op == OpZextW:
		r = f.newValue(blk, OpZextW, Uint64, w)
		r.Index = k.Fn
	default:
		r = f.newValue(blk, OpSextW, Int64, w)
		r.Index = k.Fn
	}
	b.assign(b.cur.base+b.p.Locals+b.p.Maps[pc+1].Depth-1, blk, b.bigBoxed(blk, r))
}

// bigOfCall makes BigInt(x), the call at pc, of a number, as V8 reduces
// it: checked to call the function, x checked to be an integer in the
// int64 range (OpBigOfF64), boxed; any other x is Go's call.
func (b *builder) bigOfCall(blk *Block, pc int, k Intrinsic, guard func(Op, Type, ir.ExitKind, ...*Value) *Value) {
	sp := b.cur.base + b.p.Locals + b.p.Maps[pc].Depth
	object := guard(OpObjectOf, Ptr, ir.HostExit, b.read(sp-2, blk))
	same := guard(OpSameObject, None, ir.HostExit, object)
	same.Const = ir.Value{Bits: uint64(k.Callee)}
	x := guard(OpUnboxF64, Float64, ir.HostExit, b.read(sp-1, blk))
	r := guard(OpBigOfF64, Int64, ir.HostExit, x)
	b.assign(b.cur.base+b.p.Locals+b.p.Maps[pc+1].Depth-1, blk, b.bigBoxed(blk, r))
}

// BigInt blocks a call of Go that makes a BigInt's operator may give back
// once it has made its result (CallSite's Release), as the tree tier gives
// back those of its fresh operands and of a local holding its values alone
// (the VM's bigRelease): the left operand, the right one -- each an
// operator's result that only the call takes -- and the value of the local
// the result is stored to next, which holds its values alone, the call's
// third operand, the result marked as the local's alone.
const (
	ReleaseLeft = 1 << iota
	ReleaseRight
	ReleaseOld
)

// BigIntRecycleFeedback is Feedback that says which of a function's first
// 64 locals hold their values alone, read only as operands of operators
// whose results are values of their own (the VM's soleLocals), and whether
// its code may give BigInts' blocks back at all: not a generator's or an
// async function's, whose frames suspend.
type BigIntRecycleFeedback interface {
	BigIntSole() (sole uint64, ok bool)
}

// bigRelease is what the call of Go that makes in, a BigInt's operator at
// pc of the root, may give back (CallSite's Release), and the local's value
// it passes for it, or nil: of a site that met BigInts (BigIntFeedback),
// as the tree tier marks its operators (its freshPC and ownedPC).
func (b *builder) bigRelease(blk *Block, pc int, in ir.Instruction) (uint8, *Value) {
	rf, ok := b.fb.(BigIntRecycleFeedback)
	fb := b.bigFeedback()
	if !ok || fb == nil || b.cur != b.root || !fb.BigInt(pc) || in.Left.Slot < 0 || in.Right.Slot < 0 {
		return 0, nil
	}
	if !b.soleAsked {
		b.sole, b.soleOK = rf.BigIntSole()
		b.soleAsked = true
	}
	if !b.soleOK {
		return 0, nil
	}
	var r uint8
	if b.bigFresh(pc, in.Left.Slot) {
		r |= ReleaseLeft
	}
	if in.Right.Slot != in.Left.Slot && b.bigFresh(pc, in.Right.Slot) {
		r |= ReleaseRight
	}
	if k, ok := b.bigOwner(pc, in); ok && k < 64 && b.sole&(1<<uint(k)) != 0 {
		return r | ReleaseOld, b.read(k, blk)
	}
	return r, nil
}

// bigLeafSites clears CallSite's Big in code that gives no BigInt's block
// back -- no site's Release of an operand that may be on the heap, a box
// of native code's never is -- whose calls the VM's leaf would only try:
// it makes a result in a block given back, which such code has none of.
// A sum's of powers, s += BigInt(i) ** 5n, gives back only its right
// operand, a power native code made.
func bigLeafSites(f *Func) {
	var big []*CallSite
	for _, b := range f.Blocks {
		for _, v := range b.Values {
			for _, c := range v.Calls() {
				if c.Release&ReleaseOld != 0 || c.Release&ReleaseLeft != 0 && boxedBig(v.Args[0]) == nil ||
					c.Release&ReleaseRight != 0 && boxedBig(v.Args[1]) == nil {
					return
				}
				if c.Big {
					big = append(big, c)
				}
			}
		}
	}
	for _, c := range big {
		c.Big = false
	}
}

// bigArith reports whether op is an operator whose result is a value of
// its own: arithmetic, bitwise, a shift.
func bigArith(op ir.Operator) bool {
	switch op {
	case ir.Add, ir.Sub, ir.Mul, ir.Div, ir.Mod, ir.Pow, ir.BitAnd, ir.BitOr, ir.BitXor, ir.Shl, ir.Shr:
		return true
	}
	return false
}

// bigFresh reports whether slot, an operand of the instruction at pc,
// holds the result of an operator (bigArith) that nothing but that
// instruction takes: an operand slot an operator wrote earlier in the
// block, which no instruction since has read or moved -- each since kept
// to the slots past it, by the stack's depths.
func (b *builder) bigFresh(pc, slot int) bool {
	p := b.p
	d := slot - p.Locals
	if d < 0 {
		return false
	}
	for k := pc - 1; k >= 0; k-- {
		if b.blockAt[k+1] != nil || !reachable(p, k) {
			return false
		}
		c := p.Code[k]
		before, after := p.Maps[k].Depth, p.Maps[k+1].Depth
		if c.Op == ir.Binary && c.Dest == slot && after == d+1 && before >= d && before <= d+2 {
			// The operator that made it: of two operands on the stack, of
			// one and a local or a constant, or of a local and a constant.
			return bigArith(c.Operator)
		}
		if c.Left.Slot == slot || c.Right.Slot == slot || c.Third.Slot == slot {
			return false
		}
		switch {
		case c.Op == ir.Swap || c.Op == ir.Insert2 || c.Op == ir.Insert3:
			return false
		case before >= d+1 && after >= d+2:
			// What it pops and pushes is past the slot.
		case before >= d+1 && after == d+1 && (c.Op == ir.Nop || c.Op == ir.Copy && c.Dest < p.Locals):
			// A drop, or a store to a local, of what is past it.
		default:
			return false
		}
	}
	return false
}

// bigOwner is the local the instruction right after pc stores in's result
// to, taking it off the stack, as the tree tier's ownedStore takes it: one
// that checks nothing, whose guard's state would still have the value the
// local held, given back.
func (b *builder) bigOwner(pc int, in ir.Instruction) (int, bool) {
	p := b.p
	k := pc + 1
	if k+1 >= len(p.Maps) || b.blockAt[k] != nil || !reachable(p, k) {
		return 0, false
	}
	c := p.Code[k]
	switch {
	case c.Op == ir.Copy && c.Left.Slot == in.Dest && c.Dest >= 0 && c.Dest < p.Locals && !c.Check && p.Maps[k+1].Depth == p.Maps[k].Depth-1:
	case c.Op == ir.StoreLoad && c.Left.Slot == in.Dest && c.Dest >= 0 && c.Dest < p.Locals && !c.Check:
	default:
		return 0, false
	}
	if p.IsCaptured(c.Dest) || c.Dest >= p.UpvalueBase && c.Dest < p.UpvalueBase+p.Upvalues {
		return 0, false
	}
	return c.Dest, true
}
