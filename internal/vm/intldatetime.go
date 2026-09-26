package vm

import (
	"errors"
	"math"
	"time"

	intl "github.com/go-quickjs/go-intl"
	"github.com/go-quickjs/go-intl/temporal"
)

// Intl.DateTimeFormat, on go-intl, and the toLocaleString methods of Date and
// of Temporal's types, which are defined in terms of it.
//
// A Temporal value is written as V8 writes one: its calendar is checked
// against the formatter's, go-intl's PlainInstant makes an instant of a
// plain value's fields, and its ForTemporal makes the formatter for the
// value's kind.

// dateTimeFormat is a resolved Intl.DateTimeFormat.
type dateTimeFormat struct {
	f *intl.DateTimeFormat
	// formatFn is the bound function the format getter hands out, kept so
	// that every ask answers with the same one.
	formatFn *Object
	// kinds are the formatters ForTemporal made, by kind, made when a value
	// of the kind is first written.
	kinds map[intl.TemporalKind]*intl.DateTimeFormat
}

// newDateTimeFormat is ECMA-402's CreateDateTimeFormat: the arguments read in
// its order, with its errors, and a formatter built from what they settle.
// required and defaults are the fields a method needs and the ones it
// supplies; zoned is a ZonedDateTime's zone, which its toLocaleString writes
// in, or empty.
func (r *Runtime) newDateTimeFormat(args []Value, required, defaults intl.DateTimeComponents,
	zoned string) (*dateTimeFormat, error) {
	defer r.enterIntl("Intl.DateTimeFormat")()
	tags, err := r.requestedLocales(arg(args, 0))
	if err != nil {
		return nil, err
	}
	options, err := r.optionsObject(arg(args, 1))
	if err != nil {
		return nil, err
	}
	opts := intl.DateTimeFormatOptions{Required: required, Defaults: defaults, Compat: r.intlCompat()}
	matcher, err := r.stringOption(options, "localeMatcher", "best fit", "lookup", "best fit")
	if err != nil {
		return nil, err
	}
	if opts.Calendar, err = r.typeOption(options, "calendar"); err != nil {
		return nil, err
	}
	if opts.NumberingSystem, err = r.typeOption(options, "numberingSystem"); err != nil {
		return nil, err
	}
	hour12, hour12Set, err := r.boolOption(options, "hour12")
	if err != nil {
		return nil, err
	}
	if hour12Set {
		opts.Hour12 = intl.Bool(hour12)
	}
	cycle, err := r.stringOption(options, "hourCycle", "", "h11", "h12", "h23", "h24")
	if err != nil {
		return nil, err
	}
	if !hour12Set {
		// hour12, given, says all there is to say, and the cycle is not
		// consulted.
		opts.HourCycle = map[string]intl.HourCycle{"h11": intl.H11, "h12": intl.H12,
			"h23": intl.H23, "h24": intl.H24}[cycle]
	}
	loc, err := r.intlLocale(intl.ServiceDateTimeFormat, tags, matcher)
	if err != nil {
		return nil, err
	}

	// An empty string is a zone that does not exist, which is not the same
	// as no zone at all.
	zoneValue, err := r.getProp(options, r.atoms.intern("timeZone"), Obj(options))
	if err != nil {
		return nil, err
	}
	switch {
	case zoneValue.IsUndefined() && zoned != "":
		opts.TimeZone, opts.ToLocaleStringTimeZone = zoned, true
	case zoneValue.IsUndefined():
		// The zone a Date is written in, where go-intl knows it.
		opts.TimeZone = r.localZoneName()
		if _, err := intl.ResolveTimeZone(intl.Embedded, opts.TimeZone, opts.Compat); err != nil {
			opts.TimeZone = "UTC"
		}
	case zoned != "":
		// A ZonedDateTime is written in its own zone and no other.
		return nil, r.throwTypeError("Invalid time zone specified: %s", zoned)
	default:
		name, err := r.toString(zoneValue)
		if err != nil {
			return nil, err
		}
		opts.TimeZone = name.Go()
		if _, err := intl.ResolveTimeZone(intl.Embedded, opts.TimeZone, opts.Compat); err != nil {
			return nil, r.throwRangeError("Invalid time zone specified: %s", opts.TimeZone)
		}
	}

	width := func(name string, allowed ...string) (intl.FieldWidth, error) {
		value, err := r.stringOption(options, name, "", allowed...)
		return map[string]intl.FieldWidth{"numeric": intl.WidthNumeric, "2-digit": intl.Width2Digit,
			"long": intl.WidthLong, "short": intl.WidthShort, "narrow": intl.WidthNarrow}[value], err
	}
	for _, field := range []struct {
		name    string
		into    *intl.FieldWidth
		allowed []string
	}{
		{"weekday", &opts.Weekday, []string{"narrow", "short", "long"}},
		{"era", &opts.Era, []string{"narrow", "short", "long"}},
		{"year", &opts.Year, []string{"numeric", "2-digit"}},
		{"month", &opts.Month, []string{"numeric", "2-digit", "narrow", "short", "long"}},
		{"day", &opts.Day, []string{"numeric", "2-digit"}},
		{"dayPeriod", &opts.DayPeriod, []string{"narrow", "short", "long"}},
		{"hour", &opts.Hour, []string{"numeric", "2-digit"}},
		{"minute", &opts.Minute, []string{"numeric", "2-digit"}},
		{"second", &opts.Second, []string{"numeric", "2-digit"}},
	} {
		if *field.into, err = width(field.name, field.allowed...); err != nil {
			return nil, err
		}
	}
	fractional, fractionalSet, err := r.intOption(options, "fractionalSecondDigits", 1, 3, 0)
	if err != nil {
		return nil, err
	}
	if fractionalSet {
		opts.FractionalSecondDigits = fractional
	}
	zoneName, err := r.stringOption(options, "timeZoneName", "", "short", "long", "shortOffset",
		"longOffset", "shortGeneric", "longGeneric")
	if err != nil {
		return nil, err
	}
	opts.TimeZoneName = map[string]intl.ZoneStyle{"short": intl.ZoneShort, "long": intl.ZoneLong,
		"shortOffset": intl.ZoneShortOffset, "longOffset": intl.ZoneLongOffset,
		"shortGeneric": intl.ZoneShortGeneric, "longGeneric": intl.ZoneLongGeneric}[zoneName]
	if _, err := r.stringOption(options, "formatMatcher", "best fit", "basic", "best fit"); err != nil {
		return nil, err
	}
	length := func(name string) (intl.DateTimeLength, error) {
		value, err := r.stringOption(options, name, "", "full", "long", "medium", "short")
		return map[string]intl.DateTimeLength{"full": intl.LengthFull, "long": intl.LengthLong,
			"medium": intl.LengthMedium, "short": intl.LengthShort}[value], err
	}
	if opts.DateStyle, err = length("dateStyle"); err != nil {
		return nil, err
	}
	if opts.TimeStyle, err = length("timeStyle"); err != nil {
		return nil, err
	}

	styled := opts.DateStyle != intl.LengthNone || opts.TimeStyle != intl.LengthNone
	fields := opts.Weekday != intl.WidthNone || opts.Era != intl.WidthNone || opts.Year != intl.WidthNone ||
		opts.Month != intl.WidthNone || opts.Day != intl.WidthNone || opts.DayPeriod != intl.WidthNone ||
		opts.Hour != intl.WidthNone || opts.Minute != intl.WidthNone || opts.Second != intl.WidthNone ||
		opts.FractionalSecondDigits != 0 || opts.TimeZoneName != intl.ZoneNone
	switch {
	case styled && fields:
		return nil, r.intlInvalidType("option", "option")
	// A method that writes a date will not be asked for a style of time,
	// and one that writes a time will not be asked for a style of date.
	case required == intl.ComponentsDate && opts.TimeStyle != intl.LengthNone:
		return nil, r.intlInvalidType("option", "timeStyle")
	case required == intl.ComponentsTime && opts.DateStyle != intl.LengthNone:
		return nil, r.intlInvalidType("option", "dateStyle")
	}

	f, err := intl.NewDateTimeFormat(loc, opts)
	if err != nil {
		return nil, r.intlInternal()
	}
	return &dateTimeFormat{f: f}, nil
}

// temporalKindOf is the kind of Temporal value go-intl writes a value as,
// and its calendar; false for a value that is none. A ZonedDateTime is not
// one: only its toLocaleString writes it.
func temporalKindOf(v Value) (intl.TemporalKind, string, bool) {
	if !v.IsObject() {
		return 0, "", false
	}
	switch value := v.Object().data.(type) {
	case temporal.PlainDate:
		return intl.TemporalPlainDate, value.Calendar().ID(), true
	case temporal.PlainDateTime:
		return intl.TemporalPlainDateTime, value.Calendar().ID(), true
	case temporal.PlainTime:
		return intl.TemporalPlainTime, "", true
	case temporal.PlainYearMonth:
		return intl.TemporalPlainYearMonth, value.Calendar().ID(), true
	case temporal.PlainMonthDay:
		return intl.TemporalPlainMonthDay, value.Calendar().ID(), true
	case temporal.Instant:
		return intl.TemporalInstant, "", true
	}
	return 0, "", false
}

// forValue is the formatter a value is written with, and the instant it
// is: the formatter's own for a Date's time value, and ForTemporal's for a
// Temporal value, whose calendar must be the formatter's.
func (r *Runtime) forValue(d *dateTimeFormat, v Value) (*intl.DateTimeFormat, time.Time, error) {
	if v.IsObject() {
		if _, ok := v.Object().data.(temporal.ZonedDateTime); ok {
			return nil, time.Time{}, r.throwTypeError("Invalid argument for Temporal %s", r.v8Describe(v))
		}
	}
	kind, calendar, ok := temporalKindOf(v)
	if !ok {
		t, err := r.dateTimeValue(v)
		return d.f, t, err
	}
	if !d.f.CalendarMatches(kind, calendar) {
		return nil, time.Time{}, r.throwRangeError("Mismatched calendars.")
	}
	f, err := d.forKind(kind)
	if errors.Is(err, intl.ErrTemporalFormat) {
		return nil, time.Time{}, r.throwTypeError("Invalid argument for Temporal %s", r.v8Describe(v))
	}
	if err != nil {
		return nil, time.Time{}, r.intlInternal()
	}
	return f, d.temporalInstant(v), nil
}

func (d *dateTimeFormat) forKind(kind intl.TemporalKind) (*intl.DateTimeFormat, error) {
	if f, ok := d.kinds[kind]; ok {
		return f, nil
	}
	f, err := d.f.ForTemporal(kind)
	if err != nil {
		return nil, err
	}
	if d.kinds == nil {
		d.kinds = map[intl.TemporalKind]*intl.DateTimeFormat{}
	}
	d.kinds[kind] = f
	return f, nil
}

// temporalInstant is the instant a Temporal value is written as: an
// instant's own, and the one go-intl makes of a plain value's fields.
func (d *dateTimeFormat) temporalInstant(v Value) time.Time {
	var at temporal.ISODateTime
	switch value := v.Object().data.(type) {
	case temporal.Instant:
		return temporalTime(value)
	case temporal.PlainDate:
		at.Date = value.ISO()
	case temporal.PlainDateTime:
		at = value.ISO()
	case temporal.PlainTime:
		at = temporal.ISODateTime{Date: temporal.ISODate{Year: 1970, Month: 1, Day: 1}, Time: value.ISO()}
	case temporal.PlainYearMonth:
		at.Date = value.ISO()
	case temporal.PlainMonthDay:
		at.Date = value.ISO()
	}
	t := at.Time
	return d.f.PlainInstant(at.Date.Year, time.Month(at.Date.Month), at.Date.Day, t.Hour, t.Minute, t.Second,
		(t.Millisecond*1000+t.Microsecond)*1000+t.Nanosecond)
}

// temporalTime is an Instant as a Go time, to the nanosecond.
func temporalTime(i temporal.Instant) time.Time {
	ms := i.EpochMilliseconds()
	hi, lo := i.EpochNanoseconds()
	// The nanoseconds within the millisecond, from the low word, which
	// holds them whatever the high one.
	_ = hi
	sub := int64(lo % 1_000_000)
	if sub < 0 {
		sub += 1_000_000
	}
	return time.UnixMilli(ms).Add(time.Duration(sub))
}

// dateTimeValue is a Date's time value, or now where there is none, as the
// instant a format writes: a whole number of milliseconds, counted towards
// the epoch, and within the time values a Date may hold.
func (r *Runtime) dateTimeValue(v Value) (time.Time, error) {
	if v.IsUndefined() {
		return time.UnixMilli(int64(r.now())), nil
	}
	n, err := r.toNumber(v)
	if err != nil {
		return time.Time{}, err
	}
	if math.IsNaN(n) || math.Abs(n) > 8.64e15 {
		return time.Time{}, r.intlInvalidTimeValue(false)
	}
	return time.UnixMilli(int64(math.Trunc(n))), nil
}

// formatDateTime writes a value, as FormatDateTime does.
func (r *Runtime) formatDateTime(d *dateTimeFormat, v Value) (string, error) {
	f, t, err := r.forValue(d, v)
	if err != nil {
		return "", err
	}
	return f.Format(t), nil
}

// temporalToLocaleString is a plain Temporal value's toLocaleString: a
// DateTimeFormat made with the fields the type needs and supplies, writing
// the value.
func (r *Runtime) temporalToLocaleString(v Value, args []Value, required, defaults intl.DateTimeComponents) (Value, error) {
	d, err := r.newDateTimeFormat(args, required, defaults, "")
	if err != nil {
		return Undefined, err
	}
	text, err := r.formatDateTime(d, v)
	if err != nil {
		return Undefined, err
	}
	return Str(NewString(text)), nil
}

// zonedToLocaleString is a ZonedDateTime's toLocaleString: its instant,
// written in its own zone, in a calendar that is its own or ISO's.
func (r *Runtime) zonedToLocaleString(zoned temporal.ZonedDateTime, args []Value) (Value, error) {
	d, err := r.newDateTimeFormat(args, intl.ComponentsAny, intl.ComponentsAll, zoned.TimeZone().Identifier())
	if err != nil {
		return Undefined, err
	}
	if !d.f.CalendarMatches(intl.TemporalPlainDateTime, zoned.Calendar().ID()) {
		return Undefined, r.throwRangeError("Mismatched calendars.")
	}
	f, err := d.forKind(intl.TemporalInstant)
	if err != nil {
		return Undefined, r.intlInternal()
	}
	return Str(NewString(f.Format(temporalTime(zoned.Instant())))), nil
}

// dateTimeRange writes one value against another, as FormatDateTimeRange
// does: two Date time values, or two Temporal values of one kind.
func (r *Runtime) dateTimeRange(this Value, args []Value, method string) (*rangePieces, error) {
	d, err := r.dateTimeFormatOf(this, method)
	if err != nil {
		return nil, err
	}
	if arg(args, 0).IsUndefined() || arg(args, 1).IsUndefined() {
		return nil, r.intlInvalidTimeValue(true)
	}
	fromValue, err := r.toDateTimeFormattable(arg(args, 0))
	if err != nil {
		return nil, err
	}
	toValue, err := r.toDateTimeFormattable(arg(args, 1))
	if err != nil {
		return nil, err
	}
	fromKind, toKind := temporalDateTimeKind(fromValue), temporalDateTimeKind(toValue)
	if (fromKind != "" || toKind != "") && fromKind != toKind {
		return nil, r.throwTypeError("Invalid argument for Temporal %s", r.v8Describe(toValue))
	}
	f, from, err := r.forValue(d, fromValue)
	if err != nil {
		return nil, err
	}
	_, to, err := r.forValue(d, toValue)
	if err != nil {
		return nil, err
	}
	pieces := &rangePieces{}
	for _, p := range f.FormatRangeToParts(from, to) {
		source := "shared"
		switch p.Source {
		case intl.SourceStartRange:
			source = "startRange"
		case intl.SourceEndRange:
			source = "endRange"
		}
		pieces.add(string(p.Kind), p.Value, source)
	}
	return pieces, nil
}

func (r *Runtime) dateTimeFormatOf(this Value, method string) (*dateTimeFormat, error) {
	if o := r.unwrapFormatter(this).Object(); o != nil {
		if d, ok := o.data.(*dateTimeFormat); ok {
			return d, nil
		}
	}
	// V8 unwraps an object as a legacy formatter first, and reports one that
	// is not a DateTimeFormat as UnwrapDateTimeFormat's.
	if this.IsObject() && method == "get Intl.DateTimeFormat.prototype.format" {
		method = "UnwrapDateTimeFormat"
	}
	return nil, r.intlIncompatibleReceiver(method, this)
}

// resolvedDateTimeOptions is resolvedOptions' object, its properties in the
// order ECMA-402 lists them, which a script can see.
func (r *Runtime) resolvedDateTimeOptions(d *dateTimeFormat) *Object {
	o := d.f.ResolvedOptions()
	out := newObject(r.proto.object, ClassObject)
	r.putString(out, "locale", o.Locale)
	r.putString(out, "calendar", o.Calendar)
	r.putString(out, "numberingSystem", o.NumberingSystem)
	r.putString(out, "timeZone", o.TimeZone)
	if o.HourCycle != intl.HourCycleAuto {
		r.putString(out, "hourCycle", [...]string{"", "h11", "h12", "h23", "h24"}[o.HourCycle])
		r.putBool(out, "hour12", o.HourCycle == intl.H11 || o.HourCycle == intl.H12)
	}
	widths := [...]string{"", "numeric", "2-digit", "long", "short", "narrow"}
	for _, field := range []struct {
		name  string
		width intl.FieldWidth
	}{
		{"weekday", o.Weekday}, {"era", o.Era}, {"year", o.Year}, {"month", o.Month},
		{"day", o.Day}, {"dayPeriod", o.DayPeriod}, {"hour", o.Hour}, {"minute", o.Minute},
		{"second", o.Second},
	} {
		if field.width != intl.WidthNone {
			r.putString(out, field.name, widths[field.width])
		}
	}
	if o.FractionalSecondDigits > 0 {
		r.putInt(out, "fractionalSecondDigits", o.FractionalSecondDigits)
	}
	if o.TimeZoneName != intl.ZoneNone {
		r.putString(out, "timeZoneName", [...]string{"", "short", "long", "shortOffset", "longOffset",
			"shortGeneric", "longGeneric"}[o.TimeZoneName])
	}
	lengths := [...]string{"", "full", "long", "medium", "short"}
	if o.DateStyle != intl.LengthNone {
		r.putString(out, "dateStyle", lengths[o.DateStyle])
	}
	if o.TimeStyle != intl.LengthNone {
		r.putString(out, "timeStyle", lengths[o.TimeStyle])
	}
	return out
}
