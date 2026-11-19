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
		function post(a, i) { var j = i++; return a[i++] + j + i }
		function cap(n) { var calls = 0; var f = () => { calls++ }; f(); return calls }
		function chain(o) { return o.a.b.c.d }
		function chainSelf(o) { o = o.a.b; return o }
		function chainOptional(o) { return o?.a.b }
		function initBoth(p) { var a; if (p) a = 1; else a = 2; return a }
		function initSome(p) { var a, b; if (p) a = 1; b = 2; return a }
		function initLoop(n) { var i, a; for (i = 0; i < n; i++) a = i; return a }
		function initRead() { var a, b = a; a = 1; return b }
		function initCaptured() { var a = 1; return () => a }
		function initOwned(n) { var a = n * 3n; a = a * 2n; return a }
		function initSwitch(p) { var a; switch (p) { case 1: a = 2; break; case 2: return a } return a }`)
	want := map[string]string{
		"fib": `
   0  jnlt_imm r2, 2 -> 2
   1  ret r2
   2  get_global r3, name0 @1
   3  sub_imm r5, r2, 1
   4  call_w r3 = r3(1)
   5  get_global r4, name0 @2
   6  sub_imm r6, r2, 2
   7  call_w r4 = r4(1)
   8  add r3, r3, r4
   9  ret r3
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
  10  step_jlt r3 += 1 (update), r2 -> 6
  11  jlt r3, r2 -> 6
  12  ret r4
  13  ret_undef`,
	}
	for name, listing := range want {
		f := fns[name]
		if f == nil || f.RegCode() == nil {
			t.Fatalf("%s has no register code", name)
		}
		if got := strings.TrimRight(f.RegCode().String(), "\n"); got != strings.TrimPrefix(listing, "\n") {
			t.Errorf("%s:\n%s\nwant:\n%s", name, got, strings.TrimPrefix(listing, "\n"))
		}
	}
	for _, name := range []string{"stores", "method", "post", "cap"} {
		if fns[name] == nil || fns[name].RegCode() == nil {
			t.Errorf("%s has no register code", name)
		}
	}
	has := func(name, op string) {
		if f := fns[name]; f != nil && f.RegCode() != nil && !strings.Contains(f.RegCode().String(), op) {
			t.Errorf("%s has no %s:\n%s", name, op, f.RegCode())
		}
	}
	has("stores", "set_index_imm")
	has("stores", "set_prop")
	has("method", "get_method")
	has("method", "call_method_w")
	has("post", "post_inc")
	has("post", "get_index_post")
	has("cap", "closure_w")
	for _, name := range []string{"chain", "chainSelf"} {
		has(name, "get_prop_next")
		code := fns[name].RegCode().Code
		pairs := 0
		for pc, w := range code {
			if w.Op() == bytecode.RGetPropNext {
				pairs++
				if pc+1 >= len(code) || code[pc+1].Op() != bytecode.RGetProp || code[pc+1].B() != w.A() {
					t.Errorf("%s: broken property pair at %d", name, pc)
				}
			}
		}
		if name == "chain" && pairs != 2 {
			t.Errorf("chain: %d pairs, want 2", pairs)
		}
	}
	if code := fns["chainOptional"].RegCode().String(); strings.Contains(code, "get_prop_next") {
		t.Errorf("optional chain has a fused read:\n%s", code)
	}
	for name, want := range map[string]int{
		"locals": 0, "initBoth": 0, "initSome": 1, "initLoop": 2,
		"initRead": 1, "initCaptured": 1, "initOwned": 1, "initSwitch": 1,
	} {
		rc := fns[name].RegCode()
		if got := rc.Inits - int(rc.SkipInit); got != want {
			t.Errorf("%s: %d bindings initialized, want %d:\n%s", name, got, want, rc)
		}
	}
}

// TestRegisterBackendSkipInitExclusions pins the functions whose bindings
// the entry still makes undefined, each read before it is written or where
// what reads it is not the code's own: a let read in its dead zone, a var a
// closure reads before it is written, a binding a handler or a finally
// block reads, a generator's, an async function's, one beside arguments
// and one a direct eval may read.
func TestRegisterBackendSkipInitExclusions(t *testing.T) {
	fns := regFunctions(t, `
		function tdz(p) { if (p) return x; let x = 1; return x }
		function captured() { var get = () => v; var r = get(); var v = 2; return r }
		function fin(p) { var a; try { if (p) throw 1; a = 1 } finally { return a } }
		function* gen() { var a; yield 1; a = 2; return a }
		async function asy() { var a; await 1; a = 2; return a }
		function args() { var a; a = arguments.length; return a }
		function evals(s) { var a; a = 1; return eval(s) }
		function written() { var a; a = 1; return a }`)
	for _, name := range []string{"tdz", "captured", "fin", "gen", "asy", "args", "evals"} {
		if rc := fns[name].RegCode(); rc.Inits == 0 || rc.SkipInit != 0 {
			t.Errorf("%s: %d of %d bindings left to their writes, want none:\n%s", name, rc.SkipInit, rc.Inits, rc)
		}
	}
	if rc := fns["written"].RegCode(); rc.SkipInit != 1 {
		t.Errorf("written: %d bindings left to their writes, want 1:\n%s", rc.SkipInit, rc)
	}
}

// TestRegisterBackendFallsBack pins forms the backend once refused, each
// function as a whole: a generator and a direct eval
// are register code. A try statement is register code, with a handler
// table, and so are a catch clause's pattern, an optional chain and a
// strict global store of a value that runs code, which settles whether
// the name resolves before the value runs.
func TestRegisterBackendFallsBack(t *testing.T) {
	fns := regFunctions(t, `
		function tried() { try { return 1 } catch ({ e }) { return 2 } }
		function handled(x) { try { return x.y } catch (e) { return e } finally { x = 0 } }
		function opt(o) { return o?.x }
		function* gen() { yield 1 }
		function ev(s) { return eval(s) }
		function strictStore() { 'use strict'; g = plain(1) }
		function plain(x) { return x + 1 }`)
	for _, name := range []string{"tried", "opt", "strictStore", "gen"} {
		if f := fns[name]; f == nil || f.RegCode() == nil {
			t.Errorf("%s: want register code", name)
		}
	}
	if f := fns["strictStore"]; f != nil && f.RegCode() != nil {
		for _, op := range []string{"check_global_ref", "assert_resolved", "set_global"} {
			if !strings.Contains(f.RegCode().String(), op) {
				t.Errorf("strictStore has no %s:\n%s", op, f.RegCode())
			}
		}
	}
	// A direct eval in a sloppy function is register code, its callee
	// probed among the eval's variables, which may declare eval.
	if f := fns["ev"]; f == nil || f.RegCode() == nil {
		t.Errorf("ev: want register code")
	} else {
		for _, op := range []string{"direct_eval", "with_get_this"} {
			if !strings.Contains(f.RegCode().String(), op) {
				t.Errorf("ev has no %s:\n%s", op, f.RegCode())
			}
		}
	}
	if f := fns["handled"]; f == nil || f.RegCode() == nil || len(f.RegCode().Handlers) != 2 {
		t.Errorf("handled: want register code with two handlers")
	}
	if f := fns["plain"]; f == nil || f.RegCode() == nil {
		t.Errorf("plain has no register code")
	}
}
