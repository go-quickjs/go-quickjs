//go:build !amd64 && !386

package engine

import (
	"math"
	"unsafe"
)

// The NaNs of the float operations on arm64, and on the architectures
// without native code (see nan.go): those of A64's instructions with the
// default-NaN mode off, as Go leaves it, which of NaN operands return a
// signaling one's quieted ahead of a quiet one's, and of two of a kind the
// first operand's. Their fused multiply-add (FMLA, Vd += Vn*Vm) takes the
// addend's NaN first, then the factors' in order, nmadd's first factor
// negated first, its NaN's sign with it; and of a quiet NaN addend and a
// product of an infinity and a zero it returns the default NaN.

const (
	defaultNaN32 = 0x7fc00000
	defaultNaN64 = 0x7ff8000000000000
)

// pick32 is the NaN an operation of a and b returns, one of them a NaN.
func pick32(a, b float32) float32 {
	x, y := *(*uint32)(unsafe.Pointer(&a)), *(*uint32)(unsafe.Pointer(&b))
	// b, if a is not a NaN, or b is signaling and a quiet.
	if b != b && (a == a || y&quietBit32 < x&quietBit32) {
		x = y
	}
	x |= quietBit32
	return *(*float32)(unsafe.Pointer(&x))
}

func pick64(a, b float64) float64 {
	x, y := *(*uint64)(unsafe.Pointer(&a)), *(*uint64)(unsafe.Pointer(&b))
	if b != b && (a == a || y&quietBit64 < x&quietBit64) {
		x = y
	}
	x |= quietBit64
	return *(*float64)(unsafe.Pointer(&x))
}

// fmaPick32 is the NaN a fused a*b+c returns, or -(a*b)+c if neg, one of
// the operands a NaN.
func fmaPick32(a, b, c float32, neg bool) float32 {
	x, y, z := math.Float32bits(a), math.Float32bits(b), math.Float32bits(c)
	if neg {
		x ^= 1 << 31
	}
	const inf = 0x7f800000
	switch {
	case c != c && z&quietBit32 == 0:
		return math.Float32frombits(z | quietBit32)
	case a != a && x&quietBit32 == 0:
		return math.Float32frombits(x | quietBit32)
	case b != b && y&quietBit32 == 0:
		return math.Float32frombits(y | quietBit32)
	case c != c:
		if (a == 0 && y&^(1<<31) == inf) || (b == 0 && x&^(1<<31) == inf) {
			return math.Float32frombits(defaultNaN32)
		}
		return c
	case a != a:
		return math.Float32frombits(x)
	}
	return b
}

func fmaPick64(a, b, c float64, neg bool) float64 {
	x, y, z := math.Float64bits(a), math.Float64bits(b), math.Float64bits(c)
	if neg {
		x ^= 1 << 63
	}
	const inf = 0x7ff0000000000000
	switch {
	case c != c && z&quietBit64 == 0:
		return math.Float64frombits(z | quietBit64)
	case a != a && x&quietBit64 == 0:
		return math.Float64frombits(x | quietBit64)
	case b != b && y&quietBit64 == 0:
		return math.Float64frombits(y | quietBit64)
	case c != c:
		if (a == 0 && y&^(1<<63) == inf) || (b == 0 && x&^(1<<63) == inf) {
			return math.Float64frombits(defaultNaN64)
		}
		return c
	case a != a:
		return math.Float64frombits(x)
	}
	return b
}
