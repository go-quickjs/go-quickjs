package quickjs_test

import "testing"

// TestLocalUpdates pins ++ and -- on a local whose value is read, which is
// one instruction: the old value coerced for the postfix forms and the new
// one for the prefix, a string made a number, a BigInt stepped as one, a
// valueOf that reassigns the local overwritten by the update, and a closure
// seeing what the update wrote. The answers are Node's.
func TestLocalUpdates(t *testing.T) {
	checkEval(t, `(function () {
		let i = 5; const a = [i++, i, ++i, i, i--, --i, i];
		let s = "5"; const b = [s++, typeof s, s, ++s];
		let big = 1n; const c = [String(big++), String(big), String(--big)];
		let o = { valueOf() { o = 100; return 7; } }; const d = [o++, o];
		let k = 0; const f = () => k; const e = [k++, f(), ++k, f()];
		let u; const g = [u++, u];
		let n = null; const h = [n--, n];
		var arr = [10, 20, 30], j = 0; const m = [arr[j++], arr[j++], j];
		return JSON.stringify([a, b, c, d, e, g, h, m]);
	})()`, `[[5,6,7,7,7,5,5],[5,"number",6,7],["1","2","1"],[7,8],[0,1,2,2],[null,null],[0,-1],[10,20,2]]`)
}

// TestOperatorCoercionAndImmediates pins that a bitwise or shift operator
// coerces each operand once -- valueOf ran twice on an object -- and that an
// operator whose right operand is a small integer, one instruction, answers
// as the operator does for strings, null, undefined, objects and BigInts.
// The answers are Node's.
func TestOperatorCoercionAndImmediates(t *testing.T) {
	checkEval(t, `(function () {
		var calls = [];
		function mk(name, v) { return { valueOf() { calls.push(name); return v; } }; }
		var a = mk("a", 6), b = mk("b", 3);
		var r = [a & b, a | 1, a << b, a >> 1, a >>> b, a ^ b, a - b, a * 2, a + b];
		return JSON.stringify(r) + " " + calls.join("");
	})()`, `[2,7,48,3,0,5,3,12,9] abaabaabababaab`)
	checkEval(t, `(function () {
		var s = "5", n = null, u, o = { valueOf() { return 12; } }, neg = -1, f = 3.7, huge = 2 ** 40 + 3;
		var r = [s + 1, s - 1, s * 2, n + 1, u + 1, o + 1, o & 4, o >> 1, neg >>> 0, neg >> 31, f | 0, f & 1,
			huge | 0, huge >>> 1, 1 << 31, (1 << 31) >> 31, 7 >> -1, 7 << 33, -8 >>> 1, 0.5 + 1, NaN | 0, Infinity & 1];
		var errs = [];
		for (var e of [() => 5n & 1, () => 5n + 1, () => 5n >> 1]) {
			try { e(); errs.push("none"); } catch (x) { errs.push(x.constructor.name); }
		}
		return JSON.stringify([r, errs]);
	})()`, `[["51",4,10,1,null,13,4,6,4294967295,-1,3,1,3,1,-2147483648,-1,0,14,2147483644,1.5,0,0],["TypeError","TypeError","TypeError"]]`)
}

// TestIndexedStores pins the stores the dense-element fast path must leave
// to [[Set]]: a hole, which a setter up the prototype chain is asked about,
// a frozen array, an element made non-writable, in sloppy and strict code,
// and a mapped arguments object, whose indices are its parameters. The
// answers are Node's.
func TestIndexedStores(t *testing.T) {
	checkEval(t, `(function () {
		var log = [];
		Object.defineProperty(Array.prototype, 3, { set(v) { log.push("proto set " + v); }, configurable: true });
		var a = [1, 2, 3, , 5]; a[3] = 9; a[0] = 7;
		delete Array.prototype[3];
		var b = [1, 2]; Object.freeze(b); b[0] = 5;
		var c = [1, 2]; Object.defineProperty(c, 0, { writable: false }); c[0] = 5; c[1] = 6;
		var d = (function () { "use strict"; var x = [1, 2]; Object.defineProperty(x, 1, { writable: false }); try { x[1] = 3; return "no"; } catch (e) { return e.constructor.name; } })();
		function m(p) { arguments[0] = 42; return p; }
		function um(p) { "use strict"; arguments[0] = 42; return p; }
		var e = [m(1), um(1)];
		var f = [0, 1, 2]; f[1.5] = "x"; f["1"] = "y"; f[-0] = "z";
		return JSON.stringify([a, log, b, c, d, e, f, Object.keys(f)]);
	})()`, `[[7,2,3,null,5],["proto set 9"],[1,2],[1,6],"TypeError",[42,1],["z","y",2],["0","1","2","1.5"]]`)
}

func TestIndexedReadsOfLocals(t *testing.T) {
	// a[i] with both a local is one instruction; what it reads is what the
	// separate ones read, down to the error and where it points.
	checkEval(t, `(function () {
		Object.defineProperty(Array.prototype, 1, { get() { return "proto"; }, configurable: true });
		var a = [0, , 2], i = 1, s = "abc", o = { 1: "one" }, k = "length";
		var out = [a[i], s[i], o[i], a[k], s[k]];
		delete Array.prototype[1];
		out.push(a[i]);
		var j = 1.5; out.push(a[j]); j = -0; out.push(a[j]); j = 7; out.push(a[j]);
		function m(p) { var args = arguments, z = 0; p = "changed"; return args[z]; }
		out.push(m("orig"));
		var n = null;
		try { n[i]; } catch (e) { out.push(e.constructor.name, e.stack.split("\n")[1].trim()); }
		return JSON.stringify(out);
	})()`, `["proto","b","one",3,3,null,null,0,null,"changed","TypeError","at <eval>:11:11"]`)
}

func TestInstanceofWithoutTheMethodCall(t *testing.T) {
	// The default Symbol.hasInstance is answered without calling it where
	// nothing a script could see would run; everywhere else it is called,
	// and what it does is observed as it was.
	checkEval(t, `(function () {
		var out = [];
		class A {} class B extends A {}
		function F() {} function G() {}
		out.push(new B() instanceof A, new A() instanceof B, 5 instanceof F, Object.create(G.prototype) instanceof G);
		var log = [];
		var p = new Proxy({}, { getPrototypeOf() { log.push(new Error().stack.split("\n")[2].trim()); return F.prototype; } });
		out.push(Object.create(p) instanceof F, p instanceof F, log.length, log[0]);
		var bound = F.bind(null); out.push(new F() instanceof bound);
		F.prototype = 5;
		try { ({}) instanceof F; } catch (e) { out.push(e.constructor.name); }
		try { ({}) instanceof Math.max; } catch (e) { out.push(e.constructor.name); }
		var H = function () {}; Object.defineProperty(H, Symbol.hasInstance, { value: function (v) { return v === 1; } });
		out.push(1 instanceof H, 2 instanceof H);
		return JSON.stringify(out);
	})()`, `[true,false,false,true,true,true,2,"at F.[Symbol.hasInstance] (<anonymous>)",true,"TypeError","TypeError",true,false]`)
}

func TestLookupsOfObjectsWithManyProperties(t *testing.T) {
	// An object with many properties caches its lookups, misses included;
	// what a lookup finds is what the object has at the time.
	checkEval(t, `(function () {
		var out = [];
		var o = {}; for (var i = 0; i < 20; i++) o["k" + i] = i;
		var P = Object.create(o), c = Object.create(P);
		out.push(c.missing, "missing" in c);
		o.missing = "added"; out.push(c.missing, "missing" in c);
		delete o.missing; out.push(c.missing);
		o.missing = "again"; out.push(c.missing);
		delete o.k3; out.push(o.k3, c.k3); o.k3 = "new"; out.push(o.k3, Object.keys(o).pop());
		Object.defineProperty(o, "k5", { get() { return "getter"; } }); out.push(c.k5);
		var log = []; Object.defineProperty(o, "s", { set(v) { log.push(v); }, configurable: true });
		c.s = 1; out.push(log.join(), Object.hasOwn(c, "s"));
		delete o.s; c.s = 2; out.push(log.join(), c.s, Object.hasOwn(c, "s"));
		return JSON.stringify(out);
	})()`, `[null,false,"added",true,null,"again",null,null,"new","k3","getter","1",false,"1",2,true]`)
}

func TestGlobalsReadAndWrittenInPlace(t *testing.T) {
	// A global written by assignment is written in place only when it is a
	// plain writable data property; anything else is written the long way.
	checkEval(t, `
		var g1 = 1; g1 = 2;
		globalThis.g2 = 1; g2 = 3; delete globalThis.g2; g2 = 4;
		var g3 = 1; Object.defineProperty(globalThis, "g4", { value: 1, writable: false, configurable: true });
		g4 = 5;
		var setterLog = []; Object.defineProperty(globalThis, "g5", { get() { return "got"; }, set(v) { setterLog.push(v); }, configurable: true });
		g5 = 6;
		var strictErr = (function () { "use strict"; try { g4 = 7; return "no"; } catch (e) { return e.constructor.name; } })();
		for (var k = 0; k < 20; k++) globalThis["filler" + k] = k;
		var sum = 0; for (var j = 0; j < 5; j++) sum += g1;
		JSON.stringify([g1, g2, g3, g4, g5, setterLog, strictErr, sum, Object.getOwnPropertyDescriptor(globalThis, "g2").configurable]);
	`, `[2,4,1,1,"got",[6],"TypeError",10,true]`)
}

func TestGlobalUpdatesForEffect(t *testing.T) {
	// A global updated where its value is not wanted is stored without the
	// copy the value would have needed.
	checkEval(t, `
		var gu = 0, calls = 0, go1 = { valueOf() { calls++; return 5; } };
		for (var gk = 0; gk < 3; gk++) gu += 2;
		(function () { for (gv = 0; gv < 4; gv++) gu--; gw = go1; gw++; gx = 1; gx *= 3; })();
		var err = (function () { "use strict"; try { undeclaredGlobal++; } catch (e) { return e.constructor.name; } })();
		JSON.stringify([gu, gk, gv, gw, calls, gx, err]);
	`, `[2,3,4,6,1,3,"ReferenceError"]`)
}

func TestCreatingPropertiesByAssignment(t *testing.T) {
	// A property created by assignment is added directly only where nothing
	// up the chain could have taken the assignment instead.
	checkEval(t, `(function () {
		"use strict";
		var out = [], log = [];
		function P(v) { this.a = v; this.b = v; this[Symbol.iterator] = v; }
		out.push(Object.keys(new P(1)).join());
		Object.defineProperty(Object.prototype, "b", { set(v) { log.push("set " + v); }, configurable: true });
		var p = new P(2); out.push(Object.keys(p).join(), log.join());
		delete Object.prototype.b;
		Object.defineProperty(P.prototype, "a", { value: 0, writable: false, configurable: true });
		try { new P(3); } catch (e) { out.push(e.constructor.name); }
		delete P.prototype.a;
		var viaProxy = Object.create(new Proxy({}, { set(t, k, v, r) { log.push("trap " + String(k)); return true; } }));
		viaProxy.z = 1; out.push(Object.hasOwn(viaProxy, "z"), log.at(-1));
		var viaArray = Object.create([]); viaArray.length = 5; out.push(Object.hasOwn(viaArray, "length"));
		var sealed = Object.preventExtensions({});
		try { sealed.q = 1; } catch (e) { out.push(e.constructor.name); }
		return JSON.stringify(out);
	})()`, `["a,b","a","set 2","TypeError",false,"trap z",true,"TypeError"]`)
}

func TestIndexedReadsWithAnUpdatedKey(t *testing.T) {
	// a[++i] and a[i++] with both locals are one instruction; the object is
	// read before the key is updated, and every way the update or the read
	// can go is what it was.
	checkEval(t, `(function () {
		var out = [];
		var a = [10, 20, 30], i = 0, j = 2;
		out.push(a[++i], a[i++], i, a[j--], a[--j], j);
		var b = [1, 2], k = { valueOf() { b = ["replaced", "x"]; return 0; } };
		out.push(b[k++], k, b[0]);
		Object.defineProperty(Array.prototype, 1, { get() { return "proto"; }, configurable: true });
		var h = [0, , 2], m = 0; var hv = h[++m]; delete Array.prototype[1]; out.push(hv);
		var s = "xyz", n = 0; out.push(s[++n], s[n++], n);
		var big = [5, 6], bk = 0n; out.push(big[bk++], String(bk));
		var o = { "1": "one" }, ok = 0; out.push(o[++ok]);
		var none = null, z = 0;
		try { none[++z]; } catch (e) { out.push(e.constructor.name, z, e.stack.split("\n")[1].trim()); }
		return JSON.stringify(out);
	})()`, `[20,20,2,30,10,0,1,1,"replaced","proto","y","y",2,5,"1","one","TypeError",1,"at <eval>:13:14"]`)
}

func TestApplyingArgumentsWithoutTheObject(t *testing.T) {
	// A function that only passes its arguments on with apply makes no
	// arguments object for the built-in apply; what the callee receives, and
	// what any other apply is given, is what the object would have been.
	checkEval(t, `(function () {
		var out = [];
		function list() { return Array.prototype.join.call(arguments, ","); }
		function pass(a, b) { return list.apply(this, arguments); }
		function reassign(a, b) { a = "A"; return list.apply(this, arguments); }
		function strictReassign(a, b) { "use strict"; a = "A"; return list.apply(this, arguments); }
		out.push(pass(1, 2, 3), pass(), reassign(1, 2, 3), reassign(), strictReassign(1, 2));
		function viaOwn(a) {
			var f = { apply: function (t, args) { return args; } };
			var first = f.apply(null, arguments);
			a = "changed";
			var second = f.apply(null, arguments);
			return [first === second, first[0], first.length, typeof first.callee, Object.prototype.toString.call(first)];
		}
		out.push(viaOwn("x", "y"));
		function notCallable() { return ({}).apply ? 0 : Function.prototype.apply.call(5, null, []); }
		function badTarget() { var o = { apply: Function.prototype.apply }; return o.apply(null, arguments); }
		try { badTarget(1); } catch (e) { out.push(e.constructor.name); }
		function withArrow(a) { var g = () => arguments[0]; return [g(), list.apply(null, arguments)]; }
		out.push(withArrow(7, 8));
		class K {}
		function ctor() { return K.apply(null, arguments); }
		try { ctor(); } catch (e) { out.push(e.constructor.name); }
		var saved = Function.prototype.apply;
		Function.prototype.apply = function (t, args) { return "replaced:" + args.length; };
		out.push(pass(1, 2));
		Function.prototype.apply = saved;
		return JSON.stringify(out);
	})()`, `["1,2,3","","A,2,3","","1,2",[true,"changed",2,"function","[object Arguments]"],"TypeError",[7,"7,8"],"TypeError","replaced:2"]`)
}

func TestOperatorsWithALocalOnTheRight(t *testing.T) {
	// `expr op local` is one instruction for + - *; the left operand is still
	// coerced before the right, and anything but two numbers is the operator's.
	checkEval(t, `(function () {
		var log = [], s = "x", n = 3, big = 2n;
		var l = { valueOf() { log.push("l"); return 10; } }, r = { valueOf() { log.push("r"); return 4; } };
		var out = [(1 + 1) + s, (2 * 2) - n, (log.length >= 0 ? l : 0) * r, (5n * 5n) + big, [1] + n];
		try { (1 + 1) * big; } catch (e) { out.push(e.constructor.name); }
		out.push(log.join(""));
		return JSON.stringify(out, (k, v) => typeof v === "bigint" ? String(v) + "n" : v);
	})()`, `["2x",1,40,"27n","13","TypeError","lr"]`)
}

func TestLocalOperatorImmediate(t *testing.T) {
	// `local op integer` is one instruction; anything but a number in the
	// local runs the operator itself, and an error points where it did.
	checkEval(t, `(function () {
		var calls = 0, o = { valueOf() { calls++; return 6; } }, s = "a", neg = -5, big = 3n, f = 2.5;
		var out = [o + 1, o & 3, s + 1, neg * 0, neg >>> 28, neg >> 1, f | 0, f << 2, calls];
		try { big + 1; } catch (e) { out.push(e.constructor.name, e.stack.split("\n")[1].trim()); }
		return JSON.stringify(out.map(v => Object.is(v, -0) ? "-0" : v));
	})()`, `[7,2,"a1","-0",15,-3,2,8,2,"TypeError","at <eval>:4:9"]`)
}
