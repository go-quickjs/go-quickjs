package vm

import (
	"testing"
	"unsafe"
)

// A closure is one allocation -- the object, its function data, its closure
// and, for an arrow, what it captured of its maker's frame -- and the
// programs DeltaBlue and Havlak are make one for each forEach and each loop
// over a set, so its size is most of what they allocate. On a 64-bit
// platform, an arrow with none of its maker's bindings fits Go's 256-byte
// size class and one with one or two the 288-byte class, a function of
// one or two the 240-byte class: they had been 456 to 472 bytes, the 480-byte
// class, before the parts of funcExtra few functions have went behind
// funcExtra.rare, funcData's length beside its flags, and what a template's
// closures share behind closureCode; and 320 to 352, before a method's and
// a constructor's fields went behind funcData.cls and an arrow's
// new.target into its rare part. A field added to any of them that moves a
// closure into the next class fails here.
func TestClosureSizes(t *testing.T) {
	if unsafe.Sizeof(uintptr(0)) != 8 {
		t.Skip("the sizes are a 64-bit platform's")
	}
	for _, c := range []struct {
		name      string
		size, max uintptr
	}{
		{"funcExtra", unsafe.Sizeof(funcExtra{}), 32},
		{"funcData", unsafe.Sizeof(funcData{}), 88},
		{"closure", unsafe.Sizeof(closure{}), 56},
		{"arrowFuncObject", unsafe.Sizeof(arrowFuncObject{}), 256},
		{"arrowFuncObject1", unsafe.Sizeof(arrowFuncObject1{}), 288},
		{"arrowFuncObject2", unsafe.Sizeof(arrowFuncObject2{}), 288},
		{"scriptFuncObject", unsafe.Sizeof(scriptFuncObject{}), 224},
		{"scriptFuncObject1", unsafe.Sizeof(scriptFuncObject1{}), 240},
		{"scriptFuncObject2", unsafe.Sizeof(scriptFuncObject2{}), 240},
	} {
		if c.size > c.max {
			t.Errorf("%s is %d bytes, past %d", c.name, c.size, c.max)
		}
	}
}

// An arrow made inside a method shares the method's class part, which
// says its super, rather than making one at each (funcData's classPart).
func TestArrowSharesClassPart(t *testing.T) {
	r := New(Config{})
	defer r.Close()
	if _, err := r.Run(compileForTest(t, `class A { m() { return () => super.toString === Object.prototype.toString } }
		var m = A.prototype.m, f = new A().m()`)); err != nil {
		t.Fatal(err)
	}
	m := r.global.getOwn(r.atoms.intern("m")).value().Object().fn()
	f := r.global.getOwn(r.atoms.intern("f")).value().Object().fn()
	if m.cls == nil || f.cls != m.cls {
		t.Error("an arrow made in a method has a class part of its own")
	}
	if v, err := r.Run(compileForTest(t, `f()`)); err != nil || !v.Truthy() {
		t.Errorf("the arrow's super: %v, %v", v, err)
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
