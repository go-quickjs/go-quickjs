package vm

import (
	"fmt"
	"strconv"
)

// A Temporal time zone written as an offset from UTC.

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
