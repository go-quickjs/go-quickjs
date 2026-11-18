package decoder

import "testing"

// TestModuleSizeLimit checks that a module is never taken past what its
// 32-bit offsets address, whatever its limits say, and that a lower limit
// is kept.
func TestModuleSizeLimit(t *testing.T) {
	if got := moduleSizeLimit(CoreLimits()); got != MaxModuleSize {
		t.Errorf("CoreLimits' module size %d, want %d", got, uint64(MaxModuleSize))
	}
	if got := moduleSizeLimit(&Limits{ModuleSize: 1<<63 - 1}); got != MaxModuleSize {
		t.Errorf("a limit of 2^63-1 takes %d, want %d", got, uint64(MaxModuleSize))
	}
	if got := moduleSizeLimit(V8Limits()); got != 1<<30 {
		t.Errorf("V8Limits' module size %d, want %d", got, 1<<30)
	}
	l := CoreLimits()
	l.ModuleSize = 7
	_, err := Decode([]byte("\x00asm\x01\x00\x00\x00"), l)
	if err == nil || err.Msg != "size > maximum module size (7): 8" {
		t.Errorf("Decode of 8 bytes under a limit of 7 = %v", err)
	}
}
