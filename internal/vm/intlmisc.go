package vm

import (
	"math"
	"strconv"
	"strings"

	"github.com/go-quickjs/go-quickjs/internal/icu"
)

// The rest of Intl: comparing text, choosing a plural form, joining a list,
// and saying how long ago something was.

// --- Collator ---------------------------------------------------------------

// collatorOptions is a resolved Intl.Collator.
type collatorOptions struct {
	locale *icu.Locale
	choice *localeChoice

	usage       string // sort, search
	sensitivity string // base, accent, case, variant
	numeric     bool
	caseFirst   string
	ignorePunct bool
}

func (r *Runtime) initCollator(intl *Object) {
	proto := newObject(r.proto.object, ClassObject)
	ctor := r.newCtor("Collator", 0, proto, func(rt *Runtime, this Value, args []Value) (Value, error) {
		proto, err := rt.protoFromNewTargetErr(rt.intlProtoOf("Collator"))
		if err != nil {
			return Undefined, err
		}
		tags, err := rt.requestedLocales(arg(args, 0))
		if err != nil {
			return Undefined, err
		}
		options, err := rt.optionsObject(arg(args, 1))
		if err != nil {
			return Undefined, err
		}
		// The options are read in the order the standard reads them, since a
		// getter among them can tell.
		o := &collatorOptions{}
		if o.usage, err = rt.stringOption(options, "usage", "sort", "sort", "search"); err != nil {
			return Undefined, err
		}
		if _, err := rt.stringOption(options, "localeMatcher", "best fit",
			"lookup", "best fit"); err != nil {
			return Undefined, err
		}
		collation, err := rt.typeOption(options, "collation")
		if err != nil {
			return Undefined, err
		}
		numeric, numericSet, err := rt.boolOption(options, "numeric")
		if err != nil {
			return Undefined, err
		}
		caseFirst, err := rt.stringOption(options, "caseFirst", "",
			"upper", "lower", "false")
		if err != nil {
			return Undefined, err
		}

		choice := rt.resolveLocale(tags, "co", "kn", "kf")
		if collation != "" {
			choice.override("co", collation)
		}
		if numericSet {
			choice.override("kn", boolWord(numeric))
		}
		choice.override("kf", caseFirst)
		o.locale, o.choice = choice.data, choice
		o.numeric = choice.setting("kn") == "true"
		o.caseFirst = choice.setting("kf")

		if o.sensitivity, err = rt.stringOption(options, "sensitivity", "variant",
			"base", "accent", "case", "variant"); err != nil {
			return Undefined, err
		}
		ignore, ignoreSet, err := rt.boolOption(options, "ignorePunctuation")
		if err != nil {
			return Undefined, err
		}
		// Whether punctuation counts is the language's own default: Thai
		// ignores it, most languages do not.
		o.ignorePunct = o.locale.Shifted
		if ignoreSet {
			o.ignorePunct = ignore
		}

		out := newObject(proto, ClassObject)
		out.data = o
		return Obj(out), nil
	})
	r.defValue(intl, "Collator", Obj(ctor))
	r.intlProtos["Collator"] = proto
	r.defToStringTag(proto, "Intl.Collator")
	r.defSupportedLocalesOf(ctor)

	// compare is a getter for a bound function, because it is nearly always
	// handed straight to sort.
	r.defGetter(proto, "compare", func(rt *Runtime, this Value, args []Value) (Value, error) {
		o, err := rt.collatorOf(this)
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
		o, err := rt.collatorOf(this)
		if err != nil {
			return Undefined, err
		}
		out := newObject(rt.proto.object, ClassObject)
		rt.putString(out, "locale", o.choice.locale())
		rt.putString(out, "usage", o.usage)
		rt.putString(out, "sensitivity", o.sensitivity)
		rt.putBool(out, "ignorePunctuation", o.ignorePunct)
		rt.putString(out, "collation", o.choice.setting("co"))
		rt.putBool(out, "numeric", o.numeric)
		rt.putString(out, "caseFirst", o.caseFirst)
		return Obj(out), nil
	})
}

// collatorFor builds a collator from a locale and options, for the
// localeCompare method that takes the same two.
func (r *Runtime) collatorFor(args []Value) (*collatorOptions, error) {
	tags, err := r.requestedLocales(arg(args, 0))
	if err != nil {
		return nil, err
	}
	options, err := r.optionsObject(arg(args, 1))
	if err != nil {
		return nil, err
	}
	// The same options a collator reads, in the same order.
	o := &collatorOptions{usage: "sort", sensitivity: "variant", caseFirst: "false"}
	if o.usage, err = r.stringOption(options, "usage", "sort", "sort", "search"); err != nil {
		return nil, err
	}
	if _, err := r.stringOption(options, "localeMatcher", "best fit",
		"lookup", "best fit"); err != nil {
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
	choice := r.resolveLocale(tags, "co", "kn", "kf")
	if collation != "" {
		choice.override("co", collation)
	}
	if numericSet {
		choice.override("kn", boolWord(numeric))
	}
	choice.override("kf", caseFirst)
	o.locale, o.choice = choice.data, choice
	o.numeric = choice.setting("kn") == "true"
	o.caseFirst = choice.setting("kf")
	if o.sensitivity, err = r.stringOption(options, "sensitivity", "variant",
		"base", "accent", "case", "variant"); err != nil {
		return nil, err
	}
	o.ignorePunct = o.locale.Shifted
	ignore, ignoreSet, err := r.boolOption(options, "ignorePunctuation")
	if err != nil {
		return nil, err
	}
	if ignoreSet {
		o.ignorePunct = ignore
	}
	return o, nil
}

func (r *Runtime) collatorOf(this Value) (*collatorOptions, error) {
	if o := this.Object(); o != nil {
		if opts, ok := o.data.(*collatorOptions); ok {
			return opts, nil
		}
	}
	return nil, r.throwTypeError("this is not an Intl.Collator")
}

// compare orders two strings.
//
// The ordering is the Unicode one, out of the table in internal/icu: the
// letters first, then the accents, then the case, each level only where the
// one before it came out equal. What the options change is how much of that
// counts, and whether a run of digits is read as a number.
func (o *collatorOptions) compare(a, b string) int {
	// Two spellings of the same text are the same text: "ö" written as one
	// character and as an o with a mark after it sort as equal, whatever the
	// language.
	if a != b {
		a, b = normalizeString(a, "NFC"), normalizeString(b, "NFC")
	}
	if o.ignorePunct {
		a, b = stripPunctuation(a), stripPunctuation(b)
	}
	strength, skipAccents := o.strength()
	if o.numeric {
		return o.locale.CompareNumeric(a, b, strength, skipAccents, o.caseFirst == "upper")
	}
	return o.locale.Compare(a, b, strength, skipAccents, o.caseFirst == "upper")
}

// strength is how much of a difference this collator counts, and whether the
// accents are part of it.
func (o *collatorOptions) strength() (icu.Strength, bool) {
	switch o.sensitivity {
	case "base":
		return icu.Primary, false
	case "accent":
		return icu.Secondary, false
	case "case":
		// The letters and the case, but not what is between them.
		return icu.Tertiary, true
	}
	return icu.Tertiary, false
}

// stripPunctuation drops what a collator told to ignore punctuation ignores.
func stripPunctuation(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r == ' ' || r == '\t' || r == '\n':
		case r < 0x80 && !(r >= '0' && r <= '9') && !(r >= 'a' && r <= 'z') &&
			!(r >= 'A' && r <= 'Z'):
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// --- PluralRules ------------------------------------------------------------

type pluralOptions struct {
	locale    *icu.Locale
	requested string
	ordinal   bool
	// numbers is how the count would be written, since which form a language
	// puts a number in depends on how many digits are written rather than on
	// the number itself: one apple, but 1.0 apples.
	numbers *numberOptions
}

// categoryOf is the form a language puts a count in, asked of the number as it
// would be written rather than as it was given.
func (o *pluralOptions) categoryOf(n float64) string {
	rule := o.rule()
	if o.numbers == nil {
		return rule.Category(n)
	}
	// The digits the count would be written with, which is what the rules ask
	// about: whether there is a fraction, and how long it is.
	whole, fraction := o.numbers.rawDigits(o.numbers.round(decimalOf(n)))
	return rule.CategoryOf(whole, fraction, n)
}

func (r *Runtime) initPluralRules(intl *Object) {
	proto := newObject(r.proto.object, ClassObject)
	ctor := r.newCtor("PluralRules", 0, proto, func(rt *Runtime, this Value, args []Value) (Value, error) {
		proto, err := rt.protoFromNewTargetErr(rt.intlProtoOf("PluralRules"))
		if err != nil {
			return Undefined, err
		}
		if !rt.Constructing() {
			return Undefined, rt.throwTypeError("Intl.PluralRules requires new")
		}
		tags, err := rt.requestedLocales(arg(args, 0))
		if err != nil {
			return Undefined, err
		}
		options, err := rt.strictOptions(arg(args, 1))
		if err != nil {
			return Undefined, err
		}
		if _, err := rt.stringOption(options, "localeMatcher", "best fit",
			"lookup", "best fit"); err != nil {
			return Undefined, err
		}
		choice := rt.resolveLocale(tags)
		kind, err := rt.stringOption(options, "type", "cardinal", "cardinal", "ordinal")
		if err != nil {
			return Undefined, err
		}
		o := &pluralOptions{locale: choice.data, requested: choice.locale(),
			ordinal: kind == "ordinal"}
		// How a number is written decides which form a language puts it in, so
		// a plural rule reads the same digit options a number format does.
		o.numbers = &numberOptions{locale: choice.data, choice: choice, style: "decimal",
			signDisplay: "auto", useGrouping: "auto"}
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
		out := newObject(proto, ClassObject)
		out.data = o
		return Obj(out), nil
	})
	r.defValue(intl, "PluralRules", Obj(ctor))
	r.intlProtos["PluralRules"] = proto
	r.defToStringTag(proto, "Intl.PluralRules")
	r.defSupportedLocalesOf(ctor)

	r.defMethod(proto, "select", 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
		o, err := rt.pluralOf(this)
		if err != nil {
			return Undefined, err
		}
		n, err := rt.toNumber(arg(args, 0))
		if err != nil {
			return Undefined, err
		}
		return Str(NewString(o.categoryOf(n))), nil
	})
	r.defMethod(proto, "selectRange", 2, func(rt *Runtime, this Value, args []Value) (Value, error) {
		o, err := rt.pluralOf(this)
		if err != nil {
			return Undefined, err
		}
		if arg(args, 0).IsUndefined() || arg(args, 1).IsUndefined() {
			return Undefined, rt.throwTypeError("a range has two ends")
		}
		start, err := rt.toNumber(arg(args, 0))
		if err != nil {
			return Undefined, err
		}
		end, err := rt.toNumber(arg(args, 1))
		if err != nil {
			return Undefined, err
		}
		if math.IsNaN(start) || math.IsNaN(end) {
			return Undefined, rt.throwRangeError("a range does not have a NaN at either end")
		}
		// A range takes the form its end takes, which is what the languages
		// carried here do.
		return Str(NewString(o.categoryOf(end))), nil
	})
	r.defMethod(proto, "resolvedOptions", 0, func(rt *Runtime, this Value, args []Value) (Value, error) {
		o, err := rt.pluralOf(this)
		if err != nil {
			return Undefined, err
		}
		out := newObject(rt.proto.object, ClassObject)
		rt.putString(out, "locale", o.requested)
		kind := "cardinal"
		if o.ordinal {
			kind = "ordinal"
		}
		rt.putString(out, "type", kind)
		rt.putString(out, "notation", o.numbers.notation)
		rt.putInt(out, "minimumIntegerDigits", o.numbers.minInt)
		if o.numbers.reportFrac {
			rt.putInt(out, "minimumFractionDigits", o.numbers.minFrac)
			rt.putInt(out, "maximumFractionDigits", o.numbers.maxFrac)
		}
		if o.numbers.reportSig {
			rt.putInt(out, "minimumSignificantDigits", o.numbers.minSig)
			rt.putInt(out, "maximumSignificantDigits", o.numbers.maxSig)
		}
		categories := o.rule().Categories
		values := make([]Value, len(categories))
		for i, c := range categories {
			values[i] = Str(NewString(c))
		}
		out.setOwnRaw(rt.atoms.intern("pluralCategories"), Obj(rt.newArrayFrom(values)), propDefault)
		rt.putInt(out, "roundingIncrement", o.numbers.roundingIncrement)
		rt.putString(out, "roundingMode", o.numbers.roundingMode)
		rt.putString(out, "roundingPriority", o.numbers.roundingPriority)
		rt.putString(out, "trailingZeroDisplay", o.numbers.trailingZero)
		return Obj(out), nil
	})
}

func (o *pluralOptions) rule() *icu.PluralRule {
	if o.ordinal {
		return &o.locale.Ordinal
	}
	return &o.locale.Cardinal
}

func (r *Runtime) pluralOf(this Value) (*pluralOptions, error) {
	if o := this.Object(); o != nil {
		if opts, ok := o.data.(*pluralOptions); ok {
			return opts, nil
		}
	}
	return nil, r.throwTypeError("this is not an Intl.PluralRules")
}

// --- DisplayNames -----------------------------------------------------------

type displayOptions struct {
	locale    *icu.Locale
	requested string
	kind      string // language, region, script, currency, calendar, dateTimeField
	style     string
	fallback  string
	// languageDisplay says whether a language is named as a dialect of
	// another -- Austrian German -- or on its own.
	languageDisplay string
}

func (r *Runtime) initDisplayNames(intl *Object) {
	proto := newObject(r.proto.object, ClassObject)
	ctor := r.newCtor("DisplayNames", 2, proto, func(rt *Runtime, this Value, args []Value) (Value, error) {
		proto, err := rt.protoFromNewTargetErr(rt.intlProtoOf("DisplayNames"))
		if err != nil {
			return Undefined, err
		}
		if err := rt.requireNew("Intl.DisplayNames"); err != nil {
			return Undefined, err
		}
		tags, err := rt.requestedLocales(arg(args, 0))
		if err != nil {
			return Undefined, err
		}
		if arg(args, 1).IsUndefined() {
			return Undefined, rt.throwTypeError("Intl.DisplayNames needs to be told what kind of name")
		}
		options, err := rt.strictOptions(arg(args, 1))
		if err != nil {
			return Undefined, err
		}
		// In the order the standard reads them: what kind of name is wanted is
		// not asked for first, though it is the one that must be there.
		if _, err := rt.stringOption(options, "localeMatcher", "best fit",
			"lookup", "best fit"); err != nil {
			return Undefined, err
		}
		choice := rt.resolveLocale(tags)
		o := &displayOptions{locale: choice.data, requested: choice.locale()}
		if o.style, err = rt.stringOption(options, "style", "long",
			"narrow", "short", "long"); err != nil {
			return Undefined, err
		}
		if o.kind, err = rt.stringOption(options, "type", "", "language", "region",
			"script", "currency", "calendar", "dateTimeField"); err != nil {
			return Undefined, err
		}
		if o.kind == "" {
			return Undefined, rt.throwTypeError("Intl.DisplayNames needs to be told what kind of name")
		}
		if o.fallback, err = rt.stringOption(options, "fallback", "code",
			"code", "none"); err != nil {
			return Undefined, err
		}
		if o.languageDisplay, err = rt.stringOption(options, "languageDisplay", "dialect",
			"dialect", "standard"); err != nil {
			return Undefined, err
		}
		out := newObject(proto, ClassObject)
		out.data = o
		return Obj(out), nil
	})
	r.defValue(intl, "DisplayNames", Obj(ctor))
	r.intlProtos["DisplayNames"] = proto
	r.defToStringTag(proto, "Intl.DisplayNames")
	r.defSupportedLocalesOf(ctor)

	r.defMethod(proto, "of", 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
		o, err := rt.displayNamesOf(this)
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
		if name, ok := icu.DisplayName(o.locale.Tag, o.kindLetter(), canonical); ok {
			return Str(NewString(name)), nil
		}
		// Nothing known: the code itself, or nothing at all.
		if o.fallback == "none" {
			return Undefined, nil
		}
		return Str(NewString(canonical)), nil
	})
	r.defMethod(proto, "resolvedOptions", 0, func(rt *Runtime, this Value, args []Value) (Value, error) {
		o, err := rt.displayNamesOf(this)
		if err != nil {
			return Undefined, err
		}
		out := newObject(rt.proto.object, ClassObject)
		rt.putString(out, "locale", o.requested)
		rt.putString(out, "style", o.style)
		rt.putString(out, "type", o.kind)
		rt.putString(out, "fallback", o.fallback)
		if o.kind == "language" {
			rt.putString(out, "languageDisplay", o.languageDisplay)
		}
		return Obj(out), nil
	})
}

// kindLetter is how this kind of name is filed in the tables.
func (o *displayOptions) kindLetter() string {
	switch o.kind {
	case "language":
		return icu.DisplayLanguage
	case "region":
		return icu.DisplayRegion
	case "script":
		return icu.DisplayScript
	case "currency":
		return icu.DisplayCurrency
	case "calendar":
		return icu.DisplayCalendar
	}
	return icu.DisplayField
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
		return "", r.throwRangeError("that is not a region code: %s", code)
	case "script":
		if len(code) != 4 || !allLetters(code) {
			return "", r.throwRangeError("that is not a script code: %s", code)
		}
		return strings.ToUpper(code[:1]) + strings.ToLower(code[1:]), nil
	case "currency":
		if len(code) != 3 || !allLetters(code) {
			return "", r.throwRangeError("that is not a currency code: %s", code)
		}
		return strings.ToUpper(code), nil
	case "language":
		// A language is named by a tag without any of the extensions a tag may
		// carry: "en-u-hebrew" asks for something that is not a language.
		tag, ok := parseTag(code)
		if !ok || tag.hasExtensions() {
			return "", r.throwRangeError("that is not a language tag: %s", code)
		}
		return canonicalTag(tag), nil
	case "calendar":
		// A calendar is named the way a setting in a tag is named.
		for _, part := range strings.Split(code, "-") {
			if len(part) < 3 || len(part) > 8 || !allAlphanumeric(part) {
				return "", r.throwRangeError("that is not a calendar: %s", code)
			}
		}
		return strings.ToLower(code), nil
	case "dateTimeField":
		switch code {
		case "era", "year", "quarter", "month", "weekOfYear", "weekday", "day",
			"dayPeriod", "hour", "minute", "second", "timeZoneName":
			return code, nil
		}
		return "", r.throwRangeError("that is not a part of a date: %s", code)
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

func (r *Runtime) displayNamesOf(this Value) (*displayOptions, error) {
	if o := this.Object(); o != nil {
		if opts, ok := o.data.(*displayOptions); ok {
			return opts, nil
		}
	}
	return nil, r.throwTypeError("this is not an Intl.DisplayNames")
}

// --- ListFormat -------------------------------------------------------------

type listOptions struct {
	locale    *icu.Locale
	requested string
	kind      string // conjunction, disjunction, unit
	style     string // long, short, narrow
}

func (r *Runtime) initListFormat(intl *Object) {
	proto := newObject(r.proto.object, ClassObject)
	ctor := r.newCtor("ListFormat", 0, proto, func(rt *Runtime, this Value, args []Value) (Value, error) {
		proto, err := rt.protoFromNewTargetErr(rt.intlProtoOf("ListFormat"))
		if err != nil {
			return Undefined, err
		}
		if err := rt.requireNew("Intl.ListFormat"); err != nil {
			return Undefined, err
		}
		tags, err := rt.requestedLocales(arg(args, 0))
		if err != nil {
			return Undefined, err
		}
		options, err := rt.strictOptions(arg(args, 1))
		if err != nil {
			return Undefined, err
		}
		if _, err := rt.stringOption(options, "localeMatcher", "best fit",
			"lookup", "best fit"); err != nil {
			return Undefined, err
		}
		choice := rt.resolveLocale(tags)
		o := &listOptions{locale: choice.data, requested: choice.locale()}
		if o.kind, err = rt.stringOption(options, "type", "conjunction",
			"conjunction", "disjunction", "unit"); err != nil {
			return Undefined, err
		}
		if o.style, err = rt.stringOption(options, "style", "long",
			"long", "short", "narrow"); err != nil {
			return Undefined, err
		}
		out := newObject(proto, ClassObject)
		out.data = o
		return Obj(out), nil
	})
	r.defValue(intl, "ListFormat", Obj(ctor))
	r.intlProtos["ListFormat"] = proto
	r.defToStringTag(proto, "Intl.ListFormat")
	r.defSupportedLocalesOf(ctor)

	r.defMethod(proto, "format", 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
		o, err := rt.listFormatOf(this)
		if err != nil {
			return Undefined, err
		}
		items, err := rt.stringList(arg(args, 0))
		if err != nil {
			return Undefined, err
		}
		return Str(NewString(o.join(items))), nil
	})
	r.defMethod(proto, "formatToParts", 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
		o, err := rt.listFormatOf(this)
		if err != nil {
			return Undefined, err
		}
		items, err := rt.stringList(arg(args, 0))
		if err != nil {
			return Undefined, err
		}
		var out []Value
		for i, piece := range o.pieces(items) {
			_ = i
			out = append(out, Obj(rt.partObject(piece.kind, piece.value)))
		}
		return Obj(rt.newArrayFrom(out)), nil
	})
	r.defMethod(proto, "resolvedOptions", 0, func(rt *Runtime, this Value, args []Value) (Value, error) {
		o, err := rt.listFormatOf(this)
		if err != nil {
			return Undefined, err
		}
		out := newObject(rt.proto.object, ClassObject)
		rt.putString(out, "locale", o.requested)
		rt.putString(out, "type", o.kind)
		rt.putString(out, "style", o.style)
		return Obj(out), nil
	})
}

// pattern is the locale's way of joining this kind of list, with English as
// the fallback for a locale that does not have this width.
func (o *listOptions) pattern() icu.ListPattern {
	if p, ok := o.locale.Lists[o.kind+"-"+o.style]; ok {
		return p
	}
	if p, ok := o.locale.Lists[o.kind+"-long"]; ok {
		return p
	}
	return icu.ListPattern{Pair: "{0} and {1}", Start: ", ", Middle: ", ", End: ", and "}
}

type listPiece struct{ kind, value string }

// pieces is the list as its elements and the text between them.
func (o *listOptions) pieces(items []string) []listPiece {
	p := o.pattern()
	switch len(items) {
	case 0:
		return nil
	case 1:
		return []listPiece{{"element", items[0]}}
	case 2:
		// The pair pattern is the whole of a list of two.
		at := strings.Index(p.Pair, "{0}")
		end := strings.Index(p.Pair, "{1}")
		if at < 0 || end < 0 {
			return []listPiece{{"element", items[0]}, {"literal", ", "}, {"element", items[1]}}
		}
		var out []listPiece
		if before := p.Pair[:at]; before != "" {
			out = append(out, listPiece{"literal", before})
		}
		out = append(out, listPiece{"element", items[0]})
		out = append(out, listPiece{"literal", p.Pair[at+3 : end]})
		out = append(out, listPiece{"element", items[1]})
		if after := p.Pair[end+3:]; after != "" {
			out = append(out, listPiece{"literal", after})
		}
		return out
	}

	out := []listPiece{{"element", items[0]}}
	for i := 1; i < len(items); i++ {
		switch i {
		case 1:
			out = append(out, listPiece{"literal", p.Start})
		case len(items) - 1:
			out = append(out, listPiece{"literal", p.End})
		default:
			out = append(out, listPiece{"literal", p.Middle})
		}
		out = append(out, listPiece{"element", items[i]})
	}
	return out
}

func (o *listOptions) join(items []string) string {
	var b strings.Builder
	for _, piece := range o.pieces(items) {
		b.WriteString(piece.value)
	}
	return b.String()
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
			return r.throwTypeError("a list may only contain strings")
		}
		out = append(out, item.String().Go())
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func (r *Runtime) listFormatOf(this Value) (*listOptions, error) {
	if o := this.Object(); o != nil {
		if opts, ok := o.data.(*listOptions); ok {
			return opts, nil
		}
	}
	return nil, r.throwTypeError("this is not an Intl.ListFormat")
}

// --- RelativeTimeFormat -----------------------------------------------------

type relativeOptions struct {
	locale  *icu.Locale
	choice  *localeChoice
	numeric string // always, auto
	style   string // long, short, narrow
	numbers *numberOptions
}

func (r *Runtime) initRelativeTimeFormat(intl *Object) {
	proto := newObject(r.proto.object, ClassObject)
	ctor := r.newCtor("RelativeTimeFormat", 0, proto, func(rt *Runtime, this Value, args []Value) (Value, error) {
		proto, err := rt.protoFromNewTargetErr(rt.intlProtoOf("RelativeTimeFormat"))
		if err != nil {
			return Undefined, err
		}
		if err := rt.requireNew("Intl.RelativeTimeFormat"); err != nil {
			return Undefined, err
		}
		tags, err := rt.requestedLocales(arg(args, 0))
		if err != nil {
			return Undefined, err
		}
		options, err := rt.optionsObject(arg(args, 1))
		if err != nil {
			return Undefined, err
		}
		if _, err := rt.stringOption(options, "localeMatcher", "best fit",
			"lookup", "best fit"); err != nil {
			return Undefined, err
		}
		numbering, err := rt.typeOption(options, "numberingSystem")
		if err != nil {
			return Undefined, err
		}
		choice := rt.resolveLocale(tags, "nu")
		choice.override("nu", numbering)
		o := &relativeOptions{locale: choice.data, choice: choice}
		if o.style, err = rt.stringOption(options, "style", "long", "long", "short", "narrow"); err != nil {
			return Undefined, err
		}
		if o.numeric, err = rt.stringOption(options, "numeric", "always", "always", "auto"); err != nil {
			return Undefined, err
		}
		o.numbers = &numberOptions{
			locale: choice.data, choice: choice, style: "decimal",
			notation: "standard", signDisplay: "auto", useGrouping: "auto",
			minInt: 1, maxFrac: 3, rounding: "fraction",
			roundingMode: "halfExpand", roundingIncrement: 1,
		}
		out := newObject(proto, ClassObject)
		out.data = o
		return Obj(out), nil
	})
	r.defValue(intl, "RelativeTimeFormat", Obj(ctor))
	r.intlProtos["RelativeTimeFormat"] = proto
	r.defToStringTag(proto, "Intl.RelativeTimeFormat")
	r.defSupportedLocalesOf(ctor)

	r.defMethod(proto, "format", 2, func(rt *Runtime, this Value, args []Value) (Value, error) {
		o, err := rt.relativeOf(this)
		if err != nil {
			return Undefined, err
		}
		n, err := rt.toNumber(arg(args, 0))
		if err != nil {
			return Undefined, err
		}
		unit, err := rt.toString(arg(args, 1))
		if err != nil {
			return Undefined, err
		}
		if math.IsNaN(n) || math.IsInf(n, 0) {
			return Undefined, rt.throwRangeError("a count of time is a finite number")
		}
		text, err := o.format(rt, n, unit.Go())
		if err != nil {
			return Undefined, err
		}
		return Str(NewString(text)), nil
	})
	r.defMethod(proto, "formatToParts", 2, func(rt *Runtime, this Value, args []Value) (Value, error) {
		o, err := rt.relativeOf(this)
		if err != nil {
			return Undefined, err
		}
		n, err := rt.toNumber(arg(args, 0))
		if err != nil {
			return Undefined, err
		}
		unit, err := rt.toString(arg(args, 1))
		if err != nil {
			return Undefined, err
		}
		if math.IsNaN(n) || math.IsInf(n, 0) {
			return Undefined, rt.throwRangeError("a count of time is a finite number")
		}
		text, err := o.format(rt, n, unit.Go())
		if err != nil {
			return Undefined, err
		}
		// The words around the count are literals, and the count itself is
		// broken into the pieces a number is made of, each saying which unit
		// it counts.
		var out []Value
		pieces := o.numbers.parts(math.Abs(n))
		shown := piecesText(pieces)
		at := strings.Index(text, shown)
		if shown == "" || at < 0 {
			return Obj(rt.newArrayFrom([]Value{Obj(rt.partObject("literal", text))})), nil
		}
		if at > 0 {
			out = append(out, Obj(rt.partObject("literal", text[:at])))
		}
		for _, piece := range pieces {
			part := rt.partObject(piece.kind, piece.value)
			rt.putString(part, "unit", relativeUnits[unit.Go()])
			out = append(out, Obj(part))
		}
		if rest := text[at+len(shown):]; rest != "" {
			out = append(out, Obj(rt.partObject("literal", rest)))
		}
		return Obj(rt.newArrayFrom(out)), nil
	})
	r.defMethod(proto, "resolvedOptions", 0, func(rt *Runtime, this Value, args []Value) (Value, error) {
		o, err := rt.relativeOf(this)
		if err != nil {
			return Undefined, err
		}
		out := newObject(rt.proto.object, ClassObject)
		rt.putString(out, "locale", o.choice.locale())
		rt.putString(out, "style", o.style)
		rt.putString(out, "numeric", o.numeric)
		rt.putString(out, "numberingSystem", o.locale.Numbering)
		return Obj(out), nil
	})
}

// relativeUnits are the units a relative time may be given in, with their
// plural spellings, which is how they are written.
var relativeUnits = map[string]string{
	"second": "second", "seconds": "second", "minute": "minute", "minutes": "minute",
	"hour": "hour", "hours": "hour", "day": "day", "days": "day",
	"week": "week", "weeks": "week", "month": "month", "months": "month",
	"quarter": "quarter", "quarters": "quarter", "year": "year", "years": "year",
}

func (o *relativeOptions) format(r *Runtime, n float64, unitName string) (string, error) {
	unit, ok := relativeUnits[unitName]
	if !ok {
		return "", r.throwRangeError("that is not a unit of time: %s", unitName)
	}
	// The shorter styles where the language writes them differently, and the
	// long words where it does not.
	data, ok := o.locale.Relative[o.style+"/"+unit]
	if !ok {
		if data, ok = o.locale.Relative[unit]; !ok {
			return "", r.throwRangeError("this runtime has no words for %s", unit)
		}
	}

	// The words a language has instead of a count: yesterday, next week.
	if o.numeric == "auto" && n == math.Trunc(n) && math.Abs(n) <= 2 {
		if named, ok := data.Named[int(n)]; ok {
			return named, nil
		}
	}

	forms := data.Future
	if math.Signbit(n) {
		forms = data.Past
	}
	category := o.locale.Cardinal.Category(math.Abs(n))
	pattern, ok := forms[category]
	if !ok {
		if pattern, ok = forms["other"]; !ok {
			for _, any := range forms {
				pattern = any
				break
			}
		}
	}
	if pattern == "" {
		return "", r.throwRangeError("this runtime has no words for %s", unit)
	}
	return strings.ReplaceAll(pattern, "{0}", o.numbers.format(math.Abs(n))), nil
}

func (r *Runtime) relativeOf(this Value) (*relativeOptions, error) {
	if o := this.Object(); o != nil {
		if opts, ok := o.data.(*relativeOptions); ok {
			return opts, nil
		}
	}
	return nil, r.throwTypeError("this is not an Intl.RelativeTimeFormat")
}

// formatNumberFor is how the toLocaleString methods reach the number
// formatting: the options are read as Intl.NumberFormat reads them.
func (r *Runtime) formatNumberFor(args []Value, x float64) (string, error) {
	o, err := r.numberOptionsFrom(args)
	if err != nil {
		return "", err
	}
	return o.format(x), nil
}

// formatBigIntFor writes a big integer, which cannot go through a float
// without losing its digits: the grouping is applied to the digits it has.
func (r *Runtime) formatBigIntFor(args []Value, digits string) (string, error) {
	o, err := r.numberOptionsFrom(args)
	if err != nil {
		return "", err
	}
	negative := strings.HasPrefix(digits, "-")
	digits = strings.TrimPrefix(digits, "-")
	if n, err := strconv.ParseFloat(digits, 64); err == nil && n < 1e15 {
		// Small enough to go through the ordinary path, fraction digits and
		// all.
		if negative {
			n = -n
		}
		return o.format(n), nil
	}
	var b strings.Builder
	if negative {
		b.WriteString(o.locale.Minus)
	}
	for _, piece := range o.groupDigits(o.localDigits(digits), false) {
		b.WriteString(piece.value)
	}
	return b.String(), nil
}
