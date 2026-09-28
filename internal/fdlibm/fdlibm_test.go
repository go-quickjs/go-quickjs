package fdlibm

import (
	"math"
	"testing"
)

// TestSpotValues pins answers V8 gives, which Go's math package does not:
// where it is far off -- a subnormal's logarithm, sinh and cosh near
// overflow -- and where it differs in the last place.
func TestSpotValues(t *testing.T) {
	for _, c := range []struct {
		name string
		got  float64
		want float64
	}{
		{"log(5e-324)", Log(5e-324), -744.4400719213812},
		{"log2(5e-324)", Log2(5e-324), -1074},
		{"log10(5e-324)", Log10(5e-324), -323.3062153431158},
		{"cosh(710)", Cosh(710), 1.1169973830808557e+308},
		{"sinh(-710)", Sinh(-710), -1.1169973830808557e+308},
		{"cosh(711)", Cosh(711), math.Inf(1)},
		{"sin(1e22)", Sin(1e22), -0.8522008497671888},
		{"cos(1e22)", Cos(1e22), 0.523214785395139},
		{"exp(1)", Exp(1), math.E},
		{"pow(10, 308)", PowC(10, 308), 1e308},
		{"pow(1.1, 1000)", PowC(1.1, 1000), 2.4699329180060256e+41},
		// 10^23 is midway between two doubles, and this is the one both
		// Arm's pow (and so glibc's) and Node's give.
		{"pow(10, 23)", PowC(10, 23), 1.0000000000000001e+23},
		{"pow(2, -1074)", PowC(2, -1074), 5e-324},
	} {
		if c.got != c.want && !(math.IsNaN(c.got) && math.IsNaN(c.want)) {
			t.Errorf("%s = %v, want %v", c.name, c.got, c.want)
		}
	}
}
