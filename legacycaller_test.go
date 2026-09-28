package quickjs_test

import (
	"testing"

	quickjs "github.com/go-quickjs/go-quickjs"
)

// legacyCallerCases are what a sloppy function's caller and arguments answer
// under WithNodeQuirks, as V8, SpiderMonkey and JavaScriptCore agree: the
// function whose code made the call, passing over script and eval code and
// the built-ins that only pass a call on, and null for a caller that is strict
// code or a built-in that calls back; and a copy of the arguments.
var legacyCallerCases = []struct{ src, want string }{
	{`function f() { return f.caller } function g() { return f() } g() === g`, "true"},
	{`function f() { return f.caller } String(f())`, "null"},
	{`function f() { return f.caller } String(f.caller)`, "null"},
	{`function f() { return f.caller } function g() { return f.call() } g() === g`, "true"},
	{`function f() { return f.caller } function g() { return f.apply(null, []) } g() === g`, "true"},
	{`function f() { return f.caller } function g() { return Reflect.apply(f, null, []) } g() === g`, "true"},
	{`function f() { return f.caller } function g() { return eval("f()") } g() === g`, "true"},
	{`function f() { return f.caller } function g() { return (0, eval)("f()") } g() === g`, "true"},
	{`function f() { return f.caller } const b = f.bind(null); function g() { return b() } g() === g`, "true"},
	{`function f() { return f.caller } const g = () => f(); g() === g`, "true"},
	{`function f() { return f.caller } function* g() { yield f() } g().next().value === g`, "true"},
	{`function f() { return f.caller } function s() { "use strict"; return f() } String(s())`, "null"},
	{`function f() { return f.caller } class K { static m() { return f() } } String(K.m())`, "null"},
	{`function f() { return f.caller } String([1].map(function m() { return m.caller })[0])`, "null"},
	{`function r(n) { return n ? r(n - 1) : r.caller } function g() { return r(2) } g() === r`, "true"},
	{`function f() { return f.caller.caller } function g() { return f() } function top() { return g() } top() === top`, "true"},
	{`var o = {}; Object.defineProperty(o, "x", {get: function G() { return G.caller }}); function g() { return o.x } g() === g`, "true"},
	{`function f() { return f.caller } Function("return f()")().name`, "anonymous"},

	{`function f(a, b) { a = 5; return [...f.arguments].join() } f(1, 2, 3)`, "5,2,3"},
	{`function f(a) { const A = f.arguments; a = 9; return A[0] } f(1)`, "1"},
	{`function f(a) { f.arguments[0] = 7; return a } f(1)`, "1"},
	{`function f(a) { arguments[0] = 9; return [...f.arguments].join() } f(1, 2)`, "1,2"},
	{`function f(a) { a = 5; const g = () => a; return f.arguments[0] } f(1)`, "1"},
	{`function f(a = 1) { return [...f.arguments].join() } f(4)`, "4"},
	{`function f() { return f.arguments === f.arguments } f()`, "false"},
	{`function f() { const A = f.arguments; return Object.prototype.toString.call(A) + " " + (A.callee === f) + " " + A.length } f(1, 2)`, "[object Arguments] true 2"},
	{`function f() { return Object.getOwnPropertyNames(f.arguments).join() } f(1)`, "0,length,callee"},
	{`function f() {} String(f.arguments)`, "null"},

	{`function f() {} f.caller = 5; String(f.caller)`, "null"},
	{`var e; try { (function () { "use strict" }).caller } catch (x) { e = x } e instanceof TypeError`, "true"},
	{`var e; try { (() => 1).arguments } catch (x) { e = x } e instanceof TypeError`, "true"},
	{`var e; try { ({m() {}}).m.caller } catch (x) { e = x } e instanceof TypeError`, "true"},
	{`var e; try { Array.prototype.map.caller } catch (x) { e = x } e instanceof TypeError`, "true"},
	{`var e; try { (async function () {}).caller } catch (x) { e = x } e instanceof TypeError`, "true"},
	{`var e; try { (function () {}).bind(null).caller } catch (x) { e = x } e instanceof TypeError`, "true"},
}

// TestLegacyCaller pins a sloppy function's caller and arguments, which
// WithNodeQuirks answers, and which standards mode refuses as the standard
// has it, with %ThrowTypeError%; and that a function that has neither throws
// V8's TypeError under WithNodeQuirks.
func TestLegacyCaller(t *testing.T) {
	for _, c := range legacyCallerCases {
		rt := quickjs.New(quickjs.WithNodeQuirks())
		v, err := rt.Eval(c.src)
		if err != nil {
			t.Errorf("%s: %v", c.src, err)
		} else if v.String() != c.want {
			t.Errorf("%s = %q, want %q", c.src, v, c.want)
		}
		rt.Close()
	}
	for _, src := range []string{
		`function f() { return f.caller } function g() { return f() } g()`,
		`function f() { return f.arguments } f(1)`,
		`function f() {} f.caller = 1`,
	} {
		checkEval(t, `try { `+src+`; "answered" } catch (e) { e.constructor.name + ": " + e.message }`,
			"TypeError: this property may not be read or written")
	}
	rt := quickjs.New(quickjs.WithNodeQuirks())
	defer rt.Close()
	if v, err := rt.Eval(`try { (function () { "use strict" }).caller } catch (e) { e.message }`); err != nil ||
		v.String() != "'caller', 'callee', and 'arguments' properties may not be accessed on strict mode functions or the arguments objects for calls to them" {
		t.Errorf("quirks message = %v, %v", v, err)
	}
}

// TestLegacyCallerOwnProperties pins where caller and arguments live: on
// Function.prototype in both modes, as the standard's %ThrowTypeError% or as
// V8's accessors, and never on a function itself, whose own keys are every
// engine's.
func TestLegacyCallerOwnProperties(t *testing.T) {
	const src = `
		const tte = Object.getOwnPropertyDescriptor(function () { "use strict"; return arguments }(), "callee").get;
		const proto = Object.getOwnPropertyDescriptor(Function.prototype, "caller");
		const args = Object.getOwnPropertyDescriptor(Function.prototype, "arguments");
		[
			proto.get === tte && proto.set === tte && args.get === tte,
			proto.get.name, proto.set.name, proto.set.length, args.get.name,
			proto.enumerable, proto.configurable,
			Object.getOwnPropertyNames(function () {}).join(),
			Object.hasOwn(function () {}, "caller"),
		].join(" | ")`
	for _, c := range []struct {
		opts []quickjs.Option
		want string
	}{
		{nil, "true |  |  | 0 |  | false | true | length,name,prototype | false"},
		{[]quickjs.Option{quickjs.WithNodeQuirks()},
			"false | get caller | set caller | 1 | get arguments | false | true | length,name,prototype | false"},
	} {
		rt := quickjs.New(c.opts...)
		if v, err := rt.Eval(src); err != nil || v.String() != c.want {
			t.Errorf("quirks=%v:\n got %v, %v\nwant %s", len(c.opts) > 0, v, err, c.want)
		}
		rt.Close()
	}
}
