package vm

import (
	"fmt"
	"math"
	"strconv"
	"strings"
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
	pattern string
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
func (o *dateOptions) setZone(r *Runtime, zone string) error {
	switch {
	case zone == "":
		// The machine's own zone, which is the one a Date is written in.
		o.zone = r.location()
		o.timeZone = r.localZoneName()
		return nil
	case isUTCName(zone):
		o.zone = time.UTC
		o.timeZone = "UTC"
		return nil
	}
	// An offset written out rather than a name: +03:00, -0800, +05:45.
	if minutes, name, ok := parseZoneOffset(zone); ok {
		o.zone = time.FixedZone(name, minutes*60)
		o.timeZone = name
		return nil
	}
	// Any zone the machine has the data for. Loading it reads the zone files
	// the operating system keeps, or the copy a host embedded by importing
	// time/tzdata; a script cannot reach either.
	name := canonicalZone(zone)
	loc, err := time.LoadLocation(name)
	if err != nil {
		return r.throwRangeError("there is no such time zone here: %s", zone)
	}
	o.zone, o.timeZone = loc, name
	return nil
}

// parseZoneOffset reads a zone written as an offset from Greenwich, and writes
// it back the one way it is written: a sign, two digits, a colon, two digits,
// and the seconds left off when there are none.
func parseZoneOffset(s string) (minutes int, name string, ok bool) {
	// A minus may be written as the sign a mathematician would use.
	s = strings.Replace(s, "\u2212", "-", 1)
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

	// The digits of a locale that does not use the ASCII ones.
	if l.Digits != "" {
		for i, piece := range out {
			if piece.kind != "literal" {
				out[i].value = localiseDigits(piece.value, l.Digits)
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

	switch letter {
	case 'y', 'u':
		year := t.Year()
		if l.Calendar == "buddhist" {
			// Thailand counts from the Buddhist era, which is the same year
			// under another number.
			year += 543
		}
		if year <= 0 {
			// A year before the common era is written as its number in that
			// era, which is what the era field then says.
			year = 1 - year
		}
		if n == 2 {
			push("year", pad(year%100, 2))
			return
		}
		push("year", strconv.Itoa(year))
	case 'G':
		era := l.Eras[1]
		if t.Year() <= 0 {
			era = l.Eras[0]
		}
		push("era", era)
	case 'M', 'L':
		month := int(t.Month())
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
	case 'd':
		push("day", pad(t.Day(), n))
	case 'E', 'e', 'c':
		day := int(t.Weekday())
		switch {
		case n >= 5:
			push("weekday", nameAt(l.DaysNarrow, day, ""))
		case n == 4:
			push("weekday", nameAt(l.Days, day, ""))
		default:
			push("weekday", nameAt(l.DaysShort, day, ""))
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
	case 'z', 'v':
		push("timeZoneName", o.zoneName(t, n >= 4 || o.timeZoneName == "long"))
	case 'Z', 'O', 'V', 'X', 'x':
		_, offset := t.Zone()
		push("timeZoneName", offsetName(offset))
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

// zoneName is what the zone is called at this instant: the name it has in
// English where it has one, and the offset from Greenwich otherwise -- which
// is what most zones outside the Americas are called anyway.
func (o *dateOptions) zoneName(t time.Time, long bool) string {
	if o.timeZone == "UTC" {
		return "UTC"
	}
	_, offset := t.Zone()
	// A name only where the language it is in is the language being written.
	if strings.HasPrefix(o.locale.Tag, "en") {
		short, full := icu.ZoneName(o.timeZone, offset/60)
		if long && full != "" {
			return full
		}
		if !long && short != "" {
			return short
		}
	}
	return offsetName(offset)
}

// offsetName is what a zone with no name of its own is called.
func offsetName(offset int) string {
	if offset == 0 {
		return "GMT"
	}
	sign := "+"
	if offset < 0 {
		sign = "-"
		offset = -offset
	}
	hours := offset / 3600
	minutes := (offset % 3600) / 60
	if minutes == 0 {
		return "GMT" + sign + strconv.Itoa(hours)
	}
	return "GMT" + sign + strconv.Itoa(hours) + ":" + pad2(minutes)
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
	widths := map[string]int{"full": 0, "long": 1, "medium": 2, "short": 3}

	// The part of the day on its own, which is a field no skeleton names.
	if o.dayPeriod != "" && o.dateStyle == "" && o.timeStyle == "" &&
		o.weekday == "" && o.era == "" && o.year == "" && o.month == "" &&
		o.day == "" && o.hour == "" && o.minute == "" && o.second == "" {
		return "B"
	}

	if o.dateStyle != "" || o.timeStyle != "" {
		date, time := "", ""
		if i, ok := widths[o.dateStyle]; ok {
			date = l.DatePatterns[i]
		}
		if i, ok := widths[o.timeStyle]; ok {
			time = l.TimePatterns[i]
		}
		switch {
		case date != "" && time != "":
			// Both, joined the way the locale joins them: " at ", " um ", or
			// a space, which is part of the language rather than of either
			// pattern.
			glue := l.Glue[widths[o.dateStyle]]
			if !strings.Contains(glue, "{0}") {
				glue = "{0}, {1}"
			}
			joined := strings.Replace(glue, "{0}", date, 1)
			return strings.Replace(joined, "{1}", time, 1)
		case date != "":
			return date
		default:
			return time
		}
	}

	// A set of fields: the locale's own order for that combination, when it
	// has one, and otherwise the fields in the order its short date puts them.
	pattern, ok := l.Skeletons[o.skeleton()]
	if !ok {
		// A date and a time asked for together are the locale's pattern for
		// each, joined the way it joins them.
		if date, time := o.splitSkeletons(); date != "" && time != "" {
			// The shortest glue, which is what a request by field gets: a
			// comma in English, a space in French.
			glue := l.Glue[3]
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
	if o.weekday != "" && !strings.ContainsRune(patternLettersOf(pattern), 'E') {
		pattern = o.withWeekday(pattern)
	}
	if o.era != "" && !strings.ContainsRune(patternLettersOf(pattern), 'G') {
		pattern += " G"
	}
	if o.timeZoneName != "" && !strings.ContainsAny(patternLettersOf(pattern), "zZvVOXx") {
		pattern += " z"
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
		if at := strings.Index(pattern, "ss"); at >= 0 {
			pattern = pattern[:at+2] + "." + digits + pattern[at+2:]
		} else if at := strings.IndexByte(pattern, 's'); at >= 0 {
			pattern = pattern[:at+1] + "." + digits + pattern[at+1:]
		} else {
			pattern += digits
		}
	}
	// The locale's pattern says what order the fields go in; the options say
	// how wide each one is written, and those are the caller's to choose.
	return o.applyWidths(pattern)
}

// withWeekday puts the weekday where this locale puts it, which is in front in
// most languages and behind in some.
func (o *dateOptions) withWeekday(pattern string) string {
	full := o.locale.DatePatterns[0]
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
func (o *dateOptions) applyWidths(pattern string) string {
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
	} else if o.month == "long" || o.month == "short" || o.month == "narrow" {
		want['M'] = monthLetters(o.month)
	}
	if o.day == "2-digit" {
		want['d'] = "dd"
	}
	if o.weekday != "" {
		want['E'] = weekdayLetters(o.weekday)
	}
	letter := "H"
	if o.hour12 {
		letter = "h"
	}
	switch {
	case o.hour == "2-digit":
		want['h'] = letter + letter
		want['H'] = want['h']
	case o.hour == "numeric" && !(o.hourSet && o.hour12 != o.locale.Hour12):
		// A numeric hour is written as one digit, unless the clock itself was
		// changed: switching a twelve-hour locale to the other clock takes
		// that locale's own pattern for it, padding and all.
		want['h'] = letter
		want['H'] = letter
	}
	if o.minute == "2-digit" {
		want['m'] = "mm"
	}
	if o.second == "2-digit" {
		want['s'] = "ss"
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

	date, dateOK := o.locale.Skeletons[dateOnly.skeleton()]
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
	order := dateFieldOrder(l.DatePatterns[3])
	sep := dateSeparator(l.DatePatterns[3])

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
		parts = append(parts, weekdayLetters(o.weekday))
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
		parts = append(parts, "z")
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
