package vm

import (
	"runtime"
	"strings"
	"testing"
	"time"
	"unsafe"
	"weak"
)

// collected reports whether p's target is collected within a few cycles,
// releasing r's WeakMaps that have gone after each, as the interpreter would.
func collected[T any](r *Runtime, p weak.Pointer[T]) bool {
	for range 20 {
		runtime.GC()
		runtime.GC()
		// The cleanups run on a goroutine of their own; give them the time.
		time.Sleep(time.Millisecond)
		r.releaseDeadWeakMaps()
		if p.Value() == nil {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return false
}

// tracked is how many keys a map holds a weak reference to, live or
// collected.
func tracked(s *weakMapState) int { return len(s.keys) }

// A key nothing else holds goes, and a key that is held keeps its value.
func TestWeakMapReleasesCollectedKeys(t *testing.T) {
	r := New(Config{})
	m := newWeakMap()
	var gone weak.Pointer[Object]
	func() {
		key := newObject(nil, ClassObject)
		gone = weak.Make(key)
		m.set(r, Obj(key), Int(1))
	}()
	kept := Obj(newObject(nil, ClassObject))
	m.set(r, kept, Int(-1))
	if !collected(r, gone) {
		t.Error("unreachable key was kept alive")
	}
	if v, ok := m.get(r, kept); !ok || v.Number() != -1 {
		t.Errorf("the held key was dropped")
	}
	runtime.KeepAlive(kept)
	runtime.KeepAlive(m)
}

// The ephemeron: a value that refers to its own key does not keep the key
// alive, for an object key and for a symbol key, and the map lets go of the
// collected key's record when it next prunes.
func TestWeakMapValueRefersToKey(t *testing.T) {
	r := New(Config{})
	m := newWeakMap()
	var objRef weak.Pointer[Object]
	var symRef weak.Pointer[Symbol]
	func() {
		key := newObject(nil, ClassObject)
		value := newObject(nil, ClassObject)
		value.data = key
		objRef = weak.Make(key)
		m.set(r, Obj(key), Obj(value))

		sym := NewSymbol("key", true)
		value = newObject(nil, ClassObject)
		value.props = []Property{{value: Sym(sym)}}
		symRef = weak.Make(sym)
		m.set(r, Sym(sym), Obj(value))
	}()
	if !collected(r, objRef) {
		t.Error("a WeakMap value referring to its key kept the key alive")
	}
	if !collected(r, symRef) {
		t.Error("a WeakMap value referring to its symbol key kept the key alive")
	}
	// Pruning runs as keys are added; enough of them to reach it.
	var held []Value
	for i := range 16 {
		k := Obj(newObject(nil, ClassObject))
		held = append(held, k)
		m.set(r, k, Int(i))
	}
	if n := tracked(m.state); n != len(held) {
		t.Errorf("the map tracks %d keys, want %d: the collected keys were kept", n, len(held))
	}
	runtime.KeepAlive(held)
	runtime.KeepAlive(m)
}

// A map that has gone takes its value off a key that lives on, and leaves
// another map's value for the same key where it is.
func TestWeakMapReleasesValuesWhenMapDies(t *testing.T) {
	r := New(Config{})
	key := Obj(newObject(nil, ClassObject))
	live := newWeakMap()
	live.set(r, key, Int(7))
	refs := make([]weak.Pointer[Object], 64)
	func() {
		for i := range refs {
			m := newWeakMap()
			value := newObject(nil, ClassObject)
			refs[i] = weak.Make(value)
			m.set(r, key, Obj(value))
		}
	}()
	for i, ref := range refs {
		if !collected(r, ref) {
			t.Fatalf("value %d survived its WeakMap", i)
		}
	}
	if v, ok := live.get(r, key); !ok || v.Number() != 7 {
		t.Errorf("the live WeakMap's entry = %v, %v; want 7, true", v, ok)
	}
	if kr := key.object().weakMapRefs; len(kr.more) != 0 {
		t.Errorf("the key holds %d entries of maps that have gone", len(kr.more))
	}
	runtime.KeepAlive(key)
	runtime.KeepAlive(live)
}

// The gap docs/status.md documents: a value that refers to its own map keeps
// the map alive while the key lives, since the key holds the value. An
// ephemeron would let both go. If this starts failing, the gap has closed:
// update the page.
func TestWeakMapValueRefersToMapKeepsIt(t *testing.T) {
	r := New(Config{})
	key := Obj(newObject(nil, ClassObject))
	var mapRef weak.Pointer[weakMap]
	func() {
		m := newWeakMap()
		value := newObject(nil, ClassObject)
		value.data = m
		mapRef = weak.Make(m)
		m.set(r, key, Obj(value))
	}()
	if collected(r, mapRef) {
		t.Error("the map was collected: the gap docs/status.md describes is closed")
	}
	runtime.KeepAlive(key)
}

// The values of a map that has gone are released at the end of a turn too,
// for a runtime that runs no loop long enough to sweep.
func TestWeakMapReleasedAtEndOfTurn(t *testing.T) {
	r := New(Config{})
	key := Obj(newObject(nil, ClassObject))
	var valueRef weak.Pointer[Object]
	func() {
		m := newWeakMap()
		value := newObject(nil, ClassObject)
		valueRef = weak.Make(value)
		m.set(r, key, Obj(value))
	}()
	for i := 0; i < 20 && valueRef.Value() != nil; i++ {
		runtime.GC()
		time.Sleep(5 * time.Millisecond)
		r.endTurn()
	}
	if valueRef.Value() != nil {
		t.Error("endTurn did not release the value of a WeakMap that had gone")
	}
	runtime.KeepAlive(key)
}

// Closing a runtime releases its WeakMaps' values, a live map's and a dead
// one's, though the host still holds their key: the runtime runs nothing
// that could miss them, and has no turns left to release them later.
func TestWeakMapReleasedWhenClosed(t *testing.T) {
	r := New(Config{})
	key := Obj(newObject(nil, ClassObject))
	live := newWeakMap()
	var liveValue, deadValue weak.Pointer[Object]
	func() {
		v := newObject(nil, ClassObject)
		liveValue = weak.Make(v)
		live.set(r, key, Obj(v))
		dead := newWeakMap()
		v = newObject(nil, ClassObject)
		deadValue = weak.Make(v)
		dead.set(r, key, Obj(v))
	}()
	r.Close()
	r.ReleaseClosed()
	if !collected(r, liveValue) {
		t.Error("a live WeakMap's value outlived the closed runtime")
	}
	if !collected(r, deadValue) {
		t.Error("a dead WeakMap's value outlived the closed runtime")
	}
	if refs := key.object().weakMapRefs; refs.first.m != nil || len(refs.more) != 0 {
		t.Error("the key still holds entries")
	}
	runtime.KeepAlive(key)
	runtime.KeepAlive(live)
}

// A script that closes its own runtime runs on to its next interrupt check,
// and finds its WeakMaps as they were until then: ReleaseClosed waits for it
// to stop.
func TestWeakMapKeptWhileClosingScriptRuns(t *testing.T) {
	r := New(Config{})
	r.global.setOwnRaw(r.atoms.intern("closeNow"), Obj(r.newNativeFunc("closeNow", 0,
		func(rt *Runtime, this Value, args []Value) (Value, error) {
			rt.Close()
			rt.ReleaseClosed()
			return Undefined, nil
		})), propDefault)
	v, err := r.Run(compileForTest(t, `
		var wm = new WeakMap(), k = {};
		wm.set(k, 42);
		function f() { closeNow(); return wm.get(k) }
		f()`))
	if err != nil || v.Number() != 42 {
		t.Fatalf("wm.get after closing = %v, %v; want 42", v, err)
	}
	r.ReleaseClosed()
	wm := r.global.getOwn(r.atoms.intern("wm")).value.object().data.(*weakMap)
	k := r.global.getOwn(r.atoms.intern("k")).value
	if _, ok := wm.get(r, k); ok {
		t.Error("the entry survived ReleaseClosed once the script had stopped")
	}
}

// delete lets go of the value at once. The key stays on the map's list, its
// entry marked deleted, so that setting it again does not list it twice.
func TestWeakMapDeleteReleasesValue(t *testing.T) {
	r := New(Config{})
	m := newWeakMap()
	key := Obj(newObject(nil, ClassObject))
	var valueRef weak.Pointer[Object]
	func() {
		value := newObject(nil, ClassObject)
		valueRef = weak.Make(value)
		m.set(r, key, Obj(value))
	}()
	if !m.delete(key) {
		t.Fatal("delete reported no entry")
	}
	if m.delete(key) {
		t.Fatal("a second delete found an entry")
	}
	if _, ok := m.get(r, key); ok {
		t.Fatal("get found the deleted entry")
	}
	if !collected(r, valueRef) {
		t.Error("deleted WeakMap value stayed alive")
	}
	for i := range 3 {
		m.set(r, key, Int(i))
		m.delete(key)
	}
	m.set(r, key, Int(9))
	if v, ok := m.get(r, key); !ok || v.Number() != 9 {
		t.Errorf("get after set again = %v %v, want 9", v, ok)
	}
	if n := tracked(m.state); n != 1 {
		t.Errorf("the map lists the key %d times, want 1", n)
	}
	runtime.KeepAlive(key)
	runtime.KeepAlive(m)
}

// A key of several maps keeps one entry for each, which each map reads,
// replaces and deletes on its own.
func TestWeakMapKeyOfSeveralMaps(t *testing.T) {
	r := New(Config{})
	key := Obj(newObject(nil, ClassObject))
	maps := make([]*weakMap, 5)
	for i := range maps {
		maps[i] = newWeakMap()
		maps[i].set(r, key, Int(i))
	}
	maps[2].set(r, key, Int(20))
	if !maps[0].delete(key) {
		t.Fatal("delete from the first map found nothing")
	}
	maps[0].set(r, key, Int(100))
	want := []float64{100, 1, 20, 3, 4}
	for i, m := range maps {
		if v, ok := m.get(r, key); !ok || v.Number() != want[i] {
			t.Errorf("map %d: get = %v %v, want %v", i, v, ok, want[i])
		}
	}
	if _, ok := newWeakMap().get(r, key); ok {
		t.Error("a map the key was never set in found an entry")
	}
	for _, v := range []Value{Undefined, Int(1), Str(NewString("k"))} {
		if _, ok := maps[1].get(r, v); ok {
			t.Errorf("get(%v) found an entry", v)
		}
		if maps[1].delete(v) {
			t.Errorf("delete(%v) found an entry", v)
		}
	}
}

// The memory meter counts a WeakMap's values, which are on its keys: a
// script must not be able to hide memory from a limit in one.
func TestWeakMapValuesAreMetered(t *testing.T) {
	r := New(Config{})
	big := Str(NewString(strings.Repeat("x", 1<<20)))
	obj := newObject(nil, ClassObject)
	sym := NewSymbol("k", true)
	m := newWeakMap()
	r.global.setOwnRaw(r.atoms.intern("o"), Obj(obj), propWritable)
	r.global.setOwnRaw(r.atoms.intern("s"), Sym(sym), propWritable)
	meter := &memoryMeter{seen: map[unsafe.Pointer]struct{}{}}
	before := meter.walk(r)
	m.set(r, Obj(obj), big)
	if got := meter.walk(r) - before; got < 1<<20 {
		t.Errorf("an object key's value added %d bytes, want at least %d", got, 1<<20)
	}
	m.delete(Obj(obj))
	m.set(r, Sym(sym), big)
	if got := meter.walk(r) - before; got < 1<<20 {
		t.Errorf("a symbol key's value added %d bytes, want at least %d", got, 1<<20)
	}
	runtime.KeepAlive(m)
}

// WeakSet membership must not retain its member, and the rebuild that adding
// members brings on drops the collected ones while keeping those still held.
func TestWeakSetReleasesCollectedValues(t *testing.T) {
	s := newWeakSet()

	func() {
		for range 200 {
			s.add(Obj(newObject(nil, ClassObject)))
		}
	}()
	kept := Obj(newObject(nil, ClassObject))
	s.add(kept)

	for range 4 {
		runtime.GC()
	}
	// Insertion triggers a sweep. Hold the new members so that only the first
	// 200 are eligible to disappear.
	var held []Value
	for range 400 {
		value := Obj(newObject(nil, ClassObject))
		held = append(held, value)
		s.add(value)
	}
	if s.size > 401 {
		t.Errorf("size after = %d, want at most 401: collected values were retained", s.size)
	}
	if !s.has(kept) {
		t.Error("reachable WeakSet value was dropped")
	}
	runtime.KeepAlive(held)
	runtime.KeepAlive(kept)
}

// Two references to one object are one key: the second set replaces the
// first's value, and delete removes the one entry.
func TestWeakMapKeyIdentity(t *testing.T) {
	r := New(Config{})
	m := newWeakMap()
	k := Obj(newObject(nil, ClassObject))

	m.set(r, k, Int(1))
	m.set(r, k, Int(2))
	if v, ok := m.get(r, k); !ok || v.Number() != 2 {
		t.Errorf("get = %v %v, want 2", v, ok)
	}
	if !m.delete(k) {
		t.Error("delete reported nothing to remove")
	}
	if _, ok := m.get(r, k); ok {
		t.Error("the entry survived delete")
	}
}

// An address outlives its object and comes back as another's. A WeakMap finds
// nothing by address, its entries being on their keys, so a new object at a
// collected key's address has none of its entries -- whether asked, set, or
// deleted -- and its own are not lost to them. The map's record of the
// collected keys is pruned meanwhile.
func TestWeakMapAddressReuse(t *testing.T) {
	r := New(Config{})
	m := newWeakMap()
	// The first keys' addresses, as numbers, which keep nothing alive.
	stale := map[uintptr]bool{}
	func() {
		for i := range 2000 {
			k := Obj(newObject(nil, ClassObject))
			stale[uintptr(k.ref)] = true
			m.set(r, k, Int(i))
		}
	}()
	for range 4 {
		runtime.GC()
	}

	reused := 0
	var held []Value
	for i := range 4000 {
		k := Obj(newObject(nil, ClassObject))
		held = append(held, k)
		if stale[uintptr(k.ref)] {
			reused++
		}
		if v, ok := m.get(r, k); ok {
			t.Fatalf("a new object found the collected key's value %v", v)
		}
		if m.delete(k) {
			t.Fatal("a new object deleted the collected key's entry")
		}
		m.set(r, k, Int(-i))
	}
	for i, k := range held {
		if v, ok := m.get(r, k); !ok || v.Number() != float64(-i) {
			t.Fatalf("key %d: get = %v %v, want %d", i, v, ok, -i)
		}
	}
	for _, k := range held[:100] {
		if !m.delete(k) {
			t.Fatal("delete reported nothing to remove")
		}
		if _, ok := m.get(r, k); ok {
			t.Fatal("the entry survived delete")
		}
	}
	// 2,000 collected and 3,900 held: pruning has dropped some of the
	// collected, though it need not have dropped them all.
	if n := tracked(m.state); n >= 5900 {
		t.Errorf("the map tracks %d keys: the collected ones were never pruned", n)
	}
	if reused == 0 {
		t.Log("no address was reused, so this run did not test reuse")
	}
	runtime.KeepAlive(held)
}

// A WeakSet does find its members by address. A collected member's entry
// names nothing, so a new object at its address is not a member -- whether
// asked or deleted -- and becomes one when added.
func TestWeakSetAddressReuse(t *testing.T) {
	s := newWeakSet()
	stale := map[uintptr]bool{}
	func() {
		for range 2000 {
			v := Obj(newObject(nil, ClassObject))
			stale[uintptr(v.ref)] = true
			s.add(v)
		}
	}()
	for range 4 {
		runtime.GC()
	}
	reused := 0
	var held []Value
	for range 4000 {
		v := Obj(newObject(nil, ClassObject))
		held = append(held, v)
		if stale[uintptr(v.ref)] {
			reused++
		}
		if s.has(v) {
			t.Fatal("a new object is a member in a collected one's place")
		}
		if s.delete(v) {
			t.Fatal("a new object deleted a collected member's entry")
		}
		s.add(v)
	}
	for _, v := range held {
		if !s.has(v) {
			t.Fatal("a member was lost")
		}
	}
	if reused == 0 {
		t.Log("no address was reused, so this run did not test reuse")
	}
	runtime.KeepAlive(held)
}
