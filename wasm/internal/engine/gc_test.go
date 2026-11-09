package engine

import (
	"testing"
	"unsafe"
)

// TestHeaderIsRefSized pins the layout a struct's first cell relies on.
func TestHeaderIsRefSized(t *testing.T) {
	if unsafe.Sizeof(header{}) != unsafe.Sizeof(Ref{}) ||
		unsafe.Offsetof(header{}.rtt) != unsafe.Offsetof(Ref{}.p) ||
		unsafe.Offsetof(header{}.kind) != unsafe.Offsetof(Ref{}.i) {
		t.Fatal("header and Ref differ in layout")
	}
}
