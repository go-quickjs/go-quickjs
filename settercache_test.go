package quickjs_test

import "testing"

// TestSetterCache covers writes whose site's cache remembers a setter, as a
// read's remembers a getter: the setter is called with the object written
// as this, as it is when the write is made, and the cache must notice
// anything that changes which property a write reaches. Each script writes
// enough times for the cache to be used.
func TestSetterCache(t *testing.T) {
	cases := []struct{ src, want string }{
		{`class A { set v(x) { this.log.push("A" + x) } } class B extends A {} class C extends B { constructor() { super(); this.log = [] } }
		  var o = new C(); for (var i = 0; i < 3; i++) o.v = i
		  Object.defineProperty(A.prototype, "v", { set(x) { this.log.push("A2:" + x) }, configurable: true }); o.v = 3
		  Object.defineProperty(o, "v", { value: "own", writable: true, configurable: true }); o.v = 4
		  o.log.join() + " " + o.v`, "A0,A1,A2,A2:3 4"},
		{`var proto = { set v(x) { this.got = x } }, objs = [], r = []
		  for (var i = 0; i < 4; i++) { var o = Object.create(proto); o.v = i; objs.push(o.got) } r.push(objs.join())
		  Object.defineProperty(proto, "v", { value: 0, writable: true, configurable: true })
		  var o2 = Object.create(proto); o2.v = 9; r.push(o2.v, Object.keys(o2).join(), proto.v)
		  var f = Object.freeze(Object.create({ set w(x) { r.push("frozen set " + x) } })); for (var i = 0; i < 2; i++) f.w = i
		  r.join(" ")`, "0,1,2,3 9 v 0 frozen set 0 frozen set 1"},
		{`"use strict"; var o = { get g() { return 1 } }, r = []
		  for (var i = 0; i < 3; i++) { try { o.g = i } catch (e) { r.push(e.name) } }
		  var p = { set s(x) { if (x == 2) throw new RangeError("two"); r.push(x) } }
		  for (var i = 0; i < 4; i++) { try { p.s = i } catch (e) { r.push(e.name) } }
		  r.join()`, "TypeError,TypeError,TypeError,0,1,RangeError,3"},
		{`var proto = { set v(x) { this.seen = x } }, o = Object.create(proto), r = []
		  function put(o, x) { o.v = x } function putStrict(o, x) { "use strict"; o.v = x }
		  for (var i = 0; i < 3; i++) { put(o, i); putStrict(o, i) }
		  Object.defineProperty(proto, "v", { get() { return "g" }, set: undefined, configurable: true })
		  put(o, 9); try { putStrict(o, 9) } catch (e) { r.push(e.name) }
		  r.push(o.seen, Object.keys(o).join(), o.v); r.join()`, "TypeError,2,seen,g"},
		{`var o = { get g() { return 1 } }, s = 0; for (var i = 0; i < 3; i++) { o.g = i; s += o.g }
		  function W() {} Object.defineProperty(W.prototype, "x", { set(v) { this.y = v * 2 }, configurable: true })
		  var w = new W(); function put(o, v) { o.x = v } for (var i = 0; i < 5; i++) put(w, i)
		  s + " " + w.y + " " + Object.keys(w).join()`, "3 8 y"},
	}
	for _, tc := range cases {
		checkEval(t, tc.src, tc.want)
	}
}
