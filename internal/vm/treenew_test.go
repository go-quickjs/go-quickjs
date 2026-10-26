package vm

import (
	"runtime"
	"testing"
)

// Constructions of classes are made on their trees (constructTree), base
// and derived, a derived one's super() calling its parent the same way,
// with its this binding a cell of the runtime's where nothing can capture
// it. What every construction does is what runFD's did: the binding
// unbound until super() and bound once, the errors of this before super(),
// of no super() and of a return neither an object nor undefined, the
// parent's own object or a constructor's returned one, fields, a parent
// that is a function, new.target, and constructions nested 300 deep before
// and after super() -- the runtime's cells replaced while frames below
// hold theirs. Each answer is the interpreter's and Node's (Node's
// messages are its own), from the first construction, made by runFD,
// through the later ones the plan sends to the trees.
func TestClassConstructionsOnTrees(t *testing.T) {
	const src = `'use strict'
		class A { constructor(x) { this.x = x; if (x === 'throw') throw new RangeError('A ' + x) } }
		class B extends A { constructor(x) { super(x); this.y = new.target.name } }
		class C extends B {
			constructor(x, early) {
				let seen = ''
				if (early) { try { this.z } catch (e) { seen = e.constructor.name } }
				super(x)
				this.z = seen
			}
		}
		class Twice extends A {
			constructor() { super(1); try { super(2) } catch (e) { this.t = e.constructor.name } }
		}
		class None extends A { constructor() {} }
		class Ret extends A { constructor(v) { super(0); return v } }
		class Other extends A { constructor() { super(0); return { other: true } } }
		class Fields extends A { f = this.x + 1; #p = 2; constructor(x) { super(x) } get p() { return this.#p } }
		function P(x) { this.px = x }
		class FromFn extends P { constructor(x) { super(x * 2) } }
		class Arrow extends A { constructor(x) { const g = () => this; super(x); this.same = g() === this } }
		class Nest extends A { constructor(n) { const c = n > 0 ? new Nest(n - 1) : null; super(n); this.c = c } }
		class Deep extends A { constructor(n) { super(n); this.d = n > 0 ? new Deep(n - 1) : null } }
		class Base { constructor(a, b) { this.s = a + b } }
		class Implicit extends B {}
		function err(f) { try { f(); return 'none' } catch (e) { return e.constructor.name + ': ' + e.message } }
		function depth(o, k) { let n = 0; for (; o; o = o[k]) n++; return n }
		function run() {
			const r = []
			const c = new C(1)
			r.push([c.x, c.y, c.z, c instanceof C, c instanceof A, Object.getPrototypeOf(c) === C.prototype].join())
			r.push(new C(2, true).z)
			r.push(err(() => new C('throw')))
			r.push(new Twice().t)
			r.push(err(() => new None()))
			r.push(err(() => new Ret(5)), new Ret(undefined).x, new Ret({ k: 1 }).k)
			r.push(new Other().other, new Other() instanceof Other)
			const f = new Fields(4)
			r.push([f.x, f.f, f.p].join())
			r.push(new FromFn(3).px, new FromFn(3) instanceof P)
			r.push(new Arrow(7).same)
			const n = new Nest(300)
			r.push(depth(n, 'c'), n.x, n.c.x, n.c.c.c.x)
			r.push(depth(new Deep(300), 'd'))
			r.push(new Base(2, 3).s, new Implicit(9).x, new Implicit(9).y)
			r.push(err(() => C(1)), err(() => A(1)))
			return r.join(' | ')
		}
		const first = run()
		let same = true
		for (let i = 0; i < 5; i++) if (run() !== first) same = false
		Object.assign(globalThis, { A, B, C, Base, Arrow, Nest })
		first + ' | same ' + same`
	const want = "1,C,,true,true,true | ReferenceError | RangeError: A throw | ReferenceError | " +
		"ReferenceError: a derived constructor must call super() before returning | " +
		"TypeError: a derived constructor may return only an object or undefined | 0 | 1 | true | false | " +
		"4,5,2 | 6 | true | true | 301 | 300 | 299 | 297 | 301 | 5 | 9 | Implicit | " +
		`TypeError: class constructor C cannot be invoked without "new" | ` +
		`TypeError: class constructor A cannot be invoked without "new" | same true`
	for _, tree := range []bool{true, false} {
		previous := treeTier.Swap(tree)
		r := New(Config{})
		v, err := r.Run(compileForTest(t, src))
		treeTier.Store(previous)
		if err != nil {
			r.Close()
			t.Fatalf("tree %v: %v", tree, err)
		}
		if got := v.String().Go(); got != want {
			t.Errorf("tree %v: got %s\nwant %s", tree, got, want)
		}
		fd := func(name string) *funcData { return r.global.getOwn(r.atoms.intern(name)).value().Object().fn() }
		if tree {
			for _, name := range []string{"A", "C", "Base", "Nest"} {
				if !fd(name).treeNew {
					t.Errorf("%s is not constructed on its tree", name)
				}
			}
			// An arrow made in the body captures its this binding, and the
			// tier builds no tree that reads new.target.
			for _, name := range []string{"Arrow", "B"} {
				if a := fd(name); !a.newPlanned || a.treeNew {
					t.Errorf("%s: planned %v, on its tree %v; want planned, not on its tree", name, a.newPlanned, a.treeNew)
				}
			}
			if len(r.thisCells) <= 300 {
				t.Errorf("%d this cells after constructions 300 deep", len(r.thisCells))
			}
		}
		r.Close()
	}
}

// A construction of a derived class on its tree allocates the object and
// the arguments super() passes on, and no this binding: runFD's had been
// an allocation of its own.
func TestDerivedConstructionAllocations(t *testing.T) {
	const src = `'use strict'
		class A { constructor(x) { this.x = x; this.y = 0 } }
		class B extends A { constructor(x) { super(x); this.z = 1 } }
		function make(n) { let o; for (let i = 0; i < n; i++) o = new B(i); return o.x }
		make(100)`
	r := New(Config{})
	defer r.Close()
	if _, err := r.Run(compileForTest(t, src)); err != nil {
		t.Fatal(err)
	}
	makeFn := r.global.getOwn(r.atoms.intern("make")).value()
	const n = 10000
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	if _, err := r.Call(makeFn, Undefined, []Value{Int(n)}); err != nil {
		t.Fatal(err)
	}
	runtime.ReadMemStats(&after)
	if per := float64(after.Mallocs-before.Mallocs) / n; per > 2.05 {
		t.Errorf("%.2f allocations a derived construction, want at most 2", per)
	}
}
