package quickjs_test

import "testing"

// A built-in that takes a list of numbers converts the whole list before it
// looks at any of it, so an argument whose valueOf counts its calls is called
// even when an earlier argument has already settled the answer.
func TestMathConvertsEveryArgument(t *testing.T) {
	cases := []struct{ src, want string }{
		{`var n = 0
		  Math.max(NaN, {valueOf: function () { n++; return 1 }})
		  String(n)`, "1"},
		{`var n = 0
		  Math.min(NaN, {valueOf: function () { n++; return 1 }})
		  String(n)`, "1"},
		{`var seen = []
		  Math.max({valueOf: function () { seen.push(1); return 1 }},
		           {valueOf: function () { seen.push(2); return 2 }})
		  seen.join(",")`, "1,2"},
		// The answer itself is unchanged by converting everything.
		{`String(Math.max(1, NaN, 2))`, "NaN"},
		{`String(Math.max(-0, 0)) + "," + String(1 / Math.max(-0, 0))`, "0,Infinity"},
		{`String(1 / Math.min(-0, 0))`, "-Infinity"},
		{`[Math.max(), Math.min()].join(",")`, "-Infinity,Infinity"},
	}
	for _, tc := range cases {
		checkEval(t, tc.src, tc.want)
	}
}

// Math.round rounds halves towards positive infinity, so the sign of a zero
// result follows the argument rather than the rounding.
func TestMathRoundKeepsTheSignOfZero(t *testing.T) {
	cases := []struct{ src, want string }{
		{`String(1 / Math.round(-0.5))`, "-Infinity"},
		{`String(1 / Math.round(-0.2))`, "-Infinity"},
		{`String(1 / Math.round(-0))`, "-Infinity"},
		{`String(1 / Math.round(0.4))`, "Infinity"},
		{`String(1 / Math.round(0))`, "Infinity"},
		{`[Math.round(0.5), Math.round(-0.6), Math.round(1.5), Math.round(-1.5)].join(",")`,
			"1,-1,2,-1"},
		// Adding a half to a large double would round rather than carry, so
		// values that are already whole are left alone.
		{`String(Math.round(4503599627370497))`, "4503599627370497"},
		{`[Math.round(Infinity), Math.round(-Infinity), Math.round(NaN)].join(",")`,
			"Infinity,-Infinity,NaN"},
		// The classic case: the double just below a half must not round up.
		{`String(Math.round(0.49999999999999994))`, "0"},
	}
	for _, tc := range cases {
		checkEval(t, tc.src, tc.want)
	}
}

// A vector with an infinite side has infinite length whatever its other sides
// are, including unknown ones -- so an infinity beats a NaN here, unlike
// everywhere else in Math.
func TestHypotPrefersInfinityOverNaN(t *testing.T) {
	cases := []struct{ src, want string }{
		{`String(Math.hypot(NaN, Infinity))`, "Infinity"},
		{`String(Math.hypot(Infinity, NaN))`, "Infinity"},
		{`String(Math.hypot(-Infinity, NaN))`, "Infinity"},
		{`String(Math.hypot(NaN, 1))`, "NaN"},
		{`String(Math.hypot())`, "0"},
		{`String(Math.hypot(3, 4))`, "5"},
		{`String(1 / Math.hypot(-0, -0))`, "Infinity"},
		// Scaling by the largest term is what keeps these from overflowing and
		// underflowing on the way.
		{`String(Math.hypot(1e300, 1e300) === Math.SQRT2 * 1e300)`, "true"},
		{`String(Math.hypot(1e-300, 1e-300) === Math.SQRT2 * 1e-300)`, "true"},
	}
	for _, tc := range cases {
		checkEval(t, tc.src, tc.want)
	}
}

// The sum of no numbers is -0, and so is the sum of nothing but -0s. Anything
// else that cancels to zero sums to +0.
func TestSumPreciseZeroSign(t *testing.T) {
	cases := []struct{ src, want string }{
		{`String(1 / Math.sumPrecise([]))`, "-Infinity"},
		{`String(1 / Math.sumPrecise([-0]))`, "-Infinity"},
		{`String(1 / Math.sumPrecise([-0, -0]))`, "-Infinity"},
		{`String(1 / Math.sumPrecise([-0, 0]))`, "Infinity"},
		{`String(1 / Math.sumPrecise([1, -1]))`, "Infinity"},
		{`String(Math.sumPrecise([1e20, 0.1, -1e20]))`, "0.1"},
	}
	for _, tc := range cases {
		checkEval(t, tc.src, tc.want)
	}
}

// Deciding whether something is an array asks a proxy for its target, which a
// revoked proxy no longer has: the question cannot be answered at all, so it is
// refused rather than guessed.
func TestIsArrayRefusesARevokedProxy(t *testing.T) {
	const revoked = `var h = Proxy.revocable([], {}); h.revoke(); `
	cases := []struct{ src, want string }{
		{revoked + `try { Array.isArray(h.proxy) } catch (e) { e.constructor.name }`, "TypeError"},
		{revoked + `try { Object.prototype.toString.call(h.proxy) }
		  catch (e) { e.constructor.name }`, "TypeError"},
		{revoked + `try { JSON.stringify(h.proxy) } catch (e) { e.constructor.name }`, "TypeError"},
		// A live proxy answers for its target, however deep the nesting.
		{`String(Array.isArray(new Proxy(new Proxy([], {}), {})))`, "true"},
		{`String(Array.isArray(new Proxy({}, {})))`, "false"},
	}
	for _, tc := range cases {
		checkEval(t, tc.src, tc.want)
	}
}

// Object.prototype.toString reports the tag it is given, and a getter that
// computes one may throw -- which it does through the caller rather than being
// swallowed and reported as an ordinary object.
func TestObjectToStringTag(t *testing.T) {
	cases := []struct{ src, want string }{
		{`var o = {}
		  Object.defineProperty(o, Symbol.toStringTag,
		    {get: function () { throw new RangeError() }})
		  try { Object.prototype.toString.call(o) } catch (e) { e.constructor.name }`,
			"RangeError"},
		{`Object.prototype.toString.call({[Symbol.toStringTag]: "Foo"})`, "[object Foo]"},
		// Only a string counts; anything else leaves the built-in tag.
		{`Object.prototype.toString.call({[Symbol.toStringTag]: 1})`, "[object Object]"},
		{`Object.prototype.toString.call(Object.assign([], {[Symbol.toStringTag]: null}))`,
			"[object Array]"},

		// Array.prototype.toString falls back to the ordinary description when
		// there is no join to call, and its receiver need not be an array.
		{`Array.prototype.toString.call(true)`, "[object Boolean]"},
		{`Array.prototype.toString.call({length: 0, join: 1})`, "[object Object]"},
		{`Array.prototype.toString.call({length: 0, join: 1, [Symbol.toStringTag]: "Foo"})`,
			"[object Foo]"},
		{`var a = [1, 2]; a.join = 1; a.toString()`, "[object Array]"},
		{`var a = [1, 2]; a.join = function () { return "!" }; a.toString()`, "!"},
	}
	for _, tc := range cases {
		checkEval(t, tc.src, tc.want)
	}
}

// An array method called on a primitive works through a wrapper, and the ones
// that return their receiver return that wrapper rather than the primitive.
func TestArrayMethodsReturnTheCoercedReceiver(t *testing.T) {
	cases := []struct{ src, want string }{
		{`String(Array.prototype.reverse.call(true) instanceof Boolean)`, "true"},
		{`String(Array.prototype.sort.call(true) instanceof Boolean)`, "true"},
		{`String(Array.prototype.fill.call(1) instanceof Number)`, "true"},
		// The ordinary case is unchanged: the receiver itself comes back.
		{`var a = [2, 1]; String(a.sort() === a)`, "true"},
		{`var a = [1, 2]; String(a.reverse() === a)`, "true"},
		{`var o = {length: 2, 0: 1, 1: 2}
		  String(Array.prototype.reverse.call(o) === o) + "," + o[0]`, "true,2"},
	}
	for _, tc := range cases {
		checkEval(t, tc.src, tc.want)
	}
}

// A search over nothing converts nothing: with no elements to compare against,
// the starting point cannot matter, so its valueOf is never called.
func TestEmptySearchConvertsNoStartingPoint(t *testing.T) {
	const counter = `var n = 0; var from = {valueOf: function () { n++; return 0 }}; `
	cases := []struct{ src, want string }{
		{counter + `[].includes(1, from) + "," + n`, "false,0"},
		{counter + `[].indexOf(1, from) + "," + n`, "-1,0"},
		{counter + `Array.prototype.includes.call({length: 0}, 1, from) + "," + n`, "false,0"},
		{counter + `Array.prototype.indexOf.call({length: 0}, 1, from) + "," + n`, "-1,0"},
		// With something to search, the starting point is converted as usual.
		{counter + `[1].includes(1, from) + "," + n`, "true,1"},
		{counter + `[1].indexOf(1, from) + "," + n`, "0,1"},
	}
	for _, tc := range cases {
		checkEval(t, tc.src, tc.want)
	}
}

// An argument list is read as an array-like, which a primitive is not: a string
// has indices and a length and would otherwise pass for one.
func TestArgumentListMustBeAnObject(t *testing.T) {
	cases := []struct{ src, want string }{
		{`try { (function () {}).apply(null, "1,2") } catch (e) { e.constructor.name }`,
			"TypeError"},
		{`try { (function () {}).apply(null, 1) } catch (e) { e.constructor.name }`, "TypeError"},
		{`try { (function () {}).apply(null, Symbol()) } catch (e) { e.constructor.name }`,
			"TypeError"},
		// null and undefined mean "no arguments" for apply, and only there.
		{`function f() { return arguments.length } [f.apply(null), f.apply(null, null)].join(",")`,
			"0,0"},
		{`try { Reflect.apply(function () {}, null, null) } catch (e) { e.constructor.name }`,
			"TypeError"},
		{`try { Reflect.apply(function () {}) } catch (e) { e.constructor.name }`, "TypeError"},
		{`try { Reflect.construct(function () {}) } catch (e) { e.constructor.name }`, "TypeError"},
		{`function f() { return arguments.length }
		  [Reflect.apply(f, null, []), Reflect.apply(f, null, {length: 2})].join(",")`, "0,2"},
	}
	for _, tc := range cases {
		checkEval(t, tc.src, tc.want)
	}
}

// Object.prototype is the root of every ordinary prototype chain, so giving it
// a prototype would put something above everything in the realm at once. It is
// refused however extensible the object is.
func TestObjectPrototypeKeepsItsPrototype(t *testing.T) {
	cases := []struct{ src, want string }{
		{`try { Object.setPrototypeOf(Object.prototype, {}) } catch (e) { e.constructor.name }`,
			"TypeError"},
		{`try { Object.setPrototypeOf(Object.prototype, Object.create(null)) }
		  catch (e) { e.constructor.name }`, "TypeError"},
		{`String(Reflect.setPrototypeOf(Object.prototype, Object.create(null)))`, "false"},
		{`try { Object.prototype.__proto__ = {} } catch (e) { e.constructor.name }`, "TypeError"},
		// Setting it to what it already is changes nothing and succeeds.
		{`String(Reflect.setPrototypeOf(Object.prototype, null))`, "true"},
		{`Object.setPrototypeOf(Object.prototype, null); String(Object.getPrototypeOf({}))`,
			"[object Object]"},
		// Every other object is unaffected.
		{`var o = {}; Object.setPrototypeOf(o, Array.prototype)
		  String(Object.getPrototypeOf(o) === Array.prototype)`, "true"},
	}
	for _, tc := range cases {
		checkEval(t, tc.src, tc.want)
	}
}

// An entry is a pair, and only an object can be one: a string has a [0] and a
// [1] and would otherwise be taken apart as though it were.
func TestFromEntriesRequiresObjects(t *testing.T) {
	cases := []struct{ src, want string }{
		{`try { Object.fromEntries(["ab"]) } catch (e) { e.constructor.name }`, "TypeError"},
		{`try { Object.fromEntries([1]) } catch (e) { e.constructor.name }`, "TypeError"},
		{`JSON.stringify(Object.fromEntries([["a", 1], ["b", 2]]))`, `{"a":1,"b":2}`},
		// The iterator is closed on the way out, so a source that holds
		// something is told to let go of it.
		{`var closed = false
		  var it = {[Symbol.iterator]: function () {
		    return {
		      next: function () { return {done: false, value: "ab"} },
		      return: function () { closed = true; return {} },
		    }
		  }}
		  try { Object.fromEntries(it) } catch (e) {}
		  String(closed)`, "true"},
	}
	for _, tc := range cases {
		checkEval(t, tc.src, tc.want)
	}
}

// Own keys come out in one order: indices in ascending order first, then the
// string keys in the order they were created. A String object's length is a
// string key like any other, so an index added past the end of the string comes
// before it.
func TestStringObjectKeyOrder(t *testing.T) {
	cases := []struct{ src, want string }{
		{`var s = new String("abc"); s[5] = 1
		  Object.getOwnPropertyNames(s).join(",")`, "0,1,2,5,length"},
		{`var s = new String("ab"); s.x = 1; s[9] = 2
		  Object.getOwnPropertyNames(s).join(",")`, "0,1,9,length,x"},
		{`Object.getOwnPropertyNames(new String("")).join(",")`, "length"},
		{`Object.keys(new String("ab")).join(",")`, "0,1"},
	}
	for _, tc := range cases {
		checkEval(t, tc.src, tc.want)
	}
}

// An array element defined with attributes lives in the property table rather
// than in dense storage. Redefining it as an ordinary element has to replace it
// there, not write a second answer into the dense slot.
func TestArrayElementDefinedTwice(t *testing.T) {
	cases := []struct{ src, want string }{
		{`var a = new Array(0)
		  Object.defineProperty(a, 0, {value: 0, writable: false, configurable: true})
		  Object.defineProperty(a, 0, {value: 1, writable: true, enumerable: true})
		  delete a[0];
		  [Object.prototype.hasOwnProperty.call(a, 0), a.length].join(",")`, "false,1"},
		{`var a = new Array(0)
		  Object.defineProperty(a, 0, {value: 0, writable: false, configurable: true})
		  Object.defineProperty(a, 0, {value: 1, writable: true, enumerable: true});
		  [a[0], a.length, JSON.stringify(Object.getOwnPropertyDescriptor(a, 0))].join(" ")`,
			`1 1 {"value":1,"writable":true,"enumerable":true,"configurable":true}`},
		// An accessor replaced by a plain element goes the same way.
		{`var a = new Array(0)
		  Object.defineProperty(a, 0, {get: function () { return 7 }, configurable: true})
		  Object.defineProperty(a, 0, {value: 1, writable: true, enumerable: true})
		  delete a[0];
		  [String(a[0]), Object.prototype.hasOwnProperty.call(a, 0)].join(",")`,
			"undefined,false"},
		// A read-only element still refuses a write, and keeps its attributes.
		{`var a = [1, 2, 3]
		  Object.defineProperty(a, 1, {value: 9, writable: false, enumerable: true})
		  a[1] = 5
		  a.join(",") + " " + a.length`, "1,9,3 3"},
		// The ordinary sparse array is unchanged.
		{`var a = []; a[3] = 1
		  Object.defineProperty(a, 5, {value: 2, configurable: true, writable: true, enumerable: true})
		  a[7] = 3;
		  [a.length, a[3], a[5], a[7], Object.keys(a).join("/")].join(",")`, "8,1,2,3,3/5/7"},

		// A species-made array is filled with fresh elements, whatever the
		// constructor left in the slots they land on.
		{`var a = [1]
		  a.constructor = {}
		  a.constructor[Symbol.species] = function () {
		    var q = new Array(0)
		    Object.defineProperty(q, 0, {value: 0, writable: false, configurable: true})
		    return q
		  }
		  var r = a.slice(0)
		  JSON.stringify(Object.getOwnPropertyDescriptor(r, 0))`,
			`{"value":1,"writable":true,"enumerable":true,"configurable":true}`},
	}
	for _, tc := range cases {
		checkEval(t, tc.src, tc.want)
	}
}
