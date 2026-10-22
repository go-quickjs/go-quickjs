package regexp

import (
	"sync"
	"testing"
	"unicode/utf16"
)

// searchContains is rawContains by its binary search alone.
func searchContains(s *charSet, r rune) bool {
	lo, hi := 0, len(s.ranges)-1
	for lo <= hi {
		mid := (lo + hi) / 2
		switch {
		case r < s.ranges[mid].lo:
			hi = mid - 1
		case r > s.ranges[mid].hi:
			lo = mid + 1
		default:
			return true
		}
	}
	return false
}

// TestBMPTableMatchesRanges checks a class's table against its ranges at
// every code point, for every property the package knows -- \p{...} and
// \P{...} -- and for classes of many ranges a pattern builds: the table
// answers exactly as the search did.
func TestBMPTableMatchesRanges(t *testing.T) {
	var sets []*charSet
	for name := range unicodeProperties {
		for _, neg := range []bool{false, true} {
			if s, ok := unicodeClass(name, neg); ok {
				sets = append(sets, s)
			}
		}
	}
	every := buildSet(false)
	for c := rune(0); c < 0x11000; c += 3 {
		every.addRange(c, c)
	}
	every.normalize()
	sets = append(sets, every, buildSet(false, charRange{0, 0x10FFFF}),
		buildSet(false, charRange{0x80, 0x80}, charRange{0xFFFF, 0x10000}, charRange{0x100, 0x13F}))
	for i := 0; i < bmpMinRanges; i++ {
		sets[len(sets)-1].addRange(0x200+rune(64*i), 0x200+rune(64*i+i))
	}
	sets[len(sets)-1].normalize()
	tables := 0
	for _, s := range sets {
		for c := rune(0x80); c <= 0x10FFFF; c++ {
			if c == 0x10000 && len(s.ranges) >= bmpMinRanges {
				tables++
			}
			if got, want := s.rawContains(c), searchContains(s, c); got != want {
				t.Fatalf("set of %d ranges at U+%04X: %v, want %v", len(s.ranges), c, got, want)
			}
			if c == 0x10100 {
				// Past the plane the search answers alone.
				c = 0x10FFF0
			}
		}
	}
	if tables < 100 {
		t.Fatalf("%d sets made tables", tables)
	}
}

// TestBMPTableShared checks that a property's table is made once, for
// every pattern that takes the property whole, and that a class made of
// it and more has its own.
func TestBMPTableShared(t *testing.T) {
	compile := func(src string) *charSet {
		re, err := Compile(src, "u")
		if err != nil {
			t.Fatal(err)
		}
		for _, c := range re.prog.classes {
			if len(c.ranges) >= bmpMinRanges {
				return c
			}
		}
		t.Fatalf("/%s/u has no class of many ranges", src)
		return nil
	}
	a, b, c := compile(`\p{Lu}`), compile(`x\p{Lu}+`), compile(`[\p{Lu}\d]`)
	for _, s := range []*charSet{a, b, c} {
		s.rawContains(0x100)
	}
	if a.bmp.Load() == nil || a.bmp.Load() != b.bmp.Load() {
		t.Errorf("\\p{Lu}'s patterns do not share a table")
	}
	if c.bmp.Load() == a.bmp.Load() || a.bmpFootprint() != 0 || c.bmpFootprint() == 0 {
		t.Errorf("a class of \\p{Lu} and more shares its table, or the footprints are wrong: %d, %d",
			a.bmpFootprint(), c.bmpFootprint())
	}
}

// TestBMPTableConcurrent matches a concurrent pattern of a large class from
// many goroutines at once, the first match of each making the table: under
// the race detector, what they share is made and read safely.
func TestBMPTableConcurrent(t *testing.T) {
	for _, src := range []string{`\p{L}+`, `[\p{L}\p{Nd}]+`, `\P{Lu}+`} {
		re, err := Compile(src, "u")
		if err != nil {
			t.Fatal(err)
		}
		re.Concurrent()
		subject := utf16.Encode([]rune("Ärger über Öl, ΣΊΣΥΦΟΣ and 東京"))
		var wg sync.WaitGroup
		errs := make(chan string, 16)
		for g := 0; g < 16; g++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for i := 0; i < 50; i++ {
					caps, err := re.Match(subject, 0)
					if err != nil || caps == nil || caps[1] <= caps[0] {
						errs <- src
						return
					}
				}
			}()
		}
		wg.Wait()
		close(errs)
		for e := range errs {
			t.Errorf("/%s/u failed to match", e)
		}
	}
}
