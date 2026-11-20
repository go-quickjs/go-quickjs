//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64))

package vm

import (
	"fmt"
	"math"
	"runtime"
	"strings"
	"testing"

	"github.com/go-quickjs/go-quickjs/internal/jit/ssa"
)

// TestJITRoundings runs Math.floor, Math.ceil, Math.trunc and Math.round
// natively over the edges of rounding -- the zeros, halves, the integers
// about 2^52, 2^63 and past, NaN, the infinities, subnormals -- with the
// CPU's single rounding instruction and, on amd64, without it (abi's
// Round off: SSE2's sequence, mir's roundSD), each exactly what Go's math
// gives, -0 included, and the code native.
func TestJITRoundings(t *testing.T) {
	two52, two63 := math.Ldexp(1, 52), math.Ldexp(1, 63)
	xs := []float64{0, math.Copysign(0, -1), 0.5, -0.5, 1.5, -1.5, 2.5, -2.5, 0.4, -0.4, 0.49999999999999994, -0.49999999999999994,
		two52 - 1, two52 + 1, -(two52 - 1), -(two52 + 1), two52 - 0.5, -(two52 - 0.5), two52, -two52, two63, -two63, 1e300, -1e300,
		math.NaN(), math.Inf(1), math.Inf(-1), 5e-324, -5e-324, 2.2250738585072014e-308, -2.2250738585072014e-308, 3.7, -3.7, 1e15 + 0.3}
	ops := []func(float64) float64{math.Floor, math.Ceil, math.Trunc, ssa.Round}
	var src strings.Builder
	src.WriteString("var xs = [")
	for i, x := range xs {
		if i > 0 {
			src.WriteString(", ")
		}
		switch {
		case math.IsNaN(x):
			src.WriteString("NaN")
		case math.IsInf(x, 1):
			src.WriteString("Infinity")
		case math.IsInf(x, -1):
			src.WriteString("-Infinity")
		case x == 0 && math.Signbit(x):
			src.WriteString("-0")
		default:
			fmt.Fprintf(&src, "%v", x)
		}
	}
	src.WriteString(`];
function rounds(x) { return [Math.floor(x), Math.ceil(x), Math.trunc(x), Math.round(x)]; }
function all() { var out = []; for (var i = 0; i < xs.length; i++) out.push(rounds(xs[i])); return out; }
for (var k = 0; k < 200; k++) all();
`)
	for _, round := range []bool{true, false} {
		t.Run(fmt.Sprintf("round=%v", round), func(t *testing.T) {
			jitTestSwitch(t, func(jc *jitTestConfig) { jc.round = switchOf(round || runtime.GOARCH == "arm64") })
			r := jitRuntimeForTest(t, Config{JIT: true})
			r.jitStress = jitStressConfig{threshold: true, budget: 3}
			v, err := r.Run(compileForTest(t, src.String()+"all()"))
			if err != nil {
				t.Fatal(err)
			}
			rows := v.Object().elemsOf()
			for i, x := range xs {
				got := rows[i].Object().elemsOf()
				for k, op := range ops {
					want := op(x)
					g := got[k].Number()
					if math.Float64bits(g) != math.Float64bits(want) && !(math.IsNaN(g) && math.IsNaN(want)) {
						t.Errorf("op %d of %v: %v, want %v", k, x, g, want)
					}
				}
			}
			jitNativeForTest(t, r, "all")
		})
	}
}
