package vm

import (
	"testing"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
)

// An && or || whose y calls, and x ?? y, are one node too, and answer as
// the interpreter and Node do: method calls on a local and on this that
// run only where x does not decide, a property's property, a construction,
// ?? of each kind of value there is, chained, in an || and in a ternary.
// A y that spills -- the receiver of Math.max, a global's property, or of
// a getter that logs -- is found as it is built, and the function built
// again with that jump ending a block (tbuilder.badLogic): the getter runs
// only where x does not decide, as it must. Every function is a tree, one
// node and a return but those two.
func TestLogicalCallsAndNullish(t *testing.T) {
	const src = `
		const r = []
		let log = ''
		class C {
			constructor(v) { this.v = v; this.inner = { w: v } }
			f(x) { log += 'f' + x; return this.v > x }
			g(x) { log += 'g' + x; return this.v + x }
			get lg() { log += 'L'; return { m(v) { return v + 1 } } }
		}
		const vals = [0, 1, '', 'x', null, undefined, NaN, false, -0, 0n, 2n]
		function both(o, x) { let _k = 0; return o.f(x) && o.g(x) }
		function either(o, x) { let _k = 0; return o.f(x) || o.g(x) }
		function viaThis(o, x) { let _k = 0; return o.v && o.g(x) }
		function deep(o, x) { let _k = 0; return x && o.inner.w + x }
		function deepCall(o, x) { let _k = 0; return x && Math.max(o.v, x) }
		function deepGet(o, x) { let _k = 0; return x && o.lg.m(x) }
		function made(x) { let _k = 0; return x && new C(x).v }
		function nq(a, b) { let _k = 0; return a ?? b }
		function nqCall(o, x) { let _k = 0; return o.missing ?? o.g(x) }
		function nqChain(a, b, c) { let _k = 0; return a ?? b ?? c }
		function nqMix(a, b, c) { let _k = 0; return a ?? (b || c) }
		function nqTern(a, b, c) { let _k = 0; return a ? b ?? c : c ?? b }
		for (let round = 0; round < 3; round++) {
			const s = []
			log = ''
			for (let i = 0; i < 4; i++) {
				const o = new C(i)
				s.push(both(o, 1), either(o, 2), viaThis(o, 3), String(deep(o, i)), String(deepCall(o, i)), String(deepGet(o, i)), String(made(i)))
			}
			s.push(log)
			for (const a of vals) for (const b of [7, null]) s.push(String(nq(a, b)))
			log = ''
			s.push(nqCall(new C(5), 1), log)
			s.push(String(nqChain(null, undefined, 3)), String(nqChain(0, null, 3)), String(nqChain(undefined, '', 3)))
			s.push(String(nqMix(null, 0, 'c')), String(nqMix(undefined, 'b', 'c')), String(nqMix(false, 'b', 'c')))
			s.push(String(nqTern(1, null, 9)), String(nqTern(0, null, undefined)), String(nqTern(1, 0, 9)))
			r.push(s.join(','))
		}
		r[0] + ' | ' + (r[0] === r[1] && r[1] === r[2])`
	const want = "false,2,0,0,0,0,0,false,3,4,2,1,2,1,3,4,5,4,2,3,2,4,true,6,6,3,4,3,f1f2g2f1f2g2g3Lf1g1f2g2g3Lf1g1f2g3L,0,0,1,1,,,x,x,7,null,7,null,NaN,NaN,false,false,0,0,0,0,2,2,6,g1,3,0,,c,b,false,9,null,0 | true"
	for _, c := range compileForTest(t, src).Constants {
		if c.Kind != bytecode.ConstFunction || c.Fn == nil || c.Fn.Name == "" {
			continue
		}
		tr := buildTree(c.Fn)
		switch {
		case tr == nil:
			t.Errorf("%s is not built as a tree", c.Fn.Name)
		case c.Fn.Name == "deepCall" || c.Fn.Name == "deepGet":
			if len(tr.blocks) <= 2 {
				t.Errorf("%s is one node, its y spilling", c.Fn.Name)
			}
		case len(tr.blocks) != 2:
			t.Errorf("%s is %d blocks, not one node and a return", c.Fn.Name, len(tr.blocks))
		}
	}
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
