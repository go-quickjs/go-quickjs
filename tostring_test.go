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
