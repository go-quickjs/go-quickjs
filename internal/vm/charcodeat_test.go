package vm

import "testing"

// A call s.charCodeAt(i) is a node of its own (charCodeAtCall), which reads
// the code unit where the receiver is a string, the method the built-in and
// the position a number or undefined -- NaN, negative fractions, positions
// past either end, a lone surrogate -- and makes the call for anything else:
// a position of another type, whose conversion runs script, a String
// object, an object with a method of that name, and the built-in replaced
// on String.prototype, which the same site then calls. The answers are the
// interpreter's and the trees' alike, and Node's.
func TestCharCodeAtNode(t *testing.T) {
	src := `
		function at(s, i) { return s.charCodeAt(i); }
		function at0(s) { return s.charCodeAt(); }
		var out = [];
		for (var r = 0; r < 3; r++) {
			var s = "ab\ud800c";
			out.push(at(s, 0), at(s, 1.9), at(s, -0.5), at(s, NaN), at(s, 2), at(s, 3), at(s, 4), at(s, -1), at(s, Infinity), at0(s));
			out.push(at(s, "1"), at(s, { valueOf: function () { out.push("v"); return 3; } }), at(s, true), at(s, null));
			out.push(at(new String("xyz"), 1), at({ charCodeAt: function (i) { return "own" + i; } }, 7));
		}
		var saved = String.prototype.charCodeAt;
		String.prototype.charCodeAt = function (i) { return "patched" + i; };
		out.push(at("abc", 1));
		String.prototype.charCodeAt = saved;
		out.push(at("abc", 1));
		try { at(null, 0) } catch (e) { out.push(e instanceof TypeError) }
		out.join()`
	// Node's answer.
	const one = "97,98,97,97,55296,99,NaN,NaN,NaN,97,v,98,99,98,97,121,own7"
	const want = one + "," + one + "," + one + ",patched1,98,true"
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
