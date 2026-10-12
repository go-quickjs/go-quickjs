package arm64

import "testing"

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
