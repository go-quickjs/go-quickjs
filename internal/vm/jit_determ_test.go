//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64))

package vm

import (
	"testing"
)

// keyAtomsHoldForTest reports whether r's table of key strings (keyAtoms)
// has an entry for a flat string of text.
func keyAtomsHoldForTest(r *Runtime, text string) bool {
	for i := range r.keyAtoms {
		if s := r.keyAtoms[i].s; s != nil && s.left == nil && s.s == text {
			return true
		}
	}
	return false
}

// A keyed store of a string constant, this["constructor"] = v, is compiled
// as the store of the key its site met, with no test of the key: the test
// had asked the VM's table of key strings, whose entries a string's
// address slots, and once other strings had evicted the constant's, the
// store took the way of a key the site had never met, left native code
// and was compiled again for what that taught -- at a call that varied
// from run to run with where the strings were, Octane's Typescript's
// BlockIntrinsics among them, so that two runs of one build compiled
// differently from there on.
func TestJITKeyedStoreConstantKeyEvicted(t *testing.T) {
	events := jitTierTraceForTest(t)
	r, run := jitTierRuntimeForTest(t, 8, `function B() { this.a = undefined; this["constructor"] = undefined; }
		function flood(n) { var o = {}; for (var i = 0; i < n; i++) o["k" + i] = i; return 0; }
		function f() { var b = new B(); return b.a === undefined && b.hasOwnProperty("constructor") ? 1 : 0; }`)
	r.jitStress = jitStressConfig{}
	for range 20 {
		run("f()")
	}
	run("flood(4096)")
	if keyAtomsHoldForTest(r, "constructor") {
		t.Fatal(`"constructor" is still in the table of key strings: the flood evicted nothing, and the test tests nothing`)
	}
	for range 20 {
		run("f()")
	}
	for _, e := range *events {
		if e.Func.Name == "B" && (e.Deopt || e.Compiles != 0) {
			t.Fatalf("B left its code or was compiled again after its key's entry was evicted:\n%s", jitTierLines(*events))
		}
	}
}
