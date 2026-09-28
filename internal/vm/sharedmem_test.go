package vm

import "testing"

// TestSharedMemoryGrowRefusesShrink pins that grow refuses, under the
// memory's lock, a length shorter than the memory has: a grow that lost a
// race to a larger one checked the length before taking the lock, and
// succeeded doing nothing (KI-38).
func TestSharedMemoryGrowRefusesShrink(t *testing.T) {
	m := newSharedMemory(8, 64)
	if err := m.pin(); err != nil {
		t.Fatal(err)
	}
	if !m.grow(32) {
		t.Fatal("grow(32) refused")
	}
	if m.grow(16) {
		t.Error("grow(16) after grow(32) succeeded")
	}
	if !m.grow(32) || !m.grow(40) {
		t.Error("growing to the same length, or more, was refused")
	}
	if n := len(m.bytes()); n != 40 {
		t.Errorf("length %d, want 40", n)
	}
}
