package vm

import (
	"runtime"
	"testing"
)

// gcYieldSrc allocates objects, strings and arrays, a few of which it
// keeps, so that a run makes the collector run cycles; its answer is exact:
// churn(n) is n and how many objects it kept.
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
	r := newTestRuntime(t, cfg)
	t.Cleanup(r.Close)
	if _, err := r.Run(compileForTest(t, gcYieldSrc)); err != nil {
		t.Fatal(err)
	}
	return r
}

// gcMarkingFor is a marking that a test gives the runtime
// (SetGCMarkingForTest): the collector marks for its first n questions and
// not after them, and asked counts the questions.
func gcMarkingFor(n int) (marking func() bool, asked *int) {
	asked = new(int)
	return func() bool {
		*asked++
		return *asked <= n
	}, asked
}

// Config.GCYield is off unless asked for: a runtime made without it never
// asks whether the collector marks, and never waits.
func TestGCYieldOffByDefault(t *testing.T) {
	r := gcYieldRuntime(t, Config{})
	if r.gcYield.on {
		t.Fatal("GCYield on by default")
	}
	marking, asked := gcMarkingFor(1 << 30)
	r.SetGCMarkingForTest(marking)
	got, pauses, _ := gcYieldRun(t, r, 1)
	if got != gcYieldWant || pauses != 0 || *asked != 0 {
		t.Fatalf("churn = %q with %d waits, %d questions, want %q with none", got, pauses, *asked, gcYieldWant)
	}
}

// With Config.GCYield and one P, the runtime waits while the collector
// marks, from the check that finds it marking to the look that finds it
// done: the collector marking for the first three questions -- the check's
// and the wait's first two looks, a sleep after each -- is one wait, at
// least two sleeps long, and the answer is the same.
func TestGCYieldWaitsWhileMarking(t *testing.T) {
	r := gcYieldRuntime(t, Config{GCYield: true})
	marking, asked := gcMarkingFor(3)
	r.SetGCMarkingForTest(marking)
	got, pauses, _ := gcYieldRun(t, r, 1)
	if got != gcYieldWant || pauses != 1 {
		t.Fatalf("churn = %q with %d waits, want %q with 1", got, pauses, gcYieldWant)
	}
	if *asked <= 4 {
		t.Fatalf("asked %d times, want the checks after the wait to ask too", *asked)
	}
	if _, waited, worst := r.GCYieldStats(); worst < 2*gcYieldPoll || waited != worst {
		t.Fatalf("waited %v, longest %v, want one wait of at least %v", waited, worst, 2*gcYieldPoll)
	}
}

// Config.GCYield waits only while the collector marks: never when it does
// not, though the checks ask.
func TestGCYieldOnlyWhileMarking(t *testing.T) {
	r := gcYieldRuntime(t, Config{GCYield: true})
	marking, asked := gcMarkingFor(0)
	r.SetGCMarkingForTest(marking)
	got, pauses, _ := gcYieldRun(t, r, 1)
	if got != gcYieldWant || pauses != 0 || *asked == 0 {
		t.Fatalf("churn = %q with %d waits, %d questions, want %q with none and questions", got, pauses, *asked, gcYieldWant)
	}
}

// With more than one P, Config.GCYield does nothing, the collector marking
// throughout.
func TestGCYieldNotAtTwoP(t *testing.T) {
	r := gcYieldRuntime(t, Config{GCYield: true})
	marking, asked := gcMarkingFor(1 << 30)
	r.SetGCMarkingForTest(marking)
	got, pauses, _ := gcYieldRun(t, r, 2)
	if got != gcYieldWant || pauses != 0 || *asked == 0 {
		t.Fatalf("churn = %q with %d waits, %d questions at 2 Ps, want %q with none and questions", got, pauses, *asked, gcYieldWant)
	}
}

// A runtime reads GOMAXPROCS at its first check while the collector marks,
// every gcYieldRecheck checks after, and after each wait: a change to one P
// is seen within gcYieldRecheck checks, and one from it at the next check
// after a wait.
func TestGCYieldReadsGOMAXPROCS(t *testing.T) {
	r := gcYieldRuntime(t, Config{GCYield: true})
	defer runtime.GOMAXPROCS(runtime.GOMAXPROCS(2))
	marking, _ := gcMarkingFor(0)
	r.SetGCMarkingForTest(marking)
	for range gcYieldRecheck {
		r.gcYieldNow()
	}
	runtime.GOMAXPROCS(1)
	// The next check reads it again, finds one P, and waits while the
	// collector marks: one question asked, the wait's first look.
	marking, asked := gcMarkingFor(1)
	r.SetGCMarkingForTest(marking)
	r.gcYieldNow()
	if pauses, _, _ := r.GCYieldStats(); pauses != 1 || *asked != 2 {
		t.Fatalf("%d waits, %d questions after GOMAXPROCS went to 1, want 1 and 2", pauses, *asked)
	}
	// After the wait, the next check reads it again.
	runtime.GOMAXPROCS(2)
	marking, asked = gcMarkingFor(1 << 30)
	r.SetGCMarkingForTest(marking)
	r.gcYieldNow()
	if pauses, _, _ := r.GCYieldStats(); pauses != 1 || *asked != 0 {
		t.Fatalf("%d waits, %d questions after GOMAXPROCS went to 2, want 1 and 0", pauses, *asked)
	}
}

// With the collector's own marking, a wait lasts to the end of a cycle's
// marking, so a run waits no more times than cycles end in it, whatever
// the cycles' timing, and computes the same.
func TestGCYieldWithTheCollector(t *testing.T) {
	r := gcYieldRuntime(t, Config{GCYield: true})
	got, pauses, cycles := gcYieldRun(t, r, 1)
	if got != gcYieldWant || pauses > cycles {
		t.Fatalf("churn = %q with %d waits over %d cycles, want %q with at most as many waits as cycles", got, pauses, cycles, gcYieldWant)
	}
}
