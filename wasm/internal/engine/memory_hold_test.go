package engine

import (
	"runtime"
	"testing"
)

// TestMemoryHold checks a host's hold on a reserved memory: while one is
// held the collector does not release the bytes, so that a view of them
// survives the Memory's collection, and once the last is given back the
// release is again the collector's.
func TestMemoryHold(t *testing.T) {
	m := NewMemory(1, 4, true, false, false)
	if m == nil {
		t.Fatal("no memory")
	}
	if m.reservation == nil {
		t.Skip("memories are not reserved here")
	}
	if m.cleanup == (runtime.Cleanup{}) {
		t.Fatal("a reserved memory has no cleanup")
	}
	m.Hold()
	m.Hold()
	if m.cleanup != (runtime.Cleanup{}) {
		t.Error("a held memory's bytes are still the collector's to release")
	}
	m.Drop()
	if m.cleanup != (runtime.Cleanup{}) {
		t.Error("a memory still held once is the collector's to release")
	}
	m.Drop()
	if m.cleanup == (runtime.Cleanup{}) {
		t.Error("a memory whose last hold was given back is never released")
	}

	// A view of a held memory's bytes outlives the Memory.
	h := NewMemory(1, 4, true, false, false)
	h.Hold()
	b := h.Bytes()
	collected := make(chan struct{})
	runtime.AddCleanup(h, func(c chan struct{}) { close(c) }, collected)
	h = nil
	for i := 0; i < 50; i++ {
		runtime.GC()
		select {
		case <-collected:
			i = 50
		default:
		}
	}
	b[0], b[len(b)-1] = 1, 2
	if b[0] != 1 || b[len(b)-1] != 2 {
		t.Error("a held memory's bytes changed under a view")
	}
}
