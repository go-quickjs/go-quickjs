package engine

import (
	"bytes"
	"math/rand"
	"os"
	osexec "os/exec"
	"path/filepath"
	"testing"
)

// TestSIMDGenerated fails while ops_simd_gen.go differs from what
// internal/simdgen writes; go generate ./wasm/internal/engine rewrites it.
func TestSIMDGenerated(t *testing.T) {
	if testing.Short() {
		t.Skip("runs the generator")
	}
	out := filepath.Join(t.TempDir(), "ops_simd_gen.go")
	cmd := osexec.Command("go", "run", "./internal/simdgen", out)
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("simdgen: %v\n%s", err, b)
	}
	want, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile("ops_simd_gen.go")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatal("ops_simd_gen.go is out of date: run go generate ./wasm/internal/engine")
	}
}

// randV128 returns a vector whose lanes are often at the edges of their
// ranges.
func randV128(r *rand.Rand) V128 {
	edges := []uint8{0, 1, 0x7f, 0x80, 0x81, 0xfe, 0xff}
	var b [16]uint8
	for i := range b {
		if r.Intn(3) == 0 {
			b[i] = edges[r.Intn(len(edges))]
		} else {
			b[i] = uint8(r.Uint32())
		}
	}
	return fromU8(b)
}

// TestLaneSums compares the sums and differences computed a word at a time
// with those computed lane by lane.
func TestLaneSums(t *testing.T) {
	r := rand.New(rand.NewSource(1))
	for n := 0; n < 100000; n++ {
		x, y := randV128(r), randV128(r)
		a8, b8 := x.u8(), y.u8()
		var s8, d8 [16]uint8
		for i := range s8 {
			s8[i], d8[i] = a8[i]+b8[i], a8[i]-b8[i]
		}
		a16, b16 := x.u16(), y.u16()
		var s16, d16 [8]uint16
		for i := range s16 {
			s16[i], d16[i] = a16[i]+b16[i], a16[i]-b16[i]
		}
		a32, b32 := x.u32(), y.u32()
		var s32, d32 [4]uint32
		for i := range s32 {
			s32[i], d32[i] = a32[i]+b32[i], a32[i]-b32[i]
		}
		for _, c := range []struct {
			name      string
			got, want V128
		}{
			{"i8x16.add", vI8x16Add(x, y), fromU8(s8)},
			{"i8x16.sub", vI8x16Sub(x, y), fromU8(d8)},
			{"i16x8.add", vI16x8Add(x, y), fromU16(s16)},
			{"i16x8.sub", vI16x8Sub(x, y), fromU16(d16)},
			{"i32x4.add", vI32x4Add(x, y), fromU32(s32)},
			{"i32x4.sub", vI32x4Sub(x, y), fromU32(d32)},
		} {
			if c.got != c.want {
				t.Fatalf("%s(%x, %x) = %x, want %x", c.name, x, y, c.got, c.want)
			}
		}
	}
}
