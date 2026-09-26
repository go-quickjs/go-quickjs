package vm

import (
	"sort"
	"strings"

	intl "github.com/go-quickjs/go-intl"
)

// localeOptions is Intl.Locale's internal locale record: the locale, in
// canonical form, with every syntactically valid setting it was given,
// including ones the engine does not itself format.
type localeOptions struct {
	loc intl.Locale
}

func (r *Runtime) initLocale(namespace *Object) {
	proto := newObject(r.proto.object, ClassObject)
	ctor := r.newCtor("Locale", 1, proto, func(rt *Runtime, this Value, args []Value) (Value, error) {
		if !rt.Constructing() {
			return Undefined, rt.intlRequiresNew("Intl.Locale")
		}
		made, err := rt.protoFromNewTargetErr(rt.intlProtoOf("Locale"))
		if err != nil {
			return Undefined, err
		}
		loc, err := rt.localeFrom(arg(args, 0), arg(args, 1))
		if err != nil {
			return Undefined, err
		}
		out := newObject(made, ClassObject)
		out.data = &localeOptions{loc: loc}
		return Obj(out), nil
	})
	r.defValue(namespace, "Locale", Obj(ctor))
	r.intlProtos["Locale"] = proto
	r.defToStringTag(proto, "Intl.Locale")

	r.defMethod(proto, "toString", 0, func(rt *Runtime, this Value, args []Value) (Value, error) {
		o, err := rt.localeOf(this, "Intl.Locale.prototype.toString")
		if err != nil {
			return Undefined, err
		}
		return Str(NewString(o.loc.String())), nil
	})
	r.defMethod(proto, "maximize", 0, func(rt *Runtime, this Value, args []Value) (Value, error) {
		o, err := rt.localeOf(this, "Intl.Locale.prototype.maximize")
		if err != nil {
			return Undefined, err
		}
		info, err := rt.localeInfo()
		if err != nil {
			return Undefined, err
		}
		return Obj(rt.newLocale(info.Maximize(o.loc))), nil
	})
	r.defMethod(proto, "minimize", 0, func(rt *Runtime, this Value, args []Value) (Value, error) {
		o, err := rt.localeOf(this, "Intl.Locale.prototype.minimize")
		if err != nil {
			return Undefined, err
		}
		info, err := rt.localeInfo()
		if err != nil {
			return Undefined, err
		}
		return Obj(rt.newLocale(info.Minimize(o.loc))), nil
	})

	r.localeGetter(proto, "baseName", func(l intl.Locale) Value {
		return Str(NewString(intl.Locale{Language: l.Language, Script: l.Script, Region: l.Region,
			Variants: l.Variants}.String()))
	})
	r.localeGetter(proto, "language", func(l intl.Locale) Value { return Str(NewString(l.Language.String())) })
	r.localeGetter(proto, "script", func(l intl.Locale) Value { return optionalString(l.Script.String()) })
	r.localeGetter(proto, "region", func(l intl.Locale) Value { return optionalString(l.Region.String()) })
	r.localeGetter(proto, "variants", func(l intl.Locale) Value {
		variants := make([]string, len(l.Variants))
		for i, v := range l.Variants {
			variants[i] = v.String()
		}
		return optionalString(strings.Join(variants, "-"))
	})
	for _, field := range []struct {
		name, key string
		// raw is a key whose getter answers a keyword with no value as
		// the empty string, where the others answer "true", as V8 does.
		raw bool
	}{
		{"calendar", "ca", false}, {"collation", "co", false},
		{"firstDayOfWeek", "fw", true}, {"hourCycle", "hc", false},
		{"caseFirst", "kf", true}, {"numberingSystem", "nu", false},
	} {
		r.localeGetter(proto, field.name, func(l intl.Locale) Value {
			value, ok := l.Keyword(field.key)
			if !ok {
				return Undefined
			}
			if value == "" && !field.raw {
				value = "true"
			}
			return Str(NewString(value))
		})
	}
	r.localeGetter(proto, "numeric", func(l intl.Locale) Value {
		value, ok := l.Keyword("kn")
		return Bool(ok && (value == "" || value == "true"))
	})

	list := func(name string, get func(*intl.LocaleInfo, intl.Locale) ([]string, error)) {
		r.defMethod(proto, name, 0, func(rt *Runtime, this Value, args []Value) (Value, error) {
			o, err := rt.localeOf(this, "Intl.Locale.prototype."+name)
			if err != nil {
				return Undefined, err
			}
			info, err := rt.localeInfo()
			if err != nil {
				return Undefined, err
			}
			values, err := get(info, o.loc)
			if err != nil {
				return Undefined, rt.intlInternal()
			}
			return Obj(rt.stringArray(values)), nil
		})
	}
	list("getCalendars", func(i *intl.LocaleInfo, l intl.Locale) ([]string, error) { return i.Calendars(l), nil })
	list("getCollations", (*intl.LocaleInfo).Collations)
	list("getHourCycles", (*intl.LocaleInfo).HourCycles)
	list("getNumberingSystems", (*intl.LocaleInfo).NumberingSystems)
	r.defMethod(proto, "getTimeZones", 0, func(rt *Runtime, this Value, args []Value) (Value, error) {
		o, err := rt.localeOf(this, "Intl.Locale.prototype.getTimeZones")
		if err != nil {
			return Undefined, err
		}
		info, err := rt.localeInfo()
		if err != nil {
			return Undefined, err
		}
		zones, ok, err := info.TimeZones(o.loc)
		if err != nil {
			return Undefined, rt.intlInternal()
		}
		if !ok {
			return Undefined, nil
		}
		return Obj(rt.stringArray(zones)), nil
	})
	r.defMethod(proto, "getTextInfo", 0, func(rt *Runtime, this Value, args []Value) (Value, error) {
		o, err := rt.localeOf(this, "Intl.Locale.prototype.getTextInfo")
		if err != nil {
			return Undefined, err
		}
		info, err := rt.localeInfo()
		if err != nil {
			return Undefined, err
		}
		rtl, err := info.RightToLeft(o.loc)
		if err != nil {
			return Undefined, rt.intlInternal()
		}
		direction := "ltr"
		if rtl {
			direction = "rtl"
		}
		out := newObject(rt.proto.object, ClassObject)
		rt.putString(out, "direction", direction)
		return Obj(out), nil
	})
	r.defMethod(proto, "getWeekInfo", 0, func(rt *Runtime, this Value, args []Value) (Value, error) {
		o, err := rt.localeOf(this, "Intl.Locale.prototype.getWeekInfo")
		if err != nil {
			return Undefined, err
		}
		info, err := rt.localeInfo()
		if err != nil {
			return Undefined, err
		}
		first, weekend, err := info.WeekInfo(o.loc)
		if err != nil {
			return Undefined, rt.intlInternal()
		}
		out := newObject(rt.proto.object, ClassObject)
		rt.putInt(out, "firstDay", first)
		days := make([]Value, len(weekend))
		for i, day := range weekend {
			days[i] = Int(day)
		}
		out.setOwnRaw(rt.atoms.intern("weekend"), Obj(rt.newArrayFrom(days)), propDefault)
		return Obj(out), nil
	})
}

// localeInfo answers Intl.Locale's questions of a locale.
func (r *Runtime) localeInfo() (*intl.LocaleInfo, error) {
	info, err := intl.NewLocaleInfo(intl.Embedded, intl.LocaleInfoOptions{Compat: r.intlCompat()})
	if err != nil {
		return nil, r.intlInternal()
	}
	return info, nil
}

func optionalString(value string) Value {
	if value == "" {
		return Undefined
	}
	return Str(NewString(value))
}

func (r *Runtime) localeGetter(proto *Object, name string, get func(intl.Locale) Value) {
	r.defGetter(proto, name, func(rt *Runtime, this Value, args []Value) (Value, error) {
		o, err := rt.localeOf(this, "Intl.Locale.prototype."+name)
		if err != nil {
			return Undefined, err
		}
		return get(o.loc), nil
	})
}

func (r *Runtime) localeOf(this Value, method string) (*localeOptions, error) {
	if o := this.Object(); o != nil {
		if locale, ok := o.data.(*localeOptions); ok {
			return locale, nil
		}
	}
	return nil, r.intlIncompatibleReceiver(method, this)
}

func (r *Runtime) newLocale(loc intl.Locale) *Object {
	o := newObject(r.intlProtoOf("Locale"), ClassObject)
	o.data = &localeOptions{loc: loc}
	return o
}

// localeFrom is the Intl.Locale constructor's locale: the tag, canonical,
// with the options' language, script, region and variants in place of its
// own, then with the options' keywords, canonical again after each.
func (r *Runtime) localeFrom(value, optionsValue Value) (intl.Locale, error) {
	defer r.enterIntl("Intl.Locale")()
	// ECMA-402 coerces options before inspecting the tag. Besides preserving
	// observable getter order, this determines which error wins when both are
	// invalid (for example an invalid tag with null options).
	options, err := r.optionsObject(optionsValue)
	if err != nil {
		return intl.Locale{}, err
	}
	canon, err := r.canonicalizer()
	if err != nil {
		return intl.Locale{}, r.intlInternal()
	}

	var loc intl.Locale
	if locale, ok := r.localeData(value); ok {
		loc = locale.loc
	} else {
		if !value.IsString() && !value.IsObject() {
			return loc, r.intlLocaleEmpty(true)
		}
		text, err := r.toString(value)
		if err != nil {
			return loc, err
		}
		if text.Go() == "" {
			return loc, r.intlLocaleEmpty(false)
		}
		if loc, err = canon.Canonicalize(text.Go()); err != nil {
			return loc, r.intlIncorrectLocale()
		}
	}

	if err := r.applyLocaleIdentityOptions(&loc, options); err != nil {
		return loc, err
	}
	loc = canon.CanonicalizeLocale(loc)

	calendar, err := r.typeOption(options, "calendar")
	if err != nil {
		return loc, err
	}
	collation, err := r.typeOption(options, "collation")
	if err != nil {
		return loc, err
	}
	firstDay, err := r.firstDayOption(options)
	if err != nil {
		return loc, err
	}
	hourCycle, err := r.stringOption(options, "hourCycle", "", "h11", "h12", "h23", "h24")
	if err != nil {
		return loc, err
	}
	caseFirst, err := r.stringOption(options, "caseFirst", "", "upper", "lower", "false")
	if err != nil {
		return loc, err
	}
	numeric, numericSet, err := r.boolOption(options, "numeric")
	if err != nil {
		return loc, err
	}
	numbering, err := r.typeOption(options, "numberingSystem")
	if err != nil {
		return loc, err
	}
	settings := []struct{ key, value string }{
		{"ca", calendar}, {"co", collation}, {"fw", firstDay},
		{"hc", hourCycle}, {"kf", caseFirst}, {"nu", numbering},
	}
	if numericSet {
		settings = append(settings, struct{ key, value string }{"kn", boolWord(numeric)})
	}
	for _, setting := range settings {
		if setting.value != "" {
			loc = withKeyword(loc, setting.key, setting.value)
		}
	}
	return canon.CanonicalizeLocale(loc), nil
}

// withKeyword is a locale with a Unicode extension keyword set, in place of
// any it had.
func withKeyword(loc intl.Locale, key, value string) intl.Locale {
	keywords := make([]intl.Keyword, 0, len(loc.Keywords)+1)
	for _, k := range loc.Keywords {
		if k.Key != key {
			keywords = append(keywords, k)
		}
	}
	loc.Keywords = append(keywords, intl.Keyword{Key: key, Value: value})
	return loc
}

func (r *Runtime) localeData(v Value) (*localeOptions, bool) {
	if o := v.Object(); o != nil {
		locale, ok := o.data.(*localeOptions)
		return locale, ok
	}
	return nil, false
}

// applyLocaleIdentityOptions is UpdateLanguageId: the options' language,
// script, region and variants, each read and checked in turn, then put in
// place of the tag's.
func (r *Runtime) applyLocaleIdentityOptions(loc *intl.Locale, options *Object) error {
	language, set, err := r.localePartOption(options, "language")
	if err != nil {
		return err
	}
	if set && !((len(language) >= 2 && len(language) <= 3 || len(language) >= 5 && len(language) <= 8) && allLetters(language)) {
		return r.intlIncorrectLocale()
	}
	script, scriptSet, err := r.localePartOption(options, "script")
	if err != nil {
		return err
	}
	if scriptSet && (len(script) != 4 || !allLetters(script)) {
		return r.intlIncorrectLocale()
	}
	region, regionSet, err := r.localePartOption(options, "region")
	if err != nil {
		return err
	}
	if regionSet && !(len(region) == 2 && allLetters(region) || len(region) == 3 && allDigits(region)) {
		return r.intlIncorrectLocale()
	}
	variants, variantsSet, err := r.localePartOption(options, "variants")
	if err != nil {
		return err
	}
	var parsedVariants []intl.Variant
	if variantsSet {
		seen := map[string]bool{}
		var names []string
		for _, variant := range strings.Split(strings.ToLower(variants), "-") {
			if !isVariant(variant) || seen[variant] {
				return r.intlIncorrectLocale()
			}
			seen[variant] = true
			names = append(names, variant)
		}
		sort.Strings(names)
		for _, name := range names {
			v, err := intl.ParseVariant(name)
			if err != nil {
				return r.intlIncorrectLocale()
			}
			parsedVariants = append(parsedVariants, v)
		}
	}
	// The subtags were checked above, and parse.
	if set {
		loc.Language, _ = intl.ParseLanguage(language)
	}
	if scriptSet {
		loc.Script, _ = intl.ParseScript(script)
	}
	if regionSet {
		loc.Region, _ = intl.ParseRegion(region)
	}
	if variantsSet {
		loc.Variants = parsedVariants
	}
	return nil
}

func (r *Runtime) localePartOption(options *Object, name string) (string, bool, error) {
	value, err := r.getProp(options, r.atoms.intern(name), Obj(options))
	if err != nil {
		return "", false, err
	}
	if value.IsUndefined() {
		return "", false, nil
	}
	text, err := r.toString(value)
	if err != nil {
		return "", false, err
	}
	return text.Go(), true, nil
}

// firstDayOption is the firstDayOfWeek option as a keyword's value: a
// weekday's name for its number, WeekdayToString, and otherwise the value,
// which must be a Unicode locale type.
func (r *Runtime) firstDayOption(options *Object) (string, error) {
	value, err := r.getProp(options, r.atoms.intern("firstDayOfWeek"), Obj(options))
	if err != nil || value.IsUndefined() {
		return "", err
	}
	text, err := r.toString(value)
	if err != nil {
		return "", err
	}
	day := strings.ToLower(text.Go())
	if named := weekdayName(day); named != "" {
		return named, nil
	}
	for _, part := range strings.Split(day, "-") {
		if len(part) < 3 || len(part) > 8 || !allAlphanumeric(part) {
			return "", r.intlIncorrectLocale()
		}
	}
	return day, nil
}

func weekdayName(value string) string {
	switch value {
	case "0", "7", "sun":
		return "sun"
	case "1", "mon":
		return "mon"
	case "2", "tue":
		return "tue"
	case "3", "wed":
		return "wed"
	case "4", "thu":
		return "thu"
	case "5", "fri":
		return "fri"
	case "6", "sat":
		return "sat"
	}
	return ""
}

func (r *Runtime) stringArray(values []string) *Object {
	out := make([]Value, len(values))
	for i, value := range values {
		out[i] = Str(NewString(value))
	}
	return r.newArrayFrom(out)
}
