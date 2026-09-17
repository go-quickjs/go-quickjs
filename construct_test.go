package quickjs_test

import "testing"

// A built-in constructor reads new.target's prototype exactly once, and at the
// point its specification says: after the checks that come before it, before
// the work that comes after.
func TestNativeConstructorPrototypeLookup(t *testing.T) {
	// The helper counts the reads a construction makes.
	const setup = `
		function reads(C, args) {
			var n = 0
			var nt = function () {}.bind()
			Object.defineProperty(nt, "prototype", {get: function () { n++; return {} }})
			try { Reflect.construct(C, args || [], nt) } catch (e) { return "threw " + n }
			return String(n)
		}
	`
	cases := []struct{ name, src, want string }{
		// One read each, whether the constructor asks for it itself or has it
		// applied afterwards.
		{"once for a collection", setup + `reads(Map)`, "1"},
		{"once for an array", setup + `reads(Array)`, "1"},
		{"once for an error", setup + `reads(Error)`, "1"},
		{"once for a promise", setup + `reads(Promise, [function () {}])`, "1"},
		// The prototype new.target names is the one the instance gets.
		{"applies to the instance", `
		  function nt() {}
		  nt.prototype = {tag: "custom"}
		  Reflect.construct(Array, [], nt).tag + "," +
		  Reflect.construct(Error, [], nt).tag + "," +
		  Reflect.construct(Map, [], nt).tag + "," +
		  Reflect.construct(Promise, [function () {}], nt).tag`,
			"custom,custom,custom,custom"},

		// Promise checks its arguments before reading the prototype, and reads
		// it before running the executor.
		{"executor checked first", `
		  var nt = function () {}.bind()
		  Object.defineProperty(nt, "prototype", {get: function () { throw new RangeError() }})
		  try { Reflect.construct(Promise, [], nt); "no error" }
		  catch (e) { e.constructor.name }`, "TypeError"},
		{"prototype read before the executor", `
		  var ran = false
		  var nt = function () {}.bind()
		  Object.defineProperty(nt, "prototype", {get: function () { throw new RangeError() }})
		  var caught = ""
		  try { Reflect.construct(Promise, [function () { ran = true }], nt) }
		  catch (e) { caught = e.constructor.name }
		  caught + "," + ran`, "RangeError,false"},
		{"promise needs new", `try { Promise(function () {}); "no error" }
		  catch (e) { e.constructor.name }`, "TypeError"},
		{"promise call needs new", `try { Promise.call(null, function () {}); "no error" }
		  catch (e) { e.constructor.name }`, "TypeError"},

		// An ArrayBuffer exists before its storage does.
		{"buffer object before storage", `
		  var nt = function () {}.bind()
		  Object.defineProperty(nt, "prototype", {get: function () { throw new RangeError() }})
		  try { Reflect.construct(ArrayBuffer, [7 * 1125899906842624], nt); "no error" }
		  catch (e) { e.constructor.name }`, "RangeError"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) { checkEval(t, tc.src, tc.want) })
	}
}

// A constructor that names no prototype falls back to the one of the realm it
// came from, and a revoked proxy has no realm left to be asked about.
func TestConstructThroughARevokedProxy(t *testing.T) {
	cases := []struct{ src, want string }{
		{`var handle
		  handle = Proxy.revocable(function () {}, {get: function () { handle.revoke() }})
		  try { new handle.proxy(); "no throw" } catch (e) { e.constructor.name }`, "TypeError"},
		{`var h = Proxy.revocable(function () {}, {}); h.revoke()
		  try { new h.proxy(); "no throw" } catch (e) { e.constructor.name }`, "TypeError"},
		// A live proxy constructs through its target.
		{`var p = new Proxy(function (a) { this.a = a }, {})
		  String(new p(5).a)`, "5"},
	}
	for _, tc := range cases {
		checkEval(t, tc.src, tc.want)
	}
}

// A typed array's length is converted before new.target is asked what
// prototype the view should have: an argument that cannot be a length is
// refused first.
func TestTypedArrayLengthIsConvertedFirst(t *testing.T) {
	const target = `var newTarget = function () {}.bind(null)
		Object.defineProperty(newTarget, "prototype",
		  {get: function () { throw new RangeError() }})
		`
	cases := []struct{ src, want string }{
		{target + `try { Reflect.construct(Int8Array, [Symbol()], newTarget) }
		   catch (e) { e.constructor.name }`, "TypeError"},
		{target + `try { Reflect.construct(Int8Array, [-1], newTarget) }
		   catch (e) { e.constructor.name }`, "RangeError"},
		// With a length it can use, the prototype is what is read next.
		{target + `try { Reflect.construct(Int8Array, [2], newTarget) }
		   catch (e) { e.constructor.name }`, "RangeError"},
		// The ordinary paths are unchanged.
		{`class T extends Int8Array {}
		  var a = new T(3); [a.length, a instanceof T].join(",")`, "3,true"},
		{`[new Int8Array(2).length, new Int8Array([1, 2, 3]).length,
		   new Int8Array(new ArrayBuffer(4)).length].join(",")`, "2,3,4"},
		{`var a = new Int8Array(); a.length`, "0"},
	}
	for _, tc := range cases {
		checkEval(t, tc.src, tc.want)
	}
}
