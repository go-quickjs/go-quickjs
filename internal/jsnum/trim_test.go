package jsnum

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// TestTrimSpace pins that the byte scanners take off what strings.Trim
// takes off with whitespace for a set, around and between every code point,
// a lone surrogate's WTF-8 included: one of them a space only where it is
// one of whitespace's.
func TestTrimSpace(t *testing.T) {
	enc := func(r rune) string {
		if r >= 0xD800 && r <= 0xDFFF {
			return string([]byte{byte(0xE0 | r>>12), byte(0x80 | (r>>6)&0x3F), byte(0x80 | r&0x3F)})
		}
		return string(utf8.AppendRune(nil, r))
	}
	check := func(s string) {
		t.Helper()
		if got, want := TrimSpace(s), strings.Trim(s, whitespace); got != want {
			t.Fatalf("TrimSpace(%q) = %q, want %q", s, got, want)
		}
		if got, want := TrimLeftSpace(s), strings.TrimLeft(s, whitespace); got != want {
			t.Fatalf("TrimLeftSpace(%q) = %q, want %q", s, got, want)
		}
		if got, want := TrimRightSpace(s), strings.TrimRight(s, whitespace); got != want {
			t.Fatalf("TrimRightSpace(%q) = %q, want %q", s, got, want)
		}
	}
	for r := rune(0); r <= utf8.MaxRune; r++ {
		c := enc(r)
		check(c)
		check(c + "x" + c)
		check(" " + c + "\xe3\x80\x80" + c + "\xc2\xa0")
	}
	for _, s := range []string{"", " ", "\xc2\xa0", "\xef\xbb\xbf\xe2\x80\xa9", "\xc2", "\xe2\x80", "\xe3\x80\x80\x80",
		"x\xa0", "\xa0x", "\xe2\x80\x8a\xe2\x80\x8b", "\xf0\x9f\x98\x80 ", " \xed\xa0\x80"} {
		check(s)
	}
}
