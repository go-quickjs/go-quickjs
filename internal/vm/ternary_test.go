package vm

import (
	"testing"
)

// c ? a : b as a value answers as Node does: a plain test of each kind of
// value, a comparison the test's jump makes -- of numbers, strings, mixed,
// NaN, null and undefined --, ternaries in a and in b, in an && and holding
// one, a getter in the branch taken only, and one that throws only where it
// is taken.
func TestTernaryExpressions(t *testing.T) {
	t.Parallel()
	const src = `
		const r = []
		let log = ''
		const o = { get a() { log += 'a'; return 1 }, get b() { log += 'b'; return 2 }, get t() { log += 't'; throw new Error('thrown') }, vals: [0, 1, '', 'x', null, undefined, NaN, -0, 2n, 0n, {}, []] }
		function t1(o, i) { let _k = 0; return o.vals[i] ? 'T' : 'F' }
		function lt(x, y) { let _k = 0; return x < y ? x : y }
		function ge(x, y) { let _k = 0; return x >= y ? 'ge' : 'lt' }
		function seq(x, y) { let _k = 0; return x === y ? 1 : x !== y + 1 ? 2 : 3 }
		function nestThen(x, y) { let _k = 0; return x ? (y ? 'xy' : 'x') : 'none' }
		function nestElse(x, y) { let _k = 0; return x ? 'x' : y ? 'y' : 'none' }
		function inAnd(x, y, z) { let _k = 0; return x && (y ? z : -z) }
		function andIn(x, y, z) { let _k = 0; return x ? y && z : y || z }
		function gets(o, c) { let _k = 0; return c ? o.a : o.b }
		function throws(o, c) { let _k = 0; return c ? o.t : o.a }
		function call(o, c) { let _k = 0; return c ? Math.max(o.vals.length, 3) : o.vals.length }
		for (let round = 0; round < 3; round++) {
			const s = []
			for (let i = 0; i < o.vals.length; i++) s.push(t1(o, i))
			for (const [x, y] of [[1, 2], [2, 1], [3, 3], ['2', 10], ['a', 'b'], [NaN, 1], [1, NaN], [null, 0], [undefined, 0]]) s.push(String(lt(x, y)), ge(x, y), seq(x, y))
			for (let i = 0; i < 4; i++) s.push(nestThen(i & 1, i & 2), nestElse(i & 1, i & 2), inAnd(i & 1, i & 2, i + 1), String(andIn(i & 1, i & 2, i & 3)))
			log = ''
			s.push(gets(o, 1), gets(o, 0), log)
			log = ''
			s.push(throws(o, 0) + log)
			try { throws(o, 1) } catch (e) { s.push(e.message + log) }
			s.push(call(o, 1), call(o, 0))
			r.push(s.join(','))
		}
		r[0] + ' | ' + (r[0] === r[1] && r[1] === r[2])`
	const want = "F,T,F,T,F,F,F,F,T,F,T,T,1,lt,2,1,ge,3,3,ge,1,2,lt,2,a,lt,2,1,lt,2,NaN,lt,2,0,ge,2,0,lt,2,none,none,0,0,x,x,-2,0,none,y,0,2,xy,x,4,3,1,2,ab,1a,thrownat,12,12 | true"
	r := New(Config{})
	v, err := r.Run(compileForTest(t, src))
	r.Close()
	if err != nil {
		t.Fatal(err)
	}
	if got := v.String().Go(); got != want {
		t.Errorf("got\n%s\nwant\n%s", got, want)
	}
}
