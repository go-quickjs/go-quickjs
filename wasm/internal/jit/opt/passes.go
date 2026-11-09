package opt

import (
	"math"
	"math/bits"
	"slices"
)

// Optimize runs the passes over f, which Build made, leaving it ready to
// lower: every critical edge split, no copies, no trivial phis. check, if
// not nil, is called with each pass's name after it (a test's verifier).
func Optimize(f *Func, check func(pass string)) {
	run := func(name string, pass func(*Func)) {
		pass(f)
		if check != nil {
			check(name)
		}
	}
	run("copyelim", copyelim)
	run("gvn", gvn)
	run("copyelim", copyelim)
	run("fold", fold)
	run("copyelim", copyelim)
	run("thread", thread)
	run("copyelim", copyelim)
	run("gvn", gvn)
	run("copyelim", copyelim)
	run("licm", licm)
	run("dce", dce)
	run("bce", bce)
	run("split", splitCriticalEdges)
	run("loops", func(f *Func) {
		findLoops(f, dominators(f))
		loopDepths(f)
	})
	computeUses(f)
}

// resolve follows v's copies to the value it is.
func resolve(v *Value) *Value {
	for v.Op == OpCopy {
		v = v.Args[0]
	}
	return v
}

// copyelim replaces each use of a copy by the value copied, and each
// trivial phi -- one whose arguments are all one value, or itself -- by
// that value, removing them.
func copyelim(f *Func) {
	for changed := true; changed; {
		changed = false
		for _, b := range f.Blocks {
			for _, p := range b.Phis {
				var same *Value
				trivial := true
				for _, a := range p.Args {
					a = resolve(a)
					if a == p || a == same {
						continue
					}
					if same != nil {
						trivial = false
						break
					}
					same = a
				}
				if trivial && same != nil {
					p.Op, p.Args = OpCopy, []*Value{same}
					changed = true
				}
			}
			if len(b.Phis) > 0 {
				keep := b.Phis[:0]
				for _, p := range b.Phis {
					if p.Op == OpPhi {
						keep = append(keep, p)
					} else {
						// A phi made a copy goes among the values, where its
						// uses are rewritten and it is removed.
						b.Values = append([]*Value{p}, b.Values...)
					}
				}
				b.Phis = keep
			}
		}
	}
	for _, b := range f.Blocks {
		for _, v := range b.Phis {
			for i, a := range v.Args {
				v.Args[i] = resolve(a)
			}
		}
		for _, v := range b.Values {
			for i, a := range v.Args {
				v.Args[i] = resolve(a)
			}
		}
		if b.Control != nil {
			b.Control = resolve(b.Control)
		}
		for i, a := range b.Results {
			b.Results[i] = resolve(a)
		}
	}
	for _, b := range f.Blocks {
		b.Values = slices.DeleteFunc(b.Values, func(v *Value) bool { return v.Op == OpCopy })
	}
}

// dce removes the values nothing uses that have no effect, and the phis
// only they use.
func dce(f *Func) {
	computeUses(f)
	var work []*Value
	for _, b := range f.Blocks {
		for _, v := range b.Phis {
			if v.Uses == 0 {
				work = append(work, v)
			}
		}
		for _, v := range b.Values {
			if v.Uses == 0 && !v.HasEffect() {
				work = append(work, v)
			}
		}
	}
	dead := map[*Value]bool{}
	for len(work) > 0 {
		v := work[len(work)-1]
		work = work[:len(work)-1]
		if dead[v] {
			continue
		}
		dead[v] = true
		for _, a := range v.Args {
			if a == v {
				continue
			}
			a.Uses--
			if a.Uses == 0 && !a.HasEffect() && !dead[a] {
				work = append(work, a)
			}
		}
	}
	// A phi that only phis that are dead use, its own self included, was
	// counted as used: those in cycles of phis are found next.
	for _, b := range f.Blocks {
		for _, v := range b.Phis {
			if dead[v] || v.Uses == 0 {
				continue
			}
			self := int32(0)
			for _, a := range v.Args {
				if a == v {
					self++
				}
			}
			if self == v.Uses {
				dead[v] = true
			}
		}
	}
	if len(dead) == 0 {
		return
	}
	for _, b := range f.Blocks {
		b.Phis = slices.DeleteFunc(b.Phis, func(v *Value) bool { return dead[v] })
		b.Values = slices.DeleteFunc(b.Values, func(v *Value) bool { return dead[v] })
	}
	computeUses(f)
}

// fold folds constants, simplifies identities, and branches on constants,
// removing the blocks they no longer reach.
func fold(f *Func) {
	for _, b := range f.Blocks {
		for i := 0; i < len(b.Values); i++ {
			v := b.Values[i]
			foldValue(v)
			if v.Op == OpWrap {
				i += narrow(f, v, i)
			}
		}
	}
	changed := false
	for _, b := range f.Blocks {
		if b.Control != nil {
			b.Control = resolve(b.Control)
		}
		c := b.Control
		switch {
		case b.Kind == BIf && c.Op == OpConst:
			taken := 0
			if uint32(c.Aux) == 0 {
				taken = 1
			}
			removeEdge(b, 1-taken)
			b.Kind, b.Control = BPlain, nil
			changed = true
		case b.Kind == BSwitch && c.Op == OpConst:
			i := uint64(uint32(c.Aux))
			if i >= uint64(len(b.Table)) {
				i = uint64(len(b.Table) - 1)
			}
			keep := b.Succs[b.Table[i]]
			for j := len(b.Succs) - 1; j >= 0; j-- {
				if b.Succs[j] != keep {
					removeEdge(b, j)
				}
			}
			b.Kind, b.Control, b.Table = BPlain, nil, nil
			changed = true
		}
	}
	if changed {
		removeUnreachable(f)
	}
}

// constant makes v the constant bits of its type.
func (v *Value) setConst(bits uint64) {
	switch v.Type {
	case TI32, TF32:
		bits = uint64(uint32(bits))
	}
	v.Op, v.Args, v.Aux, v.Aux2 = OpConst, nil, bits, 0
}

func (v *Value) setCopy(a *Value) { v.Op, v.Args, v.Aux, v.Aux2 = OpCopy, []*Value{a}, 0, 0 }

func isConst(v *Value) bool { return v.Op == OpConst }

// constIs reports whether v is the integer constant c of its type.
func constIs(v *Value, c int64) bool {
	if v.Op != OpConst {
		return false
	}
	if v.Type == TI32 {
		return uint32(v.Aux) == uint32(c)
	}
	return v.Aux == uint64(c)
}

// foldValue folds v if its arguments let it.
func foldValue(v *Value) {
	for i, a := range v.Args {
		v.Args[i] = resolve(a)
	}
	switch len(v.Args) {
	case 1:
		foldUnary(v)
	case 2:
		if v.Commutes() && isConst(v.Args[0]) && !isConst(v.Args[1]) {
			v.Args[0], v.Args[1] = v.Args[1], v.Args[0]
		}
		foldBinary(v)
	case 3:
		if v.Op != OpSelect {
			return
		}
		x, y, c := v.Args[0], v.Args[1], v.Args[2]
		switch {
		case x == y:
			v.setCopy(x)
		case isConst(c) && uint32(c.Aux) != 0:
			v.setCopy(x)
		case isConst(c):
			v.setCopy(y)
		}
	}
}

// narrow makes i32.wrap_i64 of a 64-bit operation of extended i32s and
// constants the 32-bit operation of the i32s, whose result is the low half
// of the 64-bit one's: add, sub, mul, and, or, xor, and shl by less than
// 32. v is block's value i; narrow returns the number of values it puts
// before it, the constants it makes.
func narrow(f *Func, v *Value, i int) int {
	a := v.Args[0]
	if a.Op == OpExtendU || a.Op == OpExtendS {
		v.setCopy(a.Args[0])
		return 0
	}
	switch a.Op {
	case OpAdd, OpSub, OpMul, OpAnd, OpOr, OpXor:
	case OpShl:
		if a.Args[1].Op != OpConst || a.Args[1].Aux&63 >= 32 {
			return 0
		}
	default:
		return 0
	}
	var args [2]*Value
	consts := 0
	for k, x := range a.Args {
		x = resolve(x)
		switch x.Op {
		case OpExtendU, OpExtendS:
			args[k] = x.Args[0]
		case OpConst:
			args[k] = &Value{ID: f.nextValue, Op: OpConst, Type: TI32, Aux: uint64(uint32(x.Aux)), Block: v.Block}
			f.nextValue++
			consts++
		default:
			return 0
		}
	}
	b := v.Block
	var made []*Value
	for _, x := range args {
		if x.Op == OpConst && x.ID >= f.nextValue-int32(consts) {
			made = append(made, x)
		}
	}
	b.Values = append(b.Values[:i], append(made, b.Values[i:]...)...)
	v.Op, v.Args = a.Op, []*Value{args[0], args[1]}
	foldValue(v)
	return len(made)
}

func foldUnary(v *Value) {
	a := v.Args[0]
	if v.Op == OpEqz {
		// eqz of an integer comparison is the comparison inverted.
		if inv, ok := invertCmp[a.Op]; ok {
			v.Op, v.Args = inv, slices.Clone(a.Args)
			return
		}
	}
	if !isConst(a) {
		return
	}
	x := a.Aux
	w32 := a.Type == TI32 || a.Type == TF32
	switch v.Op {
	case OpClz:
		if w32 {
			v.setConst(uint64(bits.LeadingZeros32(uint32(x))))
		} else {
			v.setConst(uint64(bits.LeadingZeros64(x)))
		}
	case OpCtz:
		if w32 {
			v.setConst(uint64(bits.TrailingZeros32(uint32(x))))
		} else {
			v.setConst(uint64(bits.TrailingZeros64(x)))
		}
	case OpPopcnt:
		if w32 {
			v.setConst(uint64(bits.OnesCount32(uint32(x))))
		} else {
			v.setConst(uint64(bits.OnesCount64(x)))
		}
	case OpEqz:
		if (w32 && uint32(x) == 0) || (!w32 && x == 0) {
			v.setConst(1)
		} else {
			v.setConst(0)
		}
	case OpWrap:
		v.setConst(uint64(uint32(x)))
	case OpExtendS:
		v.setConst(uint64(int64(int32(uint32(x)))))
	case OpExtendU:
		v.setConst(uint64(uint32(x)))
	case OpExtend8S:
		v.setConst(uint64(int64(int8(x))))
	case OpExtend16S:
		v.setConst(uint64(int64(int16(x))))
	case OpExtend32S:
		v.setConst(uint64(int64(int32(x))))
	case OpReinterpret:
		v.setConst(x)
	case OpFNeg:
		if w32 {
			v.setConst(x ^ 1<<31)
		} else {
			v.setConst(x ^ 1<<63)
		}
	case OpFAbs:
		if w32 {
			v.setConst(x &^ (1 << 31))
		} else {
			v.setConst(x &^ (1 << 63))
		}
	case OpPromote:
		// Exact, but for a NaN, whose payload the instruction decides.
		if f := math.Float32frombits(uint32(x)); f == f {
			v.setConst(math.Float64bits(float64(f)))
		}
	case OpDemote:
		if d := math.Float64frombits(x); d == d {
			v.setConst(uint64(math.Float32bits(float32(d))))
		}
	case OpConvertS, OpConvertU:
		// Only what converts exactly, which every rounding agrees on.
		var n float64
		var exact bool
		switch {
		case w32 && v.Op == OpConvertS:
			n, exact = float64(int32(uint32(x))), true
		case w32:
			n, exact = float64(uint32(x)), true
		case v.Op == OpConvertS:
			n = float64(int64(x))
			exact = int64(x) > -1<<53 && int64(x) < 1<<53
		default:
			n = float64(x)
			exact = x < 1<<53
		}
		if v.Type == TF32 {
			if exact && float64(float32(n)) == n {
				v.setConst(uint64(math.Float32bits(float32(n))))
			}
		} else if exact {
			v.setConst(math.Float64bits(n))
		}
	}
}

// invertCmp is each integer comparison's inverse.
var invertCmp = map[Op]Op{
	OpEq: OpNe, OpNe: OpEq, OpLtS: OpGeS, OpGeS: OpLtS, OpLtU: OpGeU, OpGeU: OpLtU,
	OpGtS: OpLeS, OpLeS: OpGtS, OpGtU: OpLeU, OpLeU: OpGtU,
}

func foldBinary(v *Value) {
	x, y := v.Args[0], v.Args[1]
	t := v.Type
	if v.Op >= OpEq && v.Op <= OpGeU {
		t = x.Type
	}
	if t.IsInt() && v.Op <= OpGeU {
		if isConst(x) && isConst(y) {
			if r, ok := foldInt(v.Op, t == TI64, x.Aux, y.Aux); ok {
				v.setConst(r)
			}
			return
		}
		if !isConst(y) {
			if x == y {
				switch v.Op {
				case OpSub, OpXor:
					v.setConst(0)
				case OpAnd, OpOr:
					v.setCopy(x)
				case OpEq, OpLeS, OpLeU, OpGeS, OpGeU:
					v.setConst(1)
				case OpNe, OpLtS, OpLtU, OpGtS, OpGtU:
					v.setConst(0)
				}
			}
			return
		}
		switch v.Op {
		case OpAdd, OpSub, OpOr, OpXor, OpShl, OpShrS, OpShrU, OpRotl, OpRotr:
			if constIs(y, 0) || (v.Op >= OpShl && (y.Aux&shiftMask(t)) == 0) {
				v.setCopy(x)
			}
		case OpMul:
			if constIs(y, 1) {
				v.setCopy(x)
			} else if constIs(y, 0) {
				v.setConst(0)
			}
		case OpAnd:
			if constIs(y, -1) {
				v.setCopy(x)
			} else if constIs(y, 0) {
				v.setConst(0)
			}
		case OpDivS, OpDivU:
			if constIs(y, 1) {
				v.setCopy(x)
			}
		}
		return
	}
	if !t.IsFloat() || !isConst(x) || !isConst(y) {
		return
	}
	switch v.Op {
	case OpFAdd, OpFSub, OpFMul, OpFDiv:
		// Without a NaN, every IEEE implementation gives these bits.
		if t == TF32 {
			a, b := math.Float32frombits(uint32(x.Aux)), math.Float32frombits(uint32(y.Aux))
			var r float32
			switch v.Op {
			case OpFAdd:
				r = a + b
			case OpFSub:
				r = a - b
			case OpFMul:
				r = a * b
			default:
				r = a / b
			}
			if a == a && b == b && r == r {
				v.setConst(uint64(math.Float32bits(r)))
			}
			return
		}
		a, b := math.Float64frombits(x.Aux), math.Float64frombits(y.Aux)
		var r float64
		switch v.Op {
		case OpFAdd:
			r = a + b
		case OpFSub:
			r = a - b
		case OpFMul:
			r = a * b
		default:
			r = a / b
		}
		if a == a && b == b && r == r {
			v.setConst(math.Float64bits(r))
		}
	case OpFCopysign:
		sign := uint64(1) << 63
		if t == TF32 {
			sign = 1 << 31
		}
		v.setConst(x.Aux&^sign | y.Aux&sign)
	case OpFEq, OpFNe, OpFLt, OpFGt, OpFLe, OpFGe:
		var a, b float64
		if x.Type == TF32 {
			a, b = float64(math.Float32frombits(uint32(x.Aux))), float64(math.Float32frombits(uint32(y.Aux)))
		} else {
			a, b = math.Float64frombits(x.Aux), math.Float64frombits(y.Aux)
		}
		var r bool
		switch v.Op {
		case OpFEq:
			r = a == b
		case OpFNe:
			r = a != b
		case OpFLt:
			r = a < b
		case OpFGt:
			r = a > b
		case OpFLe:
			r = a <= b
		default:
			r = a >= b
		}
		if r {
			v.setConst(1)
		} else {
			v.setConst(0)
		}
	}
}

func shiftMask(t Type) uint64 {
	if t == TI64 {
		return 63
	}
	return 31
}

func b2u(b bool) uint64 {
	if b {
		return 1
	}
	return 0
}

// foldInt is integer operation op of constants x and y, 64 bits wide or
// 32, or false where it traps.
func foldInt(op Op, wide bool, x, y uint64) (uint64, bool) {
	if !wide {
		a, b := uint32(x), uint32(y)
		sa, sb := int32(a), int32(b)
		var r uint32
		switch op {
		case OpAdd:
			r = a + b
		case OpSub:
			r = a - b
		case OpMul:
			r = a * b
		case OpAnd:
			r = a & b
		case OpOr:
			r = a | b
		case OpXor:
			r = a ^ b
		case OpShl:
			r = a << (b & 31)
		case OpShrS:
			r = uint32(sa >> (b & 31))
		case OpShrU:
			r = a >> (b & 31)
		case OpRotl:
			r = bits.RotateLeft32(a, int(b&31))
		case OpRotr:
			r = bits.RotateLeft32(a, -int(b&31))
		case OpDivS:
			if b == 0 || (sa == math.MinInt32 && sb == -1) {
				return 0, false
			}
			r = uint32(sa / sb)
		case OpDivU:
			if b == 0 {
				return 0, false
			}
			r = a / b
		case OpRemS:
			if b == 0 {
				return 0, false
			}
			if sb == -1 {
				r = 0
			} else {
				r = uint32(sa % sb)
			}
		case OpRemU:
			if b == 0 {
				return 0, false
			}
			r = a % b
		case OpEq:
			r = uint32(b2u(a == b))
		case OpNe:
			r = uint32(b2u(a != b))
		case OpLtS:
			r = uint32(b2u(sa < sb))
		case OpLtU:
			r = uint32(b2u(a < b))
		case OpGtS:
			r = uint32(b2u(sa > sb))
		case OpGtU:
			r = uint32(b2u(a > b))
		case OpLeS:
			r = uint32(b2u(sa <= sb))
		case OpLeU:
			r = uint32(b2u(a <= b))
		case OpGeS:
			r = uint32(b2u(sa >= sb))
		case OpGeU:
			r = uint32(b2u(a >= b))
		default:
			return 0, false
		}
		return uint64(r), true
	}
	sa, sb := int64(x), int64(y)
	switch op {
	case OpAdd:
		return x + y, true
	case OpSub:
		return x - y, true
	case OpMul:
		return x * y, true
	case OpAnd:
		return x & y, true
	case OpOr:
		return x | y, true
	case OpXor:
		return x ^ y, true
	case OpShl:
		return x << (y & 63), true
	case OpShrS:
		return uint64(sa >> (y & 63)), true
	case OpShrU:
		return x >> (y & 63), true
	case OpRotl:
		return bits.RotateLeft64(x, int(y&63)), true
	case OpRotr:
		return bits.RotateLeft64(x, -int(y&63)), true
	case OpDivS:
		if y == 0 || (sa == math.MinInt64 && sb == -1) {
			return 0, false
		}
		return uint64(sa / sb), true
	case OpDivU:
		if y == 0 {
			return 0, false
		}
		return x / y, true
	case OpRemS:
		if y == 0 {
			return 0, false
		}
		if sb == -1 {
			return 0, true
		}
		return uint64(sa % sb), true
	case OpRemU:
		if y == 0 {
			return 0, false
		}
		return x % y, true
	case OpEq:
		return b2u(x == y), true
	case OpNe:
		return b2u(x != y), true
	case OpLtS:
		return b2u(sa < sb), true
	case OpLtU:
		return b2u(x < y), true
	case OpGtS:
		return b2u(sa > sb), true
	case OpGtU:
		return b2u(x > y), true
	case OpLeS:
		return b2u(sa <= sb), true
	case OpLeU:
		return b2u(x <= y), true
	case OpGeS:
		return b2u(sa >= sb), true
	case OpGeU:
		return b2u(x >= y), true
	}
	return 0, false
}

// gvnKey is what makes two pure values the same value.
type gvnKey struct {
	op         Op
	t          Type
	a0, a1, a2 int32
	aux, aux2  uint64
}

// gvn makes each pure value that a dominating one computes the same way a
// copy of it, walking the dominator tree with the values its path holds.
func gvn(f *Func) {
	d := dominators(f)
	table := map[gvnKey]*Value{}
	var walk func(b *Block)
	walk = func(b *Block) {
		var added []gvnKey
		for _, v := range b.Values {
			if !v.Pure() || v.Op == OpCopy || len(v.Args) > 3 {
				continue
			}
			k := gvnKey{op: v.Op, t: v.Type, a0: -1, a1: -1, a2: -1, aux: v.Aux, aux2: v.Aux2}
			ids := [3]int32{-1, -1, -1}
			for i, a := range v.Args {
				ids[i] = resolve(a).ID
			}
			if v.Commutes() && ids[0] > ids[1] {
				ids[0], ids[1] = ids[1], ids[0]
			}
			k.a0, k.a1, k.a2 = ids[0], ids[1], ids[2]
			if len(v.Args) > 0 {
				// A comparison's operand type is its argument's.
				k.aux2 |= uint64(resolve(v.Args[0]).Type) << 56
			}
			if w, ok := table[k]; ok {
				v.setCopy(w)
				continue
			}
			table[k] = v
			added = append(added, k)
		}
		for _, c := range d.children[b.ID] {
			walk(c)
		}
		for _, k := range added {
			delete(table, k)
		}
	}
	walk(f.Entry)
}

// licm moves the pure values of each loop that makes no call whose
// arguments are defined outside it to the loop's preheader -- its header's
// one predecessor outside it, which jumps to the header alone -- inner
// loops first, so that a value an inner loop's preheader receives may go
// on out of the outer loop.
func licm(f *Func) {
	d := dominators(f)
	findLoops(f, d)
	var headers []*Block
	for _, b := range f.Blocks {
		if b.Loop == b {
			headers = append(headers, b)
		}
	}
	slices.SortStableFunc(headers, func(a, b *Block) int { return int(b.Depth - a.Depth) })
	inLoop := func(b, h *Block) bool {
		for l := b.Loop; l != nil; l = l.outer {
			if l == h {
				return true
			}
		}
		return false
	}
	for _, h := range headers {
		// The preheader: the one predecessor outside the loop. A loop
		// threading made has others too: its values stay.
		var pre *Block
		outside := 0
		for _, p := range h.Preds {
			if !inLoop(p, h) {
				pre = p
				outside++
			}
		}
		if outside != 1 || len(pre.Succs) != 1 || callsIn(f, h, inLoop) {
			continue
		}
		for _, b := range f.Blocks {
			if !inLoop(b, h) {
				continue
			}
			keep := b.Values[:0]
			for _, v := range b.Values {
				if v.Pure() && v.Op != OpConst && v.Op != OpCopy && invariant(v, h, inLoop) {
					v.Block = pre
					pre.Values = append(pre.Values, v)
					continue
				}
				keep = append(keep, v)
			}
			b.Values = keep
		}
	}
}

// callsIn reports whether the loop whose header is h calls: what it
// hoisted would live across the calls, kept in a slot or loaded again
// after each, where computing it again costs less.
func callsIn(f *Func, h *Block, inLoop func(b, h *Block) bool) bool {
	for _, b := range f.Blocks {
		if !inLoop(b, h) {
			continue
		}
		for _, v := range b.Values {
			if v.IsCall() {
				return true
			}
		}
	}
	return false
}

// invariant reports whether v's arguments are all defined outside the
// loop whose header is h.
func invariant(v *Value, h *Block, inLoop func(b, h *Block) bool) bool {
	for _, a := range v.Args {
		if inLoop(a.Block, h) {
			return false
		}
	}
	return true
}

// bce marks the memory accesses a dominating access of the same address
// checked as far, walking the dominator tree with the farthest end each
// address's accesses on its path checked. A memory never shrinks, so an
// access within an end checked before is within the memory.
func bce(f *Func) {
	d := dominators(f)
	// checked is the end checked for each address value, by its ID, and
	// for constant addresses at -1.
	checked := map[int32]uint64{}
	var walk func(b *Block)
	walk = func(b *Block) {
		type undo struct {
			k   int32
			end uint64
			had bool
		}
		var undos []undo
		for _, v := range b.Values {
			if v.Op != OpLoad && v.Op != OpStore {
				continue
			}
			addr := v.Args[0]
			k, end := addr.ID, v.Aux+uint64(v.Size)
			if addr.Op == OpConst {
				k, end = -1, uint64(uint32(addr.Aux))+end
			}
			old, had := checked[k]
			if had && old >= end {
				v.NoCheck = true
				continue
			}
			undos = append(undos, undo{k, old, had})
			checked[k] = end
		}
		for _, c := range d.children[b.ID] {
			walk(c)
		}
		for i := len(undos) - 1; i >= 0; i-- {
			u := undos[i]
			if u.had {
				checked[u.k] = u.end
			} else {
				delete(checked, u.k)
			}
		}
	}
	walk(f.Entry)
}
