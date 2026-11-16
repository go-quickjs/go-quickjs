package v8bench

import (
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"

	quickjs "github.com/go-quickjs/go-quickjs"
	"github.com/go-quickjs/go-quickjs/internal/compiler"
	"github.com/go-quickjs/go-quickjs/internal/vm"
)

// The register tier's differential tests (docs/register-interpreter.md,
// "Raw int32 registers"): the same programs run with the register tier off
// and on, each in runtimes of their own, and every result compared. The
// register code's correctness rests on its verifier for memory and on these
// for meaning: what a raw int32 register holds must be what the Value the
// stack form keeps would be, at every overflow, -0, NaN and shift.

// withRegTier runs f with the register backend and tier set to on, and puts
// back what the environment says after.
func withRegTier(on bool, f func()) {
	compiler.SetRegisterForm(on)
	vm.SetRegTier(on)
	defer func() {
		env := os.Getenv("QJS_REG") != ""
		compiler.SetRegisterForm(env)
		vm.SetRegTier(env)
	}()
	f()
}

// regDiffRun evaluates src in a new runtime and then each of calls, and
// returns each call's result as a string, or its error's.
func regDiffRun(t *testing.T, src string, calls []string) []string {
	t.Helper()
	rt := quickjs.New()
	defer rt.Close()
	if _, err := rt.Eval(src); err != nil {
		t.Fatalf("eval: %v", err)
	}
	out := make([]string, len(calls))
	for i, c := range calls {
		v, err := rt.Eval(c)
		if err != nil {
			out[i] = "error: " + err.Error()
			continue
		}
		out[i] = v.String()
	}
	return out
}

// regDiff runs src and calls with the register tier off, twice, and on, and
// fails where the tier on answers otherwise. A call whose two answers with
// the tier off differ -- a clock, Math.random -- is not compared. It
// returns how many functions the tier gave register code.
func regDiff(t *testing.T, src string, calls []string) int64 {
	t.Helper()
	var off, again, on []string
	withRegTier(false, func() { off = regDiffRun(t, src, calls) })
	withRegTier(false, func() { again = regDiffRun(t, src, calls) })
	before := vm.RegsBuilt()
	withRegTier(true, func() { on = regDiffRun(t, src, calls) })
	built := vm.RegsBuilt() - before
	bad, skipped := 0, 0
	defer func() {
		t.Logf("%d calls compared, %d not (their answers vary), %d functions as register code",
			len(calls)-skipped, skipped, built)
	}()
	for i := range calls {
		if off[i] != again[i] {
			skipped++
			continue
		}
		if off[i] == on[i] {
			continue
		}
		if bad++; bad <= 20 {
			t.Errorf("%s:\n  register tier off: %.300s\n  register tier on:  %.300s", calls[i], off[i], on[i])
		}
	}
	if bad > 20 {
		t.Errorf("and %d more", bad-20)
	}
	return built
}

// regDiffResult is the expression a call is compared by: what it returns
// and what it left in global_res, as text.
func regDiffResult(call string) string {
	return fmt.Sprintf(`(function () { var r = %s; var g = global_res; try { g = JSON.stringify(g) } catch (e) { g = String(g) } return String(r) + " / " + g })()`, call)
}

func TestRegDiffMicrobenchmarks(t *testing.T) {
	if testing.Short() {
		t.Skip("runs every micro-benchmark three times")
	}
	end := strings.LastIndex(microbench, "\nif (typeof scriptArgs === \"undefined\")")
	if end < 0 {
		t.Fatal("microbench.js: no call of main to leave out")
	}
	src := `if (typeof console === "undefined") globalThis.console = { log: function () {} };
if (typeof performance === "undefined") globalThis.performance = { now: function () { return 0 } };
var global_res;
` + microbench[:end+1] + microbenchExtra
	calls := make([]string, 0, len(microWork))
	for _, w := range microWork {
		// A little of each test's work: enough for its loops to turn and
		// its functions to run as each tier runs them.
		n := max(w.n/4096, 1)
		calls = append(calls, regDiffResult(fmt.Sprintf("%s(%d)", w.name, n)))
	}
	if built := regDiff(t, src, calls); built < 100 {
		t.Errorf("only %d functions ran as register code", built)
	}
}

func TestRegDiffV8Suite(t *testing.T) {
	if testing.Short() {
		t.Skip("runs the V8 suite three times")
	}
	dir := os.Getenv("QUICKJS_V8_DIR")
	if dir == "" {
		t.Skip("QUICKJS_V8_DIR names no checkout of the V8 suite (v8bench -fetch)")
	}
	var src strings.Builder
	for _, f := range suites["v8"].files {
		b, err := os.ReadFile(filepath.Join(dir, f))
		if err != nil {
			t.Skip(err)
		}
		src.Write(b)
		src.WriteString("\n")
	}
	// Each benchmark run three times, its own checks of its answers on --
	// Richards' counts, DeltaBlue's projections, Crypto's decryption,
	// NavierStokes' checksum, Splay's tree -- and what it leaves to see
	// compared: the error a check throws, if any.
	src.WriteString(`
function regDiffBench(name) {
	var out = [];
	BenchmarkSuite.suites.forEach(function (s) {
		s.benchmarks.forEach(function (b) {
			if (b.name !== name) return;
			try {
				b.Setup();
				for (var i = 0; i < 3; i++) out.push(String(b.run()));
				b.TearDown();
			} catch (e) { out.push("threw " + e) }
		});
	});
	return out.join(",");
}
var global_res;
`)
	var calls []string
	for _, name := range []string{"Richards", "DeltaBlue", "Encrypt", "Decrypt", "RayTrace", "EarleyBoyer", "RegExp", "Splay", "NavierStokes"} {
		calls = append(calls, fmt.Sprintf("regDiffBench(%q)", name))
	}
	if built := regDiff(t, src.String(), calls); built < 100 {
		t.Errorf("only %d functions ran as register code", built)
	}
}

// TestRegDiffInt32 compares generated integer arithmetic: functions of
// loops over locals that bitwise operators, shifts and | 0 keep int32s,
// and sums, differences and products that leave them -- past 2^31, to -0,
// to NaN -- with shifts by 32 and more, >>> 0, and doubles mixed in.
func TestRegDiffInt32(t *testing.T) {
	rnd := rand.New(rand.NewSource(1))
	consts := []string{"0", "1", "-1", "2", "7", "31", "32", "33", "64", "255", "0x3fff", "0xfffffff",
		"0x7fffffff", "-0x80000000", "0x80000000", "0xffffffff", "4294967296", "-0", "0.5", "-1.5",
		"1e10", "NaN", "Infinity", "65535", "65536", "-65536", "46341", "-46341"}
	ops := []string{"+", "-", "*", "&", "|", "^", "<<", ">>", ">>>", "/", "%"}
	var src strings.Builder
	var calls []string
	vars := []string{"a", "b", "c", "d"}
	operand := func() string {
		if rnd.Intn(3) == 0 {
			return consts[rnd.Intn(len(consts))]
		}
		return vars[rnd.Intn(len(vars))]
	}
	var expr func(depth int) string
	expr = func(depth int) string {
		if depth == 0 || rnd.Intn(4) == 0 {
			return operand()
		}
		e := "(" + expr(depth-1) + " " + ops[rnd.Intn(len(ops))] + " " + expr(depth-1) + ")"
		switch rnd.Intn(6) {
		case 0:
			e = "(" + e + " | 0)"
		case 1:
			e = "(" + e + " >>> 0)"
		case 2:
			e = "(" + e + " & 0x3fff)"
		}
		return e
	}
	cmps := []string{"<", "<=", ">", ">=", "==", "!=", "===", "!=="}
	for i := 0; i < 300; i++ {
		name := fmt.Sprintf("g%d", i)
		fmt.Fprintf(&src, "function %s(a, b, n) {\n\tvar c = %s, d = %s, t = [], out = [];\n", name, operand(), operand())
		fmt.Fprintf(&src, "\tfor (var i = 0; i < 8; i++) t[i] = i * 3;\n")
		fmt.Fprintf(&src, "\tfor (var j = 0; j < n; j++) {\n")
		for k := 0; k < 3; k++ {
			v := vars[rnd.Intn(len(vars))]
			switch rnd.Intn(5) {
			case 0:
				fmt.Fprintf(&src, "\t\t%s = %s;\n", v, expr(3))
			case 1:
				fmt.Fprintf(&src, "\t\t%s = (%s + j) | 0;\n", v, v)
			case 2:
				fmt.Fprintf(&src, "\t\t%s = t[j & 7] + %s;\n", v, expr(2))
			case 3:
				fmt.Fprintf(&src, "\t\tif (%s %s %s) %s++; else %s--;\n", expr(2), cmps[rnd.Intn(len(cmps))], expr(1), v, v)
			default:
				fmt.Fprintf(&src, "\t\tt[(j + %d) & 7] = %s;\n", rnd.Intn(8), expr(2))
			}
		}
		fmt.Fprintf(&src, "\t\tout.push(a, b, c, d);\n\t}\n")
		fmt.Fprintf(&src, "\treturn out.map(function (x) { return Object.is(x, -0) ? \"-0\" : String(x) }).join() + \"|\" + t.join();\n}\n")
		for _, args := range [][2]string{{"1", "2"}, {"0x7ffffff0", "-0x7ffffff0"}, {"-0", "0.25"}, {"46340", "46341"}, {"NaN", "3"}} {
			calls = append(calls, fmt.Sprintf("%s(%s, %s, 6)", name, args[0], args[1]))
		}
	}
	if built := regDiff(t, src.String(), calls); built < 300 {
		t.Errorf("only %d functions ran as register code", built)
	}
}

// TestRegDiffStacksAndNames compares what a stack trace says of register
// frames -- each call's position, its function's name, whether it is a
// construct, its receiver's type -- after the call sites' caches are warm,
// and the names a function's body resolves: its own name shadowed in a
// block, the arguments object an apply other than the built-in is given.
func TestRegDiffStacksAndNames(t *testing.T) {
	src := `
function frames(e) { return e.stack.split("\n").slice(1).map(function (s) { return s.trim() }).join(" | ") }
function thrower() { return null.x }
function calls(n) { return n > 0 ? calls(n - 1) + 1 : thrower() }
function made() { return new Error("m") }
function viaArrow() { return (() => made())() }
function L() { return Error.call(this) }
function M() { this.e = made() }
var o = { m: function () { return made() }, n: function named() { return made() } };
function Foo() {} Foo.prototype.m = function () { return made() };
var a = {}; a.b = function () { return made() };
function self() { return (function g() { { let g = 1 } return typeof g })() }
function selfVar() { return (function g() { var g; return typeof g })() }
function own(a) { var f = { apply: function (t, args) { return args } }; var x = f.apply(null, arguments); a = 2; var y = f.apply(null, arguments); return [x === y, x[0], x.length] }
function warm(f) { for (var i = 0; i < 4; i++) f() }
function stackOf(f) { try { var v = f(); return v && v.stack ? frames(v) : String(v) } catch (e) { return frames(e) } }
`
	var calls []string
	for _, f := range []string{
		"function () { return calls(3) }",
		"made", "viaArrow", "function () { return new L() }", "function () { return new M().e }",
		"function () { return o.m() }", "function () { return o.n() }", "function () { return new Foo().m() }",
		"function () { return a.b() }", "self", "selfVar", "function () { return own(1, 2) }",
	} {
		calls = append(calls, fmt.Sprintf("(function () { var f = %s; warm(function () { try { f() } catch (e) {} }); return stackOf(f) })()", f))
	}
	regDiff(t, src, calls)
}

// TestRegDiffChainsAndClosures compares string chains -- when each part
// is converted, against what the parts after it do, numbers, -0, symbols
// that throw, chains past maxChainParts -- and closures made once in a
// loop or taking what they capture by value.
func TestRegDiffChainsAndClosures(t *testing.T) {
	src := `
var log = [];
function o(n) { return { toString: function () { log.push("s" + n); return "<" + n + ">" }, valueOf: function () { log.push("v" + n); return n } } }
function e(n) { log.push("e" + n); return n }
function chain(a, b, c) { return a + b + "|" + c + e(1) + "|" + o(3) + -0 + 1.5 + e("x") }
function pre(a, b) { return a + b + "s" + o(1) + "" + b }
function long(a) { var s = ""; for (var i = 0; i < 3; i++) s = "a" + a + 1 + a + 2 + a + 3 + a + 4 + a + 5 + a + 6 + a + 7 + a + 8 + a + 9 + a + 10 + a + 11 + a + 12 + a + 13 + a + 14 + a + 15 + a + 16 + a + 17 + a + 18 + a + i; return s }
function sym(a) { try { return "x" + a + Symbol() } catch (x) { return x.constructor.name } }
function reuse(n) { 'use strict'; var t = 0; for (var i = 0; i < n; i++) { var g = function () { return i }; t += g() } return t }
function byValue(a, b, c) { 'use strict'; return () => a + b + c }
function mixed(a, b, c) { 'use strict'; b++; var f = function () { return a + b + c }; b++; return f() }
function run(f) { log = []; var v = f(); return v + " / " + log.join() }
function keys() { var o = { "<1>": 1 }, k = o(1); o[k] += 1; o[k]++; ++o[k]; return o["<1>"] }
function bad(b) { var k = o(2); b[k] += 1 }
function bad2(b) { var k = o(2); b[k]++ }
function upd(t) { t.x++ }
function updLoop(t) { for (var i = 0; i < 2; i++) t[0] += 1 }
function made(t, i) { t[i - 1] += 1; t[i + ""] |= 2; t[` + "`k${i}`" + `] += "s"; t[-i]++; t[i++] += 1; t[!i] = 1; return JSON.stringify(t) + i }
function post(t, i) { var s = []; for (var n = 0; n < 4; n++) s.push(t[i++], t[i--]); return s.join() + " " + typeof i + " " + i }
function id(a, b) { return b }
function none() {}
var ido = { id: function (k) { return k }, none: none, arrow: (k) => k };
class Ret { constructor(k) { return k } }
function ctors(n) { var r = []; for (var i = 0; i < n; i++) { try { r.push(Ret(i)) } catch (e) { r.push(e.name) } r.push(typeof new id(i, i), typeof new Ret({}), typeof new Ret(i)) } return r.join() }
function trivial(n) { var s = []; for (var i = 0; i < n; i++) s.push(id(i, i * 2), id(i), none(i), ido.id(i), ido.id(), ido.none(), ido.arrow(i), id.call(3, 1, i), ido.id.apply("s", [i])); return s.join() }
function madeObj(t) { var k = o(5); t[k + ""] += 1; t["" + e(6)] += 1; return JSON.stringify(t) }
function caught(f, a) { try { f(a) } catch (x) { return x.message + " " + x.stack.split(String.fromCharCode(10))[1] } }
`
	calls := []string{
		`run(function () { return chain(1, 2, o(2)) })`, `run(function () { return chain("a", o(1), 3) })`,
		`run(function () { return pre(1, 2) })`, `run(function () { return pre(o(4), "z") })`,
		`run(function () { return long(7) })`, `run(function () { return long(o(9)) })`,
		`run(function () { return sym(1) })`, `run(function () { return reuse(5) + reuse(1) })`,
		`run(function () { var f = byValue(1, 2, 3); return f() + f() })`, `run(function () { return mixed(1, 2, 3) })`,
		`run(keys)`, `run(function () { return caught(bad, null) })`, `run(function () { return caught(bad2, undefined) })`,
		`caught(upd, null)`, `caught(upd, undefined)`, `caught(updLoop, null)`,
		`made({}, 3)`, `run(function () { return madeObj({}) })`, `run(function () { return caught(madeObj, null) })`,
		`caught(made, null)`, `caught(made, undefined)`,
		`trivial(5)`, `ctors(4)`, `post([1, 2, , 4, 5], 0)`, `post([1, 2, 3], "1")`, `post("abcdef", 1)`, `post({ 1: "a", 2: "b" }, 1)`,
		`run(function () { return post([7, 8, 9], o(1)) })`, `caught(post, null)`, `post([1, 2], 1.5)`, `post([1, 2], -1)`,
	}
	for i := range calls {
		calls[i] = fmt.Sprintf("(function () { var r; for (var k = 0; k < 4; k++) r = %s; return r })()", calls[i])
	}
	regDiff(t, src, calls)
}

// TestRegDiffSites compares the register operations given a site or a fast
// path of their own after the stage's first measurements, each with its
// slow paths.
func TestRegDiffSites(t *testing.T) {
	src := `
function grow(n, m) { var out = []; for (var j = 0; j < m; j++) { var a = []; for (var i = 0; i < n; i++) a.push(i, " "); out.push(a.length, a[0], a[a.length - 1]); if (j == 2) a.x = 1; } return out.join() }
function fresh() { var a = [], b = []; a.push(1); return [a === b, a.length, b.length, Array.isArray(b), Object.getPrototypeOf(b) === Array.prototype].join() }
function negs(x) { var r = []; r.push(x === -0, x !== -1, x + -1, x > -2, x < -2, x <= -32768, x >= -32769, x - -1, x & -1, x | -2, x ^ -3, x << -31, x >> -1, x >>> -1, -2147483648, x === -2147483648, Object.is(x * -0, -0), 1 / -0, [-1, -0, x].map(function (v) { return 1 / v }).join("/"), "" + -1 + x + -0); var o = { a: x }; o.a += -1; r.push(o.a); switch (x) { case -1: r.push("m1"); break; case -0: r.push("z"); break; case -32768: r.push("big"); break; case -2147483648: r.push("min"); break; default: r.push("d") } return r.join() }
function konst() { return -1 }
function konstF() { return 1.5 }
function konstT() { return true }
function konstN() { return null }
function konsts(n) { var s = 0, t = []; for (var i = 0; i < n; i++) { s += konst() + konstF(i) + konst(1, 2); t.push(konstT(), konstN(), konst.call("x"), new konst() instanceof konst) } return s + t.join() }
function upd(t, k, j) { t[0] += j; t[1]++; t[2]--; t[k] += j; t[k]++; t[k] -= 0; t[k + 1] += 2; t[3] += -1; t[k] += "s"; return JSON.stringify(t) + typeof t[0] }
function updLoop(t, n) { for (var i = 0; i < n; i++) { t[i & 3] += i; t[i & 1]++; t[(i & 1) + 2] += 1.5 } return JSON.stringify(t) }
function updLog() { var log = [], k = { toString: function () { log.push("k"); return "a" } }, o = { get a() { log.push("get"); return 1 }, set a(v) { log.push("set " + v) } }; o[k] += 2; o[k]++; var j = 3; o[k] += j; return log.join() }
function updBad(t, k) { try { t[k] += 1; return "ok " + JSON.stringify(t) } catch (e) { return e.constructor.name + ": " + e.message } }
function updStr(s) { var t = ["5", 2n, null, undefined, true, {}]; for (var i = 0; i < t.length; i++) { try { t[i]++ } catch (e) { t[i] = e.name } } var u = ["5", null]; u[0] += 1; u[1] += 1; return t.map(String).join() + " " + u.join() }
function updHoles() { var t = [1, , 3]; t[1] += 1; t[5]++; var f = Object.freeze([1, 2]); f[0] += 1; var a = (function () { return arguments })(1, 2); a[0] += 5; return JSON.stringify(t) + f.join() + a[0] + a.length }
function updStrict() { 'use strict'; var f = Object.freeze([1, 2]); try { f[0] += 1 } catch (e) { return e.name } }
function updStack(k) { 'use strict'; var f = Object.freeze([1]), o = { get 0() { return 1 }, set 0(v) { throw new Error("s") } }, out = []; var j = 2;
  try { f[k] += j } catch (e) { out.push(e.stack.split(String.fromCharCode(10))[1]) }
  try { o[k]++ } catch (e) { out.push(e.stack.split(String.fromCharCode(10))[2]) }
  try { o[0] += 1 } catch (e) { out.push(e.stack.split(String.fromCharCode(10))[2]) }
  try { null[k] += 1 } catch (e) { out.push(e.stack.split(String.fromCharCode(10))[1]) }
  return out.join(" | ") }
function updTyped() { var t = new Int8Array(3); t[0] += 127; t[0]++; t[1] -= 1; t[2] += 1.7; return t.join() }
function sw(x) { switch (x) { case "a": return 1; case "bb": return 2; case 3: return 3; case -0: return "z"; case "a": return "dup"; case true: return "t"; case null: return "n"; case false: return "f"; case NaN: return "nan"; default: return "d"; case "ccc": return "c" } }
function swFall(x) { var r = []; switch (x) { case 1: r.push(1); case 2: r.push(2); break; default: r.push("d"); case 3: r.push(3) } return r.join() }
function swMixed(x, y) { var r = []; switch (x) { case "k": r.push("k"); break; case y: r.push("y"); break; case "m": r.push("m"); break; case 7: r.push(7) } return r.join() }
function swNums(x) { switch (x) { case 100: return "a"; case 2.5: return "b"; case -3: return "c"; case 1e9: return "e"; case 4: return "f"; case 5: return "g"; case 6: return "h"; case 7: return "i"; case 8: return "j"; case 9: return "k"; default: return "-" } }
function swDense(x) { switch (x) { case 0: return 0; case 1: return 1; case 2: return 2; case 4: return 4; case 1: return "dup" } return "none" }
function swType(x) { switch (typeof x) { case "number": return 1; case "string": return 2; case "undefined": return 3; case "object": return 4; case "function": return 5; default: return 6 } }
function swRope(n) { var s = ""; for (var i = 0; i < n; i++) s += "b"; return sw(s) + sw("a" + s.slice(1)) + sw("c" + "cc") }
function swAll() { var out = [], xs = ["a", "bb", 3, 0, -0, true, false, null, undefined, NaN, "3", 3.5, "ccc", "cc", {}, [3], 4, 2.5, -3, 1e9, 9, 10]; for (var i = 0; i < xs.length; i++) out.push(sw(xs[i]) + "/" + swFall(xs[i]) + "/" + swMixed(xs[i], 3) + "/" + swNums(xs[i]) + "/" + swDense(xs[i]) + "/" + swType(xs[i])); return out.join() + swRope(2) + swRope(3) + swRope(1) }
function swOne(x) { switch (x) { case "only": return 1 } return 0 }
function swNoDefault(x) { var r = 0; switch (x) { case "p": r = 1; break; case "q": r = 2 } return r + swOne(x) + swOne("on" + "ly") }
function tof(x) { var r = []; for (var i = 0; i < typeNames.length; i++) { var t = typeNames[i]; r.push((typeof x === "undefined") + "" + (typeof x == "object") + (typeof x !== "boolean") + (typeof x != "number") + ("string" === typeof x) + (typeof x === "symbol") + (typeof x === "bigint") + (typeof x === "function") + (typeof x === "nope") + (typeof x === t)); if (typeof x === "function") r.push("f"); else if (typeof x !== "object") r.push("no") } return r.join() }
var typeNames = ["undefined", "object", "boolean", "number", "string", "symbol", "bigint", "function"];
function tofAll() { var p = new Proxy(function () {}, {}), q = new Proxy({}, {}); class K {} var xs = [1, "s", function () {}, null, {}, undefined, true, Math.max, Symbol(), 1n, p, q, K, [], new Date(0), (function () { return arguments })(), async function () {}, function* () {}, () => 1, Function.prototype.bind.call(function () {}), Object.create(null)]; var r = []; for (var i = 0; i < xs.length; i++) r.push(tof(xs[i])); try { tofTdz() } catch (e) { r.push(e.name) } r.push(xs.map(function (v) { var t = typeof v; return t + (t === typeof v) + swType(v) }).join()); return r.join("|") }
function tofTdz() { if (typeof z === "number") return 1; let z = 1; return 2 }
function kept(n) { var all = []; for (var j = 0; j < n; j++) { var a = []; for (var i = 0; i < 300; i++) a.push(j); all.push(a) } return all.map(function (a) { return a.length + ":" + a[299] }).join() }
`
	calls := []string{
		`negs(-1)`, `negs(0)`, `negs(-0)`, `negs(-2)`, `negs(-32768)`, `negs(-2147483648)`, `negs("-1")`, `negs(undefined)`, `negs(5)`, `negs(-1.5)`, `negs({ valueOf: function () { return -1 } })`,
		`konsts(5)`,
		`upd([1, 2, 3, 4, 5], 0, 2)`, `upd([1, 2, 3, 4, 5], "1", 2)`, `upd({}, "x", 1)`, `upd([1, 2, 3, 4, 5], 1.5, 2)`, `upd([1, 2, 3, 4, 5], -1, 2)`,
		`updLoop([0, 0, 0, 0], 50)`, `updLog()`, `updBad(null, 1)`, `updBad(undefined, "x")`, `updBad(null, { toString: function () { throw 1 } })`,
		`updBad([1], { toString: function () { return "0" } })`, `updBad("abc", 0)`, `updBad(5, 0)`, `updStr()`, `updHoles()`, `updStrict()`, `updTyped()`, `swAll()`, `tofAll()`, `swNoDefault("p")`, `swNoDefault("q")`, `swNoDefault(1)`, `updStack(0)`, `updStack("0")`,
		`grow(300, 6)`, `grow(3, 4)`, `grow(0, 3)`, `grow(700, 3)`, `fresh()`, `kept(4)`,
	}
	for i := range calls {
		calls[i] = fmt.Sprintf("(function () { var r; for (var k = 0; k < 4; k++) r = %s; return r })()", calls[i])
	}
	regDiff(t, src, calls)
}

// TestRegDiffTyped compares typed arrays' elements read and written by
// get_index, set_index and their immediate forms, out of line
// (regTypedGet, regTypedSet), with the tree's, and updates that write a
// number's word alone.
func TestRegDiffTyped(t *testing.T) {
	src := `
var kinds = [Int8Array, Uint8Array, Uint8ClampedArray, Int16Array, Uint16Array, Int32Array, Uint32Array, Float32Array, Float64Array];
var vals = [0, -0, 1, -1, 1.5, -1.5, 127, 128, 255, 256, -129, 65535, 65536, 2147483647, 2147483648, -2147483649, 4294967296, NaN, Infinity, -Infinity, 0.1, 1e300, "7", "x", true, null, undefined, { valueOf: function () { return 3 } }];
function show(v) { return Object.is(v, -0) ? "-0" : String(v) }
function rw(K) { var t = new K(4), out = []; for (var i = 0; i < vals.length; i++) { t[0] = vals[i]; t[1] = t[0]; var k = 2; t[k] = vals[i]; out.push(show(t[0]), show(t[1]), show(t[k])) } return out.join() }
function rwAll() { var r = []; for (var i = 0; i < kinds.length; i++) r.push(rw(kinds[i])); return r.join("|") }
function keys(K) { var t = new K([10, 20, 30]), out = [], ks = [0, 1, 2, 3, -1, -0, 0.5, NaN, "1", "01", Infinity, 2147483648, 4294967295]; for (var i = 0; i < ks.length; i++) { var k = ks[i]; out.push(show(t[k])); t[k] = 9; out.push(show(t[k]), t.length) } out.push(t[3], t[7], t[-1]); t[3] = 1; t[7] = 1; out.push(Object.keys(t).join("/")); return out.join() }
function keysAll() { var r = []; for (var i = 0; i < kinds.length; i++) r.push(keys(kinds[i])); return r.join("|") }
function big() { var t = new BigInt64Array(2), u = new BigUint64Array(2), out = []; t[0] = 5n; t[1] = -1n; u[0] = -1n; out.push(t[0], t[1], u[0], u[1]); try { t[0] = 1 } catch (e) { out.push(e.name) } try { t[1] = "x" } catch (e) { out.push(e.name) } t[1] = "12"; out.push(t[1]); return out.join() }
function proto() { Object.defineProperty(Object.prototype, "5", { get: function () { return "got" }, set: function (v) { throw new Error("set") }, configurable: true }); var t = new Int32Array(2), r = []; r.push(t[5], t[0]); t[5] = 1; r.push(t[5]); var a = [1, 2]; r.push(a[5]); delete Object.prototype[5]; return r.join() }
function detach() { var b = new ArrayBuffer(8), t = new Int16Array(b), r = []; t[0] = 7; r.push(t[0]); b.transfer(); r.push(t[0], t.length); t[0] = 9; r.push(t[0]); return r.join() }
function resizable() { var b = new ArrayBuffer(4, { maxByteLength: 16 }), t = new Uint8Array(b), f = new Uint8Array(b, 0, 2), r = []; for (var i = 0; i < 4; i++) t[i] = i + 1; r.push(t[3], f[1], f[2]); b.resize(16); t[10] = 5; r.push(t[10], t.length); b.resize(1); r.push(t[3], f[1], f[0], t.length); t[3] = 1; f[0] = 2; r.push(t[0], f[0]); return r.join() }
function viaProxy() { var t = new Float64Array(2), p = new Proxy(t, { get: function (o, k) { return k === "0" ? "p" + o[k] : o[k] }, set: function (o, k, v) { o[k] = v * 2; return true } }); p[0] = 3; p[1] = 4; return [p[0], p[1], t[0], t[1]].join() }
function frozen() { 'use strict'; var r = [], e = Object.freeze(new Int8Array(0)); try { e[0] = 1 } catch (x) { r.push(x.name) } r.push(e[0]); var t = new Int8Array(2); Object.preventExtensions(t); t[1] = 3; r.push(t[1]); try { Object.freeze(t) } catch (x) { r.push(x.name) } return r.join() }
function sloppyOut() { var t = new Int8Array(2); t[5] = 1; t[-1] = 2; t["x"] = 3; return [t[5], t[-1], t.x, t.length].join() }
function strictOut() { 'use strict'; var t = new Int8Array(2), r = []; t[5] = 1; r.push(t[5]); try { t[0] = Symbol() } catch (e) { r.push(e.name + " " + e.stack.split(String.fromCharCode(10))[1]) } return r.join() }
function loop(K, n) { var t = new K(16), s = 0; for (var j = 0; j < n; j++) { for (var i = 0; i < 16; i++) t[i] = i * j - 3.25; s += t[0] + t[1] + t[15] } return show(s) }
function loopAll() { var r = []; for (var i = 0; i < kinds.length; i++) r.push(loop(kinds[i], 40)); return r.join() }
function words() { var a = [1, -0, 0.5, 2], o = { x: 1, y: -0 }; var z = -0; a[0] += NaN; a[1] += z; a[2] += 0.25; a[3] += -0; o.x += NaN; o.y += z; var k = 2; a[k] += 1e308; a[k] += 1e308; return [a.map(show).join(), show(o.x), show(o.y)].join() }
`
	calls := []string{
		`rwAll()`, `keysAll()`, `big()`, `proto()`, `detach()`, `resizable()`, `viaProxy()`, `frozen()`, `sloppyOut()`, `strictOut()`, `loopAll()`, `words()`,
	}
	for i := range calls {
		calls[i] = fmt.Sprintf("(function () { var r; for (var k = 0; k < 4; k++) r = %s; return r })()", calls[i])
	}
	regDiff(t, src, calls)
}

// TestRegDiffElemWrites compares element writes on arrays that the fast
// path does not answer (regSetMiss) with the tree's: appends at the
// length, holes filled, a shared literal's element, a frozen or
// non-extensible array, a read-only length, a setter or a read-only
// element up the chain, sparse arrays, an arguments object and keys that
// are no index.
func TestRegDiffElemWrites(t *testing.T) {
	src := `
function show(a) { return JSON.stringify(a) + a.length + Object.keys(a).join("/") }
function app(n) { var a = []; for (var i = 0; i < n; i++) a[i] = i; var b = []; for (var i = 0; i < n; i++) b[b.length] = "s" + i; return show(a) + show(b) }
function holes() { var a = new Array(6); for (var i = 5; i >= 0; i -= 2) a[i] = i; var b = [1, , 3]; b[1] = 2; var c = [1, 2, 3]; delete c[1]; c[1] = 9; return show(a) + show(b) + show(c) }
function lit() { var r = []; for (var j = 0; j < 3; j++) { var a = [1, 2, 3]; a[j] = 9; a[3] = 4; r.push(show(a)) } return r.join() }
function locked() { 'use strict'; var r = [], f = Object.freeze([1]), p = Object.preventExtensions([1, 2]), s = Object.seal([1]), l = [1, 2]; Object.defineProperty(l, "length", { writable: false }); var cases = [[f, 0], [f, 1], [p, 1], [p, 2], [s, 0], [s, 1], [l, 1], [l, 2]]; for (var i = 0; i < cases.length; i++) { try { cases[i][0][cases[i][1]] = 7; r.push("ok") } catch (e) { r.push(e.name) } } return r.join() + show(f) + show(p) + show(s) + show(l) }
function sloppyLocked() { var f = Object.freeze([1]), l = [1]; Object.defineProperty(l, "length", { writable: false }); f[1] = 2; f[0] = 3; l[1] = 2; return show(f) + show(l) }
function chain() { var log = []; Object.defineProperty(Array.prototype, "3", { set: function (v) { log.push("set" + v) }, configurable: true }); Object.defineProperty(Object.prototype, "5", { value: "ro", writable: false, configurable: true }); try { var a = [0, 1, 2]; a[3] = 3; a[3] = 4; var b = [0, 1, 2, , 4]; b[5] = 5; b[6] = 6; return show(a) + show(b) + log.join() } finally { delete Array.prototype[3]; delete Object.prototype[5] } }
function sparse() { var a = []; a[100] = 1; a[0] = 0; a[1] = 1; var b = [1, 2]; b[4294967294] = 3; b[4294967295] = 4; b[2] = 5; return show(a).length + " " + a[100] + b.length + Object.keys(b).join("/") }
function args() { var a = arguments; a[0] = 1; a[a.length] = 2; a[5] = 3; return JSON.stringify(a) + a.length }
function keys() { var a = [1], ks = [-1, -0, 0.5, NaN, "1", "x", 1, 2, 4294967296]; for (var i = 0; i < ks.length; i++) a[ks[i]] = i; return show(a) + Object.getOwnPropertyNames(a).join("/") }
function proxied() { var log = [], a = [1], p = new Proxy(a, { set: function (o, k, v) { log.push(k); o[k] = v; return true } }); for (var i = 0; i < 3; i++) p[p.length] = i; return show(a) + log.join() }
`
	calls := []string{
		`app(40)`, `holes()`, `lit()`, `locked()`, `sloppyLocked()`, `chain()`, `sparse()`, `args(5, 6)`, `keys()`, `proxied()`,
	}
	for i := range calls {
		calls[i] = fmt.Sprintf("(function () { var r; for (var k = 0; k < 4; k++) r = %s; return r })()", calls[i])
	}
	regDiff(t, src, calls)
}

// TestRegDiffLength compares get_length with the tree's: strings flat,
// built and cut, with surrogates, and what is no string or array -- a
// String object, a function, an object with a getter, null.
func TestRegDiffLength(t *testing.T) {
	src := `
function lens() { var s = "", r = []; for (var i = 0; i < 20; i++) { s += "a\ud83d\ude00"; r.push(s.length) } var c = s.slice(3, 11), e = ""; r.push(c.length, e.length, "x".length, (s + c).length); var xs = [new String("abc"), function (a, b) {}, { get length() { return "g" } }, { length: -1 }, [1, , 3], 5, true]; for (var i = 0; i < xs.length; i++) { var x = xs[i]; r.push(x.length) } try { var n = null; r.push(n.length) } catch (e) { r.push(e.name) } return r.join() }
`
	calls := []string{`lens()`}
	for i := range calls {
		calls[i] = fmt.Sprintf("(function () { var r; for (var k = 0; k < 4; k++) r = %s; return r })()", calls[i])
	}
	regDiff(t, src, calls)
}

// TestRegDiffAppends compares appends to a local (append, str_freeze)
// with the tree's: strings kept by an array, an object, a call, a return
// or a global between appends, which must not see the appends after; a
// read of the length, which keeps nothing; numbers, objects whose
// conversion runs code or throws, symbols, and a local that holds no
// string; strings long enough to be grown in place many times over.
func TestRegDiffAppends(t *testing.T) {
	src := `
var held;
function keepAll(n) { var out = "", keep = []; for (var i = 0; i < n; i++) { out += "ab"; if (i % 3 == 0) keep.push(out) } return keep.map(function (s) { return s.length }).join() + out.length }
function inObject(n) { var out = "", o = {}; for (var i = 0; i < n; i++) { out += "x"; if (i == 5) o.s = out } return o.s + " " + out.length }
function passed(n) { var out = ""; for (var i = 0; i < n; i++) { out += "y"; if (i == 7) held = out } out += "z"; return held + " " + out.length }
function early(n) { var out = ""; for (var i = 0; i < n; i++) { out += "<" + i + ">"; if (i == n - 3) return out } return "" }
function retKept() { var a = early(9), b = early(9); a += "!"; return a + " " + b }
function lengths(n) { var out = "", c = 0; for (var i = 0; i < n; i++) { out += "<x>"; c += out.length } return c + " " + out.length }
function nums(n) { var out = ""; for (var i = 0; i < n; i++) { out += i; out += ","; out += i / 4; out += -0 } return out }
function conv() { var log = [], out = "s"; var o = { toString: function () { log.push("ts"); return "O" }, valueOf: function () { log.push("vo"); return 7 } }; out += o; out += null; out += undefined; out += true; out += 1n; return out + log.join() }
function throws() { var out = "a", r = []; var bad = { valueOf: function () { throw new Error("no") } }; try { out += bad } catch (e) { r.push(e.message, e.stack.split(String.fromCharCode(10))[1]) } out += "b"; try { out += Symbol() } catch (e) { r.push(e.name) } out += "c"; return out + r.join() }
function notString(n) { var out = 0; for (var i = 0; i < n; i++) out += i; var u; u += "x"; var o = {}; o += "y"; return out + " " + u + " " + o }
function long(n) { var out = "", keep = []; for (var i = 0; i < n; i++) { out += "0123456789abcdef"; if (i % 50 == 0) keep.push(out) } var t = 0; for (var j = 0; j < keep.length; j++) t += keep[j].length * (j + 1) + keep[j].charCodeAt(keep[j].length - 1); return t + " " + out.length + " " + out.slice(-20) }
function surrogate() { var out = "", hi = String.fromCharCode(0xd83d), lo = String.fromCharCode(0xde00), k = []; for (var i = 0; i < 6; i++) { out += hi; k.push(out); out += lo } return out.length + " " + k.map(function (s) { return s.length + ":" + s.charCodeAt(s.length - 1) }).join() + " " + out.codePointAt(0) }
function guarded(n) { var t = "", r = 0, e = "&"; for (var l = 0; l < n; l++) { r !== l && (t += "(" + l + ")"), r = l + 1, t += e; l % 3 || (t += "|"); l % 5 && (t += "#") } var k = t; t += "end"; return k.length + " " + t }
function cut(n) { var out = "", parts = []; for (var i = 0; i < n; i++) { out += "abc"; if (i % 4 == 0) parts.push(out.slice(1)) } out += "z"; return parts.join("|").length + " " + out.length }
`
	calls := []string{
		`keepAll(40)`, `inObject(20)`, `passed(20)`, `retKept()`, `lengths(300)`, `nums(30)`, `conv()`, `throws()`, `notString(10)`, `long(400)`, `surrogate()`, `cut(60)`, `guarded(20)`,
	}
	for i := range calls {
		calls[i] = fmt.Sprintf("(function () { var r; for (var k = 0; k < 4; k++) r = %s; return r })()", calls[i])
	}
	regDiff(t, src, calls)
}

// TestRegDiffLeafRun compares short bodies of numbers and upvalues that
// register code runs without a frame (regLeafRun) with the tree's, where
// they run and where they give up: what is no number, a store to an
// upvalue after the operation that gave up, too few arguments, errors and
// their stack lines.
func TestRegDiffLeafRun(t *testing.T) {
	src := `
function show(v) { return Object.is(v, -0) ? "-0" : typeof v === "symbol" ? "sym" : String(v) }
function counter(n) { var c = 0; function inc() { c++ } function dec(a) { c-- } function add(a) { c += a } function sub(a, b) { c = c - a * b } for (var i = 0; i < n; i++) { inc(); add(i); sub(i, 2); dec() } return show(c) }
function mixed(xs) { var s = 0, out = []; xs.forEach((x) => { s += x }); out.push(show(s)); var p = 1; xs.forEach(function (x, i) { p = p * 1 + i }); out.push(show(p)); out.push(show(xs.reduce((a, x) => a + x + 1, 0))); out.push(xs.map((x) => x * 2).map(show).join("/")); return out.join() }
function strs() { var s = "", n = 0; function f(a) { s += a } function g(a) { n += a } for (var i = 0; i < 40; i++) { f(i); g(i % 3 ? i : "x") } return s.length + " " + n }
function args() { var u = 0; function f(a, b) { u = a + b } f(1); var r1 = show(u); f(1, 2, 3); var r2 = show(u); f(); return r1 + r2 + show(u) }
function negs() { var z = 0; function f(a) { z = a * -1 } f(0); var r = show(z); f(-0); r += show(z); f(NaN); r += show(z); f(Infinity); return r + show(z) }
function rets() { var k = 3; function a(x) { return x + k } function b(x) { var t = k; return t } function c() { return } function d(x) { k = x; return x * 2 } return [a(1), a("s"), a(), b(), c(), d(4), k, d("q"), k].map(show).join() }
function tdz() { var r = []; try { f() } catch (e) { r.push(e.name) } let q = 1; function f() { q += 1 } f(); r.push(q); return r.join() }
function konst() { const c = 1; function f() { c = 2 } try { f() } catch (e) { return e.name } return "none" }
function sym() { var s = 0; function f(a) { s += a } try { f(Symbol()) } catch (e) { return e.name + " " + show(s) + " " + e.stack.split(String.fromCharCode(10))[1] } }
function valueOf() { var s = 0, log = []; function f(a) { s += a } f({ valueOf: function () { log.push("v"); return 5 } }); f(2); return show(s) + log.join() }
function bigs() { var s = 0n; function f(a) { s += a } f(1n); f(2n); try { f(1) } catch (e) { return show(s) + e.name } }
function many() { var s = 0; function f(a) { s += a } for (var i = 0; i < 300; i++) f(i & 1 ? "a" : i); for (var j = 0; j < 50; j++) f(j); return s.length + show(s).slice(0, 12) }
function methods() { var n = 0, o = { m: function (a) { n += a }, k: function () { return 7 } }; for (var i = 0; i < 10; i++) { o.m(i); o.m.call(null, 1) } return show(n) + o.k() }
function ctor() { var n = 0; function F(a) { n += a } var x = new F(2); return show(n) + (x instanceof F) }
function held(xs) { var calls = 0, last = 0, both = 0; var r = xs.reduce((acc, x) => { calls++; last = x; return acc + x; }, 0); function f(a) { both = a; both2 = both; return a * 1 } var both2 = 0; for (var i = 0; i < xs.length; i++) f(xs[i]); function g(a) { calls = calls + 1; last = a; calls = calls * 2; return a - 1 } var gs = xs.map(g); return [show(r), calls, show(last), show(both), show(both2), gs.map(show).join("/")].join() }
`
	calls := []string{
		`counter(50)`, `held([1, 2, 3])`, `held([1, "s", 3])`, `held([2, Symbol(), 1].slice(0, 1))`, `held([{ valueOf: function () { return 4 } }, 1])`, `held([1n, 2n])`, `mixed([1, 2, 3, 4.5, -0])`, `mixed([1, "2", 3])`, `mixed([NaN, 1])`, `mixed([])`, `strs()`, `args()`, `negs()`, `rets()`, `tdz()`, `konst()`, `sym()`, `valueOf()`, `bigs()`, `many()`, `methods()`, `ctor()`,
	}
	for i := range calls {
		calls[i] = fmt.Sprintf("(function () { var r; for (var k = 0; k < 4; k++) r = %s; return r })()", calls[i])
	}
	regDiff(t, src, calls)
}

// TestRegDiffObjectLiterals compares object literals made at once
// (new_object_lit) with the tree's: key order with index-like keys,
// values that throw or read the binding the literal goes to, many keys,
// sites whose objects grow afterwards, and literals the form leaves to
// define_field.
func TestRegDiffObjectLiterals(t *testing.T) {
	src := `
function desc(o) { return JSON.stringify(Object.getOwnPropertyNames(o).map(function (k) { var d = Object.getOwnPropertyDescriptor(o, k); return [k, typeof d.value === "number" && Object.is(d.value, -0) ? "-0" : d.value, d.writable, d.enumerable, d.configurable] })) + Object.isExtensible(o) + (Object.getPrototypeOf(o) === Object.prototype) }
function lits(j) { var a = { x: j, y: j + 1, s: "k", t: true, n: null, u: undefined, z: -0 }; var b = { "1": j, "0": 2, b: 3, "01": 4, "-0": 5, "4294967295": 6, "4294967294": 7, a: 8 }; var c = { get: 1, set: 2, "__proto_": 3, constructor: 4, toString: 5 }; return desc(a) + desc(b) + desc(c) }
function grow(n) { var out = []; for (var i = 0; i < n; i++) { var o = { a: i, b: i * 2 }; if (i & 1) o.c = i; if (i % 3 == 0) delete o.a; out.push(JSON.stringify(o)) } return out.join() }
function many(j) { var o = { k0: j, k1: 1, k2: 2, k3: 3, k4: 4, k5: 5, k6: 6, k7: 7, k8: 8, k9: 9, k10: 10, k11: 11, k12: 12, k13: 13, k14: 14, k15: 15, k16: 16, k17: 17, k18: 18, k19: 19 }; o.k20 = 20; return desc(o) }
function thrower() { var o = 1, log = []; try { o = { a: log.push("a"), b: (function () { throw new Error("x") })(), c: log.push("c") } } catch (e) { log.push(e.message + " " + e.stack.split(String.fromCharCode(10))[1]) } return String(o) + log.join() }
function self() { var o = { a: 1 }; o = { a: o.a + 1, b: o, c: typeof o }; return o.a + " " + o.b.a + " " + o.c }
function order() { var log = []; function v(k) { log.push(k); return k } var o = { b: v("b"), a: v("a"), 2: v(2), 1: v(1) }; return log.join() + " " + Object.keys(o).join() }
function others(j) { var k = "dyn"; var o = { [k]: 1, a: 2 }, p = { a: 1, a: 2 }, q = { __proto__: null, a: 1 }, g = { get a() { return j } }, m = { m() { return 3 } }, sp = { ...p, z: 1 }; return [desc(o), desc(p), Object.getPrototypeOf(q), g.a, m.m(), desc(sp)].join() }
function frozenProto() { Object.defineProperty(Object.prototype, "ro", { value: 1, writable: false, configurable: true }); Object.defineProperty(Object.prototype, "acc", { set: function (v) { throw new Error("setter") }, configurable: true }); try { var o = { ro: 2, acc: 3 }; return desc(o) } finally { delete Object.prototype.ro; delete Object.prototype.acc } }
function kept(n) { var out = []; for (var i = 0; i < n; i++) { var o = { a: i, b: "s" + i, c: i / 2 }; if (i % 4 == 1) o.d = 1; if (i % 5 == 2) delete o.b; if (i % 7 == 3) Object.freeze(o); out.push(desc(o)) } return out.join() }
function wide() { var src = "return {"; for (var i = 0; i < 300; i++) src += "p" + i + ": " + i + ","; src += "}"; var o = Function(src)(); return Object.keys(o).length + " " + o.p299 }
`
	calls := []string{
		`lits(1)`, `lits(-0)`, `grow(20)`, `many(3)`, `thrower()`, `self()`, `order()`, `others(5)`, `frozenProto()`, `wide()`, `kept(30)`,
	}
	for i := range calls {
		calls[i] = fmt.Sprintf("(function () { var r; for (var k = 0; k < 4; k++) r = %s; return r })()", calls[i])
	}
	regDiff(t, src, calls)
}

// TestRegDiffCharCodeAt compares s.charCodeAt(i) (char_code_at) with the
// tree's: positions inside, at and past the ends, fractions, NaN, -0 and
// infinities; surrogate halves; arguments that are no number, whose
// conversion runs code; receivers that are no string -- a String object,
// a number, null, an object with a method of its own that throws; the
// built-in replaced by a function and by a getter, and put back; and the
// stacks and messages of what throws, placed at the read or the call.
func TestRegDiffCharCodeAt(t *testing.T) {
	src := `
function codes(s) { var r = [], i; for (i = -2; i < s.length + 2; i++) r.push(s.charCodeAt(i)); var xs = [0.5, -0.5, -0, NaN, Infinity, -Infinity, 1.99, 2147483648]; for (var k = 0; k < xs.length; k++) { i = xs[k]; r.push(s.charCodeAt(i)) } return r.join() }
function args(s) { var r = [], xs = [undefined, "1", null, true, { valueOf: function () { r.push("v"); return 2 } }, [1]], i; for (var k = 0; k < xs.length; k++) { i = xs[k]; r.push(s.charCodeAt(i)) } return r.join() }
function recv(xs) { var r = [], i = 1; for (var k = 0; k < xs.length; k++) { var s = xs[k]; try { r.push(s.charCodeAt(i)) } catch (e) { r.push(e.name + ": " + e.message + " @" + String(e.stack).split("\n").slice(1, 3).join(";")) } } return r.join("|") }
function own() { var o = { charCodeAt: function (i) { return "own" + i } }, t = { charCodeAt: function (i) { throw new Error("t" + i) } }, n = { charCodeAt: 7 }; return recv([o, t, n, new String("ab"), 5, null, undefined, "xy"]) }
function patched() { var r = [], s = "abc", i = 1, f = String.prototype.charCodeAt; String.prototype.charCodeAt = function (k) { return "p" + k + this }; r.push(s.charCodeAt(i)); String.prototype.charCodeAt = f; r.push(s.charCodeAt(i)); Object.defineProperty(String.prototype, "charCodeAt", { get: function () { r.push("g"); return f }, configurable: true }); r.push(s.charCodeAt(i)); Object.defineProperty(String.prototype, "charCodeAt", { value: f, writable: true, configurable: true }); r.push(s.charCodeAt(i)); String.prototype.charCodeAt = f.bind("zz"); r.push(s.charCodeAt(i)); String.prototype.charCodeAt = f; return r.join() }
function viaThis() { String.prototype.cc = function (i) { return this.charCodeAt(i) }; var r = "hey".cc(1); delete String.prototype.cc; return r }
function scan(s) { var h = 0, i; for (i = 0; i < s.length; i++) h = (h * 31 + s.charCodeAt(i)) | 0; return h }
`
	calls := []string{`codes("ab\ud83d\ude00c")`, `codes("")`, `args("hello")`, `own()`, `patched()`, `viaThis()`,
		`scan("the quick brown fox \u00e9\u4e2d")`}
	for i := range calls {
		calls[i] = fmt.Sprintf("(function () { var r; for (var k = 0; k < 4; k++) r = %s; return r })()", calls[i])
	}
	regDiff(t, src, calls)
}

// TestRegDiffRegExpLiterals compares RegExp literals (new_regexp) with the
// tree's: a new object at each evaluation, with its own lastIndex; a
// literal given to replace, split, match and test, whose RegExp the tree
// may have back; flags; and a literal's object kept and compared.
func TestRegDiffRegExpLiterals(t *testing.T) {
	src := `
function esc(s) { return s.indexOf("<") < 0 ? s : s.replace(/</g, "&lt;") }
function lits() { var r = [], k = [], i; for (i = 0; i < 4; i++) { var x = /a(b)?/g; k.push(x); r.push(x.lastIndex, x.exec("xabab") + "", x.lastIndex) } r.push(k[0] === k[1], k[0].source, k[0].flags, /x/imsu.flags, /\u{1F600}/u.test("\ud83d\ude00")); return r.join() }
function uses(s) { return [esc(s), s.split(/,\s*/).length, (s.match(/\d+/g) || []).join("+"), /^\w/.test(s), s.replace(/(\d)/g, function (m, d) { return d * 2 })].join("|") }
`
	calls := []string{`lits()`, `uses("a <b>, 12, c3 <")`, `uses("plain 7")`, `uses("")`}
	for i := range calls {
		calls[i] = fmt.Sprintf("(function () { var r; for (var k = 0; k < 4; k++) r = %s; return r })()", calls[i])
	}
	regDiff(t, src, calls)
}

// TestRegDiffRegExpLiteralCalls compares a literal's exec and test
// (lit_exec, lit_test, call_lit) and a literal given to replace, split and
// match (call_arg_lit) with the tree's where the RegExp may be given back
// and where it must not be: a sticky or global literal's lastIndex at each
// evaluation, an object argument whose toString runs, the methods replaced
// on the prototype and read through a getter in the order the call reads
// them, a replacer function, a split's limit, errors thrown by the call and
// by its arguments, and the RegExp kept by the method it calls.
func TestRegDiffRegExpLiteralCalls(t *testing.T) {
	src := `
var kept = [];
function loop(a) { var c = 0, s = ""; for (var j = 0; j < 8; j++) { if (/^a.c$/.test(a[j & 3])) c++; var m = /(\d+)-(\d+)/.exec("x" + j + "-" + (j * 3)); s += m[1] + m[2] + /a/y.test("ba") + /a/g.exec("aa").index } return c + s }
function objs() { var log = [], o = { toString: function () { log.push("ts"); return "abc" } }; return [/b/.test(o), /b/.exec(o)[0], "a,b,,c".split(/,/, 2).join("|"), "a1b22".replace(/\d+/g, function (d) { log.push(d); return "#" }), "x".match(/y/), log.join()].join(";") }
function patched() {
	var p = RegExp.prototype, t = p.test, e = p.exec, log = [];
	p.test = function (s) { log.push("test:" + s); kept.push(this); return t.call(this, s) };
	var r1 = /q/.test("q"), r2 = "aqa".replace(/q/, "Q"), r3 = "q1q".split(/1/).join();
	p.test = t;
	p.exec = function (s) { log.push("exec:" + s); kept.push(this); return e.call(this, s) };
	var r4 = /q/.test("zq"), r5 = /q/.exec("q")[0], r6 = "q".match(/q/)[0];
	p.exec = e;
	Object.defineProperty(p, "test", { get: function () { log.push("get"); return t }, configurable: true });
	var r7 = /q/.test((log.push("arg"), "q"));
	Object.defineProperty(p, "test", { value: t, writable: true, configurable: true });
	return [r1, r2, r3, r4, r5, r6, r7, log.join(), kept.length, kept[0] === kept[1]].join(";")
}
function errs() { var out = []; try { /a/.test({ toString: function () { throw new RangeError("r") } }) } catch (x) { out.push(x.name) } try { "s".replace(/s/, function () { throw new TypeError("t") }) } catch (x) { out.push(x.name) } try { /a/.exec(Symbol()) } catch (x) { out.push(x.name) } return out.join() }
`
	calls := []string{`loop(["abc", "axc", "ab", "abcd"])`, `objs()`, `patched()`, `errs()`}
	for i := range calls {
		calls[i] = fmt.Sprintf("(function () { var r; for (var k = 0; k < 4; k++) r = %s; return r })()", calls[i])
	}
	regDiff(t, src, calls)
}
