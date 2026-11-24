package vm

import (
	"runtime/debug"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// The collector's percent is the process's (debug.SetGCPercent), and the
// package's tests run in parallel: each that holds the collector off, or
// paces it, does so through collector, which counts what its tests ask
// for and sets the percent they make together. Saved and restored by each
// test on its own, as defer debug.SetGCPercent(debug.SetGCPercent(-1))
// does, two overlapping holds could leave it off for good: the first saves
// 100 and turns it off, the second saves -1, the first restores 100, the
// second -1, and every test after ran with no collection. Under
// QJS_JIT_STRESS, TestStrAppendRandom's strings, gigabytes of them, then
// grew the package's process past 77 GB.
//
// It also keeps the package's memory-heavy tests (heavy) and its holds
// apart: a hold waits until no heavy test runs, and a heavy test until no
// hold does, a waiting hold first, so that no heavy test's garbage piles up
// while the collector is off. A test that is both, or is inside one that
// is, waits for neither. Neither waits past collectorWait, after which it
// goes on all the same: a hold taken before t.Parallel holds while its test
// waits for the serial ones, and a heavy test among those would otherwise
// wait for it for ever.
var collector = newCollectorGate(debug.SetGCPercent, collectorWait)

// collectorWait is the longest a hold waits for the heavy tests running,
// and a heavy test for the holds.
const collectorWait = 30 * time.Second

// collectorGate is what the process's collector is asked for by the
// package's tests: holds, which turn it off; paces, each a percent, the
// least of which it runs at while nothing holds it; and the heavy tests,
// which no hold runs beside. set is the collector's (debug.SetGCPercent).
type collectorGate struct {
	mu   sync.Mutex
	set  func(int) int
	wait time.Duration
	// base is the percent before the first of what the gate was asked
	// for, which it is set back to once nothing is, and based whether it
	// has been read since.
	base  int
	based bool
	// holders, paces and heavy are the names of the tests holding the
	// collector off, pacing it -- each with its percent -- and running
	// heavy, one entry each time.
	holders []string
	paces   []collectorPace
	heavy   []string
	// holdsWaiting counts the holds waiting for heavy tests to end, which
	// a heavy test that has not begun waits for in turn.
	holdsWaiting int
}

type collectorPace struct {
	name    string
	percent int
}

func newCollectorGate(set func(int) int, wait time.Duration) *collectorGate {
	return &collectorGate{set: set, wait: wait}
}

// related reports whether the tests named a and b are one, or one is
// inside the other: a subtest's name begins with its parent's and a slash.
func related(a, b string) bool {
	return a == b || strings.HasPrefix(a, b+"/") || strings.HasPrefix(b, a+"/")
}

// anyRelated reports whether a test of names is name or related to it.
func anyRelated(names []string, name string) bool {
	return slices.ContainsFunc(names, func(n string) bool { return related(n, name) })
}

// anyUnrelated reports whether a test of names is not related to name.
func anyUnrelated(names []string, name string) bool {
	return slices.ContainsFunc(names, func(n string) bool { return !related(n, name) })
}

// begin reads the collector's percent, the gate's base, where nothing
// was asked of it: what it is set back to once nothing is. The gate's
// lock is held.
func (g *collectorGate) begin() {
	if !g.based {
		g.base, g.based = g.set(-1), true
		g.set(g.base)
	}
}

// apply sets the collector's percent to what the gate is asked for: off
// while anything holds it, else the least percent paced, else the base,
// which the next ask reads anew. The gate's lock is held.
func (g *collectorGate) apply() {
	switch {
	case len(g.holders) != 0:
		g.set(-1)
	case len(g.paces) != 0:
		want := g.paces[0].percent
		for _, p := range g.paces[1:] {
			want = min(want, p.percent)
		}
		g.set(want)
	default:
		g.set(g.base)
		g.based = false
	}
}

// waitWhile waits, its lock held and let go of between looks, while busy
// says so, up to the gate's wait.
func (g *collectorGate) waitWhile(busy func() bool) {
	deadline := time.Now().Add(g.wait)
	for busy() && time.Now().Before(deadline) {
		g.mu.Unlock()
		time.Sleep(5 * time.Millisecond)
		g.mu.Lock()
	}
}

// hold turns the collector off for the test named name until the release
// it returns is called, once or more: after the heavy tests running end,
// unless it is one of them, inside one, or holds it already.
func (g *collectorGate) hold(name string) (release func()) {
	g.mu.Lock()
	if !anyRelated(g.heavy, name) && !anyRelated(g.holders, name) {
		g.holdsWaiting++
		g.waitWhile(func() bool { return anyUnrelated(g.heavy, name) })
		g.holdsWaiting--
	}
	g.begin()
	g.holders = append(g.holders, name)
	g.apply()
	g.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			g.mu.Lock()
			g.holders = slices.Delete(g.holders, slices.Index(g.holders, name), slices.Index(g.holders, name)+1)
			g.apply()
			g.mu.Unlock()
		})
	}
}

// pace has the collector run at percent, or the least percent paced, for
// the test named name while nothing holds it off, until the release it
// returns is called.
func (g *collectorGate) pace(name string, percent int) (release func()) {
	g.mu.Lock()
	g.begin()
	g.paces = append(g.paces, collectorPace{name, percent})
	g.apply()
	g.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			g.mu.Lock()
			i := slices.Index(g.paces, collectorPace{name, percent})
			g.paces = slices.Delete(g.paces, i, i+1)
			g.apply()
			g.mu.Unlock()
		})
	}
}

// enterHeavy has the heavy test named name wait for the holds running,
// and those waiting, to end -- unless it holds the collector itself, or a
// test it is inside or that is inside it does -- and returns its leaving.
func (g *collectorGate) enterHeavy(name string) (leave func()) {
	g.mu.Lock()
	if !anyRelated(g.holders, name) {
		g.waitWhile(func() bool { return anyUnrelated(g.holders, name) || g.holdsWaiting != 0 })
	}
	g.heavy = append(g.heavy, name)
	g.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			g.mu.Lock()
			i := slices.Index(g.heavy, name)
			g.heavy = slices.Delete(g.heavy, i, i+1)
			g.mu.Unlock()
		})
	}
}

// tbName is t's name, or none for no test.
func tbName(t testing.TB) string {
	if t == nil {
		return ""
	}
	return t.Name()
}

// holdCollector holds the process's collector off for t, until t and its
// cleanups are done (collector).
func holdCollector(t testing.TB) {
	t.Helper()
	t.Cleanup(collector.hold(tbName(t)))
}

// holdCollectorNow holds the process's collector off for t, nil for no
// test, until the release it returns is called: defer
// holdCollectorNow(t)() for a function's run.
func holdCollectorNow(t testing.TB) (release func()) {
	return collector.hold(tbName(t))
}

// paceCollector has the process's collector run at percent for t while
// nothing holds it off, until t and its cleanups are done.
func paceCollector(t testing.TB, percent int) {
	t.Helper()
	t.Cleanup(collector.pace(tbName(t), percent))
}

// paceCollectorNow is paceCollector until the release it returns is
// called.
func paceCollectorNow(t testing.TB, percent int) (release func()) {
	return collector.pace(tbName(t), percent)
}

// testGate is a collector gate over a percent of its own, starting at 100,
// that waits at most wait.
func testGate(wait time.Duration) (*collectorGate, func() int) {
	var mu sync.Mutex
	percent := 100
	set := func(p int) int {
		mu.Lock()
		defer mu.Unlock()
		old := percent
		percent = p
		return old
	}
	read := func() int {
		mu.Lock()
		defer mu.Unlock()
		return percent
	}
	return newCollectorGate(set, wait), read
}

// within fails t unless f returns within d.
func within(t *testing.T, d time.Duration, what string, f func()) {
	t.Helper()
	done := make(chan struct{})
	go func() { f(); close(done) }()
	select {
	case <-done:
	case <-time.After(d):
		t.Fatalf("%s did not return within %v", what, d)
	}
}

// Overlapping holds, released in either order, give the collector back
// its percent, and a pace runs it at its percent while nothing holds it:
// each saving and restoring the percent on its own, the second of two
// overlapping holds had restored the first's -1, the collector off for
// the rest of the package.
func TestCollectorHoldsRestoreThePercent(t *testing.T) {
	for _, firstOut := range []bool{true, false} {
		g, percent := testGate(time.Second)
		a, b := g.hold("A"), g.hold("B")
		if p := percent(); p != -1 {
			t.Fatalf("two holds: percent %d, want -1", p)
		}
		if firstOut {
			a()
		} else {
			b()
		}
		if p := percent(); p != -1 {
			t.Fatalf("one hold left: percent %d, want -1", p)
		}
		a()
		b()
		if p := percent(); p != 100 {
			t.Fatalf("holds released, first out %v: percent %d, want 100", firstOut, p)
		}
	}
	g, percent := testGate(time.Second)
	pace := g.pace("P", 1)
	if p := percent(); p != 1 {
		t.Fatalf("paced: percent %d, want 1", p)
	}
	hold := g.hold("H")
	if p := percent(); p != -1 {
		t.Fatalf("paced and held: percent %d, want -1", p)
	}
	hold()
	if p := percent(); p != 1 {
		t.Fatalf("hold released under a pace: percent %d, want 1", p)
	}
	pace()
	if p := percent(); p != 100 {
		t.Fatalf("pace released: percent %d, want 100", p)
	}
}

// A hold waits for the heavy tests running, and a heavy test for the holds;
// but none waits where it would wait for itself: a test both heavy and
// holding, a hold inside a heavy test, a hold nested in one of its own.
func TestCollectorHeavyAndHoldsNoDeadlock(t *testing.T) {
	g, percent := testGate(time.Minute)
	within(t, 5*time.Second, "a heavy test's own hold", func() {
		leave := g.enterHeavy("T")
		release := g.hold("T")
		inner := g.hold("T/sub")
		if p := percent(); p != -1 {
			t.Errorf("held: percent %d, want -1", p)
		}
		inner()
		release()
		leave()
	})
	within(t, 5*time.Second, "a holder's heavy work", func() {
		release := g.hold("H")
		leave := g.enterHeavy("H/seed")
		leave()
		release()
	})
	within(t, 5*time.Second, "a nested hold while a heavy test waits", func() {
		release := g.hold("N")
		entered := make(chan func())
		go func() { entered <- g.enterHeavy("Heavy") }()
		time.Sleep(20 * time.Millisecond)
		again := g.hold("N")
		again()
		release()
		(<-entered)()
	})
	if p := percent(); p != 100 {
		t.Fatalf("after: percent %d, want 100", p)
	}
	// A hold waits for an unrelated heavy test to end.
	leave := g.enterHeavy("W")
	held := make(chan func())
	go func() { held <- g.hold("X") }()
	select {
	case <-held:
		t.Fatal("a hold began beside a heavy test")
	case <-time.After(50 * time.Millisecond):
	}
	leave()
	within(t, 5*time.Second, "a hold after the heavy test", func() { (<-held)() })
}

// A test that holds the collector and then waits in t.Parallel for the
// serial tests holds it while they run: a serial heavy test among them
// waits for it no longer than the gate's wait, and goes on.
func TestCollectorHoldBeforeParallel(t *testing.T) {
	g, _ := testGate(100 * time.Millisecond)
	within(t, 10*time.Second, "the subtests", func() {
		t.Run("group", func(t *testing.T) {
			t.Run("holder", func(t *testing.T) {
				t.Cleanup(g.hold(t.Name()))
				t.Parallel()
			})
			t.Run("serial heavy", func(t *testing.T) {
				g.enterHeavy(t.Name())()
			})
		})
	})
}
