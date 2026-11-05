//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))

package vm

import (
	"github.com/go-quickjs/go-quickjs/internal/bytecode"
	"strings"
	"testing"
	"time"
	"unsafe"

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

// A function due to be compiled in the background runs in the existing
// tiers while it is lowered there, and then compiled there, until its code
// is installed, at the Kth check after its compile was handed over -- a
// call of it is one (tryJITAt), and so is its loop's in a tree
// (tryJITLoop) -- and natively from then on.
func TestJITBackgroundFirstCompile(t *testing.T) {
	const k = 3
	r := jitBackgroundForTest(t, k)
	cl := jitGlobalClosureForTest(r, "sum")
	var j *jitJob
	for call := 1; ; call++ {
		before := r.JITStats().SSAEntries
		jitCallForTest(t, r, `sum(10)`, 45)
		e := r.jit.entryFor(cl.fn)
		if e == nil {
			t.Fatalf("call %d: no entry", call)
		}
		native := r.JITStats().SSAEntries != before
		if e.warming {
			// Its lowering, in the background first (jitLowerLater).
			if e.ssa != nil || native || j != nil {
				t.Fatalf("call %d: code %v, native %v, compile seen %v: want it lowering, run in the existing tiers", call, e.ssa != nil, native, j != nil)
			}
			continue
		}
		if e.pending != nil {
			j = e.pending
			if e.ssa != nil || native || j.checks >= k {
				t.Fatalf("call %d: code %v, native %v, %d checks: want the compile pending, run in the existing tiers", call, e.ssa != nil, native, j.checks)
			}
			continue
		}
		if j == nil {
			t.Fatalf("call %d: code %v, native %v, its compile never seen pending", call, e.ssa != nil, native)
		}
		if j.checks != k || e.ssa == nil || !native {
			t.Fatalf("call %d: its compile's checks %d, code %v, native %v: want the code installed at check %d and run", call, j.checks, e.ssa != nil, native, k)
		}
		break
	}
	if s := r.jit; s.bgLowered != 1 || s.bgSubmitted != 1 || s.bgDiscarded != 0 || len(s.jobs) != 0 || len(s.lowerings) != 0 || s.compiled != 1 {
		t.Fatalf("lowered %d, submitted %d, discarded %d, jobs %d, lowerings %d, compiled %d: want 1, 1, 0, 0, 0, 1",
			s.bgLowered, s.bgSubmitted, s.bgDiscarded, len(s.jobs), len(s.lowerings), s.compiled)
	}
}

// jitPendingRecompileForTest has sum compiled, then compiled again in the
// background, not yet installed: its entry, and the code it had.
func jitPendingRecompileForTest(t *testing.T, r *Runtime) (*jitEntry, *closure) {
	t.Helper()
	cl := jitGlobalClosureForTest(r, "sum")
	// Its first code compiled on the spot.
	stress := r.jitStress
	r.jitBackground, r.jitStress = false, jitStressConfig{}
	for range 2 {
		jitCallForTest(t, r, `sum(10)`, 45)
	}
	r.jitBackground, r.jitStress = true, stress
	e := jitEntryForTest(t, r, "sum")
	e.reopt = true
	r.jitReoptimize(cl, e)
	if e.pending == nil {
		t.Fatal("not compiled again in the background")
	}
	return e, cl
}

// Code compiled again in the background runs on, valid, until its
// replacement is installed, which replaces it then.
func TestJITBackgroundRecompileRunsOldCode(t *testing.T) {
	r := jitBackgroundForTest(t, 2)
	e, _ := jitPendingRecompileForTest(t, r)
	old := e.ssa
	before, reoptimized := r.JITStats().SSAEntries, r.jit.reoptimized
	jitCallForTest(t, r, `sum(10)`, 45)
	if e.pending == nil || e.ssa != old || r.JITStats().SSAEntries == before {
		t.Fatalf("pending %v, same code %v, native %v: want the old code run while the new compiles", e.pending != nil, e.ssa == old, r.JITStats().SSAEntries != before)
	}
	jitCallForTest(t, r, `sum(10)`, 45)
	if e.pending != nil || e.ssa == old || r.jit.reoptimized != reoptimized+1 {
		t.Fatalf("pending %v, same code %v, reoptimized %d: want the new code installed", e.pending != nil, e.ssa == old, r.jit.reoptimized-reoptimized)
	}
}

// A compile again whose function deoptimized since it was built is thrown
// away at its install, the code it would replace kept: built from what
// failed, it would fail the same guard again.
func TestJITBackgroundDiscardsAfterDeopt(t *testing.T) {
	r := jitBackgroundForTest(t, 1)
	e, _ := jitPendingRecompileForTest(t, r)
	old := e.ssa
	e.deopts++
	jitCallForTest(t, r, `sum(10)`, 45)
	if e.pending != nil || e.ssa != old || r.jit.bgDiscarded != 1 || len(r.jit.jobs) != 0 {
		t.Fatalf("pending %v, same code %v, discarded %d, jobs %d: want the compile thrown away", e.pending != nil, e.ssa == old, r.jit.bgDiscarded, len(r.jit.jobs))
	}
	if e.busy != 0 {
		t.Fatalf("busy %d after the compile was thrown away", e.busy)
	}
}

// A runtime closed with compiles in the background lets them go: its
// entries wait for none, and the workers, once done, end.
func TestJITBackgroundClose(t *testing.T) {
	r := jitBackgroundForTest(t, 100)
	e, _ := jitPendingRecompileForTest(t, r)
	j := e.pending
	r.Close()
	r.ReleaseClosed()
	if e.pending != nil || e.busy != 0 {
		t.Fatalf("pending %v, busy %d after the runtime closed", e.pending != nil, e.busy)
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
	e := jitEntryForTest(t, r, "sum")
	old := e.ssa
	j, err := r.compileSSAPrepare(cl.fn, cl, e.ssaProg, e)
	if err != nil {
		t.Fatal(err)
	}
	// A function the optimization cannot take, built as it was prepared,
	// not from a snapshot.
	j.f, j.snap = nil, nil
	j.deopts, j.install = e.deopts, (*Runtime).jitReoptimized
	r.jitSubmit(e, j)
	if !j.panicked || r.jit.backendPanics != 1 || e.pending != nil || e.ssa != old {
		t.Fatalf("panicked %v, backend panics %d, pending %v, same code %v", j.panicked, r.jit.backendPanics, e.pending != nil, e.ssa == old)
	}
	// Counted as it should be: not a backend bug for the cleanup, nor
	// TestMain, to report.
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
		if e := r.jit.entryFor(cl.fn); e != nil && e.ssa != nil && e.pending == nil {
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

// A loop's code installed at the loop's own check in a tree is entered
// there, by on-stack replacement, as code compiled on the spot is: the
// tree that runs the frame recovers its exit, though the function had no
// code when it was called -- its compile pending, whose frame a tree had
// run nested without recovering, and the script's own tree had taken the
// loop's result for the script's (jitTreeRecovery). Each answer is the
// interpreter's, the compile installed at any check.
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
	want := New(Config{})
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

// A callee compiled for its native callers (jitNativeCallee) is compiled
// in the background too: none of its code at once, its callers calling it
// through its link, which leaves for Go to run it, until the code, its
// native callers' alone (ssaCallee), is installed.
func TestJITBackgroundCalleeCompile(t *testing.T) {
	r := jitBackgroundForTest(t, 1)
	// No loop, and a call: its own program, for Go to enter, is refused
	// (jitcompile.HostOnly), its native callers' is not.
	if _, err := r.Run(compileForTest(t, `function half(x) { return x / 2 } function sq(x) { return half(x * x) + 1 }`)); err != nil {
		t.Fatal(err)
	}
	cl := jitGlobalClosureForTest(r, "sq")
	if ce := r.jitNativeCallee(cl); ce != nil {
		t.Fatal("code at once")
	}
	e := r.jit.entryFor(cl.fn)
	if e == nil || e.pending == nil || e.ssa != nil {
		t.Fatalf("entry %v: want its compile pending", e != nil)
	}
	r.jitDrain()
	if e.pending != nil || e.ssa == nil || !e.ssaCallee || !e.entrySlow {
		t.Fatalf("pending %v, code %v, callee's %v, entry slow %v: want the callers' code installed", e.pending != nil, e.ssa != nil, e.ssaCallee, e.entrySlow)
	}
	if ce := r.jitNativeCallee(cl); ce != e {
		t.Fatal("the installed code is not the callers'")
	}
	jitCallForTest(t, r, `sq(6)`, 19)
}

// A lowering in the background whose function was lowered again
// meanwhile, for hints it did not have (relower), is thrown away at its
// install, and the function lowered again from the hints it has now.
func TestJITBackgroundStaleLowering(t *testing.T) {
	r := jitBackgroundForTest(t, 2)
	cl := jitGlobalClosureForTest(r, "sum")
	jitCallForTest(t, r, `sum(10)`, 45)
	if len(r.jit.lowerings) != 1 {
		t.Fatalf("%d lowerings in the background, want 1", len(r.jit.lowerings))
	}
	r.jit.relower(r.jit.lowerings[0].l)
	for call := 0; ; call++ {
		jitCallForTest(t, r, `sum(10)`, 45)
		if e := r.jit.entryFor(cl.fn); e != nil && e.ssa != nil {
			break
		}
		if call == 20 {
			t.Fatal("no code installed")
		}
	}
	if s := r.jit; s.bgLowered != 2 || s.bgDiscarded != 1 {
		t.Fatalf("lowered %d, discarded %d: want 2, 1", s.bgLowered, s.bgDiscarded)
	}
}

// A compile in the background is made with the backend's switches and the
// encoding as they were when it was prepared, whatever a host or a test
// sets before its worker runs it: the worker reads no variable of theirs.
// Annotate shows: the code has its marks (mir.Code's Marks; on arm64, which
// marks nothing, its Listing) if it was set then, and none if it was set
// only after.
func TestJITBackgroundSettingsAtPrepare(t *testing.T) {
	defer func(on bool) { mir.Annotate = on }(mir.Annotate)
	for _, at := range []bool{true, false} {
		r := jitBackgroundForTest(t, 1)
		var marks []int
		JITCodeHook = func(_ JITFunction, _ []byte, c *mir.Code) { marks = append(marks, len(c.Marks)+len(c.Listing)) }
		mir.Annotate = at
		// Lowered at the first call, prepared at the second, installed at
		// the third.
		jitCallForTest(t, r, `sum(10)`, 45)
		jitCallForTest(t, r, `sum(10)`, 45)
		if len(r.jit.jobs) != 1 || r.jit.jobs[0].opts.Annotate != at {
			t.Fatalf("%d compiles in the background, want 1, made with Annotate %v", len(r.jit.jobs), at)
		}
		mir.Annotate = !at
		jitCallForTest(t, r, `sum(10)`, 45)
		JITCodeHook = nil
		if len(marks) != 1 || (marks[0] != 0) != at {
			t.Fatalf("Annotate %v when prepared: marks %v", at, marks)
		}
	}
}

// A compile in the background is built on its worker, from its feedback
// asked ahead (jitSnapshot): prepared, it holds the snapshot and no
// function yet; installed, its code runs, nothing missed.
func TestJITBackgroundBuildsFromSnapshot(t *testing.T) {
	r := jitBackgroundForTest(t, 100)
	e, _ := jitPendingRecompileForTest(t, r)
	j := e.pending
	if j.snap == nil || j.missed {
		t.Fatalf("snapshot %v, missed %v: want the compile prepared as a snapshot", j.snap != nil, j.missed)
	}
	<-j.done
	if j.f == nil || j.missed || j.buildErr != nil {
		t.Fatalf("built %v, missed %v, refused %v: want the worker to have built it", j.f != nil, j.missed, j.buildErr)
	}
	old := e.ssa
	r.jitStress.checks = 1
	jitCallForTest(t, r, `sum(10)`, 45)
	if e.pending != nil || e.ssa == old || r.jit.bgMissed != 0 {
		t.Fatalf("pending %v, same code %v, missed %d: want the code built from the snapshot installed", e.pending != nil, e.ssa == old, r.jit.bgMissed)
	}
}

// A build that asks what its snapshot was not asked misses, and the compile
// is built on the runtime's goroutine at its install instead, its code
// installed as any other.
func TestJITBackgroundSnapshotMiss(t *testing.T) {
	r := jitBackgroundForTest(t, 0)
	cl := jitGlobalClosureForTest(r, "sum")
	stress := r.jitStress
	r.jitBackground, r.jitStress = false, jitStressConfig{}
	for range 2 {
		jitCallForTest(t, r, `sum(10)`, 45)
	}
	r.jitBackground, r.jitStress = true, stress
	e := jitEntryForTest(t, r, "sum")
	old := e.ssa
	j, err := r.compileSSAPrepare(cl.fn, cl, e.ssaProg, e)
	if err != nil || j.snap == nil {
		t.Fatalf("prepared %v, snapshot %v", err, j != nil && j.snap != nil)
	}
	// Every build asks it.
	delete(j.snap.answers, jitSnapKey{snapKeepLimit, 0})
	j.deopts, j.install = e.deopts, (*Runtime).jitReoptimized
	r.jitSubmit(e, j)
	if !j.missed || r.jit.bgMissed != 1 || e.pending != nil || e.ssa == old {
		t.Fatalf("missed %v, counted %d, pending %v, same code %v: want it built on the runtime's goroutine and installed",
			j.missed, r.jit.bgMissed, e.pending != nil, e.ssa == old)
	}
	jitCallForTest(t, r, `sum(100)`, 4950)
}

// A compile in the background whose code would call through a link its
// callee's function let go meanwhile -- the callee's entry dropped, as
// jitHotEntry drops it to compile it again for Go to enter, its link with
// it, no installed code holding the link, and the callee's next entry
// given another -- is built again on the runtime's goroutine at its
// install (staleLinks): installed as it was, it called through the old
// link, which never had code again, and made every call of the callee
// through Go. Its native calls are then the callee's link's.
func TestJITBackgroundStaleLink(t *testing.T) {
	r := jitBackgroundForTest(t, 1)
	if _, err := r.Run(compileForTest(t, `function half(x) { return x / 2 }
		function loop(n) { let s = 0; for (let i = 0; i < n; i++) s += half(i); return s }`)); err != nil {
		t.Fatal(err)
	}
	half, loop := jitGlobalClosureForTest(r, "half"), jitGlobalClosureForTest(r, "loop")
	// Called, not inlined: through its link.
	jitNoInline = func(fn *bytecode.Function) bool { return fn == half.fn }
	t.Cleanup(func() { jitNoInline = nil })
	// half's code for native callers, installed.
	r.jitNativeCallee(half)
	r.jitDrain()
	he := r.jit.entryFor(half.fn)
	if he == nil || he.ssa == nil {
		t.Fatal("half has no code")
	}
	old := he.link
	// loop's compile handed over, its call of half through half's link.
	if e := r.jitCompileForIn(loop.fn, loop, jitLowerSSA, true); e == nil || e.pending == nil {
		t.Fatal("loop's compile not pending")
	}
	// half's entry dropped meanwhile, and half compiled again: another
	// link.
	if !r.jit.dropEntry(r.jitKeyOf(half.fn), he) {
		t.Fatal("half's entry not dropped")
	}
	r.jitNativeCallee(half)
	r.jitDrain()
	he = r.jit.entryFor(half.fn)
	if he == nil || he.ssa == nil || he.link == old {
		t.Fatalf("half compiled again: entry %v, a link of its own %v", he != nil && he.ssa != nil, he != nil && he.link != old)
	}
	le := r.jit.entryFor(loop.fn)
	if le == nil || le.ssa == nil {
		t.Fatal("loop's code not installed")
	}
	if r.jit.bgStale != 1 {
		t.Errorf("%d compiles built again for stale links, want 1", r.jit.bgStale)
	}
	in := he.link.in
	jitCallForTest(t, r, `loop(100)`, 2475)
	if n := he.link.in - in; n < 100 {
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
// runtime having as many in the background as it may (jitCalleeAwaited).
func TestJITBackgroundCallWaitsForItsTarget(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	jitNoInline = func(fn *bytecode.Function) bool { return fn.Name != "drive" }
	t.Cleanup(func() { jitNoInline = nil })
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
	e := jitCallbackEntry(r, "run")
	if e == nil || e.ssa == nil {
		t.Fatalf("run was not compiled: %+v", e)
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
// way, Go never asked again, and the recursion stayed in the tree tier.
func TestJITBackgroundHotCodeIsEntered(t *testing.T) {
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
	e := jitCallbackEntry(r, "hanoi")
	if e == nil || e.ssa == nil {
		t.Fatalf("hanoi has no code: %+v", e)
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
	if r.jit.bgHeld == 0 {
		t.Fatal("no compile waited for a callee's")
	}
	if len(r.jit.heldJobs) != 0 {
		t.Fatalf("%d compiles still held", len(r.jit.heldJobs))
	}
	e := jitCallbackEntry(r, "once")
	if e == nil || e.ssa == nil || len(e.ssaInlined) == 0 || e.ssaVirtuals == 0 {
		t.Fatalf("once does not inline add, whose objects it would leave unmade: %+v", e)
	}
}

// A compile a frame waits for at a loop waits for no call that frame has left
// behind, whose callee the loop does not need; it does for one the frame comes
// back to.
func TestJITBackgroundHoldsNotForACallLeftBehind(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	setup := `
		function fill(n) {
			const a = []
			for (let i = 0; i < n; i++) a.push(i)
			return a
		}
		function fillAndAdd(n) {
			const a = fill(64)
			let s = 0
			for (let i = 0; i < n; i++) s = (s + a[i & 63]) | 0
			return s
		}
		function fillEachTime(n) {
			let s = 0
			for (let j = 0; j < 4; j++) {
				const a = fill(64)
				for (let i = 0; i < n; i++) s = (s + a[i & 63]) | 0
			}
			return s
		}
	`
	r := jitAgreesForTestWith(t, jitStressConfig{background: true, checks: 2}, setup, `fillAndAdd(2000000)`)
	if r.jit.bgHeld != 0 {
		t.Fatalf("%d compiles held for a call behind their frames", r.jit.bgHeld)
	}
	if e := jitCallbackEntry(r, "fillAndAdd"); e == nil || e.ssa == nil {
		t.Fatalf("fillAndAdd has no code: %+v", e)
	}
	r = jitAgreesForTestWith(t, jitStressConfig{background: true, checks: 2}, setup, `fillEachTime(500000)`)
	if r.jit.bgHeld == 0 {
		t.Fatal("no compile waited for a call its frame comes back to")
	}
}

// Two functions calling each other are not held for each other, and a
// runtime's compiles held when it lets go of its JIT leave their entries as
// it found them.
func TestJITBackgroundHoldsEnd(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	setup := `
		function even(n) { return n === 0 ? 1 : odd(n - 1) }
		function odd(n) { return n === 0 ? 0 : even(n - 1) }
		function caller(n) {
			let x = 0
			for (let i = 0; i < n; i++) x += even(i & 15)
			return x
		}
	`
	r := jitAgreesForTestWith(t, jitStressConfig{background: true, checks: 2}, setup, `caller(40000)`, `caller(40000)`)
	if len(r.jit.heldJobs) != 0 {
		t.Fatalf("%d compiles held for each other", len(r.jit.heldJobs))
	}
	for _, name := range []string{"even", "odd", "caller"} {
		if e := jitCallbackEntry(r, name); e == nil || e.pending != nil {
			t.Fatalf("%s is still waiting for its compile: %+v", name, e)
		}
	}
	e, odd := jitCallbackEntry(r, "even"), jitCallbackEntry(r, "odd")
	v, err := r.Run(compileForTest(t, `even`))
	if err != nil {
		t.Fatal(err)
	}
	even := v.Object().fn().closure
	oddFn := odd.link.key.Value()
	h := &jitJob{e: e, held: true}
	e.pending, e.awaited = h, []*bytecode.Function{oddFn}
	e.busy++
	// even's compile waits for odd's: odd's does not wait for it in turn.
	odd.holds = true
	if r.jitAwaits(odd, oddFn, 0, even) {
		t.Fatal("two compiles held for each other")
	}
	e.awaited = nil
	if !r.jitAwaits(odd, oddFn, 0, even) {
		t.Fatal("a compile not held for a callee being compiled")
	}
	// Nor through others between them, however many: of a cycle held for
	// good, no function would ever be compiled.
	chain := make([]*bytecode.Function, jitSeedDepth+3)
	for i := range chain {
		chain[i] = &bytecode.Function{}
		x, key := &jitEntry{}, r.jitKeyOf(chain[i])
		x.pending = &jitJob{e: x, held: true}
		addr := jitFnAddr{uintptr(unsafe.Pointer(chain[i])), key.realm}
		r.jit.byFn[addr] = jitFnEntry{x, key}
		t.Cleanup(func() { delete(r.jit.byFn, addr) })
		if i > 0 {
			r.jit.entryFor(chain[i-1]).awaited = chain[i : i+1]
		}
	}
	if !r.jit.jitCompileWaits(chain[0], chain[len(chain)-1]) {
		t.Fatalf("a compile held through %d others is not seen to wait", len(chain)-2)
	}
	odd.holds, odd.awaited, e.awaited = false, nil, []*bytecode.Function{oddFn}
	r.jit.heldJobs = append(r.jit.heldJobs, h)
	r.jit.abandonJobs()
	if len(r.jit.heldJobs) != 0 || e.pending != nil || e.busy != 0 || len(e.awaited) != 0 {
		t.Fatalf("held compile not let go: %d held, pending %v, busy %d, waiting for %d", len(r.jit.heldJobs), e.pending != nil, e.busy, len(e.awaited))
	}
}
