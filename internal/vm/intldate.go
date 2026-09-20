package vm

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/go-quickjs/go-quickjs/internal/icu"
)

// Date formatting for Intl.DateTimeFormat, and for the toLocaleString methods
// that are defined in terms of it.
//
// A locale's date formats are patterns -- "EEEE, d. MMMM y", "M/d/yy" -- and
// what is here reads one and writes the fields it names. The patterns and the
// names of the months come from the locale data; the calendar arithmetic is
// the engine's own, so that a date formats as the same instant it compares as.

// dateOptions is a resolved Intl.DateTimeFormat.
type dateOptions struct {
	locale *icu.Locale
	choice *localeChoice
	// formatFn is the bound function the format getter hands out, kept so that
	// every ask answers with the same one.
	formatFn *Object

	// zone is where the fields are read: UTC, the machine's own, or whichever
	// one was asked for by name.
	zone     *time.Location
	timeZone string
	hour12   bool
	// hourSet says the clock was chosen by the caller rather than by the
	// locale, which changes which pattern is written.
	hourSet bool
	// hourCycle is what resolvedOptions reports: h11, h12, h23.
	hourCycle string

	dateStyle, timeStyle string
	// The fields, in the widths they were asked for. An empty string means the
	// field was not asked for at all.
	weekday, era, year, month, day     string
	dayPeriod                          string
	hour, minute, second, timeZoneName string
	// fractional is how many digits of a second to write, none by default.
	fractional int
	// calendar and digits are what the tag or the options settled on.
	calendar, digits string
	// pattern is what all of that came to, in CLDR pattern letters.
	pattern          string
	implicitDefaults bool
}

func (o *dateOptions) forDateArgument(value Value) *dateOptions {
	if !o.implicitDefaults || !value.IsObject() {
		return o
	}
	if _, ok := value.Object().data.(*temporalInstant); !ok {
		return o
	}
	copy := *o
	copy.hour, copy.minute, copy.second = "numeric", "numeric", "numeric"
	copy.pattern = adjustClock(copy.patternFor(), copy.hourCycle)
	return &copy
}

// hasFields reports whether any of the parts of a date were asked for by name,
// which is what a style may not be combined with.
func (o *dateOptions) hasFields() bool {
	return o.weekday != "" || o.year != "" || o.month != "" || o.day != "" ||
		o.hour != "" || o.minute != "" || o.second != "" || o.era != "" ||
		o.dayPeriod != "" || o.fractional != 0 || o.timeZoneName != ""
}

// setZone settles which zone the fields are read in: the machine's own, the
// one named, or an offset from Greenwich written out.
func (o *dateOptions) setZone(r *Runtime, zone string, given bool) error {
	switch {
	case !given:
		// The machine's own zone, which is the one a Date is written in.
		o.zone = r.location()
		o.timeZone = r.localZoneName()
		return nil
	}
	// An offset written out rather than a name: +03:00, -0800, +05:45.
	if minutes, name, ok := parseZoneOffset(zone); ok {
		o.zone = time.FixedZone(name, minutes*60)
		o.timeZone = name
		return nil
	}
	// A name is written in the letters the database uses and no others.
	if !asciiOnly(zone) {
		return r.throwRangeError("there is no such time zone: %s", zone)
	}
	// A zone named rather than offset, however it was spelled and whatever it
	// used to be called. Loading it reads the IANA database paired with ICU; a
	// script cannot reach the host's possibly older zone files.
	name, ok := icu.CanonicalZone(zone)
	if !ok {
		return r.throwRangeError("there is no such time zone: %s", zone)
	}
	// The name is kept as it was asked for; what it stands for is what the
	// fields are read in.
	target := name
	if to, ok := icu.ZoneTarget(name); ok {
		target = to
	}
	if isUTCName(target) {
		o.zone, o.timeZone = time.UTC, name
		return nil
	}
	loc, err := loadNamedLocation(target)
	if err != nil {
		if loc, err = loadNamedLocation(name); err != nil {
			return r.throwRangeError("there is no such time zone here: %s", zone)
		}
	}
	o.zone, o.timeZone = loc, name
	return nil
}

var namedLocations sync.Map

// loadNamedLocation retains parsed zone files so eager warm-up and later Intl
// constructors share the same time.Location values.
func loadNamedLocation(name string) (*time.Location, error) {
	if cached, ok := namedLocations.Load(name); ok {
		return cached.(*time.Location), nil
	}
	location, err := icu.LoadLocation(name)
	if err != nil {
		return nil, err
	}
	actual, _ := namedLocations.LoadOrStore(name, location)
	return actual.(*time.Location), nil
}

// parseZoneOffset reads a zone written as an offset from Greenwich, and writes
// it back the one way it is written: a sign, two digits, a colon, two digits,
// and the seconds left off when there are none.
func parseZoneOffset(s string) (minutes int, name string, ok bool) {
	if len(s) < 3 || (s[0] != '+' && s[0] != '-') {
		return 0, "", false
	}
	sign := 1
	if s[0] == '-' {
		sign = -1
	}
	rest := s[1:]
	var hours, mins string
	switch {
	case len(rest) == 2:
		hours = rest
	case len(rest) == 4:
		hours, mins = rest[:2], rest[2:]
	case len(rest) == 5 && rest[2] == ':':
		hours, mins = rest[:2], rest[3:]
	default:
		return 0, "", false
	}
	if !allDigits(hours) || (mins != "" && !allDigits(mins)) {
		return 0, "", false
	}
	h, _ := strconv.Atoi(hours)
	m := 0
	if mins != "" {
		m, _ = strconv.Atoi(mins)
	}
	if h > 23 || m > 59 {
		return 0, "", false
	}
	out := sign * (h*60 + m)
	written := "+"
	if out < 0 {
		written = "-"
	}
	away := out
	if away < 0 {
		away = -away
	}
	return out, fmt.Sprintf("%s%02d:%02d", written, away/60, away%60), true
}

// datePiece is one part of a formatted date.
type datePiece struct {
	kind  string
	value string
}

// at is an instant read in this format's zone.
func (o *dateOptions) at(ms float64) time.Time {
	zone := o.zone
	if zone == nil {
		zone = time.UTC
	}
	// The same arithmetic Date uses, so that a date formats as the instant it
	// compares as: milliseconds since the epoch, put in a zone.
	whole := math.Floor(ms / 1000)
	return time.Unix(int64(whole), int64((ms-whole*1000))*1e6).In(zone)
}

func (o *dateOptions) format(t time.Time) string {
	var b strings.Builder
	for _, piece := range o.parts(t) {
		b.WriteString(piece.value)
	}
	return b.String()
}

// parts reads the pattern and writes each field of the date it names.
func (o *dateOptions) parts(t time.Time) []datePiece {
	l := o.locale
	pattern := o.pattern
	var out []datePiece

	push := func(kind, value string) {
		// A calendar with months of its own may name a month with the word
		// the pattern would have put after it -- the eleventh month of the
		// Chinese year is 十一月, and the pattern says 月 -- and it is not
		// written twice.
		if kind == "literal" && len(out) > 0 && out[len(out)-1].kind == "month" {
			name := out[len(out)-1].value
			if r, _ := utf8.DecodeLastRuneInString(name); r != utf8.RuneError {
				if next, width := utf8.DecodeRuneInString(value); next == r && (r == '月' || r == '월') {
					value = value[width:]
				}
			}
		}
		if value == "" {
			return
		}
		if kind == "literal" && len(out) > 0 && out[len(out)-1].kind == "literal" {
			out[len(out)-1].value += value
			return
		}
		out = append(out, datePiece{kind, value})
	}

	for i := 0; i < len(pattern); {
		c := pattern[i]
		// Anything in quotes is written as it is; two quotes are one quote.
		if c == '\'' {
			i++
			for i < len(pattern) {
				if pattern[i] == '\'' {
					if i+1 < len(pattern) && pattern[i+1] == '\'' {
						push("literal", "'")
						i += 2
						continue
					}
					i++
					break
				}
				// A literal is text, and text is runes: taking it a byte at a
				// time would cut a character in half.
				_, width := utf8.DecodeRuneInString(pattern[i:])
				push("literal", pattern[i:i+width])
				i += width
			}
			continue
		}
		if !isPatternLetter(c) {
			_, width := utf8.DecodeRuneInString(pattern[i:])
			push("literal", pattern[i:i+width])
			i += width
			continue
		}
		// A run of the same letter is one field, and how many there are says
		// how wide it is written.
		n := 1
		for i+n < len(pattern) && pattern[i+n] == c {
			n++
		}
		o.field(push, t, c, n)
		i += n
	}

	// The digits of a locale that does not use the ASCII ones, or of whichever
	// numbering system was asked for.
	digits := l.Digits
	// Some decimal systems use supplementary-plane characters. Older locale
	// snapshots could not carry those as a ten-byte digit string, but the
	// numbering-system table always has the complete runes.
	if utf8.RuneCountInString(digits) != 10 && o.digits != "" && o.digits != "latn" {
		digits, _ = icu.NumberingDigits(o.digits)
	}
	if o.digits != "" && o.digits != l.Numbering {
		digits, _ = icu.NumberingDigits(o.digits)
	}
	if digits != "" {
		for i, piece := range out {
			if piece.kind != "literal" {
				out[i].value = localiseDigits(piece.value, digits)
			}
		}
	}
	return out
}

// field writes one field of the pattern.
func (o *dateOptions) field(push func(kind, value string), t time.Time, letter byte, n int) {
	l := o.locale
	pad := func(v, width int) string {
		s := strconv.Itoa(v)
		for len(s) < width {
			s = "0" + s
		}
		return s
	}

	// The date as the calendar being written counts it, which is the common
	// one unless another was asked for.
	at := o.reckon(t)

	switch letter {
	case 'y', 'u':
		year := at.Year
		if n == 2 {
			push("year", pad(year%100, 2))
			return
		}
		push("year", strconv.Itoa(year))
	case 'A':
		push("year", hebrewNumber(at.Year, true))
	case 'Y':
		year := o.weekYear(t)
		if n == 2 {
			push("year", pad(year%100, 2))
			return
		}
		push("year", strconv.Itoa(year))
	case 'r':
		// The year of the common calendar that this one's year began in,
		// which is how a calendar whose years are named says which year it
		// means.
		push("relatedYear", strconv.Itoa(at.RelatedYear))
	case 'U':
		// What this year is called, where a calendar names its years rather
		// than counting them.
		if names, ok := o.calendarNames(); ok && at.Era < len(names.Cycle) {
			push("yearName", names.Cycle[at.Era])
			return
		}
		push("relatedYear", strconv.Itoa(at.RelatedYear))
	case 'G':
		push("era", o.eraName(at, n))
	case 'M', 'L', 'N', 'P':
		if names, ok := o.calendarNames(); ok {
			// A calendar of its own has months of its own, and a month that
			// only a long year has is one of them.
			width := 0
			switch {
			case n >= 5:
				width = 2
			case n == 3:
				width = 1
			}
			months := names.Months[width]
			if letter == 'N' {
				if len(names.DateMonths[width]) > 0 {
					months = names.DateMonths[width]
				} else if len(names.FormatMonths[width]) > 0 {
					months = names.FormatMonths[width]
				}
			} else if letter == 'M' && len(names.FormatMonths[width]) > 0 {
				months = names.FormatMonths[width]
			}
			if name, ok := months[at.MonthKey()]; ok && n >= 3 {
				if letter == 'P' {
					name = strings.ToLower(name)
				}
				push("month", name)
				return
			}
			push("month", pad(at.Month, n))
			return
		}
		month := at.Month
		// M is the form a month takes in a date and L the name it is called
		// by, which are two different words in the languages that decline
		// them: 5 stycznia, but styczeń on its own.
		long, short := l.Months, l.MonthsShort
		if letter == 'L' {
			long, short = l.MonthsAlone, l.MonthsAloneShort
		}
		switch {
		case n >= 5:
			push("month", nameAt(l.MonthsNarrow, month-1, strconv.Itoa(month)))
		case n == 4:
			push("month", nameAt(long, month-1, strconv.Itoa(month)))
		case n == 3:
			push("month", nameAt(short, month-1, strconv.Itoa(month)))
		default:
			push("month", pad(month, n))
		}
	case 'J', 'j':
		value := romanMonth(at.Month)
		if letter == 'j' {
			value = strings.ToLower(value)
		}
		push("month", value)
	case 'd':
		push("day", pad(at.Day, n))
	case 'I':
		push("day", hebrewNumber(at.Day, false))
	case 'E', 'e', 'c':
		day := int(t.Weekday())
		long, short, narrow := l.DaysFormat, l.DaysFormatShort, l.DaysFormatNarrow
		if letter == 'c' {
			long, short, narrow = l.Days, l.DaysShort, l.DaysNarrow
		}
		switch {
		case n >= 5:
			push("weekday", nameAt(narrow, day, ""))
		case n == 4:
			push("weekday", nameAt(long, day, ""))
		default:
			push("weekday", nameAt(short, day, ""))
		}
	case 'h', 'K':
		hour := t.Hour() % 12
		if letter == 'h' && hour == 0 {
			hour = 12
		}
		push("hour", pad(hour, n))
	case 'H', 'k':
		hour := t.Hour()
		if letter == 'k' && hour == 0 {
			hour = 24
		}
		push("hour", pad(hour, n))
	case 'm':
		push("minute", pad(t.Minute(), n))
	case 's':
		push("second", pad(t.Second(), n))
	case 'S':
		ms := t.Nanosecond() / 1e6
		push("fractionalSecond", pad(ms, n)[:min(n, 3)])
	case 'a':
		if t.Hour() < 12 {
			push("dayPeriod", l.DayPeriods[0])
			return
		}
		push("dayPeriod", l.DayPeriods[1])
	case 'b', 'B':
		// The part of the day this hour falls in, where the language names
		// them: the small hours are not the morning.
		periods := l.HourPeriods
		if o.dayPeriod == "narrow" && len(l.HourPeriodsNarrow) == 24 {
			periods = l.HourPeriodsNarrow
		}
		if len(periods) == 24 {
			if name := periods[t.Hour()]; name != "" {
				push("dayPeriod", name)
				return
			}
		}
		if t.Hour() < 12 {
			push("dayPeriod", l.DayPeriods[0])
			return
		}
		push("dayPeriod", l.DayPeriods[1])
	case 'z', 'v', 'O':
		push("timeZoneName", o.zoneName(t, zoneStyle(letter, n)))
	case 'Z', 'V', 'X', 'x':
		_, offset := t.Zone()
		push("timeZoneName", o.offsetName(offset, false))
	case 'Q', 'q':
		quarter := (int(t.Month())-1)/3 + 1
		push("literal", "Q"+strconv.Itoa(quarter))
	case 'D':
		push("day", strconv.Itoa(t.YearDay()))
	case 'w', 'W', 'F', 'g':
		// Week numbers, which nothing here asks for.
	default:
		// An unknown letter is written as itself rather than swallowed.
		push("literal", strings.Repeat(string(letter), n))
	}
}

// weekYear is the year containing this locale's week. A week which crosses a
// calendar-year boundary belongs to the side containing the locale's minimum
// number of days. Offset-era Gregorian calendars use the underlying common
// year here, matching ICU's YEAR_WOY field.
func (o *dateOptions) weekYear(t time.Time) int {
	firstDay, minimumDays := icu.WeekInfoForLocale(o.locale.Tag)
	weekday := int(t.Weekday())
	start := t.AddDate(0, 0, -((weekday - firstDay%7 + 7) % 7))
	end := start.AddDate(0, 0, 6)
	startDate, endDate := o.reckon(start), o.reckon(end)
	value := func(date icu.Date) int {
		switch o.calendar {
		case "buddhist", "japanese", "roc":
			return date.RelatedYear
		}
		return date.Year
	}
	startYear, endYear := value(startDate), value(endDate)
	if startYear == endYear {
		return startYear
	}
	daysInEndYear := 0
	for day := 0; day < 7; day++ {
		if value(o.reckon(start.AddDate(0, 0, day))) == endYear {
			daysInEndYear++
		}
	}
	if daysInEndYear >= minimumDays {
		return endYear
	}
	return startYear
}

// zoneLetters is the pattern letter that writes a zone in the style asked
// for, which is what is added to a pattern that carries no zone of its own.
func zoneLetters(style string) string {
	switch style {
	case "long":
		return "zzzz"
	case "shortGeneric":
		return "v"
	case "longGeneric":
		return "vvvv"
	case "shortOffset":
		return "O"
	case "longOffset":
		return "OOOO"
	default:
		return "z"
	}
}

// zoneStyle is the kind of zone name a pattern letter asks for: a name for
// the season in z, one that holds all year in v, and the offset written out
// in O, each in a long form and a short one.
func zoneStyle(letter byte, n int) string {
	switch letter {
	case 'v':
		if n >= 4 {
			return "longGeneric"
		}
		return "shortGeneric"
	case 'O':
		if n >= 4 {
			return "longOffset"
		}
		return "shortOffset"
	default:
		if n >= 4 {
			return "long"
		}
		return "short"
	}
}

// zoneName is what the zone is called at this instant, in the style asked
// for. What was asked for wins over what the locale's pattern carries, since
// a pattern is chosen for the fields in it rather than for the style of them.
//
// A zone with no name in this language is written as an offset from
// Greenwich, which is what most zones outside the Americas are called anyway.
func (o *dateOptions) zoneName(t time.Time, style string) string {
	if o.timeZoneName != "" {
		style = o.timeZoneName
	}
	if style == "longOffset" || style == "shortOffset" {
		_, offset := t.Zone()
		return o.offsetName(offset, style == "longOffset")
	}
	names, ok := icu.ZoneNamesAt(o.locale.Tag, o.timeZone, t.UnixMilli())
	if !ok {
		if style == "longGeneric" || style == "shortGeneric" {
			names = icu.ZoneGenericNamesIn(o.locale.Tag, o.timeZone)
		} else {
			names = icu.ZoneSeasonNamesIn(o.locale.Tag, o.timeZone)
		}
	}
	var name string
	switch style {
	case "long":
		name = names.LongStandard
		if t.IsDST() {
			name = names.LongDaylight
		}
	case "short":
		name = names.ShortStandard
		if t.IsDST() {
			name = names.ShortDaylight
		}
	case "longGeneric":
		name = names.LongGeneric
	case "shortGeneric":
		name = names.ShortGeneric
	}
	if name != "" {
		return name
	}
	_, offset := t.Zone()
	return o.offsetName(offset, strings.HasPrefix(style, "long"))
}

// offsetName is what a zone with no name of its own is called, in the
// language being written: GMT-05:00 in English, UTC−05:00 in French.
func (o *dateOptions) offsetName(offset int, long bool) string {
	return icu.OffsetNameSeconds(o.locale.Tag, offset, long)
}

func pad2(n int) string {
	if n < 10 {
		return "0" + strconv.Itoa(n)
	}
	return strconv.Itoa(n)
}

func nameAt(names []string, i int, fallback string) string {
	if i >= 0 && i < len(names) {
		return names[i]
	}
	return fallback
}

func isPatternLetter(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

// reckon is a day as the calendar being written counts it.
func (o *dateOptions) reckon(t time.Time) icu.Date {
	switch o.calendar {
	case "", "gregory", "iso8601":
		year, month, day := t.Date()
		out := icu.Date{Era: 1, Year: year, Month: int(month), Day: day, RelatedYear: year}
		if year <= 0 {
			out.Era, out.Year = 0, 1-year
		}
		return out
	}
	return icu.DateIn(o.calendar, t)
}

// withEra puts the era after the year, which a calendar that is not the common
// one always says: the number alone would not say which calendar it belongs
// to. It is named in short where the month is written as a word and in the
// shortest form where the month is a number, which is what a full ICU does.
// The year is written out in full as well, since two digits of a year that is
// not this one's say even less.
func (o *dateOptions) withEra(pattern string) string {
	if !o.calendarNamed() {
		return pattern
	}
	// Calendar-specific CLDR patterns already say whether the era belongs in
	// this combination. Hebrew, for example, normally omits it while Coptic
	// includes it.
	if _, ok := o.locale.CalendarFormatFor(o.calendar); ok {
		return pattern
	}
	letters := patternLettersOf(pattern)
	if !strings.ContainsRune(letters, 'y') {
		return pattern
	}
	pattern = strings.ReplaceAll(pattern, "yy", "y")
	if strings.ContainsRune(letters, 'G') {
		return pattern
	}
	// In front of the year where the language writes the year first, which is
	// where a reader of that language looks for it: 令和6年1月5日. Which
	// languages those are is read from how each writes a full date, since a
	// short one is written in numbers everywhere.
	if yearFirst(o.locale.DatePatterns[0]) && strings.ContainsRune(letters, 'y') {
		at := strings.IndexByte(pattern, 'y')
		return pattern[:at] + "G" + pattern[at:]
	}
	// After it otherwise, in its shortest form where the month is a number,
	// since the two stand side by side and a name would crowd them.
	era := "G"
	if strings.ContainsRune(letters, 'M') && !strings.Contains(letters, "MMM") {
		era = "GGGGG"
	}
	return pattern + " " + era
}

// yearFirst reports whether a pattern writes the year first and marks it with
// a word of its own, which is what says the era goes in front of it: Japanese
// writes 令和6年1月5日, where the era leads and 年 closes the year.
func yearFirst(pattern string) bool {
	letters := patternLettersOf(pattern)
	year, month := strings.IndexByte(letters, 'y'), strings.IndexByte(letters, 'M')
	if year < 0 || (month >= 0 && month < year) {
		return false
	}
	// What follows the year in the pattern itself: a mark of its own rather
	// than a separator.
	at := strings.IndexByte(pattern, 'y')
	for at < len(pattern) && pattern[at] == 'y' {
		at++
	}
	if at >= len(pattern) {
		return false
	}
	next, _ := utf8.DecodeRuneInString(pattern[at:])
	return next > 0x2000
}

// withYearName writes the year of a lunisolar calendar the way such a calendar
// writes it: the year of the common calendar that it began in, and the name it
// goes by where the language has names for them.
func (o *dateOptions) withYearName(pattern string) string {
	switch o.calendar {
	case "chinese", "dangi":
	default:
		return pattern
	}
	at := strings.IndexByte(pattern, 'y')
	if at < 0 {
		return pattern
	}
	end := at
	for end < len(pattern) && pattern[end] == 'y' {
		end++
	}
	// The name is written where the language writes it in its own letters; a
	// language that only spells it out writes it beside the year where the
	// year is all that was asked for, and leaves it out otherwise.
	year := "r"
	if names, ok := o.calendarCycle(); ok {
		switch {
		case !asciiOnly(names[0]):
			year = "rU"
		case !strings.ContainsAny(patternLettersOf(pattern), "MdEhHms"):
			year = "r'('U')'"
		}
	}
	return pattern[:at] + year + pattern[end:]
}

// calendarNamed reports whether a calendar other than this language's own is
// being written. A language whose own calendar is not the common one -- Thai
// counts from the Buddhist era -- writes its dates without saying so, since
// nobody reading them would think otherwise.
func (o *dateOptions) calendarNamed() bool {
	switch o.calendar {
	case "", "gregory", "iso8601", o.locale.Calendar:
		return false
	case "chinese", "dangi":
		// A lunisolar calendar counts from no era: its years are named rather
		// than counted from anywhere.
		return false
	}
	return true
}

// calendarEras is what this language calls the eras of the calendar being
// written, where that is not the common one.
func (o *dateOptions) calendarEras() (*icu.CalendarNames, bool) {
	if !o.calendarNamed() {
		return nil, false
	}
	return o.locale.CalendarNamesFor(o.calendar)
}

// calendarNames is what this language calls the months and eras of the
// calendar being written, where that calendar has months of its own. The
// Buddhist, Japanese and Republic of China calendars keep the same sequence
// of months as the common calendar, but their locale data can still give
// those months a different standalone form or surrounding literals.
func (o *dateOptions) calendarNames() (*icu.CalendarNames, bool) {
	switch o.calendar {
	case "", "gregory", "iso8601":
		return nil, false
	}
	return o.locale.CalendarNamesFor(o.calendar)
}

// calendarCycle is what this language calls the years of a calendar that names
// them, which is a cycle of sixty.
func (o *dateOptions) calendarCycle() ([]string, bool) {
	names, ok := o.locale.CalendarNamesFor(o.calendar)
	if !ok || len(names.Cycle) == 0 {
		return nil, false
	}
	return names.Cycle, true
}

// eraName is what the era of a date is called, in the width the pattern asks
// for. A calendar that counts its years from somewhere else has eras of its
// own even where its months are the common ones: the Japanese year is counted
// from the start of a reign and named after it.
func (o *dateOptions) eraName(at icu.Date, n int) string {
	// The Islamic calendars' data has a name for AH but no localized name for
	// the proleptic era before it. BH is the standard era code and remains an
	// unambiguous fallback where CLDR supplies no display name.
	switch o.calendar {
	case "islamic", "islamic-civil", "islamic-rgsa", "islamic-tbla", "islamic-umalqura":
		if at.Era == 0 {
			return "BH"
		}
		at.Era = 0 // The one name in CLDR is the current AH era.
	}
	if names, ok := o.calendarEras(); ok {
		width := 1
		switch {
		case n >= 5:
			width = 2
		case n == 4:
			width = 0
		}
		if list := names.Eras[width]; at.Era < len(list) {
			return list[at.Era]
		}
		if list := names.Eras[1]; at.Era < len(list) {
			return list[at.Era]
		}
	}
	if at.Era == 0 {
		return o.locale.Eras[0]
	}
	return o.locale.Eras[1]
}

// localiseDigits writes ASCII digits in the locale's own.
func localiseDigits(s, digits string) string {
	runes := []rune(digits)
	if len(runes) != 10 {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] >= '0' && s[i] <= '9' {
			b.WriteRune(runes[s[i]-'0'])
			continue
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// patternFor works out which pattern the options ask for: a style is one the
// locale carries, and a set of fields is the skeleton that names them.
func (o *dateOptions) patternFor() string {
	l := o.locale
	datePatterns, gluePatterns, calendarSkeletons := o.calendarPatterns()
	widths := map[string]int{"full": 0, "long": 1, "medium": 2, "short": 3}

	// The part of the day on its own, which is a field no skeleton names.
	if o.dayPeriod != "" && o.dateStyle == "" && o.timeStyle == "" &&
		o.weekday == "" && o.era == "" && o.year == "" && o.month == "" &&
		o.day == "" && o.hour == "" && o.minute == "" && o.second == "" {
		return "B"
	}

	if o.dateStyle != "" || o.timeStyle != "" {
		date, clock := "", ""
		if i, ok := widths[o.dateStyle]; ok {
			date = datePatterns[i]
		}
		if i, ok := widths[o.timeStyle]; ok {
			clock = l.TimePatterns[i]
		}
		pattern := clock
		switch {
		case date != "" && clock != "":
			// Both, joined the way the locale joins them: " at ", " um ", or
			// a space, which is part of the language rather than of either
			// pattern.
			glue := gluePatterns[widths[o.dateStyle]]
			if !strings.Contains(glue, "{0}") {
				glue = "{0}, {1}"
			}
			joined := strings.Replace(glue, "{0}", date, 1)
			pattern = strings.Replace(joined, "{1}", clock, 1)
		case date != "":
			pattern = date
		}
		return o.withYearName(o.withEra(pattern))
	}

	// A set of fields: the locale's own order for that combination, when it
	// has one, and otherwise the fields in the order its short date puts them.
	pattern, ok := calendarSkeletons[o.skeleton()]
	calendarPattern := ok
	if !ok {
		pattern, ok = l.Skeletons[o.skeleton()]
	}
	if !ok {
		// A date and a time asked for together are the locale's pattern for
		// each, joined the way it joins them.
		if date, time := o.splitSkeletons(); date != "" && time != "" {
			// The shortest glue, which is what a request by field gets: a
			// comma in English, a space in French.
			glue := gluePatterns[3]
			if !strings.Contains(glue, "{0}") {
				glue = "{0}, {1}"
			}
			joined := strings.Replace(glue, "{0}", date, 1)
			pattern = strings.Replace(joined, "{1}", time, 1)
		} else {
			pattern = o.buildPattern()
		}
	}
	// A weekday or an era is not part of what the locale keys its patterns by,
	// so they are put where this locale puts them.
	if o.weekday != "" && !strings.ContainsAny(patternLettersOf(pattern), "Eec") {
		pattern = o.withWeekday(pattern)
	}
	noEra := o.calendar == "chinese" || o.calendar == "dangi"
	if o.era != "" && !noEra && !strings.ContainsRune(patternLettersOf(pattern), 'G') {
		pattern += " G"
	}
	pattern = o.withEra(pattern)
	pattern = o.withYearName(pattern)
	if o.timeZoneName != "" && !strings.ContainsAny(patternLettersOf(pattern), "zZvVOXx") {
		// A zone written after a time is written against it; after a date it
		// is joined the way a date is joined to a time, which is with a comma
		// in some languages and a space in others.
		joiner := " "
		if !strings.ContainsAny(patternLettersOf(pattern), "hHkKms") {
			joiner = glueSeparator(gluePatterns)
		}
		pattern += joiner + zoneLetters(o.timeZoneName)
	}
	// A part of the day asked for alongside the hour takes the place of the
	// morning-or-afternoon the pattern would have written.
	if o.dayPeriod != "" {
		if strings.ContainsRune(patternLettersOf(pattern), 'a') {
			pattern = strings.Replace(pattern, "a", "B", 1)
		} else if !strings.ContainsAny(patternLettersOf(pattern), "bB") {
			pattern += " B"
		}
	}
	// The fractions of a second, which go after the seconds themselves.
	if o.fractional > 0 {
		digits := strings.Repeat("S", o.fractional)
		// The mark before them is the one this language writes a decimal
		// point with, or the one the numbering system asked for brings.
		point := l.Decimal
		if o.digits != "" && o.digits != l.Numbering {
			if own, _, ok := icu.NumberingMarks(o.digits); ok {
				point = own
			}
		}
		point = "'" + point + "'"
		if at := strings.Index(pattern, "ss"); at >= 0 {
			pattern = pattern[:at+2] + point + digits + pattern[at+2:]
		} else if at := strings.IndexByte(pattern, 's'); at >= 0 {
			pattern = pattern[:at+1] + point + digits + pattern[at+1:]
		} else {
			pattern += digits
		}
	}
	// The locale's pattern says what order the fields go in; the options say
	// how wide each one is written, and those are the caller's to choose.
	return o.applyWidths(pattern, calendarPattern)
}

func (o *dateOptions) calendarPatterns() ([4]string, [4]string, map[string]string) {
	if o.calendar == "" || o.calendar == "gregory" || o.calendar == "iso8601" {
		return o.locale.DatePatterns, o.locale.Glue, nil
	}
	if format, ok := o.locale.CalendarFormatFor(o.calendar); ok {
		return format.DatePatterns, o.locale.Glue, format.Skeletons
	}
	return o.locale.DatePatterns, o.locale.Glue, nil
}

// glueSeparator is what a language puts between the two halves of a date and
// time written together, which is what anything added to a pattern goes after.
func glueSeparator(gluePatterns [4]string) string {
	// The date is {0} here and the time is {1}, which is the way round the
	// patterns are carried.
	glue := gluePatterns[3]
	first := strings.Index(glue, "{0}")
	second := strings.Index(glue, "{1}")
	if first < 0 || second < 0 || second < first {
		return " "
	}
	between := glue[first+3 : second]
	if between == "" {
		return " "
	}
	return between
}

// withWeekday puts the weekday where this locale puts it, which is in front in
// most languages and behind in some.
func (o *dateOptions) withWeekday(pattern string) string {
	patterns, _, _ := o.calendarPatterns()
	full := patterns[0]
	at := strings.IndexByte(full, 'E')
	if at < 0 {
		return "EEEE, " + pattern
	}
	end := at
	for end < len(full) && full[end] == 'E' {
		end++
	}
	before, after := full[:at], full[end:]
	// Whichever side of the weekday the rest of the date is on, the text
	// between them goes with it.
	if strings.TrimSpace(patternLettersOf(before)) == "" {
		return full[at:end] + separatorPrefix(after) + pattern
	}
	// The text before the weekday belongs to the field in front of it, which
	// this pattern may already end with: 5日 does not become 5日日.
	suffix := separatorSuffix(before)
	for suffix != "" {
		first, width := utf8.DecodeRuneInString(suffix)
		if !strings.HasSuffix(pattern, string(first)) {
			break
		}
		suffix = suffix[width:]
	}
	if strings.TrimSpace(suffix) == "" {
		// What is left is a space the locale's own full date puts there, and
		// a date asked for by field does without it.
		suffix = ""
	}
	return pattern + suffix + full[at:end]
}

// separatorPrefix is the literal that follows the weekday, up to the first
// field of the date itself.
func separatorPrefix(rest string) string {
	var b strings.Builder
	inQuote := false
	for i := 0; i < len(rest); i++ {
		c := rest[i]
		if c == '\'' {
			inQuote = !inQuote
			continue
		}
		if !inQuote && isPatternLetter(c) {
			break
		}
		b.WriteByte(c)
	}
	return b.String()
}

// separatorSuffix is the literal that comes before the weekday, back to the
// last field of the date itself.
func separatorSuffix(before string) string {
	end := len(before)
	for i := len(before) - 1; i >= 0; i-- {
		if isPatternLetter(before[i]) && before[i] != '\'' {
			return before[i+1 : end]
		}
	}
	return before
}

// applyWidths rewrites each field of a pattern to the width that was asked
// for, leaving the order and the literals as the locale had them.
func (o *dateOptions) applyWidths(pattern string, calendarPattern bool) string {
	// Only a width that asks for something the pattern does not already say is
	// applied. "numeric" is not such a width: it means "as this locale writes
	// it", and the locale has already said -- 5.1.2024 in German, 05/01/2024
	// in French, from patterns that were read for exactly this combination.
	want := map[byte]string{}
	if o.year == "2-digit" {
		want['y'] = "yy"
	}
	if o.month == "2-digit" {
		want['M'] = "MM"
		want['L'] = "LL"
		want['N'] = "NN"
	} else if !calendarPattern && (o.month == "long" || o.month == "short" || o.month == "narrow") {
		want['M'] = monthLetters(o.month)
		want['L'] = strings.ReplaceAll(want['M'], "M", "L")
	}
	if o.day == "2-digit" {
		want['d'] = "dd"
	}
	if o.weekday != "" {
		for _, letter := range []byte{'E', 'e', 'c'} {
			want[letter] = strings.ReplaceAll(weekdayLetters(o.weekday), "E", string(letter))
		}
	}
	if o.minute == "2-digit" {
		want['m'] = "mm"
	}
	if o.second == "2-digit" {
		want['s'] = "ss"
	}
	letter := "H"
	if o.hour12 {
		letter = "h"
	}
	switch {
	case o.hour == "2-digit":
		want['h'] = letter + letter
		want['H'] = want['h']
	case o.hour == "numeric" && len(want) > 0 &&
		!(o.hourSet && o.hour12 != o.locale.Hour12):
		// A numeric hour is written as one digit where something else about
		// the request differs from what the locale wrote its pattern for; a
		// request the locale has a pattern for is written the way that
		// pattern writes it, padding and all. So is a clock the locale does
		// not keep, since the pattern for it was written for this.
		want['h'] = letter
		want['H'] = letter
	}
	if len(want) == 0 {
		return pattern
	}

	var b strings.Builder
	inQuote := false
	for i := 0; i < len(pattern); {
		c := pattern[i]
		if c == '\'' {
			inQuote = !inQuote
			b.WriteByte(c)
			i++
			continue
		}
		if inQuote || !isPatternLetter(c) {
			_, width := utf8.DecodeRuneInString(pattern[i:])
			b.WriteString(pattern[i : i+width])
			i += width
			continue
		}
		n := 1
		for i+n < len(pattern) && pattern[i+n] == c {
			n++
		}
		if letters, ok := want[c]; ok {
			b.WriteString(letters)
		} else {
			b.WriteString(pattern[i : i+n])
		}
		i += n
	}
	return b.String()
}

func romanMonth(month int) string {
	if month < 1 || month > 13 {
		return strconv.Itoa(month)
	}
	values := [...]string{"", "I", "II", "III", "IV", "V", "VI", "VII", "VIII", "IX", "X", "XI", "XII", "XIII"}
	return values[month]
}

func hebrewNumber(value int, year bool) string {
	if year && value >= 1000 {
		value %= 1000
	}
	if value <= 0 {
		return strconv.Itoa(value)
	}
	values := [...]struct {
		value  int
		letter string
	}{
		{400, "ת"}, {300, "ש"}, {200, "ר"}, {100, "ק"},
		{90, "צ"}, {80, "פ"}, {70, "ע"}, {60, "ס"}, {50, "נ"},
		{40, "מ"}, {30, "ל"}, {20, "כ"}, {10, "י"}, {9, "ט"},
		{8, "ח"}, {7, "ז"}, {6, "ו"}, {5, "ה"}, {4, "ד"},
		{3, "ג"}, {2, "ב"}, {1, "א"},
	}
	var b strings.Builder
	for value > 0 {
		// Fifteen and sixteen avoid spelling a divine name.
		if value == 15 {
			b.WriteString("טו")
			break
		}
		if value == 16 {
			b.WriteString("טז")
			break
		}
		for _, item := range values {
			if value >= item.value {
				b.WriteString(item.letter)
				value -= item.value
				break
			}
		}
	}
	runes := []rune(b.String())
	if len(runes) == 1 {
		return string(runes) + "׳"
	}
	return string(runes[:len(runes)-1]) + "״" + string(runes[len(runes)-1])
}

// skeleton names the combination of fields that was asked for, in the form the
// locale data is keyed by.
func (o *dateOptions) skeleton() string {
	var b strings.Builder
	if o.year != "" {
		b.WriteString("y")
	}
	switch o.month {
	case "long":
		b.WriteString("MMMM")
	case "short":
		b.WriteString("MMM")
	case "narrow":
		b.WriteString("MMMMM")
	case "numeric", "2-digit":
		b.WriteString("M")
	}
	if o.day != "" {
		b.WriteString("d")
	}
	// A time is keyed by the clock it is written on, because a locale writes
	// the two differently: 9:30 AM on one, 09:30 on the other. Which clock
	// that is only matters when the caller chose it; otherwise the locale's
	// own time pattern is the one to use.
	if o.hour != "" {
		if o.hourSet && !o.hour12 {
			b.WriteString("H")
		} else {
			b.WriteString("h")
		}
	}
	if o.minute != "" {
		b.WriteString("m")
	}
	if o.second != "" {
		b.WriteString("s")
	}
	if o.hour != "" && o.hourSet && o.hour12 {
		// The twelve-hour keys are written apart from the natural ones.
		return b.String() + "12"
	}
	// A weekday or an era is not part of the key; those are added around what
	// the key produced.
	return b.String()
}

// splitSkeletons looks the date and the time up separately, for a request that
// asks for both and that the locale has no single pattern for.
func (o *dateOptions) splitSkeletons() (string, string) {
	dateOnly := *o
	dateOnly.hour, dateOnly.minute, dateOnly.second = "", "", ""
	timeOnly := *o
	timeOnly.weekday, timeOnly.era = "", ""
	timeOnly.year, timeOnly.month, timeOnly.day = "", "", ""

	_, _, calendarSkeletons := o.calendarPatterns()
	date, dateOK := calendarSkeletons[dateOnly.skeleton()]
	if !dateOK {
		date, dateOK = o.locale.Skeletons[dateOnly.skeleton()]
	}
	time, timeOK := o.locale.Skeletons[timeOnly.skeleton()]
	if !dateOK || !timeOK {
		return "", ""
	}
	if o.weekday != "" {
		date = dateOnly.withWeekday(date)
	}
	return date, time
}

// buildPattern puts a pattern together for a combination the locale has no
// pattern for, in the order that locale writes a date.
func (o *dateOptions) buildPattern() string {
	l := o.locale
	datePatterns, _, _ := o.calendarPatterns()
	order := dateFieldOrder(datePatterns[3])
	sep := dateSeparator(datePatterns[3])

	var date []string
	for _, field := range order {
		switch field {
		case 'y':
			if o.year != "" {
				date = append(date, yearLetters(o.year))
			}
		case 'M':
			if o.month != "" {
				date = append(date, monthLetters(o.month))
			}
		case 'd':
			if o.day != "" {
				date = append(date, dayLetters(o.day))
			}
		}
	}

	var parts []string
	if o.weekday != "" {
		weekday := weekdayLetters(o.weekday)
		if o.year == "" && o.month == "" && o.day == "" {
			weekday = strings.ReplaceAll(weekday, "E", "c")
		}
		parts = append(parts, weekday)
	}
	if len(date) > 0 {
		joiner := sep
		// A month written as a name does not take the separator a number does.
		if o.month == "long" || o.month == "short" || o.month == "narrow" {
			joiner = " "
		}
		parts = append(parts, strings.Join(date, joiner))
	}
	if o.era != "" {
		parts = append(parts, "G")
	}

	var clock []string
	if o.hour != "" {
		if o.hour12 {
			clock = append(clock, widthLetters("h", o.hour))
		} else {
			clock = append(clock, widthLetters("H", o.hour))
		}
	}
	// A minute and a second are written in twos whatever was asked for, which
	// is what every locale's own time pattern does: 3:04:05, not 3:4:5.
	if o.minute != "" {
		clock = append(clock, "mm")
	}
	if o.second != "" {
		clock = append(clock, "ss")
	}
	timePart := strings.Join(clock, ":")
	if timePart != "" && o.hour != "" && o.hour12 {
		timePart += " a"
	}
	if timePart != "" {
		parts = append(parts, timePart)
	}
	if o.timeZoneName != "" {
		parts = append(parts, zoneLetters(o.timeZoneName))
	}
	if len(parts) == 0 {
		return l.DatePatterns[3]
	}
	return strings.Join(parts, ", ")
}

func yearLetters(width string) string {
	if width == "2-digit" {
		return "yy"
	}
	return "y"
}

func monthLetters(width string) string {
	switch width {
	case "long":
		return "MMMM"
	case "short":
		return "MMM"
	case "narrow":
		return "MMMMM"
	case "2-digit":
		return "MM"
	}
	return "M"
}

func dayLetters(width string) string {
	if width == "2-digit" {
		return "dd"
	}
	return "d"
}

func weekdayLetters(width string) string {
	switch width {
	case "short":
		return "EEE"
	case "narrow":
		return "EEEEE"
	}
	return "EEEE"
}

func widthLetters(letter, width string) string {
	if width == "2-digit" {
		return letter + letter
	}
	return letter
}

// dateFieldOrder reports which of the year, month and day comes first in a
// pattern, which is what differs between languages.
func dateFieldOrder(pattern string) []byte {
	var out []byte
	seen := map[byte]bool{}
	inQuote := false
	for i := 0; i < len(pattern); i++ {
		c := pattern[i]
		if c == '\'' {
			inQuote = !inQuote
			continue
		}
		if inQuote {
			continue
		}
		switch c {
		case 'y', 'M', 'd':
			if !seen[c] {
				seen[c] = true
				out = append(out, c)
			}
		}
	}
	if len(out) == 0 {
		return []byte{'y', 'M', 'd'}
	}
	return out
}

// dateSeparator is what a locale puts between the numbers of a short date.
func dateSeparator(pattern string) string {
	inQuote := false
	for i := 0; i < len(pattern); i++ {
		c := pattern[i]
		if c == '\'' {
			inQuote = !inQuote
			continue
		}
		if inQuote || isPatternLetter(c) {
			continue
		}
		return string(c)
	}
	return "/"
}
