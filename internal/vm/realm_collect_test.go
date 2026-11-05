package vm

import (
	"runtime"
	"testing"
	"time"
	"weak"
)

// realmsAlive collects until no more of refs go, a turn of the runtime's
// between collections, as a host that runs on has, and says how many are
// left: what is queued for a finalization callback holds a realm until the
// turn after the cleanup has run, on a goroutine of its own.
func realmsAlive(r *Runtime, refs []weak.Pointer[Realm]) int {
	left := len(refs)
	for range 8 {
		runtime.GC()
		runtime.GC()
		time.Sleep(time.Millisecond)
		r.DrainJobs()
		left = 0
		for _, w := range refs {
			if w.Value() != nil {
				left++
			}
		}
		if left == 0 {
			break
		}
	}
	return left
}

// A registry a script has let go of goes on working, while its realm lives,
// for as long as it has cells: a registration's cleanup holds the cell only
// weakly, so that a target of a realm no longer keeps the realm (and what
// held the registry had been the cleanup). Once its cells are gone so is it.
func TestFinalizationRegistryKeptByItsRealm(t *testing.T) {
	r := New(Config{})
	defer r.Close()
	if _, err := r.Run(compileForTest(t, `
		var seen = [];
		(function () {
			new FinalizationRegistry(h => seen.push(h)).register({}, "held");
		})();`)); err != nil {
		t.Fatal(err)
	}
	read := compileForTest(t, `seen.length`)
	v := Undefined
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		runtime.GC()
		time.Sleep(time.Millisecond)
		var err error
		if v, err = r.Run(read); err != nil {
			t.Fatal(err)
		}
		if err := r.DrainJobs(); err != nil {
			t.Fatal(err)
		}
		if v.Number() != 0 {
			break
		}
	}
	if v.Number() != 1 {
		t.Fatalf("the callback ran %v times", v)
	}
	if len(r.finalizing) != 1 {
		t.Fatalf("%d registries kept", len(r.finalizing))
	}
	// The set is looked through as it doubles, not at each registration.
	if _, err := r.Run(compileForTest(t, `
		for (var i = 0; i < 100; i++) {
			var t = {};
			var fr = new FinalizationRegistry(h => h);
			fr.register(t, "held", t);
			fr.unregister(t);
		}`)); err != nil {
		t.Fatal(err)
	}
	if n := len(r.finalizing); n > 32 {
		t.Errorf("%d registries of no cells kept", n)
	}
}
