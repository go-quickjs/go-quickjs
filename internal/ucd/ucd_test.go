package ucd

import (
	"testing"
	"unicode"
)

// TestVersion pins the tables' version, and a few characters each of the
// versions after Go 1.24's 15.0 give what they gave them, so that a build with
// any toolchain answers as 17.0 does.
func TestVersion(t *testing.T) {
	if Version != "17.0.0" {
		t.Fatalf("Version = %q, want 17.0.0", Version)
	}
	for _, c := range []struct {
		r           rune
		start, part bool
		what        string
		since       string
	}{
		{0x2FFC, false, false, "IDEOGRAPHIC DESCRIPTION CHARACTER SURROUND FROM RIGHT, So", "15.1"},
		{0x31350, true, true, "CJK UNIFIED IDEOGRAPH-31350 (Extension H)", "15.0"},
		{0x2EBF0, true, true, "CJK UNIFIED IDEOGRAPH-2EBF0 (Extension I)", "15.1"},
		{0x30FB, false, true, "KATAKANA MIDDLE DOT, ID_Continue", "15.1"},
		{0xFF65, false, true, "HALFWIDTH KATAKANA MIDDLE DOT, ID_Continue", "15.1"},
		{0x200C, false, true, "ZERO WIDTH NON-JOINER, ID_Continue", "15.1"},
		{0x10D50, true, true, "GARAY CAPITAL LETTER A", "16.0"},
		{0x1C89, true, true, "CYRILLIC CAPITAL LETTER TJE", "16.0"},
		{0x16D40, true, true, "KIRAT RAI SIGN ANUSVARA", "16.0"},
		{0x11B60, false, true, "SHARADA VOWEL SIGN OE, Mn", "17.0"},
		{0x16EA0, true, true, "BERIA ERFE CAPITAL LETTER ARKAB", "17.0"},
		{0x2E2F, false, false, "VERTICAL TILDE, Lm but Pattern_Syntax", "3.0"},
		{0x2160, true, true, "ROMAN NUMERAL ONE, Nl", "1.1"},
		{0x1885, true, true, "MONGOLIAN LETTER ALI GALI BALUDA, Other_ID_Start", "9.0"},
	} {
		if got := IsIDStart(c.r); got != c.start {
			t.Errorf("IsIDStart(U+%04X %s, %s) = %v", c.r, c.what, c.since, got)
		}
		if got := IsIDContinue(c.r); got != c.part {
			t.Errorf("IsIDContinue(U+%04X %s, %s) = %v", c.r, c.what, c.since, got)
		}
	}
	for _, c := range []struct {
		r, upper, lower, fold rune
		since                 string
	}{
		{0x1C89, 0x1C89, 0x1C8A, 0x1C8A, "16.0, TJE"},
		{0x1C8A, 0x1C89, 0x1C8A, 0x1C89, "16.0, tje"},
		{0x10D50, 0x10D50, 0x10D70, 0x10D70, "16.0, Garay"},
		{0xA7CB, 0xA7CB, 0x0264, 0x0264, "16.0, RAMS HORN"},
		{0xA7DC, 0xA7DC, 0x019B, 0x019B, "16.0, LAMBDA WITH STROKE"},
		{0x16EA0, 0x16EA0, 0x16EBB, 0x16EBB, "17.0, Beria Erfe"},
		{0x1FD3, 0x1FD3, 0x1FD3, 0x0390, "15.1, folds to U+0390"},
		{0x0390, 0x0390, 0x0390, 0x1FD3, "15.1, orbit of U+1FD3"},
		{0xFB05, 0xFB05, 0xFB05, 0xFB06, "15.1, LONG S T folds to ST"},
	} {
		if got := ToUpper(c.r); got != c.upper {
			t.Errorf("ToUpper(U+%04X, %s) = U+%04X, want U+%04X", c.r, c.since, got, c.upper)
		}
		if got := ToLower(c.r); got != c.lower {
			t.Errorf("ToLower(U+%04X, %s) = U+%04X, want U+%04X", c.r, c.since, got, c.lower)
		}
		if got := SimpleFold(c.r); got != c.fold {
			t.Errorf("SimpleFold(U+%04X, %s) = U+%04X, want U+%04X", c.r, c.since, got, c.fold)
		}
	}
	if to, ok := FullUpper(0xDF); !ok || string(to) != "SS" {
		t.Errorf("FullUpper(ß) = %q, %v", string(to), ok)
	}
	if to, ok := FullLower(0x130); !ok || string(to) != "i̇" {
		t.Errorf("FullLower(İ) = %q, %v", string(to), ok)
	}
}

// TestMatchesGo compares every answer with the unicode package's, built with
// a Go whose tables are at the same version: the functions are drop-in
// replacements for Go's at 17.0, and the generator's sets and mappings are the
// ones Go derives from the same database.
func TestMatchesGo(t *testing.T) {
	if unicode.Version != Version {
		t.Skipf("Go's unicode tables are at %s, ours at %s", unicode.Version, Version)
	}
	sets := []struct {
		name string
		ours func(rune) bool
		gos  func(rune) bool
	}{
		{"ID_Start", IsIDStart, func(r rune) bool {
			return unicode.In(r, unicode.L, unicode.Nl, unicode.Other_ID_Start) &&
				!unicode.In(r, unicode.Pattern_Syntax, unicode.Pattern_White_Space)
		}},
		{"ID_Continue", IsIDContinue, func(r rune) bool {
			return unicode.In(r, unicode.L, unicode.Nl, unicode.Other_ID_Start,
				unicode.Mn, unicode.Mc, unicode.Nd, unicode.Pc, unicode.Other_ID_Continue) &&
				!unicode.In(r, unicode.Pattern_Syntax, unicode.Pattern_White_Space)
		}},
		{"Zs", IsSpaceSeparator, func(r rune) bool { return unicode.Is(unicode.Zs, r) }},
		{"Lu", IsUpper, unicode.IsUpper},
		{"Cased", IsCased, func(r rune) bool {
			return unicode.In(r, unicode.Lu, unicode.Ll, unicode.Lt,
				unicode.Other_Lowercase, unicode.Other_Uppercase)
		}},
	}
	for r := rune(0); r <= unicode.MaxRune+1; r++ {
		for _, s := range sets {
			if got, want := s.ours(r), s.gos(r); got != want {
				t.Errorf("%s(U+%04X) = %v, Go says %v", s.name, r, got, want)
			}
		}
		if got, want := ToUpper(r), unicode.ToUpper(r); got != want {
			t.Errorf("ToUpper(U+%04X) = U+%04X, Go says U+%04X", r, got, want)
		}
		if got, want := ToLower(r), unicode.ToLower(r); got != want {
			t.Errorf("ToLower(U+%04X) = U+%04X, Go says U+%04X", r, got, want)
		}
		if got, want := SimpleFold(r), unicode.SimpleFold(r); got != want {
			t.Errorf("SimpleFold(U+%04X) = U+%04X, Go says U+%04X", r, got, want)
		}
		if t.Failed() {
			return
		}
	}
}

// TestCaseIgnorable holds Case_Ignorable to its definition in Unicode's
// DerivedCoreProperties: the marks, the format characters, the modifier
// letters and symbols, and Word_Break's MidLetter, MidNumLet and
// Single_Quote, which Go has no table of and which are listed here.
func TestCaseIgnorable(t *testing.T) {
	if unicode.Version != Version {
		t.Skipf("Go's unicode tables are at %s, ours at %s", unicode.Version, Version)
	}
	word := map[rune]bool{}
	for _, r := range []rune{'\'', '.', ':', 0x00B7, 0x0387, 0x055F, 0x05F4, 0x2018, 0x2019,
		0x2024, 0x2027, 0xFE13, 0xFE52, 0xFE55, 0xFF07, 0xFF0E, 0xFF1A} {
		word[r] = true
	}
	for r := rune(0); r <= unicode.MaxRune; r++ {
		want := word[r] || unicode.In(r, unicode.Mn, unicode.Me, unicode.Cf, unicode.Lm, unicode.Sk)
		if got := IsCaseIgnorable(r); got != want {
			t.Fatalf("IsCaseIgnorable(U+%04X) = %v, want %v", r, got, want)
		}
	}
}

// benchRunes are characters past ASCII from the scripts identifiers and
// text are written in.
var benchRunes = []rune("éßΣσπжЖשׁعदनाकि日本語한글ｱ\U00010D50\U00020000ǅ")

var sink bool
var sinkRune rune

func BenchmarkIdentifier(b *testing.B) {
	b.Run("ours", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			for _, r := range benchRunes {
				sink = IsIDStart(r) || IsIDContinue(r)
			}
		}
	})
	b.Run("go", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			for _, r := range benchRunes {
				sink = unicode.In(r, unicode.L, unicode.Nl, unicode.Other_ID_Start) ||
					unicode.In(r, unicode.L, unicode.Nl, unicode.Other_ID_Start,
						unicode.Mn, unicode.Mc, unicode.Nd, unicode.Pc, unicode.Other_ID_Continue)
			}
		}
	})
}

func BenchmarkCase(b *testing.B) {
	b.Run("ours", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			for _, r := range benchRunes {
				sinkRune = ToUpper(r) + ToLower(r) + SimpleFold(r)
				sink = IsCased(r) || IsCaseIgnorable(r)
			}
		}
	})
	b.Run("go", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			for _, r := range benchRunes {
				sinkRune = unicode.ToUpper(r) + unicode.ToLower(r) + unicode.SimpleFold(r)
				sink = unicode.IsUpper(r) || unicode.IsLower(r) || unicode.IsTitle(r) ||
					unicode.In(r, unicode.Other_Lowercase, unicode.Other_Uppercase) ||
					unicode.In(r, unicode.Mn, unicode.Me, unicode.Cf, unicode.Lm, unicode.Sk)
			}
		}
	})
}
