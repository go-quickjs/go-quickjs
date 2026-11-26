package ssa

import (
	"fmt"
	"math"
	"slices"

	"github.com/go-quickjs/go-quickjs/internal/jit/abi"
	"github.com/go-quickjs/go-quickjs/internal/jit/ir"
)

// Typed code's raw instructions (ir's raw.go), built with no check: the
// kinds of their operands are proven. A raw slot holds what every slot
// does in the builder, a value -- the box of its float64, or of an
// int32's conversion -- so that the passes that unbox numbers and compute
// in int32s (unboxPhis, intPhis, truncInts) take a raw slot's phis as
// they take a number's, and what reads it unboxes the box the instruction
// that wrote it made: a raw slot's value is never anything else, so no
// unbox of one is left to check (the build's check, rawChecked).

// HeapFeedback answers the address of the abi.HeapCell of the heap typed
// code's heap accesses (ir's RawLoad, RawStore) read and write, or 0 for
// none.
type HeapFeedback interface {
	Heap() uintptr
}

// RawMathFeedback answers a raw Math function's computation (ir's
// RawFMath): its Key, the typed operation's function, as the intrinsic of
// a call of that function of the realm's Math would be (Intrinsic's Op,
// Go and Fn).
type RawMathFeedback interface {
	RawMath(fn int) (Intrinsic, bool)
}

// rawInstruction builds the raw instruction in, at pc in blk, a Branch's
// compare included; number is instruction's unboxing of an operand.
func (b *builder) rawInstruction(blk *Block, pc int, in ir.Instruction, operand func(ir.Operand) *Value,
	number func(ir.Operand, ir.ExitKind) *Value, gd *guarder) {
	f := b.f
	val := func(op Op, t Type, args ...*Value) *Value { return f.newValue(blk, op, t, args...) }
	fl := func(o ir.Operand) *Value { return number(o, ir.GuardExit) }
	i32 := func(o ir.Operand) *Value {
		if o.Slot < 0 {
			v := val(OpConstI32, Int32)
			v.Const = ir.Value{Bits: uint64(uint32(int32(math.Float64frombits(o.Literal.Bits))))}
			return v
		}
		if v := operand(o); v.Op == OpBoxF64 && v.Args[0].Op == OpI32ToF64 {
			return v.Args[0].Args[0]
		}
		return val(OpToInt32, Int32, fl(o))
	}
	setF := func(x *Value) { b.assign(in.Dest, blk, val(OpBoxF64, Tagged, x)) }
	setI := func(x *Value) { setF(val(OpI32ToF64, Float64, x)) }
	f64 := func(x *Value) *Value { return val(OpI32ToF64, Float64, x) }
	u64 := func(x *Value) *Value { return val(OpU32ToF64, Float64, x) }
	cmpI := func(o ir.Operator, unsigned bool) *Value {
		c := val(OpCmpI32, Bool, i32(in.Left), i32(in.Right))
		c.Aux = int(o)
		if unsigned {
			c.Aux |= CmpUnsigned
		}
		return c
	}
	cmpF := func(o ir.Operator) *Value {
		c := val(OpCmpF64, Bool, fl(in.Left), fl(in.Right))
		c.Aux = int(o)
		return c
	}
	switch in.Raw {
	case ir.RawBrI, ir.RawBrU, ir.RawBrF:
		var c *Value
		if in.Raw == ir.RawBrF {
			c = cmpF(in.Operator)
		} else {
			c = cmpI(in.Operator, in.Raw == ir.RawBrU)
		}
		blk.Control = c
		c.Uses++
	case ir.RawIMov, ir.RawFMov:
		b.assign(in.Dest, blk, operand(in.Left))
	case ir.RawIConst:
		setI(i32(in.Left))
	case ir.RawFConst:
		v := val(OpConstF64, Float64)
		v.Const = in.Left.Literal
		setF(v)
	case ir.RawIAdd:
		setI(val(OpAddI32, Int32, i32(in.Left), i32(in.Right)))
	case ir.RawISub:
		setI(val(OpSubI32, Int32, i32(in.Left), i32(in.Right)))
	case ir.RawIMul:
		setI(val(OpMulI32, Int32, i32(in.Left), i32(in.Right)))
	case ir.RawIDivS:
		setI(val(OpDivI32, Int32, i32(in.Left), i32(in.Right)))
	case ir.RawIDivU:
		setI(val(OpDivU32, Int32, i32(in.Left), i32(in.Right)))
	case ir.RawIRemS:
		setI(val(OpRemI32, Int32, i32(in.Left), i32(in.Right)))
	case ir.RawIRemU:
		setI(val(OpRemU32, Int32, i32(in.Left), i32(in.Right)))
	case ir.RawIAnd:
		setI(val(OpAndI32, Int32, i32(in.Left), i32(in.Right)))
	case ir.RawIOr:
		setI(val(OpOrI32, Int32, i32(in.Left), i32(in.Right)))
	case ir.RawIXor:
		setI(val(OpXorI32, Int32, i32(in.Left), i32(in.Right)))
	case ir.RawIShl:
		setI(val(OpShlI32, Int32, i32(in.Left), i32(in.Right)))
	case ir.RawIShr:
		setI(val(OpSarI32, Int32, i32(in.Left), i32(in.Right)))
	case ir.RawIUShr:
		setI(val(OpShrU32, Int32, i32(in.Left), i32(in.Right)))
	case ir.RawIMinS:
		setI(val(OpToInt32, Int32, val(OpMinF64, Float64, f64(i32(in.Left)), f64(i32(in.Right)))))
	case ir.RawIMaxS:
		setI(val(OpToInt32, Int32, val(OpMaxF64, Float64, f64(i32(in.Left)), f64(i32(in.Right)))))
	case ir.RawINeg:
		zero := val(OpConstI32, Int32)
		setI(val(OpSubI32, Int32, zero, i32(in.Left)))
	case ir.RawINot:
		setI(val(OpNotI32, Int32, i32(in.Left)))
	case ir.RawILNot:
		zero := val(OpConstI32, Int32)
		c := val(OpCmpI32, Bool, i32(in.Left), zero)
		c.Aux = int(ir.Eq)
		setI(val(OpBoolToI32, Int32, c))
	case ir.RawIClz:
		setI(val(OpClz32, Int32, i32(in.Left)))
	case ir.RawIAbs:
		setI(val(OpToInt32, Int32, val(OpAbsF64, Float64, f64(i32(in.Left)))))
	case ir.RawICmpS, ir.RawICmpU:
		setI(val(OpBoolToI32, Int32, cmpI(in.Operator, in.Raw == ir.RawICmpU)))
	case ir.RawFAdd:
		setF(val(OpAddF64, Float64, fl(in.Left), fl(in.Right)))
	case ir.RawFSub:
		setF(val(OpSubF64, Float64, fl(in.Left), fl(in.Right)))
	case ir.RawFMul:
		setF(val(OpMulF64, Float64, fl(in.Left), fl(in.Right)))
	case ir.RawFDiv:
		setF(val(OpDivF64, Float64, fl(in.Left), fl(in.Right)))
	case ir.RawFMod:
		setF(gd.guard(OpModF64, Float64, ir.HostExit, fl(in.Left), fl(in.Right)))
	case ir.RawFMin:
		setF(val(OpMinF64, Float64, fl(in.Left), fl(in.Right)))
	case ir.RawFMax:
		setF(val(OpMaxF64, Float64, fl(in.Left), fl(in.Right)))
	case ir.RawFNeg:
		setF(val(OpNegF64, Float64, fl(in.Left)))
	case ir.RawFAbs:
		setF(val(OpAbsF64, Float64, fl(in.Left)))
	case ir.RawFSqrt:
		setF(val(OpSqrtF64, Float64, fl(in.Left)))
	case ir.RawFRound:
		setF(val(OpFroundF64, Float64, fl(in.Left)))
	case ir.RawFFloor:
		setF(val(OpFloorF64, Float64, fl(in.Left)))
	case ir.RawFCeil:
		setF(val(OpCeilF64, Float64, fl(in.Left)))
	case ir.RawFMath:
		mf, _ := b.fb.(RawMathFeedback)
		var k Intrinsic
		ok := mf != nil
		if ok {
			k, ok = mf.RawMath(pc)
		}
		switch {
		case ok && k.Op == OpPowF64:
			setF(gd.guard(OpPowF64, Float64, ir.HostExit, fl(in.Left), fl(in.Right)))
		case ok && k.Op == OpMathCall && b.goCalls():
			args := []*Value{fl(in.Left)}
			if k.Go != GoMath {
				args = append(args, fl(in.Right))
			}
			r := val(OpMathCall, Float64, args...)
			r.Index, r.Aux = int(k.Go), k.Fn
			setF(r)
		default:
			panic(rawRefusal(fmt.Sprintf("raw Math function %d without its computation", in.Key)))
		}
	case ir.RawFCmp:
		setI(val(OpBoolToI32, Int32, cmpF(in.Operator)))
	case ir.RawI2FS:
		setF(f64(i32(in.Left)))
	case ir.RawI2FU:
		setF(u64(i32(in.Left)))
	case ir.RawF2I:
		setI(val(OpToInt32, Int32, fl(in.Left)))
	case ir.RawConvI, ir.RawConvF:
		// A value's ToInt32 or ToNumber: a number's natively, an
		// oddball's too, anything else's by Go at a host exit, which runs
		// the instruction whole -- valueOf, which may throw -- and goes on
		// after it; a value proven a number (ir.RawNumber), its number.
		x := operand(in.Left)
		var n *Value
		if x.Op == OpBoxF64 {
			n = x.Args[0]
		} else if in.Key == ir.RawNumber {
			// A call's result of a function of the module, a number of its
			// declared type by validation: its number word, unchecked.
			n = val(OpNumberF64, Float64, x)
		} else {
			n = gd.guard(OpOddballF64, Float64, ir.HostExit, x)
			if b.goCalls() {
				n.Index = 1
			}
		}
		if in.Raw == ir.RawConvI {
			t := val(OpToInt32, Int32, n)
			if in.Key == ir.RawNumber {
				// A signed int's: what validation proves of a call's result
				// the call coerces with | 0, the function's declared type.
				t.Aux = ToInt32Exact
			}
			setI(t)
		} else {
			setF(n)
		}
	case ir.RawLoad, ir.RawStore:
		hf, _ := b.fb.(HeapFeedback)
		var heap uintptr
		if hf != nil {
			heap = hf.Heap()
		}
		if heap == 0 {
			panic(rawRefusal("a heap access without a heap"))
		}
		addr := i32(in.Left)
		if in.Right.Slot >= 0 || in.Right.Literal.Bits != 0 {
			addr = val(OpAddI32, Int32, addr, i32(in.Right))
		}
		float := in.Key == abi.ElemFloat32 || in.Key == abi.ElemFloat64
		if in.Raw == ir.RawStore {
			var x *Value
			if float {
				x = fl(in.Third)
			} else {
				x = i32(in.Third)
			}
			base, n := b.heapOf(blk, heap)
			st := val(OpHeapStore, None, addr, x, base, n)
			st.Aux, st.Const = int(in.Key), ir.Value{Bits: uint64(heap)}
			break
		}
		t := Int32
		if float {
			t = Float64
		}
		base, n := b.heapOf(blk, heap)
		ld := val(OpHeapLoad, t, addr, base, n)
		ld.Aux, ld.Const = int(in.Key), ir.Value{Bits: uint64(heap)}
		if float {
			setF(ld)
		} else {
			setI(ld)
		}
	case ir.RawCellGetI, ir.RawCellGetF:
		n := val(OpCellNumLoad, Float64, b.rawCell(blk, int(in.Key)))
		if in.Raw == ir.RawCellGetI {
			setI(val(OpToInt32, Int32, n))
		} else {
			setF(n)
		}
	case ir.RawCellSetI, ir.RawCellSetF:
		var x *Value
		if in.Raw == ir.RawCellSetI {
			x = f64(i32(in.Left))
		} else {
			x = fl(in.Left)
		}
		val(OpCellNumStore, None, b.rawCell(blk, int(in.Key)), val(OpBoxF64, Tagged, x))
	case ir.RawIBox:
		setF(f64(i32(in.Left)))
	case ir.RawUBox:
		setF(u64(i32(in.Left)))
	case ir.RawFBox:
		setF(fl(in.Left))
	default:
		panic(rawRefusal(fmt.Sprintf("raw operation %d", in.Raw)))
	}
}

// rawCell is the cell of the module variable that is captured binding k
// of the frame being translated: the function's own, which its context
// has, or an inlined callee's, read from its closure's cells
// (InlineSite's Upvalues), as an inlined callee's captured binding is.
func (b *builder) rawCell(blk *Block, k int) *Value {
	if fr := b.cur; fr != b.root {
		cell := b.f.newValue(blk, OpUpvalueCell, Source)
		cell.Index, cell.Aux, cell.Const = k, 2, ir.Value{Bits: uint64(fr.site.Upvalues)}
		return cell
	}
	return b.cellOf(blk, b.root.p.UpvalueBase+k)
}

// The heaps' bases and lengths (OpHeapBase, OpHeapLen) are variables of
// the builder's SSA construction, as a slot is, of slots of their own
// below 0 (heapSlot): read where a block has them, loaded from the heap's
// cell where they are stale -- at an entry, which loads nothing of them,
// and after a call (reloadAfterCall), which may run script that detaches
// or moves the heap's buffer -- and at a join whose predecessors have
// different ones (heapMerge). Nothing else changes a cell (the VM's
// bufferChanged): so a loop that calls nothing reads its heap once before
// it, its header's phi trivial, and a block of accesses once, where each
// access had read the cell twice.

// heapOf is the base and the length of the heap whose abi.HeapCell is at
// cell, as blk has them where the instruction being built reads them.
func (b *builder) heapOf(blk *Block, cell uintptr) (base, n *Value) {
	i := slices.Index(b.heaps, cell)
	if i < 0 {
		i = len(b.heaps)
		b.heaps = append(b.heaps, cell)
		// Stale where a call was before the heap was first read: its kill
		// was recorded for the heaps then known alone. The calls are in
		// blocks built already, or earlier in this one, so a block's
		// value of it is stale at its end.
		for _, k := range b.heapKills {
			b.found(heapSlot(i, false), k, b.heapStale(heapSlot(i, false)))
			b.found(heapSlot(i, true), k, b.heapStale(heapSlot(i, true)))
		}
	}
	return b.heapRead(heapSlot(i, false), blk), b.heapRead(heapSlot(i, true), blk)
}

// heapSlot is the builder's slot of heap i's base, or of its length.
func heapSlot(i int, length bool) int {
	s := -2 - 2*i
	if length {
		s--
	}
	return s
}

// heapRead is heap slot's value in blk, loaded from its cell there where
// it is stale.
func (b *builder) heapRead(slot int, blk *Block) *Value {
	v := b.readVar(slot, blk)
	if v.Op == OpUpvalueStale {
		v = b.heapLoad(blk, slot)
		b.found(slot, blk, v)
	}
	return v
}

// heapLoad loads heap slot's base or length from its cell, at the end of
// blk.
func (b *builder) heapLoad(blk *Block, slot int) *Value {
	s := -2 - slot
	op := OpHeapBase
	if s%2 == 1 {
		op = OpHeapLen
	}
	v := b.f.newValue(blk, op, Ptr)
	v.Const = ir.Value{Bits: uint64(b.heaps[s/2])}
	return v
}

// heapMerge is heap slot's value in blk, of several predecessors whose
// values are args: the one they all have -- or, for a loop header's phi,
// all but the phi itself -- or a phi of them; or, where a call made one
// stale, a load of the cell at blk's start, the cell being what every
// one of them is (a call is a definition): a phi of them all had been
// moved at every edge after a call, slot to slot where they spilled, in a
// large function. phi is the phi a read made while the predecessors were
// read -- one round a loop -- or nil, or mark: made a load in place, its
// users the load's, where one is made.
func (b *builder) heapMerge(blk *Block, slot int, phi, mark *Value, args []*Value) *Value {
	// One stale since an entry, with no call since, loaded at the
	// predecessor's end: the loop an entry goes into -- by on-stack
	// replacement, or from the start -- keeps what it loaded there, where a
	// load at the join had been one every turn.
	for i, a := range args {
		if p := blk.Preds[i]; a.Op == OpUpvalueStale && a.Aux == heapAtEntry {
			args[i] = b.heapLoad(p, slot)
			b.found(slot, p, args[i])
		}
	}
	var one *Value
	same, stale := true, false
	for _, a := range args {
		if a == phi {
			continue
		}
		if one == nil {
			one = a
		}
		same = same && a == one
		stale = stale || a.Op == OpUpvalueStale
	}
	if !stale && one != nil && (!same || phi != nil && phi != mark) {
		// One value, or several none of them a call's: a phi, trivial
		// where the others are the phi itself (Optimize takes it away).
		if same && (phi == nil || phi == mark) {
			return one
		}
		if phi == nil || phi == mark {
			phi = b.newPhiID(blk, mark.ID)
		}
		phi.Type = Ptr
		phi.Args = b.f.refsOf(len(args))[:0]
		for _, a := range args {
			a.Uses++
			phi.Args = append(phi.Args, a)
		}
		return phi
	}
	s := -2 - slot
	op := OpHeapBase
	if s%2 == 1 {
		op = OpHeapLen
	}
	var v *Value
	if phi != nil && phi != mark {
		// Made a load in place, after the block's phis.
		v = phi
		v.Op, v.Args, v.Type = op, nil, Ptr
		vals := blk.Values
		i := slices.Index(vals, v)
		j := i
		for j+1 < len(vals) && vals[j+1].Op == OpPhi {
			vals[j], vals[j+1] = vals[j+1], vals[j]
			j++
		}
	} else {
		v = b.f.alloc(Value{Op: op, Type: Ptr, Block: blk})
		if mark != nil {
			b.f.nextID--
			v.ID = mark.ID
		}
		vals := blk.Values
		j := 0
		for j < len(vals) && vals[j].Op == OpPhi {
			j++
		}
		blk.Values = b.f.appendValue(blk.Values, nil)
		copy(blk.Values[j+1:], blk.Values[j:])
		blk.Values[j] = v
	}
	v.Const = ir.Value{Bits: uint64(b.heaps[s/2])}
	return v
}

// heapStale is heap slot's stale marker after a call, an OpUpvalueStale
// of the slot; heapEntryStale its marker at an entry, of Aux heapAtEntry,
// which a join loads where its predecessor ends (heapMerge).
func (b *builder) heapStale(slot int) *Value { return b.heapMark(slot, 0) }

func (b *builder) heapEntryStale(slot int) *Value { return b.heapMark(slot, heapAtEntry) }

// heapAtEntry is the Aux of a heap's marker at an entry.
const heapAtEntry = 1

func (b *builder) heapMark(slot, aux int) *Value {
	s := 2*(-2-slot) + aux
	for len(b.heapMarks) <= s {
		b.heapMarks = append(b.heapMarks, nil)
	}
	if b.heapMarks[s] == nil {
		b.heapMarks[s] = b.f.alloc(Value{Op: OpUpvalueStale, Type: Ptr, Index: slot, Aux: aux})
	}
	return b.heapMarks[s]
}

// staleHeaps makes every heap stale in blk, after a call there: those not
// read yet once they are (heapOf).
func (b *builder) staleHeaps(blk *Block) {
	if n := len(b.heapKills); n == 0 || b.heapKills[n-1] != blk {
		b.heapKills = append(b.heapKills, blk)
	}
	for i := range b.heaps {
		b.found(heapSlot(i, false), blk, b.heapStale(heapSlot(i, false)))
		b.found(heapSlot(i, true), blk, b.heapStale(heapSlot(i, true)))
	}
}

// tableRead records the function in, a function table's element, read
// (ArrayRead's Dest) at the index raw slot in.Key-1 holds -- boxed for the
// read by RawIBox, so its box's int32: a call of it finds its function by
// that index (tableCall).
func (b *builder) tableRead(blk *Block, in ir.Instruction) {
	slot := int(in.Key) - 1
	if b.cur != b.root {
		slot += b.cur.base
	}
	k := b.read(slot, blk)
	if k.Op != OpBoxF64 || k.Args[0].Op != OpI32ToF64 {
		return
	}
	if b.tableIdx == nil {
		b.tableIdx = map[*Value]*Value{}
	}
	b.tableIdx[b.read(in.Dest, blk)] = k.Args[0].Args[0]
}

// tableCall is calls, a call through a function table (CallSite's Table),
// of function fn: the entry of the function it calls found by the index
// fn was read at, for the call's checks (OpTableCallee) -- or, where the
// build does not know that index, calls without the table, the call
// checking its functions one by one.
func (b *builder) tableCall(blk *Block, calls []*CallSite, fn *Value) []*CallSite {
	idx := b.tableIdx[fn]
	if idx == nil {
		c := *calls[0]
		c.Table, c.TableLen = 0, 0
		return append([]*CallSite{&c}, calls[1:]...)
	}
	v := b.f.newValue(blk, OpTableCallee, None, idx)
	v.Const = ir.Value{Bits: uint64(calls[0].Table)}
	return calls
}

// typedInline reports whether site's callee may be inlined in the frame
// being translated, as far as typed code (ir.Program's Kinds) goes:
// ordinary code as anywhere; typed code in typed code, by the ordinary
// rules, where what it reads of the VM -- the heap, a Math function's
// computation, its module variables' cells -- its feedback answers: the
// build of a typed function refuses what its feedback does not answer,
// which no callee's may make it do. An exit inside it writes its frame's
// raw slots raw, by their kinds (frameKinds), and the VM makes its frame
// of its typed code, which the typed interpreter goes on with.
func (b *builder) typedInline(site InlineSite) bool {
	p := site.Program
	if p.Kinds == nil {
		return true
	}
	if b.f.Kinds == nil || b.p.Kinds == nil || site.Construct || site.Method || site.Restart || site.Forward || site.Arguments ||
		site.ReadsArguments || site.Captured != nil || site.LexThis || !site.Window {
		return false
	}
	fb := site.Feedback
	gf, _ := fb.(GoFeedback)
	for pc, in := range p.Code {
		if !reachable(p, pc) || in.Op != ir.Raw {
			continue
		}
		switch in.Raw {
		case ir.RawFMath:
			mf, ok := fb.(RawMathFeedback)
			if !ok {
				return false
			}
			if k, ok := mf.RawMath(pc); !ok || k.Op != OpPowF64 && (k.Op != OpMathCall || gf == nil || !gf.GoCalls()) {
				return false
			}
		case ir.RawLoad, ir.RawStore:
			if hf, ok := fb.(HeapFeedback); !ok || hf.Heap() == 0 {
				return false
			}
		case ir.RawCellGetI, ir.RawCellGetF, ir.RawCellSetI, ir.RawCellSetF:
			if site.Upvalues == 0 {
				return false
			}
		}
	}
	return true
}

// frameKinds gives the function's slots (Func's Kinds) the kinds of fr's,
// an inlined typed callee's, laid past the caller's at its base: an exit
// inside it writes its raw slots raw (abi.ExitFrame's Kinds), and so do
// the records of a native call it makes, for the VM to go on with its
// frame in the typed interpreter.
func (b *builder) frameKinds(fr *frame) {
	k := fr.p.Kinds
	if k == nil {
		return
	}
	f := b.f
	if end := fr.base + len(k); end > len(f.Kinds) || !b.kindsOwn {
		// The root program's, which other builds share, copied first.
		kinds := make([]uint8, max(end, len(f.Kinds)))
		copy(kinds, f.Kinds)
		f.Kinds, b.kindsOwn = kinds, true
	}
	copy(f.Kinds[fr.base:], k)
}

// entryLoad is slot's value as an entry in blk loads it from the frame: a
// value, or, for typed code's raw slot (ir.Program's Kinds), the box of
// the int32 or the float64 its number word holds. An entry is the root's,
// and so are the slots it loads, whatever frame is being translated when
// one is read (readVar).
func (b *builder) entryLoad(blk *Block, slot int) *Value {
	f := b.f
	kind := ir.SlotValue
	if k := b.root.p.Kinds; slot < len(k) {
		kind = k[slot]
	}
	switch kind {
	case ir.SlotI32:
		v := f.newValue(blk, OpLoadSlot, Int32)
		v.Aux = slot
		return f.newValue(blk, OpBoxF64, Tagged, f.newValue(blk, OpI32ToF64, Float64, v))
	case ir.SlotF64:
		v := f.newValue(blk, OpLoadSlot, Float64)
		v.Aux = slot
		return f.newValue(blk, OpBoxF64, Tagged, v)
	}
	v := f.newValue(blk, OpLoadSlot, Tagged)
	v.Aux = slot
	return v
}

// rawRefusal is a typed build's refusal of what its feedback does not
// answer: a Math function's computation, a heap. The function runs in the
// typed interpreter.
type rawRefusal string

func (e rawRefusal) Error() string { return "ssa: " + string(e) }
