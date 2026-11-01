package compiler

import (
	"reflect"
	"testing"

	"github.com/go-quickjs/go-quickjs/internal/ast"
	"github.com/go-quickjs/go-quickjs/internal/bytecode"
	"github.com/go-quickjs/go-quickjs/internal/parser"
)

// Siblings reuse a compiler while their returned functions remain live. Its
// settings and source references must be gone before the next sibling starts.
func TestCompilerStateReuse(t *testing.T) {
	const src = `
		function* first(a = 1) {
			let x = a;
			try { yield (() => x + arguments[0])(); }
			finally { x = eval("2"); }
		}
		function second() { "use strict"; return 7; }
		function third(a) { return function inner() { return a; }; }
	`
	prog, err := parser.Parse(src, parser.Options{})
	if err != nil {
		t.Fatal(err)
	}
	for _, debug := range []bool{false, true} {
		c := newCompiler(nil, Options{Text: src, Source: "siblings.js", Debug: debug})
		pool := c.pool
		var first, snapshot *bytecode.Function
		var captured *bytecode.Function
		var upvalues []bytecode.UpvalueDesc
		var previous *scratch
		for _, stmt := range prog.Body {
			lit := stmt.(*ast.FuncDecl).Fn
			c.compileFunctionLiteral(lit, "")
			s := pool.free[len(pool.free)-1]
			if previous != nil && s != previous {
				t.Fatal("sibling did not exercise compiler reuse")
			}
			previous = s
			if !reflect.ValueOf(s.state).IsZero() {
				t.Fatal("released compiler retains state")
			}
			fn := c.fn.Constants[len(c.fn.Constants)-1].Fn
			if fn == nil || fn.Name != lit.Name.Name || fn.Script != c.script || (fn.Debug != nil) != debug {
				t.Fatalf("debug=%v: lost function metadata for %s", debug, lit.Name.Name)
			}
			if first == nil {
				first = fn
				captured = findArrow(fn)
				if captured == nil || len(captured.Upvalues) == 0 {
					t.Fatal("first function's arrow did not capture bindings")
				}
				upvalues = exact(captured.Upvalues)
				copy := *fn
				copy.Code, copy.Lines = exact(fn.Code), exact(fn.Lines)
				copy.Names, copy.Constants = exact(fn.Names), exact(fn.Constants)
				copy.Locals, copy.Upvalues = exact(fn.Locals), exact(fn.Upvalues)
				snapshot = &copy
			} else if fn == first || !reflect.DeepEqual(first, snapshot) {
				t.Fatal("reusing the compiler changed a returned function")
			}
			if fn.Name == "second" && (!fn.Strict || fn.Generator || fn.UsesArguments || fn.HasDirectEval || len(fn.Upvalues) != 0) {
				t.Fatal("second function inherited the first function's settings")
			}
		}
		c.finish()
		fn := c.done()
		if fn == nil || !reflect.DeepEqual(first, snapshot) || !reflect.DeepEqual(captured.Upvalues, upvalues) {
			t.Fatal("returning the pool changed a returned function")
		}
	}
}
