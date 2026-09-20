package icu

import (
	"sort"
	"strings"
	"sync"
)

// How a language writes a measurement.
//
// "987 km/h" is "987 km/h" in German too but "987公里/小時" in Chinese, and
// "1 hour" is "1 Stunde" and "2 hours" is "2 Stunden": the name of the unit,
// whether a space goes before it, which form it takes with one and with many,
// and how a unit written underneath another is joined to it. None of that can
// be worked out from the name, so all of it is carried.
//
// It is kept apart from the rest of the locale data and unpacked only when a
// program asks for a measurement, since most never do.

// The units a number may be written in.
//
// Not every unit in the world: the ones ECMA-402 sanctions, which is a list of
// forty-five chosen because every language has a name for each of them. A unit
// may also be one of these divided by another -- kilometre per hour, litre per
// hundred kilometres -- which is why they are a set rather than a table.
var sanctionedUnits = map[string]bool{
	"acre": true, "bit": true, "byte": true, "celsius": true,
	"centimeter": true, "day": true, "degree": true, "fahrenheit": true,
	"fluid-ounce": true, "foot": true, "gallon": true, "gigabit": true,
	"gigabyte": true, "gram": true, "hectare": true, "hour": true,
	"inch": true, "kilobit": true, "kilobyte": true, "kilogram": true,
	"kilometer": true, "liter": true, "megabit": true, "megabyte": true,
	"meter": true, "microsecond": true, "mile": true, "mile-scandinavian": true,
	"milliliter": true, "millimeter": true, "millisecond": true, "minute": true,
	"month": true, "nanosecond": true, "ounce": true, "percent": true,
	"petabyte": true, "pound": true, "second": true, "stone": true,
	"terabit": true, "terabyte": true, "week": true, "yard": true, "year": true,
}

// HasUnit reports whether a unit is one a number may be written in.
func HasUnit(name string) bool { return sanctionedUnits[name] }

// Units lists them, in order.
func Units() []string {
	out := make([]string, 0, len(sanctionedUnits))
	for name := range sanctionedUnits {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

var (
	unitsOnce           sync.Once
	unitBlocks          map[string]map[string]string
	unitsByTag          map[string]string
	durationUnsupported map[string]bool
	unitFormSep         = ""
)

func loadUnits() {
	unitsOnce.Do(func() {
		text, err := inflate(unitsPacked)
		if err != nil {
			return
		}
		unitBlocks = map[string]map[string]string{}
		unitsByTag = map[string]string{}
		blocks, rest, _ := strings.Cut(text, "\n\n")
		tags, unsupported, _ := strings.Cut(rest, "\n\n")
		var current map[string]string
		for _, line := range strings.Split(blocks, "\n") {
			if line == "" {
				continue
			}
			key, value, ok := strings.Cut(line, "\t")
			if !ok {
				// A line without a tab names the block that follows.
				current = map[string]string{}
				unitBlocks[key] = current
				continue
			}
			if current != nil {
				current[key] = value
			}
		}
		for _, line := range strings.Split(tags, "\n") {
			tag, at, ok := strings.Cut(line, "\t")
			if ok {
				unitsByTag[tag] = at
			}
		}
		durationUnsupported = map[string]bool{}
		for _, tag := range strings.Split(unsupported, "\n") {
			if tag != "" {
				durationUnsupported[tag] = true
			}
		}
	})
}

// HasDurationLocale reports whether DurationFormat carries data for a locale.
// Other Intl constructors support a few languages that DurationFormat does
// not, so their requests must be allowed to fall through to another locale.
func HasDurationLocale(tag string) bool {
	loadUnits()
	if !Has(tag) {
		return false
	}
	resolved := ResolveTag(tag)
	return !durationUnsupported[resolved]
}

// UnitPattern is how this language writes a count of a unit, with {0} where
// the number goes: "{0} km", "{0} Stunden". The form is the plural category
// the count falls in; a language that has no such form answers with the one it
// uses for everything else.
func (l *Locale) UnitPattern(unit, width, category string) (string, bool) {
	forms, ok := l.unitForms(width + "/" + unit)
	if !ok {
		return "", false
	}
	for _, form := range strings.Split(forms, unitFormSep) {
		name, pattern, ok := strings.Cut(form, "=")
		if ok && name == category {
			return pattern, true
		}
	}
	// A language that has no such form is answered with the one it uses for
	// everything else, or failing that with whichever it has.
	all := strings.Split(forms, unitFormSep)
	for _, form := range all {
		name, pattern, ok := strings.Cut(form, "=")
		if ok && name == "other" {
			return pattern, true
		}
	}
	if len(all) > 0 {
		if _, pattern, ok := strings.Cut(all[0], "="); ok {
			return pattern, true
		}
	}
	return "", false
}

// UnitPer is a unit written underneath another, with {0} where the one above
// goes: "{0}/h" makes kilometres per hour out of kilometres.
func (l *Locale) UnitPer(unit, width string) (string, bool) {
	return l.unitForms(width + "!" + unit)
}

// UnitCompound is a pair a language writes its own way rather than by joining
// the two: Japanese says "km/h" where joining would give "km/時間".
func (l *Locale) UnitCompound(unit, width string) (string, bool) {
	return l.unitForms(width + "?" + unit)
}

// DurationListPattern returns the separators used to join duration units.
// DurationFormat supports locales that ListFormat does not, so these patterns
// are carried with the unit data rather than inferred from ListFormat.
func (l *Locale) DurationListPattern(style string) (ListPattern, bool) {
	value, ok := l.unitForms("duration-list/" + style)
	if !ok {
		return ListPattern{}, false
	}
	parts := strings.Split(value, "\x02")
	if len(parts) != 4 {
		return ListPattern{}, false
	}
	return ListPattern{
		Pair:  "{0}" + parts[0] + "{1}",
		Start: parts[1], Middle: parts[2], End: parts[3],
	}, true
}

func (l *Locale) unitForms(key string) (string, bool) {
	loadUnits()
	tag := l.Tag
	if at, ok := unitsByTag[tag]; ok {
		tag = at
	}
	block, ok := unitBlocks[tag]
	if !ok {
		block, ok = unitBlocks["en"]
		if !ok {
			return "", false
		}
	}
	value, ok := block[key]
	return value, ok
}
