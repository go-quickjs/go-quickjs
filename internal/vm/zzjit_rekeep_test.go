//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))

package vm

import (
	"runtime"
	"testing"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
	"github.com/go-quickjs/go-quickjs/internal/jit/ssa"
)

// A value read from a cell and used after several stores that may write
// that cell is kept once, at the first, in its cell, and the stores after
// it take that keep's copy: their keeps of it, which would write the cell
// the word it holds, are dropped (ssa's dropRekeeps) -- twice's p, read in
// its loop and kept at three stores of the same key, and straight's p,
// read before its loop and kept at two stores of its key in it. A keep
// the first does not dominate stays: branchy's p, kept in one arm of an if
// and again after it, where the copy that reaches is a phi of the two.
// Each answer is the interpreter's, the collector marking or not, and run
// between rounds: a dropped keep's cell holds the pointer the first keep
// wrote, which the collector finds where it scans the context.
func TestJITRekeepsDropped(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	flag := jitMarkingForTest(t)
	*flag = 0
	defer func() { *flag = 0 }()
	// The keeps of each function's last compile, by cell.
	keeps := map[string]map[int]int{}
	rekeeps := map[string]int{}
	jitSSAOptimized = func(fn *bytecode.Function, f *ssa.Func) {
		byCell := map[int]int{}
		rekeeps[fn.Name] = 0
		for _, b := range f.Blocks {
			for _, v := range b.Values {
				if v.Op != ssa.OpKeep {
					continue
				}
				byCell[v.Index]++
				if a := v.Args[0]; a.Op == ssa.OpKept && a.Args[0].Op == ssa.OpKeep && a.Args[0].Index == v.Index {
					rekeeps[fn.Name]++
				}
			}
		}
		keeps[fn.Name] = byCell
	}
	defer func() { jitSSAOptimized = nil }()
	setup := `
		function mk(k) { return { a: { v: k }, b: { v: k + 1 }, c: { v: k + 2 }, x: 0, y: 0 } }
		function twice(o, n) {
			let s = 0
			for (let i = 0; i < n; i++) { const p = o.a; o.a = o.b; o.a = o.c; o.c = o.b; o.b = p; s = (s + p.v + o.a.v) | 0 }
			return s + o.a.v * 100 + o.b.v * 1000
		}
		function straight(o, n) {
			const p = o.a
			let s = 0
			for (let i = 0; i < n; i++) { o.a = o.c; o.x = i; o.a = o.b; s = (s + p.v + o.x + o.a.v) | 0 }
			return s + p.v * 100
		}
		function branchy(o, n) {
			let s = 0
			for (let i = 0; i < n; i++) { const p = o.a; if (i & 1) o.a = o.b; o.a = o.c; o.b = p; o.c = p; s = (s + p.v + o.a.v) | 0 }
			return s + o.a.v * 100
		}`
	both := jitBothForTest(t, setup)
	for i := range 24 {
		if i >= 12 {
			// Native code's stores and keeps through Go's write barrier.
			*flag = uint8(i & 1)
		}
		both.agree(t, `twice(mk(1), 9)`, `straight(mk(2), 7)`, `branchy(mk(4), 9)`, `branchy(mk(6), 10)`)
		runtime.GC()
	}
	*flag = 0
	for _, name := range []string{"twice", "straight", "branchy"} {
		jitNativeForTest(t, both.jit, name)
		if rekeeps[name] != 0 {
			t.Errorf("%s keeps %d copies again into their own cells", name, rekeeps[name])
		}
	}
	for _, name := range []string{"twice", "straight"} {
		if len(keeps[name]) == 0 {
			t.Fatalf("%s keeps nothing: the test keeps no value", name)
		}
		for cell, n := range keeps[name] {
			if n != 1 {
				t.Errorf("%s keeps cell %d %d times, want once", name, cell, n)
			}
		}
	}
	twiceKept := false
	for _, n := range keeps["branchy"] {
		twiceKept = twiceKept || n == 2
	}
	if !twiceKept {
		t.Errorf("branchy's p is not kept both in its if and after it: %v", keeps["branchy"])
	}
}
