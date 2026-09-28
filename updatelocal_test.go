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
