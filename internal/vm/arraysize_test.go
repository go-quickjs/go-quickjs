package vm

import (
	"testing"
	"unsafe"
)

// An array's side fields are its elements' slice and its length, and no
// internal slots (arrayExt): an array of up to four elements in its own
// allocation is 128 bytes, Go's size class of 128, and a regexp's match
// result of up to four groups each the size class it fills. They had been
// 16 bytes more, a class up, with the data every other kind of object has.
func TestArraySizes(t *testing.T) {
	if unsafe.Sizeof(uintptr(0)) != 8 {
		t.Skip("the sizes are a 64-bit platform's")
	}
	for _, c := range []struct {
		name       string
		size, want uintptr
	}{
		{"arrayExt", unsafe.Sizeof(arrayExt{}), 32},
		{"arrayObject", unsafe.Sizeof(arrayObject{}), 128},
		{"matchResult1Object", unsafe.Sizeof(matchResult1Object{}), 176},
		{"matchResult2Object", unsafe.Sizeof(matchResult2Object{}), 192},
		{"matchResult3Object", unsafe.Sizeof(matchResult3Object{}), 208},
		{"matchResult4Object", unsafe.Sizeof(matchResult4Object{}), 224},
		{"matchResultObject", unsafe.Sizeof(matchResultObject{}), 240},
	} {
		if c.size != c.want {
			t.Errorf("%s is %d bytes, want %d", c.name, c.size, c.want)
		}
	}
	r := New(Config{})
	defer r.Close()
	v, err := r.Run(compileForTest(t, `[1, 2, 3]`))
	if err != nil {
		t.Fatal(err)
	}
	if a := v.Object(); a.flags&objHasExt == 0 || a.flags&objHasData != 0 || a.dataOf() != nil {
		t.Errorf("an array's flags %#x: want side fields with no data", a.flags)
	}
}
