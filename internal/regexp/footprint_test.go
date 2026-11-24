package regexp

import (
	"strings"
	"testing"
	"unicode/utf16"
	"unsafe"
)

// footprintSize is all Footprint says a pattern takes.
func footprintSize(re *Regexp) int {
	_, prog, own, _ := re.Footprint()
	return prog + own
}

// TestFootprintCountsTheMatcherOverBytes pins that Footprint counts the
// stacks a match of an ASCII subject grew, which the matcher over bytes
// holds, as it counts those a match of code units grew (KI-84): uncounted,
// 500 patterns each matched once against 4,000 "a"s took a process past a
// 32 MB limit to 287 MB.
func TestFootprintCountsTheMatcherOverBytes(t *testing.T) {
	long := strings.Repeat("ab", 2000) + "c"
	for _, bytes := range []bool{true, false} {
		re, err := Compile("(a|b)*c", "")
		if err != nil {
			t.Fatal(err)
		}
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
		m := clone.scratch
		if m == nil {
			t.Fatal("the match left the clone no matcher")
		}
		grown := cap(m.stack)*int(unsafe.Sizeof(frame{})) + cap(m.trail)*int(unsafe.Sizeof(capWrite{}))
		if a := m.bytes; bytes {
			if a == nil {
				t.Fatal("the match left no matcher over bytes")
			}
			grown = cap(a.stack)*int(unsafe.Sizeof(frame{})) + cap(a.trail)*int(unsafe.Sizeof(capWrite{}))
		}
		if grown < 2000*int(unsafe.Sizeof(frame{})) {
			t.Fatalf("bytes %v: the match grew its stacks to %d bytes only", bytes, grown)
		}
		if got := footprintSize(clone) - before; got < grown {
			t.Errorf("bytes %v: Footprint grew by %d bytes, the stacks by %d", bytes, got, grown)
		}
	}
}
