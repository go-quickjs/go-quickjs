package decoder

import "testing"

// TestLimitsMemory64 checks a memory64 of 300,000 pages against each set of
// limits: over V8's 262,144, within the JS API's 2^37-1 and the core
// specification's 2^48.
func TestLimitsMemory64(t *testing.T) {
	b := memory64Pages(300_000)
	const want = "initial memory size (300000 pages) is larger than implementation limit (262144 pages) @+12"
	if _, err := Decode(b, V8Limits()); err == nil || err.Error() != want {
		t.Errorf("V8Limits: got %v, want %s", err, want)
	}
	for name, l := range map[string]*Limits{"JSAPILimits": JSAPILimits(), "CoreLimits": CoreLimits()} {
		m, err := Decode(b, l)
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if got := m.Memories[0].Limits.Initial; got != 300_000 {
			t.Errorf("%s: initial %d pages, want 300000", name, got)
		}
	}
	// The JS API allows at most 100 memories, V8 100,000.
	many := wasmModule(sec(5, cat(uleb(101), make([]byte, 202))))
	if _, err := Decode(many, JSAPILimits()); err == nil ||
		err.Error() != "memory count of 101 exceeds internal limit of 100 @+11" {
		t.Errorf("JSAPILimits, 101 memories: got %v", err)
	}
	if _, err := Decode(many, V8Limits()); err != nil {
		t.Errorf("V8Limits, 101 memories: %v", err)
	}
	// The core specification has no limit on a memory64's maximum below 2^48
	// pages, nor on a table's size below what its index type addresses.
	core := wasmModule(sec(4, vec(cat(tFuncRef, 0, uleb(1<<32-1)))), sec(5, vec(cat(5, 1, uleb(1<<48)))))
	if _, err := Decode(core, CoreLimits()); err != nil {
		t.Errorf("CoreLimits: %v", err)
	}
	if _, err := Decode(core, V8Limits()); err == nil {
		t.Errorf("V8Limits accepted a memory64 of 2^48 pages")
	}
}
