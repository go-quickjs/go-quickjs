package icu

import (
	"slices"
	"sync"
	"testing"
	"time"
)

func TestTimeZonePossibleInstants(t *testing.T) {
	zone, err := LoadTimeZone("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name  string
		local time.Time
		want  []int64
	}{
		{
			name:  "ordinary",
			local: time.Date(2021, time.January, 15, 12, 0, 0, 0, time.UTC),
			want:  []int64{time.Date(2021, time.January, 15, 17, 0, 0, 0, time.UTC).Unix()},
		},
		{
			name:  "spring gap",
			local: time.Date(2021, time.March, 14, 2, 30, 0, 0, time.UTC),
			want:  nil,
		},
		{
			name:  "fall overlap",
			local: time.Date(2021, time.November, 7, 1, 30, 0, 0, time.UTC),
			want: []int64{
				time.Date(2021, time.November, 7, 5, 30, 0, 0, time.UTC).Unix(),
				time.Date(2021, time.November, 7, 6, 30, 0, 0, time.UTC).Unix(),
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := zone.PossibleInstants(test.local.Unix())
			if !slices.Equal(got, test.want) {
				t.Fatalf("PossibleInstants(%s) = %v, want %v", test.local, got, test.want)
			}
		})
	}
}

func TestTimeZonePossibleInstantsNonHourTransitions(t *testing.T) {
	lordHowe, err := LoadTimeZone("Australia/Lord_Howe")
	if err != nil {
		t.Fatal(err)
	}
	local := time.Date(2021, time.April, 4, 1, 45, 0, 0, time.UTC).Unix()
	want := []int64{
		time.Date(2021, time.April, 3, 14, 45, 0, 0, time.UTC).Unix(),
		time.Date(2021, time.April, 3, 15, 15, 0, 0, time.UTC).Unix(),
	}
	if got := lordHowe.PossibleInstants(local); !slices.Equal(got, want) {
		t.Fatalf("Lord Howe overlap = %v, want %v", got, want)
	}

	apia, err := LoadTimeZone("Pacific/Apia")
	if err != nil {
		t.Fatal(err)
	}
	skippedDay := time.Date(2011, time.December, 30, 12, 0, 0, 0, time.UTC).Unix()
	if got := apia.PossibleInstants(skippedDay); len(got) != 0 {
		t.Fatalf("Apia skipped day has possible instants %v", got)
	}
}

func TestTimeZoneCompatibleInstant(t *testing.T) {
	zone, err := LoadTimeZone("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name  string
		local time.Time
		want  time.Time
	}{
		{
			name:  "spring gap shifts forward",
			local: time.Date(2021, time.March, 14, 2, 30, 0, 0, time.UTC),
			want:  time.Date(2021, time.March, 14, 7, 30, 0, 0, time.UTC),
		},
		{
			name:  "fall overlap chooses earlier",
			local: time.Date(2021, time.November, 7, 1, 30, 0, 0, time.UTC),
			want:  time.Date(2021, time.November, 7, 5, 30, 0, 0, time.UTC),
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, ok := zone.CompatibleInstant(test.local.Unix())
			if !ok || got != test.want.Unix() {
				t.Fatalf("CompatibleInstant(%s) = %s, %v; want %s, true", test.local, time.Unix(got, 0), ok, test.want)
			}
		})
	}
	lordHowe, err := LoadTimeZone("Australia/Lord_Howe")
	if err != nil {
		t.Fatal(err)
	}
	lordHoweGap := time.Date(2021, time.October, 3, 2, 15, 0, 0, time.UTC)
	lordHoweWant := time.Date(2021, time.October, 2, 15, 45, 0, 0, time.UTC)
	if got, ok := lordHowe.CompatibleInstant(lordHoweGap.Unix()); !ok || got != lordHoweWant.Unix() {
		t.Fatalf("Lord Howe gap = %s, %v; want %s, true", time.Unix(got, 0).UTC(), ok, lordHoweWant)
	}

	apia, err := LoadTimeZone("Pacific/Apia")
	if err != nil {
		t.Fatal(err)
	}
	skippedDay := time.Date(2011, time.December, 30, 12, 0, 0, 0, time.UTC)
	want := time.Date(2011, time.December, 30, 22, 0, 0, 0, time.UTC)
	if got, ok := apia.CompatibleInstant(skippedDay.Unix()); !ok || got != want.Unix() {
		t.Fatalf("Apia skipped day = %s, %v; want %s, true", time.Unix(got, 0).UTC(), ok, want)
	}
}

func TestTimeZoneStartOfDayAfterMidnightGap(t *testing.T) {
	zone, err := LoadTimeZone("America/Sao_Paulo")
	if err != nil {
		t.Fatal(err)
	}
	localMidnight := time.Date(2015, time.October, 18, 0, 0, 0, 0, time.UTC)
	want := time.Date(2015, time.October, 18, 3, 0, 0, 0, time.UTC)
	if got, ok := zone.StartOfDay(localMidnight.Unix()); !ok || got != want.Unix() {
		t.Fatalf("start of skipped-midnight day = %s, %v; want %s, true", time.Unix(got, 0).UTC(), ok, want)
	}
}

func TestTimeZoneTransitions(t *testing.T) {
	zone, err := LoadTimeZone("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	spring := time.Date(2021, time.March, 14, 7, 0, 0, 0, time.UTC).Unix()
	fall := time.Date(2021, time.November, 7, 6, 0, 0, 0, time.UTC).Unix()

	if got, ok := zone.NextTransition(time.Date(2021, time.January, 1, 0, 0, 0, 0, time.UTC).Unix()); !ok || got != spring {
		t.Fatalf("next transition = %d, %v; want %d, true", got, ok, spring)
	}
	if got, ok := zone.NextTransition(spring); !ok || got != fall {
		t.Fatalf("next transition at exact boundary = %d, %v; want %d, true", got, ok, fall)
	}
	if got, ok := zone.PreviousTransition(time.Date(2021, time.December, 1, 0, 0, 0, 0, time.UTC).Unix()); !ok || got != fall {
		t.Fatalf("previous transition = %d, %v; want %d, true", got, ok, fall)
	}
	if got, ok := zone.PreviousTransition(fall); !ok || got != spring {
		t.Fatalf("previous transition at exact boundary = %d, %v; want %d, true", got, ok, spring)
	}
}

func TestTimeZonePOSIXExtension(t *testing.T) {
	zone, err := LoadTimeZone("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	start := time.Date(2200, time.January, 1, 0, 0, 0, 0, time.UTC).Unix()
	next, ok := zone.NextTransition(start)
	if !ok || next <= start {
		t.Fatalf("no future transition after %s: %d, %v", time.Unix(start, 0), next, ok)
	}
	before := zone.OffsetAt(next - 1)
	after := zone.OffsetAt(next)
	if before.OffsetSeconds == after.OffsetSeconds {
		t.Fatalf("transition at %s did not change offset: %#v to %#v", time.Unix(next, 0), before, after)
	}

	local := time.Date(2200, time.July, 1, 12, 0, 0, 0, time.UTC).Unix()
	instants := zone.PossibleInstants(local)
	if len(instants) != 1 || zone.OffsetAt(instants[0]).OffsetSeconds != -4*60*60 {
		t.Fatalf("future summer local time = %v", instants)
	}
}

func TestFixedTimeZoneHasNoTransitions(t *testing.T) {
	zone, err := LoadTimeZone("UTC")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := zone.NextTransition(0); ok {
		t.Fatal("UTC reported a next transition")
	}
	if _, ok := zone.PreviousTransition(0); ok {
		t.Fatal("UTC reported a previous transition")
	}
	if got := zone.PossibleInstants(123); !slices.Equal(got, []int64{123}) {
		t.Fatalf("UTC possible instants = %v", got)
	}
}

func TestParseTZifRejectsTruncatedData(t *testing.T) {
	data, err := embeddedTZData("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := parseTZif(data)
	if err != nil {
		t.Fatal(err)
	}
	if len(parsed.offsets) < 2 || len(parsed.transitions) == 0 {
		t.Fatalf("parsed incomplete New York data: %d offsets, %d transitions", len(parsed.offsets), len(parsed.transitions))
	}
	for _, length := range []int{0, 4, 43, len(data) / 2} {
		if _, err := parseTZif(data[:length]); err == nil {
			t.Errorf("parseTZif accepted %d-byte prefix", length)
		}
	}
}

func TestTimeZoneConcurrentLoadSharesParsedData(t *testing.T) {
	const workers = 32
	zones := make(chan *TimeZone, workers)
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			zone, err := LoadTimeZone("Europe/Berlin")
			if err != nil {
				t.Errorf("LoadTimeZone: %v", err)
				return
			}
			zones <- zone
		}()
	}
	wg.Wait()
	close(zones)
	var first *TimeZone
	for zone := range zones {
		if first == nil {
			first = zone
		} else if zone != first {
			t.Fatal("concurrent loads did not share parsed time-zone data")
		}
	}
}
