//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))

package vm

import (
	"strings"
	"testing"

	"github.com/go-quickjs/go-quickjs/internal/jit/mir"
)

// numberEqsSetup's function compares numbers, strictly and loosely, with
// numbers and with constants: NaN, which equals nothing, -0 and +0, which
// equal each other, a value an exit never needs boxed. Each comparison is
// of two float64s natively (ssa's numberEqualities), and its answer is the
// interpreter's.
const numberEqsSetup = `
	function numberEqs(n, k) {
		let s = 0, t = 0
		for (let i = 0; i < n; i++) {
			const x = i * 0.5 - 2, y = k - x
			const nan = (x - x) / (x - x)
			const z = x * -0
			if (x === y - 1) s += 1
			if (x == 8) s += 2
			if (x !== 3) s += 4
			if (nan === nan) s += 8
			if (nan == nan) s += 16
			if (nan !== nan) s += 32
			if (z === 0) s += 64
			if (z !== -0) s += 128
			if (x === Infinity * k) s += 256
			t += (x === 1.5) + (y != x) * 2
		}
		return s + ':' + t
	}
	function drive(n, k) { let r; for (let i = 0; i < 40; i++) r = numberEqs(n, k); return r }
`

// An equality of two numbers is a comparison of their float64s: none is
// left to OpEqTagged, which compares their boxes' words, and every answer
// is Node's.
func TestJITNumberEqualities(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	listings := map[string]string{}
	mir.Annotate = true
	JITCodeHook = func(f JITFunction, _ []byte, c *mir.Code) { listings[f.Name] = c.Listing }
	defer func() { JITCodeHook, mir.Annotate = nil, false }()
	rounds := []string{"drive(40, 5)", "drive(40, -3)", "drive(7, 0)"}
	r := jitAgreesForTest(t, numberEqsSetup, rounds...)
	l, ok := listings["numberEqs"]
	if !ok {
		t.Fatal("numberEqs was not compiled")
	}
	if strings.Contains(l, " eqtagged ") {
		t.Errorf("numberEqs compares numbers by their boxes:\n%s", l)
	}
	// Node's answers.
	for i, want := range []string{"3999:79", "3999:79", "701:12"} {
		v, err := r.Run(compileForTest(t, "''+"+rounds[i]))
		if err != nil {
			t.Fatal(err)
		}
		if got := v.String().Go(); got != want {
			t.Errorf("%s = %s, want %s", rounds[i], got, want)
		}
	}
	for _, c := range []jitStressConfig{{threshold: true, budget: 3}, {threshold: true, budget: 3, deopt: 7}} {
		jitAgreesForTestWith(t, c, numberEqsSetup, rounds...)
	}
}
