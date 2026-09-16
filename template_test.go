package quickjs_test

import (
	"strings"
	"testing"

	quickjs "github.com/go-quickjs/go-quickjs"
)

// A tag is called with the site's strings and the substitutions separately,
// which is what lets it see the text the substitutions were written between.
func TestTaggedTemplates(t *testing.T) {
	const tag = `function t(s, ...v) { return JSON.stringify([[...s], s.raw, v]); }
	`
	cases := []struct{ src, want string }{
		{tag + "t`hi`", `[["hi"],["hi"],[]]`},
		{tag + "t`a${1}b`", `[["a","b"],["a","b"],[1]]`},
		{tag + "t`${1}`", `[["",""],["",""],[1]]`},
		{tag + "t``", `[[""],[""],[]]`},

		// raw is what was written; cooked is what the escapes mean.
		{"function t(s) { return s.raw[0] + '|' + s[0]; } t`a\\nb`", `a\nb|a` + "\nb"},
		{"String.raw`a\\nb`", `a\nb`},
		{"String.raw`\\u{`", `\u{`},
		// A malformed escape has no cooked meaning, which is an error in an
		// ordinary template and merely undefined here.
		{"function t(s) { return String(s[0]) + '|' + s.raw[0]; } t`\\u{`",
			`undefined|\u{`},
		{"function t(s) { return String(s[0]); } t`\\01`", "undefined"},

		// A member tag keeps its receiver, exactly as a method call does.
		{"var o = {n: 'O', t(s) { return this.n + s[0]; }}; o.t`x`", "Ox"},
		{"var o = {n: 'O', t(s) { return this.n + s[0]; }}; o['t']`x`", "Ox"},
		{"class C { static m(s) { return this.name + s[0]; } } C.m`x`", "Cx"},
		// A non-member tag has no receiver, which in strict mode stays
		// undefined rather than becoming the global object.
		{"'use strict'; function t(s) { return String(this); } t`x`", "undefined"},

		// The site's object is reused, because a tag may hang state off it or
		// compare it against a later call's.
		{`var seen = []; function t(s) { seen.push(s); return s; }
		  function f() { return t` + "`x`" + `; }
		  f(); f(); String(seen[0] === seen[1])`, "true"},
		// Two sites spelled the same are still two sites.
		{`var seen = []; function t(s) { seen.push(s); }
		  t` + "`x`" + `; t` + "`x`" + `; String(seen[0] === seen[1])`, "false"},

		// The strings are frozen, since the same object comes back next time.
		{`function t(s) { return [Object.isFrozen(s), Object.isFrozen(s.raw)].join(","); }
		  t` + "`x`", "true,true"},
		{`function t(s) { "use strict"; try { s[0] = "y"; } catch (e) { return "threw"; }
		    return s[0]; } t` + "`x`", "threw"},

		// Ordinary chaining and nesting.
		{"function t(s) { return s[0]; } (t)`x`", "x"},
		{"function f() { return (s) => s[0]; } f()`x`", "x"},
		{"function t(s) { return s[0]; } var a = [t]; a[0]`x`", "x"},
		{"function t(s, v) { return s[0] + v; } t`a${`b${1}`}`", "ab1"},
		{"function t(s) { return () => s[0]; } t`x`()", "x"},
		{"function t(s) { return { m: () => s[0] }; } t`x`.m()", "x"},
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

	bad := []string{
		"undefined`x`",
		"null`x`",
		"({})`x`",
		// A malformed escape is still an error in an untagged template.
		"`\\u{`",
	}
	for _, src := range bad {
		rt := quickjs.New()
		if _, err := rt.Eval(src); err == nil {
			t.Errorf("%s: accepted, want an error", src)
		}
		rt.Close()
	}
}

// A modifier group turns a flag on or off for part of a pattern, which is the
// only way to vary one within a single regular expression.
func TestRegExpModifierGroups(t *testing.T) {
	cases := []struct{ src, want string }{
		{`String(new RegExp("(?i:a)b").test("Ab"))`, "true"},
		{`String(new RegExp("(?i:a)b").test("AB"))`, "false"},
		{`String(new RegExp("(?-i:a)b", "i").test("Ab"))`, "false"},
		{`String(new RegExp("(?-i:a)b", "i").test("aB"))`, "true"},
		{`String(new RegExp("(?m:^b)").test("a\nb"))`, "true"},
		{`String(new RegExp("(?s:.)").test("\n"))`, "true"},
		// Either half may be empty, but not both.
		{`String(new RegExp("(?m-:es$)").test("es"))`, "true"},
		{`String(new RegExp("(?ms-:a)").test("a"))`, "true"},
		{`String(new RegExp("(?-m:^b)", "m").test("a\nb"))`, "false"},
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

	bad := []string{
		// Modifying nothing.
		`new RegExp("(?-:a)")`,
		// A flag that is not one of the three a group may vary.
		`new RegExp("(?g:a)")`,
		`new RegExp("(?u:a)")`,
		// A flag named twice, in either half.
		`new RegExp("(?ii:a)")`,
		`new RegExp("(?i-i:a)")`,
	}
	for _, src := range bad {
		rt := quickjs.New()
		if _, err := rt.Eval(src); err == nil {
			t.Errorf("%s: accepted, want SyntaxError", src)
		} else if !strings.Contains(err.Error(), "SyntaxError") {
			t.Errorf("%s: got %v, want SyntaxError", src, err)
		}
		rt.Close()
	}
}
