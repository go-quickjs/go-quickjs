package quickjs_test

import (
	"strings"
	"testing"

	quickjs "github.com/go-quickjs/go-quickjs"
)

// Inline modifiers let a pattern be case-insensitive, multiline or dot-all in
// one place without being so everywhere, which otherwise takes two patterns or
// a hand-expanded character class.
func TestRegExpModifiers(t *testing.T) {
	cases := []struct{ src, want string }{
		// i applies only inside the group.
		{`String(/(?i:a)b/.test("Ab"))`, "true"},
		{`String(/(?i:a)b/.test("AB"))`, "false"},
		{`String(/(?i:a)b/.test("ab"))`, "true"},
		{`String(/a(?i:b)c/.test("aBc"))`, "true"},
		{`String(/a(?i:b)c/.test("aBC"))`, "false"},
		// And -i turns off what the pattern's own flag turned on.
		{`String(/(?-i:a)b/i.test("Ab"))`, "false"},
		{`String(/(?-i:a)b/i.test("aB"))`, "true"},

		{`String(/(?s:.)/.test("\n"))`, "true"},
		{`String(/./.test("\n"))`, "false"},
		{`String(/(?-s:.)/s.test("\n"))`, "false"},

		{`String(/(?m:^b)/.test("a\nb"))`, "true"},
		{`String(/^b/.test("a\nb"))`, "false"},
		{`String(/(?-m:^b)/m.test("a\nb"))`, "false"},

		// Several flags at once, in both directions.
		{`String(/(?im-s:a.)/.test("A\n"))`, "false"},
		{`String(/(?im-s:^a.)/.test("x\nAb"))`, "true"},

		// Nesting, where the inner group overrides the outer.
		{`String(/(?i:a(?-i:b)c)/.test("AbC"))`, "true"},
		{`String(/(?i:a(?-i:b)c)/.test("ABC"))`, "false"},

		// A modifier group does not capture.
		{`/(?i:a)(b)/.exec("Ab")[1]`, "b"},
		{`String(/(?i:a)(b)/.exec("Ab").length)`, "2"},

		// Patterns without modifiers are unaffected.
		{`String(/^a$/m.test("b\na"))`, "true"},
		{`String(/A/i.test("a"))`, "true"},
		{`"aXbXc".split(/x/i).join("-")`, "a-b-c"},
		{`String(/a.c/s.test("a\nc"))`, "true"},
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

func TestRegExpModifierErrors(t *testing.T) {
	bad := []string{
		// A flag may appear once across both halves.
		`new RegExp("(?ii:a)")`,
		`new RegExp("(?i-i:a)")`,
		// Each half must name something.
		`new RegExp("(?-:a)")`,
		// Only i, m and s are modifiers.
		`new RegExp("(?x:a)")`,
		`new RegExp("(?u:a)")`,
		// The flags must be followed by a colon.
		`new RegExp("(?i a)")`,
		`new RegExp("(?i)")`,
	}
	for _, src := range bad {
		rt := quickjs.New()
		_, err := rt.Eval(src)
		if err == nil {
			t.Errorf("%s: accepted, want SyntaxError", src)
		} else if !strings.Contains(err.Error(), "SyntaxError") {
			t.Errorf("%s: got %v, want SyntaxError", src, err)
		}
		rt.Close()
	}
}

// A property escape may name its property and value in any spelling Unicode
// publishes, so all of these describe the same class.
func TestRegExpPropertyEscapes(t *testing.T) {
	cases := []struct{ src, want string }{
		{`String(/\p{Script=Latin}/u.test("a"))`, "true"},
		{`String(/\p{Script=Latn}/u.test("a"))`, "true"},
		{`String(/\p{sc=Latn}/u.test("a"))`, "true"},
		{`String(/\p{sc=Grek}/u.test("α"))`, "true"},
		// The abbreviation is sometimes the longer string.
		{`String(/\p{Script=Han}/u.test("一"))`, "true"},
		{`String(/\p{Script=Hani}/u.test("一"))`, "true"},

		{`String(/\p{General_Category=Lowercase_Letter}/u.test("a"))`, "true"},
		{`String(/\p{gc=Ll}/u.test("a"))`, "true"},
		{`String(/\p{Lowercase_Letter}/u.test("a"))`, "true"},
		{`String(/\p{Lu}/u.test("A")) + "," + String(/\p{Lu}/u.test("a"))`, "true,false"},

		{`String(/\p{Alpha}/u.test("a")) + "," + String(/\p{Alpha}/u.test("1"))`, "true,false"},
		{`String(/\p{Alphabetic}/u.test("a"))`, "true"},
		{`String(/\p{AHex}/u.test("f")) + "," + String(/\p{AHex}/u.test("g"))`, "true,false"},
		{`String(/\p{White_Space}/u.test(" "))`, "true"},
		{`String(/\p{Cased}/u.test("a")) + "," + String(/\p{Cased}/u.test("1"))`, "true,false"},
		{`String(/\p{ID_Start}/u.test("a")) + "," + String(/\p{ID_Start}/u.test("$"))`,
			"true,false"},

		{`String(/\p{Any}/u.test("x")) + "," + String(/\p{ASCII}/u.test("x"))`, "true,true"},
		// The negated form.
		{`String(/\P{Script=Latn}/u.test("a"))`, "false"},
		{`String(/\P{Lu}/u.test("a"))`, "true"},
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

	for _, src := range []string{`/\p{Nope}/u`, `/\p{Script=Nope}/u`, `/\p{gc=Nope}/u`} {
		rt := quickjs.New()
		if _, err := rt.Eval(src); err == nil {
			t.Errorf("%s: accepted an unknown property", src)
		}
		rt.Close()
	}
}

// String.prototype.match and its relatives reach their pattern through a symbol
// method, which is what lets a RegExp subclass -- or any object at all -- define
// how it matches.
func TestRegExpSymbolMethods(t *testing.T) {
	cases := []struct{ src, want string }{
		{`typeof RegExp.prototype[Symbol.match]`, "function"},
		{`typeof RegExp.prototype[Symbol.matchAll]`, "function"},
		{`typeof RegExp.prototype[Symbol.replace]`, "function"},
		{`typeof RegExp.prototype[Symbol.search]`, "function"},
		{`typeof RegExp.prototype[Symbol.split]`, "function"},

		// Anything with the method acts as a pattern.
		{`var o = {[Symbol.replace](s, r) { return "custom:" + s + ":" + r; }};
		  "abc".replace(o, "X")`, "custom:abc:X"},
		{`var o = {[Symbol.match](s) { return ["m:" + s]; }}; "abc".match(o)[0]`, "m:abc"},
		{`var o = {[Symbol.split]() { return ["a", "b"]; }}; "xyz".split(o).join("-")`, "a-b"},
		{`var o = {[Symbol.search]() { return 42; }}; String("abc".search(o))`, "42"},
		{`var o = {[Symbol.matchAll]() { return [1, 2][Symbol.iterator](); }};
		  [..."x".matchAll(o)].join(",")`, "1,2"},
		// Including a subclass that overrides one.
		{`class R extends RegExp { [Symbol.replace]() { return "sub"; } }
		  "x".replace(new R("x"), "y")`, "sub"},

		// And the ordinary paths are unchanged.
		{`"abc".replace(/b/, "X")`, "aXc"},
		{`"aXbXc".replace(/x/gi, "-")`, "a-b-c"},
		{`"abc".replace("b", "Y")`, "aYc"},
		{`"aaa".replaceAll("a", "b")`, "bbb"},
		{`"a1b2".split(/\d/).join("-")`, "a-b-"},
		{`"a,b".split(",").join("-")`, "a-b"},
		{`"abc".match(/(b)(c)/).slice(1).join(",")`, "b,c"},
		{`[..."a1b2".matchAll(/\d/g)].map(m => m[0]).join(",")`, "1,2"},
		{`"abc".replace(/b/, m => m.toUpperCase())`, "aBc"},
		{`"abc".replace(/(b)/, "[$1]")`, "a[b]c"},
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

	// replaceAll and matchAll insist on the global flag, because doing the job
	// once would silently be the wrong answer.
	for _, src := range []string{`"a".replaceAll(/a/, "b")`, `"a".matchAll(/a/)`} {
		rt := quickjs.New()
		if _, err := rt.Eval(src); err == nil {
			t.Errorf("%s: no error, want TypeError", src)
		}
		rt.Close()
	}
}

// The v flag makes a character class a set expression rather than a flat list:
// classes nest, -- subtracts, && intersects, and \q{} names whole strings. So
// [\p{Letter}--[aeiou]] says what it means instead of enumerating the
// difference by hand.
func TestRegExpClassSets(t *testing.T) {
	cases := []struct{ src, want string }{
		{`String(/[[a-z][0-9]]/v.test("5"))`, "true"},
		{`String(/[[a-z][0-9]]/v.test("!"))`, "false"},
		{`[/[[a-z]--[aeiou]]/v.test("b"), /[[a-z]--[aeiou]]/v.test("a")].join(",")`,
			"true,false"},
		{`[/[[a-z]&&[b-d]]/v.test("c"), /[[a-z]&&[b-d]]/v.test("z")].join(",")`,
			"true,false"},
		{`[/[\p{ASCII}--[a-z]]/v.test("A"), /[\p{ASCII}--[a-z]]/v.test("a")].join(",")`,
			"true,false"},
		// Chained operators of the same kind.
		{`String(/[[a-z]--[aeiou]--[xyz]]/v.test("x"))`, "false"},
		{`String(/[\p{ASCII}&&\p{Letter}&&[a-c]]/v.test("b"))`, "true"},

		// A class can denote strings, not just code points.
		{`[/[\q{abc|d}]/v.test("abc"), /[\q{abc|d}]/v.test("d")].join(",")`, "true,true"},
		{`"xabcy".replace(/[\q{abc}]/v, "-")`, "x-y"},
		// Longest first: a class holding both must prefer the longer.
		{`"abc".replace(/[\q{abc|a}]/v, "-")`, "-"},
		// A one-character string is just a code point.
		{`String(/[\q{a}]/v.test("a"))`, "true"},
		{`String(/[\q{}]/v.test(""))`, "true"},

		{`String(/[^a-z]/v.test("5"))`, "true"},
		{`String(/[a-z]/v.test("m"))`, "true"},
		// An escaped syntax character is itself.
		{`String(/[\(]/v.test("("))`, "true"},
		{`String(/[\-]/v.test("-"))`, "true"},

		// u mode is unaffected: a bracket there is an ordinary character.
		{`String(/[[a]/u.test("["))`, "true"},
		{`String(/[a-z]/u.test("m"))`, "true"},
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

	bad := []struct{ name, src string }{
		// Mixing operators has no agreed precedence, so it has to be bracketed.
		{"mixed operators", `new RegExp("[a--b&&c]", "v")`},
		// There is no sensible "every string but these".
		{"negated string class", `new RegExp("[^\\q{ab}]", "v")`},
		// The set syntax reserves these, so a pattern written today cannot
		// change meaning when an operator is added.
		{"unescaped paren", `new RegExp("[(]", "v")`},
		{"unescaped brace", `new RegExp("[{]", "v")`},
		{"reserved double", `new RegExp("[!!]", "v")`},
		{"triple ampersand", `new RegExp("[a&&&b]", "v")`},
		{"unterminated q", `new RegExp("[\\q{ab]", "v")`},
		{"class as a range end", `new RegExp("[a-\\d]", "v")`},
	}
	for _, tc := range bad {
		rt := quickjs.New()
		if _, err := rt.Eval(tc.src); err == nil {
			t.Errorf("%s: accepted, want SyntaxError", tc.name)
		}
		rt.Close()
	}
}

// Under the u and v flags an escape stands for a character only where the
// escape is needed, and \0 is the null character rather than the start of an
// octal escape.
func TestUnicodeEscapesAreRestricted(t *testing.T) {
	cases := []struct{ src, want string }{
		// A syntax character may be escaped; anything else may not.
		{`/\$/u.test("$")`, "true"},
		{`/\//u.test("/")`, "true"},
		{`/\{/u.test("{")`, "true"},
		{`/\ /u.test(" ")`, "SyntaxError"},
		{`/\-/u.test("-")`, "SyntaxError"},
		{`/\_/u.test("_")`, "SyntaxError"},
		{`/\@/u.test("@")`, "SyntaxError"},
		// Inside a class the range character may be escaped, and the v flag
		// reserves more punctuation there.
		{`/[\-]/u.test("-")`, "true"},
		{`/[\-]/v.test("-")`, "true"},
		{`/[\&]/v.test("&")`, "true"},
		{`/[\@]/v.test("@")`, "true"},
		{`/[\@]/u.test("@")`, "SyntaxError"},
		// Without the flags the old tolerance stands.
		{`/\ /.test(" ")`, "true"},
		{`/\@/.test("@")`, "true"},
		// \0 is null, and under u nothing may follow it.
		{`/\0/u.test("\0")`, "true"},
		{`/\00/u.test("\0")`, "SyntaxError"},
		{`/\00/.test("\0")`, "true"},
	}
	for _, tc := range cases {
		checkEval(t, `try { String(eval(`+jsQuote(tc.src)+`)) } catch (e) { e.constructor.name }`,
			tc.want)
	}
}

// A repetition's first iteration is required when its lower bound is one, and
// a required iteration has no empty check: `x+` where x matches nothing still
// matches, once.
func TestRequiredRepetitionMayMatchNothing(t *testing.T) {
	cases := []struct{ src, want string }{
		{`String(/(a*)b\1+/.exec("baaaac"))`, "b,"},
		{`String(/(a*)b\1+/.exec("baaaac").index)`, "0"},
		{`String(/(?:)+/.test(""))`, "true"},
		{`String(/(a*)+/.exec("aaa"))`, "aaa,aaa"},
		// The repetitions after the first still stop at an empty one.
		{`String(/(a*)*/.exec("b"))`, ","},
		{`String("aaa".replace(/a*/g, "-"))`, "--"},
	}
	for _, tc := range cases {
		checkEval(t, tc.src, tc.want)
	}
}

// `\k` names a group only in a pattern that has one, or under the u flag.
// Elsewhere it is the letter k, and what follows it is whatever it looks like.
func TestNamedBackreferenceNeedsAName(t *testing.T) {
	cases := []struct{ src, want string }{
		{`String("x".split(/\k<x>/))`, "x"},
		{`String(/\k<x>/.test("k<x>"))`, "true"},
		{`String(/\k/.test("k"))`, "true"},
		{`String(/(?<a>x)\k<a>/.test("xx"))`, "true"},
		{`String(/(?<a>x)\k<a>/.test("xy"))`, "false"},
		// With a named group in the pattern, or under u, it has to name one.
		{`try { String(eval("/\\k<a>/u.test('x')")) } catch (e) { e.constructor.name }`,
			"SyntaxError"},
		{`try { String(eval("/(?<a>x)\\k<b>/.test('x')")) } catch (e) { e.constructor.name }`,
			"SyntaxError"},
	}
	for _, tc := range cases {
		checkEval(t, tc.src, tc.want)
	}
}
