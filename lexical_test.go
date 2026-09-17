package quickjs_test

import (
	"strings"
	"testing"

	quickjs "github.com/go-quickjs/go-quickjs"
)

// An arrow function has no bindings of its own: this, new.target, super and
// arguments all come from where it was written. That is the whole reason to
// reach for one instead of an ordinary function, so it is pinned here.
func TestArrowIsLexical(t *testing.T) {
	cases := []struct{ src, want string }{
		{`var o = {m() { return (() => this)() === o; }}; String(o.m())`, "true"},
		{`var o = {m() { var f = () => this; return f() === o; }}; String(o.m())`, "true"},
		{`var o = {m() { return (() => (() => this)())() === o; }}; String(o.m())`, "true"},
		// The captured this survives being called with a different receiver.
		{`var o = {m() { return () => this; }}; var f = o.m();
		  String(f.call({}) === o)`, "true"},
		{`function F() { this.v = 1; this.g = () => this.v; }
		  String(new F().g())`, "1"},
		{`class C { constructor() { this.v = 2; } m() { return () => this.v; } }
		  String(new C().m()())`, "2"},

		// super in an arrow resolves against the enclosing method's home
		// object.
		{`class A { m() { return 1; } }
		  class B extends A { n() { return (() => super.m())(); } }
		  String(new B().n())`, "1"},
		{`class A { m() { return 1; } }
		  class B extends A { constructor() { super(); this.f = () => super.m(); } }
		  String(new B().f())`, "1"},
		{`var o = {m() { return (() => super.toString)(); }}; typeof o.m()`, "function"},

		// new.target too: the arrow sees the enclosing call's, not undefined.
		{`var seen; function F() { (() => { seen = new.target; })(); }
		  new F(); String(seen === F)`, "true"},
		{`var seen; function F() { (() => { seen = new.target; })(); }
		  F(); String(seen)`, "undefined"},
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

// TestSuperHomeObject pins where super looks, which is the object a method was
// defined on rather than anything about the call.
func TestSuperHomeObject(t *testing.T) {
	cases := []struct{ src, want string }{
		// A shorthand method in an object literal may use super; the literal's
		// prototype is what it reaches.
		{`var p = {greet() { return "p"; }};
		  var o = {__proto__: p, greet() { return "o+" + super.greet(); }};
		  o.greet()`, "o+p"},
		{`var o = {m() { return typeof super.toString; }}; o.m()`, "function"},
		// A method taken off its object keeps its home object.
		{`var p = {greet() { return "p"; }};
		  var o = {__proto__: p, greet() { return super.greet(); }};
		  var f = o.greet; f.call({})`, "p"},

		// In a constructor, super reaches the parent's prototype.
		{`class A { m() { return 1; } }
		  class B extends A { constructor() { super(); this.v = super.m(); } }
		  String(new B().v)`, "1"},
		{`class A { static m() { return 1; } }
		  class B extends A { static n() { return super.m(); } }
		  String(B.n())`, "1"},
		{`class A { get x() { return 1; } }
		  class B extends A { get x() { return super.x + 1; } }
		  String(new B().x)`, "2"},
		// A computed method name does not displace the home object.
		{`class A { m() { return 1; } }
		  class B extends A { ["m"]() { return super.m() + 1; } }
		  String(new B().m())`, "2"},
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

// TestGeneratorParametersBindAtCall pins that a generator evaluates its
// parameters when it is created rather than on the first next().
func TestGeneratorParametersBindAtCall(t *testing.T) {
	cases := []struct{ src, want string }{
		{`var iter = {};
		  iter[Symbol.iterator] = function () { throw new TypeError("boom"); };
		  function* g([x]) {}
		  try { g(iter); "no error" } catch (e) { e.message }`, "boom"},
		{`var iter = {};
		  iter[Symbol.iterator] = function () { throw new TypeError("boom"); };
		  async function* g([x]) {}
		  try { g(iter); "no error" } catch (e) { e.message }`, "boom"},
		// An async function rejects instead: it never throws synchronously.
		{`var iter = {};
		  iter[Symbol.iterator] = function () { throw new TypeError("boom"); };
		  async function f([x]) {}
		  typeof f(iter).then`, "function"},
		// A default runs at the call, whether or not the body ever does.
		{`var n = 0; function* g(a = n++) { yield a; } g(); g(); String(n)`, "2"},

		// And the ordinary cases still work.
		{`function* g(a, b) { yield a; yield b; } [...g(1, 2)].join(",")`, "1,2"},
		{`function* g([a, b]) { yield a + b; } [...g([1, 2])].join(",")`, "3"},
		{`function* g({x} = {x: 5}) { yield x; } [...g()].join(",")`, "5"},
		{`function* g(...r) { yield r.length; } [...g(1, 2, 3)].join(",")`, "3"},
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

// TestYieldStarForwards pins that delegation passes values in both directions
// and evaluates to what the delegate returned.
func TestYieldStarForwards(t *testing.T) {
	cases := []struct{ src, want string }{
		{`function* inner() { yield 1; yield 2; return 3; }
		  function* outer() { var r = yield* inner(); yield r; }
		  [...outer()].join(",")`, "1,2,3"},
		// The value sent to the outer generator reaches the inner one.
		{`function* inner() { var a = yield 1; var b = yield a * 2; return b; }
		  function* outer() { return yield* inner(); }
		  var it = outer();
		  JSON.stringify([it.next().value, it.next(5).value, it.next(9).value])`,
			"[1,10,9]"},
		{`function* g() { yield* [1, 2]; yield 3; } [...g()].join(",")`, "1,2,3"},
		{`function* g() { yield* "ab"; } [...g()].join(",")`, "a,b"},
		// An array iterator returns undefined, which is then the value of the
		// whole expression.
		{`function* g() { var v = yield* [1, 2]; yield v; } JSON.stringify([...g()])`,
			"[1,2,null]"},
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

// TestTopLevelAwait pins that a module may await at its top level, which is
// what lets it finish loading something before its importers run.
func TestTopLevelAwait(t *testing.T) {
	cases := []struct{ src, want string }{
		{`export var v = await 9;`, "9"},
		{`var p = Promise.resolve(42); export var v = await p;`, "42"},
		{`async function f() { return await 1; } export var v = await f();`, "1"},
		{`try { await Promise.reject(new Error("x")); } catch (e) {}
		  export var v = "caught";`, "caught"},
		{`var out = []; for await (const x of [1, 2]) out.push(x);
		  export var v = out.join(",");`, "1,2"},
		// A module without one is unaffected.
		{`export var v = 1;`, "1"},
	}

	for _, tc := range cases {
		rt := quickjs.New()
		ns, err := rt.EvalModule("m.js", tc.src)
		if err != nil {
			t.Errorf("%s: %v", tc.src, err)
			rt.Close()
			continue
		}
		got, err := ns.Get("v")
		if err != nil {
			t.Errorf("%s: %v", tc.src, err)
		} else if got.String() != tc.want {
			t.Errorf("%s: v = %q, want %q", tc.src, got.String(), tc.want)
		}
		rt.Close()
	}

	// A rejected top-level await fails the module.
	rt := quickjs.New()
	defer rt.Close()
	if _, err := rt.EvalModule("m.js", `await Promise.reject(new TypeError("bang"));`); err == nil {
		t.Error("a rejected top-level await should fail the module")
	}

	// await is reserved throughout module code, even inside a plain function
	// where no await expression would be legal.
	for _, src := range []string{`var await = 1;`, `function f() { var await = 1; }`} {
		rt := quickjs.New()
		if _, err := rt.EvalModule("m.js", src); err == nil {
			t.Errorf("%s: accepted, want SyntaxError", src)
		}
		rt.Close()
	}
}

// TestGeneratorFunctionIntrinsics pins the prototype chain a generator, async or
// async generator function sits on. All three are ordinary functions as far as
// calling goes, but each has its own intrinsic prototype so that
// Object.prototype.toString names it and a generator's .prototype inherits
// next, return and throw.
func TestGeneratorFunctionIntrinsics(t *testing.T) {
	cases := []struct{ src, want string }{
		{`function* g() {} Object.prototype.toString.call(g)`, "[object GeneratorFunction]"},
		{`async function a() {} Object.prototype.toString.call(a)`, "[object AsyncFunction]"},
		{`async function* g() {} Object.prototype.toString.call(g)`,
			"[object AsyncGeneratorFunction]"},
		{`function* g() {} Object.prototype.toString.call(g())`, "[object Generator]"},

		{`function* g() {} String(Object.getPrototypeOf(g) === Function.prototype)`, "false"},
		{`function* g() {} Object.getPrototypeOf(g).constructor.name`, "GeneratorFunction"},
		{`async function a() {} Object.getPrototypeOf(a).constructor.name`, "AsyncFunction"},
		{`async function* g() {} Object.getPrototypeOf(g).constructor.name`,
			"AsyncGeneratorFunction"},
		// A generator's .prototype is what its generator objects inherit from.
		{`function* g() {}
		  String(Object.getPrototypeOf(g.prototype) === Object.getPrototypeOf(g).prototype)`,
			"true"},
		{`function* g() {} typeof g.prototype.next`, "function"},
		// It carries no constructor back-reference, since nothing constructs it.
		{`function* g() {} String(Object.getOwnPropertyDescriptor(g.prototype, "constructor"))`,
			"undefined"},

		// A completed generator keeps answering the same way however many times
		// it is asked.
		{`function* g() { yield 1; } var it = g(); it.next(); it.next();
		  JSON.stringify(it.next())`, `{"done":true}`},
		{`function* g() { yield 1; } var it = g(); it.next(); it.next();
		  String(it.next().value)`, "undefined"},
		{`function* g() { return 5; } var it = g(); JSON.stringify(it.next())`,
			`{"value":5,"done":true}`},
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

// TestSloppyThisCoercion pins that a sloppy-mode function called without a
// receiver gets the global object, and a primitive receiver gets its wrapper.
// Strict mode leaves both exactly as passed, which is the difference that lets
// it detect a missing receiver at all.
func TestSloppyThisCoercion(t *testing.T) {
	cases := []struct{ src, want string }{
		{`function f() { return this === globalThis; } String(f())`, "true"},
		{`function f() { this.x = 1; return this.x; } String(f())`, "1"},
		// An arrow inside sees the coerced value, since it captures it.
		{`function f() { return (() => this === globalThis)(); } String(f())`, "true"},
		// So does a direct eval, and a parameter default.
		{`function f() { return eval("this === globalThis"); } String(f())`, "true"},
		{`function f(a = this) { return a === globalThis; } String(f())`, "true"},
		// A primitive receiver becomes its wrapper.
		{`function f() { return typeof this; } String(f.call(5))`, "object"},
		{`function f() { return this.valueOf(); } String(f.call(5))`, "5"},

		{`function f() { "use strict"; return this; } String(f())`, "undefined"},
		{`function f() { "use strict"; return typeof this; } String(f.call(5))`, "number"},
		// A method call already has an object receiver and is unaffected.
		{`var o = {m() { return this === o; }}; String(o.m())`, "true"},
		// Construction is unaffected: this is the new object.
		{`function f() {} String(new f() instanceof f)`, "true"},
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

// A temporal dead zone violation is an ordinary runtime throw, not something
// the engine reports on its own behalf: a `try` around it catches it like any
// other error. It was not, which made every test262 case that asserts the
// throw fail even though the throw itself was right.
func TestDeadZoneIsCatchable(t *testing.T) {
	cases := []struct{ src, want string }{
		// A read and a write, at the top level and inside a function.
		{`var r; try { x } catch (e) { r = e.constructor.name } let x; r`,
			"ReferenceError"},
		{`var r; try { x = 1 } catch (e) { r = e.constructor.name } let x; r`,
			"ReferenceError"},
		{`function f() { try { x } catch (e) { return e.constructor.name } let x; }
		  f()`, "ReferenceError"},
		{`function f() { try { x = 1 } catch (e) { return e.constructor.name } let x; }
		  f()`, "ReferenceError"},

		// Through a closure, where the binding is reached as an upvalue.
		{`function o() {
		    function i() { try { y } catch (e) { return e.constructor.name } }
		    var r = i(); let y; return r;
		  } o()`, "ReferenceError"},
		{`function o() {
		    function i() { try { y = 1 } catch (e) { return e.constructor.name } }
		    var r = i(); let y; return r;
		  } o()`, "ReferenceError"},
		// The dead zone outranks constness: the binding does not exist yet, so
		// the complaint is that rather than that it cannot be written.
		{`function o() {
		    function i() { try { y = 1 } catch (e) { return e.constructor.name } }
		    var r = i(); const y = 2; return r;
		  } o()`, "ReferenceError"},
		// Once it is initialized, writing to a const is the TypeError again.
		{`function o() {
		    const y = 1;
		    function i() { try { y = 2 } catch (e) { return e.constructor.name } }
		    return i();
		  } o()`, "TypeError"},

		// The destructuring form test262 uses: the pattern's target is in its
		// dead zone, so the loop body never runs and the throw is catchable.
		{`var n = 0, r;
		  try { for ([x] of [[]]) { n += 1 } } catch (e) { r = e.constructor.name }
		  let x; r + "," + n`, "ReferenceError,0"},

		// A closure that runs after the binding is initialized sees it.
		{`function o() { let y; function i() { y = 3 } i(); return y } String(o())`,
			"3"},
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

// A parameter list with a default, a pattern or a rest element binds its names
// one at a time, in order. Each takes the slot the interpreter fills
// positionally, and a name that has not been bound yet may not be read.
func TestParametersBindInOrder(t *testing.T) {
	cases := []struct{ src, want string }{
		// A parameter after a pattern still gets its own argument. The pattern's
		// names used to take the slots the parameters after it were filled
		// through, so everything past the first pattern read the wrong one.
		{`function f({a}, b) { return a + "," + b } f({a: 1}, 9)`, "1,9"},
		{`function f({a} = {x: 1}, b) { return a + "," + b } f(undefined, 9)`,
			"undefined,9"},
		{`function f(x, {a}, b) { return [x, a, b].join(",") } f(1, {a: 2}, 3)`,
			"1,2,3"},
		{`function f([a], [b], c) { return [a, b, c].join(",") } f([1], [2], 3)`,
			"1,2,3"},
		{`var g = ({a}, b) => a + "," + b; g({a: 1}, 2)`, "1,2"},
		{`function* g({a}, b) { yield a + "," + b } g({a: 1}, 2).next().value`, "1,2"},

		// An earlier parameter is visible to a later default; a later one is
		// not visible to an earlier default, whichever way round it is read.
		{`function f(a, b = a) { return a + "," + b } f(1)`, "1,1"},
		{`function f(a = 1, b = a + 1) { return a + "," + b } f()`, "1,2"},

		// The arguments object exists before the parameters are bound, so a
		// default may read it.
		{`function f(x = arguments[1], y) { return x + "," + y } f(undefined, 2)`,
			"2,2"},
		{`class C { m(x = arguments[2], y = arguments[3], z) { return [x, y, z].join(",") } }
		  C.prototype.m(undefined, undefined, "third", "fourth")`,
			"third,fourth,third"},

		// A rest parameter is filled from the argument list rather than
		// positionally, and what came before it is unaffected.
		{`function f(a, ...r) { return a + "|" + r.join(",") } f(1, 2, 3)`, "1|2,3"},
		{`function f(a, ...[b, c]) { return [a, b, c].join(",") } f(1, 2, 3)`, "1,2,3"},

		// Passing undefined is the same as passing nothing, which is what makes
		// a default run.
		{`function f(a = 1, b = 2) { return a + "," + b } f(undefined, 5)`, "1,5"},
		{`function f(a) { return String(a) } f(undefined)`, "undefined"},
		{`function f(a) { return String(a) } f()`, "undefined"},
		{`function f({a} = {a: 3}) { return String(a) } f(undefined)`, "3"},
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

	// Reading a parameter the prologue has not reached is a ReferenceError,
	// including the one being initialized.
	for _, src := range []string{
		`function f(x = y, y) { return x } f()`,
		`function f(x = y, y) { return x } f(undefined, 1)`,
		`function f(x = x) { return x } f()`,
		`function f(a, b = c, c) { return b } f(1)`,
		`var g = (x = y, y) => x; g()`,
		`function* g(x = y, y) { yield x } g().next()`,
	} {
		rt := quickjs.New()
		_, err := rt.Eval(src)
		if err == nil {
			t.Errorf("%s: accepted, want ReferenceError", src)
		} else if !strings.Contains(err.Error(), "ReferenceError") {
			t.Errorf("%s: got %v, want ReferenceError", src, err)
		}
		rt.Close()
	}
}
