package quickjs_test

import "testing"

// TestNumberAndCaseFastPaths covers the quick ways of two conversions: an
// integer below 2^53 turned into a string, and an ASCII string's case changed.
// Past their bounds -- a larger integer, a fraction, a string with a character
// outside ASCII -- the general way must still give the same answers.
func TestNumberAndCaseFastPaths(t *testing.T) {
	cases := []struct{ src, want string }{
		{`[0, -0, 7, -7, 42, 2**31, -(2**31) - 1, 2**53 - 1, -(2**53 - 1), 2**53, -(2**53), 2**53 + 2,
		   2**60, 1e20, 1e21, 123456789012345680000, 0.1, -1.5, 1e-7, 2**-1074]
		   .map(String).join()`,
			"0,0,7,-7,42,2147483648,-2147483649,9007199254740991,-9007199254740991,9007199254740992,-9007199254740992," +
				"9007199254740994,1152921504606847000,100000000000000000000,1e+21,123456789012345680000,0.1,-1.5,1e-7,5e-324"},
		{"`${2**53 - 1}|${-0}|${2**60}|${1/3}`", "9007199254740991|0|1152921504606847000|0.3333333333333333"},
		{`var s = "Hello, World 123!";
		  [s.toUpperCase(), s.toLowerCase(), "abc".toLowerCase() === "abc", "".toUpperCase(),
		   "@[\x60{".toUpperCase(), "@[\x60{".toLowerCase(), "straße".toUpperCase(), "ΑΣ".toLowerCase(),
		   "i̇".toUpperCase() === "İ"].join("|")`,
			"HELLO, WORLD 123!|hello, world 123!|true||@[`{|@[`{|STRASSE|ας|true"},
	}
	for _, tc := range cases {
		checkEval(t, tc.src, tc.want)
	}
}
