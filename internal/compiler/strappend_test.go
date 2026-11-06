package compiler

import (
	"testing"

	"github.com/go-quickjs/go-quickjs/internal/ast"
	"github.com/go-quickjs/go-quickjs/internal/bytecode"
	"github.com/go-quickjs/go-quickjs/internal/parser"
)

// marked counts, over fn and what it nests, the stores marked as appends and
// the str_freezes.
func marked(fn *bytecode.Function) (stores, freezes int) {
	for _, in := range fn.Code {
		switch {
		case in.Op == bytecode.OpSetUpvalue && in.B != 0:
			stores++
		case in.StrFreeze():
			freezes++
		}
	}
	for _, k := range fn.Constants {
		if k.Kind == bytecode.ConstFunction && k.Fn != nil {
			s, n := marked(k.Fn)
			stores, freezes = stores+s, freezes+n
		}
	}
	return stores, freezes
}

// appended counts, over fn and what it nests, the str_appends and the
// str_freezes.
func appended(fn *bytecode.Function) (appends, freezes int) {
	for _, in := range fn.Code {
		switch {
		case in.Op == bytecode.OpAdd && in.B == bytecode.AddOwned:
			appends++
		case in.StrFreeze():
			freezes++
		}
	}
	for _, k := range fn.Constants {
		if k.Kind == bytecode.ConstFunction && k.Fn != nil {
			s, n := appended(k.Fn)
			appends, freezes = appends+s, freezes+n
		}
	}
	return appends, freezes
}

func compileStrict(t *testing.T, src string, opts Options) *bytecode.Function {
	t.Helper()
	var prog *ast.Program
	var err error
	if prog, err = parser.Parse(src, parser.Options{}); err != nil {
		t.Fatalf("%s: %v", src, err)
	}
	fn, err := Compile(prog, opts)
	if err != nil {
		t.Fatalf("%s: %v", src, err)
	}
	return fn
}

// What is an owned append and what is not, and which reads are told so. The
// rule is that of strappend.go, and each case is what would be unsound, or
// costly, were it taken.
func TestOwnedAppends(t *testing.T) {
	for _, tc := range []struct {
		src             string
		stores, freezes int
	}{
		{`function f(xs) { var g = ""; var h = { push(a) { null !== a && (g += a); return true } }; return g }`, 1, 1},
		{`function f() { var s = ""; const add = x => { s += x }; return s + s }`, 1, 2},
		{`function f() { let s = ""; const add = x => { s += "lit" }; add(); return s }`, 1, 1},
		{`function f() { var s = ""; const a = x => { s += x }, b = y => { s += y }; return s }`, 2, 1},
		{`function f() { var s = ""; const add = x => { x || (s += x) } }`, 1, 0},
		{`function f() { var s = ""; const add = x => { (x, s += x, 1) } }`, 1, 0},
		// A read compiled first: nothing is owned, and nothing frozen.
		{`function f() { var s = ""; const r = s; const add = x => { s += x } }`, 0, 0},
		{`function f() { var s = ""; function get() { return s } const add = x => { s += x }; return s }`, 0, 0},
		{`function f() { var s = ""; const get = () => s; const add = x => { s += x } }`, 0, 0},
		// The value is used.
		{`function f() { var s = ""; const add = x => s += x }`, 0, 0},
		{`function f() { var s = ""; const add = x => { return s += x } }`, 0, 0},
		{`function f() { var s = ""; const add = x => { return x && (s += x) } }`, 0, 0},
		// The top level's, a module's to export.
		{`var s = ""; function f(x) { s += x }`, 0, 0},
		{`let s = ""; { const add = x => { s += x } }`, 0, 0},
		// What it is.
		{`function f(s) { const add = x => { s += x } }`, 0, 0},
		{`function f() { const s = ""; const add = x => { s += x } }`, 0, 0},
		{`function f() { for (let s = ""; ; ) { const add = x => { s += x } } }`, 0, 0},
		{`function f() { try {} catch (s) { const add = x => { s += x } } }`, 0, 0},
		{`function f() { const add = x => { s += x }; let s = "" }`, 0, 0},
		{`var g = function s() { const add = x => { s += x } }`, 0, 0},
		// A right side that could run code.
		{`function f() { var s = ""; const add = x => { s += x() } }`, 0, 0},
		{`function f() { var s = ""; const add = x => { s += x.y } }`, 0, 0},
		{`function f() { var s = ""; const add = x => { s += x + "" } }`, 0, 0},
		{`function f() { var s = ""; const add = x => { s += global } }`, 0, 0},
		{`function f() { var s = ""; const add = x => { s += 1 } }`, 0, 0},
		// Other operators.
		{`function f() { var s = 1; const add = x => { s -= x } }`, 0, 0},
		{`function f() { var s = 1; const add = x => { s = s + x } }`, 0, 0},
		// eval, anywhere in the function that declares it.
		{`function f() { var s = ""; const add = x => { s += x }; return eval("s") }`, 0, 0},
		{`function f() { var s = ""; const add = x => { s += x }; const g = () => () => eval("s") }`, 0, 0},
		// with.
		{`function f(o) { var s = ""; with (o) { const add = x => { s += x } } }`, 0, 0},
		// A read next: the read freezes, and the append is owned all the same.
		{`function f() { var s = ""; const add = x => { s += x; return s } }`, 1, 1},
		{`function f() { var s = ""; const add = x => { s += x; if (x) return s } }`, 1, 1},
		{`function f() { var s = ""; const add = (x, y) => { s += x; s += y; return s } }`, 2, 1},
		// x.length is no read.
		{`function f() { var s = ""; const n = () => s.length; const add = x => { s += x } }`, 1, 0},
		{`function f() { var s = ""; const add = x => { s += x; return s.length } }`, 1, 0},
		// Counters are left alone: a local's reads have no str_freeze.
		{`function f(n) { var t = 0; for (var i = 0; i < n; i += 2) t += i; return t + i }`, 0, 0},
		{`function f() { var t = 0; const add = () => { t += 1 }; return t }`, 0, 0},
		{`function f(g) { var t = 0; for (var i = 0; i < 9; i++) t += g(i); return t }`, 0, 0},
		{`function f(g) { var t; t += g(1); return t }`, 0, 0},
	} {
		fn := compileStrict(t, tc.src, Options{})
		if s, n := marked(fn); s != tc.stores || n != tc.freezes {
			t.Errorf("%s:\n  %d marked stores, %d str_freezes; want %d, %d", tc.src, s, n, tc.stores, tc.freezes)
		}
	}
	for _, tc := range []struct {
		src              string
		appends, freezes int
	}{
		{`function f(g) { var out = ""; out += g(1); out += "x"; if (g) return out }`, 2, 1},
		{`function f(g) { let out = "a" + g; out += g(1); if (g) return out + out }`, 1, 2},
		{`function f(g) { var out = "<ul>"; for (var i = 0; i < 9; i++) { out += g(i) } return out }`, 1, 1},
		// The last of a sequence too, whose value nothing uses.
		{`function f(g) { var t = "", r = 0; for (var i = 0; i < 9; i++) { r !== i && (t += g()), r = i, t += g() } return t }`, 2, 1},
		{`function f(g) { var out; out = ""; g && (out += g()); if (g) return out }`, 1, 1},
		{`function f(g, x) { var out = ""; x || (out += g()); if (g) return out }`, 1, 1},
		{"function f(g) { var out = `a${g}`; out += g(); if (g) return out }", 1, 1},
		{`function* f(g) { var out = ""; out += yield 1; if (g) return out }`, 1, 1},
		// A read next: the read freezes, and the append is owned all the same.
		{`function f(g) { var out = ""; out += g(1); return out }`, 1, 1},
		{`function f(g) { var out = ""; out += g(1); out += "x"; return out }`, 2, 1},
		{`function f(g) { var out = ""; out += g(1); var r = out; if (g) out += r }`, 2, 1},
		{`function f(g) { var out = ""; out += g(1); g(out); if (g) out += "a" }`, 2, 1},
		{`function f(g) { var a = "", b = ""; a += g(1); b += g(2); return a + b }`, 2, 2},
		// x.length is no read, the object of nothing but the length.
		{`function f(g) { var out = ""; while (out.length < 9) { out += g(1) } return out }`, 1, 1},
		{`function f(g) { var out = ""; out += g(1); var n = out.length; out += "x"; return n }`, 2, 0},
		// What only looks like one.
		{`function f(g) { var out = ""; g(out).length; out += g(1); return out }`, 0, 0},
		{`function f(g) { var out = ""; out?.length; out += g(1); return out }`, 0, 0},
		{`function f(g) { var out = ""; out.length(); out += g(1); return out }`, 0, 0},
		{`function f(g) { var out = ""; out["length"]; out += g(1); return out }`, 0, 0},
		{`function f(g) { var out = ""; out[out.length]; out += g(1); return out }`, 0, 0},
		// No string to be seen.
		{`function f(g) { var out; out += g(1); return out }`, 0, 0},
		{`function f(g) { var out = g(); out += g(1); return out }`, 0, 0},
		{`function f(out, g) { out += g(1); return out }`, 0, 0},
		// A read compiled first.
		{`function f(g) { var out = ""; var r = out; out += g(1); return out }`, 0, 0},
		// The value used.
		{`function f(g) { var out = ""; return out += g(1) }`, 0, 0},
		// Mentioned by a function or a class, before or after, or on the right.
		{`function f(g) { var out = ""; out += g(1); return () => out }`, 0, 0},
		{`function f(g) { var out = ""; const h = () => out; out += g(h); return out }`, 0, 0},
		{`function f(g) { var out = ""; out += g(1); class C { x = out } }`, 0, 0},
		{`function f(g) { var out = ""; out += g(1); var C = class { x = out } }`, 0, 0},
		{`function f(g) { var out = ""; out += g(1); class C { static { out = "a" } } }`, 0, 0},
		{`function f(g) { var out = ""; out += g(1); class C { m() { out = "a" } } }`, 0, 0},
		{`function f(g) { var out = ""; out += g(1); class C extends (out = "a", Object) {} }`, 0, 0},
		{`function f(g) { var out = ""; out += g(1); class C { [out] = 1 } }`, 0, 0},
		{`function f(g) { var out = ""; out += (class { static { out = "a" } }, "b") }`, 0, 0},
		{`function f(g) { var out = ""; out += g(out) }`, 0, 0},
		{`function f(g) { var out = ""; out += (out = "a", "b") }`, 0, 0},
		// eval, with, a loop's copies.
		{`function f(g) { var out = ""; out += g(1); return eval("out") }`, 0, 0},
		{`function f(g) { var out = ""; with (g) { out += g(1) } return out }`, 0, 0},
		{`function f(g) { for (let out = ""; ; ) { out += g(() => 1) } }`, 0, 0},
		// The top level, with no function to ask.
		{`var out = ""; out += x`, 0, 0},
	} {
		fn := compileStrict(t, tc.src, Options{})
		if s, n := appended(fn); s != tc.appends || n != tc.freezes {
			t.Errorf("%s:\n  %d str_appends, %d str_freezes; want %d, %d", tc.src, s, n, tc.appends, tc.freezes)
		}
	}
	// Code for a debugger, which reads any binding, has none.
	if s, n := marked(compileStrict(t, `function f() { var s = ""; const add = x => { s += x }; return s }`, Options{Debug: true})); s != 0 || n != 0 {
		t.Errorf("code for a debugger: %d marked stores, %d str_freezes", s, n)
	}
}

// A function declaration's body and parameters can capture a local too.
func TestLocalAppendFunctionDeclarations(t *testing.T) {
	for _, src := range []string{
		`function f() { var out = ""; function emit() { out += "X"; return "Y" } out += emit(); if (1) return out }`,
		`function f() { var out = ""; out += "a"; { function emit() { out += "X"; return "Y" } out += emit() } }`,
		`function f() { var out = ""; function emit(x = (out += "X")) { return x } out += emit(); if (1) return out }`,
		`function f() { var out = ""; function* emit() { out += "X"; yield "Y" } out += emit().next().value; if (1) return out }`,
	} {
		if n, _ := appended(compileStrict(t, src, Options{})); n != 0 {
			t.Errorf("%s: %d local appends to a captured binding", src, n)
		}
	}
}

// walkFunctions calls see on fn and every function in it.
func walkFunctions(fn *bytecode.Function, see func(*bytecode.Function)) {
	see(fn)
	for _, k := range fn.Constants {
		if k.Kind == bytecode.ConstFunction && k.Fn != nil {
			walkFunctions(k.Fn, see)
		}
	}
}
