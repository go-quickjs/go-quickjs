package vm

import (
	"math"
	"math/big"
	"strings"
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
		partial, err := rt.temporalPartialDateFields(fields)
		if err != nil {
			return Undefined, err
		}
		if !partial.dayPresent && !partial.monthPresent && !partial.monthCodePresent && !partial.yearPresent {
			return Undefined, rt.throwTypeError("date fields must not be empty")
		}
		if partial.dayPresent && partial.day < 1 || partial.monthPresent && partial.month < 1 {
			return Undefined, rt.throwRangeError("invalid Temporal.PlainDate fields")
		}
		overflow, err := rt.temporalOverflowOption(arg(args, 1))
		if err != nil {
			return Undefined, err
		}
		year := date.year
		if partial.yearPresent {
			year = partial.year
		}
		month := date.month
		if partial.monthPresent || partial.monthCodePresent {
			month, err = rt.resolveTemporalISOMonth(partial.month, partial.monthPresent, partial.monthCode, partial.monthCodePresent)
			if err != nil {
				return Undefined, err
			}
		}
		day := date.day
		if partial.dayPresent {
			day = partial.day
		}
		if overflow == "constrain" {
			month = min(month, 12)
			day = min(day, isoDaysInMonth(year, month))
		}
		result := temporalPlainDate{year: year, month: month, day: day, calendar: date.calendar}
		if !result.valid() {
			return Undefined, rt.throwRangeError("invalid Temporal.PlainDate")
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

type temporalPartialDateFields struct {
	day, month, year                      int
	dayPresent, monthPresent, yearPresent bool
	monthCode                             string
	monthCodePresent                      bool
}

func (r *Runtime) temporalPartialDateFields(o *Object) (temporalPartialDateFields, error) {
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
	years, months := int64(duration.years)*sign, int64(duration.months)*sign
	totalMonths := int64(date.year)*12 + int64(date.month-1) + years*12 + months
	resultYear := floorDivInt64(totalMonths, 12)
	resultMonth := totalMonths - resultYear*12 + 1
	if resultYear < math.MinInt32 || resultYear > math.MaxInt32 {
		return temporalPlainDate{}, r.throwRangeError("date is outside the Temporal range")
	}
	resultDay := date.day
	daysInMonth := isoDaysInMonth(int(resultYear), int(resultMonth))
	if resultDay > daysInMonth {
		if overflow == "reject" {
			return temporalPlainDate{}, r.throwRangeError("date overflows the target month")
		}
		resultDay = daysInMonth
	}
	baseDays := isoDaysFromCivil(resultYear, int(resultMonth), resultDay)
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
	year, month, day := isoCivilFromDays(finalDays)
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
		months := int64(end.year-start.year)*12 + int64(end.month-start.month)
		anchor, err := r.addTemporalPlainDate(start, temporalDuration{months: float64(months)}, 1, "constrain")
		if err != nil {
			return temporalDuration{}, err
		}
		anchorDays := isoDaysFromCivil(int64(anchor.year), anchor.month, anchor.day)
		if months > 0 && anchorDays > endDays {
			months--
			anchor, err = r.addTemporalPlainDate(start, temporalDuration{months: float64(months)}, 1, "constrain")
		} else if months < 0 && anchorDays < endDays {
			months++
			anchor, err = r.addTemporalPlainDate(start, temporalDuration{months: float64(months)}, 1, "constrain")
		}
		if err != nil {
			return temporalDuration{}, err
		}
		anchorDays = isoDaysFromCivil(int64(anchor.year), anchor.month, anchor.day)
		if largest == "year" {
			result.years = float64(months / 12)
			result.months = float64(months % 12)
		} else {
			result.months = float64(months)
		}
		result.days = float64(endDays - anchorDays)
	}
	if smallest == "day" && increment == 1 {
		return result, nil
	}
	return r.roundTemporalPlainDateDifference(start, end, result, largest, smallest, increment, mode)
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
