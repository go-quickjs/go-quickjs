package quickjs_test

import (
	"testing"

	quickjs "github.com/go-quickjs/go-quickjs"
)

// A compound assignment to a member evaluates the object and the key once. They
// are expressions: base[prop] *= f() must call prop.toString once, and reading
// the property and writing it back have to address the same place even if the
// read changed what is there.
func TestCompoundAssignmentEvaluatesOnce(t *testing.T) {
	cases := []struct{ src, want string }{
		{`var n = 0; var p = {toString() { n++; return "x" }}; var b = {x: 2};
		  b[p] *= 3; [b.x, n].join(",")`, "6,1"},
		{`var n = 0; var p = {toString() { n++; return "x" }}; var b = {x: 2};
		  b[p] ??= 9; [b.x, n].join(",")`, "2,1"},
		{`var n = 0; var p = {toString() { n++; return "y" }}; var b = {};
		  b[p] ??= 9; [b.y, n].join(",")`, "9,1"},
		{`var n = 0; var o = {};
		  Object.defineProperty(o, "x", {get() { n++; return 1 }, set(v) {}});
		  o.x += 1; String(n)`, "1"},
		// The object expression is evaluated once too.
		{`var n = 0; function f() { n++; return {x: 1} } f().x += 1; String(n)`, "1"},

		{`var o = {a: 2}; o.a += 3; String(o.a)`, "5"},
		{`var o = {a: 2}; o.a ||= 9; String(o.a)`, "2"},
		{`var o = {a: 0}; o.a ||= 9; String(o.a)`, "9"},
		{`var o = {a: null}; o.a ??= 9; String(o.a)`, "9"},
		{`var o = {a: 1}; String(o.a ||= 2)`, "1"},
		{`var o = {a: null}; String(o.a ??= 2)`, "2"},
		{`var a = [1, 2]; a[0] += 5; a.join(",")`, "6,2"},

		{`class C { #v = 1; bump() { this.#v += 2; return this.#v } }
		  String(new C().bump())`, "3"},
		{`class C { #v = 0; f() { this.#v ||= 5; return this.#v } }
		  String(new C().f())`, "5"},

		// And the plain-identifier forms are unchanged.
		{`var x = 1; x += 2; String(x)`, "3"},
		{`var x = 0; x ||= 7; String(x)`, "7"},
		{`var x = 1; x &&= 7; String(x)`, "7"},
		{`var x; x ??= 7; String(x)`, "7"},
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

// TestParenthesizedTargetHasNoName covers naming an anonymous function after
// the variable it is assigned to, which a parenthesised target does not do:
// parentheses stop it being an identifier reference.
func TestParenthesizedTargetHasNoName(t *testing.T) {
	cases := []struct{ src, want string }{
		{`var fn; (fn) = function () {}; JSON.stringify(fn.name)`, `""`},
		{`var fn; (fn) = () => {}; JSON.stringify(fn.name)`, `""`},
		{`var fn; (fn) = class {}; JSON.stringify(fn.name)`, `""`},
		{`var fn; fn = function () {}; JSON.stringify(fn.name)`, `"fn"`},
		{`var fn = function () {}; JSON.stringify(fn.name)`, `"fn"`},
		{`let fn = class {}; JSON.stringify(fn.name)`, `"fn"`},
	}
	for _, tc := range cases {
		checkEval(t, tc.src, tc.want)
	}
}
