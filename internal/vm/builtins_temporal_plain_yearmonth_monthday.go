package vm

import (
	"math"
	"math/big"
	"strings"
	"time"

	"github.com/go-quickjs/go-quickjs/internal/icu"
)

type temporalPlainYearMonth struct {
	year, month, day int
	calendar         string
}

func (d temporalPlainYearMonth) valid() bool {
	if d.month < 1 || d.month > 12 || d.day < 1 || d.day > isoDaysInMonth(d.year, d.month) {
		return false
	}
	return (d.year > -271821 || d.year == -271821 && d.month >= 4) &&
		(d.year < 275760 || d.year == 275760 && d.month <= 9)
}

type temporalPlainMonthDay struct {
	year, month, day int
	calendar         string
}

func (d temporalPlainMonthDay) valid() bool {
	return (temporalPlainDate{year: d.year, month: d.month, day: d.day}).valid()
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
		return Int(compareISODate(left.year, left.month, left.day, right.year, right.month, right.day)), nil
	})

	for _, property := range []string{"year", "month", "monthCode", "calendarId", "era", "eraYear"} {
		name := property
		r.defGetter(proto, name, func(rt *Runtime, this Value, args []Value) (Value, error) {
			yearMonth, err := rt.temporalPlainYearMonthValue(this, "get Temporal.PlainYearMonth.prototype."+name)
			if err != nil {
				return Undefined, err
			}
			date := temporalPlainDate{year: yearMonth.year, month: yearMonth.month, day: yearMonth.day, calendar: yearMonth.calendar}.calendarDate()
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
			switch name {
			case "daysInMonth":
				return Int(isoDaysInMonth(yearMonth.year, yearMonth.month)), nil
			case "daysInYear":
				if isLeapYear(yearMonth.year) {
					return Int(366), nil
				}
				return Int(365), nil
			case "monthsInYear":
				return Int(12), nil
			case "inLeapYear":
				return Bool(isLeapYear(yearMonth.year)), nil
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
		month, monthPresent, monthCode, monthCodePresent, year, yearPresent, err := rt.temporalMonthAndYearFields(fields)
		if err != nil {
			return Undefined, err
		}
		if !monthPresent && !monthCodePresent && !yearPresent {
			return Undefined, rt.throwTypeError("year-month fields must not be empty")
		}
		if monthPresent && month < 1 {
			return Undefined, rt.throwRangeError("invalid Temporal.PlainYearMonth")
		}
		overflow, err := rt.temporalOverflowOption(arg(args, 1))
		if err != nil {
			return Undefined, err
		}
		if !yearPresent {
			year = yearMonth.year
		}
		if monthPresent || monthCodePresent {
			month, err = rt.resolveTemporalISOMonth(month, monthPresent, monthCode, monthCodePresent)
			if err != nil {
				return Undefined, err
			}
		} else {
			month = yearMonth.month
		}
		if overflow == "constrain" {
			month = min(month, 12)
		}
		result := temporalPlainYearMonth{year: year, month: month, day: 1, calendar: yearMonth.calendar}
		if !result.valid() {
			return Undefined, rt.throwRangeError("invalid Temporal.PlainYearMonth")
		}
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
			if _, err := rt.temporalOverflowOption(arg(args, 1)); err != nil {
				return Undefined, err
			}
			if duration.weeks != 0 || duration.days != 0 || duration.hours != 0 || duration.minutes != 0 ||
				duration.seconds != 0 || duration.milliseconds != 0 || duration.microseconds != 0 || duration.nanoseconds != 0 {
				return Undefined, rt.throwRangeError("units below months cannot be added to a year-month")
			}
			if !(temporalPlainDate{year: yearMonth.year, month: yearMonth.month, day: yearMonth.day}).valid() {
				return Undefined, rt.throwRangeError("year-month reference date is outside the Temporal date range")
			}
			delta := int64(duration.years)*12 + int64(duration.months)
			months := int64(yearMonth.year)*12 + int64(yearMonth.month-1) + op.sign*delta
			year := floorDivInt64(months, 12)
			month := months - year*12 + 1
			if year < -271821 || year > 275760 {
				return Undefined, rt.throwRangeError("year-month is outside the Temporal range")
			}
			result := temporalPlainYearMonth{year: int(year), month: int(month), day: 1, calendar: yearMonth.calendar}
			if !result.valid() {
				return Undefined, rt.throwRangeError("year-month is outside the Temporal range")
			}
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
			months := int64(other.year-yearMonth.year)*12 + int64(other.month-yearMonth.month)
			if op.since {
				months = -months
			}
			if months == 0 {
				return Obj(newTemporalDuration(rt.temporalDurationProto, temporalDuration{})), nil
			}
			if !(temporalPlainDate{year: yearMonth.year, month: yearMonth.month, day: 1}).valid() {
				return Undefined, rt.throwRangeError("year-month reference date is outside the Temporal date range")
			}
			if !(temporalPlainDate{year: other.year, month: other.month, day: 1}).valid() {
				return Undefined, rt.throwRangeError("other year-month reference date is outside the Temporal date range")
			}
			step := increment
			rounded := months
			if smallest == "year" || largest == "month" {
				if smallest == "year" {
					step *= 12
				}
				rounded = roundTemporalBigInt(big.NewInt(months), big.NewInt(step), mode).Int64()
			} else {
				years, remainder := months/12, months%12
				remainder = roundTemporalBigInt(big.NewInt(remainder), big.NewInt(step), mode).Int64()
				rounded = years*12 + remainder
			}
			if increment != 1 {
				start := int64(yearMonth.year)*12 + int64(yearMonth.month-1)
				lower := floorDivInt64(months, step) * step
				for _, candidate := range []int64{start + lower, start + lower + step} {
					year := floorDivInt64(candidate, 12)
					month := candidate - year*12 + 1
					if !((temporalPlainYearMonth{year: int(year), month: int(month), day: 1}).valid()) {
						return Undefined, rt.throwRangeError("rounded year-month is outside the Temporal range")
					}
				}
			}
			var duration temporalDuration
			if largest == "month" {
				duration.months = float64(rounded)
			} else {
				duration.years = float64(rounded / 12)
				duration.months = float64(rounded % 12)
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
		day = min(day, isoDaysInMonth(yearMonth.year, yearMonth.month))
		date := temporalPlainDate{year: yearMonth.year, month: yearMonth.month, day: day, calendar: yearMonth.calendar}
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
		yearMonth, err := rt.temporalPlainYearMonthValue(this, "Temporal.PlainYearMonth.prototype.toLocaleString")
		if err != nil {
			return Undefined, err
		}
		options, err := rt.dateOptionsFrom(args, map[string]string{"year": "numeric", "month": "numeric"}, "date")
		if err != nil {
			return Undefined, err
		}
		date := time.Date(yearMonth.year, time.Month(yearMonth.month), yearMonth.day, 12, 0, 0, 0, time.UTC)
		return Str(NewString(options.format(date))), nil
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
		dayValue, err := rt.getProp(fields, rt.atoms.intern("day"), Obj(fields))
		if err != nil {
			return Undefined, err
		}
		day, dayPresent := 0, !dayValue.IsUndefined()
		if dayPresent {
			day, err = rt.temporalTruncatedInteger(dayValue, "day")
			if err != nil {
				return Undefined, err
			}
		}
		month, monthPresent, monthCode, monthCodePresent, year, yearPresent, err := rt.temporalMonthAndYearFields(fields)
		if err != nil {
			return Undefined, err
		}
		if !dayPresent && !monthPresent && !monthCodePresent && !yearPresent {
			return Undefined, rt.throwTypeError("month-day fields must not be empty")
		}
		if dayPresent && day < 1 || monthPresent && month < 1 {
			return Undefined, rt.throwRangeError("invalid Temporal.PlainMonthDay")
		}
		overflow, err := rt.temporalOverflowOption(arg(args, 1))
		if err != nil {
			return Undefined, err
		}
		if !dayPresent {
			day = monthDay.day
		}
		if monthPresent || monthCodePresent {
			month, err = rt.resolveTemporalISOMonth(month, monthPresent, monthCode, monthCodePresent)
			if err != nil {
				return Undefined, err
			}
		} else {
			month = monthDay.month
		}
		validationYear := 1972
		if yearPresent {
			validationYear = year
		}
		if overflow == "constrain" {
			month = min(month, 12)
			day = min(day, isoDaysInMonth(validationYear, month))
		} else if month > 12 || day > isoDaysInMonth(validationYear, month) {
			return Undefined, rt.throwRangeError("invalid Temporal.PlainMonthDay")
		}
		result := temporalPlainMonthDay{year: 1972, month: month, day: day, calendar: monthDay.calendar}
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
		yearValue, err := rt.getProp(fields.Object(), rt.atoms.intern("year"), fields)
		if err != nil {
			return Undefined, err
		}
		if yearValue.IsUndefined() {
			return Undefined, rt.throwTypeError("year is required")
		}
		year, err := rt.temporalTruncatedInteger(yearValue, "year")
		if err != nil {
			return Undefined, err
		}
		day := min(monthDay.day, isoDaysInMonth(year, monthDay.month))
		date := temporalPlainDate{year: year, month: monthDay.month, day: day, calendar: monthDay.calendar}
		if !date.valid() {
			return Undefined, rt.throwRangeError("invalid Temporal.PlainDate")
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
		monthDay, err := rt.temporalPlainMonthDayValue(this, "Temporal.PlainMonthDay.prototype.toLocaleString")
		if err != nil {
			return Undefined, err
		}
		options, err := rt.dateOptionsFrom(args, map[string]string{"month": "numeric", "day": "numeric"}, "date")
		if err != nil {
			return Undefined, err
		}
		date := time.Date(monthDay.year, time.Month(monthDay.month), monthDay.day, 12, 0, 0, 0, time.UTC)
		return Str(NewString(options.format(date))), nil
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
	var b strings.Builder
	writeTemporalISOYear(&b, d.year)
	b.WriteByte('-')
	writePaddedTemporalInt(&b, d.month, 2)
	if showAnnotation {
		b.WriteByte('-')
		writePaddedTemporalInt(&b, d.day, 2)
		writeTemporalCalendarAnnotation(&b, d.calendar, showCalendar)
	}
	return b.String()
}

func (d temporalPlainMonthDay) string(showCalendar string) string {
	showAnnotation := showCalendar == "always" || showCalendar == "critical" || showCalendar == "auto" && d.calendar != "iso8601"
	var b strings.Builder
	if showAnnotation {
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
			return temporalPlainYearMonth{year: date.year, month: date.month, day: 1, calendar: date.calendar}, nil
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
	result.day = 1
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
	month, monthPresent, monthCode, monthCodePresent, year, yearPresent, err := r.temporalMonthAndYearFields(o)
	if err != nil {
		return temporalPlainYearMonth{}, err
	}
	overflow, err := r.temporalOverflowOption(optionsValue)
	if err != nil {
		return temporalPlainYearMonth{}, err
	}
	if !yearPresent || !monthPresent && !monthCodePresent {
		return temporalPlainYearMonth{}, r.throwTypeError("plain year-month property bag is missing required fields")
	}
	month, err = r.resolveTemporalISOMonth(month, monthPresent, monthCode, monthCodePresent)
	if err != nil {
		return temporalPlainYearMonth{}, err
	}
	if overflow == "constrain" {
		if month < 1 {
			return temporalPlainYearMonth{}, r.throwRangeError("invalid Temporal.PlainYearMonth")
		}
		month = min(month, 12)
	}
	result := temporalPlainYearMonth{year: year, month: month, day: 1, calendar: calendar}
	if !result.valid() {
		return temporalPlainYearMonth{}, r.throwRangeError("invalid Temporal.PlainYearMonth")
	}
	return result, nil
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
			if _, err := r.temporalOverflowOption(optionsValue); err != nil {
				return temporalPlainMonthDay{}, err
			}
			return temporalPlainMonthDay{year: 1972, month: date.month, day: date.day, calendar: date.calendar}, nil
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
	result.year = 1972
	return result, nil
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
	dayValue, err := r.getProp(o, r.atoms.intern("day"), Obj(o))
	if err != nil {
		return temporalPlainMonthDay{}, err
	}
	day, dayPresent := 0, !dayValue.IsUndefined()
	if dayPresent {
		day, err = r.temporalTruncatedInteger(dayValue, "day")
		if err != nil {
			return temporalPlainMonthDay{}, err
		}
	}
	month, monthPresent, monthCode, monthCodePresent, year, yearPresent, err := r.temporalMonthAndYearFields(o)
	if err != nil {
		return temporalPlainMonthDay{}, err
	}
	overflow, err := r.temporalOverflowOption(optionsValue)
	if err != nil {
		return temporalPlainMonthDay{}, err
	}
	if !dayPresent || !monthPresent && !monthCodePresent {
		return temporalPlainMonthDay{}, r.throwTypeError("plain month-day property bag is missing required fields")
	}
	month, err = r.resolveTemporalISOMonth(month, monthPresent, monthCode, monthCodePresent)
	if err != nil {
		return temporalPlainMonthDay{}, err
	}
	validationYear := 1972
	if yearPresent {
		validationYear = year
	}
	if overflow == "constrain" {
		if month < 1 || day < 1 {
			return temporalPlainMonthDay{}, r.throwRangeError("invalid Temporal.PlainMonthDay")
		}
		month = min(month, 12)
		day = min(day, isoDaysInMonth(validationYear, month))
	} else if month < 1 || month > 12 || day < 1 || day > isoDaysInMonth(validationYear, month) {
		return temporalPlainMonthDay{}, r.throwRangeError("invalid Temporal.PlainMonthDay")
	}
	result := temporalPlainMonthDay{year: 1972, month: month, day: day, calendar: calendar}
	if !result.valid() {
		return temporalPlainMonthDay{}, r.throwRangeError("invalid Temporal.PlainMonthDay")
	}
	return result, nil
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
