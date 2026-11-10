//go:build amd64 || 386

package engine

import (
	"runtime"

	"golang.org/x/sys/cpu"
)

// The relaxed SIMD operators on amd64 and 386 give what the SSE and AVX
// instructions that compute them give:
//
//   - relaxed_swizzle is pshufb: an index byte with its top bit set selects
//     0, any other selects the byte its low four bits number.
//   - relaxed_trunc_f32x4_s and relaxed_trunc_f64x2_s_zero are cvttps2dq and
//     cvttpd2dq: a NaN or a value out of range gives 0x80000000. The
//     unsigned ones saturate, as trunc_sat does.
//   - relaxed_madd and relaxed_nmadd are fused, one rounding, on a CPU with
//     FMA3 and the AVX state enabled; else a multiplication and an addition
//     (a subtraction from the addend), each rounded.
//   - relaxed_laneselect is pblendvb for 8 and 16-bit lanes, blendvps for 32
//     and blendvpd for 64: the top bit of each byte, or of each 32 or 64-bit
//     lane, of the mask selects the first operand's, else the second's.
//   - relaxed_min and relaxed_max are minps and maxps (minpd and maxpd):
//     the first operand if it is below (above) the second, else the second,
//     so a NaN in either operand or two zeros give the second.
//   - relaxed_q15mulr_s is pmulhrsw: (a*b + 0x4000) >> 15 without
//     saturation, so -32768 * -32768 gives -32768.
//   - relaxed_dot_i8x16_i7x16_s is pmaddubsw with the second operand's bytes
//     unsigned: the sum of each pair of products saturated to 16 bits.
//   - relaxed_dot_i8x16_i7x16_add_s on a CPU with AVX-VNNI-INT8, on amd64,
//     is vpdpbssd: the four products of signed bytes summed in 32 bits;
//     with AVX-VNNI, on amd64, vpdpbusd: the same with the second operand's
//     bytes unsigned; else pmaddubsw, pmaddwd and paddd: the pairs of
//     products saturated to 16 bits as above, then summed in 32.

var (
	relaxedFused     = cpu.X86.HasFMA && cpu.X86.HasAVX
	relaxedDotSigned = runtime.GOARCH == "amd64" && cpu.X86.HasAVXVNNIInt8
	relaxedDotVNNI   = runtime.GOARCH == "amd64" && cpu.X86.HasAVXVNNI
)

func relaxedSwizzle(x, y V128) V128 {
	a, s := x.u8(), y.u8()
	var r [16]uint8
	for i, k := range s {
		if k&0x80 == 0 {
			r[i] = a[k&15]
		}
	}
	return fromU8(r)
}

// cvtt32 converts as cvttss2si: NaN and values out of range give
// 0x80000000.
func cvtt32(a float64) uint32 {
	if !(a > -2147483649.0 && a < 2147483648.0) {
		return 0x80000000
	}
	return uint32(int32(a))
}

func relaxedTruncF32x4S(x V128) V128 {
	a := x.f32()
	var r [4]uint32
	for i := range r {
		r[i] = cvtt32(float64(a[i]))
	}
	return fromU32(r)
}

func relaxedTruncF32x4U(x V128) V128 { return vI32x4UConvertF32x4(x) }

func relaxedTruncF64x2SZero(x V128) V128 {
	a := x.f64()
	return fromU32([4]uint32{cvtt32(a[0]), cvtt32(a[1])})
}

func relaxedTruncF64x2UZero(x V128) V128 { return vI32x4TruncSatF64x2UZero(x) }

func relaxedMaddF32x4(x, y, z V128) V128 {
	if relaxedFused {
		return fusedMaddF32x4(x, y, z)
	}
	a, b, c := x.f32(), y.f32(), z.f32()
	var r [4]float32
	for i := range r {
		r[i] = fadd32(fmul32(a[i], b[i]), c[i])
	}
	return fromF32(r)
}

func relaxedNmaddF32x4(x, y, z V128) V128 {
	if relaxedFused {
		return fusedNmaddF32x4(x, y, z)
	}
	a, b, c := x.f32(), y.f32(), z.f32()
	var r [4]float32
	for i := range r {
		r[i] = fsub32(c[i], fmul32(a[i], b[i]))
	}
	return fromF32(r)
}

func relaxedMaddF64x2(x, y, z V128) V128 {
	if relaxedFused {
		return fusedMaddF64x2(x, y, z)
	}
	a, b, c := x.f64(), y.f64(), z.f64()
	return fromF64([2]float64{fadd64(fmul64(a[0], b[0]), c[0]), fadd64(fmul64(a[1], b[1]), c[1])})
}

func relaxedNmaddF64x2(x, y, z V128) V128 {
	if relaxedFused {
		return fusedNmaddF64x2(x, y, z)
	}
	a, b, c := x.f64(), y.f64(), z.f64()
	return fromF64([2]float64{fsub64(c[0], fmul64(a[0], b[0])), fsub64(c[1], fmul64(a[1], b[1]))})
}

// blendMask returns the mask selecting whole lanes of the given top bits:
// each lane whose top bit is set becomes all ones.
func blendMask(m, top uint64, width uint) uint64 {
	t := (m & top) >> (width - 1)
	return t * (1<<width - 1)
}

func relaxedLaneselect8(x, y, m V128) V128 {
	return vBitselect(x, y, V128{blendMask(m.Lo, hi8, 8), blendMask(m.Hi, hi8, 8)})
}

// The 16-bit lanes are selected by byte, as pblendvb selects them.
func relaxedLaneselect16(x, y, m V128) V128 { return relaxedLaneselect8(x, y, m) }

func relaxedLaneselect32(x, y, m V128) V128 {
	return vBitselect(x, y, V128{blendMask(m.Lo, hi32, 32), blendMask(m.Hi, hi32, 32)})
}

func relaxedLaneselect64(x, y, m V128) V128 {
	return vBitselect(x, y, V128{uint64(int64(m.Lo) >> 63), uint64(int64(m.Hi) >> 63)})
}

func relaxedMinF32x4(x, y V128) V128 {
	a, b := x.f32(), y.f32()
	var r [4]float32
	for i := range r {
		r[i] = b[i]
		if a[i] < b[i] {
			r[i] = a[i]
		}
	}
	return fromF32(r)
}

func relaxedMaxF32x4(x, y V128) V128 {
	a, b := x.f32(), y.f32()
	var r [4]float32
	for i := range r {
		r[i] = b[i]
		if a[i] > b[i] {
			r[i] = a[i]
		}
	}
	return fromF32(r)
}

func relaxedMinF64x2(x, y V128) V128 {
	a, b := x.f64(), y.f64()
	var r [2]float64
	for i := range r {
		r[i] = b[i]
		if a[i] < b[i] {
			r[i] = a[i]
		}
	}
	return fromF64(r)
}

func relaxedMaxF64x2(x, y V128) V128 {
	a, b := x.f64(), y.f64()
	var r [2]float64
	for i := range r {
		r[i] = b[i]
		if a[i] > b[i] {
			r[i] = a[i]
		}
	}
	return fromF64(r)
}

func relaxedQ15MulR(x, y V128) V128 {
	a, b := x.u16(), y.u16()
	var r [8]uint16
	for i := range r {
		r[i] = uint16((int(int16(a[i]))*int(int16(b[i])) + 0x4000) >> 15)
	}
	return fromU16(r)
}

// maddubs returns the sums of the products of each pair of x's signed bytes
// and y's unsigned bytes, saturated to 16 bits.
func maddubs(x, y V128) [8]uint16 {
	a, b := x.u8(), y.u8()
	var r [8]uint16
	for i := range r {
		p0 := int(int8(a[2*i])) * int(b[2*i])
		p1 := int(int8(a[2*i+1])) * int(b[2*i+1])
		r[i] = sat16s(p0 + p1)
	}
	return r
}

func relaxedDotI8x16I7x16S(x, y V128) V128 { return fromU16(maddubs(x, y)) }

func relaxedDotI8x16I7x16AddS(x, y, acc V128) V128 {
	switch {
	case relaxedDotSigned:
		return dotAdd32(x, y, acc, true)
	case relaxedDotVNNI:
		return dotAdd32(x, y, acc, false)
	}
	d, s := maddubs(x, y), acc.u32()
	var r [4]uint32
	for i := range r {
		r[i] = s[i] + uint32(int32(int16(d[2*i]))+int32(int16(d[2*i+1])))
	}
	return fromU32(r)
}
