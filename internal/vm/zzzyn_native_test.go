//go:build amd64 || arm64

package vm

import (
	"fmt"
	"os"
	"runtime/debug"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
	"github.com/go-quickjs/go-quickjs/internal/compiler"
)

// The native handlers' tests (zzzyn_native_run.go): each runs code by
// the Go table's loop and by the native handlers, with the collector idle
// and as while it marks, and checks the answers are the same, and that
// the native code did the work it is for -- the same exits to Go marking
// or not, the calls made natively where rhCallW enters a frame itself.

// nativeKernels are micro-benchmarks' functions by name, what each needs
// besides, and the work to give it.
var nativeKernels = []struct {
	name string
	deps []string
	n    int
}{
	{"empty_loop", nil, 1000},
	{"loop_locals", nil, 1000},
	{"call_fib", []string{"call_fib_rec"}, 177 * 20},
	{"crypto_am3", []string{"am3"}, 32 * 20},
	{"prop_read", nil, 1000},
	{"prop_write", nil, 1000},
	{"bigint64_arith", []string{"bigint_arith"}, 3},
	{"typed_f64_read", []string{"extra_f64s"}, 1000},
	{"generator_busy", []string{"extra_busy_gen"}, 3},
	{"prop_in", nil, 1000},
	{"call_mutual", []string{"call_even", "call_odd"}, 65 * 20},
	{"method_call", []string{"ExtraPoint", "=ExtraPoint.prototype.sum", "=ExtraPoint.prototype.same"}, 1000},
	{"forward_call0", []string{"extra_fwd0", "extra_fwd_target0"}, 1000},
	{"forward_call2", []string{"extra_fwd2", "extra_fwd_target2"}, 1000},
	{"forward_call6", []string{"extra_fwd6", "extra_fwd_target6"}, 1000},
}

// nativeKernelSource is the source of the named functions of the
// micro-benchmarks; a name "=x" is the statement x = function ... };.
func nativeKernelSource(t testing.TB, names ...string) string {
	var all string
	for _, f := range []string{"../v8bench/microbench.js", "../v8bench/microbench_extra.js"} {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		all += strings.ReplaceAll(string(b), "\r\n", "\n")
	}
	var out strings.Builder
	out.WriteString("var global_res;\n")
	for _, name := range names {
		if strings.HasPrefix(name, "=") {
			i := strings.Index(all, name[1:]+" = function")
			if i < 0 {
				t.Fatalf("no %s", name)
			}
			j := strings.Index(all[i:], "\n};\n")
			out.WriteString(all[i : i+j+4])
			continue
		}
		i := strings.Index(all, "function "+name+"(")
		if i < 0 {
			i = strings.Index(all, "function* "+name+"(")
		}
		if i < 0 {
			t.Fatalf("no function %s", name)
		}
		j := strings.Index(all[i:], "\n}\n")
		out.WriteString(all[i : i+j+3])
	}
	return out.String()
}

// nativeCounting turns the native handlers' counters on for a test, and
// holds the collector off: a collection between two runs may empty a
// cache one run then misses, an exit the other has not. The flag the
// code reads for the collector's is what a test sets (nativeMarking).
func nativeCounting(t testing.TB) {
	nativeCount = true
	gc := debug.SetGCPercent(-1)
	t.Cleanup(func() { nativeCount = false; debug.SetGCPercent(gc) })
}

// nativeMarking has the contexts made after it store as while the
// collector marks, or not.
func nativeMarking(t testing.TB, on bool) {
	if on {
		m := new(uint8)
		*m = 1
		nativeMarkFlag.Store(m)
	} else {
		nativeMarkFlag.Store(nil)
	}
	t.Cleanup(func() { nativeMarkFlag.Store(nil) })
}

// nativeExitOps is the exits to Go so far, by operation.
func nativeExitOps() (n [256]int64) {
	for i := range n {
		n[i] = atomic.LoadInt64(&nativeExits[i])
	}
	return n
}

// nativeRunSrc compiles src in the register form and runs it on a new
// runtime, by the native handlers or by the Go table's loop, and returns
// its value as a string.
func nativeRunSrc(t testing.TB, src string, native bool) string {
	t.Helper()
	compiler.SetRegisterForm(true)
	fn := compileForTest(t, src)
	compiler.SetRegisterForm(bytecode.RegDefault())
	SetRegTier(true)
	was := regNativeOn
	regNativeOn = native
	defer func() { SetRegTier(bytecode.RegDefault()); regNativeOn = was }()
	r := New(Config{})
	defer r.Close()
	v, err := r.Run(fn)
	if err != nil {
		t.Fatalf("native %v: %v", native, err)
	}
	if r.frameDepth != 0 {
		t.Fatalf("native %v: frame depth %d left", native, r.frameDepth)
	}
	return v.String().Go()
}

// nativeRunKernel runs a kernel and returns its answer and how many
// operations it did.
func nativeRunKernel(t testing.TB, native bool, name string, deps []string, n int) (string, int64) {
	src := nativeKernelSource(t, append([]string{name}, deps...)...) +
		fmt.Sprintf("var ops = %s(%d); String(ops) + ' ' + String(global_res)", name, n)
	s := nativeRunSrc(t, src, native)
	var ops int64
	fmt.Sscan(s, &ops)
	return s, ops
}

// TestRegNativeKernels runs each kernel by the native handlers, with the
// collector idle and as while it marks, every pointer stored by the
// barrier's routine: the answers of the Go table's loop, and the same
// exits to Go marking or not.
func TestRegNativeKernels(t *testing.T) {
	nativeCounting(t)
	for _, k := range nativeKernels {
		want, _ := nativeRunKernel(t, false, k.name, k.deps, k.n)
		var exits [2]int64
		for i, on := range []bool{false, true} {
			nativeMarking(t, on)
			before := atomic.LoadInt64(&nativeEnters)
			if got, _ := nativeRunKernel(t, true, k.name, k.deps, k.n); got != want {
				t.Errorf("%s (marking %v): %s, want %s", k.name, on, got, want)
			}
			exits[i] = atomic.LoadInt64(&nativeEnters) - before
		}
		nativeMarking(t, false)
		if exits[0] != exits[1] {
			t.Errorf("%s: %d exits, %d while marking", k.name, exits[0], exits[1])
		}
	}
}

// TestRegNativeExits logs each kernel's entries of the native code an
// operation, and its exits to Go by the operation left: where the native
// handlers leave work to Go.
func TestRegNativeExits(t *testing.T) {
	nativeCounting(t)
	for _, k := range nativeKernels {
		before, exits := atomic.LoadInt64(&nativeEnters), nativeExitOps()
		_, ops := nativeRunKernel(t, true, k.name, k.deps, k.n*50)
		var top []string
		for op, c := range nativeExitOps() {
			if d := c - exits[op]; d*20 > ops {
				top = append(top, fmt.Sprintf("%s %.2f", bytecode.ROp(op), float64(d)/float64(ops)))
			}
		}
		t.Logf("%-16s %6.3f entries an operation; exits by op: %v", k.name,
			float64(atomic.LoadInt64(&nativeEnters)-before)/float64(ops), top)
	}
}

// nativeCallsSrc is call-heavy code for the native call handlers: a
// recursion to the stack's limit and its RangeError, a throw from a deep
// callee caught several frames out, by a frame the code entered itself,
// a generator called and run from native frames, methods, closures that
// capture a frame's locals, and arguments missing and extra.
const nativeCallsSrc = `
	function deep(n) { return n === 0 ? 0 : 1 + deep(n - 1) }
	function boom(n) { if (n === 0) throw new TypeError("boom at " + n); return boom(n - 1) + 1 }
	function catcher(n) { try { return boom(n) } catch (e) { return e.message + "|" + (e instanceof TypeError) } }
	function mid(n) { return catcher(n) + "!" }
	function* gen(n) { for (var i = 0; i < n; i++) yield i * 2 }
	function sumGen(n) { var s = 0; for (var v of gen(n)) s += v; return s }
	function callsGen(n) { return sumGen(n) + sumGen(n + 1) }
	function P(x) { this.x = x }
	P.prototype.get = function () { return this.x };
	P.prototype.add = function (y) { return this.get() + y };
	function methods(n) { var p = new P(3), s = 0; for (var i = 0; i < n; i++) s += p.add(i); return s }
	function counter() { var c = 0; return function () { c++; return c } }
	function closures(n) { var f = counter(), s = 0; for (var i = 0; i < n; i++) s += f(); return s }
	function args3(a, b, c) { return String(a) + String(b) + String(c) }
	function argsMissing() { return args3(1) + args3(1, 2, 3, 4) }
	function overflow() { try { return deep(1e6) } catch (e) { return e instanceof RangeError } }
	function stack() { try { boom(3) } catch (e) { return e.stack.split(String.fromCharCode(10)).filter(l => /boom/.test(l)).length } }
	var out = [deep(500), mid(50), callsGen(10), methods(100), closures(50), argsMissing(), overflow(), stack()];
	out.join(",")`

// nativeFastSrc is code for the native handlers that answer what used to
// exit: typeof of every kind of value, the length of arrays and strings
// and of what has another, and typed arrays' elements of every kind read
// and written, at the edges -- out of range, a NaN, a fraction, a number
// too large for an integer kind, a detached and a shrunk view -- and a
// dense array's elements of any value.
const nativeFastSrc = `
	var out = [];
	var vals = [1, -0, NaN, undefined, null, true, false, "s", "", Symbol("y"), 1n, 2n ** 70n, -(2n ** 64n),
		{}, [], function () {}, () => 1, class {}, Math.max.bind(null), new Proxy(function () {}, {}),
		new Proxy({}, {}), new Date(0), new Uint8Array(2), /x/];
	for (var round = 0; round < 3; round++) {
		var t = [];
		for (var i = 0; i < vals.length; i++) t.push(typeof vals[i]);
		out.push(t.join(" "));
	}
	var sparse = []; sparse[100000] = 1;
	var holey = [1, , 3];
	var lens = [[], [1, 2, 3], sparse, holey, "", "abc", String.fromCharCode(0xe9, 0xd83d, 0xde00), "ab" + "cd".repeat(3),
		{ length: 7 }, new Uint8Array(5), function (a, b) {}, new String("xy")];
	for (var round = 0; round < 3; round++) {
		var l = [];
		for (var i = 0; i < lens.length; i++) l.push(lens[i].length);
		out.push(l.join(" "));
	}
	var mixed = [1, "s", {}, undefined, null, 2.5, [7]];
	for (var round = 0; round < 3; round++) {
		var m = [];
		for (var i = 0; i < mixed.length + 2; i++) { var v = mixed[i]; m.push(typeof v === "object" && v ? "o" : String(v)); }
		m.push(String(mixed[0]), String(mixed[1]), String(mixed[3]));
		out.push(m.join(" "));
	}
	var kinds = [Int8Array, Uint8Array, Uint8ClampedArray, Int16Array, Uint16Array, Int32Array, Uint32Array,
		Float32Array, Float64Array, typeof Float16Array === "function" ? Float16Array : Float64Array];
	var ins = [0, 1, -1, 127, 128, 255, 256, -129, 65535, 65536, 2147483647, 2147483648, -2147483649, 4294967296,
		1.5, -1.5, 0.1, NaN, Infinity, -Infinity, -0, 2 ** 53, 2 ** 63, -(2 ** 63), 2 ** 64, 1e300, 3.4e38, 1e-45];
	for (var k = 0; k < kinds.length; k++) {
		var a = new kinds[k](ins.length + 1);
		for (var round = 0; round < 2; round++) {
			for (var i = 0; i < ins.length; i++) a[i] = ins[i];
			var r = [];
			for (var i = 0; i < ins.length + 2; i++) r.push(Object.is(a[i], -0) ? "-0" : String(a[i]));
			r.push(String(a[0]), String(a[1]), String(a[2]), String(a[3]));
			a[0] = 3; a[1] = 300; a[2] = -7.5; a[3] = NaN;
			r.push(String(a[0]), String(a[1]), String(a[2]), String(a[3]));
			a[1.5] = 9; a[-1] = 9; a[ins.length + 5] = 9;
			r.push(String(a[1.5]), String(a[-1]), String(a[ins.length + 5]));
			out.push(kinds[k].name + ": " + r.join(" "));
		}
	}
	var big = new BigInt64Array(2); big[0] = 5n; big[1] = -(2n ** 63n);
	out.push(String(big[0]) + " " + String(big[1]));
	var buf = new ArrayBuffer(16), view = new Float64Array(buf);
	view[0] = 1.25; view[1] = 2.5;
	var before = view[0] + view[1];
	buf.transfer();
	view[0] = 7;
	out.push(before + " " + String(view[0]) + " " + view.length);
	var rbuf = new ArrayBuffer(16, { maxByteLength: 32 }), rview = new Int32Array(rbuf);
	rview[3] = 9;
	var r3 = rview[3];
	rbuf.resize(8);
	rview[3] = 4;
	out.push(r3 + " " + String(rview[3]) + " " + rview.length);
	var proto = { inherited: 1 }, obj = Object.create(proto), sym = Symbol("k");
	obj.a = 1; obj[sym] = 2;
	var keys = ["a", "z", "inherited", "toString", 0, "0", sym, "b"];
	for (var round = 0; round < 6; round++) {
		var hs = [];
		for (var i = 0; i < keys.length; i++) hs.push(keys[i] in obj ? 1 : 0);
		hs.push("length" in [], 1 in [5, 6], "x" in new Proxy({ x: 1 }, {}));
		try { hs.push("a" in 5) } catch (e) { hs.push(e instanceof TypeError) }
		out.push(hs.join(""));
		if (round === 1) { delete obj.a; obj.b = 1 }
		if (round === 2) { proto.z = 1 }
		if (round === 3) { Object.setPrototypeOf(obj, null) }
		if (round === 4) { obj.a = 3; Object.setPrototypeOf(obj, proto) }
	}
	var bigs = [0n, 1n, -1n, 7n, -9n, 2n ** 31n, 2n ** 48n, -(2n ** 48n), 2n ** 48n - 1n, 2n ** 49n, 2n ** 62n,
		2n ** 63n - 1n, -(2n ** 63n), 2n ** 63n, 2n ** 64n - 1n, -(2n ** 64n) + 1n, 2n ** 64n, 3n ** 50n];
	for (var round = 0; round < 2; round++) {
		var bs = [];
		for (var i = 0; i < bigs.length; i++)
			for (var j = 0; j < bigs.length; j++) {
				var x = bigs[i], y = bigs[j];
				bs.push(String(x + y), String(x - y), String(x * y));
			}
		try { bs.push(String(1n + 1)) } catch (e) { bs.push(e instanceof TypeError) }
		var acc = 0n;
		for (var i = 0; i < 2000; i++) { acc += 2n ** 53n * 3n; acc -= 5n }
		bs.push(String(acc));
		out.push(bs.join(" "));
	}
	var o1 = {}, s1 = Symbol("q");
	var eqs = [0, -0, 1, NaN, Infinity, undefined, null, true, false, "ab", "a" + "b", "", o1, {}, s1, Symbol("q"),
		1n, 2n ** 70n, 2n ** 69n * 2n, "1", 1];
	for (var round = 0; round < 2; round++) {
		var es = [];
		for (var i = 0; i < eqs.length; i++)
			for (var j = 0; j < eqs.length; j++) {
				var x = eqs[i], y = eqs[j];
				if (x === y) es.push(1); else es.push(0);
				if (x !== y) es.push(1); else es.push(0);
			}
		es.push(o1 === o1, s1 === s1, undefined === null);
		out.push(es.join(""));
	}
	function self(f) { return this === undefined ? 0 : 1 }
	out.push(String(self()) + String(self.call(5)));
	var leafer = { arg: function (k) { return k }, two: function (a, b) { return b }, konst: function () { return "c" },
		none: function () {} };
	for (var round = 0; round < 3; round++) {
		var ls = [];
		for (var i = 0; i < 3; i++) ls.push(leafer.arg(i), leafer.arg(), leafer.two(i, { v: i }).v, leafer.two(i),
			leafer.konst(), leafer.none(i), leafer.arg("s" + i));
		out.push(ls.join(","));
	}
	// o[k] += v, o[3] += v, o[k] += 2 and o[k]++ over dense numbers and
	// what they are not: a hole, a string, an object, a sloppy function's
	// arguments, a frozen array, a typed array, a plain object with index
	// keys, a string, and keys past the end, negative, fractional, NaN,
	// -0 and a string.
	function updates(o, k, v) {
		o[k] += v; o[3] += v; o[k] += 2; o[k]++;
		return o;
	}
	function mapped(a, b, c, d) { updates(arguments, 1, 5); return [a, b, c, d].join(" ") }
	var keys = [0, 1, 3, 9, -1, 1.5, NaN, -0, "1", 2 ** 32];
	for (var round = 0; round < 3; round++) {
		var u = [];
		for (var i = 0; i < keys.length; i++) {
			var k = keys[i];
			u.push(updates([1, 2, 3, 4, 5], k, 10).join(" "), updates([1, , 3, 4], k, 0.5).join(" "),
				updates(["a", "b", 3, 4], k, 1).join(" "), updates([{}, [], 3, 4], k, 1).length,
				updates(Object.freeze([1, 2, 3, 4]), k, 1).join(" "), updates(new Int8Array([1, 2, 3, 120]), k, 7).join(" "),
				JSON.stringify(updates({ 0: 1, 1: 2, 3: 4 }, k, 3)), updates([1, 2, 3, 4], k, "x").join(" "),
				updates([1.5, 2.5, 3.5, 4.5], k, NaN).join(" "));
		}
		u.push(mapped(1, 2, 3, 4));
		out.push(u.join("/"));
	}
	out.join(String.fromCharCode(10))`

// nativeCallCases are calls each of which rhCallW answers one way: the
// frame-entry path the native call handler takes itself, or a built-in
// the site cached, which it calls in Go with no exit (nativegen's light)
// -- returning, throwing to a handler of the frame's, calling back
// (native) -- or a path of Go's -- a generator, a frame that reads
// new.target, a bound function -- which it must leave to Go. Each runs 64
// times in a loop; the first call of a site, which fills its cache, is
// Go's in any case.
var nativeCallCases = []struct {
	name, src string
	native    bool
}{
	{"plain", `function f(a, b) { var t = a; return t + b + 1 } function run(n) { var s = 0; for (var i = 0; i < n; i++) s += f(i, 2); return s }`, true},
	{"missing", `function f(a, b, c) { var t = a; return t + (c === undefined ? 1 : 2) + (b === undefined ? 4 : 8) } function run(n) { var s = 0; for (var i = 0; i < n; i++) s += f(i); return s }`, true},
	{"method", `var o = { k: 3, m(x) { var t = this.k; return t + x } }; function run(n) { var s = 0; for (var i = 0; i < n; i++) s += o.m(i); return s }`, true},
	{"leafArg", `var o = { id(x) { return x } }; function run(n) { var s = 0; for (var i = 0; i < n; i++) s += o.id(i); return s }`, true},
	{"leafConst", `function k() { return 7 } function run(n) { var s = 0; for (var i = 0; i < n; i++) s += k(); return s }`, true},
	{"builtin", `function run(n) { var s = 0; for (var i = 0; i < n; i++) s += Math.max(i, 3); return s }`, true},
	{"builtinThrows", `function run(n) { var s = 0; for (var i = 0; i < n; i++) { try { s += JSON.parse(i & 1 ? "1" : "x") } catch (e) { s += e.constructor.name.length } } return s }`, true},
	{"builtinCallback", `function inc(x) { return x + 1 } function run(n) { var s = 0, a = [0]; for (var i = 0; i < n; i++) { a[0] = i; s += a.map(inc)[0] } return s }`, true},
	{"builtinStack", `function depth(x) { return new Error("e").stack.split("\n").length + x } function run(n) { var s = 0, a = [0]; for (var i = 0; i < n; i++) s += a.map(depth)[0]; return s }`, true},
	{"generator", `function* g() { yield 1 } function run(n) { var s = 0; for (var i = 0; i < n; i++) s += g().next().value; return s }`, false},
	{"arguments", `function f() { return arguments.length } function run(n) { var s = 0; for (var i = 0; i < n; i++) s += f(i, i); return s }`, true},
	{"argumentsMethod", `var o = { k: 2, m() { var t = 0; for (var j = 0; j < arguments.length; j++) t += arguments[j]; return t + this.k } }; function run(n) { var s = 0; for (var i = 0; i < n; i++) s += o.m(i, 1, 2); return s }`, true},
	{"argumentsMapped", `function f(a, b) { a = 10; return arguments[0] + (b === undefined ? 0 : arguments[1]) + arguments.length } function run(n) { var s = 0; for (var i = 0; i < n; i++) s += f(i) + f(i, 1, 2); return s }`, true},
	{"argumentsStrict", `function f(a) { "use strict"; a = 3; return arguments[0] + a } function run(n) { var s = 0; for (var i = 0; i < n; i++) s += f(i, 5); return s }`, true},
	{"leafThis", `var o = { k: 2, get() { return this.k }, set(v) { this.k = v } }; function run(n) { var s = 0; for (var i = 0; i < n; i++) { o.set(i); s += o.get() } return s }`, true},
	{"leafPure", `function sq(x) { return x * x + 1 } function run(n) { var s = 0; for (var i = 0; i < n; i++) s += sq(i); return s }`, true},
	{"forwarder", `function t(a, b) { return (a | 0) + (b | 0) } function fw() { return t.apply(this, arguments) } function run(n) { var s = 0; for (var i = 0; i < n; i++) s += fw(i, 2) + fw(); return s }`, true},
	{"argumentsThrow", `function f() { if (arguments.length > 1) throw new Error("x" + arguments[1]); return arguments[0] } function run(n) { var s = 0; for (var i = 0; i < n; i++) { try { s += f(i, i & 1 ? i : undefined) } catch (e) { s += e.message.length } } return s }`, true},
	{"argumentsClosure", `function f(a) { var g = function () { return a + 1 }; return arguments.length + (typeof g).length + a } function run(n) { var s = 0; for (var i = 0; i < n; i++) s += f(i, 2, 3); return s }`, true},
	{"argumentsKept", `var keep = []; function f() { keep[keep.length] = arguments; return arguments.length } function run(n) { var s = 0; for (var i = 0; i < n; i++) s += f(i, [i]); var t = 0; for (var k of keep) t += k[0] + k[1][0]; return s + t }`, true},
	{"newTarget", `function f() { return new.target === undefined ? 1 : 2 } function run(n) { var s = 0; for (var i = 0; i < n; i++) s += f(); return s }`, false},
	{"bound", `var b = function (x) { return x + 1 }.bind(null); function run(n) { var s = 0; for (var i = 0; i < n; i++) s += b(i); return s }`, false},
}

// TestRegNativeCalls runs nativeCallsSrc by the Go table's loop and by
// the native handlers, with the collector idle and as while it marks: the
// same answers, and the code made calls of its own.
func TestRegNativeCalls(t *testing.T) {
	nativeCounting(t)
	want := nativeRunSrc(t, nativeCallsSrc, false)
	for _, marking := range []bool{false, true} {
		nativeMarking(t, marking)
		before := atomic.LoadInt64(&nativeCalls)
		if got := nativeRunSrc(t, nativeCallsSrc, true); got != want {
			t.Errorf("native (marking %v): %s, want %s", marking, got, want)
		}
		if atomic.LoadInt64(&nativeCalls) == before {
			t.Errorf("native (marking %v): no call made natively", marking)
		}
	}
}

// TestRegNativeFastPaths runs nativeFastSrc by the Go table's loop and by
// the native handlers, with the collector idle and as while it marks: the
// same answers, and the same exits to Go marking or not.
func TestRegNativeFastPaths(t *testing.T) {
	nativeCounting(t)
	want := nativeRunSrc(t, nativeFastSrc, false)
	var exits [2]int64
	for i, marking := range []bool{false, true} {
		nativeMarking(t, marking)
		before := atomic.LoadInt64(&nativeEnters)
		if got := nativeRunSrc(t, nativeFastSrc, true); got != want {
			t.Errorf("native (marking %v):\n%s\nwant\n%s", marking, got, want)
		}
		exits[i] = atomic.LoadInt64(&nativeEnters) - before
	}
	if exits[0] != exits[1] {
		t.Errorf("%d exits, %d while marking", exits[0], exits[1])
	}
}

// TestRegNativeCallExits checks the native call handler against rhCallW,
// the Go call path, case by case: the same answer, and the call made
// natively exactly where rhCallW enters a frame itself -- at most the
// first calls of the site left to Go -- and left to Go every time where
// rhCallW takes a path of its own. A condition rhCallW gains, or one the
// handler checks that rhCallW does not, shows here.
func TestRegNativeCallExits(t *testing.T) {
	nativeCounting(t)
	const n = 64
	for _, c := range nativeCallCases {
		src := c.src + fmt.Sprintf("; String(run(%d))", n)
		want := nativeRunSrc(t, src, false)
		before := nativeExitOps()
		got := nativeRunSrc(t, src, true)
		after := nativeExitOps()
		exits := after[bytecode.RCallW] - before[bytecode.RCallW] + after[bytecode.RCallMW] - before[bytecode.RCallMW]
		if got != want {
			t.Errorf("%s: %s, want %s", c.name, got, want)
		}
		// The script's call of run and each site's first two calls, which
		// fill its cache, are Go's: a case has no more than two sites.
		if c.native && exits > 5 {
			t.Errorf("%s: %d calls left to Go, want the call made natively", c.name, exits)
		} else if !c.native && exits < n {
			t.Errorf("%s: %d calls left to Go of %d, want every one: rhCallW answers it by a path of its own", c.name, exits, n)
		}
	}
}

// TestRegNativeDeep runs recursions past the native frame stack's room,
// by calls the code makes and by calls Go makes between them, to the
// stack's limit: the answers of the Go table's loop.
func TestRegNativeDeep(t *testing.T) {
	src := `
	function deep(n) { return n === 0 ? 0 : 1 + deep(n - 1) }
	function viaGo(n) { return n === 0 ? 0 : 1 + [n - 1].map(viaGo)[0] }
	function mixed(n) { return n === 0 ? 0 : n % 3 === 0 ? 1 + [n - 1].map(mixed)[0] : 1 + mixed(n - 1) }
	function T(n) { this.d = n === 0 ? 0 : 1 + new T(n - 1).d }
	function thrower(n) { if (n === 0) throw new Error("at the bottom"); return mixed(2) + thrower(n - 1) }
	function caught() { try { return thrower(700) } catch (e) { return e.message } }
	function over() { try { return mixed(1e6) } catch (e) { return e instanceof RangeError } }
	[deep(1000), viaGo(300), mixed(2000), new T(500).d, caught(), over()].join()`
	want := nativeRunSrc(t, src, false)
	for _, marking := range []bool{false, true} {
		nativeMarking(t, marking)
		if got := nativeRunSrc(t, src, true); got != want {
			t.Errorf("native (marking %v): %s, want %s", marking, got, want)
		}
	}
}

// TestRegNativeConcurrent runs nativeCallsSrc, the call kernels and
// nativeFastSrc by the native handlers on two runtimes at once, each on a
// goroutine of its own, the code shared: each gives the answers it gives
// alone.
func TestRegNativeConcurrent(t *testing.T) {
	compiler.SetRegisterForm(true)
	fn := compileForTest(t, nativeCallsSrc)
	kern := nativeKernelSource(t, "call_fib", "call_fib_rec", "call_mutual", "call_even", "call_odd") +
		"call_fib(177 * 20); var a = global_res; call_mutual(65 * 20); a + ' ' + global_res"
	kfn := compileForTest(t, kern)
	ffn := compileForTest(t, nativeFastSrc)
	compiler.SetRegisterForm(bytecode.RegDefault())
	SetRegTier(true)
	was := regNativeOn
	regNativeOn = true
	defer func() { SetRegTier(bytecode.RegDefault()); regNativeOn = was }()
	run := func() (string, error) {
		r := New(Config{})
		defer r.Close()
		var out []string
		for i := 0; i < 20; i++ {
			for _, f := range []*bytecode.Function{fn, kfn, ffn} {
				v, err := r.Run(f)
				if err != nil {
					return "", err
				}
				out = append(out, v.String().Go())
			}
		}
		if r.frameDepth != 0 {
			return "", fmt.Errorf("frame depth %d left", r.frameDepth)
		}
		return strings.Join(out, ";"), nil
	}
	want, err := run()
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	got := make([]string, 2)
	errs := make([]error, 2)
	for i := range got {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got[i], errs[i] = run()
		}()
	}
	wg.Wait()
	for i := range got {
		if errs[i] != nil {
			t.Errorf("goroutine %d: %v", i, errs[i])
		} else if got[i] != want {
			t.Errorf("goroutine %d: answers differ from one runtime's alone", i)
		}
	}
}

// TestRegNativeOpMix, with QJS_NATIVE_MIX naming micro-benchmarks
// (comma-separated, each name:n), logs each one's operation mix: the
// instructions dispatched an operation by their operation, run by the Go
// table, and how many of them the native handlers left to Go. It is how
// a test slower natively shows which native paths it lacks.
func TestRegNativeOpMix(t *testing.T) {
	only := os.Getenv("QJS_NATIVE_MIX")
	if only == "" {
		t.Skip("QJS_NATIVE_MIX names no test")
	}
	var defs string
	for _, f := range []string{"../v8bench/microbench.js", "../v8bench/microbench_extra.js"} {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		s := strings.ReplaceAll(string(b), "\r\n", "\n")
		if i := strings.LastIndex(s, "\nif (typeof scriptArgs === \"undefined\")"); i >= 0 {
			s = s[:i+1]
		}
		defs += s
	}
	compiler.SetRegisterForm(true)
	defFn := compileForTest(t, "if (typeof console === 'undefined') globalThis.console = { log: function () {} };\n"+defs)
	compiler.SetRegisterForm(bytecode.RegDefault())
	SetRegTier(true)
	was := regNativeOn
	defer func() { SetRegTier(bytecode.RegDefault()); regNativeOn = was }()
	nativeCounting(t)
	var counts [256]int64
	orig := regTable
	defer func() { regTable = orig }()
	for _, spec := range strings.Split(only, ",") {
		name, n, _ := strings.Cut(spec, ":")
		if n == "" {
			n = "1000"
		}
		compiler.SetRegisterForm(true)
		call := compileForTest(t, fmt.Sprintf("String(%s(%s))", name, n))
		compiler.SetRegisterForm(bytecode.RegDefault())
		var before, after [256]int64
		run := func(native bool) int64 {
			regNativeOn = native
			r := New(Config{})
			defer r.Close()
			if _, err := r.Run(defFn); err != nil {
				t.Fatal(err)
			}
			counts = [256]int64{}
			before = nativeExitOps()
			v, err := r.Run(call)
			if err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			after = nativeExitOps()
			var ops int64
			fmt.Sscan(v.String().Go(), &ops)
			return max(ops, 1)
		}
		for i := range regTable {
			h, op := orig[i], i
			regTable[i] = func(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
				counts[op]++
				return h(regs, w, pc, st)
			}
		}
		ops := run(false)
		regTable = orig
		mix := counts
		run(true)
		var total, exited int64
		type row struct {
			op       bytecode.ROp
			n, exits int64
		}
		var rows []row
		for op := range mix {
			total += mix[op]
			e := after[op] - before[op]
			exited += e
			if mix[op] > 0 || e > 0 {
				rows = append(rows, row{bytecode.ROp(op), mix[op], e})
			}
		}
		for i := range rows {
			for j := i + 1; j < len(rows); j++ {
				if rows[j].n > rows[i].n {
					rows[i], rows[j] = rows[j], rows[i]
				}
			}
		}
		var parts []string
		for _, r := range rows {
			if r.n*50 < total && r.exits*50 < total {
				continue
			}
			parts = append(parts, fmt.Sprintf("%s %.2f/%.2f", r.op, float64(r.n)/float64(ops), float64(r.exits)/float64(ops)))
		}
		t.Logf("%-24s %7.2f instructions an op, %6.2f exited (%4.1f%%): %s", name,
			float64(total)/float64(ops), float64(exited)/float64(ops), 100*float64(exited)/float64(max(total, 1)), strings.Join(parts, ", "))
	}
}

// TestRegNativeFileMix, with QJS_NATIVE_FILES naming JavaScript files
// (comma-separated) and QJS_NATIVE_EXPR what to run after them, logs the
// operation mix of the whole run as TestRegNativeOpMix does: each
// operation's dispatches by the Go table and its exits from the native
// handlers, the most frequent first.
func TestRegNativeFileMix(t *testing.T) {
	files := os.Getenv("QJS_NATIVE_FILES")
	if files == "" {
		t.Skip("QJS_NATIVE_FILES names no file")
	}
	var src strings.Builder
	for _, f := range strings.Split(files, ",") {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		src.Write(b)
		src.WriteString("\n")
	}
	src.WriteString(os.Getenv("QJS_NATIVE_EXPR"))
	compiler.SetRegisterForm(true)
	fn := compileForTest(t, src.String())
	compiler.SetRegisterForm(bytecode.RegDefault())
	SetRegTier(true)
	was := regNativeOn
	defer func() { SetRegTier(bytecode.RegDefault()); regNativeOn = was }()
	nativeCounting(t)
	var counts, slow [256]int64
	orig, origSlow := regTable, regSlowTable
	defer func() { regTable, regSlowTable = orig, origSlow }()
	run := func(native bool) {
		regNativeOn = native
		r := New(Config{})
		defer r.Close()
		if _, err := r.Run(fn); err != nil {
			t.Fatal(err)
		}
	}
	for i := range regTable {
		h, op := orig[i], i
		regTable[i] = func(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
			counts[op]++
			return h(regs, w, pc, st)
		}
		hs := origSlow[i]
		regSlowTable[i] = func(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
			slow[op]++
			return hs(regs, w, pc, st)
		}
	}
	run(false)
	regTable, regSlowTable = orig, origSlow
	before, enters := nativeExitOps(), atomic.LoadInt64(&nativeEnters)
	run(true)
	after := nativeExitOps()
	var total, exited int64
	type row struct {
		op             bytecode.ROp
		n, slow, exits int64
	}
	var rows []row
	for op := range counts {
		e := after[op] - before[op]
		total += counts[op]
		exited += e
		rows = append(rows, row{bytecode.ROp(op), counts[op], slow[op], e})
	}
	for i := range rows {
		for j := i + 1; j < len(rows); j++ {
			if rows[j].exits > rows[i].exits {
				rows[i], rows[j] = rows[j], rows[i]
			}
		}
	}
	t.Logf("%d instructions, %d exits (%.1f%%), %d entries", total, exited, 100*float64(exited)/float64(max(total, 1)), atomic.LoadInt64(&nativeEnters)-enters)
	for _, r := range rows[:min(len(rows), 25)] {
		if r.exits == 0 {
			break
		}
		t.Logf("  %-22s %10d dispatched %10d slow in Go %10d exits (%5.1f%% of all exits)", r.op, r.n, r.slow, r.exits, 100*float64(r.exits)/float64(max(exited, 1)))
	}
}

// nativeMoreSrc is code for the handlers made native after the kernels':
// == and != of every pair of kinds, constants loaded over registers that
// held objects, upvalues read, written and compared against, a key of
// each kind, x[++i], and the bitwise operators of fractions, negatives,
// NaNs, infinities and numbers past 2^32 and 2^63.
const nativeMoreSrc = `
	var vals = [0, -0, 1, 1.5, NaN, Infinity, "", "1", "a", true, false, null, undefined, {}, [], Symbol.iterator, 1n, function () {}]
	var out = []
	function eqs() {
		var s = ""
		for (var i = 0; i < vals.length; i++)
			for (var j = 0; j < vals.length; j++) {
				var a = vals[i], b = vals[j], r = 0
				try { if (a == b) r |= 1; if (a != b) r |= 2 } catch (e) { r = 9 }
				s += r
			}
		return s
	}
	out.push(eqs())
	function loads(n) {
		var t = 0, x
		for (var i = 0; i < n; i++) {
			x = {}; x = null; if (x === null) t++
			x = []; x = true; if (x) t++
			x = {}; x = false; if (!x) t++
			x = vals[i % vals.length]; x = ""; t += x.length
		}
		return t
	}
	out.push(loads(100))
	function counter() {
		var c = 0, lim = 50, f = 0.5
		function inc() { c++; return c }
		function under() { var k = 0; for (var i = 0; i < lim; i++) k += i; for (var j = 0; j <= lim; j += 7) k++; while (!(k < lim)) k -= 100; return k }
		function nan() { var k = 0; lim = NaN; for (var i = 0; i < 3; i++) { if (i < lim) k++; if (!(i <= lim)) k += 10 } lim = 50; return k }
		return [inc, under, nan, function (v) { c = v; f = {o: v} }, function () { return f }]
	}
	var cs = counter()
	for (var i = 0; i < 20; i++) cs[0]()
	cs[3]("str")
	out.push(cs[0](), cs[1](), cs[2](), JSON.stringify(cs[4]()))
	function keys() {
		var o = {a: 1, 1: 2}, sym = Symbol("s"), r = []
		o[sym] = 3
		var ks = ["a", 1, sym, 1.5, {toString() { return "a" }}]
		for (var k of ks) { o[k] = (o[k] | 0) + 1; r.push(String(o[k])) }
		return r.join()
	}
	out.push(keys())
	function pre(a) { var i = -1, s = 0, h = [1, , 3]; while (i < a.length - 1) s += a[++i]; try { var j = 0; s += h[++j] } catch (e) { s = "e" } return s }
	out.push(pre([1, 2, 3, 4.5]), pre(new Float64Array([1, 2])), pre("abc"))
	function bits() {
		var xs = [0, 1, -1, 1.5, -1.5, 2147483648, 4294967296.5, -4294967297, 1e20, 9.3e18, NaN, Infinity, -Infinity, 2 ** 63, -(2 ** 63)], r = []
		for (var x of xs) r.push(x | 0, x & 255, x ^ 3, x << 1, x >> 2, x >>> 3, x | x, x & 7.5, 1 << x, -1 >>> x)
		return r.join()
	}
	out.push(bits())
	out.join("\n")`

// TestRegNativeMore runs nativeMoreSrc by the Go table's loop and by the
// native handlers, with the collector idle and as while it marks: the
// same answers, and the same exits marking or not.
func TestRegNativeMore(t *testing.T) {
	nativeCounting(t)
	want := nativeRunSrc(t, nativeMoreSrc, false)
	var exits [2]int64
	for i, marking := range []bool{false, true} {
		nativeMarking(t, marking)
		before := atomic.LoadInt64(&nativeEnters)
		if got := nativeRunSrc(t, nativeMoreSrc, true); got != want {
			t.Errorf("native (marking %v):\n%s\nwant\n%s", marking, got, want)
		}
		exits[i] = atomic.LoadInt64(&nativeEnters) - before
	}
	if exits[0] != exits[1] {
		t.Errorf("%d exits, %d while marking", exits[0], exits[1])
	}
}

// nativeMore2Src is code for global writes and property additions:
// globals written of every kind, a global made read-only and deleted
// between writes, and o.n += of numbers, NaNs and what is no number.
const nativeMore2Src = `
	var g1 = 0, g2 = null, out = []
	function globals(n) { for (var i = 0; i < n; i++) { g1 = i; g2 = {i: i}; g1 += 0.5 } return g1 + g2.i }
	out.push(globals(50))
	Object.defineProperty(globalThis, "g1", {writable: false})
	out.push(globals(5), g1)
	function adds(o, n) { for (var i = 0; i < n; i++) { o.n += 2; o.n++; o.m += o.n; o.s += 1 } return [o.n, o.m, o.s].join() }
	out.push(adds({n: 0, m: 0.5, s: "x"}, 20), adds({n: NaN, m: Infinity, s: 1}, 3), adds({n: 1e308, m: -0, s: null}, 3))
	out.join("\n")`

// TestRegNativeMore2 runs nativeMore2Src as TestRegNativeMore runs its.
func TestRegNativeMore2(t *testing.T) {
	want := nativeRunSrc(t, nativeMore2Src, false)
	for _, marking := range []bool{false, true} {
		nativeMarking(t, marking)
		if got := nativeRunSrc(t, nativeMore2Src, true); got != want {
			t.Errorf("native (marking %v):\n%s\nwant\n%s", marking, got, want)
		}
	}
}

// nativeMore3Src is code for truthiness, == of an immediate, the unary
// operators and element writes of any value.
const nativeMore3Src = `
	var vals = [0, -0, 1, 1.5, NaN, Infinity, "", "1", "a", true, false, null, undefined, {}, [], Symbol.iterator, 0n, 1n, function () {}], out = []
	function truth() { var s = ""; for (var v of vals) { s += v ? 1 : 0; if (!v) s += "n"; while (v) { s += "w"; v = 0 } } return s }
	function eqi() { var s = ""; for (var v of vals) { try { s += (v == 1 ? 1 : 0) + "" + (v != 0 ? 1 : 0) } catch (e) { s += "e" } } return s }
	function un() { var r = []; for (var v of vals) { try { r.push(String(-v), String(+v), String(~v), String(!v)) } catch (e) { r.push("e") } } return r.join() }
	function un2() { var r = [], xs = [0, -0, 1.5, NaN, -Infinity, 2 ** 31, 2 ** 32 + 0.5, -(2 ** 53), 1e300]; for (var x of xs) r.push(1 / -x, +x, ~x, -(-x)); return r.join() }
	function elems() { var a = [1, 2, 3, 4], o = {}, r = []; for (var i = 0; i < 40; i++) { a[i & 3] = i & 1 ? o : "s" + i; a[0] = null; a[1] = a } r.push(a.length, typeof a[2], a[1] === a, a[0]); var f = Object.freeze([1, 2]); try { f[0] = 5 } catch (e) {} r.push(f[0]); return r.join() }
	out.push(truth(), eqi(), un(), un2(), elems())
	out.join("\n")`

// TestRegNativeMore3 runs nativeMore3Src as TestRegNativeMore runs its.
func TestRegNativeMore3(t *testing.T) {
	want := nativeRunSrc(t, nativeMore3Src, false)
	for _, marking := range []bool{false, true} {
		nativeMarking(t, marking)
		if got := nativeRunSrc(t, nativeMore3Src, true); got != want {
			t.Errorf("native (marking %v):\n%s\nwant\n%s", marking, got, want)
		}
	}
}

// nativeMore4Src is code for properties added by a site's transition:
// constructors' objects past the room their allocation carries and past
// linearScanLimit, a frozen and a sealed object, a setter up the
// prototype chain added after the site has cached the transition, and a
// prototype changed under it.
const nativeMore4Src = `
	function P(n) { this.a = n; this.b = {n: n}; this.c = "s"; this.d = n + 1; this.e = n * 2; this.f = 6; this.g = 7; this.h = 8; this.i = 9; this.j = 10 }
	var out = [], ps = []
	for (var i = 0; i < 30; i++) ps.push(new P(i))
	out.push(ps.map(function (p) { return Object.keys(p).length + p.b.n + p.j }).join())
	function add(o, v) { o.x = v; return o }
	var objs = []
	for (var i = 0; i < 20; i++) objs.push(add({}, i))
	var fr = Object.freeze({}), se = Object.seal({}), ne = Object.preventExtensions({})
	for (var o of [fr, se, ne]) { try { add(o, 1) } catch (e) { out.push(e.constructor.name) } }
	out.push(objs.map(function (o) { return o.x }).join(), "x" in fr, "x" in se)
	var proto = {}, seen = []
	function Q() { this.y = 1 }
	Q.prototype = proto
	for (var i = 0; i < 5; i++) new Q()
	Object.defineProperty(proto, "y", { set: function (v) { seen.push(v) } })
	var q = new Q()
	out.push(seen.join(), Object.keys(q).join(), q.y)
	out.join("\n")`

// TestRegNativeMore4 runs nativeMore4Src as TestRegNativeMore runs its.
func TestRegNativeMore4(t *testing.T) {
	want := nativeRunSrc(t, nativeMore4Src, false)
	for _, marking := range []bool{false, true} {
		nativeMarking(t, marking)
		if got := nativeRunSrc(t, nativeMore4Src, true); got != want {
			t.Errorf("native (marking %v):\n%s\nwant\n%s", marking, got, want)
		}
	}
}

// nativeMore5Src is code for reads at sites of several shapes: up to the
// entries a site keeps and past them, a property deleted and redefined
// as an accessor under the site, methods of several classes.
const nativeMore5Src = `
	var shapes = [{x: 1}, {y: 0, x: 2}, {z: 0, y: 0, x: 3}, {w: 0, z: 0, y: 0, x: 4}, {v: 0, w: 0, z: 0, y: 0, x: 5}], out = []
	function rd(o) { return o.x }
	function sum(n, k) { var s = 0; for (var i = 0; i < n; i++) s += rd(shapes[i % k]); return s }
	out.push(sum(100, 2), sum(100, 3), sum(100, 5))
	delete shapes[1].x
	Object.defineProperty(shapes[2], "x", { get: function () { return 30 } })
	out.push(sum(100, 3), String(rd(shapes[1])))
	class A { m() { return 1 } } class B { m() { return 2 } } class C extends A { m() { return 3 } }
	var cs = [new A(), new B(), new C(), new A()], t = 0
	for (var i = 0; i < 60; i++) t += cs[i & 3].m()
	out.push(t)
	out.join("\n")`

// TestRegNativeMore5 runs nativeMore5Src as TestRegNativeMore runs its.
func TestRegNativeMore5(t *testing.T) {
	want := nativeRunSrc(t, nativeMore5Src, false)
	for _, marking := range []bool{false, true} {
		nativeMarking(t, marking)
		if got := nativeRunSrc(t, nativeMore5Src, true); got != want {
			t.Errorf("native (marking %v):\n%s\nwant\n%s", marking, got, want)
		}
	}
}

// A sloppy callee that keeps its arguments maps its arguments object onto
// its parameters' registers, which its return closes, as the miss's
// regEnterW closes them: the native code's return of a frame it entered
// so left them open on registers the next frames overwrote, and an
// arguments object kept past the call read what they held -- the
// arguments object itself, Array -- by a call or a call in tail position
// the site's cache answered.
func TestRegNativeKeptArgumentsClosed(t *testing.T) {
	src := `
	var kept = [];
	var s1 = function s(n, a) { kept.push(arguments); return arguments.length + ":" + Array.prototype.join.call(arguments, ","); };
	function viaTail(n, a) { return n === 0 ? s1(n, a, "x") : viaTail(n - 1, a + 1); }
	function viaCall(n, a) { var v = s1(n, a, "x"); return v; }
	var out = [];
	for (var i = 0; i < 3; i++) out.push(viaTail(3, 0));
	for (var i = 0; i < 3; i++) out.push(viaCall(0, 3));
	out.push(kept.map(function (a) { return Array.prototype.join.call(a, ","); }).join(" "));
	out.join(" | ")`
	want := "3:0,3,x | 3:0,3,x | 3:0,3,x | 3:0,3,x | 3:0,3,x | 3:0,3,x | 0,3,x 0,3,x 0,3,x 0,3,x 0,3,x 0,3,x"
	for _, native := range []bool{false, true} {
		for _, marking := range []bool{false, true} {
			nativeMarking(t, marking)
			if got := nativeRunSrc(t, src, native); got != want {
				t.Errorf("native %v (marking %v): %s, want %s", native, marking, got, want)
			}
		}
	}
}

// nativeTemplateSrc calls, at each of its sites, a closure made anew each
// time its maker runs: plain functions sloppy and strict, a method, and
// arrows each with a this of its own, and a bound function and a function
// with a property among them, which the site's cache answers as the
// closure of its template it met before or leaves to the miss.
const nativeTemplateSrc = `
	function plain(k) { var f = function () { return this === globalThis ? k : -1 }; return f() }
	function strict(k) { "use strict"; var f = function () { return this === undefined ? k : -1 }; return f() }
	function method(k) { var o = { k: k }; o.m = function () { return this.k }; return o.m() }
	function arrow(k) { var o = { k: k, a: function () { var f = () => this.k; return f() } }; return o.a() }
	function mixed(k) {
		var f = function (x) { return x + (this && this.b || 0) };
		if (k % 3 === 1) f = f.bind({ b: 100 });
		if (k % 3 === 2) f.p = 1;
		return f(k);
	}
	var s = [];
	for (var k = 0; k < 6; k++) s.push(plain(k), strict(k), method(k), arrow(k), mixed(k));
	s.join()`

// A site whose callee is a closure made anew each time its maker runs
// answers it as the closure of its template it met before: the cache is
// made the new closure's and the call entered as a hit (regSameTemplate),
// each closure's own this kept.
func TestRegCallSameTemplate(t *testing.T) {
	want := "0,0,0,0,0,1,1,1,1,101,2,2,2,2,2,3,3,3,3,3,4,4,4,4,104,5,5,5,5,5"
	for _, native := range []bool{false, true} {
		if got := nativeRunSrc(t, nativeTemplateSrc, native); got != want {
			t.Errorf("native %v: %s, want %s", native, got, want)
		}
	}
	// The closure is entered by the native code, once the exit that met
	// it has made the cache its own, not by Go from the miss, which runs
	// it in a code entry of its own: an entry for each call of outer, not
	// two.
	nativeCounting(t)
	enters, calls := atomic.LoadInt64(&nativeEnters), atomic.LoadInt64(&nativeCalls)
	nativeRunSrc(t, `
		function outer(k) { var f = function () { var a = [k]; return a[0] }; return f() }
		var s = 0;
		for (var k = 0; k < 200; k++) s += outer(k);
		String(s)`, true)
	enters, calls = atomic.LoadInt64(&nativeEnters)-enters, atomic.LoadInt64(&nativeCalls)-calls
	if enters > 210 || calls < 199 {
		t.Errorf("%d entries of the code and %d calls it made itself, want 210 entries at most and 199 calls or more", enters, calls)
	}
	// The native call answers the new closure itself, the cache made
	// its own, with no exit to Go (call's template check).
	before := nativeExitOps()[bytecode.RCallW]
	nativeRunSrc(t, `
		function maker(k) { var j = k, f = function () { var a = [j]; return a[0] }; return f() }
		function outer() { var s = 0; for (var k = 0; k < 200; k++) s += maker(k); return s }
		String(outer())`, true)
	if n := nativeExitOps()[bytecode.RCallW] - before; n > 10 {
		t.Errorf("calls of a closure made anew exited to Go %d times in 200, want 10 at most", n)
	}
}

// An element's update of a dense array's number, o[k] += v in each of its
// forms, is the native code's own: none exits to Go but the first store
// to the literal's shared elements.
func TestRegNativeAddIndex(t *testing.T) {
	nativeCounting(t)
	before := nativeExitOps()
	got := nativeRunSrc(t, `
		function run() {
			var tab = [1, 2, 3, 4];
			for (var j = 0; j < 1000; j++) {
				tab[0] += j; tab[1] += 0.5;
				var k = j & 3;
				tab[k] += j; tab[k] += 3; tab[k]++; tab[k]--;
			}
			return tab.join(" ");
		}
		run()`, true)
	if want := nativeRunSrc(t, `
		function run() {
			var tab = [1, 2, 3, 4];
			for (var j = 0; j < 1000; j++) {
				tab[0] += j; tab[1] += 0.5;
				var k = j & 3;
				tab[k] += j; tab[k] += 3; tab[k]++; tab[k]--;
			}
			return tab.join(" ");
		}
		run()`, false); got != want {
		t.Fatalf("native %s, want %s", got, want)
	}
	after := nativeExitOps()
	for _, op := range []bytecode.ROp{bytecode.RAddIndex, bytecode.RAddIndexK, bytecode.RAddIndexI} {
		// The literal's elements are shared until the first store copies
		// them, which Go makes.
		if n := after[op] - before[op]; n > 1 {
			t.Errorf("%v exited to Go %d times, want 1 at most", op, n)
		}
	}
}

// nativePrivateSrc reads and writes private members every way their
// native handlers meet them: fields of one class read and written in a
// loop, of instances of two classes with members of one name, of
// subclasses, static members, private methods and accessors, which no
// site caches, a read of a member the object does not have and of a
// non-object, a write of a method, and a field read before it is
// defined.
const nativePrivateSrc = `
	var out = [];
	class A { #n = 0; #m = "a"; add(k) { for (var i = 0; i < k; i++) this.#n = (this.#n + i) | 0; return this.#n + this.#m }
		static read(o) { return o.#n } }
	class B { #n = 5; add(k) { for (var i = 0; i < k; i++) this.#n += 2; return this.#n } }
	class C extends A { #c = 1; get() { return this.#c + A.read(this) } }
	class S { static #s = 3; static bump() { S.#s++; return S.#s } }
	class M { #m() { return 7 } get #g() { return this.#x * 2 } set #g(v) { this.#x = v } #x = 1;
		run() { this.#g = this.#m(); return this.#g } }
	class E { #e = 1; static read(o) { return o.#e } static write(o) { o.#e = 2 } static meth(o) { o.#k = 1 } #k() {} }
	class T { #a = this.#b; #b = 1; }
	for (var round = 0; round < 3; round++) {
		var s = [];
		var a = new A(), b = new B(), c = new C();
		s.push(a.add(50), b.add(30), c.add(10), c.get(), S.bump(), new M().run());
		for (var i = 0; i < 4; i++) s.push(A.read(i & 1 ? a : c));
		try { E.read({}) } catch (e) { s.push(e.constructor.name) }
		try { E.read(1) } catch (e) { s.push(e.constructor.name) }
		try { E.write(Object.freeze(new E())); s.push("frozen ok") } catch (e) { s.push(e.constructor.name) }
		try { E.meth(new E()) } catch (e) { s.push(e.constructor.name) }
		try { new T() } catch (e) { s.push(e.constructor.name) }
		out.push(s.join(","));
	}
	out.join("|")`

// Private members are the native code's own where a site's cache answers
// them: the table's answers, marking and not, and a loop of a field's
// reads and writes that exits to Go only to fill the caches.
func TestRegNativePrivate(t *testing.T) {
	want := nativeRunSrc(t, nativePrivateSrc, false)
	for _, marking := range []bool{false, true} {
		nativeMarking(t, marking)
		if got := nativeRunSrc(t, nativePrivateSrc, true); got != want {
			t.Errorf("native (marking %v):\n%s\nwant\n%s", marking, got, want)
		}
	}
	nativeCounting(t)
	before := nativeExitOps()
	if got := nativeRunSrc(t, `
		class P { #n = 0; add(k) { for (var i = 0; i < k; i++) this.#n = (this.#n + i) | 0; return this.#n } }
		String(new P().add(1000))`, true); got != "499500" {
		t.Fatalf("got %s, want 499500", got)
	}
	after := nativeExitOps()
	for _, op := range []bytecode.ROp{bytecode.RGetPrivate, bytecode.RSetPrivate} {
		if n := after[op] - before[op]; n > 2 {
			t.Errorf("%v exited to Go %d times in 1,000 turns, want 2 at most", op, n)
		}
	}
}
