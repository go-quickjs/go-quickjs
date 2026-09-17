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

// TestCollectionConstructorUsesAdder covers how Map, Set, WeakMap and WeakSet
// fill themselves from an iterable: through the method the object itself has,
// so that a subclass overriding it sees every entry go by.
func TestCollectionConstructorUsesAdder(t *testing.T) {
	cases := []struct{ src, want string }{
		{`var log = []
		  class M extends Map { set(k, v) { log.push(k); return super.set(k, v) } }
		  var m = new M([[1, "a"], [2, "b"]])
		  log.join() + "|" + m.get(2)`, "1,2|b"},
		{`var log = []
		  class S extends Set { add(v) { log.push(v); return super.add(v) } }
		  var s = new S([1, 2])
		  log.join() + "|" + s.has(2)`, "1,2|true"},
		{`var log = []
		  class W extends WeakMap { set(k, v) { log.push(v); return super.set(k, v) } }
		  var k1 = {}, k2 = {}
		  var w = new W([[k1, "a"], [k2, "b"]])
		  log.join() + "|" + w.get(k2)`, "a,b|b"},
		{`var log = []
		  class W extends WeakSet { add(v) { log.push(1); return super.add(v) } }
		  var w = new W([{}, {}])
		  log.join()`, "1,1"},

		// The method has to be there and be callable, and a failure reading it
		// is the constructor's failure.
		{`class M extends Map { get set() { return 1 } }
		  try { new M([]) } catch (e) { e.constructor.name }`, "TypeError"},
		{`class S extends Set { get add() { throw new RangeError() } }
		  try { new S([]) } catch (e) { e.constructor.name }`, "RangeError"},
		// With no iterable the method is never looked at.
		{`class M extends Map { get set() { throw new RangeError() } }
		  String(new M() instanceof M)`, "true"},

		// A registered symbol can be neither a weak key nor a weak value.
		{`var w = new WeakMap()
		  try { w.set(Symbol.for("x"), 1) } catch (e) { e.constructor.name }`, "TypeError"},
		{`var w = new WeakSet()
		  try { w.add(Symbol.for("x")) } catch (e) { e.constructor.name }`, "TypeError"},
		{`var w = new WeakSet(); var s = Symbol("x"); w.add(s); String(w.has(s))`, "true"},
	}
	for _, tc := range cases {
		checkEval(t, tc.src, tc.want)
	}
}

// TestIteratorMethodIdentity covers the methods a collection shares between two
// names, which a script can compare.
func TestIteratorMethodIdentity(t *testing.T) {
	cases := []struct{ src, want string }{
		{`String(Map.prototype[Symbol.iterator] === Map.prototype.entries)`, "true"},
		{`String(Set.prototype[Symbol.iterator] === Set.prototype.values)`, "true"},
		{`String(Set.prototype.keys === Set.prototype.values)`, "true"},
		{`String(Array.prototype[Symbol.iterator] === Array.prototype.values)`, "true"},
		{`[...new Set([1, 2]).keys()].join()`, "1,2"},
		{`[...new Map([[1, 2]])].map(e => e.join(":")).join()`, "1:2"},
		{`Set.prototype[Symbol.iterator].name`, "values"},

		// An error prototype is an ordinary object; an error is not.
		{`Object.prototype.toString.call(RangeError.prototype)`, "[object Object]"},
		{`Object.prototype.toString.call(Error.prototype)`, "[object Object]"},
		{`Object.prototype.toString.call(new RangeError())`, "[object Error]"},
		{`String(Error.isError(RangeError.prototype))`, "false"},

		// Every async iterator is its own iterable, through one shared method.
		{`var g = async function* () {}()
		  var p = Object.getPrototypeOf(Object.getPrototypeOf(Object.getPrototypeOf(g)))
		  typeof p[Symbol.asyncIterator]`, "function"},
		{`var g = async function* () {}()
		  String(g[Symbol.asyncIterator]() === g)`, "true"},
	}
	for _, tc := range cases {
		checkEval(t, tc.src, tc.want)
	}
}

// A set-like argument is walked a step at a time, and the receiver is read as
// it is when each step happens: the argument's own methods may change either
// collection while the operation runs, and the specification says what each one
// sees.
func TestSetOperationsWalkStepByStep(t *testing.T) {
	const setLike = `
		function observable(values, log) {
			var index = 0
			return {
				size: values.length,
				has: function (v) { log.push("has " + v); return values.indexOf(v) >= 0 },
				keys: function () {
					log.push("keys")
					return {
						next: function () {
							log.push("next")
							return {done: index >= values.length, value: values[index++]}
						},
						return: function () { log.push("return"); return {} },
					}
				},
			}
		}
	`
	cases := []struct{ name, src, want string }{
		// An answer that is settled stops the walk and closes the iterator.
		{"disjoint stops early", setLike + `var log = []
		  var s = new Set(["a", "b", "c", "d"])
		  String(s.isDisjointFrom(observable(["x", "a", "y"], log))) + "|" + log.join(",")`,
			"false|keys,next,next,return"},
		{"superset stops early", setLike + `var log = []
		  var s = new Set(["a", "b", "c"])
		  String(s.isSupersetOf(observable(["a", "z", "b"], log))) + "|" + log.join(",")`,
			"false|keys,next,next,return"},
		{"superset runs out", setLike + `var log = []
		  var s = new Set(["a", "b", "c"])
		  String(s.isSupersetOf(observable(["a", "b"], log))) + "|" + log.join(",")`,
			"true|keys,next,next,next"},
		// A smaller receiver is probed rather than the argument walked.
		{"disjoint probes", setLike + `var log = []
		  var s = new Set(["a"])
		  String(s.isDisjointFrom(observable(["x", "y"], log))) + "|" + log.join(",")`,
			"true|has a"},
		// union takes the receiver's members before the argument is walked.
		{"union copies first", setLike + `var s = new Set(["a", "b"])
		  var evil = {size: 1, has: function () { return false },
		    keys: function () {
		      var done = false
		      return {next: function () { s.delete("b"); var d = done; done = true
		        return {done: d, value: "z"} }}
		    }};
		  [...s.union(evil)].join(",")`, "a,b,z"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) { checkEval(t, tc.src, tc.want) })
	}
}
