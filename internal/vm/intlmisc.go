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
	locale    *icu.Locale
	requested string

	usage       string // sort, search
	sensitivity string // base, accent, case, variant
	numeric     bool
	caseFirst   string
	ignorePunct bool
}

func (r *Runtime) initCollator(intl *Object) {
	proto := newObject(r.proto.object, ClassObject)
	ctor := r.newCtor("Collator", 0, proto, func(rt *Runtime, this Value, args []Value) (Value, error) {
		tags, err := rt.requestedLocales(arg(args, 0))
		if err != nil {
			return Undefined, err
		}
		options, err := rt.optionsObject(arg(args, 1))
		if err != nil {
			return Undefined, err
		}
		locale, requested := rt.resolveLocale(tags)
		o := &collatorOptions{locale: locale, requested: requested}
		if o.usage, err = rt.stringOption(options, "usage", "sort", "sort", "search"); err != nil {
			return Undefined, err
		}
		if o.sensitivity, err = rt.stringOption(options, "sensitivity", "variant",
			"base", "accent", "case", "variant"); err != nil {
			return Undefined, err
		}
		if o.caseFirst, err = rt.stringOption(options, "caseFirst", "false",
			"upper", "lower", "false"); err != nil {
			return Undefined, err
		}
		numeric, _, err := rt.boolOption(options, "numeric")
		if err != nil {
			return Undefined, err
		}
		o.numeric = numeric
		ignore, _, err := rt.boolOption(options, "ignorePunctuation")
		if err != nil {
			return Undefined, err
		}
		o.ignorePunct = ignore

		out := newObject(rt.protoFromNewTarget(rt.intlProtoOf("Collator")), ClassObject)
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
		rt.putString(out, "locale", o.requested)
		rt.putString(out, "usage", o.usage)
		rt.putString(out, "sensitivity", o.sensitivity)
		rt.putBool(out, "ignorePunctuation", o.ignorePunct)
		rt.putString(out, "collation", "default")
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
	locale, requested := r.resolveLocale(tags)
	o := &collatorOptions{locale: locale, requested: requested, usage: "sort",
		sensitivity: "variant", caseFirst: "false"}
	if o.sensitivity, err = r.stringOption(options, "sensitivity", "variant",
		"base", "accent", "case", "variant"); err != nil {
		return nil, err
	}
	numeric, _, err := r.boolOption(options, "numeric")
	if err != nil {
		return nil, err
	}
	o.numeric = numeric
	ignore, _, err := r.boolOption(options, "ignorePunctuation")
	if err != nil {
		return nil, err
	}
	o.ignorePunct = ignore
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
// Without the Unicode collation table this is not the order a speaker of every
// language would give, but it is the order the letters themselves give once
// what the options say to ignore has been taken off: accents, case, or
// punctuation. Digits compare as numbers when the options ask, which is the
// part of collation a program most often actually wants.
func (o *collatorOptions) compare(a, b string) int {
	x, y := o.fold(a), o.fold(b)
	if o.numeric {
		if c := compareNumerically(x, y); c != 0 {
			return c
		}
	} else if x != y {
		if x < y {
			return -1
		}
		return 1
	}
	if x != y {
		if x < y {
			return -1
		}
		return 1
	}
	// Equal once folded means they differ only in what was ignored.
	return 0
}

// fold takes off what this sensitivity ignores.
func (o *collatorOptions) fold(s string) string {
	out := s
	if o.ignorePunct {
		out = stripPunctuation(out)
	}
	switch o.sensitivity {
	case "base":
		out = stripMarks(normalizeString(strings.ToLower(out), "NFD"))
	case "accent":
		out = strings.ToLower(out)
	case "case":
		out = stripMarks(normalizeString(out, "NFD"))
	}
	return out
}

// compareNumerically compares two strings with the runs of digits in them read
// as numbers, so that file9 comes before file10.
func compareNumerically(a, b string) int {
	i, j := 0, 0
	for i < len(a) && j < len(b) {
		if isDigitByte(a[i]) && isDigitByte(b[j]) {
			// The whole run on each side, without its leading zeros.
			si, sj := i, j
			for i < len(a) && isDigitByte(a[i]) {
				i++
			}
			for j < len(b) && isDigitByte(b[j]) {
				j++
			}
			x := strings.TrimLeft(a[si:i], "0")
			y := strings.TrimLeft(b[sj:j], "0")
			if len(x) != len(y) {
				if len(x) < len(y) {
					return -1
				}
				return 1
			}
			if x != y {
				if x < y {
					return -1
				}
				return 1
			}
			continue
		}
		if a[i] != b[j] {
			if a[i] < b[j] {
				return -1
			}
			return 1
		}
		i++
		j++
	}
	switch {
	case i == len(a) && j == len(b):
		return 0
	case i == len(a):
		return -1
	default:
		return 1
	}
}

func isDigitByte(c byte) bool { return c >= '0' && c <= '9' }

// stripMarks drops the combining marks a decomposition left behind, which is
// what turns é into e.
func stripMarks(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r >= 0x0300 && r <= 0x036f {
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

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
	// The digit options, which round the count before its form is chosen.
	minFrac, maxFrac int
}

func (r *Runtime) initPluralRules(intl *Object) {
	proto := newObject(r.proto.object, ClassObject)
	ctor := r.newCtor("PluralRules", 0, proto, func(rt *Runtime, this Value, args []Value) (Value, error) {
		tags, err := rt.requestedLocales(arg(args, 0))
		if err != nil {
			return Undefined, err
		}
		options, err := rt.optionsObject(arg(args, 1))
		if err != nil {
			return Undefined, err
		}
		locale, requested := rt.resolveLocale(tags)
		kind, err := rt.stringOption(options, "type", "cardinal", "cardinal", "ordinal")
		if err != nil {
			return Undefined, err
		}
		o := &pluralOptions{locale: locale, requested: requested, ordinal: kind == "ordinal"}
		if o.minFrac, _, err = rt.intOption(options, "minimumFractionDigits", 0, 100, 0); err != nil {
			return Undefined, err
		}
		if o.maxFrac, _, err = rt.intOption(options, "maximumFractionDigits", 0, 100, 3); err != nil {
			return Undefined, err
		}
		out := newObject(rt.protoFromNewTarget(rt.intlProtoOf("PluralRules")), ClassObject)
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
		return Str(NewString(o.rule().Category(n))), nil
	})
	r.defMethod(proto, "selectRange", 2, func(rt *Runtime, this Value, args []Value) (Value, error) {
		o, err := rt.pluralOf(this)
		if err != nil {
			return Undefined, err
		}
		// A range takes the form its end takes, which is what the languages
		// carried here do.
		end, err := rt.toNumber(arg(args, 1))
		if err != nil {
			return Undefined, err
		}
		return Str(NewString(o.rule().Category(end))), nil
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
		rt.putInt(out, "minimumIntegerDigits", 1)
		rt.putInt(out, "minimumFractionDigits", o.minFrac)
		rt.putInt(out, "maximumFractionDigits", o.maxFrac)
		categories := o.rule().Categories
		values := make([]Value, len(categories))
		for i, c := range categories {
			values[i] = Str(NewString(c))
		}
		out.setOwnRaw(rt.atoms.intern("pluralCategories"), Obj(rt.newArrayFrom(values)), propDefault)
		rt.putString(out, "roundingMode", "halfExpand")
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
		if err := rt.requireNew("Intl.ListFormat"); err != nil {
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
		locale, requested := rt.resolveLocale(tags)
		o := &listOptions{locale: locale, requested: requested}
		if o.kind, err = rt.stringOption(options, "type", "conjunction",
			"conjunction", "disjunction", "unit"); err != nil {
			return Undefined, err
		}
		if o.style, err = rt.stringOption(options, "style", "long",
			"long", "short", "narrow"); err != nil {
			return Undefined, err
		}
		out := newObject(rt.protoFromNewTarget(rt.intlProtoOf("ListFormat")), ClassObject)
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
	locale    *icu.Locale
	requested string
	numeric   string // always, auto
	style     string // long, short, narrow
	numbers   *numberOptions
}

func (r *Runtime) initRelativeTimeFormat(intl *Object) {
	proto := newObject(r.proto.object, ClassObject)
	ctor := r.newCtor("RelativeTimeFormat", 0, proto, func(rt *Runtime, this Value, args []Value) (Value, error) {
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
		locale, requested := rt.resolveLocale(tags)
		o := &relativeOptions{locale: locale, requested: requested}
		if o.numeric, err = rt.stringOption(options, "numeric", "always", "always", "auto"); err != nil {
			return Undefined, err
		}
		if o.style, err = rt.stringOption(options, "style", "long", "long", "short", "narrow"); err != nil {
			return Undefined, err
		}
		o.numbers = &numberOptions{
			locale: locale, requested: requested, style: "decimal",
			notation: "standard", signDisplay: "auto", useGrouping: true,
			minInt: 1, maxFrac: 3,
		}
		out := newObject(rt.protoFromNewTarget(rt.intlProtoOf("RelativeTimeFormat")), ClassObject)
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
		text, err := o.format(rt, n, unit.Go())
		if err != nil {
			return Undefined, err
		}
		// The parts are the text with the count marked, which is what a
		// program uses this for.
		shown := o.numbers.format(math.Abs(n))
		var out []Value
		if at := strings.Index(text, shown); at >= 0 && o.numeric == "always" {
			if at > 0 {
				out = append(out, Obj(rt.partObject("literal", text[:at])))
			}
			out = append(out, Obj(rt.partObject("integer", shown)))
			if rest := text[at+len(shown):]; rest != "" {
				out = append(out, Obj(rt.partObject("literal", rest)))
			}
		} else {
			out = append(out, Obj(rt.partObject("literal", text)))
		}
		return Obj(rt.newArrayFrom(out)), nil
	})
	r.defMethod(proto, "resolvedOptions", 0, func(rt *Runtime, this Value, args []Value) (Value, error) {
		o, err := rt.relativeOf(this)
		if err != nil {
			return Undefined, err
		}
		out := newObject(rt.proto.object, ClassObject)
		rt.putString(out, "locale", o.requested)
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
	data, ok := o.locale.Relative[unit]
	if !ok {
		return "", r.throwRangeError("this runtime has no words for %s", unit)
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
