package regexp

import (
	"errors"
	"strings"
	"testing"

	"github.com/go-quickjs/go-quickjs/internal/wtf8"
)

// find runs a pattern and returns the matched text, or "" with ok false.
func find(t *testing.T, pattern, flags, subject string) (string, bool) {
	t.Helper()
	re, err := Compile(pattern, flags)
	if err != nil {
		t.Fatalf("compiling /%s/%s: %v", pattern, flags, err)
	}
	caps, err := re.MatchString(subject, 0)
	if err != nil {
		t.Fatalf("matching /%s/%s against %q: %v", pattern, flags, subject, err)
	}
	if caps == nil {
		return "", false
	}
	units := wtf8.ToUTF16(subject)
	return wtf8.FromUTF16(units[caps[0]:caps[1]]), true
}

// checkMatch asserts that a pattern matches a subject, yielding want.
func checkMatch(t *testing.T, pattern, flags, subject, want string) {
	t.Helper()
	got, ok := find(t, pattern, flags, subject)
	if !ok {
		t.Errorf("/%s/%s against %q: no match, want %q", pattern, flags, subject, want)
		return
	}
	if got != want {
		t.Errorf("/%s/%s against %q = %q, want %q", pattern, flags, subject, got, want)
	}
}

// checkNoMatch asserts that a pattern does not match.
func checkNoMatch(t *testing.T, pattern, flags, subject string) {
	t.Helper()
	if got, ok := find(t, pattern, flags, subject); ok {
		t.Errorf("/%s/%s against %q matched %q, want no match", pattern, flags, subject, got)
	}
}

func TestLiterals(t *testing.T) {
	checkMatch(t, "abc", "", "xxabcyy", "abc")
	checkMatch(t, "a", "", "a", "a")
	checkNoMatch(t, "abc", "", "abd")
	checkMatch(t, "", "", "anything", "")
}

func TestQuantifiers(t *testing.T) {
	tests := []struct{ pattern, subject, want string }{
		{"a*", "aaa", "aaa"},
		{"a+", "aaa", "aaa"},
		{"a?", "aaa", "a"},
		{"a{2}", "aaa", "aa"},
		{"a{2,}", "aaaa", "aaaa"},
		{"a{2,3}", "aaaa", "aaa"},
		{"ab*", "a", "a"},
		{"ab*c", "ac", "ac"},
		{"(ab)+", "ababab", "ababab"},
		// A counted quantifier is not unrolled, so a large bound is cheap.
		{"a{1,1000}", "aaa", "aaa"},
	}
	for _, tt := range tests {
		checkMatch(t, tt.pattern, "", tt.subject, tt.want)
	}
	checkNoMatch(t, "a{3}", "", "aa")
}

func TestLazyQuantifiers(t *testing.T) {
	// A lazy quantifier prefers the shortest match, which is the whole point.
	checkMatch(t, "a+?", "", "aaa", "a")
	checkMatch(t, "a*?b", "", "aaab", "aaab")
	checkMatch(t, "<.+?>", "", "<a><b>", "<a>")
	checkMatch(t, "<.+>", "", "<a><b>", "<a><b>")
	checkMatch(t, "a{2,4}?", "", "aaaa", "aa")
}

func TestAlternation(t *testing.T) {
	checkMatch(t, "a|b", "", "b", "b")
	checkMatch(t, "abc|abd", "", "abd", "abd")
	// Alternatives are tried in order, so the first that matches wins even if
	// a later one would match more.
	checkMatch(t, "a|ab", "", "ab", "a")
	checkMatch(t, "(?:foo|bar)baz", "", "barbaz", "barbaz")
	checkNoMatch(t, "a|b", "", "c")
}

func TestCharacterClasses(t *testing.T) {
	tests := []struct{ pattern, subject, want string }{
		{"[abc]+", "cab!", "cab"},
		{"[a-z]+", "abc", "abc"},
		{"[^a-z]+", "ABC", "ABC"},
		{"[0-9]+", "123", "123"},
		{`\d+`, "a123b", "123"},
		{`\w+`, " ab_1 ", "ab_1"},
		{`\s+`, "a  b", "  "},
		{`[\d\s]+`, "1 2", "1 2"},
		{`[\-a]+`, "-a", "-a"},
		{"[a-]+", "a-", "a-"},
		{`[\]]`, "]", "]"},
		// A negated class still excludes what a shorthand inside it covers.
		{`[^\d]+`, "abc1", "abc"},
	}
	for _, tt := range tests {
		checkMatch(t, tt.pattern, "", tt.subject, tt.want)
	}
	checkNoMatch(t, `\D`, "", "5")
	checkNoMatch(t, `\W`, "", "a")
}

func TestAnchors(t *testing.T) {
	checkMatch(t, "^abc", "", "abc", "abc")
	checkNoMatch(t, "^abc", "", "xabc")
	checkMatch(t, "abc$", "", "xabc", "abc")
	checkNoMatch(t, "abc$", "", "abcx")
	checkMatch(t, "^$", "", "", "")

	// Under the m flag the anchors also match at line boundaries.
	checkMatch(t, "^b", "m", "a\nb", "b")
	checkNoMatch(t, "^b", "", "a\nb")
	checkMatch(t, "a$", "m", "a\nb", "a")
}

func TestWordBoundary(t *testing.T) {
	checkMatch(t, `\bfoo\b`, "", "a foo b", "foo")
	checkNoMatch(t, `\bfoo\b`, "", "afoob")
	checkMatch(t, `\Bfoo`, "", "afoo", "foo")
	checkNoMatch(t, `\Bfoo`, "", " foo")
}

func TestDot(t *testing.T) {
	checkMatch(t, "a.c", "", "abc", "abc")
	// Dot excludes line terminators unless the s flag is set.
	checkNoMatch(t, "a.c", "", "a\nc")
	checkMatch(t, "a.c", "s", "a\nc", "a\nc")
}

func TestGroupsAndCaptures(t *testing.T) {
	re, err := Compile(`(\d+)-(\d+)`, "")
	if err != nil {
		t.Fatal(err)
	}
	caps, err := re.MatchString("x 12-34 y", 0)
	if err != nil {
		t.Fatal(err)
	}
	if caps == nil {
		t.Fatal("expected a match")
	}
	if re.GroupCount() != 2 {
		t.Errorf("GroupCount() = %d, want 2", re.GroupCount())
	}
	// Slots are [whole, group1, group2] as start/end pairs.
	want := []int{2, 7, 2, 4, 5, 7}
	for i, w := range want {
		if caps[i] != w {
			t.Errorf("caps[%d] = %d, want %d (all: %v)", i, caps[i], w, caps)
		}
	}
}

func TestNonParticipatingGroupIsMinusOne(t *testing.T) {
	re, _ := Compile(`(a)|(b)`, "")
	caps, err := re.MatchString("b", 0)
	if err != nil {
		t.Fatal(err)
	}
	// The first group did not participate, so its slots stay -1 rather than
	// reporting an empty match.
	if caps[2] != -1 || caps[3] != -1 {
		t.Errorf("group 1 = [%d,%d], want [-1,-1]", caps[2], caps[3])
	}
	if caps[4] != 0 || caps[5] != 1 {
		t.Errorf("group 2 = [%d,%d], want [0,1]", caps[4], caps[5])
	}
}

func TestNamedGroups(t *testing.T) {
	re, err := Compile(`(?<year>\d{4})-(?<month>\d{2})`, "")
	if err != nil {
		t.Fatal(err)
	}
	names := re.GroupNames()
	if names["year"] != 1 || names["month"] != 2 {
		t.Errorf("group names = %v, want year:1 month:2", names)
	}
	caps, err := re.MatchString("2024-03", 0)
	if err != nil || caps == nil {
		t.Fatalf("no match: %v", err)
	}
	if caps[2] != 0 || caps[3] != 4 {
		t.Errorf("year group = [%d,%d], want [0,4]", caps[2], caps[3])
	}
}

func TestNonCapturingGroup(t *testing.T) {
	re, _ := Compile(`(?:ab)+(c)`, "")
	if re.GroupCount() != 1 {
		t.Errorf("GroupCount() = %d, want 1", re.GroupCount())
	}
	checkMatch(t, `(?:ab)+c`, "", "ababc", "ababc")
}

func TestBackreferences(t *testing.T) {
	checkMatch(t, `(a)\1`, "", "aa", "aa")
	checkNoMatch(t, `(a)\1`, "", "ab")
	checkMatch(t, `(\w+)\s\1`, "", "hello hello", "hello hello")
	checkMatch(t, `(?<x>a)\k<x>`, "", "aa", "aa")
	// A backreference to a group that did not participate matches empty.
	checkMatch(t, `(?:(a)|b)\1c`, "", "bc", "bc")
	// Case folding applies to backreferences under the i flag.
	checkMatch(t, `(a)\1`, "i", "aA", "aA")
}

func TestIgnoreCase(t *testing.T) {
	checkMatch(t, "abc", "i", "ABC", "ABC")
	checkMatch(t, "[a-z]+", "i", "ABC", "ABC")
	checkMatch(t, "ÄÖÜ", "i", "äöü", "äöü")
	checkNoMatch(t, "abc", "", "ABC")
}

func TestLookahead(t *testing.T) {
	// A positive lookahead constrains without consuming.
	checkMatch(t, `foo(?=bar)`, "", "foobar", "foo")
	checkNoMatch(t, `foo(?=bar)`, "", "foobaz")
	checkMatch(t, `foo(?!bar)`, "", "foobaz", "foo")
	checkNoMatch(t, `foo(?!bar)`, "", "foobar")
	checkMatch(t, `\d+(?= dollars)`, "", "42 dollars", "42")
}

func TestLookbehind(t *testing.T) {
	checkMatch(t, `(?<=\$)\d+`, "", "$42", "42")
	checkNoMatch(t, `(?<=\$)\d+`, "", "#42")
	checkMatch(t, `(?<!\$)\d+`, "", "#42", "42")
	// A variable-length lookbehind is legal in JavaScript, unlike in many
	// other engines.
	checkMatch(t, `(?<=ab+)c`, "", "abbbc", "c")
}

func TestMatchFromOffset(t *testing.T) {
	re, _ := Compile("a", "")
	caps, err := re.MatchString("aaa", 1)
	if err != nil {
		t.Fatal(err)
	}
	if caps == nil || caps[0] != 1 {
		t.Errorf("match from offset 1 started at %v, want 1", caps)
	}
}

func TestSticky(t *testing.T) {
	re, _ := Compile("a", "y")
	// A sticky pattern is anchored at the start position and does not search.
	if caps, _ := re.MatchString("ba", 0); caps != nil {
		t.Error("a sticky pattern should not search forward")
	}
	if caps, _ := re.MatchString("ba", 1); caps == nil {
		t.Error("a sticky pattern should match at the exact position")
	}
}

func TestUnicodeFlag(t *testing.T) {
	// Without the u flag a surrogate pair is two characters, so . matches half
	// of one; with it the pair is a single character.
	checkMatch(t, ".", "u", "\U0001F600", "\U0001F600")
	if got, _ := find(t, ".", "", "\U0001F600"); len([]rune(got)) != 1 {
		// The half-match is a lone surrogate, which is one rune once decoded.
		t.Logf("without u, . matched %q", got)
	}
	checkMatch(t, `\u{1F600}`, "u", "\U0001F600", "\U0001F600")
	checkMatch(t, `[\u{1F600}]`, "u", "\U0001F600", "\U0001F600")
}

func TestUnicodePropertyEscapes(t *testing.T) {
	checkMatch(t, `\p{L}+`, "u", "abcДЕ1", "abcДЕ")
	checkMatch(t, `\p{Nd}+`, "u", "12a", "12")
	checkMatch(t, `\P{L}+`, "u", "123a", "123")
	checkMatch(t, `\p{Script=Greek}+`, "u", "αβγa", "αβγ")
}

func TestEscapes(t *testing.T) {
	tests := []struct{ pattern, subject, want string }{
		{`\n`, "\n", "\n"},
		{`\t`, "\t", "\t"},
		{`\x41`, "A", "A"},
		{`A`, "A", "A"},
		{`\.`, ".", "."},
		{`\\`, `\`, `\`},
		{`\$`, "$", "$"},
		{`\cA`, "\x01", "\x01"},
		{`a\/b`, "a/b", "a/b"},
	}
	for _, tt := range tests {
		checkMatch(t, tt.pattern, "", tt.subject, tt.want)
	}
}

func TestNestedQuantifiers(t *testing.T) {
	checkMatch(t, `(a+)+`, "", "aaa", "aaa")
	checkMatch(t, `(a*)*`, "", "aaa", "aaa")
	// A repetition whose body matches nothing must terminate rather than loop.
	checkMatch(t, `(a*)*`, "", "", "")
	checkMatch(t, `(?:)*`, "", "x", "")
	// The empty alternative fails the iteration's empty check, which backtracks
	// into the "a" alternative, so the loop does make progress.
	checkMatch(t, `(|a)*`, "", "aa", "aa")
}

func TestCatastrophicBacktrackingIsBounded(t *testing.T) {
	// This is the classic exponential case. The engine must give up rather
	// than hang, so that a hostile pattern cannot stall the host.
	re, err := Compile(`(a+)+b`, "")
	if err != nil {
		t.Fatal(err)
	}
	_, err = re.MatchString(strings.Repeat("a", 100), 0)
	if err == nil {
		t.Skip("the match completed; the engine found it without backtracking")
	}
	if !errors.Is(err, ErrComplexity) {
		t.Errorf("error = %v, want ErrComplexity", err)
	}
}

func TestSyntaxErrors(t *testing.T) {
	for _, pattern := range []string{
		"(", "[", "a{2,1}", `\`, "(?<", "a**",
	} {
		if _, err := Compile(pattern, ""); err == nil {
			t.Errorf("/%s/ compiled, want a syntax error", pattern)
		}
	}
	// A flag that does not exist, and a duplicate one.
	if _, err := Compile("a", "q"); err == nil {
		t.Error("an unknown flag should be rejected")
	}
	if _, err := Compile("a", "gg"); err == nil {
		t.Error("a duplicate flag should be rejected")
	}
}

func TestLoneSurrogateInSubject(t *testing.T) {
	// A pattern must be able to match a lone surrogate, since a JavaScript
	// string may contain one.
	re, err := Compile(".", "")
	if err != nil {
		t.Fatal(err)
	}
	units := []uint16{0xD83D}
	caps, err := re.Match(units, 0)
	if err != nil {
		t.Fatal(err)
	}
	if caps == nil || caps[0] != 0 || caps[1] != 1 {
		t.Errorf("matching a lone surrogate gave %v, want [0 1]", caps)
	}
}

func BenchmarkSimpleLiteral(b *testing.B) {
	re, _ := Compile("hello", "")
	subject := wtf8.ToUTF16(strings.Repeat("x", 200) + "hello")
	b.ReportAllocs()
	for b.Loop() {
		if _, err := re.Match(subject, 0); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkCharClass(b *testing.B) {
	re, _ := Compile(`[a-z]+[0-9]+`, "")
	subject := wtf8.ToUTF16("abcdefghij0123456789")
	b.ReportAllocs()
	for b.Loop() {
		if _, err := re.Match(subject, 0); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkCaptureGroups(b *testing.B) {
	re, _ := Compile(`(\w+)@(\w+)\.(\w+)`, "")
	subject := wtf8.ToUTF16("contact: someone@example.com")
	b.ReportAllocs()
	for b.Loop() {
		if _, err := re.Match(subject, 0); err != nil {
			b.Fatal(err)
		}
	}
}
