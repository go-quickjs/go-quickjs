//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))

package vm

import "testing"

// A read native code did not find teaches the megamorphic cache, whatever
// the interpreter's holds: a site whose shapes its cases and that cache
// share out between them never fills, never gives up, and calls Go at
// every read of the shapes its code lacks, for as long as it runs.
func TestJITGoReadFillsTheMegamorphicCache(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	setup := `
		function P(v) { this.v = v }
		function run(o, n) {
			let s = new P(0)
			for (let i = 0; i < n; i++) s = new P(s.v + o.k)
			return s.v
		}
		var j = 0
		function batch(m) {
			let t = 0
			for (let c = 0; c < m; c++, j++) {
				const o = {}
				for (let k = 0; k <= j % 12; k++) o["p" + k] = k
				o.k = j
				t += run(o, 5000)
			}
			return t
		}
	`
	var rounds []string
	for range 14 {
		rounds = append(rounds, `batch(4)`)
	}
	// At the JIT's own thresholds: compiled at once, the code holds other
	// shapes than the cache comes to.
	r := jitDefaultRuns(t, setup, rounds...)
	e := jitCallbackEntry(r, "run")
	if e == nil || e.ssa == nil {
		t.Fatalf("run was not compiled: %+v", e)
	}
	calls := e.ssaStats.goCalls
	for range 6 {
		if _, err := r.Run(compileForTest(t, `batch(4)`)); err != nil {
			t.Fatal(err)
		}
	}
	// Native all along: judged slow for those calls, a tree runs it, and
	// makes no calls of Go to count.
	if n := e.ssaStats.goCalls - calls; n > 100 || e.entrySlow {
		t.Errorf("%d calls of Go in two cycles of the shapes it has met, judged slow %v", n, e.entrySlow)
	}
}
