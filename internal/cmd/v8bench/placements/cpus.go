package main

import (
	"fmt"
	"math"
	"runtime"
	"slices"
	"strconv"
	"strings"
)

// A cpuSet is the logical CPUs one side of a parallel comparison runs on.
type cpuSet []int

func (s cpuSet) String() string {
	var parts []string
	for i := 0; i < len(s); {
		j := i
		for j+1 < len(s) && s[j+1] == s[j]+1 {
			j++
		}
		if j > i {
			parts = append(parts, fmt.Sprintf("%d-%d", s[i], s[j]))
		} else {
			parts = append(parts, strconv.Itoa(s[i]))
		}
		i = j + 1
	}
	return strings.Join(parts, ",")
}

// parseCPUs reads -cpus: "auto", the groups of the fastest cores' CPUs
// that share a level 3 cache, each in one NUMA node, as the OS reports
// them (autoGroups), else those CPUs by node, or in halves; "halves", all the CPUs in halves; or the groups
// themselves, separated by semicolons, each a list of CPUs and ranges --
// "0-5;6-11", "0-2,6-8;3-5,9-11".
func parseCPUs(spec string) ([]cpuSet, string, error) {
	switch spec {
	case "", "auto":
		g, how := autoGroups(cacheGroups(), numaNodes(), coreClasses(), allCPUs())
		return g, how, nil
	case "halves":
		return halveSet(allCPUs()), "the CPUs in halves", nil
	}
	var groups []cpuSet
	for _, g := range strings.Split(spec, ";") {
		var set cpuSet
		for _, part := range strings.Split(g, ",") {
			part = strings.TrimSpace(part)
			if part == "" {
				continue
			}
			lo, hi, isRange := strings.Cut(part, "-")
			a, err := strconv.Atoi(lo)
			if err != nil {
				return nil, "", fmt.Errorf("-cpus %q: %q is not a CPU", spec, part)
			}
			b := a
			if isRange {
				if b, err = strconv.Atoi(hi); err != nil || b < a {
					return nil, "", fmt.Errorf("-cpus %q: %q is not a range of CPUs", spec, part)
				}
			}
			for c := a; c <= b; c++ {
				set = append(set, c)
			}
		}
		slices.Sort(set)
		set = slices.Compact(set)
		if len(set) > 0 {
			groups = append(groups, set)
		}
	}
	for i, g := range groups {
		for _, h := range groups[i+1:] {
			for _, c := range g {
				if slices.Contains(h, c) {
					return nil, "", fmt.Errorf("-cpus %q: CPU %d is in two groups", spec, c)
				}
			}
		}
	}
	return groups, "the groups -cpus gives", nil
}

// allCPUs is the machine's logical CPUs.
func allCPUs() cpuSet {
	all := make(cpuSet, runtime.NumCPU())
	for c := range all {
		all[c] = c
	}
	return all
}

// halveSet is set in two halves, as most CPUs number theirs: a core's
// threads next to each other, the cores of a cache's group in a run.
func halveSet(set cpuSet) []cpuSet {
	return []cpuSet{set[:len(set)/2], set[len(set)/2:]}
}

// autoGroups is -cpus auto: of the CPUs, the fastest cores' alone, where
// classes says the cores differ -- a hybrid's P-cores, an Arm's big ones,
// so that neither label runs on the slower kind -- in the groups that
// share a level 3 cache, as cache has them, where two or more have them,
// else by NUMA node, where two or more have them, else in halves. No
// group spans two nodes: each label's memory is then its node's, the
// first two groups, the two labels', from two nodes where the machine has
// them, so that neither shares the other's memory controller (the labels
// swap the groups, so that one node's being nearer a device costs neither
// more). The first two are cut to the smaller one's size, so that each
// label has as many CPUs.
func autoGroups(cache, nodes []cpuSet, classes map[int]int, all cpuSet) ([]cpuSet, string) {
	fast, how := all, ""
	if len(classes) > 0 {
		top, low := math.MinInt, math.MaxInt
		for _, v := range classes {
			top, low = max(top, v), min(low, v)
		}
		if top != low {
			fast = nil
			for _, c := range all {
				if v, ok := classes[c]; ok && v == top {
					fast = append(fast, c)
				}
			}
			how = ", the fastest cores' alone"
		}
	}
	var groups []cpuSet
	for _, g := range cache {
		var in cpuSet
		for _, c := range g {
			if slices.Contains(fast, c) {
				in = append(in, c)
			}
		}
		if len(in) > 0 {
			groups = append(groups, in)
		}
	}
	inNodes := func(groups []cpuSet) []cpuSet {
		if len(nodes) < 2 {
			return groups
		}
		var split []cpuSet
		for _, g := range groups {
			for _, n := range nodes {
				if in := intersect(g, n); len(in) > 0 {
					split = append(split, in)
				}
			}
		}
		return split
	}
	switch {
	case len(groups) >= 2:
		groups = inNodes(groups)
		how = "the groups that share a level 3 cache" + how
	case len(inNodes([]cpuSet{fast})) >= 2:
		groups = inNodes([]cpuSet{fast})
		how = "the NUMA nodes" + how + ": no two level 3 caches were found"
	default:
		groups = halveSet(fast)
		how = "the CPUs in halves" + how
		if len(cache) < 2 {
			how += ": no two level 3 caches were found"
		}
	}
	// The labels' two: the first, then the first of another node, if any.
	nodeOf := func(g cpuSet) int {
		for i, n := range nodes {
			if slices.Contains(n, g[0]) {
				return i
			}
		}
		return -1
	}
	if len(nodes) >= 2 {
		for i := 1; i < len(groups); i++ {
			if nodeOf(groups[i]) != nodeOf(groups[0]) {
				groups[1], groups[i] = groups[i], groups[1]
				how += ", in two NUMA nodes"
				break
			}
		}
	}
	size := min(len(groups[0]), len(groups[1]))
	groups[0], groups[1] = groups[0][:size], groups[1][:size]
	return groups, how
}

// intersect is the CPUs of a also in b, in a's order.
func intersect(a, b cpuSet) cpuSet {
	var in cpuSet
	for _, c := range a {
		if slices.Contains(b, c) {
			in = append(in, c)
		}
	}
	return in
}
