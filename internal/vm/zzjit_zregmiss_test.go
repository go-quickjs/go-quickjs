//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64))

package vm

import (
	"fmt"
	"testing"
)

// regMissSrc is strFuzz's seed 6 reduced: f inlines again, and the arrow
// again calls, whose destructuring of a literal leaves native code inside
// both, at every call under QJS_JIT_STRESS's budgets.
const regMissSrc = `
var log = []
function again(n, f) { for (let i = 0; i < n; i++) f(i) }
function f(k) {
	let s = 'a', saved = ''
	for (let i = 0; i < 3; i++) {
		again(2, () => { ({ a: saved } = { a: s + i }) })
		s += saved.length
	}
	log.push(s.length)
	return s
}
function run() {
	var out = []
	for (let r = 0; r < 24; r++) out.push(f(r))
	return out.join() + ':' + log.length
}
`

// TestJITRegMissedInlined: a register compile in the background whose build
// asked what its snapshot was not asked is built on the runtime's goroutine
// from feedback asked anew (jitRegInstall's missed). Built from the job's
// feedback, whose questions' effects the snapshot had taken back -- the
// closures the code inlines among them -- its keep named none of its
// inlined callees, and an exit inside one panicked: "an inlined register
// callee's closure is not its caller's" (jitRegInlined). Every compile is
// left a miss here (QJS_JIT_STRESS's miss), and the answers are the
// interpreter's, with the collector idle and marking.
func TestJITRegMissedInlined(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	for _, marking := range []bool{false, true} {
		t.Run(fmt.Sprintf("marking=%v", marking), func(t *testing.T) {
			if marking {
				jitMarkingForTest(t)
			}
			rts := regNativeRuntimes(t, regMissSrc)
			rts[0].jitStress = jitStressConfig{threshold: true, background: true, checks: 4, budget: 3, deopt: 7, miss: true}
			rts[0].jitCallThreshold, rts[0].jitICThreshold = 1, 1
			var got [2]string
			for k, r := range rts {
				fn, _ := r.GetProp(Obj(r.Global()), r.Intern("run"))
				for range 4 {
					v, err := r.Call(fn, Undefined, nil)
					got[k] += regNativeShow(r, v, err) + ";"
				}
			}
			// Node's.
			const round = "a234,a234,a234,a234,a234,a234,a234,a234,a234,a234,a234,a234,a234,a234,a234,a234,a234,a234,a234,a234,a234,a234,a234,a234"
			want := round + ":24;" + round + ":48;" + round + ":72;" + round + ":96;"
			if got[0] != got[1] || got[0] != want {
				t.Errorf("with the JIT %q, without %q, want %q", got[0], got[1], want)
			}
			if s := rts[0].jit; s.regSnapMisses == 0 {
				t.Error("no compile left as a miss")
			}
			if rts[0].RegNativeStats().Entries == 0 {
				t.Error("no native code entered")
			}
		})
	}
}
