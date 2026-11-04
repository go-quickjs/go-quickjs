package vm

import (
	"testing"
	"unsafe"
)

// A shape keeps what few layouts have -- a prototype's roots past the
// first, its chain's cell, a for-in loop's keys -- on a side it has only when it needs one, so
// that the layout of an object made on a prototype of its own is in the
// 112-byte size class (80 on 32 bits), not 144's; and a prototype's side
// goes with its layout when the layout is given a new identity, its cell
// still invalidated as its chain changes.
func TestShapeSide(t *testing.T) {
	size, want := unsafe.Sizeof(shape{}), uintptr(112)
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
	if p := globalObject(r, "p"); p.shape.protoRoot() == nil {
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

// A shared layout's first child extends its keys' array in place and the
// others copy it, which the shape no longer records but reads off its first
// transition (extendKeys): three objects grown from one layout by different
// keys, and one of them grown further, each have their own keys, and only
// the first child shares its parent's array.
func TestShapeChildKeys(t *testing.T) {
	r := New(Config{})
	defer r.Close()
	v, err := r.Run(compileForTest(t, `
		function mk() { return {} }
		var o1 = mk(); o1.a = 1; o1.b = 2; o1.c = 3; o1.d = 4
		var o2 = mk(); o2.a = 1; o2.b = 2; o2.c = 3; o2.e = 5
		var o3 = mk(); o3.a = 1; o3.b = 2; o3.c = 3; o3.f = 6
		o1.g = 7;
		[o1, o2, o3].map(o => Object.keys(o).join("")).join(",")`))
	if err != nil {
		t.Fatal(err)
	}
	if s := v.String().Go(); s != "abcdg,abce,abcf" {
		t.Errorf("keys = %s; want abcdg,abce,abcf", s)
	}
	o1, o2, o3 := globalObject(r, "o1"), globalObject(r, "o2"), globalObject(r, "o3")
	x := o1.shape.parent.parent
	if x == nil || o2.shape.parent != x || o3.shape.parent != x || x.n != 3 {
		t.Fatalf("o1, o2 and o3 do not grow from one layout of a, b and c")
	}
	if x.next != o1.shape.parent || x.nextEdge() != shapeEdge(o1.shape.keys[3]) {
		t.Errorf("x's first transition is not o1's by d")
	}
	if len(x.more) != 2 || x.more[shapeEdge(o2.shape.keys[3])] != o2.shape || x.more[shapeEdge(o3.shape.keys[3])] != o3.shape {
		t.Errorf("x's other transitions: %d, want o2's and o3's", len(x.more))
	}
	// x's array has room for a fourth key: its first child's is in it.
	if cap(x.keys) < 4 || &o1.shape.parent.keys[0] != &x.keys[0] {
		t.Errorf("x's first child copied its keys; want them in x's array")
	}
	if &o2.shape.keys[0] == &x.keys[0] || &o3.shape.keys[0] == &x.keys[0] {
		t.Errorf("a later child of x extended x's array in place")
	}
}

// A prototype's own layout is unique and grew from no other, and its parent
// field holds the first root its objects start from (protoRoot): deleting
// the prototype's last key leaves a tombstone rather than going back to a
// parent, and its objects made after it still start from that root.
func TestShapeProtoRootInParent(t *testing.T) {
	r := New(Config{})
	defer r.Close()
	v, err := r.Run(compileForTest(t, `
		var p = {m: 1, n: 2}
		var a = Object.create(p), b = Object.create(p)
		delete p.n
		var c = Object.create(p)
		c.x = 1;
		[Object.keys(p).join(""), String(p.n), c.m, Object.getPrototypeOf(c) === p].join(",")`))
	if err != nil {
		t.Fatal(err)
	}
	if s := v.String().Go(); s != "m,undefined,1,true" {
		t.Errorf("got %s; want m,undefined,1,true", s)
	}
	p, c := globalObject(r, "p"), globalObject(r, "c")
	root := p.shape.protoRoot()
	if !p.shape.unique || root == nil || root.proto != p || root.unique {
		t.Fatalf("p's layout: unique %v, root %p; want unique, with a root of p's", p.shape.unique, root)
	}
	if c.shape.parent != root {
		t.Errorf("c, made on p after the delete, did not grow from p's root")
	}
}
