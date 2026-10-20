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
	if len(y) < 4 {
		// Fewer words than a call of the kernel costs.
		for i := range y {
			z[i], c = bits.Add(x[i], y[i], c)
		}
	} else {
		c = addVV(z[:len(y)], x[:len(y)], y)
	}
	i := len(y)
	for ; i < len(x) && c != 0; i++ {
		z[i], c = bits.Add(x[i], 0, c)
	}
	if i < len(x) {
		copy(z[i:len(x)], x[i:])
		return z[:len(x)]
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
	b := subVV(z[:len(y)], x[:len(y)], y)
	i := len(y)
	for ; i < len(x) && b != 0; i++ {
		z[i], b = bits.Sub(x[i], 0, b)
	}
	copy(z[i:len(x)], x[i:])
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

// basicMul writes x*y to z[:len(x)+len(y)], schoolbook, a row a word of y:
// the first row written, not added to zeros.
func basicMul(z, x, y []uint) {
	n := len(x)
	if len(y) == 0 {
		clear(z[:n])
		return
	}
	z[n] = mulRow(z[:n], x, y[0])
	clear(z[n+1 : n+len(y)])
	for j := 1; j < len(y); j++ {
		if d := y[j]; d != 0 {
			z[n+j] = mulAddRow(z[j:j+n], x, d)
		}
	}
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
	if n < sqrThreshold {
		// A product of the rows takes less than the products of different
		// words once, doubled, and the squares, below math/big's threshold.
		basicMul(z, x, x)
		return Norm(z[:2*n])
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

// sqrThreshold is the words from which Sqr squares, not multiplies.
const sqrThreshold = 20

// karatsubaMul writes x*y to z, len(x)+len(y) words, for len(x) >= len(y):
// the longer operand in pieces of the shorter's length, each piece's
// product by Karatsuba's method, added in, with scratch of its own.
func karatsubaMul(z, x, y []uint) []uint {
	clear(z)
	m := len(y)
	t := make([]uint, 2*m+karatsubaScratch(m))
	p2, rest := t[:2*m], t[2*m:]
	for i := 0; i < len(x); i += m {
		p := x[i:min(i+m, len(x))]
		var pr []uint
		if len(p) == m {
			pr = karatsuba(p2, p, y, rest)
		} else {
			pr = Mul(p2[:len(p)+m], y, p)
		}
		addAt(z, pr, i)
	}
	return z
}

// karatsubaScratch is the scratch karatsuba asks of n words.
func karatsubaScratch(n int) int {
	if n < karatsubaThreshold {
		return 0
	}
	k := n - n/2 + 1
	return 6*k + karatsubaScratch(k)
}

// karatsuba writes x*y, both of n words, zeros on top allowed, to z, 2*n
// words: with x = x1*B+x0 and y = y1*B+y0, B the half's words' base,
// x*y = x1*y1*B^2 + ((x1+x0)*(y1+y0) - x1*y1 - x0*y0)*B + x0*y0, three
// products of halves, in t, of karatsubaScratch(n) words.
func karatsuba(z, x, y, t []uint) []uint {
	n := len(x)
	if n < karatsubaThreshold {
		basicMul(z, x, y)
		return z[:2*n]
	}
	h := n / 2
	k := n - h + 1
	x0, x1, y0, y1 := x[:h], x[h:], y[:h], y[h:]
	// The sums, each of k words, its top the carry.
	sx, sy, mid := t[:k], t[k:2*k], t[2*k:4*k]
	rest := t[6*k:]
	copy(sx, x1)
	sx[k-1] = 0
	sx[k-1] = addVV(sx[:h], sx[:h], x0)
	if h < k-1 {
		// x1 is a word longer than x0: the carry into it.
		var c uint
		sx[h], c = bits.Add(sx[h], sx[k-1], 0)
		sx[k-1] = c
	}
	copy(sy, y1)
	sy[k-1] = 0
	sy[k-1] = addVV(sy[:h], sy[:h], y0)
	if h < k-1 {
		var c uint
		sy[h], c = bits.Add(sy[h], sy[k-1], 0)
		sy[k-1] = c
	}
	karatsuba(mid, sx, sy, rest)
	// The outer products in z: x0*y0 in the low 2h words, x1*y1 above.
	karatsuba(z[:2*h], x0, y0, rest)
	karatsuba(z[2*h:2*n], x1, y1, rest)
	// mid -= x0*y0, x1*y1: both under it, which is 2k words.
	if subVV(mid[:2*h], mid[:2*h], z[:2*h]) != 0 {
		subOne(mid[2*h:])
	}
	if subVV(mid[:2*(n-h)], mid[:2*(n-h)], z[2*h:2*n]) != 0 {
		subOne(mid[2*(n-h):])
	}
	addAt(z, Norm(mid), h)
	return z[:2*n]
}

// subOne subtracts one from z, which is not zero.
func subOne(z []uint) {
	for i := range z {
		z[i]--
		if z[i] != ^uint(0) {
			return
		}
	}
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
