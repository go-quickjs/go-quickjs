package vm

import "math"

// Half-precision floats.
//
// Float16Array and DataView's getFloat16 and setFloat16 store a number in two
// bytes: one sign bit, five exponent bits and ten of mantissa. That is about
// three decimal digits, which is enough for the things half floats are for --
// image and audio samples, and the weights of a model -- and half the space of
// a Float32Array.
//
// Go has no float16, so the conversions are written out. Rounding is
// round-to-nearest-even, the same rule every other float operation uses, which
// is what keeps the result independent of how the value got here.

const (
	// f16Bias and f32Bias are the exponent offsets of the two formats.
	f16Bias = 15
	f32Bias = 127
)

// float16frombits widens a half to a float64.
func float16frombits(h uint16) float64 {
	sign := uint32(h>>15) << 31
	exp := int32(h>>10) & 0x1F
	frac := uint32(h) & 0x3FF

	switch {
	case exp == 0x1F:
		// Infinity or NaN, which widen to the same.
		return float64(math.Float32frombits(sign | 0xFF<<23 | frac<<13))
	case exp == 0:
		if frac == 0 {
			return float64(math.Float32frombits(sign))
		}
		// Subnormal at half precision is normal at single, so the leading one
		// has to be found and the exponent adjusted for it.
		shift := uint32(0)
		for frac&0x400 == 0 {
			frac <<= 1
			shift++
		}
		frac &= 0x3FF
		e := uint32(f32Bias-f16Bias+1-int32(shift)) << 23
		return float64(math.Float32frombits(sign | e | frac<<13))
	default:
		e := uint32(exp-f16Bias+f32Bias) << 23
		return float64(math.Float32frombits(sign | e | frac<<13))
	}
}

// float16bits narrows a number to a half, rounding to nearest even.
func float16bits(f float64) uint16 {
	// Narrowing through float32 first is exact: every half is a float32, and
	// double rounding cannot occur because a float32 has more than twice the
	// mantissa bits of a half plus the guard bits the second rounding needs.
	b := math.Float32bits(float32(f))
	sign := uint16(b>>31) << 15
	exp := int32(b>>23&0xFF) - f32Bias
	frac := b & 0x7FFFFF

	switch {
	case exp == 0x80:
		// Infinity keeps its sign; NaN keeps a payload bit so that it stays a
		// NaN rather than becoming an infinity.
		if frac != 0 {
			return sign | 0x7C00 | 0x200
		}
		return sign | 0x7C00
	case exp >= 16:
		// Too large to represent, which rounds to infinity rather than
		// wrapping.
		return sign | 0x7C00
	case exp >= -14:
		// Normal: ten mantissa bits, with the thirteen dropped ones deciding
		// whether to round up.
		half := uint16(uint32(exp+f16Bias)<<10 | frac>>13)
		if roundsUp(frac, 13, uint32(half)) {
			half++
		}
		return sign | half
	case exp >= -25:
		// Subnormal: the implicit leading one becomes explicit, and the shift
		// grows as the exponent falls.
		m := frac | 0x800000
		shift := uint32(-exp - 14 + 13)
		half := uint16(m >> shift)
		if roundsUp(m, shift, uint32(half)) {
			half++
		}
		return sign | half
	default:
		// Smaller than the smallest subnormal. Exactly half of it rounds to
		// even, which is zero.
		return sign
	}
}

// roundsUp reports whether dropping the low shift bits of m should round the
// result up, under round-to-nearest-even.
func roundsUp(m, shift, kept uint32) bool {
	dropped := m & (1<<shift - 1)
	halfway := uint32(1) << (shift - 1)
	if dropped > halfway {
		return true
	}
	// Exactly halfway goes to the even neighbour.
	return dropped == halfway && kept&1 == 1
}
