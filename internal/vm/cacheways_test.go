package vm

import (
	"fmt"
	"runtime"
	"strings"
	"testing"
	"unsafe"
)

// waysCtors is a script of 100 constructors, each of whose objects has a
// layout of its own (one key of its own), an addition site add that meets
// them all and so gives up its cache, and a read site rd of their y.
var waysCtors = func() string {
	src := "var ctors = [];\n"
	for i := range 100 {
		src += fmt.Sprintf("ctors.push(function () { this.k%d = 0 });\n", i)
	}
	return src
}() + `
function add(o, v) { o.x = v; return o }
function rd(o) { return o.y }
function warm() { var s = 0; for (var i = 0; i < ctors.length; i++) { var o = add(new ctors[i](), 1); o.y = i; s += rd(o) } return s }
warm(); warm();
function adds(picks, n) { for (var r = 0; r < n; r++) for (var i = 0; i < picks.length; i++) add(new ctors[picks[i]](), r) }
`

// deepProbe has the script's probe(), which it calls after each addition
// it makes, check whether r's cache of additions (deepAdds) changed since
// the last call: an entry made, or one moved to the front of its set,
// which only an addition made the long way does (fillDeepStore). It
// returns the number of changes seen so far, which reset sets to zero.
// The collector is held off until t ends: a collection's end lets the
// sites' caches and the cache of additions start again.
func deepProbe(t *testing.T, r *Runtime) (changes func() int, reset func()) {
	runtime.GC()
	holdCollector(t)
	var last deepAdds
	n := 0
	probe := r.NewFunction("probe", 0, func(r *Runtime, this Value, args []Value) (Value, error) {
		if r.deep != nil && *r.deep != last {
			n++
			last = *r.deep
		}
		return Undefined, nil
	})
	if err := r.SetProp(Obj(r.Global()), r.Intern("probe"), probe); err != nil {
		t.Fatal(err)
	}
	return func() int { return n }, func() {
		n = 0
		if r.deep != nil {
			last = *r.deep
		}
	}
}

// The realm's cache of additions (deepAdds) holds four to a set: four
// additions whose shapes and keys share a set -- the layouts picked so,
// from those made until five share one -- keep their entries, each found
// and none made again, however many times they are made in turn, where
// five in turn displace one another, each made again. Each addition is made as a write at a
// site that has given up makes it (setPropCached): looked up
// (deepStore), and where it is not found made and remembered
// (fillDeepStore).
func TestDeepAddsFourWays(t *testing.T) {
	t.Parallel()
	src := `var ctors = [], made = [];
function batch(picks, n) { var out = []; for (var r = 0; r < n; r++) for (var i = 0; i < picks.length; i++) out.push(new ctors[picks[i]]()); return out }`
	// more makes the layouts from..from+599, each a constructor's.
	more := func(from int) string {
		var b strings.Builder
		for i := from; i < from+600; i++ {
			fmt.Fprintf(&b, "ctors.push(function () { this.k%d = 0 }); made.push(new ctors[%d]());\n", i, i)
		}
		return b.String()
	}
	r := newTestRuntime(t, Config{})
	defer r.Close()
	run := func(src string) Value {
		t.Helper()
		v, err := r.Run(compileForTest(t, src))
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	run(src)
	// The layouts by the set their addition of x is in, made 600 at a time
	// until five share one: the sets are hashed from the shapes' addresses,
	// the heap's. Of 4,000 runtimes' first 600 layouts, the fullest set
	// held exactly five in some 2%, so that now and then a heap put no five
	// in one; up to 4,800 make the chance of none nil.
	x := r.atoms.intern("x")
	sets := map[uintptr][]int{}
	var five []int
	made := 0
	for five == nil && made < 4800 {
		run(more(made))
		elems := r.global.getOwn(r.atoms.intern("made")).value().Object().elemsOf()
		for i := made; i < len(elems) && five == nil; i++ {
			set := deepSetOf(elems[i].Object().shape, x)
			if sets[set] = append(sets[set], i); len(sets[set]) == deepWays+1 {
				five = sets[set]
			}
		}
		made = len(elems)
	}
	if five == nil {
		t.Fatalf("no five of %d layouts whose additions share a set", made)
	}
	// adds makes the additions of x to objects of picks' layouts, n
	// rounds of them, and returns how many were not found.
	adds := func(picks []int, n int) (misses int) {
		for i, v := range run(`batch(` + jsInts(picks) + `, ` + fmt.Sprint(n) + `)`).Object().elemsOf() {
			o := v.Object()
			if r.deepStore(o, x, Float(float64(i))) {
				continue
			}
			misses++
			before := o.shape
			if _, err := r.setProp(o, x, Float(float64(i)), Obj(o), false); err != nil {
				t.Fatal(err)
			}
			r.fillDeepStore(o, before, x)
		}
		return misses
	}
	for _, c := range []struct {
		picks []int
		fills bool
	}{{five[:deepWays], false}, {five, true}} {
		adds(c.picks, 2)
		n := adds(c.picks, 100)
		switch {
		case !c.fills && n != 0:
			t.Errorf("%v: %d of %d additions made again: some looked up again", c.picks, n, 100*len(c.picks))
		case c.fills && n == 0:
			t.Errorf("%v: no addition of %d in one set of four ways made again, want some", c.picks, 100*len(c.picks))
		}
	}
}

// The same additions, in sets as the shapes' addresses put them, in
// runtimes whose heaps differ by what was allocated before each: 24
// classes' three additions each, as Class.create's initializers make
// them, are each found after the first round in every runtime, none made
// again. Direct-mapped, from 1 in 35 to 3 in 4 of them had been made the
// long way, by where the layouts were.
func TestDeepAddsAnyHeap(t *testing.T) {
	fn := compileForTest(t, `
var Class = { create: function () { return function () { this.initialize.apply(this, arguments) } } };
var classes = [];
for (var k = 0; k < 24; k++) {
  var C = Class.create();
  C.prototype = { initialize: function (x, y, z) { this.x = x; this.y = y; this.z = z } };
  classes.push(C);
}
function run(n) { var s = 0; for (var i = 0; i < n; i++) for (var j = 0; j < 24; j++) { var o = new classes[j](i, 1, 2); probe(); s += o.y } return s }
run(50);`)
	var keep [][]byte
	for seed := range 20 {
		keep = append(keep, make([]byte, 112*(seed*37%61+1)))
		r := newTestRuntime(t, Config{})
		changes, reset := deepProbe(t, r)
		if _, err := r.Run(fn); err != nil {
			t.Fatal(err)
		}
		reset()
		if _, err := r.Run(compileForTest(t, `run(100)`)); err != nil {
			t.Fatal(err)
		}
		if n := changes(); n != 0 {
			t.Errorf("seed %d: the cache changed after %d of %d objects' additions", seed, n, 24*100)
		}
		// And it holds the classes' additions: the cache is where they
		// were found.
		held := 0
		if r.deep != nil {
			for _, e := range r.deep {
				if k := r.atoms.name(e.key); e.from != nil && (k == "x" || k == "y" || k == "z") {
					held++
				}
			}
		}
		if held == 0 {
			t.Errorf("seed %d: the cache holds none of the classes' additions", seed)
		}
		r.Close()
	}
	_ = keep
}

// A read site that has given up keeps in its own table (megaOwn) where
// its property is in objects of each layout, four to a set: four layouts
// of one set, read in turn, keep their entries however many times they
// are read, which the set's order, changed by each entry made, shows; a
// fifth displaces the oldest. That is the Go table's, whose reads ask it;
// the native handlers read a small table's own property themselves at such
// a site (nativegen's megaRead), and leave it no table to make.
func TestMegaOwnFourWays(t *testing.T) {
	t.Parallel()
	for _, native := range []bool{false, true} {
		t.Run(fmt.Sprint("native=", native), func(t *testing.T) { megaOwnFourWays(t, native) })
	}
}

func megaOwnFourWays(t *testing.T, native bool) {
	r := newTestRuntime(t, nativeConfig(native))
	defer r.Close()
	if native && !r.runNative {
		// What it checks of the native handlers is not there to check:
		// a build without them (386, QJS_NONATIVE) runs the Go table's.
		t.Skip("no native handlers in this build")
	}
	if _, err := r.Run(compileForTest(t, waysCtors+`
var objs = [];
for (var i = 0; i < ctors.length; i++) { var o = new ctors[i](); o.y = i; objs.push(o) }
function rds(picks, n) { var s = 0; for (var r = 0; r < n; r++) for (var i = 0; i < picks.length; i++) s += rd(objs[picks[i]]); return s }`)); err != nil {
		t.Fatal(err)
	}
	objs := r.global.getOwn(r.atoms.intern("objs")).value().Object().elemsOf()
	sets := map[uintptr][]int{}
	var five []int
	for i, v := range objs {
		set := megaSet(v.Object().shape)
		if sets[set] = append(sets[set], i); len(sets[set]) == megaWays+1 {
			five = sets[set]
			break
		}
	}
	if five == nil {
		t.Fatal("no five layouts share a set")
	}
	rd := r.global.getOwn(r.atoms.intern("rd")).value().Object().fn().closure
	var m *megaOwn
	for i := range rd.ic {
		if p := rd.ic[i].poly; p != nil && p.mega != nil {
			m = p.mega
		}
	}
	if native {
		if m != nil {
			t.Error("rd's reads of objects' own small tables made it a table of its own: Go's, not the native handlers'")
		}
		return
	}
	if m == nil {
		t.Fatal("rd has no table of its own")
	}
	picks := jsInts
	run := func(src string) {
		t.Helper()
		if _, err := r.Run(compileForTest(t, src)); err != nil {
			t.Fatal(err)
		}
	}
	ways := (*[megaWays]megaEntry)(m[megaSet(objs[five[0]].Object().shape):])
	run(`rds(` + picks(five[:megaWays]) + `, 2)`)
	// Read one at a time, so that an entry made again shows in the order
	// of the set, which four made again in turn would bring back.
	before := *ways
	for k := range 4 * megaWays {
		run(`rds([` + fmt.Sprint(five[k%megaWays]) + `], 1)`)
		if *ways != before {
			t.Fatalf("four layouts of one set: an entry made again at read %d: %v, was %v", k, *ways, before)
		}
	}
	run(`rds(` + picks(five[:megaWays]) + `, 100)`)
	if *ways != before {
		t.Errorf("four layouts of one set: entries made again: %v, was %v", *ways, before)
	}
	for _, i := range five[:megaWays] {
		found := false
		for _, e := range ways {
			found = found || e.shape == objs[i].Object().shape
		}
		if !found {
			t.Errorf("objs[%d]'s layout has no entry", i)
		}
	}
	run(`rds([` + fmt.Sprint(five[megaWays]) + `], 1)`)
	if ways[0].shape != objs[five[megaWays]].Object().shape {
		t.Error("a fifth layout is not the newest of its set")
	}
	for _, e := range ways {
		if e.shape == objs[five[0]].Object().shape {
			t.Error("the oldest layout is still in its set")
		}
	}
}

// jsInts is ix as a script's array literal.
func jsInts(ix []int) string {
	s := "["
	for i, x := range ix {
		if i > 0 {
			s += ", "
		}
		s += fmt.Sprint(x)
	}
	return s + "]"
}

// The table of a forwarder's method reads (fwdSite) keeps up to four
// layouts to an entry, its own and its others (poly), which native code
// reads too: two classes whose layouts meet in an entry, constructed in
// turn, are each filled once, where each had been filled again at every
// construction until the entry gave up after sixteen.
func TestFwdTableEntryKeepsItsOthers(t *testing.T) {
	t.Parallel()
	r := newTestRuntime(t, Config{})
	defer r.Close()
	if _, err := r.Run(compileForTest(t, `
var Class = { create: function () { return function () { this.initialize.apply(this, arguments) } } };
var classes = [];
for (var k = 0; k < 100; k++) {
  var C = Class.create();
  C.prototype = { initialize: function (x) { this.x = x } };
  classes.push(C);
}
for (var r = 0; r < 3; r++) for (var k = 0; k < 100; k++) new classes[k](r);
function fresh(k) { return new classes[k](0) }`)); err != nil {
		t.Fatal(err)
	}
	objs := make([]*Object, 100)
	first := map[uintptr]int{}
	pair := [2]int{-1, -1}
	for k := range objs {
		v, err := r.Run(compileForTest(t, fmt.Sprintf("fresh(%d)", k)))
		if err != nil {
			t.Fatal(err)
		}
		objs[k] = v.Object()
		if objs[k].flags&objPolyProto != 0 {
			t.Fatalf("classes[%d]'s object keeps its prototype in its table", k)
		}
		slot := uintptr(unsafe.Pointer(objs[k].shape)) >> fwdShift % fwdWays
		if j, ok := first[slot]; ok && pair[0] < 0 && objs[j].shape != objs[k].shape {
			pair = [2]int{j, k}
		}
		first[slot] = k
	}
	if pair[0] < 0 {
		t.Fatal("no two layouts share an entry")
	}
	v, err := r.Run(compileForTest(t, "classes[0]"))
	if err != nil {
		t.Fatal(err)
	}
	cl := v.Object().fn().closure
	c := cl.reg
	site := int(c.fwdSites & 0xffff)
	key := cl.names[c.siteName[site]]
	cl.ic[site].fills = maxCacheFills
	cl.fwd = nil
	var e *propCache
	for i := range 100 {
		e = r.fwdCache(cl, site, objs[pair[i%2]], key)
	}
	if e.fills != 2 {
		t.Errorf("two layouts of one entry, constructed in turn: %d fills, want 2", e.fills)
	}
	for _, k := range pair {
		if m, ok := r.cachedData(e, objs[k], key); !ok || !m.IsObject() {
			t.Errorf("classes[%d]'s method not answered by its entry", k)
		}
	}
}
