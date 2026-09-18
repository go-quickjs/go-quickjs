package icu

import (
	"strconv"
	"strings"
	"sync"
)

// What the time zones are called.
//
// A zone has up to six names in a language: a long one and a short one for
// standard time, for summer time, and for neither -- Eastern Standard Time,
// Eastern Daylight Time, Eastern Time; EST, EDT, ET. Most zones have none of
// them and are called an offset from Greenwich instead, which is itself
// written differently from language to language.
//
// The engine carries the seasonal names in every language, because a date
// written out ends with one and a German program should not be told its clock
// is on Central European Standard Time. The names that do not depend on the
// time of year are three times the size and are only ever reached by asking
// for them by name, so they come with the intldata package.

// ZoneNaming is what a zone is called in one language. Any of these may be
// empty, which means that language calls the zone an offset from Greenwich.
type ZoneNaming struct {
	LongStandard  string
	LongDaylight  string
	ShortStandard string
	ShortDaylight string
	LongGeneric   string
	ShortGeneric  string
}

// ZoneNamesIn is what a zone is called in a language.
//
// A zone nobody has named is all empty, and so is one in a language the data
// says nothing about, which leaves the caller to write the offset instead.
// The names that only intldata carries are answered in English until it is
// imported, since a name in the wrong language still says which zone it is.
func ZoneNamesIn(locale, zone string) ZoneNaming {
	zone = namedZone(zone)
	english := zoneNames[zone]
	out := ZoneNaming{
		LongStandard:  field(english, 0),
		LongDaylight:  field(english, 1),
		ShortStandard: field(english, 2),
		ShortDaylight: field(english, 3),
		LongGeneric:   field(english, 4),
		ShortGeneric:  field(english, 5),
	}
	if entry, ok := seasonNames.entry(locale, zone); ok {
		out.LongStandard = field(entry, 0)
		out.LongDaylight = field(entry, 1)
		out.ShortStandard = field(entry, 2)
		out.ShortDaylight = field(entry, 3)
	}
	if entry, ok := genericNames.entry(locale, zone); ok {
		out.LongGeneric = field(entry, 0)
		out.ShortGeneric = field(entry, 1)
	}
	return out
}

// ZoneName is what a zone is called in English, for standard time or for
// summer time.
func ZoneName(zone string, daylight bool) (short, long string) {
	names := ZoneNamesIn("en", zone)
	if daylight {
		return names.ShortDaylight, names.LongDaylight
	}
	return names.ShortStandard, names.LongStandard
}

// namedZone is the zone the names are filed under: Asia/Kolkata and
// Asia/Calcutta are one place, and Greenwich goes by half a dozen names.
func namedZone(zone string) string {
	if _, ok := zoneNames[zone]; ok {
		return zone
	}
	if other, ok := zoneAliases[zone]; ok {
		return other
	}
	return zone
}

// field returns the i'th name in an entry, which is empty where the entry
// stops short: most zones have no name at all, and the empty ones at the end
// are left off.
func field(entry string, i int) string { return piece(entry, '|', i) }

// piece returns the i'th of a run of values held in one string, which is how
// everything here is filed: a handful of short strings that are read together
// cost one string rather than a slice of them.
func piece(text string, sep byte, i int) string {
	for ; i > 0; i-- {
		at := strings.IndexByte(text, sep)
		if at < 0 {
			return ""
		}
		text = text[at+1:]
	}
	if at := strings.IndexByte(text, sep); at >= 0 {
		return text[:at]
	}
	return text
}

// RegisterZoneNames installs what the zones are called where the name does
// not depend on the time of year. The intldata package calls it; nothing else
// should.
func RegisterZoneNames(packed string) { genericNames.packed = packed }

var (
	seasonNames = zoneTable{packed: zoneSeasonPacked}
	// The names that do not turn with the seasons arrive with intldata, or
	// not at all.
	genericNames zoneTable
)

// zoneTable is one of the two tables of names, unpacked when something first
// asks for a language that is in it.
//
// The zones that are named alike everywhere share an entry and the languages
// that name every zone alike share a row, which is what turns eleven
// megabytes of names into one. English is in neither table: it is a map in
// the source, so a program that formats in English unpacks nothing.
type zoneTable struct {
	packed string
	once   sync.Once
	group  map[string]int
	block  map[string]int
	rows   []string
}

// entry is what a language calls a zone, and whether that language is in this
// table at all.
func (t *zoneTable) entry(locale, zone string) (string, bool) {
	if t.packed == "" {
		return "", false
	}
	t.load()
	at, ok := t.group[zone]
	if !ok {
		return "", false
	}
	block, ok := t.block[locale]
	if !ok {
		// The language itself, where the place it is spoken is not written
		// down separately: de-AT is written as de is.
		base, _, cut := strings.Cut(locale, "-")
		if !cut {
			return "", false
		}
		if block, ok = t.block[base]; !ok {
			return "", false
		}
	}
	if block >= len(t.rows) {
		return "", false
	}
	return piece(t.rows[block], '\x01', at), true
}

func (t *zoneTable) load() {
	t.once.Do(func() {
		text, err := inflate(t.packed)
		if err != nil {
			return
		}
		sections := strings.SplitN(text, "\n\n", 3)
		if len(sections) < 3 {
			return
		}
		t.group = numbered(sections[0])
		t.block = numbered(sections[1])
		t.rows = strings.Split(sections[2], "\n")
	})
}

// numbered reads a list of name=number pairs.
func numbered(text string) map[string]int {
	out := make(map[string]int, strings.Count(text, ";")+1)
	for len(text) > 0 {
		item := text
		if at := strings.IndexByte(text, ';'); at >= 0 {
			item, text = text[:at], text[at+1:]
		} else {
			text = ""
		}
		name, number, ok := strings.Cut(item, "=")
		if !ok {
			continue
		}
		if n, err := strconv.Atoi(number); err == nil {
			out[name] = n
		}
	}
	return out
}

// OffsetName writes an offset from Greenwich the way a language writes one:
// GMT-05:00 in English, UTC−05:00 in French, ‎−۰۵:۰۰ گرینویچ in Persian. The
// long form pads the hour to two digits and the short one does not, and the
// short one leaves off a whole hour's zero minutes.
func OffsetName(locale string, offsetMinutes int, long bool) string {
	form := zoneOffsetForms[0]
	if at, ok := zoneOffsetForm[locale]; ok {
		form = zoneOffsetForms[at]
	} else if base, _, cut := strings.Cut(locale, "-"); cut {
		if at, ok := zoneOffsetForm[base]; ok {
			form = zoneOffsetForms[at]
		}
	}
	system, forms, _ := strings.Cut(form, ";")
	digits, ok := NumberingDigits(system)
	if !ok {
		digits = "0123456789"
	}

	minutes, negative := offsetMinutes, false
	if minutes < 0 {
		minutes, negative = -minutes, true
	}
	// The four templates are the long form and the short one, each on either
	// side of Greenwich; the short form has a fifth and sixth for a whole
	// number of hours, which it writes without the minutes.
	at := 0
	switch {
	case long && negative:
		at = 1
	case long:
		at = 0
	case minutes%60 != 0 && negative:
		at = 3
	case minutes%60 != 0:
		at = 2
	case negative:
		at = 5
	default:
		at = 4
	}
	template := piece(forms, ';', at)

	var b strings.Builder
	b.Grow(len(template) + 4)
	for len(template) > 0 {
		before, rest, ok := strings.Cut(template, "{")
		b.WriteString(before)
		if !ok {
			break
		}
		which, after, ok := strings.Cut(rest, "}")
		if !ok {
			break
		}
		switch which {
		case "0":
			writeDigits(&b, minutes/60, long, digits)
		case "1":
			writeDigits(&b, minutes%60, true, digits)
		}
		template = after
	}
	return b.String()
}

// writeDigits writes a number in the digits a language counts in, padded to
// two where the form pads.
func writeDigits(b *strings.Builder, n int, pad bool, digits string) {
	if n >= 10 || pad {
		writeDigit(b, n/10, digits)
	}
	writeDigit(b, n%10, digits)
}

// writeDigit writes one digit, which is a character of its own in most
// languages and two or three bytes of one in many.
func writeDigit(b *strings.Builder, n int, digits string) {
	for at, r := range []rune(digits) {
		if at == n {
			b.WriteRune(r)
			return
		}
	}
}
