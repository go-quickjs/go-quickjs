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

// TestRegExpPiecesTogether covers the Strings of a match's groups and of a
// global match's matches, cut into one allocation where two or more need
// one of their own: among groups that matched nothing, did not take part or
// are the whole subject, which need none, over ASCII and over text past it,
// pairs of surrogates and halves of them; and a piece kept after its array
// is dropped. Each answer is Node's.
func TestRegExpPiecesTogether(t *testing.T) {
	checkEval(t, `var r = []
	  function show(m) { return m === null ? "null" : JSON.stringify(m) + ":" + m.map(function (x) { return x === undefined ? "u" : x.length }).join(",") }
	  r.push(show(/(a)(b)(c)/.exec("abc")), show(/((a)(b))(x)?()(c)/.exec("zabcz")), show(/(.*)(.*)/.exec("whole")))
	  r.push(show(/(é)(\u{1F600})(.)(b)/u.exec("aé\u{1F600}xb")), show(/(\uD83D)(\uDE00)/.exec("\u{1F600}")), show(/(?<y>\d+)-(?<m>\d+)/.exec("on 2024-05!")))
	  r.push(show("a1b22c333".match(/\d+/g)), show("abc".match(/x*/g)), show("whole".match(/whole/g)), show("é\u{1F600}é".match(/./gu)), show("\u{1F600}\u{1F600}".match(/\uD83D/g)))
	  var m = /(\w+) (\w+)/.exec("hello world"); var keep = m[2]; m = null; r.push(keep, keep.length)
	  r.join(";")`, `["abc","a","b","c"]:3,1,1,1;["abc","ab","a","b",null,"","c"]:3,2,1,1,u,0,1;["whole","whole",""]:5,5,0;`+
		`["é😀xb","é","😀","x","b"]:5,1,2,1,1;["😀","\ud83d","\ude00"]:2,1,1;["2024-05","2024","05"]:7,4,2;`+
		`["1","22","333"]:1,2,3;["","","",""]:0,0,0,0;["whole"]:5;["é","😀","é"]:1,2,1;["\ud83d","\ud83d"]:1,1;world;5`)
}

// TestRegExpLiteralReuse covers a RegExp literal's RegExp given back to it
// after a call of the built-in exec or test, as its next evaluation: each
// evaluation still a RegExp of its own where a script could tell, and the
// one given back as new -- lastIndex 0, under g and y -- where it cannot:
// literals not called, an exec of the script's, an exec swapped in while
// the argument is evaluated, a getter for test, an argument whose
// conversion swaps exec, the legacy statics, primitives of each kind as the
// argument, a generator's literal. Each function runs four times, in a
// tree after the first. Each answer is Node's.
func TestRegExpLiteralReuse(t *testing.T) {
	checkEval(t, `var out = [];
function f1(s) { var r = []; for (var i = 0; i < 4; i++) r.push(/a/g.exec(s).index + "/" + /a/y.test(s)); return r.join() }
out.push(f1("aaa"));
function f2() { var a = []; for (var i = 0; i < 3; i++) a.push(/x/); return [a[0] !== a[1], a[1] !== a[2], a[0].lastIndex].join() }
out.push(f2());
function f3() { var seen = [], orig = RegExp.prototype.exec;
  RegExp.prototype.exec = function (s) { seen.push(this); return orig.call(this, s) };
  for (var i = 0; i < 3; i++) { /q/.test("q"); /q/.exec("q") }
  RegExp.prototype.exec = orig;
  return [seen.length, new Set(seen).size, seen.every(function (r) { return r.lastIndex === 0 && r.source === "q" })].join() }
out.push(f3());
function f4() { var leaked = [], orig = RegExp.prototype.exec;
  var leaker = function (s) { leaked.push(this); RegExp.prototype.exec = orig; return orig.call(this, s) };
  var r = [];
  for (var i = 0; i < 3; i++) r.push(/z/g.test((RegExp.prototype.exec = leaker, "zz")));
  var after = []; for (var i = 0; i < 3; i++) { var m = /z/g; after.push(m); m.test("z") }
  return [r.join(""), leaked.length, new Set(leaked).size, leaked.some(function (x) { return after.indexOf(x) >= 0 }), leaked.map(function (x) { return x.lastIndex }).join("")].join() }
out.push(f4());
function f5() { var leaked = [], orig = Object.getOwnPropertyDescriptor(RegExp.prototype, "test");
  Object.defineProperty(RegExp.prototype, "test", { get() { leaked.push(this); return orig.value }, configurable: true });
  for (var i = 0; i < 3; i++) /w/.test("w");
  Object.defineProperty(RegExp.prototype, "test", orig);
  return [leaked.length, new Set(leaked).size].join() }
out.push(f5());
function f6() { var r = [];
  for (var i = 0; i < 3; i++) { /(\d)(\d)?/.test("a" + i); r.push(RegExp.$1 + RegExp.lastMatch + RegExp.input) }
  var o = { toString() { return "k7" } };
  for (var i = 0; i < 2; i++) r.push(/k(\d)/.exec(o)[1]);
  try { /x/.exec(Symbol()) } catch (e) { r.push(e.constructor.name) }
  r.push(/x/.test(), /undefined/.test(), /^1$/.exec(1)[0], /null/.exec(null).index);
  return r.join() }
out.push(f6());
function f7() { var seen = [], orig = RegExp.prototype.exec;
  var o = { toString() { RegExp.prototype.exec = function (s) { seen.push(this); RegExp.prototype.exec = orig; return orig.call(this, s) }; return "t" } };
  for (var i = 0; i < 3; i++) /t/.test(o);
  return [seen.length, new Set(seen).size].join() }
out.push(f7());
function f8(s) { var c = 0; for (var i = 0; i < 50; i++) { var m = /(b)(c)?/g.exec(s); c += m.index + m.length + m[1].length } return c }
out.push(f8("abc"));
function f9() { function* g() { for (var i = 0; i < 3; i++) yield /y/g.exec("yy").index } return [...g()].join() }
out.push(f9());
var once = out.join(";"); out = []; for (var k = 0; k < 3; k++) { out.push(f1("aaa"), f2(), f3(), f4(), f5(), f6(), f7(), f8("abc"), f9()); if (out.join(";") !== once) throw new Error("run " + k + ": " + out.join(";")); out = [] }
once`, "0/true,0/true,0/true,0/true;true,true,0;6,6,true;truetruetrue,3,3,false,111;3,3;00a0,11a1,22a2,7,7,TypeError,false,true,1,0;3,3;250;0,0,0")
}

// TestRegExpLiteralArgReuse covers a RegExp literal's RegExp given back to
// it after a string's replace by a string, split or match, which keep
// nothing of it: each evaluation still a RegExp of its own where a script
// could tell -- its Symbol.replace, exec, species, Symbol.split,
// Symbol.match or flags getter a script's -- and the one given back as new
// where it cannot, under g and y; a replace by a function, a String
// object, a limit that converts, the legacy statics; a RegExp stored as it
// is passed, or called. Each function runs
// four times, in a tree after the first. Each answer is Node's.
func TestRegExpLiteralArgReuse(t *testing.T) {
	checkEval(t, `function g1(s) { var r = []; for (var i = 0; i < 4; i++) r.push(s.replace(/a(b)?/g, "[$1]"), s.split(/b/).length, s.match(/a/g).length, s.match(/(a)(b)/).index, s.replace(/a/y, "Y")); return r.join() }
function g2() { var leaked = [], orig = RegExp.prototype[Symbol.replace];
  RegExp.prototype[Symbol.replace] = function (s, t) { leaked.push(this); return orig.call(this, s, t) };
  for (var i = 0; i < 3; i++) "abc".replace(/b/g, "x");
  RegExp.prototype[Symbol.replace] = orig;
  return [leaked.length, new Set(leaked).size, leaked.every(function (r) { return r.lastIndex === 0 })].join() }
function g3() { var leaked = [], orig = RegExp.prototype.exec;
  RegExp.prototype.exec = function (s) { leaked.push(this); return orig.call(this, s) };
  for (var i = 0; i < 3; i++) { "abc".match(/b/); "abc".match(/b/g); "abc".replace(/b/, "x"); "a,b".split(/,/) }
  RegExp.prototype.exec = orig;
  return [leaked.length, new Set(leaked).size].join() }
function g4() { var leaked = [], desc = Object.getOwnPropertyDescriptor(RegExp, Symbol.species);
  Object.defineProperty(RegExp, Symbol.species, { get() { var C = function (re, f) { leaked.push(re); return new RegExp(re, f) }; return C }, configurable: true });
  for (var i = 0; i < 3; i++) "a,b".split(/,/);
  Object.defineProperty(RegExp, Symbol.species, desc);
  return [leaked.length, new Set(leaked).size].join() }
function g5() { var leaked = [], orig = RegExp.prototype[Symbol.split];
  RegExp.prototype[Symbol.split] = function (s, l) { leaked.push(this); return orig.call(this, s, l) };
  for (var i = 0; i < 3; i++) "a,b".split(/,/);
  RegExp.prototype[Symbol.split] = orig;
  var origM = RegExp.prototype[Symbol.match];
  RegExp.prototype[Symbol.match] = function (s) { leaked.push(this); return origM.call(this, s) };
  for (var i = 0; i < 3; i++) "ab".match(/b/);
  RegExp.prototype[Symbol.match] = origM;
  return [leaked.length, new Set(leaked).size].join() }
function g6() { var r = [], seen = [];
  for (var i = 0; i < 3; i++) r.push("aXbXc".replace(/x/gi, function (m, off, s) { seen.push(typeof m); return "" + off }));
  var so = new String("a-b"); for (var i = 0; i < 2; i++) r.push(so.split(/-/).join("+"), so.replace(/-/, "="), so.match(/-/).index);
  var lim = { valueOf() { return 1 } }; for (var i = 0; i < 2; i++) r.push("a-b-c".split(/-/, lim).join(), "a-b-c".split(/-/, 2).join(), "".split(/-/).length);
  for (var i = 0; i < 2; i++) { "q1q2".replace(/q(\d)/g, "$1"); r.push(RegExp.$1, RegExp.lastMatch) }
  var t = { toString() { return "T" } }; for (var i = 0; i < 2; i++) r.push("aTb".replace(/T/, t), "aTb".replace(/t/i, "$&$&"));
  return r.join() + "|" + seen.length }
function g7() { var leaked = [], orig = Object.getOwnPropertyDescriptor(RegExp.prototype, "flags");
  Object.defineProperty(RegExp.prototype, "flags", { get() { leaked.push(this); return orig.get.call(this) }, configurable: true });
  for (var i = 0; i < 3; i++) { "a-b".replace(/-/g, "+"); "a-b".split(/-/); "a-b".match(/-/g) }
  Object.defineProperty(RegExp.prototype, "flags", orig);
  return [leaked.length, new Set(leaked).size].join() }
function h1() { var keep = [], keep2 = [], keep3 = [], o; for (var i = 0; i < 4; i++) { "ab".replace((keep[i] = /b/g), "x"); (keep2[i] = /c/y).test("c"); "a,b".split(o = /,/); keep3.push(o); (o = /d/).test("d"); keep3.push(o) } return [new Set(keep).size, new Set(keep2).size, new Set(keep3).size, keep2.map(function (r) { return r.lastIndex }).join("")].join() }
var once = [g1("abab"), g2(), g3(), g4(), g5(), g6(), g7(), h1()].join(";");
for (var k = 0; k < 3; k++) { var again = [g1("abab"), g2(), g3(), g4(), g5(), g6(), g7(), h1()].join(";"); if (again !== once) throw new Error("run " + k + ": " + again) }
once`, "[b][b],3,2,0,Ybab,[b][b],3,2,0,Ybab,[b][b],3,2,0,Ybab,[b][b],3,2,0,Ybab;3,3,true;21,12;3,3;6,6;a1b3c,a1b3c,a1b3c,a+b,a=b,1,a+b,a=b,1,a,a,b,1,a,a,b,1,2,q2,2,q2,aTb,aTTb,aTb,aTTb|6;9,9;4,4,8,1111")
}
