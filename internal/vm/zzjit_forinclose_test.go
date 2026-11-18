//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64))

package vm

import (
	"runtime"
	"runtime/debug"
	"testing"
)

// forInCloseSetup leaves for...in loops early every way: a break, a
// break of an inner loop whose outer loop goes on, a continue of an outer
// loop from an inner one, a return, a loop over an array's indices, and a
// generator's loop its for...of breaks out of -- each followed by loops
// that take the cursor given back and must walk their own object from its
// first key.
const forInCloseSetup = `var o = { a: 1, b: 2, c: 3, d: 4 }, arr = [5, 6, 7]
function brk(n) { var c = 0; for (var j = 0; j < n; j++) { for (var k in o) { c++; break } } return c }
function nested(o) { var s = ''; for (var k in o) { for (var l in o) { if (l === 'c') break; s += k + l } s += ';' } return s }
function cont(o) { var s = ''; outer: for (var k in o) { for (var l in o) { if (l === k) continue outer; s += l } } return s }
function ret(o) { for (var k in o) { if (k === 'c') return k } return '-' }
function twice(o) { var s = ''; for (var k in o) { s += k; break } for (var k in o) { s += k } for (var i in arr) { s += i; if (i === '1') break } for (var i in arr) s += arr[i]; return s }
function gen(o) { function* g() { for (var k in o) { yield k } } var s = ''; for (var x of g()) { s += x; if (x === 'b') break } for (var x of g()) s += x; return s }
function rounds() { return [brk(50), nested(o), cont(o), ret(o), twice(o), gen(o)].join(' | ') }
`

// forInCloseWant is rounds()'s answer, Node's (v26.10.0).
const forInCloseWant = "50 | aaab;babb;cacb;dadb; | aababc | c | aabcd01567 | ababcd"

// A for...in loop native code leaves early -- by a break, a continue of an
// outer loop, a return -- marks its cursor done (ssa.OpForInDone), as the
// interpreter's iter_close does, so that the next loop takes it rather than
// making one (newForInObject). Native code had left the cursor as it was:
// for_in_break made a 160-byte cursor at every loop, 2.5 times as slow with
// the JIT as without. Every answer Node's and the interpreter's, idle and
// while the collector marks, and a thousand loops left at their first key
// all take the one cursor.
func TestJITForInClose(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	var allocs [2]uint64
	for k, marking := range []bool{false, true} {
		if marking {
			jitMarkingForTest(t)
		}
		var rounds []string
		for range 30 {
			rounds = append(rounds, "rounds()", "brk(200)")
		}
		r := jitAgreesForTest(t, forInCloseSetup, rounds...)
		v, err := r.Run(compileForTest(t, "rounds()"))
		if err != nil {
			t.Fatal(err)
		}
		if got := v.String().Go(); got != forInCloseWant {
			t.Errorf("marking %v: rounds() = %s, want Node's %s", marking, got, forInCloseWant)
		}
		e := jitNativeForTest(t, r, "brk")
		st := e.ssaStats
		prog := compileForTest(t, "''+brk(1000)")
		runtime.GC()
		gc := debug.SetGCPercent(-1)
		var m0, m1 runtime.MemStats
		runtime.ReadMemStats(&m0)
		v, err = r.Run(prog)
		runtime.ReadMemStats(&m1)
		debug.SetGCPercent(gc)
		if err != nil || v.String().Go() != "1000" {
			t.Fatalf("marking %v: brk(1000) = %v, %v", marking, v, err)
		}
		if e.ssaStats.entries == st.entries || e.ssaStats.hosts != st.hosts || e.ssaStats.guards != st.guards || e.ssaStats.ended != st.ended {
			t.Errorf("marking %v: brk(1000) did not run natively throughout: %+v, before %+v", marking, e.ssaStats, st)
		}
		allocs[k] = m1.Mallocs - m0.Mallocs
	}
	// The thousand loops' cursors had been a thousand objects (1,011 in
	// all); counted where the build allocates as an ordinary one does.
	skipUnlessAllocsOrdinary(t)
	for k, n := range allocs {
		if n > 100 {
			t.Errorf("marking %v: brk(1000) allocated %d objects; want its loops to take one cursor", k == 1, n)
		}
	}
}
