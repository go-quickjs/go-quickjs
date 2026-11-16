package arm64

import (
	"math"
	"testing"
)

// An address takes four instructions whatever its value, so that code's
// length does not depend on where memory is; MovImm's shorter forms for
// zero and all-ones half-words are not used. They make the value.
func TestMovAddrFixedLength(t *testing.T) {
	for _, v := range []uint64{0, 1, 0x7FFF_0000_1000, 0x0000_C000_0000_0000, 0xFFFF_FFFF_FFFF_FFFF, 0x1234_5678_9ABC_DEF0} {
		var a Asm
		a.MovAddr(5, v)
		if len(a.buf) != 4 {
			t.Fatalf("%#x: %d instructions", v, len(a.buf))
		}
		var got uint64
		for i, w := range a.buf {
			if op := w &^ (3<<21 | 0xFFFF<<5 | 31); i == 0 && op != 0xD2800000 || i > 0 && op != 0xF2800000 || w&31 != 5 {
				t.Fatalf("%#x: instruction %d is %#08x", v, i, w)
			}
			hw := (w >> 21) & 3
			got |= uint64((w>>5)&0xFFFF) << (16 * hw)
		}
		if got != v {
			t.Fatalf("%#x: made %#x", v, got)
		}
	}
}

// MulW is MUL of W registers, MADD with WZR: mul w0, w1, w2 and mul w9,
// w28, w17.
func TestMulW(t *testing.T) {
	for _, tc := range []struct {
		d, n, m Reg
		want    uint32
	}{
		{0, 1, 2, 0x1B027C20},
		{9, 28, 17, 0x1B117F89},
	} {
		var a Asm
		a.MulW(tc.d, tc.n, tc.m)
		if len(a.buf) != 1 || a.buf[0] != tc.want {
			t.Errorf("MulW(%d, %d, %d) = %#08x, want %#08x", tc.d, tc.n, tc.m, a.buf, tc.want)
		}
	}
}

// ClzW is CLZ of W registers, sf 0: clz w0, w1, clz w15, w28 and clz w9,
// w17.
func TestClzW(t *testing.T) {
	for _, tc := range []struct {
		d, n Reg
		want uint32
	}{
		{0, 1, 0x5AC01020},
		{15, 28, 0x5AC0138F},
		{9, 17, 0x5AC01229},
	} {
		var a Asm
		a.ClzW(tc.d, tc.n)
		if len(a.buf) != 1 || a.buf[0] != tc.want {
			t.Errorf("ClzW(%d, %d) = %#08x, want %#08x", tc.d, tc.n, a.buf, tc.want)
		}
	}
}

// evalFlags runs code -- the moves, the additions and subtractions and
// their flag-setting forms, immediate and register, that AddImm, SubsImm
// and CmpImm emit -- on regs, and returns the flags the last flag-setting
// one left, as NZCV in the low four bits.
func evalFlags(t *testing.T, code []uint32, regs *[32]uint64) uint8 {
	t.Helper()
	var flags uint8
	read := func(r uint32) uint64 {
		if r == 31 {
			return 0
		}
		return regs[r]
	}
	for _, w := range code {
		rd, rn := w&31, (w>>5)&31
		switch {
		case w&0x7F800000 == 0x52800000: // MOVZ
			regs[rd] = uint64((w>>5)&0xFFFF) << (16 * ((w >> 21) & 3))
		case w&0x7F800000 == 0x12800000: // MOVN
			regs[rd] = ^(uint64((w>>5)&0xFFFF) << (16 * ((w >> 21) & 3)))
		case w&0x7F800000 == 0x72800000: // MOVK
			s := 16 * ((w >> 21) & 3)
			regs[rd] = regs[rd]&^(0xFFFF<<s) | uint64((w>>5)&0xFFFF)<<s
		case (w>>23)&0x3F == 0x22, (w>>24)&0x1F == 0x0B && w&(1<<21) == 0:
			wide, sub, setFlags := w>>31 == 1, (w>>30)&1 == 1, (w>>29)&1 == 1
			var m uint64
			if (w>>23)&0x3F == 0x22 {
				m = uint64((w >> 10) & 0xFFF)
				if (w>>22)&1 == 1 {
					m <<= 12
				}
			} else {
				if (w>>10)&0x3F != 0 || (w>>22)&3 != 0 {
					t.Fatalf("%#08x: a shifted operand", w)
				}
				m = read((w >> 16) & 31)
			}
			n := read(rn)
			bits := uint(64)
			if !wide {
				bits, n, m = 32, uint64(uint32(n)), uint64(uint32(m))
			}
			mask := uint64(1)<<bits - 1
			if bits == 64 {
				mask = ^uint64(0)
			}
			carry := uint64(0)
			if sub {
				m, carry = ^m&mask, 1
			}
			// The sum of n, m and the carry in, one bit wider than the
			// operation: n + ^m + 1 is n - m, its carry out the borrow's
			// absence.
			lo, c1 := bitsAdd(n, m, carry, bits)
			res := lo & mask
			if setFlags {
				flags = 0
				if res>>(bits-1)&1 == 1 {
					flags |= 8
				}
				if res == 0 {
					flags |= 4
				}
				if c1 {
					flags |= 2
				}
				sn, sm, sr := n>>(bits-1)&1, m>>(bits-1)&1, res>>(bits-1)&1
				if sn == sm && sr != sn {
					flags |= 1
				}
			}
			if !setFlags || rd != 31 {
				regs[rd] = res
			}
		default:
			t.Fatalf("%#08x: not an instruction evalFlags runs", w)
		}
	}
	return flags
}

// bitsAdd is n + m + carry in a width of bits, and whether it carries out.
func bitsAdd(n, m, carry uint64, bits uint) (uint64, bool) {
	if bits == 64 {
		s := n + m
		c := s < n
		s2 := s + carry
		return s2, c || s2 < s
	}
	s := n + m + carry
	return s, s>>bits != 0
}

// CmpImm sets the flags CMP of the immediate would, for any immediate of
// either width: negative ones are CMN of their negation, which sets the
// same flags but for the width's least integer, whose negation is itself
// -- CMN W, #0x80000000 had left V clear for 0, where CMP sets it, and a
// signed comparison of an int32 with -2**31 had gone the wrong way. Each
// against the flags of the subtraction itself, which SubsImm also leaves
// in its register, and AddImm's sum.
func TestCmpImmFlags(t *testing.T) {
	imms := []int64{0, 1, -1, 5, -5, 4095, -4095, 4096, -4096, 0x7FF000, -0x7FF000, 1 << 24, -(1 << 24), 3000000000, -3000000000,
		math.MaxInt32, math.MinInt32, math.MinInt32 + 1, 1 << 31, 1<<32 - 1, 1 << 32, math.MaxInt64, math.MinInt64, math.MinInt64 + 1}
	xs := []uint64{0, 1, 5, 0x7FFFFFFF, 0x80000000, 0xFFFFFFFF, 0xFFFFFFFB, 1 << 32, 0x7FFFFFFF_FFFFFFFF, 1 << 63, ^uint64(0), 0xFFFFFFFF_80000000}
	for _, wide := range []bool{false, true} {
		bits := uint(32)
		if wide {
			bits = 64
		}
		for _, imm := range imms {
			for _, x := range xs {
				var a Asm
				a.CmpImm(X1, imm, wide)
				var regs [32]uint64
				regs[X1] = x
				got := evalFlags(t, a.buf, &regs)
				var ref [32]uint64
				ref[X1], ref[X2] = x, uint64(imm)
				want := evalFlags(t, []uint32{uint32(Subs) | sfBit(wide) | uint32(X2)<<16 | uint32(X1)<<5 | 31}, &ref)
				if got != want {
					t.Errorf("CmpImm(%#x, %d, %d bits): NZCV %04b, CMP's %04b (%#08x)", x, imm, bits, got, want, a.buf)
				}
				a = Asm{}
				a.SubsImm(X3, X1, imm, wide)
				regs = [32]uint64{X1: x}
				if got := evalFlags(t, a.buf, &regs); got != want || regs[X3] != trunc(x-uint64(imm), bits) {
					t.Errorf("SubsImm(%#x, %d, %d bits): NZCV %04b and %#x, want %04b and %#x", x, imm, bits, got, regs[X3], want, trunc(x-uint64(imm), bits))
				}
				a = Asm{}
				a.AddImm(X3, X1, imm, wide)
				regs = [32]uint64{X1: x}
				evalFlags(t, a.buf, &regs)
				if regs[X3] != trunc(x+uint64(imm), bits) {
					t.Errorf("AddImm(%#x, %d, %d bits) = %#x, want %#x", x, imm, bits, regs[X3], trunc(x+uint64(imm), bits))
				}
			}
		}
	}
}

// The registers the test computes in.
const (
	X1 Reg = 1
	X2 Reg = 2
	X3 Reg = 3
)

func sfBit(wide bool) uint32 {
	if wide {
		return 1 << 31
	}
	return 0
}

func trunc(v uint64, bits uint) uint64 {
	if bits == 32 {
		return uint64(uint32(v))
	}
	return v
}

// decodeLogical is the immediate a logical operation's N:immr:imms field
// encodes, by the architecture's DecodeBitMasks, of the width; ok is
// false for a reserved field.
func decodeLogical(f uint32, wide bool) (uint64, bool) {
	n, immr, imms := f>>12&1, f>>6&0x3F, f&0x3F
	if !wide && n == 1 {
		return 0, false
	}
	l := 6
	for ; l >= 0; l-- {
		if (n<<6|^imms&0x3F)>>uint(l)&1 == 1 {
			break
		}
	}
	if l < 1 {
		return 0, false
	}
	size := uint(1) << uint(l)
	s, r := uint(imms)&(size-1), uint(immr)&(size-1)
	if s == size-1 {
		return 0, false
	}
	mask := ^uint64(0)
	if size < 64 {
		mask = uint64(1)<<size - 1
	}
	e := uint64(1)<<(s+1) - 1
	if r > 0 {
		e = (e>>r | e<<(size-r)) & mask
	}
	v := uint64(0)
	for i := uint(0); i < 64; i += size {
		v |= e << i
	}
	if !wide {
		v = uint64(uint32(v))
	}
	return v, true
}

// LogicalImm finds an encoding for every immediate that has one, which
// decodes to it, and none for any other: every field of each width
// decoded, and values near them that are no pattern.
func TestLogicalImm(t *testing.T) {
	for _, wide := range []bool{false, true} {
		valid := map[uint64]bool{}
		for f := uint32(0); f < 1<<13; f++ {
			v, ok := decodeLogical(f, wide)
			if !ok {
				continue
			}
			valid[v] = true
			got, ok := LogicalImm(v, wide)
			if !ok {
				t.Fatalf("LogicalImm(%#x, wide %v): none, want one (%#x)", v, wide, f)
			}
			if back, _ := decodeLogical(got, wide); back != v {
				t.Fatalf("LogicalImm(%#x, wide %v) = %#x, which is %#x", v, wide, got, back)
			}
		}
		for v := range valid {
			for _, w := range []uint64{v + 1, v - 1, v ^ 0x10000, v ^ 1<<40} {
				if !wide {
					w = uint64(uint32(w))
				}
				if _, ok := LogicalImm(w, wide); ok != valid[w] {
					t.Fatalf("LogicalImm(%#x, wide %v) reports %v", w, wide, ok)
				}
			}
		}
	}
}

// The encodings of the bit fields, the register-offset load, the
// exclusive pair and the logical immediates, against the architecture's:
// ubfx x1, x2, #16, #16; sbfx x3, x4, #32, #16; ldr x1, [x21, x1, lsl #3];
// ldr x1, [x21, x1]; ldxr x16, [x2]; stxr w17, x16, [x2]; and x1, x2,
// #0xff; tst w3, #0x8000; and x1, x2 of tmp's 0x12345.
func TestFieldsAndLoads(t *testing.T) {
	for _, tc := range []struct {
		f    func(a *Asm)
		want []uint32
	}{
		{func(a *Asm) { a.Ubfx(1, 2, 16, 16) }, []uint32{0xD3507C41}},
		{func(a *Asm) { a.Sbfx(3, 4, 32, 16) }, []uint32{0x9360BC83}},
		{func(a *Asm) { a.LoadIdx(1, 21, 1, true) }, []uint32{0xF8617AA1}},
		{func(a *Asm) { a.LoadIdx(1, 21, 1, false) }, []uint32{0xF8616AA1}},
		{func(a *Asm) { a.Ldxr(16, 2) }, []uint32{0xC85F7C50}},
		{func(a *Asm) { a.Stxr(17, 16, 2) }, []uint32{0xC8117C50}},
		{func(a *Asm) { a.AndImm(1, 2, 0xFF, true) }, []uint32{0x92401C41}},
		{func(a *Asm) { a.TstImm(3, 0x8000, false) }, []uint32{0x7211007F}},
		{func(a *Asm) { a.AndImm(1, 2, 0x12345, true) }, []uint32{0xD28468BB, 0xF2A0003B, 0x8A1B0041}},
	} {
		var a Asm
		tc.f(&a)
		if len(a.buf) != len(tc.want) {
			t.Errorf("%#08x, want %#08x", a.buf, tc.want)
			continue
		}
		for i := range a.buf {
			if a.buf[i] != tc.want[i] {
				t.Errorf("%#08x, want %#08x", a.buf, tc.want)
				break
			}
		}
	}
}
