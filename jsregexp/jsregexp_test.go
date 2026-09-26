package jsregexp_test

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/go-quickjs/go-quickjs/jsregexp"
)

// The point of the package is the patterns Go's own regexp refuses, so these
// start there.
func TestWhatRE2Cannot(t *testing.T) {
	cases := []struct {
		pattern, flags, subject, want string
	}{
		// A backreference.
		{`(\w+)\s+\1`, "", "the the cat", "the the"},
		{`(['"]).*?\1`, "", `say "hello" now`, `"hello"`},
		// Lookahead and lookbehind, positive and negative.
		{`\d+(?= dollars)`, "", "pay 42 dollars now", "42"},
		{`(?<=\$)\d+`, "", "costs $99 today", "99"},
		{`\b\d+\b(?! pence)`, "", "40 pence 50 pounds", "50"},
		{`(?<!un)happy`, "", "unhappy happy", "happy"},
		// Named groups.
		{`(?<year>\d{4})-(?<month>\d\d)`, "", "on 2024-09-01", "2024-09"},
		// Unicode property escapes, which need the u flag.
		{`\p{Script=Greek}+`, "u", "abc αβγ def", "αβγ"},
		{`\p{Lu}+`, "u", "fooBAR", "BAR"},
		// The v flag's set notation.
		{`[\p{ASCII}--[a-z]]+`, "v", "abcDEF", "DEF"},
	}
	for _, tc := range cases {
		re, err := jsregexp.Compile(tc.pattern, tc.flags)
		if err != nil {
			t.Errorf("Compile(%q, %q): %v", tc.pattern, tc.flags, err)
			continue
		}
		got, err := re.FindString(tc.subject)
		if err != nil {
			t.Errorf("%q against %q: %v", tc.pattern, tc.subject, err)
			continue
		}
		if got != tc.want {
			t.Errorf("%q against %q = %q, want %q", tc.pattern, tc.subject, got, tc.want)
		}
	}
}

func TestFlags(t *testing.T) {
	cases := []struct {
		pattern, flags, subject, want string
	}{
		{`abc`, "i", "xxABCxx", "ABC"},
		{`^b`, "m", "a\nbc", "b"},
		{`a.c`, "s", "a\nc", "a\nc"},
		{`a.c`, "", "a\nc", ""},
		// Sticky matches only where the search starts.
		{`b`, "y", "ab", ""},
		{`a`, "y", "ab", "a"},
		// With u, a dot is a code point rather than a code unit; the other
		// way round is in TestAstralText, where a lone half has to be built
		// rather than written.
		{`.`, "u", "😀", "😀"},
	}
	for _, tc := range cases {
		re := jsregexp.MustCompile(tc.pattern, tc.flags)
		got, err := re.FindString(tc.subject)
		if err != nil {
			t.Errorf("%q: %v", tc.pattern, err)
			continue
		}
		if got != tc.want {
			t.Errorf("/%s/%s against %q = %q, want %q",
				tc.pattern, tc.flags, tc.subject, got, tc.want)
		}
	}
}

func TestSubmatches(t *testing.T) {
	re := jsregexp.MustCompile(`(?<user>\w+)@(\w+)\.com`, "i")

	m, err := re.FindStringSubmatch("Write to Someone@Example.com today")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"Someone@Example.com", "Someone", "Example"}
	if fmt.Sprint(m) != fmt.Sprint(want) {
		t.Errorf("submatches = %q, want %q", m, want)
	}

	if got, want := re.NumSubexp(), 2; got != want {
		t.Errorf("NumSubexp = %d, want %d", got, want)
	}
	if got, want := fmt.Sprint(re.SubexpNames()), `[ user ]`; got != want {
		t.Errorf("SubexpNames = %q, want %q", got, want)
	}
	if got, want := re.SubexpIndex("user"), 1; got != want {
		t.Errorf("SubexpIndex = %d, want %d", got, want)
	}
	if got, want := re.SubexpIndex("nope"), -1; got != want {
		t.Errorf("SubexpIndex of an absent name = %d, want %d", got, want)
	}

	byName, err := re.FindStringSubmatchMap("a b@c.com")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := byName["user"], "b"; got != want {
		t.Errorf("by name = %q, want %q", got, want)
	}
}

// A group that did not participate is empty text and an index of -1, which is
// how the two are told apart.
func TestGroupThatDidNotParticipate(t *testing.T) {
	re := jsregexp.MustCompile(`a(x)?(y?)`, "")
	m, err := re.FindStringSubmatch("a")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := fmt.Sprintf("%q", m), `["a" "" ""]`; got != want {
		t.Errorf("submatch = %s, want %s", got, want)
	}
	idx, err := re.FindStringSubmatchIndex("a")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := fmt.Sprint(idx), "[0 1 -1 -1 1 1]"; got != want {
		t.Errorf("index = %s, want %s", got, want)
	}
}

// Groups in different alternatives may share a name, and the name then stands
// for whichever of them took part.
func TestDuplicateGroupNames(t *testing.T) {
	re := jsregexp.MustCompile(`(?<n>\d+)-x|y-(?<n>\d+)`, "")
	if got, want := fmt.Sprintf("%q", re.SubexpNames()), `["" "n" "n"]`; got != want {
		t.Errorf("SubexpNames = %s, want %s", got, want)
	}
	if got, want := re.SubexpIndex("n"), 1; got != want {
		t.Errorf("SubexpIndex = %d, want %d (the leftmost)", got, want)
	}
	for subject, want := range map[string]string{"12-x": "12", "y-34": "34"} {
		m, err := re.FindStringSubmatchMap(subject)
		if err != nil {
			t.Fatal(err)
		}
		if m["n"] != want {
			t.Errorf("%s: n = %q, want %q", subject, m["n"], want)
		}
	}
	if _, err := jsregexp.Compile(`(?<n>a)(?<n>b)`, ""); err == nil {
		t.Error("two groups that can both take part may not share a name")
	}
}

// Positions are byte offsets into the string that was passed in, whatever the
// text is made of.
func TestIndicesAreByteOffsets(t *testing.T) {
	// \w is ASCII in JavaScript, as it is in Go, so a word with an umlaut in
	// it is matched with \S.
	re := jsregexp.MustCompile(`(w\S+)`, "")
	const s = "héllo wörld 😀 what"

	idx, err := re.FindStringIndex(s)
	if err != nil {
		t.Fatal(err)
	}
	if got := s[idx[0]:idx[1]]; got != "wörld" {
		t.Errorf("s[%d:%d] = %q, want %q", idx[0], idx[1], got, "wörld")
	}

	all, err := re.FindAllStringIndex(s, -1)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, pair := range all {
		got = append(got, s[pair[0]:pair[1]])
	}
	if want := "wörld,what"; strings.Join(got, ",") != want {
		t.Errorf("matches = %q, want %q", got, want)
	}
}

func TestFindAll(t *testing.T) {
	re := jsregexp.MustCompile(`\d+`, "")
	const s = "a1 b22 c333"

	all, err := re.FindAllString(s, -1)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := strings.Join(all, ","), "1,22,333"; got != want {
		t.Errorf("all = %q, want %q", got, want)
	}

	two, err := re.FindAllString(s, 2)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := strings.Join(two, ","), "1,22"; got != want {
		t.Errorf("first two = %q, want %q", got, want)
	}

	none, err := re.FindAllString(s, 0)
	if err != nil {
		t.Fatal(err)
	}
	if none != nil {
		t.Errorf("zero asked for gave %q", none)
	}

	subs, err := re.FindAllStringSubmatch("x1y2", -1)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := fmt.Sprint(subs), "[[1] [2]]"; got != want {
		t.Errorf("submatches = %s, want %s", got, want)
	}
}

// An empty match advances, rather than being found for ever in one place.
func TestEmptyMatchesAdvance(t *testing.T) {
	re := jsregexp.MustCompile(`x*`, "")
	all, err := re.FindAllString("axxb", -1)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := fmt.Sprintf("%q", all), `["" "xx" "" ""]`; got != want {
		t.Errorf("all = %s, want %s", got, want)
	}
}

func TestReplace(t *testing.T) {
	cases := []struct{ pattern, flags, subject, repl, want string }{
		{`\d+`, "", "a1b22", "#", "a#b#"},
		{`(\w)(\d)`, "", "a1 b2", "$2$1", "1a 2b"},
		{`(?<letter>\w)(\d)`, "", "a1", "$<letter>!", "a!"},
		{`b`, "", "abc", "[$&]", "a[b]c"},
		{"b", "", "abc", "$`", "aac"},
		{`b`, "", "abc", "$'", "acc"},
		{`b`, "", "abc", "$$", "a$c"},
		// A group that did not participate contributes nothing.
		{`a(x)?`, "", "a", "[$1]", "[]"},
		// A number past the last group is literal text.
		{`(a)`, "", "a", "$2", "$2"},
		// Two digits name a group when there is one.
		{`(1)(2)(3)(4)(5)(6)(7)(8)(9)(10)(11)(12)`, "", "123456789101112", "$12", "12"},
	}
	for _, tc := range cases {
		re := jsregexp.MustCompile(tc.pattern, tc.flags)
		got, err := re.ReplaceAllString(tc.subject, tc.repl)
		if err != nil {
			t.Errorf("%q: %v", tc.pattern, err)
			continue
		}
		if got != tc.want {
			t.Errorf("replace %q in %q with %q = %q, want %q",
				tc.pattern, tc.subject, tc.repl, got, tc.want)
		}
	}
}

func TestReplaceFuncs(t *testing.T) {
	re := jsregexp.MustCompile(`\w+`, "")
	got, err := re.ReplaceAllStringFunc("go js", strings.ToUpper)
	if err != nil {
		t.Fatal(err)
	}
	if want := "GO JS"; got != want {
		t.Errorf("= %q, want %q", got, want)
	}

	pairs := jsregexp.MustCompile(`(\w+)=(\w+)`, "")
	got, err = pairs.ReplaceAllStringSubmatchFunc("a=1 b=2", func(g []string) string {
		return g[2] + ":" + g[1]
	})
	if err != nil {
		t.Fatal(err)
	}
	if want := "1:a 2:b"; got != want {
		t.Errorf("= %q, want %q", got, want)
	}

	lit, err := re.ReplaceAllLiteralString("a b", "$&")
	if err != nil {
		t.Fatal(err)
	}
	if want := "$& $&"; lit != want {
		t.Errorf("literal = %q, want %q", lit, want)
	}
}

func TestSplit(t *testing.T) {
	cases := []struct {
		pattern, subject string
		n                int
		want             string
	}{
		{`,`, "a,b,c", -1, "a|b|c"},
		{`,`, "a,b,c", 2, "a|b,c"},
		{`\s*,\s*`, "a , b,c", -1, "a|b|c"},
		{`x`, "abc", -1, "abc"},
		{`,`, "", -1, ""},
		{`,`, "a,b", 0, ""},
	}
	for _, tc := range cases {
		re := jsregexp.MustCompile(tc.pattern, "")
		parts, err := re.Split(tc.subject, tc.n)
		if err != nil {
			t.Fatal(err)
		}
		if got := strings.Join(parts, "|"); got != tc.want {
			t.Errorf("split %q by %q (n=%d) = %q, want %q",
				tc.subject, tc.pattern, tc.n, got, tc.want)
		}
	}
}

func TestMatchString(t *testing.T) {
	ok, err := jsregexp.MatchString(`^\d+$`, "", "12345")
	if err != nil || !ok {
		t.Errorf("MatchString = %v, %v", ok, err)
	}
	ok, err = jsregexp.MatchString(`^\d+$`, "", "12a45")
	if err != nil || ok {
		t.Errorf("MatchString = %v, %v", ok, err)
	}
	if _, err := jsregexp.MatchString(`(`, "", "x"); err == nil {
		t.Error("an unbalanced pattern should not compile")
	}
}

func TestCompileErrors(t *testing.T) {
	for _, tc := range []struct{ pattern, flags string }{
		{`(`, ""},
		{`[`, ""},
		{`a{2,1}`, ""},
		{`\`, ""},
		{`a`, "gg"},
		{`a`, "q"},
	} {
		if _, err := jsregexp.Compile(tc.pattern, tc.flags); err == nil {
			t.Errorf("Compile(%q, %q) should have failed", tc.pattern, tc.flags)
		}
	}
	defer func() {
		if recover() == nil {
			t.Error("MustCompile should have panicked")
		}
	}()
	jsregexp.MustCompile(`(`, "")
}

// A pattern that backtracks catastrophically gives up rather than running for
// ever, and says so.
func TestComplexityIsReported(t *testing.T) {
	re := jsregexp.MustCompile(`(a+)+b`, "")
	_, err := re.MatchString(strings.Repeat("a", 100))
	if err == nil {
		t.Skip("the match completed; the engine found it without backtracking")
	}
	if !errors.Is(err, jsregexp.ErrComplexity) {
		t.Errorf("error = %v, want ErrComplexity", err)
	}
	// The budget belongs to the match, so the next one starts with its own.
	ok, err := re.MatchString("aab")
	if err != nil || !ok {
		t.Errorf("the match after an exhausted one = %v, %v", ok, err)
	}
}

func TestQuoteMeta(t *testing.T) {
	quoted := jsregexp.QuoteMeta("a.b*c")
	re := jsregexp.MustCompile(quoted, "")
	ok, err := re.MatchString("a.b*c")
	if err != nil || !ok {
		t.Errorf("quoted pattern did not match its text: %v %v", ok, err)
	}
	ok, _ = re.MatchString("axbxc")
	if ok {
		t.Error("quoted pattern matched something else")
	}
}

// Text outside the basic plane is matched by code unit, and the pieces handed
// back are the text that was passed in.
func TestAstralText(t *testing.T) {
	re := jsregexp.MustCompile(`😀+`, "u")
	const s = "a😀😀b"
	got, err := re.FindString(s)
	if err != nil {
		t.Fatal(err)
	}
	if want := "😀😀"; got != want {
		t.Errorf("= %q, want %q", got, want)
	}
	idx, err := re.FindStringIndex(s)
	if err != nil {
		t.Fatal(err)
	}
	if got := s[idx[0]:idx[1]]; got != "😀😀" {
		t.Errorf("s[%d:%d] = %q", idx[0], idx[1], got)
	}

	// Without the u flag a group may match one half of a pair, which is an
	// ordinary one-code-unit string in JavaScript and has no UTF-8 form at
	// all: it comes back as the three bytes WTF-8 spells it with. Joining the
	// two halves back into the character is a thing to do with code units, not
	// with Go strings.
	half := jsregexp.MustCompile(`(.)(.)`, "")
	m, err := half.FindStringSubmatch("😀")
	if err != nil {
		t.Fatal(err)
	}
	const (
		highHalf = "\xed\xa0\xbd" // U+D83D
		lowHalf  = "\xed\xb8\x80" // U+DE00
	)
	if len(m) != 3 || m[1] != highHalf || m[2] != lowHalf {
		t.Errorf("halves = %q, want the two halves of the pair", m)
	}
	if m[0] != "😀" {
		t.Errorf("the whole match = %q, want the character itself", m[0])
	}
}

func ExampleRegexp_FindStringSubmatch() {
	re := jsregexp.MustCompile(`(?<year>\d{4})-(?<month>\d{2})`, "")
	m, _ := re.FindStringSubmatch("released 2024-09-01")
	fmt.Println(m[0], m[re.SubexpIndex("year")], m[re.SubexpIndex("month")])
	// Output: 2024-09 2024 09
}

func ExampleRegexp_ReplaceAllString() {
	re := jsregexp.MustCompile(`(\w+)@(\w+)`, "")
	out, _ := re.ReplaceAllString("mail bob@example now", "$2/$1")
	fmt.Println(out)
	// Output: mail example/bob now
}
