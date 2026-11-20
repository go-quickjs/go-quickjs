//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64))

package vm

import (
	"math"
	"testing"
	"unsafe"

	"github.com/go-quickjs/go-quickjs/internal/jit/abi"
)

// A compile in the background that is done while the code it replaces is
// run by a native call under way waits for its install (jitRegInstallWaits)
// -- kept, no longer counted among the compiles the background has room
// for -- and is installed at the first check after that call is done, not
// thrown away and asked again, built a second time and as often thrown
// away: AWFY's DeltaBlue had thrown away 77 of its 201 compiles so.
func TestJITBackgroundInstallWaitsForNativeCalls(t *testing.T) {
	r := jitBackgroundForTest(t, 1)
	e, j := jitRegPendingRecompileForTest(t, r)
	cl := jitGlobalClosureForTest(r, "sum")
	<-j.done
	s := r.jit
	// A native call of sum's code under way (regCalled).
	if len(s.ssaCtxs) == 0 {
		s.ssaCtxs = append(s.ssaCtxs, new(abi.Context))
	}
	ctx := s.ssaCtxs[0]
	top, live, called := s.ctxTop, ctx.Live, ctx.Closure
	s.ctxTop, ctx.Live, ctx.Closure = 1, abi.LiveCall, unsafe.Pointer(cl)
	reoptimized := s.reoptimized
	r.jitDrain()
	if !j.waiting || len(s.regJobs) != 1 || s.regPendingJobs[e.key] != j || s.jobsFull() || s.bgDiscarded != 0 || jitRegOfForTest(r, cl) != e {
		t.Errorf("under way: waiting %v, jobs %d, pending %v, full %v, discarded %d, replaced %v: want the compile kept, waiting, its room given back",
			j.waiting, len(s.regJobs), s.regPendingJobs[e.key] == j, s.jobsFull(), s.bgDiscarded, jitRegOfForTest(r, cl) != e)
	}
	s.ctxTop, ctx.Live, ctx.Closure = top, live, called
	jitCallForTest(t, r, `sum(10)`, 45)
	if len(s.regJobs) != 0 || s.regPendingJobs[e.key] != nil || s.bgDiscarded != 0 || s.reoptimized != reoptimized+1 || jitRegOfForTest(r, cl) == e {
		t.Errorf("done: jobs %d, pending %v, discarded %d, reoptimized %d, replaced %v: want the compile installed at the next check",
			len(s.regJobs), s.regPendingJobs[e.key] != nil, s.bgDiscarded, s.reoptimized-reoptimized, jitRegOfForTest(r, cl) != e)
	}
	jitCallForTest(t, r, `sum(100)`, 4950)
}

// A compile waiting for its install (jitRegInstallWaits) is due at a call
// of Go the code it replaces makes from its own run, where no native call
// of that code below it is under way (jitRegCompileDueNow): the run leaves
// for it, and the exit installs it (jitRegGoOnAt). Its install had been
// tried only at the checks of the calls the code made, each under way
// inside it: AWFY's DeltaBlue's constructors waited out tens of thousands
// of them, their super() calls Go's meanwhile.
func TestJITBackgroundWaitingInstallIsDue(t *testing.T) {
	r := jitBackgroundForTest(t, 1)
	e, j := jitRegPendingRecompileForTest(t, r)
	cl := jitGlobalClosureForTest(r, "sum")
	<-j.done
	s := r.jit
	run := &jitRegRun{f: &frame{cl: cl}, c: r.regOf(cl), e: e}
	if r.jitRegCompileDueNow(run, 0) {
		t.Fatal("due before its compile waits")
	}
	j.waiting = true
	e.life.reopt = true
	if !r.jitRegCompileDueNow(run, 0) {
		t.Fatal("a compile waiting for the run's code is not due at its call of Go")
	}
	// Under way below the run's context: not due.
	if len(s.ssaCtxs) < 2 {
		s.ssaCtxs = append(s.ssaCtxs, new(abi.Context), new(abi.Context))
	}
	ctx := s.ssaCtxs[0]
	top, live, called := s.ctxTop, ctx.Live, ctx.Closure
	s.ctxTop, ctx.Live, ctx.Closure = 1, abi.LiveCall, unsafe.Pointer(cl)
	due := r.jitRegCompileDueNow(run, 1)
	s.ctxTop, ctx.Live, ctx.Closure = top, live, called
	if due {
		t.Fatal("due while a native call of the code below the run is under way")
	}
	reoptimized := s.reoptimized
	jitCallForTest(t, r, `sum(10)`, 45)
	if s.regPendingJobs[e.key] != nil || s.reoptimized != reoptimized+1 || s.bgDiscarded != 0 {
		t.Errorf("pending %v, reoptimized %d, discarded %d: want the waiting compile installed", s.regPendingJobs[e.key] != nil, s.reoptimized-reoptimized, s.bgDiscarded)
	}
}

// A native caller's call Go makes of a callee whose compile in the
// background is done asks for its code (jitRegNativeCallee), and that ask
// installs it, as a call entering the callee is a check: native callers'
// calls of a callee Go makes enter none of its code, and AWFY's
// DeltaBlue called markUnsatisfied through Go 11,000 times with its
// compile done, between the checks that installed it.
//
// The test's compile is the only one: the function's own calls spend no
// budget it could compile at (jitRuntimeForTest's threshold of one had
// sum(10)'s call hand a compile of its own to the background, installed
// at whichever check came after it was done -- before the test looked on
// a slow runner, its code there), and the compile is waited for by its
// job's own signal, done, which no check installs.
func TestJITBackgroundCalleeAskInstalls(t *testing.T) {
	t.Parallel()
	r := jitBackgroundRuntime(t)
	r.jitCallThreshold, r.jitICThreshold = math.MaxUint16, math.MaxUint16
	if _, err := r.Run(compileForTest(t, `function sum(n){let s=0;for(let i=0;i<n;i++)s+=i;return s}`)); err != nil {
		t.Fatal(err)
	}
	cl := jitGlobalClosureForTest(r, "sum")
	jitCallForTest(t, r, `sum(10)`, 45)
	j := jitRegJobForTest(t, r, cl)
	r.jitRegSubmit(j)
	<-j.done
	if r.jit.regPendingJobs[j.key] != j || jitHasCodeOfForTest(r, cl) {
		t.Fatalf("pending %v, code %v: want the compile done, not installed", r.jit.regPendingJobs[j.key] == j, jitHasCodeOfForTest(r, cl))
	}
	e := r.jitRegForAs(cl, cl.reg, true)
	if e == nil || e.code == nil || r.jit.regPendingJobs[j.key] != nil || jitRegOfForTest(r, cl) != e {
		t.Fatalf("asked: entry %v, pending %v: want the compile installed and its code given", e != nil && e.code != nil, r.jit.regPendingJobs[j.key] != nil)
	}
	jitCallForTest(t, r, `sum(100)`, 4950)
}
