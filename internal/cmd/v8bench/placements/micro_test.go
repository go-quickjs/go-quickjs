package main

import (
	"strings"
	"testing"
)

// The micro mode's lines are read as its tests' times of one operation;
// its header, its total and the heap's line are not tests.
func TestParseMicro(t *testing.T) {
	out := `go-quickjs, micro
TEST                             N   TIME (ns)
empty_loop                33554432       5.073
prop_read                  8388608      10.250
TOTAL            1945.1 ms    2748.8 cpu      960.8 MB    16175778 allocs
peak heap 23.8 MB, live after GC 6.5 MB
`
	got := parseMicro(out)
	if len(got) != 2 || got[0] != (named{"empty_loop", 5.073}) || got[1] != (named{"prop_read", 10.25}) {
		t.Fatalf("parseMicro = %v", got)
	}
}

// A test's row takes each placement's best round, counts the placements
// the second label is slower at by more than microSlower, and the report
// lists the tests slower by microReport or more at every placement.
func TestReportMicro(t *testing.T) {
	names := []string{"steady", "slower", "mixed"}
	res := [2][]map[string][]float64{}
	a := []float64{10, 10, 10, 10}
	times := map[string][2][]float64{
		"steady": {a, {10.1, 9.9, 10, 10.1}},
		"slower": {a, {11, 10.5, 10.6, 11.2}},
		"mixed":  {a, {12, 9, 12, 9}},
	}
	for l := range 2 {
		for i := range 4 {
			m := map[string][]float64{}
			for _, n := range names {
				// Two rounds, of which the best is the time.
				m[n] = []float64{times[n][l][i] + 1, times[n][l][i]}
			}
			res[l] = append(res[l], m)
		}
	}
	rows := microRows(names, res, 4)
	if len(rows) != 3 || rows[0].slower != 0 || rows[1].slower != 4 || rows[2].slower != 2 {
		t.Fatalf("rows %+v", rows)
	}
	if rows[1].a != 10 || rows[1].b != (11+10.5+10.6+11.2)/4 {
		t.Fatalf("slower's means %v, %v", rows[1].a, rows[1].b)
	}
	var b strings.Builder
	reportMicro(&b, [2]string{"main", "change"}, names, res, 4, false)
	out := b.String()
	tail := out[strings.Index(out, "at every placement:"):]
	if !strings.Contains(tail, "slower") || strings.Contains(tail, "mixed") || strings.Contains(tail, "steady") {
		t.Errorf("only slower is slower at every placement:\n%s", out)
	}
	if !strings.Contains(out, "3 tests, change / main") {
		t.Errorf("no summary:\n%s", out)
	}
}
