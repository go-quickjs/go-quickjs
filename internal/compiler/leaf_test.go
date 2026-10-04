package compiler

import (
	"testing"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
	"github.com/go-quickjs/go-quickjs/internal/parser"
)

// TestArrowLeafKinds pins which arrows are marked pure: one whose body only
// reads and computes, and never reads this or super -- whose this is the
// enclosing function's, not the call's.
func TestArrowLeafKinds(t *testing.T) {
	cases := []struct {
		src  string
		want bytecode.LeafKind
	}{
		{`(x, y) => x - y`, bytecode.LeafPure},
		{`o => o.id === 7`, bytecode.LeafPure},
		{`x => x * k`, bytecode.LeafPure},
		{`(x, y) => this.k * (x - y)`, bytecode.LeafNone},
		{`x => (() => this)()`, bytecode.LeafNone},
		// An arrow's arguments is the enclosing function's, made there and
		// read here as an upvalue.
		{`x => arguments.length`, bytecode.LeafPure},
		{`x => { var y = x; return y }`, bytecode.LeafNone},
		{`(...xs) => xs.length`, bytecode.LeafNone},
		{`({ a }) => a`, bytecode.LeafNone},
	}
	for _, tc := range cases {
		prog, err := parser.Parse("var k = 2; function outer() { return "+tc.src+" }", parser.Options{})
		if err != nil {
			t.Fatalf("%s: %v", tc.src, err)
		}
		main, err := Compile(prog, Options{})
		if err != nil {
			t.Fatalf("%s: %v", tc.src, err)
		}
		arrow := findArrow(main)
		if arrow == nil {
			t.Fatalf("%s: no arrow compiled", tc.src)
		}
		if arrow.Leaf != tc.want {
			t.Errorf("%s: leaf %v, want %v", tc.src, arrow.Leaf, tc.want)
		}
	}
}

// findArrow is the first arrow among fn's nested functions.
func findArrow(fn *bytecode.Function) *bytecode.Function {
	for _, c := range fn.Constants {
		if c.Fn == nil {
			continue
		}
		if c.Fn.Kind == bytecode.KindArrow {
			return c.Fn
		}
		if a := findArrow(c.Fn); a != nil {
			return a
		}
	}
	return nil
}

// TestLiteralStatementsEmitNothing pins that a statement that is only a
// literal -- a directive such as "use strict" among them -- compiles to
// nothing, rather than a push and a drop, while the directive still makes
// the function strict.
func TestLiteralStatementsEmitNothing(t *testing.T) {
	prog, err := parser.Parse(`function f() { "use strict"; 1; 2n; true; null; "x"; return 3 }`, parser.Options{})
	if err != nil {
		t.Fatal(err)
	}
	main, err := Compile(prog, Options{})
	if err != nil {
		t.Fatal(err)
	}
	var f *bytecode.Function
	for _, c := range main.Constants {
		if c.Fn != nil {
			f = c.Fn
		}
	}
	if f == nil {
		t.Fatal("no function compiled")
	}
	if !f.Strict {
		t.Error("the directive no longer makes the function strict")
	}
	for _, in := range f.Code {
		if in.Op == bytecode.OpDrop {
			t.Errorf("a literal statement left a drop: %v", f.Code)
			break
		}
	}
}
