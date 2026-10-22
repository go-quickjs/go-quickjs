package regexp

import (
	"slices"
	"testing"
	"unicode/utf16"
	"unsafe"
)

// TestQuickSplitsAgree compares patterns whose alternations and greedy
// repetitions pass over a branch that cannot begin with the unit at hand
// (instr's quick) with the
// same patterns trying every branch, at every start, over code units, the
// bytes of ASCII and UTF-8: branches of characters, classes, groups, loops
// and lookarounds, ones that can match nothing, which have no quick, under
// the i and u flags, a branch past ASCII, and the end of the subject.
func TestQuickSplitsAgree(t *testing.T) {
	patterns := []string{
		`(\d+(?:\.\d+)?)|([A-Za-z_]\w*)|(\S)`, `\s*(?:(\d+)|(\w+)|([^\s\w]))`, `a|b|c`, `ab|ac|ad`, `(?:x|)y`,
		`(?:foo|bar)+baz`, `(?=a)ab|b`, `(?<=a|b)c`, `(é|e)x`, `(?:\u{1F600}|a)b`, `(a|b)*c`, `x?y|z`,
		`(?:k|K)elvin`, `(?:[a-c]|[x-z])\1?`, `(?:\bfoo|bar\b)`, `^(?:a|b)$`, `(?i:a|b)c`, `.|\n`,
		`(?:ab)*c`, `(?:ab)+?c`, `(?:a|bc){2,4}d`, `(?:x\d)?y`, `(?:ab)+`, `(?:\u{1F600}x)*y`, `(?:é.)*`,
	}
	subjects := []string{
		"for (var i = 0; i < 100; i += 2) total = total + f(i, 3.5);", "let x = 10 * (y + 42) - z / 7;",
		"abcabc", "foobarbaz foofoobaz barbaz", "yxy", "acab", "éx ex Ex", "\U0001F600b ab", "ababc", "zy",
		"KELVIN kelvin Kelvin", "ax by cz", "foo bar", "a", "b", "", "Ac Bc", "\n", "a\U0001F600",
		"ababc abab abc", "abcbcad aaad", "x1y x1x2y y", "\U0001F600x\U0001F600xy", "éaéb", "abababab",
	}
	for _, src := range patterns {
		for _, flags := range []string{"", "i", "u", "iu", "y"} {
			quickSplits = false
			want, err := Compile(src, flags)
			quickSplits = true
			if err != nil {
				continue
			}
			got, err := Compile(src, flags)
			if err != nil {
				t.Fatal(err)
			}
			wantU8, _ := CompileUTF8(src, flags)
			gotU8, _ := CompileUTF8(src, flags)
			for _, s := range subjects {
				units := utf16.Encode([]rune(s))
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
				if wantU8 != nil && gotU8 != nil {
					for start := 0; start <= len(s); start++ {
						w, err1 := wantU8.MatchUTF8Into(nil, s, start, nil)
						g, err2 := gotU8.MatchUTF8Into(nil, s, start, nil)
						if err1 != nil || err2 != nil || !slices.Equal(w, g) {
							t.Fatalf("/%s/%s at %d of UTF-8 %q: %v (%v), want %v (%v)", src, flags, start, s, g, err2, w, err1)
						}
					}
				}
			}
		}
	}
}

// TestQuickSplitsMade pins which alternations' first branches and greedy
// repetitions' next iterations have a quick: one that consumes a unit
// before anything else, not one that can match nothing or begins with what
// can be nearly anything, nor a lookbehind's, which matches leftwards, nor
// a lazy repetition's, whose first branch is what follows it; and that an
// instruction is still 32 bytes with it.
func TestQuickSplitsMade(t *testing.T) {
	if unsafe.Sizeof(uintptr(0)) == 8 && unsafe.Sizeof(instr{}) != 32 {
		t.Errorf("an instruction is %d bytes, not 32", unsafe.Sizeof(instr{}))
	}
	for _, tc := range []struct {
		src   string
		quick int
	}{
		{`(?:ab)*c`, 1}, {`(?:ab)*?c`, 0}, {`(?:ab)+`, 1}, {`(?:ab){2,5}`, 1}, {`(?:ab)?c`, 1}, {`(?:a?b)*`, 2}, {`(?:.b)*`, 0},
		{`a|b`, 1}, {`a|b|c`, 2}, {`(?:x|)y`, 1}, {`(?:|x)y`, 0}, {`.|a`, 0}, {`(?<=a|b)c`, 0}, {`(?=x)|a`, 0}, {`a*|b`, 0},
	} {
		re, err := Compile(tc.src, "")
		if err != nil {
			t.Fatal(err)
		}
		n := 0
		for _, in := range re.prog.code {
			if in.op == opSplit && in.quick != 0 {
				n++
			}
		}
		for _, l := range re.prog.looks {
			for _, in := range l.code {
				if in.op == opSplit && in.quick != 0 {
					n++
				}
			}
		}
		if n != tc.quick {
			t.Errorf("/%s/: %d quick splits, want %d", tc.src, n, tc.quick)
		}
	}
}
