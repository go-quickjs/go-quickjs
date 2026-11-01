package vm

import "testing"

// A callback a built-in calls for each element is called by callDirect, and
// an arrow, or a function whose frame may end in a tail call, is run there
// on its tree (treeRest), as a tree's call node runs one: what the arrow
// captured -- its this, its new.target, the bindings it writes -- what it
// throws, and a recursion through tail calls under a built-in are as they
// are when it is run by runFD. Each callback has a local of its own, so
// that it is no pure body and is run on its tree, not by pureCall. Map's
// and Set's forEach, Array.from's mapping, replace's function and JSON's
// replacer and reviver call theirs as the array methods do.
func TestCallbackTreeRest(t *testing.T) {
	const src = `
		var out = [], last
		function sums(a) {
			var s = 0, seen = 0
			a.forEach((x) => { let _k = 0; s += x + _k })
			var f = a.find((x) => { let _k = 0; seen += x; return x === 3 + _k })
			var e = a.every((x) => { let _k = 1; seen += _k; return x < 10 })
			var r = a.reduce((acc, x) => { let _k = 0; seen++; return acc + x * 2 + _k }, 0)
			var m = a.map((x) => { let _k = 0; seen++; return x * 3 + _k })
			return [s, f, e, r, m.join(','), seen].join(' ')
		}
		for (var i = 0; i < 3; i++) out.push(sums([1, 2, 3, 4]))
		out.push(sums(new Int16Array([1, 2, 3, 4])))
		function keyed() {
			var s = '', n = 0
			new Map([['a', 1], ['b', 2]]).forEach((v, k) => { let _k = ''; s += k + v + _k })
			new Set([5, 6]).forEach((v, k, set) => { let _k = 0; n += v + k + set.size + _k })
			var from = Array.from([1, 2, 3], (x, i) => { let _k = 0; return x * 10 + i + _k })
			var rep = 'a-b-c'.replace(/-/g, (m) => { let _k = '+'; return _k + m })
			var json = JSON.stringify({ a: 1, b: [2] }, (k, v) => { let _k = 0; return typeof v === 'number' ? v + 100 + _k : v })
			var back = JSON.parse('{"a":1,"b":2}', (k, v) => { let _k = 0; return typeof v === 'number' ? v * 2 + _k : v })
			return [s, n, from.join(','), rep, json, back.a + back.b].join(' ')
		}
		for (var i = 0; i < 2; i++) out.push(keyed())
		class Tally {
			constructor() { this.sum = 0 }
			add(a) { a.forEach((x) => { let _k = 0; this.sum += x + _k }); return this.sum }
		}
		var t = new Tally()
		t.add([1, 2]); out.push(t.add(new Float64Array([0.5, 0.25])))
		function Made() { this.seen = []; [1, 2].forEach((x) => { let _k = 0; this.seen.push(typeof new.target + x + _k) }) }
		out.push(new Made().seen.join(','))
		function thrower(a) {
			try {
				a.forEach((x) => { let _k = 0; if (x === 2 + _k) throw new RangeError('at ' + x) })
			} catch (e) {
				return e.name + ' ' + e.message
			}
			return 'none'
		}
		out.push(thrower([1, 2, 3]), thrower(new Map([[1, 2]])), thrower([1, 2, 3]))
		var down = (n) => { 'use strict'; let _k = 0; return n === 0 ? 'bottom' + _k : down(n - 1) }
		out.push([3, 100000].map(down).join(','))
		function Outer() { this.v = 'outer'; this.f = () => { let _k = 0; return this.v + _k } }
		var o = new Outer()
		out.push([1, 2].map(o.f).join(','))
		last = (x) => { let _k = 0; return x + _k }
		;[1, 2].map(last)
		out.join(' | ')`
	// QuickJS and Node give the same, but for the recursion 100,000 deep,
	// which neither makes in tail position: at 1,000 deep they agree.
	const want = "10 3 true 20 3,6,9,12 18 | 10 3 true 20 3,6,9,12 18 | 10 3 true 20 3,6,9,12 18 | " +
		"10 3 true 20 3,6,9,12 18 | a1b2 26 10,21,32 a+-b+-c {\"a\":101,\"b\":[102]} 6 | " +
		"a1b2 26 10,21,32 a+-b+-c {\"a\":101,\"b\":[102]} 6 | 3.75 | function10,function20 | " +
		"RangeError at 2 | RangeError at 2 | RangeError at 2 | bottom0,bottom0 | outer0,outer0"
	for _, tree := range []bool{true, false} {
		previous := treeTier.Swap(tree)
		r := New(Config{})
		v, err := r.Run(compileForTest(t, src))
		var planned bool
		if err == nil {
			g, gerr := r.Run(compileForTest(t, `last`))
			if gerr != nil {
				t.Fatal(gerr)
			}
			planned = g.Object().fn().treeRest
		}
		r.Close()
		treeTier.Store(previous)
		if err != nil {
			t.Fatalf("tree %v: %v", tree, err)
		}
		if got := v.String().Go(); got != want {
			t.Errorf("tree %v:\n%s\nwant\n%s", tree, got, want)
		}
		if tree && !planned {
			t.Errorf("the arrow a built-in called is not one callDirect runs on its tree (treeRest)")
		}
	}
}
