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
		{`{ function g() {} } typeof g`, "function"},

		// The alias exists only where it would be legal. Strict mode does not
		// grant it, an async or generator declaration never gets one, and a
		// lexical binding of the same name anywhere in between suppresses it.
		{`(function () { "use strict"; { function f() {} } return typeof f; })()`, "undefined"},
		{`(function () { { async function f() {} } return typeof f; })()`, "undefined"},
		{`(function () { { function* f() {} } return typeof f; })()`, "undefined"},
		{`(function () { { let f = 1; { function f() {} } } return typeof f; })()`, "undefined"},
		{`{ let f = 1; { function f() {} } } typeof f`, "undefined"},
		// A binding of a different name does not.
		{`(function () { for (let f of [1]) { function g() {} } return typeof g; })()`,
			"function"},
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

// A function declared in a block at a script's top level is also assigned to
// the global var of the same name, and the copy the assignment consumes is not
// the one the block's own binding needs.
func TestBlockFunctionAliasKeepsItsBinding(t *testing.T) {
	cases := []struct{ name, src, want string }{
		{"called in the block", `{ function ref(x) { return x + 1 } var r = ref(1) } String(r)`, "2"},
		{"called after the block", `{ function ref(x) { return x + 1 } } String(ref(1))`, "2"},
		{"with a lexical before it", `let c = 5
		  { function ref(x) { return x + c } var r = ref(1) }
		  String(r) + "," + c`, "6,5"},
		{"the global sees it", `{ function ref() { return 1 } }
		  String(typeof globalThis.ref)`, "function"},
		{"several in one block", `{ function a() { return 1 } function b() { return 2 }
		  var r = a() + b() } String(r)`, "3"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) { checkEval(t, tc.src, tc.want) })
	}
}

// TestAnnexBBlockFunctionHoisting pins where B.3.3 hoists a function declared
// in a block to a var of the same name: exactly where replacing the
// declaration with `var f` would be legal and f is not a parameter.
func TestAnnexBBlockFunctionHoisting(t *testing.T) {
	evalCases(t, []struct{ src, want string }{
		// A let at the function's top level is in the way.
		{`(function () { let f = 1; { function f() {} } return f; })()`, "1"},
		{`let g1 = 1; { function g1() {} } g1`, "1"},
		// A top-level function of the same name is the var, and is updated.
		{`(function () { { function f() { return "inner"; } } var r = f(); function f() { return "outer"; } return r; })()`, "inner"},
		// A simple catch parameter is passed over; a destructured one is in
		// the way, and then no binding is made at all.
		{`(function () { try { throw 0; } catch (f) { { function f() {} } } return typeof f; })()`, "function"},
		{`(function () { try { f; } catch (e) { var r = e.name; } try { throw {}; } catch ({ f }) { { function f() {} } } return r; })()`, "ReferenceError"},
		{`try { throw 0; } catch (g2) { { function g2() {} } } typeof g2`, "function"},
		// Nothing is hoisted over a parameter.
		{`(function (f) { { function f() {} } return typeof f; })(1)`, "number"},
		// Eval code hoists into the caller's variable scope, parameters and
		// all, unless a lexical binding of the caller is in the way -- which
		// skips the hoist rather than failing the eval.
		{`(function (f) { eval("{ function f() {} }"); return typeof f; })(1)`, "function"},
		{`(function () { let f = 1; eval("{ function f() {} }"); return f; })()`, "1"},
		{`(function () { eval("try { throw 0; } catch (f) { { function f() {} } }"); return typeof f; })()`, "function"},
		{`(function () { let f = 1; try { eval("var f"); } catch (e) { return e.name; } })()`, "SyntaxError"},
		// Test262 against V8: the arguments object counts as a parameter, so
		// a block function named arguments leaves it alone. V8 replaces it,
		// as WithNodeQuirks does; see TestNodeQuirksLanguage.
		{`(function () { { function arguments() {} } return typeof arguments; })()`, "object"},
		{`(function (...a) { { function arguments() {} } return typeof arguments; })()`, "object"},
		// Test262 against V8: an enclosing block's function of the same name
		// is in the way of the inner one, which stays in its block. V8 hoists
		// the inner one, as WithNodeQuirks does.
		{`(function () { { function f() { return 1; } { function f() { return 2; } } } return f(); })()`, "1"},
		{`(function () { { function f() { return 1; } if (true) function f() { return 2; } } return f(); })()`, "1"},
	})
}

// TestNodeQuirksLanguage pins where WithNodeQuirks follows V8 rather than the
// standard in the language itself, each case with the answer standards mode
// gives beside Node's.
func TestNodeQuirksLanguage(t *testing.T) {
	cases := []struct{ src, standard, node string }{
		// Strict code may assign to a call: the call is made, and then the
		// assignment is a ReferenceError.
		{`var r = []; function f() { r.push("f"); return {}; }
		try { eval("'use strict'; f() = (r.push('rhs'), 1)"); } catch (e) { r.push(e.name); }
		r.join()`, "SyntaxError", "f,ReferenceError"},
		{`var r = []; function f() { r.push("f"); return {}; }
		try { eval("'use strict'; for (f() of [1]) ;"); } catch (e) { r.push(e.name); }
		r.join()`, "SyntaxError", "f,ReferenceError"},
		{`try { new Function("'use strict'; if (false) f()++;"); "parsed" } catch (e) { e.name }`, "SyntaxError", "parsed"},
		// Neither mode lets a logical assignment or a pattern hold a call.
		{`try { new Function("'use strict'; f() &&= 1"); "parsed" } catch (e) { e.name }`, "SyntaxError", "SyntaxError"},
		{`try { new Function("[f()] = []"); "parsed" } catch (e) { e.name }`, "SyntaxError", "SyntaxError"},
		// A block-level function named arguments replaces the arguments object.
		{`(function (x) { var a = typeof arguments; { function arguments() {} } return a + "," + typeof arguments; })(1)`,
			"object,object", "object,function"},
		{`(function (...x) { { function arguments() {} } return typeof arguments; })()`, "object", "function"},
		// A parameter of the name still stops the hoist.
		{`(function (arguments) { { function arguments() {} } return typeof arguments; })(1)`, "number", "number"},
		// A function nested in a block that already declares one of its name
		// is hoisted too, as deep as it is.
		{`(function () { { function h() { return 1; } { function h() { return 2; } } var x = h(); } return x + "," + h(); })()`,
			"1,1", "1,2"},
		{`(function () { { function h() { return 1; } { { function h() { return 3; } } } } return h(); })()`, "1", "3"},
		{`{ function g1() { return 1; } { function g1() { return 2; } } } g1()`, "1", "2"},
		// Anything else in the enclosing block still stops it.
		{`(function () { { let h = 1; { function h() {} } } return typeof h; })()`, "undefined", "undefined"},
		{`(function () { { async function h() {} { function h() {} } } return typeof h; })()`, "undefined", "undefined"},
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
			for _, tc := range cases {
				rt := quickjs.New(mode.opts...)
				v, err := rt.Eval(tc.src)
				want := mode.want(tc.standard, tc.node)
				if err != nil {
					t.Errorf("%s: %v", tc.src, err)
				} else if got := v.String(); got != want {
					t.Errorf("%s = %q, want %q", tc.src, got, want)
				}
				rt.Close()
			}
		})
	}

	// A module is strict code, and takes the quirk too.
	for _, quirks := range []bool{false, true} {
		var opts []quickjs.Option
		if quirks {
			opts = append(opts, quickjs.WithNodeQuirks())
		}
		rt := quickjs.New(opts...)
		_, err := rt.EvalModule("m.js", `function f() {} if (false) f() = 1;`)
		if (err == nil) != quirks {
			t.Errorf("module assigning to a call with quirks=%v: %v", quirks, err)
		}
		rt.Close()
	}
}

// TestAnnexBBuiltins pins the web-compat built-ins Annex B describes.
func TestAnnexBBuiltins(t *testing.T) {
	evalCases(t, []struct{ src, want string }{
		// A class escape as a range's upper end makes the range three
		// alternatives: the lower end, the hyphen, and the class.
		{`var re = /[a-\s]/; [re.test("a"), re.test("-"), re.test(" "), re.test("b")].join()`, "true,true,true,false"},
		// substr clamps an infinite length rather than overflowing it.
		{`JSON.stringify(["abc".substr(1, Infinity), "abc".substr(-Infinity, 2), "abc".substr(1, -Infinity)])`, `["bc","ab",""]`},
		// setYear truncates before asking whether the year is two digits.
		{`var d = new Date(2000, 0, 1), e = new Date(2000, 0, 1); d.setYear(99.7); e.setYear(-0.5); [d.getFullYear(), e.getFullYear()].join()`, "1999,1900"},
		{`Date.prototype.toGMTString === Date.prototype.toUTCString`, "true"},
	})
}

// TestAnnexBHTMLComments pins the comments scripts keep from pages that hid
// them from old browsers: <!-- anywhere, and --> where only whitespace and
// comments precede it on its line.
func TestAnnexBHTMLComments(t *testing.T) {
	evalCases(t, []struct{ src, want string }{
		{"var x = 1; <!-- x = 2\n x", "1"},
		{"var y = 1;\n--> y = 2\ny", "1"},
		{"/* c */ --> z\n 3", "3"},
		{"var a = 1; /* one\n two */ --> a = 2\na", "1"},
		// Elsewhere --> is a decrement and a comparison.
		{"var w = 5; w-->0; w", "4"},
	})
	// A module has neither.
	rt := quickjs.New()
	defer rt.Close()
	if _, err := rt.EvalModule("m.js", "<!-- x\n"); err == nil {
		t.Fatal("<!-- in a module: no error")
	}
}

// TestAnnexBForInInitializer pins that sloppy code may give a for-in's var an
// initializer, assigned once before the object is evaluated, and that nothing
// else may.
func TestAnnexBForInInitializer(t *testing.T) {
	evalCases(t, []struct{ src, want string }{
		{`var log = []; for (var a = (log.push("init"), 0) in (log.push("obj"), {k: 1})) log.push(a); log.join() + " " + a`, "init,obj,k k"},
		{`for (var b = 7 in {}) ; b`, "7"},
		{`var errs = [];
		for (var src of ["for (a = 0 in {}) ;", "'use strict'; for (var a = 0 in {}) ;", "for (let a = 0 in {}) ;",
			"for (var [a] = 0 in {}) ;", "for (var a = 0 of []) ;"]) {
			try { new Function(src); errs.push("ok") } catch (e) { errs.push(e.constructor.name) }
		}
		errs.join()`, "SyntaxError,SyntaxError,SyntaxError,SyntaxError,SyntaxError"},
	})
}

// TestAnnexBCallAssignmentTarget pins that sloppy code may assign to a call:
// the call is made, and then the assignment throws a ReferenceError without
// evaluating the value. Strict code, logical assignment and super() and
// import() calls keep the SyntaxError.
func TestAnnexBCallAssignmentTarget(t *testing.T) {
	evalCases(t, []struct{ src, want string }{
		{`var r, out = [];
		function f() { r.push("f"); return {}; }
		for (var src of ["f() = (r.push('rhs'), 1)", "f() += 1", "f()++", "--f()",
			"for (f() in {x: 1}) ;", "for (f() of [1]) ;", "for (f() in {}) ;", "[f() = 1]"]) {
			r = [];
			try { eval(src); r.push("ok") } catch (e) { r.push(e.constructor.name) }
			out.push(r.join());
		}
		out.join(" ")`, "f,ReferenceError f,ReferenceError f,ReferenceError f,ReferenceError f,ReferenceError f,ReferenceError ok f,ReferenceError"},
		{`var errs = [];
		for (var src of ["f() &&= 1", "f() ??= 1", "'use strict'; f() = 1", "'use strict'; f()++",
			"'use strict'; for (f() in {}) ;", "[f()] = []", "({a: f()} = {})", "import('x') = 1"]) {
			try { new Function(src); errs.push("ok") } catch (e) { errs.push(e.constructor.name) }
		}
		errs.join()`, "SyntaxError,SyntaxError,SyntaxError,SyntaxError,SyntaxError,SyntaxError,SyntaxError,SyntaxError"},
	})
}
