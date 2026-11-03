package regexp

import (
	"slices"
	"testing"
	"unicode/utf16"
)

// optionalCases are patterns with groups inside a ? -- alone, in loops, in
// lookarounds, referred back to -- and their matches, as Node finds them
// (exec with the d flag, every match where the pattern is global): the
// bounds of the whole match and of each group, -1 for a group that took no
// part.
var optionalCases = []struct {
	src, flags, s string
	want          [][]int
}{
	{"(a)?b", "", "ab", [][]int{{0, 2, 0, 1}}},
	{"(a)?b", "g", "abb", [][]int{{0, 2, 0, 1}, {2, 3, -1, -1}}},
	{"(?:(a)?b)+", "", "abb", [][]int{{0, 3, -1, -1}}},
	{"((a)?x)*", "", "axx", [][]int{{0, 3, 2, 3, -1, -1}}},
	{"(?:x(y)?)*z", "", "xyxz", [][]int{{0, 4, -1, -1}}},
	{"(?:(a)?b){2}", "", "abb", [][]int{{0, 3, -1, -1}}},
	{"(?:(a)?b){1,3}", "", "abbab", [][]int{{0, 5, 3, 4}}},
	{"(?=(a))?a", "", "a", [][]int{{0, 1, -1, -1}}},
	{"(a)??b", "", "ab", [][]int{{0, 2, 0, 1}}},
	{"(?:(a)??b)+", "", "abab", [][]int{{0, 4, 2, 3}}},
	{"(?<=(a)?b)c", "", "abc", [][]int{{2, 3, 0, 1}}},
	{"(?<=(?:(a)?b)+)c", "", "abbc", [][]int{{3, 4, 0, 1}}},
	{"(?:(a)|(b))?c", "", "bc", [][]int{{0, 2, -1, -1, 0, 1}}},
	{"(?:((a)|(b))?c)+", "", "acbcc", [][]int{{0, 5, -1, -1, -1, -1, -1, -1}}},
	{"(?:(a)?\\1b)+", "", "aabbab", [][]int{{0, 4, -1, -1}}},
	{"(a)?(?:\\1x)?", "", "aax", [][]int{{0, 3, 0, 1}}},
	{"(((\\w+):\\/\\/)([^\\/:]*)(:(\\d+))?)?([^#?]*)(\\?([^#]*))?(#(.*))?", "", "uggc://jjj.snprobbx.pbz:80/ybtva.cuc?k=1#s",
		[][]int{{0, 42, 0, 26, 0, 7, 0, 4, 7, 23, 23, 26, 24, 26, 26, 36, 36, 40, 37, 40, 40, 42, 41, 42}}},
	{"(((\\w+):\\/\\/)([^\\/:]*)(:(\\d+))?)?([^#?]*)(\\?([^#]*))?(#(.*))?", "", "ybtva.cuc",
		[][]int{{0, 9, -1, -1, -1, -1, -1, -1, -1, -1, -1, -1, -1, -1, 0, 9, -1, -1, -1, -1, -1, -1, -1, -1}}},
	{"(?:(a)?(b)?c)*", "", "acbcc", [][]int{{0, 5, -1, -1, -1, -1}}},
	{"(?:(?:(a)?b)?c)*", "", "abccbc", [][]int{{0, 6, -1, -1}}},
	{"(?:a(?:(b)?c)?)*d", "", "abcacad", [][]int{{0, 7, -1, -1}}},
}

// matchesOf is every match of re in s, by match, as optionalCases has them:
// one, or for a global pattern each from where the last ended.
func matchesOf(t *testing.T, re *Regexp, s string, match func(start int) ([]int, error)) [][]int {
	t.Helper()
	var got [][]int
	for start := 0; start <= len(s); {
		caps, err := match(start)
		if err != nil {
			t.Fatal(err)
		}
		if caps == nil && len(got) > 0 {
			break
		}
		got = append(got, slices.Clone(caps))
		if caps == nil || re.Flags()&FlagGlobal == 0 {
			break
		}
		start = caps[1]
		if caps[1] == caps[0] {
			start++
		}
	}
	return got
}

// The atom of a ? is entered with its groups unset, as an iteration of any
// quantifier begins with them -- whatever an iteration of a loop around it
// left in them was unset by that loop's -- so its program unsets nothing
// there, and the matches are the same over every subject's form.
func TestOptionalAtomsMatch(t *testing.T) {
	for _, c := range optionalCases {
		re, err := Compile(c.src, c.flags)
		if err != nil {
			t.Fatalf("/%s/: %v", c.src, err)
		}
		units := utf16.Encode([]rune(c.s))
		forms := map[string]func(int) ([]int, error){
			"UTF-16": func(start int) ([]int, error) { return re.SearchCheckedInto(nil, units, start, nil) },
			"ASCII":  func(start int) ([]int, error) { return re.SearchASCIICheckedInto(nil, c.s, start, nil) },
		}
		for name, match := range forms {
			// Matched more than once, so that a pattern given code of its
			// own after some matches has its code's matches checked too.
			for i := 0; i < 3; i++ {
				if got := matchesOf(t, re, c.s, match); !slices.EqualFunc(got, c.want, slices.Equal[[]int]) {
					t.Errorf("/%s/%s on %q (%s): %v, want %v", c.src, c.flags, c.s, name, got, c.want)
				}
			}
		}
		u8, err := CompileUTF8(c.src, c.flags)
		if err != nil {
			t.Fatalf("/%s/ for UTF-8: %v", c.src, err)
		}
		got := matchesOf(t, u8, c.s, func(start int) ([]int, error) {
			return u8.MatchUTF8Into(nil, c.s, start, nil)
		})
		if !slices.EqualFunc(got, c.want, slices.Equal[[]int]) {
			t.Errorf("/%s/%s on %q (UTF-8): %v, want %v", c.src, c.flags, c.s, got, c.want)
		}
	}
}

// A ? unsets no groups as it enters its atom; a loop still unsets its
// atom's at every iteration.
func TestOptionalAtomUnsetsNothing(t *testing.T) {
	count := func(src string) int {
		re, err := Compile(src, "")
		if err != nil {
			t.Fatal(err)
		}
		n := 0
		for _, in := range re.prog.code {
			if in.op == opClearCaps {
				n++
			}
		}
		return n
	}
	for _, c := range []struct {
		src  string
		want int
	}{
		{"(a)?b", 0}, {"(a)??b", 0}, {"(?:(a)|(b))?c", 0}, {"(?=(a))?a", 0},
		{"((a)?x)*", 1}, {"(?:(a)?b)+", 1}, {"(?:(a)?b){1,3}", 1}, {"(?:x(y)?)*z", 1},
	} {
		if got := count(c.src); got != c.want {
			t.Errorf("/%s/: %d instructions unset groups, want %d", c.src, got, c.want)
		}
	}
}
