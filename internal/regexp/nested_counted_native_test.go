//go:build quickjs_jit && !android && !ios && (((linux || windows) && amd64) || ((darwin || linux) && arm64))

package regexp

import (
	"slices"
	"testing"
)

// Native code, made from the same program, matches nestedCountedCases as
// the matcher does: the same captures in the same steps, Node's. Its
// counted loop's entry keeps the count an earlier entry left, as the
// matcher's does (KI-81).
func TestNativeNestedCountedQuantifiers(t *testing.T) {
	native := 0
	for _, c := range nestedCountedCases {
		plain, nat, ok := compileBoth(t, c.src, c.flags)
		if !ok {
			t.Fatalf("/%s/%s does not compile", c.src, c.flags)
		}
		units := wtf8ToUnits(c.s)
		for start := 0; start <= len(units); start++ {
			sameOutcomeUnits(t, c.src, c.flags, units, start, plain, nat)
		}
		if isASCII(c.s) {
			for start := 0; start <= len(c.s); start++ {
				sameOutcome(t, c.src, c.flags, c.s, start, plain, nat)
			}
			if got := searchASCII(nat, c.s, 0, nil); got.err != nil || !slices.Equal(got.caps, c.want[0]) {
				t.Errorf("/%s/%s on %q natively: %v %v, want %v", c.src, c.flags, c.s, got.caps, got.err, c.want[0])
			}
		}
		if got := searchUTF16(nat, units, 0, nil); got.err != nil || !slices.Equal(got.caps, c.want[0]) {
			t.Errorf("/%s/%s on %q natively over UTF-16: %v %v, want %v", c.src, c.flags, c.s, got.caps, got.err, c.want[0])
		}
		if nat.HasNative() || nat.HasNativeUTF16() {
			native++
		}
	}
	if native == 0 {
		t.Fatal("no pattern ran natively")
	}
	t.Logf("%d of %d patterns ran natively", native, len(nestedCountedCases))
}
