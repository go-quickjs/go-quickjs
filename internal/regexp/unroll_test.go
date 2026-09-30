package regexp

import (
	"slices"
	"testing"
	"unicode/utf16"
)

// TestUnrolledRepeatsAgree pins that a repeat of one character whose required
// iterations are written out matches what the counted loop it replaces does,
// over code units and over UTF-8, forwards and in a lookbehind, greedy and
// lazy, from every start.
func TestUnrolledRepeatsAgree(t *testing.T) {
	patterns := []string{
		`^\d{4}-\d{2}-\d{2}$`, `\d{2}`, `\d{2,}`, `\d{2,}?`, `\d{2,4}`, `\d{2,4}?`,
		`a{1,3}`, `a{1,3}?b`, `a{3}`, `a{0,2}a{2}`, `[a-c]{2}c`, `.{3}`, `.{2,}x`,
		`(?<=\d{3})x`, `(?<=a{1,2})b`, `(?<=.{2,})y`, `(?<!\d{2})\d`, `(\w{2})\1`,
		`x{16}`, `x{17}`, `x{2,20}`, `😀{2}`, `[😀a]{2,3}`, `\w{2}\b`, `(?:ab){2}`,
		`(a){2}`, `\s{1,}`, `[^,]{2,}?,`, `.{1}$`,
	}
	subjects := []string{
		"", "a", "aaab", "2026-09-30", "20260930", "12345", "xx-12-345", "ab,cd,e",
		"xxxxxxxxxxxxxxxxxxxx", "aa😀😀a", "😀😀😀", "a\nbb\ncc", "abab", "x1 22y",
		"é12é", "\xed\xa0\x80a\xed\xb0\x80",
	}
	for _, flags := range []string{"", "i", "m", "s", "u", "iu", "y", "v"} {
		for _, src := range patterns {
			unrollRepeats = true
			fast, err := Compile(src, flags)
			fast8, err8 := CompileUTF8(src, flags)
			unrollRepeats = false
			slow, err2 := Compile(src, flags)
			slow8, err28 := CompileUTF8(src, flags)
			unrollRepeats = true
			if err != nil {
				if err2 == nil || err8 == nil || err28 == nil {
					t.Fatalf("/%s/%s: an error only one way: %v %v %v %v", src, flags, err, err2, err8, err28)
				}
				continue
			}
			for _, subj := range subjects {
				units := utf16.Encode([]rune(subj))
				for start := 0; start <= len(units); start++ {
					a, _ := fast.MatchCheckedInto(nil, units, start, nil)
					b, _ := slow.MatchCheckedInto(nil, units, start, nil)
					if !slices.Equal(a, b) {
						t.Errorf("/%s/%s on %q from %d: %v, counted %v", src, flags, subj, start, a, b)
					}
				}
				for start := 0; start <= len(subj); start++ {
					a, _ := fast8.MatchUTF8Into(nil, subj, start, nil)
					b, _ := slow8.MatchUTF8Into(nil, subj, start, nil)
					if !slices.Equal(a, b) {
						t.Errorf("UTF-8 /%s/%s on %q from %d: %v, counted %v", src, flags, subj, start, a, b)
					}
				}
			}
		}
	}
}
