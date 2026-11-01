//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))

package vm

import (
	"strings"
	"testing"
)

// The tree tier's object literals (zzz_zliteral.go), additions to
// captured bindings (zzz_zupvstr.go), and push and pop (pureElemOp, which
// native code's growth of an array calls too) answer as the interpreter
// does with the JIT on, the tree tier warming code up and running what
// native code leaves -- under every stress setting, with the collector
// idle and while it marks -- and native code runs some of each.
func TestJITLiteralsUpvalueAddsPushes(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	for _, marking := range []bool{false, true} {
		// The string doubled past the longest would pass jitDifferential's
		// memory limit first: doubled twelve times.
		upvalues := strings.Replace(upvalueAddTestSrc, "i < 40", "i < 12", 1)
		for name, src := range map[string]string{"literals": literalTestSrc, "upvalue adds": upvalues, "push and pop": pushTestSrc} {
			t.Run(name, func(t *testing.T) {
				if marking {
					jitMarkingForTest(t)
				}
				if st := jitDifferential(t, src); st.SSAEntries == 0 {
					t.Errorf("marking %v: no native entry", marking)
				}
			})
		}
	}
}
