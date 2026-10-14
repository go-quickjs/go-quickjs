package vm

import (
	"strings"
	"testing"
	"unicode/utf8"

	intl "github.com/go-quickjs/go-intl"
)

// TestNormalizeExpansionBound pins maxDecomposition: no code point becomes
// more than 18 times as many UTF-16 code units in any normal form, which is
// what lets normalizedTooLong leave a string shorter than maxStringLength/18
// unwalked.
func TestNormalizeExpansionBound(t *testing.T) {
	if testing.Short() {
		t.Skip("normalizes every code point")
	}
	n, err := intl.NewNormalizer()
	if err != nil {
		t.Fatal(err)
	}
	most, at := 0, rune(0)
	for r := rune(0); r <= utf8.MaxRune; r++ {
		if r >= 0xD800 && r <= 0xDFFF {
			continue
		}
		in := 1
		if r >= 0x10000 {
			in = 2
		}
		for _, form := range []intl.NormalizationForm{intl.NFD, intl.NFKD} {
			out := utf16Length(n.Normalize(string(r), form))
			if out > maxDecomposition*in {
				t.Fatalf("U+%04X becomes %d code units, more than %d times its %d", r, out, maxDecomposition, in)
			}
			if out > most*in {
				most, at = out/in, r
			}
		}
	}
	if most != maxDecomposition {
		t.Errorf("the most a code unit becomes is %d (U+%04X); maxDecomposition is %d", most, at, maxDecomposition)
	}
}

// TestNormalizedTooLong pins that normalize's bound tells the forms apart:
// U+FDFA, eighteen characters by compatibility and itself canonically, is
// too long in NFKD and NFKC and fits in NFC and NFD at thirty million.
func TestNormalizedTooLong(t *testing.T) {
	if testing.Short() {
		t.Skip("walks a string of thirty million characters")
	}
	n, err := intl.NewNormalizer()
	if err != nil {
		t.Fatal(err)
	}
	s := NewString(strings.Repeat("ﷺ", 30_000_000))
	for form, want := range map[intl.NormalizationForm]bool{intl.NFKD: true, intl.NFKC: true, intl.NFD: false, intl.NFC: false} {
		if got := normalizedTooLong(n, s, form); got != want {
			t.Errorf("form %v: too long %v, want %v", form, got, want)
		}
	}
	// Short enough that no form can take it past the limit: not walked.
	if normalizedTooLong(n, NewString(strings.Repeat("ﷺ", maxStringLength/maxDecomposition)), intl.NFKD) {
		t.Error("a string no form can take past the limit was too long")
	}
}
