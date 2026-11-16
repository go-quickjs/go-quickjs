//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64))

package vm

import (
	"strings"
	"testing"
)

// TestJITRegArgumentsLengthWhole has a function that keeps its arguments
// object in a variable and reads its length, EarleyBoyer's sc_list, called
// by a native caller deep in a recursion, and an object whose length is a
// getter's, and checks that the length, which native code does not read of
// either, Go reads from native code, run whole on the register frame (ssa's
// CallSite Whole): no exit at get_length, no run of list ended in the
// interpreter, and its callers call it natively, not through Go. Before,
// the read left native code at every call, the rest of the run the
// interpreter's, and the callers, finding it left, called it through Go:
// 2.5 million calls of Go of a function with code in a long EarleyBoyer
// run.
func TestJITRegArgumentsLengthWhole(t *testing.T) {
	const src = `
function Pair(car, cdr) { this.car = car; this.cdr = cdr }
function list() { var a = arguments, res = null; for (var i = a.length - 1; i >= 0; i--) res = new Pair(a[i], res); return res }
function len(p) { var n = 0; while (p !== null) { n++; p = p.cdr } return n }
function rec(d, k) { if (d == 0) return len(list(k, 1, 2)) + len(list()) + len(list(k)); return rec(d - 1, k) + 1 }
var g = 0, withGetter = { get length() { return ++g & 3 } };
function count(o) { var x = o, s = 0; for (var i = 0; i < 4; i++) s += x.length; return s }
function run(n) { var t = 0; for (var i = 0; i < n; i++) t += rec(20, i); return t }
function lengths(n) { var t = 0; for (var i = 0; i < n; i++) t += count(withGetter) + count("abc"); return t + ":" + g }
`
	regBothMarking(t, func(t *testing.T) {
		rts := regNativeRuntimes(t, src)
		r := rts[0]
		r.jitStress = jitStressConfig{}
		r.SetJITWarmUp(1, 1)
		r.SetJITReport(true)
		regProfitRun(t, rts, "run", 3, "12000", Float(500))
		regProfitRun(t, rts, "lengths", 3, "9000:6000", Float(500))
		st := r.RegNativeStats()
		hosts, ops, ended := st.HostOps["get_length"], st.GoCallOps, st.Ended
		regProfitRun(t, rts, "run", 3, "12000", Float(500))
		st = r.RegNativeStats()
		calls := st.GoCallOps["call_w->function with code"] - ops["call_w->function with code"]
		regProfitRun(t, rts, "lengths", 3, "9000:12000", Float(500))
		st = r.RegNativeStats()
		t.Logf("exits %v; calls of Go %v; ended %v", st.HostOps, st.GoCallOps, st.Ended)
		if n := st.HostOps["get_length"] - hosts; n != 0 {
			t.Errorf("%d exits at get_length", n)
		}
		for k, n := range st.Ended {
			if strings.HasSuffix(k, " in list") && n != ended[k] {
				t.Errorf("%d runs ended in the interpreter: %s", n-ended[k], k)
			}
		}
		if calls > 30 {
			t.Errorf("%d calls of Go of functions with code, of %d calls of list", calls, 3*500*3)
		}
		if n := st.GoCallOps["whole get_length"] - ops["whole get_length"]; n == 0 {
			t.Errorf("no length read by Go from native code")
		}
	})
}
