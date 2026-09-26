package vm

import (
	intl "github.com/go-quickjs/go-intl"
	"github.com/go-quickjs/go-intl/temporal"
)

// Temporal.PlainYearMonth and PlainMonthDay, as V8 binds them.

func (r *Runtime) initTemporalPlainYearMonth(ns *Object) {
	r.temporalPlainYearMonthProto = newObject(r.proto.object, ClassObject)
	k := temporalKind[temporal.PlainYearMonth]{r, r.temporalPlainYearMonthProto, "PlainYearMonth"}
	value := func(rt *Runtime) func(temporal.PlainYearMonth) Value {
		return func(ym temporal.PlainYearMonth) Value { return temporalObject(rt.temporalPlainYearMonthProto, ym) }
	}
	ctor := k.ctor(ns, 2, func(rt *Runtime, args []Value) (temporal.PlainYearMonth, error) {
		y, err := rt.temporalInteger(arg(args, 0))
		if err != nil {
			return temporal.PlainYearMonth{}, err
		}
		mo, err := rt.temporalInteger(arg(args, 1))
		if err != nil {
			return temporal.PlainYearMonth{}, err
		}
		cal, err := rt.temporalCalendarArg(arg(args, 2))
		if err != nil {
			return temporal.PlainYearMonth{}, err
		}
		ref := 1.0
		if !arg(args, 3).IsUndefined() {
			if ref, err = rt.temporalInteger(arg(args, 3)); err != nil {
				return temporal.PlainYearMonth{}, err
			}
		}
		if !temporalValidISODate(y, mo, ref) {
			return temporal.PlainYearMonth{}, rt.temporalRange("Invalid ISO date.")
		}
		day := int(ref)
		out, err := temporal.NewPlainYearMonth(int(y), int(mo), &day, cal, temporal.Reject)
		return out, rt.temporalErr(err)
	})
	r.defMethod(ctor, "from", 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
		ym, err := rt.toTemporalPlainYearMonth(arg(args, 0), arg(args, 1), "Temporal.PlainYearMonth.from")
		if err != nil {
			return Undefined, err
		}
		return value(rt)(ym), nil
	})
	r.defMethod(ctor, "compare", 2, func(rt *Runtime, this Value, args []Value) (Value, error) {
		const method = "Temporal.PlainYearMonth.compare"
		a, err := rt.toTemporalPlainYearMonth(arg(args, 0), Undefined, method)
		if err != nil {
			return Undefined, err
		}
		b, err := rt.toTemporalPlainYearMonth(arg(args, 1), Undefined, method)
		if err != nil {
			return Undefined, err
		}
		return Int(a.Compare(b)), nil
	})

	for _, g := range []struct {
		name string
		get  func(temporal.PlainYearMonth) Value
	}{
		{"calendarId", func(ym temporal.PlainYearMonth) Value { return Str(NewString(ym.Calendar().ID())) }},
		{"era", func(ym temporal.PlainYearMonth) Value {
			if era, ok := ym.Era(); ok && era != "" {
				return Str(NewString(era))
			}
			return Undefined
		}},
		{"eraYear", func(ym temporal.PlainYearMonth) Value {
			if y, ok := ym.EraYear(); ok {
				return Int(y)
			}
			return Undefined
		}},
		{"year", func(ym temporal.PlainYearMonth) Value { return Int(ym.Year()) }},
		{"month", func(ym temporal.PlainYearMonth) Value { return Int(ym.Month()) }},
		{"monthCode", func(ym temporal.PlainYearMonth) Value { return Str(NewString(ym.MonthCode())) }},
		{"daysInMonth", func(ym temporal.PlainYearMonth) Value { return Int(ym.DaysInMonth()) }},
		{"daysInYear", func(ym temporal.PlainYearMonth) Value { return Int(ym.DaysInYear()) }},
		{"monthsInYear", func(ym temporal.PlainYearMonth) Value { return Int(ym.MonthsInYear()) }},
		{"inLeapYear", func(ym temporal.PlainYearMonth) Value { return Bool(ym.InLeapYear()) }},
	} {
		get := g.get
		k.getter(g.name, func(rt *Runtime, ym temporal.PlainYearMonth) (Value, error) { return get(ym), nil })
	}
	k.method("equals", 1, func(rt *Runtime, ym temporal.PlainYearMonth, args []Value) (Value, error) {
		o, err := rt.toTemporalPlainYearMonth(arg(args, 0), Undefined, "Temporal.PlainYearMonth.prototype.equals")
		if err != nil {
			return Undefined, err
		}
		return Bool(ym.Equals(o)), nil
	})
	temporalAddOrSubtract(k, value(r), func(ym temporal.PlainYearMonth, dur temporal.Duration, ov temporal.Overflow, sub bool) (temporal.PlainYearMonth, error) {
		if sub {
			return ym.Subtract(dur, ov)
		}
		return ym.Add(dur, ov)
	})
	temporalDifference(k, prototypeMethod("PlainYearMonth"),
		func(rt *Runtime, v Value, method string) (temporal.PlainYearMonth, error) {
			return rt.toTemporalPlainYearMonth(v, Undefined, method)
		},
		func(rt *Runtime, a, b temporal.PlainYearMonth) error {
			return rt.temporalSameCalendar(a.Calendar(), b.Calendar())
		},
		func(a, b temporal.PlainYearMonth, s temporal.DifferenceSettings, since bool) (temporal.Duration, error) {
			if since {
				return a.Since(b, s)
			}
			return a.Until(b, s)
		})
	k.method("with", 1, func(rt *Runtime, ym temporal.PlainYearMonth, args []Value) (Value, error) {
		const method = "Temporal.PlainYearMonth.prototype.with"
		if err := rt.temporalRequirePartial(arg(args, 0)); err != nil {
			return Undefined, err
		}
		rec, err := rt.temporalPrepareFields(ym.Calendar(), arg(args, 0).Object(), temporalFieldsYear|temporalFieldsMonth,
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
		out, err := ym.With(f, ov)
		if err != nil {
			return Undefined, rt.temporalErr(err)
		}
		return value(rt)(out), nil
	})
	k.method("toPlainDate", 1, func(rt *Runtime, ym temporal.PlainYearMonth, args []Value) (Value, error) {
		if !arg(args, 0).IsObject() {
			return Undefined, rt.temporalType("year argument must be an object.")
		}
		rec, err := rt.temporalPrepareFields(ym.Calendar(), arg(args, 0).Object(), temporalFieldsDay, temporalRequireNone)
		if err != nil {
			return Undefined, err
		}
		f, err := rt.temporalRegulateDate(rec, temporal.Constrain)
		if err != nil {
			return Undefined, err
		}
		out, err := ym.ToPlainDate(f.Day)
		if err != nil {
			return Undefined, rt.temporalErr(err)
		}
		return temporalObject(rt.temporalPlainDateProto, out), nil
	})
	k.method("toString", 0, func(rt *Runtime, ym temporal.PlainYearMonth, args []Value) (Value, error) {
		o, err := rt.temporalOptionsObject(arg(args, 0))
		if err != nil {
			return Undefined, err
		}
		show, err := rt.temporalShowCalendar(o, "Temporal.PlainYearMonth.prototype.toString")
		if err != nil {
			return Undefined, err
		}
		return Str(NewString(ym.String(show))), nil
	})
	k.toJSON(func(rt *Runtime, ym temporal.PlainYearMonth) (string, error) {
		return ym.String(temporal.CalendarAuto), nil
	})
	k.toLocaleString(intl.ComponentsDate, intl.ComponentsDate)
	k.valueOf()
}

func (r *Runtime) initTemporalPlainMonthDay(ns *Object) {
	r.temporalPlainMonthDayProto = newObject(r.proto.object, ClassObject)
	k := temporalKind[temporal.PlainMonthDay]{r, r.temporalPlainMonthDayProto, "PlainMonthDay"}
	value := func(rt *Runtime) func(temporal.PlainMonthDay) Value {
		return func(md temporal.PlainMonthDay) Value { return temporalObject(rt.temporalPlainMonthDayProto, md) }
	}
	ctor := k.ctor(ns, 2, func(rt *Runtime, args []Value) (temporal.PlainMonthDay, error) {
		mo, err := rt.temporalInteger(arg(args, 0))
		if err != nil {
			return temporal.PlainMonthDay{}, err
		}
		d, err := rt.temporalInteger(arg(args, 1))
		if err != nil {
			return temporal.PlainMonthDay{}, err
		}
		cal, err := rt.temporalCalendarArg(arg(args, 2))
		if err != nil {
			return temporal.PlainMonthDay{}, err
		}
		ref := 1972.0
		if !arg(args, 3).IsUndefined() {
			if ref, err = rt.temporalInteger(arg(args, 3)); err != nil {
				return temporal.PlainMonthDay{}, err
			}
		}
		if !temporalValidISODate(ref, mo, d) {
			return temporal.PlainMonthDay{}, rt.temporalRange("Invalid ISO date.")
		}
		year := int(ref)
		out, err := temporal.NewPlainMonthDay(int(mo), int(d), cal, temporal.Reject, &year)
		return out, rt.temporalErr(err)
	})
	r.defMethod(ctor, "from", 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
		md, err := rt.toTemporalPlainMonthDay(arg(args, 0), arg(args, 1), "Temporal.PlainMonthDay.from")
		if err != nil {
			return Undefined, err
		}
		return value(rt)(md), nil
	})

	k.getter("calendarId", func(rt *Runtime, md temporal.PlainMonthDay) (Value, error) {
		return Str(NewString(md.Calendar().ID())), nil
	})
	k.getter("monthCode", func(rt *Runtime, md temporal.PlainMonthDay) (Value, error) {
		return Str(NewString(md.MonthCode())), nil
	})
	k.getter("day", func(rt *Runtime, md temporal.PlainMonthDay) (Value, error) { return Int(md.Day()), nil })
	k.method("equals", 1, func(rt *Runtime, md temporal.PlainMonthDay, args []Value) (Value, error) {
		o, err := rt.toTemporalPlainMonthDay(arg(args, 0), Undefined, "Temporal.PlainMonthDay.prototype.equals")
		if err != nil {
			return Undefined, err
		}
		return Bool(md.Equals(o)), nil
	})
	k.method("with", 1, func(rt *Runtime, md temporal.PlainMonthDay, args []Value) (Value, error) {
		// V8 names PlainMonthDay's with as PlainYearMonth's.
		const method = "Temporal.PlainYearMonth.prototype.with"
		if err := rt.temporalRequirePartial(arg(args, 0)); err != nil {
			return Undefined, err
		}
		rec, err := rt.temporalPrepareFields(md.Calendar(), arg(args, 0).Object(), temporalFieldsDate, temporalRequirePartial)
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
		out, err := md.With(f, ov)
		if err != nil {
			return Undefined, rt.temporalErr(err)
		}
		return value(rt)(out), nil
	})
	k.method("toPlainDate", 1, func(rt *Runtime, md temporal.PlainMonthDay, args []Value) (Value, error) {
		if !arg(args, 0).IsObject() {
			return Undefined, rt.temporalType("year argument must be an object.")
		}
		rec, err := rt.temporalPrepareFields(md.Calendar(), arg(args, 0).Object(), temporalFieldsYear, temporalRequireNone)
		if err != nil {
			return Undefined, err
		}
		f, err := rt.temporalRegulateDate(rec, temporal.Constrain)
		if err != nil {
			return Undefined, err
		}
		out, err := md.ToPlainDate(f.Year, f.Era, f.EraYear)
		if err != nil {
			return Undefined, rt.temporalErr(err)
		}
		return temporalObject(rt.temporalPlainDateProto, out), nil
	})
	k.method("toString", 0, func(rt *Runtime, md temporal.PlainMonthDay, args []Value) (Value, error) {
		o, err := rt.temporalOptionsObject(arg(args, 0))
		if err != nil {
			return Undefined, err
		}
		show, err := rt.temporalShowCalendar(o, "Temporal.PlainMonthDay.prototype.toString")
		if err != nil {
			return Undefined, err
		}
		return Str(NewString(md.String(show))), nil
	})
	k.toJSON(func(rt *Runtime, md temporal.PlainMonthDay) (string, error) {
		return md.String(temporal.CalendarAuto), nil
	})
	k.toLocaleString(intl.ComponentsDate, intl.ComponentsDate)
	k.valueOf()
}
