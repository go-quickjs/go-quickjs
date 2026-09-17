package vm

import (
	"runtime"
	"testing"
)

// A weak collection must actually let go: the point of a WeakMap is that an
// entry stops existing once nothing else refers to its key, and the only way to
// see that from outside is that the storage shrinks.
func TestWeakMapReleasesCollectedKeys(t *testing.T) {
	r := New(Config{})
	m := newJSMap(true)

	// Keys nothing else holds. They are made in a function of their own so
	// that the frame holding the temporaries is gone before the collection.
	func() {
		for i := 0; i < 200; i++ {
			m.set(r, Obj(newObject(nil, ClassObject)), Int(i))
		}
	}()
	// One that is held, which must survive.
	kept := Obj(newObject(nil, ClassObject))
	m.set(r, kept, Int(-1))

	if m.size != 201 {
		t.Fatalf("size before = %d, want 201", m.size)
	}

	for i := 0; i < 4; i++ {
		runtime.GC()
	}
	// The sweep runs on insertion, so give it insertions to run on. These are
	// held, so only the first 200 can go.
	var held []Value
	for i := 0; i < 400; i++ {
		k := Obj(newObject(nil, ClassObject))
		held = append(held, k)
		m.set(r, k, Int(i))
	}

	if m.size > 401 {
		t.Errorf("size after = %d, want at most 401: the collected keys were kept", m.size)
	}
	if v, ok := m.get(r, kept); !ok || v.Number() != -1 {
		t.Errorf("the held key was dropped")
	}
	runtime.KeepAlive(held)
	runtime.KeepAlive(kept)
}

// A weak.Pointer is the identity the index is keyed by, so two references to
// the same object have to find the same entry.
func TestWeakMapKeyIdentity(t *testing.T) {
	r := New(Config{})
	m := newJSMap(true)
	k := Obj(newObject(nil, ClassObject))

	m.set(r, k, Int(1))
	m.set(r, k, Int(2))
	if m.size != 1 {
		t.Errorf("size = %d, want 1: the same key made two entries", m.size)
	}
	if v, ok := m.get(r, k); !ok || v.Number() != 2 {
		t.Errorf("get = %v %v, want 2", v, ok)
	}
	if !m.delete(r, k) {
		t.Error("delete reported nothing to remove")
	}
	if _, ok := m.get(r, k); ok {
		t.Error("the entry survived delete")
	}
}
