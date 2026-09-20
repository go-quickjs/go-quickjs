package vm

import (
	"math"
	"math/big"
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
