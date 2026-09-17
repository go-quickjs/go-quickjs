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

// A sigma at the end of a word lowercases to its final form. The rule looks
// past the characters that are neither cased nor word-breaking -- accents,
// formatting, and the punctuation that may sit inside a word, a full stop among
// it.
func TestFinalSigmaLooksPastPunctuation(t *testing.T) {
	cases := []struct{ src, want string }{
		{`"A.\u03A3".toLowerCase()`, "a.\u03c2"},
		{`"A'\u03A3".toLowerCase()`, "a'\u03c2"},
		{`"A:\u03A3".toLowerCase()`, "a:\u03c2"},
		{`"A\u00AD\u03A3".toLowerCase()`, "a\u00ad\u03c2"},
		// Followed by a cased letter it is not final, whatever comes between.
		{`"A\u03A3.b".toLowerCase()`, "a\u03c3.b"},
		{`"A\u03A3\u00ADB".toLowerCase()`, "a\u03c3\u00adb"},
		// With nothing cased before it, it is not final either.
		{`".\u03A3".toLowerCase()`, ".\u03c3"},
		{`"\u03A3".toLowerCase()`, "\u03c3"},
	}
	for _, tc := range cases {
		checkEval(t, tc.src, tc.want)
	}
}

// String.prototype.split settles its limit and converts its separator before it
// decides what to return, so the side effects of both are visible even when the
// answer is an empty array.
func TestSplitConversionOrder(t *testing.T) {
	cases := []struct{ src, want string }{
		{`String("undefined is not a function".split(undefined, 0).length)`, "0"},
		{`"a,b".split(",", 0).length + "," + "a,b".split(",", 1).join("")`, "0,a"},
		{`var seen = ""
		  try { "foo".split({toString: function () { seen = "sep"; throw new RangeError() }}, 0) }
		  catch (e) { seen += "," + e.constructor.name }
		  seen`, "sep,RangeError"},
		{`var seen = []
		  "foo".split({toString: function () { seen.push("sep"); return "o" }},
		              {valueOf: function () { seen.push("lim"); return 2 }})
		  seen.join(",")`, "lim,sep"},
		{`"ab".split(undefined).join("|")`, "ab"},
		{`"ab".split(undefined, 1).join("|")`, "ab"},
	}
	for _, tc := range cases {
		checkEval(t, tc.src, tc.want)
	}
}

// Object called with a new.target that is not Object itself builds an instance
// of that constructor and ignores its argument.
func TestObjectSubclassIgnoresItsArgument(t *testing.T) {
	cases := []struct{ src, want string }{
		{`class O extends Object {}
		  var o = new O({a: 1});
		  [String(o.a), Object.getPrototypeOf(o) === O.prototype].join(",")`, "undefined,true"},
		{`class O extends Object {}
		  var o = Reflect.construct(Object, [{b: 2}], O);
		  [String(o.b), Object.getPrototypeOf(o) === O.prototype].join(",")`, "undefined,true"},
		// Object itself still converts, called or constructed.
		{`String(new Object(1) instanceof Number) + "," + String(Object(1) instanceof Number)`,
			"true,true"},
		{`var o = {}; String(new Object(o) === o)`, "true"},
		{`typeof new Object()`, "object"},
	}
	for _, tc := range cases {
		checkEval(t, tc.src, tc.want)
	}
}

// A proxy with no trap forwards to its target, which means doing what the
// target would have done -- including refusing, which is reported rather than
// thrown, and including the checks the target's own operation would run.
func TestProxyForwardsWithoutATrap(t *testing.T) {
	cases := []struct{ src, want string }{
		// The target refuses; Reflect reports it, and Object.defineProperty is
		// what turns that into a throw.
		{`var o = Object.preventExtensions({})
		  var p = new Proxy(new Proxy(o, {}), {})
		  String(Reflect.defineProperty(p, "foo", {value: 5}))`, "false"},
		{`var o = Object.preventExtensions({})
		  var p = new Proxy(new Proxy(o, {}), {})
		  try { Object.defineProperty(p, "foo", {value: 5}) } catch (e) { e.constructor.name }`,
			"TypeError"},
		{`var o = Object.freeze({})
		  var p = new Proxy(new Proxy(o, {}), {})
		  String(Reflect.set(p, "y", 1))`, "false"},
		// Forwarding a prototype change runs the cycle check the target would.
		{`var o = {}
		  var p = new Proxy(new Proxy(o, {}), {setPrototypeOf: null})
		  Object.setPrototypeOf(p, null)
		  String(Object.getPrototypeOf(o))`, "null"},
		{`var o = {}
		  var p = new Proxy(new Proxy(o, {}), {setPrototypeOf: null})
		  try { Object.setPrototypeOf(p, Object.create(o)) } catch (e) { e.constructor.name }`,
			"TypeError"},
		// A trap that refuses a delete is a delete that failed, which strict
		// mode reports.
		{`var t = new Proxy({}, {deleteProperty: function () { return false }})
		  var p = new Proxy(t, {deleteProperty: undefined})
		  try { (function () { "use strict"; delete p.bar })() } catch (e) { e.constructor.name }`,
			"TypeError"},
		{`var t = new Proxy({}, {deleteProperty: function () { return false }})
		  var p = new Proxy(t, {deleteProperty: undefined})
		  String(delete p.bar)`, "false"},
	}
	for _, tc := range cases {
		checkEval(t, tc.src, tc.want)
	}
}

// Reflect.defineProperty answers false for a refusal, but a descriptor it
// cannot even read is an error of the caller's.
func TestReflectDefinePropertyReportsAndThrows(t *testing.T) {
	cases := []struct{ src, want string }{
		{`try { Reflect.defineProperty({}, "a",
		    Object.defineProperty({}, "enumerable",
		      {get: function () { throw new RangeError() }})) }
		  catch (e) { e.constructor.name }`, "RangeError"},
		{`String(Reflect.defineProperty(Object.preventExtensions({}), "a", {value: 1}))`, "false"},
		{`String(Reflect.defineProperty({}, "a", {value: 1}))`, "true"},
		{`try { Reflect.defineProperty(1, "a", {}) } catch (e) { e.constructor.name }`, "TypeError"},
	}
	for _, tc := range cases {
		checkEval(t, tc.src, tc.want)
	}
}

// The two String methods that are not generic: they hand back the string their
// receiver is or holds, and refuse a receiver that is neither.
func TestStringToStringIsNotGeneric(t *testing.T) {
	cases := []struct{ src, want string }{
		{`try { String.prototype.toString.call(1) } catch (e) { e.constructor.name }`, "TypeError"},
		{`try { String.prototype.valueOf.call(1) } catch (e) { e.constructor.name }`, "TypeError"},
		{`try { String.prototype.toString.call({}) } catch (e) { e.constructor.name }`, "TypeError"},
		{`try { String.prototype.valueOf.call(null) } catch (e) { e.constructor.name }`, "TypeError"},
		{`String.prototype.toString.call(new String("ab"))`, "ab"},
		{`String.prototype.valueOf.call("ab")`, "ab"},
		// The generic ones still accept anything.
		{`String.prototype.charAt.call(12, 1)`, "2"},
		// And the padding methods require only what they cannot do without.
		{`[String.prototype.padStart.length, String.prototype.padEnd.length].join(",")`, "1,1"},
	}
	for _, tc := range cases {
		checkEval(t, tc.src, tc.want)
	}
}

// A bound function is not what its instances were built from, so instanceof
// asks about the function it was bound from -- afresh, so that a
// Symbol.hasInstance there is honoured.
func TestBoundFunctionInstanceOf(t *testing.T) {
	cases := []struct{ src, want string }{
		{`function F() {}; var b = F.bind(); var o = new F();
		  [o instanceof b, b[Symbol.hasInstance](o)].join(",")`, "true,true"},
		{`function F() {}; var b = F.bind().bind(); var o = new F();
		  String(o instanceof b)`, "true"},
		{`function F() {}; var b = F.bind(); String({} instanceof b)`, "false"},
		{`function F() {}
		  Object.defineProperty(F, Symbol.hasInstance, {value: function () { return true }})
		  var b = F.bind(); String(1 instanceof b)`, "true"},
	}
	for _, tc := range cases {
		checkEval(t, tc.src, tc.want)
	}
}

// The Function constructor converts its parameters before its body, in the
// order they were written.
func TestFunctionConstructorConversionOrder(t *testing.T) {
	cases := []struct{ src, want string }{
		{`var seen = []
		  var p = {toString: function () { seen.push("p"); return "a" }}
		  var body = {toString: function () { seen.push("body"); return "return a" }}
		  var f = new Function(p, body)
		  seen.join(",") + ":" + f(1)`, "p,body:1"},
		{`var p = {toString: function () { throw 1 }}
		  var body = {toString: function () { throw "body" }}
		  try { new Function(p, body) } catch (e) { String(e) }`, "1"},
		{`var seen = []
		  var mk = function (s) { return {toString: function () { seen.push(s); return s }} }
		  new Function(mk("a"), mk("b"), mk("return a + b"))
		  seen.join(",")`, "a,b,return a + b"},
	}
	for _, tc := range cases {
		checkEval(t, tc.src, tc.want)
	}
}

// The first branch of a conditional is an AssignmentExpression with `in`
// permitted whatever the surroundings say, because the colon that follows it
// leaves no room for the head of a for-in to be mistaken for one. The second
// branch inherits, since nothing separates its end from what encloses it.
func TestConditionalBranchAllowsIn(t *testing.T) {
	cases := []struct{ src, want string }{
		{`var n = 0, m = 0
		  for (true ? "" in (n++, {}) : m++; false; ) ;
		  [n, m].join(",")`, "1,0"},
		{`var o = {a: 1}; String(false ? 0 : "a" in o)`, "true"},
		{`try { eval("for (true ? 1 : '' in {}; false; ) ;"); "no throw" }
		  catch (e) { e.constructor.name }`, "SyntaxError"},
		{`try { eval("for ('' in {}; false; ) ;"); "no throw" }
		  catch (e) { e.constructor.name }`, "SyntaxError"},
	}
	for _, tc := range cases {
		checkEval(t, tc.src, tc.want)
	}
}

// Only what the escapes spell is decoded. Every other character passes through
// untouched, a lone surrogate included: that is not a malformed URI, it is a
// character decodeURI has nothing to say about.
func TestDecodeURIPassesCharactersThrough(t *testing.T) {
	cases := []struct{ src, want string }{
		{`String(decodeURI("\uD800") === "\uD800")`, "true"},
		{`String(decodeURIComponent("\uDFFF\uD800") === "\uDFFF\uD800")`, "true"},
		{`decodeURIComponent("%E4%B8%AD a")`, "中 a"},
		{`decodeURIComponent("%F0%9F%98%80")`, "\U0001F600"},
		{`decodeURI("%3B%2F%3F%3A")`, "%3B%2F%3F%3A"},
		{`decodeURIComponent("%3B%2F%3F%3A")`, ";/?:"},
		{`decodeURIComponent("a%42c")`, "aBc"},
		// What an escape spells must be a character: a surrogate half, an
		// overlong form and a truncated sequence are all malformed.
		{`try { decodeURIComponent("%ED%A0%80") } catch (e) { e.constructor.name }`, "URIError"},
		{`try { decodeURIComponent("%C0%80") } catch (e) { e.constructor.name }`, "URIError"},
		{`try { decodeURIComponent("%E4%B8") } catch (e) { e.constructor.name }`, "URIError"},
		// A continuation must be an escape too; a literal one is a character of
		// its own.
		{`try { decodeURIComponent("%E4%B8­") } catch (e) { e.constructor.name }`, "URIError"},
		{`try { decodeURIComponent("%") } catch (e) { e.constructor.name }`, "URIError"},
		{`try { decodeURIComponent("%zz") } catch (e) { e.constructor.name }`, "URIError"},
	}
	for _, tc := range cases {
		checkEval(t, tc.src, tc.want)
	}
}

// A BigInt written as a string follows a narrower grammar than Go's own: no
// digit separators, a sign only on a decimal literal, and a leading zero makes
// nothing octal. And the conversion the operations use refuses a number, unlike
// the one the constructor uses.
func TestBigIntFromStrings(t *testing.T) {
	cases := []struct{ src, want string }{
		{`[String(BigInt("0x10")), String(BigInt("0b101")), String(BigInt("0o17")),
		   String(BigInt("017")), String(BigInt(" 12 ")), String(BigInt("")),
		   String(BigInt("-12")), String(BigInt("+12"))].join(",")`,
			"16,5,15,17,12,0,-12,12"},
		{`try { BigInt("0x1_0") } catch (e) { e.constructor.name }`, "SyntaxError"},
		{`try { BigInt("1_0") } catch (e) { e.constructor.name }`, "SyntaxError"},
		{`try { BigInt("-0x10") } catch (e) { e.constructor.name }`, "SyntaxError"},
		{`try { BigInt("1n") } catch (e) { e.constructor.name }`, "SyntaxError"},
		{`try { BigInt("0x") } catch (e) { e.constructor.name }`, "SyntaxError"},
		// A literal in source may carry separators; the lexer removes them.
		{`String(1_000n)`, "1000"},
		{`String(0x1_0n)`, "16"},
		// Comparisons convert the same way.
		{`[1n == " 1 ", 1n == "0x1", 1n == "1_0", 10n == "1_0", 1n < " 2 ",
		   2n > "1", 1n < "x"].join(",")`, "true,true,false,false,true,true,false"},
		// asIntN and asUintN use ToBigInt, which refuses a number.
		{`try { BigInt.asIntN(0, 1) } catch (e) { e.constructor.name }`, "TypeError"},
		{`try { BigInt.asUintN(0, 1) } catch (e) { e.constructor.name }`, "TypeError"},
		{`[String(BigInt.asIntN(3, 10n)), String(BigInt.asUintN(3, 10n)),
		   String(BigInt.asIntN(0, 10n))].join(",")`, "2,2,0"},
		{`String(BigInt.asIntN(3, "10"))`, "2"},
	}
	for _, tc := range cases {
		checkEval(t, tc.src, tc.want)
	}
}

// Some built-in functions are not merely equivalent to another but the very
// same function object, which a script can check.
func TestSharedBuiltinFunctions(t *testing.T) {
	cases := []struct{ src, want string }{
		{`(function () { return arguments[Symbol.iterator] === Array.prototype.values })()`,
			"true"},
		{`(function (a) { "use strict"
		   return arguments[Symbol.iterator] === Array.prototype.values })(1)`, "true"},
		{`String(Int8Array.prototype.toString === Array.prototype.toString)`, "true"},
		{`String(Array.prototype[Symbol.iterator] === Array.prototype.values)`, "true"},
		// And some are deliberately not: a typed array's locale form is its own.
		{`String(Int8Array.prototype.toLocaleString === Array.prototype.toLocaleString)`,
			"false"},
		{`(function () { return [...arguments].join(",") })(1, 2)`, "1,2"},
		{`String(new Int8Array([1, 2]).toString())`, "1,2"},
	}
	for _, tc := range cases {
		checkEval(t, tc.src, tc.want)
	}
}

// Own keys come out in one order whatever order they were created in: the
// indices first, ascending, then the string keys as they were added -- a
// function's synthesized length and name among them, before anything the
// function was given afterwards.
func TestFunctionOwnKeyOrder(t *testing.T) {
	cases := []struct{ src, want string }{
		{`class C { static [1]() {} static [2]() {} static a() {} static c() {} }
		  Object.getOwnPropertyNames(C).join(",")`, "1,2,length,name,prototype,a,c"},
		{`function f() {}; f.b = 1; f[0] = 2
		  Object.getOwnPropertyNames(f).join(",")`, "0,length,name,prototype,b"},
		{`var o = {[2]: 1, b: 2, [0]: 3, a: 4}
		  Object.getOwnPropertyNames(o).join(",")`, "0,2,b,a"},
		{`Object.getOwnPropertyNames(function () {}).join(",")`, "length,name,prototype"},
		{`Object.getOwnPropertyNames(() => {}).join(",")`, "length,name"},
	}
	for _, tc := range cases {
		checkEval(t, tc.src, tc.want)
	}
}
