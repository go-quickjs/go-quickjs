package quickjs_test

import "testing"

// TestObjectCopies covers object spread and Object.assign from a plain
// object whose layout objects share, which read the values from the places
// the layout has them rather than looking each key up. What they copy, in
// what order, and what a getter or setter sees must not change: enumerable
// keys only, symbols and accessors the long way, a key the target has
// already, and a setter on the target that changes the source mid-way.
func TestObjectCopies(t *testing.T) {
	cases := []struct{ src, want string }{
		{`function show(o) { return Object.keys(o).map(function (k) { return k + "=" + o[k] }).join(",") }
		  var o = { a: 1, b: 2 }, r = []
		  for (var i = 0; i < 3; i++) r.push(show({ ...o, c: i }))
		  r.push(show({ z: 0, ...o }), show({ a: 9, ...o, b: 8 }), show({ ...{ a: 3, b: 4 } }), show(Object.assign({ q: 1 }, o, { b: 5 })))
		  var ne = { a: 1 }; Object.defineProperty(ne, "h", { value: 2, enumerable: false }); r.push(show({ ...ne }))
		  var s = Symbol("s"), sy = { a: 1 }; sy[s] = 2; var c = { ...sy }; r.push(c[s], Object.assign({}, sy)[s])
		  var acc = { a: 1, get b() { return "got" } }; r.push(show({ ...acc }), show(Object.assign({}, acc)))
		  r.push(show({ ...Object.create({ inherited: 1 }), own: 1 }))
		  r.join(" | ")`,
			"a=1,b=2,c=0 | a=1,b=2,c=1 | a=1,b=2,c=2 | z=0,a=1,b=2 | a=1,b=8 | a=3,b=4 | q=1,a=1,b=5 | a=1 | 2 | 2 | a=1,b=got | a=1,b=got | own=1"},
		{`var src = { a: 1, b: 2, c: 3 }, log = []
		  var target = { set a(v) { log.push("a=" + v); delete src.b; src.c = 30; src.d = 4 } }
		  Object.assign(target, src)
		  var src2 = { a: 1, b: 2 }; for (var i = 0; i < 3; i++) Object.assign({}, src2)
		  var t2 = { set a(v) { log.push("set " + v) } }; Object.assign(t2, src2)
		  log.join() + " | " + JSON.stringify(target) + " | " + Object.keys(target).join()`,
			"a=1,set 1 | {\"c\":30} | a,c"},
	}
	for _, tc := range cases {
		checkEval(t, tc.src, tc.want)
	}
}
