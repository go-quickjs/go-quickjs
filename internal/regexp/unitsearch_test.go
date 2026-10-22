package regexp

import (
	"math/rand"
	"slices"
	"strings"
	"testing"
	"unicode/utf16"
)

// TestIndexUnit compares indexUnit, which reads four units at a time, with a
// search of one at a time: from every start of slices of every length up to
// some dozens, the unit sought anywhere or nowhere among units that differ
// from it in one lane's low or high byte, or are its neighbours, which a
// borrow in the word's test could take for it.
func TestIndexUnit(t *testing.T) {
	r := rand.New(rand.NewSource(3))
	for _, u := range []uint16{0, 1, 'x', 0x7f, 0x80, 0xff, 0x100, 0x7fff, 0x8000, 0xd83d, 0xfffe, 0xffff} {
		near := []uint16{u ^ 1, u ^ 0x100, u ^ 0x8000, u + 1, u - 1, u ^ 0xff, 0, 0xffff, 'a'}
		for n := 0; n < 40; n++ {
			for round := 0; round < 20; round++ {
				s := make([]uint16, n)
				for i := range s {
					s[i] = near[r.Intn(len(near))]
					if s[i] == u {
						s[i] = u ^ 2
					}
				}
				if n > 0 && round%3 != 0 {
					s[r.Intn(n)] = u
				}
				for pos := 0; pos <= n; pos++ {
					want := n
					for i := pos; i < n; i++ {
						if s[i] == u {
							want = i
							break
						}
					}
					if got := indexUnit(s, pos, u); got != want {
						t.Fatalf("indexUnit(%x, %d, %x) = %d, want %d", s, pos, u, got, want)
					}
				}
			}
		}
	}
}

// TestUnitSearchesAgree pins that a search over the code units of a subject
// past ASCII that looks for a literal, or for one first unit or either of
// two, four units at a time finds what one trying every position does,
// from each start, over subjects long enough for it to pass many over.
func TestUnitSearchesAgree(t *testing.T) {
	subjects := longSubjects(11, "é", "日本", "😀", "Needle", "xé")
	for i, s := range subjects {
		// Past ASCII, so that the subjects are matched by their units.
		subjects[i] = "é" + s + strings.Repeat("ü", i%5)
	}
	for _, p := range byteSearchPatterns {
		fast, err := Compile(p.src, p.flags)
		slow, err2 := Compile(p.src, p.flags)
		if err != nil || err2 != nil {
			t.Fatalf("/%s/%s: %v %v", p.src, p.flags, err, err2)
		}
		// The slow one tries every position: no first units, no literal.
		cp := *slow.prog
		cp.first, cp.lit, cp.litOnly = nil, nil, nil
		slow = &Regexp{core: &core{source: slow.source, flags: slow.flags, prog: &cp, groupCount: slow.groupCount, groupNames: slow.groupNames}}
		for _, s := range subjects {
			units := utf16.Encode([]rune(s))
			for _, start := range byteSearchStarts(len(units)) {
				want, err1 := slow.MatchCheckedInto(nil, units, start, nil)
				got, err2 := fast.MatchCheckedInto(nil, units, start, nil)
				if err1 != nil || err2 != nil || !slices.Equal(got, want) {
					t.Fatalf("/%s/%s on %q from %d: %v, %v; every position %v, %v", p.src, p.flags, s, start, got, err2, want, err1)
				}
				want, _ = slow.SearchCheckedInto(nil, units, start, nil)
				got, _ = fast.SearchCheckedInto(nil, units, start, nil)
				if !slices.Equal(got, want) {
					t.Fatalf("/%s/%s on %q from %d: a search %v; every position %v", p.src, p.flags, s, start, got, want)
				}
			}
		}
	}
}
