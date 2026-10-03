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
