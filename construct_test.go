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
