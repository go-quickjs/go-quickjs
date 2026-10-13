package vm

import "testing"

// TestSpreadReservesRoom pins that an object spread into a literal gives it
// room for all its keys at once, out of the literal's inline room, rather
// than growing its table at every doubling; and that the keys are where a
// spread puts them, the literal's own before and after included.
func TestSpreadReservesRoom(t *testing.T) {
	defer SetTreeTier(true)
	for _, tree := range []bool{false, true} {
		SetTreeTier(tree)
		r := New(Config{})
		v, err := r.Run(compileForTest(t, `
		var src = {};
		for (var n = 0; n < 20; n++) src["k" + n] = n;
		var plain = {...src};
		var around = {x: 1, ...src, k3: "three", y: 2};
		[plain, around, JSON.stringify(around), Object.keys(plain).length]`))
		if err != nil {
			t.Fatal(err)
		}
		get := func(i int) Value {
			e, err := r.getIndexed(v, Int(i))
			if err != nil {
				t.Fatal(err)
			}
			return e
		}
		plain := get(0).Object()
		if cap(plain.vals) != 20 || plain.flags&objInlineProps != 0 {
			t.Errorf("tree %v: spread of 20 made room for %d (inline %v)", tree, cap(plain.vals), plain.flags&objInlineProps != 0)
		}
		const want = `{"x":1,"k0":0,"k1":1,"k2":2,"k3":"three","k4":4,"k5":5,"k6":6,"k7":7,"k8":8,"k9":9,` +
			`"k10":10,"k11":11,"k12":12,"k13":13,"k14":14,"k15":15,"k16":16,"k17":17,"k18":18,"k19":19,"y":2}`
		if got := get(2).String().Go(); got != want {
			t.Errorf("tree %v: got %s", tree, got)
		}
		if n := get(3); n.num != 20 {
			t.Errorf("tree %v: %v keys", tree, n.num)
		}
		r.Close()
	}
}
