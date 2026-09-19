package icu

import (
	"sort"
	"strings"
	"sync"
)

// What things are called: the languages, the regions, the scripts, the
// currencies, and the parts of a date.
//
// The engine carries English names in its core table. Every other language is
// registered from the separately compressed intldata asset and decoded one
// locale at a time. A name absent from that data falls back to English, as ICU
// does when it has no data for a locale.

// registered is the full set, if a host asked for it.
var (
	registeredMu      sync.RWMutex
	registeredTags    []string
	registeredRecords [][3]uint32
	registeredTable   *packedBlockTable
)

// RegisterDisplayNames gives the engine the names of things in every language.
// It is called by the intldata package, and by nothing else.
func RegisterDisplayNames(packed []byte, tags []string, blocks [][2]uint32, records [][3]uint32) {
	displayMu.Lock()
	registeredMu.Lock()
	registeredTags = tags
	registeredRecords = records
	registeredTable = newPackedBlockTable(packed, blocks)
	registeredMu.Unlock()
	for tag := range displayCache {
		if tag != "en" {
			delete(displayCache, tag)
		}
	}
	displayMu.Unlock()
}

// The kinds of thing that have a name, as the tables spell them.
const (
	DisplayLanguage = "l"
	DisplayRegion   = "r"
	DisplayScript   = "s"
	DisplayCurrency = "c"
	DisplayCalendar = "a"
	DisplayField    = "f"
)

// byLocale is each locale's names, read once and kept.
var (
	displayMu    sync.Mutex
	displayCache = map[string]map[string]string{}
)

// DisplayName reports what a locale calls something, and whether it knows.
//
// A locale with no names of its own is answered in English, which is better
// than the code and is what the fallback is for.
func DisplayName(locale, kind, code string) (string, bool) {
	for _, tag := range []string{locale, language(locale), "en"} {
		if tag == "" {
			continue
		}
		names := displayNamesFor(tag)
		if names == nil {
			continue
		}
		if name, ok := names[kind+code]; ok {
			return name, true
		}
	}
	if kind == DisplayCalendar {
		name, ok := englishCalendarDisplayNames[code]
		return name, ok
	}
	if kind == DisplayCurrency && code == "XCD" {
		return "Eastern Caribbean Dollar", true
	}
	return "", false
}

// These canonical calendar identifiers have their own DateTimeFormat
// behavior but CLDR files their display names under a parent calendar.
var englishCalendarDisplayNames = map[string]string{
	"ethioaa":          "Ethiopic Amete Alem Calendar",
	"islamic-civil":    "Hijri Calendar (tabular, civil epoch)",
	"islamic-tbla":     "Hijri Calendar (tabular, astronomical epoch)",
	"islamic-umalqura": "Hijri Calendar (Umm al-Qura)",
}

func language(tag string) string {
	if i := strings.IndexByte(tag, '-'); i > 0 {
		return tag[:i]
	}
	return ""
}

// displayNamesFor reads one locale's names, the first time it is asked for.
func displayNamesFor(tag string) map[string]string {
	displayMu.Lock()
	defer displayMu.Unlock()
	if names, ok := displayCache[tag]; ok {
		return names
	}
	record := ""
	if tag == "en" {
		record = strings.TrimSuffix(englishDisplay(), sectionSep)
	} else {
		record = registeredDisplay(tag)
	}
	name, rest, ok := strings.Cut(record, fieldSep)
	if !ok || name != tag {
		displayCache[tag] = nil
		return nil
	}
	names := map[string]string{}
	for _, item := range strings.Split(rest, fieldSep) {
		if key, value, ok := strings.Cut(item, "="); ok {
			names[key] = value
		}
	}
	displayCache[tag] = names
	return names
}

// registeredDisplay inflates only the requested locale's bounded block.
func registeredDisplay(tag string) string {
	registeredMu.RLock()
	i := sort.SearchStrings(registeredTags, tag)
	if i >= len(registeredTags) || registeredTags[i] != tag || i >= len(registeredRecords) {
		registeredMu.RUnlock()
		return ""
	}
	table, ref := registeredTable, registeredRecords[i]
	registeredMu.RUnlock()
	text, ok := table.record(ref)
	if !ok {
		return ""
	}
	return text
}

func englishDisplay() string {
	text, err := inflate(displayPacked)
	if err != nil {
		return ""
	}
	return text
}

func warmupDisplayNames() {
	_ = displayNamesFor("en")
	registeredMu.RLock()
	tags := append([]string(nil), registeredTags...)
	registeredMu.RUnlock()
	for _, tag := range tags {
		_ = displayNamesFor(tag)
	}
}
