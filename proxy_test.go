package quickjs_test

import (
	"strings"
	"testing"

	quickjs "github.com/go-quickjs/go-quickjs"
)

// A proxy trap may lie, but not about anything a caller could already have
// observed and relied on. Enforcing that is most of what separates a proxy from
// an object with clever getters.
func TestProxyInvariants(t *testing.T) {
	cases := []struct{ name, src string }{
		{"duplicate own key", `Object.keys(new Proxy({}, {ownKeys: () => ["a", "a"]}))`},
		{"non-key own key", `Object.keys(new Proxy({}, {ownKeys: () => [1]}))`},
		{"prototype is not an object",
			`Object.getPrototypeOf(new Proxy({}, {getPrototypeOf: () => 1}))`},
		// A target that cannot change its prototype cannot be reported as
		// having a different one.
		{"prototype of a sealed target",
			`var t = Object.preventExtensions({});
			 Object.getPrototypeOf(new Proxy(t, {getPrototypeOf: () => ({})}))`},
		{"changing the prototype of a sealed target",
			`var t = Object.preventExtensions({});
			 Object.setPrototypeOf(new Proxy(t, {setPrototypeOf: () => true}), {})`},
		// Extensibility may not be misreported at all.
		{"extensibility misreported",
			`Object.isExtensible(new Proxy({}, {isExtensible: () => false}))`},
		{"preventExtensions claiming success",
			`Object.preventExtensions(new Proxy({}, {preventExtensions: () => true}))`},
		// A property that cannot be deleted cannot be hidden.
		{"hiding a non-configurable property",
			`var t = {}; Object.defineProperty(t, "a", {value: 1, configurable: false});
			 Object.getOwnPropertyDescriptor(
			     new Proxy(t, {getOwnPropertyDescriptor: () => undefined}), "a")`},
		{"omitting a non-configurable key",
			`var t = {}; Object.defineProperty(t, "a", {value: 1, configurable: false});
			 Object.keys(new Proxy(t, {ownKeys: () => []}))`},
		{"denying a non-configurable property",
			`var t = {}; Object.defineProperty(t, "a", {value: 1, configurable: false});
			 "a" in new Proxy(t, {has: () => false})`},
		{"deleting a non-configurable property",
			`var t = {}; Object.defineProperty(t, "a", {value: 1, configurable: false});
			 delete new Proxy(t, {deleteProperty: () => true}).a`},
		// A non-extensible target cannot gain a property.
		{"inventing a key on a sealed target",
			`var t = Object.preventExtensions({});
			 Object.keys(new Proxy(t, {ownKeys: () => ["a"]}))`},
		{"defining on a sealed target",
			`var t = Object.preventExtensions({});
			 Object.defineProperty(new Proxy(t, {defineProperty: () => true}), "x", {value: 1})`},
		// A construct trap has to produce an object.
		{"construct returning a primitive",
			`function T() {} new (new Proxy(T, {construct: () => 1}))()`},
		// A trap that reports failure is reported to the caller.
		{"defineProperty returning false",
			`Object.defineProperty(new Proxy({}, {defineProperty: () => false}), "x", {value: 1})`},
		{"a trap that is not callable", `Object.keys(new Proxy({}, {ownKeys: 1}))`},
		{"operating on a revoked proxy",
			`var r = Proxy.revocable({}, {}); r.revoke(); Object.keys(r.proxy)`},
	}

	for _, tc := range cases {
		rt := quickjs.New()
		_, err := rt.Eval(tc.src)
		if err == nil {
			t.Errorf("%s: no error, want TypeError", tc.name)
		} else if !strings.Contains(err.Error(), "TypeError") {
			t.Errorf("%s: got %v, want TypeError", tc.name, err)
		}
		rt.Close()
	}
}

// TestProxyTraps pins the ordinary behaviour the invariants above sit on top
// of: a trap that answers consistently is simply believed.
func TestProxyTraps(t *testing.T) {
	cases := []struct{ src, want string }{
		{`String(Object.getPrototypeOf(new Proxy({}, {getPrototypeOf: () => null})))`, "null"},
		{`var q = {}; String(Object.getPrototypeOf(new Proxy({}, {getPrototypeOf: () => q})) === q)`,
			"true"},
		{`String(Object.isExtensible(new Proxy({}, {})))`, "true"},
		{`var t = {}; String(Object.setPrototypeOf(new Proxy(t, {}), null) !== null)`, "true"},
		{`Object.keys(new Proxy({a: 1, b: 2}, {})).join(",")`, "a,b"},
		{`Object.keys(new Proxy({a: 1}, {ownKeys: t => ["a"]})).join(",")`, "a"},
		{`JSON.stringify(Object.getOwnPropertyDescriptor(
		    new Proxy({}, {getOwnPropertyDescriptor: () => (
		        {value: 1, configurable: true, enumerable: true, writable: true})}), "x"))`,
			`{"value":1,"configurable":true,"enumerable":true,"writable":true}`},
		// The handler is the trap's receiver.
		{`var h = {has() { globalThis.ctx = this; return true; }};
		  "x" in new Proxy({}, h); String(ctx === h)`, "true"},
		// new.target reaches the construct trap as the proxy itself.
		{`function T() {}
		  var p = new Proxy(T, {construct: (t, a, nt) => ({same: nt === p})});
		  String(new p().same)`, "true"},
		{`String(Reflect.isExtensible(new Proxy({}, {})))`, "true"},
		{`var t = {a: 1}; Reflect.ownKeys(new Proxy(t, {})).join(",")`, "a"},
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

// A subclass's methods have to produce instances of the subclass, which is what
// Symbol.species decides.
func TestSpecies(t *testing.T) {
	cases := []struct{ src, want string }{
		{`[Promise, Array, RegExp, Map, Set, ArrayBuffer]
		    .map(c => c[Symbol.species] === c).join(",")`, "true,true,true,true,true,true"},
		{`class P extends Promise {} String(P.resolve(1) instanceof P)`, "true"},
		{`class P extends Promise {} String(P.reject(1) instanceof P)`, "true"},
		{`class P extends Promise {} String(P.resolve(1).then(x => x) instanceof P)`, "true"},
		{`class P extends Promise {} String(P.resolve(1).catch(x => x) instanceof P)`, "true"},
		// An ordinary promise is unaffected.
		{`String(Promise.resolve(1) instanceof Promise)`, "true"},
		{`var p = Promise.resolve(1); String(Promise.resolve(p) === p)`, "true"},
		// The accessor has a getter and no setter, and is configurable.
		{`var d = Object.getOwnPropertyDescriptor(Promise, Symbol.species);
		  [typeof d.get, String(d.set), d.configurable, d.enumerable].join(",")`,
			"function,undefined,true,false"},
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

	// A receiver that is not a constructor is refused rather than silently
	// producing an intrinsic promise.
	for _, src := range []string{`Promise.resolve.call(null, 1)`, `Promise.reject.call(1, 1)`} {
		rt := quickjs.New()
		if _, err := rt.Eval(src); err == nil {
			t.Errorf("%s: no error, want TypeError", src)
		}
		rt.Close()
	}
}

// TestDefinePropertyValidation pins the checks a script relies on when it
// freezes an object and hands it out.
func TestDefinePropertyValidation(t *testing.T) {
	bad := []struct{ name, src string }{
		{"redefining a non-configurable property",
			`var o = {}; Object.defineProperty(o, "a", {value: 1});
			 Object.defineProperty(o, "a", {value: 2})`},
		{"turning a data property into an accessor",
			`var o = {}; Object.defineProperty(o, "a", {value: 1});
			 Object.defineProperty(o, "a", {get() {}})`},
		{"making a non-configurable property configurable",
			`var o = {}; Object.defineProperty(o, "a", {value: 1});
			 Object.defineProperty(o, "a", {configurable: true})`},
		{"making a read-only property writable",
			`var o = {}; Object.defineProperty(o, "a", {value: 1, writable: false});
			 Object.defineProperty(o, "a", {writable: true})`},
		{"adding to a non-extensible object",
			`Object.defineProperty(Object.preventExtensions({}), "a", {value: 1})`},
		{"a descriptor that is not an object", `Object.defineProperty({}, "a", 1)`},
		{"a getter that is not callable", `Object.defineProperty({}, "a", {get: 1})`},
		{"both a value and a getter",
			`Object.defineProperty({}, "a", {value: 1, get() {}})`},
		{"a negative array length", `Object.defineProperty([], "length", {value: -1})`},
		{"a fractional array length", `Object.defineProperty([], "length", {value: 1.5})`},
		// A prototype cycle would make every lookup walk the ring forever.
		{"a prototype cycle",
			`var a = {}, b = Object.create(a); Object.setPrototypeOf(a, b)`},
		{"a self prototype", `var a = {}; Object.setPrototypeOf(a, a)`},
		{"a prototype cycle through __proto__",
			`var a = {}, b = Object.create(a); a.__proto__ = b`},
	}
	for _, tc := range bad {
		rt := quickjs.New()
		if _, err := rt.Eval(tc.src); err == nil {
			t.Errorf("%s: no error, want one", tc.name)
		}
		rt.Close()
	}

	ok := []struct{ src, want string }{
		// Redefining with the identical value is allowed.
		{`var o = {}; Object.defineProperty(o, "a", {value: 1});
		  Object.defineProperty(o, "a", {value: 1}); String(o.a)`, "1"},
		// A writable but non-configurable property may still change value, and
		// may be made read-only once.
		{`var o = {}; Object.defineProperty(o, "a", {value: 1, writable: true});
		  Object.defineProperty(o, "a", {value: 2}); String(o.a)`, "2"},
		{`var o = {}; Object.defineProperty(o, "a", {value: 1, writable: true});
		  Object.defineProperty(o, "a", {writable: false});
		  String(Object.getOwnPropertyDescriptor(o, "a").writable)`, "false"},
		// Every attribute defaults to false.
		{`var o = {}; Object.defineProperty(o, "a", {value: 1});
		  JSON.stringify(Object.getOwnPropertyDescriptor(o, "a"))`,
			`{"value":1,"writable":false,"enumerable":false,"configurable":false}`},
		// Array length truncates.
		{`var a = [1, 2, 3]; Object.defineProperty(a, "length", {value: 1}); a.join(",")`, "1"},
		{`var a = []; Object.defineProperty(a, "2",
		    {value: 9, enumerable: true, writable: true, configurable: true});
		  String(a.length)`, "3"},
		{`var a = {}, b = {}; Object.setPrototypeOf(a, b);
		  String(Object.getPrototypeOf(a) === b)`, "true"},
		{`var a = {}; Object.setPrototypeOf(a, null); String(Object.getPrototypeOf(a))`, "null"},
	}
	for _, tc := range ok {
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

// A combinator resolves every element through the constructor's own resolve,
// which is what lets a subclass see each value go past.
func TestPromiseCombinatorUsesReceiver(t *testing.T) {
	cases := []struct{ src, want string }{
		{`var calls = 0;
		  class P extends Promise { static resolve(v) { calls++; return super.resolve(v); } }
		  P.all([1, 2]); String(calls)`, "2"},
		{`class P extends Promise {} String(P.all([]) instanceof P)`, "true"},
		{`class P extends Promise {} String(P.race([]) instanceof P)`, "true"},
		{`class P extends Promise {} String(P.allSettled([]) instanceof P)`, "true"},

		// A plain value still works, and so does a thenable.
		{`var out; Promise.all([1, Promise.resolve(2)]).then(v => out = v.join(","));
		  Promise.resolve().then(() => {}).then(() => {}).then(() => String(out))`,
			"[object Promise]"},
		{`var out; Promise.resolve({then(res) { res(7); }}).then(v => out = v);
		  Promise.resolve().then(() => {}).then(() => String(out))`, "[object Promise]"},
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

	// The receiver has to be a constructor with a callable resolve.
	rt := quickjs.New()
	defer rt.Close()
	if _, err := rt.Eval(`Promise.all.call(1, [])`); err == nil {
		t.Error("Promise.all on a non-constructor should throw")
	}
}
