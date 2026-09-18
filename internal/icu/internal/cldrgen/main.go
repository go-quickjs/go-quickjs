// Command cldrgen writes the locale tables the icu package carries.
//
// The data is read out of an ICU that already has it, through the Intl objects
// of a JavaScript engine built with the full data -- node, as it happens --
// because Intl answers the question the tables are for: what does this locale
// actually produce. Reading CLDR's own files would mean reproducing the
// inheritance, the aliases and the pattern composition that ICU has already
// done, and getting any of it wrong would show up as a wrong month name rather
// than as an error.
//
// Usage:
//
//	go run ./internal/icu/internal/cldrgen > internal/icu/tables.go
//
// It needs node on the path, built with the full locale data, which is what an
// official build has. The version it read is recorded in the output, so that a
// table can be told from the data it came from.
package main

import (
	"bytes"
	"compress/flate"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"go/format"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// The separators the blobs are written with. They must match the constants in
// the icu package.
const (
	sectionSep = "\x1e"
	fieldSep   = "\x1f"
	itemSep    = "\x1d"
)

// extracted mirrors what extract.mjs writes.
type extracted struct {
	ICU     string       `json:"icu"`
	Locales []localeData `json:"locales"`
}

// localeData is one locale as it was read.
type localeData struct {
	Tag               string `json:"tag"`
	Numbering         string `json:"numbering"`
	Calendar          string `json:"calendar"`
	Hour12            bool   `json:"hour12"`
	DayPeriods        []string
	DateRange         string `json:"dateRange"`
	DateRangeRepeat   bool   `json:"dateRangeRepeat"`
	HourPeriods       []string
	HourPeriodsNarrow []string `json:"hourPeriodsNarrow"`
	Eras              []string
	Names             struct {
		Months, MonthsShort, MonthsNarrow []string
		MonthsAlone, MonthsAloneShort     []string
		Days, DaysShort, DaysNarrow       []string
	} `json:"names"`
	Dates     map[string]string `json:"dates"`
	Times     map[string]string `json:"times"`
	Both      map[string]string `json:"both"`
	Glue      map[string]string `json:"glue"`
	Skeletons map[string]string `json:"skeletons"`
	Numbers   struct {
		Decimal, Group, Minus, PercentSign string
		NaN                                string `json:"nan"`
		Infinity, Digits                   string
		Indian                             bool
		PercentPattern, CurrencyPattern    string
		DecimalNegative                    string `json:"decimalNegative"`
		PercentNegative                    string `json:"percentNegative"`
		CurrencyNegative                   string `json:"currencyNegative"`
		Accounting                         string `json:"accounting"`
		HourCycles                         string `json:"hourCycles"`
		Range                              string `json:"range"`
		Approximately                      string `json:"approximately"`
		Exponential                        string `json:"exponential"`
		MinGrouping                        int    `json:"minGrouping"`
	} `json:"numbers"`
	Compact    map[string]map[string]compactForm `json:"compact"`
	Currencies map[string]string                 `json:"currencies"`
	Plurals    struct {
		Cardinal plural `json:"cardinal"`
		Ordinal  plural `json:"ordinal"`
	} `json:"plurals"`
	Lists    map[string]listPattern  `json:"lists"`
	Relative map[string]relativeUnit `json:"relative"`
	// Tailoring is how this language sorts, filled in from tailoring.json
	// rather than by the extractor: where it puts the letters the root order
	// puts elsewhere, and the two things it may say besides.
	Tailoring tailoring `json:"-"`
}

// relativeUnit is how one unit of time is said in one locale.
type relativeUnit struct {
	Forms map[string]string `json:"forms"`
	Named map[string]string `json:"named"`
}

type plural struct {
	Categories []string `json:"categories"`
	Small      []string `json:"small"`
	Mod        []string `json:"mod"`
	// Classes are the residues that a rule treats differently, keyed by the
	// modulus and the residue: "100000:21000".
	Classes       map[string]string `json:"classes"`
	Exact         map[string]string `json:"exact"`
	FractionZero  string            `json:"fractionZero"`
	FractionOther string            `json:"fractionOther"`
	Disagrees     []int             `json:"disagrees"`
}

type listPattern struct {
	Pair, Start, Middle, End string
}

// compactForm is how one power of ten is shortened: what it is divided by, and
// what is written after it.
type compactForm struct {
	Divisor  int               `json:"divisor"`
	Suffixes map[string]string `json:"suffixes"`
}

// The currencies that are not written with two decimal places. CLDR keeps this
// in supplemental data rather than per locale, and it is short enough to carry
// as itself.
var currencyDigits = map[string]int{
	"BHD": 3, "BIF": 0, "CLF": 4, "CLP": 0, "DJF": 0, "GNF": 0, "IQD": 3,
	"ISK": 0, "JOD": 3, "JPY": 0, "KMF": 0, "KRW": 0, "KWD": 3, "LYD": 3,
	"OMR": 3, "PYG": 0, "RWF": 0, "TND": 3, "UGX": 0, "UYI": 0, "VND": 0,
	"VUV": 0, "XAF": 0, "XOF": 0, "XPF": 0,
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "cldrgen:", err)
		os.Exit(1)
	}
}

func run() error {
	here, err := os.Getwd()
	if err != nil {
		return err
	}
	// The names of everything in every language are written as a package of
	// their own, which a host imports when it wants them.
	if len(os.Args) > 1 && os.Args[1] == "-display" {
		return writeDisplayPackage(filepath.Join(here, "internal", "icu",
			"internal", "cldrgen"))
	}
	script := filepath.Join(here, "internal", "icu", "internal", "cldrgen", "extract.mjs")
	if _, err := os.Stat(script); err != nil {
		return fmt.Errorf("run this from the root of the repository: %w", err)
	}

	tags, err := listLocales(script)
	if err != nil {
		return err
	}
	data, skipped, err := extractAll(script, tags)
	if err != nil {
		return err
	}
	if len(skipped) > 0 {
		// A locale that brings the engine down is left out rather than left
		// half-read; it falls back to its language, as an unknown tag does.
		fmt.Fprintf(os.Stderr,
			"cldrgen: skipped %d locale(s) that crashed node: %s\n",
			len(skipped), strings.Join(skipped, " "))
	}

	// The plural model reproduces every rule in CLDR but one: Cornish asks
	// what a count is modulo a hundred thousand, which two tables of a hundred
	// cannot answer. Where that happens it is said out loud rather than
	// written out quietly, and the rule holds for every count below the one
	// named.
	for _, l := range data.Locales {
		for _, p := range []struct {
			name string
			rule plural
		}{{"cardinal", l.Plurals.Cardinal}, {"ordinal", l.Plurals.Ordinal}} {
			if len(p.rule.Disagrees) > 0 {
				fmt.Fprintf(os.Stderr,
					"cldrgen: %s %s plural rules are exact below %d\n",
					l.Tag, p.name, p.rule.Disagrees[0])
			}
		}
	}

	// Where each language puts its letters, which is read separately because
	// it is about the order rather than about the words.
	tailoring, err := readTailoring(filepath.Join(filepath.Dir(script), "tailoring.json"))
	if err != nil {
		return err
	}
	for i := range data.Locales {
		data.Locales[i].Tailoring = tailoring[data.Locales[i].Tag]
	}

	sort.Slice(data.Locales, func(i, j int) bool {
		return data.Locales[i].Tag < data.Locales[j].Tag
	})

	var b strings.Builder
	fmt.Fprintf(&b, "// Code generated by cldrgen; DO NOT EDIT.\n")
	fmt.Fprintf(&b, "//\n")
	fmt.Fprintf(&b, "// Locale data from CLDR, read through ICU %s.\n", data.ICU)
	fmt.Fprintf(&b, "// Regenerate with:\n")
	fmt.Fprintf(&b, "//\n")
	fmt.Fprintf(&b, "//\tgo run ./internal/icu/internal/cldrgen > internal/icu/tables.go\n\n")
	fmt.Fprintf(&b, "package icu\n\n")
	fmt.Fprintf(&b, "// cldrVersion is the ICU whose data this is.\n")
	fmt.Fprintf(&b, "const cldrVersion = %q\n\n", data.ICU)

	fmt.Fprintf(&b, "// tags are the locales carried here, sorted so that one can be found\n")
	fmt.Fprintf(&b, "// without a map to build at startup.\n")
	fmt.Fprintf(&b, "var tags = [...]string{\n")
	for _, l := range data.Locales {
		fmt.Fprintf(&b, "\t%q,\n", l.Tag)
	}
	fmt.Fprintf(&b, "}\n\n")

	// The locales' data, in one piece, compressed.
	//
	// Written out as itself it is more than a megabyte of Go source, which is
	// a megabyte in every binary that links this package whether or not
	// anything formats a date. Compressed it is an eighth of that, and
	// unpacking it costs a few milliseconds the first time a locale is asked
	// for -- and nothing at all to a program that never asks.
	var joined strings.Builder
	for _, l := range data.Locales {
		joined.WriteString(encode(&l))
		joined.WriteByte(0)
	}
	fmt.Fprintf(&b, "// packed is every locale's data, in the order of tags, separated by a\n")
	fmt.Fprintf(&b, "// zero byte, compressed, and written as text. It is %d bytes of data,\n",
		joined.Len())
	fmt.Fprintf(&b, "// and nothing is unpacked until a locale is asked for.\n")
	writePacked(&b, "packed", joined.String())

	// The variants that share another's data, which is how a hundred and fifty
	// tags are answered without carrying a hundred and fifty more tables.
	aliases, err := readAliases(filepath.Join(filepath.Dir(script), "locales.json"))
	if err != nil {
		return err
	}
	fmt.Fprintf(&b, "// aliases are the tags that say exactly what another tag says:\n")
	fmt.Fprintf(&b, "// ar-EG is written as ar-BH is, zh-TW as zh-Hant.\n")
	fmt.Fprintf(&b, "var aliases = map[string]string{\n")
	for _, tag := range sortedStringKeys(aliases) {
		fmt.Fprintf(&b, "\t%q: %q,\n", tag, aliases[tag])
	}
	fmt.Fprintf(&b, "}\n\n")

	// The time zones: which ones there are, and what they are called where
	// they are called anything but an offset.
	zones, err := readZones(filepath.Join(filepath.Dir(script), "zones.json"))
	if err != nil {
		return err
	}
	zoneNames, err := readZoneNames(filepath.Join(filepath.Dir(script), "zonenames.json"))
	if err != nil {
		return err
	}
	zoneTags := zoneLocales(zoneNames, tags)
	named := zoneNamesByLocale(zoneNames, zoneTags)
	// Every zone that has a name, which is the list of places plus Greenwich.
	namedZones := sortedIntValues(zoneNames.Groups)

	fmt.Fprintf(&b, "// zoneNames is what a zone is called in English: the long name for\n")
	fmt.Fprintf(&b, "// standard time and for summer time, then the short ones, then the two\n")
	fmt.Fprintf(&b, "// that do not depend on the time of year. A zone that is only ever\n")
	fmt.Fprintf(&b, "// called an offset from Greenwich is not here, since a clock can work\n")
	fmt.Fprintf(&b, "// that out; neither is the same in any other language, which is packed.\n")
	fmt.Fprintf(&b, "var zoneNames = map[string]string{\n")
	for _, zone := range namedZones {
		entry := strings.TrimRight(named["en"][zone], "|")
		if strings.Trim(entry, "|") == "" {
			continue
		}
		fmt.Fprintf(&b, "\t%q: %q,\n", zone, entry)
	}
	fmt.Fprintf(&b, "}\n\n")

	fmt.Fprintf(&b, "// zoneSeasonPacked is what a zone is called in every language but\n")
	fmt.Fprintf(&b, "// English, for standard time and for summer time. It is what a date\n")
	fmt.Fprintf(&b, "// written out ends with -- (Mitteleurop\u00e4ische Normalzeit) -- so the\n")
	fmt.Fprintf(&b, "// engine carries it; the names that only an explicit request reaches\n")
	fmt.Fprintf(&b, "// are three times the size and live in the intldata package.\n")
	writePacked(&b, "zoneSeasonPacked",
		encodeZoneNames(named, namedZones, zoneTags, seasonalSlots))

	fmt.Fprintf(&b, "// zoneOffsetForms is how a language writes an offset from Greenwich\n")
	fmt.Fprintf(&b, "// where a zone has no name of its own: the digits it counts in, then\n")
	fmt.Fprintf(&b, "// the long form either side of Greenwich, then the short one with\n")
	fmt.Fprintf(&b, "// minutes and without. English comes first, and is what a language\n")
	fmt.Fprintf(&b, "// nothing is known about is written as.\n")
	forms, formOf := zoneOffsetForms(zoneNames, zoneTags)
	fmt.Fprintf(&b, "var zoneOffsetForms = [...]string{\n")
	for _, form := range forms {
		fmt.Fprintf(&b, "\t%q,\n", form)
	}
	fmt.Fprintf(&b, "}\n\n")
	fmt.Fprintf(&b, "// zoneOffsetForm says which of those a language uses.\n")
	fmt.Fprintf(&b, "var zoneOffsetForm = map[string]int8{\n")
	for _, tag := range zoneTags {
		if at := formOf[tag]; at != 0 {
			fmt.Fprintf(&b, "\t%q: %d,\n", tag, at)
		}
	}
	fmt.Fprintf(&b, "}\n\n")

	fmt.Fprintf(&b, "// zoneAliases are the other names a zone goes by: Asia/Kolkata and\n")
	fmt.Fprintf(&b, "// Asia/Calcutta are one place, and the names are filed under one of them.\n")
	fmt.Fprintf(&b, "var zoneAliases = map[string]string{\n")
	for _, name := range sortedStringKeys(zones.Aliases) {
		fmt.Fprintf(&b, "\t%q: %q,\n", name, zones.Aliases[name])
	}
	fmt.Fprintf(&b, "}\n\n")

	fmt.Fprintf(&b, "// zoneList is every time zone this data knows of, which is what\n")
	fmt.Fprintf(&b, "// supportedValuesOf answers with -- filtered by the ones this machine\n")
	fmt.Fprintf(&b, "// can actually load.\n")
	fmt.Fprintf(&b, "var zoneList = [...]string{\n")
	for _, zone := range zones.Zones {
		fmt.Fprintf(&b, "\t%q,\n", zone)
	}
	fmt.Fprintf(&b, "}\n\n")

	// The order text sorts in: the three weights each character carries.
	collation, err := readCollation(filepath.Join(filepath.Dir(script), "collation.json"))
	if err != nil {
		return err
	}
	fmt.Fprintf(&b, "// collationPacked is the order text sorts in, compressed: the runs of\n")
	fmt.Fprintf(&b, "// characters whose letter weight advances with them, then the\n")
	fmt.Fprintf(&b, "// characters that carry an accent or a case weight, then the ones the\n")
	fmt.Fprintf(&b, "// collator ignores altogether.\n")
	writePacked(&b, "collationPacked", encodeCollation(collation))

	// What things are called, in English. The other languages are a package
	// away, because they are three megabytes and most programs never ask.
	display, err := readDisplay(filepath.Join(filepath.Dir(script), "display.json"))
	if err != nil {
		return err
	}
	fmt.Fprintf(&b, "// displayPacked is what languages, regions, scripts, currencies and\n")
	fmt.Fprintf(&b, "// the parts of a date are called, in English. The same for every other\n")
	fmt.Fprintf(&b, "// language is in the intldata package, which a host imports to have it.\n")
	writePacked(&b, "displayPacked", encodeDisplay(map[string][]string{
		"en": display["en"],
	}))

	// Where text may be broken: the classes and the pairs, for each of the
	// three sizes of piece.
	segments, err := readSegments(filepath.Join(filepath.Dir(script), "segment.json"))
	if err != nil {
		return err
	}
	// The digits of every numbering system, so that a tag may ask for digits
	// its language does not use.
	numbering, err := readNumbering(filepath.Join(filepath.Dir(script), "numbering.json"))
	if err != nil {
		return err
	}
	// How each language writes a measurement, which is a table of its own
	// because it is large and most programs never ask for one.
	units, err := readUnits(filepath.Join(filepath.Dir(script), "units.json"))
	if err != nil {
		return err
	}
	fmt.Fprintf(&b, "// unitsPacked is how each language writes a measurement: the name of the\n")
	fmt.Fprintf(&b, "// unit in each of the three widths, the form it takes with one and with\n")
	fmt.Fprintf(&b, "// many, and what it looks like written underneath another unit. Locales\n")
	fmt.Fprintf(&b, "// that write them all alike share one entry.\n")
	writePacked(&b, "unitsPacked", encodeUnits(units))

	// The names that have been replaced since.
	renames, err := readRenames(filepath.Join(filepath.Dir(script), "aliases.json"))
	if err != nil {
		return err
	}
	fmt.Fprintf(&b, "// tagAliases is what a name in a tag has been replaced by: a language\n")
	fmt.Fprintf(&b, "// renamed, a country dissolved, a variant folded into another, a setting\n")
	fmt.Fprintf(&b, "// that goes by another word now.\n")
	writePacked(&b, "tagAliases", encodeAliases(renames))

	// What the other calendars call their months and eras, and where the ones
	// that cannot be computed put their months.
	calendars, err := readCalendars(filepath.Join(filepath.Dir(script), "calendars.json"))
	if err != nil {
		return err
	}
	fmt.Fprintf(&b, "// calendarNames is what each calendar calls its months and its eras, in\n")
	fmt.Fprintf(&b, "// each language: the Islamic months, the Hebrew ones and the thirteenth\n")
	fmt.Fprintf(&b, "// it has in a long year, the two hundred and thirty-seven Japanese reigns.\n")
	writePacked(&b, "calendarNames", encodeCalendars(calendars))

	tables, err := readCalendarTables(filepath.Join(filepath.Dir(script), "calendartables.json"))
	if err != nil {
		return err
	}
	fmt.Fprintf(&b, "// calendarTables is where the calendars that cannot be computed put their\n")
	fmt.Fprintf(&b, "// months: the Islamic ones, which follow the moon or a table kept in\n")
	fmt.Fprintf(&b, "// Saudi Arabia, and the Persian one, whose year begins at the equinox.\n")
	fmt.Fprintf(&b, "// Along with them, the day each Japanese reign began.\n")
	writePacked(&b, "calendarTables", encodeCalendarTables(tables))

	fmt.Fprintf(&b, "// numberingSystems is the ten digits of each way of writing numbers.\n")
	fmt.Fprintf(&b, "var numberingSystems = map[string]string{\n")
	for _, name := range sortedNames(numbering) {
		fmt.Fprintf(&b, "\t%q: %q,\n", name, numbering[name])
	}
	fmt.Fprintf(&b, "}\n\n")

	fmt.Fprintf(&b, "// segmentPacked is where text may be broken: for graphemes, words and\n")
	fmt.Fprintf(&b, "// sentences, which class each character belongs to and which pairs of\n")
	fmt.Fprintf(&b, "// classes a break may fall between, and then the emoji.\n")
	writePacked(&b, "segmentPacked", encodeSegments(segments))

	fmt.Fprintf(&b, "// currencyDigits is how many decimal places a currency is written with,\n")
	fmt.Fprintf(&b, "// where that is not the usual two.\n")
	fmt.Fprintf(&b, "var currencyDigits = map[string]int8{\n")
	codes := make([]string, 0, len(currencyDigits))
	for code := range currencyDigits {
		codes = append(codes, code)
	}
	sort.Strings(codes)
	for _, code := range codes {
		fmt.Fprintf(&b, "\t%q: %d,\n", code, currencyDigits[code])
	}
	fmt.Fprintf(&b, "}\n")

	// The output is formatted here rather than left to whoever regenerates it,
	// so that the file in the repository is the file this writes.
	pretty, err := format.Source([]byte(b.String()))
	if err != nil {
		return fmt.Errorf("formatting what was generated: %w", err)
	}
	_, err = os.Stdout.Write(pretty)
	return err
}

// sortedNames is the keys of a map, in order, so that what is written out is
// the same from one run to the next.
func sortedNames(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// unitData is what units.mjs writes.
type unitData struct {
	Units map[string]map[string]string `json:"units"`
	Same  map[string]string            `json:"same"`
}

func readUnits(path string) (unitData, error) {
	var out unitData
	raw, err := os.ReadFile(path)
	if err != nil {
		return out, fmt.Errorf("the units: %w", err)
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return out, fmt.Errorf("the units: %w", err)
	}
	return out, nil
}

// encodeUnits writes the blocks and then which locale uses which.
func encodeUnits(d unitData) string {
	var b strings.Builder
	for _, name := range sortedBlocks(d.Units) {
		fmt.Fprintf(&b, "%s\n", name)
		block := d.Units[name]
		for _, key := range sortedNames(block) {
			fmt.Fprintf(&b, "%s\t%s\n", key, block[key])
		}
	}
	b.WriteString("\n")
	for _, tag := range sortedNames(d.Same) {
		fmt.Fprintf(&b, "%s\t%s\n", tag, d.Same[tag])
	}
	return b.String()
}

func sortedBlocks(m map[string]map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// aliasData is what aliases.mjs writes.
type aliasData struct {
	Languages         map[string]string `json:"languages"`
	Regions           map[string]string `json:"regions"`
	RegionsByLanguage map[string]string `json:"regionsByLanguage"`
	Scripts           map[string]string `json:"scripts"`
	Grandfathered     map[string]string `json:"grandfathered"`
	Variants          map[string]string `json:"variants"`
	Settings          map[string]string `json:"settings"`
}

func readRenames(path string) (aliasData, error) {
	var out aliasData
	raw, err := os.ReadFile(path)
	if err != nil {
		return out, fmt.Errorf("the aliases: %w", err)
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return out, fmt.Errorf("the aliases: %w", err)
	}
	return out, nil
}

// encodeAliases writes each kind of name on a line of its own.
func encodeAliases(d aliasData) string {
	var b strings.Builder
	for _, table := range []map[string]string{
		d.Languages, d.Regions, d.Scripts, d.Grandfathered, d.Variants,
		d.Settings, d.RegionsByLanguage,
	} {
		for _, from := range sortedNames(table) {
			// The replacements come back as whole tags: "und-MM" is the region
			// MM, and what is wanted is the part that replaces the name.
			to := strings.TrimPrefix(table[from], "und-")
			if to == "und" {
				to = ""
			}
			fmt.Fprintf(&b, "%s=%s;", from, to)
		}
		b.WriteByte('\n')
	}
	return b.String()
}

// calendarData is what calendars.mjs writes.
type calendarData struct {
	Entries []map[string]string       `json:"entries"`
	Index   map[string]map[string]int `json:"index"`
}

func readCalendars(path string) (calendarData, error) {
	var out calendarData
	raw, err := os.ReadFile(path)
	if err != nil {
		return out, fmt.Errorf("the calendar names: %w", err)
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return out, fmt.Errorf("the calendar names: %w", err)
	}
	return out, nil
}

// encodeCalendars writes the distinct sets of names, then which locale uses
// which for which calendar.
func encodeCalendars(d calendarData) string {
	var b strings.Builder
	for _, entry := range d.Entries {
		parts := make([]string, 0, len(entry))
		for _, key := range sortedNames(entry) {
			parts = append(parts, key+"\t"+entry[key])
		}
		fmt.Fprintf(&b, "%s\n", strings.Join(parts, "\x01"))
	}
	b.WriteString("\n")
	for _, tag := range sortedIndex(d.Index) {
		parts := make([]string, 0, len(d.Index[tag]))
		for _, calendar := range sortedInts(d.Index[tag]) {
			parts = append(parts, calendar+"="+strconv.Itoa(d.Index[tag][calendar]))
		}
		fmt.Fprintf(&b, "%s\t%s\n", tag, strings.Join(parts, "\x01"))
	}
	return b.String()
}

func sortedIndex(m map[string]map[string]int) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func sortedInts(m map[string]int) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// calendarTableData is what calendartables.mjs writes.
type calendarTableData struct {
	Islamic  monthTable `json:"islamic"`
	UmAlQura monthTable `json:"islamic-umalqura"`
	Persian  monthTable `json:"persian"`
	Chinese  monthTable `json:"chinese"`
	Dangi    monthTable `json:"dangi"`
	Eras     []eraStart `json:"eras"`
}

type monthTable struct {
	From    int    `json:"from"`
	Year    int    `json:"year"`
	Month   string `json:"month"`
	Lengths string `json:"lengths"`
}

type eraStart struct {
	Fixed int    `json:"fixed"`
	Era   string `json:"era"`
}

func readCalendarTables(path string) (calendarTableData, error) {
	var out calendarTableData
	raw, err := os.ReadFile(path)
	if err != nil {
		return out, fmt.Errorf("the calendar tables: %w", err)
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return out, fmt.Errorf("the calendar tables: %w", err)
	}
	return out, nil
}

// encodeCalendarTables writes one calendar to a line: where its first month
// begins, which month that is, and how long each month from there is.
func encodeCalendarTables(d calendarTableData) string {
	var b strings.Builder
	for _, entry := range []struct {
		name  string
		table monthTable
	}{
		{"islamic", d.Islamic}, {"islamic-umalqura", d.UmAlQura},
		{"persian", d.Persian}, {"chinese", d.Chinese}, {"dangi", d.Dangi},
	} {
		fmt.Fprintf(&b, "%s\t%d\t%d\t%s\t%s\n", entry.name, entry.table.From,
			entry.table.Year, entry.table.Month, entry.table.Lengths)
	}
	days := make([]string, 0, len(d.Eras))
	for _, era := range d.Eras {
		days = append(days, strconv.Itoa(era.Fixed))
	}
	fmt.Fprintf(&b, "eras\t%s\n", strings.Join(days, ","))
	return b.String()
}

// readNumbering reads the digits of every numbering system.
func readNumbering(path string) (map[string]string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("the numbering systems: %w", err)
	}
	var out struct {
		Systems map[string]string `json:"systems"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("the numbering systems: %w", err)
	}
	return out.Systems, nil
}

// segmentData is what segment.mjs writes.
type segmentData struct {
	Grapheme     granularity `json:"grapheme"`
	Word         granularity `json:"word"`
	Sentence     granularity `json:"sentence"`
	Pictographic [][2]int    `json:"pictographic"`
}

type granularity struct {
	Table    [][]int  `json:"table"`
	Runs     [][3]int `json:"runs"`
	WordLike []int    `json:"wordLike"`
}

func readSegments(path string) (segmentData, error) {
	var out segmentData
	raw, err := os.ReadFile(path)
	if err != nil {
		return out, fmt.Errorf("the break classes: %w", err)
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return out, fmt.Errorf("the break classes: %w", err)
	}
	return out, nil
}

// encodeSegments writes the classes and the pairs, a granularity at a time.
func encodeSegments(d segmentData) string {
	var b strings.Builder
	for _, g := range []granularity{d.Grapheme, d.Word, d.Sentence} {
		for _, run := range g.Runs {
			fmt.Fprintf(&b, "%x,%x,%x;", run[0], run[1], run[2])
		}
		b.WriteByte('\n')
		for _, row := range g.Table {
			for _, cell := range row {
				if cell != 0 {
					b.WriteByte('1')
				} else {
					b.WriteByte('0')
				}
			}
			b.WriteByte(';')
		}
		b.WriteByte('\n')
	}
	for _, run := range d.Pictographic {
		fmt.Fprintf(&b, "%x,%x;", run[0], run[1])
	}
	// And which of the word classes are made of letters rather than of spaces
	// and punctuation, for a caller counting words.
	b.WriteByte('\n')
	for _, ok := range d.Word.WordLike {
		if ok != 0 {
			b.WriteByte('1')
		} else {
			b.WriteByte('0')
		}
	}
	return b.String()
}

// readDisplay reads what things are called.
func readDisplay(path string) (map[string][]string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("the display names: %w", err)
	}
	var list struct {
		Names map[string][]string `json:"names"`
	}
	if err := json.Unmarshal(raw, &list); err != nil {
		return nil, fmt.Errorf("the display names: %w", err)
	}
	return list.Names, nil
}

// encodeDisplay writes the names as one record per locale.
func encodeDisplay(names map[string][]string) string {
	var b strings.Builder
	for _, tag := range sortedNameKeys(names) {
		b.WriteString(tag)
		for _, item := range names[tag] {
			b.WriteString(fieldSep)
			b.WriteString(item)
		}
		b.WriteString(sectionSep)
	}
	return b.String()
}

func sortedNameKeys(m map[string][]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// writeDisplayPackage writes the package that carries the names of everything
// in every language.
func writeDisplayPackage(dir string) error {
	names, err := readDisplay(filepath.Join(dir, "display.json"))
	if err != nil {
		return err
	}
	zoneNames, err := readZoneNames(filepath.Join(dir, "zonenames.json"))
	if err != nil {
		return err
	}
	var listed struct {
		Locales []string `json:"locales"`
	}
	raw, err := os.ReadFile(filepath.Join(dir, "locales.json"))
	if err != nil {
		return fmt.Errorf("the locale list: %w", err)
	}
	if err := json.Unmarshal(raw, &listed); err != nil {
		return fmt.Errorf("the locale list: %w", err)
	}
	zoneTags := zoneLocales(zoneNames, listed.Locales)
	named := zoneNamesByLocale(zoneNames, zoneTags)
	var b strings.Builder
	fmt.Fprintf(&b, "// Code generated by cldrgen; DO NOT EDIT.\n\n")
	fmt.Fprintf(&b, "// Package intldata carries what things are called in every language\n")
	fmt.Fprintf(&b, "// this engine knows: the languages, the regions, the scripts, the\n")
	fmt.Fprintf(&b, "// currencies, the parts of a date, and the time zones.\n")
	fmt.Fprintf(&b, "//\n")
	fmt.Fprintf(&b, "// Import it for its effect alone, and Intl.DisplayNames answers in the\n")
	fmt.Fprintf(&b, "// language it was asked in rather than in English, as does a zone\n")
	fmt.Fprintf(&b, "// asked for by a name that does not depend on the season:\n")
	fmt.Fprintf(&b, "//\n")
	fmt.Fprintf(&b, "//\timport _ \"github.com/go-quickjs/go-quickjs/intldata\"\n")
	fmt.Fprintf(&b, "//\n")
	fmt.Fprintf(&b, "// It is three megabytes of text, which is why it is a package rather\n")
	fmt.Fprintf(&b, "// than part of the engine: a program that never opens a language picker\n")
	fmt.Fprintf(&b, "// should not carry the name of every country in every language.\n")
	fmt.Fprintf(&b, "package intldata\n\n")
	fmt.Fprintf(&b, "import \"github.com/go-quickjs/go-quickjs/internal/icu\"\n\n")
	fmt.Fprintf(&b, "func init() {\n")
	fmt.Fprintf(&b, "\ticu.RegisterDisplayNames(packed)\n")
	fmt.Fprintf(&b, "\ticu.RegisterZoneNames(zonesPacked)\n")
	fmt.Fprintf(&b, "}\n\n")
	writePacked(&b, "packed", encodeDisplay(names))
	fmt.Fprintf(&b, "// zonesPacked is what a time zone is called where the name does not\n")
	fmt.Fprintf(&b, "// depend on the time of year: Mitteleurop\u00e4ische Zeit rather than\n")
	fmt.Fprintf(&b, "// Mitteleurop\u00e4ische Sommerzeit. Those two the engine carries\n")
	fmt.Fprintf(&b, "// itself, since a date written out uses them; these are reached only\n")
	fmt.Fprintf(&b, "// by asking for them, and are most of a megabyte.\n")
	writePacked(&b, "zonesPacked",
		encodeZoneNames(named, sortedIntValues(zoneNames.Groups), zoneTags, genericSlots))

	pretty, err := format.Source([]byte(b.String()))
	if err != nil {
		return fmt.Errorf("formatting what was generated: %w", err)
	}
	_, err = os.Stdout.Write(pretty)
	return err
}

// writePacked writes a string as a compressed constant, since the data here is
// text and text compresses: a megabyte in the source is an eighth of that in
// the binary, and nothing is unpacked until something asks for it.
func writePacked(b *strings.Builder, name, data string) {
	var packed bytes.Buffer
	w, err := flate.NewWriter(&packed, flate.BestCompression)
	if err != nil {
		panic(err)
	}
	if _, err := io.WriteString(w, data); err != nil {
		panic(err)
	}
	if err := w.Close(); err != nil {
		panic(err)
	}
	text := base64.StdEncoding.EncodeToString(packed.Bytes())
	fmt.Fprintf(b, "const %s = \"\" +\n", name)
	for i := 0; i < len(text); i += 100 {
		fmt.Fprintf(b, "\t%q", text[i:min(i+100, len(text))])
		if i+100 < len(text) {
			fmt.Fprintf(b, " +")
		}
		fmt.Fprintf(b, "\n")
	}
	fmt.Fprintf(b, "\n")
}

// readTailoring reads where each language moves its letters.
func readTailoring(path string) (map[string]tailoring, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("the tailorings: %w", err)
	}
	var list struct {
		Tailoring map[string]tailoring `json:"tailoring"`
	}
	if err := json.Unmarshal(raw, &list); err != nil {
		return nil, fmt.Errorf("the tailorings: %w", err)
	}
	return list.Tailoring, nil
}

// tailoring is how one language sorts, where that is not how the root order
// sorts.
type tailoring struct {
	Moved        [][3]any    `json:"moved"`
	Contractions [][2]string `json:"contractions"`
	UpperFirst   bool        `json:"upperFirst"`
	Shifted      bool        `json:"shifted"`
}

// collationData is what collation.mjs writes.
type collationData struct {
	Ignorable  []int             `json:"ignorable"`
	Marks      [][2]int          `json:"marks"`
	Weights    [][4]int          `json:"weights"`
	Expansions map[string]string `json:"expansions"`
}

func readCollation(path string) (collationData, error) {
	var out collationData
	raw, err := os.ReadFile(path)
	if err != nil {
		return out, fmt.Errorf("the collation order: %w", err)
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return out, fmt.Errorf("the collation order: %w", err)
	}
	return out, nil
}

// encodeCollation writes the order as three sections: the runs of characters
// whose letter weight advances with them, the accents and cases that are not
// the ordinary ones, and the characters that are ignored.
func encodeCollation(c collationData) string {
	var b strings.Builder

	// A run is a stretch where the code point and the letter weight both go up
	// by one, which is most of the table: the letters of a script are written
	// in the order they sort in.
	runStart, runWeight, runLength := -1, -1, 0
	flush := func() {
		if runStart < 0 {
			return
		}
		fmt.Fprintf(&b, "%x,%x,%x;", runStart, runLength, runWeight)
	}
	previous := [4]int{-2, -2}
	for _, w := range c.Weights {
		if w[0] == previous[0]+1 && w[1] == previous[1]+1 {
			runLength++
		} else {
			flush()
			runStart, runWeight, runLength = w[0], w[1], 1
		}
		previous = w
	}
	flush()
	b.WriteByte('\n')

	for _, w := range c.Weights {
		if w[2] == 0 && w[3] == 0 {
			continue
		}
		fmt.Fprintf(&b, "%x,%x,%x;", w[0], w[2], w[3])
	}
	b.WriteByte('\n')

	for _, cp := range c.Ignorable {
		fmt.Fprintf(&b, "%x;", cp)
	}
	b.WriteByte('\n')

	// The accents, which count for nothing as letters.
	for _, mark := range c.Marks {
		fmt.Fprintf(&b, "%x,%x;", mark[0], mark[1])
	}
	b.WriteByte('\n')

	// The characters that sort as several: œ as oe, ½ as 1⁄2.
	for _, key := range sortedStringKeys(c.Expansions) {
		cp, err := strconv.Atoi(key)
		if err != nil {
			continue
		}
		fmt.Fprintf(&b, "%x,%s;", cp, c.Expansions[key])
	}
	return b.String()
}

// zoneData is what zones.mjs writes: which zones there are, and the other
// names each of them goes by. What they are called is read separately, in
// every language, by zonenames.mjs.
type zoneData struct {
	Zones   []string          `json:"zones"`
	Aliases map[string]string `json:"aliases"`
}

func readZones(path string) (zoneData, error) {
	var out zoneData
	raw, err := os.ReadFile(path)
	if err != nil {
		return out, fmt.Errorf("the zone names: %w", err)
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return out, fmt.Errorf("the zone names: %w", err)
	}
	return out, nil
}

func readAliases(path string) (map[string]string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("the locale list: %w", err)
	}
	var list struct {
		Aliases map[string]string `json:"aliases"`
	}
	if err := json.Unmarshal(raw, &list); err != nil {
		return nil, fmt.Errorf("the locale list: %w", err)
	}
	return list.Aliases, nil
}

// listLocales asks the extractor which locales it knows about.
func listLocales(script string) ([]string, error) {
	out, err := exec.Command("node", script, "--list").Output()
	if err != nil {
		return nil, fmt.Errorf("asking node for the locale list: %w", err)
	}
	var tags []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			tags = append(tags, line)
		}
	}
	return tags, nil
}

// extractAll reads the locales in batches, and reports the ones it could not
// read at all.
//
// The engine the data is read from has a bug or two of its own -- a couple of
// locales make it abort -- so a batch that fails is retried one locale at a
// time and whatever survives is kept.
func extractAll(script string, tags []string) (extracted, []string, error) {
	const batch = 16
	var all extracted
	var skipped []string
	for i := 0; i < len(tags); i += batch {
		end := min(i+batch, len(tags))
		part, err := extractSome(script, tags[i:end])
		if err == nil {
			all.ICU = part.ICU
			all.Locales = append(all.Locales, part.Locales...)
			continue
		}
		for _, tag := range tags[i:end] {
			one, err := extractSome(script, []string{tag})
			if err != nil {
				skipped = append(skipped, tag)
				continue
			}
			all.ICU = one.ICU
			all.Locales = append(all.Locales, one.Locales...)
		}
	}
	if len(all.Locales) == 0 {
		return all, skipped, fmt.Errorf("no locale could be read")
	}
	return all, skipped, nil
}

func extractSome(script string, tags []string) (extracted, error) {
	var data extracted
	cmd := exec.Command("node", append([]string{script}, tags...)...)
	out, err := cmd.Output()
	if err != nil {
		return data, err
	}
	if err := json.Unmarshal(out, &data); err != nil {
		return data, fmt.Errorf("the extracted data: %w", err)
	}
	return data, nil
}

// encode writes one locale as the text the icu package reads.
func encode(l *localeData) string {
	flags := ""
	if l.Numbers.Indian {
		flags += "i"
	}
	if l.Hour12 {
		flags += "h"
	}
	if l.Numbers.MinGrouping == 2 {
		flags += "g"
	}
	if l.DateRangeRepeat {
		flags += "r"
	}

	head := strings.Join([]string{
		l.Numbering, l.Numbers.Decimal, l.Numbers.Group, l.Numbers.Minus,
		l.Numbers.PercentSign, l.Numbers.NaN, l.Numbers.Infinity, l.Numbers.Digits,
		l.Numbers.PercentPattern, l.Numbers.CurrencyPattern, flags,
		l.Numbers.DecimalNegative, l.Numbers.PercentNegative,
		l.Numbers.CurrencyNegative, l.Calendar,
		l.Numbers.Accounting, l.Numbers.Exponential,
		l.Numbers.Range, l.Numbers.Approximately, l.DateRange,
		l.Numbers.HourCycles,
	}, fieldSep)

	names := strings.Join([]string{
		strings.Join(l.Names.Months, itemSep),
		strings.Join(l.Names.MonthsShort, itemSep),
		strings.Join(l.Names.MonthsNarrow, itemSep),
		strings.Join(l.Names.Days, itemSep),
		strings.Join(l.Names.DaysShort, itemSep),
		strings.Join(l.Names.DaysNarrow, itemSep),
		strings.Join(l.DayPeriods, itemSep),
		strings.Join(l.Eras, itemSep),
		strings.Join(l.Names.MonthsAlone, itemSep),
		strings.Join(l.Names.MonthsAloneShort, itemSep),
		strings.Join(l.HourPeriods, itemSep),
		strings.Join(l.HourPeriodsNarrow, itemSep),
	}, fieldSep)

	widths := []string{"full", "long", "medium", "short"}
	byWidth := func(m map[string]string) string {
		out := make([]string, 0, 4)
		for _, w := range widths {
			out = append(out, m[w])
		}
		return strings.Join(out, itemSep)
	}
	patterns := strings.Join([]string{
		byWidth(l.Dates), byWidth(l.Times), byWidth(l.Glue),
		joinPairs(l.Skeletons),
	}, fieldSep)

	// Each table is a hundred categories, one letter each.
	letters := func(categories []string) string {
		var b strings.Builder
		for _, c := range categories {
			b.WriteString(letter(c))
		}
		return b.String()
	}
	plurals := func(p plural) string {
		return strings.Join([]string{
			strings.Join(p.Categories, itemSep),
			letters(p.Small),
			letters(p.Mod),
			joinClasses(p.Classes),
			joinNumbered(p.Exact),
			letter(p.FractionZero),
			letter(p.FractionOther),
		}, fieldSep)
	}

	lists := make([]string, 0, len(l.Lists))
	for _, name := range sortedKeys(l.Lists) {
		p := l.Lists[name]
		lists = append(lists, strings.Join(
			[]string{name, p.Pair, p.Start, p.Middle, p.End}, itemSep))
	}

	relative := make([]string, 0, len(l.Relative))
	for _, unit := range sortedKeysOf(l.Relative) {
		entry := l.Relative[unit]
		fields := []string{unit}
		for _, key := range sortedStringKeys(entry.Forms) {
			// past:one becomes p:one, future:one becomes f:one.
			short := strings.Replace(strings.Replace(key, "past:", "p:", 1),
				"future:", "f:", 1)
			fields = append(fields, short+"="+entry.Forms[key])
		}
		for _, key := range sortedStringKeys(entry.Named) {
			fields = append(fields, "n:"+key+"="+entry.Named[key])
		}
		relative = append(relative, strings.Join(fields, itemSep))
	}

	// The compact forms, as power:divisor:suffix items for each style.
	compact := func(style string) string {
		entries := l.Compact[style]
		powers := make([]int, 0, len(entries))
		for p := range entries {
			n, err := strconv.Atoi(p)
			if err != nil {
				continue
			}
			powers = append(powers, n)
		}
		sort.Ints(powers)
		out := make([]string, 0, len(powers))
		for _, power := range powers {
			form := entries[strconv.Itoa(power)]
			if len(form.Suffixes) == 0 && form.Divisor == 0 {
				continue
			}
			// Each plural form's suffix, with the letter that form is written
			// as: 6:6:x= Millionen,o= Million.
			forms := make([]string, 0, len(form.Suffixes))
			for _, category := range sortedStringKeys(form.Suffixes) {
				forms = append(forms, letter(category)+"="+form.Suffixes[category])
			}
			out = append(out, fmt.Sprintf("%d:%d:%s", power, form.Divisor,
				strings.Join(forms, ",")))
		}
		return strings.Join(out, itemSep)
	}

	return strings.Join([]string{
		head, names, patterns,
		joinPairs(l.Currencies),
		plurals(l.Plurals.Cardinal),
		plurals(l.Plurals.Ordinal),
		strings.Join(lists, fieldSep),
		strings.Join(relative, fieldSep),
		strings.Join([]string{compact("short"), compact("long")}, fieldSep),
		encodeTailoring(l.Tailoring),
	}, sectionSep)
}

// encodeTailoring writes where a language moves a letter to: the letter, the
// letter it sits after, and how far along.
func encodeTailoring(t tailoring) string {
	flags := ""
	if t.UpperFirst {
		flags += "u"
	}
	if t.Shifted {
		flags += "s"
	}
	if len(t.Moved) == 0 && flags == "" {
		return ""
	}
	out := []string{flags}
	// The letters written as two or three: cs=ch after h.
	for _, pair := range t.Contractions {
		if pair[0] == "" || pair[1] == "" {
			continue
		}
		// The mark is not a hex digit, or a letter whose code point begins
		// with one would be read as a spelling.
		out = append(out, fmt.Sprintf("*%s:%x:1", pair[0], []rune(pair[1])[0]))
	}
	for _, move := range t.Moved {
		letter, ok1 := move[0].(string)
		anchor, ok2 := move[1].(string)
		offset, ok3 := move[2].(float64)
		if !ok1 || !ok2 || !ok3 || letter == "" || anchor == "" {
			continue
		}
		out = append(out, fmt.Sprintf("%x:%x:%x",
			[]rune(letter)[0], []rune(anchor)[0], int(offset)))
	}
	return strings.Join(out, itemSep)
}

// letter is the single character a plural category is written as.
func letter(category string) string {
	if category == "" {
		return "x"
	}
	switch category {
	case "zero":
		return "z"
	case "one":
		return "o"
	case "two":
		return "t"
	case "few":
		return "f"
	case "many":
		return "m"
	}
	return "x"
}

func joinPairs(m map[string]string) string {
	if len(m) == 0 {
		return ""
	}
	out := make([]string, 0, len(m))
	for _, k := range sortedStringKeys(m) {
		out = append(out, k+"="+m[k])
	}
	return strings.Join(out, itemSep)
}

// joinClasses writes the residue classes, each as its modulus, its residue and
// its category: 100000:21000x.
func joinClasses(m map[string]string) string {
	if len(m) == 0 {
		return ""
	}
	out := make([]string, 0, len(m))
	for _, key := range sortedStringKeys(m) {
		out = append(out, key+letter(m[key]))
	}
	return strings.Join(out, itemSep)
}

// joinNumbered writes the plural exceptions, each as its number and its
// category: 100o.
func joinNumbered(m map[string]string) string {
	if len(m) == 0 {
		return ""
	}
	keys := make([]int, 0, len(m))
	for k := range m {
		n, err := strconv.Atoi(k)
		if err != nil {
			continue
		}
		keys = append(keys, n)
	}
	sort.Ints(keys)
	out := make([]string, 0, len(keys))
	for _, n := range keys {
		out = append(out, strconv.Itoa(n)+letter(m[strconv.Itoa(n)]))
	}
	return strings.Join(out, itemSep)
}

func sortedStringKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func sortedKeys(m map[string]listPattern) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func sortedKeysOf(m map[string]relativeUnit) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// quote writes a Go string literal, with the separators shown as escapes so
// that the table can be read.
func quote(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '\x1e':
			b.WriteString(`\x1e`)
		case '\x1f':
			b.WriteString(`\x1f`)
		case '\x1d':
			b.WriteString(`\x1d`)
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		case '\t':
			b.WriteString(`\t`)
		default:
			if r < 0x20 || r == 0x7f {
				fmt.Fprintf(&b, `\u%04x`, r)
				continue
			}
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

// ---------------------------------------------------------------------------
// What the zones are called
// ---------------------------------------------------------------------------

// zoneNameData is what zonenames.mjs writes: the six names each zone has in
// each language, read at two instants half a year apart.
type zoneNameData struct {
	StandIn  []string       `json:"standIn"`
	Groups   map[string]int `json:"groups"`
	Blocks   map[string]int `json:"blocks"`
	Names    [][]string     `json:"names"`
	GMT      map[string]int `json:"gmt"`
	GMTForms []string       `json:"gmtForms"`
}

func readZoneNames(path string) (zoneNameData, error) {
	var out zoneNameData
	raw, err := os.ReadFile(path)
	if err != nil {
		return out, fmt.Errorf("what the zones are called: %w", err)
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return out, fmt.Errorf("what the zones are called: %w", err)
	}
	return out, nil
}

// zoneProbes are the two instants the names were read at: mid-January and
// mid-July, which between them catch both halves of a year wherever the year
// is halved.
var zoneProbes = [2]time.Time{
	time.Date(2024, 1, 15, 12, 0, 0, 0, time.UTC),
	time.Date(2024, 7, 15, 12, 0, 0, 0, time.UTC),
}

// summerAt says, for each zone, which of the two probes was summer time. The
// language does not know this and the zone files do, which is why the names
// are read by season here rather than there.
func summerAt(zone string) (winter, summer bool) {
	loc, err := time.LoadLocation(zone)
	if err != nil {
		return false, false
	}
	return zoneProbes[0].In(loc).IsDST(), zoneProbes[1].In(loc).IsDST()
}

// bySeason files the two probes under standard time and summer time. A zone
// that keeps one kind of time all year is called one thing all year, so both
// slots are filled with it: which one the engine reaches for then depends on
// zone files that may have changed their mind since.
func bySeason(winterIsSummer, summerIsSummer bool, winter, summer string) (standard, daylight string) {
	switch {
	case winterIsSummer == summerIsSummer:
		if winter == "" {
			winter = summer
		}
		return winter, winter
	case winterIsSummer:
		return summer, winter
	default:
		return winter, summer
	}
}

// zoneNamesByLocale turns what was read into the six names each zone has in
// each language, in the order the engine reads them.
func zoneNamesByLocale(data zoneNameData, locales []string) map[string]map[string]string {
	seasons := make(map[string][2]bool, len(data.Groups))
	for zone := range data.Groups {
		w, s := summerAt(zone)
		seasons[zone] = [2]bool{w, s}
	}
	out := make(map[string]map[string]string, len(locales))
	for _, locale := range locales {
		block, ok := data.Blocks[locale]
		if !ok || block >= len(data.Names) {
			continue
		}
		row := data.Names[block]
		byZone := make(map[string]string, len(data.Groups))
		for zone, group := range data.Groups {
			if group >= len(row) {
				continue
			}
			slots := strings.SplitN(row[group], "|", 6)
			for len(slots) < 6 {
				slots = append(slots, "")
			}
			season := seasons[zone]
			longStd, longDay := bySeason(season[0], season[1], slots[0], slots[1])
			shortStd, shortDay := bySeason(season[0], season[1], slots[3], slots[4])
			byZone[zone] = strings.Join([]string{
				longStd, longDay, shortStd, shortDay, slots[2], slots[5],
			}, "|")
		}
		out[locale] = byZone
	}
	return out
}

// zoneSlots is which of the six names a table carries: the engine carries the
// ones a date written out uses, and the intldata package the rest.
type zoneSlots []int

var (
	seasonalSlots = zoneSlots{0, 1, 2, 3}
	genericSlots  = zoneSlots{4, 5}
)

// take reads the wanted names out of an entry, and drops the empty ones from
// the end since most zones have no name at all.
func (s zoneSlots) take(entry string) string {
	slots := strings.SplitN(entry, "|", 6)
	out := make([]string, 0, len(s))
	for _, at := range s {
		if at < len(slots) {
			out = append(out, slots[at])
		} else {
			out = append(out, "")
		}
	}
	for len(out) > 0 && out[len(out)-1] == "" {
		out = out[:len(out)-1]
	}
	return strings.Join(out, "|")
}

// encodeZoneNames packs the names of the zones in every language but English,
// which the engine carries in the source instead.
//
// The zones that are named alike everywhere share an entry -- most of them
// have no name at all -- and the languages that name every zone alike share a
// block, which is what turns eleven megabytes into one.
func encodeZoneNames(named map[string]map[string]string, zones, locales []string, slots zoneSlots) string {
	entryOf := func(locale, zone string) string {
		return slots.take(named[locale][zone])
	}

	groupAt := map[string]int{}
	groupOf := make(map[string]int, len(zones))
	var standIn []string
	for _, zone := range zones {
		var key strings.Builder
		for _, locale := range locales {
			key.WriteString(entryOf(locale, zone))
			key.WriteByte('\x01')
		}
		at, seen := groupAt[key.String()]
		if !seen {
			at = len(standIn)
			groupAt[key.String()] = at
			standIn = append(standIn, zone)
		}
		groupOf[zone] = at
	}

	rowOf := func(locale string) string {
		parts := make([]string, len(standIn))
		for i, zone := range standIn {
			parts[i] = entryOf(locale, zone)
		}
		return strings.Join(parts, "\x01")
	}
	english := rowOf("en")

	blockAt := map[string]int{}
	blockOf := map[string]int{}
	var rows []string
	for _, locale := range locales {
		row := rowOf(locale)
		// A language that names every zone the way English does is not
		// written down: the engine falls back to the English names, which are
		// the ones it has in the source.
		if row == english {
			continue
		}
		at, seen := blockAt[row]
		if !seen {
			at = len(rows)
			blockAt[row] = at
			rows = append(rows, row)
		}
		blockOf[locale] = at
	}

	var groups, blocks []string
	for _, zone := range zones {
		// A zone in the same group as the first one needs no entry of its
		// own, since nothing is filed under an index that is never asked for.
		groups = append(groups, fmt.Sprintf("%s=%d", zone, groupOf[zone]))
	}
	for _, locale := range locales {
		if at, ok := blockOf[locale]; ok {
			blocks = append(blocks, fmt.Sprintf("%s=%d", locale, at))
		}
	}
	sort.Strings(groups)
	sort.Strings(blocks)
	return strings.Join([]string{
		strings.Join(groups, ";"),
		strings.Join(blocks, ";"),
		strings.Join(rows, "\n"),
	}, "\n\n")
}

// zoneOffsetForms is the distinct ways the languages write an offset from
// Greenwich, with English first so that it is what anything unknown falls
// back to.
func zoneOffsetForms(data zoneNameData, locales []string) ([]string, map[string]int) {
	at := map[string]int{data.GMTForms[data.GMT["en"]]: 0}
	forms := []string{data.GMTForms[data.GMT["en"]]}
	out := make(map[string]int, len(locales))
	for _, locale := range locales {
		form := data.GMTForms[data.GMT[locale]]
		where, seen := at[form]
		if !seen {
			where = len(forms)
			at[form] = where
			forms = append(forms, form)
		}
		out[locale] = where
	}
	return forms, out
}

// sortedIntValues is the keys of a map of counts, in order.
func sortedIntValues(m map[string]int) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// zoneLocales is the languages the zone names were read in, once each and in
// the order they were asked for.
func zoneLocales(data zoneNameData, tags []string) []string {
	out := make([]string, 0, len(data.Blocks))
	seen := make(map[string]bool, len(data.Blocks))
	for _, tag := range tags {
		if _, ok := data.Blocks[tag]; !ok || seen[tag] {
			continue
		}
		seen[tag] = true
		out = append(out, tag)
	}
	return out
}
