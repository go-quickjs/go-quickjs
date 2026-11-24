package regexp

import (
	"testing"
	"unicode/utf16"

	"github.com/go-quickjs/go-quickjs/internal/ucd"
)

// TestClassSetFoldsOperands holds a v-mode class under the i flag to its
// operands folded before the set operations combine them
// (MaybeSimpleCaseFolding), the complement taken among the characters that
// fold to themselves (KI-90): [^k] leaves out "K" as it leaves out "k", and
// \P{Lu} leaves out "k", which folds as "K" does. Under u, which folds
// after the complement, \P{Lu} matches "K". The answers are Node's, but for
// those marked, where V8 leaves the Kelvin sign and the long s out of what
// \p{ASCII} folds to: there they are the specification's and QuickJS's.
func TestClassSetFoldsOperands(t *testing.T) {
	tests := []struct {
		pattern, flags, subject string
		want                    bool
	}{
		{`[^k]`, "iv", "K", false},
		{`\P{Lu}`, "iv", "K", false},
		{`[\p{Lu}--K]`, "iv", "k", false},
		{`[^k]`, "iu", "K", false},
		{`\P{Lu}`, "iu", "K", true},
		{`[\p{Lu}&&k]`, "iv", "k", true},
		{`[\p{Lu}&&k]`, "iv", "K", true},
		{`[\P{Lu}]`, "iv", "k", false},
		{`[^\P{Lu}]`, "iv", "k", true},
		{`[\q{AB}--\q{ab}]`, "iv", "ab", false},
		{`[\q{AB}&&\q{ab}]`, "iv", "Ab", true},
		{`[\w--k]`, "iv", "\u212A", false},
		{`[\W]`, "iv", "\u017F", false},
		{`[^\W]`, "iv", "\u017F", true},
		{`\P{Ll}`, "iv", "A", false},
		{`[^[^k]]`, "iv", "K", true},
		{`[[a-z]--[^K]]`, "iv", "k", true},
		{`(?i:[^k])`, "v", "K", false},
		{`(?-i:[^k])`, "iv", "K", true},
		{`(?i:\P{Lu})`, "v", "k", false},
		{`[\p{Ll}--\p{Lu}]`, "iv", "a", false},
		{`[\p{ASCII}--\p{Lu}]`, "iv", "a", false},
		{`[^\p{Lu}]`, "iv", "\u01C5", false},
		{`\P{Lt}`, "iv", "\u01C4", false},
		{`[\D--\d]`, "iv", "x", true},
		{`[\s&&\S]`, "iv", " ", false},
		{`[^\u212A]`, "iv", "k", false},
		{`[\u017F--s]`, "iv", "S", false},
		{`[^k]`, "v", "K", true},
		{`\P{Lu}`, "v", "k", true},
		// Not Node's answers.
		{`\P{ASCII}`, "iv", "\u212A", false},
		{`[\p{ASCII}--b]`, "iv", "\u212A", true},
	}
	for _, tt := range tests {
		re, err := Compile("^(?:"+tt.pattern+")$", tt.flags)
		if err != nil {
			t.Fatalf("/%s/%s: %v", tt.pattern, tt.flags, err)
		}
		caps, err := re.Match(utf16.Encode([]rune(tt.subject)), 0)
		if err != nil {
			t.Fatalf("/%s/%s: %v", tt.pattern, tt.flags, err)
		}
		if got := caps != nil; got != tt.want {
			t.Errorf("/%s/%s.test(%q) = %v, want %v", tt.pattern, tt.flags, tt.subject, got, tt.want)
		}
		caps, err = re.MatchString(tt.subject, 0)
		if err != nil {
			t.Fatalf("/%s/%s: %v", tt.pattern, tt.flags, err)
		}
		if got := caps != nil; got != tt.want {
			t.Errorf("/%s/%s.test(%q) over the string = %v, want %v", tt.pattern, tt.flags, tt.subject, got, tt.want)
		}
	}
}

// TestCaseClosure holds caseClosure to its definition over every code point
// that may be cased: a code point is in the closure of \p{Lu} when one of
// its fold orbit is in \p{Lu}.
func TestCaseClosure(t *testing.T) {
	lu, ok := unicodePropertyRanges("Lu")
	if !ok {
		t.Fatal("no Lu")
	}
	in := &charSet{ranges: lu}
	closed := &charSet{ranges: caseClosure(lu)}
	for c := rune(0); c < 0x20000; c++ {
		want := in.rawContains(c)
		for f := ucd.SimpleFold(c); f != c && !want; f = ucd.SimpleFold(f) {
			want = in.rawContains(f)
		}
		if got := closed.rawContains(c); got != want {
			t.Fatalf("U+%04X in the closure of Lu: %v, want %v", c, got, want)
		}
	}
}
