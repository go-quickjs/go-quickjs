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

// Two key strings that share a slot of keyAtoms, converted in turn, each
// find their atom remembered: the one a conversion moved out of the slot
// is kept in its second way (keyRemember) and found there (keyVictim),
// where each had evicted the other and been looked up anew every time.
// The strings are found by their addresses, which keySlot hashes.
func TestPropertyNameSharedSlot(t *testing.T) {
	r := New(Config{})
	defer r.Close()
	first := map[uint64]*String{}
	var a, b *String
	for i := 0; a == nil; i++ {
		s := NewString("key" + string(rune('a'+i%26)) + string(rune('a'+i/26%26)) + string(rune('a'+i/676%26)))
		if p, ok := first[keySlot(s)]; ok && p.Go() != s.Go() {
			a, b = p, s
		} else {
			first[keySlot(s)] = s
		}
	}
	atomA, atomB := r.atoms.intern(a.Go()), r.atoms.intern(b.Go())
	for i := 0; i < 4; i++ {
		for _, c := range []struct {
			s    *String
			atom Atom
		}{{a, atomA}, {b, atomB}} {
			pn, err := r.toPropertyName(Str(c.s))
			if err != nil || !pn.hasAtom || pn.atom != c.atom {
				t.Fatalf("round %d: %q converts to %+v, %v, want atom %v", i, c.s.Go(), pn, err, c.atom)
			}
			if i > 0 && r.keyAtoms[keySlot(c.s)].s != c.s {
				t.Fatalf("round %d: %q is not in its slot after its conversion", i, c.s.Go())
			}
		}
		if i > 0 {
			if v := r.keyVictims; v == nil || v[keySlot(a)].s != a {
				t.Fatalf("round %d: %q, evicted, is not in the slot's second way", i, a.Go())
			}
		}
	}
}
