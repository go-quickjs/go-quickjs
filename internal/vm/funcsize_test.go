package vm

import (
	"testing"
	"unsafe"
)

// A closure is one allocation -- the object, its function data, its closure
// and, for an arrow, what it captured of its maker's frame -- and the
// programs DeltaBlue and Havlak are make one for each forEach and each loop
// over a set, so its size is most of what they allocate. On a 64-bit
// platform, an arrow with up to one binding of its maker's fits Go's
// 320-byte size class and one with two the 352-byte class: they had been
// 456 to 472 bytes, the 480-byte class, before the parts of funcExtra few
// functions have went behind funcExtra.rare, funcData's length beside its
// flags, and what a template's closures share behind closureCode. A field
// added to any of them that moves an arrow into the next class fails here.
func TestClosureSizes(t *testing.T) {
	if unsafe.Sizeof(uintptr(0)) != 8 {
		t.Skip("the sizes are a 64-bit platform's")
	}
	for _, c := range []struct {
		name      string
		size, max uintptr
	}{
		{"funcExtra", unsafe.Sizeof(funcExtra{}), 48},
		{"funcData", unsafe.Sizeof(funcData{}), 104},
		{"closure", unsafe.Sizeof(closure{}), 64},
		{"arrowFuncObject", unsafe.Sizeof(arrowFuncObject{}), 320},
		{"arrowFuncObject1", unsafe.Sizeof(arrowFuncObject1{}), 320},
		{"arrowFuncObject2", unsafe.Sizeof(arrowFuncObject2{}), 352},
		{"scriptFuncObject1", unsafe.Sizeof(scriptFuncObject1{}), 288},
	} {
		if c.size > c.max {
			t.Errorf("%s is %d bytes, past %d", c.name, c.size, c.max)
		}
	}
}

// TestGeneratorSizes pins the sizes of a generator's one allocation -- its
// object, its state, and, for a function of up to two, four or eight
// arguments and locals, those -- at Go's 288-, 320- and 384-byte size
// classes: a generator or an async function is made at every call. They
// were 312, 344 and 408 bytes, the 320-, 352- and 416-byte classes,
// before the with scopes went into genRare.
func TestGeneratorSizes(t *testing.T) {
	if unsafe.Sizeof(uintptr(0)) != 8 {
		t.Skip("the sizes are a 64-bit platform's")
	}
	for _, c := range []struct {
		name      string
		size, max uintptr
	}{
		{"generator", unsafe.Sizeof(generator{}), 160},
		{"generatorObject", unsafe.Sizeof(generatorObject{}), 256},
		{"generatorVals2", unsafe.Sizeof(generatorVals[[2]Value]{}), 288},
		{"generatorVals4", unsafe.Sizeof(generatorVals[[4]Value]{}), 320},
		{"generatorVals8", unsafe.Sizeof(generatorVals[[8]Value]{}), 384},
	} {
		if c.size > c.max {
			t.Errorf("%s is %d bytes, past %d", c.name, c.size, c.max)
		}
	}
}
