package regdiff

import (
	"context"
	"fmt"
	"math/rand"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	quickjs "github.com/go-quickjs/go-quickjs"
	"github.com/go-quickjs/go-quickjs/internal/bytecode"
	"github.com/go-quickjs/go-quickjs/internal/compiler"
	"github.com/go-quickjs/go-quickjs/internal/v8bench"
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
		env := bytecode.RegDefault()
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
	regDiffAlone.RLock()
	defer regDiffAlone.RUnlock()
	return regDiffLocked(t, src, calls)
}

// regDiffAlone is held by regDiffAll alone, which sets regcomp.OnRefuse
// for the process, and shared by every other comparison: where those run
// at once (TestRegLowerEvaluate's subtests), no other program compiles
// while a refusal is taken for regDiffAll's.
var regDiffAlone sync.RWMutex

// regDiffLocked is regDiff, its caller holding regDiffAlone.
func regDiffLocked(t *testing.T, src string, calls []string) int64 {
	t.Helper()
	if regDiffHook != nil {
		// Another test's reading of the corpus (regDiffHook).
		regDiffHook(t, src, calls)
		return 1 << 20
	}
	var off, again, on []string
	withRegTier(false, func() { off = regDiffRun(t, src, calls) })
	withRegTier(false, func() { again = regDiffRun(t, src, calls) })
	before := vm.RegsBuilt()
	vm.ResetRegFallbacks()
	withRegTier(true, func() { on = regDiffRun(t, src, calls) })
	built := vm.RegsBuilt() - before
	// Register mode runs everything as register code, entered as its own:
	// nothing in the stack interpreter or the tree tier, nothing entered
	// through their frames (docs/register-interpreter.md, item 13).
	if n, by := vm.RegFallbacks(); n != 0 {
		t.Errorf("%d fallbacks to the other tiers (QJS_REGFALLTRACE names them):\n%s", n, by)
	}
	bad, skipped := 0, 0
	defer func() {
		t.Logf("%d calls compared, %d not (their answers vary), %d functions as register code",
			len(calls)-skipped, skipped, built)
	}()
	for i := range calls {
		if os.Getenv("QJS_REGDIFF_SHOW") != "" {
			// Each answer, to see that a case tests what it says.
			t.Logf("%.100s => %.300s", calls[i], on[i])
		}
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

// regDiffHook, where it is set, is given each program and its calls in
// regDiff's place: a test that reads the corpus another way runs the
// differential tests with it set (TestRegLowerEvaluate).
var regDiffHook func(t *testing.T, src string, calls []string)

// regDiffResult is the expression a call is compared by: what it returns
// and what it left in global_res, as text.
func regDiffResult(call string) string {
	return fmt.Sprintf(`(function () { var r = %s; var g = global_res; try { g = JSON.stringify(g) } catch (e) { g = String(g) } return String(r) + " / " + g })()`, call)
}

// regDiffMicro is the micro-benchmarks' program, as the v8bench runner's
// micro mode loads it, and a call of each test with a little of its work.
func regDiffMicro(t *testing.T) (src string, calls []string) {
	t.Helper()
	tests, work, err := v8bench.Corpus("micro", "")
	if err != nil {
		t.Fatal(err)
	}
	src = `if (typeof console === "undefined") globalThis.console = { log: function () {} };
if (typeof performance === "undefined") globalThis.performance = { now: function () { return 0 } };
var global_res;
` + tests
	calls = make([]string, 0, len(work))
	for _, w := range work {
		// A little of each test's work: enough for its loops to turn and
		// its functions to run as each tier runs them.
		n := max(w.N/4096, 1)
		calls = append(calls, regDiffResult(fmt.Sprintf("%s(%d)", w.Name, n)))
	}
	return src, calls
}

func TestRegDiffMicrobenchmarks(t *testing.T) {
	if testing.Short() {
		t.Skip("runs every micro-benchmark three times")
	}
	src, calls := regDiffMicro(t)
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
	files, _, err := v8bench.Corpus("v8", dir)
	if err != nil {
		t.Skip(err)
	}
	var src strings.Builder
	src.WriteString(files)
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

// TestRegDiffReceiverBindings compares calls whose receiver an
// expression leaves in a binding's register -- an assignment, a comma, ??=
// and the logical assignments -- each followed by a read of the binding:
// the call's value must go to a temporary, never over the binding
// (f25d1df4 sent it there, and (o = /d/).test("d") left o true).
func TestRegDiffReceiverBindings(t *testing.T) {
	src := `
function P(n) { this.n = n }
P.prototype.m = function (x) { return this.n + ":" + x };
P.prototype.self = function () { return this };
function show(v) { return v instanceof P ? "P" + v.n : v instanceof RegExp ? "re" + v.source : typeof v + String(v) }
function assign() { var o, r = []; r.push((o = new P(1)).m("a"), show(o)); (o = new P(2)).m("b"); r.push(show(o)); var v = (o = "str").toUpperCase(); r.push(v, show(o)); return r.join() }
function comma() { var o = new P(3), q, r = []; r.push((q = 1, o).m("c"), show(o)); (0, o).m("d"); r.push(show(o)); (q = new P(4), q).m("e"); r.push(show(q)); return r.join() }
function nullish() { var o = null, r = []; r.push((o ??= new P(5)).m("f"), show(o)); (o ??= new P(6)).m("g"); r.push(show(o)); return r.join() }
function logical() { var a = 0, b = new P(7), c = new P(8), r = []; r.push((a ||= new P(9)).m("h"), show(a)); r.push((b &&= c).m("i"), show(b)); (a ||= 1).m("j"); r.push(show(a)); return r.join() }
function regexps() { var o, keep = []; for (var i = 0; i < 3; i++) { (o = /d/g).test("d"); keep.push(o, show(o), o.lastIndex) } return keep.length + "," + new Set(keep.filter(function (k) { return k instanceof RegExp })).size + "," + keep.filter(function (k) { return typeof k !== "object" }).join() }
function chained() { var o, r = []; var v = (o = new P(10)).self().self().m("k"); r.push(v, show(o)); (o = new P(11)).self().m("l"); r.push(show(o)); return r.join() }
function lets() { let o; const r = []; r.push((o = new P(12)).m("m"), show(o)); { let o2; (o2 = new P(13)).m("n"); r.push(show(o2)) } return r.join() }
function param(o) { var r = []; r.push((o = new P(14)).m("o"), show(o)); (o ??= new P(15)).m("p"); r.push(show(o)); return r.join() }
function captured() { var o, f = function () { return show(o) }, r = []; r.push((o = new P(16)).m("q"), f()); (o = new P(17)).m("r"); r.push(f()); return r.join() }
function looped(n) { var o, s = ""; for (var i = 0; i < n; i++) { s += (o = new P(i)).m(i); s += show(o) } return s }
`
	calls := []string{`assign()`, `comma()`, `nullish()`, `logical()`, `regexps()`, `chained()`, `lets()`, `param()`, `param(new P(0))`, `captured()`, `looped(4)`}
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
function fused(a) {
	var p = RegExp.prototype, t = p.test, e = p.exec, log = [], s = "xq", n = 5, u = "uq", out = [];
	for (var j = 0; j < 4; j++) { out.push(/q/.test(s), /\d/.test(n), /q/.exec(s).index, /z/.exec("z")[0], /a/g.test(s)); if (/q/.exec(s)) out.push("dead") }
	Object.defineProperty(p, "test", { get: function () { log.push("get:" + s); s = "changed"; return t }, configurable: true });
	out.push(/q/.test(s), /q/.test("q"), /q/.test(u), /k/.test(null));
	Object.defineProperty(p, "test", { get: function () { throw new EvalError("getter") }, configurable: true });
	try { /q/.test(u) } catch (x) { out.push(x.name, String(x.stack).split(String.fromCharCode(10))[1]) }
	Object.defineProperty(p, "test", { value: t, writable: true, configurable: true });
	p.exec = function (x) { log.push("exec:" + x); return e.call(this, x) };
	out.push(/q/.test(s), /h/.exec(s)[0], /q/.exec(u)[0]);
	if (/c/.exec(s)) out.push("deadPatched");
	p.exec = e;
	return out.join() + ";" + log.join();
}
var gre = /(\w)(\d)?/g, gre2 = /b/;
function marked(a) {
	var m, out = [], c = 0;
	for (var j = 0; j < 6; j++) {
		gre.lastIndex = 0;
		m = gre.exec(a[j & 3]);
		if (m) out.push(m[0] + m.index + (m[2] === undefined ? "u" : m[2]));
		if (gre2.exec(a[j & 3])) c++;
		if (a[j & 3].replace(gre2, "B")) c++;
	}
	var keep = []; for (j = 0; j < 3; j++) { m = gre2.exec("abc"); keep.push(m) } out.push(keep[0] === keep[1], keep[0][0]);
	return out.join() + c;
}
function argLit() {
	var log = [], out = [];
	try { "abc".replace(/b/g, (log.push("arg2"), "B")); out.push("x".replace(/x/, (function () { throw new SyntaxError("second") })())) } catch (x) { out.push(x.name) }
	out.push("a-b".split(/-/).join("+"), "a1b2".match(/\d/g).join(""), "zz".replace(/z/g, "$&$&"));
	return out.join() + ";" + log.join();
}
function errs() { var out = []; try { /a/.test({ toString: function () { throw new RangeError("r") } }) } catch (x) { out.push(x.name) } try { "s".replace(/s/, function () { throw new TypeError("t") }) } catch (x) { out.push(x.name) } try { /a/.exec(Symbol()) } catch (x) { out.push(x.name) } return out.join() }
`
	calls := []string{`loop(["abc", "axc", "ab", "abcd"])`, `objs()`, `patched()`, `errs()`, `fused()`, `marked(["a1", "b", "", "c2d"])`, `argLit()`}
	for i := range calls {
		calls[i] = fmt.Sprintf("(function () { var r; for (var k = 0; k < 4; k++) r = %s; return r })()", calls[i])
	}
	regDiff(t, src, calls)
}

// TestRegDiffTry compares try statements in register code: exceptions of
// throw statements, operators, getters, setters, built-ins and calls of
// both tiers caught; catch bindings read, written, captured and shadowing;
// finally clauses run for a normal completion, break, continue, return and
// throw, nested, overriding the completion or not; rethrows from catch and
// finally; and a stack overflow.
func TestRegDiffTry(t *testing.T) {
	src := `
var log = [];
function thrower(k) { if (k === 1) throw new TypeError("t" + k); if (k === 2) return null.x; if (k === 3) throw k; return k }
function accessors() { return { get g() { throw new RangeError("g") }, set s(v) { throw new SyntaxError("s" + v) } } }
function kinds() {
	var out = [], o = accessors();
	try { throw new Error("a") } catch (e) { out.push(e.message) }
	try { null.x } catch (e) { out.push(e.name) }
	try { undefined() } catch (e) { out.push(e.name) }
	try { o.g } catch (e) { out.push(e.name + e.message) }
	try { o.s = 4 } catch (e) { out.push(e.name + e.message) }
	try { JSON.parse("{") } catch (e) { out.push(e.name) }
	try { thrower(1) } catch (e) { out.push(e.message) }
	try { thrower(2) } catch (e) { out.push(e.name) }
	try { thrower(3) } catch (e) { out.push(e) }
	try { [1, 2].map(function (x) { if (x === 2) throw "m" + x; return x }) } catch (e) { out.push(e) }
	try { x1 } catch (e) { out.push(e.name) }
	try { let q = q + 1 } catch (e) { out.push(e.name) }
	try { new (function () { this.a = 1; throw "ctor" })() } catch (e) { out.push(e) }
	try { ({}).toString.call(null).x.y } catch (e) { out.push(e.name) }
	try { thrower(4) } catch { out.push("none") }
	out.push(thrower(4));
	return out.join()
}
function bindings(v) {
	var e = "outer", fs = [], r;
	try { throw v } catch (e) { e = e + "!"; r = e; fs.push(function () { return e }) }
	try { throw v } catch (x) { var y = x }
	return [e, r, fs[0](), y].join()
}
function fin(k) {
	var log = [];
	function f(k) {
		var x = "x";
		try {
			log.push("try");
			if (k === 0) return x;
			if (k === 1) throw "t";
			if (k === 2) return log.length;
		} catch (e) {
			log.push("catch " + e);
			if (k === 1) return "c";
		} finally {
			log.push("finally");
			x = "changed";
			if (k === 3) return "f";
		}
		return "end " + x;
	}
	var r;
	try { r = f(k) } catch (e) { r = "threw " + e }
	return r + ":" + log.join("|");
}
function loops() {
	var log = [];
	outer: for (var i = 0; i < 4; i++) {
		for (var j = 0; j < 4; j++) {
			try {
				try {
					if (j === 1) continue;
					if (j === 2 && i === 1) continue outer;
					if (j === 2) break;
					if (i === 3) break outer;
					log.push(i + "" + j);
				} finally {
					log.push("in" + j);
				}
			} finally {
				log.push("out" + j);
			}
		}
		log.push("end" + i);
	}
	var w = 0;
	while (true) { try { if (++w > 3) break } finally { log.push("w" + w) } }
	do { try { continue } finally { log.push("dw") } } while (false);
	switch (w) { case 4: try { break } finally { log.push("sw") } default: log.push("no") }
	return log.join();
}
function nested() {
	var log = [];
	function f() {
		try {
			try { return "inner" } finally { log.push("f1") }
		} finally { log.push("f2") }
	}
	function g() {
		try {
			try { throw "a" } finally { log.push("g1"); throw "b" }
		} catch (e) { log.push("caught " + e); try { throw "c" } catch (e) { log.push("again " + e) } finally { log.push("g2") } }
		finally { log.push("g3") }
		return "g";
	}
	function h() {
		try { try { throw "x" } catch (e) { throw e + "y" } } catch (e) { return e }
	}
	function k() {
		for (var i = 0; i < 3; i++) { try { try { break } finally { throw "fromfinally" + i } } catch (e) { log.push(e) } }
		try { return "r" } finally { try { throw "q" } catch (e) { log.push("q") } }
	}
	function m() { try { throw "m" } finally { log.push("m") } }
	var r = [f(), g(), h(), k()];
	try { m() } catch (e) { r.push(e) }
	return r.join() + ":" + log.join();
}
function deep(n) { return n === 0 ? 0 : deep(n - 1) + 1 }
function overflow() { var c = 0; for (var i = 0; i < 3; i++) { try { deep(1e7) } catch (e) { c++; log.push(e.name) } } return c + ":" + deep(100) }
function hot(n) { var s = 0; for (var i = 0; i < n; i++) { try { if (i % 7 === 3) throw i; s += i } catch (e) { s -= e } finally { s++ } } return s }
function heldFinally() {
	var log = [];
	function f() { try { return log.length + 10 } finally { for (var i = 0; i < 3; i++) log.push(i); var z = "z"; log.push(z) } }
	function g() { try { throw "e" } finally { for (var i = 0; i < 2; i++) log.push("g" + i); var y = 1 } }
	var r = f();
	try { g() } catch (e) { r += e }
	try { throw "c" } catch (e) { for (var k = 0; k < 2; k++) log.push(k); log.push(e) }
	return r + ":" + log.join();
}
function stackOfCaught() { function a() { throw new Error("here") } try { a() } catch (e) { return e.stack.split("\n").length + " " + e.message } }
`
	calls := []string{`kinds()`, `bindings(1)`, `bindings("v")`, `fin(0)`, `fin(1)`, `fin(2)`, `fin(3)`, `fin(4)`,
		`loops()`, `nested()`, `overflow()`, `hot(1000)`, `heldFinally()`, `stackOfCaught()`, `log.join()`}
	for i := range calls {
		calls[i] = fmt.Sprintf("(function () { var r; for (var k = 0; k < 4; k++) r = %s; return r })()", calls[i])
	}
	regDiff(t, src, calls)
}

// TestRegDiffForOfIn compares for-of and for-in loops in register code:
// arrays, strings, Maps, Sets, generators and iterators of user code as
// the iterated objects; targets var, let, const, an upvalue and a global;
// the iterator closed, in the spec's order, by break, continue of an outer
// loop, return and throw, and not by its end or by a throw of its own
// next; return methods that throw, return a primitive or are missing; an
// array changed while it is walked; for-in over objects, arrays, strings,
// prototypes, keys deleted and added; labels; and the loops nested.
func TestRegDiffForOfIn(t *testing.T) {
	src := `
var glob, gl = [];
function iter(n, log, opts) {
	opts = opts || {};
	var i = 0;
	var it = {
		next: function () { log.push("next" + i); if (opts.nextThrows === i) throw "nextErr"; return i < n ? { value: i++, done: false } : { value: undefined, done: true } }
	};
	if (opts.ret !== "none") it["return"] = function () { log.push("return"); if (opts.ret === "throws") throw "retErr"; if (opts.ret === "prim") return 1; return {} };
	var o = {};
	o[Symbol.iterator] = function () { log.push("iter"); return it };
	return o;
}
function holey(a, b) { return [a, , b] }
function* bad(log) { try { yield 1; null.x } finally { log.push("badFinally") } }
function* gen(log) { try { yield 1; yield 2; yield 3 } finally { log.push("genFinally") } }
function sums() {
	var s = 0, t = "", m = new Map([[1, "a"], [2, "b"]]), out = [];
	for (var x of [1, 2, 3]) s += x;
	for (let c of "héllo") t += c.toUpperCase();
	for (const kv of m) out.push(kv[0] + kv[1]);
	for (const e of new Set([4, 5])) s += e;
	for (glob of [7, 8]) s += glob;
	var up = 0;
	(function () { for (up of [9, 10]) s += up })();
	var arr = [1, 2, 3];
	for (var a of arr) { if (a === 1) arr.push(4); if (a === 2) arr.length = 3; s += a }
	var holes = holey(1, 3);
	for (var h of holes) out.push(h);
	return [s, t, out.join(), x, glob, up].join(";");
}
function closes(kind) {
	var log = [];
	function f() {
		outer: for (var j = 0; j < 2; j++) {
			for (var v of iter(5, log, { ret: kind === 4 ? "throws" : kind === 5 ? "prim" : kind === 6 ? "none" : "ok", nextThrows: kind === 7 ? 1 : -1 })) {
				log.push("v" + v);
				if (v === 1) {
					if (kind === 0 || kind >= 4 && kind <= 6) break;
					if (kind === 1) continue outer;
					if (kind === 2) return "ret";
					if (kind === 3) throw "body";
				}
			}
			log.push("after" + j);
		}
		return "end";
	}
	var r;
	try { r = f() } catch (e) { r = "threw " + e }
	return r + ":" + log.join("|");
}
function gens() {
	var log = [], s = 0;
	for (var v of gen(log)) { s += v; if (v === 2) break }
	for (var w of gen(log)) s += w;
	try { for (var u of gen(log)) throw "x" } catch (e) { log.push(e) }
	try { for (var b of bad(log)) log.push("b" + b) } catch (e) { log.push("caught " + e) }
	var g = gen(log);
	for (var x of g) { s += x; if (x === 1) break }
	for (var y of g) s += 100;
	return s + ":" + log.join("|");
}
function inFinally() {
	var log = [];
	for (var v of iter(3, log)) { try { if (v === 1) break } finally { log.push("fin" + v) } }
	try { for (var w of iter(3, log)) { if (w === 0) continue; return log.join("|") } } finally { log.push("outerFin") }
}
function keys() {
	function P() { this.a = 1; this.b = 2 }
	P.prototype.c = 3;
	var o = new P(), out = [], k;
	for (k in o) out.push(k);
	for (var i in holey(5, 8)) out.push(i);
	for (let s in "ab") out.push(s);
	for (k in { x: 1, y: 2, z: 3 }) { out.push(k); }
	var d = { p: 1, q: 2, r: 3 };
	for (k in d) { out.push(k); delete d.q; d.added = 1 }
	for (k in null) out.push("no");
	for (k in undefined) out.push("no");
	for (k in 5) out.push("no");
	lab: for (k in { m: 1, n: 2 }) { for (var z in { y: 1 }) { if (k === "m") continue lab; break lab } }
	out.push(k);
	return out.join();
}
function nestedLoops() {
	var s = 0;
	for (var a of [[1, 2], [3, 4]]) for (var b of a) for (var c in a) s += b * c;
	return s;
}
function dead() { try { for (let x of [x]) {} } catch (e) { return e.name } }
function notIterable() { var out = []; try { for (var x of 5) {} } catch (e) { out.push(e.name) } try { for (var y of {}) {} } catch (e) { out.push(e.name) } return out.join() }
function hot(n) { var a = []; for (var i = 0; i < n; i++) a.push(i); var s = 0; for (var k = 0; k < 10; k++) for (var v of a) s += v; return s }
function held() {
	var a = [1, 2, 3], s = [];
	for (var k in a) { if (k === "0") for (var i = 0; i < 3; i++) a.push(i); var t = k + "!"; s.push(t) }
	for (var v of a) { for (var j = 0; j < 2; j++) s.push(j); var u = v * 2; s.push(u) }
	return s.join();
}
function headClosure() { var f; try { for (const x of (f = function () { return x }, [1])) {} return String(f()) } catch (e) { return e.name } }
`
	calls := []string{`held()`, `headClosure()`, `sums()`, `gens()`, `inFinally()`, `keys()`, `nestedLoops()`, `dead()`, `notIterable()`, `hot(500)`}
	for k := 0; k <= 7; k++ {
		calls = append(calls, fmt.Sprintf("closes(%d)", k))
	}
	for i := range calls {
		calls[i] = fmt.Sprintf("(function () { var r; for (var k = 0; k < 4; k++) r = %s; return r })()", calls[i])
	}
	regDiff(t, src, calls)
}

// TestRegDiffSteps covers iter_next's steps of an iterator whose next is a
// function of script (regStepNext: a result not an object, getters of done
// and value, a next throwing, an object of its own as the result), of a Map's
// and a Set's iterators, and calls of generator functions from register code
// (regGenCall: a method's this, a sloppy body's this coerced, a parameter
// default that throws, a generator method of a class or a literal).
func TestRegDiffSteps(t *testing.T) {
	src := `
function Range(n, mode) { this.i = 0; this.n = n; this.mode = mode }
Range.prototype.next = function () {
	if (this.mode === "prim" && this.i === 2) return 5;
	if (this.mode === "throw" && this.i === 2) throw new RangeError("step");
	if (this.mode === "getters" && this.i < this.n) { var self = this; return { get done() { return self.i >= self.n }, get value() { return self.i++ * 10 } } }
	if (this.mode === "badValue" && this.i === 1) return { done: false, get value() { throw "val" } };
	if (this.i < this.n) return { value: this.i++, done: 0 };
	return { value: "end", done: "yes" };
};
Range.prototype[Symbol.iterator] = function () { return this };
function walk(n, mode) {
	var out = [];
	try { for (var v of new Range(n, mode)) out.push(v) } catch (e) { out.push(typeof e === "object" ? e.name + ":" + e.message : e) }
	return out.join();
}
function maps() {
	var m = new Map([[1, "a"], [2, "b"], [3, "c"]]), s = new Set(["x", "y"]), out = [];
	for (var v of m.values()) { out.push(v); if (v === "a") m.delete(2), m.set(4, "d") }
	for (var k of m.keys()) out.push(k);
	for (var e of s) { out.push(e); if (e === "x") s.add("z") }
	for (var p of s.entries()) out.push(p.join(":"));
	return out.join();
}
function* g1(a, b) { yield a; yield b }
function* gThis() { yield typeof this; yield this === globalThis }
function* gStrict() { "use strict"; yield typeof this }
function* gDef(a = (() => { throw new TypeError("def") })()) { yield a }
var holder = { *m(x) { yield this.tag + x }, tag: "h", g: gThis };
class K { constructor() { this.v = 7 } *each() { yield this.v; yield this.v + 1 } }
function gens() {
	var out = [];
	for (var v of g1(1, 2)) out.push(v);
	for (v of gThis()) out.push(v);
	for (v of holder.g()) out.push(v);
	for (v of gThis.call(5)) out.push(v);
	for (v of gStrict.call(5)) out.push(v);
	for (v of holder.m("!")) out.push(v);
	for (v of new K().each()) out.push(v);
	try { gDef() } catch (e) { out.push(e.name + ":" + e.message) }
	for (v of gDef(3)) out.push(v);
	var it = g1(8, 9); out.push(it.next().value, [...it].join("+"));
	return out.join();
}
`
	calls := []string{`maps()`, `gens()`}
	for _, m := range []string{"plain", "prim", "throw", "getters", "badValue"} {
		calls = append(calls, fmt.Sprintf("walk(4, %q)", m))
	}
	for i := range calls {
		calls[i] = fmt.Sprintf("(function () { var r; for (var k = 0; k < 4; k++) r = %s; return r })()", calls[i])
	}
	regDiff(t, src, calls)
}

// TestRegDiffRegExpWideConstants compares RegExp literals' calls in a
// function whose literals' constants are past 16 bits, which lit_exec_call
// and lit_exec cannot name in their words: the calls are made through the
// literal's own instruction and call_method_w's Aux, all 32 bits of it.
func TestRegDiffRegExpWideConstants(t *testing.T) {
	var b strings.Builder
	b.WriteString("function wide(s) {\n\tvar x, out = [];\n")
	for i := 0; i < 70000; i++ {
		fmt.Fprintf(&b, "\tx = \"c%d\";\n", i)
	}
	b.WriteString("\tfor (var j = 0; j < 3; j++) out.push(/q/.test(s), /(q)/.exec(s)[1], s.replace(/q/g, x), s.split(/u/).length, /a/.exec(\"za\").index);\n\treturn out.join();\n}\n")
	built := regDiff(t, b.String(), []string{`wide("uqu")`, `wide("q")`})
	if built == 0 {
		t.Errorf("wide is not register code")
	}
}

// TestRegDiffFarNames compares code whose names run past 16 bits, where an
// instruction's name operand is RNameFar and RegCode.FarNames has the
// index: each function, and each script, starts with 65,600 reads of
// globals by typeof, each a name of its own, and only then names what it
// uses -- globals read, written and deleted, sloppy and strict, an object
// literal's keys made at once and one by one, accessors, a class's
// members, private names and super, a with's names, a constant's and an
// assignment target's errors -- and in scripts and indirect eval code that
// declare 65,600 vars first, top-level declarations.
func TestRegDiffFarNames(t *testing.T) {
	if testing.Short() {
		t.Skip("compiles a dozen programs of 65,600 statements three times")
	}
	var fill strings.Builder
	for i := 0; i < 65600; i++ {
		fmt.Fprintf(&fill, "typeof f%d;\n", i)
	}
	f := fill.String()
	// A script's declarations take its first names: past 16 bits, those
	// after 65,600 others.
	var d strings.Builder
	d.WriteString("var d0")
	for i := 1; i < 65600; i++ {
		fmt.Fprintf(&d, ", d%d", i)
	}
	d.WriteString(";\n")
	decls := d.String()
	src := `
var gA = 1, gD = 2;
class B { bm() { return "bm" } get bx() { return 10 } }
function n1() {
` + f + `gA = gA + 1; gA += 2; var r = [gA, typeof gZ, typeof gA, delete gD, typeof gD];
	try { gNo } catch (x) { r.push(x.name + ": " + x.message) }
	var k = "kk", ob = {ka: 1, kb: 2}, oc = {[k]: 1, kc: 3, get ga() { return 4 }, set ga(v) { this.kv = v }}, od = {...ob, kd: 5};
	oc.ga = 6;
	r.push(JSON.stringify(ob), JSON.stringify(oc), oc.ga, JSON.stringify(od));
	const cc = 1;
	try { cc = 2 } catch (x) { r.push(x.name + ": " + x.message) }
	try { n1() = 1 } catch (x) { r.push(x.name + ": " + x.message) }
	return r.join(" | ");
}
function n2() {
	"use strict";
` + f + `var r = []; gA = 7; r.push(gA);
	try { gU = 1 } catch (x) { r.push(x.name + ": " + x.message) }
	try { gV += 1 } catch (x) { r.push(x.name + ": " + x.message) }
	try { gW = String(r.length) } catch (x) { r.push(x.name + ": " + x.message) }
	gA = String(gA + 1); r.push(gA);
	return r.join(" | ");
}
function n3() {
` + f + `class K extends B { #p = 1; static sm() { return "sm" } get kg() { return 2 } set kg(v) { this.q = v } m() { return this.#p } }
	var k = new K(); k.kg = 3;
	return [K.sm(), k.kg, k.q, k.m(), k.bm(), Object.getOwnPropertyNames(K.prototype).join("/")].join();
}
class C2 extends B {
	#p = 1;
	m() {
` + f + `this.#p += 1; super.by = 5; return [this.#p, super.bx, super.bm(), this.by, #p in this].join();
	}
	n(o) {
` + f + `try { return this.#p } catch (x) { return x.name } finally { try { C2.prototype.m.call(o) } catch (x) { globalThis.pe = x.name + ": " + x.message } }
	}
}
function n4(o) {
` + f + `with (o) { wx = wx + 1; wn = 9; return [wx, wy, typeof wz, wn].join() }
}
`
	calls := []string{`n1()`, `n2()`, `n3()`, `new C2().m()`, `new C2().n({}) + " " + pe`,
		`var wo = {wx: 1, wy: 2, wn: 0}; n4(wo) + " " + wo.wx + " " + wo.wn`,
		f + `var tv = 1; let tl = 2; const tc = 3; function tf() { return 4 } class tk {} tv + tl + tc + tf() + typeof tk`,
		`tv + tl + tc + tf() + typeof tk`,
		`(0, eval)(` + "`" + f + "var iv = 5; let il = 6; iv + il`" + `) + "," + iv + "," + typeof il`,
		decls + `let dl = 1; const dc = 2; function dfn() { return 3 } class dk {} d65599 = 4; d65599 + dl + dc + dfn() + typeof dk`,
		`(0, eval)(decls + "var dv = 5; d1 = dv; d1")`,
		"let" + decls[3:] + `var ev = 1; let el = 2; function efn() { return 3 } class ek {} ev + el + efn() + typeof ek + typeof d1`,
		`(function () { return eval(` + "`" + f + "var dv = 7; let dl = 8; dv + dl + gA`" + `) + "," + dv })()`}
	if built := regDiff(t, src, calls); built == 0 {
		t.Errorf("no function is register code")
	}
}

// TestRegDiffFarRegisters compares functions whose bindings do not fit
// 16 bits of registers, where those used least have far ones, reached by
// mov_far and mov_to_far alone: each declares 65,600 bindings used five
// times, and only then those the cases use, five times or fewer, which
// are far (farSlots). Reads, stores, compound assignments and updates,
// appends, patterns, for-in and for-of heads, a loop's counter, let and
// const, typeof, a closure over a near binding beside them, the
// arguments object, a generator suspended with its far bindings, and a
// named function expression's own name; and a function whose bindings fit
// but leave its temporaries too few registers.
func TestRegDiffFarRegisters(t *testing.T) {
	if testing.Short() {
		t.Skip("compiles functions of 65,600 bindings three times")
	}
	var fill strings.Builder
	for i := 0; i < 65600; i++ {
		fmt.Fprintf(&fill, "var v%d = v%d + v%d + v%d + v%d;\n", i, i, i, i, i)
	}
	f := fill.String()
	// A function whose 30,000 bindings fit, but not beside the 40,000
	// temporaries of a template's 40,000 parts: compiled again with a
	// quarter of the registers for its bindings (nearCap).
	var deep strings.Builder
	deep.WriteString("function deep(p) {\n" + f[:strings.Index(f, "var v30000 ")] + "var x = p, y = p + 1;\n\treturn `" + strings.Repeat("${x}-", 19999) + "${y}`;\n}\n")
	src := deep.String() + `
function big(p) {
` + f + `var r = [];
	var a1 = 5; r.push(a1, typeof a1);
	var a2; a2 = p * 2; r.push(a2);
	var a3 = 1; a3 += p; r.push(a3);
	var a4 = 1; a4++; r.push(a4);
	var a5 = 1; r.push(--a5);
	var a6 = 1; r.push(a6++, a6);
	var s = ""; s += "x"; s += "y";
	var [d1, d2] = [1, 2]; var d3; ({d3} = {d3: 3}); r.push(s, d1, d2, d3);
	for (var k in {m: 1, n: 2}) r.push(k);
	for (var e of [7, 8]) r.push(e);
	var t = 0; for (var i = 0; i < 4; i++) t += i;
	r.push(t);
	let l1 = 4; const c1 = 6; r.push(l1 + c1);
	var cap = 1; var get = () => cap; cap = 2; r.push(get());
	r.push(arguments.length, arguments[0]);
	var o = {q: 9}; var q = o.q; r.push(q);
	return r.join();
}
function* gen(p) {
` + f + `var x = p; yield x; x = x + 1; yield x;
	var y = yield x * 10; yield y;
}
var named = function self(p) {
` + f + `var z = p + 1;
	return typeof self + z;
};
`
	calls := []string{`deep(2)`, `big(3)`, `big(-1)`, `var g = gen(5); [g.next().value, g.next().value, g.next().value, g.next(42).value, g.next().done].join()`, `named(1)`}
	if built := regDiff(t, src, calls); built == 0 {
		t.Errorf("no function is register code")
	}
}

// TestRegDiffTailCalls compares calls in tail position in strict code
// (tail_call_w): chains deep past tailCallDepth in constant space, mutual and
// through methods, conditionals, logical and comma forms; a construct's
// tail call, which keeps its frame; callees that are built-ins, functions
// the backend refuses, and that throw at depth; and a stack trace.
func TestRegDiffTailCalls(t *testing.T) {
	src := `
"use strict";
function even(n) { return n === 0 ? true : odd(n - 1) }
function odd(n) { return n === 0 ? false : even(n - 1) }
var o = { k: 0, down(n) { return n === 0 ? this.k : (this.k++, this.down(n - 1)) } };
function sum(n, acc) { if (n === 0) return acc; return sum(n - 1, acc + n) }
function cond(n) { return n > 0 && (n % 2 ? cond(n - 1) : (0, cond(n - 1))) || n }
function nat(n) { return Math.max(n, 3) }
function* gen() { yield 1 }
function viaGen() { return gen().next().value }
function args() { return arguments.length }
function viaArgs(n) { return args(n, n) }
function thrower(n) { if (n === 0) throw new RangeError("deep " + n); return thrower(n - 1) }
function C(n) { this.n = n; return mk(n) }
function mk(n) { return { made: n } }
function deep(n) { return n === 0 ? new C(2).made : deep(n - 1) }
function trace(n) { if (n === 0) return new Error("x").stack.split(String.fromCharCode(10)).length; return trace(n - 1) }
function deepTry(n) { try { return thrower(n) } catch (e) { return e.message } }
`
	calls := []string{`even(100001)`, `odd(77777)`, `o.k = 0, o.down(50000)`, `sum(200000, 0)`, `cond(1001)`, `nat(5)`,
		`viaGen()`, `viaArgs(4)`, `deepTry(50000)`, `deep(100)`, `trace(5)`, `trace(10)`}
	regDiff(t, src, calls)
}

// TestRegDiffLegacyArguments compares a sloppy function's f.arguments
// (WithNodeQuirks) in frames register code makes: the count, which the
// caller's call instruction holds, and the arguments past the parameters,
// which the callee's bindings overwrite in its window -- through a site's
// first call, its cached callee, a method call, a tail call, a callback,
// call, apply and a bound function, and a forwarder -- and a parameter a
// closure captures or of a list with a default, read as passed.
func TestRegDiffLegacyArguments(t *testing.T) {
	src := `
function f(a) { var x = 7, y = 8; return f.arguments.length + ":" + [...f.arguments].join() + ":" + (x + y) }
function f2(a, b) { var t = a * 2; return [...f2.arguments].join() + "|" + t }
function none() { var q = 1, w = 2; return none.arguments.length + q + w }
function g() { var r = []; for (var i = 0; i < 3; i++) r.push(f(1, 2, 3), f(4), f()); return r.join(" ") }
function h() { var o = { m: f2 }, r = []; for (var i = 0; i < 3; i++) r.push(o.m(5, 6, 7, 8), f2(9)); return r.join(" ") }
function k() { var s = 0; for (var i = 0; i < 3; i++) s += none(1, 2, 3, 4) * 10 + none(); return s }
function tl() { "use strict"; return f(i9, 10, 11) }
var i9 = 9;
function deep(n) { return n === 0 ? tl() : deep(n - 1) + "" }
function viaCall() { var r = []; for (var i = 0; i < 3; i++) r.push(f.call(null, 1, 2, 3), f.apply(null, [4, 5]), Reflect.apply(f, null, [6, 7, 8, 9]), f.bind(null, 1)(2, 3)); return r.join(" ") }
function fw() { return f.apply(null, arguments) }
function viaFw() { return fw(1, 2, 3, 4) + " " + fw() }
function cap(a, b) { a = 5; b = 6; var g = () => a + b; return [...cap.arguments].join() + ":" + g() }
function pat(a, b = 2) { a = 7; return [...pat.arguments].join() }
function viaCap() { var r = []; for (var i = 0; i < 3; i++) r.push(cap(1), cap(1, 2, 3), cap.call(null, 4), [8, 9].map(cap).join("/"), pat(1), pat(1, 2, 3), pat.call(null, 4)); return r.join(" ") }
`
	calls := []string{"none(1, 2, 3, 4)", "(function () { return none(1, 2, 3, 4) })()", "(function () { var s = 0; for (var i = 0; i < 3; i++) s += none(1, 2, 3, 4); return s })()", "g()", "h()", "k()", "tl()", "deep(40)", "viaCall()", "viaFw()", "viaCap()", "cap(1, 2)", "g() + h()"}
	run := func() []string {
		rt := quickjs.New(quickjs.WithNodeQuirks())
		defer rt.Close()
		if _, err := rt.Eval(src); err != nil {
			t.Fatalf("eval: %v", err)
		}
		out := make([]string, len(calls))
		for i, c := range calls {
			v, err := rt.Eval(c)
			if err != nil {
				out[i] = "error: " + err.Error()
			} else {
				out[i] = v.String()
			}
		}
		return out
	}
	var off, on []string
	withRegTier(false, func() { off = run() })
	before := vm.RegsBuilt()
	withRegTier(true, func() { on = run() })
	if vm.RegsBuilt() == before {
		t.Error("no function was register code")
	}
	for i, c := range calls {
		if off[i] != on[i] {
			t.Errorf("%s: %q, register code %q", c, off[i], on[i])
		}
	}
}

// TestRegTailCallsStop checks that an endless loop of calls in tail
// position, each made in its frame's place (regRestartRun), polls as a
// loop does: the host's context stops it.
func TestRegTailCallsStop(t *testing.T) {
	withRegTier(true, func() {
		rt := quickjs.New()
		defer rt.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
		defer cancel()
		for _, src := range []string{
			`"use strict"; function inf(n) { return inf(n + 1) } inf(0)`,
			`"use strict"; function a(n) { return b(n + 1) } function b(n) { return a(n + 1) } a(0)`,
		} {
			if _, err := rt.EvalContext(ctx, src); err == nil {
				t.Errorf("%s: an endless loop of calls in tail position ended", src)
			}
		}
	})
}

// TestRegDiffTopLevel compares top-level code as register code: a script's
// declarations in the global object and the global lexical environment,
// made before its first statement, and their errors -- a lexical
// redeclaration across scripts, a var over a let, a function over a
// non-configurable global, a binding read in its dead zone -- its function
// declarations hoisted, typeof of undeclared and uninitialized names, and
// its completion value through loops, switches, try statements and finally
// clauses, breaks and continues, and throws.
func TestRegDiffTopLevel(t *testing.T) {
	src := `
var v1 = 1, v2;
let l1 = 2, l2;
const c1 = 3;
function f1() { return v1 + l1 + c1 + typeof l2 + typeof v2 }
var frames = function (e) { return e.name + ": " + e.message + " | " + e.stack.split("\n").slice(1).map(function (s) { return s.trim() }).join(" | ") };
Object.defineProperty(globalThis, "fixed", { value: 1, configurable: false, writable: false });
`
	calls := []string{
		`f1()`,
		`v1 = 5; f1()`,
		`let l1 = 3`,
		`var l1 = 3`,
		`try { eval("let v1 = 2") } catch (e) { frames(e) }`,
		`function fixed() {}`,
		`fixed`,
		`typeof nope + typeof l1 + typeof f1 + typeof fixed`,
		`typeof late; let late = 1`,
		`try { typeof later; let later = 1 } catch (e) { e.name }`,
		`early; let early2 = 1; var early`,
		`hoisted(); function hoisted() { return "h" }`,
		`1; if (false) {}`,
		`1; {}`,
		`2; var q;`,
		`for (var i = 0; i < 3; i++) i * 10`,
		`for (var i = 0; i < 3; i++) { if (i == 1) break; i * 10 }`,
		`for (var i = 0; i < 3; i++) { if (i == 1) continue; i * 10 }`,
		`do { 7; break } while (false)`,
		`switch (2) { case 1: "a"; case 2: "b"; case 3: "c"; break; default: "d" }`,
		`try { 1 } finally { 2 }`,
		`try { 1; throw 0 } catch (e) { }`,
		`try { 1; throw 0 } catch (e) { 4 }`,
		`try { 1 } catch (e) { 4 } finally { 5 }`,
		`L: for (var j = 0; j < 2; j++) { try { 3; continue L } finally { 9 } }`,
		`for (var j = 0; j < 2; j++) { try { 3; break } finally { 9; } }`,
		`for (var j = 0; j < 2; j++) { try { 3 } finally { 9; break } }`,
		`for (var k of [1, 2, 3]) k + 1`,
		`for (var k in { a: 1, b: 2 }) k`,
		`var w = 0; while (w < 3) w++`,
		`try { null.x } catch (e) { frames(e) }`,
		`null.x`,
		`undeclared`,
		`"use strict"; undeclared2 = 1`,
		`"use strict"; var NaN = 1`,
		`var NaN = 1; NaN`,
		`"use strict"; var sv = 4; sv`,
		`const k1 = 1; k1 = 2`,
		`let tz = tz`,
		`class K {}; typeof K`,
		`var o = { m() { return 1 } }; o.m()`,
		`(function () { return this === globalThis })()`,
		`this === globalThis`,
		`function g2() { return 1 } function g2() { return 2 } g2()`,
		`var s = ""; for (var x = 0; x < 5; x++) s += x; s`,
		`new Error("top").stack.split("\n").length`,
		`frames(new Error("here"))`,
		// A function's own name read inside a switch, a try or a loop of
		// its body, and an arrow that returns its parameter called with an
		// object for its this.
		`(function f(n) { switch (n) { case 0: return typeof f } return f(0) })(1)`,
		`(function g() { try { return typeof g } finally {} })()`,
		`(function h(a) { for (var x of a) return typeof h })([1])`,
		`var af = eval => eval; af(1) + (function () { var a = x => x, k = () => 7; return a(5) + k() })()`,
	}
	regDiff(t, src, calls)
}

// TestRegDiffPatterns compares parameter lists with defaults, patterns and
// a rest, and destructuring in declarations, assignments, for-of and for-in
// heads and catch clauses: the order of the steps, the reads and the
// targets' evaluation, defaults, the dead zone of parameters and of a
// head's bindings, iterators closed by a pattern done early and by a throw
// and not by one their next threw, rest elements and objects, holes, and
// the errors -- destructuring null, a non-iterable, a parameter read before
// its turn -- with their stack traces.
func TestRegDiffPatterns(t *testing.T) {
	src := `
var log = [];
function frames(e) { return e.name + ": " + e.message + " | " + e.stack.split("\n").slice(1, 3).map(function (s) { return s.trim() }).join(" | ") }
function iter(n, opts) { opts = opts || {}; var i = 0; var it = {}; it[Symbol.iterator] = function () { log.push("iter"); return { next: function () { log.push("next" + i); if (opts.throwAt === i) throw new Error("next " + i); return i < n ? { value: i++, done: false } : { value: undefined, done: true } }, return: function () { log.push("return"); if (opts.badReturn) throw new Error("return"); return {} } } }; return it }
function obj() { var o = {}; ["a", "b", "c"].forEach(function (k) { Object.defineProperty(o, k, { get: function () { log.push("get " + k); return k }, enumerable: true }) }); return o }
function key(k) { log.push("key " + k); return k }
function p1(a, b = a + 1, c = b * 2) { return [a, b, c].join() }
function p2([a, b] = [1, 2], {c, d: e = 5} = {}) { return [a, b, c, e].join() }
function p3(a, ...rest) { return a + ":" + rest.join() + ":" + rest.length + ":" + arguments.length }
function p4(...[x, y]) { return [x, y].join() }
function p5(a = b, b) { return a }
function p6(f = function () { return a }, a = 2) { return f() }
function p7(a, b = arguments.length) { return b + ":" + arguments[0] }
function p8({a}) { return a }
function p9(a = 1) { var a; return a }
function p10(a, g = () => a) { a = 5; return g() }
function p11(a = a) { return a }
function p12(a, [b] = [b]) { return a + b }
function d1(n) { log = []; var [a, , b, ...c] = iter(n); return [a, b, c.join(), log.join()].join("/") }
function d2(n) { log = []; let [a, b] = iter(n); return [a, b, log.join()].join("/") }
function d3() { log = []; const { [key("a")]: x, b, ...rest } = obj(); return [x, b, JSON.stringify(rest), log.join()].join("/") }
function d4(v) { var { a = 1, b: { c } = { c: 2 } } = v; return a + c }
function d5() { log = []; try { var [a = (() => { throw new Error("def") })()] = iter(3) } catch (e) { return frames(e) + " / " + log.join() } }
function d6() { log = []; try { var [a, b] = iter(5, { throwAt: 1 }) } catch (e) { return e.message + " / " + log.join() } }
function d7() { log = []; try { var [a] = iter(5, { badReturn: true }) } catch (e) { return e.message + " / " + log.join() } }
function d8(v) { try { var { a } = v } catch (e) { return frames(e) } }
function d9(v) { try { var [a] = v } catch (e) { return frames(e) } }
function a1() { var o = {}, arr = [], x, y; [o.a, arr[0], ...o.rest] = [1, 2, 3, 4]; ({ x, y: arr[1] = 9, z: o["z" + 1] } = { x: 7, z: 8 }); return JSON.stringify([o, arr, x]) }
function a2() { log = []; var o = { set p(v) { log.push("set " + v) } }; [o.p, o[key("p")]] = iter(2); return log.join() }
function a3() { var a = 1, b = 2; [a, b] = [b, a]; var r = ({ a, b } = { a: 3, b: 4 }); return [a, b, r.a].join() }
function a4() { var o = { o: 1, p: 2 }; ({ o, p: o } = o); return o }
function f1(xs) { var out = []; for (var [k, v = "d"] of xs) out.push(k + v); for (let { length: n } of ["ab", "c"]) out.push(n); for (const [i] in { x: 1, yz: 2 }) out.push(i); return out.join() }
function f2() { var o = {}, out = []; for (o.k of [1, 2]) out.push(o.k); for ([o.a, o.b] of [[3, 4]]) out.push(o.a + o.b); return out.join() }
function f3() { log = []; try { for (var [a] of [iter(3)]) throw new Error("body") } catch (e) { return e.message + " / " + log.join() } }
function f4() { try { for (let [a = a] of [[]]) {} } catch (e) { return e.name } }
function c1() { try { throw { m: 1, n: [2, 3] } } catch ({ m, n: [x, ...y] }) { return m + x + y.length } }
function c2() { try { try { throw null } catch ({ m }) { return "no" } } catch (e) { return frames(e) } }
`
	calls := []string{
		`p1(1)`, `p1(1, 5)`, `p1(1, undefined, 0)`, `p2()`, `p2([7], { c: 3, d: 4 })`, `p3(1, 2, 3)`, `p3()`, `p4(1, 2, 3)`,
		`try { p5() } catch (e) { frames(e) }`, `p5(1)`, `p6()`, `p7(1)`, `p7(1, 2, 3)`,
		`try { p8() } catch (e) { frames(e) }`, `p8({ a: 4 })`, `p9()`, `p9(3)`, `p10(1)`, `try { p11() } catch (e) { frames(e) }`, `p11(2)`, `try { p12(1) } catch (e) { frames(e) }`, `p12(1, [2])`, `p1.length + "," + p2.length + "," + p3.length`,
		`d1(0)`, `d1(2)`, `d1(6)`, `d2(1)`, `d2(4)`, `d3()`, `d4({})`, `d4({ a: 2, b: { c: 3 } })`, `d5()`, `d6()`, `d7()`,
		`d8(null)`, `d8(undefined)`, `d8(1)`, `d9(1)`, `d9({})`, `d9("xy")`,
		`a1()`, `a2()`, `a3()`, `a4()`, `f1([[1, 2], [3]])`, `f2()`, `f3()`, `f4()`, `c1()`, `c2()`,
		`var [g1, g2 = 5] = [1]; let { g3, ...g4 } = { g3: 2, g5: 6 }; g1 + g2 + g3 + JSON.stringify(g4)`,
		`try { let [h1] = null } catch (e) { frames(e) }`,
	}
	regDiff(t, src, calls)
}

// TestRegDiffLiterals compares templates and tagged templates (each site's
// object, the conversions' order, a symbol's TypeError), BigInt literals,
// object literals of every property kind (accessors, methods and their
// names, computed and number keys, __proto__, spreads, keys twice), array
// holes and spreads, spread calls and constructions, optional chains and
// calls (this kept, document.all-like values aside), ??, logical
// assignment to names, properties and elements, and assignment to a
// constant, with their errors.
func TestRegDiffLiterals(t *testing.T) {
	src := `
var log = [];
function frames(e) { return e.name + ": " + e.message + " | " + e.stack.split("\n").slice(1, 3).map(function (s) { return s.trim() }).join(" | ") }
function obj(tag) { return { toString() { log.push("ts " + tag); return "S" + tag }, valueOf() { log.push("vo " + tag); return 1 } } }
function t1(a, b) { log = []; var s = ` + "`x${a}-${obj(1)}${(log.push(\"e\"), b)}y${obj(2)}`" + `; return s + " " + log.join() }
function t2(x) { return ` + "`${x}`" + ` + "|" + ` + "`${x}${x}`" + ` + "|" + ` + "``" + ` }
function tag(s, ...v) { return [s.length, s.raw.join("/"), v.join(), this === undefined, Object.isFrozen(s)].join(";") }
function t3(x) { var o = { tag, k: "K" }; return tag` + "`a${x}b\n`" + ` + o.tag` + "`c${x}`" + ` + o["tag"]` + "`q`" + ` }
function t4() { var r = []; for (var i = 0; i < 2; i++) r.push((function (s) { return s })` + "`same`" + `); return r[0] === r[1] }
function t5() { try { return ` + "`${Symbol(\"s\")}`" + ` } catch (e) { return frames(e) } }
function b1(x) { return [10n ** 20n + 1n, typeof 1n, 0x1fn, x * 2n].join() }
function o1(k, v) { var b = { x: 1 }; var o = { a: 1, a: 2, [k]: v, [k + "f"]: function () {}, [k + "g"]: () => 1, m() { return this.a }, get g() { return 5 }, set g(x) { this.s = x }, get [k + "h"]() { return 6 }, 1: "one", 2.5: function () {}, 3() {}, get 4() { return 4 }, ...b, __proto__: { inh: 7 }, "__proto__x": 1, ...null, ...undefined, ..."hi" }; o.g = 9;
  return JSON.stringify(o) + Object.getOwnPropertyNames(o).join() + o.m() + o.inh + o[k + "f"].name + o[k + "g"].name + o[2.5].name + o[3].name + Object.getOwnPropertyDescriptor(o, "4").get.name + Object.getOwnPropertyDescriptor(o, "g").set.name + Object.getOwnPropertyDescriptor(o, k + "h").get.name }
function o2() { var __proto__ = 3; var o = { __proto__ }; var p = { ["__proto__"]: 4 }; var q = { __proto__: null }; return o.__proto__ + "," + p.__proto__ + "," + Object.getPrototypeOf(q) }
function o3() { log = []; var k = obj("k"); var o = { [k]: (log.push("v"), 1) }; return log.join() + Object.keys(o) }
function o4() { try { return { [Symbol.toPrimitive]: 1, [{ toString() { throw new Error("key") } }]: 2 } } catch (e) { return frames(e) } }
function s1(a) { var b = [1, , 3]; var c = [...a, , ...b, 4, ...new Set([9, 9])]; return c.length + ":" + JSON.stringify(c) + (1 in b) + (1 in c) + [, ,].length }
function s2(a) { var o = { m(...x) { return this.k + x.join() }, k: "K" }; return Math.max(...a, 0) + o.m(...a, 7) + o["m"](...[1]) + new Array(...[3]).length }
function s3(v) { try { return Math.max(...v) } catch (e) { return frames(e) } }
function c1(o) { return [o?.a, o?.a.b, o?.["a"], o?.m(), o?.m?.(), o.n?.(), (o?.m)(), o?.a?.b?.c, o?.m(...[1])].join() }
function c2(f) { return f?.() + "," + (f?.(1)?.x) }
function c3(a, b) { return (a ?? b) + "," + (a ?? (b ?? 3)) + "," + (a?.x ?? "dflt") }
function c4() { try { var u; (u?.m)() } catch (e) { return frames(e) } }
var cobj = { a: { b: 2 }, m(x) { return this === cobj ? "this" + (x || "") : "no" } };
var g1;
function l1() { var a = 0, b = 1, c = null; log = []; a ||= 5; b &&= 7; c ??= 9; var o = { get x() { log.push("get"); return 0 }, set x(v) { log.push("set" + v) } }; o.x ||= 3; o.x &&= 4; o.x ??= 5; var k = obj("k"); var p = {}; p[k] ??= 1; p[k] ??= 2; g1 ??= "g"; var f; f ||= function () {}; return [a, b, c, JSON.stringify(p), log.join(), g1, f.name, (a ||= 1), (c &&= 2)].join() }
function l2() { "use strict"; const k = 1; try { k ||= 2; k &&= 3 } catch (e) { return frames(e) + k } }
function l3() { const k = 1; try { k = 2 } catch (e) { return frames(e) } }
function l4() { let x = 1; const c = 2; try { [x, c] = [3, 4] } catch (e) { return frames(e) + x } }
function m1(a) { var args = arguments; var o = { f(x) { x[0] = 100; return 1 } }; var t = 0; for (var i = 0; i < 3; i++) { a = i; t += a + o.f(args) } return t + ":" + a }
`
	calls := []string{`t1(1, 2)`, `t2(5)`, `t2({})`, `t3(5)`, `t4()`, `t5()`, `b1(3n)`, `o1("z", 8)`, `o2()`, `o3()`, `o4()`,
		`s1([5, 6])`, `s2([1, 2])`, `s3(5)`, `s3([1, 2])`, `c1(cobj)`, `c1({ a: 0, m() { return 1 }, n: null })`, `try { c1(null) } catch (e) { frames(e) }`,
		`c2(null)`, `c2(() => ({ x: 5 }))`, `c3(null, 0)`, `c3(0, 1)`, `c3(undefined, undefined)`, `c4()`, `l1()`, `l2()`, `l3()`, `l4()`,
		"`top ${1 + 1}`", `m1(0)`, `var pa = { b: function () { return this._b }, _b: { c: 42 } }; String((pa?.b)?.().c) + String((null?.b)?.())`, `({ a: 1, ...{ b: 2 } }).b`, `[...[1, 2], 3].join()`, `undefined?.x ?? "n"`}
	regDiff(t, src, calls)
}

// TestRegDiffStrictGlobalStores compares strict assignments to globals of a
// value that runs code: the reference is settled before the value runs, so
// a value that creates the global does not make the store resolvable, a
// value that throws is what the assignment reports, and an existing global,
// a lexical one and a global object's accessor take the store, with the
// errors' stack traces.
func TestRegDiffStrictGlobalStores(t *testing.T) {
	src := `
function frames(e) { return e.name + ": " + e.message + " | " + e.stack.split("\n").slice(1, 3).map(function (s) { return s.trim() }).join(" | ") }
function mk(name, v) { globalThis[name] = 0; return v }
function boom() { throw new Error("value") }
var sg1 = 1;
let sg2 = 2;
var sgLog = [];
Object.defineProperty(globalThis, "sg3", { get() { return 0 }, set(v) { sgLog.push(v) }, configurable: true });
function s1() { "use strict"; sg1 = mk("x", 5); return sg1 }
function s2() { "use strict"; sg2 = mk("x", 6); return sg2 }
function s3() { "use strict"; try { sgNew1 = mk("sgNew1", 7); return "no" } catch (e) { return frames(e) + " " + globalThis.sgNew1 } }
function s4() { "use strict"; try { sgNew2 = boom(); return "no" } catch (e) { return frames(e) + " " + ("sgNew2" in globalThis) } }
function s5() { "use strict"; sg3 = mk("x", 8); return sgLog.join() }
function s6() { "use strict"; var r = []; for (var i = 0; i < 3; i++) { sg1 = mk("x", i); r.push(sg1) } return r.join() }
function s7() { "use strict"; try { sgNew3 = mk("x", 1) } catch (e) { return frames(e) } }
`
	calls := []string{`s1()`, `s2()`, `s3()`, `s4()`, `s5()`, `s6()`, `s7()`, `s7()`}
	regDiff(t, src, calls)
}

// TestRegDiffClasses compares class declarations and expressions: base and
// derived constructors, super() with arguments, spread and forwarded,
// super properties read, written, updated and called, computed keys,
// accessors, static members and blocks, public and private fields,
// private methods and accessors, #x in o, new.target, a derived
// constructor's returns, arrows sharing its this, classes made in a loop,
// and the errors -- a class called, this before super(), super() twice, a
// private member missing, a derived constructor returning a primitive --
// with their stack traces.
func TestRegDiffClasses(t *testing.T) {
	src := `
var log = [];
function frames(e) { return e.name + ": " + e.message + " | " + e.stack.split("\n").slice(1, 3).map(function (s) { return s.trim() }).join(" | ") }
function key(k) { log.push("key " + k); return k }
class A {
  #p = 1; static #sp = 2; q = this.#p + 1; [key("ck")] = 3;
  static s = A.#sp * 10; static { log.push("static " + A.s) }
  constructor(x) { this.x = x; log.push("A " + new.target.name) }
  m() { return "A.m" + this.x } get g() { return this.x * 2 } set g(v) { this.x = v }
  static sm() { return "sm" } [key("cm")]() { return "cm" } get [key("cg")]() { return "cg" }
  #pm() { return "pm" + this.#p } get #pg() { return this.#p * 7 } set #pg(v) { this.#p = v }
  static #spm() { return "spm" }
  priv(o) { return [this.#pm(), this.#pg, (this.#pg = 5, this.#p), #p in o, A.#spm(), this.#p++, ++this.#p, this.#p += 2].join() }
  1() { return "one" } "s k"() { return "sk" }
}
class B extends A {
  y = this.x + 100;
  constructor(x, y) { super(x * 2); this.z = y }
  m() { return "B>" + super.m() + "|" + super.g + "|" + super["m"]() }
  setSuper() { super.g = 9; super["g"] = 11; super.q += 1; return this.x + "," + this.q }
  static sm() { return "B" + super.sm() }
}
class C extends B {}
class D extends A { constructor() { const f = () => super(4); f(); return undefined } }
class E extends A { constructor() { super(1); return { e: 1 } } }
class F extends A { constructor() { } }
class G extends A { constructor() { this.x = 1; super(1) } }
class H extends A { constructor() { super(1); super(2) } }
class I extends A { constructor() { super(1); return 5 } }
class J extends A { constructor(...a) { super(...a, 7) } }
class K { static make() { return new this() } constructor() { this.nt = new.target === K } }
function c1() { log = []; var a = new A(3); return [a.x, a.q, a.ck, a.m(), a.g, (a.g = 4, a.x), A.sm(), a.cm(), a.cg, a[1](), a["s k"](), a.priv(a), a.priv({}).split(",")[3], log.join()].join() }
function c2() { var b = new B(2, 3); return [b.x, b.y, b.z, b.m(), b.setSuper(), B.sm(), Object.keys(b).join("/"), b instanceof A].join() }
function c3() { var c = new C(5, 6); return [c.x, c.y, c.z, c.constructor.name, Object.getPrototypeOf(C) === B].join() }
function c4() { return [new D().x, new E().e, new J(1).x, K.make().nt].join() }
function c5(k) { try { return k === 0 ? A() : k === 1 ? new F() : k === 2 ? new G() : k === 3 ? new H() : k === 4 ? new I() : A.prototype.priv.call({}, {}) } catch (e) { return frames(e) } }
function c6() { var out = []; for (var i = 0; i < 3; i++) { class L { #v = i; static n = i; get() { return this.#v } static has(o) { return #v in o } } out.push(L) } return [new out[0]().get(), new out[2]().get(), out[0].n, out[1].has(new out[1]()), out[1].has(new out[2]())].join() }
function c7() { var X = class Named { who() { return Named.name } }; var Y = class { }; return [new X().who(), Y.name, typeof Named].join() }
function c8() { try { class P extends 5 {} } catch (e) { return frames(e) } }
function c9() { try { class Q extends (Q, Object) {} } catch (e) { return frames(e) } }
function c10() { var o = { get a() { return 1 }, set a(v) { this.b = v }, m() { return super.toString === Object.prototype.toString } }; o.a = 3; return [o.a, o.b, o.m()].join() }
function c11() { class M { static #c = 0; static inc() { return ++M.#c } static [Symbol.iterator]() {} static get #g() { return 1 } static gs() { return M.#g } } M.inc(); return M.inc() + "," + typeof M[Symbol.iterator] + M.gs() }
function c12() { class S { static d() { try { delete super.x } catch (e) { return frames(e) } } static e(k) { try { delete super[k()] } catch (e) { return frames(e) } } } return S.d() + " ; " + S.e(() => "k") }
class P2 { constructor() { return { from: "P2" } } }
class R2 extends P2 { constructor(k) { super(); this.k = k; if (k) { this.j = this.k + 1 } } }
class R3 extends P2 { constructor(k) { if (k) super(); else (() => super())(); this.k = k } }
function c14() { var a = new R2(1), b = new R3(0), c = new R3(1); return JSON.stringify([a, b, c, a instanceof R2, b instanceof R3]) }
function c13() { var r = []; class N { #x = 1; static t(o) { try { return o.#x } catch (e) { return frames(e) } } } r.push(N.t(new N()), N.t({}), N.t(1)); return r.join(" ; ") }
`
	calls := []string{`c1()`, `c2()`, `c3()`, `c4()`, `c5(0)`, `c5(1)`, `c5(2)`, `c5(3)`, `c5(4)`, `c5(5)`, `c6()`, `c7()`, `c8()`, `c9()`, `c10()`, `c11()`, `c12()`, `c13()`, `c14()`,
		`class TopA { constructor(v) { this.v = v } } class TopB extends TopA { get w() { return this.v + 1 } } new TopB(1).w`}
	regDiff(t, src, calls)
}

// TestRegDiffPrivateSites compares get_private and set_private, cached by
// shape: loops over objects of one shape and of several, a class evaluated
// twice, whose methods share their sites with keys of their own, a method
// and an accessor read, a method and a getter-only accessor written, a
// frozen object's field written, a member missing and a primitive, with
// their stack traces.
func TestRegDiffPrivateSites(t *testing.T) {
	src := `
function frames(e) { return e.name + ": " + e.message + " | " + e.stack.split("\n").slice(1, 3).map(function (s) { return s.trim() }).join(" | ") }
function make() {
	return class {
		#n = 0; #m() { return this.#n } get #g() { return this.#n * 2 }
		constructor(extra) { if (extra) this["e" + extra] = extra }
		add(k) { for (var j = 0; j < k; j++) this.#n = (this.#n + j) | 0; return this.#n }
		sum(k) { var s = 0; for (var j = 0; j < k; j++) s += this.#m() + this.#g; return s }
		static peek(o) { try { return o.#n } catch (e) { return frames(e) } }
		static poke(o, v) { try { o.#n = v; return o.#n } catch (e) { return frames(e) } }
		static setM(o) { try { o.#m = 1 } catch (e) { return frames(e) } }
		static setG(o) { try { o.#g = 1 } catch (e) { return frames(e) } }
	};
}
var A = make(), B = make();
function p1() { var r = [], a = new A(), b = new B(); r.push(a.add(50), b.add(7), a.sum(5), b.sum(5)); return r.join() }
function p2() { var r = [], s = 0; for (var i = 0; i < 40; i++) { var o = new A(i % 5); s += o.add(i) + A.peek(o) } r.push(s, A.peek(new B()), B.peek(new A())); return r.join(" ; ") }
function p3() { var a = new A(), r = [A.poke(a, 5), A.poke(Object.freeze(a), 6), A.poke({}, 1), A.poke(1, 1), A.peek(null), A.setM(a), A.setG(a)]; return r.join(" ; ") }
function p4() { var a = new A(), b = new B(), s = 0; for (var i = 0; i < 30; i++) { s += A.poke(a, i) + B.poke(b, i * 2); if (i == 20) s += typeof A.poke(b, 1) } return s }
`
	regDiff(t, src, []string{`p1()`, `p2()`, `p3()`, `p4()`})
}

// TestRegDiffBigIntArith compares BigInt arithmetic in register code, which
// regSlow makes by bigOp and bigBits and regCompare and regStep by their
// short forms: an int64's and past it, division by zero, a Number beside a
// BigInt, >>>, and the loops of the micro-benchmarks.
func TestRegDiffBigIntArith(t *testing.T) {
	src := `
var B = BigInt;
function ops(a, b) {
	var r = [a + b, a - b, a * b, a & b, a | b, a ^ b, a << B(3), a >> B(2), a < b, a <= b, a > b, a >= b, a == b, a === b];
	if (b !== B(0)) r.push(a / b, a % b);
	r.push(a ** B(3));
	var c = a; c++; r.push(c); c--; c--; r.push(c);
	return r.join();
}
function big() { var x = B(1); for (var i = 0; i < 70; i++) x = x * B(3) + B(i); return String(x) + " " + String(x % B(1000003)) + " " + String(x >> B(64)) }
function divmod() { var s = 0, t = B(0); for (var j = 0; j < 50; j++) { var x = B(9007199254740993000) + B(j); while (x > B(0)) { t += x % B(10); x /= B(10) } } return String(t) }
function xorshift() { var x = B(88172645463325252); for (var i = 0; i < 100; i++) { x ^= BigInt.asUintN(64, x << B(13)); x ^= x >> B(7); x ^= BigInt.asUintN(64, x << B(17)) } return String(x) }
function errs() { var r = []; try { B(1) / B(0) } catch (e) { r.push(e.name) } try { B(1) + 1 } catch (e) { r.push(e.name) } try { B(1) >>> B(1) } catch (e) { r.push(e.name) } return r.join() }
function wraps(x, y) {
	var r = [], n = [64, 63, 32, 1, 0, 65, 3.5, -1], m = { asUintN: function (w, v) { return "m" + w + ":" + v } };
	for (var i = 0; i < n.length; i++) {
		var w = n[i];
		try {
			r.push(BigInt.asUintN(w, x + y), BigInt.asUintN(w, x - y), BigInt.asUintN(w, x * y), BigInt.asIntN(w, x * y),
				BigInt.asUintN(w, x & y), BigInt.asIntN(w, x | y), BigInt.asUintN(w, x ^ y), BigInt.asIntN(w, x << B(13)),
				BigInt.asUintN(w, x >> B(7)), BigInt.asIntN(w, x << y), m.asUintN(w, x * y))
		} catch (e) { r.push(e.name) }
	}
	try { BigInt.asUintN(64, x + 1) } catch (e) { r.push(e.name) }
	try { BigInt.asUintN(64, 2 * 3) } catch (e) { r.push(e.name) }
	r.push(BigInt.asUintN(64, 5 * 3), tw(x, y), tw(tw(x, y), x))
	return r.join()
}
function tw(x, y) { "use strict"; return BigInt.asIntN(64, x * y) }
`
	calls := []string{
		"ops(B(7), B(3))", "ops(B(-7), B(3))", "ops(B(9223372036854775807), B(2))", "ops(B(-9223372036854775808), B(-1))",
		"ops(B(2) ** B(70), B(12345))", "ops(B(5), B(0))", "big()", "divmod()", "xorshift()", "errs()",
		"wraps(B(7), B(3))", "wraps(B(-123456789012345), B(3))", "wraps(B(2) ** B(63) + B(5), B(-1))", "wraps(B(2) ** B(70), B(9))",
		"wraps(B(-9223372036854775808), B(-1))",
	}
	regDiff(t, src, calls)
}

// TestRegDiffConstructions compares constructions entered as register
// code: a derived constructor's this binding -- read before super() binds
// it, bound by an arrow, by eval code or by an iterator's return handler
// after the body has left, a second super() -- the results a derived
// construction checks after its body, a base constructor with a direct
// eval or inside a with, new.target an arrow captures, and the stack
// traces of errors thrown in them.
func TestRegDiffConstructions(t *testing.T) {
	src := `
function frames(e) { return e.name + ": " + e.message + " | " + e.stack.split("\n").slice(1, 3).map(function (s) { return s.trim() }).join(" | ") }
var seen = [];
class A { constructor(x) { this.x = x; this.nt = new.target.name; seen.push("A" + x) } }
class D1 extends A { constructor() { var f = () => super(2); try { throw null } catch (e) { return } finally { f() } } }
var iter = { [Symbol.iterator]() { return this }, next() { return { done: false } }, return() { this.f(); return { done: true } } }
class D2 extends A { constructor() { iter.f = () => super(3); for (var k of iter) { return } } }
class D3 extends A { constructor() { super(4); this.arrow = () => this; return this.arrow } }
class D4 extends A { constructor() { var g = () => this; try { g() } catch (e) { seen.push(e.name) } super(5); this.same = g() === this } }
class D5 extends A { constructor() { eval("super(6)"); this.e = eval("this.x + 1") } }
class D6 extends A { constructor(k) { if (k) { super(7) } else { (() => super(8))() } eval("this.k = k"); this.direct = (0, eval)("typeof this") } }
class E2 { constructor(x) { eval("this.v = x") } }
class D7 extends E2 { constructor() { var n; try { eval("this") } catch (e) { n = e.name } super(1); this.n = n + eval("this.constructor.name") } }
class D8 extends A { constructor() { return } }
class D9 extends A { constructor() { super(1); return 1 } }
class D10 extends A { constructor() { super(1); super(2) } }
class D11 extends A { constructor() { var t = () => new.target; super(1); this.t = t() === D11 } }
class D12 extends A { constructor() { super(1); null.x } }
class D13 extends A { constructor() { var f = () => eval("super(2)"); f(); this.q = (() => eval("this.x"))() } }
class D14 extends A { constructor() { super(1); this.p = (async () => this.x)() } }
function FB(x) { this.fb = x; this.nt = new.target === FB }
class D15 extends FB { constructor() { super(3) } }
function E1(x) { eval("var y = x * 2"); this.y = y; this.nt = new.target === E1 }
var W; with ({ k: 9 }) { W = function (j) { this.k = k + j } }
function Rec(n) { this.n = n; this.next = n > 0 ? new Rec(n - 1) : null }
function Tail(n) { this.n = n; return mk(n) } function mk(n) { return n > 1 ? { made: n } : 0 }
function c1() { seen = []; var r = [new D1().x, new D2().x, typeof new D3(), new D3()().x, new D4().same, new D5().e, new D6(1).k, new D6(0).x, new D6(0).direct]; return r.join() + " " + seen.join() }
function c2() { return [new D7().n, new D11().t, new D13().q, new D15().fb, new D15().nt, new E1(4).y, new E1(4).nt, new W(1).k].join() }
function c3(k) { try { return k === 0 ? new D8() : k === 1 ? new D9() : k === 2 ? new D10() : new D12() } catch (e) { return frames(e) } }
class DR extends A { constructor(n) { super(n); this.c = n > 0 ? new DR(n - 1) : null } }
class DS extends A { constructor(n) { var c = n > 0 ? new DS(n - 1) : null; super(n); this.c = c; this.ok = this.x === n } }
function c4() { var r = new Rec(20), n = 0; while (r) { n += r.n; r = r.next } return [n, JSON.stringify(new Tail(1)), JSON.stringify(new Tail(2)), JSON.stringify(new DR(3)), JSON.stringify(new DS(3))].join() }
function c5() { return Reflect.construct(D5, [], D15).constructor.name + "," + Reflect.construct(E1, [1], W).nt }
async function c6() { return await new D14().p }
var out6; c6().then(function (v) { out6 = v })
`
	calls := []string{`c1()`, `c2()`, `c3(0)`, `c3(1)`, `c3(2)`, `c3(3)`, `c4()`, `c5()`, `out6`}
	regDiff(t, src, calls)
}

// TestRegDiffGenerators compares generators, async functions, async
// generators, yield* and for await as register code: what each
// resumption delivers -- a value, a throw, a return through finally
// clauses and loops' iterators -- the delegation's forwarding and closing
// order, the creation's binding of parameters, captures across
// suspensions, and the errors' positions.
func TestRegDiffGenerators(t *testing.T) {
	src := `
var log = [];
var out = [];
function frames(e) { return e.name + ": " + e.message + " | " + e.stack.split("\n").slice(1, 3).map(function (s) { return s.trim() }).join(" | ") }
function* g1(a, b = a + 1) { let x = yield a; try { yield x + b; } finally { log.push("fin") } return 7 }
function c1() { var it = g1(1); return JSON.stringify([it.next(), it.next(10), it.next(), it.next(), log.splice(0)]) }
function c2() { var it = g1(1); it.next(); it.next(5); return JSON.stringify([it.return(42), it.next(), log.splice(0)]) }
function c3() { var it = g1(1); it.next(); try { it.throw(new Error("boom")) } catch (e) { return frames(e) + JSON.stringify(it.next()) } }
function* inner() { try { log.push("in"); var s = yield 1; log.push("s" + s); yield 2 } catch (e) { log.push("caught " + e); yield 3 } finally { log.push("inner fin") } return "r" }
function* outer() { var r = yield* inner(); log.push("got " + r); yield 4; return 5 }
function c4() { return JSON.stringify([[...outer()], log.splice(0)]) }
function c5() { var o = outer(); return JSON.stringify([o.next(), o.next("x"), o.throw("t"), o.next(), o.next(), o.next(), log.splice(0)]) }
function c6() { var o = outer(); o.next(); return JSON.stringify([o.return(9), o.next(), log.splice(0)]) }
function* cnt() { let k = 0; const f = () => k; while (true) { k++; var v = yield f(); if (v) k = v } }
function c7() { var c = cnt(); return [c.next().value, c.next().value, c.next(10).value, c.next().value].join() }
function* bad(a = (() => { throw new TypeError("param") })()) { yield a }
function c8() { try { bad(); return "none" } catch (e) { return frames(e) } }
function* g9() { try { for (var x of [1, 2, 3]) { try { yield x } finally { log.push("f" + x) } } } finally { log.push("outer") } }
function c9() { var it = g9(); it.next(); it.next(); return JSON.stringify([it.return("R"), log.splice(0)]) }
function* g10() { yield* [1, 2]; yield* "ab"; var it = { [Symbol.iterator]() { return this }, next(v) { log.push("n" + v); return { value: 0, done: log.length > 2 } }, return(v) { log.push("ret " + v); return { value: "rv", done: true } } }; return yield* it }
function c10() { var it = g10(); var r = [it.next(), it.next(), it.next(), it.next(), it.next("A"), it.next("B"), it.next("C")]; return JSON.stringify([r, log.splice(0)]) }
function c11() { var it = g10(); for (var i = 0; i < 5; i++) it.next(); return JSON.stringify([it.return("q"), log.splice(0)]) }
function* g12() { var e = new Error("here"); yield e.stack.split("\n")[1].trim(); throw new RangeError("late") }
function c12() { var it = g12(); var a = it.next().value; try { it.next() } catch (e) { return a + " ; " + frames(e) } }
function* g13() { yield this === undefined ? "u" : typeof this; yield arguments.length }
function c13() { var o = { g: g13 }; return [o.g().next().value, g13.call(5).next().value].join() }
var c14g = function* () { var x = 0; while (x < 3) { yield x++ } };
function c14() { var s = 0; for (var v of c14g()) s += v; for (var v of c14g()) { if (v == 1) break } return s }
function c15() { var it = g1(1); var r = [it.return(3), it.next()]; var it2 = g1(1); try { it2.throw(4) } catch (e) { r.push(e) } return JSON.stringify(r) }
function* g17(a, b) { arguments[0] = 9; yield a; a = 5; yield arguments[0] + "," + arguments.length; b = 7; yield arguments[1] }
function* g17s(a, ...r) { arguments[0] = 9; yield a + "," + arguments[0] + "," + arguments.length + "," + r.length }
function c17() { return [...g17(1, 2, 3)].join("|") + " " + [...g17s(1, 2)].join() }
function* g18() { var fs = []; for (var i = 0; i < 3; i++) { let j = i; fs.push(() => j + i); yield } return fs.map(f => f()).join() }
function c18() { var it = g18(), r; do { r = it.next() } while (!r.done); return r.value }
function* g16() { var it = yield; it.next() }
function c16() { var it = g16(); it.next(); try { it.next(it) } catch (e) { return frames(e) } }
async function a1(x) { var y = await x; var z = await Promise.resolve(y * 2); try { await Promise.reject(new Error("rej")) } catch (e) { z += 1 } return z }
async function a2() { await null; throw new TypeError("async throw") }
var a3 = async (a, b = await0()) => a + b;
function await0() { return 0 }
async function a4() { var p = Promise.reject(new RangeError("r")); try { return await p } finally { log.push("a4 fin") } }
async function* ag1() { try { yield 1; var v = await Promise.resolve(2); yield v; yield Promise.resolve(3) } finally { log.push("ag fin") } return 4 }
async function* ag2() { var r = yield* ag1(); log.push("ag2 " + r) }
function mkIter(n, bad) { var i = 0; return { [Symbol.asyncIterator]() { return this }, next() { log.push("next"); return Promise.resolve({ value: i, done: i++ >= n }) }, return(v) { log.push("return"); return bad ? 5 : Promise.resolve({ done: true }) } } }
async function f1() { var s = 0; for await (const v of mkIter(3)) s += v; return s }
async function f2() { for await (const v of mkIter(5)) { if (v == 2) break } return "b" }
async function f3() { try { for await (const v of mkIter(5)) { throw new Error("t" + v) } } catch (e) { return e.message } }
async function f4() { for await (const v of mkIter(5)) { return "r" + v } }
async function f5() { try { for await (const v of mkIter(5, true)) { break } } catch (e) { return frames(e) } }
async function f6() { var r = []; for await (const [a, b] of [[1, 2], Promise.resolve([3, 4])]) r.push(a + b); return r.join() }
async function* f7g() { for await (const v of mkIter(4)) { yield v } }
async function f7() { var it = f7g(); await it.next(); await it.next(); return JSON.stringify(await it.return(9)) }
async function f8() { var it = ag2(); var r = []; for (var i = 0; i < 5; i++) r.push(await it.next()); return JSON.stringify(r) }
async function f9() { var it = ag1(); await it.next(); return JSON.stringify([await it.return(Promise.resolve("rr")), await it.next()]) }
function settle(name, p) { p.then(function (v) { out.push(name + "=" + v) }, function (e) { out.push(name + "!" + frames(e)) }) }
function runAsync() { out = []; log = []; settle("a1", a1(3)); settle("a2", a2()); settle("a3", a3(1)); settle("a4", a4()); settle("f1", f1()); settle("f2", f2()); settle("f3", f3()); settle("f4", f4()); settle("f5", f5()); settle("f6", f6()); settle("f7", f7()); settle("f8", f8()); settle("f9", f9()); return "started" }
`
	calls := []string{`c1()`, `c2()`, `c3()`, `c4()`, `c5()`, `c6()`, `c7()`, `c8()`, `c9()`, `c10()`, `c11()`, `c12()`, `c13()`, `c14()`, `c15()`, `c16()`, `c17()`, `c18()`,
		`runAsync()`, `out.sort().join(" ; ") + " // " + log.join()`}
	regDiff(t, src, calls)
}

// TestRegDiffGeneratorSteps compares the lean steps of register
// generators: a for-of's step (regGenStep), next resuming one at once, a
// yield as an instruction of its own, a body starting past end_params, and
// a yield* to a generator stepped directly (regDelegateNext) -- the values
// it yields on, its return ending the delegation, return and throw sent
// through it, a delegate that hands a result over as it is, a delegate
// running, finishing or throwing, and the frames a stack trace sees.
func TestRegDiffGeneratorSteps(t *testing.T) {
	src := `
var log = [];
function frames(e) { return e.name + ": " + e.message + " | " + e.stack.split("\n").slice(1, 4).map(function (s) { return s.trim() }).join(" | ") }
function* count(n) { for (var i = 0; i < n; i++) yield i }
function* inner(n) { try { for (var i = 0; i < n; i++) { var s = yield i; if (s !== undefined) log.push("sent " + s) } return "ret" + n } finally { log.push("inner fin") } }
function* outer(n) { var r = yield* inner(n); log.push("got " + r); return r }
function s1() { var s = 0; for (var v of count(5)) s += v; for (var v of count(9)) { if (v > 2) break; s += v } return s }
function s2() { return JSON.stringify([[...outer(3)], log.splice(0)]) }
function s3() { var g = outer(3); return JSON.stringify([g.next(), g.next("a"), g.return("R"), g.next(), log.splice(0)]) }
function s4() { var g = outer(3); g.next(); try { g.throw(new Error("E")) } catch (e) { return frames(e) + " " + JSON.stringify([g.next(), log.splice(0)]) } }
function* arr() { var r = yield* [1, 2]; log.push("arr ret " + r) }
function* viaArr() { yield* arr() }
function s5() { return JSON.stringify([[...viaArr()], log.splice(0)]) }
function* deep(k) { if (k == 0) { yield "leaf"; return 1 } return 1 + (yield* deep(k - 1)) }
function s6() { var g = deep(4); return JSON.stringify([g.next(), g.next()]) }
function* self() { yield* g7 }
var g7;
function s7() { g7 = self(); try { g7.next() } catch (e) { return frames(e) } }
function* thrower() { yield 1; throw new Error("in delegate") }
function* wrap() { try { yield* thrower() } catch (e) { log.push(frames(e)); yield 9 } }
function s8() { return JSON.stringify([[...wrap()], log.splice(0)]) }
function* stk() { yield frames(new Error("s")) }
function* stkOuter() { yield* stk() }
function s9() { return [...stkOuter()].join() + " " + stkOuter().next().value }
var custom = { [Symbol.iterator]() { return { n: 0, next(v) { return { value: this.n++, done: this.n > 2 } } } } };
function* viaCustom() { var r = yield* custom; log.push("custom ret " + r) }
function s10() { return JSON.stringify([[...viaCustom()], log.splice(0)]) }
function* done() { return "early" }
function* viaDone() { var r = yield* done(); yield r }
function s11() { var d = done(); d.next(); function* again() { return yield* d } return JSON.stringify([[...viaDone()], again().next()]) }
function* plain(a, b) { yield a + b; yield arguments.length }
function s12() { var g = plain(1, 2, 3); var r = [g.next(), g.next(), g.next()]; return JSON.stringify(r) }
function* lazy(a = log.push("default")) { yield a }
function s13() { var g = lazy(); log.push("made"); return JSON.stringify([g.next(), log.splice(0)]) }
function* yThrow() { try { yield 1 } catch (e) { yield "caught " + e } }
function s14() { var g = yThrow(); g.next(); return JSON.stringify([g.throw("t"), g.next()]) }
var holder = { gen: function* () { yield frames(new Error("m")) } };
function* notIter() { yield* 5 }
function* member() { yield* holder.gen() }
function* member2() { yield* holder["gen"]() }
function s15() { var r = [[...member()].join(), [...member2()].join()]; try { notIter().next() } catch (e) { r.push(frames(e)) } return r.join(" ; ") }
`
	calls := []string{`s1()`, `s2()`, `s3()`, `s4()`, `s5()`, `s6()`, `s7()`, `s8()`, `s9()`, `s10()`, `s11()`, `s12()`, `s13()`, `s14()`, `s15()`}
	regDiff(t, src, calls)
}

// TestRegDiffLoopBindings compares lexical bindings a closure keeps in a
// loop, each turn's its own: for heads' let, copied from turn to turn,
// for-of and for-in heads, blocks of a loop's body, catch clauses, and a
// head's expression's closure of the binding in its dead zone.
func TestRegDiffLoopBindings(t *testing.T) {
	src := `
function t1() { var fs = []; for (let i = 0; i < 3; i++) { fs.push(() => i); } return fs.map(f => f()).join() }
function t2() { var fs = []; for (let i = 0, g = () => i; i < 3; i++) { fs.push(() => i + "/" + g()); if (i == 1) continue; } return fs.map(f => f()).join() }
function t3() { var fs = []; for (const x of [1, 2, 3]) fs.push(() => x); for (let [a, b = a] of [[1, 2], [3]]) fs.push(() => a + b); return fs.map(f => f()).join() }
function t4() { var fs = []; var i = 0; while (i < 3) { let j = i * 10; fs.push(() => j); j++; i++ } return fs.map(f => f()).join() }
function t5() { var fs = []; for (var i = 0; i < 3; i++) { try { throw i } catch (e) { fs.push(() => e); e = -1 } } for (var i = 0; i < 2; i++) { try { throw [i] } catch ([e]) { fs.push(() => e) } } return fs.map(f => f()).join() }
function t6() { var fs = []; for (let i = 0; i < 3; i++) { fs.push(() => i++); } return fs.map(f => f()).join() + ";" + fs.map(f => f()).join() }
function t7() { var fs = []; for (var k in {a: 1, b: 2}) { let kk = k; fs.push(() => kk) } for (const k in {c: 1, d: 2}) fs.push(() => k); return fs.map(f => f()).join() }
function t8() { var fs = []; for (let i = 0; i < 2; i++) for (let j = 0; j < 2; j++) fs.push(() => i * 10 + j); return fs.map(f => f()).join() }
function t9() { var fs = []; outer: for (let i = 0; i < 3; i++) { for (const x of [1, 2]) { fs.push(() => i + ":" + x); if (x == 1) continue outer } } return fs.map(f => f()).join() }
function t10() { var g; try { for (const x of (g = () => x, [1])) {} } catch (e) { } try { return g() } catch (e) { return e.name + ": " + e.message } }
function t11() { var fs = []; for (let i = 0; i < 3; i++) { let r = () => i; if (i == 1) { fs.push(r); break } fs.push(r) } return fs.map(f => f()).join() }
function* t12g() { for (let i = 0; i < 3; i++) yield () => i }
function t12() { return [...t12g()].map(f => f()).join() }
function t13() { var fs = []; for (let i = 0; i < 2; i++) { do { let k = i; fs.push(() => k) } while (false) } return fs.map(f => f()).join() }
function t14() { let y = 'out'; var pb = () => y, pe, pd; for (const [y, z = pd = () => y] of (pe = () => y, [[1]])) {} try { pe() } catch (e) { return [pb(), pd(), e.name].join() } }
`
	calls := []string{`t1()`, `t2()`, `t3()`, `t4()`, `t5()`, `t6()`, `t7()`, `t8()`, `t9()`, `t10()`, `t11()`, `t12()`, `t13()`, `t14()`,
		`var pd, pe, pb; for (let [x, _ = pd = function() { return x }] of (pe = function() { typeof x }, [['in']])) pb = function() { return x }; try { pe() } catch (e) { [pd(), pb(), e.name].join() }`}
	regDiff(t, src, calls)
}

// TestRegDiffNames compares names read and written in their dead zone --
// a closure's upvalue, a binding of the function's own a for-of head or
// an assignment writes before its declaration -- a constant's upvalue
// written, a function expression's own name written, and for-of and
// for-in loops that store to a global var, a global let, a checked
// binding or a constant, with the errors' positions.
func TestRegDiffNames(t *testing.T) {
	src := `
function e(f) { try { return String(f()) } catch (x) { return x.name + ": " + x.message + " | " + x.stack.split("\n").slice(1, 3).map(function (s) { return s.trim() }).join(" | ") } }
function a1() { var f = () => x; var r = e(f); let x = 1; return r + "," + f() }
function a2() { var f = () => { x = 2 }; var r = e(f); let x = 1; f(); return r + "," + x }
function a3() { var f = () => { c = 2 }; const c = 1; return e(f) }
function a4() { var f = () => x++; var r = e(f); let x = 1; f(); return r + "," + x }
function a5() { return e(() => { for (h of [1]) {} let h; }) }
function a6() { var r = []; for (let x of [1]) { r.push(e(() => { y += 1 })); let y = 2; } return r.join() }
var fe = function self() { self = 1; return typeof self };
var fs = function self() { "use strict"; return e(() => { self = 1 }) };
function a7() { var f = () => typeof x; var r = e(f); let x; return r }
function a8() { return e(() => { z = 1; let z; }) }
function a9() { const q = 1; return e(() => { for (q of [2]) {} }) }
var gv; let gl;
function a10() { for (gv of [1, 2]) {} for (gl in {a: 1}) {} return gv + gl }
`
	calls := []string{`a1()`, `a2()`, `a3()`, `a4()`, `a5()`, `a6()`, `fe()`, `fs()`, `a7()`, `a8()`, `a9()`, `a10()`,
		`var out = []; for (var tv of [1, 2]) out.push(tv); for (let tl of [3]) out.push(tl); for (tk in {k: 1}) out.push(tk); out.join() + tv + tk`}
	regDiff(t, src, calls)
}

// TestRegDiffLabelsSwitchDelete compares labelled statements that are no
// loop -- blocks, a try, a switch, labels on labels -- a switch's lexical
// and class declarations, and delete of a name and of an optional chain,
// with the errors' positions.
func TestRegDiffLabelsSwitchDelete(t *testing.T) {
	src := `
function e(f) { try { return String(f()) } catch (x) { return x.name + ": " + x.message + " | " + x.stack.split("\n").slice(1, 3).map(function (s) { return s.trim() }).join(" | ") } }
function l1() { var r = []; a: { r.push(1); if (r.length) break a; r.push(2) } return r.join() }
function l2() { var n = 0; a: b: for (var i = 0; i < 5; i++) { for (;;) { n++; if (i % 2) continue a; if (i == 4) break b; continue b } } return n + ":" + i }
function l3() { var r = []; out: try { r.push("t"); break out; } finally { r.push("f") } r.push("e"); return r.join() }
function l4() { var closed = 0; var it = { [Symbol.iterator]() { return { next() { return { value: 1, done: false } }, return() { closed++; return {} } } } }; blk: { for (var x of it) { break blk } } return closed }
function l5(v) { var r = ""; s: switch (v) { case 1: r += "a"; break s; case 2: r += "b"; default: r += "d" } return r }
function l6() { var r = []; for (var i = 0; i < 3; i++) { x: { if (i == 1) break; r.push(i) } } return r.join() + i }
function l7() { var r = []; a: { b: { r.push(1); break a; } r.push(2) } c: d: { r.push(3); break c; } return r.join() }
function l8() { var r = []; a: if (r.length == 0) { r.push(1); break a; } else r.push(2); b: { try { r.push(3); break b } catch (e) {} finally { r.push(4) } } return r.join() }
function l9() { var r = []; for (var i = 0; i < 3; i++) { a: { b: { if (i == 0) continue; if (i == 1) break b; break a } r.push("b" + i) } r.push(i) } return r.join() }
function s1(v) { switch (v) { case 1: let x = 10; return x; case 2: x = 5; return x; default: const y = v; return y } }
function s2(v) { switch (v) { case 0: class C { m() { return "c" } } return new C().m(); case 1: return (() => typeof C)() } }
function s3() { var fs = []; for (var i = 0; i < 3; i++) { switch (i) { case 0: case 1: let k = i; fs.push(() => k); break; default: let j; fs.push(() => j) } } return fs.map(f => f()).join() }
function s4() { switch (1) { case 0: let z = 1; case 1: return (() => z)() } }
function s5(v) { var r = []; switch (v) { case 0: let a = 1; r.push(a); case 1: a = 2; r.push(a); } return r.join() }
var gv = 1; globalThis.gd = 2;
function d1() { var v = 1; let l = 2; return [delete v, delete l, delete d1, delete gv, delete nosuch, delete gd, typeof gd, v, l].join() }
function d2() { var u = 1; return (() => delete u)() + "," + u }
function d3() { var o = {b: {c: 1}}, n = null; return [delete o?.b.c, "c" in o.b, delete n?.b, delete n?.b.c, delete o.b?.["x"], delete o?.b, "b" in o].join() }
function d4() { "use strict"; var o = Object.freeze({b: 1}); return delete o?.b }
function d5() { var o = {}; return delete o?.b.c }
function d6() { var o = {m() { return {x: 1} }}, n; return [delete o?.m().x, delete n?.m().x, delete this, delete 1, delete (0, o)].join() }
function d7() { var o = Object.freeze({b: 1}); return [delete o?.b, "b" in o].join() }
`
	calls := []string{`l1()`, `l2()`, `l3()`, `l4()`, `l5(1)`, `l5(2)`, `l5(3)`, `l6()`, `l7()`, `l8()`, `l9()`,
		`e(() => s1(1))`, `e(() => s1(2))`, `e(() => s1(3))`, `e(() => s2(0))`, `e(() => s2(1))`, `s3()`, `e(s4)`, `e(() => s5(0))`, `e(() => s5(1))`,
		`d1()`, `d2()`, `d3()`, `e(d4)`, `e(d5)`, `d6()`, `d7()`,
		`"q"; L: { "w"; break L; 3 }`, `"q"; L: { break L; }`, `"q"; L: M: { "w"; if (true) break M; 3 }`,
		`"q"; L: switch (1) { case 1: "s"; break L }`, `globalThis.gt = 1; var gw = 2; [delete gt, delete gw, typeof gt].join()`,
		// A switch case's constant, past its dead zone, is still one.
		`(() => { switch (1) { case 1: const c = 1; try { c = 2 } catch (x) { return x.name + c } } })()`,
		`e(() => { switch (1) { case 1: const c = 1; c += 2; return c } })`,
		`e(() => { switch (1) { case 1: const c = 1; c++; return c } })`,
		`e(() => { switch (1) { case 1: const c = 1; [c] = [5]; return c } })`,
		`e(() => { switch (1) { case 1: const c = 1; for (c of [1]); return c } })`,
		`e(() => { switch (1) { case 1: const c = 1; c ??= 3; return c && (c &&= 5) } })`,
		`e(() => { switch (2) { case 1: const c = 1; case 2: c = 3; return c } })`,
		`e(() => { switch (1) { case 1: const c = 1; (() => { c = 2 })(); return c } })`}
	regDiff(t, src, calls)
}

// TestRegDiffBlockFunctions compares function declarations in blocks, in
// switches and behind labels: strict code's, a block's own binding made as
// the block is entered, Annex B's var assigned as the declaration is
// evaluated, in loops, async ones, and a thrower's stack.
func TestRegDiffBlockFunctions(t *testing.T) {
	src := `
function b1() { "use strict"; var r = []; { r.push(f()); function f() { return 1 } } return r.join() + typeof f }
function b2() { var r = [typeof f]; { r.push(typeof f); function f() { return 2 } } r.push(typeof f); return r.join() }
function b3() { { f = 1; function f() {} } return typeof f }
function b4() { var fs = []; for (var i = 0; i < 3; i++) { function g() { return i } fs.push(g) } return fs.map(f => f()).join() + (fs[0] === fs[1]) }
function b5() { var fs = []; for (let i = 0; i < 3; i++) { { function g() { return i } fs.push(g) } } return fs.map(f => f()).join() }
function b6(v) { switch (v) { case 1: function s() { return "s" } return s(); default: return typeof s } }
function b7() { L: function f() { return 7 } var r; { r = g(); L: M: function g() { return 8 } } return f() + r }
function b8() { { async function af() { return 7 } var p = af() } return typeof af + (p instanceof Promise) }
function b9() { var get = () => f; { function f() { return 9 } } return get()() }
function b10(f) { { function f() {} } return typeof f }
function b11() { { function thrower() { throw new Error("x") } } try { thrower() } catch (e) { return e.stack.split("\n").slice(0, 3).map(s => s.trim()).join(" | ") } }
function b12() { var h = () => typeof g, x; { x = h(); function g() {} } return x + "," + typeof g }
function b13() { var r = []; for (var i = 0; i < 2; i++) { r.push(typeof k); { function k() {} } } return r.join() }
function b14() { "use strict"; var fs = []; for (let i = 0; i < 2; i++) { function q() { return i } fs.push(q) } return fs.map(f => f()).join() }
`
	calls := []string{`b1()`, `b2()`, `b3()`, `b4()`, `b5()`, `b6(1)`, `b6(2)`, `b7()`, `b8()`, `b9()`, `b10(1)`, `b11()`, `b12()`, `b13()`, `b14()`,
		`{ function tf() { return "tf" } } tf()`, `"use strict"; { function ts() {} } typeof ts`, `{ tq = 5; function tq() {} } typeof tq`,
		`L: function tl() { return "tl" } tl()`}
	regDiff(t, src, calls)
}

// TestRegDiffRestArguments compares a rest parameter nothing reads beside
// the arguments object, whose register the rest's array was made in.
func TestRegDiffRestArguments(t *testing.T) {
	src := `
function r1(..._) { return arguments.toString() + arguments.length }
function r2(x, ...r) { return arguments.length + ":" + x }
function r3(x, ...[a, b]) { return arguments.length + ":" + a + b }
function r4(...r) { var k = 1; return arguments.length + k }
`
	regDiff(t, src, []string{`r1(1, 2)`, `r2(1, 2, 3)`, `r3(1, 2, 3)`, `r4(4, 5)`, `r4()`})
}

// TestRegDiffArgumentsForms compares the arguments object of strict
// functions that end in tail calls -- handed on, kept across calls made
// in place past the tail-call depth, read after -- and of functions that
// name eval without calling it.
func TestRegDiffArgumentsForms(t *testing.T) {
	src := `
function t1(n) { "use strict"; if (n === 0) return arguments.length; return t1(n - 1, 1, 2) }
function t2(n, acc) { "use strict"; var a = arguments; if (n === 0) return acc + ":" + a.length; return t2(n - 1, acc + a[0]) }
function g3(a, b) { return a.length + ":" + a[0] + ":" + b }
function t3() { "use strict"; return g3(arguments, arguments.length) }
function t4(n) { "use strict"; var keep = []; function go(k) { keep.push(arguments); return k === 0 ? keep.length : go(k - 1, k) } return go(n) + ":" + keep[3][1] + ":" + keep[keep.length - 1].length }
function t5() { "use strict"; return arguments.length > 1 ? t5(...[].slice.call(arguments, 1)) : arguments[0] }
function e1() { var x = eval; return typeof x }
function e2(a) { var x = eval; return [x === undefined, arguments.length, a].join() }
function e3() { "use strict"; return eval }
`
	regDiff(t, src, []string{`t1(5)`, `t1(100)`, `t2(5, 0)`, `t2(100, 0)`, `t3(7, 8)`, `t4(5)`, `t4(80)`, `t5(1, 2, 3, 4)`,
		`e1()`, `e2(1, 2)`, `typeof e3()`})
}

// TestRegDiffImportCalls compares import(), import.source() and
// import.defer() calls: their arguments' order, the promise, and what it
// settles to with no loader, read once the jobs have run.
func TestRegDiffImportCalls(t *testing.T) {
	src := `
var log = [], settled = [];
function i1() { var p = import((log.push(1), "./a.js"), (log.push(2), { with: {} })); p.catch(() => 0); return [p instanceof Promise, log.join()].join() }
function i2(s) { return import(s).then(() => "ok", e => e.name) }
function i3(s) { return import.source(s).then(() => "ok", e => e.name) }
function i4(s) { return import.defer(s).then(() => "ok", e => e.name) }
function i5() { "use strict"; return import({ toString() { throw new RangeError("x") } }).then(() => "ok", e => e.name) }
function run() { for (var f of [() => i2("./a.js"), () => i3("./a.js"), () => i4("./a.js"), i5, () => i2(Symbol())]) f().then(v => settled.push(v)) }
`
	regDiff(t, src, []string{`i1()`, `run(); "ran"`, `settled.join()`})
}

// TestRegDiffUsing compares using and await using declarations: in
// blocks, function bodies, for heads and for-of heads, sync and async,
// left normally, by break, continue, return, a throw and a generator's
// return, with disposers that throw (SuppressedError), null values and a
// value with no disposer.
func TestRegDiffUsing(t *testing.T) {
	src := `
var log = [];
function res(n, fail) { return { [Symbol.dispose]() { log.push("d" + n); if (fail) throw new Error("e" + n) } } }
function ares(n, fail) { return { async [Symbol.asyncDispose]() { log.push("a" + n); if (fail) throw new Error("ae" + n) } } }
function show(f) { log = []; try { var v = f(); return log.join() + "=" + v } catch (e) { return log.join() + "!" + e.name + ":" + (e.message || "") + (e.error ? "/" + e.error.message + "/" + (e.suppressed && e.suppressed.message) : "") } }
function u1() { using a = res(1), b = res(2); { using c = res(3); log.push("in") } return "r" }
function u2() { using a = res(1, true); using b = res(2, true); return "r" }
function u3() { using a = res(1); throw new Error("body") }
function u4() { for (using x = res(1); log.length < 1;) log.push("t"); for (using y of [res(2), null, res(3)]) { if (!y) continue; log.push("b") } return "r" }
function u5() { var r = []; for (var i = 0; i < 3; i++) { using z = res(i); if (i == 1) break; r.push(i) } return r.join() }
function u6() { using q = 5 }
function u7() { using n = null, u = undefined; return "ok" }
function* u8() { using a = res(1); yield 1; yield 2 }
function u9() { var g = u8(); g.next(); g.return(7); return "done" }
function u10() { var fs = []; for (using x of [res(1), res(2)]) fs.push(() => typeof x); return fs.map(f => f()).join() }
function u11(k) { switch (k) { case 1: { using a = res(1); return "one" } default: return "def" } }
async function v1() { await using a = ares(1), b = res(2); log.push("body"); return "r" }
async function v2() { await using a = ares(1, true); await using b = ares(2, true); return "r" }
async function v3() { for (await using x of [ares(1), ares(2)]) log.push("b"); { await using y = null; } return "r" }
async function v4() { await using a = ares(1); throw new Error("body") }
async function v5() { for await (await using x of [ares(1), Promise.resolve(ares(2))]) log.push("b"); return "r" }
async function shows(f) { log = []; try { var v = await f(); return log.join() + "=" + v } catch (e) { return log.join() + "!" + e.name + ":" + e.message + (e.error ? "/" + e.error.message + "/" + (e.suppressed && e.suppressed.message) : "") } }
var aout = [];
function runAsync() { (async () => { for (var f of [v1, v2, v3, v4, v5]) aout.push(await shows(f)) })(); return "ran" }
`
	calls := []string{}
	for _, f := range []string{"u1", "u2", "u3", "u4", "u5", "u6", "u7", "u9", "u10", "() => u11(1)"} {
		calls = append(calls, "show("+f+")")
	}
	calls = append(calls, `runAsync()`, `aout.join(" | ")`)
	regDiff(t, src, calls)
}

// TestRegDiffEval compares direct evals: in strict functions, arrows,
// methods and constructors, and in top-level code, reading and writing the
// caller's bindings, its this, arguments, new.target and super, closures
// the code makes over them, a binding in its dead zone, a spread, a value
// that is no string, a callee that is not the realm's eval, and the errors
// with their stack traces.
func TestRegDiffEval(t *testing.T) {
	src := `
var frames = function (e) { return e.name + ": " + e.message + " | " + e.stack.split("\n").slice(1).map(function (s) { return s.trim() }).join(" | ") };
function s1(a) { 'use strict'; var x = 1; let y = 2; const z = 3; eval("x = x + a; y++"); return x + y + z }
function s2() { 'use strict'; eval("var q = 1"); return typeof q }
function s3() { 'use strict'; return eval("this") }
function s4() { 'use strict'; return eval("arguments.length + ':' + arguments[0]") }
function S5() { 'use strict'; this.v = eval("new.target === S5") }
function s6() { 'use strict'; return String(eval(42)) + String(eval()) + String(eval({})) }
function s7() { 'use strict'; return eval("1") }
function s8() { 'use strict'; var x = 4; return eval(...["x * 2", 9]) }
function s9() { 'use strict'; return eval("(function inner() { throw new Error('in') })()") }
function s10() { 'use strict'; try { eval("t") } catch (e) { return e.name } let t = 1 }
function s11() { 'use strict'; var n = 1; var g = eval("(function () { return ++n })"); g(); g(); return n }
function s12() { 'use strict'; var fs = []; for (let i = 0; i < 3; i++) fs.push(eval("() => i")); return fs.map(f => f()).join() }
function s13() { 'use strict'; var a = 1; return (() => eval("a + this.k"))() }
var o14 = { __proto__: { m() { return "p" } }, m() { 'use strict'; return eval("super.m()") + eval("'q'") } };
function s15(a) { 'use strict'; var g = () => a; eval("a = 2"); return g() + a }
function s16() { 'use strict'; return eval("1 +") }
function s17() { 'use strict'; const c = 1; eval("c = 2") }
class K { constructor(x) { this.x = eval("x * 3") } }
class B {}
class D extends B { constructor() { var n; try { eval("this") } catch (e) { n = e.name } super(); this.n = n + eval("this.constructor.name") } }
function s18(n) { 'use strict'; return n == 0 ? "end" : eval("s18(n - 1)") }
function s19() { 'use strict'; var r = []; for (var i = 0; i < 2; i++) { let j = i * 10; r.push(eval("j + i")) } return r.join() }
function s20(a, b) { 'use strict'; return eval("[a, b, typeof arguments, arguments.length]").join() }
var own = { toString() { return "own" } };
`
	calls := []string{
		`s1(5)`,
		`s2()`,
		`s3.call(5)`,
		`s4("a", "b")`,
		`new S5().v`,
		`s6()`,
		`s7()`,
		`(function () { var e = globalThis.eval; globalThis.eval = function (s) { return "mine:" + s + this }; try { return s7() } finally { globalThis.eval = e } })()`,
		`s8()`,
		`try { s9() } catch (e) { frames(e) }`,
		`s10()`,
		`s11()`,
		`s12()`,
		`s13.call({ k: 2 })`,
		`o14.m()`,
		`s15(1)`,
		`try { s16() } catch (e) { e.name + ": " + e.message }`,
		`try { s17() } catch (e) { frames(e) }`,
		`new K(2).x`,
		`new D().n`,
		`s18(30)`,
		`s19()`,
		`s20(1)`,
		`var tv = 1; eval("tv + 1")`,
		`eval("var newGlobal = 3"); newGlobal`,
		`let tl = 5; eval("tl * 2")`,
		`{ let b = 7; eval("b + 1") }`,
		`for (let i = 0; i < 2; i++) eval("i")`,
		`eval("let inner = 1; inner") + typeof inner`,
		`try { eval("(function () { throw new Error('top') })()") } catch (e) { frames(e) }`,
		`(function () { 'use strict'; var e = globalThis.eval; globalThis.eval = own; try { return eval("x") } catch (e) { return e.name } finally { globalThis.eval = e } })()`,
	}
	regDiff(t, src, calls)
}

// TestRegDiffWith compares with statements and the variables a direct eval
// declares in sloppy functions: reads, writes, compound and logical
// assignments, updates, typeof, delete and calls through a with object --
// its receiver, its own eval, Symbol.unscopables -- patterns and var
// initializers that resolve before their value, the object popped on
// every way out, closures and generators made inside, a getter's throw
// with its stack trace, and an eval's var, function and let.
func TestRegDiffWith(t *testing.T) {
	src := `
var frames = function (e) { return e.name + ": " + e.message + " | " + e.stack.split("\n").slice(1).map(function (s) { return s.trim() }).join(" | ") };
function w1(o) { var x = 1, r = []; with (o) { r.push(x); x = 5; r.push(x); x += 2; x++; r.push(typeof y, typeof x) } return r.join() + "/" + x + "/" + JSON.stringify(o) }
var wo = { v: 3, f() { return this.v } };
function w2() { with (wo) return f() }
function w3(o) { with (o) { delete z; return typeof z + delete nope } }
function w4(o) { var s = 0; with (o) { for (var i = 0; i < 3; i++) { if (i == 1) continue; s += k } } return s }
function w5(o) { for (var i = 0; i < 3; i++) { with (o) { if (i == 1) break } } return typeof k }
function w6(o) { try { with (o) { throw new Error("in") } } catch (e) { return e.message + typeof q } }
function w7(o) { with (o) { var fn = function () { return a } } return fn() }
function w8(o) { with (o) { var [p, q] = [1, 2] } return [p, q, o.p, o.q].join() }
function w9(o) { with (o) { var t = 3 } return t + "," + o.t }
function w10(o) { with (o) { a ||= 4; b ??= 5; c &&= 6 } return JSON.stringify(o) }
var ev = { eval(s) { return "own:" + s + (this === ev) } };
function w12() { with (ev) return eval("1") }
function* wg(o) { with (o) { yield x; yield x + 1 } return typeof x }
var unsc = { u: 1, [Symbol.unscopables]: { u: true } }; var u = "global";
function w13() { with (unsc) return u }
function w14(o) { with (o) { return x } }
function w15(o) { with (o) { return (function () { 'use strict'; return typeof v })() } }
function w16(o) { var n = 0; with (o) { for (k in { a: 1, b: 2 }) n++; for (m of [1]) n++ } return n + JSON.stringify(o) }
function w17(o) { with (o) { with ({ x: "inner" }) { var r = x + y } } return r }
function w18(o) { with (o) { return x } }
function e1() { eval("var ev1 = 5"); return ev1 }
function e2() { var x = 1; eval("var x = 2"); return x }
function e3() { eval("var y = 1"); return (function () { return y })() }
function e4(a) { eval("a = 7"); return a + arguments[0] }
function e5() { eval("function inner() { return 9 }"); return inner() }
function e7() { eval("var q = 1"); var d = delete q; return d + typeof q }
function e8() { return eval("this") === globalThis }
function e9() { var eval = function (s) { return "local:" + s }; return eval("1") }
function e10() { eval("let lx = 1"); return typeof lx }
function e11() { for (var i = 0; i < 2; i++) eval("var c" + i + " = i"); return c0 + c1 }
function e12() { eval("var x = 1"); with ({}) { return x } }
function* eg() { eval("var gv = 3"); yield gv }
function e13() { eval("throw new Error('ev')") }
function e14() { var r = []; eval("var gx = 1"); r.push(gx); gx = 2; r.push(eval("gx")); gx++; r.push(gx); return r.join() }
function e15() { eval("var fnv = function () { return fnv2 }; var fnv2 = 8"); return fnv() }
`
	calls := []string{
		`w1({})`, `w1({ x: 10 })`, `w1({ y: 1 })`,
		`w2()`,
		`w3({ z: 1 })`,
		`w4({ k: 2 })`,
		`w5({ k: 1 })`,
		`w6({})`,
		`w7({ a: "A" })`, `w7({})`,
		`w8({ p: 0 })`,
		`w9({ t: 0 })`, `w9({})`,
		`w10({ a: 0, b: null, c: 1 })`,
		`w12()`,
		`[...wg({ x: 1 })].join()`, `var it = wg({ x: 1 }); it.next(); x = "g"; it.next().value + it.next().value`,
		`w13()`,
		// The stack form places the probe at the with statement, the
		// register form at the name, where V8 does: the frames compared
		// are the getter's and the caller's.
		`try { w14({ get x() { throw new Error("getter") } }) } catch (e) { var s = e.stack.split("
"); s[1] + s[3] + s.length }`,
		`w15({ v: 1 })`,
		`w16({ k: 0, m: 0 })`, `w16({})`,
		`w17({ y: "Y" })`,
		`try { w18(null) } catch (e) { frames(e) }`,
		`w18(5)`,
		`var tw = { k: 1 }; with (tw) { k = 2 }; tw.k`,
		`e1()`, `e2()`, `e3()`, `e4(1)`, `e5()`, `e7()`, `e8()`, `e9()`, `e10()`, `e11()`, `e12()`,
		`[...eg()].join()`,
		`try { e13() } catch (e) { frames(e) }`,
		`e14()`, `e15()`,
		`typeof ev1 + typeof gx`,
	}
	regDiff(t, src, calls)
}
