package vm

import (
	"math"
	"math/big"
	"time"

	intl "github.com/go-quickjs/go-intl"
	"github.com/go-quickjs/go-intl/date"
)

// Date, on go-intl's date package, which is V8's Date apart from any
// engine: its time values, local time through V8's offset cache, the
// strings it writes, and Date.parse.
//
// A Date object holds a date.Date, which keeps the local fields V8 reads
// its getters from, read through the cache when the value is set: what the
// cache answers depends on what it was asked before, as V8's does, so the
// questions are asked in the order V8 asks them.
//
// The engine has no ambient access to the clock or the local zone unless the
// host provides them, so both come from the runtime rather than from the
// process. That keeps a sandboxed script from reading the wall clock when the
// host does not want it to.

// dateOf recovers a Date object's date from a receiver.
func (r *Runtime) dateOf(this Value, name string) (*date.Date, error) {
	if !this.IsObject() || this.Object().class != ClassDate {
		return nil, r.throwTypeError("%s called on an incompatible receiver", name)
	}
	d, ok := this.Object().data.(*date.Date)
	if !ok {
		return nil, r.throwTypeError("%s called on an uninitialized Date", name)
	}
	return d, nil
}

// dateValueOf recovers a Date's time value from a receiver.
func (r *Runtime) dateValueOf(this Value, name string) (float64, error) {
	d, err := r.dateOf(this, name)
	if err != nil {
		return 0, err
	}
	return d.Value(), nil
}

// dateEnv is local time as this runtime's Dates reckon it: its zone, its
// clock, and the locale the zone's names are written in.
func (r *Runtime) dateEnv() *date.Environment {
	if r.dates != nil {
		return r.dates
	}
	opts := date.Options{TimeZone: r.timeZone, Now: r.clock}
	if loc, err := intl.ParseLocale(r.Locale()); err == nil {
		opts.Locale = &loc
	}
	env, err := date.New(opts)
	if err != nil {
		// The data is embedded; what it cannot name is written as an
		// offset, from the root locale's.
		opts.Locale = &intl.Locale{}
		env, _ = date.New(opts)
	}
	r.dates = env
	return env
}

// newDate is a Date object's date made with a time value, clipped.
func (r *Runtime) newDate(t float64) *date.Date { return r.dateEnv().NewDate(t) }

// now returns the current time value, from the host's clock if one was
// supplied.
func (r *Runtime) now() float64 {
	if r.clock != nil {
		return float64(r.clock().UnixMilli())
	}
	return float64(time.Now().UnixMilli())
}

func (r *Runtime) initDateBuiltins() {
	r.proto.date = newObject(r.proto.object, ClassObject)
	p := r.proto.date

	ctor := r.newCtor("Date", 7, p, func(rt *Runtime, this Value, args []Value) (Value, error) {
		if rt.newTarget().IsUndefined() {
			// Called rather than constructed, Date ignores its arguments and
			// reports the current time as a string. It is the only constructor
			// that does something else entirely without `new`.
			return Str(NewString(rt.dateEnv().String(rt.now()))), nil
		}
		o := newObject(rt.proto.date, ClassDate)
		switch len(args) {
		case 0:
			o.data = rt.newDate(rt.now())
		case 1:
			// A Date is taken at its time value rather than through its string
			// form, so that `new Date(d)` copies d exactly -- and does not run
			// whatever toString the object happens to carry.
			if v := args[0]; v.IsObject() && v.Object().class == ClassDate {
				t := math.NaN()
				if d, ok := v.Object().data.(*date.Date); ok {
					t = d.Value()
				}
				o.data = rt.newDate(t)
				break
			}
			// Otherwise it is a time value, a date string, or an object whose
			// primitive form is one of those.
			prim, err := rt.toPrimitive(args[0], hintDefault)
			if err != nil {
				return Undefined, err
			}
			if prim.IsString() {
				o.data = rt.newDate(rt.dateEnv().Parse(prim.String().codeUnits()))
				break
			}
			n, err := rt.toNumber(prim)
			if err != nil {
				return Undefined, err
			}
			o.data = rt.newDate(n)
		default:
			local, err := rt.dateFromParts(args)
			if err != nil {
				return Undefined, err
			}
			d := new(date.Date)
			rt.dateEnv().SetLocalTime(d, local)
			o.data = d
		}
		return Obj(o), nil
	})

	r.defMethod(ctor, "now", 0, func(rt *Runtime, this Value, args []Value) (Value, error) {
		return Float(rt.now()), nil
	})
	r.defMethod(ctor, "parse", 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
		s, err := rt.toString(arg(args, 0))
		if err != nil {
			return Undefined, err
		}
		return Float(rt.dateEnv().Parse(s.codeUnits())), nil
	})
	r.defMethod(ctor, "UTC", 7, func(rt *Runtime, this Value, args []Value) (Value, error) {
		if len(args) == 0 {
			return Float(math.NaN()), nil
		}
		t, err := rt.dateFromParts(args)
		if err != nil {
			return Undefined, err
		}
		return Float(date.TimeClip(t)), nil
	})

	r.defMethod(p, "getTime", 0, func(rt *Runtime, this Value, args []Value) (Value, error) {
		t, err := rt.dateValueOf(this, "Date.prototype.getTime")
		if err != nil {
			return Undefined, err
		}
		return Float(t), nil
	})
	r.defMethod(p, "valueOf", 0, func(rt *Runtime, this Value, args []Value) (Value, error) {
		t, err := rt.dateValueOf(this, "Date.prototype.valueOf")
		if err != nil {
			return Undefined, err
		}
		return Float(t), nil
	})
	r.defMethod(p, "toTemporalInstant", 0, func(rt *Runtime, this Value, args []Value) (Value, error) {
		t, err := rt.dateValueOf(this, "Date.prototype.toTemporalInstant")
		if err != nil {
			return Undefined, err
		}
		if math.IsNaN(t) {
			return Undefined, rt.throwRangeError("invalid time value")
		}
		nanoseconds := new(big.Int).Mul(
			big.NewInt(int64(t)), big.NewInt(1_000_000))
		instant, ok := temporalInstantFromEpochNanoseconds(nanoseconds)
		if !ok {
			return Undefined, rt.throwRangeError(
				"date is outside the Temporal range")
		}
		rt.buildTemporal()
		return Obj(newTemporalInstant(rt.temporalInstantProto, instant)), nil
	})
	r.defMethod(p, "setTime", 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
		d, err := rt.dateOf(this, "Date.prototype.setTime")
		if err != nil {
			return Undefined, err
		}
		n, err := rt.toNumber(arg(args, 0))
		if err != nil {
			return Undefined, err
		}
		return Float(rt.dateEnv().SetTime(d, n)), nil
	})

	// The field accessors differ only in which component they read, so they
	// are defined from a table. The local ones read the fields V8 keeps, the
	// UTC ones the time value taken apart.
	type field struct {
		name string
		get  func(date.Fields) int64
	}
	fields := []field{
		{"FullYear", func(f date.Fields) int64 { return f.Year }},
		// Months are zero-based, which is the language's most notorious wart.
		{"Month", func(f date.Fields) int64 { return int64(f.Month) }},
		{"Date", func(f date.Fields) int64 { return int64(f.Day) }},
		{"Day", func(f date.Fields) int64 { return int64(f.Weekday) }},
		{"Hours", func(f date.Fields) int64 { return int64(f.Hour) }},
		{"Minutes", func(f date.Fields) int64 { return int64(f.Minute) }},
		{"Seconds", func(f date.Fields) int64 { return int64(f.Second) }},
		{"Milliseconds", func(f date.Fields) int64 { return int64(f.Millisecond) }},
	}
	for _, f := range fields {
		for _, utc := range []bool{false, true} {
			name := "get" + f.name
			if utc {
				name = "getUTC" + f.name
			}
			get, isUTC := f.get, utc
			r.defMethod(p, name, 0, func(rt *Runtime, this Value, args []Value) (Value, error) {
				d, err := rt.dateOf(this, "Date.prototype."+name)
				if err != nil {
					return Undefined, err
				}
				if isUTC {
					if math.IsNaN(d.Value()) {
						return Float(math.NaN()), nil
					}
					return Float(float64(get(date.BreakDown(d.Value())))), nil
				}
				local, ok := rt.dateEnv().Local(d)
				if !ok {
					return Float(math.NaN()), nil
				}
				return Float(float64(get(local))), nil
			})
		}
	}

	r.defMethod(p, "getTimezoneOffset", 0, func(rt *Runtime, this Value, args []Value) (Value, error) {
		t, err := rt.dateValueOf(this, "Date.prototype.getTimezoneOffset")
		if err != nil {
			return Undefined, err
		}
		return Float(rt.dateEnv().TimezoneOffset(t)), nil
	})

	// The setters share a shape too: each replaces one or more components and
	// leaves the rest alone.
	setters := []struct {
		name  string
		start int // index of the first component in the parts list
		count int
	}{
		{"FullYear", 0, 3},
		{"Month", 1, 2},
		{"Date", 2, 1},
		{"Hours", 3, 4},
		{"Minutes", 4, 3},
		{"Seconds", 5, 2},
		{"Milliseconds", 6, 1},
	}
	for _, s := range setters {
		for _, utc := range []bool{false, true} {
			name := "set" + s.name
			if utc {
				name = "setUTC" + s.name
			}
			start, count, isUTC := s.start, s.count, utc
			r.defMethod(p, name, count, func(rt *Runtime, this Value, args []Value) (Value, error) {
				d, err := rt.dateOf(this, "Date.prototype."+name)
				if err != nil {
					return Undefined, err
				}
				v, err := rt.setDateParts(d, args, start, count, isUTC)
				if err != nil {
					return Undefined, err
				}
				return Float(v), nil
			})
		}
	}

	// Formatting.
	format := func(name string, write func(env *date.Environment, t float64) string) {
		r.defMethod(p, name, 0, func(rt *Runtime, this Value, args []Value) (Value, error) {
			t, err := rt.dateValueOf(this, "Date.prototype."+name)
			if err != nil {
				return Undefined, err
			}
			return Str(NewString(write(rt.dateEnv(), t))), nil
		})
	}
	format("toString", (*date.Environment).String)
	format("toDateString", (*date.Environment).DateString)
	format("toTimeString", (*date.Environment).TimeString)
	format("toUTCString", func(_ *date.Environment, t float64) string { return date.UTCString(t) })
	// The three toLocale methods are Intl.DateTimeFormat with the fields each
	// of them stands for, which is what ECMA-402 defines them as.
	locale := func(name string, required, defaults intl.DateTimeComponents) {
		r.defMethod(p, name, 0, func(rt *Runtime, this Value, args []Value) (Value, error) {
			t, err := rt.dateValueOf(this, "Date.prototype."+name)
			if err != nil {
				return Undefined, err
			}
			if math.IsNaN(t) {
				return Str(NewString("Invalid Date")), nil
			}
			d, err := rt.newDateTimeFormat(args, required, defaults, "")
			if err != nil {
				return Undefined, err
			}
			text, err := rt.formatDateTime(d, Float(t))
			if err != nil {
				return Undefined, err
			}
			return Str(NewString(text)), nil
		})
	}
	locale("toLocaleString", intl.ComponentsAny, intl.ComponentsAll)
	locale("toLocaleDateString", intl.ComponentsDate, intl.ComponentsDate)
	locale("toLocaleTimeString", intl.ComponentsTime, intl.ComponentsTime)

	r.defMethod(p, "toISOString", 0, func(rt *Runtime, this Value, args []Value) (Value, error) {
		t, err := rt.dateValueOf(this, "Date.prototype.toISOString")
		if err != nil {
			return Undefined, err
		}
		text, ok := date.ISOString(t)
		if !ok {
			// Unlike the other formatters, this one reports the invalid date
			// rather than returning a placeholder string.
			return Undefined, rt.throwRangeError("Invalid time value")
		}
		return Str(NewString(text)), nil
	})
	r.defMethod(p, "toJSON", 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
		// Unlike the rest of the prototype this one is generic: it asks the
		// receiver for a number and then for a string, and anything that
		// answers both will do. JSON.stringify calls it on whatever it finds.
		o, err := rt.toObject(this)
		if err != nil {
			return Undefined, err
		}
		num, err := rt.toPrimitive(Obj(o), hintNumber)
		if err != nil {
			return Undefined, err
		}
		// A date that cannot be represented serializes as null rather than
		// throwing, which is the one place the two formatters disagree.
		if num.IsNumber() && (math.IsNaN(num.Number()) || math.IsInf(num.Number(), 0)) {
			return Null, nil
		}
		fn, err := rt.getProp(o, rt.atoms.intern("toISOString"), Obj(o))
		if err != nil {
			return Undefined, err
		}
		if !isCallable(fn) {
			return Undefined, rt.throwTypeError("toISOString is not a function")
		}
		return rt.call(fn, Obj(o), nil)
	})

	// Date is the only built-in whose default coercion hint is string, which
	// is why `date + ""` concatenates but `date - 0` subtracts.
	r.defSymbolMethod(p, r.wellKnown.toPrimitive, "[Symbol.toPrimitive]", 1,
		func(rt *Runtime, this Value, args []Value) (Value, error) {
			if !this.IsObject() {
				return Undefined, rt.throwTypeError("Date[Symbol.toPrimitive] requires an object")
			}
			// The hint is compared as given rather than coerced: a String
			// object holding "number" is not the hint "number".
			h := arg(args, 0)
			if !h.IsString() {
				return Undefined, rt.throwTypeError(
					"invalid hint for Date[Symbol.toPrimitive]")
			}
			var want hint
			switch h.String().Go() {
			case "number":
				want = hintNumber
			case "string", "default":
				// Date is the only built-in whose default is string.
				want = hintString
			default:
				return Undefined, rt.throwTypeError(
					"invalid hint %q for Date[Symbol.toPrimitive]", h.String().Go())
			}
			return rt.ordinaryToPrimitive(this, want)
		})
	// The method is not writable, which is how a script can tell it apart from
	// one a program installed.
	if pd := p.getOwn(r.atoms.internSymbol(r.wellKnown.toPrimitive)); pd != nil {
		pd.flags &^= propWritable
	}
}

// dateFromParts is the time value the Date constructor's and Date.UTC's
// year, month, day and time arguments make, before either reads it as
// local time or UTC: every argument converted in order, and a year of two
// digits taken as 19xx.
func (r *Runtime) dateFromParts(args []Value) (float64, error) {
	parts := [7]float64{0, 0, 1, 0, 0, 0, 0}
	for i := 0; i < len(parts) && i < len(args); i++ {
		n, err := r.toNumber(args[i])
		if err != nil {
			return 0, err
		}
		parts[i] = n
	}
	year := parts[0]
	if !math.IsNaN(year) {
		if y := math.Trunc(year); y >= 0 && y <= 99 {
			year = 1900 + y
		}
	}
	return date.MakeDate(date.MakeDay(year, parts[1], parts[2]),
		date.MakeTime(parts[3], parts[4], parts[5], parts[6])), nil
}

// setDateParts replaces count components starting at start, leaving the rest
// of the date as it was, and sets the date's value: the local components,
// read through the offset cache, made a time value again, or the UTC ones.
// It returns the value set, or NaN left unset for an invalid date.
func (r *Runtime) setDateParts(d *date.Date, args []Value, start, count int, utc bool) (float64, error) {
	// The time value is read before any argument is converted, and every
	// argument is converted, in order, before anything else happens: a
	// valueOf can see that it was called, and it is called even when the date
	// is already invalid or an earlier argument was NaN.
	t := d.Value()
	n := count
	if n > len(args) {
		n = len(args)
	}
	if n < 1 {
		// The first argument is not optional: setHours() means setHours(NaN).
		n = 1
	}
	var given [7]float64
	for i := 0; i < n; i++ {
		v, err := r.toNumber(arg(args, i))
		if err != nil {
			return 0, err
		}
		given[i] = v
	}

	env := r.dateEnv()
	// Setting a component of an invalid date leaves it invalid, except for
	// setFullYear, which the specification lets revive one from the epoch:
	// +0 read as it is, not as local time.
	var base float64
	switch {
	case math.IsNaN(t) && start != 0:
		return math.NaN(), nil
	case math.IsNaN(t):
		base = 0
	case utc:
		base = t
	default:
		base = env.LocalTime(t)
	}
	f := date.BreakDown(base)
	parts := [7]float64{float64(f.Year), float64(f.Month), float64(f.Day),
		float64(f.Hour), float64(f.Minute), float64(f.Second), float64(f.Millisecond)}
	for i := 0; i < n; i++ {
		parts[start+i] = given[i]
	}
	v := date.MakeDate(date.MakeDay(parts[0], parts[1], parts[2]),
		date.MakeTime(parts[3], parts[4], parts[5], parts[6]))
	if utc {
		return env.SetTime(d, v), nil
	}
	return env.SetLocalTime(d, v), nil
}
