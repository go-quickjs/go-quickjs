package vm

import (
	intl "github.com/go-quickjs/go-intl"

	"math"
	"math/big"
	"math/bits"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
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
		return Str(NewString(o.nf.FormatDecimal(intl.ParseExactDecimal(b.V.String())))), nil
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
		if n > -(1<<63) && n < 1<<63 {
			// What an int64 holds converts exactly, and is held so.
			return shortBig(int64(n)), nil
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
			return shortBig(1), nil
		}
		return shortBig(0), nil
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

// maxBigIntBits is the most bits a BigInt may have: V8's, past which it
// throws "Maximum BigInt size exceeded".
const maxBigIntBits = 1 << 30

// throwBigIntSize refuses a BigInt wider than maxBigIntBits.
func (r *Runtime) throwBigIntSize() error {
	return r.throwRangeError("Maximum BigInt size exceeded")
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
		return shortBig(0), nil
	}
	// A value that already fits is itself, however wide the width: 2^bits
	// need not be made to learn that. One that does not fit is a result as
	// wide as bits, which past the limit no BigInt can be.
	if signed && int64(bv.V.BitLen()) < bits || !signed && bv.V.Sign() >= 0 && int64(bv.V.BitLen()) <= bits {
		b := &BigInt{}
		b.V.Set(&bv.V)
		return Big(b), nil
	}
	if bits > maxBigIntBits {
		return Undefined, r.throwBigIntSize()
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

// shortArith applies an arithmetic operator to two BigInts held in their
// Values, as int64s. It reports false where bigArith must answer: a result
// outside the int64 range, a division by zero, a negative power.
func shortArith(op bytecode.Op, a, b Value) (Value, bool) {
	x, y := a.shortBigInt(), b.shortBigInt()
	switch op {
	case bytecode.OpAdd:
		if s := x + y; (s^x)&(s^y) >= 0 {
			return shortBig(s), true
		}
	case bytecode.OpSub:
		if s := x - y; (x^y)&(s^x) >= 0 {
			return shortBig(s), true
		}
	case bytecode.OpMul:
		if p, ok := mulInt64(x, y); ok {
			return shortBig(p), true
		}
	case bytecode.OpDiv:
		// Go's division truncates toward zero, as a BigInt's does; only the
		// most negative value over -1 leaves the range.
		if y != 0 && (x != math.MinInt64 || y != -1) {
			return shortBig(x / y), true
		}
	case bytecode.OpMod:
		// The remainder takes the dividend's sign in both, and Go gives 0
		// for the most negative value over -1.
		if y != 0 {
			return shortBig(x % y), true
		}
	case bytecode.OpPow:
		if y >= 0 {
			p, base := int64(1), x
			for ok := true; ; {
				if y&1 != 0 {
					if p, ok = mulInt64(p, base); !ok {
						break
					}
				}
				if y >>= 1; y == 0 {
					return shortBig(p), true
				}
				if base, ok = mulInt64(base, base); !ok {
					break
				}
			}
		}
	}
	return Undefined, false
}

// mulInt64 is x*y, reporting false when the product is outside the int64
// range.
func mulInt64(x, y int64) (int64, bool) {
	ux, uy := uint64(x), uint64(y)
	if x < 0 {
		ux = -ux
	}
	if y < 0 {
		uy = -uy
	}
	hi, lo := bits.Mul64(ux, uy)
	if (x < 0) != (y < 0) {
		// Down to -2^63, which has no positive counterpart.
		if hi != 0 || lo > 1<<63 {
			return 0, false
		}
		return -int64(lo), true
	}
	if hi != 0 || lo > math.MaxInt64 {
		return 0, false
	}
	return int64(lo), true
}

// shortBitwise applies a bitwise or shift operator other than >>> to two
// BigInts held in their Values, which as int64s are in the two's complement
// a BigInt's bitwise operators are defined in. It reports false for a left
// shift whose result leaves the int64 range.
func shortBitwise(op bytecode.Op, a, b Value) (Value, bool) {
	x, y := a.shortBigInt(), b.shortBigInt()
	switch op {
	case bytecode.OpBitAnd:
		return shortBig(x & y), true
	case bytecode.OpBitOr:
		return shortBig(x | y), true
	case bytecode.OpBitXor:
		return shortBig(x ^ y), true
	case bytecode.OpShl, bytecode.OpShr:
		// A negative count shifts the other way.
		left := op == bytecode.OpShl
		if y < 0 {
			if y == math.MinInt64 {
				return Undefined, false
			}
			y, left = -y, !left
		}
		if !left {
			// Past every bit, the sign is what is left: 0 or -1.
			return shortBig(x >> min(y, 63)), true
		}
		if x == 0 {
			return shortBig(0), true
		}
		if y < 63 {
			if s := x << y; s>>y == x {
				return shortBig(s), true
			}
		}
	}
	return Undefined, false
}

// bigNot is ~ on a BigInt. A BigInt has no width, so the complement is
// -(x+1), which is what both an int64's and big.Int's Not compute.
func bigNot(n Value) Value {
	if n.isShortBig() {
		return shortBig(^n.shortBigInt())
	}
	out := &BigInt{}
	out.V.Not(&n.BigInt().V)
	return Big(out)
}

// bigArg is a BigInt operand as a *BigInt for an operation that keeps
// neither operand: one held in its Value is written into the runtime's
// bigArgs[i], whose own words hold any int64, rather than made anew.
func (r *Runtime) bigArg(v Value, i int) *BigInt {
	if !v.isShortBig() {
		return v.BigInt()
	}
	s := &r.bigArgs[i]
	s.b.V.SetBits(s.words[:0])
	s.b.V.SetInt64(v.shortBigInt())
	return &s.b
}

// shortBinImm is OpBinImm's operator on two BigInts held in their Values,
// reporting false where the operator's own path must answer.
func shortBinImm(op bytecode.Op, a, b Value) (Value, bool) {
	switch op {
	case bytecode.OpAdd, bytecode.OpSub, bytecode.OpMul:
		return shortArith(op, a, b)
	case bytecode.OpUShr:
		return Undefined, false
	}
	return shortBitwise(op, a, b)
}

// bigFromUint64 is the BigInt u, held in the Value when it is in the int64
// range.
func bigFromUint64(u uint64) Value {
	if u <= math.MaxInt64 {
		return shortBig(int64(u))
	}
	b := &BigInt{}
	b.V.SetUint64(u)
	return heapBig(b)
}
