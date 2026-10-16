package vm

import (
	"math/rand/v2"
	"runtime"
	"runtime/debug"
	"testing"
	"time"
)

// A key keeps its entries in the order of their maps' ids (weakMapRefs.more)
// and finds one by a search: a key of 64 maps, given their entries in no
// order, some deleted, some of the maps released as a map that has gone is
// -- the entries left marked, the rest compacted out of the way when a new
// one is added -- and some of those set again, as a closed runtime's may be,
// answers for each map what a Go map of the same entries answers.
func TestWeakMapKeyEntriesInOrder(t *testing.T) {
	r := New(Config{})
	key := Obj(newObject(nil, ClassObject))
	refs := func() *weakMapRefs { return key.object().weakMapRefsOf() }
	more := func() *weakMapMore {
		if m := refs().more; m != nil {
			return m
		}
		return &weakMapMore{}
	}
	maps := make([]*weakMap, 64)
	for i := range maps {
		maps[i] = newWeakMap()
		// Each map is given an entry first on a key of its own, so that the
		// maps' ids are in the order of maps.
		maps[i].set(r, Obj(newObject(nil, ClassObject)), Int(-1))
	}
	want := map[int]float64{}
	check := func(when string) {
		t.Helper()
		for i, m := range maps {
			v, ok := m.get(r, key)
			w, has := want[i]
			if ok != has || ok && v.Number() != w {
				t.Fatalf("%s: map %d: get = %v %v, want %v %v", when, i, v, ok, w, has)
			}
		}
		entries, gone := more().entries, 0
		for i, e := range entries {
			if i > 0 && entries[i-1].id >= e.id {
				t.Fatalf("%s: entries %d and %d are out of order: ids %d, %d", when, i-1, i, entries[i-1].id, e.id)
			}
			if e.value.sameBits(weakMapGone) {
				gone++
			}
		}
		// A count that ran ahead would drop the entries left when it
		// reached them all.
		if gone != more().gone {
			t.Fatalf("%s: %d entries are marked, gone says %d", when, gone, more().gone)
		}
	}
	rng := rand.New(rand.NewPCG(1, 2))
	for _, i := range rng.Perm(len(maps)) {
		maps[i].set(r, key, Int(i))
		want[i] = float64(i)
	}
	check("set")
	for i := 0; i < len(maps); i += 3 {
		if !maps[i].delete(key) {
			t.Fatalf("delete from map %d found nothing", i)
		}
		delete(want, i)
	}
	if maps[0].delete(key) {
		t.Fatal("a second delete from map 0 found an entry")
	}
	check("delete")
	// Released, as a map that has gone is; most are, so that the next set
	// compacts the entries.
	for i := 0; i < len(maps); i++ {
		if i%4 != 1 {
			maps[i].state.releaseKey(refs().key)
			delete(want, i)
		}
	}
	if maps[2].delete(key) {
		t.Fatal("delete from a released map found an entry")
	}
	check("release")
	if more().gone == 0 {
		t.Fatal("no entry was left marked")
	}
	// Released maps revive their entries, and a live one sets its own.
	for _, i := range []int{2, 40, 1, 63} {
		maps[i].set(r, key, Int(100+i))
		want[i] = float64(100 + i)
	}
	check("set again")
	// Two maps the key has had no entry in add theirs, the first in the
	// first entry, emptied as its map was released, the second in more,
	// which compacts it.
	if refs().first.m != nil {
		t.Fatal("the first entry's map was not released")
	}
	maps = append(maps, newWeakMap(), newWeakMap())
	for _, i := range []int{64, 65} {
		maps[i].set(r, key, Int(100+i))
		want[i] = float64(100 + i)
	}
	check("new maps")
	if g := more().gone; 2*g >= len(more().entries) && g > 0 {
		t.Errorf("%d of %d entries are marked: the set did not compact them", g, len(more().entries))
	}
	for i := range maps {
		maps[i].state.releaseKey(refs().key)
	}
	if refs().more != nil || refs().first.m != nil {
		t.Errorf("released from every map, the key holds %d entries", len(more().entries))
	}
}

// Only a collection takes a map's keys, so a map given keys while none
// runs does not look through them for any it has taken (weakMapState.pruned):
// it had, at each doubling, found none gone.
func TestWeakMapPrunesOnlyAfterCollection(t *testing.T) {
	defer debug.SetGCPercent(debug.SetGCPercent(-1))
	// A collection that ran before has its count still to come, which a
	// collection now and the wait for its count settle.
	e := gcEpoch.Load()
	runtime.GC()
	for deadline := time.Now().Add(5 * time.Second); gcEpoch.Load() == e; time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the collector's cycles are not counted")
		}
	}
	r := New(Config{})
	m := newWeakMap()
	keys := make([]Value, 100)
	for i := range keys {
		keys[i] = Obj(newObject(nil, ClassObject))
		m.set(r, keys[i], Int(i))
	}
	if m.state.pruneAt != 0 {
		t.Errorf("the map was pruned with no collection run: pruneAt = %d", m.state.pruneAt)
	}
	runtime.KeepAlive(keys)
}
