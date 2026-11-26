//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64))

package vm

import (
	"testing"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
)

// A function's report counts the calls its native callers made of it over
// its compiles (jitEntry's calledIn): its link's count of them begins
// afresh as the function is compiled again, for judging the new code, and
// the report's Native had gone down with it -- -jitreport's figure for the
// function since the last benchmark negative.
func TestJITReportCountsNativeCallsOverCompiles(t *testing.T) {
	t.Parallel()
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	jitTestSwitch(t, func(jc *jitTestConfig) { jc.noInline = func(fn *bytecode.Function) bool { return fn.Name == "callee" } })
	r := newTestRuntime(t, Config{JIT: true})
	r.jitStress = jitStressConfig{}
	r.SetJITBackground(false)
	src := `
		function callee(x) { return x + 1 }
		function caller(n) { let s = 0; for (let i = 0; i < n; i++) s = callee(s); return s }
		for (let k = 0; k < 20; k++) caller(1000)`
	if _, err := r.Run(compileForTest(t, src)); err != nil {
		t.Fatal(err)
	}
	cl := jitGlobalClosureForTest(r, "callee")
	e := jitRegOfForTest(r, cl)
	if e == nil || e.code == nil || e.invalid {
		t.Fatalf("callee has no valid code: %+v", e)
	}
	l := r.jit.regLinks[e.key]
	if l == nil || l.in == 0 {
		t.Fatal("callee was not called natively through its link")
	}
	before, _ := regReportOf(r, "callee")
	if ne := r.jitRegReoptimize(cl, r.regOf(cl), e, -1); ne == nil || ne.code == nil {
		t.Fatalf("callee not compiled again: %+v", ne)
	}
	if l.in != 0 {
		t.Fatalf("its link's count %d, not begun afresh by the compile", l.in)
	}
	if after, _ := regReportOf(r, "callee"); after.Native < before.Native {
		t.Errorf("Native %d after it was compiled again, %d before", after.Native, before.Native)
	}
}
