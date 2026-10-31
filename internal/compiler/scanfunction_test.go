package compiler

import (
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/go-quickjs/go-quickjs/internal/ast"
	"github.com/go-quickjs/go-quickjs/internal/parser"
)

// The walks scanFunction replaced, one for each fact, which it is checked
// against.

// referencesArguments reports whether a function body mentions `arguments`.
//
// The scan descends into arrow functions, because an arrow has no arguments
// object of its own and refers to the enclosing function's, but stops at any
// other function, which gets its own.
//
// The compiler needs this before emitting a function's prologue: an arrow can
// only capture `arguments` if the enclosing function materialized it into a
// slot, and adding that slot later would mean rewriting a prologue that has
// already been emitted.
func referencesArguments(body []ast.Stmt) bool {
	w := &argumentsScanner{}
	w.stmts(body)
	return w.found
}

// referencesArgumentsInParams reports whether a parameter list mentions
// `arguments`, which a default value may: the object exists before the
// parameters are initialized.
func referencesArgumentsInParams(params []ast.Expr) bool {
	w := &argumentsScanner{}
	for _, p := range params {
		w.expr(p)
	}
	return w.found
}

// thisPropertyCount is how many distinct properties a function body assigns to
// `this` by name.
//
// A constructor's object is made with room for them, rather than growing its
// table as the body fills it in. The walk descends into arrows, which share the
// `this` they are written in, and stops at any other function.
func thisPropertyCount(body []ast.Stmt) int {
	w := &argumentsScanner{thisProps: []string{}}
	w.stmts(body)
	return len(w.thisProps)
}

// containsDirectEval reports whether a statement list calls `eval` by that
// name, outside any function written inside it.
//
// The answer is needed before the body is compiled: a frame of a function that
// contains one needs somewhere to put the bindings the evaluated code may
// declare, and the references to them have to be compiled to look there.
func containsDirectEval(body []ast.Stmt) bool {
	w := &argumentsScanner{seekDirectEval: true}
	w.stmts(body)
	return w.found
}

// containsDirectEvalInParams is the same for a parameter list, where a default
// value may contain one.
func containsDirectEvalInParams(params []ast.Expr) bool {
	w := &argumentsScanner{seekDirectEval: true}
	for _, p := range params {
		w.expr(p)
	}
	return w.found
}

// referencesThis reports whether a function body can observe its `this`.
//
// It is asked so that a sloppy-mode call can skip substituting the global
// object when nothing would see the difference -- which is most functions, and
// the substitution costs a pointer write on every call.
//
// A direct eval could see it, so a body containing one counts as using it.
func referencesThis(fn *ast.FuncLit) bool {
	w := &argumentsScanner{seekThis: true}
	for _, p := range fn.Params {
		w.expr(p)
	}
	w.stmts(fn.Body)
	return w.found
}

// Each function's facts, found in one walk (scanFunction), are what the
// walks of one fact each found, for every function of every program: the
// cases below, which put each fact where only one of the walks looks --
// an eval in an arrow, arguments in a default, this in a class field, a
// this property in a parameter -- the repository's scripts, and those
// under QUICKJS_SCAN_CORPUS, test262's or Octane's, where it is set.
func TestScanFunctionMatchesTheWalks(t *testing.T) {
	sources := []string{
		`function f(a = arguments) { return a }`,
		`function f() { return () => arguments }`,
		`function f() { return function () { return arguments } }`,
		`function f() { eval("1") }`,
		`function f() { return () => eval("1") }`,
		`function f(a = eval("1")) {}`,
		`function f() { return eval }`,
		`function f(a = this) {}`,
		`function f() { return () => this }`,
		`function f() { return function () { return this } }`,
		`class A extends Object { constructor() { super(); this.x = 1; this.y = 2; this.x = 3 } }`,
		`class A { m() { return () => super.m } }`,
		`function f() { class C { x = this.y; z = eval("1"); [this.k] = 1 } }`,
		`function P(a = (this.q = 1)) { this.a = a; (() => { this.b = 2 })(); function g() { this.c = 3 } }`,
		`function f() { with (o) { arguments } }`,
		`function f() { var g = function () { eval("1") }; return arguments.length }`,
	}
	for _, path := range []string{"../../internal/v8bench", "../../testdata", "../../internal/vm/testdata", "../../stdlib/testdata"} {
		sources = append(sources, readScripts(t, path)...)
	}
	if dir := os.Getenv("QUICKJS_SCAN_CORPUS"); dir != "" {
		for _, d := range filepath.SplitList(dir) {
			sources = append(sources, readScripts(t, d)...)
		}
	}
	funcs := 0
	for _, src := range sources {
		prog, err := parser.Parse(src, parser.Options{})
		if err != nil {
			continue
		}
		for _, fn := range funcLits(prog) {
			funcs++
			got := scanFunction(fn, true)
			want := functionFacts{
				usesThis:          referencesThis(fn),
				usesArguments:     referencesArguments(fn.Body) || referencesArgumentsInParams(fn.Params),
				argumentsInParams: referencesArgumentsInParams(fn.Params),
				directEval:        containsDirectEval(fn.Body) || containsDirectEvalInParams(fn.Params),
			}
			n := thisPropertyCount(fn.Body)
			got.thisProps, got.propsDone = nil, false
			if !reflect.DeepEqual(got, want) || len(scanFunction(fn, true).thisProps) != n {
				t.Fatalf("function at %d of %.80q: one walk found %+v (%d this properties), the walks %+v (%d)",
					fn.Start, src, got, len(scanFunction(fn, true).thisProps), want, n)
			}
		}
	}
	if funcs < 200 {
		t.Fatalf("only %d functions compared", funcs)
	}
	t.Logf("%d functions compared", funcs)
}

// readScripts is the text of every .js file under dir.
func readScripts(t *testing.T, dir string) []string {
	t.Helper()
	var out []string
	filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".js") {
			return nil
		}
		if b, err := os.ReadFile(path); err == nil {
			out = append(out, string(b))
		}
		return nil
	})
	return out
}

// funcLits is every function literal in prog, found through its fields.
func funcLits(prog *ast.Program) []*ast.FuncLit {
	var out []*ast.FuncLit
	seen := map[uintptr]bool{}
	fnType := reflect.TypeOf((*ast.FuncLit)(nil))
	var walk func(v reflect.Value)
	walk = func(v reflect.Value) {
		switch v.Kind() {
		case reflect.Interface:
			if !v.IsNil() {
				walk(v.Elem())
			}
		case reflect.Pointer:
			if v.IsNil() || seen[v.Pointer()] {
				return
			}
			seen[v.Pointer()] = true
			if v.Type() == fnType {
				out = append(out, v.Interface().(*ast.FuncLit))
			}
			walk(v.Elem())
		case reflect.Struct:
			for i := range v.NumField() {
				if v.Type().Field(i).IsExported() {
					walk(v.Field(i))
				}
			}
		case reflect.Slice, reflect.Array:
			for i := range v.Len() {
				walk(v.Index(i))
			}
		}
	}
	walk(reflect.ValueOf(prog))
	return out
}
