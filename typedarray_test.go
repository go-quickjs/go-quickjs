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

// An element of a typed array lives in a buffer, and the object's internal
// methods say so: an index is present only while it is in range, cannot be
// deleted, cannot be turned into an accessor or made read-only, and a write
// past the end is dropped rather than added.
func TestTypedArrayIndicesAreNotProperties(t *testing.T) {
	const a = `var a = new Uint8Array([1, 2]); `
	cases := []struct{ src, want string }{
		// Defining an index writes the element rather than adding a property.
		{a + `Object.defineProperty(a, "0", {value: 9}); String(a[0])`, "9"},
		{a + `JSON.stringify(Object.getOwnPropertyDescriptor(a, "0"))`,
			`{"value":1,"writable":true,"enumerable":true,"configurable":true}`},
		{a + `String(Object.getOwnPropertyDescriptor(a, "5"))`, "undefined"},

		// Presence follows the range, and is never looked for up the prototype
		// chain.
		{a + `[("5" in a), ("1" in a)].join(",")`, "false,true"},
		{a + `Uint8Array.prototype[5] = "p"; [String(a[5]), 5 in a].join(",")`,
			"undefined,false"},

		// An element cannot be removed; one that is not there is gone already.
		{a + `[delete a[0], delete a[5]].join(",")`, "false,true"},
		{a + `delete a[0]; a.join(",")`, "1,2"},

		// A write past the end is dropped.
		{a + `a[5] = 7; String(a[5])`, "undefined"},
		{a + `a[5] = 7; String(a.hasOwnProperty("5"))`, "false"},

		// A canonical numeric index string is the buffer's business whether or
		// not it names an element, so these are dropped too -- while "01",
		// which is not canonical, is an ordinary property.
		{a + `a["-0"] = 7; String(a["-0"])`, "undefined"},
		{a + `a["1.5"] = 7; String(a["1.5"])`, "undefined"},
		{a + `a["NaN"] = 7; String(a["NaN"])`, "undefined"},
		{a + `a["01"] = 7; String(a["01"])`, "7"},
		{a + `a[" 1"] = 7; String(a[" 1"])`, "7"},
		{a + `[("−0" in a), ("01" in a)].join(",")`, "false,false"},

		// The coercion still happens for a write that stores nothing.
		{a + `var seen = false; a[5] = {valueOf() { seen = true; return 1; }};
		  String(seen)`, "true"},

		{a + `Object.keys(a).join(",")`, "0,1"},
		{a + `a.join(",")`, "1,2"},
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
		// The attributes of an element are fixed, so a descriptor asking for
		// others is refused rather than quietly ignored -- otherwise a script
		// would believe it had frozen one.
		a + `Object.defineProperty(a, "0", {get() { return 1; }})`,
		a + `Object.defineProperty(a, "0", {value: 9, writable: false})`,
		a + `Object.defineProperty(a, "0", {value: 9, configurable: false})`,
		a + `Object.defineProperty(a, "0", {value: 9, enumerable: false})`,
		// And an index outside the array names nothing to define.
		a + `Object.defineProperty(a, "5", {value: 1})`,
		a + `Object.defineProperty(a, "-0", {value: 1})`,
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

// A buffer can go away underneath a view at any point -- a valueOf called while
// a method is running is enough -- so every read and write is measured against
// what is there now rather than against what was there when the view was made.
func TestTypedArrayDetachDuringUse(t *testing.T) {
	cases := []struct{ src, want string }{
		// A view over a buffer that has gone is empty, not broken.
		{`var a = new Uint8Array(8); a.buffer.transfer();
		  [a.length, a.byteLength, a.byteOffset].join(",")`, "0,0,0"},
		{`var a = new Uint8Array(8); a.buffer.transfer(); String(a[0])`, "undefined"},
		{`var a = new Uint8Array(8); a.buffer.transfer(); String(0 in a)`, "false"},

		// Detaching part-way through an operation stops it rather than
		// crashing it. fill checks the view again once its arguments are
		// coerced, so the detachment is reported rather than ignored.
		{`var a = new Uint8Array(8);
		  try { a.fill(1, {valueOf() { a.buffer.transfer(); return 0 }}) }
		  catch (e) { e.constructor.name }`, "TypeError"},
		// slice looks at the source again once the result has been built, so a
		// detachment while its arguments were coerced is reported -- but only
		// when there is something to copy.
		{`var a = new Uint8Array(8)
		  try { a.slice(0, {valueOf() { a.buffer.transfer(); return 8 }}) }
		  catch (e) { e.constructor.name }`, "TypeError"},
		{`var a = new Uint8Array(8)
		  a.slice(0, {valueOf() { a.buffer.transfer(); return 0 }}).length + ""`, "0"},
		// The buffer is reported whether or not it is still attached.
		{`var a = new Uint8Array(4); var b = a.buffer; a.buffer.transfer()
		  String(a.buffer === b)`, "true"},
		{`var a = new Uint8Array(8);
		  a.copyWithin(0, 1, {valueOf() { a.buffer.transfer(); return 8 }}); "ok"`, "ok"},
		// set looks at the buffer only after the offset is coerced, so a
		// detachment there is reported rather than written through.
		{`var a = new Uint8Array(8);
		  try { a.set(new Uint8Array(2), {valueOf() { a.buffer.transfer(); return 0 }}) }
		  catch (e) { e.constructor.name }`, "TypeError"},
		{`var a = new Uint8Array(8);
		  a[0] = {valueOf() { a.buffer.transfer(); return 1 }}; String(a.length)`, "0"},

		// A proxy of a function is callable but has no source of its own.
		{`var p = new Proxy(function () {}, {}); String(p)`,
			"function () { [native code] }"},
		{`var p = new Proxy(() => 1, {}); p.toString()`,
			"function () { [native code] }"},
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

// The species protocol falls back to an intrinsic constructor when the object
// does not name one, and an intrinsic is not something a script can replace.
// Reading the default back from the prototype's constructor property made it
// one: redefining that property as an accessor left no default at all, and
// constructing nothing took the host down.
func TestTypedArraySpeciesDefaultIsIntrinsic(t *testing.T) {
	cases := []struct{ src, want string }{
		{`var a = new Uint8Array([1, 2, 3]);
		  var calls = 0;
		  Object.defineProperty(Uint8Array.prototype, "constructor",
		      {get: function () { calls += 1 }});
		  var r = a.map(function () { return 0 });
		  [calls, r.length, Object.getPrototypeOf(r) === Uint8Array.prototype].join(",")`,
			"1,3,true"},
		{`var a = new Int16Array([1, 2, 3]);
		  Object.defineProperty(Int16Array.prototype, "constructor", {value: undefined});
		  a.slice(1).join(",") + "|" + a.filter(function (x) { return x > 1 }).join(",")`,
			"2,3|2,3"},
		// The statics find their element type the same way.
		{`Object.defineProperty(Uint8Array.prototype, "constructor", {value: 1});
		  Uint8Array.from([1, 2]).join(",") + "|" + Uint8Array.of(3, 4).join(",")`,
			"1,2|3,4"},
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

// A typed array method that writes one value to many elements converts it
// once, which a valueOf that counts its calls can see -- and converts it the
// way the element type asks: a BigInt for the 64-bit integer kinds.
func TestTypedArrayValueConversion(t *testing.T) {
	cases := []struct{ src, want string }{
		{`var n = 1, t = new Float64Array(2);
		  t.fill({valueOf: function () { return n++ }});
		  t.join(",") + "|" + n`, "1,1|2"},
		{`var n = 1n, t = new BigInt64Array(2);
		  t.fill({valueOf: function () { var v = n; n += 1n; return v }});
		  t.join(",") + "|" + n`, "1,1|2"},

		// The update operators work on a BigInt as a BigInt.
		{`var n = 1n; n++; String(n)`, "2"},
		{`var n = 1n; String(n++) + "," + String(n)`, "1,2"},
		{`var n = 5n; String(--n)`, "4"},
		{`var o = {v: 1n}; o.v++; String(o.v)`, "2"},
		{`var a = [1n]; a[0]++; String(a[0])`, "2"},
		{`var x = "1"; x++; String(x)`, "2"},

		// from and of build their result with the constructor they were called
		// on, so a subclass gets one of its own.
		{`class T extends Uint8Array {}
		  var t = T.from([1, 2]); [t instanceof T, t.join(",")].join("|")`, "true|1,2"},
		{`class T extends Uint8Array {}
		  var t = T.of(3, 4); [t instanceof T, t.join(",")].join("|")`, "true|3,4"},
		{`BigInt64Array.from([1n, 2n]).join(",")`, "1,2"},

		// Constructing over a buffer coerces both arguments before it looks at
		// the buffer, and a detached one is a TypeError.
		{`var b = new ArrayBuffer(8);
		  try { new Uint8Array(b, {valueOf: function () {
		    if (typeof structuredClone === "undefined") { b.transfer() } return 0
		  }}) } catch (e) { e.constructor.name }`, "TypeError"},
		{`try { new Uint32Array(new ArrayBuffer(8), 3) } catch (e) { e.constructor.name }`,
			"RangeError"},
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

// TestTypedArrayIterationIsLive covers iterating a typed array, which reads the
// view as it goes rather than copying it -- a view over a buffer that does not
// see writes would be beside the point.
func TestTypedArrayIterationIsLive(t *testing.T) {
	cases := []struct{ src, want string }{
		{`var a = new Int8Array([3, 2, 4, 1])
		  var out = []
		  for (var x of a) { out.push(x); a[1] = 64 }
		  out.join(",")`, "3,64,4,1"},
		{`var a = new Int8Array([1, 2])
		  var out = []
		  for (var e of a.entries()) { out.push(e.join(":")); a[1] = 9 }
		  out.join(",")`, "0:1,1:9"},
		{`var a = new Int8Array([1, 2]); [...a.keys()].join(",")`, "0,1"},
		{`var a = new Int8Array([1, 2]); [...a.values()].join(",")`, "1,2"},

		// A buffer detached mid-iteration is a TypeError, not a silent end:
		// the length reads as zero, which would otherwise look like the end.
		{`var a = new Int8Array(5)
		  var i = 0
		  try {
		    for (var k of a.keys()) { i++; a.buffer.transfer() }
		    "no throw"
		  } catch (e) { e.constructor.name + "," + i }`, "TypeError,1"},

		// The methods still reject a receiver that is not a typed array.
		{`try { Int8Array.prototype.values.call([]) } catch (e) { e.constructor.name }`,
			"TypeError"},
		{`try { Int8Array.prototype.keys.call([]) } catch (e) { e.constructor.name }`,
			"TypeError"},
	}
	for _, tc := range cases {
		checkEval(t, tc.src, tc.want)
	}
}

// TestTypedArraySetOrder covers %TypedArray%.prototype.set, which reads one
// element of the source and writes it before reading the next.
func TestTypedArraySetOrder(t *testing.T) {
	cases := []struct{ src, want string }{
		{`var a = new Int8Array(5)
		  var log = []
		  var src = {length: 3}
		  for (var i = 0; i < 3; i++) {
		    (function (i) {
		      Object.defineProperty(src, i, {get() { log.push(a.join()); return 42 + i }})
		    })(i)
		  }
		  a.set(src)
		  log.join("|") + " => " + a.join()`,
			"0,0,0,0,0|42,0,0,0,0|42,43,0,0,0 => 42,43,44,0,0"},
		// A getter that throws leaves the writes that preceded it in place.
		{`var a = new Int8Array(3)
		  var src = {length: 2, 0: 7}
		  Object.defineProperty(src, 1, {get() { throw new RangeError() }})
		  try { a.set(src) } catch (e) {}
		  a.join()`, "7,0,0"},
		// The range is checked before anything is read.
		{`var a = new Int8Array(2)
		  var read = false
		  var src = {length: 3}
		  Object.defineProperty(src, 0, {get() { read = true; return 1 }})
		  try { a.set(src) } catch (e) { e.constructor.name + "," + read }`,
			"RangeError,false"},

		// A typed array source sharing the buffer is read before it is written.
		{`var buf = new ArrayBuffer(4)
		  var a = new Int8Array(buf)
		  a.set([1, 2, 3, 4])
		  a.set(new Int8Array(buf, 0, 3), 1)
		  a.join()`, "1,1,2,3"},

		// The offset is coerced before the buffer is looked at.
		{`var a = new Uint8Array(8)
		  try { a.set(new Uint8Array(2), {valueOf() { a.buffer.transfer(); return 0 }}) }
		  catch (e) { e.constructor.name }`, "TypeError"},
		{`String(Int8Array.prototype.set.length)`, "1"},
	}
	for _, tc := range cases {
		checkEval(t, tc.src, tc.want)
	}
}

// TestFloat16Rounding covers narrowing a double to a half, which rounds to
// nearest even -- including in the subnormal range, where going through a
// float32 first would lose the bits the rounding depends on.
func TestFloat16Rounding(t *testing.T) {
	cases := []struct{ src, want string }{
		// A hair above half the smallest subnormal rounds up to it; exactly
		// half rounds to even, which is zero.
		{`String(Math.f16round(2.980232238769532e-8))`, "5.960464477539063e-8"},
		{`String(Math.f16round(2.9802322387695312e-8))`, "0"},
		{`String(Math.f16round(5.960464477539063e-8))`, "5.960464477539063e-8"},
		{`var a = new Float16Array(1); a[0] = 2.980232238769532e-8; String(a[0])`,
			"5.960464477539063e-8"},

		{`String(Math.f16round(1.337))`, "1.3369140625"},
		{`String(Math.f16round(0.1))`, "0.0999755859375"},
		{`String(Math.f16round(65504))`, "65504"},
		{`String(Math.f16round(65520))`, "Infinity"},
		{`String(Math.f16round(-65520))`, "-Infinity"},
		{`String(Math.f16round(1e-10))`, "0"},
		{`String(Math.f16round(NaN))`, "NaN"},
		{`String(Math.f16round(Infinity))`, "Infinity"},
		{`Object.is(Math.f16round(-0), -0)`, "true"},
		{`new Float16Array([1.5, 2.5, -1.5]).join()`, "1.5,2.5,-1.5"},
	}
	for _, tc := range cases {
		checkEval(t, tc.src, tc.want)
	}
}

// TestTypedArrayIndexThroughReceiver covers writing a numeric key through an
// object whose prototype is a typed array. The view owns every numeric key,
// whether or not it has an element there, so one it has no element for is
// dropped rather than shadowed.
func TestTypedArrayIndexThroughReceiver(t *testing.T) {
	cases := []struct{ src, want string }{
		{`var env = Object.create(new Int32Array(10))
		  env[99] = 1
		  String(Object.getOwnPropertyDescriptor(env, "99"))`, "undefined"},
		{`var env = Object.create(new Int32Array(10))
		  env.NaN = 1
		  String(Object.getOwnPropertyDescriptor(env, "NaN"))`, "undefined"},
		{`var env = Object.create(new Int32Array(10))
		  env["1.5"] = 1
		  String(Object.getOwnPropertyDescriptor(env, "1.5"))`, "undefined"},
		// A key the view does have is written on the receiver, shadowing it.
		{`var env = Object.create(new Int32Array(10))
		  env[0] = 7
		  env.hasOwnProperty("0") + "," + env[0]`, "true,7"},
		// A key that is not numeric at all is an ordinary property.
		{`var env = Object.create(new Int32Array(10))
		  env.x = 1
		  env.hasOwnProperty("x") + "," + env.x`, "true,1"},
	}
	for _, tc := range cases {
		checkEval(t, tc.src, tc.want)
	}
}

// TestArrayBufferSliceSpecies covers ArrayBuffer.prototype.slice, which asks
// the object what constructor to build the copy with and checks what it gets.
func TestArrayBufferSliceSpecies(t *testing.T) {
	cases := []struct{ src, want string }{
		{`var b = new ArrayBuffer(8)
		  new Uint8Array(b).set([1, 2, 3])
		  Array.from(new Uint8Array(b.slice(0, 3))).join()`, "1,2,3"},
		{`String(new ArrayBuffer(8).slice(0).byteLength)`, "8"},
		{`class B extends ArrayBuffer {}
		  String(new B(8).slice(0) instanceof B)`, "true"},

		// A constructor that is neither absent nor an object is a TypeError.
		{`var b = new ArrayBuffer(8); b.constructor = null
		  try { b.slice(0) } catch (e) { e.constructor.name }`, "TypeError"},
		{`var b = new ArrayBuffer(8); b.constructor = true
		  try { b.slice(0) } catch (e) { e.constructor.name }`, "TypeError"},
		// A species that is not a constructor, or does not build a buffer.
		{`var b = new ArrayBuffer(8); b.constructor = {[Symbol.species]: {}}
		  try { b.slice(0) } catch (e) { e.constructor.name }`, "TypeError"},
		{`var b = new ArrayBuffer(8); b.constructor = {[Symbol.species]: Object}
		  try { b.slice(0) } catch (e) { e.constructor.name }`, "TypeError"},
		// One that hands back the buffer being sliced, or too small a buffer.
		{`var b = new ArrayBuffer(8)
		  b.constructor = {[Symbol.species]: function () { return b }}
		  try { b.slice(0) } catch (e) { e.constructor.name }`, "TypeError"},
		{`var b = new ArrayBuffer(8)
		  b.constructor = {[Symbol.species]: function () { return new ArrayBuffer(1) }}
		  try { b.slice(0) } catch (e) { e.constructor.name }`, "TypeError"},
		// An absent constructor falls back to the intrinsic.
		{`var b = new ArrayBuffer(8); b.constructor = undefined
		  String(b.slice(0).byteLength)`, "8"},
	}
	for _, tc := range cases {
		checkEval(t, tc.src, tc.want)
	}
}

// TestBase64Decoding covers the Uint8Array base64 and hex conversions, whose
// point is to give the caller control over the two things a hand-rolled
// decoder gets wrong: which alphabet, and what to do with a trailing chunk that
// is not a whole group.
func TestBase64Decoding(t *testing.T) {
	cases := []struct{ src, want string }{
		{`Array.from(Uint8Array.fromBase64("ZXhhZg==")).join()`, "101,120,97,102"},
		{`Array.from(Uint8Array.fromBase64("ZXhhZg")).join()`, "101,120,97,102"},
		{`Array.from(Uint8Array.fromBase64("ZXhhZg",
		    {lastChunkHandling: "stop-before-partial"})).join()`, "101,120,97"},
		{`try { Uint8Array.fromBase64("ZXhhZg", {lastChunkHandling: "strict"}) }
		  catch (e) { e.constructor.name }`, "SyntaxError"},
		// Padding must be exactly as much as the group is short.
		{`try { Uint8Array.fromBase64("ZXhhZg=") } catch (e) { e.constructor.name }`,
			"SyntaxError"},
		{`try { Uint8Array.fromBase64("ZXhhZg===") } catch (e) { e.constructor.name }`,
			"SyntaxError"},
		{`try { Uint8Array.fromBase64("ZXhhZg===", {lastChunkHandling: "stop-before-partial"}) }
		  catch (e) { e.constructor.name }`, "SyntaxError"},
		{`Array.from(Uint8Array.fromBase64("ZXhhZg=",
		    {lastChunkHandling: "stop-before-partial"})).join()`, "101,120,97"},
		// A lone character is a partial group, not a truncated one.
		{`try { Uint8Array.fromBase64("A") } catch (e) { e.constructor.name }`, "SyntaxError"},
		{`Array.from(Uint8Array.fromBase64("A",
		    {lastChunkHandling: "stop-before-partial"})).length + ""`, "0"},

		// The options are compared as given rather than coerced.
		{`try { Uint8Array.fromBase64("Zg==", {alphabet: Object("base64")}) }
		  catch (e) { e.constructor.name }`, "TypeError"},
		{`try { Uint8Array.fromBase64("Zg==", {lastChunkHandling: Object("loose")}) }
		  catch (e) { e.constructor.name }`, "TypeError"},
		{`Array.from(Uint8Array.fromBase64("x-_y", {alphabet: "base64url"})).join()`,
			"199,239,242"},

		// Whatever was decoded before a failure is still written.
		{`var a = new Uint8Array(5).fill(255)
		  try { a.setFromBase64("MjYyZm.9v") } catch (e) {}
		  Array.from(a).join()`, "50,54,50,255,255"},
		{`var a = new Uint8Array(5).fill(255)
		  try { a.setFromHex("aa a") } catch (e) {}
		  Array.from(a).join()`, "170,255,255,255,255"},
		// An odd length is settled before anything is decoded.
		{`var a = new Uint8Array(5).fill(255)
		  try { a.setFromHex("aaa") } catch (e) {}
		  Array.from(a).join()`, "255,255,255,255,255"},
		// Nothing beyond a full destination is even read.
		{`var a = new Uint8Array(0)
		  a.setFromBase64("aaaa#").read + "," + a.setFromBase64("#").read`, "0,0"},

		// The receiver is checked again after the options have been read.
		{`var a = new Uint8Array(2)
		  var calls = 0
		  var opts = {get alphabet() { calls++; a.buffer.transfer(); return "base64" }}
		  try { a.toBase64(opts) } catch (e) { e.constructor.name + "," + calls }`,
			"TypeError,1"},
		{`var a = new Uint8Array(2)
		  a.buffer.transfer()
		  var calls = 0
		  var opts = {get alphabet() { calls++; return "base64" }}
		  try { a.toBase64(opts) } catch (e) { e.constructor.name + "," + calls }`,
			"TypeError,1"},
	}
	for _, tc := range cases {
		checkEval(t, tc.src, tc.want)
	}
}

// TestTypedArraySliceSpecies covers what slice does once the species has built
// the result: it looks at the source again, because building it ran user code.
func TestTypedArraySliceSpecies(t *testing.T) {
	cases := []struct{ src, want string }{
		// Nothing to copy, so the source is never looked at again.
		{`var sample, other, counter = 0
		  var ctor = {[Symbol.species]: function (count) {
		    sample.buffer.transfer(); counter++; other = new Int8Array(count); return other
		  }}
		  sample = new Int8Array(0); sample.constructor = ctor
		  var r = sample.slice()
		  r.length + "," + (r === other) + "," + counter`, "0,true,1"},
		{`var sample, counter = 0
		  var ctor = {[Symbol.species]: function (count) {
		    sample.buffer.transfer(); counter++; return new Int8Array(count)
		  }}
		  sample = new Int8Array(4); sample.constructor = ctor
		  sample.slice(1, 1).length + "," + counter`, "0,1"},

		// With something to copy, a source detached by the species is an error.
		{`var sample = new Int8Array(4)
		  sample.constructor = {[Symbol.species]: function (count) {
		    sample.buffer.transfer(); return new Int8Array(count)
		  }}
		  try { sample.slice() } catch (e) { e.constructor.name }`, "TypeError"},
		// And a result that cannot hold what the source holds is one too.
		{`var sample = new BigInt64Array(4)
		  sample.constructor = {
		    [Symbol.species]: function (count) { return new Int8Array(count) },
		  }
		  try { sample.slice() } catch (e) { e.constructor.name }`, "TypeError"},
		// A zero-length slice does not care about the kind.
		{`var sample = new BigInt64Array(4)
		  sample.constructor = {
		    [Symbol.species]: function (count) { return new Int8Array(count) },
		  }
		  sample.slice(1, 1).length + ""`, "0"},
	}
	for _, tc := range cases {
		checkEval(t, tc.src, tc.want)
	}
}
