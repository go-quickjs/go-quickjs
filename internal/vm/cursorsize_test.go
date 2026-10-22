package vm

import (
	"testing"
	"unsafe"
)

// A for-of's cursor and its object -- every for-of's, spread's and array
// destructuring's -- are 160 bytes, a size class, with what only for-in
// walks kept apart in its own cursor's allocation, which stays in the 240
// bytes both had been.
func TestCursorSizes(t *testing.T) {
	if unsafe.Sizeof(uintptr(0)) != 8 {
		t.Skip("sizes of a 64-bit build")
	}
	if size := unsafe.Sizeof(iterObject{}); size != 160 {
		t.Errorf("iterObject is %d bytes, want 160", size)
	}
	if size := unsafe.Sizeof(forInObject{}); size > 240 {
		t.Errorf("forInObject is %d bytes, want at most 240", size)
	}
}
