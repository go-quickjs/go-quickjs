package quickjs_test

import (
	"strings"
	"testing"

	quickjs "github.com/go-quickjs/go-quickjs"
)

// Early errors are the syntax a conforming engine must reject before running
// anything. They are easy to get wrong in both directions -- accepting what the
// grammar forbids, and rejecting code that only looks similar -- so both
// directions are pinned here.

func TestEarlyErrors(t *testing.T) {
	cases := []struct {
		name string
		src  string
	}{
		{"const without initializer", `const x;`},
		{"assign to const", `"use strict"; const x = 1; x = 2;`},
		{"duplicate let", `let x; let x;`},
		{"let and const collide", `let x; const x = 1;`},
		{"let as a lexical name", `let let = 1;`},
		{"delete a name in strict mode", `"use strict"; var x; delete x;`},
		{"with in strict mode", `"use strict"; with ({}) {}`},
		{"octal literal in strict mode", `"use strict"; 01;`},
		{"eval as a binding in strict mode", `"use strict"; var eval = 1;`},
		{"duplicate parameters in strict mode", `"use strict"; function f(a, a) {}`},
		{"duplicate parameters in an arrow", `(a, a) => {};`},
		{"return outside a function", `return 1;`},
		{"new.target outside a function", `new.target;`},
		{"super outside a method", `super.x;`},
		{"break with no target", `break;`},
		{"continue with no loop", `continue;`},
		{"duplicate label", `a: a: ;`},
		{"yield reserved in strict mode", `"use strict"; var yield = 1;`},
		{"duplicate __proto__", `({__proto__: 1, __proto__: 2});`},
		{"duplicate regexp flag", `/a/gg;`},
		{"unknown regexp flag", `/a/q;`},
		{"u and v regexp flags together", `/a/uv;`},
		{"static member named prototype", `class C { static prototype() {} }`},
		{"duplicate private field", `class C { #a; #a; }`},
		{"duplicate private accessor", `class C { get #a() {} set #a(v) {} get #a() {} }`},
		{"private name constructor", `class C { #constructor; }`},
		{"field named constructor", `class C { constructor = 1; }`},
		{"static field named prototype", `class C { static prototype = 1; }`},
		{"arguments in a field initializer", `class C { a = arguments; }`},
		{"arguments in a static block", `class C { static { arguments; } }`},
		{"yield in arrow parameters", `function *g() { (x = yield) => {}; }`},
		{"var colliding with let in a block", `{ let x; var x; }`},
		{"var colliding with a for-of binding", `for (let x of []) { let x; var x; }`},
		{"var colliding with let in a function", `function f() { { let x; var x; } }`},
		{"lexical declaration as a loop body", `while (0) let x;`},
		{"invalid assignment target", `1 = 2;`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rt := quickjs.New()
			defer rt.Close()
			_, err := rt.Eval(tc.src)
			if err == nil {
				t.Fatalf("%s: accepted, want SyntaxError", tc.src)
			}
			if !strings.Contains(err.Error(), "SyntaxError") {
				t.Fatalf("%s: got %v, want SyntaxError", tc.src, err)
			}
		})
	}
}

// TestEarlyErrorsDoNotOverreach pins the near misses: programs that resemble an
// early error but are legal, and which a check written too broadly would
// reject.
func TestEarlyErrorsDoNotOverreach(t *testing.T) {
	cases := []struct {
		name string
		src  string
	}{
		// A var only conflicts with a lexical binding whose scope encloses it.
		{"block closes before the var", `function f() { { let x; } var x; }`},
		{"var precedes the shadowing block", `var x; { let x; }`},
		{"var inside a function under a let", `function f() { var x; { let x; } }`},
		{"loop variable shadowed in the body", `for (var i = 0; i < 1; i++) { let i; }`},
		{"unrelated names", `let a; { var b; }`},
		// Annex B keeps this working, and the web depends on it.
		{"var redeclaring a simple catch parameter", `try {} catch (e) { var e; }`},
		// arguments is only unavailable in a field initializer itself.
		{"arguments inside a method", `class C { m() { return arguments; } }`},
		{"arguments inside a field's function", `class C { a = function () { return arguments; }; }`},
		{"arguments inside a static block's function", `class C { static { (function () { return arguments; }); } }`},
		// A getter and a setter may share one private name.
		{"paired private accessors", `class C { get #a() {} set #a(v) {} }`},
		{"private field and method differ", `class C { #a; #b() {} }`},
		{"static method not named prototype", `class C { static m() {} }`},
		// yield is an ordinary identifier in sloppy non-generator code.
		{"yield as a parameter default outside a generator", `(x = yield) => {};`},
		{"await as an identifier in a script", `var await = 1;`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rt := quickjs.New()
			defer rt.Close()
			if _, err := rt.Eval(tc.src); err != nil {
				t.Fatalf("%s: rejected with %v, want acceptance", tc.src, err)
			}
		})
	}
}

// import() is a syntactic form rather than a function, so its shape is fixed by
// the grammar: the arity is checked when the program is parsed, and there is
// nothing for new to construct.
func TestDynamicImportSyntax(t *testing.T) {
	bad := []string{
		`throw 0; import();`,
		`throw 0; import("a", "b", "c");`,
		`throw 0; new import("a");`,
		`throw 0; new import("a", "b");`,
		`throw 0; import(...["a"]);`,
		`throw 0; import(,);`,
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

	// The legal shapes still parse, whatever they do when run.
	for _, src := range []string{
		`typeof (() => import("a"))`,
		`typeof (() => import("a", {with: {}}))`,
		`typeof (async () => await import("a"))`,
	} {
		rt := quickjs.New()
		if v, err := rt.Eval(src); err != nil {
			t.Errorf("%s: %v", src, err)
		} else if v.String() != "function" {
			t.Errorf("%s = %q", src, v.String())
		}
		rt.Close()
	}
}

// The early errors a function's shape imposes: what a directive may say given
// the parameter list, where a rest element may sit, and where a declaration may
// be the body of another statement.
func TestFunctionShapeEarlyErrors(t *testing.T) {
	bad := []string{
		// A "use strict" directive is what a non-simple parameter list
		// forbids, whatever mode the surrounding code is in -- so a class
		// method, which is strict already, may not carry one either.
		`throw 0; function f(...a) { "use strict"; }`,
		`throw 0; function f([a]) { "use strict"; }`,
		`throw 0; function f({a}) { "use strict"; }`,
		`throw 0; function f(a = 1) { "use strict"; }`,
		`throw 0; 0, function (a, ...r) { "use strict"; };`,
		`throw 0; var f = (...a) => { "use strict"; };`,
		`throw 0; ({ m(...a) { "use strict"; } });`,
		`throw 0; class C { m(...a) { "use strict"; } }`,
		`throw 0; class C { static m(...a) { "use strict"; } }`,
		`throw 0; function* g(...a) { "use strict"; }`,
		`throw 0; async function f(...a) { "use strict"; }`,
		`"use strict"; throw 0; function f(a, ...r) { "use strict"; }`,

		// A comma after a rest element is not tolerated punctuation: there is
		// nothing it could separate.
		`throw 0; var {...a,} = {};`,
		`throw 0; var [...a,] = [];`,
		`throw 0; ({...a,} = {});`,
		`throw 0; [...a,] = [];`,
		`throw 0; var [...a, b] = [];`,

		// A function declaration is the body of an if and nothing else, and a
		// labelled one is not even that.
		`throw 0; while (false) function f() {}`,
		`throw 0; do function f() {} while (false)`,
		`throw 0; for (;false;) function f() {}`,
		`throw 0; while (false) l: function f() {}`,
		`throw 0; do label1: label2: function f() {} while (false)`,
		`throw 0; if (false) l: function f() {}`,
		`throw 0; for (;false;) l: function f() {}`,
		`"use strict"; throw 0; if (false) function f() {}`,
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

	ok := []struct{ src, want string }{
		{`function f(a) { "use strict"; return 1 } String(f(0))`, "1"},
		{`"use strict"; function f(...a) { return a.length } String(f(1))`, "1"},
		{`class C { m(a) { "use strict"; return 2 } } String(new C().m())`, "2"},
		{`var {a, ...r} = {a: 1, b: 2}; a + "," + JSON.stringify(r)`, `1,{"b":2}`},
		{`var [...a] = [1, 2]; a.join(",")`, "1,2"},
		// A trailing comma is fine where there is no rest element.
		{`var [a,] = [1]; String(a)`, "1"},
		{`var {a,} = {a: 1}; String(a)`, "1"},
		{`String([1, 2,].length)`, "2"},
		// Annex B allows a bare function declaration as an if branch.
		{`if (false) function f() {} "ok"`, "ok"},
		{`l: function f() {} "ok"`, "ok"},
		{`{ l: function f() {} } "ok"`, "ok"},
		{`l: { break l } "ok"`, "ok"},
	}
	for _, tc := range ok {
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
