//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))

package vm

import (
	"runtime"
	"runtime/debug"
	"testing"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
)

// jitArrowsForTest is the entries of the arrows r's JIT holds: an arrow
// has no name of its own.
func jitArrowsForTest(t *testing.T, r *Runtime) []*jitEntry {
	t.Helper()
	var es []*jitEntry
	for k, x := range r.jit.cache {
		if fn := k.Value(); fn != nil && fn.Kind == bytecode.KindArrow {
			es = append(es, x)
		}
	}
	return es
}

// The closures of a function made anew at each call -- an arrow its maker
// makes each time -- share what the JIT knows of the function, as V8's
// share their SharedFunctionInfo (jitCodeFields): how warm it is and the
// hint of its code. Each closure called twice, at a generic call that has
// met more functions than it calls natively, every one of the functions
// is compiled and called natively. Kept by each closure, a function past
// the call's four, m6's, had never been warm enough to compile, and a
// generic call of one found no code by a new closure's hint, each call
// Go's, each run in the existing tiers; a leaf's, m5's, which the existing
// tiers run with no frame, had its calls Go made for native code warm
// nothing.
func TestJITClosuresShareCode(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	setup := `
		function each(a, fn) { let s = 0; for (let i = 0; i < a.length; i++) s = (s + fn(a[i])) | 0; return s }
		function m1(k) { return (x) => x + k }
		function m2(k) { return (x) => x * k }
		function m3(k) { return (x) => x - k }
		function m4(k) { return (x) => x ^ k }
		function m5(k) { return (x) => (x + k) | 1 }
		function m6(k) { return (x) => { const o = { v: x }; return o.v + k } }
		var ms = [m1, m2, m3, m4, m5, m6], arr = [1, 2]
		function run(n) { let s = 0; for (let j = 0; j < n; j++) s = (s + each(arr, ms[j % 6](j))) | 0; return s }
	`
	rounds := []string{}
	for range 10 {
		rounds = append(rounds, `run(600)`)
	}
	r := jitAgreesForTest(t, setup, rounds...)
	arrows := jitArrowsForTest(t, r)
	if len(arrows) != 6 {
		t.Fatalf("%d arrows have entries, want 6", len(arrows))
	}
	// Counted with the collector off: while it marks, Go makes native
	// calls (mir's markingCall).
	runtime.GC()
	defer debug.SetGCPercent(debug.SetGCPercent(-1))
	each := jitNativeForTest(t, r, "each")
	before := make([]uint64, len(arrows))
	for i, e := range arrows {
		before[i] = jitDerivedCalls(e)
	}
	goCalls := each.ssaStats.goCalls
	if _, err := r.Run(compileForTest(t, `run(600)`)); err != nil {
		t.Fatal(err)
	}
	// Each arrow's closures, a hundred of them, called twice each.
	for i, e := range arrows {
		if e.ssa == nil {
			t.Errorf("an arrow has no code: %+v", e)
			continue
		}
		if d := jitDerivedCalls(e) - before[i]; d != 200 {
			t.Errorf("an arrow's code was called natively %d times in a round, want 200", d)
		}
	}
	if d := each.ssaStats.goCalls - goCalls; d != 0 {
		t.Errorf("each made %d calls of Go in a round, want none", d)
	}
}
