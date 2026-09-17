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

// What a direct eval may write is what the call site may write. A field
// initializer is inside the constructor but is not it, so `super.x` is legal
// there and `super()` is not: there is only one constructor, and it is not
// this.
func TestDirectEvalInheritsTheCallSiteContext(t *testing.T) {
	cases := []struct{ src, want string }{
		{`class A { m() { return 1 } } class B extends A { x = eval("super.m()") }
		  String(new B().x)`, "1"},
		{`class B {} class D extends B { constructor() { eval("super()"); return this } }
		  String(new D() instanceof D)`, "true"},
		{`class A { static m() { return 2 } } class B extends A { static x = eval("super.m()") }
		  String(B.x)`, "2"},
		// The evaluated code is not run at all when it cannot be parsed, so a
		// side effect before the offending part does not happen.
		{`var executed = false; var A = class {};
		  var C = class extends A { x = eval("executed = true; () => super()[0]") };
		  try { new C() } catch (e) {} String(executed)`, "false"},
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
		{`var A = class {}; var C = class extends A { x = eval("() => super()[0]") }; new C()`,
			"SyntaxError"},
		// And outside a class there is no super at all.
		{`function f() { return eval("super.x") } f()`, "SyntaxError"},
		{`eval("super.x")`, "SyntaxError"},
		{`function f() { return eval("new.target") } "ok"; eval("new.target")`, "SyntaxError"},
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

// A direct eval in a parameter default runs in the parameter scope, which sits
// between it and the function's variable scope. A var it declares belongs to
// the variable scope, so a name the parameter scope already binds would be
// shadowed by that binding and never reachable -- which the specification makes
// an error rather than a surprise.
func TestEvalInAParameterDefault(t *testing.T) {
	bad := []string{
		`function f(p = eval("var arguments")) {} f()`,
		`function f(p = eval("var arguments")) { let arguments; } f()`,
		`function f(arguments, p = eval("var arguments")) {} f()`,
		`function f(a, p = eval("var a")) {} f()`,
		`function f(p = eval("function arguments() {}")) {} f()`,
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

	cases := []struct{ src, want string }{
		// A name the parameter scope does not bind is fine.
		{`function f(p = eval("var q = 1")) { return p } String(f())`, "undefined"},
		{`function f(p = eval("1 + 1")) { return p } String(f())`, "2"},

		// The arguments object exists in the parameter scope, so a default may
		// read it -- and the body may still bind the name for itself.
		{`function f(p = arguments[0]) { return p } String(f(7))`, "7"},
		{`function f(p = 1) { let arguments = 5; return arguments } String(f())`, "5"},

		// With a plain parameter list there is no separate scope, so a body
		// that binds the name is what the name means and no object is made.
		{`function f() { let arguments = 1; return arguments } String(f())`, "1"},
		{`function f() { function arguments() { return 2 } return arguments() } String(f())`,
			"2"},
		{`function f(arguments) { return arguments } String(f(3))`, "3"},
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

// new.target has a meaning only inside a function, and an arrow takes its
// enclosing function's. At the top level there is none to take, so a direct
// eval written in an arrow there may not mention it.
func TestDirectEvalNewTargetScope(t *testing.T) {
	cases := []struct{ name, src, want string }{
		{"arrow at top level", `var f = () => eval("new.target")
		  try { f(); "no error" } catch (e) { e.constructor.name }`, "SyntaxError"},
		{"arrow in a function", `function g() { return (() => eval("String(new.target)"))() }
		  g()`, "undefined"},
		{"arrow in a constructor", `function C() { return (() => eval("new.target"))() }
		  String(new C() === C)`, "true"},
		{"function at top level", `function g() { return eval("String(new.target)") }
		  g()`, "undefined"},
		{"nested eval in an arrow", `var f = () => eval("eval('new.target')")
		  try { f(); "no error" } catch (e) { e.constructor.name }`, "SyntaxError"},
		{"nested eval in a function", `function g() { return eval("eval('String(new.target)')") }
		  g()`, "undefined"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) { checkEval(t, tc.src, tc.want) })
	}
}

// A function declaration that eval creates on the global object keeps the
// attributes of a property it cannot redefine: a non-configurable global is
// assigned to rather than replaced.
func TestEvalFunctionKeepsNonConfigurableAttributes(t *testing.T) {
	checkEval(t, `
		Object.defineProperty(globalThis, "f", {
			value: 1, writable: true, enumerable: true, configurable: false,
		})
		eval("function f() { return 2222 }")
		var d = Object.getOwnPropertyDescriptor(globalThis, "f");
		[typeof f, f(), d.writable, d.enumerable, d.configurable].join(",")`,
		"function,2222,true,true,false")
}

// A spread element in the arguments does not make a direct eval indirect: what
// decides is that the callee was written as the name `eval`.
func TestDirectEvalWithSpreadArguments(t *testing.T) {
	cases := []struct{ name, src, want string }{
		{"spread source", `var x = "global";
		  (function () { var x = "local"; eval(...["x = 0;"]); return String(x) })()`, "0"},
		{"empty leading", `var x = "global";
		  (function () { var x = "local"; eval(...[], "x = 1;"); return String(x) })()`, "1"},
		{"empty trailing", `var x = "global";
		  (function () { var x = "local"; eval("x = 2;", ...[]); return String(x) })()`, "2"},
		// The global is left alone, which is what tells a direct eval from an
		// indirect one.
		{"global untouched", `var x = "global";
		  (function () { var x = "local"; eval(...["x = 0;"]) })(); x`, "global"},
		// The spread is still a spread: the iterable is walked once.
		{"iterable walked once", `var count = 0;
		  var iter = {}; iter[Symbol.iterator] = function () {
		    count++; return {next: function () { return {done: true} }} };
		  var r = (function () { var x = "l"; eval(...iter, "x = 4;"); return String(x) })();
		  r + "," + count`, "4,1"},
		// And a non-string is handed back whatever it arrived as.
		{"non-string", `String(eval(...[1]))`, "1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) { checkEval(t, tc.src, tc.want) })
	}
}

// A var a sloppy direct eval declares belongs to the function that called it,
// not to the global object: it is visible there and in anything created
// inside, it goes when the call does, and it can be deleted again.
func TestDirectEvalVarBelongsToTheCaller(t *testing.T) {
	cases := []struct{ name, src, want string }{
		{"visible in the caller", `function f() { eval("var q = 7"); return q }
		  f() + "," + typeof globalThis.q`, "7,undefined"},
		{"visible in a closure", `function f() { eval("var q = 7")
		    return (function () { return q })() }
		  String(f())`, "7"},
		{"visible to typeof", `function f() { eval("var q = 7"); return typeof q } f()`, "number"},
		{"gone afterwards", `function f() { eval("var q = 7") } f(); typeof q`, "undefined"},
		// An existing binding of the same name is used rather than shadowed.
		{"existing var kept", `function f() { var x = 1; eval("var x"); return x } String(f())`, "1"},
		{"existing var assigned", `function f() { var x = 1; eval("var x = 2"); return x }
		  String(f())`, "2"},
		// A global of the same name is shadowed, and left alone.
		{"shadows a global", `var q = "global"
		  function f() { eval("var q = 7"); return q }
		  f() + "," + q`, "7,global"},
		// A function declaration goes the same way.
		{"function declaration", `function f() { eval("function g() { return 3 }"); return g() }
		  String(f())`, "3"},
		// It is configurable, which a var the source names is not.
		{"deletable", `function f() { eval("var q = 7"); var gone = delete q
		    return gone + "," + typeof q }
		  f()`, "true,undefined"},
		// Strict eval keeps its vars to itself, and a script's top level still
		// declares on the global object.
		{"strict eval", `function f() { "use strict"; eval("var q = 7"); return typeof q }
		  f()`, "undefined"},
		{"global still global", `eval("var top = 1"); String(globalThis.top)`, "1"},
		// An arrow has a variable scope of its own, so the var lands there
		// rather than on the global.
		{"arrow", `var f = (p = eval("var arguments = 'param'")) => typeof arguments
		  f() + "," + typeof globalThis.arguments`, "string,undefined"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) { checkEval(t, tc.src, tc.want) })
	}
}

// A var a direct eval declares shadows a binding of an enclosing function, and
// it comes into being where the declaration runs: a reference resolved before
// then still names what it named. That is what tells `x *= (eval("var x = 2"),
// 4)` apart from an assignment written after the eval.
func TestDirectEvalVarShadowsAnOuterBinding(t *testing.T) {
	cases := []struct{ name, src, want string }{
		{"compound assignment", `function t() {
		    var x = 3
		    var inner = (function () { x *= (eval("var x = 2;"), 4); return x })()
		    return inner + "," + x }
		  t()`, "2,12"},
		{"plain assignment", `function t() {
		    var x = 3
		    var inner = (function () { x = (eval("var x = 2;"), 4); return x })()
		    return inner + "," + x }
		  t()`, "2,4"},
		{"after the eval", `function t() {
		    var x = 3
		    var inner = (function () { eval("var x = 2;"); x = 9; return x })()
		    return inner + "," + x }
		  t()`, "9,3"},
		{"read after", `function t() {
		    var x = 3
		    var inner = (function () { eval("var x = 2;"); return x })()
		    return inner + "," + x }
		  t()`, "2,3"},
		// Declared without a value, it is undefined rather than the outer one.
		{"no initializer", `function t() {
		    var x = 3
		    var inner = (function () { eval("var x;"); return String(x) })()
		    return inner + "," + x }
		  t()`, "undefined,3"},
		// A binding of the calling function itself is used rather than
		// shadowed, whatever scope it was written in.
		{"own var kept", `function t() { var x = 3; eval("var x"); return x } String(t())`, "3"},
		{"parameter kept", `function t(x) { eval("var x"); return x } String(t(5))`, "5"},
		// A lexical binding of the calling function refuses the declaration.
		{"lexical refuses", `function t() { let x; eval("var x;") }
		  try { t(); "no error" } catch (e) { e.constructor.name }`, "SyntaxError"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) { checkEval(t, tc.src, tc.want) })
	}
}
