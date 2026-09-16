package quickjs_test

import (
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
