package vm

import (
	intl "github.com/go-quickjs/go-intl"
	"github.com/go-quickjs/go-intl/temporal"
)

// Temporal.Instant, ZonedDateTime and Duration, as V8 binds them.

// ==== Instant ====

func (r *Runtime) initTemporalInstant(ns *Object) {
	r.temporalInstantProto = newObject(r.proto.object, ClassObject)
	k := temporalKind[temporal.Instant]{r, r.temporalInstantProto, "Instant"}
	value := func(rt *Runtime) func(temporal.Instant) Value {
		return func(i temporal.Instant) Value { return temporalObject(rt.temporalInstantProto, i) }
	}
	fromBigInt := func(rt *Runtime, v Value) (temporal.Instant, error) {
		b, err := rt.toBigIntOperand(v)
		if err != nil {
			return temporal.Instant{}, err
		}
		return rt.temporalBigIntNanoseconds(b)
	}
	ctor := k.ctor(ns, 1, func(rt *Runtime, args []Value) (temporal.Instant, error) {
		return fromBigInt(rt, arg(args, 0))
	})
	r.defMethod(ctor, "from", 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
		i, err := rt.toTemporalInstant(arg(args, 0))
		if err != nil {
			return Undefined, err
		}
		return value(rt)(i), nil
	})
	r.defMethod(ctor, "fromEpochNanoseconds", 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
		i, err := fromBigInt(rt, arg(args, 0))
		if err != nil {
			return Undefined, err
		}
		return value(rt)(i), nil
	})
	r.defMethod(ctor, "fromEpochMilliseconds", 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
		i, err := rt.temporalEpochMilliseconds(arg(args, 0))
		if err != nil {
			return Undefined, err
		}
		return value(rt)(i), nil
	})
	r.defMethod(ctor, "compare", 2, func(rt *Runtime, this Value, args []Value) (Value, error) {
		a, err := rt.toTemporalInstant(arg(args, 0))
		if err != nil {
			return Undefined, err
		}
		b, err := rt.toTemporalInstant(arg(args, 1))
		if err != nil {
			return Undefined, err
		}
		return Int(a.Compare(b)), nil
	})

	k.getter("epochMilliseconds", func(rt *Runtime, i temporal.Instant) (Value, error) {
		return Float(float64(i.EpochMilliseconds())), nil
	})
	k.getter("epochNanoseconds", func(rt *Runtime, i temporal.Instant) (Value, error) {
		return temporalNanosecondsBigInt(i), nil
	})
	k.method("equals", 1, func(rt *Runtime, i temporal.Instant, args []Value) (Value, error) {
		o, err := rt.toTemporalInstant(arg(args, 0))
		if err != nil {
			return Undefined, err
		}
		return Bool(i.Equals(o)), nil
	})
	k.method("round", 1, func(rt *Runtime, i temporal.Instant, args []Value) (Value, error) {
		const method = "Temporal.Instant.prototype.round"
		o, err := rt.temporalRoundTo(arg(args, 0), true)
		if err != nil {
			return Undefined, err
		}
		opts, err := rt.temporalRoundingOptions(o, method, temporal.NoUnit)
		if err != nil {
			return Undefined, err
		}
		out, err := i.Round(opts)
		if err != nil {
			return Undefined, rt.temporalErr(err)
		}
		return value(rt)(out), nil
	})
	k.method("toZonedDateTimeISO", 1, func(rt *Runtime, i temporal.Instant, args []Value) (Value, error) {
		tz, err := rt.temporalToTimeZone(arg(args, 0))
		if err != nil {
			return Undefined, err
		}
		z, err := i.ToZonedDateTimeISO(tz)
		if err != nil {
			return Undefined, rt.temporalErr(err)
		}
		return temporalObject(rt.temporalZonedDateTimeProto, z), nil
	})
	k.method("toString", 0, func(rt *Runtime, i temporal.Instant, args []Value) (Value, error) {
		const method = "Temporal.Instant.prototype.toString"
		o, err := rt.temporalOptionsObject(arg(args, 0))
		if err != nil {
			return Undefined, err
		}
		opts, err := rt.temporalToStringOptions(o, method, nil)
		if err != nil {
			return Undefined, err
		}
		tzLike, err := rt.temporalGet(o, "timeZone")
		if err != nil {
			return Undefined, err
		}
		if err := rt.temporalValidateUnit(opts.SmallestUnit, temporalGroupTime, temporal.NoUnit); err != nil {
			return Undefined, err
		}
		if opts.SmallestUnit == temporal.Hour {
			return Undefined, rt.intlPropertyOutOfRange("smallestUnit")
		}
		var tz *temporal.TimeZone
		if !tzLike.IsUndefined() {
			z, err := rt.temporalToTimeZone(tzLike)
			if err != nil {
				return Undefined, err
			}
			tz = &z
		}
		d, err := rt.temporalData()
		if err != nil {
			return Undefined, err
		}
		s, err := i.String(d.Zones, tz, opts)
		if err != nil {
			return Undefined, rt.temporalErr(err)
		}
		return Str(NewString(s)), nil
	})
	for _, sub := range []bool{false, true} {
		name := "add"
		if sub {
			name = "subtract"
		}
		k.method(name, 1, func(rt *Runtime, i temporal.Instant, args []Value) (Value, error) {
			dur, err := rt.toTemporalDuration(arg(args, 0))
			if err != nil {
				return Undefined, err
			}
			var out temporal.Instant
			if sub {
				out, err = i.Subtract(dur)
			} else {
				out, err = i.Add(dur)
			}
			if err != nil {
				return Undefined, rt.temporalErr(err)
			}
			return value(rt)(out), nil
		})
	}
	temporalDifference(k, prototypeMethod("Instant"),
		func(rt *Runtime, v Value, method string) (temporal.Instant, error) { return rt.toTemporalInstant(v) }, nil,
		func(a, b temporal.Instant, s temporal.DifferenceSettings, since bool) (temporal.Duration, error) {
			if since {
				return a.Since(b, s)
			}
			return a.Until(b, s)
		})
	k.toJSON(func(rt *Runtime, i temporal.Instant) (string, error) {
		d, err := rt.temporalData()
		if err != nil {
			return "", err
		}
		s, err := i.String(d.Zones, nil, temporal.DefaultToStringOptions)
		return s, rt.temporalErr(err)
	})
	k.toLocaleString(intl.ComponentsAny, intl.ComponentsAll)
	k.valueOf()
}

// ==== ZonedDateTime ====

func (r *Runtime) initTemporalZonedDateTime(ns *Object) {
	r.temporalZonedDateTimeProto = newObject(r.proto.object, ClassObject)
	k := temporalKind[temporal.ZonedDateTime]{r, r.temporalZonedDateTimeProto, "ZonedDateTime"}
	value := func(rt *Runtime) func(temporal.ZonedDateTime) Value {
		return func(z temporal.ZonedDateTime) Value { return temporalObject(rt.temporalZonedDateTimeProto, z) }
	}
	ctor := k.ctor(ns, 2, func(rt *Runtime, args []Value) (temporal.ZonedDateTime, error) {
		b, err := rt.toBigIntOperand(arg(args, 0))
		if err != nil {
			return temporal.ZonedDateTime{}, err
		}
		i, err := rt.temporalBigIntNanoseconds(b)
		if err != nil {
			return temporal.ZonedDateTime{}, err
		}
		if !arg(args, 1).IsString() {
			return temporal.ZonedDateTime{}, rt.temporalType("Time zone must be string")
		}
		d, err := rt.temporalData()
		if err != nil {
			return temporal.ZonedDateTime{}, err
		}
		tz, err := d.Zones.TimeZoneFromIdentifier([]byte(temporalStdString(arg(args, 1).String())))
		if err != nil {
			return temporal.ZonedDateTime{}, rt.temporalErr(err)
		}
		cal, err := rt.temporalCalendarArg(arg(args, 2))
		if err != nil {
			return temporal.ZonedDateTime{}, err
		}
		z, err := temporal.NewZonedDateTimeFromInstant(i, tz, cal)
		return z, rt.temporalErr(err)
	})
	r.defMethod(ctor, "from", 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
		z, err := rt.toTemporalZonedDateTime(arg(args, 0), arg(args, 1), "Temporal.ZonedDateTime.from", true)
		if err != nil {
			return Undefined, err
		}
		return value(rt)(z), nil
	})
	r.defMethod(ctor, "compare", 2, func(rt *Runtime, this Value, args []Value) (Value, error) {
		const method = "Temporal.ZonedDateTime.compare"
		a, err := rt.toTemporalZonedDateTime(arg(args, 0), Undefined, method, false)
		if err != nil {
			return Undefined, err
		}
		b, err := rt.toTemporalZonedDateTime(arg(args, 1), Undefined, method, false)
		if err != nil {
			return Undefined, err
		}
		return Int(a.Compare(b)), nil
	})

	temporalDateGetters(k, func(z temporal.ZonedDateTime) temporal.PlainDate { return z.ToPlainDate() })
	temporalTimeGetters(k, func(z temporal.ZonedDateTime) temporal.ISOTime { return z.ToPlainTime().ISO() })
	k.getter("epochMilliseconds", func(rt *Runtime, z temporal.ZonedDateTime) (Value, error) {
		return Float(float64(z.Instant().EpochMilliseconds())), nil
	})
	k.getter("epochNanoseconds", func(rt *Runtime, z temporal.ZonedDateTime) (Value, error) {
		return temporalNanosecondsBigInt(z.Instant()), nil
	})
	k.getter("offset", func(rt *Runtime, z temporal.ZonedDateTime) (Value, error) {
		return Str(NewString(z.Offset())), nil
	})
	k.getter("offsetNanoseconds", func(rt *Runtime, z temporal.ZonedDateTime) (Value, error) {
		return Float(float64(z.OffsetNanoseconds())), nil
	})
	k.getter("timeZoneId", func(rt *Runtime, z temporal.ZonedDateTime) (Value, error) {
		return Str(NewString(z.TimeZone().Identifier())), nil
	})
	k.getter("hoursInDay", func(rt *Runtime, z temporal.ZonedDateTime) (Value, error) {
		h, err := z.HoursInDay()
		if err != nil {
			return Undefined, rt.temporalErr(err)
		}
		return Float(h), nil
	})
	k.method("equals", 1, func(rt *Runtime, z temporal.ZonedDateTime, args []Value) (Value, error) {
		o, err := rt.toTemporalZonedDateTime(arg(args, 0), Undefined, "Temporal.ZonedDateTime.prototype.equals", false)
		if err != nil {
			return Undefined, err
		}
		return Bool(z.Equals(o)), nil
	})
	k.method("with", 1, func(rt *Runtime, z temporal.ZonedDateTime, args []Value) (Value, error) {
		const method = "Temporal.ZonedDateTime.prototype.with"
		if err := rt.temporalRequirePartial(arg(args, 0)); err != nil {
			return Undefined, err
		}
		rec, err := rt.temporalPrepareFields(z.Calendar(), arg(args, 0).Object(),
			temporalFieldsDate|temporalFieldsTime|temporalFieldsOffset, temporalRequirePartial)
		if err != nil {
			return Undefined, err
		}
		d, err := rt.temporalDisambiguation(arg(args, 1), method)
		if err != nil {
			return Undefined, err
		}
		o, err := rt.temporalOffsetOption(arg(args, 1), method, temporal.OffsetPrefer)
		if err != nil {
			return Undefined, err
		}
		ov, err := rt.temporalOverflow(arg(args, 1), method)
		if err != nil {
			return Undefined, err
		}
		p, err := rt.temporalRegulateZoned(rec, ov)
		if err != nil {
			return Undefined, err
		}
		out, err := z.With(p, d, o, ov)
		if err != nil {
			return Undefined, rt.temporalErr(err)
		}
		return value(rt)(out), nil
	})
	k.method("withCalendar", 1, func(rt *Runtime, z temporal.ZonedDateTime, args []Value) (Value, error) {
		c, err := rt.temporalToCalendar(arg(args, 0))
		if err != nil {
			return Undefined, err
		}
		return value(rt)(z.WithCalendar(c)), nil
	})
	k.method("withPlainTime", 0, func(rt *Runtime, z temporal.ZonedDateTime, args []Value) (Value, error) {
		t, err := rt.temporalTimeOrMidnight(arg(args, 0), "Temporal.ZonedDateTime.prototype.withPlainTime")
		if err != nil {
			return Undefined, err
		}
		out, err := z.WithPlainTime(t)
		if err != nil {
			return Undefined, rt.temporalErr(err)
		}
		return value(rt)(out), nil
	})
	k.method("withTimeZone", 1, func(rt *Runtime, z temporal.ZonedDateTime, args []Value) (Value, error) {
		tz, err := rt.temporalToTimeZone(arg(args, 0))
		if err != nil {
			return Undefined, err
		}
		out, err := z.WithTimeZone(tz)
		if err != nil {
			return Undefined, rt.temporalErr(err)
		}
		return value(rt)(out), nil
	})
	k.method("toString", 0, func(rt *Runtime, z temporal.ZonedDateTime, args []Value) (Value, error) {
		const method = "Temporal.ZonedDateTime.prototype.toString"
		o, err := rt.temporalOptionsObject(arg(args, 0))
		if err != nil {
			return Undefined, err
		}
		showCal, err := rt.temporalShowCalendar(o, method)
		if err != nil {
			return Undefined, err
		}
		var showOffset temporal.DisplayOffset
		opts, err := rt.temporalToStringOptions(o, method, func() error {
			i, err := rt.temporalStringOption(o, "offset", method, []string{"auto", "never"}, 0)
			showOffset = []temporal.DisplayOffset{temporal.OffsetAuto, temporal.OffsetNever}[i]
			return err
		})
		if err != nil {
			return Undefined, err
		}
		i, err := rt.temporalStringOption(o, "timeZoneName", method, []string{"auto", "never", "critical"}, 0)
		if err != nil {
			return Undefined, err
		}
		showZone := []temporal.DisplayTimeZone{temporal.TimeZoneAuto, temporal.TimeZoneNever, temporal.TimeZoneCritical}[i]
		if err := rt.temporalValidateUnit(opts.SmallestUnit, temporalGroupTime, temporal.NoUnit); err != nil {
			return Undefined, err
		}
		if opts.SmallestUnit == temporal.Hour {
			return Undefined, rt.temporalRange("smallestUnit cannot be Hour.")
		}
		s, err := z.String(showOffset, showZone, showCal, opts)
		if err != nil {
			return Undefined, rt.temporalErr(err)
		}
		return Str(NewString(s)), nil
	})
	k.method("round", 1, func(rt *Runtime, z temporal.ZonedDateTime, args []Value) (Value, error) {
		// V8 names ZonedDateTime's round as PlainDateTime's.
		const method = "Temporal.PlainDateTime.prototype.round"
		o, err := rt.temporalRoundTo(arg(args, 0), false)
		if err != nil {
			return Undefined, err
		}
		opts, err := rt.temporalRoundingOptions(o, method, temporal.Day)
		if err != nil {
			return Undefined, err
		}
		out, err := z.Round(opts)
		if err != nil {
			return Undefined, rt.temporalErr(err)
		}
		return value(rt)(out), nil
	})
	temporalAddOrSubtract(k, value(r), func(z temporal.ZonedDateTime, dur temporal.Duration, ov temporal.Overflow, sub bool) (temporal.ZonedDateTime, error) {
		if sub {
			return z.Subtract(dur, ov)
		}
		return z.Add(dur, ov)
	})
	// V8 names ZonedDateTime's until as its since.
	temporalDifference(k, func(string) string { return "Temporal.ZonedDateTime.prototype.since" },
		func(rt *Runtime, v Value, method string) (temporal.ZonedDateTime, error) {
			return rt.toTemporalZonedDateTime(v, Undefined, method, false)
		},
		func(rt *Runtime, a, b temporal.ZonedDateTime) error {
			return rt.temporalSameCalendar(a.Calendar(), b.Calendar())
		},
		func(a, b temporal.ZonedDateTime, s temporal.DifferenceSettings, since bool) (temporal.Duration, error) {
			if since {
				return a.Since(b, s)
			}
			return a.Until(b, s)
		})
	k.method("startOfDay", 0, func(rt *Runtime, z temporal.ZonedDateTime, args []Value) (Value, error) {
		out, err := z.StartOfDay()
		if err != nil {
			return Undefined, rt.temporalErr(err)
		}
		return value(rt)(out), nil
	})
	k.method("getTimeZoneTransition", 1, func(rt *Runtime, z temporal.ZonedDateTime, args []Value) (Value, error) {
		const method = "Temporal.ZonedDateTime.prototype.getTimeZoneTransition"
		v := arg(args, 0)
		var o *Object
		switch {
		case v.IsUndefined():
			return Undefined, rt.temporalType("Must specify a direction parameter.")
		case v.IsString():
			o = newObject(nil, ClassObject)
			o.setOwnRaw(rt.atoms.intern("direction"), v, propDefault)
		case v.IsObject():
			o = v.Object()
		default:
			return Undefined, rt.temporalType("directionParam must be object or string.")
		}
		i, err := rt.temporalStringOption(o, "direction", method, []string{"next", "previous"}, -1)
		if err != nil {
			return Undefined, err
		}
		out, ok, err := z.TimeZoneTransition(i == 0)
		if err != nil {
			return Undefined, rt.temporalErr(err)
		}
		if !ok {
			return Null, nil
		}
		return value(rt)(out), nil
	})
	k.method("toInstant", 0, func(rt *Runtime, z temporal.ZonedDateTime, args []Value) (Value, error) {
		return temporalObject(rt.temporalInstantProto, z.Instant()), nil
	})
	k.method("toPlainDate", 0, func(rt *Runtime, z temporal.ZonedDateTime, args []Value) (Value, error) {
		return temporalObject(rt.temporalPlainDateProto, z.ToPlainDate()), nil
	})
	k.method("toPlainTime", 0, func(rt *Runtime, z temporal.ZonedDateTime, args []Value) (Value, error) {
		return temporalObject(rt.temporalPlainTimeProto, z.ToPlainTime()), nil
	})
	k.method("toPlainDateTime", 0, func(rt *Runtime, z temporal.ZonedDateTime, args []Value) (Value, error) {
		return temporalObject(rt.temporalPlainDateTimeProto, z.ToPlainDateTime()), nil
	})
	k.toJSON(func(rt *Runtime, z temporal.ZonedDateTime) (string, error) {
		s, err := z.String(temporal.OffsetAuto, temporal.TimeZoneAuto, temporal.CalendarAuto, temporal.DefaultToStringOptions)
		return s, rt.temporalErr(err)
	})
	k.method("toLocaleString", 0, func(rt *Runtime, z temporal.ZonedDateTime, args []Value) (Value, error) {
		return rt.zonedToLocaleString(z, args)
	})
	k.valueOf()
}

// ==== Duration ====

// temporalDurationFields are a duration's ten fields, as DurationFormat
// takes them.
func temporalDurationFields(d temporal.Duration) intl.Duration {
	return intl.Duration{float64(d.Years()), float64(d.Months()), float64(d.Weeks()), float64(d.Days()),
		float64(d.Hours()), float64(d.Minutes()), float64(d.Seconds()), float64(d.Milliseconds()),
		d.Microseconds(), d.Nanoseconds()}
}

func (r *Runtime) initTemporalDuration(ns *Object) {
	r.temporalDurationProto = newObject(r.proto.object, ClassObject)
	k := temporalKind[temporal.Duration]{r, r.temporalDurationProto, "Duration"}
	value := func(rt *Runtime) func(temporal.Duration) Value {
		return func(d temporal.Duration) Value { return temporalObject(rt.temporalDurationProto, d) }
	}
	ctor := k.ctor(ns, 0, func(rt *Runtime, args []Value) (temporal.Duration, error) {
		var f [10]float64
		for i := 0; i < 10; i++ {
			if arg(args, i).IsUndefined() {
				continue
			}
			n, err := rt.temporalIntegral(arg(args, i))
			if err != nil {
				return temporal.Duration{}, err
			}
			if i < 8 && !(n >= -0x1p63 && n < 0x1p63) {
				return temporal.Duration{}, rt.temporalRange("Integer out of range.")
			}
			f[i] = n
		}
		d, err := temporal.DurationFromNumbers(f[0], f[1], f[2], f[3], f[4], f[5], f[6], f[7], f[8], f[9])
		return d, rt.temporalErr(err)
	})
	r.defMethod(ctor, "from", 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
		d, err := rt.toTemporalDuration(arg(args, 0))
		if err != nil {
			return Undefined, err
		}
		return value(rt)(d), nil
	})
	r.defMethod(ctor, "compare", 2, func(rt *Runtime, this Value, args []Value) (Value, error) {
		one, err := rt.toTemporalDuration(arg(args, 0))
		if err != nil {
			return Undefined, err
		}
		two, err := rt.toTemporalDuration(arg(args, 1))
		if err != nil {
			return Undefined, err
		}
		var rel temporal.RelativeTo
		if opts := arg(args, 2); !opts.IsUndefined() {
			if !opts.IsObject() {
				return Undefined, rt.temporalOptionType("relativeTo")
			}
			if rel, err = rt.temporalRelativeTo(opts.Object()); err != nil {
				return Undefined, err
			}
		}
		c, err := one.Compare(two, rel)
		if err != nil {
			return Undefined, rt.temporalErr(err)
		}
		return Int(c), nil
	})

	for _, g := range []struct {
		name string
		get  func(temporal.Duration) Value
	}{
		{"years", func(d temporal.Duration) Value { return Float(float64(d.Years())) }},
		{"months", func(d temporal.Duration) Value { return Float(float64(d.Months())) }},
		{"weeks", func(d temporal.Duration) Value { return Float(float64(d.Weeks())) }},
		{"days", func(d temporal.Duration) Value { return Float(float64(d.Days())) }},
		{"hours", func(d temporal.Duration) Value { return Float(float64(d.Hours())) }},
		{"minutes", func(d temporal.Duration) Value { return Float(float64(d.Minutes())) }},
		{"seconds", func(d temporal.Duration) Value { return Float(float64(d.Seconds())) }},
		{"milliseconds", func(d temporal.Duration) Value { return Float(float64(d.Milliseconds())) }},
		{"microseconds", func(d temporal.Duration) Value { return Float(d.Microseconds()) }},
		{"nanoseconds", func(d temporal.Duration) Value { return Float(d.Nanoseconds()) }},
		{"sign", func(d temporal.Duration) Value { return Int(d.Sign()) }},
		{"blank", func(d temporal.Duration) Value { return Bool(d.IsZero()) }},
	} {
		get := g.get
		k.getter(g.name, func(rt *Runtime, d temporal.Duration) (Value, error) { return get(d), nil })
	}
	k.method("negated", 0, func(rt *Runtime, d temporal.Duration, args []Value) (Value, error) {
		return value(rt)(d.Negated()), nil
	})
	k.method("abs", 0, func(rt *Runtime, d temporal.Duration, args []Value) (Value, error) {
		return value(rt)(d.Abs()), nil
	})
	for _, sub := range []bool{false, true} {
		name := "add"
		if sub {
			name = "subtract"
		}
		k.method(name, 1, func(rt *Runtime, d temporal.Duration, args []Value) (Value, error) {
			o, err := rt.toTemporalDuration(arg(args, 0))
			if err != nil {
				return Undefined, err
			}
			var out temporal.Duration
			if sub {
				out, err = d.Subtract(o)
			} else {
				out, err = d.Add(o)
			}
			if err != nil {
				return Undefined, rt.temporalErr(err)
			}
			return value(rt)(out), nil
		})
	}
	k.method("with", 1, func(rt *Runtime, d temporal.Duration, args []Value) (Value, error) {
		p, err := rt.temporalPartialDuration(arg(args, 0))
		if err != nil {
			return Undefined, err
		}
		set := func(p **int64, v int64) {
			if *p == nil {
				*p = &v
			}
		}
		setF := func(p **float64, v float64) {
			if *p == nil {
				*p = &v
			}
		}
		set(&p.Years, d.Years())
		set(&p.Months, d.Months())
		set(&p.Weeks, d.Weeks())
		set(&p.Days, d.Days())
		set(&p.Hours, d.Hours())
		set(&p.Minutes, d.Minutes())
		set(&p.Seconds, d.Seconds())
		set(&p.Milliseconds, d.Milliseconds())
		setF(&p.Microseconds, d.Microseconds())
		setF(&p.Nanoseconds, d.Nanoseconds())
		out, err := temporal.DurationFromPartial(p)
		if err != nil {
			return Undefined, rt.temporalErr(err)
		}
		return value(rt)(out), nil
	})
	k.method("round", 1, func(rt *Runtime, d temporal.Duration, args []Value) (Value, error) {
		const method = "Temporal.Duration.prototype.round"
		v := arg(args, 0)
		var o *Object
		switch {
		case v.IsUndefined():
			return Undefined, rt.temporalType("Must specify a roundTo parameter.")
		case v.IsString():
			o = newObject(nil, ClassObject)
			o.setOwnRaw(rt.atoms.intern("smallestUnit"), v, propDefault)
		case v.IsObject():
			o = v.Object()
		default:
			return Undefined, rt.temporalType("roundTo must be an object.")
		}
		largest, err := rt.temporalUnitOption(o, "largestUnit", method, false)
		if err != nil {
			return Undefined, err
		}
		rel, err := rt.temporalRelativeTo(o)
		if err != nil {
			return Undefined, err
		}
		inc, err := rt.temporalRoundingIncrement(o)
		if err != nil {
			return Undefined, err
		}
		mode, err := rt.temporalRoundingMode(o, method, temporal.HalfExpand)
		if err != nil {
			return Undefined, err
		}
		smallest, err := rt.temporalUnitOption(o, "smallestUnit", method, false)
		if err != nil {
			return Undefined, err
		}
		if err := rt.temporalValidateUnit(smallest, temporalGroupDateTime, temporal.NoUnit); err != nil {
			return Undefined, err
		}
		out, err := d.Round(temporal.RoundingOptions{LargestUnit: largest, SmallestUnit: smallest, RoundingMode: mode,
			Increment: inc, Compat: rt.intlCompat()}, rel)
		if err != nil {
			return Undefined, rt.temporalErr(err)
		}
		return value(rt)(out), nil
	})
	k.method("total", 1, func(rt *Runtime, d temporal.Duration, args []Value) (Value, error) {
		const method = "Temporal.Duration.prototype.total"
		v := arg(args, 0)
		var o *Object
		switch {
		case v.IsUndefined():
			return Undefined, rt.temporalType("Must specify a totalOf parameter")
		case v.IsString():
			o = newObject(nil, ClassObject)
			o.setOwnRaw(rt.atoms.intern("unit"), v, propDefault)
		case v.IsObject():
			o = v.Object()
		default:
			return Undefined, rt.temporalType("totalOf must be an object.")
		}
		rel, err := rt.temporalRelativeTo(o)
		if err != nil {
			return Undefined, err
		}
		u, err := rt.temporalUnitOption(o, "unit", method, true)
		if err != nil {
			return Undefined, err
		}
		if err := rt.temporalValidateUnit(u, temporalGroupDateTime, temporal.NoUnit); err != nil {
			return Undefined, err
		}
		f, err := d.Total(u, rel)
		if err != nil {
			return Undefined, rt.temporalErr(err)
		}
		return Float(f), nil
	})
	k.method("toString", 0, func(rt *Runtime, d temporal.Duration, args []Value) (Value, error) {
		const method = "Temporal.Duration.prototype.toString"
		o, err := rt.temporalOptionsObject(arg(args, 0))
		if err != nil {
			return Undefined, err
		}
		digits, err := rt.temporalFractionalSecondDigits(o)
		if err != nil {
			return Undefined, err
		}
		mode, err := rt.temporalRoundingMode(o, method, temporal.Trunc)
		if err != nil {
			return Undefined, err
		}
		smallest, err := rt.temporalUnitOption(o, "smallestUnit", method, false)
		if err != nil {
			return Undefined, err
		}
		if err := rt.temporalValidateUnit(smallest, temporalGroupTime, temporal.NoUnit); err != nil {
			return Undefined, err
		}
		s, err := d.String(temporal.ToStringRoundingOptions{Precision: digits, SmallestUnit: smallest, RoundingMode: mode})
		if err != nil {
			return Undefined, rt.temporalErr(err)
		}
		return Str(NewString(s)), nil
	})
	k.toJSON(func(rt *Runtime, d temporal.Duration) (string, error) {
		s, err := d.String(temporal.DefaultToStringOptions)
		return s, rt.temporalErr(err)
	})
	k.method("toLocaleString", 0, func(rt *Runtime, d temporal.Duration, args []Value) (Value, error) {
		options, err := rt.durationOptionsFrom(args)
		if err != nil {
			return Undefined, err
		}
		text, err := options.format.Format(temporalDurationFields(d))
		if err != nil {
			return Undefined, rt.intlTemporalRange("Duration was not valid.")
		}
		return Str(NewString(text)), nil
	})
	k.valueOf()
}
