//go:build quickjs_jit && !android && !ios && (((linux || windows) && amd64) || ((darwin || linux) && arm64))

package regexp

import (
	"strings"
	"testing"
	"unicode/utf16"
)

// TestFootprintCountsTheNativeStacks pins that Footprint counts the
// backtracking stack native code grew, over an ASCII string's bytes and over
// code units (KI-84).
func TestFootprintCountsTheNativeStacks(t *testing.T) {
	long := strings.Repeat("ab", 2000) + "c"
	for _, bytes := range []bool{true, false} {
		re, err := Compile("(a|b)*c", "")
		if err != nil {
			t.Fatal(err)
		}
		re.UseNative(testHost())
		clone := re.Clone()
		before := footprintSize(clone)
		if bytes {
			_, err = clone.MatchASCIICheckedInto(nil, long, 0, nil)
		} else {
			_, err = clone.MatchCheckedInto(nil, utf16.Encode([]rune(long)), 0, nil)
		}
		if err != nil {
			t.Fatal(err)
		}
		if bytes && !re.HasNative() || !bytes && !re.HasNativeUTF16() {
			t.Fatalf("bytes %v: no native code", bytes)
		}
		m := clone.scratch
		if m == nil {
			t.Fatal("the match left the clone no matcher")
		}
		r := m.native
		if bytes && m.bytes != nil {
			r = m.bytes.native
		}
		if r == nil || len(r.stack) <= 4*maxLentFrames {
			t.Fatalf("bytes %v: native code did not grow its stack", bytes)
		}
		if got, grown := footprintSize(clone)-before, 8*cap(r.stack); got < grown {
			t.Errorf("bytes %v: Footprint grew by %d bytes, the native stack by %d", bytes, got, grown)
		}
	}
}
