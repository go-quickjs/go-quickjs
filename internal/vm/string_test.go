package vm

import (
	"strings"
	"testing"
)

func TestStringLengthIsInCodeUnits(t *testing.T) {
	tests := []struct {
		s    string
		want int
	}{
		{"", 0},
		{"abc", 3},
		{"café", 4},       // é is one code unit
		{"变量", 2},         // CJK characters are one code unit each
		{"\U0001F600", 2}, // an emoji is a surrogate pair, so two units
		{"a\U0001F600b", 4},
	}
	for _, tt := range tests {
		if got := NewString(tt.s).Len(); got != tt.want {
			t.Errorf("NewString(%q).Len() = %d, want %d", tt.s, got, tt.want)
		}
	}
}

func TestStringASCIIDetection(t *testing.T) {
	if !NewString("plain ascii").ascii {
		t.Error("an ASCII string should take the fast path")
	}
	if NewString("café").ascii {
		t.Error("a non-ASCII string must not claim the ASCII fast path")
	}
}

func TestCharCodeAt(t *testing.T) {
	s := NewString("abc")
	for i, want := range []int{'a', 'b', 'c'} {
		if got := s.CharCodeAt(i); got != want {
			t.Errorf("CharCodeAt(%d) = %d, want %d", i, got, want)
		}
	}
	if s.CharCodeAt(3) != -1 || s.CharCodeAt(-1) != -1 {
		t.Error("an out-of-range index should give -1")
	}

	// A surrogate pair must be readable as its two halves.
	emoji := NewString("\U0001F600")
	if got := emoji.CharCodeAt(0); got != 0xD83D {
		t.Errorf("high surrogate = %#x, want 0xD83D", got)
	}
	if got := emoji.CharCodeAt(1); got != 0xDE00 {
		t.Errorf("low surrogate = %#x, want 0xDE00", got)
	}
	// CodePointAt combines them again.
	if got := emoji.CodePointAt(0); got != 0x1F600 {
		t.Errorf("CodePointAt(0) = %#x, want 0x1F600", got)
	}
}

func TestSubstring(t *testing.T) {
	s := NewString("hello world")
	tests := []struct {
		start, end int
		want       string
	}{
		{0, 5, "hello"},
		{6, 11, "world"},
		{0, 11, "hello world"},
		{3, 3, ""},
		{5, 2, ""},              // an inverted range is empty
		{0, 100, "hello world"}, // clamped to the length
	}
	for _, tt := range tests {
		if got := s.Substring(tt.start, tt.end).Go(); got != tt.want {
			t.Errorf("Substring(%d, %d) = %q, want %q", tt.start, tt.end, got, tt.want)
		}
	}
}

func TestSubstringSplittingASurrogatePair(t *testing.T) {
	// Slicing between the halves of a pair is legal and yields a lone
	// surrogate, which must survive rather than becoming U+FFFD.
	s := NewString("a\U0001F600b")
	half := s.Substring(1, 2)
	if half.Len() != 1 {
		t.Fatalf("length = %d, want 1", half.Len())
	}
	if got := half.CharCodeAt(0); got != 0xD83D {
		t.Errorf("lone surrogate = %#x, want 0xD83D", got)
	}
}

func TestConcat(t *testing.T) {
	a, b := NewString("foo"), NewString("bar")
	if got := a.Concat(b).Go(); got != "foobar" {
		t.Errorf("Concat = %q, want %q", got, "foobar")
	}
	// Concatenating with the empty string returns the other operand unchanged.
	if a.Concat(NewString("")) != a {
		t.Error("appending an empty string should return the original")
	}
	if NewString("").Concat(a) != a {
		t.Error("prepending an empty string should return the original")
	}
}

func TestConcatBuildsARope(t *testing.T) {
	// Long concatenations must defer the copy, or repeated appends are
	// quadratic. The result is still correct once flattened.
	long := NewString(strings.Repeat("x", 100))
	r := long.Concat(NewString(strings.Repeat("y", 100)))
	if r.left == nil {
		t.Error("a long concatenation should produce a rope")
	}
	if r.Len() != 200 {
		t.Errorf("rope length = %d, want 200", r.Len())
	}
	want := strings.Repeat("x", 100) + strings.Repeat("y", 100)
	if got := r.Go(); got != want {
		t.Errorf("flattened rope is wrong (len %d, want %d)", len(got), len(want))
	}
	// Flattening is idempotent.
	if got := r.Go(); got != want {
		t.Error("flattening twice changed the value")
	}
}

func TestDeepRopeFlattensWithoutStackOverflow(t *testing.T) {
	// A loop of `s += x` builds a left-leaning chain as deep as the iteration
	// count. Flattening must not recurse over it.
	s := NewString(strings.Repeat("a", 64))
	const n = 200000
	for i := 0; i < n; i++ {
		s = s.Concat(NewString("b"))
	}
	if s.Len() != 64+n {
		t.Fatalf("length = %d, want %d", s.Len(), 64+n)
	}
	got := s.Go()
	if len(got) != 64+n {
		t.Fatalf("flattened length = %d, want %d", len(got), 64+n)
	}
	if !strings.HasPrefix(got, strings.Repeat("a", 64)) || got[64] != 'b' {
		t.Error("flattened rope has the wrong contents")
	}
}

func TestEquals(t *testing.T) {
	if !NewString("abc").Equals(NewString("abc")) {
		t.Error("equal strings should compare equal")
	}
	if NewString("abc").Equals(NewString("abd")) {
		t.Error("different strings should not compare equal")
	}
	if NewString("ab").Equals(NewString("abc")) {
		t.Error("strings of different lengths should not compare equal")
	}
	// A rope must compare equal to the flat string with the same contents.
	rope := NewString(strings.Repeat("x", 100)).Concat(NewString(strings.Repeat("y", 100)))
	flat := NewString(strings.Repeat("x", 100) + strings.Repeat("y", 100))
	if !rope.Equals(flat) {
		t.Error("a rope should equal the flat string with the same contents")
	}
}

func TestCompare(t *testing.T) {
	tests := []struct {
		a, b string
		want int
	}{
		{"a", "b", -1},
		{"b", "a", 1},
		{"a", "a", 0},
		{"ab", "abc", -1},
		{"abc", "ab", 1},
		{"", "a", -1},
		// Comparison is by code unit, so a non-ASCII string orders by its
		// UTF-16 value.
		{"a", "é", -1},
	}
	for _, tt := range tests {
		got := NewString(tt.a).Compare(NewString(tt.b))
		if (got < 0) != (tt.want < 0) || (got > 0) != (tt.want > 0) {
			t.Errorf("Compare(%q, %q) = %d, want sign of %d", tt.a, tt.b, got, tt.want)
		}
	}
}

func TestIndexOf(t *testing.T) {
	s := NewString("hello world hello")
	tests := []struct {
		needle string
		from   int
		want   int
	}{
		{"hello", 0, 0},
		{"hello", 1, 12},
		{"world", 0, 6},
		{"missing", 0, -1},
		{"", 0, 0},
		{"", 5, 5},
		{"o", 5, 7},
	}
	for _, tt := range tests {
		if got := s.IndexOf(NewString(tt.needle), tt.from); got != tt.want {
			t.Errorf("IndexOf(%q, %d) = %d, want %d", tt.needle, tt.from, got, tt.want)
		}
	}
	// Searching a non-ASCII string uses the code-unit path.
	u := NewString("变量 test 变量")
	if got := u.IndexOf(NewString("test"), 0); got != 3 {
		t.Errorf("IndexOf on a non-ASCII string = %d, want 3", got)
	}
}

func TestArrayIndexRecognition(t *testing.T) {
	tests := []struct {
		name string
		idx  uint32
		ok   bool
	}{
		{"0", 0, true},
		{"1", 1, true},
		{"42", 42, true},
		{"4294967294", 4294967294, true}, // the largest legal index
		{"4294967295", 0, false},         // reserved as the length
		{"01", 0, false},                 // not canonical
		{"1.0", 0, false},
		{"-1", 0, false},
		{"", 0, false},
		{"abc", 0, false},
		{"12345678901", 0, false}, // too long
	}
	for _, tt := range tests {
		idx, ok := arrayIndexOf(tt.name)
		if ok != tt.ok || (ok && idx != tt.idx) {
			t.Errorf("arrayIndexOf(%q) = (%d, %v), want (%d, %v)",
				tt.name, idx, ok, tt.idx, tt.ok)
		}
	}
}

func BenchmarkConcatLoop(b *testing.B) {
	// The rope representation is what keeps this linear.
	b.ReportAllocs()
	for b.Loop() {
		s := NewString("")
		for i := 0; i < 1000; i++ {
			s = s.Concat(NewString("abcdefgh"))
		}
		if s.Len() != 8000 {
			b.Fatalf("length = %d", s.Len())
		}
	}
}

func BenchmarkNewStringASCII(b *testing.B) {
	b.ReportAllocs()
	for b.Loop() {
		NewString("a moderately long ascii string for scanning")
	}
}
