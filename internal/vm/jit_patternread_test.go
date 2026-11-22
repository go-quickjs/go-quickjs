//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64))

package vm

import (
	"runtime/debug"
	"testing"
)

// An object pattern's read (get_prop_under), which has no cache of its
// own, looks in the source's own table and then in the megamorphic cache,
// and Go reads what neither has from native code, filling the cache: a
// property the source lacks, whose default the pattern gives -- an options
// object's usual pattern -- is found absent natively from then on. It had
// deoptimized at every such read, nothing learned, until the function was
// compiled no more. Sources of other kinds -- a getter's, a proxy's, a
// string's, an inherited property's -- are Go's, and every answer the
// interpreter's.
func TestJITPatternReadsAbsent(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	defer debug.SetGCPercent(debug.SetGCPercent(-1))
	const setup = `
		function opts(a, o) { let s = 0; for (let i = 0; i < 4; i++) { const { x, y = 2, z = 'z' } = o; s += a + x + y + z.length } return s }
		function run(n, o) { let s = 0; for (let i = 0; i < n; i++) s += opts(i, o); return s }
		var proto = { y: 10 }
		var inherits = Object.create(proto); inherits.x = 1
		var proxy = new Proxy({ x: 3 }, { get(t, k) { return k === 'z' ? 'zz' : t[k] } })
		var getter = { get x() { return 5 }, get y() { return 6 } }`
	inlined := jitRegInlinedForTest(t)
	both := jitBothForTest(t, setup)
	both.agree(t, `run(300, { x: 1 })`, `run(300, { x: 1 })`)
	if jitRegInlinesForTest(t, both.jit, inlined, "run", "opts") {
		// The register form inlines opts in run, whose native code runs
		// its 300 calls: opts' own code is never compiled, and its
		// learning, its deoptimizations among it, is its life's.
		jitNativeForTest(t, both.jit, "run")
		if l := both.jit.jitRegLifeIfAny(jitGlobalClosureForTest(both.jit, "opts")); l != nil && l.deopts > 2 {
			t.Errorf("opts deoptimized %d times", l.deopts)
		}
	} else {
		v := jitNativeOrCalleeCodeForTest(t, both.jit, "opts")
		e, in := v.e, jitLinkInForTest(both.jit, jitGlobalClosureForTest(both.jit, "opts"))
		if e.deopts > 2 || v.invalid() || in+e.ssaStats.entries < 300 {
			t.Errorf("opts deoptimized %d times, invalid %v, called natively %d times, entered %d", e.deopts, v.invalid(), in, e.ssaStats.entries)
		}
	}
	for _, src := range []string{`{ x: 1, y: 3 }`, `inherits`, `proxy`, `getter`, `{ x: 2, z: 'abc' }`, `{ x: 1 }`} {
		both.agree(t, `run(100, `+src+`)`)
	}
	both.agree(t, `(() => { try { return run(3, null) } catch (e) { return e.constructor.name } })()`)
}

// A native callee's object pattern read Go makes, from native code, after
// which the callee leaves for Go to finish it (its call pending, as when
// its code is due to be compiled again): the read's value goes on top of
// its source, which stays, as the interpreter's get_prop_under leaves them
// (jitRegUnwind). Taken for a call's operands, the source had been dropped
// with a slot below it, and the callee had returned 0.
func TestJITPatternReadPending(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	defer debug.SetGCPercent(debug.SetGCPercent(-1))
	jitNoInlineForTest(t, "w")
	both := jitBothForTest(t, `
		function w(a, { x, y = 2 } = { x: a + 1 }) { return a + x + y }
		function ws(n, src) {
			let s = 0, l = []
			for (let i = 0; i < n; i++) { const a = w(i, undefined), b = w(i, src); l.push(a, b); s = (s + a + b) | 0 }
			return l.join()
		}`)
	for range 3 {
		both.agree(t, `ws(300, { x: 2 })`)
	}
}

// An object pattern's read compiled before it has run is compiled to
// search its source's own table alone until it misses there: its first
// miss deoptimizes, once, and the read learns the shape it missed in, as
// V8's load of an absent property learns its map, Go reading nothing from
// then on (jitRegDeoptSeen, jitRegFeedback's Property). A read that never
// misses has no branch for Go -- microbench's local_destruct, compiled
// with one at each of its eight reads, had taken 9% longer. The loop
// before each pattern is what has its reads compiled before they run: run
// by the interpreter first, a read's cache holds what it met, a miss
// included, which the read is compiled for with no deoptimization.
func TestJITPatternReadsLearnTheirMisses(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	defer debug.SetGCPercent(debug.SetGCPercent(-1))
	both := jitBothForTest(t, `
		function hits(o, n) { let s = 0; for (let i = 0; i < n; i++) s = (s + i) | 0; const { a, b } = o; return s + a + b }
		function misses(o, n) { let s = 0; for (let i = 0; i < n; i++) s = (s + i) | 0; const { a, b = 2 } = o; return s + a + b }
		function run(n) { let s = 0; for (let i = 0; i < n; i++) s += hits({ a: i, b: 1 }, 3) + misses({ a: i }, 3); return s }`)
	// Compiled at their loops, before their patterns' reads have run.
	both.agree(t, `hits({ a: 1, b: 1 }, 5000)`, `misses({ a: 1 }, 5000)`)
	r := both.jit
	for i := range 3 {
		hosts, guards := r.jit.hosts, r.jit.guards
		both.agree(t, `run(300)`)
		if n, g := r.jit.hosts-hosts, r.jit.guards-guards; i > 0 && n+g != 0 {
			t.Errorf("round %d: %d host exits, %d guards failed", i, n, g)
		}
	}
	for _, c := range []struct {
		name   string
		deopts uint16
	}{{"hits", 0}, {"misses", 1}} {
		e := jitCodeNamedForTest(t, r, c.name).e
		mega, missed := false, 0
		// The register form's sites are by register pc: the pattern's
		// reads are the only property reads of hits and misses.
		for i := range e.poly {
			mega = mega || e.poly[i].mega
			missed += len(e.poly[i].missed)
		}
		if mega || e.deopts != c.deopts || missed != int(c.deopts) {
			t.Errorf("%s: a pattern read megamorphic %v, %d shapes missed in, %d deoptimizations; want false, %d, %d (run inlines %v; sites %+v)", c.name, mega, missed, e.deopts, c.deopts, c.deopts, jitInlinedNamesForTest(r, "run"), e.poly)
		}
	}
}
