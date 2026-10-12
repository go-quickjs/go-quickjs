package vm

import (
	"math/rand"
	"testing"
)

// A piece of a string that is not all ASCII is cut from its bytes where it
// can be, and must be the string its code units would make: the same bytes,
// the same flags, and no cut between the halves of a pair.
func TestSubstringOfNonASCIIMatchesUnits(t *testing.T) {
	rng := rand.New(rand.NewSource(2))
	pool := []uint16{'a', 'z', 0x7F, 0x80, 0x7FF, 0x800, 0x1D60, 0xFFFF, 0xD800, 0xDBFF, 0xDC00, 0xDFFF, 0xD83D, 0xDE00}
	for it := 0; it < 20000; it++ {
		n := 1 + rng.Intn(20)
		u := make([]uint16, n)
		for i := range u {
			u[i] = pool[rng.Intn(len(pool))]
		}
		s := fromUnits(u)
		if s.ascii {
			continue
		}
		for a := 0; a <= n; a++ {
			for b := a; b <= n; b++ {
				want := fromUnits(u[a:b])
				if a == b {
					want = emptyString
				}
				for _, got := range []*String{s.Substring(a, b), s.substringInto(new(String), a, b)} {
					if got.Go() != want.Go() || got.length != want.length || got.ascii != want.ascii ||
						got.endsHigh != want.endsHigh || got.startsLow != want.startsLow {
						t.Fatalf("%x [%d,%d): got %q %d %v %v %v want %q %d %v %v %v", u, a, b,
							got.Go(), got.length, got.ascii, got.endsHigh, got.startsLow,
							want.Go(), want.length, want.ascii, want.endsHigh, want.startsLow)
					}
				}
			}
		}
	}
}
