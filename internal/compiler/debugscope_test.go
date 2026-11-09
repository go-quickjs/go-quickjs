package compiler

import (
	"fmt"
	"strings"
	"testing"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
	"github.com/go-quickjs/go-quickjs/internal/parser"
)

// Code for a debugger captures of the enclosing functions only what it
// uses, as V8's closures do, and each place it can stop sees its own
// bindings and those. It had captured every enclosing binding for the
// debugger's sake: a bundle's functions under its module's bindings --
// typescript.js's 21,693 under 9,523 -- made hundreds of millions of
// upvalues, and a compile that never finished.
func TestDebugScopesCaptureWhatIsUsed(t *testing.T) {
	const vars, funcs = 2000, 200
	var b strings.Builder
	b.WriteString("function outer() {")
	for i := range vars {
		fmt.Fprintf(&b, " var v%d = %d;", i, i)
	}
	for i := range funcs {
		fmt.Fprintf(&b, " function f%d(a) { let own = a; return own + v%d }", i, i)
	}
	b.WriteString(" return f0 }")
	src := b.String()
	prog, err := parser.Parse(src, parser.Options{})
	if err != nil {
		t.Fatal(err)
	}
	top, err := Compile(prog, Options{Text: src, Debug: true})
	if err != nil {
		t.Fatal(err)
	}
	outer := constFunctions(top)[0]
	inner := constFunctions(outer)
	if len(inner) != funcs {
		t.Fatalf("%d functions in outer, want %d", len(inner), funcs)
	}
	for _, fn := range inner {
		i := 0
		fmt.Sscanf(fn.Name, "f%d", &i)
		want := fmt.Sprintf("v%d", i)
		if len(fn.Upvalues) != 1 || fn.Upvalues[0].Name != want {
			t.Fatalf("%s: %d upvalues, want only %s", fn.Name, len(fn.Upvalues), want)
		}
		if fn.Debug == nil || len(fn.Debug.Statements) == 0 {
			t.Fatalf("%s: no places to stop", fn.Name)
		}
		for _, st := range fn.Debug.Statements {
			var names []string
			for _, bd := range fn.EvalScopes[st.Scope].Bindings {
				names = append(names, bd.Name)
			}
			// Its own bindings first, innermost first, then what it
			// captures.
			if got, want := strings.Join(names, " "), "own a "+want; got != want && got != "a "+want {
				t.Fatalf("%s at pc %d: scope %q, want %q", fn.Name, st.PC, got, want)
			}
		}
	}
	// outer's own scope still has each of its bindings.
	last := outer.Debug.Statements[len(outer.Debug.Statements)-1]
	if n := len(outer.EvalScopes[last.Scope].Bindings); n < vars+funcs {
		t.Fatalf("outer's scope has %d bindings, want its %d", n, vars+funcs)
	}
}

// constFunctions are the functions fn's constants hold, in order.
func constFunctions(fn *bytecode.Function) []*bytecode.Function {
	var out []*bytecode.Function
	for _, c := range fn.Constants {
		if c.Kind == bytecode.ConstFunction && c.Fn != nil {
			out = append(out, c.Fn)
		}
	}
	return out
}
