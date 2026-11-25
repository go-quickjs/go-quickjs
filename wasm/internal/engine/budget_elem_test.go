package engine

import (
	"context"
	"errors"
	"runtime"
	"testing"

	"github.com/go-quickjs/go-quickjs/wasm/internal/decoder"
)

// elemModule returns a module of one function and one passive element
// segment of n references to it, exporting "drop", which drops the
// segment, as a binary: the text format would take a token for each.
func elemModule(n int) []byte {
	b := []byte{0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00}
	section := func(id byte, payload []byte) {
		b = append(b, id)
		b = appendLEB(b, uint64(len(payload)))
		b = append(b, payload...)
	}
	section(1, []byte{1, 0x60, 0, 0})                     // type: [] -> []
	section(3, []byte{2, 0, 0})                           // two functions of it
	section(7, []byte{1, 4, 'd', 'r', 'o', 'p', 0x00, 1}) // export "drop" = func 1
	elem := []byte{1, 1, 0x00}                            // one passive segment of funcrefs
	elem = appendLEB(elem, uint64(n))
	elem = append(elem, make([]byte, n)...) // function 0, n times
	section(9, elem)
	section(10, []byte{2,
		2, 0, 0x0b, // func 0: nothing
		5, 0, 0xfc, 13, 0, 0x0b, // func 1: elem.drop 0
	})
	return b
}

func appendLEB(b []byte, v uint64) []byte {
	for {
		c := byte(v & 0x7f)
		v >>= 7
		if v == 0 {
			return append(b, c)
		}
		b = append(b, c|0x80)
	}
}

func elemCode(t *testing.T, n int) *Code {
	t.Helper()
	m, derr := decoder.Decode(elemModule(n), decoder.CoreLimits())
	if derr != nil {
		t.Fatal(derr)
	}
	if derr := decoder.ValidateFunctions(m, decoder.CoreLimits()); derr != nil {
		t.Fatal(derr)
	}
	return testCode(t, NewCode(m))
}

// TestBudgetElementSegments checks that an element segment's references
// are charged to the budget as the instance makes them, and given back
// when the segment is dropped; and that a segment that does not fit fails
// the instantiation before its references are made. Segments were roots
// the budget walked but charged to nothing: 10 million references, 160 MB,
// were made under a 64 MB budget.
func TestBudgetElementSegments(t *testing.T) {
	b := NewBudget(64 << 20)
	inst, err := InstantiateBudget(context.Background(), elemCode(t, 100_000), nil, b)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := b.Used(), int64(100_000)*refBytes; got != want {
		t.Fatalf("used %d, want the segment's %d", got, want)
	}
	if _, err := callExport(t, inst, "drop"); err != nil {
		t.Fatal(err)
	}
	if got := b.Used(); got != 0 {
		t.Fatalf("used %d after elem.drop, want 0", got)
	}

	code := elemCode(t, 10_000_000)
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	_, err = InstantiateBudget(context.Background(), code, nil, NewBudget(64<<20))
	runtime.ReadMemStats(&after)
	if !errors.Is(err, ErrMemoryLimit) {
		t.Fatalf("a 160 MB segment under a 64 MB budget: got %v, want ErrMemoryLimit", err)
	}
	if n := after.TotalAlloc - before.TotalAlloc; n > 16<<20 {
		t.Errorf("the refused instantiation allocated %d bytes", n)
	}
}
