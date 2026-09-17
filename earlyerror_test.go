package quickjs_test

import (
	"encoding/json"
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

// A name written with a unicode escape is not a keyword, and not a contextual
// keyword either. It is one rule: \u0069f is an identifier, and so
// \u0067et is a property name rather than the start of an accessor.
func TestEscapedKeywordsAreNames(t *testing.T) {
	bad := []string{
		`throw 0; ({ \u0067et m() {} });`,
		`throw 0; ({ \u0073et m(v) {} });`,
		`throw 0; ({ \u0061sync m() {} });`,
		`throw 0; class C { \u0073tatic m() {} }`,
		`throw 0; for (var x \u006ff [1]) {}`,
		`throw 0; \u0076ar x = 1;`,
		`throw 0; function* g() { \u0079ield 1; }`,
		`throw 0; \u0066unction f() {}`,
		`throw 0; \u0063lass C {}`,
		// A reserved word written with an escape is an identifier as far as
		// the lexer is concerned, but the restriction is on the name rather
		// than on how it was spelled: it may not be referred to, bound or
		// used as a label.
		`throw 0; var x = fals\u0065;`,
		`throw 0; var x = tru\u0065;`,
		`throw 0; var x = nul\u006C;`,
		`throw 0; function f() { n\u0065w.target }`,
		`throw 0; fals\u0065: 1;`,
		`throw 0; \u0069mport("m");`,
		// Nor is it the contextual keyword it spells.
		`throw 0; \u0061sync function f() {}`,
		`throw 0; \u006Cet x = 1;`,
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

	// Escaped or not, a name is a name.
	cases := []struct{ src, want string }{
		{`var get = 1; String(get)`, "1"},
		{`var o = {get: 1}; String(o.get)`, "1"},
		{`var async = 2; String(async)`, "2"},
		// And the unescaped contextual keywords still work.
		{`({get x() { return 1 }}).x + ""`, "1"},
		{`class C { static m() { return 1 } } String(C.m())`, "1"},
		{`for (var x of [1]) {} String(x)`, "1"},
		// A reserved word is still legal as a property name, spelled either
		// way, because a property name is not an identifier reference.
		{`var o = {}; o.\u0069f = 7; String(o.if)`, "7"},
		{`var o = {\u0074rue: 1}; String(o.true)`, "1"},
		// And the unescaped contextual keywords still lead declarations.
		{`var \u0061sync = 3; async function f() { return 1 }; async + typeof f`,
			"3function"},
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

// Assigning to a const is a runtime error rather than an early one: the
// assignment may sit in a function that is never called, and a program that
// never calls it is perfectly good.
func TestAssignToConst(t *testing.T) {
	cases := []struct{ src, want string }{
		{`const c = 1; try { c = 2 } catch (e) { e.constructor.name }`, "TypeError"},
		{`const c = 1; try { c++ } catch (e) { e.constructor.name }`, "TypeError"},
		{`const c = 1; try { c += 1 } catch (e) { e.constructor.name }`, "TypeError"},
		{`const c = 1; try { [c] = [2] } catch (e) { e.constructor.name }`, "TypeError"},
		{`const c = 1; try { ({a: c} = {a: 2}) } catch (e) { e.constructor.name }`, "TypeError"},
		{`const c = 1; try { for ({a: c} of [{a: 2}]) {} } catch (e) { e.constructor.name }`,
			"TypeError"},
		// The loop body never runs, because the assignment comes first.
		{`const c = 1; var n = 0;
		  try { for ({a: c} of [{a: 2}]) { n++ } } catch (e) {} String(n)`, "0"},
		// A function that assigns to a const parses; it throws when called.
		{`const c = 1; function f() { c = 2 } "parsed"`, "parsed"},
		{`const c = 1; function f() { c = 2 }
		  try { f() } catch (e) { e.constructor.name }`, "TypeError"},
		{`"use strict"; const c = 1; try { c = 2 } catch (e) { e.constructor.name }`,
			"TypeError"},

		{`const c = 1; String(c)`, "1"},
		{`let l = 1; l = 2; String(l)`, "2"},
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

// A plain function declaration in a block gets Annex B's var-like behaviour, so
// a var of the same name beside it is legal. An async function or a generator
// gets no such allowance: they were introduced after the mistake was
// recognized, so there is nothing to be compatible with.
func TestBlockFunctionDeclarationKinds(t *testing.T) {
	bad := []string{
		`throw 0; { var f; async function f() {} }`,
		`throw 0; { var f; function* f() {} }`,
		`throw 0; { var f; async function* f() {} }`,
		`throw 0; { async function f() {} var f; }`,
		`throw 0; { let f; async function f() {} }`,
		`throw 0; switch (0) { case 0: var f; async function f() {} }`,
		// Annex B's allowance is about hoisting the binding out to the
		// enclosing function, not about the clash: a var beside the
		// declaration, or anywhere inside the same block, is still an error.
		`throw 0; { var f; function f() {} }`,
		`throw 0; { function f() {} var f; }`,
		`throw 0; { function f() {} { var f; } }`,
		`throw 0; function g() { { function f() {} { var f; } } }`,
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

	for _, src := range []string{
		`{ async function f() {} } "ok"`,
		// A var outside the block is fine: that is exactly what the hoisting
		// makes the declaration into.
		`var f; { function f() {} } "ok"`,
		`function g() { var f; { function f() {} } } "ok"`,
		// Two plain declarations in one block name the same binding rather
		// than colliding, which is the part Annex B does relax.
		`{ function f() {} function f() {} } "ok"`,
		// And the hoisting still happens.
		`(function () { { function f() { return 1 } } return f() })()`,
		// At the top level of a script or a function body they are var-scoped
		// like a plain declaration.
		`var f; async function f() {} "ok"`,
		`async function f() {} var f; "ok"`,
		`function g() { var f; function* f() {} } "ok"`,
		`{ let a; async function b() {} } "ok"`,
	} {
		rt := quickjs.New()
		if _, err := rt.Eval(src); err != nil {
			t.Errorf("%s: rejected with %v", src, err)
		}
		rt.Close()
	}
}

// `await` inside an async function and `yield` inside a generator are operators
// and never names, whatever they are spelled with. Writing one with an escape
// does not turn it back into an identifier -- the escape only stops it being a
// keyword, and these are reserved by their position rather than by being
// keywords.
func TestAwaitAndYieldAreReservedWhereTheyOperate(t *testing.T) {
	esc := func(w string) string {
		return `\u00` + map[byte]string{'a': "61", 'y': "79"}[w[0]] + w[1:]
	}
	bad := []string{
		`throw 0; async function f() { ` + esc("await") + ` }`,
		`throw 0; async function f() { var x = ` + esc("await") + ` }`,
		`throw 0; async () => ` + esc("await"),
		`throw 0; function* g() { ` + esc("yield") + ` }`,
		`throw 0; function* g() { var x = ` + esc("yield") + ` }`,
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

	// Outside those positions they are ordinary names.
	for _, src := range []string{
		`var await = 1; String(await)`,
		`function f() { var yield = 1; return yield } String(f())`,
		`var o = {await: 1, yield: 2}; String(o.await + o.yield)`,
	} {
		rt := quickjs.New()
		if _, err := rt.Eval(src); err != nil {
			t.Errorf("%s: rejected with %v", src, err)
		}
		rt.Close()
	}
}

// Only a bare object or array literal on the left of `=` is a destructuring
// pattern. Parenthesize one and it is an expression again, and an expression
// that is not a reference cannot be assigned to.
func TestAssignmentTargetMustBeAReference(t *testing.T) {
	bad := []string{
		`throw 0; var a, b, c; (a = b) = c;`,
		`throw 0; var a, b, c; ((a = b)) = c;`,
		`throw 0; ({}) = 1;`,
		`throw 0; ([]) = 1;`,
		`throw 0; (function () {}) = 1;`,
		`throw 0; 1 = 1;`,
		`throw 0; (a, b) = 1;`,
		`throw 0; () => ({}) = 1;`,
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

	// The forms that are patterns, and the parenthesized reference that is
	// still a reference.
	cases := []struct{ src, want string }{
		{`var a; [a] = [1]; String(a)`, "1"},
		{`var a; ({a} = {a: 5}); String(a)`, "5"},
		{`var a; [a = 2] = []; String(a)`, "2"},
		{`var o = {}; [o.x] = [3]; String(o.x)`, "3"},
		{`var a; (a) = 7; String(a)`, "7"},
		{`var o = {}; (o.x) = 8; String(o.x)`, "8"},
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

// jsQuote renders a Go string as a JavaScript string literal.
func jsQuote(s string) string {
	q, err := json.Marshal(s)
	if err != nil {
		panic(err)
	}
	return string(q)
}

// TestNoSuspendInParameters covers yield and await in a parameter list. The
// parameters are bound as part of the call, before the body can suspend, so
// neither is allowed there even in a generator or an async function.
func TestNoSuspendInParameters(t *testing.T) {
	bad := []string{
		`function* g(a = yield 1) {}`,
		`function* g(a = yield) {}`,
		`async function f(a = await 1) {}`,
		`async function* g(a = yield 1) {}`,
		`async function* g(a = await 1) {}`,
		`class C { *m(a = yield 1) {} }`,
		`class C { async m(a = await 1) {} }`,
		`var o = {*m(a = yield 1) {}}`,
	}
	for _, src := range bad {
		checkEval(t, `try { eval(`+jsQuote(src)+`); "no throw" }
		              catch (e) { e.constructor.name }`, "SyntaxError")
	}

	// A function written inside a default has its own rules again.
	good := []string{
		`function* g(a = function* () { yield 1 }) {}`,
		`async function f(a = async () => await 1) {}`,
		`async function f(a = async function () { await 1 }) {}`,
		`function* g(a = 1) { yield a }`,
		`async function f(a = 1) { await a }`,
	}
	for _, src := range good {
		checkEval(t, `try { eval(`+jsQuote(src)+`); "ok" }
		              catch (e) { e.constructor.name + ": " + e.message }`, "ok")
	}
}

// TestPrivateNameNeedsAClass covers a private name written as a key outside a
// class body, where there is no class evaluation to hold it.
func TestPrivateNameNeedsAClass(t *testing.T) {
	bad := []string{
		`var o = {#m() {}}`,
		`var o = {* #m() {}}`,
		`var o = {async #m() {}}`,
		`var o = {get #m() {}}`,
		`var o = {set #m(v) {}}`,
		`var o = {#m: 1}`,
		`class C { m() { var o = {#x() {}} } }`,
		`function f() { this.#x }`,
	}
	for _, src := range bad {
		checkEval(t, `try { eval(`+jsQuote(src)+`); "no throw" }
		              catch (e) { e.constructor.name }`, "SyntaxError")
	}

	good := []string{
		`class C { #m() {} n() { return this.#m } }`,
		`class C { * #m() {} }`,
		`class C { get #m() { return 1 } set #m(v) {} }`,
		`class C { #x = 1; static read(o) { return o.#x } }`,
		`var o = {m() {}}`,
	}
	for _, src := range good {
		checkEval(t, `try { eval(`+jsQuote(src)+`); "ok" }
		              catch (e) { e.constructor.name + ": " + e.message }`, "ok")
	}
}

// TestLegacySyntaxUnderStrict covers the two forms that predate strict mode:
// the octal escapes and literals, and the numeric separator's one placement
// that never meant anything.
func TestLegacySyntaxUnderStrict(t *testing.T) {
	// A leading zero is a literal all by itself, so there is nothing for a
	// separator to sit between.
	bad := []string{`0_0`, `0_1`, `0_7`, `0_8`, `0_9`, `0_0n`}
	for _, src := range bad {
		checkEval(t, `try { eval(`+jsQuote(src)+`); "no throw" }
		              catch (e) { e.constructor.name }`, "SyntaxError")
	}
	for _, src := range []string{`1_000`, `0.5_1`, `0x1_0`, `0b1_0`, `0o1_0`, `1_0n`} {
		checkEval(t, `try { eval(`+jsQuote(src)+`); "ok" }
		              catch (e) { e.constructor.name }`, "ok")
	}
	checkEval(t, `String(1_000) + "," + String(0.5_1) + "," + String(0x1_0)`, "1000,0.51,16")

	// An octal escape, or \8 and \9, where strict mode applies. The rule is
	// about the body as a whole, so a directive carrying one is an error even
	// when it comes before the "use strict".
	strictBad := []string{
		`"use strict"; "\1"`,
		`"use strict"; "\08"`,
		`"use strict"; "\8"`,
		`"use strict"; "\9"`,
		`"\1"; "use strict"`,
		`"use strict"; var x = "\1"`,
		`function f() { "use strict"; return "\1" }`,
		`"use strict"; ({"\1": 1})`,
	}
	for _, src := range strictBad {
		checkEval(t, `try { eval(`+jsQuote(src)+`); "no throw" }
		              catch (e) { e.constructor.name }`, "SyntaxError")
	}

	// Sloppy mode keeps them, and \0 on its own is NUL in either.
	checkEval(t, `eval('"\\1"').charCodeAt(0)`, "1")
	checkEval(t, `eval('"\\8"')`, "8")
	checkEval(t, `eval('"use strict"; "\\0"').charCodeAt(0)`, "0")
	checkEval(t, `eval('"use strict"; "ok"')`, "ok")
}

// TestPrivateAccessorPairIsOneMember covers the one legal repeat of a private
// name: a getter paired with a setter. The two halves are one member, so they
// have to agree about being on the instances or on the class.
func TestPrivateAccessorPairIsOneMember(t *testing.T) {
	bad := []string{
		`class C { get #x() {} static set #x(v) {} }`,
		`class C { static get #x() {} set #x(v) {} }`,
		`class C { set #x(v) {} static get #x() {} }`,
		`class C { #x() {} get #x() {} }`,
		`class C { get #x() {} get #x() {} }`,
		`class C { #x = 1; #x() {} }`,
	}
	for _, src := range bad {
		checkEval(t, `try { eval(`+jsQuote(src)+`); "no throw" }
		              catch (e) { e.constructor.name }`, "SyntaxError")
	}
	good := []string{
		`class C { get #x() { return 1 } set #x(v) {} }`,
		`class C { static get #x() { return 1 } static set #x(v) {} }`,
		`class C { set #x(v) {} get #x() { return 1 } }`,
		`class C { #x = 1; #y() {} }`,
	}
	for _, src := range good {
		checkEval(t, `try { eval(`+jsQuote(src)+`); "ok" } catch (e) { e.constructor.name }`,
			"ok")
	}
}

// A class field initializer and a static block reserve `await` in their own
// statements, but the rule stops at a function written inside one -- an arrow
// included, since an arrow's body is not an await context either.
func TestAwaitBindingInsideAClassBody(t *testing.T) {
	cases := []struct{ name, src, want string }{
		{"arrow in a static block",
			`class C { static { (() => { try {} catch (await) {} }) } }; "ok"`, "ok"},
		{"function in a static block",
			`class C { static { (function (await) {}) } }; "ok"`, "ok"},
		{"var in an arrow in a static block",
			`class C { static { (() => { var await = 1; return await }) } }; "ok"`, "ok"},
		{"arrow in a field initializer",
			`class C { x = () => { let await = 1; return await } }; String(new C().x())`, "1"},
		// In the block's own statements it is still reserved.
		{"var in a static block",
			`try { eval("class C { static { var await = 1 } }"); "no error" }
			 catch (e) { e.constructor.name }`, "SyntaxError"},
		{"catch in a static block",
			`try { eval("class C { static { try {} catch (await) {} } }"); "no error" }
			 catch (e) { e.constructor.name }`, "SyntaxError"},
		{"label in a static block",
			`try { eval("class C { static { await: 1 } }"); "no error" }
			 catch (e) { e.constructor.name }`, "SyntaxError"},
		{"reference in a static block",
			`try { eval("class C { static { await } }"); "no error" }
			 catch (e) { e.constructor.name }`, "SyntaxError"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) { checkEval(t, tc.src, tc.want) })
	}
}

// An iterator's next method is read when the iterator is opened and required to
// be callable only when it is called: a pattern abandoned before it steps the
// iterator closes it without ever needing one.
func TestIteratorNextIsCheckedWhenCalled(t *testing.T) {
	cases := []struct{ name, src, want string }{
		{"closed before stepping", `
		  var iterable = {}
		  var iterator = {return: function () { return null }}
		  iterable[Symbol.iterator] = function () { return iterator }
		  function* g() { for ([...{}[yield]] of [iterable]) {} }
		  var it = g(); it.next()
		  try { it.return(); "no error" } catch (e) { e.constructor.name }`, "TypeError"},
		{"reported when stepped", `
		  var iterable = {}
		  iterable[Symbol.iterator] = function () { return {next: 1} }
		  try { for (var x of iterable) {} ; "no error" }
		  catch (e) { e.constructor.name }`, "TypeError"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) { checkEval(t, tc.src, tc.want) })
	}
}
