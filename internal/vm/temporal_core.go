package vm

import (
	"errors"
	"math/big"
	"strconv"
	"strings"
)

const (
	temporalNanosecondsPerSecond = int64(1_000_000_000)
	temporalSecondsPerDay        = int64(86_400)
	temporalMaxEpochSeconds      = int64(8_640_000_000_000)
)

var errInvalidTemporalInstant = errors.New("invalid Temporal instant")

// temporalInstant avoids an int64 nanosecond count, which cannot represent
// Temporal's full 100-million-day range. Nanosecond is always below one second.
type temporalInstant struct {
	epochSeconds int64
	nanosecond   uint32
}

func temporalInstantFromEpochNanoseconds(epochNanoseconds *big.Int) (temporalInstant, bool) {
	limit := new(big.Int).Mul(big.NewInt(temporalMaxEpochSeconds), big.NewInt(temporalNanosecondsPerSecond))
	if new(big.Int).Abs(new(big.Int).Set(epochNanoseconds)).Cmp(limit) > 0 {
		return temporalInstant{}, false
	}
	seconds, remainder := new(big.Int), new(big.Int)
	seconds.QuoRem(epochNanoseconds, big.NewInt(temporalNanosecondsPerSecond), remainder)
	if remainder.Sign() < 0 {
		seconds.Sub(seconds, big.NewInt(1))
		remainder.Add(remainder, big.NewInt(temporalNanosecondsPerSecond))
	}
	return temporalInstant{
		epochSeconds: seconds.Int64(),
		nanosecond:   uint32(remainder.Int64()),
	}, true
}

func (instant temporalInstant) epochNanoseconds() *big.Int {
	result := new(big.Int).Mul(big.NewInt(instant.epochSeconds), big.NewInt(temporalNanosecondsPerSecond))
	return result.Add(result, big.NewInt(int64(instant.nanosecond)))
}

func (instant temporalInstant) addNanoseconds(delta *big.Int) (temporalInstant, bool) {
	total := instant.epochNanoseconds()
	total.Add(total, delta)
	return temporalInstantFromEpochNanoseconds(total)
}

func compareTemporalInstants(left, right temporalInstant) int {
	if left.epochSeconds < right.epochSeconds {
		return -1
	}
	if left.epochSeconds > right.epochSeconds {
		return 1
	}
	if left.nanosecond < right.nanosecond {
		return -1
	}
	if left.nanosecond > right.nanosecond {
		return 1
	}
	return 0
}

type temporalISODateTime struct {
	year, month, day     int
	hour, minute, second int
	millisecond          int
	microsecond          int
	nanosecond           int
}

func (date temporalISODateTime) valid() bool {
	if date.month < 1 || date.month > 12 || date.day < 1 || date.day > isoDaysInMonth(date.year, date.month) {
		return false
	}
	return date.hour >= 0 && date.hour <= 23 && date.minute >= 0 && date.minute <= 59 &&
		date.second >= 0 && date.second <= 59 && date.millisecond >= 0 && date.millisecond <= 999 &&
		date.microsecond >= 0 && date.microsecond <= 999 && date.nanosecond >= 0 && date.nanosecond <= 999
}

func (date temporalISODateTime) localEpochSeconds() int64 {
	days := isoDaysFromCivil(int64(date.year), date.month, date.day)
	return days*temporalSecondsPerDay + int64(date.hour*3600+date.minute*60+date.second)
}

func (date temporalISODateTime) subsecondNanoseconds() int64 {
	return int64(date.millisecond)*1_000_000 + int64(date.microsecond)*1_000 + int64(date.nanosecond)
}

func isoDaysInMonth(year, month int) int {
	switch month {
	case 2:
		if isLeapYear(year) {
			return 29
		}
		return 28
	case 4, 6, 9, 11:
		return 30
	default:
		return 31
	}
}

// isoDaysFromCivil is the proleptic-Gregorian day number relative to the Unix
// epoch. Floor division keeps years before year zero on the same calendar.
func isoDaysFromCivil(year int64, month, day int) int64 {
	if month <= 2 {
		year--
	}
	era := floorDivInt64(year, 400)
	yearOfEra := year - era*400
	adjustedMonth := int64(month)
	if month > 2 {
		adjustedMonth -= 3
	} else {
		adjustedMonth += 9
	}
	dayOfYear := (153*adjustedMonth+2)/5 + int64(day) - 1
	dayOfEra := yearOfEra*365 + yearOfEra/4 - yearOfEra/100 + dayOfYear
	return era*146097 + dayOfEra - 719468
}

func isoCivilFromDays(days int64) (year int, month, day int) {
	days += 719468
	era := floorDivInt64(days, 146097)
	dayOfEra := days - era*146097
	yearOfEra := (dayOfEra - dayOfEra/1460 + dayOfEra/36524 - dayOfEra/146096) / 365
	y := yearOfEra + era*400
	dayOfYear := dayOfEra - (365*yearOfEra + yearOfEra/4 - yearOfEra/100)
	monthPrime := (5*dayOfYear + 2) / 153
	d := dayOfYear - (153*monthPrime+2)/5 + 1
	m := monthPrime + 3
	if monthPrime >= 10 {
		m = monthPrime - 9
		y++
	}
	return int(y), int(m), int(d)
}

func floorDivInt64(value, divisor int64) int64 {
	quotient, remainder := value/divisor, value%divisor
	if remainder < 0 {
		quotient--
	}
	return quotient
}

func parseTemporalInstant(input string) (temporalInstant, error) {
	date, offsetNanoseconds, err := parseTemporalInstantFields(input)
	if err != nil {
		return temporalInstant{}, err
	}
	total := new(big.Int).Mul(big.NewInt(date.localEpochSeconds()), big.NewInt(temporalNanosecondsPerSecond))
	total.Add(total, big.NewInt(date.subsecondNanoseconds()))
	total.Sub(total, big.NewInt(offsetNanoseconds))
	instant, ok := temporalInstantFromEpochNanoseconds(total)
	if !ok {
		return temporalInstant{}, errInvalidTemporalInstant
	}
	return instant, nil
}

func parseTemporalInstantFields(input string) (temporalISODateTime, int64, error) {
	s := input
	if s == "" {
		return temporalISODateTime{}, 0, errInvalidTemporalInstant
	}
	main, annotations, ok := splitTemporalAnnotations(s)
	if !ok || !validInstantAnnotations(annotations) {
		return temporalISODateTime{}, 0, errInvalidTemporalInstant
	}

	index := 0
	year, ok := parseTemporalYear(main, &index)
	if !ok {
		return temporalISODateTime{}, 0, errInvalidTemporalInstant
	}
	dashedDate := consumeByte(main, &index, '-')
	month, ok := parseFixedDigits(main, &index, 2)
	if !ok || dashedDate && !consumeByte(main, &index, '-') {
		return temporalISODateTime{}, 0, errInvalidTemporalInstant
	}
	day, ok := parseFixedDigits(main, &index, 2)
	if !ok || index >= len(main) || (main[index] != 'T' && main[index] != 't' && main[index] != ' ') {
		return temporalISODateTime{}, 0, errInvalidTemporalInstant
	}
	index++
	hour, ok := parseFixedDigits(main, &index, 2)
	if !ok {
		return temporalISODateTime{}, 0, errInvalidTemporalInstant
	}
	minute := 0
	colonTime := consumeByte(main, &index, ':')
	if colonTime || index < len(main) && main[index] >= '0' && main[index] <= '9' {
		minute, ok = parseFixedDigits(main, &index, 2)
		if !ok {
			return temporalISODateTime{}, 0, errInvalidTemporalInstant
		}
	}
	second, secondPresent := 0, false
	if consumeByte(main, &index, ':') || !colonTime && index < len(main) && main[index] >= '0' && main[index] <= '9' {
		second, ok = parseFixedDigits(main, &index, 2)
		if !ok {
			return temporalISODateTime{}, 0, errInvalidTemporalInstant
		}
		secondPresent = true
	}
	fraction := 0
	if index < len(main) && (main[index] == '.' || main[index] == ',') {
		if !secondPresent {
			return temporalISODateTime{}, 0, errInvalidTemporalInstant
		}
		index++
		start := index
		for index < len(main) && main[index] >= '0' && main[index] <= '9' && index-start < 9 {
			fraction = fraction*10 + int(main[index]-'0')
			index++
		}
		if index == start || index < len(main) && main[index] >= '0' && main[index] <= '9' {
			return temporalISODateTime{}, 0, errInvalidTemporalInstant
		}
		for digits := index - start; digits < 9; digits++ {
			fraction *= 10
		}
	}

	offset, ok := parseTemporalOffset(main, &index)
	if !ok || index != len(main) {
		return temporalISODateTime{}, 0, errInvalidTemporalInstant
	}
	if second == 60 {
		second = 59
	}
	date := temporalISODateTime{
		year: year, month: month, day: day,
		hour: hour, minute: minute, second: second,
		millisecond: fraction / 1_000_000,
		microsecond: fraction / 1_000 % 1_000,
		nanosecond:  fraction % 1_000,
	}
	if !date.valid() {
		return temporalISODateTime{}, 0, errInvalidTemporalInstant
	}
	return date, offset, nil
}

func splitTemporalAnnotations(s string) (string, []string, bool) {
	first := strings.IndexByte(s, '[')
	if first < 0 {
		return s, nil, !strings.ContainsRune(s, ']')
	}
	main := s[:first]
	var annotations []string
	for first < len(s) {
		if s[first] != '[' {
			return "", nil, false
		}
		end := strings.IndexByte(s[first+1:], ']')
		if end < 0 {
			return "", nil, false
		}
		end += first + 1
		if end == first+1 {
			return "", nil, false
		}
		annotations = append(annotations, s[first+1:end])
		first = end + 1
	}
	return main, annotations, true
}

func validInstantAnnotations(annotations []string) bool {
	calendarCount, seenTimeZone := 0, false
	criticalCalendar := false
	for _, annotation := range annotations {
		critical := strings.HasPrefix(annotation, "!")
		if critical {
			annotation = annotation[1:]
		}
		if key, value, keyed := strings.Cut(annotation, "="); keyed {
			if !validAnnotationKey(key) || value == "" {
				return false
			}
			if key == "u-ca" {
				calendarCount++
				if critical {
					criticalCalendar = true
				}
				if calendarCount > 1 && criticalCalendar {
					return false
				}
				continue
			}
			if critical {
				return false
			}
			continue
		}
		if seenTimeZone {
			return false
		}
		if !validTimeZoneAnnotation(annotation) {
			return false
		}
		seenTimeZone = true
	}
	return true
}

func validAnnotationKey(key string) bool {
	if key == "" {
		return false
	}
	for i := 0; i < len(key); i++ {
		c := key[i]
		if c >= 'a' && c <= 'z' || i > 0 && c >= '0' && c <= '9' || c == '_' || i > 0 && c == '-' {
			continue
		}
		return false
	}
	return true
}

func validTimeZoneAnnotation(annotation string) bool {
	if annotation == "" {
		return false
	}
	if annotation[0] == '+' || annotation[0] == '-' {
		index := 1
		hour, ok := parseFixedDigits(annotation, &index, 2)
		if !ok {
			return false
		}
		if index == len(annotation) {
			return hour <= 23
		}
		colon := consumeByte(annotation, &index, ':')
		minute, ok := parseFixedDigits(annotation, &index, 2)
		return ok && index == len(annotation) && hour <= 23 && minute <= 59 &&
			(colon || len(annotation) == 5)
	}
	for i := 0; i < len(annotation); i++ {
		c := annotation[i]
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' ||
			c == '/' || c == '_' || c == '-' || c == '+' || c == '.' {
			continue
		}
		return false
	}
	return true
}

func parseTemporalYear(s string, index *int) (int, bool) {
	if *index >= len(s) {
		return 0, false
	}
	sign, digits := 1, 4
	if s[*index] == '+' || s[*index] == '-' {
		if s[*index] == '-' {
			sign = -1
		}
		*index++
		digits = 6
	}
	year, ok := parseFixedDigits(s, index, digits)
	if !ok || sign < 0 && year == 0 {
		return 0, false
	}
	return sign * year, true
}

func parseTemporalOffset(s string, index *int) (int64, bool) {
	if *index >= len(s) {
		return 0, false
	}
	if s[*index] == 'Z' || s[*index] == 'z' {
		*index++
		return 0, true
	}
	if s[*index] != '+' && s[*index] != '-' {
		return 0, false
	}
	sign := int64(1)
	if s[*index] == '-' {
		sign = -1
	}
	*index++
	hour, ok := parseFixedDigits(s, index, 2)
	if !ok {
		return 0, false
	}
	minute := 0
	colonOffset := consumeByte(s, index, ':')
	if colonOffset || *index < len(s) && s[*index] >= '0' && s[*index] <= '9' {
		minute, ok = parseFixedDigits(s, index, 2)
		if !ok {
			return 0, false
		}
	}
	second, fraction := 0, int64(0)
	if consumeByte(s, index, ':') || !colonOffset && *index < len(s) && s[*index] >= '0' && s[*index] <= '9' {
		second, ok = parseFixedDigits(s, index, 2)
		if !ok {
			return 0, false
		}
		if *index < len(s) && (s[*index] == '.' || s[*index] == ',') {
			*index++
			start := *index
			for *index < len(s) && s[*index] >= '0' && s[*index] <= '9' && *index-start < 9 {
				fraction = fraction*10 + int64(s[*index]-'0')
				*index++
			}
			if *index == start || *index < len(s) && s[*index] >= '0' && s[*index] <= '9' {
				return 0, false
			}
			for digits := *index - start; digits < 9; digits++ {
				fraction *= 10
			}
		}
	}
	if hour > 23 || minute > 59 || second > 59 {
		return 0, false
	}
	total := (int64(hour*3600+minute*60+second) * temporalNanosecondsPerSecond) + fraction
	return sign * total, true
}

func parseFixedDigits(s string, index *int, count int) (int, bool) {
	if count < 0 || *index < 0 || *index+count > len(s) {
		return 0, false
	}
	value := 0
	for range count {
		c := s[*index]
		if c < '0' || c > '9' {
			return 0, false
		}
		value = value*10 + int(c-'0')
		*index++
	}
	return value, true
}

func consumeByte(s string, index *int, want byte) bool {
	if *index >= len(s) || s[*index] != want {
		return false
	}
	*index++
	return true
}

func (instant temporalInstant) isoDateTimeUTC() temporalISODateTime {
	days := floorDivInt64(instant.epochSeconds, temporalSecondsPerDay)
	seconds := instant.epochSeconds - days*temporalSecondsPerDay
	year, month, day := isoCivilFromDays(days)
	return temporalISODateTime{
		year: year, month: month, day: day,
		hour: int(seconds / 3600), minute: int(seconds / 60 % 60), second: int(seconds % 60),
		millisecond: int(instant.nanosecond) / 1_000_000,
		microsecond: int(instant.nanosecond) / 1_000 % 1_000,
		nanosecond:  int(instant.nanosecond) % 1_000,
	}
}

func (instant temporalInstant) string() string {
	date := instant.isoDateTimeUTC()
	var b strings.Builder
	if date.year >= 0 && date.year <= 9999 {
		writePaddedTemporalInt(&b, date.year, 4)
	} else {
		if date.year < 0 {
			b.WriteByte('-')
			writePaddedTemporalInt(&b, -date.year, 6)
		} else {
			b.WriteByte('+')
			writePaddedTemporalInt(&b, date.year, 6)
		}
	}
	b.WriteByte('-')
	writePaddedTemporalInt(&b, date.month, 2)
	b.WriteByte('-')
	writePaddedTemporalInt(&b, date.day, 2)
	b.WriteByte('T')
	writePaddedTemporalInt(&b, date.hour, 2)
	b.WriteByte(':')
	writePaddedTemporalInt(&b, date.minute, 2)
	b.WriteByte(':')
	writePaddedTemporalInt(&b, date.second, 2)
	if instant.nanosecond != 0 {
		fraction := strconv.FormatInt(int64(instant.nanosecond)+temporalNanosecondsPerSecond, 10)[1:]
		b.WriteByte('.')
		b.WriteString(strings.TrimRight(fraction, "0"))
	}
	b.WriteByte('Z')
	return b.String()
}

func writePaddedTemporalInt(b *strings.Builder, value, width int) {
	text := strconv.Itoa(value)
	for i := len(text); i < width; i++ {
		b.WriteByte('0')
	}
	b.WriteString(text)
}
