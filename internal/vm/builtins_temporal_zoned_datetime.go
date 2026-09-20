package vm

import (
	"math"
	"math/big"
	"strings"

	"github.com/go-quickjs/go-quickjs/internal/icu"
)

type temporalZonedDateTimeOptions struct {
	disambiguation string
	offset         string
	overflow       string
}

type temporalZonedDateTimeStringFormat struct {
	precision  int
	minuteOnly bool
	step       int64
	mode       string
	calendar   string
	timeZone   string
	offset     string
}

func (r *Runtime) temporalZonedDateTimeStringOptions(value Value) (temporalZonedDateTimeStringFormat, error) {
	options, err := r.strictOptions(value)
	if err != nil {
		return temporalZonedDateTimeStringFormat{}, err
	}
	format := temporalZonedDateTimeStringFormat{precision: -1, step: 1}
	format.calendar, err = r.stringOption(options, "calendarName", "auto", "auto", "always", "never", "critical")
	if err != nil {
		return temporalZonedDateTimeStringFormat{}, err
	}
	fractional, err := r.getProp(options, r.atoms.intern("fractionalSecondDigits"), Obj(options))
	if err != nil {
		return temporalZonedDateTimeStringFormat{}, err
	}
	if !fractional.IsUndefined() {
		if fractional.IsNumber() {
			digits := math.Floor(fractional.Number())
			if math.IsNaN(digits) || math.IsInf(digits, 0) || digits < 0 || digits > 9 {
				return temporalZonedDateTimeStringFormat{}, r.throwRangeError("fractionalSecondDigits is out of range")
			}
			format.precision = int(digits)
		} else {
			text, err := r.toString(fractional)
			if err != nil {
				return temporalZonedDateTimeStringFormat{}, err
			}
			if text.Go() != "auto" {
				return temporalZonedDateTimeStringFormat{}, r.throwRangeError("invalid fractionalSecondDigits")
			}
		}
	}
	format.offset, err = r.stringOption(options, "offset", "auto", "auto", "never")
	if err != nil {
		return temporalZonedDateTimeStringFormat{}, err
	}
	format.mode, err = r.stringOption(options, "roundingMode", "trunc",
		"ceil", "floor", "expand", "trunc", "halfCeil", "halfFloor", "halfExpand", "halfTrunc", "halfEven")
	if err != nil {
		return temporalZonedDateTimeStringFormat{}, err
	}
	smallestValue, err := r.getProp(options, r.atoms.intern("smallestUnit"), Obj(options))
	if err != nil {
		return temporalZonedDateTimeStringFormat{}, err
	}
	smallestRaw := ""
	if !smallestValue.IsUndefined() {
		text, err := r.toString(smallestValue)
		if err != nil {
			return temporalZonedDateTimeStringFormat{}, err
		}
		smallestRaw = text.Go()
	}
	format.timeZone, err = r.stringOption(options, "timeZoneName", "auto", "auto", "never", "critical")
	if err != nil {
		return temporalZonedDateTimeStringFormat{}, err
	}

	if smallestRaw != "" {
		smallest, ok := normalizeTemporalUnit(smallestRaw)
		if !ok || smallest == "hour" {
			return temporalZonedDateTimeStringFormat{}, r.throwRangeError("invalid smallestUnit")
		}
		format.step = temporalUnitNanoseconds[smallest]
		switch smallest {
		case "minute":
			format.minuteOnly = true
		case "second":
			format.precision = 0
		case "millisecond":
			format.precision = 3
		case "microsecond":
			format.precision = 6
		case "nanosecond":
			format.precision = 9
		}
		return format, nil
	}
	if format.precision >= 0 {
		steps := [...]int64{1_000_000_000, 100_000_000, 10_000_000, 1_000_000, 100_000, 10_000, 1_000, 100, 10, 1}
		format.step = steps[format.precision]
	}
	return format, nil
}

func (z *temporalZonedDateTime) stringWithOptions(format temporalZonedDateTimeStringFormat) string {
	dateTime := temporalPlainDateTime{temporalISODateTime: z.localISODateTime(), calendar: z.calendar}
	var b strings.Builder
	b.WriteString(dateTime.stringWithPrecision("never", format.precision, format.minuteOnly))
	if format.offset == "auto" {
		b.WriteString(formatTemporalOffsetRounded(z.offsetSeconds()))
	}
	if format.timeZone != "never" {
		b.WriteByte('[')
		if format.timeZone == "critical" {
			b.WriteByte('!')
		}
		b.WriteString(z.timeZone)
		b.WriteByte(']')
	}
	if format.calendar == "always" || format.calendar == "critical" || format.calendar == "auto" && z.calendar != "iso8601" {
		b.WriteByte('[')
		if format.calendar == "critical" {
			b.WriteByte('!')
		}
		b.WriteString("u-ca=")
		b.WriteString(z.calendar)
		b.WriteByte(']')
	}
	return b.String()
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
	instant, err := r.interpretTemporalZonedDateTime(parsed.dateTime, zoned, parsed.offsetPresent,
		parsed.offsetNanoseconds, parsed.offsetMatchMinutes, parsed.exact, parsed.startOfDay, options)
	if err != nil {
		return nil, err
	}
	zoned.instant = instant
	return zoned, nil
}

type temporalZonedDateTimeFields struct {
	dateTime           temporalISODateTime
	calendar           string
	zone               string
	offsetNanoseconds  int64
	offsetPresent      bool
	offsetMatchMinutes bool
	exact              bool
	startOfDay         bool
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
		parsed.offsetMatchMinutes = temporalStringOffsetUsesMinutes(main, timeStart)
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
	dateFields := temporalPartialDateFields{}
	timeValues := make(map[string]int, 6)
	offsetNanoseconds, offsetPresent := int64(0), false
	var timeZoneValue Value
	names := []string{"day"}
	if temporalCalendarUsesEra(calendar) {
		names = append(names, "era", "eraYear")
	}
	names = append(names, "hour", "microsecond", "millisecond", "minute",
		"month", "monthCode", "nanosecond", "offset", "second",
		"timeZone", "year")
	for _, name := range names {
		raw, err := r.getProp(o, r.atoms.intern(name), Obj(o))
		if err != nil {
			return nil, err
		}
		switch name {
		case "era":
			if raw.IsUndefined() {
				continue
			}
			text, err := r.toString(raw)
			if err != nil {
				return nil, err
			}
			dateFields.era = asciiLower(text.Go())
			dateFields.eraPresent = true
			if _, valid := temporalYearFromEra(calendar, dateFields.era, 1); !valid {
				return nil, r.throwRangeError("invalid calendar era")
			}
			continue
		case "monthCode":
			if raw.IsUndefined() {
				continue
			}
			dateFields.monthCode, err = r.temporalMonthCodeString(raw)
			if err != nil {
				return nil, err
			}
			dateFields.monthCodePresent = true
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
		switch name {
		case "day":
			dateFields.day, dateFields.dayPresent = value, true
		case "eraYear":
			dateFields.eraYear, dateFields.eraYearPresent = value, true
		case "month":
			dateFields.month, dateFields.monthPresent = value, true
		case "year":
			dateFields.year, dateFields.yearPresent = value, true
		default:
			timeValues[name] = value
		}
	}

	options, err := r.temporalZonedDateTimeOptions(optionsValue)
	if err != nil {
		return nil, err
	}
	if dateFields.eraPresent != dateFields.eraYearPresent {
		return nil, r.throwTypeError("era and eraYear must be provided together")
	}
	year, yearPresent := dateFields.year, dateFields.yearPresent
	if dateFields.eraPresent {
		eraYear, _ := temporalYearFromEra(
			calendar, dateFields.era, dateFields.eraYear)
		if yearPresent && year != eraYear {
			return nil, r.throwRangeError("year and eraYear do not agree")
		}
		year, yearPresent = eraYear, true
	}
	if !yearPresent || !dateFields.dayPresent ||
		!dateFields.monthPresent && !dateFields.monthCodePresent ||
		timeZoneValue.IsUndefined() {
		return nil, r.throwTypeError("zoned date-time property bag is missing required fields")
	}
	date, err := r.resolveTemporalMonthDayFieldsDate(calendar, year,
		dateFields, options.overflow == "constrain")
	if err != nil {
		return nil, err
	}
	if options.overflow == "constrain" {
		timeValues["hour"] = max(0, min(23, timeValues["hour"]))
		timeValues["minute"] = max(0, min(59, timeValues["minute"]))
		timeValues["second"] = max(0, min(59, timeValues["second"]))
		timeValues["millisecond"] = max(0, min(999, timeValues["millisecond"]))
		timeValues["microsecond"] = max(0, min(999, timeValues["microsecond"]))
		timeValues["nanosecond"] = max(0, min(999, timeValues["nanosecond"]))
	}
	dateTime := temporalISODateTime{
		year: date.year, month: date.month, day: date.day,
		hour: timeValues["hour"], minute: timeValues["minute"],
		second: timeValues["second"], millisecond: timeValues["millisecond"],
		microsecond: timeValues["microsecond"],
		nanosecond:  timeValues["nanosecond"],
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
	instant, err := r.interpretTemporalZonedDateTime(dateTime, zoned, offsetPresent,
		offsetNanoseconds, false, false, false, options)
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

	dateFields := temporalPartialDateFields{}
	timeValues := make(map[string]int, 6)
	timePresent := make(map[string]bool, 6)
	offsetText, offsetPresent := "", false
	names := []string{"day"}
	if temporalCalendarUsesEra(zoned.calendar) {
		names = append(names, "era", "eraYear")
	}
	names = append(names, "hour", "microsecond", "millisecond", "minute",
		"month", "monthCode", "nanosecond", "offset", "second", "year")
	for _, name := range names {
		raw, err := r.getProp(o, r.atoms.intern(name), fieldsValue)
		if err != nil {
			return nil, err
		}
		if raw.IsUndefined() {
			continue
		}
		switch name {
		case "era":
			text, err := r.toString(raw)
			if err != nil {
				return nil, err
			}
			dateFields.era = asciiLower(text.Go())
			dateFields.eraPresent = true
			if _, valid := temporalYearFromEra(
				zoned.calendar, dateFields.era, 1); !valid {
				return nil, r.throwRangeError("invalid calendar era")
			}
		case "monthCode":
			dateFields.monthCode, err = r.temporalMonthCodeString(raw)
			if err != nil {
				return nil, err
			}
			dateFields.monthCodePresent = true
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
			switch name {
			case "day":
				dateFields.day, dateFields.dayPresent = value, true
			case "eraYear":
				dateFields.eraYear, dateFields.eraYearPresent = value, true
			case "month":
				dateFields.month, dateFields.monthPresent = value, true
			case "year":
				dateFields.year, dateFields.yearPresent = value, true
			default:
				timeValues[name], timePresent[name] = value, true
			}
		}
	}
	if dateFields.monthPresent && dateFields.month < 1 ||
		dateFields.dayPresent && dateFields.day < 1 {
		return nil, r.throwRangeError("invalid Temporal.ZonedDateTime fields")
	}
	options, err := r.temporalZonedDateTimeOptionsWithOffsetDefault(optionsValue, "prefer")
	if err != nil {
		return nil, err
	}
	if !dateFields.dayPresent && !dateFields.eraPresent &&
		!dateFields.eraYearPresent && !dateFields.monthPresent &&
		!dateFields.monthCodePresent && !dateFields.yearPresent &&
		len(timePresent) == 0 && !offsetPresent {
		return nil, r.throwTypeError("zoned date-time fields contain no recognized properties")
	}
	if dateFields.eraPresent != dateFields.eraYearPresent {
		return nil, r.throwTypeError("era and eraYear must be provided together")
	}
	if dateFields.eraPresent && dateFields.yearPresent {
		year, _ := temporalYearFromEra(
			zoned.calendar, dateFields.era, dateFields.eraYear)
		if year != dateFields.year {
			return nil, r.throwRangeError("year and eraYear do not agree")
		}
	}

	local := zoned.localISODateTime()
	date, err := r.replaceTemporalPlainDateFields(temporalPlainDate{
		year: local.year, month: local.month, day: local.day,
		calendar: zoned.calendar,
	}, dateFields, options.overflow)
	if err != nil {
		return nil, err
	}
	local.year, local.month, local.day = date.year, date.month, date.day
	for _, field := range []struct {
		name   string
		target *int
		limit  int
	}{
		{"hour", &local.hour, 23}, {"minute", &local.minute, 59},
		{"second", &local.second, 59}, {"millisecond", &local.millisecond, 999},
		{"microsecond", &local.microsecond, 999}, {"nanosecond", &local.nanosecond, 999},
	} {
		if !timePresent[field.name] {
			continue
		}
		value := timeValues[field.name]
		if options.overflow == "constrain" {
			value = max(0, min(field.limit, value))
		}
		*field.target = value
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
	instant, err := r.interpretTemporalZonedDateTime(local, zoned, true,
		offsetNanoseconds, false, false, false, options)
	if err != nil {
		return nil, err
	}
	result := *zoned
	result.instant = instant
	return &result, nil
}

func (r *Runtime) interpretTemporalZonedDateTime(dateTime temporalISODateTime,
	zoned *temporalZonedDateTime, offsetPresent bool, offsetNanoseconds int64,
	offsetMatchMinutes, exact, startOfDay bool,
	options temporalZonedDateTimeOptions) (temporalInstant, error) {
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
		if options.offset == "use" {
			if !ok {
				return temporalInstant{}, r.throwRangeError(
					"zoned date-time is outside the Temporal range")
			}
			return candidate, nil
		}
		if offsetMatchMinutes {
			if matched, found := temporalMinuteOffsetCandidate(dateTime, zoned, offsetNanoseconds); found {
				return matched, nil
			}
		} else if ok {
			probe := *zoned
			probe.instant = candidate
			if int64(probe.offsetSeconds())*temporalNanosecondsPerSecond == offsetNanoseconds {
				return candidate, nil
			}
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

func temporalMinuteOffsetCandidate(dateTime temporalISODateTime,
	zoned *temporalZonedDateTime, offsetNanoseconds int64) (temporalInstant, bool) {
	if zoned.fixed {
		if !temporalOffsetMatchesMinutes(offsetNanoseconds, zoned.fixedOffsetSeconds) {
			return temporalInstant{}, false
		}
		return zoned.disambiguatedInstant(dateTime, "compatible")
	}
	for _, epochSeconds := range zoned.zone.PossibleInstants(dateTime.localEpochSeconds()) {
		actual := zoned.zone.OffsetAt(epochSeconds).OffsetSeconds
		if !temporalOffsetMatchesMinutes(offsetNanoseconds, actual) {
			continue
		}
		total := new(big.Int).Mul(big.NewInt(epochSeconds), big.NewInt(temporalNanosecondsPerSecond))
		total.Add(total, big.NewInt(dateTime.subsecondNanoseconds()))
		instant, ok := temporalInstantFromEpochNanoseconds(total)
		if ok {
			return instant, true
		}
	}
	return temporalInstant{}, false
}

func temporalOffsetMatchesMinutes(offsetNanoseconds int64, actualSeconds int) bool {
	actual := int64(actualSeconds)
	sign := int64(1)
	if actual < 0 {
		sign, actual = -1, -actual
	}
	minutes := actual / 60
	if actual%60 >= 30 {
		minutes++
	}
	return offsetNanoseconds == sign*minutes*60*temporalNanosecondsPerSecond
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
		if progress.Sign() < 0 || progress.Cmp(dayLength) > 0 {
			// A backward transition can cross midnight and re-enter the
			// preceding date after the following date has already begun. In
			// that case the instant is outside this date's start-to-start
			// interval, but rounding still follows its local wall-clock time.
			progress = temporalPlainTimeNanoseconds(temporalPlainTime{
				local.hour, local.minute, local.second,
				local.millisecond, local.microsecond, local.nanosecond,
			})
			dayLength = big.NewInt(
				temporalSecondsPerDay * temporalNanosecondsPerSecond)
		}
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
		false,
		temporalZonedDateTimeOptions{
			disambiguation: "compatible",
			offset:         "prefer",
			overflow:       "constrain",
		},
	)
}

func (r *Runtime) temporalZonedDateTimeDifferenceOptions(value Value) (largest, smallest string, increment int64, mode string, err error) {
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
		largest = "hour"
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

func (r *Runtime) differenceTemporalZonedDateTimes(start, end *temporalZonedDateTime, largest, smallest string, increment int64, mode string) (temporalDuration, error) {
	if temporalDateTimeUnitRank[largest] >= temporalDateTimeUnitRank["hour"] {
		difference := new(big.Int).Sub(end.instant.epochNanoseconds(), start.instant.epochNanoseconds())
		step := new(big.Int).Mul(big.NewInt(temporalUnitNanoseconds[smallest]), big.NewInt(increment))
		difference = roundTemporalBigInt(difference, step, mode)
		return temporalDurationFromNanoseconds(difference, largest), nil
	}

	duration, anchor, err := r.differenceTemporalZonedDateTimesUnrounded(start, end, largest)
	if err != nil {
		return temporalDuration{}, err
	}
	if temporalDateTimeUnitRank[smallest] >= temporalDateTimeUnitRank["hour"] {
		remainder := new(big.Int).Sub(end.instant.epochNanoseconds(), anchor.epochNanoseconds())
		step := new(big.Int).Mul(big.NewInt(temporalUnitNanoseconds[smallest]), big.NewInt(increment))
		remainder = roundTemporalBigInt(remainder, step, mode)
		target, ok := anchor.addNanoseconds(remainder)
		if !ok {
			return temporalDuration{}, r.throwRangeError("rounded zoned date-time is outside the Temporal range")
		}
		return r.differenceTemporalZonedDateTimesUnroundedResult(start, target, largest)
	}
	return r.roundTemporalZonedDifferenceToCalendarUnit(start, end.instant, duration, largest, smallest, increment, mode)
}

func (r *Runtime) differenceTemporalZonedDateTimesUnroundedResult(start *temporalZonedDateTime, end temporalInstant, largest string) (temporalDuration, error) {
	target := *start
	target.instant = end
	duration, _, err := r.differenceTemporalZonedDateTimesUnrounded(start, &target, largest)
	return duration, err
}

func (r *Runtime) differenceTemporalZonedDateTimesUnrounded(start, end *temporalZonedDateTime, largest string) (temporalDuration, temporalInstant, error) {
	comparison := compareTemporalInstants(start.instant, end.instant)
	if comparison == 0 {
		return temporalDuration{}, start.instant, nil
	}
	direction := int64(-comparison)
	startLocal := start.localISODateTime()
	endLocal := end.localISODateTime()
	startDate := temporalPlainDate{year: startLocal.year, month: startLocal.month, day: startLocal.day, calendar: start.calendar}
	startDays := isoDaysFromCivil(int64(startLocal.year), startLocal.month, startLocal.day)
	endDays := isoDaysFromCivil(int64(endLocal.year), endLocal.month, endLocal.day)
	startTime := temporalPlainTime{startLocal.hour, startLocal.minute, startLocal.second, startLocal.millisecond, startLocal.microsecond, startLocal.nanosecond}
	endTime := temporalPlainTime{endLocal.hour, endLocal.minute, endLocal.second, endLocal.millisecond, endLocal.microsecond, endLocal.nanosecond}
	if endDays != startDays && direction > 0 && compareTemporalPlainTimes(endTime, startTime) < 0 {
		endDays--
	} else if endDays != startDays && direction < 0 && compareTemporalPlainTimes(endTime, startTime) > 0 {
		endDays++
	}

	makeAnchor := func(days int64) (temporalDuration, temporalInstant, error) {
		year, month, day := isoCivilFromDays(days)
		endDate := temporalPlainDate{year: year, month: month, day: day, calendar: end.calendar}
		dateDuration, err := r.differenceTemporalPlainDates(
			startDate, endDate, largest, "day", 1, "trunc")
		if err != nil {
			return temporalDuration{}, temporalInstant{}, err
		}
		anchor, err := r.addTemporalDurationToZonedInstant(start, dateDuration)
		return dateDuration, anchor, err
	}
	dateDuration, anchor, err := makeAnchor(endDays)
	if err != nil {
		endDays -= direction
		dateDuration, anchor, err = makeAnchor(endDays)
		if err != nil {
			return temporalDuration{}, temporalInstant{}, err
		}
	}
	anchorComparison := compareTemporalInstants(anchor, end.instant)
	if direction > 0 && anchorComparison > 0 || direction < 0 && anchorComparison < 0 {
		endDays -= direction
		dateDuration, anchor, err = makeAnchor(endDays)
		if err != nil {
			return temporalDuration{}, temporalInstant{}, err
		}
	}
	remainder := new(big.Int).Sub(end.instant.epochNanoseconds(), anchor.epochNanoseconds())
	timeDuration := temporalDurationFromNanoseconds(remainder, "hour")
	dateDuration.hours, dateDuration.minutes, dateDuration.seconds = timeDuration.hours, timeDuration.minutes, timeDuration.seconds
	dateDuration.milliseconds, dateDuration.microseconds, dateDuration.nanoseconds = timeDuration.milliseconds, timeDuration.microseconds, timeDuration.nanoseconds
	return dateDuration, anchor, nil
}

func (r *Runtime) roundTemporalZonedDifferenceToCalendarUnit(start *temporalZonedDateTime, target temporalInstant, duration temporalDuration, largest, unit string, increment int64, mode string) (temporalDuration, error) {
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
	case "day":
		amount = int64(duration.days)
	}
	r1 := amount / increment * increment
	r2 := r1 + increment*sign
	candidate := func(amount int64) (temporalDuration, temporalInstant, error) {
		var value temporalDuration
		switch unit {
		case "year":
			value.years = float64(amount)
		case "month":
			value.years, value.months = duration.years, float64(amount)
		case "week":
			value.years, value.months, value.weeks = duration.years, duration.months, float64(amount)
		case "day":
			value.years, value.months, value.weeks, value.days = duration.years, duration.months, duration.weeks, float64(amount)
		}
		instant, err := r.addTemporalDurationToZonedInstant(start, value)
		return value, instant, err
	}
	lowerDuration, lowerInstant, err := candidate(r1)
	if err != nil {
		return temporalDuration{}, err
	}
	upperDuration, upperInstant, err := candidate(r2)
	if err != nil {
		return temporalDuration{}, err
	}
	between := func() bool {
		if sign > 0 {
			return compareTemporalInstants(lowerInstant, target) <= 0 && compareTemporalInstants(target, upperInstant) <= 0
		}
		return compareTemporalInstants(upperInstant, target) <= 0 && compareTemporalInstants(target, lowerInstant) <= 0
	}
	if !between() {
		r1, r2 = r2, r2+increment*sign
		lowerDuration, lowerInstant, err = candidate(r1)
		if err != nil {
			return temporalDuration{}, err
		}
		upperDuration, upperInstant, err = candidate(r2)
		if err != nil {
			return temporalDuration{}, err
		}
	}
	chooseUpper := compareTemporalInstants(target, upperInstant) == 0
	if compareTemporalInstants(target, lowerInstant) != 0 && !chooseUpper {
		switch mode {
		case "expand":
			chooseUpper = true
		case "ceil":
			chooseUpper = sign > 0
		case "floor":
			chooseUpper = sign < 0
		case "halfCeil", "halfFloor", "halfExpand", "halfTrunc", "halfEven":
			fromLower := new(big.Int).Abs(new(big.Int).Sub(target.epochNanoseconds(), lowerInstant.epochNanoseconds()))
			toUpper := new(big.Int).Abs(new(big.Int).Sub(upperInstant.epochNanoseconds(), target.epochNanoseconds()))
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
	result, resultInstant := lowerDuration, lowerInstant
	if chooseUpper {
		result, resultInstant = upperDuration, upperInstant
	}
	if unit == "week" || unit == "day" {
		return result, nil
	}
	return r.differenceTemporalZonedDateTimesUnroundedResult(start, resultInstant, largest)
}
