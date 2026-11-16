package regexp

import (
	"math/rand"
	"slices"
	"testing"
	"unicode/utf16"
)

// A pattern of the i flag that is nothing but ASCII characters is searched
// for as its text, case aside (foldLit), and finds what the matcher finds
// from every start: over code units, over ASCII bytes, with a pooled
// matcher and a lent one, and every match of a global search -- among
// subjects of its pieces in both cases, near misses, and the characters
// past ASCII that fold to ASCII ones only with the unicode flag.
func TestFoldLiteralAgrees(t *testing.T) {
	patterns := []struct {
		src, flags string
		fold       bool
	}{
		{`needle`, "i", true}, {`needle`, "gi", true}, {`NeEd`, "im", true}, {`a-b`, "i", true},
		{`k`, "i", true}, {`s`, "gi", true}, {`x1Y`, "i", true}, {`#`, "i", true}, {`ab`, "iy", true},
		{`ſ`, "i", false}, {`k`, "iu", false}, {`a.b`, "i", false}, {`needle`, "", false},
	}
	alphabet := []string{"n", "N", "e", "E", "d", "D", "l", "L", "needle", "NEEDLE", "nEeDlE", "needl", "a", "A", "-", "b", "B",
		"k", "K", "\u212A", "s", "S", "\u017F", "x", "X", "1", "y", "Y", "#", " ", "\u00e9"}
	r := rand.New(rand.NewSource(1))
	var subjects [][]uint16
	for i := 0; i < 400; i++ {
		var s string
		for j := r.Intn(14); j >= 0; j-- {
			s += alphabet[r.Intn(len(alphabet))]
		}
		subjects = append(subjects, utf16.Encode([]rune(s)))
	}
	for _, p := range patterns {
		re, err := Compile(p.src, p.flags)
		if err != nil {
			t.Fatalf("/%s/%s: %v", p.src, p.flags, err)
		}
		if (re.prog.foldLit != nil) != p.fold {
			t.Errorf("/%s/%s: a fold literal %v, want %v", p.src, p.flags, re.prog.foldLit != nil, p.fold)
		}
		plain, _ := Compile(p.src, p.flags)
		plain.prog = &program{}
		*plain.prog = *re.prog
		plain.prog.foldLit = nil
		for _, s := range subjects {
			ascii := true
			for _, u := range s {
				ascii = ascii && u < 0x80
			}
			str := string(utf16.Decode(s))
			for start := 0; start <= len(s); start++ {
				got, err1 := re.Match(s, start)
				want, err2 := plain.Match(s, start)
				if err1 != nil || err2 != nil || !slices.Equal(got, want) {
					t.Fatalf("/%s/%s on %q from %d: %v, %v; the matcher's %v, %v", p.src, p.flags, str, start, got, err1, want, err2)
				}
				if !ascii {
					continue
				}
				got, err1 = re.MatchASCIIInto(nil, str, start, nil)
				if err1 != nil || !slices.Equal(got, want) {
					t.Fatalf("/%s/%s on bytes %q from %d: %v, %v; the matcher's %v", p.src, p.flags, str, start, got, err1, want)
				}
				got, err1 = re.MatchASCIICheckedInto(nil, str, start, nil)
				if err1 != nil || !slices.Equal(got, want) {
					t.Fatalf("/%s/%s lent on %q from %d: %v, %v; the matcher's %v", p.src, p.flags, str, start, got, err1, want)
				}
			}
			if ascii && p.flags != "iy" {
				nop := func() error { return nil }
				got, _, err1 := MatchAllASCIIInto[struct{}](re, nil, str, nop, nop)
				want, _, err2 := MatchAllASCIIInto[struct{}](plain, nil, str, nop, nop)
				if err1 != nil || err2 != nil || !slices.Equal(got, want) {
					t.Fatalf("/%s/%s every match in %q: %v; the matcher's %v", p.src, p.flags, str, got, want)
				}
			}
		}
	}
}
