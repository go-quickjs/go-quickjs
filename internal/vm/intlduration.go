package vm

import (
	"math"
	"math/big"

	intl "github.com/go-quickjs/go-intl"
	"github.com/go-quickjs/go-intl/temporal"
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

// durationOptions is a resolved Intl.DurationFormat: go-intl's formatter,
// and the options as the runtime settled them.
type durationOptions struct {
	format *intl.DurationFormat
	style  string // long, short, narrow, digital

	// widths and shown are how each part is written and whether it is written
	// when it is zero, and askedWidths and askedShown what the options said
	// of them, empty where they said nothing, which is what go-intl is given:
	// a display asked for is judged where a default is not.
	widths      [len(durationUnits)]string
	shown       [len(durationUnits)]string
	askedWidths [len(durationUnits)]string
	askedShown  [len(durationUnits)]string

	fractionalDigits int
	hasFractional    bool
}

func (r *Runtime) initDurationFormat(intlObj *Object) {
	proto := newObject(r.proto.object, ClassObject)
	ctor := r.newCtor("DurationFormat", 0, proto, func(rt *Runtime, this Value, args []Value) (Value, error) {
		made, err := rt.protoFromNewTargetErr(rt.intlProtoOf("DurationFormat"))
		if err != nil {
			return Undefined, err
		}
		if !rt.Constructing() {
			return Undefined, rt.intlRequiresNew("Intl.DurationFormat")
		}
		o, err := rt.durationOptionsFrom(args)
		if err != nil {
			return Undefined, err
		}
		out := newObject(made, ClassObject)
		out.data = o
		return Obj(out), nil
	})
	r.defValue(intlObj, "DurationFormat", Obj(ctor))
	r.intlProtos["DurationFormat"] = proto
	r.defToStringTag(proto, "Intl.DurationFormat")
	r.defSupportedLocalesOfService(ctor, intl.ServiceDurationFormat)

	r.defMethod(proto, "format", 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
		o, err := rt.durationFormatOf(this, "Intl.DurationFormat.prototype.format")
		if err != nil {
			return Undefined, err
		}
		duration, err := rt.durationFrom(arg(args, 0))
		if err != nil {
			return Undefined, err
		}
		text, err := o.format.Format(intl.Duration(duration))
		if err != nil {
			return Undefined, rt.intlTemporalRange("Duration was not valid.")
		}
		return Str(NewString(text)), nil
	})
	r.defMethod(proto, "formatToParts", 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
		o, err := rt.durationFormatOf(this, "Intl.DurationFormat.prototype.formatToParts")
		if err != nil {
			return Undefined, err
		}
		duration, err := rt.durationFrom(arg(args, 0))
		if err != nil {
			return Undefined, err
		}
		pieces, err := o.format.FormatToParts(intl.Duration(duration))
		if err != nil {
			return Undefined, rt.intlTemporalRange("Duration was not valid.")
		}
		out := make([]Value, len(pieces))
		for i, piece := range pieces {
			part := rt.partObject(string(piece.Kind), piece.Value)
			if piece.Unit != "" {
				rt.putString(part, "unit", piece.Unit)
			}
			out[i] = Obj(part)
		}
		return Obj(rt.newArrayFrom(out)), nil
	})
	r.defMethod(proto, "resolvedOptions", 0, func(rt *Runtime, this Value, args []Value) (Value, error) {
		o, err := rt.durationFormatOf(this, "Intl.DurationFormat.prototype.resolvedOptions")
		if err != nil {
			return Undefined, err
		}
		resolved := o.format.ResolvedOptions()
		out := newObject(rt.proto.object, ClassObject)
		rt.putString(out, "locale", resolved.Locale)
		rt.putString(out, "numberingSystem", resolved.NumberingSystem)
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
	defer r.enterIntl("Intl.DurationFormat")()
	tags, err := r.requestedLocales(arg(args, 0))
	if err != nil {
		return nil, err
	}
	options, err := r.strictOptions(arg(args, 1))
	if err != nil {
		return nil, err
	}
	matcher, err := r.stringOption(options, "localeMatcher", "best fit", "lookup", "best fit")
	if err != nil {
		return nil, err
	}
	numbering, err := r.typeOption(options, "numberingSystem")
	if err != nil {
		return nil, err
	}
	loc, err := r.intlLocale(intl.ServiceDurationFormat, tags, matcher)
	if err != nil {
		return nil, err
	}
	o := &durationOptions{}
	if o.style, err = r.stringOption(options, "style", "short",
		"long", "short", "narrow", "digital"); err != nil {
		return nil, err
	}
	if err := r.readDurationUnits(o, options); err != nil {
		return nil, err
	}
	opts := intl.DurationFormatOptions{NumberingSystem: numbering, Compat: r.intlCompat(),
		Style: map[string]intl.DurationStyle{"short": intl.DurationShort, "long": intl.DurationLong,
			"narrow": intl.DurationNarrow, "digital": intl.DurationDigital}[o.style]}
	for i := range durationUnits {
		opts.Units[i] = map[string]intl.DurationUnitStyle{"long": intl.DurationUnitLong,
			"short": intl.DurationUnitShort, "narrow": intl.DurationUnitNarrow,
			"numeric": intl.DurationUnitNumeric, "2-digit": intl.DurationUnitTwoDigit}[o.askedWidths[i]]
		opts.Display[i] = map[string]intl.DurationDisplay{"auto": intl.DurationDisplayAuto,
			"always": intl.DurationDisplayAlways}[o.askedShown[i]]
	}
	if o.hasFractional {
		opts.FractionalDigits = intl.Digits(o.fractionalDigits)
	}
	if o.format, err = intl.NewDurationFormat(loc, opts); err != nil {
		// What go-intl refuses of the options that reading them did not,
		// which V8 reports with the options object.
		return nil, r.intlInvalidRange("object", r.v8Describe(Obj(options)))
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
		o.askedWidths[i] = width
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
				return r.intlInvalidRange("object", r.v8Describe(Obj(options)))
			}
			if unit.field == "minutes" || unit.field == "seconds" {
				width = "2-digit"
			}
		}
		if o.askedShown[i], err = r.stringOption(options, unit.field+"Display", "",
			"auto", "always"); err != nil {
			return err
		}
		o.shown[i] = o.askedShown[i]
		if o.shown[i] == "" {
			o.shown[i] = display
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
		// A string is a Temporal duration, which ToTemporalDuration parses.
		d, err := r.toTemporalDuration(v)
		if err != nil {
			return out, err
		}
		return [10]float64(temporalDurationFields(d)), nil
	}
	o := v.Object()
	if o == nil {
		return out, r.intlTemporalType("Duration argument must be Duration or string.")
	}
	if duration, ok := o.data.(temporal.Duration); ok {
		return [10]float64(temporalDurationFields(duration)), nil
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
			return out, r.intlTemporalRange("Expected finite integer.")
		}
		out[i] = n
		any = true
	}
	if !any {
		return out, r.intlTemporalType("Did not provide any valid Duration fields.")
	}
	if !validDuration(out) {
		return out, r.intlTemporalRange("Duration was not valid.")
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

func (r *Runtime) durationFormatOf(this Value, method string) (*durationOptions, error) {
	if o := this.Object(); o != nil {
		if opts, ok := o.data.(*durationOptions); ok {
			return opts, nil
		}
	}
	return nil, r.intlIncompatibleReceiver(method, this)
}
