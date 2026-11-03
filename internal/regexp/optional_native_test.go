//go:build quickjs_jit && !android && !ios && (((linux || windows) && amd64) || (darwin && arm64))

package regexp

import (
	"slices"
	"testing"
)

// Native code, made from the same program, matches optionalCases as the
// matcher does: the same captures in the same steps, Node's.
func TestNativeOptionalAtoms(t *testing.T) {
	native := 0
	for _, c := range optionalCases {
		plain, nat, ok := compileBoth(t, c.src, c.flags)
		if !ok {
			t.Fatalf("/%s/ does not compile", c.src)
		}
		for start := 0; start <= len(c.s); start++ {
			sameOutcome(t, c.src, c.flags, c.s, start, plain, nat)
		}
		if got := searchASCII(nat, c.s, 0, nil); !slices.Equal(got.caps, c.want[0]) {
			t.Errorf("/%s/%s on %q natively: %v, want %v", c.src, c.flags, c.s, got.caps, c.want[0])
		}
		if nat.HasNative() {
			native++
		}
	}
	if native == 0 {
		t.Fatal("no pattern ran natively")
	}
	t.Logf("%d of %d patterns ran natively", native, len(optionalCases))
}
