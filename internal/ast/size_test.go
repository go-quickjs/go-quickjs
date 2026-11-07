package ast

import (
	"testing"
	"unsafe"
)

// The nodes a program is mostly made of. A parse's bytes are most of what
// the collector does during a compile, and a field added to one of these,
// or two of them apart, is paid for at every identifier and call.
func TestNodeSizes(t *testing.T) {
	if unsafe.Sizeof(uintptr(0)) != 8 {
		t.Skip("sizes on a 64-bit platform")
	}
	for _, tt := range []struct {
		name       string
		got, limit uintptr
	}{
		{"Ident", unsafe.Sizeof(Ident{}), 24},
		{"Member", unsafe.Sizeof(Member{}), 40},
		{"Call", unsafe.Sizeof(Call{}), 56},
		{"Assign", unsafe.Sizeof(Assign{}), 56},
		{"Logical", unsafe.Sizeof(Logical{}), 56},
		{"ArrayLit", unsafe.Sizeof(ArrayLit{}), 32},
	} {
		if tt.got > tt.limit {
			t.Errorf("%s is %d bytes, more than %d", tt.name, tt.got, tt.limit)
		}
	}
}
