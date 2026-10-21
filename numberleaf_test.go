package quickjs_test

import (
	"testing"

	"github.com/go-quickjs/go-quickjs"
)

// numberLeafSweep calls Number's functions every tier calls without a frame
// where they run no script -- Number(x) and Boolean(x), Number.isInteger,
// isSafeInteger, isFinite and isNaN, the global isNaN, isFinite, parseInt
// and parseFloat, Number.prototype's toString, valueOf, toFixed,
// toExponential and toPrecision -- of every pair of 31 values of every kind,
// each the receiver and the argument -- numbers about every edge and every
// count's bounds, numeric strings, booleans, null, undefined, a BigInt, a
// symbol, an object whose valueOf and toString count their calls -- and
// digests each result or its error's type, and the calls: its sweep() is
// Node's (v26.10.0) 14415:143771872:328.
const numberLeafSweep = `
var calls = 0;
var obj = { valueOf() { calls++; return 7 }, toString() { calls++; return "9" } };
var vals = [0, -0, 1, -1, 1.5, 2, 10, 36, 37, 100, 101, -0.5, 100.9, 1.9, NaN, Infinity, -Infinity, 1e21, 123.456, 2 ** 53, "16", "0x1f", " 12px", "", true, false, null, undefined, 5n, Symbol("s"), obj];
var fns = [
	(x, a) => Number(x), (x, a) => Boolean(x), (x, a) => Number.isInteger(x), (x, a) => Number.isSafeInteger(x),
	(x, a) => Number.isFinite(x), (x, a) => Number.isNaN(x), (x, a) => isNaN(x), (x, a) => isFinite(x),
	(x, a) => parseInt(x, a), (x, a) => parseFloat(x), (x, a) => x.toString(a), (x, a) => x.valueOf(),
	(x, a) => x.toFixed(a), (x, a) => x.toExponential(a), (x, a) => x.toPrecision(a),
];
function sweep() {
	var h = 0x811c9dc5, n = 0;
	calls = 0;
	for (var f = 0; f < fns.length; f++)
		for (var i = 0; i < vals.length; i++)
			for (var j = 0; j < vals.length; j++) {
				var out;
				try { out = String(fns[f](vals[i], vals[j])) } catch (e) { out = e.constructor.name }
				for (var k = 0; k < out.length; k++) h = Math.imul(h ^ out.charCodeAt(k), 16777619);
				h = Math.imul(h ^ 124, 16777619);
				n++;
			}
	return n + ":" + (h >>> 0) + ":" + calls;
}
`

// TestNumberLeaves pins that Number's leaves answer as the ordinary call does
// and as Node does, in the interpreter and the tree tier, which a sweep run
// three times takes to; the JIT's frameless calls of them are internal/vm's
// TestJITPlainGoCalls.
func TestNumberLeaves(t *testing.T) {
	rt := quickjs.New()
	defer rt.Close()
	if got := evalString(t, rt, numberLeafSweep+`; var r; for (var t = 0; t < 3; t++) r = sweep(); r`); got != "14415:143771872:328" {
		t.Errorf("sweep() = %s, want Node's 14415:143771872:328", got)
	}
}
