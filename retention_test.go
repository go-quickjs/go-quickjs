package quickjs_test

import (
	"runtime"
	"testing"

	quickjs "github.com/go-quickjs/go-quickjs"
)

// collect runs the collector enough for what it frees to be finalized, and a
// turn for the FinalizationRegistry callbacks it queued to run.
func collect(t *testing.T, rt *quickjs.Runtime) {
	t.Helper()
	for i := 0; i < 3; i++ {
		runtime.GC()
	}
	if _, err := rt.Eval(`0`); err != nil {
		t.Fatal(err)
	}
}

// TestObjectsKeepNothingTheyLetGoOf pins that an object keeps alive only what
// it holds now: not what the room for its first properties held before they
// outgrew it -- a node's old link, which kept every node a splay tree removed
// -- and not, once the turn is over, the receiver of a method it returned
// from.
func TestObjectsKeepNothingTheyLetGoOf(t *testing.T) {
	rt := quickjs.New()
	defer rt.Close()
	if _, err := rt.Eval(`
		var collected = 0;
		var registry = new FinalizationRegistry(function () { collected++; });
		function Node(key) { this.key = key; this.value = null; }
		var kept = [];
		for (var i = 0; i < 100; i++) {
			var n = new Node(i);
			var old = {};
			registry.register(old, "old");
			n.link = old;     // the third property, in the object's own room
			n.more = 1;       // the fourth, which moves them all out of it
			n.link = null;    // what the room held is garbage now
			kept.push(n);
		}
		old = null;`); err != nil {
		t.Fatal(err)
	}
	collect(t, rt)
	if v, err := rt.Eval(`collected`); err != nil || v.Int() != 100 {
		t.Errorf("collected %v of 100 old links, %v", v, err)
	}

	if _, err := rt.Eval(`
		collected = 0;
		function Tree() {}
		Tree.prototype.size = function () { return 0; };
		(function () {
			var tree = new Tree();
			registry.register(tree, "tree");
			tree.size();
		})();`); err != nil {
		t.Fatal(err)
	}
	collect(t, rt)
	if v, err := rt.Eval(`collected`); err != nil || v.Int() != 1 {
		t.Errorf("a method's receiver was collected %v times, want 1, %v", v, err)
	}
}

// TestStaleStackSlotsAreSwept pins that a value left on the stack by code
// that has moved on becomes collectable within the same evaluation, once the
// collector has run and the interpreter has checked in: a program whose turn
// lasts as long as it does -- a server's loop -- keeps no garbage for good.
func TestStaleStackSlotsAreSwept(t *testing.T) {
	rt := quickjs.New()
	defer rt.Close()
	rt.Set("liveMB", func() float64 {
		runtime.GC()
		var m runtime.MemStats
		runtime.ReadMemStats(&m)
		return float64(m.HeapAlloc) / (1 << 20)
	})
	v, err := rt.Eval(`
		var before = liveMB();
		function ignore() {}
		// The array is the fifth argument, deeper on this frame's operand
		// stack than anything after it reaches, so nothing writes over it.
		ignore(0, 0, 0, 0, new Array(2e6).fill(0));
		// The collector runs, and then this frame runs on long enough for the
		// interpreter to check in, which is when the slots are swept.
		liveMB();
		for (var i = 0; i < 20000; i++) {}
		liveMB() - before`)
	if err != nil {
		t.Fatal(err)
	}
	if grown := v.Float(); grown > 8 {
		t.Errorf("%.1f MB is still live after the 32 MB array was let go of", grown)
	}
}
