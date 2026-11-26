//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64))

package vm

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
	"github.com/go-quickjs/go-quickjs/internal/jit/mir"
)

// jitBackgroundForTest is a runtime that compiles a function at its first
// call, in the background, each compile installed at the checks'th check
// after it was handed over (QJS_JIT_STRESS's background=K), with sum
// defined.
func jitBackgroundForTest(t *testing.T, checks uint16) *Runtime {
	t.Helper()
	r := jitBackgroundRuntime(t)
	r.jitStress = jitStressConfig{background: true, checks: checks}
	if _, err := r.Run(compileForTest(t, `function sum(n){let s=0;for(let i=0;i<n;i++)s+=i;return s}`)); err != nil {
		t.Fatal(err)
	}
	return r
}

// jitBackgroundStateForTest says what decides whether r compiles cl's
// function in the background (jitInBackground), for a test's failure.
func jitBackgroundStateForTest(r *Runtime, cl *closure) string {
	return fmt.Sprintf("background %v, stress %+v, compiling %d, timing %v, repeat %d, SSA hook %v, jobs %d, top level %v",
		r.jitBackground, r.jitStress, r.jit.compiling, mir.Timing != nil, r.jitCompileRepeat(), r.jitTest.ssaOptimized != nil, len(r.jit.regJobs), cl.fn.TopLevel)
}

// jitCallForTest runs src, a script, and checks its value is want.
func jitCallForTest(t *testing.T, r *Runtime, src string, want float64) {
	t.Helper()
	v, err := r.Run(compileForTest(t, src))
	if err != nil {
		t.Fatal(err)
	}
	if got := v.Number(); got != want {
		t.Fatalf("%s = %v, want %v", src, got, want)
	}
}

// A function due to be compiled in the background runs in the interpreter
// while it is lowered there, and then compiled there, until its code is
// installed, at the Kth check after its compile was handed over -- a call
// of it is one (jitRegEnter), and so is a sample at its loop's back edge
// (jitRegHot) -- and natively from then on.
func TestJITBackgroundFirstCompile(t *testing.T) {
	t.Parallel()
	const k = 3
	r := jitBackgroundForTest(t, k)
	cl := jitGlobalClosureForTest(r, "sum")
	// Register code is lowered on the runtime's goroutine, its compile
	// handed over at once (jitRegCompile): pending, run in the existing
	// tiers, until it is installed at the Kth check (jitRegDrainJobs).
	key := r.jitRegKeyOf(cl)
	var j *jitRegJob
	for call := 1; ; call++ {
		before := r.JITStats().SSAEntries
		jitCallForTest(t, r, `sum(10)`, 45)
		native, code := r.JITStats().SSAEntries != before, jitHasCodeOfForTest(r, cl)
		if p := r.jit.regPendingJobs[key]; p != nil {
			j = p
			if code || native || j.checks >= k {
				t.Fatalf("call %d: code %v, native %v, %d checks: want the compile pending, run in the existing tiers", call, code, native, j.checks)
			}
			continue
		}
		if j == nil {
			t.Fatalf("call %d: code %v, native %v, its compile never seen pending (%s)", call, code, native, jitBackgroundStateForTest(r, cl))
		}
		if j.checks != k || !code || !native {
			t.Fatalf("call %d: its compile's checks %d, code %v, native %v: want the code installed at check %d and run", call, j.checks, code, native, k)
		}
		break
	}
	if s := r.jit; s.bgSubmitted != 1 || s.bgDiscarded != 0 || len(s.regJobs) != 0 || len(s.regPendingJobs) != 0 || s.compiled != 1 {
		t.Fatalf("submitted %d, discarded %d, jobs %d, pending %d, compiled %d: want 1, 0, 0, 0, 1",
			s.bgSubmitted, s.bgDiscarded, len(s.regJobs), len(s.regPendingJobs), s.compiled)
	}
}

// Code compiled again in the background runs on, valid, until its
// replacement is installed, which replaces it then.
func TestJITBackgroundRecompileRunsOldCode(t *testing.T) {
	t.Parallel()
	r := jitBackgroundForTest(t, 2)
	e, j := jitRegPendingRecompileForTest(t, r)
	cl := jitGlobalClosureForTest(r, "sum")
	old := e.code
	before, reoptimized := r.JITStats().SSAEntries, r.jit.reoptimized
	jitCallForTest(t, r, `sum(10)`, 45)
	if r.jit.regPendingJobs[e.key] != j || jitRegOfForTest(r, cl) != e || e.code != old || r.JITStats().SSAEntries == before {
		t.Fatalf("pending %v, same code %v, native %v: want the old code run while the new compiles", r.jit.regPendingJobs[e.key] != nil, jitRegOfForTest(r, cl) == e, r.JITStats().SSAEntries != before)
	}
	jitCallForTest(t, r, `sum(10)`, 45)
	if r.jit.regPendingJobs[e.key] != nil || jitRegOfForTest(r, cl) == e || r.jit.reoptimized != reoptimized+1 {
		t.Fatalf("pending %v, same code %v, reoptimized %d: want the new code installed", r.jit.regPendingJobs[e.key] != nil, jitRegOfForTest(r, cl) == e, r.jit.reoptimized-reoptimized)
	}
}

// A compile again whose function deoptimized since it was built is thrown
// away at its install, the code it would replace kept: built from what
// failed, it would fail the same guard again.
func TestJITBackgroundDiscardsAfterDeopt(t *testing.T) {
	t.Parallel()
	r := jitBackgroundForTest(t, 1)
	e, _ := jitRegPendingRecompileForTest(t, r)
	cl := jitGlobalClosureForTest(r, "sum")
	old := e.code
	e.life.deopts++
	jitCallForTest(t, r, `sum(10)`, 45)
	if r.jit.regPendingJobs[e.key] != nil || jitRegOfForTest(r, cl) != e || e.code != old || r.jit.bgDiscarded != 1 || len(r.jit.regJobs) != 0 {
		t.Fatalf("pending %v, same code %v, discarded %d, jobs %d: want the compile thrown away", r.jit.regPendingJobs[e.key] != nil, jitRegOfForTest(r, cl) == e && e.code == old, r.jit.bgDiscarded, len(r.jit.regJobs))
	}
	if e.busy != 0 || r.jit.regRunning(e) {
		t.Fatalf("busy %d, in use %v after the compile was thrown away", e.busy, r.jit.regRunning(e))
	}
}

// A runtime closed with compiles in the background lets them go: its
// entries wait for none, and the workers, once done, end.
func TestJITBackgroundClose(t *testing.T) {
	r := jitBackgroundForTest(t, 100)
	e, j := jitRegPendingRecompileForTest(t, r)
	s := r.jit
	r.Close()
	r.ReleaseClosed()
	if len(s.regJobs) != 0 || len(s.regPendingJobs) != 0 || e.busy != 0 {
		t.Fatalf("jobs %d, pending %d, busy %d after the runtime closed", len(s.regJobs), len(s.regPendingJobs), e.busy)
	}
	<-j.done
	if st := j.state.Load(); st != jitJobAbandoned && st != jitJobDone {
		t.Fatalf("state %d: want abandoned or done", st)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		jitWorkers.mu.Lock()
		running := jitWorkers.running
		jitWorkers.mu.Unlock()
		if running == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d workers still running", running)
		}
		time.Sleep(time.Millisecond)
	}
}

// A panic of the compiler's on a worker is a refusal, counted as the
// backend's, not the end of the process; the code it would have replaced
// runs on.
func TestJITBackgroundWorkerPanic(t *testing.T) {
	r := jitBackgroundForTest(t, 0)
	cl := jitGlobalClosureForTest(r, "sum")
	for range 2 {
		jitCallForTest(t, r, `sum(10)`, 45)
	}
	old := jitRegOfForTest(r, cl)
	if old == nil || old.code == nil {
		t.Fatalf("sum has no code: %+v", old)
	}
	code := old.code
	j := jitRegJobForTest(t, r, cl)
	// A function the build cannot take: no feedback to build from.
	j.snap = nil
	r.jitRegSubmit(j)
	if !j.panicked || r.jit.backendPanics != 1 || r.jit.regPendingJobs[j.key] != nil || jitRegOfForTest(r, cl) != old || old.code != code {
		t.Fatalf("panicked %v, backend panics %d, pending %v, same code %v", j.panicked, r.jit.backendPanics, r.jit.regPendingJobs[j.key] != nil, jitRegOfForTest(r, cl) == old && old.code == code)
	}
	r.jit.backendPanics = 0
	jitWorkerPanics.Add(-1)
	jitCallForTest(t, r, `sum(100)`, 4950)
}

// Without the stress's schedule, a compile is installed at the first
// check after its worker is done: in a while, the answers right
// meanwhile.
func TestJITBackgroundUnscheduled(t *testing.T) {
	r := jitBackgroundRuntime(t)
	if _, err := r.Run(compileForTest(t, `function sum(n){let s=0;for(let i=0;i<n;i++)s+=i;return s}`)); err != nil {
		t.Fatal(err)
	}
	cl := jitGlobalClosureForTest(r, "sum")
	for deadline := time.Now().Add(10 * time.Second); ; {
		jitCallForTest(t, r, `sum(10)`, 45)
		if jitHasCodeOfForTest(r, cl) && r.jit.regPendingJobs[r.jitRegKeyOf(cl)] == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no code installed in 10 s")
		}
	}
	before := r.JITStats().SSAEntries
	jitCallForTest(t, r, `sum(1000)`, 499500)
	if r.JITStats().SSAEntries == before {
		t.Fatal("installed code not run")
	}
}

// A loop's code installed at the loop's own check is entered there, by
// on-stack replacement, as code compiled on the spot is (jitRegLoop),
// though the function had no code when it was called, its compile
// pending. Each answer is the interpreter's, the compile installed at any
// check.
func TestJITBackgroundInstallAtNestedLoop(t *testing.T) {
	const src = `
		function plain(a, b) { return a * 3 + b }
		function loop(n, late) {
			let s = 0
			for (let i = 0; i < n; i++) {
				s = (s + plain(i, 1)) | 0
				if (i >= late) s = (s + plain(i, 2)) | 0
			}
			return s
		}
		const log = []
		for (let r = 0; r < 12; r++) log.push(loop(400, r < 6 ? 1000 : 200))
		log.length + '|' + log.join()`
	want := newTestRuntime(t, Config{})
	defer func() { want.Close(); want.ReleaseClosed() }()
	expect, _ := jitRunWithin(t, want, src, 10*time.Second)
	for k := uint16(0); k <= 12; k++ {
		r := jitBackgroundRuntime(t)
		r.jitStress = jitStressConfig{threshold: true, background: true, checks: k}
		if got, _ := jitRunWithin(t, r, src, 10*time.Second); got != expect {
			t.Errorf("installed at check %d: %q, want %q", k, got, expect)
		}
	}
}

// A callee compiled for its native callers (jitRegNativeCallee) is
// compiled in the background too: none of its code at once, its callers
// calling it through its link, which leaves for Go to run it, until the
// code, its native callers' alone (jitRegCompile's callee), is installed.
func TestJITBackgroundCalleeCompile(t *testing.T) {
	t.Parallel()
	r := jitBackgroundForTest(t, 1)
	// No loop, and a call: its own program, for Go to enter, is refused
	// (jitcompile.HostOnly), its native callers' is not.
	if _, err := r.Run(compileForTest(t, `function half(x) { return x / 2 } function sq(x) { return half(x * x) + 1 }`)); err != nil {
		t.Fatal(err)
	}
	cl := jitGlobalClosureForTest(r, "sq")
	// Register code: compiled for native callers (jitRegNativeCallee),
	// lowered for them (LowerRegInline), its code theirs (callee).
	c, key := r.regOf(cl), r.jitRegKeyOf(cl)
	r.jitRegNativeCallee(cl, c, nil)
	if jitHasCodeOfForTest(r, cl) || cl.closureCode.jitReg != nil {
		t.Fatalf("code at once (%s)", jitBackgroundStateForTest(r, cl))
	}
	if r.jit.regPendingJobs[key] == nil {
		t.Fatal("want its compile pending")
	}
	r.jitDrain()
	e := jitRegOfForTest(r, cl)
	if r.jit.regPendingJobs[key] != nil || e == nil || e.code == nil || !e.callee {
		t.Fatalf("pending %v, code %v, callee's %v: want the callers' code installed", r.jit.regPendingJobs[key] != nil, e != nil && e.code != nil, e != nil && e.callee)
	}
	if cl.closureCode.jitReg != e {
		t.Fatal("the installed code is not the callers'")
	}
	jitCallForTest(t, r, `sq(6)`, 19)
}

// A compile in the background is made with the backend's switches and the
// encoding as they were when it was prepared, whatever a host or a test
// sets before its worker runs it: the worker reads no variable of theirs.
// Annotate shows: the code has its marks (mir.Code's Marks; on arm64, which
// marks nothing, its Listing) if it was set then, and none if it was set
// only after.
func TestJITBackgroundSettingsAtPrepare(t *testing.T) {
	// Installed at the first check after it was handed over; register
	// code's at the third: its first compile is asked at its loop's check,
	// whose next two, in the same call, would install it at the first or
	// the second, before the call is done.
	checks := uint16(1)
	checks = 3
	for _, at := range []bool{true, false} {
		r := jitBackgroundForTest(t, checks)
		var marks []int
		jitTestSwitch(t, func(jc *jitTestConfig) {
			jc.codeHook = func(_ JITFunction, _ []byte, c *mir.Code) { marks = append(marks, len(c.Marks)+len(c.Listing)) }
		})
		jitTestSwitch(t, func(jc *jitTestConfig) { jc.annotate = switchOf(at) })
		// Lowered at the first call, prepared at the second, installed at
		// the third; register code lowered and prepared at the first,
		// installed at the second.
		jitCallForTest(t, r, `sum(10)`, 45)
		if len(r.jit.regJobs) != 1 || r.jit.regJobs[0].opts.Annotate != at {
			t.Fatalf("%d compiles in the background, want 1, made with Annotate %v", len(r.jit.regJobs), at)
		}
		jitTestSwitch(t, func(jc *jitTestConfig) { jc.annotate = switchOf(!at) })
		jitCallForTest(t, r, `sum(10)`, 45)
		jitTestSwitch(t, func(jc *jitTestConfig) { jc.codeHook = nil })
		if len(marks) != 1 || (marks[0] != 0) != at {
			t.Fatalf("Annotate %v when prepared: marks %v", at, marks)
		}
	}
}

// A compile in the background is built on its worker, from its feedback
// asked ahead (jitSnapshot): prepared, it holds the snapshot and no
// function yet; installed, its code runs, nothing missed.
func TestJITBackgroundBuildsFromSnapshot(t *testing.T) {
	t.Parallel()
	r := jitBackgroundForTest(t, 100)
	e, j := jitRegPendingRecompileForTest(t, r)
	cl := jitGlobalClosureForTest(r, "sum")
	if j.snap == nil || j.missed {
		t.Fatalf("snapshot %v, missed %v: want the compile prepared as a snapshot", j.snap != nil, j.missed)
	}
	<-j.done
	if j.f == nil || j.missed || j.buildErr != nil {
		t.Fatalf("built %v, missed %v, refused %v: want the worker to have built it", j.f != nil, j.missed, j.buildErr)
	}
	r.jitStress.checks = 1
	jitCallForTest(t, r, `sum(10)`, 45)
	if r.jit.regPendingJobs[e.key] != nil || jitRegOfForTest(r, cl) == e || r.jit.regSnapMisses != 0 {
		t.Fatalf("pending %v, same code %v, missed %d: want the code built from the snapshot installed", r.jit.regPendingJobs[e.key] != nil, jitRegOfForTest(r, cl) == e, r.jit.regSnapMisses)
	}
}

// A build that asks what its snapshot was not asked misses, and the compile
// is built on the runtime's goroutine at its install instead, its code
// installed as any other.
func TestJITBackgroundSnapshotMiss(t *testing.T) {
	r := jitBackgroundForTest(t, 0)
	cl := jitGlobalClosureForTest(r, "sum")
	stress := r.jitStress
	r.jitBackground, r.jitStress = jitOnSpot, jitStressConfig{}
	for range 2 {
		jitCallForTest(t, r, `sum(10)`, 45)
	}
	r.jitBackground, r.jitStress = jitInWorkers, stress
	old := jitRegOfForTest(r, cl)
	if old == nil || old.code == nil {
		t.Fatalf("sum has no code: %+v", old)
	}
	j := jitRegJobForTest(t, r, cl)
	// Every build asks it.
	delete(j.snap.s.answers, jitSnapKey{snapKeepLimit, 0})
	r.jitRegSubmit(j)
	if !j.missed || r.jit.regSnapMisses != 1 || r.jit.regPendingJobs[j.key] != nil || jitRegOfForTest(r, cl) == old {
		t.Fatalf("missed %v, counted %d, pending %v, same code %v: want it built on the runtime's goroutine and installed",
			j.missed, r.jit.regSnapMisses, r.jit.regPendingJobs[j.key] != nil, jitRegOfForTest(r, cl) == old)
	}
	jitCallForTest(t, r, `sum(100)`, 4950)
}

// A compile in the background whose callee's code is let go meanwhile --
// the callee's entry dropped and its function compiled again -- calls
// through the callee's link all the same, a function's for good, by its
// key (regLinks), whatever code it has: it is installed as it was built,
// and its native calls are then the callee's link's. A compile that called
// through a link let go with its code had made every call of the callee
// through Go.
func TestJITBackgroundStaleLink(t *testing.T) {
	t.Parallel()
	r := jitBackgroundForTest(t, 1)
	if _, err := r.Run(compileForTest(t, `function half(x) { return x / 2 }
		function loop(n) { let s = 0; for (let i = 0; i < n; i++) s += half(i); return s }`)); err != nil {
		t.Fatal(err)
	}
	half, loop := jitGlobalClosureForTest(r, "half"), jitGlobalClosureForTest(r, "loop")
	// Called, not inlined: through its link.
	jitTestSwitch(t, func(jc *jitTestConfig) { jc.noInline = func(fn *bytecode.Function) bool { return fn == half.fn } })
	// Register code's link is its function's for good, by its key
	// (regLinks), whatever code it has: a compile that calls through
	// it is never stale, and is installed as it was built. Its native
	// calls are the link's, half compiled again meanwhile.
	hc, hkey, lkey := r.regOf(half), r.jitRegKeyOf(half), r.jitRegKeyOf(loop)
	r.jitRegNativeCallee(half, hc, nil)
	r.jitDrain()
	he := jitRegOfForTest(r, half)
	if he == nil || he.code == nil {
		t.Fatal("half has no code")
	}
	link := jitLinkOfForTest(r, half)
	// loop's call of half met by the interpreter, whose cache register
	// code's compile reads its target from (regCallSites).
	r.jitEnabled = false
	jitCallForTest(t, r, `loop(3)`, 1.5)
	r.jitEnabled = true
	if e := r.jitRegCompile(loop, r.regOf(loop), lkey, false, 0); e != nil || r.jit.regPendingJobs[lkey] == nil {
		t.Fatal("loop's compile not pending")
	}
	r.jit.regLetGo(hkey, he)
	r.jitRegNativeCallee(half, hc, nil)
	r.jitDrain()
	if ne := jitRegOfForTest(r, half); ne == nil || ne.code == nil || ne == he || r.jit.regLinks[hkey] != link {
		t.Fatalf("half compiled again: code %v, the same link %v", ne != nil && ne.code != nil && ne != he, r.jit.regLinks[hkey] == link)
	}
	if !jitHasCodeOfForTest(r, loop) {
		t.Fatal("loop's code not installed")
	}
	in := link.in
	jitCallForTest(t, r, `loop(100)`, 2475)
	if n := link.in - in; n < 100 {
		t.Errorf("loop(100) called half natively %d times, want 100", n)
	}
}

// jitBackgroundRuntime is jitRuntimeForTest's runtime with the JIT,
// compiling in the background.
func jitBackgroundRuntime(t *testing.T) *Runtime {
	t.Helper()
	r := jitRuntimeForTest(t, Config{JIT: true})
	r.SetJITBackground(true)
	return r
}

// A call decided while its target is compiled in the background waits for
// its code: where no link may take the call till then, an arrow's that reads
// this, the answer "not now" had left it Go's for good, Go entering the
// target at each call, however soon it was compiled -- AWFY's Richards at
// five times its time -- and so too where the compile had not begun, the
// runtime having as many in the background as it may (jitRegCompile,
// jobsFull).
func TestJITBackgroundCallWaitsForItsTarget(t *testing.T) {
	t.Parallel()
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	jitTestSwitch(t, func(jc *jitTestConfig) { jc.noInline = func(fn *bytecode.Function) bool { return fn.Name != "drive" } })
	var setup strings.Builder
	setup.WriteString(`
		class Task {
			constructor(k) {
				this.n = k
				this.f = k === 0 ? (x) => this.n + x : k === 1 ? (x) => this.n - x : k === 2 ? (x) => this.n * x :
					k === 3 ? (x) => this.n | x : k === 4 ? (x) => this.n & x : k === 5 ? (x) => this.n ^ x : (x) => this.n % (x + 1)
			}
			run(x) { return this.f(x) }
		}
		const tasks = [0, 1, 2, 3, 4, 5, 6].map((k) => new Task(k))
		function drive(n) {
			let s = 0
			for (let i = 0; i < n; i++) s = (s + tasks[i % 7].run(i)) | 0
			return s
		}
	`)
	r := jitAgreesForTestWith(t, jitStressConfig{background: true, checks: 40}, setup.String(), `drive(20000)`, `drive(200000)`)
	e, re := jitNamedForTest(r, "run")
	if re == nil || re.code == nil {
		t.Fatalf("run was not compiled: %+v", re)
	}
	targets := 0
	for _, x := range e.nativeCalls {
		if x.cl != nil {
			targets++
		}
	}
	if targets == 0 {
		t.Fatalf("no call of a task's arrow is native: sites %v", e.callSites)
	}
}

// A function with no loop, compiled once hot, in the background: its calls
// enter the code once it is installed. Refused for the compile's being under
// way, Go never asked again, and the recursion stayed in the interpreter.
func TestJITBackgroundHotCodeIsEntered(t *testing.T) {
	t.Parallel()
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	setup := `
		var moves = 0
		function move(a, b) { moves += a < b ? 1 : 2 }
		function hanoi(n, a, b) {
			if (n === 1) { move(a, b); return }
			const c = 3 - a - b
			hanoi(n - 1, a, c)
			move(a, b)
			hanoi(n - 1, c, b)
		}
	`
	rounds := []string{}
	for range 8 {
		rounds = append(rounds, `hanoi(12, 0, 1), moves`)
	}
	r := jitAgreesForTestWith(t, jitStressConfig{background: true, checks: 3}, setup, rounds...)
	e, re := jitNamedForTest(r, "hanoi")
	if re == nil || re.code == nil {
		t.Fatalf("hanoi has no code: %+v", re)
	}
	if e.starts == 0 {
		t.Fatalf("no call of hanoi entered its code: %+v", e)
	}
}

// A first compile that meets a small callee being compiled waits for its
// code, as a compile on the spot finds it there, and inlines it: called
// natively in its place, nothing would ever teach the code to, no call of
// such a callee leaving native code. carried_short gains nothing without it.
func TestJITBackgroundCompileWaitsForItsCallee(t *testing.T) {
	t.Parallel()
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	setup := `
		function P(v) { this.v = v }
		P.prototype.add = function (o) { return new P(this.v + o.v) }
		function once(n) {
			const one = new P(1)
			let s = new P(0)
			for (let i = 0; i < n; i++) s = s.add(one)
			return s.v
		}
		function caller(n) {
			let x = 0
			for (let i = 0; i < n; i++) x += once(100)
			return x
		}
	`
	r := jitAgreesForTestWith(t, jitStressConfig{background: true, checks: 2}, setup, `caller(20000)`, `caller(20000)`)
	// Register code's compiles in the background hold for no callee's
	// (jitRegSubmit): what holding would gain, its callee inlined, its
	// objects left unmade -- the loop's entry dropped for them
	// (unentered) -- register code has all the same.
	_, re := jitNamedForTest(r, "once")
	v, err := r.Run(compileForTest(t, `P.prototype.add`))
	if err != nil {
		t.Fatal(err)
	}
	add := v.Object().fn().closure
	inlined := re != nil && re.keep != nil && slices.Contains(re.keep.inlined, add)
	if re == nil || re.code == nil || !inlined || len(re.unentered) == 0 {
		t.Fatalf("once does not inline add, whose objects it would leave unmade: %+v", re)
	}
}
