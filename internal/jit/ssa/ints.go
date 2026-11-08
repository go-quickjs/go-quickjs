package ssa

import (
	"math"

	"github.com/go-quickjs/go-quickjs/internal/jit/ir"
)

// Integers as int32s where their uses truncate them: asm.js's
// (a + b) | 0, as V8's representation selection computes it in Word32.
//
// The slot IR computes every number as a double, and a bitwise operator
// takes its operands' ToInt32 (OpToInt32) and gives back a double
// (OpI32ToF64): (a + b) | 0 of two int32s converted both to doubles, added
// them, and converted the sum back, its range checked -- zlib's inflate
// spent a third of its native time converting. Two rewrites keep such
// values int32s:
//
//   - a constant's ToInt32 is the constant (simplify's, OpConstI32), which
//     mir makes an instruction's immediate: a shift's count, a mask;
//   - the ToInt32 of a sum, a difference, a negation or a product by a
//     constant of such integers, exact as a double, is the int32
//     operation, which wraps as ToInt32 of the exact result does
//     (truncInts).
//
// Phis stay doubles: an int32 phi holds a general register, of which mir
// has half as many as SSE registers, and Emscripten's functions, dozens of
// integers live at once, spilled half as much again.

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
			if v.Op != OpToInt32 || !truncatedArith(v.Args[0].Op) {
				continue
			}
			e := v.Args[0]
			if _, ok := exactInt(e, 8); !ok {
				continue
			}
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
			arith(v, e)
			changed = true
		}
	}
	return changed
}
