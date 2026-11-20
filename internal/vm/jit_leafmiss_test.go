//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64))

package vm

import "testing"

func TestJITRegLeafMiss(t *testing.T) {
	regWrongCheck(t, nativeLeafMissSource, []regWrongCase{
		{"leafMisses", []float64{100}, 3, "4950"},
	})
}

// In a runtime that runs the JIT, a call whose callee misses the site's
// cache is counted once by the feedback dispatcher, a leaf's as any
// other: the miss has the handler make it again, which counts it again,
// and takes back the first count, which had doubled the site's frequency.
func TestJITRegLeafMissCountedOnce(t *testing.T) {
	prog := compileForTest(t, nativeLeafMissSource)
	r := newTestRuntime(t, Config{JIT: true})
	t.Cleanup(r.Close)
	r.jitStress = jitStressConfig{}
	r.SetJITWarmUp(1<<16, 1<<16)
	if _, err := r.Run(prog); err != nil {
		t.Fatal(err)
	}
	fv, _ := r.GetProp(Obj(r.Global()), r.Intern("leafValue"))
	cl := fv.Object().fn().closure
	// Never compiled: the interpreter runs every call of it, and counts.
	cl.jitRegRefused = true
	misses, _ := r.GetProp(Obj(r.Global()), r.Intern("leafMisses"))
	if v, err := r.Call(misses, Undefined, []Value{Float(1000)}); err != nil || v.Number() != 499500 {
		t.Fatalf("got %v, %v", v, err)
	}
	if !regNativeOn {
		return
	}
	// The first call, made before the site has its counts, is not one.
	if c := cl.jitRegCounts; len(c) < 2 || c[0] < 990 || c[1] > c[0] || c[1]+1 < c[0] {
		t.Fatalf("counts %v: want the call site's the runs', about 1000", c)
	}
}
