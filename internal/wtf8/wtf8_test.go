package wtf8

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// The high and low halves of U+1F600 GRINNING FACE, used throughout as
// representative surrogates.
const (
	hi = 0xD83D
	lo = 0xDE00
)

func TestGoRefusesToEncodeSurrogates(t *testing.T) {
	// This is the reason the package exists. If Go ever started encoding
	// surrogates, the hand-rolled encoder could be dropped.
	var sb strings.Builder
	sb.WriteRune(hi)
	if sb.String() != string(utf8.RuneError) {
		t.Fatalf("WriteRune(%#x) produced % x; Go's behaviour has changed and "+
			"this package's premise should be revisited", hi, sb.String())
	}
}

func TestRoundTripLoneSurrogate(t *testing.T) {
	for _, r := range []rune{0xD800, 0xDBFF, 0xDC00, 0xDFFF, hi, lo} {
		encoded := string(AppendRune(nil, r))
		if len(encoded) != 3 {
			t.Errorf("encoding %#x produced %d bytes, want 3", r, len(encoded))
		}
		got, size := DecodeRune(encoded)
		if got != r || size != 3 {
			t.Errorf("DecodeRune(encoding of %#x) = (%#x, %d), want (%#x, 3)",
				r, got, size, r)
		}
		// The code unit must survive a trip through UTF-16 as well.
		units := ToUTF16(encoded)
		if len(units) != 1 || rune(units[0]) != r {
			t.Errorf("ToUTF16(encoding of %#x) = %#x, want one unit %#x",
				r, units, r)
		}
	}
}

func TestOrdinaryTextIsUnchanged(t *testing.T) {
	// WTF-8 and UTF-8 must agree on every well-formed string, or the encoding
	// would not be a drop-in replacement.
	for _, s := range []string{
		"", "abc", "café", "变量", "\U0001F600", "a\U0001F600b",
		"mixed ASCII and 日本語 text",
	} {
		if got := FromUTF16(ToUTF16(s)); got != s {
			t.Errorf("round trip of %q gave %q", s, got)
		}
		if !WellFormed(s) {
			t.Errorf("%q should be well formed", s)
		}
		if got := ToWellFormed(s); got != s {
			t.Errorf("ToWellFormed(%q) = %q, want it unchanged", s, got)
		}
	}
}

func TestSurrogatePairIsCombined(t *testing.T) {
	// A well-formed pair must become one code point, not two encoded halves,
	// so that the result is ordinary UTF-8.
	s := FromUTF16([]uint16{hi, lo})
	if s != "\U0001F600" {
		t.Errorf("FromUTF16(pair) = % x, want the emoji", s)
	}
	if len(s) != 4 {
		t.Errorf("combined pair is %d bytes, want 4 (ordinary UTF-8)", len(s))
	}
	if !WellFormed(s) {
		t.Error("a combined pair should be well formed")
	}
}

func TestUnpairedHalvesAreKept(t *testing.T) {
	tests := []struct {
		name  string
		units []uint16
	}{
		{"lone high", []uint16{hi}},
		{"lone low", []uint16{lo}},
		{"reversed pair", []uint16{lo, hi}},
		{"high then ASCII", []uint16{hi, 'a'}},
		{"ASCII then low", []uint16{'a', lo}},
		{"two highs", []uint16{hi, hi}},
		{"high at end", []uint16{'a', 'b', hi}},
	}
	for _, tt := range tests {
		s := FromUTF16(tt.units)
		got := ToUTF16(s)
		if len(got) != len(tt.units) {
			t.Errorf("%s: round trip gave %d units, want %d", tt.name, len(got), len(tt.units))
			continue
		}
		for i := range tt.units {
			if got[i] != tt.units[i] {
				t.Errorf("%s: unit %d = %#x, want %#x", tt.name, i, got[i], tt.units[i])
			}
		}
		if WellFormed(s) {
			t.Errorf("%s: should not be well formed", tt.name)
		}
	}
}

func TestCount(t *testing.T) {
	tests := []struct {
		units []uint16
		want  int
	}{
		{[]uint16{}, 0},
		{[]uint16{'a', 'b'}, 2},
		{[]uint16{hi}, 1},
		{[]uint16{hi, lo}, 2}, // a pair is two code units
		{[]uint16{'a', hi, 'b'}, 3},
		{[]uint16{lo, hi}, 2}, // reversed halves are two lone surrogates
	}
	for _, tt := range tests {
		s := FromUTF16(tt.units)
		if got := Count(s); got != tt.want {
			t.Errorf("Count(%#x) = %d, want %d", tt.units, got, tt.want)
		}
	}
	// Counting must agree with the code units the same string decodes to.
	for _, s := range []string{"abc", "café", "\U0001F600", "变量x"} {
		if Count(s) != len(ToUTF16(s)) {
			t.Errorf("Count(%q) disagrees with ToUTF16", s)
		}
	}
}

func TestToWellFormed(t *testing.T) {
	// Each lone surrogate becomes exactly one replacement character.
	s := FromUTF16([]uint16{'a', hi, 'b'})
	got := ToWellFormed(s)
	if got != "a�b" {
		t.Errorf("ToWellFormed = %q, want %q", got, "a�b")
	}
	// A valid pair must survive untouched.
	pair := FromUTF16([]uint16{hi, lo})
	if ToWellFormed(pair) != pair {
		t.Error("a valid surrogate pair should be left alone")
	}
}

func TestRuneLen(t *testing.T) {
	tests := []struct {
		r    rune
		want int
	}{
		{'a', 1},
		{0xE9, 2},    // é
		{0x4E2D, 3},  // 中
		{0x1F600, 4}, // emoji
		{hi, 3},      // a lone surrogate takes the three-byte form
		{lo, 3},
	}
	for _, tt := range tests {
		if got := RuneLen(tt.r); got != tt.want {
			t.Errorf("RuneLen(%#x) = %d, want %d", tt.r, got, tt.want)
		}
		if got := len(AppendRune(nil, tt.r)); got != tt.want {
			t.Errorf("AppendRune(%#x) wrote %d bytes, want %d", tt.r, got, tt.want)
		}
	}
}

func TestDecodeRuneMatchesUTF8ForValidInput(t *testing.T) {
	// For any string without a lone surrogate the decoder must behave exactly
	// like the standard one, so that ordinary text takes the same path.
	for _, s := range []string{"a", "é", "中", "\U0001F600", "\xff", ""} {
		gotR, gotN := DecodeRune(s)
		wantR, wantN := utf8.DecodeRuneInString(s)
		if gotR != wantR || gotN != wantN {
			t.Errorf("DecodeRune(%q) = (%#x, %d), want (%#x, %d)",
				s, gotR, gotN, wantR, wantN)
		}
	}
}

func TestIsSurrogate(t *testing.T) {
	for _, r := range []rune{0xD800, 0xDBFF, 0xDC00, 0xDFFF} {
		if !IsSurrogate(r) {
			t.Errorf("IsSurrogate(%#x) = false", r)
		}
	}
	for _, r := range []rune{'a', 0xD7FF, 0xE000, 0x1F600} {
		if IsSurrogate(r) {
			t.Errorf("IsSurrogate(%#x) = true", r)
		}
	}
}

func TestIsASCII(t *testing.T) {
	if !IsASCII("plain") || !IsASCII("") {
		t.Error("ASCII strings should be recognized")
	}
	if IsASCII("café") || IsASCII(FromUTF16([]uint16{hi})) {
		t.Error("non-ASCII strings should not be recognized as ASCII")
	}
}

func TestDecodeSurrogateRejectsNonSurrogateThreeByteSequences(t *testing.T) {
	// 中 is a three-byte sequence starting 0xE4, and U+FFFD starts 0xEF; the
	// surrogate check must not claim either.
	for _, s := range []string{"中", "�", "퟿", ""} {
		if _, ok := DecodeSurrogateAt(s, 0); ok {
			t.Errorf("DecodeSurrogateAt(%q) claimed a surrogate", s)
		}
	}
	// A truncated encoding must not be misread either.
	enc := string(AppendRune(nil, hi))
	for i := 1; i < 3; i++ {
		if _, ok := DecodeSurrogateAt(enc[:i], 0); ok {
			t.Errorf("a %d-byte prefix was decoded as a surrogate", i)
		}
	}
}

func BenchmarkToUTF16ASCII(b *testing.B) {
	s := strings.Repeat("abcdefgh", 16)
	b.ReportAllocs()
	b.SetBytes(int64(len(s)))
	for b.Loop() {
		ToUTF16(s)
	}
}

func BenchmarkCountASCII(b *testing.B) {
	s := strings.Repeat("abcdefgh", 16)
	b.ReportAllocs()
	b.SetBytes(int64(len(s)))
	for b.Loop() {
		Count(s)
	}
}
