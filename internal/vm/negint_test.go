package vm

import "testing"

// A negative int32 literal an operator takes is an immediate (the
// compiler's fuse): x + -8 of a number, a string and an object, x - -8, a
// shift by -1, which shifts by 31, and x >>> -30, by 2, with x === -1 and
// x < -1 beside them; -0 stays -0. The answers are the interpreter's and
// the trees' alike, and Node's.
func TestNegativeIntLiteral(t *testing.T) {
	src := `
		function f(x) {
			var s = [x + -8, x - -8, x * -2, x >> -1, x >>> -30, (x + -8) >> 1, x === -1, x < -1];
			return s.join(',');
		}
		var out = [];
		for (var i = 0; i < 3; i++) out.push(f(i - 2), f('3'), f({ valueOf: function () { return -1 } }));
		out.push(1 / -0, -2147483648 | 0, -2147483649 | 0, -1.5 + -0.5, Object.is(-0, 0 * -1));
		out.join(' | ')`
	const want = "-10,6,4,-1,1073741823,-5,false,true | 3-8,11,-6,0,0,0,false,false | -9,7,2,-1,1073741823,-5,false,false | " +
		"-9,7,2,-1,1073741823,-5,true,false | 3-8,11,-6,0,0,0,false,false | -9,7,2,-1,1073741823,-5,false,false | " +
		"-8,8,0,0,0,-4,false,false | 3-8,11,-6,0,0,0,false,false | -9,7,2,-1,1073741823,-5,false,false | " +
		"-Infinity | -2147483648 | 2147483647 | -2 | true"
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
			t.Errorf("tree tier %v:\n got %s\nwant %s", tree, got, want)
		}
	}
}
