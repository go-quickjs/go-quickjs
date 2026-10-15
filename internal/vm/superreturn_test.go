package vm

import "testing"

// super() binds this to what the parent's construction gave. An object a
// script's constructor returns of its own -- a plain object, an array, a
// Map, from a class or a function -- keeps its prototype: class R extends
// P, P returning {}, makes no R, and fields set after super() go on that
// object. It had been given R's prototype, as a native constructor's
// object is, Error's and Array's among them, which build their own and
// are still instances of the class extending them. Each answer is Node's,
// on the trees and in the interpreter.
func TestSuperReturnsOwnObject(t *testing.T) {
	const src = `
		const r = []
		class P { constructor(k) { if (k === 1) return { own: 1 }; if (k === 2) return [1, 2]; if (k === 3) return new Map([[1, 1]]); this.v = k } }
		class R extends P { constructor(k) { super(k); this.after = k } }
		class RR extends R {}
		for (const k of [0, 1, 2, 3]) { const x = new R(k), y = new RR(k); r.push([x instanceof R, x instanceof P, Object.getPrototypeOf(x) === R.prototype, x.after, Array.isArray(x), y instanceof RR, y instanceof R].join(':')) }
		function F() { return { f: 1 } }
		class FR extends F { constructor() { super(); this.g = 2 } }
		const fr = new FR()
		r.push([fr instanceof FR, fr.f, fr.g, Object.getPrototypeOf(fr) === Object.prototype].join(':'))
		class E extends Error { constructor(m) { super(m) } }
		class A extends Array { constructor() { super(3) } }
		class M extends Map { constructor() { super() } }
		class D extends Date { constructor() { super(0) } }
		class X extends RegExp { constructor() { super('a') } }
		for (const C of [E, A, M, D, X]) { const x = new C('m'); r.push(C.name + ':' + (x instanceof C) + ':' + (Object.getPrototypeOf(x) === C.prototype)) }
		r.join(' ')`
	const want = "true:true:true:0:false:true:true false:false:false:1:false:false:false false:false:false:2:true:false:false " +
		"false:false:false:3:false:false:false false:1:2:true E:true:true A:true:true M:true:true D:true:true X:true:true"
	for _, tree := range []bool{true, false} {
		SetTreeTier(tree)
		r := New(Config{})
		v, err := r.Run(compileForTest(t, src))
		r.Close()
		if err != nil {
			t.Fatal(err)
		}
		if got := v.String().Go(); got != want {
			t.Errorf("tree tier %v: got\n%s\nwant\n%s", tree, got, want)
		}
	}
	SetTreeTier(true)
}
