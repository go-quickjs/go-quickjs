package regexp

import (
	"slices"
	"testing"
	"unicode"
)

// TestFoldSets pins what opCharFold relies on, for every code point and both
// foldings: the characters foldSetOf lists for it are few enough to list, it
// is one of them, canonical takes each of them where it takes it, and they
// are the same set whichever of them is asked about -- so that every
// character canonical takes there is in it. It pins canonical's way with
// ASCII too, which is the general rule's answer.
func TestFoldSets(t *testing.T) {
	for _, unicodeFold := range []bool{false, true} {
		sets := map[rune][]rune{}
		for r := rune(0); r <= unicode.MaxRune; r++ {
			set, ok := foldSetOf(r, unicodeFold)
			if !ok {
				t.Fatalf("U+%04X (unicode %v): more characters than a foldSet holds", r, unicodeFold)
			}
			c := canonical(r, unicodeFold)
			if !set.has(r) {
				t.Fatalf("U+%04X (unicode %v): not in its own set %U", r, unicodeFold, set)
			}
			for _, x := range set {
				if canonical(x, unicodeFold) != c {
					t.Fatalf("U+%04X (unicode %v): %U in its set canonicalizes to %U, not %U",
						r, unicodeFold, x, canonical(x, unicodeFold), c)
				}
			}
			members := slices.Compact(slices.Sorted(slices.Values(set[:])))
			if prev, seen := sets[c]; seen && !slices.Equal(prev, members) {
				t.Fatalf("U+%04X (unicode %v): set %U, but another character of %U has %U",
					r, unicodeFold, members, c, prev)
			}
			sets[c] = members
		}
	}
	for r := rune(-1); r < 0x80; r++ {
		if got, want := canonical(r, true), foldCase(r); got != want {
			t.Errorf("canonical(%U, true) = %U, folding gives %U", r, got, want)
		}
		if got, want := canonical(r, false), upperCanonical(r); got != want {
			t.Errorf("canonical(%U, false) = %U, uppercasing gives %U", r, got, want)
		}
	}
}

// TestFoldedClassNoAlloc pins that a class under the i flag asks about a
// character past ASCII without allocating, which it did for each character
// of a subject, its fold orbit made a slice.
func TestFoldedClassNoAlloc(t *testing.T) {
	re, err := Compile("[a-z]+", "i")
	if err != nil {
		t.Fatal(err)
	}
	units := []uint16{0xE9, 0xE0, 0x3A3, 0x3C3, 'x'}
	if got, _ := re.Match(units, 0); !slices.Equal(got, []int{4, 5}) {
		t.Fatalf("match = %v, want [4 5]", got)
	}
	if n := testing.AllocsPerRun(100, func() { re.Match(units, 0) }); n > 1 {
		t.Errorf("%v allocations a match, want at most the result's", n)
	}
}
