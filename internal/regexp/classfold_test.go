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
// after the complement, \P{Lu} matches "K". The answers are the
// specification's and QuickJS's, and want is Node's too, which
// CompileNodeQuirks gives, but for the cases that say Node's apart: V8
// leaves the Kelvin sign and the long s out of what \p{ASCII} folds to, and
// folds a \q{...} string of one character to its folding alone.
func TestClassSetFoldsOperands(t *testing.T) {
	type test struct {
		pattern, flags, subject string
		want                    bool
		// node is Node's answer where it is not want.
		node *bool
	}
	no, yes := false, true
	tests := []test{
		{`[^k]`, "iv", "K", false, nil},
		{`\P{Lu}`, "iv", "K", false, nil},
		{`[\p{Lu}--K]`, "iv", "k", false, nil},
		{`[^k]`, "iu", "K", false, nil},
		{`\P{Lu}`, "iu", "K", true, nil},
		{`[\p{Lu}&&k]`, "iv", "k", true, nil},
		{`[\p{Lu}&&k]`, "iv", "K", true, nil},
		{`[\P{Lu}]`, "iv", "k", false, nil},
		{`[^\P{Lu}]`, "iv", "k", true, nil},
		{`[\q{AB}--\q{ab}]`, "iv", "ab", false, nil},
		{`[\q{AB}&&\q{ab}]`, "iv", "Ab", true, nil},
		{`[\w--k]`, "iv", "\u212A", false, nil},
		{`[\W]`, "iv", "\u017F", false, nil},
		{`[^\W]`, "iv", "\u017F", true, nil},
		{`\P{Ll}`, "iv", "A", false, nil},
		{`[^[^k]]`, "iv", "K", true, nil},
		{`[[a-z]--[^K]]`, "iv", "k", true, nil},
		{`(?i:[^k])`, "v", "K", false, nil},
		{`(?-i:[^k])`, "iv", "K", true, nil},
		{`(?i:\P{Lu})`, "v", "k", false, nil},
		{`[\p{Ll}--\p{Lu}]`, "iv", "a", false, nil},
		{`[\p{ASCII}--\p{Lu}]`, "iv", "a", false, nil},
		{`[^\p{Lu}]`, "iv", "\u01C5", false, nil},
		{`\P{Lt}`, "iv", "\u01C4", false, nil},
		{`[\D--\d]`, "iv", "x", true, nil},
		{`[\s&&\S]`, "iv", " ", false, nil},
		{`[^\u212A]`, "iv", "k", false, nil},
		{`[\u017F--s]`, "iv", "S", false, nil},
		{`[^k]`, "v", "K", true, nil},
		{`\P{Lu}`, "v", "k", true, nil},
		{`\p{ASCII}`, "iv", "K", true, nil},
		{`[\p{ASCII}k]`, "iv", "\u212A", true, nil},
		{`[\q{C}]`, "iv", "c", true, nil},
		{`[\q{AB}]`, "iv", "ab", true, nil},
		{`[\q{C}]`, "v", "c", false, nil},
		// Node's answers apart.
		{`\P{ASCII}`, "iv", "\u212A", false, &yes},
		{`[^\p{ASCII}]`, "iv", "\u212A", false, &yes},
		{`\p{ASCII}`, "iv", "\u017F", true, &no},
		{`[\p{ASCII}--b]`, "iv", "\u212A", true, &no},
		{`[\p{L}&&\p{ASCII}]`, "iv", "\u212A", true, &no},
		{`[\q{C}]`, "iv", "C", true, &no},
		{`[\q{k}]`, "iv", "\u212A", true, &no},
		{`[^\q{c}]`, "iv", "C", false, &yes},
		{`[\q{AB|c}&&\S]`, "iv", "C", true, &no},
		{`[\q{\u212A}]`, "iv", "k", true, nil},
	}
	for _, quirks := range []bool{false, true} {
		compile := Compile
		if quirks {
			compile = CompileNodeQuirks
		}
		for _, tt := range tests {
			want := tt.want
			if quirks && tt.node != nil {
				want = *tt.node
			}
			re, err := compile("^(?:"+tt.pattern+")$", tt.flags)
			if err != nil {
				t.Fatalf("/%s/%s: %v", tt.pattern, tt.flags, err)
			}
			caps, err := re.Match(utf16.Encode([]rune(tt.subject)), 0)
			if err != nil {
				t.Fatalf("/%s/%s: %v", tt.pattern, tt.flags, err)
			}
			if got := caps != nil; got != want {
				t.Errorf("quirks %v: /%s/%s.test(%q) = %v, want %v", quirks, tt.pattern, tt.flags, tt.subject, got, want)
			}
			caps, err = re.MatchString(tt.subject, 0)
			if err != nil {
				t.Fatalf("/%s/%s: %v", tt.pattern, tt.flags, err)
			}
			if got := caps != nil; got != want {
				t.Errorf("quirks %v: /%s/%s.test(%q) over the string = %v, want %v", quirks, tt.pattern, tt.flags, tt.subject, got, want)
			}
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
