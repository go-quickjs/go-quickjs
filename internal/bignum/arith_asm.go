//go:build (amd64 || arm64) && !purego

package bignum

// The kernels in assembly (arith_amd64.s, arith_arm64.s); arith_generic.go's
// are the same in Go, for every other machine, and for these built with
// purego.

// addVV writes x+y to z, all of len(z) words, and returns the carry.
//
//go:noescape
func addVV(z, x, y []uint) (c uint)

// subVV writes x-y to z, all of len(z) words, and returns the borrow.
//
//go:noescape
func subVV(z, x, y []uint) (b uint)

// mulAddRow adds x*d to z, of len(x) words, and returns the carry word.
//
//go:noescape
func mulAddRow(z, x []uint, d uint) (c uint)

// mulRow writes x*d to z, of len(x) words, and returns the carry word.
//
//go:noescape
func mulRow(z, x []uint, d uint) (c uint)

// mulSubRow subtracts x*d from z, of len(x) words, and returns the word to
// subtract from the one above: the product's top and the borrow.
//
//go:noescape
func mulSubRow(z, x []uint, d uint) (c uint)

// MulRow writes x*d to z, of len(x) words, and returns the carry word: the
// kernel, for callers that make their own rows (the VM's BigInt leaf).
func MulRow(z, x []uint, d uint) uint { return mulRow(z, x, d) }

// MulAddRow adds x*d to z, of len(x) words, and returns the carry word: the
// kernel, for callers that make their own rows.
func MulAddRow(z, x []uint, d uint) uint { return mulAddRow(z, x, d) }
