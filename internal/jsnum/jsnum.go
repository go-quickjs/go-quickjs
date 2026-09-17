// Package jsnum implements ECMAScript number parsing and formatting.
//
// JavaScript's number-to-string algorithm is the "shortest round-trippable
// decimal" rule, which Go's strconv already implements with 'g' and precision
// -1. The differences that remain are in presentation: JavaScript switches to
// exponential notation at different thresholds than Go, spells the exponent
// without zero padding, and has its own rules for negative zero and infinity.
// This package encodes those differences so the rest of the engine can treat
// number formatting as a solved problem.
package jsnum

import (
	"math"
	"strconv"
	"strings"
)

// FormatFloat renders v using the ECMAScript Number::toString algorithm in
// base 10.
// AppendFloat appends what FormatFloat would return.
//
// An integer below 2^53 is written digit by digit, which is what a serializer
// meets most: the great majority of the numbers in JSON text are small
// integers, and formatting one that way costs no string at all.
//
// The bound is where integers stop being exactly representable. Above it a
// double stands for a range of integers, and what must be printed is the
// shortest decimal that reads back as the same double rather than the exact
// value -- the two differ, so anything that large goes the long way.
func AppendFloat(dst []byte, v float64) []byte {
	if v == 0 {
		// Negative zero included, which prints as "0".
		return append(dst, '0')
	}
	const maxExactInt = 1 << 53
	if v > -maxExactInt && v < maxExactInt {
		if i := int64(v); float64(i) == v {
			return strconv.AppendInt(dst, i, 10)
		}
	}
	return append(dst, FormatFloat(v)...)
}

func FormatFloat(v float64) string {
	switch {
	case math.IsNaN(v):
		return "NaN"
	case v == 0:
		// Negative zero stringifies as "0", unlike strconv's "-0".
		return "0"
	case math.IsInf(v, 1):
		return "Infinity"
	case math.IsInf(v, -1):
		return "-Infinity"
	}
	if v == math.Trunc(v) && math.Abs(v) < 1e21 {
		// Integral values below 1e21 always print without a decimal point or
		// exponent, which strconv's 'f' gives us directly.
		return strconv.FormatFloat(v, 'f', -1, 64)
	}

	// Decide between fixed and exponential using the decimal exponent, which we
	// recover from the shortest representation rather than via log10 (log10 is
	// inexact near powers of ten and would misclassify values like 1e21).
	mant, exp := shortest(v)
	// n is the position of the decimal point relative to the digit string: the
	// spec's `n` in "s * 10^(n-k)".
	n := exp + len(mant)
	neg := ""
	if v < 0 {
		neg = "-"
	}
	k := len(mant)

	switch {
	case k <= n && n <= 21:
		// Integer with trailing zeros.
		return neg + mant + strings.Repeat("0", n-k)
	case 0 < n && n <= 21:
		return neg + mant[:n] + "." + mant[n:]
	case -6 < n && n <= 0:
		return neg + "0." + strings.Repeat("0", -n) + mant
	}
	// Exponential notation.
	e := n - 1
	sign := "+"
	if e < 0 {
		sign = "-"
		e = -e
	}
	if k == 1 {
		return neg + mant + "e" + sign + strconv.Itoa(e)
	}
	return neg + mant[:1] + "." + mant[1:] + "e" + sign + strconv.Itoa(e)
}

// shortest returns the shortest decimal digit string that round-trips to v,
// along with the base-10 exponent such that v = 0.digits * 10^(exp+len(digits)).
func shortest(v float64) (digits string, exp int) {
	s := strconv.FormatFloat(math.Abs(v), 'e', -1, 64)
	// s has the form "d.dddde±dd".
	epos := strings.IndexByte(s, 'e')
	mant := s[:epos]
	exp, _ = strconv.Atoi(s[epos+1:])
	mant = strings.Replace(mant, ".", "", 1)
	// Strip trailing zeros; strconv already does this, but be defensive so the
	// digit count used for placement is always minimal.
	mant = strings.TrimRight(mant, "0")
	if mant == "" {
		mant = "0"
	}
	// Convert "leading digit before the point" form to "all digits after the
	// point" form: 1.5e3 -> digits "15", exp 2.
	return mant, exp - len(mant) + 1
}

// FormatRadix renders v in the given radix (2..36), as Number.prototype.toString
// does for a non-10 radix.
func FormatRadix(v float64, radix int) string {
	if radix == 10 {
		return FormatFloat(v)
	}
	switch {
	case math.IsNaN(v):
		return "NaN"
	case math.IsInf(v, 1):
		return "Infinity"
	case math.IsInf(v, -1):
		return "-Infinity"
	case v == 0:
		return "0"
	}
	neg := v < 0
	v = math.Abs(v)

	intPart := math.Floor(v)
	frac := v - intPart

	var sb strings.Builder
	if intPart == 0 {
		sb.WriteByte('0')
	} else if intPart <= float64(math.MaxUint64) {
		sb.WriteString(strconv.FormatUint(uint64(intPart), radix))
	} else {
		// Beyond uint64 the integral part is not exactly representable in a
		// single conversion; emit digits from the most significant end by
		// repeatedly dividing, which stays exact because each step operates on
		// an exactly-representable float.
		var digits []byte
		for intPart >= 1 {
			rem := math.Mod(intPart, float64(radix))
			digits = append(digits, digitChar(int(rem)))
			intPart = math.Floor(intPart / float64(radix))
		}
		for i := len(digits) - 1; i >= 0; i-- {
			sb.WriteByte(digits[i])
		}
	}

	if frac > 0 {
		sb.WriteByte('.')
		// 1100 bits is enough to exhaust any float64 fraction in any radix;
		// in practice the loop exits far earlier when frac reaches zero.
		for i := 0; i < 1100 && frac > 0; i++ {
			frac *= float64(radix)
			d := int(math.Floor(frac))
			sb.WriteByte(digitChar(d))
			frac -= float64(d)
		}
	}
	if neg {
		return "-" + sb.String()
	}
	return sb.String()
}

func digitChar(d int) byte {
	if d < 10 {
		return byte('0' + d)
	}
	return byte('a' + d - 10)
}

// ParseFloatPrefix parses the longest prefix of s that forms a StrDecimalLiteral,
// as parseFloat does. It returns NaN if no prefix is numeric.
func ParseFloatPrefix(s string) float64 {
	s = strings.TrimLeft(s, whitespace)
	end := decimalPrefixEnd(s)
	if end == 0 {
		// Infinity is accepted by parseFloat but not by strconv.
		return math.NaN()
	}
	f, err := strconv.ParseFloat(s[:end], 64)
	if err != nil {
		// Overflow: ParseFloat reports ErrRange but still returns ±Inf, which
		// is what the spec wants.
		if ne, ok := err.(*strconv.NumError); ok && ne.Err == strconv.ErrRange {
			return f
		}
		return math.NaN()
	}
	return f
}

// decimalPrefixEnd returns the length of the longest numeric prefix of s,
// including the Infinity spelling.
func decimalPrefixEnd(s string) int {
	i := 0
	if i < len(s) && (s[i] == '+' || s[i] == '-') {
		i++
	}
	if strings.HasPrefix(s[i:], "Infinity") {
		return i + len("Infinity")
	}
	digitsStart := i
	for i < len(s) && isDigit(s[i]) {
		i++
	}
	if i < len(s) && s[i] == '.' {
		i++
		for i < len(s) && isDigit(s[i]) {
			i++
		}
	}
	if i == digitsStart || (i == digitsStart+1 && s[digitsStart] == '.') {
		return 0
	}
	// Only consume an exponent if it is well formed; otherwise the numeric
	// prefix ends before the 'e'.
	if i < len(s) && (s[i] == 'e' || s[i] == 'E') {
		j := i + 1
		if j < len(s) && (s[j] == '+' || s[j] == '-') {
			j++
		}
		if j < len(s) && isDigit(s[j]) {
			for j < len(s) && isDigit(s[j]) {
				j++
			}
			i = j
		}
	}
	return i
}

// ParseIntPrefix implements parseInt(s, radix).
func ParseIntPrefix(s string, radix int) float64 {
	s = strings.TrimLeft(s, whitespace)
	neg := false
	if len(s) > 0 && (s[0] == '+' || s[0] == '-') {
		neg = s[0] == '-'
		s = s[1:]
	}
	switch radix {
	case 0:
		if hasHexPrefix(s) {
			s, radix = s[2:], 16
		} else {
			radix = 10
		}
	case 16:
		// An explicit radix of 16 still permits the 0x prefix.
		if hasHexPrefix(s) {
			s = s[2:]
		}
	}
	if radix < 2 || radix > 36 {
		return math.NaN()
	}

	end := 0
	for end < len(s) && digitVal(s[end]) < radix {
		end++
	}
	if end == 0 {
		return math.NaN()
	}
	s = s[:end]

	// Fast path: values that fit in an int64 convert exactly.
	if v, err := strconv.ParseInt(s, radix, 64); err == nil {
		if neg {
			return float64(-v)
		}
		return float64(v)
	}
	// Slow path: accumulate in float64, accepting the precision loss the spec
	// permits for values beyond 2^53.
	var v float64
	for i := 0; i < len(s); i++ {
		v = v*float64(radix) + float64(digitVal(s[i]))
	}
	if neg {
		return -v
	}
	return v
}

// ToNumber implements the String-to-Number conversion used by the abstract
// ToNumber operation: the whole string must be numeric, and an empty or
// all-whitespace string is 0.
func ToNumber(s string) float64 {
	t := strings.Trim(s, whitespace)
	if t == "" {
		return 0
	}
	// Non-decimal integer literals are accepted here but not by parseFloat.
	if len(t) > 2 && t[0] == '0' {
		switch t[1] {
		case 'x', 'X':
			return parseRadixExact(t[2:], 16)
		case 'o', 'O':
			return parseRadixExact(t[2:], 8)
		case 'b', 'B':
			return parseRadixExact(t[2:], 2)
		}
	}
	switch t {
	case "Infinity", "+Infinity":
		return math.Inf(1)
	case "-Infinity":
		return math.Inf(-1)
	}
	// Reject anything strconv accepts but JavaScript does not: underscores,
	// "inf", "nan", and hex-float syntax.
	if strings.ContainsAny(t, "_pPxXnNiI") {
		return math.NaN()
	}
	f, err := strconv.ParseFloat(t, 64)
	if err != nil {
		if ne, ok := err.(*strconv.NumError); ok && ne.Err == strconv.ErrRange {
			return f
		}
		return math.NaN()
	}
	return f
}

// parseRadixExact converts a whole string of radix digits, returning NaN if any
// character is not a digit in that radix.
func parseRadixExact(s string, radix int) float64 {
	if s == "" {
		return math.NaN()
	}
	var v float64
	for i := 0; i < len(s); i++ {
		d := digitVal(s[i])
		if d >= radix {
			return math.NaN()
		}
		v = v*float64(radix) + float64(d)
	}
	return v
}

func hasHexPrefix(s string) bool {
	return len(s) > 2 && s[0] == '0' && (s[1] == 'x' || s[1] == 'X')
}

func digitVal(c byte) int {
	switch {
	case c >= '0' && c <= '9':
		return int(c - '0')
	case c >= 'a' && c <= 'z':
		return int(c-'a') + 10
	case c >= 'A' && c <= 'Z':
		return int(c-'A') + 10
	}
	return 99
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }

// whitespace is the set of code points ToNumber trims. It covers the ASCII
// space characters plus the Unicode space separators and the BOM.
const whitespace = " \t\n\v\f\r" +
	"\u00a0\u1680\u2000\u2001\u2002\u2003\u2004\u2005\u2006\u2007" +
	"\u2008\u2009\u200a\u2028\u2029\u202f\u205f\u3000\ufeff"

// ToInt32 implements the ToInt32 abstract operation.
func ToInt32(v float64) int32 {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return 0
	}
	// Truncate toward zero, then reduce modulo 2^32 into the signed range. The
	// uint32 conversion in Go is undefined for out-of-range floats, so the
	// modulo must happen in float64 first.
	return int32(uint32(int64(math.Mod(math.Trunc(v), 4294967296))))
}

// ToUint32 implements the ToUint32 abstract operation.
func ToUint32(v float64) uint32 { return uint32(ToInt32(v)) }

// ToInteger implements ToIntegerOrInfinity: truncation toward zero with NaN
// mapped to 0.
func ToInteger(v float64) float64 {
	if math.IsNaN(v) {
		return 0
	}
	return math.Trunc(v)
}
