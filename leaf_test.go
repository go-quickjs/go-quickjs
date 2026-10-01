package quickjs_test

import "testing"

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
