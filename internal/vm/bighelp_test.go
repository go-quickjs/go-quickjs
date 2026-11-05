package vm

import "math/big"

// math/big is the tests' oracle of BigInts, which the VM's code makes by
// bignum's.

// bigIntOf is b's value as a big.Int.
func bigIntOf(b *BigInt) *big.Int {
	w := b.words()
	z := new(big.Int)
	for i := len(w) - 1; i >= 0; i-- {
		z.Lsh(z, uint(bitsPerWord))
		z.Or(z, new(big.Int).SetUint64(uint64(w[i])))
	}
	if b.negative() {
		z.Neg(z)
	}
	return z
}

// blockOf is a BigInt of its own of z's value.
func blockOf(z *big.Int) *BigInt {
	w := z.Bits()
	b := newBigWords(len(w), z.Sign() < 0)
	// The words are read once, before the loop: Go 1.27 moves b.words()'s
	// sum, b plus a word, out of the loop and its guard, which for zero
	// would be past b's allocation (internal/cmd/hoistcheck).
	ws := b.words()
	for i, d := range w {
		ws[i] = uint(d)
	}
	return b
}

// valueOf is z as a Value: held in it in the int64 range.
func valueOf(z *big.Int) Value { return Big(blockOf(z)) }
