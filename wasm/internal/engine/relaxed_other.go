//go:build !amd64 && !386

package engine

// The relaxed SIMD operators on arm64 give what its NEON instructions that
// compute them give, and so do they on the other architectures; these are
// the results of the specification's deterministic profile too:
//
//   - relaxed_swizzle is tbl: an index past the last byte selects 0, as
//     swizzle does.
//   - the relaxed truncations are fcvtzs and fcvtzu (narrowed by sqxtn and
//     uqxtn): they saturate, as trunc_sat does.
//   - relaxed_madd and relaxed_nmadd are fmla and fmls: fused, one rounding.
//   - relaxed_laneselect is bsl: every bit of the mask selects, as bitselect
//     does.
//   - relaxed_min and relaxed_max are fmin and fmax, as min and max.
//   - relaxed_q15mulr_s is sqrdmulh: it saturates, as q15mulr_sat_s does.
//   - relaxed_dot_i8x16_i7x16_s is smull, smull2 and addp: the products of
//     signed bytes, each pair's sum wrapped to 16 bits.
//   - relaxed_dot_i8x16_i7x16_add_s adds to that saddlp and add: each pair of
//     those 16-bit sums added in 32 bits, and the addend.

func relaxedSwizzle(x, y V128) V128 { return vI8x16Swizzle(x, y) }

func relaxedTruncF32x4S(x V128) V128     { return vI32x4SConvertF32x4(x) }
func relaxedTruncF32x4U(x V128) V128     { return vI32x4UConvertF32x4(x) }
func relaxedTruncF64x2SZero(x V128) V128 { return vI32x4TruncSatF64x2SZero(x) }
func relaxedTruncF64x2UZero(x V128) V128 { return vI32x4TruncSatF64x2UZero(x) }

func relaxedMaddF32x4(x, y, z V128) V128  { return fusedMaddF32x4(x, y, z) }
func relaxedNmaddF32x4(x, y, z V128) V128 { return fusedNmaddF32x4(x, y, z) }
func relaxedMaddF64x2(x, y, z V128) V128  { return fusedMaddF64x2(x, y, z) }
func relaxedNmaddF64x2(x, y, z V128) V128 { return fusedNmaddF64x2(x, y, z) }

func relaxedLaneselect8(x, y, m V128) V128  { return vBitselect(x, y, m) }
func relaxedLaneselect16(x, y, m V128) V128 { return vBitselect(x, y, m) }
func relaxedLaneselect32(x, y, m V128) V128 { return vBitselect(x, y, m) }
func relaxedLaneselect64(x, y, m V128) V128 { return vBitselect(x, y, m) }

func relaxedMinF32x4(x, y V128) V128 { return vF32x4Min(x, y) }
func relaxedMaxF32x4(x, y V128) V128 { return vF32x4Max(x, y) }
func relaxedMinF64x2(x, y V128) V128 { return vF64x2Min(x, y) }
func relaxedMaxF64x2(x, y V128) V128 { return vF64x2Max(x, y) }

func relaxedQ15MulR(x, y V128) V128 { return vI16x8Q15MulRSatS(x, y) }

func relaxedDotI8x16I7x16S(x, y V128) V128 { return fromU16(dotSigned16(x, y)) }

func relaxedDotI8x16I7x16AddS(x, y, acc V128) V128 {
	d, s := dotSigned16(x, y), acc.u32()
	var r [4]uint32
	for i := range r {
		r[i] = s[i] + uint32(int32(int16(d[2*i]))+int32(int16(d[2*i+1])))
	}
	return fromU32(r)
}
