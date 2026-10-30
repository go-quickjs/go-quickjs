package jsnum

import (
	"strconv"
	"strings"
)

// maxDecimalDigits is the most significant digits a decimal literal is
// read with. A double nearest a decimal is decided by its first 768
// significant digits and whether any after them is not a zero: the
// midpoints between doubles, at which rounding turns, have at most 767. So
// the digits after the 800th are only looked at, as V8 looks at those
// after its 772nd (kMaxSignificantDigits): strconv reads every digit,
// which in a string of millions of them took longer than V8 by half again.
const maxDecimalDigits = 800

// ParseFloat is strconv.ParseFloat(s, 64), which a decimal literal of more
// than maxDecimalDigits characters is given in shortDecimal's shorter form.
func ParseFloat(s string) (float64, error) {
	if len(s) > maxDecimalDigits {
		if short, ok := shortDecimal(s); ok {
			s = short
		}
	}
	return strconv.ParseFloat(s, 64)
}

// shortDecimal writes s, a decimal literal -- a sign, digits with a point
// among or around them, and an exponent, as strconv.ParseFloat reads it --
// with its first maxDecimalDigits significant digits, a 1 after them when a
// digit it leaves out is not a zero, and the exponent that keeps the point
// where it was: a string that strconv rounds to the same double. ok is
// false for anything else.
func shortDecimal(s string) (short string, ok bool) {
	i := 0
	neg := false
	if i < len(s) && (s[i] == '+' || s[i] == '-') {
		neg = s[i] == '-'
		i++
	}
	intStart := i
	i = digitsEnd(s, i)
	intEnd, fracStart, fracEnd := i, i, i
	if i < len(s) && s[i] == '.' {
		fracStart = i + 1
		fracEnd = digitsEnd(s, fracStart)
		i = fracEnd
	}
	if intEnd == intStart && fracEnd == fracStart {
		return "", false
	}
	// The exponent, held to a bound far past any double's, beyond which
	// the value is infinite or zero however the digits move it; an int64,
	// which an int is not on 386.
	const expBound = 1 << 30
	var exp int64
	if i < len(s) && (s[i] == 'e' || s[i] == 'E') {
		i++
		expNeg := false
		if i < len(s) && (s[i] == '+' || s[i] == '-') {
			expNeg = s[i] == '-'
			i++
		}
		end := digitsEnd(s, i)
		if end == i {
			return "", false
		}
		for ; i < end && exp < expBound; i++ {
			exp = exp*10 + int64(s[i]-'0')
		}
		i = end
		if expNeg {
			exp = -exp
		}
	}
	if i != len(s) {
		return "", false
	}
	// The significant digits are the integer part's from its first that is
	// not a zero, then the fraction's, or, with no such digit in the
	// integer part, the fraction's from its first; point is how many of
	// them come before the point, which is negative for the zeros after it.
	var first, second string
	var point int64
	if lead := zerosEnd(s, intStart, intEnd); lead < intEnd {
		first, second = s[lead:intEnd], s[fracStart:fracEnd]
		point = int64(intEnd - lead)
	} else {
		lead := zerosEnd(s, fracStart, fracEnd)
		if lead == fracEnd {
			// Nothing but zeros: zero, of the sign written.
			if neg {
				return "-0", true
			}
			return "0", true
		}
		first = s[lead:fracEnd]
		point = -int64(lead - fracStart)
	}
	var b strings.Builder
	b.Grow(maxDecimalDigits + 32)
	if neg {
		b.WriteByte('-')
	}
	b.WriteString("0.")
	sticky := false
	if len(first) >= maxDecimalDigits {
		b.WriteString(first[:maxDecimalDigits])
		sticky = !allZeros(first[maxDecimalDigits:]) || !allZeros(second)
	} else {
		b.WriteString(first)
		rest := maxDecimalDigits - len(first)
		if len(second) > rest {
			b.WriteString(second[:rest])
			sticky = !allZeros(second[rest:])
		} else {
			b.WriteString(second)
		}
	}
	if sticky {
		b.WriteByte('1')
	}
	b.WriteByte('e')
	b.WriteString(strconv.FormatInt(point+exp, 10))
	return b.String(), true
}

// digitsEnd is the end of the run of decimal digits in s that starts at
// i, found eight bytes at a time.
func digitsEnd(s string, i int) int {
	for ; i+8 <= len(s); i += 8 {
		x := load64(s, i)
		// A byte below '0' sets its high bit in x-0x30 (and may borrow from
		// the bytes above it), and a byte above '9' sets it in x+0x46 or, past
		// 0xb9, in x-0x30: a word of digits sets none, nor carries.
		if ((x-0x3030303030303030)|(x+0x4646464646464646))&0x8080808080808080 != 0 {
			break
		}
	}
	for i < len(s) && s[i]-'0' < 10 {
		i++
	}
	return i
}

// zerosEnd is where the run of '0' in s[i:end] that starts at i ends.
func zerosEnd(s string, i, end int) int {
	for ; i+8 <= end && load64(s, i) == 0x3030303030303030; i += 8 {
	}
	for i < end && s[i] == '0' {
		i++
	}
	return i
}

// allZeros reports whether s, of digits, is nothing but '0'.
func allZeros(s string) bool { return zerosEnd(s, 0, len(s)) == len(s) }

// load64 is the eight bytes of s at i, the first the lowest.
func load64(s string, i int) uint64 {
	_ = s[i+7]
	return uint64(s[i]) | uint64(s[i+1])<<8 | uint64(s[i+2])<<16 | uint64(s[i+3])<<24 |
		uint64(s[i+4])<<32 | uint64(s[i+5])<<40 | uint64(s[i+6])<<48 | uint64(s[i+7])<<56
}
