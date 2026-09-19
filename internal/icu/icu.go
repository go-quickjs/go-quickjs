// Package icu carries the locale data that ECMA-402 needs: what a language
// does to a number, what it calls the months, how it orders a date, and which
// plural form a count takes.
//
// It is the data alone. What to do with it -- Intl.NumberFormat and its
// companions -- is the engine's business, and lives there.
//
// The data comes from CLDR, through the generator in internal/cldrgen, and is
// carried as directly indexed records that are read the first time a locale is
// asked for. A program that never formats anything pays nothing for it: the
// tables are constants in the binary, and each feature parses only its section
// of a locale record when something wants it.
//
// What is here is every locale that ICU has data of its own for -- each of the
// 250 languages it knows, and each region or script variant of one that says
// something its language does not -- together with the currencies that have
// symbols, the date patterns a program asks for, and the plural rules. A
// variant that says exactly what another says shares its data through an alias
// rather than repeating it, which is what keeps the table to a size worth
// carrying: a megabyte and a quarter of text, compressed to an eighth of that.
//
// A tag that is not here falls back to its language, and a language that is
// not here falls back to English -- which is what Resolve reports, so that
// resolvedOptions can say what was really used.
//
// Sorting is here too, in collate.go: the order the Unicode algorithm gives,
// out of a table of what each character weighs as a letter, as an accent and
// as a case, together with what each language changes about it -- where it
// puts its own letters, whether a capital comes first, and the letters it
// writes as two characters.
//
// Time-zone arithmetic comes from the IANA zone archive paired with the ICU
// release used to generate these tables. Localized CLDR names live in compact
// dictionaries here, including a historical metazone timeline that is decoded
// only when an older date needs it. Missing names fall back to a localized
// offset from Greenwich.
//
// The plural rules are stored as answers rather than as arithmetic: a hundred
// entries for the small counts, a hundred for what the last two digits say,
// and the residues that disagree at each of the moduli a rule may ask about.
// That reproduces every rule in CLDR exactly, Cornish included -- which counts
// in scores, and asks what a count is modulo a hundred thousand and modulo a
// million. The generator checks each language against the engine the data came
// from, for every count up to a hundred thousand, and up to three million for
// one that reaches that far.
package icu

import (
	"sort"
	"strconv"
	"strings"
	"sync"
	"unsafe"

	"github.com/klauspost/compress/zstd"
)

// Locale is everything known about one locale.
type Locale struct {
	// Tag is the locale this data is for, which may be less specific than the
	// tag that was asked for.
	Tag string
	// Numbering is the name of the digits, "latn" for the ASCII ones.
	Numbering string

	// Decimal and Group are the separators; Digits is the ten digits when they
	// are not the ASCII ones, and empty when they are.
	Decimal, Group, Minus, PercentSign, NaN, Infinity, Digits string
	// Indian says the separators fall after the first three digits and then
	// every two, as in 12,34,567.
	Indian bool
	// MinGrouping is how many digits the first group must have for there to be
	// a separator at all: two in Spanish and Italian, where 1234 is written
	// without one and 12.345 with one.
	MinGrouping int
	// PercentPattern and CurrencyPattern say where the sign goes, with {0}
	// standing for the number: "{0}%", "%{0}", "{0} €".
	PercentPattern, CurrencyPattern string
	// The same three for a negative number, since where the minus goes is the
	// language's business too: -€1.00, €-1.00, or a mark in front of both.
	DecimalNegative, PercentNegative, CurrencyNegative string
	// Accounting is how a loss is written where that is done differently: in
	// brackets rather than with a minus. Exponential is the mark that stands
	// between a number and its exponent.
	Accounting, Exponential string
	// Range is what stands between the two ends of a range of numbers, and
	// DateRange between two dates, which is not always the same mark.
	// Approximately says a number is not exact.
	Range, Approximately, DateRange string
	// DateRangeRepeat says a range of dates written in numbers is written out
	// twice rather than once with what the two have in common said once:
	// English says "1/1/2024 - 1/5/2024" and German says "01.-05.01.2024".
	DateRangeRepeat bool
	// HourCycle is the clock this language keeps, and HourCycle12 and
	// HourCycle24 the ones it keeps when a twelve-hour or a twenty-four-hour
	// clock is asked for: Japanese counts midnight as zero where English
	// counts it twelve.
	HourCycle, HourCycle12, HourCycle24 string
	// Calendar is the one this locale counts years in: "gregory" nearly
	// everywhere, "buddhist" in Thailand.
	Calendar string

	// Months are the names a date uses; MonthsAlone are the names the months
	// are called, which differ in the languages that decline them.
	Months, MonthsShort, MonthsNarrow []string
	MonthsAlone, MonthsAloneShort     []string
	Days, DaysShort, DaysNarrow       []string
	// DaysFormat are the forms used as part of a date. Empty entries fall back
	// to the corresponding stand-alone weekday names above.
	DaysFormat, DaysFormatShort, DaysFormatNarrow []string
	// DayPeriods is what the locale calls the two halves of the day, and Eras
	// what it calls the two eras.
	DayPeriods [2]string
	Eras       [2]string
	// HourPeriods is what it calls each hour of the day, where it has more to
	// say than morning and afternoon: Chinese distinguishes the small hours,
	// the early morning, noon and the evening. Empty where it does not.
	HourPeriods []string
	// HourPeriodsNarrow is the same in the shortest form the language writes,
	// which is usually the same words and now and then a letter.
	HourPeriodsNarrow []string
	// Hour12 says whether a time is written on a twelve-hour clock here.
	Hour12 bool

	// DatePatterns and TimePatterns are the four widths -- full, long, medium,
	// short -- in CLDR pattern letters, and Glue is what goes between them
	// when a format asks for both.
	DatePatterns [4]string
	TimePatterns [4]string
	Glue         [4]string
	// Skeletons are the patterns for the field combinations a program asks for
	// rather than a whole style: "yMd", "MMMd", "hm".
	Skeletons map[string]string

	// Currencies is the symbol for each currency that has one here.
	Currencies map[string]string
	// Lists is keyed by type and width: "conjunction-long".
	Lists map[string]ListPattern
	// Relative is keyed by unit: "day", "week".
	Relative map[string]RelativeUnit

	// Short and Long are how a large number is shortened, by the power of ten
	// it reaches: English counts in thousands, Japanese in ten-thousands.
	Short, Long map[int]CompactForm

	Cardinal, Ordinal PluralRule

	// Tailoring is where this language puts a letter that the root order puts
	// elsewhere: Swedish sorts å after z, and Azerbaijani writes I as the
	// capital of the dotless ı rather than of i. Every character of a letter
	// carries the same weight here, so that a letter moves as a letter. The
	// weights are on the scale the comparison uses, which leaves room between
	// the root ones.
	Tailoring map[rune]int32
	// Contractions are the letters this language writes as two or three
	// characters: Czech sorts ch after h, Danish aa after å. Keyed by the
	// lower-case spelling.
	Contractions map[string]int32
	// UpperFirst says a capital comes before its small letter here, which
	// Danish says and most languages do not. Shifted says punctuation is
	// passed over until everything else has been compared, which Thai says.
	UpperFirst, Shifted bool

	record                                              string
	dateOnce, currenciesOnce, cardinalOnce, ordinalOnce sync.Once
	listsOnce, relativeOnce, compactOnce, tailoringOnce sync.Once
	tailoringEncoded                                    string
}

// CompactForm is one step of a compact number: what the value is divided by,
// and what is written after it.
type CompactForm struct {
	Divisor int
	// Suffixes is what is written after the number, by plural form: a million
	// is "Million" in German and two are "Millionen".
	Suffixes map[byte]string
}

// ListPattern is how a language joins a list together.
type ListPattern struct {
	// Pair is the whole of a list of two, with {0} and {1} in it.
	Pair string
	// Start, Middle and End are what goes between the items of a longer list.
	Start, Middle, End string
}

// RelativeUnit is how a language says "in three days" and "three days ago".
type RelativeUnit struct {
	// Past and Future are keyed by plural category, with {0} for the count.
	Past, Future map[string]string
	// Named is what the language says instead of counting: "yesterday" for -1.
	Named map[int]string
}

// PluralRule says which form a count takes.
//
// A rule asks three kinds of question -- whether the count is exactly one of a
// few small values, what its last two digits are, and, in a few languages,
// what its last three are -- so it is stored as the answers rather than as an
// expression to evaluate.
type PluralRule struct {
	// Categories are the forms this locale has, in the order CLDR lists them.
	Categories []string
	// Small is the category of each count below a hundred, and Mod of each
	// remainder above it.
	Small, Mod [100]byte
	// Classes are the residues a rule treats differently, by modulus: Cornish
	// asks what a count is modulo a hundred thousand, and modulo a million.
	// Exact is for a count that no class covers.
	Classes map[int]map[int]byte
	Exact   map[int]byte
	// CompactExponents are the categories selected by CLDR's compact-decimal
	// exponent operand when that exponent alone decides the answer.
	CompactExponents map[int]byte
	// FractionZero is the category of a count with a fraction and nothing in
	// front of the point, FractionOther of one with something.
	FractionZero, FractionOther byte
}

// Category reports which plural form a count takes.
func (p *PluralRule) Category(n float64) string {
	return categoryNames[p.category(n)]
}

func (p *PluralRule) category(n float64) byte {
	if n < 0 {
		n = -n
	}
	whole := int(n)
	if n != float64(whole) {
		// A count with a fraction, which most languages treat as one case.
		if whole == 0 {
			return p.FractionZero
		}
		return p.FractionOther
	}
	if whole < 100 {
		return p.Small[whole]
	}
	if c, ok := p.Exact[whole]; ok {
		return c
	}
	// Each recorded class holds only counts that agree, so whichever matches
	// is the answer.
	for _, modulus := range classModuli {
		table, ok := p.Classes[modulus]
		if !ok {
			continue
		}
		if c, ok := table[whole%modulus]; ok {
			return c
		}
	}
	return p.Mod[whole%100]
}

// classModuli are the moduli a rule may ask about, beyond the last two digits.
var classModuli = [...]int{1000000, 100000, 1000}

// The categories, as the single letters the tables are written in.
var categoryNames = map[byte]string{
	'z': "zero", 'o': "one", 't': "two", 'f': "few", 'm': "many", 'x': "other",
}

// Resolve returns the data for a tag, and the tag it settled on.
//
// A tag with a region falls back to the plain language, and a language that is
// not carried falls back to English: a program is better served by English
// than by nothing, so long as it is told which it got.
func Resolve(tag string) *Locale {
	return get(ResolveTag(tag))
}

// ResolveTag returns only the tag Resolve would settle on. Callers that need
// a name for table lookup can avoid decoding the locale's formatting data.
func ResolveTag(tag string) string {
	tag = strings.TrimSpace(tag)
	if tag == "" {
		return "en"
	}
	// The forms a tag is written in: en_GB, EN-gb, en-GB-u-ca-gregory.
	tag = strings.ReplaceAll(tag, "_", "-")
	// A tag that says exactly what another says is answered with that one's
	// data rather than with a copy of it.
	if to, ok := aliases[canonicalCase(tag)]; ok {
		tag = to
	}
	parts := strings.Split(tag, "-")
	language := strings.ToLower(parts[0])

	// The most specific form first: language-Script-Region, then
	// language-Region or language-Script, then the language alone.
	var region, script string
	for _, part := range parts[1:] {
		switch {
		case len(part) == 4 && script == "":
			script = strings.Title(strings.ToLower(part)) //nolint:staticcheck // ASCII tags
		case (len(part) == 2 || len(part) == 3) && region == "":
			region = strings.ToUpper(part)
		case part == "u" || part == "x":
			// An extension, which says nothing about which data to use.
		}
		if part == "u" || part == "x" {
			break
		}
	}
	for _, candidate := range []string{
		language + "-" + script + "-" + region,
		language + "-" + script,
		language + "-" + region,
		language,
	} {
		if strings.Contains(candidate, "--") || strings.HasSuffix(candidate, "-") {
			continue
		}
		if i := indexOf(candidate); i >= 0 {
			return tags[i]
		}
	}
	return "en"
}

// canonicalCase writes a tag the way the tables spell one: the language in
// lower case, a script capitalised, a region in upper case.
func canonicalCase(tag string) string {
	parts := strings.Split(tag, "-")
	for i, part := range parts {
		switch {
		case i == 0:
			parts[i] = strings.ToLower(part)
		case len(part) == 4:
			parts[i] = strings.ToUpper(part[:1]) + strings.ToLower(part[1:])
		case len(part) == 2 || len(part) == 3:
			parts[i] = strings.ToUpper(part)
		}
	}
	return strings.Join(parts, "-")
}

// Has reports whether a tag resolves to data of its own rather than to
// English, which is what supportedLocalesOf is asking.
func Has(tag string) bool {
	l := Resolve(tag)
	if l == nil {
		return false
	}
	want := strings.ToLower(strings.SplitN(strings.ReplaceAll(tag, "_", "-"), "-", 2)[0])
	return strings.EqualFold(l.Tag, tag) || strings.HasPrefix(strings.ToLower(l.Tag), want)
}

// Tags lists every locale carried here.
func Tags() []string {
	out := make([]string, len(tags))
	copy(out, tags[:])
	return out
}

// Zones lists the time zones the data knows of.
func Zones() []string {
	available := make(map[string]bool, len(zoneList)+27)
	for _, zone := range zoneList {
		available[zone] = true
	}
	for hours := 1; hours <= 12; hours++ {
		available["Etc/GMT+"+strconv.Itoa(hours)] = true
	}
	for hours := 1; hours <= 14; hours++ {
		available["Etc/GMT-"+strconv.Itoa(hours)] = true
	}
	available["UTC"] = true
	out := make([]string, 0, len(available))
	for zone := range available {
		out = append(out, zone)
	}
	sort.Strings(out)
	return out
}

var dateTimeWarmupOnce sync.Once

// WarmupDateTimeData eagerly loads all locale, calendar, and time-zone data
// used by Date strings and Intl.DateTimeFormat.
func WarmupDateTimeData() {
	dateTimeWarmupOnce.Do(func() {
		TagAliases()
		CanonicalZone("UTC")
		for _, tag := range tags {
			if l := get(tag); l != nil {
				l.PrepareDate()
			}
		}
		warmupCalendarData()
		seasonNames.warmup()
		genericNames.warmup()
		historicalNames.warmup()
		legacyNames.warmup()
	})
}

var intlWarmupOnce sync.Once

// WarmupIntlData eagerly materializes every process-wide Intl dataset. It is
// intended for servers that prefer a predictable startup cost to first-use
// latency. Applications that use only date formatting should call the smaller
// WarmupDateTimeData instead.
func WarmupIntlData() {
	intlWarmupOnce.Do(func() {
		WarmupDateTimeData()
		loadLocaleInfo()
		loadUnits()
		loadSegments()
		for _, dictionary := range []*breakDictionary{
			&cjkBreakDictionary, &thaiBreakDictionary, &laoBreakDictionary,
			&khmerBreakDictionary, &burmeseBreakDictionary,
		} {
			dictionary.load()
		}
		_ = order()
		warmupCJKOrders()
		warmupDisplayNames()
		for _, tag := range tags {
			l := get(tag)
			if l == nil {
				continue
			}
			l.prepareCurrencies()
			_ = l.CardinalRule()
			_ = l.OrdinalRule()
			_, _ = l.ListPatternFor("")
			_, _ = l.RelativeUnitFor("")
			_, _, _ = l.Compact(0, false)
			l.loadTailoring()
		}
	})
}

// Currencies lists the currencies that have both NumberFormat data and an
// English DisplayNames entry, which is what supportedValuesOf is asking for.
func Currencies() []string {
	l := Resolve("en")
	l.prepareCurrencies()
	available := make(map[string]bool, len(l.Currencies))
	for code := range l.Currencies {
		if _, ok := DisplayName("en", DisplayCurrency, code); ok {
			available[code] = true
		}
	}
	for key := range displayNamesFor("en") {
		if len(key) == 4 && strings.HasPrefix(key, DisplayCurrency) {
			available[key[1:]] = true
		}
	}
	// XCD uses the currency code itself as its compact symbol, so it has no
	// separate record in either compact table.
	available["XCD"] = true
	out := make([]string, 0, len(available))
	for code := range available {
		out = append(out, code)
	}
	sort.Strings(out)
	return out
}

// CurrencyDigits reports how many decimal places a currency is written with.
func CurrencyDigits(code string) int {
	if n, ok := currencyDigits[code]; ok {
		return int(n)
	}
	return 2
}

// get parses one directly indexed locale record, once.
var (
	decodedMu sync.Mutex
	decoded   = map[string]*Locale{}
)

func get(tag string) *Locale {
	i := indexOf(tag)
	if i < 0 {
		return nil
	}
	decodedMu.Lock()
	defer decodedMu.Unlock()
	if l, ok := decoded[tags[i]]; ok {
		return l
	}
	blob, ok := localeRecord(i)
	if !ok {
		return nil
	}
	l := decode(tags[i], blob)
	decoded[tags[i]] = l
	return l
}

// unpack is retained for table-wide validation. Normal locale lookup takes a
// zero-copy string view of its directly indexed record.
var (
	unpackOnce sync.Once
	unpacked   []string
)

func unpack() []string {
	unpackOnce.Do(func() {
		unpacked = make([]string, len(packedLocales))
		for i := range packedLocales {
			text, ok := localeRecord(i)
			if !ok {
				unpacked = nil
				return
			}
			unpacked[i] = text
		}
	})
	return unpacked
}

func localeRecord(index int) (string, bool) {
	if index < 0 || index >= len(packedLocales) {
		return "", false
	}
	bounds := packedLocales[index]
	if bounds[0] > bounds[1] || uint64(bounds[1]) > uint64(len(packedTables)) {
		return "", false
	}
	data := packedTables[bounds[0]:bounds[1]]
	return rawString(data), true
}

// rawString returns a zero-copy view of immutable generated binary data.
func rawString(data []byte) string {
	if len(data) == 0 {
		return ""
	}
	return unsafe.String(unsafe.SliceData(data), len(data))
}

// inflate reads one of the compressed tables.
func inflate(packed []byte) (string, error) {
	return inflateSize(packed, 0)
}

var (
	packedDecoderOnce sync.Once
	packedDecoder     *zstd.Decoder
	packedDecoderErr  error
)

// inflateSize reserves the known output size for large generated tables.
// The decoder itself is initialized lazily so a program that never uses Intl
// pays no startup cost for locale data.
func inflateSize(packed []byte, size int) (string, error) {
	packedDecoderOnce.Do(func() {
		packedDecoder, packedDecoderErr = zstd.NewReader(nil,
			zstd.WithDecoderConcurrency(1))
	})
	if packedDecoderErr != nil {
		return "", packedDecoderErr
	}
	var destination []byte
	if size > 0 {
		destination = make([]byte, 0, size)
	}
	out, err := packedDecoder.DecodeAll(packed, destination)
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// packedBlockTable keeps cold records in moderately sized compressed blocks.
// A lookup inflates one block and retains it, preserving compression across
// neighboring records without making first use pay for the complete dataset.
type packedBlockTable struct {
	packed  []byte
	blocks  [][2]uint32
	once    []sync.Once
	decoded []string
	valid   []bool
}

func newPackedBlockTable(packed []byte, blocks [][2]uint32) *packedBlockTable {
	return &packedBlockTable{
		packed: packed, blocks: blocks,
		once: make([]sync.Once, len(blocks)), decoded: make([]string, len(blocks)),
		valid: make([]bool, len(blocks)),
	}
}

func (t *packedBlockTable) record(ref [3]uint32) (string, bool) {
	if t == nil || uint64(ref[0]) >= uint64(len(t.blocks)) || ref[1] > ref[2] {
		return "", false
	}
	block := int(ref[0])
	t.once[block].Do(func() {
		bounds := t.blocks[block]
		if bounds[0] > bounds[1] || uint64(bounds[1]) > uint64(len(t.packed)) {
			return
		}
		text, err := inflate(t.packed[bounds[0]:bounds[1]])
		if err == nil {
			t.decoded[block] = text
			t.valid[block] = true
		}
	})
	if !t.valid[block] {
		return "", false
	}
	text := t.decoded[block]
	if uint64(ref[2]) > uint64(len(text)) {
		return "", false
	}
	return text[ref[1]:ref[2]], true
}

// indexOf finds a tag in the sorted table.
func indexOf(tag string) int {
	lo, hi := 0, len(tags)
	for lo < hi {
		mid := (lo + hi) / 2
		switch {
		case tags[mid] == tag:
			return mid
		case tags[mid] < tag:
			lo = mid + 1
		default:
			hi = mid
		}
	}
	// A tag is looked up in the case it was written in as well, since the
	// table holds them the way CLDR spells them.
	for i, t := range tags {
		if strings.EqualFold(t, tag) {
			return i
		}
	}
	return -1
}

// The separators the blobs are written with: one between the sections, one
// between the fields of a section, and one between the items of a field.
const (
	sectionSep = "\x1e"
	fieldSep   = "\x1f"
	itemSep    = "\x1d"
)

func decode(tag, blob string) *Locale {
	l := &Locale{Tag: tag, record: blob}
	head := l.sectionFields(0)
	field := func(i int) string {
		if i < len(head) {
			return head[i]
		}
		return ""
	}
	l.Numbering = field(0)
	l.Decimal, l.Group, l.Minus = field(1), field(2), field(3)
	l.PercentSign, l.NaN, l.Infinity, l.Digits = field(4), field(5), field(6), field(7)
	l.PercentPattern, l.CurrencyPattern = field(8), field(9)
	l.Indian = strings.Contains(field(10), "i")
	l.Hour12 = strings.Contains(field(10), "h")
	l.MinGrouping = 1
	if strings.Contains(field(10), "g") {
		l.MinGrouping = 2
	}
	l.DateRangeRepeat = strings.Contains(field(10), "r")
	l.DecimalNegative, l.PercentNegative = field(11), field(12)
	l.CurrencyNegative, l.Calendar = field(13), field(14)
	l.Accounting, l.Exponential = field(15), field(16)
	l.Range, l.Approximately, l.DateRange = field(17), field(18), field(19)
	l.HourCycle, l.HourCycle12, l.HourCycle24 = "h12", "h12", "h23"
	if cycles := strings.Split(field(20), ","); len(cycles) == 3 && cycles[0] != "" {
		l.HourCycle, l.HourCycle12, l.HourCycle24 = cycles[0], cycles[1], cycles[2]
	} else if !l.Hour12 {
		l.HourCycle = "h23"
	}
	if l.Range == "" {
		l.Range = "\u2013"
	}
	if l.Approximately == "" {
		l.Approximately = "~"
	}
	if l.DateRange == "" {
		l.DateRange = l.Range
	}
	if l.Accounting == "" {
		l.Accounting = l.CurrencyNegative
	}
	if l.Exponential == "" {
		l.Exponential = "E"
	}
	if l.DecimalNegative == "" {
		l.DecimalNegative = "-{0}"
	}
	if l.Calendar == "" {
		l.Calendar = "gregory"
	}

	if moved := l.sectionFields(9); len(moved) > 0 {
		l.tailoringEncoded = moved[0]
		decodeTailoringFlags(moved[0], l)
	}
	return l
}

func (l *Locale) section(index int) string {
	section := l.record
	for range index {
		_, rest, ok := strings.Cut(section, sectionSep)
		if !ok {
			return ""
		}
		section = rest
	}
	section, _, _ = strings.Cut(section, sectionSep)
	return section
}

func (l *Locale) sectionFields(index int) []string {
	section := l.section(index)
	if section == "" {
		return nil
	}
	return strings.Split(section, fieldSep)
}

// PrepareDate parses the names and patterns needed by DateTimeFormat. Other
// locale features remain as direct record slices until their API is used.
func (l *Locale) PrepareDate() {
	if l == nil {
		return
	}
	l.dateOnce.Do(func() {
		names := l.sectionFields(1)
		list := func(i int) []string {
			if i >= len(names) || names[i] == "" {
				return nil
			}
			return strings.Split(names[i], itemSep)
		}
		l.Months, l.MonthsShort, l.MonthsNarrow = list(0), list(1), list(2)
		l.Days, l.DaysShort, l.DaysNarrow = list(3), list(4), list(5)
		if periods := list(6); len(periods) == 2 {
			l.DayPeriods = [2]string{periods[0], periods[1]}
		}
		if eras := list(7); len(eras) == 2 {
			l.Eras = [2]string{eras[0], eras[1]}
		}
		l.MonthsAlone, l.MonthsAloneShort = list(8), list(9)
		if periods := list(10); len(periods) == 24 {
			l.HourPeriods = periods
		}
		if periods := list(11); len(periods) == 24 {
			l.HourPeriodsNarrow = periods
		} else {
			l.HourPeriodsNarrow = l.HourPeriods
		}
		l.DaysFormat, l.DaysFormatShort, l.DaysFormatNarrow = list(12), list(13), list(14)
		if l.DaysFormat == nil {
			l.DaysFormat = l.Days
		}
		if l.DaysFormatShort == nil {
			l.DaysFormatShort = l.DaysShort
		}
		if l.DaysFormatNarrow == nil {
			l.DaysFormatNarrow = l.DaysNarrow
		}
		if l.MonthsAlone == nil {
			l.MonthsAlone, l.MonthsAloneShort = l.Months, l.MonthsShort
		}

		patterns := l.sectionFields(2)
		four := func(i int) [4]string {
			var out [4]string
			if i >= len(patterns) {
				return out
			}
			parts := strings.Split(patterns[i], itemSep)
			for k := 0; k < 4 && k < len(parts); k++ {
				out[k] = parts[k]
			}
			return out
		}
		l.DatePatterns, l.TimePatterns, l.Glue = four(0), four(1), four(2)
		l.Skeletons = pairs(patterns, 3)
	})
}

func (l *Locale) prepareCurrencies() {
	l.currenciesOnce.Do(func() { l.Currencies = pairs(l.sectionFields(3), 0) })
}

// CurrencySymbol reports the locale's compact symbol for a currency.
func (l *Locale) CurrencySymbol(code string) (string, bool) {
	l.prepareCurrencies()
	symbol, ok := l.Currencies[code]
	return symbol, ok
}

// CardinalRule returns the locale's cardinal plural rule.
func (l *Locale) CardinalRule() *PluralRule {
	l.cardinalOnce.Do(func() { l.Cardinal = decodePlural(l.sectionFields(4)) })
	return &l.Cardinal
}

// OrdinalRule returns the locale's ordinal plural rule.
func (l *Locale) OrdinalRule() *PluralRule {
	l.ordinalOnce.Do(func() { l.Ordinal = decodePlural(l.sectionFields(5)) })
	return &l.Ordinal
}

// ListPatternFor returns the requested list pattern.
func (l *Locale) ListPatternFor(name string) (ListPattern, bool) {
	l.listsOnce.Do(func() { l.Lists = decodeLists(l.sectionFields(6)) })
	pattern, ok := l.Lists[name]
	return pattern, ok
}

// RelativeUnitFor returns the requested relative-time pattern.
func (l *Locale) RelativeUnitFor(name string) (RelativeUnit, bool) {
	l.relativeOnce.Do(func() { l.Relative = decodeRelative(l.sectionFields(7)) })
	unit, ok := l.Relative[name]
	return unit, ok
}

// pairs reads a field written as name=value items.
func pairs(fields []string, i int) map[string]string {
	if i >= len(fields) || fields[i] == "" {
		return nil
	}
	out := map[string]string{}
	for _, item := range strings.Split(fields[i], itemSep) {
		if name, value, ok := strings.Cut(item, "="); ok {
			out[name] = value
		}
	}
	return out
}

func decodePlural(fields []string) PluralRule {
	var p PluralRule
	if len(fields) < 8 {
		p.Categories = []string{"other"}
		for i := range p.Small {
			p.Small[i], p.Mod[i] = 'x', 'x'
		}
		p.FractionZero, p.FractionOther = 'x', 'x'
		return p
	}
	if fields[0] != "" {
		p.Categories = strings.Split(fields[0], itemSep)
	}
	copy(p.Small[:], fields[1])
	copy(p.Mod[:], fields[2])
	p.Classes = decodeClasses(fields[3])
	p.Exact = numberedCategories(fields[4])
	p.CompactExponents = numberedCategories(fields[5])
	p.FractionZero = byteAt(fields[6])
	p.FractionOther = byteAt(fields[7])
	return p
}

func byteAt(s string) byte {
	if s == "" {
		return 'x'
	}
	return s[0]
}

// decodeClasses reads the residue classes, written as 100000:21000x items.
func decodeClasses(field string) map[int]map[int]byte {
	if field == "" {
		return nil
	}
	out := map[int]map[int]byte{}
	for _, item := range strings.Split(field, itemSep) {
		modulus, rest, ok := strings.Cut(item, ":")
		if !ok || len(rest) < 2 {
			continue
		}
		m, err := strconv.Atoi(modulus)
		if err != nil {
			continue
		}
		r, err := strconv.Atoi(rest[:len(rest)-1])
		if err != nil {
			continue
		}
		if out[m] == nil {
			out[m] = map[int]byte{}
		}
		out[m][r] = rest[len(rest)-1]
	}
	return out
}

// numberedCategories reads the exceptions, written as 100o items.
func numberedCategories(field string) map[int]byte {
	if field == "" {
		return nil
	}
	out := map[int]byte{}
	for _, item := range strings.Split(field, itemSep) {
		if len(item) < 2 {
			continue
		}
		n, err := strconv.Atoi(item[:len(item)-1])
		if err != nil {
			continue
		}
		out[n] = item[len(item)-1]
	}
	return out
}

// Compact shortens a number the way this locale shortens one, and reports the
// scaled value, what it was divided by, and what is written after it. A locale
// that does not shorten at this size answers with the number itself, a divisor
// of one, and nothing to write.
func (l *Locale) Compact(x float64, long bool) (float64, float64, CompactForm) {
	l.compactOnce.Do(func() {
		if compact := l.sectionFields(8); len(compact) >= 2 {
			l.Short = decodeCompact(compact[0])
			l.Long = decodeCompact(compact[1])
		}
	})
	table := l.Short
	if long {
		table = l.Long
	}
	if table == nil || x < 1000 {
		return x, 1, CompactForm{}
	}
	// The power of ten the number reaches, which is what the table is keyed by.
	power := 0
	for v := x; v >= 10; v /= 10 {
		power++
	}
	for power >= 3 {
		if form, ok := table[power]; ok {
			divisor := 1.0
			for i := 0; i < form.Divisor; i++ {
				divisor *= 10
			}
			return x / divisor, divisor, form
		}
		power--
	}
	return x, 1, CompactForm{}
}

// SuffixFor is what is written after a number of this size, in the form the
// count takes: one Million, two Millionen.
func (f CompactForm) SuffixFor(category string) string {
	return f.suffixFor(letterOf(category))
}

// letterOf is how a category is written in the tables.
func letterOf(category string) byte {
	for letter, name := range categoryNames {
		if name == category {
			return letter
		}
	}
	return 'x'
}

func (f CompactForm) suffixFor(category byte) string {
	if s, ok := f.Suffixes[category]; ok {
		return s
	}
	if s, ok := f.Suffixes['x']; ok {
		return s
	}
	for _, s := range f.Suffixes {
		return s
	}
	return ""
}

// decodeTailoring reads where a language moves its letters, and works out what
// each moved letter weighs: just after the letter it was moved to sit after.
func decodeTailoring(field string, l *Locale) map[rune]int32 {
	if field == "" {
		return nil
	}
	items := strings.Split(field, itemSep)
	out := map[rune]int32{}
	// The letters that moved come first, so that a spelling anchored to one of
	// them is put after where the letter went rather than where it came from:
	// Danish sorts aa after å, and å itself sits after z.
	var spellings []string
	for _, item := range items[1:] {
		if strings.HasPrefix(item, "*") {
			spellings = append(spellings, item)
			continue
		}
		parts := strings.Split(item, ":")
		if len(parts) != 3 {
			continue
		}
		letter, err1 := strconv.ParseInt(parts[0], 16, 32)
		anchor, err2 := strconv.ParseInt(parts[1], 16, 32)
		offset, err3 := strconv.ParseInt(parts[2], 16, 32)
		if err1 != nil || err2 != nil || err3 != nil || offset >= weightScale/16 {
			continue
		}
		anchorWeight, _, _, ok := order().weightsOf(rune(anchor))
		if !ok {
			continue
		}
		// The offsets are spaced out, so that a spelling can be put between
		// two letters that were moved.
		out[rune(letter)] = anchorWeight*weightScale + int32(offset)*16
	}

	for _, item := range spellings {
		parts := strings.Split(item[1:], ":")
		if len(parts) != 3 {
			continue
		}
		anchor, err := strconv.ParseInt(parts[1], 16, 32)
		if err != nil {
			continue
		}
		weight, moved := out[rune(anchor)]
		if !moved {
			anchorWeight, _, _, ok := order().weightsOf(rune(anchor))
			if !ok {
				continue
			}
			weight = anchorWeight * weightScale
		}
		if l.Contractions == nil {
			l.Contractions = map[string]int32{}
		}
		l.Contractions[parts[0]] = weight + 1
	}
	return out
}

func decodeTailoringFlags(field string, l *Locale) {
	flags, _, _ := strings.Cut(field, itemSep)
	l.UpperFirst = strings.Contains(flags, "u")
	l.Shifted = strings.Contains(flags, "s")
}

// loadTailoring resolves locale-specific collation weights only when text is
// actually compared. Number and date formatters still need the locale's two
// collation flags, but must not pay to inflate the root collation table.
func (l *Locale) loadTailoring() {
	if l == nil {
		return
	}
	l.tailoringOnce.Do(func() {
		l.Tailoring = decodeTailoring(l.tailoringEncoded, l)
		l.tailoringEncoded = ""
	})
}

func decodeCompact(field string) map[int]CompactForm {
	if field == "" {
		return nil
	}
	out := map[int]CompactForm{}
	for _, item := range strings.Split(field, itemSep) {
		power, rest, ok := strings.Cut(item, ":")
		if !ok {
			continue
		}
		divisor, suffix, ok := strings.Cut(rest, ":")
		if !ok {
			continue
		}
		p, err1 := strconv.Atoi(power)
		d, err2 := strconv.Atoi(divisor)
		if err1 != nil || err2 != nil {
			continue
		}
		forms := map[byte]string{}
		for _, one := range strings.Split(suffix, ",") {
			if len(one) >= 2 && one[1] == '=' {
				forms[one[0]] = one[2:]
			}
		}
		out[p] = CompactForm{Divisor: d, Suffixes: forms}
	}
	return out
}

func decodeLists(fields []string) map[string]ListPattern {
	if len(fields) == 0 || fields[0] == "" {
		return nil
	}
	out := map[string]ListPattern{}
	for _, item := range fields {
		parts := strings.Split(item, itemSep)
		if len(parts) != 5 {
			continue
		}
		out[parts[0]] = ListPattern{
			Pair: parts[1], Start: parts[2], Middle: parts[3], End: parts[4],
		}
	}
	return out
}

func decodeRelative(fields []string) map[string]RelativeUnit {
	if len(fields) == 0 || fields[0] == "" {
		return nil
	}
	out := map[string]RelativeUnit{}
	for _, item := range fields {
		parts := strings.Split(item, itemSep)
		if len(parts) < 1 {
			continue
		}
		unit := RelativeUnit{
			Past: map[string]string{}, Future: map[string]string{},
			Named: map[int]string{},
		}
		for _, entry := range parts[1:] {
			key, value, ok := strings.Cut(entry, "=")
			if !ok {
				continue
			}
			switch {
			case strings.HasPrefix(key, "p:"):
				unit.Past[key[2:]] = value
			case strings.HasPrefix(key, "f:"):
				unit.Future[key[2:]] = value
			case strings.HasPrefix(key, "n:"):
				n := 0
				negative := false
				for i := 0; i < len(key[2:]); i++ {
					c := key[2:][i]
					if c == '-' {
						negative = true
						continue
					}
					n = n*10 + int(c-'0')
				}
				if negative {
					n = -n
				}
				unit.Named[n] = value
			}
		}
		out[parts[0]] = unit
	}
	return out
}

// NumberingDigits is the ten digits a numbering system writes numbers with,
// and whether there is such a system at all. A system that writes numbers some
// other way -- roman numerals, or the Chinese ones written out in words -- has
// no ten digits and is not here.
func NumberingDigits(name string) (string, bool) {
	entry, ok := numberingSystems[name]
	if !ok {
		return "", false
	}
	digits, _, _ := strings.Cut(entry, "\x01")
	return digits, true
}

// NumberingMarks are the decimal point and the thousands mark that go with a
// numbering system: Arabic writes them differently from the way it writes them
// in Latin letters.
func NumberingMarks(name string) (decimal, group string, ok bool) {
	entry, found := numberingSystems[name]
	if !found {
		return "", "", false
	}
	_, marks, _ := strings.Cut(entry, "\x01")
	runes := []rune(marks)
	if len(runes) != 2 {
		return "", "", false
	}
	return string(runes[0]), string(runes[1]), true
}

// NumberingSystems lists every way of writing numbers that is ten digits.
func NumberingSystems() []string {
	out := make([]string, 0, len(numberingSystems))
	for name := range numberingSystems {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// CategoryOf is the form a count takes when it is written with these digits.
// A language counts what is written rather than what it means: one apple, but
// 1.0 apples, because the one has a fraction written after it.
func (p *PluralRule) CategoryOf(whole, fraction string, n float64, compactExponent int) string {
	if category, ok := p.CompactExponents[compactExponent]; compactExponent != 0 && ok {
		return categoryNames[category]
	}
	if fraction != "" {
		if strings.Trim(whole, "0") == "" {
			return categoryNames[p.FractionZero]
		}
		return categoryNames[p.FractionOther]
	}
	if written, err := strconv.ParseFloat(whole, 64); err == nil {
		return p.Category(written)
	}
	return p.Category(n)
}

// CanonicalZone is a time zone written the way the database writes it, found
// however it was spelled: "america/port-au-prince" is America/Port-au-Prince.
// A zone that is another zone's old name keeps its old name, since that is the
// name it was asked for by; ZoneTarget says which zone it stands for.
func CanonicalZone(name string) (string, bool) {
	zoneIndexOnce.Do(func() {
		zoneIndex = make(map[string]string, len(zoneList)+len(zoneAliases))
		for _, zone := range zoneList {
			zoneIndex[strings.ToLower(zone)] = zone
		}
		for from := range zoneAliases {
			zoneIndex[strings.ToLower(from)] = from
		}
		// Greenwich itself, which is not among the zones a place is named
		// after but is the one a program asks for most.
		for _, name := range []string{"UTC", "Etc/UTC", "Etc/GMT", "GMT"} {
			zoneIndex[strings.ToLower(name)] = name
		}
		// The zones that are an offset from Greenwich and nothing more, which
		// the database carries under names of their own.
		for hours := -14; hours <= 12; hours++ {
			name := "Etc/GMT"
			switch {
			case hours > 0:
				name += "+" + strconv.Itoa(hours)
			case hours < 0:
				name += "-" + strconv.Itoa(-hours)
			}
			if _, taken := zoneIndex[strings.ToLower(name)]; !taken {
				zoneIndex[strings.ToLower(name)] = name
			}
		}
	})
	zone, ok := zoneIndex[strings.ToLower(name)]
	return zone, ok
}

// ZoneTarget is the zone another zone's name stands for, for the names that
// are another zone's under an older spelling.
func ZoneTarget(name string) (string, bool) {
	to, ok := zoneAliases[name]
	return to, ok
}

var (
	zoneIndexOnce sync.Once
	zoneIndex     map[string]string
)

// TagAliases is what a name in a language tag has been replaced by: a language
// renamed, a country dissolved, a variant folded into another, a setting that
// goes by another word now. The kinds are asked for separately because the
// same string may be a language and a region.
func TagAliases() (languages, regions, scripts, grandfathered, variants, settings, byLanguage map[string]string) {
	aliasOnce.Do(func() {
		text := rawString(tagAliases)
		lines := strings.Split(text, "\n")
		for i := range aliasTables {
			aliasTables[i] = map[string]string{}
			if i >= len(lines) {
				continue
			}
			for _, item := range strings.Split(lines[i], ";") {
				if from, to, ok := strings.Cut(item, "="); ok {
					aliasTables[i][from] = to
				}
			}
		}
	})
	return aliasTables[0], aliasTables[1], aliasTables[2], aliasTables[3],
		aliasTables[4], aliasTables[5], aliasTables[6]
}

var (
	aliasOnce   sync.Once
	aliasTables [7]map[string]string
)
