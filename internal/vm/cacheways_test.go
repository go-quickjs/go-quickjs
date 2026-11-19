package vm

import (
	"fmt"
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

// The realm's cache of additions (deepAdds) holds four to a set: four
// additions whose shapes and keys share a set -- here every set is one,
// deepSetOf replaced -- keep their entries, each found and none made
// again, however many times they are made in turn, where five in turn
// displace one another. The set is counted by deepSetOf's calls: one for
// each addition looked up (deepStore), one more for each remembered
// (fillDeepStore).
func TestDeepAddsFourWays(t *testing.T) {
	was := deepSetOf
	calls := 0
	deepSetOf = func(*shape, Atom) uintptr { calls++; return 0 }
	t.Cleanup(func() { deepSetOf = was })
	r := New(Config{})
	defer r.Close()
	run := func(src string) {
		t.Helper()
		if _, err := r.Run(compileForTest(t, src)); err != nil {
			t.Fatal(err)
		}
	}
	run(waysCtors)
	for _, c := range []struct {
		picks []int
		fills bool
	}{{[]int{0, 1, 2, 3}, false}, {[]int{4, 5, 6, 7, 8}, true}} {
		run(`adds(` + jsInts(c.picks) + `, 2)`)
		calls = 0
		run(`adds(` + jsInts(c.picks) + `, 100)`)
		n := 100 * len(c.picks)
		switch {
		case !c.fills && calls != n:
			t.Errorf("%v: %d lookups and fills for %d additions, want %d: some looked up again", c.picks, calls, n, n)
		case c.fills && calls <= n:
			t.Errorf("%v: %d lookups and fills for %d additions in one set of four ways, want more", c.picks, calls, n)
		}
	}
}

// The same additions, in sets as the shapes' addresses put them, in
// runtimes whose heaps differ by what was allocated before each: 24
// classes' three additions each, as Class.create's initializers make
// them, are each found after the first round in every runtime. Direct-
// mapped, from 1 in 35 to 3 in 4 of them had been made the long way, by
// where the layouts were.
func TestDeepAddsAnyHeap(t *testing.T) {
	was := deepSetOf
	calls := 0
	deepSetOf = func(s *shape, key Atom) uintptr { calls++; return was(s, key) }
	t.Cleanup(func() { deepSetOf = was })
	fn := compileForTest(t, `
var Class = { create: function () { return function () { this.initialize.apply(this, arguments) } } };
var classes = [];
for (var k = 0; k < 24; k++) {
  var C = Class.create();
  C.prototype = { initialize: function (x, y, z) { this.x = x; this.y = y; this.z = z } };
  classes.push(C);
}
function run(n) { var s = 0; for (var i = 0; i < n; i++) for (var j = 0; j < 24; j++) { var o = new classes[j](i, 1, 2); s += o.y } return s }
run(50);`)
	var keep [][]byte
	for seed := range 20 {
		keep = append(keep, make([]byte, 112*(seed*37%61+1)))
		r := New(Config{})
		if _, err := r.Run(fn); err != nil {
			t.Fatal(err)
		}
		calls = 0
		if _, err := r.Run(compileForTest(t, `run(100)`)); err != nil {
			t.Fatal(err)
		}
		if calls > 3*24*100 {
			t.Errorf("seed %d: %d lookups and fills for %d additions", seed, calls, 3*24*100)
		}
		r.Close()
	}
	_ = keep
}

// A read site that has given up keeps in its own table (megaOwn) where
// its property is in objects of each layout, four to a set: four layouts
// of one set, read in turn, keep their entries however many times they
// are read, which the set's order, changed by each entry made, shows; a
// fifth displaces the oldest.
// Each with the Go table, whose reads ask it, and the native handlers.
func TestMegaOwnFourWays(t *testing.T) {
	for _, native := range []bool{false, true} {
		t.Run(fmt.Sprint("native=", native), func(t *testing.T) { megaOwnFourWays(t, native) })
	}
}

func megaOwnFourWays(t *testing.T, native bool) {
	was := regNativeOn
	regNativeOn = native
	defer func() { regNativeOn = was }()
	r := New(Config{})
	defer r.Close()
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
	r := New(Config{})
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
