//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64))

package vm

import (
	"fmt"
	"testing"
)

// regTierDrive calls a function of regNativeSrc's many times from register
// code, and runs its loops long: what samples it hot at its calls
// (rhCallW's) and at its back edges (regBackEdge's).
const regTierDrive = `
function drive(f, n, k) { var r; for (var j = 0; j < k; j++) { try { r = f(n + j % 3) } catch (e) { r = "threw " + e.message } } return r }
`

// TestJITRegTierUp is tier-up from the register interpreter (step B of
// 3a-3, zzjit_zregtier.go): regNativeSrc's functions, in register form,
// called often and looping long in a runtime that runs the JIT, compiled
// from their register code once sampled hot and entered at their calls
// and by on-stack replacement at their loops, each answer compared with a
// runtime's without the JIT, the collector seen idle and marking.
func TestJITRegTierUp(t *testing.T) {
	for _, marking := range []bool{false, true} {
		t.Run(fmt.Sprintf("marking=%v", marking), func(t *testing.T) {
			if marking {
				jitMarkingForTest(t)
			}
			rts := regNativeRuntimes(t, regNativeSrc+regTierDrive)
			for _, name := range []string{"sum", "nested", "props", "strs", "counter", "thrower", "uncaught", "maker", "arr", "forin", "holes", "argsLen", "mixed",
				"calls", "methods", "callThrows", "callUncaught", "inst", "instThrows"} {
				for _, c := range []struct{ n, k int }{{3, 3000}, {1500, 4}} {
					var got [2]string
					for i, r := range rts {
						drive, _ := r.GetProp(Obj(r.Global()), r.Intern("drive"))
						fn, _ := r.GetProp(Obj(r.Global()), r.Intern(name))
						v, err := r.Call(drive, Undefined, []Value{fn, Float(float64(c.n)), Float(float64(c.k))})
						got[i] = regNativeShow(r, v, err)
					}
					if got[0] != got[1] {
						t.Errorf("%s(%d) %d times: with the JIT %q, without %q", name, c.n, c.k, got[0], got[1])
					}
				}
			}
			st := rts[0].RegNativeStats()
			t.Logf("%+v", st)
			if st.Entries == 0 {
				t.Errorf("no native code entered")
			}
		})
	}
}

// TestJITRegTierUpForIn is a for...in loop's step compiled at its first
// call, its register written only where the loop goes on (the builder's
// stepOn): each key pushed as the loop's turn reads it.
func TestJITRegTierUpForIn(t *testing.T) {
	rts := regNativeRuntimes(t, `function props(x) { var array = []; for (let p in x) array.push(p); return array.join() }
function drive() { return [props({}), props({x:1}), props({x:1, y:2}), props({x:1, y:2, zoom:3})].join(";") }`)
	for _, r := range rts {
		r.SetJITWarmUp(1, 1)
		r.interruptCounter = 1
	}
	var got [2]string
	for i, r := range rts {
		for range 3 {
			drive, _ := r.GetProp(Obj(r.Global()), r.Intern("drive"))
			v, err := r.Call(drive, Undefined, nil)
			got[i] = regNativeShow(r, v, err)
		}
	}
	if got[0] != got[1] || got[0] != ";x;x,y;x,y,zoom" {
		t.Errorf("with the JIT %q, without %q", got[0], got[1])
	}
	if st := rts[0].RegNativeStats(); st.Entries == 0 {
		t.Errorf("no native code entered")
	}
}
