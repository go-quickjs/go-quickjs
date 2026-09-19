package vm

import (
	"testing"
	"time"

	"github.com/go-quickjs/go-quickjs/internal/icu"
)

func TestWeekdayContextNames(t *testing.T) {
	locale := &icu.Locale{
		Days:       []string{"alone-sun", "alone-mon", "alone-tue", "alone-wed", "alone-thu", "alone-fri", "alone-sat"},
		DaysFormat: []string{"format-sun", "format-mon", "format-tue", "format-wed", "format-thu", "format-fri", "format-sat"},
	}
	options := &dateOptions{locale: locale}
	friday := time.Date(2024, time.January, 5, 0, 0, 0, 0, time.UTC)

	for _, tc := range []struct {
		letter byte
		want   string
	}{
		{'E', "format-fri"},
		{'e', "format-fri"},
		{'c', "alone-fri"},
	} {
		var kind, got string
		options.field(func(k, value string) { kind, got = k, value }, friday, tc.letter, 4)
		if kind != "weekday" || got != tc.want {
			t.Errorf("%c weekday = (%q, %q), want (%q, %q)", tc.letter, kind, got, "weekday", tc.want)
		}
	}
}
