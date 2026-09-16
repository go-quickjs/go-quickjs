package vm

import (
	"math"

	"github.com/go-quickjs/go-quickjs/internal/jsnum"
)

// This file implements the abstract operations that define JavaScript's
// coercion rules. Most of the language's surprising behaviour lives here, so
// each operation follows the specification's algorithm step for step rather
// than taking shortcuts that happen to agree on common inputs.

// hint guides ToPrimitive, which tries valueOf and toString in an order that
// depends on what the caller wants.
type hint uint8

const (
	hintDefault hint = iota
	hintNumber
	hintString
)

func (h hint) String() string {
	switch h {
	case hintNumber:
		return "number"
	case hintString:
		return "string"
	}
	return "default"
}

// toPrimitive converts a value to a primitive, consulting Symbol.toPrimitive
// first and then falling back to valueOf and toString in hint order.
func (r *Runtime) toPrimitive(v Value, h hint) (Value, error) {
	if !v.IsObject() {
		return v, nil
	}
	o := v.Object()

	// A Symbol.toPrimitive method overrides the whole algorithm.
	symKey := r.atoms.internSymbol(r.wellKnown.toPrimitive)
	exotic, err := r.getProp(o, symKey, v)
	if err != nil {
		return Undefined, err
	}
	if !exotic.IsNullish() {
		if !isCallable(exotic) {
			return Undefined, r.throwTypeError("Symbol.toPrimitive is not a function")
		}
		res, err := r.call(exotic, v, []Value{Str(NewString(h.String()))})
		if err != nil {
			return Undefined, err
		}
		if res.IsObject() {
			return Undefined, r.throwTypeError("Symbol.toPrimitive returned an object")
		}
		return res, nil
	}

	return r.ordinaryToPrimitive(v, h)
}

// ordinaryToPrimitive is the fallback conversion: try valueOf and toString in
// the order the hint implies, taking the first that returns a primitive.
//
// Date's Symbol.toPrimitive calls it directly, because Date differs only in
// treating the default hint as string rather than as number.
func (r *Runtime) ordinaryToPrimitive(v Value, h hint) (Value, error) {
	o := v.Object()
	order := [2]Atom{atomValueOf, atomToString}
	if h == hintString {
		order = [2]Atom{atomToString, atomValueOf}
	}
	for _, name := range order {
		method, err := r.getProp(o, name, v)
		if err != nil {
			return Undefined, err
		}
		if !isCallable(method) {
			continue
		}
		res, err := r.call(method, v, nil)
		if err != nil {
			return Undefined, err
		}
		if !res.IsObject() {
			return res, nil
		}
	}
	return Undefined, r.throwTypeError("cannot convert an object to a primitive value")
}

// toNumber implements the ToNumber abstract operation.
func (r *Runtime) toNumber(v Value) (float64, error) {
	switch v.Kind() {
	case KindNumber:
		return v.Number(), nil
	case KindUndefined:
		return math.NaN(), nil
	case KindNull:
		return 0, nil
	case KindBool:
		if v.BoolValue() {
			return 1, nil
		}
		return 0, nil
	case KindString:
		return jsnum.ToNumber(v.String().Go()), nil
	case KindSymbol:
		return 0, r.throwTypeError("cannot convert a symbol to a number")
	case KindBigInt:
		return 0, r.throwTypeError("cannot convert a BigInt to a number")
	}
	p, err := r.toPrimitive(v, hintNumber)
	if err != nil {
		return 0, err
	}
	return r.toNumber(p)
}

// toNumeric is ToNumber except that a BigInt passes through, which the
// arithmetic operators need in order to stay in the BigInt domain.
func (r *Runtime) toNumeric(v Value) (Value, error) {
	if v.IsObject() {
		p, err := r.toPrimitive(v, hintNumber)
		if err != nil {
			return Undefined, err
		}
		v = p
	}
	if v.IsBigInt() {
		return v, nil
	}
	n, err := r.toNumber(v)
	if err != nil {
		return Undefined, err
	}
	return Float(n), nil
}

// toString implements the ToString abstract operation.
func (r *Runtime) toString(v Value) (*String, error) {
	switch v.Kind() {
	case KindString:
		return v.String(), nil
	case KindNumber:
		return NewString(jsnum.FormatFloat(v.Number())), nil
	case KindUndefined:
		return NewString("undefined"), nil
	case KindNull:
		return NewString("null"), nil
	case KindBool:
		if v.BoolValue() {
			return NewString("true"), nil
		}
		return NewString("false"), nil
	case KindSymbol:
		// A symbol must be converted explicitly with String(), never
		// implicitly, so that a typo in a template literal is caught.
		return nil, r.throwTypeError("cannot convert a symbol to a string")
	case KindBigInt:
		return NewString(v.BigInt().String()), nil
	}
	p, err := r.toPrimitive(v, hintString)
	if err != nil {
		return nil, err
	}
	return r.toString(p)
}

// toPropertyKey converts a value to an atom usable as a property key.
func (r *Runtime) toPropertyKey(v Value) (Atom, error) {
	switch v.Kind() {
	case KindNumber:
		// A numeric key that is a canonical index avoids interning entirely.
		return r.numberToAtom(v.Number()), nil
	case KindString:
		return r.atoms.intern(v.String().Go()), nil
	case KindSymbol:
		return r.atoms.internSymbol(v.Symbol()), nil
	}
	p, err := r.toPrimitive(v, hintString)
	if err != nil {
		return 0, err
	}
	if p.IsSymbol() {
		return r.atoms.internSymbol(p.Symbol()), nil
	}
	s, err := r.toString(p)
	if err != nil {
		return 0, err
	}
	return r.atoms.intern(s.Go()), nil
}

// toObject implements ToObject, wrapping a primitive in its object form.
func (r *Runtime) toObject(v Value) (*Object, error) {
	switch v.Kind() {
	case KindObject:
		return v.Object(), nil
	case KindString:
		o := newObject(r.proto.str, ClassStringWrapper)
		o.data = v.String()
		return o, nil
	case KindNumber:
		o := newObject(r.proto.number, ClassNumberWrapper)
		o.data = v.Number()
		return o, nil
	case KindBool:
		o := newObject(r.proto.boolean, ClassBooleanWrapper)
		o.data = v.BoolValue()
		return o, nil
	case KindSymbol:
		o := newObject(r.proto.symbol, ClassSymbolWrapper)
		o.data = v.Symbol()
		return o, nil
	case KindBigInt:
		o := newObject(r.proto.bigint, ClassBigIntWrapper)
		o.data = v.BigInt()
		return o, nil
	}
	return nil, r.throwTypeError("cannot convert %s to an object", v.Kind())
}

// toInt32, toUint32 and toInteger are the numeric conversions the operators
// use. They report any error from coercing an object operand.

func (r *Runtime) toInt32(v Value) (int32, error) {
	// A number operand is by far the common case and skips the conversion.
	if v.IsNumber() {
		return jsnum.ToInt32(v.Number()), nil
	}
	n, err := r.toNumber(v)
	if err != nil {
		return 0, err
	}
	return jsnum.ToInt32(n), nil
}

func (r *Runtime) toUint32(v Value) (uint32, error) {
	if v.IsNumber() {
		return jsnum.ToUint32(v.Number()), nil
	}
	n, err := r.toNumber(v)
	if err != nil {
		return 0, err
	}
	return jsnum.ToUint32(n), nil
}

func (r *Runtime) toInteger(v Value) (float64, error) {
	n, err := r.toNumber(v)
	if err != nil {
		return 0, err
	}
	return jsnum.ToInteger(n), nil
}

// toLength clamps to the range a length may take, as the array methods require.
func (r *Runtime) toLength(v Value) (int64, error) {
	n, err := r.toInteger(v)
	if err != nil {
		return 0, err
	}
	switch {
	case n <= 0:
		return 0, nil
	case n > maxSafeInteger:
		return maxSafeInteger, nil
	}
	return int64(n), nil
}

// maxSafeInteger is 2^53 - 1, the largest integer a float64 represents exactly.
const maxSafeInteger = 9007199254740991

// toIndex converts a value used as an offset, rejecting negatives and
// non-integers.
func (r *Runtime) toIndex(v Value) (int64, error) {
	if v.IsUndefined() {
		return 0, nil
	}
	n, err := r.toInteger(v)
	if err != nil {
		return 0, err
	}
	if n < 0 || n > maxSafeInteger {
		return 0, r.throwRangeError("invalid index")
	}
	return int64(n), nil
}

// isCallable reports whether a value can be called.
func isCallable(v Value) bool {
	return v.IsObject() && v.Object().IsCallable()
}

// ---------------------------------------------------------------------------
// Comparison
// ---------------------------------------------------------------------------

// looseEquals implements the == operator.
//
// The algorithm is deliberately spelled out rather than compressed, because
// every asymmetry in it is deliberate: null equals undefined but nothing else,
// NaN equals nothing, and an object is compared by converting it to a primitive
// rather than by converting the other side to an object.
func (r *Runtime) looseEquals(a, b Value) (bool, error) {
	ak, bk := a.Kind(), b.Kind()
	if ak == bk {
		return a.StrictEquals(b), nil
	}

	switch {
	case (ak == KindNull && bk == KindUndefined) ||
		(ak == KindUndefined && bk == KindNull):
		return true, nil

	case ak == KindNumber && bk == KindString:
		return a.Number() == jsnum.ToNumber(b.String().Go()), nil
	case ak == KindString && bk == KindNumber:
		return jsnum.ToNumber(a.String().Go()) == b.Number(), nil

	case ak == KindBigInt && bk == KindString:
		o, ok := ParseBigInt(b.String().Go())
		return ok && a.BigInt().Cmp(o) == 0, nil
	case ak == KindString && bk == KindBigInt:
		o, ok := ParseBigInt(a.String().Go())
		return ok && o.Cmp(b.BigInt()) == 0, nil

	case ak == KindBool:
		n, err := r.toNumber(a)
		if err != nil {
			return false, err
		}
		return r.looseEquals(Float(n), b)
	case bk == KindBool:
		n, err := r.toNumber(b)
		if err != nil {
			return false, err
		}
		return r.looseEquals(a, Float(n))

	case (ak == KindNumber || ak == KindBigInt) && bk == KindObject,
		ak == KindString && bk == KindObject,
		ak == KindSymbol && bk == KindObject:
		p, err := r.toPrimitive(b, hintDefault)
		if err != nil {
			return false, err
		}
		return r.looseEquals(a, p)

	case ak == KindObject && (bk == KindNumber || bk == KindBigInt),
		ak == KindObject && bk == KindString,
		ak == KindObject && bk == KindSymbol:
		p, err := r.toPrimitive(a, hintDefault)
		if err != nil {
			return false, err
		}
		return r.looseEquals(p, b)

	case ak == KindBigInt && bk == KindNumber:
		return bigIntEqualsFloat(a.BigInt(), b.Number()), nil
	case ak == KindNumber && bk == KindBigInt:
		return bigIntEqualsFloat(b.BigInt(), a.Number()), nil
	}
	return false, nil
}

// bigIntEqualsFloat compares a BigInt with a number without losing precision,
// which matters because a BigInt can exceed the exactly representable range.
func bigIntEqualsFloat(b *BigInt, f float64) bool {
	if math.IsNaN(f) || math.IsInf(f, 0) || f != math.Trunc(f) {
		return false
	}
	other, ok := bigIntFromFloat(f)
	return ok && b.Cmp(other) == 0
}

// compare implements the relational operators.
//
// It returns the three-way result plus an "undefined" flag for the NaN case,
// which the four operators interpret differently: `a < b` and `a > b` are false
// when either operand is NaN, and so are `a <= b` and `a >= b`.
type cmpResult uint8

const (
	cmpLess cmpResult = iota
	cmpEqual
	cmpGreater
	cmpUndefined
)

func (r *Runtime) compare(a, b Value) (cmpResult, error) {
	pa, err := r.toPrimitive(a, hintNumber)
	if err != nil {
		return cmpUndefined, err
	}
	pb, err := r.toPrimitive(b, hintNumber)
	if err != nil {
		return cmpUndefined, err
	}

	// Two strings compare by code unit, not numerically, which is why
	// "10" < "9" is true.
	if pa.IsString() && pb.IsString() {
		switch c := pa.String().Compare(pb.String()); {
		case c < 0:
			return cmpLess, nil
		case c > 0:
			return cmpGreater, nil
		}
		return cmpEqual, nil
	}

	if pa.IsBigInt() || pb.IsBigInt() {
		return r.compareBigInt(pa, pb)
	}

	na, err := r.toNumber(pa)
	if err != nil {
		return cmpUndefined, err
	}
	nb, err := r.toNumber(pb)
	if err != nil {
		return cmpUndefined, err
	}
	switch {
	case math.IsNaN(na) || math.IsNaN(nb):
		return cmpUndefined, nil
	case na < nb:
		return cmpLess, nil
	case na > nb:
		return cmpGreater, nil
	}
	return cmpEqual, nil
}

// compareBigInt orders a BigInt against another BigInt or a number.
func (r *Runtime) compareBigInt(a, b Value) (cmpResult, error) {
	switch {
	case a.IsBigInt() && b.IsBigInt():
		return cmpFromInt(a.BigInt().Cmp(b.BigInt())), nil
	case a.IsBigInt():
		n, err := r.toNumber(b)
		if err != nil {
			return cmpUndefined, err
		}
		if math.IsNaN(n) {
			return cmpUndefined, nil
		}
		return cmpBigIntFloat(a.BigInt(), n), nil
	default:
		n, err := r.toNumber(a)
		if err != nil {
			return cmpUndefined, err
		}
		if math.IsNaN(n) {
			return cmpUndefined, nil
		}
		// Reverse the comparison, since the BigInt is on the right.
		switch cmpBigIntFloat(b.BigInt(), n) {
		case cmpLess:
			return cmpGreater, nil
		case cmpGreater:
			return cmpLess, nil
		}
		return cmpEqual, nil
	}
}

func cmpFromInt(c int) cmpResult {
	switch {
	case c < 0:
		return cmpLess
	case c > 0:
		return cmpGreater
	}
	return cmpEqual
}

// cmpBigIntFloat compares a BigInt with a finite or infinite number.
func cmpBigIntFloat(b *BigInt, f float64) cmpResult {
	switch {
	case math.IsInf(f, 1):
		return cmpLess
	case math.IsInf(f, -1):
		return cmpGreater
	}
	// Compare against the truncated value, then break a tie using the
	// fractional part, so that 1n < 1.5 is correct.
	trunc := math.Trunc(f)
	other, ok := bigIntFromFloat(trunc)
	if !ok {
		return cmpUndefined
	}
	if c := b.Cmp(other); c != 0 {
		return cmpFromInt(c)
	}
	switch {
	case f > trunc:
		return cmpLess
	case f < trunc:
		return cmpGreater
	}
	return cmpEqual
}
