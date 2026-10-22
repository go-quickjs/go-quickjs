package regexp

import (
	"slices"
	"testing"
	"unicode/utf16"
)

// TestRunsMatchAsCharacters compares patterns whose characters are matched
// as runs (opString) with the same patterns compiled a character at a time,
// at every start, over subjects of code units and of ASCII bytes: runs in
// groups, alternatives, loops and lookarounds, runs a match fails partway
// through or that end the subject, under the unicode flag with characters
// past the Basic Multilingual Plane and lone surrogates the pattern or the
// subject has, with the i flag and in lookbehinds, which make none, and
// sticky.
func TestRunsMatchAsCharacters(t *testing.T) {
	patterns := []string{
		`abc`, `x(abc)y`, `(?:ab|abc)d`, `(?:foo)+bar`, `(?:abc){2,3}`, `a(?=bcd)bc`, `(?<=abc)de`,
		`(?!abd)ab.`, `ab|cd|ef`, `function\s+(\w+)\s*\(`, `<div class="(\w+)">`, `abcdefghij$`,
		`^the quick`, `ab*abc`, `(a)bc\1bc`, `[x]yz[x]`, `é½ab`, `naïve`, `\u{1F600}x\u{1F601}`,
		`a\uD83Db`, `\uD83D\uDE00z`, `q\uDE00`, `AbC`, `(?i:ab)cd`, `(?i)xyz`,
	}
	subjects := []string{
		"abc", "xabcy", "abcd ababcd", "foofoobar foobar", "abcabcabcabc", "abcd", "xabcde",
		"abc abd abe", "ef cd ab", "function  foo (x)", `<div class="main">`, "--abcdefghij",
		"the quick brown", "abbbabc", "abcabc", "xyzx", "é½ab", "naïve café", "\U0001F600x\U0001F601",
		"a\U0001F600b", "\U0001F600z", "q\U0001F600", "AbC abc ABC", "ABcd abcd", "XyZ", "",
		"ab", "abcab", "\U0001F600", "a\xed\xa0\xbdb",
	}
	for _, src := range patterns {
		for _, flags := range []string{"", "u", "y", "i", "uy"} {
			compileRuns = false
			want, err := Compile(src, flags)
			compileRuns = true
			if err != nil {
				continue
			}
			got, err := Compile(src, flags)
			if err != nil {
				t.Fatal(err)
			}
			for _, s := range subjects {
				units := []uint16(nil)
				for _, r := range s {
					units = utf16.AppendRune(units, r)
				}
				ascii := true
				for i := 0; i < len(s); i++ {
					ascii = ascii && s[i] < 0x80
				}
				for start := 0; start <= len(units); start++ {
					w, err1 := want.MatchCheckedInto(nil, units, start, nil)
					g, err2 := got.MatchCheckedInto(nil, units, start, nil)
					if err1 != nil || err2 != nil || !slices.Equal(w, g) {
						t.Fatalf("/%s/%s at %d of %q: %v (%v), want %v (%v)", src, flags, start, s, g, err2, w, err1)
					}
					if ascii {
						g, err2 = got.MatchASCIICheckedInto(nil, s, start, nil)
						if err2 != nil || !slices.Equal(w, g) {
							t.Fatalf("/%s/%s at %d of ASCII %q: %v (%v), want %v", src, flags, start, s, g, err2, w)
						}
					}
				}
			}
		}
	}
}

// TestRunsMade pins where runs are made: two or more characters in a row,
// rightwards and without folding, and under the unicode flag not a
// surrogate of the pattern's own.
func TestRunsMade(t *testing.T) {
	for _, tc := range []struct {
		src, flags string
		runs       int
	}{
		{`abc`, "", 1}, {`a`, "", 0}, {`ab(c)de`, "", 2}, {`abc`, "i", 0}, {`(?<=abc)x`, "", 0},
		{`\u{1F600}a`, "u", 1}, {`a\uD83Db`, "u", 0}, {`a\uD83Db`, "", 1}, {`(?=xy)z`, "", 1},
	} {
		re, err := Compile(tc.src, tc.flags)
		if err != nil {
			t.Fatal(err)
		}
		// A lookaround's runs are the program's too.
		if n := len(re.prog.runs); n != tc.runs {
			t.Errorf("/%s/%s: %d runs, want %d", tc.src, tc.flags, len(re.prog.runs), tc.runs)
		}
	}
}
