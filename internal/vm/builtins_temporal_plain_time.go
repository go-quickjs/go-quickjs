package vm

import (
	"math"
	"math/big"
	"strconv"
	"strings"
)

type temporalPlainTime struct {
	hour, minute, second int
	millisecond          int
	microsecond          int
	nanosecond           int
}

func (t temporalPlainTime) valid() bool {
	return t.hour >= 0 && t.hour <= 23 && t.minute >= 0 && t.minute <= 59 &&
		t.second >= 0 && t.second <= 59 && t.millisecond >= 0 && t.millisecond <= 999 &&
		t.microsecond >= 0 && t.microsecond <= 999 && t.nanosecond >= 0 && t.nanosecond <= 999
}

func (r *Runtime) initTemporalPlainTime(temporal *Object) {
	proto := newObject(r.proto.object, ClassObject)
	r.temporalPlainTimeProto = proto
	ctor := r.newTemporalCtor(temporal, "PlainTime", 0, proto, func(rt *Runtime, this Value, args []Value) (Value, error) {
		if err := rt.requireNew("Temporal.PlainTime"); err != nil {
			return Undefined, err
		}
		fields := [6]int{}
		for i, name := range []string{"hour", "minute", "second", "millisecond", "microsecond", "nanosecond"} {
			if arg(args, i).IsUndefined() {
				continue
			}
			value, err := rt.temporalTruncatedInteger(arg(args, i), name)
			if err != nil {
				return Undefined, err
			}
			fields[i] = value
		}
		time := temporalPlainTime{fields[0], fields[1], fields[2], fields[3], fields[4], fields[5]}
		if !time.valid() {
			return Undefined, rt.throwRangeError("invalid Temporal.PlainTime")
		}
		instanceProto, err := rt.protoFromNewTargetErr(proto)
		if err != nil {
			return Undefined, err
		}
		return Obj(newTemporalPlainTime(instanceProto, time)), nil
	})
	r.defMethod(ctor, "from", 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
		time, err := rt.toTemporalPlainTime(arg(args, 0), arg(args, 1))
		if err != nil {
			return Undefined, err
		}
		return Obj(newTemporalPlainTime(proto, time)), nil
	})
	r.defMethod(ctor, "compare", 2, func(rt *Runtime, this Value, args []Value) (Value, error) {
		left, err := rt.toTemporalPlainTime(arg(args, 0), Undefined)
		if err != nil {
			return Undefined, err
		}
		right, err := rt.toTemporalPlainTime(arg(args, 1), Undefined)
		if err != nil {
			return Undefined, err
		}
		return Int(compareTemporalPlainTimes(left, right)), nil
	})
	for _, property := range []string{"hour", "minute", "second", "millisecond", "microsecond", "nanosecond"} {
		name := property
		r.defGetter(proto, name, func(rt *Runtime, this Value, args []Value) (Value, error) {
			time, err := rt.temporalPlainTimeValue(this, "get Temporal.PlainTime.prototype."+name)
			if err != nil {
				return Undefined, err
			}
			switch name {
			case "hour":
				return Int(time.hour), nil
			case "minute":
				return Int(time.minute), nil
			case "second":
				return Int(time.second), nil
			case "millisecond":
				return Int(time.millisecond), nil
			case "microsecond":
				return Int(time.microsecond), nil
			default:
				return Int(time.nanosecond), nil
			}
		})
	}
	r.defMethod(proto, "equals", 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
		time, err := rt.temporalPlainTimeValue(this, "Temporal.PlainTime.prototype.equals")
		if err != nil {
			return Undefined, err
		}
		other, err := rt.toTemporalPlainTime(arg(args, 0), Undefined)
		if err != nil {
			return Undefined, err
		}
		return Bool(compareTemporalPlainTimes(time, other) == 0), nil
	})
	r.defMethod(proto, "with", 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
		time, err := rt.temporalPlainTimeValue(this, "Temporal.PlainTime.prototype.with")
		if err != nil {
			return Undefined, err
		}
		fields, err := rt.temporalPartialObject(arg(args, 0))
		if err != nil {
			return Undefined, err
		}
		names := []string{"hour", "microsecond", "millisecond", "minute", "nanosecond", "second"}
		values := make(map[string]float64, len(names))
		found := false
		for _, name := range names {
			raw, err := rt.getProp(fields, rt.atoms.intern(name), Obj(fields))
			if err != nil {
				return Undefined, err
			}
			if raw.IsUndefined() {
				continue
			}
			found = true
			value, err := rt.toNumber(raw)
			if err != nil {
				return Undefined, err
			}
			if math.IsNaN(value) || math.IsInf(value, 0) {
				return Undefined, rt.throwRangeError("%s must be a finite number", name)
			}
			values[name] = math.Trunc(value)
		}
		if !found {
			return Undefined, rt.throwTypeError("time fields must not be empty")
		}
		overflow, err := rt.temporalOverflowOption(arg(args, 1))
		if err != nil {
			return Undefined, err
		}
		result := time
		limits := map[string]float64{
			"hour": 23, "minute": 59, "second": 59,
			"millisecond": 999, "microsecond": 999, "nanosecond": 999,
		}
		for _, name := range names {
			value, present := values[name]
			if !present {
				continue
			}
			if overflow == "constrain" {
				value = max(0, min(limits[name], value))
			} else if value < 0 || value > limits[name] {
				return Undefined, rt.throwRangeError("invalid Temporal.PlainTime")
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
		return Obj(newTemporalPlainTime(rt.temporalPlainTimeProto, result)), nil
	})
	for _, operation := range []struct {
		name string
		sign int64
	}{{"add", 1}, {"subtract", -1}} {
		op := operation
		r.defMethod(proto, op.name, 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
			time, err := rt.temporalPlainTimeValue(this, "Temporal.PlainTime.prototype."+op.name)
			if err != nil {
				return Undefined, err
			}
			duration, err := rt.toTemporalDuration(arg(args, 0))
			if err != nil {
				return Undefined, err
			}
			result := addTemporalPlainTime(time, duration, op.sign)
			return Obj(newTemporalPlainTime(rt.temporalPlainTimeProto, result)), nil
		})
	}
	r.defMethod(proto, "round", 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
		time, err := rt.temporalPlainTimeValue(this, "Temporal.PlainTime.prototype.round")
		if err != nil {
			return Undefined, err
		}
		smallest, increment, mode, err := rt.temporalRoundOptions(arg(args, 0), false)
		if err != nil {
			return Undefined, err
		}
		total := temporalPlainTimeNanoseconds(time)
		step := new(big.Int).Mul(big.NewInt(temporalUnitNanoseconds[smallest]), big.NewInt(increment))
		total = roundTemporalBigIntAsIfPositive(total, step, mode)
		return Obj(newTemporalPlainTime(rt.temporalPlainTimeProto, temporalPlainTimeFromNanoseconds(total))), nil
	})
	r.defMethod(proto, "toString", 0, func(rt *Runtime, this Value, args []Value) (Value, error) {
		time, err := rt.temporalPlainTimeValue(this, "Temporal.PlainTime.prototype.toString")
		if err != nil {
			return Undefined, err
		}
		precision, minuteOnly, step, mode, err := rt.temporalPlainTimeStringOptions(arg(args, 0))
		if err != nil {
			return Undefined, err
		}
		total := roundTemporalBigIntAsIfPositive(temporalPlainTimeNanoseconds(time), big.NewInt(step), mode)
		return Str(NewString(temporalPlainTimeFromNanoseconds(total).stringWithPrecision(precision, minuteOnly))), nil
	})
	r.defMethod(proto, "toJSON", 0, func(rt *Runtime, this Value, args []Value) (Value, error) {
		time, err := rt.temporalPlainTimeValue(this, "Temporal.PlainTime.prototype.toJSON")
		if err != nil {
			return Undefined, err
		}
		return Str(NewString(time.string())), nil
	})
	r.defMethod(proto, "valueOf", 0, func(rt *Runtime, this Value, args []Value) (Value, error) {
		if _, err := rt.temporalPlainTimeValue(this, "Temporal.PlainTime.prototype.valueOf"); err != nil {
			return Undefined, err
		}
		return Undefined, rt.throwTypeError("use Temporal.PlainTime.compare() or equals() to compare times")
	})
	r.defToStringTag(proto, "Temporal.PlainTime")
}

func addTemporalPlainTime(time temporalPlainTime, duration temporalDuration, sign int64) temporalPlainTime {
	total := temporalPlainTimeNanoseconds(time)
	delta := duration.timePartNanoseconds()
	if sign < 0 {
		delta.Neg(delta)
	}
	total.Add(total, delta)
	return temporalPlainTimeFromNanoseconds(total)
}

func temporalPlainTimeNanoseconds(time temporalPlainTime) *big.Int {
	return big.NewInt(int64(time.hour)*3_600_000_000_000 +
		int64(time.minute)*60_000_000_000 + int64(time.second)*1_000_000_000 +
		int64(time.millisecond)*1_000_000 + int64(time.microsecond)*1_000 + int64(time.nanosecond))
}

func temporalPlainTimeFromNanoseconds(total *big.Int) temporalPlainTime {
	const nanosecondsPerDay = 86_400_000_000_000
	total.Mod(total, big.NewInt(nanosecondsPerDay))
	remaining := total.Int64()

	result := temporalPlainTime{hour: int(remaining / 3_600_000_000_000)}
	remaining %= 3_600_000_000_000
	result.minute = int(remaining / 60_000_000_000)
	remaining %= 60_000_000_000
	result.second = int(remaining / 1_000_000_000)
	remaining %= 1_000_000_000
	result.millisecond = int(remaining / 1_000_000)
	remaining %= 1_000_000
	result.microsecond = int(remaining / 1_000)
	result.nanosecond = int(remaining % 1_000)
	return result
}

func newTemporalPlainTime(proto *Object, time temporalPlainTime) *Object {
	o := newObject(proto, ClassObject)
	o.data = &time
	return o
}

func (r *Runtime) temporalPlainTimeValue(value Value, method string) (temporalPlainTime, error) {
	if value.IsObject() {
		if time, ok := value.Object().data.(*temporalPlainTime); ok && time != nil {
			return *time, nil
		}
	}
	return temporalPlainTime{}, r.throwTypeError("%s called on an incompatible receiver", method)
}

func (r *Runtime) temporalPlainTimeStringOptions(value Value) (precision int, minuteOnly bool, step int64, mode string, err error) {
	options, err := r.strictOptions(value)
	if err != nil {
		return 0, false, 0, "", err
	}
	precision = -1
	fractional, err := r.getProp(options, r.atoms.intern("fractionalSecondDigits"), Obj(options))
	if err != nil {
		return 0, false, 0, "", err
	}
	if !fractional.IsUndefined() {
		if fractional.IsNumber() {
			n := math.Floor(fractional.Number())
			if math.IsNaN(n) || math.IsInf(n, 0) || n < 0 || n > 9 {
				return 0, false, 0, "", r.throwRangeError("fractionalSecondDigits is out of range")
			}
			precision = int(n)
		} else {
			text, err := r.toString(fractional)
			if err != nil {
				return 0, false, 0, "", err
			}
			if text.Go() != "auto" {
				return 0, false, 0, "", r.throwRangeError("invalid fractionalSecondDigits")
			}
		}
	}
	mode, err = r.stringOption(options, "roundingMode", "trunc",
		"ceil", "floor", "expand", "trunc", "halfCeil", "halfFloor", "halfExpand", "halfTrunc", "halfEven")
	if err != nil {
		return 0, false, 0, "", err
	}
	smallestRaw, err := r.stringOption(options, "smallestUnit", "")
	if err != nil {
		return 0, false, 0, "", err
	}

	if smallestRaw != "" {
		smallest, ok := normalizeTemporalUnit(smallestRaw)
		if !ok || smallest == "hour" {
			return 0, false, 0, "", r.throwRangeError("invalid smallestUnit")
		}
		step = temporalUnitNanoseconds[smallest]
		switch smallest {
		case "minute":
			minuteOnly = true
		case "second":
			precision = 0
		case "millisecond":
			precision = 3
		case "microsecond":
			precision = 6
		case "nanosecond":
			precision = 9
		}
		return precision, minuteOnly, step, mode, nil
	}
	if precision < 0 {
		return precision, false, 1, mode, nil
	}
	steps := [...]int64{1_000_000_000, 100_000_000, 10_000_000, 1_000_000, 100_000, 10_000, 1_000, 100, 10, 1}
	return precision, false, steps[precision], mode, nil
}

func (r *Runtime) toTemporalPlainTime(value, optionsValue Value) (temporalPlainTime, error) {
	if value.IsObject() {
		if time, ok := value.Object().data.(*temporalPlainTime); ok && time != nil {
			if _, err := r.temporalOverflowOption(optionsValue); err != nil {
				return temporalPlainTime{}, err
			}
			return *time, nil
		}
		if dateTime, ok := value.Object().data.(*temporalPlainDateTime); ok && dateTime != nil {
			if _, err := r.temporalOverflowOption(optionsValue); err != nil {
				return temporalPlainTime{}, err
			}
			return temporalPlainTime{dateTime.hour, dateTime.minute, dateTime.second, dateTime.millisecond, dateTime.microsecond, dateTime.nanosecond}, nil
		}
		if zoned, ok := value.Object().data.(*temporalZonedDateTime); ok && zoned != nil {
			dateTime, err := r.toTemporalPlainDateTime(value, optionsValue)
			if err != nil {
				return temporalPlainTime{}, err
			}
			return temporalPlainTime{dateTime.hour, dateTime.minute, dateTime.second, dateTime.millisecond, dateTime.microsecond, dateTime.nanosecond}, nil
		}
		return r.temporalPlainTimeFromBag(value.Object(), optionsValue)
	}
	if !value.IsString() {
		return temporalPlainTime{}, r.throwTypeError("a plain time must be a string or object")
	}
	time, err := parseTemporalPlainTime(value.String().Go())
	if err != nil {
		return temporalPlainTime{}, r.throwRangeError("invalid Temporal.PlainTime string")
	}
	if _, err := r.temporalOverflowOption(optionsValue); err != nil {
		return temporalPlainTime{}, err
	}
	return time, nil
}

func (r *Runtime) temporalPlainTimeFromBag(o *Object, optionsValue Value) (temporalPlainTime, error) {
	values := make(map[string]int, 6)
	found := false
	for _, name := range []string{"hour", "microsecond", "millisecond", "minute", "nanosecond", "second"} {
		raw, err := r.getProp(o, r.atoms.intern(name), Obj(o))
		if err != nil {
			return temporalPlainTime{}, err
		}
		if raw.IsUndefined() {
			continue
		}
		found = true
		value, err := r.temporalTruncatedInteger(raw, name)
		if err != nil {
			return temporalPlainTime{}, err
		}
		values[name] = value
	}
	overflow, err := r.temporalOverflowOption(optionsValue)
	if err != nil {
		return temporalPlainTime{}, err
	}
	if !found {
		return temporalPlainTime{}, r.throwTypeError("plain time property bag has no time fields")
	}
	if overflow == "constrain" {
		values["hour"] = max(0, min(23, values["hour"]))
		values["minute"] = max(0, min(59, values["minute"]))
		values["second"] = max(0, min(59, values["second"]))
		values["millisecond"] = max(0, min(999, values["millisecond"]))
		values["microsecond"] = max(0, min(999, values["microsecond"]))
		values["nanosecond"] = max(0, min(999, values["nanosecond"]))
	}
	result := temporalPlainTime{values["hour"], values["minute"], values["second"], values["millisecond"], values["microsecond"], values["nanosecond"]}
	if !result.valid() {
		return temporalPlainTime{}, r.throwRangeError("invalid Temporal.PlainTime")
	}
	return result, nil
}

func parseTemporalPlainTime(input string) (temporalPlainTime, error) {
	main, annotations, ok := splitTemporalAnnotations(input)
	if !ok {
		return temporalPlainTime{}, errInvalidTemporalInstant
	}
	if !strings.HasPrefix(main, "T") && !strings.HasPrefix(main, "t") && !looksLikeTemporalDateTime(input) {
		if _, err := parseTemporalPlainYearMonth(main); err == nil {
			return temporalPlainTime{}, errInvalidTemporalInstant
		}
		if _, err := parseTemporalPlainMonthDay(main); err == nil {
			return temporalPlainTime{}, errInvalidTemporalInstant
		}
	}
	calendarSeen, criticalCalendar := false, false
	var retained strings.Builder
	retained.WriteString(main)
	for _, annotation := range annotations {
		original := annotation
		critical := strings.HasPrefix(annotation, "!")
		annotation = strings.TrimPrefix(annotation, "!")
		key, _, keyed := strings.Cut(annotation, "=")
		if keyed && key == "u-ca" {
			if calendarSeen && (criticalCalendar || critical) {
				return temporalPlainTime{}, errInvalidTemporalInstant
			}
			calendarSeen = true
			criticalCalendar = criticalCalendar || critical
			continue
		}
		retained.WriteByte('[')
		retained.WriteString(original)
		retained.WriteByte(']')
	}
	input = retained.String()
	dateTimeInput := input
	switch {
	case strings.HasPrefix(input, "T"), strings.HasPrefix(input, "t"):
		dateTimeInput = "1970-01-01" + input
	case !looksLikeTemporalDateTime(input):
		dateTimeInput = "1970-01-01T" + input
	}
	dateTime, err := parseTemporalPlainDateTime(dateTimeInput)
	if err != nil {
		return temporalPlainTime{}, err
	}
	return temporalPlainTime{dateTime.hour, dateTime.minute, dateTime.second, dateTime.millisecond, dateTime.microsecond, dateTime.nanosecond}, nil
}

func looksLikeTemporalDateTime(input string) bool {
	main, _, ok := splitTemporalAnnotations(input)
	if !ok {
		return false
	}
	index := 0
	if _, ok := parseTemporalYear(main, &index); !ok {
		return false
	}
	dashed := consumeByte(main, &index, '-')
	if _, ok := parseFixedDigits(main, &index, 2); !ok || dashed && !consumeByte(main, &index, '-') {
		return false
	}
	if _, ok := parseFixedDigits(main, &index, 2); !ok {
		return false
	}
	return index < len(main) && (main[index] == 'T' || main[index] == 't' || main[index] == ' ')
}

func compareTemporalPlainTimes(left, right temporalPlainTime) int {
	leftNS := int64(left.hour)*3_600_000_000_000 + int64(left.minute)*60_000_000_000 + int64(left.second)*1_000_000_000 + int64(left.millisecond)*1_000_000 + int64(left.microsecond)*1_000 + int64(left.nanosecond)
	rightNS := int64(right.hour)*3_600_000_000_000 + int64(right.minute)*60_000_000_000 + int64(right.second)*1_000_000_000 + int64(right.millisecond)*1_000_000 + int64(right.microsecond)*1_000 + int64(right.nanosecond)
	if leftNS < rightNS {
		return -1
	}
	if leftNS > rightNS {
		return 1
	}
	return 0
}

func (t temporalPlainTime) string() string {
	return t.stringWithPrecision(-1, false)
}

func (t temporalPlainTime) stringWithPrecision(precision int, minuteOnly bool) string {
	var b strings.Builder
	writePaddedTemporalInt(&b, t.hour, 2)
	b.WriteByte(':')
	writePaddedTemporalInt(&b, t.minute, 2)
	if minuteOnly {
		return b.String()
	}
	b.WriteByte(':')
	writePaddedTemporalInt(&b, t.second, 2)
	subsecond := int64(t.millisecond)*1_000_000 + int64(t.microsecond)*1_000 + int64(t.nanosecond)
	if precision != 0 && (precision > 0 || subsecond != 0) {
		fraction := strconv.FormatInt(subsecond+temporalNanosecondsPerSecond, 10)[1:]
		b.WriteByte('.')
		if precision < 0 {
			b.WriteString(strings.TrimRight(fraction, "0"))
		} else {
			b.WriteString(fraction[:precision])
		}
	}
	return b.String()
}
