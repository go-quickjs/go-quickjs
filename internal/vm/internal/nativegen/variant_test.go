package nativegen

import (
	"bytes"
	"encoding/binary"
	"testing"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
)

// TestVariantOverrides emits, by each architecture's emitter, a variant
// that overrides two handlers, one
// of which jumps to a handler it does not override (tail_call_w to
// call_w), and checks the mechanism: the overrides alone in its own
// block, their table entries its own and every other plain's, its exits
// and its jump to plain's handler through the table, and plain's block
// the same with the variant as without.
func TestVariantOverrides(t *testing.T) {
	if testing.Short() {
		t.Skip("builds package vm to read its layout")
	}
	l, err := ReadLayout("amd64")
	if err != nil {
		t.Fatal(err)
	}
	for _, ar := range arches {
		t.Run(ar.goarch, func(t *testing.T) { testVariantOverrides(t, l, ar) })
	}
}

func testVariantOverrides(t *testing.T, l *Layout, ar *arch) {
	v := &variant{name: "test", sym: "Test", build: "ignore", overrides: []bytecode.ROp{bytecode.RAdd, bytecode.RCallWT}}
	p, err := ar.emitFrom(l, plain)
	if err != nil {
		t.Fatal(err)
	}
	b, err := ar.emitFrom(l, v)
	if err != nil {
		t.Fatal(err)
	}
	if p2, _ := ar.emitFrom(l, plain); !bytes.Equal(p.code, p2.code) {
		t.Fatal("plain's block changed after a variant's")
	}
	if len(b.order) != 2 || b.order[0] != bytecode.RAdd || b.order[1] != bytecode.RCallWT {
		t.Fatalf("the variant's block has %v, want add and tail_call_w", b.order)
	}
	tab := v.table(p, b)
	if len(tab) != variantSlots+len(v.overrides) {
		t.Fatalf("%d slots, want %d", len(tab), variantSlots+len(v.overrides))
	}
	for _, op := range v.overrides {
		if want := (Slot{Off: p.entry(op)}); tab[v.slotPlain(op)] != want {
			t.Errorf("plain's %s's slot is %+v, want %+v", op, tab[v.slotPlain(op)], want)
		}
	}
	for op := range 256 {
		want := Slot{Off: p.entry(bytecode.ROp(op))}
		if v.has(bytecode.ROp(op)) {
			want = Slot{Own: true, Off: b.at[bytecode.ROp(op)]}
		}
		if tab[op] != want {
			t.Errorf("%s's slot is %+v, want %+v", bytecode.ROp(op), tab[op], want)
		}
	}
	if tab[slotExit] != (Slot{Off: p.exit}) || tab[slotExitRet] != (Slot{Off: p.exitRet}) {
		t.Errorf("the exits' slots are %+v and %+v, want plain's exits %d and %d", tab[slotExit], tab[slotExitRet], p.exit, p.exitRet)
	}
	// The jumps through the table: the exit's, the exit to nativeRun's,
	// call_w's entry. amd64's is jmp [r13+disp]; arm64's ldr x16,
	// [x21, #disp] and br x16.
	jmp := func(disp int) []byte {
		if ar.goarch == "arm64" {
			w := binary.LittleEndian.AppendUint32(nil, 0xF9400000|uint32(disp/8)<<10|21<<5|16)
			return binary.LittleEndian.AppendUint32(w, 0xD61F0000|16<<5)
		}
		if disp < 128 {
			return []byte{0x41, 0xff, 0x65, byte(disp)}
		}
		return binary.LittleEndian.AppendUint32([]byte{0x41, 0xff, 0xa5}, uint32(disp))
	}
	for _, want := range [][]byte{jmp(8 * slotExit), jmp(8 * slotExitRet), jmp(8 * int(bytecode.RCallW))} {
		if !bytes.Contains(b.code, want) {
			t.Errorf("the variant's block has no % x", want)
		}
	}
	src := ar.source(v, p, b)
	for _, want := range []string{"//go:build ignore\n", "TEXT ·regNativeTestHandlers(SB)", "$·regNativeTestHandlers+", "GLOBL ·regNativeTestTable(SB), RODATA, $2080\n", "TEXT ·nativeTestTableAddr(SB)"} {
		if !bytes.Contains(src, []byte(want)) {
			t.Errorf("the variant's file has no %q", want)
		}
	}
}
