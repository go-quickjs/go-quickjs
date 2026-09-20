package vm

import (
	"math/big"
	"testing"
)

func TestTemporalInstantEpochNanoseconds(t *testing.T) {
	tests := []struct {
		text        string
		wantSeconds int64
		wantNanos   uint32
	}{
		{"0", 0, 0},
		{"1", 0, 1},
		{"999999999", 0, 999999999},
		{"1000000000", 1, 0},
		{"-1", -1, 999999999},
		{"-1000000000", -1, 0},
		{"-1000000001", -2, 999999999},
		{"8640000000000000000000", temporalMaxEpochSeconds, 0},
		{"-8640000000000000000000", -temporalMaxEpochSeconds, 0},
	}
	for _, test := range tests {
		t.Run(test.text, func(t *testing.T) {
			value, ok := new(big.Int).SetString(test.text, 10)
			if !ok {
				t.Fatal("bad test value")
			}
			instant, ok := temporalInstantFromEpochNanoseconds(value)
			if !ok {
				t.Fatal("instant rejected")
			}
			if instant.epochSeconds != test.wantSeconds || instant.nanosecond != test.wantNanos {
				t.Fatalf("instant = {%d, %d}, want {%d, %d}", instant.epochSeconds, instant.nanosecond, test.wantSeconds, test.wantNanos)
			}
			if got := instant.epochNanoseconds().String(); got != test.text {
				t.Fatalf("round trip = %s", got)
			}
		})
	}
	for _, text := range []string{"8640000000000000000001", "-8640000000000000000001"} {
		value, _ := new(big.Int).SetString(text, 10)
		if _, ok := temporalInstantFromEpochNanoseconds(value); ok {
			t.Fatalf("accepted out-of-range %s", text)
		}
	}
}

func TestTemporalISODateConversion(t *testing.T) {
	tests := []struct {
		year, month, day int
		wantDays         int64
	}{
		{1970, 1, 1, 0},
		{1969, 12, 31, -1},
		{2000, 2, 29, 11016},
		{0, 1, 1, -719528},
		{-1, 12, 31, -719529},
		{-271821, 4, 20, -100000000},
		{275760, 9, 13, 100000000},
	}
	for _, test := range tests {
		got := isoDaysFromCivil(int64(test.year), test.month, test.day)
		if got != test.wantDays {
			t.Errorf("days for %d-%02d-%02d = %d, want %d", test.year, test.month, test.day, got, test.wantDays)
		}
		year, month, day := isoCivilFromDays(got)
		if year != test.year || month != test.month || day != test.day {
			t.Errorf("civil for %d = %d-%02d-%02d", got, year, month, day)
		}
	}
}

func TestParseTemporalInstant(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"1970-01-01T00:00Z", "1970-01-01T00:00:00Z"},
		{"19700101T00Z", "1970-01-01T00:00:00Z"},
		{"1970-01-01T000000Z", "1970-01-01T00:00:00Z"},
		{"+0019700101T000000Z", "1970-01-01T00:00:00Z"},
		{"1970-01-01t00:00:00.000000001z", "1970-01-01T00:00:00.000000001Z"},
		{"1969-12-31 23:59:59.999999999Z", "1969-12-31T23:59:59.999999999Z"},
		{"2000-02-29T12:34:56.123400000+01:30", "2000-02-29T11:04:56.1234Z"},
		{"2000-01-01T00:00:00-00:00:30.5", "2000-01-01T00:00:30.5Z"},
		{"1970-01-01T00+010030", "1969-12-31T22:59:30Z"},
		{"2016-12-31T23:59:60Z", "2016-12-31T23:59:59Z"},
		{"+010000-01-01T00:00:00Z", "+010000-01-01T00:00:00Z"},
		{"0000-01-01T00:00:00Z", "0000-01-01T00:00:00Z"},
		{"2020-01-01T00:00:00+00:00[America/New_York][u-ca=iso8601]", "2020-01-01T00:00:00Z"},
		{"1970-01-01T00:00Z[u-ca=iso8601][u-ca=discord]", "1970-01-01T00:00:00Z"},
		{"1970-01-01T00:00Z[!-02:30]", "1970-01-01T00:00:00Z"},
		{"1970-01-01T00:00Z[+12]", "1970-01-01T00:00:00Z"},
		{"2020-01-01T00:00:00Z[x-extra=value]", "2020-01-01T00:00:00Z"},
	}
	for _, test := range tests {
		t.Run(test.input, func(t *testing.T) {
			instant, err := parseTemporalInstant(test.input)
			if err != nil {
				t.Fatal(err)
			}
			if got := instant.string(); got != test.want {
				t.Fatalf("string = %q, want %q", got, test.want)
			}
		})
	}
}

func TestParseTemporalInstantRange(t *testing.T) {
	for _, test := range []struct {
		input string
		want  string
	}{
		{"-271821-04-20T00:00:00Z", "-271821-04-20T00:00:00Z"},
		{"+275760-09-13T00:00:00Z", "+275760-09-13T00:00:00Z"},
	} {
		instant, err := parseTemporalInstant(test.input)
		if err != nil {
			t.Fatalf("parse %s: %v", test.input, err)
		}
		if got := instant.string(); got != test.want {
			t.Fatalf("string = %q, want %q", got, test.want)
		}
	}
	for _, input := range []string{
		"-271821-04-19T23:59:59.999999999Z",
		"+275760-09-13T00:00:00.000000001Z",
	} {
		if _, err := parseTemporalInstant(input); err == nil {
			t.Fatalf("accepted out-of-range %q", input)
		}
	}
}

func TestParseTemporalInstantRejectsInvalidStrings(t *testing.T) {
	for _, input := range []string{
		"",
		" 1970-01-01T00:00:00Z",
		"1970-01-01T00:00:00Z ",
		"2020-01-01",
		"2020-01-01T00:00:00",
		"2020-02-30T00:00:00Z",
		"2021-02-29T00:00:00Z",
		"2020-01-01T24:00:00Z",
		"2020-01-01T00:60:00Z",
		"2020-01-01T00:00:00.1234567890Z",
		"2020-01-01T00:00:00+24:00",
		"-000000-01-01T00:00:00Z",
		"2020-01-01T00:00:00Z[UTC][Europe/Paris]",
		"2020-01-01T00:00:00Z[U-CA=iso8601]",
		"2020-01-01T00:00:00Z[u-ca=iso8601][!u-ca=gregory]",
		"2020-01-01T00:00:00Z[-07:00:01]",
		"2020-01-01T00:00:00Z[!x-unknown=value]",
		"2020-01-01T00:00:00Z[",
		"2020-01-01T00:00:00Zjunk",
	} {
		t.Run(input, func(t *testing.T) {
			if _, err := parseTemporalInstant(input); err == nil {
				t.Fatal("invalid string accepted")
			}
		})
	}
}

func TestTemporalInstantArithmetic(t *testing.T) {
	instant, err := parseTemporalInstant("1969-12-31T23:59:59.999999999Z")
	if err != nil {
		t.Fatal(err)
	}
	added, ok := instant.addNanoseconds(big.NewInt(2))
	if !ok || added.string() != "1970-01-01T00:00:00.000000001Z" {
		t.Fatalf("added = %q, %v", added.string(), ok)
	}
	if compareTemporalInstants(instant, added) >= 0 || compareTemporalInstants(added, instant) <= 0 || compareTemporalInstants(instant, instant) != 0 {
		t.Fatal("instant comparison is inconsistent")
	}
	max, err := parseTemporalInstant("+275760-09-13T00:00:00Z")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := max.addNanoseconds(big.NewInt(1)); ok {
		t.Fatal("addition exceeded the Temporal range")
	}
}
