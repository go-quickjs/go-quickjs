//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))

package vm

import "testing"

// TestJITArraySearches makes arraySearchSetup's searches, each a few
// times, in a runtime that compiles at the first call: native code's calls
// of the searches (ssa.GoIndexOf) and of Go with frames answer as the
// interpreter does, and as Node does.
func TestJITArraySearches(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	var rounds []string
	for range 4 {
		for _, c := range arraySearchCases {
			rounds = append(rounds, c.src)
		}
	}
	r := jitAgreesForTest(t, arraySearchSetup, rounds...)
	jitNativeForTest(t, r, "searchAll")
	for _, c := range arraySearchCases {
		v, err := r.Run(compileForTest(t, "''+("+c.src+")"))
		if err != nil {
			t.Fatalf("%s: %v", c.src, err)
		}
		if got := v.String().Go(); got != c.want {
			t.Fatalf("%s:\n got %s\nwant %s", c.src, got, c.want)
		}
	}
}
