package quickjs_test

import "testing"

// TestForInKeyCache covers for-in and Object.keys over plain objects that
// share a layout, whose keys are made once for every object of it, and the
// strings they hand out, which remember the property they name. The keys must
// still follow each object: one made different from the others, a prototype
// that comes to add a key, elements, keys deleted or added during the loop,
// a key used to read some other object, and an array Object.keys returned
// that the script then changes.
func TestForInKeyCache(t *testing.T) {
	cases := []struct{ src, want string }{
		{`function keys(o) { var r = []; for (var k in o) r.push(k + "=" + o[k]); return r.join(",") }
		  var a = { x: 1, y: 2 }, b = { x: 3, y: 4 }, r = [keys(a), keys(b)]
		  Object.defineProperty(b, "x", { enumerable: false }); r.push(keys(b), keys(a))
		  delete a.x; r.push(keys(a), keys({ x: 5, y: 6 }))
		  Object.prototype.z = 9; r.push(keys({ x: 7, y: 8 })); delete Object.prototype.z
		  r.push(keys({ x: 1, y: 2 }), keys({ 1: "e", x: 1, y: 2 }), keys({ x: 1, y: 2, w: 3 }))
		  r.join(" | ")`,
			"x=1,y=2 | x=3,y=4 | y=4 | x=1,y=2 | y=2 | x=5,y=6 | x=7,y=8,z=9 | x=1,y=2 | 1=e,x=1,y=2 | x=1,y=2,w=3"},
		{`var r = [], o = { a: 1, b: 2, c: 3 }
		  for (var k in { a: 1, b: 2, c: 3 }) ;
		  for (var k in o) { r.push(k); if (k == "a") { delete o.b; o.d = 4 } }
		  var p = { a: 1, b: 2, c: 3 }; for (var k in p) { r.push(k); if (k == "a") { delete p.c; p.c = 5 } }
		  r.join()`, "a,c,a,b,c"},
		{`var src = { p: 1, q: 2 }, dst = { p: "P", q: "Q" }, r = []
		  for (var i = 0; i < 2; i++) for (var k in src) r.push(dst[k], k in dst, dst.hasOwnProperty(k), k === "p" || k === "q")
		  r.join()`, "P,true,true,true,Q,true,true,true,P,true,true,true,Q,true,true,true"},
		{`var o = { a: 1, b: 2 }, k1 = Object.keys(o); k1.push("zz"); k1[0] = "changed"
		  var r = [Object.keys(o).join(), Object.keys({ a: 3, b: 4 }).join(), k1.join()]
		  Object.defineProperty(o, "a", { enumerable: false }); r.push(Object.keys(o).join(), Object.keys({ a: 5, b: 6 }).join())
		  r.push(Object.keys({ 2: 0, 1: 0, x: 0 }).join(), Object.keys({ [Symbol()]: 1, s: 1 }).join(), Object.keys(Object.create({ p: 1 })).length)
		  var q = { a: 1, b: 2 }; for (var k of Object.keys(q)) r.push(q[k]); r.join(" ")`,
			"a,b a,b changed,b,zz b a,b 1,2,x s 0 1 2"},
		{`function C() { this.a = 1; this.b = 2 } var r = []
		  for (var k in new C) r.push(k); C.prototype.c = 3; for (var k in new C) r.push(k); r.join()`, "a,b,a,b,c"},
	}
	for _, tc := range cases {
		checkEval(t, tc.src, tc.want)
	}
}

// TestForInArrayElements covers for-in over an array whose keys are its
// elements and nothing else, which walks them by index without a snapshot:
// each key a string, made when it is visited; an element deleted, or cut
// off by a shorter length, not visited, unless the prototype has it by
// then; elements added during the loop not visited; a hole filled during
// the loop visited; holes, other own keys, enumerable keys up the chain,
// sparse arrays, getters and non-enumerable elements walked as before; and
// the keys reading the array and another array.
func TestForInArrayElements(t *testing.T) {
	const src = `var r = [];
	  function keys(a, body) { var ks = ""; for (var k in a) { ks += k + ":" + typeof k + ","; if (body) body(a, k) } return ks }
	  r.push(keys([1, 2, 3]), keys([]), keys([1, , 3]));
	  var a = [1, 2, 3]; a.foo = 1; r.push(keys(a));
	  var b = [1, 2, 3]; Object.defineProperty(b, "hid", { value: 1, enumerable: false }); r.push(keys(b));
	  r.push(keys([1, 2, 3, 4], function (a, k) { if (k == "0") delete a[2] }));
	  r.push(keys([1, 2, 3, 4], function (a, k) { if (k == "0") a.length = 1 }));
	  r.push(keys([1, 2], function (a, k) { a.push(9) }));
	  r.push(keys([1, 2, 3], function (a, k) { if (k == "0") { delete a[1]; Object.defineProperty(Array.prototype, 1, { value: "p", configurable: true }) } }));
	  delete Array.prototype[1];
	  r.push(keys([1, 2, 3], function (a, k) { if (k == "0") { delete a[1]; a[1] = 5 } }));
	  Array.prototype.extra = 1; r.push(keys([1, 2])); delete Array.prototype.extra;
	  Object.prototype.oextra = 1; r.push(keys([1, 2])); delete Object.prototype.oextra;
	  r.push(keys([1, 2, 3], function (a, k) { if (k == "0") Object.freeze(a) }));
	  var s = [1, 2]; s[100000] = 3; r.push(keys(s));
	  var big = []; for (var i = 0; i < 1500; i++) big.push(i);
	  var sum = 0, n = 0, last; for (var k in big) { sum += big[k]; n++; last = k } r.push(sum, n, last, typeof last);
	  var t = [10, 20, 30], acc = []; for (var k in t) acc.push(t[k], k in t, t.hasOwnProperty(k)); r.push(acc.join());
	  var u = [1, 2, 3], v = [4, 5], w = []; for (var k in u) w.push(v[k]); r.push(w.join("/"));
	  (function (x, y) { var z = []; for (var k in arguments) z.push(k + "=" + arguments[k]); r.push(z.join()) })(7, 8);
	  var g = [1, 2]; Object.defineProperty(g, 0, { get() { return 42 }, enumerable: true, configurable: true }); r.push(keys(g), g[0]);
	  var ne = [1, 2]; Object.defineProperty(ne, 0, { value: 1, enumerable: false }); r.push(keys(ne));
	  r.push(keys(new Proxy([1, 2], {})));
	  class A extends Array {} r.push(keys(A.from([1, 2])));
	  var p = [1, 2]; Object.setPrototypeOf(p, { q: 1 }); r.push(keys(p));
	  r.join(" | ")`
	const want = "0:string,1:string,2:string, |  | 0:string,2:string, | 0:string,1:string,2:string,foo:string, | 0:string,1:string,2:string," +
		" | 0:string,1:string,3:string, | 0:string, | 0:string,1:string, | 0:string,1:string,2:string, | 0:string,1:string,2:string," +
		" | 0:string,1:string,extra:string, | 0:string,1:string,oextra:string, | 0:string,1:string,2:string, | 0:string,1:string,100000:string," +
		" | 1124250 | 1500 | 1499 | string | 10,true,true,20,true,true,30,true,true | 4/5/ | 0=7,1=8 | 0:string,1:string, | 42 | 1:string," +
		" | 0:string,1:string, | 0:string,1:string, | 0:string,1:string,q:string,"
	checkEval(t, src, want)
}
