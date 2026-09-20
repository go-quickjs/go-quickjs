package vm

import (
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
	for _, method := range []string{"toString", "toJSON"} {
		name := method
		r.defMethod(proto, name, 0, func(rt *Runtime, this Value, args []Value) (Value, error) {
			time, err := rt.temporalPlainTimeValue(this, "Temporal.PlainTime.prototype."+name)
			if err != nil {
				return Undefined, err
			}
			return Str(NewString(time.string())), nil
		})
	}
	r.defMethod(proto, "valueOf", 0, func(rt *Runtime, this Value, args []Value) (Value, error) {
		if _, err := rt.temporalPlainTimeValue(this, "Temporal.PlainTime.prototype.valueOf"); err != nil {
			return Undefined, err
		}
		return Undefined, rt.throwTypeError("use Temporal.PlainTime.compare() or equals() to compare times")
	})
	r.defToStringTag(proto, "Temporal.PlainTime")
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
	var b strings.Builder
	writePaddedTemporalInt(&b, t.hour, 2)
	b.WriteByte(':')
	writePaddedTemporalInt(&b, t.minute, 2)
	b.WriteByte(':')
	writePaddedTemporalInt(&b, t.second, 2)
	subsecond := int64(t.millisecond)*1_000_000 + int64(t.microsecond)*1_000 + int64(t.nanosecond)
	if subsecond != 0 {
		fraction := strconv.FormatInt(subsecond+temporalNanosecondsPerSecond, 10)[1:]
		b.WriteByte('.')
		b.WriteString(strings.TrimRight(fraction, "0"))
	}
	return b.String()
}
