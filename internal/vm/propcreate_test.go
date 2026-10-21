package vm

import "testing"

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
