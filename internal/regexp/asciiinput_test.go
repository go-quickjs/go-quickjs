package regexp

import (
	"math/rand"
	"slices"
	"testing"
)

// TestASCIIInputAgrees pins that matching an ASCII string's bytes in place
// finds what matching its UTF-16 code units does: the same match, from every
// start, for patterns that use every kind of instruction.
func TestASCIIInputAgrees(t *testing.T) {
	patterns := []struct{ src, flags string }{
		{"a", ""}, {"abc", ""}, {"a|b", ""}, {"(a|bc)+d", ""}, {"a*b", ""}, {"a*", ""}, {"a??b", ""},
		{"[a-c]x", ""}, {"[^a]", ""}, {"\\d+", ""}, {"\\w+@\\w+", ""}, {"\\W", ""}, {"\\s*k", ""},
		{"^a", "m"}, {"a$", "m"}, {"^a", ""}, {"$", ""}, {"\\bk", ""}, {"\\Bk", ""}, {"(?=k)\\w", ""},
		{"(?!a)b", ""}, {"(?<=a)b", ""}, {"(?<!a)b", ""}, {"(?<=\\w{2})c", ""}, {"(a)\\1", ""},
		{"(?<n>[ab])\\k<n>", ""}, {"(a)|b", ""}, {"k", "i"}, {"[a-z]+", "i"}, {"(?i:k)x", ""},
		{"K", "iu"}, {"\\p{L}+", "u"}, {"\\P{L}", "u"}, {".", "u"}, {".", "s"}, {"a{2,3}", ""},
		{"(a{0,2}b){2}", ""}, {"(?:)", ""}, {"x|", ""}, {"(x|)y", ""}, {"a", "y"}, {"b+", "y"},
		{"[\\q{ab}c]", "v"}, {"[\\w--\\d]+", "v"}, {"(a*)*b", ""}, {"(?:a|ab)(?:c|bcd)", ""},
		{"ab(?=c)|ab", ""}, {"[^\\n]*\\n", ""}, {"\\x41", "i"}, {"needle", ""}, {"(^|[^\\\\])\"a", ""},
	}
	alphabet := []string{"a", "b", "c", "d", "k", "K", "s", "x", "y", "1", "7", "@", " ", "\n", "\\", "\"",
		"ab", "abc", "needle", "A", "_", "-"}
	r := rand.New(rand.NewSource(3))
	var subjects []string
	for i := 0; i < 300; i++ {
		s := ""
		for j := r.Intn(14); j >= 0; j-- {
			s += alphabet[r.Intn(len(alphabet))]
		}
		subjects = append(subjects, s)
	}
	subjects = append(subjects, "")
	for _, p := range patterns {
		re, err := Compile(p.src, p.flags)
		if err != nil {
			t.Fatalf("/%s/%s: %v", p.src, p.flags, err)
		}
		for _, s := range subjects {
			units := make([]uint16, len(s))
			for i := range s {
				units[i] = uint16(s[i])
			}
			for start := 0; start <= len(s)+1; start++ {
				got, err1 := re.MatchASCIIInto(nil, s, start, nil)
				want, err2 := re.MatchCheckedInto(nil, units, start, nil)
				if err1 != nil || err2 != nil || !slices.Equal(got, want) {
					t.Fatalf("/%s/%s on %q from %d: bytes %v, %v; code units %v, %v",
						p.src, p.flags, s, start, got, err1, want, err2)
				}
			}
		}
	}
}
