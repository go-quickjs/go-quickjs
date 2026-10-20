package vm

import (
	"math"
	"math/bits"
	"strconv"
	"unsafe"

	"github.com/go-quickjs/go-quickjs/internal/bignum"
	"github.com/go-quickjs/go-quickjs/internal/bytecode"
)

// BigInt arithmetic on the VM's values by bignum's, math/big not among it:
// an operand is a sign and a magnitude -- one held in its Value spelled in
// words of scratch, one on the heap its own block's -- and a result is made
// in the runtime's scratch, then held in its Value in the int64 range, or a
// BigInt of its own (bigOf). math/big had cost more than C QuickJS's whole
// operation, for numbers of a few words.
//
// The file sorts after every file of the interpreter's hot code, so that
// what is here moves none of it.

// bigScratch is where an operand held in its Value is spelled: the words
// of any int64's magnitude.
type bigScratch [64 / bits.UintSize]uint

// bigParts is a BigInt's sign and magnitude, one held in its Value spelled
// in buf.
func bigParts(v Value, buf *bigScratch) (bool, []uint) {
	if !v.isShortBig() {
		return heapParts(v)
	}
	return shortParts(v, buf)
}

// heapParts is bigParts of a BigInt on the heap, which is never zero:
// what inlines, where bigParts does not, for the arithmetic's operands.
func heapParts(v Value) (bool, []uint) {
	b := (*BigInt)(v.ref)
	return b.hdr&bigNegBit != 0, unsafe.Slice((*uint)(unsafe.Add(unsafe.Pointer(b), bigWordSize)), b.hdr&^bigNegBit)
}

// shortParts is bigParts of a BigInt held in its Value.
func shortParts(v Value, buf *bigScratch) (bool, []uint) {
	x := v.shortBigInt()
	u := uint64(x)
	if x < 0 {
		u = -u
	}
	if bits.UintSize == 64 {
		buf[0] = uint(u)
	} else {
		buf[0], buf[len(buf)-1] = uint(u), uint(u>>32)
	}
	return x < 0, bignum.Norm(buf[:])
}

// bigOf is a result's value: held in its Value in the int64 range, a BigInt
// of a copy of its words outside it.
func bigOf(neg bool, mag []uint) Value {
	if len(mag) <= 64/bits.UintSize {
		var u uint64
		for i, w := range mag {
			u |= uint64(w) << (i * bits.UintSize)
		}
		if u <= math.MaxInt64 {
			if neg {
				return shortBig(-int64(u))
			}
			return shortBig(int64(u))
		}
		if neg && u == 1<<63 {
			return shortBig(math.MinInt64)
		}
	}
	b := newBigWords(len(mag), neg)
	copy(b.words(), mag)
	return heapBig(b)
}

// bigBlockFrom is the words from which a result is made in a BigInt of its
// own from the first, not in scratch to be copied: what no result of fewer
// words is held in its Value from.
const bigBlockFrom = 64/bits.UintSize + 2

// bigInPlace is room for a result of n words: a BigInt of its own's digits
// past bigBlockFrom words, which bigDone gives its length, or the runtime's
// scratch.
func (r *Runtime) bigInPlace(n int) (*BigInt, []uint) {
	if n < bigBlockFrom {
		return nil, r.bigRoom(n)
	}
	b := newBigWords(n, false)
	return b, b.words()
}

// bigDone is a result made in bigInPlace's room as a value: held in its
// Value in the int64 range, the BigInt, its length the result's, if one was
// made, and a BigInt of a copy of it if not.
func bigDone(b *BigInt, neg bool, mag []uint) Value {
	if b == nil || len(mag) <= 64/bits.UintSize {
		return bigOf(neg, mag)
	}
	b.hdr = uint(len(mag))
	if neg {
		b.hdr |= bigNegBit
	}
	return heapBig(b)
}

// bigRoom is the runtime's scratch for a result of n words, kept for the
// next while it is small.
func (r *Runtime) bigRoom(n int) []uint {
	if cap(r.bigTmp) < n {
		if n > bigRoomKept {
			return make([]uint, n)
		}
		r.bigTmp = make([]uint, max(n, 2*cap(r.bigTmp), 16))
	}
	return r.bigTmp[:n]
}

// bigRoomKept is the most words of scratch the runtime keeps between
// operations: one of numbers larger allocates its own.
const bigRoomKept = 1 << 12

// bigWordsNear is the words from which a result's bits are checked against
// maxBigIntBits, which no smaller one can pass.
const bigWordsNear = maxBigIntBits/bits.UintSize - 1

// bigOp is the arithmetic operator op of two BigInts, as the interpreter
// makes it: RangeErrors for a result past maxBigIntBits, a division by
// zero, a negative power; the memory a meter allows reserved first.
func (r *Runtime) bigOp(op bytecode.Op, a, b Value) (Value, error) {
	var ba, bb bigScratch
	var an, bn bool
	var x, y []uint
	if a.isShortBig() {
		an, x = shortParts(a, &ba)
	} else {
		an, x = heapParts(a)
	}
	if b.isShortBig() {
		bn, y = shortParts(b, &bb)
	} else {
		bn, y = heapParts(b)
	}
	switch op {
	case bytecode.OpAdd, bytecode.OpSub:
		if op == bytecode.OpSub {
			bn = !bn
		}
		if max(len(x), len(y)) >= bigWordsNear && max(bignum.BitLen(x), bignum.BitLen(y))+1 > maxBigIntBits {
			// The one case where a sum could exceed the limit.
			neg, z := bigSum(r.bigRoom(max(len(x), len(y))+1), an, x, bn, y)
			if bignum.BitLen(z) > maxBigIntBits {
				return Undefined, r.throwBigIntSize()
			}
			return bigOf(neg, z), nil
		}
		blk, z := r.bigInPlace(max(len(x), len(y)) + 1)
		neg, s := bigSum(z, an, x, bn, y)
		return bigDone(blk, neg, s), nil
	case bytecode.OpMul:
		if len(x) == 0 || len(y) == 0 {
			return shortBig(0), nil
		}
		// A product has at least as many bits as its factors' less one.
		if len(x)+len(y) >= bigWordsNear && bignum.BitLen(x)+bignum.BitLen(y)-1 > maxBigIntBits {
			return Undefined, r.throwBigIntSize()
		}
		if err := r.reserveBigInt(int64(len(x)+len(y)) * bits.UintSize); err != nil {
			return Undefined, err
		}
		blk, z := r.bigInPlace(bignum.MulRoom(len(x), len(y)))
		if a.sameBits(b) {
			z = bignum.Sqr(z, x)
		} else {
			z = bignum.Mul(z, x, y)
		}
		return bigDone(blk, an != bn, z), nil
	case bytecode.OpDiv, bytecode.OpMod:
		if len(y) == 0 {
			return Undefined, r.throwRangeError("division by zero")
		}
		if op == bytecode.OpDiv {
			q, _ := bignum.DivMod(r.bigRoom(bignum.QuoRoom(len(x), len(y))), nil, x, y)
			return bigOf(an != bn, q), nil
		}
		// The remainder takes the dividend's sign.
		_, rem := bignum.DivMod(nil, r.bigRoom(len(y)), x, y)
		return bigOf(an, rem), nil
	case bytecode.OpPow:
		return r.bigPow(an, x, b, bn, y)
	}
	return Undefined, r.throwTypeError("unsupported BigInt operation")
}

// bigSum is x+y of signed magnitudes, into z, of max(len(x), len(y))+1
// words.
func bigSum(z []uint, an bool, x []uint, bn bool, y []uint) (bool, []uint) {
	if an == bn {
		return an, bignum.Add(z, x, y)
	}
	// Of opposite signs: the larger magnitude's sign, the difference.
	if bignum.Cmp(x, y) < 0 {
		x, y, an = y, x, bn
	}
	d := bignum.Sub(z, x, y)
	return an && len(d) != 0, d
}

// bigPow is x**y, by squaring and multiplying, its size checked first: a
// base other than 0, 1 and -1 has at least as many bits as the power,
// times its own less one.
func (r *Runtime) bigPow(xn bool, x []uint, b Value, yn bool, y []uint) (Value, error) {
	if yn {
		return Undefined, r.throwRangeError("a BigInt cannot be raised to a negative power")
	}
	if len(y) == 0 {
		return shortBig(1), nil
	}
	if len(x) == 0 {
		return shortBig(0), nil
	}
	if len(x) == 1 && x[0] == 1 {
		// 1 or -1: the power's parity is all that matters.
		return bigOf(xn && y[0]&1 != 0, x), nil
	}
	if !b.isShortBig() || b.shortBigInt() > maxBigIntBits || int64(bignum.BitLen(x)-1)*b.shortBigInt() > maxBigIntBits {
		return Undefined, r.throwBigIntSize()
	}
	e := uint64(b.shortBigInt())
	if err := r.reserveBigInt(int64(bignum.BitLen(x)) * int64(e)); err != nil {
		return Undefined, err
	}
	words := (bignum.BitLen(x)*int(e))/bits.UintSize + 2
	acc, sq := make([]uint, 1, words), make([]uint, len(x), words)
	acc[0] = 1
	copy(sq, x)
	t := make([]uint, 2*words)
	for {
		if e&1 != 0 {
			p := bignum.Mul(t[:len(acc)+len(sq)], acc, sq)
			acc = append(acc[:0], p...)
		}
		if e >>= 1; e == 0 {
			break
		}
		p := bignum.Sqr(t[:2*len(sq)], sq)
		sq = append(sq[:0], p...)
	}
	return bigOf(xn && b.shortBigInt()&1 != 0, acc), nil
}

// bigBits is the bitwise operator or shift op of two BigInts: a shift's
// count taken whole, a negative one shifting the other way; a RangeError
// for a left shift past maxBigIntBits.
func (r *Runtime) bigBits(op bytecode.Op, a, b Value) (Value, error) {
	var ba, bb bigScratch
	var an, bn bool
	var x, y []uint
	if a.isShortBig() {
		an, x = shortParts(a, &ba)
	} else {
		an, x = heapParts(a)
	}
	if b.isShortBig() {
		bn, y = shortParts(b, &bb)
	} else {
		bn, y = heapParts(b)
	}
	switch op {
	case bytecode.OpBitAnd:
		return bigOf(bignum.And(r.bigRoom(max(len(x), len(y))+1), an, x, bn, y)), nil
	case bytecode.OpBitOr:
		return bigOf(bignum.Or(r.bigRoom(max(len(x), len(y))+1), an, x, bn, y)), nil
	case bytecode.OpBitXor:
		return bigOf(bignum.Xor(r.bigRoom(max(len(x), len(y))+1), an, x, bn, y)), nil
	case bytecode.OpShl, bytecode.OpShr:
		left := op == bytecode.OpShl
		if bn {
			left = !left
		}
		if !b.isShortBig() || b.shortBigInt() > maxBigIntBits || b.shortBigInt() < -maxBigIntBits {
			// Past every bit: a left shift of anything but zero is too
			// large, a right one leaves the sign.
			if left {
				if len(x) == 0 {
					return shortBig(0), nil
				}
				return Undefined, r.throwBigIntSize()
			}
			if an {
				return shortBig(-1), nil
			}
			return shortBig(0), nil
		}
		s := uint(b.shortBigInt())
		if bn {
			s = uint(-b.shortBigInt())
		}
		if left {
			if len(x) == 0 {
				return shortBig(0), nil
			}
			if int64(bignum.BitLen(x))+int64(s) > maxBigIntBits {
				return Undefined, r.throwBigIntSize()
			}
			if err := r.reserveBigInt(int64(bignum.BitLen(x)) + int64(s)); err != nil {
				return Undefined, err
			}
			return bigOf(an, bignum.Shl(r.bigRoom(bignum.ShlRoom(len(x), s)), x, s)), nil
		}
		return bigOf(bignum.Sar(r.bigRoom(bignum.SarRoom(len(x))), an, x, s)), nil
	}
	return Undefined, r.throwTypeError("unsupported BigInt operation")
}

// bigNeg is -v of a BigInt.
func bigNeg(v Value) Value {
	var buf bigScratch
	n, x := bigParts(v, &buf)
	return bigOf(!n && len(x) != 0, x)
}

// bigNotOf is ~v of a BigInt.
func (r *Runtime) bigNotOf(v Value) Value {
	var buf bigScratch
	n, x := bigParts(v, &buf)
	z := r.bigRoom(len(x) + 1)
	copy(z, x)
	return bigOf(bignum.Not(z, n, z[:len(x)]))
}

// bigCmp compares two BigInts.
func bigCmp(a, b Value) int {
	if a.isShortBig() && b.isShortBig() {
		x, y := a.shortBigInt(), b.shortBigInt()
		switch {
		case x < y:
			return -1
		case x > y:
			return 1
		}
		return 0
	}
	var ba, bb bigScratch
	var an, bn bool
	var x, y []uint
	if a.isShortBig() {
		an, x = shortParts(a, &ba)
	} else {
		an, x = heapParts(a)
	}
	if b.isShortBig() {
		bn, y = shortParts(b, &bb)
	} else {
		bn, y = heapParts(b)
	}
	switch {
	case an && !bn:
		return -1
	case !an && bn:
		return 1
	case an:
		return bignum.Cmp(y, x)
	}
	return bignum.Cmp(x, y)
}

// bigString is a BigInt in base, 2 to 36, as a String, its digits in lower
// case: written into the String's own room, which they are ASCII for.
func bigString(v Value, base int) *String {
	var b [128]byte
	if v.isShortBig() {
		return asciiString(strconv.AppendInt(b[:0], v.shortBigInt(), base))
	}
	var buf bigScratch
	n, x := bigParts(v, &buf)
	return asciiString(bignum.Append(b[:0], n, x, base))
}

// bigText is a BigInt in base, 2 to 36, its digits in lower case.
func bigText(v Value, base int) string {
	var buf bigScratch
	n, x := bigParts(v, &buf)
	return string(bignum.Append(nil, n, x, base))
}

// bigFloat is a BigInt as the nearest Number.
func bigFloat(v Value) float64 {
	var buf bigScratch
	n, x := bigParts(v, &buf)
	f := bignum.Float64(x)
	if n {
		return -f
	}
	return f
}
