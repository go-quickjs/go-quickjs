package icu

import (
	"sort"
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
// The engine carries all six names because Intl.DateTimeFormat is part of the
// core API. Historical names have their own transition timeline: ICU's old
// standard/daylight classification does not always agree with Go's tzdb.

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
func ZoneNamesIn(locale, zone string) ZoneNaming {
	seasonal := ZoneSeasonNamesIn(locale, zone)
	generic := ZoneGenericNamesIn(locale, zone)
	seasonal.LongGeneric = generic.LongGeneric
	seasonal.ShortGeneric = generic.ShortGeneric
	return seasonal
}

// ZoneSeasonNamesIn returns only standard/daylight names, without loading the
// independently packed generic-name table.
func ZoneSeasonNamesIn(locale, zone string) ZoneNaming {
	zone = namedZone(zone)
	english := zoneNames[zone]
	out := ZoneNaming{
		LongStandard:  field(english, 0),
		LongDaylight:  field(english, 1),
		ShortStandard: field(english, 2),
		ShortDaylight: field(english, 3),
	}
	if locale == "en" {
		return out
	}
	if entry, ok := seasonNames.entry(locale, zone); ok {
		out.LongStandard = field(entry, 0)
		out.LongDaylight = field(entry, 1)
		out.ShortStandard = field(entry, 2)
		out.ShortDaylight = field(entry, 3)
	}
	return out
}

// ZoneGenericNamesIn returns only year-round names, without loading the
// independently packed seasonal-name table.
func ZoneGenericNamesIn(locale, zone string) ZoneNaming {
	zone = namedZone(zone)
	english := zoneNames[zone]
	out := ZoneNaming{
		LongGeneric:  field(english, 4),
		ShortGeneric: field(english, 5),
	}
	if locale == "en" {
		return out
	}
	if entry, ok := genericNames.entry(locale, zone); ok {
		out.LongGeneric = field(entry, 0)
		out.ShortGeneric = field(entry, 1)
	}
	return out
}

// ZoneNamesAt is what Intl calls a zone at an exact instant. Historical
// metazones and ICU's historical standard/daylight classification can differ
// from the modern family returned by ZoneNamesIn. The boolean is false when
// the modern family should be used.
func ZoneNamesAt(locale, zone string, unixMillis int64) (ZoneNaming, bool) {
	// The generated history ends where the modern 2025 name table begins.
	// Avoid inflating the historical payload for the overwhelmingly common
	// case of formatting current and future dates.
	if unixMillis >= 1735689600000 { // 2025-01-01T00:00:00Z
		return ZoneNaming{}, false
	}
	return historicalNames.entry(locale, namedZone(zone), unixMillis)
}

// LegacyZoneNameAt is the localized long name used in Date.prototype's
// non-Intl strings. unixMillis must be in V8's directly representable window
// from the Unix epoch through signed-32-bit Unix time; callers map other
// instants to an equivalent year before asking.
func LegacyZoneNameAt(locale, zone string, unixMillis int64) (string, bool) {
	return legacyNames.entry(locale, namedZone(zone), unixMillis)
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
	switch zone {
	case "Greenwich", "Etc/Greenwich", "Etc/GMT0", "Etc/GMT+0", "Etc/GMT-0":
		return "Greenwich"
	}
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

var (
	seasonNames     = zoneTable{packed: zoneSeasonPacked}
	genericNames    = zoneTable{packed: zoneGenericPacked}
	historicalNames = zoneHistoryTable{packed: zoneHistoryPacked}
	legacyNames     = zoneLegacyTable{packed: zoneLegacyPacked}
)

type zonePeriod struct {
	from   int64
	record int
}

type zoneHistoryTable struct {
	packed         []byte
	once           sync.Once
	encodedPeriods []byte
	encodedBlocks  []byte
	periodsMu      sync.Mutex
	periods        map[string][]zonePeriod
	columns        int
	rows           [][]byte
	decodedMu      sync.Mutex
	decoded        map[int]zoneHistoryRow
}

type zoneHistoryRow struct {
	dictionary []string
	matrices   string
}

type zoneLegacyTable struct {
	packed         []byte
	once           sync.Once
	encodedChanges []byte
	changesMu      sync.Mutex
	changes        map[string][]int64
	names          zoneTable
}

func (l *zoneLegacyTable) entry(locale, zone string, unixMillis int64) (string, bool) {
	l.load()
	entry, ok := l.names.entry(locale, zone)
	if !ok && locale != "en" {
		entry, ok = l.names.entry("en", zone)
	}
	if !ok {
		return "", false
	}
	slot := l.slot(zone, unixMillis/1000)
	return field(entry, slot), true
}

func (l *zoneLegacyTable) load() {
	l.once.Do(func() {
		if len(l.packed) < 5 || string(l.packed[:5]) != "QJZL\x02" {
			return
		}
		reader := zoneTableReader{data: l.packed[5:]}
		changes, ok := reader.bytes()
		if !ok {
			return
		}
		zoneTable, ok := reader.bytes()
		if !ok {
			return
		}
		l.encodedChanges = changes
		l.names.packed = zoneTable
	})
}

// slot reports whether the requested instant uses the first or second legacy
// name. The transition lists stay encoded, so first use reads only one zone.
func (l *zoneLegacyTable) slot(zone string, second int64) int {
	changes, ok := l.changesFor(zone)
	if !ok {
		return 0
	}
	return sort.Search(len(changes), func(i int) bool { return changes[i] > second }) & 1
}

func (l *zoneLegacyTable) changesFor(zone string) ([]int64, bool) {
	l.changesMu.Lock()
	defer l.changesMu.Unlock()
	if changes, ok := l.changes[zone]; ok {
		return changes, true
	}
	reader := zoneTableReader{data: l.encodedChanges}
	count, ok := reader.uvarint()
	if !ok {
		return nil, false
	}
	for range count {
		name, ok := reader.string()
		if !ok {
			return nil, false
		}
		encoded, ok := reader.bytes()
		if !ok {
			return nil, false
		}
		if name != zone {
			continue
		}
		changeReader := zoneTableReader{data: encoded}
		changeCount, ok := changeReader.uvarint()
		if !ok {
			return nil, false
		}
		changes := make([]int64, 0, changeCount)
		for range changeCount {
			at, ok := changeReader.varint()
			if !ok {
				return nil, false
			}
			changes = append(changes, at)
		}
		if l.changes == nil {
			l.changes = make(map[string][]int64)
		}
		l.changes[zone] = changes
		return changes, true
	}
	return nil, false
}

func (h *zoneHistoryTable) entry(locale, zone string, unixMillis int64) (ZoneNaming, bool) {
	h.load()
	periods := h.periodsFor(zone)
	if len(periods) == 0 {
		return ZoneNaming{}, false
	}
	at := sort.Search(len(periods), func(i int) bool { return periods[i].from > unixMillis }) - 1
	if at < 0 || periods[at].record < 0 {
		return ZoneNaming{}, false
	}
	block, ok := h.block(locale)
	if !ok {
		base, _, cut := strings.Cut(locale, "-")
		if cut {
			block, ok = h.block(base)
		}
	}
	if !ok && locale != "en" {
		block, ok = h.block("en")
	}
	if !ok || block >= len(h.rows) || periods[at].record >= h.columns {
		return ZoneNaming{}, false
	}
	row, ok := h.row(block)
	if !ok {
		return ZoneNaming{}, false
	}
	name := func(matrix int) string {
		index := (matrix*h.columns + periods[at].record) * 2
		if index+2 > len(row.matrices) {
			return ""
		}
		id := int(row.matrices[index]) | int(row.matrices[index+1])<<8
		if id >= len(row.dictionary) {
			return ""
		}
		return row.dictionary[id]
	}
	long, short := name(0), name(1)
	return ZoneNaming{
		LongStandard:  long,
		LongDaylight:  long,
		ShortStandard: short,
		ShortDaylight: short,
		LongGeneric:   name(2),
		ShortGeneric:  name(3),
	}, true
}

func (h *zoneHistoryTable) load() {
	h.once.Do(func() {
		if len(h.packed) < 5 || string(h.packed[:5]) != "QJZH\x03" {
			return
		}
		reader := zoneTableReader{data: h.packed[5:]}
		periods, ok := reader.bytes()
		if !ok {
			return
		}
		blocks, ok := reader.bytes()
		if !ok {
			return
		}
		h.encodedPeriods = periods
		h.encodedBlocks = blocks
		rowCount, ok := reader.uvarint()
		if !ok {
			return
		}
		columns, ok := reader.uvarint()
		if !ok {
			return
		}
		h.columns = int(columns)
		h.rows = make([][]byte, 0, rowCount)
		for range rowCount {
			row, ok := reader.bytes()
			if !ok {
				return
			}
			h.rows = append(h.rows, row)
		}
		h.decoded = make(map[int]zoneHistoryRow)
	})
}

func (h *zoneHistoryTable) periodsFor(zone string) []zonePeriod {
	h.periodsMu.Lock()
	defer h.periodsMu.Unlock()
	if periods, ok := h.periods[zone]; ok {
		return periods
	}
	reader := zoneTableReader{data: h.encodedPeriods}
	count, ok := reader.uvarint()
	if !ok {
		return nil
	}
	for range count {
		name, ok := reader.string()
		if !ok {
			return nil
		}
		encoded, ok := reader.bytes()
		if !ok {
			return nil
		}
		if name != zone {
			continue
		}
		periodReader := zoneTableReader{data: encoded}
		periods := make([]zonePeriod, 0, 16)
		for len(periodReader.data) > 0 {
			from, ok := periodReader.varint()
			if !ok {
				return nil
			}
			record, ok := periodReader.uvarint()
			if !ok {
				return nil
			}
			periods = append(periods, zonePeriod{from: from, record: int(record) - 1})
		}
		if h.periods == nil {
			h.periods = make(map[string][]zonePeriod)
		}
		h.periods[zone] = periods
		return periods
	}
	return nil
}

func (h *zoneHistoryTable) block(locale string) (int, bool) {
	reader := zoneTableReader{data: h.encodedBlocks}
	count, ok := reader.uvarint()
	if !ok {
		return 0, false
	}
	for range count {
		name, ok := reader.string()
		if !ok {
			return 0, false
		}
		block, ok := reader.uvarint()
		if !ok {
			return 0, false
		}
		if name == locale {
			return int(block), true
		}
	}
	return 0, false
}

func (h *zoneHistoryTable) row(block int) (zoneHistoryRow, bool) {
	if block < 0 || block >= len(h.rows) {
		return zoneHistoryRow{}, false
	}
	h.decodedMu.Lock()
	defer h.decodedMu.Unlock()
	if row, ok := h.decoded[block]; ok {
		return row, true
	}
	text, err := inflate(h.rows[block])
	if err != nil {
		return zoneHistoryRow{}, false
	}
	reader := zoneHistoryReader{text: text}
	dictionaryCount, ok := reader.uvarint()
	if !ok {
		return zoneHistoryRow{}, false
	}
	dictionary, ok := reader.string()
	if !ok {
		return zoneHistoryRow{}, false
	}
	row := zoneHistoryRow{
		dictionary: strings.Split(dictionary, "\x00"),
		matrices:   reader.text,
	}
	if uint64(len(row.dictionary)) != dictionaryCount || len(row.matrices) != 4*h.columns*2 {
		return zoneHistoryRow{}, false
	}
	h.decoded[block] = row
	return row, true
}

type zoneHistoryReader struct {
	text string
}

func (r *zoneHistoryReader) uvarint() (uint64, bool) {
	var value uint64
	for i := 0; i < 10 && i < len(r.text); i++ {
		b := r.text[i]
		if b < 0x80 {
			if i == 9 && b > 1 {
				return 0, false
			}
			r.text = r.text[i+1:]
			return value | uint64(b)<<uint(7*i), true
		}
		value |= uint64(b&0x7f) << uint(7*i)
	}
	return 0, false
}

func (r *zoneHistoryReader) string() (string, bool) {
	length, ok := r.uvarint()
	if !ok || length > uint64(len(r.text)) {
		return "", false
	}
	value := r.text[:length]
	r.text = r.text[length:]
	return value, true
}

// zoneTable is one of the two tables of names, unpacked when something first
// asks for a language that is in it.
//
// The zones that are named alike everywhere share an entry and the languages
// that name every zone alike share a row, which is what turns eleven
// megabytes of names into one. English is in neither table: it is a map in
// the source, so a program that formats in English unpacks nothing.
type zoneTable struct {
	packed    []byte
	once      sync.Once
	group     map[string]int
	block     map[string]int
	rows      [][]byte
	decodedMu sync.Mutex
	decoded   map[int][]string
}

// entry is what a language calls a zone, and whether that language is in this
// table at all.
func (t *zoneTable) entry(locale, zone string) (string, bool) {
	if len(t.packed) == 0 && t.group == nil {
		return "", false
	}
	if t.group == nil {
		t.load()
	}
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
	row, ok := t.row(block)
	if !ok || at >= len(row) {
		return "", false
	}
	return row[at], true
}

func (t *zoneTable) load() {
	t.once.Do(func() {
		if len(t.packed) < 5 || string(t.packed[:5]) != "QJZT\x01" {
			return
		}
		reader := zoneTableReader{data: t.packed[5:]}
		groups, ok := reader.string()
		if !ok {
			return
		}
		blocks, ok := reader.string()
		if !ok {
			return
		}
		rowCount, ok := reader.uvarint()
		if !ok {
			return
		}
		t.group = numbered(groups)
		t.block = numbered(blocks)
		t.rows = make([][]byte, 0, rowCount)
		for range rowCount {
			row, ok := reader.bytes()
			if !ok {
				return
			}
			t.rows = append(t.rows, row)
		}
		t.decoded = make(map[int][]string)
	})
}

func (t *zoneTable) row(block int) ([]string, bool) {
	if block < 0 || block >= len(t.rows) {
		return nil, false
	}
	t.decodedMu.Lock()
	defer t.decodedMu.Unlock()
	if row, ok := t.decoded[block]; ok {
		return row, true
	}
	row, err := inflate(t.rows[block])
	if err != nil {
		return nil, false
	}
	entries := strings.Split(row, "\x01")
	t.decoded[block] = entries
	return entries, true
}

type zoneTableReader struct {
	data []byte
}

func (r *zoneTableReader) uvarint() (uint64, bool) {
	var value uint64
	for i := 0; i < 10 && i < len(r.data); i++ {
		b := r.data[i]
		if b < 0x80 {
			if i == 9 && b > 1 {
				return 0, false
			}
			r.data = r.data[i+1:]
			return value | uint64(b)<<uint(7*i), true
		}
		value |= uint64(b&0x7f) << uint(7*i)
	}
	return 0, false
}

func (r *zoneTableReader) varint() (int64, bool) {
	encoded, ok := r.uvarint()
	if !ok {
		return 0, false
	}
	value := int64(encoded >> 1)
	if encoded&1 != 0 {
		value = ^value
	}
	return value, true
}

func (r *zoneTableReader) bytes() ([]byte, bool) {
	length, ok := r.uvarint()
	if !ok || length > uint64(len(r.data)) {
		return nil, false
	}
	value := r.data[:length]
	r.data = r.data[length:]
	return value, true
}

func (r *zoneTableReader) string() (string, bool) {
	value, ok := r.bytes()
	return string(value), ok
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

// OffsetName writes an offset from Greenwich the way a language writes one.
// It takes minutes for the Date built-ins, whose legacy strings always write
// offsets at minute precision.
func OffsetName(locale string, offsetMinutes int, long bool) string {
	return OffsetNameSeconds(locale, offsetMinutes*60, long)
}

// OffsetNameSeconds writes an offset from Greenwich the way a language writes one:
// GMT-05:00 in English, UTC−05:00 in French, ‎−۰۵:۰۰ گرینویچ in Persian. The
// long form pads the hour to two digits and the short one does not, and the
// short one leaves off a whole hour's zero minutes. Historical offsets may
// also have seconds, which use the locale's minute separator once more.
func OffsetNameSeconds(locale string, offsetSeconds int, long bool) string {
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

	seconds, negative := offsetSeconds, false
	if seconds < 0 {
		seconds, negative = -seconds, true
	}
	minutes := seconds / 60
	second := seconds % 60
	// The four templates are the long form and the short one, each on either
	// side of Greenwich; the short form has a fifth and sixth for a whole
	// number of hours, which it writes without the minutes.
	at := 0
	switch {
	case long && negative:
		at = 1
	case long:
		at = 0
	case (minutes%60 != 0 || second != 0) && negative:
		at = 3
	case minutes%60 != 0 || second != 0:
		at = 2
	case negative:
		at = 5
	default:
		at = 4
	}
	template := piece(forms, ';', at)
	if second != 0 {
		template = offsetSecondsTemplate(template)
	}

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
		case "2":
			writeDigits(&b, second, true, digits)
		}
		template = after
	}
	return b.String()
}

// offsetSecondsTemplate extends an hour-and-minute GMT template with seconds.
// CLDR uses the same separator between every adjacent time field; keeping the
// insertion before any suffix also handles forms such as "{0}:{1} GMT".
func offsetSecondsTemplate(template string) string {
	hour := strings.Index(template, "{0}")
	minute := strings.Index(template, "{1}")
	if hour < 0 || minute < hour+3 {
		return template
	}
	separator := template[hour+3 : minute]
	return template[:minute+3] + separator + "{2}" + template[minute+3:]
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
