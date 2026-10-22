package quickjs_test

import "testing"

// TestRegExpReplaceDirect covers s.replace(re, t) for strings s and t, which
// calls the built-in Symbol.replace itself where nothing it does could run a
// script, and builds an ASCII result without scanning it. A replace, flags
// or exec a script has put in place is still used; a lastIndex that cannot
// be written still throws; and what the result is, lastIndex after, and
// RegExp.$1 are as before.
func TestRegExpReplaceDirect(t *testing.T) {
	cases := []struct{ src, want string }{
		{`var s = "the quick brown fox", r = []
		  r.push(s.replace(/o/g, "0"), s.replace(/o/, "0"), s.replace(/x$/, ""), s.replace(/zz/g, "!"), s.replace(/o/g, "é"),
		    "été".replace(/t/g, "T"), "a😀b".replace(/😀/u, "-"), "😀x".replace(/x/g, "\uDE00").length, s.replace(/(q)u/, "$1U"),
		    s.replace(/o/g, ""), "aaa".replace(/a/y, "b"), "xa".replace(/a/y, "b"))
		  r.join("|")`, "the quick br0wn f0x|the quick br0wn fox|the quick brown fo|the quick brown fox|the quick bréwn féx|éTé|a-b|3|the qUick brown fox|the quick brwn fx|baa|xa"},
		{`var re = /(o)/g, r = []; "foo".replace(re, "0"); r.push(re.lastIndex, RegExp.$1)
		  var once = /(b)/; once.lastIndex = 5; "abc".replace(once, "B"); r.push(once.lastIndex, RegExp.$1)
		  r.join()`, "0,o,5,b"},
		{`var r = [], re = /o/g
		  var orig = RegExp.prototype[Symbol.replace]
		  RegExp.prototype[Symbol.replace] = function (s, t) { return "mine:" + s + t }
		  r.push("foo".replace(re, "0")); RegExp.prototype[Symbol.replace] = orig
		  var withExec = /o/g; withExec.exec = function () { r.push("exec"); return null }; r.push("foo".replace(withExec, "0"))
		  var frozen = Object.freeze(/o/g); try { "foo".replace(frozen, "0") } catch (e) { r.push(e.name) }
		  r.push("2020-01".replace(/(?<y>\d+)-(?<m>\d+)/, "$<m>/$<y>"))
		  r.join()`, "mine:foo0,exec,foo,TypeError,01/2020"},
	}
	for _, tc := range cases {
		checkEval(t, tc.src, tc.want)
	}
}

// TestRegExpMatchesRoom covers the room a global replace or match keeps its
// matches' indices in, which the runtime lends to one at a time: more
// matches of more groups than it starts with, a replacement function that
// replaces and matches globally itself while its caller's room is lent, a
// replace of thousands of matches, which grows the room past what the
// runtime keeps, and those after it; and split's pieces, gathered in room of
// their own, past it. Each answer is Node's.
func TestRegExpMatchesRoom(t *testing.T) {
	checkEval(t, `var r = []
	  r.push("2024-01-15, 2025-12-31 and 1999-07-04 or 1970-01-01, 2000-02-29".replace(/(\d+)-(\d+)-(\d+)/g, "$3/$2/$1"))
	  r.push("a1b2c3".replace(/(\w)(\d)/g, function (m, x, d) { return "[" + "..x9..y8z7w6".replace(/(\w)(\d)/g, "$2$1") + "".concat("q1q2".match(/q\d/g)) + x + d + "]" }))
	  var big = "ab".repeat(3000).replace(/(a)(b)/g, "$2$1"); r.push(big.length, big.slice(0, 6), big.slice(-6))
	  r.push("x1y2".replace(/(\w)(\d)/g, "$2$1"))
	  var m = "k1 k22 k333 k4444 k5 k6 k7 k8 k9 k10 k11 k12 k13 k14 k15 k16 k17 k18 k19 k20".match(/k(\d)(\d*)/g); r.push(m.length, m.join(), m.length === m.slice().length)
	  r.push("p,q ,r, s ,t,u,v,w,x,y,z,aa,bb,cc,dd,ee,ff,gg,hh,ii,jj".split(/\s*(,)\s*/).join("|"))
	  r.push("p,q".split(/(,)/, 2).join("|"), "".split(/,/).length, "abc".split(/(?:)/).join("|"))
	  r.join(";")`, "15/01/2024, 31/12/2025 and 04/07/1999 or 01/01/1970, 29/02/2000;"+
		"[..9x..8y7z6wq1,q2a1][..9x..8y7z6wq1,q2b2][..9x..8y7z6wq1,q2c3];6000;bababa;bababa;1x2y;20;"+
		"k1,k22,k333,k4444,k5,k6,k7,k8,k9,k10,k11,k12,k13,k14,k15,k16,k17,k18,k19,k20;true;"+
		"p|,|q|,|r|,|s|,|t|,|u|,|v|,|w|,|x|,|y|,|z|,|aa|,|bb|,|cc|,|dd|,|ee|,|ff|,|gg|,|hh|,|ii|,|jj;p|,;1;a|b|c")
}
