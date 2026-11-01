package vm

import "github.com/go-quickjs/go-quickjs/internal/bytecode"

// BigInt64Array's and BigUint64Array's elements, out of line as
// zzz_direct.go's calls are: generic functions, which Go lays out after
// every function and closure of the package, each called where the call
// it stands for was.

// toBigIntOperandValue is toBigIntOperand's ToBigInt with its result left
// a Value: a BigInt an int64 holds stays held in the Value (shortBig),
// where toBigIntOperand makes a *BigInt of its own of one (Value.BigInt
// allocates it). A BigInt64Array's or a BigUint64Array's store, which
// needs only the result's low 64 bits (bigLow64), had made one at every
// element it stored.
func toBigIntOperandValue[_ any](r *Runtime, v Value) (Value, error) {
	if v.IsBigInt() {
		return v, nil
	}
	prim, err := r.toPrimitive(v, hintNumber)
	if err != nil {
		return Undefined, err
	}
	if prim.IsNumber() {
		return Undefined, r.throwTypeError("cannot convert a number to a BigInt")
	}
	return r.toBigIntValue(prim)
}

// pureBigCompare is pureCompare's ordering of two BigInts an int64 holds,
// as their integers are ordered, which pureCompare asks for what is not
// two numbers: a comparator or a predicate over a BigInt64Array's
// elements -- a < b ? -1 : 1, x <= last -- had missed at every call. It
// reports false for anything else, a BigInt beside a Number included,
// which the frame's comparison makes.
func pureBigCompare[_ any](op bytecode.Op, a, b Value) (res, ok bool) {
	if !a.isInt64Big() || !b.isInt64Big() {
		return false, false
	}
	x, y := a.int64Big(), b.int64Big()
	switch op {
	case bytecode.OpLt:
		return x < y, true
	case bytecode.OpLe:
		return x <= y, true
	case bytecode.OpGt:
		return x > y, true
	case bytecode.OpGe:
		return x >= y, true
	}
	return false, false
}
