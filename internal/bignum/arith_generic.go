//go:build (!amd64 && !arm64) || purego

package bignum

import "math/bits"

// addVV writes x+y to z, all of len(z) words, and returns the carry.
func addVV(z, x, y []uint) uint {
	var c uint
	for i := range z {
		z[i], c = bits.Add(x[i], y[i], c)
	}
	return c
}

// subVV writes x-y to z, all of len(z) words, and returns the borrow.
func subVV(z, x, y []uint) uint {
	var b uint
	for i := range z {
		z[i], b = bits.Sub(x[i], y[i], b)
	}
	return b
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

// mulRow writes x*d to z, of len(x) words, and returns the carry word.
func mulRow(z, x []uint, d uint) uint {
	var c uint
	for i, xi := range x {
		hi, lo := bits.Mul(xi, d)
		var k uint
		z[i], k = bits.Add(lo, c, 0)
		c = hi + k
	}
	return c
}

// mulSubRow subtracts x*d from z, of len(x) words, and returns the word to
// subtract from the one above: the product's top and the borrow.
func mulSubRow(z, x []uint, d uint) uint {
	var c uint
	for i, xi := range x {
		hi, lo := bits.Mul(xi, d)
		var k uint
		lo, k = bits.Add(lo, c, 0)
		hi += k
		z[i], k = bits.Sub(z[i], lo, 0)
		c = hi + k
	}
	return c
}

// MulRow writes x*d to z, of len(x) words, and returns the carry word: the
// kernel, for callers that make their own rows (the VM's BigInt leaf).
func MulRow(z, x []uint, d uint) uint { return mulRow(z, x, d) }

// MulAddRow adds x*d to z, of len(x) words, and returns the carry word: the
// kernel, for callers that make their own rows.
func MulAddRow(z, x []uint, d uint) uint { return mulAddRow(z, x, d) }
