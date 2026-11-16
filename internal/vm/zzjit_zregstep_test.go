//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64))

package vm

import (
	"fmt"
	"testing"
)

// regStepJITSrc's loops end in step-and-branches -- an update, a += and
// a -- of a register bound, an immediate and an upvalue -- whose counter
// the body makes a string at the turn flip says, so that the step meets a
// string natively: the code leaves at the step (the fused instruction's
// pc), where nothing is committed, and the interpreter goes on from there.
const regStepJITSrc = `
function sum(n, flip) { var s = 0, i; for (i = 0; i < n; i++) { s += i; if (i === flip) i = String(i) } return s + "@" + typeof i + i }
function addk(n, flip) { var r = 0, i; for (i = 0; i < n; i += 3) { r++; if (i === flip) i = "" + i } return r + "@" + typeof i + i }
function subk(n, flip) { var r = 0, i; for (i = n; i > 0; i -= 3) { r++; if (i === flip) i = "" + i } return r + "@" + typeof i + i }
function down(n, flip) { var c = 0; while (--n >= 0) { c++; if (n === flip) n = String(n) } return c + "@" + typeof n + n }
function upv(a) { return function (n, flip) { var c = 0, u; for (u = 0; u <= a; u++) { c++; if (u === flip) u = "" + u } return c + "@" + typeof u + u } }
var up5 = upv(600);
`

// A step-and-branch is lowered as its step, falling to its partner: a
// counter the body turns into a string mid-loop leaves native code at the
// step, which the interpreter makes -- ++ of a string a number, += a
// concatenation -- and the loop goes on, with the interpreter's answers,
// natively and by on-stack replacement, the collector idle and marking.
func TestJITRegStepLeaves(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	for _, marking := range []bool{false, true} {
		t.Run(fmt.Sprintf("marking=%v", marking), func(t *testing.T) {
			if marking {
				jitMarkingForTest(t)
			}
			rts := regNativeRuntimes(t, regStepJITSrc)
			rts[0].jitStress = jitStressConfig{}
			natives := 0
			for i, flip := range []float64{-1, -1, 300, -1, 120, 300, -1} {
				for _, name := range []string{"sum", "addk", "subk", "down", "up5"} {
					var got, want string
					for k, r := range rts {
						fn, _ := r.GetProp(Obj(r.Global()), r.Intern(name))
						args := []Value{Float(900), Float(flip)}
						if k == 0 {
							res := r.RegNativeCall(fn, args, i%2 == 1)
							if res.Skipped != "" {
								t.Errorf("%s(%v): not native: %s", name, flip, res.Skipped)
							}
							if res.Entered >= 0 {
								natives++
							}
							got = regNativeShow(r, res.Value, res.Err)
						} else {
							v, err := r.Call(fn, Undefined, args)
							want = regNativeShow(r, v, err)
						}
					}
					if got != want {
						t.Errorf("%s(900, %v): natively %q, called %q", name, flip, got, want)
					}
				}
			}
			// Node's answers, without a flip and with one at 300.
			for name, want := range map[string][2]string{
				"sum":  {"404550@number900", "404550@number900"},
				"addk": {"300@number900", "101@string3003"},
				"subk": {"300@number0", "300@number0"},
				"down": {"900@number-1", "900@number-1"},
				"up5":  {"601@number601", "601@number601"},
			} {
				for j, flip := range []float64{-1, 300} {
					for k, r := range rts {
						fn, _ := r.GetProp(Obj(r.Global()), r.Intern(name))
						v, err := r.Call(fn, Undefined, []Value{Float(900), Float(flip)})
						if got := regNativeShow(r, v, err); got != want[j] {
							t.Errorf("runtime %d: %s(900, %v) = %q, want %q", k, name, flip, got, want[j])
						}
					}
				}
			}
			st := rts[0].RegNativeStats()
			if natives == 0 || st.Entries == 0 {
				t.Errorf("not run natively: %d calls, %+v", natives, st)
			}
		})
	}
}
