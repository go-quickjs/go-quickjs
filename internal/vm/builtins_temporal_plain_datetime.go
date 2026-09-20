package vm

import (
	"math"
	"math/big"
	"strconv"
	"strings"
	"time"
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
	r.defMethod(proto, "round", 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
		dateTime, err := rt.temporalPlainDateTimeValue(this, "Temporal.PlainDateTime.prototype.round")
		if err != nil {
			return Undefined, err
		}
		smallest, increment, mode, err := rt.temporalRoundOptions(arg(args, 0), false, true)
		if err != nil {
			return Undefined, err
		}
		step := int64(86_400_000_000_000)
		if smallest != "day" {
			step = temporalUnitNanoseconds[smallest] * increment
		}
		dateTime, err = rt.roundTemporalPlainDateTime(dateTime, step, mode)
		if err != nil {
			return Undefined, err
		}
		return Obj(newTemporalPlainDateTime(rt.temporalPlainDateTimeProto, dateTime)), nil
	})
	for _, operation := range []struct {
		name string
		sign int64
	}{{"add", 1}, {"subtract", -1}} {
		op := operation
		r.defMethod(proto, op.name, 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
			dateTime, err := rt.temporalPlainDateTimeValue(this, "Temporal.PlainDateTime.prototype."+op.name)
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
			dateTime, err = rt.addTemporalPlainDateTime(dateTime, duration, op.sign, overflow)
			if err != nil {
				return Undefined, err
			}
			return Obj(newTemporalPlainDateTime(rt.temporalPlainDateTimeProto, dateTime)), nil
		})
	}
	for _, operation := range []struct {
		name  string
		since bool
	}{{"until", false}, {"since", true}} {
		op := operation
		r.defMethod(proto, op.name, 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
			dateTime, err := rt.temporalPlainDateTimeValue(this, "Temporal.PlainDateTime.prototype."+op.name)
			if err != nil {
				return Undefined, err
			}
			other, err := rt.toTemporalPlainDateTime(arg(args, 0), Undefined)
			if err != nil {
				return Undefined, err
			}
			if dateTime.calendar != other.calendar {
				return Undefined, rt.throwRangeError("date-time calendars must match")
			}
			largest, smallest, increment, mode, err := rt.temporalPlainDateTimeDifferenceOptions(arg(args, 1))
			if err != nil {
				return Undefined, err
			}
			if op.since {
				mode = negateTemporalRoundingMode(mode)
			}
			duration, err := rt.differenceTemporalPlainDateTimes(dateTime, other, largest, smallest, increment, mode)
			if err != nil {
				return Undefined, err
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
		precision, minuteOnly, step, mode, err := rt.temporalPlainTimeStringOptionsFrom(options)
		if err != nil {
			return Undefined, err
		}
		dateTime, err = rt.roundTemporalPlainDateTime(dateTime, step, mode)
		if err != nil {
			return Undefined, err
		}
		return Str(NewString(dateTime.stringWithPrecision(show, precision, minuteOnly))), nil
	})
	r.defMethod(proto, "toJSON", 0, func(rt *Runtime, this Value, args []Value) (Value, error) {
		dateTime, err := rt.temporalPlainDateTimeValue(this, "Temporal.PlainDateTime.prototype.toJSON")
		if err != nil {
			return Undefined, err
		}
		return Str(NewString(dateTime.string("auto"))), nil
	})
	r.defMethod(proto, "toLocaleString", 0, func(rt *Runtime, this Value, args []Value) (Value, error) {
		dateTime, err := rt.temporalPlainDateTimeValue(this, "Temporal.PlainDateTime.prototype.toLocaleString")
		if err != nil {
			return Undefined, err
		}
		options, err := rt.dateOptionsFrom(args, map[string]string{
			"year": "numeric", "month": "numeric", "day": "numeric",
			"hour": "numeric", "minute": "numeric", "second": "numeric",
		}, "any")
		if err != nil {
			return Undefined, err
		}
		zone := options.zone
		if zone == nil {
			zone = time.UTC
		}
		value := time.Date(dateTime.year, time.Month(dateTime.month), dateTime.day,
			dateTime.hour, dateTime.minute, dateTime.second,
			dateTime.millisecond*1_000_000+dateTime.microsecond*1_000+dateTime.nanosecond, zone)
		return Str(NewString(options.format(value))), nil
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

func (r *Runtime) roundTemporalPlainDateTime(dateTime temporalPlainDateTime, step int64, mode string) (temporalPlainDateTime, error) {
	time := temporalPlainTime{dateTime.hour, dateTime.minute, dateTime.second, dateTime.millisecond, dateTime.microsecond, dateTime.nanosecond}
	rounded := roundTemporalBigIntAsIfPositive(temporalPlainTimeNanoseconds(time), big.NewInt(step), mode)
	dayCarry, remainder := new(big.Int), new(big.Int)
	dayCarry.DivMod(rounded, big.NewInt(86_400_000_000_000), remainder)
	days := isoDaysFromCivil(int64(dateTime.year), dateTime.month, dateTime.day) + dayCarry.Int64()
	year, month, day := isoCivilFromDays(days)
	dateTime.year, dateTime.month, dateTime.day = year, month, day
	time = temporalPlainTimeFromNanoseconds(remainder)
	dateTime.hour, dateTime.minute, dateTime.second = time.hour, time.minute, time.second
	dateTime.millisecond, dateTime.microsecond, dateTime.nanosecond = time.millisecond, time.microsecond, time.nanosecond
	if !dateTime.valid() {
		return temporalPlainDateTime{}, r.throwRangeError("rounded date-time is outside the Temporal range")
	}
	return dateTime, nil
}

func (r *Runtime) addTemporalPlainDateTime(dateTime temporalPlainDateTime, duration temporalDuration, sign int64, overflow string) (temporalPlainDateTime, error) {
	time := temporalPlainTime{dateTime.hour, dateTime.minute, dateTime.second, dateTime.millisecond, dateTime.microsecond, dateTime.nanosecond}
	total := temporalPlainTimeNanoseconds(time)
	delta := duration.timePartNanoseconds()
	if sign < 0 {
		delta.Neg(delta)
	}
	total.Add(total, delta)
	dayCarry, remainder := new(big.Int), new(big.Int)
	dayCarry.DivMod(total, big.NewInt(86_400_000_000_000), remainder)
	if !dayCarry.IsInt64() {
		return temporalPlainDateTime{}, r.throwRangeError("date-time is outside the Temporal range")
	}

	dateDuration := temporalDuration{
		years: duration.years, months: duration.months, weeks: duration.weeks,
		days: duration.days + float64(dayCarry.Int64()*sign),
	}
	date := temporalPlainDate{year: dateTime.year, month: dateTime.month, day: dateTime.day, calendar: dateTime.calendar}
	date, err := r.addTemporalPlainDate(date, dateDuration, sign, overflow)
	if err != nil {
		return temporalPlainDateTime{}, err
	}
	time = temporalPlainTimeFromNanoseconds(remainder)
	result := temporalPlainDateTime{temporalISODateTime: temporalISODateTime{
		year: date.year, month: date.month, day: date.day,
		hour: time.hour, minute: time.minute, second: time.second,
		millisecond: time.millisecond, microsecond: time.microsecond, nanosecond: time.nanosecond,
	}, calendar: dateTime.calendar}
	if !result.valid() {
		return temporalPlainDateTime{}, r.throwRangeError("date-time is outside the Temporal range")
	}
	return result, nil
}

var temporalDateTimeUnitRank = map[string]int{
	"year": 0, "month": 1, "week": 2, "day": 3,
	"hour": 4, "minute": 5, "second": 6, "millisecond": 7, "microsecond": 8, "nanosecond": 9,
}

func normalizeTemporalDateTimeUnit(unit string) (string, bool) {
	unit = strings.TrimSuffix(unit, "s")
	_, ok := temporalDateTimeUnitRank[unit]
	return unit, ok
}

func (r *Runtime) temporalPlainDateTimeDifferenceOptions(value Value) (largest, smallest string, increment int64, mode string, err error) {
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
	smallestRaw, err := r.stringOption(options, "smallestUnit", "nanosecond")
	if err != nil {
		return "", "", 0, "", err
	}
	smallest, ok := normalizeTemporalDateTimeUnit(smallestRaw)
	if !ok {
		return "", "", 0, "", r.throwRangeError("invalid smallestUnit")
	}
	if largestRaw == "auto" {
		largest = "day"
		if temporalDateTimeUnitRank[smallest] < temporalDateTimeUnitRank[largest] {
			largest = smallest
		}
	} else {
		largest, ok = normalizeTemporalDateTimeUnit(largestRaw)
		if !ok {
			return "", "", 0, "", r.throwRangeError("invalid largestUnit")
		}
	}
	if temporalDateTimeUnitRank[largest] > temporalDateTimeUnitRank[smallest] {
		return "", "", 0, "", r.throwRangeError("largestUnit must not be smaller than smallestUnit")
	}
	if temporalDateTimeUnitRank[smallest] >= temporalDateTimeUnitRank["hour"] {
		increment, err = r.validateTemporalRoundingIncrement(rawIncrement, incrementSet, smallest, false)
	} else if !incrementSet {
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

func temporalPlainDateTimeEpochNanoseconds(dateTime temporalPlainDateTime) *big.Int {
	days := big.NewInt(isoDaysFromCivil(int64(dateTime.year), dateTime.month, dateTime.day))
	days.Mul(days, big.NewInt(86_400_000_000_000))
	time := temporalPlainTime{dateTime.hour, dateTime.minute, dateTime.second, dateTime.millisecond, dateTime.microsecond, dateTime.nanosecond}
	return days.Add(days, temporalPlainTimeNanoseconds(time))
}

func (r *Runtime) differenceTemporalPlainDateTimes(start, end temporalPlainDateTime, largest, smallest string, increment int64, mode string) (temporalDuration, error) {
	difference := new(big.Int).Sub(temporalPlainDateTimeEpochNanoseconds(end), temporalPlainDateTimeEpochNanoseconds(start))
	if temporalDateTimeUnitRank[largest] >= temporalDateTimeUnitRank["hour"] {
		step := new(big.Int).Mul(big.NewInt(temporalUnitNanoseconds[smallest]), big.NewInt(increment))
		difference = roundTemporalBigInt(difference, step, mode)
		return temporalDurationFromNanoseconds(difference, largest), nil
	}

	startDate := temporalPlainDate{year: start.year, month: start.month, day: start.day, calendar: start.calendar}
	endDays := isoDaysFromCivil(int64(end.year), end.month, end.day)
	startTime := temporalPlainTime{start.hour, start.minute, start.second, start.millisecond, start.microsecond, start.nanosecond}
	endTime := temporalPlainTime{end.hour, end.minute, end.second, end.millisecond, end.microsecond, end.nanosecond}
	timeDifference := new(big.Int).Sub(temporalPlainTimeNanoseconds(endTime), temporalPlainTimeNanoseconds(startTime))
	if difference.Sign() > 0 && timeDifference.Sign() < 0 {
		endDays--
		timeDifference.Add(timeDifference, big.NewInt(86_400_000_000_000))
	} else if difference.Sign() < 0 && timeDifference.Sign() > 0 {
		endDays++
		timeDifference.Sub(timeDifference, big.NewInt(86_400_000_000_000))
	}

	if temporalDateTimeUnitRank[smallest] >= temporalDateTimeUnitRank["hour"] {
		step := new(big.Int).Mul(big.NewInt(temporalUnitNanoseconds[smallest]), big.NewInt(increment))
		timeDifference = roundTemporalBigInt(timeDifference, step, mode)
		carry, remainder := new(big.Int), new(big.Int)
		carry.QuoRem(timeDifference, big.NewInt(86_400_000_000_000), remainder)
		endDays += carry.Int64()
		timeDifference = remainder
	}

	endYear, endMonth, endDay := isoCivilFromDays(endDays)
	endDate := temporalPlainDate{year: endYear, month: endMonth, day: endDay, calendar: end.calendar}
	dateDuration, err := r.differenceTemporalPlainDates(startDate, endDate, largest, "day", 1, "trunc")
	if err != nil {
		return temporalDuration{}, err
	}
	timeDuration := temporalDurationFromNanoseconds(timeDifference, "hour")
	dateDuration.hours, dateDuration.minutes, dateDuration.seconds = timeDuration.hours, timeDuration.minutes, timeDuration.seconds
	dateDuration.milliseconds, dateDuration.microseconds, dateDuration.nanoseconds = timeDuration.milliseconds, timeDuration.microseconds, timeDuration.nanoseconds
	if smallest == "day" {
		return r.roundTemporalPlainDateTimeToDay(startDate, dateDuration, largest, increment, mode)
	}
	if temporalDateTimeUnitRank[smallest] < temporalDateTimeUnitRank["day"] {
		return r.roundTemporalPlainDateTimeToCalendarUnit(start, end, dateDuration, largest, smallest, increment, mode)
	}
	return dateDuration, nil
}

func (r *Runtime) roundTemporalPlainDateTimeToDay(start temporalPlainDate, duration temporalDuration, largest string, increment int64, mode string) (temporalDuration, error) {
	const dayNanoseconds = int64(86_400_000_000_000)
	timeDuration := new(big.Int).Mul(floatIntegerBig(duration.days), big.NewInt(dayNanoseconds))
	timeDuration.Add(timeDuration, duration.timePartNanoseconds())
	step := new(big.Int).Mul(big.NewInt(dayNanoseconds), big.NewInt(increment))
	rounded := roundTemporalBigInt(timeDuration, step, mode)
	roundedDays := new(big.Int).Quo(rounded, big.NewInt(dayNanoseconds)).Int64()
	wholeDays := new(big.Int).Quo(timeDuration, big.NewInt(dayNanoseconds)).Int64()

	result := temporalDuration{
		years: duration.years, months: duration.months, weeks: duration.weeks,
		days: float64(roundedDays),
	}
	dayDelta := roundedDays - wholeDays
	if dayDelta == 0 || (dayDelta > 0) != (timeDuration.Sign() > 0) {
		return result, nil
	}
	if largest == "day" {
		return result, nil
	}
	if largest == "week" {
		result.weeks += float64(roundedDays / 7)
		result.days = float64(roundedDays % 7)
		return result, nil
	}

	end, err := r.addTemporalPlainDate(start, result, 1, "constrain")
	if err != nil {
		return temporalDuration{}, err
	}
	return r.differenceTemporalPlainDates(start, end, largest, "day", 1, "trunc")
}

func (r *Runtime) roundTemporalPlainDateTimeToCalendarUnit(start, end temporalPlainDateTime, duration temporalDuration, largest, unit string, increment int64, mode string) (temporalDuration, error) {
	sign := int64(duration.sign())
	if sign == 0 {
		return temporalDuration{}, nil
	}
	var amount int64
	switch unit {
	case "year":
		amount = int64(duration.years)
	case "month":
		amount = int64(duration.months)
	case "week":
		amount = int64(duration.weeks) + int64(duration.days)/7
	}
	r1 := amount / increment * increment
	r2 := r1 + increment*sign

	startDate := temporalPlainDate{year: start.year, month: start.month, day: start.day, calendar: start.calendar}
	candidate := func(amount int64) (temporalDuration, temporalPlainDateTime, *big.Int, error) {
		var value temporalDuration
		switch unit {
		case "year":
			value.years = float64(amount)
		case "month":
			value.years, value.months = duration.years, float64(amount)
		case "week":
			value.years, value.months, value.weeks = duration.years, duration.months, float64(amount)
		}
		date, err := r.addTemporalPlainDate(startDate, value, 1, "constrain")
		if err != nil {
			return temporalDuration{}, temporalPlainDateTime{}, nil, err
		}
		dateTime := start
		dateTime.year, dateTime.month, dateTime.day = date.year, date.month, date.day
		return value, dateTime, temporalPlainDateTimeEpochNanoseconds(dateTime), nil
	}

	lowerDuration, lowerDateTime, lowerEpoch, err := candidate(r1)
	if err != nil {
		return temporalDuration{}, err
	}
	upperDuration, upperDateTime, upperEpoch, err := candidate(r2)
	if err != nil {
		return temporalDuration{}, err
	}
	targetEpoch := temporalPlainDateTimeEpochNanoseconds(end)
	between := func() bool {
		if sign > 0 {
			return lowerEpoch.Cmp(targetEpoch) <= 0 && targetEpoch.Cmp(upperEpoch) <= 0
		}
		return upperEpoch.Cmp(targetEpoch) <= 0 && targetEpoch.Cmp(lowerEpoch) <= 0
	}
	didExpand := false
	if !between() {
		r1, r2 = r2, r2+increment*sign
		lowerDuration, lowerDateTime, lowerEpoch, err = candidate(r1)
		if err != nil {
			return temporalDuration{}, err
		}
		upperDuration, upperDateTime, upperEpoch, err = candidate(r2)
		if err != nil {
			return temporalDuration{}, err
		}
		didExpand = true
	}

	chooseUpper := targetEpoch.Cmp(upperEpoch) == 0
	if targetEpoch.Cmp(lowerEpoch) != 0 && !chooseUpper {
		switch mode {
		case "expand":
			chooseUpper = true
		case "ceil":
			chooseUpper = sign > 0
		case "floor":
			chooseUpper = sign < 0
		case "halfCeil", "halfFloor", "halfExpand", "halfTrunc", "halfEven":
			fromLower := new(big.Int).Abs(new(big.Int).Sub(targetEpoch, lowerEpoch))
			toUpper := new(big.Int).Abs(new(big.Int).Sub(upperEpoch, targetEpoch))
			switch comparison := fromLower.Cmp(toUpper); {
			case comparison > 0:
				chooseUpper = true
			case comparison == 0:
				switch mode {
				case "halfCeil":
					chooseUpper = sign > 0
				case "halfFloor":
					chooseUpper = sign < 0
				case "halfExpand":
					chooseUpper = true
				case "halfEven":
					chooseUpper = (r1/increment)%2 != 0
				}
			}
		}
	}
	result, resultDateTime := lowerDuration, lowerDateTime
	if chooseUpper {
		result, resultDateTime = upperDuration, upperDateTime
		didExpand = true
	}
	if !didExpand || unit == "week" {
		return result, nil
	}
	resultDate := temporalPlainDate{year: resultDateTime.year, month: resultDateTime.month, day: resultDateTime.day, calendar: resultDateTime.calendar}
	return r.differenceTemporalPlainDates(startDate, resultDate, largest, "day", 1, "trunc")
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
	return d.stringWithPrecision(showCalendar, -1, false)
}

func (d temporalPlainDateTime) stringWithPrecision(showCalendar string, precision int, minuteOnly bool) string {
	date := temporalPlainDate{year: d.year, month: d.month, day: d.day, calendar: d.calendar}
	base := date.string("never")
	var b strings.Builder
	b.WriteString(base)
	b.WriteByte('T')
	time := temporalPlainTime{d.hour, d.minute, d.second, d.millisecond, d.microsecond, d.nanosecond}
	b.WriteString(time.stringWithPrecision(precision, minuteOnly))
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
