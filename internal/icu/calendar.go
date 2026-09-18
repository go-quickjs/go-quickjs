package icu

import (
	"strconv"
	"time"
)

// Not every year starts in January.
//
// The Islamic year is eleven days shorter than this one and its months follow
// the moon; the Hebrew year has a thirteenth month in seven years out of
// nineteen, and starts on a day chosen so that certain feasts do not fall on
// certain weekdays; the Coptic and Ethiopic years are twelve months of thirty
// days and five days over; the Japanese year is counted from the start of an
// emperor's reign. Which day falls in which month of which year is arithmetic,
// and this is that arithmetic.
//
// Every calendar here is reckoned from a day number: the count of days since
// the first of January in the year 1 of the calendar this program was written
// in, which is what the literature calls a fixed date. Converting between two
// calendars is converting each to that count.

// A Date as a calendar counts it.
type Date struct {
	// Era is which era the year is counted in, as an index into the names the
	// locale carries: 0 is the first of them.
	Era int
	// Year is the year within that era, and Month the number this calendar
	// gives the month, counting from one.
	Year, Month, Day int
	// LongYear says the year has a thirteenth month, which is what decides
	// what the months of it are called.
	LongYear bool
	// Leap says this month is one only a long year has.
	Leap bool
	// RelatedYear is the year of the common calendar that most of this year
	// falls in, and YearName what this calendar calls the year where it names
	// them rather than numbering them.
	RelatedYear int
	YearName    string
}

// Calendars lists the ones this engine reckons.
func Calendars() []string {
	return []string{"buddhist", "chinese", "coptic", "dangi", "ethiopic",
		"ethioaa", "gregory", "hebrew", "indian", "islamic-civil",
		"islamic-tbla", "islamic-umalqura", "iso8601",
		"japanese", "persian", "roc"}
}

// HasCalendar reports whether a calendar is one this engine reckons. The two
// abstract Islamic calendars remain valid requests, but a formatter resolves
// them to a concrete calendar before reporting its choice.
func HasCalendar(name string) bool {
	if name == "islamic" || name == "islamic-rgsa" {
		return true
	}
	for _, known := range Calendars() {
		if known == name {
			return true
		}
	}
	return false
}

// unixEpoch is the day number of the first of January 1970, counting from the
// first of January in the year 1.
const unixEpoch = 719163

// DateIn is an instant as a calendar counts it. The instant is taken as it
// stands: whatever zone it was read in is the zone the day is counted in.
func DateIn(calendar string, t time.Time) Date {
	year, month, day := t.Date()
	fixed := fixedFromGregorian(year, int(month), day)
	switch calendar {
	case "buddhist":
		// The same months and days, counted from the Buddha's death.
		out := gregorianDate(year, int(month), day)
		out.Year = year + 543
		out.Era = 0
		return out
	case "roc":
		// Counted from the founding of the republic, and backwards before it.
		out := gregorianDate(year, int(month), day)
		out.Era, out.Year = 1, year-1911
		if out.Year <= 0 {
			out.Era, out.Year = 0, 1912-year
		}
		return out
	case "japanese":
		return japaneseDate(year, int(month), day, fixed)
	case "islamic", "islamic-umalqura":
		// These two follow the moon and a table kept in Saudi Arabia rather
		// than a rule, so they are read from what was recorded of them; a day
		// outside what was recorded falls back on the rule.
		loadMonthTables()
		if table, ok := monthTables[calendar]; ok {
			if year, month, day, ok := table.dateFrom(fixed, 12); ok {
				return Date{Era: 1, Year: year, Month: month, Day: day,
					RelatedYear: gregorianYearOf(fixed)}
			}
		}
		return islamicDate(fixed, islamicEpoch)
	case "islamic-civil", "islamic-rgsa":
		return islamicDate(fixed, islamicEpoch)
	case "islamic-tbla":
		// The same reckoning, counted from the day before.
		return islamicDate(fixed, islamicEpoch-1)
	case "hebrew":
		return hebrewDate(fixed)
	case "coptic":
		return copticDate(fixed, copticEpoch, 0)
	case "ethiopic":
		out := copticDate(fixed, ethiopicEpoch, 1)
		if out.Year <= 0 {
			// Before Anno Mundi year 1, Ethiopic dates use the overlapping
			// Amete Alem count, whose year is 5500 years ahead.
			out.Era, out.Year = 0, out.Year+5500
		}
		return out
	case "ethioaa":
		// The same calendar counted from the creation of the world, which is
		// one era and not two.
		out := copticDate(fixed, ethiopicEpoch, 0)
		out.Year += 5500
		return out
	case "chinese", "dangi":
		return lunisolarDate(calendar, fixed)
	case "indian":
		return indianDate(fixed)
	case "persian":
		// The Persian year begins at the equinox as it falls in Tehran, which
		// is astronomy; what was recorded of it is used where it reaches.
		loadMonthTables()
		if table, ok := monthTables["persian"]; ok {
			if year, month, day, ok := table.dateFrom(fixed, 12); ok {
				return Date{Era: 0, Year: year, Month: month, Day: day,
					RelatedYear: gregorianYearOf(fixed)}
			}
		}
		return persianDate(fixed)
	}
	return gregorianDate(year, int(month), day)
}

func gregorianDate(year, month, day int) Date {
	out := Date{Era: 1, Year: year, Month: month, Day: day, RelatedYear: year}
	if year <= 0 {
		out.Era, out.Year = 0, 1-year
	}
	return out
}

// fixedFromGregorian is the day number of a date in the common calendar.
func fixedFromGregorian(year, month, day int) int {
	prior := year - 1
	fixed := 365*prior + prior/4 - prior/100 + prior/400 +
		(367*month-362)/12 + day
	switch {
	case month <= 2:
	case gregorianLeap(year):
		fixed--
	default:
		fixed -= 2
	}
	return fixed
}

func gregorianLeap(year int) bool {
	return year%4 == 0 && (year%100 != 0 || year%400 == 0)
}

// --- the calendars that follow the moon -------------------------------------

// islamicEpoch is the day the Islamic calendar counts from, which is the day
// after the flight to Medina.
const islamicEpoch = 227015

// islamicDate reckons the Islamic calendar the way a table does rather than
// the way the sky does: a year of 354 days with eleven long years in thirty,
// which is what a program can compute and what ICU does for the civil form.
func islamicDate(fixed, epoch int) Date {
	year := divFloor(30*(fixed-epoch)+10646, 10631)
	for fixedFromIslamic(year, 1, 1, epoch) > fixed {
		year--
	}
	for fixedFromIslamic(year+1, 1, 1, epoch) <= fixed {
		year++
	}
	month := 1
	for month < 12 && fixedFromIslamic(year, month+1, 1, epoch) <= fixed {
		month++
	}
	day := fixed - fixedFromIslamic(year, month, 1, epoch) + 1
	out := Date{Era: 1, Year: year, Month: month, Day: day}
	if year <= 0 {
		out.Era, out.Year = 0, 1-year
	}
	out.RelatedYear = gregorianYearOf(fixed)
	return out
}

func fixedFromIslamic(year, month, day, epoch int) int {
	return epoch - 1 + 354*(year-1) + divFloor(3+11*year, 30) +
		29*(month-1) + month/2 + day
}

// --- the Hebrew calendar ----------------------------------------------------

// hebrewEpoch is the day the Hebrew calendar counts the creation of the world
// from.
const hebrewEpoch = -1373427

// hebrewDate reckons the Hebrew calendar, whose year is lunar but kept in step
// with the sun by a thirteenth month in seven years out of nineteen, and whose
// first day is moved when it would otherwise fall on a day it may not.
func hebrewDate(fixed int) Date {
	// The year, found by guessing low and walking up.
	approx := divFloor(98496*(fixed-hebrewEpoch), 35975351) + 1
	year := approx - 1
	for hebrewNewYear(year+1) <= fixed {
		year++
	}
	long := hebrewLeapYear(year)

	// The month, counted from Tishri, which is where this calendar starts its
	// year even though it numbers its months from Nisan in some books.
	start := 1
	if fixed < fixedFromHebrew(year, 1, 1) {
		start = 7
	}
	month := start
	for fixed > fixedFromHebrew(year, month, hebrewMonthDays(year, month)) {
		month++
	}
	day := fixed - fixedFromHebrew(year, month, 1) + 1

	// Tishri is the first month of the year as this calendar writes it, and
	// Elul the last.
	written := month - 6
	if written < 1 {
		written += 12
		if long {
			written++
		}
	}
	return Date{Era: 0, Year: year, Month: written, Day: day, LongYear: long,
		Leap: long && month == 13, RelatedYear: gregorianYearOf(fixed)}
}

func hebrewLeapYear(year int) bool {
	return mod(7*year+1, 19) < 7
}

// hebrewNewYear is the day a Hebrew year begins, which is the day of the new
// moon of Tishri unless that day is one the year may not begin on.
func hebrewNewYear(year int) int {
	return hebrewEpoch + hebrewDelay(year) + hebrewDelayFurther(year)
}

// hebrewDelay is the day the mean new moon of Tishri falls on, counted in
// whole days from the epoch.
func hebrewDelay(year int) int {
	months := divFloor(235*year-234, 19)
	parts := 12084 + 13753*months
	day := months*29 + divFloor(parts, 25920)
	if mod(3*(day+1), 7) < 3 {
		day++
	}
	return day
}

// hebrewDelayFurther is the further day a year is put off by, so that it does
// not run one day too long or one day too short.
func hebrewDelayFurther(year int) int {
	last, now, next := hebrewDelay(year-1), hebrewDelay(year), hebrewDelay(year+1)
	switch {
	case next-now == 356:
		return 2
	case now-last == 382:
		return 1
	}
	return 0
}

func hebrewYearDays(year int) int {
	return hebrewNewYear(year+1) - hebrewNewYear(year)
}

// hebrewMonthDays is how many days a month of a Hebrew year has, counting the
// months from Nisan as the arithmetic does.
func hebrewMonthDays(year, month int) int {
	switch month {
	case 2, 4, 6, 10, 13:
		return 29
	case 12:
		if !hebrewLeapYear(year) {
			return 29
		}
		return 30
	case 8:
		if hebrewYearDays(year)%10 != 5 {
			return 29
		}
		return 30
	case 9:
		if hebrewYearDays(year)%10 == 3 {
			return 29
		}
		return 30
	}
	return 30
}

// fixedFromHebrew is the day number of a Hebrew date, with the months counted
// from Nisan.
func fixedFromHebrew(year, month, day int) int {
	fixed := hebrewNewYear(year) + day - 1
	if month < 7 {
		last := 13
		if !hebrewLeapYear(year) {
			last = 12
		}
		for m := 7; m <= last; m++ {
			fixed += hebrewMonthDays(year, m)
		}
		for m := 1; m < month; m++ {
			fixed += hebrewMonthDays(year, m)
		}
		return fixed
	}
	for m := 7; m < month; m++ {
		fixed += hebrewMonthDays(year, m)
	}
	return fixed
}

// --- the calendars of twelve months of thirty days --------------------------

const (
	copticEpoch   = 103605
	ethiopicEpoch = 2796
)

// copticDate reckons the Coptic and Ethiopic calendars, which are twelve
// months of thirty days and a short thirteenth of five days, or six in a leap
// year.
func copticDate(fixed, epoch, era int) Date {
	year := divFloor(4*(fixed-epoch)+1463, 1461)
	month := (fixed-copticNewYear(year, epoch))/30 + 1
	day := fixed - copticNewYear(year, epoch) - 30*(month-1) + 1
	out := Date{Era: era, Year: year, Month: month, Day: day,
		RelatedYear: gregorianYearOf(fixed)}
	if out.Year <= 0 && era == 0 {
		out.Year = 1 - out.Year
	}
	return out
}

func copticNewYear(year, epoch int) int {
	return epoch + 365*(year-1) + divFloor(year, 4)
}

// --- the calendars that follow the sun --------------------------------------

const (
	indianEpoch  = 78
	persianEpoch = 226896
)

// indianDate reckons the Indian national calendar, whose year starts at the
// spring equinox and whose first month is a day longer in a leap year.
func indianDate(fixed int) Date {
	year := gregorianYearOf(fixed) - indianEpoch
	if fixedFromIndian(year, 1, 1) > fixed {
		year--
	}
	month := 1
	for month < 12 && fixedFromIndian(year, month+1, 1) <= fixed {
		month++
	}
	day := fixed - fixedFromIndian(year, month, 1) + 1
	out := Date{Era: 0, Year: year, Month: month, Day: day,
		RelatedYear: gregorianYearOf(fixed)}
	if out.Year <= 0 {
		out.Year = 1 - out.Year
	}
	return out
}

// fixedFromIndian is the day number of a date in the Indian calendar. The year
// begins on the twenty-first of March in a leap year and the twenty-second in
// the others, and the first month is a day longer in a leap year.
func fixedFromIndian(year, month, day int) int {
	gregorian := year + indianEpoch
	leap := gregorianLeap(gregorian)
	start := fixedFromGregorian(gregorian, 3, 22)
	first := 30
	if leap {
		start, first = fixedFromGregorian(gregorian, 3, 21), 31
	}
	switch {
	case month == 1:
		return start + day - 1
	default:
		fixed := start + first
		for m := 2; m < month; m++ {
			if m <= 6 {
				fixed += 31
			} else {
				fixed += 30
			}
		}
		return fixed + day - 1
	}
}

// persianDate reckons the Persian calendar, whose year starts at the spring
// equinox as it falls in Tehran. The rule used here is the arithmetic one:
// eight leap years in thirty-three, which agrees with the sky for the years a
// program is likely to be asked about.
func persianDate(fixed int) Date {
	year := persianYear(fixed)
	yearDay := fixed - fixedFromPersian(year, 1, 1)
	var month, day int
	switch {
	case yearDay < 6*31:
		month, day = yearDay/31+1, yearDay%31+1
	default:
		month, day = (yearDay-6*31)/30+7, (yearDay-6*31)%30+1
	}
	out := Date{Era: 0, Year: year, Month: month, Day: day,
		RelatedYear: gregorianYearOf(fixed)}
	if out.Year <= 0 {
		out.Year = 1 - out.Year
	}
	return out
}

func persianYear(fixed int) int {
	year := divFloor(fixed-persianEpoch, 365) + 1
	for fixedFromPersian(year, 1, 1) > fixed {
		year--
	}
	for fixedFromPersian(year+1, 1, 1) <= fixed {
		year++
	}
	return year
}

func fixedFromPersian(year, month, day int) int {
	// Counted in cycles of thirty-three years, of which eight are long.
	prior := year - 1
	fixed := persianEpoch + 365*prior + divFloor(8*prior+21, 33)
	if month <= 6 {
		fixed += 31 * (month - 1)
	} else {
		fixed += 6*31 + 30*(month-7)
	}
	return fixed + day - 1
}

// --- the calendars that follow the moon and the sun both --------------------

// lunisolarDate reckons the Chinese and Korean calendars, whose months follow
// the moon and whose years are kept in step with the sun by a month said
// twice. Where the months fall is astronomy, so it is read from what was
// recorded of it rather than computed.
func lunisolarDate(calendar string, fixed int) Date {
	loadMonthTables()
	table, ok := monthTables[calendar]
	if !ok {
		return gregorianDate(gregorianFromFixed(fixed))
	}
	year, month, day, leap, ok := table.lunisolar(fixed)
	if !ok {
		return gregorianDate(gregorianFromFixed(fixed))
	}
	out := Date{Year: year, Month: month, Day: day, Leap: leap, RelatedYear: year}
	// The years run in a cycle of sixty, and the one that began in 1984 is the
	// first of a cycle.
	out.Era = mod(year-1984, 60)
	return out
}

// lunisolar walks the table of months, which says how long each one is and
// which of them is a month said twice.
func (t *monthTable) lunisolar(fixed int) (year, month, day int, leap, ok bool) {
	if fixed < t.from {
		return 0, 0, 0, false, false
	}
	at, year, month := t.from, t.year, t.month
	for i := 0; i < len(t.lengths); i++ {
		code := int(t.lengths[i] - '0')
		leap := code > 4
		if leap {
			code -= 4
		}
		// A month said twice keeps the number of the one before it; any other
		// month follows on, and the year turns after the twelfth.
		if i > 0 && !leap {
			month++
			if month > 12 {
				month, year = 1, year+1
			}
		}
		if fixed < at+code+28 {
			return year, month, fixed - at + 1, leap, true
		}
		at += code + 28
	}
	return 0, 0, 0, false, false
}

// --- the Japanese eras ------------------------------------------------------

// japaneseDate is the common calendar with the year counted from the start of
// an emperor's reign rather than from the start of the era.
func japaneseDate(year, month, day, fixed int) Date {
	out := gregorianDate(year, month, day)
	era := japaneseEraAt(fixed)
	if era < 0 {
		// Before the first era named here, which is written as the common
		// calendar writes it.
		return out
	}
	startYear, _, _ := gregorianFromFixed(reignDays[era])
	out.Era = era
	out.Year = year - startYear + 1
	return out
}

// japaneseEraAt is which reign a day falls in, or -1 before the first of them.
func japaneseEraAt(fixed int) int {
	loadMonthTables()
	for i := len(reignDays) - 1; i >= 0; i-- {
		if fixed >= reignDays[i] {
			return i
		}
	}
	return -1
}

// --- the arithmetic they all share ------------------------------------------

// gregorianFromFixed is the date in the common calendar of a day number.
func gregorianFromFixed(fixed int) (year, month, day int) {
	year = gregorianYearOf(fixed)
	prior := fixed - fixedFromGregorian(year, 1, 1)
	correction := 2
	switch {
	case fixed < fixedFromGregorian(year, 3, 1):
		correction = 0
	case gregorianLeap(year):
		correction = 1
	}
	month = (12*(prior+correction) + 373) / 367
	day = fixed - fixedFromGregorian(year, month, 1) + 1
	return year, month, day
}

// gregorianYearOf is the year of the common calendar a day number falls in.
func gregorianYearOf(fixed int) int {
	d0 := fixed - 1
	n400, d1 := divFloor(d0, 146097), mod(d0, 146097)
	n100, d2 := divFloor(d1, 36524), mod(d1, 36524)
	n4, d3 := divFloor(d2, 1461), mod(d2, 1461)
	n1 := divFloor(d3, 365)
	year := 400*n400 + 100*n100 + 4*n4 + n1
	if n100 == 4 || n1 == 4 {
		return year
	}
	return year + 1
}

// divFloor and mod round towards the lower number rather than towards zero,
// which is what calendar arithmetic wants of a year before the epoch.
func divFloor(a, b int) int {
	q := a / b
	if (a%b != 0) && ((a < 0) != (b < 0)) {
		q--
	}
	return q
}

func mod(a, b int) int {
	m := a % b
	if m != 0 && (m < 0) != (b < 0) {
		m += b
	}
	return m
}

// MonthKey is how a month of a year is filed among the names: its number, and
// a mark for the years that have a thirteenth month.
func (d Date) MonthKey() string {
	key := strconv.Itoa(d.Month)
	if d.Leap {
		key += "bis"
	}
	if d.LongYear {
		key += "L"
	}
	return key
}
