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
		if leaf := regLeaf(arrow); leaf != tc.want {
			t.Errorf("%s: leaf %v, want %v", tc.src, leaf, tc.want)
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

// TestDirectiveAmongLiteralStatements pins that a directive among
// statements that are only literals still makes the function strict.
func TestDirectiveAmongLiteralStatements(t *testing.T) {
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
}

// TestArrowReadingAnInitializedConst pins that an arrow made after the const
// or let it reads has been initialized reads it without a dead-zone check,
// and so may be pure, while one made before keeps the check.
func TestArrowReadingAnInitializedConst(t *testing.T) {
	cases := []struct {
		src  string
		want bytecode.LeafKind
	}{
		{`function outer() { const k = 2; return x => x * k }`, bytecode.LeafPure},
		{`function outer() { let k = 2; { return x => x * k } }`, bytecode.LeafPure},
		{`function outer() { var f = x => x * k; const k = 2; return f }`, bytecode.LeafNone},
		{`function outer() { return h; function h() {} const k = 2; var f = () => k }`, bytecode.LeafPure},
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
		arrow := findArrow(main)
		if arrow == nil {
			t.Fatalf("%s: no arrow compiled", tc.src)
		}
		if leaf := regLeaf(arrow); leaf != tc.want {
			t.Errorf("%s: leaf %v, want %v", tc.src, leaf, tc.want)
		}
	}
}

// regLeaf is the leaf fn's register code is, LeafNone where it has none.
func regLeaf(fn *bytecode.Function) bytecode.LeafKind {
	if rc := fn.RegCode(); rc != nil {
		return rc.Leaf
	}
	return bytecode.LeafNone
}
