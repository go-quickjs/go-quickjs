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
	endDays := isoDaysFromCivil(int64(endLocal.year), endLocal.month, endLocal.day)
	startTime := temporalPlainTime{startLocal.hour, startLocal.minute, startLocal.second, startLocal.millisecond, startLocal.microsecond, startLocal.nanosecond}
	endTime := temporalPlainTime{endLocal.hour, endLocal.minute, endLocal.second, endLocal.millisecond, endLocal.microsecond, endLocal.nanosecond}
	if direction > 0 && compareTemporalPlainTimes(endTime, startTime) < 0 {
		endDays--
	} else if direction < 0 && compareTemporalPlainTimes(endTime, startTime) > 0 {
		endDays++
	}

	makeAnchor := func(days int64) (temporalDuration, temporalInstant, error) {
		year, month, day := isoCivilFromDays(days)
		endDate := temporalPlainDate{year: year, month: month, day: day, calendar: end.calendar}
		dateDuration := differenceTemporalZonedDatePortion(startDate, endDate, largest)
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

func differenceTemporalZonedDatePortion(start, end temporalPlainDate, largest string) temporalDuration {
	startDays := isoDaysFromCivil(int64(start.year), start.month, start.day)
	endDays := isoDaysFromCivil(int64(end.year), end.month, end.day)
	var result temporalDuration
	switch largest {
	case "day":
		result.days = float64(endDays - startDays)
	case "week":
		days := endDays - startDays
		result.weeks = float64(days / 7)
		result.days = float64(days % 7)
	case "month", "year":
		months := int64(end.year-start.year)*12 + int64(end.month-start.month)
		anchorDays := temporalMonthAnchorDays(start, months)
		if months > 0 && anchorDays > endDays {
			months--
			anchorDays = temporalMonthAnchorDays(start, months)
		} else if months < 0 && anchorDays < endDays {
			months++
			anchorDays = temporalMonthAnchorDays(start, months)
		}
		if largest == "year" {
			result.years = float64(months / 12)
			result.months = float64(months % 12)
		} else {
			result.months = float64(months)
		}
		result.days = float64(endDays - anchorDays)
	}
	return result
}

func temporalMonthAnchorDays(start temporalPlainDate, months int64) int64 {
	totalMonths := int64(start.year)*12 + int64(start.month-1) + months
	year := floorDivInt64(totalMonths, 12)
	month := int(totalMonths-year*12) + 1
	day := min(start.day, isoDaysInMonth(int(year), month))
	return isoDaysFromCivil(year, month, day)
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
	if unit == "week" {
		return result, nil
	}
	return r.differenceTemporalZonedDateTimesUnroundedResult(start, resultInstant, largest)
}
