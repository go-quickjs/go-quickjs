package vm

import "testing"

// A literal's objects outgrow the room they were made with once, not at
// each doubling: its site's root notes the room they came to need
// (outgrowProps), and the next objects, the tree tier's, are made with it,
// from the third on.
// A keyed store's objects -- a table of names, a dictionary past the soft
// limit -- teach their site nothing, nor do objects with no site teach
// their class's root.
func TestObjectRoomLearnedFromGrowth(t *testing.T) {
	r := New(Config{})
	defer r.Close()
	_, err := r.Run(compileForTest(t, `
		function record() {
			var o = {};
			o.a = 1; o.b = 2; o.c = 3; o.d = 4; o.e = 5; o.f = 6; o.g = 7; o.h = 8; o.i = 9; o.j = 10;
			for (var i = 0; i < 10; i++) o[i] = i;
			return o;
		}
		function table(n) {
			var o = {};
			for (var i = 0; i < n; i++) o["k" + i] = i;
			return o;
		}
		function plain() {
			var o = Object.create(Object.prototype);
			o.a = 1; o.b = 2; o.c = 3; o.d = 4; o.e = 5; o.f = 6; o.g = 7; o.h = 8; o.i = 9; o.j = 10;
			o.k = 11; o.l = 12; o.m = 13; o.n = 14; o.o = 15; o.p = 16; o.q = 17; o.r = 18; o.s = 19; o.t = 20;
			return o;
		}
		function first() {
			var o = {};
			o.a = 1; o.b = 2; o.c = 3; o.d = 4; o.e = 5; o.f = 6; o.g = 7; o.h = 8; o.i = 9; o.j = 10;
			for (var i = 0; i < 10; i++) o[i] = i;
			return o;
		}
		var firsts = [first(), first(), first()];
		var rec, tab, small, pl;
		for (var n = 0; n < 200; n++) { rec = record(); tab = table(60); pl = plain(); }
		small = table(2);`))
	if err != nil {
		t.Fatal(err)
	}
	rec := globalObject(r, "rec")
	if n, c := len(rec.vals), cap(rec.vals); n != 20 || c != 20 {
		t.Errorf("a record's table: %d properties in room for %d, want 20 in 20", n, c)
	}
	if root := rec.shape.root(); root.slack != 20 {
		t.Errorf("the record literal's root notes room for %d, want 20", root.slack)
	}
	// The second object grows once, to what the first came to have along
	// the transitions it follows; the third is made with it.
	if third := globalObject(r, "firsts").elemsOf()[2].Object(); len(third.vals) != 20 || cap(third.vals) != 20 {
		t.Errorf("a literal's third object: %d properties in room for %d, want 20 in 20", len(third.vals), cap(third.vals))
	}
	if tab := globalObject(r, "tab"); !tab.shape.unique || len(tab.vals) != 60 {
		t.Errorf("a table of 60 names is not a dictionary: unique %v, %d properties", tab.shape.unique, len(tab.vals))
	}
	small := globalObject(r, "small")
	if root := small.shape.root(); root.slack > maxSlack+1 {
		t.Errorf("the table literal's root notes room for %d: its keyed stores taught it", root.slack)
	}
	pl := globalObject(r, "pl")
	if root := pl.shape.root(); root != r.shapes.roots[ClassObject] || root.slack > maxSlack+1 {
		t.Errorf("objects of no site taught their class's root room for %d", root.slack)
	}
}

// A lookup's recent entry for a key the shapes' shared index has past an
// object's table answers that the object has no such key, and the object
// that has it finds it: objects built along one chain of twenty keys, each
// stopped at its own length, then one of them extended, one rolled back
// by a delete of its last key, one given a key's tombstone and the key
// again.
func TestFindOwnPastTheTable(t *testing.T) {
	r := New(Config{})
	defer r.Close()
	v, err := r.Run(compileForTest(t, `
		function make(n) { var o = {}; for (var i = 0; i < n; i++) o["k" + i] = i; return o; }
		var all = make(20), objs = [], out = [];
		for (var n = 0; n <= 20; n++) objs.push(make(n));
		for (var n = 0; n <= 20; n++) {
			var o = objs[n], s = 0;
			for (var round = 0; round < 3; round++)
				for (var i = 0; i < 20; i++) if (("k" + i) in o) s += 1 << i;
			out.push(s);
		}
		var x = objs[10]; x.k10 = "ten"; x.k11 = "eleven";
		var y = objs[15]; delete y.k14;
		var z = objs[18]; delete z.k3; var had = "k3" in z; z.k3 = "three";
		out.push(x.k10, x.k11, "k12" in x, Object.keys(x).length, "k14" in y, "k13" in y, Object.keys(y).length,
			had, z.k3, Object.keys(z).slice(-2).join(), "k18" in z, Object.keys(all).length);
		out.join(" ")`))
	if err != nil {
		t.Fatal(err)
	}
	// Node v26.10.0's answer.
	want := "0 3 9 21 45 93 189 381 765 1533 3069 6141 12285 24573 49149 98301 196605 393213 786429 1572861 3145725 " +
		"ten eleven false 12 false true 14 false three k17,k3 false 20"
	if got := v.String().Go(); got != want {
		t.Errorf("got  %s, want %s", got, want)
	}
}
