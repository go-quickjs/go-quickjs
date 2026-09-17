package quickjs_test

import (
	"strings"
	"testing"

	quickjs "github.com/go-quickjs/go-quickjs"
)

func TestTypedArrayMethods(t *testing.T) {
	cases := []struct{ src, want string }{
		// %TypedArray% is the prototype of all nine concrete constructors, and
		// is where the shared methods and statics live.
		{`Object.getPrototypeOf(Int8Array).name`, "TypedArray"},
		{`String(Object.getPrototypeOf(Int8Array) === Object.getPrototypeOf(Float64Array))`, "true"},
		{`String(Object.getPrototypeOf(Int8Array.prototype) ===
		         Object.getPrototypeOf(Uint8Array.prototype))`, "true"},

		{`new Uint8Array([1, 2, 3]).map(x => x * 2).join(",")`, "2,4,6"},
		// A view's map yields a view of the same kind, not a plain array.
		{`String(new Uint8Array([1, 2, 3]).map(x => x) instanceof Uint8Array)`, "true"},
		{`new Uint8Array([1, 2, 3]).filter(x => x > 1).join(",")`, "2,3"},
		{`String(new Uint8Array([1, 2, 3]).reduce((a, b) => a + b))`, "6"},
		{`String(new Uint8Array([1, 2, 3]).reduceRight((a, b) => a + b))`, "6"},
		{`new Uint8Array([1, 2, 3]).reverse().join(",")`, "3,2,1"},
		{`new Uint8Array([1, 2, 3]).toReversed().join(",")`, "3,2,1"},
		{`new Uint8Array([1, 2, 3]).with(1, 9).join(",")`, "1,9,3"},
		{`String(new Uint8Array([1, 2, 3]).at(-1))`, "3"},
		{`new Uint8Array([1, 2, 3, 4]).copyWithin(0, 2).join(",")`, "3,4,3,4"},
		{`String(new Uint8Array([1, 2, 3]).lastIndexOf(3))`, "2"},
		{`String(new Uint8Array([1, 2, 3]).findLast(x => x < 3))`, "2"},

		// A typed array sorts numerically by default, where an array sorts by
		// string.
		{`new Uint8Array([10, 9]).sort().join(",")`, "9,10"},
		{`[10, 9].sort().join(",")`, "10,9"},
		{`new Uint8Array([10, 9]).toSorted().join(",")`, "9,10"},

		{`Uint8Array.from([1, 2]).join(",")`, "1,2"},
		{`Uint8Array.from([1, 2], x => x * 3).join(",")`, "3,6"},
		{`Uint8Array.of(1, 2).join(",")`, "1,2"},
		{`String(Uint8Array.from([1]) instanceof Uint8Array)`, "true"},

		{`Array.from(new Uint8Array([1, 2]).entries()).length + ""`, "2"},
		{`Array.from(new Uint8Array([1, 2]).keys()).join(",")`, "0,1"},
		{`Array.from(new Uint8Array([1, 2]).values()).join(",")`, "1,2"},

		// The elements are own enumerable properties even though they live in a
		// buffer rather than in the property table.
		{`Object.keys(new Uint8Array([1, 2])).join(",")`, "0,1"},
		{`Object.getOwnPropertyNames(new Uint8Array([1, 2])).join(",")`, "0,1"},
		{`JSON.stringify(Object.getOwnPropertyDescriptor(new Uint8Array([7]), "0"))`,
			`{"value":7,"writable":true,"enumerable":true,"configurable":true}`},
		{`JSON.stringify(new Uint8Array([1, 2]))`, `{"0":1,"1":2}`},

		// The tag names the concrete type.
		{`Object.prototype.toString.call(new Uint8Array(1))`, "[object Uint8Array]"},
		{`Object.prototype.toString.call(new Float64Array(1))`, "[object Float64Array]"},
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

func TestBigIntBuiltins(t *testing.T) {
	cases := []struct{ src, want string }{
		{`typeof BigInt`, "function"},
		{`String(BigInt(1))`, "1"},
		{`String(BigInt("123"))`, "123"},
		{`String(BigInt(true)) + "," + String(BigInt(false))`, "1,0"},
		{`(10n).toString(2)`, "1010"},
		{`(255n).toString(16)`, "ff"},
		{`String((1n).valueOf())`, "1"},
		{`Object.prototype.toString.call(Object(1n))`, "[object BigInt]"},

		// asIntN and asUintN are how a program works with fixed-width
		// integers: the arithmetic is arbitrary-precision, and wrapping back is
		// an explicit step.
		{`String(BigInt.asIntN(8, 255n))`, "-1"},
		{`String(BigInt.asUintN(8, 255n))`, "255"},
		{`String(BigInt.asUintN(8, -1n))`, "255"},
		{`String(BigInt.asIntN(64, 2n ** 63n))`, "-9223372036854775808"},
		{`String(BigInt.asIntN(0, 5n))`, "0"},
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
		// Only an integer converts: rounding silently would be worse than
		// refusing.
		`BigInt(1.5)`,
		`BigInt(NaN)`,
		`BigInt(Infinity)`,
		`BigInt("nope")`,
		`BigInt(Symbol())`,
		// It is deliberately not a constructor.
		`new BigInt(1)`,
	}
	for _, src := range bad {
		rt := quickjs.New()
		if _, err := rt.Eval(src); err == nil {
			t.Errorf("%s: no error, want one", src)
		}
		rt.Close()
	}
}

// A callback receives the view itself as its third argument. A temporary array
// standing in for it would be observable, which is what several hundred
// conformance tests check.
func TestTypedArrayCallbackReceivesView(t *testing.T) {
	cases := []struct{ src, want string }{
		{`var seen; new Uint8Array([7]).forEach(function (v, i, a) { seen = a; });
		  String(seen instanceof Uint8Array)`, "true"},
		{`var seen; new Uint8Array([7]).map(function (v, i, a) { seen = a; return v; });
		  String(seen instanceof Uint8Array)`, "true"},
		{`var seen; new Uint8Array([7]).filter(function (v, i, a) { seen = a; return true; });
		  String(seen instanceof Uint8Array)`, "true"},
		{`var seen; new Uint8Array([7]).reduce(function (acc, v, i, a) { seen = a; return acc; }, 0);
		  String(seen instanceof Uint8Array)`, "true"},
		// And the receiver is the thisArg, not the view.
		{`var seen; new Uint8Array([7]).forEach(function () { seen = this.tag; }, {tag: "t"});
		  seen`, "t"},
		// Indices and values are the view's own.
		{`var out = []; new Uint8Array([9, 8]).forEach((v, i) => out.push(i + ":" + v));
		  out.join(",")`, "0:9,1:8"},
		{`new Uint8Array([1, 2, 3]).findLast(x => x < 3) + ""`, "2"},
		{`new Uint8Array([1, 2, 3]).findLastIndex(x => x < 3) + ""`, "1"},
		{`String(new Uint8Array([]).reduce((a, b) => a, 5))`, "5"},
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

	rt := quickjs.New()
	defer rt.Close()
	if _, err := rt.Eval(`new Uint8Array([]).reduce((a, b) => a)`); err == nil {
		t.Error("reduce of an empty typed array with no initial value should throw")
	}
}

// TestTypedArrayConstructorSources pins the three shapes the constructor
// accepts, in the order Array.from uses.
func TestTypedArrayConstructorSources(t *testing.T) {
	cases := []struct{ src, want string }{
		{`new Uint8Array([1, 2]).join(",")`, "1,2"},
		{`new Uint8Array(new Set([1, 2])).join(",")`, "1,2"},
		{`var it = {[Symbol.iterator]() { return [1, 2, 3][Symbol.iterator](); }};
		  new Uint8Array(it).join(",")`, "1,2,3"},
		// No iterator: the array-like protocol.
		{`new Uint8Array({length: 2, 0: 7, 1: 8}).join(",")`, "7,8"},
		{`String(new Uint8Array(3).length)`, "3"},
		// From another view, element by element -- so the conversion happens
		// per value rather than by reinterpreting the bytes.
		{`new Uint8Array(new Int32Array([300, 2])).join(",")`, "44,2"},
		{`new Int32Array(new Uint8Array([1, 2])).join(",")`, "1,2"},
		// A BigInt view converts each element with ToBigInt, so strings work.
		{`var it = {[Symbol.iterator]() { return ["0", "1"][Symbol.iterator](); }};
		  new BigInt64Array(it).join(",")`, "0,1"},
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

// An over-large view is refused before anything is allocated, whichever way its
// length was arrived at. Allocating first and failing afterwards means there is
// nothing left to fail with.
func TestTypedArrayLengthLimits(t *testing.T) {
	cases := []struct{ src, want string }{
		{`try { new Uint8Array(4294967296) } catch (e) { e.constructor.name }`, "RangeError"},
		{`try { new Uint8Array(-1) } catch (e) { e.constructor.name }`, "RangeError"},
		{`try { new Float64Array(2 ** 53) } catch (e) { e.constructor.name }`, "RangeError"},
		// From an array-like, the reported length decides before any element is
		// read.
		{`try { new Uint8Array({length: 4294967296}) } catch (e) { e.constructor.name }`,
			"RangeError"},
		{`try { new Float64Array({length: 4294967296}) } catch (e) { e.constructor.name }`,
			"RangeError"},

		// The ordinary sizes are unchanged.
		{`String(new Uint8Array(3).length)`, "3"},
		{`new Uint8Array({length: 2, 0: 7, 1: 8}).join(",")`, "7,8"},
		{`new Uint8Array([1, 2]).join(",")`, "1,2"},
		{`new Uint8Array(new Set([1, 2])).join(",")`, "1,2"},
		{`Uint8Array.from([1, 2]).join(",")`, "1,2"},
		// A reported length is honoured even where the elements are missing.
		{`String(new Uint8Array({length: 3}).join(","))`, "0,0,0"},
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

// An iterable is never asked how long it is, so what refuses an over-long one
// is the count of what it has produced -- which means the refusal arrives only
// after a great many elements, and has to arrive rather than not.
func TestTypedArrayFromEndlessIterable(t *testing.T) {
	if testing.Short() {
		t.Skip("the refusal only arrives after the element limit is reached")
	}
	rt := quickjs.New()
	defer rt.Close()

	_, err := rt.Eval(`new Uint8Array({[Symbol.iterator]: () => ({
	  next: () => ({value: 0, done: false}),
	})})`)
	if err == nil {
		t.Fatal("an endless source should have been refused")
	}
	if !strings.Contains(err.Error(), "RangeError") {
		t.Errorf("got %v, want RangeError", err)
	}
}

// slice, map, filter and subarray all produce a new view, and all four ask the
// receiver what kind it should be. Unlike the Array versions the answer is
// checked afterwards as well: a species may return any object at all, and
// writing elements into one that is not a view would be writing into nothing.
func TestTypedArraySpecies(t *testing.T) {
	cases := []struct{ src, want string }{
		{`class A extends Uint8Array {} String(new A([1, 2]).slice(0) instanceof A)`, "true"},
		{`class A extends Uint8Array {} String(new A([1, 2]).map(x => x) instanceof A)`, "true"},
		{`class A extends Uint8Array {}
		  String(new A([1, 2]).filter(() => true) instanceof A)`, "true"},
		{`class A extends Uint8Array {} var s = new A([1, 2, 3]).subarray(1);
		  [s instanceof A, s.length].join(",")`, "true,2"},
		// subarray shares the buffer, unlike slice, which copies.
		{`var a = new Uint8Array([1, 2, 3]); var s = a.subarray(1); s[0] = 9;
		  [a[1], s.buffer === a.buffer].join(",")`, "9,true"},
		{`var a = new Uint8Array([1, 2, 3]); var s = a.slice(1); s[0] = 9;
		  [a[1], s.buffer === a.buffer].join(",")`, "2,false"},

		{`new Uint8Array([1, 2, 3]).slice(1).join(",")`, "2,3"},
		{`new Uint8Array([1, 2, 3]).map(x => x * 2).join(",")`, "2,4,6"},
		{`new Uint8Array([1, 2, 3]).filter(x => x > 1).join(",")`, "2,3"},
		{`new Uint8Array([1, 2, 3]).subarray(1).join(",")`, "2,3"},
		{`String(new Uint8Array([1, 2]).slice(0) instanceof Uint8Array)`, "true"},
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
		// A BigInt view and a Number one cannot stand in for each other, so the
		// substitution is refused rather than left to fail element by element.
		`var a = new Uint8Array([1, 2]); a.constructor = {[Symbol.species]: BigInt64Array};
		 a.slice(0)`,
		`var a = new BigInt64Array([1n]); a.constructor = {[Symbol.species]: Uint8Array};
		 a.map(x => x)`,
		// Nor can something that is not a view at all.
		`var a = new Uint8Array([1, 2]); a.constructor = {[Symbol.species]: Array};
		 a.slice(0)`,
		// Nor one too short to hold the result.
		`var a = new Uint8Array([1, 2, 3]);
		 a.constructor = {[Symbol.species]: function () { return new Uint8Array(1); }};
		 a.slice(0)`,
		`var a = new Uint8Array([1, 2]); a.constructor = {[Symbol.species]: 1}; a.slice(0)`,
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
