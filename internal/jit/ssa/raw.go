package ssa

import (
	"fmt"
	"math"

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
	number func(ir.Operand, ir.ExitKind) *Value, guard func(Op, Type, ir.ExitKind, ...*Value) *Value) {
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
		setF(guard(OpModF64, Float64, ir.HostExit, fl(in.Left), fl(in.Right)))
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
			setF(guard(OpPowF64, Float64, ir.HostExit, fl(in.Left), fl(in.Right)))
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
		// after it.
		x := operand(in.Left)
		var n *Value
		if x.Op == OpBoxF64 {
			n = x.Args[0]
		} else {
			n = guard(OpOddballF64, Float64, ir.HostExit, x)
			if b.goCalls() {
				n.Index = 1
			}
		}
		if in.Raw == ir.RawConvI {
			setI(val(OpToInt32, Int32, n))
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
			st := val(OpHeapStore, None, addr, x)
			st.Aux, st.Const = int(in.Key), ir.Value{Bits: uint64(heap)}
			break
		}
		t := Int32
		if float {
			t = Float64
		}
		ld := val(OpHeapLoad, t, addr)
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

// typedInline reports whether site's callee may be inlined in the frame
// being translated, as far as typed code (ir.Program's Kinds) goes:
// ordinary code as anywhere; typed code only in typed code, and only
// where nothing inside it exits -- no call, no loop and so no poll, no
// host exit or guard that stays after the build's passes -- so that no
// frame state names its frame, whose raw slots no exit writes. Its
// instructions are a raw operation's that native code makes alone, a
// copy, a forward jump or a raw compare's branch, a return; a Math
// function Go computes from native code (OpMathCall), which leaves
// nothing to Go, among them, but not % of doubles or pow, which fail to
// Go where they are not exact. A parameter's conversion (ir.RawConvI,
// RawConvF) converts a number natively: typed code's call boxes each
// argument (the typed compiler's call), so where the parameter's slot is
// written by nothing in the callee, its value is the caller's box, and
// the conversion its number. What it reads of the VM -- the heap, a Math
// function's computation, its module variables' cells -- its feedback
// answers, or it is not inlined: the build of a typed function refuses
// what its feedback does not answer.
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
	written := func(slot int) bool {
		for pc := range p.Code {
			if !reachable(p, pc) {
				continue
			}
			if a, c := p.Code[pc].Writes(); a == slot || c == slot {
				return true
			}
		}
		return false
	}
	for pc, in := range p.Code {
		if !reachable(p, pc) {
			continue
		}
		if in.Check {
			return false
		}
		switch in.Op {
		case ir.Nop, ir.Copy, ir.Return:
			continue
		case ir.Jump:
			if in.Target > pc {
				continue
			}
			return false
		case ir.Branch:
			if in.Raw != ir.RawNone && in.Target > pc {
				continue
			}
			return false
		case ir.Raw:
		default:
			return false
		}
		switch in.Raw {
		case ir.RawFMod:
			return false
		case ir.RawFMath:
			mf, ok := fb.(RawMathFeedback)
			if !ok {
				return false
			}
			if k, ok := mf.RawMath(pc); !ok || k.Op != OpMathCall || gf == nil || !gf.GoCalls() {
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
		case ir.RawConvI, ir.RawConvF:
			k := in.Left.Slot - p.ParamBase
			if in.Left.Slot < 0 || k < 0 || k >= p.Params || k >= site.Argc || written(in.Left.Slot) {
				return false
			}
		}
	}
	return true
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
