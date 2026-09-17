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

// TestGlobalLexicalEnvironment covers a script's top-level let, const and
// class. They are not properties of the global object, but they outlive the
// script that declared them: the next script sees them, and so does eval.
func TestGlobalLexicalEnvironment(t *testing.T) {
	rt := quickjs.New()
	defer rt.Close()
	steps := []struct{ src, want string }{
		{`let a = 1; const b = 2; class C {}`, "undefined"},
		{`a + b`, "3"},
		{`typeof C`, "function"},
		// Not a property of globalThis, which is what makes it lexical.
		{`String(globalThis.a) + "," + ("a" in globalThis)`, "undefined,false"},
		// Reachable from an indirect eval, which runs in global scope.
		{`(0, eval)("a")`, "1"},
		{`var f = function () { return eval("a") }; f()`, "1"},
		// A function declared in an earlier script closes over it too.
		{`function g() { return a } g()`, "1"},
		{`a = 9; a`, "9"},
		{`try { b = 1 } catch (e) { e.constructor.name }`, "TypeError"},
		{`String(delete a)`, "false"},
	}
	for _, st := range steps {
		v, err := rt.Eval(st.src)
		if err != nil {
			t.Fatalf("%s: %v", st.src, err)
		}
		if got := v.String(); got != st.want {
			t.Errorf("%s\n got: %s\nwant: %s", st.src, got, st.want)
		}
	}
}

// TestGlobalLexicalCollisions covers a name two scripts both declare. They
// share the global environment, so the collision is only visible at the point
// the second script runs.
func TestGlobalLexicalCollisions(t *testing.T) {
	bad := [][2]string{
		{`let a = 1`, `var a = 2`},
		{`var b = 1`, `let b = 2`},
		{`let c = 1`, `let c = 2`},
		{`let d = 1`, `const d = 2`},
		{`let e = 1`, `function e() {}`},
		{`function f() {}`, `let f = 1`},
		{`class G {}`, `let G = 1`},
	}
	for _, pair := range bad {
		rt := quickjs.New()
		if _, err := rt.Eval(pair[0]); err != nil {
			t.Errorf("%s: %v", pair[0], err)
		} else if _, err := rt.Eval(pair[1]); err == nil ||
			!strings.Contains(err.Error(), "SyntaxError") {
			t.Errorf("%s then %s: got %v, want a SyntaxError", pair[0], pair[1], err)
		}
		rt.Close()
	}

	// A script whose declarations cannot all be honoured leaves none of them
	// behind: everything is checked before anything is created.
	rt := quickjs.New()
	defer rt.Close()
	if _, err := rt.Eval(`let taken = 1`); err != nil {
		t.Fatal(err)
	}
	if _, err := rt.Eval(`var fresh = 1; var taken = 2`); err == nil {
		t.Error("a var colliding with a lexical binding should be a SyntaxError")
	}
	if v, err := rt.Eval(`typeof fresh`); err != nil || v.String() != "undefined" {
		t.Errorf("fresh = %v, %v; the failed script should have created nothing", v, err)
	}

	// A name only one of them declares is fine, and so is a duplicate inside
	// a block or a function.
	good := [][2]string{
		{`let h = 1`, `var i = 2; h + i`},
		{`let j = 1`, `{ let j = 2 }`},
		{`let k = 1`, `(function () { let k = 2 })()`},
		// A lexical binding eval declares belongs to the eval, and a var it
		// declares is configurable, so neither collides with a script's.
		{`let l = 1`, `eval("let l = 2"); l`},
		{`eval("var m = 1")`, `let m = 2; m`},
		{`eval("function n() {}")`, `const n = 2; n`},
	}
	for _, pair := range good {
		rt := quickjs.New()
		if _, err := rt.Eval(pair[0]); err != nil {
			t.Errorf("%s: %v", pair[0], err)
		} else if _, err := rt.Eval(pair[1]); err != nil {
			t.Errorf("%s then %s: %v", pair[0], pair[1], err)
		}
		rt.Close()
	}
}

// TestGlobalLexicalDeadZone covers a script-level lexical binding read before
// its declaration runs.
func TestGlobalLexicalDeadZone(t *testing.T) {
	cases := []struct{ src, want string }{
		{`try { x } catch (e) { e.constructor.name } finally {} ; let x = 1`, "ReferenceError"},
		{`function f() { return x } 
		  var caught = ""
		  try { f() } catch (e) { caught = e.constructor.name }
		  let x = 1
		  caught + "," + f()`, "ReferenceError,1"},
		{`var caught = ""
		  try { typeof y } catch (e) { caught = e.constructor.name }
		  let y = 1
		  caught`, "ReferenceError"},
		// An undeclared name is still undefined to typeof.
		{`typeof notDeclaredAnywhere`, "undefined"},
	}
	for _, tc := range cases {
		checkEval(t, tc.src, tc.want)
	}
}

// TestForHeadLexicalScope covers a for-in or for-of head that declares a name,
// which is in scope -- and in its dead zone -- while the expression beside it
// is evaluated.
func TestForHeadLexicalScope(t *testing.T) {
	cases := []struct{ src, want string }{
		{`try { (function () { let x = 1; for (let x of [x]) {} })() }
		  catch (e) { e.constructor.name }`, "ReferenceError"},
		{`try { (function () { let x = 1; for (const x of [x]) {} })() }
		  catch (e) { e.constructor.name }`, "ReferenceError"},
		{`try { (function () { let x = 1; for (let x in {a: x}) {} })() }
		  catch (e) { e.constructor.name }`, "ReferenceError"},
		// A var head has no scope of its own, so the name is the outer one.
		{`(function () { var x = 1; var out = []; for (var x of [x]) out.push(x)
		    return out.join() })()`, "1"},

		// The ordinary loops are unaffected, including the fresh binding each
		// iteration gets.
		{`var out = []; for (let x of [1, 2]) out.push(x); out.join()`, "1,2"},
		{`var out = []; for (const k in {a: 1, b: 2}) out.push(k); out.join()`, "a,b"},
		{`var out = []; for (let i of [1, 2]) out.push(() => i)
		  out.map(f => f()).join()`, "1,2"},
	}
	for _, tc := range cases {
		checkEval(t, tc.src, tc.want)
	}
}

// A `let` in a for head is a fresh binding each iteration, and the first one is
// no exception: a closure made in the init clause keeps what the init left
// there rather than following what the loop goes on to do.
func TestForHeadPerIterationBindings(t *testing.T) {
	cases := []struct{ src, want string }{
		{`var probeBefore, probeTest, probeIncr, probeBody, run = true
		  for (
		    let x = "outside", _ = probeBefore = function () { return x };
		    run && (x = "inside", probeTest = function () { return x });
		    probeIncr = function () { return x }
		  ) probeBody = function () { return x }, run = false;
		  [probeBefore(), probeTest(), probeBody(), probeIncr()].join(",")`,
			"outside,inside,inside,inside"},
		// The familiar case: each iteration's closure keeps its own count.
		{`var fs = []
		  for (let i = 0; i < 3; i++) fs.push(function () { return i })
		  fs.map(function (f) { return f() }).join(",")`, "0,1,2"},
		{`var fs = []
		  for (var i = 0; i < 3; i++) fs.push(function () { return i })
		  fs.map(function (f) { return f() }).join(",")`, "3,3,3"},
		// A continue is the end of an iteration too.
		{`var fs = []
		  for (let i = 0; i < 3; i++) { fs.push(function () { return i }); continue }
		  fs.map(function (f) { return f() }).join(",")`, "0,1,2"},
	}
	for _, tc := range cases {
		checkEval(t, tc.src, tc.want)
	}
}

// A switch's bindings belong to the case block as a whole and are created
// before the first case expression is evaluated, so a selector closes over the
// block's binding rather than over whatever the name means outside.
func TestSwitchCaseBlockScope(t *testing.T) {
	cases := []struct{ src, want string }{
		{`let x = "outside"
		  var probeExpr, probeSelector, probeStmt
		  switch (probeExpr = function () { return x }, null) {
		    case probeSelector = function () { return x }, null:
		      probeStmt = function () { return x }
		      let x = "inside"
		  }
		  [probeExpr(), probeSelector(), probeStmt()].join(",")`, "outside,inside,inside"},
		// A binding of one clause is in scope in the others, and a function
		// declaration in a clause is callable from an earlier one.
		{`switch (1) { case 1: f(); function f() { globalThis.out = "called" } } out`, "called"},
		{`try { eval("switch (1) { case 1: let a; case 2: let a; }"); "no throw" }
		  catch (e) { e.constructor.name }`, "SyntaxError"},
		{`switch (1) { case 1: let y = 5; break }
		  try { y; "leaked" } catch (e) { e.constructor.name }`, "ReferenceError"},
		// Falling through still works, and a block in a clause is its own scope.
		{`var out = []
		  switch (2) { case 1: out.push("one"); case 2: out.push("two"); default: out.push("d") }
		  out.join(",")`, "two,d"},
		{`switch (1) { case 1: { let z = 1 } case 2: { let z = 2 } } "ok"`, "ok"},
	}
	for _, tc := range cases {
		checkEval(t, tc.src, tc.want)
	}
}

// A function created inside a `with` body keeps the object it was created
// under, not whichever object a later `with` in the same function pushes.
func TestWithScopesAreCapturedPerClosure(t *testing.T) {
	cases := []struct{ src, want string }{
		{`var a = 1
		  var f
		  with ({a: 2}) { f = function () { return a } }
		  var r
		  with ({a: 3}) { r = f() }
		  String(r)`, "2"},
		{`var a = 1, fs = []
		  for (var i = 0; i < 3; i++) {
		    with ({a: i}) { fs.push(function () { return a }) }
		  }
		  fs.map(function (f) { return f() }).join(",")`, "0,1,2"},
		// A nested `with` is still visible to what is created inside it.
		{`var f
		  with ({a: 1}) { with ({b: 2}) { f = function () { return a + b } } }
		  String(f())`, "3"},
	}
	for _, tc := range cases {
		checkEval(t, tc.src, tc.want)
	}
}

// An array whose length cannot change cannot grow, so defining an index at or
// past the end is refused. An index below it is a property like any other.
func TestDefineIndexPastAFixedLength(t *testing.T) {
	cases := []struct{ src, want string }{
		{`var a = [1, 2, 3]
		  Object.defineProperty(a, "length", {writable: false})
		  try { Object.defineProperty(a, 3, {value: "x"}) } catch (e) { e.constructor.name }`,
			"TypeError"},
		{`var a = [1, 2, 3]
		  Object.defineProperty(a, "length", {writable: false})
		  try { Object.defineProperties(a, {3: {value: "x"}}) } catch (e) { e.constructor.name }`,
			"TypeError"},
		{`var a = [1, 2, 3]
		  Object.defineProperty(a, "length", {writable: false})
		  Object.defineProperty(a, 1, {value: "x"})
		  a.join(",") + "," + a.length`, "1,x,3,3"},
		{`var a = [1, 2, 3]
		  Object.defineProperty(a, "length", {writable: false})
		  String(Reflect.defineProperty(a, 5, {value: "x"})) + "," + a.length`, "false,3"},
		// A writable length still grows.
		{`var a = [1]; Object.defineProperty(a, 3, {value: "x"}); a.length`, "4"},
	}
	for _, tc := range cases {
		checkEval(t, tc.src, tc.want)
	}
}

// The cause option is asked whether it is there before it is asked for it, and
// an object that refuses to answer refuses the construction.
func TestErrorCauseIsAskedFor(t *testing.T) {
	cases := []struct{ src, want string }{
		{`var e = new Error("x", {cause: 1}); String(e.cause)`, "1"},
		{`var e = new Error("x", {}); String("cause" in e)`, "false"},
		{`var e = new Error("x"); String("cause" in e)`, "false"},
		{`var e = new Error("x", {cause: undefined}); ["cause" in e, String(e.cause)].join(",")`,
			"true,undefined"},
		{`try { new Error("x", {get cause() { throw new RangeError() }}) }
		  catch (e) { e.constructor.name }`, "RangeError"},
		{`try { new Error("x", new Proxy({}, {has: function () { throw new RangeError() }})) }
		  catch (e) { e.constructor.name }`, "RangeError"},
		{`var e = new TypeError("x", {cause: "c"}); [e.name, e.message, e.cause].join(",")`,
			"TypeError,x,c"},
	}
	for _, tc := range cases {
		checkEval(t, tc.src, tc.want)
	}
}

// A finally clause starts with no value of its own. It matters only when the
// clause leaves abruptly: a break or a continue written inside it carries what
// the clause itself produced, and undefined when it produced nothing, rather
// than what the try block produced.
func TestFinallyCompletionValues(t *testing.T) {
	cases := []struct{ src, want string }{
		{`eval('99; do { -99; try { 39 } catch (e) { -1 } finally { 42; break; -2 }; } while (false);')`,
			"42"},
		{`String(eval('99; do { -99; try { 39 } catch (e) { -1 } finally { break; -2 }; } while (false);'))`,
			"undefined"},
		{`String(eval('99; do { -99; try { [].x.x } catch (e) { -1 } finally { break; -3 }; -77 } while (false);'))`,
			"undefined"},
		{`eval('99; do { -99; try { 39 } catch (e) { -1 } finally { 42; continue; -3 }; } while (false);')`,
			"42"},
		// A clause that finishes normally leaves the try block's value alone.
		{`eval('1; try { 2 } finally { 3 }')`, "2"},
		{`eval('1; try { throw 0 } catch (e) { 2 } finally { 3 }')`, "2"},
		{`String(eval('1; try { 2 } finally { }'))`, "2"},
		{`eval('99; try { 39 } finally { out: { break out; } }')`, "39"},
	}
	for _, tc := range cases {
		checkEval(t, tc.src, tc.want)
	}
}
