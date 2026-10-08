package vm

import (
	"sync"
	"testing"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
)

// flowShapes are functions f whose control flow structureTree rebuilds
// whole: each kind of loop, and if, else, break and continue in them.
var flowShapes = []string{
	`function f(n) { for (var i = 0; i < n; i++) {} return i }`,
	`function f(a, n) { var s = 0, i = 0; while (i < n) { s += a[i]; i++ } return s }`,
	`function f(n) { var i = 0; do { i++ } while (i < n); return i }`,
	`function f(n) { var s = 0; for (var i = 0; i < n; i++) { if (i & 1) continue; s += i } return s }`,
	`function f(n) { var s = 0; for (var i = 0; i < n; i++) { if (i & 1) s += i; else s -= 1 } return s }`,
	`function f(n) { var s = 0; for (var i = 0; i < n; i++) for (var j = 0; j < n; j++) s += j; return s }`,
	`function f(n) { var s = 0, i, j; outer: for (i = 0; i < n; i++) { for (j = 0; j < n; j++) { if (j == 3) continue outer; if (i == 5) break outer; s++ } } return s }`,
	`function f(n) { var i = 0; while (true) { i++; if (i > n) break } return i }`,
	`function f(n) { for (;;) { for (;;) { n--; if (n < 5) break } if (n < 0) break; n -= 2 } return n }`,
	`function f(n) { var s = 0; for (var i = 0; i < n; i++) { if (i > 2 && i < 8) { for (var j = 0; j < i; j++) s++ } else { s-- } } return s }`,
	`function f(n) { var s = 0; for (var i = 0; i < n; i++) { if (i == 7) return s; s += i } return -1 }`,
	`function f(n) { var r = 0; for (var i = 0; i < n; i++) { switch (i % 3) { case 0: r++; case 1: r += 2; break; default: r-- } } return r }`,
}

// flowFunc compiles src and gives its function f.
func flowFunc(t *testing.T, src string) *bytecode.Function {
	t.Helper()
	for _, c := range compileForTest(t, src).Constants {
		if c.Fn != nil && c.Fn.Name == "f" {
			return c.Fn
		}
	}
	t.Fatalf("%s: no function f", src)
	return nil
}

// TestTreeFlowStructures pins that each of flowShapes has its loops rebuilt
// whole: no block inside a loop is left to runTree's dispatch, and no block
// left goes back to itself.
func TestTreeFlowStructures(t *testing.T) {
	if !treeTier.Load() {
		t.Skip("the tree tier is off")
	}
	var src string
	seen := false
	flowSeen = func(f *flow, _ *tree) {
		seen = true
		for i, nd := range f.nodes {
			if !nd.dead && (nd.level >= 0 || has(nd.succ, i)) {
				t.Errorf("%s: block %d, in the loop at %d, is left to dispatch", src, i, nd.level)
			}
		}
	}
	defer func() { flowSeen = nil }()
	for _, src = range flowShapes {
		seen = false
		if buildTree(flowFunc(t, src)) == nil {
			t.Errorf("%s: not built", src)
		} else if !seen {
			t.Errorf("%s: not structured", src)
		}
	}
}

// TestTreeFlowCostlyLeftAlone pins which rebuilt blocks are kept: a loop
// whose body lowers to statements, simple ifs and loops is rebuilt; one
// whose body needs arms chosen by runArms -- an && in a test, an else-if
// chain, a switch, a continue to an outer loop -- is left to runTree's
// dispatch, which costs less than such a structure.
func TestTreeFlowCostlyLeftAlone(t *testing.T) {
	if !treeTier.Load() {
		t.Skip("the tree tier is off")
	}
	kept := -1
	flowKept = func(n int) { kept = n }
	defer func() { flowKept = nil }()
	for _, c := range []struct {
		src  string
		kept bool
	}{
		{`function f(n) { for (var i = 0; i < n; i++) {} return i }`, false},
		{`function f(n) { var s = 0; for (var i = 0; i < n; i++) { if (i & 1) s += i; else s -= 1 } return s }`, false},
		{`function f(n) { var s = 0; for (var i = 0; i < n; i++) for (var j = 0; j < n; j++) s += j; return s }`, false},
		{`function f(n) { var s = 0; for (var i = 0; i < n; i++) { if (i == 7) return s; s += i } return -1 }`, false},
		{`function f(a, b) { var s = 0; for (var i = 0; i < a.length; i++) { if (a[i] > 0 && b[i] > 0) s++ } return s }`, true},
		{`function f(a) { var s = 0; for (var i = 0; i < a.length; i++) { if (a[i] == 1) s++; else if (a[i] == 2) s += 2; else s-- } return s }`, true},
		{`function f(n) { var r = 0; for (var i = 0; i < n; i++) { switch (i % 3) { case 0: r++; case 1: r += 2; break; default: r-- } } return r }`, true},
	} {
		kept = -1
		if buildTree(flowFunc(t, c.src)) == nil {
			t.Fatalf("%s: not built", c.src)
		}
		if kept < 0 {
			t.Errorf("%s: not structured", c.src)
		} else if (kept > 0) != c.kept {
			t.Errorf("%s: %d blocks kept as they were, want kept %v", c.src, kept, c.kept)
		}
	}
}

// TestTreeFlowStraightCode pins that a function without loops is left to
// runTree's dispatch, without the structuring so much as looking at it:
// outside a loop a structure costs a call more than it saves.
func TestTreeFlowStraightCode(t *testing.T) {
	if !treeTier.Load() {
		t.Skip("the tree tier is off")
	}
	seen := false
	flowSeen = func(*flow, *tree) { seen = true }
	defer func() { flowSeen = nil }()
	for _, src := range []string{
		`function f(x) { if (x & 1) return 1; else return 2 }`,
		`function f(x) { var k = x & 7; if (k == 0) return 10; else if (k == 1) return 11; return 14 }`,
		`function f(x) { if ((x & 1) && (x & 2)) return 1; return 0 }`,
		`function f(x) { switch (x & 3) { case 0: return 1; case 1: return 2; default: return 4 } }`,
	} {
		seen = false
		if buildTree(flowFunc(t, src)) == nil {
			t.Fatalf("%s: not built", src)
		}
		if seen {
			t.Errorf("%s: structured", src)
		}
	}
}

// TestTreeFlowBackEdges pins that a structured loop counts the back edges
// the interpreter counts, which decide when the interrupt check runs.
func TestTreeFlowBackEdges(t *testing.T) {
	defer SetTreeTier(true)
	for _, src := range flowShapes {
		var left [2]int
		var res [2]Value
		for i, on := range []bool{false, true} {
			SetTreeTier(on)
			r := New(Config{})
			v, err := r.Run(compileForTest(t, src+"; f(12)"))
			if err != nil {
				t.Fatalf("%s: %v", src, err)
			}
			left[i], res[i] = r.backEdges, v
			r.Close()
		}
		if left[0] != left[1] || !res[0].StrictEquals(res[1]) {
			t.Errorf("%s: interpreter %v with %d back edges left, tree %v with %d", src, res[0], left[0], res[1], left[1])
		}
	}
}

// TestTreeFlowConcurrentBuilds builds the trees of flowShapes's functions,
// one compiled copy shared, from several goroutines at once, as runtimes on
// several goroutines build the trees of a program they share: the flows
// they take from flowPool, and give back, must not be shared between them.
// Each build must give the structures one build alone gives; run it with
// -race.
func TestTreeFlowConcurrentBuilds(t *testing.T) {
	if !treeTier.Load() {
		t.Skip("the tree tier is off")
	}
	var fns []*bytecode.Function
	want := map[*bytecode.Function]int{}
	for _, src := range flowShapes {
		fn := flowFunc(t, src)
		fns = append(fns, fn)
		want[fn] = len(buildTree(fn).blocks)
	}
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 50; i++ {
				for _, fn := range fns {
					if tr := buildTree(fn); tr == nil || len(tr.blocks) != want[fn] {
						t.Errorf("a concurrent build of %s differs", fn.Name)
						return
					}
				}
			}
		}()
	}
	wg.Wait()
}
