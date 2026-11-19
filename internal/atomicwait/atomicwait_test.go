package atomicwait

import (
	"runtime"
	"sync"
	"testing"
	"time"
	"unsafe"
)

// waiting returns how many wait at addr of mem.
func waiting(mem any, addr uint64) int {
	t := shardOf(slot{mem, addr})
	t.Lock()
	defer t.Unlock()
	return len(t.m[slot{mem, addr}])
}

// until waits for cond, failing the test after a while.
func until(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("timed out")
		}
		time.Sleep(time.Millisecond)
	}
}

func yes() bool { return true }
func no() bool  { return false }

// TestWaitNotify pins the table's order and counts: waiters, synchronous
// and asynchronous, are woken longest waiting first, a notify returns how
// many it woke, and different memories and addresses are apart.
func TestWaitNotify(t *testing.T) {
	mem, other := new(int), new(int)
	var mu sync.Mutex
	var order []string
	tell := func(name string) func(Result) {
		return func(r Result) { mu.Lock(); order = append(order, name); mu.Unlock() }
	}
	if _, r := WaitAsync(mem, 8, no, -1, tell("never")); r != NotEqual {
		t.Fatalf("not equal: %v", r)
	}
	if _, r := WaitAsync(mem, 8, yes, 0, tell("never")); r != TimedOut {
		t.Fatalf("no time: %v", r)
	}
	if r := Wait(mem, 8, no, -1, nil, nil); r != NotEqual {
		t.Fatalf("not equal: %v", r)
	}
	WaitAsync(mem, 8, yes, -1, tell("a1"))
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		if r := Wait(mem, 8, yes, -1, nil, nil); r != OK {
			t.Errorf("sync waiter: %v", r)
		}
		mu.Lock()
		order = append(order, "s")
		mu.Unlock()
	}()
	until(t, func() bool { return waiting(mem, 8) == 2 })
	WaitAsync(mem, 8, yes, -1, tell("a2"))
	WaitAsync(other, 8, yes, -1, tell("other"))
	WaitAsync(mem, 16, yes, -1, tell("elsewhere"))
	if n := Notify(mem, 8, 0); n != 0 {
		t.Fatalf("notify 0 woke %d", n)
	}
	if n := Notify(mem, 8, 1); n != 1 {
		t.Fatalf("notify 1 woke %d", n)
	}
	if n := Notify(mem, 8, 1); n != 1 {
		t.Fatalf("notify 1 woke %d", n)
	}
	wg.Wait()
	if n := Notify(mem, 8, ^uint64(0)); n != 1 {
		t.Fatalf("notify all woke %d", n)
	}
	if n := Notify(mem, 8, 1); n != 0 {
		t.Fatalf("notify of none woke %d", n)
	}
	mu.Lock()
	got := order
	mu.Unlock()
	if len(got) != 3 || got[0] != "a1" || got[1] != "s" || got[2] != "a2" {
		t.Fatalf("woken in the order %v", got)
	}
	// A notify of several counts each.
	for range 3 {
		WaitAsync(mem, 8, yes, -1, func(Result) {})
	}
	if n := Notify(mem, 8, 2); n != 2 || waiting(mem, 8) != 1 || Notify(mem, 8, 5) != 1 {
		t.Fatalf("notify 2 of 3 woke %d", n)
	}
	if Notify(other, 8, 1) != 1 || Notify(mem, 16, 1) != 1 || left() != 0 {
		t.Fatalf("%d waiters left", left())
	}
}

// left is how many slots of the table have waiters.
func left() int {
	n := 0
	for i := range table {
		t := &table[i]
		t.Lock()
		n += len(t.m)
		t.Unlock()
	}
	return n
}

// TestWaitTimeouts pins the ends of a wait other than a notify: its time
// running out, its being stopped, and an asynchronous one's cancel, after
// which it is told nothing and counted by no notify.
func TestWaitTimeouts(t *testing.T) {
	mem := new(int)
	if r := Wait(mem, 0, yes, time.Millisecond, nil, nil); r != TimedOut {
		t.Fatalf("timeout: %v", r)
	}
	stop := make(chan struct{})
	close(stop)
	if r := Wait(mem, 0, yes, -1, nil, stop); r != TimedOut {
		t.Fatalf("stopped: %v", r)
	}
	told := make(chan Result, 1)
	WaitAsync(mem, 0, yes, time.Millisecond, func(r Result) { told <- r })
	if r := <-told; r != TimedOut {
		t.Fatalf("async timeout: %v", r)
	}
	w, _ := WaitAsync(mem, 0, yes, time.Hour, func(r Result) { t.Errorf("told %v after a cancel", r) })
	Cancel(w)
	Cancel(w)
	if n := Notify(mem, 0, 1); n != 0 || waiting(mem, 0) != 0 {
		t.Fatalf("a cancelled waiter was woken: %d", n)
	}
}

// The table has four shards for each processor, a power of two from 64 to
// 1024, each a cache line pair of its own.
func TestShards(t *testing.T) {
	for _, c := range []struct{ cpus, want int }{
		{1, 64}, {8, 64}, {16, 64}, {17, 128}, {32, 128}, {33, 256}, {64, 256},
		{128, 512}, {200, 1024}, {256, 1024}, {1024, 1024},
	} {
		if got := shardsFor(c.cpus); got != c.want {
			t.Errorf("%d processors: %d shards, want %d", c.cpus, got, c.want)
		}
	}
	if n := len(table); n != shardsFor(runtime.NumCPU()) || 1<<(64-shardShift) != n {
		t.Errorf("%d shards, shift %d, on %d processors", n, shardShift, runtime.NumCPU())
	}
	if s := unsafe.Sizeof(shard{}); s != 128 {
		t.Errorf("a shard is %d bytes, want 128", s)
	}
}
