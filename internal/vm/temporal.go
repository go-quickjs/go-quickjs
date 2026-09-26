package vm

import (
	"time"

	intl "github.com/go-quickjs/go-intl"
	"github.com/go-quickjs/go-intl/temporal"
)

// The Temporal namespace, and what its types share: their constructors and
// prototypes, receivers, and Temporal.Now, which is the engine's.

// initTemporalBuiltins installs a lazy namespace. Temporal is large enough
// that runtimes which never ask for it should not allocate all its prototypes.
func (r *Runtime) initTemporalBuiltins() {
	name := r.atoms.intern("Temporal")
	build := r.newNativeFunc("get Temporal", 0, func(rt *Runtime, this Value, args []Value) (Value, error) {
		ns := rt.buildTemporal()
		rt.global.setOwnRaw(name, Obj(ns), propWritable|propConfigurable)
		return Obj(ns), nil
	})
	set := r.newNativeFunc("set Temporal", 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
		rt.global.setOwnRaw(name, arg(args, 0), propWritable|propConfigurable)
		return Undefined, nil
	})
	r.defineAccessor(r.global, name, build, set, propConfigurable)
}

func (r *Runtime) buildTemporal() *Object {
	if r.temporalNamespace != nil {
		return r.temporalNamespace
	}
	ns := newObject(r.proto.object, ClassObject)
	r.temporalNamespace = ns
	r.defToStringTag(ns, "Temporal")
	r.initTemporalDuration(ns)
	r.initTemporalInstant(ns)
	r.initTemporalPlainDate(ns)
	r.initTemporalPlainDateTime(ns)
	r.initTemporalPlainMonthDay(ns)
	r.initTemporalPlainTime(ns)
	r.initTemporalPlainYearMonth(ns)
	r.initTemporalZonedDateTime(ns)
	r.initTemporalNow(ns)
	return ns
}

// temporalKind is one of Temporal's types, as its prototype and the name
// V8's messages give it.
type temporalKind[T any] struct {
	r     *Runtime
	proto *Object
	name  string // "PlainDate"
}

// temporalThis is the receiver's value, or V8's TypeError for another.
func temporalThis[T any](r *Runtime, this Value, method string) (T, error) {
	if this.IsObject() {
		if v, ok := this.Object().data.(T); ok {
			return v, nil
		}
	}
	var zero T
	return zero, r.intlIncompatibleReceiver(method, this)
}

// temporalObject is a new object of a Temporal type holding a value.
func temporalObject(proto *Object, v any) Value {
	o := newObject(proto, ClassObject)
	o.data = v
	return Obj(o)
}

// ctor defines the constructor, which V8 refuses to call without new, and
// whose instances take their prototype from new.target once made.
func (k temporalKind[T]) ctor(ns *Object, length int, make func(rt *Runtime, args []Value) (T, error)) *Object {
	proto := k.proto
	c, data := k.r.newSlabFuncObject(k.r.proto.function, ClassFunction)
	*data = funcData{name: k.name, length: length, ctorKind: ctorBase,
		native: func(rt *Runtime, this Value, args []Value) (Value, error) {
			if !rt.Constructing() {
				return Undefined, rt.throwTypeError("Method invoked on an object that is not Temporal.%s.", k.name)
			}
			v, err := make(rt, args)
			if err != nil {
				return Undefined, err
			}
			p, err := rt.protoFromNewTargetErr(proto)
			if err != nil {
				return Undefined, err
			}
			return temporalObject(p, v), nil
		}}
	c.setOwnRaw(atomPrototype, Obj(proto), 0)
	proto.setOwnRaw(atomConstructor, Obj(c), propWritable|propConfigurable)
	k.r.defValue(ns, k.name, Obj(c))
	k.r.defToStringTag(proto, "Temporal."+k.name)
	return c
}

// method defines a prototype method, which takes its receiver's value.
func (k temporalKind[T]) method(name string, length int, fn func(rt *Runtime, v T, args []Value) (Value, error)) {
	full := "Temporal." + k.name + ".prototype." + name
	k.r.defMethod(k.proto, name, length, func(rt *Runtime, this Value, args []Value) (Value, error) {
		v, err := temporalThis[T](rt, this, full)
		if err != nil {
			return Undefined, err
		}
		return fn(rt, v, args)
	})
}

// getter defines a prototype getter; V8 names it without "get " in its
// messages.
func (k temporalKind[T]) getter(name string, fn func(rt *Runtime, v T) (Value, error)) {
	full := "Temporal." + k.name + ".prototype." + name
	k.r.defGetter(k.proto, name, func(rt *Runtime, this Value, args []Value) (Value, error) {
		v, err := temporalThis[T](rt, this, full)
		if err != nil {
			return Undefined, err
		}
		return fn(rt, v)
	})
}

// valueOf defines valueOf, which a Temporal value refuses.
func (k temporalKind[T]) valueOf() {
	k.r.defMethod(k.proto, "valueOf", 0, func(rt *Runtime, this Value, args []Value) (Value, error) {
		return Undefined, rt.throwTypeError("Do not use Temporal.%s.prototype.valueOf; use Temporal.%s.prototype.compare for comparison.",
			k.name, k.name)
	})
}

// toJSON defines toJSON, which is toString with its defaults.
func (k temporalKind[T]) toJSON(json func(rt *Runtime, v T) (string, error)) {
	k.method("toJSON", 0, func(rt *Runtime, v T, args []Value) (Value, error) {
		s, err := json(rt, v)
		if err != nil {
			return Undefined, err
		}
		return Str(NewString(s)), nil
	})
}

// toLocaleString defines toLocaleString, which is Intl.DateTimeFormat with
// the fields the type needs and supplies.
func (k temporalKind[T]) toLocaleString(required, defaults intl.DateTimeComponents) {
	k.method("toLocaleString", 0, func(rt *Runtime, v T, args []Value) (Value, error) {
		return rt.temporalToLocaleString(temporalObject(nil, v), args, required, defaults)
	})
}

// temporalDateGetters are the getters of a calendar date, for every type
// with one.
func temporalDateGetters[T any](k temporalKind[T], date func(T) temporal.PlainDate) {
	optionalInt := func(n int, ok bool) Value {
		if !ok {
			return Undefined
		}
		return Int(n)
	}
	for _, g := range []struct {
		name string
		get  func(temporal.PlainDate) Value
	}{
		{"calendarId", func(d temporal.PlainDate) Value { return Str(NewString(d.Calendar().ID())) }},
		{"era", func(d temporal.PlainDate) Value {
			if era, ok := d.Era(); ok && era != "" {
				return Str(NewString(era))
			}
			return Undefined
		}},
		{"eraYear", func(d temporal.PlainDate) Value { return optionalInt(d.EraYear()) }},
		{"year", func(d temporal.PlainDate) Value { return Int(d.Year()) }},
		{"month", func(d temporal.PlainDate) Value { return Int(d.Month()) }},
		{"monthCode", func(d temporal.PlainDate) Value { return Str(NewString(d.MonthCode())) }},
		{"day", func(d temporal.PlainDate) Value { return Int(d.Day()) }},
		{"dayOfWeek", func(d temporal.PlainDate) Value { return Int(d.DayOfWeek()) }},
		{"dayOfYear", func(d temporal.PlainDate) Value { return Int(d.DayOfYear()) }},
		{"weekOfYear", func(d temporal.PlainDate) Value { return optionalInt(d.WeekOfYear()) }},
		{"yearOfWeek", func(d temporal.PlainDate) Value { return optionalInt(d.YearOfWeek()) }},
		{"daysInWeek", func(d temporal.PlainDate) Value { return Int(d.DaysInWeek()) }},
		{"daysInMonth", func(d temporal.PlainDate) Value { return Int(d.DaysInMonth()) }},
		{"daysInYear", func(d temporal.PlainDate) Value { return Int(d.DaysInYear()) }},
		{"monthsInYear", func(d temporal.PlainDate) Value { return Int(d.MonthsInYear()) }},
		{"inLeapYear", func(d temporal.PlainDate) Value { return Bool(d.InLeapYear()) }},
	} {
		get := g.get
		k.getter(g.name, func(rt *Runtime, v T) (Value, error) { return get(date(v)), nil })
	}
}

// temporalTimeGetters are the getters of a time of day.
func temporalTimeGetters[T any](k temporalKind[T], clock func(T) temporal.ISOTime) {
	for _, g := range []struct {
		name string
		get  func(temporal.ISOTime) int
	}{
		{"hour", func(t temporal.ISOTime) int { return t.Hour }},
		{"minute", func(t temporal.ISOTime) int { return t.Minute }},
		{"second", func(t temporal.ISOTime) int { return t.Second }},
		{"millisecond", func(t temporal.ISOTime) int { return t.Millisecond }},
		{"microsecond", func(t temporal.ISOTime) int { return t.Microsecond }},
		{"nanosecond", func(t temporal.ISOTime) int { return t.Nanosecond }},
	} {
		get := g.get
		k.getter(g.name, func(rt *Runtime, v T) (Value, error) { return Int(get(clock(v))), nil })
	}
}

// temporalSameCalendar is the check until and since make.
func (r *Runtime) temporalSameCalendar(a, b *temporal.Calendar) error {
	if !a.Equal(b) {
		return r.throwRangeError("Mismatched calendars.")
	}
	return nil
}

// ==== Temporal.Now ====

// temporalNowInstant is the current instant, from the host's clock if one
// was supplied.
func (r *Runtime) temporalNowInstant() (temporal.Instant, error) {
	now := time.Now()
	if r.clock != nil {
		now = r.clock()
	}
	ns := now.UnixNano()
	hi := int64(0)
	if ns < 0 {
		hi = -1
	}
	i, err := temporal.NewInstant(hi, uint64(ns))
	return i, r.temporalErr(err)
}

// temporalSystemZone is SystemTimeZoneIdentifier as a Temporal zone: the
// zone local time is in, or UTC where Temporal has no such name.
func (r *Runtime) temporalSystemZone() (temporal.TimeZone, error) {
	d, err := r.temporalData()
	if err != nil {
		return temporal.TimeZone{}, err
	}
	if tz, err := d.Zones.TimeZoneFromIdentifier([]byte(r.localZoneName())); err == nil {
		return tz, nil
	}
	return d.Zones.UTC(), nil
}

// temporalNowZoned is the current instant in a zone: the system's where
// the argument is undefined.
func (r *Runtime) temporalNowZoned(v Value) (temporal.ZonedDateTime, error) {
	var tz temporal.TimeZone
	var err error
	if v.IsUndefined() {
		tz, err = r.temporalSystemZone()
	} else {
		tz, err = r.temporalToTimeZone(v)
	}
	if err != nil {
		return temporal.ZonedDateTime{}, err
	}
	i, err := r.temporalNowInstant()
	if err != nil {
		return temporal.ZonedDateTime{}, err
	}
	z, err := i.ToZonedDateTimeISO(tz)
	return z, r.temporalErr(err)
}

func (r *Runtime) initTemporalNow(ns *Object) {
	now := newObject(r.proto.object, ClassObject)
	r.defMethod(now, "instant", 0, func(rt *Runtime, this Value, args []Value) (Value, error) {
		i, err := rt.temporalNowInstant()
		if err != nil {
			return Undefined, err
		}
		return temporalObject(rt.temporalInstantProto, i), nil
	})
	r.defMethod(now, "timeZoneId", 0, func(rt *Runtime, this Value, args []Value) (Value, error) {
		tz, err := rt.temporalSystemZone()
		if err != nil {
			return Undefined, err
		}
		return Str(NewString(tz.Identifier())), nil
	})
	r.defMethod(now, "zonedDateTimeISO", 0, func(rt *Runtime, this Value, args []Value) (Value, error) {
		z, err := rt.temporalNowZoned(arg(args, 0))
		if err != nil {
			return Undefined, err
		}
		return temporalObject(rt.temporalZonedDateTimeProto, z), nil
	})
	r.defMethod(now, "plainDateTimeISO", 0, func(rt *Runtime, this Value, args []Value) (Value, error) {
		z, err := rt.temporalNowZoned(arg(args, 0))
		if err != nil {
			return Undefined, err
		}
		return temporalObject(rt.temporalPlainDateTimeProto, z.ToPlainDateTime()), nil
	})
	r.defMethod(now, "plainDateISO", 0, func(rt *Runtime, this Value, args []Value) (Value, error) {
		z, err := rt.temporalNowZoned(arg(args, 0))
		if err != nil {
			return Undefined, err
		}
		return temporalObject(rt.temporalPlainDateProto, z.ToPlainDate()), nil
	})
	r.defMethod(now, "plainTimeISO", 0, func(rt *Runtime, this Value, args []Value) (Value, error) {
		z, err := rt.temporalNowZoned(arg(args, 0))
		if err != nil {
			return Undefined, err
		}
		return temporalObject(rt.temporalPlainTimeProto, z.ToPlainTime()), nil
	})
	r.defToStringTag(now, "Temporal.Now")
	r.defValue(ns, "Now", Obj(now))
}
