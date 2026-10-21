package vm

import (
	"runtime"
	"runtime/debug"
	"testing"
	"time"
	"weak"
)

// releaseMapsUntil collects and releases r's WeakMaps that have gone until
// no more than live of its maps are left unreleased, as the end of a turn
// would.
func releaseMapsUntil(t *testing.T, r *Runtime, live int) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); len(r.weakMaps.live) > live; {
		if time.Now().After(deadline) {
			t.Fatalf("%d maps are left unreleased, want %d", len(r.weakMaps.live), live)
		}
		r.endTurn()
		runtime.GC()
		time.Sleep(time.Millisecond)
		r.releaseDeadWeakMaps()
	}
}

// Keys of a WeakMap after another -- objects, arrays and symbols, each map
// over its own of them -- answer what Node's answer, before and after most
// of the maps have gone and been released, the room of their keys' entries
// in the other maps kept as spares (weakMapRegistry.spare) that the maps
// made next take; and after deletes, and sets again. Node v26.10.0's
// answers.
func TestWeakMapKeysOfMapsAfterMaps(t *testing.T) {
	r := New(Config{})
	defer r.Close()
	run := func(src string) string {
		t.Helper()
		v, err := r.Run(compileForTest(t, src))
		if err != nil {
			t.Fatal(err)
		}
		return v.String().Go()
	}
	got := run(`
		// The maps' array is made with its room: grown from an array's own
		// room, it would keep its first elements there.
		var keys = [], maps = new Array(60);
		for (var i = 0; i < 300; i++) keys.push(i % 3 == 0 ? Symbol("s" + i) : i % 3 == 1 ? { i: i } : [i]);
		function fill(m, j) {
			for (var i = j % 7; i < keys.length; i += 1 + j % 5) m.set(keys[i], j * 1000 + i);
			return m;
		}
		function census() {
			var n = 0, sum = 0;
			for (var j = 0; j < maps.length; j++)
				if (maps[j])
					for (var i = 0; i < keys.length; i++)
						if (maps[j].has(keys[i])) { n++; sum += maps[j].get(keys[i]); }
			return n + " " + sum;
		}
		for (var j = 0; j < 40; j++) maps[j] = fill(new WeakMap(), j);
		var a = census();
		for (var j = 1; j < 40; j++) maps[j] = null;
		a + " | " + census()`)
	if want := "5440 102518142 | 300 44850"; got != want {
		t.Fatalf("before the collection: %q, want %q", got, want)
	}
	releaseMapsUntil(t, r, 1)
	spares := len(r.weakMaps.spare)
	if spares == 0 {
		t.Fatal("the maps' releases kept no spare entries")
	}
	got = run(`
		for (var j = 40; j < 60; j++) maps[j] = fill(new WeakMap(), j);
		var c = census();
		for (var j = 0; j < maps.length; j += 3)
			if (maps[j]) for (var i = 0; i < keys.length; i += 2) maps[j].delete(keys[i]);
		var d = census();
		for (var j = 0; j < maps.length; j += 6)
			if (maps[j]) for (var i = 0; i < keys.length; i += 4) maps[j].set(keys[i], -i);
		[c, d, census()].join(" | ")`)
	if want := "3014 132628785 | 2364 108204239 | 2664 108159839"; got != want {
		t.Fatalf("after the collection: %q, want %q", got, want)
	}
	if n := len(r.weakMaps.spare); n >= spares {
		t.Errorf("the new maps took none of the %d spares: %d left", spares, n)
	}
}

// A key's entries in its other maps, all released, are dropped from it and
// kept, empty, with their room; a key that needs room for its other maps
// takes them, and its entries are its own, none of the first key's.
func TestWeakMapSpareEntriesTaken(t *testing.T) {
	r := New(Config{})
	a, b := Obj(newObject(nil, ClassObject)), Obj(newObject(nil, ClassObject))
	maps := make([]*weakMap, 12)
	for i := range maps {
		maps[i] = newWeakMap()
		maps[i].set(r, a, Int(i))
	}
	q := r.weakMaps
	more := a.object().weakMapRefsOf().more
	if more == nil || len(more.entries) != 11 {
		t.Fatalf("a's entries in its other maps: %v, want 11", more)
	}
	room := cap(more.entries)
	for _, m := range maps[1:] {
		m.state.releaseKey(a.object().weakMapRefsOf().key, false, q)
	}
	if refs := a.object().weakMapRefsOf(); refs.more != nil || refs.first.m != maps[0].state {
		t.Fatalf("released from its other maps, a keeps %v, first %v", refs.more, refs.first.m)
	}
	if len(q.spare) != 1 || q.spare[0] != more || len(more.entries) != 0 || more.gone != 0 || cap(more.entries) != room {
		t.Fatalf("spares %v: want a's entries, empty, with room for %d", q.spare, room)
	}
	// b's first entry, then the others in the spare.
	others := make([]*weakMap, 3)
	b0 := newWeakMap()
	b0.set(r, b, Int(100))
	for i := range others {
		others[i] = newWeakMap()
		others[i].set(r, b, Int(101+i))
	}
	if got := b.object().weakMapRefsOf().more; got != more || len(q.spare) != 0 {
		t.Fatalf("b's other entries %p, a's spare %p, %d spares left", got, more, len(q.spare))
	}
	if v, ok := b0.get(r, b); !ok || v.Number() != 100 {
		t.Errorf("b's first map: %v %v, want 100", v, ok)
	}
	for i, m := range others {
		if v, ok := m.get(r, b); !ok || v.Number() != float64(101+i) {
			t.Errorf("b's map %d: %v %v, want %d", i, v, ok, 101+i)
		}
	}
	for i, m := range maps {
		if _, ok := m.get(r, b); ok {
			t.Errorf("b has an entry in a's map %d", i)
		}
		v, ok := m.get(r, a)
		if i == 0 && (!ok || v.Number() != 0) || i > 0 && ok {
			t.Errorf("a's map %d: %v %v", i, v, ok)
		}
	}
	if n := len(more.entries); n != 3 || more.gone != 0 {
		t.Errorf("b's other entries: %d, %d gone; want 3, 0", n, more.gone)
	}
	runtime.KeepAlive(maps)
}

// Spares no key took go once a collection has run since they were kept.
func TestWeakMapSparesGoAfterCollection(t *testing.T) {
	// No collection of its own between the spare kept and the first drop,
	// which a cycle the runtime started had made (0 spares, under
	// quickjs_verify): one finished first, and none started until the
	// test's.
	runtime.GC()
	defer debug.SetGCPercent(debug.SetGCPercent(-1))
	r := New(Config{})
	q := r.weakMapRegistry()
	e := gcEpoch.Load()
	q.dropSpares()
	q.keepSpare(&weakMapMore{entries: make([]weakMapEntry, 4, 8), gone: 4})
	q.dropSpares()
	if len(q.spare) != 1 {
		t.Fatalf("%d spares before a collection, want 1", len(q.spare))
	}
	for deadline := time.Now().Add(5 * time.Second); gcEpoch.Load() == e; time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the collector's cycles are not counted")
		}
		runtime.GC()
	}
	q.dropSpares()
	if len(q.spare) != 0 {
		t.Errorf("%d spares after a collection, want 0", len(q.spare))
	}
}

// The entries of a key's other maps keep their room when compacted, unless
// their maps since the last compaction were fewer than a quarter of it.
func TestWeakMapCompactKeepsRoom(t *testing.T) {
	r := New(Config{})
	key := Obj(newObject(nil, ClassObject))
	refs := func() *weakMapRefs { return key.object().weakMapRefsOf() }
	first := newWeakMap()
	first.set(r, key, Int(-1))
	maps := make([]*weakMap, 16)
	for i := range maps {
		maps[i] = newWeakMap()
		maps[i].set(r, key, Int(i))
	}
	room := cap(refs().more.entries)
	// All but the last released: the next map's entry compacts them, and
	// keeps the room.
	for _, m := range maps[:15] {
		m.state.releaseKey(refs().key, false, nil)
	}
	next := newWeakMap()
	next.set(r, key, Int(16))
	if more := refs().more; len(more.entries) != 2 || more.gone != 0 || cap(more.entries) != room {
		t.Fatalf("compacted: %d entries, %d gone, room %d; want 2, 0, %d", len(more.entries), more.gone, cap(more.entries), room)
	}
	// One of the two released: two entries since the compaction, fewer
	// than a quarter of the room, which goes.
	maps[15].state.releaseKey(refs().key, false, nil)
	last := newWeakMap()
	last.set(r, key, Int(17))
	if more := refs().more; len(more.entries) != 2 || cap(more.entries) >= room {
		t.Fatalf("compacted again: %d entries, room %d; want 2 and less than %d", len(more.entries), cap(more.entries), room)
	}
	for _, c := range []struct {
		m    *weakMap
		want float64
	}{{first, -1}, {next, 16}, {last, 17}} {
		if v, ok := c.m.get(r, key); !ok || v.Number() != c.want {
			t.Errorf("get = %v %v, want %v", v, ok, c.want)
		}
	}
	for _, m := range maps {
		if _, ok := m.get(r, key); ok {
			t.Error("a released map's entry is there")
		}
	}
}

// WeakMaps and WeakSets of objects, arrays and symbols, keys of many maps
// each: has, get, deletes and sets again, a registered symbol refused as a
// key and found in none, members of a set deleted and added again. Node
// v26.10.0's answers.
func TestWeakCollectionsAgreeWithNode(t *testing.T) {
	r := New(Config{})
	defer r.Close()
	v, err := r.Run(compileForTest(t, `
		var out = [];
		var keys = [];
		for (var i = 0; i < 300; i++) keys.push(i % 3 == 0 ? Symbol("s" + i) : i % 3 == 1 ? { i: i } : [i]);
		var maps = [];
		for (var j = 0; j < 40; j++) {
		  var m = new WeakMap();
		  for (var i = j % 7; i < keys.length; i += 1 + j % 5) m.set(keys[i], j * 1000 + i);
		  maps.push(m);
		}
		var sum = 0, n = 0;
		for (var j = 0; j < maps.length; j++)
		  for (var i = 0; i < keys.length; i++)
		    if (maps[j].has(keys[i])) { n++; sum += maps[j].get(keys[i]); }
		out.push(n, sum);
		for (var j = 0; j < maps.length; j += 2)
		  for (var i = 0; i < keys.length; i += 3) maps[j].delete(keys[i]);
		n = 0; sum = 0;
		for (var j = 0; j < maps.length; j++)
		  for (var i = 0; i < keys.length; i++)
		    if (maps[j].has(keys[i])) { n++; sum += maps[j].get(keys[i]); }
		out.push(n, sum);
		for (var j = 0; j < maps.length; j += 4)
		  for (var i = 0; i < keys.length; i += 6) maps[j].set(keys[i], -i);
		n = 0; sum = 0;
		for (var j = 0; j < maps.length; j++)
		  for (var i = 0; i < keys.length; i++)
		    if (maps[j].has(keys[i])) { n++; sum += maps[j].get(keys[i]); }
		out.push(n, sum);
		try { new WeakMap().set(Symbol.for("reg"), 1); out.push("no error"); } catch (e) { out.push(e.constructor.name); }
		try { new WeakSet().add(Symbol.for("reg")); out.push("no error"); } catch (e) { out.push(e.constructor.name); }
		out.push(new WeakMap().has(Symbol.for("reg")), new WeakSet().has(Symbol.for("reg")), new WeakMap().delete(Symbol.for("reg")));
		var ws = new WeakSet();
		for (var i = 0; i < keys.length; i += 2) ws.add(keys[i]);
		for (var i = 0; i < keys.length; i += 4) ws.delete(keys[i]);
		for (var i = 0; i < keys.length; i += 8) ws.add(keys[i]);
		n = 0;
		for (var i = 0; i < keys.length; i++) if (ws.has(keys[i])) n += i;
		out.push(n, ws.has({}), ws.delete({}), ws.has(1));
		out.join(" ");`))
	if err != nil {
		t.Fatal(err)
	}
	want := "5440 102518142 4668 88528588 5168 88455088 TypeError TypeError false false false 16874 false false false"
	if got := v.String().Go(); got != want {
		t.Errorf("got  %q, want %q", got, want)
	}
}

// A map that has gone takes its values off its symbol keys that live on as
// it does off its object keys: the two are listed apart (weakMapState's
// keys and syms), and each list's keys are found as what they are.
func TestWeakMapReleasesValuesOfSymbolKeys(t *testing.T) {
	r := New(Config{})
	sym, obj := Sym(NewSymbol("k", true)), Obj(newObject(nil, ClassObject))
	live := newWeakMap()
	live.set(r, sym, Int(1))
	live.set(r, obj, Int(2))
	refs := make([]weak.Pointer[Object], 16)
	func() {
		for i := range refs {
			m := newWeakMap()
			value := newObject(nil, ClassObject)
			refs[i] = weak.Make(value)
			if i%2 == 0 {
				m.set(r, sym, Obj(value))
				m.set(r, obj, Int(i))
			} else {
				m.set(r, obj, Int(i))
				m.set(r, sym, Obj(value))
			}
		}
	}()
	for i, ref := range refs {
		if !collected(r, ref) {
			t.Fatalf("value %d of a symbol key survived its WeakMap", i)
		}
	}
	for _, k := range []Value{sym, obj} {
		refs := weakRefsOf(k, false)
		if refs.more != nil || refs.first.m != live.state {
			t.Errorf("%v keeps entries of maps that have gone", k)
		}
	}
	if v, ok := live.get(r, sym); !ok || v.Number() != 1 {
		t.Errorf("the live map's symbol entry = %v %v, want 1", v, ok)
	}
	if v, ok := live.get(r, obj); !ok || v.Number() != 2 {
		t.Errorf("the live map's object entry = %v %v, want 2", v, ok)
	}
	runtime.KeepAlive(live)
}
