//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))

package vm

import (
	"runtime"
	"runtime/debug"
	"testing"
)

// arrowThisSetup's methods each make an arrow that reads this and give it
// to a method with a loop that calls it, SOM's Vector.forEach (AWFY's
// Havlak: findSet's nodeList.forEach((iter) => iter.union(this.parent))),
// or call it themselves: the arrow's this is the method's own, which the
// inlined arrow reads where the loop's call would have given a receiver
// (ssa.InlineSite's LexThis), and the arrow is not made. linkAll's arrow
// calls a method of what it is given; the arrow, which the loop's native
// code calls through its call path, has no code of its own, and the call
// is inlined as the existing tiers met it (jitCalleeInline). Then each meets
// what leaves native code inside the arrow -- a shape the code was not
// compiled for, a throw, a stack trace taken -- where Go makes the arrow on
// the method's frame, its this the method's. plain is no method: its
// arrow's this is plain's own, undefined made the global object, which it
// reads where the method's code does not read its this itself (the
// lowering's makesArrowThis). A derived constructor's arrow, made where
// its this may not be bound yet, is made and called as before.
const arrowThisSetup = `
	class Vec {
		constructor(n) { this.storage = []; this.firstIdx = 0; this.lastIdx = 0; for (let i = 0; i < n; i++) this.append(i) }
		append(e) { this.storage[this.lastIdx] = e; this.lastIdx += 1 }
		forEach(fn) { for (let i = this.firstIdx; i < this.lastIdx; i += 1) { fn(this.storage[i]) } }
	}
	class Node { constructor(v) { this.v = v; this.parent = null } union(p) { this.parent = p } }
	var vec = new Vec(12), objs = new Vec(0), nodes = new Vec(0)
	for (let i = 0; i < 9; i++) { objs.append({ v: i }); nodes.append(new Node(i)) }
	var odd = { w: 0, v: -50 }
	class Walker {
		constructor(k) { this.k = k; this.seen = 0; this.base = new Node(-k) }
		sumOf(v) { let s = 0; v.forEach((x) => { s += x * this.k }); return s }
		countOf(v) { this.seen = 0; v.forEach((x) => { if (x > this.k) this.seen++ }); return this.seen }
		linkAll(v) { v.forEach((n) => n.union(this.base)); return v.storage[3].parent === this.base && this.base.v }
		shapesOf(v, at) { let s = 0; v.forEach((o) => { s += (o.v === at ? odd : o).v + this.k }); return s }
		throwsAt(v, at) {
			let s = 0
			try { v.forEach((x) => { s += x; if (x === at) throw new RangeError('at ' + x + ':' + this.k) }) } catch (e) { return e.message }
			return s
		}
		stackAt(v, at) { let st = 'none'; v.forEach((x) => { if (x === at) st = this.k + new Error('h').stack.split('\n').length }); return st }
		directOf(x) { const f = (y) => y * this.k; return f(x) + f(x + 1) }
	}
	class Sub extends Walker { constructor(k, v) { let s = 0; super(k); v.forEach((x) => { s += x * this.k }); this.r = s } }
	function plain(v, k) { let s = 0; v.forEach((x) => { s += x * k + (this === undefined ? 1 : 2) }); return s }
	var w3 = new Walker(3), w5 = new Walker(5)
	function drive(n, w) { let r = 0; for (let i = 0; i < n; i++) r += w.sumOf(vec) + w.countOf(vec) + w.directOf(i); return r }
`

// arrowThisRounds drives arrowThisSetup's methods, the rounds with exits
// inside the arrows last when exits is set.
func arrowThisRounds(exits bool) []string {
	at := "99"
	if exits {
		at = "6"
	}
	var rounds []string
	for _, w := range []string{"w3", "w5", "w3"} {
		for _, call := range []string{
			w + ".sumOf(vec)", w + ".countOf(vec)", w + ".linkAll(nodes)", w + ".shapesOf(objs, " + at + ")",
			w + ".throwsAt(vec, " + at + ")", w + ".stackAt(vec, " + at + ")", w + ".directOf(i)",
			"new Sub(i % 4, vec).r", "plain(vec, i)",
		} {
			rounds = append(rounds, "(() => { let r; for (let i = 0; i < 150; i++) r = "+call+"; return r })()")
		}
	}
	return rounds
}

// An arrow that reads this, made by a method and given to a method's loop
// or called by its maker, is inlined there, its this the maker's, and not
// made; every answer is the interpreter's, whatever leaves native code
// inside it, under budgets of polls and of finished invocations or not.
func TestJITArrowThisInlined(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	r := jitAgreesForTestWith(t, jitStressConfig{}, arrowThisSetup, arrowThisRounds(false)...)
	for _, c := range []struct {
		name            string
		inlined, unmade int
	}{
		{"sumOf", 1, 1}, {"throwsAt", 1, 1}, {"plain", 1, 1},
		// The arrow's call of union, a method of what it is given: the
		// arrow has no code of its own.
		{"linkAll", 1, 1},
		// Called by its maker, twice.
		{"directOf", 0, 1},
	} {
		e := jitCallbackEntry(r, c.name)
		if e == nil || e.ssa == nil {
			t.Errorf("%s was not compiled", c.name)
			continue
		}
		if len(e.ssaInlinedAt) != c.inlined || e.ssaCalled != c.unmade {
			t.Errorf("%s inlines %d calls, %d closures not made; want %d, %d", c.name, len(e.ssaInlinedAt), e.ssaCalled, c.inlined, c.unmade)
		}
	}
	r = jitAgreesForTestWith(t, jitStressConfig{}, arrowThisSetup, arrowThisRounds(true)...)
	if r.jit.materialized == 0 {
		t.Error("Go made no closure the code did not make")
	}
	for _, c := range []jitStressConfig{{threshold: true, budget: 3}, {threshold: true, budget: 3, deopt: 7}, {threshold: true, budget: 1, deopt: 2}} {
		jitAgreesForTestWith(t, c, arrowThisSetup, arrowThisRounds(true)...)
	}
}

// An arrow inlined so is no allocation; and the collector, run at every
// call of Go, and marking while native code runs, finds what the frames
// hold.
func TestJITArrowThisAllocate(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	runtime.GC()
	defer debug.SetGCPercent(debug.SetGCPercent(-1))
	r := jitAgreesForTestWith(t, jitStressConfig{}, arrowThisSetup, `drive(50, w3)`, `drive(50, w3)`, `drive(50, w3)`)
	jitCallbackMallocs(t, r, `drive(5000, w3)`)
	n := jitCallbackMallocs(t, r, `drive(5000, w3)`)
	t.Logf("drive(5000): %d allocations", n)
	if n > 200 {
		t.Errorf("drive(5000): %d allocations", n)
	}
	calls := 0
	jitGoCallWork = func() {
		if calls++; calls%3 == 0 {
			runtime.GC()
		}
	}
	defer func() { jitGoCallWork = nil }()
	jitAgreesForTestWith(t, jitStressConfig{threshold: true, budget: 2, deopt: 3}, arrowThisSetup, arrowThisRounds(true)...)
	if calls == 0 {
		t.Error("no call of Go")
	}
}
