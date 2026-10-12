//go:build quickjs_jit

package jit

import (
	"runtime"
	"sync/atomic"
	"testing"
	"unsafe"
)

// The runtime's write-barrier flag is clear once a collection has finished
// and set while one marks: what native reference stores rely on, checked
// with each Go they are built with.
func TestWriteBarrierFlag(t *testing.T) {
	flag := (*uint32)(unsafe.Pointer(&writeBarrier))
	if uint64(uintptr(unsafe.Pointer(flag))) != WriteBarrier() {
		t.Fatal("WriteBarrier is not the flag's address")
	}
	runtime.GC()
	if atomic.LoadUint32(flag)&0xff != 0 {
		t.Fatal("the flag is set after a collection finished")
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		var sink [][]byte
		for range 200 {
			for range 1000 {
				sink = append(sink, make([]byte, 64))
			}
			sink = sink[:0]
			runtime.GC()
		}
	}()
	set := 0
	for {
		select {
		case <-done:
			if set == 0 {
				t.Fatal("the flag was never set while collections ran")
			}
			return
		default:
		}
		if atomic.LoadUint32(flag)&0xff != 0 {
			set++
		}
	}
}
