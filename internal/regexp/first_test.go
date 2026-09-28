package regexp

import (
	"math/rand"
	"slices"
	"testing"
)

// TestFirstUnitsAgree pins that passing over the positions no match can begin
// at changes nothing: each pattern finds the same match, from every start,
// with its first units as without them. The subjects are made of what could
// trip the reasoning: both cases of a letter, the Kelvin sign and the long s,
// which fold to ASCII letters, surrogate pairs and lone surrogates, and line
// terminators.
func TestFirstUnitsAgree(t *testing.T) {
	patterns := []struct{ src, flags string }{
		{"a", ""}, {"abc", ""}, {"a|b", ""}, {"(a|bc)+d", ""}, {"a*b", ""}, {"a*", ""}, {"a?", ""},
		{"[a-c]x", ""}, {"[^a]", ""}, {"\\d+", ""}, {"\\w+@\\w+", ""}, {"\\W", ""}, {"\\s*k", ""},
		{"^a", "m"}, {"a$", "m"}, {"\\bk", ""}, {"(?=k)\\w", ""}, {"(?<=a)b", ""}, {"(?!a)b", ""},
		{"k", "i"}, {"K", "i"}, {"s", "i"}, {"[a-z]", "i"}, {"(?i:k)x", ""}, {"(?-i:k)", "i"},
		{"k", "iu"}, {"s", "iu"}, {"K", "iu"}, {"ſ", "iu"}, {"K", "i"},
		{"\U0001F600", "u"}, {"\U0001F600", ""}, {".\U0001F600", "u"}, {"[\U0001F600-\U0001F64F]", "u"},
		{"\\p{L}", "u"}, {"\\P{L}k", "u"}, {"(a)\\1", ""}, {"(?:)", ""}, {"a{0}b", ""}, {"a{2,3}", ""},
		{"x|", ""}, {"(x|)y", ""}, {"[]a", ""}, {"\\n\\w", ""}, {"a", "y"}, {"b", "y"}, {"é", ""}, {"é", "i"},
		{"[\\q{ab}c]", "v"}, {"[a&&[a-c]]", "v"},
	}
	alphabet := []string{"a", "b", "c", "d", "k", "K", "s", "S", "x", "y", "é", "É", "1", "@", " ", "\n",
		"K", "ſ", " ", "\U0001F600", "\U0001F64F", "<hi>", "<lo>"}
	r := rand.New(rand.NewSource(1))
	var subjects [][]uint16
	for i := 0; i < 300; i++ {
		var units []uint16
		for j := r.Intn(12); j >= 0; j-- {
			// A Go string cannot hold a lone surrogate, so these two stand
			// for one each.
			a := alphabet[r.Intn(len(alphabet))]
			switch a {
			case "<hi>":
				units = append(units, 0xD83D)
				continue
			case "<lo>":
				units = append(units, 0xDE00)
				continue
			}
			for _, c := range a {
				if c > 0xFFFF {
					c -= 0x10000
					units = append(units, uint16(0xD800+c>>10), uint16(0xDC00+c&0x3FF))
				} else {
					units = append(units, uint16(c))
				}
			}
		}
		subjects = append(subjects, units)
	}
	filtered := 0
	for _, p := range patterns {
		with, err := Compile(p.src, p.flags)
		if err != nil {
			t.Fatalf("/%s/%s: %v", p.src, p.flags, err)
		}
		without, _ := Compile(p.src, p.flags)
		without.prog = &program{}
		*without.prog = *with.prog
		without.prog.first = nil
		if with.prog.first != nil {
			filtered++
		}
		for _, s := range subjects {
			for start := 0; start <= len(s); start++ {
				got, err1 := with.Match(s, start)
				want, err2 := without.Match(s, start)
				if err1 != nil || err2 != nil || !slices.Equal(got, want) {
					t.Fatalf("/%s/%s on %q from %d: %v, %v; without first units %v, %v",
						p.src, p.flags, s, start, got, err1, want, err2)
				}
			}
		}
	}
	// The test means nothing if nothing is passed over.
	if filtered < len(patterns)/2 {
		t.Errorf("only %d of %d patterns have first units", filtered, len(patterns))
	}
}
