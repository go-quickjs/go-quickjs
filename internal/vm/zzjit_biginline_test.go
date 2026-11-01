//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))

package vm

import (
	"runtime"
	"runtime/debug"
	"testing"
)

// bigInlineSetup's loop calls small functions of BigInts, which it
// inlines: an arithmetic step, a comparison and a negation. odd's
// comparisons are of a BigInt with numbers and of one past 64 bits, which
// the native comparison of BigInts exits for.
const bigInlineSetup = `function step(s, x) { return (s * 31n + x) & 0xffffffffffffn }
function less(x, y) { return x < y }
function neg(x) { return -x }
function run(n) { var s = 0n, c = 0; for (var i = 0; i < n; i++) { var b = BigInt(i); s = step(s, neg(b)); if (less(b, 50n)) c++ } return s + ':' + c }
function odd(n) { var c = 0; for (var i = 0; i < n; i++) if (less(BigInt(i), 2 ** 70)) c++; return c + ':' + less(5n, 5.5) + ':' + less(2n ** 80n, 1n) }
`

// A BigInt operator inlined from a callee is made on BigInts natively, as
// the root's are, by the callee's own feedback (ssa's bigNative): run's
// loop, its callees inlined, leaves native code nowhere, with the
// collector idle and while it marks, and every answer is the
// interpreter's and Node's (v26.10.0).
func TestJITBigIntInlined(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	for _, marking := range []bool{false, true} {
		runtime.GC()
		gc := debug.SetGCPercent(-1)
		if marking {
			jitMarkingForTest(t)
		}
		var rounds []string
		for range 8 {
			rounds = append(rounds, "run(300)", "odd(300)")
		}
		r := jitAgreesForTest(t, bigInlineSetup, rounds...)
		e := jitNativeForTest(t, r, "run")
		ended, calls, inlined := e.ssaStats.ended, e.ssaStats.goCalls, len(e.ssaInlined)
		v, err := r.Run(compileForTest(t, "run(300) + ' ' + odd(300)"))
		if err != nil {
			t.Fatal(err)
		}
		if got, want := v.String().Go(), "218824755733354:50 300:true:false"; got != want {
			t.Errorf("marking %v: got %s, want Node's %s", marking, got, want)
		}
		if inlined < 3 || e.ssaStats.ended != ended || e.ssaStats.goCalls-calls > 1 {
			t.Errorf("marking %v: run inlined %d callees, left native code %d times, made %d calls of Go", marking, inlined, e.ssaStats.ended-ended, e.ssaStats.goCalls-calls)
		}
		debug.SetGCPercent(gc)
	}
}
