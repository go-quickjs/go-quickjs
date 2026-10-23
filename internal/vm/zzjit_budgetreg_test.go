//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))

package vm

import (
	"fmt"
	"testing"
	"weak"
)

// A loop that calls nothing counts its back-edge budget in a register
// (mir's budget), stored back wherever Go may read it: Go measures the
// work native code did by the budget it spent (jitSSAProfit), the loop's
// length a back edge, and gives a new one at each poll. So the work Go
// counts for a call of such a loop, entered at its start, is its back
// edges' -- as it was with the budget in memory -- whatever the loop's
// exit (its end, a return from its body, a failed guard), polls and calls
// of Go (a remainder of floats, Go's) inside it or not, and under a
// budget of a few back edges, every poll an exit.
func TestJITBudgetInRegisterCounted(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	const setup = `
		function sum(n) { let s = 0; for (let i = 0; i < n; i++) s = (s + i) | 0; return s }
		function find(a, x) { for (let i = 0; i < a.length; i++) if (a[i] === x) return i; return -1 }
		function rem(n) { let s = 0; for (let i = 0; i < n; i++) s += (i + 0.5) % 3.5; return s }
		function total(a) { let s = 0; for (let i = 0; i < a.length; i++) s += a[i]; return s }
		const arr = []; for (let i = 0; i < 5000; i++) arr.push(i)
		const odd = arr.slice(); odd.push("x")`
	for _, budget := range []uint16{0, 7} {
		t.Run(fmt.Sprintf("budget %d", budget), func(t *testing.T) {
			r := jitRuntimeForTest(t, Config{JIT: true})
			// Whatever QJS_JIT_STRESS says: its deoptimizations finish
			// invocations in the interpreter, which this counts against.
			r.jitStress = jitStressConfig{}
			if budget != 0 {
				r.jitStress = jitStressConfig{threshold: true, budget: budget}
			}
			if _, err := r.Run(compileForTest(t, setup)); err != nil {
				t.Fatal(err)
			}
			for _, tc := range []struct {
				name, warm, run string
				edges           uint64
			}{
				{"sum", "sum(50)", "sum(20000)", 20000},
				{"find", "find(arr, 3)", "find(arr, 4000)", 4000},
				{"rem", "rem(50)", "rem(20000)", 20000},
				// Its last element no number: a guard fails, the loop left
				// at an exit.
				{"total", "total(arr)", "total(odd)", 5000},
			} {
				for range 3 {
					if _, err := r.Run(compileForTest(t, tc.warm)); err != nil {
						t.Fatal(err)
					}
				}
				e := r.jit.cache[weak.Make(r.global.getOwn(r.atoms.intern(tc.name)).value().Object().fn().closure.fn)]
				if e == nil || e.ssa == nil {
					t.Fatalf("%s has no native code", tc.name)
				}
				before := e.ssaStats.work
				if _, err := r.Run(compileForTest(t, tc.run)); err != nil {
					t.Fatal(err)
				}
				if work, want := e.ssaStats.work-before, tc.edges*uint64(e.ssaLoop)*9/10; work < want {
					t.Errorf("%s: Go counted %d of native work for %d back edges of a loop of %d, want %d or more", tc.name, work, tc.edges, e.ssaLoop, want)
				}
			}
		})
	}
}

// Many functions compiled in one runtime, as microbench's runner does,
// each tiered up by the budget its loops spend: all of them native in the
// end, and the budget the interpreter counts never past what Go gives. A
// generator's loop counts its budget in a register (its loop is its header
// alone: the rest of its turn is entered from Go at its yield's resumption)
// and its back edge comes from outside the loop -- the counter in memory,
// then loaded into the register; its poll, Go's, had gone back to the
// header, past the load, and the register a float the entry block had held
// was stored as the budget: 4627167142139895302 back edges to go, every
// later loop of the runtime run in the tree tier, never compiled, never
// interrupted.
func TestJITBudgetAcrossManyFunctions(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	const setup = `
		function* gen(n) { for (let i = 0; i < n; i++) yield i }
		function forOf(n) { let s = 0; for (const x of gen(n)) s += x; return s }
		function sum(n) { let s = 0; for (let i = 0; i < n; i++) s = (s + i) | 0; return s }
		function tryCatch(n) { let s = 0; for (let i = 0; i < n; i++) { try { s = (s + i) | 0 } catch (e) { s = 0 } } return s }
		function props(n) { const o = { a: 1, b: 2 }; for (let i = 0; i < n; i++) { o.a = i; o.b = o.a + 1 } return o.b }
		function elems(n) { const a = [1, 2, 3]; let s = 0; for (let i = 0; i < n; i++) s += a[i % 3]; return s }
		function down(n) { let s = 0; while (n-- > 0) s++; return s }`
	r := New(Config{JIT: true})
	t.Cleanup(func() { r.Close(); r.ReleaseClosed() })
	r.jitStress = jitStressConfig{}
	if _, err := r.Run(compileForTest(t, setup)); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"forOf", "sum", "tryCatch", "props", "elems", "down"} {
		for range 3 {
			if _, err := r.Run(compileForTest(t, name+"(300000)")); err != nil {
				t.Fatal(err)
			}
			if r.backEdges > 2*backEdgeCheckInterval {
				t.Fatalf("after %s: %d back edges to go, past any budget Go gives", name, r.backEdges)
			}
		}
		e := r.jit.cache[weak.Make(r.global.getOwn(r.atoms.intern(name)).value().Object().fn().closure.fn)]
		if e == nil || e.ssa == nil {
			t.Errorf("%s was never compiled", name)
		}
	}
}
