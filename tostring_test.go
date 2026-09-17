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
