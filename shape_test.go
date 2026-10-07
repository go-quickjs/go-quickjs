package quickjs_test

import (
	"testing"

	quickjs "github.com/go-quickjs/go-quickjs"
)

// TestPropertyCaches covers what a property read's or write's cache must
// notice: each case runs a site until its cache has filled, then changes
// something the cached answer depended on, and the site must see the change.
func TestPropertyCaches(t *testing.T) {
	cases := []struct{ src, want string }{
		// A read of an own property, and of one up the chain, sees a new
		// value, a property added in between, and one deleted.
		{`function get(o) { return o.x }
		  var o = {x: 1, y: 2}, r = []
		  for (var i = 0; i < 3; i++) r.push(get(o))
		  o.x = 5; r.push(get(o))
		  delete o.x; r.push(get(o))
		  r.join()`, "1,1,1,5,"},
		{`function get(o) { return o.m }
		  function C() {} C.prototype.m = "proto"
		  var o = new C(), r = []
		  for (var i = 0; i < 3; i++) r.push(get(o))
		  o.m = "own"; r.push(get(o))
		  delete o.m; C.prototype.m = "changed"; r.push(get(o))
		  r.join()`, "proto,proto,proto,own,changed"},
		{`function get(o) { return o.m }
		  function B() {} B.prototype.m = "base"
		  function C() {} C.prototype = Object.create(B.prototype)
		  var o = new C(), r = []
		  for (var i = 0; i < 3; i++) r.push(get(o))
		  C.prototype.m = "middle"; r.push(get(o))
		  Object.setPrototypeOf(o, {m: "other"}); r.push(get(o))
		  r.join()`, "base,base,base,middle,other"},
		// A property that becomes an accessor is called, not read.
		{`function get(o) { return o.x }
		  var o = {x: 1}, r = []
		  for (var i = 0; i < 3; i++) r.push(get(o))
		  Object.defineProperty(o, "x", {get() { return "getter" }})
		  r.push(get(o)); r.join()`, "1,1,1,getter"},
		{`function get(o) { return o.x }
		  var p = {x: 1}, o = Object.create(p), r = []
		  for (var i = 0; i < 3; i++) r.push(get(o))
		  Object.defineProperty(p, "x", {get() { return "getter" }})
		  r.push(get(o)); r.join()`, "1,1,1,getter"},
		// Objects built alike share a layout; one built differently does
		// not read the other's position.
		{`function get(o) { return o.b }
		  var r = []
		  for (var i = 0; i < 3; i++) r.push(get({a: i, b: i * 10}))
		  r.push(get({b: "first", a: 0}))
		  r.push(get({a: 0}))
		  r.join()`, "0,10,20,first,"},
		// A read that meets many layouts gives up its own cache and uses the
		// shared one, which must tell them apart by key and layout.
		{`function get(o) { return o.k }
		  var r = 0
		  for (var n = 0; n < 40; n++) {
		    var o = {}; o["p" + n] = 1; o.k = n; r += get(o)
		  }
		  var p = {k: 1000}; var q = Object.create(p); q["z"] = 1
		  r += get(q); p.k = 2000; r += get(q)
		  r`, "3780"},
		{`var r = []
		  for (var n = 0; n < 40; n++) {
		    var o = {}; o["p" + n] = n; r.push(o.valueOf === Object.prototype.valueOf)
		  }
		  Object.prototype.valueOf = function () { return 1 }
		  var o = {}; o.w = 1; r.push(o.valueOf === Object.prototype.valueOf)
		  r.every(Boolean)`, "true"},
		// A class maker's constructors run the same code for every class.
		{`var Class = { create() { return function () { this.initialize.apply(this, arguments) } } }
		  var A = Class.create(); A.prototype = { initialize(x) { this.a = x } }
		  var B = Class.create(); B.prototype = { initialize(x) { this.b = x; this.a = -x } }
		  var s = 0
		  for (var i = 0; i < 20; i++) { s += new A(i).a; s += new B(i).a + new B(i).b }
		  s`, "190"},

		// A write to a property the object has sees it become read-only, an
		// accessor, or frozen.
		{`"use strict"
		  function set(o, v) { o.x = v }
		  var o = {x: 0}
		  for (var i = 0; i < 3; i++) set(o, i)
		  Object.defineProperty(o, "x", {writable: false})
		  try { set(o, 9); "no error" } catch (e) { e.constructor.name + " " + o.x }`, "TypeError 2"},
		{`function set(o, v) { o.x = v }
		  var o = {x: 0}, log = []
		  for (var i = 0; i < 3; i++) set(o, i)
		  Object.defineProperty(o, "x", {set(v) { log.push(v) }})
		  set(o, 7); log.join()`, "7"},
		{`function set(o, v) { o.x = v }
		  var o = {x: 0}
		  for (var i = 0; i < 3; i++) set(o, i)
		  Object.freeze(o); set(o, 9); o.x`, "2"},
		// A write that adds a property along a cached transition sees the
		// object made non-extensible, and a setter or a read-only property
		// put on the chain.
		{`function set(o) { o.y = 1 }
		  function mk() { return {x: 0} }
		  var r = []
		  for (var i = 0; i < 3; i++) { var o = mk(); set(o); r.push(o.y) }
		  var o = mk(); Object.preventExtensions(o); set(o); r.push(o.y)
		  r.join()`, "1,1,1,"},
		{`function P() {} function set(o) { o.y = 1 }
		  var log = []
		  for (var i = 0; i < 3; i++) { var o = new P(); set(o); log.push(o.hasOwnProperty("y")) }
		  Object.defineProperty(P.prototype, "y", {set(v) { log.push("setter " + v) }})
		  var o = new P(); set(o); log.push(o.hasOwnProperty("y"))
		  log.join()`, "true,true,true,setter 1,false"},
		{`"use strict"
		  function P() {} function set(o) { o.y = 1 }
		  for (var i = 0; i < 3; i++) set(new P())
		  Object.defineProperty(Object.prototype, "y", {value: 0, writable: false, configurable: true})
		  try { set(new P()); "no error" } catch (e) { e.constructor.name }
		  finally { delete Object.prototype.y }`, "TypeError"},
		// A writable data property up the chain is shadowed, not written.
		{`function V(x) { this.x = x }
		  V.prototype = {x: 0}
		  var r = []
		  for (var i = 1; i < 4; i++) r.push(new V(i).x)
		  r.push(V.prototype.x, Object.keys(new V(5)).join())
		  r.join()`, "1,2,3,0,x"},
		// Keys keep their order however the layout was reached.
		{`function P(a) { this.a = a; this.b = 2 }
		  var o
		  for (var i = 0; i < 3; i++) o = new P(i)
		  o.c = 3; delete o.a; o.a = 4
		  Object.keys(o).join() + " " + o.a + o.b + o.c`, "b,c,a 423"},
		{`var o
		  for (var i = 0; i < 3; i++) o = {b: 1, a: 2, 1: "one"}
		  o[0] = "zero"
		  Object.keys(o).join()`, "0,1,b,a"},
		// Tables past the scanned size have an index, which the layout shares.
		{`function mk(n) { var o = {}; for (var i = 0; i < n; i++) o["k" + i] = i; return o }
		  var a = mk(20), b = mk(20)
		  delete a.k3; a.k3 = "again";
		  [a.k3, b.k3, a.k19, b.k19, Object.keys(a).pop(), Object.keys(b).pop()].join()`, "again,3,19,19,k3,k19"},
		// An array's length and a function's name are made on demand, not
		// read from a table.
		{`function len(o) { return o.length }
		  var r = []
		  for (var i = 0; i < 3; i++) r.push(len([1, 2, 3].slice(i)))
		  r.push(len(function (a, b) {}))
		  r.join()`, "3,2,1,2"},
		{`function nm(f) { return f.name }
		  var r = []
		  for (var i = 0; i < 3; i++) r.push(nm(function g() {}))
		  var h = function () {}; h.extra = 1; r.push(nm(h))
		  r.join()`, "g,g,g,h"},
		// A method read on an array or a function, whose tables are empty,
		// sees the method replaced.
		{`function call(a) { return a.push(1) }
		  var r = ""
		  for (var i = 0; i < 3; i++) r += call([]) + ","
		  Array.prototype.push = function () { return "patched" }
		  r + call([])`, "1,1,1,patched"},
	}
	for _, tc := range cases {
		checkEval(t, tc.src, tc.want)
	}
}

// TestLiteralSizes covers object literals of every size that carries its
// table in its own allocation, grown past it and changed afterwards.
func TestLiteralSizes(t *testing.T) {
	cases := []struct{ src, want string }{
		{`var r = []
		  for (var n = 0; n <= 10; n++) {
		    var o = n == 0 ? {} : n == 1 ? {a: 1} : n == 2 ? {a: 1, b: 2} : n == 3 ? {a: 1, b: 2, c: 3} :
		      n == 4 ? {a: 1, b: 2, c: 3, d: 4} : n == 5 ? {a: 1, b: 2, c: 3, d: 4, e: 5} :
		      n == 6 ? {a: 1, b: 2, c: 3, d: 4, e: 5, f: 6} : n == 7 ? {a: 1, b: 2, c: 3, d: 4, e: 5, f: 6, g: 7} :
		      n == 8 ? {a: 1, b: 2, c: 3, d: 4, e: 5, f: 6, g: 7, h: 8} :
		      n == 9 ? {a: 1, b: 2, c: 3, d: 4, e: 5, f: 6, g: 7, h: 8, i: 9} :
		      {a: 1, b: 2, c: 3, d: 4, e: 5, f: 6, g: 7, h: 8, i: 9, j: 10}
		    o.x = "x"; o.y = "y"; delete o.a; o.a = 0
		    r.push(Object.keys(o).join("") + "=" + Object.values(o).join(""))
		  }
		  r.join()`, "xya=xy0,xya=xy0,bxya=2xy0,bcxya=23xy0,bcdxya=234xy0,bcdexya=2345xy0,bcdefxya=23456xy0," +
			"bcdefgxya=234567xy0,bcdefghxya=2345678xy0,bcdefghixya=23456789xy0,bcdefghijxya=2345678910xy0"},
	}
	for _, tc := range cases {
		checkEval(t, tc.src, tc.want)
	}
}

// TestInstanceOfCache covers instanceof, whose read of the constructor's
// Symbol.hasInstance is cached: a site must notice the method defined on the
// constructor, a replaced prototype, and constructors of other kinds.
func TestInstanceOfCache(t *testing.T) {
	cases := []struct{ src, want string }{
		{`function test(o, C) { return o instanceof C }
		  function C() {} var r = []
		  for (var i = 0; i < 3; i++) r.push(test(new C(), C))
		  Object.defineProperty(C, Symbol.hasInstance, {value: () => "yes"})
		  r.push(test({}, C))
		  C.prototype = {}; r.push(test(new C(), C))
		  r.join()`, "true,true,true,true,true"},
		{`function test(o, C) { return o instanceof C }
		  function C() {} var o = new C(), r = []
		  for (var i = 0; i < 3; i++) r.push(test(o, C))
		  C.prototype = {}; r.push(test(o, C))
		  r.push(test(o, C.bind(null)), test(new C(), C.bind(null)))
		  var log = []
		  var P = new Proxy(C, {get(t, k) { log.push(typeof k); return Reflect.get(t, k) }})
		  r.push(test(new C(), P), log.join())
		  r.join()`, "true,true,true,false,false,true,true,symbol,string"},
		{`function test(o, C) { return o instanceof C }
		  class A {} class B extends A {} var r = []
		  for (var i = 0; i < 3; i++) r.push(test(new B(), A), test(new A(), B))
		  class D { static [Symbol.hasInstance](v) { return v === 1 } }
		  r.push(test(1, D), test(new D(), D))
		  r.join()`, "true,false,true,false,true,false,true,false"},
		{`"use strict"
		  function test(o, C) { return o instanceof C }
		  function C() {} for (var i = 0; i < 3; i++) test({}, C)
		  var e = []
		  for (var bad of [{}, 1, {[Symbol.hasInstance]: 1}]) {
		    try { test({}, bad); e.push("none") } catch (x) { e.push(x.constructor.name) }
		  }
		  e.join()`, "TypeError,TypeError,TypeError"},
	}
	for _, tc := range cases {
		checkEval(t, tc.src, tc.want)
	}
}

// TestGetterCache covers a property read whose cache remembers an accessor:
// the getter is called with the object read as this, and the site must see
// the getter redefined, removed, turned into a data property or thrown from.
func TestGetterCache(t *testing.T) {
	cases := []struct{ src, want string }{
		{`class P { constructor(x) { this.x = x } get double() { return this.x * 2 } }
		  class Q extends P {}
		  function read(o) { return o.double }
		  var r = []; for (var i = 1; i <= 3; i++) r.push(read(new P(i)), read(new Q(i * 10)))
		  Object.defineProperty(P.prototype, "double", {get() { return "new " + this.x }, configurable: true})
		  r.push(read(new Q(5)))
		  Object.defineProperty(P.prototype, "double", {value: "data", configurable: true})
		  r.push(read(new P(1)))
		  delete P.prototype.double; r.push(read(new P(1)))
		  r.join()`, "2,20,4,40,6,60,new 5,data,"},
		{`var o = {get g() { throw new RangeError("no") }, set s(v) {}}
		  function g(o) { return o.g } function s(o) { return o.s }
		  var r = []; for (var i = 0; i < 3; i++) { try { g(o) } catch (e) { r.push(e.name) } r.push(s(o)) }
		  r.join()`, "RangeError,,RangeError,,RangeError,"},
		{`var log = [], base = {get who() { log.push(this.name); return this.name }}
		  var mid = Object.create(base), a = Object.create(mid), b = Object.create(mid)
		  a.name = "a"; b.name = "b"
		  function who(o) { return o.who }
		  [who(a), who(b), who(a), who(mid), log.length].join()`, "a,b,a,,4"},
	}
	for _, tc := range cases {
		checkEval(t, tc.src, tc.want)
	}
}

// TestGlobalReadCache covers a global read that remembers where in the
// global object its name was: it must see the global deleted and made again,
// turned into a getter, moved by the table's reordering, and shadowed by a
// script's let or a direct eval's var.
func TestGlobalReadCache(t *testing.T) {
	cases := []struct{ src, want string }{
		{`globalThis.g1 = "a"; function read() { try { return g1 } catch (e) { return e.name } }
		  var r = [read(), read()]
		  delete globalThis.g1; r.push(read())
		  globalThis.g1 = "b"; r.push(read())
		  Object.defineProperty(globalThis, "g1", {get() { return "getter" }, configurable: true}); r.push(read())
		  delete globalThis.g1; globalThis.g1 = "c"; globalThis[0] = "zero"; r.push(read(), read())
		  delete globalThis[0]; r.join()`, "a,a,ReferenceError,b,getter,c,c"},
		{`globalThis.g4a = "A"; globalThis.g4 = "B"; function read() { return g4 }
		  var r = [read(), read()]; globalThis[5] = "index"; r.push(read())
		  delete globalThis[5]; r.push(read()); r.join()`, "B,B,B,B"},
		{`globalThis.g3 = "global"
		  function f(code) { eval(code); return g3 }
		  [f(""), f(""), f("var g3 = 'eval'"), f("")].join()`, "global,global,eval,global"},
	}
	for _, tc := range cases {
		checkEval(t, tc.src, tc.want)
	}
}

// TestGlobalWriteCache covers a global write that remembers where in the
// global object its name was, sloppy and strict: it must see the global
// deleted and made again, made read-only, turned into a setter, and shadowed
// by a direct eval's var.
func TestGlobalWriteCache(t *testing.T) {
	cases := []struct{ src, want string }{
		{`globalThis.w1 = 0; function put(v) { w1 = v }
		  var r = []; put(1); put(2); r.push(w1)
		  delete globalThis.w1; put(3); r.push(w1)
		  Object.defineProperty(globalThis, "w1", {value: 4, writable: false, configurable: true}); put(5); r.push(w1)
		  var seen; Object.defineProperty(globalThis, "w1", {set(v) { seen = v }, configurable: true}); put(6); r.push(seen)
		  delete globalThis.w1; globalThis.w1 = 7; put(8); r.push(w1); r.join()`, "2,3,4,6,8"},
		{`globalThis.w2 = 0; function put(v) { "use strict"; w2 = v }
		  var r = []; put(1); put(2); r.push(w2)
		  delete globalThis.w2; try { put(3) } catch (e) { r.push(e.name) }
		  globalThis.w2 = 4; put(5); r.push(w2)
		  Object.defineProperty(globalThis, "w2", {value: 6, writable: false, configurable: true})
		  try { put(7) } catch (e) { r.push(e.name) }; r.push(w2); r.join()`, "2,ReferenceError,5,TypeError,6"},
		{`globalThis.w3 = "global"
		  function f(code, v) { eval(code); w3 = v; return w3 }
		  var r = [f("", 1), f("", 2), f("var w3", 3), globalThis.w3, f("", 4)]; r.join()`, "1,2,3,2,4"},
	}
	for _, tc := range cases {
		checkEval(t, tc.src, tc.want)
	}
}

// TestGlobalWriteCacheShadowed covers a global write made again after a
// later script declares a let of the same name: the write must go to the
// let, not to where the property was.
func TestGlobalWriteCacheShadowed(t *testing.T) {
	rt := quickjs.New()
	defer rt.Close()
	evalString(t, rt, `globalThis.w4 = "prop"; function put(v) { w4 = v }; put("a"); put("b")`)
	evalString(t, rt, `let w4 = "lex"`)
	if got := evalString(t, rt, `put("c"); [w4, globalThis.w4].join()`); got != "c,b" {
		t.Errorf("got %s, want c,b", got)
	}
}

// TestGlobalReadCacheShadowed covers a global read made again after a later
// script declares a let of the same name, which is looked for before the
// global object: the read must find the let, not where the property was.
func TestGlobalReadCacheShadowed(t *testing.T) {
	rt := quickjs.New()
	defer rt.Close()
	evalString(t, rt, `globalThis.g2 = "prop"; function read() { return g2 }; read(); read()`)
	evalString(t, rt, `let g2 = "lex"`)
	if got := evalString(t, rt, `[read(), globalThis.g2].join()`); got != "lex,prop" {
		t.Errorf("got %s, want lex,prop", got)
	}
}

// TestPropertyCachesOtherClasses covers reads the caches remember on objects
// of the classes that are not plain objects, arrays or functions -- a WeakMap,
// a DataView, a generator, an iterator -- and on a prototype that was given
// its first property before its own prototype had a shape, as
// %MapIteratorPrototype% was. A method read must see the method replaced, an
// own property shadowing it, and that property deleted, as a plain object's
// does.
func TestPropertyCachesOtherClasses(t *testing.T) {
	cases := []struct{ src, want string }{
		{`function get(o) { return o.get }
		  var w = new WeakMap(), orig = WeakMap.prototype.get, r = []
		  for (var i = 0; i < 3; i++) r.push(get(w) === orig)
		  w.get = "own"; r.push(get(w))
		  delete w.get; r.push(get(w) === orig)
		  WeakMap.prototype.get = "replaced"; r.push(get(w))
		  WeakMap.prototype.get = orig; r.join()`, "true,true,true,own,true,replaced"},
		{`var dv = new DataView(new ArrayBuffer(4)), r = []
		  function read() { return dv.getUint8(0) }
		  for (var i = 0; i < 3; i++) r.push(read())
		  Object.defineProperty(DataView.prototype, "getUint8", {value: function () { return "patched" }, configurable: true, writable: true})
		  r.push(read()); r.join()`, "0,0,0,patched"},
		{`var MapIter = Object.getPrototypeOf(new Map().keys()), next = MapIter.next, r = []
		  function step(it) { return it.next().value }
		  var m = new Map([[1, 1], [2, 2]])
		  r.push(step(m.keys()), step(m.keys()))
		  MapIter.next = function () { return {value: "patched"} }; r.push(step(m.keys()))
		  MapIter.extra = 1; r.push(step(m.keys()))
		  MapIter.next = next; delete MapIter.extra; r.push(step(m.keys()))
		  var it = m.values(); it.next = function () { return {value: "own"} }; r.push(step(it))
		  r.join()`, "1,1,patched,patched,1,own"},
		{`function* g() { yield 1; yield 2 }
		  var GenProto = Object.getPrototypeOf(g()), r = []
		  function step(it) { return it.next().value }
		  var it = g(); r.push(step(it), step(it))
		  var next = Object.getPrototypeOf(GenProto).next
		  Object.getPrototypeOf(GenProto).next = function () { return {value: "patched"} }
		  r.push(step(g()))
		  Object.getPrototypeOf(GenProto).next = next; r.push(step(g())); r.join()`, "1,2,patched,1"},
	}
	for _, tc := range cases {
		checkEval(t, tc.src, tc.want)
	}
}

// TestPrimitivePropertyCache covers reading a method of a primitive, which is
// remembered by where it was found on the prototype: the method replaced,
// deleted, made an accessor, shadowed by one added to the prototype after a
// miss, and the prototype's own prototype changed, must all be seen.
func TestPrimitivePropertyCache(t *testing.T) {
	cases := []struct{ src, want string }{
		{`function f(s) { return s.slice(1) } var slice = String.prototype.slice, r = []
		  r.push(f("abc"), f("xyz"))
		  String.prototype.slice = function () { return "replaced" }; r.push(f("abc"))
		  delete String.prototype.slice; try { f("abc") } catch (e) { r.push(e.name) }
		  Object.defineProperty(String.prototype, "slice", { get() { return function () { return "getter" } }, configurable: true }); r.push(f("abc"))
		  Object.defineProperty(String.prototype, "slice", { value: slice, writable: true, configurable: true }); r.push(f("abc"))
		  r.join()`, "bc,yz,replaced,TypeError,getter,bc"},
		{`function g(n) { return n.custom } var r = []
		  r.push(String(g(1)))
		  Object.prototype.custom = "object"; r.push(g(1))
		  Number.prototype.custom = "number"; r.push(g(2))
		  delete Number.prototype.custom; r.push(g(3)); delete Object.prototype.custom
		  r.push(String(g(4)), (12.5).toFixed(1), true.toString(), "q".at(0))
		  r.join()`, "undefined,object,number,object,undefined,12.5,true,q"},
		{`function h(s) { return s.trim } var trim = String.prototype.trim
		  var r = [h("a") === trim]; Object.freeze(String.prototype); r.push(h("a") === trim)
		  r.join()`, "true,true"},
	}
	for _, tc := range cases {
		checkEval(t, tc.src, tc.want)
	}
}

// TestUniqueShapeChangedInPlace covers objects whose layout is their own --
// after a delete -- which change in place until a cache remembers them, and
// are replaced after: each read, write or primitive's method read below runs
// until its cache has filled, and must then see the delete, the property
// added, or the attribute changed that follows. The answers are Node's.
func TestUniqueShapeChangedInPlace(t *testing.T) {
	rt := quickjs.New()
	defer rt.Close()
	v, err := rt.Eval(`var out = [];
		function read(o) { return o.x; }
		function readY(o) { return o.y; }
		function write(o, v) { o.x = v; }
		// An object read by a cache after a delete, then changed again.
		var o = {a: 1, x: 2, y: 3}; delete o.a;
		for (var i = 0; i < 50; i++) read(o);
		delete o.x; out.push(read(o));
		o.x = 7; out.push(read(o));
		delete o.y; out.push(readY(o));
		// Read through a prototype whose layout a delete changed, then changes again.
		var proto = {p: 0, x: "proto"}; delete proto.p;
		var c = Object.create(proto);
		for (var i = 0; i < 50; i++) read(c);
		delete proto.x; out.push(read(c));
		proto.x = "again"; out.push(read(c));
		// A cached absence: x is found on the prototype, then the object gains its own.
		var d = Object.create(proto); d.q = 1; delete d.q;
		for (var i = 0; i < 50; i++) read(d);
		d.x = "own"; out.push(read(d));
		// Deletes one after another with reads between, each read a new site's.
		var e = {a: 1, b: 2, c: 3, d: 4, e: 5, f: 6, g: 7, h: 8, i: 9, j: 10};
		for (var k of "abcdefghi") { delete e[k]; out.push(Object.keys(e).join("")); out.push(e.j); }
		// A write cache on a deleted-from object, then the property deleted.
		var w = {z: 0, x: 1}; delete w.z;
		for (var i = 0; i < 50; i++) write(w, i);
		delete w.x; write(w, "re"); out.push(JSON.stringify(w), Object.keys(w).join());
		Object.defineProperty(w, "x", {writable: false}); try { (function () { "use strict"; write(w, 5); })(); } catch (err) { out.push(err.constructor.name); }
		write(w, 9); out.push(w.x);
		// A primitive's method read through its prototype, then deleted.
		String.prototype.foo = function () { return "foo"; };
		function callFoo(s) { return s.foo ? s.foo() : "none"; }
		for (var i = 0; i < 50; i++) callFoo("abc");
		delete String.prototype.foo; out.push(callFoo("abc"));
		String.prototype.foo = function () { return "back"; }; out.push(callFoo("abc"));
		// for-in over an object whose layout changes during the loop.
		var f = {a: 1, b: 2, c: 3, d: 4}; delete f.a; var seen = [];
		for (var k in f) { seen.push(k); delete f.c; f.e = 5; }
		out.push(seen.join(""));
		// Many deletes on large objects, read by a shared site.
		var sum = 0;
		for (var j = 0; j < 200; j++) {
		  var big = {}; for (var k = 0; k < 20; k++) big["k" + k] = k;
		  for (var k = 0; k < 19; k++) { delete big["k" + k]; sum += big.k19 + (big["k" + (k + 1)] | 0); }
		}
		out.push(sum);
		out.join("|")`)
	if err != nil {
		t.Fatal(err)
	}
	const want = "|7|||again|own|bcdefghij|10|cdefghij|10|defghij|10|efghij|10|fghij|10|" +
		`ghij|10|hij|10|ij|10|j|10|{"x":"re"}|x|re|none|back|bd|110200`
	if got := v.String(); got != want {
		t.Errorf("got  %s\nwant %s", got, want)
	}
}
