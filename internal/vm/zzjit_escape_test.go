//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))

package vm

import (
	"runtime"
	"runtime/debug"
	"testing"
)

// escapeSetup's functions each make objects that nothing but their own
// code sees -- object literals, the receivers of constructions they inline
// -- which native code does not make (ssa's escape.go): their properties
// are the values stored, read where they are used. Then each meets what
// leaves native code while such an object is live, where Go makes it, of
// the frame state's words and its home (jitMaterialize): a property read
// as a number that is no number, a throw inside an inlined constructor, a
// property added where the code had never added it -- after which probe
// sees the objects as Go made them:
// one object for every slot naming it, the one nested in another the one
// a local holds, written to after Go made it -- and a stack trace taken.
const escapeSetup = `
	function V(x, y, z) { this.x = x; this.y = y; this.z = z }
	V.prototype.minus = function (o) { return new V(this.x - o.x, this.y - o.y, this.z - o.z) }
	V.prototype.dot = function (o) { return this.x * o.x + this.y * o.y + this.z * o.z }
	V.prototype.sq = function () { return this.x * this.x + this.y * this.y + this.z * this.z }
	function T(x) { if (x === 13) throw new RangeError('at ' + x); this.x = x; this.half = x / 2 }
	class C { constructor(a, b) { this.a = a; this.b = b } sum() { return this.a + this.b } }
	var a = new V(1, 2, 3), b = new V(4, 6, 8)
	var odd = 'x'
	function probe(o, p) {
		const same = o === p || o.p === p || o.c === p
		o.extra2 = 'e'
		return same + JSON.stringify(o) + Object.keys(p) + JSON.stringify(p)
	}
	function dist(p, q) { return q.minus(p).sq() }
	function dots(n) { let s = 0; for (let i = 0; i < n; i++) s += a.minus(b).dot(b.minus(a)) + i; return s }
	function literal(n) { let s = 0; for (let i = 0; i < n; i++) { const o = { a: i, b: i * 2, c: 1 }; s += o.a + o.b + o.c } return s }
	function nested(n) { let s = 0; for (let i = 0; i < n; i++) { const o = { p: { q: i }, t: i + 1 }; s += o.p.q + o.t } return s }
	function branches(n) {
		let s = 0
		for (let i = 0; i < n; i++) { const o = { v: 0, w: 1 }; if (i & 1) o.v = i; else o.v = -i; o.w = o.v + 1; s += o.v * o.w }
		return s
	}
	function readsOther(n, at) {
		let s = ''
		for (let i = 0; i < n; i++) { const o = { v: i === at ? odd : i, k: 2 }; s = o.v * o.k + ',' + (s.length > 60 ? '' : s) }
		return s
	}
	function throwsIn(n, at) {
		let s = 0
		for (let i = 0; i < n; i++) { try { const t = new T(i === at ? 13 : i); s += t.x + t.half } catch (e) { s += e.message.length } }
		return s
	}
	function twice(n, at) {
		let r = ''
		for (let i = 0; i < n; i++) {
			const o = { k: i, v: i * 2 }, p = o
			if (i === at) { o.extra = 'x'; r = probe(o, p) + r }
			r = (r.length > 80 ? '' : r) + o.v
		}
		return r
	}
	function holds(n, at) {
		let r = ''
		for (let i = 0; i < n; i++) {
			const inner = { q: i }, o = { p: inner, k: i }
			if (i === at) { o.extra = 'x'; r = probe(o, inner) + r }
			r = (r.length > 80 ? '' : r) + o.p.q
		}
		return r
	}
	function constructs(n, at) {
		let r = 0
		for (let i = 0; i < n; i++) {
			const c = new C(i, i + 1), d = new C(c.a, 2)
			r += c.sum() + d.sum()
			if (i === at) { c.extra = 1; r += probe({ k: at, c: c, d: d }, c).length }
		}
		return r
	}
	function stack(n, at) {
		let s = ''
		for (let i = 0; i < n; i++) { const o = { v: i }; if (i === at) s = new Error('h' + o.v).stack.split('at ').length + ':' + o.v }
		return s
	}
	function construct(n) { let s = 0; for (let i = 0; i < n; i++) { const v = new V(i, i + 1, i + 2); s += v.sq() } return s }
	function added(n, at) {
		let s = 0
		for (let i = 0; i < n; i++) { const o = { v: i }; if (i === at) { o.v = odd; o.w = 5; s += o.w } s += typeof o.v === 'string' ? 100 : o.v }
		return s
	}
	function K(x) { this.x = x }
	K.prototype.m = function () { return this.x + 1 }
	function protos(n) { let s = 0; for (let i = 0; i < n; i++) { const k = new K(i); s += k.m() } return s }
`

// escapeRounds drives escapeSetup's functions, each past what native code
// makes of it in the warm rounds, and at what leaves in the last ones.
func escapeRounds() []string {
	var rounds []string
	for i := range 8 {
		at := []string{"-1", "17"}[i/5]
		rounds = append(rounds, `dist(a, b)`, `dots(40)`, `literal(40)`, `nested(40)`, `branches(40)`,
			`readsOther(40, `+at+`)`, `throwsIn(40, `+at+`)`, `twice(40, `+at+`)`, `holds(40, `+at+`)`,
			`constructs(40, `+at+`)`, `stack(40, `+at+`)`, `construct(40)`, `added(40, `+at+`)`, `protos(40)`)
	}
	// The constructor's prototype replaced, then a setter on it for the
	// property its body adds: the code checks both.
	rounds = append(rounds, `K.prototype = { m() { return this.x * 2 } }, protos(40)`, `protos(40)`, `protos(40)`,
		`Object.defineProperty(K.prototype, 'x', { set(v) { this.y = v }, get() { return this.y + 100 } }), protos(40)`, `protos(40)`)
	return rounds
}

// escapeUnmade are escapeSetup's functions whose code makes none of the
// objects their loops make, in the warm rounds.
var escapeUnmade = []string{"dots", "literal", "nested", "branches", "readsOther", "throwsIn", "twice", "holds", "constructs", "stack", "construct", "added", "protos"}

// Objects nothing but their code sees are not made natively, and where
// native code leaves while one is live, Go makes it, once for every slot
// naming it, as the code would have: each answer is the interpreter's,
// under every setting of the boundary's stress -- every function compiled
// at once, a budget of three back edges, and the interpreter finishing
// every few exits' invocations.
func TestJITEscapeAnalysis(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	rounds := escapeRounds()
	// Its warm rounds alone: each function's code makes none of its
	// loop's objects.
	r := jitAgreesForTestWith(t, jitStressConfig{}, escapeSetup, rounds[:5*14]...)
	for _, name := range escapeUnmade {
		if e := jitCallbackEntry(r, name); e == nil || e.ssa == nil || e.ssaVirtuals == 0 {
			t.Errorf("%s makes every object it makes: %+v", name, e)
		}
	}
	for _, c := range []jitStressConfig{{}, {threshold: true, budget: 3}, {threshold: true, budget: 3, deopt: 7}, {threshold: true, budget: 1, deopt: 2}} {
		r := jitAgreesForTestWith(t, c, escapeSetup, rounds...)
		t.Logf("%+v: %d materialized", c, r.jit.materialized)
		if r.jit.materialized == 0 {
			t.Errorf("%+v: Go made no object the code did not make", c)
		}
	}
}

// jitAgreesForTestWith is jitAgreesForTest with stress c set in the runtime
// with the JIT before anything runs.
func jitAgreesForTestWith(t *testing.T, c jitStressConfig, setup string, rounds ...string) *Runtime {
	t.Helper()
	want := New(Config{})
	t.Cleanup(func() { want.Close(); want.ReleaseClosed() })
	r := jitRuntimeForTest(t, Config{JIT: true})
	r.jitStress = c
	for _, rt := range []*Runtime{want, r} {
		if _, err := rt.Run(compileForTest(t, setup)); err != nil {
			t.Fatal(err)
		}
	}
	for i, src := range rounds {
		src = "''+(" + src + ")"
		wv, werr := want.Run(compileForTest(t, src))
		gv, gerr := r.Run(compileForTest(t, src))
		if (werr == nil) != (gerr == nil) {
			t.Fatalf("%+v round %d (%s): error %v, interpreter's %v", c, i, src, gerr, werr)
		}
		if werr != nil {
			if got, want := gerr.Error(), werr.Error(); got != want {
				t.Fatalf("%+v round %d (%s): error %q, interpreter's %q", c, i, src, got, want)
			}
			continue
		}
		if got, want := gv.String().Go(), wv.String().Go(); got != want {
			t.Fatalf("%+v round %d (%s): got %s, interpreter %s", c, i, src, got, want)
		}
	}
	return r
}

// A loop whose objects nothing else sees makes none: its allocations are
// those of the run, not one an object.
func TestJITEscapeAllocates(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	runtime.GC()
	defer debug.SetGCPercent(debug.SetGCPercent(-1))
	r := jitAgreesForTestWith(t, jitStressConfig{}, escapeSetup, `literal(50)`, `literal(50)`, `construct(50)`, `construct(50)`, `nested(50)`, `nested(50)`, `dots(50)`, `dots(50)`)
	for _, src := range []string{`literal(20000)`, `construct(20000)`, `nested(20000)`, `dots(20000)`} {
		jitCallbackMallocs(t, r, src)
		if n := jitCallbackMallocs(t, r, src); n > 200 {
			t.Errorf("%s: %d allocations", src, n)
		}
	}
}

// An object native code does not make holds what its properties hold where
// those values came from -- a string Go made, an object read from another
// -- which the collector, run at every call of Go, must find there while
// the object lives only in registers, and in what Go makes of it at an
// exit: each answer is the interpreter's, every property's value the one
// stored.
func TestJITEscapeCollector(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	runtime.GC()
	defer debug.SetGCPercent(debug.SetGCPercent(-1))
	calls := 0
	jitGoCallWork = func() {
		if calls++; calls%3 == 0 {
			runtime.GC()
		}
	}
	defer func() { jitGoCallWork = nil }()
	setup := `
		var names = ['ab', 'cde', 'f'], boxes = [{ v: 1 }, { v: 2 }, { v: 3 }], odd = 'x'
		function held(n, at) {
			let s = ''
			for (let i = 0; i < n; i++) {
				const o = { name: names[i % 3] + i, box: boxes[i % 3], k: i }
				const t = o.name + '/' + o.box.v
				if (i === at) s = JSON.stringify(o) + (odd * 2)
				else s = t + (s.length > 40 ? '' : s)
			}
			return s
		}`
	var rounds []string
	for i := range 10 {
		rounds = append(rounds, `held(30, `+[]string{"-1", "11"}[i/6]+`)`)
	}
	for _, c := range []jitStressConfig{{}, {threshold: true, budget: 2, deopt: 3}} {
		r := jitAgreesForTestWith(t, c, setup, rounds...)
		if r.jit.materialized == 0 {
			t.Errorf("%+v: Go made no object the code did not make", c)
		}
	}
	if calls == 0 {
		t.Error("no call of Go")
	}
}

// mergeSetup's functions each merge objects their code does not make: at
// a join, where a phi merges the objects its branches made, and round a
// loop, where the object a loop's body makes becomes the one the next
// turn starts with -- an accumulator -- each such merge an object not
// made of its own, its properties phis (escape.go's mergeAt). Then each
// meets what leaves native code while one is live, where Go makes it of
// its home: a property read as a number that is no number. aliased keeps
// a merged object live past its merge, which must be made: its write is
// seen through the other name.
const mergeSetup = `
	function P(v, w) { this.v = v; this.w = w }
	var odd = 'x'
	function joins(n, at) {
		let s = 0
		for (let i = 0; i < n; i++) { let o = new P(i, 1); if (i & 1) o = new P(i === at ? odd : -i, 2); s += o.v * o.w }
		return s
	}
	function aliased(n) {
		let s = 0
		for (let i = 0; i < n; i++) { let a = new P(i, 1); const b = a; if (i & 1) a = new P(-i, 2); a.v = 7; s += b.v + a.v }
		return s
	}
	function carried(n, at) {
		let c = new P(0, 1)
		for (let i = 0; i < n; i++) { if (i & 1) c = new P(c.v + (i === at ? odd : i), c.w * 2 % 7) }
		return c.v + ':' + c.w
	}
	function cycle(n, at) {
		let c = new P(1, 2)
		for (let i = 0; i < n; i++) { if (i % 3) c = new P(c.w, c.v + i); if (i === at) c.w = odd }
		return c.v + ',' + c.w
	}
	function nestedLoops(n) {
		let c = new P(0, 0)
		for (let i = 0; i < n; i++) for (let j = 0; j < 3; j++) c = new P(c.v + j, c.w + i)
		return c.v + c.w
	}
	function longLoop(n) {
		let c = new P(0, 0)
		for (let i = 0; i < n; i++) c = new P(c.v + i, c.w + 1)
		return c.v + c.w
	}
`

// Objects merged at a join or round a loop are not made natively, and
// where native code leaves while one is live, Go makes it: each answer is
// the interpreter's, under every setting of the boundary's stress. A loop
// that runs long in a frame Go cannot enter, its header's entry dropped
// for the object it carries (ssa.Entry's OSR), has the code compiled again
// with the entry, and goes on natively.
func TestJITEscapeMerges(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	var rounds []string
	for i := range 8 {
		at := []string{"-1", "17"}[i/5]
		rounds = append(rounds, `joins(40, `+at+`)`, `aliased(40)`, `carried(40, `+at+`)`, `cycle(40, `+at+`)`, `nestedLoops(20)`, `longLoop(40)`)
	}
	r := jitAgreesForTestWith(t, jitStressConfig{}, mergeSetup, rounds[:5*6]...)
	for _, name := range []string{"joins", "carried", "cycle", "nestedLoops", "longLoop"} {
		if e := jitCallbackEntry(r, name); e == nil || e.ssa == nil || e.ssaVirtuals == 0 {
			t.Errorf("%s makes every object it makes: %+v", name, e)
		}
	}
	for _, c := range []jitStressConfig{{}, {threshold: true, budget: 3}, {threshold: true, budget: 3, deopt: 7}, {threshold: true, budget: 1, deopt: 2}} {
		r := jitAgreesForTestWith(t, c, mergeSetup, rounds...)
		t.Logf("%+v: %d materialized", c, r.jit.materialized)
		if r.jit.materialized == 0 {
			t.Errorf("%+v: Go made no object the code did not make", c)
		}
	}
	// One long run, entered at its loop's header, where the code had no
	// entry: learned, the run goes on natively.
	r = jitAgreesForTestWith(t, jitStressConfig{}, mergeSetup, `longLoop(3000000)`)
	if e := jitCallbackEntry(r, "longLoop"); e == nil || e.ssa == nil || len(e.resumed) == 0 || e.ssaStats.entries == 0 {
		t.Errorf("longLoop's loop was not learned: %+v", e)
	}
}

// An object made on every path, here at its return, is made where it is
// taken: left unmade it saves nothing, and its loop keeps the entry a
// loop-carried object's would have dropped (ssa.Entry's OSR).
func TestJITEscapeMadeAnyway(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	setup := `
		function P(v, w) { this.v = v; this.w = w }
		function madeAnyway(n) {
			const o = new P(1, 2)
			let s = 0
			for (let i = 0; i < n; i++) s += o.v + i
			o.w = s
			return o
		}
		function run(n) { let s = 0; for (let i = 0; i < n; i++) s += madeAnyway(8).w; return s }`
	var rounds []string
	for range 8 {
		rounds = append(rounds, `run(40)`)
	}
	r := jitAgreesForTestWith(t, jitStressConfig{}, setup, rounds...)
	e := jitCallbackEntry(r, "madeAnyway")
	if e == nil || e.ssa == nil {
		t.Fatalf("madeAnyway was not compiled: %+v", e)
	}
	if e.ssaVirtuals != 0 || len(e.ssaUnresumed) != 0 {
		t.Errorf("%d objects unmade, entries dropped at %v", e.ssaVirtuals, e.ssaUnresumed)
	}
}
