package regexp

import (
	"errors"
	"slices"
	"testing"
	"time"
	"unicode/utf16"
	"unicode/utf8"
)

// nestedCountedCases are counted quantifiers inside counted quantifiers,
// whose bodies can match nothing, and their matches as Node finds them
// (exec with the d flag, every match where the pattern is global): the
// bounds of the whole match and of each group, -1 for a group that took no
// part, nil for no match.
//
// The inner loop is entered afresh at each of the outer one's iterations.
// Its count was set to 0 there without the count it had being kept, so
// that backtracking into an earlier iteration's inner loop resumed with a
// later iteration's count, ran iterations it had run, and left the same
// choice point again: the match never ended (KI-81).
var nestedCountedCases = []struct {
	src, flags, s string
	want          [][]int
}{
	// KI-81's table.
	{"((a?){2}){0,2}x", "", "a", [][]int{nil}},
	{"((a?){2}){0,3}a", "", "aa", [][]int{{0, 2, 0, 1, 1, 1}}},
	{"(([a-z]?){2,}){0,3}[a-z]", "", "abc", [][]int{{0, 3, 0, 2, 1, 2}}},
	{"((a?){2}){0,1}x", "", "a", [][]int{nil}},
	{"((a?){2}){2}", "", "aaa", [][]int{{0, 3, 2, 3, 3, 3}}},
	{"((a){2,}){0,3}[a-z]", "", "a", [][]int{{0, 1, -1, -1, -1, -1}}},
	{"((a?)*){0,2}b", "", "aab", [][]int{{0, 3, 0, 2, 1, 2}}},
	// Deeper, wider, without groups, global and sticky.
	{"(?:(?:a?){2}){0,2}x", "", "a", [][]int{nil}},
	{"(((a?){2}){0,2}){0,2}x", "", "a", [][]int{nil}},
	{"(((a?){2}){1,2}){0,3}x", "", "aa", [][]int{nil}},
	{"((a?){3,5}){0,4}y", "", "aaaa", [][]int{nil}},
	{"((a?){2,3}){0,3}a", "", "aaaaa", [][]int{{0, 5, 3, 4, 4, 4}}},
	{"((a|){2}){0,2}x", "", "a", [][]int{nil}},
	{"((a?b?){2}){0,3}c", "", "abab", [][]int{nil}},
	{"((a?b?){2}){0,3}c", "", "ababc", [][]int{{0, 5, 0, 4, 2, 4}}},
	{"((a?){2}){0,2}x", "", "aaaaaaaa", [][]int{nil}},
	{"((a?){2}){1,3}", "", "aaaaa", [][]int{{0, 5, 4, 5, 5, 5}}},
	{"(?:((a?){2})|b){0,3}c", "", "abab", [][]int{nil}},
	{"((a?){2}){0,2}x", "g", "aaxaxa", [][]int{{0, 3, 0, 2, 1, 2}, {3, 5, 3, 4, 4, 4}}},
	{"((a?){2}){0,2}", "g", "aaaaa", [][]int{{0, 4, 2, 4, 3, 4}, {4, 5, 4, 5, 5, 5}, {5, 5, -1, -1, -1, -1}}},
	{"((a?){2}){0,2}a", "y", "aaa", [][]int{{0, 3, 0, 2, 1, 2}}},
	// Lazy.
	{"((a?){2}){0,2}?x", "", "a", [][]int{nil}},
	{"((a??){2}){0,2}x", "", "a", [][]int{nil}},
	{"((a??){2}){0,3}a", "", "aa", [][]int{{0, 2, 0, 1, 0, 1}}},
	{"((a?){1,2}?){0,3}?a", "", "aa", [][]int{{0, 1, -1, -1, -1, -1}}},
	{"((a?){2}){0,3}?a$", "", "aaa", [][]int{{0, 3, 0, 2, 1, 2}}},
	{"(([a-z]??){2,}?){0,3}?[a-z]", "", "abc", [][]int{{0, 1, -1, -1, -1, -1}}},
	// Lookarounds.
	{"(?=((a?){2}){0,2}x)", "", "a", [][]int{nil}},
	{"(?=((a?){2}){0,2}x)", "", "aax", [][]int{{0, 0, 0, 2, 1, 2}}},
	{"(?!((a?){2}){0,2}x)a", "", "a", [][]int{{0, 1, -1, -1, -1, -1}}},
	{"(?<=((a?){2}){0,2})x", "", "aax", [][]int{{2, 3, 0, 2, 0, 1}}},
	{"(?<=((a?){2}){0,2}b)x", "", "ax", [][]int{nil}},
	{"((?=a)?(a?){2}){0,2}x", "", "a", [][]int{nil}},
	{"((?:(?=(a))a?){2}){0,3}b", "", "aab", [][]int{{0, 3, 0, 2, 1, 2}}},
	{"((?<=a|^)(a?){2}){0,3}b", "", "aab", [][]int{{0, 3, 0, 2, 1, 2}}},
	// Backreferences.
	{"((a?){2}\\2){0,3}b", "", "aab", [][]int{{0, 3, 1, 2, 2, 2}}},
	{"((a?)\\2{2}){0,3}x", "", "aa", [][]int{nil}},
	{"(a?)((\\1){2}){0,2}x", "", "aa", [][]int{nil}},
	{"((a?){2}){0,2}\\1x", "", "a", [][]int{nil}},
	{"(?<n>(a?){2}){0,3}\\k<n>b", "", "aab", [][]int{{0, 3, 0, 1, 1, 1}}},
	// Flags.
	{"((a?){2}){0,2}x", "u", "a", [][]int{nil}},
	{"((\\u{1F600}?){2}){0,2}x", "u", "\U0001f600", [][]int{nil}},
	{"((\\u{1F600}?){2}){0,3}\\u{1F600}", "u", "\U0001f600\U0001f600", [][]int{{0, 4, 0, 2, 2, 2}}},
	{"(([a-z]?){2,}){0,3}[a-z]", "v", "abc", [][]int{{0, 3, 0, 2, 1, 2}}},
	{"((\\p{L}?){2}){0,2}x", "v", "é", [][]int{nil}},
	{"(([\\p{L}--[a-z]]?){2}){0,3}\\p{L}", "v", "ÉÉ", [][]int{{0, 2, 0, 1, 1, 1}}},
	{"((é?){2}){0,2}x", "", "é", [][]int{nil}},
	{"((é?){2}){0,3}é", "", "éé", [][]int{{0, 2, 0, 1, 1, 1}}},
	{"((A?){2}){0,2}x", "i", "a", [][]int{nil}},
	{"((a?){2}){0,3}A", "i", "aa", [][]int{{0, 2, 0, 1, 1, 1}}},
	{"(([A-Z]?){2,}){0,3}[a-z]", "i", "ABC", [][]int{{0, 3, 0, 2, 1, 2}}},
	{"((É?){2}){0,3}é", "iu", "éé", [][]int{{0, 2, 0, 1, 1, 1}}},
	{"((K?){2}){0,3}k", "iu", "KK", [][]int{{0, 2, 0, 1, 1, 1}}},
	{"((a?){2}){0,2}x", "s", "a", [][]int{nil}},
	{"^((a?){2}){0,2}$", "m", "a\naa", [][]int{{0, 1, 0, 1, 1, 1}}},
}

// errNestedDeadline is what a match of nestedCountedCases is stopped with
// once it has run past its deadline.
var errNestedDeadline = errors.New("deadline")

// deadlineCheck is a host's check, failing once d has passed from now.
func deadlineCheck(d time.Duration) func() error {
	end := time.Now().Add(d)
	return func() error {
		if time.Now().After(end) {
			return errNestedDeadline
		}
		return nil
	}
}

// utf16Caps is caps, in byte offsets of s, in UTF-16 code units.
func utf16Caps(s string, caps []int) []int {
	if caps == nil {
		return nil
	}
	out := make([]int, len(caps))
	for i, b := range caps {
		if b < 0 {
			out[i] = b
			continue
		}
		n := 0
		for _, r := range s[:b] {
			n += utf16.RuneLen(r)
		}
		out[i] = n
	}
	return out
}

// Every case ends, at once, with Node's matches, in each of the matchers --
// over UTF-16 code units, over an ASCII subject's bytes and over UTF-8 --
// both under a host's deadline and under the step budget a host without
// one gets, which must not take a pattern that ends for one too complex.
func TestNestedCountedQuantifiersEnd(t *testing.T) {
	checks := map[string]func() func() error{
		"deadline": func() func() error { return deadlineCheck(5 * time.Second) },
		"budget":   func() func() error { return nil },
	}
	for _, c := range nestedCountedCases {
		re, err := Compile(c.src, c.flags)
		if err != nil {
			t.Fatalf("/%s/%s: %v", c.src, c.flags, err)
		}
		u8, err := CompileUTF8(c.src, c.flags)
		if err != nil {
			t.Fatalf("/%s/%s for UTF-8: %v", c.src, c.flags, err)
		}
		units := utf16.Encode([]rune(c.s))
		for kind, check := range checks {
			forms := map[string]func(int) ([]int, error){
				"UTF-16": func(start int) ([]int, error) { return re.SearchCheckedInto(nil, units, start, check()) },
				"UTF-8": func(start int) ([]int, error) {
					caps, err := u8.MatchUTF8Into(nil, c.s, start, check())
					return caps, err
				},
			}
			if isASCIIString(c.s) {
				forms["ASCII"] = func(start int) ([]int, error) { return re.SearchASCIICheckedInto(nil, c.s, start, check()) }
			}
			for name, match := range forms {
				// Matched more than once, so that a pattern given code of
				// its own after some matches has its code's matches
				// checked too.
				for i := 0; i < 3; i++ {
					got := matchesOf(t, re, c.s, match)
					if name == "UTF-8" {
						for j := range got {
							got[j] = utf16Caps(c.s, got[j])
						}
					}
					if !slices.EqualFunc(got, c.want, slices.Equal[[]int]) {
						t.Errorf("/%s/%s on %q (%s, %s): %v, want %v", c.src, c.flags, c.s, name, kind, got, c.want)
					}
				}
			}
		}
	}
}

// The repetitions KI-81 met end in a few steps: the counted loop inside
// the other runs each of its entries' iterations once, rather than again
// from every choice point an earlier entry left.
func TestNestedCountedQuantifiersSteps(t *testing.T) {
	for _, c := range nestedCountedCases[:3] {
		re, err := Compile(c.src, c.flags)
		if err != nil {
			t.Fatal(err)
		}
		m := re.newMatcher()
		m.input = input{units: utf16.Encode([]rune(c.s))}
		m.ready(re.prog, nil)
		m.steps, m.checkAt = 0, checkInterval
		if _, err := m.search(re, nil, 0, false); err != nil {
			t.Fatalf("/%s/ on %q: %v", c.src, c.s, err)
		}
		if m.steps > 1000 {
			t.Errorf("/%s/ on %q: %d steps", c.src, c.s, m.steps)
		}
	}
}

// isASCIIString reports whether s is ASCII alone.
func isASCIIString(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= utf8.RuneSelf {
			return false
		}
	}
	return true
}
