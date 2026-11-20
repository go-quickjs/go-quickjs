package vm

import (
	"testing"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
	"github.com/go-quickjs/go-quickjs/internal/compiler"
	"github.com/go-quickjs/go-quickjs/internal/parser"
)

// regI32Eval runs src as register code, by whichever dispatcher the
// process has, and returns its value as a string.
func regI32Eval(t *testing.T, src string) string {
	t.Helper()
	fn := compileForTest(t, src)
	r := newTestRuntime(t, Config{})
	defer r.Close()
	v, err := r.Run(fn)
	if err != nil {
		t.Fatal(err)
	}
	return v.String().Go()
}

// A body of mul_imm, return x * k, is one the leaf runner runs without a
// frame (regLeafRunnable), as it ran load_int and mul: a number's product,
// its -0 and its NaN; what is no number gives up, and the frame's run
// makes it as mul does.
func TestRegLeafMulImm(t *testing.T) {
	src := "function f(x) { return x * 3 }"
	prog, err := parser.Parse(src, parser.Options{})
	if err != nil {
		t.Fatal(err)
	}
	top, err := compiler.Compile(prog, compiler.Options{Text: src})
	if err != nil {
		t.Fatal(err)
	}
	var rc *bytecode.RegCode
	for _, k := range top.Constants {
		if k.Kind == bytecode.ConstFunction && k.Fn != nil && k.Fn.Name == "f" {
			rc = k.Fn.RegCode()
		}
	}
	if rc == nil || rc.Code[0].Op() != bytecode.RMulI || !regLeafRunnable(rc) {
		t.Fatalf("f's register code is not a leaf of mul_imm:\n%s", rc)
	}
	got := regI32Eval(t, `
		function f(x) { return x * 3 }
		function g(x) { return -2 * x }
		var out = [], vals = [1, -0, 0, NaN, Infinity, 2147483647, 0.5, "4", "x", null, true, { valueOf: function () { return 5 } }];
		for (var r = 0; r < 20; r++) for (var i = 0; i < vals.length; i++) out.push(Object.is(f(vals[i]), -0) ? "-0" : String(f(vals[i])), String(g(vals[i])));
		try { f(1n) } catch (e) { out.push(e.name) }
		out.slice(-25).join()`)
	if want := "3,-2,-0,0,0,0,NaN,NaN,Infinity,-Infinity,6442450941,-4294967294,1.5,-1,12,-8,NaN,NaN,0,0,3,-2,15,-10,TypeError"; got != want {
		t.Errorf("got  %s\nwant %s", got, want)
	}
}

// The int32 forms' and mul_imm's answers, exactly: ToInt32 at 2^31, past
// 2^32, 2^53 and 2^63, of NaN, the infinities and -0; strings by their
// numbers but for a + of one, which concatenates; valueOf called once
// for each operand, in order, once a round; and the TypeErrors, a BigInt sum's
// of the | 0 and a mixed one's of the + -- by whichever dispatcher the
// process runs (QJS_NONATIVE=1 for the Go table's).
func TestRegInt32Exact(t *testing.T) {
	got := regI32Eval(t, `
		function show(v) { return Object.is(v, -0) ? "-0" : String(v) }
		function sums(x, y) { return [(x + y) | 0, (x - y) | 0, (x + 1) | 0, (x - 1) | 0, x * 3, 3 * x].map(show).join(" ") }
		var log = [], o = { valueOf: function () { log.push("o"); return 2 } }, p = { valueOf: function () { log.push("p"); return 3 } };
		var cases = [[2147483647, 1], [-2147483648, 1], [4294967296, 5], [2 ** 53, 2 ** 53], [2 ** 63, 0], [-(2 ** 63), -1], [1e300, 1],
			[NaN, 1], [Infinity, 1], [-Infinity, -Infinity], [-0, 0], [-0, -0], ["12", 3], ["5", 1], [o, p], [2.9, -0.9]];
		var out = [];
		for (var r = 0; r < 10; r++) { out = []; log = []; for (var i = 0; i < cases.length; i++) out.push(sums(cases[i][0], cases[i][1])) }
		function err(f) { try { return f() } catch (e) { return e.name + ": " + e.message } }
		out.push(log.join(""), err(function () { var a = 1n; return (a + a) | 0 }), err(function () { var a = 1n; return (a + 1) | 0 }),
			err(function () { var a = 2n; return (a - a) | 0 }), err(function () { var a = 1n; return a * 3 }));
		out.join("; ")`)
	want := "-2147483648 2147483646 -2147483648 2147483646 6442450941 6442450941; " +
		"-2147483647 2147483647 -2147483647 2147483647 -6442450944 -6442450944; " +
		"5 -5 1 -1 12884901888 12884901888; " +
		"0 0 0 -1 27021597764222976 27021597764222976; " +
		"0 0 0 0 27670116110564327000 27670116110564327000; " +
		"0 0 0 0 -27670116110564327000 -27670116110564327000; " +
		"0 0 0 0 3e+300 3e+300; " +
		"0 0 0 0 NaN NaN; " +
		"0 0 0 0 Infinity Infinity; " +
		"0 0 0 0 -Infinity -Infinity; " +
		"0 0 1 -1 -0 -0; " +
		"0 0 1 -1 -0 -0; " +
		"123 9 121 11 36 36; " +
		"51 4 51 4 15 15; " +
		"5 -1 3 1 6 6; " +
		"2 3 3 1 8.7 8.7; " +
		"opopoooo" +
		"; TypeError: cannot mix BigInt and other types; TypeError: cannot mix BigInt and other types" +
		"; TypeError: cannot mix BigInt and other types; TypeError: cannot mix BigInt and other types"
	if got != want {
		t.Errorf("got  %s\nwant %s", got, want)
	}
}
