package vm

import (
	"math"
	"math/rand"
	"testing"
)

// The number methods take their digits from strconv, which breaks a tie to
// even, and only a tie from all of a number's digits, which breaks it upward.
// Both must give what all the digits do, ties included.
func TestNumberFormattingMatchesExactDigits(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	var xs []float64
	for i := 0; i < 3000; i++ {
		// Any finite double.
		x := math.Float64frombits(rng.Uint64() &^ (1 << 63))
		if !math.IsNaN(x) && !math.IsInf(x, 0) && x != 0 {
			xs = append(xs, x)
		}
		// A few digits past the point, where toFixed is mostly asked.
		xs = append(xs, float64(rng.Intn(1e6))/float64(int(1)<<rng.Intn(12)))
		xs = append(xs, float64(rng.Int63n(1e12))*0.001)
	}
	// Ties in both directions, between the digits strconv would keep and
	// the next: binary fractions ending in a 5, and integers whose last
	// non-zero digit is.
	for k := 1; k < 400; k += 2 {
		for j := 1; j < 12; j++ {
			xs = append(xs, float64(k)/float64(int(1)<<j))
		}
		for _, p := range []float64{1, 10, 100, 1e5, 1e15} {
			xs = append(xs, float64(k*5)*p, float64(k*5+10)*p)
		}
	}
	xs = append(xs, 0.5, 1.5, 2.5, 1.25, 1.005, 1e-7, 5e-324, math.MaxFloat64,
		1<<63, 1<<63-1024, 1e21-65536, 0.000001, 123.456)

	for _, x := range xs {
		for n := 1; n <= 101; n++ {
			got, ge := significantDigits(x, n)
			want, we := significantDigitsExact(x, n)
			if got != want || ge != we {
				t.Fatalf("significantDigits(%v, %d) = %s e%d, want %s e%d", x, n, got, ge, want, we)
			}
		}
		if x >= 1e21 {
			continue
		}
		for d := 0; d <= 100; d++ {
			if got, want := formatFixed(x, d), formatFixedExact(x, d); got != want {
				t.Fatalf("formatFixed(%v, %d) = %s, want %s", x, d, got, want)
			}
		}
	}
}
