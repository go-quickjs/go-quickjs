package vm

import (
	"strings"
	"testing"
)

// typedCallbackScript calls every callback-taking method of every kind of
// typed array -- forEach, map, filter, find, findIndex, findLast,
// findLastIndex, some, every, reduce and reduceRight -- and records what
// each callback is given and what each method answers: thisArg, boxed for
// a sloppy callback; the arguments a callback keeps, as an arguments
// object, a rest parameter or a closure, its own though the list they
// come in is one for the loop; built-ins, bound functions, proxies and
// classes as the callback; a callback that throws; a buffer detached by
// transfer, and a resizable one shrunk and grown, during the loop; map's
// and filter's species; writes the callback makes. Each method runs on
// each kind three times, so that its callbacks run as trees and as pure
// bodies.
const typedCallbackScript = `(function () {
	var out = [];
	function show(v) {
		if (typeof v === "bigint") return v + "n";
		if (typeof v === "object" && v !== null) {
			if (ArrayBuffer.isView(v)) return v.constructor.name + "[" + Array.prototype.map.call(v, show).join(",") + "]";
			if (Array.isArray(v)) return "[" + v.map(show).join(",") + "]";
			return "{" + Object.prototype.toString.call(v) + " " + String(v) + "}";
		}
		if (Object.is(v, -0)) return "-0";
		return String(v);
	}
	function line(name, v) { out.push(name + ": " + v); }
	function attempt(name, f) {
		try { line(name, show(f())); } catch (e) { line(name, "threw " + (e && e.constructor && e.constructor.name)); }
	}
	var kinds = [Int8Array, Uint8Array, Uint8ClampedArray, Int16Array, Uint16Array, Int32Array, Uint32Array,
		Float16Array, Float32Array, Float64Array, BigInt64Array, BigUint64Array];
	var methods = ["forEach", "map", "filter", "find", "findIndex", "findLast", "findLastIndex", "some", "every"];
	// Every element type, every method, and what each callback is given;
	// each run several times, so that the callbacks are planned and run
	// as trees, and answered as pure bodies.
	for (var round = 0; round < 3; round++) {
		kinds.forEach(function (K) {
			var big = K === BigInt64Array || K === BigUint64Array;
			var src = big ? [-3n, 0n, 7n, 300n] : [-3, 0, 7.5, 300];
			var ta = new K(src);
			methods.forEach(function (m) {
				var seen = [];
				var r = ta[m](function (x, i, a) {
					seen.push(show(x) + "@" + i + (a === ta ? "" : "!"));
					if (m === "map") return big ? x * 2n : x * 2;
					return big ? x > 0n : x > 0;
				});
				if (round === 0) line(K.name + "." + m, show(r) + " " + seen.join(" "));
				var count = 0;
				ta[m]((x) => { count++; return false; });
				var total = big ? 0n : 0;
				ta[m]((x) => { total += x; return m === "map" ? x : false; });
				if (round === 2) line(K.name + "." + m + " pure", count + " " + show(total));
			});
			var red = ta.reduce(function (acc, x, i, a) { return acc + "|" + show(x) + "@" + i + (a === ta ? "" : "!"); }, "");
			var redR = ta.reduceRight(function (acc, x, i, a) { return acc + "|" + show(x) + "@" + i + (a === ta ? "" : "!"); }, "");
			var redN = ta.reduce((a, b) => a + b);
			var redRN = ta.reduceRight((a, b) => a - b);
			if (round === 1) line(K.name + ".reduce", red + " " + redR + " " + show(redN) + " " + show(redRN));
		});
	}
	// thisArg: a sloppy callback sees it boxed, or the global object; a
	// strict one as it is.
	var t3 = new Int8Array([1, 2]);
	methods.concat(["reduce"]).forEach(function (m) {
		var thisSeen = [];
		var sloppy = function () { thisSeen.push(this === globalThis ? "global" : typeof this + ":" + show(this)); };
		var strict = function () { "use strict"; thisSeen.push(typeof this + ":" + show(this)); };
		if (m === "reduce") {
			t3.reduce(sloppy, 0); t3.reduce(strict, 0);
		} else {
			t3[m](sloppy, 5); t3[m](sloppy); t3[m](strict, "s"); t3[m](strict); t3[m](strict, null);
			t3[m](() => { thisSeen.push(this === undefined ? "arrow-undefined" : "arrow-other"); }, 9);
		}
		line("this " + m, thisSeen.join(" "));
	});
	// The arguments a callback is given are its own: an arguments object, a
	// rest parameter and a closure keep what each call was given.
	var t4 = new Uint16Array([10, 20, 30]);
	var kept = [], rests = [], thunks = [];
	t4.forEach(function () { kept.push(arguments); });
	t4.map(function (...a) { rests.push(a); return 0; });
	t4.some(function (x, i) { thunks.push(function () { return x + ":" + i; }); return false; });
	line("arguments kept", kept.map(function (a) { return a.length + ":" + Array.prototype.slice.call(a, 0, 2).join(","); }).join(" "));
	line("rest kept", rests.map(function (a) { return a.length + ":" + a.slice(0, 2).join(","); }).join(" "));
	line("closures kept", thunks.map(function (f) { return f(); }).join(" "));
	var mapped = [];
	t4.forEach(function (x, i) { arguments[0] = 99; mapped.push(x + "/" + arguments[0]); });
	line("arguments mapped", mapped.join(" "));
	// A callback that is a built-in, a bound function, a proxy, a class.
	attempt("native callback", function () { return new Float64Array([-1.5, 2, -0]).map(Math.abs); });
	attempt("native push", function () { var a = []; new Int8Array([4, 5]).forEach(Array.prototype.push, a); return a; });
	attempt("bound callback", function () { var a = []; new Int8Array([4, 5]).forEach(function (p, x, i) { a.push(p + x + i); }.bind(null, 100)); return a; });
	attempt("proxy callback", function () { var a = []; new Int8Array([4, 5]).forEach(new Proxy(function (x) { a.push(x); }, {})); return a; });
	attempt("class callback", function () { new Int8Array([4]).forEach(class {}); });
	attempt("not callable", function () { new Int8Array([4]).forEach({}); });
	attempt("not callable empty", function () { new Int8Array(0).map(null); });
	attempt("reduce empty", function () { new Int8Array(0).reduce((a, b) => a); });
	attempt("reduce empty seeded", function () { return new Int8Array(0).reduceRight((a, b) => a, "seed"); });
	// A callback that throws ends the loop; the error is what it threw.
	methods.concat(["reduce", "reduceRight"]).forEach(function (m) {
		var calls = 0;
		try {
			new Int32Array([1, 2, 3, 4])[m](function () { calls++; if (calls === 2) throw new RangeError("stop " + m); return m === "every"; });
			line("throw " + m, "no throw " + calls);
		} catch (e) {
			line("throw " + m, e.constructor.name + " " + e.message + " after " + calls);
		}
	});
	// Recursion: a callback calling the same method of the same array.
	var t5 = new Int8Array([1, 2, 3]), rec = [];
	t5.forEach(function (x) { t5.forEach(function (y) { rec.push(x * 10 + y); }); });
	line("nested", rec.join(" "));
	// The buffer detached during the loop: the elements left are undefined,
	// and every index is still visited; map's result has its own buffer.
	methods.concat(["reduce", "reduceRight"]).forEach(function (m) {
		var ab = new ArrayBuffer(8), ta = new Int16Array(ab);
		ta.set([1, 2, 3, 4]);
		var seen = [], first = true;
		function cb() {
			var x = m.startsWith("reduce") ? arguments[1] : arguments[0];
			var i = m.startsWith("reduce") ? arguments[2] : arguments[1];
			seen.push(show(x) + "@" + i);
			if (first) { first = false; ab.transfer(); }
			return m === "every" ? true : m === "map" ? 7 : false;
		}
		try {
			var r = m.startsWith("reduce") ? ta[m](cb, 0) : ta[m](cb);
			line("detach " + m, show(r) + " " + seen.join(" ") + " len " + ta.length + " detached " + ab.detached);
		} catch (e) {
			line("detach " + m, "threw " + e.constructor.name + " " + seen.join(" "));
		}
	});
	attempt("detached before", function () { var ab = new ArrayBuffer(4), ta = new Int8Array(ab); ab.transfer(); return ta.forEach(function () {}); });
	attempt("transferred values", function () {
		var ab = new ArrayBuffer(4), ta = new Uint8Array(ab), seen = [];
		ta.set([9, 8, 7, 6]);
		var nb;
		ta.forEach(function (x, i) { if (i === 1) nb = ab.transfer(8); seen.push(x); });
		return seen.concat([new Uint8Array(nb)[0], new Uint8Array(nb).length]);
	});
	// A resizable buffer shrunk and grown during the loop: the length is
	// read once, the elements past the shrunk end are undefined, and those
	// a growth adds past the first length are not visited.
	["track", "fixed"].forEach(function (how) {
		methods.forEach(function (m) {
			var ab = new ArrayBuffer(6, { maxByteLength: 16 });
			var ta = how === "track" ? new Uint8Array(ab) : new Uint8Array(ab, 0, 6);
			ta.set([1, 2, 3, 4, 5, 6]);
			var seen = [];
			try {
				var r = ta[m](function (x, i) {
					seen.push(show(x));
					if (i === 1) ab.resize(3);
					if (i === 3) ab.resize(12);
					return m === "every" ? true : m === "map" ? i : false;
				});
				line("resize " + how + " " + m, show(r) + " " + seen.join(",") + " len " + ta.length);
			} catch (e) {
				line("resize " + how + " " + m, "threw " + e.constructor.name + " " + seen.join(","));
			}
		});
	});
	attempt("out of bounds before", function () {
		var ab = new ArrayBuffer(8, { maxByteLength: 16 }), ta = new Uint8Array(ab, 4, 4);
		ab.resize(2);
		return ta.forEach(function () {});
	});
	// Species: map makes its result with the species before calling the
	// callback; filter after.
	(function () {
		var log = [];
		class MyTA extends Int32Array {
			static get [Symbol.species]() { log.push("species"); return Float64Array; }
		}
		var ta = new MyTA([1, 2, 3]);
		var m = ta.map(function (x) { log.push("cb" + x); return x / 2; });
		var f = ta.filter(function (x) { log.push("f" + x); return x !== 2; });
		line("species", show(m) + " " + show(f) + " " + log.join(" "));
		class Short extends Int8Array {
			static get [Symbol.species]() { return function (n) { return new Int8Array(1); }; }
		}
		attempt("species too short map", function () { return new Short([1, 2]).map(function (x) { return x; }); });
		attempt("species too short filter", function () { return new Short([1, 2]).filter(function () { return true; }); });
		class Refuse extends Int8Array {
			static get [Symbol.species]() { return function () { throw new SyntaxError("no"); }; }
		}
		var called = 0;
		attempt("species refuses", function () { return new Refuse([1, 2]).map(function () { called++; }); });
		line("species refuses calls", called);
		class Big extends Int8Array {
			static get [Symbol.species]() { return BigInt64Array; }
		}
		attempt("species content type", function () { return new Big([1]).map(function (x) { return x; }); });
	})();
	// Writes the callback makes are seen by later elements.
	var t6 = new Float32Array([1, 2, 3]), w = [];
	t6.forEach(function (x, i, a) { if (i + 1 < a.length) a[i + 1] = x * 10; w.push(x); });
	line("writes seen", w.join(" "));
	// A receiver that is not a typed array.
	attempt("generic receiver", function () { return Int8Array.prototype.forEach.call([1, 2], function () {}); });
	attempt("generic receiver map", function () { return Object.getPrototypeOf(Int8Array.prototype).map.call({ length: 1, 0: 1 }, function () {}); });
	return out.join("\n");
})()`

// typedCallbackWant is Node v26.10.0's answer, the script run by
// vm.runInThisContext.
const typedCallbackWant = `Int8Array.forEach: undefined -3@0 0@1 7@2 44@3
Int8Array.map: Int8Array[-6,0,14,88] -3@0 0@1 7@2 44@3
Int8Array.filter: Int8Array[7,44] -3@0 0@1 7@2 44@3
Int8Array.find: 7 -3@0 0@1 7@2
Int8Array.findIndex: 2 -3@0 0@1 7@2
Int8Array.findLast: 44 44@3
Int8Array.findLastIndex: 3 44@3
Int8Array.some: true -3@0 0@1 7@2
Int8Array.every: false -3@0
Uint8Array.forEach: undefined 253@0 0@1 7@2 44@3
Uint8Array.map: Uint8Array[250,0,14,88] 253@0 0@1 7@2 44@3
Uint8Array.filter: Uint8Array[253,7,44] 253@0 0@1 7@2 44@3
Uint8Array.find: 253 253@0
Uint8Array.findIndex: 0 253@0
Uint8Array.findLast: 44 44@3
Uint8Array.findLastIndex: 3 44@3
Uint8Array.some: true 253@0
Uint8Array.every: false 253@0 0@1
Uint8ClampedArray.forEach: undefined 0@0 0@1 8@2 255@3
Uint8ClampedArray.map: Uint8ClampedArray[0,0,16,255] 0@0 0@1 8@2 255@3
Uint8ClampedArray.filter: Uint8ClampedArray[8,255] 0@0 0@1 8@2 255@3
Uint8ClampedArray.find: 8 0@0 0@1 8@2
Uint8ClampedArray.findIndex: 2 0@0 0@1 8@2
Uint8ClampedArray.findLast: 255 255@3
Uint8ClampedArray.findLastIndex: 3 255@3
Uint8ClampedArray.some: true 0@0 0@1 8@2
Uint8ClampedArray.every: false 0@0
Int16Array.forEach: undefined -3@0 0@1 7@2 300@3
Int16Array.map: Int16Array[-6,0,14,600] -3@0 0@1 7@2 300@3
Int16Array.filter: Int16Array[7,300] -3@0 0@1 7@2 300@3
Int16Array.find: 7 -3@0 0@1 7@2
Int16Array.findIndex: 2 -3@0 0@1 7@2
Int16Array.findLast: 300 300@3
Int16Array.findLastIndex: 3 300@3
Int16Array.some: true -3@0 0@1 7@2
Int16Array.every: false -3@0
Uint16Array.forEach: undefined 65533@0 0@1 7@2 300@3
Uint16Array.map: Uint16Array[65530,0,14,600] 65533@0 0@1 7@2 300@3
Uint16Array.filter: Uint16Array[65533,7,300] 65533@0 0@1 7@2 300@3
Uint16Array.find: 65533 65533@0
Uint16Array.findIndex: 0 65533@0
Uint16Array.findLast: 300 300@3
Uint16Array.findLastIndex: 3 300@3
Uint16Array.some: true 65533@0
Uint16Array.every: false 65533@0 0@1
Int32Array.forEach: undefined -3@0 0@1 7@2 300@3
Int32Array.map: Int32Array[-6,0,14,600] -3@0 0@1 7@2 300@3
Int32Array.filter: Int32Array[7,300] -3@0 0@1 7@2 300@3
Int32Array.find: 7 -3@0 0@1 7@2
Int32Array.findIndex: 2 -3@0 0@1 7@2
Int32Array.findLast: 300 300@3
Int32Array.findLastIndex: 3 300@3
Int32Array.some: true -3@0 0@1 7@2
Int32Array.every: false -3@0
Uint32Array.forEach: undefined 4294967293@0 0@1 7@2 300@3
Uint32Array.map: Uint32Array[4294967290,0,14,600] 4294967293@0 0@1 7@2 300@3
Uint32Array.filter: Uint32Array[4294967293,7,300] 4294967293@0 0@1 7@2 300@3
Uint32Array.find: 4294967293 4294967293@0
Uint32Array.findIndex: 0 4294967293@0
Uint32Array.findLast: 300 300@3
Uint32Array.findLastIndex: 3 300@3
Uint32Array.some: true 4294967293@0
Uint32Array.every: false 4294967293@0 0@1
Float16Array.forEach: undefined -3@0 0@1 7.5@2 300@3
Float16Array.map: Float16Array[-6,0,15,600] -3@0 0@1 7.5@2 300@3
Float16Array.filter: Float16Array[7.5,300] -3@0 0@1 7.5@2 300@3
Float16Array.find: 7.5 -3@0 0@1 7.5@2
Float16Array.findIndex: 2 -3@0 0@1 7.5@2
Float16Array.findLast: 300 300@3
Float16Array.findLastIndex: 3 300@3
Float16Array.some: true -3@0 0@1 7.5@2
Float16Array.every: false -3@0
Float32Array.forEach: undefined -3@0 0@1 7.5@2 300@3
Float32Array.map: Float32Array[-6,0,15,600] -3@0 0@1 7.5@2 300@3
Float32Array.filter: Float32Array[7.5,300] -3@0 0@1 7.5@2 300@3
Float32Array.find: 7.5 -3@0 0@1 7.5@2
Float32Array.findIndex: 2 -3@0 0@1 7.5@2
Float32Array.findLast: 300 300@3
Float32Array.findLastIndex: 3 300@3
Float32Array.some: true -3@0 0@1 7.5@2
Float32Array.every: false -3@0
Float64Array.forEach: undefined -3@0 0@1 7.5@2 300@3
Float64Array.map: Float64Array[-6,0,15,600] -3@0 0@1 7.5@2 300@3
Float64Array.filter: Float64Array[7.5,300] -3@0 0@1 7.5@2 300@3
Float64Array.find: 7.5 -3@0 0@1 7.5@2
Float64Array.findIndex: 2 -3@0 0@1 7.5@2
Float64Array.findLast: 300 300@3
Float64Array.findLastIndex: 3 300@3
Float64Array.some: true -3@0 0@1 7.5@2
Float64Array.every: false -3@0
BigInt64Array.forEach: undefined -3n@0 0n@1 7n@2 300n@3
BigInt64Array.map: BigInt64Array[-6n,0n,14n,600n] -3n@0 0n@1 7n@2 300n@3
BigInt64Array.filter: BigInt64Array[7n,300n] -3n@0 0n@1 7n@2 300n@3
BigInt64Array.find: 7n -3n@0 0n@1 7n@2
BigInt64Array.findIndex: 2 -3n@0 0n@1 7n@2
BigInt64Array.findLast: 300n 300n@3
BigInt64Array.findLastIndex: 3 300n@3
BigInt64Array.some: true -3n@0 0n@1 7n@2
BigInt64Array.every: false -3n@0
BigUint64Array.forEach: undefined 18446744073709551613n@0 0n@1 7n@2 300n@3
BigUint64Array.map: BigUint64Array[18446744073709551610n,0n,14n,600n] 18446744073709551613n@0 0n@1 7n@2 300n@3
BigUint64Array.filter: BigUint64Array[18446744073709551613n,7n,300n] 18446744073709551613n@0 0n@1 7n@2 300n@3
BigUint64Array.find: 18446744073709551613n 18446744073709551613n@0
BigUint64Array.findIndex: 0 18446744073709551613n@0
BigUint64Array.findLast: 300n 300n@3
BigUint64Array.findLastIndex: 3 300n@3
BigUint64Array.some: true 18446744073709551613n@0
BigUint64Array.every: false 18446744073709551613n@0 0n@1
Int8Array.reduce: |-3@0|0@1|7@2|44@3 |44@3|7@2|0@1|-3@0 48 40
Uint8Array.reduce: |253@0|0@1|7@2|44@3 |44@3|7@2|0@1|253@0 304 -216
Uint8ClampedArray.reduce: |0@0|0@1|8@2|255@3 |255@3|8@2|0@1|0@0 263 247
Int16Array.reduce: |-3@0|0@1|7@2|300@3 |300@3|7@2|0@1|-3@0 304 296
Uint16Array.reduce: |65533@0|0@1|7@2|300@3 |300@3|7@2|0@1|65533@0 65840 -65240
Int32Array.reduce: |-3@0|0@1|7@2|300@3 |300@3|7@2|0@1|-3@0 304 296
Uint32Array.reduce: |4294967293@0|0@1|7@2|300@3 |300@3|7@2|0@1|4294967293@0 4294967600 -4294967000
Float16Array.reduce: |-3@0|0@1|7.5@2|300@3 |300@3|7.5@2|0@1|-3@0 304.5 295.5
Float32Array.reduce: |-3@0|0@1|7.5@2|300@3 |300@3|7.5@2|0@1|-3@0 304.5 295.5
Float64Array.reduce: |-3@0|0@1|7.5@2|300@3 |300@3|7.5@2|0@1|-3@0 304.5 295.5
BigInt64Array.reduce: |-3n@0|0n@1|7n@2|300n@3 |300n@3|7n@2|0n@1|-3n@0 304n 296n
BigUint64Array.reduce: |18446744073709551613n@0|0n@1|7n@2|300n@3 |300n@3|7n@2|0n@1|18446744073709551613n@0 18446744073709551920n -18446744073709551320n
Int8Array.forEach pure: 4 48
Int8Array.map pure: 4 48
Int8Array.filter pure: 4 48
Int8Array.find pure: 4 48
Int8Array.findIndex pure: 4 48
Int8Array.findLast pure: 4 48
Int8Array.findLastIndex pure: 4 48
Int8Array.some pure: 4 48
Int8Array.every pure: 1 -3
Uint8Array.forEach pure: 4 304
Uint8Array.map pure: 4 304
Uint8Array.filter pure: 4 304
Uint8Array.find pure: 4 304
Uint8Array.findIndex pure: 4 304
Uint8Array.findLast pure: 4 304
Uint8Array.findLastIndex pure: 4 304
Uint8Array.some pure: 4 304
Uint8Array.every pure: 1 253
Uint8ClampedArray.forEach pure: 4 263
Uint8ClampedArray.map pure: 4 263
Uint8ClampedArray.filter pure: 4 263
Uint8ClampedArray.find pure: 4 263
Uint8ClampedArray.findIndex pure: 4 263
Uint8ClampedArray.findLast pure: 4 263
Uint8ClampedArray.findLastIndex pure: 4 263
Uint8ClampedArray.some pure: 4 263
Uint8ClampedArray.every pure: 1 0
Int16Array.forEach pure: 4 304
Int16Array.map pure: 4 304
Int16Array.filter pure: 4 304
Int16Array.find pure: 4 304
Int16Array.findIndex pure: 4 304
Int16Array.findLast pure: 4 304
Int16Array.findLastIndex pure: 4 304
Int16Array.some pure: 4 304
Int16Array.every pure: 1 -3
Uint16Array.forEach pure: 4 65840
Uint16Array.map pure: 4 65840
Uint16Array.filter pure: 4 65840
Uint16Array.find pure: 4 65840
Uint16Array.findIndex pure: 4 65840
Uint16Array.findLast pure: 4 65840
Uint16Array.findLastIndex pure: 4 65840
Uint16Array.some pure: 4 65840
Uint16Array.every pure: 1 65533
Int32Array.forEach pure: 4 304
Int32Array.map pure: 4 304
Int32Array.filter pure: 4 304
Int32Array.find pure: 4 304
Int32Array.findIndex pure: 4 304
Int32Array.findLast pure: 4 304
Int32Array.findLastIndex pure: 4 304
Int32Array.some pure: 4 304
Int32Array.every pure: 1 -3
Uint32Array.forEach pure: 4 4294967600
Uint32Array.map pure: 4 4294967600
Uint32Array.filter pure: 4 4294967600
Uint32Array.find pure: 4 4294967600
Uint32Array.findIndex pure: 4 4294967600
Uint32Array.findLast pure: 4 4294967600
Uint32Array.findLastIndex pure: 4 4294967600
Uint32Array.some pure: 4 4294967600
Uint32Array.every pure: 1 4294967293
Float16Array.forEach pure: 4 304.5
Float16Array.map pure: 4 304.5
Float16Array.filter pure: 4 304.5
Float16Array.find pure: 4 304.5
Float16Array.findIndex pure: 4 304.5
Float16Array.findLast pure: 4 304.5
Float16Array.findLastIndex pure: 4 304.5
Float16Array.some pure: 4 304.5
Float16Array.every pure: 1 -3
Float32Array.forEach pure: 4 304.5
Float32Array.map pure: 4 304.5
Float32Array.filter pure: 4 304.5
Float32Array.find pure: 4 304.5
Float32Array.findIndex pure: 4 304.5
Float32Array.findLast pure: 4 304.5
Float32Array.findLastIndex pure: 4 304.5
Float32Array.some pure: 4 304.5
Float32Array.every pure: 1 -3
Float64Array.forEach pure: 4 304.5
Float64Array.map pure: 4 304.5
Float64Array.filter pure: 4 304.5
Float64Array.find pure: 4 304.5
Float64Array.findIndex pure: 4 304.5
Float64Array.findLast pure: 4 304.5
Float64Array.findLastIndex pure: 4 304.5
Float64Array.some pure: 4 304.5
Float64Array.every pure: 1 -3
BigInt64Array.forEach pure: 4 304n
BigInt64Array.map pure: 4 304n
BigInt64Array.filter pure: 4 304n
BigInt64Array.find pure: 4 304n
BigInt64Array.findIndex pure: 4 304n
BigInt64Array.findLast pure: 4 304n
BigInt64Array.findLastIndex pure: 4 304n
BigInt64Array.some pure: 4 304n
BigInt64Array.every pure: 1 -3n
BigUint64Array.forEach pure: 4 18446744073709551920n
BigUint64Array.map pure: 4 18446744073709551920n
BigUint64Array.filter pure: 4 18446744073709551920n
BigUint64Array.find pure: 4 18446744073709551920n
BigUint64Array.findIndex pure: 4 18446744073709551920n
BigUint64Array.findLast pure: 4 18446744073709551920n
BigUint64Array.findLastIndex pure: 4 18446744073709551920n
BigUint64Array.some pure: 4 18446744073709551920n
BigUint64Array.every pure: 1 18446744073709551613n
this forEach: object:{[object Number] 5} object:{[object Number] 5} global global string:s string:s undefined:undefined undefined:undefined object:null object:null arrow-other arrow-other
this map: object:{[object Number] 5} object:{[object Number] 5} global global string:s string:s undefined:undefined undefined:undefined object:null object:null arrow-other arrow-other
this filter: object:{[object Number] 5} object:{[object Number] 5} global global string:s string:s undefined:undefined undefined:undefined object:null object:null arrow-other arrow-other
this find: object:{[object Number] 5} object:{[object Number] 5} global global string:s string:s undefined:undefined undefined:undefined object:null object:null arrow-other arrow-other
this findIndex: object:{[object Number] 5} object:{[object Number] 5} global global string:s string:s undefined:undefined undefined:undefined object:null object:null arrow-other arrow-other
this findLast: object:{[object Number] 5} object:{[object Number] 5} global global string:s string:s undefined:undefined undefined:undefined object:null object:null arrow-other arrow-other
this findLastIndex: object:{[object Number] 5} object:{[object Number] 5} global global string:s string:s undefined:undefined undefined:undefined object:null object:null arrow-other arrow-other
this some: object:{[object Number] 5} object:{[object Number] 5} global global string:s string:s undefined:undefined undefined:undefined object:null object:null arrow-other arrow-other
this every: object:{[object Number] 5} global string:s undefined:undefined object:null arrow-other
this reduce: global global undefined:undefined undefined:undefined
arguments kept: 3:10,0 3:20,1 3:30,2
rest kept: 3:10,0 3:20,1 3:30,2
closures kept: 10:0 20:1 30:2
arguments mapped: 99/99 99/99 99/99
native callback: Float64Array[1.5,2,0]
native push: [4,0,Int8Array[4,5],5,1,Int8Array[4,5]]
bound callback: [104,106]
proxy callback: [4,5]
class callback: threw TypeError
not callable: threw TypeError
not callable empty: threw TypeError
reduce empty: threw TypeError
reduce empty seeded: seed
throw forEach: RangeError stop forEach after 2
throw map: RangeError stop map after 2
throw filter: RangeError stop filter after 2
throw find: RangeError stop find after 2
throw findIndex: RangeError stop findIndex after 2
throw findLast: RangeError stop findLast after 2
throw findLastIndex: RangeError stop findLastIndex after 2
throw some: RangeError stop some after 2
throw every: RangeError stop every after 2
throw reduce: RangeError stop reduce after 2
throw reduceRight: RangeError stop reduceRight after 2
nested: 11 12 13 21 22 23 31 32 33
detach forEach: undefined 1@0 undefined@1 undefined@2 undefined@3 len 0 detached true
detach map: Int16Array[7,7,7,7] 1@0 undefined@1 undefined@2 undefined@3 len 0 detached true
detach filter: Int16Array[] 1@0 undefined@1 undefined@2 undefined@3 len 0 detached true
detach find: undefined 1@0 undefined@1 undefined@2 undefined@3 len 0 detached true
detach findIndex: -1 1@0 undefined@1 undefined@2 undefined@3 len 0 detached true
detach findLast: undefined 4@3 undefined@2 undefined@1 undefined@0 len 0 detached true
detach findLastIndex: -1 4@3 undefined@2 undefined@1 undefined@0 len 0 detached true
detach some: false 1@0 undefined@1 undefined@2 undefined@3 len 0 detached true
detach every: true 1@0 undefined@1 undefined@2 undefined@3 len 0 detached true
detach reduce: false 1@0 undefined@1 undefined@2 undefined@3 len 0 detached true
detach reduceRight: false 4@3 undefined@2 undefined@1 undefined@0 len 0 detached true
detached before: threw TypeError
transferred values: [9,8,undefined,undefined,9,8]
resize track forEach: undefined 1,2,3,undefined,0,0 len 12
resize track map: Uint8Array[0,1,2,3,4,5] 1,2,3,undefined,0,0 len 12
resize track filter: Uint8Array[] 1,2,3,undefined,0,0 len 12
resize track find: undefined 1,2,3,undefined,0,0 len 12
resize track findIndex: -1 1,2,3,undefined,0,0 len 12
resize track findLast: undefined 6,5,4,3,2,1 len 3
resize track findLastIndex: -1 6,5,4,3,2,1 len 3
resize track some: false 1,2,3,undefined,0,0 len 12
resize track every: true 1,2,3,undefined,0,0 len 12
resize fixed forEach: undefined 1,2,undefined,undefined,0,0 len 6
resize fixed map: Uint8Array[0,1,2,3,4,5] 1,2,undefined,undefined,0,0 len 6
resize fixed filter: Uint8Array[] 1,2,undefined,undefined,0,0 len 6
resize fixed find: undefined 1,2,undefined,undefined,0,0 len 6
resize fixed findIndex: -1 1,2,undefined,undefined,0,0 len 6
resize fixed findLast: undefined 6,5,4,3,2,undefined len 0
resize fixed findLastIndex: -1 6,5,4,3,2,undefined len 0
resize fixed some: false 1,2,undefined,undefined,0,0 len 6
resize fixed every: true 1,2,undefined,undefined,0,0 len 6
out of bounds before: threw TypeError
species: Float64Array[0.5,1,1.5] Float64Array[1,3] species cb1 cb2 cb3 f1 f2 f3 species
species too short map: threw TypeError
species too short filter: threw TypeError
species refuses: threw SyntaxError
species refuses calls: 0
species content type: threw TypeError
writes seen: 1 10 100
generic receiver: threw TypeError
generic receiver map: threw TypeError`

// The callback methods of typed arrays answer what Node's do, in a runtime
// made twice.
func TestTypedArrayCallbacksAgreeWithNode(t *testing.T) {
	for round := range 2 {
		r := New(Config{})
		v, err := r.Run(compileForTest(t, typedCallbackScript))
		if err != nil {
			t.Fatal(err)
		}
		if got := v.String().Go(); got != typedCallbackWant {
			diffLinesForTest(t, round, got, typedCallbackWant)
		}
		r.Close()
	}
}

// diffLinesForTest reports the lines in which got and want differ.
func diffLinesForTest(t *testing.T, round int, got, want string) {
	t.Helper()
	g, w := strings.Split(got, "\n"), strings.Split(want, "\n")
	for i := range max(len(g), len(w)) {
		var gl, wl string
		if i < len(g) {
			gl = g[i]
		}
		if i < len(w) {
			wl = w[i]
		}
		if gl != wl {
			t.Errorf("round %d, line %d: got %q, want %q", round, i+1, gl, wl)
		}
	}
}

// A typed array's callback methods allocate nothing for each element: the
// arguments its callback is called with are a list made once for the loop,
// as Array.prototype's methods make it, whether the callback is answered
// as a pure body or run as its tree.
func TestTypedArrayCallbacksAllocateOncePerLoop(t *testing.T) {
	r := New(Config{})
	defer r.Close()
	_, err := r.Run(compileForTest(t, `
		var s = 0;
		function pure(x) { s += x; return x; }
		function tree(x) { for (var k = 0; k < 1; k++) s += x; return x; }
		function each(ta, cb) {
			ta.forEach(cb); ta.map(cb); ta.some(cb); ta.findLast(cb);
			ta.reduce(cb, 0); ta.reduceRight(cb, 0);
			return s;
		}
		var short = new Int32Array(1), long = new Int32Array(256);
		for (var i = 0; i < 10; i++) { each(short, pure); each(long, pure); each(short, tree); each(long, tree); }
		`))
	if err != nil {
		t.Fatal(err)
	}
	get := func(name string) Value {
		x, err := r.Run(compileForTest(t, name))
		if err != nil {
			t.Fatal(err)
		}
		return x
	}
	each, short, long := get("each"), get("short"), get("long")
	for _, cb := range []Value{get("pure"), get("tree")} {
		allocs := func(ta Value) float64 {
			args := []Value{ta, cb}
			return testing.AllocsPerRun(50, func() {
				if _, err := r.Call(each, Undefined, args); err != nil {
					t.Fatal(err)
				}
			})
		}
		if a, b := allocs(short), allocs(long); a != b {
			t.Errorf("%v allocations over one element, %v over 256", a, b)
		}
	}
}
