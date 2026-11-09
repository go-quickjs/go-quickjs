package engine

import (
	"math"
	"math/bits"
)

//go:generate go run ./internal/numgen ops_num_gen.go

// The numeric operators' semantics where Go's operators differ from wasm's.

func b2i(b bool) uint32 {
	if b {
		return 1
	}
	return 0
}

func divS32(a, b uint32) uint32 {
	if b == 0 {
		trap(TrapDivByZero)
	}
	if int32(a) == math.MinInt32 && int32(b) == -1 {
		trap(TrapDivUnrepresentable)
	}
	return uint32(int32(a) / int32(b))
}

func divU32(a, b uint32) uint32 {
	if b == 0 {
		trap(TrapDivByZero)
	}
	return a / b
}

func remS32(a, b uint32) uint32 {
	if b == 0 {
		trap(TrapRemByZero)
	}
	if int32(b) == -1 {
		return 0
	}
	return uint32(int32(a) % int32(b))
}

func remU32(a, b uint32) uint32 {
	if b == 0 {
		trap(TrapRemByZero)
	}
	return a % b
}

func divS64(a, b uint64) uint64 {
	if b == 0 {
		trap(TrapDivByZero)
	}
	if int64(a) == math.MinInt64 && int64(b) == -1 {
		trap(TrapDivUnrepresentable)
	}
	return uint64(int64(a) / int64(b))
}

func divU64(a, b uint64) uint64 {
	if b == 0 {
		trap(TrapDivByZero)
	}
	return a / b
}

func remS64(a, b uint64) uint64 {
	if b == 0 {
		trap(TrapRemByZero)
	}
	if int64(b) == -1 {
		return 0
	}
	return uint64(int64(a) % int64(b))
}

func remU64(a, b uint64) uint64 {
	if b == 0 {
		trap(TrapRemByZero)
	}
	return a % b
}

// fmin32 and the rest are min and max: NaN if either operand is, and -0
// below +0.
func fmin32(a, b float32) float32 {
	if a != a || b != b {
		return a + b
	}
	if a == b {
		return math.Float32frombits(math.Float32bits(a) | math.Float32bits(b))
	}
	if a < b {
		return a
	}
	return b
}

func fmax32(a, b float32) float32 {
	if a != a || b != b {
		return a + b
	}
	if a == b {
		return math.Float32frombits(math.Float32bits(a) & math.Float32bits(b))
	}
	if a > b {
		return a
	}
	return b
}

func fmin64(a, b float64) float64 {
	if a != a || b != b {
		return a + b
	}
	if a == b {
		return math.Float64frombits(math.Float64bits(a) | math.Float64bits(b))
	}
	if a < b {
		return a
	}
	return b
}

func fmax64(a, b float64) float64 {
	if a != a || b != b {
		return a + b
	}
	if a == b {
		return math.Float64frombits(math.Float64bits(a) & math.Float64bits(b))
	}
	if a > b {
		return a
	}
	return b
}

// The roundings return a quiet NaN for a NaN, which math's return as it is.

func fceil32(a float32) float32 {
	if a != a {
		return a + a
	}
	return float32(math.Ceil(float64(a)))
}

func ffloor32(a float32) float32 {
	if a != a {
		return a + a
	}
	return float32(math.Floor(float64(a)))
}

func ftrunc32(a float32) float32 {
	if a != a {
		return a + a
	}
	return float32(math.Trunc(float64(a)))
}

func fnearest32(a float32) float32 {
	if a != a {
		return a + a
	}
	return float32(math.RoundToEven(float64(a)))
}

func fceil64(a float64) float64 {
	if a != a {
		return a + a
	}
	return math.Ceil(a)
}

func ffloor64(a float64) float64 {
	if a != a {
		return a + a
	}
	return math.Floor(a)
}

func ftrunc64(a float64) float64 {
	if a != a {
		return a + a
	}
	return math.Trunc(a)
}

func fnearest64(a float64) float64 {
	if a != a {
		return a + a
	}
	return math.RoundToEven(a)
}

// The truncations trap on NaN and on a value out of the result's range.

func truncS32F32(a float32) uint32 {
	if !(a >= -2147483648.0 && a < 2147483648.0) {
		trap(TrapFloatUnrepresentable)
	}
	return uint32(int32(a))
}

func truncU32F32(a float32) uint32 {
	if !(a > -1.0 && a < 4294967296.0) {
		trap(TrapFloatUnrepresentable)
	}
	return uint32(a)
}

func truncS32F64(a float64) uint32 {
	if !(a > -2147483649.0 && a < 2147483648.0) {
		trap(TrapFloatUnrepresentable)
	}
	return uint32(int32(a))
}

func truncU32F64(a float64) uint32 {
	if !(a > -1.0 && a < 4294967296.0) {
		trap(TrapFloatUnrepresentable)
	}
	return uint32(a)
}

func truncS64F32(a float32) uint64 {
	if !(a >= -9223372036854775808.0 && a < 9223372036854775808.0) {
		trap(TrapFloatUnrepresentable)
	}
	return uint64(int64(a))
}

func truncU64F32(a float32) uint64 {
	if !(a > -1.0 && a < 18446744073709551616.0) {
		trap(TrapFloatUnrepresentable)
	}
	return f64ToU64(float64(a))
}

func truncS64F64(a float64) uint64 {
	if !(a >= -9223372036854775808.0 && a < 9223372036854775808.0) {
		trap(TrapFloatUnrepresentable)
	}
	return uint64(int64(a))
}

func truncU64F64(a float64) uint64 {
	if !(a > -1.0 && a < 18446744073709551616.0) {
		trap(TrapFloatUnrepresentable)
	}
	return f64ToU64(a)
}

// f64ToU64 converts a float in [0, 2^64) to an integer, truncating.
func f64ToU64(a float64) uint64 {
	if a < 9223372036854775808.0 {
		return uint64(int64(a))
	}
	return uint64(int64(a-9223372036854775808.0)) | 1<<63
}

// The saturating truncations clamp to the result's range, NaN to 0.

func satS32F32(a float32) uint32 { return satS32F64(float64(a)) }
func satU32F32(a float32) uint32 { return satU32F64(float64(a)) }

func satS32F64(a float64) uint32 {
	switch {
	case a != a:
		return 0
	case a <= -2147483648.0:
		return 0x80000000
	case a >= 2147483647.0:
		return 0x7fffffff
	}
	return uint32(int32(a))
}

func satU32F64(a float64) uint32 {
	switch {
	case a != a || a <= -1.0:
		return 0
	case a >= 4294967295.0:
		return 0xffffffff
	}
	return uint32(a)
}

func satS64F32(a float32) uint64 { return satS64F64(float64(a)) }
func satU64F32(a float32) uint64 { return satU64F64(float64(a)) }

func satS64F64(a float64) uint64 {
	switch {
	case a != a:
		return 0
	case a <= -9223372036854775808.0:
		return 1 << 63
	case a >= 9223372036854775808.0:
		return 1<<63 - 1
	}
	return uint64(int64(a))
}

func satU64F64(a float64) uint64 {
	switch {
	case a != a || a <= -1.0:
		return 0
	case a >= 18446744073709551616.0:
		return math.MaxUint64
	}
	return f64ToU64(a)
}

// u64ToF32 converts an unsigned integer to the nearest float32, rounding
// once: a value at 2^63 or above is halved, keeping its lowest bit as a
// sticky bit, converted and doubled.
func u64ToF32(a uint64) float32 {
	if int64(a) >= 0 {
		return i64ToF32(int64(a))
	}
	return i64ToF32(int64(a>>1|a&1)) * 2
}

func u64ToF64(a uint64) float64 {
	if int64(a) >= 0 {
		return float64(int64(a))
	}
	return float64(int64(a>>1|a&1)) * 2
}

// i64ToF32 converts an integer to the nearest float32, rounding once. A
// conversion through float64 rounds twice, and a value whose low bits
// decide it would round differently, so the bits below the float64's
// precision are folded into a sticky bit first.
func i64ToF32(a int64) float32 {
	u := uint64(a)
	if a < 0 {
		u = -u
	}
	if u >= 1<<53 {
		// Keep 53 significant bits, the rest as a sticky bit.
		shift := 64 - 53 - bits.LeadingZeros64(u)
		if shift > 0 {
			sticky := u & (1<<shift - 1)
			u >>= shift
			if sticky != 0 {
				u |= 1
			}
			u <<= shift
		}
	}
	f := float32(float64(u))
	if a < 0 {
		return -f
	}
	return f
}
