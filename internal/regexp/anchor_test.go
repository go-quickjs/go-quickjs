package regexp

import (
	"slices"
	"testing"
	"unicode/utf16"
)

// TestAnchoredStartsAgree pins that a pattern tried only at the start of the
// subject, because it begins with ^, matches what it does when every position
// is tried, over code units and over UTF-8, from every start.
func TestAnchoredStartsAgree(t *testing.T) {
	patterns := []string{
		`^a`, `^`, `^$`, `^a|b`, `^a|^b`, `(?:^a|^b)c`, `(^a)+`, `(?:^a)*b`, `(?:^)?a`,
		`\b^a`, `(?=a)^a`, `(?<=x)^a`, `(?!b)^.`, `(^|x)a`, `(?m:^a)`, `(?-m:^a)`,
		`^\d{4}-\d{2}-\d{2}$`, `^.*b`, `(?:)^a`, `^(?:a|b)+$`, `x|^`, `a^`, `(?<=^)a`,
		`^\w+\s`, `(?i:^A)`, `(?:^a){0,2}b`,
	}
	subjects := []string{
		"", "a", "ab", "ba", "aab", "b\na", "x\nab", "2026-09-30", "2026-09-30x",
		"abab", "c", "é\na", "😀a", "\nb",
	}
	for _, flags := range []string{"", "m", "i", "s", "u", "y", "mu"} {
		for _, src := range patterns {
			anchorStarts = true
			fast, err := Compile(src, flags)
			fast8, err8 := CompileUTF8(src, flags)
			anchorStarts = false
			slow, err2 := Compile(src, flags)
			slow8, err28 := CompileUTF8(src, flags)
			anchorStarts = true
			if err != nil || err2 != nil || err8 != nil || err28 != nil {
				t.Fatalf("/%s/%s: %v %v %v %v", src, flags, err, err2, err8, err28)
			}
			for _, subj := range subjects {
				units := utf16.Encode([]rune(subj))
				for start := 0; start <= len(units); start++ {
					a, _ := fast.MatchCheckedInto(nil, units, start, nil)
					b, _ := slow.MatchCheckedInto(nil, units, start, nil)
					if !slices.Equal(a, b) {
						t.Errorf("/%s/%s on %q from %d: %v, at every position %v", src, flags, subj, start, a, b)
					}
				}
				for start := 0; start <= len(subj); start++ {
					a, _ := fast8.MatchUTF8Into(nil, subj, start, nil)
					b, _ := slow8.MatchUTF8Into(nil, subj, start, nil)
					if !slices.Equal(a, b) {
						t.Errorf("UTF-8 /%s/%s on %q from %d: %v, at every position %v", src, flags, subj, start, a, b)
					}
				}
			}
		}
	}
}

// TestAnchoredStart pins which patterns are tried at the start alone.
func TestAnchoredStart(t *testing.T) {
	for src, want := range map[string]bool{
		`^a`: true, `^a|^b`: true, `(^a)+`: true, `\b^a`: true, `(?-m:^a)`: true,
		`^a|b`: false, `(?:^)?a`: false, `(^|x)a`: false, `(?m:^a)`: false, `a^`: false,
		`(?:^a)*b`: false, `x|^`: false,
	} {
		re, err := Compile(src, "")
		if err != nil {
			t.Fatal(err)
		}
		if re.prog.anchored != want {
			t.Errorf("/%s/: anchored = %v, want %v", src, re.prog.anchored, want)
		}
	}
	re, err := Compile(`^a`, "m")
	if err != nil {
		t.Fatal(err)
	}
	if re.prog.anchored {
		t.Errorf("/^a/m is anchored, but ^ matches after every line break")
	}
}
