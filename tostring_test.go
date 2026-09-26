package quickjs_test

import (
	"testing"

	quickjs "github.com/go-quickjs/go-quickjs"
)

// Function.prototype.toString returns the source text of the function, which
// for a method is the whole method definition -- the name, and any async, star
// or accessor keyword -- and not merely the parameters and body.
func TestFunctionToString(t *testing.T) {
	cases := []struct{ src, want string }{
		{`function f(a, b) {} f.toString()`, "function f(a, b) {}"},
		{`(function () {}).toString()`, "function () {}"},
		{`(() => 1).toString()`, "() => 1"},
		{`(async () => 1).toString()`, "async () => 1"},
		{`(function* () {}).toString()`, "function* () {}"},

		{`class C { m() {} } C.prototype.m.toString()`, "m() {}"},
		// static belongs to the class element rather than to the method.
		{`class C { static m() {} } C.m.toString()`, "m() {}"},
		{`class C { async *m() {} } C.prototype.m.toString()`, "async *m() {}"},
		{`class C { get x() {} }
		  Object.getOwnPropertyDescriptor(C.prototype, "x").get.toString()`, "get x() {}"},
		{`class C { set x(v) {} }
		  Object.getOwnPropertyDescriptor(C.prototype, "x").set.toString()`, "set x(v) {}"},

		{`({m() { return 1 }}).m.toString()`, "m() { return 1 }"},
		{`({*g() {}}).g.toString()`, "*g() {}"},
		{`({["a"]() {}}).a.toString()`, `["a"]() {}`},
		{`Object.getOwnPropertyDescriptor({get x() {}}, "x").get.toString()`, "get x() {}"},

		{`class C {} C.toString()`, "class C {}"},
		{`class C extends Array {} C.toString()`, "class C extends Array {}"},

		// A built-in has no source, and says so in the one form the language
		// requires to parse back as a function.
		{`Math.max.toString()`, "function max() { [native code] }"},
	}

	for _, tc := range cases {
		rt := quickjs.New()
		v, err := rt.Eval(tc.src)
		if err != nil {
			t.Errorf("%s: %v", tc.src, err)
		} else if got := v.String(); got != tc.want {
			t.Errorf("%s = %q, want %q", tc.src, got, tc.want)
		}
		rt.Close()
	}

	for _, src := range []string{
		`Function.prototype.toString.call({})`,
		`Function.prototype.toString.call(1)`,
		`Function.prototype.toString.call(null)`,
	} {
		rt := quickjs.New()
		if _, err := rt.Eval(src); err == nil {
			t.Errorf("%s: accepted, want TypeError", src)
		}
		rt.Close()
	}
}

// Only an object is asked for the symbol method that would stand in for a
// pattern: a primitive would find one on its own prototype, which is not
// something the caller supplied.
func TestStringPatternOnlyAsksObjects(t *testing.T) {
	cases := []struct{ src, want string }{
		{`Object.defineProperty(String.prototype, Symbol.match,
		    {get: function () { throw new Error("asked") }});
		  "a,b,c".match(",").join("|")`, ","},
		{`Object.defineProperty(String.prototype, Symbol.split,
		    {get: function () { throw new Error("asked") }});
		  "a,b".split(",").join("|")`, "a|b"},
		{`Object.defineProperty(String.prototype, Symbol.replace,
		    {get: function () { throw new Error("asked") }});
		  "abc".replace("b", "X")`, "aXc"},
		// An object is still asked.
		{`var o = {}; o[Symbol.search] = function () { return 42 };
		  String("x".search(o))`, "42"},

		// A string pattern's replacement may name the match and the text
		// around it, exactly as a regular expression's may.
		{`"abc".replace("b", "[$&]")`, "a[b]c"},
		{"\"abc\".replace(\"b\", \"$$\")", "a$c"},
		{"\"abc\".replace(\"b\", \"$`\")", "aac"},
		{`"abc".replace("b", "$'")`, "acc"},
		{`"a-b".replaceAll("-", "$&$&")`, "a--b"},

		// A spread call through super or a private name.
		{`class A { m() { return arguments.length } }
		  class B extends A { m(...a) { return super.m(...a) } }
		  String(new B().m(1, 2))`, "2"},
		{`class C { #m(...a) { return a.join(",") } run() { return this.#m(...[1, 2]) } }
		  new C().run()`, "1,2"},
	}
	for _, tc := range cases {
		rt := quickjs.New()
		v, err := rt.Eval(tc.src)
		if err != nil {
			t.Errorf("%s: %v", tc.src, err)
		} else if got := v.String(); got != tc.want {
			t.Errorf("%s = %q, want %q", tc.src, got, tc.want)
		}
		rt.Close()
	}
}

// The positions a string replacement works with are code-unit indices, not
// byte offsets into the encoded form: `$'` and "$`" name the text around the
// match as a script counts it.
func TestStringReplaceCountsCodeUnits(t *testing.T) {
	cases := []struct{ name, src, want string }{
		{"following text", `"áXbXc".replaceAll("X", "[$']")`, "á[bXc]b[c]c"},
		{"preceding text", "\"áXbXc\".replaceAll(\"X\", \"[$`]\")", "á[á]b[áXb]c"},
		{"wide characters", "\"日X本\".replaceAll(\"X\", \"[$`]\")", "日[日]本"},
		{"the match itself", `"áXbXc".replaceAll("X", "[$&]")`, "á[X]b[X]c"},
		{"a function's position", `var at = []
		  "áXbXc".replaceAll("X", function (m, i) { at.push(i); return "-" })
		  at.join(",")`, "1,3"},
		{"replace agrees", `"áXbXc".replace(/X/g, "[$']")`, "á[bXc]b[c]c"},
		// A surrogate pair is two units, and splitting around it keeps both.
		{"astral text", `"a\u{1F600}b".replaceAll("b", "[$` + "`" + `]").length`, "8"},
		{"lone surrogate", `"a\uD800b".replaceAll("b", "[$` + "`" + `]") === "a\uD800[a\uD800]"`,
			"true"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) { checkEval(t, tc.src, tc.want) })
	}
}

// A string given to a method that wants a pattern is a pattern, not text to
// find: only replace and split with a string argument search for a literal.
func TestStringPatternArgumentsArePatterns(t *testing.T) {
	cases := []struct{ src, want string }{
		{`String("ab3c".search("\\d"))`, "2"},
		{`String("a.c".search("."))`, "0"},
		{`String("a1b2".match("\\d"))`, "1"},
		{`String([..."a1b2".matchAll("\\d")].length)`, "2"},
		// A null symbol method means there is none, so the fallback applies.
		{`var re = {}; re[Symbol.search] = null; re.toString = function () { return "\\d" }
		  String("ab3c".search(re))`, "2"},
		{`var re = {}; re[Symbol.match] = null; re.toString = function () { return "\\d" }
		  String("ab3c".match(re))`, "3"},
		// These two take their argument literally.
		{`"a.c".replace(".", "X")`, "aXc"},
		{`String("a.c".split("."))`, "a,c"},
	}
	for _, tc := range cases {
		checkEval(t, tc.src, tc.want)
	}
}

// A replacement that is not a function is converted once, before the search:
// a toString that counts its calls sees exactly one, however many matches there
// turn out to be -- including none at all.
func TestStringReplaceConvertsTheReplacementOnce(t *testing.T) {
	cases := []struct{ name, src, want string }{
		{"no match", `var n = 0
		  var r = {toString: function () { n++; return "b" }}
		  "".replace("a", r) + "," + n`, ",1"},
		{"two matches", `var n = 0
		  var r = {toString: function () { n++; return "b" }}
		  "aa".replaceAll("a", r) + "," + n`, "bb,1"},
		{"toPrimitive once", `var n = 0
		  var r = {}
		  r[Symbol.toPrimitive] = function () { n++; return "b" }
		  "aa".replaceAll("a", r) + "," + n`, "bb,1"},
		{"undefined", `"aa".replaceAll("a", undefined)`, "undefinedundefined"},
		{"null", `"aa".replaceAll("a", null)`, "nullnull"},
		// A function is still called once per match.
		{"a function per match", `var n = 0
		  "aa".replaceAll("a", function () { n++; return "b" }) + "," + n`, "bb,2"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) { checkEval(t, tc.src, tc.want) })
	}
}

// Case conversion uses Unicode's full mappings: a character whose other case
// is more than one character expands, and a sigma that ends a word lowercases
// differently from one that does not.
func TestFullCaseMappings(t *testing.T) {
	cases := []struct{ name, src, want string }{
		{"sharp s", `"ß".toUpperCase()`, "SS"},
		{"ligature", `"ﬁ".toUpperCase()`, "FI"},
		{"dotted capital i", `String("İ".toLowerCase() === "i̇")`, "true"},
		{"apostrophe n", `String("ŉ".toUpperCase() === "ʼN")`, "true"},
		{"armenian ligature", `String("ﬓ".toUpperCase() === "ՄՆ")`, "true"},
		// A final sigma is the one mapping that depends on its surroundings.
		{"final sigma", `String("AΣ".toLowerCase() === "aς")`, "true"},
		{"sigma at the start", `String("ΣA".toLowerCase() === "σa")`, "true"},
		{"sigma inside", `String("AΣB".toLowerCase() === "aσb")`, "true"},
		{"sigma alone", `String("Σ".toLowerCase() === "σ")`, "true"},
		{"ignorable before", `String("A­Σ".toLowerCase() === "a­ς")`, "true"},
		{"ignorable after", `String("AΣ­".toLowerCase() === "aς­")`, "true"},
		{"cased after an ignorable", `String("AΣ­B".toLowerCase() === "aσ­b")`,
			"true"},
		// The ordinary mappings are unchanged.
		{"ascii", `"abc".toUpperCase() + "ABC".toLowerCase()`, "ABCabc"},
		{"accents", `"ÄÖÜ".toLowerCase()`, "äöü"},
		{"unchanged", `"123 !".toUpperCase()`, "123 !"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) { checkEval(t, tc.src, tc.want) })
	}
}

// String.prototype.normalize reads its argument as a form name and refuses one
// it does not know.
func TestNormalizeIsAccepted(t *testing.T) {
	cases := []struct{ src, want string }{
		{`typeof "".normalize`, "function"},
		{`String.prototype.normalize.length + ""`, "0"},
		// Text already in NFC, which is nearly all of it, is returned as it is.
		{`"abc".normalize("NFC")`, "abc"},
		{`"abc".normalize()`, "abc"},
		// A form it does not know is refused rather than ignored.
		{`try { "a".normalize("NFX") } catch (e) { e.constructor.name }`, "RangeError"},
		{`var f = {toString: function () { return "NFC" }}; "a".normalize(f)`, "a"},
		{`try { "a".normalize(Symbol()) } catch (e) { e.constructor.name }`, "TypeError"},
	}
	for _, tc := range cases {
		checkEval(t, tc.src, tc.want)
	}
}

// includes, startsWith and endsWith search for text, so a regular expression
// given to one is a mistake rather than a pattern -- and what counts as one is
// what Symbol.match says.
func TestSearchMethodsRefuseARegExp(t *testing.T) {
	cases := []struct{ name, src, want string }{
		{"includes", `try { "".includes(/a/); "ok" } catch (e) { e.constructor.name }`, "TypeError"},
		{"startsWith", `try { "".startsWith(/a/); "ok" } catch (e) { e.constructor.name }`, "TypeError"},
		{"endsWith", `try { "".endsWith(/a/); "ok" } catch (e) { e.constructor.name }`, "TypeError"},
		{"anything that says it is one", `var o = {}
		  o[Symbol.match] = true
		  o.toString = function () { return "x" }
		  try { "x".includes(o); "ok" } catch (e) { e.constructor.name }`, "TypeError"},
		{"a throwing getter", `var re = /./
		  Object.defineProperty(re, Symbol.match, {get: function () { throw new RangeError() }})
		  try { "".includes(re); "ok" } catch (e) { e.constructor.name }`, "RangeError"},
		{"one that disclaims it", `var re = /b/; re[Symbol.match] = false
		  String("a/b/c".includes(re))`, "true"},
		// indexOf and lastIndexOf take the string form of whatever they are
		// given, regular expression or not.
		{"indexOf takes it", `String("a/b/".indexOf(/b/))`, "1"},
		{"the ordinary cases", `[("abc".includes("b")), "abc".startsWith("a"),
		  "abc".endsWith("c")].join()`, "true,true,true"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) { checkEval(t, tc.src, tc.want) })
	}
}

// lastIndexOf converts its starting point after the search string, and counts
// from it backwards: what it finds is the last occurrence starting at or before
// that index.
func TestLastIndexOfPosition(t *testing.T) {
	cases := []struct{ src, want string }{
		{`String("canal".lastIndexOf("a"))`, "3"},
		{`String("canal".lastIndexOf("a", 2))`, "1"},
		{`String("canal".lastIndexOf("a", 0))`, "-1"},
		{`String("canal".lastIndexOf("x"))`, "-1"},
		// undefined becomes NaN, which means the end rather than the start.
		{`String("canal".lastIndexOf("a", undefined))`, "3"},
		{`String("canal".lastIndexOf("a", NaN))`, "3"},
		{`String("canal".lastIndexOf("a", -1))`, "-1"},
		{`String("canal".lastIndexOf("a", Infinity))`, "3"},
		{`String("abab".lastIndexOf(""))`, "4"},
		{`String("abab".lastIndexOf("", 2))`, "2"},
		// The search string is converted first, so its toString runs before
		// the position's valueOf.
		{`var order = []
		  var a = {toString: function () { order.push("search"); return "a" }}
		  var b = {valueOf: function () { order.push("position"); return 0 }}
		  "canal".lastIndexOf(a, b); order.join()`, "search,position"},
	}
	for _, tc := range cases {
		checkEval(t, tc.src, tc.want)
	}
}

// A property a class synthesizes rather than stores cannot be deleted: it is
// there for as long as the object is.
func TestDeleteSynthesizedProperties(t *testing.T) {
	cases := []struct{ src, want string }{
		{`String(Reflect.deleteProperty(new String("str"), "length"))`, "false"},
		{`String(Reflect.deleteProperty(new String("str"), "0"))`, "false"},
		// One that names no character is absent already, so it goes.
		{`String(Reflect.deleteProperty(new String("str"), "5"))`, "true"},
		{`String(Reflect.deleteProperty([], "length"))`, "false"},
		// An element is a property like any other, and deleting it leaves a
		// hole rather than shortening the array.
		{`var a = [1, 2]; String(delete a[0]) + "," + a.length + "," + (0 in a)`,
			"true,2,false"},
		{`var o = {}; String(delete o.missing)`, "true"},
		{`(function () { "use strict"
		  try { delete new String("x")[0]; return "no error" }
		  catch (e) { return e.constructor.name } })()`, "TypeError"},
	}
	for _, tc := range cases {
		checkEval(t, tc.src, tc.want)
	}
}

// Normalization settles the two ways text can be spelled differently: whether a
// character is written whole or as a base and its marks, and what order the
// marks are in. The K forms additionally replace a character by what it stands
// for, which loses the distinction rather than respelling it.
func TestNormalizeForms(t *testing.T) {
	cases := []struct{ src, want string }{
		// Composed and decomposed spellings of the same text.
		{`"Å".normalize("NFD") === "Å" ? "yes" : "no"`, "yes"},
		{`"Å".normalize("NFC") === "Å" ? "yes" : "no"`, "yes"},
		{`["Å".normalize("NFD").length, "Å".normalize("NFC").length].join(",")`,
			"2,1"},
		// The default form is NFC.
		{`"Å".normalize() === "Å" ? "yes" : "no"`, "yes"},
		// A singleton: the angstrom sign is the letter.
		{`"Å".normalize("NFC") === "Å" ? "yes" : "no"`, "yes"},

		// The marks are put in canonical order, which is by combining class
		// and otherwise stable.
		{`"q̣̇".normalize("NFC") === "q̣̇" ? "yes" : "no"`, "yes"},
		{`"ẛ̣".normalize("NFD") === "ẛ̣" ? "yes" : "no"`, "yes"},
		{`"ẛ̣".normalize("NFKD") === "ṩ" ? "yes" : "no"`, "yes"},
		{`"ẛ̣".normalize("NFKC") === "ṩ" ? "yes" : "no"`, "yes"},

		// Compatibility replaces a character by what it stands for.
		{`"ﬁ".normalize("NFKC")`, "fi"},
		{`"²".normalize("NFKC")`, "2"},
		{`"ﬁ".normalize("NFC")`, "ﬁ"},

		// Hangul comes apart and goes back together by arithmetic.
		{`"가".normalize("NFD") === "가" ? "yes" : "no"`, "yes"},
		{`"가".normalize("NFC") === "가" ? "yes" : "no"`, "yes"},
		{`"퓛".normalize("NFD") === "퓛" ? "yes" : "no"`, "yes"},
		{`"퓛".normalize("NFC") === "퓛" ? "yes" : "no"`, "yes"},

		// A character the database excludes comes apart and does not go back.
		{`"क़".normalize("NFC") === "क़" ? "yes" : "no"`, "yes"},

		// What has nothing to say for itself is returned unchanged, including
		// a lone surrogate.
		{`"abc".normalize("NFD")`, "abc"},
		{`"".normalize()`, ""},
		{`"\uD800".normalize("NFC") === "\uD800" ? "yes" : "no"`, "yes"},
		{`String("a\uD800b".normalize("NFD").length)`, "3"},
		// A lone surrogate stands between what would otherwise combine, and
		// the text either side of it is normalized as usual.
		{`"e\uD800\u0301".normalize("NFC") === "e\uD800\u0301" ? "yes" : "no"`, "yes"},
		{`"e\u0301\uDC00e\u0301".normalize("NFC") === "\u00E9\uDC00\u00E9" ? "yes" : "no"`, "yes"},

		// Unicode 17's tables, which Node 26 has: Tulu-Tigalari, added in
		// Unicode 16, writes its vowel sign AU as two signs AI.
		{`"\u{113C5}".normalize("NFD") === "\u{113C2}\u{113C2}" ? "yes" : "no"`, "yes"},
		{`"\u{113C2}\u{113C2}".normalize("NFC") === "\u{113C5}" ? "yes" : "no"`, "yes"},

		// The form has to be one of the four.
		{`try { "a".normalize("NFX") } catch (e) { e.constructor.name }`, "RangeError"},
		{`try { "a".normalize(null) } catch (e) { e.constructor.name }`, "RangeError"},
		{`String(String.prototype.normalize.length)`, "0"},

		// Two spellings of the same text compare equal, whatever the locale.
		{`String("ö".localeCompare("ö"))`, "0"},
		{`String("a".localeCompare("a"))`, "0"},
		{`String("a".localeCompare("b") < 0)`, "true"},
	}
	for _, tc := range cases {
		checkEval(t, tc.src, tc.want)
	}
}

// A template literal joins its parts in one pass, which must produce exactly
// what concatenating them one at a time did -- including where the join falls
// between the halves of a surrogate pair.
func TestTemplateJoining(t *testing.T) {
	cases := []struct{ src, want string }{
		{"`a${1}b`", "a1b"},
		{"`${1}${2}${3}`", "123"},
		{"``", ""},
		{"`${\"\"}${\"\"}`", ""},
		{"`x`", "x"},
		{"`${undefined}|${null}|${true}`", "undefined|null|true"},
		{"`${1.5}|${-0}|${1e21}|${NaN}|${Infinity}`", "1.5|0|1e+21|NaN|Infinity"},
		{"`${[1,2]}|${({})}`", "1,2|[object Object]"},
		{"`${{toString(){ return \"t\" }}}`", "t"},
		{"`${Symbol.iterator.description}`", "Symbol.iterator"},
		// Non-ASCII parts, whose lengths are code units rather than bytes.
		{"`é${\"→\"}`.length + \"\"", "2"},
		{"`${\"😀\"}a`.length + \"\"", "3"},
		// A lone high surrogate followed by a lone low one makes a pair, which
		// is one code point and two code units.
		{"`${\"\\uD83D\"}${\"\\uDE00\"}`.length + \"\"", "2"},
		{"`${\"\\uD83D\"}${\"\\uDE00\"}` === \"😀\"", "true"},
		{"`${\"\\uD800\"}x`.length + \"\"", "2"},
		{"`${\"\\uD83D\"}${\"\\uDE00\"}`.codePointAt(0).toString(16)", "1f600"},
		// The conversions happen in the order they are written, interleaved
		// with the evaluations that follow them.
		{`var log = [];
		  ` + "`" + `${{toString(){ log.push("a"); return "" }}}${(log.push("b"), "")}` + "`" + `
		  log.join()`, "a,b"},
		{`var log = [];
		  ` + "`" + `${(log.push("x"), {toString(){ log.push("y"); return "" }})}${(log.push("z"), "")}` + "`" + `
		  log.join()`, "x,y,z"},
		// A part that throws stops the rest.
		{`var log = [];
		  try {
		    ` + "`" + `${{toString(){ throw new RangeError() }}}${(log.push("after"), "")}` + "`" + `
		  } catch (e) { e.constructor.name + ":" + log.length }`, "RangeError:0"},
		// A symbol has no string form, even in a template.
		{"try { `${Symbol()}` } catch (e) { e.constructor.name }", "TypeError"},
	}
	for _, tc := range cases {
		checkEval(t, tc.src, tc.want)
	}
}
