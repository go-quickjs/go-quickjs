package vm

import "testing"

// A string converted to a property key remembers its atom
// (toPropertyName, keyAtoms), so that a constant key -- "a" in o, delete
// o["a"] -- finds it with no lookup the next time; one that is no atom's
// yet is not remembered as none, for the name may become an atom after:
// here k, made at run time, is in nothing until a property of its name is
// made, then in o, then deleted from it, each answer as Node gives it.
func TestPropertyNameAtoms(t *testing.T) {
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
