package vm

import (
	"fmt"
	"runtime"
	"strings"
	"testing"
	"weak"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
)

// emptySiteOf is the cache entry of the first {} of the function the global
// name holds: its site's record (literalkeys.go).
func emptySiteOf(t testing.TB, r *Runtime, name string) *propCache {
	t.Helper()
	cl := globalObject(r, name).fn().closure
	if rc := cl.fn.RegCompiled(); rc != nil {
		for _, w := range rc.Code {
			if w.Op() == bytecode.RNewObject && w.B() == 0 && int(w.C()) < len(cl.ic) {
				return &cl.ic[w.C()]
			}
		}
	}
	t.Fatalf("%s has no {}", name)
	return nil
}

// propCreateBody is prop_create's object (internal/v8bench/microbench.js):
// {} given ten names and ten indices.
const propCreateBody = `function mk() {
	var obj = {};
	obj.a = 1; obj.b = 2; obj.c = 3; obj.d = 4; obj.e = 5;
	obj.f = 6; obj.g = 7; obj.h = 8; obj.i = 9; obj.j = 10;
	for (var i = 0; i < 10; i++) obj[i] = i;
	return obj;
}
`

// A {} site learns the room its own objects come to hold: prop_create's
// objects, given twenty properties, are made from the site's third on with
// a table of twenty, where the fixed room of the realm's root had had each
// leave its room of four and grow its table to 9, 18 and 39. Another
// site's {} is made with the room of four still, and the realm's root keeps
// its room.
func TestEmptySiteLearnsItsRoom(t *testing.T) {
	t.Parallel()
	for _, native := range []bool{false, true} {
		t.Run(fmt.Sprintf("native=%v", native), func(t *testing.T) {
			r := newTestRuntime(t, nativeConfig(native))
			defer r.Close()
			if _, err := r.Run(compileForTest(t, propCreateBody+`
				var objs = [];
				for (var j = 0; j < 200; j++) objs.push(mk());
				var other = (function () { return {} })();`)); err != nil {
				t.Fatal(err)
			}
			objs := globalObject(r, "objs").elemsOf()
			for _, k := range []int{2, 3, 8, 199} {
				o := objs[k].Object()
				if o.valn != 20 || o.valc != 20 || o.shape.unique || o.shape.loose || o.shape.root().rootClass != emptyRootClass {
					t.Errorf("object %d: %d properties in room for %d (unique %v)", k, o.valn, o.valc, o.shape.unique)
				}
			}
			if ic := emptySiteOf(t, r, "mk"); ic.idx != 20 || ic.fills != 0 {
				t.Errorf("the site's record: room %d, %d samples left, want 20 and settled", ic.idx, ic.fills)
			}
			if o := globalObject(r, "other"); o.valc != emptyLiteralRoom {
				t.Errorf("another site's {} made with room for %d, want %d", o.valc, emptyLiteralRoom)
			}
			if root := objs[199].Object().shape.root(); root.slack != emptyLiteralRoom {
				t.Errorf("the realm's root for {} has room %d, want %d", root.slack, emptyLiteralRoom)
			}
			if r.emptySamples != nil {
				t.Errorf("the turn's end left %d samples", len(r.emptySamples))
			}
		})
	}
}

// prop_create's objects take two allocations each, the object and its table
// of twenty, as they did with a root of the site's own, where with the
// realm's root's fixed room they took four.
func TestEmptySiteAllocations(t *testing.T) {
	for _, native := range []bool{false, true} {
		t.Run(fmt.Sprintf("native=%v", native), func(t *testing.T) {
			r := newTestRuntime(t, nativeConfig(native))
			defer r.Close()
			if _, err := r.Run(compileForTest(t, propCreateBody+`
				function loop(n) { var obj; for (var j = 0; j < n; j++) obj = mk(); return obj.j; }
				loop(20);`)); err != nil {
				t.Fatal(err)
			}
			const n = 1000
			call := compileForTest(t, fmt.Sprintf("loop(%d)", n))
			per := testing.AllocsPerRun(20, func() {
				if v, err := r.Run(call); err != nil || v.Number() != 10 {
					t.Fatalf("got %v, %v", v, err)
				}
			}) / n
			if per > 2.05 {
				t.Errorf("%.3f allocations an object, want 2", per)
			}
		})
	}
}

// A {} used as a table of names becomes a dictionary past the soft limit,
// which counts the root's fixed room, and teaches its site nothing: its
// objects are made with the room of four still.
func TestEmptySiteTableTeachesNothing(t *testing.T) {
	t.Parallel()
	r := newTestRuntime(t, Config{})
	defer r.Close()
	if _, err := r.Run(compileForTest(t, `
		function table() { var o = {}; for (var i = 0; i < 100; i++) o["k" + i] = i; return o; }
		var last; for (var j = 0; j < 20; j++) last = table();`)); err != nil {
		t.Fatal(err)
	}
	if o := globalObject(r, "last"); !o.shape.unique && !o.shape.loose {
		t.Fatalf("a table of 100 names is no dictionary")
	}
	if ic := emptySiteOf(t, r, "table"); ic.idx != emptyLiteralRoom || ic.fills != 0 {
		t.Errorf("a table's site: room %d, %d samples left, want %d and settled", ic.idx, ic.fills, emptyLiteralRoom)
	}
}

// A recursive builder makes its children's {} before its own object is
// filled, so the samples its next executions read are its leaves, whole,
// and its inner nodes, not yet: it settles on what the leaves hold, never
// more than an object of it held, and its inner nodes grow past it.
func TestEmptySiteRecursiveBuilder(t *testing.T) {
	t.Parallel()
	r := newTestRuntime(t, Config{})
	defer r.Close()
	if _, err := r.Run(compileForTest(t, `
		function tree(d) {
			var o = {};
			o.kind = "n";
			if (d > 0) { o.l = tree(d - 1); o.r = tree(d - 1); }
			o.a = 1; o.b = 2; o.c = 3; o.d = 4; o.e = 5;
			return o;
		}
		var t; for (var j = 0; j < 4; j++) t = tree(5);`)); err != nil {
		t.Fatal(err)
	}
	if ic := emptySiteOf(t, r, "tree"); ic.idx != 6 || ic.fills != 0 {
		t.Errorf("the builder's site: room %d, %d samples left, want 6 and settled", ic.idx, ic.fills)
	}
	if o := globalObject(r, "t"); o.valn != 8 {
		t.Errorf("the root node: %d properties, want 8", o.valn)
	}
}

// A site whose objects vary settles after slackTracking samples on the most
// one held, as a constructor's root does (noteBuilt), up to maxRoom, and
// learns no more after: its objects of three are made with room for thirty,
// where its root of its own had learned the same of them, and no object's
// room is more than the 252 a constructor's may be.
func TestEmptySiteSettles(t *testing.T) {
	t.Parallel()
	var big strings.Builder
	big.WriteString("function huge() { var o = {};")
	for i := 0; i < 300; i++ {
		fmt.Fprintf(&big, " o.p%d = %d;", i, i)
	}
	big.WriteString(" return o; }")
	// Named stores, which a table's keyed stores past the soft limit are
	// not (TestEmptySiteTableTeachesNothing).
	var vary strings.Builder
	vary.WriteString("function vary(k) { var o = {}; o.v0 = 0; o.v1 = 1; o.v2 = 2; if (k >= 30) {")
	for i := 3; i < 30; i++ {
		fmt.Fprintf(&vary, " o.v%d = %d;", i, i)
	}
	vary.WriteString(" } if (k >= 40) {")
	for i := 30; i < 40; i++ {
		fmt.Fprintf(&vary, " o.v%d = %d;", i, i)
	}
	vary.WriteString(" } return o; }")
	r := newTestRuntime(t, Config{})
	defer r.Close()
	if _, err := r.Run(compileForTest(t, vary.String()+`
		var sizes = [3, 3, 30, 3, 3, 3, 3, 3, 3, 3, 3], small;
		for (var j = 0; j < sizes.length; j++) small = vary(sizes[j]);
		var later = vary(40), after = vary(3);
		`+big.String()+` var h; for (var j = 0; j < 12; j++) h = huge();`)); err != nil {
		t.Fatal(err)
	}
	ic := emptySiteOf(t, r, "vary")
	if ic.idx != 30 || ic.fills != 0 {
		t.Errorf("a site of 3 and 30: room %d, %d samples left, want 30 and settled", ic.idx, ic.fills)
	}
	if o := globalObject(r, "after"); o.valn != 3 || o.valc != 30 {
		t.Errorf("an object of three after settling: room %d, want 30", o.valc)
	}
	if o := globalObject(r, "later"); o.valn != 40 {
		t.Errorf("an object of forty after settling: %d properties", o.valn)
	}
	if ic := emptySiteOf(t, r, "huge"); ic.idx != maxRoom || ic.fills != 0 {
		t.Errorf("a site of 300: room %d, %d samples left, want %d and settled", ic.idx, ic.fills, maxRoom)
	}
	if o := globalObject(r, "h"); o.valn != 300 {
		t.Errorf("an object of 300: %d properties", o.valn)
	}
}

// waitCollection runs the collector until the runtime's sweeps can see it
// has (gcEpoch).
func waitCollection(t testing.TB) {
	t.Helper()
	e := gcEpoch.Load()
	for i := 0; gcEpoch.Load() == e; i++ {
		if i == 1000 {
			t.Fatal("no collection was seen")
		}
		runtime.GC()
		runtime.Gosched()
	}
}

// A sample is held until its site's next execution, a sweep or the turn's
// end, and no longer: a sample in a turn that goes on is let go at the next
// sweep after a collection, read a last time, and is collectable after it;
// one at a turn's end, at the end.
func TestEmptySampleLetGo(t *testing.T) {
	r := newTestRuntime(t, Config{})
	defer r.Close()
	var w weak.Pointer[Object]
	held := -1
	set := func(name string, f func(r *Runtime, this Value, args []Value) (Value, error)) {
		if err := r.SetProp(Obj(r.Global()), r.Intern(name), r.NewFunction(name, 0, f)); err != nil {
			t.Fatal(err)
		}
	}
	set("note", func(r *Runtime, this Value, args []Value) (Value, error) {
		w = weak.Make(args[0].Object())
		held = len(r.emptySamples)
		return Undefined, nil
	})
	set("gc", func(r *Runtime, this Value, args []Value) (Value, error) {
		waitCollection(t)
		return Undefined, nil
	})
	// A collection, and as many back edges as the interpreter checks
	// after (backEdgeCheckInterval), at the first of which it sweeps.
	if _, err := r.Run(compileForTest(t, `function sweep() { gc(); for (var k = 0; k < 1100; k++) {} }`)); err != nil {
		t.Fatal(err)
	}
	var afterSweep int
	var collected bool
	set("check", func(r *Runtime, this Value, args []Value) (Value, error) {
		afterSweep = len(r.emptySamples)
		for i := 0; i < 10 && w.Value() != nil; i++ {
			runtime.GC()
		}
		collected = w.Value() == nil
		return Undefined, nil
	})
	if _, err := r.Run(compileForTest(t, `
		function mk(i) { var o = {}; o.a = i; o.b = i; o.c = i; o.d = i; o.e = i; o.f = i; return o; }
		mk(0);
		note(mk(1));
		sweep();
		gc();
		check();`)); err != nil {
		t.Fatal(err)
	}
	if held != 1 {
		t.Fatalf("the site's second object: %d samples held, want 1", held)
	}
	if afterSweep != 0 {
		t.Errorf("after a sweep, %d samples held", afterSweep)
	}
	if !collected {
		t.Errorf("a sample let go at a sweep was not collected")
	}
	if ic := emptySiteOf(t, r, "mk"); ic.idx != 6 || ic.fills != slackTracking-1 {
		t.Errorf("the sweep's read: room %d, %d samples left, want 6 and %d", ic.idx, ic.fills, slackTracking-1)
	}
	// At a turn's end.
	if _, err := r.Run(compileForTest(t, `note(mk(2)); 0`)); err != nil {
		t.Fatal(err)
	}
	if r.emptySamples != nil {
		t.Errorf("the turn's end left %d samples", len(r.emptySamples))
	}
	for i := 0; i < 10 && w.Value() != nil; i++ {
		runtime.GC()
	}
	if w.Value() != nil {
		t.Errorf("a sample let go at the turn's end was not collected")
	}
}

// A runtime whose code has many sites learning at once holds a sample for
// each, the object each made last, and no more; and a script that never
// ends its turn, making new code with a {} at each turn of its loop, holds
// no more than the sites that learned since the last sweep.
func TestEmptySamplesBounded(t *testing.T) {
	r := newTestRuntime(t, Config{})
	defer r.Close()
	var bad []string
	most, last := 0, -1
	set := func(name string, f func(r *Runtime, this Value, args []Value) (Value, error)) {
		if err := r.SetProp(Obj(r.Global()), r.Intern(name), r.NewFunction(name, 0, f)); err != nil {
			t.Fatal(err)
		}
	}
	set("probe", func(r *Runtime, this Value, args []Value) (Value, error) {
		fns := args[0].Object().elemsOf()
		lasts := args[1].Object().elemsOf()
		if len(r.emptySamples) != len(fns) {
			bad = append(bad, fmt.Sprintf("%d samples for %d sites", len(r.emptySamples), len(fns)))
		}
		for i, f := range fns {
			cl := f.Object().fn().closure
			for _, w := range cl.fn.RegCompiled().Code {
				if w.Op() == bytecode.RNewObject {
					if o := r.emptySamples[&cl.ic[w.C()]]; o != lasts[i].Object() {
						bad = append(bad, fmt.Sprintf("site %d holds no sample of its last object", i))
					}
				}
			}
		}
		return Undefined, nil
	})
	set("count", func(r *Runtime, this Value, args []Value) (Value, error) {
		most = max(most, len(r.emptySamples))
		last = len(r.emptySamples)
		return Undefined, nil
	})
	set("gc", func(r *Runtime, this Value, args []Value) (Value, error) {
		waitCollection(t)
		return Undefined, nil
	})
	// A collection, and as many back edges as the interpreter checks
	// after (backEdgeCheckInterval), at the first of which it sweeps.
	if _, err := r.Run(compileForTest(t, `function sweep() { gc(); for (var k = 0; k < 1100; k++) {} }`)); err != nil {
		t.Fatal(err)
	}
	// Sites of their own: a function each.
	sites := func(n int) string {
		var b strings.Builder
		b.WriteString("[")
		for i := 0; i < n; i++ {
			fmt.Fprintf(&b, "function (x) { var o = {}; o.x%d = x; return o }, ", i)
		}
		b.WriteString("]")
		return b.String()
	}
	if _, err := r.Run(compileForTest(t, `
		var fns = `+sites(50)+`, lasts = [];
		for (var round = 0; round < 4; round++)
			for (var i = 0; i < fns.length; i++) lasts[i] = fns[i](round);
		probe(fns, lasts);`)); err != nil {
		t.Fatal(err)
	}
	for _, b := range bad[:min(len(bad), 5)] {
		t.Error(b)
	}
	if _, err := r.Run(compileForTest(t, `
		var many = `+sites(3000)+`;
		for (var i = 0; i < many.length; i++) {
			var f = many[i];
			f(i); f(i);
			count();
			if (i % 100 == 99) sweep();
		}
		sweep();
		count();`)); err != nil {
		t.Fatal(err)
	}
	if most > 101 || last != 0 {
		t.Errorf("a turn of 3,000 sites, a sweep each 100: at most %d samples held, %d after the last sweep", most, last)
	}
}

// A site's record is its realm's code's: the same code in two realms learns
// in each what its objects there hold.
func TestEmptySiteRealms(t *testing.T) {
	t.Parallel()
	r := newTestRuntime(t, Config{})
	defer r.Close()
	fn := compileForTest(t, `function mk(k) { var o = {}; for (var i = 0; i < k; i++) o["v" + i] = i; return o; }`)
	var sites []*propCache
	for _, k := range []int{5, 12} {
		re := r.NewRealm()
		if _, err := r.RunIn(re, fn); err != nil {
			t.Fatal(err)
		}
		v, err := r.RunIn(re, compileForTest(t, fmt.Sprintf("var last; for (var j = 0; j < 10; j++) last = mk(%d); last", k)))
		if err != nil {
			t.Fatal(err)
		}
		if o := v.Object(); o.proto() != re.proto.object || int(o.valc) != k {
			t.Fatalf("realm of %d: room %d", k, o.valc)
		}
		cl := re.global.getOwn(r.atoms.intern("mk")).value().Object().fn().closure
		for _, w := range cl.fn.RegCompiled().Code {
			if w.Op() == bytecode.RNewObject {
				sites = append(sites, &cl.ic[w.C()])
			}
		}
	}
	if len(sites) != 2 || sites[0] == sites[1] || sites[0].idx != 5 || sites[1].idx != 12 {
		t.Errorf("the realms' sites did not learn apart")
	}
}

// A Program's {} sites keep their records from run to run: the first
// object of a run after the first is made with the room the runs before
// learned, in the realm it learned it in; code run anew learns anew.
func TestEmptySiteProgramRerun(t *testing.T) {
	t.Parallel()
	r := newTestRuntime(t, Config{})
	defer r.Close()
	fn := compileForTest(t, propCreateBody+`var a = mk(), b = mk(), c = mk(); a`)
	if o := runProgramForTest(t, r, fn).Object(); o.valc == 20 {
		t.Fatalf("a first run's first object made with room for 20")
	}
	for run := 1; run < 4; run++ {
		if o := runProgramForTest(t, r, fn).Object(); o.valn != 20 || o.valc != 20 {
			t.Errorf("run %d's first object: %d properties in room for %d, want 20 in 20", run, o.valn, o.valc)
		}
	}
	re := r.NewRealm()
	var o *Object
	r.InRealm(re, func() { o = runProgramForTest(t, r, fn).Object() })
	if o.valc == 20 {
		t.Errorf("another realm's first run took this realm's room")
	}
	if v, err := r.Run(compileForTest(t, propCreateBody+`mk()`)); err != nil || v.Object().valc == 20 {
		t.Errorf("code run anew took a Program's room: %v", err)
	}
}

// A site of objects of three and of thirty, settled on thirty, makes its
// objects of three in the 512-byte class, the object and a table of thirty:
// what the site's own root, learning the same, had made of them (513.5
// bytes and 2.01 allocations an object at 469952db5, against 97 bytes and
// one with the fixed room, whose objects of thirty took 1,168 bytes and
// four), no more.
func TestEmptySiteVaryingBytes(t *testing.T) {
	var vary strings.Builder
	vary.WriteString("function vary(k) { var o = {}; o.v0 = 0; o.v1 = 1; o.v2 = 2; if (k >= 30) {")
	for i := 3; i < 30; i++ {
		fmt.Fprintf(&vary, " o.v%d = %d;", i, i)
	}
	vary.WriteString(" } return o; }")
	r := newTestRuntime(t, Config{})
	defer r.Close()
	if _, err := r.Run(compileForTest(t, vary.String()+`
		var sizes = [3, 3, 30, 3, 3, 3, 3, 3, 3, 3, 3];
		for (var j = 0; j < sizes.length; j++) vary(sizes[j]);
		function loop(n, k) { var o; for (var j = 0; j < n; j++) o = vary(k); return o; }`)); err != nil {
		t.Fatal(err)
	}
	const n = 10000
	for _, k := range []int{3, 30} {
		call := compileForTest(t, fmt.Sprintf("loop(%d, %d)", n, k))
		var a, b runtime.MemStats
		runtime.GC()
		runtime.ReadMemStats(&a)
		v, err := r.Run(call)
		runtime.ReadMemStats(&b)
		if err != nil {
			t.Fatal(err)
		}
		bytes, allocs := float64(b.TotalAlloc-a.TotalAlloc)/n, float64(b.Mallocs-a.Mallocs)/n
		if o := v.Object(); o.valc != 30 || bytes > 520 || allocs > 2.05 {
			t.Errorf("objects of %d: room %d, %.1f bytes and %.2f allocations an object, want 30 and at most 520 and 2", k, o.valc, bytes, allocs)
		}
	}
}
