package jsregexp

import (
	"fmt"
	"math/rand"
	"testing"
)

// TestLiteralPrefilterAgrees pins that looking for a pattern's required
// literal in the string first -- ruling out a subject without it, and starting
// an ASCII subject's search where the literal can first be reached -- changes
// no method's answer. Each pattern's methods run with the prefilter and with
// it turned off, over subjects in and out of ASCII, with surrogate pairs, and
// with the literal present, absent, at either end and overlapping itself.
func TestLiteralPrefilterAgrees(t *testing.T) {
	patterns := []struct{ src, flags string }{
		{`needle`, ""}, {`(^|[^\\])"\\/Date\((\d+)\)`, "g"}, {`a?bcd`, ""}, {`(ab|cd)ef`, ""},
		{`\s?;\s?end`, ""}, {`##oe##`, "i"}, {`x.yz`, "s"}, {`(?<w>\w+)@example`, ""},
		{`abab`, ""}, {`é+ab`, ""}, {`ab`, "y"}, {`cd(e)?`, "y"}, {`^\d+-log`, "m"},
	}
	alphabet := []string{"a", "b", "c", "d", "e", "f", "x", "y", "z", "ab", "abab", "cd", "ef", "bcd", "end", ";", " ",
		"needle", "need", "#", "##", "oe", "OE", `"`, `\`, "/Date(", "12", ")", "@example", "w", "é", "\U0001F600", "\n", "-log", "7"}
	r := rand.New(rand.NewSource(7))
	var subjects []string
	for i := 0; i < 300; i++ {
		s := ""
		for j := r.Intn(12); j >= 0; j-- {
			s += alphabet[r.Intn(len(alphabet))]
		}
		subjects = append(subjects, s)
	}
	subjects = append(subjects, "", "needle", "xneedle", "needlex", "ababab", "cdcd", "é")

	prefiltered := 0
	for _, p := range patterns {
		with := MustCompile(p.src, p.flags)
		without := MustCompile(p.src, p.flags)
		without.lit = ""
		if with.lit != "" {
			prefiltered++
		}
		for _, s := range subjects {
			if got, want := results(with, s), results(without, s); got != want {
				t.Fatalf("/%s/%s on %q:\n with the prefilter    %s\n without the prefilter %s", p.src, p.flags, s, got, want)
			}
		}
	}
	// The test means nothing if nothing has a literal to look for.
	if prefiltered < len(patterns)*2/3 {
		t.Errorf("only %d of %d patterns have a required literal", prefiltered, len(patterns))
	}
}

// results is every method's answer for a subject, as one string to compare.
func results(re *Regexp, s string) string {
	out := ""
	add := func(v any, err error) { out += fmt.Sprintf("%v %v|", v, err) }
	add(re.MatchString(s))
	add(re.FindString(s))
	add(re.FindStringIndex(s))
	add(re.FindStringSubmatch(s))
	add(re.FindStringSubmatchIndex(s))
	add(re.FindStringSubmatchMap(s))
	add(re.FindAllString(s, -1))
	add(re.FindAllString(s, 2))
	add(re.FindAllStringIndex(s, -1))
	add(re.FindAllStringSubmatch(s, -1))
	add(re.FindAllStringSubmatchIndex(s, -1))
	add(re.ReplaceAllString(s, "<$&>"))
	add(re.ReplaceAllLiteralString(s, "_"))
	add(re.Split(s, -1))
	add(re.Split(s, 2))
	return out
}
