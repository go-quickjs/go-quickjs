package vm

import "testing"

// TestRegJumpsWithoutCalls pins the jumps the loop answers without a call
// -- a truthiness of a boolean, an object or a number, and === of two
// numbers or of a value compared by its words -- and what they leave to the
// compare's slow path: a string's or a BigInt's truthiness and equality,
// undefined and null, document.all's stand-in aside.
func TestRegJumpsWithoutCalls(t *testing.T) {
	const src = `
		function truth(v) { var s = ""; if (v) s += "T"; else s += "F"; while (v) { s += "w"; break } if (!v) s += "n"; return s }
		function seq(a, b) { var s = ""; if (a === b) s += "="; if (a !== b) s += "!"; return s }
		function seqI(a) { var s = ""; if (a === 3) s += "="; if (a !== 3) s += "!"; return s }
		var sym = Symbol("s"), o = {}, big = 2n ** 70n;
		var vals = [true, false, 0, -0, 1, NaN, "", "x", undefined, null, o, sym, 0n, 1n, big];
		var out = [vals.map(truth).join()];
		var pairs = [[1, 1], [0, -0], [NaN, NaN], ["ab", "a" + "b"], ["a", "b"], [1n, 1n], [big, 2n ** 70n],
			[1n, 1], [o, o], [o, {}], [sym, sym], [sym, Symbol("s")], [undefined, null], [null, null],
			[undefined, undefined], [true, true], [true, 1], ["1", 1], [o, "x"], [1, big]];
		out.push(pairs.map(function (p) { return seq(p[0], p[1]) }).join());
		out.push([3, 3.5, "3", 3n, undefined, -0].map(seqI).join());
		out.join(" ")`
	const want = "Tw,Fn,Fn,Fn,Tw,Fn,Fn,Tw,Fn,Fn,Tw,Tw,Fn,Tw,Tw " +
		"=,=,!,=,!,=,=,!,=,!,=,!,!,=,=,=,!,!,!,! =,!,!,!,!,!"
	regRunBoth(t, src, want, 3)
}

// TestRegMethodReceiver pins get_method_w, which reads a method of a
// receiver held in a binding and puts the receiver in the call's window:
// the receiver is the binding's value when the method is read, whatever
// the arguments then do to the binding, and a read that misses -- a getter,
// a primitive's method, a missing method -- has the same receiver.
func TestRegMethodReceiver(t *testing.T) {
	const src = `
		function P(n) { this.n = n }
		P.prototype.who = function (x) { return this.n + ":" + x };
		function swap() { var a = new P("a"), b = new P("b"); return a.who((a = b, "x")) + "/" + a.who("y") }
		function getter() { var o = { get m() { return function () { return this.k } }, k: "g" }; return o.m() }
		function prim() { var s = "abc", n = 5; return s.toUpperCase() + n.toFixed(1) }
		function missing() { var o = {}; try { o.nope() } catch (e) { return e instanceof TypeError } }
		function thisRecv() { return this.who("t") }
		function loop() { var p = new P("l"), s = ""; for (var i = 0; i < 3; i++) s += p.who(i); return s }
		[swap(), getter(), prim(), missing(), thisRecv.call(new P("q")), loop()].join(" ")`
	const want = "a:x/b:y g ABC5.0 true q:t l:0l:1l:2"
	regRunBoth(t, src, want, 6)
}

// TestRegLeafGivenUp pins a call site of a pure body the frameless
// evaluator has given up on: the site leaves the leaf's way, and the body
// is entered as a frame with the same answers.
func TestRegLeafGivenUp(t *testing.T) {
	const src = `
		function same(k) { return k }
		function cat(k) { return k + 1 }
		function run() {
			var s = 0, t = "";
			for (var i = 0; i < 200; i++) { s += same(i); t = cat("" + (i % 10)) }
			return s + t
		}
		run() + "," + run()`
	const want = "1990091,1990091"
	regRunBoth(t, src, want, 2)
}

// TestRegStrictGlobalStores pins strict code's global stores in register
// code: a store of a value that runs no code asks whether the name resolves
// at the store, a ReferenceError for a name that does not and a TypeError
// for a binding that is not writable, on the site's first store and after
// its fast path was filled; an update asks again after its read; a value
// that runs code leaves the function to the stack form, where the
// reference is resolved first.
func TestRegStrictGlobalStores(t *testing.T) {
	const src = `
		var sg = 0;
		function put(v) { "use strict"; for (var j = 0; j < 3; j++) sg = v; return sg }
		function putMissing() { "use strict"; nope = 1; return "stored" }
		function putFixed() { "use strict"; fixed = 2; return "stored" }
		function bump() { "use strict"; sg++; sg += 2; return sg }
		function bumpGone() { "use strict"; gone += 1; return "stored" }
		function makes() { "use strict"; made = (globalThis.made = 1, 2); return "stored" }
		function tryIt(f) { try { return f() } catch (e) { return e.name } }
		Object.defineProperty(globalThis, "fixed", { value: 1, writable: false });
		globalThis.gone = 1;
		var out = [put(4), put(5), tryIt(putMissing), typeof nope, tryIt(putFixed), bump(), tryIt(bumpGone), gone];
		delete globalThis.gone;
		out.push(tryIt(bumpGone), tryIt(makes), made);
		try { putMissing() } catch (e) { out.push(e.stack.split(String.fromCharCode(10))[1].trim()) }
		out.join()`
	const want = "4,5,ReferenceError,undefined,TypeError,8,stored,2,ReferenceError,ReferenceError,1,at putMissing (test:4:41)"
	regRunBoth(t, src, want, 4)
}

// TestRegLazyArguments pins args_len and args_index, arguments.length and
// arguments[i] in a function whose arguments object is made only on
// demand: the arguments read where they lie, an index past them, a key
// that is no index, and a sloppy function's parameter that the object maps,
// which makes the object.
func TestRegLazyArguments(t *testing.T) {
	const src = `
		function append() { if (arguments.length === 0) return "none"; var s = arguments[arguments.length - 1];
			for (var i = arguments.length - 2; i >= 0; i--) s = arguments[i] + s; return s }
		function past() { return [arguments[3], arguments[-1], arguments["length"], arguments.length].join("|") }
		function mapped(a, b) { a = "A"; return arguments[0] + arguments[1] + arguments.length }
		function strict(a) { "use strict"; a = "A"; return arguments[0] + arguments.length }
		[append(), append("x"), append("a", "b", "c"), past(1, 2), mapped("p", "q"), strict("s")].join(" ")`
	const want = "none x abc ||2|2 Aq2 s1"
	regRunBoth(t, src, want, 4)
}

// TestRegSteppedReads pins x[++i], x[i++] and their decrements: the key
// stepped as the stack form steps it -- a string key made a number, an
// object's valueOf called once -- the read of the key before or after the
// step, a hole or a getter answered on the slow path, and a read of null
// that throws after the step, out of a function with no handler.
func TestRegSteppedReads(t *testing.T) {
	const src = `
		function post(a) { var i = 0, s = ""; while (i < 4) s += a[i++] + "|"; return s + "@" + i }
		function down(a) { var i = a.length - 1, s = ""; while (i >= 0) s += a[i--]; return s + "@" + i }
		function strKey(a) { var i = "1"; return [a[i++], typeof i, i].join() }
		function objKey(a) { var n = 0, i = { valueOf() { n++; return 0 } }; return [a[i++], i, n].join() }
		function self(a) { var i = 0; i = a[i++]; return i }
		function nul(o) { var i = 5; return o[i++] }
		function pre(o) { var i = 5; return o[++i] }
		function tryIt(f, o) { try { return f(o) } catch (e) { return e.name } }
		var holes = [1, , 3]; Object.defineProperty(Array.prototype, 3, { get() { return "g" }, configurable: true });
		var out = [post([1, 2, 3]), post(holes), down([1, 2, 3]), strKey(["a", "b", "c"]), objKey(["a"]), self([7]),
			tryIt(nul, null), tryIt(pre, undefined), post("abcd")];
		delete Array.prototype[3];
		out.join(" ")`
	const want = "1|2|3|g|@4 1|undefined|3|g|@4 321@-1 b,number,2 a,1,1 7 TypeError TypeError a|b|c|d|@4"
	regRunBoth(t, src, want, 8)
}
