package vm

import (
	"math"
	"strings"

	intl "github.com/go-quickjs/go-intl"
)

// The rest of Intl: comparing text, choosing a plural form, joining a list,
// and saying how long ago something was.

// --- Collator ---------------------------------------------------------------

// collatorOptions is a resolved Intl.Collator: go-intl's collator, and the
// usage and sensitivity as they were named.
type collatorOptions struct {
	collator    *intl.Collator
	usage       string // sort, search
	sensitivity string // base, accent, case, variant
}

func (r *Runtime) initCollator(intlObj *Object) {
	proto := newObject(r.proto.object, ClassObject)
	ctor := r.newCtor("Collator", 0, proto, func(rt *Runtime, this Value, args []Value) (Value, error) {
		proto, err := rt.protoFromNewTargetErr(rt.intlProtoOf("Collator"))
		if err != nil {
			return Undefined, err
		}
		o, err := rt.collatorFor(args)
		if err != nil {
			return Undefined, err
		}
		out := newObject(proto, ClassObject)
		out.data = o
		return Obj(out), nil
	})
	r.defValue(intlObj, "Collator", Obj(ctor))
	r.intlProtos["Collator"] = proto
	r.defToStringTag(proto, "Intl.Collator")
	r.defSupportedLocalesOfService(ctor, intl.ServiceCollator)

	// compare is a getter for a bound function, because it is nearly always
	// handed straight to sort.
	r.defGetter(proto, "compare", func(rt *Runtime, this Value, args []Value) (Value, error) {
		o, err := rt.collatorOf(this, "get Intl.Collator.prototype.compare")
		if err != nil {
			return Undefined, err
		}
		fn := rt.newNativeFunc("", 2, func(rt *Runtime, _ Value, args []Value) (Value, error) {
			a, err := rt.toString(arg(args, 0))
			if err != nil {
				return Undefined, err
			}
			b, err := rt.toString(arg(args, 1))
			if err != nil {
				return Undefined, err
			}
			return Int(o.compare(a.Go(), b.Go())), nil
		})
		return Obj(fn), nil
	})

	r.defMethod(proto, "resolvedOptions", 0, func(rt *Runtime, this Value, args []Value) (Value, error) {
		o, err := rt.collatorOf(this, "Intl.Collator.prototype.resolvedOptions")
		if err != nil {
			return Undefined, err
		}
		resolved := o.collator.ResolvedOptions()
		out := newObject(rt.proto.object, ClassObject)
		rt.putString(out, "locale", resolved.Locale)
		rt.putString(out, "usage", o.usage)
		rt.putString(out, "sensitivity", o.sensitivity)
		rt.putBool(out, "ignorePunctuation", resolved.IgnorePunctuation)
		rt.putString(out, "collation", resolved.Collation)
		rt.putBool(out, "numeric", resolved.Numeric)
		caseFirst := "false"
		switch resolved.CaseFirst {
		case intl.CaseFirstUpper:
			caseFirst = "upper"
		case intl.CaseFirstLower:
			caseFirst = "lower"
		}
		rt.putString(out, "caseFirst", caseFirst)
		return Obj(out), nil
	})
}

// collatorFor builds a collator from a locale and options, as the
// constructor and localeCompare, which takes the same two, both do: the
// options read in the order ECMA-402 reads them, since a getter among them
// can tell.
func (r *Runtime) collatorFor(args []Value) (*collatorOptions, error) {
	defer r.enterIntl("Intl.Collator")()
	tags, err := r.requestedLocales(arg(args, 0))
	if err != nil {
		return nil, err
	}
	options, err := r.optionsObject(arg(args, 1))
	if err != nil {
		return nil, err
	}
	o := &collatorOptions{}
	if o.usage, err = r.stringOption(options, "usage", "sort", "sort", "search"); err != nil {
		return nil, err
	}
	matcher, err := r.stringOption(options, "localeMatcher", "best fit", "lookup", "best fit")
	if err != nil {
		return nil, err
	}
	collation, err := r.typeOption(options, "collation")
	if err != nil {
		return nil, err
	}
	numeric, numericSet, err := r.boolOption(options, "numeric")
	if err != nil {
		return nil, err
	}
	caseFirst, err := r.stringOption(options, "caseFirst", "", "upper", "lower", "false")
	if err != nil {
		return nil, err
	}
	loc, err := r.intlLocale(intl.ServiceCollator, tags, matcher)
	if err != nil {
		return nil, err
	}
	if o.sensitivity, err = r.stringOption(options, "sensitivity", "variant",
		"base", "accent", "case", "variant"); err != nil {
		return nil, err
	}
	ignore, ignoreSet, err := r.boolOption(options, "ignorePunctuation")
	if err != nil {
		return nil, err
	}

	opts := intl.CollatorOptions{Collation: collation, Compat: r.intlCompat(),
		Sensitivity: map[string]intl.Sensitivity{"base": intl.SensitivityBase,
			"accent": intl.SensitivityAccent, "case": intl.SensitivityCase,
			"variant": intl.SensitivityVariant}[o.sensitivity],
		CaseFirst: map[string]intl.CaseFirst{"": intl.CaseFirstDefault, "upper": intl.CaseFirstUpper,
			"lower": intl.CaseFirstLower, "false": intl.CaseFirstFalse}[caseFirst],
	}
	if o.usage == "search" {
		opts.Usage = intl.UsageSearch
	}
	if numericSet {
		opts.Numeric = intl.Bool(numeric)
	}
	if ignoreSet {
		opts.IgnorePunctuation = intl.Bool(ignore)
	}
	if o.collator, err = intl.NewCollator(loc, opts); err != nil {
		return nil, r.intlInternal()
	}
	return o, nil
}

func (r *Runtime) collatorOf(this Value, method string) (*collatorOptions, error) {
	if o := this.Object(); o != nil {
		if opts, ok := o.data.(*collatorOptions); ok {
			return opts, nil
		}
	}
	return nil, r.intlIncompatibleReceiver(method, this)
}

// compare orders two strings, as ICU's collator does.
func (o *collatorOptions) compare(a, b string) int { return o.collator.Compare(a, b) }

// --- PluralRules ------------------------------------------------------------

// pluralOptions is a resolved Intl.PluralRules: go-intl's rules, and the
// digit options it was given, which resolvedOptions reports.
type pluralOptions struct {
	rules   *intl.PluralRules
	ordinal bool
	numbers *numberOptions
}

func (r *Runtime) initPluralRules(intlObj *Object) {
	proto := newObject(r.proto.object, ClassObject)
	ctor := r.newCtor("PluralRules", 0, proto, func(rt *Runtime, this Value, args []Value) (Value, error) {
		proto, err := rt.protoFromNewTargetErr(rt.intlProtoOf("PluralRules"))
		if err != nil {
			return Undefined, err
		}
		if !rt.Constructing() {
			return Undefined, rt.intlRequiresNew("Intl.PluralRules")
		}
		defer rt.enterIntl("Intl.PluralRules")()
		tags, err := rt.requestedLocales(arg(args, 0))
		if err != nil {
			return Undefined, err
		}
		options, err := rt.strictOptions(arg(args, 1))
		if err != nil {
			return Undefined, err
		}
		matcher, err := rt.stringOption(options, "localeMatcher", "best fit", "lookup", "best fit")
		if err != nil {
			return Undefined, err
		}
		loc, err := rt.intlLocale(intl.ServicePluralRules, tags, matcher)
		if err != nil {
			return Undefined, err
		}
		kind, err := rt.stringOption(options, "type", "cardinal", "cardinal", "ordinal")
		if err != nil {
			return Undefined, err
		}
		o := &pluralOptions{ordinal: kind == "ordinal"}
		// How a number is written decides which form a language puts it in, so
		// a plural rule reads the same digit options a number format does.
		o.numbers = &numberOptions{style: "decimal", signDisplay: "auto", useGrouping: "auto"}
		if o.numbers.notation, err = rt.stringOption(options, "notation", "standard",
			"standard", "scientific", "engineering", "compact"); err != nil {
			return Undefined, err
		}
		if o.numbers.compactDisplay, err = rt.stringOption(options, "compactDisplay", "short",
			"short", "long"); err != nil {
			return Undefined, err
		}
		if err := rt.readDigitOptions(o.numbers, options, 0, 3); err != nil {
			return Undefined, err
		}
		d := o.numbers.intlDigits()
		opts := intl.PluralRulesOptions{
			MinimumIntegerDigits: d.minInt, MinimumFractionDigits: d.minFrac,
			MaximumFractionDigits: d.maxFrac, MinimumSignificantDigits: d.minSig,
			MaximumSignificantDigits: d.maxSig, RoundingPriority: d.priority, RoundingMode: d.mode,
			RoundingIncrement: d.increment, TrailingZeroDisplay: d.trailing,
			Notation: d.notation, CompactDisplay: d.compact, Compat: rt.intlCompat(),
		}
		if o.ordinal {
			opts.Type = intl.Ordinal
		}
		if o.rules, err = intl.NewPluralRules(loc, opts); err != nil {
			return Undefined, rt.intlInternal()
		}
		out := newObject(proto, ClassObject)
		out.data = o
		return Obj(out), nil
	})
	r.defValue(intlObj, "PluralRules", Obj(ctor))
	r.intlProtos["PluralRules"] = proto
	r.defToStringTag(proto, "Intl.PluralRules")
	r.defSupportedLocalesOfService(ctor, intl.ServicePluralRules)

	r.defMethod(proto, "select", 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
		o, err := rt.pluralOf(this, "Intl.PluralRules.prototype.select")
		if err != nil {
			return Undefined, err
		}
		n, err := rt.toNumber(arg(args, 0))
		if err != nil {
			return Undefined, err
		}
		return Str(NewString(string(o.rules.Select(n)))), nil
	})
	r.defMethod(proto, "selectRange", 2, func(rt *Runtime, this Value, args []Value) (Value, error) {
		o, err := rt.pluralOf(this, "Intl.PluralRules.prototype.selectRange")
		if err != nil {
			return Undefined, err
		}
		if arg(args, 0).IsUndefined() {
			return Undefined, rt.intlInvalidType("startRange", "undefined")
		}
		if arg(args, 1).IsUndefined() {
			return Undefined, rt.intlInvalidType("endRange", "undefined")
		}
		start, err := rt.toNumber(arg(args, 0))
		if err != nil {
			return Undefined, err
		}
		end, err := rt.toNumber(arg(args, 1))
		if err != nil {
			return Undefined, err
		}
		if math.IsNaN(start) {
			return Undefined, rt.intlInvalidRange("startRange", "NaN")
		}
		if math.IsNaN(end) {
			return Undefined, rt.intlInvalidRange("endRange", "NaN")
		}
		category, err := o.rules.SelectRange(start, end)
		if err != nil {
			return Undefined, rt.intlInternal()
		}
		return Str(NewString(string(category))), nil
	})
	r.defMethod(proto, "resolvedOptions", 0, func(rt *Runtime, this Value, args []Value) (Value, error) {
		o, err := rt.pluralOf(this, "Intl.PluralRules.prototype.resolvedOptions")
		if err != nil {
			return Undefined, err
		}
		// What go-intl settled on, in the order ECMA-402's table gives.
		resolved := o.rules.ResolvedOptions()
		out := newObject(rt.proto.object, ClassObject)
		rt.putString(out, "locale", resolved.Locale)
		rt.putString(out, "type", resolved.Type.String())
		rt.putString(out, "notation", resolved.Notation.String())
		if resolved.Notation == intl.NotationCompact {
			rt.putString(out, "compactDisplay", resolved.CompactDisplay.String())
		}
		rt.putDigits(out, resolved.ResolvedDigits)
		values := make([]Value, len(resolved.PluralCategories))
		for i, c := range resolved.PluralCategories {
			values[i] = Str(NewString(string(c)))
		}
		out.setOwnRaw(rt.atoms.intern("pluralCategories"), Obj(rt.newArrayFrom(values)), propDefault)
		rt.putRounding(out, resolved.ResolvedDigits)
		return Obj(out), nil
	})
}

func (r *Runtime) pluralOf(this Value, method string) (*pluralOptions, error) {
	if o := this.Object(); o != nil {
		if opts, ok := o.data.(*pluralOptions); ok {
			return opts, nil
		}
	}
	return nil, r.intlIncompatibleReceiver(method, this)
}

// --- DisplayNames -----------------------------------------------------------

type displayOptions struct {
	names *intl.DisplayNames
	kind  string // language, region, script, currency, calendar, dateTimeField
	style string
	// fallback says whether a code with no name is answered with itself.
	fallback string
	// languageDisplay says whether a language is named as a dialect of
	// another -- Austrian German -- or on its own.
	languageDisplay string
}

func (r *Runtime) initDisplayNames(intlObj *Object) {
	proto := newObject(r.proto.object, ClassObject)
	ctor := r.newCtor("DisplayNames", 2, proto, func(rt *Runtime, this Value, args []Value) (Value, error) {
		proto, err := rt.protoFromNewTargetErr(rt.intlProtoOf("DisplayNames"))
		if err != nil {
			return Undefined, err
		}
		if !rt.Constructing() {
			return Undefined, rt.intlRequiresNew("Intl.DisplayNames")
		}
		defer rt.enterIntl("Intl.DisplayNames")()
		tags, err := rt.requestedLocales(arg(args, 0))
		if err != nil {
			return Undefined, err
		}
		if arg(args, 1).IsUndefined() {
			return Undefined, rt.intlInvalidArgumentType()
		}
		options, err := rt.strictOptions(arg(args, 1))
		if err != nil {
			return Undefined, err
		}
		// In the order the standard reads them: what kind of name is wanted is
		// not asked for first, though it is the one that must be there.
		matcher, err := rt.stringOption(options, "localeMatcher", "best fit", "lookup", "best fit")
		if err != nil {
			return Undefined, err
		}
		loc, err := rt.intlLocale(intl.ServiceDisplayNames, tags, matcher)
		if err != nil {
			return Undefined, err
		}
		o := &displayOptions{}
		if o.style, err = rt.stringOption(options, "style", "long",
			"narrow", "short", "long"); err != nil {
			return Undefined, err
		}
		if o.kind, err = rt.stringOption(options, "type", "", "language", "region",
			"script", "currency", "calendar", "dateTimeField"); err != nil {
			return Undefined, err
		}
		if o.kind == "" {
			return Undefined, rt.intlInvalidArgumentType()
		}
		if o.fallback, err = rt.stringOption(options, "fallback", "code",
			"code", "none"); err != nil {
			return Undefined, err
		}
		if o.languageDisplay, err = rt.stringOption(options, "languageDisplay", "dialect",
			"dialect", "standard"); err != nil {
			return Undefined, err
		}
		kind, _ := intl.ParseDisplayKind(o.kind)
		opts := intl.DisplayNamesOptions{Kind: kind, Compat: rt.intlCompat(),
			Style: map[string]intl.DisplayStyle{"long": intl.DisplayLong, "short": intl.DisplayShort,
				"narrow": intl.DisplayNarrow}[o.style]}
		if o.fallback == "none" {
			opts.Fallback = intl.FallbackNone
		}
		if o.languageDisplay == "standard" {
			opts.LanguageDisplay = intl.LanguageStandard
		}
		if o.names, err = intl.NewDisplayNames(loc, opts); err != nil {
			return Undefined, rt.intlInternal()
		}
		out := newObject(proto, ClassObject)
		out.data = o
		return Obj(out), nil
	})
	r.defValue(intlObj, "DisplayNames", Obj(ctor))
	r.intlProtos["DisplayNames"] = proto
	r.defToStringTag(proto, "Intl.DisplayNames")
	r.defSupportedLocalesOfService(ctor, intl.ServiceDisplayNames)

	r.defMethod(proto, "of", 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
		o, err := rt.displayNamesOf(this, "Intl.DisplayNames.prototype.of")
		if err != nil {
			return Undefined, err
		}
		code, err := rt.toString(arg(args, 0))
		if err != nil {
			return Undefined, err
		}
		canonical, err := o.canonical(rt, code.Go())
		if err != nil {
			return Undefined, err
		}
		if name, ok := o.names.Of(canonical); ok {
			return Str(NewString(name)), nil
		}
		// Nothing known: the code itself, or nothing at all.
		if o.fallback == "none" {
			return Undefined, nil
		}
		return Str(NewString(canonical)), nil
	})
	r.defMethod(proto, "resolvedOptions", 0, func(rt *Runtime, this Value, args []Value) (Value, error) {
		o, err := rt.displayNamesOf(this, "Intl.DisplayNames.prototype.resolvedOptions")
		if err != nil {
			return Undefined, err
		}
		out := newObject(rt.proto.object, ClassObject)
		rt.putString(out, "locale", o.names.ResolvedOptions().Locale)
		rt.putString(out, "style", o.style)
		rt.putString(out, "type", o.kind)
		rt.putString(out, "fallback", o.fallback)
		if o.kind == "language" {
			rt.putString(out, "languageDisplay", o.languageDisplay)
		}
		return Obj(out), nil
	})
}

// canonical checks that a code is written the way a code of its kind is
// written, and writes it that way.
func (o *displayOptions) canonical(r *Runtime, code string) (string, error) {
	switch o.kind {
	case "region":
		if len(code) == 2 && allLetters(code) {
			return strings.ToUpper(code), nil
		}
		if len(code) == 3 && allDigits(code) {
			return code, nil
		}
		return "", r.intlInvalidArgumentRange()
	case "script":
		if len(code) != 4 || !allLetters(code) {
			return "", r.intlInvalidArgumentRange()
		}
		return strings.ToUpper(code[:1]) + strings.ToLower(code[1:]), nil
	case "currency":
		if len(code) != 3 || !allLetters(code) {
			return "", r.intlInvalidArgumentRange()
		}
		return strings.ToUpper(code), nil
	case "language":
		// A language is named by a tag without any of the extensions a tag may
		// carry: "en-u-hebrew" asks for something that is not a language.
		canon, err := r.canonicalizer()
		if err != nil {
			return "", r.intlInternal()
		}
		l, err := canon.Canonicalize(code)
		if err != nil || len(l.Attributes)+len(l.Keywords)+len(l.Extensions) > 0 || l.Private != "" {
			return "", r.intlInvalidArgumentRange()
		}
		return l.String(), nil
	case "calendar":
		// A calendar is named the way a setting in a tag is named.
		for _, part := range strings.Split(code, "-") {
			if len(part) < 3 || len(part) > 8 || !allAlphanumeric(part) {
				return "", r.intlInvalidArgumentRange()
			}
		}
		return strings.ToLower(code), nil
	case "dateTimeField":
		switch code {
		case "era", "year", "quarter", "month", "weekOfYear", "weekday", "day",
			"dayPeriod", "hour", "minute", "second", "timeZoneName":
			return code, nil
		}
		return "", r.intlInvalidArgumentRange()
	}
	return code, nil
}

func allDigits(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return len(s) > 0
}

func (r *Runtime) displayNamesOf(this Value, method string) (*displayOptions, error) {
	if o := this.Object(); o != nil {
		if opts, ok := o.data.(*displayOptions); ok {
			return opts, nil
		}
	}
	return nil, r.intlIncompatibleReceiver(method, this)
}

// --- ListFormat -------------------------------------------------------------

// listOptions is a resolved Intl.ListFormat: go-intl's formatter, and the
// type and style as the options named them.
type listOptions struct {
	format      *intl.ListFormat
	kind, style string
}

func (r *Runtime) initListFormat(intlObj *Object) {
	proto := newObject(r.proto.object, ClassObject)
	ctor := r.newCtor("ListFormat", 0, proto, func(rt *Runtime, this Value, args []Value) (Value, error) {
		proto, err := rt.protoFromNewTargetErr(rt.intlProtoOf("ListFormat"))
		if err != nil {
			return Undefined, err
		}
		if !rt.Constructing() {
			return Undefined, rt.intlRequiresNew("Intl.ListFormat")
		}
		defer rt.enterIntl("Intl.ListFormat")()
		tags, err := rt.requestedLocales(arg(args, 0))
		if err != nil {
			return Undefined, err
		}
		options, err := rt.strictOptions(arg(args, 1))
		if err != nil {
			return Undefined, err
		}
		matcher, err := rt.stringOption(options, "localeMatcher", "best fit", "lookup", "best fit")
		if err != nil {
			return Undefined, err
		}
		loc, err := rt.intlLocale(intl.ServiceListFormat, tags, matcher)
		if err != nil {
			return Undefined, err
		}
		o := &listOptions{}
		if o.kind, err = rt.stringOption(options, "type", "conjunction",
			"conjunction", "disjunction", "unit"); err != nil {
			return Undefined, err
		}
		if o.style, err = rt.stringOption(options, "style", "long",
			"long", "short", "narrow"); err != nil {
			return Undefined, err
		}
		opts := intl.ListFormatOptions{
			Type:  map[string]intl.ListType{"conjunction": intl.Conjunction, "disjunction": intl.Disjunction, "unit": intl.UnitList}[o.kind],
			Style: map[string]intl.ListStyle{"long": intl.ListLong, "short": intl.ListShort, "narrow": intl.ListNarrow}[o.style],
		}
		if o.format, err = intl.NewListFormat(loc, opts); err != nil {
			return Undefined, rt.intlInternal()
		}
		out := newObject(proto, ClassObject)
		out.data = o
		return Obj(out), nil
	})
	r.defValue(intlObj, "ListFormat", Obj(ctor))
	r.intlProtos["ListFormat"] = proto
	r.defToStringTag(proto, "Intl.ListFormat")
	r.defSupportedLocalesOfService(ctor, intl.ServiceListFormat)

	r.defMethod(proto, "format", 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
		o, err := rt.listFormatOf(this, "Intl.ListFormat.prototype.format")
		if err != nil {
			return Undefined, err
		}
		items, err := rt.stringList(arg(args, 0))
		if err != nil {
			return Undefined, err
		}
		return Str(NewString(o.format.Format(items))), nil
	})
	r.defMethod(proto, "formatToParts", 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
		o, err := rt.listFormatOf(this, "Intl.ListFormat.prototype.formatToParts")
		if err != nil {
			return Undefined, err
		}
		items, err := rt.stringList(arg(args, 0))
		if err != nil {
			return Undefined, err
		}
		var out []Value
		for _, p := range o.format.FormatToParts(items) {
			out = append(out, Obj(rt.partObject(string(p.Kind), p.Value)))
		}
		return Obj(rt.newArrayFrom(out)), nil
	})
	r.defMethod(proto, "resolvedOptions", 0, func(rt *Runtime, this Value, args []Value) (Value, error) {
		o, err := rt.listFormatOf(this, "Intl.ListFormat.prototype.resolvedOptions")
		if err != nil {
			return Undefined, err
		}
		out := newObject(rt.proto.object, ClassObject)
		rt.putString(out, "locale", o.format.ResolvedOptions().Locale)
		rt.putString(out, "type", o.kind)
		rt.putString(out, "style", o.style)
		return Obj(out), nil
	})
}

// stringList reads the iterable a list format is given. Every item must be a
// string, which is what the standard asks.
func (r *Runtime) stringList(v Value) ([]string, error) {
	if v.IsUndefined() {
		return nil, nil
	}
	var out []string
	err := r.iterate(v, func(item Value) error {
		if !item.IsString() {
			return r.throwTypeError("Iterable yielded %s which is not a string", r.v8Describe(item))
		}
		out = append(out, item.String().Go())
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func (r *Runtime) listFormatOf(this Value, method string) (*listOptions, error) {
	if o := this.Object(); o != nil {
		if opts, ok := o.data.(*listOptions); ok {
			return opts, nil
		}
	}
	return nil, r.intlIncompatibleReceiver(method, this)
}

// --- RelativeTimeFormat -----------------------------------------------------

// relativeOptions is a resolved Intl.RelativeTimeFormat: go-intl's
// formatter, and the options as they were named.
type relativeOptions struct {
	format  *intl.RelativeTimeFormat
	numeric string // always, auto
	style   string // long, short, narrow
}

func (r *Runtime) initRelativeTimeFormat(intlObj *Object) {
	proto := newObject(r.proto.object, ClassObject)
	ctor := r.newCtor("RelativeTimeFormat", 0, proto, func(rt *Runtime, this Value, args []Value) (Value, error) {
		proto, err := rt.protoFromNewTargetErr(rt.intlProtoOf("RelativeTimeFormat"))
		if err != nil {
			return Undefined, err
		}
		if !rt.Constructing() {
			return Undefined, rt.intlRequiresNew("Intl.RelativeTimeFormat")
		}
		defer rt.enterIntl("Intl.RelativeTimeFormat")()
		tags, err := rt.requestedLocales(arg(args, 0))
		if err != nil {
			return Undefined, err
		}
		options, err := rt.optionsObject(arg(args, 1))
		if err != nil {
			return Undefined, err
		}
		matcher, err := rt.stringOption(options, "localeMatcher", "best fit", "lookup", "best fit")
		if err != nil {
			return Undefined, err
		}
		numbering, err := rt.typeOption(options, "numberingSystem")
		if err != nil {
			return Undefined, err
		}
		loc, err := rt.intlLocale(intl.ServiceRelativeTimeFormat, tags, matcher)
		if err != nil {
			return Undefined, err
		}
		o := &relativeOptions{}
		if o.style, err = rt.stringOption(options, "style", "long", "long", "short", "narrow"); err != nil {
			return Undefined, err
		}
		if o.numeric, err = rt.stringOption(options, "numeric", "always", "always", "auto"); err != nil {
			return Undefined, err
		}
		opts := intl.RelativeTimeFormatOptions{NumberingSystem: numbering,
			Style: map[string]intl.RelativeTimeStyle{"long": intl.RelativeLong,
				"short": intl.RelativeShort, "narrow": intl.RelativeNarrow}[o.style]}
		if o.numeric == "auto" {
			opts.Numeric = intl.RelativeAuto
		}
		if o.format, err = intl.NewRelativeTimeFormat(loc, opts); err != nil {
			return Undefined, rt.intlInternal()
		}
		out := newObject(proto, ClassObject)
		out.data = o
		return Obj(out), nil
	})
	r.defValue(intlObj, "RelativeTimeFormat", Obj(ctor))
	r.intlProtos["RelativeTimeFormat"] = proto
	r.defToStringTag(proto, "Intl.RelativeTimeFormat")
	r.defSupportedLocalesOfService(ctor, intl.ServiceRelativeTimeFormat)

	// arguments reads what format and formatToParts are given: a count and a
	// unit, in that order.
	arguments := func(rt *Runtime, this Value, args []Value, method string) (*relativeOptions, float64, intl.RelativeTimeUnit, error) {
		o, err := rt.relativeOf(this, method)
		if err != nil {
			return nil, 0, 0, err
		}
		n, err := rt.toNumber(arg(args, 0))
		if err != nil {
			return nil, 0, 0, err
		}
		name, err := rt.toString(arg(args, 1))
		if err != nil {
			return nil, 0, 0, err
		}
		if math.IsNaN(n) || math.IsInf(n, 0) {
			return nil, 0, 0, rt.throwRangeError("Value need to be finite number for %s()", method)
		}
		unit, ok := intl.ParseRelativeTimeUnit(name.Go())
		if !ok || !relativeUnitNames[name.Go()] {
			return nil, 0, 0, rt.throwRangeError("Invalid unit argument for %s() '%s'", method, name.Go())
		}
		return o, n, unit, nil
	}
	r.defMethod(proto, "format", 2, func(rt *Runtime, this Value, args []Value) (Value, error) {
		o, n, unit, err := arguments(rt, this, args, "Intl.RelativeTimeFormat.prototype.format")
		if err != nil {
			return Undefined, err
		}
		return Str(NewString(o.format.Format(n, unit))), nil
	})
	r.defMethod(proto, "formatToParts", 2, func(rt *Runtime, this Value, args []Value) (Value, error) {
		o, n, unit, err := arguments(rt, this, args, "Intl.RelativeTimeFormat.prototype.formatToParts")
		if err != nil {
			return Undefined, err
		}
		var out []Value
		for _, p := range o.format.FormatToParts(n, unit) {
			part := rt.partObject(string(p.Kind), p.Value)
			if p.Unit != "" {
				rt.putString(part, "unit", p.Unit)
			}
			out = append(out, Obj(part))
		}
		return Obj(rt.newArrayFrom(out)), nil
	})
	r.defMethod(proto, "resolvedOptions", 0, func(rt *Runtime, this Value, args []Value) (Value, error) {
		o, err := rt.relativeOf(this, "Intl.RelativeTimeFormat.prototype.resolvedOptions")
		if err != nil {
			return Undefined, err
		}
		resolved := o.format.ResolvedOptions()
		out := newObject(rt.proto.object, ClassObject)
		rt.putString(out, "locale", resolved.Locale)
		rt.putString(out, "style", o.style)
		rt.putString(out, "numeric", o.numeric)
		rt.putString(out, "numberingSystem", resolved.NumberingSystem)
		return Obj(out), nil
	})
}

// relativeUnitNames are the units a relative time may be given in, with
// their plural spellings.
var relativeUnitNames = map[string]bool{
	"second": true, "seconds": true, "minute": true, "minutes": true,
	"hour": true, "hours": true, "day": true, "days": true,
	"week": true, "weeks": true, "month": true, "months": true,
	"quarter": true, "quarters": true, "year": true, "years": true,
}

func (r *Runtime) relativeOf(this Value, method string) (*relativeOptions, error) {
	if o := this.Object(); o != nil {
		if opts, ok := o.data.(*relativeOptions); ok {
			return opts, nil
		}
	}
	return nil, r.intlIncompatibleReceiver(method, this)
}

// formatNumberFor is how the toLocaleString methods reach the number
// formatting: the options are read as Intl.NumberFormat reads them.
func (r *Runtime) formatNumberFor(args []Value, x float64) (string, error) {
	o, err := r.numberOptionsFrom(args)
	if err != nil {
		return "", err
	}
	return o.nf.Format(x), nil
}
