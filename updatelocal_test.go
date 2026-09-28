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
