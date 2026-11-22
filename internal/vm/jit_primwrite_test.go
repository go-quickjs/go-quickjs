//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64))

package vm

import (
	"strings"
	"testing"
)

// A write of a primitive's property the interpreter makes is remembered by
// its site (jitNotePrimWrite), and code compiled for the site makes such a
// write Go's from native code, as code compiled after one met at an exit
// does. A function of eight loops, a number's property written in each
// once a call -- micro-benchmark carried_exit_loops' shape, which has 24 --
// had failed a guard at each loop's write in turn, compiled again after
// each, the interpreter running the rest of every invocation that failed:
// 24 guards and 23 compiles again for carried_exit_loops, half of its
// add's calls the interpreter's. The interpreter has run every write of 1
// by the time the function's code is compiled, at its runtime's own
// thresholds, so the code fails none. Each answer is the interpreter's.
func TestJITPrimitiveWritesLearnedByTheInterpreter(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	const loops = 8
	var b strings.Builder
	b.WriteString(`function V(x, y) { this.x = x; this.y = y }
		V.prototype.add = function (o) { return new V(this.x + o.x, this.y + o.y) }
		var obj = { a: 1 }
		function loops(n, k) {
			const d = new V(1, 2)
			let t = 0
	`)
	for range loops {
		b.WriteString(`{ let s = new V(0, 0); for (let i = 0; i < n; i++) { const o = i === k ? 1 : obj; o.a = 1; s = s.add(d) } t += s.x }
	`)
	}
	b.WriteString(`return t }
		function run(n) { let t = 0; for (let j = 0; j < n; j++) t += loops(200, 5); return t }`)
	want := newTestRuntime(t, Config{})
	t.Cleanup(func() { want.Close(); want.ReleaseClosed() })
	r := newTestRuntime(t, Config{JIT: true})
	t.Cleanup(func() { r.Close(); r.ReleaseClosed() })
	// The runtime's own budgets and thresholds, QJS_JIT_STRESS's none,
	// compiled on the spot.
	r.jitStress = jitStressConfig{}
	r.SetJITWarmUp(jitInvocationsForMaglev, jitInvocationsAfterICUpdate)
	for _, rt := range []*Runtime{want, r} {
		if _, err := rt.Run(compileForTest(t, b.String())); err != nil {
			t.Fatal(err)
		}
	}
	for round := range 4 {
		wv, err := want.Run(compileForTest(t, `run(32)`))
		if err != nil {
			t.Fatal(err)
		}
		gv, err := r.Run(compileForTest(t, `run(32)`))
		if err != nil {
			t.Fatal(err)
		}
		if gv.Number() != wv.Number() {
			t.Fatalf("round %d: got %v, interpreter %v", round, gv, wv)
		}
	}
	e := jitCodeForTest(r, "loops")
	if e == nil || e.ssaStats.entries == 0 {
		t.Fatal("loops has no native code")
	}
	if n := e.ssaStats.guards; n != 0 {
		t.Errorf("loops failed %d guards, want none: its writes of a number learned one at a time", n)
	}
	if n := e.ssaStats.ended; n != 0 {
		t.Errorf("loops finished in the interpreter %d times, want none", n)
	}
	// Its writes of 1, each a call of Go from native code: one a loop a
	// call, from the first round's compile on.
	if e.ssaStats.goCalls == 0 {
		t.Error("loops made no call of Go for its writes of a number")
	}
}
