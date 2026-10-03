package quickjs_test

import "testing"

// TestConcatInteger covers a string joined to an integer with +, which writes
// the digits straight into the result where it is short. The result must be
// the string the number converts to, on either side, for any safe integer;
// anything else takes the ordinary path. An unpaired surrogate at the string's
// outer end must still pair with one joined to it later, which the result has
// to remember.
func TestConcatInteger(t *testing.T) {
	cases := []struct{ src, want string }{
		{`["ab" + 12345, 12345 + "ab", "x" + -42, -42 + "x", "z" + -0, "" + 7, 1024 + ""].join()`,
			"ab12345,12345ab,x-42,-42x,z0,7,1024"},
		{`["" + (2 ** 53 - 1), "" + -(2 ** 53 - 1), "" + 2 ** 53, "" + 1e21, "" + 0.5, "" + NaN, "" + -Infinity].join()`,
			"9007199254740991,-9007199254740991,9007199254740992,1e+21,0.5,NaN,-Infinity"},
		{`var long = "a".repeat(62); [(long + 5).length, (long + 123).length, (5 + long).slice(0, 3)].join()`, "63,65,5aa"},
		{`var s = "é"; [(s + 10).length, 10 + s, (s + 10) === "é10"].join()`, "3,10é,true"},
		{`[(1 + "\uD83D") + "\uDE00" === "1😀", "\uD83D" + ("\uDE00" + 1) === "😀1",
		   ("\uD83D" + 7 + "\uDE00").length, (1 + "\uD83D" + "\uDE00").codePointAt(1)].join()`,
			"true,true,3,128512"},
		{`var o = { valueOf() { return 3 } }; ["n" + o, o + "n", "x" + 1n, 2n + "y"].join()`, "n3,3n,x1,2y"},
	}
	for _, tc := range cases {
		checkEval(t, tc.src, tc.want)
	}
}
