package vm

import "testing"

// Calls in tail position of another function past tailCallDepth, made in
// the frame that gives itself up (tailSwap), answer as Node does, which
// makes no tail call: two functions calling each other, one with more
// locals and a deeper operand stack than the other, closures made at each
// call keeping their own bindings, methods of two objects and their this,
// fewer arguments than parameters and more; callees not made in the frame
// -- a sloppy function, a bound one, a built-in, an arrow, a generator, a
// class called without new, a default, a rest parameter, the arguments
// object -- at the chain's end; an exception from deep in a chain caught
// beneath it, and a chain after it; a finally beneath a chain; a getter and
// a valueOf in a chain calling functions of their own. On the trees and in
// the interpreter.
func TestTailSwapCalls(t *testing.T) {
	src := `
		var t1
		function sloppyF(n) { return n === 0 ? 'sloppy' : t1(n - 1) }
		var s = (function () {
		'use strict'
		const out = []
		function even(n) { return n === 0 ? true : odd(n - 1) }
		function odd(n) { return n === 0 ? false : even(n - 1) }
		out.push(even(2000), odd(2001), even(2001))
		// more locals and a deeper operand stack on one side, none on the other
		function wide(n, acc) { let a = n * 2, b = a + 1, c = [a, b, acc]; if (n === 0) return c.join(':'); return narrow(n - 1, (acc + a + b) | 0) }
		function narrow(n, acc) { return n === 0 ? 'n' + acc : wide(n - 1, acc + 1) }
		out.push(wide(1500, 0), narrow(1501, 7))
		// closures made at each level keep their own bindings
		const fs = []
		function mk1(n) { const k = n * 10; fs.push(() => k + n); if (n === 0) return fs.length; return mk2(n - 1) }
		function mk2(n) { let j = n; fs.push(() => j); j++; if (n === 0) return fs.length; return mk1(n - 1) }
		out.push(mk1(100), fs.slice(0, 6).map(f => f()).join(','), fs.slice(-4).map(f => f()).join(','))
		// this, methods, missing arguments, extra arguments
		const p = { name: 'p', go(n, x) { return n === 0 ? this.name + x : q.go(n - 1) } }
		const q = { name: 'q', go(n, x, y) { return n === 0 ? this.name + x + y : p.go(n - 1, n, 1, 2, 3) } }
		out.push(p.go(1000), p.go(1001), q.go(999, 'a', 'b'))
		// callees not made in the frame: sloppy, bound, native, arrow, generator, class, defaults, rest, arguments
		t1 = function (n) { return n === 0 ? 'done' : sloppyF(n - 1) }
		const bnd = function (n) { return n === 0 ? this.v : tb(n - 1) }.bind({ v: 'bound' })
		function tb(n) { return n === 0 ? 'tb' : bnd(n - 1) }
		function tn(n) { return n === 0 ? 'tn' : tn2(n - 1) }
		function tn2(n) { return n === 0 ? Math.max(n, 5) : tn(n - 1) }
		const holder = { name: 'holder', mk() { return (n) => n === 0 ? 'arrow' + this.name : ta(n - 1) } }
		const arrow = holder.mk()
		function ta(n) { return n === 0 ? 'ta' : arrow(n - 1) }
		function* gen(n) { yield n }
		function tg(n) { return n === 0 ? gen(7).next().value : tg2(n - 1) }
		function tg2(n) { return n === 0 ? [...gen(n)].length : tg(n - 1) }
		function td(n, k = 1) { return n === 0 ? 'td' + k : te(n - 1) }
		function te(n) { return n === 0 ? 'te' : td(n - 1) }
		function tr(n, ...rest) { return n === 0 ? 'tr' + rest.length : ts(n - 1) }
		function ts(n) { return n === 0 ? 'ts' : tr(n - 1, 1, 2) }
		function tu(n) { return n === 0 ? 'tu' + arguments.length : tv(n - 1) }
		function tv(n) { return n === 0 ? 'tv' : tu(n - 1, 9) }
		class K { constructor() {} }
		function tk(n) { return n === 0 ? K() : tk(n - 1) }
		out.push(t1(1000), t1(1001), tb(1000), tb(1001), tn(1000), tn(1001), ta(1000), ta(1001), tg(1000), tg(1001), td(1000), td(1001), tr(1000), tr(1001), tu(1000), tu(1001))
		try { tk(1000) } catch (e) { out.push(e.constructor.name) }
		// an exception from deep in the chain, caught below it, and the chain again
		function boom(n) { if (n === 0) throw new RangeError('deep'); return boom2(n - 1) }
		function boom2(n) { return boom(n - 1) }
		for (let i = 0; i < 3; i++) {
		  try { boom(1000) } catch (e) { out.push(e.message) }
		  out.push(even(1000))
		}
		// a finally below the chain runs
		function fin(n) { try { return even(n) } finally { out.push('fin') } }
		out.push(fin(1000))
		// a getter and a valueOf in the chain's operands call functions of their own
		const obj = { get v() { return even(100) } }
		function gv(n) { return n === 0 ? obj.v : gw(n - 1) }
		function gw(n) { return n === 0 ? ` + "`" + `${{ valueOf() { return odd(51) } } * 1}` + "`" + ` : gv(n - 1) }
		out.push(gv(1000), gw(1000))
		return out.join(',')
		})()
		s`
	const want = "true,true,false,0:1:2254500,0:1:2254508,101,1100,100,1078,98,1056,96,4,22,2,0,p1,qundefinedundefined,p1,done,sloppy,tb,bound,tn,5,ta,arrowholder,7,1,td1,te,tr2,ts,tu2,tv,TypeError,deep,true,deep,true,deep,true,fin,true,true,1"
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

// TestTailSwapChains counts what tailSwap does: a chain of calls in tail
// position past tailCallDepth between functions it makes in the frame is
// one call of it, whose loop makes them all, every return in tail position
// under it leaving its call to it -- it is entered no deeper than twice --
// and a chain a million calls long runs in it; a callee it does not make
// (a sloppy function) is made the ordinary way, and the interpreter leaves
// every such call to its trampoline.
func TestTailSwapChains(t *testing.T) {
	old := tailSwap
	defer func() { tailSwap = old }()
	calls, depth, deepest := 0, 0, 0
	tailSwap = func(c *tctx) {
		calls++
		depth++
		deepest = max(deepest, depth)
		old(c)
		depth--
	}
	const src = `
		var r = (function () {
			'use strict'
			function even(n) { return n === 0 ? 'even' : odd(n - 1) }
			function odd(n) { return n === 0 ? 'odd' : even(n - 1) }
			return even(1000) + odd(1e6)
		})()
		function sloppy(n) { return n === 0 ? 's' : sloppy2(n - 1) }
		function sloppy2(n) { return n === 0 ? 't' : sloppy(n - 1) }
		r + sloppy(1000)`
	for _, tree := range []bool{true, false} {
		SetTreeTier(tree)
		calls, deepest = 0, 0
		r := New(Config{})
		v, err := r.Run(compileForTest(t, src))
		r.Close()
		if err != nil {
			t.Fatal(err)
		}
		if got := v.String().Go(); got != "evenodds" {
			t.Errorf("tree tier %v: got %s, want evenodds", tree, got)
		}
		// Every call made once the frames -- the script's, the function's
		// around the chains, and the chain's -- are tailCallDepth deep, in
		// each chain: one call of it, and one more for each call under it.
		want, wantDeep := 1000+1000000+2-2*(tailCallDepth-2), 2
		if !tree {
			want, wantDeep = 0, 0
		}
		if calls != want || deepest != wantDeep {
			t.Errorf("tree tier %v: tailSwap called %d times, %d deep; want %d, %d deep", tree, calls, deepest, want, wantDeep)
		}
	}
	SetTreeTier(true)
}
