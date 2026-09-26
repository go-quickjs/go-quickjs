package vm

import (
	"math"
	"math/big"

	"github.com/go-quickjs/go-intl/temporal"
)

// The ToTemporalX operations, as V8 has them: a Temporal object is taken
// as it is or converted, a string is parsed, and an object is a property
// bag, read in V8's order.

// temporalDiscardOverflow reads the overflow option, which a value that is
// not a property bag has no use for but V8 reads all the same.
func (r *Runtime) temporalDiscardOverflow(opts Value, method string) error {
	_, err := r.temporalOverflow(opts, method)
	return err
}

// temporalPlainDateOf is PlainDate::from_partial of the date V8 takes from
// a Temporal value: its year, month and day in its calendar.
func (r *Runtime) temporalPlainDateOf(d temporal.PlainDate) (temporal.PlainDate, error) {
	out, err := temporal.PlainDateFromFields(temporal.CalendarFields{Year: temporal.Int(d.Year()),
		Month: temporal.Int(d.Month()), Day: temporal.Int(d.Day())}, d.Calendar(), temporal.Constrain)
	return out, r.temporalErr(err)
}

func (r *Runtime) toTemporalPlainTime(v, opts Value, method string) (temporal.PlainTime, error) {
	switch x := temporalValueOf(v).(type) {
	case temporal.PlainTime:
		return x, r.temporalDiscardOverflow(opts, method)
	case temporal.PlainDateTime:
		return x.ToPlainTime(), r.temporalDiscardOverflow(opts, method)
	case temporal.ZonedDateTime:
		return x.ToPlainTime(), r.temporalDiscardOverflow(opts, method)
	}
	if v.IsString() {
		b, err := r.temporalBytes(v.String())
		if err != nil {
			return temporal.PlainTime{}, err
		}
		t, err := temporal.ParsePlainTime(b)
		if err != nil {
			return temporal.PlainTime{}, r.temporalErr(err)
		}
		return t, r.temporalDiscardOverflow(opts, method)
	}
	if !v.IsObject() {
		return temporal.PlainTime{}, r.temporalType("Time-like argument must be object or string")
	}
	t, found, err := r.temporalTimeBag(v.Object())
	if err != nil {
		return temporal.PlainTime{}, err
	}
	if !found {
		return temporal.PlainTime{}, r.temporalType("Must specify at least one time field.")
	}
	zero := 0.0
	for i := range t {
		if t[i] == nil {
			t[i] = &zero
		}
	}
	overflow, err := r.temporalOverflow(opts, method)
	if err != nil {
		return temporal.PlainTime{}, err
	}
	p, err := r.temporalRegulateTime(t, overflow)
	if err != nil {
		return temporal.PlainTime{}, err
	}
	pt, err := temporal.PlainTimeFromPartial(p, overflow)
	return pt, r.temporalErr(err)
}

func (r *Runtime) toTemporalPlainDate(v, opts Value, method string) (temporal.PlainDate, error) {
	switch x := temporalValueOf(v).(type) {
	case temporal.PlainDate:
		return x, r.temporalDiscardOverflow(opts, method)
	case temporal.ZonedDateTime:
		if err := r.temporalDiscardOverflow(opts, method); err != nil {
			return temporal.PlainDate{}, err
		}
		return r.temporalPlainDateOf(x.ToPlainDate())
	case temporal.PlainDateTime:
		if err := r.temporalDiscardOverflow(opts, method); err != nil {
			return temporal.PlainDate{}, err
		}
		return r.temporalPlainDateOf(x.ToPlainDate())
	}
	if v.IsString() {
		b, err := r.temporalBytes(v.String())
		if err != nil {
			return temporal.PlainDate{}, err
		}
		p, err := temporal.ParseDate(b)
		if err != nil {
			return temporal.PlainDate{}, r.temporalErr(err)
		}
		if err := r.temporalDiscardOverflow(opts, method); err != nil {
			return temporal.PlainDate{}, err
		}
		cal, err := r.temporalCalendar(p.Calendar())
		if err != nil {
			return temporal.PlainDate{}, err
		}
		d, err := temporal.PlainDateFromParsed(p, cal)
		return d, r.temporalErr(err)
	}
	if !v.IsObject() {
		return temporal.PlainDate{}, r.temporalType("Date argument must be object or string.")
	}
	cal, err := r.temporalCalendarWithISODefault(v)
	if err != nil {
		return temporal.PlainDate{}, err
	}
	rec, err := r.temporalPrepareFields(cal, v.Object(), temporalFieldsDate, temporalRequireNone)
	if err != nil {
		return temporal.PlainDate{}, err
	}
	overflow, err := r.temporalOverflow(opts, method)
	if err != nil {
		return temporal.PlainDate{}, err
	}
	f, err := r.temporalRegulateDate(rec, overflow)
	if err != nil {
		return temporal.PlainDate{}, err
	}
	d, err := temporal.PlainDateFromFields(f, cal, overflow)
	return d, r.temporalErr(err)
}

func (r *Runtime) toTemporalPlainDateTime(v, opts Value, method string) (temporal.PlainDateTime, error) {
	var d temporal.PlainDate
	var t temporal.PartialTime
	switch x := temporalValueOf(v).(type) {
	case temporal.PlainDateTime:
		return x, r.temporalDiscardOverflow(opts, method)
	case temporal.ZonedDateTime:
		d = x.ToPlainDate()
		it := x.ToPlainTime().ISO()
		t = temporal.PartialTime{Hour: temporal.Int(it.Hour), Minute: temporal.Int(it.Minute),
			Second: temporal.Int(it.Second), Millisecond: temporal.Int(it.Millisecond),
			Microsecond: temporal.Int(it.Microsecond), Nanosecond: temporal.Int(it.Nanosecond)}
	case temporal.PlainDate:
		d = x
	default:
		if v.IsString() {
			b, err := r.temporalBytes(v.String())
			if err != nil {
				return temporal.PlainDateTime{}, err
			}
			p, err := temporal.ParseDateTime(b)
			if err != nil {
				return temporal.PlainDateTime{}, r.temporalErr(err)
			}
			if err := r.temporalDiscardOverflow(opts, method); err != nil {
				return temporal.PlainDateTime{}, err
			}
			cal, err := r.temporalCalendar(p.Calendar())
			if err != nil {
				return temporal.PlainDateTime{}, err
			}
			dt, err := temporal.PlainDateTimeFromParsed(p, cal)
			return dt, r.temporalErr(err)
		}
		if !v.IsObject() {
			return temporal.PlainDateTime{}, r.temporalType("DateTime argument must be object or string.")
		}
		cal, err := r.temporalCalendarWithISODefault(v)
		if err != nil {
			return temporal.PlainDateTime{}, err
		}
		rec, err := r.temporalPrepareFields(cal, v.Object(), temporalFieldsDate|temporalFieldsTime, temporalRequireNone)
		if err != nil {
			return temporal.PlainDateTime{}, err
		}
		overflow, err := r.temporalOverflow(opts, method)
		if err != nil {
			return temporal.PlainDateTime{}, err
		}
		f, err := r.temporalRegulateDate(rec, overflow)
		if err != nil {
			return temporal.PlainDateTime{}, err
		}
		pt, err := r.temporalRegulateTime(rec.timeFields(), overflow)
		if err != nil {
			return temporal.PlainDateTime{}, err
		}
		dt, err := temporal.PlainDateTimeFromFields(f, pt, cal, overflow)
		return dt, r.temporalErr(err)
	}
	overflow, err := r.temporalOverflow(opts, method)
	if err != nil {
		return temporal.PlainDateTime{}, err
	}
	dt, err := temporal.PlainDateTimeFromFields(temporal.CalendarFields{Year: temporal.Int(d.Year()),
		Month: temporal.Int(d.Month()), Day: temporal.Int(d.Day())}, t, d.Calendar(), overflow)
	return dt, r.temporalErr(err)
}

func (r *Runtime) toTemporalPlainYearMonth(v, opts Value, method string) (temporal.PlainYearMonth, error) {
	if x, ok := temporalValueOf(v).(temporal.PlainYearMonth); ok {
		return x, r.temporalDiscardOverflow(opts, method)
	}
	if v.IsString() {
		b, err := r.temporalBytes(v.String())
		if err != nil {
			return temporal.PlainYearMonth{}, err
		}
		p, err := temporal.ParseYearMonth(b)
		if err != nil {
			return temporal.PlainYearMonth{}, r.temporalErr(err)
		}
		if err := r.temporalDiscardOverflow(opts, method); err != nil {
			return temporal.PlainYearMonth{}, err
		}
		cal, err := r.temporalCalendar(p.Calendar())
		if err != nil {
			return temporal.PlainYearMonth{}, err
		}
		ym, err := temporal.PlainYearMonthFromParsed(p, cal)
		return ym, r.temporalErr(err)
	}
	if !v.IsObject() {
		return temporal.PlainYearMonth{}, r.temporalType("YearMonth argument must be object or string.")
	}
	cal, err := r.temporalCalendarWithISODefault(v)
	if err != nil {
		return temporal.PlainYearMonth{}, err
	}
	rec, err := r.temporalPrepareFields(cal, v.Object(), temporalFieldsYear|temporalFieldsMonth, temporalRequireNone)
	if err != nil {
		return temporal.PlainYearMonth{}, err
	}
	overflow, err := r.temporalOverflow(opts, method)
	if err != nil {
		return temporal.PlainYearMonth{}, err
	}
	f, err := r.temporalRegulateDate(rec, overflow)
	if err != nil {
		return temporal.PlainYearMonth{}, err
	}
	ym, err := temporal.PlainYearMonthFromFields(f, cal, overflow)
	return ym, r.temporalErr(err)
}

func (r *Runtime) toTemporalPlainMonthDay(v, opts Value, method string) (temporal.PlainMonthDay, error) {
	if x, ok := temporalValueOf(v).(temporal.PlainMonthDay); ok {
		return x, r.temporalDiscardOverflow(opts, method)
	}
	if v.IsString() {
		b, err := r.temporalBytes(v.String())
		if err != nil {
			return temporal.PlainMonthDay{}, err
		}
		p, err := temporal.ParseMonthDay(b)
		if err != nil {
			return temporal.PlainMonthDay{}, r.temporalErr(err)
		}
		if err := r.temporalDiscardOverflow(opts, method); err != nil {
			return temporal.PlainMonthDay{}, err
		}
		cal, err := r.temporalCalendar(p.Calendar())
		if err != nil {
			return temporal.PlainMonthDay{}, err
		}
		md, err := temporal.PlainMonthDayFromParsed(p, cal)
		return md, r.temporalErr(err)
	}
	if !v.IsObject() {
		return temporal.PlainMonthDay{}, r.temporalType("MonthDay argument must be object or string.")
	}
	cal, err := r.temporalCalendarWithISODefault(v)
	if err != nil {
		return temporal.PlainMonthDay{}, err
	}
	rec, err := r.temporalPrepareFields(cal, v.Object(), temporalFieldsDate, temporalRequireNone)
	if err != nil {
		return temporal.PlainMonthDay{}, err
	}
	overflow, err := r.temporalOverflow(opts, method)
	if err != nil {
		return temporal.PlainMonthDay{}, err
	}
	f, err := r.temporalRegulateDate(rec, overflow)
	if err != nil {
		return temporal.PlainMonthDay{}, err
	}
	md, err := temporal.PlainMonthDayFromFields(f, cal, overflow)
	return md, r.temporalErr(err)
}

// toTemporalInstant is ToTemporalInstant: an object other than an Instant
// or a ZonedDateTime is read as a string.
func (r *Runtime) toTemporalInstant(v Value) (temporal.Instant, error) {
	switch x := temporalValueOf(v).(type) {
	case temporal.Instant:
		return x, nil
	case temporal.ZonedDateTime:
		return x.Instant(), nil
	}
	if v.IsObject() {
		p, err := r.toPrimitive(v, hintString)
		if err != nil {
			return temporal.Instant{}, err
		}
		v = p
	}
	if !v.IsString() {
		return temporal.Instant{}, r.temporalType("Instant argument must be Instant or string.")
	}
	b, err := r.temporalBytes(v.String())
	if err != nil {
		return temporal.Instant{}, err
	}
	i, err := temporal.ParseInstant(b)
	return i, r.temporalErr(err)
}

// toTemporalZonedDateTime is ToTemporalZonedDateTime; present says whether
// its options are read at all.
func (r *Runtime) toTemporalZonedDateTime(v, opts Value, method string, present bool) (temporal.ZonedDateTime, error) {
	if x, ok := temporalValueOf(v).(temporal.ZonedDateTime); ok {
		if _, _, _, err := r.temporalZonedOptions(opts, method, present); err != nil {
			return temporal.ZonedDateTime{}, err
		}
		return x, nil
	}
	d, err := r.temporalData()
	if err != nil {
		return temporal.ZonedDateTime{}, err
	}
	if v.IsString() {
		b, err := r.temporalBytes(v.String())
		if err != nil {
			return temporal.ZonedDateTime{}, err
		}
		p, err := d.Zones.ParseZonedDateTime(b)
		if err != nil {
			return temporal.ZonedDateTime{}, r.temporalErr(err)
		}
		dis, off, _, err := r.temporalZonedOptions(opts, method, present)
		if err != nil {
			return temporal.ZonedDateTime{}, err
		}
		cal, err := r.temporalCalendar(p.Calendar())
		if err != nil {
			return temporal.ZonedDateTime{}, err
		}
		z, err := temporal.ZonedDateTimeFromParsed(p, cal, dis, off)
		return z, r.temporalErr(err)
	}
	if !v.IsObject() {
		return temporal.ZonedDateTime{}, r.temporalType("ZonedDateTime argument must be object or string.")
	}
	cal, err := r.temporalCalendarWithISODefault(v)
	if err != nil {
		return temporal.ZonedDateTime{}, err
	}
	rec, err := r.temporalPrepareFields(cal, v.Object(),
		temporalFieldsDate|temporalFieldsTime|temporalFieldsOffset|temporalFieldsTimeZone, temporalRequireTimeZone)
	if err != nil {
		return temporal.ZonedDateTime{}, err
	}
	dis, off, ov, err := r.temporalZonedOptions(opts, method, present)
	if err != nil {
		return temporal.ZonedDateTime{}, err
	}
	p, err := r.temporalRegulateZoned(rec, ov)
	if err != nil {
		return temporal.ZonedDateTime{}, err
	}
	z, err := d.Zones.ZonedDateTimeFromFields(p, rec.timeZone, cal, ov, dis, off)
	return z, r.temporalErr(err)
}

// temporalTimeOrMidnight is ToTimeRecordOrMidnight: nil for undefined.
func (r *Runtime) temporalTimeOrMidnight(v Value, method string) (*temporal.PlainTime, error) {
	if v.IsUndefined() {
		return nil, nil
	}
	t, err := r.toTemporalPlainTime(v, Undefined, method)
	if err != nil {
		return nil, err
	}
	return &t, nil
}

// ==== Durations ====

func (r *Runtime) toTemporalDuration(v Value) (temporal.Duration, error) {
	if x, ok := temporalValueOf(v).(temporal.Duration); ok {
		return x, nil
	}
	if v.IsString() {
		b, err := r.temporalBytes(v.String())
		if err != nil {
			return temporal.Duration{}, err
		}
		d, err := temporal.ParseDuration(b)
		return d, r.temporalErr(err)
	}
	if !v.IsObject() {
		return temporal.Duration{}, r.temporalType("Duration argument must be Duration or string.")
	}
	p, err := r.temporalPartialDuration(v)
	if err != nil {
		return temporal.Duration{}, err
	}
	d, err := temporal.DurationFromPartial(p)
	return d, r.temporalErr(err)
}

// temporalPartialDuration is ToTemporalPartialDurationRecord: the fields
// read in the order of their names.
func (r *Runtime) temporalPartialDuration(v Value) (temporal.PartialDuration, error) {
	var p temporal.PartialDuration
	if !v.IsObject() {
		return p, r.temporalType("Must provide a duration.")
	}
	o := v.Object()
	intField := func(key string, out **int64) error {
		x, err := r.temporalGet(o, key)
		if err != nil || x.IsUndefined() {
			return err
		}
		f, err := r.temporalIntegral(x)
		if err != nil {
			return err
		}
		if !(f >= -0x1p63 && f < 0x1p63) {
			return r.temporalRange("Duration field out of range.")
		}
		i := int64(f)
		*out = &i
		return nil
	}
	floatField := func(key string, out **float64) error {
		x, err := r.temporalGet(o, key)
		if err != nil || x.IsUndefined() {
			return err
		}
		f, err := r.temporalIntegral(x)
		if err != nil {
			return err
		}
		*out = &f
		return nil
	}
	for _, read := range []func() error{
		func() error { return intField("days", &p.Days) },
		func() error { return intField("hours", &p.Hours) },
		func() error { return floatField("microseconds", &p.Microseconds) },
		func() error { return intField("milliseconds", &p.Milliseconds) },
		func() error { return intField("minutes", &p.Minutes) },
		func() error { return intField("months", &p.Months) },
		func() error { return floatField("nanoseconds", &p.Nanoseconds) },
		func() error { return intField("seconds", &p.Seconds) },
		func() error { return intField("weeks", &p.Weeks) },
		func() error { return intField("years", &p.Years) },
	} {
		if err := read(); err != nil {
			return p, err
		}
	}
	if p == (temporal.PartialDuration{}) {
		return p, r.temporalType("Did not provide any valid Duration fields.")
	}
	return p, nil
}

// temporalRelativeTo is GetTemporalRelativeToOption over an options object.
func (r *Runtime) temporalRelativeTo(o *Object) (temporal.RelativeTo, error) {
	v, err := r.temporalGet(o, "relativeTo")
	if err != nil || v.IsUndefined() {
		return temporal.RelativeTo{}, err
	}
	switch x := temporalValueOf(v).(type) {
	case temporal.ZonedDateTime:
		return temporal.RelativeTo{Zoned: &x}, nil
	case temporal.PlainDate:
		return temporal.RelativeTo{Date: &x}, nil
	case temporal.PlainDateTime:
		d, err := r.temporalPlainDateOf(x.ToPlainDate())
		if err != nil {
			return temporal.RelativeTo{}, err
		}
		return temporal.RelativeTo{Date: &d}, nil
	}
	data, err := r.temporalData()
	if err != nil {
		return temporal.RelativeTo{}, err
	}
	if v.IsString() {
		b, err := r.temporalBytes(v.String())
		if err != nil {
			return temporal.RelativeTo{}, err
		}
		rel, err := data.ParseRelativeTo(b)
		return rel, r.temporalErr(err)
	}
	if !v.IsObject() {
		return temporal.RelativeTo{}, r.temporalType("relativeTo must be object or string.")
	}
	cal, err := r.temporalCalendarWithISODefault(v)
	if err != nil {
		return temporal.RelativeTo{}, err
	}
	rec, err := r.temporalPrepareFields(cal, v.Object(),
		temporalFieldsDate|temporalFieldsTime|temporalFieldsOffset|temporalFieldsTimeZone, temporalRequireNone)
	if err != nil {
		return temporal.RelativeTo{}, err
	}
	p, err := r.temporalRegulateZoned(rec, temporal.Constrain)
	if err != nil {
		return temporal.RelativeTo{}, err
	}
	if rec.timeZone == nil {
		d, err := temporal.PlainDateFromFields(p.Date, cal, temporal.Constrain)
		if err != nil {
			return temporal.RelativeTo{}, r.temporalErr(err)
		}
		return temporal.RelativeTo{Date: &d}, nil
	}
	z, err := data.Zones.ZonedDateTimeFromFields(p, rec.timeZone, cal, temporal.Constrain, temporal.Compatible,
		temporal.OffsetReject)
	if err != nil {
		return temporal.RelativeTo{}, r.temporalErr(err)
	}
	return temporal.RelativeTo{Zoned: &z}, nil
}

// temporalBigIntNanoseconds is GetI128FromBigInt, and its range check, as
// an Instant.
func (r *Runtime) temporalBigIntNanoseconds(b *BigInt) (temporal.Instant, error) {
	v := &b.V
	if v.BitLen() > 127 {
		return temporal.Instant{}, r.temporalRange("Nanoseconds out of range.")
	}
	hi, lo := temporalWords(v)
	i, err := temporal.NewInstant(hi, lo)
	if err != nil {
		return temporal.Instant{}, r.temporalRange("Nanoseconds out of range.")
	}
	return i, nil
}

// temporalEpochMilliseconds is Instant.fromEpochMilliseconds' reading of
// its argument.
func (r *Runtime) temporalEpochMilliseconds(v Value) (temporal.Instant, error) {
	ms, err := r.toNumber(v)
	if err != nil {
		return temporal.Instant{}, err
	}
	if math.IsNaN(ms) || math.IsInf(ms, 0) || !(ms >= -0x1p63 && ms < 0x1p63) || math.RoundToEven(ms) != ms {
		return temporal.Instant{}, r.temporalRange("Expected finite integer.")
	}
	i, err := temporal.InstantFromEpochMilliseconds(int64(ms))
	return i, r.temporalErr(err)
}

// temporalWords are a value's two's complement words, the high one
// signed, for a value within 127 bits.
func temporalWords(v *big.Int) (int64, uint64) {
	m := new(big.Int).And(v, new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 128), big.NewInt(1)))
	lo := new(big.Int).And(m, new(big.Int).SetUint64(math.MaxUint64)).Uint64()
	hi := new(big.Int).Rsh(m, 64).Uint64()
	return int64(hi), lo
}

// temporalNanosecondsBigInt is an Instant's nanoseconds as a BigInt.
func temporalNanosecondsBigInt(i temporal.Instant) Value {
	hi, lo := i.EpochNanoseconds()
	b := &BigInt{}
	b.V.Lsh(big.NewInt(hi), 64)
	b.V.Add(&b.V, new(big.Int).SetUint64(lo))
	return Big(b)
}
