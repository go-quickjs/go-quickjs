package vm

import (
	"testing"
)

// globalObject is the object a script's global var name holds.
func globalObject(r *Runtime, name string) *Object {
	return r.global.getOwn(r.atoms.intern(name)).value().Object()
}

// Deleting the key last added to a shared layout takes the object back to
// the layout it had before, as V8 rolls a map back along its transition;
// deleting another makes it a dictionary.
func TestShapeDeleteLastRollsBack(t *testing.T) {
	r := New(Config{})
	defer r.Close()
	if _, err := r.Run(compileForTest(t, `
		function make() { return {a: 1, b: 2, c: 3} }
		var whole = make(), abc = make(), mid = make()
		delete abc.c
		delete mid.b`)); err != nil {
		t.Fatal(err)
	}
	whole, abc, mid := globalObject(r, "whole"), globalObject(r, "abc"), globalObject(r, "mid")
	if abc.shape != whole.shape.parent || abc.shape.unique || len(abc.vals) != 2 {
		t.Fatalf("delete of the last key: shape %p unique %v, %d values; want the layout before c, %p", abc.shape, abc.shape.unique, len(abc.vals), whole.shape.parent)
	}
	if !mid.shape.unique || !r.dictObject(mid) {
		t.Fatal("delete of a key before the last did not make a dictionary")
	}
}

// Objects of one layout that change a property's attributes alike share
// the layout the change makes -- Object.freeze of a class's instances, a
// defineProperty in a constructor -- as V8's MapUpdater replays a map's
// transitions past the descriptor it reconfigures; each had been given a
// layout of its own.
func TestShapeReconfigureShared(t *testing.T) {
	r := New(Config{})
	defer r.Close()
	v, err := r.Run(compileForTest(t, `
		function P(x, y) { this.x = x; this.y = y; this.z = x + y }
		var f1 = Object.freeze(new P(1, 2)), f2 = Object.freeze(new P(3, 4))
		var r1 = new P(1, 2), r2 = new P(5, 6)
		Object.defineProperty(r1, "y", {writable: false})
		Object.defineProperty(r2, "y", {writable: false})
		;[Object.isFrozen(f1), Object.isFrozen(f2), f1.x + f1.y + f1.z, f2.z,
			Object.getOwnPropertyDescriptor(r1, "y").writable, Object.getOwnPropertyDescriptor(r2, "x").writable,
			Object.keys(r2).join("")].join()`))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := v.String().Go(), "true,true,6,7,false,true,xyz"; got != want {
		t.Fatalf("got %s, want %s", got, want)
	}
	f1, f2, r1, r2 := globalObject(r, "f1"), globalObject(r, "f2"), globalObject(r, "r1"), globalObject(r, "r2")
	if f1.shape != f2.shape || f1.shape.unique {
		t.Fatalf("frozen objects of a layout do not share one: %p %p unique %v", f1.shape, f2.shape, f1.shape.unique)
	}
	if r1.shape != r2.shape || r1.shape.unique {
		t.Fatalf("objects reconfigured alike do not share a layout: %p %p", r1.shape, r2.shape)
	}
}

// A keyed store adding past V8's soft limit of 12 makes the object a
// dictionary (Map's TooManyFastProperties); named stores do not, nor keyed
// ones within it.
func TestShapeKeyedStoresPastSoftLimit(t *testing.T) {
	r := New(Config{})
	defer r.Close()
	if _, err := r.Run(compileForTest(t, `
		var keyed = {}, few = {}
		for (var i = 0; i < 20; i++) keyed["k" + i] = i
		for (var i = 0; i < 12; i++) few["k" + i] = i
		function N() { for (var i = 0; i < 1; i++) {} this.a0 = 0; this.a1 = 1; this.a2 = 2; this.a3 = 3; this.a4 = 4;
			this.a5 = 5; this.a6 = 6; this.a7 = 7; this.a8 = 8; this.a9 = 9; this.a10 = 10; this.a11 = 11; this.a12 = 12;
			this.a13 = 13; this.a14 = 14; this.a15 = 15; this.a16 = 16; this.a17 = 17; this.a18 = 18; this.a19 = 19 }
		var named = new N()`)); err != nil {
		t.Fatal(err)
	}
	if k := globalObject(r, "keyed"); !k.shape.unique || len(k.vals) != 20 {
		t.Fatalf("20 keyed adds: unique %v, %d values; want a dictionary", k.shape.unique, len(k.vals))
	}
	if f := globalObject(r, "few"); f.shape.unique {
		t.Fatal("12 keyed adds made a dictionary")
	}
	if n := globalObject(r, "named"); n.shape.unique || n.shape.n != 20 {
		t.Fatalf("20 named adds: unique %v, n %d", n.shape.unique, n.shape.n)
	}
}

// A site meeting many dictionary objects remembers them all (dictShape),
// as V8's caches stay monomorphic on dictionary-mode objects' shared map,
// and answers by a lookup in each one's table: an own property, a method
// on the prototype the objects share -- one that has its own of the name
// shadowing it -- a write, and tables of null prototypes filled by keyed
// stores past the soft limit, keys of their own. Each answer is the
// uncached lookup's.
func TestShapeDictionarySites(t *testing.T) {
	r := New(Config{})
	defer r.Close()
	v, err := r.Run(compileForTest(t, `
		function D(i) { this.a = i; this.b = i * 2; this.c = i * 3 }
		D.prototype.m = function () { return this.a }
		function read(o) { return o.b }
		function meth(o) { return o.m }
		function write(o, v) { o.b = v }
		var ds = []
		for (var i = 0; i < 10; i++) { var d = new D(i); delete d.a; d.a = i; ds.push(d) }
		ds[7].m = function () { return -1 }
		var sum = 0, calls = 0
		for (var round = 0; round < 3; round++)
			for (var i = 0; i < ds.length; i++) { sum += read(ds[i]); calls += meth(ds[i]).call(ds[i]) }
		for (var i = 0; i < ds.length; i++) write(ds[i], 100 + i)
		var after = 0
		for (var i = 0; i < ds.length; i++) after += read(ds[i])
		var maps = []
		for (var i = 0; i < 6; i++) { var m = Object.create(null); m.k = i; m.j = -i; for (var q = 0; q < 20; q++) m["q" + i + "_" + q] = q; maps.push(m) }
		function getK(m) { return m.k }
		var ks = 0
		for (var round = 0; round < 3; round++) for (var i = 0; i < maps.length; i++) ks += getK(maps[i])
		function absentK(m) { return m.k }
		;[sum, calls, after, ks, absentK({}) === undefined].join()`))
	if err != nil {
		t.Fatal(err)
	}
	// sum: 3 rounds of 2*(0..9) = 270; calls: 3*(45-7-1) = 111; after: 100..109 = 1045; ks: 3*15.
	if got, want := v.String().Go(), "270,111,1045,45,true"; got != want {
		t.Fatalf("got %s, want %s", got, want)
	}
	for _, name := range []string{"read", "meth", "write", "getK"} {
		cl := globalObject(r, name).fn().closure
		dict := false
		for i := range cl.ic {
			dict = dict || cl.ic[i].shape == dictShape
		}
		if !dict {
			t.Errorf("%s's site does not remember dictionary objects", name)
		}
	}
}
