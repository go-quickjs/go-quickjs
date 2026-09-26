package vm

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	intl "github.com/go-quickjs/go-intl"
	"github.com/go-quickjs/go-intl/temporal"
)

// Intl: ECMA-402's objects over go-intl's services, which carry CLDR's data
// for every locale ICU has. The objects read their arguments and options in
// the order ECMA-402 gives, with V8's errors, and hand the formatting to
// go-intl.

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
	intlObj := newObject(r.proto.object, ClassObject)
	r.defToStringTag(intlObj, "Intl")

	r.initLocale(intlObj)
	r.initNumberFormat(intlObj)
	r.initDateTimeFormat(intlObj)
	r.initCollator(intlObj)
	r.initPluralRules(intlObj)
	r.initListFormat(intlObj)
	r.initDisplayNames(intlObj)
	r.initRelativeTimeFormat(intlObj)
	r.initSegmenter(intlObj)
	r.initDurationFormat(intlObj)

	r.defMethod(intlObj, "getCanonicalLocales", 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
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

	r.defMethod(intlObj, "supportedValuesOf", 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
		key, err := rt.toString(arg(args, 0))
		if err != nil {
			return Undefined, err
		}
		// Each list is what the service that takes the values honours, from
		// go-intl, where it comes from.
		var values []string
		err = nil
		switch key.Go() {
		case "calendar":
			values = intl.Calendars(rt.intlCompat())
		case "collation":
			values, err = intl.Collations()
		case "currency":
			values, err = intl.Currencies()
		case "numberingSystem":
			values, err = intl.NumberingSystems()
		case "timeZone":
			values, err = intl.TimeZones(rt.intlCompat())
		case "unit":
			values = intl.SanctionedUnits()
		default:
			return Undefined, rt.intlInvalidRange("key", key.Go())
		}
		if err != nil {
			return Undefined, rt.intlInternal()
		}
		out := make([]Value, len(values))
		for i, v := range values {
			out[i] = Str(NewString(v))
		}
		return Obj(rt.newArrayFrom(out)), nil
	})
	return intlObj
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
			return []string{locale.loc.String()}, nil
		}
	}
	length, err := r.lengthOf(o)
	if err != nil {
		return nil, err
	}
	canon, err := r.canonicalizer()
	if err != nil {
		return nil, r.intlInternal()
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
			return nil, r.throwTypeError("Language ID should be string or object.")
		}
		if locale, ok := r.localeData(item); ok {
			canonical := locale.loc.String()
			if !contains(out, canonical) {
				out = append(out, canonical)
			}
			continue
		}
		s, err := r.toString(item)
		if err != nil {
			return nil, err
		}
		loc, err := canon.Canonicalize(s.Go())
		if err != nil {
			return nil, r.intlInvalidLanguageTag(s.Go())
		}
		canonical := loc.String()
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
			if r.intlService == "Intl.Locale" {
				return "", r.intlIncorrectLocale()
			}
			return "", r.intlInvalidRange(name, got)
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
		return nil, r.intlInvalidArgumentType()
	}
	return o, nil
}

// optionsObject turns the options argument into something to read from.
func (r *Runtime) optionsObject(v Value) (*Object, error) {
	if v.IsUndefined() {
		return newObject(nil, ClassObject), nil
	}
	if v.IsNull() && r.intlService != "" {
		return nil, r.intlNullOptions()
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
	if r.intlService != "" {
		return "", r.intlValueOutOfRange(got, name)
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
		if r.intlService != "" {
			return 0, r.intlPropertyOutOfRange(name)
		}
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
		if r.intlService != "" {
			return 0, false, r.intlPropertyOutOfRange(name)
		}
		return 0, false, r.throwRangeError("%s is out of range", name)
	}
	return int(n), true, nil
}

// --- NumberFormat -----------------------------------------------------------

func (r *Runtime) initNumberFormat(intlObj *Object) {
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
	r.defValue(intlObj, "NumberFormat", Obj(ctor))
	r.intlProtos["NumberFormat"] = proto
	r.defToStringTag(proto, "Intl.NumberFormat")
	r.defSupportedLocalesOfService(ctor, intl.ServiceNumberFormat)

	// format is a getter for a function bound to this formatter, because it is
	// nearly always handed straight to a map.
	r.defGetter(proto, "format", func(rt *Runtime, this Value, args []Value) (Value, error) {
		o, err := rt.numberFormatOf(this, "get Intl.NumberFormat.prototype.format")
		if err != nil {
			return Undefined, err
		}
		return rt.bound(&o.formatFn, 1, func(rt *Runtime, _ Value, args []Value) (Value, error) {
			d, special, err := rt.numberArgument(arg(args, 0))
			if err != nil {
				return Undefined, err
			}
			return Str(NewString(o.nf.FormatDecimal(intlDecimal(d, special)))), nil
		}), nil
	})
	r.defMethod(proto, "formatRange", 2, func(rt *Runtime, this Value, args []Value) (Value, error) {
		pieces, err := rt.numberRange(this, args, "Intl.NumberFormat.prototype.formatRange")
		if err != nil {
			return Undefined, err
		}
		return Str(NewString(pieces.text())), nil
	})
	r.defMethod(proto, "formatRangeToParts", 2, func(rt *Runtime, this Value, args []Value) (Value, error) {
		pieces, err := rt.numberRange(this, args, "Intl.NumberFormat.prototype.formatRangeToParts")
		if err != nil {
			return Undefined, err
		}
		return Obj(rt.rangeParts(pieces)), nil
	})
	r.defMethod(proto, "formatToParts", 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
		o, err := rt.numberFormatOf(this, "Intl.NumberFormat.prototype.formatToParts")
		if err != nil {
			return Undefined, err
		}
		d, special, err := rt.numberArgument(arg(args, 0))
		if err != nil {
			return Undefined, err
		}
		parts := o.nf.FormatDecimalToParts(intlDecimal(d, special))
		out := make([]Value, len(parts))
		for i, p := range parts {
			out[i] = Obj(rt.partObject(string(p.Kind), p.Value))
		}
		return Obj(rt.newArrayFrom(out)), nil
	})
	r.defMethod(proto, "resolvedOptions", 0, func(rt *Runtime, this Value, args []Value) (Value, error) {
		o, err := rt.numberFormatOf(this, "UnwrapNumberFormat")
		if err != nil {
			return Undefined, err
		}
		// What go-intl settled on, in the order ECMA-402's table gives.
		resolved := o.nf.ResolvedOptions()
		out := newObject(rt.proto.object, ClassObject)
		rt.putString(out, "locale", resolved.Locale)
		rt.putString(out, "numberingSystem", resolved.NumberingSystem)
		rt.putString(out, "style", resolved.Style.String())
		switch resolved.Style {
		case intl.StyleCurrency:
			rt.putString(out, "currency", resolved.Currency)
			rt.putString(out, "currencyDisplay", resolved.CurrencyDisplay.String())
			rt.putString(out, "currencySign", resolved.CurrencySign.String())
		case intl.StyleUnit:
			rt.putString(out, "unit", resolved.Unit)
			rt.putString(out, "unitDisplay", resolved.UnitDisplay.String())
		}
		rt.putDigits(out, resolved.ResolvedDigits)
		if resolved.UseGrouping == intl.GroupingNever {
			rt.putBool(out, "useGrouping", false)
		} else {
			rt.putString(out, "useGrouping", resolved.UseGrouping.String())
		}
		rt.putString(out, "notation", resolved.Notation.String())
		if resolved.Notation == intl.NotationCompact {
			rt.putString(out, "compactDisplay", resolved.CompactDisplay.String())
		}
		rt.putString(out, "signDisplay", resolved.SignDisplay.String())
		rt.putRounding(out, resolved.ResolvedDigits)
		return Obj(out), nil
	})
}

func (r *Runtime) numberOptionsFrom(args []Value) (*numberOptions, error) {
	defer r.enterIntl("Intl.NumberFormat")()
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
	matcher, err := r.stringOption(options, "localeMatcher", "best fit", "lookup", "best fit")
	if err != nil {
		return nil, err
	}
	numbering, err := r.typeOption(options, "numberingSystem")
	if err != nil {
		return nil, err
	}
	loc, err := r.intlLocale(intl.ServiceNumberFormat, tags, matcher)
	if err != nil {
		return nil, err
	}
	o := &numberOptions{}

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
			return nil, r.throwTypeError("Currency code is required with currency style.")
		}
	default:
		text, err := r.toString(currencyValue)
		if err != nil {
			return nil, err
		}
		currency := text.Go()
		if len(currency) != 3 || !allLetters(currency) {
			return nil, r.intlInvalidRange("currency code", currency)
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
			return nil, r.throwTypeError("Invalid unit argument for Intl.NumberFormat() ''")
		}
	case !wellFormedUnit(unit):
		return nil, r.throwRangeError("Invalid unit argument for Intl.NumberFormat() '%s'", unit)
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
		places, err := intl.CurrencyDigits(intl.Embedded, o.currency)
		if err != nil {
			return nil, r.intlInternal()
		}
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
	if o.nf, err = intl.NewNumberFormat(loc, o.intlOptions(numbering, r.intlCompat())); err != nil {
		return nil, r.intlInternal()
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
		return r.intlPropertyOutOfRange("roundingIncrement")
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
				return r.intlPropertyOutOfRange("maximumFractionDigits")
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
			return r.throwTypeError("RoundingType is not fractionDigits")
		}
		if o.maxFrac != o.minFrac {
			return r.intlPropertyOutOfRange("maximumFractionDigits")
		}
	}
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
		return "", r.intlValueOutOfRange(got, "useGrouping")
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
	if !intl.HasUnit(numerator) {
		return false
	}
	return !divided || intl.HasUnit(denominator)
}

func (r *Runtime) numberFormatOf(this Value, method string) (*numberOptions, error) {
	if o := r.unwrapFormatter(this).Object(); o != nil {
		if opts, ok := o.data.(*numberOptions); ok {
			return opts, nil
		}
	}
	// V8 unwraps an object as a legacy formatter first, and reports one that
	// is not a NumberFormat as UnwrapNumberFormat's, with no receiver.
	if this.IsObject() && (method == "UnwrapNumberFormat" || method == "get Intl.NumberFormat.prototype.format") {
		return nil, r.intlIncompatibleReceiver("UnwrapNumberFormat", Undefined)
	}
	return nil, r.intlIncompatibleReceiver(method, this)
}

// --- DateTimeFormat ---------------------------------------------------------

func (r *Runtime) initDateTimeFormat(namespace *Object) {
	proto := newObject(r.proto.object, ClassObject)
	ctor := r.newCtor("DateTimeFormat", 0, proto, func(rt *Runtime, this Value, args []Value) (Value, error) {
		made, err := rt.protoFromNewTargetErr(rt.intlProtoOf("DateTimeFormat"))
		if err != nil {
			return Undefined, err
		}
		d, err := rt.newDateTimeFormat(args, intl.ComponentsAny, intl.ComponentsDate, "")
		if err != nil {
			return Undefined, err
		}
		out := newObject(made, ClassObject)
		out.data = d
		return rt.legacyFormatter(this, out), nil
	})
	r.defValue(namespace, "DateTimeFormat", Obj(ctor))
	r.intlProtos["DateTimeFormat"] = proto
	r.defToStringTag(proto, "Intl.DateTimeFormat")
	r.defSupportedLocalesOfService(ctor, intl.ServiceDateTimeFormat)

	// format is a getter for a function bound to this formatter, for the same
	// reason the number one is.
	r.defGetter(proto, "format", func(rt *Runtime, this Value, args []Value) (Value, error) {
		d, err := rt.dateTimeFormatOf(this, "get Intl.DateTimeFormat.prototype.format")
		if err != nil {
			return Undefined, err
		}
		return rt.bound(&d.formatFn, 1, func(rt *Runtime, _ Value, args []Value) (Value, error) {
			text, err := rt.formatDateTime(d, arg(args, 0))
			if err != nil {
				return Undefined, err
			}
			return Str(NewString(text)), nil
		}), nil
	})
	r.defMethod(proto, "formatRange", 2, func(rt *Runtime, this Value, args []Value) (Value, error) {
		pieces, err := rt.dateTimeRange(this, args, "Intl.DateTimeFormat.prototype.formatRange")
		if err != nil {
			return Undefined, err
		}
		return Str(NewString(pieces.text())), nil
	})
	r.defMethod(proto, "formatRangeToParts", 2, func(rt *Runtime, this Value, args []Value) (Value, error) {
		pieces, err := rt.dateTimeRange(this, args, "Intl.DateTimeFormat.prototype.formatRangeToParts")
		if err != nil {
			return Undefined, err
		}
		return Obj(rt.rangeParts(pieces)), nil
	})
	r.defMethod(proto, "formatToParts", 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
		d, err := rt.dateTimeFormatOf(this, "Intl.DateTimeFormat.prototype.formatToParts")
		if err != nil {
			return Undefined, err
		}
		f, t, err := rt.forValue(d, arg(args, 0))
		if err != nil {
			return Undefined, err
		}
		parts := f.FormatToParts(t)
		out := make([]Value, len(parts))
		for i, part := range parts {
			out[i] = Obj(rt.partObject(string(part.Kind), part.Value))
		}
		return Obj(rt.newArrayFrom(out)), nil
	})
	r.defMethod(proto, "resolvedOptions", 0, func(rt *Runtime, this Value, args []Value) (Value, error) {
		d, err := rt.dateTimeFormatOf(this, "UnwrapDateTimeFormat")
		if err != nil {
			return Undefined, err
		}
		return Obj(rt.resolvedDateTimeOptions(d)), nil
	})
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
	case temporal.Instant:
		return "instant"
	case temporal.PlainDateTime:
		return "date-time"
	case temporal.PlainDate:
		return "date"
	case temporal.PlainTime:
		return "time"
	case temporal.PlainYearMonth:
		return "year-month"
	case temporal.PlainMonthDay:
		return "month-day"
	case temporal.ZonedDateTime:
		return "zoned-date-time"
	}
	return ""
}

// localZoneName is the name of the zone local time is in, which
// DateTimeFormat and Temporal.Now take where they are given none: the name
// the zone was set by, as ICU spells it, or its canonical name, whichever a
// DateTimeFormat takes first -- a custom zone, "GMT+05:30", is "+05:30" --
// or else its offset now, which for Etc/Unknown, where Node reports that,
// is UTC.
func (r *Runtime) localZoneName() string {
	tz := r.dateEnv().TimeZone()
	canonical, _ := tz.Canonical()
	for _, name := range []string{tz.ID(), canonical} {
		if name == "" {
			continue
		}
		if _, err := intl.ResolveTimeZone(intl.Embedded, name, r.intlCompat()); err == nil {
			return name
		}
	}
	seconds := tz.Offset(time.Now().UnixMilli()).Total()
	if seconds == 0 {
		return "UTC"
	}
	sign := '+'
	if seconds < 0 {
		sign, seconds = '-', -seconds
	}
	return fmt.Sprintf("%c%02d:%02d", sign, seconds/3600, seconds/60%60)
}

// --- the smaller constructors -----------------------------------------------

// numberRange writes one number against another.
func (r *Runtime) numberRange(this Value, args []Value, method string) (*rangePieces, error) {
	o, err := r.numberFormatOf(this, method)
	if err != nil {
		return nil, err
	}
	if arg(args, 0).IsUndefined() {
		return nil, r.intlInvalidType("start", "undefined")
	}
	if arg(args, 1).IsUndefined() {
		return nil, r.intlInvalidType("end", "undefined")
	}
	from, fromSpecial, err := r.numberArgument(arg(args, 0))
	if err != nil {
		return nil, err
	}
	to, toSpecial, err := r.numberArgument(arg(args, 1))
	if err != nil {
		return nil, err
	}
	if fromSpecial == "nan" {
		return nil, r.intlInvalidRange("start", "NaN")
	}
	if toSpecial == "nan" {
		return nil, r.intlInvalidRange("end", "NaN")
	}
	parts, err := o.nf.FormatDecimalRangeToParts(intlDecimal(from, fromSpecial), intlDecimal(to, toSpecial))
	if err != nil {
		return nil, r.intlInternal()
	}
	pieces := &rangePieces{}
	for _, p := range parts {
		source := "shared"
		switch p.Source {
		case intl.SourceStartRange:
			source = "startRange"
		case intl.SourceEndRange:
			source = "endRange"
		}
		pieces.add(string(p.Kind), p.Value, source)
	}
	return pieces, nil
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

// putDigits writes the digit counts a NumberFormat or PluralRules settled
// on, those resolvedOptions reports, in its order.
func (r *Runtime) putDigits(o *Object, d intl.ResolvedDigits) {
	r.putInt(o, "minimumIntegerDigits", d.MinimumIntegerDigits)
	if d.MinimumFractionDigits != nil {
		r.putInt(o, "minimumFractionDigits", *d.MinimumFractionDigits)
		r.putInt(o, "maximumFractionDigits", *d.MaximumFractionDigits)
	}
	if d.MinimumSignificantDigits != nil {
		r.putInt(o, "minimumSignificantDigits", *d.MinimumSignificantDigits)
		r.putInt(o, "maximumSignificantDigits", *d.MaximumSignificantDigits)
	}
}

// putRounding writes the rounding options a NumberFormat or PluralRules
// settled on, in resolvedOptions' order.
func (r *Runtime) putRounding(o *Object, d intl.ResolvedDigits) {
	r.putInt(o, "roundingIncrement", d.RoundingIncrement)
	r.putString(o, "roundingMode", d.RoundingMode.String())
	r.putString(o, "roundingPriority", d.RoundingPriority.String())
	r.putString(o, "trailingZeroDisplay", d.TrailingZeroDisplay.String())
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
