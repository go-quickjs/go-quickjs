package bytecode

import (
	"testing"
	"unsafe"
)

// TestRecordedFactsInPadding holds the facts the compiler records for the
// JIT (Function's FrameCallee and the five like it, UpvalueDesc's
// Assigned) to the padding they were put in, on every architecture: each
// structure is the size it was, and the fields the interpreter reads did
// not move. Three after Anonymous end where Script, a pointer, begins;
// three after ArrowThisValue, and Assigned after ByValue, end each
// structure.
func TestRecordedFactsInPadding(t *testing.T) {
	var f Function
	var u UpvalueDesc
	align := func(n, a uintptr) uintptr { return (n + a - 1) &^ (a - 1) }
	ptr := unsafe.Alignof(f.Script)
	for _, c := range []struct {
		name      string
		got, want uintptr
	}{
		{"AssignsParams", unsafe.Offsetof(f.AssignsParams), unsafe.Offsetof(f.Anonymous) + 1},
		{"MakesArguments", unsafe.Offsetof(f.MakesArguments), unsafe.Offsetof(f.Anonymous) + 2},
		{"SuperForwards", unsafe.Offsetof(f.SuperForwards), unsafe.Offsetof(f.Anonymous) + 3},
		{"Script", unsafe.Offsetof(f.Script), align(unsafe.Offsetof(f.Anonymous)+1, ptr)},
		{"FrameCallee", unsafe.Offsetof(f.FrameCallee), unsafe.Offsetof(f.ArrowThisValue) + 1},
		{"ReadsMaker", unsafe.Offsetof(f.ReadsMaker), unsafe.Offsetof(f.ArrowThisValue) + 2},
		{"HasYieldStar", unsafe.Offsetof(f.HasYieldStar), unsafe.Offsetof(f.ArrowThisValue) + 3},
		{"Function's size", unsafe.Sizeof(f), align(unsafe.Offsetof(f.ArrowThisValue)+1, unsafe.Alignof(f))},
		{"Assigned", unsafe.Offsetof(u.Assigned), unsafe.Offsetof(u.ByValue) + 1},
		{"UpvalueDesc's size", unsafe.Sizeof(u), align(unsafe.Offsetof(u.ByValue)+1, unsafe.Alignof(u))},
	} {
		if c.got != c.want {
			t.Errorf("%s at %d, want %d", c.name, c.got, c.want)
		}
	}
}
