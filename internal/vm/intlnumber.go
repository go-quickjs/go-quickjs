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
	choice *localeChoice

	// digits is the numbering system to write in, which is the locale's own
	// unless the tag or the options asked for another.
	digits string

	style           string // decimal, percent, currency, unit
	currency        string
	currencyDisplay string
	currencySign    string // standard, accounting
	unit            string
	unitDisplay     string
	notation        string // standard, compact, scientific, engineering
	compactDisplay  string
	signDisplay     string // auto, always, never, exceptZero, negative
	// useGrouping is a word rather than a flag: always, auto, min2, or empty
	// for no grouping at all.
	useGrouping string

	minInt           int
	minFrac, maxFrac int
	// minSig and maxSig are the significant-digit counts, which take the place
	// of the fraction counts when they are given.
	minSig, maxSig int
	// rounding is which of the two counts is in force: fraction, significant,
	// or one of the two that asks for both and keeps whichever says more.
	rounding string
	// reportSig and reportFrac say which of the two counts resolvedOptions
	// tells of, which is not always the one the rounding went by.
	reportSig, reportFrac bool
	roundingMode          string
	roundingPriority      string
	roundingIncrement     int
	trailingZero          string

	// formatFn is the bound function the format getter hands out, kept so that
	// every ask answers with the same one.
	formatFn *Object
}

// marks are the decimal point and the thousands mark to write with: the
// locale's own, unless another numbering system was asked for, which brings
// its own.
func (o *numberOptions) marks() (decimal, group string) {
	if o.digits != "" && o.digits != o.locale.Numbering {
		if point, thousands, ok := icu.NumberingMarks(o.digits); ok {
			return point, thousands
		}
	}
	return o.locale.Decimal, o.locale.Group
}

// numberPiece is one part of a formatted number, in the shape formatToParts
// hands back.
type numberPiece struct {
	kind  string
	value string
}

// format writes a number the way the locale and the options ask.
func (o *numberOptions) format(x float64) string {
	return piecesText(o.parts(x))
}

func piecesText(pieces []numberPiece) string {
	var b strings.Builder
	for _, piece := range pieces {
		b.WriteString(piece.value)
	}
	return b.String()
}

// parts is the whole of the formatting: everything else here is arithmetic on
// its way to this list.
func (o *numberOptions) parts(x float64) []numberPiece {
	switch {
	case math.IsNaN(x):
		return o.valueParts(decimal{}, "nan")
	case math.IsInf(x, 0):
		special := "inf"
		if math.Signbit(x) {
			special = "-inf"
		}
		return o.valueParts(decimal{}, special)
	}
	return o.decimalParts(decimalOf(x))
}

// valueParts writes whatever was handed over, which may not be a number that
// can be written as digits.
func (o *numberOptions) valueParts(d decimal, special string) []numberPiece {
	switch special {
	case "nan":
		return o.wordParts("nan", o.locale.NaN, false, true)
	case "inf":
		return o.wordParts("infinity", o.locale.Infinity, false, false)
	case "-inf":
		return o.wordParts("infinity", o.locale.Infinity, true, false)
	}
	return o.decimalParts(d)
}

// wordParts writes the numbers that are not written as digits. They take no
// sign of their own, but the options may ask for one anyway.
func (o *numberOptions) wordParts(kind, text string, negative, zero bool) []numberPiece {
	sign := o.wants(negative, zero)
	return o.wrap([]numberPiece{{kind, text}}, o.pattern(negative, sign))
}

// decimalParts writes a number that is written as digits.
func (o *numberOptions) decimalParts(d decimal) []numberPiece {
	// The sign stays on the number while it is rounded, since half the modes
	// round by which way is up rather than by which way is away.
	negative := d.negative
	if o.style == "percent" {
		d = d.times10(2)
	}

	// The notation may divide the number down and say afterwards what it was
	// divided by: a word in compact notation, an exponent in the others.
	var tail []numberPiece
	compact := false
	var kept string
	switch o.notation {
	case "compact":
		compact = true
		var suffix string
		d, kept, suffix, _ = o.compactly(d)
		if suffix != "" {
			// The space before the word is not part of the word.
			tail = o.unitPieces(suffix, "compact")
		}
	case "scientific", "engineering":
		var exponent int
		d, kept, exponent = o.scaleForExponent(d)
		tail = o.exponentParts(exponent)
	default:
		d, kept = o.round(d)
	}

	whole, fraction := o.digitsOf(d, kept)
	// The sign follows what was written rather than what was given: a value
	// that rounds to zero is written as zero, and zero takes no sign of its
	// own.
	zero := d.isZero()
	pieces := o.groupDigits(whole, compact)
	if fraction != "" {
		point, _ := o.marks()
		pieces = append(pieces, numberPiece{"decimal", point})
		pieces = append(pieces, numberPiece{"fraction", fraction})
	}
	pieces = append(pieces, tail...)
	out := o.wrap(pieces, o.pattern(negative, o.wants(negative, zero)))
	if o.style == "unit" {
		out = o.measure(out, whole, fraction)
	}
	return out
}

// measure puts the name of the unit around the number: "km" after it in most
// languages, and in front of it in a few. A unit written underneath another is
// the one above with the one below joined to it.
func (o *numberOptions) measure(pieces []numberPiece, whole, fraction string) []numberPiece {
	above, below, divided := strings.Cut(o.unit, "-per-")
	// Which form the name takes follows the count as it is written.
	category := o.locale.CardinalRule().CategoryOf(whole, fraction, 0, 0)
	pattern, ok := o.locale.UnitPattern(above, o.unitDisplay, category)
	if !ok {
		return pieces
	}
	out := o.aroundNumber(pieces, pattern, "unit")
	if divided {
		// A pair the language writes its own way, or the two joined. What
		// joins them belongs to the name: "meters per second" is one name and
		// not two with a space between.
		if own, ok := o.locale.UnitCompound(o.unit, o.unitDisplay); ok {
			out = o.wholeAround(out, own)
		} else if per, ok := o.locale.UnitPer(below, o.unitDisplay); ok {
			out = o.wholeAround(out, per)
		}
	}
	return mergeUnitPieces(out)
}

// mergeUnitPieces joins the names that ended up side by side, since "km" and
// "/h" written together are one name and not two.
func mergeUnitPieces(pieces []numberPiece) []numberPiece {
	out := pieces[:0:0]
	for _, piece := range pieces {
		if len(out) > 0 && out[len(out)-1].kind == piece.kind &&
			(piece.kind == "unit" || piece.kind == "literal") {
			out[len(out)-1].value += piece.value
			continue
		}
		out = append(out, piece)
	}
	return out
}

// aroundNumber puts what a pattern says on either side of what has been
// written so far.
func (o *numberOptions) aroundNumber(pieces []numberPiece, pattern, kind string) []numberPiece {
	at := strings.Index(pattern, "{0}")
	if at < 0 {
		return pieces
	}
	out := make([]numberPiece, 0, len(pieces)+2)
	if before := pattern[:at]; before != "" {
		out = append(out, o.unitPieces(before, kind)...)
	}
	out = append(out, pieces...)
	if after := pattern[at+3:]; after != "" {
		out = append(out, o.unitPieces(after, kind)...)
	}
	return out
}

// wholeAround puts a compound-unit pattern around what has been written. A
// prefix's trailing space separates its name from the number; a suffix is a
// continuation of the unit name and is merged with it below.
func (o *numberOptions) wholeAround(pieces []numberPiece, pattern string) []numberPiece {
	at := strings.Index(pattern, "{0}")
	if at < 0 {
		return pieces
	}
	out := make([]numberPiece, 0, len(pieces)+2)
	if before := pattern[:at]; before != "" {
		out = append(out, o.unitPieces(before, "unit")...)
	}
	out = append(out, pieces...)
	if after := pattern[at+3:]; after != "" {
		out = append(out, numberPiece{"unit", after})
	}
	return out
}

// unitPieces splits the words around a number into the name of the unit and
// the spaces and marks that are not part of it.
func (o *numberOptions) unitPieces(text, kind string) []numberPiece {
	trimmed := strings.Trim(text, " \u00a0\u202f")
	if trimmed == "" {
		return []numberPiece{{"literal", text}}
	}
	at := strings.Index(text, trimmed)
	var out []numberPiece
	if at > 0 {
		out = append(out, numberPiece{"literal", text[:at]})
	}
	out = append(out, numberPiece{kind, trimmed})
	if rest := text[at+len(trimmed):]; rest != "" {
		out = append(out, numberPiece{"literal", rest})
	}
	return out
}

// compactly divides a number down to the step its language writes it in --
// thousands in English, ten-thousands in Japanese -- and reports the word that
// goes after it and the power of ten that was factored out.
func (o *numberOptions) compactly(d decimal) (decimal, string, string, int) {
	long := o.compactDisplay == "long"
	x, _ := strconv.ParseFloat(writeDecimalText(d), 64)
	scaled, divisor, form := o.locale.Compact(x, long)
	steps := 0
	if divisor >= 1 {
		steps = int(math.Round(math.Log10(divisor)))
	}
	out, kept := o.round(d.times10(-steps))
	// Rounding can push a number into the next step -- 999.9 thousand is a
	// million -- and then it is written there instead.
	if grown, _ := strconv.ParseFloat(writeDecimalText(out), 64); grown >= 1000 && divisor >= 1 {
		_, divisor, form = o.locale.Compact(grown*divisor, long)
		steps = 0
		if divisor >= 1 {
			steps = int(math.Round(math.Log10(divisor)))
		}
		out, kept = o.round(d.times10(-steps))
	}
	_ = scaled
	value, _ := strconv.ParseFloat(writeDecimalText(out), 64)
	return out, kept, form.SuffixFor(o.locale.CardinalRule().Category(value)), steps
}

// scaleForExponent divides a number down to one digit before the point, or to
// three where the exponent must be a multiple of three, and reports the power
// of ten it was divided by.
func (o *numberOptions) scaleForExponent(d decimal) (decimal, string, int) {
	if d.isZero() {
		out, kept := o.round(d)
		return out, kept, 0
	}
	step := 1
	if o.notation == "engineering" {
		step = 3
	}
	exponent := d.magnitude()
	if step == 3 {
		exponent = int(math.Floor(float64(exponent)/3)) * 3
	}
	out, kept := o.round(d.times10(-exponent))
	// Rounding can take the mantissa up a digit, and then it belongs to the
	// next exponent: 9.99 to two digits is 10, which is 1E1.
	if !out.isZero() && out.magnitude() >= step {
		exponent += step
		out, kept = o.round(d.times10(-exponent))
	}
	return out, kept, exponent
}

// exponentParts writes the exponent: the mark this language puts before it, a
// minus if it is below zero, and the digits.
func (o *numberOptions) exponentParts(exponent int) []numberPiece {
	out := []numberPiece{{"exponentSeparator", o.locale.Exponential}}
	if exponent < 0 {
		out = append(out, numberPiece{"exponentMinusSign", o.locale.Minus})
		exponent = -exponent
	}
	return append(out, numberPiece{"exponentInteger",
		o.localDigits(strconv.Itoa(exponent))})
}

// round rounds a number as the options ask. Where both a count of significant
// digits and a count of decimal places were given, both are done and the one
// that says more -- or less -- is kept.
func (o *numberOptions) round(d decimal) (decimal, string) {
	switch o.rounding {
	case "significant":
		return d.roundToDigits(o.maxSig, o.roundingMode), "significant"
	case "morePrecision", "lessPrecision":
		bySig := d.roundToDigits(o.maxSig, o.roundingMode)
		byFrac := d.roundToMultiple(-o.maxFrac, o.roundingIncrement, o.roundingMode)
		// Which of the two said more: the one whose last digit falls in the
		// lower place.
		sigPlace := bySig.magnitude() - o.maxSig + 1
		if (sigPlace <= -o.maxFrac) == (o.rounding == "morePrecision") {
			return bySig, "significant"
		}
		return byFrac, "fraction"
	}
	return d.roundToMultiple(-o.maxFrac, o.roundingIncrement, o.roundingMode), "fraction"
}

// writeDecimalText is a number as plain digits, for the places that have to
// hand it to arithmetic that works in binary.
func writeDecimalText(d decimal) string {
	whole, fraction := d.text(0, -1)
	if fraction == "" {
		return whole
	}
	return whole + "." + fraction
}

// wants reports whether a sign is written for this value.
func (o *numberOptions) wants(negative, zero bool) bool {
	switch o.signDisplay {
	case "never":
		return false
	case "always":
		return true
	case "exceptZero":
		return !zero
	case "negative":
		return negative && !zero
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
		if o.currencySign == "accounting" {
			minus = l.Accounting
		}
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
	if s, ok := o.locale.CurrencySymbol(o.currency); ok {
		return s
	}
	return o.currency
}

// digitsOf writes the rounded number out as the whole part and the fraction,
// in whatever digits this locale writes numbers with.
func (o *numberOptions) digitsOf(d decimal, kept string) (string, string) {
	whole, fraction := o.rawDigits(d, kept)
	return o.localDigits(whole), o.localDigits(fraction)
}

// rawDigits is the same in the digits everyone writes arithmetic in, which is
// what the plural rules ask about.
func (o *numberOptions) rawDigits(d decimal, kept string) (string, string) {
	minFrac, maxFrac := o.minFrac, o.maxFrac
	significant := kept == "significant"
	if significant {
		// A count of significant digits says how many to write as well as how
		// many to keep: 1 to three of them is 1.00.
		wanted := o.minSig
		if d.digitCount() > wanted {
			wanted = d.digitCount()
		}
		minFrac, maxFrac = wanted-d.exp, -1
		if d.isZero() {
			minFrac = o.minSig - 1
		}
		if minFrac < 0 {
			minFrac = 0
		}
	}
	if o.trailingZero == "stripIfInteger" && d.isInteger() {
		minFrac, maxFrac = 0, 0
	}
	whole, fraction := d.text(minFrac, maxFrac)
	for len(whole) < o.minInt {
		whole = "0" + whole
	}
	return whole, fraction
}

// localDigits writes ASCII digits in whatever digits are being written in,
// which is the locale's own unless another numbering system was asked for.
func (o *numberOptions) localDigits(s string) string {
	digits := o.locale.Digits
	if o.digits != "" && o.digits != o.locale.Numbering {
		digits, _ = icu.NumberingDigits(o.digits)
	}
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
	switch o.useGrouping {
	case "always":
		least = 1
	case "min2":
		least = 2
	}
	if compact && least < 2 {
		least = 2
	}
	size := 3
	if o.useGrouping == "" || len(runes) < size+least {
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
		_, thousands := o.marks()
		out = append(out, numberPiece{"group", thousands})
		last = cut
	}
	out = append(out, numberPiece{"integer", string(runes[last:])})
	return out
}
