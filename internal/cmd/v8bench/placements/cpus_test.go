package main

import (
	"runtime"
	"slices"
	"testing"
)

// -cpus reads groups of CPUs and ranges, separated by semicolons, and
// refuses what is not one, or a CPU in two groups.
func TestParseCPUs(t *testing.T) {
	for _, c := range []struct {
		spec string
		want []string
	}{
		{"0-5;6-11", []string{"0-5", "6-11"}},
		{"0-2,6-8;3-5,9-11", []string{"0-2,6-8", "3-5,9-11"}},
		{" 3, 1,2 ; 7 ", []string{"1-3", "7"}},
		{"0-1;;2-3", []string{"0-1", "2-3"}},
	} {
		g, _, err := parseCPUs(c.spec)
		if err != nil {
			t.Errorf("%q: %v", c.spec, err)
			continue
		}
		var got []string
		for _, s := range g {
			got = append(got, s.String())
		}
		if !slices.Equal(got, c.want) {
			t.Errorf("%q: %v, want %v", c.spec, got, c.want)
		}
	}
	for _, spec := range []string{"a-b", "5-2", "0-3;3-5", "1,x"} {
		if _, _, err := parseCPUs(spec); err == nil {
			t.Errorf("%q: no error", spec)
		}
	}
	h, _, err := parseCPUs("halves")
	if err != nil || len(h) != 2 || len(h[0])+len(h[1]) != runtime.NumCPU() {
		t.Errorf("halves: %v, %v", h, err)
	}
	// auto gives two groups or more, apart, whatever the machine.
	a, how, err := parseCPUs("auto")
	if err != nil || len(a) < 2 {
		t.Fatalf("auto: %v (%s), %v", a, how, err)
	}
	t.Logf("auto: %v, %s", a, how)
	seen := map[int]bool{}
	for _, g := range a {
		for _, c := range g {
			if seen[c] || c >= runtime.NumCPU() {
				t.Errorf("auto: CPU %d twice or past the machine's", c)
			}
			seen[c] = true
		}
	}
}

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

// -cpus auto keeps to the fastest cores where the machine's differ, groups
// them by their level 3 caches, else halves them, and gives each group as
// many CPUs.
func TestAutoGroups(t *testing.T) {
	cpus := func(n int) cpuSet {
		s := make(cpuSet, n)
		for i := range s {
			s[i] = i
		}
		return s
	}
	ryzen := []cpuSet{{0, 1, 2, 3, 4, 5}, {6, 7, 8, 9, 10, 11}}
	// A hybrid Intel: eight P-cores' sixteen threads, eight E-cores, one cache.
	hybrid := map[int]int{}
	for c := range 24 {
		hybrid[c] = 0
		if c < 16 {
			hybrid[c] = 1
		}
	}
	// Two caches of mixed cores, as AMD's Zen 5 and Zen 5c complexes: the
	// fast ones 0-3 and 8-9.
	mixed := map[int]int{}
	for c := range 12 {
		mixed[c] = 0
		if c < 4 || c == 8 || c == 9 {
			mixed[c] = 1
		}
	}
	for _, c := range []struct {
		name    string
		cache   []cpuSet
		classes map[int]int
		all     cpuSet
		want    []string
	}{
		{"Ryzen 3600", ryzen, nil, cpus(12), []string{"0-5", "6-11"}},
		{"cores alike", ryzen, map[int]int{0: 1, 6: 1}, cpus(12), []string{"0-5", "6-11"}},
		{"one cache", []cpuSet{cpus(8)}, nil, cpus(8), []string{"0-3", "4-7"}},
		{"no caches known", nil, nil, cpus(6), []string{"0-2", "3-5"}},
		{"hybrid Intel", []cpuSet{cpus(24)}, hybrid, cpus(24), []string{"0-7", "8-15"}},
		{"mixed complexes", []cpuSet{{0, 1, 2, 3, 4, 5, 6, 7}, {8, 9, 10, 11}}, mixed, cpus(12), []string{"0-1", "8-9"}},
	} {
		g, how := autoGroups(c.cache, nil, c.classes, c.all)
		var got []string
		for _, s := range g {
			got = append(got, s.String())
		}
		if !slices.Equal(got, c.want) {
			t.Errorf("%s: %v (%s), want %v", c.name, got, how, c.want)
		}
	}
}

// Where the machine has NUMA nodes, no group spans two, and the labels'
// two are of two nodes.
func TestAutoGroupsNUMA(t *testing.T) {
	span := func(lo, hi int) cpuSet {
		var s cpuSet
		for c := lo; c <= hi; c++ {
			s = append(s, c)
		}
		return s
	}
	// Two sockets of 16 CPUs, one cache each.
	sockets := []cpuSet{span(0, 15), span(16, 31)}
	// An EPYC in NPS4: four nodes of 16 CPUs, each two complexes' caches.
	var nps4Cache, nps4Nodes []cpuSet
	for i := range 8 {
		nps4Cache = append(nps4Cache, span(8*i, 8*i+7))
	}
	for i := range 4 {
		nps4Nodes = append(nps4Nodes, span(16*i, 16*i+15))
	}
	// One cache across two nodes, as sub-NUMA clustering has it, whose CPUs
	// are numbered across them, so that the nodes are not the halves.
	snc := []cpuSet{append(span(0, 3), span(8, 11)...), append(span(4, 7), span(12, 15)...)}
	for _, c := range []struct {
		name         string
		cache, nodes []cpuSet
		all          cpuSet
		want         []string
	}{
		{"two sockets", sockets, sockets, span(0, 31), []string{"0-15", "16-31"}},
		{"EPYC NPS4", nps4Cache, nps4Nodes, span(0, 63), []string{"0-7", "16-23"}},
		{"one cache, two nodes", []cpuSet{span(0, 15)}, snc, span(0, 15), []string{"0-3,8-11", "4-7,12-15"}},
		{"no caches, two nodes", nil, snc, span(0, 15), []string{"0-3,8-11", "4-7,12-15"}},
		{"one node", []cpuSet{span(0, 5), span(6, 11)}, []cpuSet{span(0, 11)}, span(0, 11), []string{"0-5", "6-11"}},
	} {
		g, how := autoGroups(c.cache, c.nodes, nil, c.all)
		got := []string{g[0].String(), g[1].String()}
		if !slices.Equal(got, c.want) {
			t.Errorf("%s: %v (%s), want %v", c.name, got, how, c.want)
		}
	}
}
