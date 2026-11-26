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
	run := func(name string, p Phase, pass func(*Func)) {
		pass(f)
		lap(p)
		if check != nil {
			check(name)
		}
	}
	run("globals", PhaseGlobals, forwardGlobals)
	run("copyelim", PhaseCopyelim, copyelim)
	run("gvn", PhaseGVN, gvn)
	run("copyelim", PhaseCopyelim, copyelim)
	run("fold", PhaseFold, fold)
	run("copyelim", PhaseCopyelim, copyelim)
	run("thread", PhaseThread, thread)
	run("copyelim", PhaseCopyelim, copyelim)
	run("gvn", PhaseGVN, gvn)
	run("copyelim", PhaseCopyelim, copyelim)
	run("chains", PhaseChains, func(f *Func) {
		if forwardChains(f) {
			copyelim(f)
		}
	})
	run("licm", PhaseLICM, licm)
	run("dce", PhaseDCE, dce)
	run("bce", PhaseBCE, bce)
	run("split", PhaseSplit, splitCriticalEdges)
	run("loops", PhaseLoops, func(f *Func) {
		// What weighs the allocator's uses is the depths; the natural
		// loops' headers are only printed, in a listing or a test's.
		if f.listing || check != nil {
			findLoops(f, dominators(f))
		}
		loopDepths(f)
		layout(f)
	})
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
	copies := false
	for changed := true; changed; {
		changed = false
		for _, b := range f.Blocks {
			made := false
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
					p.setCopy(same)
					made = true
				}
			}
			if made {
				changed, copies = true, true
				// A phi made a copy leaves the phis: its uses are
				// rewritten below, and it is in no list.
				keep := b.Phis[:0]
				for _, p := range b.Phis {
					if p.Op == OpPhi {
						keep = append(keep, p)
					}
				}
				clear(b.Phis[len(keep):])
				b.Phis = keep
			}
		}
	}
	// Without a copy among the values or the phis there is nothing to
	// rewrite: a value refers to a copy only while the copy is in its
	// block's values, or is a phi this pass made one.
	for _, b := range f.Blocks {
		if copies {
			break
		}
		for _, v := range b.Values {
			if v.Op == OpCopy {
				copies = true
				break
			}
		}
	}
	if !copies {
		return
	}
	// One walk rewrites the uses and drops the copies from their blocks: a
	// copy dropped keeps its argument, which a later use resolves through.
	for _, b := range f.Blocks {
		for _, v := range b.Phis {
			resolveArgs(v.Args)
		}
		// The values are moved down only from the block's first copy on.
		vs, n := b.Values, 0
		for i, v := range vs {
			resolveArgs(v.Args)
			if v.Op != OpCopy {
				if n != i {
					vs[n] = v
				}
				n++
			}
		}
		clear(vs[n:])
		b.Values = vs[:n]
		if b.Control != nil {
			b.Control = resolve(b.Control)
		}
		resolveArgs(b.Results)
	}
}

// resolveArgs makes each of args that is a copy the value it copies.
func resolveArgs(args []*Value) {
	for i, a := range args {
		if a.Op == OpCopy {
			args[i] = resolve(a)
		}
	}
}

// dce removes the values nothing uses that have no effect, and the phis
// only they use, and a global.set another of the same global follows in
// its block with nothing between that reads the global, has an effect or
// may trap: the global holds what the interpreter's does wherever anything
// could see it. Go's code's returns move its stack pointer twice.
func dce(f *Func) {
	computeUses(f)
	work := f.ws.dceWork[:0]
	for _, b := range f.Blocks {
		for _, v := range b.Phis {
			if v.Uses == 0 {
				work = append(work, v)
			}
		}
		// set is the block's last global.set that nothing after it has
		// read the global or could see it since: another of the same
		// global then makes it dead.
		var set *Value
		for _, v := range b.Values {
			if v.Uses == 0 && !v.HasEffect() {
				work = append(work, v)
			}
			if v.Op == OpGlobalSet {
				if set != nil && set.Aux == v.Aux {
					work = append(work, set)
				}
				set = v
			} else if set != nil && (v.Op == OpGlobalGet || v.HasEffect()) {
				set = nil
			}
		}
	}
	// dead is each value's, by ID; hit counts each block's dead values.
	dead := grown(f.ws.dead, f.NumValues())
	clear(dead)
	f.ws.dead = dead
	hit := f.ws.blockMarks(f)
	ndead := 0
	// high is the most the work list held, what is cleared after.
	high := len(work)
	for len(work) > 0 {
		high = max(high, len(work))
		v := work[len(work)-1]
		work = work[:len(work)-1]
		if dead[v.ID] {
			continue
		}
		dead[v.ID] = true
		hit[v.Block.ID]++
		ndead++
		for _, a := range v.Args {
			if a == v {
				continue
			}
			a.Uses--
			if a.Uses == 0 && !a.HasEffect() && !dead[a.ID] {
				work = append(work, a)
			}
		}
	}
	f.ws.dceWork = work
	// A phi that only phis that are dead use, its own self included, was
	// counted as used: those in cycles of phis are found next.
	cycles := false
	for _, b := range f.Blocks {
		for _, v := range b.Phis {
			if dead[v.ID] || v.Uses == 0 {
				continue
			}
			self := int32(0)
			for _, a := range v.Args {
				if a == v {
					self++
				}
			}
			if self == v.Uses {
				dead[v.ID] = true
				hit[b.ID]++
				ndead++
				cycles = true
			}
		}
	}
	clear(work[:high])
	if ndead == 0 {
		return
	}
	for _, b := range f.Blocks {
		if hit[b.ID] != 0 {
			b.Phis = dropDead(b.Phis, dead)
			b.Values = dropDead(b.Values, dead)
		}
	}
	// The values the work list removed took their uses with them; the
	// phis only themselves used did not.
	if cycles {
		computeUses(f)
	}
}

// dropDead is vs without the values dead says are, the rest cleared.
func dropDead(vs []*Value, dead []bool) []*Value {
	keep := vs[:0]
	for _, v := range vs {
		if !dead[v.ID] {
			keep = append(keep, v)
		}
	}
	clear(vs[len(keep):])
	return keep
}

// fold folds constants, simplifies identities, and branches on constants,
// removing the blocks they no longer reach.
func fold(f *Func) {
	for _, b := range f.Blocks {
		for i := 0; i < len(b.Values); i++ {
			v := b.Values[i]
			foldValue(f.ws, v)
			switch v.Op {
			case OpWrap:
				i += narrow(f, v, i)
			case OpDivS, OpDivU, OpRemS, OpRemU:
				i += divConst(f, v, i)
			case OpEqz:
				i += eqzEqz(f, v, i)
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

// setCopy makes v a copy of a, in its arguments' array where it has
// room: a value's arguments are its own.
func (v *Value) setCopy(a *Value) {
	v.Op, v.Args, v.Aux, v.Aux2 = OpCopy, append(v.Args[:0], a), 0, 0
}

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

// foldValue folds v if its arguments let it, any list it makes in ws.
func foldValue(ws *workspace, v *Value) {
	for i, a := range v.Args {
		v.Args[i] = resolve(a)
	}
	switch len(v.Args) {
	case 1:
		foldUnary(ws, v)
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

// eqzEqz makes eqz of an eqz of x, or of its i64 extension -- Go's code's
// test of a bool it made from a comparison with zero -- x != 0, a
// comparison that a branch on it fuses with, where it was two tests and two
// SETcc. v is block's value i; eqzEqz returns the number of values it puts
// before it, the zero it makes.
func eqzEqz(f *Func, v *Value, i int) int {
	a := resolve(v.Args[0])
	if a.Op == OpExtendU {
		// An eqz's 0 or 1 widened to an i64 is zero where it is.
		a = resolve(a.Args[0])
	}
	if a.Op != OpEqz {
		return 0
	}
	x := resolve(a.Args[0])
	b := v.Block
	c := f.value(b, OpConst, x.Type, nil)
	n := len(b.Values)
	b.Values = appendA(&f.ws.vals, b.Values, c)
	copy(b.Values[i+1:], b.Values[i:n])
	b.Values[i] = c
	v.Op = OpNe
	f.setArgs(v, x, c)
	return 1
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
	// The constants made are in no block until the operation is narrowed:
	// one made for the first operand keeps its ID where the second leaves
	// the operation as it is.
	var args, made [2]*Value
	consts := 0
	for k, x := range a.Args {
		x = resolve(x)
		switch x.Op {
		case OpExtendU, OpExtendS:
			args[k] = x.Args[0]
		case OpConst:
			c := f.value(v.Block, OpConst, TI32, nil)
			c.Aux = uint64(uint32(x.Aux))
			args[k], made[consts] = c, c
			consts++
		default:
			return 0
		}
	}
	if b := v.Block; consts > 0 {
		// The constants go before v, which is value i.
		n := len(b.Values)
		b.Values = appendA(&f.ws.vals, b.Values, made[:consts]...)
		copy(b.Values[i+consts:], b.Values[i:n])
		copy(b.Values[i:], made[:consts])
	}
	v.Op = a.Op
	f.setArgs(v, args[0], args[1])
	foldValue(f.ws, v)
	return consts
}

func foldUnary(ws *workspace, v *Value) {
	a := v.Args[0]
	if v.Op == OpEqz {
		// eqz of an integer comparison is the comparison inverted.
		if inv, ok := invertCmp[a.Op]; ok {
			v.Op, v.Args = inv, append(ws.vals.take(len(a.Args)), a.Args...)
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
	if v.Op == OpFMul && isConst(x) != isConst(y) {
		// x*2 is x+x: both exact, the same infinities, zeros and, where x is
		// a NaN, the same NaN, x's made quiet. The add takes fewer cycles,
		// on the chain of a loop that carries x from turn to turn.
		if isConst(x) {
			x, y = y, x
		}
		if two := uint64(0x4000000000000000); t == TF32 && uint32(y.Aux) == 0x40000000 || t == TF64 && y.Aux == two {
			v.Op = OpFAdd
			v.Args[0], v.Args[1] = x, x
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
	table := &f.ws.gvn
	table.reset(f.NumValues())
	var walk func(b *Block)
	walk = func(b *Block) {
		mark := table.mark()
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
			if w := table.find(&k); w != nil {
				if cap(v.Args) == 0 {
					// A copy's argument is in the workspace too.
					v.Args = f.ws.vals.take(1)
				}
				v.setCopy(w)
				continue
			}
			table.add(&k, v)
		}
		for _, c := range d.kids(b) {
			walk(c)
		}
		table.pop(mark)
	}
	walk(f.Entry)
	table.done()
}

// gvnTable is gvn's table of the values on the dominator tree's path to
// the block walked, by their keys: a hash table of chains whose entries
// are pushed as values are added and popped as the walk leaves their
// blocks.
type gvnTable struct {
	heads   []int32
	entries []gvnEntry
	// dirty says heads may hold entries: done, popping them all, leaves
	// every head -1 for the next function.
	dirty bool
}

type gvnEntry struct {
	key  gvnKey
	v    *Value
	next int32
	h    uint32
}

// reset empties t for a function of n values.
func (t *gvnTable) reset(n int) {
	size := 64
	for size < 2*n {
		size *= 2
	}
	if cap(t.heads) < size || t.dirty {
		t.heads = grown(t.heads, size)
		h := t.heads[:cap(t.heads)]
		for i := range h {
			h[i] = -1
		}
	}
	t.heads = t.heads[:size]
	t.dirty = true
	t.entries = t.entries[:0]
}

func (k *gvnKey) hash() uint64 {
	h := uint64(k.op) | uint64(k.t)<<8 | uint64(uint32(k.a0))<<16
	h ^= uint64(uint32(k.a1))*0x9e3779b97f4a7c15 ^ uint64(uint32(k.a2))*0xc2b2ae3d27d4eb4f
	h ^= k.aux*0x165667b19e3779f9 ^ k.aux2*0x27d4eb2f165667c5
	h ^= h >> 31
	h *= 0xbf58476d1ce4e5b9
	return h ^ h>>29
}

func (t *gvnTable) find(k *gvnKey) *Value {
	for e := t.heads[k.hash()&uint64(len(t.heads)-1)]; e >= 0; e = t.entries[e].next {
		if t.entries[e].key == *k {
			return t.entries[e].v
		}
	}
	return nil
}

func (t *gvnTable) add(k *gvnKey, v *Value) {
	h := uint32(k.hash() & uint64(len(t.heads)-1))
	t.entries = append(t.entries, gvnEntry{key: *k, v: v, next: t.heads[h], h: h})
	t.heads[h] = int32(len(t.entries) - 1)
}

// mark is where the entries end, to pop back to.
func (t *gvnTable) mark() int { return len(t.entries) }

// pop removes the entries added since mark.
func (t *gvnTable) pop(mark int) {
	for i := len(t.entries) - 1; i >= mark; i-- {
		e := &t.entries[i]
		t.heads[e.h] = e.next
		e.v = nil
	}
	t.entries = t.entries[:mark]
}

// done drops the table's values.
func (t *gvnTable) done() {
	t.pop(0)
	t.dirty = false
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

// bce marks the memory accesses a dominating access of the same memory and
// address checked as far, walking the dominator tree with the farthest end
// each address's accesses on its path checked. A memory never shrinks, so
// an access within an end checked before is within the memory. A shared
// memory's accesses are left as they are: each checks the size another
// thread may have grown. An access of memory 0 whose address's values
// keep it within the memory's least size is not checked (bounds.go).
func bce(f *Func) {
	d := dominators(f)
	ub := &f.ws.ub
	if f.memMin > 0 {
		ub.reset(f.NumValues())
	}
	// checked is the end checked for each memory's address value, by the
	// memory's index above the value's ID (addrBase), and for constant
	// addresses above -1: 0, which no access's end is, where none is.
	// undo is what each entry was before the blocks on the walk's path
	// changed it.
	checked := &f.ws.checked
	checked.reset(f.NumValues())
	undo := f.ws.bceUndo[:0]
	pa := &f.ws.bcePaths
	pa.ready = false
	// quiet reports whether value w neither has an effect nor may trap:
	// a load is quiet where an access dominating it checked as far.
	quiet := func(w *Value) bool {
		if !w.HasEffect() {
			return true
		}
		if w.Op != OpLoad || w.Aux2&memShared != 0 || w.Args[0].Op == OpConst {
			return false
		}
		end := w.Aux + uint64(w.Size)
		return end >= w.Aux && checked.at(uint64(memIndex(w))<<32|uint64(uint32(addrBase(w).ID))) >= end
	}
	var walk func(b *Block)
	walk = func(b *Block) {
		mark := len(undo)
		for i, v := range b.Values {
			if v.Op != OpLoad && v.Op != OpStore || v.Aux2&memShared != 0 {
				continue
			}
			addr := v.Args[0]
			id, end := addrBase(v).ID, v.Aux+uint64(v.Size)
			if end < v.Aux {
				// An i64 memory's offset whose end carries: the access traps.
				continue
			}
			plain := plainAccess(v)
			if f.memMin > 0 && plain && ub.upper(addr)+end <= f.memMin {
				// Within the memory's least size, whatever the address.
				v.NoCheck = true
				continue
			}
			if addr.Op == OpConst {
				c := constAddr(addr)
				if c+end < c {
					// The access traps.
					continue
				}
				id, end = -1, c+end
			}
			k := uint64(memIndex(v))<<32 | uint64(uint32(id))
			old := checked.at(k)
			if old >= end {
				v.NoCheck = true
				continue
			}
			if plain && id >= 0 {
				if pa.phiChecked(f, d, addrBase(v), end) {
					v.NoCheck = true
				} else {
					end = widen(f, b.Values[i+1:], v, end, quiet)
				}
			}
			undo = append(undo, [2]uint64{k, old})
			checked.set(k, end)
		}
		for _, c := range d.kids(b) {
			walk(c)
		}
		for i := len(undo) - 1; i >= mark; i-- {
			checked.set(undo[i][0], undo[i][1])
		}
		undo = undo[:mark]
	}
	f.ws.wide = f.ws.wide[:0]
	walk(f.Entry)
	checked.clean()
	f.ws.bceUndo = undo[:0]
}

// bceEnds is bce's ends checked, by a key of a memory's index above an
// address's ID (or -1, the constant addresses'): memory 0's in an array by
// the ID plus one, the other memories' in a table.
type bceEnds struct {
	mem0  []uint64
	other u64map[uint64]
	// dirty says mem0 may hold ends: bce's walk, which puts back what it
	// changes, leaves it all 0 for the next function (clean).
	dirty bool
}

// reset empties e for a function of n values.
func (e *bceEnds) reset(n int) {
	if e.dirty {
		clear(e.mem0[:cap(e.mem0)])
	}
	e.mem0 = grown(e.mem0, n+1)
	e.dirty = true
	e.other.reset(8)
}

// clean notes that e's walk has put back every end it changed.
func (e *bceEnds) clean() { e.dirty = false }

// at is key k's end, 0 where none is.
func (e *bceEnds) at(k uint64) uint64 {
	if k>>32 == 0 {
		return e.mem0[int(int32(uint32(k)))+1]
	}
	return e.other.at(k)
}

// set makes x key k's end.
func (e *bceEnds) set(k, x uint64) {
	if k>>32 == 0 {
		e.mem0[int(int32(uint32(k)))+1] = x
		return
	}
	e.other.set(k, x)
}

// addrBase is what memory access v's checks are known by: its address,
// or, where that is an i64 wrapped for a memory of i32 addresses, the
// i64, whose low half each such wrap of it is.
func addrBase(v *Value) *Value {
	a := v.Args[0]
	if a.Op == OpWrap && v.Aux2&memWide == 0 {
		return a.Args[0]
	}
	return a
}

// widenScan is how many values after an access widen looks through.
const widenScan = 32

// widen makes the check of plain load v, whose end is end, cover the
// accesses of its address among the values after it in its block, next,
// up to the first value that has an effect or may trap but those
// accesses, a store among them the last covered: nothing between v and
// them could tell that their ends were checked at v. It returns the end
// v is to check. Where that check fails, the code finds the first of
// them whose own end is past the memory's and traps there (wideCheck),
// so that the trap is the one the accesses in order make. A store's
// check is not widened: it stores once its check passes.
func widen(f *Func, next []*Value, v *Value, end uint64, quiet func(*Value) bool) uint64 {
	if v.Op == OpStore {
		return end
	}
	base := addrBase(v)
	ws := f.ws
	start := len(ws.wide)
	far := end
	for n, w := range next {
		if n == widenScan {
			break
		}
		if (w.Op == OpLoad || w.Op == OpStore) && w.Args[0].Op != OpConst && addrBase(w) == base && plainAccess(w) {
			if e := w.Aux + uint64(w.Size); e > far {
				if len(ws.wide) == start {
					ws.wide = append(ws.wide, wideCheck{v: v, end: end})
				}
				ws.wide = append(ws.wide, wideCheck{v: w, end: e})
				far = e
			}
			if w.Op == OpStore {
				break
			}
			continue
		}
		if !quiet(w) {
			break
		}
	}
	if far == end {
		return end
	}
	ws.wide = append(ws.wide, wideCheck{})
	v.Wide = int32(start + 1)
	return far
}

// wideCheck is an access a widened check covers and its end. A widened
// check's are in order from its access's Value.Wide-1: the access, then
// each after it whose end is past those before it, the last the end
// checked, then an entry of none.
type wideCheck struct {
	v   *Value
	end uint64
}

// bcePaths finds the accesses of memory 0 in bounds on every path to
// them though no access dominating them checked as far: those of an
// address that is a phi -- each of a Go program's loop's values, whose
// dispatch switch enters each of its blocks from the function's entry
// too, is a phi of a constant and what the loop brings -- whose every
// argument is a constant within the memory's least size, or a value
// accessed as far on every path to the end of the argument's
// predecessor: through an access on that predecessor's dominators, or,
// where the argument is a phi again, through its arguments in turn. An
// access is in bounds where an access before it, in time, of the same
// address was: the memory never shrinks.
type bcePaths struct {
	// acc is the plain accesses of addresses not constant of each block
	// a search has met, from span[b.ID][0] to span[b.ID][1]; a block not
	// met yet has span[b.ID][1] -1.
	acc  []bceAccess
	span []bceSpan
	// steps counts a search's blocks, against bceSteps.
	steps int
	// memo is, by a phi's ID, the farthest end found checked on every
	// path and the nearest found not to be, 0 where none is.
	memo []bceMemo
	// stamp marks the entries of span and memo that are the function's;
	// ready says it is the function's.
	stamp uint32
	ready bool
}

type bceSpan struct {
	stamp  uint32
	lo, hi int32
}

type bceMemo struct {
	stamp          uint32
	proven, failed uint32
}

// bceAccess is a plain access's address base and end.
type bceAccess struct {
	base *Value
	end  uint64
}

const (
	// bceDepth is how many phis deep, and bceSteps how many blocks in all,
	// an access's search goes.
	bceDepth = 6
	bceSteps = 64
)

// of returns block b's plain accesses of addresses not constant.
func (p *bcePaths) of(b *Block) []bceAccess {
	s := &p.span[b.ID]
	if s.stamp != p.stamp {
		s.stamp, s.lo = p.stamp, int32(len(p.acc))
		for _, v := range b.Values {
			if (v.Op == OpLoad || v.Op == OpStore) && v.Aux2&memShared == 0 && v.Args[0].Op != OpConst && plainAccess(v) {
				p.acc = append(p.acc, bceAccess{addrBase(v), v.Aux + uint64(v.Size)})
			}
		}
		s.hi = int32(len(p.acc))
	}
	return p.acc[s.lo:s.hi]
}

// phiChecked reports whether every path to a use of phi base -- an
// address base -- has made an access of it as far as end, or brings a
// constant whose access is within the memory's least size.
func (p *bcePaths) phiChecked(f *Func, d *domTree, base *Value, end uint64) bool {
	if f.memMin == 0 || base.Op != OpPhi {
		return false
	}
	if !p.ready {
		p.acc = p.acc[:0]
		p.span = grown(p.span, int(f.nextBlock))
		p.memo = grown(p.memo, f.NumValues())
		if p.stamp++; p.stamp == 0 {
			clear(p.span[:cap(p.span)])
			clear(p.memo[:cap(p.memo)])
			p.stamp = 1
		}
		p.ready = true
	}
	m := &p.memo[base.ID]
	if m.stamp != p.stamp {
		*m = bceMemo{stamp: p.stamp}
	}
	if end <= uint64(m.proven) {
		return true
	}
	if m.failed != 0 && end >= uint64(m.failed) {
		return false
	}
	p.steps = 0
	ok := p.phi(f, d, base, end, 0)
	if ok {
		m.proven = uint32(end)
	} else {
		m.failed = uint32(end)
	}
	return ok
}

func (p *bcePaths) phi(f *Func, d *domTree, phi *Value, end uint64, depth int) bool {
	if phi.Op != OpPhi || depth == bceDepth {
		return false
	}
	h := phi.Block
	for i, a := range phi.Args {
		if a.Op == OpWrap && phi.Type == TI32 {
			a = a.Args[0]
		}
		if a.Op == OpConst {
			if uint64(uint32(a.Aux))+end > f.memMin {
				return false
			}
			continue
		}
		if !p.at(f, d, a, end, h.Preds[i], depth) {
			return false
		}
	}
	return true
}

// at reports whether base has been accessed as far as end on every path
// to the end of block x, which base's block dominates.
func (p *bcePaths) at(f *Func, d *domTree, base *Value, end uint64, x *Block, depth int) bool {
	for b := x; ; b = d.idom[b.ID] {
		if p.steps++; p.steps > bceSteps {
			return false
		}
		for _, c := range p.of(b) {
			if c.base == base && c.end >= end {
				return true
			}
		}
		if b == base.Block || b == f.Entry {
			break
		}
	}
	return p.phi(f, d, base, end, depth+1)
}
