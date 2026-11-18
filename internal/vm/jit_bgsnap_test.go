//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64))

package vm

import (
	"strings"
	"testing"
)

// A compile in the background asks its snapshot nothing ahead of a callee
// no site inlines (ssa.NeverInlined), whose feedback the build never asks
// -- each callee the function had met had been asked every question,
// nested to jitSnapDepth: Typescript's snapshots took 620 ms on the
// runtime's goroutine, more than its compiles on the spot took. A callee
// it may inline is asked as before, and the build misses nothing.
func TestJITBackgroundSnapshotSkipsLongCallees(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	var long strings.Builder
	long.WriteString("function long(x) { var s = x;")
	for i := range 80 {
		long.WriteString(" s = (s * 31 + " + string(rune('0'+i%10)) + ") % 1000003;")
	}
	long.WriteString(" return s }\n")
	r := jitRuntimeForTest(t, Config{JIT: true})
	src := long.String() + `function small(x) { return x + 1 }
		function f(n) { var s = 0; for (var i = 0; i < n; i++) s += small(i) + long(i); return s }`
	if _, err := r.Run(compileForTest(t, src)); err != nil {
		t.Fatal(err)
	}
	want, err := r.Run(compileForTest(t, "f(300)"))
	if err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if _, err := r.Run(compileForTest(t, "f(300)")); err != nil {
			t.Fatal(err)
		}
	}
	j := jitRegJobForTest(t, r, jitGlobalClosureForTest(r, "f"))
	asked := map[string]int{}
	for k, c := range j.snap.s.callees {
		if k.cl != nil {
			asked[k.cl.fn.Name] = len(c.answers)
		}
	}
	if n, ok := asked["small"]; !ok || n == 0 {
		t.Errorf("small: asked %v, %d answers: want its questions asked ahead (%v)", ok, n, asked)
	}
	if n := asked["long"]; n != 0 {
		t.Errorf("long: %d answers: want none asked of a callee no site inlines (%v)", n, asked)
	}
	r.SetJITBackground(true)
	r.jitStress = jitStressConfig{background: true, checks: 1}
	r.jitRegSubmit(j)
	v, err := r.Run(compileForTest(t, "f(300)"))
	if err != nil || v.Number() != want.Number() {
		t.Fatalf("f(300) = %v, %v; want %v", v, err, want)
	}
	if j.missed || r.jit.regSnapMisses != 0 || r.jit.regPendingJobs[j.key] != nil {
		t.Errorf("missed %v (%d), pending %v: want the build from the snapshot installed", j.missed, r.jit.regSnapMisses, r.jit.regPendingJobs[j.key] != nil)
	}
}
