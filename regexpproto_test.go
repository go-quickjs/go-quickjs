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
