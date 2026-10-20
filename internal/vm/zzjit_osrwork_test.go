//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))

package vm

import (
	"fmt"
	"runtime"
	"runtime/debug"
	"testing"
)

// jitDefaultRuns runs setup, then each of srcs in turn, in a runtime with
// the JIT at its default thresholds and in one without, failing unless
// each answer is the same number; it returns the JIT's runtime.
func jitDefaultRuns(t *testing.T, setup string, srcs ...string) *Runtime {
	t.Helper()
	want := New(Config{})
	t.Cleanup(func() { want.Close(); want.ReleaseClosed() })
	r := New(Config{JIT: true})
	t.Cleanup(func() { r.Close(); r.ReleaseClosed() })
	for _, rt := range []*Runtime{want, r} {
		if _, err := rt.Run(compileForTest(t, setup)); err != nil {
			t.Fatal(err)
		}
	}
	for _, src := range srcs {
		wv, err := want.Run(compileForTest(t, src))
		if err != nil {
			t.Fatal(err)
		}
		gv, err := r.Run(compileForTest(t, src))
		if err != nil {
			t.Fatal(err)
		}
		if gv.Number() != wv.Number() {
			t.Fatalf("%s = %v, interpreter %v", src, gv.Number(), wv.Number())
		}
	}
	return r
}

// A function entered only by on-stack replacement at its loop, whose code
// leaves at the loop's first turn for something it then learns -- the
// shape of a private field's object, read before it had run -- has the
// existing tiers' work in its loop, which Go entered it after, spend its
// budget for what it learned (jitEntry's osrWork), and is compiled again
// for it, as V8's lower tiers spend the budget however a function runs.
// Before, its budget counted only invocations Go entered at the start and
// its native stretches, none here: each invocation left its code at once
// and the interpreter ran the rest, microbench_extra's private_field five
// times as long as with the JIT off. Each answer is the interpreter's.
func TestJITLoopEntriesSpendTheBudget(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	runtime.GC()
	defer debug.SetGCPercent(debug.SetGCPercent(-1))
	setup := `class Counter { #n = 0; add(n) { var j; for (j = 0; j < n; j++) this.#n = (this.#n + j) | 0; return this.#n } }
		function field(n) { return new Counter().add(n) }`
	var srcs []string
	n := 1.0
	for i := 0; i < 15; i++ {
		srcs = append(srcs, fmt.Sprintf("field(%v) + field(%v) + field(%v)", n, n, n))
		n *= [3]float64{2, 2.5, 2}[i%3]
	}
	r := jitDefaultRuns(t, setup, srcs...)
	hosts, interpreted := r.jit.hosts, r.jit.interpreted
	if _, err := r.Run(compileForTest(t, `field(200000)`)); err != nil {
		t.Fatal(err)
	}
	if h, i := r.jit.hosts-hosts, r.jit.interpreted-interpreted; h > 2 || i > 0 {
		t.Errorf("an invocation left native code %d times, %d of them finished by the interpreter", h, i)
	}
}
