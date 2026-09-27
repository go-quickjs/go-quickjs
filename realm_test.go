package quickjs_test

import (
	"context"
	"errors"
	"testing"

	quickjs "github.com/go-quickjs/go-quickjs"
)

// realmCases runs each source in a fresh runtime whose global other is a
// second realm's global object, and compares the string form of its result.
func realmCases(t *testing.T, cases []struct{ src, want string }) {
	t.Helper()
	for _, tc := range cases {
		rt := quickjs.New()
		re, err := rt.NewRealm()
		if err != nil {
			t.Fatal(err)
		}
		if err := rt.Set("other", re.Global()); err != nil {
			t.Fatal(err)
		}
		v, err := rt.Eval(tc.src)
		if err != nil {
			t.Errorf("%s: %v", tc.src, err)
		} else if got := v.String(); got != tc.want {
			t.Errorf("%s = %q, want %q", tc.src, got, tc.want)
		}
		rt.Close()
	}
}

// TestRealmIntrinsics pins that a second realm has its own intrinsics and
// global object, and shares the symbols.
func TestRealmIntrinsics(t *testing.T) {
	realmCases(t, []struct{ src, want string }{
		{`[other.Array === Array, other.Object.prototype === Object.prototype, other.globalThis === other, other === globalThis].join()`,
			"false,false,true,false"},
		{`[new other.Array() instanceof Array, Array.isArray(new other.Array()), new other.Array() instanceof other.Array].join()`,
			"false,true,true"},
		{`[other.Symbol.iterator === Symbol.iterator, other.Symbol.for("k") === Symbol.for("k")].join()`, "true,true"},
		// A var of one realm's script is its global's alone.
		{`other.eval("var inOther = 1"); [typeof inOther, other.inOther].join()`, "undefined,1"},
	})
}

// TestRealmOfFunctions pins that a function runs in the realm it was made in,
// whoever calls it: what it makes, what it throws, and the global object a
// sloppy function falls back to are its realm's.
func TestRealmOfFunctions(t *testing.T) {
	realmCases(t, []struct{ src, want string }{
		{`Object.getPrototypeOf(other.Array(3)) === other.Array.prototype`, "true"},
		{`var f = other.eval("(function () { return [{}, [], this] })"); var r = f();
[Object.getPrototypeOf(r[0]) === other.Object.prototype, Object.getPrototypeOf(r[1]) === other.Array.prototype, r[2] === other].join()`,
			"true,true,true"},
		{`try { other.Array.prototype.map.call(null) } catch (e) { [e instanceof other.TypeError, e instanceof TypeError].join() }`, "true,false"},
		// A function the other realm's eval or Function made is that realm's.
		{`var g = new other.Function("return this"); [g() === other, Object.getPrototypeOf(g) === other.Function.prototype].join()`, "true,true"},
		// A prototype made lazily is its function's realm's, whoever asks.
		{`var h = other.eval("(function () {})"); Object.getPrototypeOf(h.prototype) === other.Object.prototype`, "true"},
	})
}

// TestRealmNewTargetPrototype pins GetPrototypeFromConstructor: a new.target
// that names no prototype gives the object its own realm's intrinsic.
func TestRealmNewTargetPrototype(t *testing.T) {
	realmCases(t, []struct{ src, want string }{
		{`var nt = new other.Function(); nt.prototype = null;
[Object.getPrototypeOf(Reflect.construct(Map, [], nt)) === other.Map.prototype,
 Object.getPrototypeOf(Reflect.construct(Array, [], nt)) === other.Array.prototype,
 Object.getPrototypeOf(Reflect.construct(Error, [], nt)) === other.Error.prototype,
 Object.getPrototypeOf(Reflect.construct(function () {}, [], nt)) === other.Object.prototype].join()`,
			"true,true,true,true"},
		{`var nt = new other.Function(); nt.prototype = 1;
Object.getPrototypeOf(Reflect.construct(Intl.Collator, [], nt)) === other.Intl.Collator.prototype`, "true"},
		// A revoked proxy has no realm to ask.
		{`var p = Proxy.revocable(function () {}, {}); p.proxy.prototype = null; var pr = p.proxy; p.revoke();
try { Reflect.construct(Map, [], pr) } catch (e) { e.constructor.name }`, "TypeError"},
		// The collections report a failed read of new.target's prototype
		// rather than making an object with none.
		{`var r = []; var p = Proxy.revocable(function () {}, {}); var pr = p.proxy; p.revoke();
for (var C of [Map, Set, WeakMap, WeakSet]) { try { Reflect.construct(C, [], pr); r.push("made") } catch (e) { r.push(e.constructor.name) } }
r.join()`, "TypeError,TypeError,TypeError,TypeError"},
	})
}

// TestRealmArraySpecies pins that another realm's Array makes this realm's
// arrays when a method asks an array for its species.
func TestRealmArraySpecies(t *testing.T) {
	realmCases(t, []struct{ src, want string }{
		{`var a = new other.Array(1, 2); var m = Array.prototype.map.call(a, x => x);
[Object.getPrototypeOf(m) === Array.prototype, Object.getPrototypeOf(a.map(x => x)) === other.Array.prototype].join()`, "true,true"},
	})
}

// TestRealmDerivedConstructorWithoutSuper pins that a derived constructor
// that never calls super() fails the construction with the caller's
// ReferenceError, which the constructor's own body cannot catch.
func TestRealmDerivedConstructorWithoutSuper(t *testing.T) {
	realmCases(t, []struct{ src, want string }{
		{`var C = other.eval("(class extends Object { constructor() {} })");
try { new C() } catch (e) { [e instanceof ReferenceError, e instanceof other.ReferenceError].join() }`, "true,false"},
		{`var caught = false; class B extends Object { constructor() { try { return } catch (e) { caught = true } } }
try { new B() } catch (e) { e.constructor.name + "," + caught }`, "ReferenceError,false"},
		{`class D extends Object { constructor() { super(); } } new D() instanceof D`, "true"},
	})
}

// TestRealmRegExpCompile pins that RegExp.prototype.compile refuses another
// realm's RegExp.
func TestRealmRegExpCompile(t *testing.T) {
	realmCases(t, []struct{ src, want string }{
		{`var r = []; try { RegExp.prototype.compile.call(new other.RegExp("")) } catch (e) { r.push(e instanceof TypeError) }
try { other.RegExp.prototype.compile.call(/x/) } catch (e) { r.push(e instanceof other.TypeError) }
var o = new other.RegExp("a"); r.push(o.compile("b") === o); r.join()`, "true,true,true"},
	})
}

// TestRealmAPI pins the public realm API: a realm's globals are its own, a Go
// function set on it throws its errors, and a script run in it has the name
// it was given and stops when its context is done.
func TestRealmAPI(t *testing.T) {
	rt := quickjs.New()
	defer rt.Close()
	re, err := rt.NewRealm()
	if err != nil {
		t.Fatal(err)
	}
	if err := re.Set("fail", func() error { return errors.New("from Go") }); err != nil {
		t.Fatal(err)
	}
	if _, err := re.Eval("var inRealm = 1"); err != nil {
		t.Fatal(err)
	}
	v, err := re.Eval(`try { fail() } catch (e) { [e instanceof Error, e.message, typeof inRealm].join() }`)
	if err != nil || v.String() != "true,from Go,number" {
		t.Errorf("realm eval = %v, %v", v, err)
	}
	if v, err := rt.Eval("typeof inRealm + ',' + typeof fail"); err != nil || v.String() != "undefined,undefined" {
		t.Errorf("runtime's own realm sees %v, %v", v, err)
	}
	if err := rt.Set("other", re.Global()); err != nil {
		t.Fatal(err)
	}
	if v, err := rt.Eval("other.inRealm + ',' + (other.Array === Array)"); err != nil || v.String() != "1,false" {
		t.Errorf("other realm's global = %v, %v", v, err)
	}
	v, err = re.EvalFileContext(context.Background(), "realm.js", "function f() { return new Error() }\nf().stack")
	if err != nil || v.String() != "Error\n    at f (realm.js:1:23)\n    at realm.js:2:1" {
		t.Errorf("named script's stack = %q, %v", v.String(), err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := re.EvalFileContext(ctx, "loop.js", "for (;;) {}"); !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled run: %v", err)
	}
}
