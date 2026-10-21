package regexp

import (
	"slices"
	"testing"
	"unicode/utf16"
)

// TestGreedyLoopsAgree pins that a greedy run of one unit taken whole, and
// given back from one choice point, matches what the loop it stands for
// matches: the same span and the same captures, from every start -- and so
// does a lazy run, taking one more from one choice point.
func TestGreedyLoopsAgree(t *testing.T) {
	patterns := []string{
		`a*`, `a+`, `a*b`, `a+b`, `a*a`, `a+a+`, `[a-c]*c`, `.*x`, `.+?x`, `\s*(\w+)\s*`,
		`x(.*)y(.*)z`, `(a*)(a*)`, `[^,]*,`, `.*$`, `^.*\n`, `b*?b`, `(?:ab)*`, `\d+\.\d*`,
		`[A-Z]*a`, `(\S+)\s+(\S+)`, `.*.*=.*`, `(a+)+b`,
		`a*?b`, `a+?b`, `(.*?),`, `<(.+?)>`, `x.*?y.*?z`, `\w+?\b`, `a+?$`, `\d*?\.\d+?`, `(a+?)(a*?)`,
		`[^,]*?,`, `^.*?$`, `.+?`, `(?:a.*?)+b`,
	}
	subjects := []string{
		"", "a", "aaa", "aaab", "abcabc", "xaybz", "xyz", "xxyyzz", "a,b,,c", " foo  bar ",
		"12.5", "line1\nline2", "AAAa", "lone surrogates", "==a=b", "aaaaaaaaaaaaaaaaaaaac",
	}
	// Under the unicode flag a run is of characters, which a pair of
	// surrogates is one of: the subjects have pairs, lone halves and pairs
	// a search may begin between.
	upatterns := []string{
		`\p{L}+`, `\p{L}*x`, `.*`, `.+.`, `.*\u{1F600}`, `[^a]*a`, `[\u{1F600}-\u{1F64F}]+\u{1F64F}`,
		`\u{1F600}*\u{1F600}`, `(\S+)\s+(\S+)`, `[a-z]+`, `\w*k`, `\W+`, `\uD83D*`, `[\uD800-\uDFFF]+`,
		`.*\uDE00`, `[^x]+[\uDC00-\uDFFF]`, `\S*\uDE00`,
		`.*?\u{1F600}`, `\p{L}+?$`, `.+?\uDE00`, `(.+?)\u{1F64F}`, `[^a]*?a`, `\S+?\s`,
	}
	usubjects := []string{
		"", "a\U0001F600b", "\U0001F600\U0001F64F\U0001F600", "xy\U0001F600z\U0001F64Fa", "kK\u212Aſs",
		"éa\U0001F600 b\U0001F64Fc d", "lone surrogates", "\U0001F600\U0001F600x",
	}
	for _, flags := range []string{"u", "iu", "su", "mu", "uy", "v", "iv"} {
		for _, src := range append(append([]string(nil), patterns...), upatterns...) {
			if _, err := Compile(src, flags); err != nil && flags[0] == 'v' {
				// The v flag reads some of them otherwise: [^,] is no
				// class there.
				continue
			}
			greedyLoops = true
			fast, err := Compile(src, flags)
			greedyLoops = false
			slow, err2 := Compile(src, flags)
			greedyLoops = true
			if err != nil || err2 != nil {
				t.Fatalf("/%s/%s: %v %v", src, flags, err, err2)
			}
			for _, subj := range append(append([]string(nil), subjects...), usubjects...) {
				units := utf16.Encode([]rune(subj))
				if subj == "lone surrogates" {
					units = []uint16{0xD800, 'a', 0xDC00, 0xD83D, 0xDE00, 0xDE00, 0xD83D}
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
