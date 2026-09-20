package vm

import (
	"math/big"
	"strings"

	"github.com/go-quickjs/go-quickjs/internal/icu"
)

type temporalZonedDateTimeOptions struct {
	disambiguation string
	offset         string
	overflow       string
}

func (r *Runtime) temporalZonedDateTimeOptions(value Value) (temporalZonedDateTimeOptions, error) {
	return r.temporalZonedDateTimeOptionsWithOffsetDefault(value, "reject")
}

func (r *Runtime) temporalZonedDateTimeOptionsWithOffsetDefault(value Value, offsetDefault string) (temporalZonedDateTimeOptions, error) {
	options, err := r.strictOptions(value)
	if err != nil {
		return temporalZonedDateTimeOptions{}, err
	}
	disambiguation, err := r.stringOption(options, "disambiguation", "compatible", "compatible", "earlier", "later", "reject")
	if err != nil {
		return temporalZonedDateTimeOptions{}, err
	}
	offset, err := r.stringOption(options, "offset", offsetDefault, "prefer", "use", "ignore", "reject")
	if err != nil {
		return temporalZonedDateTimeOptions{}, err
	}
	overflow, err := r.stringOption(options, "overflow", "constrain", "constrain", "reject")
	if err != nil {
		return temporalZonedDateTimeOptions{}, err
	}
	return temporalZonedDateTimeOptions{disambiguation: disambiguation, offset: offset, overflow: overflow}, nil
}

func (r *Runtime) toTemporalZonedDateTimeWithOptions(value, optionsValue Value) (*temporalZonedDateTime, error) {
	if value.IsObject() {
		if zoned, ok := value.Object().data.(*temporalZonedDateTime); ok && zoned != nil {
			if _, err := r.temporalZonedDateTimeOptions(optionsValue); err != nil {
				return nil, err
			}
			return zoned, nil
		}
		return r.temporalZonedDateTimeFromBag(value.Object(), optionsValue)
	}
	if !value.IsString() {
		return nil, r.throwTypeError("a zoned date-time must be a string or object")
	}
	parsed, err := parseTemporalZonedDateTimeInput(value.String().Go())
	if err != nil {
		return nil, r.throwRangeError("invalid Temporal.ZonedDateTime string")
	}
	options, err := r.temporalZonedDateTimeOptions(optionsValue)
	if err != nil {
		return nil, err
	}
	zoned, err := r.newTemporalZonedDateTime(temporalInstant{}, parsed.zone, Str(NewString(parsed.calendar)))
	if err != nil {
		return nil, err
	}
	instant, err := r.interpretTemporalZonedDateTime(parsed.dateTime, zoned, parsed.offsetPresent, parsed.offsetNanoseconds, parsed.exact, parsed.startOfDay, options)
	if err != nil {
		return nil, err
	}
	zoned.instant = instant
	return zoned, nil
}

type temporalZonedDateTimeFields struct {
	dateTime          temporalISODateTime
	calendar          string
	zone              string
	offsetNanoseconds int64
	offsetPresent     bool
	exact             bool
	startOfDay        bool
}

func parseTemporalZonedDateTimeInput(input string) (temporalZonedDateTimeFields, error) {
	main, annotations, ok := splitTemporalAnnotations(input)
	if !ok || !validInstantAnnotations(annotations) {
		return temporalZonedDateTimeFields{}, errInvalidTemporalInstant
	}
	parsed := temporalZonedDateTimeFields{calendar: "iso8601"}
	calendarSeen := false
	for _, raw := range annotations {
		annotation := strings.TrimPrefix(raw, "!")
		if key, value, keyed := strings.Cut(annotation, "="); keyed {
			if key == "u-ca" && !calendarSeen {
				calendarSeen = true
				parsed.calendar = canonicalSetting("ca", asciiLower(value))
				if !icu.HasCalendar(parsed.calendar) {
					return temporalZonedDateTimeFields{}, errInvalidTemporalInstant
				}
			}
			continue
		}
		parsed.zone = annotation
	}
	if parsed.zone == "" {
		return temporalZonedDateTimeFields{}, errInvalidTemporalInstant
	}

	timeStart := strings.IndexAny(main, "Tt ")
	if timeStart < 0 {
		date, err := parseTemporalPlainDate(input)
		if err != nil {
			return temporalZonedDateTimeFields{}, errInvalidTemporalInstant
		}
		parsed.dateTime = temporalISODateTime{year: date.year, month: date.month, day: date.day}
		parsed.startOfDay = true
		return parsed, nil
	}
	dateTime, offset, err := parseTemporalInstantFields(main)
	if err == nil {
		parsed.dateTime = dateTime
		parsed.offsetNanoseconds = offset
		parsed.offsetPresent = true
		parsed.exact = main[len(main)-1] == 'Z' || main[len(main)-1] == 'z'
		return parsed, nil
	}
	dateTime, _, err = parseTemporalInstantFields(main + "Z")
	if err != nil {
		return temporalZonedDateTimeFields{}, errInvalidTemporalInstant
	}
	parsed.dateTime = dateTime
	return parsed, nil
}

func (r *Runtime) temporalZonedDateTimeFromBag(o *Object, optionsValue Value) (*temporalZonedDateTime, error) {
	calendarValue, err := r.getProp(o, r.atoms.intern("calendar"), Obj(o))
	if err != nil {
		return nil, err
	}
	calendar, err := r.toTemporalCalendarIdentifierFromBag(calendarValue)
	if err != nil {
		return nil, err
	}
	values := make(map[string]int, 9)
	present := make(map[string]bool, 9)
	monthCode, monthCodePresent := "", false
	offsetNanoseconds, offsetPresent := int64(0), false
	var timeZoneValue Value
	for _, name := range []string{"day", "hour", "microsecond", "millisecond", "minute", "month", "monthCode", "nanosecond", "offset", "second", "timeZone", "year"} {
		raw, err := r.getProp(o, r.atoms.intern(name), Obj(o))
		if err != nil {
			return nil, err
		}
		switch name {
		case "monthCode":
			if raw.IsUndefined() {
				continue
			}
			monthCodePresent = true
			if raw.IsString() {
				monthCode = raw.String().Go()
			} else if raw.IsObject() {
				primitive, err := r.toPrimitive(raw, hintString)
				if err != nil {
					return nil, err
				}
				if !primitive.IsString() {
					return nil, r.throwTypeError("monthCode must resolve to a string")
				}
				monthCode = primitive.String().Go()
			} else {
				return nil, r.throwTypeError("monthCode must be a string")
			}
			if !wellFormedTemporalMonthCode(monthCode) {
				return nil, r.throwRangeError("invalid monthCode")
			}
			continue
		case "offset":
			if raw.IsUndefined() {
				continue
			}
			if !raw.IsString() && !raw.IsObject() {
				return nil, r.throwTypeError("offset must be a string")
			}
			text, err := r.toString(raw)
			if err != nil {
				return nil, err
			}
			offsetText := text.Go()
			index := 0
			if len(offsetText) == 0 || offsetText[0] != '+' && offsetText[0] != '-' {
				return nil, r.throwRangeError("invalid offset")
			}
			offsetNanoseconds, offsetPresent = parseTemporalOffset(offsetText, &index)
			if !offsetPresent || index != len(offsetText) {
				return nil, r.throwRangeError("invalid offset")
			}
			continue
		case "timeZone":
			timeZoneValue = raw
			continue
		}
		if raw.IsUndefined() {
			continue
		}
		value, err := r.temporalTruncatedInteger(raw, name)
		if err != nil {
			return nil, err
		}
		values[name], present[name] = value, true
	}

	options, err := r.temporalZonedDateTimeOptions(optionsValue)
	if err != nil {
		return nil, err
	}
	if !present["year"] || !present["day"] || !present["month"] && !monthCodePresent || timeZoneValue.IsUndefined() {
		return nil, r.throwTypeError("zoned date-time property bag is missing required fields")
	}
	month := values["month"]
	if monthCodePresent {
		parsed, ok := parseISOMonthCode(monthCode)
		if !ok || present["month"] && month != parsed {
			return nil, r.throwRangeError("invalid monthCode")
		}
		month = parsed
	}
	day, year := values["day"], values["year"]
	if month < 1 || day < 1 {
		return nil, r.throwRangeError("invalid Temporal.ZonedDateTime")
	}
	if options.overflow == "constrain" {
		month = min(month, 12)
		day = min(day, isoDaysInMonth(year, month))
		values["hour"] = max(0, min(23, values["hour"]))
		values["minute"] = max(0, min(59, values["minute"]))
		values["second"] = max(0, min(59, values["second"]))
		values["millisecond"] = max(0, min(999, values["millisecond"]))
		values["microsecond"] = max(0, min(999, values["microsecond"]))
		values["nanosecond"] = max(0, min(999, values["nanosecond"]))
	}
	dateTime := temporalISODateTime{
		year: year, month: month, day: day,
		hour: values["hour"], minute: values["minute"], second: values["second"],
		millisecond: values["millisecond"], microsecond: values["microsecond"], nanosecond: values["nanosecond"],
	}
	if !dateTime.valid() {
		return nil, r.throwRangeError("invalid Temporal.ZonedDateTime")
	}
	timeZone, err := r.toTemporalTimeZoneIdentifier(timeZoneValue)
	if err != nil {
		return nil, err
	}
	zoned, err := r.newTemporalZonedDateTime(temporalInstant{}, timeZone, Str(NewString(calendar)))
	if err != nil {
		return nil, err
	}
	instant, err := r.interpretTemporalZonedDateTime(dateTime, zoned, offsetPresent, offsetNanoseconds, false, false, options)
	if err != nil {
		return nil, err
	}
	zoned.instant = instant
	return zoned, nil
}

func (r *Runtime) temporalZonedDateTimeWith(zoned *temporalZonedDateTime, fieldsValue, optionsValue Value) (*temporalZonedDateTime, error) {
	if !fieldsValue.IsObject() {
		return nil, r.throwTypeError("zoned date-time fields must be an object")
	}
	o := fieldsValue.Object()
	calendarValue, err := r.getProp(o, r.atoms.intern("calendar"), fieldsValue)
	if err != nil {
		return nil, err
	}
	timeZoneValue, err := r.getProp(o, r.atoms.intern("timeZone"), fieldsValue)
	if err != nil {
		return nil, err
	}
	if !calendarValue.IsUndefined() || !timeZoneValue.IsUndefined() {
		return nil, r.throwTypeError("with fields cannot include calendar or timeZone")
	}
	switch o.data.(type) {
	case *temporalPlainDate, *temporalPlainDateTime, *temporalPlainTime, *temporalPlainMonthDay,
		*temporalPlainYearMonth, *temporalZonedDateTime:
		return nil, r.throwTypeError("with fields cannot be a Temporal object with a calendar or time zone")
	}

	values := make(map[string]int, 9)
	present := make(map[string]bool, 9)
	monthCode, offsetText := "", ""
	monthCodePresent, offsetPresent := false, false
	for _, name := range []string{"day", "hour", "microsecond", "millisecond", "minute", "month", "monthCode", "nanosecond", "offset", "second", "year"} {
		raw, err := r.getProp(o, r.atoms.intern(name), fieldsValue)
		if err != nil {
			return nil, err
		}
		if raw.IsUndefined() {
			continue
		}
		switch name {
		case "monthCode":
			monthCodePresent = true
			if raw.IsString() {
				monthCode = raw.String().Go()
			} else if raw.IsObject() {
				primitive, err := r.toPrimitive(raw, hintString)
				if err != nil {
					return nil, err
				}
				if !primitive.IsString() {
					return nil, r.throwTypeError("monthCode must resolve to a string")
				}
				monthCode = primitive.String().Go()
			} else {
				return nil, r.throwTypeError("monthCode must be a string")
			}
		case "offset":
			offsetPresent = true
			if !raw.IsString() && !raw.IsObject() {
				return nil, r.throwTypeError("offset must be a string")
			}
			text, err := r.toString(raw)
			if err != nil {
				return nil, err
			}
			offsetText = text.Go()
		default:
			value, err := r.temporalTruncatedInteger(raw, name)
			if err != nil {
				return nil, err
			}
			values[name], present[name] = value, true
		}
	}
	if present["month"] && values["month"] < 1 || present["day"] && values["day"] < 1 {
		return nil, r.throwRangeError("invalid Temporal.ZonedDateTime fields")
	}
	options, err := r.temporalZonedDateTimeOptionsWithOffsetDefault(optionsValue, "prefer")
	if err != nil {
		return nil, err
	}
	if len(present) == 0 && !monthCodePresent && !offsetPresent {
		return nil, r.throwTypeError("zoned date-time fields contain no recognized properties")
	}

	local := zoned.localISODateTime()
	if present["year"] {
		local.year = values["year"]
	}
	if present["month"] {
		local.month = values["month"]
	}
	if monthCodePresent {
		month, ok := parseISOMonthCode(monthCode)
		if !ok || present["month"] && values["month"] != month {
			return nil, r.throwRangeError("invalid monthCode")
		}
		local.month = month
	}
	if present["day"] {
		local.day = values["day"]
	}
	for _, field := range []struct {
		name   string
		target *int
		limit  int
	}{
		{"hour", &local.hour, 23}, {"minute", &local.minute, 59},
		{"second", &local.second, 59}, {"millisecond", &local.millisecond, 999},
		{"microsecond", &local.microsecond, 999}, {"nanosecond", &local.nanosecond, 999},
	} {
		if !present[field.name] {
			continue
		}
		value := values[field.name]
		if options.overflow == "constrain" {
			value = max(0, min(field.limit, value))
		}
		*field.target = value
	}
	if local.month < 1 || local.day < 1 {
		return nil, r.throwRangeError("invalid Temporal.ZonedDateTime fields")
	}
	if options.overflow == "constrain" {
		local.month = min(local.month, 12)
		local.day = min(local.day, isoDaysInMonth(local.year, local.month))
	}
	if !local.valid() {
		return nil, r.throwRangeError("invalid Temporal.ZonedDateTime fields")
	}

	offsetNanoseconds := int64(zoned.offsetSeconds()) * temporalNanosecondsPerSecond
	if offsetPresent {
		index := 0
		var ok bool
		offsetNanoseconds, ok = parseTemporalOffset(offsetText, &index)
		if !ok || index != len(offsetText) {
			return nil, r.throwRangeError("invalid offset")
		}
	}
	instant, err := r.interpretTemporalZonedDateTime(local, zoned, true, offsetNanoseconds, false, false, options)
	if err != nil {
		return nil, err
	}
	result := *zoned
	result.instant = instant
	return &result, nil
}

func (r *Runtime) interpretTemporalZonedDateTime(dateTime temporalISODateTime, zoned *temporalZonedDateTime, offsetPresent bool, offsetNanoseconds int64, exact, startOfDay bool, options temporalZonedDateTimeOptions) (temporalInstant, error) {
	if startOfDay {
		date := temporalPlainDate{year: dateTime.year, month: dateTime.month, day: dateTime.day, calendar: zoned.calendar}
		instant, ok := zoned.startOfDayInstant(date)
		if !ok {
			return temporalInstant{}, r.throwRangeError("zoned date-time is outside the Temporal range")
		}
		return instant, nil
	}
	if exact {
		instant, ok := temporalInstantFromLocalAndOffset(dateTime, offsetNanoseconds)
		if !ok {
			return temporalInstant{}, r.throwRangeError("zoned date-time is outside the Temporal range")
		}
		return instant, nil
	}
	if offsetPresent && (options.offset == "prefer" || options.offset == "reject") {
		days := isoDaysFromCivil(int64(dateTime.year), dateTime.month, dateTime.day)
		if days < -100_000_000 || days > 100_000_000 {
			return temporalInstant{}, r.throwRangeError("zoned date-time is outside the Temporal range")
		}
	}
	if offsetPresent && options.offset != "ignore" {
		candidate, ok := temporalInstantFromLocalAndOffset(dateTime, offsetNanoseconds)
		if ok {
			probe := *zoned
			probe.instant = candidate
			if int64(probe.offsetSeconds())*temporalNanosecondsPerSecond == offsetNanoseconds {
				return candidate, nil
			}
		}
		if options.offset == "use" {
			if !ok {
				return temporalInstant{}, r.throwRangeError("zoned date-time is outside the Temporal range")
			}
			return candidate, nil
		}
		if options.offset == "reject" {
			return temporalInstant{}, r.throwRangeError("offset does not match time zone")
		}
	}
	instant, ok := zoned.disambiguatedInstant(dateTime, options.disambiguation)
	if !ok {
		return temporalInstant{}, r.throwRangeError("zoned date-time is outside the Temporal range")
	}
	return instant, nil
}

func temporalInstantFromLocalAndOffset(dateTime temporalISODateTime, offsetNanoseconds int64) (temporalInstant, bool) {
	total := new(big.Int).Mul(big.NewInt(dateTime.localEpochSeconds()), big.NewInt(temporalNanosecondsPerSecond))
	total.Add(total, big.NewInt(dateTime.subsecondNanoseconds()))
	total.Sub(total, big.NewInt(offsetNanoseconds))
	return temporalInstantFromEpochNanoseconds(total)
}

func (r *Runtime) roundTemporalZonedDateTime(zoned *temporalZonedDateTime, smallest string, increment int64, mode string) (temporalInstant, error) {
	local := zoned.localISODateTime()
	if smallest == "day" {
		date := temporalPlainDate{
			year: local.year, month: local.month, day: local.day,
			calendar: zoned.calendar,
		}
		start, ok := zoned.startOfDayInstant(date)
		if !ok {
			return temporalInstant{}, r.throwRangeError("start of day is outside the Temporal range")
		}
		nextDays := isoDaysFromCivil(int64(date.year), date.month, date.day) + 1
		nextYear, nextMonth, nextDay := isoCivilFromDays(nextDays)
		nextDate := temporalPlainDate{
			year: nextYear, month: nextMonth, day: nextDay,
			calendar: zoned.calendar,
		}
		end, ok := zoned.startOfDayInstant(nextDate)
		if !ok {
			return temporalInstant{}, r.throwRangeError("end of day is outside the Temporal range")
		}
		startNanoseconds := start.epochNanoseconds()
		dayLength := new(big.Int).Sub(end.epochNanoseconds(), startNanoseconds)
		progress := new(big.Int).Sub(zoned.instant.epochNanoseconds(), startNanoseconds)
		rounded := roundTemporalBigIntAsIfPositive(progress, dayLength, mode)
		rounded.Add(rounded, startNanoseconds)
		instant, ok := temporalInstantFromEpochNanoseconds(rounded)
		if !ok {
			return temporalInstant{}, r.throwRangeError("rounded zoned date-time is outside the Temporal range")
		}
		return instant, nil
	}

	dateTime := temporalPlainDateTime{temporalISODateTime: local, calendar: zoned.calendar}
	step := temporalUnitNanoseconds[smallest] * increment
	rounded, err := r.roundTemporalPlainDateTime(dateTime, step, mode)
	if err != nil {
		return temporalInstant{}, err
	}
	return r.interpretTemporalZonedDateTime(
		rounded.temporalISODateTime,
		zoned,
		true,
		int64(zoned.offsetSeconds())*temporalNanosecondsPerSecond,
		false,
		false,
		temporalZonedDateTimeOptions{
			disambiguation: "compatible",
			offset:         "prefer",
			overflow:       "constrain",
		},
	)
}
