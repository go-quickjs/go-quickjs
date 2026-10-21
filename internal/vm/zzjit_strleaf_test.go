//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))

package vm

import "testing"

// TestJITStringLeaves is the root package's TestStringLeaves with the JIT:
// the sweep of String's methods compiled, after rounds that warm it, and
// called natively, each answer and the calls of its object's conversions
// Node's, and the interpreter's.
func TestJITStringLeaves(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	r := jitAgreesForTest(t, stringLeafSweepJS, "sweep()", "sweep()", "sweep()", "sweep()")
	v, err := r.Run(compileForTest(t, "sweep()"))
	if err != nil {
		t.Fatal(err)
	}
	if got := v.String().Go(); got != "47520:244910520:3840" {
		t.Errorf("sweep() = %s, want Node's 47520:244910520:3840", got)
	}
}

// stringLeafSweepJS is the root package's stringLeafSweep.
const stringLeafSweepJS = `
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
