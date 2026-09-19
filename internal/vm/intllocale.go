package vm

import (
	"sort"
	"strings"

	"github.com/go-quickjs/go-quickjs/internal/icu"
)

// localeOptions is Intl.Locale's internal locale record. Unlike a formatter's
// locale choice it retains every syntactically valid setting, including ones
// this compact engine does not itself format.
type localeOptions struct {
	tag langTag
}

func (r *Runtime) initLocale(intl *Object) {
	proto := newObject(r.proto.object, ClassObject)
	ctor := r.newCtor("Locale", 1, proto, func(rt *Runtime, this Value, args []Value) (Value, error) {
		if err := rt.requireNew("Intl.Locale"); err != nil {
			return Undefined, err
		}
		made, err := rt.protoFromNewTargetErr(rt.intlProtoOf("Locale"))
		if err != nil {
			return Undefined, err
		}
		tag, err := rt.localeTagFrom(arg(args, 0), arg(args, 1))
		if err != nil {
			return Undefined, err
		}
		out := newObject(made, ClassObject)
		out.data = &localeOptions{tag: tag}
		return Obj(out), nil
	})
	r.defValue(intl, "Locale", Obj(ctor))
	r.intlProtos["Locale"] = proto
	r.defToStringTag(proto, "Intl.Locale")

	r.defMethod(proto, "toString", 0, func(rt *Runtime, this Value, args []Value) (Value, error) {
		o, err := rt.localeOf(this)
		if err != nil {
			return Undefined, err
		}
		return Str(NewString(o.tag.String())), nil
	})
	r.defMethod(proto, "maximize", 0, func(rt *Runtime, this Value, args []Value) (Value, error) {
		o, err := rt.localeOf(this)
		if err != nil {
			return Undefined, err
		}
		tag := maximizeLocaleTag(o.tag)
		return Obj(rt.newLocale(tag)), nil
	})
	r.defMethod(proto, "minimize", 0, func(rt *Runtime, this Value, args []Value) (Value, error) {
		o, err := rt.localeOf(this)
		if err != nil {
			return Undefined, err
		}
		tag := minimizeLocaleTag(o.tag)
		return Obj(rt.newLocale(tag)), nil
	})

	r.localeGetter(proto, "baseName", func(t *langTag) Value { return Str(NewString(t.base())) })
	r.localeGetter(proto, "language", func(t *langTag) Value { return Str(NewString(t.language)) })
	r.localeGetter(proto, "script", func(t *langTag) Value { return optionalString(t.script) })
	r.localeGetter(proto, "region", func(t *langTag) Value { return optionalString(t.region) })
	r.localeGetter(proto, "variants", func(t *langTag) Value {
		return optionalString(strings.Join(t.variants, "-"))
	})
	for _, field := range []struct {
		name, key string
		raw       bool
	}{
		{"calendar", "ca", false}, {"collation", "co", false},
		{"firstDayOfWeek", "fw", true}, {"hourCycle", "hc", false},
		{"caseFirst", "kf", true}, {"numberingSystem", "nu", false},
	} {
		field := field
		r.localeGetter(proto, field.name, func(t *langTag) Value {
			value, ok := t.keywordValue(field.key)
			if field.raw {
				value, ok = t.rawKeywordValue(field.key)
			}
			if !ok {
				return Undefined
			}
			return Str(NewString(value))
		})
	}
	r.localeGetter(proto, "numeric", func(t *langTag) Value {
		value, ok := t.keywordValue("kn")
		return Bool(ok && value == "true")
	})

	r.defMethod(proto, "getCalendars", 0, func(rt *Runtime, this Value, args []Value) (Value, error) {
		o, err := rt.localeOf(this)
		if err != nil {
			return Undefined, err
		}
		if calendar, ok := o.tag.keywordValue("ca"); ok {
			return Obj(rt.stringArray([]string{calendar})), nil
		}
		info := icu.TerritoryInfoFor(localeRegionPreference(o.tag))
		values := info.Calendars
		if len(values) == 0 {
			values = []string{"gregory"}
		}
		return Obj(rt.stringArray(values)), nil
	})
	r.defMethod(proto, "getCollations", 0, func(rt *Runtime, this Value, args []Value) (Value, error) {
		o, err := rt.localeOf(this)
		if err != nil {
			return Undefined, err
		}
		if collation, ok := o.tag.keywordValue("co"); ok {
			return Obj(rt.stringArray([]string{collation})), nil
		}
		return Obj(rt.stringArray(icu.LocaleCollations(o.tag.language, o.tag.script))), nil
	})
	r.defMethod(proto, "getHourCycles", 0, func(rt *Runtime, this Value, args []Value) (Value, error) {
		o, err := rt.localeOf(this)
		if err != nil {
			return Undefined, err
		}
		if cycle, ok := o.tag.keywordValue("hc"); ok {
			return Obj(rt.stringArray([]string{cycle})), nil
		}
		values := icu.LocaleHourCycles(o.tag.language, localeRegionPreference(o.tag))
		if len(values) == 0 {
			values = []string{"h23"}
		}
		return Obj(rt.stringArray(values)), nil
	})
	r.defMethod(proto, "getNumberingSystems", 0, func(rt *Runtime, this Value, args []Value) (Value, error) {
		o, err := rt.localeOf(this)
		if err != nil {
			return Undefined, err
		}
		if numbering, ok := o.tag.keywordValue("nu"); ok {
			return Obj(rt.stringArray([]string{numbering})), nil
		}
		numbering := icu.LocaleNumberingSystem(o.tag.language, o.tag.script, o.tag.region)
		return Obj(rt.stringArray([]string{numbering})), nil
	})
	r.defMethod(proto, "getTimeZones", 0, func(rt *Runtime, this Value, args []Value) (Value, error) {
		o, err := rt.localeOf(this)
		if err != nil {
			return Undefined, err
		}
		if o.tag.region == "" {
			return Undefined, nil
		}
		info, ok := icu.TerritoryInfoExact(o.tag.region)
		if !ok {
			return Obj(rt.stringArray(nil)), nil
		}
		return Obj(rt.stringArray(info.TimeZones)), nil
	})
	r.defMethod(proto, "getTextInfo", 0, func(rt *Runtime, this Value, args []Value) (Value, error) {
		o, err := rt.localeOf(this)
		if err != nil {
			return Undefined, err
		}
		tag := maximizeLocaleTag(o.tag)
		out := newObject(rt.proto.object, ClassObject)
		rt.putString(out, "direction", icu.ScriptDirection(tag.script))
		return Obj(out), nil
	})
	r.defMethod(proto, "getWeekInfo", 0, func(rt *Runtime, this Value, args []Value) (Value, error) {
		o, err := rt.localeOf(this)
		if err != nil {
			return Undefined, err
		}
		info := icu.TerritoryInfoFor(localeRegionPreference(o.tag))
		first := info.FirstDay
		if day, ok := o.tag.rawKeywordValue("fw"); ok {
			if n := weekdayNumber(day); n != 0 {
				first = n
			}
		}
		out := newObject(rt.proto.object, ClassObject)
		rt.putInt(out, "firstDay", first)
		weekend := make([]Value, len(info.Weekend))
		for i, day := range info.Weekend {
			weekend[i] = Int(day)
		}
		out.setOwnRaw(rt.atoms.intern("weekend"), Obj(rt.newArrayFrom(weekend)), propDefault)
		return Obj(out), nil
	})
}

func optionalString(value string) Value {
	if value == "" {
		return Undefined
	}
	return Str(NewString(value))
}

func (r *Runtime) localeGetter(proto *Object, name string, get func(*langTag) Value) {
	r.defGetter(proto, name, func(rt *Runtime, this Value, args []Value) (Value, error) {
		o, err := rt.localeOf(this)
		if err != nil {
			return Undefined, err
		}
		return get(&o.tag), nil
	})
}

func (r *Runtime) localeOf(this Value) (*localeOptions, error) {
	if o := this.Object(); o != nil {
		if locale, ok := o.data.(*localeOptions); ok {
			return locale, nil
		}
	}
	return nil, r.throwTypeError("this is not an Intl.Locale")
}

func (r *Runtime) newLocale(tag langTag) *Object {
	o := newObject(r.intlProtoOf("Locale"), ClassObject)
	o.data = &localeOptions{tag: tag.clone()}
	return o
}

func (r *Runtime) localeTagFrom(value, optionsValue Value) (langTag, error) {
	// ECMA-402 coerces options before inspecting the tag. Besides preserving
	// observable getter order, this determines which error wins when both are
	// invalid (for example an invalid tag with null options).
	options, err := r.optionsObject(optionsValue)
	if err != nil {
		return langTag{}, err
	}

	var tag langTag
	if locale, ok := r.localeData(value); ok {
		tag = locale.tag.clone()
	} else {
		if !value.IsString() && !value.IsObject() {
			return tag, r.throwTypeError("a locale tag is a string or Locale")
		}
		text, err := r.toString(value)
		if err != nil {
			return tag, err
		}
		var valid bool
		tag, valid = parseTag(text.Go())
		if !valid {
			return tag, r.throwRangeError("that is not a language tag: %s", text.Go())
		}
		tag.applyAliases()
	}

	if err := r.applyLocaleIdentityOptions(&tag, options); err != nil {
		return tag, err
	}
	tag.applyAliases()

	calendar, err := r.typeOption(options, "calendar")
	if err != nil {
		return tag, err
	}
	collation, err := r.typeOption(options, "collation")
	if err != nil {
		return tag, err
	}
	firstDay, err := r.firstDayOption(options)
	if err != nil {
		return tag, err
	}
	hourCycle, err := r.stringOption(options, "hourCycle", "", "h11", "h12", "h23", "h24")
	if err != nil {
		return tag, err
	}
	caseFirst, err := r.stringOption(options, "caseFirst", "", "upper", "lower", "false")
	if err != nil {
		return tag, err
	}
	numeric, numericSet, err := r.boolOption(options, "numeric")
	if err != nil {
		return tag, err
	}
	numbering, err := r.typeOption(options, "numberingSystem")
	if err != nil {
		return tag, err
	}
	for _, setting := range []struct{ key, value string }{
		{"ca", calendar}, {"co", collation}, {"fw", firstDay},
		{"hc", hourCycle}, {"kf", caseFirst}, {"nu", numbering},
	} {
		if setting.value != "" {
			tag.setKeyword(setting.key, setting.value)
		}
	}
	if numericSet {
		tag.setKeyword("kn", boolWord(numeric))
	}
	tag.applyAliases()
	return tag, nil
}

func (r *Runtime) localeData(v Value) (*localeOptions, bool) {
	if o := v.Object(); o != nil {
		locale, ok := o.data.(*localeOptions)
		return locale, ok
	}
	return nil, false
}

func (r *Runtime) applyLocaleIdentityOptions(tag *langTag, options *Object) error {
	language, set, err := r.localePartOption(options, "language")
	if err != nil {
		return err
	}
	if set && !((len(language) >= 2 && len(language) <= 3 || len(language) >= 5 && len(language) <= 8) && allLetters(language)) {
		return r.throwRangeError("%s is not a language", language)
	}
	script, scriptSet, err := r.localePartOption(options, "script")
	if err != nil {
		return err
	}
	if scriptSet && (len(script) != 4 || !allLetters(script)) {
		return r.throwRangeError("%s is not a script", script)
	}
	region, regionSet, err := r.localePartOption(options, "region")
	if err != nil {
		return err
	}
	if regionSet && !(len(region) == 2 && allLetters(region) || len(region) == 3 && allDigits(region)) {
		return r.throwRangeError("%s is not a region", region)
	}
	variants, variantsSet, err := r.localePartOption(options, "variants")
	if err != nil {
		return err
	}
	var parsedVariants []string
	if variantsSet {
		seen := map[string]bool{}
		for _, variant := range strings.Split(strings.ToLower(variants), "-") {
			if !isVariant(variant) || seen[variant] {
				return r.throwRangeError("%s is not a list of variants", variants)
			}
			seen[variant] = true
			parsedVariants = append(parsedVariants, variant)
		}
		sort.Strings(parsedVariants)
	}
	if set {
		tag.language = strings.ToLower(language)
	}
	if scriptSet {
		tag.script = strings.ToUpper(script[:1]) + strings.ToLower(script[1:])
	}
	if regionSet {
		tag.region = strings.ToUpper(region)
	}
	if variantsSet {
		tag.variants = parsedVariants
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
			return "", r.throwRangeError("%s is not a first day of the week", day)
		}
	}
	return canonicalSetting("fw", day), nil
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

func weekdayNumber(value string) int {
	switch weekdayName(value) {
	case "mon":
		return 1
	case "tue":
		return 2
	case "wed":
		return 3
	case "thu":
		return 4
	case "fri":
		return 5
	case "sat":
		return 6
	case "sun":
		return 7
	}
	return 0
}

func maximizeLocaleTag(tag langTag) langTag {
	out := tag.clone()
	out.language, out.script, out.region = icu.AddLikelySubtags(tag.language, tag.script, tag.region)
	return out
}

func minimizeLocaleTag(tag langTag) langTag {
	maximal := maximizeLocaleTag(tag)
	equalMaximal := func(candidate langTag) bool {
		got := maximizeLocaleTag(candidate)
		return got.language == maximal.language && got.script == maximal.script && got.region == maximal.region
	}
	base := tag.clone()
	base.language = maximal.language
	base.script, base.region = "", ""
	if equalMaximal(base) {
		return base
	}
	withRegion := base.clone()
	withRegion.region = maximal.region
	if equalMaximal(withRegion) {
		return withRegion
	}
	withScript := base.clone()
	withScript.script = maximal.script
	if equalMaximal(withScript) {
		return withScript
	}
	maximal.variants = append([]string(nil), tag.variants...)
	maximal.attributes = append([]string(nil), tag.attributes...)
	maximal.keywords = append([]keyword(nil), tag.keywords...)
	maximal.from = tag.from
	maximal.fields = append([]keyword(nil), tag.fields...)
	maximal.others = append([]string(nil), tag.others...)
	maximal.private = tag.private
	return maximal
}

func localeRegionPreference(tag langTag) string {
	region := tag.region
	if region == "" {
		if subdivision, ok := tag.rawKeywordValue("sd"); ok {
			region = subdivisionRegion(subdivision)
		}
	}
	if region == "" {
		region = maximizeLocaleTag(tag).region
	}
	if region == "" {
		region = "001"
	}
	if override, ok := tag.rawKeywordValue("rg"); ok {
		if candidate := subdivisionRegion(override); candidate != "" {
			if icu.HasTerritoryInfo(candidate) {
				return candidate
			}
		}
	}
	return region
}

func subdivisionRegion(value string) string {
	if len(value) >= 2 && allLetters(value[:2]) {
		return strings.ToUpper(value[:2])
	}
	if len(value) >= 3 && allDigits(value[:3]) {
		return value[:3]
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
