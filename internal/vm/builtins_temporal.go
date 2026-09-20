package vm

import (
	"fmt"
	"math"
	"math/big"
	"strings"
	"time"

	"github.com/go-quickjs/go-quickjs/internal/icu"
)

// initTemporalBuiltins installs a lazy namespace. Temporal is large enough
// that runtimes which never ask for it should not allocate all its prototypes.
func (r *Runtime) initTemporalBuiltins() {
	name := r.atoms.intern("Temporal")
	build := r.newNativeFunc("get Temporal", 0, func(rt *Runtime, this Value, args []Value) (Value, error) {
		temporal := rt.buildTemporal()
		rt.global.setOwnRaw(name, Obj(temporal), propWritable|propConfigurable)
		return Obj(temporal), nil
	})
	set := r.newNativeFunc("set Temporal", 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
		rt.global.setOwnRaw(name, arg(args, 0), propWritable|propConfigurable)
		return Undefined, nil
	})
	r.defineAccessor(r.global, name, build, set, propConfigurable)
}

func (r *Runtime) buildTemporal() *Object {
	temporal := newObject(r.proto.object, ClassObject)
	r.defToStringTag(temporal, "Temporal")
	r.initTemporalDuration(temporal)
	r.initTemporalInstant(temporal)
	r.initTemporalPlainDate(temporal)
	r.initTemporalPlainDateTime(temporal)
	r.initTemporalPlainMonthDay(temporal)
	r.initTemporalPlainTime(temporal)
	r.initTemporalPlainYearMonth(temporal)
	r.initTemporalZonedDateTime(temporal)
	r.initTemporalNow(temporal)
	return temporal
}

func (r *Runtime) initTemporalInstant(temporal *Object) {
	proto := newObject(r.proto.object, ClassObject)
	r.temporalInstantProto = proto
	ctor := r.newTemporalCtor(temporal, "Instant", 1, proto, func(rt *Runtime, this Value, args []Value) (Value, error) {
		if err := rt.requireNew("Temporal.Instant"); err != nil {
			return Undefined, err
		}
		nanoseconds, err := rt.toBigIntOperand(arg(args, 0))
		if err != nil {
			return Undefined, err
		}
		instant, ok := temporalInstantFromEpochNanoseconds(&nanoseconds.V)
		if !ok {
			return Undefined, rt.throwRangeError("instant is outside the Temporal range")
		}
		instanceProto, err := rt.protoFromNewTargetErr(proto)
		if err != nil {
			return Undefined, err
		}
		return Obj(newTemporalInstant(instanceProto, instant)), nil
	})

	r.defMethod(ctor, "from", 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
		instant, err := rt.toTemporalInstant(arg(args, 0))
		if err != nil {
			return Undefined, err
		}
		return Obj(newTemporalInstant(proto, instant)), nil
	})
	r.defMethod(ctor, "fromEpochMilliseconds", 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
		milliseconds, err := rt.toNumber(arg(args, 0))
		if err != nil {
			return Undefined, err
		}
		if math.IsNaN(milliseconds) || math.IsInf(milliseconds, 0) || milliseconds != math.Trunc(milliseconds) {
			return Undefined, rt.throwRangeError("epoch milliseconds must be a finite integer")
		}
		value, _ := new(big.Float).SetFloat64(milliseconds).Int(nil)
		value.Mul(value, big.NewInt(1_000_000))
		instant, ok := temporalInstantFromEpochNanoseconds(value)
		if !ok {
			return Undefined, rt.throwRangeError("instant is outside the Temporal range")
		}
		return Obj(newTemporalInstant(proto, instant)), nil
	})
	r.defMethod(ctor, "fromEpochNanoseconds", 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
		nanoseconds, err := rt.toBigIntOperand(arg(args, 0))
		if err != nil {
			return Undefined, err
		}
		instant, ok := temporalInstantFromEpochNanoseconds(&nanoseconds.V)
		if !ok {
			return Undefined, rt.throwRangeError("instant is outside the Temporal range")
		}
		return Obj(newTemporalInstant(proto, instant)), nil
	})
	r.defMethod(ctor, "compare", 2, func(rt *Runtime, this Value, args []Value) (Value, error) {
		left, err := rt.toTemporalInstant(arg(args, 0))
		if err != nil {
			return Undefined, err
		}
		right, err := rt.toTemporalInstant(arg(args, 1))
		if err != nil {
			return Undefined, err
		}
		return Int(compareTemporalInstants(left, right)), nil
	})

	r.defGetter(proto, "epochMilliseconds", func(rt *Runtime, this Value, args []Value) (Value, error) {
		instant, err := rt.temporalInstantValue(this, "get Temporal.Instant.prototype.epochMilliseconds")
		if err != nil {
			return Undefined, err
		}
		milliseconds := instant.epochSeconds*1000 + int64(instant.nanosecond)/1_000_000
		return Float(float64(milliseconds)), nil
	})
	r.defGetter(proto, "epochNanoseconds", func(rt *Runtime, this Value, args []Value) (Value, error) {
		instant, err := rt.temporalInstantValue(this, "get Temporal.Instant.prototype.epochNanoseconds")
		if err != nil {
			return Undefined, err
		}
		value := &BigInt{}
		value.V.Set(instant.epochNanoseconds())
		return Big(value), nil
	})
	r.defMethod(proto, "equals", 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
		instant, err := rt.temporalInstantValue(this, "Temporal.Instant.prototype.equals")
		if err != nil {
			return Undefined, err
		}
		other, err := rt.toTemporalInstant(arg(args, 0))
		if err != nil {
			return Undefined, err
		}
		return Bool(compareTemporalInstants(instant, other) == 0), nil
	})
	for _, operation := range []struct {
		name string
		sign int64
	}{{"add", 1}, {"subtract", -1}} {
		op := operation
		r.defMethod(proto, op.name, 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
			instant, err := rt.temporalInstantValue(this, "Temporal.Instant.prototype."+op.name)
			if err != nil {
				return Undefined, err
			}
			duration, err := rt.toTemporalDuration(arg(args, 0))
			if err != nil {
				return Undefined, err
			}
			delta, ok := duration.timeNanoseconds()
			if !ok {
				return Undefined, rt.throwRangeError("calendar units cannot be added to an instant")
			}
			if op.sign < 0 {
				delta.Neg(delta)
			}
			result, ok := instant.addNanoseconds(delta)
			if !ok {
				return Undefined, rt.throwRangeError("instant is outside the Temporal range")
			}
			return Obj(newTemporalInstant(proto, result)), nil
		})
	}
	r.defMethod(proto, "toString", 0, func(rt *Runtime, this Value, args []Value) (Value, error) {
		instant, err := rt.temporalInstantValue(this, "Temporal.Instant.prototype.toString")
		if err != nil {
			return Undefined, err
		}
		precision, minuteOnly, step, mode, timeZoneValue, err := rt.temporalInstantStringOptions(arg(args, 0))
		if err != nil {
			return Undefined, err
		}
		rounded := roundTemporalBigIntAsIfPositive(instant.epochNanoseconds(), big.NewInt(step), mode)
		instant, ok := temporalInstantFromEpochNanoseconds(rounded)
		if !ok {
			return Undefined, rt.throwRangeError("rounded instant is outside the Temporal range")
		}
		if timeZoneValue.IsUndefined() {
			dateTime := temporalPlainDateTime{temporalISODateTime: instant.isoDateTimeUTC(), calendar: "iso8601"}
			return Str(NewString(dateTime.stringWithPrecision("never", precision, minuteOnly) + "Z")), nil
		}
		timeZone, err := rt.toTemporalTimeZoneIdentifier(timeZoneValue)
		if err != nil {
			return Undefined, err
		}
		zoned, err := rt.newTemporalZonedDateTime(instant, timeZone, Str(NewString("iso8601")))
		if err != nil {
			return Undefined, err
		}
		dateTime := temporalPlainDateTime{temporalISODateTime: zoned.localISODateTime(), calendar: "iso8601"}
		text := dateTime.stringWithPrecision("never", precision, minuteOnly) + formatTemporalOffset(zoned.offsetSeconds())
		return Str(NewString(text)), nil
	})
	r.defMethod(proto, "toZonedDateTimeISO", 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
		instant, err := rt.temporalInstantValue(this, "Temporal.Instant.prototype.toZonedDateTimeISO")
		if err != nil {
			return Undefined, err
		}
		timeZone, err := rt.toTemporalTimeZoneIdentifier(arg(args, 0))
		if err != nil {
			return Undefined, err
		}
		zoned, err := rt.newTemporalZonedDateTime(instant, timeZone, Str(NewString("iso8601")))
		if err != nil {
			return Undefined, err
		}
		o := newObject(rt.temporalZonedDateTimeProto, ClassObject)
		o.data = zoned
		return Obj(o), nil
	})
	r.defMethod(proto, "toJSON", 0, func(rt *Runtime, this Value, args []Value) (Value, error) {
		instant, err := rt.temporalInstantValue(this, "Temporal.Instant.prototype.toJSON")
		if err != nil {
			return Undefined, err
		}
		return Str(NewString(instant.string())), nil
	})
	r.defMethod(proto, "toLocaleString", 0, func(rt *Runtime, this Value, args []Value) (Value, error) {
		instant, err := rt.temporalInstantValue(this, "Temporal.Instant.prototype.toLocaleString")
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
		milliseconds := float64(instant.epochSeconds*1000 + int64(instant.nanosecond)/1_000_000)
		return Str(NewString(options.format(options.at(milliseconds)))), nil
	})
	r.defMethod(proto, "valueOf", 0, func(rt *Runtime, this Value, args []Value) (Value, error) {
		if _, err := rt.temporalInstantValue(this, "Temporal.Instant.prototype.valueOf"); err != nil {
			return Undefined, err
		}
		return Undefined, rt.throwTypeError("use Temporal.Instant.compare() or equals() to compare instants")
	})
	r.defToStringTag(proto, "Temporal.Instant")
	r.initTemporalInstantOperations(proto)
}

func (r *Runtime) temporalInstantStringOptions(value Value) (precision int, minuteOnly bool, step int64, mode string, timeZone Value, err error) {
	options, err := r.strictOptions(value)
	if err != nil {
		return 0, false, 0, "", Undefined, err
	}
	precision = -1
	fractional, err := r.getProp(options, r.atoms.intern("fractionalSecondDigits"), Obj(options))
	if err != nil {
		return 0, false, 0, "", Undefined, err
	}
	if !fractional.IsUndefined() {
		if fractional.IsNumber() {
			n := math.Floor(fractional.Number())
			if math.IsNaN(n) || math.IsInf(n, 0) || n < 0 || n > 9 {
				return 0, false, 0, "", Undefined, r.throwRangeError("fractionalSecondDigits is out of range")
			}
			precision = int(n)
		} else {
			text, err := r.toString(fractional)
			if err != nil {
				return 0, false, 0, "", Undefined, err
			}
			if text.Go() != "auto" {
				return 0, false, 0, "", Undefined, r.throwRangeError("invalid fractionalSecondDigits")
			}
		}
	}
	mode, err = r.stringOption(options, "roundingMode", "trunc",
		"ceil", "floor", "expand", "trunc", "halfCeil", "halfFloor", "halfExpand", "halfTrunc", "halfEven")
	if err != nil {
		return 0, false, 0, "", Undefined, err
	}
	smallestValue, err := r.getProp(options, r.atoms.intern("smallestUnit"), Obj(options))
	if err != nil {
		return 0, false, 0, "", Undefined, err
	}
	smallestRaw := ""
	if !smallestValue.IsUndefined() {
		text, err := r.toString(smallestValue)
		if err != nil {
			return 0, false, 0, "", Undefined, err
		}
		smallestRaw = text.Go()
	}
	timeZone, err = r.getProp(options, r.atoms.intern("timeZone"), Obj(options))
	if err != nil {
		return 0, false, 0, "", Undefined, err
	}

	if smallestRaw != "" {
		smallest, ok := normalizeTemporalUnit(smallestRaw)
		if !ok || smallest == "hour" {
			return 0, false, 0, "", Undefined, r.throwRangeError("invalid smallestUnit")
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
		return precision, minuteOnly, step, mode, timeZone, nil
	}
	if precision < 0 {
		return precision, false, 1, mode, timeZone, nil
	}
	steps := [...]int64{1_000_000_000, 100_000_000, 10_000_000, 1_000_000, 100_000, 10_000, 1_000, 100, 10, 1}
	return precision, false, steps[precision], mode, timeZone, nil
}

func (r *Runtime) newTemporalCtor(namespace *Object, name string, length int, proto *Object, fn NativeFunc) *Object {
	ctor, data := r.newSlabFuncObject(r.proto.function, ClassFunction)
	*data = funcData{native: fn, name: name, length: length, ctorKind: ctorBase}
	ctor.setOwnRaw(atomPrototype, Obj(proto), 0)
	proto.setOwnRaw(atomConstructor, Obj(ctor), propWritable|propConfigurable)
	r.defValue(namespace, name, Obj(ctor))
	return ctor
}

func newTemporalInstant(proto *Object, instant temporalInstant) *Object {
	o := newObject(proto, ClassObject)
	o.data = &instant
	return o
}

func (r *Runtime) temporalInstantValue(value Value, method string) (temporalInstant, error) {
	if value.IsObject() {
		if instant, ok := value.Object().data.(*temporalInstant); ok && instant != nil {
			return *instant, nil
		}
	}
	return temporalInstant{}, r.throwTypeError("%s called on an incompatible receiver", method)
}

func (r *Runtime) toTemporalInstant(value Value) (temporalInstant, error) {
	if value.IsObject() {
		if instant, ok := value.Object().data.(*temporalInstant); ok && instant != nil {
			return *instant, nil
		}
		if zoned, ok := value.Object().data.(*temporalZonedDateTime); ok && zoned != nil {
			return zoned.instant, nil
		}
		primitive, err := r.toPrimitive(value, hintString)
		if err != nil {
			return temporalInstant{}, err
		}
		value = primitive
	}
	if !value.IsString() {
		return temporalInstant{}, r.throwTypeError("an instant must be a Temporal.Instant or string")
	}
	instant, err := parseTemporalInstant(value.String().Go())
	if err != nil {
		return temporalInstant{}, r.throwRangeError("invalid Temporal.Instant string")
	}
	return instant, nil
}

type temporalZonedDateTime struct {
	instant            temporalInstant
	timeZone           string
	calendar           string
	zone               *icu.TimeZone
	fixedOffsetSeconds int
	fixed              bool
}

func (r *Runtime) initTemporalZonedDateTime(temporal *Object) {
	proto := newObject(r.proto.object, ClassObject)
	r.temporalZonedDateTimeProto = proto
	ctor := r.newTemporalCtor(temporal, "ZonedDateTime", 2, proto, func(rt *Runtime, this Value, args []Value) (Value, error) {
		if err := rt.requireNew("Temporal.ZonedDateTime"); err != nil {
			return Undefined, err
		}
		nanoseconds, err := rt.toBigIntOperand(arg(args, 0))
		if err != nil {
			return Undefined, err
		}
		instant, ok := temporalInstantFromEpochNanoseconds(&nanoseconds.V)
		if !ok {
			return Undefined, rt.throwRangeError("instant is outside the Temporal range")
		}
		zoneValue := arg(args, 1)
		if !zoneValue.IsString() {
			return Undefined, rt.throwTypeError("time zone must be a string")
		}
		zoned, err := rt.newTemporalZonedDateTime(instant, zoneValue.String().Go(), arg(args, 2))
		if err != nil {
			return Undefined, err
		}
		instanceProto, err := rt.protoFromNewTargetErr(proto)
		if err != nil {
			return Undefined, err
		}
		o := newObject(instanceProto, ClassObject)
		o.data = zoned
		return Obj(o), nil
	})
	r.defMethod(ctor, "from", 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
		zoned, err := rt.toTemporalZonedDateTimeWithOptions(arg(args, 0), arg(args, 1))
		if err != nil {
			return Undefined, err
		}
		copy := *zoned
		return Obj(newTemporalZonedDateTimeObject(proto, &copy)), nil
	})
	r.defMethod(ctor, "compare", 2, func(rt *Runtime, this Value, args []Value) (Value, error) {
		left, err := rt.toTemporalZonedDateTimeWithOptions(arg(args, 0), Undefined)
		if err != nil {
			return Undefined, err
		}
		right, err := rt.toTemporalZonedDateTimeWithOptions(arg(args, 1), Undefined)
		if err != nil {
			return Undefined, err
		}
		return Int(compareTemporalInstants(left.instant, right.instant)), nil
	})

	r.defGetter(proto, "epochMilliseconds", func(rt *Runtime, this Value, args []Value) (Value, error) {
		zoned, err := rt.temporalZonedDateTimeValue(this, "get Temporal.ZonedDateTime.prototype.epochMilliseconds")
		if err != nil {
			return Undefined, err
		}
		return Float(float64(zoned.instant.epochSeconds*1000 + int64(zoned.instant.nanosecond)/1_000_000)), nil
	})
	r.defGetter(proto, "epochNanoseconds", func(rt *Runtime, this Value, args []Value) (Value, error) {
		zoned, err := rt.temporalZonedDateTimeValue(this, "get Temporal.ZonedDateTime.prototype.epochNanoseconds")
		if err != nil {
			return Undefined, err
		}
		value := &BigInt{}
		value.V.Set(zoned.instant.epochNanoseconds())
		return Big(value), nil
	})
	r.defGetter(proto, "timeZoneId", func(rt *Runtime, this Value, args []Value) (Value, error) {
		zoned, err := rt.temporalZonedDateTimeValue(this, "get Temporal.ZonedDateTime.prototype.timeZoneId")
		if err != nil {
			return Undefined, err
		}
		return Str(NewString(zoned.timeZone)), nil
	})
	r.defGetter(proto, "calendarId", func(rt *Runtime, this Value, args []Value) (Value, error) {
		zoned, err := rt.temporalZonedDateTimeValue(this, "get Temporal.ZonedDateTime.prototype.calendarId")
		if err != nil {
			return Undefined, err
		}
		return Str(NewString(zoned.calendar)), nil
	})
	for _, property := range []string{"year", "month", "monthCode", "day", "era", "eraYear", "hour", "minute", "second", "millisecond", "microsecond", "nanosecond"} {
		name := property
		r.defGetter(proto, name, func(rt *Runtime, this Value, args []Value) (Value, error) {
			zoned, err := rt.temporalZonedDateTimeValue(this, "get Temporal.ZonedDateTime.prototype."+name)
			if err != nil {
				return Undefined, err
			}
			dateTime := zoned.localISODateTime()
			calendarDate := temporalPlainDate{
				year: dateTime.year, month: dateTime.month, day: dateTime.day, calendar: zoned.calendar,
			}.calendarDate()
			switch name {
			case "year":
				return Int(calendarDate.Year), nil
			case "month":
				return Int(calendarDate.Month), nil
			case "monthCode":
				code := fmt.Sprintf("M%02d", calendarDate.Month)
				if calendarDate.Leap {
					code += "L"
				}
				return Str(NewString(code)), nil
			case "day":
				return Int(calendarDate.Day), nil
			case "era":
				era, ok := temporalCalendarEra(zoned.calendar, calendarDate)
				if !ok {
					return Undefined, nil
				}
				return Str(NewString(era)), nil
			case "eraYear":
				if _, ok := temporalCalendarEra(zoned.calendar, calendarDate); !ok {
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
			zoned, err := rt.temporalZonedDateTimeValue(this, "get Temporal.ZonedDateTime.prototype."+name)
			if err != nil {
				return Undefined, err
			}
			dateTime := zoned.localISODateTime()
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
	r.defGetter(proto, "offsetNanoseconds", func(rt *Runtime, this Value, args []Value) (Value, error) {
		zoned, err := rt.temporalZonedDateTimeValue(this, "get Temporal.ZonedDateTime.prototype.offsetNanoseconds")
		if err != nil {
			return Undefined, err
		}
		return Float(float64(int64(zoned.offsetSeconds()) * temporalNanosecondsPerSecond)), nil
	})
	r.defGetter(proto, "offset", func(rt *Runtime, this Value, args []Value) (Value, error) {
		zoned, err := rt.temporalZonedDateTimeValue(this, "get Temporal.ZonedDateTime.prototype.offset")
		if err != nil {
			return Undefined, err
		}
		return Str(NewString(formatTemporalOffset(zoned.offsetSeconds()))), nil
	})
	r.defGetter(proto, "hoursInDay", func(rt *Runtime, this Value, args []Value) (Value, error) {
		zoned, err := rt.temporalZonedDateTimeValue(this, "get Temporal.ZonedDateTime.prototype.hoursInDay")
		if err != nil {
			return Undefined, err
		}
		dateTime := zoned.localISODateTime()
		date := temporalPlainDate{year: dateTime.year, month: dateTime.month, day: dateTime.day, calendar: zoned.calendar}
		start, ok := zoned.startOfDayInstant(date)
		if !ok {
			return Undefined, rt.throwRangeError("start of day is outside the Temporal range")
		}
		nextDays := isoDaysFromCivil(int64(date.year), date.month, date.day) + 1
		nextYear, nextMonth, nextDay := isoCivilFromDays(nextDays)
		next := temporalPlainDate{year: nextYear, month: nextMonth, day: nextDay, calendar: zoned.calendar}
		nextStart, ok := zoned.startOfDayInstant(next)
		if !ok {
			return Undefined, rt.throwRangeError("next day is outside the Temporal range")
		}
		difference := new(big.Int).Sub(nextStart.epochNanoseconds(), start.epochNanoseconds())
		hours, _ := new(big.Rat).SetFrac(difference, big.NewInt(3600*temporalNanosecondsPerSecond)).Float64()
		return Float(hours), nil
	})

	r.defMethod(proto, "toInstant", 0, func(rt *Runtime, this Value, args []Value) (Value, error) {
		zoned, err := rt.temporalZonedDateTimeValue(this, "Temporal.ZonedDateTime.prototype.toInstant")
		if err != nil {
			return Undefined, err
		}
		return Obj(newTemporalInstant(rt.temporalInstantProto, zoned.instant)), nil
	})
	r.defMethod(proto, "toPlainDateTime", 0, func(rt *Runtime, this Value, args []Value) (Value, error) {
		zoned, err := rt.temporalZonedDateTimeValue(this, "Temporal.ZonedDateTime.prototype.toPlainDateTime")
		if err != nil {
			return Undefined, err
		}
		dateTime := temporalPlainDateTime{temporalISODateTime: zoned.localISODateTime(), calendar: zoned.calendar}
		return Obj(newTemporalPlainDateTime(rt.temporalPlainDateTimeProto, dateTime)), nil
	})
	r.defMethod(proto, "toPlainDate", 0, func(rt *Runtime, this Value, args []Value) (Value, error) {
		zoned, err := rt.temporalZonedDateTimeValue(this, "Temporal.ZonedDateTime.prototype.toPlainDate")
		if err != nil {
			return Undefined, err
		}
		dateTime := zoned.localISODateTime()
		date := temporalPlainDate{year: dateTime.year, month: dateTime.month, day: dateTime.day, calendar: zoned.calendar}
		return Obj(newTemporalPlainDate(rt.temporalPlainDateProto, date)), nil
	})
	r.defMethod(proto, "toPlainTime", 0, func(rt *Runtime, this Value, args []Value) (Value, error) {
		zoned, err := rt.temporalZonedDateTimeValue(this, "Temporal.ZonedDateTime.prototype.toPlainTime")
		if err != nil {
			return Undefined, err
		}
		dateTime := zoned.localISODateTime()
		time := temporalPlainTime{dateTime.hour, dateTime.minute, dateTime.second, dateTime.millisecond, dateTime.microsecond, dateTime.nanosecond}
		return Obj(newTemporalPlainTime(rt.temporalPlainTimeProto, time)), nil
	})
	r.defMethod(proto, "startOfDay", 0, func(rt *Runtime, this Value, args []Value) (Value, error) {
		zoned, err := rt.temporalZonedDateTimeValue(this, "Temporal.ZonedDateTime.prototype.startOfDay")
		if err != nil {
			return Undefined, err
		}
		dateTime := zoned.localISODateTime()
		date := temporalPlainDate{year: dateTime.year, month: dateTime.month, day: dateTime.day, calendar: zoned.calendar}
		instant, ok := zoned.startOfDayInstant(date)
		if !ok {
			return Undefined, rt.throwRangeError("start of day is outside the Temporal range")
		}
		result := *zoned
		result.instant = instant
		return Obj(newTemporalZonedDateTimeObject(rt.temporalZonedDateTimeProto, &result)), nil
	})
	for _, operation := range []struct {
		name string
		sign int
	}{{"add", 1}, {"subtract", -1}} {
		op := operation
		r.defMethod(proto, op.name, 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
			zoned, err := rt.temporalZonedDateTimeValue(this, "Temporal.ZonedDateTime.prototype."+op.name)
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
			if op.sign < 0 {
				fields := duration.fields()
				for index := range fields {
					if fields[index] != 0 {
						fields[index] = -fields[index]
					}
				}
				duration = durationFromFields(fields)
			}
			instant, err := rt.addTemporalDurationToZonedInstantWithOverflow(zoned, duration, overflow)
			if err != nil {
				return Undefined, err
			}
			result := *zoned
			result.instant = instant
			return Obj(newTemporalZonedDateTimeObject(rt.temporalZonedDateTimeProto, &result)), nil
		})
	}
	r.defMethod(proto, "with", 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
		zoned, err := rt.temporalZonedDateTimeValue(this, "Temporal.ZonedDateTime.prototype.with")
		if err != nil {
			return Undefined, err
		}
		result, err := rt.temporalZonedDateTimeWith(zoned, arg(args, 0), arg(args, 1))
		if err != nil {
			return Undefined, err
		}
		return Obj(newTemporalZonedDateTimeObject(rt.temporalZonedDateTimeProto, result)), nil
	})
	r.defMethod(proto, "withPlainTime", 0, func(rt *Runtime, this Value, args []Value) (Value, error) {
		zoned, err := rt.temporalZonedDateTimeValue(this, "Temporal.ZonedDateTime.prototype.withPlainTime")
		if err != nil {
			return Undefined, err
		}
		local := zoned.localISODateTime()
		var instant temporalInstant
		if arg(args, 0).IsUndefined() {
			date := temporalPlainDate{
				year: local.year, month: local.month, day: local.day,
				calendar: zoned.calendar,
			}
			var ok bool
			instant, ok = zoned.startOfDayInstant(date)
			if !ok {
				return Undefined, rt.throwRangeError("start of day is outside the Temporal range")
			}
		} else {
			plainTime, err := rt.toTemporalPlainTime(arg(args, 0), Undefined)
			if err != nil {
				return Undefined, err
			}
			local.hour, local.minute, local.second = plainTime.hour, plainTime.minute, plainTime.second
			local.millisecond, local.microsecond, local.nanosecond = plainTime.millisecond, plainTime.microsecond, plainTime.nanosecond
			var ok bool
			instant, ok = zoned.compatibleInstant(local)
			if !ok {
				return Undefined, rt.throwRangeError("plain time is outside the Temporal range")
			}
		}
		result := *zoned
		result.instant = instant
		return Obj(newTemporalZonedDateTimeObject(rt.temporalZonedDateTimeProto, &result)), nil
	})
	r.defMethod(proto, "withTimeZone", 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
		zoned, err := rt.temporalZonedDateTimeValue(this, "Temporal.ZonedDateTime.prototype.withTimeZone")
		if err != nil {
			return Undefined, err
		}
		timeZone, err := rt.toTemporalTimeZoneIdentifier(arg(args, 0))
		if err != nil {
			return Undefined, err
		}
		result, err := rt.newTemporalZonedDateTime(zoned.instant, timeZone, Str(NewString(zoned.calendar)))
		if err != nil {
			return Undefined, err
		}
		return Obj(newTemporalZonedDateTimeObject(rt.temporalZonedDateTimeProto, result)), nil
	})
	r.defMethod(proto, "withCalendar", 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
		zoned, err := rt.temporalZonedDateTimeValue(this, "Temporal.ZonedDateTime.prototype.withCalendar")
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
		result := *zoned
		result.calendar = calendar
		return Obj(newTemporalZonedDateTimeObject(rt.temporalZonedDateTimeProto, &result)), nil
	})
	r.defMethod(proto, "equals", 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
		zoned, err := rt.temporalZonedDateTimeValue(this, "Temporal.ZonedDateTime.prototype.equals")
		if err != nil {
			return Undefined, err
		}
		other, err := rt.toTemporalZonedDateTime(arg(args, 0))
		if err != nil {
			return Undefined, err
		}
		return Bool(compareTemporalInstants(zoned.instant, other.instant) == 0 &&
			zoned.timeZone == other.timeZone && zoned.calendar == other.calendar), nil
	})
	r.defMethod(proto, "toString", 0, func(rt *Runtime, this Value, args []Value) (Value, error) {
		zoned, err := rt.temporalZonedDateTimeValue(this, "Temporal.ZonedDateTime.prototype.toString")
		if err != nil {
			return Undefined, err
		}
		return Str(NewString(zoned.string())), nil
	})
	r.defMethod(proto, "toJSON", 0, func(rt *Runtime, this Value, args []Value) (Value, error) {
		zoned, err := rt.temporalZonedDateTimeValue(this, "Temporal.ZonedDateTime.prototype.toJSON")
		if err != nil {
			return Undefined, err
		}
		return Str(NewString(zoned.string())), nil
	})
	r.defMethod(proto, "valueOf", 0, func(rt *Runtime, this Value, args []Value) (Value, error) {
		if _, err := rt.temporalZonedDateTimeValue(this, "Temporal.ZonedDateTime.prototype.valueOf"); err != nil {
			return Undefined, err
		}
		return Undefined, rt.throwTypeError("use Temporal.ZonedDateTime.compare() or equals() to compare date-times")
	})
	r.defToStringTag(proto, "Temporal.ZonedDateTime")
}

func (r *Runtime) initTemporalNow(temporal *Object) {
	now := newObject(r.proto.object, ClassObject)
	r.defMethod(now, "instant", 0, func(rt *Runtime, this Value, args []Value) (Value, error) {
		instant := temporalCurrentInstant()
		return Obj(newTemporalInstant(rt.temporalInstantProto, instant)), nil
	})
	r.defMethod(now, "timeZoneId", 0, func(rt *Runtime, this Value, args []Value) (Value, error) {
		return Str(NewString(rt.localZoneName())), nil
	})
	r.defMethod(now, "zonedDateTimeISO", 0, func(rt *Runtime, this Value, args []Value) (Value, error) {
		zoned, err := rt.temporalNowZonedDateTime(arg(args, 0))
		if err != nil {
			return Undefined, err
		}
		o := newObject(rt.temporalZonedDateTimeProto, ClassObject)
		o.data = zoned
		return Obj(o), nil
	})
	r.defMethod(now, "plainDateTimeISO", 0, func(rt *Runtime, this Value, args []Value) (Value, error) {
		zoned, err := rt.temporalNowZonedDateTime(arg(args, 0))
		if err != nil {
			return Undefined, err
		}
		dateTime := temporalPlainDateTime{temporalISODateTime: zoned.localISODateTime(), calendar: "iso8601"}
		return Obj(newTemporalPlainDateTime(rt.temporalPlainDateTimeProto, dateTime)), nil
	})
	r.defMethod(now, "plainDateISO", 0, func(rt *Runtime, this Value, args []Value) (Value, error) {
		zoned, err := rt.temporalNowZonedDateTime(arg(args, 0))
		if err != nil {
			return Undefined, err
		}
		dateTime := zoned.localISODateTime()
		date := temporalPlainDate{year: dateTime.year, month: dateTime.month, day: dateTime.day, calendar: "iso8601"}
		return Obj(newTemporalPlainDate(rt.temporalPlainDateProto, date)), nil
	})
	r.defMethod(now, "plainTimeISO", 0, func(rt *Runtime, this Value, args []Value) (Value, error) {
		zoned, err := rt.temporalNowZonedDateTime(arg(args, 0))
		if err != nil {
			return Undefined, err
		}
		dateTime := zoned.localISODateTime()
		plainTime := temporalPlainTime{dateTime.hour, dateTime.minute, dateTime.second, dateTime.millisecond, dateTime.microsecond, dateTime.nanosecond}
		return Obj(newTemporalPlainTime(rt.temporalPlainTimeProto, plainTime)), nil
	})
	r.defToStringTag(now, "Temporal.Now")
	r.defValue(temporal, "Now", Obj(now))
}

func temporalCurrentInstant() temporalInstant {
	now := time.Now()
	return temporalInstant{epochSeconds: now.Unix(), nanosecond: uint32(now.Nanosecond())}
}

func (r *Runtime) temporalNowZonedDateTime(timeZoneValue Value) (*temporalZonedDateTime, error) {
	timeZone := r.localZoneName()
	var err error
	if !timeZoneValue.IsUndefined() {
		timeZone, err = r.toTemporalTimeZoneIdentifier(timeZoneValue)
		if err != nil {
			return nil, err
		}
	}
	return r.newTemporalZonedDateTime(temporalCurrentInstant(), timeZone, Str(NewString("iso8601")))
}

func (r *Runtime) newTemporalZonedDateTime(instant temporalInstant, zoneName string, calendarValue Value) (*temporalZonedDateTime, error) {
	calendar := "iso8601"
	if !calendarValue.IsUndefined() {
		if !calendarValue.IsString() {
			return nil, r.throwTypeError("calendar must be a string")
		}
		calendar = strings.ToLower(calendarValue.String().Go())
		if calendar == "" {
			return nil, r.throwRangeError("invalid calendar")
		}
	}
	if minutes, name, ok := parseZoneOffset(zoneName); ok {
		return &temporalZonedDateTime{
			instant: instant, timeZone: name, calendar: calendar,
			fixedOffsetSeconds: minutes * 60, fixed: true,
		}, nil
	}
	canonical, ok := icu.CanonicalZone(zoneName)
	if !ok {
		return nil, r.throwRangeError("unknown time zone: %s", zoneName)
	}
	target := canonical
	if alias, ok := icu.ZoneTarget(canonical); ok {
		target = alias
	}
	zone, err := icu.LoadTimeZone(target)
	if err != nil {
		return nil, r.throwRangeError("unknown time zone: %s", zoneName)
	}
	return &temporalZonedDateTime{
		instant: instant, timeZone: canonical, calendar: calendar, zone: zone,
	}, nil
}

func (r *Runtime) temporalZonedDateTimeValue(value Value, method string) (*temporalZonedDateTime, error) {
	if value.IsObject() {
		if zoned, ok := value.Object().data.(*temporalZonedDateTime); ok && zoned != nil {
			return zoned, nil
		}
	}
	return nil, r.throwTypeError("%s called on an incompatible receiver", method)
}

func newTemporalZonedDateTimeObject(proto *Object, zoned *temporalZonedDateTime) *Object {
	o := newObject(proto, ClassObject)
	o.data = zoned
	return o
}

func (r *Runtime) toTemporalZonedDateTime(value Value) (*temporalZonedDateTime, error) {
	return r.toTemporalZonedDateTimeWithOptions(value, Undefined)
}

func (z *temporalZonedDateTime) offsetSeconds() int {
	if z.fixed {
		return z.fixedOffsetSeconds
	}
	return z.zone.OffsetAt(z.instant.epochSeconds).OffsetSeconds
}

func (z *temporalZonedDateTime) localISODateTime() temporalISODateTime {
	localSeconds := z.instant.epochSeconds + int64(z.offsetSeconds())
	days := floorDivInt64(localSeconds, temporalSecondsPerDay)
	seconds := localSeconds - days*temporalSecondsPerDay
	year, month, day := isoCivilFromDays(days)
	subsecond := int(z.instant.nanosecond)
	return temporalISODateTime{
		year: year, month: month, day: day,
		hour: int(seconds / 3600), minute: int(seconds / 60 % 60), second: int(seconds % 60),
		millisecond: subsecond / 1_000_000,
		microsecond: subsecond / 1_000 % 1_000,
		nanosecond:  subsecond % 1_000,
	}
}

func (z *temporalZonedDateTime) compatibleInstant(dateTime temporalISODateTime) (temporalInstant, bool) {
	return z.disambiguatedInstant(dateTime, "compatible")
}

func (z *temporalZonedDateTime) disambiguatedInstant(dateTime temporalISODateTime, disambiguation string) (temporalInstant, bool) {
	localSeconds := dateTime.localEpochSeconds()
	epochSeconds := int64(0)
	if z.fixed {
		epochSeconds = localSeconds - int64(z.fixedOffsetSeconds)
	} else {
		possible := z.zone.PossibleInstants(localSeconds)
		switch len(possible) {
		case 1:
			epochSeconds = possible[0]
		case 2:
			switch disambiguation {
			case "compatible", "earlier":
				epochSeconds = possible[0]
			case "later":
				epochSeconds = possible[1]
			default:
				return temporalInstant{}, false
			}
		default:
			if disambiguation == "reject" {
				return temporalInstant{}, false
			}
			const day = int64(24 * 60 * 60)
			before := z.zone.OffsetAt(localSeconds - day).OffsetSeconds
			after := z.zone.OffsetAt(localSeconds + day).OffsetSeconds
			gap := int64(after - before)
			if gap <= 0 {
				return temporalInstant{}, false
			}
			shifted := localSeconds + gap
			if disambiguation == "earlier" {
				shifted = localSeconds - gap
			}
			possible = z.zone.PossibleInstants(shifted)
			if len(possible) == 0 {
				return temporalInstant{}, false
			}
			if disambiguation == "earlier" {
				epochSeconds = possible[0]
			} else {
				epochSeconds = possible[len(possible)-1]
			}
		}
	}
	total := new(big.Int).Mul(big.NewInt(epochSeconds), big.NewInt(temporalNanosecondsPerSecond))
	total.Add(total, big.NewInt(dateTime.subsecondNanoseconds()))
	return temporalInstantFromEpochNanoseconds(total)
}

func (z *temporalZonedDateTime) startOfDayInstant(date temporalPlainDate) (temporalInstant, bool) {
	localSeconds := isoDaysFromCivil(int64(date.year), date.month, date.day) * temporalSecondsPerDay
	epochSeconds := int64(0)
	if z.fixed {
		epochSeconds = localSeconds - int64(z.fixedOffsetSeconds)
	} else {
		var ok bool
		epochSeconds, ok = z.zone.StartOfDay(localSeconds)
		if !ok {
			return temporalInstant{}, false
		}
	}
	total := new(big.Int).Mul(big.NewInt(epochSeconds), big.NewInt(temporalNanosecondsPerSecond))
	return temporalInstantFromEpochNanoseconds(total)
}

func (z *temporalZonedDateTime) string() string {
	offset := z.offsetSeconds()
	local := z.instant
	local.epochSeconds += int64(offset)
	text := strings.TrimSuffix(local.string(), "Z")
	text += formatTemporalOffset(offset) + "[" + z.timeZone + "]"
	if z.calendar != "iso8601" {
		text += "[u-ca=" + z.calendar + "]"
	}
	return text
}

func formatTemporalOffset(offset int) string {
	sign := '+'
	away := offset
	if away < 0 {
		sign, away = '-', -away
	}
	text := fmt.Sprintf("%c%02d:%02d", sign, away/3600, away/60%60)
	if away%60 != 0 {
		text += fmt.Sprintf(":%02d", away%60)
	}
	return text
}

func parseTemporalZonedDateTimeString(input string) (temporalInstant, string, string, error) {
	_, annotations, ok := splitTemporalAnnotations(input)
	if !ok || !validInstantAnnotations(annotations) {
		return temporalInstant{}, "", "", errInvalidTemporalInstant
	}
	zone, calendar := "", "iso8601"
	for _, annotation := range annotations {
		annotation = strings.TrimPrefix(annotation, "!")
		if key, value, keyed := strings.Cut(annotation, "="); keyed {
			if key == "u-ca" && calendar == "iso8601" {
				calendar = canonicalSetting("ca", asciiLower(value))
				if !icu.HasCalendar(calendar) {
					return temporalInstant{}, "", "", errInvalidTemporalInstant
				}
			}
			continue
		}
		zone = annotation
	}
	if zone == "" {
		return temporalInstant{}, "", "", errInvalidTemporalInstant
	}
	instant, err := parseTemporalInstant(input)
	if err == nil {
		return instant, zone, calendar, nil
	}
	dateTime, err := parseTemporalPlainDateTime(input)
	if err != nil {
		return temporalInstant{}, "", "", err
	}
	epochSeconds := int64(0)
	if minutes, _, ok := parseZoneOffset(zone); ok {
		epochSeconds = dateTime.localEpochSeconds() - int64(minutes*60)
	} else {
		canonical, ok := icu.CanonicalZone(zone)
		if !ok {
			return temporalInstant{}, "", "", errInvalidTemporalInstant
		}
		target := canonical
		if alias, ok := icu.ZoneTarget(canonical); ok {
			target = alias
		}
		timeZone, err := icu.LoadTimeZone(target)
		if err != nil {
			return temporalInstant{}, "", "", errInvalidTemporalInstant
		}
		epochSeconds, ok = timeZone.CompatibleInstant(dateTime.localEpochSeconds())
		if !ok {
			return temporalInstant{}, "", "", errInvalidTemporalInstant
		}
	}
	total := new(big.Int).Mul(big.NewInt(epochSeconds), big.NewInt(temporalNanosecondsPerSecond))
	total.Add(total, big.NewInt(dateTime.subsecondNanoseconds()))
	instant, ok = temporalInstantFromEpochNanoseconds(total)
	if !ok {
		return temporalInstant{}, "", "", errInvalidTemporalInstant
	}
	return instant, zone, calendar, nil
}

func (r *Runtime) toTemporalTimeZoneIdentifier(value Value) (string, error) {
	if value.IsObject() {
		if zoned, ok := value.Object().data.(*temporalZonedDateTime); ok && zoned != nil {
			return zoned.timeZone, nil
		}
	}
	if !value.IsString() {
		return "", r.throwTypeError("time zone must be a string")
	}
	name, ok := parseTemporalTimeZoneIdentifier(value.String().Go())
	if !ok {
		return "", r.throwRangeError("invalid time zone identifier")
	}
	return name, nil
}

func parseTemporalTimeZoneIdentifier(input string) (string, bool) {
	if input == "" {
		return "", false
	}
	if _, name, ok := parseZoneOffset(input); ok {
		return name, true
	}
	if canonical, ok := icu.CanonicalZone(input); ok {
		return canonical, true
	}

	main, annotations, ok := splitTemporalAnnotations(input)
	if !ok || !validInstantAnnotations(annotations) {
		return "", false
	}
	zoneAnnotation := ""
	for _, annotation := range annotations {
		annotation = strings.TrimPrefix(annotation, "!")
		if _, _, keyed := strings.Cut(annotation, "="); !keyed {
			zoneAnnotation = annotation
		}
	}
	if zoneAnnotation != "" {
		if _, _, err := parseTemporalInstantFields(input); err != nil {
			if _, err := parseTemporalPlainDateTime(input); err != nil {
				return "", false
			}
		}
		if _, name, ok := parseZoneOffset(zoneAnnotation); ok {
			return name, true
		}
		canonical, ok := icu.CanonicalZone(zoneAnnotation)
		return canonical, ok
	}

	if _, _, err := parseTemporalInstantFields(input); err != nil {
		return "", false
	}
	if len(main) != 0 && (main[len(main)-1] == 'Z' || main[len(main)-1] == 'z') {
		return "UTC", true
	}
	timeStart := strings.IndexAny(main, "Tt ")
	if timeStart < 0 {
		return "", false
	}
	for index := timeStart + 1; index < len(main); index++ {
		if main[index] != '+' && main[index] != '-' {
			continue
		}
		_, name, ok := parseZoneOffset(main[index:])
		return name, ok
	}
	return "", false
}
