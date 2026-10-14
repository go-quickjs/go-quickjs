package compiler

import (
	"testing"

	"github.com/go-quickjs/go-quickjs/internal/parser"
)

// TestReturnsReceiver pins which functions return only their this
// (bytecode.Function's ReturnsReceiver): a base class's constructor, with
// its own return of this, or one returning this itself; a function with no
// return statement. Not one that returns anything else, nor nothing --
// return; makes undefined, which a construction would have to replace --
// though a function nested in it may.
func TestReturnsReceiver(t *testing.T) {
	cases := []struct {
		src  string
		want bool
	}{
		{`class C { constructor(x) { this.x = x } }`, true},
		{`class C { constructor(x) { if (x) return this; this.x = x } }`, true},
		{`class C { constructor(x) { if (x) return; this.x = x } }`, false},
		{`class C { constructor(x) { if (x) return {}; this.x = x } }`, false},
		{`class C { constructor(x) { this.f = () => { return 1 }; function g() { return 2 } } }`, true},
		{`function C(x) { this.x = x }`, true},
		{`function C(x) { return x }`, false},
		{`class B {} class C extends B { constructor() { super() } }`, true},
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
		fn := findNamed(main, "C")
		if fn == nil {
			t.Fatalf("%s: no C compiled", tc.src)
		}
		if fn.ReturnsReceiver != tc.want {
			t.Errorf("%s: ReturnsReceiver %v, want %v", tc.src, fn.ReturnsReceiver, tc.want)
		}
	}
}
