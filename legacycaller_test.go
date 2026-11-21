package quickjs_test

import (
	"strings"
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
	{`function f(x) { const g = () => x; const a = f.arguments; a[0] = 9; return g() + ',' + a[0] } f(1)`, "1,9"},
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

// TestLegacyCaller pins caller access in both modes, legacy arguments under
// WithNodeQuirks, and the restricted properties of other function kinds.
func TestLegacyCaller(t *testing.T) {
	t.Parallel()
	for _, opts := range [][]quickjs.Option{nil, {quickjs.WithNodeQuirks()}} {
		for _, c := range legacyCallerCases {
			if len(opts) == 0 && strings.Contains(c.src, ".arguments") {
				continue
			}
			rt := quickjs.New(opts...)
			v, err := rt.Eval(c.src)
			if err != nil {
				t.Errorf("%s: %v", c.src, err)
			} else if v.String() != c.want {
				t.Errorf("%s = %q, want %q", c.src, v, c.want)
			}
			rt.Close()
		}
	}
	for _, src := range []string{
		`function f() { return f.arguments } f(1)`,
		`Function.prototype.caller`,
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

// TestLegacyCallerOwnProperties pins the standard prototype's thrower and
// sloppy functions' own caller accessor; compatibility mode inherits both
// caller and arguments from Function.prototype instead.
func TestLegacyCallerOwnProperties(t *testing.T) {
	t.Parallel()
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
		{nil, "true |  |  | 0 |  | false | true | length,name,prototype,caller | true"},
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

func TestStandardCaller(t *testing.T) {
	for _, mode := range []struct {
		name string
		opts []quickjs.Option
	}{
		{"interpreter", nil},
		{"jit", []quickjs.Option{quickjs.WithJIT()}},
		{"debugger", []quickjs.Option{quickjs.WithDebugger()}},
	} {
		t.Run(mode.name, func(t *testing.T) {
			for _, src := range []string{
				`function f() { return f.caller; }
				 function g() { return f(); }
				 function s() { "use strict"; return f(); }
				 for (let i = 0; i < 2000; i++) {
				   if (g() !== g || s() !== null) throw new Error('caller');
				 }
				 f.caller === null`,
				`function f() {}
				 const d = Object.getOwnPropertyDescriptor(f, 'caller');
				 const p = new Proxy(f, {});
				 Object.hasOwn(p, 'caller') && Reflect.ownKeys(p).includes('caller') &&
				 Reflect.getOwnPropertyDescriptor(p, 'caller').get === d.get &&
				 !d.enumerable && d.configurable && d.get.call(f) === null`,
				`function f() {}
				 Object.freeze(f);
				 f.caller === null && !Object.getOwnPropertyDescriptor(f, 'caller').configurable`,
				`function f() {}
				 f.extra = 1;
				 const before = Reflect.ownKeys(f).join();
				 f.caller; f.prototype;
				 before === 'length,name,prototype,caller,extra' && Reflect.ownKeys(f).join() === before`,
				`function f() {}
				 f.prototype;
				 const before = Reflect.ownKeys(f).join();
				 Object.getOwnPropertyDescriptor(f, 'caller');
				 before === 'length,name,prototype,caller' && Reflect.ownKeys(f).join() === before`,
				`function f() {}
				 delete f.caller;
				 let threw = false;
				 try { f.caller; } catch (e) { threw = e instanceof TypeError; }
				 threw && !Object.hasOwn(f, 'caller')`,
				`function f() {}
				 Object.defineProperty(f, 'caller', {value: 42});
				 f.caller === 42 && Object.getOwnPropertyDescriptor(f, 'caller').value === 42`,
				`function f() {}
				 f.caller; f.x = 1;
				 Reflect.ownKeys(f).join() === 'length,name,prototype,caller,x'`,
				`function f() {}
				 Object.defineProperty(f, 'name', {value: 'g'}); f.x = 1;
				 Reflect.ownKeys(f).join() === 'length,name,prototype,caller,x'`,
				`function f() {}
				 Object.preventExtensions(f);
				 f.caller === null && Object.hasOwn(f, 'caller') && !Object.isSealed(f) &&
				 (Object.seal(f), Object.isSealed(f))`,
				`function f() {} function g() {}
				 const d = Object.getOwnPropertyDescriptor(f, 'caller');
				 const e = Object.getOwnPropertyDescriptor(g, 'caller');
				 let threw = '';
				 try { d.get.call(() => {}); } catch (x) { threw = x.constructor.name; }
				 d.get === e.get && d.set === e.set && d.get.call(g) === null &&
				 d.get.name === 'get caller' && d.set.name === 'set caller' && threw === 'TypeError'`,
				`[function () { 'use strict'; }, () => {}, class {}, ({m() {}}).m,
				  function () {}.bind(), function* () {}, async function () {},
				  Math.max].every(f => !Object.hasOwn(f, 'caller') &&
				    !Reflect.ownKeys(f).includes('caller'))`,
				`Object.hasOwn(new Function(''), 'caller') &&
				 !Object.hasOwn(new Function('"use strict"'), 'caller')`,
				`let n = 0;
				 for (let i = 0; i < 3000; i++) {
				   const f = function () {}, g = function () {};
				   if (f.caller === null && Object.hasOwn(f, 'caller') && f.hasOwnProperty('caller') &&
				       g.hasOwnProperty('caller') && g.caller === null &&
				       Reflect.ownKeys(i % 2 ? f : g).join() === 'length,name,prototype,caller') n++;
				 }
				 n === 3000`,
				`const a = function () {}, b = function () {};
				 function read(f) { return f.caller; }
				 for (let i = 0; i < 2000; i++) {
				   if (read(i % 2 ? a : b) !== null) throw new Error('cached caller');
				 }
				 true`,
			} {
				rt := quickjs.New(mode.opts...)
				v, err := rt.Eval(src)
				if err != nil || v.String() != "true" {
					t.Errorf("%s: got %v, %v; want true", src, v, err)
				}
				rt.Close()
			}
		})
	}
}
