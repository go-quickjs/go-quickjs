//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64))

package vm

import (
	"slices"
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

// jitTaskForTest is a queued compile whose function did waited work.
type jitTaskForTest struct {
	waited int64
	ran    *[]int64
}

func (j jitTaskForTest) run()              { *j.ran = append(*j.ran, j.waited) }
func (j jitTaskForTest) waitedWork() int64 { return j.waited }

// The workers take the queued compile whose function did most work waiting
// for it, the first queued of those that did as much, so that one no longer
// run waits behind those that are.
func TestJITBackgroundQueueMostWorkFirst(t *testing.T) {
	t.Parallel()
	var ran []int64
	p := &jitWorkerPool{running: 1}
	for _, w := range []int64{3, 0, 9, 9, 1} {
		p.queue = append(p.queue, jitTaskForTest{waited: w, ran: &ran})
	}
	p.work()
	if want := []int64{9, 9, 3, 1, 0}; !slices.Equal(ran, want) {
		t.Fatalf("ran %v: want %v", ran, want)
	}
	if p.running != 0 || len(p.queue) != 0 {
		t.Fatalf("running %d, queued %d after: want none", p.running, len(p.queue))
	}
}

// A compile no worker has begun whose function went unused for an epoch,
// from one of the flush's ticks to the next, is dropped, and asked for
// again once the function is hot again; one whose function ran meanwhile
// stays queued.
func TestJITBackgroundDropsStale(t *testing.T) {
	t.Parallel()
	r := jitUnstartedForTest(t)
	tick := uint64(0)
	r.jitTest.flushNow = func() uint64 { return tick }
	cl := jitGlobalClosureForTest(r, "loop")
	key := r.jitRegKeyOf(cl)
	for call := 1; r.jit == nil || r.jit.regPendingJobs[key] == nil; call++ {
		if call > 100 {
			t.Fatalf("no compile pending (%s)", jitBackgroundStateForTest(r, cl))
		}
		jitCallForTest(t, r, `loop(200)`, 19900)
	}
	j := r.jit.regPendingJobs[key]
	look := func() {
		tick++
		r.jit.flush()
	}
	look()
	jitCallForTest(t, r, `loop(200)`, 19900)
	look()
	if r.jit.regPendingJobs[key] != j || r.jit.bgDropped != 0 {
		t.Fatalf("pending %v, dropped %d: want the compile of a function run meanwhile kept", r.jit.regPendingJobs[key] == j, r.jit.bgDropped)
	}
	look()
	if r.jit.regPendingJobs[key] != nil || len(r.jit.regJobs) != 0 || r.jit.bgDropped != 1 || j.state.Load() != jitJobAbandoned || cl.closureCode.jitEnters {
		t.Fatalf("pending %v, jobs %d, dropped %d, state %d, enters %v: want the compile dropped",
			r.jit.regPendingJobs[key] != nil, len(r.jit.regJobs), r.jit.bgDropped, j.state.Load(), cl.closureCode.jitEnters)
	}
	for call := 1; r.jit.regPendingJobs[key] == nil; call++ {
		if call > 100 {
			t.Fatal("not handed over again")
		}
		jitCallForTest(t, r, `loop(200)`, 19900)
	}
	if r.jit.bgSubmitted != 2 {
		t.Fatalf("submitted %d: want 2", r.jit.bgSubmitted)
	}
}

// A compile in the background is placed by its worker, apart from the code
// the runtime runs meanwhile, and installed as that code; one thrown away
// at its install has it closed.
func TestJITBackgroundPlacesOnTheWorker(t *testing.T) {
	t.Parallel()
	r := jitBackgroundForTest(t, 1)
	e, j := jitRegPendingRecompileForTest(t, r)
	cl := jitGlobalClosureForTest(r, "sum")
	<-j.done
	placed := j.placed
	if placed == nil || len(placed.Text()) == 0 {
		t.Fatalf("placed %v: want the code placed by its worker (err %v)", placed != nil, j.err)
	}
	jitCallForTest(t, r, `sum(10)`, 45)
	if ne := jitRegOfForTest(r, cl); ne == e || ne == nil || ne.code != placed || j.placed != nil {
		t.Fatalf("installed %v, its code the worker's %v: want the placed code installed", ne != e, ne != nil && ne.code == placed)
	}
	_, j = jitRegPendingRecompileForTest(t, r)
	<-j.done
	placed = j.placed
	if placed == nil {
		t.Fatal("not placed by its worker")
	}
	jitRegOfForTest(r, cl).life.deopts++
	jitCallForTest(t, r, `sum(10)`, 45)
	if r.jit.bgDiscarded != 1 || len(placed.Text()) != 0 {
		t.Fatalf("discarded %d, placed code open %v: want it thrown away and closed", r.jit.bgDiscarded, len(placed.Text()) != 0)
	}
}

// A runtime that compiles in the background where GOMAXPROCS is more than 1
// (SetJITBackgroundAuto, WithJIT's default) compiles on the spot where it is
// 1, decided at each compile: with one P, a worker runs only when the
// runtime's goroutine is preempted, which native code never is.
func TestJITBackgroundAutoOneP(t *testing.T) {
	t.Parallel()
	for _, procs := range []int{1, 4} {
		r := jitRuntimeForTest(t, Config{JIT: true})
		r.SetJITBackgroundAuto()
		r.jitTest.procs = procs
		if _, err := r.Run(compileForTest(t, `function loop(n){let s=0;for(let i=0;i<n;i++)s=(s+i)|0;return s}`)); err != nil {
			t.Fatal(err)
		}
		cl := jitGlobalClosureForTest(r, "loop")
		jitCallForTest(t, r, `loop(1000)`, 499500)
		if got, want := r.JITBackground(), procs > 1; got != want || r.jit.bgSubmitted == 0 != !want {
			t.Fatalf("GOMAXPROCS %d: in the background %v, handed over %d: want %v", procs, got, r.jit.bgSubmitted, want)
		}
		if procs == 1 && !jitHasCodeOfForTest(r, cl) {
			t.Fatalf("GOMAXPROCS 1: no code compiled on the spot (%s)", jitBackgroundStateForTest(r, cl))
		}
	}
}
