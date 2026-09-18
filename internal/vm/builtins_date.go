package vm

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/go-quickjs/go-quickjs/internal/icu"
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

// The lengths of the units a time value is assembled from.
const (
	msPerSecond = 1000.0
	msPerMinute = 60 * msPerSecond
	msPerHour   = 60 * msPerMinute
	msPerDay    = 24 * msPerHour
)

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

// zoneLabel is the name in parentheses that a date written out ends with:
// "(Eastern Standard Time)" where the zone has a name in the language being
// written in, and the offset from Greenwich where it has none.
func (r *Runtime) zoneLabel(t time.Time) string {
	locale := r.formatLocale()
	names := icu.ZoneNamesIn(locale, r.localZoneName())
	name := names.LongStandard
	if t.IsDST() {
		name = names.LongDaylight
	}
	if name == "" {
		_, offset := t.Zone()
		name = icu.OffsetName(locale, offset/60, true)
	}
	return "(" + name + ")"
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
	// Truncating toward zero leaves negative zero, which is not a time value:
	// the epoch is the epoch however it was arrived at.
	return math.Trunc(ms) + 0
}

func (r *Runtime) initDateBuiltins() {
	r.proto.date = newObject(r.proto.object, ClassObject)
	p := r.proto.date

	ctor := r.newCtor("Date", 7, p, func(rt *Runtime, this Value, args []Value) (Value, error) {
		if rt.newTarget().IsUndefined() {
			// Called rather than constructed, Date ignores its arguments and
			// reports the current time as a string. It is the only constructor
			// that does something else entirely without `new`.
			return Str(NewString(rt.timeAt(rt.now(), false).
				Format("Mon Jan 02 2006 15:04:05 GMT-0700 (MST)"))), nil
		}
		o := newObject(rt.proto.date, ClassDate)
		switch len(args) {
		case 0:
			o.data = rt.now()
		case 1:
			// A Date is taken at its time value rather than through its string
			// form, so that `new Date(d)` copies d exactly -- and does not run
			// whatever toString the object happens to carry.
			if v := args[0]; v.IsObject() && v.Object().class == ClassDate {
				t, ok := v.Object().data.(float64)
				if !ok {
					t = math.NaN()
				}
				o.data = clipTime(t)
				break
			}
			// Otherwise it is a time value, a date string, or an object whose
			// primitive form is one of those.
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
		// opposite of what most people expect. It is not rounded: a zone whose
		// offset is not a whole number of minutes -- which every local mean
		// time before standard zones was -- would otherwise disagree with the
		// value the date itself was built from.
		return Float(-float64(offset) / 60), nil
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
				v, store, err := rt.setDateParts(t, args, start, count, isUTC)
				if err != nil {
					return Undefined, err
				}
				if store {
					this.Object().data = v
				}
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
		return t.Format("Mon Jan 02 2006 15:04:05 GMT-0700 ") + rt.zoneLabel(t)
	})
	format("toDateString", func(rt *Runtime, t time.Time) string {
		return t.Format("Mon Jan 02 2006")
	})
	format("toTimeString", func(rt *Runtime, t time.Time) string {
		return t.Format("15:04:05 GMT-0700 ") + rt.zoneLabel(t)
	})
	// The three toLocale methods are Intl.DateTimeFormat with the fields each
	// of them stands for, which is what ECMA-402 defines them as.
	locale := func(name, required string, defaults map[string]string) {
		r.defMethod(p, name, 0, func(rt *Runtime, this Value, args []Value) (Value, error) {
			t, err := rt.dateValueOf(this, "Date.prototype."+name)
			if err != nil {
				return Undefined, err
			}
			if math.IsNaN(t) {
				return Str(NewString("Invalid Date")), nil
			}
			o, err := rt.dateOptionsFrom(args, defaults, required)
			if err != nil {
				return Undefined, err
			}
			return Str(NewString(o.format(o.at(t)))), nil
		})
	}
	locale("toLocaleString", "any", map[string]string{
		"year": "numeric", "month": "numeric", "day": "numeric",
		"hour": "numeric", "minute": "numeric", "second": "numeric",
	})
	locale("toLocaleDateString", "date", map[string]string{
		"year": "numeric", "month": "numeric", "day": "numeric",
	})
	locale("toLocaleTimeString", "time", map[string]string{
		"hour": "numeric", "minute": "numeric", "second": "numeric",
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
	// The arithmetic is done in float64, in the groupings the specification
	// writes, because the rounding is observable: a component far outside its
	// ordinary range is not an error, and where the sum lands depends on the
	// order the terms were added in.
	tv := makeDate(makeDay(year, month, day), makeTime(hour, min, sec, ms))
	if utc {
		return clipTime(tv)
	}
	return clipTime(r.utcFromLocal(tv))
}

// utcFromLocal reads a time value as a local time and returns the instant it
// names, which is what every constructor and setter that is not the UTC form
// does with the components it was given.
func (r *Runtime) utcFromLocal(tv float64) float64 {
	if math.IsNaN(tv) || math.Abs(tv) > maxTimeValue+msPerDay {
		// Beyond the range no offset could bring it back, and the conversion
		// would have to leave the range Go's time can hold.
		return math.NaN()
	}
	// The offset is the zone's at the instant the components name, which is
	// not known until the offset is: the first guess reads the offset at the
	// same number of milliseconds treated as UTC, and the second confirms it.
	_, off := r.timeAt(tv, false).Zone()
	guess := tv - float64(off)*1000
	if _, again := r.timeAt(guess, false).Zone(); again != off {
		guess = tv - float64(again)*1000
	}
	return guess
}

// makeDay is the day number the year, month and date name, with out-of-range
// components rolling over: month 12 is January of the next year, and day 0 is
// the last day of the previous month.
func makeDay(year, month, date float64) float64 {
	if !finiteAll(year, month, date) {
		return math.NaN()
	}
	y, m, dt := math.Trunc(year), math.Trunc(month), math.Trunc(date)
	ym := y + math.Floor(m/12)
	if math.IsInf(ym, 0) {
		return math.NaN()
	}
	mn := int(m - math.Floor(m/12)*12)
	return dayFromYear(ym) + float64(dayOfYearForMonth(mn, isLeapYearFloat(ym))) + dt - 1
}

// dayFromYear is the day number of the first day of a year.
func dayFromYear(y float64) float64 {
	return 365*(y-1970) + math.Floor((y-1969)/4) -
		math.Floor((y-1901)/100) + math.Floor((y-1601)/400)
}

// isLeapYearFloat is isLeapYear for a year too large for an int, which a time
// value's components may name before the range check refuses them.
func isLeapYearFloat(y float64) bool {
	if math.Mod(y, 4) != 0 {
		return false
	}
	if math.Mod(y, 100) != 0 {
		return true
	}
	return math.Mod(y, 400) == 0
}

// dayOfYearForMonth is how many days precede a month within its year.
func dayOfYearForMonth(month int, leap bool) int {
	days := [...]int{0, 31, 59, 90, 120, 151, 181, 212, 243, 273, 304, 334}
	d := days[month]
	if leap && month >= 2 {
		d++
	}
	return d
}

// makeTime is the milliseconds within a day the components name, summed in the
// order the specification sums them.
func makeTime(hour, min, sec, ms float64) float64 {
	if !finiteAll(hour, min, sec, ms) {
		return math.NaN()
	}
	h, m, s, milli := math.Trunc(hour), math.Trunc(min), math.Trunc(sec), math.Trunc(ms)
	// Each product is rounded before it is added, which the conversions force:
	// a fused multiply-add would keep more precision than the specification's
	// arithmetic has, and the difference is observable at these magnitudes.
	return ((float64(h*msPerHour) + float64(m*msPerMinute)) +
		float64(s*msPerSecond)) + milli
}

// makeDate combines a day number and a time within the day.
func makeDate(day, time float64) float64 {
	if math.IsNaN(day) || math.IsNaN(time) ||
		math.IsInf(day, 0) || math.IsInf(time, 0) {
		return math.NaN()
	}
	return float64(day*msPerDay) + time
}

func finiteAll(vs ...float64) bool {
	for _, v := range vs {
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return false
		}
	}
	return true
}

// setDateParts replaces count components starting at start, leaving the rest of
// the date unchanged.
// The bool reports whether the result should be stored: a setter that finds an
// invalid date reports NaN without writing anything, so a valueOf that revived
// the date in the meantime is not undone.
func (r *Runtime) setDateParts(t float64, args []Value, start, count int, utc bool) (float64, bool, error) {
	// Every argument is coerced, in order, before anything else happens. A
	// valueOf can see that it was called, and it is called even when the date
	// is already invalid or an earlier argument was NaN.
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
			return 0, false, err
		}
		given[i] = v
	}

	// Setting a component of an invalid date leaves it invalid, except for
	// setFullYear, which the specification lets revive one from the epoch.
	base := t
	revived := false
	if math.IsNaN(base) {
		if start != 0 {
			return math.NaN(), false, nil
		}
		// The specification substitutes +0 for the time value and skips the
		// conversion to local time, so the components come from the epoch
		// itself rather than from what the epoch reads as here.
		base, revived = 0, true
	}
	tm := r.timeAt(base, utc || revived)

	parts := [7]float64{
		float64(tm.Year()),
		float64(int(tm.Month()) - 1),
		float64(tm.Day()),
		float64(tm.Hour()),
		float64(tm.Minute()),
		float64(tm.Second()),
		float64(tm.Nanosecond() / 1e6),
	}
	for i := 0; i < n; i++ {
		v := given[i]
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return math.NaN(), true, nil
		}
		parts[start+i] = math.Trunc(v)
	}
	out := r.composeTime(parts[0], parts[1], parts[2], parts[3], parts[4], parts[5], parts[6], utc)
	return out, true, nil
}

// dateFormats are the layouts Date.parse accepts, tried in order.
//
// The specification requires only the ISO form; the rest are the legacy shapes
// that every engine accepts in practice and that real data contains.
var dateFormats = []string{
	"2006-01-02T15:04:05.000Z07:00",
	"2006-01-02T15:04:05Z07:00",
	"2006-01-02T15:04Z07:00",
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

// parseExtendedYear reads the ±YYYYYY form of an ISO date, which a year
// outside four digits needs and which no Go layout can express.
func parseExtendedYear(s string, loc *time.Location) (float64, bool) {
	if len(s) < 7 || (s[0] != '+' && s[0] != '-') {
		return 0, false
	}
	for i := 1; i < 7; i++ {
		if s[i] < '0' || s[i] > '9' {
			return 0, false
		}
	}
	year, err := strconv.Atoi(s[:7])
	if err != nil {
		return 0, false
	}
	if year == 0 && s[0] == '-' {
		// There is no year minus zero: the form exists to name years before
		// the first, and zero is already spelled +000000.
		return math.NaN(), true
	}
	// The rest is an ordinary ISO date with a stand-in year, which is put back
	// by shifting the result by the distance between the two January firsts.
	// The stand-in has to agree with the real year about February, or every
	// date after it would move by a day.
	stand := 2001
	if isLeapYear(year) {
		stand = 2000
	}
	ms := parseDate(strconv.Itoa(stand)+s[7:], loc)
	if math.IsNaN(ms) {
		return math.NaN(), true
	}
	// The shift is the distance between the two January firsts, taken in
	// milliseconds rather than as a Duration: the span of a six-digit year
	// overflows the nanoseconds a Duration counts.
	from := time.Date(stand, time.January, 1, 0, 0, 0, 0, time.UTC).UnixMilli()
	to := time.Date(year, time.January, 1, 0, 0, 0, 0, time.UTC).UnixMilli()
	return clipTime(ms + float64(to-from)), true
}

// isLeapYear reports whether a proleptic Gregorian year has a February 29th.
func isLeapYear(y int) bool {
	return y%4 == 0 && (y%100 != 0 || y%400 == 0)
}

// parseDate implements Date.parse, returning NaN for anything it cannot read.
func parseDate(s string, loc *time.Location) float64 {
	s = strings.TrimSpace(s)
	if s == "" {
		return math.NaN()
	}

	// An extended year -- six digits with a sign -- is outside what a Go
	// layout can express, so the year is taken off the front and put back
	// afterwards. It is what toISOString writes for a date beyond four digits.
	if ms, ok := parseExtendedYear(s, loc); ok {
		return ms
	}

	// A trailing zone name in parentheses is what toString writes after the
	// offset. Go can read an abbreviation there but not an offset, and the
	// offset is already in the text, so the whole parenthesis goes.
	if strings.HasSuffix(s, ")") {
		if i := strings.LastIndex(s, " ("); i > 0 {
			s = s[:i]
		}
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
