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
