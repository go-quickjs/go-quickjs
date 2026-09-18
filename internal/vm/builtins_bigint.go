package vm

import (
	"math"
	"math/big"
)

// BigInt.
//
// The literal form and the arithmetic were already there; this is the
// constructor and the prototype, without which a BigInt could be computed but
// not converted, printed in another radix, or truncated to a width.

func (r *Runtime) initBigIntBuiltins() {
	p := r.proto.bigint

	ctor := r.newCtor("BigInt", 1, p, func(rt *Runtime, this Value, args []Value) (Value, error) {
		// BigInt is deliberately not constructible: `new BigInt(1)` would
		// suggest a wrapper object is the normal form, and it is not.
		if rt.Constructing() {
			return Undefined, rt.throwTypeError("BigInt is not a constructor")
		}
		return rt.toBigIntValue(arg(args, 0))
	})

	r.defMethod(ctor, "asIntN", 2, func(rt *Runtime, this Value, args []Value) (Value, error) {
		return rt.bigIntAsN(args, true)
	})
	r.defMethod(ctor, "asUintN", 2, func(rt *Runtime, this Value, args []Value) (Value, error) {
		return rt.bigIntAsN(args, false)
	})

	r.defMethod(p, "toString", 0, func(rt *Runtime, this Value, args []Value) (Value, error) {
		b, err := rt.thisBigInt(this, "BigInt.prototype.toString")
		if err != nil {
			return Undefined, err
		}
		radix := 10
		if rv := arg(args, 0); !rv.IsUndefined() {
			n, err := rt.toInteger(rv)
			if err != nil {
				return Undefined, err
			}
			if n < 2 || n > 36 {
				return Undefined, rt.throwRangeError("the radix must be between 2 and 36")
			}
			radix = int(n)
		}
		return Str(NewString(b.V.Text(radix))), nil
	})

	r.defMethod(p, "toLocaleString", 0, func(rt *Runtime, this Value, args []Value) (Value, error) {
		b, err := rt.thisBigInt(this, "BigInt.prototype.toLocaleString")
		if err != nil {
			return Undefined, err
		}
		// The digits are written as they are rather than through a float,
		// which would lose them: a big integer is big.
		o, err := rt.numberOptionsFrom(args)
		if err != nil {
			return Undefined, err
		}
		d, _ := parseDecimal(b.V.String())
		return Str(NewString(piecesText(o.decimalParts(d)))), nil
	})

	r.defMethod(p, "valueOf", 0, func(rt *Runtime, this Value, args []Value) (Value, error) {
		b, err := rt.thisBigInt(this, "BigInt.prototype.valueOf")
		if err != nil {
			return Undefined, err
		}
		return Big(b), nil
	})

	r.defToStringTag(p, "BigInt")
}

// thisBigInt unwraps a receiver, accepting both a BigInt and a wrapper around
// one.
func (r *Runtime) thisBigInt(this Value, name string) (*BigInt, error) {
	if this.IsBigInt() {
		return this.BigInt(), nil
	}
	if this.IsObject() && this.Object().class == ClassBigIntWrapper {
		if b, ok := this.Object().data.(*BigInt); ok {
			return b, nil
		}
	}
	return nil, r.throwTypeError("%s called on an incompatible receiver", name)
}

// toBigIntValue implements the BigInt function.
//
// A number converts only if it is an integer, because BigInt(1.5) would have to
// either round or fail, and silently rounding is the worse of the two.
func (r *Runtime) toBigIntValue(v Value) (Value, error) {
	prim, err := r.toPrimitive(v, hintNumber)
	if err != nil {
		return Undefined, err
	}
	switch {
	case prim.IsBigInt():
		return prim, nil
	case prim.IsNumber():
		n := prim.Number()
		if math.IsNaN(n) || math.IsInf(n, 0) || n != math.Trunc(n) {
			return Undefined, r.throwRangeError("only an integer can be converted to a BigInt")
		}
		b := &BigInt{}
		big.NewFloat(n).Int(&b.V)
		return Big(b), nil
	case prim.IsString():
		// A string of nothing but whitespace is zero, the way it is for
		// Number: it is an empty numeric literal, not a malformed one.
		b, ok := StringToBigInt(prim.String().Go())
		if !ok {
			return Undefined, r.throwSyntaxError("cannot convert %q to a BigInt",
				prim.String().Go())
		}
		return Big(b), nil
	case prim.IsBool():
		if prim.BoolValue() {
			return Big(NewBigInt(1)), nil
		}
		return Big(NewBigInt(0)), nil
	}
	return Undefined, r.throwTypeError("cannot convert %s to a BigInt", r.describe(prim))
}

// toBigIntOperand implements ToBigInt, which is what an operation expecting a
// BigInt uses.
//
// It differs from the BigInt function in one place, and deliberately: BigInt(1)
// is 1n, but writing the Number 1 into a BigInt64Array is a TypeError. The
// function is an explicit conversion the caller asked for; the operand position
// is one where a Number almost always means the two kinds got mixed up.
func (r *Runtime) toBigIntOperand(v Value) (*BigInt, error) {
	prim, err := r.toPrimitive(v, hintNumber)
	if err != nil {
		return nil, err
	}
	if prim.IsNumber() {
		return nil, r.throwTypeError("cannot convert a number to a BigInt")
	}
	out, err := r.toBigIntValue(prim)
	if err != nil {
		return nil, err
	}
	return out.BigInt(), nil
}

// bigIntAsN implements BigInt.asIntN and BigInt.asUintN, which truncate a
// BigInt to a given number of bits.
//
// They are how a program works with fixed-width integers: the arithmetic is
// arbitrary-precision, and wrapping back to 64 bits is an explicit step rather
// than something that happens by accident.
func (r *Runtime) bigIntAsN(args []Value, signed bool) (Value, error) {
	bits, err := r.toIndex(arg(args, 0))
	if err != nil {
		return Undefined, err
	}
	// This is ToBigInt rather than the BigInt constructor's conversion: a
	// number is refused here rather than truncated, however integral it is.
	bv, err := r.toBigIntOperand(arg(args, 1))
	if err != nil {
		return Undefined, err
	}
	if bits == 0 {
		return Big(NewBigInt(0)), nil
	}

	mod := new(big.Int).Lsh(big.NewInt(1), uint(bits))
	out := new(big.Int).Mod(&bv.V, mod)
	if out.Sign() < 0 {
		out.Add(out, mod)
	}
	if signed {
		// The top bit of the truncated value is the sign, so anything at or
		// above half the range wraps to the negative side.
		half := new(big.Int).Lsh(big.NewInt(1), uint(bits-1))
		if out.Cmp(half) >= 0 {
			out.Sub(out, mod)
		}
	}
	b := &BigInt{}
	b.V.Set(out)
	return Big(b), nil
}

// bigIntToFloat converts a BigInt to the nearest Number.
//
// The conversion is lossy above 2**53 and infinite beyond the float range,
// which is why it never happens implicitly: only Number(), and the comparison
// operators, ask for it.
func bigIntToFloat(b *BigInt) float64 {
	if b == nil {
		return 0
	}
	f, _ := new(big.Float).SetInt(&b.V).Float64()
	return f
}
