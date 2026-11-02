package regexp

import "testing"

// trimTail takes a trim's patterns, either way round and of any class, and
// no other: not under the m flag or sticky, not a run that ends in
// anything but $, not an alternative that is not anchored, not three.
func TestTrimTailOf(t *testing.T) {
	for _, c := range []struct {
		src, flags string
		ok         bool
	}{
		{`^\s+|\s+$`, "g", true}, {`\s+$|^\s+`, "g", true}, {`^[\s\xa0]+|[\s\xa0]+$`, "g", true},
		{`^\s*|\s*$`, "g", true}, {`^(a)(b)?|[ab]*$`, "g", true}, {`^\s*|\s*$`, "gi", true},
		{`^\s+|\s+$`, "gm", false}, {`^\s+|\s+$`, "gy", false}, {`^\s+|\s+(?=$)`, "g", false},
		{`^a|b$`, "g", false}, {`a|\s+$`, "g", false}, {`\s+$`, "g", false}, {`^\s+|\s+$|x`, "g", false},
	} {
		re, err := Compile(c.src, c.flags)
		if err != nil {
			t.Fatal(err)
		}
		if got := trimTailOf[struct{}](re.prog, re.flags&FlagSticky != 0).ok; got != c.ok {
			t.Errorf("/%s/%s: trimTail %v, want %v", c.src, c.flags, got, c.ok)
		}
	}
}
