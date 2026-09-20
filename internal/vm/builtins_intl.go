package vm

import (
	"fmt"
	"math"
	"os"
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
	// The symbol a formatter made without new is hidden under. Calling
	// Intl.NumberFormat as a function on an object that is already one of them
	// hangs the new formatter off the old one rather than replacing it, which
	// is how the first version of this API worked and how some code still
	// uses it.
	r.intlFallback = NewSymbol("IntlLegacyConstructedSymbol", true)
	intl := newObject(r.proto.object, ClassObject)
	r.defToStringTag(intl, "Intl")

	r.initLocale(intl)
	r.initNumberFormat(intl)
	r.initDateTimeFormat(intl)
	r.initCollator(intl)
	r.initPluralRules(intl)
	r.initListFormat(intl)
	r.initDisplayNames(intl)
	r.initRelativeTimeFormat(intl)
	r.initSegmenter(intl)
	r.initDurationFormat(intl)

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
			values = icu.Calendars()
		case "collation":
			values = icu.Collations()
		case "currency":
			values = icu.Currencies()
		case "numberingSystem":
			values = icu.NumberingSystems()
		case "timeZone":
			// The ones this machine can actually load, which is all of them
			// where the zone files are there and none where they are not.
			for _, zone := range icu.Zones() {
				if _, err := loadNamedLocation(zone); err == nil {
					values = append(values, zone)
				}
			}
		case "unit":
			values = icu.Units()
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
// or nothing, and writes each one the one way it is written. A tag that is not
// a tag is refused here rather than further in, where the mistake would be
// harder to see.
func (r *Runtime) requestedLocales(v Value) ([]string, error) {
	if v.IsUndefined() {
		return nil, nil
	}
	var o *Object
	if v.IsString() {
		o = r.newArrayFrom([]Value{v})
	} else {
		var err error
		if o, err = r.toObject(v); err != nil {
			return nil, err
		}
		// A Locale is consumed through its internal slot, without invoking an
		// overridden toString and without dropping its Unicode extensions.
		if locale, ok := r.localeData(v); ok {
			return []string{locale.tag.String()}, nil
		}
	}
	length, err := r.lengthOf(o)
	if err != nil {
		return nil, err
	}
	var out []string
	for i := int64(0); i < length; i++ {
		key := r.atoms.intern(strconv.FormatInt(i, 10))
		has, err := r.hasPropErr(o, key)
		if err != nil {
			return nil, err
		}
		if !has {
			continue
		}
		item, err := r.getProp(o, key, Obj(o))
		if err != nil {
			return nil, err
		}
		if !item.IsString() && !item.IsObject() {
			return nil, r.throwTypeError("a locale is a tag or a Locale")
		}
		if locale, ok := r.localeData(item); ok {
			canonical := locale.tag.String()
			if !contains(out, canonical) {
				out = append(out, canonical)
			}
			continue
		}
		s, err := r.toString(item)
		if err != nil {
			return nil, err
		}
		tag, ok := parseTag(s.Go())
		if !ok {
			return nil, r.throwRangeError("that is not a language tag: %s", s.Go())
		}
		canonical := canonicalTag(tag)
		if !contains(out, canonical) {
			out = append(out, canonical)
		}
	}
	return out, nil
}

func contains(list []string, s string) bool {
	for _, item := range list {
		if item == s {
			return true
		}
	}
	return false
}

// validLanguageTag reports whether a tag is written the way a tag is written.
func validLanguageTag(tag string) bool {
	_, ok := parseTag(tag)
	return ok
}

func allLetters(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z') {
			return false
		}
	}
	return len(s) > 0
}

func allAlphanumeric(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9') {
			return false
		}
	}
	return len(s) > 0
}

// localeChoice is what a constructor settled on: the data to work from, the
// tag to report, and what the "u" extension asked for.
//
// A tag may carry settings of its own -- "de-u-nu-arab" is German written with
// Arabic digits, "en-u-kn" is English sorted with numbers in numeric order --
// and an option given to the constructor overrides one. What the tag asked for
// and got is reported back in the resolved locale; what an option overrode is
// not, since the answer no longer came from the tag.
type localeChoice struct {
	data *icu.Locale
	tag  langTag
	// asked is what the extension said for each key the constructor cares
	// about, and settled is what is in force after the options have spoken.
	asked   map[string]string
	settled map[string]string
}

// resolveLocale picks the data for the first tag that has any, and reads the
// settings the caller asks about out of that tag's "u" extension.
func (r *Runtime) resolveLocale(tags []string, keys ...string) *localeChoice {
	c := &localeChoice{asked: map[string]string{}, settled: map[string]string{}}
	var requested langTag
	found := false
	for _, tag := range tags {
		t, ok := parseTag(tag)
		if !ok {
			continue
		}
		if icu.Has(t.base()) {
			c.data, requested, found = icu.Resolve(t.base()), t, true
			break
		}
	}
	if !found {
		// Either nothing was asked for, or nothing that was asked for has any
		// data: both mean the language the machine is set to, or the one the
		// host chose. What the unanswerable tag asked for in its extensions
		// goes with it, since it was not the tag that was matched.
		requested, _ = parseTag(canonicalTag(mustParse(r.Locale())))
		c.data = icu.Resolve(requested.base())
		if !icu.Has(requested.base()) {
			requested, _ = parseTag(c.data.Tag)
		}
	}

	// The tag to report is the one that was matched, without the extensions,
	// plus the settings that were asked for and can be honoured.
	c.tag = langTag{language: requested.language, script: requested.script,
		region: requested.region, variants: requested.variants}
	var used []keyword
	for _, key := range keys {
		value := defaultSetting(c.data, key)
		if asked, ok := requested.keywordValue(key); ok && supportedSetting(c.data, key, asked) {
			value = asked
			c.asked[key] = asked
			used = append(used, keyword{key: key, value: asked})
		}
		c.settled[key] = value
	}
	c.tag.setKeywords(used)
	return c
}

// mustParse is a tag that came from the host or the machine rather than from a
// script: one that cannot be read is answered with English rather than with an
// error, since there is nobody to report it to.
func mustParse(tag string) langTag {
	if t, ok := parseTag(tag); ok {
		return t
	}
	t, _ := parseTag("en-US")
	return t
}

// setting is what a key is set to, once the tag and the options have both had
// their say.
func (c *localeChoice) setting(key string) string { return c.settled[key] }

// override is an option speaking over the tag. A setting the tag asked for and
// the option agrees with stays in the resolved locale; one the option changes
// leaves it, since it is no longer the tag's doing.
func (c *localeChoice) override(key, value string) {
	// A setting this engine cannot honour is ignored, whether it came from the
	// tag or from the options: a request is not an instruction.
	if value == "" || !supportedSetting(c.data, key, value) || value == c.settled[key] {
		return
	}
	c.settled[key] = value
	if _, ok := c.asked[key]; ok {
		delete(c.asked, key)
		var used []keyword
		for k, v := range c.asked {
			used = append(used, keyword{key: k, value: v})
		}
		c.tag.setKeywords(used)
	}
}

// drop takes a setting out of the resolved locale without changing what it
// settled on: a clock asked for outright answers the question the tag asked,
// so the tag is no longer the reason for the answer.
func (c *localeChoice) drop(key string) {
	if _, ok := c.asked[key]; !ok {
		return
	}
	delete(c.asked, key)
	var used []keyword
	for k, v := range c.asked {
		used = append(used, keyword{key: k, value: v})
	}
	c.tag.setKeywords(used)
}

// locale is the tag to report, which is the one that was matched along with
// the settings it asked for and got.
func (c *localeChoice) locale() string { return c.tag.String() }

// defaultSetting is what a key means when nothing asked for anything.
func defaultSetting(l *icu.Locale, key string) string {
	switch key {
	case "nu":
		return l.Numbering
	case "ca":
		if l.Calendar != "" {
			return l.Calendar
		}
		return "gregory"
	case "co":
		return icu.DefaultCollation(l.Tag)
	case "kn", "kf":
		return "false"
	case "hc":
		return l.HourCycle
	}
	return ""
}

// supportedSetting reports whether a setting a tag asked for is one this
// engine can honour. What it cannot is ignored rather than refused: a tag is
// a request, not an instruction.
func supportedSetting(locale *icu.Locale, key, value string) bool {
	switch key {
	case "nu":
		_, ok := icu.NumberingDigits(value)
		return ok
	case "ca":
		return icu.HasCalendar(value)
	case "co":
		// The orderings named here are the ones a locale may be tailored for;
		// "standard" and "search" are not settings a tag may ask for.
		return icu.HasCollation(locale.Tag, value)
	case "kn":
		return value == "true" || value == "false"
	case "kf":
		return value == "upper" || value == "lower" || value == "false"
	case "hc":
		return value == "h11" || value == "h12" || value == "h23" || value == "h24"
	}
	return false
}

// numberArgument reads the value to format, which is not always a number: a
// string is taken as the exact decimal it is written as rather than as the
// nearest number a machine can hold, so that a value too long for a double
// still comes out right.
func (r *Runtime) numberArgument(v Value) (decimal, string, error) {
	if v.IsBigInt() {
		if d, ok := parseDecimal(v.BigInt().V.String()); ok {
			return d, "", nil
		}
	}
	prim, err := r.toPrimitive(v, hintNumber)
	if err != nil {
		return decimal{}, "", err
	}
	if prim.IsString() {
		text := strings.TrimSpace(prim.String().Go())
		switch text {
		case "":
			return decimal{}, "", nil
		case "Infinity", "+Infinity":
			return decimal{}, "inf", nil
		case "-Infinity":
			return decimal{}, "-inf", nil
		}
		if d, ok := parseDecimal(text); ok {
			return d, "", nil
		}
	}
	x, err := r.toNumber(prim)
	if err != nil {
		return decimal{}, "", err
	}
	switch {
	case math.IsNaN(x):
		return decimal{}, "nan", nil
	case math.IsInf(x, 1):
		return decimal{}, "inf", nil
	case math.IsInf(x, -1):
		return decimal{}, "-inf", nil
	}
	return decimalOf(x), "", nil
}

// legacyFormatter is what a formatter made without new answers with. Called as
// a function on an object that is already a formatter, it hangs the new one
// off the old under a symbol of its own rather than replacing it, and answers
// with the object it was called on -- which is how the first version of this
// API worked, and how some code still uses it.
func (r *Runtime) legacyFormatter(this Value, made *Object) Value {
	if r.Constructing() || !this.IsObject() {
		return Obj(made)
	}
	target := this.Object()
	if !r.inheritsFrom(this, made.proto) {
		return Obj(made)
	}
	target.setOwnRaw(r.atoms.internSymbol(r.intlFallback), Obj(made), 0)
	return this
}

// unwrapFormatter follows the symbol a formatter made without new was hidden
// under, for the methods that have to work on either.
func (r *Runtime) unwrapFormatter(this Value) Value {
	o := this.Object()
	if o == nil {
		return this
	}
	if v, err := r.getProp(o, r.atoms.internSymbol(r.intlFallback), this); err == nil && v.IsObject() {
		return v
	}
	return this
}

// bound hands out the function a format getter answers with. It is made once
// and kept, since a script may compare the one it got with the one it gets
// next, and it carries no name, which is what the standard says of a function
// that was never written down anywhere.
func (r *Runtime) bound(cache **Object, length int, fn NativeFunc) Value {
	if *cache == nil {
		*cache = r.newNativeFunc("", length, fn)
	}
	return Obj(*cache)
}

// typeOption reads an option whose value is a setting a tag could have asked
// for, which has a shape of its own: words of three to eight characters.
func (r *Runtime) typeOption(o *Object, name string) (string, error) {
	v, err := r.getProp(o, r.atoms.intern(name), Obj(o))
	if err != nil {
		return "", err
	}
	if v.IsUndefined() {
		return "", nil
	}
	s, err := r.toString(v)
	if err != nil {
		return "", err
	}
	got := s.Go()
	for _, part := range strings.Split(got, "-") {
		if len(part) < 3 || len(part) > 8 || !allAlphanumeric(part) {
			return "", r.throwRangeError("%s is not a value %s may take", got, name)
		}
	}
	// A setting that has been renamed is answered by its name now, whether it
	// was asked for in a tag or in the options.
	key := map[string]string{
		"calendar": "ca", "numberingSystem": "nu", "collation": "co",
	}[name]
	return canonicalSetting(key, strings.ToLower(got)), nil
}

// boolWord is how a flag is written in a tag.
func boolWord(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

// strictOptions is the options argument where nothing may stand in for an
// object: undefined means no options, and anything else must be one. The older
// formatters take whatever they are given and make an object of it; the newer
// ones refuse.
func (r *Runtime) strictOptions(v Value) (*Object, error) {
	if v.IsUndefined() {
		return newObject(nil, ClassObject), nil
	}
	o := v.Object()
	if o == nil {
		return nil, r.throwTypeError("the options are an object or nothing")
	}
	return o, nil
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
// rawNumberOption reads a number option without judging it, for the digit
// counts, which cannot be judged until it is known which of them are in force.
func (r *Runtime) rawNumberOption(o *Object, name string) (float64, bool, error) {
	v, err := r.getProp(o, r.atoms.intern(name), Obj(o))
	if err != nil {
		return 0, false, err
	}
	if v.IsUndefined() {
		return 0, false, nil
	}
	n, err := r.toNumber(v)
	if err != nil {
		return 0, false, err
	}
	return n, true, nil
}

// boundedOption checks a number option that was read earlier against the
// bounds it has to fall within.
func (r *Runtime) boundedOption(n float64, set bool, name string, min, max, fallback int) (int, error) {
	if !set {
		return fallback, nil
	}
	if math.IsNaN(n) || n < float64(min) || n > float64(max) {
		return 0, r.throwRangeError("%s is out of range", name)
	}
	return int(math.Floor(n)), nil
}

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
		made, err := rt.protoFromNewTargetErr(rt.intlProtoOf("NumberFormat"))
		if err != nil {
			return Undefined, err
		}
		o, err := rt.numberOptionsFrom(args)
		if err != nil {
			return Undefined, err
		}
		out := newObject(made, ClassObject)
		out.data = o
		return rt.legacyFormatter(this, out), nil
	})
	r.defValue(intl, "NumberFormat", Obj(ctor))
	r.intlProtos["NumberFormat"] = proto
	r.defToStringTag(proto, "Intl.NumberFormat")
	r.defSupportedLocalesOf(ctor)

	// format is a getter for a function bound to this formatter, because it is
	// nearly always handed straight to a map.
	r.defGetter(proto, "format", func(rt *Runtime, this Value, args []Value) (Value, error) {
		o, err := rt.numberFormatOf(this)
		if err != nil {
			return Undefined, err
		}
		return rt.bound(&o.formatFn, 1, func(rt *Runtime, _ Value, args []Value) (Value, error) {
			d, special, err := rt.numberArgument(arg(args, 0))
			if err != nil {
				return Undefined, err
			}
			return Str(NewString(piecesText(o.valueParts(d, special)))), nil
		}), nil
	})
	r.defMethod(proto, "formatRange", 2, func(rt *Runtime, this Value, args []Value) (Value, error) {
		pieces, err := rt.numberRange(this, args)
		if err != nil {
			return Undefined, err
		}
		return Str(NewString(pieces.text())), nil
	})
	r.defMethod(proto, "formatRangeToParts", 2, func(rt *Runtime, this Value, args []Value) (Value, error) {
		pieces, err := rt.numberRange(this, args)
		if err != nil {
			return Undefined, err
		}
		return Obj(rt.rangeParts(pieces)), nil
	})
	r.defMethod(proto, "formatToParts", 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
		o, err := rt.numberFormatOf(this)
		if err != nil {
			return Undefined, err
		}
		d, special, err := rt.numberArgument(arg(args, 0))
		if err != nil {
			return Undefined, err
		}
		pieces := o.valueParts(d, special)
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
		rt.putString(out, "locale", o.choice.locale())
		rt.putString(out, "numberingSystem", o.choice.setting("nu"))
		rt.putString(out, "style", o.style)
		if o.style == "currency" {
			rt.putString(out, "currency", o.currency)
			rt.putString(out, "currencyDisplay", o.currencyDisplay)
			rt.putString(out, "currencySign", o.currencySign)
		}
		if o.style == "unit" {
			rt.putString(out, "unit", o.unit)
			rt.putString(out, "unitDisplay", o.unitDisplay)
		}
		rt.putInt(out, "minimumIntegerDigits", o.minInt)
		if o.reportFrac {
			rt.putInt(out, "minimumFractionDigits", o.minFrac)
			rt.putInt(out, "maximumFractionDigits", o.maxFrac)
		}
		if o.reportSig {
			rt.putInt(out, "minimumSignificantDigits", o.minSig)
			rt.putInt(out, "maximumSignificantDigits", o.maxSig)
		}
		if o.useGrouping == "" {
			rt.putBool(out, "useGrouping", false)
		} else {
			rt.putString(out, "useGrouping", o.useGrouping)
		}
		rt.putString(out, "notation", o.notation)
		if o.notation == "compact" {
			rt.putString(out, "compactDisplay", o.compactDisplay)
		}
		rt.putString(out, "signDisplay", o.signDisplay)
		rt.putInt(out, "roundingIncrement", o.roundingIncrement)
		rt.putString(out, "roundingMode", o.roundingMode)
		rt.putString(out, "roundingPriority", o.roundingPriority)
		rt.putString(out, "trailingZeroDisplay", o.trailingZero)
		return Obj(out), nil
	})
}

// newNumberFormat reads the arguments a NumberFormat is made with.
func (r *Runtime) newNumberFormat(args []Value) (*Object, error) {
	o, err := r.numberOptionsFrom(args)
	if err != nil {
		return nil, err
	}
	proto, err := r.protoFromNewTargetErr(r.intlProtoOf("NumberFormat"))
	if err != nil {
		return nil, err
	}
	out := newObject(proto, ClassObject)
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
	// The options are read in the order the standard reads them: a getter
	// among them can see which came first.
	if _, err := r.stringOption(options, "localeMatcher", "best fit",
		"lookup", "best fit"); err != nil {
		return nil, err
	}
	numbering, err := r.typeOption(options, "numberingSystem")
	if err != nil {
		return nil, err
	}
	choice := r.resolveLocale(tags, "nu")
	choice.override("nu", numbering)
	o := &numberOptions{locale: choice.data, choice: choice}
	o.digits = choice.setting("nu")

	if o.style, err = r.stringOption(options, "style", "decimal",
		"decimal", "percent", "currency", "unit"); err != nil {
		return nil, err
	}
	// A currency that was not given at all is not the same as one given as an
	// empty string: the first is missing, the second is wrong.
	currencyValue, err := r.getProp(options, r.atoms.intern("currency"), Obj(options))
	if err != nil {
		return nil, err
	}
	switch {
	case currencyValue.IsUndefined():
		if o.style == "currency" {
			return nil, r.throwTypeError("a currency style needs a currency")
		}
	default:
		text, err := r.toString(currencyValue)
		if err != nil {
			return nil, err
		}
		currency := text.Go()
		if len(currency) != 3 || !allLetters(currency) {
			return nil, r.throwRangeError("that is not a currency code: %s", currency)
		}
		o.currency = strings.ToUpper(currency)
	}
	if o.currencyDisplay, err = r.stringOption(options, "currencyDisplay", "symbol",
		"code", "symbol", "narrowSymbol", "name"); err != nil {
		return nil, err
	}
	if o.currencySign, err = r.stringOption(options, "currencySign", "standard",
		"standard", "accounting"); err != nil {
		return nil, err
	}
	unit, err := r.stringOption(options, "unit", "")
	if err != nil {
		return nil, err
	}
	switch {
	case unit == "":
		if o.style == "unit" {
			return nil, r.throwTypeError("a unit style needs a unit")
		}
	case !wellFormedUnit(unit):
		return nil, r.throwRangeError("that is not a unit: %s", unit)
	default:
		o.unit = unit
	}
	if o.unitDisplay, err = r.stringOption(options, "unitDisplay", "short",
		"short", "narrow", "long"); err != nil {
		return nil, err
	}
	if o.notation, err = r.stringOption(options, "notation", "standard",
		"standard", "scientific", "engineering", "compact"); err != nil {
		return nil, err
	}

	// How many digits, which is a knot of its own: the fraction digits and the
	// significant digits are two ways of asking the same question, and which
	// of them is in force depends on which were given.
	minFracDefault, maxFracDefault := 0, 3
	switch {
	case o.style == "currency" && o.notation == "standard":
		// A currency is written with as many decimal places as it has, unless
		// the number is being shortened, where those places are not the point.
		places := icu.CurrencyDigits(o.currency)
		minFracDefault, maxFracDefault = places, places
	case o.style == "percent":
		maxFracDefault = 0
	}
	if err := r.readDigitOptions(o, options, minFracDefault, maxFracDefault); err != nil {
		return nil, err
	}

	if o.compactDisplay, err = r.stringOption(options, "compactDisplay", "short",
		"short", "long"); err != nil {
		return nil, err
	}
	if o.useGrouping, err = r.groupingOption(options, o.notation); err != nil {
		return nil, err
	}
	if o.signDisplay, err = r.stringOption(options, "signDisplay", "auto",
		"auto", "never", "always", "exceptZero", "negative"); err != nil {
		return nil, err
	}
	return o, nil
}

// roundingIncrements are the steps a number may be rounded to: a price to the
// nearest five cents, a measurement to the nearest quarter.
var roundingIncrements = []int{1, 2, 5, 10, 20, 25, 50, 100, 200, 250, 500,
	1000, 2000, 2500, 5000}

// readDigitOptions reads how many digits to write, which is the one part of
// the options that cannot be read one at a time: what a minimum means depends
// on whether a maximum was given, and whether either is in force at all
// depends on whether significant digits were asked for.
func (r *Runtime) readDigitOptions(o *numberOptions, options *Object, minFracDefault, maxFracDefault int) error {
	var err error
	if o.minInt, _, err = r.intOption(options, "minimumIntegerDigits", 1, 21, 1); err != nil {
		return err
	}
	// These four are read now and made sense of afterwards, since the standard
	// reads them in this order whatever it does with them.
	minFrac, minFracSet, err := r.rawNumberOption(options, "minimumFractionDigits")
	if err != nil {
		return err
	}
	maxFrac, maxFracSet, err := r.rawNumberOption(options, "maximumFractionDigits")
	if err != nil {
		return err
	}
	minSig, minSigSet, err := r.rawNumberOption(options, "minimumSignificantDigits")
	if err != nil {
		return err
	}
	maxSig, maxSigSet, err := r.rawNumberOption(options, "maximumSignificantDigits")
	if err != nil {
		return err
	}
	if o.roundingIncrement, _, err = r.intOption(options, "roundingIncrement", 1, 5000, 1); err != nil {
		return err
	}
	if !containsInt(roundingIncrements, o.roundingIncrement) {
		return r.throwRangeError("%d is not a step a number may be rounded to", o.roundingIncrement)
	}
	if o.roundingMode, err = r.stringOption(options, "roundingMode", "halfExpand",
		"ceil", "floor", "expand", "trunc", "halfCeil", "halfFloor",
		"halfExpand", "halfTrunc", "halfEven"); err != nil {
		return err
	}
	if o.roundingPriority, err = r.stringOption(options, "roundingPriority", "auto",
		"auto", "morePrecision", "lessPrecision"); err != nil {
		return err
	}
	if o.trailingZero, err = r.stringOption(options, "trailingZeroDisplay", "auto",
		"auto", "stripIfInteger"); err != nil {
		return err
	}

	hasSig, hasFrac := minSigSet || maxSigSet, minFracSet || maxFracSet
	needSig, needFrac := true, true
	if o.roundingPriority == "auto" {
		needSig = hasSig
		if hasSig || (!hasFrac && o.notation == "compact") {
			needFrac = false
		}
	}
	if needSig {
		o.minSig, o.maxSig = 1, 21
		if hasSig {
			if o.minSig, err = r.boundedOption(minSig, minSigSet, "minimumSignificantDigits", 1, 21, 1); err != nil {
				return err
			}
			if o.maxSig, err = r.boundedOption(maxSig, maxSigSet, "maximumSignificantDigits", o.minSig, 21, 21); err != nil {
				return err
			}
		}
	}
	if needFrac {
		o.minFrac, o.maxFrac = minFracDefault, maxFracDefault
		if hasFrac {
			if minFracSet {
				if o.minFrac, err = r.boundedOption(minFrac, true, "minimumFractionDigits", 0, 100, 0); err != nil {
					return err
				}
			}
			if maxFracSet {
				if o.maxFrac, err = r.boundedOption(maxFrac, true, "maximumFractionDigits", 0, 100, 0); err != nil {
					return err
				}
			}
			switch {
			case !minFracSet:
				o.minFrac = min(minFracDefault, o.maxFrac)
			case !maxFracSet:
				o.maxFrac = max(maxFracDefault, o.minFrac)
			case o.minFrac > o.maxFrac:
				return r.throwRangeError("the fraction digits are the wrong way round")
			}
		}
	}

	switch {
	case !needSig && !needFrac:
		// A compact number with nothing asked of it is written to two
		// significant digits, whichever of the two ways of counting is asked
		// for afterwards.
		o.rounding = "morePrecision"
		o.minFrac, o.maxFrac, o.minSig, o.maxSig = 0, 0, 1, 2
	case o.roundingPriority != "auto":
		o.rounding = o.roundingPriority
	case hasSig:
		o.rounding = "significant"
	default:
		o.rounding = "fraction"
	}
	if o.roundingIncrement != 1 {
		if o.rounding != "fraction" {
			return r.throwTypeError("a rounding step goes with fraction digits, not with significant ones")
		}
		if o.maxFrac != o.minFrac {
			return r.throwRangeError("a rounding step needs the fraction digits fixed")
		}
	}
	o.reportSig = needSig || !needFrac
	o.reportFrac = needFrac || !needSig
	return nil
}

// groupingOption reads useGrouping, which takes a word as well as a flag: an
// empty string and false mean no grouping, true means always, and a number may
// ask for grouping only once there are two digits in front of the first group.
func (r *Runtime) groupingOption(o *Object, notation string) (string, error) {
	fallback := "auto"
	if notation == "compact" {
		fallback = "min2"
	}
	v, err := r.getProp(o, r.atoms.intern("useGrouping"), Obj(o))
	if err != nil {
		return "", err
	}
	switch {
	case v.IsUndefined():
		return fallback, nil
	case v.IsBool() && v.Truthy():
		return "always", nil
	case !v.Truthy():
		// Anything that reads as false -- the flag, an empty string, nothing
		// at all -- asks for no grouping.
		return "", nil
	}
	s, err := r.toString(v)
	if err != nil {
		return "", err
	}
	switch got := s.Go(); got {
	case "min2", "auto", "always":
		return got, nil
	case "true", "false":
		// The words, as against the flags, say nothing either way, and the
		// number is grouped the way it would have been anyway.
		return fallback, nil
	default:
		return "", r.throwRangeError("%s is not a value useGrouping may take", got)
	}
}

func containsInt(list []int, n int) bool {
	for _, item := range list {
		if item == n {
			return true
		}
	}
	return false
}

// wellFormedUnit reports whether a unit is one this may be asked for: one of
// the sanctioned ones, or one of them divided by another.
func wellFormedUnit(unit string) bool {
	numerator, denominator, divided := strings.Cut(unit, "-per-")
	if !icu.HasUnit(numerator) {
		return false
	}
	return !divided || icu.HasUnit(denominator)
}

func (r *Runtime) numberFormatOf(this Value) (*numberOptions, error) {
	if o := r.unwrapFormatter(this).Object(); o != nil {
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
		made, err := rt.protoFromNewTargetErr(rt.intlProtoOf("DateTimeFormat"))
		if err != nil {
			return Undefined, err
		}
		o, err := rt.dateOptionsFrom(args, nil, "any")
		if err != nil {
			return Undefined, err
		}
		out := newObject(made, ClassObject)
		out.data = o
		return rt.legacyFormatter(this, out), nil
	})
	r.defValue(intl, "DateTimeFormat", Obj(ctor))
	r.intlProtos["DateTimeFormat"] = proto
	r.defToStringTag(proto, "Intl.DateTimeFormat")
	r.defSupportedLocalesOf(ctor)

	// format is a getter for a function bound to this formatter, for the same
	// reason the number one is.
	r.defGetter(proto, "format", func(rt *Runtime, this Value, args []Value) (Value, error) {
		o, err := rt.dateFormatOf(this)
		if err != nil {
			return Undefined, err
		}
		return rt.bound(&o.formatFn, 1, func(rt *Runtime, _ Value, args []Value) (Value, error) {
			value := arg(args, 0)
			format, err := rt.dateOptionsForArgument(o, value)
			if err != nil {
				return Undefined, err
			}
			t, err := rt.dateArgument(format, value)
			if err != nil {
				return Undefined, err
			}
			return Str(NewString(format.format(t))), nil
		}), nil
	})
	r.defMethod(proto, "formatRange", 2, func(rt *Runtime, this Value, args []Value) (Value, error) {
		pieces, err := rt.dateRange(this, args)
		if err != nil {
			return Undefined, err
		}
		return Str(NewString(pieces.text())), nil
	})
	r.defMethod(proto, "formatRangeToParts", 2, func(rt *Runtime, this Value, args []Value) (Value, error) {
		pieces, err := rt.dateRange(this, args)
		if err != nil {
			return Undefined, err
		}
		return Obj(rt.rangeParts(pieces)), nil
	})
	r.defMethod(proto, "formatToParts", 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
		o, err := rt.dateFormatOf(this)
		if err != nil {
			return Undefined, err
		}
		value := arg(args, 0)
		format, err := rt.dateOptionsForArgument(o, value)
		if err != nil {
			return Undefined, err
		}
		t, err := rt.dateArgument(format, value)
		if err != nil {
			return Undefined, err
		}
		pieces := format.parts(t)
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
		// In the order the standard lists them, which a script can see.
		out := newObject(rt.proto.object, ClassObject)
		rt.putString(out, "locale", o.choice.locale())
		rt.putString(out, "calendar", o.calendar)
		rt.putString(out, "numberingSystem", o.digits)
		rt.putString(out, "timeZone", o.timeZone)
		if o.hour != "" || o.timeStyle != "" {
			rt.putString(out, "hourCycle", o.hourCycle)
			rt.putBool(out, "hour12", o.hour12)
		}
		for _, field := range []struct{ name, value string }{
			{"weekday", o.weekday}, {"era", o.era}, {"year", o.year},
			{"month", o.month}, {"day", o.day}, {"dayPeriod", o.dayPeriod},
			{"hour", o.hour}, {"minute", o.minute}, {"second", o.second},
		} {
			if field.value != "" {
				rt.putString(out, field.name, field.value)
			}
		}
		if o.fractional > 0 {
			rt.putInt(out, "fractionalSecondDigits", o.fractional)
		}
		for _, field := range []struct{ name, value string }{
			{"timeZoneName", o.timeZoneName}, {"dateStyle", o.dateStyle},
			{"timeStyle", o.timeStyle},
		} {
			if field.value != "" {
				rt.putString(out, field.name, field.value)
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
	if v.IsObject() {
		switch value := v.Object().data.(type) {
		case *temporalInstant:
			if value == nil {
				break
			}
			zone := o.zone
			if zone == nil {
				zone = time.UTC
			}
			return time.Unix(value.epochSeconds, int64(value.nanosecond)).In(zone), nil
		case *temporalPlainDateTime:
			if value != nil {
				if value.calendar != "iso8601" && value.calendar != o.calendar {
					return time.Time{}, r.throwRangeError("Temporal calendar does not match the formatter calendar")
				}
				return time.Date(value.year, time.Month(value.month), value.day, value.hour, value.minute, value.second,
					value.millisecond*1_000_000+value.microsecond*1_000+value.nanosecond, time.UTC), nil
			}
		case *temporalPlainDate:
			if value != nil {
				if value.calendar != "iso8601" && value.calendar != o.calendar {
					return time.Time{}, r.throwRangeError("Temporal calendar does not match the formatter calendar")
				}
				return time.Date(value.year, time.Month(value.month), value.day, 0, 0, 0, 0, time.UTC), nil
			}
		case *temporalPlainTime:
			if value != nil {
				return time.Date(1970, time.January, 1, value.hour, value.minute, value.second,
					value.millisecond*1_000_000+value.microsecond*1_000+value.nanosecond, time.UTC), nil
			}
		case *temporalPlainYearMonth:
			if value != nil {
				if value.calendar != o.calendar {
					return time.Time{}, r.throwRangeError("Temporal calendar does not match the formatter calendar")
				}
				return time.Date(value.year, time.Month(value.month), value.day, 0, 0, 0, 0, time.UTC), nil
			}
		case *temporalPlainMonthDay:
			if value != nil {
				if value.calendar != o.calendar {
					return time.Time{}, r.throwRangeError("Temporal calendar does not match the formatter calendar")
				}
				return time.Date(value.year, time.Month(value.month), value.day, 0, 0, 0, 0, time.UTC), nil
			}
		case *temporalZonedDateTime:
			return time.Time{}, r.throwTypeError("Intl.DateTimeFormat cannot format a Temporal.ZonedDateTime")
		}
	}
	n, err := r.toNumber(v)
	if err != nil {
		return time.Time{}, err
	}
	if math.IsNaN(n) || math.Abs(n) > 8.64e15 {
		return time.Time{}, r.throwRangeError("that is not a date this can format")
	}
	// An instant is a whole number of milliseconds, counted towards the epoch
	// rather than away from it: a fraction of a millisecond is dropped.
	return o.at(math.Trunc(n)), nil
}

// toDateTimeFormattable performs the observable conversion done before range
// formatting compares argument kinds. Temporal values keep their internal
// slots; every other value is converted to a number first.
func (r *Runtime) toDateTimeFormattable(v Value) (Value, error) {
	if temporalDateTimeKind(v) != "" {
		return v, nil
	}
	n, err := r.toNumber(v)
	if err != nil {
		return Undefined, err
	}
	return Float(n), nil
}

func temporalDateTimeKind(v Value) string {
	if !v.IsObject() {
		return ""
	}
	switch v.Object().data.(type) {
	case *temporalInstant:
		return "instant"
	case *temporalPlainDateTime:
		return "date-time"
	case *temporalPlainDate:
		return "date"
	case *temporalPlainTime:
		return "time"
	case *temporalPlainYearMonth:
		return "year-month"
	case *temporalPlainMonthDay:
		return "month-day"
	case *temporalZonedDateTime:
		return "zoned-date-time"
	}
	return ""
}

// dateOptionsFrom reads the arguments a DateTimeFormat is made with. defaults
// fills in the fields the toLocale methods ask for when the caller named none.
func (r *Runtime) dateOptionsFrom(args []Value, defaults map[string]string, required string) (*dateOptions, error) {
	tags, err := r.requestedLocales(arg(args, 0))
	if err != nil {
		return nil, err
	}
	options, err := r.optionsObject(arg(args, 1))
	if err != nil {
		return nil, err
	}
	// The options are read in the order the standard reads them, since a
	// getter among them can see which came first.
	if _, err := r.stringOption(options, "localeMatcher", "best fit",
		"lookup", "best fit"); err != nil {
		return nil, err
	}
	calendar, err := r.typeOption(options, "calendar")
	if err != nil {
		return nil, err
	}
	numbering, err := r.typeOption(options, "numberingSystem")
	if err != nil {
		return nil, err
	}
	hour12, hour12Set, err := r.boolOption(options, "hour12")
	if err != nil {
		return nil, err
	}
	cycle, err := r.stringOption(options, "hourCycle", "", "h11", "h12", "h23", "h24")
	if err != nil {
		return nil, err
	}
	if hour12Set {
		// A clock asked for outright says all there is to say, and the cycle
		// is not consulted.
		cycle = ""
	}

	choice := r.resolveLocale(tags, "ca", "nu", "hc")
	choice.override("ca", calendar)
	choice.override("nu", numbering)
	choice.override("hc", cycle)
	if hour12Set {
		// A clock asked for outright answers the question the tag asked, so
		// the tag is no longer the reason for the answer.
		choice.drop("hc")
	}
	o := &dateOptions{locale: choice.data, choice: choice, timeZone: "UTC"}
	o.locale.PrepareDate()
	o.calendar = choice.setting("ca")
	// These are abstract/deprecated calendar requests. ECMA-402 requires the
	// formatter to settle on a concrete member of AvailableCalendars.
	if o.calendar == "islamic" || o.calendar == "islamic-rgsa" {
		o.calendar = "islamic-civil"
	}
	o.digits = choice.setting("nu")

	// An empty string is a zone that does not exist, which is not the same as
	// no zone at all.
	zoneValue, err := r.getProp(options, r.atoms.intern("timeZone"), Obj(options))
	if err != nil {
		return nil, err
	}
	zone, given := "", !zoneValue.IsUndefined()
	o.timeZoneSet = given
	if given {
		text, err := r.toString(zoneValue)
		if err != nil {
			return nil, err
		}
		zone = text.Go()
	}
	if err := o.setZone(r, zone, given); err != nil {
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
	if o.dayPeriod, err = read("dayPeriod", "narrow", "short", "long"); err != nil {
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
	fractional, fractionalSet, err := r.intOption(options, "fractionalSecondDigits", 1, 3, 0)
	if err != nil {
		return nil, err
	}
	if fractionalSet {
		o.fractional = fractional
	}
	if o.timeZoneName, err = read("timeZoneName", "short", "long", "shortOffset",
		"longOffset", "shortGeneric", "longGeneric"); err != nil {
		return nil, err
	}
	o.timeZoneNameSet = o.timeZoneName != ""
	if _, err := r.stringOption(options, "formatMatcher", "best fit",
		"basic", "best fit"); err != nil {
		return nil, err
	}
	if o.dateStyle, err = r.stringOption(options, "dateStyle", "",
		"full", "long", "medium", "short"); err != nil {
		return nil, err
	}
	if o.timeStyle, err = r.stringOption(options, "timeStyle", "",
		"full", "long", "medium", "short"); err != nil {
		return nil, err
	}

	if (o.dateStyle != "" || o.timeStyle != "") && o.hasFields() {
		return nil, r.throwTypeError("a style and a field cannot both be asked for")
	}
	// A method that writes a date will not be asked for a style of time, and
	// one that writes a time will not be asked for a style of date.
	switch {
	case required == "date" && o.timeStyle != "":
		return nil, r.throwTypeError("a date cannot be written in a style of time")
	case required == "time" && o.dateStyle != "":
		return nil, r.throwTypeError("a time cannot be written in a style of date")
	}

	// What was not asked for is filled in, but only where the caller named
	// nothing of the kind it is asking about: a date asked for with an hour
	// gets the date fields as well as the hour.
	needed := o.dateStyle == "" && o.timeStyle == ""
	if needed && (required == "date" || required == "any") {
		needed = o.weekday == "" && o.year == "" && o.month == "" && o.day == ""
	}
	if needed && (required == "time" || required == "any") {
		needed = o.dayPeriod == "" && o.hour == "" && o.minute == "" &&
			o.second == "" && o.fractional == 0
	}
	if needed {
		o.implicitDefaults = true
		if defaults == nil {
			defaults = map[string]string{"year": "numeric", "month": "numeric", "day": "numeric"}
		}
		if o.weekday == "" {
			o.weekday = defaults["weekday"]
		}
		if o.era == "" {
			o.era = defaults["era"]
		}
		for field, into := range map[string]*string{
			"year": &o.year, "month": &o.month, "day": &o.day,
			"hour": &o.hour, "minute": &o.minute, "second": &o.second,
			"timeZoneName": &o.timeZoneName,
		} {
			if *into == "" {
				*into = defaults[field]
			}
		}
	}

	// Which clock to keep: the one asked for outright, the one the tag asked
	// for, or the one the language keeps.
	switch {
	case hour12Set && hour12:
		o.hourCycle, o.hourSet = o.locale.HourCycle12, true
	case hour12Set:
		o.hourCycle, o.hourSet = o.locale.HourCycle24, true
	case choice.setting("hc") != "":
		o.hourCycle, o.hourSet = choice.setting("hc"), cycle != ""
	default:
		o.hourCycle = o.locale.HourCycle
	}
	o.hour12 = o.hourCycle == "h11" || o.hourCycle == "h12"

	o.pattern = o.patternFor()
	// A twelve-hour clock asked for where the locale writes a
	// twenty-four-hour one, or the other way round, changes the pattern.
	o.pattern = adjustClock(o.pattern, o.hourCycle)
	return o, nil
}

// adjustClock writes the hour on the clock that was asked for, and puts the
// day period there or takes it away to match.
func adjustClock(pattern, cycle string) string {
	// A pattern with no hour in it has no clock to adjust, and its day period
	// -- if it was asked for on its own -- is not the hour's to take away.
	if !strings.ContainsAny(patternLettersOf(pattern), "hHkK") {
		return pattern
	}
	hour12 := cycle == "h11" || cycle == "h12"
	hourLetter := byte('H')
	switch cycle {
	case "h11":
		hourLetter = 'K'
	case "h12":
		hourLetter = 'h'
	case "h24":
		hourLetter = 'k'
	}
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
		case strings.ContainsRune("hHkK", rune(c)):
			b.WriteByte(hourLetter)
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
	if hour12 && strings.ContainsRune(letters, rune(hourLetter)) &&
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
	if name != "" && name != "Local" {
		return name
	}
	// A zone loaded from the machine rather than by name: what the operating
	// system says it is, which is a name in the database if it can be had.
	if zone, ok := machineZoneName(); ok {
		return zone
	}
	// Failing that, the offset is the best name there is, written the way an
	// offset is written.
	t := time.Now().In(r.location())
	_, offset := t.Zone()
	if offset == 0 {
		return "UTC"
	}
	minutes := offset / 60
	sign := "+"
	if minutes < 0 {
		sign, minutes = "-", -minutes
	}
	return fmt.Sprintf("%s%02d:%02d", sign, minutes/60, minutes%60)
}

// machineZoneName is the name this machine's own zone goes by in the database:
// what TZ says, or what the file the system points at is called.
func machineZoneName() (string, bool) {
	if tz := os.Getenv("TZ"); tz != "" {
		if zone, ok := icu.CanonicalZone(strings.TrimPrefix(tz, ":")); ok {
			return zone, true
		}
	}
	path, err := os.Readlink("/etc/localtime")
	if err != nil {
		return "", false
	}
	// The link points into the zone files: .../zoneinfo/Asia/Shanghai.
	if at := strings.Index(path, "zoneinfo/"); at >= 0 {
		if zone, ok := icu.CanonicalZone(path[at+len("zoneinfo/"):]); ok {
			return zone, true
		}
	}
	return "", false
}

func (r *Runtime) dateFormatOf(this Value) (*dateOptions, error) {
	if o := r.unwrapFormatter(this).Object(); o != nil {
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
	r.defSupportedLocalesOfWhere(ctor, icu.Has)
}

func (r *Runtime) defSupportedLocalesOfWhere(ctor *Object, supported func(string) bool) {
	r.defMethod(ctor, "supportedLocalesOf", 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
		tags, err := rt.requestedLocales(arg(args, 0))
		if err != nil {
			return Undefined, err
		}
		// The options are read even though the only one that could matter is
		// which matcher to use, since a bad value has to be refused.
		options, err := rt.optionsObject(arg(args, 1))
		if err != nil {
			return Undefined, err
		}
		if _, err := rt.stringOption(options, "localeMatcher", "best fit",
			"lookup", "best fit"); err != nil {
			return Undefined, err
		}
		var out []Value
		for _, tag := range tags {
			t, ok := parseTag(tag)
			if ok && supported(t.base()) {
				out = append(out, Str(NewString(tag)))
			}
		}
		return Obj(rt.newArrayFrom(out)), nil
	})
}

// numberRange writes one number against another.
func (r *Runtime) numberRange(this Value, args []Value) (*rangePieces, error) {
	o, err := r.numberFormatOf(this)
	if err != nil {
		return nil, err
	}
	if arg(args, 0).IsUndefined() || arg(args, 1).IsUndefined() {
		return nil, r.throwTypeError("a range has two ends")
	}
	from, fromSpecial, err := r.numberArgument(arg(args, 0))
	if err != nil {
		return nil, err
	}
	to, toSpecial, err := r.numberArgument(arg(args, 1))
	if err != nil {
		return nil, err
	}
	if fromSpecial == "nan" || toSpecial == "nan" {
		return nil, r.throwRangeError("a range does not have a NaN at either end")
	}
	start := numberPiecesOf(o.valueParts(from, fromSpecial))
	end := numberPiecesOf(o.valueParts(to, toSpecial))
	if piecesEqual(start, end) {
		return sameRange(start, o.locale.Approximately), nil
	}
	separator := o.locale.Range
	// A number with something written around it shares matching affixes where
	// the locale's automatic range collapse calls for it. Otherwise it is
	// written out twice, with the mark set apart from both ends.
	if o.style == "currency" || o.style == "percent" {
		if merged, ok := mergeRangeAffixes(start, end, separator); ok {
			return merged, nil
		}
		return joinRange(start, end, spacedOut(separator)), nil
	}
	// A measurement is written once and the two counts against it: 1–5 m.
	if o.style == "unit" {
		return mergeRange(start, end, separator), nil
	}
	return joinRange(start, end, separator), nil
}

// dateRange writes one date against another.
func (r *Runtime) dateRange(this Value, args []Value) (*rangePieces, error) {
	o, err := r.dateFormatOf(this)
	if err != nil {
		return nil, err
	}
	if arg(args, 0).IsUndefined() || arg(args, 1).IsUndefined() {
		return nil, r.throwTypeError("a range has two ends")
	}
	fromValue, err := r.toDateTimeFormattable(arg(args, 0))
	if err != nil {
		return nil, err
	}
	toValue, err := r.toDateTimeFormattable(arg(args, 1))
	if err != nil {
		return nil, err
	}
	fromKind, toKind := temporalDateTimeKind(fromValue), temporalDateTimeKind(toValue)
	if (fromKind != "" || toKind != "") && fromKind != toKind {
		return nil, r.throwTypeError("a date-time range must have matching argument types")
	}
	format, err := r.dateOptionsForArgument(o, fromValue)
	if err != nil {
		return nil, err
	}
	from, err := r.dateArgument(format, fromValue)
	if err != nil {
		return nil, err
	}
	to, err := r.dateArgument(format, toValue)
	if err != nil {
		return nil, err
	}
	start := datePiecesOf(format.parts(from))
	end := datePiecesOf(format.parts(to))
	if piecesEqual(start, end) {
		// Two dates that come to the same thing are written once, and without
		// the mark a number takes.
		return sameRange(start, ""), nil
	}
	if fromKind == "date-time" || fromKind == "instant" {
		if joined, ok := joinDateTimeRange(start, end, format.locale.DateRange); ok {
			return joined, nil
		}
	}
	// A date written in numbers is written out twice in some languages and
	// once in others, with what the two have in common said once.
	if format.locale.DateRangeRepeat && numericDate(format.pattern) {
		return joinRange(start, end, spacedOut(format.locale.DateRange)), nil
	}
	return mergeRange(start, end, format.locale.DateRange), nil
}

func piecesEqual(a, b []pieceOf) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// rangeParts is a range in the shape formatRangeToParts hands back, where each
// piece says which end it came from.
func (r *Runtime) rangeParts(pieces *rangePieces) *Object {
	out := make([]Value, len(pieces.kinds))
	for i := range pieces.kinds {
		o := r.partObject(pieces.kinds[i], pieces.values[i])
		r.putString(o, "source", pieces.sources[i])
		out[i] = Obj(o)
	}
	return r.newArrayFrom(out)
}

// numericDate reports whether a pattern writes a date in numbers, which is
// the kind of range some languages write out twice.
func numericDate(pattern string) bool {
	letters := patternLettersOf(pattern)
	if strings.Contains(letters, "MMM") || strings.Contains(letters, "LLL") {
		return false
	}
	return strings.ContainsAny(letters, "yMdL")
}

// spacedOut is a mark with room around it, for the ranges that are written out
// in full and would otherwise run together.
func spacedOut(separator string) string {
	if strings.TrimLeft(separator, " \u00a0\u202f\u2009") == separator {
		separator = " " + separator
	}
	if strings.TrimRight(separator, " \u00a0\u202f\u2009") == separator {
		separator += " "
	}
	return separator
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
