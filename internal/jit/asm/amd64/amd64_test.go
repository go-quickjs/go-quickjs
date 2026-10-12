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

// decodeJumps walks code made of single-byte NOPs, jumps and leas (as
// TestRelaxedJumps makes), and returns each jump's or lea's target.
func decodeJumps(t *testing.T, code []byte) (targets []int, shortCount int) {
	t.Helper()
	for i := 0; i < len(code); {
		switch b := code[i]; {
		case b == 0x90:
			i++
		case b == 0xEB || b&0xF0 == 0x70:
			targets = append(targets, i+2+int(int8(code[i+1])))
			shortCount++
			i += 2
		case b == 0xE9:
			targets = append(targets, i+5+int(int32(binary.LittleEndian.Uint32(code[i+1:]))))
			i += 5
		case b == 0x0F && code[i+1]&0xF0 == 0x80:
			targets = append(targets, i+6+int(int32(binary.LittleEndian.Uint32(code[i+2:]))))
			i += 6
		case b == 0x48 && code[i+1] == 0x8D:
			targets = append(targets, i+7+int(int32(binary.LittleEndian.Uint32(code[i+3:]))))
			i += 7
		default:
			t.Fatalf("unexpected byte %#x at %d", b, i)
		}
	}
	return targets, shortCount
}

// Finish shortens the jumps whose labels are within a byte's displacement
// -- one shortened can bring another's label in reach -- and leaves the
// rest, and every lea, at 32 bits; every branch reaches the position its
// label moved to, which Moved gives for a position taken before.
func TestRelaxedJumps(t *testing.T) {
	var a Asm
	nops := func(n int) {
		for range n {
			a.buf = append(a.buf, 0x90)
		}
	}
	near, far, chained, back, entry := a.NewLabel(), a.NewLabel(), a.NewLabel(), a.NewLabel(), 0
	a.Bind(back)
	nops(3)
	a.Jcc(CondE, near) // 0: near, short
	nops(10)
	a.Bind(near)
	a.Jmp(far) // 1: far, stays long
	// 2: 126 bytes of code to chained once the jumps between shrink, but
	// 138 before.
	a.Jcc(CondNE, chained)
	for range 4 {
		a.Jmp(near) // backward, near: short
	}
	nops(118)
	a.Bind(chained)
	entry = a.Len()
	nops(300)
	a.LeaLabel(RAX, back)
	a.Bind(far)
	a.Jmp(back) // far back: long
	code, err := a.Finish()
	if err != nil {
		t.Fatal(err)
	}
	targets, short := decodeJumps(t, code)
	want := []int{a.Offset(near), a.Offset(far), a.Offset(chained), a.Offset(near), a.Offset(near), a.Offset(near), a.Offset(near), a.Offset(back), a.Offset(back)}
	if len(targets) != len(want) {
		t.Fatalf("%d branches, want %d: % x", len(targets), len(want), code)
	}
	for i := range want {
		if targets[i] != want[i] {
			t.Fatalf("branch %d reaches %d, its label is at %d", i, targets[i], want[i])
		}
	}
	if short != 6 {
		t.Fatalf("%d jumps shortened, want 6 (the near ones and the chained one)", short)
	}
	if a.Moved(entry) != a.Offset(chained) {
		t.Fatalf("a position taken before moved to %d, its label to %d", a.Moved(entry), a.Offset(chained))
	}
}
