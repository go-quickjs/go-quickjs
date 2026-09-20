package vm

import (
	"math"
	"math/big"
	"strconv"
	"strings"

	"github.com/go-quickjs/go-quickjs/internal/icu"
)

// Intl.DurationFormat: how long something took, written the way a language
// writes it.
//
// "1 hr, 46 min, 40 sec" in English, "1 h, 46 min et 40 s" in French, and
// "1:46:40" where a clock is what is wanted. Each part is a measurement, so
// it is written the way a measurement is written; the parts are joined the way
// a list is joined; and the parts that are written as numbers run together
// with the mark this language puts between the hours and the minutes.

// durationUnits are the parts of a duration, largest first, with the name each
// is measured in.
var durationUnits = [...]struct{ field, unit string }{
	{"years", "year"}, {"months", "month"}, {"weeks", "week"}, {"days", "day"},
	{"hours", "hour"}, {"minutes", "minute"}, {"seconds", "second"},
	{"milliseconds", "millisecond"}, {"microseconds", "microsecond"},
	{"nanoseconds", "nanosecond"},
}

// durationOptions is a resolved Intl.DurationFormat.
type durationOptions struct {
	locale *icu.Locale
	choice *localeChoice
	style  string // long, short, narrow, digital

	// widths and shown are how each part is written and whether it is written
	// when it is zero.
	widths [len(durationUnits)]string
	shown  [len(durationUnits)]string

	fractionalDigits int
	hasFractional    bool
}

func (r *Runtime) initDurationFormat(intl *Object) {
	proto := newObject(r.proto.object, ClassObject)
	ctor := r.newCtor("DurationFormat", 0, proto, func(rt *Runtime, this Value, args []Value) (Value, error) {
		made, err := rt.protoFromNewTargetErr(rt.intlProtoOf("DurationFormat"))
		if err != nil {
			return Undefined, err
		}
		if !rt.Constructing() {
			return Undefined, rt.throwTypeError("Intl.DurationFormat requires new")
		}
		o, err := rt.durationOptionsFrom(args)
		if err != nil {
			return Undefined, err
		}
		out := newObject(made, ClassObject)
		out.data = o
		return Obj(out), nil
	})
	r.defValue(intl, "DurationFormat", Obj(ctor))
	r.intlProtos["DurationFormat"] = proto
	r.defToStringTag(proto, "Intl.DurationFormat")
	r.defSupportedLocalesOf(ctor)

	r.defMethod(proto, "format", 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
		o, err := rt.durationFormatOf(this)
		if err != nil {
			return Undefined, err
		}
		duration, err := rt.durationFrom(arg(args, 0))
		if err != nil {
			return Undefined, err
		}
		return Str(NewString(durationText(o.parts(rt, duration)))), nil
	})
	r.defMethod(proto, "formatToParts", 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
		o, err := rt.durationFormatOf(this)
		if err != nil {
			return Undefined, err
		}
		duration, err := rt.durationFrom(arg(args, 0))
		if err != nil {
			return Undefined, err
		}
		pieces := o.parts(rt, duration)
		out := make([]Value, len(pieces))
		for i, piece := range pieces {
			part := rt.partObject(piece.kind, piece.value)
			if piece.unit != "" {
				rt.putString(part, "unit", piece.unit)
			}
			out[i] = Obj(part)
		}
		return Obj(rt.newArrayFrom(out)), nil
	})
	r.defMethod(proto, "resolvedOptions", 0, func(rt *Runtime, this Value, args []Value) (Value, error) {
		o, err := rt.durationFormatOf(this)
		if err != nil {
			return Undefined, err
		}
		out := newObject(rt.proto.object, ClassObject)
		rt.putString(out, "locale", o.choice.locale())
		rt.putString(out, "numberingSystem", o.choice.setting("nu"))
		rt.putString(out, "style", o.style)
		for i, unit := range durationUnits {
			rt.putString(out, unit.field, o.widths[i])
			rt.putString(out, unit.field+"Display", o.shown[i])
		}
		if o.hasFractional {
			rt.putInt(out, "fractionalDigits", o.fractionalDigits)
		}
		return Obj(out), nil
	})
}

// durationOptionsFrom reads the arguments shared by the DurationFormat
// constructor and Temporal.Duration.prototype.toLocaleString.
func (r *Runtime) durationOptionsFrom(args []Value) (*durationOptions, error) {
	tags, err := r.requestedLocales(arg(args, 0))
	if err != nil {
		return nil, err
	}
	options, err := r.strictOptions(arg(args, 1))
	if err != nil {
		return nil, err
	}
	if _, err := r.stringOption(options, "localeMatcher", "best fit",
		"lookup", "best fit"); err != nil {
		return nil, err
	}
	numbering, err := r.typeOption(options, "numberingSystem")
	if err != nil {
		return nil, err
	}
	choice := r.resolveLocale(tags, "nu")
	choice.override("nu", numbering)
	o := &durationOptions{locale: choice.data, choice: choice}
	if o.style, err = r.stringOption(options, "style", "short",
		"long", "short", "narrow", "digital"); err != nil {
		return nil, err
	}
	if err := r.readDurationUnits(o, options); err != nil {
		return nil, err
	}
	return o, nil
}

// readDurationUnits reads how each part of a duration is to be written. A part
// asked for as a number draws the parts after it into the same run of numbers,
// which is how "1:46:40" comes about.
func (r *Runtime) readDurationUnits(o *durationOptions, options *Object) error {
	previous := ""
	for i, unit := range durationUnits {
		allowed := []string{"long", "short", "narrow"}
		switch unit.field {
		case "hours", "minutes", "seconds":
			allowed = append(allowed, "numeric", "2-digit")
		case "milliseconds", "microseconds", "nanoseconds":
			allowed = append(allowed, "numeric")
		}
		width, err := r.stringOption(options, unit.field, "", allowed...)
		if err != nil {
			return err
		}
		display := "always"
		if width == "" {
			switch {
			case o.style == "digital":
				// A clock writes the hours, the minutes and the seconds
				// whether or not they are zero, and the rest only when they
				// are not.
				width = "numeric"
				switch unit.field {
				case "years", "months", "weeks", "days":
					width, display = "short", "auto"
				case "minutes", "seconds":
					width = "2-digit"
				case "milliseconds", "microseconds", "nanoseconds":
					width, display = "numeric", "auto"
				}
			case previous == "numeric" || previous == "2-digit":
				// A part written as a number is followed by parts written as
				// numbers, since they are read together -- and written whether
				// or not they are zero, since a clock shows every field. What
				// is smaller than a second is written after the point of the
				// seconds rather than as a field of its own.
				switch unit.field {
				case "minutes", "seconds":
					width = "2-digit"
				case "milliseconds", "microseconds", "nanoseconds":
					width, display = "numeric", "auto"
				default:
					width = "numeric"
				}
			default:
				width, display = o.style, "auto"
			}
		}
		// A part that follows one written as a number is written as a number
		// too, and the minutes and the seconds in two digits: 2:03 and not
		// 2:3. A part that asks to be written as words there cannot be.
		if previous == "numeric" || previous == "2-digit" {
			switch width {
			case "long", "short", "narrow":
				return r.throwRangeError(
					"%s cannot be written as words after a part written as a number",
					unit.field)
			}
			if unit.field == "minutes" || unit.field == "seconds" {
				width = "2-digit"
			}
		}
		if o.shown[i], err = r.stringOption(options, unit.field+"Display", display,
			"auto", "always"); err != nil {
			return err
		}
		o.widths[i] = width
		previous = width
	}
	var err error
	if o.fractionalDigits, o.hasFractional, err = r.intOption(options,
		"fractionalDigits", 0, 9, 0); err != nil {
		return err
	}
	return nil
}

// durationFrom reads the duration to write: an object with a field for each
// part of it, or the string a duration is written as.
func (r *Runtime) durationFrom(v Value) ([len(durationUnits)]float64, error) {
	var out [len(durationUnits)]float64
	if v.IsString() {
		if duration, err := parseTemporalDuration(v.String().Go()); err == nil &&
			duration.valid() && duration.withinRange() {
			return duration.fields(), nil
		}
		if parsed, ok := parseDuration(v.String().Go()); ok {
			return parsed, nil
		}
		return out, r.throwRangeError("that is not a duration: %s", v.String().Go())
	}
	o := v.Object()
	if o == nil {
		return out, r.throwTypeError("a duration is an object")
	}
	if duration, ok := o.data.(*temporalDuration); ok {
		return duration.fields(), nil
	}
	any := false
	for i, unit := range durationUnits {
		field, err := r.getProp(o, r.atoms.intern(unit.field), v)
		if err != nil {
			return out, err
		}
		if field.IsUndefined() {
			continue
		}
		n, err := r.toNumber(field)
		if err != nil {
			return out, err
		}
		if math.IsNaN(n) || math.IsInf(n, 0) || n != math.Trunc(n) {
			return out, r.throwRangeError("a duration is counted in whole units")
		}
		out[i] = n
		any = true
	}
	if !any {
		return out, r.throwTypeError("a duration has to say how long it was")
	}
	if !validDuration(out) {
		return out, r.throwRangeError("that is longer than a duration may be")
	}
	return out, nil
}

// validDuration reports whether a duration is one that can be counted: every
// part pointing the same way, and the whole of it within the bounds arithmetic
// can hold.
//
// The bound is checked exactly rather than in floating point, since the
// question is asked of numbers near the edge of what floating point can hold
// and the answer would otherwise be wrong there.
func validDuration(duration [len(durationUnits)]float64) bool {
	sign := 0
	for _, value := range duration {
		switch {
		case value > 0 && sign < 0, value < 0 && sign > 0:
			return false
		case value > 0:
			sign = 1
		case value < 0:
			sign = -1
		}
	}
	for i := 0; i < 3; i++ {
		if math.Abs(duration[i]) >= 1<<32 {
			return false
		}
	}
	// The days and everything under them, counted in seconds.
	seconds := new(big.Rat)
	for i, factor := range []*big.Rat{
		big.NewRat(86400, 1), big.NewRat(3600, 1), big.NewRat(60, 1),
		big.NewRat(1, 1), big.NewRat(1, 1e3), big.NewRat(1, 1e6),
		big.NewRat(1, 1e9),
	} {
		part := new(big.Rat).SetFloat64(duration[i+3])
		if part == nil {
			return false
		}
		seconds.Add(seconds, part.Mul(part, factor))
	}
	seconds.Abs(seconds)
	return seconds.Cmp(new(big.Rat).SetInt64(1<<53)) < 0
}

// parseDuration reads a duration written the way ISO 8601 writes one:
// P1Y2M3DT4H5M6S, and the same with any part left out.
func parseDuration(text string) ([len(durationUnits)]float64, bool) {
	var out [len(durationUnits)]float64
	sign := 1.0
	switch {
	case strings.HasPrefix(text, "-"), strings.HasPrefix(text, "\u2212"):
		sign, text = -1, text[strings.IndexAny(text, "-\u2212")+1:]
	case strings.HasPrefix(text, "+"):
		text = text[1:]
	}
	if len(text) == 0 || (text[0] != 'P' && text[0] != 'p') {
		return out, false
	}
	text = text[1:]

	// The date parts, then the time parts after the T.
	date, clock, _ := strings.Cut(text, "T")
	if clock == "" {
		date, clock, _ = strings.Cut(text, "t")
	}
	fields := map[byte]int{'Y': 0, 'M': 1, 'W': 2, 'D': 3}
	any := false
	for _, part := range []struct {
		text   string
		fields map[byte]int
	}{
		{date, fields},
		{clock, map[byte]int{'H': 4, 'M': 5, 'S': 6}},
	} {
		rest := part.text
		last := -1
		for rest != "" {
			at := 0
			for at < len(rest) && (rest[at] >= '0' && rest[at] <= '9') {
				at++
			}
			if at == 0 || at == len(rest) {
				return out, false
			}
			n, err := strconv.ParseFloat(rest[:at], 64)
			if err != nil {
				return out, false
			}
			which, ok := part.fields[rest[at]&^0x20]
			if !ok || which <= last {
				return out, false
			}
			out[which], last, any = sign*n, which, true
			rest = rest[at+1:]
		}
	}
	if !any || !validDuration(out) {
		return out, false
	}
	return out, true
}

// durationPiece is one part of a written duration, which says which unit it
// belongs to where it belongs to one.
type durationPiece struct {
	kind  string
	value string
	unit  string
}

// durationText is the pieces run together, which is what format answers with.
func durationText(pieces []durationPiece) string {
	var b strings.Builder
	for _, piece := range pieces {
		b.WriteString(piece.value)
	}
	return b.String()
}

// parts writes a duration out: each part as a measurement or as a number, the
// numbers run together with a colon, and the whole joined the way this
// language joins a list.
//
// A part smaller than a second, where the part before it was written as a
// number, is written after the point of that part rather than on its own:
// 1.5 seconds and not 1 second and 500 milliseconds.
func (o *durationOptions) parts(r *Runtime, duration [len(durationUnits)]float64) []durationPiece {
	negative := false
	for _, value := range duration {
		if value < 0 {
			negative = true
			break
		}
	}

	var groups [][]durationPiece
	running := false
	signWritten := false
	for i, unit := range durationUnits {
		value, places := decimalOf(duration[i]), -1
		if duration[i] == 0 && math.Signbit(duration[i]) {
			value.negative = true
		}
		numeric := o.widths[i] == "numeric" || o.widths[i] == "2-digit"

		// The parts smaller than a second run into the one before them.
		last := false
		if i+1 < len(durationUnits) && o.widths[i+1] == "numeric" {
			switch unit.field {
			case "seconds", "milliseconds", "microseconds":
				value, _ = parseDecimal(fractionOf(duration, i))
				last = true
				places = 9
				if o.hasFractional {
					places = o.fractionalDigits
				}
			}
		}

		// A zero is left out unless it was asked for, or unless it is the
		// minutes of a clock that goes on to show seconds.
		needed := false
		if unit.field == "minutes" && running {
			needed = o.shown[i+1] == "always" || duration[i+1] != 0 ||
				duration[i+2] != 0 || duration[i+3] != 0 || duration[i+4] != 0
		}
		if value.isZero() && o.shown[i] == "auto" && !needed {
			if last {
				break
			}
			continue
		}

		// The sign is written once, on the first part that is written.
		sign := false
		if !signWritten {
			signWritten, sign = true, negative
		}
		pieces := o.written(unit.unit, value, o.widths[i], sign, places)
		switch {
		case running:
			at := len(groups) - 1
			groups[at] = append(groups[at],
				durationPiece{"literal", o.timeSeparator(), ""})
			groups[at] = append(groups[at], pieces...)
		default:
			groups = append(groups, pieces)
			running = numeric
		}
		if last {
			break
		}
	}
	if len(groups) == 0 {
		return []durationPiece{{"literal", "", ""}}
	}

	// Joined the way a list of measurements is joined.
	list := &listOptions{locale: o.locale, kind: "unit", style: o.listStyle()}
	items := make([]string, len(groups))
	for i, group := range groups {
		items[i] = durationText(group)
	}
	var out []durationPiece
	at := 0
	for _, piece := range list.pieces(items) {
		if piece.kind == "element" && at < len(groups) {
			out = append(out, groups[at]...)
			at++
			continue
		}
		out = append(out, durationPiece{"literal", piece.value, ""})
	}
	return out
}

// fractionOf is a part of a duration with everything smaller than it counted
// after the point: the seconds with the milliseconds, microseconds and
// nanoseconds written as a fraction of a second.
//
// The sum is taken in whole nanoseconds, which every part below a second
// divides into exactly, so that a duration counted precisely is written
// precisely: 1.500250000 and not 1.500249999. It is taken in whole numbers of
// any size, since a duration may be longer than floating point counts exactly.
func fractionOf(duration [len(durationUnits)]float64, from int) string {
	first := len(durationUnits) - 4
	scale := [...]int64{1e9, 1e6, 1e3, 1}
	total := new(big.Int)
	for i := from; i < len(durationUnits); i++ {
		part, _ := new(big.Float).SetFloat64(duration[i]).Int(nil)
		total.Add(total, part.Mul(part, big.NewInt(scale[i-first])))
	}
	unit := big.NewInt(scale[from-first])
	whole, rest := new(big.Int).QuoRem(total, unit, new(big.Int))

	out := whole.String()
	if whole.Sign() == 0 && total.Sign() < 0 {
		out = "-0"
	}
	if digits := 9 - 3*(from-first); digits > 0 {
		fraction := rest.Abs(rest).String()
		for len(fraction) < digits {
			fraction = "0" + fraction
		}
		out += "." + fraction
	}
	return out
}

// written is one part of a duration: a measurement where it stands on its own,
// and a plain number where it is part of a clock.
func (o *durationOptions) written(unit string, value decimal, width string, sign bool, places int) []durationPiece {
	numbers := o.numberFormat()
	switch width {
	case "numeric", "2-digit":
		numbers.useGrouping = ""
		if width == "2-digit" {
			numbers.minInt = 2
		}
	default:
		numbers.style, numbers.unit, numbers.unitDisplay = "unit", unit, width
	}
	if places >= 0 {
		// What will not fit after the point is dropped rather than rounded,
		// since a duration counted exactly should not come out longer than it
		// was.
		numbers.maxFrac, numbers.roundingMode = places, "trunc"
		if o.hasFractional {
			numbers.minFrac = places
		}
	}
	if !sign {
		numbers.signDisplay = "never"
	} else if value.isZero() {
		// The first part written carries the sign even where it is nothing.
		value.negative = true
	}
	out := make([]durationPiece, 0, 4)
	for _, piece := range numbers.decimalParts(value) {
		out = append(out, durationPiece{piece.kind, piece.value, unit})
	}
	return out
}

// numberFormat is how the counts in a duration are written: plainly, in
// whatever digits this locale writes numbers with.
func (o *durationOptions) numberFormat() *numberOptions {
	return &numberOptions{
		locale: o.locale, choice: o.choice, digits: o.choice.setting("nu"),
		style: "decimal", notation: "standard", signDisplay: "auto",
		useGrouping: "auto", minInt: 1, rounding: "fraction",
		roundingMode: "halfExpand", roundingIncrement: 1,
	}
}

// timeSeparator is what stands between the hours and the minutes, which is a
// colon nearly everywhere.
func (o *durationOptions) timeSeparator() string { return ":" }

// listStyle is how the parts are joined: a clock joins them the way short
// measurements are joined.
func (o *durationOptions) listStyle() string {
	if o.style == "digital" {
		return "short"
	}
	return o.style
}

func (r *Runtime) durationFormatOf(this Value) (*durationOptions, error) {
	if o := this.Object(); o != nil {
		if opts, ok := o.data.(*durationOptions); ok {
			return opts, nil
		}
	}
	return nil, r.throwTypeError("this is not an Intl.DurationFormat")
}
