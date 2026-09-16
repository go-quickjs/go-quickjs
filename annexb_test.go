package quickjs_test

import (
	"testing"

	quickjs "github.com/go-quickjs/go-quickjs"
)

// Annex B is the legacy the specification keeps only because the web depends on
// it. None of this is a good idea in new code, but a host running existing code
// needs it to exist.
func TestAnnexB(t *testing.T) {
	cases := []struct{ src, want string }{
		// escape predates encodeURIComponent and writes %uXXXX above Latin-1,
		// a form nothing else understands.
		{`escape("a b")`, "a%20b"},
		{`escape("ሴ")`, "%u1234"},
		{`escape("@*_+-./")`, "@*_+-./"},
		{`unescape("a%20b")`, "a b"},
		{`unescape("%u1234") === "ሴ" + ""`, "true"},
		{`unescape("%zz")`, "%zz"},
		{`unescape(escape("Ünïcødé ✓"))`, "Ünïcødé ✓"},

		{`"x".anchor("y")`, `<a name="y">x</a>`},
		// Only the quote is escaped, which is the whole of the specified
		// behaviour -- and why none of these should build markup from
		// untrusted input.
		{`"x".anchor('a"b')`, `<a name="a&quot;b">x</a>`},
		{`"x".link("u")`, `<a href="u">x</a>`},
		{`"x".bold()`, "<b>x</b>"},
		{`"x".fixed()`, "<tt>x</tt>"},
		{`"x".fontsize(3)`, `<font size="3">x</font>`},

		// getYear reports the year minus 1900, which is the bug that made the
		// year 2000 interesting.
		{`String(new Date(Date.UTC(1999, 0, 1)).getUTCFullYear() - 1900)`, "99"},
		{`var d = new Date(0); d.setYear(99); String(d.getFullYear())`, "1999"},
		{`var d = new Date(0); d.setYear(2005); String(d.getFullYear())`, "2005"},
		{`String(new Date(0).toGMTString() === new Date(0).toUTCString())`, "true"},

		// A function declared in a block is also assigned to a var-scoped
		// binding of the same name.
		{`(function () { { function f() {} } return typeof f; })()`, "function"},
		{`(function () { if (true) function f() {} return typeof f; })()`, "function"},
		{`(function () { { function f() { return 1; } } return f(); })() + ""`, "1"},
		// It is only assigned once the declaration is reached.
		{`(function () { var before = typeof f; { function f() {} } return before; })()`,
			"undefined"},
		// Strict mode keeps the function inside its block.
		{`(function () { "use strict"; { function f() {} } return typeof f; })()`, "undefined"},
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

	// Strict mode refuses a function declaration as a statement body.
	rt := quickjs.New()
	defer rt.Close()
	if _, err := rt.Eval(`"use strict"; if (true) function f() {}`); err == nil {
		t.Error("a function declaration as an if body should be a strict-mode error")
	}
}
