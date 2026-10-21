package regexp

import (
	"math/rand"
	"slices"
	"strings"
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
				// With the matcher the pattern lends, as a match of code
				// units has it, and searching whatever the sticky flag says.
				got, err1 = re.MatchASCIICheckedInto(nil, s, start, nil)
				if err1 != nil || !slices.Equal(got, want) {
					t.Fatalf("/%s/%s on %q from %d: bytes with the lent matcher %v, %v; code units %v",
						p.src, p.flags, s, start, got, err1, want)
				}
				got, err1 = re.SearchASCIICheckedInto(nil, s, start, nil)
				want, err2 = re.SearchCheckedInto(nil, units, start, nil)
				if err1 != nil || err2 != nil || !slices.Equal(got, want) {
					t.Fatalf("/%s/%s on %q from %d: a search of bytes %v, %v; of code units %v, %v",
						p.src, p.flags, s, start, got, err1, want, err2)
				}
			}
		}
	}
}

// TestASCIILentMatcher pins what a match of an ASCII subject with the lent
// matcher leaves: the subject not held on to, and the backtracking state it
// grew given back to the matcher it borrowed it from -- moved off the core
// once large, as a match of code units moves it -- for the next match.
func TestASCIILentMatcher(t *testing.T) {
	re, err := Compile("(a|b)*c", "")
	if err != nil {
		t.Fatal(err)
	}
	clone := re.Clone()
	long := strings.Repeat("ab", 2000) + "c"
	if got, _ := clone.MatchASCIICheckedInto(nil, long, 0, nil); !slices.Equal(got, []int{0, len(long), len(long) - 2, len(long) - 1}) {
		t.Fatalf("match = %v", got)
	}
	m := clone.scratch
	if m == nil || clone.lent != nil || cap(m.stack) <= maxLentFrames && cap(m.trail) <= maxLentFrames {
		t.Fatalf("a large matcher was not moved to the clone: scratch %v, lent %v", m != nil, clone.lent != nil)
	}
	if a := m.bytes; a == nil || a.input.units != nil || a.in != nil || a.stack != nil || a.trail != nil || a.caps != nil {
		t.Fatal("the matcher over bytes holds on to the subject or its storage")
	}
	if got, _ := clone.MatchASCIICheckedInto(nil, "abc", 0, nil); !slices.Equal(got, []int{0, 3, 1, 2}) {
		t.Fatalf("match after = %v", got)
	}
	if clone.scratch != m {
		t.Fatal("the next match did not use the matcher kept")
	}
}
