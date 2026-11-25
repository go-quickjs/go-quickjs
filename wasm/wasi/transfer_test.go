package wasi_test

import (
	"math"
	"math/bits"
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
