package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
)

// canPin says whether startPinned gives a process its CPUs.
const canPin = true

// cacheGroups is the logical CPUs of each level 3 cache, as sysfs lists
// what shares each CPU's, or nil.
func cacheGroups() []cpuSet {
	paths, _ := filepath.Glob("/sys/devices/system/cpu/cpu[0-9]*/cache/index3/shared_cpu_list")
	var groups []cpuSet
	for _, p := range paths {
		b, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		g, _, err := parseCPUs(strings.TrimSpace(string(b)))
		if err != nil || len(g) != 1 {
			continue
		}
		if !slices.ContainsFunc(groups, func(h cpuSet) bool { return slices.Equal(h, g[0]) }) {
			groups = append(groups, g[0])
		}
	}
	slices.SortFunc(groups, func(a, b cpuSet) int { return a[0] - b[0] })
	return groups
}

// numaNodes is the logical CPUs of each NUMA node, as sysfs lists them, or
// nil.
func numaNodes() []cpuSet {
	paths, _ := filepath.Glob("/sys/devices/system/node/node[0-9]*/cpulist")
	var nodes []cpuSet
	for _, p := range paths {
		b, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		if g, _, err := parseCPUs(strings.TrimSpace(string(b))); err == nil && len(g) == 1 {
			nodes = append(nodes, g[0])
		}
	}
	slices.SortFunc(nodes, func(a, b cpuSet) int { return a[0] - b[0] })
	return nodes
}

// coreClasses is each logical CPU's class, higher for a faster core, or
// nil: a hybrid Intel's P-cores and E-cores as perf lists them
// (/sys/devices/cpu_core and cpu_atom); else each CPU's capacity, as an
// Arm's big and little cores have theirs; else its highest performance,
// as ACPI's CPPC ranks an AMD's cores.
func coreClasses() map[int]int {
	if core, err := os.ReadFile("/sys/devices/cpu_core/cpus"); err == nil {
		if atom, err := os.ReadFile("/sys/devices/cpu_atom/cpus"); err == nil {
			classes := map[int]int{}
			for class, list := range [][]byte{atom, core} {
				if g, _, err := parseCPUs(strings.TrimSpace(string(list))); err == nil && len(g) == 1 {
					for _, c := range g[0] {
						classes[c] = class
					}
				}
			}
			return classes
		}
	}
	for _, file := range []string{"cpu_capacity", "acpi_cppc/highest_perf"} {
		paths, _ := filepath.Glob("/sys/devices/system/cpu/cpu[0-9]*/" + file)
		classes := map[int]int{}
		for _, p := range paths {
			b, err := os.ReadFile(p)
			if err != nil {
				continue
			}
			var c, v int
			if _, err := fmt.Sscanf(strings.TrimPrefix(p, "/sys/devices/system/cpu/cpu"), "%d", &c); err != nil {
				continue
			}
			if _, err := fmt.Sscanf(strings.TrimSpace(string(b)), "%d", &v); err != nil {
				continue
			}
			classes[c] = v
		}
		if len(classes) > 0 {
			return classes
		}
	}
	return nil
}

// startPinned starts cmd on set, so that every thread the process makes is
// on it from its start: through numactl, its memory then the set's node's
// (--localalloc), as the kernel would give it anyway unless told
// otherwise, else through taskset.
func startPinned(cmd *exec.Cmd, set cpuSet) error {
	if numactl, err := exec.LookPath("numactl"); err == nil {
		cmd.Args = append([]string{numactl, "--physcpubind=" + set.String(), "--localalloc"}, cmd.Args...)
		cmd.Path = numactl
		return cmd.Start()
	}
	taskset, err := exec.LookPath("taskset")
	if err != nil {
		return fmt.Errorf("-parallel needs numactl or taskset: %w", err)
	}
	cmd.Args = append([]string{taskset, "-c", set.String()}, cmd.Args...)
	cmd.Path = taskset
	return cmd.Start()
}
