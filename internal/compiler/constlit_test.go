package compiler

import (
	"testing"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
	"github.com/go-quickjs/go-quickjs/internal/parser"
)

// A read of a const initialized with a literal is the literal where the
// binding is out of its dead zone wherever the read can run (constLiteral):
// in its own function after the declaration, and in a function made after
// it, which then captures nothing for it. A function made before -- an
// arrow above the declaration, a hoisted declaration -- a switch's case
// block's binding, a let, an initializer that is not a literal, a script's
// top-level const and code compiled for a debugger keep the read.
func TestConstLiteralReads(t *testing.T) {
	cases := []struct {
		src  string
		fn   string // the function whose reads of X are looked at
		want bool   // whether they are the literal
	}{
		{`function f() { const X = null; const g = () => X; return g }`, "g", true},
		{`function f() { const X = 4; const g = function () { return () => X + 1 }; return g }`, "g", true},
		{`function f() { const X = 'go'; class C { m() { return X } } return C }`, "m", true},
		{`function f() { const X = true; for (;;) { const g = () => X; return g } }`, "g", true},
		{`function f() { const X = 1.5; return X + X }`, "f", true},
		{`function f() { const g = () => X; const X = 1; return g }`, "g", false},
		{`function f() { const X = 1; function g() { return X } return g }`, "g", false},
		{`function f(k) { switch (k) { case 0: const X = 1; case 1: const g = () => X; return g } }`, "g", false},
		{`function f() { let X = 1; const g = () => X; return g }`, "g", false},
		{`function f() { const X = {}; const g = () => X; return g }`, "g", false},
		{`function f() { const X = -1; const g = () => X; return g }`, "g", false},
		{`const X = null; function g() { return X }`, "g", false},
	}
	for _, tc := range cases {
		prog, err := parser.Parse(tc.src, parser.Options{})
		if err != nil {
			t.Fatalf("%s: %v", tc.src, err)
		}
		for _, debug := range []bool{false, true} {
			main, err := Compile(prog, Options{Debug: debug})
			if err != nil {
				t.Fatalf("%s: %v", tc.src, err)
			}
			fn := findNamed(main, tc.fn)
			if fn == nil {
				t.Fatalf("%s: no function %s", tc.src, tc.fn)
			}
			want := tc.want && !debug
			if got := !readsX(fn); got != want {
				t.Errorf("%s (debugger %v): %s's reads of X are the literal: %v, want %v", tc.src, debug, tc.fn, got, want)
			}
		}
	}
}

// readsX reports whether fn reads a binding X: an upvalue of it, or, in a
// function declaring it, a read of a local.
func readsX(fn *bytecode.Function) bool {
	for _, u := range fn.Upvalues {
		if u.Name == "X" {
			return true
		}
	}
	for _, in := range fn.Code {
		switch in.Op {
		case bytecode.OpGetLocal, bytecode.OpGetLocal2, bytecode.OpGetLocalCheck, bytecode.OpGetGlobal:
			return true
		}
	}
	return false
}
