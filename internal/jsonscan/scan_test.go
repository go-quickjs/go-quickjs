package jsonscan

import (
	"math/rand/v2"
	"strings"
	"testing"
)

func stopsASCII(c byte) bool { return c == '"' || c == '\\' || c < 0x20 || c >= 0x80 }

func stopsWide(c, other byte) bool { return c == '"' || c == '\\' || c < 0x20 || c == other }

// check holds of what a scan said of s, for the test of what stops it: every
// byte before it is plain, and, where the machine has a fast way to tell, it
// is at a byte that is not, or within the last bytes the scan does not read.
// Elsewhere 0 is the answer, and says only that nothing is known.
func check(t *testing.T, s string, at int, stops func(byte) bool) {
	t.Helper()
	if at < 0 || at > len(s) {
		t.Fatalf("%d of %d bytes", at, len(s))
	}
	for i := range at {
		if stops(s[i]) {
			t.Fatalf("passed the %#x at %d of %d, stopping at %d", s[i], i, len(s), at)
		}
	}
	if fast && at < len(s)-31 && !stops(s[at]) {
		t.Fatalf("stopped at %#x at %d of %d, with %d bytes to go", s[at], at, len(s), len(s)-at)
	}
}

// Each byte there is, at each place in a read, in text that is otherwise
// plain, for lengths around a read's.
func TestEveryByteAtEveryOffset(t *testing.T) {
	for n := MinLength; n < 2*MinLength+34; n++ {
		base := []byte(strings.Repeat("a", n))
		for at := 0; at < n; at += 1 + at/40 {
			for c := range 256 {
				s := base
				s[at] = byte(c)
				text := string(s)
				check(t, text, ASCII(text), func(b byte) bool { return stopsASCII(b) })
				check(t, text, Wide(text, 0xED), func(b byte) bool { return stopsWide(b, 0xED) })
				check(t, text, Wide(text, '"'), func(b byte) bool { return stopsWide(b, '"') })
				s[at] = 'a'
			}
		}
	}
}

func TestRandomText(t *testing.T) {
	r := rand.New(rand.NewPCG(1, 2))
	for range 200000 {
		b := make([]byte, MinLength+32+r.IntN(300))
		for i := range b {
			b[i] = 'a' + byte(r.IntN(26))
			if r.IntN(200) == 0 {
				b[i] = byte(r.IntN(256))
			}
		}
		// Not at the start of the allocation, which may be aligned.
		s := string(b)[r.IntN(32):]
		check(t, s, ASCII(s), stopsASCII)
		check(t, s, Wide(s, 0xED), func(c byte) bool { return stopsWide(c, 0xED) })
	}
}

func TestShortTextIsLeftToTheCaller(t *testing.T) {
	s := strings.Repeat("a", MinLength-1)
	if ASCII(s) != 0 || Wide(s, 0xED) != 0 {
		t.Errorf("read text shorter than MinLength")
	}
}

func BenchmarkASCII(b *testing.B) {
	s := strings.Repeat("ten chars.", 6554)
	b.SetBytes(int64(len(s)))
	for b.Loop() {
		ASCII(s)
	}
}

func BenchmarkWide(b *testing.B) {
	s := strings.Repeat("ten chars.", 6554)
	b.SetBytes(int64(len(s)))
	for b.Loop() {
		Wide(s, 0xED)
	}
}
