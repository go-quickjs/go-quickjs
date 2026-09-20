package vm

import (
	"math"
	"math/big"
	"strconv"
	"strings"
)

type temporalDuration struct {
	years, months, weeks, days              float64
	hours, minutes, seconds                 float64
	milliseconds, microseconds, nanoseconds float64
}

func (d temporalDuration) fields() [10]float64 {
	return [10]float64{d.years, d.months, d.weeks, d.days, d.hours, d.minutes, d.seconds, d.milliseconds, d.microseconds, d.nanoseconds}
}

func (d temporalDuration) sign() int {
	for _, value := range d.fields() {
		if value < 0 {
			return -1
		}
		if value > 0 {
			return 1
		}
	}
	return 0
}

func (d temporalDuration) valid() bool {
	sign := 0
	for _, value := range d.fields() {
		if value == 0 {
			continue
		}
		current := 1
		if value < 0 {
			current = -1
		}
		if sign != 0 && sign != current {
			return false
		}
		sign = current
	}
	return true
}

func (d temporalDuration) timeNanoseconds() (*big.Int, bool) {
	if d.years != 0 || d.months != 0 || d.weeks != 0 || d.days != 0 {
		return nil, false
	}
	return d.timePartNanoseconds(), true
}

func (d temporalDuration) timePartNanoseconds() *big.Int {
	total := new(big.Int)
	for _, unit := range []struct {
		value float64
		scale int64
	}{
		{d.hours, 3_600_000_000_000}, {d.minutes, 60_000_000_000}, {d.seconds, 1_000_000_000},
		{d.milliseconds, 1_000_000}, {d.microseconds, 1_000}, {d.nanoseconds, 1},
	} {
		integer, _ := new(big.Float).SetFloat64(unit.value).Int(nil)
		total.Add(total, new(big.Int).Mul(integer, big.NewInt(unit.scale)))
	}
	return total
}

func (d temporalDuration) dayAndTimeNanoseconds() *big.Int {
	total := new(big.Int).Mul(
		floatIntegerBig(d.days),
		big.NewInt(temporalSecondsPerDay*temporalNanosecondsPerSecond),
	)
	return total.Add(total, d.timePartNanoseconds())
}

func (d temporalDuration) withinRange() bool {
	const maxCalendarUnit = 4_294_967_295
	for _, value := range []float64{d.years, d.months, d.weeks} {
		if math.Abs(value) > maxCalendarUnit {
			return false
		}
	}

	total := new(big.Int).Mul(floatIntegerBig(d.days), big.NewInt(86_400_000_000_000))
	total.Add(total, d.timePartNanoseconds())
	max := new(big.Int).Mul(big.NewInt(9_007_199_254_740_991), big.NewInt(1_000_000_000))
	max.Add(max, big.NewInt(999_999_999))
	return new(big.Int).Abs(total).Cmp(max) <= 0
}

func (r *Runtime) initTemporalDuration(temporal *Object) {
	proto := newObject(r.proto.object, ClassObject)
	r.temporalDurationProto = proto
	r.newTemporalCtor(temporal, "Duration", 0, proto, func(rt *Runtime, this Value, args []Value) (Value, error) {
		if err := rt.requireNew("Temporal.Duration"); err != nil {
			return Undefined, err
		}
		var d temporalDuration
		fields := []*float64{&d.years, &d.months, &d.weeks, &d.days, &d.hours, &d.minutes, &d.seconds, &d.milliseconds, &d.microseconds, &d.nanoseconds}
		for i, target := range fields {
			value, err := rt.temporalInteger(arg(args, i))
			if err != nil {
				return Undefined, err
			}
			if value > 9_007_199_254_740_991 || value < -9_007_199_254_740_991 {
				return Undefined, rt.throwRangeError("Duration constructor fields must be safe integers")
			}
			*target = value
		}
		if !d.valid() {
			return Undefined, rt.throwRangeError("duration fields must have the same sign")
		}
		if !d.withinRange() {
			return Undefined, rt.throwRangeError("duration is out of range")
		}
		instanceProto, err := rt.protoFromNewTargetErr(proto)
		if err != nil {
			return Undefined, err
		}
		return Obj(newTemporalDuration(instanceProto, d)), nil
	})
	ctor, _ := r.getProp(temporal, r.atoms.intern("Duration"), Obj(temporal))
	r.defMethod(ctor.Object(), "from", 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
		d, err := rt.toTemporalDuration(arg(args, 0))
		if err != nil {
			return Undefined, err
		}
		return Obj(newTemporalDuration(proto, d)), nil
	})
	r.defMethod(ctor.Object(), "compare", 2, func(rt *Runtime, this Value, args []Value) (Value, error) {
		left, err := rt.toTemporalDuration(arg(args, 0))
		if err != nil {
			return Undefined, err
		}
		right, err := rt.toTemporalDuration(arg(args, 1))
		if err != nil {
			return Undefined, err
		}
		options, err := rt.strictOptions(arg(args, 2))
		if err != nil {
			return Undefined, err
		}
		relativeValue, err := rt.getProp(options, rt.atoms.intern("relativeTo"), Obj(options))
		if err != nil {
			return Undefined, err
		}
		var relativeTo temporalDurationRelativeTo
		if !relativeValue.IsUndefined() {
			relativeTo, err = rt.toTemporalDurationRelativeTo(relativeValue)
			if err != nil {
				return Undefined, err
			}
		}
		if left.fields() == right.fields() {
			return Int(0), nil
		}
		leftValue, err := rt.totalTemporalDurationNanoseconds(left, relativeTo, false)
		if err != nil {
			return Undefined, err
		}
		rightValue, err := rt.totalTemporalDurationNanoseconds(right, relativeTo, false)
		if err != nil {
			return Undefined, err
		}
		return Int(leftValue.Cmp(rightValue)), nil
	})
	for i, name := range []string{"years", "months", "weeks", "days", "hours", "minutes", "seconds", "milliseconds", "microseconds", "nanoseconds"} {
		index, property := i, name
		r.defGetter(proto, property, func(rt *Runtime, this Value, args []Value) (Value, error) {
			d, err := rt.temporalDurationValue(this, "get Temporal.Duration.prototype."+property)
			if err != nil {
				return Undefined, err
			}
			return Float(d.fields()[index]), nil
		})
	}
	r.defGetter(proto, "sign", func(rt *Runtime, this Value, args []Value) (Value, error) {
		d, err := rt.temporalDurationValue(this, "get Temporal.Duration.prototype.sign")
		if err != nil {
			return Undefined, err
		}
		return Int(d.sign()), nil
	})
	r.defGetter(proto, "blank", func(rt *Runtime, this Value, args []Value) (Value, error) {
		d, err := rt.temporalDurationValue(this, "get Temporal.Duration.prototype.blank")
		if err != nil {
			return Undefined, err
		}
		return Bool(d.sign() == 0), nil
	})
	r.defMethod(proto, "negated", 0, func(rt *Runtime, this Value, args []Value) (Value, error) {
		d, err := rt.temporalDurationValue(this, "Temporal.Duration.prototype.negated")
		if err != nil {
			return Undefined, err
		}
		values := d.fields()
		for i := range values {
			if values[i] != 0 {
				values[i] = -values[i]
			}
		}
		return Obj(newTemporalDuration(proto, durationFromFields(values))), nil
	})
	r.defMethod(proto, "abs", 0, func(rt *Runtime, this Value, args []Value) (Value, error) {
		d, err := rt.temporalDurationValue(this, "Temporal.Duration.prototype.abs")
		if err != nil {
			return Undefined, err
		}
		values := d.fields()
		for i := range values {
			if values[i] < 0 {
				values[i] = -values[i]
			}
		}
		return Obj(newTemporalDuration(proto, durationFromFields(values))), nil
	})
	for _, operation := range []struct {
		name string
		sign int
	}{{"add", 1}, {"subtract", -1}} {
		op := operation
		r.defMethod(proto, op.name, 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
			left, err := rt.temporalDurationValue(this, "Temporal.Duration.prototype."+op.name)
			if err != nil {
				return Undefined, err
			}
			right, err := rt.toTemporalDuration(arg(args, 0))
			if err != nil {
				return Undefined, err
			}
			if left.years != 0 || left.months != 0 || left.weeks != 0 ||
				right.years != 0 || right.months != 0 || right.weeks != 0 {
				return Undefined, rt.throwRangeError("cannot add calendar duration units")
			}
			leftNS := left.dayAndTimeNanoseconds()
			rightNS := right.dayAndTimeNanoseconds()
			if op.sign < 0 {
				rightNS.Neg(rightNS)
			}
			leftNS.Add(leftNS, rightNS)
			largest := ""
			switch {
			case left.days != 0 || right.days != 0:
				largest = "day"
			default:
				largest = left.largestTimeUnit()
				if other := right.largestTimeUnit(); temporalUnitRank[other] < temporalUnitRank[largest] {
					largest = other
				}
			}
			result := temporalDurationFromNanoseconds(leftNS, largest)
			if !result.withinRange() {
				return Undefined, rt.throwRangeError("duration result is out of range")
			}
			return Obj(newTemporalDuration(proto, result)), nil
		})
	}
	r.defMethod(proto, "with", 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
		duration, err := rt.temporalDurationValue(this, "Temporal.Duration.prototype.with")
		if err != nil {
			return Undefined, err
		}
		value := arg(args, 0)
		if !value.IsObject() {
			return Undefined, rt.throwTypeError("duration fields must be an object")
		}
		fields := duration.fields()
		found := false
		for _, field := range []struct {
			name  string
			index int
		}{
			{"days", 3}, {"hours", 4}, {"microseconds", 8}, {"milliseconds", 7}, {"minutes", 5},
			{"months", 1}, {"nanoseconds", 9}, {"seconds", 6}, {"weeks", 2}, {"years", 0},
		} {
			raw, err := rt.getProp(value.Object(), rt.atoms.intern(field.name), value)
			if err != nil {
				return Undefined, err
			}
			if raw.IsUndefined() {
				continue
			}
			found = true
			fields[field.index], err = rt.temporalInteger(raw)
			if err != nil {
				return Undefined, err
			}
		}
		if !found {
			return Undefined, rt.throwTypeError("duration property bag has no duration fields")
		}
		result := durationFromFields(fields)
		if !result.valid() {
			return Undefined, rt.throwRangeError("duration fields must have the same sign")
		}
		if !result.withinRange() {
			return Undefined, rt.throwRangeError("duration is out of range")
		}
		return Obj(newTemporalDuration(proto, result)), nil
	})
	r.defMethod(proto, "round", 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
		duration, err := rt.temporalDurationValue(this, "Temporal.Duration.prototype.round")
		if err != nil {
			return Undefined, err
		}
		largest, smallest, increment, mode, relativeTo, err := rt.temporalDurationRoundOptions(arg(args, 0), duration)
		if err != nil {
			return Undefined, err
		}
		result, err := rt.roundTemporalDuration(duration, largest, smallest, increment, mode, relativeTo)
		if err != nil {
			return Undefined, err
		}
		if !result.valid() || !result.withinRange() {
			return Undefined, rt.throwRangeError("rounded duration is out of range")
		}
		return Obj(newTemporalDuration(proto, result)), nil
	})
	r.defMethod(proto, "total", 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
		d, err := rt.temporalDurationValue(this, "Temporal.Duration.prototype.total")
		if err != nil {
			return Undefined, err
		}
		unit, relativeTo, err := rt.temporalTotalOptions(arg(args, 0))
		if err != nil {
			return Undefined, err
		}
		value, err := rt.totalTemporalDuration(d, relativeTo, unit)
		if err != nil {
			return Undefined, err
		}
		return Float(value), nil
	})
	r.defMethod(proto, "toString", 0, func(rt *Runtime, this Value, args []Value) (Value, error) {
		duration, err := rt.temporalDurationValue(this, "Temporal.Duration.prototype.toString")
		if err != nil {
			return Undefined, err
		}
		precision, _, step, mode, err := rt.temporalPlainTimeStringOptions(arg(args, 0))
		if err != nil {
			return Undefined, err
		}
		if step >= temporalUnitNanoseconds["minute"] {
			return Undefined, rt.throwRangeError("invalid smallestUnit")
		}
		if step != 1 {
			duration, err = roundTemporalDurationForString(duration, step, mode)
			if err != nil || !duration.withinRange() {
				return Undefined, rt.throwRangeError("rounded duration is out of range")
			}
		}
		return Str(NewString(duration.stringWithPrecision(precision))), nil
	})
	r.defMethod(proto, "toJSON", 0, func(rt *Runtime, this Value, args []Value) (Value, error) {
		duration, err := rt.temporalDurationValue(this, "Temporal.Duration.prototype.toJSON")
		if err != nil {
			return Undefined, err
		}
		return Str(NewString(duration.string())), nil
	})
	r.defMethod(proto, "toLocaleString", 0, func(rt *Runtime, this Value, args []Value) (Value, error) {
		duration, err := rt.temporalDurationValue(this, "Temporal.Duration.prototype.toLocaleString")
		if err != nil {
			return Undefined, err
		}
		return Str(NewString(duration.string())), nil
	})
	r.defMethod(proto, "valueOf", 0, func(rt *Runtime, this Value, args []Value) (Value, error) {
		if _, err := rt.temporalDurationValue(this, "Temporal.Duration.prototype.valueOf"); err != nil {
			return Undefined, err
		}
		return Undefined, rt.throwTypeError("durations cannot be converted to primitive values")
	})
	r.defToStringTag(proto, "Temporal.Duration")
}

type temporalDurationRelativeTo struct {
	plain *temporalPlainDateTime
	zoned *temporalZonedDateTime
}

func (d temporalDuration) largestUnit() string {
	for i, value := range d.fields() {
		if value != 0 {
			return []string{"year", "month", "week", "day", "hour", "minute", "second", "millisecond", "microsecond", "nanosecond"}[i]
		}
	}
	return "nanosecond"
}

func (r *Runtime) temporalDurationRoundOptions(value Value, duration temporalDuration) (largest, smallest string, increment int64, mode string, relativeTo temporalDurationRelativeTo, err error) {
	var options *Object
	if value.IsString() {
		options = newObject(nil, ClassObject)
		options.setOwnRaw(r.atoms.intern("smallestUnit"), value, propDefault)
	} else {
		if value.IsUndefined() {
			err = r.throwTypeError("options or a smallestUnit string is required")
			return
		}
		options, err = r.strictOptions(value)
		if err != nil {
			return
		}
	}

	largestValue, err := r.getProp(options, r.atoms.intern("largestUnit"), Obj(options))
	if err != nil {
		return "", "", 0, "", relativeTo, err
	}
	largestSet := !largestValue.IsUndefined()
	largestRaw := "auto"
	if largestSet {
		text, conversionErr := r.toString(largestValue)
		if conversionErr != nil {
			return "", "", 0, "", relativeTo, conversionErr
		}
		largestRaw = text.Go()
	}
	relativeValue, err := r.getProp(options, r.atoms.intern("relativeTo"), Obj(options))
	if err != nil {
		return "", "", 0, "", relativeTo, err
	}
	if !relativeValue.IsUndefined() {
		relativeTo, err = r.toTemporalDurationRelativeTo(relativeValue)
		if err != nil {
			return "", "", 0, "", relativeTo, err
		}
	}
	rawIncrement, incrementSet, err := r.rawNumberOption(options, "roundingIncrement")
	if err != nil {
		return "", "", 0, "", relativeTo, err
	}
	mode, err = r.stringOption(options, "roundingMode", "halfExpand",
		"ceil", "floor", "expand", "trunc", "halfCeil", "halfFloor", "halfExpand", "halfTrunc", "halfEven")
	if err != nil {
		return "", "", 0, "", relativeTo, err
	}
	smallestRaw, err := r.stringOption(options, "smallestUnit", "")
	if err != nil {
		return "", "", 0, "", relativeTo, err
	}
	if !largestSet && smallestRaw == "" {
		return "", "", 0, "", relativeTo, r.throwRangeError("largestUnit or smallestUnit is required")
	}
	var ok bool
	if smallestRaw == "" {
		smallest = "nanosecond"
	} else {
		smallest, ok = normalizeTemporalDateTimeUnit(smallestRaw)
		if !ok {
			return "", "", 0, "", relativeTo, r.throwRangeError("invalid smallestUnit")
		}
	}
	if largestRaw == "auto" {
		largest = duration.largestUnit()
		if temporalDateTimeUnitRank[smallest] < temporalDateTimeUnitRank[largest] {
			largest = smallest
		}
	} else {
		largest, ok = normalizeTemporalDateTimeUnit(largestRaw)
		if !ok {
			return "", "", 0, "", relativeTo, r.throwRangeError("invalid largestUnit")
		}
	}
	if temporalDateTimeUnitRank[largest] > temporalDateTimeUnitRank[smallest] {
		return "", "", 0, "", relativeTo, r.throwRangeError("largestUnit must not be smaller than smallestUnit")
	}
	if temporalDateTimeUnitRank[smallest] >= temporalDateTimeUnitRank["hour"] {
		increment, err = r.validateTemporalRoundingIncrement(rawIncrement, incrementSet, smallest, false)
	} else if !incrementSet {
		increment = 1
	} else {
		if math.IsNaN(rawIncrement) || math.IsInf(rawIncrement, 0) {
			return "", "", 0, "", relativeTo, r.throwRangeError("roundingIncrement must be finite")
		}
		rawIncrement = math.Trunc(rawIncrement)
		if rawIncrement < 1 || rawIncrement > 1_000_000_000 {
			return "", "", 0, "", relativeTo, r.throwRangeError("roundingIncrement is out of range")
		}
		increment = int64(rawIncrement)
	}
	if increment != 1 && temporalDateTimeUnitRank[smallest] <= temporalDateTimeUnitRank["day"] && largest != smallest {
		return "", "", 0, "", relativeTo, r.throwRangeError("cannot round a calendar unit increment while balancing to a larger unit")
	}
	return
}

func (r *Runtime) toTemporalDurationRelativeTo(value Value) (temporalDurationRelativeTo, error) {
	if value.IsObject() {
		switch relative := value.Object().data.(type) {
		case *temporalZonedDateTime:
			if relative != nil {
				copy := *relative
				return temporalDurationRelativeTo{zoned: &copy}, nil
			}
		case *temporalPlainDate:
			if relative != nil {
				plain := temporalPlainDateTime{temporalISODateTime: temporalISODateTime{year: relative.year, month: relative.month, day: relative.day}, calendar: relative.calendar}
				return temporalDurationRelativeTo{plain: &plain}, nil
			}
		case *temporalPlainDateTime:
			if relative != nil {
				plain := temporalPlainDateTime{
					temporalISODateTime: temporalISODateTime{
						year: relative.year, month: relative.month, day: relative.day,
					},
					calendar: relative.calendar,
				}
				return temporalDurationRelativeTo{plain: &plain}, nil
			}
		}
		return r.temporalDurationRelativeToFromBag(value.Object())
	}
	if !value.IsString() {
		return temporalDurationRelativeTo{}, r.throwTypeError("relativeTo must be a string or object")
	}
	text := value.String().Go()
	_, annotations, ok := splitTemporalAnnotations(text)
	if !ok {
		return temporalDurationRelativeTo{}, r.throwRangeError("invalid relativeTo string")
	}
	for _, annotation := range annotations {
		if !strings.Contains(strings.TrimPrefix(annotation, "!"), "=") {
			zoned, err := r.temporalDurationRelativeToFromZonedString(text)
			if err != nil {
				return temporalDurationRelativeTo{}, err
			}
			return temporalDurationRelativeTo{zoned: zoned}, nil
		}
	}
	date, err := r.toTemporalPlainDate(value, Undefined)
	if err != nil {
		return temporalDurationRelativeTo{}, err
	}
	plain := temporalPlainDateTime{temporalISODateTime: temporalISODateTime{year: date.year, month: date.month, day: date.day}, calendar: date.calendar}
	return temporalDurationRelativeTo{plain: &plain}, nil
}

func (r *Runtime) temporalDurationRelativeToFromBag(o *Object) (temporalDurationRelativeTo, error) {
	calendarValue, err := r.getProp(o, r.atoms.intern("calendar"), Obj(o))
	if err != nil {
		return temporalDurationRelativeTo{}, err
	}
	calendar, err := r.toTemporalCalendarIdentifierFromBag(calendarValue)
	if err != nil {
		return temporalDurationRelativeTo{}, err
	}
	values := map[string]int{}
	present := map[string]bool{}
	var monthCode string
	monthCodePresent := false
	var timeZoneValue Value
	offsetText, offsetPresent := "", false
	for _, name := range []string{"day", "hour", "microsecond", "millisecond", "minute", "month", "monthCode", "nanosecond", "offset", "second", "timeZone", "year"} {
		raw, getErr := r.getProp(o, r.atoms.intern(name), Obj(o))
		if getErr != nil {
			return temporalDurationRelativeTo{}, getErr
		}
		switch name {
		case "offset":
			if raw.IsUndefined() {
				continue
			}
			if !raw.IsString() && !raw.IsObject() {
				return temporalDurationRelativeTo{}, r.throwTypeError("offset must be a string")
			}
			offsetString, conversionErr := r.toString(raw)
			if conversionErr != nil {
				return temporalDurationRelativeTo{}, conversionErr
			}
			offsetText, offsetPresent = offsetString.Go(), true
			continue
		case "timeZone":
			timeZoneValue = raw
			continue
		case "monthCode":
			if raw.IsUndefined() {
				continue
			}
			monthCodePresent = true
			text, conversionErr := r.toString(raw)
			if conversionErr != nil {
				return temporalDurationRelativeTo{}, conversionErr
			}
			monthCode = text.Go()
			if !wellFormedTemporalMonthCode(monthCode) {
				return temporalDurationRelativeTo{}, r.throwRangeError("invalid monthCode")
			}
			continue
		}
		if raw.IsUndefined() {
			continue
		}
		present[name] = true
		values[name], err = r.temporalTruncatedInteger(raw, name)
		if err != nil {
			return temporalDurationRelativeTo{}, err
		}
	}
	if !present["year"] || !present["day"] || !present["month"] && !monthCodePresent {
		return temporalDurationRelativeTo{}, r.throwTypeError("relativeTo property bag is missing required fields")
	}
	month := values["month"]
	if monthCodePresent {
		parsed, ok := parseISOMonthCode(monthCode)
		if !ok || present["month"] && month != parsed {
			return temporalDurationRelativeTo{}, r.throwRangeError("invalid monthCode")
		}
		month = parsed
	}
	if month < 1 || values["day"] < 1 {
		return temporalDurationRelativeTo{}, r.throwRangeError("invalid relativeTo date")
	}
	month = min(month, 12)
	day := min(values["day"], isoDaysInMonth(values["year"], month))
	second := values["second"]
	if second == 60 {
		second = 59
	}
	dateTime := temporalISODateTime{
		year: values["year"], month: month, day: day,
		hour: values["hour"], minute: values["minute"], second: second,
		millisecond: values["millisecond"], microsecond: values["microsecond"], nanosecond: values["nanosecond"],
	}
	if !dateTime.valid() {
		return temporalDurationRelativeTo{}, r.throwRangeError("invalid relativeTo date-time")
	}
	if timeZoneValue.IsUndefined() {
		date := temporalPlainDate{year: dateTime.year, month: dateTime.month, day: dateTime.day, calendar: calendar}
		if !date.valid() {
			return temporalDurationRelativeTo{}, r.throwRangeError("relativeTo is outside the Temporal range")
		}
		plain := temporalPlainDateTime{temporalISODateTime: temporalISODateTime{year: date.year, month: date.month, day: date.day}, calendar: date.calendar}
		return temporalDurationRelativeTo{plain: &plain}, nil
	}
	zoneName, err := r.toTemporalTimeZoneIdentifier(timeZoneValue)
	if err != nil {
		return temporalDurationRelativeTo{}, err
	}
	zoned, err := r.newTemporalZonedDateTime(temporalInstant{}, zoneName, Str(NewString(calendar)))
	if err != nil {
		return temporalDurationRelativeTo{}, err
	}
	instant, ok := zoned.compatibleInstant(dateTime)
	if !ok {
		return temporalDurationRelativeTo{}, r.throwRangeError("relativeTo is outside the Temporal range")
	}
	if offsetPresent {
		index := 0
		offsetNanoseconds, valid := parseTemporalOffset(offsetText, &index)
		if !valid || index != len(offsetText) {
			return temporalDurationRelativeTo{}, r.throwRangeError("invalid offset")
		}
		actual := int64(zoned.offsetSeconds()) * temporalNanosecondsPerSecond
		zoned.instant = instant
		actual = int64(zoned.offsetSeconds()) * temporalNanosecondsPerSecond
		if offsetNanoseconds != actual {
			return temporalDurationRelativeTo{}, r.throwRangeError("offset does not match time zone")
		}
	}
	zoned.instant = instant
	if !validTemporalDurationRelativeZonedLocal(zoned) {
		return temporalDurationRelativeTo{}, r.throwRangeError("relativeTo is outside the Temporal range")
	}
	return temporalDurationRelativeTo{zoned: zoned}, nil
}

func (r *Runtime) temporalDurationRelativeToFromZonedString(text string) (*temporalZonedDateTime, error) {
	main, _, splitOK := splitTemporalAnnotations(text)
	timeStart := strings.IndexAny(main, "Tt ")
	hasExplicitOffset := timeStart >= 0 && strings.ContainsAny(main[timeStart:], "Zz+-")
	if splitOK && hasExplicitOffset {
		if _, instantErr := parseTemporalInstant(text); instantErr != nil {
			return nil, r.throwRangeError("relativeTo is outside the Temporal range")
		}
	}
	instant, zone, calendar, err := parseTemporalZonedDateTimeString(text)
	if err != nil {
		// Date-only strings with a zone annotation represent the zone's start
		// of day and are not accepted by the ZonedDateTime string parser.
		if timeStart >= 0 {
			return nil, r.throwRangeError("relativeTo is outside the Temporal range")
		}
		date, dateErr := parseTemporalPlainDate(text)
		if dateErr != nil {
			return nil, r.throwRangeError("invalid relativeTo string")
		}
		_, annotations, _ := splitTemporalAnnotations(text)
		zone = ""
		for _, annotation := range annotations {
			annotation = strings.TrimPrefix(annotation, "!")
			if !strings.Contains(annotation, "=") {
				zone = annotation
			}
		}
		zoned, zoneErr := r.newTemporalZonedDateTime(temporalInstant{}, zone, Str(NewString(date.calendar)))
		if zoneErr != nil {
			return nil, zoneErr
		}
		instant, ok := zoned.startOfDayInstant(date)
		if !ok {
			return nil, r.throwRangeError("relativeTo is outside the Temporal range")
		}
		zoned.instant = instant
		if !validTemporalDurationRelativeZonedLocal(zoned) {
			return nil, r.throwRangeError("relativeTo is outside the Temporal range")
		}
		return zoned, nil
	}
	zoned, err := r.newTemporalZonedDateTime(instant, zone, Str(NewString(calendar)))
	if err != nil {
		return nil, err
	}
	if !validTemporalDurationRelativeZonedLocal(zoned) {
		return nil, r.throwRangeError("relativeTo is outside the Temporal range")
	}
	if timeStart >= 0 && !strings.ContainsAny(main[timeStart:], "Zz") {
		for index := timeStart + 1; index < len(main); index++ {
			if main[index] != '+' && main[index] != '-' {
				continue
			}
			offsetIndex := index
			offsetNanoseconds, valid := parseTemporalOffset(main, &offsetIndex)
			if !valid || offsetIndex != len(main) || offsetNanoseconds != int64(zoned.offsetSeconds())*temporalNanosecondsPerSecond {
				return nil, r.throwRangeError("offset does not match time zone")
			}
			break
		}
	}
	return zoned, nil
}

func validTemporalDurationRelativeZonedLocal(zoned *temporalZonedDateTime) bool {
	local := zoned.localISODateTime()
	days := isoDaysFromCivil(int64(local.year), local.month, local.day)
	return days >= -100_000_000 && days <= 100_000_000
}

func (r *Runtime) totalTemporalDurationNanoseconds(duration temporalDuration, relativeTo temporalDurationRelativeTo, validateEndpoint bool) (*big.Int, error) {
	if duration.years == 0 && duration.months == 0 && duration.weeks == 0 && duration.days == 0 &&
		(!validateEndpoint || relativeTo.plain == nil && relativeTo.zoned == nil) {
		return duration.timePartNanoseconds(), nil
	}
	if relativeTo.plain == nil && relativeTo.zoned == nil {
		if duration.years != 0 || duration.months != 0 || duration.weeks != 0 {
			return nil, r.throwRangeError("calendar durations require relativeTo")
		}
		total := new(big.Int).Mul(floatIntegerBig(duration.days), big.NewInt(86_400_000_000_000))
		return total.Add(total, duration.timePartNanoseconds()), nil
	}
	if relativeTo.zoned != nil {
		end, err := r.addTemporalDurationToZonedInstant(relativeTo.zoned, duration)
		if err != nil {
			return nil, err
		}
		return new(big.Int).Sub(end.epochNanoseconds(), relativeTo.zoned.instant.epochNanoseconds()), nil
	}
	start := *relativeTo.plain
	if duration.sign() == 0 {
		return new(big.Int), nil
	}
	if !start.valid() {
		return nil, r.throwRangeError("relativeTo is outside the Temporal date-time range")
	}
	end, err := r.addTemporalPlainDateTime(start, duration, 1, "constrain")
	if err != nil {
		return nil, err
	}
	return new(big.Int).Sub(temporalPlainDateTimeEpochNanoseconds(end), temporalPlainDateTimeEpochNanoseconds(start)), nil
}

func (r *Runtime) addTemporalDurationToZonedInstant(relativeTo *temporalZonedDateTime, duration temporalDuration) (temporalInstant, error) {
	return r.addTemporalDurationToZonedInstantWithOverflow(relativeTo, duration, "constrain")
}

func (r *Runtime) addTemporalDurationToZonedInstantWithOverflow(relativeTo *temporalZonedDateTime, duration temporalDuration, overflow string) (temporalInstant, error) {
	if duration.years == 0 && duration.months == 0 && duration.weeks == 0 && duration.days == 0 {
		end, ok := relativeTo.instant.addNanoseconds(duration.timePartNanoseconds())
		if !ok {
			return temporalInstant{}, r.throwRangeError("duration endpoint is outside the Temporal range")
		}
		return end, nil
	}
	startLocal := temporalPlainDateTime{temporalISODateTime: relativeTo.localISODateTime(), calendar: relativeTo.calendar}
	dateDuration := temporalDuration{years: duration.years, months: duration.months, weeks: duration.weeks, days: duration.days}
	afterDate, err := r.addTemporalPlainDateTime(startLocal, dateDuration, 1, overflow)
	if err != nil {
		return temporalInstant{}, err
	}
	afterDateInstant, ok := relativeTo.compatibleInstant(afterDate.temporalISODateTime)
	if !ok {
		return temporalInstant{}, r.throwRangeError("duration endpoint is outside the Temporal range")
	}
	end, ok := afterDateInstant.addNanoseconds(duration.timePartNanoseconds())
	if !ok {
		return temporalInstant{}, r.throwRangeError("duration endpoint is outside the Temporal range")
	}
	return end, nil
}

func (r *Runtime) roundTemporalDuration(duration temporalDuration, largest, smallest string, increment int64, mode string, relativeTo temporalDurationRelativeTo) (temporalDuration, error) {
	if relativeTo.plain == nil && relativeTo.zoned == nil {
		if duration.years != 0 || duration.months != 0 || duration.weeks != 0 || temporalDateTimeUnitRank[largest] < temporalDateTimeUnitRank["day"] || temporalDateTimeUnitRank[smallest] < temporalDateTimeUnitRank["day"] {
			return temporalDuration{}, r.throwRangeError("calendar units require relativeTo")
		}
		return roundTemporalDurationWithoutRelativeTo(duration, largest, smallest, increment, mode), nil
	}
	if relativeTo.zoned != nil {
		return r.roundTemporalDurationRelativeToZoned(duration, largest, smallest, increment, mode, relativeTo.zoned)
	}
	if duration.sign() == 0 {
		return temporalDuration{}, nil
	}
	if !relativeTo.plain.valid() {
		return temporalDuration{}, r.throwRangeError("relativeTo is outside the Temporal date-time range")
	}
	if duration.years == 0 && duration.months == 0 && duration.weeks == 0 && temporalDateTimeUnitRank[largest] >= temporalDateTimeUnitRank["day"] && temporalDateTimeUnitRank[smallest] >= temporalDateTimeUnitRank["day"] {
		return roundTemporalDurationWithoutRelativeTo(duration, largest, smallest, increment, mode), nil
	}
	start := *relativeTo.plain
	end, err := r.addTemporalPlainDateTime(start, duration, 1, "constrain")
	if err != nil {
		return temporalDuration{}, err
	}
	return r.differenceTemporalPlainDateTimes(start, end, largest, smallest, increment, mode)
}

func roundTemporalDurationWithoutRelativeTo(duration temporalDuration, largest, smallest string, increment int64, mode string) temporalDuration {
	const dayNanoseconds = int64(86_400_000_000_000)
	total := new(big.Int).Mul(floatIntegerBig(duration.days), big.NewInt(dayNanoseconds))
	total.Add(total, duration.timePartNanoseconds())
	unitNanoseconds := int64(dayNanoseconds)
	if smallest != "day" {
		unitNanoseconds = temporalUnitNanoseconds[smallest]
	}
	step := new(big.Int).Mul(big.NewInt(unitNanoseconds), big.NewInt(increment))
	total = roundTemporalBigInt(total, step, mode)
	if largest == "day" {
		dayCount, remainder := new(big.Int), new(big.Int)
		dayCount.QuoRem(total, big.NewInt(dayNanoseconds), remainder)
		result := temporalDurationFromNanoseconds(remainder, "hour")
		result.days, _ = new(big.Float).SetInt(dayCount).Float64()
		return result
	}
	return temporalDurationFromNanoseconds(total, largest)
}

func (r *Runtime) roundTemporalDurationRelativeToZoned(duration temporalDuration, largest, smallest string, increment int64, mode string, relativeTo *temporalZonedDateTime) (temporalDuration, error) {
	if duration.sign() == 0 && largest != "day" {
		return temporalDuration{}, nil
	}
	if largest == "day" && temporalDateTimeUnitRank[smallest] >= temporalDateTimeUnitRank["hour"] {
		local := relativeTo.localISODateTime()
		nextDate, err := r.addTemporalPlainDate(temporalPlainDate{year: local.year, month: local.month, day: local.day, calendar: relativeTo.calendar}, temporalDuration{days: 1}, 1, "constrain")
		if err != nil {
			return temporalDuration{}, err
		}
		next := local
		next.year, next.month, next.day = nextDate.year, nextDate.month, nextDate.day
		if _, ok := relativeTo.compatibleInstant(next); !ok {
			return temporalDuration{}, r.throwRangeError("next day boundary is outside the Temporal range")
		}
	}
	if duration.sign() == 0 {
		return temporalDuration{}, nil
	}
	if _, err := r.addTemporalDurationToZonedInstant(relativeTo, duration); err != nil {
		return temporalDuration{}, err
	}
	startLocal := temporalPlainDateTime{temporalISODateTime: relativeTo.localISODateTime(), calendar: relativeTo.calendar}
	// UTC and fixed-offset zones have constant-length days, so the plain
	// date-time algorithm is exact for them. Named-zone transitions are handled
	// below as additional ZonedDateTime operations are implemented.
	if relativeTo.fixed || relativeTo.timeZone == "UTC" {
		end, err := r.addTemporalPlainDateTime(startLocal, duration, 1, "constrain")
		if err != nil {
			return temporalDuration{}, err
		}
		return r.differenceTemporalPlainDateTimes(startLocal, end, largest, smallest, increment, mode)
	}
	end, err := r.addTemporalPlainDateTime(startLocal, duration, 1, "constrain")
	if err != nil {
		return temporalDuration{}, err
	}
	return r.differenceTemporalPlainDateTimes(startLocal, end, largest, smallest, increment, mode)
}

func durationFromFields(v [10]float64) temporalDuration {
	return temporalDuration{v[0], v[1], v[2], v[3], v[4], v[5], v[6], v[7], v[8], v[9]}
}
func newTemporalDuration(proto *Object, d temporalDuration) *Object {
	o := newObject(proto, ClassObject)
	o.data = &d
	return o
}
func (r *Runtime) temporalDurationValue(v Value, method string) (temporalDuration, error) {
	if v.IsObject() {
		if d, ok := v.Object().data.(*temporalDuration); ok {
			return *d, nil
		}
	}
	return temporalDuration{}, r.throwTypeError("%s called on an incompatible receiver", method)
}
func (r *Runtime) temporalInteger(v Value) (float64, error) {
	if v.IsUndefined() {
		return 0, nil
	}
	n, err := r.toNumber(v)
	if err != nil {
		return 0, err
	}
	if math.IsNaN(n) || math.IsInf(n, 0) || n != math.Trunc(n) {
		return 0, r.throwRangeError("duration fields must be finite integers")
	}
	if n == 0 {
		return 0, nil
	}
	return n, nil
}

func (r *Runtime) toTemporalDuration(v Value) (temporalDuration, error) {
	if v.IsObject() {
		if d, ok := v.Object().data.(*temporalDuration); ok {
			return *d, nil
		}
		return r.temporalDurationFromBag(v.Object())
	}
	if v.IsString() {
		d, err := parseTemporalDuration(v.String().Go())
		if err != nil {
			return temporalDuration{}, r.throwRangeError("invalid Temporal.Duration string")
		}
		if !d.withinRange() {
			return temporalDuration{}, r.throwRangeError("duration is out of range")
		}
		return d, nil
	}
	return temporalDuration{}, r.throwTypeError("a duration must be a string or object")
}

func (r *Runtime) temporalDurationFromBag(o *Object) (temporalDuration, error) {
	var d temporalDuration
	found := false
	for _, field := range []struct {
		name   string
		target *float64
	}{
		{"days", &d.days}, {"hours", &d.hours}, {"microseconds", &d.microseconds}, {"milliseconds", &d.milliseconds}, {"minutes", &d.minutes}, {"months", &d.months}, {"nanoseconds", &d.nanoseconds}, {"seconds", &d.seconds}, {"weeks", &d.weeks}, {"years", &d.years},
	} {
		raw, err := r.getProp(o, r.atoms.intern(field.name), Obj(o))
		if err != nil {
			return d, err
		}
		if raw.IsUndefined() {
			continue
		}
		found = true
		value, err := r.temporalInteger(raw)
		if err != nil {
			return d, err
		}
		*field.target = value
	}
	if !found {
		return d, r.throwTypeError("duration property bag has no duration fields")
	}
	if !d.valid() {
		return d, r.throwRangeError("duration fields must have the same sign")
	}
	if !d.withinRange() {
		return d, r.throwRangeError("duration is out of range")
	}
	return d, nil
}

func (d temporalDuration) largestTimeUnit() string {
	if d.hours != 0 {
		return "hour"
	}
	if d.minutes != 0 {
		return "minute"
	}
	if d.seconds != 0 {
		return "second"
	}
	if d.milliseconds != 0 {
		return "millisecond"
	}
	if d.microseconds != 0 {
		return "microsecond"
	}
	return "nanosecond"
}

func (r *Runtime) temporalTotalOptions(value Value) (string, temporalDurationRelativeTo, error) {
	var raw Value
	var relativeTo temporalDurationRelativeTo
	if value.IsString() {
		raw = value
	} else {
		if !value.IsObject() {
			return "", relativeTo, r.throwTypeError("total options must be a unit string or object")
		}
		relativeValue, err := r.getProp(value.Object(), r.atoms.intern("relativeTo"), value)
		if err != nil {
			return "", relativeTo, err
		}
		if !relativeValue.IsUndefined() {
			relativeTo, err = r.toTemporalDurationRelativeTo(relativeValue)
			if err != nil {
				return "", relativeTo, err
			}
		}
		raw, err = r.getProp(value.Object(), r.atoms.intern("unit"), value)
		if err != nil {
			return "", relativeTo, err
		}
	}
	if raw.IsUndefined() {
		return "", relativeTo, r.throwRangeError("unit is required")
	}
	text, err := r.toString(raw)
	if err != nil {
		return "", relativeTo, err
	}
	unit, ok := normalizeTemporalUnit(text.Go())
	if !ok {
		unit, ok = normalizeTemporalDateTimeUnit(text.Go())
	}
	if !ok {
		return "", relativeTo, r.throwRangeError("invalid total unit")
	}
	return unit, relativeTo, nil
}

func (r *Runtime) totalTemporalDuration(duration temporalDuration, relativeTo temporalDurationRelativeTo, unit string) (float64, error) {
	hasRelativeTo := relativeTo.plain != nil || relativeTo.zoned != nil
	if !hasRelativeTo {
		if temporalDateTimeUnitRank[unit] < temporalDateTimeUnitRank["day"] ||
			duration.years != 0 || duration.months != 0 || duration.weeks != 0 {
			return 0, r.throwRangeError("calendar units require relativeTo")
		}
		return temporalBigRatio(duration.dayAndTimeNanoseconds(), temporalDurationUnitNanoseconds(unit)), nil
	}
	if duration.sign() == 0 {
		if relativeTo.zoned != nil && temporalDateTimeUnitRank[unit] <= temporalDateTimeUnitRank["day"] {
			start := temporalPlainDateTime{
				temporalISODateTime: relativeTo.zoned.localISODateTime(),
				calendar:            relativeTo.zoned.calendar,
			}
			next, err := r.addTemporalPlainDateTime(start, temporalCalendarUnitDuration(unit, 1), 1, "constrain")
			if err != nil {
				return 0, err
			}
			if _, ok := relativeTo.zoned.compatibleInstant(next.temporalISODateTime); !ok {
				return 0, r.throwRangeError("next calendar boundary is outside the Temporal range")
			}
		}
		return 0, nil
	}
	if temporalDateTimeUnitRank[unit] >= temporalDateTimeUnitRank["hour"] {
		total, err := r.totalTemporalDurationNanoseconds(duration, relativeTo, true)
		if err != nil {
			return 0, err
		}
		return temporalBigRatio(total, temporalDurationUnitNanoseconds(unit)), nil
	}
	if relativeTo.zoned != nil {
		return r.totalTemporalDurationRelativeToZoned(duration, relativeTo.zoned, unit)
	}
	return r.totalTemporalDurationRelativeToPlain(duration, *relativeTo.plain, unit)
}

func temporalDurationUnitNanoseconds(unit string) *big.Int {
	if unit == "day" {
		return big.NewInt(temporalSecondsPerDay * temporalNanosecondsPerSecond)
	}
	return big.NewInt(temporalUnitNanoseconds[unit])
}

func temporalBigRatio(numerator, denominator *big.Int) float64 {
	ratio := new(big.Rat).SetFrac(numerator, denominator)
	value, _ := ratio.Float64()
	return value
}

func temporalCalendarUnitDuration(unit string, amount int64) temporalDuration {
	var duration temporalDuration
	switch unit {
	case "year":
		duration.years = float64(amount)
	case "month":
		duration.months = float64(amount)
	case "week":
		duration.weeks = float64(amount)
	case "day":
		duration.days = float64(amount)
	}
	return duration
}

func approximateTemporalCalendarUnits(start, end temporalISODateTime, unit string) int64 {
	switch unit {
	case "year":
		return int64(end.year - start.year)
	case "month":
		return int64(end.year-start.year)*12 + int64(end.month-start.month)
	case "week":
		return (isoDaysFromCivil(int64(end.year), end.month, end.day) -
			isoDaysFromCivil(int64(start.year), start.month, start.day)) / 7
	default:
		return isoDaysFromCivil(int64(end.year), end.month, end.day) -
			isoDaysFromCivil(int64(start.year), start.month, start.day)
	}
}

func temporalCalendarTotal(amount int64, target, anchor, next *big.Int) float64 {
	span := new(big.Int).Sub(next, anchor)
	span.Abs(span)
	progress := new(big.Int).Sub(target, anchor)
	numerator := new(big.Int).Mul(big.NewInt(amount), span)
	numerator.Add(numerator, progress)
	return temporalBigRatio(numerator, span)
}

func (r *Runtime) totalTemporalDurationRelativeToPlain(duration temporalDuration, start temporalPlainDateTime, unit string) (float64, error) {
	target, err := r.addTemporalPlainDateTime(start, duration, 1, "constrain")
	if err != nil {
		return 0, err
	}
	startValue := temporalPlainDateTimeEpochNanoseconds(start)
	targetValue := temporalPlainDateTimeEpochNanoseconds(target)
	direction := int64(targetValue.Cmp(startValue))
	amount := approximateTemporalCalendarUnits(start.temporalISODateTime, target.temporalISODateTime, unit)
	makeAnchor := func(value int64) (temporalPlainDateTime, *big.Int, error) {
		anchor, err := r.addTemporalPlainDateTime(start, temporalCalendarUnitDuration(unit, value), 1, "constrain")
		if err != nil {
			return temporalPlainDateTime{}, nil, err
		}
		return anchor, temporalPlainDateTimeEpochNanoseconds(anchor), nil
	}
	_, anchorValue, err := makeAnchor(amount)
	if err != nil {
		return 0, err
	}
	if direction > 0 && anchorValue.Cmp(targetValue) > 0 || direction < 0 && anchorValue.Cmp(targetValue) < 0 {
		amount -= direction
		_, anchorValue, err = makeAnchor(amount)
		if err != nil {
			return 0, err
		}
	}
	if anchorValue.Cmp(targetValue) == 0 {
		return float64(amount), nil
	}
	_, nextValue, err := makeAnchor(amount + direction)
	if err != nil {
		return 0, err
	}
	return temporalCalendarTotal(amount, targetValue, anchorValue, nextValue), nil
}

func (r *Runtime) totalTemporalDurationRelativeToZoned(duration temporalDuration, start *temporalZonedDateTime, unit string) (float64, error) {
	targetInstant, err := r.addTemporalDurationToZonedInstant(start, duration)
	if err != nil {
		return 0, err
	}
	startLocal := temporalPlainDateTime{temporalISODateTime: start.localISODateTime(), calendar: start.calendar}
	targetZoned := *start
	targetZoned.instant = targetInstant
	targetLocal := targetZoned.localISODateTime()
	startValue := start.instant.epochNanoseconds()
	targetValue := targetInstant.epochNanoseconds()
	direction := int64(targetValue.Cmp(startValue))
	amount := approximateTemporalCalendarUnits(startLocal.temporalISODateTime, targetLocal, unit)
	makeAnchor := func(value int64) (*big.Int, error) {
		local, err := r.addTemporalPlainDateTime(startLocal, temporalCalendarUnitDuration(unit, value), 1, "constrain")
		if err != nil {
			return nil, err
		}
		instant, ok := start.compatibleInstant(local.temporalISODateTime)
		if !ok {
			return nil, r.throwRangeError("duration endpoint is outside the Temporal range")
		}
		return instant.epochNanoseconds(), nil
	}
	anchorValue, err := makeAnchor(amount)
	if err != nil {
		return 0, err
	}
	if direction > 0 && anchorValue.Cmp(targetValue) > 0 || direction < 0 && anchorValue.Cmp(targetValue) < 0 {
		amount -= direction
		anchorValue, err = makeAnchor(amount)
		if err != nil {
			return 0, err
		}
	}
	if anchorValue.Cmp(targetValue) == 0 {
		return float64(amount), nil
	}
	nextValue, err := makeAnchor(amount + direction)
	if err != nil {
		return 0, err
	}
	return temporalCalendarTotal(amount, targetValue, anchorValue, nextValue), nil
}

func parseTemporalDuration(s string) (temporalDuration, error) {
	var d temporalDuration
	sign := float64(1)
	i := 0
	if i < len(s) && (s[i] == '+' || s[i] == '-') {
		if s[i] == '-' {
			sign = -1
		}
		i++
	}
	if i >= len(s) || s[i] != 'P' && s[i] != 'p' {
		return d, errInvalidTemporalInstant
	}
	i++
	seen := false
	inTime := false
	for i < len(s) {
		if (s[i] == 'T' || s[i] == 't') && !inTime {
			inTime = true
			i++
			continue
		}
		start := i
		for i < len(s) && s[i] >= '0' && s[i] <= '9' {
			i++
		}
		if start == i {
			return d, errInvalidTemporalInstant
		}
		n, err := strconv.ParseFloat(s[start:i], 64)
		if err != nil || math.IsInf(n, 0) || n < 0 || n != math.Trunc(n) {
			return d, errInvalidTemporalInstant
		}
		fraction := ""
		if i < len(s) && (s[i] == '.' || s[i] == ',') {
			i++
			fractionStart := i
			for i < len(s) && s[i] >= '0' && s[i] <= '9' {
				i++
			}
			if fractionStart == i {
				return d, errInvalidTemporalInstant
			}
			fraction = s[fractionStart:i]
			if len(fraction) > 9 {
				return d, errInvalidTemporalInstant
			}
		}
		if i >= len(s) {
			return d, errInvalidTemporalInstant
		}
		unit := s[i]
		if unit >= 'a' && unit <= 'z' {
			unit -= 'a' - 'A'
		}
		i++
		target := (*float64)(nil)
		switch unit {
		case 'Y':
			if inTime {
				return d, errInvalidTemporalInstant
			}
			target = &d.years
		case 'M':
			if inTime {
				target = &d.minutes
			} else {
				target = &d.months
			}
		case 'W':
			if inTime {
				return d, errInvalidTemporalInstant
			}
			target = &d.weeks
		case 'D':
			if inTime {
				return d, errInvalidTemporalInstant
			}
			target = &d.days
		case 'H':
			if !inTime {
				return d, errInvalidTemporalInstant
			}
			target = &d.hours
		case 'S':
			if !inTime {
				return d, errInvalidTemporalInstant
			}
			target = &d.seconds
		default:
			return d, errInvalidTemporalInstant
		}
		*target = sign * n
		if fraction != "" {
			if !inTime || i != len(s) {
				return d, errInvalidTemporalInstant
			}
			var scale int64
			switch unit {
			case 'H':
				scale = 3_600_000_000_000
			case 'M':
				scale = 60_000_000_000
			case 'S':
				scale = 1_000_000_000
			default:
				return d, errInvalidTemporalInstant
			}
			extra, ok := roundedFractionNanoseconds(fraction, scale)
			if !ok {
				return d, errInvalidTemporalInstant
			}
			d.addFractionalTime(sign * float64(extra))
		}
		seen = true
	}
	if !seen {
		return d, errInvalidTemporalInstant
	}
	return d, nil
}

func roundedFractionNanoseconds(digits string, scale int64) (int64, bool) {
	numerator, ok := new(big.Int).SetString(digits, 10)
	if !ok {
		return 0, false
	}
	numerator.Mul(numerator, big.NewInt(scale))
	denominator := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(len(digits))), nil)
	quotient, remainder := new(big.Int), new(big.Int)
	quotient.QuoRem(numerator, denominator, remainder)
	if new(big.Int).Lsh(remainder, 1).Cmp(denominator) >= 0 {
		quotient.Add(quotient, big.NewInt(1))
	}
	if !quotient.IsInt64() {
		return 0, false
	}
	return quotient.Int64(), true
}

func (d *temporalDuration) addFractionalTime(nanoseconds float64) {
	d.minutes += math.Trunc(nanoseconds / 60_000_000_000)
	nanoseconds = math.Mod(nanoseconds, 60_000_000_000)
	d.seconds += math.Trunc(nanoseconds / 1_000_000_000)
	nanoseconds = math.Mod(nanoseconds, 1_000_000_000)
	d.milliseconds += math.Trunc(nanoseconds / 1_000_000)
	nanoseconds = math.Mod(nanoseconds, 1_000_000)
	d.microseconds += math.Trunc(nanoseconds / 1_000)
	d.nanoseconds += math.Mod(nanoseconds, 1_000)
}

func roundTemporalDurationForString(d temporalDuration, step int64, mode string) (temporalDuration, error) {
	rounded := roundTemporalBigInt(d.timePartNanoseconds(), big.NewInt(step), mode)
	hasDateUnits := d.years != 0 || d.months != 0 || d.weeks != 0 || d.days != 0
	if hasDateUnits {
		dayCarry, remainder := new(big.Int), new(big.Int)
		dayCarry.QuoRem(rounded, big.NewInt(86_400_000_000_000), remainder)
		carry, _ := new(big.Float).SetInt(dayCarry).Float64()
		d.days += carry
		rounded = remainder
	}
	largest := d.largestTimeUnit()
	if hasDateUnits {
		largest = "hour"
	}
	time := temporalDurationFromNanoseconds(rounded, largest)
	d.hours, d.minutes, d.seconds = time.hours, time.minutes, time.seconds
	d.milliseconds, d.microseconds, d.nanoseconds = time.milliseconds, time.microseconds, time.nanoseconds
	if !d.valid() {
		return temporalDuration{}, errInvalidTemporalInstant
	}
	return d, nil
}

func (d temporalDuration) string() string {
	return d.stringWithPrecision(-1)
}

func (d temporalDuration) stringWithPrecision(precision int) string {
	sign := d.sign()
	if sign == 0 {
		if precision >= 0 {
			if precision == 0 {
				return "PT0S"
			}
			return "PT0." + strings.Repeat("0", precision) + "S"
		}
		return "PT0S"
	}
	values := d.fields()
	if sign < 0 {
		for i := range values {
			values[i] = -values[i]
		}
	}
	var b strings.Builder
	if sign < 0 {
		b.WriteByte('-')
	}
	b.WriteByte('P')
	labels := []byte{'Y', 'M', 'W', 'D'}
	for i := 0; i < 4; i++ {
		if values[i] != 0 {
			b.WriteString(formatTemporalInteger(values[i]))
			b.WriteByte(labels[i])
		}
	}
	if values[4] != 0 || values[5] != 0 || values[6] != 0 || values[7] != 0 || values[8] != 0 || values[9] != 0 || precision >= 0 {
		b.WriteByte('T')
		if values[4] != 0 {
			b.WriteString(formatTemporalInteger(values[4]))
			b.WriteByte('H')
		}
		if values[5] != 0 {
			b.WriteString(formatTemporalInteger(values[5]))
			b.WriteByte('M')
		}
		subseconds := new(big.Int)
		for _, part := range []struct {
			value float64
			scale int64
		}{{values[6], 1_000_000_000}, {values[7], 1_000_000}, {values[8], 1_000}, {values[9], 1}} {
			integer, _ := new(big.Float).SetFloat64(part.value).Int(nil)
			subseconds.Add(subseconds, new(big.Int).Mul(integer, big.NewInt(part.scale)))
		}
		if subseconds.Sign() != 0 || precision >= 0 {
			whole, fractionValue := new(big.Int), new(big.Int)
			whole.QuoRem(subseconds, big.NewInt(1_000_000_000), fractionValue)
			b.WriteString(whole.String())
			if precision > 0 {
				fraction := strconv.FormatInt(fractionValue.Int64()+1_000_000_000, 10)[1:]
				b.WriteByte('.')
				b.WriteString(fraction[:precision])
			} else if precision < 0 && fractionValue.Sign() != 0 {
				fraction := strconv.FormatInt(fractionValue.Int64()+1_000_000_000, 10)[1:]
				b.WriteByte('.')
				b.WriteString(strings.TrimRight(fraction, "0"))
			}
			b.WriteByte('S')
		}
	}
	return b.String()
}

func formatTemporalInteger(value float64) string {
	return strconv.FormatFloat(value, 'f', 0, 64)
}
