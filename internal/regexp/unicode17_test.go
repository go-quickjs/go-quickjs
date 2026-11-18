package regexp

import (
	"testing"
	"unicode/utf16"

	"github.com/go-quickjs/go-quickjs/internal/ucd"
)

// TestOneUnicodeVersion pins that the property escapes and the case folding,
// group names and the rest package ucd answers are one Unicode version, which
// the generator writes both of in one run.
func TestOneUnicodeVersion(t *testing.T) {
	if UnicodeVersion != ucd.Version {
		t.Fatalf("property escapes at %s, package ucd at %s", UnicodeVersion, ucd.Version)
	}
}

// TestFoldsAtUnicode17 matches under /iu the characters Unicode 15.1 gave a
// simple case folding -- U+1FD3 to U+0390, U+1FE3 to U+03B0, U+FB05 to
// U+FB06 -- and 16.0's new case pairs, which a build with Go 1.24's tables, at
// 15.0, kept apart (test262's built-ins/RegExp/unicode_full_case_folding.js).
// Without the u flag the comparison is of upper cases, which none of the
// first three has.
func TestFoldsAtUnicode17(t *testing.T) {
	for _, c := range []struct {
		pattern, subject rune
		iu, i            bool
	}{
		{0x0390, 0x1FD3, true, false},
		{0x1FD3, 0x0390, true, false},
		{0x03B0, 0x1FE3, true, false},
		{0xFB05, 0xFB06, true, false},
		{0x1C89, 0x1C8A, true, true},    // CYRILLIC LETTER TJE, 16.0
		{0x10D50, 0x10D70, true, false}, // GARAY LETTER A, 16.0; past the BMP, two units without u
		{0xA7CB, 0x0264, true, true},    // LATIN CAPITAL LETTER RAMS HORN, 16.0
		{0x16EA0, 0x16EBB, true, false}, // BERIA ERFE LETTER ARKAB, 17.0
		{0x0130, 'i', false, false},     // no simple folding
		{0x212A, 'k', true, false},      // KELVIN SIGN, an ASCII fold
	} {
		for _, flags := range []string{"iu", "i"} {
			want := c.iu
			if flags == "i" {
				want = c.i
			}
			re, err := Compile(string(c.pattern), flags)
			if err != nil {
				t.Fatal(err)
			}
			got, err := re.Match(utf16.Encode([]rune{c.subject}), 0)
			if err != nil {
				t.Fatal(err)
			}
			if (got != nil) != want {
				t.Errorf("/\\u{%X}/%s on U+%04X matched %v, want %v", c.pattern, flags, c.subject, got != nil, want)
			}
		}
	}
}

// TestGroupNamesAreIdentifiers takes a group name's characters by the
// identifier rules -- ID_Start and ID_Continue -- rather than by letters:
// a letter number and an Other_ID_Start begin one, a character of 17.0
// continues one, and a letter that is Pattern_Syntax does neither.
func TestGroupNamesAreIdentifiers(t *testing.T) {
	for _, name := range []string{"\u2160", "\u1885", "a\U00011B60", "\U00016EA0", "a\u30FB", "$_\u200C"} {
		if _, err := Compile("(?<"+name+">.)", "u"); err != nil {
			t.Errorf("(?<%+q>.): %v", name, err)
		}
	}
	for _, name := range []string{"\u2E2F", "a\u2E2F", "\u2FFC", "1a"} {
		if _, err := Compile("(?<"+name+">.)", "u"); err == nil {
			t.Errorf("(?<%+q>.) compiles, want a syntax error", name)
		}
	}
}
