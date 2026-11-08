package ssa

import (
	"math"
	"slices"

	"github.com/go-quickjs/go-quickjs/internal/jit/ir"
)

// Integers as int32s where their uses truncate them, or compare them:
// asm.js's (a + b) | 0 and (i | 0) < (n | 0), as V8's representation
// selection computes them in Word32.
//
// The slot IR computes every number as a double, and a bitwise operator
// takes its operands' ToInt32 (OpToInt32) and gives back a double
// (OpI32ToF64): (a + b) | 0 of two int32s converted both to doubles, added
// them, and converted the sum back, its range checked -- zlib's inflate
// spent a third of its native time converting. Four rewrites keep such
// values int32s:
//
//   - a constant's ToInt32 is the constant (simplify's, OpConstI32), which
//     mir makes an instruction's immediate: a shift's count, a mask;
//   - the ToInt32 of a sum, a difference, a negation or a product by a
//     constant of such integers, exact as a double, is the int32
//     operation, which wraps as ToInt32 of the exact result does
//     (truncInts);
//   - a comparison of two int32s' conversions, or two uint32s', is the
//     int32 comparison (OpCmpI32), and an element read or write of
//     numbers whose key is an int32's conversion takes the int32;
//   - a phi of doubles that only ever merges int32s is an int32 phi
//     (intPhis), as many as a loop's budget of general registers allows
//     (intPhiBudgets): mir has half as many of those as SSE registers.

// maxExact is the largest magnitude below which every integer is a
// double: a sum of integers whose magnitudes add up to no more is exact.
const maxExact = 1 << 53

// int32Const reports whether the double c is an int32's: an integer of
// the range, and not -0, which an int32 phi would make +0.
func int32Const(c float64) bool {
	return c >= math.MinInt32 && c <= math.MaxInt32 && math.Trunc(c) == c && !(c == 0 && math.Signbit(c))
}

// exactInt is the bound of the magnitude of e, a double that is an exact
// integer -- the conversion of an int32 or a uint32, an integer constant,
// their sums, differences, negations and products by constants, each
// exact -- or false. depth bounds the expression's height.
func exactInt(e *Value, depth int) (float64, bool) {
	switch e.Op {
	case OpI32ToF64:
		return 1 << 31, true
	case OpU32ToF64:
		return 1<<32 - 1, true
	case OpConstF64:
		c := math.Float64frombits(e.Const.Bits)
		if math.Trunc(c) != c || math.Abs(c) > maxExact {
			return 0, false
		}
		return math.Abs(c), true
	}
	if depth == 0 {
		return 0, false
	}
	switch e.Op {
	case OpAddF64, OpSubF64:
		x, ok := exactInt(e.Args[0], depth-1)
		if !ok {
			return 0, false
		}
		y, ok := exactInt(e.Args[1], depth-1)
		if !ok || x+y > maxExact {
			return 0, false
		}
		return x + y, true
	case OpNegF64:
		return exactInt(e.Args[0], depth-1)
	case OpMulF64:
		// By a constant: the other operand's bound times it.
		k, x := e.Args[1], e.Args[0]
		if x.Op == OpConstF64 {
			k, x = x, k
		}
		if k.Op != OpConstF64 {
			return 0, false
		}
		c, ok := exactInt(k, 0)
		if !ok {
			return 0, false
		}
		b, ok := exactInt(x, depth-1)
		if !ok || b*c > maxExact {
			return 0, false
		}
		return b * c, true
	}
	return 0, false
}

// intCompare reports whether a comparison of x and y, doubles, is one of
// int32s -- each an int32's conversion or an integer constant of the
// range, not both constants -- or of uint32s (unsigned), which compare
// as the doubles do: neither is NaN, and -0, a constant's, is 0's equal.
func intCompare(x, y *Value) (unsigned, ok bool) {
	kind := func(a *Value) (signed, unsigned bool) {
		switch a.Op {
		case OpI32ToF64:
			return true, false
		case OpU32ToF64:
			return false, true
		case OpConstF64:
			c := math.Float64frombits(a.Const.Bits)
			if math.Trunc(c) != c {
				return false, false
			}
			return c >= math.MinInt32 && c <= math.MaxInt32, c >= 0 && c <= math.MaxUint32
		}
		return false, false
	}
	if x.Op == OpConstF64 && y.Op == OpConstF64 {
		return false, false
	}
	xs, xu := kind(x)
	ys, yu := kind(y)
	switch {
	case xs && ys:
		return false, true
	case xu && yu:
		return true, true
	}
	return false, false
}

// truncatedArith reports an operation truncInts makes an int32 one of.
func truncatedArith(op Op) bool {
	return op == OpAddF64 || op == OpSubF64 || op == OpNegF64 || op == OpMulF64
}

// truncInts makes each ToInt32 of an exact integer's arithmetic
// (exactInt) the int32 operations: ToInt32 is a ring homomorphism to the
// integers modulo 2**32, so the int32 sum of the operands' ToInt32s is
// the ToInt32 of their exact sum. The ToInt32 becomes the outermost
// operation in place; the inner ones are made before it, and the doubles'
// operations stay for any other use.
func truncInts(f *Func) bool {
	changed := false
	for _, b := range f.Blocks {
		for i := 0; i < len(b.Values); i++ {
			v := b.Values[i]
			if k := v.Args; (v.Op == OpElemRead || v.Op == OpElemWrite) && k[1].Op == OpI32ToF64 {
				// An element's key that is an int32's conversion: the
				// int32, an index where it is not negative (mir's index).
				k[1] = k[1].Args[0]
				changed = true
				continue
			}
			unsigned, cmp := false, v.Op == OpCmpF64
			if cmp {
				if unsigned, cmp = intCompare(v.Args[0], v.Args[1]); !cmp {
					continue
				}
			} else if v.Op != OpToInt32 || !truncatedArith(v.Args[0].Op) {
				continue
			} else if _, ok := exactInt(v.Args[0], 8); !ok {
				continue
			}
			e := v.Args[0]
			// place puts x before v, which moves along.
			place := func(x *Value) *Value {
				b.Values = f.appendValue(b.Values, nil)
				copy(b.Values[i+1:], b.Values[i:])
				b.Values[i] = x
				i++
				return x
			}
			konst := func(bits uint32) *Value {
				return place(f.alloc(Value{Op: OpConstI32, Type: Int32, Const: ir.Value{Bits: uint64(bits)}, Block: b}))
			}
			var build func(e *Value) *Value
			// arith makes x e's int32 operation, of its operands' int32s.
			arith := func(x, e *Value) {
				var op Op
				var l, r *Value
				switch e.Op {
				case OpAddF64:
					op, l, r = OpAddI32, build(e.Args[0]), build(e.Args[1])
				case OpSubF64:
					op, l, r = OpSubI32, build(e.Args[0]), build(e.Args[1])
				case OpNegF64:
					op, l, r = OpSubI32, konst(0), build(e.Args[0])
				case OpMulF64:
					op, l, r = OpMulI32, build(e.Args[0]), build(e.Args[1])
				}
				args := f.refsOf(2)
				args[0], args[1] = l, r
				x.Op, x.Type, x.Args, x.State, x.Aux, x.Const = op, Int32, args, nil, 0, ir.Value{}
			}
			build = func(e *Value) *Value {
				switch e.Op {
				case OpI32ToF64, OpU32ToF64:
					return e.Args[0]
				case OpConstF64:
					return konst(ir.ToUint32(math.Float64frombits(e.Const.Bits)))
				}
				x := f.alloc(Value{Block: b})
				arith(x, e)
				return place(x)
			}
			if cmp {
				// The comparison's operands, as int32s.
				x, y := build(v.Args[0]), build(v.Args[1])
				if unsigned {
					v.Aux |= CmpUnsigned
				}
				v.Op, v.Args = OpCmpI32, f.refsOf(2)
				v.Args[0], v.Args[1] = x, y
				changed = true
				continue
			}
			arith(v, e)
			changed = true
		}
	}
	return changed
}

// intPhis makes a phi of doubles that only ever merges int32s an int32
// phi, its old self the new one's conversion, which the uses that take
// its ToInt32 cancel (simplify) and truncInts reads through. A
// candidate's arguments are conversions of int32s, integer constants of
// the range (int32Const), other candidates, or numbers an entry unboxed
// (unboxPhis's), which a guard there speculates are int32s (OpCheckI32),
// exiting to the entry's state otherwise, which then stops speculating
// (Block's Generic). A candidate needs a reason: the conversion of an
// int32 among its arguments, or a candidate's that has one -- a loop
// variable assigned a bitwise result, not a phi that merges an entry's
// number with a constant only, which would speculate for nothing.
func intPhis(f *Func) bool {
	n := f.nextID
	// The phis of doubles, from scratch the Func keeps: none, or none with
	// an int32's conversion among its arguments -- most functions, and
	// every round but the first in most of the rest -- is done with no
	// table made.
	// A phi an earlier round dropped (intNo) is no candidate, and so gives
	// no reason: a round whose conversions are all among those -- most
	// rounds after the first that made int32 phis -- is done too.
	sc := f.scr()
	phis, any := sc.intPhis[:0], false
	for _, b := range f.Blocks {
		for _, v := range b.Values {
			if v.Op != OpPhi {
				break
			}
			if v.Type == Float64 && !trivialPhi(v) {
				phis = append(phis, v)
				if !any && (v.ID >= len(sc.intNo) || !sc.intNo[v.ID]) {
					for _, a := range v.Args {
						any = any || a.Op == OpI32ToF64
					}
				}
			}
		}
	}
	sc.intPhis = phis[:0]
	if !any {
		return false
	}
	sc.intCand, sc.intReason = growFlags(sc.intCand, n), growFlags(sc.intReason, n)
	cand, reason := sc.intCand, sc.intReason
	defer func() {
		for _, v := range phis {
			cand[v.ID], reason[v.ID] = false, false
		}
	}()
	no := sc.intNo
	for len(no) < n {
		no = append(no, false)
	}
	sc.intNo = no
	for _, v := range phis {
		cand[v.ID] = !no[v.ID]
	}
	isCand := func(a *Value) bool { return a.ID < n && cand[a.ID] }
	entry := func(a *Value) bool {
		return a.Op == OpUnboxF64 && ir.ExitKind(a.Aux) == ir.GuardExit && a.State != nil && a.Block.PC < 0 && !a.Block.Generic
	}
	accepted := func(a *Value) bool {
		return a.Op == OpI32ToF64 || a.Op == OpConstF64 && int32Const(math.Float64frombits(a.Const.Bits)) || isCand(a) || entry(a)
	}
	// The candidates' users among the candidates, listed once, as
	// unboxPhis lists its own (candUsers): removals spread to the phis that
	// merge the removed, and reasons to the phis that merge one with a
	// reason, along worklists -- a pass over the phis until none changed
	// took a pass for each phi of a chain, Mandreel's functions hundreds.
	idx, list := candUsers(f, phis, cand, n)
	users := func(v *Value) []*Value { return list[idx[v.ID]:idx[v.ID+1]] }
	var work []*Value
	// drop removes the candidates on the worklist, and those that merge
	// them: a phi of doubles is no argument a candidate accepts.
	drop := func() {
		for len(work) != 0 {
			v := work[len(work)-1]
			work = work[:len(work)-1]
			for _, u := range users(v) {
				if cand[u.ID] {
					cand[u.ID] = false
					work = append(work, u)
				}
			}
		}
	}
	filter := func() {
		for _, v := range phis {
			if !cand[v.ID] {
				continue
			}
			for _, a := range v.Args {
				if !accepted(a) {
					cand[v.ID] = false
					work = append(work, v)
					break
				}
			}
		}
		drop()
		for {
			for _, v := range phis {
				reason[v.ID] = false
			}
			for _, v := range phis {
				if !cand[v.ID] {
					continue
				}
				for _, a := range v.Args {
					if a.Op == OpI32ToF64 {
						reason[v.ID] = true
						work = append(work, v)
						break
					}
				}
			}
			for len(work) != 0 {
				v := work[len(work)-1]
				work = work[:len(work)-1]
				for _, u := range users(v) {
					if cand[u.ID] && !reason[u.ID] {
						reason[u.ID] = true
						work = append(work, u)
					}
				}
			}
			for _, v := range phis {
				if cand[v.ID] && !reason[v.ID] {
					cand[v.ID] = false
					work = append(work, v)
				}
			}
			if len(work) == 0 {
				return
			}
			drop()
		}
	}
	filter()
	some := func() bool {
		for _, v := range phis {
			if cand[v.ID] {
				return true
			}
		}
		return false
	}
	if !some() {
		return false
	}
	if intPhiUses(f, phis, cand) {
		filter()
		if !some() {
			return false
		}
	}
	if intPhiBudgets(f, phis, cand) {
		filter()
	}
	// In block order, so that value numbers do not vary from one compile to
	// the next: each candidate's int32 phi (ip) and what an argument that
	// is no conversion is made into (made), by value ID.
	ip, made := f.refsOf(n), f.refsOf(n)
	ordered := sc.ordered[:0]
	for _, v := range phis {
		if cand[v.ID] {
			ip[v.ID] = f.alloc(Value{Op: OpPhi, Type: Int32, Block: v.Block})
			ordered = append(ordered, v)
		}
	}
	sc.ordered = ordered[:0]
	if len(ordered) == 0 {
		return false
	}
	for _, v := range ordered {
		p := ip[v.ID]
		p.Args = f.refsOf(len(v.Args))[:0]
		for _, a := range v.Args {
			var x *Value
			switch {
			case a.Op == OpI32ToF64:
				x = a.Args[0]
			case isCand(a):
				x = ip[a.ID]
			default:
				if a.ID < n {
					x = made[a.ID]
				}
				if x == nil && a.Op == OpConstF64 {
					x = f.alloc(Value{Op: OpConstI32, Type: Int32, Block: a.Block,
						Const: ir.Value{Bits: uint64(uint32(int32(math.Float64frombits(a.Const.Bits))))}})
					insertAfter(f, a, x)
				} else if x == nil {
					// An entry's number, speculated an int32 there.
					x = f.alloc(Value{Op: OpCheckI32, Type: Int32, Args: f.refsOf(1), Aux: int(ir.GuardExit), State: a.State, Block: a.Block})
					x.Args[0] = a
					insertAfter(f, a, x)
				}
				if a.ID < n {
					made[a.ID] = x
				}
			}
			p.Args = append(p.Args, x)
		}
	}
	// Each block's phis, the new ones in place of those they replace, then
	// the old ones, now their conversions, then the rest: built in buffers
	// the pass reuses, as unboxPhis does.
	front, conv, rest := sc.front[:0], sc.boxes[:0], sc.rest[:0]
	var last *Block
	for _, o := range ordered {
		b := o.Block
		if b == last {
			continue
		}
		last = b
		front, conv, rest = front[:0], conv[:0], rest[:0]
		for _, v := range b.Values {
			switch {
			case v.Op != OpPhi:
				rest = append(rest, v)
			case v.ID < n && cand[v.ID]:
				p := ip[v.ID]
				front = append(front, p)
				*v = Value{ID: v.ID, Op: OpI32ToF64, Type: Float64, Args: v.Args[:1], Block: b, Uses: v.Uses}
				v.Args[0] = p
				conv = append(conv, v)
			default:
				front = append(front, v)
			}
		}
		total := len(front) + len(conv) + len(rest)
		if cap(b.Values) < total {
			b.Values = f.refsOf(total)
		}
		b.Values = b.Values[:total]
		copy(b.Values, front)
		copy(b.Values[len(front):], conv)
		copy(b.Values[len(front)+len(conv):], rest)
	}
	sc.front, sc.boxes, sc.rest = front[:0], conv[:0], rest[:0]
	return true
}

// intPhiBudget is how many general registers a loop's int32 phis, and its
// header's other phis held in them, may take (intPhiBudgets): amd64's code
// has seven for values, and a loop's own values want some.
const intPhiBudget = 5

// intPhiBudgets drops the candidates of each loop header past its budget
// (intPhiBudget), and reports whether it dropped any. An int32 phi holds a
// general register through its loop, two if a use after the loop reads it
// -- the allocation's intervals have no holes, so the phi then lives
// through the loop's body beside the value its back edge carries -- where
// a phi of doubles held an SSE register, of which there are twice as many:
// four sums read after their loop had two of them, and a phi a third, in
// slots, stored and loaded at every turn. The header's other phis in
// general registers count against the budget; the candidates cheapest in
// registers are kept first, in block order.
func intPhiBudgets(f *Func, phis []*Value, cand []bool) bool {
	n := len(cand)
	isCand := func(a *Value) bool { return a.ID < n && cand[a.ID] }
	// An int32 phi an earlier round made, whose registers count as a
	// candidate's.
	made := func(a *Value) bool { return a.Op == OpPhi && a.Type == Int32 }
	tracked := func(a *Value) bool { return a.ID < n && (cand[a.ID] || made(a)) }
	// Only a header whose candidates might not fit, at two registers each,
	// needs to know which are used after its loop: most fit, and the scan
	// of every value below is not made.
	need := false
	for _, h := range f.Blocks {
		if !h.LoopHeader {
			continue
		}
		k, held := 0, 0
		for _, v := range h.Values {
			if v.Op != OpPhi {
				break
			}
			switch {
			case isCand(v), made(v):
				k++
			case v.Type != Float64 && !slices.Contains(v.Args, v):
				held++
			}
		}
		need = need || k != 0 && held+2*k > intPhiBudget
	}
	if !need {
		return false
	}
	// Each candidate's, and each int32 phi's made, uses are through its box
	// too, which unboxPhis left in the header for the uses after the loop
	// (sinkLoopBoxes moves it there later), and an int32 phi's through its
	// conversion, the old phi, and that one's box: alias is, by value ID,
	// the phi a value stands for, or nil -- each made in the header, before
	// any use in the blocks' order, a loop's header before its body and
	// its exits. The blocks that use each, once each, are a list of nodes
	// from head, by value ID, in one walk.
	alias, head, last := f.refsOf(n), f.ints(n), f.ints(n)
	type node struct{ block, next int }
	var nodes []node
	of := func(a *Value) *Value {
		if a.ID < n && alias[a.ID] != nil {
			return alias[a.ID]
		}
		if tracked(a) {
			return a
		}
		return nil
	}
	note := func(a *Value, b *Block) {
		x := of(a)
		if x == nil || last[x.ID] == b.ID+1 {
			return
		}
		last[x.ID] = b.ID + 1
		nodes = append(nodes, node{b.ID, head[x.ID]})
		head[x.ID] = len(nodes)
	}
	for _, b := range f.Blocks {
		for _, v := range b.Values {
			if v.Op == OpPhi {
				for i, a := range v.Args {
					note(a, b.Preds[i])
				}
				continue
			}
			for _, a := range v.Args {
				note(a, b)
			}
			if a := v.Args; (v.Op == OpBoxF64 || v.Op == OpI32ToF64) && len(a) == 1 && v.ID < n {
				if x := of(a[0]); x != nil {
					alias[v.ID] = x
				}
			}
		}
		if c := b.Control; c != nil {
			note(c, b)
		}
	}
	ids := 0
	for _, b := range f.Blocks {
		ids = max(ids, b.ID+1)
	}
	index := f.ints(ids)
	for i, b := range f.Blocks {
		index[b.ID] = i
	}
	dropped := false
	var inLoop []bool
	var mine, earlier []*Value
	var cost, order []int
	for _, h := range f.Blocks {
		if !h.LoopHeader {
			continue
		}
		mine, earlier = mine[:0], earlier[:0]
		held := 0
		for _, v := range h.Values {
			if v.Op != OpPhi {
				break
			}
			switch {
			case isCand(v):
				mine = append(mine, v)
			case made(v):
				earlier = append(earlier, v)
			case v.Type != Float64 && !slices.Contains(v.Args, v):
				// A phi the loop changes, in a general register.
				held++
			}
		}
		if len(mine) == 0 {
			continue
		}
		if inLoop == nil {
			inLoop = f.bools(len(f.Blocks))
		} else {
			clear(inLoop)
		}
		loopBlocks(h, inLoop, index)
		regs := func(v *Value) int {
			for k := head[v.ID]; k != 0; k = nodes[k-1].next {
				if !inLoop[index[nodes[k-1].block]] {
					return 2
				}
			}
			return 1
		}
		for _, v := range earlier {
			held += regs(v)
		}
		cost, order = cost[:0], order[:0]
		for i, v := range mine {
			cost, order = append(cost, regs(v)), append(order, i)
		}
		slices.SortStableFunc(order, func(a, b int) int { return cost[a] - cost[b] })
		for _, i := range order {
			if held+cost[i] <= intPhiBudget {
				held += cost[i]
				continue
			}
			cand[mine[i].ID], dropped = false, true
			f.scr().intNo[mine[i].ID] = true
		}
	}
	return dropped
}

// intPhiUses drops each candidate that its uses would rather have a
// double, and reports whether it dropped any. An int32 phi pays for each
// use of it as a double with a conversion, at every turn of its loop; a
// candidate is kept if a use takes it as an int32 -- its ToInt32, a
// comparison, an element's key, a sum or difference truncInts makes an
// int32 one -- and none as a double but those, a box or a phi: Crypto's
// am3 carries x & 0x3fff and x >> 14 through its loop for products with
// its digits, and had converted both at every digit.
func intPhiUses(f *Func, phis []*Value, cand []bool) bool {
	n := len(cand)
	isCand := func(a *Value) bool { return a.ID < n && cand[a.ID] }
	// The arithmetic a ToInt32 takes the result of, as truncInts makes it
	// int32 arithmetic: sums, differences, negations and products by
	// constants, nested.
	sc := f.scr()
	sc.intTrunc = growFlags(sc.intTrunc, f.nextID)
	for len(sc.intOf) < f.nextID {
		sc.intOf = append(sc.intOf, nil)
	}
	truncated, of := sc.intTrunc, sc.intOf
	var touched []*Value
	defer func() {
		for _, v := range touched {
			truncated[v.ID], of[v.ID] = false, nil
		}
	}()
	var mark func(v *Value, depth int)
	mark = func(v *Value, depth int) {
		if v.ID >= len(truncated) || truncated[v.ID] || depth == 0 {
			return
		}
		switch v.Op {
		case OpAddF64, OpSubF64, OpNegF64:
		case OpMulF64:
			if !numConst(v.Args[0]) && !numConst(v.Args[1]) {
				return
			}
		case OpUnboxF64:
			// A box's unboxing, before simplify cancels the two: the
			// bitwise operator's operand, the arithmetic's box.
			if b := v.Args[0]; b.Op == OpBoxF64 {
				mark(b.Args[0], depth)
			}
			return
		default:
			return
		}
		truncated[v.ID] = true
		touched = append(touched, v)
		for _, a := range v.Args {
			mark(a, depth-1)
		}
	}
	// Each candidate's value is also its box's, which unboxPhis left for
	// its old uses, and the unboxings of that box (simplify cancels them
	// later): of, by value ID, the candidate a value stands for.
	for _, b := range f.Blocks {
		for _, v := range b.Values {
			switch {
			case v.Op == OpToInt32:
				mark(v.Args[0], 8)
			case v.Op == OpBoxF64 && isCand(v.Args[0]):
				of[v.ID] = v.Args[0]
				touched = append(touched, v)
			case v.Op == OpUnboxF64 && of[v.Args[0].ID] != nil:
				of[v.ID] = of[v.Args[0].ID]
				touched = append(touched, v)
			}
		}
	}
	// The candidates merged by one another, as a value carried round a
	// loop through its header's phi and its body's, are one group (root),
	// which wants an int32, or a double, if any member does.
	for len(sc.intRoot) < n {
		sc.intRoot = append(sc.intRoot, 0)
	}
	root := sc.intRoot
	for _, v := range phis {
		root[v.ID] = v.ID
	}
	var find func(i int) int
	find = func(i int) int {
		for root[i] != i {
			root[i] = root[root[i]]
			i = root[i]
		}
		return i
	}
	for _, v := range phis {
		if !cand[v.ID] {
			continue
		}
		for _, a := range v.Args {
			if isCand(a) {
				root[find(a.ID)] = find(v.ID)
			}
		}
	}
	// A use as a double counts against a candidate only in its loop, where
	// it runs at every turn: the blocks of each candidate's header's loop,
	// found once for each header that has such a use (loopBlocks).
	ids := 0
	for _, b := range f.Blocks {
		ids = max(ids, b.ID+1)
	}
	index := f.ints(ids)
	for i, b := range f.Blocks {
		index[b.ID] = i
	}
	sets := make(map[*Block][]bool)
	inLoop := func(h, b *Block) bool {
		in := sets[h]
		if in == nil {
			in = f.bools(len(f.Blocks))
			loopBlocks(h, in, index)
			sets[h] = in
		}
		return in[index[b.ID]]
	}
	sc.intWants, sc.intAgainst = growFlags(sc.intWants, n), growFlags(sc.intAgainst, n)
	wants, against := sc.intWants, sc.intAgainst
	defer func() {
		for _, v := range phis {
			wants[v.ID], against[v.ID] = false, false
		}
	}()
	for _, b := range f.Blocks {
		for _, u := range b.Values {
			for i, a := range u.Args {
				x := a
				if x.ID < len(of) && of[x.ID] != nil {
					x = of[x.ID]
				}
				if !isCand(x) {
					continue
				}
				g := find(x.ID)
				switch {
				case u.Op == OpPhi, u.Op == OpBoxF64 && a == x:
				case a != x && a.Op == OpBoxF64 && u.Op == OpUnboxF64:
					// Its box unboxed: the unboxing's uses are its own.
				case u.Op == OpToInt32, u.Op == OpCmpF64:
					wants[g] = true
				case (u.Op == OpElemRead || u.Op == OpElemWrite) && i == 1:
					wants[g] = true
				case truncated[u.ID]:
					wants[g] = true
				case inLoop(x.Block, b):
					// A double's use in its loop -- its box read as a
					// value too -- made of the int32 at every turn.
					against[g] = true
				}
			}
		}
	}
	// One dropped is so for the rounds after (Func's intNo), which find
	// its uses as this one did, folded or not.
	dropped := false
	for _, v := range phis {
		if g := find(v.ID); cand[v.ID] && (against[g] || !wants[g]) {
			cand[v.ID], dropped = false, true
			f.scr().intNo[v.ID] = true
		}
	}
	return dropped
}

// numConst reports a numeric constant, as a double or, before simplify
// unboxes it, a number's constant unboxed.
func numConst(v *Value) bool {
	return v.Op == OpConstF64 || v.Op == OpUnboxF64 && v.Args[0].Op == OpConst && v.Args[0].Const.Kind == ir.Number
}

// growFlags is t at least n long, the flags past its length false.
func growFlags(t []bool, n int) []bool {
	if len(t) < n {
		t = append(t, make([]bool, n-len(t))...)
	}
	return t
}
