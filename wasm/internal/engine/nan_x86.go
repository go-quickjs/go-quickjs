//go:build amd64 || 386

package engine

// The NaNs of the float operations on amd64 and 386 (see nan.go): those of
// SSE's instructions, which of NaN operands return the first source
// operand's quieted -- the destination's, the operation's first operand
// -- else the second's. Their fused multiply-adds (FMA3's) return the
// first factor's, then the second's, then the addend's, the negation of
// nmadd's product leaving the NaN's sign as it is.

// pick32 is the NaN an operation of a and b returns, one of them a NaN.
func pick32(a, b float32) float32 {
	if a != a {
		b = a
	}
	return quiet32(b)
}

func pick64(a, b float64) float64 {
	if a != a {
		b = a
	}
	return quiet64(b)
}

// fmaPick32 is the NaN a fused a*b+c returns, or -(a*b)+c if neg, one of
// the operands a NaN.
func fmaPick32(a, b, c float32, neg bool) float32 {
	switch {
	case a != a:
		return quiet32(a)
	case b != b:
		return quiet32(b)
	}
	return quiet32(c)
}

func fmaPick64(a, b, c float64, neg bool) float64 {
	switch {
	case a != a:
		return quiet64(a)
	case b != b:
		return quiet64(b)
	}
	return quiet64(c)
}
