package quickjs_test

import (
	"strings"
	"testing"

	quickjs "github.com/go-quickjs/go-quickjs"
)

// slice, map, filter, concat and splice all build a new array, and all five ask
// the receiver what kind of array that should be. A subclass gets back an
// instance of itself.
func TestArraySpecies(t *testing.T) {
	cases := []struct{ src, want string }{
		{`class A extends Array {} String(new A(1, 2).slice(0) instanceof A)`, "true"},
		{`class A extends Array {} String(new A(1, 2).map(x => x) instanceof A)`, "true"},
		{`class A extends Array {} String(new A(1, 2).filter(() => true) instanceof A)`, "true"},
		{`class A extends Array {} String(new A(1, 2).concat([3]) instanceof A)`, "true"},
		{`class A extends Array {} String(new A(1, 2).splice(0, 1) instanceof A)`, "true"},
		{`class A extends Array {} new A(1, 2, 3).slice(1).join(",")`, "2,3"},

		// A species of null means a plain array, which is how a subclass opts
		// out of having its methods return instances of itself.
		{`class A extends Array { static get [Symbol.species]() { return null; } }
		  var r = new A(1, 2).map(x => x);
		  [r instanceof A, Array.isArray(r)].join(",")`, "false,true"},
		// And any constructor at all may be named.
		{`function C(n) { this.n = n; }
		  var a = [1, 2]; a.constructor = {[Symbol.species]: C};
		  var r = a.map(x => x * 2);
		  [r instanceof C, r.n, r[0], r[1]].join(",")`, "true,2,2,4"},
		// The result is filled through the property protocol, so a setter on
		// the species' prototype sees each element.
		{`var seen = [];
		  function C() {}
		  Object.defineProperty(C.prototype, "0", {set(v) { seen.push(v); }});
		  var a = [7]; a.constructor = {[Symbol.species]: C};
		  a.map(x => x); seen.length + ""`, "0"},

		// A length beyond what a length may hold is refused before anything is
		// copied, rather than after memory has run out.
		{`var o = {length: 4294967296, slice: Array.prototype.slice};
		  try { o.slice(0, 4294967296) } catch (e) { e.constructor.name }`, "RangeError"},
		{`var o = {length: 4294967296, map: Array.prototype.map};
		  try { o.map(x => x) } catch (e) { e.constructor.name }`, "RangeError"},
		{`try { new Array(4294967296) } catch (e) { e.constructor.name }`, "RangeError"},

		// concat asks each argument whether it should be flattened, so an
		// array-like can join in and an array can opt out.
		{`var a = [1]; a[Symbol.isConcatSpreadable] = false; String([].concat(a).length)`, "1"},
		{`var o = {length: 2, 0: "a", 1: "b", [Symbol.isConcatSpreadable]: true};
		  [].concat(o).join(",")`, "a,b"},

		// The ordinary behaviour is unchanged.
		{`[1, 2, 3].slice(1).join(",")`, "2,3"},
		{`[1, 2, 3].map(x => x * 2).join(",")`, "2,4,6"},
		{`[1, 2, 3].filter(x => x > 1).join(",")`, "2,3"},
		{`[1, 2].concat([3, 4], 5).join(",")`, "1,2,3,4,5"},
		{`String([1, 2].concat([, 3]).length)`, "4"},
		// A hole stays a hole rather than becoming an undefined element.
		{`String([, 1].map(x => x).hasOwnProperty(0))`, "false"},
		{`String([, 1].slice(0).hasOwnProperty(0))`, "false"},
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
		`var a = [1]; a.constructor = {[Symbol.species]: 1}; a.map(x => x)`,
		`var a = [1]; a.constructor = {[Symbol.species]: {}}; a.slice(0)`,
		`var a = [1]; a.constructor = 1; a.map(x => x)`,
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

// splice and copyWithin work through the property protocol rather than the
// dense element slice, so they apply to an array-like, to a sparse array and to
// one whose elements have moved out of dense storage.
func TestSpliceAndCopyWithin(t *testing.T) {
	cases := []struct{ src, want string }{
		{`var a = [1, 2, 3, 4]; var r = a.splice(1, 2, "x");
		  a.join(",") + "|" + r.join(",")`, "1,x,4|2,3"},
		{`var a = [1, 2, 3]; a.splice(1, 0, "x", "y"); a.join(",")`, "1,x,y,2,3"},
		{`var a = [1, 2, 3]; a.splice(1); a.join(",")`, "1"},
		// With no arguments at all nothing is removed, which differs from
		// removing zero.
		{`var a = [1, 2, 3]; a.splice(); a.join(",")`, "1,2,3"},
		{`var a = [1, 2, 3]; a.splice(-1).join(",")`, "3"},
		{`var a = [1, 2, 3]; a.splice(0, 99).join(",") + "|" + a.length`, "1,2,3|0"},

		{`var o = {0: "a", 1: "b", length: 2};
		  Array.prototype.splice.call(o, 0, 1).join(",") + "|" + o.length`, "a|1"},
		{`var a = [1, 2, 3]; Object.freeze(a);
		  try { a.splice(0, 1) } catch (e) { e.constructor.name }`, "TypeError"},

		{`[1, 2, 3, 4, 5].copyWithin(0, 3).join(",")`, "4,5,3,4,5"},
		{`[1, 2, 3, 4, 5].copyWithin(1, 3, 4).join(",")`, "1,4,3,4,5"},
		{`[1, 2, 3, 4, 5].copyWithin(0, -2, -1).join(",")`, "4,2,3,4,5"},
		// Overlapping the other way has to copy backwards, or an element would
		// be overwritten before it was read.
		{`[1, 2, 3, 4, 5].copyWithin(2, 0).join(",")`, "1,2,1,2,3"},
		{`var o = {0: "a", 1: "b", 2: "c", length: 3};
		  Array.prototype.copyWithin.call(o, 0, 1);
		  [o[0], o[1], o[2]].join(",")`, "b,c,c"},
		{`var a = [1, 2, 3]; Object.freeze(a);
		  try { a.copyWithin(0, 1) } catch (e) { e.constructor.name }`, "TypeError"},
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

// An array's length is a number rather than a count of what is present: setting
// it beyond the highest index does not materialize anything, and it cannot be
// derived back from the keys.
func TestSparseArrayLength(t *testing.T) {
	cases := []struct{ src, want string }{
		{`var a = []; a.length = 4294967295; String(a.length)`, "4294967295"},
		{`String(new Array(4294967295).length)`, "4294967295"},
		{`var a = [1]; a.length = 100000; String(a.length)`, "100000"},
		{`var a = [1]; a.length = 100000; String(a.hasOwnProperty(50000))`, "false"},
		{`var a = []; a[1000000] = 1; String(a.length)`, "1000001"},
		{`var a = [1, 2, 3]; delete a[2]; String(a.length)`, "3"},
		{`var a = [1, 2, 3]; a.length = 1; a.join(",")`, "1"},
		{`var a = []; a.length = 5; a.length = 0; String(a.length)`, "0"},
		{`var a = []; a.length = 5; String(Object.keys(a).length)`, "0"},
		// Freezing moves the elements out of dense storage, which must not
		// change the length.
		{`var a = [1, 2]; Object.freeze(a); String(a.length)`, "2"},
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

// A combinator resolves each element as it arrives rather than draining the
// iterable first, because a resolve that throws has to stop the iteration --
// an iterator that never finishes on its own would otherwise be walked forever.
func TestPromiseCombinatorStopsOnError(t *testing.T) {
	rt := quickjs.New()
	defer rt.Close()

	v, err := rt.Eval(`
		var closed = 0;
		var endless = {
		  [Symbol.iterator]: () => ({
		    next: () => ({value: null, done: false}),
		    return() { closed++; return {}; },
		  }),
		};
		Promise.resolve = function () { throw new TypeError("no"); };
		Promise.all(endless);
		String(closed);
	`)
	if err != nil {
		t.Fatal(err)
	}
	if v.String() != "1" {
		t.Errorf("the iterator should have been closed once, got %s", v.String())
	}
}
