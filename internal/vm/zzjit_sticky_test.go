//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))

package vm

import (
	"slices"
	"testing"
)

// Code judged not to pay on its first stretches (entrySlow) -- here
// invocations that do nothing -- is entered by Go again after
// jitNativeRetry calls the existing tiers made of it, and judged by what it
// does from then on (jitRetrySlow): a loop that runs long stays native.
// Judged once, for good, it had stayed in the tree tier.
func TestJITSlowCodeJudgedAgain(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	r := New(Config{JIT: true})
	t.Cleanup(func() { r.Close(); r.ReleaseClosed() })
	if r.jitCallThreshold <= 1 {
		t.Skip("the runtime compiles eagerly (QJS_JIT_STRESS)")
	}
	const setup = `
		var o = { k: 1 }
		function f(n) { let s = o.k; for (let i = 0; i < n; i++) s = (s * 3 + i) | 0; return s }`
	if _, err := r.Run(compileForTest(t, setup)); err != nil {
		t.Fatal(err)
	}
	cl := r.global.getOwn(r.atoms.intern("f")).value().Object().fn().closure
	if _, err := r.Run(compileForTest(t, `for (let k = 0; k < 400; k++) f(0)`)); err != nil {
		t.Fatal(err)
	}
	e := r.jit.hint(cl)
	if e == nil || e.ssa == nil || !e.entrySlow {
		t.Skipf("f was not judged slow on its empty invocations: %+v", e)
	}
	v, err := r.Run(compileForTest(t, `let t = 0; for (let k = 0; k < 3000; k++) t = (t + f(200)) | 0; t`))
	if err != nil {
		t.Fatal(err)
	}
	w := New(Config{})
	t.Cleanup(func() { w.Close(); w.ReleaseClosed() })
	if _, err := w.Run(compileForTest(t, setup)); err != nil {
		t.Fatal(err)
	}
	wv, _ := w.Run(compileForTest(t, `let t = 0; for (let k = 0; k < 3000; k++) t = (t + f(200)) | 0; t`))
	if v.Number() != wv.Number() {
		t.Fatalf("= %v, interpreter %v", v, wv)
	}
	if e = r.jit.hint(cl); e == nil || e.entrySlow || e.slowBackoff == 0 {
		t.Errorf("f, its loops long now, was not entered again: %+v", e)
	}
}

// A call left to Go (jitCallDone), decided from what it met at its fourth
// exit -- a bound function, which native code does not call -- is decided
// again when the code is compiled again (jitSeedCalls), from what the
// caches know then: the plain function it calls now, called natively.
func TestJITCallLeftToGoDecidedAgain(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	r := New(Config{JIT: true})
	t.Cleanup(func() { r.Close(); r.ReleaseClosed() })
	if r.jitCallThreshold <= 1 {
		t.Skip("the runtime compiles eagerly (QJS_JIT_STRESS)")
	}
	const setup = `
		function add(a, b) { let s = a; for (let i = 0; i < 2; i++) s = (s + b) | 0; return s }
		var fn = add.bind(null)
		var o = { k: 1 }
		function h(n, j) { let s = 0; for (let i = 0; i < n; i++) { s = fn(s, i) | 0; if (j) s = (s + o.k) | 0 } return s }`
	if _, err := r.Run(compileForTest(t, setup)); err != nil {
		t.Fatal(err)
	}
	for range 12 {
		if _, err := r.Run(compileForTest(t, `h(200, 0)`)); err != nil {
			t.Fatal(err)
		}
	}
	cl := r.global.getOwn(r.atoms.intern("h")).value().Object().fn().closure
	e := r.jit.hint(cl)
	if e == nil || e.ssa == nil || !slices.Contains(e.callSites, jitCallDone) {
		t.Skipf("h's call of a bound function was not left to Go: %+v", e)
	}
	// The plain function from now on, and a count that is a string, a
	// speculation h is compiled again for.
	if _, err := r.Run(compileForTest(t, `fn = add; for (let k = 0; k < 12; k++) h('200', 0)`)); err != nil {
		t.Fatal(err)
	}
	e = r.jit.hint(cl)
	if e == nil || !slices.ContainsFunc(e.nativeCalls, func(x jitInline) bool { return x.cl != nil && x.cl.fn.Name == "add" }) &&
		!slices.ContainsFunc(e.inlines, func(x jitInline) bool { return x.cl != nil && x.cl.fn.Name == "add" }) {
		t.Errorf("h's call, left to Go, not decided again for add: reopts %d inline %d callSites %v nativeCalls %d inlines %d failed %v", e.reopts, e.inlineReopts, e.callSites, len(e.nativeCalls), len(e.inlines), e.failed)
	}
}
