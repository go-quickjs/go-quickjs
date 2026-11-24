package quickjs_test

import (
	"testing"

	quickjs "github.com/go-quickjs/go-quickjs"
)

// TestNodeQuirksRegExpFolding pins both answers where V8 folds a v-mode
// class's operands under i otherwise than the standard (KI-90): \p{ASCII}
// takes in neither the Kelvin sign nor the long s, which fold into it, and
// a \q{...} string of one character is its folding alone, matched as it is.
// Everything else folds alike, as the last cases show.
func TestNodeQuirksRegExpFolding(t *testing.T) {
	t.Parallel()
	cases := []struct{ src, standard, node string }{
		{`/^\p{ASCII}$/iv.test("K")`, "true", "false"},
		{`/^\p{ASCII}$/iv.test("ſ")`, "true", "false"},
		{`/^\P{ASCII}$/iv.test("K")`, "false", "true"},
		{`/^[^\p{ASCII}]$/iv.test("K")`, "false", "true"},
		{`/^[\p{ASCII}--b]$/iv.test("K")`, "true", "false"},
		{`/^[\p{L}&&\p{ASCII}]$/iv.test("K")`, "true", "false"},
		{`/^[\q{C}]$/iv.test("C")`, "true", "false"},
		{`/^[\q{C}]$/iv.test("c")`, "true", "true"},
		{`/^[\q{k}]$/iv.test("K")`, "true", "false"},
		{`/^[^\q{c}]$/iv.test("C")`, "false", "true"},
		{`/^[\q{AB|c}&&\S]$/iv.test("C")`, "true", "false"},
		// Alike in both.
		{`/^\p{ASCII}$/iv.test("K")`, "true", "true"},
		{`/^[\p{ASCII}k]$/iv.test("K")`, "true", "true"},
		{`/^[\q{AB}]$/iv.test("ab")`, "true", "true"},
		{`/^[^k]$/iv.test("K")`, "false", "false"},
		{`/^\P{Lu}$/iv.test("K")`, "false", "false"},
		{`/^[\p{Lu}&&k]$/iv.test("k")`, "true", "true"},
		{`/^\p{ASCII}$/u.test("K")`, "false", "false"},
		{`/^[\q{C}]$/v.test("c")`, "false", "false"},
	}
	for _, mode := range []struct {
		name string
		opts []quickjs.Option
		want func(standard, node string) string
	}{
		{"standard", nil, func(s, _ string) string { return s }},
		{"node", []quickjs.Option{quickjs.WithNodeQuirks()}, func(_, n string) string { return n }},
	} {
		t.Run(mode.name, func(t *testing.T) {
			rt := quickjs.New(mode.opts...)
			defer rt.Close()
			for _, tc := range cases {
				v, err := rt.Eval(tc.src)
				if err != nil {
					t.Errorf("%s: %v", tc.src, err)
				} else if got, want := v.String(), mode.want(tc.standard, tc.node); got != want {
					t.Errorf("%s = %s, want %s", tc.src, got, want)
				}
			}
		})
	}
}
