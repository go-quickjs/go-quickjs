//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64))

package vm

import (
	"slices"
	"testing"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
)

// frequencySetup's caller makes, at each invocation, three calls of g in a
// loop, one method call, none of g where k is negative, one construction,
// one call of a pure body, which the call path answers without a frame,
// one of g through Function.prototype.call, whose frame is the built-in's,
// and one call in tail position; tail's call of g is in tail position too.
const frequencySetup = `
	function C(x) { let t = x + 1; this.v = t }
	function g(o) { let t = o.n + 1; o.n = t & 1023; return t }
	var obj = { m(o) { let t = o.n + 2; o.n = t & 1023; return t } }
	function pure(o) { return o.n * 2 }
	function tail(o) { return g(o) }
	function caller(o, k) {
		let s = 0
		for (let i = 0; i < 3; i++) s += g(o)
		s += obj.m(o)
		if (k < 0) s += g(o)
		const c = new C(s)
		s += c.v + pure(o)
		s += g.call(null, o)
		return tail(o) + s
	}
	var o = {n: 0}`

// frequencyRuntime is a runtime running frequencySetup with the JIT's own
// thresholds.
func frequencyRuntime(t *testing.T) *Runtime {
	t.Helper()
	r := jitRuntimeForTest(t, Config{JIT: true})
	jitDefaultsForTest(r)
	if _, err := r.Run(compileForTest(t, frequencySetup)); err != nil {
		t.Fatal(err)
	}
	return r
}

// Native code counts nothing, as V8's optimized code counts no feedback:
// once caller and c2 are compiled, from their budgets, their invocations
// and their calls -- natively, inlined, or through Go, as c2's of gw,
// which is not compiled -- are not counted, and the counts the existing
// tiers made while they warmed are their code's entries'. Each answer is
// the interpreter's.
func TestJITCountsNotNative(t *testing.T) {
	t.Parallel()
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	r := frequencyRuntime(t)
	want := newTestRuntime(t, Config{})
	t.Cleanup(func() { want.Close(); want.ReleaseClosed() })
	if _, err := want.Run(compileForTest(t, frequencySetup)); err != nil {
		t.Fatal(err)
	}
	both := jitBoth{jit: r, want: want}
	both.script(t, `
		function gw(o) { with (o) { return n } }
		function c2(o, i) { let s = g(o) + gw(o); if (i < 0) s += gw(o); for (let j = 0; j < 40; j++) s = (s * 31 + j) | 0; return s }
		function drive(n) { let s = 0; for (let i = 0; i < n; i++) s = (s + caller(o, i) + c2(o, i)) | 0; return s }`)
	for range 10 {
		both.agree(t, `drive(100)`)
	}
	// Register code's counts are its closures' (jitRegCounts), which
	// the feedback dispatcher counts and native code does not (their
	// frequencies TestJITRegFrequency's).
	var counts [][]uint64
	for _, name := range []string{"caller", "c2"} {
		e := regEntryNamedForTest(r, name)
		if e == nil || e.invalid {
			t.Fatalf("%s has no code: %+v", name, e)
		}
		c := jitGlobalClosure(r, name).jitRegCounts
		if c == nil || regNativeDefault && c[0] == 0 {
			t.Fatalf("%s counts nothing: %v", name, c)
		}
		counts = append(counts, slices.Clone(c))
	}
	goCalls := r.jit.goCalls
	both.agree(t, `drive(1000)`)
	if r.jit.goCalls-goCalls < 1000 {
		t.Errorf("c2's code made %d calls of Go in 1,000 runs", r.jit.goCalls-goCalls)
	}
	for i, name := range []string{"caller", "c2"} {
		if got := jitGlobalClosure(r, name).jitRegCounts; !slices.Equal(got, counts[i]) {
			t.Errorf("%s's code counted: %v, after %v", name, got, counts[i])
		}
	}
}

// The counts outlive the code the cache drops, as V8's feedback outlives
// the code optimized from it (jitGhost): caller's, flushed unused, are its
// next entry's, which goes on counting from them.
func TestJITCountsOutliveCode(t *testing.T) {
	t.Parallel()
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	setEpoch := jitFlushForTest(t)
	r := frequencyRuntime(t)
	if _, err := r.Run(compileForTest(t, `for (let i = 0; i < 700; i++) caller(o, i)`)); err != nil {
		t.Fatal(err)
	}
	// Register code's counts are its closure's (jitRegCounts), which
	// outlive its code: the code flushed, they go on counting.
	e, code := jitCachedForTest(r, "caller")
	c := jitGlobalClosure(r, "caller").jitRegCounts
	if !code || c == nil {
		t.Fatalf("caller has no code: %+v", e)
	}
	runs := c[0]
	setEpoch(1000 + jitFlushAge)
	r.jit.flush()
	setEpoch(1000 + 2*jitFlushAge)
	r.jit.flush()
	if e, _ := jitCachedForTest(r, "caller"); e != nil {
		t.Fatalf("caller's unused code was not flushed: %+v", e)
	}
	if _, err := r.Run(compileForTest(t, `for (let i = 0; i < 20; i++) caller(o, i)`)); err != nil {
		t.Fatal(err)
	}
	if now := jitGlobalClosure(r, "caller").jitRegCounts; len(now) == 0 || &now[0] != &c[0] || now[0] < runs {
		t.Fatalf("caller counts %v, not %v from %d runs", now, c, runs)
	}
}

// jitFnForTest is the function of the global name.
func jitFnForTest(t *testing.T, r *Runtime, name string) *bytecode.Function {
	t.Helper()
	return r.global.getOwn(r.atoms.intern(name)).value().Object().fn().closure.fn
}
