package quickjs_test

import "testing"

// TestNumberAndCaseFastPaths covers the quick ways of two conversions: an
// integer below 2^53 turned into a string, and an ASCII string's case changed.
// Past their bounds -- a larger integer, a fraction, a string with a character
// outside ASCII -- the general way must still give the same answers.
func TestNumberAndCaseFastPaths(t *testing.T) {
	cases := []struct{ src, want string }{
		{`[0, -0, 7, -7, 42, 2**31, -(2**31) - 1, 2**53 - 1, -(2**53 - 1), 2**53, -(2**53), 2**53 + 2,
		   2**60, 1e20, 1e21, 123456789012345680000, 0.1, -1.5, 1e-7, 2**-1074]
		   .map(String).join()`,
			"0,0,7,-7,42,2147483648,-2147483649,9007199254740991,-9007199254740991,9007199254740992,-9007199254740992," +
				"9007199254740994,1152921504606847000,100000000000000000000,1e+21,123456789012345680000,0.1,-1.5,1e-7,5e-324"},
		{"`${2**53 - 1}|${-0}|${2**60}|${1/3}`", "9007199254740991|0|1152921504606847000|0.3333333333333333"},
		{`var s = "Hello, World 123!";
		  [s.toUpperCase(), s.toLowerCase(), "abc".toLowerCase() === "abc", "".toUpperCase(),
		   "@[\x60{".toUpperCase(), "@[\x60{".toLowerCase(), "straße".toUpperCase(), "ΑΣ".toLowerCase(),
		   "i\u0307".toUpperCase() === "I\u0307"].join("|")`,
			"HELLO, WORLD 123!|hello, world 123!|true||@[`{|@[`{|STRASSE|ας|true"},
	}
	for _, tc := range cases {
		checkEval(t, tc.src, tc.want)
	}
}

// TestMathCallFastPaths covers Math functions applied without a call: a
// function of one number, and min or max of two. Anything else -- an argument
// to convert, one missing, a replaced method -- still makes the call, with
// its conversions in order and its frame in a stack trace.
func TestMathCallFastPaths(t *testing.T) {
	cases := []struct{ src, want string }{
		{`[Math.floor(-1.5), Math.ceil(-0.5), Math.round(-2.5), Math.round(2.5), Math.sign(-0), Math.abs(-0),
		   Math.sqrt(-1), Math.trunc(-0.9), Math.fround(5.5), Math.floor(), Math.floor(NaN), Math.cbrt(27)]
		   .map(v => Object.is(v, -0) ? "-0" : String(v)).join()`, "-2,-0,-2,3,-0,0,NaN,-0,5.5,NaN,NaN,3"},
		{`var log = [], o = {valueOf() { log.push("v"); return 4.5 }};
		  [Math.floor(o), Math.floor("7.9"), Math.floor.call(null, 2.5), Math.abs(-3, o), log.join("")].join()`, "4,7,2,3,v"},
		{`function f(x) { return Math.floor(x) }
		  var saved = Math.floor, r = [f(1.5)]; Math.floor = x => "mine " + x; r.push(f(1.5)); Math.floor = saved; r.push(f(1.5))
		  try { f({valueOf() { throw new Error("boom") }}) } catch (e) { r.push(/Math\.floor|floor/.test(e.stack)) }
		  r.join()`, "1,mine 1.5,1,true"},
		{`[Math.max(1, 2), Math.min(1, 2), Math.max(-0, 0), Math.max(0, -0), Math.min(-0, 0), Math.min(0, -0),
		   Math.max(NaN, 1), Math.min(1, NaN), Math.max(), Math.min(), Math.max(3), Math.max(1, 5, 2), Math.min(2, "1")]
		   .map(v => Object.is(v, -0) ? "-0" : String(v)).join()`, "2,1,0,0,-0,-0,NaN,NaN,-Infinity,Infinity,3,5,1"},
	}
	for _, tc := range cases {
		checkEval(t, tc.src, tc.want)
	}
}

// TestCallThrough covers f.call(x, ...) made from the interpreter's loop
// without going through call's own call: the receiver, strict and sloppy, the
// arguments, call applied to itself and to built-ins, and what is not a
// function.
func TestCallThrough(t *testing.T) {
	cases := []struct{ src, want string }{
		{`function who() { return this === globalThis ? "global" : String(this) }
		  function strictWho() { "use strict"; return String(this) }
		  function args() { return arguments.length + ":" + [].join.call(arguments) }
		  [who.call(7), who.call(), who.call(null), strictWho.call(), strictWho.call(null), strictWho.call(7),
		   args.call(0), args.call(0, 1, 2), Function.prototype.call.call(args, 0, "a"),
		   Math.max.call(null, 3, 9), [].slice.call("abc").join("")].join()`,
			"7,global,global,undefined,null,7,0:,2:1,2,1:a,9,abc"},
		{`function Base(v) { this.v = v } function Derived(v) { Base.call(this, v * 2); this.w = v }
		  var d = new Derived(3); [d.v, d.w, d instanceof Derived].join()`, "6,3,true"},
		{`var r = []
		  try { ({}).nope.call(null) } catch (e) { r.push(e.constructor.name) }
		  try { Function.prototype.call.call(5) } catch (e) { r.push(e.constructor.name) }
		  try { Function.prototype.call.call({}) } catch (e) { r.push(e.constructor.name) }
		  try { (class A {}).call({}) } catch (e) { r.push(e.constructor.name) }
		  r.join()`, "TypeError,TypeError,TypeError,TypeError"},
	}
	for _, tc := range cases {
		checkEval(t, tc.src, tc.want)
	}
}
