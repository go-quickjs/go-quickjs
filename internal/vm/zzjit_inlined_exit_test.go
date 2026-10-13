//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))

package vm

import (
	"runtime"
	"runtime/debug"
	"testing"
)

// A native callee whose code inlined a callee before that callee's
// property reads had run -- move, compiled when the native rec first
// called it, inlining pop, which had never run -- leaves native code at
// those reads until it is compiled again for what they learned. Its exits
// are its code's, ticking toward that compile, and Go compiles it before
// it goes on in it: counted for pop's own code, which Go finishes them in,
// move never came to be compiled again, and every call of it left native
// code, 2.5 times slower than the tree tier with no JIT.
func TestJITInlinedExitsCompileNativeCalleeAgain(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	runtime.GC()
	defer debug.SetGCPercent(debug.SetGCPercent(-1))
	r := New(Config{JIT: true})
	t.Cleanup(func() { r.Close(); r.ReleaseClosed() })
	const src = `
		const piles = [null, null, null]
		function Disk(s) { this.size = s; this.next = null }
		function push(d, p) { const t = piles[p]; d.next = t; piles[p] = d }
		function pop(p) { const t = piles[p]; piles[p] = t.next; t.next = null; return t }
		function move(f, t) { push(pop(f), t) }
		function rec(n, f, t) { if (n === 1) { move(f, t) } else { const o = 3 - f - t; rec(n - 1, f, o); move(f, t); rec(n - 1, o, t) } }
		for (let i = 10; i >= 0; i--) push(new Disk(i), 0)`
	// A top-level loop, a script's: native code, entered at the loop,
	// calls rec natively from its first call.
	const run = `for (let k = 0; k < 100; k++) { rec(10, 0, 1); rec(10, 1, 0) } String(piles[0].size)`
	if _, err := r.Run(compileForTest(t, src)); err != nil {
		t.Fatal(err)
	}
	unwound := r.jit.unwound
	v, err := r.Run(compileForTest(t, run))
	if err != nil {
		t.Fatal(err)
	}
	if got := v.String().Go(); got != "0" {
		t.Fatalf("the top disk is %s, want 0", got)
	}
	// Compiled for its native callers alone (jitNativeCallee).
	e := r.jitEntryOf(r.global.getOwn(r.atoms.intern("move")).value().Object().fn().closure)
	if e == nil || e.ssa == nil {
		t.Fatalf("move has no code: %+v", e)
	}
	// 200 calls of rec(10), each moving 1023 disks; without the compile,
	// more than 500,000 exits.
	if n := r.jit.unwound - unwound; n > 2000 {
		t.Errorf("native calls left native code %d times in 204,600 moves", n)
	}
}
