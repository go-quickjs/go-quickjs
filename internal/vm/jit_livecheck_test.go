//go:build quickjs_jit && quickjs_verify && !android && !ios && ((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64))

package vm

import (
	"fmt"
	"strings"
	"testing"
)

// liveCheckSource is n reads of an object from a property, each stored
// into an array on a branch of its own and into a property after it: n
// values read from cells, each live across a store or two, in a function
// of n stores and some 2n blocks, as TestJITLongCode's reads of o.v are.
func liveCheckSource(name string, n int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "function %s(a, rounds) {\nvar o = {p: {k: 1}, q: null}, arr = [null, null, null, null, null, null, null, null], s = 0;\n", name)
	b.WriteString("for (var r = 0; r < rounds; r++) {\n")
	for i := range n {
		fmt.Fprintf(&b, "var v%d = o.p;\nif (a > %d) { arr[%d] = v%d; }\no.q = v%d;\n", i, i%7, i%8, i, i)
	}
	b.WriteString("s = s + o.q.k;\n}\nreturn s;\n}\n")
	return b.String()
}

// The quickjs_verify build checks what is live across a function's stores
// (ssa's liveAcross) against a reference made afresh (liveAfterRef), whose
// memory is linear in the function as the compile's is: eight times the
// statements build in no more than sixteen times the memory. The reference
// had kept a table of every candidate by every block -- 132 MB for a
// thousand of these statements, 6.8 GB for eight thousand, and 17 GB for
// TestJITLongCode's longest function, which a 16 GB runner could not give.
func TestJITLiveCheckLinear(t *testing.T) {
	if !jitSSABackend || testing.Short() {
		t.Skip("no SSA backend on this architecture, or short")
	}
	plain := func(*Runtime) []Value { return []Value{Float(3), Float(2)} }
	var cs [2]linearCompile
	for k, n := range []int{1000, 8000} {
		name := fmt.Sprintf("livecheck%d", n)
		cs[k] = linearCompileOf(t, liveCheckSource(name, n), name, plain)
	}
	s, b := cs[0], cs[1]
	t.Logf("build %.1f MB -> %.1f MB", float64(s.build)/(1<<20), float64(b.build)/(1<<20))
	if b.build > 16*s.build {
		t.Errorf("the build with its liveness checked: eight times the statements took %.1f times the memory", float64(b.build)/float64(s.build))
	}
}
