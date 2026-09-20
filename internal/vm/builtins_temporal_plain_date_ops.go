package vm

import (
	"math"
	"math/big"
	"strings"

	"github.com/go-quickjs/go-quickjs/internal/icu"
)

func (r *Runtime) initTemporalPlainDateOperations(proto *Object) {
	r.defMethod(proto, "withCalendar", 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
		date, err := rt.temporalPlainDateValue(this, "Temporal.PlainDate.prototype.withCalendar")
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
		date.calendar = calendar
		return Obj(newTemporalPlainDate(rt.temporalPlainDateProto, date)), nil
	})

	r.defMethod(proto, "with", 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
		date, err := rt.temporalPlainDateValue(this, "Temporal.PlainDate.prototype.with")
		if err != nil {
			return Undefined, err
		}
		fields, err := rt.temporalPartialObject(arg(args, 0))
		if err != nil {
			return Undefined, err
		}
		partial, err := rt.temporalPartialDateFields(fields, date.calendar)
		if err != nil {
			return Undefined, err
		}
		if partial.eraPresent != partial.eraYearPresent {
			return Undefined, rt.throwTypeError("era and eraYear must be provided together")
		}
		if !partial.dayPresent && !partial.eraPresent && !partial.eraYearPresent &&
			!partial.monthPresent && !partial.monthCodePresent && !partial.yearPresent {
			return Undefined, rt.throwTypeError("date fields must not be empty")
		}
		if partial.dayPresent && partial.day < 1 || partial.monthPresent && partial.month < 1 {
			return Undefined, rt.throwRangeError("invalid Temporal.PlainDate fields")
		}
		overflow, err := rt.temporalOverflowOption(arg(args, 1))
		if err != nil {
			return Undefined, err
		}
		result, err := rt.replaceTemporalPlainDateFields(date, partial, overflow)
		if err != nil {
			return Undefined, err
		}
		return Obj(newTemporalPlainDate(rt.temporalPlainDateProto, result)), nil
	})

	for _, operation := range []struct {
		name string
		sign int64
	}{{"add", 1}, {"subtract", -1}} {
		op := operation
		r.defMethod(proto, op.name, 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
			date, err := rt.temporalPlainDateValue(this, "Temporal.PlainDate.prototype."+op.name)
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
			result, err := rt.addTemporalPlainDate(date, duration, op.sign, overflow)
			if err != nil {
				return Undefined, err
			}
			return Obj(newTemporalPlainDate(rt.temporalPlainDateProto, result)), nil
		})
	}

	for _, operation := range []struct {
		name  string
		since bool
	}{{"until", false}, {"since", true}} {
		op := operation
		r.defMethod(proto, op.name, 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
			date, err := rt.temporalPlainDateValue(this, "Temporal.PlainDate.prototype."+op.name)
			if err != nil {
				return Undefined, err
			}
			other, err := rt.toTemporalPlainDate(arg(args, 0), Undefined)
			if err != nil {
				return Undefined, err
			}
			largest, smallest, increment, mode, err := rt.temporalPlainDateDifferenceOptions(arg(args, 1))
			if err != nil {
				return Undefined, err
			}
			if date.calendar != other.calendar {
				return Undefined, rt.throwRangeError("date calendars must match")
			}
			if op.since {
				mode = negateTemporalRoundingMode(mode)
			}
			duration, err := rt.differenceTemporalPlainDates(date, other, largest, smallest, increment, mode)
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
}

func (r *Runtime) replaceTemporalPlainDateFields(date temporalPlainDate,
	partial temporalPartialDateFields, overflow string) (temporalPlainDate, error) {
	calendarDate := date.calendarDate()
	year := calendarDate.ArithmeticYear
	if partial.eraPresent {
		year, _ = temporalYearFromEra(date.calendar, partial.era, partial.eraYear)
	} else if partial.yearPresent {
		year = partial.year
	}
	day := calendarDate.Day
	if partial.dayPresent {
		day = partial.day
	}
	month, leap := calendarDate.MonthCode()
	byCode := true
	if partial.monthPresent {
		month, leap, byCode = partial.month, false, false
	}
	if partial.monthCodePresent {
		month, leap, _ = parseTemporalMonthCode(partial.monthCode)
		byCode = true
	}
	isoYear, isoMonth, isoDay, ok := icu.ResolveDate(date.calendar, year,
		month, day, leap, byCode, overflow == "constrain")
	if !ok && byCode && leap && overflow == "constrain" &&
		!partial.monthCodePresent {
		if date.calendar == "hebrew" {
			month = 6
		}
		isoYear, isoMonth, isoDay, ok = icu.ResolveDate(date.calendar, year,
			month, day, false, true, true)
	}
	if !ok {
		return temporalPlainDate{}, r.throwRangeError("invalid Temporal.PlainDate")
	}
	result := temporalPlainDate{
		year: isoYear, month: isoMonth, day: isoDay, calendar: date.calendar,
	}
	if partial.monthPresent && partial.monthCodePresent &&
		result.calendarDate().OrdinalMonth != partial.month {
		return temporalPlainDate{}, r.throwRangeError("month and monthCode do not agree")
	}
	if !result.valid() {
		return temporalPlainDate{}, r.throwRangeError("invalid Temporal.PlainDate")
	}
	return result, nil
}

type temporalPartialDateFields struct {
	day, month, year                      int
	dayPresent, monthPresent, yearPresent bool
	era, monthCode                        string
	eraPresent                            bool
	eraYear                               int
	eraYearPresent                        bool
	monthCodePresent                      bool
}

func (r *Runtime) temporalPartialDateFields(o *Object, calendar string) (temporalPartialDateFields, error) {
	var fields temporalPartialDateFields
	dayValue, err := r.getProp(o, r.atoms.intern("day"), Obj(o))
	if err != nil {
		return fields, err
	}
	fields.dayPresent = !dayValue.IsUndefined()
	if fields.dayPresent {
		fields.day, err = r.temporalTruncatedInteger(dayValue, "day")
		if err != nil {
			return fields, err
		}
	}
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

func (r *Runtime) addTemporalPlainDate(date temporalPlainDate, duration temporalDuration, sign int64, overflow string) (temporalPlainDate, error) {
	const calendarUnitLimit = 1 << 32
	for _, value := range []float64{duration.years, duration.months, duration.weeks} {
		if math.Abs(value) >= calendarUnitLimit {
			return temporalPlainDate{}, r.throwRangeError("duration calendar unit is out of range")
		}
	}
	calendarDate := date.calendarDate()
	years, months := int64(duration.years)*sign, int64(duration.months)*sign
	resultYear := int64(calendarDate.ArithmeticYear) + years
	if resultYear < math.MinInt32 || resultYear > math.MaxInt32 {
		return temporalPlainDate{}, r.throwRangeError("date is outside the Temporal range")
	}
	resultMonth := calendarDate.OrdinalMonth
	if years != 0 {
		monthCode, leap := calendarDate.MonthCode()
		year, month, day, ok := icu.ResolveDate(date.calendar, int(resultYear),
			monthCode, 1, leap, true, false)
		if !ok && leap && overflow == "constrain" {
			if date.calendar == "hebrew" {
				monthCode = 6
			}
			year, month, day, ok = icu.ResolveDate(date.calendar, int(resultYear),
				monthCode, 1, false, true, false)
		}
		if !ok {
			return temporalPlainDate{}, r.throwRangeError("date overflows the target year")
		}
		resolved := temporalPlainDate{
			year: year, month: month, day: day, calendar: date.calendar,
		}.calendarDate()
		resultMonth = resolved.OrdinalMonth
	}
	for months > 0 {
		monthsInYear, ok := icu.MonthsInYear(date.calendar, int(resultYear))
		if !ok {
			return temporalPlainDate{}, r.throwRangeError("date is outside the calendar range")
		}
		remaining := int64(monthsInYear - resultMonth)
		if months <= remaining {
			resultMonth += int(months)
			months = 0
			break
		}
		months -= remaining + 1
		resultYear++
		resultMonth = 1
		if resultYear > math.MaxInt32 {
			return temporalPlainDate{}, r.throwRangeError("date is outside the Temporal range")
		}
	}
	for months < 0 {
		before := int64(resultMonth - 1)
		if -months <= before {
			resultMonth -= int(-months)
			months = 0
			break
		}
		months += before + 1
		resultYear--
		if resultYear < math.MinInt32 {
			return temporalPlainDate{}, r.throwRangeError("date is outside the Temporal range")
		}
		monthsInYear, ok := icu.MonthsInYear(date.calendar, int(resultYear))
		if !ok {
			return temporalPlainDate{}, r.throwRangeError("date is outside the calendar range")
		}
		resultMonth = monthsInYear
	}
	year, month, day, ok := icu.ResolveDate(date.calendar, int(resultYear),
		resultMonth, calendarDate.Day, false, false, overflow == "constrain")
	if !ok {
		return temporalPlainDate{}, r.throwRangeError("date overflows the target month")
	}
	baseDays := isoDaysFromCivil(int64(year), month, day)
	deltaDays, ok := temporalDurationWholeDays(duration)
	if !ok {
		return temporalPlainDate{}, r.throwRangeError("duration time portion is out of range")
	}
	deltaDays.Mul(deltaDays, big.NewInt(sign))
	weeks := new(big.Int).Mul(floatIntegerBig(duration.weeks), big.NewInt(7*sign))
	deltaDays.Add(deltaDays, weeks)
	if !deltaDays.IsInt64() {
		return temporalPlainDate{}, r.throwRangeError("date is outside the Temporal range")
	}
	finalDays := baseDays + deltaDays.Int64()
	if finalDays < -100_000_001 || finalDays > 100_000_000 {
		return temporalPlainDate{}, r.throwRangeError("date is outside the Temporal range")
	}
	year, month, day = isoCivilFromDays(finalDays)
	return temporalPlainDate{year: year, month: month, day: day, calendar: date.calendar}, nil
}

func temporalDurationWholeDays(duration temporalDuration) (*big.Int, bool) {
	const maxSafeInteger = int64(9_007_199_254_740_991)
	total := new(big.Int).Mul(floatIntegerBig(duration.days), big.NewInt(temporalSecondsPerDay*temporalNanosecondsPerSecond))
	for _, field := range []struct {
		value float64
		scale int64
	}{
		{duration.hours, 3_600_000_000_000}, {duration.minutes, 60_000_000_000},
		{duration.seconds, 1_000_000_000}, {duration.milliseconds, 1_000_000},
		{duration.microseconds, 1_000}, {duration.nanoseconds, 1},
	} {
		part := new(big.Int).Mul(floatIntegerBig(field.value), big.NewInt(field.scale))
		total.Add(total, part)
	}
	limit := new(big.Int).Mul(big.NewInt(maxSafeInteger), big.NewInt(temporalNanosecondsPerSecond))
	limit.Add(limit, big.NewInt(temporalNanosecondsPerSecond-1))
	if new(big.Int).Abs(new(big.Int).Set(total)).Cmp(limit) > 0 {
		return nil, false
	}
	return total.Quo(total, big.NewInt(temporalSecondsPerDay*temporalNanosecondsPerSecond)), true
}

func floatIntegerBig(value float64) *big.Int {
	integer, _ := new(big.Float).SetFloat64(value).Int(nil)
	return integer
}

var temporalDateUnitRank = map[string]int{"year": 0, "month": 1, "week": 2, "day": 3}

func normalizeTemporalDateUnit(unit string) (string, bool) {
	unit = strings.TrimSuffix(unit, "s")
	_, ok := temporalDateUnitRank[unit]
	return unit, ok
}

func (r *Runtime) temporalPlainDateDifferenceOptions(value Value) (largest, smallest string, increment int64, mode string, err error) {
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
	smallestRaw, err := r.stringOption(options, "smallestUnit", "day")
	if err != nil {
		return "", "", 0, "", err
	}
	var ok bool
	smallest, ok = normalizeTemporalDateUnit(smallestRaw)
	if !ok {
		return "", "", 0, "", r.throwRangeError("invalid smallestUnit")
	}
	if largestRaw == "auto" {
		largest = "day"
		if temporalDateUnitRank[smallest] < temporalDateUnitRank[largest] {
			largest = smallest
		}
	} else {
		largest, ok = normalizeTemporalDateUnit(largestRaw)
		if !ok {
			return "", "", 0, "", r.throwRangeError("invalid largestUnit")
		}
	}
	if temporalDateUnitRank[largest] > temporalDateUnitRank[smallest] {
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

func negateTemporalRoundingMode(mode string) string {
	switch mode {
	case "ceil":
		return "floor"
	case "floor":
		return "ceil"
	case "halfCeil":
		return "halfFloor"
	case "halfFloor":
		return "halfCeil"
	default:
		return mode
	}
}

func (r *Runtime) differenceTemporalPlainDates(start, end temporalPlainDate, largest, smallest string, increment int64, mode string) (temporalDuration, error) {
	startDays := isoDaysFromCivil(int64(start.year), start.month, start.day)
	endDays := isoDaysFromCivil(int64(end.year), end.month, end.day)
	if startDays == endDays {
		return temporalDuration{}, nil
	}
	var result temporalDuration
	switch largest {
	case "day":
		result.days = float64(endDays - startDays)
	case "week":
		days := endDays - startDays
		result.weeks = float64(days / 7)
		result.days = float64(days % 7)
	case "month", "year":
		var err error
		result, err = r.differenceTemporalCalendarDates(start, end, largest)
		if err != nil {
			return temporalDuration{}, err
		}
	}
	if smallest == "day" && increment == 1 {
		return result, nil
	}
	return r.roundTemporalPlainDateDifference(start, end, result, largest, smallest, increment, mode)
}

func compareTemporalCalendarDates(one, two icu.Date) int64 {
	if one.ArithmeticYear != two.ArithmeticYear {
		if one.ArithmeticYear < two.ArithmeticYear {
			return -1
		}
		return 1
	}
	if one.OrdinalMonth != two.OrdinalMonth {
		if one.OrdinalMonth < two.OrdinalMonth {
			return -1
		}
		return 1
	}
	if one.Day < two.Day {
		return -1
	}
	if one.Day > two.Day {
		return 1
	}
	return 0
}

func compareTemporalMonthCodes(one, two icu.Date) int64 {
	oneMonth, oneLeap := one.MonthCode()
	twoMonth, twoLeap := two.MonthCode()
	oneKey, twoKey := oneMonth*2, twoMonth*2
	if oneLeap {
		oneKey++
	}
	if twoLeap {
		twoKey++
	}
	if oneKey < twoKey {
		return -1
	}
	if oneKey > twoKey {
		return 1
	}
	return 0
}

func (r *Runtime) resolveTemporalCalendarDate(calendar string, date icu.Date) (temporalPlainDate, error) {
	year, month, day, ok := icu.ResolveDate(calendar, date.ArithmeticYear,
		date.OrdinalMonth, date.Day, false, false, true)
	if !ok {
		return temporalPlainDate{}, r.throwRangeError("date is outside the calendar range")
	}
	return temporalPlainDate{year: year, month: month, day: day, calendar: calendar}, nil
}

func (r *Runtime) addTemporalCalendarMonth(calendar string, date icu.Date, sign int64) (icu.Date, error) {
	year, month := date.ArithmeticYear, date.OrdinalMonth
	if sign > 0 {
		monthsInYear, ok := icu.MonthsInYear(calendar, year)
		if !ok {
			return icu.Date{}, r.throwRangeError("date is outside the calendar range")
		}
		month++
		if month > monthsInYear {
			year, month = year+1, 1
		}
	} else {
		month--
		if month < 1 {
			year--
			monthsInYear, ok := icu.MonthsInYear(calendar, year)
			if !ok {
				return icu.Date{}, r.throwRangeError("date is outside the calendar range")
			}
			month = monthsInYear
		}
	}
	resolved, err := r.resolveTemporalCalendarDate(calendar, icu.Date{
		ArithmeticYear: year, OrdinalMonth: month, Day: date.Day,
	})
	if err != nil {
		return icu.Date{}, err
	}
	return resolved.calendarDate(), nil
}

func temporalCalendarMonthDistance(calendar string, one, two icu.Date) (int64, bool) {
	months, ok := icu.MonthsBetweenYears(calendar, one.ArithmeticYear, two.ArithmeticYear)
	if !ok {
		return 0, false
	}
	return months + int64(two.OrdinalMonth-one.OrdinalMonth), true
}

func (r *Runtime) differenceTemporalCalendarDates(start, end temporalPlainDate, largest string) (temporalDuration, error) {
	one, two := start.calendarDate(), end.calendarDate()
	sign := compareTemporalCalendarDates(two, one)
	if sign == 0 {
		return temporalDuration{}, nil
	}

	diffYears := int64(two.ArithmeticYear - one.ArithmeticYear)
	years := int64(0)
	if diffYears != 0 {
		inYearSign := compareTemporalMonthCodes(two, one)
		if inYearSign == 0 {
			if two.Day < one.Day {
				inYearSign = -1
			} else if two.Day > one.Day {
				inYearSign = 1
			}
		}
		years = diffYears
		if inYearSign*sign < 0 {
			years -= sign
		}
	}

	intermediate := start
	if years != 0 {
		var err error
		intermediate, err = r.addTemporalPlainDate(start,
			temporalDuration{years: float64(years)}, 1, "constrain")
		if err != nil {
			return temporalDuration{}, err
		}
		// Compare the candidate with the original day restored. A constrained
		// shorter month must not turn an overshooting year into a full year.
		conceptual := intermediate.calendarDate()
		conceptual.Day = one.Day
		if compareTemporalCalendarDates(two, conceptual)*sign < 0 {
			years -= sign
			intermediate, err = r.addTemporalPlainDate(start,
				temporalDuration{years: float64(years)}, 1, "constrain")
			if err != nil {
				return temporalDuration{}, err
			}
		}
	}
	months := int64(0)
	if largest == "month" {
		var ok bool
		months, ok = temporalCalendarMonthDistance(start.calendar, one,
			intermediate.calendarDate())
		if !ok {
			return temporalDuration{}, r.throwRangeError("unsupported calendar")
		}
		years = 0
	}

	current := intermediate.calendarDate()
	next := current
	for {
		months += sign
		current = next
		var err error
		next, err = r.addTemporalCalendarMonth(start.calendar, current, sign)
		if err != nil {
			return temporalDuration{}, err
		}
		if next.Day != one.Day {
			next.Day = one.Day
		}
		if compareTemporalCalendarDates(two, next)*sign < 0 {
			break
		}
	}
	months -= sign
	anchor, err := r.resolveTemporalCalendarDate(start.calendar, current)
	if err != nil {
		return temporalDuration{}, err
	}
	anchorDays := isoDaysFromCivil(int64(anchor.year), anchor.month, anchor.day)
	endDays := isoDaysFromCivil(int64(end.year), end.month, end.day)
	return temporalDuration{
		years:  float64(years),
		months: float64(months),
		days:   float64(endDays - anchorDays),
	}, nil
}

func (r *Runtime) roundTemporalPlainDateDifference(start, end temporalPlainDate, duration temporalDuration, largest, smallest string, increment int64, mode string) (temporalDuration, error) {
	if largest == smallest {
		startDays := isoDaysFromCivil(int64(start.year), start.month, start.day)
		endDays := isoDaysFromCivil(int64(end.year), end.month, end.day)
		switch smallest {
		case "day":
			rounded := roundTemporalBigInt(big.NewInt(endDays-startDays), big.NewInt(increment), mode).Int64()
			return temporalDuration{days: float64(rounded)}, nil
		case "week":
			step := new(big.Int).Mul(big.NewInt(increment), big.NewInt(7))
			roundedDays := roundTemporalBigInt(big.NewInt(endDays-startDays), step, mode).Int64()
			return temporalDuration{weeks: float64(roundedDays / 7)}, nil
		}
	}
	// Date-unit rounding is expressed through adjacent calendar candidates so
	// irregular month and year lengths are compared in actual ISO days.
	var larger temporalDuration
	switch smallest {
	case "day":
		larger.years, larger.months, larger.weeks = duration.years, duration.months, duration.weeks
	case "week":
		larger.years, larger.months = duration.years, duration.months
	case "month":
		larger.years = duration.years
	}
	anchor, err := r.addTemporalPlainDate(start, larger, 1, "constrain")
	if err != nil {
		return temporalDuration{}, err
	}
	anchorDays := isoDaysFromCivil(int64(anchor.year), anchor.month, anchor.day)
	endDays := isoDaysFromCivil(int64(end.year), end.month, end.day)
	var approximate int64
	switch smallest {
	case "day":
		approximate = endDays - anchorDays
	case "week":
		approximate = (endDays - anchorDays) / 7
	case "month":
		approximate = int64(end.year-anchor.year)*12 + int64(end.month-anchor.month)
	case "year":
		approximate = int64(end.year - anchor.year)
	}
	if smallest == "month" || smallest == "year" {
		probe := larger
		if smallest == "month" {
			probe.months = float64(approximate)
		} else {
			probe.years = float64(approximate)
		}
		probeDate, probeErr := r.addTemporalPlainDate(start, probe, 1, "constrain")
		if probeErr != nil {
			return temporalDuration{}, probeErr
		}
		probeDays := isoDaysFromCivil(int64(probeDate.year), probeDate.month, probeDate.day)
		if probeDays > endDays {
			approximate--
		}
	}
	lower := floorDivInt64(approximate, increment) * increment
	upper := lower + increment
	makeCandidate := func(amount int64) (temporalPlainDate, error) {
		d := larger
		switch smallest {
		case "day":
			d.days = float64(amount)
		case "week":
			d.weeks = float64(amount)
		case "month":
			d.months = float64(amount)
		case "year":
			d.years = float64(amount)
		}
		return r.addTemporalPlainDate(start, d, 1, "constrain")
	}
	lowerDate, err := makeCandidate(lower)
	if err != nil {
		return temporalDuration{}, err
	}
	upperDate, err := makeCandidate(upper)
	if err != nil {
		return temporalDuration{}, err
	}
	lowerDays := isoDaysFromCivil(int64(lowerDate.year), lowerDate.month, lowerDate.day)
	upperDays := isoDaysFromCivil(int64(upperDate.year), upperDate.month, upperDate.day)
	chosen := chooseTemporalDateRounding(lower, upper, lowerDays, upperDays, endDays, mode)
	chosenDate, err := makeCandidate(chosen)
	if err != nil {
		return temporalDuration{}, err
	}
	return r.differenceTemporalPlainDates(start, chosenDate, largest, "day", 1, "trunc")
}

func chooseTemporalDateRounding(lower, upper, lowerDays, upperDays, targetDays int64, mode string) int64 {
	if lower == upper || targetDays == lowerDays {
		return lower
	}
	if targetDays == upperDays {
		return upper
	}
	switch mode {
	case "ceil":
		return upper
	case "floor":
		return lower
	case "expand":
		if absInt64(upper) > absInt64(lower) {
			return upper
		}
		return lower
	case "trunc":
		if absInt64(lower) <= absInt64(upper) {
			return lower
		}
		return upper
	}
	lowerDistance := absInt64(targetDays - lowerDays)
	upperDistance := absInt64(upperDays - targetDays)
	if lowerDistance < upperDistance {
		return lower
	}
	if upperDistance < lowerDistance {
		return upper
	}
	switch mode {
	case "halfCeil":
		return upper
	case "halfFloor":
		return lower
	case "halfExpand":
		if absInt64(upper) > absInt64(lower) {
			return upper
		}
	case "halfEven":
		if (lower/(upper-lower))%2 == 0 {
			return lower
		}
		return upper
	}
	return lower
}

func absInt64(value int64) int64 {
	if value < 0 {
		return -value
	}
	return value
}
