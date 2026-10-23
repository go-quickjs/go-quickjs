//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))

package vm

import (
	"runtime"
	"runtime/debug"
	"testing"
)

// cellsBackSetup's functions capture their locals and parameters only in
// the callbacks of loops their code inlines and does not make
// (ssa.MadeCallback), so the cells a native call of them takes at its
// entry go back to their pool as it returns (ssa.Func's CellsBack): called
// natively, in a driver's loop, and through recursion, each call takes
// the block an earlier one gave back. Then a callback leaves native code,
// where Go makes its frame and the closure on the frame's cells, which
// then return through Go and are not given back; a per-iteration binding
// gets fresh cells at each turn (GoClose); a function that keeps a closure
// on its cells keeps them; and the collector runs at every call of Go.
const cellsBackSetup = `
	var nums = [1, 2, 3, 4, 5, 6, 7, 8], odd = { v: 'x' }
	function scale(k, at) {
		let s = 0
		nums.forEach((x, i) => { s += (i === at ? odd.v : x) * k })
		return s
	}
	function turns(k) {
		let s = 0
		for (let j = 0; j < 3; j++) nums.forEach((x) => { s += x * k + j })
		return s
	}
	function deep(n, k) {
		if (n === 0) return 0
		let s = 0
		nums.forEach((x) => { s += x + k })
		return s + deep(n - 1, k + 1)
	}
	var saved = []
	function keeps(k) {
		let s = 0
		nums.forEach((x) => { s += x * k })
		if (k % 7 === 0) saved.push(() => s + k)
		return s
	}
	function drive(n, at) { let r = 0; for (let i = 0; i < n; i++) r += scale(i, i === at ? 3 : -1) + turns(i) + deep(4, i) + keeps(i); return r }
	function kept() { const r = saved.map((f) => f()).join(); saved = []; return r }
`

// Each answer is the interpreter's, under every stress setting, and a
// driver's loop of native calls makes no block of cells for them.
func TestJITCellsBack(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	runtime.GC()
	defer debug.SetGCPercent(debug.SetGCPercent(-1))
	calls := 0
	jitGoCallWork = func() {
		if calls++; calls%5 == 0 {
			runtime.GC()
		}
	}
	defer func() { jitGoCallWork = nil }()
	var rounds []string
	for i := range 10 {
		rounds = append(rounds, `drive(30, `+[]string{"-1", "17"}[i/7]+`)`, `kept()`)
	}
	for _, c := range []jitStressConfig{{}, {threshold: true, budget: 3}, {threshold: true, budget: 2, deopt: 3}} {
		jitAgreesForTestWith(t, c, cellsBackSetup, rounds...)
	}
	jitGoCallWork = nil
	r := jitAgreesForTestWith(t, jitStressConfig{}, cellsBackSetup, rounds[:7]...)
	for _, name := range []string{"scale", "deep"} {
		if e := jitCallbackEntry(r, name); e == nil || e.ssa == nil || e.ssaUnmade == 0 {
			t.Errorf("%s does not inline its callback unmade: %+v", name, e)
		}
	}
	// Each a loop of 2,000 native calls -- deep's of itself five deep --
	// which had each taken a block of cells, and turns a block at each of
	// its three turns.
	for _, f := range []string{"scale(i, -1)", "turns(i)", "deep(4, i)"} {
		src := `(() => { let r = 0; for (let i = 0; i < 2000; i++) r += ` + f + `; return r })()`
		jitCallbackMallocs(t, r, src)
		if n := jitCallbackMallocs(t, r, src); n > 600 {
			t.Errorf("2,000 calls of %s: %d allocations", f, n)
		}
	}
}
