package main

import (
	"math/rand"
	"testing"
)

// TestDesignBalanced pins that over the eight placements every target is at
// each phase four times, and every pair of targets in each combination of
// phases twice: what keeps a change from being measured at the placements
// that happen to suit it.
func TestDesignBalanced(t *testing.T) {
	var ones [4]int
	var pairs [4][4][4]int
	for n := range 8 {
		d := design(n)
		for i := range 4 {
			ones[i] += d[i]
			for j := i + 1; j < 4; j++ {
				pairs[i][j][2*d[i]+d[j]]++
			}
		}
	}
	for i := range 4 {
		if ones[i] != 4 {
			t.Errorf("target %d is at phase 1 in %d placements, want 4", i, ones[i])
		}
		for j := i + 1; j < 4; j++ {
			for c, got := range pairs[i][j] {
				if got != 2 {
					t.Errorf("targets %d and %d are in combination %d in %d placements, want 2", i, j, c, got)
				}
			}
		}
	}
}

// TestPadCountsReachTheDesign pins that the pads padCounts chooses put the
// targets at the design's phases, wherever the targets start: a pad of m
// slots moves its target and every later one by 32*m bytes.
func TestPadCountsReachTheDesign(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	for range 1000 {
		var ref [4]uint64
		a := uint64(0x140000000) + uint64(rng.Intn(1<<20))*32
		for i := range ref {
			ref[i] = a
			a += uint64(rng.Intn(1<<16)) * 32
		}
		for n := range 8 {
			counts := padCounts(ref, n)
			var got [4]uint64
			shift := uint64(0)
			for i := range ref {
				shift += uint64(counts[i]) * 32
				got[i] = ref[i] + shift
			}
			if p := phases(got); p != design(n) {
				t.Fatalf("ref %x, placement %d: pads %v give phases %v, want %v", ref, n, counts, p, design(n))
			}
		}
	}
}
