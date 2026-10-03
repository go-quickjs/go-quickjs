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
