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

// TestRegExpResultRoom covers exec's arrays, made with room for their
// elements and properties sized to the pattern's groups: none to five
// groups, named ones, the d flag's, each grown past its room by elements
// and properties, a property deleted and the length cut, as Node has them.
func TestRegExpResultRoom(t *testing.T) {
	src := `var out = []
	  var pats = [/b/, /(b)/, /(b)(c)?/, /(b)(c)?(d)/, /(b)(x)?(d)(e)/, /(b)(c)?(d)(e)(f)/, /(?<p>b)(c)?/, /(b)(c)?/d]
	  for (var p of pats) {
	    var m = p.exec("abdef")
	    var r = [m.length, JSON.stringify(m), Object.keys(m).join(), m.index, m.input, m.groups === undefined ? "u" : JSON.stringify(m.groups)]
	    m.push("z"); m[m.length + 1] = "y"; m.extra = 1; m.more = 2; delete m.index
	    r.push(m.length, JSON.stringify(m), Object.keys(m).join(), Object.getOwnPropertyNames(m).join())
	    m.length = 1; r.push(JSON.stringify(m), m.length)
	    out.push(r.join("|"))
	  }
	  out.join(";")`
	checkEval(t, src, `1|["b"]|0,index,input,groups|1|abdef|u|4|["b","z",null,"y"]|0,1,3,input,groups,extra,more|0,1,3,length,input,groups,extra,more|["b"]|1`+";"+
		`2|["b","b"]|0,1,index,input,groups|1|abdef|u|5|["b","b","z",null,"y"]|0,1,2,4,input,groups,extra,more|0,1,2,4,length,input,groups,extra,more|["b"]|1`+";"+
		`3|["b","b",null]|0,1,2,index,input,groups|1|abdef|u|6|["b","b",null,"z",null,"y"]|0,1,2,3,5,input,groups,extra,more|0,1,2,3,5,length,input,groups,extra,more|["b"]|1`+";"+
		`4|["bd","b",null,"d"]|0,1,2,3,index,input,groups|1|abdef|u|7|["bd","b",null,"d","z",null,"y"]|0,1,2,3,4,6,input,groups,extra,more|0,1,2,3,4,6,length,input,groups,extra,more|["bd"]|1`+";"+
		`5|["bde","b",null,"d","e"]|0,1,2,3,4,index,input,groups|1|abdef|u|8|["bde","b",null,"d","e","z",null,"y"]|0,1,2,3,4,5,7,input,groups,extra,more|0,1,2,3,4,5,7,length,input,groups,extra,more|["bde"]|1`+";"+
		`6|["bdef","b",null,"d","e","f"]|0,1,2,3,4,5,index,input,groups|1|abdef|u|9|["bdef","b",null,"d","e","f","z",null,"y"]|0,1,2,3,4,5,6,8,input,groups,extra,more|0,1,2,3,4,5,6,8,length,input,groups,extra,more|["bdef"]|1`+";"+
		`3|["b","b",null]|0,1,2,index,input,groups|1|abdef|{"p":"b"}|6|["b","b",null,"z",null,"y"]|0,1,2,3,5,input,groups,extra,more|0,1,2,3,5,length,input,groups,extra,more|["b"]|1`+";"+
		`3|["b","b",null]|0,1,2,index,input,groups,indices|1|abdef|u|6|["b","b",null,"z",null,"y"]|0,1,2,3,5,input,groups,indices,extra,more|0,1,2,3,5,length,input,groups,indices,extra,more|["b"]|1`)
}

// TestRegExpSourceAndLegacy covers what a RegExp's data now keeps of it:
// the source getter's answer, which the RegExps of one pattern share and
// compile replaces with its new pattern's, and the legacy features, which
// a subclass's instance lacks: RegExp.$1 after its match, and compile on
// it, throw TypeErrors (Node, which has no such features, throws neither).
func TestRegExpSourceAndLegacy(t *testing.T) {
	checkEval(t, `var r = []
	  function mk() { return /a\/b/g }
	  var a = mk(), b = mk()
	  r.push(a.source, a.source === b.source)
	  a.compile("x/y", "i"); r.push(a.source, b.source, mk().source, a.flags, new RegExp("").source, RegExp("\n").source)
	  class S extends RegExp {}
	  var s = new S("(q)")
	  s.exec("q")
	  try { RegExp.$1; r.push("no throw") } catch (e) { r.push(e.constructor.name) }
	  try { s.compile("z"); r.push("no throw") } catch (e) { r.push(e.constructor.name) }
	  /(w)/.exec("w"); r.push(RegExp.$1, s.source, new S(s).source)
	  var c = new RegExp(a); r.push(c.source, c.flags)
	  c.compile(/(k)/); r.push(c.source, c.exec("k")[1], RegExp.$1)
	  r.join()`, `a\/b,true,x\/y,a\/b,a\/b,i,(?:),\n,TypeError,TypeError,w,(q),(q),x\/y,i,(k),k,k`)
}
