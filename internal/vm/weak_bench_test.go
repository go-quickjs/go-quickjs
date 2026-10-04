package vm

import "testing"

// The weak collections' costs, against a Map's and against making the keys
// themselves. A WeakMap's values live on its keys (weakmap.go), so what these
// measure is that bookkeeping: an existing key's value is a field of the key,
// a new key costs its weakMapRefs and a weak pointer to it, and a new map its
// cleanup.

// Sinks, so that what a benchmark makes is used.
var (
	weakBenchValue  Value
	weakBenchObject *Object
	weakBenchSymbol *Symbol
)

// BenchmarkWeakMapHot sets and gets keys that already have entries: the
// steady state of a cache keyed by objects.
func BenchmarkWeakMapHot(b *testing.B) {
	r := New(Config{})
	m := newWeakMap()
	keys := make([]Value, 256)
	for i := range keys {
		keys[i] = Obj(newObject(nil, ClassObject))
		m.set(r, keys[i], Int(i))
	}
	b.ReportAllocs()
	i := 0
	for b.Loop() {
		key := keys[i&255]
		m.set(r, key, Int(i))
		weakBenchValue, _ = m.get(r, key)
		i++
	}
}

// BenchmarkWeakMapInsert gives one map a new key each time, the key made
// too: what tagging fresh objects costs.
func BenchmarkWeakMapInsert(b *testing.B) {
	r := New(Config{})
	m := newWeakMap()
	b.ReportAllocs()
	i := 0
	for b.Loop() {
		m.set(r, Obj(newObject(nil, ClassObject)), Int(i))
		i++
	}
}

// BenchmarkWeakMapFirstSet makes a map and gives it its first key, each
// time: what a map that holds one entry costs, its cleanup included. The
// maps that have gone are released as the interpreter would, once per
// collection, so that their queue does not grow with b.N.
func BenchmarkWeakMapFirstSet(b *testing.B) {
	r := New(Config{})
	b.ReportAllocs()
	i := 0
	for b.Loop() {
		m := newWeakMap()
		m.set(r, Obj(newObject(nil, ClassObject)), Int(i))
		r.sweepStaleSlots(0)
		i++
	}
}

// BenchmarkWeakSetHot adds and asks about members already in the set.
func BenchmarkWeakSetHot(b *testing.B) {
	s := newWeakSet()
	keys := make([]Value, 256)
	for i := range keys {
		keys[i] = Obj(newObject(nil, ClassObject))
		s.add(keys[i])
	}
	b.ReportAllocs()
	i := 0
	for b.Loop() {
		key := keys[i&255]
		s.add(key)
		if !s.has(key) {
			b.Fatal("lost member")
		}
		i++
	}
}

// BenchmarkWeakSetInsert adds a new member each time, the member made too.
func BenchmarkWeakSetInsert(b *testing.B) {
	s := newWeakSet()
	b.ReportAllocs()
	for b.Loop() {
		s.add(Obj(newObject(nil, ClassObject)))
	}
}

// BenchmarkMapHot is BenchmarkWeakMapHot for a Map, to compare with.
func BenchmarkMapHot(b *testing.B) {
	r := New(Config{})
	m := newJSMap()
	keys := make([]Value, 256)
	for i := range keys {
		keys[i] = Obj(newObject(nil, ClassObject))
		m.set(r, keys[i], Int(i))
	}
	b.ReportAllocs()
	i := 0
	for b.Loop() {
		key := keys[i&255]
		m.set(r, key, Int(i))
		weakBenchValue, _ = m.get(r, key)
		i++
	}
}

// BenchmarkNewObject is what making a key costs, which the insert benchmarks
// include: subtract it to see their own.
func BenchmarkNewObject(b *testing.B) {
	b.ReportAllocs()
	for b.Loop() {
		weakBenchObject = newObject(nil, ClassObject)
	}
}

// BenchmarkNewSymbol is what making a symbol costs, whose struct the
// weakMapRefs field grew from 24 bytes to 32.
func BenchmarkNewSymbol(b *testing.B) {
	b.ReportAllocs()
	for b.Loop() {
		weakBenchSymbol = NewSymbol("key", true)
	}
}
