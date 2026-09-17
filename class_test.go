package quickjs_test

import (
	"strings"
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

// A private name is resolved when the program is compiled, not when it runs:
// `this.#x` outside any class declaring #x is a syntax error, in the same way
// an unbalanced brace is. That is what makes a private field private -- there
// is no way to ask an object for one you were not written alongside, and so no
// way to discover one by probing.
func TestPrivateNamesAreResolvedAtCompileTime(t *testing.T) {
	cases := []struct{ src, want string }{
		{`class C { #x = 1; m() { return this.#x; } } String(new C().m())`, "1"},
		{`class C { #m() { return 1; } m() { return this.#m(); } } String(new C().m())`, "1"},
		{`class C { static #x = 1; static m() { return C.#x; } } String(C.m())`, "1"},
		{`class C { #x; static has(o) { return #x in o; } }
		  [C.has(new C()), C.has({})].join(",")`, "true,false"},
		// A method may refer to a field declared below it, so the names are
		// collected from the whole body before any of it is compiled.
		{`class C { m() { return this.#x; } #x = 2; } String(new C().m())`, "2"},
		// A nested class sees the enclosing one's names.
		{`class Outer { #x = 3;
		    m() { var self = this; return (class { static f() { return self.#x; } }).f(); }
		  } String(new Outer().m())`, "3"},
		// A getter and a setter may share a name.
		{`class C { get #x() { return 4; } set #x(v) {} m() { return this.#x; } }
		  String(new C().m())`, "4"},
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

	// Each of these has to fail to parse, so a throw before it never runs.
	bad := []string{
		// Undeclared, inside a class and outside one.
		`throw 0; class C { m() { this.#x; } }`,
		`throw 0; class C { m() { return #x in this; } }`,
		`throw 0; this.#x;`,
		`throw 0; function f() { return this.#x; }`,
		`throw 0; class C { #x; } class D { m() { return this.#x; } }`,
		// A sibling class's name is not in scope either.
		`throw 0; class C { #x; m() { return (class { n(o) { return o.#y; } }); } }`,
		// Writing one is the same as reading one.
		`throw 0; class C { m() { this.#x = 1; } }`,
		`throw 0; class C { m() { this.#x += 1; } }`,
		// A private name cannot be deleted, declared or not.
		`throw 0; class C { #x; m() { delete this.#x; } }`,
		`throw 0; class C { #x; m() { delete (this.#x); } }`,
		`throw 0; class C { #m() {} m() { delete this.#m; } }`,
		// A constructor has to be a plain method.
		`throw 0; class C { get constructor() {} }`,
		`throw 0; class C { set constructor(v) {} }`,
		`throw 0; class C { *constructor() {} }`,
		`throw 0; class C { async constructor() {} }`,
		// And the names have to be distinct.
		`throw 0; class C { #x; #x; }`,
		`throw 0; class C { #constructor; }`,
		`throw 0; class C { static prototype() {} }`,
	}
	for _, src := range bad {
		rt := quickjs.New()
		if _, err := rt.Eval(src); err == nil {
			t.Errorf("%s: accepted, want SyntaxError", src)
		} else if !strings.Contains(err.Error(), "SyntaxError") {
			t.Errorf("%s: got %v, want SyntaxError", src, err)
		}
		rt.Close()
	}
}

// A derived constructor does not receive `this`; super() binds it. Until then
// the object exists but is unreachable, which is what stops a subclass from
// touching what the base class has not finished building.
func TestThisIsBoundBySuper(t *testing.T) {
	cases := []struct{ src, want string }{
		{`class B {} class D extends B { constructor() { this.x; super(); } }
		  try { new D() } catch (e) { e.constructor.name }`, "ReferenceError"},
		{`class B {} class D extends B { constructor() { super(); super(); } }
		  try { new D() } catch (e) { e.constructor.name }`, "ReferenceError"},
		// Falling off the end without calling super() is the same mistake.
		{`class B {} class D extends B { constructor() {} }
		  try { new D() } catch (e) { e.constructor.name }`, "ReferenceError"},
		// An arrow written before super() closes over the same unbound `this`.
		{`class B {} class D extends B {
		    constructor() { var f = () => this; var n;
		      try { f() } catch (e) { n = e.constructor.name } super(); this.n = n; }
		  } new D().n`, "ReferenceError"},

		// And the ordinary paths are unchanged.
		{`class B {} class D extends B {} String(new D() instanceof D)`, "true"},
		{`class B { constructor() { this.a = 1; } }
		  class D extends B { b = 2; constructor() { super(); this.c = 3; } }
		  var d = new D(); [d.a, d.b, d.c].join(",")`, "1,2,3"},
		{`class D extends Array { constructor() { super(1, 2); } } new D().join(",")`, "1,2"},
		// A base constructor has its `this` from the start.
		{`class B { constructor() { this.x = 1; } } String(new B().x)`, "1"},
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

// A class field is created on the instance rather than written through it, so a
// setter the prototype happens to have for the same name is not called, and a
// private field is added rather than requiring one to be there already.
func TestClassFieldsAreDefined(t *testing.T) {
	cases := []struct{ src, want string }{
		{`class C { x = 1; } String(new C().x)`, "1"},
		{`class C { x = 1 } var d = Object.getOwnPropertyDescriptor(new C(), "x");
		  [d.writable, d.enumerable, d.configurable].join(",")`, "true,true,true"},
		{`class C { ["a" + "b"] = 1 } String(new C().ab)`, "1"},
		{`class C { #x = 1; #y = 2; m() { return this.#x + this.#y; } }
		  String(new C().m())`, "3"},
		{`class C { static x = 1 } String(C.x)`, "1"},

		// A setter inherited from the parent is not called by a field.
		{`var called = false;
		  class B { set x(v) { called = true; } }
		  class D extends B { x = 1; }
		  [new D().x, called].join(",")`, "1,false"},
		{`var called = false;
		  class B { static set x(v) { called = true; } }
		  class D extends B { static x = 1; }
		  [D.x, called].join(",")`, "1,false"},
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
		// A private field is added when the object is constructed and never
		// afterwards, so writing one to an object that does not have it is a
		// mistake rather than a way to add it.
		`class C { #x; static set(o) { o.#x = 1; } } C.set({})`,
		`class C { #x; static get(o) { return o.#x; } } C.get({})`,
		`class C { #m() {} static call(o) { return o.#m(); } } C.call({})`,
		`class C { static #x = 1; static get(o) { return o.#x; } } C.get({})`,
	}
	for _, src := range bad {
		rt := quickjs.New()
		if _, err := rt.Eval(src); err == nil {
			t.Errorf("%s: accepted, want TypeError", src)
		} else if !strings.Contains(err.Error(), "TypeError") {
			t.Errorf("%s: got %v, want TypeError", src, err)
		}
		rt.Close()
	}
}

// Array.prototype[Symbol.iterator] is the same function object as values, and
// the fast paths that copy an array's elements directly check for exactly that
// -- including the case where the iterator has been deleted, which makes
// destructuring an array a TypeError that a fast path would quietly succeed at.
func TestArrayIterationRespectsTheProtocol(t *testing.T) {
	cases := []struct{ src, want string }{
		{`String(Array.prototype.values === Array.prototype[Symbol.iterator])`, "true"},
		{`Array.prototype[Symbol.iterator] = function* () { yield 9; };
		  [...[1, 2]].join(",")`, "9"},
		{`var a = [1, 2]; a[Symbol.iterator] = function* () { yield 7; };
		  [...a].join(",")`, "7"},
		{`[...[1, 2, 3]].join(",")`, "1,2,3"},
		{`var [a, b] = [1, 2]; a + "," + b`, "1,2"},
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

	const noIter = "delete Array.prototype[Symbol.iterator];\n"
	for _, src := range []string{
		noIter + `var [a] = [1, 2];`,
		noIter + `[...[1, 2]];`,
		noIter + `function f([x]) {} f([1]);`,
		noIter + `for (const x of [1]) {}`,
	} {
		rt := quickjs.New()
		if _, err := rt.Eval(src); err == nil {
			t.Errorf("%s: accepted, want TypeError", src)
		} else if !strings.Contains(err.Error(), "TypeError") {
			t.Errorf("%s: got %v, want TypeError", src, err)
		}
		rt.Close()
	}
}

// An object pattern reads properties from its source, so the source has to be
// something properties can be read from -- and that is checked before any of
// them are, which is the only thing an empty pattern does.
func TestObjectPatternRequiresCoercible(t *testing.T) {
	bad := []string{
		`var {} = null`,
		`var {} = undefined`,
		`var {a} = null`,
		`var {a} = undefined`,
		`({} = null)`,
		`function f({}) {} f(null)`,
		`function f({} = null) {} f()`,
		`function* g({} = null) {} g()`,
		`var C = class { m({} = null) {} }; C.prototype.m()`,
		`var C = class { *m({} = null) {} }; C.prototype.m()`,
		`var {a: {b}} = {a: null}`,
		`for (var {} of [null]) {}`,
		`try { throw null } catch ({}) {}`,
	}
	for _, src := range bad {
		rt := quickjs.New()
		if _, err := rt.Eval(src); err == nil {
			t.Errorf("%s: accepted, want TypeError", src)
		} else if !strings.Contains(err.Error(), "TypeError") {
			t.Errorf("%s: got %v, want TypeError", src, err)
		}
		rt.Close()
	}

	cases := []struct{ src, want string }{
		{`var {} = {}; "ok"`, "ok"},
		{`var {a} = {a: 1}; String(a)`, "1"},
		// A primitive that is coercible reads through its wrapper.
		{`var {length} = "abc"; String(length)`, "3"},
		{`var {constructor} = 5; String(constructor === Number)`, "true"},
		{`var {a, ...r} = {a: 1, b: 2}; JSON.stringify(r)`, `{"b":2}`},
		{`var {a = 5} = {}; String(a)`, "5"},
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

// A String object is a box round an immutable string, and its characters are
// own properties: indexed, enumerable, read-only and not configurable. Its
// length is one too, and is not enumerable.
func TestStringObjectIndices(t *testing.T) {
	cases := []struct{ src, want string }{
		{`Object.keys("ab").join(",")`, "0,1"},
		{`Object.getOwnPropertyNames("ab").join(",")`, "0,1,length"},
		{`Object.keys(new String("ab")).join(",")`, "0,1"},
		{`JSON.stringify(Object.entries("ab"))`, `[["0","a"],["1","b"]]`},
		{`JSON.stringify(Object.getOwnPropertyDescriptor(new String("ab"), "0"))`,
			`{"value":"a","writable":false,"enumerable":true,"configurable":false}`},
		{`JSON.stringify(Object.getOwnPropertyDescriptor(new String("ab"), "length"))`,
			`{"value":2,"writable":false,"enumerable":false,"configurable":false}`},
		{`String(Object.getOwnPropertyDescriptor(new String("ab"), "5"))`, "undefined"},
		// Characters come first, then anything the object was given.
		{`var s = new String("ab"); s.x = 1; Object.keys(s).join(",")`, "0,1,x"},
		{`var out = []; for (var k in "ab") out.push(k); out.join(",")`, "0,1"},

		{`String(Object.setPrototypeOf(1, null))`, "1"},
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

	for _, src := range []string{
		// The target is checked before the prototype, so the more obvious
		// mistake is the one reported.
		`Object.setPrototypeOf(null, {})`,
		`Object.setPrototypeOf(undefined, {})`,
		`Object.setPrototypeOf(null, 1)`,
	} {
		rt := quickjs.New()
		if _, err := rt.Eval(src); err == nil {
			t.Errorf("%s: accepted, want TypeError", src)
		} else if !strings.Contains(err.Error(), "TypeError") {
			t.Errorf("%s: got %v, want TypeError", src, err)
		}
		rt.Close()
	}
}

// Function.prototype.length is how many arguments a function expects, not how
// many it has room for: it counts the parameters before the first one with a
// default or a rest element.
func TestFunctionLength(t *testing.T) {
	cases := []struct{ src, want string }{
		{`function f() {} String(f.length)`, "0"},
		{`function f(a, b) {} String(f.length)`, "2"},
		{`function f(a, b,) {} String(f.length)`, "2"},
		{`function f(a, b = 1) {} String(f.length)`, "1"},
		{`function f(a, b = 1,) {} String(f.length)`, "1"},
		{`function f(a = 1, b) {} String(f.length)`, "0"},
		{`function f(a, ...r) {} String(f.length)`, "1"},
		{`function f(...r) {} String(f.length)`, "0"},
		{`function f([a], {b}) {} String(f.length)`, "2"},

		{`({m(a, b = 1) {}}).m.length + ""`, "1"},
		{`class C { m(a, b = 1) {} } String(C.prototype.m.length)`, "1"},
		{`((a, b = 1) => {}).length + ""`, "1"},
		{`(function* (a, b = 1) {}).length + ""`, "1"},
		{`(async function (a, b = 1) {}).length + ""`, "1"},
		{`class C { constructor(a, b = 1) {} } String(C.length)`, "1"},

		// length is configurable and not writable, like every built-in's.
		{`function f(a) {} var d = Object.getOwnPropertyDescriptor(f, "length");
		  [d.writable, d.enumerable, d.configurable].join(",")`, "false,false,true"},
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

// `super` in a static field initializer or a static block resolves against the
// parent class rather than its prototype, because the home object there is the
// class itself.
func TestSuperInStaticInitializers(t *testing.T) {
	cases := []struct{ src, want string }{
		{`class A { static m() { return 1 } } class B extends A { static x = super.m(); }
		  String(B.x)`, "1"},
		{`class A { static m() { return 2 } }
		  class B extends A { static { B.y = super.m() } } String(B.y)`, "2"},
		{`class A { static m() { return 3 } }
		  class B extends A { static n() { return super.m() } } String(B.n())`, "3"},
		{`class A { static get p() { return 5 } }
		  class B extends A { static x = super.p; } String(B.x)`, "5"},
		// An arrow in the initializer shares its super.
		{`class A { static m() { return 6 } }
		  class B extends A { static x = (() => super.m())(); } String(B.x)`, "6"},
		// A static member sees the parent class, not its prototype, so an
		// instance method is not there.
		{`class A { m() { return 1 } }
		  class B extends A { static x = typeof super.m; } B.x`, "undefined"},

		// An instance field initializer resolves against the prototype, as a
		// method does.
		{`class A { m() { return 4 } } class B extends A { x = super.m(); }
		  String(new B().x)`, "4"},
		{`class A { m() { return 7 } } class B extends A { x = () => super.m(); }
		  String(new B().x())`, "7"},
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

// Number is the one place a BigInt converts to a Number without complaint: it
// is what a program asking for the conversion explicitly has written, rather
// than one mixing the two kinds by accident.
func TestNumberOfBigInt(t *testing.T) {
	cases := []struct{ src, want string }{
		{`String(Number(1n))`, "1"},
		{`String(Number(-2n))`, "-2"},
		// The conversion is lossy above 2**53, which is why it is never
		// implicit.
		{`String(Number(-9007199254740993n))`, "-9007199254740992"},
		{`String(Number(2n ** 100n))`, "1.2676506002282294e+30"},
		{`String(Number(Object(5n)))`, "5"},
		{`String(new Number(3n).valueOf())`, "3"},

		{`String(Number("3"))`, "3"},
		{`String(Number())`, "0"},
		{`String(Number(null))`, "0"},
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

	// Everything else still refuses to mix the two kinds.
	for _, src := range []string{`1n + 1`, `+1n`, `Math.max(1n)`, `1n | 1`} {
		rt := quickjs.New()
		if _, err := rt.Eval(src); err == nil {
			t.Errorf("%s: accepted, want TypeError", src)
		}
		rt.Close()
	}
}

// A derived constructor may return an object, which becomes the result, or
// nothing, in which case the object super() built is. Anything else would
// silently discard that object, so it is a TypeError.
func TestDerivedConstructorReturn(t *testing.T) {
	cases := []struct{ src, want string }{
		{`class B {} class D extends B { constructor() { super(); return {tag: 1} } }
		  String(new D().tag)`, "1"},
		{`class B {} class D extends B { constructor() { super(); return undefined } }
		  String(new D() instanceof D)`, "true"},
		{`class B {} class D extends B { constructor() { super(); return } }
		  String(new D() instanceof D)`, "true"},
		// Returning an object does not even need super() to have run.
		{`class B {} class D extends B { constructor() { return {tag: 2} } }
		  String(new D().tag)`, "2"},

		// A base constructor has no such rule: it returns `this` for anything
		// that is not an object.
		{`class D { constructor() { return 1 } } String(new D() instanceof D)`, "true"},
		{`class B { constructor() { return 1 } } class D extends B {}
		  String(new D() instanceof D)`, "true"},
		{`class D extends Array { constructor() { super(1, 2) } } new D().join(",")`, "1,2"},
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
		`class B {} class D extends B { constructor() { super(); return 1 } } new D()`,
		`class B {} class D extends B { constructor() { super(); return null } } new D()`,
		`class B {} class D extends B { constructor() { super(); return "s" } } new D()`,
		`class B {} class D extends B { constructor() { return 1 } } new D()`,
		// Through a finally, which is where the return actually happens.
		`class B {} class D extends B { constructor() { try { return 1 } finally {} } } new D()`,
	}
	for _, src := range bad {
		rt := quickjs.New()
		if _, err := rt.Eval(src); err == nil {
			t.Errorf("%s: accepted, want TypeError", src)
		} else if !strings.Contains(err.Error(), "TypeError") {
			t.Errorf("%s: got %v, want TypeError", src, err)
		}
		rt.Close()
	}
}
