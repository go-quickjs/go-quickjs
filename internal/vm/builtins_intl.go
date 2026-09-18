package vm

import (
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/go-quickjs/go-quickjs/internal/icu"
)

// Intl, as far as it goes with the locale data the engine carries.
//
// ECMA-402 is written against the whole of CLDR, which is tens of megabytes; a
// hundred locales' worth of it is in internal/icu, which is what these objects
// read. A tag that is not there falls back to its language and then to English,
// and resolvedOptions says which locale it settled on, so a program can tell.
//
// What is here: NumberFormat, DateTimeFormat, Collator, PluralRules,
// ListFormat and RelativeTimeFormat, each with format, formatToParts where the
// standard has one, resolvedOptions and supportedLocalesOf. What is not: the
// calendars other than the Gregorian, the time zones other than UTC and the
// machine's own, DisplayNames, Segmenter, and a collation that follows the
// Unicode algorithm rather than comparing what the letters decompose to.

// initIntlBuiltins puts Intl on the global object, but does not build it.
//
// Intl is six constructors with their prototypes and methods, and most
// programs never format anything: building it with every runtime costs every
// embedder for what few of them use. So the property is an accessor that
// builds the namespace the first time it is read and then puts itself away,
// leaving an ordinary property behind. The only way to tell is to ask for the
// property's descriptor before anything has touched it.
func (r *Runtime) initIntlBuiltins() {
	name := r.atoms.intern("Intl")
	build := r.newNativeFunc("get Intl", 0, func(rt *Runtime, this Value, args []Value) (Value, error) {
		intl := rt.buildIntl()
		rt.global.setOwnRaw(name, Obj(intl), propWritable|propConfigurable)
		return Obj(intl), nil
	})
	// A setter, so that a host that wants its own Intl can simply assign one.
	set := r.newNativeFunc("set Intl", 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
		rt.global.setOwnRaw(name, arg(args, 0), propWritable|propConfigurable)
		return Undefined, nil
	})
	r.defineAccessor(r.global, name, build, set, propConfigurable)
}

// buildIntl makes the namespace, the first time anything asks for it.
func (r *Runtime) buildIntl() *Object {
	r.intlProtos = map[string]*Object{}
	intl := newObject(r.proto.object, ClassObject)
	r.defToStringTag(intl, "Intl")

	r.initNumberFormat(intl)
	r.initDateTimeFormat(intl)
	r.initCollator(intl)
	r.initPluralRules(intl)
	r.initListFormat(intl)
	r.initDisplayNames(intl)
	r.initRelativeTimeFormat(intl)

	r.defMethod(intl, "getCanonicalLocales", 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
		tags, err := rt.requestedLocales(arg(args, 0))
		if err != nil {
			return Undefined, err
		}
		out := make([]Value, len(tags))
		for i, tag := range tags {
			out[i] = Str(NewString(tag))
		}
		return Obj(rt.newArrayFrom(out)), nil
	})

	r.defMethod(intl, "supportedValuesOf", 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
		key, err := rt.toString(arg(args, 0))
		if err != nil {
			return Undefined, err
		}
		var values []string
		switch key.Go() {
		case "calendar":
			values = []string{"gregory"}
		case "collation":
			values = []string{"default"}
		case "currency":
			values = icu.Currencies()
		case "numberingSystem":
			values = []string{"latn"}
		case "timeZone":
			// The ones this machine can actually load, which is all of them
			// where the zone files are there and none where they are not.
			for _, zone := range icu.Zones() {
				if _, err := time.LoadLocation(zone); err == nil {
					values = append(values, zone)
				}
			}
		case "unit":
			values = nil
		default:
			return Undefined, rt.throwRangeError("there is no such key: %s", key.Go())
		}
		out := make([]Value, len(values))
		for i, v := range values {
			out[i] = Str(NewString(v))
		}
		return Obj(rt.newArrayFrom(out)), nil
	})
	return intl
}

// --- the options every constructor reads ------------------------------------

// requestedLocales reads the locales argument, which is a tag, a list of them,
// or nothing.
func (r *Runtime) requestedLocales(v Value) ([]string, error) {
	if v.IsUndefined() {
		return nil, nil
	}
	if v.IsString() {
		tag := v.String().Go()
		if !validLanguageTag(tag) {
			return nil, r.throwRangeError("that is not a language tag: %s", tag)
		}
		return []string{canonicalTag(tag)}, nil
	}
	o, err := r.toObject(v)
	if err != nil {
		return nil, err
	}
	// A Locale object, or anything else with a baseName, says which tag it is.
	if base, err := r.getProp(o, r.atoms.intern("baseName"), v); err == nil && base.IsString() {
		return []string{canonicalTag(base.String().Go())}, nil
	}
	length, err := r.lengthOf(o)
	if err != nil {
		return nil, err
	}
	var out []string
	for i := int64(0); i < length; i++ {
		item, err := r.getProp(o, r.atoms.intern(strconv.FormatInt(i, 10)), v)
		if err != nil {
			return nil, err
		}
		if item.IsUndefined() {
			continue
		}
		s, err := r.toString(item)
		if err != nil {
			return nil, err
		}
		if !validLanguageTag(s.Go()) {
			return nil, r.throwRangeError("that is not a language tag: %s", s.Go())
		}
		out = append(out, canonicalTag(s.Go()))
	}
	return out, nil
}

// validLanguageTag reports whether a tag is written the way a tag is written,
// which is as much of BCP 47 as matters here.
func validLanguageTag(tag string) bool {
	if tag == "" {
		return false
	}
	for i, part := range strings.Split(strings.ReplaceAll(tag, "_", "-"), "-") {
		if part == "" {
			return false
		}
		if i == 0 {
			if len(part) < 2 || len(part) > 8 || !allLetters(part) {
				return false
			}
			continue
		}
		if len(part) > 8 || !allAlphanumeric(part) {
			return false
		}
	}
	return true
}

func allLetters(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z') {
			return false
		}
	}
	return true
}

func allAlphanumeric(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9') {
			return false
		}
	}
	return true
}

// canonicalTag writes a tag the way BCP 47 does: the language in lower case,
// a script with its first letter capital, a region in upper case.
func canonicalTag(tag string) string {
	parts := strings.Split(strings.ReplaceAll(tag, "_", "-"), "-")
	for i, part := range parts {
		switch {
		case i == 0:
			parts[i] = strings.ToLower(part)
		case len(part) == 4 && allLetters(part):
			parts[i] = strings.ToUpper(part[:1]) + strings.ToLower(part[1:])
		case len(part) == 2 && allLetters(part):
			parts[i] = strings.ToUpper(part)
		default:
			parts[i] = strings.ToLower(part)
		}
	}
	return strings.Join(parts, "-")
}

// resolveLocale picks the data for the first tag that has any, and reports
// what it settled on.
func (r *Runtime) resolveLocale(tags []string) (*icu.Locale, string) {
	for _, tag := range tags {
		if icu.Has(tag) {
			return icu.Resolve(tag), tag
		}
	}
	if len(tags) > 0 {
		// Nothing had data of its own, so the first is answered with English.
		return icu.Resolve(tags[0]), icu.Resolve(tags[0]).Tag
	}
	l := icu.Resolve("")
	return l, l.Tag
}

// optionsObject turns the options argument into something to read from.
func (r *Runtime) optionsObject(v Value) (*Object, error) {
	if v.IsUndefined() {
		return newObject(nil, ClassObject), nil
	}
	return r.toObject(v)
}

// stringOption reads one option, checking it against the values it may take.
func (r *Runtime) stringOption(o *Object, name, fallback string, allowed ...string) (string, error) {
	v, err := r.getProp(o, r.atoms.intern(name), Obj(o))
	if err != nil {
		return "", err
	}
	if v.IsUndefined() {
		return fallback, nil
	}
	s, err := r.toString(v)
	if err != nil {
		return "", err
	}
	got := s.Go()
	if len(allowed) == 0 {
		return got, nil
	}
	for _, want := range allowed {
		if got == want {
			return got, nil
		}
	}
	return "", r.throwRangeError("%s is not a value %s may take", got, name)
}

// boolOption reads an option that is a flag, with undefined meaning unset.
func (r *Runtime) boolOption(o *Object, name string) (bool, bool, error) {
	v, err := r.getProp(o, r.atoms.intern(name), Obj(o))
	if err != nil {
		return false, false, err
	}
	if v.IsUndefined() {
		return false, false, nil
	}
	return v.Truthy(), true, nil
}

// intOption reads a number option and checks its bounds.
func (r *Runtime) intOption(o *Object, name string, min, max, fallback int) (int, bool, error) {
	v, err := r.getProp(o, r.atoms.intern(name), Obj(o))
	if err != nil {
		return 0, false, err
	}
	if v.IsUndefined() {
		return fallback, false, nil
	}
	n, err := r.toNumber(v)
	if err != nil {
		return 0, false, err
	}
	if math.IsNaN(n) || n < float64(min) || n > float64(max) {
		return 0, false, r.throwRangeError("%s is out of range", name)
	}
	return int(n), true, nil
}

// --- NumberFormat -----------------------------------------------------------

func (r *Runtime) initNumberFormat(intl *Object) {
	proto := newObject(r.proto.object, ClassObject)
	ctor := r.newCtor("NumberFormat", 0, proto, func(rt *Runtime, this Value, args []Value) (Value, error) {
		o, err := rt.newNumberFormat(args)
		if err != nil {
			return Undefined, err
		}
		return Obj(o), nil
	})
	r.defValue(intl, "NumberFormat", Obj(ctor))
	r.intlProtos["NumberFormat"] = proto
	r.defToStringTag(proto, "Intl.NumberFormat")
	r.defSupportedLocalesOf(ctor)

	r.defMethod(proto, "format", 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
		o, err := rt.numberFormatOf(this)
		if err != nil {
			return Undefined, err
		}
		x, err := rt.toNumber(arg(args, 0))
		if err != nil {
			return Undefined, err
		}
		return Str(NewString(o.format(x))), nil
	})
	r.defMethod(proto, "formatToParts", 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
		o, err := rt.numberFormatOf(this)
		if err != nil {
			return Undefined, err
		}
		x, err := rt.toNumber(arg(args, 0))
		if err != nil {
			return Undefined, err
		}
		pieces := o.parts(x)
		out := make([]Value, len(pieces))
		for i, piece := range pieces {
			out[i] = Obj(rt.partObject(piece.kind, piece.value))
		}
		return Obj(rt.newArrayFrom(out)), nil
	})
	r.defMethod(proto, "resolvedOptions", 0, func(rt *Runtime, this Value, args []Value) (Value, error) {
		o, err := rt.numberFormatOf(this)
		if err != nil {
			return Undefined, err
		}
		out := newObject(rt.proto.object, ClassObject)
		rt.putString(out, "locale", o.requested)
		rt.putString(out, "numberingSystem", o.locale.Numbering)
		rt.putString(out, "style", o.style)
		if o.currency != "" {
			rt.putString(out, "currency", o.currency)
			rt.putString(out, "currencyDisplay", o.currencyDisplay)
			rt.putString(out, "currencySign", "standard")
		}
		if o.unit != "" {
			rt.putString(out, "unit", o.unit)
			rt.putString(out, "unitDisplay", o.unitDisplay)
		}
		rt.putInt(out, "minimumIntegerDigits", o.minInt)
		if o.maxSig > 0 {
			rt.putInt(out, "minimumSignificantDigits", o.minSig)
			rt.putInt(out, "maximumSignificantDigits", o.maxSig)
		} else {
			rt.putInt(out, "minimumFractionDigits", o.minFrac)
			rt.putInt(out, "maximumFractionDigits", o.maxFrac)
		}
		rt.putBool(out, "useGrouping", o.useGrouping)
		rt.putString(out, "notation", o.notation)
		if o.notation == "compact" {
			rt.putString(out, "compactDisplay", o.compactDisplay)
		}
		rt.putString(out, "signDisplay", o.signDisplay)
		rt.putString(out, "roundingMode", "halfExpand")
		return Obj(out), nil
	})
}

// newNumberFormat reads the arguments a NumberFormat is made with.
func (r *Runtime) newNumberFormat(args []Value) (*Object, error) {
	o, err := r.numberOptionsFrom(args)
	if err != nil {
		return nil, err
	}
	out := newObject(r.protoFromNewTarget(r.intlProtoOf("NumberFormat")), ClassObject)
	out.data = o
	return out, nil
}

func (r *Runtime) numberOptionsFrom(args []Value) (*numberOptions, error) {
	tags, err := r.requestedLocales(arg(args, 0))
	if err != nil {
		return nil, err
	}
	options, err := r.optionsObject(arg(args, 1))
	if err != nil {
		return nil, err
	}
	locale, requested := r.resolveLocale(tags)
	o := &numberOptions{locale: locale, requested: requested}

	if o.style, err = r.stringOption(options, "style", "decimal",
		"decimal", "percent", "currency", "unit"); err != nil {
		return nil, err
	}
	currency, err := r.stringOption(options, "currency", "")
	if err != nil {
		return nil, err
	}
	if currency != "" {
		if len(currency) != 3 || !allLetters(currency) {
			return nil, r.throwRangeError("that is not a currency code: %s", currency)
		}
		o.currency = strings.ToUpper(currency)
	}
	if o.style == "currency" && o.currency == "" {
		return nil, r.throwTypeError("a currency style needs a currency")
	}
	if o.currencyDisplay, err = r.stringOption(options, "currencyDisplay", "symbol",
		"code", "symbol", "narrowSymbol", "name"); err != nil {
		return nil, err
	}
	if o.unit, err = r.stringOption(options, "unit", ""); err != nil {
		return nil, err
	}
	if o.style == "unit" && o.unit == "" {
		return nil, r.throwTypeError("a unit style needs a unit")
	}
	if o.unitDisplay, err = r.stringOption(options, "unitDisplay", "short",
		"short", "narrow", "long"); err != nil {
		return nil, err
	}
	if o.notation, err = r.stringOption(options, "notation", "standard",
		"standard", "scientific", "engineering", "compact"); err != nil {
		return nil, err
	}
	if o.compactDisplay, err = r.stringOption(options, "compactDisplay", "short",
		"short", "long"); err != nil {
		return nil, err
	}
	if o.signDisplay, err = r.stringOption(options, "signDisplay", "auto",
		"auto", "never", "always", "exceptZero", "negative"); err != nil {
		return nil, err
	}

	grouping, set, err := r.boolOption(options, "useGrouping")
	if err != nil {
		return nil, err
	}
	o.useGrouping = !set || grouping
	if set && !grouping {
		o.useGrouping = false
	}

	// How many digits: a currency is written with as many as it has, and
	// everything else with up to three.
	fractionDigits := 0
	if o.style == "currency" {
		fractionDigits = icu.CurrencyDigits(o.currency)
	}
	if o.minInt, _, err = r.intOption(options, "minimumIntegerDigits", 1, 21, 1); err != nil {
		return nil, err
	}
	minFracDefault, maxFracDefault := 0, 3
	if o.style == "currency" {
		minFracDefault, maxFracDefault = fractionDigits, fractionDigits
	} else if o.style == "percent" {
		maxFracDefault = 0
	}
	minSet, maxSet := false, false
	if o.minFrac, minSet, err = r.intOption(options, "minimumFractionDigits", 0, 100, minFracDefault); err != nil {
		return nil, err
	}
	if o.maxFrac, maxSet, err = r.intOption(options, "maximumFractionDigits", 0, 100, maxFracDefault); err != nil {
		return nil, err
	}
	if minSet && !maxSet && o.maxFrac < o.minFrac {
		o.maxFrac = o.minFrac
	}
	if o.maxFrac < o.minFrac {
		return nil, r.throwRangeError("the fraction digits are the wrong way round")
	}
	if o.minSig, _, err = r.intOption(options, "minimumSignificantDigits", 1, 21, 1); err != nil {
		return nil, err
	}
	if o.maxSig, maxSet, err = r.intOption(options, "maximumSignificantDigits", 1, 21, 0); err != nil {
		return nil, err
	}
	if !maxSet {
		// Significant digits are only in force when they were asked for.
		if _, set, err := r.intOption(options, "minimumSignificantDigits", 1, 21, 0); err == nil && set {
			o.maxSig = 21
		} else {
			o.maxSig = 0
		}
	}
	return o, nil
}

func (r *Runtime) numberFormatOf(this Value) (*numberOptions, error) {
	if o := this.Object(); o != nil {
		if opts, ok := o.data.(*numberOptions); ok {
			return opts, nil
		}
	}
	return nil, r.throwTypeError("this is not an Intl.NumberFormat")
}

// --- DateTimeFormat ---------------------------------------------------------

func (r *Runtime) initDateTimeFormat(intl *Object) {
	proto := newObject(r.proto.object, ClassObject)
	ctor := r.newCtor("DateTimeFormat", 0, proto, func(rt *Runtime, this Value, args []Value) (Value, error) {
		o, err := rt.dateOptionsFrom(args, nil)
		if err != nil {
			return Undefined, err
		}
		out := newObject(rt.protoFromNewTarget(rt.intlProtoOf("DateTimeFormat")), ClassObject)
		out.data = o
		return Obj(out), nil
	})
	r.defValue(intl, "DateTimeFormat", Obj(ctor))
	r.intlProtos["DateTimeFormat"] = proto
	r.defToStringTag(proto, "Intl.DateTimeFormat")
	r.defSupportedLocalesOf(ctor)

	r.defMethod(proto, "format", 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
		o, err := rt.dateFormatOf(this)
		if err != nil {
			return Undefined, err
		}
		t, err := rt.dateArgument(o, arg(args, 0))
		if err != nil {
			return Undefined, err
		}
		return Str(NewString(o.format(t))), nil
	})
	r.defMethod(proto, "formatToParts", 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
		o, err := rt.dateFormatOf(this)
		if err != nil {
			return Undefined, err
		}
		t, err := rt.dateArgument(o, arg(args, 0))
		if err != nil {
			return Undefined, err
		}
		pieces := o.parts(t)
		out := make([]Value, len(pieces))
		for i, piece := range pieces {
			out[i] = Obj(rt.partObject(piece.kind, piece.value))
		}
		return Obj(rt.newArrayFrom(out)), nil
	})
	r.defMethod(proto, "resolvedOptions", 0, func(rt *Runtime, this Value, args []Value) (Value, error) {
		o, err := rt.dateFormatOf(this)
		if err != nil {
			return Undefined, err
		}
		out := newObject(rt.proto.object, ClassObject)
		rt.putString(out, "locale", o.requested)
		rt.putString(out, "calendar", "gregory")
		rt.putString(out, "numberingSystem", o.locale.Numbering)
		rt.putString(out, "timeZone", o.timeZone)
		if o.hour != "" {
			rt.putBool(out, "hour12", o.hour12)
			rt.putString(out, "hourCycle", o.hourCycle)
		}
		for name, value := range map[string]string{
			"weekday": o.weekday, "era": o.era, "year": o.year, "month": o.month,
			"day": o.day, "hour": o.hour, "minute": o.minute, "second": o.second,
			"timeZoneName": o.timeZoneName, "dateStyle": o.dateStyle,
			"timeStyle": o.timeStyle,
		} {
			if value != "" {
				rt.putString(out, name, value)
			}
		}
		return Obj(out), nil
	})
}

// dateArgument is the instant a format was asked about: the argument, or now
// when there was none.
func (r *Runtime) dateArgument(o *dateOptions, v Value) (time.Time, error) {
	if v.IsUndefined() {
		return o.at(r.now()), nil
	}
	n, err := r.toNumber(v)
	if err != nil {
		return time.Time{}, err
	}
	if math.IsNaN(n) || math.Abs(n) > 8.64e15 {
		return time.Time{}, r.throwRangeError("that is not a date this can format")
	}
	return o.at(n), nil
}

// dateOptionsFrom reads the arguments a DateTimeFormat is made with. defaults
// fills in the fields the toLocale methods ask for when the caller named none.
func (r *Runtime) dateOptionsFrom(args []Value, defaults map[string]string) (*dateOptions, error) {
	tags, err := r.requestedLocales(arg(args, 0))
	if err != nil {
		return nil, err
	}
	options, err := r.optionsObject(arg(args, 1))
	if err != nil {
		return nil, err
	}
	locale, requested := r.resolveLocale(tags)
	o := &dateOptions{locale: locale, requested: requested, timeZone: "UTC"}

	zone, err := r.stringOption(options, "timeZone", "")
	if err != nil {
		return nil, err
	}
	switch {
	case zone == "":
		// The machine's own zone, which is the one a Date is written in.
		o.zone = r.location()
		o.timeZone = r.localZoneName()
	case isUTCName(zone):
		o.zone = time.UTC
		o.timeZone = "UTC"
	default:
		// Any zone the machine has the data for. Loading it reads the zone
		// files the operating system keeps, or the copy a host embedded by
		// importing time/tzdata; a script cannot reach either.
		name := canonicalZone(zone)
		loc, err := time.LoadLocation(name)
		if err != nil {
			return nil, r.throwRangeError("there is no such time zone here: %s", zone)
		}
		o.zone, o.timeZone = loc, name
	}

	if o.dateStyle, err = r.stringOption(options, "dateStyle", "",
		"full", "long", "medium", "short"); err != nil {
		return nil, err
	}
	if o.timeStyle, err = r.stringOption(options, "timeStyle", "",
		"full", "long", "medium", "short"); err != nil {
		return nil, err
	}

	read := func(name string, allowed ...string) (string, error) {
		return r.stringOption(options, name, "", allowed...)
	}
	if o.weekday, err = read("weekday", "narrow", "short", "long"); err != nil {
		return nil, err
	}
	if o.era, err = read("era", "narrow", "short", "long"); err != nil {
		return nil, err
	}
	if o.year, err = read("year", "numeric", "2-digit"); err != nil {
		return nil, err
	}
	if o.month, err = read("month", "numeric", "2-digit", "narrow", "short", "long"); err != nil {
		return nil, err
	}
	if o.day, err = read("day", "numeric", "2-digit"); err != nil {
		return nil, err
	}
	if o.hour, err = read("hour", "numeric", "2-digit"); err != nil {
		return nil, err
	}
	if o.minute, err = read("minute", "numeric", "2-digit"); err != nil {
		return nil, err
	}
	if o.second, err = read("second", "numeric", "2-digit"); err != nil {
		return nil, err
	}
	if o.timeZoneName, err = read("timeZoneName", "short", "long", "shortOffset",
		"longOffset", "shortGeneric", "longGeneric"); err != nil {
		return nil, err
	}

	if (o.dateStyle != "" || o.timeStyle != "") &&
		(o.weekday != "" || o.year != "" || o.month != "" || o.day != "" ||
			o.hour != "" || o.minute != "" || o.second != "") {
		return nil, r.throwTypeError("a style and a field cannot both be asked for")
	}

	// Nothing asked for at all is a date, or whatever the caller said instead.
	if o.dateStyle == "" && o.timeStyle == "" && o.weekday == "" && o.year == "" &&
		o.month == "" && o.day == "" && o.hour == "" && o.minute == "" && o.second == "" {
		if defaults == nil {
			defaults = map[string]string{"year": "numeric", "month": "numeric", "day": "numeric"}
		}
		o.weekday, o.era = defaults["weekday"], defaults["era"]
		o.year, o.month, o.day = defaults["year"], defaults["month"], defaults["day"]
		o.hour, o.minute, o.second = defaults["hour"], defaults["minute"], defaults["second"]
	}

	hour12, set, err := r.boolOption(options, "hour12")
	if err != nil {
		return nil, err
	}
	cycle, err := r.stringOption(options, "hourCycle", "", "h11", "h12", "h23", "h24")
	if err != nil {
		return nil, err
	}
	switch {
	case set:
		o.hour12, o.hourSet = hour12, true
	case cycle != "":
		o.hour12, o.hourSet = cycle == "h11" || cycle == "h12", true
	default:
		o.hour12 = locale.Hour12
	}
	if o.hour12 {
		o.hourCycle = "h12"
	} else {
		o.hourCycle = "h23"
	}

	o.pattern = o.patternFor()
	// A twelve-hour clock asked for where the locale writes a
	// twenty-four-hour one, or the other way round, changes the pattern.
	o.pattern = adjustClock(o.pattern, o.hour12)
	return o, nil
}

// adjustClock writes the hour on the clock that was asked for, and puts the
// day period there or takes it away to match.
func adjustClock(pattern string, hour12 bool) string {
	var b strings.Builder
	inQuote := false
	for i := 0; i < len(pattern); i++ {
		c := pattern[i]
		if c == '\'' {
			inQuote = !inQuote
			b.WriteByte(c)
			continue
		}
		if inQuote {
			b.WriteByte(c)
			continue
		}
		switch {
		case c == 'h' && !hour12:
			b.WriteByte('H')
		case c == 'H' && hour12:
			b.WriteByte('h')
		case (c == 'a' || c == 'B' || c == 'b') && !hour12:
			// The day period goes, and whatever space was in front of it.
			s := b.String()
			b.Reset()
			b.WriteString(strings.TrimRight(s, " \u00a0\u202f"))
		default:
			b.WriteByte(c)
		}
	}
	out := b.String()
	letters := patternLettersOf(out)
	if hour12 && strings.ContainsRune(letters, 'h') &&
		!strings.ContainsAny(letters, "aBb") {
		out += " a"
	}
	return out
}

// patternLettersOf is a pattern with its literals taken out, for asking which
// fields it names.
func patternLettersOf(pattern string) string {
	var b strings.Builder
	inQuote := false
	for i := 0; i < len(pattern); i++ {
		c := pattern[i]
		if c == '\'' {
			inQuote = !inQuote
			continue
		}
		if !inQuote && isPatternLetter(c) {
			b.WriteByte(c)
		}
	}
	return b.String()
}

// canonicalZone writes a zone name the way the zone files spell it, so that a
// tag written in any case finds its zone: america/new_york is New York.
func canonicalZone(zone string) string {
	parts := strings.Split(zone, "/")
	for i, part := range parts {
		var b strings.Builder
		upper := true
		for j := 0; j < len(part); j++ {
			c := part[j]
			switch {
			case upper && c >= 'a' && c <= 'z':
				b.WriteByte(c - ('a' - 'A'))
			case !upper && c >= 'A' && c <= 'Z':
				b.WriteByte(c + ('a' - 'A'))
			default:
				b.WriteByte(c)
			}
			// A new word starts after a separator, and GMT+5 is left alone.
			upper = c == '_' || c == '-' || c == '/'
		}
		parts[i] = b.String()
	}
	return strings.Join(parts, "/")
}

func isUTCName(zone string) bool {
	switch strings.ToUpper(zone) {
	case "UTC", "GMT", "ETC/UTC", "ETC/GMT", "ETC/GREENWICH", "UNIVERSAL", "Z":
		return true
	}
	return false
}

// localZoneName is what the machine's zone is called, which is the name Go
// knows it by.
func (r *Runtime) localZoneName() string {
	name := r.location().String()
	if name == "" || name == "Local" {
		// A zone with no name of its own is reported as UTC only when it is
		// UTC; otherwise the offset is the best name there is.
		t := time.Now().In(r.location())
		zone, offset := t.Zone()
		if offset == 0 {
			return "UTC"
		}
		return zone
	}
	return name
}

func (r *Runtime) dateFormatOf(this Value) (*dateOptions, error) {
	if o := this.Object(); o != nil {
		if opts, ok := o.data.(*dateOptions); ok {
			return opts, nil
		}
	}
	return nil, r.throwTypeError("this is not an Intl.DateTimeFormat")
}

// --- the smaller constructors -----------------------------------------------

// defSupportedLocalesOf gives a constructor the method that says which of a
// list of tags this engine has data for.
func (r *Runtime) defSupportedLocalesOf(ctor *Object) {
	r.defMethod(ctor, "supportedLocalesOf", 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
		tags, err := rt.requestedLocales(arg(args, 0))
		if err != nil {
			return Undefined, err
		}
		var out []Value
		for _, tag := range tags {
			if icu.Has(tag) {
				out = append(out, Str(NewString(tag)))
			}
		}
		return Obj(rt.newArrayFrom(out)), nil
	})
}

// partObject is one entry of a formatToParts result.
func (r *Runtime) partObject(kind, value string) *Object {
	o := newObject(r.proto.object, ClassObject)
	r.putString(o, "type", kind)
	r.putString(o, "value", value)
	return o
}

func (r *Runtime) putString(o *Object, name, value string) {
	o.setOwnRaw(r.atoms.intern(name), Str(NewString(value)), propDefault)
}

func (r *Runtime) putInt(o *Object, name string, value int) {
	o.setOwnRaw(r.atoms.intern(name), Int(value), propDefault)
}

func (r *Runtime) putBool(o *Object, name string, value bool) {
	o.setOwnRaw(r.atoms.intern(name), Bool(value), propDefault)
}

// intlProtoOf is the prototype a constructor's instances are given, kept aside
// so that finding it does not mean walking the global object.
func (r *Runtime) intlProtoOf(name string) *Object {
	if p, ok := r.intlProtos[name]; ok {
		return p
	}
	return r.proto.object
}
