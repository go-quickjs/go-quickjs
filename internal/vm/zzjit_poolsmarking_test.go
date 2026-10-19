//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))

package vm

import "testing"

// poolsSetup takes from every kind of pool native code takes from, and
// uses what it takes: closures capturing parameters, locals and a binding
// of their maker's, an arrow's this, an entry's cells, object and array
// literals, new Array(n), constructions, and a push's and a pop's
// results.
const poolsSetup = `
	var objs = [{v:1}, {v:2}, {v:3}, {v:4}]
	function mk(a, b) { let c = a * 2; const f = () => a + b + c; c += 1; return f }
	function addr(a, b) { return function () { return a * 3 + b } }
	function P(x) { this.x = x; for (let i = 0; i < 2; i++) this.x = (this.x + i) | 0 }
	class K { constructor(v) { this.v = v } arrow(k) { const a = k; return () => this.v * 10 + a } }
	var ks = [new K(1), new K(2), new K(3)]
	function bump(a) { let x = a; const g = () => x++; g(); return x + g() }
	function hold(o, p) { let q = p; const f = () => o.v * 10 + q.v; q = o; return f() + f.call(null) }
	function pools(n) {
		let s = 0
		const st = []
		for (let i = 0; i < n; i++) {
			s = (s + mk(i, 3)() + addr(i, 7)() + ks[i % 3].arrow(i)() + bump(i) + hold(objs[i & 3], objs[(i + 2) & 3])) | 0
			const o = { a: i, b: objs[i & 3] }, l = [i, o, 's', objs[(i + 1) & 3]], z = new Array(3)
			z[1] = o
			const p = new P(i)
			s = (s + o.a + o.b.v + l[0] + l[1].b.v + l[3].v + z.length + z[1].a + p.x) | 0
			s = (s + st.push(l[1]) + st.push(objs[i & 3])) | 0
			s = (s + st.pop().v + st.pop().b.v + st.length) | 0
			st.push(i * 2)
			s = (s + st.pop() + st.length) | 0
		}
		return s
	}
`

// While the collector marks, native code takes from its pools and writes
// what it took with Go's write barrier, as V8's code allocates (mir's
// poolTakeMarking, poolShade, closureTakeMarking, cellsTakeMarking, push,
// pop): every register native code may keep is given junk across each
// such store (jit.StoreRefClobber), and every answer is the
// interpreter's. Go had made each while it marked (ssa.GoTake,
// markingCall), and the closures and cells.
func TestJITPoolsWhileMarking(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	jitMarkingForTest(t)
	jitClobberStoresForTest(t)
	rounds := []string{}
	for range 10 {
		rounds = append(rounds, `pools(200)`)
	}
	r := jitAgreesForTest(t, poolsSetup, rounds...)
	jitNativeForTest(t, r, "pools")
	for _, name := range []string{"mk", "bump", "hold"} {
		var e *jitEntry
		for k, x := range r.jit.cache {
			if fn := k.Value(); fn != nil && fn.Name == name {
				e = x
			}
		}
		if e == nil || e.cellsPool == nil || e.cellsPool.Count == e.cellsPool.Size {
			t.Errorf("%s's cells were not taken from their pool while the collector marked: %+v", name, e)
		}
	}
}

// A call in tail position, deep enough in the stack that its frame is given
// up (mir's tailSelf), while the collector marks: its operands, read into
// the context's GoArgs before the frame they came from is written, are each
// shaded first, stored through Go's write barrier (abi.Context's Shade) --
// here objects swapped between parameters and a receiver -- every register
// native code may keep given junk across those stores; each answer is the
// interpreter's. It had left native code at each such call while the
// collector marked.
func TestJITTailCallsWhileMarking(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	jitMarkingForTest(t)
	jitClobberStoresForTest(t)
	setup := `
		"use strict"
		var objs = [{v:1}, {v:2}, {v:3}, {v:4}]
		function swap(n, a, b, acc) { if (n === 0) return acc + a.v * 10 + b.v; return swap(n - 1, b, objs[n & 3], acc + a.v) }
		class L { constructor(v, next) { this.v = v; this.next = next } sum(acc, o) { if (this.next === null) return acc + this.v + o.v; return this.next.sum(acc + this.v, this) } }
		var list = null
		for (let i = 1; i <= 3000; i++) list = new L(i & 7, list)
		function run() { return swap(20000, objs[0], objs[1], 0) + ':' + list.sum(0, objs[2]) }
	`
	rounds := []string{}
	for range 6 {
		rounds = append(rounds, `run()`)
	}
	r := jitAgreesForTest(t, setup, rounds...)
	jitNativeForTest(t, r, "swap")
}
