//go:build quickjs_jit && !android && !ios && amd64 && (linux || windows)

package engine

import (
	"fmt"
	"strings"
	"testing"

	wjit "github.com/go-quickjs/go-quickjs/wasm/internal/jit"
)

// The JIT makes the relaxed SIMD choices the interpreter makes on this
// CPU from the same features.
func TestJITRelaxedFeatures(t *testing.T) {
	if wjit.CPU.RelaxedFused != relaxedFused || wjit.CPU.RelaxedDotSigned != relaxedDotSigned ||
		wjit.CPU.RelaxedDotVNNI != relaxedDotVNNI {
		t.Fatalf("the JIT's relaxed choices %+v, the interpreter's fused %v, dot signed %v, VNNI %v",
			wjit.CPU, relaxedFused, relaxedDotSigned, relaxedDotVNNI)
	}
}

// Native code gives the interpreter's relaxed SIMD results under each
// choice amd64's CPUs make -- fused relaxed_madd or not, and each
// relaxed_dot_i8x16_i7x16_add_s -- the choice made for both.
func TestJITRelaxedChoices(t *testing.T) {
	if !JITAvailable() {
		t.Skip("no native code in this build")
	}
	cpu, fused, signed, vnni := wjit.CPU, relaxedFused, relaxedDotSigned, relaxedDotVNNI
	defer func() { wjit.CPU, relaxedFused, relaxedDotSigned, relaxedDotVNNI = cpu, fused, signed, vnni }()
	for _, c := range []struct{ fused, signed, vnni bool }{{false, false, false}, {true, false, false},
		{false, false, true}, {false, true, false}, {false, true, true}} {
		t.Run(fmt.Sprintf("fused=%v,signed=%v,vnni=%v", c.fused, c.signed, c.vnni), func(t *testing.T) {
			relaxedFused, relaxedDotSigned, relaxedDotVNNI = c.fused, c.signed, c.vnni
			wjit.CPU.RelaxedFused, wjit.CPU.RelaxedDotSigned, wjit.CPU.RelaxedDotVNNI = c.fused, c.signed, c.vnni
			_, calls, exits := simdBits(t, func(f simdFunc) bool {
				return strings.Contains(f.name, "relaxed") || strings.Contains(f.name, "qfm") ||
					strings.Contains(f.name, "i7x16")
			})
			t.Logf("%d calls compared; run in Go: %v", calls, exits)
		})
	}
}
