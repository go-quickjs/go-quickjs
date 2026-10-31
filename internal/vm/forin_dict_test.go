package vm

import "testing"

// A for-in over a plain object of no layout objects share -- a dictionary,
// as one is once a key is deleted -- takes its keys from its table, in
// order (startForIn's tableKeys): enumerable string keys only, none
// deleted, a symbol or a non-enumerable key left out; and an object with an
// index among its keys, which comes first in order of its value, a large
// one spelled as a string included, is listed the long way. A key deleted
// during the loop is not visited. The answers are the spec's order, which
// Node gives.
func TestForInDictionary(t *testing.T) {
	src := `
		function keys(o) { var s = []; for (var k in o) s.push(k); return s.join(',') }
		var d = { z: 0, a: 1, b: 2 }; delete d.z;
		var out = [keys(d)];
		d[Symbol('s')] = 1; Object.defineProperty(d, 'h', { value: 1, enumerable: false }); d.c = 3;
		out.push(keys(d));
		var e = { x: 1 }; delete e.x; e['4294967294'] = 1; e.y = 2; e['7'] = 3;
		out.push(keys(e));
		var f = { q: 1, w: 2 }; delete f.q; f.e = 3;
		out.push((function () { var s = []; for (var k in f) { s.push(k); if (k === 'w') delete f.e } return s.join(',') })());
		var g = Object.create({ inherited: 1 }); g.own = 1; delete g.own; g.mine = 2;
		out.push(keys(g));
		for (var i = 0; i < 3; i++) out.push(keys(d));
		out.join(' | ')`
	const want = "a,b | a,b,c | 7,4294967294,y | w | mine,inherited | a,b,c | a,b,c | a,b,c"
	defer SetTreeTier(true)
	for _, tree := range []bool{false, true} {
		SetTreeTier(tree)
		r := New(Config{})
		v, err := r.Run(compileForTest(t, src))
		r.Close()
		if err != nil {
			t.Fatal(err)
		}
		if got := v.String().Go(); got != want {
			t.Errorf("tree tier %v: got %q, want %q", tree, got, want)
		}
	}
}
