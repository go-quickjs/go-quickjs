package quickjs_test

import "testing"

// TestRegExpCaseFoldingAgreesWithNode covers the i flag's comparison of a
// character, which is one of the characters listed for it rather than a
// fold of what is read: letters past ASCII with two and three forms and
// four (θ, ϑ, ϴ), the long s and the Kelvin sign, which only the u flag
// takes for s and k, the dotted and dotless i, which neither does, a
// character outside the Basic Multilingual Plane, a backreference, classes
// and a global search the first units of the pattern pass over text for.
// The answers are Node v26's.
func TestRegExpCaseFoldingAgreesWithNode(t *testing.T) {
	checkEval(t, `var K = String.fromCharCode(0x212A)
	JSON.stringify([
	  "Ärger ÜBER Öl".match(/über/iu)?.index, "Ärger ÜBER Öl".match(/über/i)?.index,
	  "ΣΊΣΥΦΟΣ".match(/σίσυφος/iu)?.[0], "ΣΊΣΥΦΟΣ".match(/σίσυφος/i)?.[0],
	  "ς".match(/Σ/i)?.[0], "ϑ".match(/θ/iu)?.[0], "ϴ".match(/θ/iu)?.[0], "ϴ".match(/θ/i)?.[0] ?? null,
	  K.match(/k/iu)?.index, K.match(/k/i), "ſ".match(/S/i), "ſ".match(/S/iu)?.index,
	  "ǅ".match(/ǆ/i)?.[0], "ẞ".match(/ß/iu)?.[0] ?? null, "ẞ".match(/ß/i)?.[0] ?? null,
	  "xxİ".match(/i/iu), "ı".match(/I/i), "ı".match(/I/iu),
	  "café CAFÉ".match(/É/gi), String.fromCodePoint(0x10400).match(new RegExp(String.fromCodePoint(0x10428), "iu"))?.index,
	  "aBc".replace(/b/gi, "-"), "xyzK".search(/k/i), ("xyz" + K).search(/k/i), ("xyz" + K).search(/k/iu),
	  /(é)\1/i.test("éÉ"), /(é)\1/iu.test("éÉ"), "ÀÉÎ".replace(/[à-ï]/gi, "."), "ÀÉÎ".replace(/[à-ï]/giu, "."),
	  "Straße STRASSE".match(/strasse/gi), "ﬀ FF".match(/ﬀ/giu), "µ Μ μ".match(/µ/gi), "µ Μ μ".match(/µ/giu),
	  "abcÅÅå".match(/å+/giu)?.[0], "İı".match(/[İ]/giu)?.[0],
	  "naïve café ΣΊΣΥΦΟΣ".match(/café|σίσυφος/giu), "ΣΊΣΥΦΟΣ naïve".replace(/ü|ï|σ/giu, "_"),
	])`, `[6,6,"ΣΊΣΥΦΟΣ","ΣΊΣΥΦΟΣ","ς","ϑ","ϴ",null,0,null,null,0,"ǅ","ẞ",null,null,null,null,["é","É"],0,"a-c",3,-1,3,true,true,"...","...",["STRASSE"],["ﬀ"],["µ","Μ","μ"],["µ","Μ","μ"],"ÅÅå","İ",["café","ΣΊΣΥΦΟΣ"],"_Ί_ΥΦΟ_ na_ve"]`)
}

// TestRegExpUnicodeRunsAgreeWithNode covers a greedy * or + of one character
// under the u and v flags, which runs over characters, a pair of
// surrogates one of them, and gives them back one by one: runs that end
// in a pair, that a lone half follows, that a pattern matching a half
// alone backs into, that begin between the halves of a pair, and runs with
// groups, splits and global searches. The answers are Node v26's.
func TestRegExpUnicodeRunsAgreeWithNode(t *testing.T) {
	checkEval(t, `JSON.stringify([
	  "a😀b😀c".match(/.*😀/u)[0], "a😀b😀c".match(/.+?😀/u)[0], "😀😀x".match(/\p{Emoji_Presentation}+/u)[0],
	  "a😀b".match(/.*\uDE00/u), "a😀b".match(/.*\uDE00/)[0].length, "日本語テキスト abc".match(/\p{L}+/gu),
	  "x\uD83Dy".match(/[^x]+/u)[0].length, "😀😀😀".replace(/.+(😀)/u, "[$1]"), "ab😀cd".split(/\P{L}*/u),
	  "a😀😀b".match(/(\S+)\S/u).slice(), /^.+$/u.test("😀\uD800"), "naïve café".match(/\w+/gu),
	  "😀a".search(/[^😀]*a/u), [..."x😀😀y".matchAll(/\S*?😀/gu)].map(m => m.index),
	  "😀😀".match(/[😀]*/v)[0].length, "ab😀😀".match(/\p{L}*[\u{1F600}]+$/v)[0],
	  (function () { var re = /\S+/uy; re.lastIndex = 2; var m = re.exec("😀😀 x"); return [m[0].length, re.lastIndex] })(),
	  (function () { var re = /.+/gu; re.lastIndex = 1; var m = re.exec("😀z"); return [m.index, m[0].length, re.lastIndex] })(),
	])`, `["a😀b😀","a😀","😀😀",null,3,["日本語テキスト","abc"],2,"[😀]",["a","b","c","d"],["a😀😀b","a😀😀"],true,["na","ve","caf"],2,[0,3],4,"ab😀😀",[2,4],[0,3,3]]`)
}

// TestRegExpNamedGroupsAgreeWithNode covers the groups object of a match,
// which every match of a pattern makes with the layout its first had: the
// names in the order written, not alphabetical, a group that took no part,
// names two alternatives share, more names than a table scans, the d
// flag's groups, an object of them changed -- a name added, deleted,
// redefined, frozen -- with the next one unchanged, and replace with $<name>
// and with a function. The answers are Node v26's.
func TestRegExpNamedGroupsAgreeWithNode(t *testing.T) {
	checkEval(t, `function d(s) { return /(?<year>\d{4})-(?<month>\d{2})(?:-(?<day>\d{2}))?/.exec(s) }
	d("0000-00-00")
	var a = d("2024-01-15"), b = d("1999-12"), r = []
	r.push(JSON.stringify(a.groups), JSON.stringify(b.groups), Object.keys(b.groups).join(),
	  "day" in b.groups, b.groups.day, Object.getPrototypeOf(a.groups))
	a.groups.extra = 1; delete a.groups.year
	Object.defineProperty(a.groups, "month", { enumerable: false })
	var c = d("2000-02-29")
	r.push(JSON.stringify(a.groups), Object.getOwnPropertyNames(a.groups).join(), JSON.stringify(c.groups))
	Object.freeze(c.groups); c.groups.year = "x"
	r.push(c.groups.year, JSON.stringify(d("1970-01-01").groups), Object.isFrozen(d("1970-01-01").groups),
	  JSON.stringify(Object.getOwnPropertyDescriptor(d("1970-01-01").groups, "month")))
	var z = /(?<z>z)|(?<a>a)(?<m>m)?/
	r.push(JSON.stringify(z.exec("am").groups), JSON.stringify(z.exec("z").groups), Object.keys(z.exec("a").groups).join())
	var dup = /(?<n>x)|(?<n>y)/
	r.push(dup.exec("y").groups.n, dup.exec("x").groups.n, Object.keys(dup.exec("x").groups).join())
	var many = /(?<g1>.)(?<g2>.)(?<g3>.)(?<g4>.)(?<g5>.)(?<g6>.)(?<g7>.)(?<g8>.)(?<g9>.)(?<g10>.)(?<g11>.)?/
	var m1 = many.exec("abcdefghij"), m2 = many.exec("0123456789!")
	m1.groups.g3 = "C"
	r.push(Object.values(m1.groups).join(""), Object.values(m2.groups).join(""), m2.groups.g10, m1.groups.g11)
	var di = /(?<w>\w+) (?<n>\d+)?/d.exec("ab cd")
	r.push(JSON.stringify(di.indices.groups), JSON.stringify(di.groups), Object.getPrototypeOf(di.indices.groups))
	r.push("2024-01-15 and 1999-12-31".replace(/(?<y>\d+)-(?<m>\d+)-(?<d>\d+)/g, "$<d>.$<m>.$<y>$<none>|"))
	r.push("k=v; a=b".replace(/(?<key>\w+)=(?<val>\w+)/g, function () {
	  var g = arguments[arguments.length - 1]; return g.val + ":" + g.key + ":" + Object.keys(g).length }))
	r.push([..."a1b2".matchAll(/(?<l>[a-z])(?<d>\d)/g)].map(m => m.groups.l + m.groups.d).join())
	r.join("|")`, `{"year":"2024","month":"01","day":"15"}|{"year":"1999","month":"12"}|year,month,day|true|||{"day":"15","extra":1}|month,day,extra|{"year":"2000","month":"02","day":"29"}|2000|{"year":"1970","month":"01","day":"01"}|false|{"value":"01","writable":true,"enumerable":true,"configurable":true}|{"a":"a","m":"m"}|{"z":"z"}|z,a,m|y|x|n|abCdefghij|0123456789!|9||{"w":[0,2]}|{"w":"ab"}||15.01.2024| and 31.12.1999||v:k:2; b:a:2|a1,b2`)
}

// TestRegExpReplaceTemplateAgreesWithNode covers a replacement template with
// $ forms on the built-in exec's matches, which is expanded from where the
// matches are in the subject: every form, two digits and one, groups there
// are not and groups that took no part, $< without named groups, a $ at the
// end, matches empty and not, global, sticky and neither, and subjects past
// ASCII whose groups split a pair of surrogates and join one again. The
// answers are Node v26's.
func TestRegExpReplaceTemplateAgreesWithNode(t *testing.T) {
	checkEval(t, `var r = [], s = "2024-01-15, 2025-12-31", bt = String.fromCharCode(96)
	r.push(s.replace(/(\d+)-(\d+)-(\d+)/g, "$3/$2/$1"), s.replace(/(\d+)-(\d+)-(\d+)/, "[$&|$" + bt + "|$'|$$|$]"))
	r.push(s.replace(/(\d+)-(\d+)/g, "$0;$00;$01;$02;$03;$10;$21;$9$"), "12345678901x".replace(/(\d)(\d)(\d)(\d)(\d)(\d)(\d)(\d)(\d)(\d)(\d)/g, "$11$10$1;$12;$1a"))
	r.push("abc".replace(/(x)?b/, "[$1]"), "abc".replace(/b/, "$<n>$<"), "abc".replace(/(?<n>b)/, "$<n>$<m>"))
	r.push("aaa".replace(/a*?/g, "<$&>"), "abc".replace(/x*/g, "-$&-"), "a😀b".replace(/(?:)/gu, "[$']"))
	r.push("a😀b".replace(/(?:)/g, ".").length, "😀😀".replace(/(\uD83D)(\uDE00)/g, "$2$1") === "\uDE00\uD83D\uDE00\uD83D")
	r.push("😀".replace(/(\uD83D)(\uDE00)/, "$1$2") === "😀", "x😀y".replace(/\uDE00/, "$" + bt) === "x\uD83Dx\uD83Dy")
	r.push("ü-é-ö".replace(/(\W)-(\W)/g, "$2+$1"), "日本-語".replace(/(.)-(.)/u, "$2$1$&"))
	var y = /(\w)/y; y.lastIndex = 1
	r.push("abc".replace(y, "<$1>"), y.lastIndex, "abc".replace(/(\w)/gy, "$1$1"), "a,b".replace(/(\w)/gy, "$1$1"))
	var g = /(o)/g; g.lastIndex = 5
	r.push("foo".replace(g, "[$1]"), g.lastIndex, "x".replace(/x/, "$$$"), "x".replace(/x/g, "$"))
	r.join("|")`, `15/01/2024, 31/12/2025|[2024-01-15||, 2025-12-31|$|$], 2025-12-31|$0;$00;2024;01;$03;20240;011;$9$-15, $0;$00;2025;12;$03;20250;121;$9$-31|101;12;1ax|a[]c|a$<n>$<c|abc|<>a<>a<>a<>|--a--b--c--|[a😀b]a[😀b]😀[b]b[]|9|true|true|true|é+ü-ö|日語本本-語|a<b>c|2|aabbcc|aa,b|f[o][o]|0|$$|$`)
}

// TestRegExpProtocolAgreesWithNode covers what the fast paths of RegExp's
// and String's methods must still see: an exec of a RegExp's own, the
// prototype's exec replaced, the flag getters and flags replaced -- which
// test does not read and replace, match and split do -- a subclass's exec,
// Symbol.replace replaced, lastIndex an object whose valueOf runs, a
// lastIndex that cannot be written, and a flag a RegExp has of its own. The
// answers are Node v26's.
func TestRegExpProtocolAgreesWithNode(t *testing.T) {
	checkEval(t, `var log = [], r = []
	var o = /b/; o.exec = function (s) { log.push("own exec " + s); return null }
	r.push(o.test("b"), "b".replace(o, "c"))
	var saved = RegExp.prototype.exec
	RegExp.prototype.exec = function (s) { log.push("proto exec " + this.source); return saved.call(this, s) }
	r.push(/c/.test("xc"), "xc".replace(/c/g, "$&$&"), "a-b".split(/-/).join(), "aa".match(/a/g).length, "ab".search(/b/))
	RegExp.prototype.exec = saved
	var gdesc = Object.getOwnPropertyDescriptor(RegExp.prototype, "global")
	Object.defineProperty(RegExp.prototype, "global", { get() { log.push("global"); return gdesc.get.call(this) }, configurable: true })
	r.push(/d/g.test("d"), "dd".replace(/d/g, "e"), "dd".match(/d/g).length, /d/g.exec("dd").index)
	Object.defineProperty(RegExp.prototype, "global", gdesc)
	var fdesc = Object.getOwnPropertyDescriptor(RegExp.prototype, "flags")
	Object.defineProperty(RegExp.prototype, "flags", { get() { log.push("flags"); return fdesc.get.call(this) }, configurable: true })
	r.push("ee".replace(/e/g, "f"), "e,e".split(/,/).length, /e/.test("e"), "e".replace(/(e)/, "$1$1"))
	Object.defineProperty(RegExp.prototype, "flags", fdesc)
	class R extends RegExp { exec(s) { log.push("sub exec"); return super.exec(s) } }
	r.push(new R("x").test("x"), "xx".replace(new R("x", "g"), "y"), "x-x".split(new R("-")).join(), "x1".replace(new R("(1)"), "<$1>"))
	var sr = RegExp.prototype[Symbol.replace]
	RegExp.prototype[Symbol.replace] = function (s, t) { log.push("sym replace"); return sr.call(this, s, t) }
	r.push("gg".replace(/g/g, "h"), "gg".replace(/(g)/g, "[$1]"))
	RegExp.prototype[Symbol.replace] = sr
	var li = /z/g; li.lastIndex = { valueOf() { log.push("valueOf"); return 1 } }
	r.push(li.test("zz"), li.lastIndex)
	var nw = /q/g; Object.defineProperty(nw, "lastIndex", { writable: false, value: 0 })
	try { nw.test("q") } catch (e) { r.push(e.constructor.name) }
	try { "qq".replace(nw, "$&") } catch (e) { r.push(e.constructor.name) }
	var nw2 = /q/; Object.defineProperty(nw2, "lastIndex", { writable: false, value: 0 })
	r.push(nw2.test("q"), "q".replace(nw2, "[$&]"))
	var sh = /s/g; Object.defineProperty(sh, "global", { value: false })
	r.push("ss".replace(sh, "t"), "ss".replace(sh, "<$&>"), sh.test("ss"), sh.lastIndex)
	r.push(log.join(";"))
	r.join("|")`, `false|b|true|xcc|a,b|2|1|true|ee|2|0|ff|2|true|ee|true|yy|x,x|x<1>|hh|[g][g]|true|2|TypeError|TypeError|true|[q]|ts|s<s>|false|0|own exec b;own exec b;proto exec c;proto exec c;proto exec c;proto exec -;proto exec -;proto exec -;proto exec a;proto exec a;proto exec a;proto exec b;global;global;flags;flags;flags;sub exec;sub exec;sub exec;sub exec;sub exec;sub exec;sub exec;sub exec;sym replace;sym replace;valueOf`)
}
