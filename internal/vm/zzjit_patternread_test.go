//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))

package vm

import (
	"runtime/debug"
	"testing"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
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
	both := jitBothForTest(t, setup)
	both.agree(t, `run(300, { x: 1 })`, `run(300, { x: 1 })`)
	e := jitNativeOrCalleeForTest(t, both.jit, "opts")
	if e.deopts > 2 || e.invalid || e.link.in+e.ssaStats.entries < 300 {
		t.Errorf("opts deoptimized %d times, invalid %v, called natively %d times, entered %d", e.deopts, e.invalid, e.link.in, e.ssaStats.entries)
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
// (jitCallResult). Taken for a call's operands, the source had been dropped
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

// An object pattern's read is compiled to search its source's own table
// alone until it misses there: its first miss deoptimizes, once, and the
// site looks in the megamorphic cache from then on, Go reading what that
// has not (jitDeoptSeen, jitFeedback's Property). A read that never misses
// has no branch for Go -- microbench's local_destruct, compiled with one
// at each of its eight reads, had taken 9% longer.
func TestJITPatternReadsLearnTheirMisses(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	defer debug.SetGCPercent(debug.SetGCPercent(-1))
	both := jitBothForTest(t, `
		function hits(o) { let s = 0; for (let i = 0; i < 4; i++) { const { a, b } = o; s += a + b } return s }
		function misses(o) { let s = 0; for (let i = 0; i < 4; i++) { const { a, b = 2 } = o; s += a + b } return s }
		function run(n) { let s = 0; for (let i = 0; i < n; i++) s += hits({ a: i, b: 1 }) + misses({ a: i }); return s }`)
	for range 3 {
		both.agree(t, `run(300)`)
	}
	r := both.jit
	for _, c := range []struct {
		name string
		mega bool
	}{{"hits", false}, {"misses", true}} {
		e := jitEntryForTest(t, r, c.name)
		fn := r.global.getOwn(r.atoms.intern(c.name)).value().Object().fn().closure.fn
		mega := false
		for pc, in := range fn.Code {
			if in.Op == bytecode.OpGetPropUnder && jitPolyMega(e, pc) {
				mega = true
			}
		}
		if mega != c.mega || e.deopts > 2 {
			t.Errorf("%s: a pattern read megamorphic %v, want %v; %d deoptimizations", c.name, mega, c.mega, e.deopts)
		}
	}
}
