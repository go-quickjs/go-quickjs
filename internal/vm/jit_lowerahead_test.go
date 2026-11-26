//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64))

package vm

import (
	"fmt"
	"reflect"
	"slices"
	"sync"
	"testing"
)

// A compile in the background takes its programs lowered ahead by a worker
// (jit_lowerahead.go), and they are the programs the goroutine lowers: each
// kept in the runtime's lowerings is what jitRegLowerWith makes of its
// register code and hints, and the code gives the interpreter's answers.
// Under QJS_JIT_STRESS's background the lowering is waited for, so that
// the compile is made where it would have been.
func TestJITLowerAheadSamePrograms(t *testing.T) {
	t.Parallel()
	for _, marking := range []bool{false, true} {
		t.Run(fmt.Sprintf("marking=%v", marking), func(t *testing.T) {
			if marking {
				jitMarkingForTest(t)
			}
			prog := compileForTest(t, regBackgroundSrc)
			want := ""
			{
				r := newTestRuntime(t, Config{})
				t.Cleanup(r.Close)
				if _, err := r.Run(prog); err != nil {
					t.Fatal(err)
				}
				fn, _ := r.GetProp(Obj(r.Global()), r.Intern("work"))
				for range 6 {
					v, err := r.Call(fn, Undefined, []Value{Float(2000)})
					want += regNativeShow(r, v, err) + ";"
				}
			}
			r := newTestRuntime(t, Config{JIT: true})
			t.Cleanup(r.Close)
			r.SetJITWarmUp(1, 1)
			r.SetJITBackground(true)
			r.jitStress.background, r.jitStress.checks = true, 1
			if _, err := r.Run(prog); err != nil {
				t.Fatal(err)
			}
			fn, _ := r.GetProp(Obj(r.Global()), r.Intern("work"))
			got := ""
			for range 6 {
				v, err := r.Call(fn, Undefined, []Value{Float(2000)})
				got += regNativeShow(r, v, err) + ";"
			}
			if got != want {
				t.Fatalf("got %q, want the interpreter's %q", got, want)
			}
			s := r.jit
			if s.aheadJobs == 0 || s.aheadTaken == 0 || s.bgSubmitted == 0 {
				t.Fatalf("%d lowerings ahead, %d programs taken of them, %d compiles handed over: want some of each", s.aheadJobs, s.aheadTaken, s.bgSubmitted)
			}
			if len(s.regAhead) != 0 || len(s.regAheadRoots) != 0 {
				t.Errorf("%d lowerings ahead and %d roots kept after the compiles", len(s.regAhead), len(s.regAheadRoots))
			}
			if s.regSnapMisses != 0 {
				t.Errorf("%d snapshot misses", s.regSnapMisses)
			}
			for key, l := range s.regLowered {
				fn := key.Value()
				if fn == nil {
					continue
				}
				for kind, p := range l.progs {
					if !l.done[kind] {
						continue
					}
					h := l.hints
					h.KeepParams = r.nodeQuirks && !fn.Strict
					q, err := jitRegLowerWith(fn.RegCompiled(), fn, jitLowerKind(kind), h)
					if !reflect.DeepEqual(p, q) || fmt.Sprint(err) != fmt.Sprint(l.errs[kind]) {
						t.Errorf("%s's program of kind %d (error %v) is not the goroutine's lowering (error %v)", fn.Name, kind, l.errs[kind], err)
					}
				}
			}
		})
	}
}

// jitAheadForTest is a runtime that compiles in the background with no
// worker given its lowerings ahead (jitTestConfig's aheadUnstarted), with
// loop, a function of a loop, defined, and loop's closure and key.
func jitAheadForTest(t *testing.T) (*Runtime, *closure, jitKey) {
	t.Helper()
	r := jitBackgroundRuntime(t)
	r.jitTest.aheadUnstarted = true
	// A budget a call does a fraction of: asks a few calls apart.
	r.jitCallThreshold = jitInvocationsForMaglev
	if _, err := r.Run(compileForTest(t, `function loop(n){let s=0;for(let i=0;i<n;i++)s=(s+i)|0;return s}`)); err != nil {
		t.Fatal(err)
	}
	cl := jitGlobalClosureForTest(r, "loop")
	return r, cl, r.jitRegKeyOf(cl)
}

// jitAheadPendingForTest calls loop until its compile waits for its
// lowering ahead, which it returns.
func jitAheadPendingForTest(t *testing.T, r *Runtime, cl *closure, key jitKey) *jitAheadJob {
	t.Helper()
	for call := 1; ; call++ {
		jitCallForTest(t, r, `loop(200)`, 19900)
		if r.jit == nil {
			continue
		}
		if j := r.jit.regAheadRoots[key]; j != nil {
			return j
		}
		if call > 100 {
			t.Fatalf("no lowering ahead (%s)", jitBackgroundStateForTest(r, cl))
		}
	}
}

// A compile in the background whose program is not lowered hands its
// lowering to the workers and is asked for again a part of a budget later
// (jitAheadBudgetPart), not at every sample, nothing compiled meanwhile;
// asked again once the worker has lowered it, it takes the worker's
// program and is handed over, and what it did not take is let go of.
func TestJITLowerAheadAsksAgain(t *testing.T) {
	t.Parallel()
	r, cl, key := jitAheadForTest(t)
	j := jitAheadPendingForTest(t, r, cl, key)
	budget := jitRegBudget(r.jitCallThreshold, cl.reg)
	if life := r.jitRegLifeOf(cl); life.tierBudget <= 0 || life.tierBudget > budget/jitAheadBudgetPart {
		t.Fatalf("budget left %d: want a part of %d, %d, counted while the lowering waits", life.tierBudget, budget, budget/jitAheadBudgetPart)
	}
	if r.jit.bgSubmitted != 0 || jitHasCodeOfForTest(r, cl) || len(j.items) == 0 || j.items[0].at != (jitAheadKey{key, jitLowerSSA}) {
		t.Fatalf("handed over %d, code %v, items %+v: want loop's own program waited for, nothing compiled", r.jit.bgSubmitted, jitHasCodeOfForTest(r, cl), j.items)
	}
	for range 3 {
		jitCallForTest(t, r, `loop(200)`, 19900)
	}
	if r.jit.bgSubmitted != 0 || r.jit.regAheadRoots[key] != j || r.jit.aheadJobs != 1 {
		t.Fatalf("handed over %d, lowerings ahead %d: want the one lowering waited for", r.jit.bgSubmitted, r.jit.aheadJobs)
	}
	j.run()
	p := j.items[0].got.progs[jitLowerSSA]
	if p == nil || j.state.Load() != jitJobDone {
		t.Fatalf("the worker lowered no program (state %d, error %v)", j.state.Load(), j.items[0].got.errs[jitLowerSSA])
	}
	for call := 1; r.jit.bgSubmitted == 0; call++ {
		if call > 100 {
			t.Fatalf("never handed over (%s)", jitBackgroundStateForTest(r, cl))
		}
		jitCallForTest(t, r, `loop(200)`, 19900)
	}
	if l := r.jit.regLowered[key]; l == nil || l.progs[jitLowerSSA] != p || r.jit.aheadTaken == 0 {
		t.Fatalf("taken %d: want loop's program the worker's", r.jit.aheadTaken)
	}
	if len(r.jit.regAhead) != 0 || len(r.jit.regAheadRoots) != 0 {
		t.Fatalf("%d lowerings ahead, %d roots kept after the compile", len(r.jit.regAhead), len(r.jit.regAheadRoots))
	}
	if pj := r.jit.regPendingJobs[key]; pj != nil {
		<-pj.done
	}
	for call := 1; !jitHasCodeOfForTest(r, cl); call++ {
		if call > 100 {
			t.Fatalf("never installed (%s)", jitBackgroundStateForTest(r, cl))
		}
		jitCallForTest(t, r, `loop(200)`, 19900)
	}
}

// A lowering ahead no worker has begun once its function has spent
// jitTakeBackAsks budgets waiting is taken back: the compile goes on, its
// program lowered on the goroutine, and a worker that comes to it after
// leaves it.
func TestJITLowerAheadTakeBack(t *testing.T) {
	t.Parallel()
	r, cl, key := jitAheadForTest(t)
	j := jitAheadPendingForTest(t, r, cl, key)
	for call := 1; r.jit.bgSubmitted == 0; call++ {
		if call > 400 {
			t.Fatalf("never handed over, waited %d (%s)", j.waited.Load(), jitBackgroundStateForTest(r, cl))
		}
		jitCallForTest(t, r, `loop(200)`, 19900)
	}
	if j.state.Load() != jitJobTaken || j.waited.Load() < jitTakeBackAsks*jitRegBudget(r.jitCallThreshold, cl.reg) {
		t.Fatalf("state %d, waited %d: want it taken back after %d budgets", j.state.Load(), j.waited.Load(), jitTakeBackAsks)
	}
	if l := r.jit.regLowered[key]; l == nil || l.progs[jitLowerSSA] == nil || r.jit.aheadTaken != 0 {
		t.Fatalf("taken %d: want loop's program lowered on the goroutine", r.jit.aheadTaken)
	}
	j.run()
	if j.state.Load() != jitJobTaken || j.items[0].got.done[jitLowerSSA] {
		t.Fatalf("state %d after a worker's run: want it left as taken", j.state.Load())
	}
}

// regReplaySrc has work call big natively, through a link (too long to
// inline), and norm inlined.
const regReplaySrc = `
function Point(x, y) { this.x = x; this.y = y }
function norm(p) { return p.x * p.x + p.y * p.y }
function big(a) {
	let s = a;
	for (let i = 0; i < 3; i++) { s = (s * 31 + i) % 1000003; s = (s ^ (s >> 3)) % 1000003; s = (s + a * i) % 1000003;
		s = (s * 7 + 1) % 1000003; s = (s ^ (s << 2)) % 1000003; s = (s + 11) % 1000003; s = (s * 13) % 1000003 }
	return s
}
function work(n) {
	let s = 0;
	for (let i = 0; i < n; i++) {
		const p = new Point(i, i + 1);
		s = (s + norm(p) + big(i)) % 1000003;
	}
	return s;
}
`

// A compile's worker puts back what its snapshot's questions took back
// (workRecovering's replay), and the runtime's goroutine, which may unlink
// a link meanwhile (regUnlink), finds every link the replay puts in the
// compile's keep among those it reads (holdsLink): those the keep held at
// the hand-over and those the questions took back -- links are made only
// on the goroutine, never by a worker.
func TestJITReplayLinksHeldAtHandOver(t *testing.T) {
	t.Parallel()
	prog := compileForTest(t, regReplaySrc)
	want := ""
	{
		r := newTestRuntime(t, Config{})
		t.Cleanup(r.Close)
		if _, err := r.Run(prog); err != nil {
			t.Fatal(err)
		}
		fn, _ := r.GetProp(Obj(r.Global()), r.Intern("work"))
		for range 6 {
			v, err := r.Call(fn, Undefined, []Value{Float(500)})
			want += regNativeShow(r, v, err) + ";"
		}
	}
	var mu sync.Mutex
	replays, links := 0, 0
	jitTestSwitch(t, func(c *jitTestConfig) {
		c.bgReplayed = func(j *jitRegJob) {
			mu.Lock()
			defer mu.Unlock()
			replays++
			for _, l := range j.rf.fb.keep().links {
				links++
				if !slices.Contains(j.links, l) && !slices.Contains(j.snap.s.gained.links, l) {
					t.Errorf("%s: the replay put a link in its keep that the goroutine does not see", j.cl.fn.Name)
				}
			}
		}
	})
	r := newTestRuntime(t, Config{JIT: true})
	t.Cleanup(r.Close)
	r.SetJITWarmUp(1, 1)
	r.SetJITBackground(true)
	r.jitStress.background, r.jitStress.checks = true, 1
	if _, err := r.Run(prog); err != nil {
		t.Fatal(err)
	}
	fn, _ := r.GetProp(Obj(r.Global()), r.Intern("work"))
	got := ""
	for range 6 {
		v, err := r.Call(fn, Undefined, []Value{Float(500)})
		got += regNativeShow(r, v, err) + ";"
	}
	if got != want {
		t.Fatalf("got %q, want the interpreter's %q", got, want)
	}
	mu.Lock()
	defer mu.Unlock()
	if replays == 0 || links == 0 {
		t.Fatalf("%d replays, %d links in their keeps: want some of each", replays, links)
	}
}
