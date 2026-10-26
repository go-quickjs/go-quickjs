package vm

import "testing"

// super(a, b) passes its arguments in the derived constructor's slots, as
// a call node passes a call's, rather than a copy of them: the parent's
// frame is made above the constructor's window, and nothing keeps the
// slice past the call -- an arguments object and a rest parameter copy
// it, as do Array's, Map's and Error's constructors. The parent keeps its
// arguments object, its rest array and a default computed by a call while
// the constructor goes on computing in its slots; built-ins and a parent
// whose argument is a construction of the same class get theirs as they
// were given. Node's answers, on the trees and in the interpreter.
func TestSuperArgumentsInPlace(t *testing.T) {
	const src = `'use strict'
		class Keep { constructor(a, b = a * 10, ...rest) { this.args = arguments; this.b = b; this.rest = rest; this.n = arguments.length } }
		function calc(k) { let s = 0; for (let i = 0; i < k; i++) s += i; return s }
		class K extends Keep {
			constructor(x, y, z) {
				super(x + 1, calc(y), z * 2, 'w')
				const after = [calc(4), x * 3, y * 4, z * 5, 'q', 'r']
				this.after = after.join()
			}
		}
		class Arr extends Array { constructor(a, b) { super(a, b, a + b); this.tail = calc(5) + a } }
		class M extends Map { constructor(k, v) { super([[k, v]]); this.extra = calc(3) } }
		class E extends Error { constructor(m) { super(m + '!', { cause: m }); this.more = calc(6) } }
		class Nested extends Keep { constructor(n) { super(n, n > 0 ? new Nested(n - 1).n : -1, n) } }
		const out = []
		for (let i = 0; i < 4; i++) {
			const k = new K(1, 5, 7)
			out.push([Array.from(k.args).join(), k.b, k.rest.join(), k.n, k.after].join(';'))
			const a = new Arr(2, 3)
			out.push([a.join(), a.length, a.tail, a instanceof Arr].join(';'))
			const m = new M('k', 'v')
			out.push([m.get('k'), m.size, m.extra].join(';'))
			const e = new E('m')
			out.push([e.message, e.cause, e.more, e instanceof E].join(';'))
			const nn = new Nested(3)
			out.push([Array.from(nn.args).join(), nn.b].join(';'))
		}
		out.slice(0, 5).join(' | ') + ' | same ' + out.slice(5).every((s, i) => s === out[i % 5])`
	const want = "2,10,14,w;10;14,w;4;6,3,20,35,q,r | 2,3,5;3;12;true | v;1;3 | m!;m;15;true | 3,3,3;3 | same true"
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
			t.Errorf("tree %v: got %s, want %s", tree, got, want)
		}
	}
}
