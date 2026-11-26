//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64))

package vm

import (
	"testing"
)

// jitUnstartedForTest is a runtime that compiles in the background with no
// worker given its compiles (jitTestConfig's bgUnstarted), with loop, a
// function of a loop, defined: what every worker being busy leaves.
func jitUnstartedForTest(t *testing.T) *Runtime {
	t.Helper()
	r := jitBackgroundRuntime(t)
	r.jitTest.bgUnstarted = true
	// A budget a call does a fraction of: asks a few calls apart.
	r.jitCallThreshold = jitInvocationsForMaglev
	if _, err := r.Run(compileForTest(t, `function loop(n){let s=0;for(let i=0;i<n;i++)s=(s+i)|0;return s}`)); err != nil {
		t.Fatal(err)
	}
	return r
}

// A function whose compile waits in the background counts a budget again,
// its loops' back edges and its calls, rather than asking at every one: a
// back edge's check is a call of Go, which had made the interpreter slower
// while the function waited than before it was hot.
func TestJITBackgroundWaitCountsABudget(t *testing.T) {
	t.Parallel()
	r := jitUnstartedForTest(t)
	cl := jitGlobalClosureForTest(r, "loop")
	key := r.jitRegKeyOf(cl)
	for call := 1; r.jit == nil || r.jit.regPendingJobs[key] == nil; call++ {
		if call > 100 {
			t.Fatalf("no compile pending (%s)", jitBackgroundStateForTest(r, cl))
		}
		jitCallForTest(t, r, `loop(200)`, 19900)
	}
	jitCallForTest(t, r, `loop(200)`, 19900)
	cc, life := cl.closureCode, r.jitRegLifeOf(cl)
	budget := jitRegBudget(r.jitCallThreshold, cl.reg)
	if cc.jitRegEdgesSet < 2 || life.tierBudget <= 0 || life.tierBudget > budget {
		t.Fatalf("back edges armed with %d, budget left %d: want a budget of %d counted while the compile waits", cc.jitRegEdgesSet, life.tierBudget, budget)
	}
}

// A compile in the background no worker has begun by its function's second
// ask after it was handed over, each a budget spent in the interpreter, is
// taken back and made on the spot, so that a runtime with no worker free
// waits for its code no longer than about twice what compiling on its own
// goroutine costs.
func TestJITBackgroundTakeBack(t *testing.T) {
	t.Parallel()
	r := jitUnstartedForTest(t)
	cl := jitGlobalClosureForTest(r, "loop")
	key := r.jitRegKeyOf(cl)
	var j *jitRegJob
	for call := 1; ; call++ {
		before := r.JITStats().SSAEntries
		jitCallForTest(t, r, `loop(200)`, 19900)
		if r.jit == nil {
			continue
		}
		if p := r.jit.regPendingJobs[key]; p != nil {
			if j != nil && p != j {
				t.Fatalf("call %d: another compile handed over while the first waits", call)
			}
			j = p
			if jitHasCodeOfForTest(r, cl) || j.asked >= jitTakeBackAsks || call > 100 {
				t.Fatalf("call %d: code %v, %d asks: want it waiting", call, jitHasCodeOfForTest(r, cl), j.asked)
			}
			continue
		}
		if j == nil {
			if call > 100 {
				t.Fatalf("call %d: never handed over (%s)", call, jitBackgroundStateForTest(r, cl))
			}
			continue
		}
		if !jitHasCodeOfForTest(r, cl) || r.jit.bgTakenBack != 1 || j.state.Load() != jitJobDone || j.asked != jitTakeBackAsks {
			t.Fatalf("call %d: code %v, taken back %d, state %d, asks %d: want it taken back at ask %d and installed",
				call, jitHasCodeOfForTest(r, cl), r.jit.bgTakenBack, j.state.Load(), j.asked, jitTakeBackAsks)
		}
		if r.JITStats().SSAEntries == before {
			t.Fatalf("call %d: the code taken back not entered", call)
		}
		break
	}
	// A worker that comes to it after leaves it as it is.
	j.run()
	if j.state.Load() != jitJobDone || j.w != nil {
		t.Fatalf("state %d, workspace %v after a worker's run: want it left done, its workspace given back", j.state.Load(), j.w != nil)
	}
	if s := r.jit; s.bgSubmitted != 1 || len(s.regJobs) != 0 || len(s.regPendingJobs) != 0 || s.compiled != 1 {
		t.Fatalf("submitted %d, jobs %d, pending %d, compiled %d: want 1, 0, 0, 1", s.bgSubmitted, len(s.regJobs), len(s.regPendingJobs), s.compiled)
	}
	jitCallForTest(t, r, `loop(100)`, 4950)
}
