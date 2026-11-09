//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))

package vm

import (
	"testing"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
)

// A method read compiled before its cache knew a receiver, which the code
// takes for a string's (jitFeedback's StringProperty) -- at, a method
// String.prototype has -- and whose cache the interpreter fills with an
// object's shape before native code first leaves there: the code is
// compiled again for the shape at that exit (jitPolySeen), not left at
// every read from then on, which a loop doing enough besides kept from
// being judged slow.
func TestJITStringGuessFilledLater(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	r := jitRuntimeForTest(t, Config{JIT: true})
	jitDefaultsForTest(r)
	src := `
		function f(a, n) {
			let s = 0
			for (let i = 0; i < n; i++) { let w = i; for (let k = 0; k < 30; k++) w = (w * 31 + k) ^ (w >>> 3); s = (s + (w & 1) + a.at(1)) | 0 }
			return s
		}`
	if _, err := r.Run(compileForTest(t, src)); err != nil {
		t.Fatal(err)
	}
	cl := r.global.getOwn(r.atoms.intern("f")).value().Object().fn().closure
	// Compiled before any read: the cache knows no receiver.
	r.jitCallThreshold = 1
	if _, err := r.Run(compileForTest(t, "f([1, 2], 0)")); err != nil {
		t.Fatal(err)
	}
	e := r.jit.entryFor(cl.fn)
	if e == nil || e.ssa == nil {
		t.Fatalf("f has no code: %+v", e)
	}
	// The interpreter reads it, its code not entered.
	e.entrySlow, e.loopSlow = true, true
	if _, err := r.Run(compileForTest(t, "f([1, 2], 3)")); err != nil {
		t.Fatal(err)
	}
	e.entrySlow, e.loopSlow = false, false
	if c := &cl.ic[cl.fn.Code[jitPCOf(t, cl.fn, "get_prop_this")].B]; c.fills == 0 {
		t.Fatal("the interpreter did not fill the read's cache")
	}
	for range 40 {
		if _, err := r.Run(compileForTest(t, "f([1, 2], 50)")); err != nil {
			t.Fatal(err)
		}
	}
	hosts, entries := r.jit.hosts, r.jit.entries
	v, err := r.Run(compileForTest(t, "''+f([1, 2], 1000)"))
	if err != nil {
		t.Fatal(err)
	}
	want := New(Config{})
	t.Cleanup(func() { want.Close(); want.ReleaseClosed() })
	if _, err := want.Run(compileForTest(t, src)); err != nil {
		t.Fatal(err)
	}
	wv, err := want.Run(compileForTest(t, "''+f([1, 2], 1000)"))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := v.String().Go(), wv.String().Go(); got != want {
		t.Fatalf("f = %s, interpreter %s", got, want)
	}
	// Run natively, not given up on as slow for its exits.
	if e := r.jit.entryFor(cl.fn); e == nil || e.ssa == nil || e.entrySlow || r.jit.entries == entries {
		t.Fatalf("f did not run natively: slow %v loopSlow %v entries %d invalid %v deopts %d", e.entrySlow, e.loopSlow, r.jit.entries-entries, e.invalid, e.deopts)
	}
	if n := r.jit.hosts - hosts; n != 0 {
		t.Errorf("f left native code %d times in 1,000 reads", n)
	}
}

// jitPCOf is the PC of fn's first instruction of the opcode named op.
func jitPCOf(t *testing.T, fn *bytecode.Function, op string) int {
	t.Helper()
	for pc, in := range fn.Code {
		if in.Op.String() == op {
			return pc
		}
	}
	t.Fatalf("no %s in %s", op, fn.Name)
	return 0
}
