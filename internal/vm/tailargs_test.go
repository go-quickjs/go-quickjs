package vm

import "testing"

// A call in tail position past tailCallDepth copies its arguments out of
// its frame, which its callee's reuses: for a strict callee with no
// arguments object and no rest parameter, into the runtime's tailArgs,
// the next such call's too (tailArgsOf); for any other, into a copy of its
// own. Recursions through tail calls thousands deep answer as Node does --
// one alone, with arguments and with a rest parameter, two by turns, one
// through a bound function, one that rotates five arguments and one that
// passes fewer than it takes, one whose frames call a function deep in
// tail calls of its own between theirs, and one that reads arguments
// after such a call -- on the trees and in the interpreter, three rounds
// alike.
func TestTailCallArguments(t *testing.T) {
	const src = `
		"use strict"
		const r = []
		function deep(n, acc) { let _k = 0; if (n === 0) return acc; return deep(n - 1, (acc + n) | 0) }
		function viaArgs(n, acc) { let _k = 0; if (n === 0) return acc + ':' + arguments.length + ':' + arguments[2]; return viaArgs(n - 1, acc + 1, 'extra' + n) }
		function viaRest(n, ...more) { let _k = 0; if (n === 0) return more.join('/'); return viaRest(n - 1, n, n * 2) }
		function inner(n, acc) { let _k = 0; if (n === 0) return acc; return inner(n - 1, acc + 1) }
		function outer(n, a, b) {
			let _k = 0
			if (n === 0) return a + ',' + b
			const x = inner(100, 0)
			return outer(n - 1, a + x, b + n)
		}
		function argsAfter(n, x) { let _k = 0; if (n === 0) { inner(100, 0); return arguments[1] + ':' + x } return argsAfter(n - 1, x + 1) }
		function even(n) { let _k = 0; return n === 0 ? 'even' : odd(n - 1) }
		function odd(n) { let _k = 0; return n === 0 ? 'odd' : even(n - 1) }
		const bound = function (k, n, acc) { let _k = 0; if (n === 0) return acc + k; return bound2(n - 1, acc + 1) }
		const bound2 = bound.bind(null, 7)
		function wide(n, a, b, c, d, e) { let _k = 0; if (n === 0) return [a, b, c, d, e].join(); return wide(n - 1, b, c, d, e, a) }
		function short(n, a, b) { let _k = 0; if (n === 0) return String(a) + String(b); return short(n - 1, a) }
		for (let round = 0; round < 3; round++) {
			const s = []
			s.push(deep(5000, 0), viaArgs(200, 0), viaRest(100), outer(60, 0, 0), even(1001), odd(1001), bound2(300, 0))
			s.push(wide(203, 1, 2, 3, 4, 5), short(100, 'a', 'b'), argsAfter(100, 0))
			r.push(s.join(' '))
		}
		r[0] + ' | ' + (r[0] === r[1] && r[1] === r[2])`
	const want = "12502500 200:3:extra1 1/2 6000,1830 odd even 307 4,5,1,2,3 aundefined 100:100 | true"
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
