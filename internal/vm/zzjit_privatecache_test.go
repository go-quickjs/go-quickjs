//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))

package vm

import "testing"

// privateMemberScript's reads and writes, in code the JIT compiles round
// after round, its members' sites the tree tier's where the JIT leaves
// them to Go, answer what Node's do.
func TestJITPrivateMembersAgreeWithNode(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	r := jitAgreesForTest(t, "function run() { return "+privateMemberScript+"; }", "run()", "run()", "run()", "run()")
	for i := range 2 {
		v, err := r.Run(compileForTest(t, "run()"))
		if err != nil {
			t.Fatal(err)
		}
		if got := v.String().Go(); got != privateMemberWant {
			diffLinesForTest(t, i, got, privateMemberWant)
		}
	}
}
