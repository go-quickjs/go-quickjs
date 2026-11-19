package vm

import "testing"

// A string converted to a property key remembers its atom
// (toPropertyName, keyAtoms), so that a constant key -- "a" in o, delete
// o["a"] -- finds it with no lookup the next time; one that is no atom's
// yet is not remembered as none, for the name may become an atom after:
// here k, made at run time, is in nothing until a property of its name is
// made, then in o, then deleted from it, each answer as Node gives it.
func TestPropertyNameAtoms(t *testing.T) {
	t.Parallel()
	const src = `
		var o = { a: 1 }, r = []
		var k = ['zq', 'x', 'w'].join('')
		for (var i = 0; i < 3; i++) r.push('a' in o, k in o)
		o[k] = 1
		for (var i = 0; i < 3; i++) r.push(k in o, delete o[k], k in o, 'a' in o)
		r.push(delete o.a, 'a' in o)
		r.join()`
	const want = "true,false,true,false,true,false,true,true,false,true,false,true,false,true,false,true,false,true,true,false"
	r := New(Config{})
	defer r.Close()
	v, err := r.Run(compileForTest(t, src))
	if err != nil {
		t.Fatal(err)
	}
	if got := v.String().Go(); got != want {
		t.Errorf("got %s, want %s", got, want)
	}
}

// keyStringsInOneSet makes n strings of distinct names whose entries in
// keyAtoms share a set: the set is the strings' addresses', which keySet
// hashes, so that a test finds such strings rather than hoping for them.
func keyStringsInOneSet(n int, seed int) []*String {
	sets := map[uint64][]*String{}
	for i := 0; ; i++ {
		j := i + seed*100000
		s := NewString("key" + string(rune('a'+j%26)) + string(rune('a'+j/26%26)) + string(rune('a'+j/676%26)) + string(rune('a'+j/17576%26)))
		set := keySet(s)
		if sets[set] = append(sets[set], s); len(sets[set]) == n {
			return sets[set]
		}
	}
}

// Four key strings that share a set of keyAtoms, converted in turn, each
// keep their entry, wherever their addresses put them: after the first
// round none is looked up again, which the set's next way, moved on by
// each new entry (keyPut), shows unchanged. A fifth takes the oldest's
// way, and the oldest is then the one looked up. Direct-mapped, two keys
// that shared a slot had each evicted the other at every conversion, in
// some runs and not others.
func TestPropertyNameSharedSet(t *testing.T) {
	for seed := range 20 {
		r := New(Config{})
		keys := keyStringsInOneSet(keyWays+1, seed)
		atoms := make([]Atom, len(keys))
		for i, s := range keys {
			atoms[i] = r.atoms.intern(s.Go())
		}
		set := (*[keyWays]keyAtom)(r.keyAtoms[keySet(keys[0]):])
		convert := func(i int) {
			t.Helper()
			pn, err := r.toPropertyName(Str(keys[i]))
			if err != nil || !pn.hasAtom || pn.atom != atoms[i] {
				t.Fatalf("seed %d: %q converts to %+v, %v, want atom %v", seed, keys[i].Go(), pn, err, atoms[i])
			}
		}
		for i := range keyWays {
			convert(i)
		}
		next := set[0].next
		for range 100 {
			for i := range keyWays {
				convert(i)
				if r.keyFind(keys[i]) == nil {
					t.Fatalf("seed %d: %q has no entry after its conversion", seed, keys[i].Go())
				}
			}
		}
		if set[0].next != next {
			t.Fatalf("seed %d: four keys of one set were looked up again: next %d, was %d", seed, set[0].next, next)
		}
		convert(keyWays)
		if r.keyFind(keys[0]) != nil || r.keyFind(keys[keyWays]) == nil {
			t.Fatalf("seed %d: a fifth key did not take the oldest's way", seed)
		}
		for i := 1; i < keyWays; i++ {
			if r.keyFind(keys[i]) == nil {
				t.Fatalf("seed %d: a fifth key displaced %q, not the oldest", seed, keys[i].Go())
			}
		}
		r.Close()
	}
}

// A key string a define or a keyed write converts (toPropertyKey) is
// remembered with its atom as a read's is (stringKey), so that after the
// first evaluation no conversion of it asks the atom table: here the
// table's maps are emptied after a warming run, so that a lookup made
// after would find nothing and intern each name anew, a new atom each,
// which the table's next atom and the objects' keys would show. A key
// first converted by a read that found no atom for it (an entry saying
// none) is then defined, and the entry says its atom from then on.
func TestPropertyKeyAtoms(t *testing.T) {
	t.Parallel()
	const src = `
		var keys = ['kq', 'kr', 'ks', 'kt'].map(function (s) { return s + 'x' })
		var probe = ['zz', 'y'].join('')
		function run(n) {
			var out = []
			for (var i = 0; i < n; i++) {
				var k = keys[i & 3], o = {}
				o[k] = i
				var p = { [k]: i, v: 1 }
				class C { [k] = i }
				out.push(o, p, new C())
			}
			return out
		}
		function late() {
			var r = probe in {}
			var o = { [probe]: 1 }
			return r
		}`
	r := New(Config{})
	defer r.Close()
	if _, err := r.Run(compileForTest(t, src)); err != nil {
		t.Fatal(err)
	}
	call := func(name string, args ...Value) Value {
		t.Helper()
		fn, err := r.getValueProp(Obj(r.global), r.atoms.intern(name))
		if err != nil {
			t.Fatal(err)
		}
		v, err := r.Call(fn, Undefined, args)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	call("run", Int32(8))
	if v := call("late"); !v.IsBool() || v.BoolValue() {
		t.Fatalf("probe in {} is %v, want false", v)
	}
	// late's read remembered probe as naming no atom; its define made one,
	// which the entry now says.
	probe, err := r.getValueProp(Obj(r.global), r.atoms.intern("probe"))
	if err != nil || !probe.IsString() {
		t.Fatalf("probe is %v, %v", probe, err)
	}
	if e := r.keyFind(probe.String()); e == nil || e.none || e.atom != r.atoms.intern("zzy") {
		t.Fatalf("probe's entry after its define is %+v, want zzy's atom", e)
	}
	runFn, err := r.getValueProp(Obj(r.global), r.atoms.intern("run"))
	if err != nil {
		t.Fatal(err)
	}
	next := r.atoms.next()
	base, byName := r.atoms.base, r.atoms.byName
	if base != nil {
		r.atoms.base = &atomBase{entries: base.entries, byName: map[string]Atom{}}
	}
	r.atoms.byName = map[string]Atom{}
	out, err := r.Call(runFn, Undefined, []Value{Int32(400)})
	r.atoms.base, r.atoms.byName = base, byName
	if err != nil {
		t.Fatal(err)
	}
	if n := r.atoms.next() - next; n != 0 {
		t.Errorf("400 rounds of keyed writes and defines looked up %d names in the atom table, want none", n)
	}
	arr := out.Object().elemsOf()
	for i, v := range arr {
		o := v.Object()
		want := r.atoms.intern([]string{"kqx", "krx", "ksx", "ktx"}[i/3&3])
		if j := o.findOwn(want); j < 0 || o.vals()[j] != Int32(int32(i/3)) {
			t.Fatalf("object %d has no property %q of %d", i, r.atoms.name(want), i/3)
		}
	}
}
