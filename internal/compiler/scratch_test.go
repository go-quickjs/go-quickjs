package compiler

import (
	"fmt"
	"reflect"
	"strings"
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

// The size a function's tables are given comes from its own source: the
// wrapper of a bundle spans the bundle, and has little code of its own.
func TestReserveLeavesNestedFunctionsOut(t *testing.T) {
	body := strings.Repeat("x = x + 1;\n", 2000)
	src := "(function wrapper() {\n" +
		"function inner() {" + body + "}\n" +
		"var arrow = () => {" + body + "};\n" +
		"class C { m() {" + body + "} }\n" +
		"})"
	prog, err := parser.Parse(src, parser.Options{})
	if err != nil {
		t.Fatal(err)
	}
	wrapper := prog.Body[0].(*ast.ExprStmt).X.(*ast.FuncLit)
	own := wrapper.End - wrapper.Start - scanFunction(wrapper, false).innerBytes
	if own >= reserveMin {
		t.Errorf("the wrapper's own source is %d of its %d bytes", own, wrapper.End-wrapper.Start)
	}
	inner := wrapper.Body[0].(*ast.FuncDecl).Fn
	if got := scanFunction(inner, false).innerBytes; got != 0 {
		t.Errorf("a function with none written in it has %d bytes of them", got)
	}
}

// A function's scratch clears what it wrote and no more, and a table a large
// function grew is not cleared again for each sibling after it.
func TestScratchAfterALargeFunction(t *testing.T) {
	var large strings.Builder
	large.WriteString("function large() { return 0")
	for i := range 2 * bigIndex {
		fmt.Fprintf(&large, " + g%d.p%d + 'c%d'", i, i, i)
	}
	large.WriteString("; }\nfunction small() { return s.t + 'u'; }\n")
	src := large.String()
	prog, err := parser.Parse(src, parser.Options{})
	if err != nil {
		t.Fatal(err)
	}
	c := newCompiler(nil, Options{Text: src, Source: "large.js"})
	pool := c.pool
	var fns []*bytecode.Function
	for _, stmt := range prog.Body {
		c.compileFunctionLiteral(stmt.(*ast.FuncDecl).Fn, "")
		fns = append(fns, c.fn.Constants[len(c.fn.Constants)-1].Fn)
		s := pool.free[len(pool.free)-1]
		if len(s.nameIndex) != 0 || len(s.constIndex) != 0 {
			t.Fatal("a map was left with entries")
		}
		for _, name := range s.names[:cap(s.names)] {
			if name != "" {
				t.Fatalf("the scratch holds the name %q", name)
			}
		}
		for _, k := range s.consts[:cap(s.consts)] {
			if k != (bytecode.Constant{}) {
				t.Fatalf("the scratch holds the constant %+v", k)
			}
		}
	}
	if len(fns[0].Names) < 4*bigIndex || len(fns[1].Names) != 2 || fns[1].Names[0] != "s" || fns[1].Names[1] != "t" {
		t.Errorf("functions with %d and %d names: %q", len(fns[0].Names), len(fns[1].Names), fns[1].Names)
	}
}

// Locals a scope left stay out of a pooled scratch too, though the list no
// longer reaches them.
func TestScratchClearsLocalsOfClosedScopes(t *testing.T) {
	const src = `function f() { { let a = 1, b = 2, c = 3; { let d = 4; g(a + b + c + d); } } return 1; }`
	prog, err := parser.Parse(src, parser.Options{})
	if err != nil {
		t.Fatal(err)
	}
	c := newCompiler(nil, Options{Text: src, Source: "scopes.js"})
	pool := c.pool
	c.compileFunctionLiteral(prog.Body[0].(*ast.FuncDecl).Fn, "")
	s := pool.free[len(pool.free)-1]
	for i, l := range s.locals[:cap(s.locals)] {
		if l.name != "" {
			t.Fatalf("the scratch's local %d is still %q", i, l.name)
		}
	}
}

// A name a function does not bind is not found in it, and one it bound in a
// scope that has ended is not either: the filter only ever says no.
func TestLocalNamesFilter(t *testing.T) {
	c := newCompiler(nil, Options{Text: "x", Source: "filter.js"})
	defer c.done()
	if _, ok := c.resolveLocal("a"); ok {
		t.Error("found a name in no scope")
	}
	c.addLocal(localVar{name: "a"})
	c.addLocal(localVar{name: ""})
	if l, ok := c.resolveLocal("a"); !ok || l.name != "a" {
		t.Error("a bound name was not found")
	}
	if _, ok := c.resolveLocal(""); !ok {
		t.Error("an empty name was not found")
	}
	if _, ok := c.resolveLocal("b"); ok {
		t.Error("found a name never bound")
	}
}
