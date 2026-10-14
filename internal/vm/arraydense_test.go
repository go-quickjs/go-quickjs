package vm

import "testing"

// An array new Array(n) makes up to 2**16 long, and one whose length is
// at most doubled, keeps dense storage, holes and all: sparse, every
// element written to it after was a property (setArrayLength, the Array
// constructor). A longer one, or one lengthened past twice its elements,
// is sparse as before.
func TestArraysStayDense(t *testing.T) {
	r := New(Config{})
	t.Cleanup(func() { r.Close(); r.ReleaseClosed() })
	for _, c := range []struct {
		src    string
		sparse bool
	}{
		{`new Array(5000)`, false},
		{`Array(65536)`, false},
		{`new Array(65537)`, true},
		{`const a = []; for (let i = 0; i < 2000; i++) a.push(i); a.length = 4000; a`, false},
		{`const a = []; for (let i = 0; i < 2000; i++) a.push(i); a.length = 4001; a`, true},
		{`const a = [1, 2]; a.length = 1026; a`, false},
		{`const a = [1, 2]; a.length = 1027; a`, true},
		// A vector growing as AWFY's does: copied, doubled, filled.
		{`let s = Array.from({length: 1500}, (_, i) => i); for (let k = 0; k < 3; k++) { s = s.slice(); const n = s.length; s.length = 2 * n; for (let i = n; i < 2 * n; i++) s[i] = i } s`, false},
	} {
		v, err := r.Run(compileForTest(t, "{"+c.src+"}"))
		if err != nil {
			t.Fatalf("%s: %v", c.src, err)
		}
		o := v.Object()
		if sparse := o.flags&objHasSparseElements != 0; sparse != c.sparse {
			t.Errorf("%s: sparse %v, want %v (%d elements, length %d)", c.src, sparse, c.sparse, len(o.elemsOf()), o.arrayLength())
		}
	}
}
