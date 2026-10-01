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
