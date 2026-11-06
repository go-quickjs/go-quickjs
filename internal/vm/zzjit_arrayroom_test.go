//go:build quickjs_jit

package vm

import (
	"fmt"
	"testing"
)

// A native caller uses a tree callee's array-capacity feedback with the
// collector idle and marking, and under exits and small tier budgets.
func TestJITArrayLiteralRoom(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	jitNoInlineForTest(t, "fill")
	for _, marking := range []bool{false, true} {
		t.Run(fmt.Sprint(marking), func(t *testing.T) {
			if marking {
				jitMarkingForTest(t)
			}
			r := jitRuntimeForTest(t, Config{JIT: true})
			r.jitStress = jitStressConfig{threshold: true, budget: 3, deopt: 7}
			if _, err := r.Run(compileForTest(t, `function fill(n) {
				var a = [];
				for (var i = 0; i < n; i++) a.push(i);
				return a;
			}`)); err != nil {
				t.Fatal(err)
			}
			cl := r.global.getOwn(r.atoms.intern("fill")).value().Object().fn().closure
			cl.jitRefused = true
			for range 3 {
				if _, err := r.Run(compileForTest(t, "fill(4096)")); err != nil {
					t.Fatal(err)
				}
			}
			if len(cl.arrays) != 1 || cl.arrays[0].room != 4096 {
				t.Fatal("callee did not learn its array's room")
			}
			v, err := r.Run(compileForTest(t, `function render() {
				var sum = 0;
				for (var round = 0; round < 30; round++) {
					var a = fill(4096);
					sum += a.length + a[0] + a[4095];
				}
				return sum;
			}
			render()`))
			if err != nil || !v.IsNumber() || v.Number() != 245730 {
				t.Fatalf("got %v, %v; want 245730", v, err)
			}
			if r.JITStats().SSAEntries == 0 {
				t.Fatal("caller never ran native code")
			}
			if len(r.arraySamples) != 0 || cl.arrays[0].sample != nil || cl.arrays[0].weak.Value() != nil {
				t.Fatal("native caller kept a sample after the turn")
			}
		})
	}
}
