package quickjs_test

import "testing"

// TestRegExpUnitReads covers the reads the matcher does in place: a code unit
// that is a character of its own, compared with a literal or tested against
// an ASCII class, and the surrogates it leaves to the general read, with and
// without the unicode flag, forwards and in a lookbehind.
func TestRegExpUnitReads(t *testing.T) {
	cases := []struct{ src, want string }{
		{`var s = "a\u{1F600}b\uD83Dc\uDE00d"
		  JSON.stringify([
		    s.match(/\u{1F600}/u).index, s.match(/\uD83D/).index, s.match(/\uD83D/u).index,
		    s.match(/\uDE00/).index, s.match(/\uDE00/u).index,
		    s.match(/./gu).length, s.match(/./g).length,
		    s.match(/b.c/u)?.[0].length, s.match(/[a-d]/g).join(""),
		    s.match(/(?<=\u{1F600})b/u).index, s.match(/(?<=\uDE00)b/).index,
		  ])`, "[1,1,4,2,6,7,8,3,\"abcd\",3,3]"},
		{`JSON.stringify([
		    "xAbCx".match(/abc/i).index, "xKx".match(/k/iu)?.index ?? null, "xKx".match(/k/i),
		    "a-b_c".replace(/[\w]/g, "."), "é1".match(/\w/).index, "ſ".match(/s/iu)?.index ?? null,
		    "aaab".match(/a+b/).index, "abcabd".match(/ab(?!c)/).index,
		  ])`, "[1,1,null,\".-...\",1,0,0,3]"},
	}
	for _, tc := range cases {
		checkEval(t, tc.src, tc.want)
	}
}

// TestRegExpSharedMatcher covers RegExps made from the same pattern, which
// borrow one matcher: one used inside another's replacement callback, one
// whose match needs a large matcher, and their lastIndex and compile, which
// stay each object's own.
func TestRegExpSharedMatcher(t *testing.T) {
	cases := []struct{ src, want string }{
		{`function mk() { return /(\w)(\d)?/g }
		  var outer = mk()
		  "a1b2c".replace(outer, (m, x) => x + "a9".replace(mk(), (n, y, d) => y + d + ",") + "|")`,
			"aa9,|ba9,|ca9,|"},
		{`function mk() { return /(a|b)*c/ }
		  var big = "ab".repeat(5000) + "c", r = []
		  r.push(mk().exec(big)[0].length, mk().exec("abc")[0], mk().exec("xbac")[1])
		  var keep = mk(); r.push(keep.exec(big)[1], keep.exec("aac")[1], mk().test("c"))
		  r.join()`, "10001,abc,a,b,a,true"},
		{`function mk() { return /x/g }
		  var a = mk(), b = mk(); a.exec("xxx"); a.exec("xxx")
		  var r = [a.lastIndex, b.lastIndex, b.exec("-x").index, a.lastIndex]
		  a.compile("y+", "g"); r.push(a.exec("xyy")[0], b.source, mk().source)
		  r.join()`, "2,0,1,2,yy,x,x"},
	}
	for _, tc := range cases {
		checkEval(t, tc.src, tc.want)
	}
}
