package vm

import (
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/go-quickjs/go-quickjs/internal/jsnum"
)

// Date.
//
// A Date holds one number: milliseconds since the Unix epoch, or NaN for an
// invalid date. Everything else is derived, which is why the internal slot is a
// float64 rather than a time.Time -- a Date can hold a value outside the range
// time.Time represents, and NaN has no time.Time equivalent.
//
// The engine has no ambient access to the clock or the local zone unless the
// host provides them, so both come from the runtime rather than from the
// process. That keeps a sandboxed script from reading the wall clock when the
// host does not want it to.

// maxTimeValue is the largest magnitude a Date may hold: 100 million days
// either side of the epoch, in milliseconds.
const maxTimeValue = 8.64e15

// dateValueOf recovers a Date's time value from a receiver.
func (r *Runtime) dateValueOf(this Value, name string) (float64, error) {
	if !this.IsObject() || this.Object().class != ClassDate {
		return 0, r.throwTypeError("%s called on an incompatible receiver", name)
	}
	t, ok := this.Object().data.(float64)
	if !ok {
		return 0, r.throwTypeError("%s called on an uninitialized Date", name)
	}
	return t, nil
}

// timeAt converts a time value to a Go time in the runtime's zone.
func (r *Runtime) timeAt(ms float64, utc bool) time.Time {
	sec := math.Floor(ms / 1000)
	nsec := (ms - sec*1000) * 1e6
	t := time.Unix(int64(sec), int64(nsec))
	if utc {
		return t.UTC()
	}
	return t.In(r.location())
}

// location returns the zone local time is expressed in.
func (r *Runtime) location() *time.Location {
	if r.timeZone != nil {
		return r.timeZone
	}
	return time.Local
}

// now returns the current time value, from the host's clock if one was
// supplied.
func (r *Runtime) now() float64 {
	if r.clock != nil {
		return float64(r.clock().UnixMilli())
	}
	return float64(time.Now().UnixMilli())
}

// clipTime applies the range limit and integer truncation the specification
// puts on every time value.
func clipTime(ms float64) float64 {
	if math.IsNaN(ms) || math.Abs(ms) > maxTimeValue {
		return math.NaN()
	}
	return math.Trunc(ms)
}

func (r *Runtime) initDateBuiltins() {
	r.proto.date = newObject(r.proto.object, ClassObject)
	p := r.proto.date

	ctor := r.newCtor("Date", 7, p, func(rt *Runtime, this Value, args []Value) (Value, error) {
		o := newObject(rt.proto.date, ClassDate)
		switch len(args) {
		case 0:
			o.data = rt.now()
		case 1:
			// A single argument is a time value, a date string, or an object
			// whose primitive form is one of those.
			prim, err := rt.toPrimitive(args[0], hintDefault)
			if err != nil {
				return Undefined, err
			}
			if prim.IsString() {
				o.data = parseDate(prim.String().Go(), rt.location())
				break
			}
			n, err := rt.toNumber(prim)
			if err != nil {
				return Undefined, err
			}
			o.data = clipTime(n)
		default:
			ms, err := rt.makeDateFromParts(args, false)
			if err != nil {
				return Undefined, err
			}
			o.data = ms
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
		return Float(parseDate(s.Go(), rt.location())), nil
	})
	r.defMethod(ctor, "UTC", 7, func(rt *Runtime, this Value, args []Value) (Value, error) {
		if len(args) == 0 {
			return Float(math.NaN()), nil
		}
		ms, err := rt.makeDateFromParts(args, true)
		if err != nil {
			return Undefined, err
		}
		return Float(ms), nil
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
	r.defMethod(p, "setTime", 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
		if _, err := rt.dateValueOf(this, "Date.prototype.setTime"); err != nil {
			return Undefined, err
		}
		n, err := rt.toNumber(arg(args, 0))
		if err != nil {
			return Undefined, err
		}
		v := clipTime(n)
		this.Object().data = v
		return Float(v), nil
	})

	// The field accessors differ only in which component they read, so they
	// are defined from a table.
	type field struct {
		name string
		get  func(time.Time) int
	}
	fields := []field{
		{"FullYear", func(t time.Time) int { return t.Year() }},
		// Months are zero-based, which is the language's most notorious wart.
		{"Month", func(t time.Time) int { return int(t.Month()) - 1 }},
		{"Date", func(t time.Time) int { return t.Day() }},
		{"Day", func(t time.Time) int { return int(t.Weekday()) }},
		{"Hours", func(t time.Time) int { return t.Hour() }},
		{"Minutes", func(t time.Time) int { return t.Minute() }},
		{"Seconds", func(t time.Time) int { return t.Second() }},
		{"Milliseconds", func(t time.Time) int { return t.Nanosecond() / 1e6 }},
	}
	for _, f := range fields {
		for _, utc := range []bool{false, true} {
			name := "get" + f.name
			if utc {
				name = "getUTC" + f.name
			}
			get, isUTC := f.get, utc
			r.defMethod(p, name, 0, func(rt *Runtime, this Value, args []Value) (Value, error) {
				t, err := rt.dateValueOf(this, name)
				if err != nil {
					return Undefined, err
				}
				if math.IsNaN(t) {
					return Float(math.NaN()), nil
				}
				return Int(get(rt.timeAt(t, isUTC))), nil
			})
		}
	}

	r.defMethod(p, "getTimezoneOffset", 0, func(rt *Runtime, this Value, args []Value) (Value, error) {
		t, err := rt.dateValueOf(this, "Date.prototype.getTimezoneOffset")
		if err != nil {
			return Undefined, err
		}
		if math.IsNaN(t) {
			return Float(math.NaN()), nil
		}
		_, offset := rt.timeAt(t, false).Zone()
		// The offset is reported as minutes behind UTC, so the sign is the
		// opposite of what most people expect.
		return Int(-offset / 60), nil
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
				t, err := rt.dateValueOf(this, name)
				if err != nil {
					return Undefined, err
				}
				v, err := rt.setDateParts(t, args, start, count, isUTC)
				if err != nil {
					return Undefined, err
				}
				this.Object().data = v
				return Float(v), nil
			})
		}
	}

	// Formatting.
	format := func(name string, fn func(*Runtime, time.Time) string) {
		r.defMethod(p, name, 0, func(rt *Runtime, this Value, args []Value) (Value, error) {
			t, err := rt.dateValueOf(this, name)
			if err != nil {
				return Undefined, err
			}
			if math.IsNaN(t) {
				return Str(NewString("Invalid Date")), nil
			}
			return Str(NewString(fn(rt, rt.timeAt(t, false)))), nil
		})
	}
	format("toString", func(rt *Runtime, t time.Time) string {
		return t.Format("Mon Jan 02 2006 15:04:05 GMT-0700 (MST)")
	})
	format("toDateString", func(rt *Runtime, t time.Time) string {
		return t.Format("Mon Jan 02 2006")
	})
	format("toTimeString", func(rt *Runtime, t time.Time) string {
		return t.Format("15:04:05 GMT-0700 (MST)")
	})
	format("toLocaleString", func(rt *Runtime, t time.Time) string {
		return t.Format("1/2/2006, 3:04:05 PM")
	})
	format("toLocaleDateString", func(rt *Runtime, t time.Time) string {
		return t.Format("1/2/2006")
	})
	format("toLocaleTimeString", func(rt *Runtime, t time.Time) string {
		return t.Format("3:04:05 PM")
	})

	r.defMethod(p, "toISOString", 0, func(rt *Runtime, this Value, args []Value) (Value, error) {
		t, err := rt.dateValueOf(this, "Date.prototype.toISOString")
		if err != nil {
			return Undefined, err
		}
		if math.IsNaN(t) {
			// Unlike the other formatters, this one reports the invalid date
			// rather than returning a placeholder string.
			return Undefined, rt.throwRangeError("invalid time value")
		}
		return Str(NewString(isoString(rt.timeAt(t, true)))), nil
	})
	r.defMethod(p, "toUTCString", 0, func(rt *Runtime, this Value, args []Value) (Value, error) {
		t, err := rt.dateValueOf(this, "Date.prototype.toUTCString")
		if err != nil {
			return Undefined, err
		}
		if math.IsNaN(t) {
			return Str(NewString("Invalid Date")), nil
		}
		return Str(NewString(rt.timeAt(t, true).Format("Mon, 02 Jan 2006 15:04:05 GMT"))), nil
	})
	r.defMethod(p, "toJSON", 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
		t, err := rt.dateValueOf(this, "Date.prototype.toJSON")
		if err != nil {
			return Undefined, err
		}
		// An invalid date serializes as null rather than throwing, which is
		// the one place the two formatters disagree.
		if math.IsNaN(t) {
			return Null, nil
		}
		return Str(NewString(isoString(rt.timeAt(t, true)))), nil
	})

	// Date is the only built-in whose default coercion hint is string, which
	// is why `date + ""` concatenates but `date - 0` subtracts.
	r.defSymbolMethod(p, r.wellKnown.toPrimitive, "[Symbol.toPrimitive]", 1,
		func(rt *Runtime, this Value, args []Value) (Value, error) {
			if !this.IsObject() {
				return Undefined, rt.throwTypeError("Date[Symbol.toPrimitive] requires an object")
			}
			h, err := rt.toString(arg(args, 0))
			if err != nil {
				return Undefined, err
			}
			want := hintString
			if h.Go() == "number" {
				want = hintNumber
			}
			return rt.ordinaryToPrimitive(this, want)
		})
}

// isoString formats a time as Date.prototype.toISOString does, which always
// uses UTC and always shows milliseconds.
func isoString(t time.Time) string {
	year := t.Year()
	// Years outside four digits take an expanded form with an explicit sign.
	switch {
	case year < 0:
		return fmt.Sprintf("-%06d-%s", -year, t.Format("01-02T15:04:05.000Z"))
	case year > 9999:
		return fmt.Sprintf("+%06d-%s", year, t.Format("01-02T15:04:05.000Z"))
	}
	return t.Format("2006-01-02T15:04:05.000Z")
}

// makeDateFromParts builds a time value from year, month, day and time
// components.
func (r *Runtime) makeDateFromParts(args []Value, utc bool) (float64, error) {
	parts := [7]float64{0, 0, 1, 0, 0, 0, 0}
	for i := 0; i < len(parts) && i < len(args); i++ {
		n, err := r.toNumber(args[i])
		if err != nil {
			return 0, err
		}
		if math.IsNaN(n) || math.IsInf(n, 0) {
			return math.NaN(), nil
		}
		parts[i] = math.Trunc(n)
	}
	// A two-digit year means 19xx, a rule kept for compatibility.
	year := parts[0]
	if year >= 0 && year <= 99 {
		year += 1900
	}
	return r.composeTime(year, parts[1], parts[2], parts[3], parts[4], parts[5], parts[6], utc), nil
}

// composeTime assembles a time value from components, letting out-of-range
// values roll over as the specification requires: month 12 is January of the
// next year, and day 0 is the last day of the previous month.
func (r *Runtime) composeTime(year, month, day, hour, min, sec, ms float64, utc bool) float64 {
	for _, v := range []float64{year, month, day, hour, min, sec, ms} {
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return math.NaN()
		}
	}
	loc := time.UTC
	if !utc {
		loc = r.location()
	}
	// time.Date normalizes out-of-range components exactly as JavaScript does.
	t := time.Date(int(year), time.Month(int(month)+1), int(day),
		int(hour), int(min), int(sec), int(ms)*1e6, loc)
	return clipTime(float64(t.UnixMilli()))
}

// setDateParts replaces count components starting at start, leaving the rest of
// the date unchanged.
func (r *Runtime) setDateParts(t float64, args []Value, start, count int, utc bool) (float64, error) {
	// Setting a component of an invalid date leaves it invalid, except for
	// setFullYear, which the specification lets revive one from the epoch.
	base := t
	if math.IsNaN(base) {
		if start != 0 {
			return math.NaN(), nil
		}
		base = 0
	}
	tm := r.timeAt(base, utc)

	parts := [7]float64{
		float64(tm.Year()),
		float64(int(tm.Month()) - 1),
		float64(tm.Day()),
		float64(tm.Hour()),
		float64(tm.Minute()),
		float64(tm.Second()),
		float64(tm.Nanosecond() / 1e6),
	}
	for i := 0; i < count && i < len(args); i++ {
		n, err := r.toNumber(args[i])
		if err != nil {
			return 0, err
		}
		if math.IsNaN(n) || math.IsInf(n, 0) {
			return math.NaN(), nil
		}
		parts[start+i] = math.Trunc(n)
	}
	return r.composeTime(parts[0], parts[1], parts[2], parts[3], parts[4], parts[5], parts[6], utc), nil
}

// dateFormats are the layouts Date.parse accepts, tried in order.
//
// The specification requires only the ISO form; the rest are the legacy shapes
// that every engine accepts in practice and that real data contains.
var dateFormats = []string{
	"2006-01-02T15:04:05.000Z07:00",
	"2006-01-02T15:04:05Z07:00",
	"2006-01-02T15:04:05.000",
	"2006-01-02T15:04:05",
	"2006-01-02T15:04",
	"2006-01-02",
	"2006-01",
	"2006",
	"Mon Jan 02 2006 15:04:05 GMT-0700 (MST)",
	"Mon Jan 02 2006 15:04:05 GMT-0700",
	"Mon Jan 02 2006 15:04:05",
	"Mon Jan 02 2006",
	"Jan 02, 2006 15:04:05",
	"Jan 02, 2006",
	"January 2, 2006",
	"Mon, 02 Jan 2006 15:04:05 GMT",
	"Mon, 02 Jan 2006 15:04:05 -0700",
	"01/02/2006",
	"01/02/2006 15:04:05",
}

// parseDate implements Date.parse, returning NaN for anything it cannot read.
func parseDate(s string, loc *time.Location) float64 {
	s = strings.TrimSpace(s)
	if s == "" {
		return math.NaN()
	}

	// A date-only ISO form is UTC, while a date-time form without a zone is
	// local. That asymmetry is in the specification and surprises everyone.
	dateOnly := len(s) <= 10 && !strings.ContainsAny(s, "T :")

	for _, layout := range dateFormats {
		zone := loc
		if dateOnly || strings.HasSuffix(layout, "Z07:00") || strings.Contains(layout, "MST") ||
			strings.Contains(layout, "-0700") || strings.HasSuffix(layout, "GMT") {
			zone = time.UTC
		}
		t, err := time.ParseInLocation(layout, s, zone)
		if err != nil {
			continue
		}
		return clipTime(float64(t.UnixMilli()))
	}
	// A bare number is not a date, even though ToNumber would accept it.
	if n := jsnum.ToNumber(s); !math.IsNaN(n) && strings.IndexAny(s, "0123456789") == 0 &&
		!strings.ContainsAny(s, "-/:") {
		return math.NaN()
	}
	return math.NaN()
}
