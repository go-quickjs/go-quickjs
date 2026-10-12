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

// toFixed with up to 19 digits works in integers, which must give what Node
// does at the edges: ties taken upward, values that are not quite the decimal
// they print as, integers past 2**64, and digits past what a double holds.
func TestFormatFixedIntegerPath(t *testing.T) {
	for _, c := range []struct {
		x    float64
		d    int
		want string
	}{
		{0, 5, "0.00000"},
		{math.Copysign(0, -1), 5, "0.00000"},
		{0.5, 0, "1"},
		{2.5, 0, "3"},
		{-2.5, 0, "-3"},
		{1.005, 2, "1.00"},
		{0.045, 2, "0.04"},
		{9.995, 2, "9.99"},
		{5e-324, 19, "0.0000000000000000000"},
		{1e-7, 5, "0.00000"},
		{1e-7, 19, "0.0000001000000000000"},
		{0.9999999999999999, 15, "1.000000000000000"},
		{0.9999999999999999, 16, "0.9999999999999999"},
		{18446744073709551616, 0, "18446744073709551616"},
		{18446744073709551616, 19, "18446744073709551616.0000000000000000000"},
		{9007199254740992, 17, "9007199254740992.00000000000000000"},
		{999999999999999900000, 3, "999999999999999868928.000"},
		{6172839000000.1, 15, "6172839000000.099609375000000"},
		{6172839000000.1, 19, "6172839000000.0996093750000000000"},
	} {
		if got := formatFixed(c.x, c.d); got != c.want {
			t.Errorf("formatFixed(%v, %d) = %s, want %s", c.x, c.d, got, c.want)
		}
	}
}

// toPrecision and toExponential with up to 17 digits work in integers, which
// must give what Node does: ties taken upward (2.5, 125), values a hair
// under the decimal they print as (1.005, 0.045, 9.995), a carry into a new
// decade (9.995 to one digit, 0.9999999999999999), and values either side of
// 1 that the scaling reaches by division and by multiplication.
func TestSignificantIntegerPath(t *testing.T) {
	for _, c := range []struct {
		x         float64
		d         int
		exp, prec string
	}{
		{2.5, 0, "3e+0", ""},
		{2.5, 1, "2.5e+0", "3"},
		{125, 1, "1.3e+2", "1e+2"},
		{125, 2, "1.25e+2", "1.3e+2"},
		{1.005, 2, "1.00e+0", "1.0"},
		{1.005, 3, "1.005e+0", "1.00"},
		{1.005, 16, "1.0049999999999999e+0", "1.005000000000000"},
		{0.045, 1, "4.5e-2", "0.04"},
		{0.045, 16, "4.4999999999999998e-2", "0.04500000000000000"},
		{9.995, 1, "1.0e+1", "1e+1"},
		{9.995, 2, "9.99e+0", "10"},
		{9.995, 15, "9.994999999999999e+0", "9.99500000000000"},
		{0.9999999999999999, 15, "9.999999999999999e-1", "1.00000000000000"},
		{0.9999999999999999, 16, "9.9999999999999989e-1", "0.9999999999999999"},
		{1e-7, 16, "9.9999999999999995e-8", "1.000000000000000e-7"},
		{18446744073709551616, 15, "1.844674407370955e+19", "1.84467440737096e+19"},
		{18446744073709551616, 16, "1.8446744073709552e+19", "1.844674407370955e+19"},
		{0.001234565, 3, "1.235e-3", "0.00123"},
		{0.001234565, 16, "1.2345650000000000e-3", "0.001234565000000000"},
	} {
		if got := string(appendExponential(nil, c.x, c.d)); got != c.exp {
			t.Errorf("toExponential(%v, %d) = %s, want %s", c.x, c.d, got, c.exp)
		}
		if c.d == 0 {
			continue
		}
		if got := string(appendPrecision(nil, c.x, c.d)); got != c.prec {
			t.Errorf("toPrecision(%v, %d) = %s, want %s", c.x, c.d, got, c.prec)
		}
	}
}
