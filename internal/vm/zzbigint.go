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
	return b.hdr&bigNegBit != 0, unsafe.Slice((*uint)(unsafe.Add(unsafe.Pointer(b), bigWordSize)), b.hdr&bigLenMask)
}

// shortParts is bigParts of a BigInt held in its Value.
func shortParts(v Value, buf *bigScratch) (bool, []uint) {
	neg, u := v.shortBigParts()
	return neg, wordsOfUint64(u, buf)
}

// wordsOfUint64 is a magnitude below 2^64 in the words of buf.
func wordsOfUint64(u uint64, buf *bigScratch) []uint {
	if bits.UintSize == 64 {
		buf[0] = uint(u)
	} else {
		buf[0], buf[len(buf)-1] = uint(u), uint(u>>32)
	}
	return bignum.Norm(buf[:])
}

// bigOf is a result's value: held in its Value in the int64 range, a BigInt
// of a copy of its words outside it.
func bigOf(neg bool, mag []uint) Value {
	if len(mag) <= 64/bits.UintSize {
		// Below 2^64: held in its Value.
		var u uint64
		for i, w := range mag {
			u |= uint64(w) << (i * bits.UintSize)
		}
		return shortBigMag(neg, u)
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
// past bigBlockFrom words, which bigDone gives its length -- one given
// back of room for n, where there is one -- or the runtime's scratch.
func (r *Runtime) bigInPlace(n int) (*BigInt, []uint) {
	if n < bigBlockFrom {
		return nil, r.bigRoom(n)
	}
	if b := r.bigTake(n); b != nil {
		return b, b.words()
	}
	b := newBigWords(n, false)
	return b, b.words()
}

// bigTake is a block given back of room for n words, of n words of
// digits, or nil where there is none.
func (r *Runtime) bigTake(n int) *BigInt {
	f := r.bigFree
	if f == nil || n < bigBlockFrom || n > bigFreeTop {
		return nil
	}
	i := n - bigBlockFrom
	k := &f.n[i]
	if *k == 0 {
		return nil
	}
	*k--
	b := f.b[i][*k]
	f.b[i][*k] = nil
	b.hdr = uint(n) | uint(n)<<bigCapShift
	return b
}

// bigOfKept is bigOf, the BigInt made in a block given back of its room
// where there is one.
func (r *Runtime) bigOfKept(neg bool, mag []uint) Value {
	if len(mag) >= bigBlockFrom {
		if b := r.bigTake(len(mag)); b != nil {
			copy(b.words(), mag)
			if neg {
				b.hdr |= bigNegBit
			}
			return heapBig(b)
		}
	}
	return bigOf(neg, mag)
}

// bigFreeList is the BigInts given back to a runtime, by their room in
// words, from bigBlockFrom to bigFreeTop, up to bigFreeDepth of each:
// the results of arithmetic in the tree tier that an operator of its
// parent node took and nothing else saw, whose blocks C QuickJS would
// free as it goes. A loop of arithmetic on numbers of hundreds of bits
// makes results the size of the ones before, and in their blocks the
// collector has a fraction of the bytes to collect -- and scans the
// runtime's live heap that much less often.
type bigFreeList struct {
	n [bigFreeTop - bigBlockFrom + 1]uint8
	b [bigFreeTop - bigBlockFrom + 1][bigFreeDepth]*BigInt
}

const (
	bigFreeTop   = 64
	bigFreeDepth = 4
)

// bigRelease gives back a's and b's blocks, those of a and b that flags
// marks as fresh (treeFreshA, treeFreshB) -- each a result of the tree
// tier's arithmetic that an operator of two BigInts has just taken, and
// nothing else has seen -- and old's, the value of the local v is about
// to be stored to, where flags says the local holds its values alone
// (treeOwnRes) and old is one such a store kept (bigSoleBit), for
// bigInPlace to make another result in, but for one that is v, the
// operator's result; and marks v as its local's alone. A 32-bit machine's
// BigInts have no room in their header for their room, and are not given
// back.
func (r *Runtime) bigRelease(v, a, b, old Value, flags int) {
	if bits.UintSize != 64 {
		return
	}
	if flags&treeFreshA != 0 && a.isHeapBig() && a.ref != v.ref {
		r.bigGive((*BigInt)(a.ref))
	}
	if flags&treeFreshB != 0 && b.isHeapBig() && b.ref != v.ref && b.ref != a.ref {
		r.bigGive((*BigInt)(b.ref))
	}
	if flags&treeOwnRes != 0 {
		if old.isHeapBig() && old.ref != v.ref && (*BigInt)(old.ref).hdr&bigSoleBit != 0 &&
			(flags&treeFreshA == 0 || old.ref != a.ref) && (flags&treeFreshB == 0 || old.ref != b.ref) {
			r.bigGive((*BigInt)(old.ref))
		}
		if v.isHeapBig() {
			(*BigInt)(v.ref).hdr |= bigSoleBit
		}
	}
}

// bigGive keeps the block b for bigInPlace, if it has room for a list's.
func (r *Runtime) bigGive(b *BigInt) {
	c := int(uint32(b.hdr>>bigCapShift) & bigLenMask)
	if c < bigBlockFrom || c > bigFreeTop {
		return
	}
	f := r.bigFree
	if f == nil {
		f = new(bigFreeList)
		r.bigFree = f
	}
	if k := &f.n[c-bigBlockFrom]; *k < bigFreeDepth {
		f.b[c-bigBlockFrom][*k] = b
		*k++
	}
}

// treeFreshA and treeFreshB mark, in the pc a tree's arithmetic or bitwise
// node passes arithSlow or bitwiseSlow, its left and right operands as
// fresh: trees of arithmetic of their own, whose results nothing else
// takes, which a BigInt operator of the two may give back (bigRelease).
// treeOwnRes marks the node as one whose result is stored to a local that
// holds its values alone, the local treeOwnShift's bits number
// (zzbigown.go). treePCMask is the pc itself. A function whose pc is past
// it has no marks.
const (
	treePCMask   = 1<<20 - 1
	treeOwnShift = 20
	treeFreshA   = 1 << 26
	treeFreshB   = 1 << 27
	treeOwnRes   = 1 << 28
)

// treeOwned is the value of the local a pc that treeOwnRes marks names,
// which the result is about to be stored over.
func (c *tctx) treeOwned(pc int) Value {
	if pc&treeOwnRes == 0 {
		return Undefined
	}
	return c.locals[pc>>treeOwnShift&63]
}

// bigDone is a result made in bigInPlace's room as a value: held in its
// Value in the int64 range, the BigInt, its length the result's, if one was
// made, and a BigInt of a copy of it if not.
func bigDone(b *BigInt, neg bool, mag []uint) Value {
	if b == nil || len(mag) <= 64/bits.UintSize {
		return bigOf(neg, mag)
	}
	b.hdr = uint(len(mag)) | b.hdr&^(bigNegBit|bigLenMask)
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
	if bits.UintSize == 64 && a.isShortBig() && b.isShortBig() {
		if v, ok := shortOp(op, a, b); ok {
			return v, nil
		}
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
			return r.bigOfKept(an != bn, q), nil
		}
		// The remainder takes the dividend's sign.
		_, rem := bignum.DivMod(nil, r.bigRoom(len(y)), x, y)
		return r.bigOfKept(an, rem), nil
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
	if !b.isInt64Big() || b.int64Big() > maxBigIntBits || int64(bignum.BitLen(x)-1)*b.int64Big() > maxBigIntBits {
		return Undefined, r.throwBigIntSize()
	}
	e := uint64(b.int64Big())
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
	return bigOf(xn && b.int64Big()&1 != 0, acc), nil
}

// bigBits is the bitwise operator or shift op of two BigInts: a shift's
// count taken whole, a negative one shifting the other way; a RangeError
// for a left shift past maxBigIntBits.
func (r *Runtime) bigBits(op bytecode.Op, a, b Value) (Value, error) {
	if bits.UintSize == 64 && a.isShortBig() && b.isShortBig() {
		if v, ok := shortBits(op, a, b); ok {
			return v, nil
		}
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
	switch op {
	case bytecode.OpBitAnd:
		return r.bigOfKept(bignum.And(r.bigRoom(max(len(x), len(y))+1), an, x, bn, y)), nil
	case bytecode.OpBitOr:
		return r.bigOfKept(bignum.Or(r.bigRoom(max(len(x), len(y))+1), an, x, bn, y)), nil
	case bytecode.OpBitXor:
		return r.bigOfKept(bignum.Xor(r.bigRoom(max(len(x), len(y))+1), an, x, bn, y)), nil
	case bytecode.OpShl, bytecode.OpShr:
		left := op == bytecode.OpShl
		if bn {
			left = !left
		}
		if !b.isInt64Big() || b.int64Big() > maxBigIntBits || b.int64Big() < -maxBigIntBits {
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
		s := uint(b.int64Big())
		if bn {
			s = uint(-b.int64Big())
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
			return r.bigOfKept(an, bignum.Shl(r.bigRoom(bignum.ShlRoom(len(x), s)), x, s)), nil
		}
		return r.bigOfKept(bignum.Sar(r.bigRoom(bignum.SarRoom(len(x))), an, x, s)), nil
	}
	return Undefined, r.throwTypeError("unsupported BigInt operation")
}

// shortOp is the arithmetic operator op of two BigInts held in their
// Values, on a 64-bit machine: a sign and a magnitude below 2^64 each, the
// result one too, or two words on the heap -- without bignum's slices of
// words, for what most BigInts past the int64 range are, a 64-bit hash's
// or generator's. It reports false where bigOp must answer: a division by
// zero, a power.
func shortOp(op bytecode.Op, a, b Value) (Value, bool) {
	an, x := a.shortBigParts()
	bn, y := b.shortBigParts()
	switch op {
	case bytecode.OpSub:
		bn = !bn
		fallthrough
	case bytecode.OpAdd:
		if an == bn {
			s, c := bits.Add64(x, y, 0)
			if c != 0 {
				return bigTwoWords(an, s, 1), true
			}
			return shortBigMag(an, s), true
		}
		if x >= y {
			return shortBigMag(an, x-y), true
		}
		return shortBigMag(bn, y-x), true
	case bytecode.OpMul:
		hi, lo := bits.Mul64(x, y)
		if hi != 0 {
			return bigTwoWords(an != bn, lo, hi), true
		}
		return shortBigMag(an != bn, lo), true
	case bytecode.OpDiv:
		if y == 0 {
			return Undefined, false
		}
		return shortBigMag(an != bn, x/y), true
	case bytecode.OpMod:
		if y == 0 {
			return Undefined, false
		}
		// The remainder takes the dividend's sign.
		return shortBigMag(an, x%y), true
	}
	return Undefined, false
}

// shortBits is the bitwise operator or shift op of two BigInts held in their
// Values, on a 64-bit machine, as shortOp: the bitwise ones on 128 bits of
// two's complement, a shift by fewer than 64 on the magnitude. It reports
// false where bigBits must answer: a longer shift left.
func shortBits(op bytecode.Op, a, b Value) (Value, bool) {
	switch op {
	case bytecode.OpBitAnd, bytecode.OpBitOr, bytecode.OpBitXor:
		xh, xl := shortTwos(a)
		yh, yl := shortTwos(b)
		var h, l uint64
		switch op {
		case bytecode.OpBitAnd:
			h, l = xh&yh, xl&yl
		case bytecode.OpBitOr:
			h, l = xh|yh, xl|yl
		default:
			h, l = xh^yh, xl^yl
		}
		return shortOfTwos(h, l), true
	case bytecode.OpShl, bytecode.OpShr:
		if !b.isInt64Big() {
			return Undefined, false
		}
		// The count's magnitude as a uint64: the most negative int64's too.
		left, n := op == bytecode.OpShl, b.int64Big()
		s := uint64(n)
		if n < 0 {
			left, s = !left, -s
		}
		neg, x := a.shortBigParts()
		if left {
			if s >= 64 {
				return Undefined, false
			}
			hi, lo := x>>(64-s), x<<s
			if s == 0 {
				hi = 0
			}
			if hi != 0 {
				return bigTwoWords(neg, lo, hi), true
			}
			return shortBigMag(neg, lo), true
		}
		// Toward minus infinity: a magnitude below zero less one, shifted,
		// and one more.
		if !neg {
			if s >= 64 {
				return shortBig(0), true
			}
			return shortBigMag(false, x>>s), true
		}
		if s >= 64 {
			return shortBig(-1), true
		}
		return shortBigMag(true, (x-1)>>s+1), true
	}
	return Undefined, false
}

// shortTwos is a BigInt held in its Value as 128 bits of two's complement,
// high word and low: the low its 65 bits' low 64, the high their sign's.
func shortTwos(v Value) (uint64, uint64) {
	if v.isInt64Big() {
		x := v.int64Big()
		return uint64(x >> 63), uint64(x)
	}
	off := uint64(uintptr(v.ref) - uintptr(unsafe.Pointer(&shortBigHighX)))
	lo := off<<shortBigLowBits | math.Float64bits(v.num)&shortBigLowMask
	return -(off >> (64 - shortBigLowBits)), lo
}

// shortOfTwos is the BigInt of 128 bits of two's complement whose high word
// is its sign's, 0 or all ones: held in its Value, but for -2^64.
func shortOfTwos(h, l uint64) Value {
	if uint64(int64(l)>>63) == h {
		return shortBig(int64(l))
	}
	if h != 0 && l == 0 {
		return bigTwoWords(true, 0, 1)
	}
	// One past the int64 range: the low 64 bits and the 65th, the sign's.
	return Value{
		num: math.Float64frombits(shortBigBase | l&shortBigLowMask),
		ref: unsafe.Pointer(&shortBigHighX[l>>shortBigLowBits|(h&1)<<(64-shortBigLowBits)]),
	}
}

// shortOpOf is shortOp where a machine has it, a 64-bit one's: on another
// it reports false, for bigOp.
func shortOpOf(op bytecode.Op, a, b Value) (Value, bool) {
	if bits.UintSize != 64 {
		return Undefined, false
	}
	return shortOp(op, a, b)
}

// shortBitsOf is shortBits where a machine has it, a 64-bit one's, and
// shortBitwise's int64s on another.
func shortBitsOf(op bytecode.Op, a, b Value) (Value, bool) {
	if bits.UintSize != 64 {
		return shortBitwise(op, a, b)
	}
	return shortBits(op, a, b)
}

// bigTwoWords is the BigInt of sign neg and magnitude hi*2^64+lo, hi not
// zero, on a 64-bit machine: two words on the heap.
func bigTwoWords(neg bool, lo, hi uint64) Value {
	b := newBigWords(2, neg)
	w := b.words()
	w[0], w[1] = uint(lo), uint(hi)
	return heapBig(b)
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
	if a.isInt64Big() && b.isInt64Big() {
		x, y := a.int64Big(), b.int64Big()
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
	if v.isInt64Big() {
		return asciiString(strconv.AppendInt(b[:0], v.int64Big(), base))
	}
	if v.isShortBig() {
		neg, mag := v.shortBigParts()
		d := b[:0]
		if neg {
			d = append(d, '-')
		}
		return asciiString(strconv.AppendUint(d, mag, base))
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

// wrapsOperator reports whether a is an operator wrapCall makes
// BigInt.asUintN's and asIntN's of from its operands, over two that may be
// BigInts: not a number constant.
func wrapsOperator(a *arithOf) bool {
	if a.imm || a.x.number || a.y.number {
		return false
	}
	switch a.op {
	case bytecode.OpAdd, bytecode.OpSub, bytecode.OpMul,
		bytecode.OpBitAnd, bytecode.OpBitOr, bytecode.OpBitXor, bytecode.OpShl, bytecode.OpShr:
		return true
	}
	return false
}

// wrapShort is asIntN, signed, or asUintN of the width w and a op b, two
// BigInts held in their Values, made of the low 64 bits of their two's
// complement (wrapLow), as bigAsNCall would make it of a op b. It reports
// false where the call must be made: a width that is not an integral
// number of 0 to 64, an operator wrapLow does not make.
func wrapShort(signed bool, w Value, op bytecode.Op, a, b Value) (Value, bool) {
	n := w.num
	if !w.IsNumber() || !(n >= 0 && n <= 64) || n != float64(int(n)) {
		return Undefined, false
	}
	u, ok := wrapLow(op, a, b)
	if !ok {
		return Undefined, false
	}
	if n == 0 {
		return shortBig(0), true
	}
	return asN64(u, uint(n), signed), true
}

// wrapLow is the low 64 bits of a op b's two's complement, of two BigInts
// held in their Values: the operator on their own low 64 bits, but for a
// right shift, whose bits come from above as well, the sign's. It reports
// false for a shift of 64 bits or more, either way, which may throw.
func wrapLow(op bytecode.Op, a, b Value) (uint64, bool) {
	x, y := a.shortBigLow(), b.shortBigLow()
	switch op {
	case bytecode.OpAdd:
		return x + y, true
	case bytecode.OpSub:
		return x - y, true
	case bytecode.OpMul:
		return x * y, true
	case bytecode.OpBitAnd:
		return x & y, true
	case bytecode.OpBitOr:
		return x | y, true
	case bytecode.OpBitXor:
		return x ^ y, true
	case bytecode.OpShl, bytecode.OpShr:
		if !b.isInt64Big() {
			return 0, false
		}
		left, s := op == bytecode.OpShl, b.int64Big()
		if s < 0 {
			// A negative count shifts the other way.
			left, s = !left, -s
		}
		if s < 0 || s >= 64 {
			return 0, false
		}
		if left {
			return x << s, true
		}
		if s == 0 {
			return x, true
		}
		h, _ := shortTwos(a)
		return x>>s | h<<(64-s), true
	}
	return 0, false
}

// asN64 is asIntN, signed, or asUintN to a width of 1 to 64 bits of a
// BigInt whose two's complement's low 64 bits are u.
func asN64(u uint64, bits uint, signed bool) Value {
	if bits < 64 {
		u &= 1<<bits - 1
	}
	if signed {
		return shortBig(int64(u<<(64-bits)) >> (64 - bits))
	}
	return shortBigMag(false, u)
}

// wrapOperator is a op b for wrapCall where it is not made of the low bits:
// the operator's node's, its fast path for two numbers too.
func (c *tctx) wrapOperator(op bytecode.Op, a, b Value, pc int) Value {
	bitwise := isBitwise(op)
	if a.IsNumber() && b.IsNumber() {
		if bitwise {
			return int32Op(op, a.num, b.num)
		}
		return Float(numericOp(op, a.num, b.num))
	}
	if bitwise {
		return c.bitwiseSlow(op, a, b, pc)
	}
	return c.arithSlow(op, a, b, pc)
}

// wrapSeen is the mark arithSlow would have made for the JIT of the
// arithmetic a op b, two BigInts held in their Values, that wrapCall made
// of their low bits instead: one on the heap, or made there, unless both
// and the result are in the int64 range.
func wrapSeen(c *tctx, op bytecode.Op, a, b Value, pc int) {
	if isBitwise(op) {
		return
	}
	_, ok := shortArith(op, a, b)
	treeBigInt(c, pc&treePCMask, !ok)
}
