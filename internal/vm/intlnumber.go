package vm

import (
	"math"
	"strconv"
	"strings"

	"github.com/go-quickjs/go-quickjs/internal/icu"
)

// Number formatting for Intl.NumberFormat, and for the toLocaleString methods
// that are defined in terms of it.
//
// The shape of a formatted number is the same everywhere -- a sign, some
// grouped digits, a decimal separator, some more digits, and whatever the style
// puts around them -- so what varies by locale is which characters those are
// and where the separators fall. All of that is data; this is the arithmetic.

// numberOptions is a resolved Intl.NumberFormat.
type numberOptions struct {
	locale *icu.Locale
	// requested is the tag that was asked for, which resolvedOptions reports
	// when the data for it was found.
	requested string

	style           string // decimal, percent, currency, unit
	currency        string
	currencyDisplay string
	unit            string
	unitDisplay     string
	notation        string // standard, compact, scientific, engineering
	compactDisplay  string
	signDisplay     string // auto, always, never, exceptZero, negative
	useGrouping     bool

	minInt           int
	minFrac, maxFrac int
	// minSig and maxSig are the significant-digit counts, which take the place
	// of the fraction counts when they are given.
	minSig, maxSig int
}

// numberPiece is one part of a formatted number, in the shape formatToParts
// hands back.
type numberPiece struct {
	kind  string
	value string
}

// formatNumber writes a number the way the locale and the options ask.
func (o *numberOptions) format(x float64) string {
	var b strings.Builder
	for _, piece := range o.parts(x) {
		b.WriteString(piece.value)
	}
	return b.String()
}

// parts is the whole of the formatting: everything else here is arithmetic on
// its way to this list.
func (o *numberOptions) parts(x float64) []numberPiece {
	l := o.locale
	if math.IsNaN(x) {
		return []numberPiece{{"nan", l.NaN}}
	}

	negative := math.Signbit(x)
	x = math.Abs(x)
	if o.style == "percent" {
		x *= 100
	}

	if math.IsInf(x, 0) {
		return o.wrap([]numberPiece{{"infinity", l.Infinity}},
			o.pattern(negative, o.wants(negative, x)))
	}

	// Compact notation divides the number down and writes what it was divided
	// by after it -- thousands in English, ten-thousands in Japanese, which is
	// part of the language rather than of the arithmetic.
	suffix := ""
	if o.notation == "compact" {
		long := o.compactDisplay == "long"
		scaled, divisor, form := l.Compact(x, long)
		// Rounding can push a number into the next step -- 999.9 thousand is
		// a million -- and then it is written there instead.
		if rounded := roundCompact(scaled); rounded >= 1000 && divisor >= 1 {
			scaled, _, form = l.Compact(rounded*divisor, long)
		}
		x = scaled
		// Which form the word takes follows the number as it will be written,
		// not the one it was before it was rounded.
		suffix = form.SuffixFor(l.Cardinal.Category(roundCompact(scaled)))
	}

	compact := o.notation == "compact"
	whole, fraction := o.digitsOf(x, compact)
	// The sign follows what was written rather than what was given: a value
	// that rounds to zero is written as zero, and zero takes no sign.
	if isZeroText(whole, fraction) {
		x = 0
	}
	pieces := o.groupDigits(whole, compact)
	if fraction != "" {
		pieces = append(pieces, numberPiece{"decimal", l.Decimal})
		pieces = append(pieces, numberPiece{"fraction", fraction})
	}
	if suffix != "" {
		pieces = append(pieces, numberPiece{"compact", suffix})
	}
	return o.wrap(pieces, o.pattern(negative, o.wants(negative, x)))
}

// wants reports whether a sign is written for this value.
func (o *numberOptions) wants(negative bool, x float64) bool {
	switch o.signDisplay {
	case "never":
		return false
	case "always":
		return true
	case "exceptZero":
		return x != 0
	case "negative":
		return negative && x != 0
	}
	return negative
}

// pattern is what goes around the digits: the style's affixes, and the sign
// where this language puts it -- which is in front of the currency symbol in
// most places and behind it in Switzerland.
func (o *numberOptions) pattern(negative, sign bool) string {
	l := o.locale
	positive, minus := "{0}", l.DecimalNegative
	switch o.style {
	case "percent":
		positive, minus = l.PercentPattern, l.PercentNegative
	case "currency":
		positive, minus = l.CurrencyPattern, l.CurrencyNegative
		if o.currencyDisplay == "code" || o.currencyDisplay == "name" {
			// A code is a word rather than a mark, and a word is not written
			// against the number.
			positive, minus = spaceCurrency(positive), spaceCurrency(minus)
		}
	}
	switch {
	case !sign:
		return positive
	case negative:
		return minus
	}
	// A plus goes where the minus would have gone.
	if strings.Contains(minus, l.Minus) {
		return strings.Replace(minus, l.Minus, "+", 1)
	}
	return "+" + positive
}

// spaceCurrency puts a space between a currency written as a word and the
// number, which a symbol does not need.
func spaceCurrency(pattern string) string {
	pattern = strings.ReplaceAll(pattern, "\u00a4{0}", "\u00a4\u00a0{0}")
	return strings.ReplaceAll(pattern, "{0}\u00a4", "{0}\u00a0\u00a4")
}

// wrap puts the pattern around the digits, calling each piece of it what it
// is: the currency, the percent sign, the sign, or a literal.
func (o *numberOptions) wrap(pieces []numberPiece, pattern string) []numberPiece {
	at := strings.Index(pattern, "{0}")
	if at < 0 {
		return pieces
	}
	out := make([]numberPiece, 0, len(pieces)+4)
	out = append(out, o.affix(pattern[:at])...)
	out = append(out, pieces...)
	return append(out, o.affix(pattern[at+3:])...)
}

// affix breaks one side of a pattern into its pieces.
func (o *numberOptions) affix(text string) []numberPiece {
	if text == "" {
		return nil
	}
	marks := []struct{ text, kind string }{
		{"\u00a4", "currency"},
		{o.locale.PercentSign, "percentSign"},
		{o.locale.Minus, "minusSign"},
		{"+", "plusSign"},
	}
	var out []numberPiece
	for len(text) > 0 {
		// Whichever mark comes first in what is left of the text.
		best, bestAt, bestKind := "", -1, ""
		for _, mark := range marks {
			if mark.text == "" {
				continue
			}
			if at := strings.Index(text, mark.text); at >= 0 && (bestAt < 0 || at < bestAt) {
				best, bestAt, bestKind = mark.text, at, mark.kind
			}
		}
		if bestAt < 0 {
			out = append(out, numberPiece{"literal", text})
			break
		}
		if bestAt > 0 {
			out = append(out, numberPiece{"literal", text[:bestAt]})
		}
		if bestKind == "currency" {
			out = append(out, numberPiece{"currency", o.currencyText()})
		} else {
			out = append(out, numberPiece{bestKind, best})
		}
		text = text[bestAt+len(best):]
	}
	return out
}

// currencyText is how this currency is written here: its symbol, or its code
// when that is what was asked for -- or when this runtime has no symbol for
// it, which is what a currency without one is written as anyway.
func (o *numberOptions) currencyText() string {
	switch o.currencyDisplay {
	case "code", "name":
		return o.currency
	}
	if s, ok := o.locale.Currencies[o.currency]; ok {
		return s
	}
	return o.currency
}

// digitsOf rounds the number as the options ask and returns its digits, in the
// locale's own digits, as the whole part and the fraction.
func (o *numberOptions) digitsOf(x float64, compact bool) (string, string) {
	minFrac, maxFrac := o.minFrac, o.maxFrac
	if compact {
		// A compact number is written to two significant digits, except that
		// the whole part is never rounded away: 1.2K, 12K, 123K.
		minFrac, maxFrac = 0, compactPlaces(x)
	}

	var text string
	if o.maxSig > 0 {
		text = formatSignificant(x, o.minSig, o.maxSig)
	} else {
		text = roundDecimal(x, maxFrac)
		if maxFrac > minFrac {
			text = trimFraction(text, minFrac)
		} else {
			text = padFraction(text, minFrac)
		}
	}

	whole, fraction, _ := strings.Cut(text, ".")
	for len(whole) < o.minInt {
		whole = "0" + whole
	}
	return o.localDigits(whole), o.localDigits(fraction)
}

// isZeroText reports whether what was written amounts to zero.
func isZeroText(whole, fraction string) bool {
	for _, part := range []string{whole, fraction} {
		for _, r := range part {
			if r != '0' && (r < '\u0660' || r > '\u0669') {
				// A digit that is not zero, in any of the digit sets carried
				// here: the ASCII zero and the ones that follow their own.
				if !isZeroDigit(r) {
					return false
				}
			}
			if !isZeroDigit(r) {
				return false
			}
		}
	}
	return true
}

// isZeroDigit reports whether a rune is the zero of its own digit set, which
// is the first of the ten.
func isZeroDigit(r rune) bool {
	switch r {
	case '0', '\u0660', '\u06f0', '\u0966', '\u09e6', '\u0be6', '\u0c66',
		'\u0ce6', '\u0d66', '\u0e50', '\u1040', '\u17e0':
		return true
	}
	return false
}

// compactPlaces is how many decimal places a compact number keeps: enough for
// two significant digits, and none once the whole part has that many.
func compactPlaces(x float64) int {
	if x == 0 {
		return 0
	}
	places := 2 - (int(math.Floor(math.Log10(math.Abs(x)))) + 1)
	if places < 0 {
		return 0
	}
	return places
}

// roundCompact is the value a compact number rounds to, which is what says
// whether it has grown into the next step.
func roundCompact(x float64) float64 {
	places := compactPlaces(x)
	scale := math.Pow(10, float64(places))
	return math.Floor(x*scale+0.5) / scale
}

// localDigits writes ASCII digits in whatever digits the locale uses.
func (o *numberOptions) localDigits(s string) string {
	digits := o.locale.Digits
	if digits == "" || s == "" {
		return s
	}
	runes := []rune(digits)
	if len(runes) != 10 {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] >= '0' && s[i] <= '9' {
			b.WriteRune(runes[s[i]-'0'])
			continue
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// roundDecimal writes a number with a given number of decimal places, rounding
// a half away from zero.
//
// Go rounds a half to even, which is the better rule for arithmetic and the
// wrong one here: what a person expects, and what ICU does, is that 0.5 written
// without decimals is 1. The rounding is done on the shortest decimal form of
// the number, which is the same one every other engine rounds.
func roundDecimal(x float64, places int) string {
	text := strconv.FormatFloat(x, 'f', -1, 64)
	whole, fraction, _ := strings.Cut(text, ".")
	if len(fraction) <= places {
		return padFraction(text, places)
	}

	digits := []byte(whole + fraction[:places])
	roundUp := fraction[places] >= '5'
	if roundUp {
		i := len(digits) - 1
		for ; i >= 0; i-- {
			if digits[i] < '9' {
				digits[i]++
				break
			}
			digits[i] = '0'
		}
		if i < 0 {
			digits = append([]byte{'1'}, digits...)
		}
	}
	cut := len(digits) - places
	out := string(digits[:cut])
	if out == "" {
		out = "0"
	}
	if places > 0 {
		out += "." + string(digits[cut:])
	}
	return out
}

// padFraction writes the zeros a minimum asks for.
func padFraction(text string, min int) string {
	if min == 0 {
		return text
	}
	whole, fraction, ok := strings.Cut(text, ".")
	if !ok {
		fraction = ""
	}
	for len(fraction) < min {
		fraction += "0"
	}
	return whole + "." + fraction
}

// trimFraction drops the trailing zeros a number does not need, down to the
// minimum the options asked for.
func trimFraction(text string, min int) string {
	whole, fraction, ok := strings.Cut(text, ".")
	if !ok {
		return text
	}
	for len(fraction) > min && strings.HasSuffix(fraction, "0") {
		fraction = fraction[:len(fraction)-1]
	}
	if fraction == "" {
		return whole
	}
	return whole + "." + fraction
}

// formatSignificant rounds to a number of significant digits rather than to a
// number of decimal places.
func formatSignificant(x float64, min, max int) string {
	if x == 0 {
		if min <= 1 {
			return "0"
		}
		return "0." + strings.Repeat("0", min-1)
	}
	text := strconv.FormatFloat(x, 'e', max-1, 64)
	rounded, err := strconv.ParseFloat(text, 64)
	if err != nil {
		rounded = x
	}
	// How many decimal places those significant digits amount to.
	places := max - 1 - int(math.Floor(math.Log10(math.Abs(rounded))))
	if places < 0 {
		places = 0
	}
	out := strconv.FormatFloat(rounded, 'f', places, 64)
	// Trailing zeros beyond the minimum are not significant.
	digits := 0
	seen := false
	for i := 0; i < len(out); i++ {
		if out[i] >= '0' && out[i] <= '9' {
			if out[i] != '0' {
				seen = true
			}
			if seen {
				digits++
			}
		}
	}
	for digits > min && strings.Contains(out, ".") && strings.HasSuffix(out, "0") {
		out = out[:len(out)-1]
		digits--
	}
	return strings.TrimSuffix(out, ".")
}

// groupDigits puts the separators in: every three digits, or the first three
// and then every two where that is the convention.
func (o *numberOptions) groupDigits(whole string, compact bool) []numberPiece {
	runes := []rune(whole)
	// How many digits the first group must have. A compact number asks for
	// two whatever the language says, since 1235万 is short enough already.
	least := o.locale.MinGrouping
	if least < 1 {
		least = 1
	}
	if compact && least < 2 {
		least = 2
	}
	size := 3
	if o.locale.Indian {
		size = 3
	}
	if !o.useGrouping || len(runes) < size+least {
		return []numberPiece{{"integer", whole}}
	}
	var cuts []int
	if o.locale.Indian {
		// The last three stand alone, and what comes before them goes in twos.
		for i := len(runes) - 3; i > 0; i -= 2 {
			cuts = append(cuts, i)
		}
	} else {
		for i := len(runes) - 3; i > 0; i -= 3 {
			cuts = append(cuts, i)
		}
	}
	// The cuts came out back to front.
	for i, j := 0, len(cuts)-1; i < j; i, j = i+1, j-1 {
		cuts[i], cuts[j] = cuts[j], cuts[i]
	}

	var out []numberPiece
	last := 0
	for _, cut := range cuts {
		out = append(out, numberPiece{"integer", string(runes[last:cut])})
		out = append(out, numberPiece{"group", o.locale.Group})
		last = cut
	}
	out = append(out, numberPiece{"integer", string(runes[last:])})
	return out
}
