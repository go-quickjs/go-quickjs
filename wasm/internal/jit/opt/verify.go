package opt

import (
	"fmt"
	"slices"
)

// Verify checks f's invariants: the edges agree with the blocks' kinds and
// each other, each phi has an argument for each predecessor, each value
// is in its block once, its arguments are values of the function defined
// where they dominate the use, and the types agree. It returns the first
// violation found.
func Verify(f *Func) error {
	if len(f.Blocks) == 0 || f.Blocks[0] != f.Entry {
		return fmt.Errorf("the entry is not the first block")
	}
	if len(f.Entry.Preds) != 0 {
		return fmt.Errorf("the entry has predecessors")
	}
	live := map[*Block]bool{}
	for _, b := range f.Blocks {
		if b.dead {
			return fmt.Errorf("%v is removed but listed", b)
		}
		if live[b] {
			return fmt.Errorf("%v is listed twice", b)
		}
		live[b] = true
	}
	// where says where each value is: its block and index, phis at -1.
	type pos struct {
		b *Block
		i int
	}
	where := map[*Value]pos{}
	for _, b := range f.Blocks {
		for _, v := range b.Phis {
			if v.Op != OpPhi {
				return fmt.Errorf("%v: %s among the phis", b, v.long())
			}
			if _, dup := where[v]; dup {
				return fmt.Errorf("%v is in the function twice", v)
			}
			where[v] = pos{b, -1}
		}
		for i, v := range b.Values {
			if v.Op == OpPhi || v.Op == OpInvalid {
				return fmt.Errorf("%v: %s among the values", b, v.long())
			}
			if _, dup := where[v]; dup {
				return fmt.Errorf("%v is in the function twice", v)
			}
			where[v] = pos{b, i}
		}
	}
	for _, b := range f.Blocks {
		want := -1
		switch b.Kind {
		case BPlain:
			want = 1
		case BIf:
			want = 2
		case BReturn, BTrap:
			want = 0
		case BTail:
			want = 0
			if n := len(b.Values); n == 0 || !isTail(b.Values[n-1].Op) {
				return fmt.Errorf("%v: a tail call's block not ending with one", b)
			}
		case BSwitch:
			if len(b.Succs) == 0 || len(b.Table) == 0 {
				return fmt.Errorf("%v: a switch without targets", b)
			}
			for _, i := range b.Table {
				if i < 0 || int(i) >= len(b.Succs) {
					return fmt.Errorf("%v: a switch's entry %d out of its %d successors", b, i, len(b.Succs))
				}
			}
		}
		if want >= 0 && len(b.Succs) != want {
			return fmt.Errorf("%v: kind %d with %d successors", b, b.Kind, len(b.Succs))
		}
		if (b.Kind == BIf || b.Kind == BSwitch) != (b.Control != nil) {
			return fmt.Errorf("%v: kind %d with control %v", b, b.Kind, b.Control)
		}
		if b.Control != nil && b.Control.Type != TI32 {
			return fmt.Errorf("%v: control %v of type %v", b, b.Control, b.Control.Type)
		}
		for _, s := range b.Succs {
			if !live[s] {
				return fmt.Errorf("%v: successor %v is not in the function", b, s)
			}
			if countOf(s.Preds, b) != countOf(b.Succs, s) {
				return fmt.Errorf("%v -> %v: the edge is not among %v's predecessors", b, s, s)
			}
		}
		for _, p := range b.Preds {
			if !live[p] {
				return fmt.Errorf("%v: predecessor %v is not in the function", b, p)
			}
			if countOf(p.Succs, b) != countOf(b.Preds, p) {
				return fmt.Errorf("%v -> %v: the edge is not among %v's successors", p, b, p)
			}
		}
		if b.Kind == BReturn {
			if len(b.Results) != len(f.Results) {
				return fmt.Errorf("%v: returns %d values, not %d", b, len(b.Results), len(f.Results))
			}
			for i, r := range b.Results {
				if r.Type != f.Results[i] {
					return fmt.Errorf("%v: result %d is %v, not %v", b, i, r.Type, f.Results[i])
				}
			}
		}
	}
	d := newDominators(f)
	// dominatesUse reports whether a's definition comes before a use in
	// block b at index i (-1 for the block's phis, len(Values) for its
	// end).
	dominatesUse := func(a *Value, b *Block, i int) error {
		p, ok := where[a]
		if !ok {
			return fmt.Errorf("%v is not in the function", a)
		}
		if p.b == b {
			if p.i < i {
				return nil
			}
			return fmt.Errorf("%v is used in %v before it is defined", a, b)
		}
		if !d.dominates(p.b, b) {
			return fmt.Errorf("%v, defined in %v, does not dominate its use in %v", a, p.b, b)
		}
		return nil
	}
	for _, b := range f.Blocks {
		for _, v := range b.Phis {
			if v.Block != b {
				return fmt.Errorf("%v is in %v but says %v", v, b, v.Block)
			}
			if len(v.Args) != len(b.Preds) {
				return fmt.Errorf("%v: a phi of %d arguments in a block of %d predecessors", v, len(v.Args), len(b.Preds))
			}
			for i, a := range v.Args {
				if a.Type != v.Type {
					return fmt.Errorf("%v: a phi of %v with an argument %v of %v", v, v.Type, a, a.Type)
				}
				if err := dominatesUse(a, b.Preds[i], len(b.Preds[i].Values)); err != nil {
					return err
				}
			}
		}
		for i, v := range b.Values {
			if v.Block != b {
				return fmt.Errorf("%v is in %v but says %v", v, b, v.Block)
			}
			for _, a := range v.Args {
				if err := dominatesUse(a, b, i); err != nil {
					return err
				}
			}
			if err := checkTypes(v); err != nil {
				return fmt.Errorf("%s: %v", v.long(), err)
			}
		}
		uses := slices.Clone(b.Results)
		if b.Control != nil {
			uses = append(uses, b.Control)
		}
		for _, a := range uses {
			if err := dominatesUse(a, b, len(b.Values)); err != nil {
				return err
			}
		}
	}
	return nil
}

func countOf(bs []*Block, b *Block) int {
	n := 0
	for _, x := range bs {
		if x == b {
			n++
		}
	}
	return n
}

// checkTypes checks an operation's arguments' and result's types.
func checkTypes(v *Value) error {
	n := len(v.Args)
	arity := func(k int) error {
		if n != k {
			return fmt.Errorf("%d arguments, not %d", n, k)
		}
		return nil
	}
	same := func(t Type) error {
		for _, a := range v.Args {
			if a.Type != t {
				return fmt.Errorf("argument %v of type %v, not %v", a, a.Type, t)
			}
		}
		return nil
	}
	switch v.Op {
	case OpConst, OpParam:
		if !v.Type.IsInt() && !v.Type.IsFloat() {
			return fmt.Errorf("of type %v", v.Type)
		}
		return arity(0)
	case OpGlobalAddr:
		if v.Type != TI64 {
			return fmt.Errorf("of type %v", v.Type)
		}
		return arity(0)
	case OpGlobalGet:
		if !v.Type.IsInt() && !v.Type.IsFloat() {
			return fmt.Errorf("of type %v", v.Type)
		}
		if err := arity(1); err != nil {
			return err
		}
		if v.Args[0].Op != OpGlobalAddr {
			return fmt.Errorf("a global's address %v", v.Args[0])
		}
	case OpAdd, OpSub, OpMul, OpAnd, OpOr, OpXor, OpShl, OpShrS, OpShrU, OpRotl, OpRotr,
		OpDivS, OpDivU, OpRemS, OpRemU:
		if !v.Type.IsInt() {
			return fmt.Errorf("of type %v", v.Type)
		}
		if err := arity(2); err != nil {
			return err
		}
		return same(v.Type)
	case OpClz, OpCtz, OpPopcnt, OpExtend8S, OpExtend16S, OpExtend32S:
		if err := arity(1); err != nil {
			return err
		}
		return same(v.Type)
	case OpEqz:
		if err := arity(1); err != nil {
			return err
		}
		if v.Type != TI32 || !v.Args[0].Type.IsInt() {
			return fmt.Errorf("of %v", v.Args[0].Type)
		}
	case OpEq, OpNe, OpLtS, OpLtU, OpGtS, OpGtU, OpLeS, OpLeU, OpGeS, OpGeU:
		if err := arity(2); err != nil {
			return err
		}
		if v.Type != TI32 || !v.Args[0].Type.IsInt() {
			return fmt.Errorf("a comparison of %v", v.Args[0].Type)
		}
		return same(v.Args[0].Type)
	case OpWrap:
		if err := arity(1); err != nil {
			return err
		}
		return same(TI64)
	case OpExtendS, OpExtendU:
		if err := arity(1); err != nil {
			return err
		}
		return same(TI32)
	case OpFAdd, OpFSub, OpFMul, OpFDiv, OpFMin, OpFMax, OpFCopysign:
		if err := arity(2); err != nil {
			return err
		}
		if !v.Type.IsFloat() {
			return fmt.Errorf("of type %v", v.Type)
		}
		return same(v.Type)
	case OpFAbs, OpFNeg, OpFSqrt, OpFCeil, OpFFloor, OpFTrunc, OpFNearest:
		if err := arity(1); err != nil {
			return err
		}
		if !v.Type.IsFloat() {
			return fmt.Errorf("of type %v", v.Type)
		}
		return same(v.Type)
	case OpFEq, OpFNe, OpFLt, OpFGt, OpFLe, OpFGe:
		if err := arity(2); err != nil {
			return err
		}
		if v.Type != TI32 || !v.Args[0].Type.IsFloat() {
			return fmt.Errorf("a comparison of %v", v.Args[0].Type)
		}
		return same(v.Args[0].Type)
	case OpPromote:
		if v.Type != TF64 {
			return fmt.Errorf("of type %v", v.Type)
		}
		return same(TF32)
	case OpDemote:
		if v.Type != TF32 {
			return fmt.Errorf("of type %v", v.Type)
		}
		return same(TF64)
	case OpReinterpret:
		if err := arity(1); err != nil {
			return err
		}
		a := v.Args[0].Type
		ok := v.Type == TI32 && a == TF32 || v.Type == TI64 && a == TF64 || v.Type == TF32 && a == TI32 || v.Type == TF64 && a == TI64
		if !ok {
			return fmt.Errorf("%v as %v", a, v.Type)
		}
	case OpConvertS, OpConvertU:
		if err := arity(1); err != nil {
			return err
		}
		if !v.Type.IsFloat() || !v.Args[0].Type.IsInt() {
			return fmt.Errorf("%v to %v", v.Args[0].Type, v.Type)
		}
	case OpTruncS, OpTruncU, OpTruncSatS, OpTruncSatU:
		if err := arity(1); err != nil {
			return err
		}
		if !v.Type.IsInt() || !v.Args[0].Type.IsFloat() {
			return fmt.Errorf("%v to %v", v.Args[0].Type, v.Type)
		}
	case OpSelect:
		if err := arity(3); err != nil {
			return err
		}
		if v.Args[0].Type != v.Type || v.Args[1].Type != v.Type || v.Args[2].Type != TI32 {
			return fmt.Errorf("select of %v, %v by %v", v.Args[0].Type, v.Args[1].Type, v.Args[2].Type)
		}
	case OpLoad:
		if err := arity(1); err != nil {
			return err
		}
		if v.Args[0].Type != TI32 {
			return fmt.Errorf("an address of %v", v.Args[0].Type)
		}
	case OpStore:
		if err := arity(2); err != nil {
			return err
		}
		if v.Args[0].Type != TI32 {
			return fmt.Errorf("an address of %v", v.Args[0].Type)
		}
	case OpGlobalSet:
		if err := arity(2); err != nil {
			return err
		}
		if v.Args[0].Op != OpGlobalAddr {
			return fmt.Errorf("a global's address %v", v.Args[0])
		}
	case OpCall, OpCallIndirect, OpCallRef:
		if v.Type != TTuple {
			return fmt.Errorf("a call of type %v", v.Type)
		}
	case OpResult:
		if err := arity(1); err != nil {
			return err
		}
		if c := v.Args[0]; c.Op != OpCall && c.Op != OpCallIndirect && c.Op != OpCallRef {
			return fmt.Errorf("a result of %s", c.long())
		}
	case OpRef:
		if v.Type != TNone && !v.Type.IsInt() {
			return fmt.Errorf("of type %v", v.Type)
		}
	case OpTail, OpTailIndirect, OpTailRef:
		if v.Type != TNone {
			return fmt.Errorf("of type %v", v.Type)
		}
		if b := v.Block; b.Kind != BTail || b.Values[len(b.Values)-1] != v {
			return fmt.Errorf("a tail call that does not end its block")
		}
	case OpCopy:
		if err := arity(1); err != nil {
			return err
		}
		return same(v.Type)
	case OpGoOp:
	default:
		return fmt.Errorf("an unknown operation")
	}
	return nil
}

// checkLowerable checks what lowering requires of an optimized function:
// no critical edge, no phi in a block of one predecessor, and each call's
// results right after the call.
func checkLowerable(f *Func) error {
	for _, b := range f.Blocks {
		if len(b.Succs) > 1 {
			for _, s := range b.Succs {
				if len(s.Preds) > 1 {
					return fmt.Errorf("a critical edge %v -> %v", b, s)
				}
			}
		}
		if len(b.Phis) > 0 && len(b.Preds) < 2 {
			return fmt.Errorf("%v: phis in a block of %d predecessors", b, len(b.Preds))
		}
		for i, v := range b.Values {
			if v.Op != OpResult {
				continue
			}
			if i == 0 {
				return fmt.Errorf("%v: %v begins its block", b, v)
			}
			p := b.Values[i-1]
			if p != v.Args[0] && !(p.Op == OpResult && p.Args[0] == v.Args[0]) {
				return fmt.Errorf("%v: %v is not right after its call", b, v)
			}
		}
	}
	return nil
}
