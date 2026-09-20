package vm

import (
	"math"
	"strconv"
	"strings"
)

type temporalPlainDateTime struct {
	temporalISODateTime
	calendar string
}

func (d temporalPlainDateTime) valid() bool {
	if !d.temporalISODateTime.valid() {
		return false
	}
	days := isoDaysFromCivil(int64(d.year), d.month, d.day)
	if days < -100_000_001 || days > 100_000_000 {
		return false
	}
	return days != -100_000_001 || d.hour != 0 || d.minute != 0 || d.second != 0 || d.subsecondNanoseconds() != 0
}

func (r *Runtime) initTemporalPlainDateTime(temporal *Object) {
	proto := newObject(r.proto.object, ClassObject)
	r.temporalPlainDateTimeProto = proto
	ctor := r.newTemporalCtor(temporal, "PlainDateTime", 3, proto, func(rt *Runtime, this Value, args []Value) (Value, error) {
		if err := rt.requireNew("Temporal.PlainDateTime"); err != nil {
			return Undefined, err
		}
		fields := [9]int{}
		for i, name := range []string{"year", "month", "day", "hour", "minute", "second", "millisecond", "microsecond", "nanosecond"} {
			if i >= 3 && arg(args, i).IsUndefined() {
				continue
			}
			value, err := rt.temporalTruncatedInteger(arg(args, i), name)
			if err != nil {
				return Undefined, err
			}
			fields[i] = value
		}
		calendar, err := rt.toTemporalCalendarIdentifier(arg(args, 9))
		if err != nil {
			return Undefined, err
		}
		dateTime := temporalPlainDateTime{temporalISODateTime: temporalISODateTime{
			year: fields[0], month: fields[1], day: fields[2],
			hour: fields[3], minute: fields[4], second: fields[5],
			millisecond: fields[6], microsecond: fields[7], nanosecond: fields[8],
		}, calendar: calendar}
		if !dateTime.valid() {
			return Undefined, rt.throwRangeError("invalid Temporal.PlainDateTime")
		}
		instanceProto, err := rt.protoFromNewTargetErr(proto)
		if err != nil {
			return Undefined, err
		}
		return Obj(newTemporalPlainDateTime(instanceProto, dateTime)), nil
	})

	r.defMethod(ctor, "from", 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
		dateTime, err := rt.toTemporalPlainDateTime(arg(args, 0), arg(args, 1))
		if err != nil {
			return Undefined, err
		}
		return Obj(newTemporalPlainDateTime(proto, dateTime)), nil
	})
	r.defMethod(ctor, "compare", 2, func(rt *Runtime, this Value, args []Value) (Value, error) {
		left, err := rt.toTemporalPlainDateTime(arg(args, 0), Undefined)
		if err != nil {
			return Undefined, err
		}
		right, err := rt.toTemporalPlainDateTime(arg(args, 1), Undefined)
		if err != nil {
			return Undefined, err
		}
		return Int(compareTemporalPlainDateTimes(left, right)), nil
	})

	for _, property := range []string{"year", "month", "monthCode", "day", "calendarId", "era", "eraYear", "hour", "minute", "second", "millisecond", "microsecond", "nanosecond"} {
		name := property
		r.defGetter(proto, name, func(rt *Runtime, this Value, args []Value) (Value, error) {
			dateTime, err := rt.temporalPlainDateTimeValue(this, "get Temporal.PlainDateTime.prototype."+name)
			if err != nil {
				return Undefined, err
			}
			date := temporalPlainDate{year: dateTime.year, month: dateTime.month, day: dateTime.day, calendar: dateTime.calendar}
			calendarDate := date.calendarDate()
			switch name {
			case "year":
				return Int(calendarDate.Year), nil
			case "month":
				return Int(calendarDate.Month), nil
			case "monthCode":
				code := "M" + strconv.Itoa(calendarDate.Month + 100)[1:]
				if calendarDate.Leap {
					code += "L"
				}
				return Str(NewString(code)), nil
			case "day":
				return Int(calendarDate.Day), nil
			case "calendarId":
				return Str(NewString(dateTime.calendar)), nil
			case "era":
				era, ok := temporalCalendarEra(dateTime.calendar, calendarDate)
				if !ok {
					return Undefined, nil
				}
				return Str(NewString(era)), nil
			case "eraYear":
				if _, ok := temporalCalendarEra(dateTime.calendar, calendarDate); !ok {
					return Undefined, nil
				}
				return Int(calendarDate.Year), nil
			case "hour":
				return Int(dateTime.hour), nil
			case "minute":
				return Int(dateTime.minute), nil
			case "second":
				return Int(dateTime.second), nil
			case "millisecond":
				return Int(dateTime.millisecond), nil
			case "microsecond":
				return Int(dateTime.microsecond), nil
			case "nanosecond":
				return Int(dateTime.nanosecond), nil
			}
			return Undefined, nil
		})
	}
	for _, property := range []string{"dayOfWeek", "dayOfYear", "weekOfYear", "yearOfWeek", "daysInWeek", "daysInMonth", "daysInYear", "monthsInYear", "inLeapYear"} {
		name := property
		r.defGetter(proto, name, func(rt *Runtime, this Value, args []Value) (Value, error) {
			dateTime, err := rt.temporalPlainDateTimeValue(this, "get Temporal.PlainDateTime.prototype."+name)
			if err != nil {
				return Undefined, err
			}
			days := isoDaysFromCivil(int64(dateTime.year), dateTime.month, dateTime.day)
			switch name {
			case "dayOfWeek":
				return Int(isoDayOfWeek(days)), nil
			case "dayOfYear":
				return Int(int(days - isoDaysFromCivil(int64(dateTime.year), 1, 1) + 1)), nil
			case "weekOfYear":
				week, _ := isoWeekOfYear(days)
				return Int(week), nil
			case "yearOfWeek":
				_, year := isoWeekOfYear(days)
				return Int(year), nil
			case "daysInWeek":
				return Int(7), nil
			case "daysInMonth":
				return Int(isoDaysInMonth(dateTime.year, dateTime.month)), nil
			case "daysInYear":
				if isLeapYear(dateTime.year) {
					return Int(366), nil
				}
				return Int(365), nil
			case "monthsInYear":
				return Int(12), nil
			case "inLeapYear":
				return Bool(isLeapYear(dateTime.year)), nil
			}
			return Undefined, nil
		})
	}

	r.defMethod(proto, "equals", 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
		dateTime, err := rt.temporalPlainDateTimeValue(this, "Temporal.PlainDateTime.prototype.equals")
		if err != nil {
			return Undefined, err
		}
		other, err := rt.toTemporalPlainDateTime(arg(args, 0), Undefined)
		if err != nil {
			return Undefined, err
		}
		return Bool(compareTemporalPlainDateTimes(dateTime, other) == 0 && dateTime.calendar == other.calendar), nil
	})
	r.defMethod(proto, "toPlainDate", 0, func(rt *Runtime, this Value, args []Value) (Value, error) {
		dateTime, err := rt.temporalPlainDateTimeValue(this, "Temporal.PlainDateTime.prototype.toPlainDate")
		if err != nil {
			return Undefined, err
		}
		date := temporalPlainDate{year: dateTime.year, month: dateTime.month, day: dateTime.day, calendar: dateTime.calendar}
		return Obj(newTemporalPlainDate(rt.temporalPlainDateProto, date)), nil
	})
	r.defMethod(proto, "toPlainTime", 0, func(rt *Runtime, this Value, args []Value) (Value, error) {
		dateTime, err := rt.temporalPlainDateTimeValue(this, "Temporal.PlainDateTime.prototype.toPlainTime")
		if err != nil {
			return Undefined, err
		}
		time := temporalPlainTime{dateTime.hour, dateTime.minute, dateTime.second, dateTime.millisecond, dateTime.microsecond, dateTime.nanosecond}
		return Obj(newTemporalPlainTime(rt.temporalPlainTimeProto, time)), nil
	})
	r.defMethod(proto, "withPlainTime", 0, func(rt *Runtime, this Value, args []Value) (Value, error) {
		dateTime, err := rt.temporalPlainDateTimeValue(this, "Temporal.PlainDateTime.prototype.withPlainTime")
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
		dateTime.hour, dateTime.minute, dateTime.second = time.hour, time.minute, time.second
		dateTime.millisecond, dateTime.microsecond, dateTime.nanosecond = time.millisecond, time.microsecond, time.nanosecond
		if !dateTime.valid() {
			return Undefined, rt.throwRangeError("combined date and time are outside the Temporal range")
		}
		return Obj(newTemporalPlainDateTime(rt.temporalPlainDateTimeProto, dateTime)), nil
	})
	r.defMethod(proto, "withCalendar", 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
		dateTime, err := rt.temporalPlainDateTimeValue(this, "Temporal.PlainDateTime.prototype.withCalendar")
		if err != nil {
			return Undefined, err
		}
		calendarValue := arg(args, 0)
		if calendarValue.IsUndefined() {
			return Undefined, rt.throwTypeError("calendar is required")
		}
		calendar, err := rt.toTemporalCalendarIdentifierFromBag(calendarValue)
		if err != nil {
			return Undefined, err
		}
		dateTime.calendar = calendar
		return Obj(newTemporalPlainDateTime(rt.temporalPlainDateTimeProto, dateTime)), nil
	})
	r.defMethod(proto, "with", 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
		dateTime, err := rt.temporalPlainDateTimeValue(this, "Temporal.PlainDateTime.prototype.with")
		if err != nil {
			return Undefined, err
		}
		fields, err := rt.temporalPartialObject(arg(args, 0))
		if err != nil {
			return Undefined, err
		}
		names := []string{"day", "hour", "microsecond", "millisecond", "minute", "month", "monthCode", "nanosecond", "second", "year"}
		values := make(map[string]float64, len(names))
		present := make(map[string]bool, len(names))
		monthCode := ""
		for _, name := range names {
			raw, err := rt.getProp(fields, rt.atoms.intern(name), Obj(fields))
			if err != nil {
				return Undefined, err
			}
			if raw.IsUndefined() {
				continue
			}
			present[name] = true
			if name == "monthCode" {
				monthCode, err = rt.temporalMonthCodeString(raw)
				if err != nil {
					return Undefined, err
				}
				continue
			}
			value, err := rt.toNumber(raw)
			if err != nil {
				return Undefined, err
			}
			if math.IsNaN(value) || math.IsInf(value, 0) {
				return Undefined, rt.throwRangeError("%s must be a finite number", name)
			}
			values[name] = math.Trunc(value)
		}
		if len(present) == 0 {
			return Undefined, rt.throwTypeError("date-time fields must not be empty")
		}
		if present["day"] && values["day"] < 1 || present["month"] && values["month"] < 1 {
			return Undefined, rt.throwRangeError("invalid Temporal.PlainDateTime")
		}
		overflow, err := rt.temporalOverflowOption(arg(args, 1))
		if err != nil {
			return Undefined, err
		}

		year := float64(dateTime.year)
		if present["year"] {
			year = values["year"]
		}
		month := float64(dateTime.month)
		if present["month"] {
			month = values["month"]
		}
		if present["monthCode"] {
			parsed, ok := parseISOMonthCode(monthCode)
			if !ok || present["month"] && month != float64(parsed) {
				return Undefined, rt.throwRangeError("invalid monthCode")
			}
			month = float64(parsed)
		}
		day := float64(dateTime.day)
		if present["day"] {
			day = values["day"]
		}
		if year < -271821 || year > 275760 || month < 1 || day < 1 {
			return Undefined, rt.throwRangeError("invalid Temporal.PlainDateTime")
		}
		if overflow == "constrain" {
			month = min(month, 12)
			day = min(day, float64(isoDaysInMonth(int(year), int(month))))
		} else if month > 12 || day > float64(isoDaysInMonth(int(year), int(month))) {
			return Undefined, rt.throwRangeError("invalid Temporal.PlainDateTime")
		}
		result := dateTime
		result.year, result.month, result.day = int(year), int(month), int(day)
		timeLimits := map[string]float64{
			"hour": 23, "minute": 59, "second": 59,
			"millisecond": 999, "microsecond": 999, "nanosecond": 999,
		}
		for _, name := range []string{"hour", "microsecond", "millisecond", "minute", "nanosecond", "second"} {
			if !present[name] {
				continue
			}
			value := values[name]
			if overflow == "constrain" {
				value = max(0, min(timeLimits[name], value))
			} else if value < 0 || value > timeLimits[name] {
				return Undefined, rt.throwRangeError("invalid Temporal.PlainDateTime")
			}
			switch name {
			case "hour":
				result.hour = int(value)
			case "minute":
				result.minute = int(value)
			case "second":
				result.second = int(value)
			case "millisecond":
				result.millisecond = int(value)
			case "microsecond":
				result.microsecond = int(value)
			case "nanosecond":
				result.nanosecond = int(value)
			}
		}
		if !result.valid() {
			return Undefined, rt.throwRangeError("invalid Temporal.PlainDateTime")
		}
		return Obj(newTemporalPlainDateTime(rt.temporalPlainDateTimeProto, result)), nil
	})
	r.defMethod(proto, "toZonedDateTime", 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
		dateTime, err := rt.temporalPlainDateTimeValue(this, "Temporal.PlainDateTime.prototype.toZonedDateTime")
		if err != nil {
			return Undefined, err
		}
		timeZone, err := rt.toTemporalTimeZoneIdentifier(arg(args, 0))
		if err != nil {
			return Undefined, err
		}
		options, err := rt.strictOptions(arg(args, 1))
		if err != nil {
			return Undefined, err
		}
		disambiguation, err := rt.stringOption(options, "disambiguation", "compatible", "compatible", "earlier", "later", "reject")
		if err != nil {
			return Undefined, err
		}
		zoned, err := rt.newTemporalZonedDateTime(temporalInstant{}, timeZone, Str(NewString(dateTime.calendar)))
		if err != nil {
			return Undefined, err
		}
		instant, ok := zoned.disambiguatedInstant(dateTime.temporalISODateTime, disambiguation)
		if !ok {
			return Undefined, rt.throwRangeError("zoned date-time is ambiguous or outside the Temporal range")
		}
		zoned.instant = instant
		o := newObject(rt.temporalZonedDateTimeProto, ClassObject)
		o.data = zoned
		return Obj(o), nil
	})
	r.defMethod(proto, "toString", 0, func(rt *Runtime, this Value, args []Value) (Value, error) {
		dateTime, err := rt.temporalPlainDateTimeValue(this, "Temporal.PlainDateTime.prototype.toString")
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
		return Str(NewString(dateTime.string(show))), nil
	})
	r.defMethod(proto, "toJSON", 0, func(rt *Runtime, this Value, args []Value) (Value, error) {
		dateTime, err := rt.temporalPlainDateTimeValue(this, "Temporal.PlainDateTime.prototype.toJSON")
		if err != nil {
			return Undefined, err
		}
		return Str(NewString(dateTime.string("auto"))), nil
	})
	r.defMethod(proto, "valueOf", 0, func(rt *Runtime, this Value, args []Value) (Value, error) {
		if _, err := rt.temporalPlainDateTimeValue(this, "Temporal.PlainDateTime.prototype.valueOf"); err != nil {
			return Undefined, err
		}
		return Undefined, rt.throwTypeError("use Temporal.PlainDateTime.compare() or equals() to compare date-times")
	})
	r.defToStringTag(proto, "Temporal.PlainDateTime")
}

func newTemporalPlainDateTime(proto *Object, dateTime temporalPlainDateTime) *Object {
	o := newObject(proto, ClassObject)
	o.data = &dateTime
	return o
}

func (r *Runtime) temporalPlainDateTimeValue(value Value, method string) (temporalPlainDateTime, error) {
	if value.IsObject() {
		if dateTime, ok := value.Object().data.(*temporalPlainDateTime); ok && dateTime != nil {
			return *dateTime, nil
		}
	}
	return temporalPlainDateTime{}, r.throwTypeError("%s called on an incompatible receiver", method)
}

func (r *Runtime) toTemporalPlainDateTime(value, optionsValue Value) (temporalPlainDateTime, error) {
	if value.IsObject() {
		if dateTime, ok := value.Object().data.(*temporalPlainDateTime); ok && dateTime != nil {
			if _, err := r.temporalOverflowOption(optionsValue); err != nil {
				return temporalPlainDateTime{}, err
			}
			return *dateTime, nil
		}
		if date, ok := value.Object().data.(*temporalPlainDate); ok && date != nil {
			if _, err := r.temporalOverflowOption(optionsValue); err != nil {
				return temporalPlainDateTime{}, err
			}
			return temporalPlainDateTime{temporalISODateTime: temporalISODateTime{year: date.year, month: date.month, day: date.day}, calendar: date.calendar}, nil
		}
		if zoned, ok := value.Object().data.(*temporalZonedDateTime); ok && zoned != nil {
			if _, err := r.temporalOverflowOption(optionsValue); err != nil {
				return temporalPlainDateTime{}, err
			}
			localSeconds := zoned.instant.epochSeconds + int64(zoned.offsetSeconds())
			days := floorDivInt64(localSeconds, temporalSecondsPerDay)
			seconds := localSeconds - days*temporalSecondsPerDay
			year, month, day := isoCivilFromDays(days)
			return temporalPlainDateTime{temporalISODateTime: temporalISODateTime{
				year: year, month: month, day: day,
				hour: int(seconds / 3600), minute: int(seconds / 60 % 60), second: int(seconds % 60),
				millisecond: int(zoned.instant.nanosecond) / 1_000_000,
				microsecond: int(zoned.instant.nanosecond) / 1_000 % 1_000,
				nanosecond:  int(zoned.instant.nanosecond) % 1_000,
			}, calendar: zoned.calendar}, nil
		}
		return r.temporalPlainDateTimeFromBag(value.Object(), optionsValue)
	}
	if !value.IsString() {
		return temporalPlainDateTime{}, r.throwTypeError("a plain date-time must be a string or object")
	}
	dateTime, err := parseTemporalPlainDateTime(value.String().Go())
	if err != nil {
		return temporalPlainDateTime{}, r.throwRangeError("invalid Temporal.PlainDateTime string")
	}
	if _, err := r.temporalOverflowOption(optionsValue); err != nil {
		return temporalPlainDateTime{}, err
	}
	return dateTime, nil
}

func (r *Runtime) temporalPlainDateTimeFromBag(o *Object, optionsValue Value) (temporalPlainDateTime, error) {
	calendarValue, err := r.getProp(o, r.atoms.intern("calendar"), Obj(o))
	if err != nil {
		return temporalPlainDateTime{}, err
	}
	calendar, err := r.toTemporalCalendarIdentifierFromBag(calendarValue)
	if err != nil {
		return temporalPlainDateTime{}, err
	}
	values := make(map[string]int, 9)
	present := make(map[string]bool, 9)
	monthCode, monthCodePresent := "", false
	for _, name := range []string{"day", "hour", "microsecond", "millisecond", "minute", "month", "monthCode", "nanosecond", "second", "year"} {
		raw, err := r.getProp(o, r.atoms.intern(name), Obj(o))
		if err != nil {
			return temporalPlainDateTime{}, err
		}
		if raw.IsUndefined() {
			continue
		}
		if name == "monthCode" {
			monthCodePresent = true
			if raw.IsString() {
				monthCode = raw.String().Go()
			} else if raw.IsObject() {
				primitive, err := r.toPrimitive(raw, hintString)
				if err != nil {
					return temporalPlainDateTime{}, err
				}
				if !primitive.IsString() {
					return temporalPlainDateTime{}, r.throwTypeError("monthCode must resolve to a string")
				}
				monthCode = primitive.String().Go()
			} else {
				return temporalPlainDateTime{}, r.throwTypeError("monthCode must be a string")
			}
			if !wellFormedTemporalMonthCode(monthCode) {
				return temporalPlainDateTime{}, r.throwRangeError("invalid monthCode")
			}
			continue
		}
		value, err := r.temporalTruncatedInteger(raw, name)
		if err != nil {
			return temporalPlainDateTime{}, err
		}
		values[name], present[name] = value, true
	}
	overflow, err := r.temporalOverflowOption(optionsValue)
	if err != nil {
		return temporalPlainDateTime{}, err
	}
	if !present["year"] || !present["day"] || !present["month"] && !monthCodePresent {
		return temporalPlainDateTime{}, r.throwTypeError("plain date-time property bag is missing required fields")
	}
	month := values["month"]
	if monthCodePresent {
		parsed, ok := parseISOMonthCode(monthCode)
		if !ok || present["month"] && month != parsed {
			return temporalPlainDateTime{}, r.throwRangeError("invalid monthCode")
		}
		month = parsed
	}
	day, year := values["day"], values["year"]
	if overflow == "constrain" {
		if month < 1 || day < 1 {
			return temporalPlainDateTime{}, r.throwRangeError("invalid Temporal.PlainDateTime")
		}
		month = min(12, month)
		day = min(isoDaysInMonth(year, month), day)
		values["hour"] = max(0, min(23, values["hour"]))
		values["minute"] = max(0, min(59, values["minute"]))
		values["second"] = max(0, min(59, values["second"]))
		values["millisecond"] = max(0, min(999, values["millisecond"]))
		values["microsecond"] = max(0, min(999, values["microsecond"]))
		values["nanosecond"] = max(0, min(999, values["nanosecond"]))
	}
	result := temporalPlainDateTime{temporalISODateTime: temporalISODateTime{
		year: year, month: month, day: day,
		hour: values["hour"], minute: values["minute"], second: values["second"],
		millisecond: values["millisecond"], microsecond: values["microsecond"], nanosecond: values["nanosecond"],
	}, calendar: calendar}
	if !result.valid() {
		return temporalPlainDateTime{}, r.throwRangeError("invalid Temporal.PlainDateTime")
	}
	return result, nil
}

func parseTemporalPlainDateTime(input string) (temporalPlainDateTime, error) {
	date, err := parseTemporalPlainDate(input)
	if err != nil {
		return temporalPlainDateTime{}, err
	}
	main, _, _ := splitTemporalAnnotations(input)
	index := 0
	if _, ok := parseTemporalYear(main, &index); !ok {
		return temporalPlainDateTime{}, errInvalidTemporalInstant
	}
	dashed := consumeByte(main, &index, '-')
	if _, ok := parseFixedDigits(main, &index, 2); !ok || dashed && !consumeByte(main, &index, '-') {
		return temporalPlainDateTime{}, errInvalidTemporalInstant
	}
	if _, ok := parseFixedDigits(main, &index, 2); !ok {
		return temporalPlainDateTime{}, errInvalidTemporalInstant
	}
	fields := temporalISODateTime{year: date.year, month: date.month, day: date.day}
	if index < len(main) {
		if main[len(main)-1] == 'Z' || main[len(main)-1] == 'z' {
			return temporalPlainDateTime{}, errInvalidTemporalInstant
		}
		parsed, _, parseErr := parseTemporalInstantFields(main)
		if parseErr != nil {
			parsed, _, parseErr = parseTemporalInstantFields(main + "Z")
		}
		if parseErr != nil {
			return temporalPlainDateTime{}, errInvalidTemporalInstant
		}
		fields = parsed
	}
	result := temporalPlainDateTime{temporalISODateTime: fields, calendar: date.calendar}
	if !result.valid() {
		return temporalPlainDateTime{}, errInvalidTemporalInstant
	}
	return result, nil
}

func compareTemporalPlainDateTimes(left, right temporalPlainDateTime) int {
	leftSeconds, rightSeconds := left.localEpochSeconds(), right.localEpochSeconds()
	if leftSeconds < rightSeconds {
		return -1
	}
	if leftSeconds > rightSeconds {
		return 1
	}
	leftSubsecond, rightSubsecond := left.subsecondNanoseconds(), right.subsecondNanoseconds()
	if leftSubsecond < rightSubsecond {
		return -1
	}
	if leftSubsecond > rightSubsecond {
		return 1
	}
	return 0
}

func (d temporalPlainDateTime) string(showCalendar string) string {
	date := temporalPlainDate{year: d.year, month: d.month, day: d.day, calendar: d.calendar}
	base := date.string("never")
	var b strings.Builder
	b.WriteString(base)
	b.WriteByte('T')
	writePaddedTemporalInt(&b, d.hour, 2)
	b.WriteByte(':')
	writePaddedTemporalInt(&b, d.minute, 2)
	b.WriteByte(':')
	writePaddedTemporalInt(&b, d.second, 2)
	subsecond := d.subsecondNanoseconds()
	if subsecond != 0 {
		fraction := strconv.FormatInt(subsecond+temporalNanosecondsPerSecond, 10)[1:]
		b.WriteByte('.')
		b.WriteString(strings.TrimRight(fraction, "0"))
	}
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
