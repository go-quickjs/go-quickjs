package quickjs_test

import (
	"strings"
	"testing"

	quickjs "github.com/go-quickjs/go-quickjs"
)

func TestSetOperations(t *testing.T) {
	cases := []struct{ src, want string }{
		{`[...new Set([1,2,3]).union(new Set([3,4]))].join(",")`, "1,2,3,4"},
		{`[...new Set([1,2,3]).intersection(new Set([2,3,4]))].join(",")`, "2,3"},
		{`[...new Set([1,2,3]).difference(new Set([2]))].join(",")`, "1,3"},
		{`[...new Set([1,2,3]).symmetricDifference(new Set([3,4]))].join(",")`, "1,2,4"},
		{`String(new Set([1,2]).isSubsetOf(new Set([1,2,3])))`, "true"},
		{`String(new Set([1,2,4]).isSubsetOf(new Set([1,2,3])))`, "false"},
		{`String(new Set([1,2,3]).isSupersetOf(new Set([1,2])))`, "true"},
		{`String(new Set([1,2,3]).isSupersetOf(new Set([1,5])))`, "false"},
		{`String(new Set([1,2]).isDisjointFrom(new Set([3,4])))`, "true"},
		{`String(new Set([1,2]).isDisjointFrom(new Set([2,3])))`, "false"},

		// Anything set-like works as the argument, not just a Set. A Map
		// qualifies: it has size, has and keys.
		{`[...new Set([1,2,3]).intersection(new Map([[2,"a"],[3,"b"]]))].join(",")`, "2,3"},
		{`[...new Set([1,2]).union({size: 1, has: () => true,
		   keys: () => [9][Symbol.iterator]()})].join(",")`, "1,2,9"},

		// Which side is walked depends on the sizes, and the result's order
		// follows whichever it was -- observable, and so specified.
		{`[...new Set([1,2,3,4,5]).intersection(new Set([4,2]))].join(",")`, "4,2"},
		{`[...new Set([2,4]).intersection(new Set([1,2,3,4,5]))].join(",")`, "2,4"},

		// A Set holds +0 and -0 as one key, so a -0 arriving from the argument
		// must normalize.
		{`String(Object.is([...new Set([1]).union({size: 1, has: () => true,
		   keys: () => [-0][Symbol.iterator]()})][1], 0))`, "true"},

		// Every operation returns a new Set and leaves both operands alone.
		{`var a = new Set([1,2]); a.union(new Set([3])); [...a].join(",")`, "1,2"},
		{`var a = new Set([1,2]); var b = a.union(new Set([3])); String(a === b)`, "false"},
		{`String(new Set([1]).union(new Set([2])) instanceof Set)`, "true"},

		{`[Set.prototype.union.length, Set.prototype.isSubsetOf.length].join(",")`, "1,1"},
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

func TestSetOperationErrors(t *testing.T) {
	cases := []struct{ src, want string }{
		// An array is not set-like: it has no size, has or keys in the right
		// shape.
		{`new Set().union([1,2])`, "TypeError"},
		{`new Set().union({size: NaN, has() {}, keys() {}})`, "TypeError"},
		{`new Set().union({size: 1, has: 1, keys() {}})`, "TypeError"},
		{`new Set().union({size: 1, has() {}, keys: 1})`, "TypeError"},
		{`new Set().union({size: -1, has() {}, keys() {}})`, "RangeError"},
		{`new Set().union(1)`, "TypeError"},
		// The receiver is checked before the argument.
		{`Set.prototype.union.call([], new Set())`, "TypeError"},
		{`Set.prototype.union.call(new Map(), new Set())`, "TypeError"},
	}

	for _, tc := range cases {
		rt := quickjs.New()
		_, err := rt.Eval(tc.src)
		if err == nil {
			t.Errorf("%s: no error, want %s", tc.src, tc.want)
		} else if !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: got %v, want %s", tc.src, err, tc.want)
		}
		rt.Close()
	}
}

func TestWeakRefAndFinalizationRegistry(t *testing.T) {
	cases := []struct{ src, want string }{
		// Collection is never required, so a WeakRef that still derefs to its
		// target is conforming; this engine's always does.
		{`var o = {a: 1}; String(new WeakRef(o).deref().a)`, "1"},
		{`var o = {}; String(new WeakRef(o).deref() === o)`, "true"},
		{`Object.prototype.toString.call(new WeakRef({}))`, "[object WeakRef]"},
		{`String(new WeakRef(Symbol("x")).deref().description)`, "x"},

		{`var r = new FinalizationRegistry(() => {}); var t = {};
		  r.register(t, "held", t); String(r.unregister(t))`, "true"},
		{`var r = new FinalizationRegistry(() => {});
		  String(r.unregister({}))`, "false"},
		{`Object.prototype.toString.call(new FinalizationRegistry(() => {}))`,
			"[object FinalizationRegistry]"},
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
		`new WeakRef(1)`,
		`new WeakRef("s")`,
		// A registered symbol lives forever, so a weak reference to one is
		// meaningless and rejected.
		`new WeakRef(Symbol.for("registered"))`,
		`WeakRef({})`,
		`new FinalizationRegistry(1)`,
		`FinalizationRegistry(() => {})`,
		// Holding the target as its own held value would keep it alive.
		`var t = {}; new FinalizationRegistry(() => {}).register(t, t)`,
		`new FinalizationRegistry(() => {}).register(1, "held")`,
	}
	for _, src := range bad {
		rt := quickjs.New()
		if _, err := rt.Eval(src); err == nil {
			t.Errorf("%s: no error, want TypeError", src)
		} else if !strings.Contains(err.Error(), "TypeError") {
			t.Errorf("%s: got %v, want TypeError", src, err)
		}
		rt.Close()
	}
}

func TestRecentBuiltins(t *testing.T) {
	cases := []struct{ src, want string }{
		// Promise.try runs its function now, unlike Promise.resolve().then.
		{`var order = []; Promise.try(() => order.push("sync"));
		  order.push("after"); order.join(",")`, "sync,after"},
		{`var out = "";
		  Promise.try(() => { throw new TypeError("x"); }).catch(e => { out = e.name; });
		  Promise.resolve().then(() => {}).then(() => out)`, "[object Promise]"},
		{`String(Promise.try(() => 1) instanceof Promise)`, "true"},

		// instanceof can be forged; the internal slot cannot.
		{`[Error.isError(new TypeError()), Error.isError({}),
		   Error.isError(Object.create(Error.prototype))].join(",")`, "true,false,false"},

		{`RegExp.escape("hello.world")`, `\x68ello\.world`},
		{`RegExp.escape("^$\\.*+?()[]{}|/")`, `\^\$\\\.\*\+\?\(\)\[\]\{\}\|\/`},
		{`RegExp.escape("\t\n")`, `\t\n`},
		// The point of it: the result matches the literal text and nothing else.
		{`[new RegExp(RegExp.escape("a.b")).test("a.b"),
		   new RegExp(RegExp.escape("a.b")).test("axb")].join(",")`, "true,false"},

		// One rounding at the end, rather than one per addition.
		{`String(Math.sumPrecise([1e20, 0.1, -1e20]))`, "0.1"},
		{`String([1e20, 0.1, -1e20].reduce((a, b) => a + b, 0))`, "0"},
		{`String(Math.sumPrecise([1, 2, 3]))`, "6"},
		// The sum of nothing is -0, so that adding -0 to it is still -0.
		{`String(Object.is(Math.sumPrecise([]), -0))`, "true"},
		{`String(Math.sumPrecise([Infinity, -Infinity]))`, "NaN"},
		{`String(Math.sumPrecise([Infinity, 1]))`, "Infinity"},
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
		// Deliberately not ToString: escaping a number would silently produce
		// a pattern the caller never wrote.
		`RegExp.escape(1)`,
		`RegExp.escape({})`,
		`Math.sumPrecise(["a"])`,
		`Math.sumPrecise(1)`,
	}
	for _, src := range bad {
		rt := quickjs.New()
		if _, err := rt.Eval(src); err == nil {
			t.Errorf("%s: no error, want TypeError", src)
		}
		rt.Close()
	}
}
