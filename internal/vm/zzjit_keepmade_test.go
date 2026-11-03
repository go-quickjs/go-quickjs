//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))

package vm

import (
	"runtime"
	"testing"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
	"github.com/go-quickjs/go-quickjs/internal/jit/ssa"
)

// A value read from a cell and used after stores that may write that
// cell is kept once, where it is made (ssa's keepAcrossStores), however
// many stores it is used after: twice's p, read in its loop and used after
// three stores of its key; straight's p, read before its loop and used
// after two stores of its key in it, kept before the loop; branchy's p,
// used after a store in one arm of an if and after another past it. None
// is kept again into its own cell, and no keep is of a copy. Each answer
// is the interpreter's, the collector marking or not, and run between
// rounds: the cell holds the pointer the keep wrote, which the collector
// finds where it scans the context.
func TestJITKeptWhereMade(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	flag := jitMarkingForTest(t)
	*flag = 0
	defer func() { *flag = 0 }()
	// The keeps of each function's last compile, by cell, and how many
	// keep a copy.
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
				if v.Args[0].Op == ssa.OpKept {
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
			t.Errorf("%s keeps %d copies again", name, rekeeps[name])
		}
	}
	for _, name := range []string{"twice", "straight", "branchy"} {
		if len(keeps[name]) == 0 {
			t.Fatalf("%s keeps nothing: the test keeps no value", name)
		}
		for cell, n := range keeps[name] {
			if n != 1 {
				t.Errorf("%s keeps cell %d %d times, want once", name, cell, n)
			}
		}
	}
}
