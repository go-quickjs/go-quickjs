package vm

import (
	"fmt"
	intl "github.com/go-quickjs/go-intl"
	"math"
	"strings"
	"time"

	"github.com/go-quickjs/go-quickjs/internal/icu"
)

type temporalPlainYearMonth struct {
	year, month, day int
	calendar         string
}

func (d temporalPlainYearMonth) valid() bool {
	if d.month < 1 || d.month > 12 || d.day < 1 ||
		d.day > isoDaysInMonth(d.year, d.month) {
		return false
	}
	return (d.year > -271821 || d.year == -271821 && d.month >= 4) &&
		(d.year < 275760 || d.year == 275760 && d.month <= 9)
}

func (d temporalPlainYearMonth) date() temporalPlainDate {
	return temporalPlainDate{
		year: d.year, month: d.month, day: d.day, calendar: d.calendar,
	}
}

func temporalPlainYearMonthFromDate(date temporalPlainDate) temporalPlainYearMonth {
	return temporalPlainYearMonth{
		year: date.year, month: date.month, day: date.day, calendar: date.calendar,
	}
}

func (r *Runtime) temporalYearMonthFromDate(date temporalPlainDate) (temporalPlainYearMonth, error) {
	calendarDate := date.calendarDate()
	month, leap := calendarDate.MonthCode()
	year, isoMonth, isoDay, ok := icu.ResolveDate(date.calendar,
		calendarDate.ArithmeticYear, month, 1, leap, true, false)
	if !ok {
		return temporalPlainYearMonth{}, r.throwRangeError("year-month is outside the Temporal range")
	}
	result := temporalPlainYearMonth{
		year: year, month: isoMonth, day: isoDay, calendar: date.calendar,
	}
	if !result.valid() {
		return temporalPlainYearMonth{}, r.throwRangeError("year-month is outside the Temporal range")
	}
	return result, nil
}

func (r *Runtime) temporalYearMonthArithmeticDate(yearMonth temporalPlainYearMonth) (temporalPlainDate, error) {
	resolved, err := r.temporalYearMonthFromDate(yearMonth.date())
	if err != nil {
		return temporalPlainDate{}, err
	}
	date := resolved.date()
	if !date.valid() {
		return temporalPlainDate{}, r.throwRangeError("year-month reference date is outside the Temporal date range")
	}
	return date, nil
}

type temporalPlainMonthDay struct {
	year, month, day int
	calendar         string
}

func (d temporalPlainMonthDay) valid() bool {
	return (temporalPlainDate{year: d.year, month: d.month, day: d.day}).valid()
}

func (d temporalPlainMonthDay) date() temporalPlainDate {
	return temporalPlainDate{
		year: d.year, month: d.month, day: d.day, calendar: d.calendar,
	}
}

func (r *Runtime) initTemporalPlainYearMonth(temporal *Object) {
	proto := newObject(r.proto.object, ClassObject)
	r.temporalPlainYearMonthProto = proto
	ctor := r.newTemporalCtor(temporal, "PlainYearMonth", 2, proto, func(rt *Runtime, this Value, args []Value) (Value, error) {
		if err := rt.requireNew("Temporal.PlainYearMonth"); err != nil {
			return Undefined, err
		}
		year, err := rt.temporalTruncatedInteger(arg(args, 0), "year")
		if err != nil {
			return Undefined, err
		}
		month, err := rt.temporalTruncatedInteger(arg(args, 1), "month")
		if err != nil {
			return Undefined, err
		}
		calendar, err := rt.toTemporalCalendarIdentifier(arg(args, 2))
		if err != nil {
			return Undefined, err
		}
		day := 1
		if !arg(args, 3).IsUndefined() {
			day, err = rt.temporalTruncatedInteger(arg(args, 3), "referenceISODay")
			if err != nil {
				return Undefined, err
			}
		}
		result := temporalPlainYearMonth{year: year, month: month, day: day, calendar: calendar}
		if !result.valid() {
			return Undefined, rt.throwRangeError("invalid Temporal.PlainYearMonth")
		}
		instanceProto, err := rt.protoFromNewTargetErr(proto)
		if err != nil {
			return Undefined, err
		}
		return Obj(newTemporalPlainYearMonth(instanceProto, result)), nil
	})

	r.defMethod(ctor, "from", 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
		result, err := rt.toTemporalPlainYearMonth(arg(args, 0), arg(args, 1))
		if err != nil {
			return Undefined, err
		}
		return Obj(newTemporalPlainYearMonth(proto, result)), nil
	})
	r.defMethod(ctor, "compare", 2, func(rt *Runtime, this Value, args []Value) (Value, error) {
		left, err := rt.toTemporalPlainYearMonth(arg(args, 0), Undefined)
		if err != nil {
			return Undefined, err
		}
		right, err := rt.toTemporalPlainYearMonth(arg(args, 1), Undefined)
		if err != nil {
			return Undefined, err
		}
		return Int(compareISODate(left.year, left.month, left.day,
			right.year, right.month, right.day)), nil
	})

	for _, property := range []string{"year", "month", "monthCode", "calendarId", "era", "eraYear"} {
		name := property
		r.defGetter(proto, name, func(rt *Runtime, this Value, args []Value) (Value, error) {
			yearMonth, err := rt.temporalPlainYearMonthValue(this, "get Temporal.PlainYearMonth.prototype."+name)
			if err != nil {
				return Undefined, err
			}
			date := yearMonth.date().calendarDate()
			switch name {
			case "year":
				return Int(date.ArithmeticYear), nil
			case "month":
				return Int(date.OrdinalMonth), nil
			case "monthCode":
				return Str(NewString(temporalCalendarMonthCode(date))), nil
			case "calendarId":
				return Str(NewString(yearMonth.calendar)), nil
			case "era":
				era, ok := temporalCalendarEra(yearMonth.calendar, date)
				if !ok {
					return Undefined, nil
				}
				return Str(NewString(era)), nil
			case "eraYear":
				eraYear, ok := temporalCalendarEraYear(yearMonth.calendar, date)
				if !ok {
					return Undefined, nil
				}
				return Int(eraYear), nil
			}
			return Undefined, nil
		})
	}
	for _, property := range []string{"daysInMonth", "daysInYear", "monthsInYear", "inLeapYear"} {
		name := property
		r.defGetter(proto, name, func(rt *Runtime, this Value, args []Value) (Value, error) {
			yearMonth, err := rt.temporalPlainYearMonthValue(this, "get Temporal.PlainYearMonth.prototype."+name)
			if err != nil {
				return Undefined, err
			}
			date := yearMonth.date().calendarDate()
			_, daysInMonth, daysInYear, monthsInYear, inLeapYear, ok :=
				icu.DateInfo(yearMonth.calendar, date)
			if !ok {
				return Undefined, rt.throwRangeError("year-month is outside the calendar range")
			}
			switch name {
			case "daysInMonth":
				return Int(daysInMonth), nil
			case "daysInYear":
				return Int(daysInYear), nil
			case "monthsInYear":
				return Int(monthsInYear), nil
			case "inLeapYear":
				return Bool(inLeapYear), nil
			}
			return Undefined, nil
		})
	}
	r.defMethod(proto, "equals", 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
		yearMonth, err := rt.temporalPlainYearMonthValue(this, "Temporal.PlainYearMonth.prototype.equals")
		if err != nil {
			return Undefined, err
		}
		other, err := rt.toTemporalPlainYearMonth(arg(args, 0), Undefined)
		if err != nil {
			return Undefined, err
		}
		return Bool(yearMonth == other), nil
	})
	r.defMethod(proto, "with", 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
		yearMonth, err := rt.temporalPlainYearMonthValue(this, "Temporal.PlainYearMonth.prototype.with")
		if err != nil {
			return Undefined, err
		}
		fields, err := rt.temporalPartialObject(arg(args, 0))
		if err != nil {
			return Undefined, err
		}
		partial, err := rt.temporalYearMonthFields(fields, yearMonth.calendar)
		if err != nil {
			return Undefined, err
		}
		if !partial.eraPresent && !partial.eraYearPresent &&
			!partial.monthPresent && !partial.monthCodePresent && !partial.yearPresent {
			return Undefined, rt.throwTypeError("year-month fields must not be empty")
		}
		if partial.eraPresent != partial.eraYearPresent {
			return Undefined, rt.throwTypeError("era and eraYear must be provided together")
		}
		if partial.monthPresent && partial.month < 1 {
			return Undefined, rt.throwRangeError("invalid Temporal.PlainYearMonth")
		}
		overflow, err := rt.temporalOverflowOption(arg(args, 1))
		if err != nil {
			return Undefined, err
		}
		date, err := rt.replaceTemporalPlainYearMonthFields(yearMonth, partial, overflow)
		if err != nil {
			return Undefined, err
		}
		result := temporalPlainYearMonthFromDate(date)
		return Obj(newTemporalPlainYearMonth(rt.temporalPlainYearMonthProto, result)), nil
	})
	for _, operation := range []struct {
		name string
		sign int64
	}{{"add", 1}, {"subtract", -1}} {
		op := operation
		r.defMethod(proto, op.name, 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
			yearMonth, err := rt.temporalPlainYearMonthValue(this, "Temporal.PlainYearMonth.prototype."+op.name)
			if err != nil {
				return Undefined, err
			}
			duration, err := rt.toTemporalDuration(arg(args, 0))
			if err != nil {
				return Undefined, err
			}
			overflow, err := rt.temporalOverflowOption(arg(args, 1))
			if err != nil {
				return Undefined, err
			}
			if duration.weeks != 0 || duration.days != 0 || duration.hours != 0 || duration.minutes != 0 ||
				duration.seconds != 0 || duration.milliseconds != 0 || duration.microseconds != 0 || duration.nanoseconds != 0 {
				return Undefined, rt.throwRangeError("units below months cannot be added to a year-month")
			}
			start, err := rt.temporalYearMonthArithmeticDate(yearMonth)
			if err != nil {
				return Undefined, err
			}
			date, err := rt.addTemporalPlainDate(start, duration,
				op.sign, overflow)
			if err != nil {
				return Undefined, err
			}
			result := temporalPlainYearMonthFromDate(date)
			return Obj(newTemporalPlainYearMonth(rt.temporalPlainYearMonthProto, result)), nil
		})
	}
	for _, operation := range []struct {
		name  string
		since bool
	}{{"until", false}, {"since", true}} {
		op := operation
		r.defMethod(proto, op.name, 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
			yearMonth, err := rt.temporalPlainYearMonthValue(this, "Temporal.PlainYearMonth.prototype."+op.name)
			if err != nil {
				return Undefined, err
			}
			other, err := rt.toTemporalPlainYearMonth(arg(args, 0), Undefined)
			if err != nil {
				return Undefined, err
			}
			largest, smallest, increment, mode, err := rt.temporalYearMonthDifferenceOptions(arg(args, 1))
			if err != nil {
				return Undefined, err
			}
			if yearMonth.calendar != other.calendar {
				return Undefined, rt.throwRangeError("year-month calendars must match")
			}
			leftCalendarDate := yearMonth.date().calendarDate()
			rightCalendarDate := other.date().calendarDate()
			if leftCalendarDate.ArithmeticYear == rightCalendarDate.ArithmeticYear &&
				leftCalendarDate.OrdinalMonth == rightCalendarDate.OrdinalMonth {
				return Obj(newTemporalDuration(rt.temporalDurationProto, temporalDuration{})), nil
			}
			start, err := rt.temporalYearMonthArithmeticDate(yearMonth)
			if err != nil {
				return Undefined, err
			}
			end, err := rt.temporalYearMonthArithmeticDate(other)
			if err != nil {
				return Undefined, err
			}
			if op.since {
				mode = negateTemporalRoundingMode(mode)
			}
			duration, err := rt.differenceTemporalPlainDates(start, end,
				largest, smallest, increment, mode)
			if err != nil {
				return Undefined, err
			}
			if duration.weeks != 0 || duration.days != 0 {
				return Undefined, rt.throwRangeError("year-month difference did not resolve to calendar months")
			}
			if op.since {
				values := duration.fields()
				for i := range values {
					if values[i] != 0 {
						values[i] = -values[i]
					}
				}
				duration = durationFromFields(values)
			}
			return Obj(newTemporalDuration(rt.temporalDurationProto, duration)), nil
		})
	}
	r.defMethod(proto, "toPlainDate", 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
		yearMonth, err := rt.temporalPlainYearMonthValue(this, "Temporal.PlainYearMonth.prototype.toPlainDate")
		if err != nil {
			return Undefined, err
		}
		fields := arg(args, 0)
		if !fields.IsObject() {
			return Undefined, rt.throwTypeError("day fields must be an object")
		}
		dayValue, err := rt.getProp(fields.Object(), rt.atoms.intern("day"), fields)
		if err != nil {
			return Undefined, err
		}
		if dayValue.IsUndefined() {
			return Undefined, rt.throwTypeError("day is required")
		}
		day, err := rt.temporalTruncatedInteger(dayValue, "day")
		if err != nil {
			return Undefined, err
		}
		if day < 1 {
			return Undefined, rt.throwRangeError("invalid Temporal.PlainDate")
		}
		calendarDate := yearMonth.date().calendarDate()
		monthCode, leap := calendarDate.MonthCode()
		year, month, day, ok := icu.ResolveDate(yearMonth.calendar,
			calendarDate.ArithmeticYear, monthCode, day, leap, true, true)
		if !ok {
			return Undefined, rt.throwRangeError("invalid Temporal.PlainDate")
		}
		date := temporalPlainDate{
			year: year, month: month, day: day, calendar: yearMonth.calendar,
		}
		if !date.valid() {
			return Undefined, rt.throwRangeError("invalid Temporal.PlainDate")
		}
		return Obj(newTemporalPlainDate(rt.temporalPlainDateProto, date)), nil
	})
	r.defMethod(proto, "toString", 0, temporalPlainYearMonthToString)
	r.defMethod(proto, "toJSON", 0, func(rt *Runtime, this Value, args []Value) (Value, error) {
		yearMonth, err := rt.temporalPlainYearMonthValue(this, "Temporal.PlainYearMonth.prototype.toJSON")
		if err != nil {
			return Undefined, err
		}
		return Str(NewString(yearMonth.string("auto"))), nil
	})
	r.defMethod(proto, "toLocaleString", 0, func(rt *Runtime, this Value, args []Value) (Value, error) {
		if _, err := rt.temporalPlainYearMonthValue(this, "Temporal.PlainYearMonth.prototype.toLocaleString"); err != nil {
			return Undefined, err
		}
		return rt.temporalToLocaleString(this, args, intl.ComponentsDate, intl.ComponentsDate)
	})
	r.defMethod(proto, "valueOf", 0, func(rt *Runtime, this Value, args []Value) (Value, error) {
		if _, err := rt.temporalPlainYearMonthValue(this, "Temporal.PlainYearMonth.prototype.valueOf"); err != nil {
			return Undefined, err
		}
		return Undefined, rt.throwTypeError("use Temporal.PlainYearMonth.compare() or equals() to compare year-months")
	})
	r.defToStringTag(proto, "Temporal.PlainYearMonth")
}

func (r *Runtime) initTemporalPlainMonthDay(temporal *Object) {
	proto := newObject(r.proto.object, ClassObject)
	r.temporalPlainMonthDayProto = proto
	ctor := r.newTemporalCtor(temporal, "PlainMonthDay", 2, proto, func(rt *Runtime, this Value, args []Value) (Value, error) {
		if err := rt.requireNew("Temporal.PlainMonthDay"); err != nil {
			return Undefined, err
		}
		month, err := rt.temporalTruncatedInteger(arg(args, 0), "month")
		if err != nil {
			return Undefined, err
		}
		day, err := rt.temporalTruncatedInteger(arg(args, 1), "day")
		if err != nil {
			return Undefined, err
		}
		calendar, err := rt.toTemporalCalendarIdentifier(arg(args, 2))
		if err != nil {
			return Undefined, err
		}
		year := 1972
		if !arg(args, 3).IsUndefined() {
			year, err = rt.temporalTruncatedInteger(arg(args, 3), "referenceISOYear")
			if err != nil {
				return Undefined, err
			}
		}
		result := temporalPlainMonthDay{year: year, month: month, day: day, calendar: calendar}
		if !result.valid() {
			return Undefined, rt.throwRangeError("invalid Temporal.PlainMonthDay")
		}
		instanceProto, err := rt.protoFromNewTargetErr(proto)
		if err != nil {
			return Undefined, err
		}
		return Obj(newTemporalPlainMonthDay(instanceProto, result)), nil
	})
	r.defMethod(ctor, "from", 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
		result, err := rt.toTemporalPlainMonthDay(arg(args, 0), arg(args, 1))
		if err != nil {
			return Undefined, err
		}
		return Obj(newTemporalPlainMonthDay(proto, result)), nil
	})

	for _, property := range []string{"monthCode", "day", "calendarId"} {
		name := property
		r.defGetter(proto, name, func(rt *Runtime, this Value, args []Value) (Value, error) {
			monthDay, err := rt.temporalPlainMonthDayValue(this, "get Temporal.PlainMonthDay.prototype."+name)
			if err != nil {
				return Undefined, err
			}
			date := temporalPlainDate{year: monthDay.year, month: monthDay.month, day: monthDay.day, calendar: monthDay.calendar}.calendarDate()
			switch name {
			case "monthCode":
				return Str(NewString(temporalCalendarMonthCode(date))), nil
			case "day":
				return Int(date.Day), nil
			case "calendarId":
				return Str(NewString(monthDay.calendar)), nil
			}
			return Undefined, nil
		})
	}
	r.defMethod(proto, "equals", 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
		monthDay, err := rt.temporalPlainMonthDayValue(this, "Temporal.PlainMonthDay.prototype.equals")
		if err != nil {
			return Undefined, err
		}
		other, err := rt.toTemporalPlainMonthDay(arg(args, 0), Undefined)
		if err != nil {
			return Undefined, err
		}
		return Bool(monthDay == other), nil
	})
	r.defMethod(proto, "with", 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
		monthDay, err := rt.temporalPlainMonthDayValue(this, "Temporal.PlainMonthDay.prototype.with")
		if err != nil {
			return Undefined, err
		}
		fields, err := rt.temporalPartialObject(arg(args, 0))
		if err != nil {
			return Undefined, err
		}
		partial, err := rt.temporalPartialDateFields(fields, monthDay.calendar)
		if err != nil {
			return Undefined, err
		}
		if !partial.dayPresent && !partial.eraPresent &&
			!partial.eraYearPresent && !partial.monthPresent &&
			!partial.monthCodePresent && !partial.yearPresent {
			return Undefined, rt.throwTypeError("month-day fields must not be empty")
		}
		if partial.dayPresent && partial.day < 1 ||
			partial.monthPresent && partial.month < 1 {
			return Undefined, rt.throwRangeError(
				"invalid Temporal.PlainMonthDay")
		}
		overflow, err := rt.temporalOverflowOption(arg(args, 1))
		if err != nil {
			return Undefined, err
		}
		calendarDate := monthDay.date().calendarDate()
		if !partial.dayPresent {
			partial.day, partial.dayPresent = calendarDate.Day, true
		}
		if !partial.monthPresent && !partial.monthCodePresent {
			partial.monthCode = temporalCalendarMonthCode(calendarDate)
			partial.monthCodePresent = true
		}
		result, err := rt.temporalPlainMonthDayFromFields(
			monthDay.calendar, partial, overflow)
		if err != nil {
			return Undefined, err
		}
		return Obj(newTemporalPlainMonthDay(rt.temporalPlainMonthDayProto, result)), nil
	})
	r.defMethod(proto, "toPlainDate", 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
		monthDay, err := rt.temporalPlainMonthDayValue(this, "Temporal.PlainMonthDay.prototype.toPlainDate")
		if err != nil {
			return Undefined, err
		}
		fields := arg(args, 0)
		if !fields.IsObject() {
			return Undefined, rt.throwTypeError("year fields must be an object")
		}
		year, err := rt.temporalMonthDayYearFromFields(fields.Object(), monthDay.calendar)
		if err != nil {
			return Undefined, err
		}
		calendarDate := monthDay.date().calendarDate()
		month, leap := calendarDate.MonthCode()
		date, err := rt.resolveTemporalMonthDayDate(monthDay.calendar, year,
			month, leap, calendarDate.Day, true)
		if err != nil {
			return Undefined, err
		}
		return Obj(newTemporalPlainDate(rt.temporalPlainDateProto, date)), nil
	})
	r.defMethod(proto, "toString", 0, temporalPlainMonthDayToString)
	r.defMethod(proto, "toJSON", 0, func(rt *Runtime, this Value, args []Value) (Value, error) {
		monthDay, err := rt.temporalPlainMonthDayValue(this, "Temporal.PlainMonthDay.prototype.toJSON")
		if err != nil {
			return Undefined, err
		}
		return Str(NewString(monthDay.string("auto"))), nil
	})
	r.defMethod(proto, "toLocaleString", 0, func(rt *Runtime, this Value, args []Value) (Value, error) {
		if _, err := rt.temporalPlainMonthDayValue(this, "Temporal.PlainMonthDay.prototype.toLocaleString"); err != nil {
			return Undefined, err
		}
		return rt.temporalToLocaleString(this, args, intl.ComponentsDate, intl.ComponentsDate)
	})
	r.defMethod(proto, "valueOf", 0, func(rt *Runtime, this Value, args []Value) (Value, error) {
		if _, err := rt.temporalPlainMonthDayValue(this, "Temporal.PlainMonthDay.prototype.valueOf"); err != nil {
			return Undefined, err
		}
		return Undefined, rt.throwTypeError("use equals() to compare month-days")
	})
	r.defToStringTag(proto, "Temporal.PlainMonthDay")
}

func newTemporalPlainYearMonth(proto *Object, value temporalPlainYearMonth) *Object {
	o := newObject(proto, ClassObject)
	o.data = &value
	return o
}

func newTemporalPlainMonthDay(proto *Object, value temporalPlainMonthDay) *Object {
	o := newObject(proto, ClassObject)
	o.data = &value
	return o
}

func (r *Runtime) temporalPlainYearMonthValue(value Value, method string) (temporalPlainYearMonth, error) {
	if value.IsObject() {
		if yearMonth, ok := value.Object().data.(*temporalPlainYearMonth); ok && yearMonth != nil {
			return *yearMonth, nil
		}
	}
	return temporalPlainYearMonth{}, r.throwTypeError("%s called on an incompatible receiver", method)
}

func (r *Runtime) temporalPlainMonthDayValue(value Value, method string) (temporalPlainMonthDay, error) {
	if value.IsObject() {
		if monthDay, ok := value.Object().data.(*temporalPlainMonthDay); ok && monthDay != nil {
			return *monthDay, nil
		}
	}
	return temporalPlainMonthDay{}, r.throwTypeError("%s called on an incompatible receiver", method)
}

func (r *Runtime) temporalPartialObject(value Value) (*Object, error) {
	if !value.IsObject() || isTemporalObject(value.Object()) {
		return nil, r.throwTypeError("partial Temporal fields must be an ordinary object")
	}
	o := value.Object()
	calendar, err := r.getProp(o, r.atoms.intern("calendar"), value)
	if err != nil {
		return nil, err
	}
	timeZone, err := r.getProp(o, r.atoms.intern("timeZone"), value)
	if err != nil {
		return nil, err
	}
	if !calendar.IsUndefined() || !timeZone.IsUndefined() {
		return nil, r.throwTypeError("partial Temporal fields must not contain calendar or timeZone")
	}
	return o, nil
}

func isTemporalObject(o *Object) bool {
	switch o.data.(type) {
	case *temporalDuration, *temporalInstant, *temporalPlainDate, *temporalPlainDateTime,
		*temporalPlainMonthDay, *temporalPlainTime, *temporalPlainYearMonth, *temporalZonedDateTime:
		return true
	default:
		return false
	}
}

func compareISODate(leftYear, leftMonth, leftDay, rightYear, rightMonth, rightDay int) int {
	if leftYear != rightYear {
		if leftYear < rightYear {
			return -1
		}
		return 1
	}
	if leftMonth != rightMonth {
		if leftMonth < rightMonth {
			return -1
		}
		return 1
	}
	if leftDay < rightDay {
		return -1
	}
	if leftDay > rightDay {
		return 1
	}
	return 0
}

func normalizeTemporalYearMonthUnit(unit string) (string, bool) {
	unit = strings.TrimSuffix(unit, "s")
	return unit, unit == "year" || unit == "month"
}

func (r *Runtime) temporalYearMonthDifferenceOptions(value Value) (largest, smallest string, increment int64, mode string, err error) {
	options, err := r.strictOptions(value)
	if err != nil {
		return "", "", 0, "", err
	}
	largestRaw, err := r.stringOption(options, "largestUnit", "auto")
	if err != nil {
		return "", "", 0, "", err
	}
	rawIncrement, incrementSet, err := r.rawNumberOption(options, "roundingIncrement")
	if err != nil {
		return "", "", 0, "", err
	}
	mode, err = r.stringOption(options, "roundingMode", "trunc",
		"ceil", "floor", "expand", "trunc", "halfCeil", "halfFloor", "halfExpand", "halfTrunc", "halfEven")
	if err != nil {
		return "", "", 0, "", err
	}
	smallestRaw, err := r.stringOption(options, "smallestUnit", "month")
	if err != nil {
		return "", "", 0, "", err
	}
	var ok bool
	smallest, ok = normalizeTemporalYearMonthUnit(smallestRaw)
	if !ok {
		return "", "", 0, "", r.throwRangeError("invalid smallestUnit")
	}
	if largestRaw == "auto" {
		largest = "year"
	} else {
		largest, ok = normalizeTemporalYearMonthUnit(largestRaw)
		if !ok {
			return "", "", 0, "", r.throwRangeError("invalid largestUnit")
		}
	}
	if largest == "month" && smallest == "year" {
		return "", "", 0, "", r.throwRangeError("largestUnit must not be smaller than smallestUnit")
	}
	if !incrementSet {
		increment = 1
	} else {
		if math.IsNaN(rawIncrement) || math.IsInf(rawIncrement, 0) {
			return "", "", 0, "", r.throwRangeError("roundingIncrement must be finite")
		}
		rawIncrement = math.Trunc(rawIncrement)
		if rawIncrement < 1 || rawIncrement > 1_000_000_000 {
			return "", "", 0, "", r.throwRangeError("roundingIncrement is out of range")
		}
		increment = int64(rawIncrement)
	}
	return
}

func temporalPlainYearMonthToString(rt *Runtime, this Value, args []Value) (Value, error) {
	yearMonth, err := rt.temporalPlainYearMonthValue(this, "Temporal.PlainYearMonth.prototype.toString")
	if err != nil {
		return Undefined, err
	}
	options, err := rt.strictOptions(arg(args, 0))
	if err != nil {
		return Undefined, err
	}
	show, err := rt.stringOption(options, "calendarName", "auto", "auto", "always", "never", "critical")
	if err != nil {
		return Undefined, err
	}
	return Str(NewString(yearMonth.string(show))), nil
}

func temporalPlainMonthDayToString(rt *Runtime, this Value, args []Value) (Value, error) {
	monthDay, err := rt.temporalPlainMonthDayValue(this, "Temporal.PlainMonthDay.prototype.toString")
	if err != nil {
		return Undefined, err
	}
	options, err := rt.strictOptions(arg(args, 0))
	if err != nil {
		return Undefined, err
	}
	show, err := rt.stringOption(options, "calendarName", "auto", "auto", "always", "never", "critical")
	if err != nil {
		return Undefined, err
	}
	return Str(NewString(monthDay.string(show))), nil
}

func (d temporalPlainYearMonth) string(showCalendar string) string {
	showAnnotation := showCalendar == "always" || showCalendar == "critical" || showCalendar == "auto" && d.calendar != "iso8601"
	showDay := d.calendar != "iso8601" || showCalendar == "always" || showCalendar == "critical"
	var b strings.Builder
	writeTemporalISOYear(&b, d.year)
	b.WriteByte('-')
	writePaddedTemporalInt(&b, d.month, 2)
	if showDay {
		b.WriteByte('-')
		writePaddedTemporalInt(&b, d.day, 2)
	}
	if showAnnotation {
		writeTemporalCalendarAnnotation(&b, d.calendar, showCalendar)
	}
	return b.String()
}

func (d temporalPlainMonthDay) string(showCalendar string) string {
	showAnnotation := showCalendar == "always" || showCalendar == "critical" || showCalendar == "auto" && d.calendar != "iso8601"
	var b strings.Builder
	if d.calendar != "iso8601" || showAnnotation {
		writeTemporalISOYear(&b, d.year)
		b.WriteByte('-')
	}
	writePaddedTemporalInt(&b, d.month, 2)
	b.WriteByte('-')
	writePaddedTemporalInt(&b, d.day, 2)
	if showAnnotation {
		writeTemporalCalendarAnnotation(&b, d.calendar, showCalendar)
	}
	return b.String()
}

func writeTemporalCalendarAnnotation(b *strings.Builder, calendar, show string) {
	b.WriteByte('[')
	if show == "critical" {
		b.WriteByte('!')
	}
	b.WriteString("u-ca=")
	b.WriteString(calendar)
	b.WriteByte(']')
}

func (r *Runtime) toTemporalPlainYearMonth(value, optionsValue Value) (temporalPlainYearMonth, error) {
	if value.IsObject() {
		if result, ok := value.Object().data.(*temporalPlainYearMonth); ok && result != nil {
			if _, err := r.temporalOverflowOption(optionsValue); err != nil {
				return temporalPlainYearMonth{}, err
			}
			return *result, nil
		}
		if date, ok := value.Object().data.(*temporalPlainDate); ok && date != nil {
			if _, err := r.temporalOverflowOption(optionsValue); err != nil {
				return temporalPlainYearMonth{}, err
			}
			return r.temporalYearMonthFromDate(*date)
		}
		return r.temporalPlainYearMonthFromBag(value.Object(), optionsValue)
	}
	if !value.IsString() {
		return temporalPlainYearMonth{}, r.throwTypeError("a plain year-month must be a string or object")
	}
	result, err := parseTemporalPlainYearMonth(value.String().Go())
	if err != nil {
		return temporalPlainYearMonth{}, r.throwRangeError("invalid Temporal.PlainYearMonth string")
	}
	if _, err := r.temporalOverflowOption(optionsValue); err != nil {
		return temporalPlainYearMonth{}, err
	}
	if result.calendar == "iso8601" {
		result.day = 1
	}
	return result, nil
}

func (r *Runtime) temporalPlainYearMonthFromBag(o *Object, optionsValue Value) (temporalPlainYearMonth, error) {
	calendarValue, err := r.getProp(o, r.atoms.intern("calendar"), Obj(o))
	if err != nil {
		return temporalPlainYearMonth{}, err
	}
	calendar, err := r.toTemporalCalendarIdentifierFromBag(calendarValue)
	if err != nil {
		return temporalPlainYearMonth{}, err
	}
	fields, err := r.temporalYearMonthFields(o, calendar)
	if err != nil {
		return temporalPlainYearMonth{}, err
	}
	overflow, err := r.temporalOverflowOption(optionsValue)
	if err != nil {
		return temporalPlainYearMonth{}, err
	}
	if fields.eraPresent != fields.eraYearPresent {
		return temporalPlainYearMonth{}, r.throwTypeError("era and eraYear must be provided together")
	}
	if !fields.yearPresent && fields.eraPresent {
		fields.year, _ = temporalYearFromEra(calendar, fields.era, fields.eraYear)
		fields.yearPresent = true
	}
	if !fields.yearPresent || !fields.monthPresent && !fields.monthCodePresent {
		return temporalPlainYearMonth{}, r.throwTypeError("plain year-month property bag is missing required fields")
	}
	date, err := r.resolveTemporalYearMonthFields(calendar, fields, overflow)
	if err != nil {
		return temporalPlainYearMonth{}, err
	}
	return temporalPlainYearMonthFromDate(date), nil
}

func (r *Runtime) toTemporalPlainMonthDay(value, optionsValue Value) (temporalPlainMonthDay, error) {
	if value.IsObject() {
		if result, ok := value.Object().data.(*temporalPlainMonthDay); ok && result != nil {
			if _, err := r.temporalOverflowOption(optionsValue); err != nil {
				return temporalPlainMonthDay{}, err
			}
			return *result, nil
		}
		if date, ok := value.Object().data.(*temporalPlainDate); ok && date != nil {
			overflow, err := r.temporalOverflowOption(optionsValue)
			if err != nil {
				return temporalPlainMonthDay{}, err
			}
			return r.temporalMonthDayFromDate(*date, overflow)
		}
		return r.temporalPlainMonthDayFromBag(value.Object(), optionsValue)
	}
	if !value.IsString() {
		return temporalPlainMonthDay{}, r.throwTypeError("a plain month-day must be a string or object")
	}
	result, err := parseTemporalPlainMonthDay(value.String().Go())
	if err != nil {
		return temporalPlainMonthDay{}, r.throwRangeError("invalid Temporal.PlainMonthDay string")
	}
	if _, err := r.temporalOverflowOption(optionsValue); err != nil {
		return temporalPlainMonthDay{}, err
	}
	if result.calendar == "iso8601" {
		result.year = 1972
		return result, nil
	}
	return r.temporalMonthDayFromDate(result.date(), "constrain")
}

func (r *Runtime) temporalPlainMonthDayFromBag(o *Object, optionsValue Value) (temporalPlainMonthDay, error) {
	calendarValue, err := r.getProp(o, r.atoms.intern("calendar"), Obj(o))
	if err != nil {
		return temporalPlainMonthDay{}, err
	}
	calendar, err := r.toTemporalCalendarIdentifierFromBag(calendarValue)
	if err != nil {
		return temporalPlainMonthDay{}, err
	}
	fields, err := r.temporalPartialDateFields(o, calendar)
	if err != nil {
		return temporalPlainMonthDay{}, err
	}
	overflow, err := r.temporalOverflowOption(optionsValue)
	if err != nil {
		return temporalPlainMonthDay{}, err
	}
	return r.temporalPlainMonthDayFromFields(calendar, fields, overflow)
}

func (r *Runtime) temporalPlainMonthDayFromFields(calendar string,
	fields temporalPartialDateFields, overflow string) (temporalPlainMonthDay, error) {
	if fields.eraPresent != fields.eraYearPresent {
		return temporalPlainMonthDay{}, r.throwTypeError(
			"era and eraYear must be provided together")
	}
	year, yearPresent := fields.year, fields.yearPresent
	if fields.eraPresent {
		eraYear, _ := temporalYearFromEra(calendar, fields.era, fields.eraYear)
		if yearPresent && year != eraYear {
			return temporalPlainMonthDay{}, r.throwRangeError(
				"year and eraYear do not agree")
		}
		year, yearPresent = eraYear, true
	}
	if !fields.dayPresent || !fields.monthPresent && !fields.monthCodePresent ||
		calendar != "iso8601" && fields.monthPresent && !yearPresent {
		return temporalPlainMonthDay{}, r.throwTypeError(
			"plain month-day property bag is missing required fields")
	}
	if fields.day < 1 || fields.monthPresent && fields.month < 1 {
		return temporalPlainMonthDay{}, r.throwRangeError(
			"invalid Temporal.PlainMonthDay")
	}

	codeMonth, leap, codePresent := fields.month, false, false
	if fields.monthCodePresent {
		var ok bool
		codeMonth, leap, ok = parseTemporalMonthCode(fields.monthCode)
		if !ok || !temporalCalendarMonthCodeValid(calendar, codeMonth, leap) {
			return temporalPlainMonthDay{}, r.throwRangeError("invalid monthCode")
		}
		codePresent = true
	}
	if calendar == "iso8601" {
		month := fields.month
		if codePresent {
			if fields.monthPresent && fields.month != codeMonth {
				return temporalPlainMonthDay{}, r.throwRangeError(
					"month and monthCode do not agree")
			}
			month = codeMonth
		}
		validationYear := 1972
		if yearPresent {
			validationYear = year
		}
		day := fields.day
		if overflow == "constrain" {
			month = min(month, 12)
			day = min(day, isoDaysInMonth(validationYear, month))
		} else if month > 12 || day > isoDaysInMonth(validationYear, month) {
			return temporalPlainMonthDay{}, r.throwRangeError(
				"invalid Temporal.PlainMonthDay")
		}
		return r.temporalMonthDayReferenceDate(calendar, month, false,
			day, "reject")
	}

	if yearPresent || !codePresent {
		if !yearPresent {
			year = 1972
		}
		date, err := r.resolveTemporalMonthDayFieldsDate(calendar, year,
			fields, overflow == "constrain")
		if err != nil {
			return temporalPlainMonthDay{}, err
		}
		calendarDate := date.calendarDate()
		codeMonth, leap = calendarDate.MonthCode()
		fields.day = calendarDate.Day
	}
	return r.temporalMonthDayReferenceDate(calendar, codeMonth, leap,
		fields.day, overflow)
}

func (r *Runtime) resolveTemporalMonthDayFieldsDate(calendar string, year int,
	fields temporalPartialDateFields, constrain bool) (temporalPlainDate, error) {
	month, leap, byCode := fields.month, false, false
	if fields.monthCodePresent {
		var ok bool
		month, leap, ok = parseTemporalMonthCode(fields.monthCode)
		if !ok || !temporalCalendarMonthCodeValid(calendar, month, leap) {
			return temporalPlainDate{}, r.throwRangeError("invalid monthCode")
		}
		byCode = true
	}
	isoYear, isoMonth, isoDay, ok := icu.ResolveDate(calendar, year,
		month, fields.day, leap, byCode, constrain)
	if !ok && byCode && leap && constrain {
		month = temporalMonthDayLeapFallbackMonth(calendar, month)
		isoYear, isoMonth, isoDay, ok = icu.ResolveDate(calendar, year,
			month, fields.day, false, true, true)
	}
	if !ok {
		return temporalPlainDate{}, r.throwRangeError(
			"invalid Temporal.PlainMonthDay")
	}
	result := temporalPlainDate{
		year: isoYear, month: isoMonth, day: isoDay, calendar: calendar,
	}
	if !result.valid() {
		return temporalPlainDate{}, r.throwRangeError(
			"invalid Temporal.PlainMonthDay")
	}
	if fields.monthPresent && fields.monthCodePresent &&
		result.calendarDate().OrdinalMonth != fields.month {
		return temporalPlainDate{}, r.throwRangeError(
			"month and monthCode do not agree")
	}
	return result, nil
}

func temporalMonthDayLeapFallbackMonth(calendar string, month int) int {
	if calendar == "hebrew" {
		return 6
	}
	return month
}

func (r *Runtime) temporalMonthDayReferenceDate(calendar string, month int,
	leap bool, day int, overflow string) (temporalPlainMonthDay, error) {
	if !temporalCalendarMonthCodeValid(calendar, month, leap) || day < 1 {
		return temporalPlainMonthDay{}, r.throwRangeError(
			"invalid Temporal.PlainMonthDay")
	}
	if result, ok := temporalMonthDayReferenceExact(calendar, month, leap, day); ok {
		return result, nil
	}
	if overflow == "reject" {
		return temporalPlainMonthDay{}, r.throwRangeError(
			"invalid Temporal.PlainMonthDay")
	}

	regularMonth := month
	if leap {
		regularMonth = temporalMonthDayLeapFallbackMonth(calendar, month)
	}
	regularMaximum := temporalMonthDayMaximum(calendar, regularMonth, false)
	requestedMaximum := temporalMonthDayMaximum(calendar, month, leap)
	maximum := max(regularMaximum, requestedMaximum)
	if maximum == 0 {
		return temporalPlainMonthDay{}, r.throwRangeError(
			"invalid Temporal.PlainMonthDay")
	}
	day = min(day, maximum)
	if requestedMaximum >= day {
		if result, ok := temporalMonthDayReferenceExact(
			calendar, month, leap, day); ok {
			return result, nil
		}
	}
	if result, ok := temporalMonthDayReferenceExact(
		calendar, regularMonth, false, day); ok {
		return result, nil
	}
	return temporalPlainMonthDay{}, r.throwRangeError(
		"invalid Temporal.PlainMonthDay")
}

func temporalMonthDayMaximum(calendar string, month int, leap bool) int {
	for day := 31; day >= 1; day-- {
		if _, ok := temporalMonthDayReferenceExact(calendar, month, leap, day); ok {
			return day
		}
	}
	return 0
}

func temporalMonthDayReferenceExact(calendar string, month int, leap bool,
	day int) (temporalPlainMonthDay, bool) {
	cutoff := time.Date(1972, 12, 31, 12, 0, 0, 0, time.UTC)
	cutoffDays := isoDaysFromCivil(1972, 12, 31)
	referenceYear := icu.DateIn(calendar, cutoff).ArithmeticYear
	var before, after temporalPlainMonthDay
	const minInt64 = int64(-1 << 63)
	const maxInt64 = int64(1<<63 - 1)
	beforeDays, afterDays := minInt64, maxInt64
	for year := referenceYear - 400; year <= referenceYear+400; year++ {
		isoYear, isoMonth, isoDay, ok := icu.ResolveDate(calendar, year,
			month, day, leap, true, false)
		if !ok {
			continue
		}
		candidate := temporalPlainMonthDay{
			year: isoYear, month: isoMonth, day: isoDay, calendar: calendar,
		}
		if (calendar == "chinese" || calendar == "dangi") &&
			(isoYear < 1900 || isoYear > 2034) {
			continue
		}
		if !candidate.valid() {
			continue
		}
		candidateDays := isoDaysFromCivil(int64(isoYear), isoMonth, isoDay)
		if candidateDays <= cutoffDays && candidateDays > beforeDays {
			before, beforeDays = candidate, candidateDays
		}
		if candidateDays > cutoffDays && candidateDays < afterDays {
			after, afterDays = candidate, candidateDays
		}
	}
	if beforeDays != minInt64 {
		return before, true
	}
	if afterDays != maxInt64 {
		return after, true
	}
	return temporalPlainMonthDay{}, false
}

func (r *Runtime) temporalMonthDayFromDate(date temporalPlainDate,
	overflow string) (temporalPlainMonthDay, error) {
	calendarDate := date.calendarDate()
	month, leap := calendarDate.MonthCode()
	return r.temporalMonthDayReferenceDate(date.calendar, month, leap,
		calendarDate.Day, overflow)
}

func (r *Runtime) temporalMonthDayYearFromFields(o *Object,
	calendar string) (int, error) {
	era, eraYear, year := "", 0, 0
	eraPresent, eraYearPresent, yearPresent := false, false, false
	if temporalCalendarUsesEra(calendar) {
		eraValue, err := r.getProp(o, r.atoms.intern("era"), Obj(o))
		if err != nil {
			return 0, err
		}
		eraPresent = !eraValue.IsUndefined()
		if eraPresent {
			value, err := r.toString(eraValue)
			if err != nil {
				return 0, err
			}
			era = asciiLower(value.Go())
			if _, valid := temporalYearFromEra(calendar, era, 1); !valid {
				return 0, r.throwRangeError("invalid calendar era")
			}
		}
		eraYearValue, err := r.getProp(o, r.atoms.intern("eraYear"), Obj(o))
		if err != nil {
			return 0, err
		}
		eraYearPresent = !eraYearValue.IsUndefined()
		if eraYearPresent {
			eraYear, err = r.temporalTruncatedInteger(eraYearValue, "eraYear")
			if err != nil {
				return 0, err
			}
		}
	}
	yearValue, err := r.getProp(o, r.atoms.intern("year"), Obj(o))
	if err != nil {
		return 0, err
	}
	yearPresent = !yearValue.IsUndefined()
	if yearPresent {
		year, err = r.temporalTruncatedInteger(yearValue, "year")
		if err != nil {
			return 0, err
		}
	}
	if eraPresent != eraYearPresent {
		return 0, r.throwTypeError("era and eraYear must be provided together")
	}
	if eraPresent {
		eraArithmeticYear, _ := temporalYearFromEra(calendar, era, eraYear)
		if yearPresent && year != eraArithmeticYear {
			return 0, r.throwRangeError("year and eraYear do not agree")
		}
		return eraArithmeticYear, nil
	}
	if !yearPresent {
		return 0, r.throwTypeError("year or era and eraYear are required")
	}
	return year, nil
}

func (r *Runtime) resolveTemporalMonthDayDate(calendar string, year, month int,
	leap bool, day int, constrain bool) (temporalPlainDate, error) {
	fields := temporalPartialDateFields{
		day: day, dayPresent: true,
		monthCode: fmt.Sprintf("M%02d", month), monthCodePresent: true,
	}
	if leap {
		fields.monthCode += "L"
	}
	return r.resolveTemporalMonthDayFieldsDate(calendar, year, fields, constrain)
}

func (r *Runtime) temporalYearMonthFields(o *Object, calendar string) (temporalPartialDateFields, error) {
	var fields temporalPartialDateFields
	var err error
	if temporalCalendarUsesEra(calendar) {
		eraValue, getErr := r.getProp(o, r.atoms.intern("era"), Obj(o))
		if getErr != nil {
			return fields, getErr
		}
		fields.eraPresent = !eraValue.IsUndefined()
		if fields.eraPresent {
			era, conversionErr := r.toString(eraValue)
			if conversionErr != nil {
				return fields, conversionErr
			}
			fields.era = asciiLower(era.Go())
			if _, valid := temporalYearFromEra(calendar, fields.era, 1); !valid {
				return fields, r.throwRangeError("invalid calendar era")
			}
		}
		eraYearValue, getErr := r.getProp(o, r.atoms.intern("eraYear"), Obj(o))
		if getErr != nil {
			return fields, getErr
		}
		fields.eraYearPresent = !eraYearValue.IsUndefined()
		if fields.eraYearPresent {
			fields.eraYear, err = r.temporalTruncatedInteger(eraYearValue, "eraYear")
			if err != nil {
				return fields, err
			}
		}
	}
	monthValue, err := r.getProp(o, r.atoms.intern("month"), Obj(o))
	if err != nil {
		return fields, err
	}
	fields.monthPresent = !monthValue.IsUndefined()
	if fields.monthPresent {
		fields.month, err = r.temporalTruncatedInteger(monthValue, "month")
		if err != nil {
			return fields, err
		}
	}
	monthCodeValue, err := r.getProp(o, r.atoms.intern("monthCode"), Obj(o))
	if err != nil {
		return fields, err
	}
	fields.monthCodePresent = !monthCodeValue.IsUndefined()
	if fields.monthCodePresent {
		fields.monthCode, err = r.temporalMonthCodeString(monthCodeValue)
		if err != nil {
			return fields, err
		}
	}
	yearValue, err := r.getProp(o, r.atoms.intern("year"), Obj(o))
	if err != nil {
		return fields, err
	}
	fields.yearPresent = !yearValue.IsUndefined()
	if fields.yearPresent {
		fields.year, err = r.temporalTruncatedInteger(yearValue, "year")
	}
	return fields, err
}

func (r *Runtime) resolveTemporalYearMonthFields(calendar string,
	fields temporalPartialDateFields, overflow string) (temporalPlainDate, error) {
	year := fields.year
	if fields.eraPresent {
		year, _ = temporalYearFromEra(calendar, fields.era, fields.eraYear)
	}
	month, leap, byCode := fields.month, false, false
	if fields.monthCodePresent {
		var ok bool
		month, leap, ok = parseTemporalMonthCode(fields.monthCode)
		if !ok || !temporalCalendarMonthCodeValid(calendar, month, leap) {
			return temporalPlainDate{}, r.throwRangeError("invalid monthCode")
		}
		byCode = true
	}
	if month < 1 {
		return temporalPlainDate{}, r.throwRangeError("invalid Temporal.PlainYearMonth")
	}
	constrain := overflow == "constrain"
	isoYear, isoMonth, isoDay, ok := icu.ResolveDate(calendar, year,
		month, 1, leap, byCode, constrain)
	if !ok && byCode && leap && constrain {
		if calendar == "hebrew" {
			month = 6
		}
		isoYear, isoMonth, isoDay, ok = icu.ResolveDate(calendar, year,
			month, 1, false, true, true)
	}
	if !ok {
		return temporalPlainDate{}, r.throwRangeError("invalid Temporal.PlainYearMonth")
	}
	result := temporalPlainDate{
		year: isoYear, month: isoMonth, day: isoDay, calendar: calendar,
	}
	if fields.monthPresent && fields.monthCodePresent &&
		result.calendarDate().OrdinalMonth != fields.month {
		return temporalPlainDate{}, r.throwRangeError("month and monthCode do not agree")
	}
	if !(temporalPlainYearMonthFromDate(result).valid()) {
		return temporalPlainDate{}, r.throwRangeError("invalid Temporal.PlainYearMonth")
	}
	return result, nil
}

func temporalCalendarMonthCodeValid(calendar string, month int, leap bool) bool {
	if leap {
		return calendar == "hebrew" && month == 5 ||
			(calendar == "chinese" || calendar == "dangi") && month <= 12
	}
	if calendar == "coptic" || calendar == "ethiopic" || calendar == "ethioaa" {
		return month <= 13
	}
	return month <= 12
}

func (r *Runtime) replaceTemporalPlainYearMonthFields(yearMonth temporalPlainYearMonth,
	partial temporalPartialDateFields, overflow string) (temporalPlainDate, error) {
	calendarDate := yearMonth.date().calendarDate()
	fields := temporalPartialDateFields{
		year: calendarDate.ArithmeticYear, yearPresent: true,
	}
	fields.monthCode = temporalCalendarMonthCode(calendarDate)
	fields.monthCodePresent = true
	if partial.eraPresent {
		fields.era, fields.eraPresent = partial.era, true
		fields.eraYear, fields.eraYearPresent = partial.eraYear, true
		fields.yearPresent = false
	} else if partial.yearPresent {
		fields.year, fields.yearPresent = partial.year, true
	}
	if partial.monthPresent {
		fields.month, fields.monthPresent = partial.month, true
		fields.monthCodePresent = false
	}
	if partial.monthCodePresent {
		fields.monthCode, fields.monthCodePresent = partial.monthCode, true
		fields.monthPresent = partial.monthPresent
	}
	return r.resolveTemporalYearMonthFields(yearMonth.calendar, fields, overflow)
}

func (r *Runtime) temporalMonthAndYearFields(o *Object) (month int, monthPresent bool, monthCode string, monthCodePresent bool, year int, yearPresent bool, err error) {
	monthValue, err := r.getProp(o, r.atoms.intern("month"), Obj(o))
	if err != nil {
		return 0, false, "", false, 0, false, err
	}
	monthPresent = !monthValue.IsUndefined()
	if monthPresent {
		month, err = r.temporalTruncatedInteger(monthValue, "month")
		if err != nil {
			return 0, false, "", false, 0, false, err
		}
	}
	monthCodeValue, err := r.getProp(o, r.atoms.intern("monthCode"), Obj(o))
	if err != nil {
		return 0, false, "", false, 0, false, err
	}
	monthCodePresent = !monthCodeValue.IsUndefined()
	if monthCodePresent {
		monthCode, err = r.temporalMonthCodeString(monthCodeValue)
		if err != nil {
			return 0, false, "", false, 0, false, err
		}
	}
	yearValue, err := r.getProp(o, r.atoms.intern("year"), Obj(o))
	if err != nil {
		return 0, false, "", false, 0, false, err
	}
	yearPresent = !yearValue.IsUndefined()
	if yearPresent {
		year, err = r.temporalTruncatedInteger(yearValue, "year")
	}
	return
}

func (r *Runtime) temporalMonthCodeString(value Value) (string, error) {
	if !value.IsString() {
		if !value.IsObject() {
			return "", r.throwTypeError("monthCode must be a string")
		}
		primitive, err := r.toPrimitive(value, hintString)
		if err != nil {
			return "", err
		}
		if !primitive.IsString() {
			return "", r.throwTypeError("monthCode must resolve to a string")
		}
		value = primitive
	}
	monthCode := value.String().Go()
	if !wellFormedTemporalMonthCode(monthCode) {
		return "", r.throwRangeError("invalid monthCode")
	}
	return monthCode, nil
}

func (r *Runtime) resolveTemporalISOMonth(month int, monthPresent bool, monthCode string, monthCodePresent bool) (int, error) {
	if !monthCodePresent {
		return month, nil
	}
	parsed, ok := parseISOMonthCode(monthCode)
	if !ok || monthPresent && month != parsed {
		return 0, r.throwRangeError("invalid monthCode")
	}
	return parsed, nil
}

func parseTemporalPlainYearMonth(input string) (temporalPlainYearMonth, error) {
	if date, err := parseTemporalPlainDate(input); err == nil {
		result := temporalPlainYearMonth{year: date.year, month: date.month, day: date.day, calendar: date.calendar}
		if result.valid() {
			return result, nil
		}
	}
	main, calendar, ok := parseTemporalPartialAnnotations(input)
	if !ok {
		return temporalPlainYearMonth{}, errInvalidTemporalInstant
	}
	year, month, day, hasDay, ok := parseTemporalPartialDateWithTime(main, true)
	if !ok || calendar != "iso8601" && !hasDay {
		return temporalPlainYearMonth{}, errInvalidTemporalInstant
	}
	if !hasDay {
		day = 1
	}
	result := temporalPlainYearMonth{year: year, month: month, day: day, calendar: calendar}
	if !result.valid() {
		return temporalPlainYearMonth{}, errInvalidTemporalInstant
	}
	return result, nil
}

func parseTemporalPlainMonthDay(input string) (temporalPlainMonthDay, error) {
	if date, err := parseTemporalPlainDate(input); err == nil {
		return temporalPlainMonthDay{year: date.year, month: date.month, day: date.day, calendar: date.calendar}, nil
	}
	main, calendar, ok := parseTemporalPartialAnnotations(input)
	if !ok {
		return temporalPlainMonthDay{}, errInvalidTemporalInstant
	}
	if strings.HasPrefix(main, "--") {
		main = main[2:]
	}
	if month, day, ok := parseBareTemporalMonthDay(main); ok {
		if calendar != "iso8601" {
			return temporalPlainMonthDay{}, errInvalidTemporalInstant
		}
		return temporalPlainMonthDay{year: 1972, month: month, day: day, calendar: calendar}, nil
	}
	year, month, day, hasDay, ok := parseTemporalPartialDateWithTime(main, false)
	if !ok || !hasDay || month < 1 || month > 12 || day < 1 || day > isoDaysInMonth(year, month) {
		return temporalPlainMonthDay{}, errInvalidTemporalInstant
	}
	if calendar != "iso8601" && !(temporalPlainDate{year: year, month: month, day: day}).valid() {
		return temporalPlainMonthDay{}, errInvalidTemporalInstant
	}
	return temporalPlainMonthDay{year: year, month: month, day: day, calendar: calendar}, nil
}

func parseTemporalPartialAnnotations(input string) (string, string, bool) {
	main, annotations, ok := splitTemporalAnnotations(input)
	if !ok {
		return "", "", false
	}
	calendar := "iso8601"
	calendarSeen, criticalCalendar, timeZoneSeen := false, false, false
	for _, annotation := range annotations {
		critical := strings.HasPrefix(annotation, "!")
		annotation = strings.TrimPrefix(annotation, "!")
		key, value, keyed := strings.Cut(annotation, "=")
		if !keyed {
			if timeZoneSeen || !validTimeZoneAnnotation(annotation) {
				return "", "", false
			}
			timeZoneSeen = true
			continue
		}
		if key == "u-ca" {
			if calendarSeen && (criticalCalendar || critical) {
				return "", "", false
			}
			if calendarSeen {
				continue
			}
			calendarSeen = true
			criticalCalendar = critical
			calendar = canonicalSetting("ca", asciiLower(value))
			if !icu.HasCalendar(calendar) {
				return "", "", false
			}
			continue
		}
		if critical || !validAnnotationKey(key) || value == "" {
			return "", "", false
		}
	}
	return main, calendar, true
}

func parseTemporalPartialDate(main string, allowYearMonth bool) (year, month, day int, hasDay, ok bool) {
	index := 0
	year, ok = parseTemporalYear(main, &index)
	if !ok {
		return 0, 0, 0, false, false
	}
	dashed := consumeByte(main, &index, '-')
	month, ok = parseFixedDigits(main, &index, 2)
	if !ok {
		return 0, 0, 0, false, false
	}
	if index == len(main) {
		return year, month, 0, false, allowYearMonth && month >= 1 && month <= 12
	}
	if dashed && !consumeByte(main, &index, '-') {
		return 0, 0, 0, false, false
	}
	day, ok = parseFixedDigits(main, &index, 2)
	if !ok || index != len(main) || month < 1 || month > 12 || day < 1 || day > isoDaysInMonth(year, month) {
		return 0, 0, 0, false, false
	}
	return year, month, day, true, true
}

func parseTemporalPartialDateWithTime(main string, allowYearMonth bool) (year, month, day int, hasDay, ok bool) {
	separator := strings.IndexAny(main, "Tt ")
	if separator < 0 {
		return parseTemporalPartialDate(main, allowYearMonth)
	}
	year, month, day, hasDay, ok = parseTemporalPartialDate(main[:separator], false)
	if !ok || !hasDay {
		return 0, 0, 0, false, false
	}
	yearEnd := 0
	if _, ok = parseTemporalYear(main, &yearEnd); !ok {
		return 0, 0, 0, false, false
	}
	// Validate the time and offset grammar using an in-range leap year. The
	// original date fields were validated above and remain the result fields.
	if _, err := parseTemporalPlainDate("2000" + main[yearEnd:]); err != nil {
		return 0, 0, 0, false, false
	}
	return year, month, day, true, true
}

func parseBareTemporalMonthDay(main string) (month, day int, ok bool) {
	index := 0
	month, ok = parseFixedDigits(main, &index, 2)
	if !ok {
		return 0, 0, false
	}
	dashed := consumeByte(main, &index, '-')
	day, ok = parseFixedDigits(main, &index, 2)
	return month, day, ok && index == len(main) && (dashed || len(main) == 4) &&
		month >= 1 && month <= 12 && day >= 1 && day <= isoDaysInMonth(1972, month)
}
