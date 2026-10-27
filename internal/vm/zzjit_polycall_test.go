//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))

package vm

import (
	"slices"
	"testing"
)

// polySetup's call fns[i & 1](x) meets two functions before it is decided.
const polySetup = `
	function inc(x) { return x + 1 }
	function dbl(x) { return x * 2 }
	var fns = [inc, dbl]
	function poly(n) { let s = 0; for (let i = 0; i < n; i++) s = (s + fns[i & 1](i)) | 0; return s }
`

// A call that met more than one function while it settled is polymorphic,
// as V8's feedback has it: made natively to each, not inlined for the one
// it met last, which would leave native code at every call of the other.
func TestJITPolymorphicCallNotInlined(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	rounds := []string{}
	for range 30 {
		rounds = append(rounds, `poly(100)`)
	}
	r := jitAgreesForTest(t, polySetup, rounds...)
	e := jitCallbackEntry(r, "poly")
	if e == nil || e.ssa == nil {
		t.Fatalf("poly is not compiled: %+v", e)
	}
	if len(e.inlines) != 0 || len(e.ssaInlinedAt) != 0 {
		t.Errorf("poly inlines its polymorphic call: inlines %d, inlined at %v", len(e.inlines), e.ssaInlinedAt)
	}
	targets := 0
	for _, x := range e.nativeCalls {
		if x.cl != nil && slices.Contains([]string{"inc", "dbl"}, x.cl.fn.Name) {
			targets++
		}
	}
	if targets != 2 {
		t.Errorf("poly calls %d of inc and dbl natively: %+v", targets, e.nativeCalls)
	}
	before := r.JITStats()
	if _, err := r.Run(compileForTest(t, `poly(100)`)); err != nil {
		t.Fatal(err)
	}
	if after := r.JITStats(); after.Hosts != before.Hosts {
		t.Errorf("poly still leaves native code: %d exits", after.Hosts-before.Hosts)
	}
	for _, c := range []jitStressConfig{{threshold: true, budget: 3}, {threshold: true, budget: 3, deopt: 7}} {
		jitAgreesForTestWith(t, c, polySetup, rounds...)
	}
}
