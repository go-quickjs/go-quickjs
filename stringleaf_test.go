package quickjs_test

import (
	"testing"

	"github.com/go-quickjs/go-quickjs"
)

// stringLeafSweep calls each of String's methods that every tier calls
// without a frame where its check passes (a native leaf), and those it
// leaves to the ordinary call, on receivers of every kind of string and
// arguments of every kind of value -- numbers about every edge, strings,
// booleans, undefined, null, a BigInt, a symbol, an object whose valueOf
// and toString count their calls -- and digests each result or the error's
// type, and the calls: its sweep() is Node's (v26.10.0) 47520:244910520:3840.
const stringLeafSweep = `
var calls = 0;
var obj = { valueOf() { calls++; return 2 }, toString() { calls++; return "o" } };
var recvs = ["", "a", "abc", "ABCdef", "  x y  ", "😀z", "\uD800x", "éé", "straße", "xy".repeat(20)];
var vals = [0, 1, -1, 2, 3.7, -0, NaN, Infinity, -Infinity, 1e9, 2147483648, "1", "b", "", "NFKD", true, false, undefined, null, 5n, Symbol("s"), obj];
var fns = [
	(s, a, b) => s.at(a), (s, a, b) => s.charAt(a), (s, a, b) => s.charCodeAt(a), (s, a, b) => s.codePointAt(a),
	(s, a, b) => s.indexOf(a, b), (s, a, b) => s.lastIndexOf(a, b), (s, a, b) => s.includes(a, b),
	(s, a, b) => s.startsWith(a, b), (s, a, b) => s.endsWith(a, b), (s, a, b) => s.slice(a, b),
	(s, a, b) => s.substring(a, b), (s, a, b) => s.substr(a, b), (s, a, b) => s.toUpperCase(a),
	(s, a, b) => s.toLowerCase(a), (s, a, b) => s.trim(a), (s, a, b) => s.trimStart(a), (s, a, b) => s.trimEnd(b),
	(s, a, b) => s.isWellFormed(a), (s, a, b) => s.toWellFormed(b), (s, a, b) => s.concat(a, b),
	(s, a, b) => s.repeat(a), (s, a, b) => s.padStart(a, b), (s, a, b) => s.padEnd(a, b),
	(s, a, b) => s.normalize(a), (s, a, b) => s.localeCompare(a), (s, a, b) => String.fromCharCode(a, b),
	(s, a, b) => String.fromCodePoint(a, b),
];
function sweep() {
	var h = 0x811c9dc5, n = 0;
	calls = 0;
	for (var f = 0; f < fns.length; f++) {
		var fn = fns[f];
		for (var r = 0; r < recvs.length; r++)
			for (var i = 0; i < vals.length; i++)
				for (var j = 0; j < vals.length; j += 3) {
					var out;
					try { out = String(fn(recvs[r], vals[i], vals[j])) } catch (e) { out = e.constructor.name }
					for (var k = 0; k < out.length; k++) h = Math.imul(h ^ out.charCodeAt(k), 16777619);
					h = Math.imul(h ^ 124, 16777619);
					n++;
				}
	}
	return n + ":" + (h >>> 0) + ":" + calls;
}
`

// TestStringLeaves pins that String's methods, called without a frame where
// they run no script (ztree_aleaf.go), answer as the ordinary call does and
// as Node does, in the interpreter and the tree tier, which a sweep run
// three times takes to; the JIT's is internal/vm's TestJITStringLeaves.
func TestStringLeaves(t *testing.T) {
	rt := quickjs.New()
	defer rt.Close()
	if got := evalString(t, rt, stringLeafSweep+`; var r; for (var t = 0; t < 3; t++) r = sweep(); r`); got != "47520:244910520:3840" {
		t.Errorf("sweep() = %s, want Node's 47520:244910520:3840", got)
	}
}
