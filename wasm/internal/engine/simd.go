package engine

import (
	"encoding/binary"
	"math"
)

//go:generate go run ./internal/simdgen ops_simd_gen.go

// The SIMD operators' lanes. A vector's lanes run from the low bytes of Lo
// to the high bytes of Hi; an operator unpacks its operands into arrays of
// lanes, computes each lane and packs the result.

func (v V128) u8() (r [16]uint8) {
	binary.LittleEndian.PutUint64(r[:8], v.Lo)
	binary.LittleEndian.PutUint64(r[8:], v.Hi)
	return r
}

func fromU8(r [16]uint8) V128 {
	return V128{binary.LittleEndian.Uint64(r[:8]), binary.LittleEndian.Uint64(r[8:])}
}

func (v V128) u16() (r [8]uint16) {
	for i := 0; i < 4; i++ {
		r[i] = uint16(v.Lo >> (16 * i))
		r[i+4] = uint16(v.Hi >> (16 * i))
	}
	return r
}

func fromU16(r [8]uint16) V128 {
	var lo, hi uint64
	for i := 0; i < 4; i++ {
		lo |= uint64(r[i]) << (16 * i)
		hi |= uint64(r[i+4]) << (16 * i)
	}
	return V128{lo, hi}
}

func (v V128) u32() [4]uint32 {
	return [4]uint32{uint32(v.Lo), uint32(v.Lo >> 32), uint32(v.Hi), uint32(v.Hi >> 32)}
}

func fromU32(r [4]uint32) V128 {
	return V128{uint64(r[0]) | uint64(r[1])<<32, uint64(r[2]) | uint64(r[3])<<32}
}

func (v V128) u64() [2]uint64 { return [2]uint64{v.Lo, v.Hi} }

func fromU64(r [2]uint64) V128 { return V128{r[0], r[1]} }

func (v V128) f32() [4]float32 {
	return [4]float32{
		math.Float32frombits(uint32(v.Lo)), math.Float32frombits(uint32(v.Lo >> 32)),
		math.Float32frombits(uint32(v.Hi)), math.Float32frombits(uint32(v.Hi >> 32)),
	}
}

func fromF32(r [4]float32) V128 {
	return V128{
		uint64(math.Float32bits(r[0])) | uint64(math.Float32bits(r[1]))<<32,
		uint64(math.Float32bits(r[2])) | uint64(math.Float32bits(r[3]))<<32,
	}
}

func (v V128) f64() [2]float64 {
	return [2]float64{math.Float64frombits(v.Lo), math.Float64frombits(v.Hi)}
}

func fromF64(r [2]float64) V128 {
	return V128{math.Float64bits(r[0]), math.Float64bits(r[1])}
}

// The comparisons' lanes: all ones if true.

func mask8(b bool) uint8 {
	if b {
		return 0xff
	}
	return 0
}

func mask16(b bool) uint16 {
	if b {
		return 0xffff
	}
	return 0
}

func mask32(b bool) uint32 {
	if b {
		return 0xffffffff
	}
	return 0
}

func mask64(b bool) uint64 {
	if b {
		return math.MaxUint64
	}
	return 0
}

// The saturations clamp an exact result to a lane's range.

func sat8s(x int) uint8 { return uint8(int8(max(min(x, math.MaxInt8), math.MinInt8))) }
func sat8u(x int) uint8 { return uint8(max(min(x, math.MaxUint8), 0)) }
func sat16s(x int) uint16 {
	return uint16(int16(max(min(x, math.MaxInt16), math.MinInt16)))
}
func sat16u(x int) uint16 { return uint16(max(min(x, math.MaxUint16), 0)) }

func abs8(a uint8) uint8 {
	if int8(a) < 0 {
		return -a
	}
	return a
}

func abs16(a uint16) uint16 {
	if int16(a) < 0 {
		return -a
	}
	return a
}

func abs32(a uint32) uint32 {
	if int32(a) < 0 {
		return -a
	}
	return a
}

func abs64(a uint64) uint64 {
	if int64(a) < 0 {
		return -a
	}
	return a
}

// pmin and pmax are the pseudo-minimum and maximum: b if it is below (above)
// a, else a, whatever their NaNs and zeros.

func pmin32(a, b float32) float32 {
	if b < a {
		return b
	}
	return a
}

func pmax32(a, b float32) float32 {
	if a < b {
		return b
	}
	return a
}

func pmin64(a, b float64) float64 {
	if b < a {
		return b
	}
	return a
}

func pmax64(a, b float64) float64 {
	if a < b {
		return b
	}
	return a
}

func fsqrt32(a float32) float32 { return float32(math.Sqrt(float64(a))) }

// The lane-wise sums and differences of 8, 16 and 32-bit lanes are computed
// on a 64-bit word at once: the top bit h of each lane is left out of the
// sum, so that no carry crosses a lane, and put back by its own xor.

const (
	hi8  = 0x8080808080808080
	hi16 = 0x8000800080008000
	hi32 = 0x8000000080000000
)

func addLanes(a, b, h uint64) uint64 { return ((a &^ h) + (b &^ h)) ^ ((a ^ b) & h) }

func subLanes(a, b, h uint64) uint64 { return ((a | h) - (b &^ h)) ^ ((a ^ ^b) & h) }

func vI8x16Add(x, y V128) V128 { return V128{addLanes(x.Lo, y.Lo, hi8), addLanes(x.Hi, y.Hi, hi8)} }
func vI16x8Add(x, y V128) V128 {
	return V128{addLanes(x.Lo, y.Lo, hi16), addLanes(x.Hi, y.Hi, hi16)}
}
func vI32x4Add(x, y V128) V128 {
	return V128{addLanes(x.Lo, y.Lo, hi32), addLanes(x.Hi, y.Hi, hi32)}
}
func vI64x2Add(x, y V128) V128 { return V128{x.Lo + y.Lo, x.Hi + y.Hi} }

func vI8x16Sub(x, y V128) V128 { return V128{subLanes(x.Lo, y.Lo, hi8), subLanes(x.Hi, y.Hi, hi8)} }
func vI16x8Sub(x, y V128) V128 {
	return V128{subLanes(x.Lo, y.Lo, hi16), subLanes(x.Hi, y.Hi, hi16)}
}
func vI32x4Sub(x, y V128) V128 {
	return V128{subLanes(x.Lo, y.Lo, hi32), subLanes(x.Hi, y.Hi, hi32)}
}
func vI64x2Sub(x, y V128) V128 { return V128{x.Lo - y.Lo, x.Hi - y.Hi} }

func vI8x16Neg(x V128) V128    { return vI8x16Sub(V128{}, x) }
func vI16x8Neg(x V128) V128    { return vI16x8Sub(V128{}, x) }
func vI32x4Neg(x V128) V128    { return vI32x4Sub(V128{}, x) }
func vI64x2Neg(x V128) V128    { return V128{-x.Lo, -x.Hi} }
func vI64x2Mul(x, y V128) V128 { return V128{x.Lo * y.Lo, x.Hi * y.Hi} }

// The bitwise operators.

func vAnd(x, y V128) V128    { return V128{x.Lo & y.Lo, x.Hi & y.Hi} }
func vOr(x, y V128) V128     { return V128{x.Lo | y.Lo, x.Hi | y.Hi} }
func vXor(x, y V128) V128    { return V128{x.Lo ^ y.Lo, x.Hi ^ y.Hi} }
func vAndNot(x, y V128) V128 { return V128{x.Lo &^ y.Lo, x.Hi &^ y.Hi} }
func vNot(x V128) V128       { return V128{^x.Lo, ^x.Hi} }
func vBitselect(x, y, m V128) V128 {
	return V128{x.Lo&m.Lo | y.Lo&^m.Lo, x.Hi&m.Hi | y.Hi&^m.Hi}
}

// The float signs are bits: abs and neg change no NaN's payload.

func vF32x4Abs(x V128) V128 { return V128{x.Lo &^ hi32, x.Hi &^ hi32} }
func vF32x4Neg(x V128) V128 { return V128{x.Lo ^ hi32, x.Hi ^ hi32} }
func vF64x2Abs(x V128) V128 { return V128{x.Lo &^ (1 << 63), x.Hi &^ (1 << 63)} }
func vF64x2Neg(x V128) V128 { return V128{x.Lo ^ 1<<63, x.Hi ^ 1<<63} }

// The tests.

func vAnyTrue(x V128) uint32 { return b2i(x.Lo|x.Hi != 0) }

func vI8x16AllTrue(x V128) uint32 {
	for _, l := range x.u8() {
		if l == 0 {
			return 0
		}
	}
	return 1
}

func vI16x8AllTrue(x V128) uint32 {
	for _, l := range x.u16() {
		if l == 0 {
			return 0
		}
	}
	return 1
}

func vI32x4AllTrue(x V128) uint32 {
	for _, l := range x.u32() {
		if l == 0 {
			return 0
		}
	}
	return 1
}

func vI64x2AllTrue(x V128) uint32 { return b2i(x.Lo != 0 && x.Hi != 0) }

func vI8x16BitMask(x V128) uint32 {
	var m uint32
	for i, l := range x.u8() {
		m |= uint32(l>>7) << i
	}
	return m
}

func vI16x8BitMask(x V128) uint32 {
	var m uint32
	for i, l := range x.u16() {
		m |= uint32(l>>15) << i
	}
	return m
}

func vI32x4BitMask(x V128) uint32 {
	var m uint32
	for i, l := range x.u32() {
		m |= (l >> 31) << i
	}
	return m
}

func vI64x2BitMask(x V128) uint32 { return uint32(x.Lo>>63 | x.Hi>>63<<1) }

// swizzle selects x's bytes by y's: an index past the last byte selects 0.
func vI8x16Swizzle(x, y V128) V128 {
	a, s := x.u8(), y.u8()
	var r [16]uint8
	for i, k := range s {
		if k < 16 {
			r[i] = a[k]
		}
	}
	return fromU8(r)
}

// The narrowings saturate x's lanes, then y's, to the half width.

func vI8x16NarrowS(x, y V128) V128 {
	a, b := x.u16(), y.u16()
	var r [16]uint8
	for i := 0; i < 8; i++ {
		r[i] = sat8s(int(int16(a[i])))
		r[i+8] = sat8s(int(int16(b[i])))
	}
	return fromU8(r)
}

func vI8x16NarrowU(x, y V128) V128 {
	a, b := x.u16(), y.u16()
	var r [16]uint8
	for i := 0; i < 8; i++ {
		r[i] = sat8u(int(int16(a[i])))
		r[i+8] = sat8u(int(int16(b[i])))
	}
	return fromU8(r)
}

func vI16x8NarrowS(x, y V128) V128 {
	a, b := x.u32(), y.u32()
	var r [8]uint16
	for i := 0; i < 4; i++ {
		r[i] = sat16s(int(int32(a[i])))
		r[i+4] = sat16s(int(int32(b[i])))
	}
	return fromU16(r)
}

func vI16x8NarrowU(x, y V128) V128 {
	a, b := x.u32(), y.u32()
	var r [8]uint16
	for i := 0; i < 4; i++ {
		r[i] = sat16u(int(int32(a[i])))
		r[i+4] = sat16u(int(int32(b[i])))
	}
	return fromU16(r)
}

// The extensions widen the low or high half of the lanes, starting at lane
// from.

func extend8S(x V128, from int) (r [8]uint16) {
	a := x.u8()
	for i := range r {
		r[i] = uint16(int8(a[from+i]))
	}
	return r
}

func extend8U(x V128, from int) (r [8]uint16) {
	a := x.u8()
	for i := range r {
		r[i] = uint16(a[from+i])
	}
	return r
}

func extend16S(x V128, from int) (r [4]uint32) {
	a := x.u16()
	for i := range r {
		r[i] = uint32(int16(a[from+i]))
	}
	return r
}

func extend16U(x V128, from int) (r [4]uint32) {
	a := x.u16()
	for i := range r {
		r[i] = uint32(a[from+i])
	}
	return r
}

func extend32S(x V128, from int) [2]uint64 {
	a := x.u32()
	return [2]uint64{uint64(int32(a[from])), uint64(int32(a[from+1]))}
}

func extend32U(x V128, from int) [2]uint64 {
	a := x.u32()
	return [2]uint64{uint64(a[from]), uint64(a[from+1])}
}

func vI16x8ExtendLowS(x V128) V128  { return fromU16(extend8S(x, 0)) }
func vI16x8ExtendHighS(x V128) V128 { return fromU16(extend8S(x, 8)) }
func vI16x8ExtendLowU(x V128) V128  { return fromU16(extend8U(x, 0)) }
func vI16x8ExtendHighU(x V128) V128 { return fromU16(extend8U(x, 8)) }
func vI32x4ExtendLowS(x V128) V128  { return fromU32(extend16S(x, 0)) }
func vI32x4ExtendHighS(x V128) V128 { return fromU32(extend16S(x, 4)) }
func vI32x4ExtendLowU(x V128) V128  { return fromU32(extend16U(x, 0)) }
func vI32x4ExtendHighU(x V128) V128 { return fromU32(extend16U(x, 4)) }
func vI64x2ExtendLowS(x V128) V128  { return fromU64(extend32S(x, 0)) }
func vI64x2ExtendHighS(x V128) V128 { return fromU64(extend32S(x, 2)) }
func vI64x2ExtendLowU(x V128) V128  { return fromU64(extend32U(x, 0)) }
func vI64x2ExtendHighU(x V128) V128 { return fromU64(extend32U(x, 2)) }

// The extending multiplications multiply the widened halves.

func mul16(a, b [8]uint16) V128 {
	var r [8]uint16
	for i := range r {
		r[i] = a[i] * b[i]
	}
	return fromU16(r)
}

func mul32(a, b [4]uint32) V128 {
	var r [4]uint32
	for i := range r {
		r[i] = a[i] * b[i]
	}
	return fromU32(r)
}

func mul64(a, b [2]uint64) V128 { return V128{a[0] * b[0], a[1] * b[1]} }

func vI16x8ExtMulLowS(x, y V128) V128  { return mul16(extend8S(x, 0), extend8S(y, 0)) }
func vI16x8ExtMulHighS(x, y V128) V128 { return mul16(extend8S(x, 8), extend8S(y, 8)) }
func vI16x8ExtMulLowU(x, y V128) V128  { return mul16(extend8U(x, 0), extend8U(y, 0)) }
func vI16x8ExtMulHighU(x, y V128) V128 { return mul16(extend8U(x, 8), extend8U(y, 8)) }
func vI32x4ExtMulLowS(x, y V128) V128  { return mul32(extend16S(x, 0), extend16S(y, 0)) }
func vI32x4ExtMulHighS(x, y V128) V128 { return mul32(extend16S(x, 4), extend16S(y, 4)) }
func vI32x4ExtMulLowU(x, y V128) V128  { return mul32(extend16U(x, 0), extend16U(y, 0)) }
func vI32x4ExtMulHighU(x, y V128) V128 { return mul32(extend16U(x, 4), extend16U(y, 4)) }
func vI64x2ExtMulLowS(x, y V128) V128  { return mul64(extend32S(x, 0), extend32S(y, 0)) }
func vI64x2ExtMulHighS(x, y V128) V128 { return mul64(extend32S(x, 2), extend32S(y, 2)) }
func vI64x2ExtMulLowU(x, y V128) V128  { return mul64(extend32U(x, 0), extend32U(y, 0)) }
func vI64x2ExtMulHighU(x, y V128) V128 { return mul64(extend32U(x, 2), extend32U(y, 2)) }

// The pairwise additions add each pair of adjacent lanes, widened.

func vI16x8ExtAddPairwiseS(x V128) V128 {
	a := x.u8()
	var r [8]uint16
	for i := range r {
		r[i] = uint16(int16(int8(a[2*i])) + int16(int8(a[2*i+1])))
	}
	return fromU16(r)
}

func vI16x8ExtAddPairwiseU(x V128) V128 {
	a := x.u8()
	var r [8]uint16
	for i := range r {
		r[i] = uint16(a[2*i]) + uint16(a[2*i+1])
	}
	return fromU16(r)
}

func vI32x4ExtAddPairwiseS(x V128) V128 {
	a := x.u16()
	var r [4]uint32
	for i := range r {
		r[i] = uint32(int32(int16(a[2*i])) + int32(int16(a[2*i+1])))
	}
	return fromU32(r)
}

func vI32x4ExtAddPairwiseU(x V128) V128 {
	a := x.u16()
	var r [4]uint32
	for i := range r {
		r[i] = uint32(a[2*i]) + uint32(a[2*i+1])
	}
	return fromU32(r)
}

// vI32x4DotS sums the products of each pair of signed 16-bit lanes, wrapping.
func vI32x4DotS(x, y V128) V128 {
	a, b := x.u16(), y.u16()
	var r [4]uint32
	for i := range r {
		p0 := int32(int16(a[2*i])) * int32(int16(b[2*i]))
		p1 := int32(int16(a[2*i+1])) * int32(int16(b[2*i+1]))
		r[i] = uint32(p0 + p1)
	}
	return fromU32(r)
}

// The conversions between the shapes of different lane counts.

func vF64x2ConvertLowS(x V128) V128 {
	a := x.u32()
	return fromF64([2]float64{float64(int32(a[0])), float64(int32(a[1]))})
}

func vF64x2ConvertLowU(x V128) V128 {
	a := x.u32()
	return fromF64([2]float64{float64(a[0]), float64(a[1])})
}

func vF32x4DemoteZero(x V128) V128 {
	a := x.f64()
	return fromF32([4]float32{float32(a[0]), float32(a[1])})
}

func vF64x2PromoteLow(x V128) V128 {
	a := x.f32()
	return fromF64([2]float64{float64(a[0]), float64(a[1])})
}

func vI32x4TruncSatF64x2SZero(x V128) V128 {
	a := x.f64()
	return fromU32([4]uint32{satS32F64(a[0]), satS32F64(a[1])})
}

func vI32x4TruncSatF64x2UZero(x V128) V128 {
	a := x.f64()
	return fromU32([4]uint32{satU32F64(a[0]), satU32F64(a[1])})
}

// The lanes of a given width at a lane index: the word that holds the lane,
// and the lane's shift in it.

func laneAt(lane, bits int) (hi bool, shift uint) {
	pos := lane * bits
	return pos >= 64, uint(pos % 64)
}

// getLane returns the lane of the given width at word hi and shift.
func getLane(v V128, hi bool, shift uint, mask uint64) uint64 {
	w := v.Lo
	if hi {
		w = v.Hi
	}
	return w >> shift & mask
}

// setLane returns v with the lane at word hi and shift replaced by x.
func setLane(v V128, hi bool, shift uint, mask, x uint64) V128 {
	m := mask << shift
	x = (x & mask) << shift
	if hi {
		v.Hi = v.Hi&^m | x
	} else {
		v.Lo = v.Lo&^m | x
	}
	return v
}
