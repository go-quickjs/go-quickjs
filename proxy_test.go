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
