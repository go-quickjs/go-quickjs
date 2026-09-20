package vm

import (
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/go-quickjs/go-quickjs/internal/icu"
)

type temporalPlainDate struct {
	year, month, day int
	calendar         string
}

func (d temporalPlainDate) valid() bool {
	if d.month < 1 || d.month > 12 || d.day < 1 || d.day > isoDaysInMonth(d.year, d.month) {
		return false
	}
	days := isoDaysFromCivil(int64(d.year), d.month, d.day)
	return days >= -100_000_001 && days <= 100_000_000
}

func (r *Runtime) initTemporalPlainDate(temporal *Object) {
	proto := newObject(r.proto.object, ClassObject)
	r.temporalPlainDateProto = proto
	ctor := r.newTemporalCtor(temporal, "PlainDate", 3, proto, func(rt *Runtime, this Value, args []Value) (Value, error) {
		if err := rt.requireNew("Temporal.PlainDate"); err != nil {
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
		day, err := rt.temporalTruncatedInteger(arg(args, 2), "day")
		if err != nil {
			return Undefined, err
		}
		calendar, err := rt.toTemporalCalendarIdentifier(arg(args, 3))
		if err != nil {
			return Undefined, err
		}
		date := temporalPlainDate{year: year, month: month, day: day, calendar: calendar}
		if !date.valid() {
			return Undefined, rt.throwRangeError("invalid Temporal.PlainDate")
		}
		instanceProto, err := rt.protoFromNewTargetErr(proto)
		if err != nil {
			return Undefined, err
		}
		return Obj(newTemporalPlainDate(instanceProto, date)), nil
	})

	r.defMethod(ctor, "from", 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
		date, err := rt.toTemporalPlainDate(arg(args, 0), arg(args, 1))
		if err != nil {
			return Undefined, err
		}
		return Obj(newTemporalPlainDate(proto, date)), nil
	})
	r.defMethod(ctor, "compare", 2, func(rt *Runtime, this Value, args []Value) (Value, error) {
		left, err := rt.toTemporalPlainDate(arg(args, 0), Undefined)
		if err != nil {
			return Undefined, err
		}
		right, err := rt.toTemporalPlainDate(arg(args, 1), Undefined)
		if err != nil {
			return Undefined, err
		}
		leftDays := isoDaysFromCivil(int64(left.year), left.month, left.day)
		rightDays := isoDaysFromCivil(int64(right.year), right.month, right.day)
		switch {
		case leftDays < rightDays:
			return Int(-1), nil
		case leftDays > rightDays:
			return Int(1), nil
		default:
			return Int(0), nil
		}
	})

	for _, property := range []string{"year", "month", "monthCode", "day", "calendarId", "era", "eraYear"} {
		name := property
		r.defGetter(proto, name, func(rt *Runtime, this Value, args []Value) (Value, error) {
			date, err := rt.temporalPlainDateValue(this, "get Temporal.PlainDate.prototype."+name)
			if err != nil {
				return Undefined, err
			}
			calendarDate := date.calendarDate()
			switch name {
			case "year":
				return Int(calendarDate.ArithmeticYear), nil
			case "month":
				return Int(calendarDate.OrdinalMonth), nil
			case "monthCode":
				return Str(NewString(temporalCalendarMonthCode(calendarDate))), nil
			case "day":
				return Int(calendarDate.Day), nil
			case "calendarId":
				return Str(NewString(date.calendar)), nil
			case "era":
				era, ok := temporalCalendarEra(date.calendar, calendarDate)
				if !ok {
					return Undefined, nil
				}
				return Str(NewString(era)), nil
			case "eraYear":
				eraYear, ok := temporalCalendarEraYear(date.calendar, calendarDate)
				if !ok {
					return Undefined, nil
				}
				return Int(eraYear), nil
			}
			return Undefined, nil
		})
	}

	for _, property := range []string{"dayOfWeek", "dayOfYear", "weekOfYear", "yearOfWeek", "daysInWeek", "daysInMonth", "daysInYear", "monthsInYear", "inLeapYear"} {
		name := property
		r.defGetter(proto, name, func(rt *Runtime, this Value, args []Value) (Value, error) {
			date, err := rt.temporalPlainDateValue(this, "get Temporal.PlainDate.prototype."+name)
			if err != nil {
				return Undefined, err
			}
			days := isoDaysFromCivil(int64(date.year), date.month, date.day)
			calendarDate := date.calendarDate()
			dayOfYear, daysInMonth, daysInYear, monthsInYear, inLeapYear, _ :=
				icu.DateInfo(date.calendar, calendarDate)
			switch name {
			case "dayOfWeek":
				return Int(isoDayOfWeek(days)), nil
			case "dayOfYear":
				return Int(dayOfYear), nil
			case "weekOfYear":
				if date.calendar != "iso8601" {
					return Undefined, nil
				}
				week, _ := isoWeekOfYear(days)
				return Int(week), nil
			case "yearOfWeek":
				if date.calendar != "iso8601" {
					return Undefined, nil
				}
				_, year := isoWeekOfYear(days)
				return Int(year), nil
			case "daysInWeek":
				return Int(7), nil
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
		date, err := rt.temporalPlainDateValue(this, "Temporal.PlainDate.prototype.equals")
		if err != nil {
			return Undefined, err
		}
		other, err := rt.toTemporalPlainDate(arg(args, 0), Undefined)
		if err != nil {
			return Undefined, err
		}
		return Bool(date.year == other.year && date.month == other.month && date.day == other.day && date.calendar == other.calendar), nil
	})
	r.defMethod(proto, "toPlainDateTime", 0, func(rt *Runtime, this Value, args []Value) (Value, error) {
		date, err := rt.temporalPlainDateValue(this, "Temporal.PlainDate.prototype.toPlainDateTime")
		if err != nil {
			return Undefined, err
		}
		time := temporalPlainTime{}
		if !arg(args, 0).IsUndefined() {
			time, err = rt.toTemporalPlainTime(arg(args, 0), Undefined)
			if err != nil {
				return Undefined, err
			}
		}
		result := temporalPlainDateTime{temporalISODateTime: temporalISODateTime{
			year: date.year, month: date.month, day: date.day,
			hour: time.hour, minute: time.minute, second: time.second,
			millisecond: time.millisecond, microsecond: time.microsecond, nanosecond: time.nanosecond,
		}, calendar: date.calendar}
		if !result.valid() {
			return Undefined, rt.throwRangeError("combined date and time are outside the Temporal range")
		}
		return Obj(newTemporalPlainDateTime(rt.temporalPlainDateTimeProto, result)), nil
	})
	r.defMethod(proto, "toZonedDateTime", 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
		date, err := rt.temporalPlainDateValue(this, "Temporal.PlainDate.prototype.toZonedDateTime")
		if err != nil {
			return Undefined, err
		}
		item := arg(args, 0)
		timeZoneValue, plainTimeValue := item, Undefined
		if item.IsObject() {
			timeZoneValue, err = rt.getProp(item.Object(), rt.atoms.intern("timeZone"), item)
			if err != nil {
				return Undefined, err
			}
			if timeZoneValue.IsUndefined() {
				return Undefined, rt.throwTypeError("timeZone is required")
			}
		}
		timeZone, err := rt.toTemporalTimeZoneIdentifier(timeZoneValue)
		if err != nil {
			return Undefined, err
		}
		if item.IsObject() {
			plainTimeValue, err = rt.getProp(item.Object(), rt.atoms.intern("plainTime"), item)
			if err != nil {
				return Undefined, err
			}
		}

		timeOmitted := plainTimeValue.IsUndefined()
		time := temporalPlainTime{}
		if !timeOmitted {
			time, err = rt.toTemporalPlainTime(plainTimeValue, Undefined)
			if err != nil {
				return Undefined, err
			}
		}
		dateTime := temporalPlainDateTime{temporalISODateTime: temporalISODateTime{
			year: date.year, month: date.month, day: date.day,
			hour: time.hour, minute: time.minute, second: time.second,
			millisecond: time.millisecond, microsecond: time.microsecond, nanosecond: time.nanosecond,
		}, calendar: date.calendar}
		if !dateTime.valid() {
			return Undefined, rt.throwRangeError("combined date and time are outside the Temporal range")
		}
		zoned, err := rt.newTemporalZonedDateTime(temporalInstant{}, timeZone, Str(NewString(date.calendar)))
		if err != nil {
			return Undefined, err
		}
		instant, ok := temporalInstant{}, false
		if timeOmitted {
			instant, ok = zoned.startOfDayInstant(date)
		} else {
			instant, ok = zoned.compatibleInstant(dateTime.temporalISODateTime)
		}
		if !ok {
			return Undefined, rt.throwRangeError("zoned date-time is outside the Temporal range")
		}
		zoned.instant = instant
		o := newObject(rt.temporalZonedDateTimeProto, ClassObject)
		o.data = zoned
		return Obj(o), nil
	})
	r.defMethod(proto, "toPlainYearMonth", 0, func(rt *Runtime, this Value, args []Value) (Value, error) {
		date, err := rt.temporalPlainDateValue(this, "Temporal.PlainDate.prototype.toPlainYearMonth")
		if err != nil {
			return Undefined, err
		}
		result, err := rt.temporalYearMonthFromDate(date)
		if err != nil {
			return Undefined, err
		}
		return Obj(newTemporalPlainYearMonth(rt.temporalPlainYearMonthProto, result)), nil
	})
	r.defMethod(proto, "toPlainMonthDay", 0, func(rt *Runtime, this Value, args []Value) (Value, error) {
		date, err := rt.temporalPlainDateValue(this, "Temporal.PlainDate.prototype.toPlainMonthDay")
		if err != nil {
			return Undefined, err
		}
		result := temporalPlainMonthDay{year: 1972, month: date.month, day: date.day, calendar: date.calendar}
		return Obj(newTemporalPlainMonthDay(rt.temporalPlainMonthDayProto, result)), nil
	})
	r.defMethod(proto, "toString", 0, func(rt *Runtime, this Value, args []Value) (Value, error) {
		date, err := rt.temporalPlainDateValue(this, "Temporal.PlainDate.prototype.toString")
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
		return Str(NewString(date.string(show))), nil
	})
	r.defMethod(proto, "toJSON", 0, func(rt *Runtime, this Value, args []Value) (Value, error) {
		date, err := rt.temporalPlainDateValue(this, "Temporal.PlainDate.prototype.toJSON")
		if err != nil {
			return Undefined, err
		}
		return Str(NewString(date.string("auto"))), nil
	})
	r.defMethod(proto, "toLocaleString", 0, func(rt *Runtime, this Value, args []Value) (Value, error) {
		date, err := rt.temporalPlainDateValue(this, "Temporal.PlainDate.prototype.toLocaleString")
		if err != nil {
			return Undefined, err
		}
		options, err := rt.dateOptionsFrom(args, map[string]string{
			"year": "numeric", "month": "numeric", "day": "numeric",
		}, "date")
		if err != nil {
			return Undefined, err
		}
		if date.calendar != "iso8601" && date.calendar != options.calendar {
			return Undefined, rt.throwRangeError("Temporal calendar does not match the formatter calendar")
		}
		// A PlainDate supplies wall-clock fields, not an instant. Its requested
		// formatting time zone therefore cannot shift or skip the date.
		localDate := time.Date(date.year, time.Month(date.month), date.day,
			12, 0, 0, 0, time.UTC)
		return Str(NewString(options.format(localDate))), nil
	})
	r.defMethod(proto, "valueOf", 0, func(rt *Runtime, this Value, args []Value) (Value, error) {
		if _, err := rt.temporalPlainDateValue(this, "Temporal.PlainDate.prototype.valueOf"); err != nil {
			return Undefined, err
		}
		return Undefined, rt.throwTypeError("use Temporal.PlainDate.compare() or equals() to compare dates")
	})
	r.defToStringTag(proto, "Temporal.PlainDate")
	r.initTemporalPlainDateOperations(proto)
}

func temporalCalendarMonthCode(date icu.Date) string {
	month, leap := date.MonthCode()
	code := fmt.Sprintf("M%02d", month)
	if leap {
		code += "L"
	}
	return code
}

func newTemporalPlainDate(proto *Object, date temporalPlainDate) *Object {
	o := newObject(proto, ClassObject)
	o.data = &date
	return o
}

func (r *Runtime) temporalPlainDateValue(value Value, method string) (temporalPlainDate, error) {
	if value.IsObject() {
		if date, ok := value.Object().data.(*temporalPlainDate); ok && date != nil {
			return *date, nil
		}
	}
	return temporalPlainDate{}, r.throwTypeError("%s called on an incompatible receiver", method)
}

func (r *Runtime) temporalTruncatedInteger(value Value, field string) (int, error) {
	number, err := r.toNumber(value)
	if err != nil {
		return 0, err
	}
	if math.IsNaN(number) || math.IsInf(number, 0) {
		return 0, r.throwRangeError("%s must be a finite number", field)
	}
	number = math.Trunc(number)
	if number < math.MinInt32 || number > math.MaxInt32 {
		return 0, r.throwRangeError("%s is outside the supported range", field)
	}
	return int(number), nil
}

func (r *Runtime) toTemporalCalendarIdentifier(value Value) (string, error) {
	if value.IsUndefined() {
		return "iso8601", nil
	}
	if !value.IsString() {
		return "", r.throwTypeError("calendar must be a string")
	}
	calendar := asciiLower(value.String().Go())
	calendar = canonicalSetting("ca", calendar)
	if !icu.HasCalendar(calendar) {
		return "", r.throwRangeError("invalid calendar identifier")
	}
	return calendar, nil
}

func (r *Runtime) toTemporalCalendarIdentifierFromBag(value Value) (string, error) {
	if value.IsObject() {
		if date, ok := value.Object().data.(*temporalPlainDate); ok && date != nil {
			return date.calendar, nil
		}
		if dateTime, ok := value.Object().data.(*temporalPlainDateTime); ok && dateTime != nil {
			return dateTime.calendar, nil
		}
		if monthDay, ok := value.Object().data.(*temporalPlainMonthDay); ok && monthDay != nil {
			return monthDay.calendar, nil
		}
		if yearMonth, ok := value.Object().data.(*temporalPlainYearMonth); ok && yearMonth != nil {
			return yearMonth.calendar, nil
		}
		if zoned, ok := value.Object().data.(*temporalZonedDateTime); ok && zoned != nil {
			return zoned.calendar, nil
		}
	}
	calendar, err := r.toTemporalCalendarIdentifier(value)
	if err == nil || value.IsUndefined() || !value.IsString() {
		return calendar, err
	}
	calendar, ok := temporalCalendarFromISOString(value.String().Go())
	if !ok {
		return "", err
	}
	return calendar, nil
}

func asciiLower(value string) string {
	var b strings.Builder
	b.Grow(len(value))
	for i := 0; i < len(value); i++ {
		c := value[i]
		if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		b.WriteByte(c)
	}
	return b.String()
}

func (r *Runtime) toTemporalPlainDate(value, optionsValue Value) (temporalPlainDate, error) {
	if value.IsObject() {
		if date, ok := value.Object().data.(*temporalPlainDate); ok && date != nil {
			if _, err := r.temporalOverflowOption(optionsValue); err != nil {
				return temporalPlainDate{}, err
			}
			return *date, nil
		}
		if dateTime, ok := value.Object().data.(*temporalPlainDateTime); ok && dateTime != nil {
			if _, err := r.temporalOverflowOption(optionsValue); err != nil {
				return temporalPlainDate{}, err
			}
			return temporalPlainDate{year: dateTime.year, month: dateTime.month, day: dateTime.day, calendar: dateTime.calendar}, nil
		}
		if zoned, ok := value.Object().data.(*temporalZonedDateTime); ok && zoned != nil {
			if _, err := r.temporalOverflowOption(optionsValue); err != nil {
				return temporalPlainDate{}, err
			}
			localSeconds := zoned.instant.epochSeconds + int64(zoned.offsetSeconds())
			days := floorDivInt64(localSeconds, temporalSecondsPerDay)
			year, month, day := isoCivilFromDays(days)
			return temporalPlainDate{year: year, month: month, day: day, calendar: zoned.calendar}, nil
		}
		return r.temporalPlainDateFromBag(value.Object(), optionsValue)
	}
	if !value.IsString() {
		return temporalPlainDate{}, r.throwTypeError("a plain date must be a string or object")
	}
	date, err := parseTemporalPlainDate(value.String().Go())
	if err != nil {
		return temporalPlainDate{}, r.throwRangeError("invalid Temporal.PlainDate string")
	}
	if _, err := r.temporalOverflowOption(optionsValue); err != nil {
		return temporalPlainDate{}, err
	}
	return date, nil
}

func (r *Runtime) temporalPlainDateFromBag(o *Object, optionsValue Value) (temporalPlainDate, error) {
	calendarValue, err := r.getProp(o, r.atoms.intern("calendar"), Obj(o))
	if err != nil {
		return temporalPlainDate{}, err
	}
	calendar, err := r.toTemporalCalendarIdentifierFromBag(calendarValue)
	if err != nil {
		return temporalPlainDate{}, err
	}
	dayValue, err := r.getProp(o, r.atoms.intern("day"), Obj(o))
	if err != nil {
		return temporalPlainDate{}, err
	}
	day, dayPresent := 0, !dayValue.IsUndefined()
	if dayPresent {
		day, err = r.temporalTruncatedInteger(dayValue, "day")
		if err != nil {
			return temporalPlainDate{}, err
		}
	}
	era, eraPresent, eraYear, eraYearPresent := "", false, 0, false
	if temporalCalendarUsesEra(calendar) {
		eraValue, getErr := r.getProp(o, r.atoms.intern("era"), Obj(o))
		if getErr != nil {
			return temporalPlainDate{}, getErr
		}
		if !eraValue.IsUndefined() {
			eraString, conversionErr := r.toString(eraValue)
			if conversionErr != nil {
				return temporalPlainDate{}, conversionErr
			}
			era, eraPresent = asciiLower(eraString.Go()), true
			if _, valid := temporalYearFromEra(calendar, era, 1); !valid {
				return temporalPlainDate{}, r.throwRangeError("invalid calendar era")
			}
		}
		eraYearValue, getErr := r.getProp(o, r.atoms.intern("eraYear"), Obj(o))
		if getErr != nil {
			return temporalPlainDate{}, getErr
		}
		if !eraYearValue.IsUndefined() {
			eraYearPresent = true
			eraYear, err = r.temporalTruncatedInteger(eraYearValue, "eraYear")
			if err != nil {
				return temporalPlainDate{}, err
			}
		}
	}
	monthValue, err := r.getProp(o, r.atoms.intern("month"), Obj(o))
	if err != nil {
		return temporalPlainDate{}, err
	}
	month, monthPresent := 0, !monthValue.IsUndefined()
	if monthPresent {
		month, err = r.temporalTruncatedInteger(monthValue, "month")
		if err != nil {
			return temporalPlainDate{}, err
		}
	}
	monthCodeValue, err := r.getProp(o, r.atoms.intern("monthCode"), Obj(o))
	if err != nil {
		return temporalPlainDate{}, err
	}
	monthCode, monthCodePresent := "", !monthCodeValue.IsUndefined()
	if monthCodePresent {
		if monthCodeValue.IsString() {
			monthCode = monthCodeValue.String().Go()
		} else if monthCodeValue.IsObject() {
			primitive, err := r.toPrimitive(monthCodeValue, hintString)
			if err != nil {
				return temporalPlainDate{}, err
			}
			if !primitive.IsString() {
				return temporalPlainDate{}, r.throwTypeError("monthCode must resolve to a string")
			}
			monthCode = primitive.String().Go()
		} else {
			return temporalPlainDate{}, r.throwTypeError("monthCode must be a string")
		}
		if !wellFormedTemporalMonthCode(monthCode) {
			return temporalPlainDate{}, r.throwRangeError("invalid monthCode")
		}
	}
	yearValue, err := r.getProp(o, r.atoms.intern("year"), Obj(o))
	if err != nil {
		return temporalPlainDate{}, err
	}
	year, yearPresent := 0, !yearValue.IsUndefined()
	if yearPresent {
		year, err = r.temporalTruncatedInteger(yearValue, "year")
		if err != nil {
			return temporalPlainDate{}, err
		}
	}
	overflow, err := r.temporalOverflowOption(optionsValue)
	if err != nil {
		return temporalPlainDate{}, err
	}
	if eraPresent != eraYearPresent {
		return temporalPlainDate{}, r.throwTypeError("era and eraYear must be provided together")
	}
	if !dayPresent || !yearPresent || !monthPresent && !monthCodePresent {
		if !yearPresent && eraPresent && eraYearPresent {
			year, _ = temporalYearFromEra(calendar, era, eraYear)
			yearPresent = true
		}
		if !dayPresent || !yearPresent || !monthPresent && !monthCodePresent {
			return temporalPlainDate{}, r.throwTypeError("plain date property bag is missing required fields")
		}
	}
	constrain := overflow == "constrain"
	var isoYear, isoMonth, isoDay int
	if monthCodePresent {
		codeMonth, leap, ok := parseTemporalMonthCode(monthCode)
		if !ok {
			return temporalPlainDate{}, r.throwRangeError("invalid monthCode")
		}
		isoYear, isoMonth, isoDay, ok = icu.ResolveDate(calendar, year,
			codeMonth, day, leap, true, constrain)
		if !ok {
			return temporalPlainDate{}, r.throwRangeError("invalid Temporal.PlainDate")
		}
		if monthPresent {
			resolved := temporalPlainDate{
				year: isoYear, month: isoMonth, day: isoDay, calendar: calendar,
			}.calendarDate()
			if month != resolved.OrdinalMonth {
				return temporalPlainDate{}, r.throwRangeError("month and monthCode do not agree")
			}
		}
	} else {
		var ok bool
		isoYear, isoMonth, isoDay, ok = icu.ResolveDate(calendar, year,
			month, day, false, false, constrain)
		if !ok {
			return temporalPlainDate{}, r.throwRangeError("invalid Temporal.PlainDate")
		}
	}
	date := temporalPlainDate{
		year: isoYear, month: isoMonth, day: isoDay, calendar: calendar,
	}
	if !date.valid() {
		return temporalPlainDate{}, r.throwRangeError("invalid Temporal.PlainDate")
	}
	return date, nil
}

func temporalCalendarUsesEra(calendar string) bool {
	switch calendar {
	case "buddhist", "coptic", "ethioaa", "ethiopic", "gregory", "hebrew",
		"indian", "islamic-civil", "islamic-tbla", "islamic-umalqura",
		"japanese", "persian", "roc":
		return true
	}
	return false
}

func (r *Runtime) temporalOverflowOption(value Value) (string, error) {
	options, err := r.strictOptions(value)
	if err != nil {
		return "", err
	}
	return r.stringOption(options, "overflow", "constrain", "constrain", "reject")
}

func parseISOMonthCode(value string) (int, bool) {
	if len(value) != 3 || value[0] != 'M' || value[1] < '0' || value[1] > '9' || value[2] < '0' || value[2] > '9' {
		return 0, false
	}
	month := int(value[1]-'0')*10 + int(value[2]-'0')
	return month, month >= 1 && month <= 12
}

func parseTemporalMonthCode(value string) (month int, leap, ok bool) {
	if !wellFormedTemporalMonthCode(value) {
		return 0, false, false
	}
	month = int(value[1]-'0')*10 + int(value[2]-'0')
	return month, len(value) == 4, month >= 1
}

func wellFormedTemporalMonthCode(value string) bool {
	return (len(value) == 3 || len(value) == 4 && value[3] == 'L') &&
		value[0] == 'M' && value[1] >= '0' && value[1] <= '9' && value[2] >= '0' && value[2] <= '9'
}

func parseTemporalPlainDate(input string) (temporalPlainDate, error) {
	main, annotations, ok := splitTemporalAnnotations(input)
	if !ok {
		return temporalPlainDate{}, errInvalidTemporalInstant
	}
	calendar := "iso8601"
	calendarSeen, criticalCalendar, timeZoneSeen := false, false, false
	for _, annotation := range annotations {
		critical := strings.HasPrefix(annotation, "!")
		if critical {
			annotation = annotation[1:]
		}
		key, value, keyed := strings.Cut(annotation, "=")
		if !keyed {
			if timeZoneSeen || !validTimeZoneAnnotation(annotation) {
				return temporalPlainDate{}, errInvalidTemporalInstant
			}
			timeZoneSeen = true
			continue
		}
		if key == "u-ca" {
			if calendarSeen && (criticalCalendar || critical) {
				return temporalPlainDate{}, errInvalidTemporalInstant
			}
			if calendarSeen {
				continue
			}
			calendarSeen = true
			criticalCalendar = critical
			calendar = canonicalSetting("ca", asciiLower(value))
			if !icu.HasCalendar(calendar) {
				return temporalPlainDate{}, errInvalidTemporalInstant
			}
			continue
		}
		if critical || !validAnnotationKey(key) || value == "" {
			return temporalPlainDate{}, errInvalidTemporalInstant
		}
	}
	index := 0
	year, ok := parseTemporalYear(main, &index)
	if !ok {
		return temporalPlainDate{}, errInvalidTemporalInstant
	}
	dashed := consumeByte(main, &index, '-')
	month, ok := parseFixedDigits(main, &index, 2)
	if !ok || dashed && !consumeByte(main, &index, '-') {
		return temporalPlainDate{}, errInvalidTemporalInstant
	}
	day, ok := parseFixedDigits(main, &index, 2)
	if !ok {
		return temporalPlainDate{}, errInvalidTemporalInstant
	}
	if index < len(main) {
		if main[index] != 'T' && main[index] != 't' && main[index] != ' ' {
			return temporalPlainDate{}, errInvalidTemporalInstant
		}
		if main[len(main)-1] == 'Z' || main[len(main)-1] == 'z' {
			return temporalPlainDate{}, errInvalidTemporalInstant
		}
		if _, _, err := parseTemporalInstantFields(main); err != nil {
			if _, _, err := parseTemporalInstantFields(main + "Z"); err != nil {
				return temporalPlainDate{}, errInvalidTemporalInstant
			}
		}
	}
	date := temporalPlainDate{year: year, month: month, day: day, calendar: calendar}
	if !date.valid() {
		return temporalPlainDate{}, errInvalidTemporalInstant
	}
	return date, nil
}

func temporalCalendarFromISOString(input string) (string, bool) {
	if date, err := parseTemporalPlainDate(input); err == nil {
		return date.calendar, true
	}
	if _, err := parseTemporalPlainTime(input); err == nil {
		_, calendar, ok := parseTemporalPartialAnnotations(input)
		return calendar, ok
	}
	main, annotations, ok := splitTemporalAnnotations(input)
	if !ok {
		return "", false
	}
	calendar := "iso8601"
	for _, annotation := range annotations {
		annotation = strings.TrimPrefix(annotation, "!")
		key, value, keyed := strings.Cut(annotation, "=")
		if !keyed || key != "u-ca" {
			return "", false
		}
		calendar = canonicalSetting("ca", asciiLower(value))
		if !icu.HasCalendar(calendar) {
			return "", false
		}
	}
	// Calendar strings also admit ISO year-month and month-day forms.
	if len(main) == 5 && main[2] == '-' {
		month, monthOK := parseTwoDigits(main[:2])
		day, dayOK := parseTwoDigits(main[3:])
		return calendar, monthOK && dayOK && month >= 1 && month <= 12 && day >= 1 && day <= isoDaysInMonth(1972, month)
	}
	index := 0
	year, yearOK := parseTemporalYear(main, &index)
	if !yearOK {
		return "", false
	}
	dashed := consumeByte(main, &index, '-')
	month, monthOK := parseFixedDigits(main, &index, 2)
	return calendar, monthOK && index == len(main) && (!dashed || strings.Contains(main, "-")) && year >= -271821 && year <= 275760 && month >= 1 && month <= 12
}

func parseTwoDigits(value string) (int, bool) {
	index := 0
	number, ok := parseFixedDigits(value, &index, 2)
	return number, ok && index == len(value)
}

func (d temporalPlainDate) calendarDate() icu.Date {
	if d.calendar == "iso8601" {
		return icu.Date{Year: d.year, Month: d.month, Day: d.day,
			ArithmeticYear: d.year, OrdinalMonth: d.month, RelatedYear: d.year}
	}
	// DateIn only uses the civil fields of time.Time. Keeping the time at noon
	// avoids any boundary behavior in callers that later attach a zone.
	return icu.DateIn(d.calendar, time.Date(d.year, time.Month(d.month), d.day, 12, 0, 0, 0, time.UTC))
}

func temporalCalendarEra(calendar string, date icu.Date) (string, bool) {
	era, _, ok := temporalCalendarEraFields(calendar, date)
	return era, ok
}

func temporalCalendarEraYear(calendar string, date icu.Date) (int, bool) {
	_, year, ok := temporalCalendarEraFields(calendar, date)
	return year, ok
}

func temporalCalendarEraFields(calendar string, date icu.Date) (string, int, bool) {
	switch calendar {
	case "iso8601":
		return "", 0, false
	case "buddhist":
		return "be", date.Year, true
	case "gregory":
		if date.Era == 0 {
			return "bce", date.Year, true
		}
		return "ce", date.Year, true
	case "roc":
		if date.Era == 0 {
			return "broc", date.Year, true
		}
		return "roc", date.Year, true
	case "coptic":
		return "am", date.Year, true
	case "ethiopic":
		if date.Era == 0 {
			return "aa", date.Year, true
		}
		return "am", date.Year, true
	case "ethioaa":
		return "aa", date.Year, true
	case "hebrew":
		return "am", date.Year, true
	case "indian":
		return "shaka", date.Year, true
	case "islamic-civil", "islamic-tbla", "islamic-umalqura":
		if date.Era == 0 {
			return "bh", date.Year, true
		}
		return "ah", date.Year, true
	case "japanese":
		isoYear, month, day := date.RelatedYear, date.Month, date.Day
		after := func(year, startMonth, startDay int) bool {
			return isoYear > year || isoYear == year &&
				(month > startMonth || month == startMonth && day >= startDay)
		}
		switch {
		case after(2019, 5, 1):
			return "reiwa", isoYear - 2018, true
		case after(1989, 1, 8):
			return "heisei", isoYear - 1988, true
		case after(1926, 12, 25):
			return "showa", isoYear - 1925, true
		case after(1912, 7, 30):
			return "taisho", isoYear - 1911, true
		case after(1873, 1, 1):
			return "meiji", isoYear - 1867, true
		case isoYear <= 0:
			return "bce", 1 - isoYear, true
		default:
			return "ce", isoYear, true
		}
	case "persian":
		return "ap", date.Year, true
	}
	return "", 0, false
}

func temporalYearFromEra(calendar, era string, eraYear int) (int, bool) {
	switch calendar {
	case "buddhist":
		return eraYear, era == "be"
	case "coptic":
		return eraYear, era == "am"
	case "ethioaa":
		return eraYear, era == "aa"
	case "ethiopic":
		switch era {
		case "aa":
			return eraYear - 5500, true
		case "am":
			return eraYear, true
		}
	case "gregory":
		switch era {
		case "ce", "ad":
			return eraYear, true
		case "bce", "bc":
			return 1 - eraYear, true
		}
	case "hebrew":
		return eraYear, era == "am"
	case "indian":
		return eraYear, era == "shaka"
	case "islamic-civil", "islamic-tbla", "islamic-umalqura":
		switch era {
		case "ah":
			return eraYear, true
		case "bh":
			return 1 - eraYear, true
		}
	case "japanese":
		switch era {
		case "ce", "ad":
			return eraYear, true
		case "bce", "bc":
			return 1 - eraYear, true
		case "meiji":
			return 1867 + eraYear, true
		case "taisho":
			return 1911 + eraYear, true
		case "showa":
			return 1925 + eraYear, true
		case "heisei":
			return 1988 + eraYear, true
		case "reiwa":
			return 2018 + eraYear, true
		}
	case "persian":
		return eraYear, era == "ap"
	case "roc":
		switch era {
		case "roc":
			return eraYear, true
		case "broc", "before-roc":
			return 1 - eraYear, true
		}
	}
	return 0, false
}

func isoDayOfWeek(days int64) int {
	weekday := int((days + 3) % 7)
	if weekday < 0 {
		weekday += 7
	}
	return weekday + 1
}

func isoWeekOfYear(days int64) (week, year int) {
	weekday := isoDayOfWeek(days)
	thursday := days + int64(4-weekday)
	year, _, _ = isoCivilFromDays(thursday)
	jan4 := isoDaysFromCivil(int64(year), 1, 4)
	weekOneMonday := jan4 - int64(isoDayOfWeek(jan4)-1)
	return int((days-weekOneMonday)/7) + 1, year
}

func (d temporalPlainDate) string(showCalendar string) string {
	var b strings.Builder
	writeTemporalISOYear(&b, d.year)
	b.WriteByte('-')
	writePaddedTemporalInt(&b, d.month, 2)
	b.WriteByte('-')
	writePaddedTemporalInt(&b, d.day, 2)
	if showCalendar == "always" || showCalendar == "critical" || showCalendar == "auto" && d.calendar != "iso8601" {
		b.WriteByte('[')
		if showCalendar == "critical" {
			b.WriteByte('!')
		}
		b.WriteString("u-ca=")
		b.WriteString(d.calendar)
		b.WriteByte(']')
	}
	return b.String()
}

func writeTemporalISOYear(b *strings.Builder, year int) {
	if year >= 0 && year <= 9999 {
		writePaddedTemporalInt(b, year, 4)
		return
	}
	if year < 0 {
		b.WriteByte('-')
		writePaddedTemporalInt(b, -year, 6)
		return
	}
	b.WriteByte('+')
	writePaddedTemporalInt(b, year, 6)
}
