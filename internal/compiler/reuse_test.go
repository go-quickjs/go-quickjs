package compiler

import (
	"testing"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
	"github.com/go-quickjs/go-quickjs/internal/parser"
)

// A var's function expression in a loop is made once per invocation of the
// function -- the var read, and the closure made only while it holds none
// -- exactly where nothing could tell one iteration's function from
// another's (reusedClosures): strict code with no eval, an ordinary
// function only ever called, by the var and by its own name, the var
// declared nowhere else, and nothing captured that an iteration makes anew.
func TestReusedClosures(t *testing.T) {
	cases := []struct {
		src  string
		want bool
	}{
		// PDF.js's getToken: a helper of the loop, called, and calling itself.
		{`function f(s) { "use strict"; var t = 0; for (var i = 0; i < s.length; i++) { var get = function get() { return i < 0 ? get() : s[i] }; t += get().length } return t }`, true},
		{`function f(n) { "use strict"; var t = 0; while (n--) { var g = function () { return n }; t += g() + g() } return t }`, true},
		{`function f(n) { "use strict"; for (var i = 0; i < n; i++) { var g = function () { return i }; (function () { g() })() } }`, true},
		// Not strict: a callee's caller, and a stack's getFunction, see it.
		{`function f(n) { for (var i = 0; i < n; i++) { var g = function () { return i }; g() } }`, false},
		// Read as a value.
		{`function f(n, a) { "use strict"; for (var i = 0; i < n; i++) { var g = function () { return i }; a.push(g) } }`, false},
		{`function f(n, p) { "use strict"; for (var i = 0; i < n; i++) { var g = function () {}; p = p === g } return p }`, false},
		{`function f(n) { "use strict"; for (var i = 0; i < n; i++) { var g = function () {}; g.x = i } }`, false},
		{`function f(n) { "use strict"; for (var i = 0; i < n; i++) { var g = function () {}; new g() } }`, false},
		{`function f(n) { "use strict"; for (var i = 0; i < n; i++) { var g = function () {}; if (typeof g) g() } }`, false},
		{`function f(n) { "use strict"; for (var i = 0; i < n; i++) { var g = function () {}; g?.() } }`, false},
		{`function f(n) { "use strict"; for (var i = 0; i < n; i++) { var g = function () {}; g.call(null) } }`, false},
		{`function f(n) { "use strict"; for (var i = 0; i < n; i++) { var g = function h() { return h }; g() } }`, false},
		// Assigned or declared elsewhere.
		{`function f(n) { "use strict"; for (var i = 0; i < n; i++) { var g = function () {}; g() } g = null }`, false},
		{`function f(g) { "use strict"; for (var i = 0; i < 3; i++) { var g = function () {}; g() } }`, false},
		{`function f(n) { "use strict"; for (var i = 0; i < n; i++) { var g = function () {}; g() } var g = 1 }`, false},
		{`function f(n) { "use strict"; for (var i = 0; i < n; i++) { var g = function () {}; g() } function k() { var g = 2; return g } }`, false},
		// Capturing what an iteration makes anew.
		{`function f(n) { "use strict"; for (let i = 0; i < n; i++) { var g = function () { return i }; g() } }`, false},
		{`function f(n) { "use strict"; for (var i = 0; i < n; i++) { const k = i; var g = function () { return k }; g() } }`, false},
		{`function f(n) { "use strict"; for (var i = 0; i < n; i++) { try { throw i } catch (e) { var g = function () { return e }; g() } } }`, false},
		// eval, an arrow, and no loop.
		{`function f(n) { "use strict"; for (var i = 0; i < n; i++) { var g = function () { return eval("i") }; g() } }`, false},
		{`function f(n) { "use strict"; for (var i = 0; i < n; i++) { var g = () => i; g() } }`, false},
		{`function f(n) { "use strict"; var g = function () { return n }; return g() }`, false},
	}
	for _, tc := range cases {
		prog, err := parser.Parse(tc.src, parser.Options{})
		if err != nil {
			t.Fatalf("%s: %v", tc.src, err)
		}
		main, err := Compile(prog, Options{})
		if err != nil {
			t.Fatalf("%s: %v", tc.src, err)
		}
		f := findNamed(main, "f")
		if f == nil {
			t.Fatalf("%s: no function f", tc.src)
		}
		if got := reusesClosure(f); got != tc.want {
			t.Errorf("%s: made once %v, want %v", tc.src, got, tc.want)
		}
		if debug, err := Compile(prog, Options{Debug: true}); err != nil {
			t.Fatal(err)
		} else if reusesClosure(findNamed(debug, "f")) {
			t.Errorf("%s: made once in code compiled for a debugger", tc.src)
		}
	}
}

// reusesClosure reports whether fn makes a closure only while a local holds
// none: get_local, jump_if_true past the closure and its store.
func reusesClosure(fn *bytecode.Function) bool {
	for pc := 0; pc+3 < len(fn.Code); pc++ {
		c := fn.Code
		if c[pc].Op == bytecode.OpGetLocal && c[pc+1].Op == bytecode.OpJumpIfTrue && c[pc+2].Op == bytecode.OpClosure && int(c[pc+1].A) == pc+4 {
			return true
		}
	}
	return false
}
