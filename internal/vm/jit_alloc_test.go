//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64))

package vm

import (
	"strings"
	"testing"
)

// Object literals, additions to captured bindings, push
// and pop (pureElemOp, which native code's growth of an array calls too), a
// for...in's cursors used again (forincursor.go) and arrays grown past
// 16,384 elements (growelems.go) answer as the interpreter does with
// the JIT on, the interpreter warming code up and running what native code
// leaves -- under every stress setting, with the collector idle and while
// it marks -- and native code runs some of each.
func TestJITLiteralsUpvalueAddsPushes(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	for _, marking := range jitMarkingModes() {
		// The string doubled past the longest would pass jitDifferential's
		// memory limit first: doubled twelve times.
		upvalues := strings.Replace(upvalueAddTestSrc, "i < 40", "i < 12", 1)
		for name, src := range map[string]string{"literals": literalTestSrc, "upvalue adds": upvalues, "push and pop": pushTestSrc, "for-in cursors": forInCursorTestSrc, "array growth": arrayGrowthTestSrc} {
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
