package vm

import (
	intl "github.com/go-quickjs/go-intl"

	"math"
	"math/bits"

	"github.com/go-quickjs/go-quickjs/internal/bignum"
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
	ctor.fn().mathOp = mathBigInt

	r.defMethod(ctor, "asIntN", 2, func(rt *Runtime, this Value, args []Value) (Value, error) {
		return rt.bigIntAsN(args, true)
	}).fn().mathOp = mathAsIntN
	r.defMethod(ctor, "asUintN", 2, func(rt *Runtime, this Value, args []Value) (Value, error) {
		return rt.bigIntAsN(args, false)
	}).fn().mathOp = mathAsUintN

	r.defMethod(p, "toString", 0, func(rt *Runtime, this Value, args []Value) (Value, error) {
		v, err := rt.thisBigIntValue(this, "BigInt.prototype.toString")
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
		return Str(bigString(v, radix)), nil
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
		return Str(NewString(o.nf.FormatDecimal(intl.ParseExactDecimal(b.String())))), nil
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

// thisBigIntValue is thisBigInt's BigInt as a value: one held in a Value is
// not made a BigInt of its own.
func (r *Runtime) thisBigIntValue(this Value, name string) (Value, error) {
	if this.IsBigInt() {
		return this, nil
	}
	b, err := r.thisBigInt(this, name)
	if err != nil {
		return Undefined, err
	}
	return Big(b), nil
}

// thisBigInt unwraps a receiver, accepting both a BigInt and a wrapper around
// one.
func (r *Runtime) thisBigInt(this Value, name string) (*BigInt, error) {
	if this.IsBigInt() {
		return this.BigInt(), nil
	}
	if this.IsObject() && this.Object().class == ClassBigIntWrapper {
		if b, ok := this.Object().dataOf().(*BigInt); ok {
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
		mag, neg, _ := bignum.FromFloat64(n)
		return bigOf(neg, mag), nil
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
	v := arg(args, 1)
	if !v.IsBigInt() {
		bv, err := r.toBigIntOperand(v)
		if err != nil {
			return Undefined, err
		}
		v = Big(bv)
	}
	if bits == 0 {
		return shortBig(0), nil
	}
	if bits <= 64 {
		return bigAsN64(v, uint(bits), signed), nil
	}
	// A value that already fits is itself, however wide the width: 2^bits
	// need not be made to learn that. One that does not fit is a result as
	// wide as bits, which past the limit no BigInt can be.
	if v.isShortBig() {
		// Of less than 64 bits of magnitude, past which the width is.
		if neg, _ := v.shortBigParts(); signed || !neg {
			return v, nil
		}
	}
	var buf bigScratch
	neg, x := bigParts(v, &buf)
	if signed && int64(bignum.BitLen(x)) < bits || !signed && !neg && int64(bignum.BitLen(x)) <= bits {
		return v, nil
	}
	if bits > maxBigIntBits {
		return Undefined, r.throwBigIntSize()
	}
	// The low bits of the two's complement: |x|'s, and for x below zero
	// 2^bits less them, unless they are zero.
	m := make([]uint, (bits+bitsPerWord-1)/bitsPerWord)
	copy(m, x)
	u := lowBits(m, bits)
	if neg && len(u) != 0 {
		u = complementBits(m, u, bits)
	}
	if signed && bitAt(u, bits-1) {
		// The top bit of the truncated value is the sign, so anything at or
		// above half the range wraps to the negative side.
		return bigOf(true, complementBits(make([]uint, len(m)), u, bits)), nil
	}
	return bigOf(false, u), nil
}

// bitsPerWord is a word's bits, as an int64 of a width.
const bitsPerWord = int64(bits.UintSize)

// lowBits is m, of the words of n bits, its bits from n on cleared,
// normalized.
func lowBits(m []uint, n int64) []uint {
	if r := n % bitsPerWord; r != 0 {
		m[len(m)-1] &= 1<<uint(r) - 1
	}
	return bignum.Norm(m)
}

// complementBits writes 2^n - u to z, of the words of n bits, for u under
// 2^n and not zero: the complement of u-1, in n bits.
func complementBits(z, u []uint, n int64) []uint {
	d := bignum.SubWord(make([]uint, len(u)), u, 1)
	for i := range z {
		var w uint
		if i < len(d) {
			w = d[i]
		}
		z[i] = ^w
	}
	return lowBits(z, n)
}

// bitAt reports whether bit n of a magnitude is set.
func bitAt(x []uint, n int64) bool {
	i := n / bitsPerWord
	return i < int64(len(x)) && x[i]>>uint(n%bitsPerWord)&1 != 0
}

// bigAsN64 is asIntN or asUintN of a BigInt to a width of 1 to 64 bits: the
// low bits of the value's two's complement, as a fixed-width integer's, no
// division, nor a BigInt made to learn them.
func bigAsN64(v Value, bits uint, signed bool) Value {
	return asN64(bigLow64(v), bits, signed)
}

// bigAsNCall is asIntN or asUintN, signed for the first, applied without a
// frame (mathCall) where its arguments need no conversion that could call
// anything or throw: an integral width of at most 64 bits, and a BigInt --
// how a program wraps its arithmetic to a fixed width, a hash's or a
// generator's at every step.
func bigAsNCall(signed bool, args []Value) (Value, bool) {
	if len(args) < 2 || !args[0].IsNumber() || !args[1].IsBigInt() {
		return Undefined, false
	}
	n := args[0].num
	if !(n >= 0 && n <= 64) || n != math.Trunc(n) {
		return Undefined, false
	}
	if n == 0 {
		return shortBig(0), true
	}
	return bigAsN64(args[1], uint(n), signed), true
}

// bigIntCall is BigInt(v) applied without a frame (mathCall) where v needs
// no conversion that could call anything or throw: an integral number in
// the int64 range, held in its Value, or a BigInt, itself -- how a program
// makes a BigInt of a character's code or a counter at every step.
func bigIntCall(args []Value) (Value, bool) {
	if len(args) == 0 {
		return Undefined, false
	}
	v := args[0]
	if v.IsBigInt() {
		return v, true
	}
	if n := v.num; v.IsNumber() && n > -(1<<63) && n < 1<<63 && n == math.Trunc(n) {
		return shortBig(int64(n)), true
	}
	return Undefined, false
}

// bigLow64 is the low 64 bits of a BigInt's two's complement.
func bigLow64(v Value) uint64 {
	if v.isShortBig() {
		return v.shortBigLow()
	}
	b := (*BigInt)(v.ref)
	words := b.words()
	var u uint64
	if len(words) > 0 {
		u = uint64(words[0])
	}
	if bits.UintSize == 32 && len(words) > 1 {
		u |= uint64(words[1]) << 32
	}
	if b.negative() {
		u = -u
	}
	return u
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
	return b.Float()
}

// shortArith applies an arithmetic operator to two BigInts held in their
// Values, as int64s. It reports false where bigOp must answer: an operand
// or a result outside the int64 range, a division by zero, a negative
// power.
func shortArith(op bytecode.Op, a, b Value) (Value, bool) {
	if !a.isInt64Big() || !b.isInt64Big() {
		return Undefined, false
	}
	x, y := a.int64Big(), b.int64Big()
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
// a BigInt's bitwise operators are defined in. It reports false for an
// operand outside the int64 range, and a left shift whose result leaves it.
func shortBitwise(op bytecode.Op, a, b Value) (Value, bool) {
	if !a.isInt64Big() || !b.isInt64Big() {
		return Undefined, false
	}
	x, y := a.int64Big(), b.int64Big()
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
	if n.isInt64Big() {
		return shortBig(^n.int64Big())
	}
	var buf bigScratch
	neg, x := bigParts(n, &buf)
	z := make([]uint, len(x)+1)
	copy(z, x)
	return bigOf(bignum.Not(z, neg, z[:len(x)]))
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
	var w bigScratch
	if bits.UintSize == 64 {
		w[0] = uint(u)
	} else {
		w[0], w[len(w)-1] = uint(u), uint(u>>32)
	}
	return bigOf(false, w[:])
}
