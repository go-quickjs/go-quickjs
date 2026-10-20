//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))

package vm

import "testing"

// mapStepSetup's loops walk Maps and Sets with for...of, by their keys,
// their values and their entries, of numbers, strings and objects, and
// change them as they go: entries deleted ahead, added, the collection
// cleared and compacted under the iterator, an iterator stepped by hand
// before the loop takes it, a loop left early; then an iterator's next
// replaced, which the loop calls.
const mapStepSetup = `
	var big = new Map(); for (let i = 0; i < 100; i++) big.set(i, i * 2)
	function sumValues(m, n) { let s = 0; for (let j = 0; j < n; j++) for (const v of m.values()) s += v; return s }
	function sumKeys(m, n) { let s = 0; for (let j = 0; j < n; j++) for (const k of m.keys()) s += k; return s }
	function sumSet(st, n) { let s = 0; for (let j = 0; j < n; j++) for (const v of st) s += v; return s }
	function walk(it) { let out = ''; for (const v of it) out += (typeof v === 'object' ? JSON.stringify(v) : v) + ','; return out }
	function kinds() {
		const o = { k: 1 }, m = new Map([['a', 1], [o, 'x'], [2, o]]), st = new Set(['s', o, 3])
		return walk(m.keys()) + '|' + walk(m.values()) + '|' + walk(m) + '|' + walk(st) + '|' + walk(st.keys()) + '|' + walk(st.entries())
	}
	function deleting() {
		const m = new Map(); for (let i = 0; i < 10; i++) m.set(i, i)
		let out = ''; for (const v of m.values()) { out += v + ','; if (v % 3 === 0) { m.delete(v + 1); m.delete(v + 2) } }
		return out + m.size
	}
	function adding() {
		const m = new Map([[0, 0]]); let out = ''
		for (const k of m.keys()) { out += k + ','; if (k < 6) m.set(k + 1, 0) }
		return out
	}
	function clearing() {
		const m = new Map([[1, 1], [2, 2], [3, 3]]); let out = ''
		for (const v of m.values()) { out += v + ','; if (v === 2) { m.clear(); m.set(9, 9) } }
		return out
	}
	function compacting() {
		const m = new Map(); for (let i = 0; i < 64; i++) m.set(i, i)
		let out = 0, seen = 0
		for (const v of m.values()) { seen++; out += v; if (v === 5) for (let i = 6; i < 60; i++) m.delete(i) }
		return seen + ':' + out + ':' + m.size
	}
	function byHand() {
		const m = new Map([[1, 'a'], [2, 'b'], [3, 'c'], [4, 'd']]), it = m.values()
		it.next(); let out = ''
		for (const v of it) { out += v; if (v === 'c') break }
		return out + ':' + JSON.stringify(it.next()) + ':' + JSON.stringify(it.next())
	}
	function done() { const it = new Set([1]).values(); for (const v of it); let n = 0; for (const v of it) n++; return n + ':' + it.next().done }
	function sparse(n) { const m = new Map(); for (let i = 0; i < 20; i++) m.set(i, i); for (let i = 0; i < 20; i += 2) m.delete(i); return sumValues(m, n) }
`

// mapStepRounds are mapStepSetup's calls and their answers, Node's
// (v26.10.0).
var mapStepRounds = [][2]string{
	{`sumValues(big, 30)`, `297000`},
	{`sumKeys(big, 30)`, `148500`},
	{`sumSet(new Set([1, 2, 3, 'x']), 3)`, `6x123x123x`},
	{`kinds()`, `a,{"k":1},2,|1,x,{"k":1},|["a",1],[{"k":1},"x"],[2,{"k":1}],|s,{"k":1},3,|s,{"k":1},3,|["s","s"],[{"k":1},{"k":1}],[3,3],`},
	{`deleting()`, `0,3,6,9,4`},
	{`adding()`, `0,1,2,3,4,5,6,`},
	{`clearing()`, `1,2,9,`},
	{`compacting()`, `10:261:10`},
	{`byHand()`, `bc:{"value":"d","done":false}:{"done":true}`},
	{`done()`, `0:true`},
	{`sparse(30)`, `3000`},
}

// A for...of over a Map's or a Set's iterator whose next is the realm's is
// stepped natively (mir's mapStep), as V8 inlines the iterator's next: each
// answer is Node's and the interpreter's, and sumValues's steps call no Go
// but at each loop's start and end.
func TestJITMapStep(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	var rounds []string
	for range 6 {
		for _, r := range mapStepRounds {
			rounds = append(rounds, r[0])
		}
	}
	r := jitAgreesForTest(t, mapStepSetup, rounds...)
	for _, round := range mapStepRounds {
		v, err := r.Run(compileForTest(t, "''+("+round[0]+")"))
		if err != nil {
			t.Fatalf("%s: %v", round[0], err)
		}
		if got := v.String().Go(); got != round[1] {
			t.Errorf("%s = %s, want %s", round[0], got, round[1])
		}
	}
	// Calls of Go: the loop's values() and its cursor, and its last step, at
	// each of the 30 turns, against the hundred steps of each.
	jitNativeForTest(t, r, "sumValues")
	calls := 0
	jitGoCallWork = func() { calls++ }
	defer func() { jitGoCallWork = nil }()
	if _, err := r.Run(compileForTest(t, `sumValues(big, 30)`)); err != nil {
		t.Fatal(err)
	}
	if calls > 4*30 {
		t.Errorf("sumValues called Go %d times for 30 loops of 100 steps", calls)
	}
}

// A Map's iterator whose next is replaced is stepped by calling it, as the
// interpreter does: the native step checks the iterator's next.
func TestJITMapStepNextReplaced(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	const setup = mapStepSetup + `
		var proto = Object.getPrototypeOf(new Map().values()), original = proto.next
		function replaced(n) {
			const first = sumValues(big, n)
			let calls = 0
			proto.next = function () { calls++; return original.call(this) }
			const second = sumValues(big, n)
			proto.next = original
			return first + ':' + second + ':' + calls + ':' + sumValues(big, n)
		}
	`
	for _, round := range [][2]string{
		// Node's (v26.10.0) answer.
		{`replaced(30)`, `297000:297000:3030:297000`},
	} {
		r := jitAgreesForTest(t, setup, round[0], round[0])
		v, err := r.Run(compileForTest(t, "''+("+round[0]+")"))
		if err != nil {
			t.Fatalf("%s: %v", round[0], err)
		}
		if got := v.String().Go(); got != round[1] {
			t.Errorf("%s = %s, want %s", round[0], got, round[1])
		}
	}
}
