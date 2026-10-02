package quickjs_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	quickjs "github.com/go-quickjs/go-quickjs"
	"github.com/go-quickjs/go-quickjs/internal/vm"
)

// treeScripts are run with the tree tier and without it, and must give the
// same answers: each value, each error and each stack trace. Between them
// they run every instruction the tier builds, and the slow path behind each
// fast one -- a valueOf, a getter, a proxy, a hole, a BigInt, a string, an
// exception from inside an expression -- and values left on the stack across
// blocks, by ?:, && and ||.
var treeScripts = []string{
	// Arithmetic, bitwise and comparison operators over numbers.
	`function f(n) { var r = []; for (var i = -3; i < n; i++) { var x = i * 1.5, y = i - 2;
	   r.push(x + y, x - y, x * y, x / y, x % y, -x, i & 5, i | 9, i ^ 3, i << 3, i >> 1, i >>> 1,
	     i < y, i <= y, i > y, i >= y, i == y, i != y, i === y, i !== y, 1 / -0 * i, i % 0) } return r.join() } f(5)`,
	`function f() { var r = []; for (var i = 0; i < 4; i++) { var x = 2147483647 + i, y = -2147483648 - i;
	   r.push(x | 0, y | 0, x >>> 0, y >>> 0, x << 1, y >> 33, x & y, ~~x, x ^ -1, 1e21 | 0, NaN | 0) } return r.join() } f()`,
	// Strings, objects with valueOf, and the order they are converted in.
	`function f() { var log = [], s = ""; var o = { valueOf() { log.push("v" + s.length); return 2 } };
	   for (var i = 0; i < 3; i++) { s = s + i + "-"; var t = o * i + o - i / o; log.push(t, "a" < "b" + i, "10" < 9 + i, o > i) }
	   return s + " " + log.join() } f()`,
	`function f() { var r = []; var a = { valueOf() { r.push("a"); return 1 } }, b = { valueOf() { r.push("b"); return 2 } };
	   for (var i = 0; i < 2; i++) { r.push(a + b, a - b, a < b, a == 1, b != "2", a | b, a << b) } return r.join() } f()`,
	// BigInt, ++ and -- on what is not a number.
	`function f() { var r = [], b = 10n, s = "5", u, o = { valueOf() { return 7 } };
	   for (var i = 0; i < 3; i++) { b++; r.push(b, b * 3n, -b, b % 4n); s++; u--; var p = o++; r.push(s, u, p, o, typeof o) }
	   return r.join() } function g(b) { for (var i = 0; i < 1; i++) b = b + 1; return b }
	 f() + " " + (function () { try { return g(1n) } catch (e) { return e.constructor.name } })()`,
	`function f() { var r = []; var a = [1, 2, 3], i = 0, s = "1"; for (var k = 0; k < 3; k++) { r.push(a[i++], a[++i - 1], a[s++]) } return r.join() } f()`,
	// Array reads and writes: holes, a getter and a setter up the chain,
	// bounds, odd keys, typed arrays, frozen arrays.
	`function f() { Object.defineProperty(Array.prototype, 3, { get() { return "proto3" }, set(v) { log.push("set" + v) }, configurable: true });
	   var log = [], a = [0, , 2], r = []; for (var i = -1; i < 6; i++) { r.push(a[i], a[i + 0.5], a["" + i]); a[i] = i * 2 }
	   delete Array.prototype[3]; return r.join() + " " + a.join() + " " + log.join() } f()`,
	`function f() { var t = new Float64Array(4), u = new Uint8Array(3), r = []; for (var i = 0; i < 5; i++) { t[i] = i / 3; u[i] = i * 100; r.push(t[i], u[i]) } return r.join() } f()`,
	`"use strict"; function f(a, i) { for (var k = 0; k < 1; k++) a[i] = 9 } var a = Object.freeze([1, 2]), r = [];
	 for (var i = 0; i < 2; i++) { try { f(a, i) } catch (e) { r.push(e.constructor.name + ": " + e.message) } } r.join() + a`,
	`function f() { var a = Object.freeze([1, 2]); for (var i = 0; i < 2; i++) a[i] = 9; return a.join() } f()`,
	// Properties: getters, setters, proxies, the chain, methods and new.
	`function f() { var log = [], p = { get x() { log.push("get"); return 1 }, set x(v) { log.push("set" + v) } };
	   var q = new Proxy({}, { get(t, k) { log.push("pget " + String(k)); return 5 }, set(t, k, v) { log.push("pset " + String(k) + v); return true } });
	   var s = 0; for (var i = 0; i < 2; i++) { s += p.x + q.y; p.x = i; q.z = i; s += Object.create(p).x } return s + " " + log.join() } f()`,
	`function P(v) { this.v = v } P.prototype.get = function () { return this.v };
	 function f() { var s = 0, o; for (var i = 0; i < 4; i++) { o = new P(i); s += o.get() * 10 + o.v } return s } f()`,
	`function f() { var o = { n: 0, inc() { return ++this.n } }, s = 0; for (var i = 0; i < 5; i++) { s += o.inc(); o.m = o.n } return s + "," + o.m } f()`,
	// Globals: declared, lexical, in their dead zone, undeclared, and
	// assignment to them, sloppy and strict.
	`var gv = 1; let gl = 2; const gc = 3; function f() { var s = 0; for (var i = 0; i < 3; i++) { s += gv + gl + gc; gv++; gl += 2 } return s + " " + gv + " " + gl }
	 function g() { for (var i = 0; i < 1; i++) gc = 4 } f() + " " + (function () { try { g() } catch (e) { return e.constructor.name + ": " + e.message } })()`,
	`function f() { var r = []; for (var i = 0; i < 2; i++) { newGlobal = i; r.push(newGlobal) } return r.join() }
	 function g() { for (var i = 0; i < 1; i++) return undeclared + 1 }
	 f() + " " + newGlobal + " " + (function () { try { g() } catch (e) { return e.constructor.name + ": " + e.message } })()`,
	`"use strict"; function f() { for (var i = 0; i < 2; i++) strictUndeclared = i } try { f() } catch (e) { e.constructor.name + ": " + e.message }`,
	`function f() { var r = []; for (var i = 0; i < 2; i++) r.push(tdz); return r } var res; try { f() } catch (e) { res = e.constructor.name + ": " + e.message } let tdz = 1; res + " " + f()`,
	// Upvalues, closures made in a loop, recursion.
	`function outer() { var a = 1, fs = []; function f() { for (var i = 0; i < 4; i++) { a = a * 2 + i; fs.push(function () { return a + i }) } return a }
	   var r = f(); return r + " " + fs.map(g => g()).join() } outer()`,
	`function fib(n) { var a = 0, b = 1; for (var i = 0; i < n; i++) { var t = a + b; a = b; b = t } return n < 2 ? n : fib(n - 1) + a - fib(n - 1) + b - b } fib(12)`,
	// Values carried from block to block: ?:, &&, ||, nested assignments.
	`function f() { var s = 0, a = [0, 0, 0], b = [0, 0, 0], o = {}, q = {}; for (var i = 0; i < 6; i++) {
	   s += (i % 2 ? i : -i) + (i && 3 || 4) + (i > 2 ? (i > 4 ? 100 : 10) : 1);
	   var v = a[i % 3] = b[(i + 1) % 3] = i * 2; o.p = q.r = v; s += o.p + q.r + v } return s + " " + a + " " + b } f()`,
	`function f() { var r = []; for (var i = 0; i < 4; i++) { var x = i === 0 ? null : i === 1 ? undefined : i; r.push(x || "dflt", x && x * 2, !x, !!x) } return r.join() } f()`,
	// Calls in the middle of an expression that change what the rest reads.
	`function f() { var h = [1, 2, 3], s = 0; function g(i) { h[i] = h[i] * 10; return i } for (var i = 0; i < 3; i++) { s += h[i] + g(i) * h[i] + h[i] } return s + " " + h } f()`,
	`function f() { var x = 1, r = []; for (var i = 0; i < 3; i++) { r.push(x + (x = x + 1) + x, x++ + x, x + x++) } return r.join() } f()`,
	// Operands read in place, a local's or an upvalue's: each is read where
	// the operator's own operand would have been, before what follows it
	// can change it.
	`function f() { var x = 1, y = 2.5, a = [5, 6, 7, 8], r = []; for (var i = 0; i < 3; i++) {
	   r.push(x * x++, x - (x = 10), y / (y = 4), (x = 3) - x, x + 0.5, 0.5 - x, 2 * y, x * y, a[i] + a[x - i], a[i++] + a[i]) } return r.join() } f()`,
	`function f() { var a = [0, 0, 0], i = 0, r = []; for (var n = 0; n < 2; n++) { a[i] = (i = 2); r.push(a.join(), i); a[i - 1] = i++ } return r.join("|") + " " + a } f()`,
	`function outer() { var lim = 3, u = 1.5; function f() { var r = []; for (var i = 0; i < lim; i++) { if (u < i) r.push("u" + i); if (i >= u) r.push("ge"); r.push(i) } lim = 1; return r.join() }
	   return f() + " " + f() } outer()`,
	`function f() { var n = 0; for (var i = 0; i < i++ + 1 && n < 5;) n++; var r = [n, i];
	   for (var j = 0; j < NaN; j++) r.push("nan"); for (var s = "a"; s < "aaa"; s += "a") r.push(s);
	   for (var k = 0; k <= "2"; k++) r.push(k); var o = { valueOf() { r.push("v"); return 1 } };
	   for (var m = 0; m < o; m++) r.push("m"); for (var p = 3; p > 1n; p--) r.push(p); return r.join() } f()`,
	`function f(a) { var s = "abc", r = ""; for (var i = -1; i < 4; i++) { r += a[i] + "/" + s[i] + "/" + a[i + 1] + ";"; a[i] = i } return r + " " + a } f([1, , 3])`,
	// Labelled loops, break, continue, switch, returns from inside a loop.
	`function f(n) { var r = []; outer: for (var i = 0; i < n; i++) { for (var j = 0; j < n; j++) { if (j > i) continue outer; if (i + j > 5) break outer;
	   switch (j % 3) { case 0: r.push("z"); break; case 1: r.push(i); default: r.push(j) } } } return r.join() } f(5)`,
	`function f(a) { for (var i = 0; i < a.length; i++) { if (a[i] < 0) return i; while (a[i] > 10) a[i] -= 7 } return -1 } f([3, 20, 15, -1, 2]) + "," + f([1])`,
	// this, in a method and in a sloppy function.
	`function f() { var s = 0; for (var i = 0; i < 3; i++) s += this.k * i; return s } f.call({ k: 7 }) + "," + (function () { var c = 0; for (var i = 0; i < 2; i++) c += this === globalThis; return c })()`,
	// A read the site's cache answers for an own property: the property
	// deleted and added back, made a getter, inherited at the same site,
	// frozen, and the site meeting objects of other shapes.
	`function rd(o) { var s = ""; for (var i = 0; i < 3; i++) s += o.x + ","; return s }
	 function P() { this.x = 1; this.y = 2 } P.prototype.z = "pz";
	 var a = new P(), b = new P(), r = [rd(a), rd(b)];
	 delete b.x; r.push(rd(b)); b.x = 5; r.push(rd(b));
	 Object.defineProperty(a, "x", { get() { return "g" }, configurable: true }); r.push(rd(a));
	 var c = Object.create({ x: "inh" }), d = { x: "own" }; for (var k = 0; k < 4; k++) r.push(rd(k % 2 ? c : d));
	 var f = Object.freeze(new P()); r.push(rd(f)); var q = new P(); q.x = "changed"; r.push(rd(q));
	 function m(o) { var s = 0; for (var i = 0; i < 3; i++) s += o.y; return s } r.push(m(new P()), m({ y: 10 }), m({ a: 1, y: 20 }));
	 r.join("|")`,
	// Equalities ending a block: against undefined, null and the booleans,
	// which are tested for without a call, and between two values, strict
	// and loose -- NaN, -0, a string and a number, a valueOf, and one that
	// throws.
	`function f(vals) { var r = ""; for (var i = 0; i < vals.length; i++) { var v = vals[i];
	   if (v === null) r += "N"; if (v !== undefined) r += "d"; if (v == null) r += "n"; if (null != v) r += "m"; if (v === true) r += "T"; if (v !== false) r += "f";
	   if (v == true) r += "t"; if (v === v) r += "s"; if (v == 0) r += "z"; if (v === "1") r += "q"; r += ";" } return r }
	 var log = [], o = { valueOf() { log.push("v"); return 1 } }, bad = { valueOf() { throw new RangeError("no") } };
	 function g(a, b) { var r = ""; for (var i = 0; i < 2; i++) { if (a == b) r += "e"; if (a != b) r += "x"; if (a === b) r += "S" } return r }
	 var out = f([null, undefined, 0, -0, "", "1", 1, true, false, NaN, {}, [], o]) + " " + log.join() + " " + g(0, -0) + g(NaN, NaN) + g("1", 1) + g(o, 1) + g(null, undefined) + " " + log.join();
	 try { g(bad, 1) } catch (e) { out += " " + e.constructor.name + "|" + e.stack.split("\n")[1].trim().split(" (")[0] } out`,
	// instanceof: the prototype chain, Symbol.hasInstance, a bound function,
	// a primitive, and a right operand that is not an object.
	`function P() {} var Q = { [Symbol.hasInstance](v) { log.push("has"); return v === 1 } }, log = [];
	 function f(vals) { var r = ""; for (var i = 0; i < vals.length; i++) { var v = vals[i];
	   r += (v instanceof P) + "/" + (v instanceof Q) + "/" + (v instanceof P.bind(null)) + "/" + (v instanceof Object) + ";" } return r }
	 function g(v) { for (var i = 0; i < 1; i++) return v instanceof 3 }
	 f([new P(), 1, "s", null, Object.create(P.prototype)]) + " " + log.join() + " " +
	   (function () { try { g({}) } catch (e) { return e.constructor.name + ": " + e.message + "|" + e.stack.split("\n")[1].trim() } })()`,
	// this where it is a binding: a derived constructor's before and after
	// super(), and an arrow's inside one.
	`class A { constructor() { this.a = 1 } } class B extends A { constructor(early) { var r = "", f = () => this.a;
	   if (early) { try { r += this.a } catch (e) { r += e.constructor.name } try { r += f() } catch (e) { r += e.constructor.name } }
	   super(); for (var i = 0; i < 2; i++) { this.a += i; r += "," + this.a + f() } this.r = r } }
	 new B(true).r + " " + new B(false).r`,
	`var o = { k: 2, m() { var s = 0; for (var i = 0; i < 3; i++) { s += this.k; this.k = this.k + 1 } return s + "," + this.k } }; o.m() + " " + o.m.call({ k: "x" })`,
	// Writes to globals, and strict mode's check before one, where the slot
	// the site remembers stops being the name's: deleted, made again
	// elsewhere, turned into an accessor or read-only.
	`globalThis.g1 = 0; globalThis.g2 = 0; var log = []; function w(n) { "use strict"; for (var i = 0; i < n; i++) { g1 = i; g2 = g1 + 1 } return g1 + "," + g2 }
	 function s(n) { for (var i = 0; i < n; i++) { g1 = i; g3 = i } return g1 + "," + g3 }
	 function c(n) { "use strict"; for (var i = 0; i < n; i++) g1 = (globalThis.g1 = 9, i) }
	 var r = [w(3), s(2), c(1)]; delete globalThis.g1; try { w(1) } catch (e) { r.push(e.constructor.name + ": " + e.message) }
	 try { c(1) } catch (e) { r.push(e.constructor.name + ": " + e.message + " " + g1) } delete globalThis.g1;
	 globalThis.zz = 1; globalThis.g1 = 5; r.push(w(2), s(1));
	 Object.defineProperty(globalThis, "g1", { get() { log.push("get"); return 7 }, set(v) { log.push("set" + v) }, configurable: true });
	 r.push(w(2), s(1)); Object.defineProperty(globalThis, "g2", { value: 1, writable: false });
	 try { w(1) } catch (e) { r.push(e.constructor.name) } r.push(s(1), g2, log.join()); r.join(" ")`,
	// Updates of an element -- op=, ++ and -- as statements and as values --
	// and the order the object, the key, the element and the value are taken
	// in: the key converted once, the object and key read before the value
	// changes them.
	`var log = [], k = { toString() { log.push("k"); return "x" } };
	 function f() { var a = [1, 2, 3], o = { x: 1 }, i = 0, r = [];
	   for (var j = 0; j < 3; j++) { a[j] += j; a[i] *= 2; a[j] -= a[i]; o[k] += 1; o["x"] |= 4; a[i] += (i = 1);
	     r.push((a[0] += 5) * 2, a[1]++, ++a[2], a[j]--, a[i] ** 1, o.x) }
	   return a.join() + " " + o.x + " " + log.join() + " " + r.join() } f()`,
	`var log = [], p = new Proxy([1, 2], { get(t, k) { log.push("get " + String(k)); return t[k] },
	   set(t, k, v) { log.push("set " + String(k) + "=" + v); t[k] = v; return true } });
	 var g = { get 0() { log.push("g"); return 1 }, set 0(v) { log.push("s" + v) } };
	 function f(h, s, ta) { for (var i = 0; i < 2; i++) { h[1] += 1; p[i] += 10; ta[i] += 200; g[0] += 1; h[5] -= 1; s[0] += "x"; h[i + 0.5] += 1 }
	   return h.join() + "|" + h[5] + "|" + h[0.5] + "|" + ta.join() + "|" + s + "|" + log.join() } f([0, , 2], "ab", new Int8Array(2))`,
	`var bad = { toString() { throw new Error("key") } };
	 function f(n, k) { for (var i = 0; i < 1; i++) n[k] += 1 }
	 function g(a) { "use strict"; for (var i = 0; i < 1; i++) a[0] += 1 }
	 var r = []; for (var v of [null, undefined, {}]) { try { f(v, bad) } catch (e) { r.push(e.constructor.name + ": " + e.message) } }
	 try { g(Object.freeze([1])) } catch (e) { r.push(e.constructor.name + ": " + e.message) }
	 r.join()`,
	// Exceptions from inside an expression, and their stacks.
	`function f(a) { var s = 0; for (var i = 0; i < a.length; i++) { s += a[i].x.y } return s }
	 try { f([{ x: { y: 1 } }, { x: null }]) } catch (e) { e.constructor.name + " " + e.message + "|" + e.stack.split("\n").slice(0, 3).join("|") }`,
	`function g(v) { if (v > 1) throw new RangeError("big " + v); return v }
	 function f() { var s = 0; for (var i = 0; i < 5; i++) s += g(i) * 2; return s }
	 try { f() } catch (e) { e.message + "|" + e.stack.split("\n").slice(0, 3).join("|") }`,
	`function f() { var o = { valueOf() { throw new TypeError("no") } }; var s = 0; for (var i = 0; i < 2; i++) s += i * o; return s }
	 try { f() } catch (e) { e.message + "|" + e.stack.split("\n").slice(0, 3).join("|") }`,
	`function f(o) { for (var i = 0; i < 2; i++) o.p.q = i } try { f({}) } catch (e) { e.message + "|" + e.stack.split("\n").slice(0, 2).join("|") }`,
}

// treeRun evaluates a script and gives its value, or its error.
func treeRun(t *testing.T, src string) string {
	t.Helper()
	rt := quickjs.New()
	defer rt.Close()
	v, err := rt.Eval(src)
	if err != nil {
		return "error: " + err.Error()
	}
	return v.String()
}

// TestTreeTierMatchesInterpreter runs each of treeScripts with every
// function the tree tier can build built, and compares it with the
// interpreter's run.
func TestTreeTierMatchesInterpreter(t *testing.T) {
	defer vm.SetTreeTier(true, false)
	for _, src := range treeScripts {
		vm.SetTreeTier(false, false)
		want := treeRun(t, src)
		vm.SetTreeTier(true, true)
		before := vm.TreesBuilt()
		got := treeRun(t, src)
		if got != want {
			t.Errorf("%s\n tree: %s\ninterp: %s", src, got, want)
		}
		if vm.TreesBuilt() == before {
			t.Errorf("%s\nno function was built as a tree", src)
		}
	}
}

// TestGlobalWriteSites pins, in both tiers, writes to globals and strict
// mode's check before one where the slot a site remembers stops being the
// name's. The tiers share the sites' code, so comparing them is not enough.
// A strict assignment whose value creates the global it names is still a
// ReferenceError, as the specification says and C QuickJS no longer does.
func TestGlobalWriteSites(t *testing.T) {
	defer vm.SetTreeTier(true, false)
	var src string
	for _, s := range treeScripts {
		if strings.HasPrefix(s, "globalThis.g1 = 0;") {
			src = s
		}
	}
	const want = "2,3 1,1  ReferenceError: g1 is not defined ReferenceError: g1 is not defined 9 1,2 0,0 7,8 7,0 " +
		"TypeError 7,0 1 set0,get,set1,get,get,set0,get,set0,get,set0,get"
	for _, tier := range []bool{false, true} {
		vm.SetTreeTier(tier, tier)
		if got := treeRun(t, src); got != want {
			t.Errorf("tree tier %v:\n got %s\nwant %s", tier, got, want)
		}
	}
}

// TestArrayAppendThroughTheChain pins, in both tiers, assignments at an
// array's end, which skip the walk up the chain when nothing on it has an
// element or an index -- and must take it whenever something does: a setter
// or a read-only element on Array.prototype or Object.prototype, a proxy or
// an object with indices as the prototype, an accessor on the array itself,
// an array that may not grow, a sparse one. C QuickJS and Node agree.
func TestArrayAppendThroughTheChain(t *testing.T) {
	defer vm.SetTreeTier(true, false)
	const src = `function fill(a, n) { for (var i = 0; i < n; i++) a[i] = i; return a; }
		function sfill(a, n) { "use strict"; for (var i = 0; i < n; i++) a[i] = i; return a; }
		var log = [], r = [];
		r.push(fill([], 3).join());
		Object.defineProperty(Array.prototype, 2, { set(v) { log.push("A" + v) }, get() { return "g" }, configurable: true });
		r.push(fill([], 4).join() + "|" + fill([], 4).length); delete Array.prototype[2];
		Object.prototype[1] = "op"; Object.defineProperty(Object.prototype, 1, { set(v) { log.push("O" + v) }, configurable: true });
		r.push(fill([], 3).length); delete Object.prototype[1];
		Object.defineProperty(Object.prototype, 0, { value: "ro", writable: false, configurable: true });
		r.push(fill([], 2).join()); try { sfill([], 1) } catch (e) { r.push(e.constructor.name) } delete Object.prototype[0];
		r.push(fill(Object.preventExtensions([]), 2).length); try { sfill(Object.preventExtensions([]), 1) } catch (e) { r.push(e.constructor.name) }
		var ro = []; Object.defineProperty(ro, "length", { writable: false }); r.push(fill(ro, 2).length); try { sfill(ro, 1) } catch (e) { r.push(e.constructor.name) }
		var p = new Proxy({}, { set(t, k, v, rcv) { log.push("P" + k); return Reflect.set(t, k, v, rcv) } });
		var a = []; Object.setPrototypeOf(a, p); r.push(fill(a, 2).length);
		var b = [], q = { 1: "q" }; Object.setPrototypeOf(b, q); r.push(Array.prototype.join.call(fill(b, 2)));
		var c = []; Object.defineProperty(c, 0, { get() { return "own" }, set(v) { log.push("C" + v) }, configurable: true }); r.push(fill(c, 2).join());
		var s = []; s[5000] = 1; r.push(fill(s, 3).slice(0, 3).join() + "," + s.length);
		class Sub extends Array {} r.push(fill(new Sub(), 3).length, Array.isArray(fill(new Sub(), 1)));
		var t = Object.create(fill([], 2)); r.push(fill(t, 3).length, Object.keys(t).join());
		r.push(log.join()); r.join(" ")`
	const want = "0,1,2 0,1,g,3|4 3 ro,1 TypeError 0 TypeError 0 TypeError 2 0,1 own,1 0,1,2,5001 3 true 2 0,1,2 " +
		"A2,A2,O1,P0,P1,C0"
	for _, tier := range []bool{false, true} {
		vm.SetTreeTier(tier, tier)
		if got := treeRun(t, src); got != want {
			t.Errorf("tree tier %v:\n got %s\nwant %s", tier, got, want)
		}
	}
}

// TestArgumentsReads pins, in both tiers, arguments[i] and arguments.length in
// functions that read the arguments themselves until something needs the
// object: a mapped index naming a parameter, a key that names no argument, a
// write, a delete, a call through it, apply. And that a function that
// rebinds the name arguments does not. Node gives the same answers.
func TestArgumentsReads(t *testing.T) {
	defer vm.SetTreeTier(true, false)
	cases := []struct{ src, want string }{
		{`var r = [];
		function s1(a, b) { "use strict"; a = 9; var x = []; for (var i = 0; i < arguments.length + 1; i++) x.push(arguments[i]); return x.join("/") + ":" + arguments.length; }
		function m1(a, b) { a = 9; var x = []; for (var i = 0; i < arguments.length + 1; i++) x.push(arguments[i]); return x.join("/") + ":" + arguments.length; }
		function m2(a) { var x = arguments[1]; a = 7; return [x, arguments[0], arguments[1], arguments.length].join("/"); }
		function keys(k) { return [arguments[k], arguments["1"], arguments[-0], arguments[0.5], arguments[1e10], arguments.length].join("/"); }
		function wr() { arguments[0] = "w"; arguments.length = 7; return [arguments[0], arguments[1], arguments.length].join("/"); }
		function del() { var a = arguments[0]; delete arguments[0]; return [a, arguments[0], arguments.length].join("/"); }
		function swr() { "use strict"; arguments[1] = "s"; return [arguments[0], arguments[1], arguments.length].join("/"); }
		function call() { return arguments[0](); }
		function both(f) { var n = arguments.length; return f.apply(this, arguments) + n + arguments[1]; }
		function sum() { var s = 0; for (var i = 0; i < arguments.length; i++) s += arguments[i]; return s; }
		function viaProto() { return arguments[3]; }
		function getter(o) { "use strict"; return arguments[0].x + arguments.length; }
		r.push(s1(1, 2, 3), m1(1, 2, 3), m1(), s1(), m2(1, 2), keys(0, 1), keys("length", "b"), wr(1, 2), del(1, 2), swr(1, 2));
		r.push(call(function () { return typeof this + (this.length) }, 5), both(function () { return arguments.length }, "x"), sum(1, 2, 3, 4));
		Object.prototype[3] = "proto"; r.push(viaProto(1, 2), viaProto(1, 2, 3, 4)); delete Object.prototype[3];
		r.push(getter({ x: 1 }, 2));
		r.join(" | ")`,
			"1/2/3/:3 | 9/2/3/:3 | :0 | :0 | 2/7/2/2 | 0/1/0///2 | 2/b/length///2 | w/2/7 | 1//2 | 1/s/2 | " +
				"object2 | 4x | 10 | proto | 4 | 3"},
		{`var r = [];
		function v1() { var arguments = "xy"; return arguments[0] + arguments.length; }
		function v2() { arguments = "pq"; return arguments[1] + arguments.length; }
		function v3() { function arguments() {} return arguments.length + "/" + arguments[0]; }
		function v4() { for (var arguments of ["ab"]) ; return arguments[0] + arguments.length; }
		function v5() { try { throw "zz" } catch (arguments) { return arguments[0] + arguments.length } }
		function v6() { { let arguments = "kk"; r.push(arguments[1]) } return arguments[0]; }
		function v7() { [arguments] = ["dd"]; return arguments[0] + arguments.length; }
		function v8() { ({ a: arguments } = { a: "ee" }); return arguments[1]; }
		function v9() { var f = () => arguments[0]; return f() + arguments[1]; }
		function v10(a = arguments[1]) { return a + arguments[0]; }
		function v11() { eval("var q = 1"); return arguments[0]; }
		function v12() { arguments++; return arguments + "/" + arguments[0]; }
		r.push(v1(1), v2(1), v3(1), v4(1), v5(1), v6(1), v7(1), v8(1), v9(1, 2), v10(1, 2), v11(3), v12(4));
		r.join(" | ")`,
			"k | x2 | q2 | 0/undefined | a2 | z2 | 1 | d2 | e | 3 | 2 | 3 | NaN/undefined"},
	}
	for _, tc := range cases {
		for _, tier := range []bool{false, true} {
			vm.SetTreeTier(tier, tier)
			if got := treeRun(t, tc.src); got != tc.want {
				t.Errorf("tree tier %v:\n got %s\nwant %s", tier, got, tc.want)
			}
		}
	}
}

// TestTreeTierInterrupted stops a loop running as a tree, as one running in
// the interpreter is stopped.
func TestTreeTierInterrupted(t *testing.T) {
	defer vm.SetTreeTier(true, false)
	vm.SetTreeTier(true, true)
	rt := quickjs.New()
	defer rt.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	before := vm.TreesBuilt()
	_, err := rt.EvalContext(ctx, `function spin() { var n = 0; for (;;) { n = n + 1 } } spin()`)
	if err == nil || !errors.Is(err, context.DeadlineExceeded) && !strings.Contains(err.Error(), "interrupt") {
		t.Fatalf("got %v, want the deadline", err)
	}
	if vm.TreesBuilt() == before {
		t.Fatal("spin was not built as a tree")
	}
}

// TestTreeTierSharedAcrossRuntimes runs one compiled program in runtimes on
// several goroutines at once. They share its functions, and so the trees
// built for them, which the first to run each one builds.
func TestTreeTierSharedAcrossRuntimes(t *testing.T) {
	defer vm.SetTreeTier(true, false)
	vm.SetTreeTier(true, true)
	p, err := quickjs.Compile("shared.js", `function f(n) { var s = 0, a = []; for (var i = 0; i < n; i++) { a.push(i * 2); s += a[i] % 7 } return s } f(2000)`)
	if err != nil {
		t.Fatal(err)
	}
	const workers = 8
	results := make(chan string, workers)
	for w := 0; w < workers; w++ {
		go func() {
			rt := quickjs.New()
			defer rt.Close()
			v, err := rt.RunProgram(p)
			if err != nil {
				results <- err.Error()
				return
			}
			results <- v.String()
		}()
	}
	for w := 0; w < workers; w++ {
		if got := <-results; got != "5998" {
			t.Errorf("got %s, want 5998", got)
		}
	}
}
