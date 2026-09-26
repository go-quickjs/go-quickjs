package vm

import (
	"errors"
	"math"
	"strings"
	"unicode/utf16"

	intl "github.com/go-quickjs/go-intl"
	"github.com/go-quickjs/go-intl/temporal"
)

// Temporal, on go-intl's temporal package, which is temporal_rs 0.2.3 as
// Node's V8 builds Temporal from. What V8 does before it calls temporal_rs
// is V8's, and is here (js-temporal-objects.cc): reading property bags and
// options in its order, with its conversions, clamping a field to the width
// temporal_capi takes it in, and the messages it throws itself. A Temporal
// object holds the package's value, and each method is the package's.

// temporalData is Temporal's calendars and zones, read when the runtime
// first needs them.
func (r *Runtime) temporalData() (*temporal.Data, error) {
	if r.temporalLoaded == nil {
		d, err := temporal.LoadData(intl.Embedded)
		if err != nil {
			return nil, r.intlInternal()
		}
		r.temporalLoaded = d
	}
	return r.temporalLoaded, nil
}

// temporalCalendar is the calendar of an identifier temporal_rs gave.
func (r *Runtime) temporalCalendar(id string) (*temporal.Calendar, error) {
	d, err := r.temporalData()
	if err != nil {
		return nil, err
	}
	c, err := d.Calendar(id)
	if err != nil {
		return nil, r.intlInternal()
	}
	return c, nil
}

// ==== Errors ====

// temporalType and temporalRange are the errors V8 throws itself, with
// temporal_rs's prefix.
func (r *Runtime) temporalType(msg string) error { return r.throwTypeError("Temporal error: %s", msg) }
func (r *Runtime) temporalRange(msg string) error {
	return r.throwRangeError("Temporal error: %s", msg)
}

// temporalOptionType is V8's "Option must be object: name." TypeError.
func (r *Runtime) temporalOptionType(name string) error {
	return r.throwTypeError("Temporal error: Option must be object: %s.", name)
}

// temporalErr is the error V8 throws for a temporal_rs error: a RangeError
// or TypeError with its message, anything else an Error saying it is
// internal.
func (r *Runtime) temporalErr(err error) error {
	if err == nil {
		return nil
	}
	var thrown *Thrown
	if errors.As(err, &thrown) {
		return err
	}
	msg := err.Error()
	switch {
	case errors.Is(err, temporal.ErrRange):
		return r.temporalRange(strings.TrimPrefix(msg, "RangeError: "))
	case errors.Is(err, temporal.ErrType):
		return r.temporalType(strings.TrimPrefix(msg, "TypeError: "))
	}
	return r.throwError(errError, "Temporal error: Internal error: %s.", strings.TrimPrefix(msg, "Error: "))
}

// ==== Values ====

// temporalStdString is V8's String::ToStdString: UTF-8, a lone surrogate
// as U+FFFD.
func temporalStdString(s *String) string {
	return string(utf16.Decode(s.codeUnits()))
}

// temporalBytes is the bytes V8 hands temporal_rs for a string to parse
// (HandleStringEncodings).
func (r *Runtime) temporalBytes(s *String) ([]byte, error) {
	b, err := temporal.JSStringBytes(s.codeUnits())
	if err != nil {
		return nil, r.temporalErr(err)
	}
	return b, nil
}

// temporalInteger is ToIntegerWithTruncation.
func (r *Runtime) temporalInteger(v Value) (float64, error) {
	n, err := r.toNumber(v)
	if err != nil {
		return 0, err
	}
	if math.IsNaN(n) || math.IsInf(n, 0) {
		return 0, r.temporalRange("Expected finite integer.")
	}
	return math.Trunc(n) + 0, nil
}

// temporalIntegral is ToIntegerIfIntegral.
func (r *Runtime) temporalIntegral(v Value) (float64, error) {
	n, err := r.toNumber(v)
	if err != nil {
		return 0, err
	}
	if math.IsNaN(n) || math.IsInf(n, 0) || math.Trunc(n) != n {
		return 0, r.temporalRange("Expected finite integer.")
	}
	return n, nil
}

// temporalPositive is ToPositiveIntegerWithTruncation.
func (r *Runtime) temporalPositive(v Value) (float64, error) {
	n, err := r.temporalInteger(v)
	if err != nil {
		return 0, err
	}
	if n <= 0 {
		return 0, r.temporalRange("Expected positive integer.")
	}
	return n, nil
}

// temporalGet is Get of a property of an object, or undefined where there
// is no object.
func (r *Runtime) temporalGet(o *Object, name string) (Value, error) {
	if o == nil {
		return Undefined, nil
	}
	return r.getProp(o, r.atoms.intern(name), Obj(o))
}

// temporalValueOf is the Temporal value an object holds, or nil.
func temporalValueOf(v Value) any {
	if !v.IsObject() {
		return nil
	}
	switch x := v.Object().data.(type) {
	case temporal.PlainDate, temporal.PlainDateTime, temporal.PlainTime, temporal.PlainYearMonth,
		temporal.PlainMonthDay, temporal.Instant, temporal.ZonedDateTime, temporal.Duration:
		return x
	}
	return nil
}

// ==== Options ====

// temporalOptionsObject is GetOptionsObject: nil for undefined, which reads
// as an object with no properties.
func (r *Runtime) temporalOptionsObject(v Value) (*Object, error) {
	switch {
	case v.IsUndefined():
		return nil, nil
	case v.IsObject():
		return v.Object(), nil
	}
	return nil, r.intlInvalidArgumentType()
}

// temporalStringOption is V8's GetStringOption over a list of values: the
// index of the value, def where the option is absent, and an error where it
// is required (def < 0).
func (r *Runtime) temporalStringOption(o *Object, key, method string, values []string, def int) (int, error) {
	v, err := r.temporalGet(o, key)
	if err != nil {
		return 0, err
	}
	if v.IsUndefined() {
		if def >= 0 {
			return def, nil
		}
		return 0, r.throwRangeError("Value undefined out of range for %s options property %s", method, key)
	}
	s, err := r.toString(v)
	if err != nil {
		return 0, err
	}
	for i, x := range values {
		if s.Go() == x {
			return i, nil
		}
	}
	return 0, r.throwRangeError("Value %s out of range for %s options property %s", s.Go(), method, key)
}

var temporalUnitStrings = []string{"year", "month", "week", "day", "hour", "minute", "second", "millisecond",
	"microsecond", "nanosecond", "auto", "years", "months", "weeks", "days", "hours", "minutes", "seconds",
	"milliseconds", "microseconds", "nanoseconds"}

// temporalUnitOption is GetTemporalUnitValuedOption: NoUnit where unset.
func (r *Runtime) temporalUnitOption(o *Object, key, method string, required bool) (temporal.Unit, error) {
	def := len(temporalUnitStrings)
	if required {
		def = -1
	}
	i, err := r.temporalStringOption(o, key, method, temporalUnitStrings, def)
	if err != nil {
		return 0, err
	}
	if i == len(temporalUnitStrings) {
		return temporal.NoUnit, nil
	}
	u, _ := temporal.ParseUnit(temporalUnitStrings[i])
	return u, nil
}

type temporalUnitGroup int

const (
	temporalGroupDate temporalUnitGroup = iota
	temporalGroupTime
	temporalGroupDateTime
)

// temporalValidateUnit is ValidateTemporalUnitValue.
func (r *Runtime) temporalValidateUnit(u temporal.Unit, group temporalUnitGroup, extra temporal.Unit) error {
	if u == temporal.NoUnit || u == extra && extra != temporal.NoUnit {
		return nil
	}
	if u == temporal.UnitAuto {
		return r.temporalRange("Auto unit not allowed here")
	}
	if u.IsDateUnit() {
		if group == temporalGroupDate || group == temporalGroupDateTime {
			return nil
		}
		return r.temporalRange("Found date unit, expect time unit")
	}
	if group == temporalGroupTime || group == temporalGroupDateTime {
		return nil
	}
	return r.temporalRange("Found date unit, expect time unit")
}

func (r *Runtime) temporalRoundingIncrement(o *Object) (temporal.RoundingIncrement, error) {
	v, err := r.temporalGet(o, "roundingIncrement")
	if err != nil || v.IsUndefined() {
		return 1, err
	}
	n, err := r.temporalInteger(v)
	if err != nil {
		return 0, err
	}
	if n < 1 || n > 1e9 {
		return 0, r.temporalRange("Integer out of range.")
	}
	return temporal.RoundingIncrement(n), nil
}

var temporalModeStrings = []string{"ceil", "floor", "expand", "trunc", "halfCeil", "halfFloor", "halfExpand",
	"halfTrunc", "halfEven"}

func (r *Runtime) temporalRoundingMode(o *Object, method string, def temporal.RoundingMode) (temporal.RoundingMode, error) {
	i, err := r.temporalStringOption(o, "roundingMode", method, temporalModeStrings, int(def-temporal.Ceil))
	if err != nil {
		return 0, err
	}
	return temporal.RoundingMode(i) + temporal.Ceil, nil
}

// temporalFractionalSecondDigits is GetTemporalFractionalSecondDigitsOption.
func (r *Runtime) temporalFractionalSecondDigits(o *Object) (temporal.Precision, error) {
	v, err := r.temporalGet(o, "fractionalSecondDigits")
	if err != nil || v.IsUndefined() {
		return temporal.PrecisionAuto, err
	}
	if !v.IsNumber() {
		s, err := r.toString(v)
		if err != nil {
			return 0, err
		}
		if s.Go() != "auto" {
			return 0, r.intlPropertyOutOfRange("fractionalSecondDigits")
		}
		return temporal.PrecisionAuto, nil
	}
	n := v.Number()
	if math.IsNaN(n) || math.IsInf(n, 0) {
		return 0, r.intlPropertyOutOfRange("fractionalSecondDigits")
	}
	d := math.Floor(n)
	if d < 0 || d > 9 {
		return 0, r.intlPropertyOutOfRange("fractionalSecondDigits")
	}
	return temporal.Precision(d), nil
}

// temporalOverflow is GetTemporalOverflowOptionHandleUndefined.
func (r *Runtime) temporalOverflow(opts Value, method string) (temporal.Overflow, error) {
	if opts.IsUndefined() {
		return temporal.Constrain, nil
	}
	if !opts.IsObject() {
		return 0, r.temporalOptionType("overflow")
	}
	i, err := r.temporalStringOption(opts.Object(), "overflow", method, []string{"constrain", "reject"}, 0)
	return temporal.Overflow(i), err
}

// temporalDisambiguation is GetTemporalDisambiguationOptionHandleUndefined.
func (r *Runtime) temporalDisambiguation(opts Value, method string) (temporal.Disambiguation, error) {
	if opts.IsUndefined() {
		return temporal.Compatible, nil
	}
	if !opts.IsObject() {
		return 0, r.temporalOptionType("disambiguation")
	}
	i, err := r.temporalStringOption(opts.Object(), "disambiguation", method,
		[]string{"compatible", "earlier", "later", "reject"}, 0)
	return temporal.Disambiguation(i), err
}

var temporalOffsets = []temporal.OffsetDisambiguation{temporal.OffsetPrefer, temporal.OffsetUse,
	temporal.OffsetIgnore, temporal.OffsetReject}

// temporalOffsetOption is GetTemporalOffsetOptionHandleUndefined.
func (r *Runtime) temporalOffsetOption(opts Value, method string, fallback temporal.OffsetDisambiguation) (temporal.OffsetDisambiguation, error) {
	if opts.IsUndefined() {
		return fallback, nil
	}
	if !opts.IsObject() {
		return 0, r.temporalOptionType("offset")
	}
	def := 0
	for i, x := range temporalOffsets {
		if x == fallback {
			def = i
		}
	}
	i, err := r.temporalStringOption(opts.Object(), "offset", method, []string{"prefer", "use", "ignore", "reject"}, def)
	return temporalOffsets[i], err
}

// temporalZonedOptions is GetZDTOptions: the disambiguation, offset and
// overflow, read only where options were given.
func (r *Runtime) temporalZonedOptions(opts Value, method string, present bool) (temporal.Disambiguation,
	temporal.OffsetDisambiguation, temporal.Overflow, error) {
	if !present || opts.IsUndefined() {
		return temporal.Compatible, temporal.OffsetReject, temporal.Constrain, nil
	}
	if !opts.IsObject() {
		return 0, 0, 0, r.temporalOptionType("disambiguation")
	}
	o := opts.Object()
	d, err := r.temporalStringOption(o, "disambiguation", method, []string{"compatible", "earlier", "later", "reject"}, 0)
	if err != nil {
		return 0, 0, 0, err
	}
	od, err := r.temporalStringOption(o, "offset", method, []string{"prefer", "use", "ignore", "reject"}, 3)
	if err != nil {
		return 0, 0, 0, err
	}
	ov, err := r.temporalStringOption(o, "overflow", method, []string{"constrain", "reject"}, 0)
	if err != nil {
		return 0, 0, 0, err
	}
	return temporal.Disambiguation(d), temporalOffsets[od], temporal.Overflow(ov), nil
}

// temporalShowCalendar is GetTemporalShowCalendarNameOption.
func (r *Runtime) temporalShowCalendar(o *Object, method string) (temporal.DisplayCalendar, error) {
	i, err := r.temporalStringOption(o, "calendarName", method, []string{"auto", "always", "never", "critical"}, 0)
	return []temporal.DisplayCalendar{temporal.CalendarAuto, temporal.CalendarAlways, temporal.CalendarNever,
		temporal.CalendarCritical}[i], err
}

// temporalToStringOptions reads the fractional digits, the rounding mode
// and the smallest unit, with between read after the digits.
func (r *Runtime) temporalToStringOptions(o *Object, method string, between func() error) (temporal.ToStringRoundingOptions, error) {
	digits, err := r.temporalFractionalSecondDigits(o)
	if err != nil {
		return temporal.ToStringRoundingOptions{}, err
	}
	if between != nil {
		if err := between(); err != nil {
			return temporal.ToStringRoundingOptions{}, err
		}
	}
	mode, err := r.temporalRoundingMode(o, method, temporal.Trunc)
	if err != nil {
		return temporal.ToStringRoundingOptions{}, err
	}
	smallest, err := r.temporalUnitOption(o, "smallestUnit", method, false)
	if err != nil {
		return temporal.ToStringRoundingOptions{}, err
	}
	return temporal.ToStringRoundingOptions{Precision: digits, SmallestUnit: smallest, RoundingMode: mode}, nil
}

// temporalDifferenceSettings is GetDifferenceSettingsWithoutChecks.
func (r *Runtime) temporalDifferenceSettings(opts Value, method string) (temporal.DifferenceSettings, error) {
	o, err := r.temporalOptionsObject(opts)
	if err != nil {
		return temporal.DifferenceSettings{}, err
	}
	largest, err := r.temporalUnitOption(o, "largestUnit", method, false)
	if err != nil {
		return temporal.DifferenceSettings{}, err
	}
	inc, err := r.temporalRoundingIncrement(o)
	if err != nil {
		return temporal.DifferenceSettings{}, err
	}
	mode, err := r.temporalRoundingMode(o, method, temporal.Trunc)
	if err != nil {
		return temporal.DifferenceSettings{}, err
	}
	smallest, err := r.temporalUnitOption(o, "smallestUnit", method, false)
	if err != nil {
		return temporal.DifferenceSettings{}, err
	}
	return temporal.DifferenceSettings{LargestUnit: largest, SmallestUnit: smallest, RoundingMode: mode,
		Increment: inc, Compat: r.intlCompat()}, nil
}

// temporalRoundTo is round's reading of its argument: a string is the
// smallest unit. V8 checks for an object itself, but in Instant's round,
// where GetOptionsObject does.
func (r *Runtime) temporalRoundTo(v Value, optionsObject bool) (*Object, error) {
	switch {
	case v.IsUndefined():
		return nil, r.temporalType("Must specify a roundTo parameter.")
	case v.IsString():
		o := newObject(nil, ClassObject)
		o.setOwnRaw(r.atoms.intern("smallestUnit"), v, propDefault)
		return o, nil
	case v.IsObject():
		return v.Object(), nil
	case optionsObject:
		return nil, r.intlInvalidArgumentType()
	}
	return nil, r.temporalType("roundTo must be an object.")
}

// temporalRoundingOptions reads round's options: the increment, the mode,
// and the required smallest unit, a time unit or extra.
func (r *Runtime) temporalRoundingOptions(o *Object, method string, extra temporal.Unit) (temporal.RoundingOptions, error) {
	inc, err := r.temporalRoundingIncrement(o)
	if err != nil {
		return temporal.RoundingOptions{}, err
	}
	mode, err := r.temporalRoundingMode(o, method, temporal.HalfExpand)
	if err != nil {
		return temporal.RoundingOptions{}, err
	}
	smallest, err := r.temporalUnitOption(o, "smallestUnit", method, true)
	if err != nil {
		return temporal.RoundingOptions{}, err
	}
	if err := r.temporalValidateUnit(smallest, temporalGroupTime, extra); err != nil {
		return temporal.RoundingOptions{}, err
	}
	return temporal.RoundingOptions{LargestUnit: temporal.NoUnit, SmallestUnit: smallest, RoundingMode: mode,
		Increment: inc, Compat: r.intlCompat()}, nil
}

// ==== Calendars and zones ====

// temporalCalendarOf is the calendar of a calendared Temporal object.
func temporalCalendarOf(v Value) (*temporal.Calendar, bool) {
	switch x := temporalValueOf(v).(type) {
	case temporal.PlainDate:
		return x.Calendar(), true
	case temporal.PlainDateTime:
		return x.Calendar(), true
	case temporal.PlainMonthDay:
		return x.Calendar(), true
	case temporal.PlainYearMonth:
		return x.Calendar(), true
	case temporal.ZonedDateTime:
		return x.Calendar(), true
	}
	return nil, false
}

// temporalToCalendar is ToTemporalCalendarIdentifier.
func (r *Runtime) temporalToCalendar(v Value) (*temporal.Calendar, error) {
	if c, ok := temporalCalendarOf(v); ok {
		return c, nil
	}
	if !v.IsString() {
		return nil, r.temporalType("Calendar must be string or calendared Temporal object.")
	}
	id, ok := temporal.ParseCalendarString([]byte(temporalStdString(v.String())))
	if !ok {
		return nil, r.temporalRange("Invalid calendar string")
	}
	return r.temporalCalendar(id)
}

// temporalCanonicalCalendar is V8's CanonicalizeCalendar.
func (r *Runtime) temporalCanonicalCalendar(s *String) (*temporal.Calendar, error) {
	// Lowercased as ASCII: Unicode's lowercase of "İ" is an ASCII "i",
	// which would make "İSO8601" a calendar.
	text := temporalStdString(s)
	id, ok := temporal.CalendarID(temporalLowerASCII(text))
	if !ok {
		return nil, r.temporalRange("Unknown calendar type " + text + ".")
	}
	return r.temporalCalendar(id)
}

// temporalCalendarArg is a constructor's calendar: ISO where undefined,
// else a string CanonicalizeCalendar takes.
func (r *Runtime) temporalCalendarArg(v Value) (*temporal.Calendar, error) {
	if v.IsUndefined() {
		return temporal.ISOCalendar, nil
	}
	if !v.IsString() {
		return nil, r.temporalType("Calendar must be string.")
	}
	return r.temporalCanonicalCalendar(v.String())
}

// temporalCalendarWithISODefault is
// GetTemporalCalendarIdentifierWithISODefault.
func (r *Runtime) temporalCalendarWithISODefault(v Value) (*temporal.Calendar, error) {
	if c, ok := temporalCalendarOf(v); ok {
		return c, nil
	}
	c, err := r.temporalGet(v.Object(), "calendar")
	if err != nil {
		return nil, err
	}
	if c.IsUndefined() {
		return temporal.ISOCalendar, nil
	}
	return r.temporalToCalendar(c)
}

// temporalToTimeZone is ToTemporalTimeZoneIdentifier.
func (r *Runtime) temporalToTimeZone(v Value) (temporal.TimeZone, error) {
	if z, ok := temporalValueOf(v).(temporal.ZonedDateTime); ok {
		return z.TimeZone(), nil
	}
	if !v.IsString() {
		return temporal.TimeZone{}, r.temporalType("Time zone must be string or ZonedDateTime object.")
	}
	d, err := r.temporalData()
	if err != nil {
		return temporal.TimeZone{}, err
	}
	tz, err := d.Zones.TimeZoneFromString([]byte(temporalStdString(v.String())))
	return tz, r.temporalErr(err)
}

// ==== Property bags ====

type temporalFields int

const (
	temporalFieldsDay temporalFields = 1 << iota
	temporalFieldsMonth
	temporalFieldsYear
	temporalFieldsTime
	temporalFieldsOffset
	temporalFieldsTimeZone
	temporalFieldsDate = temporalFieldsDay | temporalFieldsMonth | temporalFieldsYear
)

type temporalRequired int

const (
	temporalRequireNone temporalRequired = iota
	temporalRequirePartial
	temporalRequireTimeZone
)

// temporalRecord is V8's CombinedRecord.
type temporalRecord struct {
	year, month, day, eraYear                                  *float64
	monthCode, era                                             *string
	hour, minute, second, millisecond, microsecond, nanosecond *float64
	offset                                                     *string
	timeZone                                                   *temporal.TimeZone
}

// temporalMonthCode is V8's ToMonthCode.
func (r *Runtime) temporalMonthCode(v Value) (string, error) {
	p, err := r.toPrimitive(v, hintString)
	if err != nil {
		return "", err
	}
	if !p.IsString() {
		return "", r.temporalType("Month code out of range.")
	}
	mc := temporalStdString(p.String())
	switch {
	case len(mc) != 3 && len(mc) != 4, mc[0] != 'M', mc[1] < '0' || mc[1] > '9', mc[2] < '0' || mc[2] > '9',
		len(mc) == 4 && mc[3] != 'L', mc[1] == '0' && mc[2] == '0' && len(mc) != 4:
		return "", r.temporalRange("Month code out of range.")
	}
	return mc, nil
}

// temporalOffsetString is V8's ToOffsetString.
func (r *Runtime) temporalOffsetString(v Value) (string, error) {
	p, err := r.toPrimitive(v, hintString)
	if err != nil {
		return "", err
	}
	if !p.IsString() {
		return "", r.temporalType("Offset must be string.")
	}
	s := temporalStdString(p.String())
	if _, err := temporal.ParseOffset([]byte(s)); err != nil {
		return "", r.temporalErr(err)
	}
	return s, nil
}

// temporalPrepareFields is PrepareCalendarFields: the fields asked for,
// read in the order of their names, each converted as it is read.
func (r *Runtime) temporalPrepareFields(cal *temporal.Calendar, o *Object, which temporalFields, req temporalRequired) (*temporalRecord, error) {
	rec := &temporalRecord{}
	eras := cal.HasEras()
	found := false
	num := func(key string, conv func(Value) (float64, error), out **float64) error {
		x, err := r.temporalGet(o, key)
		if err != nil || x.IsUndefined() {
			return err
		}
		found = true
		f, err := conv(x)
		if err != nil {
			return err
		}
		*out = &f
		return nil
	}
	steps := []func() error{
		func() error {
			if which&temporalFieldsDay == 0 {
				return nil
			}
			return num("day", r.temporalPositive, &rec.day)
		},
		func() error {
			if which&temporalFieldsYear == 0 || !eras {
				return nil
			}
			x, err := r.temporalGet(o, "era")
			if err != nil || x.IsUndefined() {
				return err
			}
			found = true
			s, err := r.toString(x)
			if err != nil {
				return err
			}
			text := temporalStdString(s)
			rec.era = &text
			return nil
		},
		func() error {
			if which&temporalFieldsYear == 0 || !eras {
				return nil
			}
			return num("eraYear", r.temporalInteger, &rec.eraYear)
		},
		func() error {
			if which&temporalFieldsTime == 0 {
				return nil
			}
			return num("hour", r.temporalInteger, &rec.hour)
		},
		func() error {
			if which&temporalFieldsTime == 0 {
				return nil
			}
			return num("microsecond", r.temporalInteger, &rec.microsecond)
		},
		func() error {
			if which&temporalFieldsTime == 0 {
				return nil
			}
			return num("millisecond", r.temporalInteger, &rec.millisecond)
		},
		func() error {
			if which&temporalFieldsTime == 0 {
				return nil
			}
			return num("minute", r.temporalInteger, &rec.minute)
		},
		func() error {
			if which&temporalFieldsMonth == 0 {
				return nil
			}
			return num("month", r.temporalPositive, &rec.month)
		},
		func() error {
			if which&temporalFieldsMonth == 0 {
				return nil
			}
			x, err := r.temporalGet(o, "monthCode")
			if err != nil || x.IsUndefined() {
				return err
			}
			found = true
			mc, err := r.temporalMonthCode(x)
			if err != nil {
				return err
			}
			rec.monthCode = &mc
			return nil
		},
		func() error {
			if which&temporalFieldsTime == 0 {
				return nil
			}
			return num("nanosecond", r.temporalInteger, &rec.nanosecond)
		},
		func() error {
			if which&temporalFieldsOffset == 0 {
				return nil
			}
			x, err := r.temporalGet(o, "offset")
			if err != nil || x.IsUndefined() {
				return err
			}
			found = true
			off, err := r.temporalOffsetString(x)
			if err != nil {
				return err
			}
			rec.offset = &off
			return nil
		},
		func() error {
			if which&temporalFieldsTime == 0 {
				return nil
			}
			return num("second", r.temporalInteger, &rec.second)
		},
		func() error {
			if which&temporalFieldsTimeZone == 0 {
				return nil
			}
			x, err := r.temporalGet(o, "timeZone")
			if err != nil {
				return err
			}
			if x.IsUndefined() {
				if req == temporalRequireTimeZone {
					return r.temporalType("Must specify time zone.")
				}
				return nil
			}
			found = true
			tz, err := r.temporalToTimeZone(x)
			if err != nil {
				return err
			}
			rec.timeZone = &tz
			return nil
		},
		func() error {
			if which&temporalFieldsYear == 0 {
				return nil
			}
			return num("year", r.temporalInteger, &rec.year)
		},
	}
	for _, step := range steps {
		if err := step(); err != nil {
			return nil, err
		}
	}
	if req == temporalRequirePartial && !found {
		return nil, r.temporalType("Must specify at least one calendar field.")
	}
	return rec, nil
}

func temporalClamp(f, lo, hi float64) float64 { return math.Max(lo, math.Min(hi, f)) }

// temporalRegulateDate is DateRecord::Regulate, and temporal_capi's
// conversion of the result.
func (r *Runtime) temporalRegulateDate(rec *temporalRecord, overflow temporal.Overflow) (temporal.CalendarFields, error) {
	var f temporal.CalendarFields
	if overflow == temporal.Constrain {
		if rec.year != nil {
			f.Year = temporal.Int(int(int32(temporalClamp(*rec.year, math.MinInt32, math.MaxInt32))))
		}
		// V8 clamps the month and day to an int8, and hands them to Rust
		// as a u8.
		if rec.month != nil {
			f.Month = temporal.Int(int(uint8(int8(temporalClamp(*rec.month, -128, 127)))))
		}
		if rec.day != nil {
			f.Day = temporal.Int(int(uint8(int8(temporalClamp(*rec.day, -128, 127)))))
		}
		if rec.eraYear != nil {
			f.EraYear = temporal.Int(int(int32(temporalClamp(*rec.eraYear, math.MinInt32, math.MaxInt32))))
		}
	} else {
		check := func(v *float64, lo, hi float64) (*int, error) {
			if v == nil {
				return nil, nil
			}
			if !(*v >= lo && *v <= hi) {
				return nil, r.temporalRange("Integer out of range.")
			}
			return temporal.Int(int(*v)), nil
		}
		var err error
		if f.Year, err = check(rec.year, math.MinInt32, math.MaxInt32); err != nil {
			return f, err
		}
		if f.Month, err = check(rec.month, 0, 255); err != nil {
			return f, err
		}
		if f.Day, err = check(rec.day, 0, 255); err != nil {
			return f, err
		}
		if f.EraYear, err = check(rec.eraYear, math.MinInt32, math.MaxInt32); err != nil {
			return f, err
		}
	}
	if rec.monthCode != nil && *rec.monthCode != "" {
		if err := temporal.ParseMonthCode(*rec.monthCode); err != nil {
			return f, r.temporalErr(err)
		}
		f.MonthCode = temporal.String(*rec.monthCode)
	}
	if rec.era != nil && *rec.era != "" {
		if len(*rec.era) > 19 || !temporalASCII(*rec.era) {
			return f, r.temporalRange("Invalid era code.")
		}
		f.Era = temporal.String(*rec.era)
	}
	return f, nil
}

// temporalLowerASCII is a string with its ASCII letters in lowercase, and
// nothing else changed.
func temporalLowerASCII(s string) string {
	b := []byte(s)
	for i, c := range b {
		if c >= 'A' && c <= 'Z' {
			b[i] = c + 'a' - 'A'
		}
	}
	return string(b)
}

func temporalASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= 0x80 {
			return false
		}
	}
	return true
}

var temporalTimeMaxes = [6]float64{23, 59, 59, 999, 999, 999}

// temporalRegulateTime is TimeRecord::Regulate.
func (r *Runtime) temporalRegulateTime(t [6]*float64, overflow temporal.Overflow) (temporal.PartialTime, error) {
	var p temporal.PartialTime
	out := [6]**int{&p.Hour, &p.Minute, &p.Second, &p.Millisecond, &p.Microsecond, &p.Nanosecond}
	if overflow == temporal.Constrain {
		for i, v := range t {
			if v != nil {
				*out[i] = temporal.Int(int(temporalClamp(*v, 0, temporalTimeMaxes[i])))
			}
		}
		return p, nil
	}
	for i, v := range t {
		x := 0.0
		if v != nil {
			x = *v
		}
		if x < 0 || x > temporalTimeMaxes[i] {
			return p, r.temporalRange("Invalid time provided")
		}
	}
	for i, v := range t {
		if v != nil {
			*out[i] = temporal.Int(int(*v))
		}
	}
	return p, nil
}

func (rec *temporalRecord) timeFields() [6]*float64 {
	return [6]*float64{rec.hour, rec.minute, rec.second, rec.millisecond, rec.microsecond, rec.nanosecond}
}

// temporalRegulateZoned is CombinedRecord::Regulate for a
// PartialZonedDateTime.
func (r *Runtime) temporalRegulateZoned(rec *temporalRecord, overflow temporal.Overflow) (temporal.PartialZonedDateTime, error) {
	var p temporal.PartialZonedDateTime
	var err error
	if p.Date, err = r.temporalRegulateDate(rec, overflow); err != nil {
		return p, err
	}
	if p.Time, err = r.temporalRegulateTime(rec.timeFields(), overflow); err != nil {
		return p, err
	}
	if rec.offset != nil {
		ns, err := temporal.ParseOffset([]byte(*rec.offset))
		if err != nil {
			return p, r.temporalErr(err)
		}
		p.Offset = &ns
	}
	return p, nil
}

// temporalTimeBag reads a property bag's time fields, in the order of
// their names, each converted as it is read; false where there are none.
func (r *Runtime) temporalTimeBag(o *Object) ([6]*float64, bool, error) {
	var t [6]*float64
	found := false
	for i, key := range []string{"hour", "microsecond", "millisecond", "minute", "nanosecond", "second"} {
		x, err := r.temporalGet(o, key)
		if err != nil {
			return t, false, err
		}
		if x.IsUndefined() {
			continue
		}
		f, err := r.temporalInteger(x)
		if err != nil {
			return t, false, err
		}
		found = true
		t[[6]int{0, 4, 3, 1, 5, 2}[i]] = &f
	}
	return t, found, nil
}

// temporalIsPartial is IsPartialTemporalObject.
func (r *Runtime) temporalIsPartial(v Value) (bool, error) {
	if !v.IsObject() || temporalValueOf(v) != nil {
		return false, nil
	}
	for _, key := range []string{"calendar", "timeZone"} {
		x, err := r.temporalGet(v.Object(), key)
		if err != nil {
			return false, err
		}
		if !x.IsUndefined() {
			return false, nil
		}
	}
	return true, nil
}

// temporalRequirePartial throws where a with() argument is no partial
// Temporal object.
func (r *Runtime) temporalRequirePartial(v Value) error {
	ok, err := r.temporalIsPartial(v)
	if err != nil {
		return err
	}
	if !ok {
		return r.temporalType("Argument to with() must contain some date/time fields.")
	}
	return nil
}

// temporalValidISODate is V8's IsValidIsoDate.
func temporalValidISODate(y, m, d float64) bool {
	if m < 1 || m > 12 || !(y >= math.MinInt32 && y <= math.MaxInt32) {
		return false
	}
	days := [...]float64{31, 28, 31, 30, 31, 30, 31, 31, 30, 31, 30, 31}[int(m)-1]
	if year := int64(y); m == 2 && year%4 == 0 && (year%100 != 0 || year%400 == 0) {
		days = 29
	}
	return d >= 1 && d <= days
}

func temporalValidTime(t [6]float64) bool {
	for i, v := range t {
		if v < 0 || v > temporalTimeMaxes[i] {
			return false
		}
	}
	return true
}

// temporalTimeArgs reads a constructor's time arguments, from index i.
func (r *Runtime) temporalTimeArgs(args []Value, i int) ([6]float64, error) {
	var t [6]float64
	for j := range t {
		v := arg(args, i+j)
		if v.IsUndefined() {
			continue
		}
		f, err := r.temporalInteger(v)
		if err != nil {
			return t, err
		}
		t[j] = f
	}
	return t, nil
}
