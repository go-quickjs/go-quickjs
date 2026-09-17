package quickjs_test

import (
	"runtime"
	"strings"
	"testing"
	"time"

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
		// A reference whose target is still reachable always derefs to it.
		{`var o = {a: 1}; String(new WeakRef(o).deref().a)`, "1"},
		{`var o = {}; String(new WeakRef(o).deref() === o)`, "true"},
		{`Object.prototype.toString.call(new WeakRef({}))`, "[object WeakRef]"},
		{`String(new WeakRef(Symbol("x")).deref().description)`, "x"},

		{`var r = new FinalizationRegistry(() => {}); var t = {};
		  r.register(t, "held", t); String(r.unregister(t))`, "true"},
		// Two derefs in one turn answer the same way, whatever the collector
		// does in between.
		{`var ref = new WeakRef({}); String(ref.deref() === ref.deref())`, "true"},
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

// Every collection constructor takes its contents from an iterable, and
// undefined means an empty one.
func TestCollectionConstructorSources(t *testing.T) {
	cases := []struct{ src, want string }{
		{`var k = {}; String(new WeakMap([[k, 1]]).get(k))`, "1"},
		{`var s = Symbol(); String(new WeakMap([[s, 1]]).get(s))`, "1"},
		{`var k = {}; String(new WeakSet([k]).has(k))`, "true"},
		{`String(new Map([[1, 2]]).get(1))`, "2"},
		{`[...new Set([1, 2])].join(",")`, "1,2"},
		{`String(new WeakMap().has({})) + "," + String(new Map(undefined).size)`, "false,0"},
		{`String(new Set(null).size)`, "0"},
		// A Map takes any iterable of entries, not just an array of arrays.
		{`String(new Map(new Map([[1, 2]])).get(1))`, "2"},
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
		// A weak key has to be something that could be collected.
		`new WeakMap([[1, 2]])`,
		`new WeakSet([1])`,
		`new WeakMap([[Symbol.for("registered"), 1]])`,
		// The argument has to be iterable.
		`new WeakMap(1)`,
		`new Map(1)`,
		`new Set(1)`,
		// And its entries have to be objects.
		`new Map([1])`,
	}
	for _, src := range bad {
		rt := quickjs.New()
		if _, err := rt.Eval(src); err == nil {
			t.Errorf("%s: no error, want TypeError", src)
		}
		rt.Close()
	}
}

// A weak reference is one the collector may ignore. Go gained the pieces needed
// to mean that in 1.24 -- weak.Pointer and runtime.AddCleanup -- so these are
// the real thing rather than strong references wearing the name.
//
// The test drives Go's collector directly, because there is no way to ask for
// one from JavaScript and no guarantee about when one happens.
func TestWeakReferencesAreWeak(t *testing.T) {
	rt := quickjs.New()
	defer rt.Close()

	if _, err := rt.Eval(`
		var ref, kept = {}, keptRef = new WeakRef(kept);
		(function () { var o = {}; ref = new WeakRef(o); })();
	`); err != nil {
		t.Fatal(err)
	}
	// The target was kept alive for the turn it was made in, which is what
	// stops a reference being created and found empty in the same breath.
	if v, err := rt.Eval(`ref.deref() ? "live" : "cleared"`); err != nil {
		t.Fatal(err)
	} else if v.String() != "live" {
		t.Errorf("before collection = %q, want live", v.String())
	}

	for i := 0; i < 4; i++ {
		runtime.GC()
	}

	v, err := rt.Eval(`ref.deref() ? "live" : "cleared"`)
	if err != nil {
		t.Fatal(err)
	}
	if v.String() != "cleared" {
		t.Errorf("after collection = %q, want cleared", v.String())
	}
	// One that is still reachable must not clear.
	if v, err := rt.Eval(`keptRef.deref() === kept ? "live" : "cleared"`); err != nil {
		t.Fatal(err)
	} else if v.String() != "live" {
		t.Errorf("a reachable target = %q, want live", v.String())
	}
}

// A FinalizationRegistry is told when a target has gone, as a job of its own --
// the collector reports it on another goroutine, and running JavaScript there
// would not be safe.
func TestFinalizationRegistryIsCalled(t *testing.T) {
	rt := quickjs.New()
	defer rt.Close()

	if _, err := rt.Eval(`
		var seen = [];
		var reg = new FinalizationRegistry(h => seen.push(h));
		var live = {};
		reg.register(live, "live-held");
		(function () { var t = {}; reg.register(t, "held"); })();
	`); err != nil {
		t.Fatal(err)
	}

	// The cleanup runs on its own goroutine at a moment the collector chooses,
	// so this waits for it rather than assuming it has happened.
	deadline := time.Now().Add(5 * time.Second)
	for {
		for i := 0; i < 4; i++ {
			runtime.GC()
		}
		runtime.Gosched()
		time.Sleep(5 * time.Millisecond)
		if _, err := rt.Eval(`0`); err != nil {
			t.Fatal(err)
		}
		v, err := rt.Eval(`seen.join(",")`)
		if err != nil {
			t.Fatal(err)
		}
		if v.String() == "held" {
			break
		}
		if v.String() != "" {
			t.Fatalf("callback saw %q, want held", v.String())
		}
		if time.Now().After(deadline) {
			t.Fatal("the finalization callback never ran")
		}
	}
	// The registration whose target is still reachable must not have fired.
	runtime.KeepAlive(rt)
}
