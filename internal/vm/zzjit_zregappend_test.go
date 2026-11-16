//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64))

package vm

import (
	"fmt"
	"runtime"
	"testing"

	"github.com/go-quickjs/go-quickjs/internal/jit/abi"
	"github.com/go-quickjs/go-quickjs/internal/jit/ssa"
)

// regUpvalueAppendSrc's methods append to a binding of their maker's,
// s += x, which the compiler marks an append (RAdd's RAddAppend): the
// method made anew at each call of its maker, inlined in the maker's loop
// or called natively, the string read between -- kept whole once, at
// turn 500, which must not change as the binding grows on -- and a number
// appended.
const regUpvalueAppendSrc = `
function appends(n) {
	var i, j, s, sum = 0, kept = "";
	var sink = { push: function (x) { return x !== null && (s += x), true; } };
	for (j = 0; j < n; j += 1024) {
		s = "";
		for (i = 0; i < 1024; i++) {
			sink.push(i === 700 ? 7 : "<x>");
			if (i === 500 && j === 0) kept = s;
		}
		sum += s.length + s.lastIndexOf("<x>");
	}
	return sum + ":" + kept.length + ":" + s.slice(2097, 2106) + ":" + kept.slice(-6);
}
`

// TestJITRegUpvalueAppendOwned runs regUpvalueAppendSrc's appends
// natively and with no JIT, the collector idle and marking, and compares
// them with the exact answer: the addition Go makes for the appends, a
// string's learned at its exits (Hints' StringAdds), grows the string the
// binding alone holds in place (ir's Owned, abi.GoOpOwned), as the
// interpreter's rsAdd does, rather than making a new string at each:
// once warm, the code's call of Go for it is marked so, and, in a build
// that allocates as an ordinary one does, a run of 64 strings of 1,024
// appends each allocates a small share of what one allocation for each
// append would.
func TestJITRegUpvalueAppendOwned(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	const n = 64 * 1024
	const want = "392768:1503:<x>7<x><x:<x><x>"
	for _, marking := range []bool{false, true} {
		t.Run(fmt.Sprintf("marking=%v", marking), func(t *testing.T) {
			if marking {
				jitMarkingForTest(t)
			}
			rts := regNativeRuntimes(t, regUpvalueAppendSrc)
			r := rts[0]
			r.SetJITWarmUp(1, 1)
			run := func() {
				t.Helper()
				var got [2]string
				for i, r := range rts {
					fn, _ := r.GetProp(Obj(r.Global()), r.Intern("appends"))
					v, err := r.Call(fn, Undefined, []Value{Float(n)})
					got[i] = regNativeShow(r, v, err)
				}
				if got[0] != got[1] || got[0] != want {
					t.Fatalf("appends: with the JIT %q, without %q, want %q", got[0], got[1], want)
				}
			}
			for range 4 {
				run()
			}
			// What follows counts what native code did, which the stress
			// modes' compiles, deoptimizations and budgets change.
			r.jitStress = jitStressConfig{}
			for range 4 {
				run()
			}
			// The code that ran it, appends' own or push's, called or
			// inlined, makes the append as one whose string Go may grow in
			// place (abi.GoOpOwned on its call of Go), in any build.
			owned := 0
			for key, e := range r.jit.regCache {
				if fn := key.Value(); fn == nil || e == nil || e.code == nil || fn.Name != "appends" && fn.Name != "push" {
					continue
				}
				for _, st := range e.code.GoCallStates() {
					if ssa.GoOp(st.Op&0xff) == ssa.GoAdd && st.Op&abi.GoOpOwned != 0 {
						owned++
					}
				}
			}
			if owned == 0 {
				t.Error("no code of appends' or push's makes the append as one that grows in place")
			}
			if r.RegNativeStats().Entries == 0 {
				t.Error("no native code entered")
			}
			// The allocations, where they are the engine's: checkptr=2,
			// CI's native boundary, moves to the heap every variable whose
			// address becomes an unsafe.Pointer, six an append here.
			skipUnlessAllocsOrdinary(t)
			fn, _ := r.GetProp(Obj(r.Global()), r.Intern("appends"))
			var before, after runtime.MemStats
			runtime.ReadMemStats(&before)
			if _, err := r.Call(fn, Undefined, []Value{Float(n)}); err != nil {
				t.Fatal(err)
			}
			runtime.ReadMemStats(&after)
			if allocs := after.Mallocs - before.Mallocs; allocs > n/8 {
				t.Errorf("%d allocations for %d appends: not grown in place", allocs, n)
			}
		})
	}
}
