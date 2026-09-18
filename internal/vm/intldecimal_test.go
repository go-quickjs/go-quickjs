package vm

import "testing"

// Rounding is what a person would do with a pencil, which is not what the
// arithmetic would do on its own.
func TestDecimalRounding(t *testing.T) {
	for _, tc := range []struct {
		in    float64
		place int
		mode  string
		want  string
	}{
		{1.005, -2, "halfExpand", "1.01"},
		{1.005, -2, "halfEven", "1"},
		{1.25, -1, "halfExpand", "1.3"},
		{1.25, -1, "halfEven", "1.2"},
		{1.35, -1, "halfEven", "1.4"},
		{-1.25, -1, "halfExpand", "-1.3"},
		{-1.25, -1, "halfCeil", "-1.2"},
		{-1.25, -1, "halfFloor", "-1.3"},
		{1.101, -1, "ceil", "1.2"},
		{1.101, -1, "floor", "1.1"},
		{-1.101, -1, "ceil", "-1.1"},
		{-1.101, -1, "floor", "-1.2"},
		{1.1999, -1, "trunc", "1.1"},
		{-1.1999, -1, "trunc", "-1.1"},
		{1.1999, -1, "expand", "1.2"},
		{9.99, -1, "halfExpand", "10"},
		{0.5, 0, "halfEven", "0"},
		{1.5, 0, "halfEven", "2"},
		{99.5, 0, "halfExpand", "100"},
		{0.04, -1, "halfExpand", "0"},
		{123, 2, "halfExpand", "100"},
		{150, 2, "halfExpand", "200"},
		{150, 2, "halfEven", "200"},
		{250, 2, "halfEven", "200"},
	} {
		got := writeDecimal(decimalOf(tc.in).roundAt(tc.place, tc.mode))
		if got != tc.want {
			t.Errorf("%v rounded at %d, %s = %s, want %s", tc.in, tc.place, tc.mode, got, tc.want)
		}
	}
}

// Rounding to a number of significant digits, which is a different question
// from rounding to a number of decimal places.
func TestDecimalSignificant(t *testing.T) {
	for _, tc := range []struct {
		in   float64
		n    int
		want string
	}{
		{123.456, 4, "123.5"},
		{123.456, 2, "120"},
		{0.0001234, 2, "0.00012"},
		{999.9, 3, "1000"},
		{0, 3, "0"},
		{1234500000, 3, "1230000000"},
	} {
		if got := writeDecimal(decimalOf(tc.in).roundToDigits(tc.n, "halfExpand")); got != tc.want {
			t.Errorf("%v to %d digits = %s, want %s", tc.in, tc.n, got, tc.want)
		}
	}
}

// Rounding to a step that is not ten: a price to the nearest five cents, or a
// measurement to the nearest quarter.
func TestDecimalIncrement(t *testing.T) {
	for _, tc := range []struct {
		in        float64
		place     int
		increment int
		want      string
	}{
		{1.25, -2, 5, "1.25"},
		{1.26, -2, 5, "1.25"},
		{1.28, -2, 5, "1.3"},
		{1.625, -2, 5, "1.65"},
		{1.3125, -4, 25, "1.3125"},
		{1.311, -4, 25, "1.31"},
		{-1.28, -2, 5, "-1.3"},
		{12, 0, 5, "10"},
		{13, 0, 5, "15"},
	} {
		got := writeDecimal(decimalOf(tc.in).roundToMultiple(tc.place, tc.increment, "halfExpand"))
		if got != tc.want {
			t.Errorf("%v to a multiple of %d at %d = %s, want %s",
				tc.in, tc.increment, tc.place, got, tc.want)
		}
	}
}

// writeDecimal is the plainest form of a number, for the tests to read.
func writeDecimal(d decimal) string {
	whole, fraction := d.text(0, -1)
	out := whole
	if fraction != "" {
		out += "." + fraction
	}
	if d.negative && !d.isZero() {
		out = "-" + out
	}
	return out
}
