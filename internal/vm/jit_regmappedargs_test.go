//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64))

package vm

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"testing"
)

// regMappedArgsSrc is earley-boyer's numeric comparisons, sloppy
// functions whose mapped arguments object aliases the parameter x they
// assign while they read the rest by index, and one whose assigned
// parameter is dead by name past the assignment, read only through
// arguments[0] after host exits; then mappedArgsSetup.
const regMappedArgsSrc = `
function sc_greater(x, y) {
	for (var i = 1; i < arguments.length; i++) { if (x <= arguments[i]) return false; x = arguments[i]; }
	return true;
}
function sc_lessEqual(x, y) {
	for (var i = 1; i < arguments.length; i++) { if (x > arguments[i]) return false; x = arguments[i]; }
	return true;
}
function sc_less(x, y) {
	for (var i = 1; i < arguments.length; i++) { if (x >= arguments[i]) return false; x = arguments[i]; }
	return true;
}
function sc_greaterEqual(x, y) {
	for (var i = 1; i < arguments.length; i++) { if (x < arguments[i]) return false; x = arguments[i]; }
	return true;
}
function deadParam(a, b) { a = b * 2; var s = 0; for (var i = 0; i < 3; i++) s += b.length | 0; return arguments[0] + "/" + s }
` + mappedArgsSetup

// A sloppy function's mapped arguments object aliases its parameters,
// which register code keeps in registers 2 on (ir.Program's ParamBase):
// native code passes Go's read of arguments[k] the parameters from there,
// as they are, and keeps them live at each read. It had passed registers
// 0 and 1 on, the function and its this, so that earley-boyer's
// sc_greater(3, 3) and sc_lessEqual(12, 0) answered true; and a parameter
// assigned, dead by name, an exit had left stale for the interpreter's
// read of arguments[0]. Each call's answer is pinned, natively from the
// start and by on-stack replacement, and the same as an ordinary call's,
// with the collector idle and marking; and the stack form's rounds agree
// with the interpreter under CI's stress.
func TestJITRegMappedArgumentsAssigned(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	nan := math.NaN()
	calls := []struct {
		name string
		args []float64
		want string
	}{
		{"sc_greater", []float64{3, 3}, "false"},
		{"sc_greater", []float64{12, 0}, "true"},
		{"sc_greater", []float64{0, 12}, "false"},
		{"sc_greater", []float64{2, 3}, "false"},
		{"sc_greater", []float64{3, 2}, "true"},
		{"sc_greater", []float64{1.5, 1.5}, "false"},
		{"sc_greater", []float64{1.5, 1}, "true"},
		{"sc_greater", []float64{2, 1.5}, "true"},
		{"sc_greater", []float64{3, 2, 1}, "true"},
		{"sc_greater", []float64{3, 2, 2}, "false"},
		{"sc_greater", []float64{3, 1, 2}, "false"},
		{"sc_greater", []float64{nan, 1}, "true"},
		{"sc_greater", []float64{1, nan}, "true"},
		{"sc_greater", []float64{5}, "true"},
		{"sc_lessEqual", []float64{12, 0}, "false"},
		{"sc_lessEqual", []float64{3, 3}, "true"},
		{"sc_lessEqual", []float64{0, 12}, "true"},
		{"sc_lessEqual", []float64{1.5, 1}, "false"},
		{"sc_lessEqual", []float64{1, 1.5}, "true"},
		{"sc_lessEqual", []float64{1, 2, 3}, "true"},
		{"sc_lessEqual", []float64{2, 2, 2}, "true"},
		{"sc_lessEqual", []float64{1, 3, 2}, "false"},
		{"sc_lessEqual", []float64{nan, 1}, "true"},
		{"sc_less", []float64{3, 3}, "false"},
		{"sc_less", []float64{2, 3}, "true"},
		{"sc_less", []float64{1, 1.5}, "true"},
		{"sc_less", []float64{1, 2, 2}, "false"},
		{"sc_less", []float64{1, 2, 3}, "true"},
		{"sc_greaterEqual", []float64{3, 3}, "true"},
		{"sc_greaterEqual", []float64{2, 3}, "false"},
		{"sc_greaterEqual", []float64{1.5, 2}, "false"},
		{"sc_greaterEqual", []float64{3, 3, 2}, "true"},
		{"sc_greaterEqual", []float64{3, 2, 3}, "false"},
		{"deadParam", []float64{1, 4}, "8/0"},
		{"deadParam", []float64{1.5, 2.25}, "4.5/0"},
		{"deadParam", []float64{7, 0.5, 9}, "1/0"},
	}
	for _, marking := range []bool{false, true} {
		t.Run(fmt.Sprintf("marking=%v", marking), func(t *testing.T) {
			if marking {
				jitMarkingForTest(t)
			}
			rts := regNativeRuntimes(t, regMappedArgsSrc)
			natives := 0
			for round := range 4 {
				for _, c := range calls {
					vals := make([]Value, len(c.args))
					shown := make([]string, len(c.args))
					for i, x := range c.args {
						vals[i] = Float(x)
						shown[i] = strconv.FormatFloat(x, 'g', -1, 64)
					}
					call := c.name + "(" + strings.Join(shown, ", ") + ")"
					var got [2]string
					for k, r := range rts {
						fn, _ := r.GetProp(Obj(r.Global()), r.Intern(c.name))
						if k == 0 {
							res := r.RegNativeCall(fn, vals, round%2 == 1)
							if res.Skipped != "" {
								t.Errorf("%s: not native: %s", call, res.Skipped)
							}
							if res.Entered >= 0 {
								natives++
							}
							got[k] = regNativeShow(r, res.Value, res.Err)
						} else {
							v, err := r.Call(fn, Undefined, vals)
							got[k] = regNativeShow(r, v, err)
						}
					}
					if got[0] != c.want || got[1] != c.want {
						t.Errorf("round %d, %s: natively %s, called %s, want %s", round, call, got[0], got[1], c.want)
					}
				}
			}
			if natives == 0 {
				t.Errorf("no call entered native code")
			}
		})
	}
	t.Run("stress", func(t *testing.T) {
		jitMarkingForTest(t)
		rts := regNativeRuntimes(t, regMappedArgsSrc)
		regStressForTest(rts[0], regStressConfigs[0])
		rounds := []string{"loop(30)", "loop(5)", "[less(1, 2, 3), less(3, 2), greater(5, 4, 3), greater(1, 2)].join()",
			"twice(1, 2) + twice(4) + twice(4, 5, 6)", "dyn(1) + dyn(1, 2, 3, 4, 5)", "dup(1, 2) + dup(1)",
			"closed(1, 2) + closed()", "strict(1) + strict(1, 2)", "kinds(1) + kinds()",
			"late(1, 2) + late(1, 2, 3, 4)", "[...gen(1, 5)].join() + [...gen()].join()",
			"[sc_greater(3, 3), sc_lessEqual(12, 0), sc_greater(3, 2, 1), sc_lessEqual(1, 2, 3), deadParam(1, '33')].join()"}
		for range 3 {
			for _, round := range rounds {
				prog := regBigCompile(t, "''+("+round+")")
				var got [2]string
				for k, r := range rts {
					v, err := r.Run(prog)
					got[k] = regNativeShow(r, v, err)
				}
				if got[0] != got[1] {
					t.Fatalf("%s: with the JIT %q, without %q", round, got[0], got[1])
				}
			}
		}
	})
}
