package quickjs_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	quickjs "github.com/go-quickjs/go-quickjs"
)

// The five symbol methods are written against the receiver's properties rather
// than against a compiled pattern: the match comes from whatever exec the
// receiver has, the flags from its flags getter, the position from lastIndex,
// and a result's pieces from that result's own index, length and numbered
// properties. That is what lets a subclass override any of them.
func TestRegExpSymbolMethodProtocol(t *testing.T) {
	cases := []struct{ src, want string }{
		// A custom exec drives replace, match and split.
		{`var rx = /x/; rx.exec = () => null; "abc".replace(rx, "-")`, "abc"},
		{`var rx = /./; var n = 0;
		  rx.exec = () => n++ ? null : Object.assign(["b"], {index: 1});
		  "abc".replace(rx, "-")`, "a-c"},
		{`var rx = /./g; var n = 0;
		  rx.exec = () => n++ ? null : Object.assign(["b"], {index: 1});
		  "abc".match(rx).join(",")`, "b"},
		{`var rx = /x/; rx.exec = () => null; String("abc".search(rx))`, "-1"},

		// The index a result claims is what decides where the replacement
		// lands, whatever the pattern would have matched.
		{`var rx = /a/; rx.exec = () => ({0: "zz", index: 0, length: 1});
		  "abc".replace(rx, "-")`, "-c"},
		// And it is clamped rather than trusted.
		{`var rx = /a/; rx.exec = () => ({0: "x", index: 99, length: 1});
		  "abc".replace(rx, "-")`, "abc-"},
		{`var rx = /a/; rx.exec = () => ({0: "x", index: -5, length: 1});
		  "abc".replace(rx, "-")`, "-bc"},

		// Groups come from the result, so $<name> works on a synthesized one.
		{`var rx = /a/; rx.exec = () => ({0: "b", index: 1, length: 1, groups: {g: "G"}});
		  "abc".replace(rx, "[$<g>]")`, "a[G]c"},
		// With no groups on the result, $<g> is four literal characters.
		{`var rx = /a/; rx.exec = () => ({0: "b", index: 1, length: 1});
		  "abc".replace(rx, "[$<g>]")`, "a[$<g>]c"},
		// A capture is read by number and coerced.
		{`var rx = /a/; rx.exec = () => ({0: "b", 1: 42, index: 1, length: 2});
		  "abc".replace(rx, "[$1]")`, "a[42]c"},

		// The flags getter decides whether every match is replaced. A pattern
		// whose own flags lack g needs an exec that advances, since the
		// built-in one would keep returning the same match forever.
		{`var rx = /a/; var n = 0;
		  Object.defineProperty(rx, "flags", {get: () => "g"});
		  rx.exec = () => n < 3 ? {0: "a", index: n++, length: 1} : null;
		  "aaa".replace(rx, "-")`, "---"},
		{`var rx = /a/g;
		  Object.defineProperty(rx, "flags", {get: () => ""});
		  "aaa".replace(rx, "-")`, "-aa"},

		// search restores lastIndex, since finding where a pattern occurs is
		// not meant to move a global one along.
		{`var r = /a/g; r.lastIndex = 2; "aaa".search(r) + "," + r.lastIndex`, "0,2"},

		// The ordinary behaviour is unchanged.
		{`"abc".replace(/b/, "-")`, "a-c"},
		{`"aaa".replace(/a/g, "-")`, "---"},
		{`"abc".replace(/(b)/, "[$1]")`, "a[b]c"},
		{`"abc".replace(/(?<g>b)/, "[$<g>]")`, "a[b]c"},
		{`"abc".replace(/b/, "$` + "`" + `|$'")`, "aa|cc"},
		{`"a1b2".replace(/\d/g, (m, i) => i)`, "a1b3"},
		{`"aaa".match(/a/g).join(",")`, "a,a,a"},
		{`String("abc".match(/z/g))`, "null"},
		{`"a1b2c".split(/(\d)/).join("|")`, "a|1|b|2|c"},
		{`"ab".split(/(?:)/).join("-")`, "a-b"},
		{`String("😀x".split(/(?:)/u).length)`, "2"},
		{`[..."a1b2".matchAll(/\d/g)].map(m => m[0] + "@" + m.index).join(",")`, "1@1,2@3"},
		{`Object.prototype.toString.call("a".matchAll(/a/g))`, "[object RegExp String Iterator]"},
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

	bad := []string{
		// exec must return a match or the absence of one.
		`var rx = /a/; rx.exec = () => 1; "abc".replace(rx, "-")`,
		`var rx = /a/; rx.exec = () => "x"; "abc".match(rx)`,
		// A method that cannot be given a receiver to read.
		`RegExp.prototype[Symbol.replace].call(1, "abc", "-")`,
		`RegExp.prototype[Symbol.match].call(1, "abc")`,
		`RegExp.prototype[Symbol.split].call(1, "abc")`,
		// lastIndex has to be writable for a global pattern to advance.
		`var rx = /a/g; Object.defineProperty(rx, "lastIndex", {value: 0, writable: false});
		 "aaa".replace(rx, "-")`,
	}
	for _, src := range bad {
		rt := quickjs.New()
		if _, err := rt.Eval(src); err == nil {
			t.Errorf("%s: accepted, want TypeError", src)
		}
		rt.Close()
	}
}

// source, flags and toString read through the receiver rather than the compiled
// pattern, so that overriding one is reflected in the others.
func TestRegExpAccessors(t *testing.T) {
	cases := []struct{ src, want string }{
		{`/ab/gi.flags`, "gi"},
		{`/ab/dgimsuy.flags`, "dgimsuy"},
		// v turns on the u behaviour internally, but the two are alternatives
		// to a script.
		{`/ab/v.flags`, "v"},
		{`[/a/v.unicodeSets, /a/v.unicode, /a/u.unicode].join(",")`, "true,false,true"},

		// flags is assembled from the individual getters.
		{`Object.getOwnPropertyDescriptor(RegExp.prototype, "flags").get.call(
		    {hasIndices: 1, global: 0, ignoreCase: 1, sticky: true})`, "diy"},
		{`RegExp.prototype.toString.call({source: "x", flags: "g"})`, "/x/g"},

		// RegExp.prototype is not a RegExp, but naming it must not throw.
		{`RegExp.prototype.flags`, ""},
		{`String(RegExp.prototype.source)`, "(?:)"},
		{`String(RegExp.prototype.global)`, "undefined"},
		{`String(RegExp.prototype)`, "/(?:)/"},

		// A pattern built from a string that contains a slash or a line
		// terminator has to be escaped before it can sit between two slashes,
		// or the printed form would not parse back.
		{`new RegExp("/").source`, `\/`},
		{`String(new RegExp("/"))`, `/\//`},
		{`new RegExp("a/b").source`, `a\/b`},
		{`String(new RegExp("\n"))`, `/\n/`},
		// One that is already escaped is left alone.
		{`new RegExp("\\/").source`, `\/`},
		{`String(eval(String(new RegExp("/"))).test("/"))`, "true"},
		{`String(new RegExp(""))`, "/(?:)/"},
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

	bad := []string{
		`RegExp.prototype.source.call ? 0 : Object.getOwnPropertyDescriptor(
		   RegExp.prototype, "source").get.call({})`,
		`Object.getOwnPropertyDescriptor(RegExp.prototype, "global").get.call({})`,
		`RegExp.prototype.toString.call(1)`,
	}
	for _, src := range bad {
		rt := quickjs.New()
		if _, err := rt.Eval(src); err == nil {
			t.Errorf("%s: accepted, want TypeError", src)
		} else if !strings.Contains(err.Error(), "TypeError") {
			t.Errorf("%s: got %v, want TypeError", src, err)
		}
		rt.Close()
	}
}

// A pattern whose flags claim to be global while its exec keeps returning the
// same match never terminates -- which the protocol permits, and which is why
// the loop that collects matches has to be interruptible.
func TestRegExpSymbolReplaceIsInterruptible(t *testing.T) {
	rt := quickjs.New()
	defer rt.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	_, err := rt.EvalContext(ctx, `
		var rx = /a/;
		Object.defineProperty(rx, "flags", {get: () => "g"});
		"aaa".replace(rx, "-");
	`)
	if err == nil {
		t.Fatal("a replace that cannot terminate should have been interrupted")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("got %v, want the deadline", err)
	}
}

// The String methods that take a pattern ask it for its symbol method before
// doing anything themselves, which is what lets anything act as a pattern. Who
// gets asked, and in what order, is observable.
func TestStringPatternDispatch(t *testing.T) {
	cases := []struct{ src, want string }{
		// Only an object is asked. A primitive cannot carry the method itself,
		// and reaching through to its wrapper prototype would let a change
		// there rewrite every string replace in the program.
		{`var asked = 0;
		  Object.defineProperty(BigInt.prototype, Symbol.replace, {get() { asked++; }});
		  "a1b1c".replaceAll(1n, "X") + "," + asked`, "aXbXc,0"},
		{`var asked = 0;
		  Object.defineProperty(String.prototype, Symbol.replace, {get() { asked++; }});
		  "aba".replace("b", "X") + "," + asked`, "aXa,0"},

		// An object is, and its method wins.
		{`var o = {[Symbol.replace]: (s, r) => "from " + s};
		  "abc".replace(o, "-")`, "from abc"},
		{`var o = {[Symbol.split]: s => ["from", s]};
		  "abc".split(o).join("|")`, "from|abc"},
		{`var o = {[Symbol.match]: s => "m:" + s}; "abc".match(o)`, "m:abc"},

		// A pattern presents itself as a regexp through Symbol.match, which is
		// what the global-flag check consults -- not its class.
		{`var o = {[Symbol.match]: true, flags: "g",
		           [Symbol.replace]: () => "ok"};
		  "abc".replaceAll(o, "-")`, "ok"},
		// A real RegExp can disclaim being one.
		{`var r = /a/; r[Symbol.match] = false; r[Symbol.replace] = () => "ok";
		  "abc".replaceAll(r, "-")`, "ok"},

		// Position arguments are clamped into the string.
		{`["word".includes("w", 5), "word".includes("d", -1)].join(",")`, "false,true"},
		{`["word".startsWith("", Infinity), "word".startsWith("w", -1)].join(",")`, "true,true"},
		{`["word".endsWith("d", Infinity), "word".endsWith("", -1)].join(",")`, "true,true"},
		{`["word".includes("or", 1), "word".includes("or", 2)].join(",")`, "true,false"},
		{`["word".startsWith("or", 1), "word".startsWith("or", 2)].join(",")`, "true,false"},
		{`["word".endsWith("or", 3), "word".endsWith("or", 4)].join(",")`, "true,false"},

		{`"aaa".replaceAll("a", "-")`, "---"},
		{`"aaa".replaceAll("", "-")`, "-a-a-a-"},
		{`"".replaceAll("", "x")`, "x"},
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

	bad := []string{
		// Present but not callable is a mistake, not an absence.
		`"abc".replace({[Symbol.replace]: 1}, "-")`,
		`"abc".split({[Symbol.split]: 1})`,
		`"abc".match({[Symbol.match]: 1})`,
		// Replacing every occurrence of a pattern that only matches once.
		`"aaa".replaceAll(/a/, "-")`,
		`"aaa".matchAll(/a/)`,
		// A pattern claiming to be one but with no flags to read.
		`"abc".replaceAll({[Symbol.match]: true, flags: null}, "-")`,
		// The receiver is checked before the pattern is asked anything.
		`String.prototype.replace.call(null, {get [Symbol.replace]() { throw 0; }}, "-")`,
		`String.prototype.split.call(undefined, {get [Symbol.split]() { throw 0; }})`,
	}
	for _, src := range bad {
		rt := quickjs.New()
		if _, err := rt.Eval(src); err == nil {
			t.Errorf("%s: accepted, want TypeError", src)
		} else if !strings.Contains(err.Error(), "TypeError") {
			t.Errorf("%s: got %v, want TypeError", src, err)
		}
		rt.Close()
	}
}

// Anything that says it is a regular expression is treated as one, which is
// what Symbol.match is for: a plain object that defines it truthily can stand
// in for one, and its source and flags are read rather than its string form.
func TestRegExpConstructorFromRegExpLike(t *testing.T) {
	cases := []struct{ src, want string }{
		{`var o = {source: "abc", flags: "g"}; o[Symbol.match] = true;
		  String(new RegExp(o))`, "/abc/g"},
		{`var o = {source: "abc", flags: "g"}; o[Symbol.match] = true;
		  String(new RegExp(o, "i"))`, "/abc/i"},
		// An object that says no is stringified as before.
		{`var o = {source: "abc", toString: function () { return "x" }};
		  String(new RegExp(o))`, "/x/"},
		// Called rather than constructed on a regular expression of this very
		// constructor, RegExp hands it straight back.
		{`var re = /a/g; String(RegExp(re) === re)`, "true"},
		{`var re = /a/g; String(new RegExp(re) === re)`, "false"},
		{`var re = /a/g; String(RegExp(re, "i") === re)`, "false"},
		{`String(new RegExp("a", "g"))`, "/a/g"},

		// RegExp.escape escapes a leading digit or letter numerically so the
		// result cannot merge with what precedes it, and the syntax characters
		// with a backslash.
		{`RegExp.escape("$")`, `\$`},
		{`RegExp.escape(".")`, `\.`},
		{`RegExp.escape("_")`, "_"},
		{`RegExp.escape("a")`, `\x61`},
		{`RegExp.escape("ab")`, `\x61b`},
		{`RegExp.escape(" ")`, `\x20`},
		{`RegExp.escape(" ")`, `\u202f`},
		{`RegExp.escape("-")`, `\x2d`},
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

// TestMatchIndices covers the d flag, which asks a match to report where each
// group matched as well as what it matched.
func TestMatchIndices(t *testing.T) {
	cases := []struct{ src, want string }{
		{`JSON.stringify(/a(b)/d.exec("xab").indices)`, "[[1,3],[2,3]]"},
		{`String(/a(b)/.exec("xab").indices)`, "undefined"},
		// A group that did not participate has no bounds, which is distinct
		// from having matched the empty string.
		{`JSON.stringify(/(a)(b)?/d.exec("a").indices)`, "[[0,1],[0,1],null]"},
		{`JSON.stringify(/(a)()/d.exec("a").indices)`, "[[0,1],[0,1],[1,1]]"},
		{`JSON.stringify(/(?<n>b)/d.exec("ab").indices.groups)`, `{"n":[1,2]}`},
		{`String(/(?<n>b)/d.exec("ab").indices.groups.n)`, "1,2"},
		{`String(/(b)/d.exec("ab").indices.groups)`, "undefined"},
		{`String("bab".match(/(a)/du).indices)`, "1,2,1,2"},
		{`var d = Object.getOwnPropertyDescriptor(/a/d.exec("a"), "indices");
		  [d.writable, d.enumerable, d.configurable].join(",")`, "true,true,true"},
		{`String(/a/d.flags) + "," + /a/d.hasIndices + "," + /a/.hasIndices`,
			"d,true,false"},
		// The indices array is a plain Array, so it iterates and destructures.
		{`var [all, g] = /a(b)/d.exec("ab").indices; all.join("-") + "|" + g.join("-")`,
			"0-2|1-2"},
	}
	for _, tc := range cases {
		checkEval(t, tc.src, tc.want)
	}
}

// TestRegExpLiteralEarlyErrors covers two shapes a pattern may not take.
func TestRegExpLiteralEarlyErrors(t *testing.T) {
	bad := []string{
		// The flags are a token of their own, not an identifier, so an escape
		// there is not a flag.
		"/a/\\u0067",
		"/a/\\u0069",
		// A lookahead may be quantified in sloppy mode as a legacy allowance;
		// a lookbehind came long afterwards, so there is nothing to be
		// compatible with.
		`/(?<=a)?/`,
		`/(?<!a)*/`,
		`/(?<=a){2}/`,
	}
	for _, src := range bad {
		checkEval(t, `try { eval(`+jsQuote(src)+`); "no throw" }
		              catch (e) { e.constructor.name }`, "SyntaxError")
	}
	good := []struct{ src, want string }{
		{`/a/gimsuy.flags`, "gimsuy"},
		{`String(/(?=a)?b/.test("b"))`, "true"},
		{`String(/(?<=a)b/.test("ab"))`, "true"},
		{`String(/(?<!a)b/.test("cb"))`, "true"},
	}
	for _, tc := range good {
		checkEval(t, tc.src, tc.want)
	}
}

// TestRegExpGroupNames covers a named group whose name is spelled with an
// escape, which is allowed whatever the flags say: the name is an identifier,
// and an identifier written in a pattern has the escapes one in source does.
func TestRegExpGroupNames(t *testing.T) {
	cases := []struct{ src, want string }{
		{`Object.keys(new RegExp("(?<\\u{1d4d1}rown>x)", "u").exec("x").groups).join()`,
			"\U0001d4d1rown"},
		{`Object.keys(new RegExp("(?<\\u0041b>x)").exec("x").groups).join()`, "Ab"},
		{`new RegExp("(?<\\u0041b>x)").exec("x").groups.Ab`, "x"},
		// The name is read the same way twice, so a reference finds the group.
		{`new RegExp("\\k<\\u0041b>(?<Ab>x)").exec("x")[0]`, "x"},
		{`/(?<a>x)(?<b>y)/.exec("xy").groups.b`, "y"},
		{`try { eval("/(?<a>x)(?<a>y)/") } catch (e) { e.constructor.name }`, "SyntaxError"},
		{`try { eval("/(?<\\x41>a)/") } catch (e) { e.constructor.name }`, "SyntaxError"},
	}
	for _, tc := range cases {
		checkEval(t, tc.src, tc.want)
	}
}

// TestRegExpExecLastIndex covers lastIndex, which exec reads whatever the flags
// say and writes back only for a global or sticky pattern.
func TestRegExpExecLastIndex(t *testing.T) {
	cases := []struct{ src, want string }{
		{`var gets = 0
		  var counter = {valueOf() { gets++; return 0 }}
		  var r = /a/
		  r.lastIndex = counter
		  String(r.exec("nbc")) + "," + (r.lastIndex === counter) + "," + gets`,
			"null,true,1"},
		{`var r = /a/g; r.lastIndex = 3; r.exec("bbb"); String(r.lastIndex)`, "0"},
		{`var r = /b/g; r.exec("bbb")[0] + "," + r.lastIndex`, "b,1"},
		// A negative lastIndex is zero, not a search from the end.
		{`var r = /(?:ab|cd)\d?/g
		  r.lastIndex = -1
		  r.exec("aacd22 ")[0] + "," + r.lastIndex`, "cd2,5"},
		{`var r = /a/; r.lastIndex = 3; r.exec("bba"); String(r.lastIndex)`, "3"},
	}
	for _, tc := range cases {
		checkEval(t, tc.src, tc.want)
	}
}

// TestRegExpLookaroundCaptures covers what a lookaround leaves behind. A
// positive one that matched contributes its captures; one that did not, and a
// negative one that did, contribute nothing.
func TestRegExpLookaroundCaptures(t *testing.T) {
	cases := []struct{ src, want string }{
		{`JSON.stringify(/(?=(abc))a/.exec("abc"))`, `["a","abc"]`},
		// The one iteration of `?` matched nothing, so it is not an iteration
		// at all and the group it set goes back to being unset.
		{`JSON.stringify(/(?=(abc))?a/.exec("abc"))`, `["a",null]`},
		{`JSON.stringify(/(?!(x))a/.exec("a"))`, `["a",null]`},
		{`JSON.stringify("abcdef".match(/(?<!(^|[ab]))\w{2}/))`, `["de",null]`},
		{`JSON.stringify(/(a)(?=(b))c?/.exec("ab"))`, `["a","a","b"]`},
		// Backtracking past a lookaround takes its captures back too.
		{`JSON.stringify(/(?:(?=(a))a|b)*$/.exec("ab"))`, `["ab",null]`},
		{`JSON.stringify(/(a)?b/.exec("b"))`, `["b",null]`},
		{`JSON.stringify(/(a)?b/.exec("ab"))`, `["ab","a"]`},

		// Every iteration of a repetition starts with the groups inside it
		// unset: one that matched on an earlier pass is not part of the match
		// unless it matches again.
		{`JSON.stringify(/(z)((a+)?(b+)?(c))*/.exec("zaacbbbcac"))`,
			`["zaacbbbcac","z","ac","a",null,"c"]`},
		{`JSON.stringify("aabb".match(/(a)*(b)*/))`, `["aabb","a","b"]`},
	}
	for _, tc := range cases {
		checkEval(t, tc.src, tc.want)
	}
}

// TestRegExpLookbehind covers a lookbehind, which matches leftwards from where
// it stands: its terms run last-first, so a quantifier's last iteration is the
// leftmost one and that is what its group keeps.
func TestRegExpLookbehind(t *testing.T) {
	cases := []struct{ src, want string }{
		{`JSON.stringify("abcdef".match(/(?<=(c))def/))`, `["def","c"]`},
		{`JSON.stringify("abcdef".match(/(?<=(\w{2}))def/))`, `["def","bc"]`},
		{`JSON.stringify("abcdef".match(/(?<=(\w(\w)))def/))`, `["def","bc","c"]`},
		{`JSON.stringify("abcdef".match(/(?<=(\w){3})def/))`, `["def","a"]`},
		{`JSON.stringify("abcdef".match(/(?<=(bc)|(cd))./))`, `["d","bc",null]`},
		{`JSON.stringify("abcdef".match(/(?<=([ab]{1,2})\D|(abc))\w/))`, `["c","a",null]`},
		{`JSON.stringify("abcdef".match(/\D(?<=([ab]+))(\w)/))`, `["ab","a","b"]`},
		{`JSON.stringify("abcdef".match(/(?<=b|c)\w/g))`, `["c","d"]`},
		{`JSON.stringify("abcdef".match(/(?<=[b-e])\w{2}/g))`, `["cd","ef"]`},
		{`JSON.stringify("abcdef".match(/(?<!(^|[ab]))\w{2}/))`, `["de",null]`},

		// A greedy quantifier inside one takes as much as it can, leftwards.
		{`JSON.stringify("abbbbbbc".match(/(?<=(b+))c/))`, `["c","bbbbbb"]`},
		{`JSON.stringify("abbbbbbc".match(/(?<=(b+?))c/))`, `["c","b"]`},
		// A lookahead inside a lookbehind still matches rightwards.
		{`JSON.stringify("abcdef".match(/(?<=(?=c)cd)ef/))`, `["ef"]`},
		// A backreference inside one matches the text ending at the cursor.
		{`JSON.stringify("abab".match(/(?<=(ab)\1)$/))`, `["","ab"]`},
	}
	for _, tc := range cases {
		checkEval(t, tc.src, tc.want)
	}
}

// A pattern's step budget is what one match may spend, not what the pattern
// may spend over its life: the matcher is lent back for every match, and a
// pattern that had once given up would otherwise refuse everything after.
func TestRegExpBudgetIsPerMatch(t *testing.T) {
	rt := quickjs.New()
	defer rt.Close()

	v, err := rt.Eval(`
		var re = /(a+)+b/
		var gaveUp = false
		try { re.test("a".repeat(100)) } catch (e) { gaveUp = e instanceof SyntaxError }
		// Whatever the first attempt did, an ordinary match after it works.
		var after = re.exec("aab")
		gaveUp + ":" + (after === null ? "null" : after[0])
	`)
	if err != nil {
		t.Fatal(err)
	}
	if got := v.String(); got != "true:aab" {
		t.Errorf("gave up and then matched = %q, want %q", got, "true:aab")
	}
}

// The same for a pattern used over and over, which is what a program does with
// one: the work of the matches already done may not count against the next.
func TestRegExpRepeatedUseKeepsWorking(t *testing.T) {
	rt := quickjs.New()
	defer rt.Close()

	v, err := rt.Eval(`
		var re = /(\w+)@(\w+)\.com/
		var s = "write to someone@example.com today"
		var n = 0
		for (var i = 0; i < 20000; i++) n += re.exec(s)[1].length
		n
	`)
	if err != nil {
		t.Fatal(err)
	}
	if got := v.String(); got != "140000" {
		t.Errorf("total = %q, want 140000", got)
	}
}

// What a match reports is cut from the subject rather than encoded again from
// its code units, which has to give the same text back -- including where a
// match falls on half of a surrogate pair, which is legal and not the same
// string as the pair.
func TestMatchPiecesComeFromTheSubject(t *testing.T) {
	cases := []struct{ src, want string }{
		{`/(\w+)@(\w+)/.exec("a-someone@example-b").slice(0, 3).join("|")`,
			"someone@example|someone|example"},
		{`/(\w+)@(\w+)/.exec("a-someone@example-b").index + ""`, "2"},
		{`/x(y)?z/.exec("xz").map(v => String(v)).join()`, "xz,undefined"},
		// Non-ASCII, where a code unit is not a byte.
		{`/(é+)(→)/.exec("aééé→b").slice(0, 3).join("|")`, "ééé→|ééé|→"},
		{`/(é+)(→)/.exec("aééé→b").index + ""`, "1"},
		{`"aéb".match(/./g).join()`, "a,é,b"},
		{`"😀x".match(/./gu).join()`, "😀,x"},
		// Without the u flag a dot matches one code unit, so a pair is split.
		{`"😀".match(/./g).length + ""`, "2"},
		{`"😀".match(/./g)[0].charCodeAt(0).toString(16)`, "d83d"},
		{`"😀".match(/./g).join("") === "😀"`, "true"},
		{`/(.)(.)/.exec("😀")[1].charCodeAt(0).toString(16)`, "d83d"},
		{`/(.)(.)/.exec("😀")[2].charCodeAt(0).toString(16)`, "de00"},
		{`"a\uD800b".match(/./g)[1].charCodeAt(0).toString(16)`, "d800"},
		{`"a\uD800b".match(/./g)[1].length + ""`, "1"},
		// split, which cuts the pieces between the matches.
		{`"a1b2c".split(/\d/).join("|")`, "a|b|c"},
		{`"é1é2é".split(/\d/).join("|")`, "é|é|é"},
		{`"😀1😀".split(/\d/).map(s => s.length).join()`, "2,2"},
		{`"a\uD800b1c".split(/\d/)[0].length + ""`, "3"},
		{`"x".split(/(y)?/).map(v => String(v)).join()`, "x"},
		// replace, whose replacement sees the matched text.
		{`"a1b".replace(/\d/, m => "[" + m + "]")`, "a[1]b"},
		{`"éXé".replace(/X/, m => m.length + "")`, "é1é"},
	}
	for _, tc := range cases {
		checkEval(t, tc.src, tc.want)
	}
}

// TestRegExpDuplicateNamedGroups pins that groups in different alternatives
// may share a name, and that everything reading a group by name reads the one
// that took part.
func TestRegExpDuplicateNamedGroups(t *testing.T) {
	evalCases(t, []struct{ src, want string }{
		{`JSON.stringify(/(?<x>a)|(?<x>b)/.exec("b").groups)`, `{"x":"b"}`},
		{`/(?<x>a)|(?<x>b)/.exec("a").length`, "3"},
		// The groups object lists a shared name once, where it first appears.
		{`Object.keys(/(?<y>a)(?<x>a)|(?<x>b)(?<y>b)/.exec("bb").groups).join()`, "y,x"},
		{`"b".replace(/(?<x>a)|(?<x>b)/, "[$<x>]")`, "[b]"},
		// A backreference means whichever group took part.
		{`String(/(?:(?<x>a)|(?<x>b))\k<x>/.test("bb"))`, "true"},
		{`String(/(?:(?<x>a)|(?<x>b))\k<x>/.test("ba"))`, "false"},
		{`JSON.stringify(/(?<x>a)|(?<x>b)/d.exec("b").indices.groups.x)`, "[0,1]"},
		{`JSON.stringify(/(?:(?<x>a)|(?<y>a)(?<x>b))(?:(?<z>c)|(?<z>d))/.exec("abd").groups)`,
			`{"x":"b","y":"a","z":"d"}`},
		// Two groups that can both take part still may not share a name.
		{`try { new RegExp("(?<x>a)(?<x>b)") } catch (e) { e.constructor.name }`, "SyntaxError"},
		{`try { new RegExp("(?<x>a)|((?<x>b)(?<x>c))") } catch (e) { e.constructor.name }`, "SyntaxError"},
		{`try { new RegExp("(?:(?<x>a)|b)(?<x>c)") } catch (e) { e.constructor.name }`, "SyntaxError"},
	})
}

// TestRegExpLegacyStatics pins RegExp.$1 and the rest: what the last match of
// a RegExp of the intrinsic constructor left, read through RegExp itself.
func TestRegExpLegacyStatics(t *testing.T) {
	evalCases(t, []struct{ src, want string }{
		{`[RegExp.$1, RegExp.lastMatch, RegExp.input].join("|")`, "||"},
		{`/(a)(b)?(c)/.exec("xacz"); [RegExp.$1, RegExp.$2, RegExp.$3, RegExp.$4, RegExp.lastMatch, RegExp["$&"],
		   RegExp.lastParen, RegExp.leftContext, RegExp["$'"], RegExp.input, RegExp.$_].join("|")`,
			"a||c||ac|ac|c|x|z|xacz|xacz"},
		{`"hello world".replace(/o (w)/, "_"); [RegExp.$1, RegExp.leftContext].join()`, "w,hell"},
		{`RegExp.input = 5; [RegExp.input, typeof RegExp.$_].join()`, "5,string"},
		// Only RegExp itself answers, and a subclass's match leaves nothing
		// to answer with.
		{`var errs = []; class R extends RegExp {}
		  for (var f of [() => R.$1, () => Object.getOwnPropertyDescriptor(RegExp, "$1").get.call({})]) {
		    try { f() } catch (e) { errs.push(e.constructor.name) }
		  }
		  new R("x").exec("x"); try { RegExp.lastMatch } catch (e) { errs.push(e.constructor.name) }
		  /y/.exec("y"); errs.push(RegExp.lastMatch); errs.join()`, "TypeError,TypeError,TypeError,y"},
		// compile reinitializes in place, and refuses a subclass's instance.
		{`var re = /a/g; re.lastIndex = 3; var same = re.compile("b", "i") === re;
		   [same, re.source, re.flags, re.lastIndex, re.test("B")].join()`, "true,b,i,0,true"},
		{`class R extends RegExp {} try { new R("a").compile("b") } catch (e) { e.constructor.name }`, "TypeError"},
		{`Object.getOwnPropertyDescriptor(RegExp, "$1").set === undefined &&
		  typeof Object.getOwnPropertyDescriptor(RegExp, "input").set`, "function"},
	})
}

// The built-in symbol methods take shortcuts for a RegExp nothing a script
// wrote can reach: lastIndex read and written at its slot, a global match
// found without exec's arrays, a split searched with the RegExp's own pattern
// rather than the sticky copy of it the method makes, replace told the flags
// it was found with. Each answer here is Node's, and each shortcut must give
// way where a script could see the difference: a lastIndex that cannot be
// written, an exec, flags getter, species, constructor or Symbol.match a
// script replaced, a limit whose conversion runs a script.
func TestRegExpBuiltinShortcuts(t *testing.T) {
	evalCases(t, []struct{ src, want string }{
		// lastIndex is converted, written and refused as an ordinary property.
		{`var n = 0, r = /a/g; r.lastIndex = {valueOf() { n++; return 2 }};
		  var m = r.exec("aaaa"); [m.index, r.lastIndex, n].join()`, "2,3,1"},
		{`var r = /a/g; r.lastIndex = 1e20; [r.test("aaa"), r.lastIndex].join()`, "false,0"},
		{`var r = /b/y; r.lastIndex = 1; [r.test("abc"), r.lastIndex, r.test("abc"), r.lastIndex].join()`,
			"true,2,false,0"},
		{`var r = /a/g; r.foo = 1; r.lastIndex = 1; [r.exec("aba").index, r.lastIndex].join()`, "2,3"},
		{`var r = /a/; Object.freeze(r); r.exec("bab")[0]`, "a"},
		{`var errs = [];
		  for (var f of [r => r.exec("bab"), r => "aa".match(r), r => "aa".replace(r, "b")]) {
		    var r = /a/g; Object.freeze(r);
		    try { f(r) } catch (e) { errs.push(e.constructor.name) }
		  }
		  var r = /x/y; Object.defineProperty(r, "lastIndex", {writable: false});
		  try { r.test("bab") } catch (e) { errs.push(e.constructor.name) }
		  errs.join()`, "TypeError,TypeError,TypeError,TypeError"},

		// A global match ends with lastIndex at 0 and the last match in the
		// legacy statics, and steps past an empty match by code point under u.
		{`var r = /a(b)?/g; r.lastIndex = 5;
		  JSON.stringify(["xabaab".match(r), r.lastIndex, RegExp.lastMatch, RegExp.$1])`,
			`[["ab","a","ab"],0,"ab","b"]`},
		{`var r = /z/g; r.lastIndex = 3; JSON.stringify(["abc".match(r), r.lastIndex])`, "[null,0]"},
		{`"\u{1F600}a\u{1F600}".match(/(?:)/gu).length + "," + "\u{1F600}a\u{1F600}".match(/(?:)/g).length`, "4,6"},
		{`"a1b2".match(/(?<d>\d)/g).join()`, "1,2"},
		{`var r = /a/g; r.compile("b", "g"); "abab".match(r).join()`, "b,b"},
		{`var save = RegExp.prototype.exec, n = 0;
		  RegExp.prototype.exec = function (s) { n++; return save.call(this, s) };
		  var m = "aXa".match(/a/g); RegExp.prototype.exec = save; m.join() + "," + n`, "a,a,3"},
		{`var n = 0, d = Object.getOwnPropertyDescriptor(RegExp.prototype, "global");
		  Object.defineProperty(RegExp.prototype, "global", {get() { n++; return d.get.call(this) }, configurable: true});
		  var m = "aa".match(/a/g); m.join() + "," + n`, "a,a,1"},

		// replace leaves lastIndex alone without g, and at 0 with it.
		{`var r = /a/g; r.lastIndex = 2; ["banana".replace(r, "o"), r.lastIndex].join()`, "bonono,0"},
		{`var r = /a/; r.lastIndex = 2; ["banana".replace(r, "o"), r.lastIndex, RegExp.leftContext].join()`,
			"bonana,2,b"},

		// split: pieces, captures, limits and the legacy statics as the
		// sticky copy would have left them, and rx's lastIndex untouched.
		{`JSON.stringify(["a1b22c".split(/(\d)+/), RegExp.lastMatch, RegExp.$1, RegExp.rightContext])`,
			`[["a","1","b","2","c"],"22","2","c"]`},
		{`JSON.stringify(["a,b,c,d".split(/,/, 2), "a,b".split(/,/, 0), "a,b".split(/,/, -1), "a,b".split(/,/, 2.7)])`,
			`[["a","b"],[],["a","b"],["a","b"]]`},
		{`JSON.stringify(["".split(/x/), "".split(/(?:)/)])`, `[[""],[]]`},
		{`var r = /,/g; r.lastIndex = 3; "a,b".split(r); r.lastIndex`, "3"},
		{`JSON.stringify(["a,b,c".split(/,/y), "a,b,c".split(/b/y)])`, `[["a","b","c"],["a,",",c"]]`},
		{`"\u{1F600}\u{1F600}".split(/(?:)/u).length`, "2"},
		{`var r = /a/; r.compile(","); "1,2".split(r).join("|")`, "1|2"},
		{`class R extends RegExp {} var r = new R(","); Object.setPrototypeOf(r, RegExp.prototype);
		  /q(w)/.exec("qw"); "x,y".split(r).join() + "|" + RegExp.lastMatch`, "x,y|,"},
		// What making the copy reads is read where a script replaced it.
		{`var n = 0, d = Object.getOwnPropertyDescriptor(RegExp, Symbol.species);
		  Object.defineProperty(RegExp, Symbol.species, {get() { n++; return RegExp }, configurable: true});
		  var p = "a,b".split(/,/); Object.defineProperty(RegExp, Symbol.species, d); p.join() + "," + n`, "a,b,1"},
		{`var n = 0, save = RegExp.prototype.constructor;
		  RegExp.prototype.constructor = function () {};
		  RegExp.prototype.constructor[Symbol.species] = function (p, f) { n++; return new RegExp(p, f) };
		  var p = "a,b".split(/,/); RegExp.prototype.constructor = save; p.join() + "," + n`, "a,b,1"},
		{`var n = 0, d = Object.getOwnPropertyDescriptor(RegExp.prototype, Symbol.match);
		  Object.defineProperty(RegExp.prototype, Symbol.match, {get() { n++; return d.value }, configurable: true});
		  var p = "a,b".split(/,/); Object.defineProperty(RegExp.prototype, Symbol.match, d); p.join() + "," + n`, "a,b,1"},
		// A limit is converted after the copy is made, and what its valueOf
		// does to exec is what the copy's search sees.
		{`var save = RegExp.prototype.exec, n = 0;
		  var lim = {valueOf() { RegExp.prototype.exec = function (s) { n++; return save.call(this, s) }; return 9 }};
		  var p = "a,b".split(/,/, lim); RegExp.prototype.exec = save; p.join() + "," + n`, "a,b,3"},
	})
}
