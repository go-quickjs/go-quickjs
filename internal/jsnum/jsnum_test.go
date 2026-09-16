package jsnum

import (
	"math"
	"testing"
)

func TestFormatFloatMatchesJavaScript(t *testing.T) {
	// Expected values are what V8/QuickJS print for String(x).
	tests := []struct {
		in   float64
		want string
	}{
		{0, "0"},
		{math.Copysign(0, -1), "0"}, // negative zero prints as "0"
		{1, "1"},
		{-1, "-1"},
		{42, "42"},
		{3.14, "3.14"},
		{0.1, "0.1"},
		{1.0 / 3.0, "0.3333333333333333"},
		{math.NaN(), "NaN"},
		{math.Inf(1), "Infinity"},
		{math.Inf(-1), "-Infinity"},
		// The fixed/exponential switchover points.
		{1e20, "100000000000000000000"},
		{1e21, "1e+21"},
		{1e-6, "0.000001"},
		{1e-7, "1e-7"},
		{1.5e-7, "1.5e-7"},
		{-1e21, "-1e+21"},
		{5e-324, "5e-324"}, // smallest denormal
		{1.7976931348623157e308, "1.7976931348623157e+308"},
		{123456789012345680000, "123456789012345680000"},
		{0.000001, "0.000001"},
		{1e6, "1000000"},
		{2e-3, "0.002"},
	}
	for _, tt := range tests {
		if got := FormatFloat(tt.in); got != tt.want {
			t.Errorf("FormatFloat(%v) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestFormatFloatRoundTrips(t *testing.T) {
	// Whatever we print must parse back to the identical bit pattern, which is
	// the defining property of the shortest-representation algorithm.
	values := []float64{
		1, 0.1, 1.0 / 3, 1e21, 1e-7, 5e-324, 1.7976931348623157e308,
		2.2250738585072014e-308, 1234.5678, -9.87654321e-15,
	}
	for _, v := range values {
		s := FormatFloat(v)
		if got := ToNumber(s); got != v {
			t.Errorf("round trip of %v via %q gave %v", v, s, got)
		}
	}
}

func TestFormatRadix(t *testing.T) {
	tests := []struct {
		in    float64
		radix int
		want  string
	}{
		{255, 16, "ff"},
		{255, 2, "11111111"},
		{-255, 16, "-ff"},
		{0, 16, "0"},
		{35, 36, "z"},
		{0.5, 2, "0.1"},
		{0.25, 2, "0.01"},
		{10, 10, "10"},
		{math.NaN(), 16, "NaN"},
		{math.Inf(1), 2, "Infinity"},
	}
	for _, tt := range tests {
		if got := FormatRadix(tt.in, tt.radix); got != tt.want {
			t.Errorf("FormatRadix(%v, %d) = %q, want %q", tt.in, tt.radix, got, tt.want)
		}
	}
}

func TestToNumber(t *testing.T) {
	tests := []struct {
		in   string
		want float64
	}{
		{"", 0},
		{"   ", 0},
		{"42", 42},
		{" 42 ", 42},
		{"3.14", 3.14},
		{"0x1f", 31},
		{"0b101", 5},
		{"0o17", 15},
		{"Infinity", math.Inf(1)},
		{"-Infinity", math.Inf(-1)},
		{"+Infinity", math.Inf(1)},
		{"1e3", 1000},
		{".5", 0.5},
		{"5.", 5},
	}
	for _, tt := range tests {
		if got := ToNumber(tt.in); got != tt.want {
			t.Errorf("ToNumber(%q) = %v, want %v", tt.in, got, tt.want)
		}
	}
	// Strings JavaScript rejects, where Go's strconv would not.
	for _, s := range []string{"abc", "42abc", "1_000", "0x", "inf", "nan", "1p3", "--1", "0x1p2"} {
		if got := ToNumber(s); !math.IsNaN(got) {
			t.Errorf("ToNumber(%q) = %v, want NaN", s, got)
		}
	}
}

func TestParseFloatPrefix(t *testing.T) {
	tests := []struct {
		in   string
		want float64
	}{
		{"42abc", 42},
		{"3.14xyz", 3.14},
		{"  1e3end", 1000},
		{"Infinity!", math.Inf(1)},
		{"-Infinity!", math.Inf(-1)},
		{".5.5", 0.5},
		{"1e", 1}, // the malformed exponent is not consumed
		{"1e+", 1},
		{"12e5x", 1200000},
		// parseFloat is decimal-only: it reads "0" and stops at the 'x'.
		{"0x10", 0},
	}
	for _, tt := range tests {
		if got := ParseFloatPrefix(tt.in); got != tt.want {
			t.Errorf("ParseFloatPrefix(%q) = %v, want %v", tt.in, got, tt.want)
		}
	}
	for _, s := range []string{"", "abc", ".", "+"} {
		if got := ParseFloatPrefix(s); !math.IsNaN(got) {
			t.Errorf("ParseFloatPrefix(%q) = %v, want NaN", s, got)
		}
	}
}

func TestParseIntPrefix(t *testing.T) {
	tests := []struct {
		in    string
		radix int
		want  float64
	}{
		{"42", 0, 42},
		{"42abc", 0, 42},
		{"0x1f", 0, 31},
		{"0x1f", 16, 31},
		{"1f", 16, 31},
		{"ff", 16, 255},
		{"101", 2, 5},
		{"-42", 0, -42},
		{"+42", 0, 42},
		{"  42  ", 0, 42},
		{"z", 36, 35},
		{"08", 0, 8}, // no legacy octal in parseInt
	}
	for _, tt := range tests {
		if got := ParseIntPrefix(tt.in, tt.radix); got != tt.want {
			t.Errorf("ParseIntPrefix(%q, %d) = %v, want %v", tt.in, tt.radix, got, tt.want)
		}
	}
	for _, tt := range []struct {
		in    string
		radix int
	}{{"abc", 10}, {"", 0}, {"42", 1}, {"42", 37}, {"", 16}} {
		if got := ParseIntPrefix(tt.in, tt.radix); !math.IsNaN(got) {
			t.Errorf("ParseIntPrefix(%q, %d) = %v, want NaN", tt.in, tt.radix, got)
		}
	}
}

func TestToInt32(t *testing.T) {
	tests := []struct {
		in   float64
		want int32
	}{
		{0, 0},
		{42, 42},
		{-42, -42},
		{math.NaN(), 0},
		{math.Inf(1), 0},
		{math.Inf(-1), 0},
		{4294967296, 0},           // 2^32 wraps to 0
		{4294967297, 1},           // 2^32 + 1
		{2147483648, -2147483648}, // 2^31 wraps to INT32_MIN
		{-2147483649, 2147483647},
		{3.9, 3}, // truncates toward zero
		{-3.9, -3},
		// 1e21 is exactly representable, and 10^21 mod 2^32 = 3735027712,
		// which reinterpreted as a signed 32-bit value is -559939584.
		{1e21, -559939584},
	}
	for _, tt := range tests {
		if got := ToInt32(tt.in); got != tt.want {
			t.Errorf("ToInt32(%v) = %d, want %d", tt.in, got, tt.want)
		}
	}
}

func TestToUint32(t *testing.T) {
	tests := []struct {
		in   float64
		want uint32
	}{
		{0, 0},
		{42, 42},
		{-1, 4294967295},
		{4294967296, 0},
		{math.NaN(), 0},
		{-2147483648, 2147483648},
	}
	for _, tt := range tests {
		if got := ToUint32(tt.in); got != tt.want {
			t.Errorf("ToUint32(%v) = %d, want %d", tt.in, got, tt.want)
		}
	}
}

func TestToInteger(t *testing.T) {
	tests := []struct{ in, want float64 }{
		{3.9, 3},
		{-3.9, -3},
		{math.NaN(), 0},
		{math.Inf(1), math.Inf(1)},
		{0, 0},
	}
	for _, tt := range tests {
		if got := ToInteger(tt.in); got != tt.want {
			t.Errorf("ToInteger(%v) = %v, want %v", tt.in, got, tt.want)
		}
	}
}

func BenchmarkFormatFloatInteger(b *testing.B) {
	b.ReportAllocs()
	for b.Loop() {
		FormatFloat(1234567)
	}
}

func BenchmarkFormatFloatFractional(b *testing.B) {
	b.ReportAllocs()
	for b.Loop() {
		FormatFloat(3.141592653589793)
	}
}

func BenchmarkToNumber(b *testing.B) {
	b.ReportAllocs()
	for b.Loop() {
		ToNumber("3.141592653589793")
	}
}
