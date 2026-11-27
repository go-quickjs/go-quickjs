//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64))

package vm

import (
	"bytes"
	"fmt"
	"testing"
)

// TestJITRegWorkspaceBackAfterPlace hands a compile's workspace back, at
// its install, only once its code is placed (letGoApart): the code it runs
// is its own, not the backend's bytes in the workspace -- which on arm64
// are the workspace's buffer until placed. Placed by the worker, apart,
// or by the install, from the backend's code: at the give-back the code
// is placed, and the backend's bytes and the workspace scribbled over and
// rewound change nothing the code runs, which goes on giving the
// interpreter's answers natively. A compile whose lowering its worker
// refused builds nothing, and is given back with no code.
func TestJITRegWorkspaceBackAfterPlace(t *testing.T) {
	prog := compileForTest(t, regBackgroundSrc)
	want := ""
	{
		r := newTestRuntime(t, Config{})
		t.Cleanup(r.Close)
		if _, err := r.Run(prog); err != nil {
			t.Fatal(err)
		}
		fn, _ := r.GetProp(Obj(r.Global()), r.Intern("work"))
		for range 6 {
			v, err := r.Call(fn, Undefined, []Value{Float(2000)})
			want += regNativeShow(r, v, err) + ";"
		}
	}
	for _, atInstall := range []bool{false, true} {
		t.Run(fmt.Sprintf("placeAtInstall=%v", atInstall), func(t *testing.T) {
			backs, works := 0, 0
			jitTestSwitch(t, func(c *jitTestConfig) {
				c.placeAtInstall = atInstall
				c.workspaceBack = func(j *jitRegJob) {
					backs++
					e := j.e
					if rt := j.root; rt != nil && rt.err != nil {
						// Its lowering refused on the worker (lowerRoot) --
						// norm's, of host operations alone, at its first
						// ask: nothing built, nothing placed.
						if j.placed != nil || e.code != nil || j.mc != nil {
							t.Errorf("%s refused at its lowering (%v) given back with code", j.cl.fn.Name, rt.err)
						}
						return
					}
					if j.placed != nil || e.code == nil || j.mc == nil {
						t.Errorf("%s given back with code placed %v, kept apart %v", j.cl.fn.Name, e.code != nil, j.placed != nil)
						return
					}
					if j.cl.fn.Name == "work" {
						works++
					}
					text := bytes.Clone(e.code.Text())
					for i := range j.mc.Bytes {
						j.mc.Bytes[i] = 0xff
					}
					j.w.SSA.Rewind()
					j.w.MIR.Rewind()
					if !bytes.Equal(e.code.Text(), text) {
						t.Errorf("%s's code changed with the backend's bytes", j.cl.fn.Name)
					}
				}
			})
			r := newTestRuntime(t, Config{JIT: true})
			t.Cleanup(r.Close)
			r.SetJITWarmUp(1, 1)
			r.SetJITBackground(true)
			r.jitStress.background, r.jitStress.checks = true, 1
			if _, err := r.Run(prog); err != nil {
				t.Fatal(err)
			}
			fn, _ := r.GetProp(Obj(r.Global()), r.Intern("work"))
			got := ""
			for range 6 {
				v, err := r.Call(fn, Undefined, []Value{Float(2000)})
				got += regNativeShow(r, v, err) + ";"
			}
			if got != want {
				t.Fatalf("got %q, want the interpreter's %q", got, want)
			}
			if backs == 0 || works == 0 || uint64(backs) != r.jit.bgSubmitted {
				t.Fatalf("%d workspaces given back, %d of work's, of %d compiles handed over", backs, works, r.jit.bgSubmitted)
			}
			if r.jit.ssaEntries == 0 {
				t.Fatal("no native code entered")
			}
		})
	}
}
