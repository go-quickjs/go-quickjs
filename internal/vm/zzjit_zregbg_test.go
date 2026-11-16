//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64))

package vm

import (
	"fmt"
	"testing"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
	"github.com/go-quickjs/go-quickjs/internal/compiler"
)

// regBackgroundSrc has calls, properties, a callee to inline and a loop:
// register code whose feedback the snapshot asks ahead.
const regBackgroundSrc = `
function Point(x, y) { this.x = x; this.y = y }
function norm(p) { return p.x * p.x + p.y * p.y }
function work(n) {
	let s = 0;
	for (let i = 0; i < n; i++) {
		const p = new Point(i, i + 1);
		s = (s + norm(p)) % 1000003;
	}
	return s;
}
`

// TestJITRegBackground compiles register code in the background: built
// on a worker from its feedback asked ahead (jitRegSnapshotOf), installed
// at a later check of the register tier's (jitDrain at jitRegHot). The
// answers are those of a runtime that compiles in the foreground, the
// compiles were handed over and installed, and the code then runs
// natively.
func TestJITRegBackground(t *testing.T) {
	for _, marking := range []bool{false, true} {
		t.Run(fmt.Sprintf("marking=%v", marking), func(t *testing.T) {
			if marking {
				jitMarkingForTest(t)
			}
			compiler.SetRegisterForm(true)
			prog := compileForTest(t, regBackgroundSrc)
			compiler.SetRegisterForm(false)
			SetRegTier(true)
			was := SetJITRegTier(true)
			t.Cleanup(func() { SetRegTier(bytecode.RegDefault()); SetJITRegTier(was) })
			var got [2]string
			var rts [2]*Runtime
			for i := range rts {
				r := New(Config{JIT: true})
				t.Cleanup(r.Close)
				rts[i] = r
				r.SetJITWarmUp(1, 1)
				r.SetJITBackground(i == 0)
				if i == 0 {
					// Each compile installed at the first check after it
					// was handed over, waited for: what is installed when
					// is the same from one run to the next.
					r.jitStress.background, r.jitStress.checks = true, 1
				} else {
					r.jitStress.background = false
				}
				if _, err := r.Run(prog); err != nil {
					t.Fatal(err)
				}
				fn, _ := r.GetProp(Obj(r.Global()), r.Intern("work"))
				var out string
				for range 6 {
					v, err := r.Call(fn, Undefined, []Value{Float(2000)})
					out += regNativeShow(r, v, err) + ";"
				}
				got[i] = out
			}
			if got[0] != got[1] {
				t.Fatalf("in the background %q, in the foreground %q", got[0], got[1])
			}
			r := rts[0]
			if r.jit.bgSubmitted == 0 {
				t.Fatal("no register compile was handed to the background")
			}
			if len(r.jit.regJobs) != 0 {
				t.Errorf("%d register compiles not installed", len(r.jit.regJobs))
			}
			compiled := false
			for key, e := range r.jit.regCache {
				if fn := key.Value(); fn != nil && fn.Name == "work" && e.code != nil {
					compiled = true
				}
			}
			if !compiled {
				t.Error("work's register code was not compiled and installed")
			}
			if r.jit.regSnapMisses != 0 {
				t.Errorf("%d register compiles asked what their snapshot was not asked", r.jit.regSnapMisses)
			}
			if r.jit.ssaEntries == 0 {
				t.Error("no native code entered")
			}
		})
	}
}
