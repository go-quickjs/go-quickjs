//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))

package vm

import "testing"

// A second super() in code the JIT runs: native code's super() of a this
// already bound is Go's (superGuards), which constructs the parent's own
// object and then throws, as the interpreter and the trees do. The answers
// are compared tier by tier under the stress settings, CI's among them.
func TestJITSecondSuperMakesItsOwnObject(t *testing.T) {
	st := jitDifferential(t, superAgainSrc)
	if st.Entries == 0 {
		t.Errorf("native code never ran: %+v", st)
	}
	r := New(Config{JIT: true})
	defer r.Close()
	r.jitCallThreshold, r.jitICThreshold = 1, 1
	v, err := r.Run(compileForTest(t, superAgainSrc))
	if err != nil {
		t.Fatal(err)
	}
	if got := v.String().Go(); got != superAgainWant {
		t.Errorf("got %s\nwant %s", got, superAgainWant)
	}
}
