package regexp

import (
	"strings"
	"testing"
	"unicode/utf16"
	"unsafe"
)

// A clone is the core it shares and a matcher of its own: two words. The
// clones of one pattern borrow the matcher its core lends while it is small
// and free; one a match makes large becomes the clone's, and goes with it;
// a pattern matched again while its matcher is busy gets another. Footprint
// counts the shared core, the lent matcher with it, once.
func TestClonesBorrowTheCoresMatcher(t *testing.T) {
	re, err := Compile(`(a|b)*c`, "")
	if err != nil {
		t.Fatal(err)
	}
	if n := unsafe.Sizeof(Regexp{}); n != 2*unsafe.Sizeof(uintptr(0)) {
		t.Fatalf("a Regexp is %d bytes", n)
	}
	var c1, c2 Regexp
	re.CloneInto(&c1)
	re.CloneInto(&c2)
	if c1.core != re.core || c2.core != re.core || c1.scratch != nil {
		t.Fatal("clones do not share the core")
	}
	short := utf16.Encode([]rune("xxabc"))
	if caps, err := c1.Match(short, 0); err != nil || len(caps) < 2 || caps[0] != 2 || caps[1] != 5 {
		t.Fatalf("short match: %v %v", caps, err)
	}
	lent := re.lent
	if lent == nil || c1.scratch != nil {
		t.Fatal("the first match did not leave the core a matcher to lend")
	}
	if _, err := c2.Match(short, 0); err != nil || re.lent != lent || c2.scratch != nil {
		t.Fatal("the second clone did not borrow the core's matcher")
	}

	// A busy lent matcher: the clone gets one of its own.
	lent.busy = true
	if caps, err := c2.Match(short, 0); err != nil || caps == nil || c2.scratch == nil || c2.scratch == lent {
		t.Fatalf("busy: %v %v, own %p", caps, err, c2.scratch)
	}
	lent.busy = false

	// A long subject grows the matcher past what a core lends: the clone
	// that grew it keeps it.
	long := utf16.Encode([]rune(strings.Repeat("ab", 5000) + "c"))
	if caps, err := c1.Match(long, 0); err != nil || caps == nil || caps[1] != len(long) {
		t.Fatalf("long match: %v %v", caps, err)
	}
	if c1.scratch != lent || re.lent != nil {
		t.Fatalf("the grown matcher stayed lent: own %p, lent %p", c1.scratch, re.lent)
	}
	p1, shared, own1, _ := c1.Footprint()
	p2, _, own2, _ := c2.Footprint()
	if p1 != p2 || p1 != unsafe.Pointer(re.core) || shared <= 0 || own1 <= own2 {
		t.Fatalf("footprints: %p %p shared %d own %d %d", p1, p2, shared, own1, own2)
	}
}
