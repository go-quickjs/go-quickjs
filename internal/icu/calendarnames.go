package icu

import (
	"strconv"
	"strings"
	"sync"
)

// What the calendars call their months and their years, and where the ones
// that cannot be computed put their months.
//
// Both are kept apart from the rest of the locale data and unpacked only when
// a program asks for a calendar that is not the common one, since most never
// do.

// CalendarNames is what a language calls the months and the eras of a
// calendar. The months are keyed by the number the calendar gives them, with a
// mark for a month only a long year has and for the years that have one; the
// eras are in order, so that the first is the one a date before all the others
// falls in.
type CalendarNames struct {
	// Months and Eras are indexed by width: long, short, narrow.
	Months [3]map[string]string
	Eras   [3][]string
	// Cycle is what a lunisolar year is called where its years run in a cycle
	// of sixty rather than counting upwards.
	Cycle []string
}

// CalendarNamesFor is what this locale calls a calendar's months and eras, and
// whether it has anything to say about it at all.
func (l *Locale) CalendarNamesFor(name string) (*CalendarNames, bool) {
	loadCalendars()
	at, ok := calendarIndex[l.Tag]
	if !ok {
		at, ok = calendarIndex["en"]
		if !ok {
			return nil, false
		}
	}
	which, ok := at[name]
	if !ok || which >= len(calendarEntries) {
		return nil, false
	}
	return calendarEntries[which], true
}

var (
	calendarOnce    sync.Once
	calendarEntries []*CalendarNames
	calendarIndex   map[string]map[string]int
)

func loadCalendars() {
	calendarOnce.Do(func() {
		text, err := inflate(calendarNames)
		if err != nil {
			return
		}
		entries, index, _ := strings.Cut(text, "\n\n")
		for _, line := range strings.Split(entries, "\n") {
			if line == "" {
				continue
			}
			names := &CalendarNames{}
			for i := range names.Months {
				names.Months[i] = map[string]string{}
			}
			for _, field := range strings.Split(line, "\x01") {
				key, value, ok := strings.Cut(field, "\t")
				if !ok || len(key) < 2 {
					continue
				}
				width := widthOf(key[1:])
				if width < 0 && key != "cycle" {
					continue
				}
				switch key[0] {
				case 'm':
					for _, item := range strings.Split(value, "|") {
						if number, name, ok := strings.Cut(item, "="); ok {
							names.Months[width][number] = name
						}
					}
				case 'e':
					if value != "" {
						names.Eras[width] = strings.Split(value, "|")
					}
				case 'c':
					if value != "" {
						names.Cycle = strings.Split(value, "|")
					}
				}
			}
			calendarEntries = append(calendarEntries, names)
		}
		calendarIndex = map[string]map[string]int{}
		for _, line := range strings.Split(index, "\n") {
			tag, rest, ok := strings.Cut(line, "\t")
			if !ok {
				continue
			}
			at := map[string]int{}
			for _, field := range strings.Split(rest, "\x01") {
				if name, which, ok := strings.Cut(field, "="); ok {
					n, err := strconv.Atoi(which)
					if err == nil {
						at[name] = n
					}
				}
			}
			calendarIndex[tag] = at
		}
	})
}

func widthOf(name string) int {
	switch name {
	case "long":
		return 0
	case "short":
		return 1
	case "narrow":
		return 2
	}
	return -1
}

// --- where the months fall ---------------------------------------------------

// monthTable is where a calendar that cannot be computed puts its months: the
// day the first of them begins, which month that is, and how long each month
// from there runs.
type monthTable struct {
	from    int
	year    int
	month   int
	lengths string
}

var (
	monthTableOnce sync.Once
	monthTables    map[string]*monthTable
	reignDays      []int
)

func loadMonthTables() {
	monthTableOnce.Do(func() {
		monthTables = map[string]*monthTable{}
		text, err := inflate(calendarTables)
		if err != nil {
			return
		}
		for _, line := range strings.Split(text, "\n") {
			fields := strings.Split(line, "\t")
			if len(fields) == 2 && fields[0] == "eras" {
				for _, day := range strings.Split(fields[1], ",") {
					if n, err := strconv.Atoi(day); err == nil {
						reignDays = append(reignDays, n)
					}
				}
				continue
			}
			if len(fields) != 5 {
				continue
			}
			from, err1 := strconv.Atoi(fields[1])
			year, err2 := strconv.Atoi(fields[2])
			month, err3 := strconv.Atoi(fields[3])
			if err1 != nil || err2 != nil || err3 != nil {
				continue
			}
			monthTables[fields[0]] = &monthTable{
				from: from, year: year, month: month, lengths: fields[4],
			}
		}
	})
}

// dateFrom finds a day in a table of months: which year and month it falls in,
// and how far into the month it is. A day outside what the table covers is
// answered by the arithmetic instead.
func (t *monthTable) dateFrom(fixed, monthsInYear int) (year, month, day int, ok bool) {
	if fixed < t.from {
		return 0, 0, 0, false
	}
	at, year, month := t.from, t.year, t.month
	for i := 0; i < len(t.lengths); i++ {
		length := int(t.lengths[i]-'0') + 28
		if fixed < at+length {
			return year, month, fixed - at + 1, true
		}
		at += length
		month++
		if month > monthsInYear {
			month, year = 1, year+1
		}
	}
	return 0, 0, 0, false
}
