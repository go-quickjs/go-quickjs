// Package bignum is arbitrary-precision arithmetic on magnitudes: slices of
// words, least significant first, normalized -- no most significant zero
// word -- which the caller owns and gives the signs of, and which results
// are written into, so that a result is made where it will be kept, with
// nothing allocated but what an operation asks the caller for room in.
//
// It is the VM's BigInts' arithmetic. math/big's had cost more than C
// QuickJS's whole operation for numbers of a few words: its general entry,
// its own allocations, and a copy into the value that keeps the result.
// The words are uint, as math/big's are: 32 bits on a 32-bit machine.
//
// A result's room is the caller's: each operation says how many words it
// may write, and returns the normalized result, a prefix of the room.
// Unless an operation says it may, the room must not overlap an operand.
package bignum

import "math/bits"

// W is a word's size in bits.
const W = bits.UintSize

// Norm is x without its most significant zero words.
func Norm(x []uint) []uint {
	i := len(x)
	for i > 0 && x[i-1] == 0 {
		i--
	}
	return x[:i]
}

// Cmp compares two magnitudes: -1, 0 or +1.
func Cmp(x, y []uint) int {
	if len(x) != len(y) {
		if len(x) < len(y) {
			return -1
		}
		return 1
	}
	for i := len(x) - 1; i >= 0; i-- {
		if x[i] != y[i] {
			if x[i] < y[i] {
				return -1
			}
			return 1
		}
	}
	return 0
}

// Add writes x+y to z, which has room for max(len(x), len(y))+1 words and
// may be x or y.
func Add(z, x, y []uint) []uint {
	if len(x) < len(y) {
		x, y = y, x
	}
	var c uint
	i := 0
	for ; i < len(y); i++ {
		z[i], c = bits.Add(x[i], y[i], c)
	}
	for ; i < len(x); i++ {
		z[i], c = bits.Add(x[i], 0, c)
	}
	if c != 0 {
		z[i] = c
		return z[:i+1]
	}
	return z[:i]
}

// AddWord writes x+w to z, which has room for len(x)+1 words and may be x.
func AddWord(z, x []uint, w uint) []uint {
	c := w
	for i := range x {
		z[i], c = bits.Add(x[i], c, 0)
	}
	if c != 0 {
		z[len(x)] = c
		return z[:len(x)+1]
	}
	return Norm(z[:len(x)])
}

// Sub writes x-y to z, for x at least y: z has room for len(x) words and may
// be x or y.
func Sub(z, x, y []uint) []uint {
	var b uint
	i := 0
	for ; i < len(y); i++ {
		z[i], b = bits.Sub(x[i], y[i], b)
	}
	for ; i < len(x); i++ {
		z[i], b = bits.Sub(x[i], 0, b)
	}
	return Norm(z[:len(x)])
}

// SubWord writes x-w to z, for x at least w: z has room for len(x) words
// and may be x.
func SubWord(z, x []uint, w uint) []uint {
	b := w
	for i := range x {
		z[i], b = bits.Sub(x[i], b, 0)
	}
	return Norm(z[:len(x)])
}

// MulRoom is how many words Mul may write for operands of n and m words.
func MulRoom(n, m int) int { return n + m }

// Mul writes x*y to z, which has room for len(x)+len(y) words (MulRoom) and
// overlaps neither. Operands of many words are multiplied by Karatsuba's
// method, in scratch Mul allocates.
func Mul(z, x, y []uint) []uint {
	if len(x) < len(y) {
		x, y = y, x
	}
	if len(y) == 0 {
		return z[:0]
	}
	if len(y) >= karatsubaThreshold {
		return Norm(karatsubaMul(z[:len(x)+len(y)], x, y))
	}
	basicMul(z, x, y)
	return Norm(z[:len(x)+len(y)])
}

// basicMul writes x*y to z[:len(x)+len(y)], schoolbook, a row a word of y.
func basicMul(z, x, y []uint) {
	clear(z[:len(x)+len(y)])
	for j, d := range y {
		if d != 0 {
			z[len(x)+j] = mulAddRow(z[j:j+len(x)], x, d)
		}
	}
}

// mulAddRow adds x*d to z, of len(x) words, and returns the carry word.
func mulAddRow(z, x []uint, d uint) uint {
	var c uint
	for i, xi := range x {
		hi, lo := bits.Mul(xi, d)
		var k uint
		lo, k = bits.Add(lo, z[i], 0)
		hi += k
		lo, k = bits.Add(lo, c, 0)
		hi += k
		z[i], c = lo, hi
	}
	return c
}

// Sqr writes x*x to z, which has room for 2*len(x) words and overlaps x not:
// each product of two different words once, doubled, and the squares.
func Sqr(z, x []uint) []uint {
	n := len(x)
	if n == 0 {
		return z[:0]
	}
	if n >= karatsubaThreshold {
		return Norm(karatsubaMul(z[:2*n], x, x))
	}
	clear(z[:2*n])
	// The products of different words, x[i]*x[j] for i < j.
	for i := 0; i < n-1; i++ {
		if d := x[i]; d != 0 {
			z[n+i] = mulAddRow(z[2*i+1:n+i], x[i+1:], d)
		}
	}
	// Doubled.
	var c uint
	for i := range z[:2*n] {
		z[i], c = z[i]<<1|c, z[i]>>(W-1)
	}
	// And the squares.
	c = 0
	for i, d := range x {
		hi, lo := bits.Mul(d, d)
		var k uint
		z[2*i], k = bits.Add(z[2*i], lo, c)
		z[2*i+1], c = bits.Add(z[2*i+1], hi, k)
	}
	return Norm(z[:2*n])
}

// MulWord writes x*w to z, which has room for len(x)+1 words and may be x.
func MulWord(z, x []uint, w uint) []uint {
	var c uint
	for i, xi := range x {
		hi, lo := bits.Mul(xi, w)
		var k uint
		z[i], k = bits.Add(lo, c, 0)
		c = hi + k
	}
	z[len(x)] = c
	return Norm(z[:len(x)+1])
}

// MulAddWord writes x*w+a to z, which has room for len(x)+1 words and may
// be x.
func MulAddWord(z, x []uint, w, a uint) []uint {
	c := a
	for i, xi := range x {
		hi, lo := bits.Mul(xi, w)
		var k uint
		z[i], k = bits.Add(lo, c, 0)
		c = hi + k
	}
	z[len(x)] = c
	return Norm(z[:len(x)+1])
}

// karatsubaThreshold is the words of the shorter operand from which Mul
// multiplies by Karatsuba's method: math/big's, measured for its own
// schoolbook loop, about where the method's additions cost less than the
// products it saves.
const karatsubaThreshold = 40

// karatsubaMul writes x*y to z, len(x)+len(y) words, for len(x) >= len(y):
// the longer operand in pieces of the shorter's length, each piece's
// product by Karatsuba's method, added in.
func karatsubaMul(z, x, y []uint) []uint {
	clear(z)
	m := len(y)
	t := make([]uint, 2*m)
	for i := 0; i < len(x); i += m {
		p := x[i:min(i+m, len(x))]
		var pr []uint
		if len(p) == m {
			pr = karatsuba(t, p, y)
		} else {
			pr = Mul(t[:len(p)+m], y, p)
		}
		addAt(z, pr, i)
	}
	return z
}

// karatsuba writes x*y, both of n words, to z, 2*n words: with x = x1*B+x0
// and y = y1*B+y0, x*y = x1*y1*B^2 + ((x1+x0)*(y1+y0) - x1*y1 - x0*y0)*B
// + x0*y0, three products of half the length.
func karatsuba(z, x, y []uint) []uint {
	n := len(x)
	if n < karatsubaThreshold {
		basicMul(z, x, y)
		return z[:2*n]
	}
	h := n / 2
	x0, x1 := Norm(x[:h]), x[h:]
	y0, y1 := Norm(y[:h]), y[h:]
	clear(z[:2*n])
	lo := Mul(make([]uint, MulRoom(len(x0), len(y0))), x0, y0)
	hi := Mul(make([]uint, MulRoom(len(x1), len(y1))), Norm(x1), Norm(y1))
	sx := Add(make([]uint, len(x1)+1), Norm(x1), x0)
	sy := Add(make([]uint, len(y1)+1), Norm(y1), y0)
	mid := Mul(make([]uint, MulRoom(len(sx), len(sy))), sx, sy)
	mid = Sub(mid, mid, lo)
	mid = Sub(mid, mid, hi)
	addAt(z, lo, 0)
	addAt(z, mid, h)
	addAt(z, hi, 2*h)
	return z[:2*n]
}

// addAt adds x to z from word i on, z having room for the carry.
func addAt(z, x []uint, i int) {
	var c uint
	j := 0
	for ; j < len(x); j++ {
		z[i+j], c = bits.Add(z[i+j], x[j], c)
	}
	for ; c != 0; j++ {
		z[i+j], c = bits.Add(z[i+j], 0, c)
	}
}

// ShlRoom is how many words Shl may write for x of n words shifted by s.
func ShlRoom(n int, s uint) int { return n + int(s/W) + 1 }

// Shl writes x<<s to z, which has room for ShlRoom words and may be x when
// s is under a word.
func Shl(z, x []uint, s uint) []uint {
	if len(x) == 0 {
		return z[:0]
	}
	q, r := int(s/W), s%W
	n := len(x) + q
	if r == 0 {
		copy(z[q:n], x)
		clear(z[:q])
		return z[:n]
	}
	z[n] = x[len(x)-1] >> (W - r)
	for i := len(x) - 1; i > 0; i-- {
		z[q+i] = x[i]<<r | x[i-1]>>(W-r)
	}
	z[q] = x[0] << r
	clear(z[:q])
	return Norm(z[:n+1])
}

// Shr writes x>>s to z, which has room for len(x) words and may be x.
func Shr(z, x []uint, s uint) []uint {
	q, r := int(s/W), s%W
	if q >= len(x) {
		return z[:0]
	}
	n := len(x) - q
	if r == 0 {
		copy(z, x[q:])
		return z[:n]
	}
	for i := 0; i < n-1; i++ {
		z[i] = x[q+i]>>r | x[q+i+1]<<(W-r)
	}
	z[n-1] = x[len(x)-1] >> r
	return Norm(z[:n])
}

// BitLen is how many bits x has.
func BitLen(x []uint) int {
	if len(x) == 0 {
		return 0
	}
	return (len(x)-1)*W + bits.Len(x[len(x)-1])
}

// LowBitsZero reports whether x's bits below bit s are all zero.
func LowBitsZero(x []uint, s uint) bool {
	q, r := int(s/W), s%W
	for i := 0; i < q && i < len(x); i++ {
		if x[i] != 0 {
			return false
		}
	}
	return q >= len(x) || x[q]&(1<<r-1) == 0
}
