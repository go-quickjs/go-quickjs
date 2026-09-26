package vm

import (
	"math"
	"math/big"
	"strconv"
	"strings"
)

// A number written out in decimal, which is how it has to be held before it can
// be rounded the way a person rounds.
//
// Rounding in binary and then writing the result is not the same thing as
// rounding what is written: 1.005 is not exactly 1.005 in binary, and whether
// it goes up or down depends on which side of it the binary fell. Every engine
// answers these questions the same way, by taking the shortest decimal that
// reads back as the same number and rounding that. So that is what is held
// here: the digits, and where the point falls among them.
type decimal struct {
	negative bool
	// digits are the significant ones, without leading or trailing zeros. An
	// empty string is zero.
	digits string
	// exp is where the point falls: the value is 0.digits times ten to the exp.
	exp int
}

// decimalOf is the shortest decimal that reads back as this number, which is
// the one every engine rounds.
func decimalOf(x float64) decimal {
	if x == 0 || math.IsNaN(x) || math.IsInf(x, 0) {
		return decimal{negative: math.Signbit(x)}
	}
	text := strconv.FormatFloat(x, 'e', -1, 64)
	d, _ := parseDecimal(text)
	return d
}

// parseDecimal reads a number as it is written: digits, a point, an exponent.
func parseDecimal(s string) (decimal, bool) {
	var d decimal
	if s == "" {
		return d, false
	}
	switch s[0] {
	case '-':
		d.negative, s = true, s[1:]
	case '+':
		s = s[1:]
	}
	mantissa, exponent, hasExp := strings.Cut(s, "e")
	if !hasExp {
		mantissa, exponent, hasExp = strings.Cut(s, "E")
	}
	whole, fraction, _ := strings.Cut(mantissa, ".")
	if whole == "" && fraction == "" {
		return d, false
	}
	for _, part := range []string{whole, fraction} {
		for i := 0; i < len(part); i++ {
			if part[i] < '0' || part[i] > '9' {
				return d, false
			}
		}
	}
	d.digits = whole + fraction
	d.exp = len(whole)
	if hasExp {
		n, err := strconv.Atoi(exponent)
		if err != nil {
			return decimal{}, false
		}
		d.exp += n
	}
	d.normalize()
	return d, true
}

// normalize strips the zeros that say nothing: the ones in front of the first
// digit and the ones after the last.
func (d *decimal) normalize() {
	i := 0
	for i < len(d.digits) && d.digits[i] == '0' {
		i++
	}
	d.digits = d.digits[i:]
	d.exp -= i
	d.digits = strings.TrimRight(d.digits, "0")
	if d.digits == "" {
		d.exp = 0
	}
}

func (d decimal) isZero() bool { return d.digits == "" }

// roundAt rounds to a whole multiple of ten to the place, the way the mode
// asks: 1.25 at place -1 is 1.3 rounding half up and 1.2 rounding half even.
func (d decimal) roundAt(place int, mode string) decimal {
	if d.isZero() {
		return d
	}
	keep := d.exp - place
	if keep >= len(d.digits) {
		return d
	}
	kept, dropped := "", d.digits
	switch {
	case keep > 0:
		kept, dropped = d.digits[:keep], d.digits[keep:]
	case keep < 0:
		// The place is above the first digit, so the whole number is dropped,
		// with the zeros in between counting as dropped digits too.
		dropped = strings.Repeat("0", -keep) + d.digits
	}
	if !takesAStep(mode, d.negative, kept, dropped) {
		out := decimal{negative: d.negative, digits: kept, exp: place + len(kept)}
		out.normalize()
		return out
	}
	raised, carried := increment(kept)
	out := decimal{negative: d.negative, digits: raised, exp: place + len(raised)}
	if carried {
		out.exp = place + len(raised)
	}
	out.normalize()
	return out
}

// roundToDigits rounds to a number of significant digits rather than to a
// number of decimal places.
func (d decimal) roundToDigits(n int, mode string) decimal {
	if d.isZero() || n <= 0 {
		return d
	}
	return d.roundAt(d.exp-n, mode)
}

// roundToMultiple rounds to a whole multiple of an increment of tens: 1.25 to
// the nearest 0.05 is 1.25, and to the nearest 0.5 is 1.5.
func (d decimal) roundToMultiple(place, increment int, mode string) decimal {
	if increment <= 1 {
		return d.roundAt(place, mode)
	}
	if d.isZero() {
		return d
	}
	// The number as a whole count of the place, and whatever is left over
	// below it.
	keep := d.exp - place
	if keep < 0 {
		keep = 0
	}
	kept := d.digits
	dropped := ""
	if keep < len(d.digits) {
		kept, dropped = d.digits[:keep], d.digits[keep:]
	} else {
		kept = d.digits + strings.Repeat("0", keep-len(d.digits))
	}
	n, ok := new(big.Int).SetString("0"+kept, 10)
	if !ok {
		return d.roundAt(place, mode)
	}
	inc := big.NewInt(int64(increment))
	quotient, remainder := new(big.Int).QuoRem(n, inc, new(big.Int))

	// Whether what is left over is more than half a step, asked exactly: both
	// sides are multiplied out so that the leftover below the place counts.
	scale := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(len(dropped))), nil)
	over, _ := new(big.Int).SetString("0"+dropped, 10)
	left := new(big.Int).Mul(remainder, scale)
	left.Add(left, over)
	left.Lsh(left, 1)
	cmp := left.Cmp(new(big.Int).Mul(inc, scale))
	var up bool
	switch {
	case left.Sign() == 0:
		up = false
	case cmp == 0:
		up = takesAStep(mode, d.negative, quotient.String(), "5")
	case cmp > 0:
		up = takesAStep(mode, d.negative, quotient.String(), "6")
	default:
		up = takesAStep(mode, d.negative, quotient.String(), "4")
	}
	if up {
		quotient.Add(quotient, big.NewInt(1))
	}
	quotient.Mul(quotient, inc)

	out := decimal{negative: d.negative, digits: quotient.String()}
	out.exp = len(out.digits) + place
	out.normalize()
	return out
}

// takesAStep reports whether what is left over takes the kept digits up a step.
// A mode says what to do with a half, and half of them say it by where the
// number lies rather than by what it is.
func takesAStep(mode string, negative bool, kept, dropped string) bool {
	if dropped == "" || strings.Trim(dropped, "0") == "" {
		return false
	}
	first := dropped[0]
	half := first == '5' && strings.Trim(dropped[1:], "0") == ""
	above := first > '5' || (first == '5' && !half)

	switch mode {
	case "ceil":
		return !negative
	case "floor":
		return negative
	case "expand":
		return true
	case "trunc":
		return false
	case "halfCeil":
		if half {
			return !negative
		}
	case "halfFloor":
		if half {
			return negative
		}
	case "halfTrunc":
		if half {
			return false
		}
	case "halfEven":
		if half {
			last := byte('0')
			if kept != "" {
				last = kept[len(kept)-1]
			}
			return (last-'0')%2 == 1
		}
	default: // halfExpand
		if half {
			return true
		}
	}
	return above
}

// increment adds one to a string of digits, reporting whether it grew.
func increment(s string) (string, bool) {
	if s == "" {
		return "1", true
	}
	out := []byte(s)
	for i := len(out) - 1; i >= 0; i-- {
		if out[i] < '9' {
			out[i]++
			return string(out), false
		}
		out[i] = '0'
	}
	return "1" + string(out), true
}

// text writes the number out with at least one digit before the point and
// between min and max after it, which is the form the pieces are cut from.
func (d decimal) text(minFrac, maxFrac int) (whole, fraction string) {
	if d.isZero() {
		return "0", strings.Repeat("0", minFrac)
	}
	switch {
	case d.exp <= 0:
		whole, fraction = "0", strings.Repeat("0", -d.exp)+d.digits
	case d.exp >= len(d.digits):
		whole = d.digits + strings.Repeat("0", d.exp-len(d.digits))
	default:
		whole, fraction = d.digits[:d.exp], d.digits[d.exp:]
	}
	for len(fraction) > minFrac && strings.HasSuffix(fraction, "0") {
		fraction = fraction[:len(fraction)-1]
	}
	for len(fraction) < minFrac {
		fraction += "0"
	}
	if maxFrac >= 0 && len(fraction) > maxFrac {
		fraction = fraction[:maxFrac]
	}
	return whole, fraction
}
