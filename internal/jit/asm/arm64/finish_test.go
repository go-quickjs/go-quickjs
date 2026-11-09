package arm64

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"testing"
)

// The output can be reused across functions of different sizes without
// keeping their old instructions or displacements. Finish still owns its
// result independently of the assembler and the buffer given to FinishInto.
func TestFinishInto(t *testing.T) {
	var a Asm
	dst := bytes.Repeat([]byte{0xFF}, 4096)
	for _, n := range []int{32, 1, 2048, 8, 0} {
		a.Reset()
		start, end := a.NewLabel(), a.NewLabel()
		a.Bind(start)
		a.B(end)
		a.BCond(NE, end)
		a.Adr(3, end)
		for range n {
			a.MovRR(4, 5)
		}
		a.Bind(end)
		a.Cbnz(6, start, true)
		want, err := a.Finish()
		if err != nil {
			t.Fatal(err)
		}
		got, err := a.FinishInto(dst)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("%d moves: output differs from Finish", n)
		}
		if len(want) <= cap(dst) && &got[0] != &dst[:cap(dst)][0] {
			t.Fatalf("%d moves: did not reuse the buffer", n)
		}
		// Pin forward B, conditional and ADR, and backward CBNZ, after
		// every word in the destination held another function's code.
		for i, w := range map[int]uint32{
			0:     0x14000000 | uint32(n+3),
			1:     0x54000001 | uint32(n+2)<<5,
			2:     0x10000003 | uint32(n+1)<<5,
			n + 3: 0xB5000006 | (uint32(-n-3)&(1<<19-1))<<5,
		} {
			if v := binary.LittleEndian.Uint32(got[4*i:]); v != w {
				t.Fatalf("%d moves, word %d: %#08x, want %#08x", n, i, v, w)
			}
		}
		clear(got)
		again, err := a.Finish()
		if err != nil || !bytes.Equal(want, again) {
			t.Fatalf("%d moves: output aliases Finish's result or the assembler: %v", n, err)
		}
		dst = got
	}
	if allocs := testing.AllocsPerRun(100, func() {
		var err error
		dst, err = a.FinishInto(dst)
		if err != nil {
			t.Fatal(err)
		}
	}); allocs != 0 {
		t.Fatalf("%.0f allocations with a buffer that fits", allocs)
	}
	a.Reset()
	a.B(a.NewLabel())
	if got, err := a.FinishInto(dst); got != nil || err == nil {
		t.Fatalf("unbound label: code %x, error %v", got, err)
	}
}

func BenchmarkFinish(b *testing.B) {
	for _, n := range []int{16, 1024, 16384} {
		for _, reuse := range []bool{false, true} {
			b.Run(fmt.Sprintf("words=%d/reuse=%t", n, reuse), func(b *testing.B) {
				var a Asm
				start := a.NewLabel()
				a.Bind(start)
				for i := range n {
					if i%8 == 0 {
						a.BCond(NE, start)
					} else {
						a.MovRR(4, 5)
					}
				}
				var dst []byte
				if reuse {
					dst = make([]byte, a.Len())
				}
				b.ReportAllocs()
				b.ResetTimer()
				for range b.N {
					code, err := a.FinishInto(dst)
					if err != nil {
						b.Fatal(err)
					}
					if reuse {
						dst = code
					}
				}
			})
		}
	}
}
