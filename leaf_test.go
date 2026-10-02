package quickjs_test

import (
	"testing"

	quickjs "github.com/go-quickjs/go-quickjs"
)

// TestLeafFunctions covers the calls the VM answers without a frame: a body
// that only returns this.k, this.k.length or this.k[p], or only stores its
// parameters in this. Each script calls the function enough times for its
// sites' caches to fill, so that the frameless path is taken, and then
// changes what it would have to see -- an accessor, a hole, a proxy, a setter
// or a read-only property up the chain, a frozen or non-extensible object, a
// missing argument, an error whose stack must show the frame -- which the
// function's body proper has to answer instead. Each answer is Node's.
func TestLeafFunctions(t *testing.T) {
	tests := []struct{ src, want string }{
		{`function C(){ this.x = 1 } C.prototype.get = function () { return this.x };
			var o = new C, s = 0; for (var i = 0; i < 5; i++) s += o.get();
			var p = Object.create({ get x() { return 40 } }); for (var i = 0; i < 5; i++) s += C.prototype.get.call(p); s`,
			"205"},
		{`var P = { get x() { return 7 } }; function g() { return this.x }; var a = Object.create(P), r = [];
			for (var i = 0; i < 4; i++) r.push(g.call(a)); r.join()`,
			"7,7,7,7"},
		{`function L() { return this.v.length } var r = [];
			for (const v of [[1,2,3], "abcd", {length: 9}, new Uint8Array(5), "", []]) r.push(L.call({v})); r.join()`,
			"3,4,9,5,0,0"},
		{`function I(k) { return this.e[k] } Array.prototype[1] = "proto"; var o = {e: [10,,30]}, r = [];
			for (const k of [0, 1, 2, 3, -1, 0.5, "2", 1]) r.push(I.call(o, k)); delete Array.prototype[1]; r.join()`,
			"10,proto,30,,,,30,proto"},
		{`function I(k) { return this.e[k] } function A() { return arguments } var o = {e: A(5, 6)}; I.call(o, 0) + "," + I.call(o, 1) + "," + I.call(o)`,
			"5,6,undefined"},
		{`function g() { return this.x } var px = new Proxy({x: 1}, { get(t, k) { return "trap:" + String(k) } }); g.call({x: 2}) + " " + g.call(px) + " " + g.call(px)`,
			"2 trap:x trap:x"},
		{`function S(a, b) { this.a = a; this.b = b } var log = [];
			for (var i = 0; i < 4; i++) new S(i, i);
			Object.defineProperty(S.prototype, "b", { set(v) { log.push("set " + v) }, configurable: true });
			var s = new S(1, 2); log.join() + " " + Object.keys(s).join()`,
			"set 2 a"},
		{`function S(a) { this.q = a } for (var i = 0; i < 4; i++) new S(i);
			Object.defineProperty(Object.prototype, "q", { set(v) { this.seen = v }, configurable: true });
			var s = new S(5); delete Object.prototype.q; JSON.stringify(s)`,
			"{\"seen\":5}"},
		{`function S(a) { this.r = a } for (var i = 0; i < 4; i++) new S(i);
			Object.defineProperty(S.prototype, "r", { value: 0, writable: false });
			var s = new S(5); Object.keys(s).length + " " + s.r`,
			"0 0"},
		{`"use strict"; function S(a) { this.r = a } for (var i = 0; i < 4; i++) new S(i);
			Object.defineProperty(S.prototype, "r", { value: 0, writable: false });
			try { new S(5); "no error" } catch (e) { e.constructor.name }`,
			"TypeError"},
		{`function init(a, b) { this.a = a; this.b = b } var o = {}; for (var i = 0; i < 4; i++) init.call(o, i, i * 2);
			Object.freeze(o); init.call(o, 9, 9); o.a + "," + o.b`,
			"3,6"},
		{`"use strict"; function init(a) { this.a = a } var o = {}; for (var i = 0; i < 4; i++) init.call(o, i);
			Object.freeze(o); try { init.call(o, 9); "no error" } catch (e) { e.constructor.name + " " + o.a }`,
			"TypeError 3"},
		{`function init(a) { this.n = a } var objs = [{}, {}, {}, {}]; objs.forEach((o, i) => init.call(o, i));
			var x = Object.preventExtensions({}); init.call(x, 1); "n" in x`,
			"false"},
		{`function S(a, b) { this.a = a; this.b = b } for (var i = 0; i < 4; i++) new S(1, 2);
			var s = new S(7); JSON.stringify(s) + " " + ("b" in s)`,
			"{\"a\":7} true"},
		{`function G(a, b) { this.x = a; this.x = b } for (var i = 0; i < 4; i++) new G(1, 2); var g = new G(3, 4); JSON.stringify(g)`,
			"{\"x\":4}"},
		{`class A { z = 1; constructor(a) { this.a = a } } for (var i = 0; i < 4; i++) new A(i); Object.keys(new A(5)).join() + " " + new A(6).a`,
			"z,a 6"},
		{`class B { constructor(a, b) { this.a = a; this.b = b } } class D extends B { constructor() { super(1, 2); this.c = 3 } }
			for (var i = 0; i < 4; i++) new D; JSON.stringify(new D)`,
			"{\"a\":1,\"b\":2,\"c\":3}"},
		{`function S(a) { this.a = a } var r = []; for (var i = 0; i < 4; i++) r.push(new S(i).a); r.push(typeof S(1), globalThis.a); r.join()`,
			"0,1,2,3,undefined,1"},
		{`function L() { return this.v.length } for (var i = 0; i < 4; i++) L.call({v: [1]});
			try { L.call({v: null}) } catch (e) { e.stack.split(String.fromCharCode(10))[1].trim().split(" (")[0] }`,
			"at Object.L"},
		{`class C { constructor(){ this.e = [1, 2] } get size() { return this.e.length } } var c = new C, n = 0;
			for (var i = 0; i < 4; i++) n += c.size; n`,
			"8"},
	}
	for _, tt := range tests {
		checkEval(t, tt.src, tt.want)
	}
}

// TestLeafForward covers constructors whose body is only
// this.k.apply(this, arguments), which the VM constructs with by calling the
// method itself, with the constructor's frame and apply's standing in for
// the body. Each script warms the constructor and then looks at what the
// frames are seen by -- a stack trace's names and positions, f.caller and
// f.arguments under Node quirks, a trace taken after an error, the depth at
// which recursion fails -- or makes the shortcut fall back: a getter for the
// method, a method gone, Function.prototype.apply replaced, a method that is
// an object with an apply of its own. The answers are the ones the
// constructor's body gave when it was run.
func TestLeafForward(t *testing.T) {
	tests := []struct{ src, want string }{
		{`var Class = { create: function () { return function () { this.initialize.apply(this, arguments) } } };
   var V = Class.create(); V.prototype = { initialize: function (x, y) { this.x = x; this.y = y; return {other: 1} } };
   for (var i = 0; i < 5; i++) var v = new V(i, i + 1); JSON.stringify(v) + " " + (v instanceof V)`,
			"{\"x\":4,\"y\":5} true"},
		{`var Class = { create: function () { return function () { this.initialize.apply(this, arguments) } } };
   var T = Class.create(); T.prototype = { initialize: function (x) { if (x) throw new Error("boom") } };
   for (var i = 0; i < 5; i++) new T(0);
   function at(e) { return e.stack.split(String.fromCharCode(10)).slice(1, 3).map(function (s) { s = s.trim(); return s.split(" (")[0] + " " + s.match(/:([0-9]+:[0-9]+)[)]?$/)[1] }).join(" | ") }
   var r = []; try { new T(1) } catch (e) { r.push(at(e)) } try { null.x } catch (e) { r.push(e.stack.split(String.fromCharCode(10)).length) } r.join(" ; ")`,
			"at Object.initialize 2:84 | at new <anonymous> 1:74 ; 2"},
		{`var Class = { create: function () { return function () { this.initialize.apply(this, arguments) } } };
   var C = Class.create(), seen = []; C.prototype = { initialize: function (a, b) {
     seen.push(C.prototype.initialize.caller === C, Array.prototype.join.call(C.arguments)) } };
   for (var i = 0; i < 4; i++) new C(i, "b"); seen.join()`,
			"true,0,b,true,1,b,true,2,b,true,3,b"},
		{`var Class = { create: function () { return function () { this.initialize.apply(this, arguments) } } };
   var calls = 0, G = Class.create();
   Object.defineProperty(G.prototype, "initialize", { get: function () { calls++; return function (x) { this.x = x } } });
   for (var i = 0; i < 4; i++) var g = new G(i); calls + " " + g.x`,
			"4 3"},
		{`var Class = { create: function () { return function () { this.initialize.apply(this, arguments) } } };
   var M = Class.create(); M.prototype = { initialize: function () {} }; for (var i = 0; i < 4; i++) new M;
   M.prototype = {}; try { new M } catch (e) { e.constructor.name }`,
			"TypeError"},
		{`var Class = { create: function () { return function () { this.initialize.apply(this, arguments) } } };
   var A = Class.create(); A.prototype = { initialize: function (x) { this.x = x } }; for (var i = 0; i < 4; i++) new A(1);
   var real = Function.prototype.apply; Function.prototype.apply = function (t) { return real.call(this, t, [99]) };
   var x = new A(1).x; Function.prototype.apply = real; x`,
			"99"},
		{`var Class = { create: function () { return function () { this.initialize.apply(this, arguments) } } };
   var N = Class.create(); N.prototype = { initialize: { apply: function (t, a) { t.via = a.length } } };
   for (var i = 0; i < 3; i++) var n = new N(1, 2); n.via`,
			"2"},
		{`var Class = { create: function () { return function () { this.initialize.apply(this, arguments) } } };
   var R = Class.create(), depth = 0; R.prototype = { initialize: function () { depth++; new R } };
   try { new R } catch (e) { e.constructor.name + " " + (depth > 100) }`,
			"RangeError true"},
		{`var Class = { create: function () { return function () { this.initialize.apply(this, arguments) } } };
   var NT = Class.create(); NT.prototype = { initialize: function () { this.nt = typeof new.target; this.self = this instanceof NT } };
   for (var i = 0; i < 3; i++) var nt = new NT; nt.nt + " " + nt.self`,
			"undefined true"},
		{`function W(a) { this.init.apply(this, arguments) } W.prototype.init = function (a, b) { this.s = a + b };
   for (var i = 0; i < 4; i++) var w = new W(1, 2); w.s`,
			"3"},
	}
	for _, tt := range tests {
		rt := quickjs.New(quickjs.WithNodeQuirks())
		if got := evalString(t, rt, tt.src); got != tt.want {
			t.Errorf("%s\n got: %q\nwant: %q", tt.src, got, tt.want)
		}
		rt.Close()
	}
}

// TestPureLeaves covers bodies that only read and compute, which a call
// evaluates without a frame where every read and operator takes its fast
// path, and otherwise makes as usual: a getter or a proxy for a property,
// an object, a string or a boolean where an operator wants numbers, a
// global in its dead zone, undeclared or an accessor, a primitive this for
// a sloppy function, an exception and its stack. Each answer is Node's.
func TestPureLeaves(t *testing.T) {
	tests := []struct{ src, want string }{
		{`var FWD = 1; function input() { return this.dir == FWD ? this.a : this.b }
			var r = []; for (var i = 0; i < 4; i++) r.push(input.call({ dir: i % 2, a: "A", b: "B" })); r.join()`,
			"B,A,B,A"},
		{`var HELD = 4, SUSP = 2; function held() { return (this.state & HELD) != 0 || this.state == SUSP }
			var r = []; for (const s of [0, 2, 4, 6, 1]) r.push(held.call({ state: s })); r.join()`,
			"false,true,true,true,false"},
		{`function dot(w) { return this.x * w.x + this.y * w.y } var v = { x: 1, y: 2 }, r = [];
			for (var i = 0; i < 3; i++) r.push(dot.call(v, { x: i, y: 10 })); r.join()`,
			"20,21,22"},
		{`function isNum(n) { return typeof n === "number" } [1, "1", null, undefined, 1n, {}].map(isNum).join()`,
			"true,false,false,false,false,false"},
		{`function len(s) { return s.length + 1 } [len("abc"), len([1, 2]), len({ length: 7 })].join()`,
			"4,3,8"},
		{`function g() { return this.x } var log = [], o = { get x() { log.push("get"); return 5 } };
			var r = []; for (var i = 0; i < 3; i++) r.push(g.call(o)); r.join() + " " + log.join()`,
			"5,5,5 get,get,get"},
		{`function g() { return this.x } var p = new Proxy({}, { get(t, k) { return "trap:" + String(k) } }); [g.call(p), g.call({ x: 1 })].join()`,
			"trap:x,1"},
		{`function add(a, b) { return a + b } var o = { valueOf() { return 41 } }; [add(1, 2), add("a", 1), add(o, 1), add(1n, 2n)].join()`,
			"3,a1,42,3"},
		{`function lt(a, b) { return a < b } [lt(1, 2), lt("b", "a"), lt("10", "9"), lt({ valueOf() { return 1 } }, 2)].join()`,
			"true,false,true,true"},
		{`function eq(a, b) { return a == b } [eq(1, "1"), eq(null, undefined), eq(0, ""), eq({ valueOf() { return 1 } }, 1), eq(1n, 1), eq("x", "x")].join()`,
			"true,true,true,true,true,true"},
		{`function sloppy() { return typeof this } function strict() { "use strict"; return typeof this }
			[sloppy.call(1), sloppy.call(undefined), strict.call(1), strict.call(undefined)].join()`,
			"object,object,number,undefined"},
		{`function readTdz() { return later } var r; try { readTdz() } catch (e) { r = e.constructor.name } let later = 3; r + " " + readTdz()`,
			"ReferenceError 3"},
		{`function readUndeclared() { return nope + 1 } try { readUndeclared() } catch (e) { e.constructor.name + ": " + e.message }`,
			"ReferenceError: nope is not defined"},
		{`Object.defineProperty(globalThis, "acc", { get() { return "accessor" }, configurable: true }); function readAcc() { return acc } [readAcc(), readAcc()].join()`,
			"accessor,accessor"},
		{`function boom(o) { return o.a.b } try { boom({}) } catch (e) { e.constructor.name + "|" + e.stack.split(String.fromCharCode(10))[1].trim().split(" (")[0] }`,
			"TypeError|at boom"},
		{`function bits(x) { return (x << 3) ^ (x >>> 1) | ~~-x } [1, -5, 2147483647, 3.7].map(bits).join()`,
			"-1,-2147483611,-1073741817,-3"},
		{`function pick(a, b) { return a && b || "none" } [pick(1, 2), pick(0, 2), pick(1, 0), pick("", "")].join()`,
			"2,none,none,none"},
		{`function neg(a) { return -a + !a } [neg(3), neg(0), neg("4")].join()`,
			"-3,1,-4"},
		{`var o = { m(x) { return this.k * x } , k: 3 }; var r = 0; for (var i = 0; i < 100; i++) r += o.m(i); r`,
			"14850"},
	}
	for _, tt := range tests {
		checkEval(t, tt.src, tt.want)
	}
}

// TestPureLeafCalls covers calls inside bodies that only read and compute,
// made without a frame where the callee is such a body too, a body that
// reads one property of this, or a unary Math function given a number: a
// chain of delegating methods, a callee that stores, a Math function given
// an object, recursion past the depth evaluated without frames, a native
// method, a getter for the method, an operand that is an object, and an
// exception from a callee with its stack. Each answer is Node's.
func TestPureLeafCalls(t *testing.T) {
	tests := []struct{ src, want string }{
		{`function Coll() { this.elms = [1, 2, 3] } Coll.prototype.size = function () { return this.elms.length };
			Coll.prototype.at = function (i) { return this.elms[i] };
			function Plan() { this.v = new Coll() } Plan.prototype.size = function () { return this.v.size() };
			Plan.prototype.at = function (i) { return this.v.at(i) }; Plan.prototype.last = function () { return this.at(this.size() - 1) };
			var p = new Plan(), r = []; for (var i = 0; i < 4; i++) r.push(p.size(), p.at(i), p.last()); r.join()`,
			"3,1,3,3,2,3,3,3,3,3,,3"},
		{`var log = []; var o = { n: 0, bump() { this.n++; log.push(this.n); return this.n }, twice() { return this.bump() + this.bump() } };
			var r = []; for (var i = 0; i < 3; i++) r.push(o.twice()); r.join() + " " + log.join()`,
			"3,7,11 1,2,3,4,5,6"},
		{`function mag(v) { return Math.sqrt(v.x * v.x + v.y * v.y) } var r = []; for (var i = 0; i < 3; i++) r.push(mag({ x: 3 * i, y: 4 * i }));
			r.push(Math.sqrt({ valueOf() { return 9 } })); function sq(v) { return Math.sqrt(v) } r.push(sq({ valueOf() { r.push("v"); return 16 } })); r.join()`,
			"0,5,10,3,v,4"},
		{`function down(n) { return n == 0 ? 0 : 1 + down(n - 1) } [down(3), down(20), down(100)].join()`,
			"3,20,100"},
		{`function cap(s) { return s.toUpperCase() } [cap("ab"), cap("x")].join()`,
			"AB,X"},
		{`var o = { get m() { log.push("getm"); return function () { return 1 } }, call() { return this.m() } }, log = [];
			[o.call(), o.call()].join() + " " + log.join()`,
			"1,1 getm,getm"},
		{`function add(a, b) { return a + b } function sum3(a, b, c) { return add(add(a, b), c) } var o = { valueOf() { return 10 } };
			[sum3(1, 2, 3), sum3(1, o, 2), sum3("a", 1, 2)].join()`,
			"6,13,a12"},
		{`function thrower() { return null.x } function calls() { return thrower() + 1 }
			try { calls() } catch (e) { e.constructor.name + "|" + e.stack.split(String.fromCharCode(10)).slice(1, 3).map(s => s.trim().split(" (")[0]).join("|") }`,
			"TypeError|at thrower|at calls"},
	}
	for _, tt := range tests {
		checkEval(t, tt.src, tt.want)
	}
}

// TestPureLeafStores checks the bodies LeafPure marks that store: once
// and last, where nothing after the store can give the frameless evaluation
// up, so that a store is never made twice or seen half done.
func TestPureLeafStores(t *testing.T) {
	tests := []struct{ src, want string }{
		{`var HELD = 4; function T(s) { this.state = s } T.prototype.mark = function () { this.state = this.state | HELD };
			var a = new T(1), b = { other: 1, state: 2 }; for (var i = 0; i < 100; i++) { a.mark(); T.prototype.mark.call(b); a.state++ }
			a.state + "," + b.state`,
			"205,6"},
		{`function In(v) { this.value = v } function C(a, b) { this.a = a; this.b = b }
			C.prototype.input = function () { return this.a }; C.prototype.output = function () { return this.b };
			C.prototype.execute = function () { this.output().value = this.input().value };
			var c = new C(new In(1), new In(0)), r = []; for (var i = 0; i < 100; i++) { c.a.value = i; c.execute(); r.push(c.b.value) } r.slice(-3).join()`,
			"97,98,99"},
		{`var log = [], P = { set x(v) { log.push(v) } }; function sx(v) { this.x = v + 1 }
			for (var i = 0; i < 70; i++) sx.call(i % 2 ? Object.create(P) : {}, i); log.length + "," + log.slice(-2).join()`,
			"35,68,70"},
		{`function add(v) { this.n = v * 2 } var r = []; for (var i = 0; i < 70; i++) { var o = i < 68 ? {} : Object.preventExtensions({}); add.call(o, i); r.push(o.n) }
			r.slice(-4).join()`,
			"132,134,,"},
		{`"use strict"; function put(v) { this.k = v + 0 } var f = Object.freeze({ k: 1 }), r = [];
			for (var i = 0; i < 70; i++) put.call({ k: 0 }, i);
			try { put.call(f, 2) } catch (e) { r.push(e.constructor.name, e.stack.split(String.fromCharCode(10))[1].trim().split(" (")[0]) } r.join()`,
			"TypeError,at Object.put"},
		{`function put(v) { this.k = v + 0 } var f = Object.freeze({ k: 1 }); for (var i = 0; i < 70; i++) put.call(f, i); f.k`,
			"1"},
		// A store a caller could still give up after is not made by the
		// frameless evaluation: the getter g makes f give up, and then f
		// runs as usual, which would bump c a second time.
		{`var o = { c: 0, bump() { this.c = this.c + 1 }, get g() { return this.c }, f() { this.bump(); return this.g },
			t() { return this.bump() } };
			var r = []; for (var i = 0; i < 70; i++) r.push(o.f()); for (var i = 0; i < 70; i++) o.t(); r.slice(-2).join() + "," + o.c`,
			"69,70,140"},
	}
	for _, tt := range tests {
		checkEval(t, tt.src, tt.want)
	}
}
