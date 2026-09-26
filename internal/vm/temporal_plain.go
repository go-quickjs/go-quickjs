package vm

import (
	intl "github.com/go-quickjs/go-intl"
	"github.com/go-quickjs/go-intl/temporal"
)

// Temporal.PlainDate, PlainTime and PlainDateTime, as V8 binds them.

// temporalAddOrSubtract defines add and subtract over a type whose method
// takes a duration and an overflow.
func temporalAddOrSubtract[T any](k temporalKind[T], result func(T) Value,
	add func(v T, d temporal.Duration, ov temporal.Overflow, sub bool) (T, error)) {
	for _, sub := range []bool{false, true} {
		name := "add"
		if sub {
			name = "subtract"
		}
		method := "Temporal." + k.name + ".prototype." + name
		k.method(name, 1, func(rt *Runtime, v T, args []Value) (Value, error) {
			dur, err := rt.toTemporalDuration(arg(args, 0))
			if err != nil {
				return Undefined, err
			}
			ov, err := rt.temporalOverflow(arg(args, 1), method)
			if err != nil {
				return Undefined, err
			}
			out, err := add(v, dur, ov, sub)
			if err != nil {
				return Undefined, rt.temporalErr(err)
			}
			return result(out), nil
		})
	}
}

// temporalDifference defines until and since: the other value, a check of
// the two, and the difference settings.
func temporalDifference[T any](k temporalKind[T], methodName func(name string) string,
	other func(rt *Runtime, v Value, method string) (T, error), check func(rt *Runtime, a, b T) error,
	diff func(a, b T, s temporal.DifferenceSettings, since bool) (temporal.Duration, error)) {
	for _, since := range []bool{false, true} {
		name := "until"
		if since {
			name = "since"
		}
		method := methodName(name)
		k.method(name, 1, func(rt *Runtime, v T, args []Value) (Value, error) {
			o, err := other(rt, arg(args, 0), method)
			if err != nil {
				return Undefined, err
			}
			if check != nil {
				if err := check(rt, v, o); err != nil {
					return Undefined, err
				}
			}
			s, err := rt.temporalDifferenceSettings(arg(args, 1), method)
			if err != nil {
				return Undefined, err
			}
			d, err := diff(v, o, s, since)
			if err != nil {
				return Undefined, rt.temporalErr(err)
			}
			return temporalObject(rt.temporalDurationProto, d), nil
		})
	}
}

func prototypeMethod(typ string) func(string) string {
	return func(name string) string { return "Temporal." + typ + ".prototype." + name }
}

// ==== PlainDate ====

func (r *Runtime) initTemporalPlainDate(ns *Object) {
	r.temporalPlainDateProto = newObject(r.proto.object, ClassObject)
	k := temporalKind[temporal.PlainDate]{r, r.temporalPlainDateProto, "PlainDate"}
	value := func(rt *Runtime) func(temporal.PlainDate) Value {
		return func(d temporal.PlainDate) Value { return temporalObject(rt.temporalPlainDateProto, d) }
	}
	ctor := k.ctor(ns, 3, func(rt *Runtime, args []Value) (temporal.PlainDate, error) {
		var f [3]float64
		for i := range f {
			n, err := rt.temporalInteger(arg(args, i))
			if err != nil {
				return temporal.PlainDate{}, err
			}
			f[i] = n
		}
		cal, err := rt.temporalCalendarArg(arg(args, 3))
		if err != nil {
			return temporal.PlainDate{}, err
		}
		if !temporalValidISODate(f[0], f[1], f[2]) {
			return temporal.PlainDate{}, rt.temporalRange("Invalid ISO date.")
		}
		d, err := temporal.NewPlainDate(int(f[0]), int(f[1]), int(f[2]), cal, temporal.Reject)
		return d, rt.temporalErr(err)
	})
	r.defMethod(ctor, "from", 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
		d, err := rt.toTemporalPlainDate(arg(args, 0), arg(args, 1), "Temporal.PlainDate.from")
		if err != nil {
			return Undefined, err
		}
		return value(rt)(d), nil
	})
	r.defMethod(ctor, "compare", 2, func(rt *Runtime, this Value, args []Value) (Value, error) {
		const method = "Temporal.PlainDate.compare"
		a, err := rt.toTemporalPlainDate(arg(args, 0), Undefined, method)
		if err != nil {
			return Undefined, err
		}
		b, err := rt.toTemporalPlainDate(arg(args, 1), Undefined, method)
		if err != nil {
			return Undefined, err
		}
		return Int(a.Compare(b)), nil
	})

	temporalDateGetters(k, func(d temporal.PlainDate) temporal.PlainDate { return d })
	k.method("equals", 1, func(rt *Runtime, d temporal.PlainDate, args []Value) (Value, error) {
		o, err := rt.toTemporalPlainDate(arg(args, 0), Undefined, "Temporal.PlainDate.prototype.equals")
		if err != nil {
			return Undefined, err
		}
		return Bool(d.Equals(o)), nil
	})
	k.method("toPlainYearMonth", 0, func(rt *Runtime, d temporal.PlainDate, args []Value) (Value, error) {
		ym, err := d.ToPlainYearMonth()
		if err != nil {
			return Undefined, rt.temporalErr(err)
		}
		return temporalObject(rt.temporalPlainYearMonthProto, ym), nil
	})
	k.method("toPlainMonthDay", 0, func(rt *Runtime, d temporal.PlainDate, args []Value) (Value, error) {
		md, err := d.ToPlainMonthDay()
		if err != nil {
			return Undefined, rt.temporalErr(err)
		}
		return temporalObject(rt.temporalPlainMonthDayProto, md), nil
	})
	k.method("toPlainDateTime", 0, func(rt *Runtime, d temporal.PlainDate, args []Value) (Value, error) {
		t, err := rt.temporalTimeOrMidnight(arg(args, 0), "Temporal.PlainDate.toPlainDateTime")
		if err != nil {
			return Undefined, err
		}
		dt, err := d.ToPlainDateTime(t)
		if err != nil {
			return Undefined, rt.temporalErr(err)
		}
		return temporalObject(rt.temporalPlainDateTimeProto, dt), nil
	})
	k.method("with", 1, func(rt *Runtime, d temporal.PlainDate, args []Value) (Value, error) {
		const method = "Temporal.PlainDate.prototype.with"
		if err := rt.temporalRequirePartial(arg(args, 0)); err != nil {
			return Undefined, err
		}
		rec, err := rt.temporalPrepareFields(d.Calendar(), arg(args, 0).Object(), temporalFieldsDate, temporalRequirePartial)
		if err != nil {
			return Undefined, err
		}
		ov, err := rt.temporalOverflow(arg(args, 1), method)
		if err != nil {
			return Undefined, err
		}
		f, err := rt.temporalRegulateDate(rec, ov)
		if err != nil {
			return Undefined, err
		}
		out, err := d.With(f, ov)
		if err != nil {
			return Undefined, rt.temporalErr(err)
		}
		return value(rt)(out), nil
	})
	k.method("withCalendar", 1, func(rt *Runtime, d temporal.PlainDate, args []Value) (Value, error) {
		c, err := rt.temporalToCalendar(arg(args, 0))
		if err != nil {
			return Undefined, err
		}
		return value(rt)(d.WithCalendar(c)), nil
	})
	k.method("toZonedDateTime", 1, func(rt *Runtime, d temporal.PlainDate, args []Value) (Value, error) {
		const method = "Temporal.PlainDate.toZonedDateTime"
		item := arg(args, 0)
		var tz temporal.TimeZone
		timeLike := Undefined
		var err error
		if item.IsObject() {
			tzLike, err := rt.temporalGet(item.Object(), "timeZone")
			if err != nil {
				return Undefined, err
			}
			if tzLike.IsUndefined() {
				tz, err = rt.temporalToTimeZone(item)
			} else {
				if tz, err = rt.temporalToTimeZone(tzLike); err == nil {
					timeLike, err = rt.temporalGet(item.Object(), "plainTime")
				}
			}
			if err != nil {
				return Undefined, err
			}
		} else if tz, err = rt.temporalToTimeZone(item); err != nil {
			return Undefined, err
		}
		var t *temporal.PlainTime
		if !timeLike.IsUndefined() {
			pt, err := rt.toTemporalPlainTime(timeLike, Undefined, method)
			if err != nil {
				return Undefined, err
			}
			t = &pt
		}
		z, err := d.ToZonedDateTime(tz, t)
		if err != nil {
			return Undefined, rt.temporalErr(err)
		}
		return temporalObject(rt.temporalZonedDateTimeProto, z), nil
	})
	temporalAddOrSubtract(k, value(r), func(d temporal.PlainDate, dur temporal.Duration, ov temporal.Overflow, sub bool) (temporal.PlainDate, error) {
		if sub {
			return d.Subtract(dur, ov)
		}
		return d.Add(dur, ov)
	})
	temporalDifference(k, prototypeMethod("PlainDate"),
		func(rt *Runtime, v Value, method string) (temporal.PlainDate, error) {
			return rt.toTemporalPlainDate(v, Undefined, method)
		},
		func(rt *Runtime, a, b temporal.PlainDate) error {
			return rt.temporalSameCalendar(a.Calendar(), b.Calendar())
		},
		func(a, b temporal.PlainDate, s temporal.DifferenceSettings, since bool) (temporal.Duration, error) {
			if since {
				return a.Since(b, s)
			}
			return a.Until(b, s)
		})
	k.method("toString", 0, func(rt *Runtime, d temporal.PlainDate, args []Value) (Value, error) {
		o, err := rt.temporalOptionsObject(arg(args, 0))
		if err != nil {
			return Undefined, err
		}
		show, err := rt.temporalShowCalendar(o, "Temporal.PlainDate.prototype.toString")
		if err != nil {
			return Undefined, err
		}
		return Str(NewString(d.String(show))), nil
	})
	k.toJSON(func(rt *Runtime, d temporal.PlainDate) (string, error) { return d.String(temporal.CalendarAuto), nil })
	k.toLocaleString(intl.ComponentsDate, intl.ComponentsDate)
	k.valueOf()
}

// ==== PlainTime ====

func (r *Runtime) initTemporalPlainTime(ns *Object) {
	r.temporalPlainTimeProto = newObject(r.proto.object, ClassObject)
	k := temporalKind[temporal.PlainTime]{r, r.temporalPlainTimeProto, "PlainTime"}
	value := func(rt *Runtime) func(temporal.PlainTime) Value {
		return func(t temporal.PlainTime) Value { return temporalObject(rt.temporalPlainTimeProto, t) }
	}
	ctor := k.ctor(ns, 0, func(rt *Runtime, args []Value) (temporal.PlainTime, error) {
		t, err := rt.temporalTimeArgs(args, 0)
		if err != nil {
			return temporal.PlainTime{}, err
		}
		if !temporalValidTime(t) {
			return temporal.PlainTime{}, rt.temporalRange("Invalid time")
		}
		out, err := temporal.NewPlainTime(int(t[0]), int(t[1]), int(t[2]), int(t[3]), int(t[4]), int(t[5]), temporal.Reject)
		return out, rt.temporalErr(err)
	})
	r.defMethod(ctor, "from", 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
		t, err := rt.toTemporalPlainTime(arg(args, 0), arg(args, 1), "Temporal.PlainTime.from")
		if err != nil {
			return Undefined, err
		}
		return value(rt)(t), nil
	})
	r.defMethod(ctor, "compare", 2, func(rt *Runtime, this Value, args []Value) (Value, error) {
		const method = "Temporal.PlainTime.compare"
		a, err := rt.toTemporalPlainTime(arg(args, 0), Undefined, method)
		if err != nil {
			return Undefined, err
		}
		b, err := rt.toTemporalPlainTime(arg(args, 1), Undefined, method)
		if err != nil {
			return Undefined, err
		}
		return Int(a.Compare(b)), nil
	})

	temporalTimeGetters(k, func(t temporal.PlainTime) temporal.ISOTime { return t.ISO() })
	k.method("equals", 1, func(rt *Runtime, t temporal.PlainTime, args []Value) (Value, error) {
		o, err := rt.toTemporalPlainTime(arg(args, 0), Undefined, "Temporal.PlainTime.prototype.equals")
		if err != nil {
			return Undefined, err
		}
		return Bool(t.Compare(o) == 0), nil
	})
	k.method("round", 1, func(rt *Runtime, t temporal.PlainTime, args []Value) (Value, error) {
		// V8 names PlainTime's round as PlainDateTime's.
		const method = "Temporal.PlainDateTime.prototype.round"
		o, err := rt.temporalRoundTo(arg(args, 0), false)
		if err != nil {
			return Undefined, err
		}
		opts, err := rt.temporalRoundingOptions(o, method, temporal.NoUnit)
		if err != nil {
			return Undefined, err
		}
		out, err := t.Round(opts)
		if err != nil {
			return Undefined, rt.temporalErr(err)
		}
		return value(rt)(out), nil
	})
	k.method("with", 1, func(rt *Runtime, t temporal.PlainTime, args []Value) (Value, error) {
		const method = "Temporal.PlainTime.prototype.with"
		if err := rt.temporalRequirePartial(arg(args, 0)); err != nil {
			return Undefined, err
		}
		fields, found, err := rt.temporalTimeBag(arg(args, 0).Object())
		if err != nil {
			return Undefined, err
		}
		if !found {
			return Undefined, rt.temporalType("Must specify at least one time field.")
		}
		ov, err := rt.temporalOverflow(arg(args, 1), method)
		if err != nil {
			return Undefined, err
		}
		p, err := rt.temporalRegulateTime(fields, ov)
		if err != nil {
			return Undefined, err
		}
		out, err := t.With(p, ov)
		if err != nil {
			return Undefined, rt.temporalErr(err)
		}
		return value(rt)(out), nil
	})
	for _, sub := range []bool{false, true} {
		name := "add"
		if sub {
			name = "subtract"
		}
		k.method(name, 1, func(rt *Runtime, t temporal.PlainTime, args []Value) (Value, error) {
			dur, err := rt.toTemporalDuration(arg(args, 0))
			if err != nil {
				return Undefined, err
			}
			if sub {
				return value(rt)(t.Subtract(dur)), nil
			}
			return value(rt)(t.Add(dur)), nil
		})
	}
	temporalDifference(k, prototypeMethod("PlainTime"),
		func(rt *Runtime, v Value, method string) (temporal.PlainTime, error) {
			return rt.toTemporalPlainTime(v, Undefined, method)
		}, nil,
		func(a, b temporal.PlainTime, s temporal.DifferenceSettings, since bool) (temporal.Duration, error) {
			if since {
				return a.Since(b, s)
			}
			return a.Until(b, s)
		})
	k.method("toString", 0, func(rt *Runtime, t temporal.PlainTime, args []Value) (Value, error) {
		const method = "Temporal.PlainTime.prototype.toString"
		o, err := rt.temporalOptionsObject(arg(args, 0))
		if err != nil {
			return Undefined, err
		}
		opts, err := rt.temporalToStringOptions(o, method, nil)
		if err != nil {
			return Undefined, err
		}
		if err := rt.temporalValidateUnit(opts.SmallestUnit, temporalGroupTime, temporal.NoUnit); err != nil {
			return Undefined, err
		}
		s, err := t.String(opts)
		if err != nil {
			return Undefined, rt.temporalErr(err)
		}
		return Str(NewString(s)), nil
	})
	k.toJSON(func(rt *Runtime, t temporal.PlainTime) (string, error) {
		s, err := t.String(temporal.DefaultToStringOptions)
		return s, rt.temporalErr(err)
	})
	k.toLocaleString(intl.ComponentsTime, intl.ComponentsTime)
	k.valueOf()
}

// ==== PlainDateTime ====

func (r *Runtime) initTemporalPlainDateTime(ns *Object) {
	r.temporalPlainDateTimeProto = newObject(r.proto.object, ClassObject)
	k := temporalKind[temporal.PlainDateTime]{r, r.temporalPlainDateTimeProto, "PlainDateTime"}
	value := func(rt *Runtime) func(temporal.PlainDateTime) Value {
		return func(dt temporal.PlainDateTime) Value { return temporalObject(rt.temporalPlainDateTimeProto, dt) }
	}
	ctor := k.ctor(ns, 3, func(rt *Runtime, args []Value) (temporal.PlainDateTime, error) {
		var f [3]float64
		for i := range f {
			n, err := rt.temporalInteger(arg(args, i))
			if err != nil {
				return temporal.PlainDateTime{}, err
			}
			f[i] = n
		}
		t, err := rt.temporalTimeArgs(args, 3)
		if err != nil {
			return temporal.PlainDateTime{}, err
		}
		cal, err := rt.temporalCalendarArg(arg(args, 9))
		if err != nil {
			return temporal.PlainDateTime{}, err
		}
		if !temporalValidISODate(f[0], f[1], f[2]) {
			return temporal.PlainDateTime{}, rt.temporalRange("Invalid ISO date.")
		}
		if !temporalValidTime(t) {
			return temporal.PlainDateTime{}, rt.temporalRange("Invalid time")
		}
		out, err := temporal.NewPlainDateTime(temporal.ISODate{Year: int(f[0]), Month: int(f[1]), Day: int(f[2])},
			temporal.ISOTime{Hour: int(t[0]), Minute: int(t[1]), Second: int(t[2]), Millisecond: int(t[3]),
				Microsecond: int(t[4]), Nanosecond: int(t[5])}, cal, temporal.Reject)
		return out, rt.temporalErr(err)
	})
	r.defMethod(ctor, "from", 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
		dt, err := rt.toTemporalPlainDateTime(arg(args, 0), arg(args, 1), "Temporal.PlainDateTime.from")
		if err != nil {
			return Undefined, err
		}
		return value(rt)(dt), nil
	})
	r.defMethod(ctor, "compare", 2, func(rt *Runtime, this Value, args []Value) (Value, error) {
		const method = "Temporal.PlainDateTime.compare"
		a, err := rt.toTemporalPlainDateTime(arg(args, 0), Undefined, method)
		if err != nil {
			return Undefined, err
		}
		b, err := rt.toTemporalPlainDateTime(arg(args, 1), Undefined, method)
		if err != nil {
			return Undefined, err
		}
		return Int(a.Compare(b)), nil
	})

	temporalDateGetters(k, func(dt temporal.PlainDateTime) temporal.PlainDate { return dt.ToPlainDate() })
	temporalTimeGetters(k, func(dt temporal.PlainDateTime) temporal.ISOTime { return dt.ISO().Time })
	k.method("equals", 1, func(rt *Runtime, dt temporal.PlainDateTime, args []Value) (Value, error) {
		o, err := rt.toTemporalPlainDateTime(arg(args, 0), Undefined, "Temporal.PlainDateTime.prototype.equals")
		if err != nil {
			return Undefined, err
		}
		return Bool(dt.Equals(o)), nil
	})
	k.method("with", 1, func(rt *Runtime, dt temporal.PlainDateTime, args []Value) (Value, error) {
		const method = "Temporal.PlainDateTime.prototype.with"
		if err := rt.temporalRequirePartial(arg(args, 0)); err != nil {
			return Undefined, err
		}
		rec, err := rt.temporalPrepareFields(dt.Calendar(), arg(args, 0).Object(), temporalFieldsDate|temporalFieldsTime,
			temporalRequirePartial)
		if err != nil {
			return Undefined, err
		}
		ov, err := rt.temporalOverflow(arg(args, 1), method)
		if err != nil {
			return Undefined, err
		}
		f, err := rt.temporalRegulateDate(rec, ov)
		if err != nil {
			return Undefined, err
		}
		t, err := rt.temporalRegulateTime(rec.timeFields(), ov)
		if err != nil {
			return Undefined, err
		}
		out, err := dt.With(f, t, ov)
		if err != nil {
			return Undefined, rt.temporalErr(err)
		}
		return value(rt)(out), nil
	})
	k.method("withCalendar", 1, func(rt *Runtime, dt temporal.PlainDateTime, args []Value) (Value, error) {
		c, err := rt.temporalToCalendar(arg(args, 0))
		if err != nil {
			return Undefined, err
		}
		return value(rt)(dt.WithCalendar(c)), nil
	})
	k.method("withPlainTime", 0, func(rt *Runtime, dt temporal.PlainDateTime, args []Value) (Value, error) {
		t, err := rt.temporalTimeOrMidnight(arg(args, 0), "Temporal.PlainDateTime.prototype.withPlainTime")
		if err != nil {
			return Undefined, err
		}
		out, err := dt.WithPlainTime(t)
		if err != nil {
			return Undefined, rt.temporalErr(err)
		}
		return value(rt)(out), nil
	})
	k.method("toZonedDateTime", 1, func(rt *Runtime, dt temporal.PlainDateTime, args []Value) (Value, error) {
		const method = "Temporal.PlainDateTime.prototype.toZonedDateTime"
		tz, err := rt.temporalToTimeZone(arg(args, 0))
		if err != nil {
			return Undefined, err
		}
		d, err := rt.temporalDisambiguation(arg(args, 1), method)
		if err != nil {
			return Undefined, err
		}
		z, err := dt.ToZonedDateTime(tz, d)
		if err != nil {
			return Undefined, rt.temporalErr(err)
		}
		return temporalObject(rt.temporalZonedDateTimeProto, z), nil
	})
	k.method("toString", 0, func(rt *Runtime, dt temporal.PlainDateTime, args []Value) (Value, error) {
		// V8 names PlainDateTime's toString as Temporal.DateTime's.
		const method = "Temporal.DateTime.prototype.toString"
		o, err := rt.temporalOptionsObject(arg(args, 0))
		if err != nil {
			return Undefined, err
		}
		show, err := rt.temporalShowCalendar(o, method)
		if err != nil {
			return Undefined, err
		}
		opts, err := rt.temporalToStringOptions(o, method, nil)
		if err != nil {
			return Undefined, err
		}
		if err := rt.temporalValidateUnit(opts.SmallestUnit, temporalGroupTime, temporal.NoUnit); err != nil {
			return Undefined, err
		}
		s, err := dt.String(opts, show)
		if err != nil {
			return Undefined, rt.temporalErr(err)
		}
		return Str(NewString(s)), nil
	})
	k.method("round", 1, func(rt *Runtime, dt temporal.PlainDateTime, args []Value) (Value, error) {
		const method = "Temporal.PlainDateTime.prototype.round"
		o, err := rt.temporalRoundTo(arg(args, 0), false)
		if err != nil {
			return Undefined, err
		}
		opts, err := rt.temporalRoundingOptions(o, method, temporal.Day)
		if err != nil {
			return Undefined, err
		}
		out, err := dt.Round(opts)
		if err != nil {
			return Undefined, rt.temporalErr(err)
		}
		return value(rt)(out), nil
	})
	temporalAddOrSubtract(k, value(r), func(dt temporal.PlainDateTime, dur temporal.Duration, ov temporal.Overflow, sub bool) (temporal.PlainDateTime, error) {
		if sub {
			return dt.Subtract(dur, ov)
		}
		return dt.Add(dur, ov)
	})
	temporalDifference(k, prototypeMethod("PlainDateTime"),
		func(rt *Runtime, v Value, method string) (temporal.PlainDateTime, error) {
			return rt.toTemporalPlainDateTime(v, Undefined, method)
		},
		func(rt *Runtime, a, b temporal.PlainDateTime) error {
			return rt.temporalSameCalendar(a.Calendar(), b.Calendar())
		},
		func(a, b temporal.PlainDateTime, s temporal.DifferenceSettings, since bool) (temporal.Duration, error) {
			if since {
				return a.Since(b, s)
			}
			return a.Until(b, s)
		})
	k.method("toPlainDate", 0, func(rt *Runtime, dt temporal.PlainDateTime, args []Value) (Value, error) {
		return temporalObject(rt.temporalPlainDateProto, dt.ToPlainDate()), nil
	})
	k.method("toPlainTime", 0, func(rt *Runtime, dt temporal.PlainDateTime, args []Value) (Value, error) {
		return temporalObject(rt.temporalPlainTimeProto, dt.ToPlainTime()), nil
	})
	k.toJSON(func(rt *Runtime, dt temporal.PlainDateTime) (string, error) {
		s, err := dt.String(temporal.DefaultToStringOptions, temporal.CalendarAuto)
		return s, rt.temporalErr(err)
	})
	k.toLocaleString(intl.ComponentsAny, intl.ComponentsAll)
	k.valueOf()
}
