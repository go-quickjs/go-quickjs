package vm

import "testing"

// The constructor a derived class that declares none is given constructs
// its parent with the arguments it was called with, as they are
// (bytecode.SuperForward): no arguments object is made, and nothing is
// iterated, which a replaced %ArrayIteratorPrototype%.next or
// Array.prototype[Symbol.iterator] would see -- only the one spread the
// script writes calls next, three times. It had been super(...arguments),
// which called next at each construction. The arguments reach the parent as
// they were given -- none, an undefined, a null, through two implicit
// constructors -- and so do those of a class extending Array and a
// function. Each answer is Node's, on the trees and in the interpreter.
func TestImplicitConstructorForwards(t *testing.T) {
	const src = `
		class B { constructor(...a) { this.a = a; this.n = arguments.length } }
		class D extends B {}
		class E extends D {}
		const proto = Object.getPrototypeOf([][Symbol.iterator]())
		const next = proto.next
		let calls = 0
		proto.next = function () { calls++; return next.call(this) }
		const r = []
		r.push(new D(1, 2, 3).a.join(), new E().n, new E(undefined, null).a.length, new D(...[4, 5]).a.join())
		proto.next = next
		const saved = Array.prototype[Symbol.iterator]
		Array.prototype[Symbol.iterator] = function () { calls += 100; return saved.call(this) }
		r.push(new E(7).a.join())
		Array.prototype[Symbol.iterator] = saved
		r.push(calls)
		class F extends Array {}
		r.push(new F(3).length, new F(1, 2).join(), Array.isArray(new F()))
		function P(x, y) { this.s = x + y }
		class G extends P {}
		r.push(new G(2, 3).s, new G().s)
		r.join(' | ')`
	const want = "1,2,3 | 0 | 2 | 4,5 | 7 | 3 | 3 | 1,2 | true | 5 | NaN"
	for _, tree := range []bool{true, false} {
		SetTreeTier(tree)
		r := New(Config{})
		v, err := r.Run(compileForTest(t, src))
		r.Close()
		if err != nil {
			t.Fatal(err)
		}
		if got := v.String().Go(); got != want {
			t.Errorf("tree tier %v: got %s, want %s", tree, got, want)
		}
	}
	SetTreeTier(true)
}
