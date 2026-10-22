//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))

package vm

import (
	"runtime"
	"runtime/debug"
	"slices"
	"testing"
	"unsafe"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
	"github.com/go-quickjs/go-quickjs/internal/jit/ssa"
)

// jitInlinedForTest records, for each function compiled while the test
// runs, the closures its last compile inlined (ssa.Func's Inlined).
func jitInlinedForTest(t *testing.T) map[string][]uintptr {
	t.Helper()
	got := map[string][]uintptr{}
	jitSSAOptimized = func(fn *bytecode.Function, f *ssa.Func) {
		got[fn.Name] = slices.Clone(f.Inlined)
	}
	t.Cleanup(func() { jitSSAOptimized = nil })
	return got
}

// jitClosureForTest is the closure of the function the script's global
// expression src gives.
func jitClosureForTest(t *testing.T, r *Runtime, src string) uintptr {
	t.Helper()
	v, err := r.Run(compileForTest(t, src))
	if err != nil || !v.IsObject() || v.Object().fn() == nil {
		t.Fatalf("%s: %v %v", src, v, err)
	}
	return uintptr(unsafe.Pointer(v.Object().fn().closure))
}

const inlineThrowSetup = `
	function Pile() { this.items = []; this.n = 0 }
	Pile.prototype.push = function (v) {
		if (v < 0) throw new RangeError('negative ' + v + ' at ' + this.n)
		this.items[this.n] = v
		this.n = this.n + 1
	}
	Pile.prototype.pop = function () {
		if (this.n === 0) throw new Error('empty')
		this.n = this.n - 1
		return this.items[this.n]
	}
	Pile.prototype.move = function (o) { o.push(this.pop()) }
	Pile.prototype.check = function (v) { if (v > this.n) throw v; return v }
	function fill(p, n) { for (let i = 0; i < n; i++) p.push(i); return p.n }
	function shuffle(a, b, n) { let s = 0; for (let i = 0; i < n; i++) { a.move(b); b.move(a); s += b.n + a.n } return s }
	function pushAll(p, vs) {
		let out = ''
		for (let i = 0; i < vs.length; i++) {
			try { p.push(vs[i]); out += p.pop() } catch (e) { out += e.name + ':' + e.message + '|' + e.stack } finally { out += '|fin;' }
		}
		return out
	}
	function drain(p, k) { let s = ''; for (let i = 0; i < k; i++) { try { s += p.pop() + ',' } catch (e) { s += e.name + ':' + e.message + '|' + e.stack + ';' } } return s }
	function moves(a, b, k) { let s = 0; for (let i = 0; i < k; i++) { a.move(b); s += b.n } return s }
	function checks(p, k) { let s = 0; for (let i = 0; i < k; i++) { try { s += p.check(i) } catch (e) { s += 1000 * e } } return s }
	var a = new Pile(), b = new Pile()
	fill(a, 50); fill(b, 50)
`

// A callee whose throw, or whose call it has never been seen to make, is
// on a path seldom taken is inlined all the same, as V8 inlines one
// (ssa.InlineExitFeedback): native code leaves the inlined frame there, Go
// making its frame, and the interpreter throws -- the exception caught by
// the caller's try and its finally run, the stack trace the interpreter's,
// with the callee's frame and the caller's, this and the arguments as the
// callee had them. Before, a throw new Error(...) guard had kept each
// callee from being inlined, and the caller that called it with it. In the
// warm rounds, each callee inlined, no call leaves native code.
func TestJITInlinesColdThrows(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	runtime.GC()
	defer debug.SetGCPercent(debug.SetGCPercent(-1))
	inlined := jitInlinedForTest(t)
	var rounds []string
	for range 4 {
		rounds = append(rounds, `shuffle(a, b, 200)`, `pushAll(a, [1, 2, 3])`, `checks(a, 20)`)
	}
	rounds = append(rounds,
		// The throws: the callee inlined at each, the frame Go makes for
		// it, the catch and the finally the caller's.
		`pushAll(a, [4, -5, 6, -7])`,
		`drain(new Pile(), 3)`,
		`checks(a, 300)`,
		`moves(new Pile(), a, 3)`,
		// Uncaught, out of an inlined frame through the callers'.
		`moves(new Pile(), a, 2)`,
		`shuffle(a, b, 10)`)
	both := jitBothForTest(t, inlineThrowSetup)
	both.agree(t, rounds[:12]...)
	r := both.jit
	push := jitClosureForTest(t, r, `Pile.prototype.push`)
	pop := jitClosureForTest(t, r, `Pile.prototype.pop`)
	move := jitClosureForTest(t, r, `Pile.prototype.move`)
	check := jitClosureForTest(t, r, `Pile.prototype.check`)
	for _, c := range []struct {
		fn   string
		want []uintptr
	}{{"shuffle", []uintptr{pop, push, move}}, {"pushAll", []uintptr{push}}, {"checks", []uintptr{check}}} {
		for _, w := range c.want {
			if !slices.Contains(inlined[c.fn], w) {
				t.Errorf("%s inlines %x, not %x of %x, %x, %x, %x", c.fn, inlined[c.fn], w, push, pop, move, check)
			}
		}
	}
	hosts := r.jit.hosts
	both.agree(t, `shuffle(a, b, 1000)`, `pushAll(a, [1, 2, 3, 4, 5])`, `checks(a, 50)`)
	if n := r.jit.hosts - hosts; n != 0 {
		t.Errorf("the inlined callees left native code %d times", n)
	}
	// The throws, each answer the interpreter's.
	both.agree(t, rounds[12:]...)
}

// jitBoth is a runtime with the JIT and one without, which have run the
// same script.
type jitBoth struct{ jit, want *Runtime }

// jitBothForTest is a jitBoth that has run setup.
func jitBothForTest(t *testing.T, setup string) jitBoth {
	t.Helper()
	want := New(Config{})
	t.Cleanup(func() { want.Close(); want.ReleaseClosed() })
	r := jitRuntimeForTest(t, Config{JIT: true})
	for _, rt := range []*Runtime{want, r} {
		if _, err := rt.Run(compileForTest(t, setup)); err != nil {
			t.Fatal(err)
		}
	}
	return jitBoth{jit: r, want: want}
}

// script runs src, a script, in both.
func (b jitBoth) script(t *testing.T, src string) {
	t.Helper()
	for _, rt := range []*Runtime{b.want, b.jit} {
		if _, err := rt.Run(compileForTest(t, src)); err != nil {
			t.Fatal(err)
		}
	}
}

// agree runs each of rounds in both, and fails unless each gives what the
// interpreter gives, or throws what it throws.
func (b jitBoth) agree(t *testing.T, rounds ...string) {
	t.Helper()
	for _, src := range rounds {
		src = "''+(" + src + ")"
		wv, werr := b.want.Run(compileForTest(t, src))
		gv, gerr := b.jit.Run(compileForTest(t, src))
		if (werr == nil) != (gerr == nil) {
			t.Fatalf("%s: error %v, interpreter's %v", src, gerr, werr)
		}
		if werr != nil {
			if got, want := gerr.Error(), werr.Error(); got != want {
				t.Fatalf("%s: error %q, interpreter's %q", src, got, want)
			}
			continue
		}
		if got, want := gv.String().Go(), wv.String().Go(); got != want {
			t.Fatalf("%s: got %s, interpreter %s", src, got, want)
		}
	}
}

// An inlined callee's speculations are those its own code learned: a
// failure its own code met is built generic where it is inlined too, as
// it is in its own code (jitFeedback's Generic, ownEntry). take's read of
// t.next, an object stored as an element, had been read for a number in
// every caller that inlined take, though take's own code had learned it
// was not one: each call left native code, until the call was no longer
// inlined -- AWFY Towers' popDiskFrom.
func TestJITInlinedCalleeKeepsItsFailures(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	runtime.GC()
	defer debug.SetGCPercent(debug.SetGCPercent(-1))
	inlined := jitInlinedForTest(t)
	both := jitBothForTest(t, `
		function D(v) { this.v = v; this.next = null }
		function Q() { this.slots = [null, null] }
		Q.prototype.take = function (i) { const t = this.slots[i]; this.slots[i] = t.next; t.next = null; return t }
		Q.prototype.put = function (i, d) { d.next = this.slots[i]; this.slots[i] = d }
		function own(q, n) { const take = Q.prototype.take; let s = 0; for (let k = 0; k < n; k++) { const d = take.call(q, 0); s += d.v; q.put(0, d) } return s }
		function cycle(q, n) { let s = 0; for (let k = 0; k < n; k++) { q.put(1, q.take(0)); q.put(0, q.take(1)); s += q.slots[0].v } return s }
		var q = new Q()
		for (let i = 0; i < 4; i++) q.put(0, new D(i))`)
	// take's own code first, called natively by own, never inlined there.
	both.agree(t, `own(q, 50)`, `own(q, 50)`)
	r := both.jit
	take := jitClosureForTest(t, r, `Q.prototype.take`)
	takeV, err := r.Run(compileForTest(t, `Q.prototype.take`))
	if err != nil {
		t.Fatal(err)
	}
	if e := r.jit.entryFor(takeV.Object().fn().closure.fn); e == nil || len(e.failed) == 0 {
		t.Fatalf("take's own code learned no failure: %+v", e)
	}
	both.agree(t, `cycle(q, 100)`, `cycle(q, 100)`)
	hosts := r.jit.hosts
	both.agree(t, `cycle(q, 1000)`)
	if n := r.jit.hosts - hosts; n != 0 {
		t.Errorf("cycle left native code %d times", n)
	}
	if !slices.Contains(inlined["cycle"], take) {
		t.Errorf("cycle inlines %x, not take %x", inlined["cycle"], take)
	}
	if e := jitNativeForTest(t, r, "cycle"); len(e.notInline) != 0 {
		t.Errorf("cycle's calls at %v left native code until they were not inlined", e.notInline)
	}
}

// A callee that reads globals is inlined with its reads, made as the
// caller's own are, from the callee's own sites (jitFeedback's Global, of
// the callee's closure, whose scope a native call's target shares
// (jitNativeTargetOf)): a var, a let, a const, Math.abs. What the script
// does to the bindings after is seen where they are read: a var assigned,
// a let given a string, a global property a later script's let shadows.
func TestJITInlinesGlobalReads(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	runtime.GC()
	defer debug.SetGCPercent(debug.SetGCPercent(-1))
	inlined := jitInlinedForTest(t)
	both := jitBothForTest(t, `
		var K = 3; let L = 4; const C = 5; globalThis.M = 1
		function g(x) { return x * K + L + C + M + Math.abs(x - 50) }
		function f(n) { let s = 0; for (let i = 0; i < n; i++) s += g(i); return s }`)
	both.agree(t, `f(100)`, `f(100)`, `f(100)`)
	r := both.jit
	if g := jitClosureForTest(t, r, `g`); !slices.Contains(inlined["f"], g) {
		t.Fatalf("f inlines %x, not g %x", inlined["f"], g)
	}
	hosts := r.jit.hosts
	both.agree(t, `f(1000)`)
	if n := r.jit.hosts - hosts; n != 0 {
		t.Errorf("f left native code %d times", n)
	}
	both.agree(t, `K = 7, f(10)`, `L = 'x', f(3)`, `L = 1, f(10)`, `f(100)`)
	both.script(t, `let M = 9`)
	both.agree(t, `f(10)`, `f(100)`, `M = 10, f(100)`)
}
