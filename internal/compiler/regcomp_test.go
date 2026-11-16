package compiler

import (
	"strings"
	"testing"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
	"github.com/go-quickjs/go-quickjs/internal/parser"
)

// regFunctions compiles src with the register backend on and returns its
// functions by name.
func regFunctions(t *testing.T, src string) map[string]*bytecode.Function {
	t.Helper()
	SetRegisterForm(true)
	defer SetRegisterForm(false)
	prog, err := parser.Parse(src, parser.Options{})
	if err != nil {
		t.Fatal(err)
	}
	fn, err := Compile(prog, Options{})
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]*bytecode.Function{}
	var walk func(f *bytecode.Function)
	walk = func(f *bytecode.Function) {
		out[f.Name] = f
		for _, c := range f.Constants {
			if c.Kind == bytecode.ConstFunction && c.Fn != nil {
				walk(c.Fn)
			}
		}
	}
	walk(fn)
	return out
}

// TestRegisterBackendCode pins the register backend's code for the core
// shapes: operands read in place, a loop's test at its end as one
// compare-and-branch, immediates, a call whose window is the callee's frame,
// a method call's get_method into the window, a postfix update whose value
// is used, a closure's capture of a binding, and a compound assignment
// into the binding's register.
func TestRegisterBackendCode(t *testing.T) {
	fns := regFunctions(t, `
		function fib(k) { return k < 2 ? k : fib(k - 1) + fib(k - 2) }
		function locals(n) { var j, a = 1, b = 2, c = 3, d = 4; for (j = 0; j < n; j++) { a = b; b = c; c = d; d = a } return a }
		function stores(o, t, n) { for (var j = 0; j < n; j++) { o.a = j; t[3] = j } }
		function method(o, n) { var s = 0; for (var j = 0; j < n; j++) s += o.sum(0); return s }
		function post(a, i) { return a[i++] + i }
		function cap(n) { var calls = 0; var f = () => { calls++ }; f(); return calls }`)
	want := map[string]string{
		"fib": `
   0  jnlt_imm r2, 2 -> 2
   1  ret r2
   2  get_global r3, name0 @3
   3  sub_imm r5, r2, 1
   4  call_w r3 = r3(1)
   5  get_global r6, name0 @4
   6  sub_imm r8, r2, 2
   7  call_w r6 = r6(1)
   8  add r9, r3, r6
   9  ret r9
  10  ret_undef`,
		"locals": `
   0  load_int r4, 1
   1  load_int r5, 2
   2  load_int r6, 3
   3  load_int r7, 4
   4  load_int r3, 0
   5  jmp -> 11
   6  move r4, r5
   7  move r5, r6
   8  move r6, r7
   9  move r7, r4
  10  inc r3, r3
  11  jlt r3, r2 -> 6
  12  ret r4
  13  ret_undef`,
	}
	for name, listing := range want {
		f := fns[name]
		if f == nil || f.Reg == nil {
			t.Fatalf("%s has no register code", name)
		}
		if got := strings.TrimRight(f.Reg.String(), "\n"); got != strings.TrimPrefix(listing, "\n") {
			t.Errorf("%s:\n%s\nwant:\n%s", name, got, strings.TrimPrefix(listing, "\n"))
		}
	}
	for _, name := range []string{"stores", "method", "post", "cap"} {
		if fns[name] == nil || fns[name].Reg == nil {
			t.Errorf("%s has no register code", name)
		}
	}
	has := func(name, op string) {
		if f := fns[name]; f != nil && f.Reg != nil && !strings.Contains(f.Reg.String(), op) {
			t.Errorf("%s has no %s:\n%s", name, op, f.Reg)
		}
	}
	has("stores", "set_index_imm")
	has("stores", "set_prop")
	has("method", "get_method")
	has("method", "call_method_w")
	has("post", "post_inc")
	has("cap", "closure_w")
}

// TestRegisterBackendFallsBack pins what the backend leaves to the stack
// form, each function as a whole: a try statement, an optional chain, a
// generator, a strict global store of a value that runs code.
func TestRegisterBackendFallsBack(t *testing.T) {
	fns := regFunctions(t, `
		function tried() { try { return 1 } catch (e) { return 2 } }
		function opt(o) { return o?.x }
		function* gen() { yield 1 }
		function strictStore() { 'use strict'; g = plain(1) }
		function plain(x) { return x + 1 }`)
	for _, name := range []string{"tried", "opt", "gen", "strictStore"} {
		if f := fns[name]; f == nil || f.Reg != nil {
			t.Errorf("%s: register code %v, want none", name, f != nil && f.Reg != nil)
		}
	}
	if f := fns["plain"]; f == nil || f.Reg == nil {
		t.Errorf("plain has no register code")
	}
	// The stack form is the same with the backend on as off.
	SetRegisterForm(false)
	prog, _ := parser.Parse(`function plain(x) { return x + 1 }`, parser.Options{})
	off, _ := Compile(prog, Options{})
	on := regFunctions(t, `function plain(x) { return x + 1 }`)["plain"]
	if a, b := off.Constants[0].Fn.Disassemble(), on.Disassemble(); a != b {
		t.Errorf("the stack form differs with the backend on:\n%s\nwant:\n%s", b, a)
	}
}
