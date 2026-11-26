//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64)) && !quickjs_nativelayout

package vm

import (
	"runtime"
	"testing"
	"weak"
)

// A module's heap goes with its typed code: once the program lets the
// module go and the cache its code, the buffer is collected. The runtime
// had kept every heap its modules were linked to for its life, which held
// Octane's zlib's 128 MB buffer after the run. Its answers are Node's.
func TestJITAsmHeapNotKept(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	setEpoch := jitFlushForTest(t)
	r := jitRuntimeForTest(t, Config{JIT: true})
	if _, err := r.Run(compileForTest(t, `
		function Mod(stdlib, foreign, heap) {
			"use asm";
			var H32 = new stdlib.Int32Array(heap);
			function sum(n) {
				n = n | 0;
				var s = 0, i = 0;
				for (i = 0; (i | 0) < (n | 0); i = (i + 1) | 0) {
					H32[i << 2 >> 2] = i;
					s = (s + (H32[i << 2 >> 2] | 0)) | 0;
				}
				return s | 0;
			}
			return { sum: sum };
		}
		var heapProbe = new ArrayBuffer(1 << 20);
		var m = Mod(globalThis, {}, heapProbe);`)); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 20; i++ {
		// Node: m.sum(1000)
		if v, err := r.Run(compileForTest(t, `m.sum(1000)`)); err != nil || v.Number() != 499500 {
			t.Fatalf("m.sum(1000): %v, %v", v, err)
		}
	}
	if how := asmJITNative(r, "m", []string{"sum"})["sum"]; how != "native" {
		t.Fatalf("sum: %s", how)
	}
	if len(r.asmHeaps) == 0 {
		t.Fatal("no heap kept current")
	}
	probe := weak.Make(r.global.getOwn(r.atoms.intern("heapProbe")).value().Object())
	if _, err := r.Run(compileForTest(t, `m = null; heapProbe = null; Mod = null;`)); err != nil {
		t.Fatal(err)
	}
	// Two looks, jitFlushAge epochs apart, the module's code unused
	// between them.
	setEpoch(1000 + jitFlushAge)
	r.jit.flush()
	setEpoch(1000 + 2*jitFlushAge)
	r.jit.flush()
	for i := 0; i < 3; i++ {
		runtime.GC()
	}
	if probe.Value() != nil {
		t.Fatal("the module's buffer is alive")
	}
	// A buffer's change lets go of the heaps collected.
	r.bufferChanged()
	if len(r.asmHeaps) != 0 {
		t.Fatalf("%d heaps kept current, none alive", len(r.asmHeaps))
	}
}
