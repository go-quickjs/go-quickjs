package bignum

// The bitwise operators of integers, a sign and a magnitude each, as if in
// two's complement of infinite width -- what JavaScript's BigInt operators
// are -- by what each is of the magnitudes: for x below zero, ~x = |x|-1,
// so x & y = -(((|x|-1) | (|y|-1)) + 1) for both below zero, and the like.
// Each writes to z, which has room for max(len(x), len(y))+1 words and
// overlaps neither operand, and returns the result's sign and magnitude.

func andNat(z, x, y []uint) []uint {
	n := min(len(x), len(y))
	for i := 0; i < n; i++ {
		z[i] = x[i] & y[i]
	}
	return Norm(z[:n])
}

func orNat(z, x, y []uint) []uint {
	if len(x) < len(y) {
		x, y = y, x
	}
	for i := range y {
		z[i] = x[i] | y[i]
	}
	copy(z[len(y):len(x)], x[len(y):])
	return Norm(z[:len(x)])
}

func xorNat(z, x, y []uint) []uint {
	if len(x) < len(y) {
		x, y = y, x
	}
	for i := range y {
		z[i] = x[i] ^ y[i]
	}
	copy(z[len(y):len(x)], x[len(y):])
	return Norm(z[:len(x)])
}

// andNotNat is x &^ y.
func andNotNat(z, x, y []uint) []uint {
	n := min(len(x), len(y))
	for i := 0; i < n; i++ {
		z[i] = x[i] &^ y[i]
	}
	copy(z[n:len(x)], x[n:])
	return Norm(z[:len(x)])
}

// less1 is |x|-1 of a magnitude not zero, in scratch of its own.
func less1(x []uint) []uint {
	return SubWord(make([]uint, len(x)), x, 1)
}

// plus1 writes x+1 to z, which may be x and has room for len(x)+1 words.
func plus1(z, x []uint) []uint { return AddWord(z, x, 1) }

// And is x & y.
func And(z []uint, xneg bool, x []uint, yneg bool, y []uint) (bool, []uint) {
	switch {
	case !xneg && !yneg:
		return false, andNat(z, x, y)
	case xneg && yneg:
		return true, plus1(z, orNat(z, less1(x), less1(y)))
	case xneg:
		x, y = y, x
	}
	// x not below zero, y below it.
	return false, andNotNat(z, x, less1(y))
}

// Or is x | y.
func Or(z []uint, xneg bool, x []uint, yneg bool, y []uint) (bool, []uint) {
	switch {
	case !xneg && !yneg:
		return false, orNat(z, x, y)
	case xneg && yneg:
		return true, plus1(z, andNat(z, less1(x), less1(y)))
	case xneg:
		x, y = y, x
	}
	return true, plus1(z, andNotNat(z, less1(y), x))
}

// Xor is x ^ y.
func Xor(z []uint, xneg bool, x []uint, yneg bool, y []uint) (bool, []uint) {
	switch {
	case !xneg && !yneg:
		return false, xorNat(z, x, y)
	case xneg && yneg:
		return false, xorNat(z, less1(x), less1(y))
	case xneg:
		x, y = y, x
	}
	return true, plus1(z, xorNat(z, x, less1(y)))
}

// Not is ~x, -x-1: z has room for len(x)+1 words and may be x.
func Not(z []uint, xneg bool, x []uint) (bool, []uint) {
	if xneg {
		return false, SubWord(z, x, 1)
	}
	return true, plus1(z, x)
}

// SarRoom is how many words Sar may write for x of n words.
func SarRoom(n int) int { return n + 1 }

// Sar is x >> s, arithmetic: toward minus infinity, as a right shift of
// two's complement. z has room for SarRoom words and may be x.
func Sar(z []uint, xneg bool, x []uint, s uint) (bool, []uint) {
	if !xneg {
		return false, Shr(z, x, s)
	}
	if LowBitsZero(x, s) {
		// Exact: the magnitude shifted.
		r := Shr(z, x, s)
		return len(r) != 0, r
	}
	// -((|x|-1) >> s) - 1, which is -(|x| >> s) - 1 when bits are lost.
	r := plus1(z, Shr(z, x, s))
	return true, r
}
