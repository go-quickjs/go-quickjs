//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))

package vm

import (
	"runtime"
	"runtime/debug"
	"testing"
)

// cellsSetup's callees keep captured locals in cells of their own: mk its
// parameters a and b and its local c, which its arrow reads after c
// changed; bump a local its closure increments; five, more than a pool's
// blocks hold; and mk entered by Go itself, its cells open on its frame.
const cellsSetup = `
	function mk(a, b) { let c = a * 2; const f = () => a + b + c; c += 1; return f }
	function drive(n) { let s = 0; for (let i = 0; i < n; i++) s = (s + mk(i, 3)()) | 0; return s }
	function bump(a) { let x = a; const g = () => x++; g(); g(); return x + g() }
	function bumps(n) { let s = 0; for (let i = 0; i < n; i++) s = (s + bump(i)) | 0; return s }
	function five(a, b) { let c = 1, d = 2, e = 3; const h = () => a + b + c + d + e; c = d = e = a; return h() }
	function fives(n) { let s = 0; for (let i = 0; i < n; i++) s = (s + five(i, 1)) | 0; return s }
	function direct() { let s = 0; for (let i = 0; i < 40; i++) s += mk(i, i)(); return s }
`

// A native callee whose captured locals are in cells of their own takes
// its cells from its function's pool (mir's cellsTake), as V8's code
// allocates a function's context: a block Go linked, each cell holding its
// argument or undefined. Its runs make few calls of Go, where each entry
// had been one (ssa.GoCells); Go makes them while the collector marks, on
// a frame Go entered, past four cells, and when the pool runs out, which
// it fills again with more. Each answer is the interpreter's.
func TestJITCellsFromPools(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	calls := 0
	jitGoCallWork = func() { calls++ }
	defer func() { jitGoCallWork = nil }()
	rounds := []string{}
	for range 8 {
		rounds = append(rounds, `drive(300)`, `bumps(300)`, `fives(50)`, `direct()+mk(5,6)()`)
	}
	// The collector marking would have Go make the calls, and the cells:
	// not counted here (TestJITCellsFromPoolsMarking).
	runtime.GC()
	defer debug.SetGCPercent(debug.SetGCPercent(-1))
	r := jitAgreesForTest(t, cellsSetup, rounds...)
	before := calls
	if v, err := r.Run(compileForTest(t, `drive(300)+bumps(300)`)); err != nil || v.Number() != 226650 {
		t.Fatalf("= %v, %v", v, err)
	}
	// 600 entries; a pool of up to PoolCapacity blocks fills again a few
	// times a run.
	if calls-before >= 60 {
		t.Errorf("cells made by Go: %d calls of Go in 600 entries", calls-before)
	}
	for _, name := range []string{"mk", "bump"} {
		var e *jitEntry
		for k, x := range r.jit.cache {
			if fn := k.Value(); fn != nil && fn.Name == name {
				e = x
			}
		}
		if e == nil || e.cellsPool == nil || e.cellsPool.Size <= 8 {
			t.Errorf("%s's cells were not taken from their pool: %+v", name, e)
		}
	}
}

// What TestJITCellsFromPools runs while the collector marks: Go makes the
// cells, and each answer is still the interpreter's.
func TestJITCellsFromPoolsMarking(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	flag := jitMarkingForTest(t)
	rounds := []string{}
	for range 4 {
		rounds = append(rounds, `drive(300)`, `bumps(300)`, `fives(50)`, `direct()`)
	}
	r := jitRuntimeForTest(t, Config{JIT: true})
	*flag = 0
	want := New(Config{})
	t.Cleanup(func() { want.Close(); want.ReleaseClosed() })
	for _, rt := range []*Runtime{want, r} {
		if _, err := rt.Run(compileForTest(t, cellsSetup)); err != nil {
			t.Fatal(err)
		}
	}
	for i := range 2 * len(rounds) {
		src := rounds[i%len(rounds)]
		// Marking in every other pass over the rounds.
		*flag = uint8(i / len(rounds) % 2)
		wv, err := want.Run(compileForTest(t, src))
		if err != nil {
			t.Fatal(err)
		}
		gv, err := r.Run(compileForTest(t, src))
		if err != nil || gv.Number() != wv.Number() {
			t.Fatalf("round %d (%s), marking %d: %v, %v; interpreter %v", i, src, *flag, gv, err, wv)
		}
	}
}
