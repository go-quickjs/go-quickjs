package ssa

import (
	"math"
	"strconv"

	"github.com/go-quickjs/go-quickjs/internal/jit/ir"
)

// Optimize runs the passes until nothing changes:
//   - trivial phis (every argument the same value, or the phi itself) become
//     that value;
//   - a guard that cannot fail goes: unboxing a boxed number or a numeric
//     constant, truthiness of a boxed boolean, an initialization check of a
//     box or of a constant that is not the uninitialized marker;
//   - ToInt32 of an integer converted to a number is that integer, and a
//     double Not is its operand;
//   - a pure numeric operation on constants is folded, by the same apply the
//     evaluator uses;
//   - within a block, a guard repeating an earlier one on the same value is
//     the earlier one;
//   - values nothing uses are deleted. A guard that can fail is never
//     deleted: it is an effect.
func Optimize(f *Func) {
	// Shadows, and the stores' checks of them, are made at the end, for
	// the phis that are left, and again if they were made already.
	if f.shadowed {
		clearStoreChecks(f)
		clearShadows(f)
	}
	f.originsKept = false
	// Tables are by value ID, not maps: a compile at run time pays for every
	// pass (BenchmarkJITCompile in internal/vm).
	subst := f.scr().subst
	// A guard repeats another that dominates it, as V8's redundancy
	// elimination has it: the blocks are in reverse post-order, each after
	// its dominator (Func.Blocks).
	dom := dominators(f)
	// The guards met, of each operand in the order met, by op: chains
	// through a list the Func's scratch keeps, from tables by operand ID.
	sc := f.scr()
	var first, last []int32
	guards := sc.guards[:0]
	// repeated is the first guard of op and a's that dominates b and is
	// not replaced, or nil; meet adds v as one. A guard asks what its
	// constant and index say too -- which object (OpSameObject), which type
	// (OpTypeIs) -- so the guard it repeats asks the same, v's.
	repeated := func(op Op, a *Value, b *Block, v *Value) *Value {
		for i := first[a.ID]; i != 0; i = guards[i-1].next {
			if g := &guards[i-1]; g.op == op && subst[g.v.ID] == nil && dom.dominates(g.v.Block, b) &&
				(op == OpCheckInit || g.v.Const == v.Const && g.v.Index == v.Index && g.v.Type == v.Type) {
				return g.v
			}
		}
		return nil
	}
	meet := func(op Op, a, v *Value) {
		guards = append(guards, guardSeen{v: v, op: op})
		n := int32(len(guards))
		if last[a.ID] != 0 {
			guards[last[a.ID]-1].next = n
		} else {
			first[a.ID] = n
		}
		last[a.ID] = n
	}
	for round := 0; round < 32; round++ {
		// Phis are unboxed first, so that the boxes and unboxings it makes
		// cancel in this round's walk, not the next's: most functions are
		// then done in two rounds, the second finding nothing.
		unboxed := unboxPhis(f)
		if unboxBigPhis(f) {
			unboxed = true
		}
		first, last, guards = idTable(sc.first, f.nextID), idTable(sc.last, f.nextID), guards[:0]
		sc.first, sc.last = first, last
		subst = idTable(subst, f.nextID)
		replaced := false
		find := func(v *Value) *Value {
			for v.ID < len(subst) && subst[v.ID] != nil {
				v = subst[v.ID]
			}
			return v
		}
		changed := unboxed
		for _, b := range f.Blocks {
			for _, v := range b.Values {
				if subst[v.ID] != nil {
					continue
				}
				for i, a := range v.Args {
					v.Args[i] = find(a)
				}
				if w := simplify(f, v); w != nil {
					subst[v.ID], replaced = w, true
					changed = true
					continue
				}
				// A value a dominating guard found initialized -- or an object, a
				// number or an array, which an uninitialized binding is not --
				// is initialized still: its check is none.
				if len(v.Args) == 1 {
					switch v.Op {
					case OpCheckInit, OpObjectOf, OpUnboxF64, OpArrayOf, OpIndexedOf:
						if v.Op == OpCheckInit {
							if w := repeated(OpCheckInit, v.Args[0], b, v); w != nil {
								subst[v.ID], replaced = w, true
								changed = true
								continue
							}
						}
						meet(OpCheckInit, v.Args[0], v)
					}
				}
				// A guard of one operand repeats another of the same op and
				// operand; one of more operands is never merged, since the
				// key names only the first.
				if v.Op.isGuard() && v.Op != OpCheckInit && !v.Op.readsMemory() && len(v.Args) == 1 {
					if w := repeated(v.Op, v.Args[0], b, v); w != nil {
						subst[v.ID], replaced = w, true
						changed = true
						continue
					}
					meet(v.Op, v.Args[0], v)
				}
			}
		}
		if replaced {
			rewrite(f, find)
		}
		// Another round is for what this one's replacements may simplify
		// further: a phi its back edge's operand, replaced after the phi
		// was met, may now make trivial. Taking dead values away, and a
		// value rewritten in place -- a constant folded, met by its users
		// later in the walk, or a guard that cannot fail, which nothing
		// uses -- simplify nothing more: a value's simplification asks
		// only what its operands are, and a phi's only which, so a round
		// that only did those is the last.
		removeDead(f)
		f.rounds = round + 1
		if !changed {
			break
		}
	}
	if rematerializeUpvalues(f) {
		removeDead(f)
	}
	stringBuilders(f)
	shadowMerges(f)
	if kept, stores, live := keepAcrossStores(f); kept {
		// The phis' shadows again, for the phis the keeps made; those
		// made before, now unused, go.
		clearShadows(f)
		removeDead(f)
		shadowMerges(f)
		storeChecks(f)
	} else {
		storeChecksAt(f, stores, live)
		if verifyLiveness {
			checkStoreChecks(f)
		}
	}
	clear(guards)
	sc.guards = guards[:0]
	clear(subst)
	sc.subst = subst[:0]
	f.shadowed, f.originsKept = true, true
	recount(f)
}

// idTable returns t cleared and n long, reusing its storage.
func idTable[T any](t []T, n int) []T {
	if cap(t) < n {
		return make([]T, n)
	}
	t = t[:n]
	clear(t)
	return t
}

// simplify returns a value v can be replaced by, or nil. It may instead
// rewrite v in place (into a constant, or into a guard that cannot fail,
// which removeDead then deletes).
func simplify(f *Func, v *Value) *Value {
	arg := func(i int) *Value { return v.Args[i] }
	switch v.Op {
	case OpPhi:
		var same *Value
		for _, a := range v.Args {
			if a == v || a == same {
				continue
			}
			if same != nil {
				return nil
			}
			same = a
		}
		return same
	case OpElemStore:
		// A number stored as one: its element's number word alone. Its
		// checks are made after (storeChecks), as an element write's:
		// what it leaves to Go may write over a reference's cell.
		if x := arg(2); x.Op == OpBoxF64 {
			array, key := arg(0), arg(1)
			v.Op, v.Args, v.Index = OpElemWrite, f.refsOf(3), 0
			v.Args[0], v.Args[1], v.Args[2] = array, key, x.Args[0]
		}
		return nil
	case OpUnboxF64:
		switch a := arg(0); {
		case a.Op == OpBoxF64:
			return a.Args[0]
		case a.Op == OpConst && a.Const.Kind == ir.Number:
			v.Op, v.Const, v.Args, v.State = OpConstF64, a.Const, nil, nil
		}
	case OpTruth:
		if a := arg(0); a.Op == OpBoxBool {
			return a.Args[0]
		}
	case OpBigUnbox, OpBigUnboxU64, OpBigLow:
		a := arg(0)
		if a.Op == OpKept && a.Args[1].Op == OpBigBox {
			// A BigInt boxed (OpBigBox): its value, kept in its cell --
			// checked to be in the other type's range where it is not of
			// v's (OpBigSign), the low bits of either as they are.
			x := a.Args[1].Args[0]
			switch {
			case x.Type == v.Type:
				return x
			case v.Op == OpBigLow:
				v.Op, v.Index, v.Args, v.State, v.Aux = OpZextW, 64, v.Args[:1], nil, 0
				v.Args[0] = x
			default:
				v.Op = OpBigSign
				v.Args[0] = x
			}
			return nil
		}
		// A constant held in its value (ir.BigIntConst): its bits, where
		// they are of v's type, which they are past the int64 range only
		// as a Uint64, and below 0 only as an Int64.
		if bits, kind, ok := bigConst(a); ok {
			neg := kind == BigConstInt64 && int64(bits) < 0
			if v.Op == OpBigLow || v.Op == OpBigUnbox && kind == BigConstInt64 || v.Op == OpBigUnboxU64 && !neg {
				v.Op, v.Const, v.Args, v.State, v.Aux = OpConstI64, a.Const, nil, nil, 0
			}
		}
	case OpBigSign:
		if a := arg(0); a.Op == OpConstI64 && int64(a.Const.Bits) >= 0 {
			v.Op, v.Const, v.Args, v.State, v.Aux = OpConstI64, a.Const, nil, nil, 0
		}
	case OpBigOfF64:
		// BigInt(c) of a constant integer in the int64 range: its bits --
		// a loop's sum = BigInt(0), which a phi then takes as a Uint64 as
		// well as an Int64 (unboxBigPhis).
		if a := arg(0); a.Op == OpConstF64 {
			if c := math.Float64frombits(a.Const.Bits); c == math.Trunc(c) && c >= -(1<<63) && c < 1<<63 {
				v.Op, v.Const, v.Args, v.State, v.Aux = OpConstI64, ir.Value{Bits: uint64(int64(c))}, nil, nil, 0
			}
		}
	case OpCheckInit:
		a := arg(0)
		if a.Op == OpBoxF64 || a.Op == OpBoxBool || a.Op == OpConst && a.Const.Kind != ir.Uninitialized {
			v.Op, v.Args, v.State = OpInvalid, nil, nil
		}
	case OpCheckTrue:
		if a := arg(0); a.Op == OpConst && a.Const == ir.Bool(true) {
			v.Op, v.Args, v.State = OpInvalid, nil, nil
		}
	case OpToInt32:
		if a := arg(0); a.Op == OpI32ToF64 || a.Op == OpU32ToF64 {
			return a.Args[0]
		} else if a.Op == OpBoolToF64 {
			b := a.Args[0]
			v.Op, v.Args = OpBoolToI32, f.refsOf(1)
			v.Args[0] = b
		}
	case OpNot:
		if a := arg(0); a.Op == OpNot {
			return a.Args[0]
		}
	case OpAddF64, OpSubF64, OpMulF64, OpDivF64, OpNegF64, OpSqrtF64, OpAbsF64, OpMaxF64, OpMinF64,
		OpFloorF64, OpCeilF64, OpTruncF64, OpRoundF64, OpSignF64, OpFroundF64:
		for _, a := range v.Args {
			if a.Op != OpConstF64 {
				return nil
			}
		}
		var x, y val
		x.f = math.Float64frombits(arg(0).Const.Bits)
		if len(v.Args) > 1 {
			y.f = math.Float64frombits(arg(1).Const.Bits)
		}
		r := apply(v.Op, v.Aux, x, y)
		v.Op, v.Const, v.Args = OpConstF64, ir.Value{Kind: ir.Number, Bits: math.Float64bits(r.f)}, nil
	}
	return nil
}

// unboxPhis gives a phi that only ever carries numbers a Float64 phi, so a
// loop's numbers stay unboxed. A candidate's arguments are boxed numbers,
// numeric constants, other candidates, or slots loaded at an entry; a loaded
// slot is unboxed by a guard in its entry block, which exits there if the
// slot is not a number (a speculation, as V8's optimizing tiers make). A candidate
// needs a reason: a use that unboxes it, or a candidate with one that it
// flows into. A phi that only carries a number -- x=i, returned -- gains
// nothing unboxed, and its entry guard would exit every time the slot it is
// loaded from holds anything else, such as the reference x held before the
// loop. The old phi becomes a box of the new one, which other passes then
// cancel.
func unboxPhis(f *Func) bool {
	// By value ID: n bounds the IDs of the values there are now; the values
	// this pass makes are numbered from n.
	n := f.nextID
	// The candidates, in block order, and what guards unbox: evidence is an
	// unboxing that speculates, as arithmetic makes. One that exits to Go (a
	// comparison, a property write, a remainder) marks a site that takes any
	// value, which says nothing of the phi's: counting it had the entry
	// speculate a receiver written to a property is a number, and fail
	// every call.
	phis, guarded := f.scr().phis[:0], f.scr().work[:0]
	for _, b := range f.Blocks {
		for _, v := range b.Values {
			switch {
			case v.Op == OpPhi && v.Type == Tagged && !trivialPhi(v):
				// A trivial phi is the value it carries, which this round
				// makes it: unboxed, it would speculate on an entry's slot
				// where only one path used it as a number.
				phis = append(phis, v)
			case v.Op == OpUnboxF64 && ir.ExitKind(v.Aux) == ir.GuardExit:
				guarded = append(guarded, v.Args[0])
			}
		}
	}
	f.scr().phis, f.scr().work = phis[:0], guarded[:0] // kept for the next round
	if len(phis) == 0 {
		return false
	}
	// Three tables of flags and two of values, by ID, from scratch the
	// Func keeps.
	flags := idTable(f.scr().flags, 3*n)
	f.scr().flags = flags
	cand, unboxedUse, boxed := flags[:n:n], flags[n:2*n:2*n], flags[2*n:]
	vals := idTable(f.scr().vals, 2*n)
	f.scr().vals = vals
	for _, v := range phis {
		cand[v.ID] = true
	}
	for _, a := range guarded {
		unboxedUse[a.ID] = true
	}
	isCand := func(a *Value) bool { return a.ID < n && cand[a.ID] }
	numeric := func(a *Value) bool {
		return a.Op == OpBoxF64 || a.Op == OpConst && a.Const.Kind == ir.Number
	}
	// loaded is a slot loaded at an entry whose speculation has not
	// failed, or a captured binding read from its cell there or after a
	// call, with the state to exit to (builder.reloadUpvalues).
	loaded := func(a *Value) bool {
		upvalue := a.Op == OpLoadCell && (a.Args[0].Op == OpUpvalueCell || a.Args[0].Op == OpLocalCell)
		return upvalue && a.State != nil || (a.Op == OpLoadSlot || upvalue) && a.Block.PC < 0 && !a.Block.Generic
	}
	// Filter until stable: a candidate's arguments must be acceptable, and it
	// needs evidence, directly or through candidates. Removing one candidate
	// can disqualify another, so both filters repeat. The result is the same
	// in any order.
	for changed := true; changed; {
		changed = false
		for _, v := range phis {
			if !cand[v.ID] {
				continue
			}
			for _, a := range v.Args {
				if !numeric(a) && !isCand(a) && !loaded(a) {
					cand[v.ID] = false
					changed = true
					break
				}
			}
		}
		// Evidence flows from a candidate with it to the candidates it
		// merges, by a worklist: a pass over the phis until none grew took
		// a pass for each phi of a chain, and a function of hundreds of
		// calls, a phi of its loop's counter at the entry after each, took
		// hundreds of passes -- RegExp's runBlock11 a fifth of its compile.
		clear(boxed)
		stack := guarded[:0]
		for _, v := range phis {
			if cand[v.ID] && unboxedUse[v.ID] {
				boxed[v.ID] = true
				stack = append(stack, v)
			}
		}
		for len(stack) != 0 {
			v := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			for _, a := range v.Args {
				if isCand(a) && !boxed[a.ID] {
					boxed[a.ID] = true
					stack = append(stack, a)
				}
			}
		}
		guarded = stack[:0]
		f.scr().work = guarded
		for _, v := range phis {
			if cand[v.ID] && !boxed[v.ID] {
				cand[v.ID] = false
				changed = true
			}
		}
	}
	// In block order: value numbers, and so register allocation and code,
	// must not vary from one compilation to the next.
	fp := vals[:n:n]
	ordered := f.scr().ordered[:0]
	for _, v := range phis {
		if cand[v.ID] {
			p := f.alloc(Value{Op: OpPhi, Type: Float64, Block: v.Block})
			fp[v.ID] = p
			ordered = append(ordered, v)
		}
	}
	f.scr().ordered = ordered[:0]
	if len(ordered) == 0 {
		return false
	}
	unboxed := vals[n:]
	for _, v := range ordered {
		p := fp[v.ID]
		p.Args = f.refsOf(len(v.Args))[:0]
		for _, a := range v.Args {
			var x *Value
			switch {
			case a.Op == OpBoxF64:
				x = a.Args[0]
			case a.Op == OpConst:
				// A numeric constant: its number, defined beside it.
				x = unboxed[a.ID]
				if x == nil {
					x = f.alloc(Value{Op: OpConstF64, Type: Float64, Const: a.Const, Block: a.Block})
					insertAfter(f, a, x)
					unboxed[a.ID] = x
				}
			case isCand(a):
				x = fp[a.ID]
			default:
				x = unboxed[a.ID]
				if x == nil && a.Block.PC >= 0 {
					// A captured binding read after a call: unboxed there.
					x = f.alloc(Value{Op: OpUnboxF64, Type: Float64, Args: f.refsOf(1),
						Aux: int(ir.GuardExit), State: a.State, Block: a.Block})
					x.Args[0] = a
					insertAfter(f, a, x)
					unboxed[a.ID] = x
				} else if x == nil {
					e := a.Block
					x = f.alloc(Value{Op: OpUnboxF64, Type: Float64, Args: f.refsOf(1),
						Aux: int(ir.GuardExit), State: e.Header, Block: e})
					x.Args[0] = a
					e.Values = f.appendValue(e.Values, x)
					unboxed[a.ID] = x
				}
			}
			p.Args = append(p.Args, x)
		}
	}
	// Each block's values become its phis, the new numeric ones in place of
	// those they replace, the boxes of those, then the rest, in order:
	// built in buffers the pass reuses. An old phi becomes its box, so
	// whatever used it -- values, frame states, controls -- uses the box.
	front, boxes, rest := f.scr().front, f.scr().boxes, f.scr().rest
	// Only the blocks of the phis unboxed change: ordered is in block
	// order, and phis come first in a block, where every pass puts them.
	var last *Block
	for _, o := range ordered {
		b := o.Block
		if b == last {
			continue
		}
		last = b
		front, boxes, rest = front[:0], boxes[:0], rest[:0]
		for _, v := range b.Values {
			if v.Op != OpPhi {
				rest = append(rest, v)
				continue
			}
			if v.ID >= n || fp[v.ID] == nil {
				front = append(front, v)
				continue
			}
			p := fp[v.ID]
			front = append(front, p)
			*v = Value{ID: v.ID, Op: OpBoxF64, Type: Tagged, Args: v.Args[:1], Block: b, Uses: v.Uses}
			v.Args[0] = p
			boxes = append(boxes, v)
		}
		total := len(front) + len(boxes) + len(rest)
		if cap(b.Values) < total {
			b.Values = f.refsOf(total)
		}
		b.Values = b.Values[:total]
		copy(b.Values, front)
		copy(b.Values[len(front):], boxes)
		copy(b.Values[len(front)+len(boxes):], rest)
	}
	f.scr().front, f.scr().boxes, f.scr().rest = front[:0], boxes[:0], rest[:0]
	return true
}

// rematerializeUpvalues replaces each tagged phi that only merges reads of
// one captured binding's cell -- at entries, after calls, through other
// such phis -- with a read of the cell where the phi is. Native code's
// binding is its cell's value throughout: it assigns both, and reads the
// cell again after a call (builder.reloadUpvalues). Carried through a loop,
// such a phi held a register and its pointer word another, and its loads
// at every entry stayed live; read again, it holds nothing until used, as
// a slot loaded at an entry does in mir. What unboxPhis made a number is no
// longer a tagged phi, and stays as it is.
func rematerializeUpvalues(f *Func) bool {
	// cand, by ID, is a phi's cell -- its captured binding's index plus
	// one -- or pending, for one whose arguments so far are such phis, or
	// none or failed.
	const none, failed, pending = 0, -1, -2
	var cand []int32
	phis := f.scr().phis[:0]
	loads := false
	for _, b := range f.Blocks {
		for _, v := range b.Values {
			if v.Op == OpPhi && v.Type == Tagged {
				phis = append(phis, v)
			}
			loads = loads || v.Op == OpLoadCell && v.Args[0].Op == OpUpvalueCell && v.Args[0].Aux < 2
		}
	}
	f.scr().phis = phis[:0]
	if !loads || len(phis) == 0 {
		return false
	}
	cand = make([]int32, f.nextID)
	for _, v := range phis {
		cand[v.ID] = pending
	}
	// One a loop header's state names, for a local that copied the
	// binding, is there before the read would be.
	for _, b := range f.Blocks {
		if b.Header != nil && b.PC >= 0 {
			for _, v := range b.Header.Slots {
				if v != nil && v.Op == OpPhi && v.Block == b {
					cand[v.ID] = failed
				}
			}
		}
	}
	// A phi of loads of another cell, or of anything else, fails, and so
	// does every phi that merges it: until nothing changes, the phis met
	// pending taken for the same cell.
	for changed := true; changed; {
		changed = false
		for _, v := range phis {
			if cand[v.ID] == failed {
				continue
			}
			want := cand[v.ID]
			for _, a := range v.Args {
				var k int32
				switch {
				case a.Op == OpLoadCell && a.Args[0].Op == OpUpvalueCell && a.Args[0].Aux < 2:
					// A binding's cell, or a captured local's of the code's
					// own (Aux 1), each its own; not an inlined callee's
					// (Aux 2), which the code reads anew each time.
					k = int32(2*a.Args[0].Index+a.Args[0].Aux) + 1
				case a.Op == OpPhi && a.ID < len(cand) && cand[a.ID] != none:
					k = cand[a.ID]
				default:
					k = failed
				}
				if k == pending {
					continue
				}
				if k == failed || want != pending && want != k {
					want = failed
					break
				}
				want = k
			}
			if want != cand[v.ID] {
				cand[v.ID], changed = want, true
			}
		}
	}
	subst := make([]*Value, f.nextID)
	for _, b := range f.Blocks {
		var reads []*Value
		for _, v := range b.Values {
			if v.Op != OpPhi || cand[v.ID] <= 0 {
				continue
			}
			k := int(cand[v.ID] - 1)
			cell := f.alloc(Value{Op: OpUpvalueCell, Type: Source, Index: k / 2, Aux: k % 2, Block: b})
			r := f.alloc(Value{Op: OpLoadCell, Type: Tagged, Args: f.refsOf(1), Block: b})
			r.Args[0], r.Shadow = cell, cell
			reads = append(reads, cell, r)
			subst[v.ID] = r
		}
		if len(reads) == 0 {
			continue
		}
		// After the block's phis.
		at := 0
		for at < len(b.Values) && b.Values[at].Op == OpPhi {
			at++
		}
		values := f.refsOf(len(b.Values) + len(reads))[:0]
		values = append(values, b.Values[:at]...)
		values = append(values, reads...)
		b.Values = append(values, b.Values[at:]...)
	}
	rewrite(f, func(v *Value) *Value {
		if v.ID < len(subst) && subst[v.ID] != nil {
			return subst[v.ID]
		}
		return v
	})
	return true
}

// trivialPhi reports a phi whose arguments are one value, or itself.
func trivialPhi(phi *Value) bool {
	var same *Value
	for _, a := range phi.Args {
		if a == phi || a == same {
			continue
		}
		if same != nil {
			return false
		}
		same = a
	}
	return true
}

// rewrite replaces every use of a value with find's answer for it.
func rewrite(f *Func, find func(*Value) *Value) {
	state := func(s *FrameState) {
		if s == nil {
			return
		}
		for i, v := range s.Slots {
			if v != nil {
				s.Slots[i] = find(v)
			}
		}
	}
	for _, b := range f.Blocks {
		kept := b.Values[:0]
		// The guards of an instruction share its state, rewritten once.
		var last *FrameState
		for _, v := range b.Values {
			if find(v) != v {
				continue
			}
			for i, a := range v.Args {
				v.Args[i] = find(a)
			}
			if v.Op == OpLoadCell || v.Op == OpKept {
				v.Shadow = v.Args[0]
			}
			if v.State != last {
				state(v.State)
				last = v.State
			}
			kept = append(kept, v)
		}
		b.Values = kept
		if b.Control != nil {
			b.Control = find(b.Control)
		}
		state(b.State)
		state(b.Header)
	}
}

// removeDead deletes values nothing live uses. Guards that can fail,
// controls, and the frame states of exits, guards and loop headers are what
// keeps values live.
func removeDead(f *Func) bool {
	live := f.bools(f.nextID)
	work := f.scr().work[:0]
	mark := func(v *Value) {
		if v != nil && !live[v.ID] {
			live[v.ID] = true
			work = append(work, v)
		}
	}
	// A state is marked once: the guards of an instruction share theirs.
	marked := f.bools(f.nextState)
	markState := func(s *FrameState) {
		if s != nil && (s.id >= len(marked) || !marked[s.id]) {
			if s.id < len(marked) {
				marked[s.id] = true
			}
			for _, v := range s.Slots {
				if v != nil {
					mark(v)
				}
			}
		}
	}
	for _, b := range f.Blocks {
		mark(b.Control)
		markState(b.State)
		markState(b.Header)
		for _, v := range b.Values {
			// A Math.random() takes its number from the cache whether or
			// not the number is used, as the interpreter's does: both
			// take from the one cache, in the same order.
			if v.Op.isGuard() || v.Op == OpFreeze || v.Op == OpRandomF64 || v.Op == OpLoopMark {
				mark(v)
			}
		}
	}
	for len(work) > 0 {
		v := work[len(work)-1]
		work = work[:len(work)-1]
		for _, a := range v.Args {
			mark(a)
		}
		markState(v.State)
	}
	f.scr().work = work[:0] // kept for the next round
	removed := false
	for _, b := range f.Blocks {
		kept := b.Values[:0]
		for _, v := range b.Values {
			if live[v.ID] {
				kept = append(kept, v)
			} else {
				removed = true
			}
		}
		b.Values = kept
	}
	return removed
}

// recount recomputes every value's use count.
func recount(f *Func) {
	for _, b := range f.Blocks {
		for _, v := range b.Values {
			v.Uses = 0
		}
	}
	use := func(v *Value) {
		if v != nil {
			v.Uses++
		}
	}
	state := func(s *FrameState) {
		if s != nil {
			for _, v := range s.Slots {
				if v != nil {
					use(v)
				}
			}
		}
	}
	for _, b := range f.Blocks {
		for _, v := range b.Values {
			for _, a := range v.Args {
				use(a)
			}
			state(v.State)
		}
		use(b.Control)
		state(b.State)
		state(b.Header)
	}
}

// insertAfter places v in a's block right after a.
func insertAfter(f *Func, a, v *Value) {
	b := a.Block
	for i, w := range b.Values {
		if w == a {
			b.Values = f.appendValue(b.Values, nil)
			copy(b.Values[i+2:], b.Values[i+1:])
			b.Values[i+1] = v
			return
		}
	}
	panic("ssa: insertAfter: value not in its block")
}

// domTree is a function's dominator tree: each block's immediate dominator,
// by block ID, -1 for an entry block, whose dominator is the function's
// (virtual) start; and each block's dominators as a bitset, words of them
// by block, for dominates to test.
type domTree struct {
	idom, set []int
	words     int
}

// dominators computes the dominator tree, by Cooper, Harvey and Kennedy's
// iteration over the blocks in reverse post-order, which their IDs are
// (Func.Blocks).
func dominators(f *Func) *domTree {
	d := &domTree{idom: f.ints(len(f.Blocks))}
	for i := range d.idom {
		d.idom[i] = -2
	}
	for _, e := range f.Entries {
		d.idom[e.Block.ID] = -1
	}
	intersect := func(a, b int) int {
		for a != b {
			for a > b {
				a = d.idom[a]
			}
			for b > a {
				b = d.idom[b]
			}
		}
		return a
	}
	for changed := true; changed; {
		changed = false
		for _, b := range f.Blocks {
			if d.idom[b.ID] == -1 {
				continue
			}
			nd := -2
			for _, p := range b.Preds {
				switch {
				case d.idom[p.ID] == -2:
				case nd == -2:
					nd = p.ID
				default:
					nd = intersect(p.ID, nd)
				}
			}
			if nd != d.idom[b.ID] {
				d.idom[b.ID], changed = nd, true
			}
		}
	}
	// A block's dominators are its immediate dominator's and itself; that
	// one comes first in the blocks' order.
	d.words = (len(f.Blocks) + strconv.IntSize - 1) / strconv.IntSize
	d.set = f.ints(len(f.Blocks) * d.words)
	for _, b := range f.Blocks {
		row := d.set[b.ID*d.words : (b.ID+1)*d.words]
		if up := d.idom[b.ID]; up >= 0 {
			copy(row, d.set[up*d.words:(up+1)*d.words])
		}
		row[b.ID/strconv.IntSize] |= 1 << (b.ID % strconv.IntSize)
	}
	return d
}

// dominates reports whether every path from an entry to b passes a.
func (d *domTree) dominates(a, b *Block) bool {
	return d.set[b.ID*d.words+a.ID/strconv.IntSize]&(1<<(a.ID%strconv.IntSize)) != 0
}
