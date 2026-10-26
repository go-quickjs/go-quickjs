package main

import (
	"slices"
	"testing"
)

// -k 4 takes placements over which the first three targets are each at
// each phase twice.
func TestFourPlacementsBalance(t *testing.T) {
	ns := placementsOf(4)
	for target := range 3 {
		ones := 0
		for _, n := range ns {
			ones += design(n)[target]
		}
		if ones != 2 {
			t.Errorf("target %d at phase 1 in %d of %v", target, ones, ns)
		}
	}
	if got := placementsOf(8); !slices.Equal(got, []int{0, 1, 2, 3, 4, 5, 6, 7}) {
		t.Errorf("placementsOf(8) = %v", got)
	}
}

// A benchmark is run again as many times as fill a second of it: none
// for a long one, the most for the shortest.
func TestRepsFor(t *testing.T) {
	for _, c := range []struct {
		ms   float64
		want int
	}{{9700, 0}, {1000, 0}, {999, 3}, {400, 3}, {120, 10}, {40, 30}, {12, 100}, {4, 300}, {0.5, 300}} {
		if got := repsFor(c.ms); got != c.want {
			t.Errorf("repsFor(%v) = %d, want %d", c.ms, got, c.want)
		}
	}
}

// A benchmark's runs are decided by the first run that times it and kept
// for every other, whichever label or placement it is: Gameboy's first run
// of 330 ms by one label and 350 by the other must not give it 10 runs and
// 3.
func TestRepsDecidedOnce(t *testing.T) {
	var reps repsTable
	if got := reps.of("Gameboy", 330); got != 10 {
		t.Fatalf("first run of 330 ms: %d runs, want 10", got)
	}
	if got := reps.of("Gameboy", 350); got != 10 {
		t.Errorf("second label's run of 350 ms: %d runs, want the first's 10", got)
	}
	if got := reps.of("Richards", 4); got != 300 {
		t.Errorf("another benchmark: %d runs, want its own 300", got)
	}
}
