package vm

import (
	"math/rand"
	"testing"
)

// An index's slot table (slotTable) answers as a map of the same keys does,
// through additions, updates, deletions, additions again of deleted keys
// and the growth they cause; and a key read as an object's property is
// found, deleted and found again by the index.
func TestSlotTable(t *testing.T) {
	var s slotTable
	s.init(2)
	want := map[Atom]int32{}
	rnd := rand.New(rand.NewSource(1))
	for step := 0; step < 20000; step++ {
		k := Atom(rnd.Intn(600))
		if k&7 == 0 {
			k |= atomIndexTag
		}
		switch rnd.Intn(3) {
		case 0, 1:
			v := int32(rnd.Intn(1000))
			s.set(k, v)
			want[k] = v
		case 2:
			s.del(k)
			delete(want, k)
		}
		if s.n != len(want) {
			t.Fatalf("step %d: %d keys, want %d", step, s.n, len(want))
		}
	}
	for k := Atom(0); k < 600; k++ {
		for _, k := range []Atom{k, k | atomIndexTag} {
			v, ok := s.get(k)
			w, wok := want[k]
			if ok != wok || ok && v != w {
				t.Fatalf("key %d: %d %v, want %d %v", k, v, ok, w, wok)
			}
		}
	}
	n := 0
	s.all(func(k Atom, v int32) {
		if want[k] != v {
			t.Fatalf("all gave %d for key %d, want %d", v, k, want[k])
		}
		n++
	})
	if n != len(want) {
		t.Fatalf("all gave %d keys, want %d", n, len(want))
	}

	src := `
		var o = {};
		for (var i = 0; i < 200; i++) o["k" + i] = i;
		var out = [o.k7, o["k150"], "k3" in o];
		delete o.k3;
		out.push("k3" in o, o.k3);
		o.k3 = "again";
		out.push(o.k3, Object.keys(o).length, Object.keys(o)[199]);
		out.join()`
	r := New(Config{})
	defer r.Close()
	v, err := r.Run(compileForTest(t, src))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := v.String().Go(), "7,150,true,false,,again,200,k3"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}
