package compiler

import (
	"testing"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
	"github.com/go-quickjs/go-quickjs/internal/parser"
)

// TestHasTailCall pins which functions are marked as having a call in tail
// position: a strict function's call that its return gives back, as a
// plain call, a method call or a direct eval, and in either arm of a
// conditional -- and not a sloppy function's, a call whose result is used,
// or a call in a nested function only.
func TestHasTailCall(t *testing.T) {
	cases := []struct {
		src  string
		want bool
	}{
		{`function f(n) { "use strict"; return g(n) }`, true},
		{`function f(o) { "use strict"; return o.m() }`, true},
		{`function f(n) { "use strict"; return n ? g(n) : 0 }`, true},
		{`function f(s) { "use strict"; return eval(s) }`, true},
		{`function f(n) { return g(n) }`, false},
		{`function f(n) { "use strict"; return 1 + g(n) }`, false},
		{`function f(n) { "use strict"; g(n) }`, false},
		{`function f(n) { "use strict"; return function () { return g(n) } }`, false},
	}
	for _, tc := range cases {
		prog, err := parser.Parse("var g; "+tc.src, parser.Options{})
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
		if f.HasTailCall != tc.want {
			t.Errorf("%s: HasTailCall = %v, want %v", tc.src, f.HasTailCall, tc.want)
		}
	}
}

// findNamed finds the function of the given name compiled within fn.
func findNamed(fn *bytecode.Function, name string) *bytecode.Function {
	for _, c := range fn.Constants {
		if c.Fn == nil {
			continue
		}
		if c.Fn.Name == name {
			return c.Fn
		}
		if f := findNamed(c.Fn, name); f != nil {
			return f
		}
	}
	return nil
}
