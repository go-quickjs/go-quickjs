package vm

import (
	"testing"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
)

// x && y and x || y as values, built as one node where y is an
// expression nothing else jumps into (logicalJumps, logicalNode), answer as
// the interpreter and Node do: x's own value where it decides, of each kind
// of value there is to test -- 0, -0, NaN, ”, null, 0n and their truthy
// fellows; chains, an || inside an && and the other way round, a getter in
// y that runs only where x does not decide and one that throws only then,
// comparisons, a ternary whose else is an && the then's jump ends at too,
// which stays blocks, and this read in a derived constructor before super().
// On the trees -- each function is one, declaring a local, which keeps it
// from being a pure body pureCall would evaluate instead -- and in the
// interpreter, three rounds alike.
func TestLogicalExpressions(t *testing.T) {
	const src = `
		const r = []
		let log = ''
		const o = { get a() { log += 'a'; return this.av }, get b() { log += 'b'; return this.bv }, get t() { log += 't'; throw new Error('thrown') }, av: 0, bv: 0, arr: [0, 1, '', 'x', null, NaN, -0, 2n, 0n] }
		function and(o, i, j) { let _k = 0; return o.arr[i] && o.arr[j] }
		function or(o, i, j) { let _k = 0; return o.arr[i] || o.arr[j] }
		function chain(o, i) { let _k = 0; return o.arr[i] && o.arr[i + 1] && o.arr[i + 2] }
		function mix(o, i) { let _k = 0; return (o.arr[i] || o.arr[i + 1]) && (o.arr[i + 2] || o.arr[i + 3]) }
		function nest(o, i) { let _k = 0; return o.arr[i] && (o.arr[i + 1] || o.arr[i + 2]) }
		function gets(o) { let _k = 0; return o.a && o.b }
		function getsOr(o) { let _k = 0; return o.a || o.b }
		function throws(o) { let _k = 0; return o.av && o.t }
		function tern(o, c, i, j) { let _k = 0; return c ? o.arr[i] : o.arr[i] && o.arr[j] }
		function cmp(x, y) { let _k = 0; return x < y && y < 10 || x === y }
		for (let round = 0; round < 3; round++) {
			const s = []
			for (let i = 0; i < 9; i++) for (let j = 0; j < 9; j++) s.push(String(and(o, i, j)) + '/' + String(or(o, i, j)))
			for (let i = 0; i < 7; i++) s.push(String(chain(o, i)), String(mix(o, i % 5)), String(nest(o, i)))
			log = ''
			o.av = 0; o.bv = 5; s.push(gets(o), getsOr(o), log)
			log = ''
			o.av = 1; s.push(gets(o), getsOr(o), log)
			o.av = 0
			s.push(String(throws(o)))
			o.av = 1
			try { throws(o) } catch (e) { s.push(e.message + log) }
			for (const [x, y] of [[1, 2], [3, 3], [11, 12], [2, 1]]) s.push(cmp(x, y))
			for (let i = 0; i < 8; i++) s.push(String(tern(o, i & 1, i, i + 1)))
			r.push(s.join(','))
		}
		class B { constructor(v) { this.v = v } }
		class D extends B { constructor(k) { const early = k && this; super(1); r.push(String(early)) } }
		try { new D(0) } catch (e) { r.push(e.name) }
		try { new D(1) } catch (e) { r.push(e.name) }
		r[0] + ' | ' + (r[0] === r[1] && r[1] === r[2]) + ' | ' + r.slice(3).join(',')`
	const want = "0/0,0/1,0/,0/x,0/null,0/NaN,0/0,0/2,0/0,0/1,1/1,/1,x/1,null/1,NaN/1,0/1,2/1,0/1,/0,/1,/,/x,/null,/NaN,/0,/2,/0,0/x,1/x,/x,x/x,null/x,NaN/x,0/x,2/x,0/x,null/0,null/1,null/,null/x,null/null,null/NaN,null/0,null/2,null/0,NaN/0,NaN/1,NaN/,NaN/x,NaN/null,NaN/NaN,NaN/0,NaN/2,NaN/0,0/0,0/1,0/,0/x,0/null,0/NaN,0/0,0/2,0/0,0/2,1/2,/2,x/2,null/2,NaN/2,0/2,2/2,0/2,0/0,0/1,0/,0/x,0/null,0/NaN,0/0,0/2,0/0,0,x,0,,x,x,,NaN,,null,0,NaN,null,NaN,null,NaN,x,NaN,0,x,0,0,5,aab,5,1,aba,0,thrownabat,true,true,false,false,0,1,,x,null,NaN,0,2 | true | 0,ReferenceError"
	// Each function is a tree: one the tier gave up on would answer as
	// the interpreter does, and say nothing of the tier.
	var walk func(fn *bytecode.Function)
	walk = func(fn *bytecode.Function) {
		for _, c := range fn.Constants {
			if c.Kind == bytecode.ConstFunction && c.Fn != nil {
				if c.Fn.Name != "" && buildTree(c.Fn) == nil {
					t.Errorf("%s is not built as a tree", c.Fn.Name)
				}
				walk(c.Fn)
			}
		}
	}
	walk(compileForTest(t, src))
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
