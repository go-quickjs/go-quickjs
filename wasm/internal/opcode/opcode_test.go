package opcode

import (
	"testing"

	"github.com/go-quickjs/go-quickjs/wasm/internal/module"
)

func TestNames(t *testing.T) {
	for _, c := range []struct {
		op   Opcode
		name string
	}{
		{Unreachable, "unreachable"}, {I32Add, "i32.add"}, {SelectWithType, "select"},
		{0x17, "invalid opcode"}, {Opcode(PrefixGC), "unknown"}, {StructNew, "struct.new"},
		{I8x16RelaxedSwizzle, "i8x16.relaxed_swizzle"}, {0xfd1ff, "invalid opcode"}, {0xfb100, "invalid opcode"},
		{AtomicFence, "atomic.fence"}, {I32SConvertSatF32, "i32.trunc_sat_f32_s"}, {F64Acos, "f64.acos"},
	} {
		if got := c.op.Name(); got != c.name {
			t.Errorf("%#x: name %q, want %q", uint32(c.op), got, c.name)
		}
	}
}

func TestPrefixed(t *testing.T) {
	if got := Prefixed(PrefixSIMD, 0x0c); got != S128Const {
		t.Errorf("Prefixed(0xfd, 0x0c) = %#x", uint32(got))
	}
	if got := Prefixed(PrefixSIMD, 0x105); got != F32x4Qfma {
		t.Errorf("Prefixed(0xfd, 0x105) = %#x", uint32(got))
	}
	for _, o := range []Opcode{I32Add, StructNew, F32x4Qfma, AtomicFence} {
		want := byte(0)
		if o > 0xff {
			want = byte(o >> 8)
			if o > 0xffff {
				want = byte(o >> 12)
			}
		}
		if got := o.Prefix(); got != want {
			t.Errorf("%s: prefix %#x, want %#x", o, got, want)
		}
	}
}

func TestSignatures(t *testing.T) {
	s := Signature(I32Add)
	if s == nil || len(s.Params) != 2 || s.Params[0] != module.I32 || len(s.Results) != 1 || s.Results[0] != module.I32 {
		t.Errorf("i32.add: %+v", s)
	}
	if s := Signature(I64SExtendI32); s == nil || s.Params[0] != module.I64 {
		t.Errorf("i64.extend32_s: %+v", s)
	}
	if s := Signature(I32x4DotI8x16I7x16AddS); s == nil || len(s.Params) != 3 {
		t.Errorf("i32x4.dot_i8x16_i7x16_add_s: %+v", s)
	}
	for _, o := range []Opcode{Block, StructNew, I8x16ExtractLaneS, 0xfd200} {
		if s := Signature(o); s != nil {
			t.Errorf("%s: unexpected signature %+v", o, s)
		}
	}
	if s := AtomicSignature(I32AtomicWait, true); s == nil || s.Params[0] != module.I64 || s.Params[2] != module.I64 {
		t.Errorf("memory.atomic.wait32 on memory64: %+v", s)
	}
	if s := AtomicSignature(I64AtomicStore, false); s == nil || len(s.Results) != 0 || s.Params[1] != module.I64 {
		t.Errorf("i64.atomic.store: %+v", s)
	}
	if !IsRelaxedSIMD(F64x2Qfms) || IsRelaxedSIMD(F64x2Add) || !IsFP16SIMD(F16x8Add) || IsFP16SIMD(F32x4Qfma) {
		t.Errorf("SIMD classes")
	}
}
