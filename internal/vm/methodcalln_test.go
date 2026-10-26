package vm

import (
	"testing"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
)

// recv.name(...) with up to three arguments, on a local or this, is
// methodCallN's node: the method read before the arguments, each argument
// evaluated into its slot in turn, then the call, of whatever the method
// is -- a planned tree, a pure body, a leaf, a body ending in a tail call,
// one reading arguments, an arrow, a built-in, a bound function, a getter's
// function, something not callable (after its arguments), nothing at all
// -- and an exception in an argument. Node's answers and order, on the
// trees and in the interpreter; the trees build the nodes.
func TestMethodCallNodesByCount(t *testing.T) {
	const src = `'use strict'
		const log = []
		function arg(v) { log.push('a' + v); return v }
		class P {
			constructor() { this.n = 0 }
			framed(a, b, c) { let t = (a | 0) + (b | 0) + (c | 0); if (t > 1e9) t = 0; return t }
			pure(a) { return a }
			leaf() { return this.n }
			tail(a, b) { return this.framed(a, b, 1) }
			none() { let k = 1; return arguments.length * k }
			four(a, b, c, d) { let s = a + b + c + d; return s }
			viaThis(a, b, c) { let k = 0; return [this.framed(a, b, c), this.four(1, 2, 3, 4), this.leaf(), this.none(), this.pure(a)].join('/') + k }
			get g() { log.push('get'); return function (x, y) { let k = 2; return [this === p, x, y].join() } }
		}
		const p = new P()
		p.arrow = (x) => { let k = 1; return x + k }
		p.native = Math.max
		p.bound = function (a, b) { let k = 0; return this.tag + a + b + k }.bind({ tag: 'B' })
		p.notFn = 7
		function run() {
			const q = p
			const r = []
			for (let i = 0; i < 3; i++) {
				r.push(q.viaThis(1, 2, 3), q.framed(1, 2, 3), q.framed(4, 5), q.framed(6), q.framed())
				r.push(q.pure(arg(1)), q.leaf(), q.tail(arg(2), arg(3)), q.none(), q.none(1, 2, 3), q.four(1, 2, 3, 4))
				r.push(q.g(arg(4), arg(5)), q.arrow(arg(6)), q.native(arg(7), arg(8), arg(9)), q.bound(1, 2))
				try { q.notFn(arg('x')) } catch (e) { r.push(e.constructor.name) }
				try { q.missing() } catch (e) { r.push(e.constructor.name) }
				try { q.framed(arg(10), (() => { throw new RangeError('in arg') })()) } catch (e) { r.push(e.constructor.name + ':' + e.message) }
			}
			return r.join(' ') + ' | ' + log.join()
		}
		run()`
	const want = "6/10/0/0/10 6 9 6 0 1 0 6 0 3 10 true,4,5 7 9 B120 TypeError TypeError RangeError:in arg 6/10/0/0/10 6 9 6 0 1 0 6 0 3 10 true,4,5 7 9 B120 TypeError TypeError RangeError:in arg 6/10/0/0/10 6 9 6 0 1 0 6 0 3 10 true,4,5 7 9 B120 TypeError TypeError RangeError:in arg | a1,a2,a3,get,a4,a5,a6,a7,a8,a9,ax,a10,a1,a2,a3,get,a4,a5,a6,a7,a8,a9,ax,a10,a1,a2,a3,get,a4,a5,a6,a7,a8,a9,ax,a10"
	built := 0
	orig := methodCallN
	methodCallN = func(recv tentry, get bytecode.Instr, gpc int, args []tval, p, pc int) tval {
		n := orig(recv, get, gpc, args, p, pc)
		if n != nil {
			built++
		}
		return n
	}
	defer func() { methodCallN = orig }()
	for _, tree := range []bool{true, false} {
		previous := treeTier.Swap(tree)
		r := New(Config{})
		v, err := r.Run(compileForTest(t, src))
		r.Close()
		treeTier.Store(previous)
		if err != nil {
			t.Fatalf("tree %v: %v", tree, err)
		}
		if got := v.String().Go(); got != want {
			t.Errorf("tree %v: got %s\nwant %s", tree, got, want)
		}
	}
	if built < 20 {
		t.Errorf("%d nodes built by count, want the run's and viaThis's", built)
	}
}

// f(...) and a method call of a receiver in its slot, with up to three
// arguments, are callOperandsN's node: the callee evaluated, then each
// argument into its slot in turn, then the call, of whatever the callee
// is -- a planned tree, a pure body, a body ending in a tail call, one
// reading arguments, a bound function, an arrow, a built-in, a method of a
// getter's object, of an element, of a nested object, something not
// callable (after its arguments) -- and an exception in an argument.
// Node's answers and order, on the trees and in the interpreter; the
// trees build the nodes.
func TestCallNodesByCount(t *testing.T) {
	const src = `'use strict'
		const log = []
		function arg(v) { log.push('a' + v); return v }
		function framed(a, b, c) { let t = (a | 0) + (b | 0) + (c | 0); if (t > 1e9) t = 0; return t }
		function pure(a) { return a }
		function tail(a, b) { return framed(a, b, 1) }
		function count() { let k = 1; return arguments.length * k }
		function four(a, b, c, d) { let s = a + b + c + d; return s }
		class P { m(a, b) { let k = 0; return [this.tag, a, b, k].join() } }
		const holder = { get p() { log.push('get'); const o = new P(); o.tag = 'P'; return o } }
		const arr = [new P(), new P()]
		arr[0].tag = 'z'; arr[1].tag = 'o'
		function run() {
			const r = []
			const bound = framed.bind(null, 100)
			const arrow = (x) => { let k = 1; return x + k }
			const notFn = 7
			const nested = { inner: { f(x) { let k = 3; return x * k } } }
			for (let i = 0; i < 3; i++) {
				r.push(framed(1, 2, 3), framed(4, 5), framed(6), framed(), pure(arg(1)), tail(arg(2), arg(3)), count(), count(1, 2, 3), four(1, 2, 3, 4))
				r.push(bound(1, 2), arrow(arg(4)), Math.max(arg(5), arg(6), arg(7)), String(arg(8)))
				r.push(holder.p.m(arg(9), arg(10)), arr[i & 1].m(arg(11)), nested.inner.f(arg(12)), arr[0].m())
				try { notFn(arg('x')) } catch (e) { r.push(e.constructor.name) }
				try { framed(arg(13), (() => { throw new RangeError('in arg') })()) } catch (e) { r.push(e.constructor.name + ':' + e.message) }
			}
			return r.join(' ') + ' | ' + log.join()
		}
		run()`
	const want = "6 9 6 0 1 6 0 3 10 103 5 7 8 P,9,10,0 z,11,,0 36 z,,,0 TypeError RangeError:in arg 6 9 6 0 1 6 0 3 10 103 5 7 8 P,9,10,0 o,11,,0 36 z,,,0 TypeError RangeError:in arg 6 9 6 0 1 6 0 3 10 103 5 7 8 P,9,10,0 z,11,,0 36 z,,,0 TypeError RangeError:in arg | a1,a2,a3,a4,a5,a6,a7,a8,get,a9,a10,a11,a12,ax,a13,a1,a2,a3,a4,a5,a6,a7,a8,get,a9,a10,a11,a12,ax,a13,a1,a2,a3,a4,a5,a6,a7,a8,get,a9,a10,a11,a12,ax,a13"
	built := 0
	orig := callOperandsN
	callOperandsN = func(op bytecode.Op, f tval, args []tval, p, pc int) tval {
		n := orig(op, f, args, p, pc)
		if n != nil {
			built++
		}
		return n
	}
	defer func() { callOperandsN = orig }()
	for _, tree := range []bool{true, false} {
		previous := treeTier.Swap(tree)
		r := New(Config{})
		v, err := r.Run(compileForTest(t, src))
		r.Close()
		treeTier.Store(previous)
		if err != nil {
			t.Fatalf("tree %v: %v", tree, err)
		}
		if got := v.String().Go(); got != want {
			t.Errorf("tree %v: got %s\nwant %s", tree, got, want)
		}
	}
	if built < 15 {
		t.Errorf("%d nodes built by count, want the run's", built)
	}
}
