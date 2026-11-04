package vm

import (
	"testing"
	"unsafe"
)

// A shape keeps what few layouts have -- a prototype's roots past the
// first, its chain's cell, a for-in loop's keys -- on a side it has only
// when it needs one, so that the layout of an object made on a prototype of
// its own is in the 128-byte size class (80 on 32 bits), not 144's; and a
// prototype's side goes with its layout when the layout is given a new
// identity, its cell still invalidated as its chain changes.
func TestShapeSide(t *testing.T) {
	size, want := unsafe.Sizeof(shape{}), uintptr(128)
	if unsafe.Sizeof(uintptr(0)) == 4 {
		want = 80
	}
	if size > want {
		t.Errorf("shape is %d bytes; want at most %d", size, want)
	}
	r := New(Config{})
	defer r.Close()
	if _, err := r.Run(compileForTest(t, `
		var p = {m: 1}
		var a = Object.create(p), b = Object.create(p)
		var q = Object.create(p), c = Object.create(q)
		function rd(o) { return o.m }
		function absent(o) { return o.nothing }
		var got = 0
		for (var i = 0; i < 5; i++) got += rd(c) + (absent(c) === undefined ? 1 : 0)
		var w = {u: 1, v: 2}, keys = ""
		for (var k in w) keys += k`)); err != nil {
		t.Fatal(err)
	}
	if a := globalObject(r, "a"); a.shape.side != nil {
		t.Errorf("first object made on p: side %+v; want none", *a.shape.side)
	}
	if p := globalObject(r, "p"); p.shape.protoRoot == nil {
		t.Errorf("p, the prototype of b: no root kept on its layout")
	}
	if b := globalObject(r, "b"); b.shape.side != nil {
		t.Errorf("second object made on p: side %+v; want none", *b.shape.side)
	}
	if w := globalObject(r, "w"); w.shape.side == nil || w.shape.side.forIn == nil {
		t.Errorf("object walked by for-in: no keys kept on its layout's side")
	}
	q := globalObject(r, "q")
	info := infoOf[struct{}](q.shape)
	if info == nil {
		t.Fatalf("q, a prototype a read found a property past: no cell on its layout")
	}
	// q's layout, which caches remember, is given a new identity as q is
	// given a property: the side goes with it.
	old := q.shape
	if _, err := r.Run(compileForTest(t, `q.extra = 1`)); err != nil {
		t.Fatal(err)
	}
	if q.shape == old {
		t.Fatalf("q given a property kept the layout caches remembered")
	}
	if infoOf[struct{}](q.shape) != info || old.side != nil {
		t.Errorf("q's new layout: info %p, old layout's side %p; want %p and none", infoOf[struct{}](q.shape), old.side, info)
	}
	v, err := r.Run(compileForTest(t, `
		Object.setPrototypeOf(q, {m: 7})
		var after = 0
		for (var i = 0; i < 5; i++) after += rd(c) + (absent(c) === undefined ? 1 : 0)
		got + "," + after + "," + keys`))
	if err != nil {
		t.Fatal(err)
	}
	if s := v.String().Go(); s != "10,40,uv" {
		t.Errorf("reads through the chain = %s; want 10,40,uv", s)
	}
}
