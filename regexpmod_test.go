package quickjs_test

import (
	"strings"
	"testing"

	quickjs "github.com/go-quickjs/go-quickjs"
)

// Inline modifiers let a pattern be case-insensitive, multiline or dot-all in
// one place without being so everywhere, which otherwise takes two patterns or
// a hand-expanded character class.
func TestRegExpModifiers(t *testing.T) {
	cases := []struct{ src, want string }{
		// i applies only inside the group.
		{`String(/(?i:a)b/.test("Ab"))`, "true"},
		{`String(/(?i:a)b/.test("AB"))`, "false"},
		{`String(/(?i:a)b/.test("ab"))`, "true"},
		{`String(/a(?i:b)c/.test("aBc"))`, "true"},
		{`String(/a(?i:b)c/.test("aBC"))`, "false"},
		// And -i turns off what the pattern's own flag turned on.
		{`String(/(?-i:a)b/i.test("Ab"))`, "false"},
		{`String(/(?-i:a)b/i.test("aB"))`, "true"},

		{`String(/(?s:.)/.test("\n"))`, "true"},
		{`String(/./.test("\n"))`, "false"},
		{`String(/(?-s:.)/s.test("\n"))`, "false"},

		{`String(/(?m:^b)/.test("a\nb"))`, "true"},
		{`String(/^b/.test("a\nb"))`, "false"},
		{`String(/(?-m:^b)/m.test("a\nb"))`, "false"},

		// Several flags at once, in both directions.
		{`String(/(?im-s:a.)/.test("A\n"))`, "false"},
		{`String(/(?im-s:^a.)/.test("x\nAb"))`, "true"},

		// Nesting, where the inner group overrides the outer.
		{`String(/(?i:a(?-i:b)c)/.test("AbC"))`, "true"},
		{`String(/(?i:a(?-i:b)c)/.test("ABC"))`, "false"},

		// A modifier group does not capture.
		{`/(?i:a)(b)/.exec("Ab")[1]`, "b"},
		{`String(/(?i:a)(b)/.exec("Ab").length)`, "2"},

		// Patterns without modifiers are unaffected.
		{`String(/^a$/m.test("b\na"))`, "true"},
		{`String(/A/i.test("a"))`, "true"},
		{`"aXbXc".split(/x/i).join("-")`, "a-b-c"},
		{`String(/a.c/s.test("a\nc"))`, "true"},
	}

	for _, tc := range cases {
		rt := quickjs.New()
		v, err := rt.Eval(tc.src)
		if err != nil {
			t.Errorf("%s: %v", tc.src, err)
		} else if got := v.String(); got != tc.want {
			t.Errorf("%s = %q, want %q", tc.src, got, tc.want)
		}
		rt.Close()
	}
}

func TestRegExpModifierErrors(t *testing.T) {
	bad := []string{
		// A flag may appear once across both halves.
		`new RegExp("(?ii:a)")`,
		`new RegExp("(?i-i:a)")`,
		// Each half must name something.
		`new RegExp("(?-:a)")`,
		// Only i, m and s are modifiers.
		`new RegExp("(?x:a)")`,
		`new RegExp("(?u:a)")`,
		// The flags must be followed by a colon.
		`new RegExp("(?i a)")`,
		`new RegExp("(?i)")`,
	}
	for _, src := range bad {
		rt := quickjs.New()
		_, err := rt.Eval(src)
		if err == nil {
			t.Errorf("%s: accepted, want SyntaxError", src)
		} else if !strings.Contains(err.Error(), "SyntaxError") {
			t.Errorf("%s: got %v, want SyntaxError", src, err)
		}
		rt.Close()
	}
}

// A property escape may name its property and value in any spelling Unicode
// publishes, so all of these describe the same class.
func TestRegExpPropertyEscapes(t *testing.T) {
	cases := []struct{ src, want string }{
		{`String(/\p{Script=Latin}/u.test("a"))`, "true"},
		{`String(/\p{Script=Latn}/u.test("a"))`, "true"},
		{`String(/\p{sc=Latn}/u.test("a"))`, "true"},
		{`String(/\p{sc=Grek}/u.test("α"))`, "true"},
		// The abbreviation is sometimes the longer string.
		{`String(/\p{Script=Han}/u.test("一"))`, "true"},
		{`String(/\p{Script=Hani}/u.test("一"))`, "true"},

		{`String(/\p{General_Category=Lowercase_Letter}/u.test("a"))`, "true"},
		{`String(/\p{gc=Ll}/u.test("a"))`, "true"},
		{`String(/\p{Lowercase_Letter}/u.test("a"))`, "true"},
		{`String(/\p{Lu}/u.test("A")) + "," + String(/\p{Lu}/u.test("a"))`, "true,false"},

		{`String(/\p{Alpha}/u.test("a")) + "," + String(/\p{Alpha}/u.test("1"))`, "true,false"},
		{`String(/\p{Alphabetic}/u.test("a"))`, "true"},
		{`String(/\p{AHex}/u.test("f")) + "," + String(/\p{AHex}/u.test("g"))`, "true,false"},
		{`String(/\p{White_Space}/u.test(" "))`, "true"},
		{`String(/\p{Cased}/u.test("a")) + "," + String(/\p{Cased}/u.test("1"))`, "true,false"},
		{`String(/\p{ID_Start}/u.test("a")) + "," + String(/\p{ID_Start}/u.test("$"))`,
			"true,false"},

		{`String(/\p{Any}/u.test("x")) + "," + String(/\p{ASCII}/u.test("x"))`, "true,true"},
		// The negated form.
		{`String(/\P{Script=Latn}/u.test("a"))`, "false"},
		{`String(/\P{Lu}/u.test("a"))`, "true"},
	}

	for _, tc := range cases {
		rt := quickjs.New()
		v, err := rt.Eval(tc.src)
		if err != nil {
			t.Errorf("%s: %v", tc.src, err)
		} else if got := v.String(); got != tc.want {
			t.Errorf("%s = %q, want %q", tc.src, got, tc.want)
		}
		rt.Close()
	}

	for _, src := range []string{`/\p{Nope}/u`, `/\p{Script=Nope}/u`, `/\p{gc=Nope}/u`} {
		rt := quickjs.New()
		if _, err := rt.Eval(src); err == nil {
			t.Errorf("%s: accepted an unknown property", src)
		}
		rt.Close()
	}
}
