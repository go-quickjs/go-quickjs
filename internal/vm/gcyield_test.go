package vm

import (
	"runtime"
	"runtime/debug"
	"testing"
)

// gcYieldSrc allocates objects, strings and arrays, a few of which it
// keeps, so that a run makes the collector run several cycles; its answer
// is exact: churn(n) is n and how many objects it kept.
const gcYieldSrc = `function churn(n) {
	let keep = [], sum = 0;
	for (let i = 0; i < n; i++) {
		const o = {i, s: "x" + i, a: [i, i + 1]};
		if (i % 64 == 0) keep.push(o);
		sum += o.a[1] - o.i;
	}
	return sum + ":" + keep.length;
}`

const gcYieldN, gcYieldWant = 300000, "300000:4688"

// gcYieldRun calls churn on r at GOMAXPROCS procs and reports its answer,
// the waits Config.GCYield had r take meanwhile and the collector's cycles
// that ended meanwhile.
func gcYieldRun(t *testing.T, r *Runtime, procs int) (string, int, int) {
	t.Helper()
	defer runtime.GOMAXPROCS(runtime.GOMAXPROCS(procs))
	fn, err := r.GetProp(Obj(r.Global()), r.Intern("churn"))
	if err != nil {
		t.Fatal(err)
	}
	p0, _, _ := r.GCYieldStats()
	var m0, m1 runtime.MemStats
	runtime.ReadMemStats(&m0)
	v, err := r.Call(fn, Undefined, []Value{Float(gcYieldN)})
	runtime.ReadMemStats(&m1)
	if err != nil {
		t.Fatal(err)
	}
	s, err := r.toString(v)
	if err != nil {
		t.Fatal(err)
	}
	p1, _, _ := r.GCYieldStats()
	return s.Go(), p1 - p0, int(m1.NumGC - m0.NumGC)
}

func gcYieldRuntime(t *testing.T, cfg Config) *Runtime {
	t.Helper()
	r := New(cfg)
	t.Cleanup(r.Close)
	if _, err := r.Run(compileForTest(t, gcYieldSrc)); err != nil {
		t.Fatal(err)
	}
	return r
}

// Config.GCYield is off unless asked for: a runtime made without it never
// waits, with one P and the collector running.
func TestGCYieldOffByDefault(t *testing.T) {
	r := gcYieldRuntime(t, Config{})
	if r.gcYield.on {
		t.Fatal("GCYield on by default")
	}
	got, pauses, cycles := gcYieldRun(t, r, 1)
	if got != gcYieldWant || pauses != 0 {
		t.Fatalf("churn = %q with %d waits over %d cycles, want %q with none", got, pauses, cycles, gcYieldWant)
	}
}

// With Config.GCYield and one P, the runtime waits while the collector
// marks, each wait to the end of a mark phase: at least one over a run
// that ends cycles, never more than the cycles that ended, and the answer
// the same.
func TestGCYieldPausesAtOneP(t *testing.T) {
	r := gcYieldRuntime(t, Config{GCYield: true})
	got, pauses, cycles := gcYieldRun(t, r, 1)
	if got != gcYieldWant {
		t.Fatalf("churn = %q, want %q", got, gcYieldWant)
	}
	if cycles == 0 {
		t.Fatal("no collection ended during the run")
	}
	if pauses < 1 || pauses > cycles {
		t.Fatalf("%d waits over %d cycles, want 1 to %d", pauses, cycles, cycles)
	}
	if _, waited, worst := r.GCYieldStats(); worst <= 0 || waited < worst {
		t.Fatalf("waited %v, longest %v", waited, worst)
	}
}

// With more than one P, Config.GCYield does nothing; a later change to one
// P is seen within gcYieldRecheck checks made while the collector marks.
func TestGCYieldNotAtTwoP(t *testing.T) {
	r := gcYieldRuntime(t, Config{GCYield: true})
	got, pauses, cycles := gcYieldRun(t, r, 2)
	if got != gcYieldWant || pauses != 0 {
		t.Fatalf("churn = %q with %d waits over %d cycles at 2 Ps, want %q with none", got, pauses, cycles, gcYieldWant)
	}
	for range 20 {
		got, pauses, _ := gcYieldRun(t, r, 1)
		if got != gcYieldWant {
			t.Fatalf("churn = %q, want %q", got, gcYieldWant)
		}
		if pauses > 0 {
			return
		}
	}
	t.Fatal("no wait in 20 runs after GOMAXPROCS went from 2 to 1")
}

// Config.GCYield waits only while the collector marks: with the collector
// off it never does.
func TestGCYieldOnlyWhileMarking(t *testing.T) {
	r := gcYieldRuntime(t, Config{GCYield: true})
	gc := debug.SetGCPercent(-1)
	t.Cleanup(func() { debug.SetGCPercent(gc) })
	got, pauses, cycles := gcYieldRun(t, r, 1)
	if got != gcYieldWant || pauses != 0 || cycles != 0 {
		t.Fatalf("churn = %q with %d waits over %d cycles, collector off, want %q with none", got, pauses, cycles, gcYieldWant)
	}
}
