package vm

import "testing"

// A function whose body ends in a tail call, an arrow, and the calls a
// frame ends in are made on the callee's tree where planTreeCall has
// given it one (treeRest, callTreeWith, tailFromTree), and answer as the
// interpreter's do: a class's methods returning another's result, a
// recursion through tail calls 100,000 deep -- past the depth a frame is
// given up at -- by itself and by two functions in turn, tail calls of an
// arrow, a bound function, a built-in and a sloppy function that is given
// a primitive this, the arguments of a function a tail call reached, an
// exception from the depth of a chain of them, caught outside it, and an
// arrow's this and new.target, as captured. Each is called once first:
// a function is planned for after a call that ran it. Each allocates,
// so that it is not a pure body, which pureCall evaluates without frames.
func TestTreeRestCalls(t *testing.T) {
	const src = `
		'use strict'
		const out = []
		class Counter {
			constructor() { this.n = 0 }
			inc(k) { this.n += k; return this.get() }
			get() { return this.n }
			twice(k) { this.inc(k); return this.inc(k) }
		}
		const c = new Counter()
		let s = 0
		for (let i = 0; i < 300; i++) s += c.twice(1)
		out.push(s, c.n)
		function down(n, acc) { void [n]; return n === 0 ? acc : down(n - 1, (acc + n) | 0) }
		down(3, 0); out.push(down(100000, 0))
		function even(n) { void [n]; return n === 0 ? true : odd(n - 1) }
		function odd(n) { void [n]; return n === 0 ? false : even(n - 1) }
		even(3); odd(3); out.push(even(100001), odd(100001))
		const add = (a, b) => a + b
		function viaArrow(n) { void [n]; return n === 0 ? add(40, 2) : viaArrow(n - 1) }
		viaArrow(1); out.push(viaArrow(5000))
		const bound = function (x) { return this.base + x }.bind({ base: 100 })
		function viaBound(n) { void [n]; return n === 0 ? bound(5) : viaBound(n - 1) }
		viaBound(1); out.push(viaBound(5000))
		function viaNative(n) { void [n]; return n === 0 ? Math.max(3, 9, 4) : viaNative(n - 1) }
		viaNative(1); out.push(viaNative(5000))
		function viaSloppy(n) { void [n]; return n === 0 ? sloppy.call(7) : viaSloppy(n - 1) }
		viaSloppy(1); out.push(viaSloppy(5000))
		function args() { return [arguments.length, arguments[1], Array.prototype.slice.call(arguments, 2).join('')].join('/') }
		function viaArgs(n) { void [n]; return n === 0 ? args('a', 'b', 'c', 'd') : viaArgs(n - 1) }
		viaArgs(1); out.push(viaArgs(5000))
		function boom(n) { if (n === 0) throw new RangeError('deep'); return boom(n - 1) }
		try { boom(1) } catch (e) {}
		try { boom(50000) } catch (e) { out.push(e.name + ' ' + e.message) }
		function Outer() { this.v = 'outer'; this.f = () => [this.v, typeof new.target] }
		const o = new Outer()
		function viaCaptured(n) { void [n]; return n === 0 ? o.f().join() : viaCaptured(n - 1) }
		for (let i = 0; i < 3; i++) out.push(viaCaptured(100))
		out.join(' | ')`
	// A sloppy function, its this a primitive's wrapper, from a script of
	// its own: the test's is strict.
	const sloppySrc = `function sloppy() { return typeof this + ":" + (this === globalThis) }`
	const want = "90300 | 600 | 705082704 | false | true | 42 | 105 | 9 | object:false | 4/b/cd | RangeError deep | " +
		"outer,function | outer,function | outer,function"
	for _, tree := range []bool{true, false} {
		previous := treeTier.Swap(tree)
		r := New(Config{})
		if _, err := r.Run(compileForTest(t, sloppySrc)); err != nil {
			t.Fatal(err)
		}
		v, err := r.Run(compileForTest(t, src))
		r.Close()
		treeTier.Store(previous)
		if err != nil {
			t.Fatalf("tree %v: %v", tree, err)
		}
		if got := v.String().Go(); got != want {
			t.Errorf("tree %v:\n%s\nwant\n%s", tree, got, want)
		}
	}
}
