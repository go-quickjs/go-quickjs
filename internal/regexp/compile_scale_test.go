package regexp

import (
	"strings"
	"testing"
	"time"
)

// TestGroupNamesShareOnlyAcrossAlternatives pins when two groups may share a
// name -- only where no match can take part in both -- as V8 decides it, now
// that the decision walks a trie of the groups' places rather than comparing
// each with every other.
func TestGroupNamesShareOnlyAcrossAlternatives(t *testing.T) {
	for src, ok := range map[string]bool{
		`(?<a>x)(?<a>y)`:                     false,
		`(?<a>x)|(?<a>y)`:                    true,
		`((?<a>x)|(?<a>y))`:                  true,
		`(?<a>x)|((?<a>y)(?<a>z))`:           false,
		`(?:(?<a>x)|y)(?<a>z)`:               false,
		`(?<a>(?<a>x)|y)`:                    false,
		`(?:(?<a>x)|(?<a>y))|(?<a>z)`:        true,
		`(?:(?<a>x)|(?<a>y))(?<b>z)|(?<b>w)`: true,
		`((?<a>x))|((?<a>y))`:                true,
		`(?<a>x)|(?:(?<a>y)|(?<a>z))`:        true,
	} {
		if _, err := Compile(src, ""); (err == nil) != ok {
			t.Errorf("%s: err = %v, want allowed %v", src, err, ok)
		}
	}
}

// TestCompilingIsLinear pins that compiling does not slow down by the square
// of a pattern's nesting or of its groups sharing a name: a hundred thousand
// nested groups took minutes (KI-16).
func TestCompilingIsLinear(t *testing.T) {
	for name, src := range map[string]string{
		"nested":      strings.Repeat("(", 100000) + strings.Repeat(")", 100000),
		"non-capture": strings.Repeat("(?:", 100000) + strings.Repeat(")", 100000),
		"shared name": strings.TrimSuffix(strings.Repeat("(?<a>x)|", 30000), "|"),
	} {
		start := time.Now()
		if _, err := Compile(src, ""); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if d := time.Since(start); d > 3*time.Second {
			t.Errorf("%s took %v", name, d)
		}
	}
}
