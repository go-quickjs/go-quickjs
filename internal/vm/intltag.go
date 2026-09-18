package vm

import (
	"sort"
	"strings"

	"github.com/go-quickjs/go-quickjs/internal/icu"
)

// A language tag, taken apart the way Unicode defines one.
//
// "en" is a tag and so is "zh-Hant-HK-u-nu-hanidec-ca-chinese-x-private": a
// language, then a script, a region, the variants of it, and then extensions
// -- the "u" one, which asks for a calendar or a numbering system or an
// ordering, the "t" one, which says what the text was translated from, any
// other single-letter one, and the private use, which means nothing to anyone
// but whoever wrote it.
//
// A tag is not free-form. A script is four letters, a region is two letters or
// three digits, a variant is five to eight characters or four beginning with a
// digit, and nothing may be said twice. What is written here is that grammar,
// because a tag that does not follow it has to be refused rather than guessed
// at, and because two tags that mean the same thing have to be written the
// same way before they can be compared.
type langTag struct {
	language string   // lower case: "en", "cmn", "root"
	script   string   // title case: "Hant"
	region   string   // upper case or three digits: "HK", "419"
	variants []string // lower case, in order

	// The "u" extension: the attributes, which stand alone, and the keywords,
	// which are a two-character key and what it is set to.
	attributes []string
	keywords   []keyword
	// The "t" extension: what the text came from, and the fields that say how
	// it was turned into this language.
	from   string
	fields []keyword
	// Any other single-letter extension, and the private use, which is carried
	// along untouched apart from its case.
	others  []string
	private string
}

// keyword is one setting: "nu" and "hanidec", or "kn" and nothing, which means
// yes.
type keyword struct {
	key   string
	value string
}

// parseTag takes a tag apart, and reports whether it is one at all.
func parseTag(s string) (langTag, bool) {
	var t langTag
	if s == "" || !asciiOnly(s) {
		return t, false
	}
	parts := strings.Split(s, "-")
	for _, p := range parts {
		if p == "" {
			return t, false
		}
	}

	i := 0
	// The language, which is the only part that must be there.
	switch p := parts[0]; {
	case strings.EqualFold(p, "root"):
		// The root locale is written "und" here; "root" is how CLDR writes it
		// and is not a tag a script may use.
		return t, false
	case (len(p) >= 2 && len(p) <= 3 || len(p) >= 5 && len(p) <= 8) && allLetters(p):
		t.language = strings.ToLower(p)
	default:
		return t, false
	}
	i++

	if i < len(parts) && len(parts[i]) == 4 && allLetters(parts[i]) {
		t.script = strings.ToUpper(parts[i][:1]) + strings.ToLower(parts[i][1:])
		i++
	}
	if i < len(parts) && (len(parts[i]) == 2 && allLetters(parts[i]) ||
		len(parts[i]) == 3 && allDigits(parts[i])) {
		t.region = strings.ToUpper(parts[i])
		i++
	}
	for i < len(parts) && isVariant(parts[i]) {
		v := strings.ToLower(parts[i])
		for _, seen := range t.variants {
			if seen == v {
				return t, false
			}
		}
		t.variants = append(t.variants, v)
		i++
	}

	// The extensions, each introduced by a single character that may be used
	// only once.
	var seen []string
	for i < len(parts) {
		singleton := strings.ToLower(parts[i])
		if len(singleton) != 1 || !allAlphanumeric(singleton) {
			return t, false
		}
		for _, s := range seen {
			if s == singleton {
				return t, false
			}
		}
		seen = append(seen, singleton)
		i++

		start := i
		for i < len(parts) && len(parts[i]) > 1 {
			i++
		}
		body := parts[start:i]
		if singleton == "x" {
			// The private use runs to the end of the tag, and its parts may be
			// a single character.
			body = parts[start:]
			i = len(parts)
			if len(body) == 0 {
				return t, false
			}
			for _, p := range body {
				if len(p) > 8 || !allAlphanumeric(p) {
					return t, false
				}
			}
			t.private = "x-" + strings.ToLower(strings.Join(body, "-"))
			continue
		}
		if len(body) == 0 {
			return t, false
		}
		switch singleton {
		case "u":
			if !t.parseUnicode(body) {
				return t, false
			}
		case "t":
			if !t.parseTransform(body) {
				return t, false
			}
		default:
			for _, p := range body {
				if len(p) < 2 || len(p) > 8 || !allAlphanumeric(p) {
					return t, false
				}
			}
			t.others = append(t.others,
				singleton+"-"+strings.ToLower(strings.Join(body, "-")))
		}
	}
	return t, true
}

// parseUnicode reads the "u" extension: attributes, which stand alone, then
// keywords, which are a key and however many words it is set to.
func (t *langTag) parseUnicode(body []string) bool {
	i := 0
	for i < len(body) && isAttribute(body[i]) {
		t.attributes = append(t.attributes, strings.ToLower(body[i]))
		i++
	}
	for i < len(body) {
		if !isUnicodeKey(body[i]) {
			return false
		}
		k := keyword{key: strings.ToLower(body[i])}
		i++
		var value []string
		for i < len(body) && isAttribute(body[i]) {
			value = append(value, strings.ToLower(body[i]))
			i++
		}
		k.value = strings.Join(value, "-")
		for _, seenK := range t.keywords {
			if seenK.key == k.key {
				return false
			}
		}
		t.keywords = append(t.keywords, k)
	}
	return len(t.attributes) > 0 || len(t.keywords) > 0
}

// parseTransform reads the "t" extension: the tag the text came from, and the
// fields that say how it got here.
func (t *langTag) parseTransform(body []string) bool {
	i := 0
	// The language it came from, which is a tag in itself, without extensions.
	if i < len(body) && !isTransformKey(body[i]) {
		var from []string
		for i < len(body) && !isTransformKey(body[i]) {
			from = append(from, body[i])
			i++
		}
		inner, ok := parseTag(strings.Join(from, "-"))
		if !ok || inner.hasExtensions() {
			return false
		}
		// The tag a text came from is written in small letters throughout,
		// with its variants in order and its names the ones they are now.
		inner.applyAliases()
		sort.Strings(inner.variants)
		t.from = strings.ToLower(inner.base())
	}
	for i < len(body) {
		if !isTransformKey(body[i]) {
			return false
		}
		f := keyword{key: strings.ToLower(body[i])}
		i++
		var value []string
		for i < len(body) && isAttribute(body[i]) {
			value = append(value, strings.ToLower(body[i]))
			i++
		}
		if len(value) == 0 {
			return false
		}
		f.value = strings.Join(value, "-")
		for _, seenF := range t.fields {
			if seenF.key == f.key {
				return false
			}
		}
		t.fields = append(t.fields, f)
	}
	return t.from != "" || len(t.fields) > 0
}

func (t *langTag) hasExtensions() bool {
	return len(t.attributes) > 0 || len(t.keywords) > 0 || t.from != "" ||
		len(t.fields) > 0 || len(t.others) > 0 || t.private != ""
}

// base is the tag without any of its extensions, which is what data is filed
// under.
func (t *langTag) base() string {
	parts := []string{t.language}
	if t.script != "" {
		parts = append(parts, t.script)
	}
	if t.region != "" {
		parts = append(parts, t.region)
	}
	parts = append(parts, t.variants...)
	return strings.Join(parts, "-")
}

// String writes the tag the one way it is written: the variants and the
// keywords in order, a setting of yes left unsaid, the private use last.
func (t *langTag) String() string {
	parts := []string{t.base()}
	var singletons []string
	if len(t.fields) > 0 || t.from != "" {
		fields := append([]keyword(nil), t.fields...)
		sort.Slice(fields, func(i, j int) bool { return fields[i].key < fields[j].key })
		ext := "t"
		if t.from != "" {
			ext += "-" + t.from
		}
		for _, f := range fields {
			ext += "-" + f.key + "-" + f.value
		}
		singletons = append(singletons, ext)
	}
	if len(t.attributes) > 0 || len(t.keywords) > 0 {
		attrs := append([]string(nil), t.attributes...)
		sort.Strings(attrs)
		keywords := append([]keyword(nil), t.keywords...)
		sort.Slice(keywords, func(i, j int) bool { return keywords[i].key < keywords[j].key })
		ext := "u"
		for _, a := range attrs {
			ext += "-" + a
		}
		for _, k := range keywords {
			ext += "-" + k.key
			// A key set to yes is written by naming it and nothing more.
			if k.value != "" && k.value != "true" {
				ext += "-" + k.value
			}
		}
		singletons = append(singletons, ext)
	}
	singletons = append(singletons, t.others...)
	sort.Strings(singletons)
	parts = append(parts, singletons...)
	if t.private != "" {
		parts = append(parts, t.private)
	}
	return strings.Join(parts, "-")
}

// canonicalSetting is what a setting or transform field goes by now, for the
// values that have been renamed: "islamicc" is the civil Islamic calendar,
// and a key set to yes is a key set to true.
func canonicalSetting(key, value string) string {
	_, _, _, _, _, settings, _ := icu.TagAliases()
	to, ok := settings[key+"-"+value]
	if !ok {
		return value
	}
	_, replaced, found := strings.Cut(to, "-")
	if !found {
		return "true"
	}
	return replaced
}

// canonicalTag writes a tag the one way it is written, with the names that
// have been replaced since replaced.
func canonicalTag(t langTag) string {
	t.applyAliases()
	return t.String()
}

// keywordValue is what a "u" key is set to, and whether it was set at all. A
// key named with nothing after it means yes.
func (t *langTag) keywordValue(key string) (string, bool) {
	for _, k := range t.keywords {
		if k.key == key {
			if k.value == "" {
				return "true", true
			}
			return k.value, true
		}
	}
	return "", false
}

// setKeywords replaces the "u" extension with the settings given, dropping it
// altogether when there are none. This is what a resolved locale carries: the
// keys that were asked for and answered, and nothing else.
func (t *langTag) setKeywords(kw []keyword) {
	t.attributes = nil
	t.keywords = nil
	for _, k := range kw {
		if k.value != "" {
			t.keywords = append(t.keywords, k)
		}
	}
}

func isVariant(s string) bool {
	if len(s) >= 5 && len(s) <= 8 && allAlphanumeric(s) {
		return true
	}
	return len(s) == 4 && s[0] >= '0' && s[0] <= '9' && allAlphanumeric(s)
}

// isAttribute covers the words a keyword is set to as well, which are written
// the same way.
func isAttribute(s string) bool {
	return len(s) >= 3 && len(s) <= 8 && allAlphanumeric(s)
}

// isUnicodeKey reports a "u" key: two characters, the second a letter.
func isUnicodeKey(s string) bool {
	return len(s) == 2 && allAlphanumeric(s) && isLetter(s[1])
}

// isTransformKey reports a "t" key: a letter and then a digit.
func isTransformKey(s string) bool {
	return len(s) == 2 && isLetter(s[0]) && s[1] >= '0' && s[1] <= '9'
}

func isLetter(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
}

func asciiOnly(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= 0x80 {
			return false
		}
	}
	return true
}

// applyAliases replaces the names a tag may still be written with by the ones
// they became: a language that was renamed, a country that was dissolved, a
// variant that was folded into another, a setting that goes by another word
// now.
func (t *langTag) applyAliases() {
	languages, regions, scripts, grandfathered, variants, settings, byLanguage :=
		icu.TagAliases()

	// A tag registered before the rules were what they are, which is replaced
	// whole: "art-lojban" is "jbo" and "zh-guoyu" is "zh".
	if to, ok := grandfathered[strings.ToLower(t.base())]; ok {
		if inner, ok := parseTag(to); ok {
			kept := *t
			*t = inner
			t.attributes, t.keywords = kept.attributes, kept.keywords
			t.from, t.fields = kept.from, kept.fields
			t.others, t.private = kept.others, kept.private
		}
	}

	if to, ok := languages[t.language]; ok {
		// A language may become a language written in a script, or one spoken
		// in a place: "sh" is Serbian written in Latin letters.
		if inner, ok := parseTag(to); ok {
			t.language = inner.language
			if inner.script != "" && t.script == "" {
				t.script = inner.script
			}
			if inner.region != "" && t.region == "" {
				t.region = inner.region
			}
		}
	}
	if to, ok := scripts[t.script]; ok {
		t.script = to
	}
	// A country that was dissolved became several, and which of them a tag
	// means depends on what language it is in, or on the letters it is written
	// with.
	switch to, ok := byLanguage[t.language+"-"+t.region]; {
	case ok:
		t.region = to
	default:
		if to, ok := byLanguage["und-"+t.script+"-"+t.region]; ok && t.script != "" {
			t.region = to
		} else if to, ok := regions[t.region]; ok {
			t.region = to
		}
	}

	// The variants, which may become another variant, a region, or nothing.
	// A pair that became one is looked for before either of them alone.
	if len(t.variants) > 1 {
		joined := strings.Join(t.variants, "-")
		if to, ok := variants[joined]; ok {
			t.variants = nil
			if to != "" {
				t.variants = []string{to}
			}
		}
	}
	kept := t.variants[:0]
	for _, variant := range t.variants {
		to, ok := variants[variant]
		switch {
		case !ok:
			kept = append(kept, variant)
		case to == "":
		case isVariant(to):
			kept = append(kept, to)
		case t.region == "":
			t.region = to
		}
	}
	t.variants = kept
	sort.Strings(t.variants)

	// The settings, where a value may have been renamed or may be the word for
	// yes, which is written by naming the key and nothing more.
	for i, k := range t.keywords {
		value := k.value
		if value == "" {
			value = "true"
		}
		if to, ok := settings[k.key+"-"+value]; ok {
			_, replaced, found := strings.Cut(to, "-")
			if !found {
				replaced = ""
			}
			t.keywords[i].value = replaced
		}
	}
	// Transform fields draw their aliases from the same Unicode BCP 47 data.
	// Unlike a Unicode keyword, a transform field always has a value.
	for i, f := range t.fields {
		if to, ok := settings[f.key+"-"+f.value]; ok {
			if _, replaced, found := strings.Cut(to, "-"); found {
				t.fields[i].value = replaced
			}
		}
	}
}
