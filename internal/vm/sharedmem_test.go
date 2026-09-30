package vm

import (
	"math"
	"testing"
	"time"
)

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

// TestNotifyDeliversUnlocked pins that notify tells an asynchronous waiter
// once it has let go of the memory's lock: telling it posts to a host, and a
// waiter whose delivery took the lock -- a host that waits on the memory, a
// runtime closing, which cancels its waiters -- waited on it forever (KI-60).
func TestNotifyDeliversUnlocked(t *testing.T) {
	m := newSharedMemory(8, 8)
	delivered := make(chan string, 1)
	w, now := m.waitAsync(0, 4, 0, math.Inf(1), func(outcome string) {
		m.mu.Lock()
		delivered <- outcome
		m.mu.Unlock()
	})
	if w == nil {
		t.Fatalf("waitAsync answered %q at once", now)
	}
	woke := make(chan int, 1)
	go func() { woke <- m.notify(0, 1) }()
	select {
	case n := <-woke:
		if n != 1 {
			t.Errorf("notify woke %d, want 1", n)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("notify held the memory's lock while it told the waiter")
	}
	if got := <-delivered; got != "ok" {
		t.Errorf("the waiter was told %q, want ok", got)
	}
}

// TestHostJobsReadyAfterAttach pins that HostJobsReady signals only while
// jobs wait for the runtime to run them: a loop attached is told to run them
// but the runtime keeps them until it has, and running them leaves no signal
// behind (KI-60).
func TestHostJobsReadyAfterAttach(t *testing.T) {
	signalled := func(r *Runtime) bool {
		select {
		case <-r.HostJobsReady():
			return true
		default:
			return false
		}
	}
	r := New(Config{})
	ran := 0
	r.PostFromElsewhere(func() { ran++ })
	var posted []func()
	r.AttachHostLoop(func(fn func()) { posted = append(posted, fn) })
	if len(posted) != 1 {
		t.Fatalf("attaching told the loop %d times, want 1", len(posted))
	}
	if !signalled(r) {
		t.Error("HostJobsReady does not signal the job still waiting")
	}
	posted[0]()
	if ran != 1 || signalled(r) {
		t.Errorf("the loop ran the queue: %d jobs ran, want 1, and the signal is %v", ran, signalled(r))
	}
	r.PostFromElsewhere(func() { ran++ })
	if len(posted) != 2 {
		t.Fatalf("with a loop, it was told %d times, want 2", len(posted))
	}
	posted[1]()
	if ran != 2 || signalled(r) {
		t.Errorf("with a loop, %d jobs ran, want 2, and the signal is %v", ran, signalled(r))
	}

	r = New(Config{})
	ran = 0
	r.PostFromElsewhere(func() { ran++ })
	_ = r.runHostJobs()
	if ran != 1 {
		t.Errorf("%d jobs ran, want 1", ran)
	}
	if signalled(r) {
		t.Error("HostJobsReady still signals the jobs that ran")
	}
}
