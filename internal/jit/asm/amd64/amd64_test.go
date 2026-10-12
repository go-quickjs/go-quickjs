package amd64

import (
	"bytes"
	"encoding/binary"
	"testing"
)

// An address takes ten bytes whatever its value, so that code's length
// does not depend on where memory is: never MovImm's shorter forms for a
// value of 32 bits.
func TestMovAddrFixedLength(t *testing.T) {
	for _, v := range []uint64{0, 1, 0x7FFF_FFFF, 0xC000_0000_0000, 0xFFFF_FFFF_FFFF_FFFF} {
		for _, r := range []Reg{RAX, R11} {
			var a Asm
			a.MovAddr(r, v)
			if len(a.buf) != 10 || binary.LittleEndian.Uint64(a.buf[2:]) != v || a.buf[1] != 0xB8+byte(r)&7 {
				t.Fatalf("%#x into %d: % x", v, r, a.buf)
			}
		}
	}
}

// A memory operand's displacement takes a byte where it fits, four where
// it does not; RSP and R12 as a base take a SIB byte, and RBP and R13 a
// displacement even of 0.
func TestMemoryDisplacements(t *testing.T) {
	for _, tc := range []struct {
		base Reg
		disp int32
		want []byte
	}{
		{RBX, 8, []byte{0x48, 0x8B, 0x43, 0x08}},
		{RBX, -128, []byte{0x48, 0x8B, 0x43, 0x80}},
		{RBX, 127, []byte{0x48, 0x8B, 0x43, 0x7F}},
		{RBX, 128, []byte{0x48, 0x8B, 0x83, 0x80, 0x00, 0x00, 0x00}},
		{RBX, -129, []byte{0x48, 0x8B, 0x83, 0x7F, 0xFF, 0xFF, 0xFF}},
		{RBP, 0, []byte{0x48, 0x8B, 0x45, 0x00}},
		{R13, 0, []byte{0x49, 0x8B, 0x45, 0x00}},
		{RSP, 16, []byte{0x48, 0x8B, 0x44, 0x24, 0x10}},
		{R12, 0x200, []byte{0x49, 0x8B, 0x84, 0x24, 0x00, 0x02, 0x00, 0x00}},
	} {
		var a Asm
		a.Load(RAX, tc.base, tc.disp)
		if !bytes.Equal(a.buf, tc.want) {
			t.Errorf("Load(RAX, %d, %d) = % x, want % x", tc.base, tc.disp, a.buf, tc.want)
		}
	}
}
