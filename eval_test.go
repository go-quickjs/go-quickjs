package quickjs_test

import (
	"strings"
	"testing"

	quickjs "github.com/go-quickjs/go-quickjs"
)

// `eval(src)` written as a plain call runs its code in the caller's scope: it
// reads and writes the caller's variables and sees the caller's `this`,
// `new.target` and `super`. Calling the same function through anything else
// runs the code in global scope, which is what makes `(0, eval)(src)` the way
// to ask for a sandbox.
func TestDirectEval(t *testing.T) {
	cases := []struct{ src, want string }{
		// The caller's bindings, read and written.
		{`function f() { var x = 1; return eval("x") } String(f())`, "1"},
		{`function f() { var x = 1; eval("x = 2"); return x } String(f())`, "2"},
		{`function f() { let y = 3; return eval("y") } String(f())`, "3"},
		{`function f() { const c = 4; return eval("c") } String(f())`, "4"},
		{`function f(p) { return eval("p") } String(f(5))`, "5"},
		// Including one from an enclosing function.
		{`function outer() { var a = 6; function inner() { return eval("a") } return inner() }
		  String(outer())`, "6"},
		// And through a closure the evaluated code creates.
		{`function f() { var x = 7; return eval("(function () { return x })()") } String(f())`, "7"},
		// A nested direct eval names the same bindings.
		{`function f() { var a = 8; return eval("eval(\"a\")") } String(f())`, "8"},
		{`function f() { var a = 9; return eval("(function () { return eval(\"a\") })()") }
		  String(f())`, "9"},

		// The caller's this, new.target, arguments and super.
		{`var o = {m() { return eval("this") }}; String(o.m() === o)`, "true"},
		{`function F() { return eval("new.target") } String(new F() === F)`, "true"},
		{`function f() { return eval("new.target") } String(f())`, "undefined"},
		{`function f(a, b) { return eval("arguments.length") } String(f(1, 2, 3))`, "3"},
		{`class A { m() { return 10 } }
		  class B extends A { n() { return eval("super.m()") } } String(new B().n())`, "10"},
		{`class C { #p = 11; m() { return eval("this.#p") } } String(new C().m())`, "11"},
		// Strictness is inherited.
		{`(function () { "use strict"; return eval("this") === undefined ? "undefined" : "other" })()`,
			"undefined"},

		// An indirect eval runs in global scope and sees none of it.
		{`var e = eval; function f() { var x = 1;
		    try { e("x") } catch (err) { return err.constructor.name } } f()`, "ReferenceError"},
		{`var x = "global"; var e = eval; function f() { var x = "local"; return e("x") } f()`,
			"global"},
		{`String(eval("1 + 1"))`, "2"},
		// eval of anything that is not a string is that thing.
		{`String(eval(42))`, "42"},

		// A var eval declares is configurable, unlike one a script declares:
		// the evaluated code could have declared it anywhere, so nothing
		// should be able to rely on it.
		{`eval("var v = 1");
		  String(Object.getOwnPropertyDescriptor(globalThis, "v").configurable)`, "true"},
		{`var w = 1;
		  String(Object.getOwnPropertyDescriptor(globalThis, "w").configurable)`, "false"},
		{`eval("function h() { return 12 }"); String(h())`, "12"},

		// Strict eval code has a variable environment of its own, which is
		// where eval stops being able to reach into its surroundings.
		{`(function () { "use strict"; eval("var z = 3"); return typeof z })()`, "undefined"},
		{`(function () { "use strict"; return eval("var z = 3; z") })()`, "3"},
		{`"use strict"; eval("var q = 1"); typeof q`, "undefined"},
		// Sloppy eval still declares into the enclosing function.
		{`function f() { eval("var y = 2"); return typeof y } f()`, "number"},
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

	bad := []struct{ src, want string }{
		// The caller's const is still a const.
		{`function f() { const c = 1; eval("c = 2") } f()`, "TypeError"},
		// A syntax error in the evaluated code is reported as one.
		{`eval("(")`, "SyntaxError"},
		{`function f() { eval("var 1 = 2") } f()`, "SyntaxError"},
	}
	for _, tc := range bad {
		rt := quickjs.New()
		if _, err := rt.Eval(tc.src); err == nil {
			t.Errorf("%s: accepted, want %s", tc.src, tc.want)
		} else if !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: got %v, want %s", tc.src, err, tc.want)
		}
		rt.Close()
	}
}

// Code generation can be turned off, and a direct eval has to be refused too --
// it is the same capability reached a different way. With it off there is no
// eval to reach at all, so naming it is a ReferenceError.
func TestDirectEvalRespectsCodeGenerationOff(t *testing.T) {
	rt := quickjs.New(quickjs.WithoutCodeGeneration())
	defer rt.Close()

	for _, src := range []string{
		`eval("1")`,
		`function f() { var x = 1; return eval("x") } f()`,
		`(0, eval)("1")`,
		`typeof Function === "undefined" ? (() => { throw new TypeError("no Function") })() : Function("return 1")()`,
	} {
		if _, err := rt.Eval(src); err == nil {
			t.Errorf("%s: accepted, want a refusal", src)
		}
	}
	// And nothing it would have needed is there.
	if v, err := rt.Eval(`typeof eval`); err != nil {
		t.Fatal(err)
	} else if v.String() != "undefined" {
		t.Errorf("typeof eval = %q, want undefined", v.String())
	}
}
