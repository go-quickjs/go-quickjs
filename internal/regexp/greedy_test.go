package regexp

import (
	"slices"
	"testing"
	"unicode/utf16"
)

// TestGreedyLoopsAgree pins that a greedy run of one unit taken whole, and
// given back from one choice point, matches what the loop it stands for
// matches: the same span and the same captures, from every start.
func TestGreedyLoopsAgree(t *testing.T) {
	patterns := []string{
		`a*`, `a+`, `a*b`, `a+b`, `a*a`, `a+a+`, `[a-c]*c`, `.*x`, `.+?x`, `\s*(\w+)\s*`,
		`x(.*)y(.*)z`, `(a*)(a*)`, `[^,]*,`, `.*$`, `^.*\n`, `b*?b`, `(?:ab)*`, `\d+\.\d*`,
		`[A-Z]*a`, `(\S+)\s+(\S+)`, `.*.*=.*`, `(a+)+b`,
	}
	subjects := []string{
		"", "a", "aaa", "aaab", "abcabc", "xaybz", "xyz", "xxyyzz", "a,b,,c", " foo  bar ",
		"12.5", "line1\nline2", "AAAa", "lone surrogates", "==a=b", "aaaaaaaaaaaaaaaaaaaac",
	}
	for _, flags := range []string{"", "i", "m", "s", "y", "g"} {
		for _, src := range patterns {
			greedyLoops = true
			fast, err := Compile(src, flags)
			greedyLoops = false
			slow, err2 := Compile(src, flags)
			greedyLoops = true
			if err != nil || err2 != nil {
				t.Fatalf("/%s/%s: %v %v", src, flags, err, err2)
			}
			for _, subj := range subjects {
				units := utf16.Encode([]rune(subj))
				if subj == "lone surrogates" {
					units = []uint16{0xD800, 'a', 0xDC00}
				}
				for start := 0; start <= len(units); start++ {
					a, _ := fast.MatchCheckedInto(nil, units, start, nil)
					b, _ := slow.MatchCheckedInto(nil, units, start, nil)
					if !slices.Equal(a, b) {
						t.Errorf("/%s/%s on %q from %d: %v, as a loop %v", src, flags, subj, start, a, b)
					}
				}
			}
		}
	}
}
