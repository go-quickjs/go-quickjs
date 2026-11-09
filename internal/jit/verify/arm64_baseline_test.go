package verify

import (
	"fmt"
	"math/bits"
	"testing"

	"github.com/go-quickjs/go-quickjs/internal/jit/asm/arm64"
	"golang.org/x/arch/arm64/arm64asm"
)

// The forms the WebAssembly baseline compiler (wasm/internal/jit) adds,
// each disassembled with x/arch's arm64asm.

func TestARM64WasmInteger(t *testing.T) {
	expectA64(t, "ror w3, w4, w5", func(a *arm64.Asm) { a.ShiftReg(arm64.Ror, 3, 4, 5, false) })
	expectA64(t, "ror x3, x4, x5", func(a *arm64.Asm) { a.ShiftReg(arm64.Ror, 3, 4, 5, true) })
	expectA64(t, "ror w3, w4, #7", func(a *arm64.Asm) { a.ShiftImm(arm64.Ror, 3, 4, 7, false) })
	expectA64(t, "ror x3, x4, #63", func(a *arm64.Asm) { a.ShiftImm(arm64.Ror, 3, 4, 63, true) })
	expectA64(t, "ror w3, w4, #31", func(a *arm64.Asm) { a.ShiftImm(arm64.Ror, 3, 4, 31, false) })
	expectA64(t, "clz w3, w4", func(a *arm64.Asm) { a.Clz(3, 4, false) })
	expectA64(t, "clz x3, x26", func(a *arm64.Asm) { a.Clz(3, 26, true) })
	expectA64(t, "rbit w3, w4", func(a *arm64.Asm) { a.Rbit(3, 4, false) })
	expectA64(t, "rbit x3, x4", func(a *arm64.Asm) { a.Rbit(3, 4, true) })
	expectA64(t, "csel w3, w4, w5, ne", func(a *arm64.Asm) { a.Csel(3, 4, 5, arm64.NE, false) })
	expectA64(t, "csel x3, x4, x5, cc", func(a *arm64.Asm) { a.Csel(3, 4, 5, arm64.LO, true) })
	expectA64(t, "sxtb w3, w4", func(a *arm64.Asm) { a.Sxtb(3, 4, false) })
	expectA64(t, "sxtb x3, w4", func(a *arm64.Asm) { a.Sxtb(3, 4, true) })
	expectA64(t, "sxth w3, w4", func(a *arm64.Asm) { a.Sxth(3, 4, false) })
	expectA64(t, "sxth x3, w4", func(a *arm64.Asm) { a.Sxth(3, 4, true) })
	expectA64(t, "sdiv w3, w4, w5", func(a *arm64.Asm) { a.SdivW(3, 4, 5) })
	expectA64(t, "udiv w3, w4, w5", func(a *arm64.Asm) { a.UdivW(3, 4, 5) })
	expectA64(t, "msub w3, w4, w5, w6", func(a *arm64.Asm) { a.MsubW(3, 4, 5, 6) })
	expectA64(t, "adds x16, x3, #0x8", func(a *arm64.Asm) { a.AddsImm(16, 3, 8, true) })
	expectA64(t, "adds x16, x3, #0x1", func(a *arm64.Asm) { a.AddsImm(16, 3, 1, true) })
	expectA64(t, "mov x27, #0x2345; movk x27, #0x1, lsl #16; adds x3, x4, x27", func(a *arm64.Asm) { a.AddsImm(3, 4, 0x12345, true) })
	expectA64(t, "adds x3, x3, x16", func(a *arm64.Asm) { a.Op(arm64.Adds, 3, 3, 16, true) })
}

// Every logical immediate of both widths: each of the architecture's
// N:immr:imms encodings is decoded as DecodeBitMasks does, and the value
// must give that encoding back -- the canonical one, as each value has
// only one -- and disassemble as itself. Values with none are refused.
func TestARM64LogicalImmediates(t *testing.T) {
	count := 0
	for _, wide := range []bool{false, true} {
		for n := uint32(0); n < 2; n++ {
			if n == 1 && !wide {
				continue
			}
			for immr := uint32(0); immr < 64; immr++ {
				for imms := uint32(0); imms < 64; imms++ {
					v, ok := decodeBitMasks(n, immr, imms, wide)
					if !ok {
						continue
					}
					f, ok := arm64.LogicalImm(v, wide)
					if !ok || f != n<<12|immr<<6|imms {
						t.Fatalf("LogicalImm(%#x, %v) = %#x, %v, want %#x", v, wide, f, ok, n<<12|immr<<6|imms)
					}
					count++
				}
			}
		}
	}
	if count != 5334+1302 {
		t.Fatalf("%d immediates, want %d", count, 5334+1302)
	}
	for _, tc := range []struct {
		v    uint64
		wide bool
	}{{0, true}, {^uint64(0), true}, {0, false}, {0xFFFFFFFF, false}, {0x12345678, false}, {5, true}} {
		if f, ok := arm64.LogicalImm(tc.v, tc.wide); ok {
			t.Fatalf("LogicalImm(%#x, %v) = %#x, but it has none", tc.v, tc.wide, f)
		}
	}
	// A 32-bit operation's immediate is its low 32 bits.
	if f, ok := arm64.LogicalImm(1<<32|1, false); !ok || f != 0 {
		t.Fatalf("LogicalImm(1<<32|1, false) = %#x, %v, want 1's encoding, 0", f, ok)
	}
	expectA64(t, "and w3, w4, #0x7fffffff", func(a *arm64.Asm) { a.LogicImm(arm64.And, 3, 4, 0x7FFFFFFF, false) })
	expectA64(t, "eor x3, x4, #0x8000000000000000", func(a *arm64.Asm) { a.LogicImm(arm64.Eor, 3, 4, 1<<63, true) })
	expectA64(t, "orr w3, w4, #0xff00ff", func(a *arm64.Asm) { a.LogicImm(arm64.Orr, 3, 4, 0xFF00FF, false) })
	expectA64(t, "ands x3, x4, #0x1", func(a *arm64.Asm) { a.LogicImm(arm64.Ands, 3, 4, 1, true) })
	expectA64(t, "tst w4, #0x80000000", func(a *arm64.Asm) { a.LogicImm(arm64.Ands, arm64.ZR, 4, 0x80000000, false) })
	expectA64(t, "and x3, x4, #0x7fffffffffffffff", func(a *arm64.Asm) { a.LogicImm(arm64.And, 3, 4, 1<<63-1, true) })
	expectA64(t, "eor w3, w4, #0xaaaaaaaa", func(a *arm64.Asm) { a.LogicImm(arm64.Eor, 3, 4, 0xAAAAAAAA, false) })
	var a arm64.Asm
	if a.LogicImm(arm64.And, 3, 4, 0x12345, true) || a.Len() != 0 {
		t.Fatal("LogicImm emitted an immediate it has none for")
	}
	// A disassembled immediate is the value the encoder was given, across
	// the sizes and rotations.
	for _, v := range []uint64{0x5555555555555555, 0x0F0F0F0F0F0F0F0F, 0x00FF00FF00FF00FF, 0x0000FFFF0000FFFF, 0x00000000FFFFFFFF, 0xFFFFFFFF00000000, 0x8000000000000001, 0x7FFE} {
		var a arm64.Asm
		if !a.LogicImm(arm64.Orr, 3, 4, v, true) {
			t.Fatalf("%#x has no immediate", v)
		}
		code, _ := a.Finish()
		inst, err := arm64asm.Decode(code)
		if err != nil {
			t.Fatal(err)
		}
		if got, want := arm64asm.GNUSyntax(inst), fmt.Sprintf("orr x3, x4, #%#x", v); got != want {
			t.Fatalf("got %s, want %s", got, want)
		}
	}
}

// decodeBitMasks is the architecture's DecodeBitMasks for a logical
// instruction's immediate, reporting whether the fields are one.
func decodeBitMasks(n, immr, imms uint32, wide bool) (uint64, bool) {
	l := bits.Len32(n<<6|(^imms&0x3F)) - 1
	if l < 1 {
		return 0, false
	}
	size := uint(1) << l
	if size > 32 && !wide {
		return 0, false
	}
	levels := uint32(size - 1)
	s, r := imms&levels, immr&levels
	if s == levels {
		return 0, false
	}
	// The fields of a given size are unique when immr is within it.
	if immr > levels {
		return 0, false
	}
	elem := uint64(1)<<(s+1) - 1
	mask := ^uint64(0)
	if size < 64 {
		mask = uint64(1)<<size - 1
	}
	rot := (elem>>r | elem<<(size-uint(r))) & mask
	if r == 0 {
		rot = elem
	}
	v := uint64(0)
	for i := uint(0); i < 64; i += size {
		v |= rot << i
	}
	if !wide {
		v &= 0xFFFFFFFF
	}
	return v, true
}

func TestARM64WasmMemory(t *testing.T) {
	expectA64(t, "ldr x3, [x20,x5]", func(a *arm64.Asm) { a.LoadIdx(3, 20, 5, false) })
	expectA64(t, "ldr w3, [x20,x5]", func(a *arm64.Asm) { a.LoadU32Idx(3, 20, 5) })
	expectA64(t, "ldrh w3, [x20,x5]", func(a *arm64.Asm) { a.LoadU16Idx(3, 20, 5) })
	expectA64(t, "ldrb w3, [x20,x5]", func(a *arm64.Asm) { a.LoadU8Idx(3, 20, 5) })
	expectA64(t, "ldrsb w3, [x20,x5]", func(a *arm64.Asm) { a.LoadS8Idx(3, 20, 5, false) })
	expectA64(t, "ldrsb x3, [x20,x5]", func(a *arm64.Asm) { a.LoadS8Idx(3, 20, 5, true) })
	expectA64(t, "ldrsh w3, [x20,x5]", func(a *arm64.Asm) { a.LoadS16Idx(3, 20, 5, false) })
	expectA64(t, "ldrsh x3, [x20,x5]", func(a *arm64.Asm) { a.LoadS16Idx(3, 20, 5, true) })
	expectA64(t, "ldrsw x3, [x20,x5]", func(a *arm64.Asm) { a.LoadS32Idx(3, 20, 5) })
	expectA64(t, "str x3, [x20,x5]", func(a *arm64.Asm) { a.StoreIdx(20, 5, 3) })
	expectA64(t, "str w3, [x20,x5]", func(a *arm64.Asm) { a.Store32Idx(20, 5, 3) })
	expectA64(t, "strh w3, [x20,x5]", func(a *arm64.Asm) { a.Store16Idx(20, 5, 3) })
	expectA64(t, "strb w3, [x20,x5]", func(a *arm64.Asm) { a.Store8Idx(20, 5, 3) })
	expectA64(t, "ldr s3, [x20,x5]", func(a *arm64.Asm) { a.LoadSIdx(3, 20, 5) })
	expectA64(t, "ldr d3, [x20,x5]", func(a *arm64.Asm) { a.LoadFIdx(3, 20, 5) })
	expectA64(t, "str s3, [x20,x5]", func(a *arm64.Asm) { a.StoreSIdx(20, 5, 3) })
	expectA64(t, "str d3, [x20,x5]", func(a *arm64.Asm) { a.StoreFIdx(20, 5, 3) })
	// Every register as each operand.
	for r := arm64.Reg(0); r < 31; r++ {
		want := fmt.Sprintf("ldr x%d, [x%d,x%d]", r, (r+1)%31, (r+2)%31)
		expectA64(t, want, func(a *arm64.Asm) { a.LoadIdx(r, (r+1)%31, (r+2)%31, false) })
	}
}

func TestARM64WasmFloat(t *testing.T) {
	for _, tc := range []struct {
		op   arm64.FOp
		name string
	}{{arm64.FAdd, "fadd"}, {arm64.FSub, "fsub"}, {arm64.FMul, "fmul"}, {arm64.FDiv, "fdiv"}, {arm64.FMin, "fmin"}, {arm64.FMax, "fmax"}} {
		expectA64(t, tc.name+" s3, s4, s5", func(a *arm64.Asm) { a.FArithS(tc.op, 3, 4, 5) })
	}
	expectA64(t, "fcmp s4, s5", func(a *arm64.Asm) { a.FCmpS(4, 5) })
	expectA64(t, "fsqrt s3, s4", func(a *arm64.Asm) { a.FSqrtS(3, 4) })
	expectA64(t, "frintn s3, s4", func(a *arm64.Asm) { a.FRintS(arm64.FRintN, 3, 4) })
	expectA64(t, "frintm s3, s4", func(a *arm64.Asm) { a.FRintS(arm64.FRintM, 3, 4) })
	expectA64(t, "frintp s3, s4", func(a *arm64.Asm) { a.FRintS(arm64.FRintP, 3, 4) })
	expectA64(t, "frintz s3, s4", func(a *arm64.Asm) { a.FRintS(arm64.FRintZ, 3, 4) })
	expectA64(t, "fcsel d3, d4, d5, ne", func(a *arm64.Asm) { a.FCsel(3, 4, 5, arm64.NE, false) })
	expectA64(t, "fcsel s3, s4, s5, eq", func(a *arm64.Asm) { a.FCsel(3, 4, 5, arm64.EQ, true) })
	expectA64(t, "fmov s3, w4", func(a *arm64.Asm) { a.FMovToS(3, 4) })
	expectA64(t, "fmov w4, s3", func(a *arm64.Asm) { a.FMovFromS(4, 3) })
	for _, tc := range []struct {
		signed, wide, single bool
		want                 string
	}{
		{true, false, false, "fcvtzs w3, d4"},
		{true, true, false, "fcvtzs x3, d4"},
		{false, false, false, "fcvtzu w3, d4"},
		{false, true, false, "fcvtzu x3, d4"},
		{true, false, true, "fcvtzs w3, s4"},
		{true, true, true, "fcvtzs x3, s4"},
		{false, false, true, "fcvtzu w3, s4"},
		{false, true, true, "fcvtzu x3, s4"},
	} {
		expectA64(t, tc.want, func(a *arm64.Asm) { a.Fcvtz(3, 4, tc.signed, tc.wide, tc.single) })
	}
	for _, tc := range []struct {
		signed, wide, single bool
		want                 string
	}{
		{true, false, false, "scvtf d3, w4"},
		{true, true, false, "scvtf d3, x4"},
		{false, false, false, "ucvtf d3, w4"},
		{false, true, false, "ucvtf d3, x4"},
		{true, false, true, "scvtf s3, w4"},
		{true, true, true, "scvtf s3, x4"},
		{false, false, true, "ucvtf s3, w4"},
		{false, true, true, "ucvtf s3, x4"},
	} {
		expectA64(t, tc.want, func(a *arm64.Asm) { a.Cvtf(3, 4, tc.signed, tc.wide, tc.single) })
	}
	expectA64(t, "cnt v3.8b, v4.8b", func(a *arm64.Asm) { a.Cnt8B(3, 4) })
	expectA64(t, "addv b3, v4.8b", func(a *arm64.Asm) { a.Addv8B(3, 4) })
}
