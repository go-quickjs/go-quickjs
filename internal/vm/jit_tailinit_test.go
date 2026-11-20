//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64))

package vm

import (
	"fmt"
	"testing"
)

// regTailInitSrc's calls in tail position are of functions whose
// bindings past their parameters a call makes undefined: two that call
// each other, each reading its var bindings before it assigns them at
// the last call, one that calls itself so, and two that call each other
// with fewer arguments than parameters, the rest undefined, the binding
// one reads where the other had a parameter.
const regTailInitSrc = `
function ev(n, acc) { "use strict"; var seen; if (n === 0) return acc + ":" + typeof seen; seen = n * 2; var t = (acc + seen) | 0; return od(n - 1, t) }
function od(n, acc) { "use strict"; var u, w; if (n === 0) return acc + ":" + typeof u + typeof w; u = n; w = u + acc; return ev(n - 1, (w ^ u) | 0) }
function deepEvOd(n, k) { var s; for (var i = 0; i < k; i++) s = ev(n, 0) + "," + od(n, 1); return s }
function selfInit(n, acc) { "use strict"; var x; if (n === 0) return acc + ":" + typeof x; x = n; return selfInit(n - 1, (acc + x) | 0) }
function deepSelfInit(n, k) { var s; for (var i = 0; i < k; i++) s = selfInit(n, 0); return s }
function fewer(n, a, b, c) { "use strict"; if (n === 0) return a + ":" + typeof b + typeof c; return more(n - 1, a + 1) }
function more(n, a, b) { "use strict"; var z; if (n === 0) return a + ":" + typeof b + typeof z; z = b; return fewer(n - 1, a, n, n) }
function deepFewer(n, k) { var s; for (var i = 0; i < k; i++) s = fewer(n, 0, 1, 2) + "," + fewer(n + 1, 0, 1, 2); return s }
`

// TestJITRegTailInits: register code's call in tail position of another
// function's code (mir's windowTailJump) writes the callee's window's
// function, receiver and parameters, as a native call does, and jumps to
// the callee's tail entry, which makes what its call entry makes past the
// window -- the context's Stack, the VM's stack's top and high water, of
// its own frame's size -- and its bindings past its parameters undefined;
// and a call of the function itself (tailSelf) writes its
// window to its bindings, not its temporaries. The calls had written every
// register, temporaries too, and gone through a tail entry that made the
// frame again. Each answer is the interpreter's, with the collector idle
// and marking -- every binding read before it is assigned undefined, at
// the millionth call too -- and a million calls deep leave native code
// at none of the calls of each other's code.
func TestJITRegTailInits(t *testing.T) {
	t.Parallel()
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	want := map[string]string{
		"deepEvOd":     "-867267968:undefined,-867267967:undefinedundefined",
		"deepSelfInit": "1784293664:undefined",
		"deepFewer":    "500000:numbernumber,500001:undefinedundefined",
	}
	for _, marking := range []bool{false, true} {
		t.Run(fmt.Sprintf("marking=%v", marking), func(t *testing.T) {
			if marking {
				jitMarkingForTest(t)
			}
			rts := regNativeRuntimes(t, regTailInitSrc)
			r := rts[0]
			r.jitStress = jitStressConfig{}
			r.SetJITWarmUp(1, 1)
			run := func(name string, n, k float64) string {
				t.Helper()
				var got [2]string
				for i, r := range rts {
					fn, _ := r.GetProp(Obj(r.Global()), r.Intern(name))
					v, err := r.Call(fn, Undefined, []Value{Float(n), Float(k)})
					got[i] = regNativeShow(r, v, err)
				}
				if got[0] != got[1] {
					t.Errorf("%s(%v): with the JIT %q, without %q", name, n, got[0], got[1])
				}
				return got[0]
			}
			for _, name := range []string{"deepEvOd", "deepSelfInit", "deepFewer"} {
				// Until its code has learned what the guards met.
				for range 8 {
					run(name, 40, 50)
				}
				st := r.RegNativeStats()
				if got := run(name, 1e6, 1); got != want[name] {
					t.Errorf("%s(1e6) = %q, want %q", name, got, want[name])
				}
				st2 := r.RegNativeStats()
				exits := st2.HostOps["tail_call_w"] + st2.HostOps["tail_call_method_w"] - st.HostOps["tail_call_w"] - st.HostOps["tail_call_method_w"]
				if exits > 100 || st2.Entries == st.Entries {
					t.Errorf("%s: %d calls in tail position left native code, %d entries", name, exits, st2.Entries-st.Entries)
				}
			}
		})
	}
}
