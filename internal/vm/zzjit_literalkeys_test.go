//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))

package vm

import (
	"fmt"
	"testing"
)

// Native code a runtime keeps reads the literals of code new for each request
// as it read the last request's: their layouts are the realm's, and the read's
// guards hold. Each request's own layouts had made the read polymorphic, then
// megamorphic, in its first few.
func TestJITKeptCodeReadsNewCodesLiterals(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	r := jitRuntimeForTest(t, Config{JIT: true})
	if _, err := r.Run(compileForTest(t, `
		function read(o) { return o.a + o.b }
		function each(o, n) { var s = 0; for (var i = 0; i < n; i++) s += read(o); return s }`)); err != nil {
		t.Fatal(err)
	}
	request := func(i int) string { return fmt.Sprintf(`each({a: %d, b: 2}, 200)`, i) }
	for i := range 20 {
		if _, err := r.Run(compileForTest(t, request(i))); err != nil {
			t.Fatal(err)
		}
	}
	before := r.JITStats()
	for i := 20; i < 220; i++ {
		v, err := r.Run(compileForTest(t, request(i)))
		if err != nil {
			t.Fatal(err)
		}
		if want := float64(200 * (i + 2)); v.Number() != want {
			t.Fatalf("request %d: got %v, want %v", i, v, want)
		}
	}
	after := r.JITStats()
	if after.Entries == before.Entries {
		t.Fatal("the requests ran no native code")
	}
	if n := after.Guards - before.Guards; n != 0 {
		t.Errorf("%d guards failed in 200 requests", n)
	}
}

// Native code is as right as the rest with two lists of keys on one root.
func TestJITLiteralKeysCollision(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	testLiteralKeysCollision(t, jitRuntimeForTest(t, Config{JIT: true}))
}
