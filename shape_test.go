package quickjs_test

import "testing"

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
