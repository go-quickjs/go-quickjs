package verify

import (
	"encoding/binary"
	"fmt"
	"math"
	"strconv"
	"strings"
	"testing"

	"github.com/go-quickjs/go-quickjs/internal/jit/asm/arm64"
	"golang.org/x/arch/arm64/arm64asm"
)

// a64 encodes with f and disassembles each instruction in GNU syntax.
func a64(t *testing.T, f func(a *arm64.Asm)) []string {
	t.Helper()
	var a arm64.Asm
	f(&a)
	code, err := a.Finish()
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for pc := 0; pc < len(code); pc += 4 {
		inst, err := arm64asm.Decode(code[pc:])
		if err != nil {
			t.Fatalf("%08x does not decode: %v", binary.LittleEndian.Uint32(code[pc:]), err)
		}
		out = append(out, arm64asm.GNUSyntax(inst))
	}
	return out
}

// expectA64 requires f's code to disassemble to want, one instruction per
// line.
func expectA64(t *testing.T, want string, f func(a *arm64.Asm)) {
	t.Helper()
	if got := strings.Join(a64(t, f), "; "); got != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
}

func TestARM64Moves(t *testing.T) {
	expectA64(t, "mov x3, x20", func(a *arm64.Asm) { a.MovRR(3, 20) })
	expectA64(t, "mov w3, w20", func(a *arm64.Asm) { a.MovRR32(3, 20) })
	for _, tc := range []struct {
		v    uint64
		want string
	}{
		{0, "mov x5, #0x0"},
		{1, "mov x5, #0x1"},
		{0xFFFF, "mov x5, #0xffff"},
		{0x10000, "mov x5, #0x10000"},
		{0x12345678, "mov x5, #0x5678; movk x5, #0x1234, lsl #16"},
		{^uint64(0), "mov x5, #0xffffffffffffffff"},
		{0xFFF8000000000000, "mov x5, #0xfff8000000000000"},
		{0x7FF8000000000000, "mov x5, #0x7ff8000000000000"},
		{0x8000000000000000, "mov x5, #0x8000000000000000"},
		{0xFFFFFFFFFFFF1234, "mov x5, #0xffffffffffff1234"},
		{0x123456789ABCDEF0, "mov x5, #0xdef0; movk x5, #0x9abc, lsl #16; movk x5, #0x5678, lsl #32; movk x5, #0x1234, lsl #48"},
	} {
		expectA64(t, tc.want, func(a *arm64.Asm) { a.MovImm(5, tc.v) })
	}
}

func TestARM64Memory(t *testing.T) {
	expectA64(t, "ldr x3, [x0,#16]", func(a *arm64.Asm) { a.Load(3, 0, 16) })
	expectA64(t, "str x3, [x1,#2056]", func(a *arm64.Asm) { a.Store(1, 2056, 3) })
	expectA64(t, "ldur x3, [x0,#-8]", func(a *arm64.Asm) { a.Load(3, 0, -8) })
	expectA64(t, "ldur x3, [x0,#12]", func(a *arm64.Asm) { a.Load(3, 0, 12) })
	expectA64(t, "mov x27, #0x8000; add x27, x0, x27; ldr x3, [x27]", func(a *arm64.Asm) { a.Load(3, 0, 0x8000) })
	expectA64(t, "ldr w3, [x2,#8]", func(a *arm64.Asm) { a.LoadU32(3, 2, 8) })
	expectA64(t, "ldrh w3, [x2,#6]", func(a *arm64.Asm) { a.LoadU16(3, 2, 6) })
	expectA64(t, "ldrb w3, [x2,#5]", func(a *arm64.Asm) { a.LoadU8(3, 2, 5) })
	expectA64(t, "ldr d4, [x0,#24]", func(a *arm64.Asm) { a.LoadF(4, 0, 24) })
	expectA64(t, "str d4, [x0,#24]", func(a *arm64.Asm) { a.StoreF(0, 24, 4) })
	expectA64(t, "stur d4, [x0,#-16]", func(a *arm64.Asm) { a.StoreF(0, -16, 4) })
}

func TestARM64Integer(t *testing.T) {
	for _, tc := range []struct {
		op   arm64.ALU
		name string
	}{{arm64.Add, "add"}, {arm64.Sub, "sub"}, {arm64.Adds, "adds"}, {arm64.Subs, "subs"},
		{arm64.And, "and"}, {arm64.Orr, "orr"}, {arm64.Eor, "eor"}, {arm64.Ands, "ands"}} {
		expectA64(t, tc.name+" x3, x4, x5", func(a *arm64.Asm) { a.Op(tc.op, 3, 4, 5, true) })
		expectA64(t, tc.name+" w3, w4, w5", func(a *arm64.Asm) { a.Op(tc.op, 3, 4, 5, false) })
	}
	expectA64(t, "cmp x4, x5", func(a *arm64.Asm) { a.Cmp(4, 5, true) })
	expectA64(t, "tst w4, w5", func(a *arm64.Asm) { a.Tst(4, 5, false) })
	expectA64(t, "add x3, x4, #0x10", func(a *arm64.Asm) { a.AddImm(3, 4, 16, true) })
	expectA64(t, "sub x3, x4, #0x10", func(a *arm64.Asm) { a.AddImm(3, 4, -16, true) })
	expectA64(t, "add x3, x4, #0x5, lsl #12", func(a *arm64.Asm) { a.AddImm(3, 4, 0x5000, true) })
	expectA64(t, "mov x27, #0x2345; movk x27, #0x1, lsl #16; add x3, x4, x27", func(a *arm64.Asm) { a.AddImm(3, 4, 0x12345, true) })
	expectA64(t, "cmp w4, #0x7ff", func(a *arm64.Asm) { a.CmpImm(4, 0x7FF, false) })
	expectA64(t, "cmn x4, #0x1", func(a *arm64.Asm) { a.CmpImm(4, -1, true) })
	expectA64(t, "lsl w3, w4, w5", func(a *arm64.Asm) { a.ShiftReg(arm64.Lsl, 3, 4, 5, false) })
	expectA64(t, "lsr x3, x4, x5", func(a *arm64.Asm) { a.ShiftReg(arm64.Lsr, 3, 4, 5, true) })
	expectA64(t, "asr w3, w4, w5", func(a *arm64.Asm) { a.ShiftReg(arm64.Asr, 3, 4, 5, false) })
	expectA64(t, "lsl x3, x4, #12", func(a *arm64.Asm) { a.ShiftImm(arm64.Lsl, 3, 4, 12, true) })
	expectA64(t, "lsr x3, x4, #52", func(a *arm64.Asm) { a.ShiftImm(arm64.Lsr, 3, 4, 52, true) })
	expectA64(t, "lsl w3, w4, #4", func(a *arm64.Asm) { a.ShiftImm(arm64.Lsl, 3, 4, 4, false) })
	expectA64(t, "asr w3, w4, #31", func(a *arm64.Asm) { a.ShiftImm(arm64.Asr, 3, 4, 31, false) })
	expectA64(t, "mvn w3, w4", func(a *arm64.Asm) { a.Mvn(3, 4, false) })
	expectA64(t, "neg w3, w4", func(a *arm64.Asm) { a.Neg(3, 4, false) })
	expectA64(t, "cset w3, mi", func(a *arm64.Asm) { a.Cset(3, arm64.MI) })
	expectA64(t, "sdiv x3, x4, x5", func(a *arm64.Asm) { a.Sdiv(3, 4, 5) })
	expectA64(t, "msub x3, x4, x5, x6", func(a *arm64.Asm) { a.Msub(3, 4, 5, 6) })
}

func TestARM64Float(t *testing.T) {
	expectA64(t, "fmov d3, x4", func(a *arm64.Asm) { a.FMovToF(3, 4) })
	expectA64(t, "fmov x4, d3", func(a *arm64.Asm) { a.FMovFromF(4, 3) })
	expectA64(t, "fmov d3, d31", func(a *arm64.Asm) { a.FMov(3, 31) })
	for _, tc := range []struct {
		op   arm64.FOp
		name string
	}{{arm64.FAdd, "fadd"}, {arm64.FSub, "fsub"}, {arm64.FMul, "fmul"}, {arm64.FDiv, "fdiv"}} {
		expectA64(t, tc.name+" d3, d4, d5", func(a *arm64.Asm) { a.FArith(tc.op, 3, 4, 5) })
	}
	expectA64(t, "fcmp d4, d5", func(a *arm64.Asm) { a.FCmp(4, 5) })
	expectA64(t, "fneg d3, d4", func(a *arm64.Asm) { a.FNeg(3, 4) })
	expectA64(t, "fsqrt d3, d4", func(a *arm64.Asm) { a.FSqrt(3, 4) })
	expectA64(t, "fabs d3, d4", func(a *arm64.Asm) { a.FAbs(3, 4) })
	expectA64(t, "fmax d3, d4, d5", func(a *arm64.Asm) { a.FArith(arm64.FMax, 3, 4, 5) })
	expectA64(t, "fmin d3, d4, d5", func(a *arm64.Asm) { a.FArith(arm64.FMin, 3, 4, 5) })
	expectA64(t, "frintm d3, d4", func(a *arm64.Asm) { a.FRint(arm64.FRintM, 3, 4) })
	expectA64(t, "frintp d3, d4", func(a *arm64.Asm) { a.FRint(arm64.FRintP, 3, 4) })
	expectA64(t, "frintz d3, d4", func(a *arm64.Asm) { a.FRint(arm64.FRintZ, 3, 4) })
	expectA64(t, "fcvt s3, d4", func(a *arm64.Asm) { a.FCvtToSingle(3, 4) })
	expectA64(t, "fcvt d3, s4", func(a *arm64.Asm) { a.FCvtFromSingle(3, 4) })
	expectA64(t, "fcvtzs x3, d4", func(a *arm64.Asm) { a.Fcvtzs(3, 4) })
	expectA64(t, "scvtf d3, x4", func(a *arm64.Asm) { a.Scvtf(3, 4, true) })
	expectA64(t, "scvtf d3, w4", func(a *arm64.Asm) { a.Scvtf(3, 4, false) })
	expectA64(t, "ucvtf d3, w4", func(a *arm64.Asm) { a.Ucvtf(3, 4, false) })
}

// Every FMOV immediate: its double, by the architecture's VFPExpandImm,
// has that immediate by FloatImm, and the disassembler reads the
// instruction as that double. Doubles with none are refused.
func TestARM64FloatImmediates(t *testing.T) {
	for imm := 0; imm < 256; imm++ {
		a, b, rest := uint64(imm>>7), uint64(imm>>6&1), uint64(imm&0x3F)
		bits := a<<63 | (b^1)<<62 | 0xFF*b<<54 | rest<<48
		if got, ok := arm64.FloatImm(bits); !ok || got != uint8(imm) {
			t.Fatalf("FloatImm(%v) = %#x, %v, want %#x", math.Float64frombits(bits), got, ok, imm)
		}
		text := a64(t, func(a *arm64.Asm) { a.FMovImm(3, uint8(imm)) })[0]
		number, found := strings.CutPrefix(text, "fmov d3, #")
		if v, err := strconv.ParseFloat(number, 64); !found || err != nil || v != math.Float64frombits(bits) {
			t.Fatalf("imm %#x: %s, want %v", imm, text, math.Float64frombits(bits))
		}
	}
	for _, v := range []float64{0, math.Copysign(0, -1), 0.1, 3.99, 32, 0.0625, 1e10, math.Inf(1), math.NaN()} {
		if imm, ok := arm64.FloatImm(math.Float64bits(v)); ok {
			t.Fatalf("FloatImm(%v) = %#x, but it has no immediate", v, imm)
		}
	}
	expectA64(t, "fmov d3, xzr", func(a *arm64.Asm) { a.FMovToF(3, arm64.ZR) })
}

func TestARM64Control(t *testing.T) {
	got := a64(t, func(a *arm64.Asm) {
		top, end := a.NewLabel(), a.NewLabel()
		a.Bind(top)
		a.BCond(arm64.NE, end)
		a.Cbz(3, top, true)
		a.Cbnz(4, end, false)
		a.B(top)
		a.Bind(end)
		a.Ret()
	})
	want := []string{"b.ne .+0x10", "cbz x3, .+0xfffffffffffffffc", "cbnz w4, .+0x8", "b .+0xfffffffffffffff4", "ret"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("got  %q\nwant %q", got, want)
	}
}

// Native calls jump through registers to addresses taken from labels.
func TestARM64IndirectJumps(t *testing.T) {
	expectA64(t, "br x15", func(a *arm64.Asm) { a.Br(15) })
	expectA64(t, "br x3", func(a *arm64.Asm) { a.Br(3) })
	got := a64(t, func(a *arm64.Asm) {
		back, ahead := a.NewLabel(), a.NewLabel()
		a.Bind(back)
		a.Adr(5, ahead)
		a.Adr(17, back)
		a.Ret()
		a.Bind(ahead)
	})
	want := []string{"adr x5, .+0xc", "adr x17, .+0xfffffffffffffffc", "ret"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("got  %q\nwant %q", got, want)
	}
}
