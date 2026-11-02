package regexp

import (
	"math/rand"
	"testing"
)

// MissesASCII says a match finds nothing only where MatchASCIICheckedInto
// finds nothing, and says so, without matching, for a pattern anchored at
// the start whose first unit is not the subject's, and for one whose
// required literal, or whose one or two first units, the subject lacks.
func TestMissesASCII(t *testing.T) {
	cases := []struct {
		pattern, flags, s string
		start             int
		miss              bool
	}{
		// Anchored, a first unit or two: the subject's first unit decides.
		{`^ba`, "", "pyvpx", 0, true},
		{`^ba`, "", "bax", 0, false},
		{`^ba`, "", "bb", 0, false},
		{`^ba`, "", "", 0, true},
		{`^ba`, "", "xba", 1, true},
		{`^[ab]c`, "", "cab", 0, true},
		{`^[ab]c`, "", "bc", 0, false},
		{`^(?:x|y)z`, "", "zz", 0, true},
		{`^(?<=q)x`, "", "x", 0, false},
		{`^\s*(\S+)`, "", "  a", 0, false},
		// No first unit (it can match the empty string, or the m flag
		// lets ^ match after a line terminator): not known.
		{`^a*`, "", "b", 0, false},
		{`^ba`, "m", "x\nba", 0, false},
		{`^ba|c`, "", "xc", 0, false},
		// A required literal, not anchored.
		{`\bQBZ\b`, "", "vachggrkg", 0, true},
		{`\bQBZ\b`, "", "a QBZ b", 0, false},
		{`x.{0,3}abc`, "", "xxxabd", 0, true},
		{`x.{0,3}abc`, "", "x12abc", 0, false},
		{`x.{0,3}abc`, "", "x12abc", 1, true},
		// One first unit or two, not anchored.
		{`a\d+`, "", "bcd", 0, true},
		{`a\d+`, "", "a", 0, false},
		{`a\d+`, "", "a1", 1, true},
		{`[qz]\d`, "", "abc", 0, true},
		{`[qz]\d`, "", "abz", 0, false},
		// Of one literal or one class: matched where they are already.
		{`abc`, "", "x", 0, false},
		{`[<>]`, "", "x", 0, false},
		// A start past the subject.
		{`a\d+`, "", "a1", 3, false},
	}
	for _, c := range cases {
		re, err := Compile(c.pattern, c.flags)
		if err != nil {
			t.Fatal(err)
		}
		if got := MissesASCII[struct{}](re, c.s, c.start); got != c.miss {
			t.Errorf("MissesASCII(/%s/%s, %q, %d) = %v, want %v", c.pattern, c.flags, c.s, c.start, got, c.miss)
		}
		if c.start <= len(c.s) {
			caps, err := re.MatchASCIICheckedInto(nil, c.s, c.start, nil)
			if err != nil {
				t.Fatal(err)
			}
			if c.miss && caps != nil {
				t.Errorf("/%s/%s of %q from %d matched %v, said to miss", c.pattern, c.flags, c.s, c.start, caps)
			}
		}
	}
	// Patterns of every kind, against random subjects of their units: a
	// miss is never said where there is a match.
	patterns := []string{`^ba`, `^[ab]+c`, `^(a|bc)d`, `a\d`, `[ab]c`, `ab+c`, `\bab\b`, `x.?yz`, `(?:ab|ac)d`, `^(?=a)ab`, `(a)|b`, `^$`, `^.`, `a(?!b)`}
	rng := rand.New(rand.NewSource(1))
	const units = "abcdxyz01 \n"
	misses := 0
	for _, p := range patterns {
		for _, fl := range []string{"", "i", "m", "s"} {
			re, err := Compile(p, fl)
			if err != nil {
				t.Fatal(err)
			}
			for range 300 {
				b := make([]byte, rng.Intn(6))
				for i := range b {
					b[i] = units[rng.Intn(len(units))]
				}
				s := string(b)
				start := rng.Intn(len(s) + 1)
				if !MissesASCII[struct{}](re, s, start) {
					continue
				}
				misses++
				if caps, err := re.MatchASCIICheckedInto(nil, s, start, nil); err != nil || caps != nil {
					t.Fatalf("/%s/%s of %q from %d: %v %v, said to miss", p, fl, s, start, caps, err)
				}
			}
		}
	}
	if misses == 0 {
		t.Fatal("no miss said")
	}
}
