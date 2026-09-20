package vm

import (
	"fmt"
	"math"
	"math/big"
	"strings"

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
	return temporal
}

func (r *Runtime) initTemporalInstant(temporal *Object) {
	proto := newObject(r.proto.object, ClassObject)
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
		return Str(NewString(instant.string())), nil
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
	r.newTemporalCtor(temporal, "ZonedDateTime", 2, proto, func(rt *Runtime, this Value, args []Value) (Value, error) {
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

func (z *temporalZonedDateTime) offsetSeconds() int {
	if z.fixed {
		return z.fixedOffsetSeconds
	}
	return z.zone.OffsetAt(z.instant.epochSeconds).OffsetSeconds
}

func (z *temporalZonedDateTime) string() string {
	offset := z.offsetSeconds()
	local := z.instant
	local.epochSeconds += int64(offset)
	text := strings.TrimSuffix(local.string(), "Z")
	sign := '+'
	away := offset
	if away < 0 {
		sign, away = '-', -away
	}
	offsetText := fmt.Sprintf("%c%02d:%02d", sign, away/3600, away/60%60)
	if away%60 != 0 {
		offsetText += fmt.Sprintf(":%02d", away%60)
	}
	text += offsetText + "[" + z.timeZone + "]"
	if z.calendar != "iso8601" {
		text += "[u-ca=" + z.calendar + "]"
	}
	return text
}
