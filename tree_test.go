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
	// == and != of two numbers, as a value and ending a block: NaN, 0 and
	// -0, infinities, and a number beside a string, a boolean, null and an
	// object, which are not two numbers.
	`function f(a, b) { var r = []; for (var i = 0; i < 1; i++) { r.push(a == b, a != b); if (a == b) r.push("eq"); if (a != b) r.push("ne") } return r.join("") }
	 var vals = [0, -0, 1, NaN, Infinity, -Infinity, "1", true, null, { valueOf() { return 1 } }], out = [];
	 for (var x of vals) for (var y of vals) out.push(f(x, y)); out.join(" ")`,
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
	// Updates of a property, which read the object twice rather than copy
	// it where it is a local, an upvalue or this: the order of the getter,
	// valueOf and the setter; a getter that reassigns the very variable the
	// update reads, which the write must not see; a key's toString once; a
	// constant key read twice; a null base; an object that is not a plain
	// read, which is copied; values used and unused; BigInts and Symbols.
	`function f() { var log = [], o = {}, v = { valueOf() { log.push("v"); return 1 } };
	   Object.defineProperty(o, "x", { get() { log.push("g"); return v }, set(n) { log.push("s" + n) } });
	   for (var i = 0; i < 2; i++) { o.x += 1; o.x++; ++o.x; log.push(o.x++ + "|" + ++o.x) }
	   return log.join() } f()`,
	`function f() { var r = [], a = { n: 1 }, b = { n: 10 }, o = a;
	   Object.defineProperty(a, "m", { get() { o = b; return 5 }, set(v) { r.push("a.m=" + v) } });
	   for (var i = 0; i < 2; i++) { o = a; o.m += 1; r.push(o === b); o = a; o.m++; o = a; o[0] = 1; o[0] += o === a ? 1 : 2 }
	   return r.join() + " " + a.n + " " + b.n + " " + a[0] } f()`,
	`function f() { var r = [], k = { toString() { r.push("k"); return "p" } }, o = { p: 1 }, t = [1, 2, 3];
	   for (var i = 0; i < 2; i++) { o[k] += 1; o[k]++; t[0] += 10; t[1]++; t[2] -= o[k]; r.push(o.p, t.join("/")) }
	   return r.join() } f()`,
	`function f(c) { var a = { x: 1 }, b = { x: 100 };
	   for (var i = 0; i < 2; i++) { (c ? a : b).x += 1; (c ? a : b).x++ } return a.x + " " + b.x }
	 function g(t) { for (var i = 0; i < 2; i++) t[0] += 1 } function h(t) { for (var i = 0; i < 2; i++) t.x++ }
	 function errs() { var r = []; for (var t of [null, undefined]) { try { g(t) } catch (e) { r.push(e.message, e.stack) }
	   try { h(t) } catch (e) { r.push(e.message, e.stack) } } return r.join() }
	 f(true) + " " + f(false) + " " + errs()`,
	`function C() { this.n = 0; this.big = 1n } C.prototype.bump = function () { this.n++; this.big += 2n; return this };
	 C.prototype.down = function (o) { o.count--; o.k -= 3 };
	 function f() { var c = new C(), o = { count: 5, k: "9" }; for (var i = 0; i < 3; i++) { c.bump(); c.down(o) }
	   var s = { x: Symbol() }, e1, e2; try { s.x++ } catch (e) { e1 = e.constructor.name } try { ++s.x } catch (e) { e2 = e.constructor.name }
	   return [c.n, c.big, o.count, o.k, e1, e2].join() } f()`,
	// Strict assignments to globals: of a value that runs no code, one
	// set_global_strict; of anything else, the check before the value and
	// the assertion after it. A proxy on the global object's chain is asked
	// has twice and then set, by both; a name that resolves to nothing is a
	// ReferenceError at the name; a value in its dead zone throws first.
	`"use strict"; var log = [], g1 = 0, base = Object.getPrototypeOf(globalThis); base.pg1 = 1; base.pg2 = 2;
	 Object.setPrototypeOf(globalThis, new Proxy(base, {
	   has(t, k) { if (typeof k == "string" && k[0] == "p") log.push("has " + k); return Reflect.has(t, k) },
	   set(t, k, v, r) { if (typeof k == "string" && k[0] == "p") log.push("set " + k); return Reflect.set(t, k, v, r) } }));
	 function f(j) { for (var i = 0; i < 2; i++) { g1 = j; g1 = 5; pg1 = j; pg2 = i + j; pg1 = this } }
	 function u(n) { for (var i = 0; i < 1; i++) { if (n === 0) undeclared1 = 1; else if (n === 1) undeclared2 = g1;
	   else if (n === 2) undeclared3 = n + 1; else if (n === 3) undeclared4 = function () {}; else { g1 = z; let z = 1 } } }
	 function errs() { var r = []; for (var n = 0; n < 5; n++) try { u(n) } catch (e) { r.push(e.message, e.stack) } return r.join() }
	 f(3); log.join() + " | " + g1 + " " + pg1 + " " + pg2 + " " + Object.hasOwn(globalThis, "pg1") + " | " + errs()`,
	// RegExp literals, a new object each time round with its own lastIndex,
	// and patterns that are nothing but characters, which a search answers
	// without the matcher: exec, test, replace, split, match, sticky and
	// global, case folding, a miss.
	`function f(s) { var r = []; for (var i = 0; i < 3; i++) { var re = /fox/g, a = /fox/;
	   r.push(re.lastIndex, re.test(s), re.lastIndex, a.exec(s).index, /over the/.exec(s)[0], /cat/.exec(s),
	     s.replace(/o/g, "0"), s.split(/ /).length, s.match(/the/g).length, /FOX/i.test(s), /quick/y.test(s), re === /fox/g) }
	   return r.join() } f("the quick brown fox jumped over the lazy dog")`,
	// What pushes a value the compiler once counted as pushing nothing -- a
	// RegExp literal, a tagged template's strings -- at the deepest point of
	// a call's operands: a frame sized one slot short spills past its end.
	`function g(a, b, c) { return String(a) + String(b) + String(c) }
	 function tag(s, x) { return s.raw.join("|") + x }
	 function f() { var r = []; for (var i = 0; i < 2; i++) {
	   r.push(g(i, i + 1, /x+/g), g(i, [i], tag` + "`a${i}b`" + `), g(/a/, /b/, /c/.source)) } return r.join() } f()`,
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
	// .length of an array, a sparse one, one whose length was set, a
	// subclass's, a string, a string wrapper, a typed array, an object with
	// a length and one with a getter, a function, and null.
	`class A extends Array {} var sp = []; sp[100000] = 1; var big = [1, 2]; big.length = 7;
	 var vals = [[1, 2, 3], sp, big, new A(4), "héllo", new String("ab"), new Int8Array(3), { length: "x" },
	   { get length() { return 9 } }, function (a, b) {}, ""];
	 function f(v) { for (var i = 0; i < 1; i++) return v.length } var r = vals.map(f);
	 try { f(null) } catch (e) { r.push(e.constructor.name) } r.join(",")`,
	// Updates of a property -- op=, ++ and -- as statements and values -- and
	// the order the object, the property and the value are taken in: the
	// object read before a value that reassigns it, a getter and a setter
	// called once each, a proxy's traps, a primitive base, and a frozen
	// object, quietly and in strict mode.
	`var log = [], g = { get k() { log.push("get"); return 1 }, set k(v) { log.push("set" + v) } };
	 var p = new Proxy({ k: 5 }, { get(t, n) { log.push("pget " + String(n)); return t[n] }, set(t, n, v) { log.push("pset " + String(n) + "=" + v); t[n] = v; return true } });
	 function f(o, q) { var r = []; for (var i = 0; i < 2; i++) { o.a += i; o.b *= 2; r.push(o.a++, ++o.b, (o.c -= 1) * 10); o.a += (o = q, 100) }
	   g.k += 1; p.k += 2; var s = "ab"; s.length += 1; return r.join() + "|" + JSON.stringify(q) + "|" + s.length }
	 function h(o) { for (var i = 0; i < 1; i++) o.k += 1 } function sh(o) { "use strict"; for (var i = 0; i < 1; i++) o.k += 1 }
	 var out = [f({ a: 1, b: 1, c: 0 }, { a: 7, b: 2, c: 1 })], fr = Object.freeze({ k: 1 }); h(fr); out.push(fr.k);
	 for (var v of [null, undefined, fr]) { try { sh(v) } catch (e) { out.push(e.constructor.name + ": " + e.message) } }
	 out.join(" ") + " " + log.join()`,
	// Reads at a constant index: a hole, past the end, an element up the
	// chain, a string, a typed array, a mapped arguments object, -0, and a
	// base that is null.
	`Array.prototype[1] = "q"; Array.prototype[2] = "p"; function f(a) { var r = []; for (var i = 0; i < 1; i++) r.push(String(a[0]), String(a[1]), String(a[2]), String(a[3]), String(a[-0]), String(a[2147483648])); return r.join("/") }
	 function g(x) { var a = arguments; x = 5; return f(a) } function h(a) { for (var i = 0; i < 1; i++) return a[0] }
	 var d = [1, 2, 3]; delete d[1]; var e = new Array(3); var r = [f([1, , 3]), f(d), f(e), f([1]), f("str"), f(new Int8Array([7, 8])), f({ 0: "o", 3: "t" }), g(1)];
	 try { h(null) } catch (e) { r.push(e.constructor.name + ": " + e.message) } delete Array.prototype[1]; delete Array.prototype[2]; r.join(" ")`,
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
	// Optional chains and ??, whose value stays on the stack across the
	// jump that tests it: links that short-circuit at each place, method
	// calls keeping their receiver, computed links, a getter in the chain,
	// ?? over falsy values that are not nullish, ??= and ?. in a loop's
	// condition, which jumps back, and a link without ?. that throws.
	`function f(objs) { var r = []; for (var i = 0; i < objs.length; i++) { var o = objs[i];
	   r.push(String(o?.a?.b?.c), String(o?.a?.["b"]?.c ?? "dflt"), String(o?.m?.(i)), String(o?.a?.b?.c ?? 0)) } return r.join() }
	 var g = { get a() { return { b: { c: "gc" } } } };
	 f([undefined, null, {}, { a: null }, { a: { b: { c: 0 } } }, { a: { b: { c: "" } } }, { m(x) { return this === objs3 ? "self" + x : "other" } }, g])
	 var objs3; objs3 = { m(x) { return this === objs3 ? "self" + x : "other" } }; f([objs3, { a: { b: { c: false } } }])`,
	`function f(n) { var r = [], x = null, y; for (var i = 0; i < n; i++) { x ??= i; y = (y ?? 10) - 1; r.push(x, y, i ?? -1, null ?? i, undefined ?? null ?? i) } return r.join() } f(4)`,
	`function f(list) { var n = 0, p = list; while ((p = p?.next) ?? false) n++; return n }
	 f({ next: { next: { next: null } } }) + "," + f(null) + "," + f({ next: undefined })`,
	`function f(o) { var s = 0; for (var i = 0; i < 2; i++) s += o?.a.b ?? 1; return s }
	 [f(undefined), f({ a: { b: 5 } })].join() + "|" + (function () { try { return f({}) } catch (e) { return e.constructor.name } })()`,
	// Tail calls, which give up their frame once the stack is deep: plain,
	// a method's with its receiver, mutual, through a bound function and a
	// native one, from a constructor -- whose frame is kept -- and one that
	// throws deep down, whose stack is what the frames left make it.
	`"use strict"; function f(n, acc) { return n === 0 ? acc : f(n - 1, acc + 1) }
	 const o = { k: 2, m(n) { return n === 0 ? this.k : this.m(n - 1) } };
	 function even(n) { return n === 0 || odd(n - 1) } function odd(n) { return n !== 0 && even(n - 1) }
	 function b(n) { return n === 0 ? "b" : bb(n - 1) } const bb = b.bind(null);
	 function m(n) { return n === 0 ? Math.max(1, 2) : m(n - 1) }
	 function C(n) { this.n = n; return n === 0 ? undefined : g(n - 1) } function g(n) { return new C(n) }
	 [f(50000, 0), o.m(50000), even(50001), b(50000), m(50000), new C(40).n].join()`,
	`"use strict"; function f(n) { if (n === 0) throw new Error("deep"); return f(n - 1) }
	 try { f(100) } catch (e) { e.message + "|" + e.stack.split("\n").length }`,
	// A local given an arithmetic or bitwise result in one statement: each
	// pairing of a local, a constant and a tree, the slow paths -- strings,
	// valueOf, a BigInt, and mixing one with a number, which throws -- a
	// local stored into from itself, %, which has no such statement, an
	// upvalue, a chained assignment, and the bitwise operators with a
	// constant over a local and over a tree.
	`function f(a, b, s, o, big) { var x, y, z, r = [];
	   for (var i = 0; i < 3; i++) {
	     x = a + b; r.push(x); x = a - i; r.push(x); x = i * b; r.push(x); x = a / 2; r.push(x);
	     x = 3 - a; r.push(x); x = (a + 1) * (b - 1); r.push(x); x = (a + i) / b; r.push(x); x = 2 * (b + i); r.push(x);
	     x = s + a; r.push(x); x = a + s; r.push(x); x = o * 2; r.push(x); x = o - a; r.push(x);
	     x = x + x; r.push(x); x = a % 3; r.push(x); y = z = a + i; r.push(y, z);
	     x = a & 6; r.push(x); x = a | 8; r.push(x); x = a ^ 5; r.push(x); x = a << 2; r.push(x);
	     x = -a >> 1; r.push(x); x = -a >>> 28; r.push(x); x = (a + b) & 0xff; r.push(x); x = o | 0; r.push(x);
	     x = big + 1n; r.push(String(x));
	     try { x = big + a } catch (e) { r.push(e.constructor.name) }
	     a = a + 1.5 }
	   return r.join() }
	 f(7, 3, "s", { valueOf() { return 10 } }, 5n)`,
	`function f(n) { var up = 4; function g(k) { var x = up * k + up; up = up - 1; return x } var s = 0;
	   for (var i = 0; i < n; i++) s = s + g(i); return s + "," + up } f(5)`,
	// An exception thrown in a tree that trees called, which goes past
	// their frames to the try that catches it: closures over the locals of
	// the frames it passed see their last values, the stack trace is the
	// same, and the frames are gone -- calls go on at the same depth, and a
	// recursion that runs out of stack can be caught again and again.
	`var fs = [], depth = 0;
	 function C(n) { this.n = n }
	 C.prototype.a = function (k) { var x = k * 2; fs.push(function () { return x }); x = x + 1; return this.b(x) };
	 C.prototype.b = function (k) { var y = k + 100; fs.push(function () { return y }); y = y - 1; return this.c(y) };
	 C.prototype.c = function (k) { if (k > this.n) throw new RangeError("big " + k); return k };
	 C.prototype.deep = function (k) { depth = k; return this.deep(k + 1) + 1 };
	 function run() { var o = new C(150), r = [];
	   for (var i = 0; i < 40; i++) { try { r.push(o.a(i)) } catch (e) { r.push(e.message) } }
	   for (var j = 0; j < 3; j++) { try { o.deep(0) } catch (e) { r.push(e.constructor.name, depth > 100) } }
	   try { o.a(1000) } catch (e) { r.push(e.stack.split("\n").length) }
	   r.push(fs.map(function (f) { return f() }).join("/"), o.a(1));
	   return r.join() }
	 run()`,
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
	defer vm.SetTreeTier(true)
	for _, src := range treeScripts {
		vm.SetTreeTier(false)
		want := treeRun(t, src)
		vm.SetTreeTier(true)
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
	defer vm.SetTreeTier(true)
	var src string
	for _, s := range treeScripts {
		if strings.HasPrefix(s, "globalThis.g1 = 0;") {
			src = s
		}
	}
	const want = "2,3 1,1  ReferenceError: g1 is not defined ReferenceError: g1 is not defined 9 1,2 0,0 7,8 7,0 " +
		"TypeError 7,0 1 set0,get,set1,get,get,set0,get,set0,get,set0,get"
	for _, tier := range []bool{false, true} {
		vm.SetTreeTier(tier)
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
	defer vm.SetTreeTier(true)
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
		vm.SetTreeTier(tier)
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
	defer vm.SetTreeTier(true)
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
			vm.SetTreeTier(tier)
			if got := treeRun(t, tc.src); got != tc.want {
				t.Errorf("tree tier %v:\n got %s\nwant %s", tier, got, tc.want)
			}
		}
	}
}

// TestArrayPushThroughTheChain pins, in both tiers, push and the other array
// methods that skip the walk up the chain when its prototypes have no index:
// a setter on Array.prototype or Object.prototype, and a read-only element on
// an Array.prototype large enough to be indexed. Node gives the same answers.
func TestArrayPushThroughTheChain(t *testing.T) {
	defer vm.SetTreeTier(true)
	const src = `var log = "", r = [];
		function fill(n) { var a = []; for (var i = 0; i < n; i++) a.push(i); return a; }
		r.push(fill(3).join());
		Object.defineProperty(Array.prototype, 1, { set(v) { log += "A" + v + "," }, get() { return "g" }, configurable: true });
		r.push(fill(3).join()); delete Array.prototype[1];
		Object.defineProperty(Object.prototype, 0, { set(v) { log += "O" + v + "," }, configurable: true });
		r.push(fill(2).length); delete Object.prototype[0];
		for (var k = 0; k < 40; k++) Array.prototype["m" + k] = k;
		Object.defineProperty(Array.prototype, 2, { value: "ro", writable: false, configurable: true });
		try { r.push(fill(3).join()) } catch (e) { r.push(e.constructor.name) } delete Array.prototype[2];
		r.push(fill(4).join(), [1, 2].concat([3]).join(), [3, 1, 2].sort().join(), log);
		r.join(" | ")`
	const want = "0,1,2 |  | 2 | TypeError | 0,1,2,3 | 1,2,3 | 1,2,3 | A1,A0,g,2,O0,"
	for _, tier := range []bool{false, true} {
		vm.SetTreeTier(tier)
		if got := treeRun(t, src); got != want {
			t.Errorf("tree tier %v:\n got %s\nwant %s", tier, got, want)
		}
	}
}

// TestElementKeysAndViews pins, in both tiers, element reads and writes
// whose key is not an index -- -0, a fraction, a negative, NaN, 2**32, a
// string, null, a boolean, undefined, a BigInt, a Symbol -- which the fast
// paths refuse by the key's number alone, and typed arrays whose view is of
// a fixed length over a buffer that shrinks, grows back and is detached,
// which the fast element paths answer without countIn. C QuickJS and Node
// give the same answer.
func TestElementKeysAndViews(t *testing.T) {
	defer vm.SetTreeTier(true)
	const src = `function f() {
		  var r = [], a = [10, , 30], s = Symbol("s"), ks = [0, -0, 1, 2, 1.5, -1, NaN, 2 ** 32, "0", null, true, undefined, 0n, s];
		  a[s] = "sym"; a["null"] = "N"; a["true"] = "T"; a["undefined"] = "U"; a["1.5"] = "F"; a["-1"] = "M"; a["NaN"] = "nan"; a["4294967296"] = "big";
		  for (var i = 0; i < ks.length; i++) { var k = ks[i]; r.push(String(a[k])); }
		  for (var i = 0; i < ks.length; i++) { var k = ks[i], b = [1, , 3]; b[k] = 7; r.push(b.length + ":" + b[0] + b[1] + b[2]); }
		  return r.join(",");
		}
		function g() {
		  var r = [], rab = new ArrayBuffer(16, { maxByteLength: 32 });
		  var fixed = new Int32Array(rab, 4, 2), track = new Float64Array(rab), u8 = new Uint8ClampedArray(rab, 0, 4), i8 = new Int8Array(rab, 8, 4);
		  for (var i = 0; i < 4; i++) { fixed[i] = i * 1000 + 0.75; u8[i] = i * 100 - 20.5; i8[i] = 200 + i; }
		  for (var i = 0; i < 5; i++) r.push(fixed[i], u8[i], i8[i], track[i] === undefined);
		  rab.resize(10);
		  for (var i = 0; i < 3; i++) { fixed[i] = 5; r.push(fixed[i], fixed.length, u8[i], i8[i]); }
		  rab.resize(32);
		  for (var i = 0; i < 3; i++) r.push(fixed[i], fixed.length, i8[i]);
		  var big = new BigInt64Array(2), f32 = new Float32Array(2), dv = new Float64Array([1.5, -0, NaN]);
		  for (var i = 0; i < 2; i++) { big[i] = BigInt(i) - 3n; f32[i] = 1 / 3 + i; try { big[i] = 1 } catch (e) { r.push(e.constructor.name) } }
		  r.push(big[0], big[1], f32[0], f32[1], dv[0], 1 / dv[1], dv[2], dv[3]);
		  var buf = new ArrayBuffer(8), v = new Uint16Array(buf), o = { valueOf() { r.push("v"); return 9 } };
		  for (var i = 0; i < 2; i++) { v[i] = o; r.push(v[i]); }
		  buf.transfer();
		  for (var i = 0; i < 2; i++) { v[i] = o; r.push(v[i], v.length); }
		  return r.join(",");
		}
		f() + " | " + g()`
	const want = "10,10,undefined,30,F,M,nan,big,10,N,T,U,10,sym,3:7undefined3,3:7undefined3,3:173,3:1undefined7,3:1undefined3,3:1undefined3,3:1undefined3,3:1undefined3,3:7undefined3,3:1undefined3,3:1undefined3,3:1undefined3,3:7undefined3,3:1undefined3" +
		" | 0,0,-24,false,-875902488,80,-55,false,,180,-54,true,,255,-53,true,,,,true,,0,0,,,0,80,,,0,180,,0,2,-24,51688,2,-55,,2,0,TypeError,TypeError,-3,-2,0.3333333432674408,1.3333333730697632,1.5,-Infinity,NaN,,v,9,v,9,v,,0,v,,0"
	for _, tier := range []bool{false, true} {
		vm.SetTreeTier(tier)
		if got := treeRun(t, src); got != want {
			t.Errorf("tree tier %v:\n got %s\nwant %s", tier, got, want)
		}
	}
}

// TestElementUpdates pins a[k] op= y and a[k]++ over a local array, which
// the tree tier updates in place when the element is a number in the
// array's dense storage, in both tiers: holes read through the prototype,
// typed arrays, frozen arrays, null bases, an operand a getter changes
// (read after the element), a valueOf that shrinks the array or reassigns
// the local, keys that are not indices, arguments objects, BigInts,
// strings, NaN and -0.
func TestElementUpdates(t *testing.T) {
	defer vm.SetTreeTier(true)
	const src = `function f() {
		  var r = [];
		  function upd(a, i, x) { for (var j = 0; j < 2; j++) { a[i] += x; a[i]++; a[0]--; a[1] -= x; a[i] *= 2; a[2] += 0.5 } return a }
		  r.push(upd([1, 2, 3, 4], 2, 3).join("/"), upd(["a", "b", "c"], 2, 3).join("/"), upd([1, , 3], 1, 1).join("/"));
		  Array.prototype[1] = 10;
		  r.push(upd([1, , 3], 1, 1).join("/"));
		  delete Array.prototype[1];
		  r.push(upd(new Int32Array([1, 2, 3]), 2, 3).join("/"), upd(new Uint8ClampedArray([1, 2, 3]), 2, 300).join("/"));
		  r.push(upd(Object.freeze([1, 2, 3]), 2, 3).join("/"), upd({ 0: 1, 1: 2, 2: 3 }, "2", 3)[2]);
		  function strict(a) { "use strict"; for (var j = 0; j < 2; j++) a[0] += 1; return a }
		  try { strict(Object.freeze([1])) } catch (e) { r.push(e.constructor.name) }
		  for (var base of [null, undefined]) {
		    try { upd(base, 0, 1) } catch (e) { r.push(e.message) }
		    try { (function (a) { for (var j = 0; j < 1; j++) a[3]-- })(base) } catch (e) { r.push(e.message) }
		  }
		  var y = 1, g = [1, 2];
		  Object.defineProperty(g, 1, { get() { y = 100; return 5 }, set(v) { r.push("set" + v) } });
		  (function (a) { for (var j = 0; j < 1; j++) { a[1] += y } })(g);
		  function own(a) { var x = 1; Object.defineProperty(a, 0, { get() { x = 50; return 1 }, configurable: true }); for (var j = 0; j < 1; j++) a[0] += x; return x }
		  r.push(own([0]));
		  var sh = [1, 2, 3];
		  (function (a, v) { for (var j = 0; j < 1; j++) a[2] += v })(sh, { valueOf() { sh.length = 0; return 1 } });
		  r.push(JSON.stringify(sh));
		  (function () { var a = [{ valueOf() { a = [7]; return 1 } }], b = a; for (var j = 0; j < 1; j++) a[0] += 1; r.push(b[0], a[0]) })();
		  (function (a) { for (var j = 0; j < 2; j++) { a[-1] += 1; a[1.5]++; a[2147483648] -= 1; a["0"] *= 3; a[4294967295]++ } r.push(Object.keys(a).join(), a[-1], a[1.5], a[2147483648], a[0], a[4294967295]) })([2]);
		  r.push((function (p) { for (var j = 0; j < 2; j++) arguments[0] += 1; return p })(1));
		  r.push((function (p) { "use strict"; for (var j = 0; j < 2; j++) arguments[0] += 1; return p + "," + arguments[0] })(1));
		  try { (function (a) { for (var j = 0; j < 1; j++) { a[0]++; a[1] *= 2 } })([1n, 2n]) } catch (e) { r.push(e.constructor.name) }
		  r.push((function (a) { for (var j = 0; j < 2; j++) a[0]++; return a[0] })(["5"]));
		  r.push(Object.is((function (a) { for (var j = 0; j < 1; j++) a[0] *= NaN; return a[0] })([0]), NaN));
		  r.push(Object.is((function (a) { for (var j = 0; j < 1; j++) a[0] *= 1; return a[0] })([-0]), -0));
		  r.push((function (a) { for (var j = 0; j < 1; j++) a[0] -= Infinity; return a[0] })([Infinity]));
		  return r.join(" ");
		}
		f()`
	const want = "-1/-4/37.5/4 NaN/NaN/NaN -1/NaN/4 -1/46/4 -1/-4/36 0/0/255 1/2/3 37.5 TypeError" +
		" cannot read property of null cannot read property of null cannot read property of undefined cannot read property of undefined" +
		" set105 50 [null,null,4] 2 7 0,2147483648,-1,1.5,4294967295 NaN NaN NaN 18 NaN 3 1,3 TypeError 7 true true NaN"
	for _, tier := range []bool{false, true} {
		vm.SetTreeTier(tier)
		if got := treeRun(t, src); got != want {
			t.Errorf("tree tier %v:\n got %s\nwant %s", tier, got, want)
		}
	}
}

// TestTypedElementAccess pins reads and writes of typed array elements by
// number -- read by getElem from the read node, written by typedWrite for a
// view of a fixed length, with the view's storage kept from the first write: every element type,
// out-of-range and fractional keys, a view at an offset, a fixed view whose
// resizable buffer shrinks past it and grows back, a detached buffer, an
// immutable one, shared ones (one that grows), values that are not numbers,
// a valueOf that detaches, updates, a subclass and an index on the prototype.
func TestTypedElementAccess(t *testing.T) {
	defer vm.SetTreeTier(true)
	const src = `var r = [];
		function rw(t, n, v) { var out = []; for (var i = -1; i <= n; i++) { t[i] = v + i; out.push(t[i]); } out.push(t[0], t[1], t[2]); return out.join("/"); }
		function rw0(t, v) { for (var j = 0; j < 2; j++) { t[0] = v; t[1] = t[0] + 1; } return [t[0], t[1], t[2]].join("/"); }
		var kinds = [Int8Array, Uint8Array, Uint8ClampedArray, Int16Array, Uint16Array, Int32Array, Uint32Array, Float16Array, Float32Array, Float64Array];
		for (var K of kinds) r.push(K.name + ":" + rw(new K(3), 3, 300.7) + ":" + rw0(new K(4), -1.5) + ":" + rw0(new K(4), NaN) + ":" + rw0(new K(4), 1e10));
		var big = new BigInt64Array(2); try { rw0(big, 1) } catch (e) { r.push(e.constructor.name) } big[0] = 5n; r.push(big[0], big[1]);
		var ub = new BigUint64Array(2); ub[0] = -1n; r.push(ub[0]);
		// offsets
		var buf = new ArrayBuffer(16), a = new Int16Array(buf, 4, 3), whole = new Uint8Array(buf);
		rw0(a, 0x1234); r.push(whole.join(","), a.length);
		// resizable: shrink under a fixed view, grow back
		var rab = new ArrayBuffer(16, { maxByteLength: 32 }), f = new Int32Array(rab, 4, 2), tr = new Int32Array(rab);
		f[0] = 1; f[1] = 2; r.push(rw0(f, 7), tr[1], tr[2]);
		rab.resize(8); r.push(f[0], f[1], f.length, rw0(f, 9), tr[1], tr.length); f[0] = 3; r.push(tr[1]);
		rab.resize(32); r.push(f[0], f[1], f.length, rw0(f, 11), tr[1], tr.length);
		// detach
		var d = new Float64Array(4); d[0] = 1.5; rw0(d, 2); var d2 = d.buffer.transfer(); r.push(d[0], d.length, rw0(d, 3), new Float64Array(d2)[0]);
		// immutable
		if (ArrayBuffer.prototype.transferToImmutable) {
		  var im = new Uint8Array([1, 2, 3]).buffer.transferToImmutable(), iv = new Uint8Array(im);
		  r.push(rw0(iv, 9));
		  try { (function () { "use strict"; iv[0] = 5 })() } catch (e) { r.push(e.constructor.name + ":" + e.message) }
		}
		// shared
		var sab = new SharedArrayBuffer(8), sv = new Int32Array(sab); r.push(rw0(sv, 42));
		var gsab = new SharedArrayBuffer(8, { maxByteLength: 16 }), gv = new Int32Array(gsab, 0, 2); r.push(rw0(gv, 43)); gsab.grow(16); r.push(rw0(gv, 44), new Int32Array(gsab)[3]);
		// strings and objects stored, valueOf detaching
		var o = { valueOf() { return 77 } }, vt = new Int8Array(2); vt[0] = o; vt[1] = "12"; r.push(vt[0], vt[1]);
		var det = new Uint8Array(4), dv = { valueOf() { det.buffer.transfer(); return 5 } }; det[0] = dv; r.push(det[0], det.length);
		// -0 and fractional keys
		var fk = new Float64Array(2); fk[-0] = 3; fk[0.5] = 4; fk["1"] = 6; r.push(fk[0], fk[0.5], fk[1], Object.keys(fk).join());
		r.push(Object.is(new Float32Array([-0])[0], -0), Object.is(new Float64Array([NaN])[0], NaN));
		// typed array updates in a loop with constant and local keys
		function upd(t, k) { for (var j = 0; j < 3; j++) { t[k] += 2; t[0]++; t[1] *= 3; } return Array.from(t).join("/") }
		r.push(upd(new Int8Array([1, 2, 3]), 2), upd(new Uint8ClampedArray([250, 2, 3]), 2), upd(new Float32Array([0.1, 0.2, 0.3]), 2));
		// subclass and proto with index
		class MyI32 extends Int32Array {} var mi = new MyI32(2); rw0(mi, 5); r.push(mi[0], mi[1]);
		Object.prototype[5] = "proto"; var pt = new Int8Array(2); r.push(pt[5], pt[1]); delete Object.prototype[5];
		r.join(" | ")`
	const want = "Int8Array:/44/45/46//44/45/46:-1/0/0:0/1/0:0/1/0 | Uint8Array:/44/45/46//44/45/46:255/0/0:0/1/0:0/1/0 | Uint8ClampedArray:/255/255/255//255/255/255:0/1/0:0/1/0:255/255/0 | Int16Array:/300/301/302//300/301/302:-1/0/0:0/1/0:-7168/-7167/0 | Uint16Array:/300/301/302//300/301/302:65535/0/0:0/1/0:58368/58369/0 | Int32Array:/300/301/302//300/301/302:-1/0/0:0/1/0:1410065408/1410065409/0" +
		" | " + "Uint32Array:/300/301/302//300/301/302:4294967295/0/0:0/1/0:1410065408/1410065409/0 | Float16Array:/300.75/301.75/302.75//300.75/301.75/302.75:-1.5/-0.5/0:NaN/NaN/0:Infinity/Infinity/0 | Float32Array:/300.70001220703125/301.70001220703125/302.70001220703125//300.70001220703125/301.70001220703125/302.70001220703125:-1.5/-0.5/0:NaN/NaN/0:10000000000/10000000000/0 | Float64Array:/300.7/301.7/302.7//300.7/301.7/302.7:-1.5/-0.5/0:NaN/NaN/0:10000000000/10000000001/0 | TypeError | 5" +
		" | " + "0 | 18446744073709551615 | 0,0,0,0,52,18,53,18,0,0,0,0,0,0,0,0 | 3 | 7/8/ | 7" +
		" | " + "8 |  |  | 0 | // | 7" +
		" | " + "2 | 7 | 7 | 0 | 2 | 11/12/" +
		" | " + "11 | 8 |  | 0 | // | 2" +
		" | " + "1/2/3 | TypeError:cannot assign to \"0\" of a typed array over an immutable ArrayBuffer | 42/43/ | 43/44/ | 44/45/ | 0" +
		" | " + "77 | 12 |  | 0 | 3 | " +
		" | " + "6 | 0,1 | true | true | 4/54/9 | 253/54/9" +
		" | " + "3.0999999046325684/5.400000095367432/6.300000190734863 | 5 | 6 |  | 0"
	for _, tier := range []bool{false, true} {
		vm.SetTreeTier(tier)
		if got := treeRun(t, src); got != want {
			t.Errorf("tree tier %v:\n got %s\nwant %s", tier, got, want)
		}
	}
}

// TestTreeTierInterrupted stops a loop running as a tree, as one running in
// the interpreter is stopped.
func TestTreeTierInterrupted(t *testing.T) {
	defer vm.SetTreeTier(true)
	vm.SetTreeTier(true)
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
	defer vm.SetTreeTier(true)
	vm.SetTreeTier(true)
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

// TestPureGlobalReads pins a global read in a body the frameless evaluator
// runs, through the slot its site remembers, as the slot stops being the
// name's: deleted, redefined as an accessor, shadowed by a script's let.
func TestPureGlobalReads(t *testing.T) {
	defer vm.SetTreeTier(true)
	const want = "2,2,2,6,ReferenceError,8,get,11,11"
	for _, tier := range []bool{false, true} {
		vm.SetTreeTier(tier)
		rt := quickjs.New()
		_, err := rt.Eval(`var r = []; globalThis.G = 1; globalThis.H = 5;
			function rd() { return G + 1 } function rh() { return H + 1 }
			function run(f) { for (var i = 0; i < 3; i++) r.push(f()) }
			run(rd); r.push(rh()); delete globalThis.H;
			try { rh() } catch (e) { r.push(e.constructor.name) }
			globalThis.H = 7; r.push(rh());
			Object.defineProperty(globalThis, "H", { get() { r.push("get"); return 0 }, configurable: true }); rh();`)
		if err != nil {
			t.Fatal(err)
		}
		v, err := rt.Eval(`let G = 10; r.push(rd(), rd()); r.join()`)
		if err != nil {
			t.Fatal(err)
		}
		if got := v.String(); got != want {
			t.Errorf("tree tier %v:\n got %s\nwant %s", tier, got, want)
		}
		rt.Close()
	}
}
