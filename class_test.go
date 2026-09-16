package quickjs_test

import (
	"testing"

	quickjs "github.com/go-quickjs/go-quickjs"
)

// new.target is what decides what an object is, and it is not always the
// constructor being run: `new D()` on a class derived from B eventually runs
// B's constructor, but the object is a D. Reflect.construct exposes the same
// split directly.
func TestConstructNewTarget(t *testing.T) {
	const isCtor = `function isConstructor(f) {
	  try { Reflect.construct(function () {}, [], f); } catch (e) { return false; }
	  return true;
	}
	`
	cases := []struct{ src, want string }{
		// A built-in method is not a constructor, however ordinary it looks.
		{isCtor + `[isConstructor(Array.prototype.map), isConstructor(Object.keys),
		   isConstructor(Math.max), isConstructor(RegExp.prototype[Symbol.match])].join(",")`,
			"false,false,false,false"},
		{isCtor + `[isConstructor(Array), isConstructor(RegExp), isConstructor(Proxy),
		   isConstructor(class {}), isConstructor(function () {})].join(",")`,
			"true,true,true,true,true"},
		// Nor is anything that has no object to build: an arrow has no
		// prototype at all, and calling a generator produces an iterator.
		{isCtor + `[isConstructor(() => {}), isConstructor({m() {}}.m),
		   isConstructor(function* () {}), isConstructor(async function () {})].join(",")`,
			"false,false,false,false"},

		// The prototype comes from new.target, not from the constructor.
		{`function B() {} function D() {} D.prototype = {tag: "D"};
		  String(Reflect.construct(B, [], D).tag)`, "D"},
		{`class A { constructor() { this.nt = new.target.name; } } function F() {}
		  String(Reflect.construct(A, [], F).nt)`, "F"},
		// Including for a native constructor, which builds its own object and
		// knows nothing of new.target.
		{`function F() {}
		  String(Object.getPrototypeOf(Reflect.construct(Array, [], F)) === F.prototype)`, "true"},
		{`function F() {} var a = Reflect.construct(Array, [1, 2], F);
		  [a.length, Array.isArray(a)].join(",")`, "2,true"},
		{`String(Reflect.construct(Array, [3]).length)`, "3"},

		// The ordinary path is unchanged.
		{`class B { constructor() { this.nt = new.target.name; } } class D extends B {}
		  new D().nt`, "D"},
		{`class A extends Array {} var a = new A(); a.push(1);
		  [a.length, a instanceof A].join(",")`, "1,true"},
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

	bad := []string{
		// Both arguments have to be constructors.
		`Reflect.construct(Math.max, [])`,
		`Reflect.construct(function () {}, [], 1)`,
		`Reflect.construct(function () {}, [], Math.max)`,
		`Reflect.construct({}, [])`,
		// A generator has a prototype property, which is not the same thing.
		`new (function* () {})()`,
		`new (async function () {})()`,
	}
	for _, src := range bad {
		rt := quickjs.New()
		if _, err := rt.Eval(src); err == nil {
			t.Errorf("%s: accepted, want TypeError", src)
		}
		rt.Close()
	}
}

// An array iterator reads through the property table rather than the dense
// element slice, which is what makes a frozen or sparse array iterate at all:
// freezing moves the elements out of the dense slice so that they can be marked
// read-only.
func TestIteratingNonDenseArrays(t *testing.T) {
	cases := []struct{ src, want string }{
		{`var a = [1, 2]; Object.freeze(a); [...a].join(",")`, "1,2"},
		{`var a = [1, 2]; Object.freeze(a); Array.from(a).join(",")`, "1,2"},
		{`var a = [1, 2]; Object.freeze(a);
		  var o = []; for (const x of a) o.push(x); o.join(",")`, "1,2"},
		{`var a = [1, 2]; Object.freeze(a); [...a.keys()].join(",")`, "0,1"},
		{`var a = [1, 2]; Object.freeze(a);
		  [...a.entries()].map(e => e.join(":")).join(",")`, "0:1,1:2"},
		{`var a = [1, 2]; Object.seal(a); [...a].join(",")`, "1,2"},
		{`var a = [1, 2]; Object.defineProperty(a, 0, {value: 9}); [...a].join(",")`, "9,2"},
		{`function f(...r) { return r.join(","); }
		  var a = [1, 2]; Object.freeze(a); f(...a)`, "1,2"},

		// A hole is filled from the prototype chain, which the dense path
		// would have reported as undefined.
		{`Object.defineProperty(Array.prototype, 0, {value: "p", configurable: true});
		  var a = [, 1]; [...a].join(",")`, "p,1"},
		{`var a = [, 1]; [...a].map(String).join(",")`, "undefined,1"},

		// Length is re-read as iteration proceeds, so an array that grows is
		// seen doing so.
		{`var a = [1]; var out = [];
		  for (const x of a) { out.push(x); if (a.length < 3) a.push(a.length + 1); }
		  out.join(",")`, "1,2,3"},

		// And the ordinary dense path is unchanged.
		{`[...[1, 2, 3]].join(",")`, "1,2,3"},
		{`[...[1, 2].keys()].join(",")`, "0,1"},
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
