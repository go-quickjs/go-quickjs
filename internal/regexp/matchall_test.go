package regexp

import (
	"errors"
	"fmt"
	"math/rand"
	"slices"
	"strings"
	"testing"
)

// matchAllOneByOne is MatchAllASCIIInto's matches made as a global replace had made
// it: a match of MatchASCIICheckedInto's at each start, the tick before each
// after the first.
func matchAllOneByOne(re *Regexp, spans []int, s string, check, tick func() error) ([]int, int, error) {
	first := len(spans)
	var buf []int
	for pos := 0; pos <= len(s); {
		if len(spans) > first {
			if err := tick(); err != nil {
				return spans, pos, err
			}
		}
		caps, err := re.MatchASCIICheckedInto(buf, s, pos, check)
		if err != nil {
			return spans, pos, err
		}
		if caps == nil {
			break
		}
		buf = caps
		spans = append(spans, caps...)
		if pos = caps[1]; caps[0] == caps[1] {
			pos++
		}
	}
	return spans, -1, nil
}

// MatchAllASCIIInto, which borrows the matcher once for all its matches and
// searches for a literal or a class where it is, finds what a match from
// each start in turn finds -- every span, where it stops and its error --
// for patterns of every kind, sticky and anchored ones among them, with the
// tick failing at each of its first calls, and where a match spends the
// step budget.
func TestMatchAllASCIIAgrees(t *testing.T) {
	patterns := []struct{ src, flags string }{
		{"a", "g"}, {"ab", "g"}, {"aa", "g"}, {"needle", "g"}, {"[ab]", "g"}, {"\\s", "g"}, {"a|b", "g"},
		{"(a)(b)?", "g"}, {"(a)|(b)", "g"}, {"a*", "g"}, {"(?:)", "g"}, {"^", "g"}, {"$", "g"}, {"^a", "gm"},
		{"\\s+|^\\s*|\\s*$", "g"}, {"^\\s*|\\s*$", "g"}, {"(\\d*)(\\D*)", "g"}, {"\\bk\\w*", "g"},
		{"a", "gy"}, {"ab", "gy"}, {"[ab]", "gy"}, {"(a)b", "gy"}, {"k", "gi"}, {"[a-z]+", "gi"},
		{"\\w", "gu"}, {"a", "gd"}, {"(?<n>a)", "g"}, {"x*", "gy"}, {"(?<=a)b", "g"},
		// A trim's: an alternative anchored at the start and a run to the
		// end (trimTail), and ones like it it must not take.
		{`^\s+|\s+$`, "g"}, {`\s+$|^\s+`, "g"}, {`^[\s\xa0]+|[\s\xa0]+$`, "g"}, {`^\s*|\s*$`, "gi"},
		{`^(a)(b)?|[ab]*$`, "g"}, {`^k|[a\n ]+$`, "g"}, {`^\s+|\s+$`, "gm"}, {`^\s+|\s+$`, "gy"},
		{`^\s+|\s+(?=$)`, "g"}, {`^\s+|\s+\b`, "g"}, {`^a|b$`, "g"}, {`a|\s+$`, "g"},
	}
	alphabet := []string{"a", "b", "c", "k", "K", " ", "\n", "1", "7", "ab", "aab", "needle", "_", "  ", "\t"}
	r := rand.New(rand.NewSource(5))
	subjects := []string{"", "a", "ab", strings.Repeat("a", 30) + "c"}
	for i := 0; i < 150; i++ {
		s := ""
		for j := r.Intn(16); j >= 0; j-- {
			s += alphabet[r.Intn(len(alphabet))]
		}
		subjects = append(subjects, s)
	}
	errTick := errors.New("tick")
	for _, p := range patterns {
		re, err := Compile(p.src, p.flags)
		if err != nil {
			t.Fatal(err)
		}
		for _, s := range subjects {
			for stop := 0; stop <= 6; stop++ {
				var got, want string
				for i, all := range []func(*Regexp, []int, string, func() error, func() error) ([]int, int, error){
					MatchAllASCIIInto[struct{}], matchAllOneByOne,
				} {
					ticks := 0
					tick := func() error {
						if ticks++; stop > 0 && ticks >= stop {
							return errTick
						}
						return nil
					}
					spans, next, err := all(re, []int{7, 7}, s, nil, tick)
					out := fmt.Sprint(spans, next, err, ticks)
					if i == 0 {
						got = out
					} else {
						want = out
					}
				}
				if got != want {
					t.Errorf("/%s/%s on %q, tick failing at %d: got %s, want %s", p.src, p.flags, s, stop, got, want)
				}
			}
		}
	}
	// The budget is each match's own, as a match's in turn is.
	re, err := Compile("(a+)+b", "g")
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{"ab" + strings.Repeat("a", 40) + "c", strings.Repeat("ab", 100)} {
		spans, next, err := MatchAllASCIIInto[struct{}](re, nil, s, nil, func() error { return nil })
		wspans, wnext, werr := matchAllOneByOne(re, nil, s, nil, func() error { return nil })
		if !slices.Equal(spans, wspans) || next != wnext || err != werr {
			t.Errorf("%.10q...: got %v %d %v, want %v %d %v", s, spans, next, err, wspans, wnext, werr)
		}
	}
}
