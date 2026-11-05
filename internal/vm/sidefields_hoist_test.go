package vm

import (
	"testing"
	"unsafe"
)

// TestArrayExtOverlay checks that arrayExtObject, the struct arrayExt sees
// an array's allocation as, has its side fields right after the Object,
// where embedArrayExt puts them.
func TestArrayExtOverlay(t *testing.T) {
	if off, size := unsafe.Offsetof(arrayExtObject{}.side), unsafe.Sizeof(Object{}); off != size {
		t.Fatalf("arrayExtObject's side fields at %d, not right after the Object (%d)", off, size)
	}
}

// arrayExtHoistProbe is the shape Go 1.27 hoisted jitSortCopy's a.extAt()
// out of: an accessor's address, used as an address, in a branch of a loop
// on a base that is nil on the other path, live across a call that grows
// the stack.
//
//go:noinline
func arrayExtHoistProbe(o *Object, n int) int {
	t := 0
	for i := 0; i < n; i++ {
		if o != nil {
			t += arrayExtHoistLen(o.arrayExt())
		}
		t += arrayExtHoistGrow(i)
	}
	return t
}

//go:noinline
func arrayExtHoistLen(a *arrayExt) int { return int(a.arrayLen) }

// arrayExtHoistGrow grows the goroutine's stack, which copies it and checks
// every pointer slot of the frames below it.
//
//go:noinline
func arrayExtHoistGrow(i int) int {
	var buf [16 << 10]byte
	buf[i&1023] = byte(i)
	return int(buf[0] & 1)
}

// TestArrayExtNotHoisted runs arrayExtHoistProbe with a nil array and a
// real one, each on a new goroutine's small stack, which the probe's call
// grows. With arrayExt written as unsafe.Add(o, 32), Go 1.27.3 computes the
// sum before the loop, and the nil array leaves 0x20 in a pointer slot:
// "invalid pointer found on stack" on the first run.
func TestArrayExtNotHoisted(t *testing.T) {
	arr := newObject(nil, ClassArray)
	for k := 0; k < 200; k++ {
		done := make(chan int)
		go func() { done <- arrayExtHoistProbe(nil, 2) + arrayExtHoistProbe(arr, 2) }()
		<-done
	}
}
