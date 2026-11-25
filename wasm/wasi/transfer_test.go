package wasi_test

import (
	"math"
	"math/bits"
	"runtime"
	"testing"

	"github.com/go-quickjs/go-quickjs/wasm"
)

// TestWriteCountFitsU32 checks that a write whose iovecs name more than a
// u32 can count -- here three of 2 GiB over the same bytes -- writes what
// the count can report and reports it, a short write, rather than a count
// wrapped around 4 GiB.
func TestWriteCountFitsU32(t *testing.T) {
	if bits.UintSize == 32 {
		t.Skip("a 2 GiB memory does not fit a 32-bit address space")
	}
	h := newHarness(t, wasm.NewModuleConfig())
	if _, ok := h.mem.Grow(1<<15 - 1); !ok {
		t.Skip("no 2 GiB memory to be had")
	}
	const iovs = scratch
	for i := range uint32(3) {
		h.iovec(iovs+8*i, 0, 1<<31)
	}
	h.expect("ESUCCESS", "fd_write", 1, iovs, 3, out)
	if got := h.u32(out); got != math.MaxUint32 {
		t.Errorf("fd_write reported %d bytes written; want %d", got, uint32(math.MaxUint32))
	}
}

// TestIovecsCostNoHostMemory checks that the count of iovecs a read or a
// write is given costs the host no memory of its own per iovec: 2M of them,
// which a module's 16 MiB of memory holds, once cost 48 MiB of Go heap in
// one call. The iovecs are all read, the empty ones skipped: the last one's
// bytes are written.
func TestIovecsCostNoHostMemory(t *testing.T) {
	h := newHarness(t, wasm.NewModuleConfig())
	if _, ok := h.mem.Grow(255); !ok {
		t.Fatal("no 16 MiB memory")
	}
	const iovs = 0x10000
	count := (h.mem.Size() - iovs) / 8
	h.iovec(iovs+8*(count-1), scratch, 5)
	call := func(name string) uint64 {
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		h.expect("ESUCCESS", name, 1, iovs, uint64(count), out)
		runtime.ReadMemStats(&after)
		return after.TotalAlloc - before.TotalAlloc
	}
	call("fd_write") // the first call's own allocations
	if n := call("fd_write"); n > 1<<20 {
		t.Errorf("fd_write of %d iovecs allocated %d bytes", count, n)
	}
	if got := h.u32(out); got != 5 {
		t.Errorf("fd_write wrote %d bytes; want 5", got)
	}
}

// TestPollSubscriptionsCostNoHostMemory checks that poll_oneoff's
// subscriptions cost the host no memory of its own each: 200,000 clocks,
// all due, once cost 1.6 MB of Go heap in one call.
func TestPollSubscriptionsCostNoHostMemory(t *testing.T) {
	h := newHarness(t, wasm.NewModuleConfig())
	if _, ok := h.mem.Grow(256); !ok {
		t.Fatal("no 16 MiB memory")
	}
	// Memory as it is grown holds subscriptions to the real-time clock,
	// relative and due at once.
	const subs, n = 0x10000, 200_000
	events := uint64(subs + 48*n)
	call := func() uint64 {
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		h.expect("ESUCCESS", "poll_oneoff", subs, events, n, out)
		runtime.ReadMemStats(&after)
		return after.TotalAlloc - before.TotalAlloc
	}
	call()
	// What a call costs besides -- some 330 KB on 386 and 400 KB under
	// the race detector -- is not per subscription: half of the 1.6 MB.
	if got := call(); got > 800<<10 {
		t.Errorf("poll_oneoff of %d subscriptions allocated %d bytes", n, got)
	}
	if got := h.u32(out); got != n {
		t.Errorf("poll_oneoff reported %d events; want %d", got, n)
	}
}
