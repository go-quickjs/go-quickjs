//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))

package vm

import "testing"

// TestJITArrayLiteralHoles makes arrayHolesSetup's literals in a runtime
// that compiles at the first call: a literal with holes is one new_array,
// which native code makes (ir.ArrayLiteral) where it refused a function
// with array_push, and holeLoop, which makes one and asks in of it at each
// turn, runs natively, answering as the interpreter and Node do.
func TestJITArrayLiteralHoles(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	var rounds []string
	for range 4 {
		for _, c := range arrayHolesCases {
			rounds = append(rounds, c.src)
		}
	}
	r := jitAgreesForTest(t, arrayHolesSetup, rounds...)
	jitNativeForTest(t, r, "holeLoop")
	for _, c := range arrayHolesCases {
		v, err := r.Run(compileForTest(t, "''+("+c.src+")"))
		if err != nil {
			t.Fatalf("%s: %v", c.src, err)
		}
		if got := v.String().Go(); got != c.want {
			t.Fatalf("%s:\n got %q\nwant %q", c.src, got, c.want)
		}
	}
}
