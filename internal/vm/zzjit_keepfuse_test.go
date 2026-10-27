//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))

package vm

import (
	"runtime"
	"testing"
)

// A value read from a cell a store then overwrites is kept (ssa's
// keep.go), its keep reading its pointer word itself (fuseKeepRefs) --
// fused's p, read from o.a before o.a is written -- or, where another keep
// of the store writes the cell it is read from first, before them all --
// swapped's p and q, each kept round the loop in the cell the other's keep
// writes. Each answer is the interpreter's, the collector marking or not,
// and run between rounds.
func TestJITKeepsReadTheirPointerWords(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	flag := jitMarkingForTest(t)
	*flag = 0
	defer func() { *flag = 0 }()
	setup := `
		function mk(k) { return { a: { v: k }, b: { v: k + 1 }, c: { v: k + 2 } } }
		function fused(o, n) { let s = 0; for (let i = 0; i < n; i++) { const p = o.a, q = o.c; o.a = o.b; o.b = q; o.c = p; s = (s + p.v * 3 + q.v) | 0 } return s + o.a.v * 1000 + o.c.v * 100 }
		function swapped(o, n) { let p = o.a, q = o.b; for (let i = 0; i < n; i++) { o.a = q; o.b = p; const t = p; p = q; q = t } return p.v + q.v * 10 + o.a.v * 100 + o.b.v * 1000 }
		function picked(o, n) { let p = o.a, q = o.b; for (let i = 0; i < n; i++) { const t = i & 1 ? p : q; o.a = p; o.b = q; p = t } return p.v + q.v * 10 + o.a.v * 100 }`
	both := jitBothForTest(t, setup)
	for i := range 24 {
		if i >= 12 {
			// Native code's stores and keeps through Go's write barrier.
			*flag = uint8(i & 1)
		}
		both.agree(t, `fused(mk(1), 7)`, `swapped(mk(3), 9)`, `picked(mk(5), 11)`)
		runtime.GC()
	}
	*flag = 0
	for _, name := range []string{"fused", "swapped", "picked"} {
		jitNativeForTest(t, both.jit, name)
	}
}
