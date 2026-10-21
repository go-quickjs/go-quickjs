package quickjs_test

import (
	"testing"

	"github.com/go-quickjs/go-quickjs"
)

// mathLeafSweep calls Math.imul, Math.pow, Math.atan2 and Math.hypot of
// every pair of a set of values -- numbers about every edge, a numeric
// string, a boolean, null, undefined, a BigInt and an object whose valueOf
// counts its calls -- and Math.clz32, and hypot, pow and imul of other
// counts of arguments, and digests each result or its error's type, and the
// calls; hot is a loop of the five on numbers. Node's (v26.10.0): sweep()
// 2328:818518462:193, hot(5000) 1433262336:12696087.58781071.
const mathLeafSweep = `
var vals = [0, -0, 1, -1, 2.5, -2.5, 3, 0x7fffffff, -0x80000000, 0xffffffff, 2**32 + 5, 2**53, -(2**53), 1e300, -1e-300, NaN, Infinity, -Infinity, "7", true, null, undefined, 5n, { valueOf() { calls++; return 3 } }];
var calls = 0;
function digest(out) { let h = 0x811c9dc5; for (let k = 0; k < out.length; k++) h = Math.imul(h ^ out.charCodeAt(k), 16777619); return h >>> 0 }
function sweep() {
  calls = 0; let parts = [];
  for (const a of vals) for (const b of vals) {
    for (const f of [Math.imul, Math.pow, Math.atan2, Math.hypot]) { let o; try { o = String(f(a, b)) } catch (e) { o = e.constructor.name } parts.push(o) }
  }
  for (const a of vals) { let o; try { o = String(Math.clz32(a)) + ':' + String(Math.hypot(a)) + ':' + String(Math.hypot(a, 1, 2)) + ':' + String(Math.pow(a)) + ':' + String(Math.imul(a)) } catch (e) { o = e.constructor.name } parts.push(o) }
  return parts.length + ':' + digest(parts.join('|')) + ':' + calls;
}
function hot(n) { let s = 0, t = 0; for (let i = 0; i < n; i++) { s = Math.imul(s ^ i, 0x9e3779b1) | 0; t += Math.clz32(i) + Math.pow(i & 7, 2) + Math.atan2(i, 3) + Math.hypot(i, 4) } return s + ':' + t }
`

// TestMathLeaves pins Math's functions every tier but the JIT applies
// without a frame (funcData.mathOp) -- imul, pow, atan2 and hypot of two
// numbers, clz32 of one -- to the ordinary call's answers and Node's, in the
// interpreter and the tree tier, which the sweep's rounds take it to; the
// JIT's is internal/vm's TestJITMathFamily.
func TestMathLeaves(t *testing.T) {
	rt := quickjs.New()
	defer rt.Close()
	if got := evalString(t, rt, mathLeafSweep+`; var r; for (var t = 0; t < 3; t++) r = sweep() + " " + hot(5000); r`); got != "2328:818518462:193 1433262336:12696087.58781071" {
		t.Errorf("sweep() hot(5000) = %s, want Node's 2328:818518462:193 1433262336:12696087.58781071", got)
	}
}
