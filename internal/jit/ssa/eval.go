package ssa

import (
	"errors"
	"fmt"
	"math"
	"slices"
	"unsafe"

	"github.com/go-quickjs/go-quickjs/internal/jit/ir"
)

// canonicalNaN is the NaN a number cell holds, and numberLimit bounds the
// words that hold numbers (abi.NumberLimit).
const (
	canonicalNaN = 0x7ff8000000000000
	numberLimit  = 0xFFF8000000000000
)

// integer64 reports whether x is an integer an int64 holds exactly.
func integer64(x float64) bool {
	return x >= -1<<63 && x < 1<<63 && math.Trunc(x) == x
}

// ExactPow is Math.pow(x, y) where native code computes it (OpPowF64), and
// false where it leaves the call to Go: x*x for y 2, as the VM's pow has
// it, and an integer x to an integer power from 0 to 64 whose result is at
// most 2**53, by repeated squaring, every product exact. The VM's pow errs
// by less than an ulp, so it gives such a result exactly too; past 2**53
// the two differ (10**23 is 1e23 squared so, 1.0000000000000001e23 by the
// VM's), and for a fraction (0.3**3).
func ExactPow(x, y float64) (float64, bool) {
	if y == 2 {
		return x * x, true
	}
	if !integer64(y) || y < 0 || y > 64 || !integer64(x) {
		return 0, false
	}
	r, b := 1.0, x
	for e := int(y); e != 0; {
		if e&1 != 0 {
			r *= b
		}
		if e >>= 1; e != 0 {
			b *= b
		}
	}
	if math.Abs(r) > 1<<53 {
		return 0, false
	}
	return r, true
}

// Extremum is Math.max of two numbers, or Math.min (OpMaxF64, OpMinF64):
// NaN if either is, and -0 below +0.
func Extremum(x, y float64, max bool) float64 {
	switch {
	case x != x || y != y:
		return math.NaN()
	case x == y:
		// Only the zeros' signs can differ.
		if max == math.Signbit(x) {
			return y
		}
		return x
	case (x > y) == max:
		return x
	}
	return y
}

// Round is Math.round(x) as native code computes it (OpRoundF64), as V8's
// does: the ceiling, less one if that is more than a half above x. The
// ceiling less a half is exact for every ceiling below 2**53, and past it
// never above x; NaN, the infinities and the zeros are their own ceilings,
// -0 kept for x in (-0.5, -0].
func Round(x float64) float64 {
	r := math.Ceil(x)
	if r-0.5 > x {
		return r - 1
	}
	return r
}

// Sign is Math.sign(x) (OpSignF64): 1 or -1, or x itself, a zero or NaN.
func Sign(x float64) float64 {
	switch {
	case x > 0:
		return 1
	case x < 0:
		return -1
	}
	return x
}

// index converts an element's key, as the slot IR does: an integer in
// [0, 2**32), negative zero included.
func index(x float64) (uint64, bool) {
	if x < 0 || x > math.MaxUint32 || math.Trunc(x) != x {
		return 0, false
	}
	return uint64(x), true
}

// element is the cell for key in an array view, if it holds a number.
func element(view ir.ArrayView, key float64) (*uint64, bool) {
	i, ok := index(key)
	if !ok || i >= view.DenseLength || view.Data == nil {
		return nil, false
	}
	cell := (*uint64)(unsafe.Add(view.Data, uintptr(i)*16))
	if *cell >= view.NumberLimit {
		return nil, false
	}
	return cell, true
}

// eqTagged is OpEqTagged's comparison, by the words native code sees: two
// numbers compare as numbers; equal words (objects' are one word, strings'
// another) are equal values but for objects, the same only if they are one,
// and strings, the same if one, unequal if of different lengths (a string
// the heap does not describe is empty), and otherwise compared by their
// code units if neither is a rope nor longer than maxEqualUnits, else left
// to Go; other words differ, strictly unequal. Loosely, null and undefined
// equal each other and an object with [[IsHTMLDDA]] alone, with nothing
// converted; any other two are left to Go. It reports false where Go
// decides.
func eqTagged(x, y ir.Value, strict bool, strings map[uint64]String, objects []Object) (bool, bool) {
	if x.Kind == ir.Number && y.Kind == ir.Number {
		return math.Float64frombits(x.Bits) == math.Float64frombits(y.Bits), true
	}
	if x.Kind == y.Kind && (x.Kind == ir.Opaque || x.Kind == ir.String || x.Bits == y.Bits) {
		switch x.Kind {
		case ir.Undefined, ir.Null, ir.Boolean:
			return true, true
		case ir.Opaque:
			return x.Bits == y.Bits, true
		case ir.String:
			if x.Bits == y.Bits {
				return true, true
			}
			a, b := strings[x.Bits], strings[y.Bits]
			if len(a.Units) != len(b.Units) {
				return false, true
			}
			if !a.Rope && !b.Rope && len(a.Units) <= maxEqualUnits {
				return slices.Equal(a.Units, b.Units), true
			}
		}
		return false, false
	}
	if !strict {
		nullish := func(v ir.Value) bool { return v.Kind == ir.Null || v.Kind == ir.Undefined }
		htmldda := func(v ir.Value) bool { return v.Bits < uint64(len(objects)) && objects[v.Bits].HTMLDDA }
		switch {
		case nullish(x) && nullish(y):
			return true, true
		case nullish(x) && y.Kind == ir.Opaque:
			return htmldda(y), true
		case nullish(y) && x.Kind == ir.Opaque:
			return htmldda(x), true
		case nullish(x) || nullish(y):
			return false, true
		}
	}
	return false, strict
}

// ErrOrigin reports a reference at an exit that is not where its origin
// says: a bug in origin.go.
var ErrOrigin = errors.New("ssa: a reference is not its origin's")

// val is a value at run time, in the representation its type names.
type val struct {
	t ir.Value // Tagged
	f float64  // Float64
	i uint32   // Int32, as bits; Source, a slot's index or -1
	p int      // Ptr: the array view's index
	// cell is a Source that is a heap cell, which an object's table holds.
	cell *ir.Value
	b    bool // Bool
}

// apply computes a pure, non-guard op. It is the definition of each op's
// semantics, shared by the evaluator and constant folding.
func apply(op Op, aux int, a, b val) val {
	switch op {
	case OpBoxF64:
		return val{t: ir.Value{Kind: ir.Number, Bits: math.Float64bits(a.f)}}
	case OpBoxBool:
		return val{t: ir.Bool(a.b)}
	case OpAddF64:
		return val{f: a.f + b.f}
	case OpSubF64:
		return val{f: a.f - b.f}
	case OpMulF64:
		return val{f: a.f * b.f}
	case OpDivF64:
		return val{f: a.f / b.f}
	case OpNegF64:
		return val{f: math.Float64frombits(math.Float64bits(a.f) ^ 1<<63)}
	case OpSqrtF64:
		return val{f: math.Sqrt(a.f)}
	case OpAbsF64:
		return val{f: math.Float64frombits(math.Float64bits(a.f) &^ (1 << 63))}
	case OpMaxF64, OpMinF64:
		return val{f: Extremum(a.f, b.f, op == OpMaxF64)}
	case OpFloorF64:
		return val{f: math.Floor(a.f)}
	case OpCeilF64:
		return val{f: math.Ceil(a.f)}
	case OpTruncF64:
		return val{f: math.Trunc(a.f)}
	case OpRoundF64:
		return val{f: Round(a.f)}
	case OpSignF64:
		return val{f: Sign(a.f)}
	case OpFroundF64:
		return val{f: float64(float32(a.f))}
	case OpCmpF64:
		x, y := a.f, b.f
		switch ir.Operator(aux) {
		case ir.Lt:
			return val{b: x < y}
		case ir.Le:
			return val{b: x <= y}
		case ir.Gt:
			return val{b: x > y}
		case ir.Ge:
			return val{b: x >= y}
		case ir.Eq:
			return val{b: x == y}
		case ir.Ne:
			return val{b: x != y}
		}
	case OpNot:
		return val{b: !a.b}
	case OpStrictNullish:
		kind := ir.Null
		if aux == 1 {
			kind = ir.Undefined
		}
		return val{b: a.t.Kind == kind}
	case OpToInt32:
		return val{i: ir.ToUint32(a.f)}
	case OpAndI32:
		return val{i: a.i & b.i}
	case OpOrI32:
		return val{i: a.i | b.i}
	case OpXorI32:
		return val{i: a.i ^ b.i}
	case OpShlI32:
		return val{i: a.i << (b.i & 31)}
	case OpSarI32:
		return val{i: uint32(int32(a.i) >> (b.i & 31))}
	case OpShrU32:
		return val{i: a.i >> (b.i & 31)}
	case OpNotI32:
		return val{i: ^a.i}
	case OpI32ToF64:
		return val{f: float64(int32(a.i))}
	case OpU32ToF64:
		return val{f: float64(a.i)}
	}
	panic("ssa: apply " + op.String())
}

// truth is JavaScript's truthiness for the kinds the slot IR decides it for.
func truth(v ir.Value) (bool, bool) {
	switch v.Kind {
	case ir.Undefined, ir.Null:
		return false, true
	case ir.Boolean:
		return v.Bits != 0, true
	case ir.Number:
		n := math.Float64frombits(v.Bits)
		return n != 0 && !math.IsNaN(n), true
	}
	return false, false
}

// Evaluate runs f from the entry for a slot IR PC, on slots (locals, then
// operands, as the slot IR evaluator takes them), and returns the exit the
// slot IR evaluator would: the same kind, state, value and live slots. Steps
// is not counted. With pollEvery > 0, every pollEvery-th backward branch
// exits as a budget exit at its loop header, where the function can be
// entered again.
//
// It also checks what generated code relies on to move references: at
// every exit and return, a reference is the value its origin slot held at
// entry (origin.go), and it reports ErrOrigin when one is not.
func Evaluate(f *Func, pc int, slots []ir.Value, pollEvery int) (ir.Exit, error) {
	return EvaluateArrays(f, pc, slots, nil, pollEvery)
}

// EvaluateArrays is Evaluate with arrays, as the slot IR's EvaluateArrays
// takes them: an Opaque value's Bits index arrays, and a view with a
// NumberLimit is an array whose cells it reads and writes in place.
func EvaluateArrays(f *Func, pc int, slots []ir.Value, arrays []ir.ArrayView, pollEvery int) (ir.Exit, error) {
	return EvaluateHeap(f, pc, slots, Heap{Arrays: arrays}, pollEvery)
}

// Heap is what an evaluation reads and writes besides the slots, by an
// Opaque value's Bits: arrays, as the slot IR's views, and objects.
type Heap struct {
	Arrays  []ir.ArrayView
	Objects []Object
	// Global is the object global names are read from, and Lexical the
	// names script-level lexical bindings have.
	Global  *Object
	Lexical map[uint32]bool
	// CharCodeAt is the context's cell for charCodeAt, which holds the
	// intrinsic, or is nil; Strings are strings by their handles: their
	// code units, and whether they are flat.
	CharCodeAt *ir.Value
	Strings    map[uint64]String
	// Word decodes an element's word, for OpElemCell; nil, only numbers'.
	Word func(uint64) ir.Value
	// Holders are the prototypes objects may have, by address (Object's
	// Proto, Holder's Object).
	Holders map[uintptr]*Object
	// WriteBarrier is the collector's write-barrier flag, which leaves a
	// store that changes a reference to Go while it is set.
	WriteBarrier bool
	// Address is an object's address, which OpSameObject compares; nil if
	// no object is any particular one.
	Address func(object int) uintptr
}

// String is a string as charCodeAt sees it.
type String struct {
	Units []uint16
	Flat  bool
	// Rope marks a string whose UTF-8 form is not made yet, which Go
	// flattens to compare it.
	Rope bool
}

// Object is an object as property operations see it: its shape, whether it
// is an ordinary object whose table may be searched, and its table: each
// entry's key, whether it is plain data and plain writable data, and its
// value.
type Object struct {
	Shape    uintptr
	Proto    uintptr // a prototype's address, in Heap.Holders, or 0
	Ordinary bool
	// HTMLDDA is Annex B's [[IsHTMLDDA]]: == null and == undefined hold.
	HTMLDDA  bool
	Keys     []uint32
	Data     []bool
	Writable []bool
	Uninit   []bool
	Props    []ir.Value
}

// holder is the object a property read of o finds its property in, and its
// index there, as Holders say, or -1: o itself, as property finds it, for a
// site without them or a shape none of its cases has.
func (h *Heap) holder(o *Object, v *Value) (*Object, int) {
	if v.Holders() == nil && v.Cases() == nil {
		return o, o.property(v)
	}
	var first HolderChain
	if v.Holders() != nil {
		first = *v.Holders()
	}
	cases := append([]PropertyCase{{Shape: uintptr(v.Const.Bits), Index: int32(v.Index), Holders: first.Holders, More: first.More}}, v.Cases()...)
	for _, c := range cases {
		if o.Shape != c.Shape {
			continue
		}
		for _, p := range append(c.Holders[:], c.More...) {
			if p.Object == 0 {
				break
			}
			next := h.Holders[p.Object]
			if o.Proto != p.Object || next == nil || next.Shape != p.Shape {
				return nil, -1
			}
			o = next
		}
		return o, int(c.Index)
	}
	// A shape none of the cases has: the receiver's own table, as for a
	// site without them (mir's scan).
	if i := o.property(v); i >= 0 {
		return o, i
	}
	return nil, -1
}

// isReference reports a value whose pointer word is not empty: an object or
// a string.
func isReference(x ir.Value) bool { return x.Kind == ir.Opaque || x.Kind == ir.String }

// maxScan is abi.MaxScan, and maxEqualUnits abi.MaxEqualUnits.
const (
	maxScan       = 8
	maxEqualUnits = 256
)

// property finds the property a property operation names in o, as the
// operation's semantics say (OpPropRead), or -1.
func (o *Object) property(v *Value) int {
	if v.Const.Bits != 0 && o.Shape == uintptr(v.Const.Bits) {
		return v.Index
	}
	if !o.Ordinary || len(o.Keys) > maxScan {
		return -1
	}
	for i, k := range o.Keys {
		if k != v.Key {
			continue
		}
		if !o.Data[i] || v.Op == OpPropWrite && !o.Writable[i] {
			return -1
		}
		return i
	}
	return -1
}

// EvaluateHeap is Evaluate with a heap.
func EvaluateHeap(f *Func, pc int, slots []ir.Value, heap Heap, pollEvery int) (ir.Exit, error) {
	f.Finish()
	arrays := heap.Arrays
	e, ok := f.EntryFor(pc)
	if !ok || len(slots) != f.Locals+f.StackSize {
		return ir.Exit{}, ir.ErrState
	}
	vals := make([]val, f.nextID)
	// An exit rematerializes boxes and constants from their operands, as
	// generated code does: a loop header's state names boxes of its phis,
	// which a poll exits to before the boxes themselves are computed.
	var slot func(v *Value) ir.Value
	slot = func(v *Value) ir.Value {
		switch v.Op {
		case OpConst:
			return v.Const
		case OpBoxF64, OpBoxBool:
			return apply(v.Op, v.Aux, vals[v.Args[0].ID], val{}).t
		}
		return vals[v.ID].t
	}
	origin := Origins(f)
	// The frame is the entry's until an exit writes it.
	traced := func(v *Value) bool {
		x := slot(v)
		if x.Kind != ir.Opaque && x.Kind != ir.String {
			return true
		}
		o := origin.At(v)
		if v.Shadow != nil {
			s := vals[v.Shadow.ID]
			if s.cell != nil {
				return *s.cell == x
			}
			o = int(int32(s.i))
		}
		return o >= 0 && o < len(slots) && slots[o] == x
	}
	exit := func(s *FrameState, kind ir.ExitKind) (ir.Exit, error) {
		if s.Inline != nil {
			return ir.Exit{}, fmt.Errorf("%w: an inlined callee's frame", ErrUnsupported)
		}
		// A slot with no value is a local dead there (ir.Program.Live): the
		// exit leaves it as it was.
		for i, v := range s.Slots {
			if v != nil && !traced(v) {
				return ir.Exit{}, fmt.Errorf("%w: slot %d's %v at pc %d", ErrOrigin, i, v, s.PC)
			}
		}
		for i, v := range s.Slots {
			if v != nil {
				slots[i] = slot(v)
			}
		}
		return ir.Exit{Kind: kind, State: ir.StateMap{PC: s.PC, Depth: s.Depth}}, nil
	}
	// keep is the context's keep cells (OpKeep).
	keep := make([]ir.Value, max(f.Keeps, 1))
	polls := 0
	blk, from := e.Block, -1
	for {
		// Phis take their arguments from the edge just taken, all at once.
		var phis []val
		for _, v := range blk.Values {
			if v.Op != OpPhi {
				break
			}
			phis = append(phis, vals[v.Args[from].ID])
		}
		for i, v := range blk.Values[:len(phis)] {
			vals[v.ID] = phis[i]
		}
		if from >= 0 && blk.Backedge[from] && pollEvery > 0 {
			polls++
			if polls%pollEvery == 0 {
				return exit(blk.Header, ir.BudgetExit)
			}
		}
		for _, v := range blk.Values[len(phis):] {
			var a, b val
			if len(v.Args) > 0 {
				a = vals[v.Args[0].ID]
			}
			if len(v.Args) > 1 {
				b = vals[v.Args[1].ID]
			}
			switch v.Op {
			case OpLoadSlot:
				vals[v.ID] = val{t: slots[v.Aux]}
			case OpConst:
				vals[v.ID] = val{t: v.Const}
			case OpConstF64:
				vals[v.ID] = val{f: math.Float64frombits(v.Const.Bits)}
			case OpConstSource:
				vals[v.ID] = val{i: uint32(int32(v.Aux))}
			case OpUnboxF64:
				if a.t.Kind != ir.Number {
					return exit(v.State, ir.ExitKind(v.Aux))
				}
				vals[v.ID] = val{f: math.Float64frombits(a.t.Bits)}
			case OpEqTagged:
				r, ok := eqTagged(a.t, b.t, v.Index == 1, heap.Strings, heap.Objects)
				if !ok {
					return exit(v.State, ir.ExitKind(v.Aux))
				}
				vals[v.ID] = val{b: r}
			case OpLooseNullish:
				t := a.t
				r := t.Kind == ir.Null || t.Kind == ir.Undefined
				if t.Kind == ir.Opaque && t.Bits < uint64(len(heap.Objects)) {
					r = heap.Objects[t.Bits].HTMLDDA
				}
				vals[v.ID] = val{b: r}
			case OpTruth:
				t, ok := truth(a.t)
				switch x := a.t; {
				case ok:
				case x.Kind == ir.Opaque:
					// True unless [[IsHTMLDDA]]; a string unless empty (one
					// the heap does not describe is).
					t, ok = x.Bits >= uint64(len(heap.Objects)) || !heap.Objects[x.Bits].HTMLDDA, true
				case x.Kind == ir.String:
					t, ok = len(heap.Strings[x.Bits].Units) > 0, true
				}
				if !ok {
					return exit(v.State, ir.ExitKind(v.Aux))
				}
				vals[v.ID] = val{b: t}
			case OpArrayOf, OpIndexedOf:
				// The model's arrays are Arrays.
				t := a.t
				if t.Kind != ir.Opaque || t.Bits >= uint64(len(arrays)) || arrays[t.Bits].NumberLimit == 0 {
					return exit(v.State, ir.ExitKind(v.Aux))
				}
				vals[v.ID] = val{p: int(t.Bits)}
			case OpElemKey:
				if _, ok := index(a.f); !ok {
					return exit(v.State, ir.ExitKind(v.Aux))
				}
			case OpElemRead, OpElemWrite:
				cell, ok := element(arrays[a.p], b.f)
				if !ok {
					return exit(v.State, ir.ExitKind(v.Aux))
				}
				if v.Op == OpElemRead {
					vals[v.ID] = val{f: math.Float64frombits(*cell)}
					break
				}
				bits := math.Float64bits(vals[v.Args[2].ID].f)
				if math.IsNaN(math.Float64frombits(bits)) {
					bits = canonicalNaN
				}
				*cell = bits
			case OpElemCell:
				view := arrays[a.p]
				i, ok := index(b.f)
				if !ok || i >= view.DenseLength || view.Data == nil {
					return exit(v.State, ir.ExitKind(v.Aux))
				}
				word := *(*uint64)(unsafe.Add(view.Data, uintptr(i)*16))
				t := ir.Value{Kind: ir.Number, Bits: word}
				switch {
				case heap.Word != nil:
					t = heap.Word(word)
				case word >= view.NumberLimit:
					return exit(v.State, ir.ExitKind(v.Aux))
				}
				if t.Kind == ir.Uninitialized {
					// A hole.
					return exit(v.State, ir.ExitKind(v.Aux))
				}
				vals[v.ID] = val{cell: &t}
			case OpLength:
				t := a.t
				switch {
				case t.Kind == ir.Opaque && t.Bits < uint64(len(arrays)) && arrays[t.Bits].NumberLimit != 0:
					vals[v.ID] = val{f: float64(arrays[t.Bits].Length)}
				case t.Kind == ir.String && heap.Strings != nil:
					s, ok := heap.Strings[t.Bits]
					if !ok {
						return exit(v.State, ir.ExitKind(v.Aux))
					}
					vals[v.ID] = val{f: float64(len(s.Units))}
				default:
					return exit(v.State, ir.ExitKind(v.Aux))
				}
			case OpMathCall:
				// The VM's functions are not the model's.
				return ir.Exit{}, fmt.Errorf("%w: a Math function", ErrUnsupported)
			case OpPowF64:
				r, ok := ExactPow(a.f, b.f)
				if !ok {
					return exit(v.State, ir.ExitKind(v.Aux))
				}
				vals[v.ID] = val{f: r}
			case OpModF64:
				if !integer64(a.f) || !integer64(b.f) || b.f == 0 {
					return exit(v.State, ir.ExitKind(v.Aux))
				}
				vals[v.ID] = val{f: math.Mod(a.f, b.f)}
			case OpObjectOf:
				t := a.t
				if t.Kind != ir.Opaque || t.Bits >= uint64(len(heap.Objects)) {
					return exit(v.State, ir.ExitKind(v.Aux))
				}
				vals[v.ID] = val{p: int(t.Bits)}
			case OpPropRead:
				o, i := heap.holder(&heap.Objects[a.p], v)
				if i < 0 || i >= len(o.Props) || o.Props[i].Kind != ir.Number {
					return exit(v.State, ir.ExitKind(v.Aux))
				}
				vals[v.ID] = val{f: math.Float64frombits(o.Props[i].Bits)}
			case OpPropWrite:
				if v.Global {
					// The heap model has no global object: Go assigns.
					return exit(v.State, ir.ExitKind(v.Aux))
				}
				if v.Add() != nil || len(v.Adds()) != 0 || len(v.Cases()) != 0 {
					// The heap model's objects have no shapes to add along,
					// nor to tell a write's cases by.
					return ir.Exit{}, fmt.Errorf("%w: a property added", ErrUnsupported)
				}
				// Any value, unless it changes a reference while the
				// collector marks, or the cell is one a live reference was
				// loaded from (storeChecks).
				o := &heap.Objects[a.p]
				i := o.property(v)
				if i < 0 || i >= len(o.Props) {
					return exit(v.State, ir.ExitKind(v.Aux))
				}
				x := b.t
				if heap.WriteBarrier && (isReference(x) || isReference(o.Props[i])) {
					return exit(v.State, ir.ExitKind(v.Aux))
				}
				for _, s := range v.Args[2:] {
					if vals[s.ID].cell == &o.Props[i] && isReference(o.Props[i]) {
						return exit(v.State, ir.ExitKind(v.Aux))
					}
				}
				if x.Kind == ir.Number && math.IsNaN(math.Float64frombits(x.Bits)) {
					x.Bits = canonicalNaN
				}
				o.Props[i] = x
			case OpSameObject:
				if heap.Address == nil || heap.Address(a.p) != uintptr(v.Const.Bits) {
					return exit(v.State, ir.ExitKind(v.Aux))
				}
			case OpPropCell:
				o, i := heap.holder(&heap.Objects[a.p], v)
				if i < 0 || i >= len(o.Props) {
					return exit(v.State, ir.ExitKind(v.Aux))
				}
				vals[v.ID] = val{cell: &o.Props[i]}
			case OpGlobalCell:
				g, i := heap.Global, v.Index
				if g == nil || heap.Lexical[v.Key] || i < 0 || i >= len(g.Props) ||
					g.Keys[i] != v.Key || !g.Data[i] || g.Uninit[i] {
					return exit(v.State, ir.ExitKind(v.Aux))
				}
				vals[v.ID] = val{cell: &g.Props[i]}
			case OpConstCell, OpUpvalueCell, OpLocalCell, OpLexCell, OpBindingCell, OpStringProp:
				// The heap model has no constants' or captured bindings'
				// cells.
				return ir.Exit{}, fmt.Errorf("%w: a constant's or binding's cell", ErrUnsupported)
			case OpStringMethod:
				if a.t.Kind != ir.String || heap.CharCodeAt == nil {
					return exit(v.State, ir.ExitKind(v.Aux))
				}
				vals[v.ID] = val{cell: heap.CharCodeAt}
			case OpStringCode:
				s, ok := heap.Strings[b.t.Bits]
				i, isIndex := index(vals[v.Args[2].ID].f)
				if heap.CharCodeAt == nil || a.t != *heap.CharCodeAt || b.t.Kind != ir.String || !ok || !s.Flat ||
					!isIndex || i >= uint64(len(s.Units)) {
					return exit(v.State, ir.ExitKind(v.Aux))
				}
				vals[v.ID] = val{f: float64(s.Units[i])}
			case OpLoadCell:
				vals[v.ID] = val{t: *a.cell}
			case OpKeep:
				// The value's source while the collector marks, else a copy.
				if heap.WriteBarrier {
					x := v.Args[0]
					switch o := origin.At(x); {
					case x.Shadow != nil:
						vals[v.ID] = vals[x.Shadow.ID]
					case o >= 0:
						vals[v.ID] = val{i: uint32(int32(o))}
					default:
						vals[v.ID] = val{i: uint32(0xFFFFFFFF)}
					}
					break
				}
				keep[v.Index] = a.t
				vals[v.ID] = val{cell: &keep[v.Index]}
			case OpKept:
				vals[v.ID] = b
			case OpKeepRef:
				// The evaluator keeps whole values (OpKeep).
				vals[v.ID] = val{}
			case OpCall, OpInstanceOf, OpTypeIs:
				// The evaluator calls nothing, nor walks prototypes: Go
				// makes the operation.
				return exit(v.State, ir.ExitKind(v.Aux))
			case OpCallCell:
				vals[v.ID] = val{}
			case OpCheckInit:
				if a.t.Kind == ir.Uninitialized {
					return exit(v.State, ir.ExitKind(v.Aux))
				}
			case OpCheckTrue:
				if a.t != ir.Bool(true) {
					return exit(v.State, ir.ExitKind(v.Aux))
				}
			case OpTailRoom, OpTailSelf:
				// The model's stack is shallow: the call is made.
			case OpSameSymbol:
				return ir.Exit{}, fmt.Errorf("%w: a symbol", ErrUnsupported)
			case OpIterClosed:
				return ir.Exit{}, fmt.Errorf("%w: a cursor", ErrUnsupported)
			default:
				vals[v.ID] = apply(v.Op, v.Aux, a, b)
			}
		}
		switch blk.Kind {
		case BlockReturn:
			if !traced(blk.Control) {
				return ir.Exit{}, fmt.Errorf("%w: returned %v", ErrOrigin, blk.Control)
			}
			return ir.Exit{Kind: ir.Returned, Value: vals[blk.Control.ID].t}, nil
		case BlockExit:
			return exit(blk.State, blk.ExitKind)
		}
		next := blk.Succs[0]
		if blk.Kind == BlockIf && !vals[blk.Control.ID].b {
			next = blk.Succs[1]
		}
		for i, p := range next.Preds {
			if p == blk {
				from = i
				break
			}
		}
		blk = next
	}
}
