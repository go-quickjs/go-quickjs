package vm

import (
	"reflect"
	"testing"
	"unsafe"
)

// The JIT must cost nothing when it is off, in builds with it as in builds
// without: no object grows a size class, and nothing the interpreter reads
// moves. The JIT's fields sit second to last, after every other field (an
// empty last field would add padding).
func TestJITFieldLayout(t *testing.T) {
	if unsafe.Sizeof(uintptr(0)) == 8 {
		if size := unsafe.Sizeof(closure{}); size != 128 {
			t.Errorf("closure is %d bytes, want 128 (its size class)", size)
		}
	}
	for _, tc := range []struct {
		typ   reflect.Type
		field string
	}{
		{reflect.TypeOf(Runtime{}), "jitFields"},
		{reflect.TypeOf(Realm{}), "jitRealmFields"},
	} {
		f, ok := tc.typ.FieldByName(tc.field)
		if !ok || len(f.Index) != 1 || f.Index[0] != tc.typ.NumField()-2 {
			t.Errorf("%s.%s is not second to last", tc.typ.Name(), tc.field)
		}
	}
}
