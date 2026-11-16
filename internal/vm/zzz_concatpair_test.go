package vm

import (
	"strings"
	"testing"
	"unsafe"
)

// The memo of short concatenations answers every concatenation with its
// bytes: pieces flat or a slice of a longer string, ASCII or not, a result
// found by its pieces, by its bytes, or made, past the count after which
// it is made and across the sets its hash spreads them over.
func TestPairMemoAnswers(t *testing.T) {
	t.Parallel()
	r := New(Config{})
	defer r.Close()
	v, err := r.Run(compileForTest(t, `
		const bad = []
		const long = 'abcdefghijklmnopqrstuvwxyz0123456789'.repeat(4)
		for (let round = 0; round < 3; round++) {
			for (let i = 0; i < 3000; i++) {
				const a = long.slice(i % 30, i % 30 + 1 + i % 20), b = String(i % 97)
				let s = a + b
				if (s !== a.concat(b) || s.length !== a.length + b.length) bad.push(s)
				s = '<' + a; s += b; s += '\u00e9'
				if (s !== '<' + a.concat(b, '\u00e9')) bad.push(s)
			}
		}
		let row = ''
		for (let i = 0; i < 2000; i++) { row = '<td>'; row += 'x' + (i & 7); row += '</td>' }
		bad.push(row)
		bad.join()`))
	if err != nil {
		t.Fatal(err)
	}
	if got := v.String().Go(); got != "<td>x7</td>" {
		t.Fatalf("got %q", got)
	}
	if r.concats == nil {
		t.Fatal("no memo after thousands of short concatenations")
	}
}

// A concatenation the memo has seen allocates nothing, whether its pieces
// are their own allocations, kept with the result, or constants, kept by
// their bytes' address; one whose piece's address another string has come
// to have is not answered by the first's result.
func TestPairMemoReuses(t *testing.T) {
	r := New(Config{})
	defer r.Close()
	own := func(s string) *String {
		x, room := newStringBytes(len(s))
		copy(room, s)
		x.s = unsafe.String(unsafe.SliceData(room), len(s))
		x.length, x.ascii = len(s), true
		return x
	}
	a, b := own("<td>"), NewString("alpha")
	if !ownBytes(a) || ownBytes(NewString(strings.Repeat("y", 7))) {
		t.Fatal("ownBytes cannot tell a string's own bytes")
	}
	for i := 0; i < pairMemoAfter+10; i++ {
		r.concatShort(a, b)
	}
	first := r.concatShort(a, b)
	if n := testing.AllocsPerRun(100, func() {
		if s := r.concatShort(a, b); s != first {
			t.Fatal("a second result for the same pieces")
		}
		r.concatShort(b, a)
	}); n != 0 {
		t.Errorf("a remembered concatenation allocates %v times", n)
	}
	// The same bytes from other pieces find the result by its bytes.
	if s := r.concatShort(own("<td>al"), own("pha")); s != first {
		t.Errorf("the same bytes made %q anew", s.s)
	}
	// Bytes at an address a piece had, holding other text, are not the
	// piece: the result is made from what they hold.
	buf := []byte("alpha")
	c := &String{s: unsafe.String(&buf[0], len(buf)), length: 5, ascii: true}
	r.concatShort(a, c)
	copy(buf, "omega")
	if s := r.concatShort(a, c); s.s != "<td>omega" {
		t.Errorf("a piece's new bytes gave %q", s.s)
	}
	// A builder's string is never a piece, nor a result.
	bld := own("abc")
	bld.building = true
	if s := r.concatShort(bld, b); s.s != "abcalpha" || s.building {
		t.Errorf("a builder's piece gave %q", s.s)
	}
}

// A runtime with a memory limit charges each concatenation and keeps no
// memo.
func TestPairMemoNotMetered(t *testing.T) {
	r := New(Config{MemoryLimit: 64 << 20})
	defer r.Close()
	if _, err := r.Run(compileForTest(t, `let s; for (let i = 0; i < 5000; i++) { s = 'a' + (i & 3); s += 'b' }`)); err != nil {
		t.Fatal(err)
	}
	if r.concats != nil {
		t.Error("a metered runtime made the memo")
	}
}
